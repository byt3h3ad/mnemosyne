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
    synced_back     INTEGER NOT NULL DEFAULT 0,
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
	return &DB{conn: conn}, nil
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
// If already archived, leaves it alone.
// If failed_transient, resets to pending for retry.
func (d *DB) UpsertPending(raindropID int64, originalURL string) error {
	_, err := d.conn.Exec(`
		INSERT INTO archived_bookmarks (raindrop_id, original_url, status)
		VALUES (?, ?, 'pending')
		ON CONFLICT(raindrop_id) DO UPDATE SET
			status = CASE
				WHEN excluded.status = 'pending' AND archived_bookmarks.status = 'failed_transient'
				THEN 'pending'
				ELSE archived_bookmarks.status
			END
	`, raindropID, originalURL)
	return err
}

// ResetTransient resets all failed_transient rows back to pending.
func (d *DB) ResetTransient() error {
	_, err := d.conn.Exec(`UPDATE archived_bookmarks SET status = 'pending' WHERE status = 'failed_transient'`)
	return err
}

func (d *DB) ListPending() ([]Bookmark, error) {
	rows, err := d.conn.Query(`
		SELECT raindrop_id, original_url FROM archived_bookmarks WHERE status = 'pending'
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

func (d *DB) MarkArchived(raindropID int64, archiveURL string) error {
	now := time.Now().UTC()
	_, err := d.conn.Exec(`
		UPDATE archived_bookmarks
		SET status = 'archived', archive_url = ?, archived_at = ?, error = NULL
		WHERE raindrop_id = ?
	`, archiveURL, now, raindropID)
	return err
}

func (d *DB) MarkFailed(raindropID int64, permanent bool, statusExt string) error {
	status := "failed_transient"
	if permanent {
		status = "failed_permanent"
	}
	now := time.Now().UTC()
	_, err := d.conn.Exec(`
		UPDATE archived_bookmarks
		SET status = ?, status_ext = ?, error = ?, attempted_at = ?
		WHERE raindrop_id = ?
	`, status, statusExt, statusExt, now, raindropID)
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

// Counts returns (archived, failedPermanent, failedTransient) for the summary.
func (d *DB) Counts() (archived, failedPermanent, failedTransient int, err error) {
	row := d.conn.QueryRow(`
		SELECT
			SUM(CASE WHEN status = 'archived' THEN 1 ELSE 0 END),
			SUM(CASE WHEN status = 'failed_permanent' THEN 1 ELSE 0 END),
			SUM(CASE WHEN status = 'failed_transient' THEN 1 ELSE 0 END)
		FROM archived_bookmarks
	`)
	err = row.Scan(&archived, &failedPermanent, &failedTransient)
	return
}

func (d *DB) CountSynced() (int, error) {
	var n int
	err := d.conn.QueryRow(`SELECT COUNT(*) FROM archived_bookmarks WHERE synced_back = 1`).Scan(&n)
	return n, err
}
