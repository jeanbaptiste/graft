package sync

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"

	"graft/internal/forgejo"
	"graft/internal/radicle"
	"graft/internal/state"
)

// PatchSyncer mirrors Forgejo pull requests and Radicle patches. It reuses
// the same working clone GitSyncer maintains, since a patch/PR can only be
// opened once its head commit is reachable on the destination side.
type PatchSyncer struct {
	RepoPair      string
	WorkDir       string
	RadHome       string
	DefaultBranch string
	Forgejo       *forgejo.Client
	Radicle       *radicle.Client
	State         *state.Store
	ForgejoWebURL string // https://host/owner/repo, for links in the UI
	RadicleWebURL string // https://explorer/nodes/host/rid, for links in the UI
	Series        string // dashboard row this pair's activity groups under
	SeriesURL     string // where clicking that row's name goes
}

func (s *PatchSyncer) git(args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = s.WorkDir
	// Must start from the inherited environment, not a bare slice: git
	// needs PATH to resolve the git-remote-rad helper for rad:// remotes.
	cmd.Env = append(os.Environ(), "RAD_HOME="+s.RadHome)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		return out.String(), fmt.Errorf("git %v: %w: %s", args, err, out.String())
	}
	return out.String(), nil
}

func (s *PatchSyncer) Sync() error {
	prs, err := s.Forgejo.ListPullRequests()
	if err != nil {
		return fmt.Errorf("list forgejo pull requests: %w", err)
	}
	patches, err := s.Radicle.ListPatches()
	if err != nil {
		return fmt.Errorf("list radicle patches: %w", err)
	}

	for _, pr := range prs {
		if err := s.mirrorForgejoToRadicle(pr); err != nil {
			return fmt.Errorf("mirror forgejo PR #%d: %w", pr.Index, err)
		}
	}
	for _, p := range patches {
		if err := s.mirrorRadicleToForgejo(p); err != nil {
			return fmt.Errorf("mirror radicle patch %s: %w", p.ID, err)
		}
	}
	return nil
}

var patchOpenedRe = regexp.MustCompile(`Patch ([0-9a-f]{40}) opened`)

func (s *PatchSyncer) mirrorForgejoToRadicle(pr forgejo.PullRequest) error {
	existing, err := s.State.FindByForgejoID(s.RepoPair, "patch", pr.Index)
	if err != nil {
		return err
	}
	if existing != nil {
		return nil
	}

	// Bring the PR's head branch content into the local clone, then open a
	// patch from it via the pseudo-ref, exactly as verified interactively:
	// `git push rad HEAD:refs/patches`.
	if _, err := s.git("fetch", "-q", "forgejo", pr.Head.Ref); err != nil {
		return fmt.Errorf("fetch PR head: %w", err)
	}
	if _, err := s.git("checkout", "-q", "-B", localPRBranch(pr.Index), "FETCH_HEAD"); err != nil {
		return fmt.Errorf("checkout PR head: %w", err)
	}
	// git push -o values cannot contain newlines, unlike issue/PR bodies.
	message := pr.Title + " (mirrored automatically, do not edit here)"
	out, err := s.git("push", "rad", "-o", "patch.message="+message, "HEAD:refs/patches")
	// The working clone's mirroring never touches local branch refs (only
	// FETCH_HEAD and the remotes), so there may be no local default-branch
	// ref to switch back to yet: fetch and (re)create it rather than assume
	// it exists.
	if _, errFetch := s.git("fetch", "-q", "forgejo", s.DefaultBranch); errFetch != nil {
		return fmt.Errorf("fetch %s to restore: %w", s.DefaultBranch, errFetch)
	}
	if _, errCheckout := s.git("checkout", "-q", "-B", s.DefaultBranch, "FETCH_HEAD"); errCheckout != nil {
		return fmt.Errorf("checkout back to %s: %w", s.DefaultBranch, errCheckout)
	}
	if err != nil {
		return fmt.Errorf("open radicle patch: %w", err)
	}

	m := patchOpenedRe.FindStringSubmatch(out)
	if m == nil {
		return fmt.Errorf("could not parse patch id from: %q", out)
	}

	if err := s.State.Upsert(s.RepoPair, state.ItemMapping{
		Kind:        "patch",
		ForgejoID:   pr.Index,
		RadicleID:   m[1],
		ContentHash: hashText(pr.Title, pr.Body),
	}); err != nil {
		return err
	}
	s.State.LogActivity(state.Activity{
		RepoPair: s.RepoPair, Series: s.Series, SeriesURL: s.SeriesURL,
		Kind: "patch", Direction: state.ForgejoToRadicle, Summary: pr.Title,
		URL:       s.RadicleWebURL + "/patches/" + m[1],
		ForgejoID: pr.Index, RadicleID: m[1],
	})
	return nil
}

func (s *PatchSyncer) mirrorRadicleToForgejo(p radicle.Patch) error {
	existing, err := s.State.FindByRadicleID(s.RepoPair, "patch", p.ID)
	if err != nil {
		return err
	}
	if existing != nil {
		return nil
	}
	if len(p.Revisions) == 0 {
		return fmt.Errorf("patch has no revisions")
	}
	latest := p.Revisions[len(p.Revisions)-1]

	branch := localPatchBranch(p.ID)
	if _, err := s.git("fetch", "-q", "rad", latest.OID); err != nil {
		return fmt.Errorf("fetch patch revision: %w", err)
	}
	if _, err := s.git("branch", "-q", "-f", branch, "FETCH_HEAD"); err != nil {
		return fmt.Errorf("branch patch revision: %w", err)
	}
	if _, err := s.git("push", "-q", "forgejo", branch+":refs/heads/"+branch); err != nil {
		return fmt.Errorf("push branch to forgejo: %w", err)
	}

	pr, err := s.Forgejo.CreatePullRequest(p.Title, latest.Description+mirrorNotice, branch, s.DefaultBranch)
	if err != nil {
		return fmt.Errorf("create forgejo pull request: %w", err)
	}

	if err := s.State.Upsert(s.RepoPair, state.ItemMapping{
		Kind:        "patch",
		ForgejoID:   pr.Index,
		RadicleID:   p.ID,
		ContentHash: hashText(p.Title, latest.Description),
	}); err != nil {
		return err
	}
	s.State.LogActivity(state.Activity{
		RepoPair: s.RepoPair, Series: s.Series, SeriesURL: s.SeriesURL,
		Kind: "patch", Direction: state.RadicleToForgejo, Summary: p.Title,
		URL:       fmt.Sprintf("%s/pulls/%d", s.ForgejoWebURL, pr.Index),
		ForgejoID: pr.Index, RadicleID: p.ID,
	})
	return nil
}

func localPRBranch(index int64) string {
	return fmt.Sprintf("sync/forgejo-pr-%d", index)
}

func localPatchBranch(radicleID string) string {
	short := radicleID
	if len(short) > 12 {
		short = short[:12]
	}
	return "sync/radicle-patch-" + strings.ToLower(short)
}
