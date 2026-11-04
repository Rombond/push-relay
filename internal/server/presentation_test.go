package server

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Rombond/push-relay/internal/media"
	"github.com/Rombond/push-relay/internal/push"
	"github.com/Rombond/push-relay/internal/templates"
)

// freeApp adds a free_text app "jarvis" with an image allow-list and media uploads, and returns its key and sender.
func freeApp(t *testing.T, e *env) (string, *fakeSender) {
	t.Helper()
	tpl, _ := templates.Parse(templates.Builtin("tipsarr"))
	snd := &fakeSender{}
	e.s.Apps["jarvis"] = &App{Templates: tpl, FreeText: true, Senders: map[string]push.Sender{"ios": snd},
		ImageDomains: []string{"image.tmdb.org", "*.example.org"}, Media: true}
	k, _, _ := e.st.Add("jarvis", "orchestrator")
	return k, snd
}

func TestTipsarrRequestCarriesNoPresentation(t *testing.T) {
	e := setup(t, defaults)
	e.post(t, "/v1/push", e.key, good())
	m := e.ios.got[0]
	if m.Level != "" || m.Silent || m.Thread != "" || m.Collapse != "" || m.TTL != nil || m.Image != "" {
		t.Fatalf("v0.2.0 request grew v0.3.0 fields: %+v", m)
	}
}

func TestPresentationFieldsPassThrough(t *testing.T) {
	e := setup(t, defaults)
	k, snd := freeApp(t, e)
	r := good()
	for kv, v := range map[string]any{"level": "time-sensitive", "sound": "none", "thread": "watchdogs", "collapse": "imap",
		"ttl": 600, "image": "https://image.tmdb.org/t/p/w500/x.jpg"} {
		r[kv] = v
	}
	resp, out := e.post(t, "/v1/push", k, r)
	if resp.StatusCode != 200 {
		t.Fatalf("%d %v", resp.StatusCode, out)
	}
	m := snd.got[0]
	if m.Level != "time-sensitive" || !m.Silent || m.Thread != "watchdogs" || m.Collapse != "imap" || m.TTL == nil || *m.TTL != 600 ||
		m.Image != "https://image.tmdb.org/t/p/w500/x.jpg" {
		t.Fatalf("%+v", m)
	}
}

func TestPresentationNeedsFreeText(t *testing.T) {
	e := setup(t, defaults) // tipsarr in the test setup has no free_text
	for field, v := range map[string]any{"level": "passive", "sound": "none", "thread": "a", "collapse": "a", "ttl": 0} {
		r := good()
		r[field] = v
		if resp, _ := e.post(t, "/v1/push", e.key, r); resp.StatusCode != 400 {
			t.Errorf("%s accepted without free_text: %d", field, resp.StatusCode)
		}
	}
	if len(e.ios.got) != 0 {
		t.Fatal("sent")
	}
}

func TestPresentationValidation(t *testing.T) {
	e := setup(t, defaults)
	k, snd := freeApp(t, e)
	bad := []map[string]any{
		{"level": "critical"}, {"sound": "bell"}, {"thread": "has space"}, {"collapse": strings.Repeat("x", 65)},
		{"ttl": -1}, {"ttl": maxTTL + 1},
		{"image": "http://image.tmdb.org/a.jpg"}, {"image": "https://evil.org/a.jpg"}, {"image": "https://example.org/a.jpg"},
		{"image": "https://image.tmdb.org:8443/a.jpg"}, {"image": "https://u@image.tmdb.org/a.jpg"},
		{"image": "https://image.tmdb.org/" + strings.Repeat("a", 600)},
	}
	for _, b := range bad {
		r := good()
		for kk, v := range b {
			r[kk] = v
		}
		if resp, _ := e.post(t, "/v1/push", k, r); resp.StatusCode != 400 {
			t.Errorf("%v accepted: %d", b, resp.StatusCode)
		}
	}
	r := good()
	r["image"] = "https://cdn.example.org/a.png" // wildcard
	if resp, _ := e.post(t, "/v1/push", k, r); resp.StatusCode != 200 || len(snd.got) != 1 {
		t.Fatalf("wildcard domain refused: %d", resp.StatusCode)
	}
	tr := good()
	tr["image"] = "https://image.tmdb.org/a.jpg" // tipsarr in the setup has no allow-list
	if resp, _ := e.post(t, "/v1/push", e.key, tr); resp.StatusCode != 400 {
		t.Fatalf("image without allow-list: %d", resp.StatusCode)
	}
}

var pngBytes = append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{0}, 64)...)

func upload(t *testing.T, e *env, key string, data []byte) (*http.Response, map[string]any) {
	t.Helper()
	return e.post(t, "/v1/media", key, string(data))
}

func TestMediaUploadThenPushThenDownload(t *testing.T) {
	e := setup(t, defaults)
	now := time.Unix(1_800_000_000, 0)
	e.s.Media = media.New("https://push.example.org", time.Hour, 2, 1<<10, 10)
	e.s.Media.Now = func() time.Time { return now }
	e.srv.Config.Handler = e.s.Handler()
	k, snd := freeApp(t, e)

	if resp, _ := upload(t, e, e.key, pngBytes); resp.StatusCode != 403 { // tipsarr: media off
		t.Fatalf("media allowed for an app without media: %d", resp.StatusCode)
	}
	if resp, _ := upload(t, e, "", pngBytes); resp.StatusCode != 401 {
		t.Fatalf("no key: %d", resp.StatusCode)
	}
	if resp, _ := upload(t, e, k, []byte("GIF89a not allowed")); resp.StatusCode != 415 {
		t.Fatalf("gif: %d", resp.StatusCode)
	}
	if resp, _ := upload(t, e, k, bytes.Repeat([]byte{0x89}, 2<<10)); resp.StatusCode != 413 {
		t.Fatalf("too big: %d", resp.StatusCode)
	}
	resp, out := upload(t, e, k, pngBytes)
	u, _ := out["url"].(string)
	if resp.StatusCode != 200 || !strings.HasPrefix(u, "https://push.example.org/m/") || out["expiresAt"] != "2027-01-15T09:00:00Z" {
		t.Fatalf("%d %v", resp.StatusCode, out)
	}
	r := good()
	r["image"] = u // its own upload is allowed without being in image_domains
	if resp, _ := e.post(t, "/v1/push", k, r); resp.StatusCode != 200 || snd.got[0].Image != u {
		t.Fatalf("push with uploaded image: %d", resp.StatusCode)
	}

	local := e.srv.URL + strings.TrimPrefix(u, "https://push.example.org")
	get := func(url string) (int, string) {
		res, err := http.Get(url)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return res.StatusCode, res.Header.Get("Content-Type") + ":" + string(b[:min(4, len(b))])
	}
	if code, _ := get(local[:len(local)-2] + "xx"); code != 404 {
		t.Fatalf("bad signature served: %d", code)
	}
	if code, body := get(local); code != 200 || body != "image/png:\x89PNG" {
		t.Fatalf("download %d %q", code, body)
	}
	get(local) // second and last allowed download
	if code, _ := get(local); code != 404 {
		t.Fatalf("used up but served: %d", code)
	}
	_, out = upload(t, e, k, pngBytes)
	now = now.Add(time.Hour)
	if code, _ := get(e.srv.URL + strings.TrimPrefix(out["url"].(string), "https://push.example.org")); code != 404 {
		t.Fatalf("expired but served: %d", code)
	}
}

func TestMediaStoreFull(t *testing.T) {
	m := media.New("https://push.example.org", time.Hour, 1, 1<<10, 2)
	for i := 0; i < 2; i++ {
		if _, _, err := m.Put(pngBytes); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := m.Put(pngBytes); err != media.ErrFull {
		t.Fatalf("got %v", err)
	}
}

func TestMediaRoutesOffWithoutStore(t *testing.T) {
	e := setup(t, defaults)
	if resp, _ := upload(t, e, e.key, pngBytes); resp.StatusCode != 404 && resp.StatusCode != 405 {
		t.Fatalf("media on without config: %d", resp.StatusCode)
	}
}
