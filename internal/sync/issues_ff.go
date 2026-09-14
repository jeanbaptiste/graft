package sync

import (
	"fmt"
	"strings"

	"graft/internal/forgejo"
	"graft/internal/state"
)

// IssueSyncerFF mirrors issues (and their comments) between two Forgejo
// instances directly — no Radicle side involved — the same create-only way
// IssueSyncer mirrors Forgejo<->Radicle. "a" is the pair's primary Forgejo
// (pair.Forgejo); "b" is its mirror (pair.ForgejoMirror), matching
// GitSyncerFF's naming.
//
// Reuses the existing item_mapping table rather than adding a new one: for
// an FF pair, ForgejoID holds side a's issue number and RadicleID (a TEXT
// column) holds side b's issue number as a string. The column names stay
// Radicle-flavored, but nothing about the table's shape actually assumes
// Radicle — it was already just "the other side's id, in whatever form",
// and a Forgejo issue number stringifies losslessly. Kept this way instead
// of a parallel ff_item_mapping table specifically to avoid a schema
// migration for what's otherwise an identical mapping problem.
type IssueSyncerFF struct {
	RepoPair  string
	A         *forgejo.Client
	B         *forgejo.Client
	State     *state.Store
	AWebURL   string
	BWebURL   string
	Series    string
	SeriesURL string
}

func (s *IssueSyncerFF) Sync() error {
	aIssues, err := s.A.ListIssues()
	if err != nil {
		return fmt.Errorf("list a issues: %w", err)
	}
	bIssues, err := s.B.ListIssues()
	if err != nil {
		return fmt.Errorf("list b issues: %w", err)
	}
	bByID := make(map[string]forgejo.Issue, len(bIssues))
	for _, bi := range bIssues {
		bByID[fmt.Sprint(bi.Index)] = bi
	}

	for _, ai := range aIssues {
		if err := s.mirrorAToB(ai, bByID); err != nil {
			return fmt.Errorf("mirror a issue #%d: %w", ai.Index, err)
		}
	}
	for _, bi := range bIssues {
		if err := s.mirrorBToA(bi); err != nil {
			return fmt.Errorf("mirror b issue #%d: %w", bi.Index, err)
		}
	}
	return nil
}

func (s *IssueSyncerFF) mirrorAToB(ai forgejo.Issue, bByID map[string]forgejo.Issue) error {
	existing, err := s.State.FindByForgejoID(s.RepoPair, "issue", ai.Index)
	if err != nil {
		return err
	}
	if existing != nil {
		if bi, ok := bByID[existing.RadicleID]; ok {
			if err := s.syncComments(ai.Index, bi.Index); err != nil {
				return fmt.Errorf("sync comments: %w", err)
			}
		}
		return nil // already mirrored; edits not yet synced (see issues.go's package doc)
	}

	bi, err := s.B.CreateIssue(ai.Title, ai.Body+mirrorNotice)
	if err != nil {
		return fmt.Errorf("create b issue: %w", err)
	}

	if err := s.State.Upsert(s.RepoPair, state.ItemMapping{
		Kind: "issue", ForgejoID: ai.Index, RadicleID: fmt.Sprint(bi.Index),
		ContentHash: hashText(ai.Title, ai.Body),
	}); err != nil {
		return err
	}
	s.State.LogActivity(state.Activity{
		RepoPair: s.RepoPair, Series: s.Series, SeriesURL: s.SeriesURL,
		Kind: "issue", Direction: state.ForgejoAToForgejoB, Summary: ai.Title,
		URL:       fmt.Sprintf("%s/issues/%d", s.BWebURL, bi.Index),
		ForgejoID: ai.Index, RadicleID: fmt.Sprint(bi.Index),
	})
	return nil
}

func (s *IssueSyncerFF) mirrorBToA(bi forgejo.Issue) error {
	existing, err := s.State.FindByRadicleID(s.RepoPair, "issue", fmt.Sprint(bi.Index))
	if err != nil {
		return err
	}
	if existing != nil {
		return nil
	}

	ai, err := s.A.CreateIssue(bi.Title, bi.Body+mirrorNotice)
	if err != nil {
		return fmt.Errorf("create a issue: %w", err)
	}

	if err := s.State.Upsert(s.RepoPair, state.ItemMapping{
		Kind: "issue", ForgejoID: ai.Index, RadicleID: fmt.Sprint(bi.Index),
		ContentHash: hashText(bi.Title, bi.Body),
	}); err != nil {
		return err
	}
	s.State.LogActivity(state.Activity{
		RepoPair: s.RepoPair, Series: s.Series, SeriesURL: s.SeriesURL,
		Kind: "issue", Direction: state.ForgejoBToForgejoA, Summary: bi.Title,
		URL:       fmt.Sprintf("%s/issues/%d", s.AWebURL, ai.Index),
		ForgejoID: ai.Index, RadicleID: fmt.Sprint(bi.Index),
	})
	return nil
}

// syncComments cross-mirrors one issue's comments between a and b, the FF
// equivalent of comments.go's syncIssueComments. itemID (for comment_seen,
// which is keyed by a string id regardless of side) uses b's issue number,
// matching mirrorAToB/mirrorBToA's RadicleID convention above.
func (s *IssueSyncerFF) syncComments(aIndex, bIndex int64) error {
	itemID := fmt.Sprint(bIndex)

	acs, err := s.A.ListIssueComments(aIndex)
	if err != nil {
		return fmt.Errorf("list a comments: %w", err)
	}
	for _, c := range acs {
		if state.IsMirroredComment(c.Body) {
			continue
		}
		hash := hashText(c.Body)
		seen, err := s.State.CommentSeen(s.RepoPair, "issue", itemID, hash)
		if err != nil {
			return err
		}
		if seen {
			continue
		}
		if err := s.B.CreateIssueComment(bIndex, state.MarkMirrored("**via Forgejo ("+shortHost(s.AWebURL)+"):**\n\n"+c.Body)); err != nil {
			return fmt.Errorf("mirror comment a->b: %w", err)
		}
		if err := s.State.MarkCommentSeen(s.RepoPair, "issue", itemID, hash); err != nil {
			return err
		}
		s.State.LogActivity(state.Activity{
			RepoPair: s.RepoPair, Series: s.Series, SeriesURL: s.SeriesURL,
			Kind: "comment", Direction: state.ForgejoAToForgejoB, Origin: originForgejo,
			Summary:   truncateComment(c.Body),
			URL:       fmt.Sprintf("%s/issues/%d", s.BWebURL, bIndex),
			ForgejoID: aIndex, RadicleID: itemID,
		})
	}

	bcs, err := s.B.ListIssueComments(bIndex)
	if err != nil {
		return fmt.Errorf("list b comments: %w", err)
	}
	for _, c := range bcs {
		if state.IsMirroredComment(c.Body) {
			continue
		}
		hash := hashText(c.Body)
		seen, err := s.State.CommentSeen(s.RepoPair, "issue", itemID, hash)
		if err != nil {
			return err
		}
		if seen {
			continue
		}
		if err := s.A.CreateIssueComment(aIndex, state.MarkMirrored("**via Forgejo ("+shortHost(s.BWebURL)+"):**\n\n"+c.Body)); err != nil {
			return fmt.Errorf("mirror comment b->a: %w", err)
		}
		if err := s.State.MarkCommentSeen(s.RepoPair, "issue", itemID, hash); err != nil {
			return err
		}
		s.State.LogActivity(state.Activity{
			RepoPair: s.RepoPair, Series: s.Series, SeriesURL: s.SeriesURL,
			Kind: "comment", Direction: state.ForgejoBToForgejoA, Origin: originForgejo,
			Summary:   truncateComment(c.Body),
			URL:       fmt.Sprintf("%s/issues/%d", s.AWebURL, aIndex),
			ForgejoID: aIndex, RadicleID: itemID,
		})
	}
	return nil
}

// shortHost pulls a bare host out of a web URL for an inline attribution
// label, e.g. "https://f1.cyberwild.org/owner/repo" -> "f1.cyberwild.org".
func shortHost(webURL string) string {
	rest := webURL
	if _, after, ok := strings.Cut(rest, "://"); ok {
		rest = after
	}
	if host, _, ok := strings.Cut(rest, "/"); ok {
		return host
	}
	return rest
}
