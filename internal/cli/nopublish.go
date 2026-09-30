package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"sigs.k8s.io/yaml"

	"github.com/jgruberf5/roksbnkargoctl/internal/argocd"
	"github.com/jgruberf5/roksbnkargoctl/internal/render"
)

// With `install --no-publish` roksbnkargoctl does not push to Git: the user
// committed an `export` themselves. Before syncing, install compares what the
// Application would sync (Argo CD's view of Git) with this workspace's render,
// so a missing, partial or stale upload is refused instead of installed.

// objectKey identifies an object independent of file names.
func objectKey(obj map[string]any) string {
	md, _ := obj["metadata"].(map[string]any)
	ns, _ := md["namespace"].(string)
	name, _ := md["name"].(string)
	api, _ := obj["apiVersion"].(string)
	kind, _ := obj["kind"].(string)
	group := api
	if i := strings.LastIndex(api, "/"); i >= 0 {
		group = api[:i]
	} else {
		group = ""
	}
	return fmt.Sprintf("%s/%s %s/%s", group, kind, ns, name)
}

// objectDigest hashes an object without what Argo CD adds when it generates
// manifests (its tracking annotation and the app-instance label), so the same
// file hashes the same whether read from disk or from Argo CD.
func objectDigest(obj map[string]any) (string, error) {
	b, err := json.Marshal(obj)
	if err != nil {
		return "", err
	}
	var c map[string]any
	if err := json.Unmarshal(b, &c); err != nil {
		return "", err
	}
	if md, ok := c["metadata"].(map[string]any); ok {
		if a, ok := md["annotations"].(map[string]any); ok {
			delete(a, "argocd.argoproj.io/tracking-id")
			if len(a) == 0 {
				delete(md, "annotations")
			}
		}
		if l, ok := md["labels"].(map[string]any); ok {
			delete(l, "app.kubernetes.io/instance")
			if len(l) == 0 {
				delete(md, "labels")
			}
		}
	}
	nb, err := json.Marshal(c) // map keys are sorted: canonical
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(nb)
	return hex.EncodeToString(sum[:]), nil
}

// gitMatchesRender compares Argo CD's manifests (JSON strings) with the
// rendered objects. It returns human-readable differences, empty when equal.
func gitMatchesRender(argo []string, rendered []render.Object) ([]string, error) {
	want := map[string]string{}
	for _, o := range rendered {
		d, err := objectDigest(o)
		if err != nil {
			return nil, err
		}
		want[objectKey(o)] = d
	}
	got := map[string]string{}
	for _, m := range argo {
		var o map[string]any
		if err := yaml.Unmarshal([]byte(m), &o); err != nil {
			return nil, fmt.Errorf("a manifest from Argo CD does not parse: %w", err)
		}
		d, err := objectDigest(o)
		if err != nil {
			return nil, err
		}
		got[objectKey(o)] = d
	}
	var diffs []string
	for k, d := range want {
		switch g, ok := got[k]; {
		case !ok:
			diffs = append(diffs, "missing in Git: "+k)
		case g != d:
			diffs = append(diffs, "different in Git: "+k)
		}
	}
	for k := range got {
		if _, ok := want[k]; !ok {
			diffs = append(diffs, "in Git but not in this render: "+k)
		}
	}
	sort.Strings(diffs)
	return diffs, nil
}

// missingGitCredential is gitOptions' error when neither a token nor an SSH
// key is set. With --no-publish nothing is pushed, so Argo CD may read the
// repository anonymously.
func missingGitCredential(err error) bool { return errors.Is(err, errNoGitCredential) }

// checkGitMatchesRender asks Argo CD what the Application would sync and
// compares it with manifests/git. It returns the revision it compared, which
// install then syncs, so a push landing in between is not what gets applied.
func checkGitMatchesRender(ctx context.Context, s *session, ac *argocd.Client) (string, error) {
	c, p := s.cfg, s.p
	rendered, err := readYAMLDir(filepath.Join(s.ws.ManifestsDir(), "git"))
	if err != nil {
		return "", fmt.Errorf("reading this workspace's render (run `roksbnkargoctl render`): %w", err)
	}
	p.step("checking %s %s:%s against this workspace's render", c.Git.URL, c.Git.Branch, c.Git.Path)
	rev, manifests, err := ac.Manifests(ctx, c.ArgoCD.Application, "")
	if err != nil {
		return "", fmt.Errorf("the Application was created but not synced: Argo CD could not read %s %s:%s: %w (was the export pushed there?)", c.Git.URL, c.Git.Branch, c.Git.Path, err)
	}
	diffs, err := gitMatchesRender(manifests, rendered)
	if err != nil {
		return "", err
	}
	if len(diffs) > 0 {
		shown := diffs
		if len(shown) > 10 {
			shown = append(shown[:10:10], fmt.Sprintf("… and %d more", len(diffs)-10))
		}
		return "", fmt.Errorf("the Application was created but NOT synced: what is in Git (revision %s) does not match this workspace's render (%d difference(s)):\n  %s\nrun `roksbnkargoctl export`, replace %s in the repository with its contents, push, and run install --no-publish again",
			short(rev), len(diffs), strings.Join(shown, "\n  "), c.Git.Path)
	}
	p.ok("Git at %s matches this render (%d objects)", short(rev), len(rendered))
	return rev, nil
}
