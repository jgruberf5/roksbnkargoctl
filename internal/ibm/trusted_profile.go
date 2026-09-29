package ibm

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// TrustedProfile is an IAM trusted profile.
type TrustedProfile struct {
	ID        string `json:"id"`     // Profile-<uuid>
	IAMID     string `json:"iam_id"` // iam-Profile-<uuid>; the policy subject
	CRN       string `json:"crn"`
	Name      string `json:"name"`
	AccountID string `json:"account_id"`
}

// CreateTrustedProfile creates a trusted profile in the API key's account. Not
// idempotent: call FindTrustedProfileByName first.
func (c *Client) CreateTrustedProfile(ctx context.Context, name, description string) (*TrustedProfile, error) {
	if name == "" {
		return nil, errors.New("trusted profile name is empty")
	}
	acct, err := c.AccountID(ctx)
	if err != nil {
		return nil, err
	}
	body := map[string]any{"name": name, "account_id": acct}
	if description != "" {
		body["description"] = description
	}
	var tp TrustedProfile
	if err := c.do(ctx, http.MethodPost, c.iamURL+"/v1/profiles", nil, body, &tp); err != nil {
		return nil, fmt.Errorf("creating trusted profile %q: %w", name, friendlyAuthErr(err))
	}
	return &tp, nil
}

// FindTrustedProfileByName returns the account's trusted profile called name,
// or (nil, nil).
func (c *Client) FindTrustedProfileByName(ctx context.Context, name string) (*TrustedProfile, error) {
	acct, err := c.AccountID(ctx)
	if err != nil {
		return nil, err
	}
	q := url.Values{"account_id": {acct}, "name": {name}, "pagesize": {"100"}}
	var out struct {
		Profiles []TrustedProfile `json:"profiles"`
	}
	if err := c.do(ctx, http.MethodGet, c.iamURL+"/v1/profiles?"+q.Encode(), nil, nil, &out); err != nil {
		return nil, fmt.Errorf("listing trusted profiles: %w", friendlyAuthErr(err))
	}
	for i := range out.Profiles {
		if out.Profiles[i].Name == name {
			return &out.Profiles[i], nil
		}
	}
	return nil, nil
}

// ProfileLink is a trusted profile's compute-resource link.
type ProfileLink struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	CRType string `json:"cr_type"`
	Link   struct {
		CRN       string `json:"crn"`
		Namespace string `json:"namespace"`
		Name      string `json:"name"`
	} `json:"link"`
}

// ListProfileLinks returns a trusted profile's links.
func (c *Client) ListProfileLinks(ctx context.Context, profileID string) ([]ProfileLink, error) {
	var out struct {
		Links []ProfileLink `json:"links"`
	}
	u := c.iamURL + "/v1/profiles/" + url.PathEscape(profileID) + "/links"
	if err := c.do(ctx, http.MethodGet, u, nil, nil, &out); err != nil {
		return nil, fmt.Errorf("listing links of trusted profile %s: %w", profileID, friendlyAuthErr(err))
	}
	return out.Links, nil
}

// LinkROKSServiceAccount links a trusted profile to a ServiceAccount on a ROKS
// cluster (cr_type ROKS_SA), so pods running as that account can assume the
// profile. Idempotent: an existing identical link (HTTP 409) is found and its
// id returned.
func (c *Client) LinkROKSServiceAccount(ctx context.Context, profileID, clusterCRN, namespace, saName, linkName string) (string, error) {
	if profileID == "" || clusterCRN == "" || namespace == "" || saName == "" {
		return "", errors.New("LinkROKSServiceAccount needs a profile id, cluster CRN, namespace and ServiceAccount name")
	}
	body := map[string]any{
		"cr_type": "ROKS_SA",
		"link":    map[string]string{"crn": clusterCRN, "namespace": namespace, "name": saName},
	}
	if linkName != "" {
		body["name"] = linkName
	}
	var out struct {
		ID string `json:"id"`
	}
	u := c.iamURL + "/v1/profiles/" + url.PathEscape(profileID) + "/links"
	err := c.do(ctx, http.MethodPost, u, nil, body, &out)
	if err == nil {
		return out.ID, nil
	}
	if !IsConflict(err) {
		return "", fmt.Errorf("linking trusted profile %s to %s/%s: %w", profileID, namespace, saName, friendlyAuthErr(err))
	}
	links, lerr := c.ListProfileLinks(ctx, profileID)
	if lerr != nil {
		return "", lerr
	}
	for _, l := range links {
		if l.CRType == "ROKS_SA" && l.Link.CRN == clusterCRN && l.Link.Namespace == namespace && l.Link.Name == saName {
			return l.ID, nil
		}
	}
	return "", fmt.Errorf("linking trusted profile %s: IAM reported a conflict but no matching link exists: %w", profileID, err)
}

// PolicyAttribute is one resource attribute of an access policy.
type PolicyAttribute struct {
	Name  string `json:"name"`
	Value string `json:"value"`
	// Operator defaults to stringEquals on the wire when empty.
	Operator string `json:"operator,omitempty"`
}

// VPCPolicyAttributes scopes a policy to one VPC's infrastructure: service
// "is", vpcId=<id>.
func VPCPolicyAttributes(vpcID string) []PolicyAttribute {
	return []PolicyAttribute{{Name: "serviceName", Value: "is"}, {Name: "vpcId", Value: vpcID}}
}

// ClusterPolicyAttributes scopes a policy to one Kubernetes Service cluster:
// service "containers-kubernetes", serviceInstance=<cluster id>.
//
// serviceInstance, NOT clusterId: IAM rejects clusterId with HTTP 400
// "Invalid Attribute(s): clusterId" (roksbnkctl #166). The VPC policy's vpcId
// makes the wrong name look plausible.
func ClusterPolicyAttributes(clusterID string) []PolicyAttribute {
	return []PolicyAttribute{{Name: "serviceName", Value: "containers-kubernetes"}, {Name: "serviceInstance", Value: clusterID}}
}

// RoleCRN maps a role name to its IAM CRN: "Viewer" ->
// crn:v1:bluemix:public:iam::::role:Viewer. Service roles (Reader, Writer,
// Manager) live under serviceRole. A value that is already a CRN is returned
// as is.
func RoleCRN(role string) string {
	if strings.HasPrefix(role, "crn:") {
		return role
	}
	switch role {
	case "Reader", "Writer", "Manager":
		return "crn:v1:bluemix:public:iam::::serviceRole:" + role
	}
	return "crn:v1:bluemix:public:iam::::role:" + role
}

// profileIAMID turns a profile id (Profile-<uuid>) into its IAM id
// (iam-Profile-<uuid>), the form a policy subject takes. An IAM id passes
// through.
func profileIAMID(profileID string) string {
	if strings.HasPrefix(profileID, "iam-") {
		return profileID
	}
	return "iam-" + profileID
}

// CreatePolicy grants roles on the resource described by attrs to a trusted
// profile (profileID is the Profile-<uuid> id or the iam-Profile-<uuid> IAM
// id). The account scope is added automatically. Returns the policy id.
func (c *Client) CreatePolicy(ctx context.Context, profileID string, roles []string, attrs []PolicyAttribute) (string, error) {
	if profileID == "" || len(roles) == 0 || len(attrs) == 0 {
		return "", errors.New("CreatePolicy needs a profile, at least one role and at least one resource attribute")
	}
	acct, err := c.AccountID(ctx)
	if err != nil {
		return "", err
	}
	resAttrs := []PolicyAttribute{{Name: "accountId", Value: acct}}
	for _, a := range attrs {
		if a.Name == "accountId" {
			continue
		}
		resAttrs = append(resAttrs, a)
	}
	roleRefs := make([]map[string]string, 0, len(roles))
	for _, r := range roles {
		roleRefs = append(roleRefs, map[string]string{"role_id": RoleCRN(r)})
	}
	body := map[string]any{
		"type":      "access",
		"subjects":  []map[string]any{{"attributes": []PolicyAttribute{{Name: "iam_id", Value: profileIAMID(profileID)}}}},
		"roles":     roleRefs,
		"resources": []map[string]any{{"attributes": resAttrs}},
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := c.do(ctx, http.MethodPost, c.iamURL+"/v1/policies", nil, body, &out); err != nil {
		return "", fmt.Errorf("creating access policy for %s: %w", profileID, friendlyAuthErr(err))
	}
	return out.ID, nil
}

// ListProfilePolicies returns the ids of the access policies whose subject is
// the trusted profile.
func (c *Client) ListProfilePolicies(ctx context.Context, profileID string) ([]string, error) {
	acct, err := c.AccountID(ctx)
	if err != nil {
		return nil, err
	}
	q := url.Values{"account_id": {acct}, "iam_id": {profileIAMID(profileID)}, "type": {"access"}}
	var out struct {
		Policies []struct {
			ID string `json:"id"`
		} `json:"policies"`
	}
	if err := c.do(ctx, http.MethodGet, c.iamURL+"/v1/policies?"+q.Encode(), nil, nil, &out); err != nil {
		return nil, fmt.Errorf("listing policies of %s: %w", profileID, friendlyAuthErr(err))
	}
	ids := make([]string, 0, len(out.Policies))
	for _, p := range out.Policies {
		ids = append(ids, p.ID)
	}
	return ids, nil
}

// DeletePolicy deletes an access policy (404 is success).
func (c *Client) DeletePolicy(ctx context.Context, policyID string) error {
	if err := c.del(ctx, c.iamURL+"/v1/policies/"+url.PathEscape(policyID)); err != nil {
		return fmt.Errorf("deleting policy %s: %w", policyID, friendlyAuthErr(err))
	}
	return nil
}

// DeleteTrustedProfile deletes a trusted profile's access policies explicitly
// and then the profile, which takes its links with it. 404 is success at each
// step, so a partial earlier teardown is finished rather than failed.
func (c *Client) DeleteTrustedProfile(ctx context.Context, profileID string) error {
	if profileID == "" {
		return errors.New("trusted profile id is empty")
	}
	policies, err := c.ListProfilePolicies(ctx, profileID)
	if err != nil {
		return err
	}
	for _, id := range policies {
		if err := c.DeletePolicy(ctx, id); err != nil {
			return err
		}
	}
	if err := c.del(ctx, c.iamURL+"/v1/profiles/"+url.PathEscape(profileID)); err != nil {
		return fmt.Errorf("deleting trusted profile %s: %w", profileID, friendlyAuthErr(err))
	}
	return nil
}
