package gitpub

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-git/go-billy/v5/memfs"
	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-git/v5/plumbing/transport/client"
	"github.com/go-git/go-git/v5/storage/memory"
)

// noGit makes any attempt to exec git fail: PATH points at an empty directory.
func noGit(t *testing.T) {
	t.Helper()
	t.Setenv("PATH", t.TempDir())
}

func bareRepo(t *testing.T) (dir, url string) {
	t.Helper()
	dir = filepath.Join(t.TempDir(), "remote.git")
	r, err := git.PlainInit(dir, true)
	if err != nil {
		t.Fatal(err)
	}
	// Like a hosted Git service, HEAD names the default branch "main".
	if err := r.Storer.SetReference(plumbing.NewSymbolicReference(plumbing.HEAD, plumbing.NewBranchReferenceName("main"))); err != nil {
		t.Fatal(err)
	}
	// file:// + an absolute path: "file:///tmp/x" on Unix, "file:///C:/x" on
	// Windows (a bare "file://C:/x" makes the drive letter the URL host).
	slashed := filepath.ToSlash(dir)
	if !strings.HasPrefix(slashed, "/") {
		slashed = "/" + slashed
	}
	return dir, "file://" + slashed
}

func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// seed pushes files straight to branch of the remote, independently of gitpub.
func seed(t *testing.T, url, branch string, files map[string]string) {
	t.Helper()
	fs := memfs.New()
	repo, err := git.Init(memory.NewStorage(), fs)
	if err != nil {
		t.Fatal(err)
	}
	ref := plumbing.NewBranchReferenceName(branch)
	if err := repo.Storer.SetReference(plumbing.NewSymbolicReference(plumbing.HEAD, ref)); err != nil {
		t.Fatal(err)
	}
	w, _ := repo.Worktree()
	for name, body := range files {
		f, err := fs.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = f.Write([]byte(body))
		f.Close()
		if _, err := w.Add(name); err != nil {
			t.Fatal(err)
		}
	}
	sig := &object.Signature{Name: "seed", Email: "seed@x", When: time.Now()}
	if _, err := w.Commit("seed", &git.CommitOptions{Author: sig}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.CreateRemote(&config.RemoteConfig{Name: "origin", URLs: []string{url}}); err != nil {
		t.Fatal(err)
	}
	if err := repo.Push(&git.PushOptions{RefSpecs: []config.RefSpec{config.RefSpec(ref + ":" + ref)}}); err != nil {
		t.Fatal(err)
	}
}

// remoteFiles returns path -> content at the tip of branch.
func remoteFiles(t *testing.T, url, branch string) (map[string]string, string) {
	t.Helper()
	repo, err := git.Clone(memory.NewStorage(), memfs.New(), &git.CloneOptions{
		URL: url, ReferenceName: plumbing.NewBranchReferenceName(branch), SingleBranch: true})
	if err != nil {
		t.Fatalf("clone for verification: %v", err)
	}
	head, _ := repo.Head()
	c, err := repo.CommitObject(head.Hash())
	if err != nil {
		t.Fatal(err)
	}
	tree, _ := c.Tree()
	out := map[string]string{}
	_ = tree.Files().ForEach(func(f *object.File) error {
		s, _ := f.Contents()
		out[f.Name] = s
		return nil
	})
	return out, head.Hash().String()
}

func keys(m map[string]string) string {
	var k []string
	for x := range m {
		k = append(k, x)
	}
	sort.Strings(k)
	return strings.Join(k, ",")
}

func TestPublishLifecycle(t *testing.T) {
	for _, cache := range []bool{false, true} {
		name := "memory"
		if cache {
			name = "cachedir"
		}
		t.Run(name, func(t *testing.T) {
			noGit(t)
			ctx := context.Background()
			_, url := bareRepo(t)
			seed(t, url, "main", map[string]string{"README.md": "hi\n", "other/keep.yaml": "keep: true\n", "clusters/demo-x/a.yaml": "sibling\n"})

			opts := Options{URL: url, Branch: "main", Path: "clusters/demo", Message: "publish demo"}
			if cache {
				opts.CacheDir = t.TempDir()
			}

			// 1. first publish creates a commit
			opts.SrcDir = writeTree(t, map[string]string{"00-ns.yaml": "ns\n", "sub/10-cm.yaml": "cm\n"})
			sha1, changed, err := Publish(ctx, opts)
			if err != nil || !changed || sha1 == "" {
				t.Fatalf("first publish: sha=%q changed=%v err=%v", sha1, changed, err)
			}
			files, head := remoteFiles(t, url, "main")
			if head != sha1 {
				t.Errorf("remote head %s != returned %s", head, sha1)
			}
			if got := keys(files); got != "README.md,clusters/demo-x/a.yaml,clusters/demo/00-ns.yaml,clusters/demo/sub/10-cm.yaml,other/keep.yaml" {
				t.Fatalf("files after first publish: %s", got)
			}

			// 2. identical publish is a no-op
			sha2, changed, err := Publish(ctx, opts)
			if err != nil || changed || sha2 != sha1 {
				t.Fatalf("identical publish: sha=%q changed=%v err=%v (want %s,false)", sha2, changed, err, sha1)
			}

			// 3. a removed file disappears, a modified one updates, outside Path is untouched
			opts.SrcDir = writeTree(t, map[string]string{"00-ns.yaml": "ns v2\n"})
			sha3, changed, err := Publish(ctx, opts)
			if err != nil || !changed || sha3 == sha1 {
				t.Fatalf("third publish: sha=%q changed=%v err=%v", sha3, changed, err)
			}
			files, _ = remoteFiles(t, url, "main")
			if got := keys(files); got != "README.md,clusters/demo-x/a.yaml,clusters/demo/00-ns.yaml,other/keep.yaml" {
				t.Fatalf("files after third publish: %s", got)
			}
			if files["clusters/demo/00-ns.yaml"] != "ns v2\n" || files["README.md"] != "hi\n" ||
				files["other/keep.yaml"] != "keep: true\n" || files["clusters/demo-x/a.yaml"] != "sibling\n" {
				t.Errorf("contents: %v", files)
			}

			// 4. Remove deletes Path only; a second Remove is a no-op
			_, changed, err = Remove(ctx, opts)
			if err != nil || !changed {
				t.Fatalf("remove: changed=%v err=%v", changed, err)
			}
			files, _ = remoteFiles(t, url, "main")
			if got := keys(files); got != "README.md,clusters/demo-x/a.yaml,other/keep.yaml" {
				t.Fatalf("files after remove: %s", got)
			}
			if _, changed, err = Remove(ctx, opts); err != nil || changed {
				t.Fatalf("second remove: changed=%v err=%v", changed, err)
			}
		})
	}
}

func TestPublishEmptyRepoAndNewBranch(t *testing.T) {
	noGit(t)
	ctx := context.Background()
	_, url := bareRepo(t)
	src := writeTree(t, map[string]string{"app.yaml": "x\n"})

	// empty remote: the branch is created
	sha, changed, err := Publish(ctx, Options{URL: url, Branch: "gitops", Path: "bnk", SrcDir: src})
	if err != nil || !changed || sha == "" {
		t.Fatalf("empty repo publish: sha=%q changed=%v err=%v", sha, changed, err)
	}
	files, _ := remoteFiles(t, url, "gitops")
	if keys(files) != "bnk/app.yaml" {
		t.Fatalf("files: %s", keys(files))
	}

	// non-empty remote, missing branch: created from the default HEAD, keeping its files
	_, url2 := bareRepo(t)
	seed(t, url2, "main", map[string]string{"README.md": "hi\n"})
	if _, changed, err := Publish(ctx, Options{URL: url2, Branch: "bnk", Path: "bnk", SrcDir: src}); err != nil || !changed {
		t.Fatalf("new branch publish: changed=%v err=%v", changed, err)
	}
	files, _ = remoteFiles(t, url2, "bnk")
	if keys(files) != "README.md,bnk/app.yaml" {
		t.Fatalf("new-branch files: %s", keys(files))
	}
	mainFiles, _ := remoteFiles(t, url2, "main")
	if keys(mainFiles) != "README.md" {
		t.Fatalf("main was modified: %s", keys(mainFiles))
	}
}

func TestPublishRepoRootKeepsGitDir(t *testing.T) {
	noGit(t)
	ctx := context.Background()
	_, url := bareRepo(t)
	seed(t, url, "main", map[string]string{"old.yaml": "old\n"})
	src := writeTree(t, map[string]string{"new.yaml": "new\n"})
	if _, changed, err := Publish(ctx, Options{URL: url, SrcDir: src, CacheDir: t.TempDir()}); err != nil || !changed {
		t.Fatalf("root publish: changed=%v err=%v", changed, err)
	}
	files, _ := remoteFiles(t, url, "main")
	if keys(files) != "new.yaml" {
		t.Fatalf("files: %s", keys(files))
	}
}

func TestCleanRepoPath(t *testing.T) {
	for in, want := range map[string]string{"": "", ".": "", "a/b/": "a/b", `a\b`: "a/b", "./a//b": "a/b"} {
		got, err := cleanRepoPath(in)
		if err != nil || got != want {
			t.Errorf("%q: got %q %v, want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"../x", "a/../../x", "/abs", ".git", "a/.git/hooks"} {
		if _, err := cleanRepoPath(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestAuthFor(t *testing.T) {
	if _, err := authFor(Options{URL: "ssh://git@example.com/org/repo.git"}); err == nil {
		t.Error("ssh without key accepted")
	}
	a, err := authFor(Options{URL: "https://example.com/org/repo.git", Token: "t"})
	if err != nil || a == nil || a.String() == "" {
		t.Fatalf("https auth: %v %v", a, err)
	}
	if !strings.Contains(a.String(), "git") {
		t.Errorf("default user: %s", a.String())
	}
}

func TestIsNonFastForward(t *testing.T) {
	if !isNonFastForward(git.ErrForceNeeded) || !isNonFastForward(errString("non-fast-forward update: refs/heads/main")) {
		t.Error("non-ff not detected")
	}
	if isNonFastForward(errString("authentication required")) {
		t.Error("auth error treated as non-ff")
	}
}

type errString string

func (e errString) Error() string { return string(e) }

// racyTransport pushes a competing commit to the remote the first time a push session
// opens, after gitpub has cloned, so gitpub's push is non-fast-forward.
type racyTransport struct {
	transport.Transport
	once  sync.Once
	races int
	hook  func()
}

func (r *racyTransport) NewReceivePackSession(ep *transport.Endpoint, a transport.AuthMethod) (transport.ReceivePackSession, error) {
	r.once.Do(func() { r.races++; r.hook() })
	return r.Transport.NewReceivePackSession(ep, a)
}

// commitOnto adds one file on top of branch at url, through a separate clone.
func commitOnto(t *testing.T, url, branch, name, body string) {
	t.Helper()
	fs := memfs.New()
	ref := plumbing.NewBranchReferenceName(branch)
	repo, err := git.Clone(memory.NewStorage(), fs, &git.CloneOptions{URL: url, ReferenceName: ref, SingleBranch: true})
	if err != nil {
		t.Fatal(err)
	}
	f, _ := fs.Create(name)
	_, _ = f.Write([]byte(body))
	f.Close()
	w, _ := repo.Worktree()
	if _, err := w.Add(name); err != nil {
		t.Fatal(err)
	}
	sig := &object.Signature{Name: "other", Email: "other@x", When: time.Now()}
	if _, err := w.Commit("concurrent", &git.CommitOptions{Author: sig}); err != nil {
		t.Fatal(err)
	}
	if err := repo.Push(&git.PushOptions{RefSpecs: []config.RefSpec{config.RefSpec(ref + ":" + ref)}}); err != nil {
		t.Fatal(err)
	}
}

func TestPublishRetriesNonFastForward(t *testing.T) {
	noGit(t)
	dir, url := bareRepo(t)
	seed(t, url, "main", map[string]string{"README.md": "hi\n"})
	rt := &racyTransport{Transport: client.Protocols["file"]}
	rt.hook = func() { commitOnto(t, url, "main", "concurrent.yaml", "c\n") }
	client.InstallProtocol("racy", rt)
	t.Cleanup(func() { client.InstallProtocol("racy", nil) })

	src := writeTree(t, map[string]string{"app.yaml": "x\n"})
	sha, changed, err := Publish(context.Background(), Options{URL: "racy://" + filepath.ToSlash(dir), Path: "bnk", SrcDir: src})
	if err != nil || !changed {
		t.Fatalf("publish: changed=%v err=%v", changed, err)
	}
	if rt.races != 1 {
		t.Fatalf("race hook ran %d times", rt.races)
	}
	files, head := remoteFiles(t, url, "main")
	if keys(files) != "README.md,bnk/app.yaml,concurrent.yaml" || head != sha {
		t.Fatalf("files %s head %s sha %s", keys(files), head, sha)
	}
}

// A new branch whose content already equals its base still has to exist afterwards, and
// Remove on a missing branch must not create it.
func TestNewBranchWithNoDiffIsCreated(t *testing.T) {
	noGit(t)
	ctx := context.Background()
	_, url := bareRepo(t)
	seed(t, url, "main", map[string]string{"bnk/app.yaml": "x\n"})
	o := Options{URL: url, Branch: "bnk", Path: "bnk", SrcDir: writeTree(t, map[string]string{"app.yaml": "x\n"})}
	if _, changed, err := Remove(ctx, o); err != nil || changed {
		t.Fatalf("remove on missing branch: changed=%v err=%v", changed, err)
	}
	if _, err := git.Clone(memory.NewStorage(), memfs.New(), &git.CloneOptions{URL: url,
		ReferenceName: plumbing.NewBranchReferenceName("bnk"), SingleBranch: true}); err == nil {
		t.Fatal("Remove created the branch")
	}
	sha, changed, err := Publish(ctx, o)
	if err != nil || !changed {
		t.Fatalf("publish: changed=%v err=%v", changed, err)
	}
	files, head := remoteFiles(t, url, "bnk")
	if keys(files) != "bnk/app.yaml" || head != sha {
		t.Fatalf("files %s head %s sha %s", keys(files), head, sha)
	}
}
