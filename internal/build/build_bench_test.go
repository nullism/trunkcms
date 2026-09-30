package build

import (
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/nullism/trunkcms/internal/render"
)

// synthSite generates a blog with n posts of roughly `words` words each,
// spread over 50 tags and 5 authors, plus `images` assets of imageKB each.
func synthSite(n, words, images, imageKB int) fstest.MapFS {
	r := rand.New(rand.NewPCG(1, 2))
	vocab := strings.Fields("the quick brown fox jumps over lazy dog go server memory build render cache " +
		"kubernetes lambda commit branch deploy markdown template theme editor draft publish syntax")
	m := fstest.MapFS{"site.yaml": {Data: []byte("title: Bench\nbase_url: https://example.com\n")}}
	start := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := range n {
		var b strings.Builder
		fmt.Fprintf(&b, "---\ntitle: Post number %d\ntags: [t%d, t%d]\nauthor: a%d\nsummary: Summary of post %d.\n---\n",
			i, r.IntN(50), r.IntN(50), r.IntN(5), i)
		for w := 0; w < words; w++ {
			b.WriteString(vocab[r.IntN(len(vocab))])
			switch {
			case w%120 == 119:
				b.WriteString("\n\n## A heading\n\n")
			case w%400 == 399:
				b.WriteString("\n\n```go\nfunc main() {\n\tfmt.Println(\"hello\")\n}\n```\n\n")
			default:
				b.WriteByte(' ')
			}
		}
		date := start.Add(time.Duration(i) * time.Hour).Format("2006-01-02")
		m[fmt.Sprintf("posts/%s-post-%d.md", date, i)] = &fstest.MapFile{Data: []byte(b.String())}
	}
	for i := range images {
		img := make([]byte, imageKB<<10)
		for j := range img {
			img[j] = byte(r.Uint32()) // random, so identical-content dedup doesn't flatter the numbers
		}
		m[fmt.Sprintf("assets/img-%d.jpg", i)] = &fstest.MapFile{Data: img}
	}
	return m
}

func heapMB() float64 {
	runtime.GC()
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return float64(ms.HeapAlloc) / (1 << 20)
}

func dirMB(dir string) float64 {
	var n int64
	filepath.WalkDir(dir, func(_ string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if info, err := d.Info(); err == nil {
				n += info.Size()
			}
		}
		return nil
	})
	return float64(n) / (1 << 20)
}

// writeSite materializes a synthetic site on disk, like a snapshot checkout.
func writeSite(t *testing.T, m fstest.MapFS) string {
	dir := t.TempDir()
	for p, f := range m {
		dst := filepath.Join(dir, filepath.FromSlash(p))
		os.MkdirAll(filepath.Dir(dst), 0o755)
		if err := os.WriteFile(dst, f.Data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// TestMemoryFootprint reports memory and disk used by synthetic sites.
// Run with: TRUNKCMS_MEMTEST=1 go test -run MemoryFootprint -v ./internal/build/
func TestMemoryFootprint(t *testing.T) {
	if os.Getenv("TRUNKCMS_MEMTEST") == "" {
		t.Skip("set TRUNKCMS_MEMTEST=1 to run")
	}
	for _, tc := range []struct {
		posts, words, images, imageKB int
	}{
		{100, 1000, 0, 0},
		{1000, 1000, 0, 0},
		{5000, 1000, 0, 0},
		{1000, 1000, 1000, 200},
	} {
		src := writeSite(t, synthSite(tc.posts, tc.words, tc.images, tc.imageKB))
		objDir := t.TempDir()
		objs, err := render.NewObjects(objDir)
		if err != nil {
			t.Fatal(err)
		}
		base := heapMB()
		start := time.Now()
		res, err := Run("x", os.DirFS(src), objs, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		took := time.Since(start)
		retained := heapMB() - base
		t.Logf("%5d posts × %d words, %4d images × %dKB | disk: source %6.1f MB, rendered %6.1f MB | memory retained %5.1f MB | %d URLs | build %v",
			tc.posts, tc.words, tc.images, tc.imageKB, dirMB(src), dirMB(objDir), retained, len(res.Output.Files), took.Round(time.Millisecond))
		runtime.KeepAlive(res)
	}
}
