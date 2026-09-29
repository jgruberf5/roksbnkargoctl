package gitpub

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"net"
	"strings"
	"testing"

	gitssh "github.com/go-git/go-git/v5/plumbing/transport/ssh"
	"golang.org/x/crypto/ssh"
)

func clientKeyPEM(t *testing.T) []byte {
	t.Helper()
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	blk, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(blk)
}

// github.com's real ed25519 host key (from BuiltinKnownHosts).
func githubKey(t *testing.T) ssh.PublicKey {
	t.Helper()
	for _, l := range strings.Split(BuiltinKnownHosts, "\n") {
		f := strings.Fields(l)
		if len(f) == 3 && f[0] == "github.com" && f[1] == "ssh-ed25519" {
			k, _, _, _, err := ssh.ParseAuthorizedKey([]byte(f[1] + " " + f[2]))
			if err != nil {
				t.Fatal(err)
			}
			return k
		}
	}
	t.Fatal("no github.com ed25519 key in BuiltinKnownHosts")
	return nil
}

func hostKeyCallback(t *testing.T, o Options) ssh.HostKeyCallback {
	t.Helper()
	o.SSHKeyPEM = clientKeyPEM(t)
	a, err := authFor(o)
	if err != nil {
		t.Fatal(err)
	}
	return a.(*gitssh.PublicKeys).HostKeyCallback
}

var addr = &net.TCPAddr{IP: net.IPv4(140, 82, 112, 3), Port: 22}

// Issue #2: an SSH repo on a public host must verify its host key with no
// configuration and no insecure override.
func TestBuiltinKnownHostsVerifyGitHub(t *testing.T) {
	cb := hostKeyCallback(t, Options{URL: "git@github.com:jgruberf5/x.git"})
	if err := cb("github.com:22", addr, githubKey(t)); err != nil {
		t.Fatalf("github.com's published key was rejected: %v", err)
	}
}

// A different key for github.com is a man-in-the-middle signal, not "unknown".
func TestHostKeyMismatchIsLoud(t *testing.T) {
	cb := hostKeyCallback(t, Options{URL: "git@github.com:jgruberf5/x.git"})
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	forged, _ := ssh.NewPublicKey(pub)
	err := cb("github.com:22", addr, forged)
	if err == nil || !strings.Contains(err.Error(), "MISMATCH") {
		t.Fatalf("a forged github.com key must fail as a mismatch, got %v", err)
	}
}

// An unknown host fails with the setting that fixes it; a known_hosts file adds
// hosts without dropping the built-in ones.
func TestUnknownHostNamesTheSetting(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	key, _ := ssh.NewPublicKey(pub)
	cb := hostKeyCallback(t, Options{URL: "git@git.example.com:org/x.git"})
	err := cb("git.example.com:22", addr, key)
	if err == nil || !strings.Contains(err.Error(), "git.known_hosts_file") {
		t.Fatalf("unknown host must name git.known_hosts_file, got %v", err)
	}
	line := "git.example.com " + strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key)))
	cb = hostKeyCallback(t, Options{URL: "git@git.example.com:org/x.git", SSHKnownHosts: line})
	if err := cb("git.example.com:22", addr, key); err != nil {
		t.Fatalf("host from SSHKnownHosts rejected: %v", err)
	}
	if err := cb("github.com:22", addr, githubKey(t)); err != nil {
		t.Fatalf("an operator known_hosts must not drop the built-in hosts: %v", err)
	}
}
