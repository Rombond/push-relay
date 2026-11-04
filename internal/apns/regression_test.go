package apns

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Rombond/push-relay/internal/push"
)

// A Tipsarr push as v0.2.0 sent it must reach Apple byte for byte the same in later versions:
// the new optional fields (level, sound, thread, collapse, ttl, image) change nothing when absent.
func TestTipsarrPayloadUnchangedSinceV020(t *testing.T) {
	key := newKey(t)
	now := time.Unix(1_800_000_000, 0)
	var body []byte
	var hdr http.Header
	srv := fakeAPNs(t, key, func(w http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
		hdr = r.Header.Clone()
	})
	c := New(Config{BundleID: "com.brebond.tipsarr-app", TeamID: "TEAM", KeyID: "KID", Key: key,
		ProdURL: srv.URL, SandboxURL: srv.URL, Client: srv.Client(), Now: func() time.Time { return now }})
	res := c.Send(context.Background(), push.Message{Token: "abcd", Title: "Disponible", Body: "Dune est prête à regarder.",
		Event: "request.available", ID: "42", Category: "requests", Server: "tipsarr.example.org"})
	if res.Status != push.Sent {
		t.Fatalf("%v", res)
	}
	const want = `{"aps":{"alert":{"title":"Disponible","body":"Dune est prête à regarder."},"sound":"default","thread-id":"42"},"event":"request.available","id":"42","server":"tipsarr.example.org"}`
	if string(body) != want {
		t.Fatalf("payload changed:\n got %s\nwant %s", body, want)
	}
	for k, v := range map[string]string{"apns-topic": "com.brebond.tipsarr-app", "apns-push-type": "alert", "apns-priority": "10",
		"apns-expiration": "1800086400", "content-type": "application/json", "apns-collapse-id": ""} {
		if hdr.Get(k) != v {
			t.Fatalf("header %s = %q, want %q", k, hdr.Get(k), v)
		}
	}
}

func TestPresentationFields(t *testing.T) {
	key := newKey(t)
	now := time.Unix(1_800_000_000, 0)
	var body []byte
	var hdr http.Header
	srv := fakeAPNs(t, key, func(w http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
		hdr = r.Header.Clone()
	})
	c := New(Config{BundleID: "com.brebond.jarvis", TeamID: "TEAM", KeyID: "KID", Key: key,
		ProdURL: srv.URL, SandboxURL: srv.URL, Client: srv.Client(), Now: func() time.Time { return now }})
	ttl := 3600
	c.Send(context.Background(), push.Message{Token: "abcd", Title: "IMAP", Body: "bloqué", Event: "alert.high", ID: "7",
		Level: "passive", Silent: true, Thread: "watchdogs", Collapse: "imap", TTL: &ttl, Image: "https://img.example.org/a.png"})
	const want = `{"aps":{"alert":{"title":"IMAP","body":"bloqué"},"thread-id":"watchdogs","mutable-content":1,"interruption-level":"passive"},"event":"alert.high","id":"7","image":"https://img.example.org/a.png"}`
	if string(body) != want {
		t.Fatalf("payload:\n got %s\nwant %s", body, want)
	}
	if hdr.Get("apns-collapse-id") != "imap" || hdr.Get("apns-expiration") != "1800003600" {
		t.Fatalf("headers %v", hdr)
	}
	zero := 0
	c.Send(context.Background(), push.Message{Token: "abcd", Event: "timer.done", ID: "8", Level: "time-sensitive", TTL: &zero})
	if hdr.Get("apns-expiration") != "0" || !strings.Contains(string(body), `"sound":"default","thread-id":"8","interruption-level":"time-sensitive"`) {
		t.Fatalf("time-sensitive: %s %v", body, hdr)
	}
}
