package agent

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

func TestArgvStartsEachCLIWithThePersona(t *testing.T) {
	p := Prompt("troubleshooter")
	for cli, want := range map[string][]string{
		"agy":      {"agy", "-i", p},
		"gemini":   {"gemini", "-i", p},
		"claude":   {"claude", p},
		"codex":    {"codex", p},
		"aider":    {"aider", "--read", "AGENTS.md", "--read", "personas/troubleshooter.md"},
		"pi":       {"pi"},
		"opencode": {"opencode"},
	} {
		got, err := Argv(cli, "troubleshooter")
		if err != nil || !slices.Equal(got, want) {
			t.Errorf("%s: got %q, %v; want %q", cli, got, err, want)
		}
	}
	if _, err := Argv("vim", "troubleshooter"); err == nil {
		t.Error("an unknown CLI was accepted")
	}
	if _, err := Argv("agy", "wizard"); err == nil {
		t.Error("an unknown persona was accepted")
	}
	if !strings.Contains(Prompt("operator"), "personas/operator.md") {
		t.Error("the operator prompt does not name its persona file")
	}
}

// Every persona the prompt can name must be a file the scaffold writes.
func TestEveryPersonaIsScaffolded(t *testing.T) {
	dir := t.TempDir()
	if _, err := Init(dir, false); err != nil {
		t.Fatal(err)
	}
	for _, p := range Personas {
		if _, err := os.Stat(filepath.Join(dir, "personas", p+".md")); err != nil {
			t.Errorf("persona %s: %v", p, err)
		}
	}
}

func TestCommandRefusesWithoutATerminal(t *testing.T) {
	defer func(f func() bool) { StdoutIsTerminal = f }(StdoutIsTerminal)
	StdoutIsTerminal = func() bool { return false }
	if _, err := Command("agy", "troubleshooter", t.TempDir()); err == nil || !strings.Contains(err.Error(), "not a terminal") {
		t.Fatalf("want a refusal, got %v", err)
	}
}

// Runs a stand-in agy from PATH and checks what it was started with and where.
func TestCommandStartsTheAgentInTheWorkspace(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the stand-in agent is a shell script")
	}
	defer func(f func() bool) { StdoutIsTerminal = f }(StdoutIsTerminal)
	StdoutIsTerminal = func() bool { return true }
	bin, ws := t.TempDir(), t.TempDir()
	rec := filepath.Join(t.TempDir(), "rec")
	script := "#!/bin/sh\npwd > " + rec + "\nfor a in \"$@\"; do echo \"$a\" >> " + rec + "; done\n"
	if err := os.WriteFile(filepath.Join(bin, "agy"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	c, err := Command("agy", "operator", ws)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Run(); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(rec)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	gotDir, _ := filepath.EvalSymlinks(lines[0])
	wantDir, _ := filepath.EvalSymlinks(ws)
	if gotDir != wantDir || !slices.Equal(lines[1:], []string{"-i", Prompt("operator")}) {
		t.Fatalf("agy ran in %s with %q", lines[0], lines[1:])
	}
}

func TestShowQuotesThePrompt(t *testing.T) {
	got := Show([]string{"agy", "-i", "it's a prompt"})
	if got != `agy -i 'it'\''s a prompt'` {
		t.Fatalf("got %s", got)
	}
}
