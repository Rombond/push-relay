package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func load(t *testing.T, body string) error {
	t.Helper()
	p := filepath.Join(t.TempDir(), "relay.yaml")
	os.WriteFile(p, []byte(body), 0o600)
	_, err := Load(p)
	return err
}

const apns = "    apns: {bundle_id: a, team_id: b, key_id: c, key_file: /k.p8}\n"

func TestMediaAndImageDomains(t *testing.T) {
	ok := []string{
		"apps:\n  x:\n" + apns,
		"apps:\n  x:\n    image_domains: [image.tmdb.org, \"*.example.org\"]\n" + apns,
		"media: {base_url: \"https://push.example.org\"}\napps:\n  x:\n    media: true\n" + apns,
	}
	for _, c := range ok {
		if err := load(t, c); err != nil {
			t.Errorf("%q: %v", c, err)
		}
	}
	bad := map[string]string{
		"apps:\n  x:\n    image_domains: [\"https://a.org\"]\n" + apns:              "image_domains",
		"apps:\n  x:\n    image_domains: [\"a*.org\"]\n" + apns:                     "image_domains",
		"apps:\n  x:\n    media: true\n" + apns:                                     "base_url",
		"media: {base_url: \"http://push.example.org\"}\napps:\n  x:\n" + apns:      "base_url",
		"media: {base_url: \"https://push.example.org/sub\"}\napps:\n  x:\n" + apns: "base_url",
	}
	for c, want := range bad {
		if err := load(t, c); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: got %v, want %s", c, err, want)
		}
	}
}

func TestRegistrationDefaults(t *testing.T) {
	p := filepath.Join(t.TempDir(), "relay.yaml")
	os.WriteFile(p, []byte("apps:\n  x:\n    open_registration: true\n"+apns), 0o600)
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	r := c.Register
	if r.Closed || r.NewKeysPerHour != 50 || r.PerIPPerMinute != 30 || r.DailyQuota != 200 || r.UnusedDays != 90 || r.IPRetentionDays != 30 || !c.Apps["x"].OpenRegistration {
		t.Fatalf("%+v", r)
	}
	t.Setenv("RELAY_REGISTRATION_CLOSED", "1")
	if c, _ = Load(p); !c.Register.Closed {
		t.Fatal("env kill switch ignored")
	}
	if err := load(t, "registration: {daily_quota: -5}\napps:\n  x:\n"+apns); err == nil {
		t.Fatal("negative accepted")
	}
}
