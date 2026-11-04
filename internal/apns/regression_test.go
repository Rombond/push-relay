package apns

import (
	"context"
	"io"
	"net/http"
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
