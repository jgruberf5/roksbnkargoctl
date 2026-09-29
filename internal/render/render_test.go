package render

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"os/exec"
	"strings"
	"testing"

	"github.com/jgruberf5/roksbnkargoctl/internal/config"
	"github.com/jgruberf5/roksbnkargoctl/internal/far"
)

// fakeChart builds a tiny chart that, like FLO, renders a CRD, a Deployment and a
// Secret (FLO renders external-otelsvr-secret), with no namespace on the
// Deployment (Helm leaves namespacing to the installer).
func fakeChart(t *testing.T, name string) []byte {
	t.Helper()
	files := map[string]string{
		name + "/Chart.yaml": "apiVersion: v2\nname: " + name + "\nversion: 0.1.0\n",
		name + "/templates/all.yaml": `apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: things.k8s.f5.com
spec:
  group: k8s.f5.com
  names: {kind: Thing, plural: things}
  scope: Namespaced
  versions: [{name: v1, served: true, storage: true, schema: {openAPIV3Schema: {type: object}}}]
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: {{ .Release.Name }}-op
spec:
  selector: {matchLabels: {app: x}}
  template:
    metadata: {labels: {app: x}}
    spec: {containers: [{name: c, image: "{{ .Values.image.repository }}/x:1"}]}
---
apiVersion: v1
kind: Secret
metadata:
  name: chart-shipped-secret
data:
  tls.key: c2VjcmV0
`,
		name + "/values.yaml": "image: {repository: default}\n",
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

func baseConfig(mode, source string) *config.Config {
	c := &config.Config{
		IBMCloud: config.IBMCloud{Region: "us-east"}, Cluster: "c", TransitGateway: "t",
		BNK:      config.BNK{Mode: mode},
		Registry: config.Registry{Source: source, Mirror: config.Mirror{Host: "harbor.x:8443", Prefix: "bnk-mirror", Username: "robot"}},
		COS:      config.COS{Bucket: "b"}, ArgoCD: config.ArgoCD{Server: "https://argo"}, Git: config.Git{URL: "https://git/r.git"},
	}
	c.Defaults("ws")
	c.Resolved = &config.Resolved{VPCName: "vpc1", TrustedProfileID: "Profile-1", ArgoCDClusterServer: "https://roks:6443"}
	return c
}

func manifest() *far.Manifest {
	return &far.Manifest{Version: config.BNKVersion,
		Charts: []far.Artifact{{Name: "charts/f5-lifecycle-operator", Version: "v2.30.0-0.5.2"}},
		Images: []far.Artifact{{Name: "images/f5-lifecycle-operator", Version: "v2.30.0-0.5.2"}}}
}

func doRender(t *testing.T, c *config.Config, mutate func(*Inputs)) *Output {
	t.Helper()
	in := Inputs{Config: c, Workspace: "ws", Manifest: manifest(), FLOChart: fakeChart(t, "flo"),
		CertManagerChart: fakeChart(t, "cm"), KubeVersion: "v1.34.0", CheckImage: "ghcr.io/x/check:1", RunID: "r1",
		Secrets: Secrets{PullHost: "repo.f5.com", PullUsername: "_json_key_base64", PullPassword: "FARKEYVALUE-123456", JWT: "hdr.payloadJWT.sig"}}
	if c.BNK.Mode == config.ModeDisconnected {
		in.FLPURL = "https://10.248.0.4:8443"
		in.Secrets.FLPCAPEM = "-----BEGIN CERTIFICATE-----\nX\n-----END CERTIFICATE-----\n"
	}
	if mutate != nil {
		mutate(&in)
	}
	out, err := Render(in)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func find(objs []Object, kind, name string) Object {
	for _, o := range objs {
		if o.Kind() == kind && o.Name() == name {
			return o
		}
	}
	return nil
}

// DESIGN.md: no secret value is ever published. Every Secret — ours and any a
// chart renders — must be Direct, and no secret VALUE may appear in Git.
func TestNoSecretReachesGit(t *testing.T) {
	out := doRender(t, baseConfig(config.ModeConnected, config.SourceFAR), nil)
	for _, o := range out.Git {
		if o.Kind() == "Secret" {
			t.Fatalf("Secret %s/%s is in the Git set", o.Namespace(), o.Name())
		}
		b, _ := o.YAML()
		for _, v := range []string{"FARKEYVALUE-123456", "hdr.payloadJWT.sig"} {
			if strings.Contains(string(b), v) {
				t.Fatalf("%s leaked into %s/%s", v, o.Kind(), o.Name())
			}
		}
	}
	if find(out.Direct, "Secret", "chart-shipped-secret") == nil {
		t.Fatal("a chart-rendered Secret must be moved to the direct set, not dropped")
	}
	if find(out.Direct, "Secret", LicenseJWTSecret) == nil || find(out.Direct, "Secret", "far-secret") == nil {
		t.Fatal("the JWT and FAR pull secrets must be written directly")
	}
}

// The guard must actually fire: a secret value smuggled into a Git object
// (here, as an annotation) fails the render.
func TestSecretGuardFires(t *testing.T) {
	c := baseConfig(config.ModeConnected, config.SourceFAR)
	c.ArgoCD.Project = "FARKEYVALUE-123456" // lands in the Application, not Git — must not trip
	_ = doRender(t, c, nil)
	c = baseConfig(config.ModeConnected, config.SourceFAR)
	c.BNK.NADAddress = "FARKEYVALUE-123456" // lands in the NAD, which IS Git
	in := Inputs{Config: c, Workspace: "ws", Manifest: manifest(), FLOChart: fakeChart(t, "flo"), CertManagerChart: fakeChart(t, "cm"),
		CheckImage: "i", RunID: "r", Secrets: Secrets{PullHost: "h", PullUsername: "u", PullPassword: "FARKEYVALUE-123456", JWT: "a.b.c"}}
	if _, err := Render(in); err == nil || !strings.Contains(err.Error(), "refusing to publish") {
		t.Fatalf("expected the no-secrets-in-Git guard to refuse, got %v", err)
	}
}

// Argo CD must never delete BNK namespaces or CRDs itself (a namespace stuck on
// an F5 finalizer would hang the Application before PostDelete runs).
func TestNamespacesAndCRDsSurviveArgoDeletion(t *testing.T) {
	out := doRender(t, baseConfig(config.ModeConnected, config.SourceFAR), nil)
	for _, o := range out.Git {
		if o.Kind() == "Namespace" || o.Kind() == "CustomResourceDefinition" {
			if !strings.Contains(o.Annotation(AnnoSyncOptions), "Delete=false") {
				t.Errorf("%s %s lacks Delete=false", o.Kind(), o.Name())
			}
		}
	}
	// The check namespace must NOT be in Git at all: PostDelete runs there.
	for _, o := range out.Git {
		if o.Kind() == "Namespace" && o.Name() == CheckNamespace {
			t.Fatal("the check namespace must be out of band so it outlives the Application")
		}
	}
}

func TestWaveOrder(t *testing.T) {
	out := doRender(t, baseConfig(config.ModeConnected, config.SourceFAR), nil)
	wave := func(kind, name string) int {
		o := find(out.Git, kind, name)
		if o == nil {
			t.Fatalf("%s %s not rendered", kind, name)
		}
		return o.Wave()
	}
	order := []int{
		wave("Namespace", "f5-bnk"),
		wave("Job", "check-pre-install"),
		wave("Deployment", "cert-manager-op"),
		wave("Job", "check-cert-manager-ready"),
		wave("ClusterIssuer", ClusterIssuerSelfSigned),
		wave("Certificate", CACertName),
		wave("ClusterIssuer", ClusterIssuerCA),
		wave("Deployment", "flo-op"),
		wave("CNEManifest", CNEManifestName(config.BNKVersion)),
		wave("CNEInstance", CNEInstanceName),
		wave("Job", "check-license"),
	}
	for i := 1; i < len(order); i++ {
		if order[i] <= order[i-1] {
			t.Fatalf("waves out of order at step %d: %v", i, order)
		}
	}
	if w := wave("Deployment", SweepDeployment); w >= wave("Deployment", "flo-op") {
		t.Fatal("the Gateway API sweep must be running before FLO's CRD installer")
	}
}

func TestConnectedVsDisconnected(t *testing.T) {
	con := doRender(t, baseConfig(config.ModeConnected, config.SourceFAR), nil)
	lic := find(con.Git, "Job", "check-license")
	args := strings.Join(jobArgs(lic), " ")
	if !strings.Contains(args, "--mode=connected") || strings.Contains(args, "--flp-url") {
		t.Fatalf("connected license args: %s", args)
	}
	dis := doRender(t, baseConfig(config.ModeDisconnected, config.SourceFAR), nil)
	args = strings.Join(jobArgs(find(dis.Git, "Job", "check-license")), " ")
	for _, want := range []string{"--mode=f5licenseproxy", "--flp-url=https://10.248.0.4:8443", "--flp-ca-path=" + FLPRootCAPath} {
		if !strings.Contains(args, want) {
			t.Errorf("disconnected license args missing %s: %s", want, args)
		}
	}
	if find(dis.Direct, "Secret", FLPRootCASecret) == nil {
		t.Error("disconnected mode must write the FLP root CA secret")
	}
	probe := strings.Join(jobArgs(find(dis.Git, "DaemonSet", ProbeDaemonSet)), " ")
	if !strings.Contains(probe, "10.248.0.4:8443,tls,insecure") || strings.Contains(probe, "product.apis.f5.com") {
		t.Errorf("disconnected probe targets wrong: %s", probe)
	}
}

func TestMirrorRedirectsEveryPull(t *testing.T) {
	c := baseConfig(config.ModeConnected, config.SourceMirror)
	out := doRender(t, c, func(in *Inputs) {
		in.Secrets = Secrets{PullHost: "harbor.x:8443", PullUsername: "robot", PullPassword: "MIRRORPASS-99", JWT: "a.b.c"}
		in.CheckImage = "harbor.x:8443/bnk-mirror/jgruberf5/roksbnkargoctl-check:1"
	})
	flo := find(out.Git, "Deployment", "flo-op")
	b, _ := flo.YAML()
	if !strings.Contains(string(b), "harbor.x:8443/bnk-mirror/images/x:1") {
		t.Fatalf("FLO image not redirected to the mirror:\n%s", b)
	}
	cne := find(out.Git, "CNEInstance", CNEInstanceName)
	reg := cne["spec"].(map[string]any)["registry"].(map[string]any)
	if reg["uri"] != "harbor.x:8443/bnk-mirror" {
		t.Fatalf("CNEInstance registry.uri = %v", reg["uri"])
	}
	for _, ns := range []string{"f5-bnk", "f5-utils", "cert-manager", CheckNamespace} {
		found := false
		for _, o := range out.Direct {
			if o.Kind() == "Secret" && o.Name() == "mirror-secret" && o.Namespace() == ns {
				found = true
			}
		}
		if !found {
			t.Errorf("no mirror-secret in %s", ns)
		}
	}
}

func jobArgs(o Object) []string {
	spec := o["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
	c := spec["containers"].([]any)[0].(map[string]any)
	var out []string
	for _, a := range c["args"].([]any) {
		out = append(out, a.(string))
	}
	return out
}

func TestRenderIsDeterministic(t *testing.T) {
	a := doRender(t, baseConfig(config.ModeConnected, config.SourceFAR), nil)
	b := doRender(t, baseConfig(config.ModeConnected, config.SourceFAR), nil)
	if len(a.Git) != len(b.Git) {
		t.Fatal("object counts differ")
	}
	for i := range a.Git {
		x, _ := a.Git[i].YAML()
		y, _ := b.Git[i].YAML()
		if !bytes.Equal(x, y) {
			t.Fatalf("object %d differs between identical renders: an unchanged install would commit", i)
		}
	}
}

// A re-sync must not fail pre-install on the FLO this Application created.
func TestPreInstallKnowsItsApplication(t *testing.T) {
	out := doRender(t, baseConfig(config.ModeConnected, config.SourceFAR), nil)
	args := strings.Join(jobArgs(find(out.Git, "Job", "check-pre-install")), " ")
	if !strings.Contains(args, "--argocd-app=bnk-ws") {
		t.Fatalf("pre-install is not told its Application: %s", args)
	}
}

// Found live: an empty annotations map on a server-side-applied Namespace keeps
// OpenShift from adding the SCC annotations, and every pod in it is rejected.
// No rendered object may carry an empty metadata map.
func TestNoEmptyMetadataMaps(t *testing.T) {
	out := doRender(t, baseConfig(config.ModeConnected, config.SourceFAR), nil)
	for _, set := range [][]Object{out.Git, out.Direct} {
		for _, o := range set {
			m := o["metadata"].(map[string]any)
			for _, k := range []string{"annotations", "labels"} {
				if v, ok := m[k].(map[string]any); ok && len(v) == 0 {
					t.Errorf("%s %s/%s has an empty metadata.%s", o.Kind(), o.Namespace(), o.Name(), k)
				}
			}
		}
	}
}

// IBM ROKS runs only deploymentSize Tiny (larger sizes need hugepages ROKS cannot
// allocate). Every Application this tool creates must say Tiny.
func TestCNEInstanceIsAlwaysTiny(t *testing.T) {
	for _, mode := range []string{config.ModeConnected, config.ModeDisconnected} {
		c := baseConfig(mode, config.SourceFAR)
		c.BNK.TMMReplicas = 3
		out := doRender(t, c, nil)
		spec := find(out.Git, "CNEInstance", CNEInstanceName)["spec"].(map[string]any)
		if spec["deploymentSize"] != "Tiny" {
			t.Fatalf("%s: deploymentSize = %v, want Tiny", mode, spec["deploymentSize"])
		}
		if spec["tmmReplicas"] != 3 {
			t.Fatalf("%s: tmmReplicas = %v, want 3", mode, spec["tmmReplicas"])
		}
	}
}

// Seen live: right after a successful sync the CNEInstance read OutOfSync,
// because FLO rewrites its spec without false bools and empty env values. The
// rendered CNEInstance must hold neither, anywhere.
func TestCNEInstanceRendersNoDefaultFalseOrEmpty(t *testing.T) {
	out := doRender(t, baseConfig(config.ModeConnected, config.SourceFAR), nil)
	var walk func(path string, v any)
	walk = func(path string, v any) {
		switch x := v.(type) {
		case map[string]any:
			for k, c := range x {
				walk(path+"."+k, c)
			}
		case []any:
			for i, c := range x {
				walk(fmt.Sprintf("%s[%d]", path, i), c)
			}
		case bool:
			if !x && !strings.HasSuffix(path, ".stopOnFail") && !strings.HasSuffix(path, ".runAfterSuccess") {
				t.Errorf("%s: false (FLO drops it; Argo CD then reports a diff)", path)
			}
		case string:
			if x == "" {
				t.Errorf("%s: empty string", path)
			}
		}
	}
	walk("spec", find(out.Git, "CNEInstance", CNEInstanceName)["spec"])
}

// OpenShift adds <sa>-dockercfg-<suffix> to every ServiceAccount's
// imagePullSecrets. The Application must ignore exactly those entries — never
// the mirror pull secret it renders. Runs the expression through jq, as Argo
// CD does (del(<expr>)).
func TestApplicationIgnoresOnlyOpenShiftPullSecrets(t *testing.T) {
	jq, err := exec.LookPath("jq")
	if err != nil {
		t.Skip("jq not installed")
	}
	app := doRender(t, baseConfig(config.ModeConnected, config.SourceMirror), nil).Application
	var expr string
	for _, d := range app["spec"].(map[string]any)["ignoreDifferences"].([]any) {
		d := d.(map[string]any)
		if d["kind"] == "ServiceAccount" && d["group"] == "" {
			expr = d["jqPathExpressions"].([]any)[0].(string)
		}
	}
	if expr == "" {
		t.Fatal("no ServiceAccount ignoreDifferences")
	}
	live := `{"imagePullSecrets":[{"name":"mirror-secret"},{"name":"cert-manager-dockercfg-4jtg6"}]}`
	cmd := exec.Command(jq, "-c", "del("+expr+")")
	cmd.Stdin = strings.NewReader(live)
	got, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"imagePullSecrets":[{"name":"mirror-secret"}]}`; strings.TrimSpace(string(got)) != want {
		t.Fatalf("after ignoring: %s, want %s", got, want)
	}
}
