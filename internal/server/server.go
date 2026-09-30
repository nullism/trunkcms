// Package server serves the rendered public site from memory.
package server

import (
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/nullism/trunkcms/internal/auth"
	"github.com/nullism/trunkcms/internal/policy"
	"github.com/nullism/trunkcms/internal/render"
	"github.com/nullism/trunkcms/internal/source"
)

type Server struct {
	Syncer *source.Syncer
	Auth   *auth.Resolver // nil disables draft access (static/local-only mode)
	Admin  http.Handler   // mounted at /admin/; nil disables it
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/healthz":
		w.Write([]byte("ok"))
		return
	case r.URL.Path == "/readyz":
		if s.Syncer.Current() == nil {
			http.Error(w, "not ready: no successful build yet", http.StatusServiceUnavailable)
			return
		}
		w.Write([]byte("ok"))
		return
	case r.URL.Path == "/admin" || strings.HasPrefix(r.URL.Path, "/admin/"):
		if s.Admin == nil {
			http.NotFound(w, r)
			return
		}
		s.Admin.ServeHTTP(w, r)
		return
	}
	s.public(w, r)
}

func (s *Server) public(w http.ResponseWriter, r *http.Request) {
	s.Syncer.MaybeSync()
	res := s.Syncer.Current()
	if res == nil {
		w.Header().Set("Retry-After", "5")
		http.Error(w, "The site is starting up. Try again in a few seconds.", http.StatusServiceUnavailable)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	out := res.Output
	p := r.URL.Path

	if e, ok := out.Files[p]; ok {
		cc := "public, max-age=60"
		if e.Immutable {
			cc = "public, max-age=31536000, immutable"
		}
		if strings.HasPrefix(p, "/assets/") {
			// Uploaded files must never run script on the site's origin if opened directly.
			w.Header().Set("Content-Security-Policy", "sandbox; default-src 'none'; img-src 'self'; style-src 'unsafe-inline'")
		}
		serve(w, r, out, e, cc, http.StatusOK)
		return
	}
	if !strings.HasSuffix(p, "/") {
		_, pub := out.Files[p+"/"]
		_, draft := out.Drafts[p+"/"]
		if pub || draft {
			target := p + "/"
			if r.URL.RawQuery != "" {
				target += "?" + r.URL.RawQuery
			}
			http.Redirect(w, r, target, http.StatusMovedPermanently)
			return
		}
	}
	// Drafts are only looked up on a miss, so anonymous traffic never pays for a session check.
	if d, ok := out.Drafts[p]; ok && s.Auth != nil {
		if u, _ := s.Auth.User(r); policy.Can(u, policy.DraftView, d.Owner) {
			serve(w, r, out, d.Entry, "private, no-store", http.StatusOK)
			return
		}
	}
	serve(w, r, out, out.NotFound, "no-store", http.StatusNotFound)
}

// serve streams an entry's file from the object store. The index lookup
// already happened; this is just "URL → file on disk → bytes".
func serve(w http.ResponseWriter, r *http.Request, out *render.Output, e *render.Entry, cacheControl string, status int) {
	h := w.Header()
	gz := false
	if e.Gzip {
		h.Add("Vary", "Accept-Encoding")
		gz = strings.Contains(r.Header.Get("Accept-Encoding"), "gzip")
	}
	f, err := os.Open(out.Path(e, gz))
	if err != nil {
		slog.Error("object missing", "object", e.Object, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	defer f.Close()
	h.Set("Content-Type", e.ContentType)
	h.Set("Cache-Control", cacheControl)
	h.Set("X-Content-Type-Options", "nosniff")
	if gz {
		h.Set("Content-Encoding", "gzip")
	}
	if status == http.StatusOK {
		// ServeContent handles If-None-Match against this ETag, HEAD, and Range.
		h.Set("ETag", e.ETag)
		http.ServeContent(w, r, "", time.Time{}, f)
		return
	}
	if st, err := f.Stat(); err == nil {
		h.Set("Content-Length", strconv.FormatInt(st.Size(), 10))
	}
	w.WriteHeader(status)
	if r.Method != http.MethodHead {
		io.Copy(w, f)
	}
}
