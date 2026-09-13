package admin

import (
	"fmt"
	"html/template"
	"net/http"
	"os"
	"strconv"
	"strings"
)

// pendingRow is one submitted request awaiting approval, whichever of the
// two underlying tables (dynamic_repo, pending_radicle_peer) it came
// from — unified here so the review page shows one list.
type pendingRow struct {
	ID      int64
	Type    string // "peer" (dynamic_repo — a Forgejo peer or a new repo) or "radicle"
	Summary string
	Detail  string
}

var pendingLoginTmpl = template.Must(template.New("pending-login").Parse(`
<h1>Pending requests</h1>
<p class="lead">Review peer and repo requests submitted through the onboarding forms. Approving is the only step — every field was already filled in by whoever submitted the request.</p>
{{if .Error}}<div class="notice bad">{{.Error}}</div>{{end}}
<div class="card">
  <form method="POST" action="/admin/pending">
    <label for="password">Admin password</label>
    <input type="password" id="password" name="password" required autocomplete="off" autofocus>
    <button type="submit">View pending requests</button>
  </form>
</div>
`))

var pendingListTmpl = template.Must(template.New("pending-list").Parse(`
<h1>Pending requests</h1>
{{if .Notice}}<div class="notice good">{{.Notice}}</div>{{end}}
{{if .Error}}<div class="notice bad">{{.Error}}</div>{{end}}
{{if not .Rows}}<p class="empty">Nothing pending.</p>{{end}}
{{range .Rows}}
<div class="card">
  <div><b>{{.Summary}}</b></div>
  <div class="hint">{{.Detail}}</div>
  <div class="row" style="margin-top:.75rem; gap:.5rem;">
    <form method="POST" action="/admin/pending/approve">
      <input type="hidden" name="id" value="{{.ID}}">
      <input type="hidden" name="type" value="{{.Type}}">
      <input type="hidden" name="password" value="{{$.Password}}">
      <button type="submit">Approve</button>
    </form>
    <form method="POST" action="/admin/pending/reject">
      <input type="hidden" name="id" value="{{.ID}}">
      <input type="hidden" name="type" value="{{.Type}}">
      <input type="hidden" name="password" value="{{$.Password}}">
      <button type="submit">Reject</button>
    </form>
  </div>
</div>
{{end}}
`))

func (h *Handler) pending(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		h.renderPendingLogin(w, "")
	case http.MethodPost:
		ip := clientIP(r)
		if !h.passwordLimiter.allowed(ip) {
			h.renderPendingLogin(w, "too many attempts from your address — try again later")
			return
		}
		if err := r.ParseForm(); err != nil {
			h.renderPendingLogin(w, "couldn't read the form")
			return
		}
		password := r.FormValue("password")
		if !checkPassword(h.cfg, password) {
			h.passwordLimiter.recordFailure(ip)
			h.renderPendingLogin(w, "wrong admin password")
			return
		}
		h.renderPendingList(w, password, "", "")
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (h *Handler) renderPendingLogin(w http.ResponseWriter, errMsg string) {
	render(w, "pending requests", "ADMIN", renderFragment(pendingLoginTmpl, map[string]string{"Error": errMsg}))
}

func (h *Handler) renderPendingList(w http.ResponseWriter, password, notice, errMsg string) {
	var rows []pendingRow

	dr, err := h.cfg.Store.PendingDynamicRepos()
	if err != nil && errMsg == "" {
		errMsg = err.Error()
	}
	for _, d := range dr {
		var scope []string
		if d.SyncGit {
			scope = append(scope, "git")
		}
		if d.SyncIssues {
			scope = append(scope, "issues")
		}
		if d.SyncPatches {
			scope = append(scope, "patches")
		}
		rows = append(rows, pendingRow{
			ID: d.ID, Type: "peer",
			Summary: fmt.Sprintf("%s → series %s", d.Name, d.Series),
			Detail:  fmt.Sprintf("%s/%s/%s · %s", d.ForgejoBaseURL, d.ForgejoOwner, d.ForgejoRepo, strings.Join(scope, "+")),
		})
	}

	rp, err := h.cfg.Store.ListPendingRadiclePeers()
	if err != nil && errMsg == "" {
		errMsg = err.Error()
	}
	for _, p := range rp {
		rows = append(rows, pendingRow{
			ID: p.ID, Type: "radicle",
			Summary: fmt.Sprintf("%s → series %s", p.NodeID, p.Series),
			Detail:  p.Address,
		})
	}

	render(w, "pending requests", "ADMIN", renderFragment(pendingListTmpl, map[string]any{
		"Rows": rows, "Password": password, "Notice": notice, "Error": errMsg,
	}))
}

// pendingAction handles the shared shape of approve/reject: check the
// password (re-submitted from the list page, not a session), find the
// request, run the caller's effect, then re-render the list with the
// result — approve/reject are the only two things that can happen here,
// so there's no separate confirmation step.
func (h *Handler) pendingAction(w http.ResponseWriter, r *http.Request, action func(id int64, kind string) (string, error)) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	ip := clientIP(r)
	if !h.passwordLimiter.allowed(ip) {
		h.renderPendingLogin(w, "too many attempts from your address — try again later")
		return
	}
	if err := r.ParseForm(); err != nil {
		h.renderPendingLogin(w, "couldn't read the form")
		return
	}
	password := r.FormValue("password")
	if !checkPassword(h.cfg, password) {
		h.passwordLimiter.recordFailure(ip)
		h.renderPendingLogin(w, "wrong admin password")
		return
	}
	id, err := strconv.ParseInt(r.FormValue("id"), 10, 64)
	if err != nil {
		h.renderPendingList(w, password, "", "invalid request id")
		return
	}
	notice, err := action(id, r.FormValue("type"))
	if err != nil {
		h.renderPendingList(w, password, "", err.Error())
		return
	}
	h.renderPendingList(w, password, notice, "")
}

func (h *Handler) approvePending(w http.ResponseWriter, r *http.Request) {
	h.pendingAction(w, r, func(id int64, kind string) (string, error) {
		switch kind {
		case "peer":
			if err := h.cfg.Store.ApproveDynamicRepo(id); err != nil {
				return "", err
			}
			return "Approved — live within one sync interval, no restart.", nil

		case "radicle":
			p, err := h.cfg.Store.GetPendingRadiclePeer(id)
			if err != nil {
				return "", err
			}
			if p == nil {
				return "", fmt.Errorf("request not found — already handled?")
			}
			series := findSeries(h.cfg.Series(), p.Series)
			if series == nil {
				return "", fmt.Errorf("series %q no longer exists", p.Series)
			}
			if _, err := runRad(h.cfg.DefaultRadHome, "node", "connect", p.NodeID+"@"+p.Address); err != nil {
				return "", fmt.Errorf("could not connect to that node: %w", err)
			}
			if _, err := runRad(h.cfg.DefaultRadHome, "seed", series.RadicleRID); err != nil {
				return "", fmt.Errorf("connected, but could not seed the repository: %w", err)
			}
			if err := h.cfg.Store.DeletePendingRadiclePeer(id); err != nil {
				return "", err
			}
			return "Connected and seeding.", nil

		default:
			return "", fmt.Errorf("unknown request type %q", kind)
		}
	})
}

func (h *Handler) rejectPending(w http.ResponseWriter, r *http.Request) {
	h.pendingAction(w, r, func(id int64, kind string) (string, error) {
		switch kind {
		case "peer":
			row, err := h.cfg.Store.GetDynamicRepo(id)
			if err != nil {
				return "", err
			}
			if row != nil && row.ForgejoTokenFile != "" {
				_ = os.Remove(row.ForgejoTokenFile) // best-effort — don't leave the submitted token behind
			}
			if err := h.cfg.Store.RejectDynamicRepo(id); err != nil {
				return "", err
			}
			return "Rejected.", nil

		case "radicle":
			if err := h.cfg.Store.DeletePendingRadiclePeer(id); err != nil {
				return "", err
			}
			return "Rejected.", nil

		default:
			return "", fmt.Errorf("unknown request type %q", kind)
		}
	})
}
