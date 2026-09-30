package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/jgruberf5/roksbnkargoctl/internal/agent"
)

// runRoot runs the real command tree and returns its combined output.
func runRoot(t *testing.T, args ...string) (string, error) {
	t.Helper()
	defer func() { flagWorkspace, flagYes = "", false }()
	root := newRoot()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(args)
	err := root.Execute()
	return out.String(), err
}

// fakeGitHubForHost is a fake GitHub carrying v0.6.0 for this host's platform,
// with githubAPI pointed at it for the test.
func fakeGitHubForHost(t *testing.T) *fakeGitHub {
	t.Helper()
	f := newFakeGitHub(t, release(t, "v0.6.0", []string{"2.4.0"}, runtime.GOOS+"/"+runtime.GOARCH))
	old := githubAPI
	githubAPI = f.URL
	t.Cleanup(func() { githubAPI = old })
	return f
}

// In the image, self update must refuse before touching GitHub or the binary:
// a download that then fails on the read-only, root-owned binary is the
// half-working behaviour this replaces. The refusal names the image tag to pull.
func TestSelfUpdateRefusesInTheImage(t *testing.T) {
	f := fakeGitHubForHost(t)
	t.Setenv(containerEnv, "1")
	for _, tc := range []struct {
		args []string
		pull string
	}{
		{[]string{"self", "update", "--yes"}, "docker pull " + CLIImageRepo + ":latest"},
		{[]string{"self", "update", "--version", "0.6.0"}, "docker pull " + CLIImageRepo + ":v0.6.0"},
	} {
		out, err := runRoot(t, tc.args...)
		if err == nil || !strings.Contains(err.Error(), tc.pull) {
			t.Errorf("%v in the image: err = %v, want it to say %q\n%s", tc.args, err, tc.pull, out)
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if n := len(f.apiAuthHdr) + len(f.downloadAuthHdr); n != 0 {
		t.Errorf("self update in the image made %d GitHub requests; it must refuse first", n)
	}
}

// --check still works in the image, and points at the image tag to pull rather
// than at `self update`, which would refuse.
func TestSelfUpdateCheckInTheImageSaysDockerPull(t *testing.T) {
	fakeGitHubForHost(t)
	t.Setenv(containerEnv, "1")
	out, err := runRoot(t, "self", "update", "--check")
	if err != nil {
		t.Fatalf("self update --check in the image: %v\n%s", err, out)
	}
	if !strings.Contains(out, "v0.6.0  (latest)") || !strings.Contains(out, "docker pull "+CLIImageRepo+":v0.6.0") {
		t.Errorf("check should list v0.6.0 and the image to pull:\n%s", out)
	}
	if strings.Contains(out, "Run `roksbnkargoctl self update`") {
		t.Errorf("check in the image suggested self update, which refuses there:\n%s", out)
	}
}

// Outside the image nothing changes: the marker must be exactly "1", and
// --check keeps suggesting self update.
func TestSelfUpdateCheckOnTheHostIsUnchanged(t *testing.T) {
	fakeGitHubForHost(t)
	for _, v := range []string{"", "0", "true"} {
		t.Setenv(containerEnv, v)
		out, err := runRoot(t, "self", "update", "--check")
		if err != nil {
			t.Fatalf("%s=%q: %v\n%s", containerEnv, v, err, out)
		}
		if !strings.Contains(out, "Run `roksbnkargoctl self update`") || strings.Contains(out, "docker pull") {
			t.Errorf("%s=%q is not the image, but check said:\n%s", containerEnv, v, out)
		}
	}
}

// self install in the image would copy the binary to a PATH inside a container
// that is about to exit; it refuses and writes nothing.
func TestSelfInstallRefusesInTheImage(t *testing.T) {
	t.Setenv(containerEnv, "1")
	dir := filepath.Join(t.TempDir(), "bin")
	out, err := runRoot(t, "self", "install", "--dir", dir)
	if err == nil || !strings.Contains(err.Error(), "self install does not work in the "+CLIImageRepo+" image") {
		t.Fatalf("self install in the image: err = %v\n%s", err, out)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("self install in the image touched %s (%v)", dir, err)
	}
}

// agentWorkspace makes a workspace "demo" under a fresh ROKSBNKARGOCTL_HOME.
func agentWorkspace(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	ws := filepath.Join(home, "demo")
	if err := os.MkdirAll(ws, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "config.yaml"), []byte("cluster: c\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ROKSBNKARGOCTL_HOME", home)
	return ws
}

// `agent <cli>` in the image says there is no agent CLI there, before
// scaffolding anything. Without the guard it scaffolds AGENTS.md and then fails
// on the terminal check, whose message also mentions --show: so the assertion
// is on the image wording and on AGENTS.md not being written.
func TestAgentRefusesInTheImage(t *testing.T) {
	ws := agentWorkspace(t)
	t.Setenv(containerEnv, "1")
	out, err := runRoot(t, "agent", "claude", "-w", "demo")
	if err == nil || !strings.Contains(err.Error(), "no agent CLIs are installed in the "+CLIImageRepo+" image") ||
		!strings.Contains(err.Error(), "agent claude --show") {
		t.Fatalf("agent claude in the image: err = %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(ws, "AGENTS.md")); err == nil {
		t.Error("agent in the image scaffolded AGENTS.md before refusing")
	}
}

// --show only prints, so it keeps working in the image.
func TestAgentShowWorksInTheImage(t *testing.T) {
	ws := agentWorkspace(t)
	t.Setenv(containerEnv, "1")
	out, err := runRoot(t, "agent", "claude", "--show", "-w", "demo")
	if err != nil {
		t.Fatalf("agent claude --show in the image: %v\n%s", err, out)
	}
	if !strings.HasPrefix(out, "cd "+agent.Show([]string{ws})+" && claude ") {
		t.Errorf("got %q", out)
	}
}

// The guards above are only live if the image sets the marker, and the image is
// only useful if it stamps the version the way releases do (checkImageTag picks
// the :vX.Y.Z check image from it). Pin both to build/cli/Dockerfile.
func TestCLIImageSetsTheMarkerAndStampsTheVersion(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "build", "cli", "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	// Join continuation lines, so ENV A=… \ B=… reads as one instruction.
	df := strings.ReplaceAll(string(b), "\\\n", " ")
	if !regexp.MustCompile(`(?m)^ENV\b.*\b` + containerEnv + `=1\b`).MatchString(df) {
		t.Errorf("build/cli/Dockerfile does not set ENV %s=1", containerEnv)
	}
	for _, v := range []string{"Version=${VERSION}", "Commit=${COMMIT}", "BuildDate=${DATE}"} {
		if !strings.Contains(df, "-X github.com/jgruberf5/roksbnkargoctl/internal/cli."+v) {
			t.Errorf("build/cli/Dockerfile ldflags do not set internal/cli.%s", v)
		}
	}
	if !strings.Contains(df, `ENTRYPOINT ["roksbnkargoctl"]`) {
		t.Error(`build/cli/Dockerfile has no ENTRYPOINT ["roksbnkargoctl"]`)
	}
	// A default on a platform ARG beats the value BuildKit sets per platform: with
	// ARG TARGETARCH=amd64 the linux/arm64 image was built with an x86-64 binary.
	if m := regexp.MustCompile(`(?m)^ARG\s+TARGET(OS|ARCH|PLATFORM)\s*=.*$`).FindString(df); m != "" {
		t.Errorf("build/cli/Dockerfile gives a platform ARG a default (%q): every image would get that platform's binary", m)
	}
	if !regexp.MustCompile(`\bGOOS=\$\{TARGETOS\}\s+GOARCH=\$\{TARGETARCH\}\s+go build\b`).MatchString(df) {
		t.Error("build/cli/Dockerfile does not build for GOOS=${TARGETOS} GOARCH=${TARGETARCH}")
	}
}
