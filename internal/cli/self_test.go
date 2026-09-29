package cli

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/jgruberf5/roksbnkargoctl/internal/config"
)

// testUpdater points the real pipeline at a fake GitHub, as a linux/amd64
// binary for BNK 2.4.0 at version current.
func testUpdater(f *fakeGitHub, current string) (*updater, *bytes.Buffer) {
	var log bytes.Buffer
	return &updater{
		api: f.URL, repo: selfRepo, client: f.Client(),
		goos: "linux", goarch: "amd64", bnk: "2.4.0", current: current, w: &log,
	}, &log
}

// installedBinary writes an "old" binary to replace and returns its path.
func installedBinary(t *testing.T, name string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte("OLD"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

const linuxAMD = "linux/amd64"

// ---- release naming ---------------------------------------------------------------

// assetName and checksumsName must produce what goreleaser publishes. Pinned to
// the literal templates in .goreleaser.yaml, so a change to either side fails
// here instead of every self update failing to find its archive.
func TestAssetNamesMatchGoreleaser(t *testing.T) {
	if got, want := assetName("v0.5.0", "2.4.0", "linux", "amd64"), "roksbnkargoctl_0.5.0_bnk-2.4.0_linux_amd64.tar.gz"; got != want {
		t.Errorf("assetName = %q, want %q", got, want)
	}
	if got, want := assetName("v0.5.0", "2.4.0", "windows", "arm64"), "roksbnkargoctl_0.5.0_bnk-2.4.0_windows_arm64.zip"; got != want {
		t.Errorf("windows assetName = %q, want %q", got, want)
	}
	if got, want := checksumsName("v0.5.0"), "roksbnkargoctl_0.5.0_checksums.txt"; got != want {
		t.Errorf("checksumsName = %q, want %q", got, want)
	}

	b, err := os.ReadFile("../../.goreleaser.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`name_template: "{{ .ProjectName }}_{{ .Version }}_bnk-{{ .Env.BNK_VERSION }}_{{ .Os }}_{{ .Arch }}"`,
		`name_template: "{{ .ProjectName }}_{{ .Version }}_checksums.txt"`,
		`project_name: roksbnkargoctl`,
	} {
		if !bytes.Contains(b, []byte(want)) {
			t.Errorf(".goreleaser.yaml no longer has %s; assetName/checksumsName must follow it", want)
		}
	}
	if !regexp.MustCompile(`(?s)format_overrides:\s*- goos: windows\s*formats: \[zip\]`).Match(b) {
		t.Error(".goreleaser.yaml no longer ships .zip on Windows; assetName must follow it")
	}
}

// The binary this package builds asks for its own BNK version.
func TestNewUpdaterUsesTheBinarysBNKVersion(t *testing.T) {
	if u := newUpdater(&bytes.Buffer{}); u.bnk != config.BNKVersion {
		t.Errorf("updater asks for BNK %q, but this binary installs %q", u.bnk, config.BNKVersion)
	}
}

func TestNormalizeTag(t *testing.T) {
	for in, want := range map[string]string{"": "", "  ": "", "0.5.1": "v0.5.1", "v0.5.1": "v0.5.1", " 0.5.1 ": "v0.5.1"} {
		if got := normalizeTag(in); got != want {
			t.Errorf("normalizeTag(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSameVersion(t *testing.T) {
	if !sameVersion("0.5.0", "v0.5.0") || !sameVersion("v0.5.0", "0.5.0") {
		t.Error("sameVersion must ignore a leading v")
	}
	if sameVersion("0.5.0", "v0.5.1") {
		t.Error("distinct versions must not compare equal")
	}
}

func TestCompareSemver(t *testing.T) {
	order := []string{"v0.5.0-rc1", "v0.5.0", "v0.5.1", "v0.9.0", "v0.10.0", "v1.0.0"}
	for i := 0; i < len(order)-1; i++ {
		a, _ := parseSemver(order[i])
		b, _ := parseSemver(order[i+1])
		if compareSemver(a, b) >= 0 || compareSemver(b, a) <= 0 {
			t.Errorf("%s must sort below %s", order[i], order[i+1])
		}
	}
	for _, bad := range []string{"dev", "v1.2", "v1.2.x", ""} {
		if _, ok := parseSemver(bad); ok {
			t.Errorf("parseSemver(%q) should fail", bad)
		}
	}
}

// ---- asset selection by BNK version ----------------------------------------------------

// "Latest" is the newest release that has an archive for this BNK line. A newer
// release cut only for another line must be skipped, not installed and not
// treated as "no update".
func TestUpdateSkipsANewerReleaseWithoutThisBNKLine(t *testing.T) {
	f := newFakeGitHub(t,
		release(t, "v0.7.0", []string{"2.5.0"}, linuxAMD),          // another BNK line only
		release(t, "v0.6.0", []string{"2.4.0", "2.5.0"}, linuxAMD), // both lines
		release(t, "v0.5.0", []string{"2.4.0"}, linuxAMD),
	)
	u, log := testUpdater(f, "v0.5.0")
	target := installedBinary(t, selfBinary)

	if err := u.run(t.Context(), target, "", true); err != nil {
		t.Fatalf("run: %v\n%s", err, log)
	}
	if got, want := readFile(t, target), string(binaryFor("v0.6.0", "2.4.0")); got != want {
		t.Errorf("installed %q, want %q", got, want)
	}
}

// A pinned release that only has another BNK line's archive is refused, the
// installed binary is untouched, and the error names the lines it does have.
func TestPinnedReleaseWithOnlyAnotherBNKLineIsRefused(t *testing.T) {
	f := newFakeGitHub(t, release(t, "v0.7.0", []string{"2.5.0", "2.6.0"}, linuxAMD))
	u, _ := testUpdater(f, "v0.5.0")
	target := installedBinary(t, selfBinary)

	err := u.run(t.Context(), target, "0.7.0", true)
	if err == nil {
		t.Fatal("a release with no archive for BNK 2.4.0 must be refused")
	}
	for _, want := range []string{"no archive for BNK 2.4.0", "2.5.0, 2.6.0", "never switches BNK lines"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should contain %q:\n%s", want, err)
		}
	}
	if got := readFile(t, target); got != "OLD" {
		t.Errorf("the installed binary changed to %q", got)
	}
}

// A release that has this BNK line but not this platform says so, rather than
// claiming the BNK line is missing.
func TestPinnedReleaseMissingOnlyThisPlatform(t *testing.T) {
	f := newFakeGitHub(t, release(t, "v0.6.0", []string{"2.4.0"}, "darwin/arm64"))
	u, _ := testUpdater(f, "v0.5.0")
	err := u.run(t.Context(), installedBinary(t, selfBinary), "v0.6.0", true)
	if err == nil || !strings.Contains(err.Error(), "none for linux/amd64") {
		t.Fatalf("want a missing-platform error, got %v", err)
	}
}

func TestNoReleaseForThisBNKLine(t *testing.T) {
	f := newFakeGitHub(t, release(t, "v0.7.0", []string{"2.5.0"}, linuxAMD))
	u, _ := testUpdater(f, "v0.5.0")
	target := installedBinary(t, selfBinary)
	err := u.run(t.Context(), target, "", true)
	if err == nil || !strings.Contains(err.Error(), "no release of "+selfRepo+" has an archive for BNK 2.4.0") {
		t.Fatalf("want a no-release-for-this-line error, got %v", err)
	}
	if got := readFile(t, target); got != "OLD" {
		t.Errorf("the installed binary changed to %q", got)
	}
}

// Drafts and prereleases are not "latest"; --version still installs them.
func TestLatestSkipsPrereleasesAndDrafts(t *testing.T) {
	pre := release(t, "v0.8.0-rc1", []string{"2.4.0"}, linuxAMD)
	pre.pre = true
	draft := release(t, "v0.8.0", []string{"2.4.0"}, linuxAMD)
	draft.draft = true
	f := newFakeGitHub(t, draft, pre, release(t, "v0.6.0", []string{"2.4.0"}, linuxAMD))
	u, _ := testUpdater(f, "v0.5.0")
	target := installedBinary(t, selfBinary)
	if err := u.run(t.Context(), target, "", true); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, target); got != string(binaryFor("v0.6.0", "2.4.0")) {
		t.Errorf("installed %q, want v0.6.0", got)
	}

	if err := u.run(t.Context(), target, "v0.8.0-rc1", true); err != nil {
		t.Fatalf("a pinned prerelease should install: %v", err)
	}
	if got := readFile(t, target); got != string(binaryFor("v0.8.0-rc1", "2.4.0")) {
		t.Errorf("installed %q, want v0.8.0-rc1", got)
	}
}

// Newest by version, not by list position: GitHub lists by creation date, and
// a patch to an older line published later must not win.
func TestLatestIsTheHighestVersion(t *testing.T) {
	f := newFakeGitHub(t,
		release(t, "v0.5.9", []string{"2.4.0"}, linuxAMD), // created last
		release(t, "v0.6.0", []string{"2.4.0"}, linuxAMD),
	)
	u, _ := testUpdater(f, "v0.5.0")
	target := installedBinary(t, selfBinary)
	if err := u.run(t.Context(), target, "", true); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, target); got != string(binaryFor("v0.6.0", "2.4.0")) {
		t.Errorf("installed %q, want v0.6.0", got)
	}
}

func TestAlreadyAtLatestChangesNothing(t *testing.T) {
	f := newFakeGitHub(t, release(t, "v0.6.0", []string{"2.4.0"}, linuxAMD))
	u, log := testUpdater(f, "0.6.0")
	target := installedBinary(t, selfBinary)
	if err := u.run(t.Context(), target, "", true); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, target); got != "OLD" {
		t.Errorf("an up-to-date binary was replaced with %q", got)
	}
	if !strings.Contains(log.String(), "Already at the latest release for BNK 2.4.0") {
		t.Errorf("log should say it is up to date:\n%s", log)
	}
}

func TestCheckReportsWithoutChanging(t *testing.T) {
	f := newFakeGitHub(t,
		release(t, "v0.7.0", []string{"2.5.0"}, linuxAMD),
		release(t, "v0.6.0", []string{"2.4.0"}, linuxAMD),
		release(t, "v0.5.0", []string{"2.4.0"}, linuxAMD),
	)
	u, _ := testUpdater(f, "v0.5.0")
	var out bytes.Buffer
	if err := u.check(t.Context(), &out); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	if !strings.Contains(s, "v0.6.0  (latest)") {
		t.Errorf("check should offer v0.6.0 as latest:\n%s", s)
	}
	if strings.Contains(s, "v0.7.0") || strings.Contains(s, "  v0.5.0") {
		t.Errorf("check listed a release it should not (another BNK line, or not newer):\n%s", s)
	}
}

// ---- checksum ---------------------------------------------------------------------

func TestChecksumMismatchRefuses(t *testing.T) {
	rel := release(t, "v0.6.0", []string{"2.4.0"}, linuxAMD)
	name := assetName("v0.6.0", "2.4.0", "linux", "amd64")
	rel.sums = []byte(strings.Repeat("0", 64) + "  " + name + "\n")
	f := newFakeGitHub(t, rel)
	u, _ := testUpdater(f, "v0.5.0")
	target := installedBinary(t, selfBinary)

	err := u.run(t.Context(), target, "", true)
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("want a checksum mismatch, got %v", err)
	}
	if got := readFile(t, target); got != "OLD" {
		t.Errorf("a binary that failed verification was installed: %q", got)
	}
}

func TestMissingChecksumsFileRefuses(t *testing.T) {
	rel := release(t, "v0.6.0", []string{"2.4.0"}, linuxAMD)
	rel.noSums = true
	f := newFakeGitHub(t, rel)
	u, _ := testUpdater(f, "v0.5.0")
	target := installedBinary(t, selfBinary)
	err := u.run(t.Context(), target, "", true)
	if err == nil || !strings.Contains(err.Error(), "refusing to update without checksum verification") {
		t.Fatalf("want a refusal, got %v", err)
	}
	if got := readFile(t, target); got != "OLD" {
		t.Errorf("an unverified binary was installed: %q", got)
	}
}

func TestChecksumsNotListingTheArchiveRefuses(t *testing.T) {
	rel := release(t, "v0.6.0", []string{"2.4.0"}, linuxAMD)
	rel.sums = []byte(strings.Repeat("a", 64) + "  something-else.tar.gz\n")
	f := newFakeGitHub(t, rel)
	u, _ := testUpdater(f, "v0.5.0")
	target := installedBinary(t, selfBinary)
	err := u.run(t.Context(), target, "", true)
	if err == nil || !strings.Contains(err.Error(), "does not list") {
		t.Fatalf("want a refusal, got %v", err)
	}
	if got := readFile(t, target); got != "OLD" {
		t.Errorf("an unverified binary was installed: %q", got)
	}
}

// ---- Windows: zip + move-aside, from any host ---------------------------------------------

func TestWindowsUpdateUsesTheZipAndMovesAside(t *testing.T) {
	f := newFakeGitHub(t, release(t, "v0.6.0", []string{"2.4.0"}, "windows/amd64", linuxAMD))
	u, _ := testUpdater(f, "v0.5.0")
	u.goos = "windows"
	target := installedBinary(t, selfBinary+".exe")
	windowsLock(t, target)

	if err := u.run(t.Context(), target, "", true); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, target); got != string(binaryFor("v0.6.0", "2.4.0")) {
		t.Errorf("installed %q", got)
	}
	// Unlocked here, so the sidecar is removed at once; on Windows it waits
	// for the next run's sweep.
	if _, err := os.Stat(target + ".old"); !os.IsNotExist(err) {
		t.Errorf("the .old sidecar should be gone: %v", err)
	}
}

// A replace that cannot roll back exits 125, so a wrapper can tell "retry" from
// "there is no binary left to retry with".
func TestStrandedUpdateExitsWithItsOwnCode(t *testing.T) {
	f := newFakeGitHub(t, release(t, "v0.6.0", []string{"2.4.0"}, "windows/amd64"))
	u, _ := testUpdater(f, "v0.5.0")
	u.goos = "windows"
	target := installedBinary(t, selfBinary+".exe")

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

	err := u.run(t.Context(), target, "", true)
	var ee exitError
	if !errors.As(err, &ee) || ee.code != exitSelfUpdateStranded {
		t.Fatalf("want exit code %d, got %v", exitSelfUpdateStranded, err)
	}
	if !strings.Contains(err.Error(), "NO binary") {
		t.Errorf("the error must say no binary remains:\n%s", err)
	}
}

// ---- GitHub API errors --------------------------------------------------------------

func TestRateLimitIsNamed(t *testing.T) {
	f := newFakeGitHub(t, release(t, "v0.6.0", []string{"2.4.0"}, linuxAMD))
	f.mu.Lock()
	f.rateLimited = true
	f.mu.Unlock()
	u, _ := testUpdater(f, "v0.5.0")
	err := u.run(t.Context(), installedBinary(t, selfBinary), "", true)
	if err == nil || !strings.Contains(err.Error(), "rate limit exhausted; set GITHUB_TOKEN") {
		t.Fatalf("want the rate-limit message, got %v", err)
	}
}

func TestUnknownPinnedReleaseIsNamed(t *testing.T) {
	f := newFakeGitHub(t, release(t, "v0.6.0", []string{"2.4.0"}, linuxAMD))
	u, _ := testUpdater(f, "v0.5.0")
	err := u.run(t.Context(), installedBinary(t, selfBinary), "v9.9.9", true)
	if err == nil || !strings.Contains(err.Error(), "release v9.9.9 not found") {
		t.Fatalf("want a not-found message, got %v", err)
	}
}

// GITHUB_TOKEN authenticates API calls but must not follow the asset download
// to the storage host it redirects to.
func TestTokenIsNotSentWithDownloads(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "secret-token")
	f := newFakeGitHub(t, release(t, "v0.6.0", []string{"2.4.0"}, linuxAMD))
	u, _ := testUpdater(f, "v0.5.0")
	if err := u.run(t.Context(), installedBinary(t, selfBinary), "", true); err != nil {
		t.Fatal(err)
	}
	seen := f.downloadAuth()
	if len(seen) == 0 {
		t.Fatal("no downloads were made")
	}
	for _, h := range seen {
		if h != "" {
			t.Errorf("a download carried Authorization %q", h)
		}
	}
}

// ---- extraction -----------------------------------------------------------------------

func TestExtractFromTarGz(t *testing.T) {
	a := tarGzOf(t, map[string][]byte{"LICENSE": []byte("MIT"), selfBinary: []byte("ELF")})
	got, err := extractBinary("linux", a)
	if err != nil || string(got) != "ELF" {
		t.Fatalf("extract = %q, %v", got, err)
	}
	if _, err := extractFromTarGz(a, "nope"); err == nil {
		t.Error("a missing binary must be an error")
	}
}

func TestExtractFromZip(t *testing.T) {
	a := zipOf(t, map[string][]byte{"README.md": []byte("readme"), selfBinary + ".exe": []byte("MZ")})
	got, err := extractBinary("windows", a)
	if err != nil || string(got) != "MZ" {
		t.Fatalf("extract = %q, %v", got, err)
	}
	// The unix name is not the Windows binary.
	if _, err := extractFromZip(a, selfBinary); err == nil {
		t.Error("a missing binary must be an error")
	}
}

// ---- replace ------------------------------------------------------------------------

func TestReplaceBinaryUnix(t *testing.T) {
	target := installedBinary(t, selfBinary)
	if err := replaceBinary("linux", target, []byte("NEW")); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, target); got != "NEW" {
		t.Errorf("target = %q", got)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o100 == 0 { // no exec bit on Windows
		t.Errorf("installed binary is not executable: %v", info.Mode())
	}
	entries, _ := os.ReadDir(filepath.Dir(target))
	if len(entries) != 1 {
		t.Errorf("stray files left beside the target: %d entries", len(entries))
	}
}

func TestInstallByMoveAside(t *testing.T) {
	target := installedBinary(t, selfBinary+".exe")
	staged := filepath.Join(filepath.Dir(target), ".staged")
	if err := os.WriteFile(staged, []byte("NEW"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := installByMoveAside(target, staged); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, target); got != "NEW" {
		t.Errorf("target = %q", got)
	}
	if _, err := os.Stat(staged); !os.IsNotExist(err) {
		t.Error("the staged file should have been renamed away")
	}
}
