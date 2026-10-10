package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Rombond/push-relay/internal/config"
	"github.com/Rombond/push-relay/internal/push"
	"github.com/Rombond/push-relay/internal/store"
	"github.com/Rombond/push-relay/internal/templates"
)

type fakeSender struct {
	mu   sync.Mutex
	got  []push.Message
	next push.Result
}

func (f *fakeSender) Send(_ context.Context, m push.Message) push.Result {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.got = append(f.got, m)
	if f.next.Status == "" {
		return push.Result{Status: push.Sent}
	}
	return f.next
}

type env struct {
	srv  *httptest.Server
	s    *Server
	ios  *fakeSender
	andr *fakeSender
	key  string
	st   *store.Store
}

const iosTok = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
const andTok = "dXlfZ0FfZmFrZV9mY21fdG9rZW46QVBBOTFiRl9fX19fX19fX19fX19fX18"

func setup(t *testing.T, lim config.Limits) *env {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "r.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	tpl, err := templates.Parse(templates.Builtin("tipsarr"))
	if err != nil {
		t.Fatal(err)
	}
	e := &env{ios: &fakeSender{}, andr: &fakeSender{}, st: st}
	apps := map[string]*App{
		"tipsarr": {Templates: tpl, Senders: map[string]push.Sender{"ios": e.ios, "android": e.andr}},
		"other":   {Templates: tpl, Senders: map[string]push.Sender{"ios": e.ios}},
	}
	e.s = New(st, apps, lim, slog.New(slog.NewTextHandler(io.Discard, nil)))
	e.srv = httptest.NewServer(e.s.Handler())
	t.Cleanup(e.srv.Close)
	e.key, _, _ = st.Add("tipsarr", "test")
	return e
}

var defaults = config.Limits{KeyRate: 1000, KeyBurst: 1000, TokenPerMinute: 600}

func (e *env) post(t *testing.T, path, key string, body any) (*http.Response, map[string]any) {
	t.Helper()
	var buf bytes.Buffer
	switch b := body.(type) {
	case string:
		buf.WriteString(b)
	default:
		json.NewEncoder(&buf).Encode(b)
	}
	req, _ := http.NewRequest("POST", e.srv.URL+path, &buf)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return resp, out
}

func good() map[string]any {
	return map[string]any{"platform": "ios", "token": iosTok, "event": "request.approved", "id": "42", "lang": "fr"}
}

func TestPushHappyPathBuildsLocalizedAlert(t *testing.T) {
	e := setup(t, defaults)
	resp, out := e.post(t, "/v1/push", e.key, good())
	if resp.StatusCode != 200 || out["status"] != "sent" {
		t.Fatalf("%d %v", resp.StatusCode, out)
	}
	if len(e.ios.got) != 1 {
		t.Fatalf("sent %d", len(e.ios.got))
	}
	m := e.ios.got[0]
	if m.Title != "Demande approuvée" || m.Category != "requests" || m.ID != "42" || m.Event != "request.approved" || m.Token != iosTok {
		t.Fatalf("%+v", m)
	}
}

func TestServerHostIsPassedThrough(t *testing.T) {
	e := setup(t, defaults)
	r := good()
	r["server"] = "tipsarr.example.org:8443"
	if resp, _ := e.post(t, "/v1/push", e.key, r); resp.StatusCode != 200 || e.ios.got[0].Server != "tipsarr.example.org:8443" {
		t.Fatalf("%d %+v", resp.StatusCode, e.ios.got)
	}
}

func TestAndroidRoutesToFCM(t *testing.T) {
	e := setup(t, defaults)
	r := good()
	r["platform"], r["token"], r["lang"] = "android", andTok, "en"
	resp, _ := e.post(t, "/v1/push", e.key, r)
	if resp.StatusCode != 200 || len(e.andr.got) != 1 || len(e.ios.got) != 0 || e.andr.got[0].Title != "Request approved" {
		t.Fatalf("%d %+v", resp.StatusCode, e.andr.got)
	}
}

func TestAuth(t *testing.T) {
	e := setup(t, defaults)
	for name, key := range map[string]string{"missing": "", "wrong": "pr_nope"} {
		resp, _ := e.post(t, "/v1/push", key, good())
		if resp.StatusCode != 401 {
			t.Errorf("%s: %d", name, resp.StatusCode)
		}
	}
	_, id, _ := e.st.Add("tipsarr", "x")
	raw, _, _ := e.st.Add("tipsarr", "revoked")
	_ = id
	ks, _ := e.st.List()
	e.st.Revoke(ks[len(ks)-1].ID)
	if resp, _ := e.post(t, "/v1/push", raw, good()); resp.StatusCode != 401 {
		t.Errorf("revoked: %d", resp.StatusCode)
	}
	if len(e.ios.got) != 0 {
		t.Fatal("unauthorized request reached APNs")
	}
}

func TestKeyScopedToItsApp(t *testing.T) {
	e := setup(t, defaults)
	r := good()
	r["app"] = "other"
	if resp, _ := e.post(t, "/v1/push", e.key, r); resp.StatusCode != 403 {
		t.Fatalf("%d", resp.StatusCode)
	}
	r["app"] = "tipsarr"
	if resp, _ := e.post(t, "/v1/push", e.key, r); resp.StatusCode != 200 {
		t.Fatalf("%d", resp.StatusCode)
	}
}

func TestValidation(t *testing.T) {
	e := setup(t, defaults)
	mut := func(k string, v any) map[string]any { r := good(); r[k] = v; return r }
	cases := map[string]any{
		"bad platform":  mut("platform", "windows"),
		"bad ios token": mut("token", "not hex!"),
		"short token":   mut("token", "abcd"),
		"bad event":     mut("event", "request.exploded"),
		"id with space": mut("id", "a b"),
		"id empty":      mut("id", ""),
		"id too long":   mut("id", strings.Repeat("a", 65)),
		"unknown field": mut("text", "free text phishing"),
		"not json":      "{",
		"trailing json": `{"platform":"ios"} {}`,
	}
	for name, body := range cases {
		resp, _ := e.post(t, "/v1/push", e.key, body)
		if resp.StatusCode != 400 {
			t.Errorf("%s: %d", name, resp.StatusCode)
		}
	}
	if len(e.ios.got) != 0 {
		t.Fatal("invalid request was delivered")
	}
}

func TestPlatformNotConfiguredForApp(t *testing.T) {
	e := setup(t, defaults)
	k, _, _ := e.st.Add("other", "o")
	r := good()
	r["platform"], r["token"] = "android", andTok
	if resp, _ := e.post(t, "/v1/push", k, r); resp.StatusCode != 400 {
		t.Fatalf("%d", resp.StatusCode)
	}
}

func TestBodyTooLarge(t *testing.T) {
	e := setup(t, defaults)
	r := good()
	r["lang"] = strings.Repeat("x", 9000)
	if resp, _ := e.post(t, "/v1/push", e.key, r); resp.StatusCode != 413 {
		t.Fatalf("%d", resp.StatusCode)
	}
}

func TestInvalidTokenPassedBackWith200(t *testing.T) {
	e := setup(t, defaults)
	e.ios.next = push.Result{Status: push.InvalidToken}
	resp, out := e.post(t, "/v1/push", e.key, good())
	if resp.StatusCode != 200 || out["status"] != "invalid_token" {
		t.Fatalf("%d %v", resp.StatusCode, out)
	}
}

func TestUpstreamFailureIs502(t *testing.T) {
	e := setup(t, defaults)
	e.ios.next = push.Result{Status: push.Failed, Error: "apns unreachable"}
	resp, out := e.post(t, "/v1/push", e.key, good())
	if resp.StatusCode != 502 || out["status"] != "error" {
		t.Fatalf("%d %v", resp.StatusCode, out)
	}
}

func TestKeyRateLimit(t *testing.T) {
	e := setup(t, config.Limits{KeyRate: 0.001, KeyBurst: 2, TokenPerMinute: 600})
	var last *http.Response
	for i := 0; i < 3; i++ {
		last, _ = e.post(t, "/v1/push", e.key, good())
	}
	if last.StatusCode != 429 || last.Header.Get("Retry-After") == "" {
		t.Fatalf("%d %q", last.StatusCode, last.Header.Get("Retry-After"))
	}
}

func TestDeviceTokenRateLimit(t *testing.T) {
	e := setup(t, config.Limits{KeyRate: 1000, KeyBurst: 1000, TokenPerMinute: 6}) // burst 3
	var resp *http.Response
	var out map[string]any
	for i := 0; i < 4; i++ {
		resp, out = e.post(t, "/v1/push", e.key, good())
	}
	if resp.StatusCode != 429 || out["status"] != "rate_limited" || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("%d %v", resp.StatusCode, out)
	}
	if len(e.ios.got) != 3 {
		t.Fatalf("delivered %d", len(e.ios.got))
	}
	// another device is unaffected
	r := good()
	r["token"] = strings.Repeat("ab", 32)
	if resp, _ := e.post(t, "/v1/push", e.key, r); resp.StatusCode != 200 {
		t.Fatalf("other device: %d", resp.StatusCode)
	}
}

func TestDailyQuota(t *testing.T) {
	lim := defaults
	lim.DailyQuota = 2
	e := setup(t, lim)
	e.s.Now = func() time.Time { return time.Date(2026, 10, 10, 20, 0, 0, 0, time.UTC) }
	var resp *http.Response
	var out map[string]any
	for i := 0; i < 3; i++ {
		r := good()
		r["token"] = strings.Repeat(string(rune('a'+i)), 64)
		resp, out = e.post(t, "/v1/push", e.key, r)
	}
	if resp.StatusCode != 429 || out["retryAfter"].(float64) != 4*3600 {
		t.Fatalf("%d %v", resp.StatusCode, out)
	}
	e.s.Now = func() time.Time { return time.Date(2026, 10, 11, 0, 0, 1, 0, time.UTC) }
	if resp, _ := e.post(t, "/v1/push", e.key, good()); resp.StatusCode != 200 {
		t.Fatalf("next day: %d", resp.StatusCode)
	}
}

func TestBatch(t *testing.T) {
	e := setup(t, defaults)
	bad := good()
	bad["event"] = "nope"
	gone := good()
	gone["token"] = strings.Repeat("cd", 32)
	resp, out := e.post(t, "/v1/push/batch", e.key, map[string]any{"items": []any{good(), bad, gone}})
	if resp.StatusCode != 200 {
		t.Fatalf("%d", resp.StatusCode)
	}
	items := out["items"].([]any)
	if len(items) != 3 || items[0].(map[string]any)["status"] != "sent" ||
		items[1].(map[string]any)["status"] != "error" || items[1].(map[string]any)["error"] != "unknown event" {
		t.Fatalf("%v", items)
	}
	if resp, _ := e.post(t, "/v1/push/batch", e.key, map[string]any{"items": []any{}}); resp.StatusCode != 400 {
		t.Fatalf("empty batch %d", resp.StatusCode)
	}
	many := make([]any, 101)
	for i := range many {
		many[i] = good()
	}
	if resp, _ := e.post(t, "/v1/push/batch", e.key, map[string]any{"items": many}); resp.StatusCode != 400 {
		t.Fatalf("101 items %d", resp.StatusCode)
	}
	if resp, _ := e.post(t, "/v1/push/batch", "", map[string]any{"items": []any{good()}}); resp.StatusCode != 401 {
		t.Fatalf("batch unauthenticated %d", resp.StatusCode)
	}
}

func TestUsageRecordedAndMetrics(t *testing.T) {
	e := setup(t, defaults)
	e.post(t, "/v1/push", e.key, good())
	ks, _ := e.st.List()
	if ks[0].Sent != 1 {
		t.Fatalf("%+v", ks[0])
	}
	rec := httptest.NewRecorder()
	e.s.Metrics().Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	if !strings.Contains(rec.Body.String(), `push_relay_pushes_total{app="tipsarr",platform="ios",status="sent"} 1`) {
		t.Fatalf("%s", rec.Body.String())
	}
}

func TestHealthz(t *testing.T) {
	e := setup(t, defaults)
	resp, err := http.Get(e.srv.URL + "/healthz")
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("%v %v", resp, err)
	}
}

func TestFreeTextOnlyForAppsThatAllowIt(t *testing.T) {
	e := setup(t, defaults)
	tpl, _ := templates.Parse(templates.Builtin("tipsarr"))
	free := &fakeSender{}
	e.s.Apps["free"] = &App{Templates: tpl, FreeText: true, Senders: map[string]push.Sender{"ios": free}}
	freeKey, _, _ := e.st.Add("free", "test")

	r := good()
	r["title"] = "Demande approuvée\n"
	r["body"] = "Inception\x07 " + strings.Repeat("é", 400)
	if resp, out := e.post(t, "/v1/push", freeKey, r); resp.StatusCode != 200 {
		t.Fatalf("%d %v", resp.StatusCode, out)
	}
	m := free.got[0]
	if m.Title != "Demande approuvée" || !strings.HasPrefix(m.Body, "Inception é") || len([]rune(m.Body)) != maxBodyRunes {
		t.Fatalf("title %q body %q (%d runes)", m.Title, m.Body, len([]rune(m.Body)))
	}

	// An app without free_text ignores the fields and keeps its template.
	if resp, _ := e.post(t, "/v1/push", e.key, r); resp.StatusCode != 200 {
		t.Fatal(resp.StatusCode)
	}
	if got := e.ios.got[0]; got.Title != "Demande approuvée" || strings.Contains(got.Body, "Inception") {
		t.Fatalf("%+v", got)
	}
}

func TestFreeTextIsNeverLogged(t *testing.T) {
	e := setup(t, defaults)
	tpl, _ := templates.Parse(templates.Builtin("tipsarr"))
	var logs bytes.Buffer
	s := New(e.st, map[string]*App{"free": {Templates: tpl, FreeText: true, Senders: map[string]push.Sender{"ios": &fakeSender{}}}},
		defaults, slog.New(slog.NewTextHandler(&logs, nil)))
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	key, _, _ := e.st.Add("free", "test")
	r := good()
	r["title"], r["body"] = "SECRET-TITLE", "SECRET-BODY"
	var buf bytes.Buffer
	json.NewEncoder(&buf).Encode(r)
	req, _ := http.NewRequest("POST", srv.URL+"/v1/push", &buf)
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("%v %v", err, resp)
	}
	resp.Body.Close()
	if logs.Len() == 0 || strings.Contains(logs.String(), "SECRET") || strings.Contains(logs.String(), iosTok) {
		t.Fatalf("logs: %s", logs.String())
	}
}
