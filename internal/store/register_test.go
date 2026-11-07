package store

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

var t0 = time.Date(2026, 11, 7, 10, 0, 0, 0, time.UTC)

func reg(app, inst, ip string) Registration {
	return Registration{App: app, InstallID: inst, ServerURL: "https://tipsarr.example.org", Version: "1.0.0", IPHash: ip}
}

func TestMigrationFromV03(t *testing.T) {
	p := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(schema); err != nil {
		t.Fatal(err)
	}
	db.Exec(`INSERT INTO keys (app, name, hash, created_at) VALUES ('tipsarr', 'old', 'h', '2026-01-01T00:00:00Z')`)
	db.Close()
	s, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ks, err := s.List()
	if err != nil || len(ks) != 1 || ks[0].Public || ks[0].Quota != nil {
		t.Fatalf("%+v %v", ks, err)
	}
	s.Close()
	if s, err = Open(p); err != nil { // idempotent
		t.Fatal(err)
	}
}

func TestRegisterRotatesSameInstall(t *testing.T) {
	s := open(t)
	a, _, err := s.Register(reg("tipsarr", "install-aaaaaaaaaaaa", ""), 10, t0)
	if err != nil || a.Rotated {
		t.Fatalf("%+v %v", a, err)
	}
	k, err := s.Lookup(a.Raw)
	if err != nil || !k.Public || k.InstallID != "install-aaaaaaaaaaaa" {
		t.Fatalf("%+v %v", k, err)
	}
	b, _, err := s.Register(reg("tipsarr", "install-aaaaaaaaaaaa", ""), 10, t0.Add(time.Minute))
	if err != nil || !b.Rotated || b.ID != a.ID || b.Raw == a.Raw {
		t.Fatalf("%+v %v", b, err)
	}
	if _, err := s.Lookup(a.Raw); !errors.Is(err, ErrNotFound) {
		t.Fatal("old key still valid after rotation")
	}
	if _, err := s.Lookup(b.Raw); err != nil {
		t.Fatal(err)
	}
	// same install id, other app: another key
	c, _, _ := s.Register(reg("other", "install-aaaaaaaaaaaa", ""), 10, t0)
	if c.ID == a.ID || c.Rotated {
		t.Fatalf("%+v", c)
	}
}

func TestRegisterGlobalCap(t *testing.T) {
	s := open(t)
	first, _, _ := s.Register(reg("a", "install-000000000000", ""), 2, t0)
	s.Register(reg("a", "install-111111111111", ""), 2, t0.Add(10*time.Minute))
	_, wait, err := s.Register(reg("a", "install-222222222222", ""), 2, t0.Add(20*time.Minute))
	if !errors.Is(err, ErrCap) || wait != 40*time.Minute {
		t.Fatalf("%v %v", err, wait)
	}
	// rotations are free
	if r, _, err := s.Register(reg("a", "install-000000000000", ""), 2, t0.Add(21*time.Minute)); err != nil || !r.Rotated {
		t.Fatalf("rotation under cap: %+v %v", r, err)
	}
	// a rotation does not refresh created_at: the window slides past the first key
	if _, _, err := s.Register(reg("a", "install-222222222222", ""), 2, t0.Add(61*time.Minute)); err != nil {
		t.Fatal(err)
	}
	// revoked install comes back as a new key (counts toward the cap)
	s.Revoke(first.ID)
	r, _, err := s.Register(reg("a", "install-000000000000", ""), 0, t0.Add(62*time.Minute))
	if err != nil || r.Rotated || r.ID == first.ID {
		t.Fatalf("%+v %v", r, err)
	}
}

func TestQuotaOverride(t *testing.T) {
	s := open(t)
	raw, id, _ := s.Add("a", "n")
	n := 1000
	if err := s.SetQuota(id, &n); err != nil {
		t.Fatal(err)
	}
	if k, _ := s.Lookup(raw); k.Quota == nil || *k.Quota != 1000 {
		t.Fatalf("%+v", k)
	}
	s.SetQuota(id, nil)
	if k, _ := s.Lookup(raw); k.Quota != nil {
		t.Fatalf("%+v", k)
	}
	if err := s.SetQuota(999, &n); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
}

func TestMassRevoke(t *testing.T) {
	s := open(t)
	h1, _ := s.HashIP("203.0.113.7")
	h2, _ := s.HashIP("2001:db8::1")
	if h, _ := s.HashIP(" 2001:DB8:0::1 "); h != h2 {
		t.Fatal("ip not normalised")
	}
	if h1 == "203.0.113.7" || len(h1) != 64 {
		t.Fatal("ip stored in clear")
	}
	manual, _, _ := s.Add("a", "manual")
	a, _, _ := s.Register(reg("a", "install-000000000000", h1), 0, t0)
	b, _, _ := s.Register(reg("a", "install-111111111111", h2), 0, t0.Add(2*time.Hour))
	c, _, _ := s.Register(reg("a", "install-222222222222", h1), 0, t0.Add(4*time.Hour))
	if _, err := s.RevokePublic(RevokeFilter{}, t0); err == nil {
		t.Fatal("empty filter accepted")
	}
	n, err := s.RevokePublic(RevokeFilter{IPHash: h1, Until: t0.Add(time.Hour)}, t0)
	if err != nil || n != 1 {
		t.Fatalf("%d %v", n, err)
	}
	if _, err := s.Lookup(a.Raw); err == nil {
		t.Fatal("a not revoked")
	}
	n, _ = s.RevokePublic(RevokeFilter{Since: t0.Add(time.Hour)}, t0)
	if n != 2 {
		t.Fatalf("window revoked %d", n)
	}
	for _, raw := range []string{b.Raw, c.Raw} {
		if _, err := s.Lookup(raw); err == nil {
			t.Fatal("not revoked")
		}
	}
	if _, err := s.Lookup(manual); err != nil {
		t.Fatal("manual key revoked by a mass revoke")
	}
}

func TestCleanup(t *testing.T) {
	s := open(t)
	manual, _, _ := s.Add("a", "manual") // created now, never deleted anyway
	old, _, _ := s.Register(reg("a", "install-000000000000", "h"), 0, t0.Add(-100*24*time.Hour))
	used, _, _ := s.Register(reg("a", "install-111111111111", "h"), 0, t0.Add(-100*24*time.Hour))
	fresh, _, _ := s.Register(reg("a", "install-222222222222", "h"), 0, t0.Add(-time.Hour))
	k, _ := s.Lookup(used.Raw)
	s.Touch(k, t0.Add(-10*24*time.Hour))
	s.Record(old.ID, t0.Add(-100*24*time.Hour), true)
	s.db.Exec(`UPDATE keys SET created_at = '2020-01-01T00:00:00Z' WHERE hash = ?`, hash(manual))
	n, err := s.Cleanup(t0, 90*24*time.Hour, 30*24*time.Hour)
	if err != nil || n != 1 {
		t.Fatalf("%d %v", n, err)
	}
	if _, err := s.Lookup(old.Raw); err == nil {
		t.Fatal("old key kept")
	}
	for _, raw := range []string{manual, used.Raw, fresh.Raw} {
		if _, err := s.Lookup(raw); err != nil {
			t.Fatal("kept key deleted")
		}
	}
	var ips int
	s.db.QueryRow(`SELECT COUNT(*) FROM keys WHERE reg_ip IS NOT NULL`).Scan(&ips)
	if ips != 1 { // only the fresh one keeps its address hash
		t.Fatalf("ip hashes left %d", ips)
	}
}

func TestTouchAndCount(t *testing.T) {
	s := open(t)
	raw, _, _ := s.Add("a", "n")
	s.Register(reg("a", "install-000000000000", ""), 0, t0)
	k, _ := s.Lookup(raw)
	s.Touch(k, t0)
	k, _ = s.Lookup(raw)
	if k.LastUsed == nil || !k.LastUsed.Equal(t0) {
		t.Fatalf("%+v", k)
	}
	s.Touch(k, t0.Add(time.Minute)) // throttled
	if k2, _ := s.Lookup(raw); !k2.LastUsed.Equal(t0) {
		t.Fatal("touch not throttled")
	}
	if p, m, err := s.CountKeys(); err != nil || p != 1 || m != 1 {
		t.Fatalf("%d %d %v", p, m, err)
	}
	if c, _ := s.RegistrationClosed(); c {
		t.Fatal("closed by default")
	}
	s.SetRegistrationClosed(true)
	if c, _ := s.RegistrationClosed(); !c {
		t.Fatal("switch")
	}
}
