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

// --effective prints what commands use: the environment override applied and
// the defaults filled in, with the override named on stderr. The file itself
// is left alone, because overrides are never saved.
func TestShowEffectiveAppliesOverridesAndDefaults(t *testing.T) {
	home := t.TempDir()
	ws := filepath.Join(home, "demo")
	if err := os.MkdirAll(ws, 0o700); err != nil {
		t.Fatal(err)
	}
	file := "cluster: c\ntransit_gateway: t\n"
	if err := os.WriteFile(filepath.Join(ws, "config.yaml"), []byte(file), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ROKSBNKARGOCTL_HOME", home)
	t.Setenv("ROKSBNKARGOCTL_CLUSTER", "from-env")
	defer func() { flagWorkspace = "" }()
	root := newRoot()
	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.SetArgs([]string{"show", "--effective", "-w", "demo"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"cluster: from-env", "transit_gateway: t", "namespace: f5-bnk"} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("stdout lacks %q:\n%s", want, stdout.String())
		}
	}
	if !strings.Contains(stderr.String(), "cluster set by ROKSBNKARGOCTL_CLUSTER") {
		t.Errorf("stderr does not name the override: %q", stderr.String())
	}
	if b, _ := os.ReadFile(filepath.Join(ws, "config.yaml")); string(b) != file {
		t.Errorf("config.yaml changed: %q", b)
	}
}
