package forge

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
	"sync"
	"time"
)

// Client talks to a BNK Forge v3 server. The zero value is not usable; use New.
type Client struct {
	BaseURL string
	Token   string
	// Log receives warnings (nil is os.Stderr).
	Log io.Writer

	http *http.Client
	// insecure records that TLS verification is off, so the first request can
	// say so. Warning at construction would miss the case that matters: a
	// setting enabled once for a lab and forgotten when the same configuration
	// is pointed at a production Forge.
	insecure bool
	warnOnce sync.Once
}

// Options configures the transport's trust.
type Options struct {
	// CAPEM pins the certificate authority the Forge server's certificate must
	// chain to. For a self-signed lab install this authenticates the connection
	// instead of abandoning authentication.
	CAPEM []byte
	// Insecure disables verification entirely. Ignored when CAPEM is set.
	Insecure bool
}

// New returns a Client for baseURL.
//
// Verification precedence: a pinned CA, else the system roots, else (only with
// Insecure) nothing. The pool built from CAPEM is exclusive on purpose: this
// client talks to exactly one server, and pinning is the point.
func New(baseURL string, opts Options) (*Client, error) {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	switch {
	case len(opts.CAPEM) > 0:
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(opts.CAPEM) {
			return nil, errors.New("BNK Forge CA: no certificate found in the supplied PEM")
		}
		tr.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	case opts.Insecure:
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // opt-in; the first request warns, see warnInsecure
	}
	return &Client{
		BaseURL:  strings.TrimRight(baseURL, "/"),
		insecure: len(opts.CAPEM) == 0 && opts.Insecure,
		http:     &http.Client{Timeout: 60 * time.Second, Transport: tr},
	}, nil
}

func (c *Client) log() io.Writer {
	if c.Log == nil {
		return os.Stderr
	}
	return c.Log
}

func (c *Client) warnf(format string, a ...any) { fmt.Fprintf(c.log(), format, a...) }

// warnInsecure says, once per client, that credentials are sent over an
// unauthenticated connection. It fires from do rather than New, so it attaches
// to actual transmission: a client that is built and never used disclosed
// nothing.
func (c *Client) warnInsecure() {
	if !c.insecure {
		return
	}
	c.warnOnce.Do(func() {
		host := c.BaseURL
		if u, err := url.Parse(c.BaseURL); err == nil && u.Host != "" {
			host = u.Host
		}
		w := c.log()
		fmt.Fprintf(w, "⚠ BNK Forge TLS verification is DISABLED for %s: the password and the API token are sent over a connection that is encrypted but NOT authenticated.\n", host)
		fmt.Fprintln(w, "  Anyone able to intercept it can present any certificate for that host and read them.")
		if isPublicIP(host) {
			fmt.Fprintf(w, "  %s is a public address, so this connection leaves your network.\n", host)
		}
		fmt.Fprintln(w, "  Pin the Forge CA instead (forge.ca_file, --ca-file) and drop insecure.")
	})
}

// isPublicIP reports whether host is an IP literal routable on the public
// internet. A hostname returns false: it cannot be classified without resolving
// it, so the warning escalates only where the answer is certain.
func isPublicIP(host string) bool {
	h := host
	if hp, _, err := net.SplitHostPort(host); err == nil {
		h = hp
	} else if strings.HasPrefix(h, "[") && strings.HasSuffix(h, "]") {
		// A bracketed IPv6 literal without a port fails SplitHostPort, and
		// ParseIP does not accept the brackets.
		h = h[1 : len(h)-1]
	}
	ip := net.ParseIP(h)
	if ip == nil {
		return false
	}
	return !ip.IsPrivate() && !ip.IsLoopback() && !ip.IsLinkLocalUnicast() &&
		!ip.IsUnspecified() && !ip.IsMulticast()
}

// do issues a request; body (if non-nil) is JSON-encoded. It returns the raw
// response body and status code.
func (c *Client) do(ctx context.Context, method, path string, body any) ([]byte, int, error) {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, 0, fmt.Errorf("encoding request body: %w", err)
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, r)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "roksbnkargoctl")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	c.warnInsecure()
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("reading response: %w", err)
	}
	return data, resp.StatusCode, nil
}

func ok(code int) bool { return code >= 200 && code < 300 }

func httpErr(method, path string, code int, body []byte) error {
	s := strings.TrimSpace(string(body))
	if len(s) > 2048 {
		s = s[:2048]
	}
	return fmt.Errorf("%s %s → HTTP %d: %s", method, path, code, s)
}

// numField returns the first of keys in m whose value is a non-zero JSON
// number. Forge's create responses are inconsistent about the id field name
// (id, project_id, cluster_id), so callers pass every plausible name.
func numField(m map[string]any, keys ...string) int {
	for _, k := range keys {
		if f, ok := m[k].(float64); ok && f != 0 {
			return int(f)
		}
	}
	return 0
}

// createdID extracts a created object's id from a response body, at the top
// level or under a wrapper object (e.g. {"project":{...}}).
func createdID(body []byte, wrapper string, keys ...string) int {
	var m map[string]any
	if json.Unmarshal(body, &m) != nil {
		return 0
	}
	if id := numField(m, keys...); id != 0 {
		return id
	}
	if wrapper != "" {
		if w, ok := m[wrapper].(map[string]any); ok {
			return numField(w, keys...)
		}
	}
	return 0
}

// truncateBody renders a short, single-line excerpt of a response body for an
// error message.
func truncateBody(b []byte) string {
	s := strings.TrimSpace(string(b))
	if s == "" {
		return "<empty>"
	}
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 160 {
		return s[:160] + "…"
	}
	return s
}

// Login exchanges a username and password for a session token and stores it on c.
func (c *Client) Login(ctx context.Context, username, password string) error {
	data, code, err := c.do(ctx, http.MethodPost, "/api/auth/login",
		map[string]string{"username": username, "password": password})
	if err != nil {
		return err
	}
	if !ok(code) {
		return httpErr("POST", "/api/auth/login", code, data)
	}
	var r struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(data, &r); err != nil {
		return fmt.Errorf("parsing login response: %w", err)
	}
	if r.Token == "" {
		return errors.New("login succeeded but returned no token")
	}
	c.Token = r.Token
	return nil
}

// ---- credential template ------------------------------------------------------

type credentialTemplate struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

// IBMCredentialTemplate is what a Forge IBM credential template needs to be
// useful, as opposed to merely present.
type IBMCredentialTemplate struct {
	Name          string
	APIKey        string
	ResourceGroup string
	// Region and COSInstance are inherited by blueprint inputs declared with
	// `source: credential_template`; left empty they are not sent, so a value
	// set by hand in Forge is not overwritten with an empty one.
	Region      string
	COSInstance string
}

// ibmProvider is the value Forge's code compares against, case-sensitively.
// "IBM" is accepted by the API and then matches nothing (roksbnkctl #223).
const ibmProvider = "ibm"

// EnsureIBMCredentialTemplate returns the id of the IBM credential template
// named tmpl.Name, creating it, or updating the existing one, so it holds the
// given API key and resource group and is marked default.
func (c *Client) EnsureIBMCredentialTemplate(ctx context.Context, tmpl IBMCredentialTemplate) (int, error) {
	const p = "/api/credential-templates"
	data, code, err := c.do(ctx, http.MethodGet, p, nil)
	if err != nil {
		return 0, err
	}
	if !ok(code) {
		return 0, httpErr("GET", p, code, data)
	}
	var existing []credentialTemplate
	if err := json.Unmarshal(data, &existing); err != nil {
		// Reading an unparseable list as "none" would create a duplicate
		// template holding the API key on every run.
		return 0, fmt.Errorf("GET %s returned an unrecognised body (%d bytes): %s", p, len(data), truncateBody(data))
	}

	fields := map[string]any{
		// Sent on update as well as create, so a template written as "IBM" is
		// repaired rather than left inert.
		"provider":                ibmProvider,
		"ibmcloud_api_key":        tmpl.APIKey,
		"ibmcloud_resource_group": tmpl.ResourceGroup,
		"is_default":              true,
	}
	if tmpl.Region != "" {
		fields["region"] = tmpl.Region
	}
	if tmpl.COSInstance != "" {
		fields["ibm_cos_instance_name"] = tmpl.COSInstance
	}
	for _, t := range existing {
		if t.Name == tmpl.Name {
			up := fmt.Sprintf("%s/%d", p, t.ID)
			d, code, err := c.do(ctx, http.MethodPut, up, fields)
			if err != nil {
				return 0, err
			}
			if !ok(code) {
				return 0, httpErr("PUT", up, code, d)
			}
			return t.ID, nil
		}
	}

	create := map[string]any{"name": tmpl.Name}
	for k, v := range fields {
		create[k] = v
	}
	d, code, err := c.do(ctx, http.MethodPost, p, create)
	if err != nil {
		return 0, err
	}
	if !ok(code) {
		return 0, httpErr("POST", p, code, d)
	}
	if id := createdID(d, "credential_template", "id", "template_id", "credential_template_id"); id != 0 {
		return id, nil
	}
	return 0, fmt.Errorf("create credential-template returned no id: %s", truncateBody(d))
}

// ---- projects -------------------------------------------------------------------

type project struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

// listProjects returns every project Forge lists.
//
// Three shapes must be told apart: {"projects":[…]} (with null meaning none), a
// bare array, and a body that does not parse. The third is an error, never an
// empty list: for a teardown it would print "nothing to unregister" while the
// cluster stays registered, and for the ownership scan it is exactly the answer
// that lets a silent takeover through.
func (c *Client) listProjects(ctx context.Context) ([]project, error) {
	const p = "/api/projects"
	data, code, err := c.do(ctx, http.MethodGet, p, nil)
	if err != nil {
		return nil, err
	}
	if !ok(code) {
		return nil, httpErr("GET", p, code, data)
	}
	return parseList[project](data, "projects", "GET "+p)
}

// parseError is a response body that is not the shape Forge documents.
type parseError struct{ msg string }

func (e *parseError) Error() string { return e.msg }

// parseList decodes {"<key>":[…]} or a bare array; anything else is a
// *parseError.
func parseList[T any](data []byte, key, what string) ([]T, error) {
	var probe map[string]json.RawMessage
	var list []T
	switch {
	case json.Unmarshal(data, &probe) == nil:
		raw, has := probe[key]
		if !has {
			return nil, &parseError{fmt.Sprintf("%s returned an object with no %q key (%d bytes): %s", what, key, len(data), truncateBody(data))}
		}
		if err := json.Unmarshal(raw, &list); err != nil {
			return nil, &parseError{fmt.Sprintf("%s: %q is not a list (%d bytes): %s", what, key, len(data), truncateBody(data))}
		}
	case json.Unmarshal(data, &list) == nil:
	default:
		return nil, &parseError{fmt.Sprintf("%s returned an unrecognised body (%d bytes): %s", what, len(data), truncateBody(data))}
	}
	return list, nil
}

// EnsureProject returns the id of the project named name, creating it if
// absent, and sets its target platform to IBM ROKS.
func (c *Client) EnsureProject(ctx context.Context, name string) (int, error) {
	id, err := c.ProjectIDByName(ctx, name)
	if err != nil {
		return 0, err
	}
	if id == 0 {
		d, code, err := c.do(ctx, http.MethodPost, "/api/projects",
			map[string]string{"name": name, "description": "roksbnkargoctl"})
		if err != nil {
			return 0, err
		}
		if !ok(code) {
			return 0, httpErr("POST", "/api/projects", code, d)
		}
		if id = createdID(d, "project", "id", "project_id"); id == 0 {
			return 0, fmt.Errorf("create project returned no id: %s", truncateBody(d))
		}
	}
	c.setProjectPlatform(ctx, id)
	return id, nil
}

// setProjectPlatform marks the project as targeting IBM ROKS, so the Forge UI
// does not show "Target Platform: Unknown". Best-effort: the label is cosmetic
// and older Forge builds may lack these fields.
func (c *Client) setProjectPlatform(ctx context.Context, id int) {
	_, _, _ = c.do(ctx, http.MethodPut, fmt.Sprintf("/api/projects/%d", id), map[string]string{
		"target_platform_profile": "roks",
		"platform_provider":       "ibm",
		"cloud_provider":          "ibm",
	})
}

// ProjectIDByName returns the id of a project, or 0 when there is none of that
// name. It never creates one: a teardown asking "is this still here" must not
// bring it into being.
func (c *Client) ProjectIDByName(ctx context.Context, name string) (int, error) {
	list, err := c.listProjects(ctx)
	if err != nil {
		return 0, err
	}
	for _, p := range list {
		if p.Name == name {
			return p.ID, nil
		}
	}
	return 0, nil
}

// ---- clusters -------------------------------------------------------------------

// RegisterRequest is the body that registers a cluster into a project.
type RegisterRequest struct {
	Name string `json:"name"`
	// Provider is the credential provider ("IBM"). CloudProvider is the
	// platform Forge displays (lowercase "ibm"); without it Forge stores its
	// "on-prem" default and the UI shows the platform as Unknown.
	Provider      string `json:"provider"`
	CloudProvider string `json:"cloud_provider"`
	ClusterID     string `json:"cluster_id"`
	Region        string `json:"region"`
	TemplateID    int    `json:"template_id"`
	// Kubeconfig is base64. Forge connects as soon as the cluster is
	// registered, so it is required.
	Kubeconfig string `json:"kubeconfig"`
}

// ClusterOwner identifies the Forge project holding a cluster.
type ClusterOwner struct {
	ProjectID   int
	ProjectName string
	ClusterID   int
}

// FindClusterOwner looks for a cluster of this name across every project.
// It returns a zero ClusterOwner when nothing holds the name.
//
// A project it cannot read is warned about and skipped: this scan is a second
// opinion layered on the direct query of the target project, and failing
// closed would block a least-privilege account from its own project. A project
// list it cannot parse is an error.
func (c *Client) FindClusterOwner(ctx context.Context, name string) (ClusterOwner, error) {
	list, err := c.listProjects(ctx)
	if err != nil {
		// Unreadable is tolerated; unrecognisable is not.
		var pe *parseError
		if errors.As(err, &pe) {
			return ClusterOwner{}, err
		}
		c.warnf("⚠ cannot list BNK Forge projects to check who holds cluster %q: %v\n"+
			"  Continuing; cross-project ownership was not verified.\n", name, err)
		return ClusterOwner{}, nil
	}
	for _, pr := range list {
		id, cerr := c.projectClusterID(ctx, pr.ID, name)
		var pe *parseError
		if errors.As(cerr, &pe) {
			return ClusterOwner{}, cerr
		}
		if cerr != nil {
			c.warnf("⚠ cannot check BNK Forge project %q (%d) for an existing cluster %q: %v\n"+
				"  Continuing; if that project holds it, this registration will move it.\n",
				pr.Name, pr.ID, name, cerr)
			continue
		}
		if id != 0 {
			return ClusterOwner{ProjectID: pr.ID, ProjectName: pr.Name, ClusterID: id}, nil
		}
	}
	return ClusterOwner{}, nil
}

// ErrClusterOwnedElsewhere is returned when the cluster is registered to a
// different Forge project. Callers surface it as a refusal; force overrides.
var ErrClusterOwnedElsewhere = errors.New("cluster is registered to another BNK Forge project")

// RegisterCluster records a cluster with a Forge project, non-destructively:
//
//   - held by this project: updated in place, keeping its id;
//   - held by another: refused with ErrClusterOwnedElsewhere unless force;
//   - held by nobody: created.
//
// The in-place update falls back to delete-and-create when the server has no
// PUT for clusters (404 or 405 only). It returns the Forge cluster id.
func (c *Client) RegisterCluster(ctx context.Context, projectID int, req RegisterRequest, force bool) (int, error) {
	// Ask the target project directly first. It is the authoritative answer for
	// the common case and does not depend on /api/projects listing anything: a
	// paginated or permission-filtered list that omitted the owner would
	// otherwise send us straight to POST, over the cluster this guard protects.
	owner := ClusterOwner{ProjectID: projectID}
	id, oerr := c.projectClusterID(ctx, projectID, req.Name)
	var pe *parseError
	if errors.As(oerr, &pe) {
		// An unrecognised list is not "not ours": creating on top of it would
		// register a duplicate.
		return 0, oerr
	}
	if oerr == nil && id != 0 {
		owner.ClusterID = id
	} else {
		found, ferr := c.FindClusterOwner(ctx, req.Name)
		if ferr != nil {
			return 0, ferr
		}
		owner = found
	}
	switch {
	case owner.ClusterID != 0 && owner.ProjectID != projectID && !force:
		return 0, fmt.Errorf("%w: %q is held by project %q (id %d, cluster id %d). "+
			"Re-registering would move it, changing its cluster id and removing it from that project's view. "+
			"Pass --force to take it over deliberately, or unregister it there first",
			ErrClusterOwnedElsewhere, req.Name, owner.ProjectName, owner.ProjectID, owner.ClusterID)

	case owner.ClusterID != 0 && owner.ProjectID == projectID:
		up := fmt.Sprintf("/api/k8s/clusters/%d", owner.ClusterID)
		d, code, uerr := c.do(ctx, http.MethodPut, up, req)
		if uerr != nil {
			return 0, fmt.Errorf("updating cluster %q (id %d) in project %d: %w", req.Name, owner.ClusterID, projectID, uerr)
		}
		if ok(code) {
			return owner.ClusterID, nil
		}
		// Only a missing route or method means "this build has no PUT".
		// Anything else (a transient 500) is a real failure: retrying it
		// destructively would lose the id the update exists to keep.
		if code != http.StatusNotFound && code != http.StatusMethodNotAllowed {
			return 0, httpErr("PUT", up, code, d)
		}
		d, code, derr := c.do(ctx, http.MethodDelete, up, nil)
		if derr != nil {
			return 0, derr
		}
		if !ok(code) && code != http.StatusNotFound {
			return 0, httpErr("DELETE", up, code, d)
		}

	case owner.ClusterID != 0:
		// A forced takeover. It must not share the in-place PUT: that body has
		// no project field, so it would update the cluster where it lives and
		// report success while it stayed in the other project.
		dp := fmt.Sprintf("/api/k8s/clusters/%d", owner.ClusterID)
		d, code, derr := c.do(ctx, http.MethodDelete, dp, nil)
		if derr != nil {
			return 0, fmt.Errorf("taking cluster %q from project %q (%d): %w", req.Name, owner.ProjectName, owner.ProjectID, derr)
		}
		if !ok(code) && code != http.StatusNotFound {
			return 0, fmt.Errorf("taking cluster %q from project %q (%d): %w",
				req.Name, owner.ProjectName, owner.ProjectID, httpErr("DELETE", dp, code, d))
		}
		c.warnf("⚠ --force: removed cluster %q from BNK Forge project %q (id %d) to register it here; its cluster id changes\n",
			req.Name, owner.ProjectName, owner.ProjectID)
	}

	p := fmt.Sprintf("/api/projects/%d/k8s/clusters", projectID)
	d, code, err := c.do(ctx, http.MethodPost, p, req)
	if err != nil {
		return 0, err
	}
	if !ok(code) {
		return 0, httpErr("POST", p, code, d)
	}
	if id := createdID(d, "cluster", "id", "cluster_id", "registered_cluster_id"); id != 0 {
		return id, nil
	}
	return 0, fmt.Errorf("register returned no cluster id: %s", truncateBody(d))
}

// UnregisterCluster removes a cluster from a project by name. It returns the
// id removed, or 0 when the project holds no cluster of that name: absence is
// not an error, so a teardown can run twice.
func (c *Client) UnregisterCluster(ctx context.Context, projectID int, name string) (int, error) {
	id, err := c.projectClusterID(ctx, projectID, name)
	if err != nil {
		return 0, err
	}
	if id == 0 {
		return 0, nil
	}
	p := fmt.Sprintf("/api/k8s/clusters/%d", id)
	d, code, derr := c.do(ctx, http.MethodDelete, p, nil)
	if derr != nil {
		return 0, derr
	}
	// 404: someone else got there first; the end state is the one we want.
	if !ok(code) && code != http.StatusNotFound {
		return 0, httpErr("DELETE", p, code, d)
	}
	return id, nil
}

// projectClusterID returns the id of the cluster named name in projectID, or 0
// if there is none. A body it cannot parse is an error, not "none": for an
// unregister that would report success while the cluster stays registered.
func (c *Client) projectClusterID(ctx context.Context, projectID int, name string) (int, error) {
	p := fmt.Sprintf("/api/projects/%d/k8s/clusters", projectID)
	data, code, err := c.do(ctx, http.MethodGet, p, nil)
	if err != nil {
		return 0, err
	}
	if !ok(code) {
		return 0, httpErr("GET", p, code, data)
	}
	type clu struct {
		ID   int    `json:"id"`
		Name string `json:"name"`
	}
	list, err := parseList[clu](data, "clusters", "GET "+p)
	if err != nil {
		return 0, err
	}
	for _, cl := range list {
		if cl.Name == name {
			return cl.ID, nil
		}
	}
	return 0, nil
}
