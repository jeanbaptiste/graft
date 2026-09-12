// Package status tracks the outcome of each repo pair's most recent sync
// pass, keeps a small dashboard over the activity log, and serves both over
// HTTP — so an operator can tell the daemon is alive and see what it's
// actually done without reading logs.
package status

import (
	"encoding/json"
	"html/template"
	"net/http"
	"sort"
	"sync"
	"time"

	"graft/internal/state"
)

// PairResult is the outcome of one repo pair's most recent pass. An empty
// error string means that scope is either disabled or succeeded.
type PairResult struct {
	Name        string    `json:"name"`
	LastRun     time.Time `json:"last_run"`
	GitError    string    `json:"git_error,omitempty"`
	IssuesError string    `json:"issues_error,omitempty"`
	PatchError  string    `json:"patch_error,omitempty"`
}

func (r PairResult) ok() bool {
	return r.GitError == "" && r.IssuesError == "" && r.PatchError == ""
}

// Tracker holds the latest result per repo pair, safe for concurrent use,
// plus a handle on the state store for the activity dashboard.
type Tracker struct {
	startedAt time.Time
	store     *state.Store
	sourceURL string

	mu    sync.Mutex
	pairs map[string]PairResult
}

func NewTracker(store *state.Store, sourceURL string) *Tracker {
	return &Tracker{startedAt: time.Now(), store: store, sourceURL: sourceURL, pairs: map[string]PairResult{}}
}

// Record stores the outcome of a pass for one pair. Pass nil for a scope
// that succeeded or wasn't enabled.
func (t *Tracker) Record(name string, gitErr, issuesErr, patchErr error) {
	r := PairResult{Name: name, LastRun: time.Now()}
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

	return mux
}

const dashboardDays = 90

func (t *Tracker) serveDashboard(w http.ResponseWriter, r *http.Request) {
	since := time.Now().UTC().AddDate(0, 0, -dashboardDays).Truncate(24 * time.Hour)
	entries, err := t.store.ActivitySince(since)
	if err != nil {
		http.Error(w, "load activity: "+err.Error(), http.StatusInternalServerError)
		return
	}

	data := dashboardData{
		Pairs:     t.snapshot(),
		Weeks:     buildCalendar(entries, since),
		Commits:   recentCommits(entries),
		SourceURL: t.sourceURL,
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := dashboardTmpl.Execute(w, data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

type dashboardData struct {
	Pairs     []PairResult
	Weeks     [][]day
	Commits   []commitLine
	SourceURL string
}

type day struct {
	Date    string // YYYY-MM-DD, human-readable
	Count   int
	Level   int // 0-4, for the color ramp
	Events  []event
	InRange bool
}

// event is one activity_log row, shaped for the tooltip's clickable list.
type event struct {
	Label string // "3 commits", "1 issue", etc. joined for the summary line
	Text  string
	URL   string
}

type commitLine struct {
	When    string
	Pair    string
	Message string
	URL     string
}

// buildCalendar buckets entries by day and lays them out GitHub-style:
// one column per week, Sunday to Saturday down each column.
func buildCalendar(entries []state.ActivityEntry, since time.Time) [][]day {
	byDay := map[string][]state.ActivityEntry{}
	for _, e := range entries {
		key := e.OccurredAt.Format("2006-01-02")
		byDay[key] = append(byDay[key], e)
	}

	start := since
	for start.Weekday() != time.Sunday {
		start = start.AddDate(0, 0, -1)
	}
	today := time.Now().UTC().Truncate(24 * time.Hour)

	var weeks [][]day
	var week []day
	for d := start; !d.After(today); d = d.AddDate(0, 0, 1) {
		key := d.Format("2006-01-02")
		es := byDay[key]
		week = append(week, day{
			Date:    d.Format("Jan 2, 2006"),
			Count:   len(es),
			Level:   levelFor(len(es)),
			Events:  toEvents(es),
			InRange: !d.Before(since),
		})
		if d.Weekday() == time.Saturday {
			weeks = append(weeks, week)
			week = nil
		}
	}
	if len(week) > 0 {
		weeks = append(weeks, week)
	}
	return weeks
}

func levelFor(count int) int {
	switch {
	case count == 0:
		return 0
	case count <= 2:
		return 1
	case count <= 5:
		return 2
	case count <= 10:
		return 3
	default:
		return 4
	}
}

func toEvents(es []state.ActivityEntry) []event {
	out := make([]event, 0, len(es))
	for _, e := range es {
		out = append(out, event{
			Label: kindLabel(e.Kind),
			Text:  e.Summary,
			URL:   e.URL,
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

func recentCommits(entries []state.ActivityEntry) []commitLine {
	var out []commitLine
	for _, e := range entries {
		if e.Kind != "git" {
			continue
		}
		out = append(out, commitLine{
			When:    e.OccurredAt.Format("Jan 2, 15:04"),
			Pair:    e.RepoPair,
			Message: e.Summary,
			URL:     e.URL,
		})
	}
	return out
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
    --cell-0:       #f4f5f7;
    --cell-1:       #cde2fb;
    --cell-2:       #86b6ef;
    --cell-3:       #3987e5;
    --cell-4:       #184f95;
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
  h1 { font-size: 1.25rem; margin: 0; font-weight: 600; }
  .sub { color: var(--text-muted); font-size: .85rem; }

  .card {
    background: var(--surface);
    border: 1px solid var(--border);
    border-radius: 4px;
    padding: 1.25rem 1.5rem;
    margin-bottom: 1.25rem;
  }

  .pairs { display: flex; flex-wrap: wrap; gap: .5rem; }
  .pair {
    border: 1px solid var(--border); border-radius: 3px; background: var(--surface-sunk);
    padding: .35rem .65rem; font-size: .78rem; display: flex; align-items: center; gap: .4rem;
  }
  .dot { width: 8px; height: 8px; border-radius: 50%; background: var(--good); flex: none; }
  .dot.bad { background: var(--bad); }

  .calendar-scroll { overflow-x: auto; overflow-y: visible; padding: 20px 0 4px; }
  .calendar { display: flex; gap: 3px; width: max-content; }
  .week { display: flex; flex-direction: column; gap: 3px; }
  .cell {
    width: 12px; height: 12px; border-radius: 2px;
    background: var(--cell-0); position: relative; cursor: default;
  }
  .cell[data-level="1"] { background: var(--cell-1); }
  .cell[data-level="2"] { background: var(--cell-2); }
  .cell[data-level="3"] { background: var(--cell-3); }
  .cell[data-level="4"] { background: var(--cell-4); }
  .cell[data-out-of-range="1"] { visibility: hidden; }

  .tip {
    display: none; position: absolute; bottom: calc(100% + 6px); left: 50%; transform: translateX(-50%);
    background: var(--text); color: #fff; border-radius: 6px;
    font-size: .78rem; white-space: nowrap; z-index: 10; padding: .5rem 0;
    box-shadow: 0 4px 12px rgba(9,30,66,.25);
  }
  .cell:hover .tip, .tip:hover { display: block; }
  .tip .tip-date { padding: 0 .75rem .35rem; font-weight: 600; border-bottom: 1px solid rgba(255,255,255,.15); margin-bottom: .35rem; }
  .tip a.tip-row {
    display: block; padding: .3rem .75rem; color: #fff; text-decoration: none; white-space: nowrap;
  }
  .tip a.tip-row:hover { background: rgba(255,255,255,.12); }
  .tip .tip-kind { color: #b3bac5; margin-right: .4em; }
  .tip .tip-empty { padding: 0 .75rem; color: #b3bac5; }

  h2 { font-size: .78rem; text-transform: uppercase; letter-spacing: .04em; color: var(--text-muted); margin: 0 0 .9rem; font-weight: 600; }
  .commits { border-top: 1px solid var(--border); }
  a.commit {
    display: flex; gap: .75rem; padding: .55rem 0; border-bottom: 1px solid var(--border);
    font-size: .82rem; color: inherit; text-decoration: none;
  }
  a.commit:hover .msg { color: var(--brand); text-decoration: underline; }
  .commit .when { color: var(--text-muted); flex: none; width: 9em; }
  .commit .pair { color: var(--text-muted); flex: none; width: 10em; }
  .commit .msg { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; font-family: ui-monospace, monospace; }
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
    <h1>graft</h1>
    <span class="sub">Forgejo &lt;-&gt; Radicle sync — last 90 days</span>
  </div>

  <div class="card">
    <div class="pairs">
    {{range .Pairs}}
      <div class="pair">
        <span class="dot{{if or .GitError .IssuesError .PatchError}} bad{{end}}"></span>
        {{.Name}}
      </div>
    {{else}}
      <span class="empty">no pairs configured</span>
    {{end}}
    </div>
  </div>

  <div class="card">
    <div class="calendar-scroll">
      <div class="calendar">
      {{range .Weeks}}
        <div class="week">
        {{range .}}
          <div class="cell" data-level="{{.Level}}" data-out-of-range="{{if not .InRange}}1{{end}}">
            <div class="tip">
              <div class="tip-date">{{.Date}}</div>
              {{range .Events}}
                {{if .URL}}
                <a class="tip-row" href="{{.URL}}" target="_blank" rel="noopener"><span class="tip-kind">{{.Label}}</span>{{.Text}}</a>
                {{else}}
                <span class="tip-row"><span class="tip-kind">{{.Label}}</span>{{.Text}}</span>
                {{end}}
              {{else}}
                <span class="tip-empty">nothing synced</span>
              {{end}}
            </div>
          </div>
        {{end}}
        </div>
      {{end}}
      </div>
    </div>
  </div>

  <div class="card">
    <h2>Recent commits</h2>
    <div class="commits">
    {{range .Commits}}
      {{if .URL}}
      <a class="commit" href="{{.URL}}" target="_blank" rel="noopener">
        <span class="when">{{.When}}</span>
        <span class="pair">{{.Pair}}</span>
        <span class="msg">{{.Message}}</span>
      </a>
      {{else}}
      <div class="commit">
        <span class="when">{{.When}}</span>
        <span class="pair">{{.Pair}}</span>
        <span class="msg">{{.Message}}</span>
      </div>
      {{end}}
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
