// Package agent scaffolds the troubleshooting knowledge base into a workspace
// and launches the operator's own coding-agent CLI against it. roksbnkargoctl
// embeds no LLM.
package agent

import (
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"golang.org/x/term"
)

//go:embed files
var files embed.FS

// Init writes the scaffold into dir. Existing files are kept unless force, so
// an operator's edits to AGENTS.md survive a re-run.
func Init(dir string, force bool) (written []string, err error) {
	err = fs.WalkDir(files, "files", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel("files", p)
		dst := filepath.Join(dir, filepath.FromSlash(rel))
		if d.IsDir() {
			return os.MkdirAll(dst, 0o700)
		}
		if _, statErr := os.Stat(dst); statErr == nil && !force {
			return nil
		}
		b, err := files.ReadFile(p)
		if err != nil {
			return err
		}
		if err := os.WriteFile(dst, b, 0o600); err != nil {
			return err
		}
		written = append(written, rel)
		return nil
	})
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(dir, "journal"), 0o700); err != nil {
		return nil, err
	}
	claude := filepath.Join(dir, "CLAUDE.md")
	if _, err := os.Stat(claude); errors.Is(err, os.ErrNotExist) || force {
		if err := os.WriteFile(claude, []byte("@AGENTS.md\n"), 0o600); err != nil {
			return nil, err
		}
		written = append(written, "CLAUDE.md")
	}
	return written, nil
}

// Personas are the persona files the scaffold ships, by name.
var Personas = []string{"troubleshooter", "operator"}

// Prompt is the first turn an agent is started with: which files to read and
// which persona to take. Agents that load AGENTS.md by themselves still get it,
// because it also picks the persona.
func Prompt(persona string) string {
	return "Read AGENTS.md, then act as the " + persona + " persona (personas/" + persona + ".md). " +
		"Start from `roksbnkargoctl status`; run `roksbnkargoctl diagnose` before drawing conclusions."
}

// binaries is the executable each supported CLI starts.
var binaries = map[string]string{
	"claude": "claude", "codex": "codex", "gemini": "gemini", "aider": "aider",
	"pi": "pi", "opencode": "opencode", "agy": "agy",
}

// Names lists the supported CLIs.
func Names() []string {
	var out []string
	for k := range binaries {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Argv is the command that starts cli with persona. Each CLI takes its first
// turn differently:
//   - claude, codex: a positional prompt starts an interactive session with it
//   - agy, gemini: -i runs the prompt and continues interactively
//   - aider: no initial prompt; the files are loaded read-only instead
//   - pi, opencode: started plainly; both read AGENTS.md from the directory
func Argv(cli, persona string) ([]string, error) {
	if _, ok := binaries[cli]; !ok {
		return nil, fmt.Errorf("unknown agent CLI %q (supported: %v)", cli, Names())
	}
	if !slices.Contains(Personas, persona) {
		return nil, fmt.Errorf("unknown persona %q (supported: %v)", persona, Personas)
	}
	prompt := Prompt(persona)
	switch cli {
	case "claude", "codex":
		return []string{cli, prompt}, nil
	case "agy", "gemini":
		return []string{cli, "-i", prompt}, nil
	case "aider":
		return []string{"aider", "--read", "AGENTS.md", "--read", "personas/" + persona + ".md"}, nil
	default:
		return []string{cli}, nil
	}
}

// Show renders argv as a shell command line, for --show.
func Show(argv []string) string {
	q := make([]string, len(argv))
	for i, a := range argv {
		if strings.ContainsAny(a, " \t\"'`$()&;|<>*?!#") || a == "" {
			a = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
		}
		q[i] = a
	}
	return strings.Join(q, " ")
}

// StdoutIsTerminal is a variable so the refusal in Command can be tested both
// ways (under go test stdout is never a terminal).
var StdoutIsTerminal = func() bool { return term.IsTerminal(int(os.Stdout.Fd())) }

// Command returns the command that starts cli in dir. Starting the operator's
// chosen agent is the one place this tool runs another program: it is the
// feature, not a substitute for doing the work in-process.
//
// It refuses when stdout is not a terminal: an agent session needs one, and a
// captured session — `$(roksbnkargoctl agent claude)` or a pipe — would send
// whatever the model prints somewhere it does not belong (roksbnkctl's rule).
func Command(cli, persona, dir string) (*exec.Cmd, error) {
	argv, err := Argv(cli, persona)
	if err != nil {
		return nil, err
	}
	if !StdoutIsTerminal() {
		return nil, fmt.Errorf("refusing to start %s: stdout is not a terminal; to see the command instead: roksbnkargoctl agent %s --show", cli, cli)
	}
	path, err := exec.LookPath(argv[0])
	if err != nil {
		return nil, fmt.Errorf("%s is not installed or not on PATH (the command it would run: %s)", argv[0], Show(argv))
	}
	cmd := exec.Command(path, argv[1:]...)
	cmd.Dir = dir
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd, nil
}
