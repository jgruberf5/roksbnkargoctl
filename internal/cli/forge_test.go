package cli

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/jgruberf5/roksbnkargoctl/internal/ibm"
)

// ---- a fake BNK Forge -----------------------------------------------------------

type fakeForgeCluster struct {
	ID      int
	Name    string
	Project int
	Body    map[string]any
}

type fakeForge struct {
	mu        sync.Mutex
	user, pw  string
	logins    int
	calls     []string // "METHOD path", every request
	templates []map[string]any
	projects  []map[string]any
	clusters  []*fakeForgeCluster
	nextID    int
}

func newFakeForge() *fakeForge { return &fakeForge{user: "admin", pw: "s3cret", nextID: 100} }

func (f *fakeForge) id() int { f.nextID++; return f.nextID }

func (f *fakeForge) count(prefix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

func (f *fakeForge) cluster(name string) *fakeForgeCluster {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.clusters {
		if c.Name == name {
			return c
		}
	}
	return nil
}

func (f *fakeForge) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, r.Method+" "+r.URL.Path)
	reply := func(code int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(v)
	}
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	if r.URL.Path == "/api/auth/login" {
		f.logins++
		if body["username"] != f.user || body["password"] != f.pw {
			reply(401, map[string]string{"detail": "bad credentials"})
			return
		}
		reply(200, map[string]string{"token": "tok"})
		return
	}
	if r.Header.Get("Authorization") != "Bearer tok" {
		reply(401, map[string]string{"detail": "not authenticated"})
		return
	}
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/") // api, ...
	switch {
	case r.URL.Path == "/api/credential-templates" && r.Method == http.MethodGet:
		reply(200, f.templates) // a bare array, as Forge returns
	case r.URL.Path == "/api/credential-templates" && r.Method == http.MethodPost:
		body["id"] = f.id()
		f.templates = append(f.templates, body)
		reply(201, body)
	case len(parts) == 3 && parts[1] == "credential-templates" && r.Method == http.MethodPut:
		for _, t := range f.templates {
			if fmt.Sprint(t["id"]) == parts[2] {
				for k, v := range body {
					t[k] = v
				}
			}
		}
		reply(200, map[string]any{"ok": true})
	case r.URL.Path == "/api/projects" && r.Method == http.MethodGet:
		reply(200, map[string]any{"projects": f.projects})
	case r.URL.Path == "/api/projects" && r.Method == http.MethodPost:
		pid := f.id()
		f.projects = append(f.projects, map[string]any{"id": pid, "name": body["name"]})
		reply(201, map[string]any{"success": true, "project_id": pid})
	case len(parts) == 3 && parts[1] == "projects" && r.Method == http.MethodPut:
		reply(200, map[string]any{"ok": true})
	case len(parts) == 5 && parts[1] == "projects" && parts[3] == "k8s":
		pid, _ := strconv.Atoi(parts[2])
		if r.Method == http.MethodGet {
			list := []map[string]any{}
			for _, c := range f.clusters {
				if c.Project == pid {
					list = append(list, map[string]any{"id": c.ID, "name": c.Name})
				}
			}
			reply(200, map[string]any{"clusters": list})
			return
		}
		c := &fakeForgeCluster{ID: f.id(), Name: fmt.Sprint(body["name"]), Project: pid, Body: body}
		f.clusters = append(f.clusters, c)
		reply(201, map[string]any{"id": c.ID})
	case len(parts) == 4 && parts[1] == "k8s" && parts[2] == "clusters":
		id, _ := strconv.Atoi(parts[3])
		for i, c := range f.clusters {
			if c.ID != id {
				continue
			}
			if r.Method == http.MethodDelete {
				f.clusters = append(f.clusters[:i], f.clusters[i+1:]...)
				w.WriteHeader(204)
				return
			}
			c.Body = body
			reply(200, map[string]any{"id": id})
			return
		}
		reply(404, map[string]any{"detail": "not found"})
	default:
		reply(404, map[string]any{"detail": "no route " + r.Method + " " + r.URL.Path})
	}
}

// ---- a fake IBM Cloud -----------------------------------------------------------

const fakeAdminKubeconfig = `apiVersion: v1
kind: Config
clusters:
- name: bnkargo/c0ffee
  cluster:
    server: %s
contexts:
- name: bnkargo/c0ffee
  context: {cluster: bnkargo/c0ffee, user: admin}
current-context: bnkargo/c0ffee
users:
- name: admin
  user:
    client-certificate-data: Q0VSVA==
    client-key-data: S0VZ
`

type fakeForgeIBM struct {
	clusters     []ibm.Cluster
	lookups      int
	fetchPrivate []bool
}

func newFakeForgeIBM() *fakeForgeIBM {
	return &fakeForgeIBM{clusters: []ibm.Cluster{{
		ID: "c0ffee", Name: "bnkargo", Region: "us-south", ResourceGroupName: "bnk-rg",
		PublicServiceEndpointURL:  "https://c1.us-south.containers.cloud.ibm.com:30001",
		PrivateServiceEndpointURL: "https://c1.private.us-south.containers.cloud.ibm.com:30001",
	}}}
}

func (f *fakeForgeIBM) GetCluster(_ context.Context, nameOrID string) (*ibm.Cluster, error) {
	f.lookups++
	for i := range f.clusters {
		if c := &f.clusters[i]; c.Name == nameOrID || c.ID == nameOrID {
			return c, nil
		}
	}
	return nil, fmt.Errorf("%w: %q", ibm.ErrClusterNotFound, nameOrID)
}

func (f *fakeForgeIBM) FetchAdminKubeconfig(_ context.Context, nameOrID string, private bool) ([]byte, error) {
	f.fetchPrivate = append(f.fetchPrivate, private)
	for _, c := range f.clusters {
		if c.ID == nameOrID {
			server := c.PublicServiceEndpointURL
			if private {
				server = c.PrivateServiceEndpointURL
			}
			return []byte(fmt.Sprintf(fakeAdminKubeconfig, server)), nil
		}
	}
	return nil, fmt.Errorf("fetch by %q: want the cluster id", nameOrID)
}

// ---- harness --------------------------------------------------------------------

type forgeEnv struct {
	home  string
	forge *fakeForge
	srv   *httptest.Server
	ibm   *fakeForgeIBM
}

// setupForge isolates the tool (ROKSBNKARGOCTL_HOME an empty temp dir), starts a
// fake Forge, and substitutes a fake IBM Cloud. Credentials come from the
// BNK_FORGE_* variables, as in a pipeline.
func setupForge(t *testing.T) *forgeEnv {
	t.Helper()
	e := &forgeEnv{home: isolate(t), forge: newFakeForge(), ibm: newFakeForgeIBM()}
	e.srv = httptest.NewServer(e.forge)
	t.Cleanup(e.srv.Close)
	t.Setenv("IBMCLOUD_API_KEY", "ibm-api-key")
	t.Setenv(envForgeURL, e.srv.URL)
	t.Setenv(envForgeUser, "admin")
	t.Setenv(envForgePassword, "s3cret")
	prevIBM, prevTerm, prevRead := forgeIBMFor, forgeTerminal, forgeReadPassword
	forgeIBMFor = func(*session) (forgeIBM, error) { return e.ibm, nil }
	forgeTerminal = func() bool { return false }
	t.Cleanup(func() { forgeIBMFor, forgeTerminal, forgeReadPassword = prevIBM, prevTerm, prevRead })
	return e
}

var noWS = []string{"forge", "register", "--cluster", "bnkargo", "--region", "us-south"}

// ---- register -------------------------------------------------------------------

// Without a workspace: the cluster is looked up by name, and Forge gets the
// credential template, the project (named after the cluster) and a
// certificate-based kubeconfig for the public endpoint. Nothing is written.
func TestForgeRegisterWithoutAWorkspace(t *testing.T) {
	e := setupForge(t)
	out, err := runRoot(t, noWS...)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if e.forge.logins != 1 {
		t.Errorf("logins %d, want 1", e.forge.logins)
	}
	if len(e.forge.templates) != 1 {
		t.Fatalf("templates %v", e.forge.templates)
	}
	tmpl := e.forge.templates[0]
	want := map[string]any{
		"name": "roksbnkargoctl-bnkargo", "provider": "ibm", "ibmcloud_api_key": "ibm-api-key",
		"ibmcloud_resource_group": "bnk-rg", "region": "us-south", "is_default": true,
	}
	for k, v := range want {
		if tmpl[k] != v {
			t.Errorf("template %s = %v, want %v", k, tmpl[k], v)
		}
	}
	if _, sent := tmpl["ibm_cos_instance_name"]; sent {
		t.Errorf("without a workspace the default COS instance name is a guess and must not be sent: %v", tmpl)
	}
	if len(e.forge.projects) != 1 || e.forge.projects[0]["name"] != "bnkargo" {
		t.Fatalf("project should default to the cluster name: %v", e.forge.projects)
	}
	c := e.forge.cluster("bnkargo")
	if c == nil {
		t.Fatalf("cluster not registered; calls %v", e.forge.calls)
	}
	b := c.Body
	if b["cluster_id"] != "c0ffee" || b["region"] != "us-south" || b["provider"] != "IBM" || b["cloud_provider"] != "ibm" ||
		fmt.Sprint(b["template_id"]) != fmt.Sprint(tmpl["id"]) {
		t.Errorf("register body %v", b)
	}
	raw, err := base64.StdEncoding.DecodeString(fmt.Sprint(b["kubeconfig"]))
	if err != nil {
		t.Fatalf("kubeconfig is not base64: %v", err)
	}
	var kc map[string]any
	if err := yaml.Unmarshal(raw, &kc); err != nil {
		t.Fatal(err)
	}
	if s := string(raw); !strings.Contains(s, "client-certificate-data: Q0VSVA==") || !strings.Contains(s, "client-key-data: S0VZ") ||
		!strings.Contains(s, "server: https://c1.us-south.containers.cloud.ibm.com:30001") || strings.Contains(s, "token") {
		t.Errorf("kubeconfig sent to Forge:\n%s", s)
	}
	if len(e.ibm.fetchPrivate) != 1 || e.ibm.fetchPrivate[0] {
		t.Errorf("admin kubeconfig fetches (private?) %v, want one public", e.ibm.fetchPrivate)
	}
	if !strings.Contains(out, "registered cluster bnkargo with BNK Forge") {
		t.Errorf("output:\n%s", out)
	}
	if got := homeEntries(t, e.home); len(got) != 0 {
		t.Errorf("a no-workspace run wrote %v into ROKSBNKARGOCTL_HOME", got)
	}
}

// A second run reuses the template (updated, not duplicated) and updates the
// cluster in place, keeping its Forge id.
func TestForgeRegisterTwiceReusesTheTemplateAndKeepsTheID(t *testing.T) {
	e := setupForge(t)
	if out, err := runRoot(t, noWS...); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	first := e.forge.cluster("bnkargo").ID
	out, err := runRoot(t, append(noWS, "--cluster", "c0ffee")...) // by id this time
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if n := e.forge.count("POST /api/credential-templates"); n != 1 {
		t.Errorf("template created %d times, want once", n)
	}
	if n := e.forge.count("PUT /api/credential-templates/"); n != 1 {
		t.Errorf("template updated %d times on the second run, want once", n)
	}
	if len(e.forge.templates) != 1 || len(e.forge.projects) != 1 || len(e.forge.clusters) != 1 {
		t.Errorf("duplicates: %d templates, %d projects, %d clusters", len(e.forge.templates), len(e.forge.projects), len(e.forge.clusters))
	}
	if got := e.forge.cluster("bnkargo").ID; got != first {
		t.Errorf("Forge cluster id changed %d → %d", first, got)
	}
	if e.forge.count("DELETE ") != 0 {
		t.Errorf("re-registering deleted something: %v", e.forge.calls)
	}
	if !strings.Contains(out, fmt.Sprintf("Forge cluster id %d", first)) {
		t.Errorf("output:\n%s", out)
	}
}

// A cluster held by another project is refused, naming it, and nothing is
// changed; --force moves it.
func TestForgeRegisterRefusesAnotherProjectsClusterUnlessForced(t *testing.T) {
	e := setupForge(t)
	e.forge.projects = []map[string]any{{"id": 7, "name": "platform-team"}}
	e.forge.clusters = []*fakeForgeCluster{{ID: 35, Name: "bnkargo", Project: 7}}

	out, err := runRoot(t, noWS...)
	if err == nil {
		t.Fatalf("registered over another project's cluster\n%s", out)
	}
	for _, want := range []string{"another BNK Forge project", `"platform-team"`, "id 7", "cluster id 35", "--force"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal lacks %q: %v", want, err)
		}
	}
	if c := e.forge.cluster("bnkargo"); c == nil || c.Project != 7 || e.forge.count("DELETE ") != 0 {
		t.Fatalf("the refusal changed something: %+v %v", c, e.forge.calls)
	}

	out, err = runRoot(t, append(noWS, "--force")...)
	if err != nil {
		t.Fatalf("--force: %v\n%s", err, out)
	}
	c := e.forge.cluster("bnkargo")
	if c == nil || c.Project == 7 || c.ID == 35 || len(e.forge.clusters) != 1 {
		t.Fatalf("--force did not move the cluster: %+v", c)
	}
	if !strings.Contains(out, `removed cluster "bnkargo" from BNK Forge project "platform-team"`) {
		t.Errorf("the takeover was not reported:\n%s", out)
	}
}

func TestForgeRegisterPrivateEndpoint(t *testing.T) {
	e := setupForge(t)
	if out, err := runRoot(t, append(noWS, "--endpoint", "private")...); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if len(e.ibm.fetchPrivate) != 1 || !e.ibm.fetchPrivate[0] {
		t.Fatalf("fetches %v, want one private", e.ibm.fetchPrivate)
	}
	raw, _ := base64.StdEncoding.DecodeString(fmt.Sprint(e.forge.cluster("bnkargo").Body["kubeconfig"]))
	if !strings.Contains(string(raw), "c1.private.us-south") {
		t.Errorf("kubeconfig:\n%s", raw)
	}

	out, err := runRoot(t, append(noWS, "--endpoint", "both")...)
	if err == nil || !strings.Contains(err.Error(), "must be public or private") {
		t.Errorf("--endpoint both: %v\n%s", err, out)
	}
	// A cluster with no public endpoint is refused rather than handed to Forge
	// with a kubeconfig for an endpoint it does not have.
	e.ibm.clusters[0].PrivateServiceEndpointURL = ""
	if _, err := runRoot(t, append(noWS, "--endpoint", "private")...); err == nil || !strings.Contains(err.Error(), "no private service endpoint") {
		t.Errorf("private endpoint missing: %v", err)
	}
	e.ibm.clusters[0].PrivateServiceEndpointURL = "https://c1.private.us-south.containers.cloud.ibm.com:30001"
	e.ibm.clusters[0].PublicServiceEndpointURL = ""
	if _, err := runRoot(t, noWS...); err == nil || !strings.Contains(err.Error(), "no public service endpoint") {
		t.Errorf("public endpoint missing: %v", err)
	}
}

// Everything that can be checked locally is checked before Forge is contacted.
func TestForgeRegisterInputErrorsContactNothing(t *testing.T) {
	e := setupForge(t)
	for name, tc := range map[string]struct {
		args []string
		env  map[string]string
		want string
	}{
		"no cluster": {args: []string{"forge", "register", "--region", "us-south"}, want: "--cluster (or ROKSBNKARGOCTL_CLUSTER)"},
		"no region":  {args: []string{"forge", "register", "--cluster", "bnkargo"}, want: "--region (or ROKSBNKARGOCTL_IBMCLOUD_REGION)"},
		"wrong region": {args: []string{"forge", "register", "--cluster", "bnkargo", "--region", "eu-de"},
			want: "is in region us-south, not eu-de"},
		"no url":               {args: noWS, env: map[string]string{envForgeURL: ""}, want: "no BNK Forge URL"},
		"url without a scheme": {args: noWS, env: map[string]string{envForgeURL: "forge.example.com"}, want: "must start with https://"},
		"no user":              {args: noWS, env: map[string]string{envForgeUser: ""}, want: "no BNK Forge username"},
		"no password":          {args: noWS, env: map[string]string{envForgePassword: ""}, want: "set BNK_FORGE_PASSWORD"},
		"no api key":           {args: noWS, env: map[string]string{"IBMCLOUD_API_KEY": "", "IC_API_KEY": ""}, want: "IBM Cloud API key"},
		"unknown cluster": {args: []string{"forge", "register", "--cluster", "nope", "--region", "us-south"},
			want: "cluster not found"},
	} {
		t.Run(name, func(t *testing.T) {
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			before := e.forge.count("")
			out, err := runRoot(t, tc.args...)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want %q, got %v\n%s", tc.want, err, out)
			}
			if n := e.forge.count(""); n != before {
				t.Fatalf("Forge was contacted: %v", e.forge.calls[before:])
			}
		})
	}
}

// The password is never a flag.
func TestForgeHasNoPasswordFlag(t *testing.T) {
	setupForge(t)
	for _, sub := range []string{"register", "unregister"} {
		if _, err := runRoot(t, "forge", sub, "--password", "x"); err == nil || !strings.Contains(err.Error(), "unknown flag: --password") {
			t.Errorf("forge %s --password: %v", sub, err)
		}
	}
}

// On a terminal a missing password is prompted for, without echo.
func TestForgePromptsForThePasswordOnATerminal(t *testing.T) {
	e := setupForge(t)
	t.Setenv(envForgePassword, "")
	forgeTerminal = func() bool { return true }
	prompted := 0
	forgeReadPassword = func() ([]byte, error) { prompted++; return []byte("s3cret\n"), nil }
	out, err := runRoot(t, noWS...)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if prompted != 1 || e.forge.logins != 1 || !strings.Contains(out, "BNK Forge password: ") {
		t.Errorf("prompted %d, logins %d\n%s", prompted, e.forge.logins, out)
	}
}

// forge.url and forge.username from a flag or ROKSBNKARGOCTL_* beat the
// BNK_FORGE_* variables, which beat nothing.
func TestForgeConnectionPrecedence(t *testing.T) {
	e := setupForge(t)
	t.Setenv(envForgeURL, "http://127.0.0.1:1") // nothing listens
	t.Setenv(envForgeUser, "wrong")
	out, err := runRoot(t, append(noWS, "--url", e.srv.URL, "--username", "admin")...)
	if err != nil {
		t.Fatalf("flags must beat BNK_FORGE_*: %v\n%s", err, out)
	}
	t.Setenv("ROKSBNKARGOCTL_FORGE_URL", e.srv.URL)
	t.Setenv("ROKSBNKARGOCTL_FORGE_USERNAME", "admin")
	if out, err := runRoot(t, noWS...); err != nil {
		t.Fatalf("ROKSBNKARGOCTL_FORGE_* must beat BNK_FORGE_*: %v\n%s", err, out)
	}
	t.Setenv("ROKSBNKARGOCTL_FORGE_USERNAME", "")
	if _, err := runRoot(t, noWS...); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("BNK_FORGE_USER should be used when nothing overrides it: %v", err)
	}
}

// --insecure reaches a self-signed Forge and says, once, what it costs.
func TestForgeInsecureWarns(t *testing.T) {
	e := setupForge(t)
	tls := httptest.NewTLSServer(e.forge)
	defer tls.Close()
	t.Setenv(envForgeURL, tls.URL)
	if _, err := runRoot(t, noWS...); err == nil || !strings.Contains(err.Error(), "certificate") {
		t.Fatalf("a self-signed Forge must fail verification: %v", err)
	}
	out, err := runRoot(t, append(noWS, "--insecure")...)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if n := strings.Count(out, "TLS verification is DISABLED"); n != 1 {
		t.Errorf("want one insecure warning, got %d:\n%s", n, out)
	}
}

// ---- with a workspace -----------------------------------------------------------

const forgeWorkspaceYAML = `
ibmcloud:
  region: us-south
  resource_group: ws-rg
cluster: bnkargo
cos:
  instance: my-cos
forge:
  project: team-a
resolved:
  cluster_id: c0ffee
  cluster_name: bnkargo
  public_endpoint: https://c1.us-south.containers.cloud.ibm.com:30001
  private_endpoint: https://c1.private.us-south.containers.cloud.ibm.com:30001
`

// With a workspace the resolved facts are used (no cluster lookup), and the
// forge section of config.yaml is read.
func TestForgeRegisterWithAWorkspace(t *testing.T) {
	e := setupForge(t)
	writeWorkspace(t, e.home, "w", forgeWorkspaceYAML)
	out, err := runRoot(t, "-w", "w", "forge", "register")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if e.ibm.lookups != 0 {
		t.Errorf("looked the cluster up %d times; the workspace has it resolved", e.ibm.lookups)
	}
	if len(e.forge.projects) != 1 || e.forge.projects[0]["name"] != "team-a" {
		t.Errorf("projects %v, want forge.project team-a", e.forge.projects)
	}
	tmpl := e.forge.templates[0]
	if tmpl["name"] != "roksbnkargoctl-team-a" || tmpl["ibmcloud_resource_group"] != "ws-rg" || tmpl["ibm_cos_instance_name"] != "my-cos" {
		t.Errorf("template %v", tmpl)
	}
	if c := e.forge.cluster("bnkargo"); c == nil || c.Body["cluster_id"] != "c0ffee" {
		t.Errorf("cluster %+v", c)
	}
}

// ---- unregister -----------------------------------------------------------------

func TestForgeUnregister(t *testing.T) {
	e := setupForge(t)
	unreg := []string{"forge", "unregister", "--cluster", "bnkargo", "--region", "us-south"}

	// No project at all: success, and nothing is created.
	out, err := runRoot(t, unreg...)
	if err != nil || !strings.Contains(out, `no BNK Forge project "bnkargo": nothing to unregister`) {
		t.Fatalf("%v\n%s", err, out)
	}
	if e.forge.count("POST /api/projects") != 0 {
		t.Fatal("unregister created a project")
	}

	// Registered, then removed.
	if out, err := runRoot(t, noWS...); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	id := e.forge.cluster("bnkargo").ID
	out, err = runRoot(t, unreg...)
	if err != nil || !strings.Contains(out, fmt.Sprintf("unregistered cluster bnkargo (Forge cluster id %d)", id)) {
		t.Fatalf("%v\n%s", err, out)
	}
	if e.forge.cluster("bnkargo") != nil {
		t.Fatal("cluster still registered")
	}

	// Project exists, cluster does not: success.
	out, err = runRoot(t, unreg...)
	if err != nil || !strings.Contains(out, "is not registered in BNK Forge project") {
		t.Fatalf("%v\n%s", err, out)
	}
}

// A cluster already deleted from IBM Cloud is still unregistered by the name
// given: teardowns run late.
func TestForgeUnregisterAClusterGoneFromIBMCloud(t *testing.T) {
	e := setupForge(t)
	if out, err := runRoot(t, noWS...); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	e.ibm.clusters = nil
	out, err := runRoot(t, "forge", "unregister", "--cluster", "bnkargo", "--region", "us-south")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if e.forge.cluster("bnkargo") != nil || !strings.Contains(out, "not in IBM Cloud") {
		t.Fatalf("not unregistered by name:\n%s", out)
	}
}

func TestForgeUnregisterWithAWorkspace(t *testing.T) {
	e := setupForge(t)
	writeWorkspace(t, e.home, "w", forgeWorkspaceYAML)
	if out, err := runRoot(t, "-w", "w", "forge", "register"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	out, err := runRoot(t, "-w", "w", "forge", "unregister")
	if err != nil || e.forge.cluster("bnkargo") != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if e.ibm.lookups != 0 {
		t.Errorf("looked the cluster up %d times", e.ibm.lookups)
	}
}
