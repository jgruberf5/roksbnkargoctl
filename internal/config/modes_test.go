package config

import (
	"os/exec"
	"strings"
	"testing"
)

// v0.5.0's book job failed "scripts/book-pdf.sh: Permission denied": every
// script had been committed 100644 from a checkout with core.fileMode=false,
// which also left .githooks/pre-push silently ignored by git. Hold the
// executables' modes in the index, where a fresh clone gets them.
func TestScriptsAreExecutableInGit(t *testing.T) {
	out, err := exec.Command("git", "-C", "../..", "ls-files", "-s", ".githooks/pre-push", "install.sh", "scripts").Output()
	if err != nil {
		t.Skipf("not a git checkout: %v", err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		f := strings.Fields(line)
		if len(f) == 4 && strings.HasSuffix(f[3], ".sh") || len(f) == 4 && f[3] == ".githooks/pre-push" {
			if f[0] != "100755" {
				t.Errorf("%s is %s in git, want 100755", f[3], f[0])
			}
		}
	}
}
