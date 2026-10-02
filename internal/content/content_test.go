package content

import (
	"os"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

func TestFrontMatterRoundTrip(t *testing.T) {
	src := []byte("---\ntitle: Hi\ndate: 2026-01-02\ncustom: kept\n---\nBody text\n")
	var m PostMeta
	body, err := ParseDocument(src, &m)
	if err != nil {
		t.Fatal(err)
	}
	if m.Title != "Hi" || m.Date.Format("2006-01-02") != "2026-01-02" || string(body) != "Body text\n" {
		t.Fatalf("unexpected parse: %+v %q", m, body)
	}
	if m.Extra["custom"] != "kept" {
		t.Fatalf("unknown key dropped: %v", m.Extra)
	}
	out, err := FormatDocument(m, body)
	if err != nil {
		t.Fatal(err)
	}
	want := "---\ntitle: Hi\ndate: \"2026-01-02T00:00:00Z\"\ncustom: kept\n---\nBody text\n"
	if string(out) != want {
		t.Fatalf("format:\n%s\nwant:\n%s", out, want)
	}
}

func TestSplitFrontMatterEdgeCases(t *testing.T) {
	if _, body, _ := SplitFrontMatter([]byte("no front matter")); string(body) != "no front matter" {
		t.Fatal("plain document should pass through")
	}
	if _, _, err := SplitFrontMatter([]byte("---\ntitle: x\n")); err == nil {
		t.Fatal("unterminated front matter should error")
	}
	meta, body, err := SplitFrontMatter([]byte("\r\n---\r\n"))
	if err != nil || meta != nil || len(body) == 0 {
		t.Fatal("leading blank line means no front matter")
	}
	meta, body, err = SplitFrontMatter([]byte("---\na: 1\n---"))
	if err != nil || string(meta) != "a: 1\n" || len(body) != 0 {
		t.Fatalf("closing delimiter at EOF: %q %q %v", meta, body, err)
	}
}

func TestLoadFixture(t *testing.T) {
	now := time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)
	s, err := Load(os.DirFS("../../testdata/site"), now)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Problems) != 0 {
		t.Fatalf("problems: %v", s.Problems)
	}
	if got := len(s.Posts); got != 5 {
		t.Fatalf("posts = %d, want 5", got)
	}
	if got := len(s.Published()); got != 3 {
		t.Fatalf("published = %d, want 3", got)
	}
	if s.Posts[0].Slug != "future-post" || !s.Posts[0].Scheduled {
		t.Fatalf("newest post should be the scheduled one: %+v", s.Posts[0])
	}
	if s.NextPublish.Year() != 2099 {
		t.Fatalf("NextPublish = %v", s.NextPublish)
	}
	if p := s.PostByPath("posts/2026-09-20-bundled-post/index.md"); p == nil || p.BundleDir == "" || p.URL != "/posts/bundled-post/" {
		t.Fatalf("bundle post: %+v", p)
	}
	if s.AuthorName("dev-author") != "Dev Author" || s.AuthorName("nobody") != "nobody" {
		t.Fatal("author name resolution")
	}
	if s.Users.Users["dev-author"].Role != "author" {
		t.Fatalf("users: %+v", s.Users)
	}
}

func TestLoadProblemsAndConflicts(t *testing.T) {
	fsys := fstest.MapFS{
		"posts/2026-01-01-a.md":  {Data: []byte("---\ntitle: A\n---\n")},
		"posts/2026-01-02-a.md":  {Data: []byte("---\ntitle: A again\n---\n")},
		"posts/no-date.md":       {Data: []byte("---\ntitle: Undated\n---\n")},
		"posts/2026-01-03-b.md":  {Data: []byte("---\ntitle: [unclosed\n---\n")},
		"pages/tags/x.md":        {Data: []byte("---\ntitle: Clash\n---\n")},
		"pages/index.md":         {Data: []byte("home?")},
		"posts/2026-01-04-nt.md": {Data: []byte("no title here")},
	}
	s, err := Load(fsys, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Posts) != 1 || s.Posts[0].Meta.Title != "A again" {
		t.Fatalf("expected only the newest duplicate to survive, got %d posts", len(s.Posts))
	}
	var paths []string
	for _, p := range s.Problems {
		paths = append(paths, p.Path)
	}
	joined := strings.Join(paths, " ")
	for _, want := range []string{"posts/2026-01-01-a.md", "posts/no-date.md", "posts/2026-01-03-b.md", "pages/tags/x.md", "pages/index.md", "posts/2026-01-04-nt.md"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing problem for %s (got %s)", want, joined)
		}
	}
}

func TestBadConfigFailsLoad(t *testing.T) {
	_, err := Load(fstest.MapFS{"site.yaml": {Data: []byte("permalink: /no-slug/")}}, time.Now())
	if err == nil {
		t.Fatal("permalink without :slug should fail")
	}
	_, err = Load(fstest.MapFS{UsersPath: {Data: []byte("users:\n  x: { role: god }")}}, time.Now())
	if err == nil {
		t.Fatal("unknown role should fail")
	}
}

func TestSafeFileName(t *testing.T) {
	for in, want := range map[string]string{
		"My Photo.PNG":   "my-photo.png",
		"../../etc/pass": "etc-pass",
		"...":            "file",
		"a b__c.jpg":     "a-b__c.jpg",
	} {
		if got := SafeFileName(in); got != want {
			t.Errorf("SafeFileName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSearchConfigDefaults(t *testing.T) {
	c, err := ParseConfig([]byte("title: x\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Search.Enabled || !c.Search.Pages || c.Search.TitleBoost != DefaultTitleBoost {
		t.Fatalf("defaults: %+v", c.Search)
	}
	// A partial search block keeps the other defaults.
	if c, _ = ParseConfig([]byte("title: x\nsearch: {enabled: true, title_boost: 0}\n")); !c.Search.Enabled || !c.Search.Pages || c.Search.TitleBoost != 0 {
		t.Fatalf("partial block: %+v", c.Search)
	}
	if _, err := ParseConfig([]byte("title: x\nsearch: {title_boost: -1}\n")); err == nil {
		t.Fatal("negative title_boost should be rejected")
	}
}
