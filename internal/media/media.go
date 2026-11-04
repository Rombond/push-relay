// Package media keeps the private images of notifications for a short time (POST /v1/media, GET /m/<id>).
//
// A server uploads a JPEG or PNG it cannot publish itself (a camera snapshot, a graph) and puts the returned URL in
// the push `image` field; the phone's notification extension downloads it, from anywhere. Images live in memory
// only, for TTL, and for at most MaxUses downloads. The URL carries a random id and an HMAC of it: neither can be
// guessed, and the HMAC key changes at every start, so a restart forgets every image.
package media

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	ErrType = errors.New("image must be JPEG or PNG")
	ErrFull = errors.New("media store full")
)

type item struct {
	data    []byte
	ctype   string
	expires time.Time
	uses    int
}

type Store struct {
	Base     string // https://host, no trailing slash
	TTL      time.Duration
	MaxUses  int
	MaxBytes int
	MaxItems int
	Now      func() time.Time

	secret []byte
	mu     sync.Mutex
	items  map[string]*item
}

func New(base string, ttl time.Duration, maxUses, maxBytes, maxItems int) *Store {
	secret := make([]byte, 32)
	rand.Read(secret)
	return &Store{Base: strings.TrimRight(base, "/"), TTL: ttl, MaxUses: maxUses, MaxBytes: maxBytes, MaxItems: maxItems,
		Now: time.Now, secret: secret, items: map[string]*item{}}
}

func (s *Store) sig(id string) string {
	m := hmac.New(sha256.New, s.secret)
	m.Write([]byte(id))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil)[:16])
}

// Prefix is what every URL of this store starts with (the image allow-list accepts it for apps with media on).
func (s *Store) Prefix() string { return s.Base + "/m/" }

// Put stores one image and returns its URL and expiry.
func (s *Store) Put(data []byte) (string, time.Time, error) {
	ctype := http.DetectContentType(data)
	if ctype != "image/jpeg" && ctype != "image/png" {
		return "", time.Time{}, ErrType
	}
	raw := make([]byte, 16)
	rand.Read(raw)
	id := hex.EncodeToString(raw)
	now := s.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweep(now)
	if len(s.items) >= s.MaxItems {
		return "", time.Time{}, ErrFull
	}
	exp := now.Add(s.TTL)
	s.items[id] = &item{data: data, ctype: ctype, expires: exp}
	return s.Prefix() + id + "?sig=" + s.sig(id), exp, nil
}

func (s *Store) sweep(now time.Time) {
	for id, it := range s.items {
		if !now.Before(it.expires) || it.uses >= s.MaxUses {
			delete(s.items, id)
		}
	}
}

// Len is the number of images still downloadable (tests, metrics).
func (s *Store) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweep(s.Now())
	return len(s.items)
}

// Serve answers GET /m/{id}?sig=...: 404 for anything unknown, expired, used up or wrongly signed.
func (s *Store) Serve(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !hmac.Equal([]byte(r.URL.Query().Get("sig")), []byte(s.sig(id))) {
		http.NotFound(w, r)
		return
	}
	now := s.Now()
	s.mu.Lock()
	it := s.items[id]
	if it == nil || !now.Before(it.expires) || it.uses >= s.MaxUses {
		delete(s.items, id)
		s.mu.Unlock()
		http.NotFound(w, r)
		return
	}
	it.uses++
	data, ctype, left := it.data, it.ctype, it.expires.Sub(now)
	s.mu.Unlock()
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.Header().Set("Cache-Control", "private, max-age="+strconv.Itoa(int(left.Seconds())))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Write(data)
}
