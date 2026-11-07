// Package store keeps the server keys (hashed) and usage counters in SQLite.
// Device tokens are never stored.
package store

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

var ErrNotFound = errors.New("key not found")

type Key struct {
	ID        int64
	App       string
	Name      string
	CreatedAt time.Time
	Revoked   bool
	Sent      int64
	Failed    int64
	// v0.4.0 open registration
	Public    bool       // created by POST /v1/register
	Quota     *int       // per-key daily quota override, nil = the default for its kind, 0 = unlimited
	LastUsed  *time.Time // last authenticated request (updated at most once an hour)
	InstallID string
}

type Store struct{ db *sql.DB }

const schema = `
CREATE TABLE IF NOT EXISTS keys (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  app TEXT NOT NULL,
  name TEXT NOT NULL,
  hash TEXT NOT NULL UNIQUE,
  created_at TEXT NOT NULL,
  revoked_at TEXT
);
CREATE TABLE IF NOT EXISTS usage (
  key_id INTEGER NOT NULL REFERENCES keys(id),
  day TEXT NOT NULL,
  sent INTEGER NOT NULL DEFAULT 0,
  failed INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (key_id, day)
);`

// migrations run in order; PRAGMA user_version holds how many were applied.
var migrations = []string{
	// 1: v0.4.0 open registration. reg_ip is a keyed hash (see HashIP), never the address itself.
	`ALTER TABLE keys ADD COLUMN public INTEGER NOT NULL DEFAULT 0;
ALTER TABLE keys ADD COLUMN install_id TEXT;
ALTER TABLE keys ADD COLUMN server_url TEXT;
ALTER TABLE keys ADD COLUMN version TEXT;
ALTER TABLE keys ADD COLUMN reg_ip TEXT;
ALTER TABLE keys ADD COLUMN quota INTEGER;
ALTER TABLE keys ADD COLUMN last_used_at TEXT;
ALTER TABLE keys ADD COLUMN rotated_at TEXT;
CREATE UNIQUE INDEX keys_install ON keys(app, install_id) WHERE install_id IS NOT NULL;
CREATE INDEX keys_public_created ON keys(public, created_at);
CREATE TABLE meta (k TEXT PRIMARY KEY, v TEXT NOT NULL);`,
}

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, err
	}
	if err := migrate(db); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

func migrate(db *sql.DB) error {
	if _, err := db.Exec(schema); err != nil {
		return err
	}
	var v int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		return err
	}
	for i := v; i < len(migrations); i++ {
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(migrations[i]); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
		if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, i+1)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) Close() error { return s.db.Close() }

func hash(raw string) string {
	h := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(h[:])
}

func newRaw() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "pr_" + base64.RawURLEncoding.EncodeToString(b), nil
}

func ts(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// Add creates a key for an app. The raw key is returned once and never stored.
func (s *Store) Add(app, name string) (string, int64, error) {
	raw, err := newRaw()
	if err != nil {
		return "", 0, err
	}
	res, err := s.db.Exec(`INSERT INTO keys (app, name, hash, created_at) VALUES (?, ?, ?, ?)`,
		app, name, hash(raw), ts(time.Now()))
	if err != nil {
		return "", 0, err
	}
	id, _ := res.LastInsertId()
	return raw, id, nil
}

// Lookup returns the active key for a raw key, or ErrNotFound (unknown or revoked).
func (s *Store) Lookup(raw string) (Key, error) {
	var k Key
	var created string
	var quota sql.NullInt64
	var last, inst sql.NullString
	err := s.db.QueryRow(`SELECT id, app, name, created_at, public, quota, last_used_at, install_id FROM keys WHERE hash = ? AND revoked_at IS NULL`, hash(raw)).
		Scan(&k.ID, &k.App, &k.Name, &created, &k.Public, &quota, &last, &inst)
	if errors.Is(err, sql.ErrNoRows) {
		return Key{}, ErrNotFound
	}
	if err != nil {
		return Key{}, err
	}
	k.CreatedAt, _ = time.Parse(time.RFC3339, created)
	fill(&k, quota, last, inst)
	return k, nil
}

func fill(k *Key, quota sql.NullInt64, last, inst sql.NullString) {
	if quota.Valid {
		q := int(quota.Int64)
		k.Quota = &q
	}
	if last.Valid {
		if t, err := time.Parse(time.RFC3339, last.String); err == nil {
			k.LastUsed = &t
		}
	}
	k.InstallID = inst.String
}

func (s *Store) Revoke(id int64) error {
	res, err := s.db.Exec(`UPDATE keys SET revoked_at = ? WHERE id = ? AND revoked_at IS NULL`,
		ts(time.Now()), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// List returns every key with lifetime counters.
func (s *Store) List() ([]Key, error) {
	rows, err := s.db.Query(`
SELECT k.id, k.app, k.name, k.created_at, k.revoked_at IS NOT NULL,
       COALESCE(SUM(u.sent), 0), COALESCE(SUM(u.failed), 0), k.public, k.quota, k.last_used_at, k.install_id
FROM keys k LEFT JOIN usage u ON u.key_id = k.id
GROUP BY k.id ORDER BY k.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Key
	for rows.Next() {
		var k Key
		var created string
		var quota sql.NullInt64
		var last, inst sql.NullString
		if err := rows.Scan(&k.ID, &k.App, &k.Name, &created, &k.Revoked, &k.Sent, &k.Failed, &k.Public, &quota, &last, &inst); err != nil {
			return nil, err
		}
		k.CreatedAt, _ = time.Parse(time.RFC3339, created)
		fill(&k, quota, last, inst)
		out = append(out, k)
	}
	return out, rows.Err()
}

func day(t time.Time) string { return t.UTC().Format("2006-01-02") }

// Today returns the pushes attempted today by a key (sent + failed).
func (s *Store) Today(keyID int64, now time.Time) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COALESCE(SUM(sent + failed), 0) FROM usage WHERE key_id = ? AND day = ?`, keyID, day(now)).Scan(&n)
	return n, err
}

func (s *Store) Record(keyID int64, now time.Time, ok bool) error {
	sent, failed := 1, 0
	if !ok {
		sent, failed = 0, 1
	}
	_, err := s.db.Exec(`
INSERT INTO usage (key_id, day, sent, failed) VALUES (?, ?, ?, ?)
ON CONFLICT (key_id, day) DO UPDATE SET sent = sent + excluded.sent, failed = failed + excluded.failed`,
		keyID, day(now), sent, failed)
	return err
}
