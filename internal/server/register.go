package server

import (
	"errors"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"golang.org/x/time/rate"

	"github.com/Rombond/push-relay/internal/config"
	"github.com/Rombond/push-relay/internal/limit"
	"github.com/Rombond/push-relay/internal/store"
)

var (
	installRe = regexp.MustCompile(`^[A-Za-z0-9_-]{16,128}$`)
	versionRe = regexp.MustCompile(`^[A-Za-z0-9._+-]{1,32}$`)
)

const maxServerURL = 256

// SetRegistration applies the registration settings (defaults come from config.Load).
func (s *Server) SetRegistration(r config.Registration) {
	s.reg = r
	s.regLim = limit.New(rate.Limit(float64(r.PerIPPerMinute)/60), r.PerIPPerMinute)
}

// RegisterRequest is the body of POST /v1/register.
type RegisterRequest struct {
	App       string `json:"app"`
	InstallID string `json:"installId"`
	ServerURL string `json:"serverUrl"`
	Version   string `json:"version"`
}

// RegisterResponse: the raw key is shown once.
type RegisterResponse struct {
	Key        string `json:"key"`
	KeyID      int64  `json:"keyId"`
	App        string `json:"app"`
	Rotated    bool   `json:"rotated"`
	DailyQuota int    `json:"dailyQuota"` // 0 = unlimited
}

func (s *Server) clientIP(r *http.Request) string {
	if h := s.reg.ClientIPHeader; h != "" {
		if v := r.Header.Get(h); v != "" {
			parts := strings.Split(v, ",")
			if ip := net.ParseIP(strings.TrimSpace(parts[len(parts)-1])); ip != nil {
				return ip.String()
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func (s *Server) refuse(w http.ResponseWriter, status int, reason string, wait time.Duration) {
	s.met.inc("push_relay_registration_refusals_total", "reason", reason)
	if wait > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(retrySeconds(wait)))
	}
	writeErr(w, status, reason)
}

func (s *Server) registrationClosed() bool {
	if s.reg.Closed {
		return true
	}
	closed, err := s.Store.RegistrationClosed()
	if err != nil {
		s.Log.Error("registration switch", "err", err)
		return true // fail closed
	}
	return closed
}

func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	ip := s.clientIP(r)
	if ok, wait := s.regLim.Allow(ip); !ok {
		s.refuse(w, http.StatusTooManyRequests, "rate_limited", wait)
		return
	}
	if s.registrationClosed() {
		s.refuse(w, http.StatusForbidden, "registration_closed", 0)
		return
	}
	var req RegisterRequest
	if err := decode(w, r, 2<<10, &req); err != nil {
		var he *httpError
		errors.As(err, &he)
		s.refuse(w, he.status, "invalid_request", 0)
		return
	}
	app := s.Apps[req.App]
	switch {
	case app == nil:
		s.refuse(w, http.StatusBadRequest, "unknown_app", 0)
		return
	case !app.OpenRegistration:
		s.refuse(w, http.StatusForbidden, "registration_not_allowed", 0)
		return
	case !installRe.MatchString(req.InstallID):
		s.refuse(w, http.StatusBadRequest, "invalid_install_id", 0)
		return
	case req.Version != "" && !versionRe.MatchString(req.Version):
		s.refuse(w, http.StatusBadRequest, "invalid_version", 0)
		return
	case req.ServerURL != "" && !validServerURL(req.ServerURL):
		s.refuse(w, http.StatusBadRequest, "invalid_server_url", 0)
		return
	}
	ipHash, err := s.Store.HashIP(ip)
	if err != nil {
		s.Log.Error("hash ip", "err", err)
		writeErr(w, http.StatusInternalServerError, "internal_error")
		return
	}
	limitN := s.reg.NewKeysPerHour
	if limitN < 0 {
		limitN = 0
	}
	s.regMu.Lock()
	res, wait, err := s.Store.Register(store.Registration{App: req.App, InstallID: req.InstallID, ServerURL: req.ServerURL,
		Version: req.Version, IPHash: ipHash}, limitN, s.Now())
	s.regMu.Unlock()
	if errors.Is(err, store.ErrCap) {
		s.Log.Warn("registration cap reached", "app", req.App)
		s.refuse(w, http.StatusTooManyRequests, "registration_cap", wait)
		return
	}
	if err != nil {
		s.Log.Error("register", "err", err)
		writeErr(w, http.StatusInternalServerError, "internal_error")
		return
	}
	status := http.StatusCreated
	if res.Rotated {
		status = http.StatusOK
		s.met.inc("push_relay_rotations_total", "app", req.App)
	} else {
		s.met.inc("push_relay_registrations_total", "app", req.App)
	}
	s.Log.Info("register", "key", res.ID, "app", req.App, "rotated", res.Rotated, "version", req.Version)
	writeJSON(w, status, RegisterResponse{Key: res.Raw, KeyID: res.ID, App: req.App, Rotated: res.Rotated,
		DailyQuota: s.quotaFor(store.Key{Public: true})})
}

func validServerURL(raw string) bool {
	if len(raw) > maxServerURL {
		return false
	}
	u, err := url.Parse(raw)
	return err == nil && (u.Scheme == "https" || u.Scheme == "http") && u.Host != "" && u.User == nil
}

// quotaFor is a key's daily push quota, 0 = unlimited: its own override, else the default for public keys
// (registration.daily_quota) or for manual keys (limits.daily_quota).
func (s *Server) quotaFor(k store.Key) int {
	if k.Quota != nil {
		return *k.Quota
	}
	if k.Public {
		return max(0, s.reg.DailyQuota)
	}
	return s.Quota
}
