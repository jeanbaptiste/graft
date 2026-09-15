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

func TestAuthorProfileLinks(t *testing.T) {
	defer func() { profileLink = nil }()
	cases := []struct{ platform, in, want string }{
		{"Discourse", "**@pipeline-test-bot** wrote:", `<strong><a href="https://discourse.example/u/pipeline-test-bot">@pipeline-test-bot</a></strong> wrote:`},
		{"Bluesky", "issue · **@juliette.pds.example**", `issue · <strong><a href="https://bsky.app/profile/juliette.pds.example">@juliette.pds.example</a></strong>`},
		{"Mastodon", "**@lea.moreau@social.example** wrote:", `<strong><a href="https://social.example/@lea.moreau">@lea.moreau@social.example</a></strong> wrote:`},
		{"Mastodon", "**@researcher** wrote:", `<strong>@researcher</strong> wrote:`},
		{"Zulip", "**@thomas.k** wrote:", `<strong>@thomas.k</strong> wrote:`},
	}
	for _, c := range cases {
		profileLink = profileLinker(c.platform, "https://discourse.example", "https://tangled.example")
		if got := inline(c.in); got != c.want {
			t.Errorf("%s: inline(%q)\n got  %q\n want %q", c.platform, c.in, got, c.want)
		}
	}
}
