// Command relay is the push relay: `relay serve` runs the API, `relay keys ...` manages server keys.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/Rombond/push-relay/internal/apns"
	"github.com/Rombond/push-relay/internal/config"
	"github.com/Rombond/push-relay/internal/fcm"
	"github.com/Rombond/push-relay/internal/media"
	"github.com/Rombond/push-relay/internal/push"
	"github.com/Rombond/push-relay/internal/server"
	"github.com/Rombond/push-relay/internal/store"
	"github.com/Rombond/push-relay/internal/templates"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	switch os.Args[1] {
	case "serve":
		fs := flag.NewFlagSet("serve", flag.ExitOnError)
		path := fs.String("config", envOr("RELAY_CONFIG", "/config/relay.yaml"), "config file")
		fs.Parse(os.Args[2:])
		if err := serve(*path); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
	case "check":
		fs := flag.NewFlagSet("check", flag.ExitOnError)
		path := fs.String("config", envOr("RELAY_CONFIG", "/config/relay.yaml"), "config file")
		fs.Parse(os.Args[2:])
		if err := check(*path); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
	case "registration":
		if err := registration(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
	case "keys":
		if err := keys(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
	default:
		usage()
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `usage:
  relay serve [-config relay.yaml]
  relay check [-config relay.yaml]     validate config, templates and credential files, then exit
  relay keys add  -app <app> -name <owner> [-config relay.yaml]
  relay keys list [-config relay.yaml]
  relay keys revoke <id> [-config relay.yaml]
  relay keys revoke -since <time> -until <time> -ip <addr>   mass revoke of public keys (any filter, combined with AND)
  relay keys quota <id> <n|default>    daily pushes for this key, 0 = unlimited, default = back to its kind's default
  relay keys cleanup                   delete public keys unused for registration.unused_days now
  relay registration open|close|status runtime switch for POST /v1/register (config/env closed always wins)
times: RFC 3339 (2026-11-07T10:00:00Z) or a date (2026-11-07, UTC)
`)
	os.Exit(2)
}

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func buildApps(ctx context.Context, cfg *config.Config) (map[string]*server.App, error) {
	apps := map[string]*server.App{}
	for name, ac := range cfg.Apps {
		data := templates.Builtin(name)
		if ac.Templates != "" {
			b, err := os.ReadFile(ac.Templates)
			if err != nil {
				return nil, fmt.Errorf("app %s templates: %w", name, err)
			}
			data = b
		}
		if data == nil {
			return nil, fmt.Errorf("app %s: no built-in texts, set templates: <file>", name)
		}
		set, err := templates.Parse(data)
		if err != nil {
			return nil, fmt.Errorf("app %s: %w", name, err)
		}
		app := &server.App{Templates: set, FreeText: ac.FreeText, Senders: map[string]push.Sender{},
			ImageDomains: ac.ImageDomains, Media: ac.Media, OpenRegistration: ac.OpenRegistration}
		if ac.APNs != nil {
			pem, err := os.ReadFile(ac.APNs.KeyFile)
			if err != nil {
				return nil, fmt.Errorf("app %s apns key: %w", name, err)
			}
			key, err := apns.ParseKey(pem)
			if err != nil {
				return nil, fmt.Errorf("app %s: %w", name, err)
			}
			app.Senders["ios"] = apns.New(apns.Config{BundleID: ac.APNs.BundleID, TeamID: ac.APNs.TeamID, KeyID: ac.APNs.KeyID, Key: key})
		}
		if ac.FCM != nil {
			creds, err := os.ReadFile(ac.FCM.CredentialsFile)
			if err != nil {
				return nil, fmt.Errorf("app %s fcm credentials: %w", name, err)
			}
			c, err := fcm.New(ctx, ac.FCM.ProjectID, creds)
			if err != nil {
				return nil, fmt.Errorf("app %s: %w", name, err)
			}
			app.Senders["android"] = c
		}
		apps[name] = app
	}
	return apps, nil
}

// check loads everything serve would, without opening the database or listening: run it before a restart.
func check(path string) error {
	cfg, err := config.Load(path)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	apps, err := buildApps(ctx, cfg)
	if err != nil {
		return err
	}
	for name, a := range apps {
		fmt.Printf("app %s: %d events, free_text %v, platforms %d, image domains %d, media %v, open registration %v\n",
			name, len(a.Templates.Events), a.FreeText, len(a.Senders), len(a.ImageDomains), a.Media, a.OpenRegistration)
	}
	fmt.Println("config ok")
	return nil
}

func serve(path string) error {
	cfg, err := config.Load(path)
	if err != nil {
		return err
	}
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	st, err := store.Open(cfg.DB)
	if err != nil {
		return err
	}
	defer st.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	apps, err := buildApps(ctx, cfg)
	if err != nil {
		return err
	}
	srv := server.New(st, apps, cfg.Limits, log)
	srv.SetRegistration(cfg.Register)
	go cleanupLoop(ctx, st, cfg.Register, log)
	if m := cfg.Media; m.BaseURL != "" {
		srv.Media = media.New(m.BaseURL, time.Duration(m.TTL)*time.Second, m.MaxUses, m.MaxBytes, m.MaxItems)
	}

	api := &http.Server{Addr: cfg.Listen, Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 60 * time.Second}
	errc := make(chan error, 2)
	go func() { errc <- api.ListenAndServe() }()
	var admin *http.Server
	if cfg.AdminListen != "" {
		mux := http.NewServeMux()
		mux.Handle("GET /metrics", srv.Metrics().Handler())
		admin = &http.Server{Addr: cfg.AdminListen, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
		go func() { errc <- admin.ListenAndServe() }()
	}
	log.Info("listening", "api", cfg.Listen, "admin", cfg.AdminListen)

	select {
	case <-ctx.Done():
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	}
	shut, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	api.Shutdown(shut)
	if admin != nil {
		admin.Shutdown(shut)
	}
	return nil
}

func days(n int) time.Duration {
	if n <= 0 {
		return 0
	}
	return time.Duration(n) * 24 * time.Hour
}

// cleanupLoop deletes unused public keys and old address hashes at start, then daily.
func cleanupLoop(ctx context.Context, st *store.Store, r config.Registration, log *slog.Logger) {
	t := time.NewTicker(24 * time.Hour)
	defer t.Stop()
	for {
		n, err := st.Cleanup(time.Now(), days(r.UnusedDays), days(r.IPRetentionDays))
		if err != nil {
			log.Error("cleanup", "err", err)
		} else if n > 0 {
			log.Info("cleanup", "deleted_public_keys", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func parseTime(v string) (time.Time, error) {
	if v == "" {
		return time.Time{}, nil
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t, nil
	}
	return time.Parse("2006-01-02", v)
}

func registration(args []string) error {
	if len(args) == 0 {
		usage()
	}
	fs := flag.NewFlagSet("registration", flag.ExitOnError)
	path := fs.String("config", envOr("RELAY_CONFIG", "/config/relay.yaml"), "config file")
	fs.Parse(args[1:])
	cfg, err := config.Load(*path)
	if err != nil {
		return err
	}
	st, err := store.Open(cfg.DB)
	if err != nil {
		return err
	}
	defer st.Close()
	switch args[0] {
	case "open", "close":
		if err := st.SetRegistrationClosed(args[0] == "close"); err != nil {
			return err
		}
	case "status":
	default:
		usage()
	}
	closed, err := st.RegistrationClosed()
	if err != nil {
		return err
	}
	fmt.Printf("runtime switch: closed=%v, config/env closed=%v\n", closed, cfg.Register.Closed)
	return nil
}

func keys(args []string) error {
	if len(args) == 0 {
		usage()
	}
	fs := flag.NewFlagSet("keys", flag.ExitOnError)
	path := fs.String("config", envOr("RELAY_CONFIG", "/config/relay.yaml"), "config file")
	app := fs.String("app", "", "app id (add)")
	name := fs.String("name", "", "owner of the key (add)")
	since := fs.String("since", "", "mass revoke: public keys created at or after")
	until := fs.String("until", "", "mass revoke: public keys created at or before")
	ip := fs.String("ip", "", "mass revoke: public keys registered from this address")
	// flags may come before or after the sub-command arguments
	sub, rest := args[0], args[1:]
	var pos []string
	for len(rest) > 0 && rest[0] != "" && rest[0][0] != '-' {
		pos, rest = append(pos, rest[0]), rest[1:]
	}
	fs.Parse(rest)
	pos = append(pos, fs.Args()...)
	cfg, err := config.Load(*path)
	if err != nil {
		return err
	}
	st, err := store.Open(cfg.DB)
	if err != nil {
		return err
	}
	defer st.Close()

	switch sub {
	case "add":
		if cfg.Apps[*app] == nil {
			return fmt.Errorf("unknown app %q (declared in config: %d)", *app, len(cfg.Apps))
		}
		if *name == "" {
			return errors.New("-name is required")
		}
		raw, id, err := st.Add(*app, *name)
		if err != nil {
			return err
		}
		fmt.Printf("key #%d for %s (%s), shown once:\n%s\n", id, *app, *name, raw)
	case "list":
		ks, err := st.List()
		if err != nil {
			return err
		}
		fmt.Printf("%-4s %-12s %-24s %-7s %-10s %8s %8s %8s  %-10s %s\n", "ID", "APP", "NAME", "KIND", "STATE", "SENT", "FAILED", "QUOTA", "CREATED", "LAST USED")
		for _, k := range ks {
			state := "active"
			if k.Revoked {
				state = "revoked"
			}
			kind, quota, last := "manual", "default", "-"
			if k.Public {
				kind = "public"
			}
			if k.Quota != nil {
				quota = strconv.Itoa(*k.Quota)
			}
			if k.LastUsed != nil {
				last = k.LastUsed.Format("2006-01-02")
			}
			fmt.Printf("%-4d %-12s %-24s %-7s %-10s %8d %8d %8s  %-10s %s\n", k.ID, k.App, k.Name, kind, state, k.Sent, k.Failed, quota, k.CreatedAt.Format("2006-01-02"), last)
		}
	case "quota":
		if len(pos) != 2 {
			return errors.New("usage: keys quota <id> <n|default>")
		}
		id, err := strconv.ParseInt(pos[0], 10, 64)
		if err != nil {
			return errors.New("quota needs a key id")
		}
		var q *int
		if pos[1] != "default" {
			n, err := strconv.Atoi(pos[1])
			if err != nil || n < 0 {
				return errors.New("quota must be a number >= 0 (0 = unlimited) or default")
			}
			q = &n
		}
		if err := st.SetQuota(id, q); err != nil {
			return err
		}
		fmt.Printf("key #%d quota set to %s\n", id, pos[1])
	case "cleanup":
		n, err := st.Cleanup(time.Now(), days(cfg.Register.UnusedDays), days(cfg.Register.IPRetentionDays))
		if err != nil {
			return err
		}
		fmt.Printf("%d unused public keys deleted\n", n)
	case "revoke":
		if *since != "" || *until != "" || *ip != "" {
			var f store.RevokeFilter
			if f.Since, err = parseTime(*since); err != nil {
				return fmt.Errorf("-since: %w", err)
			}
			if f.Until, err = parseTime(*until); err != nil {
				return fmt.Errorf("-until: %w", err)
			}
			if *ip != "" {
				if f.IPHash, err = st.HashIP(*ip); err != nil {
					return err
				}
			}
			n, err := st.RevokePublic(f, time.Now())
			if err != nil {
				return err
			}
			fmt.Printf("%d public keys revoked\n", n)
			return nil
		}
		if len(pos) != 1 {
			return errors.New("revoke needs a key id, or -since/-until/-ip")
		}
		n, err := strconv.ParseInt(pos[0], 10, 64)
		if err != nil {
			return errors.New("revoke needs a key id")
		}
		if err := st.Revoke(n); err != nil {
			return err
		}
		fmt.Printf("key #%d revoked\n", n)
	default:
		usage()
	}
	return nil
}
