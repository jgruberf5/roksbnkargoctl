// Package gitpub publishes a directory tree into a path of a Git repository, in-process
// with go-git: clone, replace the path exactly, commit only if something changed, push.
//
// No git binary is ever run. go-git's own file:// transport shells out to
// git-upload-pack/git-receive-pack, so this package replaces it (see init) with go-git's
// in-process server, which makes file:// URLs and local paths work on hosts without git.
package gitpub

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-git/go-billy/v5"
	"github.com/go-git/go-billy/v5/memfs"
	"github.com/go-git/go-billy/v5/osfs"
	"github.com/go-git/go-billy/v5/util"
	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/cache"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/storer"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-git/v5/plumbing/transport/client"
	"github.com/go-git/go-git/v5/plumbing/transport/http"
	"github.com/go-git/go-git/v5/plumbing/transport/server"
	gitssh "github.com/go-git/go-git/v5/plumbing/transport/ssh"
	"github.com/go-git/go-git/v5/storage"
	"github.com/go-git/go-git/v5/storage/filesystem"
	"github.com/go-git/go-git/v5/storage/memory"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

func init() {
	client.InstallProtocol("file", server.NewServer(localLoader{}))
}

// Options describes one publish.
type Options struct {
	URL    string // https://..., ssh://git@host/org/repo.git, git@host:org/repo.git, or file://
	Branch string // default "main"
	Path   string // directory inside the repo that SrcDir replaces, e.g. "clusters/demo"

	Username string // HTTPS basic auth user (default "git" when only a token is given)
	Token    string // HTTPS token or password

	SSHKeyPEM             []byte // private key for ssh URLs
	SSHKeyPassphrase      string
	SSHKnownHosts         string // known_hosts content; required for ssh unless InsecureIgnoreHostKey
	InsecureIgnoreHostKey bool

	CABundle []byte // extra CA for HTTPS

	AuthorName  string // default "roksbnkargoctl"
	AuthorEmail string // default "roksbnkargoctl@localhost"
	Message     string // commit message

	SrcDir   string // local directory whose contents become Path
	CacheDir string // optional: clone on disk here instead of in memory (wiped on each run)
}

// Publish replaces Path in the repository with the contents of SrcDir and pushes. It
// returns the branch head after the operation and whether a commit was made. A
// non-fast-forward push (someone pushed meanwhile) is retried once from a fresh clone.
func Publish(ctx context.Context, o Options) (sha string, changed bool, err error) {
	if o.SrcDir == "" {
		return "", false, errors.New("gitpub: SrcDir is required")
	}
	if st, err := os.Stat(o.SrcDir); err != nil || !st.IsDir() {
		return "", false, fmt.Errorf("gitpub: SrcDir %q is not a directory", o.SrcDir)
	}
	return run(ctx, o, true, func(wt billy.Filesystem, p string) error {
		if err := clearPath(wt, p); err != nil {
			return err
		}
		return copyTree(o.SrcDir, wt, p)
	})
}

// Remove deletes Path from the repository and pushes. changed is false when Path was
// already absent.
func Remove(ctx context.Context, o Options) (sha string, changed bool, err error) {
	return run(ctx, o, false, func(wt billy.Filesystem, p string) error { return clearPath(wt, p) })
}

func run(ctx context.Context, o Options, publish bool, mutate func(billy.Filesystem, string) error) (string, bool, error) {
	if o.URL == "" {
		return "", false, errors.New("gitpub: URL is required")
	}
	if o.Branch == "" {
		o.Branch = "main"
	}
	p, err := cleanRepoPath(o.Path)
	if err != nil {
		return "", false, err
	}
	auth, err := authFor(o)
	if err != nil {
		return "", false, err
	}
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		sha, changed, err := once(ctx, o, p, auth, publish, mutate)
		if err == nil {
			return sha, changed, nil
		}
		lastErr = err
		if !isNonFastForward(err) || ctx.Err() != nil {
			break
		}
	}
	return "", false, lastErr
}

func isNonFastForward(err error) bool {
	if errors.Is(err, git.ErrForceNeeded) || errors.Is(err, git.ErrNonFastForwardUpdate) {
		return true
	}
	s := err.Error()
	return strings.Contains(s, "non-fast-forward") || strings.Contains(s, "fetch first")
}

func once(ctx context.Context, o Options, p string, auth transport.AuthMethod, publish bool, mutate func(billy.Filesystem, string) error) (string, bool, error) {
	st, wt, err := newStorage(o.CacheDir)
	if err != nil {
		return "", false, err
	}
	branch := plumbing.NewBranchReferenceName(o.Branch)
	hasBranch, hasHead, err := probe(ctx, o, auth, branch)
	if err != nil {
		return "", false, err
	}
	var repo *git.Repository
	switch {
	case hasBranch:
		repo, err = git.CloneContext(ctx, st, wt, &git.CloneOptions{
			URL: o.URL, Auth: auth, CABundle: o.CABundle,
			ReferenceName: branch, SingleBranch: true, Tags: git.NoTags,
		})
		if err != nil {
			return "", false, fmt.Errorf("gitpub: clone %s (branch %s): %w", o.URL, o.Branch, err)
		}
	case hasHead && !publish:
		return "", false, nil // Remove: the branch does not exist, so neither does Path
	case hasHead:
		// The repo has commits but not this branch: start the branch from the default HEAD.
		repo, err = git.CloneContext(ctx, st, wt, &git.CloneOptions{
			URL: o.URL, Auth: auth, CABundle: o.CABundle, SingleBranch: true, Tags: git.NoTags,
		})
		if err != nil {
			return "", false, fmt.Errorf("gitpub: clone %s: %w", o.URL, err)
		}
		w, err := repo.Worktree()
		if err != nil {
			return "", false, err
		}
		if err := w.Checkout(&git.CheckoutOptions{Branch: branch, Create: true}); err != nil {
			return "", false, fmt.Errorf("gitpub: create branch %s: %w", o.Branch, err)
		}
	default:
		// Empty repository (or no resolvable HEAD): the branch starts with no parent.
		if repo, err = initEmpty(o, branch, st, wt); err != nil {
			return "", false, err
		}
	}

	w, err := repo.Worktree()
	if err != nil {
		return "", false, err
	}
	if err := mutate(wt, p); err != nil {
		return "", false, err
	}
	if err := stageAll(w); err != nil {
		return "", false, err
	}
	status, err := w.Status()
	if err != nil {
		return "", false, fmt.Errorf("gitpub: status: %w", err)
	}
	if status.IsClean() {
		head, err := repo.Head()
		if err != nil {
			return "", false, nil // empty repo and nothing to publish
		}
		if hasBranch || !hasHead {
			return head.Hash().String(), false, nil
		}
		// A new branch whose content already matches its base: no commit, but the
		// branch must still be created for Argo CD to find it.
		if err := push(ctx, repo, o, branch, auth); err != nil {
			return "", false, err
		}
		return head.Hash().String(), true, nil
	}

	msg := o.Message
	if msg == "" {
		msg = "roksbnkargoctl: publish " + p
	}
	sig := &object.Signature{Name: orDefault(o.AuthorName, "roksbnkargoctl"),
		Email: orDefault(o.AuthorEmail, "roksbnkargoctl@localhost"), When: time.Now()}
	h, err := w.Commit(msg, &git.CommitOptions{Author: sig, Committer: sig})
	if err != nil {
		return "", false, fmt.Errorf("gitpub: commit: %w", err)
	}
	if err := push(ctx, repo, o, branch, auth); err != nil {
		return "", false, err
	}
	return h.String(), true, nil
}

func push(ctx context.Context, repo *git.Repository, o Options, branch plumbing.ReferenceName, auth transport.AuthMethod) error {
	spec := config.RefSpec(branch.String() + ":" + branch.String())
	err := repo.PushContext(ctx, &git.PushOptions{
		RemoteName: git.DefaultRemoteName, RefSpecs: []config.RefSpec{spec},
		Auth: auth, CABundle: o.CABundle,
	})
	if err != nil && !errors.Is(err, git.NoErrAlreadyUpToDate) {
		return fmt.Errorf("gitpub: push %s to %s: %w", o.Branch, o.URL, err)
	}
	return nil
}

// probe lists the remote's refs (ls-remote) and reports whether branch exists and
// whether HEAD resolves to a commit. An empty repository reports neither.
func probe(ctx context.Context, o Options, auth transport.AuthMethod, branch plumbing.ReferenceName) (hasBranch, hasHead bool, err error) {
	rem := git.NewRemote(memory.NewStorage(), &config.RemoteConfig{Name: git.DefaultRemoteName, URLs: []string{o.URL}})
	refs, err := rem.ListContext(ctx, &git.ListOptions{Auth: auth, CABundle: o.CABundle})
	if errors.Is(err, transport.ErrEmptyRemoteRepository) {
		return false, false, nil
	}
	if err != nil {
		return false, false, fmt.Errorf("gitpub: list refs of %s: %w", o.URL, err)
	}
	byName := map[plumbing.ReferenceName]*plumbing.Reference{}
	for _, r := range refs {
		byName[r.Name()] = r
	}
	_, hasBranch = byName[branch]
	// HEAD is advertised either as a hash or, with the symref capability, as a symbolic
	// ref to the default branch; either way it must resolve to a commit.
	if h, ok := byName[plumbing.HEAD]; ok {
		switch h.Type() {
		case plumbing.HashReference:
			hasHead = !h.Hash().IsZero()
		case plumbing.SymbolicReference:
			t, ok := byName[h.Target()]
			hasHead = ok && t.Type() == plumbing.HashReference && !t.Hash().IsZero()
		}
	}
	return hasBranch, hasHead, nil
}

func initEmpty(o Options, branch plumbing.ReferenceName, st storage.Storer, wt billy.Filesystem) (*git.Repository, error) {
	repo, err := git.Init(st, wt)
	if err != nil {
		return nil, fmt.Errorf("gitpub: init: %w", err)
	}
	if err := st.SetReference(plumbing.NewSymbolicReference(plumbing.HEAD, branch)); err != nil {
		return nil, err
	}
	if _, err := repo.CreateRemote(&config.RemoteConfig{Name: git.DefaultRemoteName, URLs: []string{o.URL}}); err != nil {
		return nil, fmt.Errorf("gitpub: add remote: %w", err)
	}
	return repo, nil
}

// newStorage returns in-memory storage, or a freshly wiped on-disk clone location.
func newStorage(cacheDir string) (storage.Storer, billy.Filesystem, error) {
	if cacheDir == "" {
		return memory.NewStorage(), memfs.New(), nil
	}
	dir := filepath.Join(cacheDir, "repo")
	if err := os.RemoveAll(dir); err != nil {
		return nil, nil, fmt.Errorf("gitpub: clear cache %s: %w", dir, err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, nil, err
	}
	wt := osfs.New(dir)
	dot, err := wt.Chroot(git.GitDirName)
	if err != nil {
		return nil, nil, err
	}
	return filesystem.NewStorage(dot, cache.NewObjectLRUDefault()), wt, nil
}

// stageAll mirrors `git add -A`: stages new, modified and deleted files.
func stageAll(w *git.Worktree) error {
	if err := w.AddWithOptions(&git.AddOptions{All: true}); err != nil {
		return fmt.Errorf("gitpub: stage changes: %w", err)
	}
	return nil
}

// cleanRepoPath normalises Path to a slash-separated relative path; "" means the repo root.
func cleanRepoPath(p string) (string, error) {
	p = strings.ReplaceAll(strings.TrimSpace(p), "\\", "/")
	if strings.HasPrefix(p, "/") {
		return "", fmt.Errorf("gitpub: Path %q must be relative to the repository root", p)
	}
	p = path.Clean(p)
	if p == "." {
		return "", nil
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." || seg == git.GitDirName {
			return "", fmt.Errorf("gitpub: Path %q must stay inside the repository and not touch .git", p)
		}
	}
	return p, nil
}

// clearPath removes p from the worktree; for the repo root it removes everything but .git.
func clearPath(wt billy.Filesystem, p string) error {
	if p != "" {
		if err := util.RemoveAll(wt, p); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("gitpub: clear %s: %w", p, err)
		}
		return nil
	}
	entries, err := wt.ReadDir("/")
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.Name() == git.GitDirName {
			continue
		}
		if err := util.RemoveAll(wt, e.Name()); err != nil {
			return fmt.Errorf("gitpub: clear %s: %w", e.Name(), err)
		}
	}
	return nil
}

// copyTree copies regular files under src into wt at dst. .git directories are skipped;
// symlinks are refused rather than followed.
func copyTree(src string, wt billy.Filesystem, dst string) error {
	return filepath.WalkDir(src, func(fp string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, fp)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if d.Name() == git.GitDirName && rel != "." {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("gitpub: %s is not a regular file", fp)
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		mode := os.FileMode(0o644)
		if info.Mode()&0o111 != 0 {
			mode = 0o755
		}
		target := path.Join(dst, rel)
		if err := wt.MkdirAll(path.Dir(target), 0o755); err != nil {
			return err
		}
		in, err := os.Open(fp)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := wt.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, in); err != nil {
			out.Close()
			return err
		}
		return out.Close()
	})
}

func authFor(o Options) (transport.AuthMethod, error) {
	ep, err := transport.NewEndpoint(o.URL)
	if err != nil {
		return nil, fmt.Errorf("gitpub: bad URL %q: %w", o.URL, err)
	}
	switch ep.Protocol {
	case "http", "https":
		if o.Token == "" && o.Username == "" {
			return nil, nil
		}
		return &http.BasicAuth{Username: orDefault(o.Username, "git"), Password: o.Token}, nil
	case "ssh":
		if len(o.SSHKeyPEM) == 0 {
			return nil, errors.New("gitpub: an ssh URL needs SSHKeyPEM")
		}
		user := ep.User
		if user == "" {
			user = orDefault(o.Username, "git")
		}
		keys, err := gitssh.NewPublicKeys(user, o.SSHKeyPEM, o.SSHKeyPassphrase)
		if err != nil {
			return nil, fmt.Errorf("gitpub: parse SSH key: %w", err)
		}
		switch {
		case o.InsecureIgnoreHostKey:
			keys.HostKeyCallback = ssh.InsecureIgnoreHostKey() //nolint:gosec // operator opted in
		case o.SSHKnownHosts != "":
			cb, err := knownHostsCallback(o.SSHKnownHosts)
			if err != nil {
				return nil, err
			}
			keys.HostKeyCallback = cb
		default:
			return nil, errors.New("gitpub: an ssh URL needs SSHKnownHosts or InsecureIgnoreHostKey")
		}
		return keys, nil
	default:
		return nil, nil
	}
}

func knownHostsCallback(content string) (ssh.HostKeyCallback, error) {
	f, err := os.CreateTemp("", "gitpub-known-hosts-*")
	if err != nil {
		return nil, err
	}
	defer os.Remove(f.Name())
	if _, err := f.WriteString(content); err != nil {
		f.Close()
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	cb, err := knownhosts.New(f.Name())
	if err != nil {
		return nil, fmt.Errorf("gitpub: parse known_hosts: %w", err)
	}
	return cb, nil
}

func orDefault(v, d string) string {
	if v == "" {
		return d
	}
	return v
}

// localLoader serves file:// and plain-path remotes from the local filesystem for
// go-git's in-process server (replacing the transport that execs git-upload-pack).
type localLoader struct{}

func (localLoader) Load(ep *transport.Endpoint) (storer.Storer, error) {
	p := ep.Path
	// file:///C:/x arrives as /C:/x on Windows.
	if len(p) >= 3 && p[0] == '/' && p[2] == ':' && filepath.VolumeName(p[1:]) != "" {
		p = p[1:]
	}
	fsys := osfs.New(p)
	if _, err := fsys.Stat("config"); err != nil {
		dot, err := fsys.Chroot(git.GitDirName)
		if err != nil {
			return nil, err
		}
		if _, err := dot.Stat("config"); err != nil {
			return nil, transport.ErrRepositoryNotFound
		}
		fsys = dot
	}
	return filesystem.NewStorage(fsys, cache.NewObjectLRUDefault()), nil
}
