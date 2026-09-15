package sync

import (
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"graft/internal/config"
	"graft/internal/forgejo"
	"graft/internal/radicle"
	"graft/internal/state"
	"graft/internal/wiki"
)

// RepoSyncer runs one repo pair's enabled sync scopes for one pass.
type RepoSyncer struct {
	pair     config.RepoPair
	forgejo  *forgejo.Client
	radicle  *radicle.Client
	git      *GitSyncer
	gitFF    *GitSyncerFF // set instead of git when pair.ForgejoMirror is used
	issues   *IssueSyncer
	issuesFF *IssueSyncerFF // set instead of issues when pair.ForgejoMirror is used
	patches  *PatchSyncer
	wiki     *wiki.Client // this pair's Forgejo wiki — social replies land here when the token allows it, else fall back to an issue comment (see LogSocialReply)

	state  *state.Store
	fanout []fanoutTarget // see config.RepoPair.SocialFanout

	wikiModeMu   sync.Mutex
	wikiModeKnow bool // false until LogSocialReply has decided a mode at least once
	wikiModeWiki bool // last decided mode, valid only if wikiModeKnow
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

	rs := &RepoSyncer{pair: pair, forgejo: fc, wiki: wiki.New(pair.Forgejo.BaseURL, pair.Forgejo.Owner, pair.Forgejo.Repo, token), state: st}

	for _, f := range pair.SocialFanout {
		fanoutToken, err := config.ReadToken(f.Forgejo.TokenFile)
		if err != nil {
			return nil, fmt.Errorf("social_fanout %s: %w", f.Forgejo.BaseURL, err)
		}
		rs.fanout = append(rs.fanout, fanoutTarget{
			forgejo:     forgejo.New(f.Forgejo.BaseURL, f.Forgejo.Owner, f.Forgejo.Repo, fanoutToken),
			mapPairName: f.MapPairName,
		})
	}

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
		if pair.Sync.Issues {
			rs.issuesFF = &IssueSyncerFF{
				RepoPair: pair.Name, A: fc, B: mfc, State: st,
				AWebURL: forgejoWebURL, BWebURL: mirrorWebURL,
				Series: series, SeriesURL: forgejoWebURL,
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

// LogSocialReply appends one social-platform reply to this pair's own
// Forgejo wiki, on a page dedicated to platform ("Social-Discourse",
// "Social-Bluesky", "Social-Mastodon", "Social-Zulip", "Social-Tangled" —
// created on first use). This is where social discussion normally lands
// instead of as a real Forgejo/Radicle issue comment — see internal/wiki's
// package doc for why.
//
// Not every pair's token can actually write wiki pages: a pair graft only
// observes rather than pushes to (a third-party Forgejo instance sharing
// the same Radicle repo, deliberately configured with a read-only token)
// gets a 403 from the wiki API. There is no reliable way to predict this
// in advance — a repo's own GET .../repos/{owner}/{repo} "permissions"
// object reflects the underlying account's collaborator rights, not the
// presented token's own scope restriction, confirmed directly against a
// live instance where it reported push=true for a token that then 403'd
// on the actual write. So LogSocialReply always attempts the wiki write
// first and only falls back to CommentOnItem — the original
// Forgejo/Radicle issue-comment path — when that attempt itself comes
// back as wiki.ErrForbidden. forgejoID/radicleID identify the underlying
// mirrored item for that fallback; itemKind/itemTitle/itemURL describe it
// for the wiki timeline entry when the wiki write succeeds.
//
// This does mean a single pair's social history can end up split across
// the wiki and issue comments if its token's scope changes mid-flight
// (exactly what prompted this: a token gaining or losing write:repository
// between graft restarts). That split is never silent — see the WARN log
// below — but it is not reconciled retroactively; entries already written
// stay where they were written.
func (rs *RepoSyncer) LogSocialReply(platform, author, body, itemKind, itemTitle, itemURL, sourceURL string, forgejoID int64, radicleID string, occurredAt time.Time) error {
	slog.Info("LogSocialReply: entered", "pair", rs.pair.Name, "platform", platform, "fanoutCount", len(rs.fanout))
	if itemKind == "git" {
		return rs.logCommitReply(platform, author, body, itemTitle, itemURL, sourceURL)
	}
	page := "Social-" + platform
	header := fmt.Sprintf(
		"# Social — %s\n\nDiscussion about this repository mirrored from **%s**, newest first.",
		platform, platform)

	// Bridges already hand over "@name"; the entry adds its own "@".
	author = strings.TrimLeft(author, "@")
	quoted := "> " + strings.ReplaceAll(strings.TrimSpace(body), "\n", "\n> ")
	link := itemTitle
	if itemURL != "" {
		link = fmt.Sprintf("[%s](%s)", itemTitle, itemURL)
	}
	// A trackback to the reply's own post/comment on the originating
	// platform, not just the mirrored issue/patch — lets a reader jump
	// straight to the real conversation instead of only seeing the
	// quoted excerpt here. The trackback is rendered as a clickable link
	// with platform context.
	var trackbackLine string
	if sourceURL != "" {
		trackbackLine = fmt.Sprintf("\n\n[→ %s conversation](%s)", platform, sourceURL)
	}
	entry := fmt.Sprintf("### **%s UTC**\n\n%s &middot; %s &middot; **@%s**%s\n\n**%s** wrote:\n\n%s\n",
		occurredAt.UTC().Format("2006-01-02 15:04"), link, itemKind, author, trackbackLine, author, quoted)

	slog.Info("LogSocialReply: before fanout", "pair", rs.pair.Name)
	rs.fanoutSocialReply(itemKind, radicleID, platform, author, body)
	slog.Info("LogSocialReply: after fanout, before wiki write", "pair", rs.pair.Name)

	err := rs.wiki.AppendEntry(page, header, entry)
	slog.Info("LogSocialReply: wiki write returned", "pair", rs.pair.Name, "err", err)
	if err == nil {
		rs.noteWikiMode(true)
		return nil
	}
	if !errors.Is(err, wiki.ErrForbidden) {
		return err
	}
	rs.noteWikiMode(false)
	tagged := fmt.Sprintf("via %s, %s:\n\n%s", platform, author, body)
	return rs.postToFallbackIssue(platform, tagged)
}

// CommitThreadElsewhereError is returned by LogSocialReply for a reply to
// a commit whose discussion issue lives on another pair of the same series
// (each pair logs the same commit as its own activity, so a reply can point
// at any of them). The caller retries on that pair's RepoSyncer.
type CommitThreadElsewhereError struct {
	RepoPair string
}

func (e *CommitThreadElsewhereError) Error() string {
	return "commit discussion is held by pair " + e.RepoPair
}

// logCommitReply turns a social reply to a commit's post into a real
// discussion thread: the commit's first reply opens a dedicated Forgejo
// issue, and every later reply to the same commit — from any pair of the
// series — becomes a comment on it. Unlike a reply to an issue or patch,
// which already has its own thread (only the wiki timeline records it), a
// commit has nowhere else for a conversation to live. The issue and its
// comments are ordinary Forgejo content, so the regular issue and comment
// sync carries them on to Radicle and every other forge in the federation.
func (rs *RepoSyncer) logCommitReply(platform, author, body, commitSummary, commitURL, sourceURL string) error {
	sha := commitSHA(commitSummary, commitURL)
	if sha == "" {
		return fmt.Errorf("commit reply on %s: cannot tell which commit %q is", rs.pair.Name, commitSummary)
	}
	series := rs.Series()
	holder, issueID, ok, err := rs.state.CommitThread(series, sha)
	if err != nil {
		return fmt.Errorf("look up commit discussion: %w", err)
	}
	if ok && holder != rs.pair.Name {
		return &CommitThreadElsewhereError{RepoPair: holder}
	}
	if !ok {
		title := "Discussion du commit " + truncateTitle(commitSummary, 80)
		issueBody := fmt.Sprintf("Fil de discussion ouvert automatiquement par [graft](https://github.com/jeanbaptiste/graft) "+
			"pour les réponses publiées sur les réseaux sociaux à propos du commit [%s](%s).", commitSummary, commitURL)
		issue, err := rs.forgejo.CreateIssue(title, issueBody)
		if err != nil {
			return fmt.Errorf("open commit discussion issue: %w", err)
		}
		if err := rs.state.SaveCommitThread(series, sha, rs.pair.Name, issue.Index); err != nil {
			return fmt.Errorf("save commit discussion: %w", err)
		}
		issueID = issue.Index
		slog.Info("opened commit discussion", "pair", rs.pair.Name, "series", series, "sha", sha, "issue", issueID)
	}
	comment := fmt.Sprintf("**via %s, %s:**\n\n%s", platform, author, strings.TrimSpace(body))
	if sourceURL != "" {
		comment += fmt.Sprintf("\n\n[→ %s conversation](%s)", platform, sourceURL)
	}
	return rs.forgejo.CreateIssueComment(issueID, comment)
}

// commitSHA is the 7-character short SHA identifying a commit across
// pairs: the "%h %s" summary graft logs starts with it, and the commit
// URL (Forgejo .../commit/<sha>, Radicle .../commits/<sha>) ends with the
// full SHA.
func commitSHA(summary, url string) string {
	isHex := func(s string) bool {
		for _, r := range s {
			if !strings.ContainsRune("0123456789abcdef", r) {
				return false
			}
		}
		return s != ""
	}
	if first, _, _ := strings.Cut(strings.TrimSpace(summary), " "); len(first) >= 7 && isHex(first) {
		return first[:7]
	}
	if i := strings.LastIndex(url, "/"); i >= 0 && len(url)-i-1 >= 7 && isHex(url[i+1:]) {
		return url[i+1 : i+8]
	}
	return ""
}

func truncateTitle(s string, max int) string {
	s = strings.TrimSpace(strings.SplitN(s, "\n", 2)[0])
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max-1]) + "…"
}

// postToFallbackIssue is LogSocialReply's wiki.ErrForbidden fallback: a
// dedicated issue per (pair, platform) — created once, reused for every
// later reply on that pair+platform — rather than a comment on whichever
// original item the reply happened to be about. One issue per platform
// per pair keeps this from flooding the tracker: every reply on a pair
// whose wiki access is permanently unavailable (a read-mostly token on a
// third-party Forgejo, say) lands as a comment on the same issue, not a
// fresh one each time.
func (rs *RepoSyncer) postToFallbackIssue(platform, tagged string) error {
	forgejoID, _, ok, err := rs.state.SocialFallbackIssue(rs.pair.Name, platform)
	if err != nil {
		return fmt.Errorf("look up fallback issue: %w", err)
	}
	if !ok {
		issue, err := rs.forgejo.CreateIssue(
			"Social — "+platform,
			fmt.Sprintf("Replies about this repository from **%s**, collected here because the wiki isn't writable with this pair's current token scope.", platform),
		)
		if err != nil {
			return fmt.Errorf("create fallback issue: %w", err)
		}
		if err := rs.state.SaveSocialFallbackIssue(rs.pair.Name, platform, issue.Index, ""); err != nil {
			return fmt.Errorf("save fallback issue: %w", err)
		}
		forgejoID = issue.Index
	}
	return rs.forgejo.CreateIssueComment(forgejoID, tagged)
}

// fanoutTarget is one extra Forgejo instance LogSocialReply also posts to
// — see config.RepoPair.SocialFanout.
type fanoutTarget struct {
	forgejo     *forgejo.Client
	mapPairName string
}

// fanoutSocialReply best-effort posts a social reply as an issue comment
// on every configured fanout target, resolving each target's own issue
// number via mapPairName's item_mapping (already maintained by whatever
// sync actually links radicleID to that instance — see
// config.RepoPair.SocialFanout's doc comment; this never talks to
// Radicle directly itself). Silently skips a target with no resolvable
// mapping — that's the expected, common case for anything not yet
// mirrored there — and only logs on a real API failure, since a fanout
// target failing must never fail the caller's primary wiki/fallback
// write.
func (rs *RepoSyncer) fanoutSocialReply(itemKind, radicleID, platform, author, body string) {
	if len(rs.fanout) == 0 || radicleID == "" {
		return
	}
	tagged := fmt.Sprintf("via %s, %s:\n\n%s", platform, author, body)
	for _, f := range rs.fanout {
		m, err := rs.state.FindByRadicleID(f.mapPairName, itemKind, radicleID)
		if err != nil {
			slog.Error("social fanout: resolve mapping", "pair", rs.pair.Name, "map_pair", f.mapPairName, "err", err)
			continue
		}
		if m == nil {
			continue // not (yet) mirrored to this target under that pair — nothing to comment on
		}
		if err := f.forgejo.CreateIssueComment(m.ForgejoID, tagged); err != nil {
			slog.Error("social fanout: post comment", "pair", rs.pair.Name, "map_pair", f.mapPairName, "issue", m.ForgejoID, "err", err)
		}
	}
}

// noteWikiMode logs a WARN the first time this pair's wiki-writability
// differs from what it was last time, so a token-scope change (the
// operator adding or removing write:repository) always shows up in
// graft's own logs instead of silently splitting a series' social history
// across two places.
func (rs *RepoSyncer) noteWikiMode(writable bool) {
	rs.wikiModeMu.Lock()
	defer rs.wikiModeMu.Unlock()
	if rs.wikiModeKnow && rs.wikiModeWiki == writable {
		return
	}
	changed := rs.wikiModeKnow
	rs.wikiModeKnow = true
	rs.wikiModeWiki = writable
	if !changed {
		return // first decision ever for this pair — nothing to flag as a change
	}
	if writable {
		slog.Warn("social replies switched to wiki (token regained write access)", "pair", rs.pair.Name)
	} else {
		slog.Warn("social replies switched to issue comments (token lost write access to wiki)", "pair", rs.pair.Name)
	}
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
	if rs.issuesFF != nil {
		if issuesErr = rs.issuesFF.Sync(); issuesErr != nil {
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
