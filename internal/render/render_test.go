package render

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"os/exec"
	"slices"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/jgruberf5/roksbnkargoctl/internal/config"
	"github.com/jgruberf5/roksbnkargoctl/internal/far"
)

// fakeChart builds a tiny chart that, like FLO, renders a CRD, a Deployment and a
// Secret (FLO renders external-otelsvr-secret), with no namespace on the
// Deployment (Helm leaves namespacing to the installer).
func fakeChart(t *testing.T, name string) []byte { return fakeChartNS(t, name, "") }

// fakeChartNS is fakeChart with every namespaced object naming nsTmpl as its
// namespace, as the real cert-manager chart does ({{ .Release.Namespace }}).
func fakeChartNS(t *testing.T, name, nsTmpl string) []byte {
	t.Helper()
	nsLine := ""
	if nsTmpl != "" {
		nsLine = "\n  namespace: " + nsTmpl
	}
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
  name: {{ .Release.Name }}-op` + nsLine + `
spec:
  selector: {matchLabels: {app: x}}
  template:
    metadata: {labels: {app: x}}
    spec: {containers: [{name: c, image: "{{ .Values.image.repository }}/x:1"}]}
---
apiVersion: v1
kind: Secret
metadata:
  name: chart-shipped-secret` + nsLine + `
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
		CertManagerChart: fakeChartNS(t, "cm", "{{ .Release.Namespace }}"), KubeVersion: "v1.34.0",
		FLOChartRef: "repo.f5.com/charts/f5-lifecycle-operator:v2.30.0-0.5.2", CertManagerChartRef: "quay.io/jetstack/charts/cert-manager:v1.17.3", CheckImage: "ghcr.io/x/check:1", RunID: "r1",
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

// DESIGN.md: no secret value is ever published. Every Secret of ours is Direct;
// a Secret a chart ships stays chart content (Argo CD renders it from the
// chart), so it is neither in Git nor applied directly as well. No secret VALUE
// may appear in Git, values files included.
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
	files, err := out.ValuesFiles()
	if err != nil {
		t.Fatal(err)
	}
	for name, b := range files {
		for _, v := range []string{"FARKEYVALUE-123456", "hdr.payloadJWT.sig"} {
			if strings.Contains(string(b), v) {
				t.Fatalf("%s leaked into %s", v, name)
			}
		}
	}
	if find(out.Direct, "Secret", "chart-shipped-secret") != nil {
		t.Fatal("a chart-shipped Secret is applied by Argo CD from the chart; applying it directly too would fight it")
	}
	for _, ch := range out.Charts {
		if find(ch.Objects, "Secret", "chart-shipped-secret") == nil {
			t.Errorf("chart %s: its Secret is missing from what Argo CD will render", ch.Name)
		}
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
	in := Inputs{Config: c, Workspace: "ws", Manifest: manifest(), FLOChart: fakeChart(t, "flo"),
		CertManagerChart: fakeChartNS(t, "cm", "{{ .Release.Namespace }}"),
		FLOChartRef:      "r/charts/f5-lifecycle-operator:1", CertManagerChartRef: "q/charts/cert-manager:1",
		CheckImage: "i", RunID: "r", Secrets: Secrets{PullHost: "h", PullUsername: "u", PullPassword: "FARKEYVALUE-123456", JWT: "a.b.c"}}
	if _, err := Render(in); err == nil || !strings.Contains(err.Error(), "refusing to publish") {
		t.Fatalf("expected the no-secrets-in-Git guard to refuse, got %v", err)
	}
	// A value that reaches a chart's values file is caught there: the FLO
	// values carry bnk.namespace.
	c = baseConfig(config.ModeConnected, config.SourceFAR)
	c.BNK.Namespace = "farkeyvalue-123456"
	in.Config, in.Secrets.PullPassword = c, "farkeyvalue-123456"
	if _, err := Render(in); err == nil || !strings.Contains(err.Error(), "appears in values/flo.yaml") {
		t.Fatalf("expected the values-file guard to refuse, got %v", err)
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
		WaveCharts, // cert-manager and FLO, from their Helm sources
		wave("Job", "check-cert-manager-ready"),
		wave("ClusterIssuer", ClusterIssuerSelfSigned),
		wave("Certificate", CACertName),
		wave("ClusterIssuer", ClusterIssuerCA),
		wave("CNEManifest", CNEManifestName(config.BNKVersion)),
		wave("CNEInstance", CNEInstanceName("f5-bnk")),
		wave("Job", "check-license"),
	}
	for i := 1; i < len(order); i++ {
		if order[i] <= order[i-1] {
			t.Fatalf("waves out of order at step %d: %v", i, order)
		}
	}
	if w := wave("Deployment", SweepDeployment); w >= WaveCharts {
		t.Fatal("the Gateway API sweep must be running before FLO's CRD installer")
	}
	if w := wave("ClusterRoleBinding", "system:openshift:scc:privileged:f5-bnk:flo-f5-lifecycle-operator"); w >= WaveCharts {
		t.Fatal("FLO's SCC binding must exist before FLO's pods")
	}
	// Chart objects carry no wave: Argo CD renders them from the chart.
	for _, ch := range out.Charts {
		for _, o := range ch.Objects {
			if o.Annotation(AnnoWave) != "" {
				t.Errorf("chart %s: %s %s has a wave; Argo CD's render would not", ch.Name, o.Kind(), o.Name())
			}
		}
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
	var flo Object
	for _, ch := range out.Charts {
		if ch.Name == "flo" {
			flo = find(ch.Objects, "Deployment", "flo-op")
		}
	}
	b, _ := flo.YAML()
	if !strings.Contains(string(b), "harbor.x:8443/bnk-mirror/images/x:1") {
		t.Fatalf("FLO image not redirected to the mirror:\n%s", b)
	}
	cne := find(out.Git, "CNEInstance", CNEInstanceName("f5-bnk"))
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
		spec := find(out.Git, "CNEInstance", CNEInstanceName("f5-bnk"))["spec"].(map[string]any)
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
	walk("spec", find(out.Git, "CNEInstance", CNEInstanceName("f5-bnk"))["spec"])
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

// Git holds none of the charts' objects, only their values files; the
// Application installs each chart as a Helm source rendered exactly as here.
func TestChartsAreHelmSourcesNotGitObjects(t *testing.T) {
	c := baseConfig(config.ModeConnected, config.SourceFAR)
	c.Git.Path = "/bnk/c1/"
	out := doRender(t, c, nil)
	for _, o := range out.Git {
		if o.Kind() == "CustomResourceDefinition" || o.Name() == "flo-op" || o.Name() == "cm-op" {
			t.Errorf("chart object %s %s is in Git", o.Kind(), o.Name())
		}
	}
	files, err := out.ValuesFiles()
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 || files["values/flo.yaml"] == nil || files["values/cert-manager.yaml"] == nil {
		t.Fatalf("values files %v", keys(files))
	}
	var flo map[string]any
	if err := yaml.Unmarshal(files["values/flo.yaml"], &flo); err != nil {
		t.Fatal(err)
	}
	if flo["namespace"] != "f5-bnk" || flo["skipCertMgr"] != true || flo["crds"].(map[string]any)["keep"] != true {
		t.Errorf("values/flo.yaml: %v", flo)
	}
	// Both charts' CRDs must carry helm.sh/resource-policy: keep, which Argo CD
	// honours on delete: uninstall leaves them (and every CR with them) alone.
	var cm map[string]any
	if err := yaml.Unmarshal(files["values/cert-manager.yaml"], &cm); err != nil {
		t.Fatal(err)
	}
	if crds, _ := cm["crds"].(map[string]any); crds["enabled"] != true || crds["keep"] != true {
		t.Errorf("values/cert-manager.yaml crds: %v", cm["crds"])
	}

	spec := out.Application["spec"].(map[string]any)
	if _, single := spec["source"]; single {
		t.Fatal("the Application still has a single source")
	}
	srcs := spec["sources"].([]any)
	if len(srcs) != 3 {
		t.Fatalf("%d sources, want cert-manager, FLO and Git", len(srcs))
	}
	want := []struct{ repo, chart, version, release, ns, values string }{
		{"quay.io/jetstack/charts", "cert-manager", "v1.17.3", "cert-manager", "cert-manager", "$values/bnk/c1/values/cert-manager.yaml"},
		{"repo.f5.com/charts", "f5-lifecycle-operator", "v2.30.0-0.5.2", "flo", "f5-bnk", "$values/bnk/c1/values/flo.yaml"},
	}
	for i, w := range want {
		s := srcs[i].(map[string]any)
		h := s["helm"].(map[string]any)
		if s["repoURL"] != w.repo || s["chart"] != w.chart || s["targetRevision"] != w.version ||
			h["releaseName"] != w.release || h["namespace"] != w.ns || h["valueFiles"].([]any)[0] != w.values || h["kubeVersion"] != "v1.34.0" {
			t.Errorf("source %d = %v, want %+v", i, s, w)
		}
		if fmt.Sprint(h["apiVersions"]) != fmt.Sprint(toAnySlice(OpenShiftAPIVersions)) {
			t.Errorf("source %d apiVersions %v: Argo CD must render with the API versions render uses", i, h["apiVersions"])
		}
	}
	g := srcs[2].(map[string]any)
	if g["repoURL"] != c.Git.URL || g["path"] != c.Git.Path || g["ref"] != "values" || g["directory"].(map[string]any)["recurse"] != false {
		t.Errorf("Git source %v: must be the objects (no recursion into values/) and the $values ref", g)
	}

	// Without cert-manager installed by us, only FLO and Git.
	c.BNK.CertManager.Install = new(bool)
	out = doRender(t, c, nil)
	if n := len(out.Application["spec"].(map[string]any)["sources"].([]any)); n != 2 || len(out.Charts) != 1 {
		t.Errorf("cert_manager.install false: %d sources, %d charts", n, len(out.Charts))
	}
}

// A chart released into a namespace other than the Application's must name it
// on every namespaced object: Argo CD would put one that names none in the
// destination namespace instead.
func TestChartWithoutNamespacesIsRefusedOutsideTheDestination(t *testing.T) {
	c := baseConfig(config.ModeConnected, config.SourceFAR)
	in := Inputs{Config: c, Workspace: "ws", Manifest: manifest(), FLOChart: fakeChart(t, "flo"), CertManagerChart: fakeChart(t, "cm"),
		FLOChartRef: "r/charts/f5-lifecycle-operator:1", CertManagerChartRef: "q/charts/cert-manager:1", CheckImage: "i", RunID: "r"}
	if _, err := Render(in); err == nil || !strings.Contains(err.Error(), "Argo CD would put it in f5-bnk, not cert-manager") {
		t.Fatalf("got %v", err)
	}
}

func keys(m map[string][]byte) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

// Single-certificate mode (F5's procedure): no cert-manager at all — no chart,
// namespace, issuers or readiness gate — the cert Job before the charts, FLO
// told to mount the one Secret, the CNEInstance without spec.certificate, and
// the check allowed to create Secrets only in this mode.
func TestSingleCertificateMode(t *testing.T) {
	c := baseConfig(config.ModeConnected, config.SourceFAR)
	c.BNK.Certificates.Mode = config.CertModeSingle
	c.Defaults("ws")
	out := doRender(t, c, nil)
	for _, ch := range out.Charts {
		if ch.Name == "cert-manager" {
			t.Fatal("the cert-manager chart is installed in single-certificate mode")
		}
	}
	for _, o := range out.Git {
		switch {
		case o.Kind() == "Namespace" && o.Name() == "cert-manager",
			o.Kind() == "ClusterIssuer", o.Kind() == "Certificate",
			o.Kind() == "Job" && o.Name() == "check-cert-manager-ready":
			t.Errorf("%s %s rendered in single-certificate mode", o.Kind(), o.Name())
		}
	}
	job := find(out.Git, "Job", "check-cert")
	if job == nil {
		t.Fatal("no cert Job")
	}
	if w := job.Wave(); w >= WaveCharts || w <= WavePreInstall {
		t.Errorf("cert Job in wave %d: it must run after the pre-install check and before FLO (wave %d)", w, WaveCharts)
	}
	args := strings.Join(jobArgs(job), " ")
	for _, want := range []string{"cert", "--issuer=self-signed", "--secret-name=bnk-single-cert", "--namespace=f5-bnk,f5-utils",
		"--common-name=f5net", "--ca-common-name=f5net-ca", "--key-type=rsa", "--key-bits=4096", "--validity=87600h"} {
		if !strings.Contains(args, want) {
			t.Errorf("cert Job args lack %q: %s", want, args)
		}
	}
	var flo map[string]any
	for _, ch := range out.Charts {
		if ch.Name == "flo" {
			flo = ch.Values
		}
	}
	cm := flo["global"].(map[string]any)["certmgr"].(map[string]any)
	if cm["enabled"] != false || cm["secretName"] != "bnk-single-cert" {
		t.Errorf("FLO certmgr values %v", cm)
	}
	cne := find(out.Git, "CNEInstance", CNEInstanceName("f5-bnk"))
	if _, has := cne["spec"].(map[string]any)["certificate"]; has {
		t.Error("CNEInstance keeps spec.certificate in single-certificate mode")
	}
	if !secretCreateGranted(out.Direct) {
		t.Error("the check cannot create the Secret: no create on secrets")
	}
	if post := find(out.Git, "Job", "check-post-uninstall"); strings.Contains(strings.Join(jobArgs(post), " "), "cert-manager") {
		t.Error("post-uninstall deletes a cert-manager namespace that was never created")
	}

	// cert-manager mode is unchanged: issuers, gate, spec.certificate, no create.
	d := doRender(t, baseConfig(config.ModeConnected, config.SourceFAR), nil)
	if find(d.Git, "Job", "check-cert") != nil || find(d.Git, "Job", "check-cert-manager-ready") == nil || find(d.Git, "ClusterIssuer", ClusterIssuerCA) == nil {
		t.Error("cert-manager mode lost its issuers or gained the cert Job")
	}
	if _, has := find(d.Git, "CNEInstance", CNEInstanceName("f5-bnk"))["spec"].(map[string]any)["certificate"]; !has {
		t.Error("cert-manager mode lost spec.certificate")
	}
	if secretCreateGranted(d.Direct) {
		t.Error("cert-manager mode grants the check create on Secrets")
	}
}

// issuer ca / provided: the operator's material goes into the check namespace
// as a direct Secret the Job reads, never into Git or a values file.
func TestSingleCertificateSourceStaysOutOfGit(t *testing.T) {
	for _, issuer := range []string{"ca", "provided"} {
		c := baseConfig(config.ModeConnected, config.SourceFAR)
		c.BNK.Certificates = config.Certificates{Mode: config.CertModeSingle, Issuer: issuer}
		c.Defaults("ws")
		key := "-----BEGIN EC PRIVATE KEY-----\nSECRETKEYMATERIAL-" + issuer + "\n-----END EC PRIVATE KEY-----\n"
		out := doRender(t, c, func(in *Inputs) {
			in.Secrets.SingleCertPEM, in.Secrets.SingleCertKey = "-----BEGIN CERTIFICATE-----\nC\n-----END CERTIFICATE-----\n", key
			if issuer == "provided" {
				in.Secrets.SingleCertCAPEM = "-----BEGIN CERTIFICATE-----\nCA\n-----END CERTIFICATE-----\n"
			}
		})
		src := find(out.Direct, "Secret", SingleCertSourceSecret)
		if src == nil || src.Namespace() != CheckNamespace {
			t.Fatalf("%s: no source Secret in the check namespace", issuer)
		}
		if find(out.Git, "Secret", SingleCertSourceSecret) != nil {
			t.Fatalf("%s: the source Secret is in Git", issuer)
		}
		if !strings.Contains(strings.Join(jobArgs(find(out.Git, "Job", "check-cert")), " "), "--source-secret="+CheckNamespace+"/"+SingleCertSourceSecret) {
			t.Errorf("%s: the Job does not read the source Secret", issuer)
		}
		if !strings.Contains(strings.Join(jobArgs(find(out.Git, "Job", "check-pre-install")), " "), CheckNamespace+"/"+SingleCertSourceSecret) {
			t.Errorf("%s: the pre-install check does not require the source Secret", issuer)
		}
	}
	// The guard fires if the key reaches Git.
	c := baseConfig(config.ModeConnected, config.SourceFAR)
	c.BNK.Certificates = config.Certificates{Mode: config.CertModeSingle, Issuer: "ca"}
	c.Defaults("ws")
	c.BNK.NADAddress = "LEAKEDKEYMATERIAL-123"
	in := Inputs{Config: c, Workspace: "ws", Manifest: manifest(), FLOChart: fakeChart(t, "flo"),
		FLOChartRef: "r/charts/f5-lifecycle-operator:1", CheckImage: "i", RunID: "r",
		Secrets: Secrets{SingleCertPEM: "x", SingleCertKey: "LEAKEDKEYMATERIAL-123"}}
	if _, err := Render(in); err == nil || !strings.Contains(err.Error(), "single-certificate private key") {
		t.Errorf("a private key in Git was not refused: %v", err)
	}
}

// One namespace (bnk.utils_namespace = bnk.namespace): every object lands in
// it, no object is rendered twice, and nothing names a second namespace.
func TestSingleNamespaceRendersNoCollisions(t *testing.T) {
	for _, src := range []string{config.SourceFAR, config.SourceMirror} {
		for _, mode := range []string{config.ModeConnected, config.ModeDisconnected} {
			for _, certs := range []string{config.CertModeCertManager, config.CertModeSingle} {
				c := baseConfig(mode, src)
				c.BNK.UtilsNamespace = c.BNK.Namespace
				c.BNK.Certificates.Mode = certs
				c.Defaults("ws")
				out := doRender(t, c, nil)
				where := mode + "/" + src + "/" + certs
				for _, set := range [][]Object{out.Git, out.Direct} {
					seen := map[string]bool{}
					for _, o := range set {
						k := o.Kind() + " " + o.Namespace() + "/" + o.Name()
						if seen[k] {
							t.Errorf("%s: %s rendered twice", where, k)
						}
						seen[k] = true
					}
				}
				all := append(append([]Object{}, out.Git...), out.Direct...)
				for _, o := range all {
					b, _ := o.YAML()
					if strings.Contains(string(b), "f5-utils") {
						t.Errorf("%s: %s %s still names f5-utils", where, o.Kind(), o.Name())
					}
				}
				if certs == config.CertModeSingle {
					if args := jobArgs(find(out.Git, "Job", "check-cert")); !slices.Contains(args, "--namespace=f5-bnk") {
						t.Errorf("%s: the cert Job must write into the one namespace only: %v", where, args)
					}
				}
			}
		}
	}
}

func secretCreateGranted(direct []Object) bool {
	role := find(direct, "ClusterRole", CheckClusterRole)
	for _, r := range role["rules"].([]any) {
		m := r.(map[string]any)
		if fmt.Sprint(m["resources"]) == "[secrets]" && fmt.Sprint(m["verbs"]) == "[create]" {
			return true
		}
	}
	return false
}
