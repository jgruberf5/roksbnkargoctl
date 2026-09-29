package argocd

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// Version returns the Argo CD server version string, e.g. "v3.5.1+abcdef0"
// (GET /api/version, field "Version").
func (c *Client) Version(ctx context.Context) (string, error) {
	var v struct {
		Version string `json:"Version"`
	}
	if err := c.do(ctx, "read server version", "GET", "/api/version", nil, &v); err != nil {
		return "", err
	}
	if v.Version == "" {
		return "", fmt.Errorf("argocd: server at %s returned an empty version", c.server)
	}
	return v.Version, nil
}

// RequireAtLeast fails unless the server version is >= min (e.g. "3.3.0").
func (c *Client) RequireAtLeast(ctx context.Context, min string) error {
	have, err := c.Version(ctx)
	if err != nil {
		return err
	}
	ok, err := AtLeast(have, min)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("argocd: server version %s is older than the required %s (PreDelete hooks need Argo CD >= 3.3)", have, min)
	}
	return nil
}

// AtLeast reports whether version have >= min. Both may carry a leading "v" and
// "-prerelease" / "+build" suffixes, which are ignored.
func AtLeast(have, min string) (bool, error) {
	h, err := ParseVersion(have)
	if err != nil {
		return false, err
	}
	m, err := ParseVersion(min)
	if err != nil {
		return false, err
	}
	for i := range 3 {
		if h[i] != m[i] {
			return h[i] > m[i], nil
		}
	}
	return true, nil
}

// ParseVersion parses "v3.5.1+abcdef" / "3.3.0-rc1" / "3.4" into [major, minor, patch].
func ParseVersion(s string) ([3]int, error) {
	var out [3]int
	v := strings.TrimSpace(s)
	v = strings.TrimPrefix(strings.TrimPrefix(v, "v"), "V")
	if i := strings.IndexAny(v, "+-"); i >= 0 {
		v = v[:i]
	}
	parts := strings.Split(v, ".")
	if v == "" || len(parts) > 3 {
		return out, fmt.Errorf("argocd: cannot parse version %q", s)
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return out, fmt.Errorf("argocd: cannot parse version %q", s)
		}
		out[i] = n
	}
	return out, nil
}

// CheckServer is install's first step: the server is new enough AND accepts the
// token. /api/version answers without authentication (Argo CD 3.5.1 returned
// 200 to a bogus token and to none), so a wrong token used to pass here and
// fail only at cluster registration, after the transit gateway, IAM and ROKS
// had been changed. /api/v1/session/userinfo needs no RBAC permission and
// reports loggedIn only for a token it accepted.
func (c *Client) CheckServer(ctx context.Context, min string) error {
	if err := c.RequireAtLeast(ctx, min); err != nil {
		return err
	}
	var u struct {
		LoggedIn bool   `json:"loggedIn"`
		Username string `json:"username"`
	}
	if err := c.do(ctx, "check the token", "GET", "/api/v1/session/userinfo", nil, &u); err != nil {
		return err
	}
	if !u.LoggedIn {
		return fmt.Errorf("argocd: the server at %s did not accept the API token (session/userinfo: not logged in); check ARGOCD_AUTH_TOKEN", c.server)
	}
	return nil
}
