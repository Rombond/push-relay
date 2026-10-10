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
  relay keys add  -app <app> -name <owner> [-config relay.yaml]
  relay keys list [-config relay.yaml]
  relay keys revoke <id> [-config relay.yaml]
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
		app := &server.App{Templates: set, Senders: map[string]push.Sender{}}
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

func keys(args []string) error {
	if len(args) == 0 {
		usage()
	}
	fs := flag.NewFlagSet("keys", flag.ExitOnError)
	path := fs.String("config", envOr("RELAY_CONFIG", "/config/relay.yaml"), "config file")
	app := fs.String("app", "", "app id (add)")
	name := fs.String("name", "", "owner of the key (add)")
	// flags may come before or after the sub-command argument
	sub, rest := args[0], args[1:]
	var id string
	if sub == "revoke" && len(rest) > 0 && rest[0] != "" && rest[0][0] != '-' {
		id, rest = rest[0], rest[1:]
	}
	fs.Parse(rest)
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
		fmt.Printf("%-4s %-12s %-24s %-10s %8s %8s  %s\n", "ID", "APP", "NAME", "STATE", "SENT", "FAILED", "CREATED")
		for _, k := range ks {
			state := "active"
			if k.Revoked {
				state = "revoked"
			}
			fmt.Printf("%-4d %-12s %-24s %-10s %8d %8d  %s\n", k.ID, k.App, k.Name, state, k.Sent, k.Failed, k.CreatedAt.Format("2006-01-02"))
		}
	case "revoke":
		if id == "" && fs.NArg() == 1 {
			id = fs.Arg(0)
		}
		n, err := strconv.ParseInt(id, 10, 64)
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
