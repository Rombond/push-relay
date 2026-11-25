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

// Field order and omitempty keep a v0.2.0 message byte for byte identical (regression_test.go).
type payload struct {
	APS    aps    `json:"aps"`
	Event  string `json:"event"`
	ID     string `json:"id"`
	Server string `json:"server,omitempty"`
	Image  string `json:"image,omitempty"` // read by the app's Notification Service Extension
}

type aps struct {
	Alert             alert  `json:"alert"`
	Sound             string `json:"sound,omitempty"`
	ThreadID          string `json:"thread-id,omitempty"`
	MutableContent    int    `json:"mutable-content,omitempty"`
	InterruptionLevel string `json:"interruption-level,omitempty"`
}

type alert struct {
	Title string `json:"title"`
	Body  string `json:"body"`
}

func buildPayload(m push.Message) []byte {
	a := aps{Alert: alert{Title: m.Title, Body: m.Body}, Sound: "default", ThreadID: m.ID, InterruptionLevel: m.Level}
	if m.Silent {
		a.Sound = ""
	}
	if m.Thread != "" {
		a.ThreadID = m.Thread
	}
	if m.Image != "" {
		a.MutableContent = 1
	}
	body, _ := json.Marshal(payload{APS: a, Event: m.Event, ID: m.ID, Server: m.Server, Image: m.Image})
	return body
}

// Live Activity payload (v1.1.0): content-state only, no alert. Keys match the app's ActivityAttributes.ContentState.
type livePayload struct {
	APS liveAPS `json:"aps"`
}

type liveAPS struct {
	Timestamp     int64        `json:"timestamp"`
	Event         string       `json:"event"`
	ContentState  contentState `json:"content-state"`
	DismissalDate *int64       `json:"dismissal-date,omitempty"`
}

type contentState struct {
	Stage      string `json:"stage"`
	Percent    int    `json:"percent"`
	ETASeconds int    `json:"etaSeconds"`
	UpdatedAt  int64  `json:"updatedAt"`
}

func buildLivePayload(l *push.Live) []byte {
	a := liveAPS{Timestamp: l.Timestamp, Event: "update",
		ContentState: contentState{Stage: l.Stage, Percent: l.Percent, ETASeconds: l.ETASeconds, UpdatedAt: l.Timestamp}}
	if l.End {
		a.Event = "end"
		d := l.Timestamp + int64(l.DismissAfter)
		a.DismissalDate = &d
	}
	body, _ := json.Marshal(livePayload{APS: a})
	return body
}

func (c *Client) Send(ctx context.Context, m push.Message) push.Result {
	body := buildPayload(m)
	if m.Live != nil {
		body = buildLivePayload(m.Live)
	}
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
	if m.Live != nil {
		prio, keep := "5", time.Hour
		if m.Live.High {
			prio = "10"
		}
		if m.Live.End {
			keep = 4 * time.Hour
		}
		req.Header.Set("apns-topic", c.cfg.BundleID+".push-type.liveactivity")
		req.Header.Set("apns-push-type", "liveactivity")
		req.Header.Set("apns-priority", prio)
		req.Header.Set("apns-expiration", strconv.FormatInt(c.cfg.Now().Add(keep).Unix(), 10))
	} else {
		c.alertHeaders(req, m)
	}
	req.Header.Set("content-type", "application/json")
	return c.exchange(req)
}

func (c *Client) alertHeaders(req *http.Request, m push.Message) {
	req.Header.Set("apns-topic", c.cfg.BundleID)
	req.Header.Set("apns-push-type", "alert")
	req.Header.Set("apns-priority", "10")
	expiry := strconv.FormatInt(c.cfg.Now().Add(24*time.Hour).Unix(), 10)
	if m.TTL != nil {
		expiry = "0" // deliver now or never
		if *m.TTL > 0 {
			expiry = strconv.FormatInt(c.cfg.Now().Add(time.Duration(*m.TTL)*time.Second).Unix(), 10)
		}
	}
	req.Header.Set("apns-expiration", expiry)
	if m.Collapse != "" {
		req.Header.Set("apns-collapse-id", m.Collapse)
	}
}

func (c *Client) exchange(req *http.Request) (push.Result, bool) {
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
