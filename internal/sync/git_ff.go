package sync

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"graft/internal/state"
)

// GitSyncerFF mirrors a repository's default branch between two Forgejo
// instances (no Radicle side involved), the same fast-forward-only way
// GitSyncer mirrors Forgejo<->Radicle: a real divergence is reported, never
// force-pushed. "a" is the pair's primary Forgejo (pair.Forgejo); "b" is its
// mirror (pair.ForgejoMirror).
type GitSyncerFF struct {
	RepoPair      string
	WorkDir       string
	AURL          string // authenticated clone URL (token embedded)
	AWebURL       string
	BURL          string
	BWebURL       string
	DefaultBranch string
	State         *state.Store
	Series        string
	SeriesURL     string
	Bluesky       *BlueskyPoster
	// AuthorizedIntegrationSource mirrors GitSyncer's field of the same
	// name — kept here for symmetry (SetAuthorizedIntegrationSource is
	// called uniformly across every pair in a series) even though
	// detectAuthorizedIntegrations doesn't yet look at ForgejoMirror hosts.
	AuthorizedIntegrationSource string
	aiSkipStreak                int
}

// run captures stdout and stderr separately — a git warning (e.g. "warning:
// redirecting to ..." on a renamed repo, printed to stderr) must never land
// in the stdout callers like headOf parse for a commit SHA. See git.go's
// run, which has the same split for the same reason.
func (g *GitSyncerFF) run(args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), execTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = g.WorkDir
	var out, errOut bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	err := cmd.Run()
	if err != nil {
		return out.String(), fmt.Errorf("git %v: %w: %s", args, err, errOut.String())
	}
	return out.String(), nil
}

func (g *GitSyncerFF) ensureClone() error {
	if _, err := os.Stat(filepath.Join(g.WorkDir, ".git")); err == nil {
		return nil
	}
	if err := os.MkdirAll(g.WorkDir, 0o700); err != nil {
		return fmt.Errorf("create work dir: %w", err)
	}
	if _, err := g.run("init", "-q", "-b", g.DefaultBranch); err != nil {
		return err
	}
	if _, err := g.run("remote", "add", "a", g.AURL); err != nil {
		return err
	}
	if _, err := g.run("remote", "add", "b", g.BURL); err != nil {
		return err
	}
	return nil
}

func (g *GitSyncerFF) headOf(remote, branch string) (string, error) {
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

func (g *GitSyncerFF) isAncestor(a, b string) bool {
	_, err := g.run("merge-base", "--is-ancestor", a, b)
	return err == nil
}

// Sync mirrors the default branch in whichever direction has new,
// fast-forwardable commits — same logic as GitSyncer.Sync, between "a" and
// "b" instead of "forgejo" and "rad".
func (g *GitSyncerFF) Sync() error {
	if err := g.ensureClone(); err != nil {
		return err
	}

	aHead, err := g.headOf("a", g.DefaultBranch)
	if err != nil {
		return fmt.Errorf("read a head: %w", err)
	}
	bHead, err := g.headOf("b", g.DefaultBranch)
	if err != nil {
		return fmt.Errorf("read b head: %w", err)
	}

	lastA, lastB, err := g.State.GitCursor(g.RepoPair)
	if err != nil {
		return fmt.Errorf("read cursor: %w", err)
	}

	switch {
	case aHead == bHead:
		g.aiSkipStreak = 0

	case bHead == "" || bHead == lastB:
		if err := g.pushBranch("a", "b", bHead, state.ForgejoAToForgejoB); err != nil {
			return fmt.Errorf("mirror a -> b: %w", err)
		}

	case aHead == "" || aHead == lastA:
		if g.AuthorizedIntegrationSource != "" && g.aiSkipStreak < maxAISkipStreak {
			g.aiSkipStreak++
			break
		}
		if err := g.pushBranch("b", "a", aHead, state.ForgejoBToForgejoA); err != nil {
			return fmt.Errorf("mirror b -> a: %w", err)
		}

	case g.isAncestor(lastA, aHead) && g.isAncestor(lastB, bHead) && g.isAncestor(bHead, aHead):
		if err := g.pushBranch("a", "b", bHead, state.ForgejoAToForgejoB); err != nil {
			return fmt.Errorf("mirror a -> b: %w", err)
		}

	case g.isAncestor(aHead, bHead):
		if g.AuthorizedIntegrationSource != "" && g.aiSkipStreak < maxAISkipStreak {
			g.aiSkipStreak++
			break
		}
		if err := g.pushBranch("b", "a", aHead, state.ForgejoBToForgejoA); err != nil {
			return fmt.Errorf("mirror b -> a: %w", err)
		}

	default:
		return fmt.Errorf(
			"%s: a and b diverged since last sync (a=%s b=%s, last a=%s b=%s); resolve manually, not auto-merging",
			g.RepoPair, aHead, bHead, lastA, lastB)
	}

	aHead, err = g.headOf("a", g.DefaultBranch)
	if err != nil {
		return err
	}
	bHead, err = g.headOf("b", g.DefaultBranch)
	if err != nil {
		return err
	}
	return g.State.SetGitCursor(g.RepoPair, aHead, bHead)
}

func (g *GitSyncerFF) pushBranch(from, to, oldToHead, direction string) error {
	if _, err := g.run("fetch", "-q", from, g.DefaultBranch); err != nil {
		return fmt.Errorf("fetch %s: %w", from, err)
	}
	if _, err := g.run("push", to, "FETCH_HEAD:refs/heads/"+g.DefaultBranch); err != nil {
		return fmt.Errorf("push %s: %w", to, err)
	}
	g.logMirroredCommits(oldToHead, direction, to)
	return nil
}

func (g *GitSyncerFF) logMirroredCommits(oldHead, direction, to string) {
	rangeSpec := "FETCH_HEAD"
	if oldHead != "" {
		rangeSpec = oldHead + "..FETCH_HEAD"
	}
	out, err := g.run("log", "--pretty=format:%H%x1f%h %s", rangeSpec)
	if err != nil || out == "" {
		return
	}
	webURL := g.AWebURL
	if to == "b" {
		webURL = g.BWebURL
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
		if webURL != "" {
			url = webURL + "/commit/" + full
		}
		id, _ := g.State.LogActivity(state.Activity{
			RepoPair: g.RepoPair, Series: g.Series, SeriesURL: g.SeriesURL,
			Kind: "git", Direction: direction, Summary: summary, URL: url,
		})
		text := g.Series + ": " + summary
		if url != "" {
			text += "\n" + url
		}
		if postURI, err := g.Bluesky.Post(text); err == nil && postURI != "" {
			g.State.SaveATProtoPost(postURI, id, g.Series)
		}
	}
}
