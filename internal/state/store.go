// Package state persists sync bookkeeping so the daemon can tell, across
// restarts, what has already been mirrored and avoid duplicating issues,
// patches or commits on every poll.
package state

import (
	"database/sql"
	"fmt"

	_ "modernc.org/sqlite"
)

// Store wraps the local SQLite database used to track sync state.
type Store struct {
	db *sql.DB
}

// Open creates (if needed) and opens the state database at path.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open state db: %w", err)
	}
	if err := migrate(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate state db: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func migrate(db *sql.DB) error {
	_, err := db.Exec(`
CREATE TABLE IF NOT EXISTS git_cursor (
	repo_pair       TEXT PRIMARY KEY,
	forgejo_sha     TEXT NOT NULL DEFAULT '',
	radicle_sha     TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS item_mapping (
	repo_pair       TEXT NOT NULL,
	kind            TEXT NOT NULL, -- 'issue' or 'patch'
	forgejo_id      INTEGER NOT NULL DEFAULT 0,
	radicle_id      TEXT NOT NULL DEFAULT '',
	content_hash    TEXT NOT NULL DEFAULT '',
	synced_at       TEXT NOT NULL,
	PRIMARY KEY (repo_pair, kind, forgejo_id, radicle_id)
);

CREATE INDEX IF NOT EXISTS idx_item_mapping_forgejo
	ON item_mapping (repo_pair, kind, forgejo_id);
CREATE INDEX IF NOT EXISTS idx_item_mapping_radicle
	ON item_mapping (repo_pair, kind, radicle_id);
`)
	return err
}

// GitCursor returns the last-synced commit SHAs on each side for a repo pair.
func (s *Store) GitCursor(repoPair string) (forgejoSHA, radicleSHA string, err error) {
	row := s.db.QueryRow(`SELECT forgejo_sha, radicle_sha FROM git_cursor WHERE repo_pair = ?`, repoPair)
	err = row.Scan(&forgejoSHA, &radicleSHA)
	if err == sql.ErrNoRows {
		return "", "", nil
	}
	return forgejoSHA, radicleSHA, err
}

// SetGitCursor records the commit SHAs last observed on each side.
func (s *Store) SetGitCursor(repoPair, forgejoSHA, radicleSHA string) error {
	_, err := s.db.Exec(`
INSERT INTO git_cursor (repo_pair, forgejo_sha, radicle_sha) VALUES (?, ?, ?)
ON CONFLICT (repo_pair) DO UPDATE SET forgejo_sha = excluded.forgejo_sha, radicle_sha = excluded.radicle_sha
`, repoPair, forgejoSHA, radicleSHA)
	return err
}

// ItemMapping records the correspondence between a Forgejo issue/PR and its
// mirrored Radicle issue/patch, plus a content hash used to detect edits
// that need re-syncing.
type ItemMapping struct {
	Kind        string // "issue" or "patch"
	ForgejoID   int64
	RadicleID   string
	ContentHash string
}

// FindByForgejoID looks up an existing mapping created for a Forgejo item.
func (s *Store) FindByForgejoID(repoPair, kind string, forgejoID int64) (*ItemMapping, error) {
	row := s.db.QueryRow(`
SELECT radicle_id, content_hash FROM item_mapping
WHERE repo_pair = ? AND kind = ? AND forgejo_id = ?
`, repoPair, kind, forgejoID)
	m := &ItemMapping{Kind: kind, ForgejoID: forgejoID}
	err := row.Scan(&m.RadicleID, &m.ContentHash)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return m, err
}

// FindByRadicleID looks up an existing mapping created for a Radicle item.
func (s *Store) FindByRadicleID(repoPair, kind, radicleID string) (*ItemMapping, error) {
	row := s.db.QueryRow(`
SELECT forgejo_id, content_hash FROM item_mapping
WHERE repo_pair = ? AND kind = ? AND radicle_id = ?
`, repoPair, kind, radicleID)
	m := &ItemMapping{Kind: kind, RadicleID: radicleID}
	err := row.Scan(&m.ForgejoID, &m.ContentHash)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return m, err
}

// Upsert records or updates a mapping between a Forgejo item and its
// Radicle counterpart.
func (s *Store) Upsert(repoPair string, m ItemMapping) error {
	_, err := s.db.Exec(`
INSERT INTO item_mapping (repo_pair, kind, forgejo_id, radicle_id, content_hash, synced_at)
VALUES (?, ?, ?, ?, ?, datetime('now'))
ON CONFLICT (repo_pair, kind, forgejo_id, radicle_id)
DO UPDATE SET content_hash = excluded.content_hash, synced_at = excluded.synced_at
`, repoPair, m.Kind, m.ForgejoID, m.RadicleID, m.ContentHash)
	return err
}
