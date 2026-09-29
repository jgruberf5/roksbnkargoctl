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

// Cluster is the subset of the Kubernetes Service getCluster response that
// roksbnkargoctl uses.
type Cluster struct {
	ID                string
	Name              string
	Region            string
	ResourceGroupID   string
	ResourceGroupName string
	State             string
	Type              string // "openshift" for ROKS
	CRN               string
	MasterURL         string
	// PrivateServiceEndpointURL / PublicServiceEndpointURL are the API server
	// endpoints; either is empty when that endpoint is disabled.
	PrivateServiceEndpointURL string
	PublicServiceEndpointURL  string
	VPCIDs                    []string
	WorkerZones               []string
	// MasterKubeVersion is as reported, e.g. "4.16.20_openshift".
	MasterKubeVersion string
	// OpenShiftVersion is MasterKubeVersion without the "_openshift" suffix
	// ("4.16.20"); empty for a non-OpenShift cluster.
	OpenShiftVersion string
}

// VPCID returns the cluster's first VPC, or "".
func (cl *Cluster) VPCID() string {
	if cl == nil || len(cl.VPCIDs) == 0 {
		return ""
	}
	return cl.VPCIDs[0]
}

// IsOpenShift reports whether the cluster is ROKS.
func (cl *Cluster) IsOpenShift() bool {
	return strings.EqualFold(cl.Type, "openshift") ||
		strings.Contains(strings.ToLower(cl.MasterKubeVersion), "openshift")
}

// ErrClusterNotFound is returned when the Kubernetes Service has no cluster by
// that name or id.
var ErrClusterNotFound = errors.New("cluster not found")

// clusterWire is the wire shape. The service reports the endpoints both at the
// top level (v1-compatible) and under serviceEndpoints (v2); both are read.
type clusterWire struct {
	ID                        string   `json:"id"`
	Name                      string   `json:"name"`
	Region                    string   `json:"region"`
	ResourceGroup             string   `json:"resourceGroup"`
	ResourceGroupName         string   `json:"resourceGroupName"`
	State                     string   `json:"state"`
	Type                      string   `json:"type"`
	CRN                       string   `json:"crn"`
	MasterURL                 string   `json:"masterURL"`
	MasterKubeVersion         string   `json:"masterKubeVersion"`
	PrivateServiceEndpointURL string   `json:"privateServiceEndpointURL"`
	PublicServiceEndpointURL  string   `json:"publicServiceEndpointURL"`
	VPCs                      []string `json:"vpcs"`
	WorkerZones               []string `json:"workerZones"`
	ServiceEndpoints          struct {
		PrivateServiceEndpointEnabled bool   `json:"privateServiceEndpointEnabled"`
		PrivateServiceEndpointURL     string `json:"privateServiceEndpointURL"`
		PublicServiceEndpointEnabled  bool   `json:"publicServiceEndpointEnabled"`
		PublicServiceEndpointURL      string `json:"publicServiceEndpointURL"`
	} `json:"serviceEndpoints"`
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}

func (w clusterWire) toCluster() *Cluster {
	cl := &Cluster{
		ID:                        w.ID,
		Name:                      w.Name,
		Region:                    w.Region,
		ResourceGroupID:           w.ResourceGroup,
		ResourceGroupName:         w.ResourceGroupName,
		State:                     w.State,
		Type:                      w.Type,
		CRN:                       w.CRN,
		MasterURL:                 w.MasterURL,
		MasterKubeVersion:         w.MasterKubeVersion,
		PrivateServiceEndpointURL: firstNonEmpty(w.ServiceEndpoints.PrivateServiceEndpointURL, w.PrivateServiceEndpointURL),
		PublicServiceEndpointURL:  firstNonEmpty(w.ServiceEndpoints.PublicServiceEndpointURL, w.PublicServiceEndpointURL),
		VPCIDs:                    w.VPCs,
		WorkerZones:               w.WorkerZones,
	}
	if v, ok := strings.CutSuffix(strings.ToLower(w.MasterKubeVersion), "_openshift"); ok {
		cl.OpenShiftVersion = v
	}
	return cl
}

// GetCluster fetches a cluster by name or id — the call `ibmcloud ks cluster
// get` makes. Returns ErrClusterNotFound on 404.
func (c *Client) GetCluster(ctx context.Context, nameOrID string) (*Cluster, error) {
	if strings.TrimSpace(nameOrID) == "" {
		return nil, errors.New("cluster name/id is empty")
	}
	u := c.containersURL + "/v2/getCluster?cluster=" + url.QueryEscape(nameOrID)
	var w clusterWire
	err := c.do(ctx, http.MethodGet, u, c.containerHeaders(), nil, &w)
	if IsNotFound(err) {
		return nil, fmt.Errorf("%w: %q", ErrClusterNotFound, nameOrID)
	}
	if err != nil {
		return nil, fmt.Errorf("getting cluster %q: %w", nameOrID, err)
	}
	return w.toCluster(), nil
}

// ListClusters returns the account's OpenShift VPC clusters, sorted by name.
func (c *Client) ListClusters(ctx context.Context) ([]Cluster, error) {
	var all []clusterWire
	if err := c.do(ctx, http.MethodGet, c.containersURL+"/v2/vpc/getClusters", c.containerHeaders(), nil, &all); err != nil {
		return nil, fmt.Errorf("listing clusters: %w", err)
	}
	var out []Cluster
	for _, w := range all {
		cl := w.toCluster()
		if cl.IsOpenShift() {
			out = append(out, *cl)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (c *Client) containerHeaders() map[string]string {
	if c.region == "" {
		return nil
	}
	return map[string]string{"X-Region": c.region}
}
