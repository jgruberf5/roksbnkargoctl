package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jgruberf5/roksbnkargoctl/internal/config"
)

func sshConfig(t *testing.T, knownHosts string) *config.Config {
	t.Helper()
	dir := t.TempDir()
	key := filepath.Join(dir, "id")
	if err := os.WriteFile(key, []byte("-----BEGIN OPENSSH PRIVATE KEY-----\nx\n-----END OPENSSH PRIVATE KEY-----\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := &config.Config{Git: config.Git{URL: "git@git.example.com:org/r.git", Branch: "main", Path: "bnk/x", SSHKeyFile: key}}
	if knownHosts != "" {
		kh := filepath.Join(dir, "known_hosts")
		_ = os.WriteFile(kh, []byte(knownHosts), 0o600)
		c.Git.KnownHostsFile = kh
	}
	return c
}

const exampleKH = "git.example.com ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl\n"

// Review findings M1/M5: the known_hosts file must reach the gitpub options that
// BOTH install and purge-git use (they now share gitOptions).
func TestGitOptionsCarriesKnownHosts(t *testing.T) {
	t.Setenv("ROKSBNKARGOCTL_GIT_INSECURE_HOSTKEY", "")
	o, err := gitOptions(sshConfig(t, exampleKH), func(string, ...any) {})
	if err != nil {
		t.Fatal(err)
	}
	if o.SSHKnownHosts != exampleKH || o.InsecureIgnoreHostKey || len(o.SSHKeyPEM) == 0 {
		t.Fatalf("options: knownHosts=%q insecure=%v key=%d bytes", o.SSHKnownHosts, o.InsecureIgnoreHostKey, len(o.SSHKeyPEM))
	}
}

// Review findings M6/M15: host-key checking is off ONLY with the explicit
// override, and switching it off always warns.
func TestGitOptionsInsecureOnlyWhenAskedAndWarned(t *testing.T) {
	var warned []string
	warn := func(f string, a ...any) { warned = append(warned, f) }
	t.Setenv("ROKSBNKARGOCTL_GIT_INSECURE_HOSTKEY", "")
	o, _ := gitOptions(sshConfig(t, ""), warn)
	if o.InsecureIgnoreHostKey || len(warned) != 0 {
		t.Fatalf("default must verify host keys silently: insecure=%v warnings=%v", o.InsecureIgnoreHostKey, warned)
	}
	t.Setenv("ROKSBNKARGOCTL_GIT_INSECURE_HOSTKEY", "1")
	o, _ = gitOptions(sshConfig(t, ""), warn)
	if !o.InsecureIgnoreHostKey || len(warned) != 1 || !strings.Contains(warned[0], "NOT verified") {
		t.Fatalf("override: insecure=%v warnings=%v", o.InsecureIgnoreHostKey, warned)
	}
}

// Review finding (low): a malformed known_hosts failed only at the Git push,
// after install had already changed the cluster and Argo CD.
func TestGitOptionsRejectsMalformedKnownHostsEarly(t *testing.T) {
	_, err := gitOptions(sshConfig(t, "git.example.com ssh-ed25519 not-base64!!\n"), func(string, ...any) {})
	if err == nil || !strings.Contains(err.Error(), "git.known_hosts_file") {
		t.Fatalf("want an early git.known_hosts_file error, got %v", err)
	}
}
