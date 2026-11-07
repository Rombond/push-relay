package store

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"net"
	"strings"
	"time"
)

// Registration is what POST /v1/register stores next to a public key.
type Registration struct {
	App, InstallID, ServerURL, Version string
	IPHash                             string // from HashIP, "" if unknown
}

// RegisterResult tells whether Register created a key or rotated an existing one.
type RegisterResult struct {
	Raw     string
	ID      int64
	Rotated bool
}

// ErrCap is returned by Register when a new key would exceed the hourly cap. Rotations never hit it.
var ErrCap = errors.New("registration cap reached")

// Register issues a public key for app+installID. An active key for the same pair is rotated in place (old raw key
// revoked by replacing its hash) and does not count toward capPerHour. A new or previously revoked installation gets a
// new row, refused with ErrCap (and the wait) when capPerHour public keys were already created in the last hour
// (0 = no cap). Callers must serialise Register so the cap check and the insert stay atomic.
func (s *Store) Register(r Registration, capPerHour int, now time.Time) (RegisterResult, time.Duration, error) {
	raw, err := newRaw()
	if err != nil {
		return RegisterResult{}, 0, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return RegisterResult{}, 0, err
	}
	defer tx.Rollback()

	var id int64
	var revoked sql.NullString
	err = tx.QueryRow(`SELECT id, revoked_at FROM keys WHERE app = ? AND install_id = ?`, r.App, r.InstallID).Scan(&id, &revoked)
	switch {
	case err == nil && !revoked.Valid:
		_, err = tx.Exec(`UPDATE keys SET hash = ?, rotated_at = ?, server_url = ?, version = ?, reg_ip = ? WHERE id = ?`,
			hash(raw), ts(now), nullable(r.ServerURL), nullable(r.Version), nullable(r.IPHash), id)
		if err != nil {
			return RegisterResult{}, 0, err
		}
		return RegisterResult{Raw: raw, ID: id, Rotated: true}, 0, tx.Commit()
	case err == nil:
		// Revoked: keep the old row for the record, free the installation id for a new key.
		if _, err := tx.Exec(`UPDATE keys SET install_id = NULL WHERE id = ?`, id); err != nil {
			return RegisterResult{}, 0, err
		}
	case !errors.Is(err, sql.ErrNoRows):
		return RegisterResult{}, 0, err
	}

	if capPerHour > 0 {
		var n int
		var oldest sql.NullString
		since := ts(now.Add(-time.Hour))
		if err := tx.QueryRow(`SELECT COUNT(*), MIN(created_at) FROM keys WHERE public = 1 AND created_at > ?`, since).Scan(&n, &oldest); err != nil {
			return RegisterResult{}, 0, err
		}
		if n >= capPerHour {
			wait := time.Minute
			if t, err := time.Parse(time.RFC3339, oldest.String); err == nil {
				wait = max(time.Second, t.Add(time.Hour).Sub(now))
			}
			return RegisterResult{}, wait, ErrCap
		}
	}
	res, err := tx.Exec(`INSERT INTO keys (app, name, hash, created_at, public, install_id, server_url, version, reg_ip)
VALUES (?, 'registered', ?, ?, 1, ?, ?, ?, ?)`, r.App, hash(raw), ts(now), r.InstallID, nullable(r.ServerURL), nullable(r.Version), nullable(r.IPHash))
	if err != nil {
		return RegisterResult{}, 0, err
	}
	id, _ = res.LastInsertId()
	return RegisterResult{Raw: raw, ID: id}, 0, tx.Commit()
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// HashIP returns a keyed hash (HMAC-SHA256 with a random secret kept in this database) of a client address.
// Mass revoke by address only needs equality, so the address itself is never stored; the secret stops a plain
// lookup table, and the hash is dropped by Cleanup after ipRetention.
func (s *Store) HashIP(ip string) (string, error) {
	sec, err := s.secret()
	if err != nil {
		return "", err
	}
	m := hmac.New(sha256.New, sec)
	ip = strings.TrimSpace(ip)
	if p := net.ParseIP(ip); p != nil {
		ip = p.String() // one spelling per address, so the CLI matches what the server stored
	}
	m.Write([]byte(ip))
	return hex.EncodeToString(m.Sum(nil)), nil
}

func (s *Store) secret() ([]byte, error) {
	var v string
	err := s.db.QueryRow(`SELECT v FROM meta WHERE k = 'ip_secret'`).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		b := make([]byte, 32)
		if _, err := rand.Read(b); err != nil {
			return nil, err
		}
		if _, err := s.db.Exec(`INSERT OR IGNORE INTO meta (k, v) VALUES ('ip_secret', ?)`, hex.EncodeToString(b)); err != nil {
			return nil, err
		}
		err = s.db.QueryRow(`SELECT v FROM meta WHERE k = 'ip_secret'`).Scan(&v)
	}
	if err != nil {
		return nil, err
	}
	return hex.DecodeString(v)
}

// Touch records that a key was used. It writes at most once an hour per key.
func (s *Store) Touch(k Key, now time.Time) error {
	if k.LastUsed != nil && now.Sub(*k.LastUsed) < time.Hour {
		return nil
	}
	_, err := s.db.Exec(`UPDATE keys SET last_used_at = ? WHERE id = ?`, ts(now), k.ID)
	return err
}

// SetQuota sets a key's daily quota: n >= 0 (0 = unlimited), or nil to go back to the default for its kind.
func (s *Store) SetQuota(id int64, n *int) error {
	var v any
	if n != nil {
		v = *n
	}
	res, err := s.db.Exec(`UPDATE keys SET quota = ? WHERE id = ?`, v, id)
	if err != nil {
		return err
	}
	if c, _ := res.RowsAffected(); c == 0 {
		return ErrNotFound
	}
	return nil
}

// RevokeFilter selects public keys for a mass revoke. Zero fields do not filter; at least one must be set.
type RevokeFilter struct {
	Since, Until time.Time // creation time window
	IPHash       string
}

// RevokePublic revokes every active public key matching f and returns how many.
func (s *Store) RevokePublic(f RevokeFilter, now time.Time) (int64, error) {
	q := `UPDATE keys SET revoked_at = ? WHERE public = 1 AND revoked_at IS NULL`
	args := []any{ts(now)}
	if f.Since.IsZero() && f.Until.IsZero() && f.IPHash == "" {
		return 0, errors.New("mass revoke needs since, until or ip")
	}
	if !f.Since.IsZero() {
		q += ` AND created_at >= ?`
		args = append(args, ts(f.Since))
	}
	if !f.Until.IsZero() {
		q += ` AND created_at <= ?`
		args = append(args, ts(f.Until))
	}
	if f.IPHash != "" {
		q += ` AND reg_ip = ?`
		args = append(args, f.IPHash)
	}
	res, err := s.db.Exec(q, args...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// Cleanup deletes public keys (and their usage) not used for unused (never used: since creation), and forgets
// registration address hashes older than ipRetention. A zero duration skips that step.
func (s *Store) Cleanup(now time.Time, unused, ipRetention time.Duration) (int64, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var n int64
	if unused > 0 {
		cut := ts(now.Add(-unused))
		sel := `SELECT id FROM keys WHERE public = 1 AND MAX(created_at, COALESCE(last_used_at, ''), COALESCE(rotated_at, '')) < ?`
		if _, err := tx.Exec(`DELETE FROM usage WHERE key_id IN (`+sel+`)`, cut); err != nil {
			return 0, err
		}
		res, err := tx.Exec(`DELETE FROM keys WHERE id IN (`+sel+`)`, cut)
		if err != nil {
			return 0, err
		}
		n, _ = res.RowsAffected()
	}
	if ipRetention > 0 {
		if _, err := tx.Exec(`UPDATE keys SET reg_ip = NULL WHERE reg_ip IS NOT NULL AND COALESCE(rotated_at, created_at) < ?`, ts(now.Add(-ipRetention))); err != nil {
			return 0, err
		}
	}
	return n, tx.Commit()
}

// CountKeys returns active keys by kind, for the metrics.
func (s *Store) CountKeys() (public, manual int64, err error) {
	err = s.db.QueryRow(`SELECT COALESCE(SUM(public = 1), 0), COALESCE(SUM(public = 0), 0) FROM keys WHERE revoked_at IS NULL`).Scan(&public, &manual)
	return
}

// RegistrationClosed reports the runtime kill switch set by `relay registration close`.
func (s *Store) RegistrationClosed() (bool, error) {
	var v string
	err := s.db.QueryRow(`SELECT v FROM meta WHERE k = 'registration_closed'`).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return v == "1", err
}

func (s *Store) SetRegistrationClosed(closed bool) error {
	v := "0"
	if closed {
		v = "1"
	}
	_, err := s.db.Exec(`INSERT INTO meta (k, v) VALUES ('registration_closed', ?) ON CONFLICT (k) DO UPDATE SET v = excluded.v`, v)
	return err
}
