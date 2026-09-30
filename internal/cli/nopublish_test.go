package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

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

// The export flow end to end: the zip `export` writes, committed to Git and read
// back by Argo CD, matches the render it came from.
func TestAnExportedRenderMatchesWhatArgoCDReadsBack(t *testing.T) {
	dir := renderedGitDir(t)
	var buf bytes.Buffer
	if _, err := writeExport(&buf, dir, "bnk/demo", "r", nil); err != nil {
		t.Fatal(err)
	}
	inGit := map[string]string{}
	for name, body := range readZip(t, buf.Bytes()) {
		if strings.HasPrefix(name, "bnk/demo/") {
			inGit[name] = body
		}
	}
	rendered, err := readYAMLDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	diffs, err := gitMatchesRender(argoView(t, inGit), rendered)
	if err != nil {
		t.Fatal(err)
	}
	if len(diffs) != 0 {
		t.Fatalf("an unchanged export does not match its render:\n%s", strings.Join(diffs, "\n"))
	}
}

// A partial, stale or foreign upload is reported object by object.
func TestGitMatchesRenderReportsEachDifference(t *testing.T) {
	dir := renderedGitDir(t)
	rendered, err := readYAMLDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{}
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
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
