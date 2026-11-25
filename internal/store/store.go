// Package store is git-purge's local SQLite database: the last synced view of
// every branch, a cache of forge PR lookups, and a log of purged branches so
// any deletion can be undone.
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"

	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS repos (
	id        INTEGER PRIMARY KEY,
	path      TEXT NOT NULL UNIQUE,
	base      TEXT NOT NULL DEFAULT '',
	forge     TEXT NOT NULL DEFAULT '',
	last_sync INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS branches (
	repo_id      INTEGER NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
	name         TEXT NOT NULL,
	sha          TEXT NOT NULL,
	upstream     TEXT NOT NULL DEFAULT '',
	remote       TEXT NOT NULL DEFAULT '',
	merge_ref    TEXT NOT NULL DEFAULT '',
	gone         INTEGER NOT NULL DEFAULT 0,
	merged       INTEGER NOT NULL DEFAULT 0,
	squashed     INTEGER NOT NULL DEFAULT 0,
	pr_number    INTEGER NOT NULL DEFAULT 0,
	pr_state     TEXT NOT NULL DEFAULT '',
	pr_head_sha  TEXT NOT NULL DEFAULT '',
	pr_url       TEXT NOT NULL DEFAULT '',
	pr_checked   TEXT NOT NULL DEFAULT '',  -- local sha at the time of the PR lookup
	status       TEXT NOT NULL,
	reason       TEXT NOT NULL DEFAULT '',
	commit_time  INTEGER NOT NULL DEFAULT 0,
	subject      TEXT NOT NULL DEFAULT '',
	synced_at    INTEGER NOT NULL,
	PRIMARY KEY (repo_id, name)
);
CREATE TABLE IF NOT EXISTS purged (
	id          INTEGER PRIMARY KEY,
	repo_id     INTEGER NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
	name        TEXT NOT NULL,
	sha         TEXT NOT NULL,
	remote      TEXT NOT NULL DEFAULT '',
	merge_ref   TEXT NOT NULL DEFAULT '',
	status      TEXT NOT NULL,
	reason      TEXT NOT NULL DEFAULT '',
	batch       INTEGER NOT NULL,
	purged_at   INTEGER NOT NULL,
	restored_at INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS purged_repo ON purged(repo_id, purged_at);
`

// Store wraps the database.
type Store struct{ db *sql.DB }

// DefaultPath is $GIT_PURGE_DB, else $XDG_DATA_HOME/git-purge/state.db, else
// the platform's per-user data directory.
func DefaultPath() (string, error) {
	if p := os.Getenv("GIT_PURGE_DB"); p != "" {
		return p, nil
	}
	dir := os.Getenv("XDG_DATA_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		switch runtime.GOOS {
		case "darwin":
			dir = filepath.Join(home, "Library", "Application Support")
		case "windows":
			dir = os.Getenv("LOCALAPPDATA")
		default:
			dir = filepath.Join(home, ".local", "share")
		}
	}
	return filepath.Join(dir, "git-purge", "state.db"), nil
}

// Open opens (creating if needed) the database at path.
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path+"?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("init database %s: %w", path, err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// Repo is a tracked repository.
type Repo struct {
	ID       int64
	Path     string
	Base     string
	Forge    string
	LastSync time.Time
}

// Repo returns the record for path, creating it if needed.
func (s *Store) Repo(path string) (*Repo, error) {
	if _, err := s.db.Exec(`INSERT OR IGNORE INTO repos(path) VALUES (?)`, path); err != nil {
		return nil, err
	}
	var r Repo
	var last int64
	err := s.db.QueryRow(`SELECT id, path, base, forge, last_sync FROM repos WHERE path = ?`, path).
		Scan(&r.ID, &r.Path, &r.Base, &r.Forge, &last)
	if err != nil {
		return nil, err
	}
	if last > 0 {
		r.LastSync = time.Unix(last, 0)
	}
	return &r, nil
}

// Branch is a synced branch row.
type Branch struct {
	Name       string
	SHA        string
	Upstream   string
	Remote     string
	MergeRef   string
	Gone       bool
	Merged     bool
	Squashed   bool
	PRNumber   int
	PRState    string
	PRHeadSHA  string
	PRURL      string
	PRChecked  string
	Status     string
	Reason     string
	CommitTime time.Time
	Subject    string
}

// Branches returns the last synced branches of a repo.
func (s *Store) Branches(repoID int64) ([]Branch, error) {
	rows, err := s.db.Query(`SELECT name, sha, upstream, remote, merge_ref, gone, merged, squashed,
		pr_number, pr_state, pr_head_sha, pr_url, pr_checked, status, reason, commit_time, subject
		FROM branches WHERE repo_id = ? ORDER BY commit_time DESC`, repoID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Branch
	for rows.Next() {
		var b Branch
		var ct int64
		if err := rows.Scan(&b.Name, &b.SHA, &b.Upstream, &b.Remote, &b.MergeRef, &b.Gone, &b.Merged, &b.Squashed,
			&b.PRNumber, &b.PRState, &b.PRHeadSHA, &b.PRURL, &b.PRChecked, &b.Status, &b.Reason, &ct, &b.Subject); err != nil {
			return nil, err
		}
		b.CommitTime = time.Unix(ct, 0)
		out = append(out, b)
	}
	return out, rows.Err()
}

// ReplaceBranches atomically replaces a repo's branch snapshot.
func (s *Store) ReplaceBranches(repo *Repo, base, forgeName string, branches []Branch) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().Unix()
	if _, err := tx.Exec(`DELETE FROM branches WHERE repo_id = ?`, repo.ID); err != nil {
		return err
	}
	stmt, err := tx.Prepare(`INSERT INTO branches(repo_id, name, sha, upstream, remote, merge_ref, gone, merged, squashed,
		pr_number, pr_state, pr_head_sha, pr_url, pr_checked, status, reason, commit_time, subject, synced_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, b := range branches {
		if _, err := stmt.Exec(repo.ID, b.Name, b.SHA, b.Upstream, b.Remote, b.MergeRef, b.Gone, b.Merged, b.Squashed,
			b.PRNumber, b.PRState, b.PRHeadSHA, b.PRURL, b.PRChecked, b.Status, b.Reason, b.CommitTime.Unix(), b.Subject, now); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`UPDATE repos SET base = ?, forge = ?, last_sync = ? WHERE id = ?`, base, forgeName, now, repo.ID); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	repo.Base, repo.Forge, repo.LastSync = base, forgeName, time.Unix(now, 0)
	return nil
}

// RemoveBranch drops a branch from the snapshot (after it was purged).
func (s *Store) RemoveBranch(repoID int64, name string) error {
	_, err := s.db.Exec(`DELETE FROM branches WHERE repo_id = ? AND name = ?`, repoID, name)
	return err
}

// Purged is one entry in the deletion log.
type Purged struct {
	ID         int64
	Name       string
	SHA        string
	Remote     string
	MergeRef   string
	Status     string
	Reason     string
	Batch      int64
	PurgedAt   time.Time
	RestoredAt time.Time
}

// NewBatch returns an id grouping the deletions of one purge run.
func NewBatch() int64 { return time.Now().UnixNano() }

// LogPurge records a deleted branch.
func (s *Store) LogPurge(repoID int64, batch int64, b Branch) error {
	_, err := s.db.Exec(`INSERT INTO purged(repo_id, name, sha, remote, merge_ref, status, reason, batch, purged_at)
		VALUES (?,?,?,?,?,?,?,?,?)`, repoID, b.Name, b.SHA, b.Remote, b.MergeRef, b.Status, b.Reason, batch, time.Now().Unix())
	return err
}

// History returns the deletion log, newest first.
func (s *Store) History(repoID int64, limit int) ([]Purged, error) {
	rows, err := s.db.Query(`SELECT id, name, sha, remote, merge_ref, status, reason, batch, purged_at, restored_at
		FROM purged WHERE repo_id = ? ORDER BY purged_at DESC, id DESC LIMIT ?`, repoID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Purged
	for rows.Next() {
		var p Purged
		var pa, ra int64
		if err := rows.Scan(&p.ID, &p.Name, &p.SHA, &p.Remote, &p.MergeRef, &p.Status, &p.Reason, &p.Batch, &pa, &ra); err != nil {
			return nil, err
		}
		p.PurgedAt = time.Unix(pa, 0)
		if ra > 0 {
			p.RestoredAt = time.Unix(ra, 0)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// LastBatch returns the unrestored entries of the most recent purge run.
func (s *Store) LastBatch(repoID int64) ([]Purged, error) {
	var batch int64
	err := s.db.QueryRow(`SELECT batch FROM purged WHERE repo_id = ? AND restored_at = 0
		ORDER BY purged_at DESC, id DESC LIMIT 1`, repoID).Scan(&batch)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	all, err := s.History(repoID, 1<<30)
	if err != nil {
		return nil, err
	}
	var out []Purged
	for _, p := range all {
		if p.Batch == batch && p.RestoredAt.IsZero() {
			out = append(out, p)
		}
	}
	return out, nil
}

// LatestPurged returns the newest unrestored log entry for a branch name.
func (s *Store) LatestPurged(repoID int64, name string) (*Purged, error) {
	all, err := s.History(repoID, 1<<30)
	if err != nil {
		return nil, err
	}
	for _, p := range all {
		if p.Name == name && p.RestoredAt.IsZero() {
			return &p, nil
		}
	}
	return nil, nil
}

// MarkRestored flags a log entry as restored.
func (s *Store) MarkRestored(id int64) error {
	_, err := s.db.Exec(`UPDATE purged SET restored_at = ? WHERE id = ?`, time.Now().Unix(), id)
	return err
}
