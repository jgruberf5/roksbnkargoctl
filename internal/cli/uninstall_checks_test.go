package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/jgruberf5/roksbnkargoctl/internal/config"
	"github.com/jgruberf5/roksbnkargoctl/internal/far"
	"github.com/jgruberf5/roksbnkargoctl/internal/render"
)

// Argo CD's upsert replaces the finalizer list. The controller's own
// pre/post-delete finalizers (and their /cleanup stages) must survive a
// re-install; anything else not ours is dropped.
func TestMergeFinalizersKeepsArgoHookFinalizers(t *testing.T) {
	live := []string{
		"resources-finalizer.argocd.argoproj.io",
		"pre-delete-finalizer.argocd.argoproj.io",
		"pre-delete-finalizer.argocd.argoproj.io/cleanup",
		"post-delete-finalizer.argocd.argoproj.io",
		"post-delete-finalizer.argocd.argoproj.io/cleanup",
		"example.com/someone-elses",
	}
	got := mergeFinalizers(live, []string{"resources-finalizer.argocd.argoproj.io"})
	want := []string{
		"resources-finalizer.argocd.argoproj.io",
		"pre-delete-finalizer.argocd.argoproj.io",
		"pre-delete-finalizer.argocd.argoproj.io/cleanup",
		"post-delete-finalizer.argocd.argoproj.io",
		"post-delete-finalizer.argocd.argoproj.io/cleanup",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v\nwant %v", got, want)
	}
	if got := mergeFinalizers(nil, []string{"resources-finalizer.argocd.argoproj.io"}); len(got) != 1 {
		t.Fatalf("a new Application gets ours only, got %v", got)
	}
}

func chartTGZ(t *testing.T, name string) []byte {
	t.Helper()
	files := map[string]string{
		name + "/Chart.yaml":        "apiVersion: v2\nname: " + name + "\nversion: 0.1.0\n",
		name + "/templates/cm.yaml": "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: " + name + "\ndata: {}\n",
		name + "/values.yaml":       "{}\n",
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for n, b := range files {
		_ = tw.WriteHeader(&tar.Header{Name: n, Mode: 0o644, Size: int64(len(b)), Typeflag: tar.TypeReg})
		_, _ = tw.Write([]byte(b))
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

// renderedGitDir renders a workspace the way `render` does and writes it.
func renderedGitDir(t *testing.T) string {
	c := &config.Config{IBMCloud: config.IBMCloud{Region: "us-east"}, Cluster: "c", TransitGateway: "t",
		COS: config.COS{Bucket: "b"}, ArgoCD: config.ArgoCD{Server: "https://a"}, Git: config.Git{URL: "https://g/r.git"}}
	c.Defaults("ws")
	c.Resolved = &config.Resolved{VPCName: "v", TrustedProfileID: "p", ArgoCDClusterServer: "https://k"}
	out, err := render.Render(render.Inputs{Config: c, Workspace: "ws", CheckImage: "ghcr.io/x/check:dev", RunID: "r",
		Manifest: &far.Manifest{Version: config.BNKVersion,
			Charts: []far.Artifact{{Name: "charts/f5-lifecycle-operator", Version: "1"}},
			Images: []far.Artifact{{Name: "images/f5-lifecycle-operator", Version: "1"}}},
		FLOChart: chartTGZ(t, "flo"), CertManagerChart: chartTGZ(t, "cm"),
		Secrets: render.Secrets{PullHost: "h", PullUsername: "u", PullPassword: "pppppppp", JWT: "a.b.c"}})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := render.Write(out, dir); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(dir, "git")
}

// The CLI's copy of a hook must be the same check (image, args, account,
// deadline) minus everything that makes Argo CD treat it as a hook, and must not
// share the hook's name.
func TestCheckJobFromManifestsIsTheHookWithoutArgo(t *testing.T) {
	dir := renderedGitDir(t)
	objs, err := readYAMLDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"pre-uninstall", "post-uninstall"} {
		j, err := checkJobFromManifests(dir, mode)
		if err != nil {
			t.Fatal(err)
		}
		var hookName string
		var hookArgs []any
		for _, o := range objs {
			md := o["metadata"].(map[string]any)
			if o.Kind() == "Job" && md["labels"].(map[string]any)["roksbnkargoctl.io/check-mode"] == mode {
				hookName = md["name"].(string)
				c := o["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)["containers"].([]any)[0]
				hookArgs = c.(map[string]any)["args"].([]any)
				if md["annotations"].(map[string]any)["argocd.argoproj.io/hook"] == nil {
					t.Fatalf("%s: fixture is not a hook", mode)
				}
			}
		}
		if j.Name == hookName || !strings.HasPrefix(j.Name, hookName) {
			t.Fatalf("%s: CLI Job named %q, hook %q", mode, j.Name, hookName)
		}
		for k := range j.Annotations {
			if strings.HasPrefix(k, "argocd.argoproj.io/") {
				t.Fatalf("%s: kept Argo CD annotation %s", mode, k)
			}
		}
		args := j.Spec.Template.Spec.Containers[0].Args
		if args[0] != mode || len(args) != len(hookArgs) {
			t.Fatalf("%s: args %v, hook %v", mode, args, hookArgs)
		}
		if j.Namespace != render.CheckNamespace || j.Spec.Template.Spec.ServiceAccountName != "check" || j.Spec.ActiveDeadlineSeconds == nil {
			t.Fatalf("%s: namespace %q sa %q deadline %v", mode, j.Namespace, j.Spec.Template.Spec.ServiceAccountName, j.Spec.ActiveDeadlineSeconds)
		}
	}
}

func TestNamespacesRemaining(t *testing.T) {
	c := &config.Config{}
	c.Defaults("ws")
	k := fake.NewSimpleClientset(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "f5-utils"}, Status: corev1.NamespaceStatus{Phase: corev1.NamespaceTerminating}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "unrelated"}})
	left, err := namespacesRemaining(context.Background(), k, bnkNamespaces(c))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(left, []string{"f5-utils (Terminating)"}) {
		t.Fatalf("got %v", left)
	}
	if !slices.Contains(bnkNamespaces(c), "cert-manager") {
		t.Fatal("an installed cert-manager's namespace must be checked")
	}
}

// fakeUninstall records the order of the steps. remaining returns the next
// entry of left on each call.
type fakeUninstall struct {
	calls   []string
	exists  bool
	failPre bool
	left    [][]string
}

func (f *fakeUninstall) steps(force bool) uninstallSteps {
	return uninstallSteps{p: printer{io.Discard}, force: force,
		appExists: func() (bool, error) { return f.exists, nil },
		deleteApp: func(func()) error { f.calls = append(f.calls, "delete"); return nil },
		runCheck: func(mode string) error {
			f.calls = append(f.calls, mode)
			if mode == "pre-uninstall" && f.failPre {
				return errors.New("ipams remain")
			}
			return nil
		},
		remaining: func() ([]string, error) {
			f.calls = append(f.calls, "verify")
			l := f.left[0]
			if len(f.left) > 1 {
				f.left = f.left[1:]
			}
			return l, nil
		},
	}
}

// The live failure: Argo CD skipped both hooks and uninstall still said
// "uninstalled". The CLI runs the checks around the delete itself.
func TestUninstallRunsTheChecksAroundTheDelete(t *testing.T) {
	f := &fakeUninstall{exists: true, left: [][]string{{"f5-bnk (Active)"}, nil}}
	if err := f.steps(false).run(func() {}); err != nil {
		t.Fatal(err)
	}
	want := []string{"pre-uninstall", "delete", "verify", "post-uninstall", "verify"}
	if !slices.Equal(f.calls, want) {
		t.Fatalf("got %v, want %v", f.calls, want)
	}
}

func TestUninstallDoesNotDeleteWhenPreUninstallFails(t *testing.T) {
	f := &fakeUninstall{exists: true, failPre: true, left: [][]string{nil}}
	if err := f.steps(false).run(func() {}); err == nil || !strings.Contains(err.Error(), "NOT deleted") {
		t.Fatalf("want a refusal, got %v", err)
	}
	if slices.Contains(f.calls, "delete") {
		t.Fatalf("deleted the Application after a failed drain: %v", f.calls)
	}
	f = &fakeUninstall{exists: true, failPre: true, left: [][]string{nil}}
	if err := f.steps(true).run(func() {}); err != nil || !slices.Contains(f.calls, "delete") {
		t.Fatalf("--force must delete anyway: %v %v", err, f.calls)
	}
}

func TestUninstallFailsWhileNamespacesRemain(t *testing.T) {
	f := &fakeUninstall{exists: true, left: [][]string{{"f5-bnk (Active)"}, {"f5-bnk (Terminating)"}}}
	err := f.steps(false).run(func() {})
	if err == nil || !strings.Contains(err.Error(), "f5-bnk (Terminating)") {
		t.Fatalf("want the remaining namespace reported, got %v", err)
	}
}

// Re-running after an interrupted uninstall: no Application, BNK namespaces
// still there — post-uninstall finishes the job.
func TestUninstallRerunFinishesWithoutTheApplication(t *testing.T) {
	f := &fakeUninstall{exists: false, left: [][]string{{"cert-manager (Active)"}, nil}}
	if err := f.steps(false).run(func() {}); err != nil {
		t.Fatal(err)
	}
	if want := []string{"verify", "post-uninstall", "verify"}; !slices.Equal(f.calls, want) {
		t.Fatalf("got %v, want %v", f.calls, want)
	}
}
