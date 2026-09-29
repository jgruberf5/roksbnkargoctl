package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Review findings: `agent <cli> --show` scaffolded AGENTS.md although it only
// prints, and left the workspace directory unquoted, so a path with a space
// did not paste back as one command.
func TestAgentShowOnlyPrintsAndQuotesTheDirectory(t *testing.T) {
	home := filepath.Join(t.TempDir(), "my home")
	ws := filepath.Join(home, "demo")
	if err := os.MkdirAll(ws, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "config.yaml"), []byte("cluster: c\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ROKSBNKARGOCTL_HOME", home)
	defer func() { flagWorkspace = "" }()

	root := newRoot()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"agent", "agy", "--show", "-w", "demo"})
	if err := root.Execute(); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if want := "cd '" + ws + "' && agy -i "; !strings.HasPrefix(out.String(), want) {
		t.Errorf("got %q, want prefix %q", out.String(), want)
	}
	if _, err := os.Stat(filepath.Join(ws, "AGENTS.md")); err == nil {
		t.Error("--show scaffolded AGENTS.md")
	}
}
