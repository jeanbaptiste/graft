package sync

import (
	"fmt"
	"strings"

	"graft/internal/radicle"
	"graft/internal/state"
)

// Origin values for a "comment" kind activity_log row — where the
// comment actually happened, shown in the dashboard tooltip only (see
// internal/status's event.Origin).
const (
	originForgejo     = "forgejo"
	originRadicle     = "radicle"
	originActivityPub = "activitypub"
	originATProto     = "atproto"
)

// mirrorPrefixes marks a comment's body as graft's own output — a copy
// already mirrored from somewhere else — so a comment-sync pass never
// treats it as new source content and mirrors it right back, which would
// loop. This is the fast, always-on guard; commentSeen (the DB-backed
// hash set) is the durable one, needed because a comment's *original*
// copy carries no such prefix and must still be recognized as already
// handled on every later pass.
var mirrorPrefixes = []string{"**via Forgejo:**", "**via Radicle:**", "**via Fediverse,", "**via Bluesky,"}

func isMirroredComment(body string) bool {
	for _, p := range mirrorPrefixes {
		if strings.HasPrefix(body, p) {
			return true
		}
	}
	return false
}

// maxCommentSummary bounds how much of a comment's body lands in the
// dashboard's summary column — comments run much longer than a commit
// subject line or an issue title.
const maxCommentSummary = 200

func truncateComment(body string) string {
	body = strings.TrimSpace(body)
	r := []rune(body)
	if len(r) <= maxCommentSummary {
		return body
	}
	return string(r[:maxCommentSummary-1]) + "…"
}

// syncIssueComments cross-mirrors an issue's comments between Forgejo and
// Radicle. ri is the already-fetched Radicle issue (its Discussion is
// used directly — no extra API call), forgejoIndex identifies the same
// issue on Forgejo.
func (s *IssueSyncer) syncIssueComments(forgejoIndex int64, ri radicle.Issue) error {
	fcs, err := s.Forgejo.ListIssueComments(forgejoIndex)
	if err != nil {
		return fmt.Errorf("list forgejo comments: %w", err)
	}

	for _, fc := range fcs {
		if isMirroredComment(fc.Body) {
			continue
		}
		hash := hashText(fc.Body)
		seen, err := s.State.CommentSeen(s.RepoPair, "issue", ri.ID, hash)
		if err != nil {
			return err
		}
		if seen {
			continue
		}
		if err := s.Radicle.CommentIssue(ri.ID, "**via Forgejo:**\n\n"+fc.Body); err != nil {
			return fmt.Errorf("mirror comment to radicle: %w", err)
		}
		if err := s.State.MarkCommentSeen(s.RepoPair, "issue", ri.ID, hash); err != nil {
			return err
		}
		s.State.LogActivity(state.Activity{
			RepoPair: s.RepoPair, Series: s.Series, SeriesURL: s.SeriesURL,
			Kind: "comment", Direction: state.ForgejoToRadicle, Origin: originForgejo,
			Summary:   truncateComment(fc.Body),
			URL:       s.RadicleWebURL + "/issues/" + ri.ID,
			ForgejoID: forgejoIndex, RadicleID: ri.ID,
		})
	}

	for _, item := range ri.Comments() {
		if isMirroredComment(item.Body) {
			continue
		}
		hash := hashText(item.Body)
		seen, err := s.State.CommentSeen(s.RepoPair, "issue", ri.ID, hash)
		if err != nil {
			return err
		}
		if seen {
			continue
		}
		if err := s.Forgejo.CreateIssueComment(forgejoIndex, "**via Radicle:**\n\n"+item.Body); err != nil {
			return fmt.Errorf("mirror comment to forgejo: %w", err)
		}
		if err := s.State.MarkCommentSeen(s.RepoPair, "issue", ri.ID, hash); err != nil {
			return err
		}
		s.State.LogActivity(state.Activity{
			RepoPair: s.RepoPair, Series: s.Series, SeriesURL: s.SeriesURL,
			Kind: "comment", Direction: state.RadicleToForgejo, Origin: originRadicle,
			Summary:   truncateComment(item.Body),
			URL:       fmt.Sprintf("%s/issues/%d", s.ForgejoWebURL, forgejoIndex),
			ForgejoID: forgejoIndex, RadicleID: ri.ID,
		})
	}
	return nil
}

// syncPatchComments is syncIssueComments' patch equivalent. Radicle-side
// comments come from the patch's first revision (Patch.Comments; see its
// doc comment — the same single-revision scoping CommentPatch already
// has) and are NOT independently verified against a live node the way
// issue comments are (see the Discussion field's doc comment in
// radicle/client.go). Degrades to silently seeing no Radicle comments
// if that schema assumption is wrong, rather than erroring.
func (s *PatchSyncer) syncPatchComments(forgejoIndex int64, rp radicle.Patch) error {
	if len(rp.Revisions) == 0 {
		return nil
	}
	revisionID := rp.Revisions[0].ID

	fcs, err := s.Forgejo.ListIssueComments(forgejoIndex)
	if err != nil {
		return fmt.Errorf("list forgejo comments: %w", err)
	}
	for _, fc := range fcs {
		if isMirroredComment(fc.Body) {
			continue
		}
		hash := hashText(fc.Body)
		seen, err := s.State.CommentSeen(s.RepoPair, "patch", rp.ID, hash)
		if err != nil {
			return err
		}
		if seen {
			continue
		}
		if err := s.Radicle.CommentPatch(revisionID, "**via Forgejo:**\n\n"+fc.Body); err != nil {
			return fmt.Errorf("mirror comment to radicle: %w", err)
		}
		if err := s.State.MarkCommentSeen(s.RepoPair, "patch", rp.ID, hash); err != nil {
			return err
		}
		s.State.LogActivity(state.Activity{
			RepoPair: s.RepoPair, Series: s.Series, SeriesURL: s.SeriesURL,
			Kind: "comment", Direction: state.ForgejoToRadicle, Origin: originForgejo,
			Summary:   truncateComment(fc.Body),
			URL:       s.RadicleWebURL + "/patches/" + rp.ID,
			ForgejoID: forgejoIndex, RadicleID: rp.ID,
		})
	}

	for _, item := range rp.Comments() {
		if isMirroredComment(item.Body) {
			continue
		}
		hash := hashText(item.Body)
		seen, err := s.State.CommentSeen(s.RepoPair, "patch", rp.ID, hash)
		if err != nil {
			return err
		}
		if seen {
			continue
		}
		if err := s.Forgejo.CreateIssueComment(forgejoIndex, "**via Radicle:**\n\n"+item.Body); err != nil {
			return fmt.Errorf("mirror comment to forgejo: %w", err)
		}
		if err := s.State.MarkCommentSeen(s.RepoPair, "patch", rp.ID, hash); err != nil {
			return err
		}
		s.State.LogActivity(state.Activity{
			RepoPair: s.RepoPair, Series: s.Series, SeriesURL: s.SeriesURL,
			Kind: "comment", Direction: state.RadicleToForgejo, Origin: originRadicle,
			Summary:   truncateComment(item.Body),
			URL:       fmt.Sprintf("%s/pulls/%d", s.ForgejoWebURL, forgejoIndex),
			ForgejoID: forgejoIndex, RadicleID: rp.ID,
		})
	}
	return nil
}
