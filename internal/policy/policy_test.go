package policy

import (
	"testing"

	"github.com/nullism/trunkcms/internal/content"
)

func TestCheckChange(t *testing.T) {
	author := &User{Login: "alice", Role: RoleAuthor}
	editor := &User{Login: "sally", Role: RoleEditor}
	admin := &User{Login: "root", Role: RoleAdmin}

	tests := []struct {
		name string
		u    *User
		c    Change
		ok   bool
	}{
		{"author creates own post", author, Change{Path: "posts/x.md", NewOwner: "alice"}, true},
		{"author creates post for someone else", author, Change{Path: "posts/x.md", NewOwner: "bob"}, false},
		{"author edits own post", author, Change{Path: "posts/x.md", Exists: true, OldOwner: "alice", NewOwner: "alice"}, true},
		{"author hands post to someone else", author, Change{Path: "posts/x.md", Exists: true, OldOwner: "alice", NewOwner: "bob"}, false},
		{"author edits other's post", author, Change{Path: "posts/x.md", Exists: true, OldOwner: "bob", NewOwner: "bob"}, false},
		{"author edits unowned post", author, Change{Path: "posts/x.md", Exists: true}, false},
		{"author deletes own post", author, Change{Path: "posts/x.md", Exists: true, Delete: true, OldOwner: "alice"}, true},
		{"author adds file to other's bundle", author, Change{Path: "posts/b/img.png", OldOwner: "bob"}, false},
		{"author uploads new asset", author, Change{Path: "assets/uploads/a.png"}, true},
		{"author overwrites asset", author, Change{Path: "assets/uploads/a.png", Exists: true}, false},
		{"author edits page", author, Change{Path: "pages/about.md"}, false},
		{"author edits own profile", author, Change{Path: "authors/alice.md"}, true},
		{"author edits other profile", author, Change{Path: "authors/bob.md"}, false},
		{"author edits settings", author, Change{Path: "site.yaml"}, false},
		{"editor edits any post", editor, Change{Path: "posts/x.md", Exists: true, OldOwner: "bob", NewOwner: "alice"}, true},
		{"editor edits page", editor, Change{Path: "pages/about.md", Exists: true}, true},
		{"editor overwrites asset", editor, Change{Path: "assets/a.png", Exists: true}, true},
		{"editor edits settings", editor, Change{Path: "site.yaml"}, false},
		{"editor manages users", editor, Change{Path: content.UsersPath}, false},
		{"admin edits settings", admin, Change{Path: "site.yaml"}, true},
		{"admin manages users", admin, Change{Path: content.UsersPath}, true},
		{"admin edits theme (not via UI)", admin, Change{Path: "theme/templates/base.html"}, false},
		{"path traversal", admin, Change{Path: "posts/../site.yaml"}, false},
		{"absolute path", admin, Change{Path: "/etc/passwd"}, false},
		{"nested profile path", admin, Change{Path: "authors/x/y.md"}, false},
		{"nil user", nil, Change{Path: "posts/x.md"}, false},
	}
	for _, tt := range tests {
		err := CheckChange(tt.u, tt.c)
		if (err == nil) != tt.ok {
			t.Errorf("%s: err = %v, want ok=%v", tt.name, err, tt.ok)
		}
	}
}

func TestResolveRole(t *testing.T) {
	users := content.UsersFile{Users: map[string]content.UserEntry{
		"alice": {ID: 42, Role: "author"},
		"bob":   {Role: "editor"},
	}}
	cases := []struct {
		login string
		id    int64
		perm  string
		want  Role
	}{
		{"owner", 1, "admin", RoleAdmin},
		{"pusher", 2, "write", RoleAdmin},
		{"alice", 42, "none", RoleAuthor},
		{"Alice", 42, "read", RoleAuthor},
		{"alice", 99, "none", RoleNone}, // renamed-and-reclaimed username
		{"bob", 7, "none", RoleEditor},  // no pinned ID
		{"stranger", 3, "read", RoleNone},
	}
	for _, c := range cases {
		if got := ResolveRole(c.login, c.id, c.perm, users); got != c.want {
			t.Errorf("ResolveRole(%s, %d, %s) = %q, want %q", c.login, c.id, c.perm, got, c.want)
		}
	}
}
