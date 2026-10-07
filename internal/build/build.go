// Package build turns a repo snapshot into an immutable, servable Result.
package build

import (
	"io/fs"
	"time"

	"github.com/nullism/trunkcms/internal/content"
	"github.com/nullism/trunkcms/internal/render"
	"github.com/nullism/trunkcms/internal/theme"
)

// Result is everything derived from one snapshot. It is never mutated after
// creation; the syncer swaps whole Results atomically.
type Result struct {
	SHA       string
	FS        fs.FS  // the snapshot itself, for the admin UI to read sources
	SourceDir string // where the snapshot lives on disk (set by the syncer)
	Site      *content.Site
	Renderer  *render.Renderer
	Output    *render.Output
	BuiltAt   time.Time
	Duration  time.Duration
}

// Run loads and renders a snapshot into objs. A nil objs validates the
// snapshot (every page is rendered) without writing anything.
func Run(sha string, snapshot fs.FS, objs *render.Objects, now time.Time) (*Result, error) {
	return run(sha, snapshot, objs, now, true)
}

// Export renders a snapshot for static hosting: like Run, but without the
// admin bar, since nothing serves /admin there.
func Export(sha string, snapshot fs.FS, objs *render.Objects, now time.Time) (*Result, error) {
	return run(sha, snapshot, objs, now, false)
}

func run(sha string, snapshot fs.FS, objs *render.Objects, now time.Time, server bool) (*Result, error) {
	start := time.Now()
	site, err := content.Load(snapshot, now)
	if err != nil {
		return nil, err
	}
	th, err := theme.ForSnapshot(snapshot, site.Config.Theme.Name)
	if err != nil {
		return nil, err
	}
	r, err := render.New(site, th)
	if err != nil {
		return nil, err
	}
	r.AdminBar = r.AdminBar && server
	out, err := r.Build(snapshot, objs)
	if err != nil {
		return nil, err
	}
	return &Result{
		SHA: sha, FS: snapshot, Site: site, Renderer: r, Output: out,
		BuiltAt: now, Duration: time.Since(start),
	}, nil
}
