package github

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/nullism/trunkcms/internal/source"
)

// fakeGitHub implements the subset of the GitHub API trunkcms uses, in memory.
type fakeGitHub struct {
	t        *testing.T
	mu       sync.Mutex
	head     string
	commits  map[string]fakeCommit
	trees    map[string]map[string][]byte
	blobs    map[string][]byte
	headHits int // 200 responses from the head endpoint (304s are free)
	authors  []string
}

type fakeCommit struct {
	tree, parent string
}

func newFake(t *testing.T, files map[string]string) (*fakeGitHub, *Client) {
	f := &fakeGitHub{t: t, commits: map[string]fakeCommit{}, trees: map[string]map[string][]byte{}, blobs: map[string][]byte{}}
	if files != nil {
		m := map[string][]byte{}
		for k, v := range files {
			m[k] = []byte(v)
		}
		f.head = f.addCommit(m, "")
	}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)

	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	pemKey := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	c, err := New(Config{APIURL: srv.URL, WebURL: srv.URL, Owner: "o", Repo: "r", AppID: 1, PrivateKeyPEM: pemKey})
	if err != nil {
		t.Fatal(err)
	}
	return f, c
}

func hashOf(parts ...string) string {
	h := sha1.New()
	for _, p := range parts {
		h.Write([]byte(p))
	}
	return hex.EncodeToString(h.Sum(nil))
}

func (f *fakeGitHub) addTree(files map[string][]byte) string {
	keys := make([]string, 0, len(files))
	for k := range files {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		parts = append(parts, k, string(files[k]))
	}
	sha := hashOf(append([]string{"tree"}, parts...)...)
	f.trees[sha] = files
	return sha
}

func (f *fakeGitHub) addCommit(files map[string][]byte, parent string) string {
	tree := f.addTree(files)
	sha := hashOf("commit", tree, parent, fmt.Sprint(len(f.commits)))
	f.commits[sha] = fakeCommit{tree: tree, parent: parent}
	return sha
}

func (f *fakeGitHub) filesAt(sha string) map[string][]byte { return f.trees[f.commits[sha].tree] }

// push simulates someone else committing directly to the branch.
func (f *fakeGitHub) push(changes map[string]string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	files := maps.Clone(f.filesAt(f.head))
	for k, v := range changes {
		files[k] = []byte(v)
	}
	f.head = f.addCommit(files, f.head)
}

func (f *fakeGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := r.URL.Path
	auth := r.Header.Get("Authorization")
	writeJSON := func(status int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(v)
	}
	var body map[string]any
	if r.Body != nil {
		json.NewDecoder(r.Body).Decode(&body)
	}

	switch {
	case p == "/repos/o/r/installation":
		if !strings.HasPrefix(auth, "Bearer ") {
			writeJSON(401, map[string]string{"message": "JWT required"})
			return
		}
		writeJSON(200, map[string]any{"id": 99})
	case p == "/app/installations/99/access_tokens":
		writeJSON(201, map[string]any{"token": "inst-token", "expires_at": "2099-01-01T00:00:00Z"})
	case auth != "token inst-token":
		writeJSON(401, map[string]string{"message": "bad credentials " + auth})

	case p == "/repos/o/r/commits/main":
		if f.head == "" {
			writeJSON(409, map[string]string{"message": "Git Repository is empty."})
			return
		}
		etag := `"` + f.head + `"`
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(304)
			return
		}
		f.headHits++
		w.Header().Set("ETag", etag)
		w.Write([]byte(f.head))
	case strings.HasPrefix(p, "/repos/o/r/tarball/"):
		sha := strings.TrimPrefix(p, "/repos/o/r/tarball/")
		var buf bytes.Buffer
		zw := gzip.NewWriter(&buf)
		tw := tar.NewWriter(zw)
		tw.WriteHeader(&tar.Header{Name: "o-r-" + sha[:7] + "/", Typeflag: tar.TypeDir, Mode: 0o755})
		for name, data := range f.filesAt(sha) {
			tw.WriteHeader(&tar.Header{Name: "o-r-" + sha[:7] + "/" + name, Typeflag: tar.TypeReg, Size: int64(len(data)), Mode: 0o644})
			tw.Write(data)
		}
		tw.Close()
		zw.Close()
		w.Write(buf.Bytes())
	case strings.HasPrefix(p, "/repos/o/r/git/commits/") && r.Method == "GET":
		c := f.commits[strings.TrimPrefix(p, "/repos/o/r/git/commits/")]
		writeJSON(200, map[string]any{"tree": map[string]string{"sha": c.tree}})
	case p == "/repos/o/r/git/blobs":
		data, _ := base64.StdEncoding.DecodeString(body["content"].(string))
		sha := hashOf("blob", string(data))
		f.blobs[sha] = data
		writeJSON(201, map[string]string{"sha": sha})
	case p == "/repos/o/r/git/trees":
		files := maps.Clone(f.trees[body["base_tree"].(string)])
		for _, e := range body["tree"].([]any) {
			entry := e.(map[string]any)
			path := entry["path"].(string)
			if entry["sha"] == nil {
				delete(files, path)
			} else {
				files[path] = f.blobs[entry["sha"].(string)]
			}
		}
		writeJSON(201, map[string]string{"sha": f.addTree(files)})
	case p == "/repos/o/r/git/commits" && r.Method == "POST":
		parent := body["parents"].([]any)[0].(string)
		sha := hashOf("commit", body["tree"].(string), parent, fmt.Sprint(len(f.commits)))
		f.commits[sha] = fakeCommit{tree: body["tree"].(string), parent: parent}
		author := body["author"].(map[string]any)
		f.authors = append(f.authors, author["name"].(string)+" <"+author["email"].(string)+">")
		writeJSON(201, map[string]string{"sha": sha})
	case p == "/repos/o/r/git/refs/heads/main" && r.Method == "PATCH":
		sha := body["sha"].(string)
		if f.commits[sha].parent != f.head {
			writeJSON(422, map[string]string{"message": "Update is not a fast forward"})
			return
		}
		f.head = sha
		writeJSON(200, map[string]any{"object": map[string]string{"sha": sha}})
	case strings.HasPrefix(p, "/repos/o/r/compare/"):
		base, head, _ := strings.Cut(strings.TrimPrefix(p, "/repos/o/r/compare/"), "...")
		a, b := f.filesAt(base), f.filesAt(head)
		var changed []map[string]string
		for k := range a {
			if !bytes.Equal(a[k], b[k]) {
				changed = append(changed, map[string]string{"filename": k})
			}
		}
		for k := range b {
			if _, ok := a[k]; !ok {
				changed = append(changed, map[string]string{"filename": k})
			}
		}
		writeJSON(200, map[string]any{"files": changed})
	case strings.HasPrefix(p, "/repos/o/r/contents/") && r.Method == "PUT":
		if f.head != "" {
			writeJSON(422, map[string]string{"message": "sha required"})
			return
		}
		data, _ := base64.StdEncoding.DecodeString(body["content"].(string))
		f.head = f.addCommit(map[string][]byte{strings.TrimPrefix(p, "/repos/o/r/contents/"): data}, "")
		writeJSON(201, map[string]any{"commit": map[string]string{"sha": f.head}})
	default:
		f.t.Errorf("fake github: unexpected %s %s", r.Method, p)
		writeJSON(404, map[string]string{"message": "Not Found"})
	}
}

func snapshot(t *testing.T, c *Client, sha string) map[string]string {
	t.Helper()
	dir := t.TempDir()
	if err := c.Snapshot(context.Background(), sha, dir); err != nil {
		t.Fatal(err)
	}
	return readAll(t, os.DirFS(dir))
}

func readAll(t *testing.T, fsys fs.FS) map[string]string {
	out := map[string]string{}
	fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			b, _ := fs.ReadFile(fsys, p)
			out[p] = string(b)
		}
		return nil
	})
	return out
}

func TestHeadUsesETag(t *testing.T) {
	f, c := newFake(t, map[string]string{"site.yaml": "title: x"})
	ctx := context.Background()
	a, err := c.Head(ctx)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := c.Head(ctx)
	if a != b || a != f.head {
		t.Fatalf("head mismatch %s %s %s", a, b, f.head)
	}
	if f.headHits != 1 {
		t.Fatalf("expected second head check to be a free 304, got %d full responses", f.headHits)
	}
	f.push(map[string]string{"x.md": "x"})
	if got, _ := c.Head(ctx); got != f.head {
		t.Fatal("head should change after a push")
	}
}

func TestSnapshot(t *testing.T) {
	f, c := newFake(t, map[string]string{"site.yaml": "title: x", "posts/a.md": "A"})
	got := snapshot(t, c, f.head)
	if len(got) != 2 || got["posts/a.md"] != "A" {
		t.Fatalf("snapshot = %v", got)
	}
}

func TestCommitFastForward(t *testing.T) {
	f, c := newFake(t, map[string]string{"posts/a.md": "A"})
	base := f.head
	sha, err := c.Commit(context.Background(), source.CommitRequest{
		Base: base, Message: "m", Author: source.Author{Name: "Alice", Email: "1+alice@users.noreply.github.com"},
		Changes: []source.Change{{Path: "posts/b.md", Content: []byte("B")}, {Path: "posts/a.md", Delete: true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if sha != f.head {
		t.Fatal("ref not updated")
	}
	files := f.filesAt(sha)
	if string(files["posts/b.md"]) != "B" || files["posts/a.md"] != nil {
		t.Fatalf("files after commit: %v", files)
	}
	if f.authors[0] != "Alice <1+alice@users.noreply.github.com>" {
		t.Fatalf("author = %q", f.authors[0])
	}
}

func TestCommitRebasesOverUnrelatedChange(t *testing.T) {
	f, c := newFake(t, map[string]string{"posts/a.md": "A", "posts/b.md": "B"})
	base := f.head
	f.push(map[string]string{"posts/b.md": "B2"}) // someone else edits a different file
	sha, err := c.Commit(context.Background(), source.CommitRequest{
		Base: base, Message: "m", Changes: []source.Change{{Path: "posts/a.md", Content: []byte("A2")}},
	})
	if err != nil {
		t.Fatalf("expected automatic rebase, got %v", err)
	}
	files := f.filesAt(sha)
	if string(files["posts/a.md"]) != "A2" || string(files["posts/b.md"]) != "B2" {
		t.Fatalf("rebase lost a change: %v", files)
	}
}

func TestCommitConflictOnSameFile(t *testing.T) {
	f, c := newFake(t, map[string]string{"posts/a.md": "A"})
	base := f.head
	f.push(map[string]string{"posts/a.md": "theirs"})
	_, err := c.Commit(context.Background(), source.CommitRequest{
		Base: base, Message: "m", Changes: []source.Change{{Path: "posts/a.md", Content: []byte("mine")}},
	})
	if !errors.Is(err, source.ErrConflict) {
		t.Fatalf("err = %v, want ErrConflict", err)
	}
	if string(f.filesAt(f.head)["posts/a.md"]) != "theirs" {
		t.Fatal("their change was overwritten")
	}
}

func TestCommitToEmptyRepo(t *testing.T) {
	f, c := newFake(t, nil)
	ctx := context.Background()
	if h, err := c.Head(ctx); err != nil || h != "" {
		t.Fatalf("empty repo head = %q, %v", h, err)
	}
	if len(snapshot(t, c, "")) != 0 {
		t.Fatal("empty snapshot expected")
	}
	sha, err := c.Commit(ctx, source.CommitRequest{Message: "init", Changes: []source.Change{
		{Path: "site.yaml", Content: []byte("title: t")},
		{Path: "posts/a.md", Content: []byte("A")},
	}})
	if err != nil {
		t.Fatal(err)
	}
	files := f.filesAt(sha)
	if string(files["site.yaml"]) != "title: t" || string(files["posts/a.md"]) != "A" {
		t.Fatalf("files = %v", files)
	}
}

func TestRepoSubdirectory(t *testing.T) {
	f, c := newFake(t, map[string]string{
		"README.md":         "monorepo",
		"blog1/site.yaml":   "title: one",
		"blog1/posts/a.md":  "A",
		"blog10/posts/x.md": "X", // shares a prefix, must not leak in
		"blog2/site.yaml":   "title: two",
	})
	c.cfg.Dir = "blog1"
	got := snapshot(t, c, f.head)
	if len(got) != 2 || got["site.yaml"] != "title: one" || got["posts/a.md"] != "A" {
		t.Fatalf("snapshot = %v", got)
	}

	base := f.head
	f.push(map[string]string{"blog2/posts/a.md": "other blog"}) // same site path, different blog
	sha, err := c.Commit(context.Background(), source.CommitRequest{
		Base: base, Message: "m", Changes: []source.Change{
			{Path: "posts/a.md", Content: []byte("A2")},
			{Path: "posts/b.md", Content: []byte("B")},
		},
	})
	if err != nil {
		t.Fatalf("expected automatic rebase, got %v", err)
	}
	files := f.filesAt(sha)
	if string(files["blog1/posts/a.md"]) != "A2" || string(files["blog1/posts/b.md"]) != "B" ||
		string(files["blog2/posts/a.md"]) != "other blog" || files["posts/b.md"] != nil {
		t.Fatalf("files after commit: %v", files)
	}

	f.push(map[string]string{"blog1/posts/a.md": "theirs"})
	_, err = c.Commit(context.Background(), source.CommitRequest{
		Base: sha, Message: "m", Changes: []source.Change{{Path: "posts/a.md", Content: []byte("mine")}},
	})
	if !errors.Is(err, source.ErrConflict) {
		t.Fatalf("err = %v, want ErrConflict", err)
	}
}

func TestRepoSubdirectoryInEmptyRepo(t *testing.T) {
	f, c := newFake(t, nil)
	c.cfg.Dir = "blog1"
	sha, err := c.Commit(context.Background(), source.CommitRequest{Message: "init", Changes: []source.Change{
		{Path: "site.yaml", Content: []byte("title: t")},
		{Path: "posts/a.md", Content: []byte("A")},
	}})
	if err != nil {
		t.Fatal(err)
	}
	files := f.filesAt(sha)
	if len(files) != 2 || string(files["blog1/site.yaml"]) != "title: t" || string(files["blog1/posts/a.md"]) != "A" {
		t.Fatalf("files = %v", files)
	}
}

func TestVerifyWebhook(t *testing.T) {
	secret, body := []byte("s3cret"), []byte(`{"ref":"refs/heads/main"}`)
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	good := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	if !VerifyWebhook(secret, body, good) {
		t.Fatal("valid signature rejected")
	}
	if VerifyWebhook(secret, []byte("tampered"), good) || VerifyWebhook(nil, body, good) || VerifyWebhook(secret, body, "sha1=abc") {
		t.Fatal("invalid signature accepted")
	}
}
