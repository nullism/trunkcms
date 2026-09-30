package admin

import (
	"errors"
	"fmt"
	"html"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/nullism/trunkcms/internal/content"
	"github.com/nullism/trunkcms/internal/policy"
	"github.com/nullism/trunkcms/internal/source"
)

const formDate = "2006-01-02T15:04"

var uploadExts = map[string]bool{
	".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".webp": true, ".avif": true,
	".pdf": true, ".mp4": true, ".webm": true, ".mp3": true,
}

type postRow struct {
	*content.Post
	Status string
}

func (h *Handler) listPosts(c *req) {
	var rows []postRow
	for _, p := range c.res.Site.Posts {
		if !policy.Can(c.u, policy.PostEdit, p.Meta.Author) {
			continue // authors only see their own posts
		}
		status := "Published"
		switch {
		case p.Meta.Draft:
			status = "Draft"
		case p.Scheduled:
			status = "Scheduled"
		}
		rows = append(rows, postRow{p, status})
	}
	var problems []content.Problem
	if policy.CanAny(c.u, policy.PageEdit) {
		for _, p := range c.res.Site.Problems {
			if strings.HasPrefix(p.Path, "posts/") {
				problems = append(problems, p)
			}
		}
	}
	title := "Posts"
	if !policy.Can(c.u, policy.PostEdit, "") {
		title = "My posts"
	}
	h.render(c.w, http.StatusOK, "posts", h.newView(c, title, map[string]any{"Posts": rows, "Problems": problems}))
}

func (h *Handler) listPages(c *req) {
	if !policy.CanAny(c.u, policy.PageEdit) {
		h.forbidden(c)
		return
	}
	h.render(c.w, http.StatusOK, "pages", h.newView(c, "Pages", c.res.Site.Pages))
}

// postForm is the editor's state, round-tripped through the HTML form.
type postForm struct {
	Kind         string // "post" or "page"
	Path         string // empty for a new document
	Base         string
	Title        string
	Slug         string
	Date         string
	Tags         string
	Summary      string
	Author       string
	AuthorLocked bool
	Draft        bool
	Body         string
	Message      string
	UploadPrefix string
	URL          string
}

func (h *Handler) edit(c *req) {
	q := c.r.URL.Query()
	f := &postForm{Kind: q.Get("kind"), Path: q.Get("path"), Base: c.res.SHA}
	if f.Path != "" {
		if _, err := policy.CleanPath(f.Path); err != nil {
			h.message(c.w, http.StatusBadRequest, c.u, "Bad path", err.Error())
			return
		}
		f.Kind = "page"
		if strings.HasPrefix(f.Path, "posts/") {
			f.Kind = "post"
		}
	}

	switch f.Kind {
	case "post":
		if f.Path == "" {
			if !policy.Can(c.u, policy.PostCreate, "") {
				h.forbidden(c)
				return
			}
			f.Date = time.Now().UTC().Format(formDate)
			f.Author = c.u.Login
			f.Draft = true
		} else {
			p := c.res.Site.PostByPath(f.Path)
			if p == nil {
				h.brokenOrMissing(c, f.Path)
				return
			}
			if !policy.Can(c.u, policy.PostEdit, p.Meta.Author) {
				h.forbidden(c)
				return
			}
			f.Title, f.Slug, f.Summary, f.Author = p.Meta.Title, p.Meta.Slug, p.Meta.Summary, p.Meta.Author
			f.Date = p.Date().UTC().Format(formDate)
			f.Tags = strings.Join(p.Meta.Tags, ", ")
			f.Draft = p.Meta.Draft
			f.Body = string(p.Body)
			f.URL = p.URL
		}
		f.AuthorLocked = !policy.Can(c.u, policy.PostEdit, "")
	case "page":
		if !policy.CanAny(c.u, policy.PageEdit) {
			h.forbidden(c)
			return
		}
		if f.Path != "" {
			p := c.res.Site.PageByPath(f.Path)
			if p == nil {
				h.brokenOrMissing(c, f.Path)
				return
			}
			f.Title, f.Body, f.URL = p.Meta.Title, string(p.Body), p.URL
		}
	default:
		h.message(c.w, http.StatusBadRequest, c.u, "Bad request", "unknown document type")
		return
	}
	f.UploadPrefix = uploadPrefix()
	h.renderEditor(c, http.StatusOK, f, "")
}

func (h *Handler) brokenOrMissing(c *req, p string) {
	for _, prob := range c.res.Site.Problems {
		if prob.Path == p {
			h.message(c.w, http.StatusUnprocessableEntity, c.u, "This file has an error",
				prob.Error()+". Fix it directly in the repository, then reload.")
			return
		}
	}
	h.message(c.w, http.StatusNotFound, c.u, "Not found", p+" doesn't exist.")
}

func (h *Handler) renderEditor(c *req, status int, f *postForm, errMsg string) {
	title := "Edit " + f.Kind
	if f.Path == "" {
		title = "New " + f.Kind
	}
	v := h.newView(c, title, f)
	v.Error = errMsg
	h.render(c.w, status, "edit", v)
}

func uploadPrefix() string { return "assets/uploads/" + time.Now().UTC().Format("2006/01") + "/" }

func readForm(r *http.Request) *postForm {
	return &postForm{
		Kind: r.FormValue("kind"), Path: r.FormValue("path"), Base: r.FormValue("base"),
		Title: strings.TrimSpace(r.FormValue("title")), Slug: strings.TrimSpace(r.FormValue("slug")),
		Date: r.FormValue("date"), Tags: r.FormValue("tags"), Summary: strings.TrimSpace(r.FormValue("summary")),
		Author: strings.ToLower(strings.TrimSpace(r.FormValue("author"))), Draft: r.FormValue("draft") == "on",
		Body: r.FormValue("body"), Message: r.FormValue("message"), UploadPrefix: r.FormValue("upload_prefix"),
		AuthorLocked: r.FormValue("author_locked") == "1",
	}
}

// document turns the form into a repo path and file content, starting from the
// existing file so unknown front matter keys survive.
func (h *Handler) document(c *req, f *postForm) (string, []byte, error) {
	if f.Title == "" {
		return "", nil, userErrorf("a title is required")
	}
	body := []byte(strings.ReplaceAll(f.Body, "\r\n", "\n"))
	switch f.Kind {
	case "post":
		var meta content.PostMeta
		p := f.Path
		if p != "" {
			if existing := c.res.Site.PostByPath(p); existing != nil {
				meta = existing.Meta
			}
		}
		date, err := content.ParseDate(f.Date)
		if err != nil {
			return "", nil, userErrorf("date: %v", err)
		}
		meta.Title, meta.Summary, meta.Draft = f.Title, f.Summary, f.Draft
		meta.Date = content.Date{Time: date}
		meta.Slug = content.Slugify(f.Slug)
		meta.Tags = nil
		for _, t := range strings.Split(f.Tags, ",") {
			if t = strings.TrimSpace(t); t != "" {
				meta.Tags = append(meta.Tags, t)
			}
		}
		meta.Author = f.Author
		if p == "" {
			slug := meta.Slug
			if slug == "" {
				slug = content.Slugify(f.Title)
			}
			if slug == "" {
				return "", nil, userErrorf("couldn't make a URL slug from the title; set a slug")
			}
			if meta.Author == "" {
				meta.Author = c.u.Login
			}
			p = "posts/" + date.Format("2006-01-02") + "-" + slug + ".md"
			if _, err := fs.Stat(c.res.FS, p); err == nil {
				return "", nil, userErrorf("a post file named %s already exists; choose a different slug", p)
			}
		}
		b, err := content.FormatDocument(meta, body)
		return p, b, err
	case "page":
		var meta content.PageMeta
		p := f.Path
		if p != "" {
			if existing := c.res.Site.PageByPath(p); existing != nil {
				meta = existing.Meta
			}
		} else {
			slug := content.Slugify(f.Slug)
			if slug == "" {
				slug = content.Slugify(f.Title)
			}
			p = "pages/" + slug + ".md"
			if _, err := fs.Stat(c.res.FS, p); err == nil {
				return "", nil, userErrorf("a page named %s already exists", p)
			}
		}
		meta.Title = f.Title
		b, err := content.FormatDocument(meta, body)
		return p, b, err
	}
	return "", nil, userErrorf("unknown document type")
}

// preview renders unsaved content with the live theme into the preview iframe.
func (h *Handler) preview(c *req) {
	f := readForm(c.r)
	var out []byte
	p, src, err := h.document(c, f)
	if err == nil {
		switch f.Kind {
		case "post":
			var post *content.Post
			base := strings.TrimSuffix(path.Base(p), ".md")
			if post, err = content.ParsePostBytes(p, base, src); err == nil {
				post.URL = content.Permalink(c.res.Site.Config.Permalink, post.Slug, post.Date())
				if v, verr := c.res.Renderer.PostView(post); verr != nil {
					err = verr
				} else {
					out, err = c.res.Renderer.RenderPost(v, post.Meta.Draft || post.Date().After(time.Now()))
				}
			}
		case "page":
			var page *content.Page
			if page, err = content.ParsePageBytes(p, src); err == nil {
				out, err = c.res.Renderer.RenderPage(page)
			}
		}
	}
	if err != nil {
		out = []byte("<!doctype html><meta charset=utf-8><p style=\"font:14px system-ui;color:#b91c1c;padding:1rem\">Preview unavailable: " +
			html.EscapeString(err.Error()) + "</p>")
	}
	// The preview is framed by the editor, so relax framing for this response only; no scripts may run.
	c.w.Header().Set("Content-Security-Policy", "script-src 'none'; frame-ancestors 'self'")
	c.w.Header().Set("X-Frame-Options", "SAMEORIGIN")
	c.w.Header().Set("Content-Type", "text/html; charset=utf-8")
	c.w.Write(out)
}

func (h *Handler) save(c *req) {
	f := readForm(c.r)
	if f.Path != "" {
		if _, err := policy.CleanPath(f.Path); err != nil {
			h.renderEditor(c, http.StatusBadRequest, f, err.Error())
			return
		}
	}
	isNew := f.Path == ""
	p, src, err := h.document(c, f)
	if err != nil {
		h.renderEditor(c, http.StatusBadRequest, f, err.Error())
		return
	}
	changes := []source.Change{{Path: p, Content: src}}
	uploads, err := h.stagedUploads(c, f.UploadPrefix)
	if err != nil {
		h.renderEditor(c, http.StatusBadRequest, f, err.Error())
		return
	}
	changes = append(changes, uploads...)

	verb := "Update"
	if isNew {
		verb = "Create"
	}
	_, err = h.commit(c, f.Base, changes, commitMessage(f.Message, fmt.Sprintf("%s %s: %s", verb, f.Kind, f.Title)))
	if err != nil {
		var ue userError
		status := http.StatusInternalServerError
		if errors.As(err, &ue) {
			status = http.StatusConflict
		}
		h.renderEditor(c, status, f, err.Error())
		return
	}
	http.Redirect(c.w, c.r, "/admin/edit?saved=1&path="+url.QueryEscape(p), http.StatusSeeOther)
}

// stagedUploads turns files attached to the editor form into changes, so they
// land in the same commit as the post that references them.
func (h *Handler) stagedUploads(c *req, prefix string) ([]source.Change, error) {
	if c.r.MultipartForm == nil {
		return nil, nil
	}
	files := c.r.MultipartForm.File["uploads"]
	if len(files) == 0 {
		return nil, nil
	}
	if !strings.HasPrefix(prefix, "assets/uploads/") || !strings.HasSuffix(prefix, "/") {
		return nil, userErrorf("invalid upload location")
	}
	var out []source.Change
	for _, fh := range files {
		name := content.SafeFileName(fh.Filename)
		if !uploadExts[path.Ext(name)] {
			return nil, userErrorf("%s: this file type can't be uploaded", fh.Filename)
		}
		if fh.Size > maxUpload {
			return nil, userErrorf("%s is larger than %d MB", fh.Filename, maxUpload>>20)
		}
		p, err := policy.CleanPath(prefix + name)
		if err != nil {
			return nil, err
		}
		f, err := fh.Open()
		if err != nil {
			return nil, err
		}
		b, err := io.ReadAll(io.LimitReader(f, maxUpload+1))
		f.Close()
		if err != nil {
			return nil, err
		}
		out = append(out, source.Change{Path: p, Content: b})
	}
	return out, nil
}

func (h *Handler) delete(c *req) {
	p, err := policy.CleanPath(c.r.FormValue("path"))
	if err != nil {
		h.message(c.w, http.StatusBadRequest, c.u, "Bad path", err.Error())
		return
	}
	changes := []source.Change{{Path: p, Delete: true}}
	// Deleting a bundle's index.md removes the whole bundle.
	if strings.HasPrefix(p, "posts/") && path.Base(p) == "index.md" {
		changes = nil
		fs.WalkDir(c.res.FS, path.Dir(p), func(fp string, d fs.DirEntry, err error) error {
			if err == nil && !d.IsDir() {
				changes = append(changes, source.Change{Path: fp, Delete: true})
			}
			return nil
		})
	}
	back := "/admin/pages"
	if strings.HasPrefix(p, "posts/") {
		back = "/admin/posts"
	}
	if _, err := h.commit(c, c.r.FormValue("base"), changes, "Delete "+p); err != nil {
		h.message(c.w, http.StatusConflict, c.u, "Couldn't delete", err.Error())
		return
	}
	http.Redirect(c.w, c.r, back+"?saved=1", http.StatusSeeOther)
}
