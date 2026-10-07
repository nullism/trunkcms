package server_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/nullism/trunkcms/internal/admin"
	"github.com/nullism/trunkcms/internal/auth"
	"github.com/nullism/trunkcms/internal/content"
	"github.com/nullism/trunkcms/internal/policy"
	"github.com/nullism/trunkcms/internal/server"
	"github.com/nullism/trunkcms/internal/session"
	"github.com/nullism/trunkcms/internal/source"
)

type env struct {
	t      *testing.T
	srv    *httptest.Server
	dir    string
	syncer *source.Syncer
}

func copyDir(t *testing.T, src, dst string) {
	err := filepath.WalkDir(src, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, b, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
}

func newEnv(t *testing.T, fixture bool) *env {
	dir := t.TempDir()
	if fixture {
		copyDir(t, "../../testdata/site", dir)
	}
	store := source.NewDirStore(dir)
	syncer, err := source.NewSyncer(store, t.TempDir(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := syncer.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	sessions, _ := session.New([][]byte{bytes.Repeat([]byte("k"), 32)}, false)
	roles := map[string]policy.Role{"dev-admin": policy.RoleAdmin, "dev-editor": policy.RoleEditor, "dev-author": policy.RoleAuthor, "other-author": policy.RoleAuthor}
	var devUsers []admin.DevUser
	for l, r := range roles {
		devUsers = append(devUsers, admin.DevUser{Login: l, Role: r})
	}
	resolver := &auth.Resolver{Sessions: sessions, DevRoles: roles, Users: func() content.UsersFile { return syncer.Current().Site.Users }}
	a, err := admin.New(admin.Options{Syncer: syncer, Store: store, Sessions: sessions, Auth: resolver, DevUsers: devUsers})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(&server.Server{Syncer: syncer, Auth: resolver, Admin: a})
	t.Cleanup(srv.Close)
	return &env{t: t, srv: srv, dir: dir, syncer: syncer}
}

type client struct {
	*http.Client
	e *env
}

func (e *env) anon() *client {
	jar, _ := cookiejar.New(nil)
	return &client{&http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, e}
}

func (e *env) as(login string) *client {
	c := e.anon()
	resp, err := c.PostForm(e.srv.URL+"/admin/login", url.Values{"login": {login}})
	if err != nil || resp.StatusCode != http.StatusFound {
		e.t.Fatalf("login as %s: %v %v", login, err, resp.Status)
	}
	return c
}

func (c *client) get(path string) (*http.Response, string) {
	c.e.t.Helper()
	resp, err := c.Get(c.e.srv.URL + path)
	if err != nil {
		c.e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

var csrfRe = regexp.MustCompile(`name="csrf" value="([^"]+)"`)
var baseRe = regexp.MustCompile(`name="base" value="([^"]*)"`)

// form loads an admin page and returns its CSRF token and base SHA.
func (c *client) form(path string) (csrf, base string) {
	c.e.t.Helper()
	_, body := c.get(path)
	m := csrfRe.FindStringSubmatch(body)
	if m == nil {
		c.e.t.Fatalf("no csrf token on %s:\n%s", path, body)
	}
	if b := baseRe.FindStringSubmatch(body); b != nil {
		base = b[1]
	}
	return m[1], base
}

func (c *client) post(path string, v url.Values) (*http.Response, string) {
	c.e.t.Helper()
	resp, err := c.PostForm(c.e.srv.URL+path, v)
	if err != nil {
		c.e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func TestPublicSite(t *testing.T) {
	e := newEnv(t, true)
	c := e.anon()

	resp, body := c.get("/")
	if resp.StatusCode != 200 || !strings.Contains(body, "Hello, world") {
		t.Fatalf("home: %d", resp.StatusCode)
	}
	if strings.Contains(body, "Secret draft") || strings.Contains(body, "from the future") {
		t.Fatal("hidden posts leaked onto the home page")
	}
	if resp, _ := c.get("/posts/hello-world"); resp.StatusCode != http.StatusMovedPermanently || resp.Header.Get("Location") != "/posts/hello-world/" {
		t.Fatalf("expected trailing-slash redirect, got %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	resp, body = c.get("/posts/hello-world/")
	// Code is highlighted with classes, so the theme's CSS controls the colors.
	if !strings.Contains(body, `<pre class="chroma">`) || strings.Contains(body, `<span style=`) {
		t.Fatal("code blocks should be highlighted with classes, not inline styles")
	}
	etag := resp.Header.Get("ETag")
	req, _ := http.NewRequest("GET", e.srv.URL+"/posts/hello-world/", nil)
	req.Header.Set("If-None-Match", etag)
	if r2, _ := c.Do(req); r2.StatusCode != http.StatusNotModified {
		t.Fatalf("conditional GET: %d", r2.StatusCode)
	}

	req, _ = http.NewRequest("GET", e.srv.URL+"/", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	r3, _ := http.DefaultTransport.RoundTrip(req)
	if r3.Header.Get("Content-Encoding") != "gzip" {
		t.Fatal("expected gzip")
	}
	zr, err := gzip.NewReader(r3.Body)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := io.ReadAll(zr); !bytes.Contains(b, []byte("Hello, world")) {
		t.Fatal("gzip body mismatch")
	}

	for path, want := range map[string]string{
		"/posts/bundled-post/dot.svg": "<svg",
		"/index.xml":                  "<rss",
		"/atom.xml":                   "<feed",
		"/sitemap.xml":                "/posts/hello-world/",
		"/tags/notes/":                "A bundled post",
		"/authors/dev-author/":        "Writes the occasional post",
		"/about/":                     "example site",
	} {
		if resp, body := c.get(path); resp.StatusCode != 200 || !strings.Contains(body, want) {
			t.Errorf("%s: %d, missing %q", path, resp.StatusCode, want)
		}
	}
	if resp, _ := c.get("/assets/hello.txt"); !strings.Contains(resp.Header.Get("Content-Security-Policy"), "sandbox") {
		t.Error("assets must be served sandboxed")
	}
	if resp, _ := c.get("/nope/"); resp.StatusCode != 404 {
		t.Errorf("missing page: %d", resp.StatusCode)
	}
}

func TestDraftVisibility(t *testing.T) {
	e := newEnv(t, true)
	const draft = "/posts/secret-draft/"
	cases := map[string]int{"": 404, "other-author": 404, "dev-author": 200, "dev-editor": 200, "dev-admin": 200}
	for who, want := range cases {
		c := e.anon()
		if who != "" {
			c = e.as(who)
		}
		resp, body := c.get(draft)
		if resp.StatusCode != want {
			t.Errorf("%q: status %d, want %d", who, resp.StatusCode, want)
		}
		if want == 200 {
			if !strings.Contains(body, "draft-banner") || resp.Header.Get("Cache-Control") != "private, no-store" {
				t.Errorf("%q: draft must carry a banner and no-store", who)
			}
		} else if strings.Contains(body, "Secret draft") {
			t.Errorf("%q: draft content leaked", who)
		}
	}
}

func TestAdminRequiresLogin(t *testing.T) {
	e := newEnv(t, true)
	resp, _ := e.anon().get("/admin/")
	if resp.StatusCode != http.StatusFound || !strings.HasPrefix(resp.Header.Get("Location"), "/admin/login") {
		t.Fatalf("anonymous admin: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
}

func TestAuthorWorkflow(t *testing.T) {
	e := newEnv(t, true)
	c := e.as("dev-author")

	_, body := c.get("/admin/posts")
	if !strings.Contains(body, "A post by the author") || strings.Contains(body, "Hello, world") {
		t.Fatal("authors should only see their own posts")
	}
	if strings.Contains(body, `href="/admin/settings"`) || strings.Contains(body, `href="/admin/users"`) {
		t.Fatal("authors should not see settings/users navigation")
	}
	if resp, _ := c.get("/admin/settings"); resp.StatusCode != 403 {
		t.Fatalf("author settings: %d", resp.StatusCode)
	}

	csrf, base := c.form("/admin/new?kind=post")
	post := url.Values{"csrf": {csrf}, "base": {base}, "kind": {"post"}, "title": {"My New Post"},
		"date": {"2026-09-30T12:00"}, "tags": {"a, b"}, "author": {"dev-author"}, "body": {"Hello **there**"},
		"upload_prefix": {"assets/uploads/2026/09/"}}

	// Missing CSRF is rejected.
	noCSRF := url.Values{}
	for k, v := range post {
		noCSRF[k] = v
	}
	noCSRF.Del("csrf")
	if resp, _ := c.post("/admin/save", noCSRF); resp.StatusCode != 403 {
		t.Fatalf("save without csrf: %d", resp.StatusCode)
	}

	// Authors can't create posts attributed to someone else.
	stolen := url.Values{}
	for k, v := range post {
		stolen[k] = v
	}
	stolen.Set("author", "dev-editor")
	if resp, body := c.post("/admin/save", stolen); resp.StatusCode != 409 || !strings.Contains(body, "permission") {
		t.Fatalf("author impersonation: %d", resp.StatusCode)
	}

	resp, _ := c.post("/admin/save", post)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("save: %d", resp.StatusCode)
	}
	src, err := os.ReadFile(filepath.Join(e.dir, "posts/2026-09-30-my-new-post.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(src), "author: dev-author") || !strings.Contains(string(src), "tags: [a, b]") {
		t.Fatalf("saved file:\n%s", src)
	}
	if resp, body := e.anon().get("/posts/my-new-post/"); resp.StatusCode != 200 || !strings.Contains(body, "<strong>there</strong>") {
		t.Fatalf("new post not live after save: %d", resp.StatusCode)
	}

	// Authors can't edit someone else's post, even by posting the form directly.
	csrf, base = c.form("/admin/profile")
	resp, body = c.post("/admin/save", url.Values{"csrf": {csrf}, "base": {base}, "kind": {"post"},
		"path": {"posts/2026-09-28-hello-world.md"}, "title": {"Hijacked"}, "date": {"2026-09-28T00:00"},
		"author": {"dev-author"}, "body": {"x"}})
	if resp.StatusCode != 409 || !strings.Contains(body, "permission") {
		t.Fatalf("editing another's post: %d", resp.StatusCode)
	}
	if resp, _ := c.get("/admin/edit?path=posts/2026-09-28-hello-world.md"); resp.StatusCode != 403 {
		t.Fatalf("opening another's post: %d", resp.StatusCode)
	}
}

func TestConflictOnStaleBase(t *testing.T) {
	e := newEnv(t, true)
	c := e.as("dev-editor")
	path := "posts/2026-09-25-authors-post.md"
	csrf, base := c.form("/admin/edit?path=" + path)

	// Someone changes the repo after the editor opened the form.
	os.WriteFile(filepath.Join(e.dir, "pages/new.md"), []byte("---\ntitle: New\n---\n"), 0o644)

	resp, body := c.post("/admin/save", url.Values{"csrf": {csrf}, "base": {base}, "kind": {"post"}, "path": {path},
		"title": {"Edited"}, "date": {"2026-09-25T10:00"}, "author": {"dev-author"}, "body": {"my edits"}})
	if resp.StatusCode != 409 || !strings.Contains(body, "changed this content") || !strings.Contains(body, "my edits") {
		t.Fatalf("expected a conflict that keeps the user's text, got %d", resp.StatusCode)
	}
}

func TestValidationBlocksBrokenSave(t *testing.T) {
	e := newEnv(t, true)
	c := e.as("dev-admin")
	csrf, base := c.form("/admin/new?kind=page")
	resp, body := c.post("/admin/save", url.Values{"csrf": {csrf}, "base": {base}, "kind": {"page"},
		"title": {"Tags"}, "slug": {"tags"}, "body": {"collides with /tags/"}})
	if resp.StatusCode != 409 || !strings.Contains(body, "reserved") {
		t.Fatalf("expected validation error, got %d", resp.StatusCode)
	}
	if _, err := os.Stat(filepath.Join(e.dir, "pages/tags.md")); err == nil {
		t.Fatal("invalid page was written")
	}
}

func TestStagedUpload(t *testing.T) {
	e := newEnv(t, true)
	c := e.as("dev-author")
	csrf, base := c.form("/admin/new?kind=post")

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, v := range map[string]string{"csrf": csrf, "base": base, "kind": "post", "title": "With image",
		"date": "2026-09-30T08:00", "author": "dev-author", "body": "![pic](/assets/uploads/2026/09/my-pic.png)",
		"upload_prefix": "assets/uploads/2026/09/"} {
		mw.WriteField(k, v)
	}
	fw, _ := mw.CreateFormFile("uploads", "My Pic.PNG")
	fw.Write([]byte("\x89PNG fake"))
	mw.Close()
	resp, err := c.Post(e.srv.URL+"/admin/save", mw.FormDataContentType(), &buf)
	if err != nil || resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("upload save: %v %d", err, resp.StatusCode)
	}
	if b, err := os.ReadFile(filepath.Join(e.dir, "assets/uploads/2026/09/my-pic.png")); err != nil || string(b) != "\x89PNG fake" {
		t.Fatalf("upload not committed: %v", err)
	}
	if resp, _ := e.anon().get("/assets/uploads/2026/09/my-pic.png"); resp.StatusCode != 200 {
		t.Fatalf("uploaded asset not served: %d", resp.StatusCode)
	}
}

func TestAdminSettingsAndUsers(t *testing.T) {
	e := newEnv(t, true)
	c := e.as("dev-admin")
	if _, body := c.get("/admin/settings"); !strings.Contains(body, "github.com/nullism/trunkcms") {
		t.Fatal("settings page doesn't show theme info")
	}
	csrf, base := c.form("/admin/settings")
	resp, _ := c.post("/admin/settings", url.Values{"csrf": {csrf}, "base": {base}, "title": {"Renamed Blog"},
		"posts_per_page": {"5"}, "permalink": {"/:year/:slug/"}, "nav": {"Home | /\nAbout | /about/"},
		"rss": {"on"}})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("settings save: %d", resp.StatusCode)
	}
	if _, body := e.anon().get("/2026/hello-world/"); !strings.Contains(body, "Renamed Blog") {
		t.Fatal("settings not applied")
	}

	csrf, base = c.form("/admin/users")
	if resp, _ := c.post("/admin/users", url.Values{"csrf": {csrf}, "base": {base}, "action": {"add"}, "login": {"@NewPerson"}, "role": {"editor"}}); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("add user: %d", resp.StatusCode)
	}
	if u := e.syncer.Current().Site.Users.Users["newperson"]; u.Role != "editor" {
		t.Fatalf("user not added: %+v", u)
	}
}

func TestInitializeEmptyRepo(t *testing.T) {
	e := newEnv(t, false)
	c := e.as("dev-admin")
	_, body := c.get("/admin/")
	if !strings.Contains(body, "Initialize site") {
		t.Fatal("empty repo should offer initialization")
	}
	csrf, base := c.form("/admin/")
	resp, _ := c.post("/admin/init", url.Values{"csrf": {csrf}, "base": {base}, "title": {"Fresh"}})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("init: %d", resp.StatusCode)
	}
	if _, body := e.anon().get("/"); !strings.Contains(body, "Fresh") || !strings.Contains(body, "Hello, world") {
		t.Fatal("initialized site not served")
	}
	if _, body := e.anon().get("/"); !strings.Contains(body, `class="site-search"`) {
		t.Fatal("new sites should start with search on")
	}
}

func TestThemeSelection(t *testing.T) {
	e := newEnv(t, true)
	os.MkdirAll(filepath.Join(e.dir, "themes/paper/static"), 0o755)
	os.WriteFile(filepath.Join(e.dir, "themes/paper/theme.yaml"), []byte("name: Paper\nauthor: Ada\n"), 0o644)
	os.WriteFile(filepath.Join(e.dir, "themes/paper/static/style.css"), []byte("/* paper */"), 0o644)
	os.WriteFile(filepath.Join(e.dir, "themes/paper/screenshot.png"), []byte("\x89PNG fake"), 0o644)
	if err := e.syncer.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}

	c := e.as("dev-admin")
	if _, body := c.get("/admin/settings"); !strings.Contains(body, `name="theme" value="paper"`) || !strings.Contains(body, "by Ada") ||
		!strings.Contains(body, `src="/admin/theme-screenshot?theme=paper&amp;v=`) {
		t.Fatal("settings page doesn't offer the paper theme with its screenshot")
	}
	if resp, body := c.get("/admin/theme-screenshot?theme=paper"); resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "image/png" || body != "\x89PNG fake" {
		t.Fatalf("screenshot: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	if resp, _ := c.get("/admin/theme-screenshot?theme=../posts"); resp.StatusCode != 404 {
		t.Fatalf("screenshot outside themes/: %d", resp.StatusCode)
	}
	if resp, _ := e.as("dev-author").get("/admin/theme-screenshot?theme=paper"); resp.StatusCode != 403 {
		t.Fatalf("screenshot as author: %d", resp.StatusCode)
	}
	save := func(name string) (*http.Response, string) {
		csrf, base := c.form("/admin/settings")
		return c.post("/admin/settings", url.Values{"csrf": {csrf}, "base": {base}, "title": {"Themed"}, "theme": {name}})
	}

	if resp, body := save("missing"); resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, "not found") {
		t.Fatalf("selecting a missing theme: %d", resp.StatusCode)
	}
	if resp, _ := save("paper"); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("selecting paper: %d", resp.StatusCode)
	}
	if b, _ := os.ReadFile(filepath.Join(e.dir, "site.yaml")); !strings.Contains(string(b), "name: paper") {
		t.Fatalf("site.yaml not updated:\n%s", b)
	}
	_, home := e.anon().get("/")
	css := regexp.MustCompile(`href="(/theme/style\.[0-9a-f]+\.css)"`).FindStringSubmatch(home)
	if css == nil {
		t.Fatal("no stylesheet on the home page")
	}
	if _, body := e.anon().get(css[1]); body != "/* paper */" {
		t.Fatalf("stylesheet isn't the paper theme's: %q", body)
	}
	if _, body := c.get("/admin/settings"); !strings.Contains(body, `value="paper" checked`) {
		t.Fatal("paper isn't shown as selected")
	}
}

func TestRawHTML(t *testing.T) {
	e := newEnv(t, true)
	const raw = `<div class="raw">hi</div>`
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(e.dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("pages/raw.md", "---\ntitle: Raw page\n---\n"+raw+"\n")
	write("posts/2026-09-27-raw.md", "---\ntitle: Raw post\n---\n"+raw+"\n")
	sync := func() {
		if err := e.syncer.Sync(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	sync()

	c := e.anon()
	if _, body := c.get("/raw/"); !strings.Contains(body, raw) {
		t.Fatal("page doesn't render raw HTML by default")
	}
	if _, body := c.get("/posts/raw/"); strings.Contains(body, raw) {
		t.Fatal("post renders raw HTML without markdown.unsafe_html")
	}

	b, _ := os.ReadFile(filepath.Join(e.dir, "site.yaml"))
	write("site.yaml", string(b)+"markdown:\n  unsafe_html: true\n")
	sync()
	if _, body := c.get("/posts/raw/"); !strings.Contains(body, raw) {
		t.Fatal("post doesn't render raw HTML with markdown.unsafe_html")
	}
}

var searchIndexRe = regexp.MustCompile(`data-index="([^"]+)"`)

func TestSearchIndex(t *testing.T) {
	e := newEnv(t, true)
	c := e.anon()
	if _, body := c.get("/"); strings.Contains(body, "site-search") {
		t.Fatal("search box shown without search.enabled")
	}
	if _, body := e.as("dev-admin").get("/admin/"); !regexp.MustCompile(`Search index</dt><dd><span class="muted">Off`).MatchString(body) {
		t.Fatal("dashboard should report search as off")
	}

	cfg, _ := os.ReadFile(filepath.Join(e.dir, "site.yaml"))
	setSearch := func(yaml string) (docs []string, terms map[string][]int) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(e.dir, "site.yaml"), append(slices.Clone(cfg), yaml...), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := e.syncer.Sync(context.Background()); err != nil {
			t.Fatal(err)
		}
		_, home := c.get("/")
		m := searchIndexRe.FindStringSubmatch(home)
		if m == nil {
			t.Fatalf("no search box on the home page:\n%s", home)
		}
		resp, body := c.get(m[1])
		// Not immutable: the index changes with every save, at the same URL.
		if resp.StatusCode != http.StatusOK || strings.Contains(resp.Header.Get("Cache-Control"), "immutable") || resp.Header.Get("ETag") == "" {
			t.Fatalf("index: %d %q", resp.StatusCode, resp.Header.Get("Cache-Control"))
		}
		var ix struct {
			Docs  [][]any
			Terms map[string][]int
		}
		if err := json.Unmarshal([]byte(body), &ix); err != nil {
			t.Fatal(err)
		}
		for _, d := range ix.Docs {
			docs = append(docs, d[0].(string))
		}
		return docs, ix.Terms
	}

	docs, terms := setSearch("search:\n  enabled: true\n")
	if !slices.Contains(docs, "/posts/hello-world/") || !slices.Contains(docs, "/about/") {
		t.Fatalf("published post or page missing: %v", docs)
	}
	for _, hidden := range []string{"/posts/secret-draft/", "/posts/future-post/"} {
		if slices.Contains(docs, hidden) {
			t.Fatalf("hidden post %s is in the public index", hidden)
		}
	}
	for _, w := range []string{"secret", "scheduled", "2099"} {
		if _, ok := terms[w]; ok {
			t.Fatalf("word %q from a hidden post is in the index", w)
		}
	}
	if _, ok := terms["trunkcms"]; !ok {
		t.Fatal("body text not indexed")
	}
	if _, body := e.as("dev-admin").get("/admin/"); !regexp.MustCompile(`Search index</dt><dd>[\d.]+ KB`).MatchString(body) {
		t.Fatal("dashboard doesn't show the index size")
	}

	docs, _ = setSearch("search:\n  enabled: true\n  pages: false\n")
	if slices.Contains(docs, "/about/") || !slices.Contains(docs, "/posts/hello-world/") {
		t.Fatalf("pages: false should index posts only: %v", docs)
	}
}

func TestAdminBar(t *testing.T) {
	e := newEnv(t, true)
	const loader = "/admin/static/bar.js"
	for _, p := range []string{"/", "/posts/hello-world/", "/about/", "/tags/", "/authors/dev-author/", "/nope/"} {
		if _, body := e.anon().get(p); !strings.Contains(body, loader) || !strings.Contains(body, "trunkcms_editor=") {
			t.Errorf("%s: no admin bar loader", p)
		}
	}
	if _, body := e.anon().get("/index.xml"); strings.Contains(body, loader) {
		t.Error("loader leaked into the feed")
	}
	if resp, body := e.anon().get(loader); resp.StatusCode != 200 || !strings.Contains(body, "/admin/bar") {
		t.Fatalf("bar.js: %d", resp.StatusCode)
	}

	// Login sets a readable hint cookie next to the HttpOnly session; logout clears both.
	c := e.anon()
	resp, err := c.PostForm(e.srv.URL+"/admin/login", url.Values{"login": {"dev-author"}})
	if err != nil {
		t.Fatal(err)
	}
	var hint *http.Cookie
	for _, ck := range resp.Cookies() {
		if ck.Name == "trunkcms_editor" {
			hint = ck
		}
	}
	if hint == nil || hint.HttpOnly || hint.MaxAge <= 0 {
		t.Fatalf("login should set a readable hint cookie: %+v", hint)
	}

	type info struct{ Login, Role, Edit, EditLabel, Status, New string }
	bar := func(c *client, path string) (int, info) {
		t.Helper()
		resp, body := c.get("/admin/bar?path=" + url.QueryEscape(path))
		var i info
		json.Unmarshal([]byte(body), &i)
		return resp.StatusCode, i
	}
	if code, _ := bar(e.anon(), "/"); code != http.StatusUnauthorized {
		t.Fatalf("anonymous bar: %d", code)
	}
	if code, i := bar(c, "/posts/secret-draft/"); code != 200 || i.Login != "dev-author" ||
		i.Status != "Draft" || !strings.Contains(i.Edit, "secret-draft") || i.New == "" {
		t.Fatalf("author on own draft: %d %+v", code, i)
	}
	if _, i := bar(c, "/posts/future-post"); i.Status != "Scheduled" {
		t.Fatalf("scheduled post without trailing slash: %+v", i)
	}
	if _, i := bar(c, "/about/"); i.Edit != "" {
		t.Fatalf("authors can't edit pages: %+v", i)
	}
	if _, i := bar(c, "/authors/dev-author/"); i.Edit != "/admin/profile?login=dev-author" {
		t.Fatalf("own profile: %+v", i)
	}
	if _, i := bar(e.as("other-author"), "/posts/secret-draft/"); i.Edit != "" {
		t.Fatalf("other author got an edit link: %+v", i)
	}
	if _, i := bar(e.as("dev-editor"), "/about/"); i.Edit != "/admin/edit?path=pages%2Fabout.md" || i.EditLabel != "Edit page" {
		t.Fatalf("editor on a page: %+v", i)
	}

	resp, _ = c.post("/admin/logout", nil)
	cleared := false
	for _, ck := range resp.Cookies() {
		if ck.Name == "trunkcms_editor" && ck.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Fatal("logout should clear the hint cookie")
	}

	b, _ := os.ReadFile(filepath.Join(e.dir, "site.yaml"))
	if err := os.WriteFile(filepath.Join(e.dir, "site.yaml"), append(b, "admin_bar: false\n"...), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := e.syncer.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, body := e.anon().get("/"); strings.Contains(body, loader) {
		t.Fatal("admin_bar: false still injects the loader")
	}
}
