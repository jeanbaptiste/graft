// Package sync is the actual synchronization brick: it takes a Forgejo
// client and a Radicle client for one repo pair and mirrors content between
// them, using the state store to stay idempotent across runs.
//
// Scope of this first version, deliberately: fast-forward-only git mirroring
// (a real divergence is logged and skipped, never force-pushed), and
// create-only issue/patch mirroring (edits made after the initial mirror are
// not propagated — that needs per-item diffing, left for a follow-up once
// the create path is proven in practice).
package sync

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"graft/internal/state"
)

// execTimeout bounds every git/rad subprocess this package runs. Without
// it, one hung fetch/push (a slow network, an unresponsive Radicle node)
// would block the single-threaded sync loop forever — every other
// configured pair too, not just the stuck one.
const execTimeout = 5 * time.Minute

// GitSyncer mirrors a repository's default branch between Forgejo and
// Radicle using a local working clone with two remotes.
type GitSyncer struct {
	RepoPair      string // key used in the state store
	WorkDir       string // local clone, created on first run if absent
	ForgejoURL    string // authenticated clone URL (token embedded), for git
	ForgejoWebURL string // https://host/owner/repo, for links in the UI
	RadicleWebURL string // https://explorer/nodes/host/rid, for links in the UI
	ForgejoBranch string
	RID           string
	RadHome       string
	State         *state.Store
	Series        string         // dashboard row this pair's activity groups under
	SeriesURL     string         // where clicking that row's name goes
	Bluesky       *BlueskyPoster // nil unless this pair has bluesky configured
	// AuthorizedIntegrationSource, if set, names another pair's Forgejo
	// host that already pushes new commits directly into this pair's
	// Forgejo via Authorized Integrations (see authint.go) — faster than
	// this pair waiting for the next poll to relay it via Radicle. When
	// set, Sync skips its own rad -> forgejo push and just observes
	// whether the content has already arrived, falling back to pushing it
	// itself only once a real divergence would otherwise be reported —
	// or after maxAISkipStreak consecutive passes without convergence
	// (see Sync), since detection here is a content heuristic (authint.go),
	// not a guarantee the workflow is actually delivering.
	AuthorizedIntegrationSource string
	aiSkipStreak                int
}

// maxAISkipStreak bounds how many consecutive passes GitSyncer will trust
// AuthorizedIntegrationSource without seeing forgejo actually catch up to
// radicle. Detection is a heuristic (a matching string in a workflow
// file), not proof the integration is configured correctly or even still
// working — without this bound, a stale or misconfigured workflow would
// make graft silently stop mirroring that direction forever.
const maxAISkipStreak = 10

// run captures stdout and stderr separately — a git warning (e.g. "warning:
// redirecting to ..." on a renamed repo) is printed to stderr and must
// never end up in the stdout headOf parses for a commit SHA. Discovered via
// a real graft-presentation pair whose Forgejo peer got renamed: git's
// redirect warning landed first in the merged buffer and headOf's Sscanf
// picked "warning:" up as the SHA instead of skipping to the real line.
func (g *GitSyncer) run(args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), execTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = g.WorkDir
	cmd.Env = append(os.Environ(), "RAD_HOME="+g.RadHome)
	var out, errOut bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	err := cmd.Run()
	if err != nil {
		return out.String(), fmt.Errorf("git %v: %w: %s", args, err, errOut.String())
	}
	return out.String(), nil
}

// ensureClone creates the working clone and its two remotes if this is the
// first run for this repo pair. The rad remote mirrors exactly what
// `rad init` itself sets up (verified against a live repo): a fetch URL of
// rad://<rid>, and a separate push URL of rad://<rid>/<local-node-id> —
// git-remote-rad needs the pushing node's own id in the push URL.
func (g *GitSyncer) ensureClone() error {
	if _, err := os.Stat(filepath.Join(g.WorkDir, ".git")); err == nil {
		return nil
	}
	if err := os.MkdirAll(g.WorkDir, 0o700); err != nil {
		return fmt.Errorf("create work dir: %w", err)
	}
	if _, err := g.run("init", "-q", "-b", g.ForgejoBranch); err != nil {
		return err
	}
	if _, err := g.run("remote", "add", "forgejo", g.ForgejoURL); err != nil {
		return err
	}

	ridBare := strings.TrimPrefix(g.RID, "rad:")
	nid, err := g.localNodeID()
	if err != nil {
		return fmt.Errorf("resolve local node id: %w", err)
	}
	if _, err := g.run("remote", "add", "rad", "rad://"+ridBare); err != nil {
		return err
	}
	if _, err := g.run("remote", "set-url", "--push", "rad", "rad://"+ridBare+"/"+nid); err != nil {
		return err
	}
	return nil
}

// localNodeID returns the Radicle node id (DID, without the did:key:
// prefix) that owns g.RadHome.
func (g *GitSyncer) localNodeID() (string, error) {
	// --nid is deprecated in favor of `rad node status --only nid` and
	// prints a warning to stderr; keep that separate from stdout so it
	// never ends up concatenated into the id itself.
	ctx, cancel := context.WithTimeout(context.Background(), execTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "rad", "self", "--nid")
	cmd.Env = append(os.Environ(), "RAD_HOME="+g.RadHome)
	var out, errOut bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("rad self --nid: %w: %s", err, errOut.String())
	}
	return strings.TrimSpace(out.String()), nil
}

// headOf returns the commit SHA a remote branch currently points to, or ""
// if the branch does not exist yet on that remote.
func (g *GitSyncer) headOf(remote, branch string) (string, error) {
	out, err := g.run("ls-remote", remote, "refs/heads/"+branch)
	if err != nil {
		return "", err
	}
	if out == "" {
		return "", nil
	}
	var sha string
	fmt.Sscanf(out, "%s", &sha)
	return sha, nil
}

// isAncestor reports whether commit a is an ancestor of commit b (i.e.
// fast-forwarding from a to b is safe).
func (g *GitSyncer) isAncestor(a, b string) bool {
	_, err := g.run("merge-base", "--is-ancestor", a, b)
	return err == nil
}

// Sync mirrors the default branch in whichever direction has new,
// fast-forwardable commits. It never force-pushes: a genuine divergence
// (commits on both sides since the last sync, neither an ancestor of the
// other) is reported as an error so the operator can resolve it by hand.
func (g *GitSyncer) Sync() error {
	if err := g.ensureClone(); err != nil {
		return err
	}

	forgejoHead, err := g.headOf("forgejo", g.ForgejoBranch)
	if err != nil {
		return fmt.Errorf("read forgejo head: %w", err)
	}
	radHead, err := g.headOf("rad", g.ForgejoBranch)
	if err != nil {
		return fmt.Errorf("read rad head: %w", err)
	}

	lastForgejo, lastRad, err := g.State.GitCursor(g.RepoPair)
	if err != nil {
		return fmt.Errorf("read cursor: %w", err)
	}

	// headOf only reads refs (ls-remote); the ancestry checks below run
	// git merge-base in the local clone, which fails — and so reads as
	// "not an ancestor" — for any commit not fetched yet. Without this, a
	// merge commit that reconciles both sides is still reported as a
	// divergence forever, since nothing else fetches it before the check.
	if forgejoHead != radHead {
		if _, err := g.run("fetch", "-q", "forgejo", g.ForgejoBranch); err != nil && forgejoHead != "" {
			return fmt.Errorf("fetch forgejo: %w", err)
		}
		if _, err := g.run("fetch", "-q", "rad", g.ForgejoBranch); err != nil && radHead != "" {
			return fmt.Errorf("fetch rad: %w", err)
		}
	}

	switch {
	case forgejoHead == radHead:
		// Already in sync; still record the cursor in case this is the
		// first run and it was empty before.
		g.aiSkipStreak = 0

	case radHead == "" || radHead == lastRad:
		// Radicle hasn't moved since we last looked: forgejo is ahead.
		if err := g.pushBranch("forgejo", "rad", radHead, state.ForgejoToRadicle); err != nil {
			return fmt.Errorf("mirror forgejo -> rad: %w", err)
		}

	case forgejoHead == "" || forgejoHead == lastForgejo:
		// Forgejo hasn't moved: rad is ahead.
		if g.AuthorizedIntegrationSource != "" && g.aiSkipStreak < maxAISkipStreak {
			// Someone else's CI already pushes directly into this
			// Forgejo (see AuthorizedIntegrationSource's doc comment) —
			// faster than waiting for us to relay it via Radicle.
			// Nothing to do; next pass sees forgejoHead == radHead once
			// it arrives, same as any other convergence.
			g.aiSkipStreak++
			break
		}
		if err := g.pushBranch("rad", "forgejo", forgejoHead, state.RadicleToForgejo); err != nil {
			return fmt.Errorf("mirror rad -> forgejo: %w", err)
		}

	case g.isAncestor(lastForgejo, forgejoHead) && g.isAncestor(lastRad, radHead) &&
		g.isAncestor(radHead, forgejoHead):
		// Both moved, but rad's tip is contained in forgejo's: forgejo wins.
		if err := g.pushBranch("forgejo", "rad", radHead, state.ForgejoToRadicle); err != nil {
			return fmt.Errorf("mirror forgejo -> rad: %w", err)
		}

	case g.isAncestor(forgejoHead, radHead):
		// Symmetric case: forgejo's tip is contained in rad's.
		if g.AuthorizedIntegrationSource != "" && g.aiSkipStreak < maxAISkipStreak {
			g.aiSkipStreak++
			break
		}
		if err := g.pushBranch("rad", "forgejo", forgejoHead, state.RadicleToForgejo); err != nil {
			return fmt.Errorf("mirror rad -> forgejo: %w", err)
		}

	default:
		return fmt.Errorf(
			"%s: forgejo and rad diverged since last sync (forgejo=%s rad=%s, last forgejo=%s rad=%s); resolve manually, not auto-merging",
			g.RepoPair, forgejoHead, radHead, lastForgejo, lastRad)
	}

	forgejoHead, err = g.headOf("forgejo", g.ForgejoBranch)
	if err != nil {
		return err
	}
	radHead, err = g.headOf("rad", g.ForgejoBranch)
	if err != nil {
		return err
	}
	return g.State.SetGitCursor(g.RepoPair, forgejoHead, radHead)
}

// pushBranch fetches g.ForgejoBranch from "from" and pushes it to "to".
// oldToHead is that branch's head on "to" before the push (possibly ""),
// used only to log which commits this pass actually moved, for the status
// page's activity calendar.
func (g *GitSyncer) pushBranch(from, to, oldToHead, direction string) error {
	if _, err := g.run("fetch", "-q", from, g.ForgejoBranch); err != nil {
		return fmt.Errorf("fetch %s: %w", from, err)
	}
	if _, err := g.run("push", to, "FETCH_HEAD:refs/heads/"+g.ForgejoBranch); err != nil {
		return fmt.Errorf("push %s: %w", to, err)
	}
	g.logMirroredCommits(oldToHead, direction, to)
	return nil
}

// logMirroredCommits records one activity_log entry per commit newly
// reachable from FETCH_HEAD that wasn't reachable from oldHead. Best-effort:
// a failure here never fails the sync itself, it just leaves the status
// page's calendar short an entry. to is "forgejo" or "rad" — the remote the
// commits were just pushed to — so the logged link (and the dashboard's
// derived server label) points at where the content actually landed,
// instead of always pointing at Forgejo regardless of direction.
func (g *GitSyncer) logMirroredCommits(oldHead, direction, to string) {
	rangeSpec := "FETCH_HEAD"
	if oldHead != "" {
		rangeSpec = oldHead + "..FETCH_HEAD"
	}
	// %x1f (unit separator) between fields: commit subjects can contain
	// anything else printable, so a space or comma delimiter would be
	// ambiguous to split back apart.
	out, err := g.run("log", "--pretty=format:%H%x1f%h %s", rangeSpec)
	if err != nil || out == "" {
		return
	}
	for _, line := range strings.Split(out, "\n") {
		if line == "" {
			continue
		}
		full, summary, ok := strings.Cut(line, "\x1f")
		if !ok {
			continue
		}
		url := ""
		switch to {
		case "rad":
			if g.RadicleWebURL != "" {
				url = g.RadicleWebURL + "/commits/" + full
			}
		default:
			if g.ForgejoWebURL != "" {
				url = g.ForgejoWebURL + "/commit/" + full
			}
		}
		id, _ := g.State.LogActivity(state.Activity{
			RepoPair: g.RepoPair, Series: g.Series, SeriesURL: g.SeriesURL,
			Kind: "git", Direction: direction, Summary: summary, URL: url,
		})
		// Best-effort, same as LogActivity above: a Bluesky hiccup never
		// fails the sync, and nil Bluesky (the common case, no account
		// configured) is a silent no-op.
		text := g.Series + ": " + summary
		if url != "" {
			text += "\n" + url
		}
		if postURI, err := g.Bluesky.Post(text); err == nil && postURI != "" {
			g.State.SaveATProtoPost(postURI, id, g.Series)
		}
	}
}
