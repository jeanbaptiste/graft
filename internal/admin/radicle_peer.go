package admin

import (
	"bytes"
	"context"
	"fmt"
	"html/template"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"time"
)

// radExecTimeout bounds "rad node connect" / "rad seed" — a network
// handshake with a node that may never answer shouldn't hang the request.
const radExecTimeout = 60 * time.Second

var validNodeID = regexp.MustCompile(`^[a-zA-Z0-9]{1,80}$`)
var validNodeAddress = regexp.MustCompile(`^[a-zA-Z0-9.\-]+:[0-9]{1,5}$`)

// runRad runs the rad CLI against graft's own local Radicle identity —
// the same one every pair (static or dynamic) already pushes as. Never
// scoped to a single repo (unlike internal/radicle.Client), since
// "connect" operates on the node itself, not a repository.
func runRad(radHome string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), radExecTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "rad", args...)
	cmd.Env = append(os.Environ(), "RAD_HOME="+radHome)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		return out.String(), fmt.Errorf("rad %v: %w: %s", args, err, out.String())
	}
	return out.String(), nil
}

var addRadiclePeerTmpl = template.Must(template.New("add-radicle-peer").Parse(`
<h1>Add a Radicle peer</h1>
<p class="lead">Connects a new Radicle node to the mesh and has it seed an existing federation.</p>
{{if .Error}}<div class="notice bad">{{.Error}}</div>{{end}}
<div class="card">
  <form method="POST" action="/add-radicle-peer">
    <label for="series">Federation (series)</label>
    <select id="series" name="series" required>
      <option value="">— choose —</option>
      {{range .Series}}<option value="{{.}}" {{if eq . $.SelectedSeries}}selected{{end}}>{{.}}</option>{{end}}
    </select>

    <label for="node_id">Node ID</label>
    <input type="text" id="node_id" name="node_id" required value="{{.NodeID}}" placeholder="z6Mk...">

    <label for="address">Address</label>
    <input type="text" id="address" name="address" required value="{{.Address}}" placeholder="host:8776">

    <label for="password">Admin password</label>
    <input type="password" id="password" name="password" required autocomplete="off">

    <button type="submit">Connect &amp; seed</button>
  </form>
</div>
<p><a class="nav-link" href="/add-peer">Add a Forgejo peer instead &rarr;</a></p>
`))

func (h *Handler) addRadiclePeer(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		h.renderAddRadiclePeer(w, map[string]any{"Series": seriesNames(h.cfg.Series())})
	case http.MethodPost:
		h.submitAddRadiclePeer(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (h *Handler) renderAddRadiclePeer(w http.ResponseWriter, data map[string]any) {
	render(w, "add a radicle peer", "SELF-SERVICE ONBOARDING", renderFragment(addRadiclePeerTmpl, data))
}

func (h *Handler) submitAddRadiclePeer(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	if !h.passwordLimiter.allowed(ip) {
		h.renderAddRadiclePeer(w, map[string]any{"Series": seriesNames(h.cfg.Series()), "Error": "too many attempts from your address — try again later"})
		return
	}
	if err := r.ParseForm(); err != nil {
		h.renderAddRadiclePeer(w, map[string]any{"Series": seriesNames(h.cfg.Series()), "Error": "couldn't read the form"})
		return
	}
	form := formData(r, "series", "node_id", "address", "password")
	retry := func(msg string) {
		h.renderAddRadiclePeer(w, map[string]any{
			"Series": seriesNames(h.cfg.Series()), "Error": msg, "SelectedSeries": form["series"],
			"NodeID": form["node_id"], "Address": form["address"],
		})
	}

	if !checkPassword(h.cfg, form["password"]) {
		h.passwordLimiter.recordFailure(ip)
		retry("wrong admin password")
		return
	}
	series := findSeries(h.cfg.Series(), form["series"])
	if series == nil {
		retry("pick a federation to join")
		return
	}
	if !validNodeID.MatchString(form["node_id"]) {
		retry("that doesn't look like a valid node id")
		return
	}
	if !validNodeAddress.MatchString(form["address"]) {
		retry("address must be host:port")
		return
	}

	if _, err := runRad(h.cfg.DefaultRadHome, "node", "connect", form["node_id"]+"@"+form["address"]); err != nil {
		retry("could not connect to that node: " + err.Error())
		return
	}
	if _, err := runRad(h.cfg.DefaultRadHome, "seed", series.RadicleRID); err != nil {
		retry("connected, but could not seed the repository: " + err.Error())
		return
	}

	render(w, "radicle peer added", "SELF-SERVICE ONBOARDING", renderFragment(onboardOKTmpl, map[string]string{"Title": "Radicle peer added", "Name": form["node_id"]}))
}
