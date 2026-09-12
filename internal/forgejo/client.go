// Package forgejo is a minimal client for the subset of the Forgejo REST
// API the sync daemon needs: reading/creating issues and pull requests,
// and resolving a repository's clone URL and default branch.
package forgejo

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Client talks to one Forgejo instance on behalf of one repository.
type Client struct {
	baseURL string
	owner   string
	repo    string
	token   string
	http    *http.Client
}

func New(baseURL, owner, repo, token string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		owner:   owner,
		repo:    repo,
		token:   token,
		http:    &http.Client{Timeout: 30 * time.Second},
	}
}

func (c *Client) do(method, path string, body any, out any) error {
	var reqBody io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode request: %w", err)
		}
		reqBody = bytes.NewReader(b)
	}

	url := c.baseURL + "/api/v1" + path
	req, err := http.NewRequest(method, url, reqBody)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "token "+c.token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("%s %s: HTTP %d: %s", method, path, resp.StatusCode, string(respBody))
	}

	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return fmt.Errorf("decode response for %s %s: %w", method, path, err)
		}
	}
	return nil
}

// Repository is the subset of Forgejo's repository object the daemon uses.
type Repository struct {
	CloneURL      string `json:"clone_url"`
	DefaultBranch string `json:"default_branch"`
}

func (c *Client) GetRepository() (*Repository, error) {
	var repo Repository
	path := fmt.Sprintf("/repos/%s/%s", c.owner, c.repo)
	if err := c.do(http.MethodGet, path, nil, &repo); err != nil {
		return nil, err
	}
	return &repo, nil
}

// Issue is the subset of Forgejo's issue object the daemon reads and writes.
type Issue struct {
	Index int64  `json:"number"`
	Title string `json:"title"`
	Body  string `json:"body"`
	State string `json:"state"`
}

func (c *Client) ListIssues() ([]Issue, error) {
	var issues []Issue
	path := fmt.Sprintf("/repos/%s/%s/issues?type=issues&state=all&limit=50", c.owner, c.repo)
	if err := c.do(http.MethodGet, path, nil, &issues); err != nil {
		return nil, err
	}
	return issues, nil
}

func (c *Client) CreateIssue(title, body string) (*Issue, error) {
	var issue Issue
	path := fmt.Sprintf("/repos/%s/%s/issues", c.owner, c.repo)
	req := map[string]string{"title": title, "body": body}
	if err := c.do(http.MethodPost, path, req, &issue); err != nil {
		return nil, err
	}
	return &issue, nil
}

// PullRequest is the subset of Forgejo's pull request object the daemon
// reads and writes.
type PullRequest struct {
	Index int64  `json:"number"`
	Title string `json:"title"`
	Body  string `json:"body"`
	State string `json:"state"`
	Head  Branch `json:"head"`
	Base  Branch `json:"base"`
}

type Branch struct {
	Ref string `json:"ref"`
}

func (c *Client) ListPullRequests() ([]PullRequest, error) {
	var prs []PullRequest
	path := fmt.Sprintf("/repos/%s/%s/pulls?state=all&limit=50", c.owner, c.repo)
	if err := c.do(http.MethodGet, path, nil, &prs); err != nil {
		return nil, err
	}
	return prs, nil
}

func (c *Client) CreatePullRequest(title, body, head, base string) (*PullRequest, error) {
	var pr PullRequest
	path := fmt.Sprintf("/repos/%s/%s/pulls", c.owner, c.repo)
	req := map[string]string{"title": title, "body": body, "head": head, "base": base}
	if err := c.do(http.MethodPost, path, req, &pr); err != nil {
		return nil, err
	}
	return &pr, nil
}

// CreateIssueComment adds a comment to an issue or pull request — Forgejo
// (like Gitea) treats PR comments as issue comments under the same
// endpoint, verified against a live PR on f1.
func (c *Client) CreateIssueComment(index int64, body string) error {
	path := fmt.Sprintf("/repos/%s/%s/issues/%d/comments", c.owner, c.repo, index)
	req := map[string]string{"body": body}
	return c.do(http.MethodPost, path, req, nil)
}
