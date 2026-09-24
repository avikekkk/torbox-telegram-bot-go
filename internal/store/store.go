// Package store persists bot state in SQLite so it survives restarts.
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	// Pure-Go SQLite driver, so the bot builds without cgo.
	_ "modernc.org/sqlite"
)

// Store holds authorized user and chat IDs, and the downloads waiting to be
// posted to the download channel.
type Store struct {
	db *sql.DB
}

// Entry is one authorized ID.
type Entry struct {
	ID      int64
	AddedBy int64
	AddedAt time.Time
}

// Job is a download waiting for its channel post.
type Job struct {
	OwnerID int64
	Kind    string
	ItemID  int64
	Name    string
}

// Channel publication outcomes.
const (
	JobSuccess = "success"
	JobFailed  = "failed"
)

// The channel table keeps the Python bot's layout, so DATABASE_PATH can point
// at its torbot.db and pick up the jobs it left pending.
const schema = `
CREATE TABLE IF NOT EXISTS authorized_ids (
	id       INTEGER PRIMARY KEY,
	added_by INTEGER NOT NULL,
	added_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS channel_publish_jobs (
	owner_id     INTEGER NOT NULL,
	kind         TEXT NOT NULL,
	item_id      INTEGER NOT NULL,
	name         TEXT NOT NULL,
	status       TEXT NOT NULL DEFAULT 'pending',
	created_at   TEXT DEFAULT (datetime('now')),
	published_at TEXT,
	PRIMARY KEY (owner_id, kind, item_id)
);`

// Open opens (and if needed creates) the database at path.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open database %s: %w", path, err)
	}
	// One connection: SQLite serialises writers anyway, and a second pooled
	// connection only turns a concurrent read into "database is locked".
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("create schema in %s: %w", path, err)
	}
	return &Store{db: db}, nil
}

// Close releases the database handle.
func (s *Store) Close() error { return s.db.Close() }

// IsAuthorized reports whether any of the given IDs is authorized. Passing both
// a user and a chat ID authorizes the command if either was granted access.
func (s *Store) IsAuthorized(ids ...int64) (bool, error) {
	for _, id := range ids {
		if id == 0 {
			continue
		}
		var exists int
		err := s.db.QueryRow("SELECT 1 FROM authorized_ids WHERE id = ?", id).Scan(&exists)
		if err == nil {
			return true, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return false, fmt.Errorf("look up authorization: %w", err)
		}
	}
	return false, nil
}

// Authorize grants access to an ID. It reports false when the ID was already
// authorized, so callers can tell the user nothing changed.
func (s *Store) Authorize(id, addedBy int64) (bool, error) {
	result, err := s.db.Exec(
		"INSERT OR IGNORE INTO authorized_ids (id, added_by, added_at) VALUES (?, ?, ?)",
		id, addedBy, time.Now().Unix(),
	)
	if err != nil {
		return false, fmt.Errorf("authorize %d: %w", id, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("authorize %d: %w", id, err)
	}
	return affected > 0, nil
}

// Revoke removes access. It reports false when the ID was not authorized.
func (s *Store) Revoke(id int64) (bool, error) {
	result, err := s.db.Exec("DELETE FROM authorized_ids WHERE id = ?", id)
	if err != nil {
		return false, fmt.Errorf("revoke %d: %w", id, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("revoke %d: %w", id, err)
	}
	return affected > 0, nil
}

// EnqueueJob records a download for the channel. It reports false when the
// download was already queued or published, so it is never posted twice.
func (s *Store) EnqueueJob(job Job) (bool, error) {
	if job.Name == "" {
		job.Name = "Unknown"
	}
	result, err := s.db.Exec(
		"INSERT OR IGNORE INTO channel_publish_jobs (owner_id, kind, item_id, name) VALUES (?, ?, ?, ?)",
		job.OwnerID, job.Kind, job.ItemID, job.Name,
	)
	if err != nil {
		return false, fmt.Errorf("queue %s %d: %w", job.Kind, job.ItemID, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("queue %s %d: %w", job.Kind, job.ItemID, err)
	}
	return affected > 0, nil
}

// PendingJobs returns every download not yet posted, oldest first.
func (s *Store) PendingJobs() ([]Job, error) {
	rows, err := s.db.Query(
		"SELECT owner_id, kind, item_id, name FROM channel_publish_jobs WHERE status = 'pending' ORDER BY created_at")
	if err != nil {
		return nil, fmt.Errorf("list pending jobs: %w", err)
	}
	defer rows.Close()

	var jobs []Job
	for rows.Next() {
		var job Job
		if err := rows.Scan(&job.OwnerID, &job.Kind, &job.ItemID, &job.Name); err != nil {
			return nil, fmt.Errorf("list pending jobs: %w", err)
		}
		jobs = append(jobs, job)
	}
	return jobs, rows.Err()
}

// FinishJob marks a download as posted, with JobSuccess or JobFailed.
func (s *Store) FinishJob(job Job, status string) error {
	if status != JobSuccess && status != JobFailed {
		return fmt.Errorf("channel publication status must be %s or %s", JobSuccess, JobFailed)
	}
	_, err := s.db.Exec(
		`UPDATE channel_publish_jobs SET status = ?, published_at = datetime('now')
		 WHERE owner_id = ? AND kind = ? AND item_id = ?`,
		status, job.OwnerID, job.Kind, job.ItemID,
	)
	if err != nil {
		return fmt.Errorf("finish %s %d: %w", job.Kind, job.ItemID, err)
	}
	return nil
}
