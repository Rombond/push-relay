package fcm

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Rombond/push-relay/internal/push"
)

func TestLiveGolden(t *testing.T) {
	got := buildLive(push.Message{Token: "tok", Event: "live", ID: "r1", Server: "tipsarr.example.org",
		Live: &push.Live{Stage: "downloading", Percent: 42, ETASeconds: 1800, Timestamp: 1800000000}})
	want := `{"message":{"token":"tok","data":{"dismissAfter":"0","end":"false","etaSeconds":"1800","event":"live","id":"r1","percent":"42","server":"tipsarr.example.org","stage":"downloading","updatedAt":"1800000000"},"android":{"priority":"normal","collapse_key":"live-r1","ttl":"3600s"}}}`
	if string(got) != want {
		t.Fatalf("\n got %s\nwant %s", got, want)
	}
	end := string(buildLive(push.Message{Token: "tok", Event: "live", ID: "r1",
		Live: &push.Live{Stage: "available", Percent: 100, Timestamp: 1800000000, End: true, DismissAfter: 3600, High: true}}))
	for _, s := range []string{`"priority":"high"`, `"ttl":"14400s"`, `"end":"true"`, `"dismissAfter":"3600"`} {
		if !strings.Contains(end, s) {
			t.Fatalf("missing %s in %s", s, end)
		}
	}
	if strings.Contains(end, "server") {
		t.Fatalf("empty server sent: %s", end)
	}
}

func TestLiveIsDataOnly(t *testing.T) {
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body = string(b)
	}))
	defer srv.Close()
	c := NewWithClient("proj", srv.URL, srv.Client())
	res := c.Send(context.Background(), push.Message{Token: "tok", Event: "live", ID: "r1", Live: &push.Live{Stage: "approved", High: true}})
	if res.Status != push.Sent {
		t.Fatalf("%v", res)
	}
	if strings.Contains(body, "notification") {
		t.Fatalf("live message has a notification block: %s", body)
	}
}
