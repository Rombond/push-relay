package fcm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Rombond/push-relay/internal/push"
)

func TestSendPayload(t *testing.T) {
	var path string
	var body map[string]map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		json.NewDecoder(r.Body).Decode(&body)
	}))
	defer srv.Close()
	c := NewWithClient("proj", srv.URL, srv.Client())
	res := c.Send(context.Background(), push.Message{Token: "tok", Title: "T", Body: "B", Event: "request.available", ID: "7", Category: "requests", Server: "tipsarr.example.org"})
	if res.Status != push.Sent {
		t.Fatalf("%v", res)
	}
	if path != "/v1/projects/proj/messages:send" {
		t.Fatalf("path %s", path)
	}
	m := body["message"]
	notif := m["notification"].(map[string]any)
	data := m["data"].(map[string]any)
	and := m["android"].(map[string]any)
	if m["token"] != "tok" || notif["title"] != "T" || data["event"] != "request.available" || data["id"] != "7" || data["server"] != "tipsarr.example.org" ||
		and["priority"] != "high" || and["notification"].(map[string]any)["channel_id"] != "requests" {
		t.Fatalf("body %v", body)
	}
}

func TestErrorMapping(t *testing.T) {
	cases := []struct {
		status int
		body   string
		retry  string
		want   push.Status
	}{
		{404, `{"error":{"status":"NOT_FOUND","details":[{"errorCode":"UNREGISTERED"}]}}`, "", push.InvalidToken},
		{400, `{"error":{"status":"INVALID_ARGUMENT"}}`, "", push.InvalidToken},
		{403, `{"error":{"status":"PERMISSION_DENIED","details":[{"errorCode":"SENDER_ID_MISMATCH"}]}}`, "", push.InvalidToken},
		{429, `{"error":{"status":"RESOURCE_EXHAUSTED"}}`, "30", push.RateLimited},
		{503, `{"error":{"status":"UNAVAILABLE"}}`, "", push.Failed},
		{500, `not json`, "", push.Failed},
	}
	for _, tc := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if tc.retry != "" {
				w.Header().Set("Retry-After", tc.retry)
			}
			w.WriteHeader(tc.status)
			w.Write([]byte(tc.body))
		}))
		c := NewWithClient("p", srv.URL, srv.Client())
		got := c.Send(context.Background(), push.Message{Token: "t"})
		srv.Close()
		if got.Status != tc.want {
			t.Errorf("%d: got %v want %v", tc.status, got.Status, tc.want)
		}
		if tc.want == push.RateLimited && got.RetryAfter != 30 {
			t.Errorf("retryAfter %d", got.RetryAfter)
		}
	}
}

func TestNewRejectsBadCredentials(t *testing.T) {
	if _, err := New(context.Background(), "p", []byte("{}")); err == nil {
		t.Fatal("want error")
	}
}
