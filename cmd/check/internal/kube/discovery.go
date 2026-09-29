package kube

import (
	"context"
	"net/http"
	"sort"
	"strings"
)

// APIResource is one entry of a group-version's discovery document.
type APIResource struct {
	Name       string   `json:"name"`
	Namespaced bool     `json:"namespaced"`
	Kind       string   `json:"kind"`
	Verbs      []string `json:"verbs"`
}

// HasVerb matches a verb exactly. strings.Contains over the joined list would
// accept "delete" inside "deletecollection" — the bug roksbnkctl's hasVerb fixed.
func (r APIResource) HasVerb(v string) bool {
	for _, x := range r.Verbs {
		if x == v {
			return true
		}
	}
	return false
}

// PreferredVersion returns the preferred version of an API group, or "" when the
// group is not served (its CRDs are absent).
func (c *Client) PreferredVersion(ctx context.Context, group string) (string, error) {
	var gl struct {
		Groups []struct {
			Name             string `json:"name"`
			PreferredVersion struct {
				Version string `json:"version"`
			} `json:"preferredVersion"`
		} `json:"groups"`
	}
	if err := c.Do(ctx, http.MethodGet, "/apis", nil, "", nil, &gl); err != nil {
		return "", err
	}
	for _, g := range gl.Groups {
		if g.Name == group {
			return g.PreferredVersion.Version, nil
		}
	}
	return "", nil
}

// Resources lists the resources of group/version, without subresources.
func (c *Client) Resources(ctx context.Context, group, version string) ([]APIResource, error) {
	p := "/apis/" + group + "/" + version
	if group == "" {
		p = "/api/" + version
	}
	var rl struct {
		Resources []APIResource `json:"resources"`
	}
	if err := c.Do(ctx, http.MethodGet, p, nil, "", nil, &rl); err != nil {
		return nil, err
	}
	var out []APIResource
	for _, r := range rl.Resources {
		if strings.Contains(r.Name, "/") {
			continue // status, scale, ... — not objects
		}
		out = append(out, r)
	}
	return out, nil
}

// DiscoverGroups resolves every listable resource in the given API groups at
// their preferred versions.
//
// PARTIAL FAILURE IS NORMAL. During a teardown CRDs disappear while this looks,
// so a group that fails to resolve is skipped rather than failing the whole
// discovery — giving up on all groups because one was mid-deletion would skip a
// sweep exactly when it is needed. The per-group errors are returned alongside.
func (c *Client) DiscoverGroups(ctx context.Context, groups []string) ([]GVR, []error) {
	var gl struct {
		Groups []struct {
			Name             string `json:"name"`
			PreferredVersion struct {
				Version string `json:"version"`
			} `json:"preferredVersion"`
		} `json:"groups"`
	}
	if err := c.Do(ctx, http.MethodGet, "/apis", nil, "", nil, &gl); err != nil {
		return nil, []error{err}
	}
	want := map[string]bool{}
	for _, g := range groups {
		want[g] = true
	}
	var out []GVR
	var errs []error
	for _, g := range gl.Groups {
		if !want[g.Name] || g.PreferredVersion.Version == "" {
			continue
		}
		rs, err := c.Resources(ctx, g.Name, g.PreferredVersion.Version)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, r := range rs {
			if !r.HasVerb("list") {
				continue
			}
			out = append(out, GVR{Group: g.Name, Version: g.PreferredVersion.Version, Resource: r.Name, Namespaced: r.Namespaced, Kind: r.Kind, CanDelete: r.HasVerb("delete")})
		}
	}
	// Stable order: the log reads the same twice and tests do not depend on
	// discovery order.
	sort.Slice(out, func(i, j int) bool {
		if out[i].Group != out[j].Group {
			return out[i].Group < out[j].Group
		}
		return out[i].Resource < out[j].Resource
	})
	return out, errs
}
