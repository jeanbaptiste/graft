// Package status tracks the outcome of each repo pair's most recent sync
// pass, keeps a small dashboard over the activity log, and serves both over
// HTTP — so an operator can tell the daemon is alive and see what it's
// actually done without reading logs.
package status

import (
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
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

	mu    sync.Mutex
	pairs map[string]PairResult
}

func NewTracker(store *state.Store) *Tracker {
	return &Tracker{startedAt: time.Now(), store: store, pairs: map[string]PairResult{}}
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
//	GET /healthz  - 200 if every pair's last pass was clean, 503 otherwise
//	GET /api/status - the JSON snapshot Record() has been fed
//	GET /        - the activity dashboard (calendar + recent commits)
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
		Pairs:   t.snapshot(),
		Weeks:   buildCalendar(entries, since),
		Commits: recentCommits(entries),
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := dashboardTmpl.Execute(w, data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

type dashboardData struct {
	Pairs   []PairResult
	Weeks   [][]day
	Commits []commitLine
}

type day struct {
	Date    string // YYYY-MM-DD
	Count   int
	Level   int // 0-4, for the color ramp
	Tooltip string
	InRange bool
}

type commitLine struct {
	When    string
	Pair    string
	Message string
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
			Date:    key,
			Count:   len(es),
			Level:   levelFor(len(es)),
			Tooltip: tooltipFor(key, es),
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

func tooltipFor(date string, es []state.ActivityEntry) string {
	if len(es) == 0 {
		return date + ": nothing synced"
	}
	byKind := map[string]int{}
	for _, e := range es {
		byKind[e.Kind]++
	}
	var parts []string
	for _, k := range []string{"git", "issue", "patch"} {
		if n := byKind[k]; n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, plural(k, n)))
		}
	}
	return fmt.Sprintf("%s: %s", date, strings.Join(parts, ", "))
}

func plural(word string, n int) string {
	if n == 1 {
		return word
	}
	if word == "patch" {
		return "patches"
	}
	return word + "s"
}

func recentCommits(entries []state.ActivityEntry) []commitLine {
	var out []commitLine
	for _, e := range entries {
		if e.Kind != "git" {
			continue
		}
		out = append(out, commitLine{
			When:    e.OccurredAt.Format("2006-01-02 15:04"),
			Pair:    e.RepoPair,
			Message: e.Summary,
		})
	}
	return out
}

var dashboardTmpl = template.Must(template.New("dashboard").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>graft</title>
<style>
  :root {
    color-scheme: light;
    --surface:    #fcfcfb;
    --text:       #0b0b0b;
    --text-muted: #52514e;
    --border:     #e4e2dc;
    --cell-0:     #ebedf0;
    --cell-1:     #cde2fb;
    --cell-2:     #86b6ef;
    --cell-3:     #3987e5;
    --cell-4:     #184f95;
    --bad:        #d03b3b;
  }
  @media (prefers-color-scheme: dark) {
    :root {
      color-scheme: dark;
      --surface:    #1a1a19;
      --text:       #ffffff;
      --text-muted: #c3c2b7;
      --border:     #33322e;
      --cell-0:     #22252a;
      --cell-1:     #16324d;
      --cell-2:     #1c5cab;
      --cell-3:     #3987e5;
      --cell-4:     #86b6ef;
      --bad:        #e66767;
    }
  }
  * { box-sizing: border-box; }
  body {
    margin: 0;
    padding: 2.5rem 1.5rem;
    background: var(--surface);
    color: var(--text);
    font: 14px/1.5 -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif;
  }
  main { max-width: 900px; margin: 0 auto; }
  h1 { font-size: 1.1rem; margin: 0 0 .25rem; }
  .sub { color: var(--text-muted); margin: 0 0 2rem; }

  .pairs { display: flex; flex-wrap: wrap; gap: .5rem; margin-bottom: 2rem; }
  .pair {
    border: 1px solid var(--border); border-radius: 6px;
    padding: .4rem .7rem; font-size: .8rem; display: flex; align-items: center; gap: .4rem;
  }
  .dot { width: 8px; height: 8px; border-radius: 50%; background: var(--cell-3); flex: none; }
  .dot.bad { background: var(--bad); }

  .calendar { display: flex; gap: 3px; margin-bottom: 2rem; overflow-x: auto; padding-bottom: .5rem; }
  .week { display: flex; flex-direction: column; gap: 3px; }
  .cell {
    width: 11px; height: 11px; border-radius: 2px;
    background: var(--cell-0); position: relative; cursor: default;
  }
  .cell[data-level="1"] { background: var(--cell-1); }
  .cell[data-level="2"] { background: var(--cell-2); }
  .cell[data-level="3"] { background: var(--cell-3); }
  .cell[data-level="4"] { background: var(--cell-4); }
  .cell[data-out-of-range="1"] { visibility: hidden; }

  .cell .tip {
    display: none; position: absolute; bottom: 150%; left: 50%; transform: translateX(-50%);
    background: var(--text); color: var(--surface); padding: .3rem .5rem; border-radius: 4px;
    font-size: .72rem; white-space: nowrap; z-index: 1; pointer-events: none;
  }
  .cell:hover .tip { display: block; }

  h2 { font-size: .85rem; text-transform: uppercase; letter-spacing: .04em; color: var(--text-muted); margin: 0 0 .75rem; }
  .commits { border-top: 1px solid var(--border); }
  .commit { display: flex; gap: .75rem; padding: .5rem 0; border-bottom: 1px solid var(--border); font-size: .82rem; }
  .commit .when { color: var(--text-muted); flex: none; width: 11em; }
  .commit .pair { color: var(--text-muted); flex: none; width: 10em; }
  .commit .msg { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .empty { color: var(--text-muted); padding: 1rem 0; }
</style>
</head>
<body>
<main>
  <h1>graft</h1>
  <p class="sub">Forgejo &lt;-&gt; Radicle sync — last {{len .Weeks}} weeks</p>

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

  <div class="calendar">
  {{range .Weeks}}
    <div class="week">
    {{range .}}
      <div class="cell" data-level="{{.Level}}" data-out-of-range="{{if not .InRange}}1{{end}}">
        <span class="tip">{{.Tooltip}}</span>
      </div>
    {{end}}
    </div>
  {{end}}
  </div>

  <h2>Recent commits</h2>
  <div class="commits">
  {{range .Commits}}
    <div class="commit">
      <span class="when">{{.When}}</span>
      <span class="pair">{{.Pair}}</span>
      <span class="msg">{{.Message}}</span>
    </div>
  {{else}}
    <p class="empty">nothing mirrored yet</p>
  {{end}}
  </div>
</main>
</body>
</html>
`))
