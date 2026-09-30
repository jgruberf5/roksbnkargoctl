package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/jgruberf5/roksbnkargoctl/internal/config"
	"github.com/jgruberf5/roksbnkargoctl/internal/gitpub"
)

func TestUsesTestHub(t *testing.T) {
	for _, tc := range []struct {
		server, cidr string
		want         bool
	}{
		{testHubPlaceholder, "", true},
		{"https://ARGOCD.PLACEHOLDER.INVALID:30443", "", true},
		{"", "10.248.1.0/28", true},
		{"", "", false},
		{"https://argocd.example.com", "10.248.1.0/28", false},
	} {
		c := &config.Config{ArgoCD: config.ArgoCD{Server: tc.server}, TestHub: config.TestHub{CIDR: tc.cidr}}
		if got := usesTestHub(c); got != tc.want {
			t.Errorf("server %q cidr %q: got %v", tc.server, tc.cidr, got)
		}
	}
}

// fakeArgo answers /api/version for anyone and userinfo loggedIn only for the
// good token, as Argo CD 3.5.1 was measured to.
func fakeArgo(t *testing.T) string {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/version":
			_ = json.NewEncoder(w).Encode(map[string]string{"Version": "v3.5.1"})
		case "/api/v1/session/userinfo":
			if r.Header.Get("Authorization") == "Bearer good" {
				_ = json.NewEncoder(w).Encode(map[string]any{"loggedIn": true})
			} else {
				_ = json.NewEncoder(w).Encode(map[string]any{})
			}
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func initSession(c *config.Config) (*session, *bytes.Buffer) {
	var out bytes.Buffer
	return &session{cfg: c, p: printer{&out}}, &out
}

// init refuses an existing Argo CD it cannot use: no token, or one Argo CD
// does not accept. A test-hub placeholder needs no token yet.
func TestCheckArgoCDAtInit(t *testing.T) {
	url := fakeArgo(t)
	c := &config.Config{ArgoCD: config.ArgoCD{Server: url, Insecure: true, TokenEnv: "ROKSBNKARGOCTL_TEST_ARGO_TOKEN"}}
	s, out := initSession(c)

	t.Setenv("ROKSBNKARGOCTL_TEST_ARGO_TOKEN", "")
	// The error names the variable AND says where a token comes from: the
	// Argo CD client alone would name only the variable.
	if err := checkArgoCDAtInit(context.Background(), s); err == nil || !strings.Contains(err.Error(), "ROKSBNKARGOCTL_TEST_ARGO_TOKEN") || !strings.Contains(err.Error(), "Generate New") {
		t.Fatalf("no token: %v", err)
	}
	t.Setenv("ROKSBNKARGOCTL_TEST_ARGO_TOKEN", "bad")
	if err := checkArgoCDAtInit(context.Background(), s); err == nil || !strings.Contains(err.Error(), "did not accept") {
		t.Fatalf("a rejected token: %v", err)
	}
	t.Setenv("ROKSBNKARGOCTL_TEST_ARGO_TOKEN", "good")
	if err := checkArgoCDAtInit(context.Background(), s); err != nil {
		t.Fatalf("a good token: %v", err)
	}
	if !strings.Contains(out.String(), "accepts the token") {
		t.Errorf("no confirmation: %s", out.String())
	}

	c.ArgoCD.Server = testHubPlaceholder
	t.Setenv("ROKSBNKARGOCTL_TEST_ARGO_TOKEN", "")
	if err := checkArgoCDAtInit(context.Background(), s); err != nil {
		t.Fatalf("a test-hub placeholder needs no token yet: %v", err)
	}
}

// Git at init: an unreadable repository is an error; a readable one passes.
func TestCheckGitAtInit(t *testing.T) {
	dir := t.TempDir()
	if _, err := git.PlainInit(dir, true); err != nil {
		t.Fatal(err)
	}
	c := &config.Config{Git: config.Git{URL: "file://" + strings.TrimPrefix(dir, "/"), Branch: "main", TokenEnv: "ROKSBNKARGOCTL_TEST_GIT_TOKEN"}}
	if !strings.HasPrefix(dir, "/") {
		t.Skip("file:// URL form below assumes a Unix path")
	}
	c.Git.URL = "file://" + dir
	t.Setenv("ROKSBNKARGOCTL_TEST_GIT_TOKEN", "tok")
	s, out := initSession(c)
	if err := checkGitAtInit(context.Background(), s); err != nil {
		t.Fatalf("a readable, pushable repo: %v\n%s", err, out.String())
	}
	c.Git.URL = "file://" + dir + "-missing"
	if err := checkGitAtInit(context.Background(), s); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("a missing repo: %v", err)
	}
	// No credential: only warnings, init goes on.
	t.Setenv("ROKSBNKARGOCTL_TEST_GIT_TOKEN", "")
	out.Reset()
	if err := checkGitAtInit(context.Background(), s); err != nil {
		t.Fatalf("no credential must only warn: %v", err)
	}
	if !strings.Contains(out.String(), "no Git credential") {
		t.Errorf("no warning: %s", out.String())
	}
}

// init runs the Argo CD / Git checks before it resolves or saves anything, and
// a failed check leaves no workspace behind.
func TestInitStopsOnAFailedRemoteCheck(t *testing.T) {
	home := isolate(t)
	src := home + "/../in.yaml"
	if err := writeFile(src, `
ibmcloud: {region: us-south}
cluster: c
transit_gateway: tgw
cos: {instance: ci, bucket: b}
argocd: {server: "https://argocd.example"}
git: {url: "https://git.example/r.git"}
`); err != nil {
		t.Fatal(err)
	}
	resolved := false
	resolveWorkspace = func(context.Context, *session) error { resolved = true; return nil }
	initRemoteChecks = func(context.Context, *session) error { return errRemoteCheckRan }
	t.Cleanup(func() { resolveWorkspace, initRemoteChecks = resolve, defaultInitRemoteChecks })
	_, err := runRoot(t, "init", "-w", "chk", "-f", src)
	if err != errRemoteCheckRan {
		t.Fatalf("init did not stop on the check: %v", err)
	}
	if resolved {
		t.Error("init resolved the workspace before checking Argo CD and Git")
	}
	if len(homeEntries(t, home)) != 0 {
		t.Errorf("a failed init left files: %v", homeEntries(t, home))
	}
}

var errRemoteCheckRan = &checkErr{}

type checkErr struct{}

func (*checkErr) Error() string { return "remote check ran" }

func writeFile(p, s string) error { return os.WriteFile(p, []byte(s), 0o600) }

// --no-publish syncs what is already there, so the branch must exist.
func TestCheckGitAccessNeedsTheBranchForNoPublish(t *testing.T) {
	dir := t.TempDir()
	if _, err := git.PlainInit(dir, true); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(dir, "/") {
		t.Skip("file:// URL form below assumes a Unix path")
	}
	c := &config.Config{Git: config.Git{URL: "file://" + dir, Branch: "main"}}
	s, _ := initSession(c)
	err := checkGitAccess(context.Background(), s.p, c, gitOptsFor(c), false, true)
	if err == nil || !strings.Contains(err.Error(), "has no branch main") {
		t.Fatalf("an empty repo with --no-publish: %v", err)
	}
}

func gitOptsFor(c *config.Config) gitpub.Options {
	return gitpub.Options{URL: c.Git.URL, Branch: c.Git.Branch}
}

// init's read-only check says what it saw: the branch present, the repo empty,
// or no such branch yet. It used to say "branch present" for all three.
func TestCheckGitAccessReportsWhatItSaw(t *testing.T) {
	populated := t.TempDir()
	r, err := git.PlainInit(populated, false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(populated, "/") {
		t.Skip("file:// URL form below assumes a Unix path")
	}
	if err := writeFile(filepath.Join(populated, "f"), "x"); err != nil {
		t.Fatal(err)
	}
	wt, _ := r.Worktree()
	if _, err := wt.Add("f"); err != nil {
		t.Fatal(err)
	}
	sig := &object.Signature{Name: "t", Email: "t@example.com", When: time.Unix(0, 0)}
	if _, err := wt.Commit("c", &git.CommitOptions{Author: sig}); err != nil {
		t.Fatal(err)
	}
	empty := t.TempDir()
	if _, err := git.PlainInit(empty, true); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ dir, branch, want string }{
		{populated, "master", "readable, branch master present"},
		{populated, "main", "readable; no branch main yet"},
		{empty, "main", "readable, and empty"},
	} {
		c := &config.Config{Git: config.Git{URL: "file://" + tc.dir, Branch: tc.branch}}
		s, out := initSession(c)
		if err := checkGitAccess(context.Background(), s.p, c, gitOptsFor(c), false, false); err != nil {
			t.Fatalf("%s %s: %v", tc.dir, tc.branch, err)
		}
		if !strings.Contains(out.String(), tc.want) {
			t.Errorf("branch %s: got %q, want %q", tc.branch, out.String(), tc.want)
		}
	}
}

type fakeBranches struct {
	branches []string
	err      error
}

func (f fakeBranches) RepositoryBranches(context.Context, string, string) ([]string, error) {
	return f.branches, f.err
}

// install --no-publish with no local credential asks Argo CD, which reads the
// repository with its own registration, instead of failing an anonymous read.
func TestCheckGitThroughArgoCD(t *testing.T) {
	c := &config.Config{Git: config.Git{URL: "git@example.com:o/r.git", Branch: "main"}}
	s, out := initSession(c)
	if err := checkGitThroughArgoCD(context.Background(), s.p, c, fakeBranches{branches: []string{"dev", "main"}}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Argo CD reads it, branch main present") {
		t.Errorf("output %q", out.String())
	}
	err := checkGitThroughArgoCD(context.Background(), s.p, c, fakeBranches{branches: []string{"dev"}})
	if err == nil || !strings.Contains(err.Error(), "has no branch main") {
		t.Errorf("missing branch: %v", err)
	}
	err = checkGitThroughArgoCD(context.Background(), s.p, c, fakeBranches{err: errors.New("authentication required")})
	if err == nil || !strings.Contains(err.Error(), "Argo CD cannot read") || !strings.Contains(err.Error(), "authentication required") {
		t.Errorf("unreadable: %v", err)
	}
}
