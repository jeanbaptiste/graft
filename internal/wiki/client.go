// Package wiki writes social-platform discussion (Discourse, Zulip,
// Mastodon/ActivityPub, Tangled) into a Forgejo repository's wiki, one page
// per platform, as a newest-first timeline — instead of as issue/PR
// comments. Radicle and Tangled issues/tickets are untouched by this
// package: they still mirror through the normal Forgejo<->Radicle/Tangled
// issue path (internal/sync/issues.go, patches.go). This package only
// carries the *social reply* traffic that used to land as a Forgejo issue
// comment via RepoSyncer.CommentOnItem.
package wiki

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ErrForbidden means the token this Client was built with cannot write to
// this repo's wiki (Forgejo returns 403 with a "missing scope:
// write:repository" message). Callers should treat this as "fall back to
// a different write path", not as a transient failure to retry.
//
// Checked by trying the real write and inspecting the response, not by
// predicting it in advance: a repo's GET .../repos/{owner}/{repo}
// "permissions.push" field reflects the underlying account's collaborator
// permission on the repo, not the presented token's own OAuth-style
// scope restriction — confirmed directly against a live instance, where
// a deliberately read-only-scoped token for an account that does have
// push rights on the repo still reported permissions.push=true, only to
// then 403 on the actual write. Predicting from that field would have
// been silently wrong.
var ErrForbidden = errors.New("wiki: token lacks write:repository scope")

// entriesMarker delimits a page's fixed header from its entries, so a new
// entry can be inserted right after it (newest-first) without disturbing
// the header or any earlier entries.
const entriesMarker = "<!-- graft:entries -->"

// Client talks to one Forgejo repository's wiki via its REST API
// (POST .../wiki/new, GET/PATCH .../wiki/page/{title}).
type Client struct {
	BaseURL string
	Owner   string
	Repo    string
	Token   string
	HTTP    *http.Client
}

func New(baseURL, owner, repo, token string) *Client {
	return &Client{BaseURL: baseURL, Owner: owner, Repo: repo, Token: token, HTTP: &http.Client{Timeout: 20 * time.Second}}
}

// AppendEntry inserts entryMarkdown at the top of pageTitle's timeline
// (right after entriesMarker), creating the page with header first if it
// doesn't exist yet. header is only used on first creation.
//
// Not safe against two concurrent writers racing on the *same* page (a
// read-modify-write, no optimistic locking) — acceptable for now given how
// infrequently a single platform's page gets a new entry, and because each
// platform has its own page so different bridges never contend for the
// same one. A genuine race would just lose one entry's insert, not corrupt
// the page.
func (c *Client) AppendEntry(pageTitle, header, entryMarkdown string) error {
	existing, sha, err := c.getPage(pageTitle)
	if err != nil {
		return fmt.Errorf("get wiki page %q: %w", pageTitle, err)
	}

	entryMarkdown = strings.TrimRight(entryMarkdown, "\n") + "\n\n---\n"

	if existing == "" {
		content := header + "\n\n" + entriesMarker + "\n\n" + entryMarkdown + pageFooter()
		return c.createPage(pageTitle, content)
	}

	idx := strings.Index(existing, entriesMarker)
	if idx < 0 {
		// Marker missing (page edited by hand, or an older format) —
		// append the marker plus the new entry at the end rather than
		// silently dropping it.
		content := strings.TrimRight(existing, "\n") + "\n\n" + entriesMarker + "\n\n" + entryMarkdown + pageFooter()
		return c.updatePage(pageTitle, content, sha)
	}
	insertAt := idx + len(entriesMarker)
	// Remove old footer if present, add new entry and fresh footer
	beforeMarker := existing[:insertAt]
	afterMarker := strings.TrimLeft(existing[insertAt:], "\n")
	afterMarker = removeFooter(afterMarker)
	content := beforeMarker + "\n\n" + entryMarkdown + afterMarker + pageFooter()
	return c.updatePage(pageTitle, content, sha)
}

// ResetPage overwrites pageTitle's content back to just its header plus an
// empty entries marker — an update, not a delete, used to fix a page whose
// entries were written in the wrong order rather than removing it outright.
func (c *Client) ResetPage(pageTitle, header string) error {
	content := header + "\n\n" + entriesMarker + "\n"
	_, sha, err := c.getPage(pageTitle)
	if err != nil {
		return fmt.Errorf("get wiki page %q: %w", pageTitle, err)
	}
	if sha == "" {
		return c.createPage(pageTitle, content)
	}
	return c.updatePage(pageTitle, content, sha)
}

// slugFor resolves a page title to the slug Forgejo actually files it
// under. Forgejo's wiki filename-escaping scheme mangles a title
// containing a hyphen — "Social-Discourse" gets stored (and must be
// addressed) as "Social-Discourse.-", not the plain title — confirmed
// directly against a live instance, not assumed from docs. GET/PATCH by
// the plain title 404s outright. Returns "" if no page with this title
// exists yet.
func (c *Client) slugFor(title string) (string, error) {
	u := fmt.Sprintf("%s/api/v1/repos/%s/%s/wiki/pages?limit=50", strings.TrimRight(c.BaseURL, "/"), c.Owner, c.Repo)
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	req.SetBasicAuth(c.Token, "")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode == http.StatusNotFound {
		// A repo whose wiki has never had a single page saved has no
		// .wiki.git at all yet — confirmed directly against a live
		// instance, where this 404s instead of returning an empty list.
		// No pages exist yet, not an error.
		return "", nil
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("list wiki pages: HTTP %d: %s", resp.StatusCode, truncate(b, 300))
	}
	var pages []struct {
		Title  string `json:"title"`
		SubURL string `json:"sub_url"`
	}
	if err := json.Unmarshal(b, &pages); err != nil {
		return "", fmt.Errorf("decode wiki pages list: %w", err)
	}
	for _, p := range pages {
		if p.Title == title {
			return p.SubURL, nil
		}
	}
	return "", nil
}

// getPage returns a page's decoded content ("" if it doesn't exist yet)
// and its current commit sha (empty if the page doesn't exist).
func (c *Client) getPage(title string) (content, sha string, err error) {
	slug, err := c.slugFor(title)
	if err != nil {
		return "", "", err
	}
	if slug == "" {
		return "", "", nil
	}
	u := fmt.Sprintf("%s/api/v1/repos/%s/%s/wiki/page/%s", strings.TrimRight(c.BaseURL, "/"), c.Owner, c.Repo, urlEscape(slug))
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return "", "", err
	}
	req.SetBasicAuth(c.Token, "")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return "", "", nil
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(b, 300))
	}
	var page struct {
		ContentBase64 string `json:"content_base64"`
		LastCommit    struct {
			SHA string `json:"sha"`
		} `json:"last_commit"`
	}
	if err := json.Unmarshal(b, &page); err != nil {
		return "", "", fmt.Errorf("decode page: %w", err)
	}
	decoded, err := base64.StdEncoding.DecodeString(page.ContentBase64)
	if err != nil {
		return "", "", fmt.Errorf("decode base64: %w", err)
	}
	return string(decoded), page.LastCommit.SHA, nil
}

func (c *Client) createPage(title, content string) error {
	body, _ := json.Marshal(map[string]string{
		"title":          title,
		"content_base64": base64.StdEncoding.EncodeToString([]byte(content)),
		"message":        "graft: create social timeline page",
	})
	u := fmt.Sprintf("%s/api/v1/repos/%s/%s/wiki/new", strings.TrimRight(c.BaseURL, "/"), c.Owner, c.Repo)
	return c.do(http.MethodPost, u, body)
}

func (c *Client) updatePage(title, content, sha string) error {
	slug, err := c.slugFor(title)
	if err != nil {
		return err
	}
	if slug == "" {
		return fmt.Errorf("update wiki page %q: page vanished between read and write", title)
	}
	// title must be resent even though it isn't changing: despite the
	// swagger doc ("leave empty to keep unchanged"), omitting it against
	// a live instance actually renamed the page to "unnamed" — confirmed
	// directly, not assumed.
	body, _ := json.Marshal(map[string]string{
		"title":          title,
		"content_base64": base64.StdEncoding.EncodeToString([]byte(content)),
		"message":        "graft: log social reply",
	})
	u := fmt.Sprintf("%s/api/v1/repos/%s/%s/wiki/page/%s", strings.TrimRight(c.BaseURL, "/"), c.Owner, c.Repo, urlEscape(slug))
	return c.do(http.MethodPatch, u, body)
}

func (c *Client) do(method, u string, body []byte) error {
	req, err := http.NewRequest(method, u, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.SetBasicAuth(c.Token, "")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusForbidden {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return fmt.Errorf("%s %s: %w: %s", method, u, ErrForbidden, truncate(b, 300))
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return fmt.Errorf("%s %s: HTTP %d: %s", method, u, resp.StatusCode, truncate(b, 300))
	}
	return nil
}

func urlEscape(s string) string {
	// Forgejo wiki page titles use spaces (encoded as %20 or +); our own
	// titles are plain ASCII with hyphens, so this is only ever exercised
	// for those — kept simple rather than pulling in net/url just for this.
	return strings.ReplaceAll(s, " ", "%20")
}

func pageFooter() string {
	return "\n\n---\n\n_Page generée par [graft](https://github.com/jeanbaptiste/graft) le " +
		time.Now().UTC().Format("2006-01-02 15:04 UTC") + "_"
}

func removeFooter(content string) string {
	// Remove the footer line if present (starts with _Page generée par graft...)
	idx := strings.LastIndex(content, "\n\n---\n\n_Page generée par")
	if idx < 0 {
		return content
	}
	return strings.TrimRight(content[:idx], "\n")
}

func truncate(b []byte, n int) string {
	s := string(b)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
