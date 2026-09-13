package status

import (
	"testing"
	"time"

	"graft/internal/state"
)

func mkEntry(repoPair, series, kind, url, summary string, t time.Time) state.ActivityEntry {
	return state.ActivityEntry{
		Activity: state.Activity{
			RepoPair: repoPair,
			Series:   series,
			Kind:     kind,
			URL:      url,
			Summary:  summary,
		},
		OccurredAt: t,
	}
}

// TestBuildSeriesRowsAlignsHealthySides verifies the case this test exists
// for: two sides that actually hold the same content, but where graft only
// happened to log the push under one of them, must render the exact same
// number of cells — a real one on the side that logged it, a synthetic one
// (visually identical, only the tooltip differs) on the side that didn't.
func TestBuildSeriesRowsAlignsHealthySides(t *testing.T) {
	base := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	// Both commits are only ever logged under the Forgejo side's own host
	// (f1.example) — mirroring the real-world case that motivated this
	// fix, where the sync code happens to log an event's URL against one
	// particular host even though the content lands everywhere.
	entries := []state.ActivityEntry{
		mkEntry("x-f1", "x", "git", "https://f1.example/x/commit/aaa", "commit aaa", base),
		mkEntry("x-f1", "x", "git", "https://f1.example/x/commit/bbb", "commit bbb", base.Add(time.Minute)),
	}
	topology := map[string][]ServerRef{
		"x": {
			{Label: "f1.example", URL: "https://f1.example/x", PairName: "x-f1"},
			{Label: "r1.example", URL: "https://r1.example/x", Radicle: true, PairName: "x-f1"},
		},
	}
	health := map[string]bool{"x": true}

	rows := buildSeriesRows(entries, health, topology, map[string]PairResult{})
	if len(rows) != 1 {
		t.Fatalf("expected 1 series row, got %d", len(rows))
	}
	sub := map[string]subRow{}
	for _, s := range rows[0].SubRows {
		sub[s.Label] = s
	}
	f1, r1 := sub["f1.example"], sub["r1.example"]
	if len(f1.Events) != len(r1.Events) {
		t.Fatalf("expected aligned cell counts, got f1=%d r1=%d", len(f1.Events), len(r1.Events))
	}
	if len(f1.Events) != 2 {
		t.Fatalf("expected 2 cells per side, got %d", len(f1.Events))
	}
	// f1 logged both itself: both real (State == "").
	for _, e := range f1.Events {
		if e.State != "" {
			t.Errorf("expected f1's own events to be real, got State=%q", e.State)
		}
	}
	// r1 never logged either directly: both synthetic, marked "replicated"
	// since it's the Radicle side — but same count, same order, so the
	// two rows read as aligned rather than one looking "behind".
	for _, e := range r1.Events {
		if e.State != "replicated" {
			t.Errorf("expected r1's filled-in events to be synthetic/replicated, got State=%q", e.State)
		}
	}
}

// TestBuildSeriesRowsSkipsFillOnError verifies the other half: a side whose
// last sync pass actually failed must NOT get the synthetic alignment fill
// — it should show only what it has confirmed itself, plus a non-empty
// subRow.Error, so a real problem stays visible instead of being papered
// over by the alignment logic above.
func TestBuildSeriesRowsSkipsFillOnError(t *testing.T) {
	base := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	entries := []state.ActivityEntry{
		mkEntry("x-f1", "x", "git", "https://f1.example/x/commit/aaa", "commit aaa", base),
		mkEntry("x-f1", "x", "git", "https://f1.example/x/commit/bbb", "commit bbb", base.Add(time.Minute)),
	}
	topology := map[string][]ServerRef{
		"x": {
			{Label: "f1.example", URL: "https://f1.example/x", PairName: "x-f1"},
			{Label: "f2.example", URL: "https://f2.example/x", PairName: "x-f2"},
		},
	}
	health := map[string]bool{"x": false}
	pairResults := map[string]PairResult{
		"x-f2": {Name: "x-f2", GitError: "read forgejo head: 502 Bad Gateway"},
	}

	rows := buildSeriesRows(entries, health, topology, pairResults)
	sub := map[string]subRow{}
	for _, s := range rows[0].SubRows {
		sub[s.Label] = s
	}
	f2 := sub["f2.example"]
	if f2.Error == "" {
		t.Fatal("expected f2's subRow.Error to be set")
	}
	if len(f2.Events) != 0 {
		t.Fatalf("expected no synthetic fill on an errored side, got %d events", len(f2.Events))
	}
	f1 := sub["f1.example"]
	if f1.Error != "" {
		t.Fatalf("expected f1 (healthy) to have no error, got %q", f1.Error)
	}
	if len(f1.Events) != 2 {
		t.Fatalf("expected f1's own 2 real events, got %d", len(f1.Events))
	}
}
