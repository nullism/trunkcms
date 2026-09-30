// Package render turns a content.Site into a servable in-memory Output.
package render

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"html/template"
	"io/fs"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	chromahtml "github.com/alecthomas/chroma/v2/formatters/html"
	"github.com/yuin/goldmark"
	highlighting "github.com/yuin/goldmark-highlighting/v2"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	gmhtml "github.com/yuin/goldmark/renderer/html"

	"github.com/nullism/trunkcms/internal/content"
	"github.com/nullism/trunkcms/internal/theme"
)

// Page templates a theme must provide (after layering over the default theme).
var pageTemplates = []string{"list", "post", "page", "author", "tags", "404"}

// Renderer holds a parsed theme and Markdown pipeline for one site.
type Renderer struct {
	site   *content.Site
	Theme  theme.Theme
	md     goldmark.Markdown
	tmpl   map[string]*template.Template
	assets map[string]string     // theme static name → hashed URL
	static map[string]staticFile // hashed URL → file
}

type staticFile struct {
	body        []byte
	contentType string
}

// PostView is a post with rendered HTML and resolved author, as seen by templates.
type PostView struct {
	*content.Post
	Content    template.HTML
	AuthorName string
	AuthorURL  string
}

type PageView struct {
	*content.Page
	Content template.HTML
}

type AuthorView struct {
	Login, Name, Avatar, URL string
	Links                    []content.Link
	Content                  template.HTML
}

type TagCount struct {
	Name  string
	Count int
}

type Pagination struct {
	Page, Total int
	Prev, Next  string
}

// Data is what every template receives.
type Data struct {
	Site        *content.Site
	Theme       *theme.Info
	Title       string
	Description string
	URL         string
	Draft       bool
	Post        *PostView
	Page        *PageView
	Author      *AuthorView
	Posts       []*PostView
	Tag         string
	Tags        []TagCount
	Pagination  *Pagination
}

// New parses the theme and prepares Markdown rendering.
func New(site *content.Site, th theme.Theme) (*Renderer, error) {
	r := &Renderer{site: site, Theme: th, tmpl: map[string]*template.Template{}, assets: map[string]string{}, static: map[string]staticFile{}}

	rendererOpts := []goldmark.Option{
		goldmark.WithExtensions(
			extension.GFM, extension.Footnote, extension.Typographer,
			// Classes rather than inline styles, so the theme's CSS decides the colors.
			highlighting.NewHighlighting(highlighting.WithFormatOptions(chromahtml.WithClasses(true))),
		),
		goldmark.WithParserOptions(parser.WithAutoHeadingID()),
	}
	if site.Config.Markdown.UnsafeHTML {
		rendererOpts = append(rendererOpts, goldmark.WithRendererOptions(gmhtml.WithUnsafe()))
	}
	r.md = goldmark.New(rendererOpts...)

	if err := r.loadStatic(th.FS); err != nil {
		return nil, err
	}
	if err := r.loadTemplates(th.FS); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *Renderer) loadStatic(themeFS fs.FS) error {
	return fs.WalkDir(themeFS, "static", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if p == "static" {
				return fs.SkipDir
			}
			return err
		}
		if d.IsDir() {
			return nil
		}
		b, err := fs.ReadFile(themeFS, p)
		if err != nil {
			return err
		}
		name := strings.TrimPrefix(p, "static/")
		sum := sha256.Sum256(b)
		ext := path.Ext(name)
		url := "/theme/" + strings.TrimSuffix(name, ext) + "." + hex.EncodeToString(sum[:4]) + ext
		r.assets[name] = url
		r.static[url] = staticFile{b, contentTypeFor(name)}
		return nil
	})
}

func (r *Renderer) funcs() template.FuncMap {
	return template.FuncMap{
		"asset": func(name string) (string, error) {
			if u, ok := r.assets[name]; ok {
				return u, nil
			}
			return "", fmt.Errorf("theme asset %q not found", name)
		},
		"absURL": func(p string) string { return r.site.Config.BaseURL + p },
		"date":   func(t time.Time, layout string) string { return t.Format(layout) },
		"tagURL": TagURL,
		"year":   func() int { return time.Now().Year() },
	}
}

func TagURL(tag string) string { return "/tags/" + content.Slugify(tag) + "/" }

func (r *Renderer) loadTemplates(themeFS fs.FS) error {
	read := func(p string) (string, error) {
		b, err := fs.ReadFile(themeFS, p)
		if err != nil {
			return "", fmt.Errorf("theme: %w", err)
		}
		return string(b), nil
	}
	base := template.New("base.html").Funcs(r.funcs())
	src, err := read("templates/base.html")
	if err != nil {
		return err
	}
	if _, err := base.Parse(src); err != nil {
		return fmt.Errorf("theme: %w", err)
	}
	partials, _ := fs.ReadDir(themeFS, "templates/partials")
	for _, e := range partials {
		if e.IsDir() || path.Ext(e.Name()) != ".html" {
			continue
		}
		src, err := read("templates/partials/" + e.Name())
		if err != nil {
			return err
		}
		if _, err := base.New(e.Name()).Parse(src); err != nil {
			return fmt.Errorf("theme: partials/%s: %w", e.Name(), err)
		}
	}
	for _, name := range pageTemplates {
		src, err := read("templates/" + name + ".html")
		if err != nil {
			return err
		}
		t, err := base.Clone()
		if err != nil {
			return err
		}
		if _, err := t.New(name + ".html").Parse(src); err != nil {
			return fmt.Errorf("theme: %s.html: %w", name, err)
		}
		r.tmpl[name] = t
	}
	return nil
}

func (r *Renderer) markdown(src []byte) (template.HTML, error) {
	var buf bytes.Buffer
	if err := r.md.Convert(src, &buf); err != nil {
		return "", err
	}
	return template.HTML(buf.String()), nil
}

func (r *Renderer) execute(name string, d *Data) ([]byte, error) {
	d.Site = r.site
	d.Theme = &r.Theme.Info
	var buf bytes.Buffer
	if err := r.tmpl[name].ExecuteTemplate(&buf, "base.html", d); err != nil {
		return nil, fmt.Errorf("theme: rendering %s for %s: %w", name, d.URL, err)
	}
	return buf.Bytes(), nil
}

// PostView renders a post's Markdown and resolves its author.
func (r *Renderer) PostView(p *content.Post) (*PostView, error) {
	html, err := r.markdown(p.Body)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", p.Path, err)
	}
	v := &PostView{Post: p, Content: html, AuthorName: r.site.AuthorName(p.Meta.Author)}
	if a := r.site.Author(p.Meta.Author); a != nil {
		v.AuthorURL = a.URL
	}
	return v, nil
}

// RenderPost renders a full post page. draft adds the draft banner.
func (r *Renderer) RenderPost(v *PostView, draft bool) ([]byte, error) {
	return r.execute("post", &Data{Title: v.Meta.Title, Description: v.Meta.Summary, URL: v.URL, Post: v, Draft: draft})
}

// RenderPage renders a full static page.
func (r *Renderer) RenderPage(p *content.Page) ([]byte, error) {
	html, err := r.markdown(p.Body)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", p.Path, err)
	}
	return r.execute("page", &Data{Title: p.Meta.Title, URL: p.URL, Page: &PageView{Page: p, Content: html}})
}

// builder writes rendered files into the object store and indexes them.
type builder struct {
	objs *Objects
	out  *Output
}

func (b *builder) add(url string, e *Entry, hidden bool, owner string) {
	if hidden {
		b.out.Drafts[url] = &DraftEntry{Entry: e, Owner: owner}
	} else {
		b.out.Files[url] = e
	}
}

func (b *builder) bytes(url string, body []byte, contentType string) error {
	e, err := b.objs.PutBytes(body, contentType)
	if err != nil {
		return err
	}
	b.add(url, e, false, "")
	return nil
}

// Build renders the whole site into objs. snapshot supplies assets/ and post
// bundle files. A nil objs renders without writing anything (validation).
func (r *Renderer) Build(snapshot fs.FS, objs *Objects) (*Output, error) {
	out := &Output{Objects: objs, Files: map[string]*Entry{}, Drafts: map[string]*DraftEntry{}}
	b := &builder{objs: objs, out: out}
	s := r.site
	for url, f := range r.static {
		e, err := objs.PutBytes(f.body, f.contentType)
		if err != nil {
			return nil, err
		}
		e.Immutable = true
		out.Files[url] = e
	}

	views := map[*content.Post]*PostView{}
	var published []*PostView
	for _, p := range s.Posts {
		v, err := r.PostView(p)
		if err != nil {
			return nil, err
		}
		views[p] = v
		if !p.Hidden() {
			published = append(published, v)
		}
	}

	// Home page with pagination.
	per := s.Config.PostsPerPage
	total := max(1, (len(published)+per-1)/per)
	pageURL := func(n int) string {
		if n == 1 {
			return "/"
		}
		return "/page/" + strconv.Itoa(n) + "/"
	}
	for n := 1; n <= total; n++ {
		pg := &Pagination{Page: n, Total: total}
		if n > 1 {
			pg.Prev = pageURL(n - 1)
		}
		if n < total {
			pg.Next = pageURL(n + 1)
		}
		chunk := published[min((n-1)*per, len(published)):min(n*per, len(published))]
		title := ""
		if n > 1 {
			title = "Page " + strconv.Itoa(n)
		}
		if err := r.emit(b, "list", &Data{Title: title, Description: s.Config.Description, URL: pageURL(n), Posts: chunk, Pagination: pg}); err != nil {
			return nil, err
		}
	}

	// Posts: public ones into Files, drafts and scheduled posts into the draft overlay.
	for _, p := range s.Posts {
		v := views[p]
		body, err := r.RenderPost(v, p.Hidden())
		if err != nil {
			return nil, err
		}
		e, err := objs.PutBytes(body, htmlType)
		if err != nil {
			return nil, err
		}
		b.add(p.URL, e, p.Hidden(), p.Meta.Author)
		if p.BundleDir != "" {
			if err := r.copyBundle(b, snapshot, p); err != nil {
				return nil, err
			}
		}
	}

	for _, p := range s.Pages {
		body, err := r.RenderPage(p)
		if err != nil {
			return nil, err
		}
		if err := b.bytes(p.URL, body, htmlType); err != nil {
			return nil, err
		}
	}

	// Tags.
	tags := s.Tags()
	var counts []TagCount
	for tag, posts := range tags {
		counts = append(counts, TagCount{tag, len(posts)})
		var pv []*PostView
		for _, p := range posts {
			pv = append(pv, views[p])
		}
		if err := r.emit(b, "list", &Data{Title: "#" + tag, URL: TagURL(tag), Posts: pv, Tag: tag}); err != nil {
			return nil, err
		}
	}
	sort.Slice(counts, func(i, j int) bool { return counts[i].Name < counts[j].Name })
	if err := r.emit(b, "tags", &Data{Title: "Tags", URL: "/tags/", Tags: counts}); err != nil {
		return nil, err
	}

	// Author pages: every profile, plus anyone with published posts.
	byAuthor := map[string][]*PostView{}
	for _, v := range published {
		if v.Meta.Author != "" {
			byAuthor[v.Meta.Author] = append(byAuthor[v.Meta.Author], v)
		}
	}
	for login := range s.Authors {
		if _, ok := byAuthor[login]; !ok {
			byAuthor[login] = nil
		}
	}
	for login, posts := range byAuthor {
		av := &AuthorView{Login: login, Name: s.AuthorName(login), URL: "/authors/" + login + "/"}
		if a := s.Author(login); a != nil {
			av.Avatar, av.Links = a.Meta.Avatar, a.Meta.Links
			html, err := r.markdown(a.Body)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", a.Path, err)
			}
			av.Content = html
		}
		if av.Avatar == "" {
			av.Avatar = "https://github.com/" + login + ".png?size=192"
		}
		if err := r.emit(b, "author", &Data{Title: av.Name, URL: av.URL, Author: av, Posts: posts}); err != nil {
			return nil, err
		}
	}

	// assets/ is served verbatim, streamed from the snapshot to the object store.
	err := fs.WalkDir(snapshot, "assets", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if p == "assets" {
				return fs.SkipDir
			}
			return err
		}
		if d.IsDir() {
			return nil
		}
		e, err := objs.PutFile(snapshot, p)
		if err != nil {
			return err
		}
		b.add("/"+p, e, false, "")
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("assets: %w", err)
	}

	if err := r.feeds(b, published); err != nil {
		return nil, err
	}
	if err := r.sitemap(b, published); err != nil {
		return nil, err
	}

	body, err := r.execute("404", &Data{Title: "Not found", URL: "/404"})
	if err != nil {
		return nil, err
	}
	if out.NotFound, err = objs.PutBytes(body, htmlType); err != nil {
		return nil, err
	}
	return out, nil
}

func (r *Renderer) emit(b *builder, tmpl string, d *Data) error {
	body, err := r.execute(tmpl, d)
	if err != nil {
		return err
	}
	return b.bytes(d.URL, body, htmlType)
}

func (r *Renderer) copyBundle(b *builder, snapshot fs.FS, p *content.Post) error {
	return fs.WalkDir(snapshot, p.BundleDir, func(fp string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || fp == p.Path {
			return err
		}
		e, err := b.objs.PutFile(snapshot, fp)
		if err != nil {
			return err
		}
		b.add(p.URL+strings.TrimPrefix(fp, p.BundleDir+"/"), e, p.Hidden(), p.Meta.Author)
		return nil
	})
}
