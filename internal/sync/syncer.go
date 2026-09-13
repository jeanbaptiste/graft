package sync

import (
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"path/filepath"
	"strings"

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
	gitFF   *GitSyncerFF // set instead of git when pair.ForgejoMirror is used
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

	rs := &RepoSyncer{pair: pair, forgejo: fc}

	forgejoWebURL := strings.TrimRight(pair.Forgejo.BaseURL, "/") + "/" + pair.Forgejo.Owner + "/" + pair.Forgejo.Repo

	series := pair.Series
	if series == "" {
		series = pair.Name
	}

	var bluesky *BlueskyPoster
	if pair.Bluesky != nil {
		appPassword, err := config.ReadToken(pair.Bluesky.AppPasswordFile)
		if err != nil {
			return nil, fmt.Errorf("read bluesky app password: %w", err)
		}
		bluesky = &BlueskyPoster{
			Handle:      pair.Bluesky.Handle,
			AppPassword: appPassword,
			PDSHost:     pair.Bluesky.PDSHost,
		}
	}

	if pair.ForgejoMirror != nil {
		mirrorToken, err := config.ReadToken(pair.ForgejoMirror.TokenFile)
		if err != nil {
			return nil, err
		}
		mfc := forgejo.New(pair.ForgejoMirror.BaseURL, pair.ForgejoMirror.Owner, pair.ForgejoMirror.Repo, mirrorToken)
		mrepo, err := mfc.GetRepository()
		if err != nil {
			return nil, fmt.Errorf("resolve forgejo_mirror repository: %w", err)
		}
		mirrorWebURL := strings.TrimRight(pair.ForgejoMirror.BaseURL, "/") + "/" + pair.ForgejoMirror.Owner + "/" + pair.ForgejoMirror.Repo

		if pair.Sync.Git {
			rs.gitFF = &GitSyncerFF{
				RepoPair:      pair.Name,
				WorkDir:       workDir,
				AURL:          authenticatedCloneURL(repo.CloneURL, token),
				AWebURL:       forgejoWebURL,
				BURL:          authenticatedCloneURL(mrepo.CloneURL, mirrorToken),
				BWebURL:       mirrorWebURL,
				DefaultBranch: repo.DefaultBranch,
				State:         st,
				Series:        series,
				SeriesURL:     forgejoWebURL,
				Bluesky:       bluesky,
			}
		}
		return rs, nil
	}

	rc := radicle.New(pair.Radicle.HTTPBaseURL, pair.Radicle.RID, pair.Radicle.RadHome, "rad")
	rs.radicle = rc

	radicleWebURL := RadicleExplorerLink(*pair.Radicle)
	seriesURL := radicleWebURL
	if seriesURL == "" {
		seriesURL = forgejoWebURL
	}

	if pair.Sync.Git || pair.Sync.Patches {
		rs.git = &GitSyncer{
			RepoPair:      pair.Name,
			WorkDir:       workDir,
			ForgejoURL:    authenticatedCloneURL(repo.CloneURL, token),
			ForgejoWebURL: forgejoWebURL,
			RadicleWebURL: radicleWebURL,
			ForgejoBranch: repo.DefaultBranch,
			RID:           pair.Radicle.RID,
			RadHome:       pair.Radicle.RadHome,
			State:         st,
			Series:        series,
			SeriesURL:     seriesURL,
			Bluesky:       bluesky,
		}
	}
	if pair.Sync.Issues {
		rs.issues = &IssueSyncer{
			RepoPair: pair.Name, Forgejo: fc, Radicle: rc, State: st,
			ForgejoWebURL: forgejoWebURL, RadicleWebURL: radicleWebURL,
			Series: series, SeriesURL: seriesURL,
		}
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
			ForgejoWebURL: forgejoWebURL,
			RadicleWebURL: radicleWebURL,
			Series:        series,
			SeriesURL:     seriesURL,
			Bluesky:       bluesky,
		}
	}
	return rs, nil
}

// RadicleExplorerLink builds the base URL for "view this on Radicle" links:
// <explorer>/nodes/<httpd-host>/<rid>. Empty if no explorer_url is
// configured, so templates can skip the link entirely.
func RadicleExplorerLink(rt config.RadicleTarget) string {
	if rt.ExplorerURL == "" {
		return ""
	}
	host := rt.HTTPBaseURL
	if u, err := url.Parse(rt.HTTPBaseURL); err == nil && u.Host != "" {
		host = u.Host
	}
	return strings.TrimRight(rt.ExplorerURL, "/") + "/nodes/" + host + "/" + rt.RID
}

// Name is this pair's name, as given in the config.
func (rs *RepoSyncer) Name() string { return rs.pair.Name }

// ForgejoClient exposes this pair's Forgejo client for cross-pair
// detection helpers (see HasAuthorizedIntegration) that need to make API
// calls outside the normal sync flow.
func (rs *RepoSyncer) ForgejoClient() *forgejo.Client { return rs.forgejo }

// ForgejoHost is the host this pair's Forgejo repo lives on, e.g.
// "f1.cyberwild.org".
func (rs *RepoSyncer) ForgejoHost() string {
	u, err := url.Parse(rs.pair.Forgejo.BaseURL)
	if err != nil {
		return ""
	}
	return u.Host
}

// SetAuthorizedIntegrationSource declares that another pair's Forgejo host
// already pushes directly into this pair's Forgejo via Authorized
// Integrations (see authint.go and GitSyncer.AuthorizedIntegrationSource).
// Set once, after every pair in a series is known, from cmd/sync/main.go —
// a single pair's own construction can't see its siblings.
func (rs *RepoSyncer) SetAuthorizedIntegrationSource(host string) {
	if rs.git != nil {
		rs.git.AuthorizedIntegrationSource = host
	}
	if rs.gitFF != nil {
		rs.gitFF.AuthorizedIntegrationSource = host
	}
}

// HasAuthorizedIntegrationSource reports whether this pair's Forgejo side
// already receives pushes directly from another pair's Forgejo via
// Authorized Integrations (set by SetAuthorizedIntegrationSource at
// startup) — used by the dashboard to explain why this side shows no
// graft-logged events even though it holds the mirrored content.
func (rs *RepoSyncer) HasAuthorizedIntegrationSource() bool {
	if rs.git != nil && rs.git.AuthorizedIntegrationSource != "" {
		return true
	}
	return rs.gitFF != nil && rs.gitFF.AuthorizedIntegrationSource != ""
}

// Series is the dashboard row this pair's activity groups under: its
// configured series, or its own name if unset.
func (rs *RepoSyncer) Series() string {
	if rs.pair.Series != "" {
		return rs.pair.Series
	}
	return rs.pair.Name
}

// CommentOnItem posts a reply (from the ActivityPub comment bridge, see
// internal/activitypub) onto the underlying Forgejo issue/PR and Radicle
// issue/patch this pair mirrors. For a patch, radicleID is used as the
// revision to comment on — correct for the common single-revision case,
// since a patch's own id equals its first revision's id; a patch that has
// since been updated with further revisions may have the comment attach
// to an older revision instead of the latest one, a known scoped
// limitation rather than a bug to chase down for v2.
func (rs *RepoSyncer) CommentOnItem(kind string, forgejoID int64, radicleID, body string) error {
	var errs []error
	if forgejoID != 0 {
		if err := rs.forgejo.CreateIssueComment(forgejoID, body); err != nil {
			errs = append(errs, fmt.Errorf("forgejo: %w", err))
		}
	}
	if radicleID != "" {
		var err error
		switch kind {
		case "issue":
			err = rs.radicle.CommentIssue(radicleID, body)
		case "patch":
			err = rs.radicle.CommentPatch(radicleID, body)
		default:
			err = fmt.Errorf("cannot comment on kind %q", kind)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("radicle: %w", err))
		}
	}
	return errors.Join(errs...)
}

// RadicleDID returns the local Radicle node identity this pair pushes as,
// resolved the same way GitSyncer resolves it for remote setup (`rad self
// --nid`). Empty if this pair has no git sync configured, so nothing to
// report.
func (rs *RepoSyncer) RadicleDID() (string, error) {
	if rs.git == nil {
		return "", nil
	}
	nid, err := rs.git.localNodeID()
	if err != nil {
		return "", err
	}
	if nid == "" {
		return "", nil
	}
	return "did:key:" + nid, nil
}

// Run executes every enabled scope once, logging and continuing past
// per-scope errors so one broken scope doesn't block the others. It
// returns each scope's error (nil if disabled or successful) for the
// caller to report via internal/status.
func (rs *RepoSyncer) Run(log *slog.Logger) (gitErr, issuesErr, patchErr error) {
	log = log.With("pair", rs.pair.Name)

	if rs.git != nil {
		if gitErr = rs.git.Sync(); gitErr != nil {
			log.Error("git sync failed", "err", gitErr)
		}
	}
	if rs.gitFF != nil {
		if gitErr = rs.gitFF.Sync(); gitErr != nil {
			log.Error("git sync failed", "err", gitErr)
		}
	}
	if rs.issues != nil {
		if issuesErr = rs.issues.Sync(); issuesErr != nil {
			log.Error("issue sync failed", "err", issuesErr)
		}
	}
	if rs.patches != nil {
		if patchErr = rs.patches.Sync(); patchErr != nil {
			log.Error("patch sync failed", "err", patchErr)
		}
	}
	return gitErr, issuesErr, patchErr
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
