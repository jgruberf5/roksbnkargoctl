package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// runRoot runs the real root command with args and returns its combined
// output. Cobra binds persistent flags to package variables, so every one is
// reset afterwards: a test that passes -w or --no-workspace must not leak it
// into the next.
func runRoot(t *testing.T, args ...string) (string, error) {
	t.Helper()
	defer func() { flagWorkspace, flagYes, flagNoWorkspace, flagVerbose = "", false, false, false }()
	root := newRoot()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(args)
	err := root.Execute()
	return out.String(), err
}

// homeEntries lists every path under home, recursively, so a test can assert a
// command wrote nothing anywhere in it.
func homeEntries(t *testing.T, home string) []string {
	t.Helper()
	var out []string
	filepath.WalkDir(home, func(p string, _ os.DirEntry, _ error) error {
		if p != home {
			out = append(out, strings.TrimPrefix(p, home))
		}
		return nil
	})
	return out
}
