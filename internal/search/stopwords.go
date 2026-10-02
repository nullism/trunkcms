package search

import "strings"

// stopwords are built-in lists by language code. They hold function words
// only: BM25 already gives words found in most posts little weight, so the
// lists are about cleaner results more than index size. Short words that are
// often topics (go, ai, ui, os, js) and common page names (about) are
// deliberately absent.
var stopwords = map[string]string{
	"en": `a above after again against all am an and any are as at be because been before being below
between both but by can could did do does doing down during each few for from further had has have having he
her here hers herself him himself his how i if in into is it its itself just me more most my myself no nor not
now of off on once only or other our ours ourselves out over own same she should so some such than that the
their theirs them themselves then there these they this those through to too under until up very was we were
what when where which while who whom why will with would you your yours yourself yourselves`,
}

// Stopwords returns the built-in list for a language tag such as "en" or
// "en-US", falling back from the full tag to its primary language. Languages
// without a list get none.
func Stopwords(lang string) []string {
	lang = strings.ToLower(strings.ReplaceAll(lang, "_", "-"))
	for lang != "" {
		if s, ok := stopwords[lang]; ok {
			return strings.Fields(s)
		}
		i := strings.LastIndex(lang, "-")
		if i < 0 {
			break
		}
		lang = lang[:i]
	}
	return nil
}
