// Package admin serves graft's self-service dashboard actions: a one-time
// secret-sharing page (so a token never has to travel through chat or
// email in the clear), and two onboarding forms ("add a peer to an
// existing federation" and "start a new repo from scratch") gated by a
// single shared admin password rather than a config.yaml edit and a
// restart.
//
// None of this requires a session or a cookie: every state-changing
// action re-submits the password on its own form, checked with a
// constant-time comparison and backed by a rate limiter — the password is
// short and shared, so the limiter (not the comparison's timing safety
// alone) is the thing actually standing between an attacker and a brute
// force of it.
package admin

import (
	"crypto/subtle"
	"html/template"
	"net/http"
	"strings"
	"sync"
	"time"

	"graft/internal/state"
)

// SeriesInfo is one federation an "add a peer" submission can join,
// carrying the Radicle side every existing pair in it already shares —
// so a newly-added Forgejo peer never has to (and never gets to) pick its
// own RID or seed.
type SeriesInfo struct {
	Name               string
	RadicleRID         string
	RadicleHTTPBaseURL string
	RadicleExplorerURL string
}

// Config wires the admin package to the rest of graft without importing
// cmd/sync or internal/sync — it only ever reads/writes through Store and
// the callbacks below.
type Config struct {
	Store *state.Store

	// AdminPassword gates every state-changing action here: claiming a
	// share needs its own 6-digit passcode instead, but creating a share,
	// and submitting or approving an add-peer/new-repo form, all check
	// this. Never logged, never echoed back on error.
	AdminPassword string

	// PublicHost is used to build the share link shown after creation
	// (https://<PublicHost>/share/<id>). Required for the share feature;
	// the onboarding forms work without it.
	PublicHost string

	// TokenDir is the directory a submitted token gets written into as
	// its own chmod-600 file, following the same convention as every
	// config.yaml-declared pair (e.g. /etc/graft).
	TokenDir string

	// DefaultRadHome is graft's own local Radicle identity — the same
	// one every pair, static or dynamic, pushes as. Never user-supplied.
	DefaultRadHome string

	// Series returns every federation an "add a peer" submission can
	// join, freshly computed per request (cheap: it's just config.Repos
	// plus whatever dynamic_repo rows exist already).
	Series func() []SeriesInfo
}

// Handler serves every admin-facing route. Three independent rate
// limiters: a wrong admin password (used only on the /admin/pending
// review page now), a wrong share passcode, and a plain volume cap on
// onboarding submissions (which need no password at all — anyone can
// propose a peer, only an admin approval activates it) are different
// concerns, so a lockout on one never blocks the others.
type Handler struct {
	cfg             Config
	passwordLimiter *rateLimiter
	shareLimiter    *rateLimiter
	submitLimiter   *rateLimiter
}

func NewHandler(cfg Config) *Handler {
	return &Handler{cfg: cfg, passwordLimiter: newRateLimiter(), shareLimiter: newRateLimiter(), submitLimiter: newRateLimiter()}
}

func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("/share/new", h.shareNew)
	mux.HandleFunc("/share/", h.shareClaim) // also serves /share/<id>/qr.png
	mux.HandleFunc("/add-peer", h.addPeer)
	mux.HandleFunc("/add-radicle-peer", h.addRadiclePeer)
	mux.HandleFunc("/new-repo", h.newRepo)
	mux.HandleFunc("/admin/pending", h.pending)
	mux.HandleFunc("/admin/pending/approve", h.approvePending)
	mux.HandleFunc("/admin/pending/reject", h.rejectPending)
}

// checkPassword compares in constant time, so a wrong guess never takes
// measurably longer or shorter based on which character differs.
func checkPassword(cfg Config, submitted string) bool {
	want := []byte(cfg.AdminPassword)
	got := []byte(submitted)
	if len(want) != len(got) {
		return false
	}
	return subtle.ConstantTimeCompare(want, got) == 1
}

// clientIP prefers the first hop of X-Forwarded-For (set by graft's own
// nginx reverse proxy in front of it) and falls back to the raw remote
// address — imperfect if a future proxy change stops setting that header
// faithfully, which is exactly why the rate limiter also enforces a
// global cap independent of IP attribution (see rateLimiter).
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.Index(xff, ","); i >= 0 {
			return strings.TrimSpace(xff[:i])
		}
		return strings.TrimSpace(xff)
	}
	if i := strings.LastIndex(r.RemoteAddr, ":"); i >= 0 {
		return r.RemoteAddr[:i]
	}
	return r.RemoteAddr
}

const (
	rateLimitWindow   = 15 * time.Minute
	maxFailuresPerIP  = 5
	maxFailuresGlobal = 20
)

// rateLimiter is a simple in-memory sliding-window counter over password
// failures — not persisted (a restart resets it), which is an acceptable
// trade for a password meant to be short-lived and low-stakes rather than
// a real credential; see TUTORIAL.md's "Future: built-in token exchange"
// for the actually-strong alternative (delegated token minting) this
// password model is a deliberately lightweight stand-in for.
type rateLimiter struct {
	mu     sync.Mutex
	byIP   map[string][]time.Time
	global []time.Time
}

func newRateLimiter() *rateLimiter {
	return &rateLimiter{byIP: map[string][]time.Time{}}
}

func prune(times []time.Time, now time.Time) []time.Time {
	out := times[:0]
	for _, t := range times {
		if now.Sub(t) < rateLimitWindow {
			out = append(out, t)
		}
	}
	return out
}

// allowed reports whether ip may attempt a password check right now.
func (rl *rateLimiter) allowed(ip string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	now := time.Now()
	rl.global = prune(rl.global, now)
	if len(rl.global) >= maxFailuresGlobal {
		return false
	}
	rl.byIP[ip] = prune(rl.byIP[ip], now)
	return len(rl.byIP[ip]) < maxFailuresPerIP
}

// recordFailure counts one wrong password attempt against both the
// per-IP and global windows.
func (rl *rateLimiter) recordFailure(ip string) {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	now := time.Now()
	rl.byIP[ip] = append(rl.byIP[ip], now)
	rl.global = append(rl.global, now)
}

// pageTmpl is the shared chrome every admin page renders inside —
// visually related to the main dashboard (same palette) but with a
// distinct amber accent so a self-service action page is never
// mistaken for the read-only status page.
var pageTmpl = template.Must(template.New("admin").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>graft — {{.Title}}</title>
<style>
  :root {
    color-scheme: light;
    --surface: #ffffff; --surface-sunk: #f4f5f7; --text: #172b4d; --text-muted: #6b778c;
    --border: #dfe1e6; --brand: #0052cc; --accent: #ae6b00; --accent-soft: #fff4e0;
    --bad: #de350b; --good: #36b37e;
  }
  * { box-sizing: border-box; }
  body { margin: 0; background: var(--surface-sunk); color: var(--text);
    font: 14px/1.55 -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, Helvetica, Arial, sans-serif; }
  main { max-width: 640px; margin: 0 auto; padding: 2.5rem 1.5rem 3rem; }
  .top { display: flex; align-items: baseline; justify-content: space-between; margin-bottom: 1.5rem; }
  h1 { font-size: 1.15rem; margin: 0; font-weight: 600; }
  .eyebrow { display: inline-block; font-size: .7rem; text-transform: uppercase; letter-spacing: .06em;
    color: var(--accent); background: var(--accent-soft); padding: .15rem .5rem; border-radius: 3px; margin-bottom: .5rem; }
  .nav-link { color: var(--brand); font-size: .82rem; text-decoration: none; font-weight: 500; }
  .nav-link:hover { text-decoration: underline; }
  .card { background: var(--surface); border: 1px solid var(--border); border-radius: 6px; padding: 1.5rem; margin-bottom: 1.25rem; }
  p.lead { color: var(--text-muted); font-size: .88rem; margin: 0 0 1.25rem; }
  label { display: block; font-size: .78rem; font-weight: 600; color: var(--text-muted); margin: 1rem 0 .3rem; }
  label:first-of-type { margin-top: 0; }
  input[type=text], input[type=password], input[type=url], select, textarea {
    width: 100%; padding: .55rem .65rem; border: 1px solid var(--border); border-radius: 4px;
    font: inherit; background: var(--surface-sunk); color: var(--text);
  }
  textarea { resize: vertical; min-height: 4.5rem; font-family: ui-monospace, monospace; font-size: .82rem; }
  .hint { font-size: .74rem; color: var(--text-muted); margin-top: .3rem; }
  .row { display: flex; flex-wrap: wrap; gap: 1rem; }
  .row > div { flex: 1 1 12rem; min-width: 0; }
  .checks { display: flex; gap: 1.25rem; margin-top: .5rem; }
  .checks label { display: flex; align-items: center; gap: .4rem; font-weight: 500; margin: 0; }
  .checks input { width: auto; }
  button {
    margin-top: 1.5rem; background: var(--brand); color: #fff; border: none; border-radius: 4px;
    padding: .65rem 1.1rem; font: inherit; font-weight: 600; cursor: pointer;
  }
  button:hover { background: #0747a6; }
  .steps { counter-reset: step; list-style: none; padding: 0; margin: 0 0 1.5rem; }
  .steps li { counter-increment: step; position: relative; padding: 0 0 1rem 2.2rem; font-size: .86rem; }
  .steps li::before {
    content: counter(step); position: absolute; left: 0; top: 0; width: 1.5rem; height: 1.5rem;
    border-radius: 50%; background: var(--accent-soft); color: var(--accent); font-weight: 700;
    font-size: .76rem; display: flex; align-items: center; justify-content: center;
  }
  .steps code { background: var(--surface-sunk); padding: .1rem .35rem; border-radius: 3px; font-size: .82em; }
  .notice { border-radius: 6px; padding: .9rem 1.1rem; font-size: .86rem; margin-bottom: 1.25rem; }
  .notice.good { background: #e3fcef; color: #006644; border: 1px solid #abf5d1; }
  .notice.bad { background: #ffebe6; color: #bf2600; border: 1px solid #ffbdad; }
  .secret-box {
    font-family: ui-monospace, monospace; font-size: .95rem; background: var(--surface-sunk);
    border: 1px dashed var(--border); border-radius: 4px; padding: .9rem; word-break: break-all; margin: .5rem 0;
  }
  .passcode { font-size: 1.6rem; font-weight: 700; letter-spacing: .1em; text-align: center;
    background: var(--accent-soft); color: var(--accent); border-radius: 6px; padding: .7rem; margin: .5rem 0; }
  footer { max-width: 640px; margin: 0 auto; padding: 1rem 1.5rem 2.5rem; color: var(--text-muted); font-size: .78rem; }
</style>
</head>
<body>
<main>
  <div class="top">
    <h1>graft</h1>
    <a class="nav-link" href="/">&larr; Dashboard</a>
  </div>
  {{if .Eyebrow}}<span class="eyebrow">{{.Eyebrow}}</span>{{end}}
  {{.Body}}
</main>
</body>
</html>
`))

type pageData struct {
	Title   string
	Eyebrow string
	Body    template.HTML
}

// render wraps body (already-escaped HTML built from a sub-template) in
// the shared page chrome. body is template.HTML deliberately — every
// caller builds it via html/template itself, so this isn't an escaping
// bypass, just composition of two already-safe fragments.
func render(w http.ResponseWriter, title, eyebrow string, body template.HTML) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = pageTmpl.Execute(w, pageData{Title: title, Eyebrow: eyebrow, Body: body})
}

func renderFragment(tmpl *template.Template, data any) template.HTML {
	var b strings.Builder
	if err := tmpl.Execute(&b, data); err != nil {
		return template.HTML("<p class=\"notice bad\">" + template.HTMLEscapeString(err.Error()) + "</p>")
	}
	return template.HTML(b.String())
}
