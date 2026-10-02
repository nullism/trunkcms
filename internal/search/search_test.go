package search

import (
	"encoding/json"
	"slices"
	"testing"
)

func words(t *Tokenizer, s string) []string {
	var out []string
	t.Words(s, func(w string) { out = append(out, w) })
	return out
}

func TestWords(t *testing.T) {
	tok := NewTokenizer("en-US", 0, []string{"Because"}, []string{"a", "IT"})
	got := words(tok, "The Café's résumé: it's GO, not Rust! Because a 2026-10-02 x IT")
	want := []string{"cafes", "resume", "go", "rust", "a", "2026", "10", "02", "it"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}

	tok = NewTokenizer("fr", 4, nil, nil) // no French list, so only the length rule applies
	if got := words(tok, "les chats and the dogs"); !slices.Equal(got, []string{"chats", "dogs"}) {
		t.Fatalf("min length: got %q", got)
	}
}

func TestStopwordsFallback(t *testing.T) {
	if len(Stopwords("en_GB")) == 0 || len(Stopwords("EN")) == 0 {
		t.Fatal("en variants should fall back to the en list")
	}
	if Stopwords("de") != nil {
		t.Fatal("unknown languages have no list")
	}
}

func TestIndexJSON(t *testing.T) {
	ix := NewIndex("en", 2, NewTokenizer("en", 0, nil, nil))
	ix.Add(Doc{URL: "/a/", Title: "CMS", Date: "2026-10-01"}, Field{"cms", WeightTitle}, Field{"cms content cms", WeightBody})
	ix.Add(Doc{URL: "/b/", Title: "Other"}, Field{"funny", WeightBody})
	ix.Add(Doc{URL: "/c/", Title: "More"}, Field{"cms funny funny", WeightBody})

	b, err := json.Marshal(ix)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		V          int
		Lang       string
		TitleBoost float64 `json:"title_boost"`
		Docs       [][]any
		Terms      map[string][]int
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got.V != Version || got.Lang != "en" || got.TitleBoost != 2 || len(got.Docs) != 3 {
		t.Fatalf("header: %s", b)
	}
	// Doc /a/: cms ×1 (title) + 2 (body), content 1 → length 4.
	if d := got.Docs[0]; d[0] != "/a/" || d[2] != "2026-10-01" || d[3] != float64(4) {
		t.Fatalf("doc 0: %v", d)
	}
	// Postings are [gap, weight, ...]: cms is in docs 0 and 2, funny in 1 and 2.
	want := map[string][]int{"cms": {0, 3, 2, 1}, "content": {0, 1}, "funny": {1, 1, 1, 2}}
	for w, p := range want {
		if !slices.Equal(got.Terms[w], p) {
			t.Errorf("%s: got %v, want %v", w, got.Terms[w], p)
		}
	}

	again, _ := json.Marshal(ix)
	if string(again) != string(b) {
		t.Fatal("output isn't deterministic")
	}
}

func TestEmptyIndex(t *testing.T) {
	b, _ := json.Marshal(NewIndex("en", 2, NewTokenizer("en", 0, nil, nil)))
	if string(b) != `{"v":1,"lang":"en","title_boost":2,"docs":[],"terms":{}}` {
		t.Fatalf("got %s", b)
	}
}

func TestTextFromHTML(t *testing.T) {
	got := TextFromHTML(`<p>Fish &amp; <em>chips</em></p><script type="x">var hidden</script><STYLE>.c{}</STYLE><p>end</p>`)
	if w := words(NewTokenizer("en", 0, nil, nil), got); !slices.Equal(w, []string{"fish", "chips", "end"}) {
		t.Fatalf("got %q from %q", w, got)
	}
}
