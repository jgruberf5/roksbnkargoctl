package argocd

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// recorder is an httptest handler that records requests and answers from a script.
type recorder struct {
	mu   sync.Mutex
	reqs []recorded
	h    func(w http.ResponseWriter, r *http.Request, body []byte)
}

type recorded struct {
	Method, Path, RawPath, Query, Auth string
	Body                               []byte
}

func (rc *recorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	rc.mu.Lock()
	rc.reqs = append(rc.reqs, recorded{r.Method, r.URL.Path, r.URL.EscapedPath(), r.URL.RawQuery, r.Header.Get("Authorization"), b})
	rc.mu.Unlock()
	rc.h(w, r, b)
}

func newTest(t *testing.T, h func(w http.ResponseWriter, r *http.Request, body []byte)) (*Client, *recorder) {
	t.Helper()
	rc := &recorder{h: h}
	srv := httptest.NewTLSServer(rc)
	t.Cleanup(srv.Close)
	c := New(srv.URL, "tok", true, nil)
	c.PollInterval = time.Millisecond
	return c, rc
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func TestRequireAtLeast(t *testing.T) {
	for _, tc := range []struct {
		have string
		ok   bool
	}{
		{"v3.2.9+abc1234", false},
		{"v3.2.0", false},
		{"v2.14.3+x", false},
		{"v3.3.0", true},
		{"3.3.0", true},
		{"v3.3.0-rc1", true},
		{"v3.5.1+abcdef0", true},
		{"v4.0.0", true},
	} {
		t.Run(tc.have, func(t *testing.T) {
			c, rc := newTest(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
				if r.URL.Path != "/api/version" {
					t.Errorf("path %s", r.URL.Path)
				}
				writeJSON(w, 200, map[string]string{"Version": tc.have, "GitCommit": "abc"})
			})
			err := c.RequireAtLeast(context.Background(), "3.3.0")
			if tc.ok && err != nil {
				t.Fatalf("want accepted, got %v", err)
			}
			if !tc.ok && err == nil {
				t.Fatalf("want rejected")
			}
			if got := rc.reqs[0].Auth; got != "Bearer tok" {
				t.Errorf("auth header %q", got)
			}
		})
	}
	if _, err := ParseVersion("vX.1"); err == nil {
		t.Error("garbage version parsed")
	}
}

func TestUpsertClusterBody(t *testing.T) {
	c, rc := newTest(t, func(w http.ResponseWriter, r *http.Request, b []byte) {
		writeJSON(w, 200, map[string]any{"name": "roks", "server": "https://c.private:30000"})
	})
	ca := []byte("-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n")
	_, err := c.UpsertCluster(context.Background(), Cluster{
		Name: "roks", Server: "https://c.private:30000", BearerToken: "sa-token", CAData: ca,
		Labels: map[string]string{"app.kubernetes.io/managed-by": "roksbnkargoctl"},
	})
	if err != nil {
		t.Fatal(err)
	}
	r := rc.reqs[0]
	if r.Method != "POST" || r.Path != "/api/v1/clusters" || r.Query != "upsert=true" {
		t.Fatalf("got %s %s?%s", r.Method, r.Path, r.Query)
	}
	var body map[string]any
	if err := json.Unmarshal(r.Body, &body); err != nil {
		t.Fatal(err)
	}
	if body["name"] != "roks" || body["server"] != "https://c.private:30000" {
		t.Errorf("name/server: %v", body)
	}
	cfg := body["config"].(map[string]any)
	if cfg["bearerToken"] != "sa-token" {
		t.Errorf("bearerToken: %v", cfg)
	}
	tlsc := cfg["tlsClientConfig"].(map[string]any)
	if tlsc["caData"] != base64.StdEncoding.EncodeToString(ca) {
		t.Errorf("caData must be base64 of the PEM, got %v", tlsc["caData"])
	}
	if _, ok := tlsc["insecure"]; ok {
		t.Errorf("insecure should be omitted when false: %v", tlsc)
	}
	if body["labels"].(map[string]any)["app.kubernetes.io/managed-by"] != "roksbnkargoctl" {
		t.Errorf("labels: %v", body["labels"])
	}
}

func TestDeleteClusterEscapesServerAndHandlesMissing(t *testing.T) {
	present := true
	c, rc := newTest(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		switch {
		case r.Method == "GET" && r.URL.Path == "/api/v1/clusters":
			items := []any{}
			if present {
				items = append(items, map[string]any{"server": r.URL.Query().Get("server")})
			}
			writeJSON(w, 200, map[string]any{"items": items})
		case r.Method == "DELETE":
			writeJSON(w, 200, map[string]any{})
		default:
			writeJSON(w, 500, map[string]any{"message": "unexpected"})
		}
	})
	if err := c.DeleteCluster(context.Background(), "https://c.private:30000"); err != nil {
		t.Fatal(err)
	}
	del := rc.reqs[1]
	if del.Method != "DELETE" || del.RawPath != "/api/v1/clusters/https%3A%2F%2Fc.private%3A30000" {
		t.Fatalf("delete path: %s %s", del.Method, del.RawPath)
	}
	present = false
	if err := c.DeleteCluster(context.Background(), "https://c.private:30000"); !IsNotFound(err) {
		t.Fatalf("missing cluster: want IsNotFound, got %v", err)
	}
}

func TestUpsertApplicationBody(t *testing.T) {
	c, rc := newTest(t, func(w http.ResponseWriter, r *http.Request, b []byte) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(b)
	})
	app := &Application{
		Metadata: ObjectMeta{Name: "bnk-demo", Namespace: "argocd", Finalizers: []string{ResourcesFinalizer}},
		Spec: ApplicationSpec{
			Project: "default",
			Source: &ApplicationSource{RepoURL: "https://git.example.com/bnk.git", Path: "clusters/demo",
				TargetRevision: "main", Directory: &SourceDirectory{Recurse: true}},
			Destination: ApplicationDestination{Server: "https://c.private:30000"},
			SyncPolicy: &SyncPolicy{
				SyncOptions: []string{"ServerSideApply=true", "RespectIgnoreDifferences=true"},
				Retry:       &RetryStrategy{Limit: ptrInt64(2), Backoff: &Backoff{Duration: "10s", Factor: 2, MaxDuration: "3m"}},
			},
		},
		Status: &ApplicationStatus{Sync: SyncStatus{Status: "Synced"}},
	}
	if _, err := c.UpsertApplication(context.Background(), app); err != nil {
		t.Fatal(err)
	}
	r := rc.reqs[0]
	if r.Method != "POST" || r.Path != "/api/v1/applications" || r.Query != "upsert=true&validate=true" {
		t.Fatalf("got %s %s?%s", r.Method, r.Path, r.Query)
	}
	var body map[string]any
	if err := json.Unmarshal(r.Body, &body); err != nil {
		t.Fatal(err)
	}
	if body["apiVersion"] != "argoproj.io/v1alpha1" || body["kind"] != "Application" {
		t.Errorf("type meta: %v %v", body["apiVersion"], body["kind"])
	}
	if _, ok := body["status"]; ok {
		t.Error("status must not be sent")
	}
	md := body["metadata"].(map[string]any)
	if md["name"] != "bnk-demo" || md["finalizers"].([]any)[0] != ResourcesFinalizer {
		t.Errorf("metadata: %v", md)
	}
	spec := body["spec"].(map[string]any)
	src := spec["source"].(map[string]any)
	if src["repoURL"] != "https://git.example.com/bnk.git" || src["path"] != "clusters/demo" || src["targetRevision"] != "main" {
		t.Errorf("source: %v", src)
	}
	if src["directory"].(map[string]any)["recurse"] != true {
		t.Errorf("directory.recurse: %v", src["directory"])
	}
	if spec["destination"].(map[string]any)["server"] != "https://c.private:30000" {
		t.Errorf("destination: %v", spec["destination"])
	}
	sp := spec["syncPolicy"].(map[string]any)
	opts := sp["syncOptions"].([]any)
	if len(opts) != 2 || opts[0] != "ServerSideApply=true" || opts[1] != "RespectIgnoreDifferences=true" {
		t.Errorf("syncOptions: %v", opts)
	}
	if _, ok := sp["automated"]; ok {
		t.Error("sync must be manual")
	}
	if sp["retry"].(map[string]any)["limit"] != float64(2) {
		t.Errorf("retry: %v", sp["retry"])
	}
}

func TestSyncBody(t *testing.T) {
	c, rc := newTest(t, func(w http.ResponseWriter, r *http.Request, b []byte) {
		writeJSON(w, 200, map[string]any{"metadata": map[string]any{"name": "bnk-demo"}})
	})
	if _, err := c.Sync(context.Background(), "bnk-demo", SyncOptions{Prune: true, Revision: "abc123"}); err != nil {
		t.Fatal(err)
	}
	r := rc.reqs[0]
	if r.Method != "POST" || r.Path != "/api/v1/applications/bnk-demo/sync" {
		t.Fatalf("got %s %s", r.Method, r.Path)
	}
	var body map[string]any
	_ = json.Unmarshal(r.Body, &body)
	if body["name"] != "bnk-demo" || body["prune"] != true || body["revision"] != "abc123" || body["project"] != "default" {
		t.Errorf("sync body: %s", r.Body)
	}
}

// opApp builds an Application JSON answer with the given operation state.
func opApp(pending bool, phase, msg string, res []ResourceResult) map[string]any {
	a := map[string]any{"metadata": map[string]any{"name": "bnk-demo"}, "spec": map[string]any{}}
	if pending {
		a["operation"] = map[string]any{"sync": map[string]any{"revision": "HEAD"}}
	}
	st := map[string]any{"sync": map[string]any{"status": "OutOfSync"}, "health": map[string]any{"status": "Progressing"}}
	if phase != "" {
		st["operationState"] = map[string]any{"phase": phase, "message": msg,
			"syncResult": map[string]any{"revision": "abc", "resources": res}}
	}
	a["status"] = st
	return a
}

func TestWaitOperation(t *testing.T) {
	hookFail := []ResourceResult{
		{Group: "batch", Kind: "Job", Namespace: "roksbnkargoctl-check", Name: "check-license", Status: "Synced",
			HookType: "Sync", HookPhase: "Failed", Message: "Job has reached the specified backoff limit"},
		{Kind: "Namespace", Name: "f5-bnk", Status: "Synced"},
	}
	for _, tc := range []struct {
		name      string
		seq       []map[string]any
		wantPhase string
		wantErr   string
		polls     int
	}{
		{
			name: "stale Succeeded while operation pending is not terminal",
			seq: []map[string]any{
				opApp(true, "Succeeded", "old sync", nil),
				opApp(true, "Running", "", nil),
				opApp(false, "Succeeded", "successfully synced (all tasks run)", nil),
			},
			wantPhase: "Succeeded", polls: 3,
		},
		{
			name: "Failed with a hook message",
			seq: []map[string]any{
				opApp(true, "", "", nil),
				opApp(true, "Running", "waiting for completion of hook batch/Job/check-license", nil),
				opApp(false, "Failed", "one or more synchronization tasks completed unsuccessfully", hookFail),
			},
			wantPhase: "Failed", wantErr: "check-license (Sync hook, Failed): Job has reached the specified backoff limit", polls: 3,
		},
		{
			name: "Failed but retrying (operation still set) keeps waiting",
			seq: []map[string]any{
				opApp(true, "Failed", "retrying", nil),
				opApp(false, "Error", "ComparisonError: boom", nil),
			},
			wantPhase: "Error", wantErr: "ended Error: ComparisonError: boom", polls: 2,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			i := 0
			c, rc := newTest(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
				if r.Method != "GET" || r.URL.Path != "/api/v1/applications/bnk-demo" || r.URL.Query().Get("project") != "default" {
					t.Errorf("unexpected %s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
				}
				a := tc.seq[min(i, len(tc.seq)-1)]
				i++
				writeJSON(w, 200, a)
			})
			seen := 0
			res, err := c.WaitOperation(context.Background(), "bnk-demo", 5*time.Second, func(*Application) { seen++ })
			if res == nil || res.Phase != tc.wantPhase {
				t.Fatalf("phase: got %+v err %v", res, err)
			}
			if tc.wantErr == "" && err != nil {
				t.Fatalf("unexpected err %v", err)
			}
			if tc.wantErr != "" {
				var oe *OperationError
				if !errors.As(err, &oe) || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err %v, want containing %q", err, tc.wantErr)
				}
			}
			if len(rc.reqs) != tc.polls || seen != tc.polls {
				t.Errorf("polls %d, progress calls %d, want %d", len(rc.reqs), seen, tc.polls)
			}
		})
	}
}

func TestWaitOperationTimeout(t *testing.T) {
	c, _ := newTest(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		writeJSON(w, 200, opApp(true, "Running", "", nil))
	})
	_, err := c.WaitOperation(context.Background(), "bnk-demo", 50*time.Millisecond, nil)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("want timeout, got %v", err)
	}
}

func TestDeleteQueryAndWaitGone(t *testing.T) {
	gets := 0
	c, rc := newTest(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		switch r.Method {
		case "DELETE":
			writeJSON(w, 200, map[string]any{})
		case "GET":
			gets++
			if gets < 3 {
				writeJSON(w, 200, opApp(false, "", "", nil))
				return
			}
			writeJSON(w, 404, map[string]any{"error": "applications.argoproj.io \"bnk-demo\" not found", "code": 5,
				"message": "applications.argoproj.io \"bnk-demo\" not found"})
		}
	})
	if err := c.Delete(context.Background(), "bnk-demo", true, "foreground"); err != nil {
		t.Fatal(err)
	}
	d := rc.reqs[0]
	if d.Method != "DELETE" || d.Path != "/api/v1/applications/bnk-demo" {
		t.Fatalf("got %s %s", d.Method, d.Path)
	}
	for _, want := range []string{"cascade=true", "propagationPolicy=foreground", "project=default"} {
		if !strings.Contains(d.Query, want) {
			t.Errorf("query %q lacks %q", d.Query, want)
		}
	}
	if err := c.WaitGone(context.Background(), "bnk-demo", 5*time.Second, nil); err != nil {
		t.Fatal(err)
	}
	if gets != 3 {
		t.Errorf("gets %d, want 3", gets)
	}

	// cascade=false must not send a propagation policy (Argo CD rejects the pair).
	if err := c.Delete(context.Background(), "bnk-demo", false, "foreground"); err != nil {
		t.Fatal(err)
	}
	if q := rc.reqs[len(rc.reqs)-1].Query; !strings.Contains(q, "cascade=false") || strings.Contains(q, "propagationPolicy") {
		t.Errorf("non-cascade query %q", q)
	}
}

func TestErrors(t *testing.T) {
	for _, tc := range []struct {
		code int
		body string
		want string
		nf   bool
	}{
		{401, `{"error":"invalid session: token is expired","code":16,"message":"invalid session: token is expired"}`, "token rejected (401): invalid session: token is expired", false},
		{403, `{"error":"permission denied","code":7,"message":"permission denied"}`, "token lacks permission for create application bnk-demo (403): permission denied", false},
		{404, `{"error":"not found","code":5,"message":"applications.argoproj.io \"bnk-demo\" not found"}`, "not found", true},
		{502, `<html>bad gateway</html>`, "HTTP 502: <html>bad gateway</html>", false},
	} {
		c, _ := newTest(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
			w.WriteHeader(tc.code)
			_, _ = io.WriteString(w, tc.body)
		})
		_, err := c.UpsertApplication(context.Background(), &Application{Metadata: ObjectMeta{Name: "bnk-demo"}})
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%d: got %v, want containing %q", tc.code, err, tc.want)
		}
		if IsNotFound(err) != tc.nf {
			t.Errorf("%d: IsNotFound=%v", tc.code, IsNotFound(err))
		}
	}
}

func TestDeleteRepositoryEscapes(t *testing.T) {
	c, rc := newTest(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		if r.Method == "GET" {
			writeJSON(w, 200, map[string]any{"items": []any{map[string]any{"repo": "https://git.example.com/org/bnk.git"}}})
			return
		}
		writeJSON(w, 200, map[string]any{})
	})
	if err := c.DeleteRepository(context.Background(), "https://git.example.com/org/bnk"); err != nil {
		t.Fatal(err)
	}
	if got := rc.reqs[1].RawPath; got != "/api/v1/repositories/https%3A%2F%2Fgit.example.com%2Forg%2Fbnk.git" {
		t.Errorf("path %s", got)
	}
	if err := c.DeleteRepository(context.Background(), "https://other/x.git"); !IsNotFound(err) {
		t.Errorf("missing repo: %v", err)
	}
}

func TestJobs(t *testing.T) {
	tree := &ApplicationTree{Nodes: []ResourceNode{
		{Group: "batch", Kind: "Job", Namespace: "ns", Name: "check-pre-install", Health: &HealthStatus{Status: "Healthy"}},
		{Group: "apps", Kind: "Deployment", Namespace: "ns", Name: "x"},
	}}
	op := &OperationState{SyncResult: &SyncOperationResult{Resources: []ResourceResult{
		{Group: "batch", Kind: "Job", Namespace: "ns", Name: "check-pre-install", HookType: "Sync", HookPhase: "Succeeded"},
		{Group: "batch", Kind: "Job", Namespace: "ns", Name: "check-license", HookType: "Sync", HookPhase: "Failed", Message: "boom"},
	}}}
	js := Jobs(tree, op)
	if len(js) != 2 || js[0].HookPhase != "Succeeded" || js[0].Health != "Healthy" || js[1].Health != "Missing" || js[1].Message != "boom" {
		t.Errorf("jobs: %+v", js)
	}
}

// A 403 is ambiguous (missing app without project, or no permission) and must not be
// taken as "deleted".
func TestWaitGoneForbiddenIsError(t *testing.T) {
	c, _ := newTest(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		writeJSON(w, 403, map[string]any{"error": "permission denied", "code": 7, "message": "permission denied"})
	})
	err := c.WaitGone(context.Background(), "bnk-demo", time.Second, nil)
	if err == nil || !IsForbidden(err) {
		t.Fatalf("want forbidden error, got %v", err)
	}
}

// Found live on Argo CD 3.5.1: a body-less DELETE without a Content-Type is
// rejected 415 "Invalid content type". This fake enforces the same rule for every
// mutating call the tool makes, so a regression on any of them fails here.
func TestMutatingCallsDeclareJSON(t *testing.T) {
	c, _ := newTest(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		if r.Method != http.MethodGet && r.Header.Get("Content-Type") != "application/json" {
			writeJSON(w, 415, map[string]any{"error": "Invalid content type", "message": "Invalid content type"})
			return
		}
		switch {
		case r.Method == "GET" && r.URL.Path == "/api/v1/clusters":
			writeJSON(w, 200, map[string]any{"items": []any{map[string]any{"server": "https://k"}}})
		case r.Method == "GET" && r.URL.Path == "/api/v1/repositories":
			writeJSON(w, 200, map[string]any{"items": []any{map[string]any{"repo": "https://g/r.git"}}})
		default:
			writeJSON(w, 200, map[string]any{})
		}
	})
	ctx := context.Background()
	if err := c.Delete(ctx, "app", true, "foreground"); err != nil {
		t.Errorf("Delete application: %v", err)
	}
	if err := c.DeleteCluster(ctx, "https://k"); err != nil {
		t.Errorf("DeleteCluster: %v", err)
	}
	if err := c.DeleteRepository(ctx, "https://g/r.git"); err != nil {
		t.Errorf("DeleteRepository: %v", err)
	}
}

// Issue #2: a custom Git host's key must reach Argo CD in the shape
// `argocd cert add-ssh` sends — one item per hostname, certData = the key field.
func TestUpsertSSHKnownHosts(t *testing.T) {
	c, rc := newTest(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		writeJSON(w, 200, map[string]any{"items": []any{}})
	})
	kh := "# comment\n" +
		"git.example.com,10.0.0.5 ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl\n" +
		"|1|abc=|def= ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl\n"
	n, skipped, err := c.UpsertSSHKnownHosts(context.Background(), kh)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 || len(skipped) != 1 {
		t.Fatalf("added=%d skipped=%v, want 2 added (both names) and the hashed entry skipped", n, skipped)
	}
	req := rc.reqs[len(rc.reqs)-1]
	if req.Method != "POST" || req.Path != "/api/v1/certificates" || req.Query != "upsert=true" {
		t.Fatalf("request: %s %s?%s", req.Method, req.Path, req.Query)
	}
	var body struct {
		Items []struct {
			ServerName, CertType, CertSubType string
			CertData                          []byte
		}
	}
	if err := json.Unmarshal(req.Body, &body); err != nil {
		t.Fatal(err)
	}
	if body.Items[0].ServerName != "git.example.com" || body.Items[1].ServerName != "10.0.0.5" ||
		body.Items[0].CertType != "ssh" || body.Items[0].CertSubType != "ssh-ed25519" ||
		string(body.Items[0].CertData) != "AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl" {
		t.Fatalf("certificate body wrong: %+v", body.Items)
	}
}

// Review finding: a wildcard or negated host pattern made Argo CD reject the
// whole certificate request. Patterns are skipped, like hashed names.
func TestUpsertSSHKnownHostsSkipsPatterns(t *testing.T) {
	c, rc := newTest(t, func(w http.ResponseWriter, r *http.Request, _ []byte) { writeJSON(w, 200, map[string]any{}) })
	kh := "*.corp.example,!bad.corp.example,git.corp.example ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl\n"
	n, skipped, err := c.UpsertSSHKnownHosts(context.Background(), kh)
	if err != nil || n != 1 || len(skipped) != 2 {
		t.Fatalf("n=%d skipped=%v err=%v; want only git.corp.example sent", n, skipped, err)
	}
	if strings.Contains(string(rc.reqs[len(rc.reqs)-1].Body), "*.corp") {
		t.Fatal("a pattern reached Argo CD")
	}
}

func ptrInt64(v int64) *int64 { return &v }

// Behaves like Argo CD 3.5.1 measured live: /api/version answers anyone;
// userinfo reports loggedIn only for the accepted token.
func TestCheckServerRejectsATokenTheServerDoesNotAccept(t *testing.T) {
	h := func(w http.ResponseWriter, r *http.Request, _ []byte) {
		switch r.URL.Path {
		case "/api/version":
			writeJSON(w, 200, map[string]string{"Version": "v3.5.1"})
		case "/api/v1/session/userinfo":
			if r.Header.Get("Authorization") == "Bearer tok" {
				writeJSON(w, 200, map[string]any{"loggedIn": true, "username": "roksbnkargoctl"})
			} else {
				writeJSON(w, 200, map[string]any{})
			}
		default:
			t.Errorf("unexpected %s", r.URL.Path)
		}
	}
	c, _ := newTest(t, h)
	if err := c.CheckServer(context.Background(), "3.3.0"); err != nil {
		t.Fatalf("accepted token refused: %v", err)
	}
	bad := New(c.server, "wrong", true, nil)
	err := bad.CheckServer(context.Background(), "3.3.0")
	if err == nil || !strings.Contains(err.Error(), "did not accept the API token") {
		t.Fatalf("a rejected token passed: %v", err)
	}
}

func TestManifestsReadsWhatTheApplicationWouldSync(t *testing.T) {
	c, rc := newTest(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		writeJSON(w, 200, map[string]any{"revision": "abc123", "manifests": []string{`{"kind":"Namespace"}`, `{"kind":"ConfigMap"}`}})
	})
	c.Project = "default"
	rev, ms, err := c.Manifests(context.Background(), "bnk-demo", "")
	if err != nil {
		t.Fatal(err)
	}
	if rev != "abc123" || len(ms) != 2 || ms[1] != `{"kind":"ConfigMap"}` {
		t.Fatalf("revision %q manifests %v", rev, ms)
	}
	if got := rc.reqs[0]; got.Method != "GET" || got.Path != "/api/v1/applications/bnk-demo/manifests" {
		t.Fatalf("request %s %s", got.Method, got.Path)
	}
}

// RepositoryBranches asks for the escaped repo's refs with the project first,
// then falls back to the global registration.
func TestRepositoryBranches(t *testing.T) {
	c, rc := newTest(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		if r.URL.Query().Get("appProject") != "" {
			writeJSON(w, 404, map[string]any{"message": "repo not in project"})
			return
		}
		writeJSON(w, 200, map[string]any{"branches": []string{"main", "dev"}, "tags": []string{"v1"}})
	})
	got, err := c.RepositoryBranches(context.Background(), "git@g.example.com:o/r.git", "bnk")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "main,dev" {
		t.Errorf("branches %v", got)
	}
	if len(rc.reqs) != 2 || rc.reqs[0].Query != "appProject=bnk" || rc.reqs[1].Query != "" {
		t.Fatalf("requests %+v", rc.reqs)
	}
	if p := rc.reqs[1].RawPath; !strings.HasPrefix(p, "/api/v1/repositories/") || !strings.HasSuffix(p, "/refs") || !strings.Contains(p, "o%2Fr.git") {
		t.Errorf("path %s, want the repo URL escaped as one segment", p)
	}
	c2, _ := newTest(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		writeJSON(w, 500, map[string]any{"message": "authentication required: Repository not found."})
	})
	_, err = c2.RepositoryBranches(context.Background(), "https://g/x.git", "bnk")
	if err == nil || strings.Count(err.Error(), "Repository not found") != 1 {
		t.Errorf("unreadable repo, same answer with and without the project, said once: %v", err)
	}
}

// A Helm OCI registry is registered as type helm with enableOCI and a name;
// a Git repository stays type git without them.
func TestUpsertRepositoryHelmOCI(t *testing.T) {
	c, rc := newTest(t, func(w http.ResponseWriter, r *http.Request, _ []byte) { writeJSON(w, 200, map[string]any{}) })
	ctx := context.Background()
	if _, err := c.UpsertRepository(ctx, Repo{URL: "harbor.x:8443/bnk/charts", HelmOCI: true, Name: "f5-lifecycle-operator", Username: "u", Password: "p"}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.UpsertRepository(ctx, Repo{URL: "https://g/r.git", Username: "git", Password: "t"}); err != nil {
		t.Fatal(err)
	}
	var helm, git map[string]any
	_ = json.Unmarshal(rc.reqs[0].Body, &helm)
	_ = json.Unmarshal(rc.reqs[1].Body, &git)
	if helm["type"] != "helm" || helm["enableOCI"] != true || helm["name"] != "f5-lifecycle-operator" || helm["repo"] != "harbor.x:8443/bnk/charts" || helm["username"] != "u" {
		t.Errorf("helm OCI body %v", helm)
	}
	if git["type"] != "git" || git["enableOCI"] != nil || git["name"] != nil {
		t.Errorf("git body %v", git)
	}
}

// A private CA is added as an https certificate for the host, without a port.
func TestUpsertTLSCert(t *testing.T) {
	c, rc := newTest(t, func(w http.ResponseWriter, r *http.Request, _ []byte) { writeJSON(w, 200, map[string]any{}) })
	if err := c.UpsertTLSCert(context.Background(), "harbor.x", "-----BEGIN CERTIFICATE-----\nX\n-----END CERTIFICATE-----\n"); err != nil {
		t.Fatal(err)
	}
	var body struct {
		Items []struct {
			ServerName, CertType string
			CertData             []byte
		}
	}
	_ = json.Unmarshal(rc.reqs[0].Body, &body)
	if rc.reqs[0].Path != "/api/v1/certificates" || rc.reqs[0].Query != "upsert=true" || len(body.Items) != 1 ||
		body.Items[0].ServerName != "harbor.x" || body.Items[0].CertType != "https" || !strings.HasPrefix(string(body.Items[0].CertData), "-----BEGIN CERTIFICATE") {
		t.Errorf("request %+v body %+v", rc.reqs[0], body)
	}
	if err := c.UpsertTLSCert(context.Background(), "harbor.x:8443", "x"); err == nil {
		t.Error("a server name with a port was accepted; Argo CD matches TLS certificates by host")
	}
}

// A multi-source sync pins one source: revisions + sourcePositions, no
// revision; a single-source sync keeps revision.
func TestSyncPinsOneSource(t *testing.T) {
	c, rc := newTest(t, func(w http.ResponseWriter, r *http.Request, _ []byte) { writeJSON(w, 200, map[string]any{}) })
	ctx := context.Background()
	if _, err := c.Sync(ctx, "a", SyncOptions{Revision: "abc", SourcePosition: 3}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Sync(ctx, "a", SyncOptions{Revision: "abc"}); err != nil {
		t.Fatal(err)
	}
	var multi, single map[string]any
	_ = json.Unmarshal(rc.reqs[0].Body, &multi)
	_ = json.Unmarshal(rc.reqs[1].Body, &single)
	if multi["revision"] != nil || fmt.Sprint(multi["revisions"]) != "[abc]" || fmt.Sprint(multi["sourcePositions"]) != "[3]" {
		t.Errorf("multi-source body %v", multi)
	}
	if single["revision"] != "abc" || single["revisions"] != nil {
		t.Errorf("single-source body %v", single)
	}
}

// ManifestsAt pins one source's revision; SourceRevisions refreshes and reads
// status.sync.revisions.
func TestManifestsAtAndSourceRevisions(t *testing.T) {
	c, rc := newTest(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		if strings.HasSuffix(r.URL.Path, "/manifests") {
			writeJSON(w, 200, map[string]any{"manifests": []string{"{}"}})
			return
		}
		writeJSON(w, 200, map[string]any{"metadata": map[string]any{"name": "a"}, "status": map[string]any{"sync": map[string]any{"revisions": []string{"v1", "sha1"}}}})
	})
	ctx := context.Background()
	rev, ms, err := c.ManifestsAt(ctx, "a", "", 2, "sha1")
	if err != nil || rev != "sha1" || len(ms) != 1 {
		t.Fatalf("%q %v %v", rev, ms, err)
	}
	if q := rc.reqs[0].Query; !strings.Contains(q, "sourcePositions=2") || !strings.Contains(q, "revisions=sha1") {
		t.Errorf("manifests query %q", q)
	}
	revs, err := c.SourceRevisions(ctx, "a", "")
	if err != nil || strings.Join(revs, ",") != "v1,sha1" {
		t.Fatalf("%v %v", revs, err)
	}
	if q := rc.reqs[1].Query; !strings.Contains(q, "refresh=normal") {
		t.Errorf("refresh query %q", q)
	}
}
