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
	"strings"
	"time"
	"unicode/utf8"
)

const defaultPDSHost = "https://bsky.social"

// maxPostRunes is Bluesky's post length limit (in grapheme-ish terms; this
// package truncates on runes, which is close enough not to fail the API
// for graft's short auto-generated notes).
const maxPostRunes = 300

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
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("create session: HTTP %d: %s", resp.StatusCode, b)
	}

	var out struct {
		DID       string `json:"did"`
		AccessJwt string `json:"accessJwt"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return fmt.Errorf("decode session: %w", err)
	}
	c.did = out.DID
	c.accessJWT = out.AccessJwt
	return nil
}

// Post publishes a short text-only post via com.atproto.repo.createRecord.
// text is truncated to maxPostRunes if needed. Login must have succeeded
// first.
func (c *Client) Post(text string) error {
	if c.accessJWT == "" {
		return fmt.Errorf("not logged in")
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
		return err
	}
	req, err := http.NewRequest(http.MethodPost, c.pdsHost+"/xrpc/com.atproto.repo.createRecord", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.accessJWT)

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("create record: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("create record: HTTP %d: %s", resp.StatusCode, b)
	}
	return nil
}
