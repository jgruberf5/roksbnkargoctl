package ibm

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// FetchAdminKubeconfig downloads the admin kubeconfig for a cluster and returns
// it SELF-CONTAINED: the client certificate, key and cluster CA are inlined as
// *-data fields, so the result needs no companion .pem files.
//
// private selects the cluster's private service endpoint as the kubeconfig's
// server (endpointType "private") — the endpoint an Argo CD hub reaches over
// the transit gateway. Otherwise the service's default endpoint is used.
//
// This is what `ibmcloud ks cluster config --admin` does on the wire: POST
// /v2/applyRBACAndGetKubeconfig, unpack the ZIP. HTTP 404 and 503 are retried:
// a just-created cluster reaches the kubeconfig endpoint a minute or two after
// it reports ready.
func (c *Client) FetchAdminKubeconfig(ctx context.Context, nameOrID string, private bool) ([]byte, error) {
	if strings.TrimSpace(nameOrID) == "" {
		return nil, errors.New("cluster name/id is empty")
	}
	attempts := c.kubeconfigAttempts
	if attempts < 1 {
		attempts = 1
	}
	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		out, err := c.fetchKubeconfigOnce(ctx, nameOrID, private)
		if err == nil {
			return out, nil
		}
		lastErr = err
		if s := statusOf(err); s != http.StatusNotFound && s != http.StatusServiceUnavailable {
			return nil, err
		}
		if attempt == attempts {
			break
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(c.kubeconfigRetryWait):
		}
	}
	return nil, fmt.Errorf("admin kubeconfig for %q still unavailable after %d attempts: %w", nameOrID, attempts, lastErr)
}

func (c *Client) fetchKubeconfigOnce(ctx context.Context, nameOrID string, private bool) ([]byte, error) {
	endpoint := ""
	if private {
		endpoint = "private"
	}
	body := map[string]any{
		"cluster":      nameOrID,
		"format":       "zip",
		"admin":        true,
		"network":      false,
		"endpointType": endpoint,
	}
	raw, err := c.doRaw(ctx, http.MethodPost, c.containersURL+"/v2/applyRBACAndGetKubeconfig", c.containerHeaders(), body)
	if err != nil {
		return nil, fmt.Errorf("fetching admin kubeconfig for %q: %w", nameOrID, err)
	}
	if !isZIP(raw) {
		// The contract is a ZIP; accept a bare YAML rather than fail if IBM
		// ever changes that, but still inline anything it references.
		return inlineCertRefs(raw, nil)
	}
	return buildSelfContainedKubeconfig(raw)
}

func isZIP(b []byte) bool {
	return len(b) >= 4 && b[0] == 'P' && b[1] == 'K' && b[2] == 0x03 && b[3] == 0x04
}

// buildSelfContainedKubeconfig parses the admin ZIP (kube-config YAML plus the
// admin.pem / admin-key.pem / CA files) and returns the YAML with every file
// reference inlined.
func buildSelfContainedKubeconfig(zipBytes []byte) ([]byte, error) {
	r, err := zip.NewReader(bytes.NewReader(zipBytes), int64(len(zipBytes)))
	if err != nil {
		return nil, fmt.Errorf("opening admin kubeconfig zip: %w", err)
	}
	var kubeconfig []byte
	files := map[string][]byte{}
	for _, f := range r.File {
		base := f.Name
		if i := strings.LastIndex(base, "/"); i >= 0 {
			base = base[i+1:]
		}
		if base == "" {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return nil, fmt.Errorf("opening %s in zip: %w", f.Name, err)
		}
		b, err := io.ReadAll(io.LimitReader(rc, 16<<20))
		_ = rc.Close()
		if err != nil {
			return nil, fmt.Errorf("reading %s from zip: %w", f.Name, err)
		}
		files[base] = b
		isYAML := strings.HasPrefix(base, "kube-config") || strings.HasSuffix(base, ".yml") || strings.HasSuffix(base, ".yaml")
		if isYAML && kubeconfig == nil {
			kubeconfig = b
		}
	}
	if kubeconfig == nil {
		return nil, errors.New("no kubeconfig YAML in the admin kubeconfig zip")
	}
	return inlineCertRefs(kubeconfig, files)
}

// inlineCertRefs converts client-certificate / client-key /
// certificate-authority file references into their *-data forms using files.
// A reference whose file is not in the archive is an error: leaving it would
// produce a kubeconfig that silently depends on a file nobody has.
func inlineCertRefs(kubeconfig []byte, files map[string][]byte) ([]byte, error) {
	var doc map[string]any
	if err := yaml.Unmarshal(kubeconfig, &doc); err != nil {
		return nil, fmt.Errorf("parsing kubeconfig: %w", err)
	}
	users, _ := doc["users"].([]any)
	for _, u := range users {
		inner := subMap(u, "user")
		if err := swapCertField(inner, "client-certificate", "client-certificate-data", files); err != nil {
			return nil, err
		}
		if err := swapCertField(inner, "client-key", "client-key-data", files); err != nil {
			return nil, err
		}
	}
	clusters, _ := doc["clusters"].([]any)
	for _, cl := range clusters {
		if err := swapCertField(subMap(cl, "cluster"), "certificate-authority", "certificate-authority-data", files); err != nil {
			return nil, err
		}
	}
	out, err := yaml.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("re-emitting kubeconfig: %w", err)
	}
	return out, nil
}

func subMap(v any, key string) map[string]any {
	m, _ := v.(map[string]any)
	if m == nil {
		return nil
	}
	inner, _ := m[key].(map[string]any)
	return inner
}

func swapCertField(inner map[string]any, refKey, dataKey string, files map[string][]byte) error {
	if inner == nil {
		return nil
	}
	ref, ok := inner[refKey].(string)
	if !ok || ref == "" {
		return nil
	}
	base := ref
	if i := strings.LastIndexAny(base, `/\`); i >= 0 {
		base = base[i+1:]
	}
	b, ok := files[base]
	if !ok {
		return fmt.Errorf("kubeconfig references %s %q, which is not in the admin archive", refKey, ref)
	}
	delete(inner, refKey)
	inner[dataKey] = base64.StdEncoding.EncodeToString(b)
	return nil
}
