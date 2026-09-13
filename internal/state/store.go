// Package state persists sync bookkeeping so the daemon can tell, across
// restarts, what has already been mirrored and avoid duplicating issues,
// patches or commits on every poll.
package state

import (
	"database/sql"
	"fmt"
	"os"
	"strings"
	"time"

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
	// This database holds every series' ActivityPub private key
	// (ap_actor.private_key) in plaintext — it must never be
	// group/world-readable, regardless of the umask the daemon happened to
	// start with. Best-effort: sqlite may not have created the file yet on
	// the very first Open before migrate runs, but by this point it has.
	if err := os.Chmod(path, 0o600); err != nil {
		return nil, fmt.Errorf("restrict state db permissions: %w", err)
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

-- One row per thing actually mirrored: a commit, an issue, a patch. This is
-- what the status page's activity calendar and commit list read from — it's
-- an append-only log, never updated, so it can't itself be a source of the
-- kind of state-loss bug that hit item_mapping.
CREATE TABLE IF NOT EXISTS activity_log (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	repo_pair   TEXT NOT NULL,
	kind        TEXT NOT NULL, -- 'git', 'issue', 'patch'
	direction   TEXT NOT NULL, -- 'forgejo_to_radicle' or 'radicle_to_forgejo'
	summary     TEXT NOT NULL,
	url         TEXT NOT NULL DEFAULT '',
	series      TEXT NOT NULL DEFAULT '',
	series_url  TEXT NOT NULL DEFAULT '',
	forgejo_id  INTEGER NOT NULL DEFAULT 0,
	radicle_id  TEXT NOT NULL DEFAULT '',
	occurred_at TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_activity_log_occurred_at
	ON activity_log (occurred_at);

-- One row per series' ActivityPub actor: its RSA keypair, generated once
-- lazily the first time that series is served over ActivityPub.
CREATE TABLE IF NOT EXISTS ap_actor (
	series      TEXT PRIMARY KEY,
	private_key TEXT NOT NULL,
	public_key  TEXT NOT NULL,
	created_at  TEXT NOT NULL
);

-- One row per remote actor following a series' ActivityPub actor.
CREATE TABLE IF NOT EXISTS ap_follower (
	series    TEXT NOT NULL,
	actor_uri TEXT NOT NULL,
	inbox_uri TEXT NOT NULL,
	PRIMARY KEY (series, actor_uri)
);

-- How far delivery has gotten through activity_log for each series, so a
-- restart doesn't redeliver everything to every follower.
CREATE TABLE IF NOT EXISTS ap_delivery_cursor (
	series            TEXT PRIMARY KEY,
	last_delivered_id INTEGER NOT NULL DEFAULT 0
);
`)
	if err != nil {
		return err
	}
	// These columns were added after the table already shipped once; each
	// ALTER is a no-op (ignored) on a fresh db where the CREATE above
	// already included it.
	for _, stmt := range []string{
		`ALTER TABLE activity_log ADD COLUMN url TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE activity_log ADD COLUMN series TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE activity_log ADD COLUMN series_url TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE activity_log ADD COLUMN forgejo_id INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE activity_log ADD COLUMN radicle_id TEXT NOT NULL DEFAULT ''`,
	} {
		if _, err := db.Exec(stmt); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			return err
		}
	}
	return nil
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

// Activity is one thing that was actually mirrored, for the status page.
type Activity struct {
	RepoPair string // the sync pair that did it, e.g. "graft-test-v2-f1"
	Series   string // the human-facing repo group this pair belongs to,
	// e.g. "graft-test-v2" — several pairs (one per forge/Radicle side)
	// can share a series so the dashboard shows one row per repo, not
	// one per pair.
	SeriesURL string // where clicking the series' own name should go
	Kind      string // "git", "issue", "patch"
	Direction string // ForgejoToRadicle or RadicleToForgejo
	Summary   string
	URL       string // where to send someone who clicks this entry — the
	// forge or Radicle page for the thing that now exists because of
	// this sync; "" if none is known.
	ForgejoID int64  // issue/PR number on Forgejo, for kind "issue"/"patch"; 0 for "git"
	RadicleID string // issue/patch id on Radicle, for kind "issue"/"patch"; "" for "git"
}

// Direction values for Activity.Direction, kept as constants so callers
// can't typo a value the status page's rendering silently fails to
// recognize.
const (
	ForgejoToRadicle = "forgejo_to_radicle"
	RadicleToForgejo = "radicle_to_forgejo"
)

// LogActivity records one Activity. Call it once per commit/issue/patch,
// not once per sync pass.
func (s *Store) LogActivity(a Activity) error {
	_, err := s.db.Exec(`
INSERT INTO activity_log (repo_pair, kind, direction, summary, url, series, series_url, forgejo_id, radicle_id, occurred_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
`, a.RepoPair, a.Kind, a.Direction, a.Summary, a.URL, a.Series, a.SeriesURL, a.ForgejoID, a.RadicleID, time.Now().UTC().Format(time.RFC3339))
	return err
}

// ActivityEntry is one row of activity_log.
type ActivityEntry struct {
	ID int64 // activity_log.id, used as the stable suffix of ActivityPub Note URIs
	Activity
	OccurredAt time.Time
}

// ActivitySince returns every activity entry at or after since, newest first.
func (s *Store) ActivitySince(since time.Time) ([]ActivityEntry, error) {
	rows, err := s.db.Query(`
SELECT id, repo_pair, kind, direction, summary, url, series, series_url, forgejo_id, radicle_id, occurred_at FROM activity_log
WHERE occurred_at >= ?
ORDER BY occurred_at DESC
`, since.UTC().Format(time.RFC3339))
	if err != nil {
		return nil, err
	}
	return scanActivityRows(rows)
}

// ActivityForSeries returns the most recent activity entries for one
// series, newest first, capped at limit — used to build that series'
// ActivityPub outbox.
func (s *Store) ActivityForSeries(series string, limit int) ([]ActivityEntry, error) {
	rows, err := s.db.Query(`
SELECT id, repo_pair, kind, direction, summary, url, series, series_url, forgejo_id, radicle_id, occurred_at FROM activity_log
WHERE series = ?
ORDER BY occurred_at DESC
LIMIT ?
`, series, limit)
	if err != nil {
		return nil, err
	}
	return scanActivityRows(rows)
}

// ActivityByID looks up one activity_log row by id, e.g. to resolve an
// ActivityPub Note URI (/actors/{series}/notes/{id}) back to the mirrored
// item it describes.
func (s *Store) ActivityByID(id int64) (*ActivityEntry, error) {
	row := s.db.QueryRow(`
SELECT id, repo_pair, kind, direction, summary, url, series, series_url, forgejo_id, radicle_id, occurred_at FROM activity_log
WHERE id = ?
`, id)
	var e ActivityEntry
	var occurredAt string
	err := row.Scan(&e.ID, &e.RepoPair, &e.Kind, &e.Direction, &e.Summary, &e.URL, &e.Series, &e.SeriesURL, &e.ForgejoID, &e.RadicleID, &occurredAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	e.OccurredAt, err = time.Parse(time.RFC3339, occurredAt)
	if err != nil {
		return nil, fmt.Errorf("parse occurred_at %q: %w", occurredAt, err)
	}
	return &e, nil
}

func scanActivityRows(rows *sql.Rows) ([]ActivityEntry, error) {
	defer rows.Close()
	var out []ActivityEntry
	for rows.Next() {
		var e ActivityEntry
		var occurredAt string
		if err := rows.Scan(&e.ID, &e.RepoPair, &e.Kind, &e.Direction, &e.Summary, &e.URL, &e.Series, &e.SeriesURL, &e.ForgejoID, &e.RadicleID, &occurredAt); err != nil {
			return nil, err
		}
		var err error
		e.OccurredAt, err = time.Parse(time.RFC3339, occurredAt)
		if err != nil {
			return nil, fmt.Errorf("parse occurred_at %q: %w", occurredAt, err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ActorKeys is one series' ActivityPub actor keypair, PEM-encoded.
type ActorKeys struct {
	PrivateKeyPEM string
	PublicKeyPEM  string
}

// ActorKeys returns the stored keypair for a series, or nil if none has
// been generated yet.
func (s *Store) ActorKeys(series string) (*ActorKeys, error) {
	row := s.db.QueryRow(`SELECT private_key, public_key FROM ap_actor WHERE series = ?`, series)
	var k ActorKeys
	err := row.Scan(&k.PrivateKeyPEM, &k.PublicKeyPEM)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &k, nil
}

// SaveActorKeys stores a newly generated keypair for a series. Safe to call
// concurrently: if another goroutine already inserted one, this becomes a
// no-op via INSERT OR IGNORE, and the caller should re-read with
// ActorKeys to get whichever copy won.
func (s *Store) SaveActorKeys(series string, k ActorKeys) error {
	_, err := s.db.Exec(`
INSERT OR IGNORE INTO ap_actor (series, private_key, public_key, created_at)
VALUES (?, ?, ?, ?)
`, series, k.PrivateKeyPEM, k.PublicKeyPEM, time.Now().UTC().Format(time.RFC3339))
	return err
}

// Follower is one remote actor following a series' ActivityPub actor.
type Follower struct {
	ActorURI string
	InboxURI string
}

// SaveFollower records (or updates the inbox URL of) a follower.
func (s *Store) SaveFollower(series, actorURI, inboxURI string) error {
	_, err := s.db.Exec(`
INSERT INTO ap_follower (series, actor_uri, inbox_uri) VALUES (?, ?, ?)
ON CONFLICT (series, actor_uri) DO UPDATE SET inbox_uri = excluded.inbox_uri
`, series, actorURI, inboxURI)
	return err
}

// RemoveFollower drops a follower, e.g. on Undo{Follow}.
func (s *Store) RemoveFollower(series, actorURI string) error {
	_, err := s.db.Exec(`DELETE FROM ap_follower WHERE series = ? AND actor_uri = ?`, series, actorURI)
	return err
}

// Followers lists everyone following a series.
func (s *Store) Followers(series string) ([]Follower, error) {
	rows, err := s.db.Query(`SELECT actor_uri, inbox_uri FROM ap_follower WHERE series = ?`, series)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Follower
	for rows.Next() {
		var f Follower
		if err := rows.Scan(&f.ActorURI, &f.InboxURI); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// FollowerCount is len(Followers(series)) without building the slice.
func (s *Store) FollowerCount(series string) (int, error) {
	row := s.db.QueryRow(`SELECT COUNT(*) FROM ap_follower WHERE series = ?`, series)
	var n int
	err := row.Scan(&n)
	return n, err
}

// SeriesWithFollowers lists every series that has at least one follower —
// the set worth checking for new activity to deliver.
func (s *Store) SeriesWithFollowers() ([]string, error) {
	rows, err := s.db.Query(`SELECT DISTINCT series FROM ap_follower`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var series string
		if err := rows.Scan(&series); err != nil {
			return nil, err
		}
		out = append(out, series)
	}
	return out, rows.Err()
}

// DeliveryCursor returns how far ActivityPub delivery has gotten through
// activity_log for a series (0 if nothing has been delivered yet).
func (s *Store) DeliveryCursor(series string) (int64, error) {
	row := s.db.QueryRow(`SELECT last_delivered_id FROM ap_delivery_cursor WHERE series = ?`, series)
	var id int64
	err := row.Scan(&id)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	return id, err
}

// SetDeliveryCursor records how far delivery has gotten for a series.
func (s *Store) SetDeliveryCursor(series string, id int64) error {
	_, err := s.db.Exec(`
INSERT INTO ap_delivery_cursor (series, last_delivered_id) VALUES (?, ?)
ON CONFLICT (series) DO UPDATE SET last_delivered_id = excluded.last_delivered_id
`, series, id)
	return err
}

// NewActivityForSeries returns activity_log entries for a series with
// id > afterID, oldest first — the delivery order for ActivityPub.
func (s *Store) NewActivityForSeries(series string, afterID int64) ([]ActivityEntry, error) {
	rows, err := s.db.Query(`
SELECT id, repo_pair, kind, direction, summary, url, series, series_url, forgejo_id, radicle_id, occurred_at FROM activity_log
WHERE series = ? AND id > ?
ORDER BY id ASC
`, series, afterID)
	if err != nil {
		return nil, err
	}
	return scanActivityRows(rows)
}
