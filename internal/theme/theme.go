// Package theme provides the embedded default theme, resolves the theme a
// content repo selects from its themes/ directory, and layers the repo's
// theme/ overrides on top.
package theme

import (
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"sort"

	"gopkg.in/yaml.v3"
)

//go:embed all:default
var embedded embed.FS

// Default returns the embedded default theme rooted at templates/ and static/.
func Default() fs.FS {
	sub, err := fs.Sub(embedded, "default")
	if err != nil {
		panic(err)
	}
	return sub
}

// Directories in a content repo.
const (
	LocalDir  = "theme"  // per-site overrides, applied on top of whichever theme is selected
	ThemesDir = "themes" // selectable themes, one per subdirectory
)

// InfoPath is the theme's metadata file, relative to the theme root.
const InfoPath = "theme.yaml"

// Info describes a theme. It is informational only and doesn't change rendering.
type Info struct {
	Name        string `yaml:"name"`
	Version     string `yaml:"version,omitempty"`
	Description string `yaml:"description,omitempty"`
	Author      string `yaml:"author,omitempty"`
	Homepage    string `yaml:"homepage,omitempty"`
	License     string `yaml:"license,omitempty"`
}

// Theme is the resolved theme for one snapshot.
type Theme struct {
	FS         fs.FS
	Dir        string // the selected directory under themes/, or "" for the built-in default
	Info       Info
	Customized bool // the snapshot has theme/ overrides on top of the selected theme
}

// Choice is a theme a site can select.
type Choice struct {
	Dir        string // "" for the built-in default
	Info       Info
	Screenshot string // file name at the theme root, or "" if it has none
	Err        error  // a broken theme.yaml; selecting the theme fails until it's fixed
}

var nameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// Screenshot file names, in order of preference. SVG is left out on purpose:
// the admin serves screenshots from its own origin, and SVG can run scripts.
var screenshotNames = []string{"screenshot.png", "screenshot.jpg", "screenshot.jpeg", "screenshot.webp"}

// ForSnapshot resolves the theme selected by name (site.yaml's theme.name).
// Files are looked up in theme/, then themes/<name>/, then the built-in
// default, so a theme only needs the files it changes. theme.yaml follows the
// same order, except that a selected theme without one is named after its
// directory rather than borrowing the default's.
func ForSnapshot(snapshot fs.FS, name string) (Theme, error) {
	t := Theme{FS: Default(), Dir: name}
	info, err := readInfo(t.FS, InfoPath, Info{})
	if err != nil {
		return t, err
	}
	if name != "" {
		sub, err := themeFS(snapshot, name)
		if err != nil {
			return t, err
		}
		if info, err = readInfo(sub, path.Join(ThemesDir, name, InfoPath), Info{Name: name}); err != nil {
			return t, err
		}
		t.FS = Layer(sub, t.FS)
	}
	if sub, ok := subDir(snapshot, LocalDir); ok {
		if info, err = readInfo(sub, path.Join(LocalDir, InfoPath), info); err != nil {
			return t, err
		}
		t.FS = Layer(sub, t.FS)
		t.Customized = true
	}
	t.Info = info
	return t, nil
}

// List returns the built-in default followed by each themes/<dir>/ in the snapshot.
func List(snapshot fs.FS) []Choice {
	def, _ := readInfo(Default(), InfoPath, Info{})
	out := []Choice{{Info: def, Screenshot: findScreenshot(Default())}}
	entries, _ := fs.ReadDir(snapshot, ThemesDir)
	for _, e := range entries {
		if !e.IsDir() || !nameRe.MatchString(e.Name()) {
			continue
		}
		dir := path.Join(ThemesDir, e.Name())
		sub, _ := fs.Sub(snapshot, dir)
		info, err := readInfo(sub, path.Join(dir, InfoPath), Info{Name: e.Name()})
		out = append(out, Choice{Dir: e.Name(), Info: info, Screenshot: findScreenshot(sub), Err: err})
	}
	return out
}

// Screenshot returns the screenshot of the theme in themes/<name>/, or of the
// built-in default when name is "". It returns fs.ErrNotExist if there is none.
func Screenshot(snapshot fs.FS, name string) (file string, b []byte, err error) {
	fsys := Default()
	if name != "" {
		if fsys, err = themeFS(snapshot, name); err != nil {
			return "", nil, fs.ErrNotExist
		}
	}
	if file = findScreenshot(fsys); file == "" {
		return "", nil, fs.ErrNotExist
	}
	b, err = fs.ReadFile(fsys, file)
	return file, b, err
}

func findScreenshot(fsys fs.FS) string {
	for _, name := range screenshotNames {
		if st, err := fs.Stat(fsys, name); err == nil && st.Mode().IsRegular() {
			return name
		}
	}
	return ""
}

// themeFS returns themes/<name>/ from the snapshot.
func themeFS(snapshot fs.FS, name string) (fs.FS, error) {
	if !nameRe.MatchString(name) {
		return nil, fmt.Errorf("theme: invalid theme name %q", name)
	}
	dir := path.Join(ThemesDir, name)
	sub, ok := subDir(snapshot, dir)
	if !ok {
		return nil, fmt.Errorf("theme: %q not found (no %s/ directory)", name, dir)
	}
	return sub, nil
}

// readInfo reads theme.yaml from themeFS, returning fallback if there is none.
// A theme.yaml without a name keeps fallback's name. label is used in errors.
func readInfo(themeFS fs.FS, label string, fallback Info) (Info, error) {
	b, err := fs.ReadFile(themeFS, InfoPath)
	if errors.Is(err, fs.ErrNotExist) {
		return fallback, nil
	}
	if err != nil {
		return fallback, fmt.Errorf("theme: %w", err)
	}
	var info Info
	if err := yaml.Unmarshal(b, &info); err != nil {
		return fallback, fmt.Errorf("theme: %s: %w", label, err)
	}
	if info.Name == "" {
		info.Name = fallback.Name
	}
	return info, nil
}

func subDir(fsys fs.FS, dir string) (fs.FS, bool) {
	if st, err := fs.Stat(fsys, dir); err != nil || !st.IsDir() {
		return nil, false
	}
	sub, err := fs.Sub(fsys, dir)
	return sub, err == nil
}

// Layer returns an FS where files in upper replace files in lower by name.
// upper may be nil.
func Layer(upper, lower fs.FS) fs.FS {
	if upper == nil {
		return lower
	}
	return layered{upper, lower}
}

type layered struct{ upper, lower fs.FS }

func (l layered) Open(name string) (fs.File, error) {
	if f, err := l.upper.Open(name); err == nil {
		if st, err := f.Stat(); err == nil && !st.IsDir() {
			return f, nil
		}
		f.Close()
	}
	return l.lower.Open(name)
}

// ReadDir merges both layers so overrides can also add new files.
func (l layered) ReadDir(name string) ([]fs.DirEntry, error) {
	seen := map[string]fs.DirEntry{}
	var errs []error
	for _, layer := range []fs.FS{l.lower, l.upper} {
		entries, err := fs.ReadDir(layer, name)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, e := range entries {
			seen[e.Name()] = e
		}
	}
	if len(errs) == 2 {
		return nil, errs[0]
	}
	out := make([]fs.DirEntry, 0, len(seen))
	for _, e := range seen {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out, nil
}

var _ fs.ReadDirFS = layered{}
