// Package policy is the single place that decides who may do what.
// Handlers and templates call Can; nothing else compares roles directly.
package policy

import (
	"fmt"
	"path"
	"strings"

	"github.com/nullism/trunkcms/internal/content"
)

type Role string

const (
	RoleNone   Role = ""
	RoleAuthor Role = "author"
	RoleEditor Role = "editor"
	RoleAdmin  Role = "admin"
)

type Capability string

const (
	PostCreate   Capability = "post.create"
	PostEdit     Capability = "post.edit"
	PostDelete   Capability = "post.delete"
	DraftView    Capability = "draft.view"
	PageEdit     Capability = "page.edit"
	ProfileEdit  Capability = "profile.edit"
	AssetUpload  Capability = "asset.upload"  // add new files under assets/
	AssetReplace Capability = "asset.replace" // overwrite or delete existing assets
	SettingsEdit Capability = "settings.edit"
	UsersManage  Capability = "users.manage"
)

type scope int

const (
	none scope = iota
	own
	all
)

var roles = map[Role]map[Capability]scope{
	RoleAuthor: {
		PostCreate: all, PostEdit: own, PostDelete: own, DraftView: own,
		ProfileEdit: own, AssetUpload: all,
	},
	RoleEditor: {
		PostCreate: all, PostEdit: all, PostDelete: all, DraftView: all,
		PageEdit: all, ProfileEdit: all, AssetUpload: all, AssetReplace: all,
	},
	RoleAdmin: {
		PostCreate: all, PostEdit: all, PostDelete: all, DraftView: all,
		PageEdit: all, ProfileEdit: all, AssetUpload: all, AssetReplace: all,
		SettingsEdit: all, UsersManage: all,
	},
}

type User struct {
	Login string
	ID    int64
	Role  Role
}

// Can reports whether u may use capability c on a resource owned by owner.
// Pass owner "" for resources without an owner; "own" scopes never match "".
func Can(u *User, c Capability, owner string) bool {
	if u == nil {
		return false
	}
	switch roles[u.Role][c] {
	case all:
		return true
	case own:
		return owner != "" && strings.EqualFold(owner, u.Login)
	}
	return false
}

// CanAny reports whether u has c for at least some resources (used to show UI entry points).
func CanAny(u *User, c Capability) bool {
	return u != nil && roles[u.Role][c] != none
}

// ResolveRole combines GitHub repo permission with users.yaml.
// Anyone who can push to the repo directly is an admin: they could edit users.yaml anyway.
func ResolveRole(login string, id int64, repoPermission string, users content.UsersFile) Role {
	switch repoPermission {
	case "admin", "maintain", "write":
		return RoleAdmin
	}
	e, ok := users.Users[strings.ToLower(login)]
	if !ok {
		return RoleNone
	}
	if e.ID != 0 && e.ID != id {
		return RoleNone // username was renamed and reclaimed by someone else
	}
	return Role(e.Role)
}

// Change describes one file in a proposed commit, with owners resolved by the caller.
type Change struct {
	Path     string
	Exists   bool   // file exists in the base snapshot
	Delete   bool   // change removes the file
	OldOwner string // author of the existing post (or its bundle), if any
	NewOwner string // author after the change, for posts
}

// CheckChange enforces capabilities on a single path of a commit.
func CheckChange(u *User, c Change) error {
	p, err := CleanPath(c.Path)
	if err != nil {
		return err
	}
	deny := func(what string) error {
		return fmt.Errorf("you don't have permission to %s (%s)", what, p)
	}
	switch {
	case p == content.ConfigPath:
		if !Can(u, SettingsEdit, "") {
			return deny("change site settings")
		}
	case p == content.UsersPath:
		if !Can(u, UsersManage, "") {
			return deny("manage users")
		}
	case strings.HasPrefix(p, "pages/"):
		if !Can(u, PageEdit, "") {
			return deny("edit pages")
		}
	case strings.HasPrefix(p, "authors/"):
		login := strings.TrimSuffix(path.Base(p), ".md")
		if path.Dir(p) != "authors" || !strings.HasSuffix(p, ".md") || !Can(u, ProfileEdit, login) {
			return deny("edit this profile")
		}
	case strings.HasPrefix(p, "posts/"):
		// OldOwner is also set for new files added to an existing post bundle.
		if c.Exists || c.OldOwner != "" {
			capability := PostEdit
			if c.Delete {
				capability = PostDelete
			}
			if !Can(u, capability, c.OldOwner) {
				return deny("change this post")
			}
		} else if !Can(u, PostCreate, "") {
			return deny("create posts")
		}
		if !c.Delete && strings.HasSuffix(p, ".md") && !Can(u, PostEdit, c.NewOwner) {
			return deny("set this post's author")
		}
	case strings.HasPrefix(p, "assets/"):
		if c.Exists || c.Delete {
			if !Can(u, AssetReplace, "") {
				return deny("replace existing files")
			}
		} else if !Can(u, AssetUpload, "") {
			return deny("upload files")
		}
	default:
		return fmt.Errorf("%s is not editable from the admin UI", p)
	}
	return nil
}

// CleanPath validates a repo-relative path from user input.
func CleanPath(p string) (string, error) {
	if p == "" || strings.HasPrefix(p, "/") || strings.Contains(p, "\\") {
		return "", fmt.Errorf("invalid path %q", p)
	}
	c := path.Clean(p)
	if c != p || c == "." || strings.HasPrefix(c, "../") || c == ".." {
		return "", fmt.Errorf("invalid path %q", p)
	}
	return c, nil
}
