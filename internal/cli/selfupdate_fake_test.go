package cli

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
)

// fakeGitHub stands in for the GitHub releases API and the release downloads,
// so the self-update pipeline and install.sh run end to end with no network.

type fakeRelease struct {
	tag        string
	draft, pre bool
	// assets maps an asset name to its bytes.
	assets map[string][]byte
	// sums, when non-nil, replaces the generated checksums file; noSums omits it.
	sums   []byte
	noSums bool
}

type fakeGitHub struct {
	*httptest.Server
	rels []fakeRelease // newest first, as GitHub lists them

	mu              sync.Mutex
	downloadAuthHdr []string // Authorization seen on asset downloads
	apiAuthHdr      []string // Authorization seen on API requests
	rateLimited     bool
	pretty          bool // indent the JSON, as api.github.com does for curl
}

func newFakeGitHub(t *testing.T, rels ...fakeRelease) *fakeGitHub {
	t.Helper()
	f := &fakeGitHub{rels: rels}
	// Assigned before Start, so handlers reading f.URL are ordered after the
	// write: the requests come from another process (install.sh), which the
	// race detector cannot see.
	f.Server = httptest.NewUnstartedServer(http.HandlerFunc(f.serve))
	f.Start()
	t.Cleanup(f.Close)
	return f
}

const fakeRepoPath = "/repos/" + selfRepo + "/releases"

func (f *fakeGitHub) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	limited, pretty := f.rateLimited, f.pretty
	f.mu.Unlock()
	enc := json.NewEncoder(w)
	if pretty {
		enc.SetIndent("", "  ")
	}
	switch {
	case strings.HasPrefix(r.URL.Path, "/dl/"):
		f.mu.Lock()
		f.downloadAuthHdr = append(f.downloadAuthHdr, r.Header.Get("Authorization"))
		f.mu.Unlock()
		parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/dl/"), "/", 2)
		for _, rel := range f.rels {
			if rel.tag != parts[0] {
				continue
			}
			if b, ok := f.files(rel)[parts[1]]; ok {
				_, _ = w.Write(b)
				return
			}
		}
		http.NotFound(w, r)
	case func() bool {
		f.mu.Lock()
		f.apiAuthHdr = append(f.apiAuthHdr, r.Header.Get("Authorization"))
		f.mu.Unlock()
		return false
	}():
	case limited:
		w.Header().Set("X-RateLimit-Remaining", "0")
		http.Error(w, `{"message":"API rate limit exceeded"}`, http.StatusForbidden)
	case r.URL.Path == fakeRepoPath:
		var out []map[string]any
		for _, rel := range f.rels {
			out = append(out, f.json(rel))
		}
		_ = enc.Encode(out)
	case r.URL.Path == fakeRepoPath+"/latest":
		for _, rel := range f.rels {
			if !rel.draft && !rel.pre {
				_ = enc.Encode(f.json(rel))
				return
			}
		}
		http.NotFound(w, r)
	case strings.HasPrefix(r.URL.Path, fakeRepoPath+"/tags/"):
		tag := strings.TrimPrefix(r.URL.Path, fakeRepoPath+"/tags/")
		for _, rel := range f.rels {
			if rel.tag == tag {
				_ = enc.Encode(f.json(rel))
				return
			}
		}
		http.NotFound(w, r)
	default:
		http.NotFound(w, r)
	}
}

// files is every downloadable file of rel, checksums included.
func (f *fakeGitHub) files(rel fakeRelease) map[string][]byte {
	out := map[string][]byte{}
	var sums bytes.Buffer
	for name, b := range rel.assets {
		out[name] = b
		fmt.Fprintf(&sums, "%s  %s\n", sha256Hex(b), name)
	}
	if !rel.noSums {
		if rel.sums != nil {
			out[checksumsName(rel.tag)] = rel.sums
		} else {
			out[checksumsName(rel.tag)] = sums.Bytes()
		}
	}
	return out
}

func (f *fakeGitHub) json(rel fakeRelease) map[string]any {
	var assets []map[string]any
	for name, b := range f.files(rel) {
		assets = append(assets, map[string]any{
			"name": name, "size": len(b),
			"browser_download_url": f.URL + "/dl/" + rel.tag + "/" + name,
		})
	}
	return map[string]any{
		"tag_name": rel.tag, "draft": rel.draft, "prerelease": rel.pre, "assets": assets,
		// Release notes that mention the keys the shell installer scans for:
		// they arrive JSON-escaped and must not be mistaken for real fields.
		"body": `see "browser_download_url": "https://evil.example/x", "tag_name": "v9.9.9"`,
	}
}

func (f *fakeGitHub) downloadAuth() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.downloadAuthHdr...)
}

// release builds a fakeRelease whose archives each hold a binary whose bytes
// name the release and BNK line, so a test can tell which one was installed.
func release(t *testing.T, tag string, lines []string, platforms ...string) fakeRelease {
	t.Helper()
	rel := fakeRelease{tag: tag, assets: map[string][]byte{}}
	for _, bnk := range lines {
		for _, p := range platforms {
			goos, goarch, _ := strings.Cut(p, "/")
			rel.assets[assetName(tag, bnk, goos, goarch)] = archiveFor(t, goos, binaryFor(tag, bnk))
		}
	}
	return rel
}

func binaryFor(tag, bnk string) []byte { return []byte("binary " + tag + " bnk " + bnk) }

func archiveFor(t *testing.T, goos string, bin []byte) []byte {
	t.Helper()
	if goos == "windows" {
		return zipOf(t, map[string][]byte{"LICENSE": []byte("license"), selfBinary + ".exe": bin})
	}
	return tarGzOf(t, map[string][]byte{"LICENSE": []byte("license"), selfBinary: bin})
}

func tarGzOf(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	// LICENSE first, so an extractor must skip past it to reach the binary.
	for _, name := range sortedKeys(files) {
		b := files[name]
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(b)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(b); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func zipOf(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, name := range sortedKeys(files) {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(files[name]); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func sortedKeys(m map[string][]byte) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	// "LICENSE" sorts before "roksbnkargoctl", which is the order wanted.
	sort.Strings(keys)
	return keys
}
