package content

import (
	"errors"
	"fmt"
	"io/fs"
	"path"
	"slices"
	"sort"
	"strings"
	"time"
)

// Site is the parsed content of a repo snapshot.
type Site struct {
	Config    Config
	HasConfig bool    // false when site.yaml is missing (uninitialized repo)
	Posts     []*Post // all posts, newest first, including hidden ones
	Pages     []*Page
	Authors   map[string]*Author
	Users     UsersFile
	Problems  []Problem
	// NextPublish is the earliest future post date, or zero. The syncer rebuilds when it passes.
	NextPublish time.Time
}

// Load parses a snapshot. A broken post or page is skipped and recorded in
// Problems. A broken site.yaml or users.yaml fails the whole load, because
// serving with the wrong settings or permissions is worse than serving stale.
func Load(fsys fs.FS, now time.Time) (*Site, error) {
	s := &Site{Config: DefaultConfig(), Authors: map[string]*Author{}}

	if b, err := ReadFile(fsys, ConfigPath); err == nil {
		if s.Config, err = ParseConfig(b); err != nil {
			return nil, err
		}
		s.HasConfig = true
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}

	s.Users = UsersFile{Users: map[string]UserEntry{}}
	if b, err := ReadFile(fsys, UsersPath); err == nil {
		if s.Users, err = ParseUsers(b); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}

	s.loadPosts(fsys, now)
	s.loadPages(fsys)
	s.loadAuthors(fsys)
	s.checkURLs()
	return s, nil
}

func (s *Site) problem(p string, err error) { s.Problems = append(s.Problems, Problem{p, err}) }

func (s *Site) loadPosts(fsys fs.FS, now time.Time) {
	entries, err := fs.ReadDir(fsys, "posts")
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		switch {
		case e.IsDir():
			p := path.Join("posts", name, "index.md")
			if _, err := fs.Stat(fsys, p); err != nil {
				continue
			}
			s.addPost(fsys, p, name, path.Join("posts", name), now)
		case strings.HasSuffix(name, ".md"):
			s.addPost(fsys, path.Join("posts", name), strings.TrimSuffix(name, ".md"), "", now)
		}
	}
	sort.SliceStable(s.Posts, func(i, j int) bool {
		if !s.Posts[i].Date().Equal(s.Posts[j].Date()) {
			return s.Posts[i].Date().After(s.Posts[j].Date())
		}
		return s.Posts[i].Path < s.Posts[j].Path
	})
}

func (s *Site) addPost(fsys fs.FS, p, base, bundle string, now time.Time) {
	post, err := ParsePost(fsys, p, base)
	if err != nil {
		s.problem(p, err)
		return
	}
	post.BundleDir = bundle
	post.URL = Permalink(s.Config.Permalink, post.Slug, post.Date())
	if post.Date().After(now) {
		post.Scheduled = true
		if !post.Meta.Draft && (s.NextPublish.IsZero() || post.Date().Before(s.NextPublish)) {
			s.NextPublish = post.Date()
		}
	}
	s.Posts = append(s.Posts, post)
}

// ParsePost reads a post; base is the file or bundle name used for defaults.
func ParsePost(fsys fs.FS, p, base string) (*Post, error) {
	src, err := ReadFile(fsys, p)
	if err != nil {
		return nil, err
	}
	return ParsePostBytes(p, base, src)
}

func ParsePostBytes(p, base string, src []byte) (*Post, error) {
	post := &Post{Path: p}
	body, err := ParseDocument(src, &post.Meta)
	if err != nil {
		return nil, err
	}
	post.Body = body
	slug := base
	if m := datePrefix.FindStringSubmatch(base); m != nil {
		slug = m[2]
		if post.Meta.Date.IsZero() {
			post.Meta.Date.Time, _ = time.Parse("2006-01-02", m[1])
		}
	}
	if post.Meta.Slug != "" {
		slug = post.Meta.Slug
	}
	post.Slug = Slugify(slug)
	if post.Slug == "" {
		return nil, fmt.Errorf("empty slug")
	}
	if post.Meta.Date.IsZero() {
		return nil, fmt.Errorf("missing date (set `date:` or prefix the file name with YYYY-MM-DD-)")
	}
	if strings.TrimSpace(post.Meta.Title) == "" {
		return nil, fmt.Errorf("missing title")
	}
	post.Meta.Author = strings.ToLower(post.Meta.Author)
	return post, nil
}

func (s *Site) loadPages(fsys fs.FS) {
	fs.WalkDir(fsys, "pages", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".md") {
			return nil
		}
		src, err := ReadFile(fsys, p)
		if err != nil {
			s.problem(p, err)
			return nil
		}
		page, err := ParsePageBytes(p, src)
		if err != nil {
			s.problem(p, err)
			return nil
		}
		s.Pages = append(s.Pages, page)
		return nil
	})
}

func ParsePageBytes(p string, src []byte) (*Page, error) {
	page := &Page{Path: p}
	body, err := ParseDocument(src, &page.Meta)
	if err != nil {
		return nil, err
	}
	page.Body = body
	rel := strings.TrimSuffix(strings.TrimPrefix(p, "pages/"), ".md")
	rel = strings.TrimSuffix(rel, "index")
	rel = strings.Trim(rel, "/")
	if rel == "" {
		return nil, fmt.Errorf("pages/index.md would replace the home page")
	}
	page.URL = "/" + rel + "/"
	if page.Meta.Title == "" {
		page.Meta.Title = path.Base(rel)
	}
	return page, nil
}

func (s *Site) loadAuthors(fsys fs.FS) {
	entries, err := fs.ReadDir(fsys, "authors")
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		p := path.Join("authors", e.Name())
		src, err := ReadFile(fsys, p)
		if err != nil {
			s.problem(p, err)
			continue
		}
		a, err := ParseAuthorBytes(p, src)
		if err != nil {
			s.problem(p, err)
			continue
		}
		s.Authors[a.Login] = a
	}
}

func ParseAuthorBytes(p string, src []byte) (*Author, error) {
	login := strings.ToLower(strings.TrimSuffix(path.Base(p), ".md"))
	a := &Author{Login: login, Path: p, URL: "/authors/" + login + "/"}
	body, err := ParseDocument(src, &a.Meta)
	if err != nil {
		return nil, err
	}
	a.Body = body
	if a.Meta.Name == "" {
		a.Meta.Name = login
	}
	return a, nil
}

// ReservedPrefixes are URL spaces generated by the engine itself.
var ReservedPrefixes = []string{"/admin/", "/theme/", "/assets/", "/tags/", "/authors/", "/page/"}

// checkURLs drops documents whose URL collides with an earlier one.
func (s *Site) checkURLs() {
	seen := map[string]string{"/": "home page"}
	claim := func(url, p string) bool {
		if prev, ok := seen[url]; ok {
			s.problem(p, fmt.Errorf("URL %s already used by %s", url, prev))
			return false
		}
		for _, reserved := range ReservedPrefixes {
			if strings.HasPrefix(url, reserved) {
				s.problem(p, fmt.Errorf("URL %s is under reserved path %s", url, reserved))
				return false
			}
		}
		seen[url] = p
		return true
	}
	s.Posts = slices.DeleteFunc(s.Posts, func(p *Post) bool { return !claim(p.URL, p.Path) })
	s.Pages = slices.DeleteFunc(s.Pages, func(p *Page) bool { return !claim(p.URL, p.Path) })
}

// Published returns public posts, newest first.
func (s *Site) Published() []*Post {
	var out []*Post
	for _, p := range s.Posts {
		if !p.Hidden() {
			out = append(out, p)
		}
	}
	return out
}

// Tags maps each tag to its published posts.
func (s *Site) Tags() map[string][]*Post {
	m := map[string][]*Post{}
	for _, p := range s.Published() {
		for _, t := range p.Meta.Tags {
			m[t] = append(m[t], p)
		}
	}
	return m
}

// AuthorName resolves a login to a display name, falling back to the login.
func (s *Site) AuthorName(login string) string {
	if a, ok := s.Authors[login]; ok {
		return a.Meta.Name
	}
	if login == "" {
		return s.Config.Author.Name
	}
	return login
}

// Author returns the profile for login, or nil.
func (s *Site) Author(login string) *Author { return s.Authors[login] }

// PostByPath finds a post by its repo path.
func (s *Site) PostByPath(p string) *Post {
	for _, post := range s.Posts {
		if post.Path == p {
			return post
		}
	}
	return nil
}

func (s *Site) PageByPath(p string) *Page {
	for _, page := range s.Pages {
		if page.Path == p {
			return page
		}
	}
	return nil
}
