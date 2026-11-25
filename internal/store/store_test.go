package store

import (
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "r.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestKeyLifecycle(t *testing.T) {
	s := open(t)
	raw, id, err := s.Add("tipsarr", "home")
	if err != nil || !strings.HasPrefix(raw, "pr_") {
		t.Fatalf("%q %v", raw, err)
	}
	k, err := s.Lookup(raw)
	if err != nil || k.ID != id || k.App != "tipsarr" || k.Name != "home" {
		t.Fatalf("%+v %v", k, err)
	}
	if _, err := s.Lookup(raw + "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("wrong key: %v", err)
	}
	if err := s.Revoke(id); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Lookup(raw); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoked key still valid: %v", err)
	}
	if err := s.Revoke(id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("double revoke: %v", err)
	}
}

func TestRawKeyNeverStored(t *testing.T) {
	s := open(t)
	raw, _, _ := s.Add("a", "n")
	var n int
	s.db.QueryRow(`SELECT COUNT(*) FROM keys WHERE hash = ?`, raw).Scan(&n)
	if n != 0 {
		t.Fatal("raw key stored in clear")
	}
}

func TestUsageAndList(t *testing.T) {
	s := open(t)
	_, id, _ := s.Add("a", "n")
	day1 := time.Date(2026, 10, 10, 23, 0, 0, 0, time.UTC)
	s.Record(id, day1, true)
	s.Record(id, day1, true)
	s.Record(id, day1, false)
	s.Record(id, day1.Add(2*time.Hour), true)
	if n, _ := s.Today(id, day1); n != 3 {
		t.Fatalf("today %d", n)
	}
	if n, _ := s.Today(id, day1.Add(2*time.Hour)); n != 1 {
		t.Fatalf("next day %d", n)
	}
	ks, err := s.List()
	if err != nil || len(ks) != 1 || ks[0].Sent != 3 || ks[0].Failed != 1 {
		t.Fatalf("%+v %v", ks, err)
	}
}

func TestMigrationTwoOnV1Database(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v1.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(schema); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(migrations[0] + `PRAGMA user_version = 1;
INSERT INTO keys (app, name, hash, created_at) VALUES ('tipsarr', 'old', 'h', '2026-01-01T00:00:00Z');
INSERT INTO usage (key_id, day, sent, failed) VALUES (1, '2026-11-07', 3, 1);`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	st.RecordLive(1, now, true)
	st.RecordLive(1, now, false)
	if n, _ := st.Today(1, now); n != 4 {
		t.Fatalf("alerts %d", n)
	}
	if n, _ := st.LiveToday(1, now); n != 2 {
		t.Fatalf("live %d", n)
	}
}
