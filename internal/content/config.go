package content

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	ConfigPath = "site.yaml"
	UsersPath  = ".trunkcms/users.yaml"
)

type Link struct {
	Title string `yaml:"title"`
	URL   string `yaml:"url"`
}

type Person struct {
	Name  string `yaml:"name,omitempty"`
	Email string `yaml:"email,omitempty"`
}

// Config is site.yaml.
type Config struct {
	Title        string `yaml:"title"`
	Description  string `yaml:"description,omitempty"`
	BaseURL      string `yaml:"base_url,omitempty"`
	Language     string `yaml:"language,omitempty"`
	Author       Person `yaml:"author,omitempty"`
	PostsPerPage int    `yaml:"posts_per_page,omitempty"`
	Permalink    string `yaml:"permalink,omitempty"`
	Nav          []Link `yaml:"nav,omitempty"`
	Feeds        struct {
		RSS  bool `yaml:"rss"`
		Atom bool `yaml:"atom"`
	} `yaml:"feeds"`
	Theme struct {
		Name string `yaml:"name,omitempty"` // a directory under themes/; empty for the built-in default
	} `yaml:"theme,omitempty"`
	Markdown struct {
		UnsafeHTML bool `yaml:"unsafe_html,omitempty"` // raw HTML in posts and author profiles; pages always allow it
	} `yaml:"markdown,omitempty"`
	// Extra keeps keys this version doesn't know about (e.g. for custom themes),
	// so saving settings from the UI never drops them.
	Extra map[string]any `yaml:",inline"`
}

func DefaultConfig() Config {
	var c Config
	c.Title = "Untitled site"
	c.Language = "en"
	c.PostsPerPage = 10
	c.Permalink = "/posts/:slug/"
	c.Feeds.RSS = true
	c.Feeds.Atom = true
	return c
}

func ParseConfig(b []byte) (Config, error) {
	c := DefaultConfig()
	if err := yaml.Unmarshal(b, &c); err != nil {
		return c, fmt.Errorf("%s: %w", ConfigPath, err)
	}
	if c.PostsPerPage <= 0 {
		c.PostsPerPage = 10
	}
	if c.Permalink == "" {
		c.Permalink = "/posts/:slug/"
	}
	if !strings.Contains(c.Permalink, ":slug") {
		return c, fmt.Errorf("%s: permalink must contain :slug", ConfigPath)
	}
	c.BaseURL = strings.TrimRight(c.BaseURL, "/")
	return c, nil
}

func (c Config) Marshal() ([]byte, error) { return yaml.Marshal(c) }

// UserEntry is one row of .trunkcms/users.yaml.
type UserEntry struct {
	ID   int64  `yaml:"id,omitempty"`
	Role string `yaml:"role"`
}

type UsersFile struct {
	Users map[string]UserEntry `yaml:"users"`
}

func ParseUsers(b []byte) (UsersFile, error) {
	var u UsersFile
	if err := yaml.Unmarshal(b, &u); err != nil {
		return u, fmt.Errorf("%s: %w", UsersPath, err)
	}
	if u.Users == nil {
		u.Users = map[string]UserEntry{}
	}
	lower := make(map[string]UserEntry, len(u.Users))
	for login, e := range u.Users {
		switch e.Role {
		case "author", "editor", "admin":
		default:
			return u, fmt.Errorf("%s: user %q has unknown role %q", UsersPath, login, e.Role)
		}
		lower[strings.ToLower(login)] = e
	}
	u.Users = lower
	return u, nil
}

func (u UsersFile) Marshal() ([]byte, error) { return yaml.Marshal(u) }
