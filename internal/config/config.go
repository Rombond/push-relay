// Package config loads the relay's YAML configuration.
package config

import (
	"fmt"
	"net/url"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Listen      string          `yaml:"listen"`
	AdminListen string          `yaml:"admin_listen"` // /metrics, empty = off
	DB          string          `yaml:"db"`
	Limits      Limits          `yaml:"limits"`
	Media       Media           `yaml:"media"`
	Register    Registration    `yaml:"registration"`
	Apps        map[string]*App `yaml:"apps"`
}

// Media is the short-lived image store behind POST /v1/media and GET /m/<id> (v0.3.0). Off without base_url.
type Media struct {
	BaseURL  string `yaml:"base_url"`  // public address of this relay, e.g. https://push.example.org
	TTL      int    `yaml:"ttl"`       // seconds an image stays downloadable, default 3600
	MaxUses  int    `yaml:"max_uses"`  // downloads per image, default 5
	MaxBytes int    `yaml:"max_bytes"` // per image, default 1 MiB
	MaxItems int    `yaml:"max_items"` // images kept at once (in memory), default 100
}

// Registration drives POST /v1/register (v0.4.0): self-hosted servers get a public key for apps with
// open_registration: true.
type Registration struct {
	// Closed refuses every registration (403 registration_closed). Also RELAY_REGISTRATION_CLOSED=1, or at runtime
	// `relay registration close`.
	Closed bool `yaml:"closed"`
	// NewKeysPerHour caps new public keys over a sliding hour, across all apps and addresses (rotations are free).
	NewKeysPerHour  int `yaml:"new_keys_per_hour"` // default 50, -1 = no cap
	PerIPPerMinute  int `yaml:"per_ip_per_minute"` // HTTP rate limit of /v1/register per client address, default 30
	DailyQuota      int `yaml:"daily_quota"`       // pushes per public key per UTC day, default 200, -1 = unlimited
	UnusedDays      int `yaml:"unused_days"`       // public keys unused this long are deleted, default 90, -1 = never
	IPRetentionDays int `yaml:"ip_retention_days"` // registration address hashes kept, default 30, -1 = forever
	// ClientIPHeader names a header set by your reverse proxy (e.g. X-Forwarded-For, last entry used).
	// Empty = the TCP peer address. Only set it when the relay is reachable through that proxy alone.
	ClientIPHeader string `yaml:"client_ip_header"`
}

type Limits struct {
	KeyRate        float64 `yaml:"key_rate"`         // requests per second per server key
	KeyBurst       int     `yaml:"key_burst"`        //
	TokenPerMinute int     `yaml:"token_per_minute"` // pushes per minute per device token
	DailyQuota     int     `yaml:"daily_quota"`      // pushes per key per UTC day, 0 = unlimited
}

type App struct {
	APNs      *APNs  `yaml:"apns"`
	FCM       *FCM   `yaml:"fcm"`
	Templates string `yaml:"templates"` // optional file, replaces the built-in texts
	// FreeText lets this app's servers send their own alert title and body (templates stay the fallback).
	// Off by default: only for apps whose servers you trust, since the text reaches the phones as is.
	FreeText bool `yaml:"free_text"`
	// ImageDomains lists the hosts an `image` URL may point to ("image.tmdb.org", "*.example.org").
	// Without it the app cannot send images, except its own uploads when Media is on.
	ImageDomains []string `yaml:"image_domains"`
	Media        bool     `yaml:"media"` // its servers may upload private images (POST /v1/media)
	// OpenRegistration lets any server get a public key with POST /v1/register (public store apps).
	// Public keys never get free_text.
	OpenRegistration bool `yaml:"open_registration"`
}

type APNs struct {
	BundleID string `yaml:"bundle_id"`
	TeamID   string `yaml:"team_id"`
	KeyID    string `yaml:"key_id"`
	KeyFile  string `yaml:"key_file"`
}

type FCM struct {
	ProjectID       string `yaml:"project_id"`
	CredentialsFile string `yaml:"credentials_file"`
}

func Load(path string) (*Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	c := Config{}
	dec := yaml.NewDecoder(f)
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	c.defaults()
	return &c, c.validate()
}

func (c *Config) defaults() {
	if c.Listen == "" {
		c.Listen = ":8080"
	}
	if c.DB == "" {
		c.DB = "/data/relay.db"
	}
	if c.Limits.KeyRate == 0 {
		c.Limits.KeyRate = 5
	}
	if c.Limits.KeyBurst == 0 {
		c.Limits.KeyBurst = 50
	}
	if c.Limits.TokenPerMinute == 0 {
		c.Limits.TokenPerMinute = 10
	}
	if c.Media.TTL == 0 {
		c.Media.TTL = 3600
	}
	if c.Media.MaxUses == 0 {
		c.Media.MaxUses = 5
	}
	if c.Media.MaxBytes == 0 {
		c.Media.MaxBytes = 1 << 20
	}
	if c.Media.MaxItems == 0 {
		c.Media.MaxItems = 100
	}
	r := &c.Register
	if v := os.Getenv("RELAY_REGISTRATION_CLOSED"); v == "1" || v == "true" {
		r.Closed = true
	}
	def := func(p *int, d int) {
		if *p == 0 {
			*p = d
		}
	}
	def(&r.NewKeysPerHour, 50)
	def(&r.PerIPPerMinute, 30)
	def(&r.DailyQuota, 200)
	def(&r.UnusedDays, 90)
	def(&r.IPRetentionDays, 30)
}

func (c *Config) validate() error {
	if len(c.Apps) == 0 {
		return fmt.Errorf("config: no apps")
	}
	for name, a := range c.Apps {
		if a == nil || (a.APNs == nil && a.FCM == nil) {
			return fmt.Errorf("config: app %q needs apns and/or fcm", name)
		}
		if a.APNs != nil && (a.APNs.BundleID == "" || a.APNs.TeamID == "" || a.APNs.KeyID == "" || a.APNs.KeyFile == "") {
			return fmt.Errorf("config: app %q apns needs bundle_id, team_id, key_id, key_file", name)
		}
		if a.FCM != nil && (a.FCM.ProjectID == "" || a.FCM.CredentialsFile == "") {
			return fmt.Errorf("config: app %q fcm needs project_id, credentials_file", name)
		}
		for _, d := range a.ImageDomains {
			if d == "" || strings.ContainsAny(d, "/:@ ") || strings.Count(d, "*") > 1 || (strings.Contains(d, "*") && !strings.HasPrefix(d, "*.")) {
				return fmt.Errorf("config: app %q image_domains: %q is not a host or *.host", name, d)
			}
		}
		if a.Media && c.Media.BaseURL == "" {
			return fmt.Errorf("config: app %q has media: true but media.base_url is empty", name)
		}
	}
	for _, v := range []int{c.Register.NewKeysPerHour, c.Register.PerIPPerMinute, c.Register.DailyQuota, c.Register.UnusedDays, c.Register.IPRetentionDays} {
		if v < -1 {
			return fmt.Errorf("config: registration values must be > 0, or -1 for none")
		}
	}
	if c.Register.PerIPPerMinute < 1 {
		return fmt.Errorf("config: registration.per_ip_per_minute must be > 0")
	}
	if c.Media.BaseURL != "" {
		u, err := url.Parse(c.Media.BaseURL)
		if err != nil || u.Scheme != "https" || u.Host == "" || (u.Path != "" && u.Path != "/") {
			return fmt.Errorf("config: media.base_url must be https://host")
		}
	}
	return nil
}
