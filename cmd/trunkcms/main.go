// Command trunkcms serves a blog from a GitHub repository, with an editing UI
// that commits changes back to it.
//
//	trunkcms [serve]                  run the server (configured by environment)
//	trunkcms build -dir DIR -out OUT  render a content directory to static files
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
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/nullism/trunkcms/internal/admin"
	"github.com/nullism/trunkcms/internal/auth"
	"github.com/nullism/trunkcms/internal/build"
	"github.com/nullism/trunkcms/internal/config"
	"github.com/nullism/trunkcms/internal/content"
	"github.com/nullism/trunkcms/internal/github"
	"github.com/nullism/trunkcms/internal/policy"
	"github.com/nullism/trunkcms/internal/render"
	"github.com/nullism/trunkcms/internal/server"
	"github.com/nullism/trunkcms/internal/session"
	"github.com/nullism/trunkcms/internal/source"
)

func main() {
	if os.Getenv("TRUNKCMS_LOG_JSON") != "" {
		slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))
	}
	cmd := "serve"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	var err error
	switch cmd {
	case "serve":
		err = serve()
	case "build":
		err = buildStatic(os.Args[2:])
	default:
		fmt.Fprintln(os.Stderr, "usage: trunkcms [serve | build -dir DIR -out OUT]")
		os.Exit(2)
	}
	if err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func serve() error {
	cfg, err := config.FromEnv()
	if err != nil {
		return fmt.Errorf("configuration:\n%w", err)
	}

	var store source.Store
	var gh *github.Client
	if cfg.Local() {
		store = source.NewDirStore(cfg.ContentDir)
		slog.Info("local mode", "dir", cfg.ContentDir)
	} else {
		gh, err = github.New(github.Config{
			APIURL: cfg.APIURL, WebURL: cfg.WebURL,
			Owner: cfg.Owner, Repo: cfg.Repo, Dir: cfg.RepoPath, Branch: cfg.Branch,
			AppID: cfg.AppID, InstallationID: cfg.InstallationID, PrivateKeyPEM: cfg.PrivateKey,
			ClientID: cfg.ClientID, ClientSecret: cfg.ClientSecret,
		})
		if err != nil {
			return err
		}
		store = gh
		slog.Info("github mode", "repo", cfg.Owner+"/"+cfg.Repo, "path", cfg.RepoPath, "branch", cfg.Branch)
	}

	// Start from a clean data directory; everything in it is derived from the repo.
	if err := os.RemoveAll(cfg.DataDir); err != nil {
		return err
	}
	syncer, err := source.NewSyncer(store, cfg.DataDir, cfg.PollInterval)
	if err != nil {
		return err
	}
	sessions, err := session.New(cfg.SessionKeys, strings.HasPrefix(cfg.SiteURL, "https://"))
	if err != nil {
		return err
	}
	resolver := &auth.Resolver{
		Sessions: sessions,
		Users: func() content.UsersFile {
			if cur := syncer.Current(); cur != nil {
				return cur.Site.Users
			}
			return content.UsersFile{}
		},
	}
	var devUsers []admin.DevUser
	if gh != nil {
		resolver.Perm = gh.Permission
	} else {
		resolver.DevRoles = map[string]policy.Role{}
		for login, role := range cfg.DevUsers {
			resolver.DevRoles[login] = policy.Role(role)
			devUsers = append(devUsers, admin.DevUser{Login: login, Role: policy.Role(role)})
		}
	}

	adminHandler, err := admin.New(admin.Options{
		Syncer: syncer, Store: store, GitHub: gh, Sessions: sessions, Auth: resolver,
		SiteURL: cfg.SiteURL, WebhookSecret: cfg.WebhookSecret, DevUsers: devUsers,
	})
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go initialSync(ctx, syncer)
	go syncer.Run(ctx)

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           &server.Server{Syncer: syncer, Auth: resolver, Admin: adminHandler},
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       2 * time.Minute, // uploads
		WriteTimeout:      2 * time.Minute,
		IdleTimeout:       2 * time.Minute,
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	slog.Info("listening", "addr", cfg.Addr, "site_url", cfg.SiteURL, "data_dir", cfg.DataDir, "poll", cfg.PollInterval)

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	slog.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// initialSync retries until the first build succeeds; /readyz reports not-ready until then.
func initialSync(ctx context.Context, s *source.Syncer) {
	delay := time.Second
	for s.Current() == nil {
		syncCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		s.Sync(syncCtx)
		cancel()
		if s.Current() != nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
		delay = min(delay*2, 30*time.Second)
	}
}

// buildStatic renders a content directory to plain files (useful for testing themes).
func buildStatic(args []string) error {
	fl := flag.NewFlagSet("build", flag.ExitOnError)
	dir := fl.String("dir", ".", "content directory")
	out := fl.String("out", "public", "output directory")
	fl.Parse(args)

	work, err := os.MkdirTemp("", "trunkcms-build-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work)
	objs, err := render.NewObjects(filepath.Join(work, "objects"))
	if err != nil {
		return err
	}
	res, err := build.Run("local", os.DirFS(*dir), objs, time.Now())
	if err != nil {
		return err
	}
	for _, p := range res.Site.Problems {
		slog.Warn("skipped", "problem", p.Error())
	}
	write := func(urlPath string, e *render.Entry) error {
		rel := strings.TrimPrefix(urlPath, "/")
		if rel == "" || strings.HasSuffix(rel, "/") {
			rel += "index.html"
		}
		b, err := os.ReadFile(res.Output.Path(e, false))
		if err != nil {
			return err
		}
		full := filepath.Join(*out, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return err
		}
		return os.WriteFile(full, b, 0o644)
	}
	for u, e := range res.Output.Files {
		if err := write(u, e); err != nil {
			return err
		}
	}
	if err := write("/404.html", res.Output.NotFound); err != nil {
		return err
	}
	slog.Info("built", "files", len(res.Output.Files), "out", *out, "took", res.Duration.Round(time.Millisecond))
	return nil
}
