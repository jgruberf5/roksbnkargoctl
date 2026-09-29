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
	u := c.rcURL + "/v2/resource_instances?" + q.Encode()
	var matches []ServiceInstance
	for u != "" {
		var page struct {
			Resources []ServiceInstance `json:"resources"`
			NextURL   string            `json:"next_url"`
		}
		if err := c.do(ctx, http.MethodGet, u, nil, nil, &page); err != nil {
			return nil, fmt.Errorf("listing resource instances: %w", err)
		}
		for _, r := range page.Resources {
			if r.Name != name || !strings.Contains(r.CRN, ":"+serviceName+":") {
				continue
			}
			if s := strings.ToLower(r.State); s == "removed" || s == "pending_reclamation" {
				continue
			}
			matches = append(matches, r)
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
