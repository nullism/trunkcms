package admin

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"

	"github.com/nullism/trunkcms/internal/policy"
)

// barInfo tells the admin bar (ui/static/bar.js) what to offer on one public page.
type barInfo struct {
	Login     string `json:"login"`
	Role      string `json:"role"`
	Edit      string `json:"edit,omitempty"`      // editor URL for this page, if the user may edit it
	EditLabel string `json:"editLabel,omitempty"` // "Edit post", "Edit page", "Edit profile"
	Status    string `json:"status,omitempty"`    // "Draft" or "Scheduled"
	New       string `json:"new,omitempty"`       // new post URL, if the user may create posts
}

// bar answers the admin bar on public pages. It doesn't redirect to login
// like authed handlers: a 401 tells the bar to remove its hint cookie and stay hidden.
func (h *Handler) bar(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	u, _ := h.Auth.User(r)
	if u == nil || u.Role == policy.RoleNone {
		h.Sessions.ClearHint(w)
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte("{}\n"))
		return
	}
	res := h.Syncer.Current()
	if res == nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte("{}\n"))
		return
	}
	info := barInfo{Login: u.Login, Role: string(u.Role)}
	if policy.Can(u, policy.PostCreate, "") {
		info.New = "/admin/new?kind=post"
	}

	p := r.URL.Query().Get("path")
	if p != "" && !strings.HasSuffix(p, "/") {
		p += "/"
	}
	site := res.Site
	if post := site.PostByURL(p); post != nil {
		if policy.Can(u, policy.PostEdit, post.Meta.Author) {
			info.Edit, info.EditLabel = "/admin/edit?path="+url.QueryEscape(post.Path), "Edit post"
		}
		switch {
		case post.Meta.Draft:
			info.Status = "Draft"
		case post.Scheduled:
			info.Status = "Scheduled"
		}
	} else if page := site.PageByURL(p); page != nil {
		if policy.Can(u, policy.PageEdit, "") {
			info.Edit, info.EditLabel = "/admin/edit?path="+url.QueryEscape(page.Path), "Edit page"
		}
	} else if login, ok := strings.CutPrefix(p, "/authors/"); ok {
		login = strings.ToLower(strings.TrimSuffix(login, "/"))
		if githubLogin.MatchString(login) && policy.Can(u, policy.ProfileEdit, login) {
			info.Edit, info.EditLabel = "/admin/profile?login="+url.QueryEscape(login), "Edit profile"
		}
	}
	json.NewEncoder(w).Encode(info)
}
