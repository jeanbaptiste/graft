package admin

import (
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"graft/internal/state"
)

// validName matches what's safe to use both as a token filename and as a
// dynamic_repo primary key — deliberately narrow rather than trying to
// escape whatever isn't.
var validName = regexp.MustCompile(`^[a-zA-Z0-9._-]{1,80}$`)

var addPeerTmpl = template.Must(template.New("add-peer").Parse(`
<h1>Add a peer to an existing federation</h1>
<p class="lead">Joins a Forgejo instance to a repo already mirrored elsewhere.</p>
{{if .Error}}<div class="notice bad">{{.Error}}</div>{{end}}
<div class="card">
  <form method="POST" action="/add-peer">
    <label for="series">Federation (series)</label>
    <select id="series" name="series" required>
      <option value="">— choose —</option>
      {{range .Series}}<option value="{{.}}" {{if eq . $.SelectedSeries}}selected{{end}}>{{.}}</option>{{end}}
    </select>

    <label for="name">Peer name</label>
    <input type="text" id="name" name="name" required maxlength="80" pattern="[a-zA-Z0-9._-]+" value="{{.Name}}" placeholder="e.g. federation-x-gitadmin">

    <div class="row">
      <div>
        <label for="base_url">Forgejo base URL</label>
        <input type="url" id="base_url" name="base_url" required value="{{.BaseURL}}" placeholder="https://git.example.org">
      </div>
      <div>
        <label for="owner">Owner</label>
        <input type="text" id="owner" name="owner" required value="{{.Owner}}">
      </div>
    </div>
    <label for="repo">Repository</label>
    <input type="text" id="repo" name="repo" required value="{{.Repo}}">

    <label for="token">Forgejo token</label>
    <textarea id="token" name="token" required placeholder="scoped to write:repository + write:issue"></textarea>
    <div class="hint">Written to its own chmod-600 file on submit. Not kept in this form, not logged. From a third party: use <a href="/share/new">one-time share</a>.</div>

    <label>Sync scope</label>
    <div class="checks">
      <label><input type="checkbox" name="sync_git" checked> git</label>
      <label><input type="checkbox" name="sync_issues" checked> issues</label>
      <label><input type="checkbox" name="sync_patches" checked> patches</label>
    </div>

    <label for="password">Admin password (optional)</label>
    <input type="password" id="password" name="password" autocomplete="off">
    <div class="hint">Know it? Enter it to go live immediately. Leave it blank to submit for the graft admin to review and approve instead.</div>

    <button type="submit">Submit</button>
  </form>
</div>
<p><a class="nav-link" href="/add-radicle-peer">Add a Radicle peer instead &rarr;</a></p>
`))

var newRepoTmpl = template.Must(template.New("new-repo").Parse(`
<h1>Start a new repo</h1>
<p class="lead">Mirrors a repo on a new Forgejo instance and a new Radicle node.</p>
<ol class="steps">
  <li>On the Forgejo instance: create the repo with one initial commit. Generate a token scoped to <code>write:repository</code> + <code>write:issue</code>.</li>
  <li>Clone it locally. Run <code>rad init --name &lt;repo&gt; --default-branch &lt;branch&gt; --public</code>. Note the RID (<code>rad:z...</code>).</li>
  <li>To replicate to other Radicle nodes: <code>rad seed &lt;rid&gt;</code> on each.</li>
  <li>Fill in the form below.</li>
</ol>
{{if .Error}}<div class="notice bad">{{.Error}}</div>{{end}}
<div class="card">
  <form method="POST" action="/new-repo">
    <label for="name">Repo name</label>
    <input type="text" id="name" name="name" required maxlength="80" pattern="[a-zA-Z0-9._-]+" value="{{.Name}}">

    <div class="row">
      <div>
        <label for="base_url">Forgejo base URL</label>
        <input type="url" id="base_url" name="base_url" required value="{{.BaseURL}}" placeholder="https://git.example.org">
      </div>
      <div>
        <label for="owner">Owner</label>
        <input type="text" id="owner" name="owner" required value="{{.Owner}}">
      </div>
    </div>
    <label for="repo">Repository</label>
    <input type="text" id="repo" name="repo" required value="{{.Repo}}">
    <label for="token">Forgejo token</label>
    <textarea id="token" name="token" required placeholder="scoped to write:repository + write:issue"></textarea>
    <div class="hint">Got it from someone else safely? See <a href="/share/new">one-time share</a>.</div>

    <label for="rid">Radicle RID</label>
    <input type="text" id="rid" name="rid" required value="{{.RID}}" placeholder="rad:z...">
    <div class="row">
      <div>
        <label for="rad_http">Radicle HTTP base URL</label>
        <input type="url" id="rad_http" name="rad_http" required value="{{.RadHTTP}}" placeholder="https://seed.example.org">
      </div>
      <div>
        <label for="explorer">Explorer URL (optional)</label>
        <input type="url" id="explorer" name="explorer" value="{{.Explorer}}" placeholder="https://app.radicle.xyz">
      </div>
    </div>

    <label>Sync scope</label>
    <div class="checks">
      <label><input type="checkbox" name="sync_git" checked> git</label>
      <label><input type="checkbox" name="sync_issues" checked> issues</label>
      <label><input type="checkbox" name="sync_patches" checked> patches</label>
    </div>

    <label for="password">Admin password (optional)</label>
    <input type="password" id="password" name="password" autocomplete="off">
    <div class="hint">Know it? Enter it to go live immediately. Leave it blank to submit for the graft admin to review and approve instead.</div>

    <button type="submit">Submit</button>
  </form>
</div>
`))

var onboardOKTmpl = template.Must(template.New("onboard-ok").Parse(`
<h1>{{.Title}}</h1>
{{if .Approved}}
<div class="notice good">{{.Name}} is live within one sync interval — no restart.</div>
{{else}}
<div class="notice good">{{.Name}} submitted. Waiting on the graft admin to review and approve it.</div>
{{end}}
<p><a class="nav-link" href="/">&larr; Back to the dashboard</a></p>
`))

func (h *Handler) addPeer(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		h.renderAddPeer(w, map[string]any{"Series": seriesNames(h.cfg.Series())})
	case http.MethodPost:
		h.submitAddPeer(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func seriesNames(s []SeriesInfo) []string {
	out := make([]string, 0, len(s))
	for _, si := range s {
		out = append(out, si.Name)
	}
	return out
}

func (h *Handler) renderAddPeer(w http.ResponseWriter, data map[string]any) {
	render(w, "add a peer", "SELF-SERVICE ONBOARDING", renderFragment(addPeerTmpl, data))
}

func (h *Handler) submitAddPeer(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	if !h.submitLimiter.allowed(ip) {
		h.renderAddPeer(w, map[string]any{"Series": seriesNames(h.cfg.Series()), "Error": "too many submissions from your address — try again later"})
		return
	}
	if err := r.ParseForm(); err != nil {
		h.renderAddPeer(w, map[string]any{"Series": seriesNames(h.cfg.Series()), "Error": "couldn't read the form"})
		return
	}
	form := formData(r, "series", "name", "base_url", "owner", "repo", "token", "password")
	retry := func(msg string) {
		h.renderAddPeer(w, map[string]any{
			"Series": seriesNames(h.cfg.Series()), "Error": msg, "SelectedSeries": form["series"],
			"Name": form["name"], "BaseURL": form["base_url"], "Owner": form["owner"], "Repo": form["repo"],
		})
	}
	h.submitLimiter.recordFailure(ip) // counts every submission attempt, not just invalid ones — bounds queue-flooding volume

	if !validName.MatchString(form["name"]) {
		retry("pair name must be letters, digits, dots, dashes or underscores only")
		return
	}
	if form["token"] = strings.TrimSpace(r.FormValue("token")); form["token"] == "" {
		retry("a Forgejo token is required")
		return
	}
	series := findSeries(h.cfg.Series(), form["series"])
	if series == nil {
		retry("pick a federation to join")
		return
	}
	if err := validateForgejo(form["base_url"], form["owner"], form["repo"]); err != nil {
		retry(err.Error())
		return
	}
	// The password is optional: given and correct, this goes live right
	// away (the "hand out the password for a class/workshop" path);
	// given and wrong, reject outright — never silently fall back to the
	// approval queue, which would make wrong-password attempts
	// indistinguishable from a real submission; left blank, it queues for
	// the admin to review, same as a stranger submitting with no password
	// at all.
	autoApprove := false
	if form["password"] != "" {
		if !checkPassword(h.cfg, form["password"]) {
			h.passwordLimiter.recordFailure(ip)
			retry("wrong admin password")
			return
		}
		autoApprove = true
	}

	tokenFile, err := writeTokenFile(h.cfg.TokenDir, form["name"], form["token"])
	if err != nil {
		retry("could not save the token: " + err.Error())
		return
	}

	id, err := h.cfg.Store.CreateDynamicRepo(state.DynamicRepo{
		Name: form["name"], Series: series.Name,
		ForgejoBaseURL: form["base_url"], ForgejoOwner: form["owner"], ForgejoRepo: form["repo"], ForgejoTokenFile: tokenFile,
		RadicleRID: series.RadicleRID, RadicleHTTPBaseURL: series.RadicleHTTPBaseURL, RadicleExplorerURL: series.RadicleExplorerURL,
		RadicleRadHome: h.cfg.DefaultRadHome,
		SyncGit:        r.FormValue("sync_git") != "", SyncIssues: r.FormValue("sync_issues") != "", SyncPatches: r.FormValue("sync_patches") != "",
	})
	if err == nil && autoApprove {
		err = h.cfg.Store.ApproveDynamicRepo(id)
	}
	if err != nil {
		retry("could not save: " + err.Error())
		return
	}
	title := "Peer submitted"
	if autoApprove {
		title = "Peer added"
	}
	render(w, title, "SELF-SERVICE ONBOARDING", renderFragment(onboardOKTmpl, map[string]any{"Title": title, "Name": form["name"], "Approved": autoApprove}))
}

func (h *Handler) newRepo(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		h.renderNewRepo(w, map[string]any{})
	case http.MethodPost:
		h.submitNewRepo(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (h *Handler) renderNewRepo(w http.ResponseWriter, data map[string]any) {
	render(w, "start a new repo", "SELF-SERVICE ONBOARDING", renderFragment(newRepoTmpl, data))
}

func (h *Handler) submitNewRepo(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	if !h.submitLimiter.allowed(ip) {
		h.renderNewRepo(w, map[string]any{"Error": "too many submissions from your address — try again later"})
		return
	}
	if err := r.ParseForm(); err != nil {
		h.renderNewRepo(w, map[string]any{"Error": "couldn't read the form"})
		return
	}
	form := formData(r, "name", "base_url", "owner", "repo", "token", "rid", "rad_http", "explorer", "password")
	retry := func(msg string) {
		h.renderNewRepo(w, map[string]any{
			"Error": msg, "Name": form["name"], "BaseURL": form["base_url"], "Owner": form["owner"], "Repo": form["repo"],
			"RID": form["rid"], "RadHTTP": form["rad_http"], "Explorer": form["explorer"],
		})
	}
	h.submitLimiter.recordFailure(ip)

	if !validName.MatchString(form["name"]) {
		retry("repo name must be letters, digits, dots, dashes or underscores only")
		return
	}
	if form["token"] = strings.TrimSpace(r.FormValue("token")); form["token"] == "" {
		retry("a Forgejo token is required")
		return
	}
	if err := validateForgejo(form["base_url"], form["owner"], form["repo"]); err != nil {
		retry(err.Error())
		return
	}
	if form["rid"] == "" || !strings.HasPrefix(form["rid"], "rad:") {
		retry("that doesn't look like a Radicle RID (expected rad:z...)")
		return
	}
	if _, err := url.ParseRequestURI(form["rad_http"]); err != nil {
		retry("the Radicle HTTP base URL isn't valid")
		return
	}
	autoApprove := false
	if form["password"] != "" {
		if !checkPassword(h.cfg, form["password"]) {
			h.passwordLimiter.recordFailure(ip)
			retry("wrong admin password")
			return
		}
		autoApprove = true
	}

	tokenFile, err := writeTokenFile(h.cfg.TokenDir, form["name"], form["token"])
	if err != nil {
		retry("could not save the token: " + err.Error())
		return
	}

	id, err := h.cfg.Store.CreateDynamicRepo(state.DynamicRepo{
		Name: form["name"], Series: form["name"],
		ForgejoBaseURL: form["base_url"], ForgejoOwner: form["owner"], ForgejoRepo: form["repo"], ForgejoTokenFile: tokenFile,
		RadicleRID: form["rid"], RadicleHTTPBaseURL: form["rad_http"], RadicleExplorerURL: form["explorer"],
		RadicleRadHome: h.cfg.DefaultRadHome,
		SyncGit:        r.FormValue("sync_git") != "", SyncIssues: r.FormValue("sync_issues") != "", SyncPatches: r.FormValue("sync_patches") != "",
	})
	if err == nil && autoApprove {
		err = h.cfg.Store.ApproveDynamicRepo(id)
	}
	if err != nil {
		retry("could not save: " + err.Error())
		return
	}
	title := "Repo submitted"
	if autoApprove {
		title = "Repo started"
	}
	render(w, title, "SELF-SERVICE ONBOARDING", renderFragment(onboardOKTmpl, map[string]any{"Title": title, "Name": form["name"], "Approved": autoApprove}))
}

func formData(r *http.Request, fields ...string) map[string]string {
	out := make(map[string]string, len(fields))
	for _, f := range fields {
		out[f] = strings.TrimSpace(r.FormValue(f))
	}
	return out
}

func findSeries(all []SeriesInfo, name string) *SeriesInfo {
	for _, s := range all {
		if s.Name == name {
			return &s
		}
	}
	return nil
}

func validateForgejo(baseURL, owner, repo string) error {
	u, err := url.ParseRequestURI(baseURL)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return fmt.Errorf("the Forgejo base URL must be a valid https:// URL")
	}
	if owner == "" || repo == "" {
		return fmt.Errorf("owner and repository are required")
	}
	return nil
}

// writeTokenFile saves a submitted token to its own chmod-600 file,
// following the same convention as every config.yaml-declared pair.
func writeTokenFile(dir, name, token string) (string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(dir, name+".token")
	if err := os.WriteFile(path, []byte(token+"\n"), 0o600); err != nil {
		return "", err
	}
	return path, nil
}
