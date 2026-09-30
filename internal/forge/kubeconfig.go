package forge

import (
	"errors"
	"fmt"

	"gopkg.in/yaml.v3"
)

// CertKubeconfig builds the kubeconfig Forge registers a ROKS cluster with
// from an admin kubeconfig whose certificates are already inlined (what
// ibm.Client.FetchAdminKubeconfig returns): one cluster (the server, plus
// certificate-authority-data only if the source has one), one user carrying
// the admin client certificate and key, and one context. It has no file
// references and no token.
//
// It is roksbnkctl's k8s.BuildCertKubeconfig, which produces what roksbnkctl
// sends. ROKS is OpenShift: its API server authenticates client certificates or
// OpenShift OAuth tokens and rejects IBM IAM bearer tokens with 401, so a token
// kubeconfig registers and then cannot connect. An admin kubeconfig without an
// inlined client certificate and key is therefore an error.
//
// The cluster and user are the current context's, falling back to the first
// entries. certificate-authority-data is optional: ROKS public masters present
// a publicly trusted certificate, and an empty value would read as an empty CA
// bundle.
func CertKubeconfig(adminKubeconfig []byte, userName string) ([]byte, error) {
	var doc map[string]any
	if err := yaml.Unmarshal(adminKubeconfig, &doc); err != nil {
		return nil, fmt.Errorf("parsing admin kubeconfig: %w", err)
	}
	ctxCluster, ctxUser, ns := currentContext(doc)

	c0 := pickNamed(doc["clusters"], ctxCluster)
	if c0 == nil {
		return nil, errors.New("admin kubeconfig has no clusters")
	}
	clusterName, _ := c0["name"].(string)
	if clusterName == "" {
		clusterName = "cluster"
	}
	inner, _ := c0["cluster"].(map[string]any)
	if inner == nil {
		return nil, errors.New("admin kubeconfig: cluster entry has no `cluster` block")
	}
	server, _ := inner["server"].(string)
	if server == "" {
		return nil, errors.New("admin kubeconfig: cluster has no server URL")
	}
	caData, _ := inner["certificate-authority-data"].(string)
	if ref, _ := inner["certificate-authority"].(string); ref != "" && caData == "" {
		return nil, fmt.Errorf("admin kubeconfig references a CA file (%s) instead of inlining it", ref)
	}

	u0 := pickNamed(doc["users"], ctxUser)
	if u0 == nil {
		return nil, errors.New("admin kubeconfig has no users")
	}
	uinner, _ := u0["user"].(map[string]any)
	if uinner == nil {
		return nil, errors.New("admin kubeconfig: user has no `user` block")
	}
	cert, _ := uinner["client-certificate-data"].(string)
	key, _ := uinner["client-key-data"].(string)
	if cert == "" || key == "" {
		return nil, errors.New("admin kubeconfig has no inlined client certificate and key (client-certificate-data, client-key-data); " +
			"Forge needs certificate authentication, because ROKS rejects IBM IAM tokens")
	}

	if userName == "" {
		userName = clusterName + "-admin"
	}
	if ns == "" {
		ns = "default"
	}
	clusterBlock := map[string]any{"server": server}
	if caData != "" {
		clusterBlock["certificate-authority-data"] = caData
	}
	out := map[string]any{
		"apiVersion": "v1",
		"kind":       "Config",
		"clusters":   []any{map[string]any{"name": clusterName, "cluster": clusterBlock}},
		"contexts": []any{map[string]any{
			"name":    clusterName,
			"context": map[string]any{"cluster": clusterName, "user": userName, "namespace": ns},
		}},
		"current-context": clusterName,
		"users": []any{map[string]any{
			"name": userName,
			"user": map[string]any{"client-certificate-data": cert, "client-key-data": key},
		}},
	}
	b, err := yaml.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("emitting the Forge kubeconfig: %w", err)
	}
	return b, nil
}

// currentContext returns the current context's cluster, user and namespace
// ("" for any that is not set).
func currentContext(doc map[string]any) (cluster, user, ns string) {
	cur, _ := doc["current-context"].(string)
	if cur == "" {
		return "", "", ""
	}
	c := pickNamed(doc["contexts"], cur)
	if c == nil || c["name"] != cur {
		return "", "", ""
	}
	inner, _ := c["context"].(map[string]any)
	cluster, _ = inner["cluster"].(string)
	user, _ = inner["user"].(string)
	ns, _ = inner["namespace"].(string)
	return cluster, user, ns
}

// pickNamed returns the entry of list named name, or the first entry when
// name is "" or absent; nil when the list is empty.
func pickNamed(list any, name string) map[string]any {
	items, _ := list.([]any)
	var first map[string]any
	for _, it := range items {
		m, _ := it.(map[string]any)
		if m == nil {
			continue
		}
		if first == nil {
			first = m
		}
		if name != "" && m["name"] == name {
			return m
		}
	}
	return first
}
