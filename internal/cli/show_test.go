package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// stdout is exactly the file, so `show > config.yaml` round-trips; the path
// goes to stderr.
func TestShowPrintsTheWorkspaceConfigVerbatim(t *testing.T) {
	home := t.TempDir()
	ws := filepath.Join(home, "demo")
	if err := os.MkdirAll(ws, 0o700); err != nil {
		t.Fatal(err)
	}
	want := "cluster: c\ntransit_gateway: t # a comment kept as written\n"
	if err := os.WriteFile(filepath.Join(ws, "config.yaml"), []byte(want), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ROKSBNKARGOCTL_HOME", home)
	defer func() { flagWorkspace = "" }()
	root := newRoot()
	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.SetArgs([]string{"show", "-w", "demo"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if stdout.String() != want {
		t.Errorf("stdout %q, want the file verbatim %q", stdout.String(), want)
	}
	if !strings.Contains(stderr.String(), filepath.Join(ws, "config.yaml")) {
		t.Errorf("stderr does not name the file: %q", stderr.String())
	}
}
