package cli

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Ported from roksbnkctl (#146 there). installByMoveAside documented a guarantee — "the caller is never left
// without a working binary" — that the code did not keep: the rollback's error
// was discarded with `_ =`, so a failed rollback returned an error naming the
// wrong problem ("installing new binary") while target no longer existed.
//
// The path exists BECAUSE Windows locks a running .exe, which is exactly where
// a second process holding a handle mid-update is routine rather than exotic.

// The unrecoverable state, driven through the real function: the staged binary
// will not move into place AND the old one will not move back. Ordinary
// permissions cannot produce this — target and target.old share a directory, so
// anything that fails the rollback also fails the first rename — so the rename
// syscall is stubbed for the duration.
//
// This is the branch that strands a user, and it is the reason the fix exists;
// leaving it to a message-only test would cover the wording and not the path.
func TestUnrecoverableRollbackLeavesNoBinaryAndSaysSo(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "roksbnkargoctl")
	if err := os.WriteFile(target, []byte("the real binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	staged := filepath.Join(dir, "roksbnkargoctl.new")
	if err := os.WriteFile(staged, []byte("the new binary"), 0o755); err != nil {
		t.Fatal(err)
	}

	// Let the first rename through (that is the move-aside), then fail every
	// rename after it: the install AND the rollback.
	orig := renameFile
	t.Cleanup(func() { renameFile = orig })
	calls := 0
	renameFile = func(from, to string) error {
		calls++
		if calls == 1 {
			return orig(from, to)
		}
		return fs.ErrPermission
	}

	err := installByMoveAside(target, staged)
	if err == nil {
		t.Fatal("both the install and the rollback failed; this must return an error")
	}
	// At least move-aside, install, rollback. Not an equality check: retrying
	// the rollback is a plausible refactor on this exact function and would not
	// change the behaviour any of these assertions care about.
	if calls < 3 {
		t.Errorf("expected at least move-aside, install and rollback, got %d renames", calls)
	}

	// The filesystem is genuinely in the bad state — this is not a simulated
	// message, the binary really is gone from target.
	if _, serr := os.Stat(target); !os.IsNotExist(serr) {
		t.Errorf("target should not exist after a failed rollback, stat gave: %v", serr)
	}
	if _, serr := os.Stat(target + ".old"); serr != nil {
		t.Fatalf("the only surviving binary should be the sidecar: %v", serr)
	}

	msg := err.Error()
	if !strings.Contains(msg, "NO binary") {
		t.Errorf("the error must state that no binary remains:\n%s", msg)
	}
	assertRecoveryCommandIsUsable(t, msg, target+".old", target)

	// The precise regression: the old code returned this text alone, which
	// points at the new binary and hides the missing one.
	if msg == "installing new binary: "+fs.ErrPermission.Error() {
		t.Error("this is the pre-fix message — it names the wrong problem and omits the recovery")
	}
}

// A rollback that SUCCEEDS is the ordinary recoverable failure, and must not
// alarm: the binary is back, so the error stays the plain one. Without this,
// a fix that always reported the scary message would pass the test above.
func TestSuccessfulRollbackKeepsThePlainError(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "roksbnkargoctl")
	if err := os.WriteFile(target, []byte("the real binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	staged := filepath.Join(dir, "roksbnkargoctl.new")
	if err := os.WriteFile(staged, []byte("the new binary"), 0o755); err != nil {
		t.Fatal(err)
	}

	orig := renameFile
	t.Cleanup(func() { renameFile = orig })
	calls := 0
	renameFile = func(from, to string) error {
		calls++
		if calls == 2 { // fail only the install; let the rollback through
			return fs.ErrPermission
		}
		return orig(from, to)
	}

	err := installByMoveAside(target, staged)
	if err == nil {
		t.Fatal("the install failed, so an error is expected")
	}
	if strings.Contains(err.Error(), "NO binary") {
		t.Errorf("the rollback worked — the error must not claim the binary is gone:\n%s", err)
	}

	// And the original binary really is back in place.
	got, rerr := os.ReadFile(target)
	if rerr != nil {
		t.Fatalf("the rollback should have restored target: %v", rerr)
	}
	if string(got) != "the real binary" {
		t.Errorf("target holds %q, want the original binary", got)
	}
}

// The unrecoverable case, driven directly: both renames fail. Constructed by
// calling the error type the way installByMoveAside does, and asserting the
// message carries what a stranded user needs.
func TestRollbackFailureMessageNamesBothFilesAndTheCommand(t *testing.T) {
	install := errors.New("access is denied")
	rollback := fs.ErrPermission
	e := errRollbackFailed{
		install:  install,
		rollback: rollback,
		old:      `C:\tools\roksbnkargoctl.exe.old`,
		target:   `C:\tools\roksbnkargoctl.exe`,
	}

	msg := e.Error()
	for _, want := range []string{
		"NO binary",        // states the actual condition
		"access is denied", // why the install failed
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("the message must contain %q:\n%s", want, msg)
		}
	}
	assertRecoveryCommandIsUsable(t, msg, `C:\tools\roksbnkargoctl.exe.old`, `C:\tools\roksbnkargoctl.exe`)

	// It must NOT read as a plain install failure — that is the misdirection
	// this fixes. The old code returned "installing new binary: ..." and said
	// nothing about the missing file.
	if !strings.Contains(msg, "rolling back also failed") {
		t.Errorf("the message must distinguish itself from an ordinary install failure:\n%s", msg)
	}

	// Both causes stay reachable, so a caller inspecting for a permission
	// problem still finds it through either one.
	if !errors.Is(e, fs.ErrPermission) {
		t.Error("errors.Is must match the rollback cause")
	}
	if !errors.Is(e, install) {
		t.Error("errors.Is must match the install cause")
	}
}

// assertRecoveryCommandIsUsable checks the LAST line of the message — the
// copy-pasteable command — rather than the message as a whole.
//
// This distinction is the whole point. The first version of these tests looked
// for the path and the verb anywhere in the message, and passed against a
// command whose arguments were mangled: the path appeared correctly in the
// prose sentence above it, and the verb matched as a bare substring. A test
// that cannot fail on a broken command does not guard the command.
func assertRecoveryCommandIsUsable(t *testing.T, msg, wantOld, wantTarget string) {
	t.Helper()

	lines := strings.Split(strings.TrimRight(msg, "\n"), "\n")
	cmd := strings.TrimSpace(lines[len(lines)-1])
	if cmd == "" {
		t.Fatalf("no recovery command on the last line of:\n%s", msg)
	}

	// Both paths must appear in the COMMAND exactly as they are on disk. %q
	// would double every backslash, producing a command naming a path that does
	// not exist — and the prose line would still look correct.
	for _, want := range []string{wantOld, wantTarget} {
		if !strings.Contains(cmd, want) {
			t.Errorf("the recovery command does not contain the literal path %s:\n  %s", want, cmd)
		}
	}
	if strings.Contains(cmd, `\\`) {
		t.Errorf("the recovery command has escaped backslashes, so the paths are wrong:\n  %s", cmd)
	}

	// And it must be a command the user's shell actually has. installBinary
	// only reaches installByMoveAside on Windows, where `mv` is not a command.
	if runtime.GOOS == "windows" && !strings.Contains(cmd, "Move-Item") {
		t.Errorf("on Windows the recovery must use Move-Item, not Unix advice:\n  %s", cmd)
	}
}

// installBinary only reaches installByMoveAside when GOOS is windows, so every
// real reader of this message is on Windows — where `mv` is not a command.
// cmd.exe has no builtin and ships no mv.exe; PowerShell aliases it, so Unix
// advice works in one of the two shells and fails silently in the other, at the
// moment the user has no working binary.
//
// This is asserted through the goos parameter rather than runtime.GOOS because
// CI builds for Windows but runs tests only on ubuntu and macos. A test gated on
// the host OS would never execute the branch that actually ships.
func TestTheRecoveryCommandSuitsThePlatformItShipsTo(t *testing.T) {
	const old, target = `C:\tools\roksbnkargoctl.exe.old`, `C:\tools\roksbnkargoctl.exe`

	win := recoverCommand("windows", old, target)
	if !strings.Contains(win, "Move-Item") {
		t.Errorf("Windows has no mv; the recovery must use Move-Item:\n  %s", win)
	}
	if strings.HasPrefix(strings.TrimSpace(win), "mv ") {
		t.Errorf("this is the Unix advice roksbnkctl already had to fix once:\n  %s", win)
	}
	// The paths must survive verbatim. %q would double every backslash and
	// produce a command naming a path that does not exist.
	for _, want := range []string{old, target} {
		if !strings.Contains(win, want) {
			t.Errorf("the command must contain the literal path %s:\n  %s", want, win)
		}
	}
	if strings.Contains(win, `\\`) {
		t.Errorf("escaped backslashes — the paths in this command are wrong:\n  %s", win)
	}

	// Unix keeps mv. The function is not reached there today, but a message
	// that is wrong for the host is worse than one that is merely unused.
	unix := recoverCommand("linux", "/usr/local/bin/roksbnkargoctl.old", "/usr/local/bin/roksbnkargoctl")
	if !strings.Contains(unix, "mv ") {
		t.Errorf("on unix the recovery should be mv:\n  %s", unix)
	}
}
