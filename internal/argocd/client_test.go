package argocd

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
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
				Retry:       &RetryStrategy{Limit: 2, Backoff: &Backoff{Duration: "10s", Factor: 2, MaxDuration: "3m"}},
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
