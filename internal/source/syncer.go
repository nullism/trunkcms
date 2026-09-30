package source

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nullism/trunkcms/internal/build"
	"github.com/nullism/trunkcms/internal/render"
)

// Status is a point-in-time view of sync health for the admin dashboard.
type Status struct {
	LastCheck   time.Time
	LastError   string
	LastErrorAt time.Time
	FailedSHA   string // head SHA whose build failed; not retried until it changes
}

// Syncer keeps the current build.Result in step with the Store.
// All triggers (webhook, poll, lazy check, read-your-writes) funnel into Sync.
//
// Everything bulky lives under dataDir:
//
//	src/<sha>/   one checkout per snapshot (current and previous are kept)
//	objects/     content-addressed rendered files, shared across builds
type Syncer struct {
	store    Store
	interval time.Duration
	lazy     bool
	srcDir   string
	objects  *render.Objects

	mu          sync.Mutex // serializes syncs and garbage collection
	cur         atomic.Pointer[build.Result]
	prev        *build.Result // kept so in-flight requests on the old build still find their files
	status      atomic.Pointer[Status]
	lastCheck   atomic.Int64
	lazyRunning atomic.Bool
}

// NewSyncer creates a syncer that owns dataDir. interval 0 disables polling;
// lazy means checks run on incoming requests instead of a background ticker.
func NewSyncer(store Store, dataDir string, interval time.Duration, lazy bool) (*Syncer, error) {
	s := &Syncer{store: store, interval: interval, lazy: lazy, srcDir: filepath.Join(dataDir, "src")}
	if err := os.MkdirAll(s.srcDir, 0o755); err != nil {
		return nil, err
	}
	objs, err := render.NewObjects(filepath.Join(dataDir, "objects"))
	if err != nil {
		return nil, err
	}
	s.objects = objs
	s.status.Store(&Status{})
	return s, nil
}

// Current returns the live build, or nil before the first successful sync.
func (s *Syncer) Current() *build.Result { return s.cur.Load() }

func (s *Syncer) Status() Status { return *s.status.Load() }

func (s *Syncer) setStatus(f func(*Status)) {
	st := *s.status.Load()
	f(&st)
	s.status.Store(&st)
}

// Sync checks the branch head and rebuilds if it moved or a scheduled post came due.
func (s *Syncer) Sync(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	s.lastCheck.Store(now.UnixNano())
	s.setStatus(func(st *Status) { st.LastCheck = now })

	head, err := s.store.Head(ctx)
	if err != nil {
		s.fail("", err)
		return err
	}
	cur := s.Current()
	switch {
	case cur == nil, head != cur.SHA:
		if head == s.Status().FailedSHA {
			return nil // already known broken; wait for a new commit
		}
		return s.install(ctx, head, now)
	case !cur.Site.NextPublish.IsZero() && !now.Before(cur.Site.NextPublish):
		slog.Info("scheduled post is due; rebuilding", "at", cur.Site.NextPublish)
		return s.install(ctx, head, now)
	}
	return nil
}

// SyncTo builds a specific commit, used right after this instance commits so
// the editor sees their change without waiting for the API to catch up.
func (s *Syncer) SyncTo(ctx context.Context, sha string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if cur := s.Current(); cur != nil && cur.SHA == sha {
		return nil
	}
	return s.install(ctx, sha, time.Now())
}

// checkout returns the snapshot directory for sha, downloading it if needed.
func (s *Syncer) checkout(ctx context.Context, sha string) (string, error) {
	sum := sha256.Sum256([]byte(sha))
	dir := filepath.Join(s.srcDir, hex.EncodeToString(sum[:8]))
	if _, err := os.Stat(dir); err == nil {
		return dir, nil
	}
	tmp, err := os.MkdirTemp(s.srcDir, ".tmp-")
	if err != nil {
		return "", err
	}
	if err := s.store.Snapshot(ctx, sha, tmp); err != nil {
		os.RemoveAll(tmp)
		return "", err
	}
	if err := os.Rename(tmp, dir); err != nil {
		os.RemoveAll(tmp)
		return "", err
	}
	return dir, nil
}

func (s *Syncer) install(ctx context.Context, sha string, now time.Time) error {
	dir, err := s.checkout(ctx, sha)
	if err != nil {
		s.fail("", err)
		return err
	}
	res, err := build.Run(sha, os.DirFS(dir), s.objects, now)
	if err != nil {
		s.fail(sha, err)
		s.collect(dir)
		return err
	}
	res.SourceDir = dir
	if old := s.cur.Swap(res); old != nil {
		s.prev = old
	}
	s.setStatus(func(st *Status) { st.LastError, st.FailedSHA = "", "" })
	slog.Info("site built", "sha", sha, "posts", len(res.Site.Posts), "files", len(res.Output.Files),
		"problems", len(res.Site.Problems), "took", res.Duration.Round(time.Millisecond))
	s.collect("")
	return nil
}

// collect deletes snapshots and objects not used by the current or previous
// build. Keeping the previous one means requests that started before the swap
// still find their files. extra names a snapshot dir to delete regardless.
func (s *Syncer) collect(extra string) {
	keepDirs := map[string]bool{}
	keepObjs := map[string]bool{}
	for _, r := range []*build.Result{s.Current(), s.prev} {
		if r == nil {
			continue
		}
		keepDirs[r.SourceDir] = true
		for h := range r.Output.Refs() {
			keepObjs[h] = true
		}
	}
	if extra != "" && !keepDirs[extra] {
		os.RemoveAll(extra)
	}
	entries, _ := os.ReadDir(s.srcDir)
	for _, e := range entries {
		if p := filepath.Join(s.srcDir, e.Name()); !keepDirs[p] {
			os.RemoveAll(p)
		}
	}
	if s.Current() == nil {
		return // nothing is being served yet; don't touch objects
	}
	if n, err := s.objects.GC(keepObjs); err != nil {
		slog.Warn("object cleanup", "err", err)
	} else if n > 0 {
		slog.Debug("removed unused objects", "count", n)
	}
}

func (s *Syncer) fail(sha string, err error) {
	slog.Error("sync failed", "sha", sha, "err", err)
	s.setStatus(func(st *Status) {
		st.LastError, st.LastErrorAt = err.Error(), time.Now()
		if sha != "" {
			st.FailedSHA = sha
		}
	})
}

// Run polls in the background until ctx is done (no-op in lazy mode or when disabled).
func (s *Syncer) Run(ctx context.Context) {
	if s.lazy || s.interval <= 0 {
		return
	}
	t := time.NewTicker(s.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.Sync(ctx)
		}
	}
}

// MaybeSync is called on each request in lazy mode. It never blocks the
// request: the current site is served while a stale check runs in the background.
func (s *Syncer) MaybeSync() {
	if !s.lazy || s.interval <= 0 {
		return
	}
	if time.Since(time.Unix(0, s.lastCheck.Load())) < s.interval {
		return
	}
	if !s.lazyRunning.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer s.lazyRunning.Store(false)
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		s.Sync(ctx)
	}()
}

// Kick triggers an asynchronous sync, e.g. from a webhook.
func (s *Syncer) Kick() {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		s.Sync(ctx)
	}()
}

// DataDirSize reports disk used by snapshots and objects, for the dashboard.
func (s *Syncer) DataDirSize() (int64, error) {
	var total int64
	for _, dir := range []string{s.srcDir, s.objects.Dir()} {
		err := filepath.WalkDir(dir, func(_ string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			info, err := d.Info()
			if err == nil {
				total += info.Size()
			}
			return nil
		})
		if err != nil {
			return total, fmt.Errorf("measuring %s: %w", dir, err)
		}
	}
	return total, nil
}
