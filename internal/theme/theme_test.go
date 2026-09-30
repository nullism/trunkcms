package theme

import (
	"errors"
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"
)

func file(s string) *fstest.MapFile { return &fstest.MapFile{Data: []byte(s)} }

func read(t *testing.T, th Theme, name string) string {
	t.Helper()
	b, err := fs.ReadFile(th.FS, name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestDefault(t *testing.T) {
	th, err := ForSnapshot(fstest.MapFS{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if th.Info.Name != "Default" || th.Info.Homepage == "" || th.Dir != "" || th.Customized {
		t.Fatalf("default theme: %+v", th)
	}
}

func TestLayering(t *testing.T) {
	snap := fstest.MapFS{
		"themes/paper/theme.yaml":          file("name: Paper\nversion: 1.2.0\nauthor: Ada\nlicense: MIT\nunknown: ignored\n"),
		"themes/paper/static/style.css":    file("paper"),
		"themes/paper/templates/post.html": file("paper post"),
		"themes/midnight/static/style.css": file("midnight"),
		"theme/templates/post.html":        file("local post"),
	}
	th, err := ForSnapshot(snap, "paper")
	if err != nil {
		t.Fatal(err)
	}
	if got := read(t, th, "static/style.css"); got != "paper" {
		t.Errorf("style.css from selected theme: got %q", got)
	}
	if got := read(t, th, "templates/post.html"); got != "local post" {
		t.Errorf("theme/ should win over the selected theme: got %q", got)
	}
	if _, err := fs.Stat(th.FS, "templates/base.html"); err != nil {
		t.Errorf("missing files should come from the default: %v", err)
	}
	want := Info{Name: "Paper", Version: "1.2.0", Author: "Ada", License: "MIT"}
	if th.Info != want || !th.Customized || th.Dir != "paper" {
		t.Errorf("got %+v, want info %+v, customized", th, want)
	}

	// A theme without theme.yaml is named after its directory, not "Default".
	if th, err := ForSnapshot(snap, "midnight"); err != nil || th.Info.Name != "midnight" {
		t.Errorf("midnight: %+v, %v", th.Info, err)
	}

	// theme/theme.yaml describes the site's own variant; a missing name is inherited.
	snap["theme/theme.yaml"] = file("version: 1.2.0-local\n")
	if th, err := ForSnapshot(snap, "paper"); err != nil || th.Info.Name != "Paper" || th.Info.Version != "1.2.0-local" {
		t.Errorf("local theme.yaml: %+v, %v", th.Info, err)
	}
}

func TestSelectionErrors(t *testing.T) {
	snap := fstest.MapFS{
		"themes/broken/theme.yaml": file("name: [broken"),
		"themes/notadir":           file("x"),
		"secret/templates/x.html":  file("x"),
	}
	for name, want := range map[string]string{
		"missing":   "not found",
		"notadir":   "not found",
		"broken":    "themes/broken/theme.yaml",
		"../secret": "invalid theme name",
		"a/b":       "invalid theme name",
		".hidden":   "invalid theme name",
	} {
		if _, err := ForSnapshot(snap, name); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: got %v, want error containing %q", name, err, want)
		}
	}
}

func TestList(t *testing.T) {
	got := List(fstest.MapFS{
		"themes/paper/theme.yaml":      file("name: Paper\n"),
		"themes/broken/theme.yaml":     file("name: [broken"),
		"themes/bare/static/style.css": file(""),
		"themes/README.md":             file("not a theme"),
		"themes/.git/config":           file(""),
	})
	var dirs []string
	for _, c := range got {
		dirs = append(dirs, c.Dir+"="+c.Info.Name)
	}
	if s := strings.Join(dirs, ","); s != "=Default,bare=bare,broken=broken,paper=Paper" {
		t.Fatalf("got %s", s)
	}
	if got[2].Err == nil || got[3].Err != nil {
		t.Fatalf("only broken should have an error: %+v", got)
	}
}

func TestScreenshot(t *testing.T) {
	snap := fstest.MapFS{
		"themes/paper/screenshot.jpg":  file("jpg"),
		"themes/paper/screenshot.png":  file("png"),
		"themes/svg/screenshot.svg":    file("<svg/>"),
		"themes/bare/static/style.css": file(""),
		"secret/screenshot.png":        file("secret"),
	}
	if file, b, err := Screenshot(snap, "paper"); err != nil || file != "screenshot.png" || string(b) != "png" {
		t.Fatalf("paper: %q %q %v", file, b, err)
	}
	for _, name := range []string{"svg", "bare", "missing", "../secret"} {
		if _, _, err := Screenshot(snap, name); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%q: got %v, want fs.ErrNotExist", name, err)
		}
	}
	if file, _, err := Screenshot(snap, ""); err != nil || file == "" {
		t.Errorf("built-in theme should have a screenshot: %q %v", file, err)
	}
	for _, c := range List(snap) {
		if want := map[string]string{"": "screenshot.png", "paper": "screenshot.png"}[c.Dir]; c.Screenshot != want {
			t.Errorf("List: %q screenshot %q, want %q", c.Dir, c.Screenshot, want)
		}
	}
}
