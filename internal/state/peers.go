package state

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"gopkg.in/yaml.v3"
)

// Peers added through the dashboard's onboarding forms live in the
// dynamic_repo table, so before this file they existed nowhere else: a
// lost or restored state.db silently dropped every one of them, and
// config.yaml gave no hint they had ever been configured. The peers file
// is their durable, human-readable mirror, kept next to config.yaml:
// ExportDynamicRepos rewrites it from the approved rows, and
// ImportDynamicRepos puts back any peer it lists that the database no
// longer has. config.yaml itself is never rewritten — it is edited by
// hand, and re-encoding it would drop its comments.

type peersDoc struct {
	Peers []peerEntry `yaml:"peers"`
}

type peerEntry struct {
	Name    string `yaml:"name"`
	Series  string `yaml:"series"`
	Forgejo struct {
		BaseURL   string `yaml:"base_url"`
		Owner     string `yaml:"owner"`
		Repo      string `yaml:"repo"`
		TokenFile string `yaml:"token_file"`
	} `yaml:"forgejo"`
	Radicle struct {
		RID         string `yaml:"rid"`
		HTTPBaseURL string `yaml:"http_base_url"`
		ExplorerURL string `yaml:"explorer_url,omitempty"`
		RadHome     string `yaml:"rad_home"`
	} `yaml:"radicle"`
	Sync struct {
		Git     bool `yaml:"git"`
		Issues  bool `yaml:"issues"`
		Patches bool `yaml:"patches"`
	} `yaml:"sync"`
}

const peersFileHeader = "# Maintained by graft — peers added through the dashboard's onboarding\n" +
	"# forms (dynamic_repo in state.db). Rewritten automatically; a peer listed\n" +
	"# here but missing from state.db is re-imported at startup.\n"

// ExportDynamicRepos writes every approved dynamic_repo row to path,
// sorted by name. The file is only replaced when its content actually
// changes (written to a temp file, then renamed, mode 600), and changed
// reports whether that happened.
func (s *Store) ExportDynamicRepos(path string) (changed bool, err error) {
	rows, err := s.AllDynamicRepos()
	if err != nil {
		return false, err
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Name < rows[j].Name })
	doc := peersDoc{Peers: make([]peerEntry, 0, len(rows))}
	for _, r := range rows {
		var e peerEntry
		e.Name, e.Series = r.Name, r.Series
		e.Forgejo.BaseURL, e.Forgejo.Owner, e.Forgejo.Repo, e.Forgejo.TokenFile = r.ForgejoBaseURL, r.ForgejoOwner, r.ForgejoRepo, r.ForgejoTokenFile
		e.Radicle.RID, e.Radicle.HTTPBaseURL, e.Radicle.ExplorerURL, e.Radicle.RadHome = r.RadicleRID, r.RadicleHTTPBaseURL, r.RadicleExplorerURL, r.RadicleRadHome
		e.Sync.Git, e.Sync.Issues, e.Sync.Patches = r.SyncGit, r.SyncIssues, r.SyncPatches
		doc.Peers = append(doc.Peers, e)
	}
	body, err := yaml.Marshal(doc)
	if err != nil {
		return false, err
	}
	content := append([]byte(peersFileHeader), body...)
	if existing, err := os.ReadFile(path); err == nil && bytes.Equal(existing, content) {
		return false, nil
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".peers-*.yaml")
	if err != nil {
		return false, err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		return false, err
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return false, err
	}
	if err := tmp.Close(); err != nil {
		return false, err
	}
	return true, os.Rename(tmp.Name(), path)
}

// ImportDynamicRepos re-creates, as approved rows, every peer listed in
// path that dynamic_repo doesn't already have (matched by name). Peers
// already in the database are left untouched — the database stays the
// source of truth while it has the row. A missing file is not an error.
// Returns the names that were restored.
func (s *Store) ImportDynamicRepos(path string) ([]string, error) {
	doc, err := readPeersDoc(path)
	if err != nil || doc == nil {
		return nil, err
	}
	existing, err := s.db.Query(`SELECT name FROM dynamic_repo`)
	if err != nil {
		return nil, err
	}
	have := map[string]bool{}
	for existing.Next() {
		var n string
		if err := existing.Scan(&n); err != nil {
			existing.Close()
			return nil, err
		}
		have[n] = true
	}
	existing.Close()

	var restored []string
	for _, e := range doc.Peers {
		if e.Name == "" || have[e.Name] {
			continue
		}
		_, err := s.db.Exec(`
INSERT INTO dynamic_repo (
	name, series, forgejo_base_url, forgejo_owner, forgejo_repo, forgejo_token_file,
	radicle_rid, radicle_http_base_url, radicle_explorer_url, radicle_rad_home,
	sync_git, sync_issues, sync_patches, created_at, materialized, approved
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, 1)
`, e.Name, e.Series, e.Forgejo.BaseURL, e.Forgejo.Owner, e.Forgejo.Repo, e.Forgejo.TokenFile,
			e.Radicle.RID, e.Radicle.HTTPBaseURL, e.Radicle.ExplorerURL, e.Radicle.RadHome,
			boolToInt(e.Sync.Git), boolToInt(e.Sync.Issues), boolToInt(e.Sync.Patches), time.Now().UTC().Format(time.RFC3339))
		if err != nil {
			return restored, fmt.Errorf("restore peer %q: %w", e.Name, err)
		}
		restored = append(restored, e.Name)
	}
	return restored, nil
}

// readPeersDoc parses a peers file; a missing file is (nil, nil).
func readPeersDoc(path string) (*peersDoc, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var doc peersDoc
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return &doc, nil
}

// ReadPeersFile returns the peers listed in path without touching any
// database — for tools such as cmd/wikirender that only need to know
// which repos exist. A missing file yields no peers and no error.
func ReadPeersFile(path string) ([]DynamicRepo, error) {
	doc, err := readPeersDoc(path)
	if err != nil || doc == nil {
		return nil, err
	}
	out := make([]DynamicRepo, 0, len(doc.Peers))
	for _, e := range doc.Peers {
		out = append(out, DynamicRepo{
			Name: e.Name, Series: e.Series,
			ForgejoBaseURL: e.Forgejo.BaseURL, ForgejoOwner: e.Forgejo.Owner, ForgejoRepo: e.Forgejo.Repo, ForgejoTokenFile: e.Forgejo.TokenFile,
			RadicleRID: e.Radicle.RID, RadicleHTTPBaseURL: e.Radicle.HTTPBaseURL, RadicleExplorerURL: e.Radicle.ExplorerURL, RadicleRadHome: e.Radicle.RadHome,
			SyncGit: e.Sync.Git, SyncIssues: e.Sync.Issues, SyncPatches: e.Sync.Patches,
			Approved: true,
		})
	}
	return out, nil
}
