package gitpub

import (
	"context"
	"errors"
	"fmt"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-git/v5/plumbing/transport/client"
)

// Access is what Check found.
type Access struct {
	HasBranch bool // the branch exists on the remote
	Empty     bool // the repository has no commits yet
}

// Check proves the repository is reachable with o's credential, before anything
// is changed elsewhere (#18). With write it asks for the receive-pack ref
// advertisement, the one a push starts with: Git hosts answer it only to a
// credential that may push, so push rights are checked without pushing
// anything. Without write it asks for the upload-pack (clone) advertisement.
// The same authentication and SSH host-key verification as Publish apply.
func Check(ctx context.Context, o Options, write bool) (Access, error) {
	if o.URL == "" {
		return Access{}, errors.New("git.url is not set")
	}
	branch := plumbing.NewBranchReferenceName(orDefault(o.Branch, "main"))
	auth, err := authFor(o)
	if err != nil {
		return Access{}, err
	}
	ep, err := transport.NewEndpoint(o.URL)
	if err != nil {
		return Access{}, fmt.Errorf("git.url %q: %w", o.URL, err)
	}
	ep.CaBundle = o.CABundle
	cli, err := client.NewClient(ep)
	if err != nil {
		return Access{}, fmt.Errorf("git.url %q: %w", o.URL, err)
	}
	var ar *packp.AdvRefs
	if write {
		s, err := cli.NewReceivePackSession(ep, auth)
		if err != nil {
			return Access{}, explainAccess(o.URL, true, err)
		}
		defer s.Close()
		if ar, err = s.AdvertisedReferencesContext(ctx); err != nil && !errors.Is(err, transport.ErrEmptyRemoteRepository) {
			return Access{}, explainAccess(o.URL, true, err)
		}
	} else {
		s, err := cli.NewUploadPackSession(ep, auth)
		if err != nil {
			return Access{}, explainAccess(o.URL, false, err)
		}
		defer s.Close()
		if ar, err = s.AdvertisedReferencesContext(ctx); err != nil && !errors.Is(err, transport.ErrEmptyRemoteRepository) {
			return Access{}, explainAccess(o.URL, false, err)
		}
	}
	if ar == nil || len(ar.References) == 0 {
		return Access{Empty: true}, nil
	}
	_, has := ar.References[branch.String()]
	return Access{HasBranch: has}, nil
}

// explainAccess turns go-git's transport errors into what to fix.
func explainAccess(url string, write bool, err error) error {
	what := "read"
	if write {
		what = "push to"
	}
	switch {
	case errors.Is(err, transport.ErrRepositoryNotFound):
		return fmt.Errorf("git: %s not found, or the credential cannot see it: %w", url, err)
	case errors.Is(err, transport.ErrAuthenticationRequired):
		return fmt.Errorf("git: %s needs a credential (a token in git.token_env, ROKSBNKARGOCTL_GIT_TOKEN by default, or git.ssh_key_file): %w", url, err)
	case errors.Is(err, transport.ErrAuthorizationFailed):
		return fmt.Errorf("git: the credential may not %s %s (a read-only token or deploy key?): %w", what, url, err)
	}
	return fmt.Errorf("git: cannot %s %s: %w", what, url, err)
}
