package admin

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/nullism/trunkcms/internal/github"
	"github.com/nullism/trunkcms/internal/policy"
)

func safeNext(next string) string {
	if !strings.HasPrefix(next, "/admin") || strings.HasPrefix(next, "//") {
		return "/admin/"
	}
	return next
}

func (h *Handler) callbackURL() string {
	return strings.TrimRight(h.SiteURL, "/") + "/admin/callback"
}

func (h *Handler) login(w http.ResponseWriter, r *http.Request) {
	next := safeNext(r.URL.Query().Get("next"))
	if h.GitHub == nil {
		h.render(w, http.StatusOK, "login", &view{Title: "Sign in", LocalDev: true, Data: map[string]any{
			"Next": next, "Users": h.DevUsers,
		}})
		return
	}
	state, err := h.Sessions.BeginOAuth(w, next)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, h.GitHub.AuthorizeURL(state, h.callbackURL()), http.StatusFound)
}

// devLogin signs in as a configured local user. Only available without GitHub.
func (h *Handler) devLogin(w http.ResponseWriter, r *http.Request) {
	if h.GitHub != nil {
		http.NotFound(w, r)
		return
	}
	login := r.FormValue("login")
	for _, u := range h.DevUsers {
		if u.Login == login {
			h.Sessions.Start(w, u.Login, 0, u.Login)
			http.Redirect(w, r, safeNext(r.FormValue("next")), http.StatusFound)
			return
		}
	}
	http.Error(w, "unknown dev user", http.StatusBadRequest)
}

func (h *Handler) callback(w http.ResponseWriter, r *http.Request) {
	if h.GitHub == nil {
		http.NotFound(w, r)
		return
	}
	q := r.URL.Query()
	next, err := h.Sessions.FinishOAuth(w, r, q.Get("state"))
	if err != nil {
		h.message(w, http.StatusBadRequest, nil, "Sign-in failed", err.Error())
		return
	}
	gu, err := h.GitHub.ExchangeCode(r.Context(), q.Get("code"), h.callbackURL())
	if err != nil {
		slog.Warn("oauth exchange failed", "err", err)
		h.message(w, http.StatusBadGateway, nil, "Sign-in failed", "GitHub sign-in didn't complete. Please try again.")
		return
	}
	role := h.Auth.Role(r.Context(), gu.Login, gu.ID, true)
	if role == policy.RoleNone {
		h.message(w, http.StatusForbidden, nil, "No access",
			"You signed in as "+gu.Login+", but that account doesn't have access to this site. Ask an admin to add you.")
		return
	}
	name := gu.Name
	if name == "" {
		name = gu.Login
	}
	if err := h.Sessions.Start(w, gu.Login, gu.ID, name); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	slog.Info("signed in", "login", gu.Login, "role", role)
	http.Redirect(w, r, next, http.StatusFound)
}

func (h *Handler) logout(w http.ResponseWriter, r *http.Request) {
	// SameSite=Lax already blocks cross-site POSTs carrying the cookie; logout is harmless anyway.
	h.Sessions.Clear(w)
	http.Redirect(w, r, "/", http.StatusFound)
}

// webhook triggers a sync on push events to the content branch.
func (h *Handler) webhook(w http.ResponseWriter, r *http.Request) {
	if len(h.WebhookSecret) == 0 || h.GitHub == nil {
		http.Error(w, "webhook secret not configured", http.StatusNotFound)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 25<<20))
	if err != nil {
		http.Error(w, "body too large", http.StatusRequestEntityTooLarge)
		return
	}
	if !github.VerifyWebhook(h.WebhookSecret, body, r.Header.Get("X-Hub-Signature-256")) {
		http.Error(w, "bad signature", http.StatusUnauthorized)
		return
	}
	switch r.Header.Get("X-GitHub-Event") {
	case "ping":
		w.Write([]byte("pong"))
		return
	case "push":
	default:
		w.WriteHeader(http.StatusNoContent)
		return
	}
	var ev struct {
		Ref string `json:"ref"`
	}
	json.Unmarshal(body, &ev)
	if ev.Ref != "refs/heads/"+h.GitHub.Branch() {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	slog.Info("push webhook received; syncing", "ref", ev.Ref)
	h.Syncer.Kick()
	w.WriteHeader(http.StatusAccepted)
}
