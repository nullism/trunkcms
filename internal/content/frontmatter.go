package content

import (
	"bytes"
	"fmt"
	"io/fs"
	"testing/fstest"

	"gopkg.in/yaml.v3"
)

var fmDelim = []byte("---")

// SplitFrontMatter separates a YAML front matter block from the Markdown body.
// A document without front matter returns nil meta and the whole input as body.
func SplitFrontMatter(src []byte) (meta, body []byte, err error) {
	src = bytes.TrimPrefix(src, []byte{0xEF, 0xBB, 0xBF}) // UTF-8 BOM
	if bytes.IndexByte(src, '\r') >= 0 {
		src = bytes.ReplaceAll(src, []byte("\r\n"), []byte("\n"))
	}
	if !bytes.HasPrefix(src, fmDelim) {
		return nil, src, nil
	}
	rest := src[len(fmDelim):]
	nl := bytes.IndexByte(rest, '\n')
	if nl < 0 || len(bytes.TrimSpace(rest[:nl])) != 0 {
		return nil, src, nil
	}
	rest = rest[nl+1:]
	for off := 0; off <= len(rest); {
		line := rest[off:]
		end := bytes.IndexByte(line, '\n')
		if end < 0 {
			end = len(line)
		}
		if bytes.Equal(bytes.TrimRight(line[:end], " \t"), fmDelim) {
			body = rest[min(off+end+1, len(rest)):]
			return rest[:off], body, nil
		}
		off += end + 1
	}
	return nil, nil, fmt.Errorf("front matter: missing closing ---")
}

// ParseDocument decodes front matter into meta (a pointer) and returns the body.
func ParseDocument(src []byte, meta any) (body []byte, err error) {
	m, body, err := SplitFrontMatter(src)
	if err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace(m)) > 0 {
		if err := yaml.Unmarshal(m, meta); err != nil {
			return nil, fmt.Errorf("front matter: %w", err)
		}
	}
	return body, nil
}

// FormatDocument writes meta as YAML front matter followed by body.
// Field order follows the struct definition, so diffs stay stable.
func FormatDocument(meta any, body []byte) ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteString("---\n")
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(meta); err != nil {
		return nil, err
	}
	enc.Close()
	buf.WriteString("---\n")
	body = bytes.ReplaceAll(body, []byte("\r\n"), []byte("\n"))
	buf.Write(bytes.TrimLeft(body, "\n"))
	if len(body) > 0 && !bytes.HasSuffix(body, []byte("\n")) {
		buf.WriteByte('\n')
	}
	return buf.Bytes(), nil
}

// ReadFile is fs.ReadFile without the copy for in-memory snapshots. Snapshots
// are immutable, so builds can share their bytes instead of duplicating them
// (which matters for large assets). Callers must not modify the result.
func ReadFile(fsys fs.FS, name string) ([]byte, error) {
	if m, ok := fsys.(fstest.MapFS); ok {
		if f, ok := m[name]; ok && !f.Mode.IsDir() {
			return f.Data, nil
		}
	}
	return fs.ReadFile(fsys, name)
}
