package forge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// mockForge is a minimal stateful BNK Forge v3 API for tests. Preseed
// credTemplates / projects / clusters to exercise the find-existing paths.
type mockForge struct {
	token         string
	credTemplates []map[string]any
	projects      []map[string]any
	clusters      []map[string]any // registered clusters (for idempotency)
	deletedIDs    []string         // cluster ids DELETEd
	putIDs        []string         // cluster ids updated in place (PUT)
	putStatus     int              // status for PUT /api/k8s/clusters/{id}; 0 => 200
	// clustersByProject lets a test model a cluster held by ANOTHER project. When
	// set it takes precedence over `clusters` for GET /api/projects/{id}/k8s/clusters.
	clustersByProject map[string][]map[string]any
	// clusterListStatus forces a non-200 on GET /api/projects/{id}/k8s/clusters for
	// specific projects — a least-privilege account that can see the project list but
	// not every project's contents.
	clusterListStatus map[string]int
	// credUpdate captures the PUT body for a credential template, so a test can
	// assert what the UPDATE path sends — not just the create path (#223).
	credUpdate      map[string]any
	nextID          int
	registerBody    map[string]any // captured POST body of the last cluster register
	registerPath    string
	projectPlatform map[string]any // captured PUT body of the project platform update
}

func (m *mockForge) id() int { m.nextID++; return m.nextID }

func (m *mockForge) handler(t *testing.T) http.Handler {
	mux := http.NewServeMux()
	writeJSON := func(w http.ResponseWriter, code int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(v)
	}
	mux.HandleFunc("/api/auth/login", func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Username, Password string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Username == "" || body.Password == "" {
			writeJSON(w, 401, map[string]string{"detail": "bad creds"})
			return
		}
		writeJSON(w, 200, map[string]string{"token": m.token})
	})
	mux.HandleFunc("/api/auth/me", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+m.token {
			writeJSON(w, 401, map[string]string{"detail": "unauthorized"})
			return
		}
		writeJSON(w, 200, map[string]any{"user": map[string]any{"username": "admin"}})
	})
	mux.HandleFunc("/api/credential-templates", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			writeJSON(w, 200, m.credTemplates)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		body["id"] = m.id()
		m.credTemplates = append(m.credTemplates, body)
		writeJSON(w, 201, body)
	})
	mux.HandleFunc("/api/credential-templates/", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		m.credUpdate = body
		writeJSON(w, 200, map[string]any{"ok": true}) // PUT update
	})
	mux.HandleFunc("/api/projects", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			writeJSON(w, 200, map[string]any{"projects": m.projects})
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		pid := m.id()
		m.projects = append(m.projects, map[string]any{"id": pid, "name": body["name"]})
		// Forge's create response uses project_id (not id) — regression-guards
		// the field-mapping bug fixed for v1.17.3.
		writeJSON(w, 201, map[string]any{"success": true, "project_id": pid, "name": body["name"]})
	})
	// project-scoped clusters: GET list + POST register (idempotency)
	mux.HandleFunc("/api/projects/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/k8s/clusters") {
			if r.Method == http.MethodGet {
				pid := strings.TrimPrefix(strings.TrimSuffix(r.URL.Path, "/k8s/clusters"), "/api/projects/")
				if code := m.clusterListStatus[pid]; code != 0 {
					writeJSON(w, code, map[string]any{"detail": "forbidden"})
					return
				}
				if m.clustersByProject != nil {
					writeJSON(w, 200, map[string]any{"clusters": m.clustersByProject[pid]})
					return
				}
				writeJSON(w, 200, map[string]any{"clusters": m.clusters})
				return
			}
			_ = json.NewDecoder(r.Body).Decode(&m.registerBody)
			m.registerPath = r.URL.Path
			cid := m.id()
			m.clusters = append(m.clusters, map[string]any{"id": cid, "name": m.registerBody["name"]})
			writeJSON(w, 201, map[string]any{"id": cid})
			return
		}
		if r.Method == http.MethodPut { // PUT /api/projects/{id}
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if _, isPlatform := body["target_platform_profile"]; isPlatform {
				m.projectPlatform = body
				writeJSON(w, 200, map[string]any{"ok": true})
				return
			}
			writeJSON(w, 200, map[string]any{"ok": true})
			return
		}
		writeJSON(w, 404, map[string]any{"detail": "not found"})
	})
	// DELETE /api/k8s/clusters/{id} — used by the idempotent re-register.
	mux.HandleFunc("/api/k8s/clusters/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			id := strings.TrimPrefix(r.URL.Path, "/api/k8s/clusters/")
			if m.putStatus != 0 && m.putStatus != 200 {
				w.WriteHeader(m.putStatus) // model a build with no PUT for this resource
				return
			}
			m.putIDs = append(m.putIDs, id)
			writeJSON(w, 200, map[string]any{"id": id})
			return
		}
		if r.Method != http.MethodDelete {
			writeJSON(w, 200, map[string]any{})
			return
		}
		idStr := strings.TrimPrefix(r.URL.Path, "/api/k8s/clusters/")
		// Record every DELETE, not only ones matching `clusters` — a cross-project
		// takeover deletes a cluster that lives in clustersByProject, and that is
		// exactly the call those tests assert on.
		m.deletedIDs = append(m.deletedIDs, idStr)
		kept := m.clusters[:0]
		for _, cl := range m.clusters {
			if fmt.Sprint(cl["id"]) != idStr {
				kept = append(kept, cl)
			}
		}
		m.clusters = kept
		for pid, cls := range m.clustersByProject {
			keptP := cls[:0]
			for _, cl := range cls {
				if fmt.Sprint(cl["id"]) != idStr {
					keptP = append(keptP, cl)
				}
			}
			m.clustersByProject[pid] = keptP
		}
		w.WriteHeader(204)
	})
	return mux
}

func TestRegisterFlow_CreatesResources(t *testing.T) {
	m := &mockForge{token: "tok123", nextID: 10}
	srv := httptest.NewServer(m.handler(t))
	defer srv.Close()
	ctx := context.Background()
	c := mustNew(t, srv.URL, Options{})

	if err := c.Login(ctx, "admin", "pw"); err != nil {
		t.Fatalf("login: %v", err)
	}
	if c.Token != "tok123" {
		t.Fatalf("token = %q, want tok123", c.Token)
	}
	tid, err := c.EnsureIBMCredentialTemplate(ctx, IBMCredentialTemplate{
		Name: "roksbnkargoctl-ws", APIKey: "APIKEY", ResourceGroup: "default",
		Region: "eu-gb", COSInstance: "bnk-orchestration"})
	if err != nil {
		t.Fatalf("ensure cred: %v", err)
	}
	pid, err := c.EnsureProject(ctx, "roks-demo")
	if err != nil {
		t.Fatalf("ensure project: %v", err)
	}
	// EnsureProject must set the project's target platform (else Forge shows Unknown).
	if m.projectPlatform["target_platform_profile"] != "roks" ||
		m.projectPlatform["platform_provider"] != "ibm" ||
		m.projectPlatform["cloud_provider"] != "ibm" {
		t.Errorf("project platform not set: %v", m.projectPlatform)
	}
	fid, err := c.RegisterCluster(ctx, pid, RegisterRequest{
		Name: "ws", Provider: "IBM", CloudProvider: "ibm",
		ClusterID: "cid", Region: "eu-gb", TemplateID: tid,
		Kubeconfig: "YXBpVmVyc2lvbjp2MQ==", // base64 — Forge requires it in the body
	}, false)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if fid == 0 {
		t.Fatal("register returned id 0")
	}
	// The register must hit the project-scoped path and carry the right body.
	wantPath := "/api/projects/" + itoa(pid) + "/k8s/clusters"
	if m.registerPath != wantPath {
		t.Errorf("register path = %q, want %q", m.registerPath, wantPath)
	}
	if m.registerBody["cluster_id"] != "cid" || m.registerBody["provider"] != "IBM" ||
		m.registerBody["cloud_provider"] != "ibm" ||
		m.registerBody["region"] != "eu-gb" || m.registerBody["kubeconfig"] != "YXBpVmVyc2lvbjp2MQ==" {
		t.Errorf("register body = %v", m.registerBody)
	}
	if _, ok := m.registerBody["template_id"]; !ok {
		t.Errorf("register body missing template_id: %v", m.registerBody)
	}
}

func TestEnsureReusesExisting(t *testing.T) {
	m := &mockForge{
		token:         "t",
		nextID:        100,
		credTemplates: []map[string]any{{"id": 5, "name": "roksbnkargoctl-ws", "provider": "IBM"}},
		projects:      []map[string]any{{"id": 7, "name": "roks-demo"}},
	}
	srv := httptest.NewServer(m.handler(t))
	defer srv.Close()
	ctx := context.Background()
	c := mustNew(t, srv.URL, Options{})
	c.Token = "t"

	tid, err := c.EnsureIBMCredentialTemplate(ctx, IBMCredentialTemplate{
		Name: "roksbnkargoctl-ws", APIKey: "K", ResourceGroup: "rg",
		Region: "eu-gb", COSInstance: "bnk-orchestration"})
	if err != nil || tid != 5 {
		t.Fatalf("reuse cred: id=%d err=%v (want 5)", tid, err)
	}
	pid, err := c.EnsureProject(ctx, "roks-demo")
	if err != nil || pid != 7 {
		t.Fatalf("reuse project: id=%d err=%v (want 7)", pid, err)
	}
	if len(m.projects) != 1 {
		t.Errorf("existing project should not have been re-created: %v", m.projects)
	}
}

// Re-registering a cluster THIS project already holds must update it in place and
// keep its id. The old behaviour DELETEd and re-POSTed, which was called idempotent
// but changed the cluster id — breaking anything holding a reference and discarding
// the scan history attached to it (issue #54).
func TestRegisterSameProject_UpdatesInPlaceAndKeepsID(t *testing.T) {
	m := &mockForge{token: "t", nextID: 40, clusters: []map[string]any{{"id": 5, "name": "ws"}}}
	srv := httptest.NewServer(m.handler(t))
	defer srv.Close()
	c := mustNew(t, srv.URL, Options{})
	c.Token = "t"

	fid, err := c.RegisterCluster(context.Background(), 1, RegisterRequest{
		Name: "ws", Provider: "IBM", CloudProvider: "ibm", ClusterID: "cid", Region: "eu-gb", Kubeconfig: "eA==",
	}, false)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if fid != 5 {
		t.Errorf("the cluster id must survive a re-register, got %d want 5", fid)
	}
	if len(m.deletedIDs) != 0 {
		t.Errorf("re-registering our own cluster must not DELETE it, deleted: %v", m.deletedIDs)
	}
	if len(m.putIDs) != 1 || m.putIDs[0] != "5" {
		t.Errorf("expected an in-place PUT of cluster 5, got %v", m.putIDs)
	}
}

// A Forge build with no PUT for this resource must still register — falling back to
// the historical delete-and-recreate rather than failing something that used to work.
// The id changes; that is the cost of the older server.
func TestRegisterSameProject_FallsBackWhenNoPUT(t *testing.T) {
	m := &mockForge{token: "t", nextID: 40, putStatus: 405,
		clusters: []map[string]any{{"id": 5, "name": "ws"}}}
	srv := httptest.NewServer(m.handler(t))
	defer srv.Close()
	c := mustNew(t, srv.URL, Options{})
	c.Token = "t"

	fid, err := c.RegisterCluster(context.Background(), 1, RegisterRequest{
		Name: "ws", Provider: "IBM", CloudProvider: "ibm", ClusterID: "cid", Region: "eu-gb", Kubeconfig: "eA==",
	}, false)
	if err != nil {
		t.Fatalf("register should fall back, not fail: %v", err)
	}
	if len(m.deletedIDs) != 1 || m.deletedIDs[0] != "5" {
		t.Errorf("expected the DELETE fallback, got %v", m.deletedIDs)
	}
	if fid == 0 || fid == 5 {
		t.Errorf("the fallback recreates, so a fresh id is expected, got %d", fid)
	}
}

// The dangerous case: a cluster held by ANOTHER project. Registering used to POST
// straight over it — moving it silently, or failing with a bare exit 1 that named
// nothing. It must refuse, and the refusal must name the owner.
func TestRegisterOtherProject_RefusedWithoutForce(t *testing.T) {
	m := &mockForge{token: "t", nextID: 40,
		projects: []map[string]any{{"id": 93, "name": "owner-proj"}, {"id": 96, "name": "mine"}},
		clustersByProject: map[string][]map[string]any{
			"93": {{"id": 35, "name": "ws"}},
			"96": {},
		}}
	srv := httptest.NewServer(m.handler(t))
	defer srv.Close()
	c := mustNew(t, srv.URL, Options{})
	c.Token = "t"

	_, err := c.RegisterCluster(context.Background(), 96, RegisterRequest{
		Name: "ws", Provider: "IBM", CloudProvider: "ibm", ClusterID: "cid", Region: "eu-gb", Kubeconfig: "eA==",
	}, false)
	if err == nil {
		t.Fatal("registering a cluster held by another project must be refused")
	}
	if !errors.Is(err, ErrClusterOwnedElsewhere) {
		t.Errorf("the refusal must be identifiable by callers: %v", err)
	}
	for _, want := range []string{"owner-proj", "93"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal must name the owning project (%q missing): %v", want, err)
		}
	}
	if len(m.deletedIDs) != 0 {
		t.Errorf("a refused registration must not have deleted anything: %v", m.deletedIDs)
	}
}

// --force is the deliberate takeover. It must work, so the refusal is a speed bump
// rather than a dead end — and it must actually MOVE the cluster.
//
// The obvious implementation shares the in-place PUT with the same-project path, and
// is silently wrong: that PUT carries no project field, so it updates the cluster
// where it already lives. The cluster stays with the other project, never appears in
// this one, has its kubeconfig quietly overwritten, and the command exits 0. So the
// assertions here are about the DELETE and the POST, not about the absence of an
// error.
func TestRegisterOtherProject_ForceTakesOver(t *testing.T) {
	m := &mockForge{token: "t", nextID: 40,
		projects: []map[string]any{{"id": 93, "name": "owner-proj"}, {"id": 96, "name": "mine"}},
		clustersByProject: map[string][]map[string]any{
			"93": {{"id": 35, "name": "ws"}},
			"96": {},
		}}
	srv := httptest.NewServer(m.handler(t))
	defer srv.Close()
	c := mustNew(t, srv.URL, Options{})
	c.Token = "t"

	fid, err := c.RegisterCluster(context.Background(), 96, RegisterRequest{
		Name: "ws", Provider: "IBM", CloudProvider: "ibm", ClusterID: "cid", Region: "eu-gb", Kubeconfig: "eA==",
	}, true)
	if err != nil {
		t.Fatalf("--force must allow the takeover: %v", err)
	}
	if len(m.putIDs) != 0 {
		t.Errorf("a cross-project takeover must NOT be an in-place PUT (that leaves it in the other project): %v", m.putIDs)
	}
	if len(m.deletedIDs) != 1 || m.deletedIDs[0] != "35" {
		t.Errorf("the takeover must remove the cluster from the owning project, deleted: %v", m.deletedIDs)
	}
	if m.registerPath != "/api/projects/96/k8s/clusters" {
		t.Errorf("the cluster must be re-created in the TARGET project, posted to %q", m.registerPath)
	}
	if fid == 0 || fid == 35 {
		t.Errorf("a move re-creates, so a fresh id is expected, got %d", fid)
	}
}

// A transient PUT failure must not escalate into a destructive retry.
//
// Reading every PUT failure as "this build has no PUT" turns a momentary 500 into
// DELETE + re-POST: the cluster id changes, and the scan history attached to it is
// discarded — losing exactly the continuity the in-place update exists to preserve,
// against a server that supports PUT perfectly well. Only 404/405 means the route is
// absent.
func TestRegisterSameProject_TransientPUTFailureIsNotDestructive(t *testing.T) {
	m := &mockForge{token: "t", nextID: 40, putStatus: 500,
		clusters: []map[string]any{{"id": 5, "name": "ws"}}}
	srv := httptest.NewServer(m.handler(t))
	defer srv.Close()
	c := mustNew(t, srv.URL, Options{})
	c.Token = "t"

	_, err := c.RegisterCluster(context.Background(), 1, RegisterRequest{
		Name: "ws", Provider: "IBM", CloudProvider: "ibm", ClusterID: "cid", Region: "eu-gb", Kubeconfig: "eA==",
	}, false)
	if err == nil {
		t.Fatal("a 500 on the in-place update must be reported, not worked around destructively")
	}
	if len(m.deletedIDs) != 0 {
		t.Errorf("a transient failure must not delete the cluster: %v", m.deletedIDs)
	}
}

// A project this account cannot read must not block registration.
//
// The cross-project scan is a second opinion layered on the direct same-project query
// that already ran and already said "not mine". Failing closed on it turns an ordinary
// least-privilege Forge account — one that can see its own project but not others —
// into a blocked deployment, where the tool used to work.
func TestFindClusterOwner_UnreadableProjectDoesNotBlock(t *testing.T) {
	m := &mockForge{token: "t", nextID: 40,
		projects: []map[string]any{{"id": 93, "name": "locked"}, {"id": 96, "name": "mine"}},
		clustersByProject: map[string][]map[string]any{
			"96": {},
		},
		clusterListStatus: map[string]int{"93": 403}}
	srv := httptest.NewServer(m.handler(t))
	defer srv.Close()
	c := mustNew(t, srv.URL, Options{})
	c.Token = "t"

	owner, err := c.FindClusterOwner(context.Background(), "ws")
	if err != nil {
		t.Fatalf("an unreadable project must not fail the lookup: %v", err)
	}
	if owner.ClusterID != 0 {
		t.Errorf("nothing readable owns the cluster, got %+v", owner)
	}

	fid, rerr := c.RegisterCluster(context.Background(), 96, RegisterRequest{
		Name: "ws", Provider: "IBM", CloudProvider: "ibm", ClusterID: "cid", Region: "eu-gb", Kubeconfig: "eA==",
	}, false)
	if rerr != nil {
		t.Fatalf("registration must still succeed: %v", rerr)
	}
	if fid == 0 {
		t.Error("expected a freshly created cluster id")
	}
}

// An unrecognisable project list is a different matter: reading a body we cannot parse
// as "nobody owns it" is precisely what lets a silent takeover through. Unreadable is
// tolerated; unparseable is not.
func TestFindClusterOwner_UnparseableBodyIsStillAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write([]byte("<html>not json</html>"))
	}))
	defer srv.Close()
	c := mustNew(t, srv.URL, Options{})
	c.Token = "t"

	if _, err := c.FindClusterOwner(context.Background(), "ws"); err == nil {
		t.Fatal("an unparseable project list must be an error, not an empty answer")
	}
}

// An empty Forge answers {"projects": null}. That is "no projects", not a broken
// body — reading it as unparseable would fail every registration against a fresh
// server.
func TestFindClusterOwner_EmptyProjectsIsNotAnError(t *testing.T) {
	m := &mockForge{token: "t"}
	srv := httptest.NewServer(m.handler(t))
	defer srv.Close()
	c := mustNew(t, srv.URL, Options{})
	c.Token = "t"

	owner, err := c.FindClusterOwner(context.Background(), "ws")
	if err != nil {
		t.Fatalf("an empty project list must not be an error: %v", err)
	}
	if owner.ClusterID != 0 {
		t.Errorf("nothing should own the cluster, got %+v", owner)
	}
}

func TestInsecureTLS(t *testing.T) {
	m := &mockForge{token: "tls-tok"}
	srv := httptest.NewTLSServer(m.handler(t))
	defer srv.Close()
	ctx := context.Background()

	// Default (verify on) must fail against the self-signed test cert.
	if err := mustNew(t, srv.URL, Options{}).Login(ctx, "a", "b"); err == nil {
		t.Fatal("expected TLS verification error with insecure=false")
	}
	// insecure=true skips verification and succeeds.
	if err := mustNew(t, srv.URL, Options{Insecure: true}).Login(ctx, "a", "b"); err != nil {
		t.Fatalf("insecure login: %v", err)
	}
}

func TestLoginError(t *testing.T) {
	m := &mockForge{token: "x"}
	srv := httptest.NewServer(m.handler(t))
	defer srv.Close()
	if err := mustNew(t, srv.URL, Options{}).Login(context.Background(), "", ""); err == nil {
		t.Fatal("expected login error on empty creds")
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var b [20]byte
	p := len(b)
	for i > 0 {
		p--
		b[p] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		p--
		b[p] = '-'
	}
	return string(b[p:])
}

// mustNew keeps the existing tests to one line now that New reports a bad CA.
func mustNew(t *testing.T, baseURL string, opts Options) *Client {
	t.Helper()
	c, err := New(baseURL, opts)
	if err != nil {
		t.Fatalf("New(%s): %v", baseURL, err)
	}
	return c
}

// A cluster list for the target project that cannot be parsed must stop the
// registration. Reading it as "not ours" goes on to POST, registering a
// duplicate of a cluster the project may already hold.
//
// The project list here is readable and does not include the target project
// (paginated or permission-filtered), so the cross-project scan finds nothing:
// only the direct query's parse error stands between this and a POST.
func TestRegisterUnparseableOwnClusterListIsAnError(t *testing.T) {
	posted := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost:
			posted = true
			_, _ = w.Write([]byte(`{"id":9}`))
		case r.URL.Path == "/api/projects":
			_, _ = w.Write([]byte(`{"projects":[]}`))
		default:
			_, _ = w.Write([]byte("<html>502</html>"))
		}
	}))
	defer srv.Close()
	c := mustNew(t, srv.URL, Options{})
	_, err := c.RegisterCluster(context.Background(), 1, RegisterRequest{Name: "ws"}, false)
	if err == nil || !strings.Contains(err.Error(), "unrecognised body") {
		t.Fatalf("want an unrecognised-body error, got %v", err)
	}
	if posted {
		t.Fatal("registered on top of a cluster list it could not read")
	}
}

// Same for a project list the ownership scan cannot parse, reached when the
// target project does not hold the cluster.
func TestRegisterUnparseableProjectListIsAnError(t *testing.T) {
	posted := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost:
			posted = true
		case r.URL.Path == "/api/projects":
			_, _ = w.Write([]byte(`{"items":[]}`))
		default:
			_, _ = w.Write([]byte(`{"clusters":[]}`))
		}
	}))
	defer srv.Close()
	c := mustNew(t, srv.URL, Options{})
	if _, err := c.RegisterCluster(context.Background(), 1, RegisterRequest{Name: "ws"}, false); err == nil {
		t.Fatal("an unparseable project list must stop the registration")
	}
	if posted {
		t.Fatal("registered without knowing who holds the cluster")
	}
}

// A template list that cannot be parsed must not read as "none": that would
// create another template holding the API key on every run.
func TestEnsureTemplateUnparseableListIsAnError(t *testing.T) {
	posted := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			posted = true
		}
		_, _ = w.Write([]byte(`{"detail":"x"}`))
	}))
	defer srv.Close()
	c := mustNew(t, srv.URL, Options{})
	if _, err := c.EnsureIBMCredentialTemplate(context.Background(), IBMCredentialTemplate{Name: "n", APIKey: "k"}); err == nil {
		t.Fatal("want an error")
	}
	if posted {
		t.Fatal("created a template on top of a list it could not read")
	}
}

// The cross-project warnings go to the client's Log, so a command can route
// them to its own output.
func TestWarningsGoToLog(t *testing.T) {
	m := &mockForge{token: "t", nextID: 40,
		projects:          []map[string]any{{"id": 93, "name": "locked"}},
		clustersByProject: map[string][]map[string]any{},
		clusterListStatus: map[string]int{"93": 403}}
	srv := httptest.NewServer(m.handler(t))
	defer srv.Close()
	c := mustNew(t, srv.URL, Options{})
	var log strings.Builder
	c.Log = &log
	if _, err := c.FindClusterOwner(context.Background(), "ws"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(log.String(), `cannot check BNK Forge project "locked"`) {
		t.Fatalf("warning not in Log: %q", log.String())
	}
}

// Another project's cluster list that cannot be parsed is an error too: it is
// not a permission (which is warned about), and reading it as "not there" could
// move that project's cluster.
func TestFindClusterOwner_UnparseableClusterListIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/projects" {
			_, _ = w.Write([]byte(`{"projects":[{"id":93,"name":"other"}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"items":[]}`))
	}))
	defer srv.Close()
	c := mustNew(t, srv.URL, Options{})
	c.Log = &strings.Builder{}
	if _, err := c.FindClusterOwner(context.Background(), "ws"); err == nil || !strings.Contains(err.Error(), `no "clusters" key`) {
		t.Fatalf("want a parse error, got %v", err)
	}
}
