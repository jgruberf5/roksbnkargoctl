package cli

import (
	"archive/zip"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func readZip(t *testing.T, b []byte) map[string]string {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(rc)
		rc.Close()
		out[f.Name] = buf.String()
	}
	return out
}

// The export of a real render: every manifest under git.path, byte-for-byte,
// plus the instructions and the Application; no Secret; and nothing else.
func TestExportIsTheRenderedGitContentUnderGitPath(t *testing.T) {
	dir := renderedGitDir(t)
	ents, _ := os.ReadDir(dir)
	var buf bytes.Buffer
	n, err := writeExport(&buf, dir, "bnk/demo", "readme", []byte("app"))
	if err != nil {
		t.Fatal(err)
	}
	if n != len(ents) {
		t.Fatalf("exported %d of %d manifests", n, len(ents))
	}
	files := readZip(t, buf.Bytes())
	for _, e := range ents {
		want, _ := os.ReadFile(filepath.Join(dir, e.Name()))
		if files["bnk/demo/"+e.Name()] != string(want) {
			t.Errorf("%s missing or changed in the zip", e.Name())
		}
	}
	if files[exportReadme] != "readme" || files[exportApplication] != "app" {
		t.Error("instructions or Application missing")
	}
	if len(files) != len(ents)+2 {
		t.Errorf("zip has %d entries, want %d", len(files), len(ents)+2)
	}
	for name, body := range files {
		if strings.Contains(body, "\nkind: Secret") || strings.HasPrefix(body, "kind: Secret") {
			t.Errorf("%s is a Secret", name)
		}
	}
}

// An unchanged render exports byte-identical zips.
func TestExportIsDeterministic(t *testing.T) {
	dir := renderedGitDir(t)
	var a, b bytes.Buffer
	if _, err := writeExport(&a, dir, "bnk/demo", "r", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := writeExport(&b, dir, "bnk/demo", "r", nil); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a.Bytes(), b.Bytes()) {
		t.Fatal("two exports of the same render differ")
	}
	// Two exports in the same second match whatever the timestamp (zip keeps
	// 2-second resolution), so assert the timestamp itself: it must not be the
	// time of the export.
	zr, err := zip.NewReader(bytes.NewReader(a.Bytes()), int64(a.Len()))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range zr.File {
		if !f.Modified.Equal(exportEpoch) {
			t.Fatalf("%s has time %s, want the fixed %s", f.Name, f.Modified, exportEpoch)
		}
	}
}

// A Secret that found its way into manifests/git is refused, not exported.
func TestExportRefusesASecret(t *testing.T) {
	dir := renderedGitDir(t)
	if err := os.WriteFile(filepath.Join(dir, "999-secret-x.yaml"), []byte("apiVersion: v1\nkind: Secret\nmetadata:\n  name: x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := writeExport(&bytes.Buffer{}, dir, "bnk/demo", "r", nil); err == nil || !strings.Contains(err.Error(), "Secret") {
		t.Fatalf("want a refusal, got %v", err)
	}
}

func TestExportRefusesAPathOutsideTheRepo(t *testing.T) {
	dir := renderedGitDir(t)
	for _, p := range []string{"../elsewhere", "", "."} {
		if _, err := writeExport(&bytes.Buffer{}, dir, p, "r", nil); err == nil {
			t.Errorf("git.path %q accepted", p)
		}
	}
}
