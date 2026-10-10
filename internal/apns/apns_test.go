package apns

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Rombond/push-relay/internal/push"
)

func newKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// fakeAPNs is an HTTP/2 TLS server like Apple's. status and reason drive the reply.
func fakeAPNs(t *testing.T, key *ecdsa.PrivateKey, handler func(w http.ResponseWriter, r *http.Request)) *httptest.Server {
	t.Helper()
	srv := httptest.NewUnstartedServer(http.HandlerFunc(handler))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}

func verifyJWT(t *testing.T, header string, key *ecdsa.PublicKey, team, kid string) {
	t.Helper()
	tok, ok := strings.CutPrefix(header, "bearer ")
	if !ok {
		t.Fatalf("authorization %q", header)
	}
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("jwt parts %d", len(parts))
	}
	var h map[string]string
	b, _ := base64.RawURLEncoding.DecodeString(parts[0])
	json.Unmarshal(b, &h)
	if h["alg"] != "ES256" || h["kid"] != kid {
		t.Fatalf("jwt header %v", h)
	}
	var c map[string]any
	b, _ = base64.RawURLEncoding.DecodeString(parts[1])
	json.Unmarshal(b, &c)
	if c["iss"] != team {
		t.Fatalf("jwt claims %v", c)
	}
	sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if len(sig) != 64 || !ecdsa.Verify(key, digest[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])) {
		t.Fatal("jwt signature invalid")
	}
}

func TestSendPayloadAndHeaders(t *testing.T) {
	key := newKey(t)
	var gotPath, gotTopic, gotType string
	var body map[string]any
	srv := fakeAPNs(t, key, func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor != 2 {
			t.Errorf("proto %s", r.Proto)
		}
		gotPath, gotTopic, gotType = r.URL.Path, r.Header.Get("apns-topic"), r.Header.Get("apns-push-type")
		verifyJWT(t, r.Header.Get("authorization"), &key.PublicKey, "TEAM", "KID")
		json.NewDecoder(r.Body).Decode(&body)
	})
	c := New(Config{BundleID: "com.x.app", TeamID: "TEAM", KeyID: "KID", Key: key, ProdURL: srv.URL, Client: srv.Client()})
	res := c.Send(context.Background(), push.Message{Token: "abcd", Title: "T", Body: "B", Event: "request.approved", ID: "42", Server: "tipsarr.example.org"})
	if res.Status != push.Sent {
		t.Fatalf("status %v", res)
	}
	if gotPath != "/3/device/abcd" || gotTopic != "com.x.app" || gotType != "alert" {
		t.Fatalf("path %s topic %s type %s", gotPath, gotTopic, gotType)
	}
	a := body["aps"].(map[string]any)
	al := a["alert"].(map[string]any)
	if al["title"] != "T" || al["body"] != "B" || a["thread-id"] != "42" || body["event"] != "request.approved" || body["id"] != "42" || body["server"] != "tipsarr.example.org" {
		t.Fatalf("body %v", body)
	}
}

func TestSandboxUsesSandboxURL(t *testing.T) {
	key := newKey(t)
	hit := ""
	prod := fakeAPNs(t, key, func(w http.ResponseWriter, r *http.Request) { hit = "prod" })
	sand := fakeAPNs(t, key, func(w http.ResponseWriter, r *http.Request) { hit = "sandbox" })
	// both fake servers use different self signed certs, trust both
	pool := prod.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs
	pool.AddCert(sand.Certificate())
	c := New(Config{BundleID: "b", TeamID: "t", KeyID: "k", Key: key, ProdURL: prod.URL, SandboxURL: sand.URL, Client: prod.Client()})
	c.Send(context.Background(), push.Message{Token: "ab", Sandbox: true})
	if hit != "sandbox" {
		t.Fatalf("hit %s", hit)
	}
}

func TestErrorMapping(t *testing.T) {
	key := newKey(t)
	cases := []struct {
		status int
		reason string
		want   push.Status
	}{
		{410, "Unregistered", push.InvalidToken},
		{400, "BadDeviceToken", push.InvalidToken},
		{400, "DeviceTokenNotForTopic", push.InvalidToken},
		{429, "TooManyRequests", push.RateLimited},
		{500, "InternalServerError", push.Failed},
		{403, "InvalidProviderToken", push.Failed},
	}
	for _, tc := range cases {
		srv := fakeAPNs(t, key, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.status)
			json.NewEncoder(w).Encode(map[string]string{"reason": tc.reason})
		})
		c := New(Config{BundleID: "b", TeamID: "t", KeyID: "k", Key: key, ProdURL: srv.URL, Client: srv.Client()})
		if got := c.Send(context.Background(), push.Message{Token: "ab"}); got.Status != tc.want {
			t.Errorf("%d %s: got %v want %v", tc.status, tc.reason, got.Status, tc.want)
		}
	}
}

func TestExpiredProviderTokenRetriesOnceWithFreshJWT(t *testing.T) {
	key := newKey(t)
	calls := 0
	var seen []string
	srv := fakeAPNs(t, key, func(w http.ResponseWriter, r *http.Request) {
		calls++
		seen = append(seen, r.Header.Get("authorization"))
		if calls == 1 {
			w.WriteHeader(403)
			json.NewEncoder(w).Encode(map[string]string{"reason": "ExpiredProviderToken"})
		}
	})
	now := time.Unix(1_700_000_000, 0)
	c := New(Config{BundleID: "b", TeamID: "t", KeyID: "k", Key: key, ProdURL: srv.URL, Client: srv.Client(),
		Now: func() time.Time { now = now.Add(time.Second); return now }})
	if res := c.Send(context.Background(), push.Message{Token: "ab"}); res.Status != push.Sent {
		t.Fatalf("%v", res)
	}
	if calls != 2 || seen[0] == seen[1] {
		t.Fatalf("calls %d, same jwt %v", calls, seen[0] == seen[1])
	}
}

func TestJWTCachedThenRefreshed(t *testing.T) {
	key := newKey(t)
	now := time.Unix(1_700_000_000, 0)
	c := New(Config{TeamID: "t", KeyID: "k", Key: key, Now: func() time.Time { return now }})
	a, _ := c.jwt(false)
	b, _ := c.jwt(false)
	if a != b {
		t.Fatal("not cached")
	}
	now = now.Add(51 * time.Minute)
	d, _ := c.jwt(false)
	if d == a {
		t.Fatal("not refreshed after 51 minutes")
	}
}

func TestParseKeyRejectsGarbage(t *testing.T) {
	if _, err := ParseKey([]byte("nope")); err == nil {
		t.Fatal("want error")
	}
}

func TestUnreachableIsFailed(t *testing.T) {
	key := newKey(t)
	c := New(Config{BundleID: "b", TeamID: "t", KeyID: "k", Key: key, ProdURL: "https://127.0.0.1:1"})
	if res := c.Send(context.Background(), push.Message{Token: "ab"}); res.Status != push.Failed {
		t.Fatalf("%v", res)
	}
}
