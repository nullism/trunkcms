package github

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nullism/trunkcms/internal/source"
)

// Snapshot limits. Files are streamed to disk, so these guard disk, not memory.
const (
	maxFileSize  = 100 << 20 // GitHub's own per-file limit
	maxTotalSize = 10 << 30
)

var _ source.Store = (*Client)(nil)

// Head returns the branch head using a conditional request; unchanged heads
// return 304, which doesn't count against the rate limit.
func (c *Client) Head(ctx context.Context) (string, error) {
	c.mu.Lock()
	etag, cached := c.headETag, c.headSHA
	c.mu.Unlock()
	hdr := http.Header{"Accept": {"application/vnd.github.sha"}}
	if etag != "" {
		hdr.Set("If-None-Match", etag)
	}
	var buf bytes.Buffer
	resp, err := c.do(ctx, "GET", c.repoPath("/commits/"+url.PathEscape(c.cfg.Branch)), nil, &buf, hdr)
	if IsStatus(err, http.StatusConflict) {
		return "", nil // empty repository
	}
	if err != nil {
		return "", err
	}
	if resp.StatusCode == http.StatusNotModified {
		return cached, nil
	}
	sha := strings.TrimSpace(buf.String())
	c.mu.Lock()
	c.headETag, c.headSHA = resp.Header.Get("ETag"), sha
	c.mu.Unlock()
	return sha, nil
}

// freshHead bypasses the ETag cache.
func (c *Client) freshHead(ctx context.Context) (string, error) {
	c.mu.Lock()
	c.headETag = ""
	c.mu.Unlock()
	return c.Head(ctx)
}

// Snapshot streams the repo tarball at sha and unpacks it into dir.
func (c *Client) Snapshot(ctx context.Context, sha, dir string) error {
	if sha == "" {
		return nil // empty repository
	}
	tok, err := c.installationToken(ctx)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, "GET", c.cfg.APIURL+c.repoPath("/tarball/"+url.PathEscape(sha)), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "token "+tok)
	req.Header.Set("User-Agent", "trunkcms")
	resp, err := c.http.Do(req) // follows the redirect to codeload
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return &APIError{Status: resp.StatusCode, Message: "tarball download failed", Path: "GET tarball " + sha}
	}
	return ExtractTarball(resp.Body, dir, c.cfg.Dir)
}

// ExtractTarball unpacks a GitHub tarball into dir, stripping the
// "owner-repo-sha/" prefix. If sub is set, only files under that repo
// subdirectory are written, relative to it. Only regular files are written.
func ExtractTarball(r io.Reader, dir, sub string) error {
	zr, err := gzip.NewReader(r)
	if err != nil {
		return err
	}
	tr := tar.NewReader(zr)
	var total int64
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if h.Typeflag != tar.TypeReg {
			continue // directories are implied; symlinks are ignored
		}
		_, name, ok := strings.Cut(h.Name, "/")
		if ok && sub != "" {
			name, ok = strings.CutPrefix(name, sub+"/")
		}
		if !ok || !source.ValidPath(name) {
			continue
		}
		if h.Size > maxFileSize {
			return fmt.Errorf("snapshot: %s is larger than %d MB", name, maxFileSize>>20)
		}
		if total += h.Size; total > maxTotalSize {
			return fmt.Errorf("snapshot: repo is larger than %d GB", maxTotalSize>>30)
		}
		dst := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		f, err := os.Create(dst)
		if err != nil {
			return err
		}
		_, err = io.Copy(f, io.LimitReader(tr, h.Size))
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return err
		}
	}
}

type gitAuthor struct {
	Name  string `json:"name"`
	Email string `json:"email"`
	Date  string `json:"date,omitempty"`
}

type treeEntry struct {
	Path string  `json:"path"`
	Mode string  `json:"mode"`
	Type string  `json:"type"`
	SHA  *string `json:"sha"` // nil deletes the path
}

// Commit writes all changes as one commit via the Git Data API. If the branch
// moved but the new commits touched different files, it rebases once.
func (c *Client) Commit(ctx context.Context, req source.CommitRequest) (string, error) {
	if req.When.IsZero() {
		req.When = time.Now()
	}
	author := gitAuthor{Name: req.Author.Name, Email: req.Author.Email, Date: req.When.UTC().Format(time.RFC3339)}
	changes := req.Changes
	if c.cfg.Dir != "" {
		// Callers use site-relative paths; from here on everything is repo-relative.
		changes = make([]source.Change, len(req.Changes))
		for i, ch := range req.Changes {
			ch.Path = c.cfg.Dir + "/" + ch.Path
			changes[i] = ch
		}
	}
	base := req.Base

	if base == "" {
		// Git Data API doesn't work on an empty repo; create the first file with the Contents API.
		head, err := c.freshHead(ctx)
		if err != nil {
			return "", err
		}
		if head != "" {
			return "", source.ErrConflict
		}
		first := changes[0]
		var res struct {
			Commit struct {
				SHA string `json:"sha"`
			} `json:"commit"`
		}
		_, err = c.do(ctx, "PUT", c.repoPath("/contents/"+escapePath(first.Path)), map[string]any{
			"message": req.Message, "content": base64.StdEncoding.EncodeToString(first.Content),
			"branch": c.cfg.Branch, "author": author, "committer": author,
		}, &res, nil)
		if err != nil {
			return "", err
		}
		base, changes = res.Commit.SHA, changes[1:]
		if len(changes) == 0 {
			return base, nil
		}
	}

	// Blobs don't depend on the base, so a rebase can reuse them.
	blobs := make([]*string, len(changes))
	for i, ch := range changes {
		if ch.Delete {
			continue
		}
		var b struct {
			SHA string `json:"sha"`
		}
		if _, err := c.do(ctx, "POST", c.repoPath("/git/blobs"), map[string]string{
			"content": base64.StdEncoding.EncodeToString(ch.Content), "encoding": "base64",
		}, &b, nil); err != nil {
			return "", err
		}
		blobs[i] = &b.SHA
	}

	for attempt := 0; ; attempt++ {
		sha, commitErr := c.commitOnto(ctx, base, changes, blobs, req.Message, author)
		if commitErr == nil {
			return sha, nil
		}
		// A ref update that isn't a fast-forward comes back as 422.
		if !IsStatus(commitErr, http.StatusUnprocessableEntity) && !IsStatus(commitErr, http.StatusConflict) {
			return "", commitErr
		}
		if attempt > 0 {
			return "", source.ErrConflict
		}
		newHead, err := c.freshHead(ctx)
		if err != nil {
			return "", err
		}
		if newHead == base {
			return "", commitErr // the 422 was about something else
		}
		touched, err := c.changedFiles(ctx, base, newHead)
		if err != nil {
			return "", err
		}
		for _, ch := range changes {
			if touched[ch.Path] {
				return "", source.ErrConflict
			}
		}
		base = newHead
	}
}

func (c *Client) commitOnto(ctx context.Context, base string, changes []source.Change, blobs []*string, msg string, author gitAuthor) (string, error) {
	var parent struct {
		Tree struct {
			SHA string `json:"sha"`
		} `json:"tree"`
	}
	if _, err := c.do(ctx, "GET", c.repoPath("/git/commits/"+base), nil, &parent, nil); err != nil {
		return "", err
	}
	entries := make([]treeEntry, len(changes))
	for i, ch := range changes {
		entries[i] = treeEntry{Path: ch.Path, Mode: "100644", Type: "blob", SHA: blobs[i]}
	}
	var tree struct {
		SHA string `json:"sha"`
	}
	if _, err := c.do(ctx, "POST", c.repoPath("/git/trees"), map[string]any{
		"base_tree": parent.Tree.SHA, "tree": entries,
	}, &tree, nil); err != nil {
		return "", err
	}
	var commit struct {
		SHA string `json:"sha"`
	}
	if _, err := c.do(ctx, "POST", c.repoPath("/git/commits"), map[string]any{
		"message": msg, "tree": tree.SHA, "parents": []string{base}, "author": author,
	}, &commit, nil); err != nil {
		return "", err
	}
	if _, err := c.do(ctx, "PATCH", c.repoPath("/git/refs/heads/"+escapePath(c.cfg.Branch)), map[string]any{
		"sha": commit.SHA, "force": false,
	}, nil, nil); err != nil {
		return "", err
	}
	return commit.SHA, nil
}

// changedFiles lists paths changed between two commits.
func (c *Client) changedFiles(ctx context.Context, base, head string) (map[string]bool, error) {
	var cmp struct {
		Files []struct {
			Filename         string `json:"filename"`
			PreviousFilename string `json:"previous_filename"`
		} `json:"files"`
	}
	if _, err := c.do(ctx, "GET", c.repoPath("/compare/"+base+"..."+head), nil, &cmp, nil); err != nil {
		return nil, err
	}
	if len(cmp.Files) >= 300 {
		return nil, errors.New("too many changes to rebase automatically") // compare API truncates
	}
	out := map[string]bool{}
	for _, f := range cmp.Files {
		out[f.Filename] = true
		if f.PreviousFilename != "" {
			out[f.PreviousFilename] = true
		}
	}
	return out, nil
}

func escapePath(p string) string {
	parts := strings.Split(p, "/")
	for i, s := range parts {
		parts[i] = url.PathEscape(s)
	}
	return strings.Join(parts, "/")
}
