// Package radicle talks to one local Radicle node: reads go through
// radicle-httpd's JSON API (schema confirmed against a live v0.28.0 node —
// see the field comments below), writes go through the "rad" CLI since
// radicle-httpd exposes no write endpoints.
package radicle

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"time"
)

// Client operates on one repository (identified by its RID) tracked by one
// local Radicle node.
type Client struct {
	httpdBaseURL string // e.g. https://r1.cyberwild.org, used for reads
	rid          string
	radHome      string // RAD_HOME for the "rad" CLI, used for writes
	radBin       string // path to the rad binary
	http         *http.Client
}

func New(httpdBaseURL, rid, radHome, radBin string) *Client {
	return &Client{
		httpdBaseURL: httpdBaseURL,
		rid:          rid,
		radHome:      radHome,
		radBin:       radBin,
		http:         &http.Client{Timeout: 30 * time.Second},
	}
}

func (c *Client) getJSON(path string, out any) error {
	resp, err := c.http.Get(c.httpdBaseURL + "/api/v1/repos/" + c.rid + path)
	if err != nil {
		return fmt.Errorf("GET %s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("GET %s: HTTP %d", path, resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// rad runs the CLI against this client's repository and RAD_HOME, returning
// combined stdout+stderr for logging on error.
func (c *Client) rad(args ...string) (string, error) {
	args = append(args, "--repo", c.rid)
	cmd := exec.Command(c.radBin, args...)
	cmd.Env = append(os.Environ(), "RAD_HOME="+c.radHome)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		return out.String(), fmt.Errorf("rad %v: %w: %s", args, err, out.String())
	}
	return out.String(), nil
}

// Actor identifies who authored an issue, comment, or patch revision.
type Actor struct {
	ID    string `json:"id"` // did:key:...
	Alias string `json:"alias"`
}

// IssueState mirrors radicle-httpd's issue state object.
type IssueState struct {
	Status string `json:"status"` // "open" or "closed"
}

// Issue is the subset of radicle-httpd's issue JSON the daemon needs.
// Verified against GET /api/v1/repos/:rid/issues?status=all on httpd 0.28.0.
type Issue struct {
	ID         string     `json:"id"`
	Title      string     `json:"title"`
	State      IssueState `json:"state"`
	Author     Actor      `json:"author"`
	Discussion []struct {
		Body string `json:"body"`
	} `json:"discussion"`
}

// Body returns the issue's opening comment, if any.
func (i Issue) Body() string {
	if len(i.Discussion) == 0 {
		return ""
	}
	return i.Discussion[0].Body
}

func (c *Client) ListIssues() ([]Issue, error) {
	var issues []Issue
	if err := c.getJSON("/issues?status=all", &issues); err != nil {
		return nil, err
	}
	return issues, nil
}

// issueIDRe matches the "Issue   <id>" line in rad's non-quiet issue-open
// output box. --quiet was tried first and rejected: it suppresses the id
// entirely (verified against a live node), printing only the sync summary,
// which silently corrupted the stored mapping and caused every issue to be
// re-mirrored on each pass.
var issueIDRe = regexp.MustCompile(`Issue\s+([0-9a-f]{40})`)

// OpenIssue creates a new issue via the CLI (no write API exists on httpd).
func (c *Client) OpenIssue(title, description string) (string, error) {
	out, err := c.rad("issue", "open", "-t", title, "-d", description)
	if err != nil {
		return "", err
	}
	m := issueIDRe.FindStringSubmatch(out)
	if m == nil {
		return "", fmt.Errorf("rad issue open: could not parse issue id from output: %q", out)
	}
	return m[1], nil
}

// PatchState mirrors radicle-httpd's patch state object.
type PatchState struct {
	Status string `json:"status"` // "open", "draft", "archived", "merged"
}

// Patch is the subset of radicle-httpd's patch JSON the daemon needs.
// Verified against GET /api/v1/repos/:rid/patches?status=all on httpd 0.28.0.
type Patch struct {
	ID        string     `json:"id"`
	Title     string     `json:"title"`
	State     PatchState `json:"state"`
	Author    Actor      `json:"author"`
	Revisions []struct {
		Description string `json:"description"`
		OID         string `json:"oid"`  // head commit of the revision
		Base        string `json:"base"` // commit the patch is based on
	} `json:"revisions"`
}

func (c *Client) ListPatches() ([]Patch, error) {
	var patches []Patch
	if err := c.getJSON("/patches?status=all", &patches); err != nil {
		return nil, err
	}
	return patches, nil
}
