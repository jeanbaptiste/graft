// Package atproto is a minimal AT Protocol client: just enough to log in
// with an app password and publish a short post — the two calls graft's
// Bluesky bridge needs, not a general-purpose AT Proto SDK.
package atproto

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"
)

const defaultPDSHost = "https://bsky.social"

// maxPostRunes is Bluesky's post length limit (in grapheme-ish terms; this
// package truncates on runes, which is close enough not to fail the API
// for graft's short auto-generated notes).
const maxPostRunes = 300

// maxResponseBody caps how much of a PDS response this client reads into
// memory; maxLoggedBody further caps how much of an error body ends up
// embedded in an error message (and from there, in logs) — untrusted
// upstream content shouldn't get an unbounded write into our own logging.
const (
	maxResponseBody = 1 << 20 // 1 MiB
	maxLoggedBody   = 500
)

func truncate(b []byte) string {
	if len(b) <= maxLoggedBody {
		return string(b)
	}
	return string(b[:maxLoggedBody]) + "... (truncated)"
}

// Client is a session against one AT Proto PDS, authenticated as one
// account.
type Client struct {
	pdsHost   string
	http      *http.Client
	did       string
	accessJWT string
}

// New creates a client for pdsHost (empty defaults to https://bsky.social).
// Call Login before Post.
func New(pdsHost string) *Client {
	if pdsHost == "" {
		pdsHost = defaultPDSHost
	}
	return &Client{
		pdsHost: strings.TrimRight(pdsHost, "/"),
		http:    &http.Client{Timeout: 15 * time.Second},
	}
}

// Login authenticates with an app password (never the account's real
// password) via com.atproto.server.createSession.
func (c *Client) Login(handle, appPassword string) error {
	body, err := json.Marshal(map[string]string{"identifier": handle, "password": appPassword})
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, c.pdsHost+"/xrpc/com.atproto.server.createSession", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("create session: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
		return fmt.Errorf("create session: HTTP %d: %s", resp.StatusCode, truncate(b))
	}

	var out struct {
		DID       string `json:"did"`
		AccessJwt string `json:"accessJwt"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBody)).Decode(&out); err != nil {
		return fmt.Errorf("decode session: %w", err)
	}
	c.did = out.DID
	c.accessJWT = out.AccessJwt
	return nil
}

// Post publishes a short text-only post via com.atproto.repo.createRecord
// and returns its AT-URI (at://did/app.bsky.feed.post/rkey) — needed to
// later recognize a reply whose parent points back at it. text is
// truncated to maxPostRunes if needed. Login must have succeeded first.
func (c *Client) Post(text string) (uri string, err error) {
	if c.accessJWT == "" {
		return "", fmt.Errorf("not logged in")
	}
	if utf8.RuneCountInString(text) > maxPostRunes {
		r := []rune(text)
		text = string(r[:maxPostRunes-1]) + "…"
	}

	payload := map[string]any{
		"repo":       c.did,
		"collection": "app.bsky.feed.post",
		"record": map[string]any{
			"$type":     "app.bsky.feed.post",
			"text":      text,
			"createdAt": time.Now().UTC().Format(time.RFC3339),
		},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequest(http.MethodPost, c.pdsHost+"/xrpc/com.atproto.repo.createRecord", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.accessJWT)

	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("create record: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
		return "", fmt.Errorf("create record: HTTP %d: %s", resp.StatusCode, truncate(b))
	}
	var out struct {
		URI string `json:"uri"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBody)).Decode(&out); err != nil {
		return "", fmt.Errorf("decode create record response: %w", err)
	}
	return out.URI, nil
}

// Notification is the subset of app.bsky.notification.listNotifications'
// response this client reads. NOT verified against a live PDS — no
// Bluesky account was available to test against when this was written;
// built directly from AT Proto's published lexicon
// (app.bsky.notification.listNotifications / app.bsky.feed.post), which
// is a stable, documented public API, but that is not the same as having
// exercised it. Verify against a real reply before trusting this deeply.
type Notification struct {
	URI    string `json:"uri"`
	Reason string `json:"reason"` // "reply", "like", "repost", "mention", "quote", "follow"
	Author struct {
		Handle string `json:"handle"`
	} `json:"author"`
	IndexedAt string `json:"indexedAt"`
	Record    struct {
		Text  string `json:"text"`
		Reply *struct {
			Parent struct {
				URI string `json:"uri"`
			} `json:"parent"`
		} `json:"reply"`
	} `json:"record"`
}

// DID is the logged-in account's DID ("" before Login).
func (c *Client) DID() string { return c.did }

// PostText returns the text of the app.bsky.feed.post record at atURI,
// read from this client's PDS (records are public, no auth needed).
func (c *Client) PostText(atURI string) (string, error) {
	rest := strings.TrimPrefix(atURI, "at://")
	parts := strings.Split(rest, "/")
	if len(parts) != 3 || parts[1] != "app.bsky.feed.post" {
		return "", fmt.Errorf("not a post URI: %q", atURI)
	}
	q := url.Values{"repo": {parts[0]}, "collection": {parts[1]}, "rkey": {parts[2]}}
	resp, err := c.http.Get(c.pdsHost + "/xrpc/com.atproto.repo.getRecord?" + q.Encode())
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("getRecord %s: HTTP %d", atURI, resp.StatusCode)
	}
	var out struct {
		Value struct {
			Text string `json:"text"`
		} `json:"value"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	return out.Value.Text, nil
}

// ListNotifications fetches one page of notifications, newest first.
// Login must have succeeded first.
func (c *Client) ListNotifications() ([]Notification, error) {
	if c.accessJWT == "" {
		return nil, fmt.Errorf("not logged in")
	}
	req, err := http.NewRequest(http.MethodGet, c.pdsHost+"/xrpc/app.bsky.notification.listNotifications", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.accessJWT)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("list notifications: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
		return nil, fmt.Errorf("list notifications: HTTP %d: %s", resp.StatusCode, truncate(b))
	}
	var out struct {
		Notifications []Notification `json:"notifications"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBody)).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode notifications: %w", err)
	}
	return out.Notifications, nil
}
