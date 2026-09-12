package activitypub

import (
	"html"
	"regexp"
	"strings"
)

var (
	tagRe      = regexp.MustCompile(`<[^>]*>`)
	blockEndRe = regexp.MustCompile(`</(p|br|div)\s*>`)
)

// stripHTML turns Mastodon's rich-text Note content (HTML, e.g.
// "<p>hello <a href=...>@bot</a></p>") into plain text suitable for a
// Forgejo/Radicle comment body. Not a sanitizer — this only ever displays
// the result as inert markdown-adjacent plain text, never re-renders it as
// HTML, so no XSS surface is introduced by skipping a full parser here.
func stripHTML(s string) string {
	s = blockEndRe.ReplaceAllString(s, "\n")
	s = tagRe.ReplaceAllString(s, "")
	s = html.UnescapeString(s)
	return strings.TrimSpace(s)
}
