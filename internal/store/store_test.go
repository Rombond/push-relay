package store

import (
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
