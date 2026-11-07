package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Rombond/push-relay/internal/config"
)

func regEnv(t *testing.T, r config.Registration) *env {
	t.Helper()
	e := setup(t, defaults)
	e.s.Apps["tipsarr"].OpenRegistration = true
	e.s.Apps["tipsarr"].FreeText = true
	if r.PerIPPerMinute == 0 {
		r.PerIPPerMinute = 1000
	}
	e.s.SetRegistration(r)
	return e
}

func regBody(inst string) map[string]any {
	return map[string]any{"app": "tipsarr", "installId": inst, "serverUrl": "https://tipsarr.example.org", "version": "1.2.0"}
}

const inst1 = "a1b2c3d4e5f6a7b8c9d0"

func TestRegisterAndRotate(t *testing.T) {
	e := regEnv(t, config.Registration{NewKeysPerHour: 50, DailyQuota: 200})
	resp, out := e.post(t, "/v1/register", "", regBody(inst1))
	if resp.StatusCode != 201 || out["rotated"] != false || out["dailyQuota"].(float64) != 200 {
		t.Fatalf("%d %v", resp.StatusCode, out)
	}
	k1 := out["key"].(string)
	if r, _ := e.post(t, "/v1/push", k1, good()); r.StatusCode != 200 {
		t.Fatalf("new key cannot push: %d", r.StatusCode)
	}
	resp, out = e.post(t, "/v1/register", "", regBody(inst1))
	if resp.StatusCode != 200 || out["rotated"] != true || out["key"] == k1 {
		t.Fatalf("%d %v", resp.StatusCode, out)
	}
	if r, _ := e.post(t, "/v1/push", k1, good()); r.StatusCode != 401 {
		t.Fatalf("old key still works: %d", r.StatusCode)
	}
	if r, _ := e.post(t, "/v1/push", out["key"].(string), good()); r.StatusCode != 200 {
		t.Fatalf("rotated key: %d", r.StatusCode)
	}
}

func TestRegisterRefusals(t *testing.T) {
	e := regEnv(t, config.Registration{NewKeysPerHour: 50, DailyQuota: 200})
	cases := []struct {
		body   any
		status int
		reason string
	}{
		{map[string]any{"app": "other", "installId": inst1}, 403, "registration_not_allowed"},
		{map[string]any{"app": "nope", "installId": inst1}, 400, "unknown_app"},
		{map[string]any{"app": "tipsarr", "installId": "short"}, 400, "invalid_install_id"},
		{map[string]any{"app": "tipsarr", "installId": inst1, "serverUrl": "ftp://x"}, 400, "invalid_server_url"},
		{map[string]any{"app": "tipsarr", "installId": inst1, "version": "1 0"}, 400, "invalid_version"},
		{map[string]any{"app": "tipsarr", "installId": inst1, "extra": 1}, 400, "invalid_request"},
	}
	for _, c := range cases {
		resp, out := e.post(t, "/v1/register", "", c.body)
		if resp.StatusCode != c.status || out["error"] != c.reason {
			t.Errorf("%v: %d %v", c.body, resp.StatusCode, out)
		}
	}
}

func TestRegisterKillSwitch(t *testing.T) {
	e := regEnv(t, config.Registration{Closed: true, DailyQuota: 200})
	if resp, out := e.post(t, "/v1/register", "", regBody(inst1)); resp.StatusCode != 403 || out["error"] != "registration_closed" {
		t.Fatalf("config switch: %d %v", resp.StatusCode, out)
	}
	e.s.SetRegistration(config.Registration{PerIPPerMinute: 1000, DailyQuota: 200})
	if resp, _ := e.post(t, "/v1/register", "", regBody(inst1)); resp.StatusCode != 201 {
		t.Fatalf("open: %d", resp.StatusCode)
	}
	e.st.SetRegistrationClosed(true)
	if resp, out := e.post(t, "/v1/register", "", regBody(inst1)); resp.StatusCode != 403 || out["error"] != "registration_closed" {
		t.Fatalf("runtime switch: %d %v", resp.StatusCode, out)
	}
}

func TestRegisterGlobalCap(t *testing.T) {
	e := regEnv(t, config.Registration{NewKeysPerHour: 2, DailyQuota: 200})
	now := time.Date(2026, 11, 7, 10, 0, 0, 0, time.UTC)
	e.s.Now = func() time.Time { return now }
	e.post(t, "/v1/register", "", regBody(inst1))
	e.post(t, "/v1/register", "", regBody(inst1+"x"))
	resp, out := e.post(t, "/v1/register", "", regBody(inst1+"y"))
	if resp.StatusCode != 429 || out["error"] != "registration_cap" || resp.Header.Get("Retry-After") != "3600" {
		t.Fatalf("%d %v %q", resp.StatusCode, out, resp.Header.Get("Retry-After"))
	}
	if resp, _ := e.post(t, "/v1/register", "", regBody(inst1)); resp.StatusCode != 200 {
		t.Fatalf("rotation refused by the cap: %d", resp.StatusCode)
	}
	now = now.Add(time.Hour + time.Second)
	if resp, _ := e.post(t, "/v1/register", "", regBody(inst1+"y")); resp.StatusCode != 201 {
		t.Fatalf("after the window: %d", resp.StatusCode)
	}
}

func TestRegisterPerIPLimit(t *testing.T) {
	e := regEnv(t, config.Registration{NewKeysPerHour: 50, PerIPPerMinute: 2, DailyQuota: 200})
	e.post(t, "/v1/register", "", regBody(inst1))
	e.post(t, "/v1/register", "", regBody(inst1))
	resp, out := e.post(t, "/v1/register", "", regBody(inst1))
	if resp.StatusCode != 429 || out["error"] != "rate_limited" || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("%d %v", resp.StatusCode, out)
	}
}

func TestClientIPHeader(t *testing.T) {
	e := regEnv(t, config.Registration{ClientIPHeader: "X-Forwarded-For"})
	r, _ := http.NewRequest("POST", "/", nil)
	r.RemoteAddr = "10.0.0.1:5555"
	if ip := e.s.clientIP(r); ip != "10.0.0.1" {
		t.Fatal(ip)
	}
	r.Header.Set("X-Forwarded-For", "1.1.1.1, 203.0.113.9")
	if ip := e.s.clientIP(r); ip != "203.0.113.9" {
		t.Fatal(ip)
	}
}

func TestPublicKeyNeverGetsFreeText(t *testing.T) {
	e := regEnv(t, config.Registration{DailyQuota: 200})
	_, out := e.post(t, "/v1/register", "", regBody(inst1))
	key := out["key"].(string)
	r := good()
	r["title"], r["body"] = "Spam", "Buy now"
	if resp, _ := e.post(t, "/v1/push", key, r); resp.StatusCode != 200 {
		t.Fatal(resp.StatusCode)
	}
	if m := e.ios.got[0]; m.Title != "Demande approuvée" || m.Body == "Buy now" {
		t.Fatalf("public key used free text: %+v", m)
	}
	r = good()
	r["level"] = "time-sensitive"
	if resp, _ := e.post(t, "/v1/push", key, r); resp.StatusCode != 400 {
		t.Fatalf("level accepted for a public key: %d", resp.StatusCode)
	}
	// the manual key of the same app keeps free_text
	r = good()
	r["title"] = "Mine"
	e.post(t, "/v1/push", e.key, r)
	if e.ios.got[1].Title != "Mine" {
		t.Fatalf("%+v", e.ios.got[1])
	}
}

func TestPublicQuotaAndOverride(t *testing.T) {
	e := regEnv(t, config.Registration{DailyQuota: 2})
	e.s.Now = func() time.Time { return time.Date(2026, 11, 7, 20, 0, 0, 0, time.UTC) }
	_, out := e.post(t, "/v1/register", "", regBody(inst1))
	key, id := out["key"].(string), int64(out["keyId"].(float64))
	item := func(i int) map[string]any {
		r := good()
		r["token"] = strings.Repeat(string(rune('a'+i)), 64)
		return r
	}
	_, out = e.post(t, "/v1/push/batch", key, map[string]any{"items": []any{item(0), item(1), item(2)}})
	items := out["items"].([]any)
	if items[1].(map[string]any)["status"] != "sent" || items[2].(map[string]any)["status"] != "quota_exceeded" ||
		items[2].(map[string]any)["retryAfter"].(float64) != 4*3600 {
		t.Fatalf("%v", items)
	}
	// manual keys stay unlimited (limits.daily_quota = 0)
	for i := 0; i < 5; i++ {
		if resp, _ := e.post(t, "/v1/push", e.key, item(i)); resp.StatusCode != 200 {
			t.Fatalf("manual key limited: %d", resp.StatusCode)
		}
	}
	n := 3
	e.st.SetQuota(id, &n)
	if resp, _ := e.post(t, "/v1/push", key, item(3)); resp.StatusCode != 200 {
		t.Fatalf("raised quota: %d", resp.StatusCode)
	}
	if resp, out := e.post(t, "/v1/push", key, item(4)); resp.StatusCode != 429 || out["status"] != "quota_exceeded" {
		t.Fatalf("%d %v", resp.StatusCode, out)
	}
}

func TestRegistrationMetrics(t *testing.T) {
	e := regEnv(t, config.Registration{NewKeysPerHour: 1, DailyQuota: 1})
	_, out := e.post(t, "/v1/register", "", regBody(inst1))
	e.post(t, "/v1/register", "", regBody(inst1))
	e.post(t, "/v1/register", "", regBody(inst1+"x"))
	_, out = e.post(t, "/v1/register", "", regBody(inst1)) // rotation, newest key
	key := out["key"].(string)
	e.post(t, "/v1/push", key, good())
	e.post(t, "/v1/push", key, good())
	rec := httptestRecorder(t, e)
	for _, want := range []string{
		`push_relay_registrations_total{app="tipsarr"} 1`,
		`push_relay_rotations_total{app="tipsarr"} 2`,
		`push_relay_registration_refusals_total{reason="registration_cap"} 1`,
		`push_relay_quota_refusals_total{app="tipsarr"} 1`,
		`push_relay_pushes_total{app="tipsarr",platform="ios",status="quota_exceeded"} 1`,
		`push_relay_keys{kind="manual"} 1`,
		`push_relay_keys{kind="public"} 1`,
	} {
		if !strings.Contains(rec, want) {
			t.Errorf("missing %s in\n%s", want, rec)
		}
	}
}

func httptestRecorder(t *testing.T, e *env) string {
	t.Helper()
	w := httptest.NewRecorder()
	e.s.Metrics().Handler().ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil))
	return w.Body.String()
}
