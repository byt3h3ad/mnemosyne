package db

import (
	"database/sql"
	"time"

	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS archived_bookmarks (
    raindrop_id     INTEGER PRIMARY KEY,
    original_url    TEXT NOT NULL,
    archive_url     TEXT,
    status          TEXT NOT NULL DEFAULT 'pending',
    status_ext      TEXT,
    synced_back     INTEGER NOT NULL DEFAULT 0,  -- 0 unsynced, 1 synced, -1 permanently unsyncable
    created_at      TIMESTAMP,
    attempted_at    TIMESTAMP,
    archived_at     TIMESTAMP,
    error           TEXT
);

CREATE TABLE IF NOT EXISTS run_state (
    key   TEXT PRIMARY KEY,
    value TEXT
);
`

type DB struct {
	conn *sql.DB
}

func Open(path string) (*DB, error) {
	conn, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	conn.SetMaxOpenConns(1) // SQLite doesn't support concurrent writes
	if _, err := conn.Exec(schema); err != nil {
		conn.Close()
		return nil, err
	}
	if err := ensureBookmarkCreatedAt(conn); err != nil {
		conn.Close()
		return nil, err
	}
	return &DB{conn: conn}, nil
}

func ensureBookmarkCreatedAt(conn *sql.DB) error {
	rows, err := conn.Query(`PRAGMA table_info(archived_bookmarks)`)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, columnType string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			return err
		}
		if name == "created_at" {
			return rows.Err()
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	_, err = conn.Exec(`ALTER TABLE archived_bookmarks ADD COLUMN created_at TIMESTAMP`)
	return err
}

func (d *DB) Close() error {
	return d.conn.Close()
}

// --- run_state ---

func (d *DB) GetState(key string) (string, error) {
	var val string
	err := d.conn.QueryRow(`SELECT value FROM run_state WHERE key = ?`, key).Scan(&val)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return val, err
}

func (d *DB) SetState(key, value string) error {
	_, err := d.conn.Exec(
		`INSERT INTO run_state (key, value) VALUES (?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		key, value,
	)
	return err
}

// --- archived_bookmarks ---

type Bookmark struct {
	RaindropID  int64
	OriginalURL string
	ArchiveURL  string
	Status      string
	StatusExt   string
	SyncedBack  bool
	AttemptedAt *time.Time
	ArchivedAt  *time.Time
	Error       string
}

// UpsertPending inserts the bookmark as pending if not present.
// If already archived, leaves its URL alone. The Raindrop creation timestamp
// is always refreshed so retries can be ordered newest first.
// For non-archived rows the URL is refreshed in case it was edited in Raindrop.
func (d *DB) UpsertPending(raindropID int64, originalURL string, createdAt time.Time) error {
	_, err := d.conn.Exec(`
		INSERT INTO archived_bookmarks (raindrop_id, original_url, created_at, status)
		VALUES (?, ?, ?, 'pending')
		ON CONFLICT(raindrop_id) DO UPDATE SET
			original_url = CASE
				WHEN archived_bookmarks.status = 'archived' THEN archived_bookmarks.original_url
				ELSE excluded.original_url
			END,
			created_at = excluded.created_at,
			status = CASE archived_bookmarks.status
				WHEN 'failed_transient' THEN 'pending'
				ELSE archived_bookmarks.status
			END
	`, raindropID, originalURL, createdAt)
	return err
}

// ResetTransient resets all failed_transient rows back to pending.
func (d *DB) ResetTransient() error {
	_, err := d.conn.Exec(`UPDATE archived_bookmarks SET status = 'pending' WHERE status = 'failed_transient'`)
	return err
}

func (d *DB) ListPending() ([]Bookmark, error) {
	return d.listByStatus("pending")
}

// ListPendingNewestFirst returns pending bookmarks by Raindrop creation time.
// Legacy entries without a stored timestamp fall back to descending bookmark ID.
func (d *DB) ListPendingNewestFirst() ([]Bookmark, error) {
	rows, err := d.conn.Query(`
		SELECT raindrop_id, original_url
		FROM archived_bookmarks
		WHERE status = 'pending'
		ORDER BY created_at IS NULL, created_at DESC, raindrop_id DESC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Bookmark
	for rows.Next() {
		var b Bookmark
		if err := rows.Scan(&b.RaindropID, &b.OriginalURL); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// ListTransient returns rows that failed transiently and would be retried
// with --retry-failed.
func (d *DB) ListTransient() ([]Bookmark, error) {
	return d.listByStatus("failed_transient")
}

func (d *DB) listByStatus(status string) ([]Bookmark, error) {
	rows, err := d.conn.Query(`
		SELECT raindrop_id, original_url FROM archived_bookmarks WHERE status = ?
	`, status)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Bookmark
	for rows.Next() {
		var b Bookmark
		if err := rows.Scan(&b.RaindropID, &b.OriginalURL); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// StatusOf returns the stored status for a bookmark, or "" if it is unknown.
func (d *DB) StatusOf(raindropID int64) (string, error) {
	var s string
	err := d.conn.QueryRow(`SELECT status FROM archived_bookmarks WHERE raindrop_id = ?`, raindropID).Scan(&s)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return s, err
}

func (d *DB) MarkArchived(raindropID int64, archiveURL string) error {
	now := time.Now().UTC()
	_, err := d.conn.Exec(`
		UPDATE archived_bookmarks
		SET status = 'archived', archive_url = ?, archived_at = ?, error = NULL
		WHERE raindrop_id = ?
	`, archiveURL, now, raindropID)
	return err
}

func (d *DB) MarkFailed(raindropID int64, permanent bool, statusExt, message string) error {
	status := "failed_transient"
	if permanent {
		status = "failed_permanent"
	}
	now := time.Now().UTC()
	_, err := d.conn.Exec(`
		UPDATE archived_bookmarks
		SET status = ?, status_ext = ?, error = ?, attempted_at = ?
		WHERE raindrop_id = ?
	`, status, statusExt, message, now, raindropID)
	return err
}

// ListUnsynced returns archived rows that haven't been written back to Raindrop yet.
func (d *DB) ListUnsynced() ([]Bookmark, error) {
	rows, err := d.conn.Query(`
		SELECT raindrop_id, original_url, archive_url
		FROM archived_bookmarks
		WHERE status = 'archived' AND synced_back = 0
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Bookmark
	for rows.Next() {
		var b Bookmark
		if err := rows.Scan(&b.RaindropID, &b.OriginalURL, &b.ArchiveURL); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

func (d *DB) MarkSynced(raindropID int64) error {
	_, err := d.conn.Exec(`UPDATE archived_bookmarks SET synced_back = 1 WHERE raindrop_id = ?`, raindropID)
	return err
}

// MarkSyncFailedPermanent excludes a bookmark from future sync-back attempts
// (e.g. it was deleted in Raindrop, or its note is full).
func (d *DB) MarkSyncFailedPermanent(raindropID int64, reason string) error {
	_, err := d.conn.Exec(
		`UPDATE archived_bookmarks SET synced_back = -1, error = ? WHERE raindrop_id = ?`,
		reason, raindropID,
	)
	return err
}

// Stats holds aggregate counts across all bookmarks for the status command.
type Stats struct {
	Pending         int
	Archived        int
	FailedPermanent int
	FailedTransient int
	SyncedBack      int
	Unsynced        int
	Unsyncable      int
}

func (d *DB) Stats() (Stats, error) {
	var s Stats
	row := d.conn.QueryRow(`
		SELECT
			COALESCE(SUM(status = 'pending'), 0),
			COALESCE(SUM(status = 'archived'), 0),
			COALESCE(SUM(status = 'failed_permanent'), 0),
			COALESCE(SUM(status = 'failed_transient'), 0),
			COALESCE(SUM(status = 'archived' AND synced_back = 1), 0),
			COALESCE(SUM(status = 'archived' AND synced_back = 0), 0),
			COALESCE(SUM(status = 'archived' AND synced_back = -1), 0)
		FROM archived_bookmarks
	`)
	err := row.Scan(
		&s.Pending, &s.Archived, &s.FailedPermanent, &s.FailedTransient,
		&s.SyncedBack, &s.Unsynced, &s.Unsyncable,
	)
	return s, err
}
