package sync

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"graft/internal/forgejo"
	"graft/internal/radicle"
	"graft/internal/state"
)

// A marker prepended to mirrored items' bodies so a human browsing either
// side can tell where an issue/patch actually originated.
const mirrorNotice = "\n\n---\n_Mirrored automatically, do not edit here — edit at the source instead._"

// IssueSyncer mirrors issues between one Forgejo repository and one Radicle
// repository, create-only (see package doc for what that means).
type IssueSyncer struct {
	RepoPair string
	Forgejo  *forgejo.Client
	Radicle  *radicle.Client
	State    *state.Store
}

func (s *IssueSyncer) Sync() error {
	forgejoIssues, err := s.Forgejo.ListIssues()
	if err != nil {
		return fmt.Errorf("list forgejo issues: %w", err)
	}
	radicleIssues, err := s.Radicle.ListIssues()
	if err != nil {
		return fmt.Errorf("list radicle issues: %w", err)
	}

	for _, fi := range forgejoIssues {
		if err := s.mirrorForgejoToRadicle(fi); err != nil {
			return fmt.Errorf("mirror forgejo issue #%d: %w", fi.Index, err)
		}
	}
	for _, ri := range radicleIssues {
		if err := s.mirrorRadicleToForgejo(ri); err != nil {
			return fmt.Errorf("mirror radicle issue %s: %w", ri.ID, err)
		}
	}
	return nil
}

func (s *IssueSyncer) mirrorForgejoToRadicle(fi forgejo.Issue) error {
	existing, err := s.State.FindByForgejoID(s.RepoPair, "issue", fi.Index)
	if err != nil {
		return err
	}
	if existing != nil {
		return nil // already mirrored; edits not yet synced (see package doc)
	}

	radicleID, err := s.Radicle.OpenIssue(fi.Title, fi.Body+mirrorNotice)
	if err != nil {
		return fmt.Errorf("open radicle issue: %w", err)
	}

	if err := s.State.Upsert(s.RepoPair, state.ItemMapping{
		Kind:        "issue",
		ForgejoID:   fi.Index,
		RadicleID:   radicleID,
		ContentHash: hashText(fi.Title, fi.Body),
	}); err != nil {
		return err
	}
	s.State.LogActivity(s.RepoPair, "issue", state.ForgejoToRadicle, fi.Title)
	return nil
}

func (s *IssueSyncer) mirrorRadicleToForgejo(ri radicle.Issue) error {
	existing, err := s.State.FindByRadicleID(s.RepoPair, "issue", ri.ID)
	if err != nil {
		return err
	}
	if existing != nil {
		return nil
	}

	fi, err := s.Forgejo.CreateIssue(ri.Title, ri.Body()+mirrorNotice)
	if err != nil {
		return fmt.Errorf("create forgejo issue: %w", err)
	}

	if err := s.State.Upsert(s.RepoPair, state.ItemMapping{
		Kind:        "issue",
		ForgejoID:   fi.Index,
		RadicleID:   ri.ID,
		ContentHash: hashText(ri.Title, ri.Body()),
	}); err != nil {
		return err
	}
	s.State.LogActivity(s.RepoPair, "issue", state.RadicleToForgejo, ri.Title)
	return nil
}

func hashText(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}
