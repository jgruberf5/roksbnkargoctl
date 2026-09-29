package cli

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// install.sh, run for real with sh against a fake GitHub serving fake release
// archives. The "binary" in each archive is a shell script that records how it
// was invoked, and which release and BNK line it came from, so each test can
// see exactly what the installer chose and handed off to.

// shRelease is a release whose archive for this host holds a recording script.
func shRelease(t *testing.T, tag string, lines ...string) fakeRelease {
	t.Helper()
	rel := fakeRelease{tag: tag, assets: map[string][]byte{}}
	for _, bnk := range lines {
		script := "#!/bin/sh\n" +
			"{ echo \"from $(dirname \"$0\")\"; echo \"args $*\"; echo \"installed " + tag + " bnk " + bnk + "\"; } > \"$MARKER\"\n"
		rel.assets[assetName(tag, bnk, runtime.GOOS, runtime.GOARCH)] = tarGzOf(t, map[string][]byte{
			"LICENSE": []byte("license"), selfBinary: []byte(script),
		})
	}
	return rel
}

type shResult struct {
	err    error
	stderr string
	marker string // empty when the binary was never run
}

func runInstallSh(t *testing.T, f *fakeGitHub, env []string, args ...string) shResult {
	t.Helper()
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("install.sh is the Linux/macOS installer")
	}
	if runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64" {
		t.Skip("install.sh supports amd64 and arm64")
	}
	for _, tool := range []string{"sh", "curl", "tar"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not available", tool)
		}
	}
	marker := filepath.Join(t.TempDir(), "marker")
	cmd := exec.Command("sh", append([]string{"../../install.sh"}, args...)...)
	cmd.Env = append(os.Environ(),
		"ROKSBNKARGOCTL_GITHUB_API="+f.URL, "MARKER="+marker,
		"VERSION=", "BNK_VERSION=", "ROKSBNKARGOCTL_INSTALL_ARGS=", "GITHUB_TOKEN=")
	cmd.Env = append(cmd.Env, env...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.Stdout = &stderr
	err := cmd.Run()
	m, _ := os.ReadFile(marker)
	return shResult{err: err, stderr: stderr.String(), marker: string(m)}
}

// Compact and indented JSON both parse: the installer reads it with tr and sed.
func TestInstallShInstallsTheLatestForTheDefaultBNKLine(t *testing.T) {
	for _, pretty := range []bool{false, true} {
		t.Run(map[bool]string{false: "compact", true: "indented"}[pretty], func(t *testing.T) {
			f := newFakeGitHub(t, shRelease(t, "v0.6.0", "2.4.0", "2.5.0"), shRelease(t, "v0.5.0", "2.4.0"))
			f.mu.Lock()
			f.pretty = pretty
			f.mu.Unlock()
			installShLatest(t, f)
		})
	}
}

func installShLatest(t *testing.T, f *fakeGitHub) {
	t.Helper()
	r := runInstallSh(t, f, []string{"ROKSBNKARGOCTL_INSTALL_ARGS=--dir /opt/x/bin"})
	if r.err != nil {
		t.Fatalf("install.sh failed: %v\n%s", r.err, r.stderr)
	}
	if !strings.Contains(r.marker, "installed v0.6.0 bnk 2.4.0") {
		t.Errorf("wrong archive installed:\n%s", r.marker)
	}
	if !strings.Contains(r.marker, "args self install --force --dir /opt/x/bin") {
		t.Errorf("wrong hand-off:\n%s", r.marker)
	}
	if !strings.Contains(r.stderr, "Checksum OK.") {
		t.Errorf("the checksum was not verified:\n%s", r.stderr)
	}
	// The temp dir the binary ran from is removed on exit.
	from := strings.TrimPrefix(strings.SplitN(r.marker, "\n", 2)[0], "from ")
	if from == "" || from == r.marker {
		t.Fatalf("no temp dir recorded:\n%s", r.marker)
	}
	if _, err := os.Stat(from); !os.IsNotExist(err) {
		t.Errorf("the temp dir %s survived the installer: %v", from, err)
	}
}

func TestInstallShSelectsBNKVersion(t *testing.T) {
	f := newFakeGitHub(t, shRelease(t, "v0.6.0", "2.4.0", "2.5.0"))
	r := runInstallSh(t, f, []string{"BNK_VERSION=2.5.0"})
	if r.err != nil {
		t.Fatalf("install.sh failed: %v\n%s", r.err, r.stderr)
	}
	if !strings.Contains(r.marker, "installed v0.6.0 bnk 2.5.0") {
		t.Errorf("BNK_VERSION=2.5.0 installed:\n%s", r.marker)
	}
}

// The latest release has only another BNK line: fail, name what it does have,
// and run nothing.
func TestInstallShRefusesALatestWithoutThisBNKLine(t *testing.T) {
	f := newFakeGitHub(t, shRelease(t, "v0.7.0", "2.5.0", "2.6.0"), shRelease(t, "v0.6.0", "2.4.0"))
	r := runInstallSh(t, f, nil)
	if r.err == nil {
		t.Fatalf("install.sh should fail:\n%s", r.stderr)
	}
	if !strings.Contains(r.stderr, "release v0.7.0 has no roksbnkargoctl_0.7.0_bnk-2.4.0_") ||
		!strings.Contains(r.stderr, "it has archives for BNK: 2.5.0 2.6.0") {
		t.Errorf("the error should name the missing archive and the lines present:\n%s", r.stderr)
	}
	if r.marker != "" {
		t.Errorf("a binary ran:\n%s", r.marker)
	}
}

func TestInstallShPinnedVersion(t *testing.T) {
	f := newFakeGitHub(t, shRelease(t, "v0.6.0", "2.4.0"), shRelease(t, "v0.5.0", "2.4.0"))
	for name, tc := range map[string]struct {
		env  []string
		args []string
	}{
		"VERSION env":    {env: []string{"VERSION=v0.5.0"}},
		"first argument": {args: []string{"0.5.0"}},
	} {
		t.Run(name, func(t *testing.T) {
			r := runInstallSh(t, f, tc.env, tc.args...)
			if r.err != nil {
				t.Fatalf("install.sh failed: %v\n%s", r.err, r.stderr)
			}
			if !strings.Contains(r.marker, "installed v0.5.0 bnk 2.4.0") {
				t.Errorf("pinned install got:\n%s", r.marker)
			}
		})
	}
}

func TestInstallShChecksumIsMandatory(t *testing.T) {
	asset := assetName("v0.6.0", "2.4.0", runtime.GOOS, runtime.GOARCH)
	for name, tc := range map[string]struct {
		mutate func(*fakeRelease)
		want   string // the refusal must be this one, not an incidental failure
	}{
		"mismatch": {func(r *fakeRelease) {
			r.sums = []byte(strings.Repeat("0", 64) + "  " + asset + "\n")
		}, "checksum mismatch for " + asset},
		"checksums file missing": {func(r *fakeRelease) { r.noSums = true },
			"has no roksbnkargoctl_0.6.0_checksums.txt; refusing to install without checksum verification"},
		"archive not listed": {func(r *fakeRelease) {
			r.sums = []byte(strings.Repeat("0", 64) + "  other_" + asset + "\n")
		}, "does not list " + asset},
	} {
		t.Run(name, func(t *testing.T) {
			rel := shRelease(t, "v0.6.0", "2.4.0")
			tc.mutate(&rel)
			r := runInstallSh(t, newFakeGitHub(t, rel), nil)
			if r.err == nil {
				t.Fatalf("install.sh accepted an unverified archive:\n%s", r.stderr)
			}
			if !strings.Contains(r.stderr, tc.want) {
				t.Errorf("want the refusal %q, got:\n%s", tc.want, r.stderr)
			}
			if r.marker != "" {
				t.Errorf("the unverified binary ran:\n%s", r.marker)
			}
		})
	}
}

func TestInstallShRejectsABadBNKVersion(t *testing.T) {
	f := newFakeGitHub(t, shRelease(t, "v0.6.0", "2.4.0"))
	r := runInstallSh(t, f, []string{"BNK_VERSION=2.4.0_linux_amd64.tar.gz*"})
	if r.err == nil || !strings.Contains(r.stderr, "is not a version") {
		t.Fatalf("want a refusal, got %v:\n%s", r.err, r.stderr)
	}
}

// Static checks: POSIX sh syntax, and shellcheck where it is installed.
func TestInstallShLints(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	if out, err := exec.Command("sh", "-n", "../../install.sh").CombinedOutput(); err != nil {
		t.Fatalf("sh -n: %v\n%s", err, out)
	}
	if _, err := exec.LookPath("shellcheck"); err != nil {
		t.Log("shellcheck not installed; skipped")
		return
	}
	if out, err := exec.Command("shellcheck", "-s", "sh", "../../install.sh").CombinedOutput(); err != nil {
		t.Fatalf("shellcheck: %v\n%s", err, out)
	}
}
