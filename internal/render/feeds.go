package render

import (
	"bytes"
	"encoding/xml"
	"strings"
	"time"
)

const feedSize = 20

type rssFeed struct {
	XMLName xml.Name   `xml:"rss"`
	Version string     `xml:"version,attr"`
	Channel rssChannel `xml:"channel"`
}

type rssChannel struct {
	Title       string    `xml:"title"`
	Link        string    `xml:"link"`
	Description string    `xml:"description"`
	Language    string    `xml:"language,omitempty"`
	Items       []rssItem `xml:"item"`
}

type rssItem struct {
	Title       string `xml:"title"`
	Link        string `xml:"link"`
	GUID        string `xml:"guid"`
	PubDate     string `xml:"pubDate"`
	Author      string `xml:"dc:creator,omitempty"`
	Description string `xml:"description"`
}

type atomFeed struct {
	XMLName xml.Name    `xml:"http://www.w3.org/2005/Atom feed"`
	Title   string      `xml:"title"`
	ID      string      `xml:"id"`
	Updated string      `xml:"updated"`
	Links   []atomLink  `xml:"link"`
	Entries []atomEntry `xml:"entry"`
}

type atomLink struct {
	Href string `xml:"href,attr"`
	Rel  string `xml:"rel,attr,omitempty"`
}

type atomEntry struct {
	Title   string      `xml:"title"`
	ID      string      `xml:"id"`
	Link    atomLink    `xml:"link"`
	Updated string      `xml:"updated"`
	Author  *atomAuthor `xml:"author,omitempty"`
	Content atomContent `xml:"content"`
}

type atomAuthor struct {
	Name string `xml:"name"`
}

type atomContent struct {
	Type string `xml:"type,attr"`
	Body string `xml:",chardata"`
}

func (r *Renderer) feeds(out *builder, posts []*PostView) error {
	cfg := r.site.Config
	abs := func(p string) string { return cfg.BaseURL + p }
	posts = posts[:min(len(posts), feedSize)]

	if cfg.Feeds.RSS {
		f := rssFeed{Version: "2.0", Channel: rssChannel{
			Title: cfg.Title, Link: abs("/"), Description: cfg.Description, Language: cfg.Language,
		}}
		for _, p := range posts {
			f.Channel.Items = append(f.Channel.Items, rssItem{
				Title: p.Meta.Title, Link: abs(p.URL), GUID: abs(p.URL),
				PubDate: p.Date().Format(time.RFC1123Z), Author: p.AuthorName,
				Description: string(p.Content),
			})
		}
		b, err := marshalXML(f)
		if err != nil {
			return err
		}
		// dc:creator needs its namespace declared on the root element.
		b = bytes.Replace(b, []byte(`<rss version="2.0">`), []byte(`<rss version="2.0" xmlns:dc="http://purl.org/dc/elements/1.1/">`), 1)
		if err := out.bytes("/index.xml", b, "application/rss+xml; charset=utf-8"); err != nil {
			return err
		}
	}

	if cfg.Feeds.Atom {
		updated := time.Unix(0, 0).UTC()
		if len(posts) > 0 {
			updated = posts[0].Date()
		}
		f := atomFeed{
			Title: cfg.Title, ID: abs("/"), Updated: updated.Format(time.RFC3339),
			Links: []atomLink{{Href: abs("/")}, {Href: abs("/atom.xml"), Rel: "self"}},
		}
		for _, p := range posts {
			e := atomEntry{
				Title: p.Meta.Title, ID: abs(p.URL), Link: atomLink{Href: abs(p.URL)},
				Updated: p.Date().Format(time.RFC3339),
				Content: atomContent{Type: "html", Body: string(p.Content)},
			}
			if p.AuthorName != "" {
				e.Author = &atomAuthor{Name: p.AuthorName}
			}
			f.Entries = append(f.Entries, e)
		}
		b, err := marshalXML(f)
		if err != nil {
			return err
		}
		if err := out.bytes("/atom.xml", b, "application/atom+xml; charset=utf-8"); err != nil {
			return err
		}
	}
	return nil
}

func marshalXML(v any) ([]byte, error) {
	b, err := xml.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, err
	}
	return append([]byte(xml.Header), b...), nil
}

func (r *Renderer) sitemap(out *builder, posts []*PostView) error {
	abs := func(p string) string { return r.site.Config.BaseURL + p }
	var b strings.Builder
	b.WriteString(xml.Header)
	b.WriteString(`<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">` + "\n")
	loc := func(u, mod string) {
		b.WriteString("  <url><loc>")
		xml.EscapeText(&b, []byte(abs(u)))
		b.WriteString("</loc>")
		if mod != "" {
			b.WriteString("<lastmod>" + mod + "</lastmod>")
		}
		b.WriteString("</url>\n")
	}
	loc("/", "")
	for _, p := range posts {
		loc(p.URL, p.Date().Format("2006-01-02"))
	}
	for _, p := range r.site.Pages {
		loc(p.URL, "")
	}
	b.WriteString("</urlset>\n")
	if err := out.bytes("/sitemap.xml", []byte(b.String()), "application/xml; charset=utf-8"); err != nil {
		return err
	}

	robots := "User-agent: *\nDisallow: /admin/\n"
	if r.site.Config.BaseURL != "" {
		robots += "Sitemap: " + abs("/sitemap.xml") + "\n"
	}
	return out.bytes("/robots.txt", []byte(robots), "text/plain; charset=utf-8")
}
