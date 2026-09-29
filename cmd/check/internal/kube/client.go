// Package kube is a deliberately tiny Kubernetes REST client for the check
// binary.
//
// WHY NOT client-go. The check image is a static binary on `scratch`, pulled on
// every node by a DaemonSet and on every sync by a hook. client-go and its
// transitive apimachinery tree would multiply the binary several times over to
// provide a typed API this program uses a dozen verbs of. Every object here is
// an untyped JSON map, which is also what a CRD client would have to be anyway.
//
// The base URL and the *http.Client are injectable so every caller can be driven
// against an httptest server; InCluster is only the production constructor.
package kube

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// Paths of the projected ServiceAccount credentials.
const (
	saDir           = "/var/run/secrets/kubernetes.io/serviceaccount"
	saTokenFile     = saDir + "/token"
	saCAFile        = saDir + "/ca.crt"
	saNamespaceFile = saDir + "/namespace"
)

// Content types for PATCH.
const (
	MergePatch     = "application/merge-patch+json"
	StrategicPatch = "application/strategic-merge-patch+json"
	ApplyPatch     = "application/apply-patch+yaml" // JSON is valid YAML
)

// Client talks to one API server.
type Client struct {
	// Base is the API server URL, e.g. https://172.21.0.1:443. No trailing slash.
	Base string
	// HTTP performs the requests. Its transport carries the cluster CA.
	HTTP *http.Client
	// Token returns the bearer token for each request. It is a function, not a
	// string, because bound ServiceAccount tokens are rotated by the kubelet in
	// place: a hook that runs for 20 minutes must re-read the file, not reuse
	// what it read at start. nil means no Authorization header.
	Token func() (string, error)
	// UserAgent is sent on every request.
	UserAgent string
}

// New returns a client for base using httpClient (http.DefaultClient if nil)
// and a static token (none if empty). It is what tests use.
func New(base string, httpClient *http.Client, token string) *Client {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	c := &Client{Base: strings.TrimRight(base, "/"), HTTP: httpClient, UserAgent: "roksbnkargoctl-check"}
	if token != "" {
		c.Token = func() (string, error) { return token, nil }
	}
	return c
}

// InCluster builds a client from the pod's ServiceAccount and the
// KUBERNETES_SERVICE_HOST/PORT environment.
func InCluster() (*Client, error) {
	host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" || port == "" {
		return nil, errors.New("KUBERNETES_SERVICE_HOST/PORT are not set: the check binary only runs inside a cluster")
	}
	caPEM, err := os.ReadFile(saCAFile)
	if err != nil {
		return nil, fmt.Errorf("reading the ServiceAccount CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("%s holds no PEM certificate", saCAFile)
	}
	tr := &http.Transport{
		Proxy:               nil, // in-cluster traffic never goes through a proxy
		TLSClientConfig:     &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
		DialContext:         (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout: 10 * time.Second,
		MaxIdleConnsPerHost: 4,
		IdleConnTimeout:     90 * time.Second,
	}
	c := &Client{
		Base:      "https://" + net.JoinHostPort(host, port),
		HTTP:      &http.Client{Transport: tr, Timeout: 60 * time.Second},
		UserAgent: "roksbnkargoctl-check",
		Token: func() (string, error) {
			b, err := os.ReadFile(saTokenFile)
			if err != nil {
				return "", fmt.Errorf("reading the ServiceAccount token: %w", err)
			}
			return strings.TrimSpace(string(b)), nil
		},
	}
	return c, nil
}

// PodNamespace is the namespace this pod runs in, from the ServiceAccount mount.
func PodNamespace() string {
	b, err := os.ReadFile(saNamespaceFile)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// StatusError is a non-2xx answer from the API server.
type StatusError struct {
	Code    int
	Reason  string
	Message string
	Method  string
	Path    string
}

func (e *StatusError) Error() string {
	msg := e.Message
	if msg == "" {
		msg = http.StatusText(e.Code)
	}
	return fmt.Sprintf("%s %s: HTTP %d %s: %s", e.Method, e.Path, e.Code, e.Reason, msg)
}

func code(err error) int {
	var se *StatusError
	if errors.As(err, &se) {
		return se.Code
	}
	return 0
}

// IsNotFound reports a 404.
func IsNotFound(err error) bool { return code(err) == http.StatusNotFound }

// IsForbidden reports a 403.
func IsForbidden(err error) bool { return code(err) == http.StatusForbidden }

// IsConflict reports a 409.
func IsConflict(err error) bool { return code(err) == http.StatusConflict }

// IsAlreadyExists reports a 409 with reason AlreadyExists.
func IsAlreadyExists(err error) bool {
	var se *StatusError
	return errors.As(err, &se) && se.Code == http.StatusConflict && se.Reason == "AlreadyExists"
}

// Path builds a REST path. group "" is the core API.
// ns "" is cluster scope (or all namespaces for a list); name "" is the collection.
func Path(group, version, resource, ns, name string) string {
	var b strings.Builder
	if group == "" {
		b.WriteString("/api/" + version)
	} else {
		b.WriteString("/apis/" + group + "/" + version)
	}
	if ns != "" {
		b.WriteString("/namespaces/" + url.PathEscape(ns))
	}
	b.WriteString("/" + resource)
	if name != "" {
		b.WriteString("/" + url.PathEscape(name))
	}
	return b.String()
}

// GVR names a resource type.
type GVR struct {
	Group, Version, Resource string
	Namespaced               bool
	Kind                     string
	// CanDelete is set by discovery when the resource serves the delete verb.
	CanDelete bool
}

// Path is Path() for this resource.
func (g GVR) Path(ns, name string) string { return Path(g.Group, g.Version, g.Resource, ns, name) }

func (g GVR) String() string {
	if g.Group == "" {
		return g.Resource
	}
	return g.Resource + "." + g.Group
}

// Do performs one request. body may be nil; out may be nil. query may be nil.
func (c *Client) Do(ctx context.Context, method, path string, query url.Values, contentType string, body []byte, out any) error {
	u := c.Base + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if c.UserAgent != "" {
		req.Header.Set("User-Agent", c.UserAgent)
	}
	if c.Token != nil {
		tok, err := c.Token()
		if err != nil {
			return err
		}
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		se := &StatusError{Code: resp.StatusCode, Method: method, Path: path}
		var st struct {
			Reason  string `json:"reason"`
			Message string `json:"message"`
		}
		if json.Unmarshal(data, &st) == nil {
			se.Reason, se.Message = st.Reason, st.Message
		}
		if se.Message == "" {
			se.Message = strings.TrimSpace(string(data))
		}
		return se
	}
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("%s %s: decoding response: %w", method, path, err)
		}
	}
	return nil
}

// Get reads one object.
func (c *Client) Get(ctx context.Context, path string) (Object, error) {
	var o Object
	if err := c.Do(ctx, http.MethodGet, path, nil, "", nil, &o); err != nil {
		return nil, err
	}
	return o, nil
}

// ListOptions narrows a list.
type ListOptions struct {
	LabelSelector string
	FieldSelector string
	// Limit is the page size; 0 means 500. Pages are followed transparently.
	Limit int
}

// List reads a whole collection, following `continue` tokens.
func (c *Client) List(ctx context.Context, path string, opts ListOptions) ([]Object, error) {
	var out []Object
	cont := ""
	limit := opts.Limit
	if limit <= 0 {
		limit = 500
	}
	for {
		q := url.Values{}
		if opts.LabelSelector != "" {
			q.Set("labelSelector", opts.LabelSelector)
		}
		if opts.FieldSelector != "" {
			q.Set("fieldSelector", opts.FieldSelector)
		}
		q.Set("limit", fmt.Sprint(limit))
		if cont != "" {
			q.Set("continue", cont)
		}
		var page struct {
			Metadata struct {
				Continue string `json:"continue"`
			} `json:"metadata"`
			Items []Object `json:"items"`
		}
		if err := c.Do(ctx, http.MethodGet, path, q, "", nil, &page); err != nil {
			return nil, err
		}
		out = append(out, page.Items...)
		if page.Metadata.Continue == "" {
			return out, nil
		}
		cont = page.Metadata.Continue
	}
}

// Create POSTs obj to the collection path.
func (c *Client) Create(ctx context.Context, collectionPath string, obj any) (Object, error) {
	body, err := json.Marshal(obj)
	if err != nil {
		return nil, err
	}
	var o Object
	if err := c.Do(ctx, http.MethodPost, collectionPath, nil, "application/json", body, &o); err != nil {
		return nil, err
	}
	return o, nil
}

// Patch sends patch with the given content type (MergePatch, StrategicPatch).
func (c *Client) Patch(ctx context.Context, path, contentType string, patch any) (Object, error) {
	body, err := json.Marshal(patch)
	if err != nil {
		return nil, err
	}
	var o Object
	if err := c.Do(ctx, http.MethodPatch, path, nil, contentType, body, &o); err != nil {
		return nil, err
	}
	return o, nil
}

// Apply is a server-side apply of obj to its item path as fieldManager. force
// takes ownership of fields another manager holds.
func (c *Client) Apply(ctx context.Context, path string, obj any, fieldManager string, force bool) (Object, error) {
	body, err := json.Marshal(obj)
	if err != nil {
		return nil, err
	}
	q := url.Values{"fieldManager": {fieldManager}}
	if force {
		q.Set("force", "true")
	}
	var o Object
	if err := c.Do(ctx, http.MethodPatch, path, q, ApplyPatch, body, &o); err != nil {
		return nil, err
	}
	return o, nil
}

// Delete deletes one object. propagation is "", "Background", "Foreground" or
// "Orphan".
func (c *Client) Delete(ctx context.Context, path, propagation string) error {
	var body []byte
	ct := ""
	if propagation != "" {
		body, _ = json.Marshal(map[string]any{
			"apiVersion":        "v1",
			"kind":              "DeleteOptions",
			"propagationPolicy": propagation,
		})
		ct = "application/json"
	}
	return c.Do(ctx, http.MethodDelete, path, nil, ct, body, nil)
}

// DeleteIfExists is Delete with 404 treated as success. It reports whether
// something was actually deleted.
func (c *Client) DeleteIfExists(ctx context.Context, path, propagation string) (bool, error) {
	err := c.Delete(ctx, path, propagation)
	if err == nil {
		return true, nil
	}
	if IsNotFound(err) {
		return false, nil
	}
	return false, err
}

// Exists reports whether a GET of path succeeds; 404 is (false, nil).
func (c *Client) Exists(ctx context.Context, path string) (bool, error) {
	_, err := c.Get(ctx, path)
	if err == nil {
		return true, nil
	}
	if IsNotFound(err) {
		return false, nil
	}
	return false, err
}
