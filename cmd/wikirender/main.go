// Command wikirender renders every repo's Forgejo wiki pages (Home,
// _Sidebar, Social-*) to standalone static HTML — white background,
// sans-serif, real typography — since Forgejo's own wiki view has no CSS
// of its own and just inherits whatever theme the viewer's account (or
// browser) happens to be in. Meant to run on a timer via systemd, writing
// into a directory a plain static file server serves.
package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

func read(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		fmt.Println("read", path, err)
		os.Exit(1)
	}
	s := string(b)
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}

type wikiPage struct {
	Title  string `json:"title"`
	SubURL string `json:"sub_url"`
}

func listPages(client *http.Client, token, base, owner, repo string) ([]wikiPage, error) {
	req, err := http.NewRequest(http.MethodGet, fmt.Sprintf("%s/api/v1/repos/%s/%s/wiki/pages?limit=50", base, owner, repo), nil)
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(token, "")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("list pages: HTTP %d: %s", resp.StatusCode, string(b))
	}
	var pages []wikiPage
	if err := json.NewDecoder(resp.Body).Decode(&pages); err != nil {
		return nil, err
	}
	return pages, nil
}

func getPageContent(client *http.Client, token, base, owner, repo, subURL string) (string, error) {
	req, err := http.NewRequest(http.MethodGet, fmt.Sprintf("%s/api/v1/repos/%s/%s/wiki/page/%s", base, owner, repo, subURL), nil)
	if err != nil {
		return "", err
	}
	req.SetBasicAuth(token, "")
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("get page: HTTP %d: %s", resp.StatusCode, string(b))
	}
	var out struct {
		ContentBase64 string `json:"content_base64"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	decoded, err := base64.StdEncoding.DecodeString(out.ContentBase64)
	if err != nil {
		return "", err
	}
	return string(decoded), nil
}

// --- a small, deliberately non-general markdown->HTML renderer: it only
// needs to handle the shapes graft's own wiki writer (internal/wiki) and
// cmd/wikihome actually produce (#/##/### headings, **bold**, [text](url)
// links, "> " blockquotes, "---" rules, plain paragraphs, "- " bullet
// lists) — not arbitrary CommonMark. ---

var (
	reBold       = regexp.MustCompile(`\*\*(.+?)\*\*`)
	reLink       = regexp.MustCompile(`\[([^\]]*)\]\(([^)]+)\)`)
	reHeading    = regexp.MustCompile(`^(#{1,3})\s+(.*)$`)
	reBullet     = regexp.MustCompile(`^-\s+(.*)$`)
	reSocialPage = regexp.MustCompile(`^Social-([A-Za-z]+)$`)
)

func inline(s string) string {
	// The wiki source's own literal "&middot;" HTML entity must survive
	// html.EscapeString untouched (it would otherwise double-escape to
	// the literal text "&middot;"), so swap it for the real character
	// first — the only HTML entity graft's own wiki writers ever emit.
	s = strings.ReplaceAll(s, "&middot;", "·")
	s = html.EscapeString(s)
	// html.EscapeString turns "&" in an already-escaped &amp; from a
	// literal & in source text; safe since our source markdown never
	// contains raw HTML entities itself.
	// Home.md (written by cmd/wikihome) links to each platform page as
	// its plain title, e.g. "Social-Bluesky" — correct for Forgejo's own
	// wiki, which resolves titles itself. This mirror instead writes
	// each platform's page as social-<platform>.html (see the fileName
	// computation below) and never goes through Forgejo's own
	// further-mangled slug ("Social-Bluesky.-", see
	// internal/wiki/client.go's slugFor) — so a same-wiki link needs
	// rewriting here or it 404s on a page this mirror never made.
	s = reLink.ReplaceAllStringFunc(s, func(m string) string {
		parts := reLink.FindStringSubmatch(m)
		text, href := parts[1], parts[2]
		if plat := reSocialPage.FindStringSubmatch(href); plat != nil {
			href = "social-" + strings.ToLower(plat[1]) + ".html"
		}
		return fmt.Sprintf(`<a href="%s">%s</a>`, href, text)
	})
	s = reBold.ReplaceAllString(s, `<strong>$1</strong>`)
	return s
}

func renderMarkdown(md string) string {
	lines := strings.Split(md, "\n")
	var out strings.Builder
	inQuote := false
	inList := false
	closeQuote := func() {
		if inQuote {
			out.WriteString("</blockquote>\n")
			inQuote = false
		}
	}
	closeList := func() {
		if inList {
			out.WriteString("</ul>\n")
			inList = false
		}
	}
	for _, line := range lines {
		trimmed := strings.TrimRight(line, " ")
		if trimmed == "<!-- graft:entries -->" {
			continue
		}
		if trimmed == "" {
			closeQuote()
			closeList()
			continue
		}
		if trimmed == "---" {
			closeQuote()
			closeList()
			out.WriteString("<hr>\n")
			continue
		}
		if m := reHeading.FindStringSubmatch(trimmed); m != nil {
			closeQuote()
			closeList()
			level := len(m[1])
			fmt.Fprintf(&out, "<h%d>%s</h%d>\n", level, inline(m[2]), level)
			continue
		}
		if strings.HasPrefix(trimmed, "> ") {
			closeList()
			if !inQuote {
				out.WriteString("<blockquote>\n")
				inQuote = true
			}
			fmt.Fprintf(&out, "<p>%s</p>\n", inline(strings.TrimPrefix(trimmed, "> ")))
			continue
		}
		if m := reBullet.FindStringSubmatch(trimmed); m != nil {
			closeQuote()
			if !inList {
				out.WriteString("<ul>\n")
				inList = true
			}
			fmt.Fprintf(&out, "<li>%s</li>\n", inline(m[1]))
			continue
		}
		closeQuote()
		closeList()
		fmt.Fprintf(&out, "<p>%s</p>\n", inline(trimmed))
	}
	closeQuote()
	closeList()
	return out.String()
}

const pageTemplate = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>%s</title>
<style>
  :root { color-scheme: light; }
  * { box-sizing: border-box; }
  body {
    margin: 0; background: #ffffff; color: #1c1c1c;
    font-family: -apple-system, "Segoe UI", Helvetica, Arial, sans-serif;
    line-height: 1.6;
  }
  .wrap { max-width: 760px; margin: 0 auto; padding: 3rem 1.5rem 5rem; }
  nav.crumbs { font-size: .85rem; color: #6b6b6b; margin-bottom: 2rem; }
  nav.crumbs a { color: #2a5db0; text-decoration: none; }
  nav.crumbs a:hover { text-decoration: underline; }
  h1 { font-size: 1.9rem; font-weight: 700; margin: 0 0 .5rem; letter-spacing: -.01em; }
  h2 { font-size: 1.3rem; font-weight: 600; margin: 2.2rem 0 .8rem; border-bottom: 1px solid #eee; padding-bottom: .4rem; }
  h3 { font-size: 1.05rem; font-weight: 600; margin: 1.8rem 0 .4rem; color: #333; }
  p { margin: 0 0 .9rem; }
  a { color: #2a5db0; }
  hr { border: none; border-top: 1px solid #ececec; margin: 1.6rem 0; }
  blockquote {
    margin: .6rem 0 1.2rem; padding: .1rem 1.1rem; border-left: 3px solid #d8d8d8;
    color: #333; background: #fafafa; border-radius: 0 4px 4px 0;
  }
  blockquote p { margin: .5rem 0; }
  ul { margin: .4rem 0 1rem; padding-left: 1.3rem; }
  li { margin: .25rem 0; }
  strong { font-weight: 600; }
  footer { margin-top: 3rem; padding-top: 1rem; border-top: 1px solid #eee; font-size: .78rem; color: #999; }
</style>
</head>
<body>
<div class="wrap">
<nav class="crumbs">%s</nav>
%s
<footer>Rendered automatically from the Forgejo wiki by graft's wikirender job — edits here are not preserved; edit the wiki directly.</footer>
</div>
</body>
</html>
`

func writeHTML(outPath, title, crumbs, bodyHTML string) error {
	if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
		return err
	}
	content := fmt.Sprintf(pageTemplate, html.EscapeString(title), crumbs, bodyHTML)
	return os.WriteFile(outPath, []byte(content), 0o644)
}

type repoSpec struct {
	owner, repo, slug string
}

func main() {
	token := read("/etc/graft/f1.token")
	base := "https://f1.cyberwild.org"
	outRoot := "/var/www/graft-presentation/wiki"
	client := &http.Client{}

	repos := []repoSpec{
		{"forgeadmin", "constitution", "constitution"},
		{"forgeadmin", "graft", "graft-source"},
		{"forgeadmin", "federation-x", "federation-x"},
		{"forgeadmin", "graft-test-v2", "graft-test-v2"},
		{"forgeadmin", "graft-presentation", "graft-presentation"},
	}

	var indexLinks []string

	for _, r := range repos {
		pages, err := listPages(client, token, base, r.owner, r.repo)
		if err != nil {
			fmt.Println(r.slug, "list error:", err)
			continue
		}
		if len(pages) == 0 {
			continue
		}

		var repoLinks []string
		var homeOutPath, homeTitle, homeBody string
		for _, p := range pages {
			if strings.HasPrefix(p.Title, "_") {
				continue // _Sidebar, _Footer: navigation, not content pages
			}
			md, err := getPageContent(client, token, base, r.owner, r.repo, p.SubURL)
			if err != nil {
				fmt.Println(r.slug, p.Title, "fetch error:", err)
				continue
			}
			bodyHTML := renderMarkdown(md)

			fileName := "index.html"
			if p.Title != "Home" {
				fileName = strings.ToLower(strings.ReplaceAll(p.Title, " ", "-")) + ".html"
			}
			outPath := filepath.Join(outRoot, r.slug, fileName)
			crumbs := fmt.Sprintf(`<a href="/wiki/">wiki</a> &rsaquo; <a href="/wiki/%s/">%s</a> &rsaquo; %s`, r.slug, r.slug, html.EscapeString(p.Title))
			if p.Title == "Home" {
				// Deferred: the sub-page list (repoLinks) isn't complete
				// until every page in this repo has been processed, so the
				// Home page itself is written last, after this loop.
				homeOutPath, homeTitle, homeBody = outPath, r.slug+" — "+p.Title, bodyHTML
				continue
			}
			if err := writeHTML(outPath, r.slug+" — "+p.Title, crumbs, bodyHTML); err != nil {
				fmt.Println(r.slug, p.Title, "write error:", err)
				continue
			}
			repoLinks = append(repoLinks, fmt.Sprintf(`<li><a href="%s">%s</a></li>`, fileName, html.EscapeString(p.Title)))
			fmt.Println("wrote", outPath)
		}
		if homeOutPath != "" {
			if len(repoLinks) > 0 {
				homeBody += "\n<h2>Pages</h2>\n<ul>\n" + strings.Join(repoLinks, "\n") + "\n</ul>\n"
			}
			crumbs := fmt.Sprintf(`<a href="/wiki/">wiki</a> &rsaquo; %s`, r.slug)
			if err := writeHTML(homeOutPath, homeTitle, crumbs, homeBody); err != nil {
				fmt.Println(r.slug, "Home", "write error:", err)
			} else {
				fmt.Println("wrote", homeOutPath)
			}
		}
		indexLinks = append(indexLinks, fmt.Sprintf(`<li><a href="%s/">%s</a></li>`, r.slug, r.slug))
	}

	// top-level index of every repo that has a rendered wiki
	topBody := "<h1>graft — wiki mirror</h1>\n<p>Static, styled snapshot of every repo's Forgejo wiki, refreshed periodically.</p>\n<ul>\n" + strings.Join(indexLinks, "\n") + "\n</ul>\n"
	if err := writeHTML(filepath.Join(outRoot, "index.html"), "graft — wiki mirror", "wiki", topBody); err != nil {
		fmt.Println("top index write error:", err)
	}
}
