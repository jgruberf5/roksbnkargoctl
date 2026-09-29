package cli

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestSelfInstallCopiesOntoTheDirectory(t *testing.T) {
	self := installedBinary(t, "downloaded")
	dir := filepath.Join(t.TempDir(), "bin") // does not exist yet
	var log bytes.Buffer

	dest, err := installSelf(&log, "linux", self, dir, false)
	if err != nil {
		t.Fatal(err)
	}
	if dest != filepath.Join(dir, selfBinary) {
		t.Errorf("installed at %s", dest)
	}
	if got := readFile(t, dest); got != "OLD" {
		t.Errorf("dest holds %q, want a copy of the running binary", got)
	}
	info, err := os.Stat(dest)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o100 == 0 { // no exec bit on Windows
		t.Errorf("installed binary is not executable: %v", info.Mode())
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("stray files left in %s: %d entries", dir, len(entries))
	}
}

// Replacing an existing copy (what the one-line installers do on an upgrade).
func TestSelfInstallReplacesAnExistingCopy(t *testing.T) {
	for _, goos := range []string{"linux", "windows"} {
		t.Run(goos, func(t *testing.T) {
			dir := t.TempDir()
			name := selfBinary
			if goos == "windows" {
				name += ".exe"
			}
			if err := os.WriteFile(filepath.Join(dir, name), []byte("PREVIOUS"), 0o755); err != nil {
				t.Fatal(err)
			}
			src := filepath.Join(t.TempDir(), "new")
			if err := os.WriteFile(src, []byte("NEW"), 0o755); err != nil {
				t.Fatal(err)
			}
			dest, err := installSelf(&bytes.Buffer{}, goos, src, dir, true)
			if err != nil {
				t.Fatal(err)
			}
			if got := readFile(t, dest); got != "NEW" {
				t.Errorf("dest holds %q, want NEW", got)
			}
			if _, err := os.Stat(dest + ".old"); !os.IsNotExist(err) {
				t.Errorf("no sidecar should remain: %v", err)
			}
		})
	}
}

// windowsLock makes renameFile behave as Windows does for a running .exe:
// locked may be renamed away, but nothing may be renamed onto it while it is
// there. On Linux a rename onto an existing file just works, so without this a
// plain rename would pass for the move-aside path.
func windowsLock(t *testing.T, locked string) {
	t.Helper()
	orig := renameFile
	t.Cleanup(func() { renameFile = orig })
	renameFile = func(from, to string) error {
		if to == locked {
			if _, err := os.Stat(locked); err == nil {
				return &os.LinkError{Op: "rename", Old: from, New: to, Err: fs.ErrPermission}
			}
		}
		return orig(from, to)
	}
}

// The one-line installer runs `self install --force` over a copy that may be
// running (Windows locks it): it must be moved aside, not overwritten.
func TestSelfInstallOverALockedWindowsBinary(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, selfBinary+".exe")
	if err := os.WriteFile(dest, []byte("RUNNING"), 0o755); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(t.TempDir(), "new.exe")
	if err := os.WriteFile(src, []byte("NEW"), 0o755); err != nil {
		t.Fatal(err)
	}
	windowsLock(t, dest)
	if _, err := installSelf(&bytes.Buffer{}, "windows", src, dir, true); err != nil {
		t.Fatalf("installing over a locked binary: %v", err)
	}
	if got := readFile(t, dest); got != "NEW" {
		t.Errorf("dest holds %q, want NEW", got)
	}
}

// Without --force, the running binary at the destination is left alone; with
// it, the copy happens anyway.
func TestSelfInstallIsIdempotentUnlessForced(t *testing.T) {
	dir := t.TempDir()
	self := filepath.Join(dir, selfBinary)
	if err := os.WriteFile(self, []byte("SELF"), 0o755); err != nil {
		t.Fatal(err)
	}
	var log bytes.Buffer
	if _, err := installSelf(&log, "linux", self, dir, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(log.String(), "Already installed") {
		t.Errorf("want the no-op message:\n%s", log.String())
	}
	if strings.Contains(log.String(), "Copying") {
		t.Errorf("the running binary was copied onto itself without --force:\n%s", log.String())
	}

	log.Reset()
	if _, err := installSelf(&log, "linux", self, dir, true); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(log.String(), "Copying") {
		t.Errorf("--force should copy:\n%s", log.String())
	}
	if got := readFile(t, self); got != "SELF" {
		t.Errorf("a forced self-copy corrupted the binary: %q", got)
	}
}

func TestSelfInstallExpandsTilde(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	dest, err := installSelf(&bytes.Buffer{}, "linux", installedBinary(t, "x"), "~/bin", false)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, "bin", selfBinary); dest != want {
		t.Errorf("installed at %s, want %s", dest, want)
	}
}

func TestPATHGuidanceSuitsThePlatform(t *testing.T) {
	var win, unix bytes.Buffer
	printPATHGuidance(&win, "windows", `C:\tools`)
	printPATHGuidance(&unix, "linux", "/home/u/.local/bin")
	if !strings.Contains(win.String(), "SetEnvironmentVariable") || strings.Contains(win.String(), "export PATH") {
		t.Errorf("Windows guidance must be PowerShell:\n%s", win.String())
	}
	if !strings.Contains(unix.String(), `export PATH="/home/u/.local/bin:$PATH"`) {
		t.Errorf("unix guidance:\n%s", unix.String())
	}
}

func TestDirWritable(t *testing.T) {
	dir := t.TempDir()
	if !dirWritable(dir) {
		t.Error("a fresh temp dir should be writable")
	}
	if dirWritable(filepath.Join(dir, "does-not-exist")) {
		t.Error("a non-existent dir must report not writable")
	}
	f := filepath.Join(dir, "afile")
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if dirWritable(f) {
		t.Error("a regular file is not a writable directory")
	}
}

func TestIsUnderDir(t *testing.T) {
	base := t.TempDir()
	child := filepath.Join(base, "a", "b")
	if !isUnderDir("linux", child, base) || !isUnderDir("linux", base, base) {
		t.Error("a descendant, and the dir itself, are under it")
	}
	if isUnderDir("linux", base, child) || isUnderDir("linux", child, "") {
		t.Error("an ancestor, or an empty base, is not")
	}
	// A shared name prefix is not containment (/foo-bar is not under /foo).
	if isUnderDir("linux", base+"-sibling", base) {
		t.Error("a sibling sharing a prefix must not count")
	}
}

// `self` is registered, and BNK's own `install` is still BNK's.
func TestSelfCommandsAreRegistered(t *testing.T) {
	root := newRoot()
	for _, path := range [][]string{{"self", "install"}, {"self", "update"}} {
		c, _, err := root.Find(path)
		if err != nil || c.Name() != path[len(path)-1] || c.Parent().Name() != "self" {
			t.Errorf("%v is not registered: %v", path, err)
		}
	}
	inst, _, err := root.Find([]string{"install"})
	if err != nil || inst.Parent() != root || !strings.Contains(inst.Short, "Argo CD") {
		t.Errorf("top-level install must remain the BNK install, got %q", inst.Short)
	}
	upd, _, _ := root.Find([]string{"self", "update"})
	for _, flag := range []string{"version", "check"} {
		if upd.Flags().Lookup(flag) == nil {
			t.Errorf("self update has no --%s", flag)
		}
	}
	si, _, _ := root.Find([]string{"self", "install"})
	for _, flag := range []string{"dir", "force"} {
		if si.Flags().Lookup(flag) == nil {
			t.Errorf("self install has no --%s", flag)
		}
	}
}
