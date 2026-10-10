// Package apns sends notifications to Apple's push service with token based authentication.
package apns

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/Rombond/push-relay/internal/push"
)

const (
	ProdURL    = "https://api.push.apple.com"
	SandboxURL = "https://api.sandbox.push.apple.com"
	jwtTTL     = 50 * time.Minute // Apple wants a refresh between 20 and 60 minutes
)

type Config struct {
	BundleID   string
	TeamID     string
	KeyID      string
	Key        *ecdsa.PrivateKey
	ProdURL    string // default ProdURL
	SandboxURL string // default SandboxURL
	Client     *http.Client
	Now        func() time.Time
}

type Client struct {
	cfg Config

	mu    sync.Mutex
	token string
	made  time.Time
}

func New(cfg Config) *Client {
	if cfg.ProdURL == "" {
		cfg.ProdURL = ProdURL
	}
	if cfg.SandboxURL == "" {
		cfg.SandboxURL = SandboxURL
	}
	if cfg.Client == nil {
		// The default transport negotiates HTTP/2 with Apple on its own.
		cfg.Client = &http.Client{Timeout: 15 * time.Second}
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Client{cfg: cfg}
}

// ParseKey reads the .p8 file downloaded from the Apple developer portal (PKCS8 EC key).
func ParseKey(data []byte) (*ecdsa.PrivateKey, error) {
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, errors.New("no PEM block in APNs key")
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse APNs key: %w", err)
	}
	ec, ok := k.(*ecdsa.PrivateKey)
	if !ok {
		return nil, errors.New("APNs key is not an EC key")
	}
	return ec, nil
}

func (c *Client) jwt(force bool) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.cfg.Now()
	if !force && c.token != "" && now.Sub(c.made) < jwtTTL {
		return c.token, nil
	}
	enc := base64.RawURLEncoding
	header, _ := json.Marshal(map[string]string{"alg": "ES256", "kid": c.cfg.KeyID})
	claims, _ := json.Marshal(map[string]any{"iss": c.cfg.TeamID, "iat": now.Unix()})
	signing := enc.EncodeToString(header) + "." + enc.EncodeToString(claims)
	digest := sha256.Sum256([]byte(signing))
	r, s, err := ecdsa.Sign(rand.Reader, c.cfg.Key, digest[:])
	if err != nil {
		return "", err
	}
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	c.token = signing + "." + enc.EncodeToString(sig)
	c.made = now
	return c.token, nil
}

type payload struct {
	APS    aps    `json:"aps"`
	Event  string `json:"event"`
	ID     string `json:"id"`
	Server string `json:"server,omitempty"`
}

type aps struct {
	Alert    alert  `json:"alert"`
	Sound    string `json:"sound"`
	ThreadID string `json:"thread-id,omitempty"`
}

type alert struct {
	Title string `json:"title"`
	Body  string `json:"body"`
}

func (c *Client) Send(ctx context.Context, m push.Message) push.Result {
	body, _ := json.Marshal(payload{
		APS:    aps{Alert: alert{Title: m.Title, Body: m.Body}, Sound: "default", ThreadID: m.ID},
		Event:  m.Event,
		ID:     m.ID,
		Server: m.Server,
	})
	for attempt := 0; ; attempt++ {
		res, expired := c.do(ctx, m, body, attempt > 0)
		if expired && attempt == 0 {
			continue
		}
		return res
	}
}

// do returns expired=true when Apple rejected the provider token, so the caller retries once with a fresh one.
func (c *Client) do(ctx context.Context, m push.Message, body []byte, forceJWT bool) (push.Result, bool) {
	tok, err := c.jwt(forceJWT)
	if err != nil {
		return push.Result{Status: push.Failed, Error: "sign provider token"}, false
	}
	base := c.cfg.ProdURL
	if m.Sandbox {
		base = c.cfg.SandboxURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/3/device/"+m.Token, bytes.NewReader(body))
	if err != nil {
		return push.Result{Status: push.Failed, Error: "build request"}, false
	}
	req.Header.Set("authorization", "bearer "+tok)
	req.Header.Set("apns-topic", c.cfg.BundleID)
	req.Header.Set("apns-push-type", "alert")
	req.Header.Set("apns-priority", "10")
	req.Header.Set("apns-expiration", strconv.FormatInt(c.cfg.Now().Add(24*time.Hour).Unix(), 10))
	req.Header.Set("content-type", "application/json")

	resp, err := c.cfg.Client.Do(req)
	if err != nil {
		return push.Result{Status: push.Failed, Error: "apns unreachable"}, false
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		io.Copy(io.Discard, resp.Body)
		return push.Result{Status: push.Sent}, false
	}
	var fail struct {
		Reason string `json:"reason"`
	}
	json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&fail)

	switch {
	case fail.Reason == "ExpiredProviderToken":
		return push.Result{Status: push.Failed, Error: fail.Reason}, true
	case resp.StatusCode == http.StatusGone, fail.Reason == "Unregistered", fail.Reason == "BadDeviceToken", fail.Reason == "DeviceTokenNotForTopic":
		return push.Result{Status: push.InvalidToken}, false
	case resp.StatusCode == http.StatusTooManyRequests:
		return push.Result{Status: push.RateLimited, RetryAfter: 60}, false
	}
	msg := fail.Reason
	if msg == "" {
		msg = "apns status " + strconv.Itoa(resp.StatusCode)
	}
	return push.Result{Status: push.Failed, Error: msg}, false
}
