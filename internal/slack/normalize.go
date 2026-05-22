package slack

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// NormalizedHash returns a SHA-256 over the deterministic shape of files
// written by the poller. It strips time-dependent fields (captured_at,
// received_at, tracking_since, last_event_at, created_at, occurred_at) so
// the hash is reproducible across runs.
//
// Files considered (under stateDir):
//
//	threads/slack-*/raw/<ts>.json
//	threads/slack-*/meta.json
//	threads/slack-*/.dirty                (presence only)
//	inbox/new/slack-*.json
//	inbox/updates/slack-*.json
//	inbox/anomalies/slack-*.json
//	sources/slack/cursors/channels/*.json
//	sources/slack/cursors/threads/*.json
//
// Order is lexicographic by path.
func NormalizedHash(stateDir string) string {
	files := collectSnapshotFiles(stateDir)
	if len(files) == 0 {
		return ""
	}
	h := sha256.New()
	for _, f := range files {
		normalized, err := normalizeFile(f.path, f.kind)
		if err != nil {
			fmt.Fprintf(h, "ERR %s %v\n", f.path, err)
			continue
		}
		// Use the path relative to stateDir so different temp dirs hash to
		// the same value.
		rel, _ := filepath.Rel(stateDir, f.path)
		fmt.Fprintf(h, "FILE %s\n", filepath.ToSlash(rel))
		h.Write(normalized)
		fmt.Fprintln(h)
	}
	return hex.EncodeToString(h.Sum(nil))
}

type snapshotFile struct {
	path string
	kind string // "json" | "marker"
}

func collectSnapshotFiles(stateDir string) []snapshotFile {
	var out []snapshotFile
	roots := []struct {
		path    string
		prefix  string
		kind    string
	}{
		{filepath.Join(stateDir, "threads"), "slack-", "tree"},
		{filepath.Join(stateDir, "inbox", "new"), "slack-", "json"},
		{filepath.Join(stateDir, "inbox", "updates"), "slack-", "json"},
		{filepath.Join(stateDir, "inbox", "anomalies"), "slack-", "json"},
		{filepath.Join(stateDir, "sources", "slack", "cursors", "channels"), "", "json"},
		{filepath.Join(stateDir, "sources", "slack", "cursors", "threads"), "slack-", "json"},
	}
	for _, r := range roots {
		switch r.kind {
		case "tree":
			// Walk threads/<id>/...
			_ = filepath.WalkDir(r.path, func(p string, d fs.DirEntry, err error) error {
				if err != nil {
					return nil
				}
				if d.IsDir() {
					return nil
				}
				rel, _ := filepath.Rel(r.path, p)
				parts := strings.Split(filepath.ToSlash(rel), "/")
				if len(parts) == 0 || !strings.HasPrefix(parts[0], r.prefix) {
					return nil
				}
				if filepath.Base(p) == ".dirty" {
					out = append(out, snapshotFile{path: p, kind: "marker"})
				} else if strings.HasSuffix(p, ".json") {
					out = append(out, snapshotFile{path: p, kind: "json"})
				}
				return nil
			})
		case "json":
			entries, _ := os.ReadDir(r.path)
			for _, e := range entries {
				if e.IsDir() {
					continue
				}
				if !strings.HasSuffix(e.Name(), ".json") {
					continue
				}
				if r.prefix != "" && !strings.HasPrefix(e.Name(), r.prefix) {
					continue
				}
				out = append(out, snapshotFile{
					path: filepath.Join(r.path, e.Name()),
					kind: "json",
				})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].path < out[j].path })
	return out
}

// normalizeFile returns canonical bytes for path. For JSON files, parses,
// strips wall-clock fields, and re-marshals deterministically. For marker
// files (.dirty), returns a static byte string.
func normalizeFile(path, kind string) ([]byte, error) {
	if kind == "marker" {
		return []byte("DIRTY"), nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var obj any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, err
	}
	stripVolatile(obj)
	return canonicalJSON(obj), nil
}

// stripVolatile recursively removes wall-clock fields from a decoded JSON
// value.
func stripVolatile(v any) {
	switch m := v.(type) {
	case map[string]any:
		for _, k := range []string{
			"captured_at", "received_at", "tracking_since",
			"last_event_at", "created_at", "occurred_at",
		} {
			delete(m, k)
		}
		for _, vv := range m {
			stripVolatile(vv)
		}
	case []any:
		for _, vv := range m {
			stripVolatile(vv)
		}
	}
}

// canonicalJSON marshals v with sorted keys and no whitespace.
func canonicalJSON(v any) []byte {
	b, _ := canonicalMarshal(v)
	return b
}

// canonicalMarshal is json.Marshal with sorted object keys.
func canonicalMarshal(v any) ([]byte, error) {
	switch t := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var b strings.Builder
		b.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				b.WriteByte(',')
			}
			kBytes, _ := json.Marshal(k)
			b.Write(kBytes)
			b.WriteByte(':')
			vBytes, err := canonicalMarshal(t[k])
			if err != nil {
				return nil, err
			}
			b.Write(vBytes)
		}
		b.WriteByte('}')
		return []byte(b.String()), nil
	case []any:
		var b strings.Builder
		b.WriteByte('[')
		for i, vv := range t {
			if i > 0 {
				b.WriteByte(',')
			}
			vBytes, err := canonicalMarshal(vv)
			if err != nil {
				return nil, err
			}
			b.Write(vBytes)
		}
		b.WriteByte(']')
		return []byte(b.String()), nil
	default:
		return json.Marshal(v)
	}
}
