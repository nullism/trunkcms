package search

import (
	"encoding/json"
	"html"
	"slices"
	"strings"
)

// Version is the index format version, written as "v". Bump it when
// search.js would misread an older index.
const Version = 1

// Field weights, multiplying each word's count. Titles count like body text
// here; search.js ranks title matches with a separate bonus (title_boost),
// because BM25 saturates repeated words and a heavier weight can't guarantee
// that a title match beats a post that repeats the word often.
const (
	WeightTitle   = 1
	WeightTags    = 3
	WeightSummary = 2
	WeightBody    = 1
)

// Doc is one searchable post or page.
type Doc struct {
	URL, Title, Summary string
	Date                string // YYYY-MM-DD, or "" for pages
}

// Field is a piece of a document's text and how much each word in it counts.
type Field struct {
	Text   string
	Weight int
}

// Index accumulates documents and serializes them as:
//
//	{"v":1, "lang":"en", "title_boost": 2,
//	 "docs":  [[url, title, date, length, summary], ...],
//	 "terms": {"word": [gap, weight, gap, weight, ...], ...}}
//
// A term's postings are doc indexes in ascending order, each stored as the
// gap from the previous one (the first from 0), followed by its weight: the
// word's count in the doc, multiplied by the field weight. length is the
// doc's total weight, for BM25's length normalization.
type Index struct {
	lang       string
	titleBoost float64
	tok        *Tokenizer
	docs       [][]any
	terms      map[string][]int // doc, weight pairs, docs ascending
}

func NewIndex(lang string, titleBoost float64, tok *Tokenizer) *Index {
	return &Index{lang: lang, titleBoost: titleBoost, tok: tok, terms: map[string][]int{}}
}

// Add indexes a document. Docs added earlier win ties in search results, so
// add them in the order they should appear (newest posts first).
func (ix *Index) Add(d Doc, fields ...Field) {
	id := len(ix.docs)
	counts := map[string]int{}
	length := 0
	for _, f := range fields {
		ix.tok.Words(f.Text, func(w string) {
			counts[w] += f.Weight
			length += f.Weight
		})
	}
	for w, n := range counts {
		ix.terms[w] = append(ix.terms[w], id, n)
	}
	ix.docs = append(ix.docs, []any{d.URL, d.Title, d.Date, length, d.Summary})
}

func (ix *Index) Docs() int  { return len(ix.docs) }
func (ix *Index) Terms() int { return len(ix.terms) }

// MarshalJSON writes the index. The output is deterministic, so an unchanged
// site produces the same bytes and the same content-hashed URL.
func (ix *Index) MarshalJSON() ([]byte, error) {
	terms := make(map[string][]int, len(ix.terms))
	for w, p := range ix.terms {
		enc := slices.Clone(p)
		for i := len(enc) - 2; i > 0; i -= 2 {
			enc[i] -= enc[i-2]
		}
		terms[w] = enc
	}
	docs := ix.docs
	if docs == nil {
		docs = [][]any{}
	}
	return json.Marshal(struct {
		V          int              `json:"v"`
		Lang       string           `json:"lang"`
		TitleBoost float64          `json:"title_boost"`
		Docs       [][]any          `json:"docs"`
		Terms      map[string][]int `json:"terms"`
	}{Version, ix.lang, ix.titleBoost, docs, terms})
}

// TextFromHTML returns the visible text of rendered HTML: tags become spaces,
// entities are decoded, and <script> and <style> contents are dropped.
func TextFromHTML(s string) string {
	var b strings.Builder
	for len(s) > 0 {
		lt := strings.IndexByte(s, '<')
		if lt < 0 {
			b.WriteString(s)
			break
		}
		b.WriteString(s[:lt])
		b.WriteByte(' ')
		s = s[lt:]
		gt := strings.IndexByte(s, '>')
		if gt < 0 {
			break
		}
		tag := strings.ToLower(s[1:gt])
		s = s[gt+1:]
		for _, raw := range []string{"script", "style"} {
			if tag == raw || strings.HasPrefix(tag, raw+" ") {
				end := strings.Index(strings.ToLower(s), "</"+raw)
				if end < 0 {
					end = len(s)
				}
				s = s[end:]
			}
		}
	}
	return html.UnescapeString(b.String())
}
