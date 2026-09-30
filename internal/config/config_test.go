package config

import "testing"

func TestParseRepo(t *testing.T) {
	for _, in := range []string{
		"nullism/myblog",
		"github.com/nullism/myblog",
		"https://github.com/nullism/myblog",
		"https://github.com/nullism/myblog.git",
		"https://www.github.com/nullism/myblog/",
	} {
		o, n, err := ParseRepo(in)
		if err != nil || o != "nullism" || n != "myblog" {
			t.Errorf("ParseRepo(%q) = %q, %q, %v", in, o, n, err)
		}
	}
	for _, bad := range []string{"", "nullism", "a/b/c", "/myblog"} {
		if _, _, err := ParseRepo(bad); err == nil {
			t.Errorf("ParseRepo(%q) should fail", bad)
		}
	}
}

func TestParseRepoPath(t *testing.T) {
	for in, want := range map[string]string{
		"": "", "/": "", ".": "", "blog1": "blog1", "/blog1/": "blog1", "sites/blog1": "sites/blog1",
	} {
		if got, err := ParseRepoPath(in); err != nil || got != want {
			t.Errorf("ParseRepoPath(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"../x", "a/../b", "a//b", ".git", ".git/hooks"} {
		if _, err := ParseRepoPath(bad); err == nil {
			t.Errorf("ParseRepoPath(%q) should fail", bad)
		}
	}
}
