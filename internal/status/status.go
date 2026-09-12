// Package status tracks the outcome of each repo pair's most recent sync
// pass, keeps a small dashboard over the activity log, and serves both over
// HTTP — so an operator can tell the daemon is alive and see what it's
// actually done without reading logs.
package status

import (
	"encoding/json"
	"html/template"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"graft/internal/state"
)

// PairResult is the outcome of one repo pair's most recent pass. An empty
// error string means that scope is either disabled or succeeded.
type PairResult struct {
	Name        string    `json:"name"`
	Series      string    `json:"series"`
	LastRun     time.Time `json:"last_run"`
	GitError    string    `json:"git_error,omitempty"`
	IssuesError string    `json:"issues_error,omitempty"`
	PatchError  string    `json:"patch_error,omitempty"`
}

func (r PairResult) ok() bool {
	return r.GitError == "" && r.IssuesError == "" && r.PatchError == ""
}

// ServerRef is one repo pair's declared mirror target — a specific Forgejo
// instance or a specific Radicle node — as configured, regardless of
// whether it has ever actually produced an event yet.
type ServerRef struct {
	Label string // host, e.g. "f1.cyberwild.org"
	URL   string // repo root on that host
}

// Tracker holds the latest result per repo pair, safe for concurrent use,
// plus a handle on the state store for the activity dashboard.
type Tracker struct {
	startedAt time.Time
	store     *state.Store
	sourceURL string

	mu         sync.Mutex
	pairs      map[string]PairResult
	topology   map[string][]ServerRef
	publicHost string
}

func NewTracker(store *state.Store, sourceURL string) *Tracker {
	return &Tracker{startedAt: time.Now(), store: store, sourceURL: sourceURL, pairs: map[string]PairResult{}}
}

// SetTopology declares every mirror target configured for each series, so
// the dashboard can show a side that's never produced an event yet (e.g. a
// Radicle node nothing has been pushed to) instead of only sides the
// activity log happens to mention.
func (t *Tracker) SetTopology(topology map[string][]ServerRef) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.topology = topology
}

// SetPublicHost declares the host graft is reachable at (e.g.
// graft.cyberwild.org), enabling the "Social bridges" link and the
// /social page. Call with "" (the default) to leave ActivityPub disabled.
func (t *Tracker) SetPublicHost(host string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.publicHost = host
}

// Record stores the outcome of a pass for one pair. Pass nil for a scope
// that succeeded or wasn't enabled. series is the dashboard row this pair's
// status badge groups under (see config.RepoPair.Series).
func (t *Tracker) Record(name, series string, gitErr, issuesErr, patchErr error) {
	r := PairResult{Name: name, Series: series, LastRun: time.Now()}
	if gitErr != nil {
		r.GitError = gitErr.Error()
	}
	if issuesErr != nil {
		r.IssuesError = issuesErr.Error()
	}
	if patchErr != nil {
		r.PatchError = patchErr.Error()
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.pairs[name] = r
}

func (t *Tracker) snapshot() []PairResult {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]PairResult, 0, len(t.pairs))
	for _, r := range t.pairs {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

type statusResponse struct {
	StartedAt time.Time    `json:"started_at"`
	Pairs     []PairResult `json:"pairs"`
}

// Handler serves:
//
//	GET /healthz    - 200 if every pair's last pass was clean, 503 otherwise
//	GET /api/status - the JSON snapshot Record() has been fed
//	GET /           - the activity dashboard (calendar + recent commits)
func (t *Tracker) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		for _, p := range t.snapshot() {
			if !p.ok() {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
		}
		w.WriteHeader(http.StatusOK)
	})

	mux.HandleFunc("/api/status", func(w http.ResponseWriter, r *http.Request) {
		resp := statusResponse{StartedAt: t.startedAt, Pairs: t.snapshot()}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	})

	mux.HandleFunc("/", t.serveDashboard)
	mux.HandleFunc("/social", t.serveSocial)

	return mux
}

const dashboardDays = 3

func (t *Tracker) serveDashboard(w http.ResponseWriter, r *http.Request) {
	since := time.Now().UTC().AddDate(0, 0, -(dashboardDays - 1)).Truncate(24 * time.Hour)
	entries, err := t.store.ActivitySince(since)
	if err != nil {
		http.Error(w, "load activity: "+err.Error(), http.StatusInternalServerError)
		return
	}

	pairs := groupPairStatus(t.snapshot())
	health := make(map[string]bool, len(pairs))
	for _, p := range pairs {
		health[p.Name] = p.OK
	}

	t.mu.Lock()
	topology := t.topology
	publicHost := t.publicHost
	t.mu.Unlock()

	data := dashboardData{
		Series:        buildSeriesRows(entries, health, topology),
		Activity:      recentActivity(entries),
		SourceURL:     t.sourceURL,
		SocialEnabled: publicHost != "",
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err := dashboardTmpl.Execute(w, data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// socialData is what the /social page renders: one row per series
// describing its ActivityPub and AT Proto/Bluesky bridges. Built
// incrementally across the v2 milestones — see the plan for what each
// stage adds (follower counts, delivery timestamps, bridged-comment
// counts, Bluesky status).
type socialData struct {
	PublicHost string
	Series     []socialRow
	SourceURL  string
}

type socialRow struct {
	Name         string
	Handle       string // "series@host"
	WebfingerURL string
	ActorURL     string
	OutboxURL    string
}

func (t *Tracker) serveSocial(w http.ResponseWriter, r *http.Request) {
	t.mu.Lock()
	topology := t.topology
	publicHost := t.publicHost
	t.mu.Unlock()

	if publicHost == "" {
		http.NotFound(w, r)
		return
	}

	names := make([]string, 0, len(topology))
	for name := range topology {
		names = append(names, name)
	}
	sort.Strings(names)

	rows := make([]socialRow, 0, len(names))
	for _, name := range names {
		actorURL := "https://" + publicHost + "/actors/" + name
		rows = append(rows, socialRow{
			Name:         name,
			Handle:       name + "@" + publicHost,
			WebfingerURL: "https://" + publicHost + "/.well-known/webfinger?resource=acct:" + name + "@" + publicHost,
			ActorURL:     actorURL,
			OutboxURL:    actorURL + "/outbox",
		})
	}

	data := socialData{PublicHost: publicHost, Series: rows, SourceURL: t.sourceURL}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err := socialTmpl.Execute(w, data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

type dashboardData struct {
	Series        []seriesRow
	Activity      []commitLine
	SourceURL     string
	SocialEnabled bool
}

// pairStatus is one badge in the dashboard's top status card: one per
// series (repo), OK only if every pair mirroring that repo's last pass was
// clean — grouped so a repo with 3 mirrored sides (e.g. two Forgejos plus
// an external instance) shows one badge, not three.
type pairStatus struct {
	Name string
	OK   bool
}

func groupPairStatus(results []PairResult) []pairStatus {
	order := []string{}
	ok := map[string]bool{}
	for _, r := range results {
		name := r.Series
		if name == "" {
			name = r.Name
		}
		if _, seen := ok[name]; !seen {
			order = append(order, name)
			ok[name] = true
		}
		if !r.ok() {
			ok[name] = false
		}
	}
	sort.Strings(order)
	out := make([]pairStatus, 0, len(order))
	for _, name := range order {
		out = append(out, pairStatus{Name: name, OK: ok[name]})
	}
	return out
}

// seriesRow is one full-width strip of the activity heatmap: everything
// mirrored for one underlying repository (grouped across its Forgejo and
// Radicle sides via config.RepoPair.Series), one cell per actual event —
// not one per day — so nothing is ever aggregated behind a single tooltip.
type seriesRow struct {
	Name    string
	OK      bool
	SubRows []subRow
}

// subRow is one mirrored side of a series — one real repo (a specific
// Forgejo instance, or a specific Radicle node) — shown once the series is
// expanded, with only that side's own events and its own link. A series
// with 3 mirrored sides (two Forgejos plus Radicle) gets 3 sub-rows, never
// one link picked arbitrarily to represent all of them.
type subRow struct {
	Label  string
	URL    string
	Events []event
}

// event is one activity_log row: one heatmap cell, one tooltip, one link.
type event struct {
	Kind   string // "git", "issue", "patch" — drives the cell's color
	Label  string // "Commit", "Issue", "Patch"
	Text   string
	URL    string
	Server string // which mirrored side this happened on, e.g. "f1", "alice"
	When   string // human-readable timestamp, for the tooltip
}

type commitLine struct {
	When    string
	Pair    string
	PairURL string
	Server  string
	Kind    string // "Commit", "Issue", "Patch"
	Message string
	URL     string
}

// serverLabel names which side of a series a repo_pair belongs to, e.g.
// "federation-x-f1" under series "federation-x" becomes "f1". Several pairs
// share one series row/badge, so without this an operator can't tell which
// mirrored server (f1, f2, an external instance...) a given event actually
// happened on. Empty if the pair name doesn't start with its series name.
func serverLabel(repoPair, series string) string {
	if series == "" {
		return ""
	}
	suffix := strings.TrimPrefix(repoPair, series+"-")
	if suffix == repoPair {
		return ""
	}
	return suffix
}

// hostLabel shortens a URL's host to its canonical name for compact
// display: the TLD is dropped ("f1.cyberwild.org" -> "f1.cyberwild",
// "artefacts.bimr.net" -> "artefacts.bimr"), so every server — ours or an
// external instance's — reads the same way, never a per-pair alias.
func hostLabel(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return ""
	}
	host := u.Host
	// A Radicle Explorer URL's host is always the shared frontend
	// (e.g. radicle.cyberwild.org) — the actual node lives in the path,
	// /nodes/<node-host>/<rid>/... — so different Radicle nodes get
	// distinct labels instead of collapsing into one shared one.
	if segs := strings.Split(strings.TrimPrefix(u.Path, "/"), "/"); len(segs) >= 2 && segs[0] == "nodes" {
		host = segs[1]
	}
	return host
}

// buildSeriesRows groups entries by their Series (one row per monitored
// repo, spanning both its Forgejo and Radicle sides) — Atlassian
// Statuspage style: one full-width horizontal strip per series. Each entry
// becomes its own cell, oldest first, so a busy day never hides events
// behind one shared tooltip.
func buildSeriesRows(entries []state.ActivityEntry, health map[string]bool, topology map[string][]ServerRef) []seriesRow {
	type subAcc struct {
		url    string
		events []state.ActivityEntry
	}
	type seriesAcc struct {
		subOrder []string
		subs     map[string]*subAcc
	}
	seriesAccs := map[string]*seriesAcc{}
	var seriesOrder []string

	ensureSeries := func(name string) *seriesAcc {
		sa, ok := seriesAccs[name]
		if !ok {
			sa = &seriesAcc{subs: map[string]*subAcc{}}
			seriesAccs[name] = sa
			seriesOrder = append(seriesOrder, name)
		}
		return sa
	}
	ensureSub := func(sa *seriesAcc, label string) *subAcc {
		sub, ok := sa.subs[label]
		if !ok {
			sub = &subAcc{}
			sa.subs[label] = sub
			sa.subOrder = append(sa.subOrder, label)
		}
		return sub
	}

	// Seed every configured mirror target first, so a side that has never
	// produced an event (e.g. a Radicle node nothing has been pushed to
	// yet) still gets its own row instead of being invisible.
	for name, refs := range topology {
		sa := ensureSeries(name)
		for _, ref := range refs {
			sub := ensureSub(sa, ref.Label)
			if sub.url == "" {
				sub.url = ref.URL
			}
		}
	}

	for _, e := range entries {
		name := e.Series
		if name == "" {
			name = e.RepoPair
		}
		sa := ensureSeries(name)
		server := hostLabel(e.URL)
		if server == "" {
			server = serverLabel(e.RepoPair, e.Series)
		}
		if server == "" {
			server = e.RepoPair
		}
		sub := ensureSub(sa, server)
		if sub.url == "" {
			sub.url = repoRootURL(e.URL)
		}
		sub.events = append(sub.events, e)
	}
	sort.Strings(seriesOrder)

	rows := make([]seriesRow, 0, len(seriesOrder))
	for _, name := range seriesOrder {
		sa := seriesAccs[name]
		subOrder := append([]string(nil), sa.subOrder...)
		sort.Strings(subOrder)

		subRows := make([]subRow, 0, len(subOrder))
		for _, label := range subOrder {
			sub := sa.subs[label]
			// entries arrive newest-first (see ActivitySince); the
			// heatmap reads left-to-right as oldest-to-newest.
			es := make([]state.ActivityEntry, len(sub.events))
			copy(es, sub.events)
			sort.Slice(es, func(i, j int) bool { return es[i].OccurredAt.Before(es[j].OccurredAt) })
			subRows = append(subRows, subRow{Label: label, URL: sub.url, Events: toEvents(es)})
		}

		ok, known := health[name]
		if !known {
			ok = true
		}
		rows = append(rows, seriesRow{Name: name, OK: ok, SubRows: subRows})
	}
	return rows
}

func toEvents(es []state.ActivityEntry) []event {
	out := make([]event, 0, len(es))
	for _, e := range es {
		server := hostLabel(e.URL)
		if server == "" {
			server = serverLabel(e.RepoPair, e.Series)
		}
		out = append(out, event{
			Kind:   e.Kind,
			Label:  kindLabel(e.Kind),
			Text:   e.Summary,
			URL:    e.URL,
			Server: server,
			When:   e.OccurredAt.Format("Mon, Jan 2, 15:04"),
		})
	}
	return out
}

func kindLabel(kind string) string {
	switch kind {
	case "git":
		return "Commit"
	case "issue":
		return "Issue"
	case "patch":
		return "Patch"
	default:
		return kind
	}
}

// recentActivity lists every mirrored event — commits, issues, and
// patches/pull requests alike, newest first. graft doesn't mirror stars or
// other social metadata: its sync scope is git content, issues, and
// patches/PRs only (see config.SyncScope), so that's what shows up here.
func recentActivity(entries []state.ActivityEntry) []commitLine {
	out := make([]commitLine, 0, len(entries))
	for _, e := range entries {
		name := e.Series
		if name == "" {
			name = e.RepoPair
		}
		server := hostLabel(e.URL)
		if server == "" {
			server = serverLabel(e.RepoPair, e.Series)
		}
		// The (server) parenthetical should point at that specific
		// server's own repo, not the series' shared Radicle explorer
		// link (which is fixed to whichever pair logged first).
		pairURL := repoRootURL(e.URL)
		if pairURL == "" {
			pairURL = e.SeriesURL
		}
		out = append(out, commitLine{
			When:    e.OccurredAt.Format("Jan 2, 15:04"),
			Pair:    name,
			PairURL: pairURL,
			Server:  server,
			Kind:    kindLabel(e.Kind),
			Message: e.Summary,
			URL:     e.URL,
		})
	}
	return out
}

// repoRootURL trims an event's specific-item URL (a commit, issue, or
// patch/PR page) back down to its repository root, e.g.
// ".../owner/repo/commit/<sha>" -> ".../owner/repo". Empty if rawURL is
// empty or doesn't contain a recognized item path.
func repoRootURL(rawURL string) string {
	// Forgejo uses singular "/commit/"; Radicle Explorer uses plural
	// "/commits/" — both must be recognized, or every Radicle-hosted
	// commit link falls back to a non-clickable label.
	for _, marker := range []string{"/commit/", "/commits/", "/issues/", "/pulls/", "/patches/"} {
		if idx := strings.Index(rawURL, marker); idx >= 0 {
			return rawURL[:idx]
		}
	}
	return ""
}

var dashboardTmpl = template.Must(template.New("dashboard").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>graft</title>
<style>
  :root {
    color-scheme: light;
    --surface:      #ffffff;
    --surface-sunk:  #f4f5f7;
    --text:         #172b4d;
    --text-muted:   #6b778c;
    --border:       #dfe1e6;
    --brand:        #0052cc;
    --brand-dark:   #0747a6;
    --kind-git:     #0052cc;
    --kind-issue:   #00875a;
    --kind-patch:   #6554c0;
    --bad:          #de350b;
    --good:         #36b37e;
  }
  * { box-sizing: border-box; }
  body {
    margin: 0;
    background: var(--surface-sunk);
    color: var(--text);
    font: 14px/1.5 -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, Helvetica, Arial, sans-serif;
  }
  main { max-width: 920px; margin: 0 auto; padding: 2.5rem 1.5rem 1rem; }

  .top { display: flex; align-items: baseline; justify-content: space-between; margin-bottom: 1.5rem; }
  .top > div { display: flex; align-items: baseline; gap: .6rem; }
  h1 { font-size: 1.25rem; margin: 0; font-weight: 600; }
  .sub { color: var(--text-muted); font-size: .85rem; }
  .nav-link { color: var(--brand); font-size: .82rem; text-decoration: none; font-weight: 500; }
  .nav-link:hover { text-decoration: underline; }

  .card {
    background: var(--surface);
    border: 1px solid var(--border);
    border-radius: 4px;
    padding: 1.25rem 1.5rem;
    margin-bottom: 1.25rem;
  }

  .dot { width: 7px; height: 7px; border-radius: 50%; background: var(--good); flex: none; display: inline-block; }
  .dot.bad { background: var(--bad); }

  .heatmap-head { display: flex; align-items: baseline; justify-content: space-between; }
  .heatmap-head h2 { margin: 0; }
  .legend { display: flex; gap: .9rem; margin-bottom: .9rem; }
  .legend span { display: flex; align-items: center; gap: .35rem; font-size: .74rem; color: var(--text-muted); }
  .legend .sw { width: 8px; height: 8px; border-radius: 2px; display: inline-block; }
  .legend .sw[data-kind="git"] { background: var(--kind-git); }
  .legend .sw[data-kind="issue"] { background: var(--kind-issue); }
  .legend .sw[data-kind="patch"] { background: var(--kind-patch); }
  .heatmap { display: flex; flex-direction: column; gap: .15rem; }
  .series-group { border-bottom: 1px solid var(--border); }
  .series-group:last-child { border-bottom: none; }
  .series-group[open] { padding-bottom: .5rem; }
  .series-label {
    display: flex; align-items: center; gap: .45rem; padding: .55rem 0;
    cursor: pointer; list-style: none;
  }
  .series-label::-webkit-details-marker { display: none; }
  .series-label::before {
    content: ""; width: 0; height: 0; flex: none;
    border-style: solid; border-width: 4px 0 4px 5px;
    border-color: transparent transparent transparent var(--text-muted);
    transition: transform .12s ease;
  }
  .series-group[open] > .series-label::before { transform: rotate(90deg); }
  .series-name { font-size: .82rem; font-weight: 600; color: var(--text); }
  .series-count { font-size: .74rem; color: var(--text-muted); }
  .subrows { display: flex; flex-direction: column; gap: .5rem; padding-left: 1.3rem; }
  .subrow { display: flex; align-items: center; gap: .75rem; }
  .subrow-name {
    flex: 0 0 11rem; font-size: .78rem; color: var(--text-muted); text-decoration: none;
    overflow: hidden; text-overflow: ellipsis; white-space: nowrap;
  }
  a.subrow-name { color: var(--brand); }
  a.subrow-name:hover { text-decoration: underline; }
  .days { display: flex; gap: 3px; flex-wrap: wrap; }
  .cell {
    flex: none; width: 10px; height: 22px; border-radius: 3px;
    background: var(--border); position: relative; cursor: default;
  }
  .cell[data-kind="git"] { background: var(--kind-git); }
  .cell[data-kind="issue"] { background: var(--kind-issue); }
  .cell[data-kind="patch"] { background: var(--kind-patch); }

  .tip {
    display: none; position: absolute; top: calc(100% + 8px); left: 50%; transform: translateX(-50%);
    background: var(--text); color: #fff; border-radius: 6px;
    font-size: .78rem; z-index: 20; padding: .5rem 0;
    box-shadow: 0 4px 12px rgba(9,30,66,.25);
    width: max-content; max-width: min(320px, 90vw);
  }
  .cell:nth-child(-n+2) .tip { left: 0; transform: none; }
  .cell:nth-last-child(-n+2) .tip { left: auto; right: 0; transform: none; }
  .cell:hover .tip, .tip:hover { display: block; }
  .tip .tip-date { padding: 0 .75rem .35rem; font-weight: 600; border-bottom: 1px solid rgba(255,255,255,.15); margin-bottom: .35rem; }
  .tip a.tip-row {
    display: flex; gap: .4em; padding: .3rem .75rem; color: #fff; text-decoration: none;
    white-space: normal; overflow-wrap: anywhere; line-height: 1.35;
  }
  .tip a.tip-row:hover { background: rgba(255,255,255,.12); }
  .tip .tip-kind { color: #b3bac5; flex: none; }
  .tip .tip-empty { padding: 0 .75rem; color: #b3bac5; }

  h2 { font-size: .78rem; text-transform: uppercase; letter-spacing: .04em; color: var(--text-muted); margin: 0 0 .9rem; font-weight: 600; }
  .commits { border-top: 1px solid var(--border); }
  .commit {
    display: flex; align-items: center; gap: .75rem; padding: .55rem 0; border-bottom: 1px solid var(--border);
    font-size: .82rem;
  }
  .commit .when { color: var(--text-muted); flex: none; width: 9em; }
  .commit .kind { color: var(--text-muted); flex: none; width: 4.5em; font-size: .74rem; text-transform: uppercase; letter-spacing: .03em; }
  .commit .pair {
    color: var(--text-muted); flex: 0 1 auto; max-width: 22em; overflow: hidden; text-overflow: ellipsis; white-space: nowrap;
    text-decoration: none;
  }
  .commit a.pair { color: var(--brand); }
  .commit a.pair:hover { text-decoration: underline; }
  .commit .msg {
    flex: 1 1 auto; min-width: 0;
    overflow: hidden; text-overflow: ellipsis; white-space: nowrap; font-family: ui-monospace, monospace;
    color: inherit; text-decoration: none;
  }
  .commit a.msg { color: var(--brand); }
  .commit a.msg:hover { text-decoration: underline; }
  .empty { color: var(--text-muted); padding: .5rem 0; }

  footer {
    max-width: 920px; margin: 0 auto; padding: 1rem 1.5rem 2.5rem;
    color: var(--text-muted); font-size: .78rem; display: flex; gap: 1rem;
  }
  footer a { color: var(--text-muted); }
</style>
</head>
<body>
<main>
  <div class="top">
    <div>
      <h1>graft</h1>
      <span class="sub">Forgejo &lt;-&gt; Radicle sync — last 3 days</span>
    </div>
    {{if .SocialEnabled}}<a class="nav-link" href="/social">Social bridges &rarr;</a>{{end}}
  </div>

  <div class="card">
    <div class="heatmap-head">
      <h2>Activity by repo</h2>
      <div class="legend">
        <span><i class="sw" data-kind="git"></i>Commit</span>
        <span><i class="sw" data-kind="issue"></i>Issue</span>
        <span><i class="sw" data-kind="patch"></i>Patch</span>
      </div>
    </div>
    <div class="heatmap">
    {{range .Series}}
      <details class="series-group">
        <summary class="series-label">
          <span class="dot{{if not .OK}} bad{{end}}"></span>
          <span class="series-name">{{.Name}}</span>
          <span class="series-count">{{len .SubRows}} mirrored side{{if ne (len .SubRows) 1}}s{{end}}</span>
        </summary>
        <div class="subrows">
        {{range .SubRows}}
          <div class="subrow">
            {{if .URL}}
            <a class="subrow-name" href="{{.URL}}" target="_blank" rel="noopener">{{.Label}}</a>
            {{else}}
            <span class="subrow-name">{{.Label}}</span>
            {{end}}
            <div class="days">
            {{range .Events}}
              <div class="cell" data-kind="{{.Kind}}">
                <div class="tip">
                  <div class="tip-date">{{.When}}</div>
                  {{if .URL}}
                  <a class="tip-row" href="{{.URL}}" target="_blank" rel="noopener"><span class="tip-kind">{{.Label}}</span>{{.Text}}</a>
                  {{else}}
                  <span class="tip-row"><span class="tip-kind">{{.Label}}</span>{{.Text}}</span>
                  {{end}}
                </div>
              </div>
            {{else}}
              <span class="tip-empty">nothing synced</span>
            {{end}}
            </div>
          </div>
        {{end}}
        </div>
      </details>
    {{else}}
      <span class="empty">nothing synced yet</span>
    {{end}}
    </div>
  </div>

  <div class="card">
    <h2>Recent activity</h2>
    <div class="commits">
    {{range .Activity}}
      <div class="commit">
        <span class="when">{{.When}}</span>
        <span class="kind">{{.Kind}}</span>
        {{if .PairURL}}
        <a class="pair" href="{{.PairURL}}" target="_blank" rel="noopener">{{.Pair}}{{if .Server}} ({{.Server}}){{end}}</a>
        {{else}}
        <span class="pair">{{.Pair}}{{if .Server}} ({{.Server}}){{end}}</span>
        {{end}}
        {{if .URL}}
        <a class="msg" href="{{.URL}}" target="_blank" rel="noopener">{{.Message}}</a>
        {{else}}
        <span class="msg">{{.Message}}</span>
        {{end}}
      </div>
    {{else}}
      <p class="empty">nothing mirrored yet</p>
    {{end}}
    </div>
  </div>
</main>
<footer>
  {{if .SourceURL}}<a href="{{.SourceURL}}" target="_blank" rel="noopener">{{.SourceURL}}</a>{{end}}
  <span>GPLv3</span>
</footer>
</body>
</html>
`))

var socialTmpl = template.Must(template.New("social").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>graft — social bridges</title>
<style>
  :root {
    color-scheme: light;
    --surface:      #ffffff;
    --surface-sunk:  #f4f5f7;
    --text:         #172b4d;
    --text-muted:   #6b778c;
    --border:       #dfe1e6;
    --brand:        #0052cc;
  }
  * { box-sizing: border-box; }
  body {
    margin: 0; background: var(--surface-sunk); color: var(--text);
    font: 14px/1.5 -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, Helvetica, Arial, sans-serif;
  }
  main { max-width: 760px; margin: 0 auto; padding: 2.5rem 1.5rem 1rem; }
  .top { display: flex; align-items: baseline; justify-content: space-between; margin-bottom: 1.5rem; }
  .top > div { display: flex; align-items: baseline; gap: .6rem; }
  h1 { font-size: 1.25rem; margin: 0; font-weight: 600; }
  .sub { color: var(--text-muted); font-size: .85rem; }
  .nav-link { color: var(--brand); font-size: .82rem; text-decoration: none; font-weight: 500; }
  .nav-link:hover { text-decoration: underline; }
  .card {
    background: var(--surface); border: 1px solid var(--border); border-radius: 4px;
    padding: 1.25rem 1.5rem; margin-bottom: 1.25rem;
  }
  .series-block { padding: .75rem 0; border-bottom: 1px solid var(--border); }
  .series-block:last-child { border-bottom: none; }
  .handle { font-size: .95rem; font-weight: 600; color: var(--text); }
  h2 { font-size: .78rem; text-transform: uppercase; letter-spacing: .04em; color: var(--text-muted); margin: .6rem 0 .4rem; font-weight: 600; }
  .endpoints { display: flex; flex-direction: column; gap: .3rem; }
  .endpoints a { font-size: .8rem; color: var(--brand); text-decoration: none; font-family: ui-monospace, monospace; }
  .endpoints a:hover { text-decoration: underline; }
  .badge {
    display: inline-block; font-size: .72rem; color: var(--text-muted); background: var(--surface-sunk);
    border: 1px solid var(--border); border-radius: 3px; padding: .15rem .5rem; margin-top: .4rem;
  }
  .empty { color: var(--text-muted); padding: .5rem 0; }
  footer {
    max-width: 760px; margin: 0 auto; padding: 1rem 1.5rem 2.5rem;
    color: var(--text-muted); font-size: .78rem; display: flex; gap: 1rem;
  }
  footer a { color: var(--text-muted); }
</style>
</head>
<body>
<main>
  <div class="top">
    <div>
      <h1>graft — social bridges</h1>
      <span class="sub">ActivityPub + AT Proto, per repo</span>
    </div>
    <a class="nav-link" href="/">&larr; Dashboard</a>
  </div>

  <div class="card">
  {{range .Series}}
    <div class="series-block">
      <div class="handle">@{{.Handle}}</div>
      <h2>ActivityPub</h2>
      <div class="endpoints">
        <a href="{{.WebfingerURL}}" target="_blank" rel="noopener">WebFinger</a>
        <a href="{{.ActorURL}}" target="_blank" rel="noopener">Actor</a>
        <a href="{{.OutboxURL}}" target="_blank" rel="noopener">Outbox</a>
      </div>
      <h2>AT Proto / Bluesky</h2>
      <span class="badge">not configured</span>
    </div>
  {{else}}
    <span class="empty">no series configured</span>
  {{end}}
  </div>
</main>
<footer>
  {{if .SourceURL}}<a href="{{.SourceURL}}" target="_blank" rel="noopener">{{.SourceURL}}</a>{{end}}
  <span>GPLv3</span>
</footer>
</body>
</html>
`))
