package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
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

type page struct {
	Title  string `json:"title"`
	SubURL string `json:"sub_url"`
}

func listPages(token, base, owner, repo string) []page {
	req, _ := http.NewRequest(http.MethodGet, fmt.Sprintf("%s/api/v1/repos/%s/%s/wiki/pages?limit=50", base, owner, repo), nil)
	req.SetBasicAuth(token, "")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode == 404 {
		return nil
	}
	b, _ := io.ReadAll(resp.Body)
	var pages []page
	json.Unmarshal(b, &pages)
	return pages
}

func upsertPage(token, base, owner, repo, title, content string) error {
	// find existing
	pages := listPages(token, base, owner, repo)
	slug := ""
	for _, p := range pages {
		if p.Title == title {
			slug = p.SubURL
		}
	}
	if slug == "" {
		payload, _ := json.Marshal(map[string]string{
			"title": title, "content_base64": base64.StdEncoding.EncodeToString([]byte(content)),
			"message": "graft: create wiki landing page",
		})
		u := fmt.Sprintf("%s/api/v1/repos/%s/%s/wiki/new", base, owner, repo)
		req, _ := http.NewRequest(http.MethodPost, u, bytes.NewReader(payload))
		req.Header.Set("Content-Type", "application/json")
		req.SetBasicAuth(token, "")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode >= 300 {
			b, _ := io.ReadAll(resp.Body)
			return fmt.Errorf("create %s: HTTP %d: %s", title, resp.StatusCode, string(b))
		}
		return nil
	}
	payload, _ := json.Marshal(map[string]string{
		"title": title, "content_base64": base64.StdEncoding.EncodeToString([]byte(content)),
		"message": "graft: update wiki landing page",
	})
	u := fmt.Sprintf("%s/api/v1/repos/%s/%s/wiki/page/%s", base, owner, repo, strings.ReplaceAll(slug, " ", "%20"))
	req, _ := http.NewRequest(http.MethodPatch, u, bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req.SetBasicAuth(token, "")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("update %s: HTTP %d: %s", title, resp.StatusCode, string(b))
	}
	return nil
}

var platformInfo = map[string]struct{ label, blurb string }{
	"Social-Discourse":  {"Discourse", "Forum replies about this repository, relayed via the Discourse bridge."},
	"Social-Zulip":      {"Zulip", "Chat replies about this repository, relayed via the Zulip bridge."},
	"Social-Mastodon":   {"Mastodon / Fediverse", "Native ActivityPub replies from the wider Fediverse."},
	"Social-Tangled":    {"Tangled", "Comments on this repository's mirrored Tangled issues (AT Proto)."},
	"Social-Bluesky":    {"Bluesky", "Replies to this repository's posts on Bluesky (AT Proto)."},
}

var platformOrder = []string{"Social-Discourse", "Social-Zulip", "Social-Mastodon", "Social-Tangled", "Social-Bluesky"}

func main() {
	token := read("/etc/graft/f1.token")
	base := "https://f1.cyberwild.org"
	owner := "forgeadmin"
	repos := []string{"graft-test-v2", "constitution", "graft-source", "federation-x", "graft-presentation", "graft"}

	for _, repo := range repos {
		pages := listPages(token, base, owner, repo)
		if len(pages) == 0 {
			fmt.Println(repo, "- no wiki pages yet, skipping")
			continue
		}
		have := map[string]bool{}
		for _, p := range pages {
			have[p.Title] = true
		}

		present := []string{}
		for _, key := range platformOrder {
			if have[key] {
				present = append(present, key)
			}
		}
		sort.Strings(present)

		var sb strings.Builder
		sb.WriteString("# " + repo + " — social timeline\n\n")
		sb.WriteString("This wiki mirrors discussion **about this repository** from every platform graft bridges to it — one page per platform, each a newest-first timeline. Native Forgejo/Radicle/Tangled issues and tickets stay exactly where they are (the Tickets tab); only the *social* conversation around them lands here.\n\n")
		sb.WriteString("## Platforms\n\n")
		if len(present) == 0 {
			sb.WriteString("_No social platform has posted about this repository yet._\n\n")
		}
		for _, key := range present {
			info := platformInfo[key]
			sb.WriteString(fmt.Sprintf("- **[%s](%s)** — %s\n", info.label, key, info.blurb))
		}
		sb.WriteString("\n---\n\nMaintained automatically by [graft](https://github.com/jeanbaptiste/graft).\n")

		home := sb.String()

		var sidebar strings.Builder
		sidebar.WriteString("**Social timeline**\n\n[Home](Home)\n\n")
		for _, key := range present {
			info := platformInfo[key]
			sidebar.WriteString(fmt.Sprintf("[%s](%s)\n\n", info.label, key))
		}
		side := sidebar.String()

		if err := upsertPage(token, base, owner, repo, "Home", home); err != nil {
			fmt.Println(repo, "Home error:", err)
		} else {
			fmt.Println(repo, "Home OK")
		}
		if err := upsertPage(token, base, owner, repo, "_Sidebar", side); err != nil {
			fmt.Println(repo, "_Sidebar error:", err)
		} else {
			fmt.Println(repo, "_Sidebar OK")
		}
	}
}
