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

-- One-time secret shares: an admin (graft's own, or a peer's) encrypts a
-- secret (typically a Forgejo token) under a key derived from a 6-digit
-- passcode plus the high-entropy id itself. Only id_hash (never the id in
-- reversible form) is stored, so a database dump alone can never be used
-- to derive the decryption key. Deleted the moment it's successfully
-- claimed, or after too many wrong passcode attempts, or past expires_at —
-- whichever comes first.
CREATE TABLE IF NOT EXISTS token_share (
	id_hash    TEXT PRIMARY KEY,
	nonce      TEXT NOT NULL,
	ciphertext TEXT NOT NULL,
	attempts   INTEGER NOT NULL DEFAULT 0,
	created_at TEXT NOT NULL,
	expires_at TEXT NOT NULL
);

-- A repo pair added through the dashboard's self-service "add a peer" /
-- "start a new repo" forms, gated by the shared admin password rather
-- than a config.yaml edit + restart. forgejo_token_file (not the token
-- itself) follows the same convention as every statically-configured
-- pair: graft writes the submitted token out to its own chmod-600 file
-- under the same directory config.yaml's token_file paths live in, and
-- only the path is kept here. materialized flips to 1 once the running
-- daemon has turned this row into a live RepoSyncer — it happens on the
-- next sync pass, never mid-request, so a submission is never trusted
-- before graft itself has validated it end to end.
CREATE TABLE IF NOT EXISTS dynamic_repo (
	id                    INTEGER PRIMARY KEY AUTOINCREMENT,
	name                  TEXT NOT NULL UNIQUE,
	series                TEXT NOT NULL,
	forgejo_base_url      TEXT NOT NULL,
	forgejo_owner         TEXT NOT NULL,
	forgejo_repo          TEXT NOT NULL,
	forgejo_token_file    TEXT NOT NULL,
	radicle_rid           TEXT NOT NULL,
	radicle_http_base_url TEXT NOT NULL,
	radicle_explorer_url  TEXT NOT NULL DEFAULT '',
	radicle_rad_home      TEXT NOT NULL,
	sync_git              INTEGER NOT NULL DEFAULT 1,
	sync_issues           INTEGER NOT NULL DEFAULT 1,
	sync_patches          INTEGER NOT NULL DEFAULT 1,
	created_at            TEXT NOT NULL,
	materialized          INTEGER NOT NULL DEFAULT 0
);

-- Dedupes comment mirroring across Forgejo and Radicle: a comment body's
-- hash, once mirrored (or recognized as already present on both sides),
-- is recorded here so a later pass never re-posts it back the other way.
-- item_id is the Radicle-side issue/patch id, stable per repo_pair+kind
-- regardless of which Forgejo peer in a federation is being scanned.
CREATE TABLE IF NOT EXISTS comment_seen (
	repo_pair TEXT NOT NULL,
	kind      TEXT NOT NULL, -- 'issue' or 'patch'
	item_id   TEXT NOT NULL, -- radicle issue/patch id
	hash      TEXT NOT NULL,
	PRIMARY KEY (repo_pair, kind, item_id, hash)
);

-- One row per Bluesky post graft has made, so an inbound reply's parent
-- URI can be traced back to the activity_log entry (and from there, the
-- underlying Forgejo/Radicle item) it was posted about.
CREATE TABLE IF NOT EXISTS atproto_post (
	uri         TEXT PRIMARY KEY,
	activity_id INTEGER NOT NULL,
	series      TEXT NOT NULL,
	created_at  TEXT NOT NULL
);

-- How far AT Proto notification polling has gotten for each series'
-- Bluesky account, so a restart doesn't re-process old replies.
CREATE TABLE IF NOT EXISTS atproto_cursor (
	series TEXT PRIMARY KEY,
	cursor TEXT NOT NULL DEFAULT ''
);

-- A submitted "add a Radicle peer" request awaiting admin approval.
-- Unlike dynamic_repo this never becomes an ongoing sync pair — approval
-- just runs rad node connect + rad seed once, then the row is gone.
CREATE TABLE IF NOT EXISTS pending_radicle_peer (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	series     TEXT NOT NULL,
	node_id    TEXT NOT NULL,
	address    TEXT NOT NULL,
	created_at TEXT NOT NULL
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
		`ALTER TABLE activity_log ADD COLUMN origin TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE dynamic_repo ADD COLUMN approved INTEGER NOT NULL DEFAULT 0`,
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
	// Origin names where a "comment" kind entry actually happened —
	// "forgejo", "radicle", "activitypub", or "atproto". Empty for every
	// other kind. Display-only: shown in the dashboard tooltip, never
	// drives a cell's color (see status.event.Origin).
	Origin string
}

// Direction values for Activity.Direction, kept as constants so callers
// can't typo a value the status page's rendering silently fails to
// recognize.
const (
	ForgejoToRadicle = "forgejo_to_radicle"
	RadicleToForgejo = "radicle_to_forgejo"
)

// LogActivity records one Activity and returns its new row id — used by
// the Bluesky bridge to link a posted note back to what it was about, so
// a later reply can be traced to the right item. Call it once per
// commit/issue/patch/comment, not once per sync pass.
func (s *Store) LogActivity(a Activity) (int64, error) {
	res, err := s.db.Exec(`
INSERT INTO activity_log (repo_pair, kind, direction, summary, url, series, series_url, forgejo_id, radicle_id, origin, occurred_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
`, a.RepoPair, a.Kind, a.Direction, a.Summary, a.URL, a.Series, a.SeriesURL, a.ForgejoID, a.RadicleID, a.Origin, time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
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
SELECT id, repo_pair, kind, direction, summary, url, series, series_url, forgejo_id, radicle_id, origin, occurred_at FROM activity_log
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
SELECT id, repo_pair, kind, direction, summary, url, series, series_url, forgejo_id, radicle_id, origin, occurred_at FROM activity_log
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
SELECT id, repo_pair, kind, direction, summary, url, series, series_url, forgejo_id, radicle_id, origin, occurred_at FROM activity_log
WHERE id = ?
`, id)
	var e ActivityEntry
	var occurredAt string
	err := row.Scan(&e.ID, &e.RepoPair, &e.Kind, &e.Direction, &e.Summary, &e.URL, &e.Series, &e.SeriesURL, &e.ForgejoID, &e.RadicleID, &e.Origin, &occurredAt)
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
		if err := rows.Scan(&e.ID, &e.RepoPair, &e.Kind, &e.Direction, &e.Summary, &e.URL, &e.Series, &e.SeriesURL, &e.ForgejoID, &e.RadicleID, &e.Origin, &occurredAt); err != nil {
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
SELECT id, repo_pair, kind, direction, summary, url, series, series_url, forgejo_id, radicle_id, origin, occurred_at FROM activity_log
WHERE series = ? AND id > ?
ORDER BY id ASC
`, series, afterID)
	if err != nil {
		return nil, err
	}
	return scanActivityRows(rows)
}

// TokenShare is one encrypted one-time secret. Nonce and Ciphertext are
// base64-encoded AES-256-GCM output; the decryption key is derived from
// the passcode the claimant supplies plus the share's own id (which is
// never stored — only IDHash is), so a database dump alone never yields
// enough to decrypt.
type TokenShare struct {
	IDHash     string
	Nonce      string
	Ciphertext string
	Attempts   int
	ExpiresAt  time.Time
}

// CreateTokenShare stores a new encrypted share, indexed by the hash of
// its id (the plaintext id is only ever known to whoever holds the URL).
func (s *Store) CreateTokenShare(idHash, nonce, ciphertext string, expiresAt time.Time) error {
	_, err := s.db.Exec(`
INSERT INTO token_share (id_hash, nonce, ciphertext, attempts, created_at, expires_at)
VALUES (?, ?, ?, 0, ?, ?)
`, idHash, nonce, ciphertext, time.Now().UTC().Format(time.RFC3339), expiresAt.UTC().Format(time.RFC3339))
	return err
}

// GetTokenShare looks up a share by id hash. Returns nil, nil if it
// doesn't exist or has already expired (expiry is checked here rather
// than left to a background sweep, so a stale row can never be claimed
// even if cleanup hasn't run yet).
func (s *Store) GetTokenShare(idHash string) (*TokenShare, error) {
	row := s.db.QueryRow(`SELECT id_hash, nonce, ciphertext, attempts, expires_at FROM token_share WHERE id_hash = ?`, idHash)
	var t TokenShare
	var expiresAt string
	if err := row.Scan(&t.IDHash, &t.Nonce, &t.Ciphertext, &t.Attempts, &expiresAt); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	exp, err := time.Parse(time.RFC3339, expiresAt)
	if err != nil {
		return nil, err
	}
	t.ExpiresAt = exp
	if time.Now().UTC().After(exp) {
		_ = s.DeleteTokenShare(idHash)
		return nil, nil
	}
	return &t, nil
}

// IncrementTokenShareAttempts records one failed passcode attempt and
// returns the new total, so the caller can destroy the share once too
// many wrong guesses have been made — the actual brute-force defense,
// since a 6-digit passcode alone is far too small a space to rely on
// key-derivation cost against a determined online attacker.
func (s *Store) IncrementTokenShareAttempts(idHash string) (int, error) {
	_, err := s.db.Exec(`UPDATE token_share SET attempts = attempts + 1 WHERE id_hash = ?`, idHash)
	if err != nil {
		return 0, err
	}
	row := s.db.QueryRow(`SELECT attempts FROM token_share WHERE id_hash = ?`, idHash)
	var n int
	if err := row.Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// DeleteTokenShare destroys a share — called on a successful claim (it's
// one-time by design), on exceeding the attempt limit (fail closed), or
// on expiry.
func (s *Store) DeleteTokenShare(idHash string) error {
	_, err := s.db.Exec(`DELETE FROM token_share WHERE id_hash = ?`, idHash)
	return err
}

// DynamicRepo is one repo pair added through the dashboard's self-service
// onboarding forms rather than a config.yaml edit.
type DynamicRepo struct {
	ID                 int64
	Name               string
	Series             string
	ForgejoBaseURL     string
	ForgejoOwner       string
	ForgejoRepo        string
	ForgejoTokenFile   string
	RadicleRID         string
	RadicleHTTPBaseURL string
	RadicleExplorerURL string
	RadicleRadHome     string
	SyncGit            bool
	SyncIssues         bool
	SyncPatches        bool
	Materialized       bool
	Approved           bool
}

// CreateDynamicRepo inserts a newly-submitted repo pair as an unapproved
// request — it never joins the sync loop until ApproveDynamicRepo is
// called. Fails if the name is already taken — the same uniqueness
// graft would need across config.yaml pairs anyway.
func (s *Store) CreateDynamicRepo(r DynamicRepo) (int64, error) {
	res, err := s.db.Exec(`
INSERT INTO dynamic_repo (
	name, series, forgejo_base_url, forgejo_owner, forgejo_repo, forgejo_token_file,
	radicle_rid, radicle_http_base_url, radicle_explorer_url, radicle_rad_home,
	sync_git, sync_issues, sync_patches, created_at, materialized, approved
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, 0)
`, r.Name, r.Series, r.ForgejoBaseURL, r.ForgejoOwner, r.ForgejoRepo, r.ForgejoTokenFile,
		r.RadicleRID, r.RadicleHTTPBaseURL, r.RadicleExplorerURL, r.RadicleRadHome,
		boolToInt(r.SyncGit), boolToInt(r.SyncIssues), boolToInt(r.SyncPatches), time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

const dynamicRepoColumns = `id, name, series, forgejo_base_url, forgejo_owner, forgejo_repo, forgejo_token_file,
	radicle_rid, radicle_http_base_url, radicle_explorer_url, radicle_rad_home,
	sync_git, sync_issues, sync_patches, materialized, approved`

func scanDynamicRepoRows(rows *sql.Rows) ([]DynamicRepo, error) {
	defer rows.Close()
	var out []DynamicRepo
	for rows.Next() {
		var r DynamicRepo
		var g, i, p, m, a int
		if err := rows.Scan(&r.ID, &r.Name, &r.Series, &r.ForgejoBaseURL, &r.ForgejoOwner, &r.ForgejoRepo, &r.ForgejoTokenFile,
			&r.RadicleRID, &r.RadicleHTTPBaseURL, &r.RadicleExplorerURL, &r.RadicleRadHome, &g, &i, &p, &m, &a); err != nil {
			return nil, err
		}
		r.SyncGit, r.SyncIssues, r.SyncPatches, r.Materialized, r.Approved = g != 0, i != 0, p != 0, m != 0, a != 0
		out = append(out, r)
	}
	return out, rows.Err()
}

// PendingDynamicRepos returns every submitted peer/repo request still
// awaiting admin approval.
func (s *Store) PendingDynamicRepos() ([]DynamicRepo, error) {
	rows, err := s.db.Query(`SELECT ` + dynamicRepoColumns + ` FROM dynamic_repo WHERE approved = 0`)
	if err != nil {
		return nil, err
	}
	return scanDynamicRepoRows(rows)
}

// GetDynamicRepo looks up one row by id, e.g. to recover its token file
// path before deleting a rejected request.
func (s *Store) GetDynamicRepo(id int64) (*DynamicRepo, error) {
	rows, err := s.db.Query(`SELECT `+dynamicRepoColumns+` FROM dynamic_repo WHERE id = ?`, id)
	if err != nil {
		return nil, err
	}
	out, err := scanDynamicRepoRows(rows)
	if err != nil || len(out) == 0 {
		return nil, err
	}
	return &out[0], nil
}

// ApproveDynamicRepo marks a pending request approved — the next sync
// pass turns it into a live RepoSyncer (see UnmaterializedDynamicRepos).
func (s *Store) ApproveDynamicRepo(id int64) error {
	_, err := s.db.Exec(`UPDATE dynamic_repo SET approved = 1 WHERE id = ?`, id)
	return err
}

// RejectDynamicRepo permanently removes a pending request.
func (s *Store) RejectDynamicRepo(id int64) error {
	_, err := s.db.Exec(`DELETE FROM dynamic_repo WHERE id = ?`, id)
	return err
}

// UnmaterializedDynamicRepos returns every approved dynamic_repo row the
// running daemon hasn't yet turned into a live RepoSyncer.
func (s *Store) UnmaterializedDynamicRepos() ([]DynamicRepo, error) {
	rows, err := s.db.Query(`SELECT ` + dynamicRepoColumns + ` FROM dynamic_repo WHERE approved = 1 AND materialized = 0`)
	if err != nil {
		return nil, err
	}
	return scanDynamicRepoRows(rows)
}

// MarkDynamicRepoMaterialized flips a row so it's never re-registered on
// a later pass.
func (s *Store) MarkDynamicRepoMaterialized(id int64) error {
	_, err := s.db.Exec(`UPDATE dynamic_repo SET materialized = 1 WHERE id = ?`, id)
	return err
}

// AllDynamicRepos returns every approved dynamic_repo row regardless of
// materialization state — used at startup to rebuild the full set of
// live pairs after a restart, since these rows persist across it but
// config.yaml doesn't know about them. Never includes a row still
// awaiting approval.
func (s *Store) AllDynamicRepos() ([]DynamicRepo, error) {
	rows, err := s.db.Query(`SELECT ` + dynamicRepoColumns + ` FROM dynamic_repo WHERE approved = 1`)
	if err != nil {
		return nil, err
	}
	return scanDynamicRepoRows(rows)
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// CommentSeen reports whether a comment body's hash has already been
// dealt with (mirrored, or recognized as already present on both sides)
// for one item, so a comment sync pass never re-posts the same content
// back and forth between Forgejo and Radicle.
func (s *Store) CommentSeen(repoPair, kind, itemID, hash string) (bool, error) {
	row := s.db.QueryRow(`SELECT 1 FROM comment_seen WHERE repo_pair = ? AND kind = ? AND item_id = ? AND hash = ?`, repoPair, kind, itemID, hash)
	var one int
	err := row.Scan(&one)
	if err == sql.ErrNoRows {
		return false, nil
	}
	return err == nil, err
}

// MarkCommentSeen records a comment body's hash as dealt with.
func (s *Store) MarkCommentSeen(repoPair, kind, itemID, hash string) error {
	_, err := s.db.Exec(`INSERT OR IGNORE INTO comment_seen (repo_pair, kind, item_id, hash) VALUES (?, ?, ?, ?)`, repoPair, kind, itemID, hash)
	return err
}

// SaveATProtoPost records a Bluesky post graft made, so a later reply's
// parent URI can be traced back to the activity_log entry it was about.
func (s *Store) SaveATProtoPost(uri string, activityID int64, series string) error {
	_, err := s.db.Exec(`INSERT OR IGNORE INTO atproto_post (uri, activity_id, series, created_at) VALUES (?, ?, ?, ?)`,
		uri, activityID, series, time.Now().UTC().Format(time.RFC3339))
	return err
}

// ATProtoPostActivity looks up which activity_log entry a Bluesky post
// URI was about, for resolving an inbound reply back to the underlying
// Forgejo/Radicle item.
func (s *Store) ATProtoPostActivity(uri string) (int64, bool, error) {
	row := s.db.QueryRow(`SELECT activity_id FROM atproto_post WHERE uri = ?`, uri)
	var id int64
	err := row.Scan(&id)
	if err == sql.ErrNoRows {
		return 0, false, nil
	}
	return id, err == nil, err
}

// ATProtoCursor returns how far notification polling has gotten for a
// series' Bluesky account ("" if never polled).
func (s *Store) ATProtoCursor(series string) (string, error) {
	row := s.db.QueryRow(`SELECT cursor FROM atproto_cursor WHERE series = ?`, series)
	var c string
	err := row.Scan(&c)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return c, err
}

// SetATProtoCursor records how far notification polling has gotten.
func (s *Store) SetATProtoCursor(series, cursor string) error {
	_, err := s.db.Exec(`
INSERT INTO atproto_cursor (series, cursor) VALUES (?, ?)
ON CONFLICT (series) DO UPDATE SET cursor = excluded.cursor
`, series, cursor)
	return err
}

// PendingRadiclePeer is one submitted "add a Radicle peer" request
// awaiting admin approval.
type PendingRadiclePeer struct {
	ID      int64
	Series  string
	NodeID  string
	Address string
}

// CreatePendingRadiclePeer records a submitted request.
func (s *Store) CreatePendingRadiclePeer(series, nodeID, address string) (int64, error) {
	res, err := s.db.Exec(`INSERT INTO pending_radicle_peer (series, node_id, address, created_at) VALUES (?, ?, ?, ?)`,
		series, nodeID, address, time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// ListPendingRadiclePeers returns every request awaiting approval.
func (s *Store) ListPendingRadiclePeers() ([]PendingRadiclePeer, error) {
	rows, err := s.db.Query(`SELECT id, series, node_id, address FROM pending_radicle_peer`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PendingRadiclePeer
	for rows.Next() {
		var p PendingRadiclePeer
		if err := rows.Scan(&p.ID, &p.Series, &p.NodeID, &p.Address); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// GetPendingRadiclePeer looks up one request by id.
func (s *Store) GetPendingRadiclePeer(id int64) (*PendingRadiclePeer, error) {
	row := s.db.QueryRow(`SELECT id, series, node_id, address FROM pending_radicle_peer WHERE id = ?`, id)
	var p PendingRadiclePeer
	err := row.Scan(&p.ID, &p.Series, &p.NodeID, &p.Address)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// DeletePendingRadiclePeer removes a request — called after it's been
// approved and acted on, or rejected.
func (s *Store) DeletePendingRadiclePeer(id int64) error {
	_, err := s.db.Exec(`DELETE FROM pending_radicle_peer WHERE id = ?`, id)
	return err
}
