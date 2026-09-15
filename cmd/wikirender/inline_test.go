package main

import "testing"

func TestInlineRendersTrackbackAndFooter(t *testing.T) {
	cases := map[string]string{
		"[→ Discourse conversation](https://discourse.example/t/slug/1/2)": `<a href="https://discourse.example/t/slug/1/2">→ Discourse conversation</a>`,
		"*Last updated: 2026-09-15 10:00 UTC*":                             `<em>Last updated: 2026-09-15 10:00 UTC</em>`,
		"**@alice** wrote:":                                                `<strong>@alice</strong> wrote:`,
		"2 * 3 * 4":                                                        `2 * 3 * 4`,
		"_Page mise à jour par graft_":                                     `<em>Page mise à jour par graft</em>`,
		"keep snake_case_names intact":                                     `keep snake_case_names intact`,
	}
	for in, want := range cases {
		if got := inline(in); got != want {
			t.Errorf("inline(%q)\n got  %q\n want %q", in, got, want)
		}
	}
}

func TestRenderCollapsesRepeatedRules(t *testing.T) {
	got := renderMarkdown("a\n\n---\n\n---\n\nb")
	if want := "<p>a</p>\n<hr>\n<p>b</p>\n"; got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}
