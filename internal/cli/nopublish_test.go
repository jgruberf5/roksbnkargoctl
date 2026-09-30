package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/jgruberf5/roksbnkargoctl/internal/argocd"
	"github.com/jgruberf5/roksbnkargoctl/internal/config"
	"github.com/jgruberf5/roksbnkargoctl/internal/render"
)

// argoView turns YAML files into what Argo CD's manifests API returns: one
// JSON document per object, with the tracking annotation and instance label
// its repo server adds.
func argoView(t *testing.T, files map[string]string) []string {
	t.Helper()
	var out []string
	for _, y := range files {
		var o map[string]any
		if err := yaml.Unmarshal([]byte(y), &o); err != nil {
			t.Fatal(err)
		}
		md := o["metadata"].(map[string]any)
		a, _ := md["annotations"].(map[string]any)
		if a == nil {
			a = map[string]any{}
		}
		a["argocd.argoproj.io/tracking-id"] = "bnk-demo:x"
		md["annotations"] = a
		l, _ := md["labels"].(map[string]any)
		if l == nil {
			l = map[string]any{}
		}
		l["app.kubernetes.io/instance"] = "bnk-demo"
		md["labels"] = l
		b, _ := json.Marshal(o)
		out = append(out, string(b))
	}
	return out
}

// chartView is what Argo CD renders from the charts' Helm sources, built from
// the in-memory render (not render's files on disk), with Secret values masked
// as Argo CD's manifests API masks them.
func chartView(t *testing.T, out *render.Output) map[string]string {
	t.Helper()
	view := map[string]string{}
	for _, ch := range out.Charts {
		for i, o := range ch.Objects {
			if o.Kind() == "Secret" {
				o = render.MaskLikeArgoCD(o)
			}
			b, err := o.YAML()
			if err != nil {
				t.Fatal(err)
			}
			view[fmt.Sprintf("%s-%d", ch.Name, i)] = string(b)
		}
	}
	if len(view) == 0 {
		t.Fatal("the render has no charts")
	}
	return view
}

// The export flow end to end: the zip `export` writes, committed to Git and read
// back by Argo CD (our manifests from Git, the charts from their Helm sources),
// matches the render it came from.
func TestAnExportedRenderMatchesWhatArgoCDReadsBack(t *testing.T) {
	dir, out := renderedWorkspace(t)
	var buf bytes.Buffer
	if _, err := writeExport(&buf, dir, "bnk/demo", "r", nil); err != nil {
		t.Fatal(err)
	}
	argo := chartView(t, out)
	for name, body := range readZip(t, buf.Bytes()) {
		// Argo CD applies git.path without recursing: values/ is only read by
		// the Helm sources.
		if strings.HasPrefix(name, "bnk/demo/") && !strings.HasPrefix(name, "bnk/demo/values/") {
			argo[name] = body
		}
	}
	rendered, err := expectedInArgoCD(filepath.Dir(dir))
	if err != nil {
		t.Fatal(err)
	}
	diffs, err := gitMatchesRender(argoView(t, argo), rendered)
	if err != nil {
		t.Fatal(err)
	}
	if len(diffs) != 0 {
		t.Fatalf("an unchanged export does not match its render:\n%s", strings.Join(diffs, "\n"))
	}
}

// A partial, stale or foreign upload is reported object by object.
func TestGitMatchesRenderReportsEachDifference(t *testing.T) {
	dir, out := renderedWorkspace(t)
	rendered, err := expectedInArgoCD(filepath.Dir(dir))
	if err != nil {
		t.Fatal(err)
	}
	files := chartView(t, out)
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		if e.IsDir() {
			continue
		}
		b, _ := os.ReadFile(dir + "/" + e.Name())
		files[e.Name()] = string(b)
	}
	var missing, changed string
	for n, y := range files {
		switch {
		case missing == "" && strings.Contains(y, "kind: CNEInstance"):
			missing = n
		case changed == "" && strings.Contains(y, "kind: Deployment") && strings.Contains(y, "image:"):
			changed = n
		}
	}
	delete(files, missing)
	files[changed] = strings.Replace(files[changed], "image: ", "image: stale.example.com/", 1)
	files["zzz-extra.yaml"] = "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: leftover\n  namespace: f5-bnk\n"
	diffs, err := gitMatchesRender(argoView(t, files), rendered)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(diffs, "\n")
	for _, want := range []string{"missing in Git: k8s.f5.com/CNEInstance", "different in Git: apps/Deployment", "in Git but not in this render: /ConfigMap f5-bnk/leftover"} {
		if !strings.Contains(got, want) {
			t.Errorf("no %q in:\n%s", want, got)
		}
	}
	if len(diffs) != 3 {
		t.Errorf("want exactly 3 differences, got %d:\n%s", len(diffs), got)
	}
}

// --no-publish tolerates a missing Git credential (nothing is pushed), and
// only that error.
func TestMissingGitCredentialIsRecognised(t *testing.T) {
	c := &config.Config{Git: config.Git{TokenEnv: "ROKSBNKARGOCTL_TEST_NO_SUCH_TOKEN"}}
	t.Setenv("ROKSBNKARGOCTL_TEST_NO_SUCH_TOKEN", "")
	_, _, err := gitCredentials(c)
	if !missingGitCredential(err) {
		t.Fatalf("an unset token is not recognised as a missing credential: %v", err)
	}
	c.Git.SSHKeyFile = "/nonexistent/key"
	_, _, err = gitCredentials(c)
	if err == nil || missingGitCredential(err) {
		t.Fatalf("an unreadable SSH key must stay an error: %v", err)
	}
}

var _ = render.Object{}

// checkGitMatchesRender reads the commit the Git source resolves to and asks
// for the manifests pinned to it (so the comparison and the sync see the same
// commit), then compares every object, charts included.
func TestCheckGitMatchesRenderPinsTheGitSource(t *testing.T) {
	_, out := renderedWorkspace(t)
	ws := &config.Workspace{Name: "demo", Dir: t.TempDir()}
	if err := render.Write(out, ws.ManifestsDir()); err != nil {
		t.Fatal(err)
	}
	gitFiles := map[string]string{}
	for i, o := range out.Git {
		b, _ := o.YAML()
		gitFiles[fmt.Sprint(i)] = string(b)
	}
	argo := argoView(t, gitFiles)
	argo = append(argo, argoView(t, chartView(t, out))...)
	var manifestsQuery string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/manifests") {
			manifestsQuery = r.URL.RawQuery
			_ = json.NewEncoder(w).Encode(map[string]any{"manifests": argo})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"metadata": map[string]any{"name": "bnk-demo"},
			"status": map[string]any{"sync": map[string]any{"revisions": []string{"v1.17.3", "v2.30", "c0ffee1234"}}}})
	}))
	defer srv.Close()
	c := &config.Config{ArgoCD: config.ArgoCD{Application: "bnk-demo"}, Git: config.Git{URL: "https://g/r.git", Branch: "main", Path: "bnk/demo"}}
	s, _ := initSession(c)
	s.ws = ws
	rev, err := checkGitMatchesRender(context.Background(), s, argocd.New(srv.URL, "t", true, nil), 3)
	if err != nil {
		t.Fatal(err)
	}
	if rev != "c0ffee1234" {
		t.Errorf("revision %q, want the Git source's resolved commit", rev)
	}
	if !strings.Contains(manifestsQuery, "sourcePositions=3") || !strings.Contains(manifestsQuery, "revisions=c0ffee1234") {
		t.Errorf("manifests were not pinned to the Git source's commit: %q", manifestsQuery)
	}
	// No source revisions yet (not compared): an error, not a panic.
	empty := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"metadata": map[string]any{"name": "bnk-demo"}, "status": map[string]any{"sync": map[string]any{}}})
	}))
	defer empty.Close()
	if _, err := checkGitMatchesRender(context.Background(), s, argocd.New(empty.URL, "t", true, nil), 3); err == nil || !strings.Contains(err.Error(), "reports 0 source revisions") {
		t.Errorf("no revisions: %v", err)
	}
	// One object changed in what Argo CD renders: refused.
	argo[0] = strings.Replace(argo[0], `"kind":`, `"x":1,"kind":`, 1)
	if _, err := checkGitMatchesRender(context.Background(), s, argocd.New(srv.URL, "t", true, nil), 3); err == nil || !strings.Contains(err.Error(), "NOT synced") {
		t.Errorf("a difference was not refused: %v", err)
	}
}
