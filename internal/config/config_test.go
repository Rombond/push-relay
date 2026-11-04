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
