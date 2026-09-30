package source

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
)

// DirStore serves content from a local directory, for development and tests.
// Its "SHA" hashes every file's path, size and modification time, so edits on
// disk are picked up by polling like new commits without reading file contents.
type DirStore struct {
	Root string
	mu   sync.Mutex
}

func NewDirStore(root string) *DirStore { return &DirStore{Root: root} }

func (d *DirStore) walk(fn func(rel string, info fs.FileInfo) error) error {
	root := os.DirFS(d.Root)
	return fs.WalkDir(root, ".", func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if e.IsDir() {
			if e.Name() == ".git" {
				return fs.SkipDir
			}
			return nil
		}
		if !e.Type().IsRegular() {
			return nil
		}
		info, err := e.Info()
		if err != nil {
			return err
		}
		return fn(p, info)
	})
}

func (d *DirStore) Head(ctx context.Context) (string, error) {
	h := sha256.New()
	err := d.walk(func(rel string, info fs.FileInfo) error {
		fmt.Fprintf(h, "%s\x00%d\x00%d\x00", rel, info.Size(), info.ModTime().UnixNano())
		return nil
	})
	return hex.EncodeToString(h.Sum(nil))[:12], err
}

// Snapshot copies the directory, so edits made during a build can't tear it.
func (d *DirStore) Snapshot(ctx context.Context, sha, dir string) error {
	return d.walk(func(rel string, info fs.FileInfo) error {
		return copyFile(filepath.Join(d.Root, filepath.FromSlash(rel)), filepath.Join(dir, filepath.FromSlash(rel)))
	})
}

func copyFile(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func (d *DirStore) Commit(ctx context.Context, req CommitRequest) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	head, err := d.Head(ctx)
	if err != nil {
		return "", err
	}
	if head != req.Base {
		return "", ErrConflict
	}
	for _, c := range req.Changes {
		full := filepath.Join(d.Root, filepath.FromSlash(c.Path))
		if c.Delete {
			if err := os.Remove(full); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return "", err
			}
			// Tidy up empty directories left behind (e.g. a deleted bundle).
			for dir := filepath.Dir(full); dir != filepath.Clean(d.Root); dir = filepath.Dir(dir) {
				if os.Remove(dir) != nil {
					break
				}
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return "", err
		}
		if err := os.WriteFile(full, c.Content, 0o644); err != nil {
			return "", err
		}
	}
	sha, err := d.Head(ctx)
	slog.Info("local commit", "sha", sha, "author", req.Author.Name, "message", req.Message, "files", len(req.Changes))
	return sha, err
}
