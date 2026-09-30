// Package config reads trunkcms settings from the environment.
// Every secret can also be read from a file via NAME_FILE.
package config

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	// Local mode: serve and edit a directory instead of GitHub.
	ContentDir string
	DevUsers   map[string]string // login → role, local mode only

	Owner, Repo    string
	RepoPath       string // subdirectory of the repo holding the site; "" for the root
	Branch         string
	AppID          int64
	InstallationID int64
	PrivateKey     []byte
	ClientID       string
	ClientSecret   string
	WebhookSecret  []byte
	APIURL, WebURL string

	// DataDir holds snapshots and rendered files. trunkcms owns it and wipes it at startup.
	DataDir string

	SessionKeys  [][]byte
	SiteURL      string
	PollInterval time.Duration
	Lazy         bool
	Addr         string
}

func (c *Config) Local() bool { return c.ContentDir != "" }

func FromEnv() (*Config, error) {
	c := &Config{
		ContentDir: os.Getenv("TRUNKCMS_CONTENT_DIR"),
		Branch:     envOr("TRUNKCMS_BRANCH", "main"),
		APIURL:     os.Getenv("TRUNKCMS_GITHUB_API_URL"),
		WebURL:     os.Getenv("TRUNKCMS_GITHUB_WEB_URL"),
		SiteURL:    strings.TrimRight(os.Getenv("TRUNKCMS_SITE_URL"), "/"),
		Addr:       ":" + envOr("PORT", "8080"),
	}
	// Keyed by port so two local instances never share (and wipe) a directory.
	c.DataDir = filepath.Join(os.TempDir(), "trunkcms-"+envOr("PORT", "8080"))
	if d := os.Getenv("TRUNKCMS_DATA_DIR"); d != "" {
		c.DataDir = filepath.Join(d, "trunkcms")
	}
	var errs []error

	defaultPoll := "30s"
	if c.Local() {
		defaultPoll = "2s" // picks up edits made directly on disk
	}
	poll, err := time.ParseDuration(envOr("TRUNKCMS_POLL_INTERVAL", defaultPoll))
	if err != nil {
		errs = append(errs, fmt.Errorf("TRUNKCMS_POLL_INTERVAL: %w", err))
	}
	c.PollInterval = poll
	switch mode := envOr("TRUNKCMS_POLL_MODE", "auto"); mode {
	case "lazy":
		c.Lazy = true
	case "background":
	case "auto":
		// Request-driven platforms freeze the CPU between requests, so check on requests.
		c.Lazy = os.Getenv("AWS_LAMBDA_FUNCTION_NAME") != "" || os.Getenv("K_SERVICE") != ""
	default:
		errs = append(errs, fmt.Errorf("TRUNKCMS_POLL_MODE: unknown mode %q", mode))
	}

	keys, err := secret("TRUNKCMS_SESSION_KEY")
	if err != nil {
		errs = append(errs, err)
	}
	for _, k := range strings.Split(string(keys), ",") {
		if k = strings.TrimSpace(k); k == "" {
			continue
		}
		b, err := decodeKey(k)
		if err != nil {
			errs = append(errs, fmt.Errorf("TRUNKCMS_SESSION_KEY: %w", err))
			continue
		}
		c.SessionKeys = append(c.SessionKeys, b)
	}

	if c.Local() {
		if c.SiteURL == "" {
			c.SiteURL = "http://localhost" + c.Addr
		}
		if len(c.SessionKeys) == 0 {
			k := make([]byte, 32)
			rand.Read(k)
			c.SessionKeys = [][]byte{k}
			slog.Warn("TRUNKCMS_SESSION_KEY not set; using a random key (sessions end on restart)")
		}
		c.DevUsers = map[string]string{}
		for _, pair := range strings.Split(envOr("TRUNKCMS_DEV_USERS", "dev-admin:admin,dev-editor:editor,dev-author:author"), ",") {
			login, role, ok := strings.Cut(strings.TrimSpace(pair), ":")
			if !ok || (role != "admin" && role != "editor" && role != "author") {
				errs = append(errs, fmt.Errorf("TRUNKCMS_DEV_USERS: expected login:role, got %q", pair))
				continue
			}
			c.DevUsers[strings.ToLower(login)] = role
		}
		return c, errors.Join(errs...)
	}

	// GitHub mode.
	if c.Owner, c.Repo, err = ParseRepo(os.Getenv("TRUNKCMS_REPO")); err != nil {
		errs = append(errs, err)
	}
	if c.RepoPath, err = ParseRepoPath(os.Getenv("TRUNKCMS_REPO_PATH")); err != nil {
		errs = append(errs, err)
	}
	c.AppID, err = envInt("TRUNKCMS_GITHUB_APP_ID", true)
	if err != nil {
		errs = append(errs, err)
	}
	c.InstallationID, err = envInt("TRUNKCMS_GITHUB_INSTALLATION_ID", false)
	if err != nil {
		errs = append(errs, err)
	}
	if c.PrivateKey, err = secret("TRUNKCMS_GITHUB_PRIVATE_KEY"); err != nil || len(c.PrivateKey) == 0 {
		errs = append(errs, errors.New("TRUNKCMS_GITHUB_PRIVATE_KEY (or _FILE) is required"))
	}
	c.ClientID = os.Getenv("TRUNKCMS_GITHUB_CLIENT_ID")
	cs, err := secret("TRUNKCMS_GITHUB_CLIENT_SECRET")
	c.ClientSecret = strings.TrimSpace(string(cs))
	if err != nil || c.ClientID == "" || c.ClientSecret == "" {
		errs = append(errs, errors.New("TRUNKCMS_GITHUB_CLIENT_ID and TRUNKCMS_GITHUB_CLIENT_SECRET are required for sign-in"))
	}
	ws, err := secret("TRUNKCMS_WEBHOOK_SECRET")
	if err != nil {
		errs = append(errs, err)
	}
	c.WebhookSecret = []byte(strings.TrimSpace(string(ws)))
	if len(c.WebhookSecret) == 0 {
		slog.Warn("TRUNKCMS_WEBHOOK_SECRET not set; relying on polling only")
	}
	if len(c.SessionKeys) == 0 {
		errs = append(errs, errors.New("TRUNKCMS_SESSION_KEY is required (generate one with: openssl rand -base64 32)"))
	}
	if c.SiteURL == "" {
		errs = append(errs, errors.New("TRUNKCMS_SITE_URL is required (e.g. https://myblog.com)"))
	}
	return c, errors.Join(errs...)
}

// ParseRepo accepts owner/name, github.com/owner/name, or a full URL.
func ParseRepo(s string) (owner, name string, err error) {
	s = strings.TrimSpace(s)
	for _, p := range []string{"https://", "http://", "www.", "github.com/"} {
		s = strings.TrimPrefix(s, p)
	}
	s = strings.TrimSuffix(strings.TrimSuffix(s, "/"), ".git")
	owner, name, ok := strings.Cut(s, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return "", "", fmt.Errorf("TRUNKCMS_REPO: expected owner/name, got %q", s)
	}
	return owner, name, nil
}

// ParseRepoPath cleans a repo subdirectory such as "blog1" or "/sites/blog1/".
// The repo root comes back as "".
func ParseRepoPath(s string) (string, error) {
	s = strings.Trim(strings.TrimSpace(s), "/")
	if s == "" || s == "." {
		return "", nil
	}
	if !fs.ValidPath(s) || s == ".git" || strings.HasPrefix(s, ".git/") {
		return "", fmt.Errorf("TRUNKCMS_REPO_PATH: expected a directory inside the repo, got %q", s)
	}
	return s, nil
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envInt(k string, required bool) (int64, error) {
	v := os.Getenv(k)
	if v == "" {
		if required {
			return 0, fmt.Errorf("%s is required", k)
		}
		return 0, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", k, err)
	}
	return n, nil
}

// secret reads NAME, or the file named by NAME_FILE.
func secret(name string) ([]byte, error) {
	if v := os.Getenv(name); v != "" {
		return []byte(v), nil
	}
	if f := os.Getenv(name + "_FILE"); f != "" {
		b, err := os.ReadFile(f)
		if err != nil {
			return nil, fmt.Errorf("%s_FILE: %w", name, err)
		}
		return b, nil
	}
	return nil, nil
}

func decodeKey(s string) ([]byte, error) {
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if b, err := enc.DecodeString(s); err == nil {
			if len(b) != 32 {
				return nil, fmt.Errorf("key must decode to 32 bytes, got %d", len(b))
			}
			return b, nil
		}
	}
	return nil, errors.New("key must be base64")
}
