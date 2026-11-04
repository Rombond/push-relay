package fcm

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Rombond/push-relay/internal/push"
)

// Same guarantee as the APNs one: a v0.2.0 Tipsarr message produces the same FCM body.
func TestTipsarrPayloadUnchangedSinceV020(t *testing.T) {
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
	}))
	defer srv.Close()
	c := NewWithClient("proj", srv.URL, srv.Client())
	res := c.Send(context.Background(), push.Message{Token: "tok", Title: "Disponible", Body: "Prête.", Event: "request.available",
		ID: "42", Category: "requests", Server: "tipsarr.example.org"})
	if res.Status != push.Sent {
		t.Fatalf("%v", res)
	}
	const want = `{"message":{"token":"tok","notification":{"title":"Disponible","body":"Prête."},"data":{"event":"request.available","id":"42","server":"tipsarr.example.org"},"android":{"priority":"high","notification":{"channel_id":"requests","tag":"42"},"ttl":"86400s"}}}`
	if string(body) != want {
		t.Fatalf("payload changed:\n got %s\nwant %s", body, want)
	}
}
