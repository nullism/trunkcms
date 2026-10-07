package admin

import (
	"fmt"
	"mime"
	"net/http"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/nullism/trunkcms/internal/content"
	"github.com/nullism/trunkcms/internal/policy"
	"github.com/nullism/trunkcms/internal/source"
	"github.com/nullism/trunkcms/internal/theme"
)

func (h *Handler) dashboard(c *req) {
	private := h.isRepoPrivate(c.ctx())
	diskMB := ""
	if n, err := h.Syncer.DataDirSize(); err == nil {
		diskMB = fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	}
	h.render(c.w, http.StatusOK, "dashboard", h.newView(c, "Dashboard", map[string]any{
		"Status":     h.Syncer.Status(),
		"PublicRepo": private != nil && !*private,
		"Disk":       diskMB,
	}))
}

func (h *Handler) initSite(c *req) {
	if c.res.Site.HasConfig {
		http.Redirect(c.w, c.r, "/admin/", http.StatusSeeOther)
		return
	}
	title := strings.TrimSpace(c.r.FormValue("title"))
	if title == "" {
		title = "My Blog"
	}
	cfg := content.DefaultConfig()
	cfg.Title = title
	cfg.BaseURL = strings.TrimRight(h.SiteURL, "/")
	cfg.Nav = []content.Link{{Title: "About", URL: "/about/"}}
	cfg.Search.Enabled = true
	cfgYAML, _ := cfg.Marshal()

	now := time.Now().UTC()
	post, _ := content.FormatDocument(content.PostMeta{
		Title: "Hello, world", Date: content.Date{Time: now}, Tags: []string{"meta"}, Author: c.u.Login,
		Summary: "The first post on " + title + ".",
	}, []byte("Welcome to your new blog! Edit or delete this post from **/admin**.\n\n```go\nfmt.Println(\"hello, world\")\n```\n"))
	about, _ := content.FormatDocument(content.PageMeta{Title: "About"}, []byte("Tell your readers about yourself.\n"))
	profile, _ := content.FormatDocument(content.AuthorMeta{Name: c.s.Name}, []byte("A short bio goes here.\n"))

	changes := []source.Change{
		{Path: content.ConfigPath, Content: cfgYAML},
		{Path: "posts/" + now.Format("2006-01-02") + "-hello-world.md", Content: post},
		{Path: "pages/about.md", Content: about},
		{Path: "authors/" + strings.ToLower(c.u.Login) + ".md", Content: profile},
	}
	if _, err := h.commit(c, c.r.FormValue("base"), changes, "Initialize site"); err != nil {
		h.message(c.w, http.StatusConflict, c.u, "Couldn't initialize", err.Error())
		return
	}
	http.Redirect(c.w, c.r, "/admin/?saved=1", http.StatusSeeOther)
}

// Settings.

func (h *Handler) settings(c *req) {
	if !policy.Can(c.u, policy.SettingsEdit, "") {
		h.forbidden(c)
		return
	}
	cfg := c.res.Site.Config
	h.render(c.w, http.StatusOK, "settings", h.newView(c, "Settings", map[string]any{
		"Config": cfg, "Nav": formatLinks(cfg.Nav), "Themes": theme.List(c.res.FS),
	}))
}

// themeScreenshot serves a theme's screenshot for the theme picker. The page
// links it with the snapshot SHA in the URL, so it can be cached.
func (h *Handler) themeScreenshot(c *req) {
	if !policy.Can(c.u, policy.SettingsEdit, "") {
		h.forbidden(c)
		return
	}
	file, b, err := theme.Screenshot(c.res.FS, c.r.URL.Query().Get("theme"))
	if err != nil {
		http.NotFound(c.w, c.r)
		return
	}
	c.w.Header().Set("Content-Type", mime.TypeByExtension(path.Ext(file)))
	c.w.Header().Set("Cache-Control", "private, max-age=86400")
	c.w.Write(b)
}

func (h *Handler) saveSettings(c *req) {
	if !policy.Can(c.u, policy.SettingsEdit, "") {
		h.forbidden(c)
		return
	}
	r := c.r
	cfg := c.res.Site.Config // starts from current, so Extra keys are kept
	cfg.Title = strings.TrimSpace(r.FormValue("title"))
	cfg.Description = strings.TrimSpace(r.FormValue("description"))
	cfg.BaseURL = strings.TrimRight(strings.TrimSpace(r.FormValue("base_url")), "/")
	cfg.Language = strings.TrimSpace(r.FormValue("language"))
	cfg.Author.Name = strings.TrimSpace(r.FormValue("author_name"))
	cfg.Author.Email = strings.TrimSpace(r.FormValue("author_email"))
	cfg.PostsPerPage, _ = strconv.Atoi(r.FormValue("posts_per_page"))
	cfg.Permalink = strings.TrimSpace(r.FormValue("permalink"))
	cfg.Nav = parseLinks(r.FormValue("nav"))
	cfg.Feeds.RSS = r.FormValue("rss") == "on"
	cfg.Feeds.Atom = r.FormValue("atom") == "on"
	cfg.Theme.Name = r.FormValue("theme")
	cfg.Markdown.UnsafeHTML = r.FormValue("unsafe_html") == "on"
	cfg.AdminBar = r.FormValue("admin_bar") == "on"
	cfg.Search.Enabled = r.FormValue("search") == "on"
	cfg.Search.Pages = r.FormValue("search_pages") == "on"
	cfg.Search.Stopwords = parseWords(r.FormValue("search_stopwords"))
	cfg.Search.Keep = parseWords(r.FormValue("search_keep"))
	cfg.Search.MinLength, _ = strconv.Atoi(r.FormValue("search_min_length"))
	if tb, err := strconv.ParseFloat(strings.TrimSpace(r.FormValue("search_title_boost")), 64); err == nil {
		cfg.Search.TitleBoost = tb
	}

	fail := func(msg string) {
		v := h.newView(c, "Settings", map[string]any{"Config": cfg, "Nav": r.FormValue("nav"), "Themes": theme.List(c.res.FS)})
		v.Error = msg
		h.render(c.w, http.StatusBadRequest, "settings", v)
	}
	if cfg.Title == "" {
		fail("a site title is required")
		return
	}
	b, err := cfg.Marshal()
	if err != nil {
		fail(err.Error())
		return
	}
	if _, err := h.commit(c, r.FormValue("base"), []source.Change{{Path: content.ConfigPath, Content: b}},
		commitMessage(r.FormValue("message"), "Update site settings")); err != nil {
		fail(err.Error())
		return
	}
	http.Redirect(c.w, c.r, "/admin/settings?saved=1", http.StatusSeeOther)
}

func formatLinks(links []content.Link) string {
	var b strings.Builder
	for _, l := range links {
		fmt.Fprintf(&b, "%s | %s\n", l.Title, l.URL)
	}
	return b.String()
}

// parseWords splits a comma- or space-separated word list.
func parseWords(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool { return r == ',' || unicode.IsSpace(r) })
}

func parseLinks(s string) []content.Link {
	var out []content.Link
	for _, line := range strings.Split(s, "\n") {
		title, u, ok := strings.Cut(line, "|")
		if !ok {
			continue
		}
		if title, u = strings.TrimSpace(title), strings.TrimSpace(u); title != "" && u != "" {
			out = append(out, content.Link{Title: title, URL: u})
		}
	}
	return out
}

// Users.

type userRow struct {
	Login string
	content.UserEntry
}

func (h *Handler) users(c *req) {
	if !policy.Can(c.u, policy.UsersManage, "") {
		h.forbidden(c)
		return
	}
	h.render(c.w, http.StatusOK, "users", h.newView(c, "Users", userRows(c.res.Site.Users)))
}

func userRows(u content.UsersFile) []userRow {
	var rows []userRow
	for login, e := range u.Users {
		rows = append(rows, userRow{login, e})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Login < rows[j].Login })
	return rows
}

var githubLogin = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,38})$`)

func (h *Handler) saveUsers(c *req) {
	if !policy.Can(c.u, policy.UsersManage, "") {
		h.forbidden(c)
		return
	}
	r := c.r
	login := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(r.FormValue("login"), "@")))
	role := r.FormValue("role")
	fail := func(msg string) {
		v := h.newView(c, "Users", userRows(c.res.Site.Users))
		v.Error = msg
		h.render(c.w, http.StatusBadRequest, "users", v)
	}
	if !githubLogin.MatchString(login) {
		fail("that doesn't look like a GitHub username")
		return
	}
	users := content.UsersFile{Users: map[string]content.UserEntry{}}
	for k, v := range c.res.Site.Users.Users {
		users.Users[k] = v
	}
	var msg string
	switch r.FormValue("action") {
	case "add", "update":
		if role != "author" && role != "editor" && role != "admin" {
			fail("pick a role")
			return
		}
		e, exists := users.Users[login]
		if !exists && h.GitHub != nil {
			id, err := h.GitHub.UserID(c.ctx(), login)
			if err != nil {
				fail("couldn't find GitHub user " + login)
				return
			}
			e.ID = id
		}
		e.Role = role
		users.Users[login] = e
		msg = fmt.Sprintf("Set @%s as %s", login, role)
	case "remove":
		delete(users.Users, login)
		msg = "Remove @" + login
	default:
		fail("unknown action")
		return
	}
	b, err := users.Marshal()
	if err != nil {
		fail(err.Error())
		return
	}
	if _, err := h.commit(c, r.FormValue("base"), []source.Change{{Path: content.UsersPath, Content: b}}, msg); err != nil {
		fail(err.Error())
		return
	}
	http.Redirect(c.w, c.r, "/admin/users?saved=1", http.StatusSeeOther)
}

// Profiles.

type profileForm struct {
	Login, Name, Avatar, Links, Bio, Base string
}

func (h *Handler) profile(c *req) {
	login := strings.ToLower(c.r.URL.Query().Get("login"))
	if login == "" {
		login = strings.ToLower(c.u.Login)
	}
	if !githubLogin.MatchString(login) || !policy.Can(c.u, policy.ProfileEdit, login) {
		h.forbidden(c)
		return
	}
	f := profileForm{Login: login, Name: c.s.Name, Base: c.res.SHA}
	if a := c.res.Site.Author(login); a != nil {
		f.Name, f.Avatar, f.Links, f.Bio = a.Meta.Name, a.Meta.Avatar, formatLinks(a.Meta.Links), string(a.Body)
	}
	h.render(c.w, http.StatusOK, "profile", h.newView(c, "Profile", f))
}

func (h *Handler) saveProfile(c *req) {
	r := c.r
	f := profileForm{
		Login: strings.ToLower(r.FormValue("login")), Name: strings.TrimSpace(r.FormValue("name")),
		Avatar: strings.TrimSpace(r.FormValue("avatar")), Links: r.FormValue("links"),
		Bio: strings.ReplaceAll(r.FormValue("bio"), "\r\n", "\n"), Base: r.FormValue("base"),
	}
	if !githubLogin.MatchString(f.Login) {
		h.forbidden(c)
		return
	}
	var meta content.AuthorMeta
	if a := c.res.Site.Author(f.Login); a != nil {
		meta = a.Meta
	}
	meta.Name, meta.Avatar, meta.Links = f.Name, f.Avatar, parseLinks(f.Links)
	b, err := content.FormatDocument(meta, []byte(f.Bio))
	if err == nil {
		_, err = h.commit(c, f.Base, []source.Change{{Path: "authors/" + f.Login + ".md", Content: b}},
			"Update profile for @"+f.Login)
	}
	if err != nil {
		v := h.newView(c, "Profile", f)
		v.Error = err.Error()
		h.render(c.w, http.StatusBadRequest, "profile", v)
		return
	}
	http.Redirect(c.w, c.r, "/admin/profile?saved=1&login="+f.Login, http.StatusSeeOther)
}
