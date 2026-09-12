// Package config loads and validates the sync daemon's configuration.
package config

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the top-level configuration for the sync daemon.
type Config struct {
	SyncInterval time.Duration `yaml:"sync_interval"`
	StateDB      string        `yaml:"state_db"`
	Repos        []RepoPair    `yaml:"repos"`
	// SourceURL is graft's own source repository, shown in the status
	// dashboard's footer. Optional.
	SourceURL string `yaml:"source_url"`
	// PublicHost is the hostname graft is reachable at (e.g.
	// graft.cyberwild.org), used to build ActivityPub actor/acct URIs.
	// Required only if any series should be reachable over ActivityPub.
	PublicHost string `yaml:"public_host"`
}

// RepoPair links one Forgejo repository to one Radicle repository and
// declares which content types are kept in sync between them.
type RepoPair struct {
	Name    string        `yaml:"name"`
	Forgejo ForgejoTarget `yaml:"forgejo"`
	Radicle RadicleTarget `yaml:"radicle"`
	Sync    SyncScope     `yaml:"sync"`
	// Series groups this pair with others that mirror the same underlying
	// repository (one per forge/Radicle side) under one row in the status
	// dashboard. Defaults to Name — i.e. its own row — if unset.
	Series string `yaml:"series"`
	// Bluesky, if set, posts a short note to this account whenever this
	// pair mirrors a new commit or opens a patch/PR. Optional: nil means
	// this pair never posts to Bluesky.
	Bluesky *BlueskyTarget `yaml:"bluesky"`
}

// BlueskyTarget identifies the Bluesky account to post mirror activity to.
// AppPasswordFile follows the same convention as ForgejoTarget.TokenFile —
// an app password (never the account's real password), read from a
// separate chmod-600 file so the config itself holds no secrets.
type BlueskyTarget struct {
	Handle          string `yaml:"handle"`
	AppPasswordFile string `yaml:"app_password_file"`
	// PDSHost is the XRPC endpoint to talk to. Optional: defaults to
	// https://bsky.social, correct for any account hosted there (the
	// common case).
	PDSHost string `yaml:"pds_host"`
}

// ForgejoTarget identifies a repository on a Forgejo instance and where to
// read its access token from. The token is never stored inline in the
// config file so the file itself can be handled as non-sensitive.
type ForgejoTarget struct {
	BaseURL   string `yaml:"base_url"`
	Owner     string `yaml:"owner"`
	Repo      string `yaml:"repo"`
	TokenFile string `yaml:"token_file"`
}

// RadicleTarget identifies a repository on a Radicle node. HTTPBaseURL is
// that node's radicle-httpd endpoint (e.g. https://r1.cyberwild.org), used
// for reads; RadHome is the local RAD_HOME whose keys the "rad" CLI uses
// for writes — the two are only the same node when the daemon runs
// alongside it, which is not assumed here.
type RadicleTarget struct {
	RID         string `yaml:"rid"`
	HTTPBaseURL string `yaml:"http_base_url"`
	RadHome     string `yaml:"rad_home"`
	// ExplorerURL is a Radicle Explorer deployment (e.g.
	// https://app.radicle.xyz, or a self-hosted one) used to build "view
	// this issue/patch" links in the status dashboard. Optional: links are
	// omitted if unset.
	ExplorerURL string `yaml:"explorer_url"`
}

// SyncScope selects which content types are synchronized for a repo pair.
type SyncScope struct {
	Git     bool `yaml:"git"`
	Issues  bool `yaml:"issues"`
	Patches bool `yaml:"patches"`
}

// Load reads and validates the configuration file at path.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	if cfg.SyncInterval <= 0 {
		cfg.SyncInterval = 5 * time.Minute
	}
	if cfg.StateDB == "" {
		return nil, fmt.Errorf("state_db must be set")
	}
	if len(cfg.Repos) == 0 {
		return nil, fmt.Errorf("no repos configured")
	}
	for i, r := range cfg.Repos {
		if err := r.validate(); err != nil {
			return nil, fmt.Errorf("repos[%d] (%s): %w", i, r.Name, err)
		}
	}

	return &cfg, nil
}

func (r RepoPair) validate() error {
	if r.Name == "" {
		return fmt.Errorf("name is required")
	}
	if r.Forgejo.BaseURL == "" || r.Forgejo.Owner == "" || r.Forgejo.Repo == "" {
		return fmt.Errorf("forgejo.base_url, owner and repo are required")
	}
	if r.Forgejo.TokenFile == "" {
		return fmt.Errorf("forgejo.token_file is required")
	}
	if r.Radicle.RID == "" {
		return fmt.Errorf("radicle.rid is required")
	}
	if r.Radicle.HTTPBaseURL == "" {
		return fmt.Errorf("radicle.http_base_url is required")
	}
	if r.Radicle.RadHome == "" {
		return fmt.Errorf("radicle.rad_home is required")
	}
	if !r.Sync.Git && !r.Sync.Issues && !r.Sync.Patches {
		return fmt.Errorf("at least one of sync.git, sync.issues, sync.patches must be true")
	}
	if r.Bluesky != nil {
		if r.Bluesky.Handle == "" || r.Bluesky.AppPasswordFile == "" {
			return fmt.Errorf("bluesky.handle and app_password_file are required when bluesky is set")
		}
	}
	return nil
}

// ReadToken reads a token from a file, trimming surrounding whitespace.
// Token files are expected to be chmod 600 and owned by the service user.
func ReadToken(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read token file %s: %w", path, err)
	}
	token := string(data)
	for len(token) > 0 && (token[len(token)-1] == '\n' || token[len(token)-1] == '\r' || token[len(token)-1] == ' ') {
		token = token[:len(token)-1]
	}
	if token == "" {
		return "", fmt.Errorf("token file %s is empty", path)
	}
	return token, nil
}
