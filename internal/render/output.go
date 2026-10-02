package render

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"mime"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// Entry maps a URL to a file in the object store. Entries are all that a
// build keeps in memory; the bytes live on disk.
type Entry struct {
	Object      string // content hash; the file name in the object store
	Gzip        bool   // a precompressed Object+".gz" exists
	ContentType string
	ETag        string
	Immutable   bool // content-hashed URL; cache forever
}

// DraftEntry is a hidden page, served only to users allowed to see its owner's drafts.
type DraftEntry struct {
	*Entry
	Owner string
}

// Output is a rendered site: an index from URL path ("/posts/hello/") to objects on disk.
type Output struct {
	Objects  *Objects
	Files    map[string]*Entry
	Drafts   map[string]*DraftEntry
	NotFound *Entry
	Search   *SearchStats // nil when search is off
}

// Refs returns every object this output uses, for garbage collection.
func (o *Output) Refs() map[string]bool {
	refs := map[string]bool{}
	add := func(e *Entry) {
		if e != nil {
			refs[e.Object] = true
		}
	}
	for _, e := range o.Files {
		add(e)
	}
	for _, d := range o.Drafts {
		add(d.Entry)
	}
	add(o.NotFound)
	return refs
}

// Path returns the file to serve for e, preferring the gzip variant if allowed.
func (o *Output) Path(e *Entry, gz bool) string {
	name := e.Object
	if gz && e.Gzip {
		name += ".gz"
	}
	return filepath.Join(o.Objects.dir, name)
}

// Objects is a content-addressed file store. Identical content is stored
// once, so unchanged pages cost nothing on rebuild. A nil *Objects discards
// everything, which is how validation builds avoid touching disk.
type Objects struct {
	dir string
}

func NewObjects(dir string) (*Objects, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &Objects{dir: dir}, nil
}

func (o *Objects) Dir() string { return o.dir }

func hashName(sum []byte) string { return hex.EncodeToString(sum)[:40] }

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// writeAtomic writes data to name unless it already exists.
func (o *Objects) writeAtomic(name string, write func(io.Writer) error) error {
	final := filepath.Join(o.dir, name)
	if exists(final) {
		return nil
	}
	tmp, err := os.CreateTemp(o.dir, ".tmp-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := write(tmp); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), final)
}

// PutBytes stores body (and a gzip variant when worthwhile).
func (o *Objects) PutBytes(body []byte, contentType string) (*Entry, error) {
	sum := sha256.Sum256(body)
	h := hashName(sum[:])
	e := &Entry{Object: h, ContentType: contentType, ETag: `"` + h[:20] + `"`}
	if o == nil {
		return e, nil
	}
	if err := o.writeAtomic(h, func(w io.Writer) error { _, err := w.Write(body); return err }); err != nil {
		return nil, err
	}
	if compressible(contentType) && len(body) > 512 {
		gzPath := filepath.Join(o.dir, h+".gz")
		if exists(gzPath) {
			e.Gzip = true
		} else {
			var buf bytes.Buffer
			zw, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
			zw.Write(body)
			zw.Close()
			if buf.Len() < len(body)*9/10 {
				if err := o.writeAtomic(h+".gz", func(w io.Writer) error { _, err := buf.WriteTo(w); return err }); err != nil {
					return nil, err
				}
				e.Gzip = true
			}
		}
	}
	return e, nil
}

// maxCompressSize bounds how large a compressible file is read into memory.
const maxCompressSize = 4 << 20

// PutFile streams a file from fsys into the store without loading it into
// memory (small compressible files excepted, so they get a gzip variant).
func (o *Objects) PutFile(fsys fs.FS, name string) (*Entry, error) {
	ct := contentTypeFor(name)
	if o == nil {
		return &Entry{ContentType: ct}, nil
	}
	st, err := fs.Stat(fsys, name)
	if err != nil {
		return nil, err
	}
	if compressible(ct) && st.Size() <= maxCompressSize {
		b, err := fs.ReadFile(fsys, name)
		if err != nil {
			return nil, err
		}
		return o.PutBytes(b, ct)
	}
	src, err := fsys.Open(name)
	if err != nil {
		return nil, err
	}
	defer src.Close()
	tmp, err := os.CreateTemp(o.dir, ".tmp-")
	if err != nil {
		return nil, err
	}
	defer os.Remove(tmp.Name())
	hasher := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, hasher), src); err != nil {
		tmp.Close()
		return nil, err
	}
	if err := tmp.Close(); err != nil {
		return nil, err
	}
	h := hashName(hasher.Sum(nil))
	if final := filepath.Join(o.dir, h); !exists(final) {
		if err := os.Rename(tmp.Name(), final); err != nil {
			return nil, err
		}
	}
	return &Entry{Object: h, ContentType: ct, ETag: `"` + h[:20] + `"`}, nil
}

// GC deletes objects not in keep, plus leftover temp files.
func (o *Objects) GC(keep map[string]bool) (removed int, err error) {
	entries, err := os.ReadDir(o.dir)
	if err != nil {
		return 0, err
	}
	var errs []error
	for _, e := range entries {
		name := e.Name()
		if keep[strings.TrimSuffix(name, ".gz")] {
			continue
		}
		if err := os.Remove(filepath.Join(o.dir, name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
			continue
		}
		removed++
	}
	return removed, errors.Join(errs...)
}

func compressible(ct string) bool {
	return strings.HasPrefix(ct, "text/") ||
		strings.Contains(ct, "xml") || strings.Contains(ct, "json") ||
		strings.Contains(ct, "javascript") || strings.Contains(ct, "svg")
}

func contentTypeFor(name string) string {
	if ct := mime.TypeByExtension(path.Ext(name)); ct != "" {
		return ct
	}
	return "application/octet-stream"
}

const htmlType = "text/html; charset=utf-8"
