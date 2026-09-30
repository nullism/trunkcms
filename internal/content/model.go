package content

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Date accepts the handful of date formats people actually type in front matter.
type Date struct{ time.Time }

var dateLayouts = []string{
	time.RFC3339,
	"2006-01-02T15:04:05",
	"2006-01-02 15:04:05",
	"2006-01-02 15:04",
	"2006-01-02T15:04", // <input type="datetime-local">
	"2006-01-02",
}

func ParseDate(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	for _, l := range dateLayouts {
		if t, err := time.Parse(l, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("unrecognized date %q", s)
}

func (d *Date) UnmarshalYAML(n *yaml.Node) error {
	t, err := ParseDate(n.Value)
	if err != nil {
		return err
	}
	d.Time = t
	return nil
}

func (d Date) MarshalYAML() (any, error) { return d.UTC().Format(time.RFC3339), nil }

type PostMeta struct {
	Title   string         `yaml:"title"`
	Date    Date           `yaml:"date,omitempty"`
	Slug    string         `yaml:"slug,omitempty"`
	Tags    []string       `yaml:"tags,omitempty,flow"`
	Author  string         `yaml:"author,omitempty"`
	Summary string         `yaml:"summary,omitempty"`
	Draft   bool           `yaml:"draft,omitempty"`
	Extra   map[string]any `yaml:",inline"`
}

type Post struct {
	Path      string // repo path of the Markdown file
	BundleDir string // non-empty for posts/<name>/index.md bundles
	Meta      PostMeta
	Body      []byte
	Slug      string
	URL       string
	Scheduled bool // date in the future
}

// Hidden reports whether the post is excluded from public output.
func (p *Post) Hidden() bool    { return p.Meta.Draft || p.Scheduled }
func (p *Post) Date() time.Time { return p.Meta.Date.Time }

type PageMeta struct {
	Title string         `yaml:"title"`
	Extra map[string]any `yaml:",inline"`
}

type Page struct {
	Path string
	Meta PageMeta
	Body []byte
	URL  string
}

type AuthorMeta struct {
	Name   string `yaml:"name"`
	Avatar string `yaml:"avatar,omitempty"`
	Links  []Link `yaml:"links,omitempty"`
}

type Author struct {
	Login string
	Path  string
	Meta  AuthorMeta
	Body  []byte
	URL   string
}

type Problem struct {
	Path string
	Err  error
}

func (p Problem) Error() string { return p.Path + ": " + p.Err.Error() }

var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

// Slugify turns arbitrary text into a URL-safe slug.
func Slugify(s string) string {
	return strings.Trim(nonSlug.ReplaceAllString(strings.ToLower(s), "-"), "-")
}

var nonFileName = regexp.MustCompile(`[^a-z0-9._-]+`)

// SafeFileName normalizes an uploaded file name. admin.js mirrors this rule.
func SafeFileName(s string) string {
	s = strings.Trim(nonFileName.ReplaceAllString(strings.ToLower(s), "-"), "-.")
	if s == "" {
		s = "file"
	}
	return s
}

var datePrefix = regexp.MustCompile(`^(\d{4}-\d{2}-\d{2})-(.+)$`)

// Permalink expands a permalink pattern for a post.
func Permalink(pattern, slug string, date time.Time) string {
	r := strings.NewReplacer(
		":year", date.Format("2006"),
		":month", date.Format("01"),
		":day", date.Format("02"),
		":slug", slug,
	)
	u := r.Replace(pattern)
	if !strings.HasPrefix(u, "/") {
		u = "/" + u
	}
	if !strings.HasSuffix(u, "/") {
		u += "/"
	}
	return u
}
