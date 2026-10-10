// Package config loads the relay's YAML configuration.
package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Listen      string          `yaml:"listen"`
	AdminListen string          `yaml:"admin_listen"` // /metrics, empty = off
	DB          string          `yaml:"db"`
	Limits      Limits          `yaml:"limits"`
	Apps        map[string]*App `yaml:"apps"`
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
	}
	return nil
}
