// Package admin is the editing UI mounted at /admin/.
package admin

import (
	"bytes"
	"context"
	"crypto/subtle"
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/nullism/trunkcms/internal/auth"
	"github.com/nullism/trunkcms/internal/build"
	"github.com/nullism/trunkcms/internal/github"
	"github.com/nullism/trunkcms/internal/policy"
	"github.com/nullism/trunkcms/internal/session"
	"github.com/nullism/trunkcms/internal/source"
)

//go:embed ui
var uiFS embed.FS

const maxUpload = 10 << 20

// DevUser is a selectable identity in local mode (no GitHub).
type DevUser struct {
	Login string
	Role  policy.Role
}

type Options struct {
	Syncer        *source.Syncer
	Store         source.Store
	GitHub        *github.Client // nil in local mode
	Sessions      *session.Manager
	Auth          *auth.Resolver
	SiteURL       string // e.g. https://myblog.com, used for the OAuth callback
	WebhookSecret []byte
	DevUsers      []DevUser
}

type Handler struct {
	Options
	mux  *http.ServeMux
	tmpl map[string]*template.Template

	visMu       sync.Mutex
	repoPrivate *bool
	visChecked  time.Time
}

func New(o Options) (*Handler, error) {
	h := &Handler{Options: o, mux: http.NewServeMux(), tmpl: map[string]*template.Template{}}
	if err := h.parseTemplates(); err != nil {
		return nil, err
	}
	static, _ := fs.Sub(uiFS, "ui/static")
	h.mux.Handle("GET /admin/static/", http.StripPrefix("/admin/static/", http.FileServerFS(static)))

	h.mux.HandleFunc("GET /admin/login", h.login)
	h.mux.HandleFunc("POST /admin/login", h.devLogin)
	h.mux.HandleFunc("GET /admin/callback", h.callback)
	h.mux.HandleFunc("POST /admin/logout", h.logout)
	h.mux.HandleFunc("POST /admin/webhook", h.webhook)

	h.mux.HandleFunc("GET /admin/{$}", h.authed(h.dashboard))
	h.mux.HandleFunc("GET /admin", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/admin/", http.StatusFound) })
	h.mux.HandleFunc("POST /admin/init", h.authed(h.initSite))
	h.mux.HandleFunc("GET /admin/posts", h.authed(h.listPosts))
	h.mux.HandleFunc("GET /admin/pages", h.authed(h.listPages))
	h.mux.HandleFunc("GET /admin/edit", h.authed(h.edit))
	h.mux.HandleFunc("GET /admin/new", h.authed(h.edit))
	h.mux.HandleFunc("POST /admin/preview", h.authed(h.preview))
	h.mux.HandleFunc("POST /admin/save", h.authed(h.save))
	h.mux.HandleFunc("POST /admin/delete", h.authed(h.delete))
	h.mux.HandleFunc("GET /admin/settings", h.authed(h.settings))
	h.mux.HandleFunc("POST /admin/settings", h.authed(h.saveSettings))
	h.mux.HandleFunc("GET /admin/theme-screenshot", h.authed(h.themeScreenshot))
	h.mux.HandleFunc("GET /admin/users", h.authed(h.users))
	h.mux.HandleFunc("POST /admin/users", h.authed(h.saveUsers))
	h.mux.HandleFunc("GET /admin/profile", h.authed(h.profile))
	h.mux.HandleFunc("POST /admin/profile", h.authed(h.saveProfile))
	return h, nil
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Security-Policy", "default-src 'self'; img-src 'self' https: data:; style-src 'self' 'unsafe-inline'; frame-ancestors 'none'")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "same-origin")
	w.Header().Set("Cache-Control", "no-store")
	h.mux.ServeHTTP(w, r)
}

func (h *Handler) parseTemplates() error {
	layout, err := uiFS.ReadFile("ui/layout.html")
	if err != nil {
		return err
	}
	pages, err := fs.Glob(uiFS, "ui/*.html")
	if err != nil {
		return err
	}
	funcs := template.FuncMap{
		"short": func(s string) string { return s[:min(len(s), 7)] },
		"ago":   func(t time.Time) string { return time.Since(t).Round(time.Second).String() + " ago" },
		"date":  func(t time.Time) string { return t.Format("2006-01-02 15:04") },
		"list":  func(s ...string) []string { return s },
	}
	for _, p := range pages {
		name := strings.TrimSuffix(strings.TrimPrefix(p, "ui/"), ".html")
		if name == "layout" {
			continue
		}
		src, err := uiFS.ReadFile(p)
		if err != nil {
			return err
		}
		t, err := template.New("layout").Funcs(funcs).Parse(string(layout))
		if err != nil {
			return fmt.Errorf("admin layout: %w", err)
		}
		if _, err := t.New(name).Parse(string(src)); err != nil {
			return fmt.Errorf("admin %s: %w", name, err)
		}
		h.tmpl[name] = t
	}
	return nil
}

// req bundles what every authenticated handler needs.
type req struct {
	w   http.ResponseWriter
	r   *http.Request
	u   *policy.User
	s   *session.Session
	res *build.Result
}

func (c *req) ctx() context.Context { return c.r.Context() }

// view is the data passed to admin templates.
type view struct {
	Title    string
	User     *policy.User
	CSRF     string
	Res      *build.Result
	Flash    string
	Error    string
	RepoURL  string
	LocalDev bool
	Data     any
}

func (v *view) Can(capability, owner string) bool {
	return policy.Can(v.User, policy.Capability(capability), owner)
}

func (v *view) CanAny(capability string) bool {
	return policy.CanAny(v.User, policy.Capability(capability))
}

func (h *Handler) newView(c *req, title string, data any) *view {
	v := &view{Title: title, User: c.u, Res: c.res, Data: data, LocalDev: h.GitHub == nil}
	if c.s != nil {
		v.CSRF = c.s.CSRF
	}
	if h.GitHub != nil {
		v.RepoURL = h.GitHub.RepoURL()
	}
	if c.r.URL.Query().Has("saved") {
		v.Flash = "Saved. The site has been rebuilt."
	}
	return v
}

func (h *Handler) render(w http.ResponseWriter, status int, name string, v *view) {
	var buf bytes.Buffer
	if err := h.tmpl[name].ExecuteTemplate(&buf, "layout", v); err != nil {
		slog.Error("admin template", "name", name, "err", err)
		http.Error(w, "template error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	w.Write(buf.Bytes())
}

func (h *Handler) message(w http.ResponseWriter, status int, u *policy.User, title, msg string) {
	h.render(w, status, "message", &view{Title: title, User: u, Data: msg, LocalDev: h.GitHub == nil})
}

// authed wraps handlers that need a signed-in user with some role, and
// enforces CSRF on every POST.
func (h *Handler) authed(fn func(*req)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u, s := h.Auth.User(r)
		if u == nil {
			http.Redirect(w, r, "/admin/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusFound)
			return
		}
		if u.Role == policy.RoleNone {
			h.message(w, http.StatusForbidden, nil, "No access",
				"You're signed in as "+u.Login+", but that account doesn't have access to this site. Ask an admin to add you.")
			return
		}
		if r.Method == http.MethodPost {
			r.Body = http.MaxBytesReader(w, r.Body, 4*maxUpload)
			if err := r.ParseMultipartForm(maxUpload); err != nil && err != http.ErrNotMultipart {
				h.message(w, http.StatusBadRequest, u, "Bad request", err.Error())
				return
			}
			if subtle.ConstantTimeCompare([]byte(r.FormValue("csrf")), []byte(s.CSRF)) != 1 {
				h.message(w, http.StatusForbidden, u, "Expired form", "This form expired. Go back, reload the page, and try again.")
				return
			}
		}
		res := h.Syncer.Current()
		if res == nil {
			msg := "The site hasn't finished its first build yet."
			if st := h.Syncer.Status(); st.LastError != "" {
				msg += " Last error: " + st.LastError
			}
			h.message(w, http.StatusServiceUnavailable, u, "Not ready", msg)
			return
		}
		fn(&req{w: w, r: r, u: u, s: s, res: res})
	}
}

func (h *Handler) forbidden(c *req) {
	h.message(c.w, http.StatusForbidden, c.u, "Not allowed", "You don't have permission to do that.")
}

// isRepoPrivate caches the repo visibility check for the dashboard warning.
func (h *Handler) isRepoPrivate(ctx context.Context) *bool {
	if h.GitHub == nil {
		return nil
	}
	h.visMu.Lock()
	defer h.visMu.Unlock()
	if h.repoPrivate == nil || time.Since(h.visChecked) > 10*time.Minute {
		if p, err := h.GitHub.RepoPrivate(ctx); err == nil {
			h.repoPrivate, h.visChecked = &p, time.Now()
		}
	}
	return h.repoPrivate
}
