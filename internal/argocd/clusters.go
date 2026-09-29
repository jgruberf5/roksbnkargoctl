package argocd

import (
	"context"
	"net/url"
	"strings"
)

// Cluster is what the tool registers: a bearer-token cluster.
type Cluster struct {
	Name        string
	Server      string
	BearerToken string
	CAData      []byte // PEM
	Insecure    bool
	Labels      map[string]string
}

// ClusterInfo is the subset of Argo CD's v1alpha1.Cluster read back.
type ClusterInfo struct {
	Name            string            `json:"name"`
	Server          string            `json:"server"`
	Project         string            `json:"project,omitempty"`
	Labels          map[string]string `json:"labels,omitempty"`
	ConnectionState struct {
		Status  string `json:"status,omitempty"`
		Message string `json:"message,omitempty"`
	} `json:"connectionState"`
	ServerVersion string `json:"serverVersion,omitempty"`
}

type tlsClientConfig struct {
	Insecure bool   `json:"insecure,omitempty"`
	CAData   []byte `json:"caData,omitempty"` // swagger: string, format byte (base64) — encoding/json does that
}

type clusterConfig struct {
	BearerToken     string          `json:"bearerToken,omitempty"`
	TLSClientConfig tlsClientConfig `json:"tlsClientConfig"`
}

type clusterBody struct {
	Name   string            `json:"name"`
	Server string            `json:"server"`
	Config clusterConfig     `json:"config"`
	Labels map[string]string `json:"labels,omitempty"`
}

// UpsertCluster registers (or updates) a cluster: POST /api/v1/clusters?upsert=true.
func (c *Client) UpsertCluster(ctx context.Context, cl Cluster) (*ClusterInfo, error) {
	body := clusterBody{
		Name:   cl.Name,
		Server: cl.Server,
		Config: clusterConfig{
			BearerToken:     cl.BearerToken,
			TLSClientConfig: tlsClientConfig{Insecure: cl.Insecure, CAData: cl.CAData},
		},
		Labels: cl.Labels,
	}
	var out ClusterInfo
	if err := c.do(ctx, "register cluster "+cl.Server, "POST", "/api/v1/clusters?upsert=true", body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetCluster returns the cluster registered for server. It lists with ?server= rather
// than GET /api/v1/clusters/{server}, because Argo CD answers that for an unknown
// cluster with 403, not 404 (server/cluster getClusterWith403IfNotExist); a missing
// cluster here yields an error for which IsNotFound is true.
func (c *Client) GetCluster(ctx context.Context, server string) (*ClusterInfo, error) {
	var list struct {
		Items []ClusterInfo `json:"items"`
	}
	op := "read cluster " + server
	if err := c.do(ctx, op, "GET", "/api/v1/clusters?server="+url.QueryEscape(server), nil, &list); err != nil {
		return nil, err
	}
	want := strings.TrimRight(server, "/")
	for i := range list.Items {
		if strings.TrimRight(list.Items[i].Server, "/") == want {
			return &list.Items[i], nil
		}
	}
	return nil, notFound(op, "cluster "+server+" is not registered")
}

// DeleteCluster removes the registration for server (DELETE /api/v1/clusters/{server}).
// A missing cluster returns an error for which IsNotFound is true.
func (c *Client) DeleteCluster(ctx context.Context, server string) error {
	if _, err := c.GetCluster(ctx, server); err != nil {
		return err
	}
	return c.do(ctx, "delete cluster "+server, "DELETE",
		"/api/v1/clusters/"+pathEscape(server), nil, nil)
}
