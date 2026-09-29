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
	"sort"
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

// CLIs are the agent CLIs `agent <cli>` knows how to start, and the argv each
// uses to load AGENTS.md.
var CLIs = map[string][]string{
	"claude": {"claude"},
	"codex":  {"codex"},
	"gemini": {"gemini"},
	"aider":  {"aider", "--read", "AGENTS.md", "--read", "personas/troubleshooter.md"},
}

// Names lists the supported CLIs.
func Names() []string {
	var out []string
	for k := range CLIs {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Command returns the command that starts cli in dir. Starting the operator's
// chosen agent is the one place this tool runs another program: it is the
// feature, not a substitute for doing the work in-process.
func Command(cli, dir string) (*exec.Cmd, error) {
	argv, ok := CLIs[cli]
	if !ok {
		return nil, fmt.Errorf("unknown agent CLI %q (supported: %v)", cli, Names())
	}
	path, err := exec.LookPath(argv[0])
	if err != nil {
		return nil, fmt.Errorf("%s is not installed or not on PATH", argv[0])
	}
	cmd := exec.Command(path, argv[1:]...)
	cmd.Dir = dir
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd, nil
}
