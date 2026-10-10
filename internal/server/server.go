// Package server is the relay's HTTP API: POST /v1/push, POST /v1/push/batch, GET /healthz.
package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"golang.org/x/time/rate"

	"github.com/Rombond/push-relay/internal/config"
	"github.com/Rombond/push-relay/internal/limit"
	"github.com/Rombond/push-relay/internal/push"
	"github.com/Rombond/push-relay/internal/store"
	"github.com/Rombond/push-relay/internal/templates"
)

const (
	maxSingleBody = 4 << 10
	maxBatchBody  = 64 << 10
	maxBatchItems = 100
)

var (
	serverRe   = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.-]{0,251}[A-Za-z0-9])?(:[0-9]{1,5})?$`)
	idRe       = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	iosTokenRe = regexp.MustCompile(`^[0-9a-fA-F]{64,200}$`)
	androidRe  = regexp.MustCompile(`^[A-Za-z0-9:_-]{20,1000}$`)
)

// App is one registered application: its texts and one sender per platform ("ios", "android").
type App struct {
	Templates *templates.Set
	Senders   map[string]push.Sender
}

type Server struct {
	Store  *store.Store
	Apps   map[string]*App
	Log    *slog.Logger
	Now    func() time.Time
	Quota  int
	keyLim *limit.Limiter
	tokLim *limit.Limiter
	met    *Metrics
}

func New(st *store.Store, apps map[string]*App, lim config.Limits, log *slog.Logger) *Server {
	perMin := lim.TokenPerMinute
	return &Server{
		Store:  st,
		Apps:   apps,
		Log:    log,
		Now:    time.Now,
		Quota:  lim.DailyQuota,
		keyLim: limit.New(rate.Limit(lim.KeyRate), lim.KeyBurst),
		tokLim: limit.New(rate.Limit(float64(perMin)/60), max(3, perMin/2)),
		met:    newMetrics(),
	}
}

func (s *Server) Metrics() *Metrics { return s.met }

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/push", s.handlePush)
	mux.HandleFunc("POST /v1/push/batch", s.handleBatch)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	return mux
}

// Request is the fixed message shape. Unknown fields are rejected.
type Request struct {
	App      string `json:"app"`
	Platform string `json:"platform"`
	Token    string `json:"token"`
	Sandbox  bool   `json:"sandbox"`
	Event    string `json:"event"`
	ID       string `json:"id"`
	Lang     string `json:"lang"`
	Server   string `json:"server"` // optional: host (and port) of the Tipsarr server
}

type httpError struct {
	status int
	msg    string
}

func (e *httpError) Error() string { return e.msg }

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func decode(w http.ResponseWriter, r *http.Request, max int64, v any) error {
	r.Body = http.MaxBytesReader(w, r.Body, max)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			return &httpError{http.StatusRequestEntityTooLarge, "body too large"}
		}
		return &httpError{http.StatusBadRequest, "invalid JSON body"}
	}
	if _, err := dec.Token(); err != io.EOF {
		return &httpError{http.StatusBadRequest, "invalid JSON body"}
	}
	return nil
}

// authenticate checks the bearer key and the per-key rate limit.
func (s *Server) authenticate(r *http.Request) (store.Key, *httpError, time.Duration) {
	h := r.Header.Get("Authorization")
	raw, ok := strings.CutPrefix(h, "Bearer ")
	if !ok || raw == "" {
		return store.Key{}, &httpError{http.StatusUnauthorized, "unauthorized"}, 0
	}
	key, err := s.Store.Lookup(raw)
	if errors.Is(err, store.ErrNotFound) {
		return store.Key{}, &httpError{http.StatusUnauthorized, "unauthorized"}, 0
	}
	if err != nil {
		s.Log.Error("key lookup", "err", err)
		return store.Key{}, &httpError{http.StatusInternalServerError, "internal error"}, 0
	}
	if ok, wait := s.keyLim.Allow(strconv.FormatInt(key.ID, 10)); !ok {
		return store.Key{}, &httpError{http.StatusTooManyRequests, "rate limit"}, wait
	}
	return key, nil, 0
}

func retrySeconds(d time.Duration) int { return max(1, int((d+time.Second-1)/time.Second)) }

func (s *Server) handlePush(w http.ResponseWriter, r *http.Request) {
	key, herr, wait := s.authenticate(r)
	if herr != nil {
		if herr.status == http.StatusTooManyRequests {
			w.Header().Set("Retry-After", strconv.Itoa(retrySeconds(wait)))
		}
		writeErr(w, herr.status, herr.msg)
		return
	}
	var req Request
	if err := decode(w, r, maxSingleBody, &req); err != nil {
		var he *httpError
		errors.As(err, &he)
		writeErr(w, he.status, he.msg)
		return
	}
	res, verr := s.process(r.Context(), key, req)
	if verr != nil {
		writeErr(w, verr.status, verr.msg)
		return
	}
	switch res.Status {
	case push.RateLimited:
		w.Header().Set("Retry-After", strconv.Itoa(res.RetryAfter))
		writeJSON(w, http.StatusTooManyRequests, res)
	case push.Failed:
		writeJSON(w, http.StatusBadGateway, res)
	default:
		writeJSON(w, http.StatusOK, res)
	}
}

func (s *Server) handleBatch(w http.ResponseWriter, r *http.Request) {
	key, herr, wait := s.authenticate(r)
	if herr != nil {
		if herr.status == http.StatusTooManyRequests {
			w.Header().Set("Retry-After", strconv.Itoa(retrySeconds(wait)))
		}
		writeErr(w, herr.status, herr.msg)
		return
	}
	var body struct {
		Items []Request `json:"items"`
	}
	if err := decode(w, r, maxBatchBody, &body); err != nil {
		var he *httpError
		errors.As(err, &he)
		writeErr(w, he.status, he.msg)
		return
	}
	if len(body.Items) == 0 || len(body.Items) > maxBatchItems {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("items must hold 1 to %d entries", maxBatchItems))
		return
	}
	out := make([]push.Result, len(body.Items))
	for i, it := range body.Items {
		res, verr := s.process(r.Context(), key, it)
		if verr != nil {
			res = push.Result{Status: push.Failed, Error: verr.msg}
		}
		out[i] = res
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}

func tokenHash(platform, token string) string {
	h := sha256.Sum256([]byte(platform + ":" + token))
	return hex.EncodeToString(h[:])
}

func (s *Server) validate(key store.Key, req *Request) (*App, *httpError) {
	if req.App == "" {
		req.App = key.App
	}
	if req.App != key.App {
		return nil, &httpError{http.StatusForbidden, "key is not valid for this app"}
	}
	app := s.Apps[req.App]
	if app == nil {
		return nil, &httpError{http.StatusBadRequest, "unknown app"}
	}
	switch req.Platform {
	case "ios":
		if !iosTokenRe.MatchString(req.Token) {
			return nil, &httpError{http.StatusBadRequest, "invalid token"}
		}
	case "android":
		if !androidRe.MatchString(req.Token) {
			return nil, &httpError{http.StatusBadRequest, "invalid token"}
		}
	default:
		return nil, &httpError{http.StatusBadRequest, "platform must be ios or android"}
	}
	if app.Senders[req.Platform] == nil {
		return nil, &httpError{http.StatusBadRequest, "platform not configured for this app"}
	}
	if !idRe.MatchString(req.ID) {
		return nil, &httpError{http.StatusBadRequest, "invalid id"}
	}
	if req.Server != "" && !serverRe.MatchString(req.Server) {
		return nil, &httpError{http.StatusBadRequest, "invalid server"}
	}
	return app, nil
}

// process validates one request, applies the limits, delivers it and records the outcome.
// A non-nil *httpError means the request itself is wrong (nothing was sent).
func (s *Server) process(ctx context.Context, key store.Key, req Request) (push.Result, *httpError) {
	app, herr := s.validate(key, &req)
	if herr != nil {
		return push.Result{}, herr
	}
	text, category, ok := app.Templates.Lookup(req.Event, req.Lang)
	if !ok {
		return push.Result{}, &httpError{http.StatusBadRequest, "unknown event"}
	}
	th := tokenHash(req.Platform, req.Token)
	log := s.Log.With("key", key.ID, "app", req.App, "platform", req.Platform, "event", req.Event, "device", th[:8])

	now := s.Now()
	if s.Quota > 0 {
		n, err := s.Store.Today(key.ID, now)
		if err != nil {
			log.Error("quota", "err", err)
			return push.Result{}, &httpError{http.StatusInternalServerError, "internal error"}
		}
		if n >= s.Quota {
			next := time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, time.UTC)
			res := push.Result{Status: push.RateLimited, RetryAfter: retrySeconds(next.Sub(now)), Error: "daily quota reached"}
			s.met.count(req.App, req.Platform, res.Status)
			return res, nil
		}
	}
	if ok, wait := s.tokLim.Allow(th); !ok {
		res := push.Result{Status: push.RateLimited, RetryAfter: retrySeconds(wait)}
		s.met.count(req.App, req.Platform, res.Status)
		return res, nil
	}

	res := app.Senders[req.Platform].Send(ctx, push.Message{
		Token: req.Token, Sandbox: req.Sandbox,
		Title: text.Title, Body: text.Body,
		Event: req.Event, ID: req.ID, Category: category, Server: req.Server,
	})
	// A device that went away is a normal outcome, not a failure of the relay.
	if err := s.Store.Record(key.ID, now, res.Status != push.Failed); err != nil {
		log.Error("record usage", "err", err)
	}
	s.met.count(req.App, req.Platform, res.Status)
	log.Info("push", "status", res.Status, "error", res.Error)
	return res, nil
}
