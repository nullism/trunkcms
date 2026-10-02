// Package search builds the static JSON index that the theme's search.js
// queries in the browser. Tokenizing here must match words() in search.js,
// or queries won't find what was indexed.
package search

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// DefaultMinLength is the shortest word indexed when the config doesn't say.
const DefaultMinLength = 2

// Fold lowercases s, splits accented letters into base letter plus mark
// (NFKD), and drops the marks and apostrophes, so "Café's" becomes "cafes".
// search.js does the same with toLowerCase().normalize("NFKD").
func Fold(s string) string {
	s = norm.NFKD.String(strings.ToLower(s))
	return strings.Map(func(r rune) rune {
		if unicode.Is(unicode.Mn, r) || r == '\'' || r == '’' {
			return -1
		}
		return r
	}, s)
}

// Tokenizer splits text into indexable words.
type Tokenizer struct {
	minLength int
	stop      map[string]bool
	keep      map[string]bool
}

// NewTokenizer combines the built-in stopwords for lang with the site's own.
func NewTokenizer(lang string, minLength int, stopwords, keep []string) *Tokenizer {
	if minLength <= 0 {
		minLength = DefaultMinLength
	}
	t := &Tokenizer{minLength: minLength, stop: map[string]bool{}, keep: map[string]bool{}}
	for _, w := range append(Stopwords(lang), stopwords...) {
		t.stop[Fold(strings.TrimSpace(w))] = true
	}
	for _, w := range keep {
		t.keep[Fold(strings.TrimSpace(w))] = true
	}
	return t
}

// Words calls fn for each indexable word in s: runs of letters and digits,
// folded, minus stopwords and short words that aren't kept.
func (t *Tokenizer) Words(s string, fn func(string)) {
	for _, w := range strings.FieldsFunc(Fold(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsNumber(r)
	}) {
		if t.keep[w] || (utf8.RuneCountInString(w) >= t.minLength && !t.stop[w]) {
			fn(w)
		}
	}
}
