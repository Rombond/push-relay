package server

import (
	"strings"
	"testing"
	"time"

	"github.com/Rombond/push-relay/internal/config"
)

func liveItem() map[string]any {
	return map[string]any{"platform": "ios", "token": iosTok, "event": "live", "id": "r1", "server": "tipsarr.example.org",
		"live": map[string]any{"stage": "downloading", "percent": 42, "etaSeconds": 1800, "priority": "low"}}
}

func TestLiveSends(t *testing.T) {
	e := setup(t, defaults)
	now := time.Unix(1800000000, 0)
	e.s.Now = func() time.Time { return now }
	r := liveItem()
	if resp, out := e.post(t, "/v1/push", e.key, r); resp.StatusCode != 200 || out["status"] != "sent" {
		t.Fatalf("%d %v", resp.StatusCode, out)
	}
	m := e.ios.got[0]
	if m.Live == nil || m.Live.Stage != "downloading" || m.Live.Percent != 42 || m.Live.Timestamp != now.Unix() || m.Live.High ||
		m.Title != "" || m.Body != "" || m.Server != "tipsarr.example.org" {
		t.Fatalf("%+v %+v", m, m.Live)
	}
	r["live"].(map[string]any)["priority"] = "high"
	r["live"].(map[string]any)["end"] = true
	r["live"].(map[string]any)["dismissAfter"] = 3600
	r["live"].(map[string]any)["timestamp"] = now.Unix() - 10
	e.post(t, "/v1/push", e.key, r)
	if m := e.ios.got[1]; !m.Live.High || !m.Live.End || m.Live.DismissAfter != 3600 || m.Live.Timestamp != now.Unix()-10 {
		t.Fatalf("%+v", m.Live)
	}
	// alert counters untouched
	if n, _ := e.st.Today(1, now); n != 0 {
		t.Fatalf("alert usage %d", n)
	}
	if n, _ := e.st.LiveToday(1, now); n != 2 {
		t.Fatalf("live usage %d", n)
	}
}

func TestLiveValidation(t *testing.T) {
	e := setup(t, defaults)
	set := func(k string, v any) map[string]any { r := liveItem(); r[k] = v; return r }
	setLive := func(k string, v any) map[string]any { r := liveItem(); r["live"].(map[string]any)[k] = v; return r }
	cases := map[string]map[string]any{
		"event":         set("event", "request.approved"),
		"title":         set("title", "Spam"),
		"body":          set("body", "Spam"),
		"level":         set("level", "passive"),
		"sound":         set("sound", "none"),
		"thread":        set("thread", "x"),
		"collapse":      set("collapse", "x"),
		"ttl":           set("ttl", 0),
		"image":         set("image", "https://x.org/a.jpg"),
		"stage":         setLive("stage", "Breaking Bad"),
		"percent<0":     setLive("percent", -1),
		"percent>100":   setLive("percent", 101),
		"eta":           setLive("etaSeconds", 2592001),
		"timestamp":     setLive("timestamp", 1),
		"dismiss":       setLive("dismissAfter", 14401),
		"priority":      setLive("priority", "urgent"),
		"unknown field": setLive("title", "x"),
	}
	for name, r := range cases {
		if resp, _ := e.post(t, "/v1/push", e.key, r); resp.StatusCode != 400 {
			t.Errorf("%s: %d", name, resp.StatusCode)
		}
	}
	if len(e.ios.got) != 0 {
		t.Fatalf("sent %d", len(e.ios.got))
	}
}

func TestLiveAllowedForPublicKeyWithSeparateQuota(t *testing.T) {
	e := regEnv(t, config.Registration{DailyQuota: 1, LiveDailyQuota: 2})
	e.s.Now = func() time.Time { return time.Date(2026, 11, 7, 20, 0, 0, 0, time.UTC) }
	_, out := e.post(t, "/v1/register", "", regBody(inst1))
	key := out["key"].(string)
	if out["liveDailyQuota"].(float64) != 2 {
		t.Fatalf("%v", out)
	}
	// alert quota used up: live still goes
	if resp, _ := e.post(t, "/v1/push", key, good()); resp.StatusCode != 200 {
		t.Fatal(resp.StatusCode)
	}
	if resp, out := e.post(t, "/v1/push", key, good()); resp.StatusCode != 429 || out["status"] != "quota_exceeded" {
		t.Fatalf("%d %v", resp.StatusCode, out)
	}
	for i := 0; i < 2; i++ {
		if resp, out := e.post(t, "/v1/push", key, liveItem()); resp.StatusCode != 200 {
			t.Fatalf("live %d: %d %v", i, resp.StatusCode, out)
		}
	}
	resp, out := e.post(t, "/v1/push", key, liveItem())
	if resp.StatusCode != 429 || out["status"] != "quota_exceeded" || !strings.Contains(out["error"].(string), "live") {
		t.Fatalf("%d %v", resp.StatusCode, out)
	}
}

func TestLiveQuotaDoesNotBlockAlerts(t *testing.T) {
	e := setup(t, config.Limits{KeyRate: 1000, KeyBurst: 1000, TokenPerMinute: 600, LiveDailyQuota: 1})
	e.post(t, "/v1/push", e.key, liveItem())
	if resp, _ := e.post(t, "/v1/push", e.key, liveItem()); resp.StatusCode != 429 {
		t.Fatal(resp.StatusCode)
	}
	if resp, _ := e.post(t, "/v1/push", e.key, good()); resp.StatusCode != 200 {
		t.Fatalf("alert blocked by live quota: %d", resp.StatusCode)
	}
}
