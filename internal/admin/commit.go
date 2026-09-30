package admin

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"path"
	"strings"
	"time"

	"github.com/nullism/trunkcms/internal/build"
	"github.com/nullism/trunkcms/internal/content"
	"github.com/nullism/trunkcms/internal/policy"
	"github.com/nullism/trunkcms/internal/source"
)

// userError is shown to the editor as-is.
type userError struct{ msg string }

func (e userError) Error() string { return e.msg }

func userErrorf(format string, a ...any) error { return userError{fmt.Sprintf(format, a...)} }

// commit is the only path from the UI to the Store. It enforces policy on
// every changed path, validates the resulting site, commits, and rebuilds.
func (h *Handler) commit(c *req, base string, changes []source.Change, message string) (string, error) {
	if len(changes) == 0 {
		return "", userErrorf("nothing to save")
	}
	for _, ch := range changes {
		if err := policy.CheckChange(c.u, h.changeInfo(c.res.FS, ch)); err != nil {
			return "", userError{err.Error()}
		}
	}

	// Validation renders every page but writes nothing (nil object store).
	proposed, err := build.Run("", source.Overlay(c.res.FS, changes), nil, time.Now())
	if err != nil {
		return "", userErrorf("this change would break the site, so it wasn't saved: %v", err)
	}
	changed := map[string]bool{}
	for _, ch := range changes {
		changed[ch.Path] = true
	}
	for _, p := range proposed.Site.Problems {
		if changed[p.Path] {
			return "", userErrorf("%v", p)
		}
	}

	name := c.s.Name
	if name == "" {
		name = c.u.Login
	}
	sha, err := h.Store.Commit(c.ctx(), source.CommitRequest{
		Base:    base,
		Changes: changes,
		Message: message + "\n\nEdited with trunkcms by @" + c.u.Login,
		Author:  source.Author{Name: name, Email: fmt.Sprintf("%d+%s@users.noreply.github.com", c.u.ID, c.u.Login)},
	})
	if errors.Is(err, source.ErrConflict) {
		return "", userErrorf("%v. Your text is still below: copy it, reload the page, and apply your changes again.", err)
	}
	if err != nil {
		slog.Error("commit failed", "err", err)
		return "", userErrorf("saving to the repository failed: %v", err)
	}
	if err := h.Syncer.SyncTo(c.ctx(), sha); err != nil {
		slog.Error("rebuild after commit failed", "sha", sha, "err", err)
	}
	return sha, nil
}

// changeInfo resolves ownership for a change so policy can decide on it.
func (h *Handler) changeInfo(snap fs.FS, ch source.Change) policy.Change {
	info := policy.Change{Path: ch.Path, Delete: ch.Delete}
	_, err := fs.Stat(snap, ch.Path)
	info.Exists = err == nil
	if !strings.HasPrefix(ch.Path, "posts/") {
		return info
	}
	// The owner of any file in a post (the .md or a bundle file) is the post's author.
	postFile := ch.Path
	if dir := path.Dir(ch.Path); dir != "posts" {
		postFile = path.Join(strings.Join(strings.SplitN(dir, "/", 3)[:2], "/"), "index.md")
	}
	if src, err := fs.ReadFile(snap, postFile); err == nil {
		info.OldOwner = ownerOf(src)
	}
	if !ch.Delete && strings.HasSuffix(ch.Path, ".md") {
		info.NewOwner = ownerOf(ch.Content)
	}
	return info
}

func ownerOf(src []byte) string {
	var meta content.PostMeta
	if _, err := content.ParseDocument(src, &meta); err != nil {
		return ""
	}
	return strings.ToLower(meta.Author)
}

func commitMessage(custom, fallback string) string {
	if s := strings.TrimSpace(custom); s != "" {
		return s
	}
	return fallback
}
