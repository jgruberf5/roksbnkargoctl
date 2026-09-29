package config

import (
	"os"
	"regexp"
	"testing"
)

// The release archive is named for the BNK version the binary installs
// (roksbnkargoctl_<v>_bnk-<BNK>_<os>_<arch>). If .goreleaser.yaml's BNK_VERSION
// drifted from BNKVersion, an asset would advertise one BNK release and install
// another.
func TestReleaseNamesTheBNKVersion(t *testing.T) {
	b, err := os.ReadFile("../../.goreleaser.yaml")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^\s*-\s*BNK_VERSION=(\S+)\s*$`).FindSubmatch(b)
	if m == nil {
		t.Fatal(".goreleaser.yaml sets no BNK_VERSION")
	}
	if string(m[1]) != BNKVersion {
		t.Fatalf(".goreleaser.yaml BNK_VERSION=%s, but this build installs BNK %s", m[1], BNKVersion)
	}
	if !regexp.MustCompile(`name_template: .*_bnk-\{\{ \.Env\.BNK_VERSION \}\}_`).Match(b) {
		t.Fatal("the archive name does not carry the BNK version")
	}
}

// v0.5.0 was published with empty release notes: changelog.disable skips the
// pipe that loads --release-notes, which the workflow relies on.
func TestReleaseNotesAreNotDisabled(t *testing.T) {
	g, err := os.ReadFile("../../.goreleaser.yaml")
	if err != nil {
		t.Fatal(err)
	}
	w, err := os.ReadFile("../../.github/workflows/release.yml")
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`--release-notes`).Match(w) {
		t.Fatal("release.yml no longer passes --release-notes")
	}
	if regexp.MustCompile(`(?m)^changelog:\s*\n(\s+.*\n)*?\s+disable:\s*true`).Match(g) {
		t.Fatal(".goreleaser.yaml disables the changelog pipe, which drops --release-notes")
	}
}
