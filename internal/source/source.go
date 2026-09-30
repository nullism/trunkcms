// Package source abstracts where content lives (GitHub, or a local directory
// for development) and keeps the current build in sync with it.
package source

import (
	"context"
	"errors"
	"io/fs"
	"path"
	"sort"
	"strings"
	"testing/fstest"
	"time"
)

// ErrConflict means the branch moved in a way that touches the same files.
var ErrConflict = errors.New("someone else changed this content since you opened it")

type Change struct {
	Path    string
	Content []byte
	Delete  bool
}

type Author struct {
	Name  string
	Email string
}

type CommitRequest struct {
	Base    string // SHA the edit was based on
	Changes []Change
	Message string
	Author  Author
	When    time.Time
}

// Store is the content backend.
type Store interface {
	// Head returns the branch's current commit SHA, or "" for an empty repo.
	Head(ctx context.Context) (string, error)
	// Snapshot writes the files at sha into dir, which exists and is empty.
	Snapshot(ctx context.Context, sha, dir string) error
	// Commit applies changes on top of req.Base and returns the new SHA.
	// It returns ErrConflict if another commit changed the same paths.
	Commit(ctx context.Context, req CommitRequest) (string, error)
}

// Overlay returns base with changes applied, for validating a commit before
// making it. Nothing from base is copied; only the changed files are held.
func Overlay(base fs.FS, changes []Change) fs.FS {
	o := overlay{base: base, upper: fstest.MapFS{}, deleted: map[string]bool{}}
	for _, c := range changes {
		if c.Delete {
			o.deleted[c.Path] = true
			delete(o.upper, c.Path)
		} else {
			delete(o.deleted, c.Path)
			o.upper[c.Path] = &fstest.MapFile{Data: c.Content, Mode: 0o644}
		}
	}
	return o
}

type overlay struct {
	base    fs.FS
	upper   fstest.MapFS
	deleted map[string]bool
}

func (o overlay) Open(name string) (fs.File, error) {
	if o.deleted[name] {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
	}
	if f, ok := o.upper[name]; ok && !f.Mode.IsDir() {
		return o.upper.Open(name)
	}
	if f, err := o.base.Open(name); err == nil {
		return f, nil
	}
	return o.upper.Open(name) // directories implied only by new files
}

func (o overlay) ReadDir(name string) ([]fs.DirEntry, error) {
	merged := map[string]fs.DirEntry{}
	baseEntries, baseErr := fs.ReadDir(o.base, name)
	for _, e := range baseEntries {
		if !o.deleted[path.Join(name, e.Name())] {
			merged[e.Name()] = e
		}
	}
	upperEntries, upperErr := fs.ReadDir(o.upper, name)
	for _, e := range upperEntries {
		merged[e.Name()] = e
	}
	if baseErr != nil && upperErr != nil {
		return nil, baseErr
	}
	out := make([]fs.DirEntry, 0, len(merged))
	for _, e := range merged {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out, nil
}

func (o overlay) Stat(name string) (fs.FileInfo, error) {
	f, err := o.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return f.Stat()
}

// ValidPath reports whether a snapshot entry name is safe to write under a directory.
func ValidPath(name string) bool {
	return name != "" && fs.ValidPath(name) && !strings.HasPrefix(name, ".git/") && name != ".git"
}
