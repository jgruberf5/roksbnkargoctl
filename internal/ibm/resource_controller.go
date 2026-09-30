package ibm

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
)

// ResourceGroup is an account resource group.
type ResourceGroup struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	State     string `json:"state"`
	IsDefault bool   `json:"default"`
}

// ListResourceGroups returns every resource group in the API key's account.
func (c *Client) ListResourceGroups(ctx context.Context) ([]ResourceGroup, error) {
	acct, err := c.AccountID(ctx)
	if err != nil {
		return nil, err
	}
	var out struct {
		Resources []ResourceGroup `json:"resources"`
	}
	u := c.rcURL + "/v2/resource_groups?account_id=" + url.QueryEscape(acct)
	if err := c.do(ctx, http.MethodGet, u, nil, nil, &out); err != nil {
		return nil, fmt.Errorf("listing resource groups: %w", err)
	}
	return out.Resources, nil
}

// ResolveResourceGroup matches nameOrID against both the id and the name of the
// account's resource groups and returns the group's id and name.
func (c *Client) ResolveResourceGroup(ctx context.Context, nameOrID string) (id, name string, err error) {
	nameOrID = strings.TrimSpace(nameOrID)
	if nameOrID == "" {
		return "", "", errors.New("resource group name/id is empty")
	}
	groups, err := c.ListResourceGroups(ctx)
	if err != nil {
		return "", "", err
	}
	for _, g := range groups {
		if g.ID == nameOrID {
			return g.ID, g.Name, nil
		}
	}
	var matches []ResourceGroup
	for _, g := range groups {
		if g.Name == nameOrID {
			matches = append(matches, g)
		}
	}
	switch len(matches) {
	case 1:
		return matches[0].ID, matches[0].Name, nil
	case 0:
		return "", "", fmt.Errorf("no resource group named or with id %q in this account", nameOrID)
	default:
		return "", "", fmt.Errorf("resource group name %q is ambiguous (%d matches); pass its id", nameOrID, len(matches))
	}
}

// ServiceInstance is a Resource Controller service instance (a COS instance,
// for example).
type ServiceInstance struct {
	ID              string `json:"id"`
	GUID            string `json:"guid"`
	CRN             string `json:"crn"`
	Name            string `json:"name"`
	RegionID        string `json:"region_id"`
	ResourceGroupID string `json:"resource_group_id"`
	State           string `json:"state"`
}

// ErrServiceInstanceNotFound is returned by FindServiceInstance when nothing
// matches.
var ErrServiceInstanceNotFound = errors.New("service instance not found")

// FindServiceInstance finds the live instance of serviceName (the CRN service
// segment, e.g. "cloud-object-storage") called name.
//
// resourceGroupID scopes the search when set. When it is empty EVERY group is
// searched, and more than one match is an error naming the groups: a shared
// supply-chain COS instance typically lives in a different group from the
// workspace (roksbnkctl#295), so pinning the lookup to the workspace group
// fails for an instance that plainly exists.
func (c *Client) FindServiceInstance(ctx context.Context, name, serviceName, resourceGroupID string) (*ServiceInstance, error) {
	if name == "" || serviceName == "" {
		return nil, errors.New("service instance name and service name are required")
	}
	q := url.Values{}
	q.Set("name", name)
	if resourceGroupID != "" {
		q.Set("resource_group_id", resourceGroupID)
	}
	live, err := c.listServiceInstances(ctx, q, serviceName)
	if err != nil {
		return nil, err
	}
	var matches []ServiceInstance
	for _, r := range live {
		if r.Name == name {
			matches = append(matches, r)
		}
	}
	switch len(matches) {
	case 1:
		return &matches[0], nil
	case 0:
		scope := "any resource group"
		if resourceGroupID != "" {
			scope = "resource group " + resourceGroupID
		}
		return nil, fmt.Errorf("%w: no %s instance named %q in %s", ErrServiceInstanceNotFound, serviceName, name, scope)
	default:
		var groups []string
		for _, m := range matches {
			groups = append(groups, m.ResourceGroupID)
		}
		sort.Strings(groups)
		return nil, fmt.Errorf("%s instance name %q is ambiguous: %d instances, in resource groups %s; set the resource group",
			serviceName, name, len(matches), strings.Join(groups, ", "))
	}
}

// ListServiceInstances returns every live instance of serviceName (the CRN
// service segment, e.g. "cloud-object-storage") in the account, in every
// resource group, sorted by name then GUID.
func (c *Client) ListServiceInstances(ctx context.Context, serviceName string) ([]ServiceInstance, error) {
	if serviceName == "" {
		return nil, errors.New("service name is required")
	}
	q := url.Values{}
	q.Set("type", "service_instance")
	out, err := c.listServiceInstances(ctx, q, serviceName)
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].GUID < out[j].GUID
	})
	return out, nil
}

// listServiceInstances walks /v2/resource_instances with q, keeping the live
// instances of serviceName: a removed or reclaimable instance is not one a
// command can use.
func (c *Client) listServiceInstances(ctx context.Context, q url.Values, serviceName string) ([]ServiceInstance, error) {
	u := c.rcURL + "/v2/resource_instances?" + q.Encode()
	var out []ServiceInstance
	for u != "" {
		var page struct {
			Resources []ServiceInstance `json:"resources"`
			NextURL   string            `json:"next_url"`
		}
		if err := c.do(ctx, http.MethodGet, u, nil, nil, &page); err != nil {
			return nil, fmt.Errorf("listing resource instances: %w", err)
		}
		for _, r := range page.Resources {
			if !strings.Contains(r.CRN, ":"+serviceName+":") {
				continue
			}
			if s := strings.ToLower(r.State); s == "removed" || s == "pending_reclamation" {
				continue
			}
			out = append(out, r)
		}
		u = ""
		if page.NextURL != "" {
			if strings.HasPrefix(page.NextURL, "http") {
				u = page.NextURL
			} else {
				u = c.rcURL + page.NextURL
			}
		}
	}
	return out, nil
}

// MatchServiceInstance picks the instance ref names: its CRN, its GUID, or its
// name, tried in that order. A name two instances share is an error that lists
// both, so the operator can pass the GUID or CRN instead.
func MatchServiceInstance(instances []ServiceInstance, ref string) (*ServiceInstance, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil, errors.New("service instance name, GUID or CRN is empty")
	}
	for i := range instances {
		if instances[i].CRN == ref || instances[i].GUID == ref {
			return &instances[i], nil
		}
	}
	var matches []ServiceInstance
	for _, si := range instances {
		if si.Name == ref {
			matches = append(matches, si)
		}
	}
	switch len(matches) {
	case 1:
		return &matches[0], nil
	case 0:
		return nil, fmt.Errorf("%w: no instance named, or with GUID or CRN, %q", ErrServiceInstanceNotFound, ref)
	}
	var lines []string
	for _, m := range matches {
		lines = append(lines, fmt.Sprintf("  %s  guid %s  resource group %s  %s", m.Name, m.GUID, m.ResourceGroupID, m.CRN))
	}
	return nil, fmt.Errorf("instance name %q is ambiguous: %d instances share it; pass the GUID or CRN of one:\n%s",
		ref, len(matches), strings.Join(lines, "\n"))
}
