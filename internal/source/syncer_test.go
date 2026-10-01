package source

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"
)

// memStore is a Store whose head and files tests can swap.
type memStore struct {
	head      atomic.Value // string
	files     atomic.Value // fstest.MapFS
	snapshots atomic.Int32
	heads     atomic.Int32
}

func newMemStore(sha string, files fstest.MapFS) *memStore {
	m := &memStore{}
	m.set(sha, files)
	return m
}

func (m *memStore) set(sha string, files fstest.MapFS) { m.head.Store(sha); m.files.Store(files) }
func (m *memStore) Head(context.Context) (string, error) {
	m.heads.Add(1)
	return m.head.Load().(string), nil
}
func (m *memStore) Snapshot(_ context.Context, _, dir string) error {
	m.snapshots.Add(1)
	files := m.files.Load().(fstest.MapFS)
	return fs.WalkDir(files, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		dst := filepath.Join(dir, filepath.FromSlash(p))
		os.MkdirAll(filepath.Dir(dst), 0o755)
		return os.WriteFile(dst, files[p].Data, 0o644)
	})
}
func (m *memStore) Commit(context.Context, CommitRequest) (string, error) {
	return "", errors.New("read-only")
}

func newSyncer(t *testing.T, store Store) *Syncer {
	s, err := NewSyncer(store, t.TempDir(), 0)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func post(title, date string) *fstest.MapFile {
	return &fstest.MapFile{Data: []byte("---\ntitle: " + title + "\ndate: " + date + "\n---\nbody\n")}
}

func TestSyncerKeepsLastGoodBuild(t *testing.T) {
	store := newMemStore("a", fstest.MapFS{"posts/x.md": post("X", "2026-01-01")})
	s := newSyncer(t, store)
	ctx := context.Background()
	if err := s.Sync(ctx); err != nil || s.Current().SHA != "a" {
		t.Fatalf("first sync: %v", err)
	}

	// A broken site.yaml must not replace the live site.
	store.set("b", fstest.MapFS{"site.yaml": {Data: []byte("permalink: /broken/")}})
	if err := s.Sync(ctx); err == nil {
		t.Fatal("expected build failure")
	}
	if s.Current().SHA != "a" || s.Status().FailedSHA != "b" {
		t.Fatalf("current=%s failed=%s", s.Current().SHA, s.Status().FailedSHA)
	}
	// The broken SHA isn't refetched on every poll.
	n := store.snapshots.Load()
	s.Sync(ctx)
	if store.snapshots.Load() != n {
		t.Fatal("known-bad SHA was refetched")
	}

	// A fixing commit recovers.
	store.set("c", fstest.MapFS{"posts/x.md": post("X", "2026-01-01")})
	if err := s.Sync(ctx); err != nil || s.Current().SHA != "c" || s.Status().LastError != "" {
		t.Fatalf("recovery: %v", err)
	}
}

func TestSyncerPublishesScheduledPosts(t *testing.T) {
	// Front matter dates have second precision, so pick a whole second at least 1s away.
	at := time.Now().Add(2 * time.Second).Truncate(time.Second)
	store := newMemStore("a", fstest.MapFS{"posts/x.md": post("Later", at.UTC().Format(time.RFC3339))})
	s := newSyncer(t, store)
	ctx := context.Background()
	s.Sync(ctx)
	if len(s.Current().Site.Published()) != 0 {
		t.Fatal("scheduled post published early")
	}
	time.Sleep(time.Until(at) + 50*time.Millisecond)
	s.Sync(ctx)
	if len(s.Current().Site.Published()) != 1 {
		t.Fatal("scheduled post should be published once its time passes, without a new commit")
	}
}

func TestSyncerPollsAndChecksOnRequest(t *testing.T) {
	store := newMemStore("a", fstest.MapFS{"posts/x.md": post("X", "2026-01-01")})
	s, err := NewSyncer(store, t.TempDir(), 100*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	s.Sync(context.Background())
	waitFor := func(what string, ok func() bool) {
		t.Helper()
		for deadline := time.Now().Add(2 * time.Second); !ok(); time.Sleep(5 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatal("timed out waiting for " + what)
			}
		}
	}

	// Requests within the interval don't check again.
	heads := store.heads.Load()
	for range 10 {
		s.MaybeSync()
	}
	if store.heads.Load() != heads {
		t.Fatal("checked again before the interval passed")
	}

	// Once the interval passes, one request starts one check.
	store.set("b", fstest.MapFS{"posts/x.md": post("X", "2026-01-01")})
	time.Sleep(110 * time.Millisecond)
	for range 10 {
		s.MaybeSync()
	}
	waitFor("request-triggered sync", func() bool { return s.Current().SHA == "b" })
	if n := store.heads.Load() - heads; n != 1 {
		t.Fatalf("expected one check, got %d", n)
	}

	// Without requests, the poller picks up changes on its own.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)
	store.set("c", fstest.MapFS{"posts/x.md": post("X", "2026-01-01")})
	waitFor("background poll", func() bool { return s.Current().SHA == "c" })
}

func TestSyncerCleansUpOldBuilds(t *testing.T) {
	store := newMemStore("a", fstest.MapFS{
		"posts/x.md": post("Stays the same", "2026-01-01"),
		"posts/y.md": post("Version A", "2026-01-02"),
	})
	s := newSyncer(t, store)
	ctx := context.Background()
	s.Sync(ctx)
	objectOf := func(url string) string { return s.Current().Output.Files[url].Object }
	stable, oldY := objectOf("/posts/x/"), objectOf("/posts/y/")

	for i, v := range []string{"B", "C"} {
		store.set(v, fstest.MapFS{
			"posts/x.md": post("Stays the same", "2026-01-01"),
			"posts/y.md": post("Version "+v, "2026-01-02"),
		})
		if err := s.Sync(ctx); err != nil {
			t.Fatal(err)
		}
		if objectOf("/posts/x/") != stable {
			t.Fatal("an unchanged page should reuse its object")
		}
		_, err := os.Stat(filepath.Join(s.objects.Dir(), oldY))
		if i == 0 && err != nil {
			t.Fatal("the previous build's objects must survive one rebuild for in-flight requests")
		}
		if i == 1 && err == nil {
			t.Fatal("objects two builds old should be removed")
		}
	}
	dirs, _ := os.ReadDir(s.srcDir)
	var names []string
	for _, d := range dirs {
		names = append(names, d.Name())
	}
	if len(dirs) != 2 {
		t.Fatalf("expected current and previous snapshots only, got %s", strings.Join(names, ", "))
	}
}

func TestOverlay(t *testing.T) {
	base := fstest.MapFS{
		"posts/a.md":   {Data: []byte("a")},
		"posts/b.md":   {Data: []byte("b")},
		"assets/x.png": {Data: []byte("x")},
	}
	o := Overlay(base, []Change{
		{Path: "posts/a.md", Content: []byte("A2")},
		{Path: "posts/b.md", Delete: true},
		{Path: "pages/new/deep.md", Content: []byte("new")},
	})
	got := map[string]string{}
	fs.WalkDir(o, ".", func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			b, _ := fs.ReadFile(o, p)
			got[p] = string(b)
		}
		return err
	})
	want := map[string]string{"posts/a.md": "A2", "assets/x.png": "x", "pages/new/deep.md": "new"}
	if len(got) != len(want) {
		t.Fatalf("overlay = %v", got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
	if _, err := fs.Stat(o, "posts/b.md"); err == nil {
		t.Error("deleted file still visible")
	}
}
