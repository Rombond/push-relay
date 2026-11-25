package apns

import (
	"context"
	"io"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/Rombond/push-relay/internal/push"
)

// Golden bodies: the keys must match the app's RequestActivityAttributes.ContentState.
func TestLivePayloadGolden(t *testing.T) {
	up := buildLivePayload(&push.Live{Stage: "downloading", Percent: 42, ETASeconds: 1800, Timestamp: 1800000000})
	want := `{"aps":{"timestamp":1800000000,"event":"update","content-state":{"stage":"downloading","percent":42,"etaSeconds":1800,"updatedAt":1800000000}}}`
	if string(up) != want {
		t.Fatalf("update\n got %s\nwant %s", up, want)
	}
	end := buildLivePayload(&push.Live{Stage: "available", Percent: 100, Timestamp: 1800000000, End: true, DismissAfter: 3600})
	want = `{"aps":{"timestamp":1800000000,"event":"end","content-state":{"stage":"available","percent":100,"etaSeconds":0,"updatedAt":1800000000},"dismissal-date":1800003600}}`
	if string(end) != want {
		t.Fatalf("end\n got %s\nwant %s", end, want)
	}
}

func TestLiveHeaders(t *testing.T) {
	key := newKey(t)
	now := time.Unix(1800000000, 0)
	var h http.Header
	var body []byte
	srv := fakeAPNs(t, key, func(w http.ResponseWriter, r *http.Request) {
		h = r.Header.Clone()
		body, _ = io.ReadAll(r.Body)
	})
	c := New(Config{BundleID: "com.x.app", TeamID: "t", KeyID: "k", Key: key, ProdURL: srv.URL, Client: srv.Client(), Now: func() time.Time { return now }})
	cases := []struct {
		live       push.Live
		prio       string
		expiration time.Duration
	}{
		{push.Live{Stage: "downloading", Percent: 5, Timestamp: now.Unix()}, "5", time.Hour},
		{push.Live{Stage: "approved", Timestamp: now.Unix(), High: true}, "10", time.Hour},
		{push.Live{Stage: "available", Percent: 100, Timestamp: now.Unix(), End: true, High: true}, "10", 4 * time.Hour},
	}
	for _, tc := range cases {
		l := tc.live
		if res := c.Send(context.Background(), push.Message{Token: "abcd", Event: "live", ID: "1", Live: &l}); res.Status != push.Sent {
			t.Fatalf("%v", res)
		}
		if h.Get("apns-push-type") != "liveactivity" || h.Get("apns-topic") != "com.x.app.push-type.liveactivity" ||
			h.Get("apns-priority") != tc.prio || h.Get("apns-collapse-id") != "" ||
			h.Get("apns-expiration") != strconv.FormatInt(now.Add(tc.expiration).Unix(), 10) {
			t.Fatalf("headers %v", h)
		}
		if string(body) != string(buildLivePayload(&l)) {
			t.Fatalf("body %s", body)
		}
	}
}
