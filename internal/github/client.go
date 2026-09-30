// Package github is a small hand-written GitHub client covering exactly what
// trunkcms needs: GitHub App auth, repo snapshots, Git Data API commits,
// user OAuth, and webhook verification.
package github

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Config struct {
	APIURL         string // default https://api.github.com
	WebURL         string // default https://github.com
	Owner, Repo    string
	Dir            string // subdirectory of the repo holding the site; "" for the root
	Branch         string
	AppID          int64
	InstallationID int64 // discovered from the repo when 0
	PrivateKeyPEM  []byte
	ClientID       string
	ClientSecret   string
	HTTP           *http.Client
}

type Client struct {
	cfg  Config
	key  *rsa.PrivateKey
	http *http.Client

	mu       sync.Mutex
	instID   int64
	token    string
	tokenExp time.Time
	headETag string
	headSHA  string
}

// APIError is a non-2xx response from the GitHub API.
type APIError struct {
	Status  int
	Message string
	Path    string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("github: %s: %d %s", e.Path, e.Status, e.Message)
}

func IsStatus(err error, status int) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.Status == status
}

func New(cfg Config) (*Client, error) {
	if cfg.APIURL == "" {
		cfg.APIURL = "https://api.github.com"
	}
	if cfg.WebURL == "" {
		cfg.WebURL = "https://github.com"
	}
	cfg.APIURL = strings.TrimRight(cfg.APIURL, "/")
	cfg.WebURL = strings.TrimRight(cfg.WebURL, "/")
	if cfg.Branch == "" {
		cfg.Branch = "main"
	}
	if cfg.HTTP == nil {
		cfg.HTTP = &http.Client{Timeout: 60 * time.Second}
	}
	key, err := ParsePrivateKey(cfg.PrivateKeyPEM)
	if err != nil {
		return nil, err
	}
	return &Client{cfg: cfg, key: key, http: cfg.HTTP, instID: cfg.InstallationID}, nil
}

func ParsePrivateKey(pemBytes []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("github: private key is not PEM")
	}
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return k, nil
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("github: parse private key: %w", err)
	}
	rk, ok := k.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("github: private key is not RSA")
	}
	return rk, nil
}

// appJWT signs a short-lived RS256 JWT identifying the App.
func (c *Client) appJWT() (string, error) {
	now := time.Now()
	enc := base64.RawURLEncoding
	header := enc.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	claims, _ := json.Marshal(map[string]any{
		"iat": now.Add(-60 * time.Second).Unix(),
		"exp": now.Add(9 * time.Minute).Unix(),
		"iss": strconv.FormatInt(c.cfg.AppID, 10),
	})
	signing := header + "." + enc.EncodeToString(claims)
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, c.key, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	return signing + "." + enc.EncodeToString(sig), nil
}

// installationToken returns a cached repo-scoped token, refreshing it early.
func (c *Client) installationToken(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && time.Until(c.tokenExp) > 5*time.Minute {
		return c.token, nil
	}
	jwt, err := c.appJWT()
	if err != nil {
		return "", err
	}
	if c.instID == 0 {
		var inst struct {
			ID int64 `json:"id"`
		}
		if _, err := c.send(ctx, "GET", c.repoPath("/installation"), "Bearer "+jwt, nil, &inst, nil); err != nil {
			return "", fmt.Errorf("finding App installation on %s/%s (is the App installed on the repo?): %w", c.cfg.Owner, c.cfg.Repo, err)
		}
		c.instID = inst.ID
	}
	var tok struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if _, err := c.send(ctx, "POST", fmt.Sprintf("/app/installations/%d/access_tokens", c.instID), "Bearer "+jwt, map[string]any{}, &tok, nil); err != nil {
		return "", err
	}
	c.token, c.tokenExp = tok.Token, tok.ExpiresAt
	return c.token, nil
}

func (c *Client) repoPath(suffix string) string {
	return "/repos/" + url.PathEscape(c.cfg.Owner) + "/" + url.PathEscape(c.cfg.Repo) + suffix
}

// do calls the API as the App installation.
func (c *Client) do(ctx context.Context, method, path string, in, out any, hdr http.Header) (*http.Response, error) {
	tok, err := c.installationToken(ctx)
	if err != nil {
		return nil, err
	}
	return c.send(ctx, method, path, "token "+tok, in, out, hdr)
}

func (c *Client) send(ctx context.Context, method, path, auth string, in, out any, hdr http.Header) (*http.Response, error) {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(b)
	}
	u := path
	if !strings.HasPrefix(u, "http") {
		u = c.cfg.APIURL + path
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "trunkcms")
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	maps.Copy(req.Header, hdr)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotModified {
		return resp, nil
	}
	if resp.StatusCode >= 300 {
		var e struct {
			Message string `json:"message"`
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		if json.Unmarshal(b, &e) != nil || e.Message == "" {
			e.Message = strings.TrimSpace(string(b))
		}
		return resp, &APIError{Status: resp.StatusCode, Message: e.Message, Path: method + " " + path}
	}
	if out != nil {
		if w, ok := out.(io.Writer); ok {
			_, err = io.Copy(w, resp.Body)
			return resp, err
		}
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return resp, fmt.Errorf("github: decoding %s: %w", path, err)
		}
	}
	return resp, nil
}

// Permission returns the user's effective permission on the repo:
// "admin", "maintain", "write", "triage", "read" or "none".
func (c *Client) Permission(ctx context.Context, login string) (string, error) {
	var p struct {
		Permission string `json:"permission"`
		RoleName   string `json:"role_name"`
	}
	_, err := c.do(ctx, "GET", c.repoPath("/collaborators/"+url.PathEscape(login)+"/permission"), nil, &p, nil)
	if IsStatus(err, 404) || IsStatus(err, 403) {
		return "none", nil
	}
	if err != nil {
		return "", err
	}
	if p.RoleName == "maintain" {
		return "maintain", nil
	}
	return p.Permission, nil
}

// UserID looks up a GitHub user's numeric ID.
func (c *Client) UserID(ctx context.Context, login string) (int64, error) {
	var u struct {
		ID int64 `json:"id"`
	}
	_, err := c.do(ctx, "GET", "/users/"+url.PathEscape(login), nil, &u, nil)
	return u.ID, err
}

// RepoPrivate reports whether the content repo is private.
func (c *Client) RepoPrivate(ctx context.Context) (bool, error) {
	var r struct {
		Private bool `json:"private"`
	}
	_, err := c.do(ctx, "GET", c.repoPath(""), nil, &r, nil)
	return r.Private, err
}

func (c *Client) RepoURL() string { return c.cfg.WebURL + "/" + c.cfg.Owner + "/" + c.cfg.Repo }
