package render

import (
	"encoding/json"
	"os"
	"strings"

	"github.com/nullism/trunkcms/internal/search"
)

// SearchStats describes a build's search index, for the admin dashboard.
type SearchStats struct {
	URL      string
	Docs     int
	Terms    int
	Size     int64 // bytes as stored
	GzipSize int64 // bytes as most browsers download it; 0 if unknown
}

// searchIndexURL is stable rather than content-hashed: the index changes on
// every content save, and a page cached from two builds ago would otherwise
// point at an index that has been garbage-collected.
const searchIndexURL = "/search.json"

// searchIndex writes the search index for published posts and, if the site
// allows it, pages. Drafts and scheduled posts are never included, because
// the index is public. It runs before any HTML is rendered, so templates see
// its URL.
func (r *Renderer) searchIndex(b *builder, posts []*PostView, pages []*PageView) error {
	cfg := r.site.Config.Search
	if !cfg.Enabled {
		return nil
	}
	tok := search.NewTokenizer(r.site.Config.Language, cfg.MinLength, cfg.Stopwords, cfg.Keep)
	ix := search.NewIndex(r.site.Config.Language, cfg.TitleBoost, tok)
	for _, p := range posts {
		ix.Add(search.Doc{URL: p.URL, Title: p.Meta.Title, Summary: p.Meta.Summary, Date: p.Date().Format("2006-01-02")},
			search.Field{Text: p.Meta.Title, Weight: search.WeightTitle},
			search.Field{Text: strings.Join(p.Meta.Tags, " "), Weight: search.WeightTags},
			search.Field{Text: p.Meta.Summary, Weight: search.WeightSummary},
			search.Field{Text: search.TextFromHTML(string(p.Content)), Weight: search.WeightBody})
	}
	if cfg.Pages {
		for _, p := range pages {
			ix.Add(search.Doc{URL: p.URL, Title: p.Meta.Title},
				search.Field{Text: p.Meta.Title, Weight: search.WeightTitle},
				search.Field{Text: search.TextFromHTML(string(p.Content)), Weight: search.WeightBody})
		}
	}
	body, err := json.Marshal(ix)
	if err != nil {
		return err
	}
	e, err := b.objs.PutBytes(body, "application/json")
	if err != nil {
		return err
	}
	b.add(searchIndexURL, e, false, "")

	stats := &SearchStats{URL: searchIndexURL, Docs: ix.Docs(), Terms: ix.Terms(), Size: int64(len(body))}
	if e.Gzip && b.objs != nil {
		if st, err := os.Stat(b.out.Path(e, true)); err == nil {
			stats.GzipSize = st.Size()
		}
	}
	b.out.Search = stats
	r.searchURL = searchIndexURL
	return nil
}
