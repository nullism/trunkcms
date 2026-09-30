// Package auth resolves a request to a policy.User: the session says who you
// are; GitHub repo permission and users.yaml say what role you have.
package auth

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/nullism/trunkcms/internal/content"
	"github.com/nullism/trunkcms/internal/policy"
	"github.com/nullism/trunkcms/internal/session"
)

// PermissionFunc returns a user's GitHub permission on the content repo.
type PermissionFunc func(ctx context.Context, login string) (string, error)

type Resolver struct {
	Sessions *session.Manager
	Perm     PermissionFunc           // nil in local mode
	DevRoles map[string]policy.Role   // local mode: fixed roles for dev logins
	Users    func() content.UsersFile // users.yaml from the live snapshot
	PermTTL  time.Duration

	cache sync.Map // login → cachedPerm
}

type cachedPerm struct {
	perm string
	exp  time.Time
}

// User returns the signed-in user with their current role, or nil if signed out.
// A signed-in user without access gets RoleNone.
func (a *Resolver) User(r *http.Request) (*policy.User, *session.Session) {
	s := a.Sessions.Get(r)
	if s == nil {
		return nil, nil
	}
	return &policy.User{Login: s.Login, ID: s.ID, Role: a.Role(r.Context(), s.Login, s.ID, false)}, s
}

// Role resolves a role; fresh bypasses the permission cache (used at login).
func (a *Resolver) Role(ctx context.Context, login string, id int64, fresh bool) policy.Role {
	if role, ok := a.DevRoles[strings.ToLower(login)]; ok {
		return role
	}
	return policy.ResolveRole(login, id, a.permission(ctx, login, fresh), a.Users())
}

func (a *Resolver) permission(ctx context.Context, login string, fresh bool) string {
	if a.Perm == nil {
		return "none"
	}
	key := strings.ToLower(login)
	if v, ok := a.cache.Load(key); ok && !fresh && time.Now().Before(v.(cachedPerm).exp) {
		return v.(cachedPerm).perm
	}
	ttl := a.PermTTL
	if ttl == 0 {
		ttl = 10 * time.Minute
	}
	perm, err := a.Perm(ctx, login)
	if err != nil {
		slog.Warn("permission lookup failed", "login", login, "err", err)
		perm, ttl = "none", time.Minute
	}
	a.cache.Store(key, cachedPerm{perm, time.Now().Add(ttl)})
	return perm
}
