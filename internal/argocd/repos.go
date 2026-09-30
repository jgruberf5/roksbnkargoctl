package argocd

import (
	"context"
	"errors"
	"net/url"
	"strings"
)

// Repo is a repository credential entry: a Git repository, or with HelmOCI an
// OCI registry path Helm charts are pulled from (URL without a scheme).
type Repo struct {
	URL           string
	Username      string
	Password      string
	SSHPrivateKey string
	Insecure      bool // skip TLS verification / SSH host key checking
	Project       string
	HelmOCI       bool
	Name          string // required by Argo CD for a Helm repository
}

type repoBody struct {
	Repo                  string `json:"repo"`
	Type                  string `json:"type"`
	Username              string `json:"username,omitempty"`
	Password              string `json:"password,omitempty"`
	SSHPrivateKey         string `json:"sshPrivateKey,omitempty"`
	Insecure              bool   `json:"insecure,omitempty"`
	InsecureIgnoreHostKey bool   `json:"insecureIgnoreHostKey,omitempty"`
	Project               string `json:"project,omitempty"`
	EnableOCI             bool   `json:"enableOCI,omitempty"`
	Name                  string `json:"name,omitempty"`
}

// RepoInfo is the subset of v1alpha1.Repository read back.
type RepoInfo struct {
	Repo            string `json:"repo"`
	Type            string `json:"type,omitempty"`
	Project         string `json:"project,omitempty"`
	ConnectionState struct {
		Status  string `json:"status,omitempty"`
		Message string `json:"message,omitempty"`
	} `json:"connectionState"`
}

// UpsertRepository creates or updates a repository: POST /api/v1/repositories?upsert=true.
func (c *Client) UpsertRepository(ctx context.Context, r Repo) (*RepoInfo, error) {
	body := repoBody{
		Repo:                  r.URL,
		Type:                  "git",
		Username:              r.Username,
		Password:              r.Password,
		SSHPrivateKey:         r.SSHPrivateKey,
		Insecure:              r.Insecure,
		InsecureIgnoreHostKey: r.Insecure && r.SSHPrivateKey != "",
		Project:               r.Project,
	}
	if r.HelmOCI {
		body.Type, body.EnableOCI, body.Name = "helm", true, r.Name
	}
	var out RepoInfo
	if err := c.do(ctx, "add repository "+r.URL, "POST", "/api/v1/repositories?upsert=true", body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetRepository finds the repository entry for repoURL by listing (Argo CD returns 403,
// not 404, for an unknown repo on GET/DELETE). A missing repo yields IsNotFound.
func (c *Client) GetRepository(ctx context.Context, repoURL string) (*RepoInfo, error) {
	var list struct {
		Items []RepoInfo `json:"items"`
	}
	op := "read repository " + repoURL
	if err := c.do(ctx, op, "GET", "/api/v1/repositories", nil, &list); err != nil {
		return nil, err
	}
	for i := range list.Items {
		if sameRepoURL(list.Items[i].Repo, repoURL) {
			return &list.Items[i], nil
		}
	}
	return nil, notFound(op, "repository "+repoURL+" is not configured")
}

// DeleteRepository removes the repository entry (DELETE /api/v1/repositories/{repo},
// the URL escaped as one path segment). A missing repo yields IsNotFound.
func (c *Client) DeleteRepository(ctx context.Context, repoURL string) error {
	r, err := c.GetRepository(ctx, repoURL)
	if err != nil {
		return err
	}
	path := "/api/v1/repositories/" + pathEscape(r.Repo)
	if r.Project != "" {
		path += "?appProject=" + url.QueryEscape(r.Project)
	}
	return c.do(ctx, "delete repository "+repoURL, "DELETE", path, nil, nil)
}

// sameRepoURL is a loose version of Argo CD's git.SameURL: case-insensitive, ignoring
// a trailing "/" or ".git".
func sameRepoURL(a, b string) bool {
	norm := func(s string) string {
		s = strings.ToLower(strings.TrimSpace(s))
		s = strings.TrimRight(s, "/")
		return strings.TrimSuffix(s, ".git")
	}
	return norm(a) == norm(b)
}

// RepositoryBranches lists repoURL's branches as Argo CD reads them (GET
// /api/v1/repositories/{repo}/refs), with the credential registered in Argo CD,
// or anonymously when none is. It proves Argo CD can read the repository
// without the operator holding a Git credential. project is tried first, for
// a repository registered to that project, then the global registration.
func (c *Client) RepositoryBranches(ctx context.Context, repoURL, project string) ([]string, error) {
	var refs struct {
		Branches []string `json:"branches"`
	}
	path := "/api/v1/repositories/" + pathEscape(repoURL) + "/refs"
	op := "read the branches of " + repoURL
	var err error
	if project != "" {
		if err = c.do(ctx, op, "GET", path+"?appProject="+url.QueryEscape(project), nil, &refs); err == nil {
			return refs.Branches, nil
		}
	}
	if err2 := c.do(ctx, op, "GET", path, nil, &refs); err2 != nil {
		if err == nil || err.Error() == err2.Error() {
			return nil, err2
		}
		return nil, errors.Join(err, err2)
	}
	return refs.Branches, nil
}
