package wiki

import (
	"strings"
	"testing"
)

func TestRemoveFooterStripsEveryKnownFormat(t *testing.T) {
	entries := "### **2026-09-15 10:00 UTC**\n\n[x](u) &middot; issue &middot; **@a**\n\n[→ Discourse conversation](https://d/t/s/1/2)\n\n---\n"
	stacked := entries +
		"\n\n---\n\n_Page generée par [graft](https://github.com/jeanbaptiste/graft) le 2026-09-15 09:00 UTC_" +
		"\n\n<!-- graft-sync @ 2026-09-15 09:30 UTC -->\n" +
		"\n\n---\n\n*Last updated: 2026-09-15 10:00 UTC*\n" +
		"\n\n---\n\n*Last updated: 2026-09-15 11:00 UTC*\n" +
		"\n\n---\n\n_Page mise à jour par graft 2026-09-15 14:30 UTC_\n"

	got := removeFooter(stacked)
	if got != strings.TrimRight(entries, "\n") {
		t.Fatalf("footers not fully removed:\n%q", got)
	}
}

func TestRemoveFooterKeepsContentWithoutFooter(t *testing.T) {
	in := "### entry\n\n> quoted *Last updated: not a footer*\n\nmore text\n\n---\n"
	if got := removeFooter(in); got != strings.TrimRight(in, "\n") {
		t.Fatalf("content altered:\n%q", got)
	}
}

func TestAppendedPageKeepsSingleFooter(t *testing.T) {
	page := "entries\n\n---\n" + pageFooter()
	for i := 0; i < 3; i++ {
		page = removeFooter(page) + pageFooter()
	}
	if n := strings.Count(page, "*Last updated: "); n != 1 {
		t.Fatalf("want exactly one footer, got %d:\n%s", n, page)
	}
}
