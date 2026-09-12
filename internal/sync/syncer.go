package sync

import (
	"fmt"
	"log/slog"
	"net/url"
	"path/filepath"

	"graft/internal/config"
	"graft/internal/forgejo"
	"graft/internal/radicle"
	"graft/internal/state"
)

// RepoSyncer runs one repo pair's enabled sync scopes for one pass.
type RepoSyncer struct {
	pair    config.RepoPair
	forgejo *forgejo.Client
	radicle *radicle.Client
	git     *GitSyncer
	issues  *IssueSyncer
	patches *PatchSyncer
}

// New builds a RepoSyncer for one configured pair. workDir is where this
// pair's local git mirror is kept, e.g. <state-dir>/<pair-name>.
func New(pair config.RepoPair, st *state.Store, workDir string) (*RepoSyncer, error) {
	token, err := config.ReadToken(pair.Forgejo.TokenFile)
	if err != nil {
		return nil, err
	}
	fc := forgejo.New(pair.Forgejo.BaseURL, pair.Forgejo.Owner, pair.Forgejo.Repo, token)

	repo, err := fc.GetRepository()
	if err != nil {
		return nil, fmt.Errorf("resolve forgejo repository: %w", err)
	}

	rc := radicle.New(pair.Radicle.HTTPBaseURL, pair.Radicle.RID, pair.Radicle.RadHome, "rad")

	rs := &RepoSyncer{pair: pair, forgejo: fc, radicle: rc}

	if pair.Sync.Git || pair.Sync.Patches {
		rs.git = &GitSyncer{
			RepoPair:      pair.Name,
			WorkDir:       workDir,
			ForgejoURL:    authenticatedCloneURL(repo.CloneURL, token),
			ForgejoBranch: repo.DefaultBranch,
			RID:           pair.Radicle.RID,
			RadHome:       pair.Radicle.RadHome,
			State:         st,
		}
	}
	if pair.Sync.Issues {
		rs.issues = &IssueSyncer{RepoPair: pair.Name, Forgejo: fc, Radicle: rc, State: st}
	}
	if pair.Sync.Patches {
		rs.patches = &PatchSyncer{
			RepoPair:      pair.Name,
			WorkDir:       workDir,
			RadHome:       pair.Radicle.RadHome,
			DefaultBranch: repo.DefaultBranch,
			Forgejo:       fc,
			Radicle:       rc,
			State:         st,
		}
	}
	return rs, nil
}

// Run executes every enabled scope once, logging and continuing past
// per-scope errors so one broken scope doesn't block the others.
func (rs *RepoSyncer) Run(log *slog.Logger) {
	log = log.With("pair", rs.pair.Name)

	if rs.git != nil {
		if err := rs.git.Sync(); err != nil {
			log.Error("git sync failed", "err", err)
		}
	}
	if rs.issues != nil {
		if err := rs.issues.Sync(); err != nil {
			log.Error("issue sync failed", "err", err)
		}
	}
	if rs.patches != nil {
		if err := rs.patches.Sync(); err != nil {
			log.Error("patch sync failed", "err", err)
		}
	}
}

// authenticatedCloneURL embeds a token into an HTTPS clone URL the way
// Forgejo expects (token as the basic-auth username).
func authenticatedCloneURL(cloneURL, token string) string {
	u, err := url.Parse(cloneURL)
	if err != nil {
		return cloneURL
	}
	u.User = url.User(token)
	return u.String()
}

// WorkDirFor returns the per-pair git mirror directory under stateDir.
func WorkDirFor(stateDir, pairName string) string {
	return filepath.Join(stateDir, "repos", pairName)
}
