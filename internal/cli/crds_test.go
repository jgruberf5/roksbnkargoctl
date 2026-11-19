package cli

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

func crd(group, plural, channel string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apiextensions.k8s.io/v1", "kind": "CustomResourceDefinition",
		"metadata": map[string]any{"name": plural + "." + group},
		"spec": map[string]any{
			"group": group, "names": map[string]any{"plural": plural},
			"versions": []any{
				map[string]any{"name": "v1alpha1", "served": true, "storage": false},
				map[string]any{"name": "v1", "served": true, "storage": true},
			},
		},
	}}
	if channel != "" {
		u.SetAnnotations(map[string]string{"gateway.networking.k8s.io/channel": channel})
	}
	return u
}

func obj(apiVersion, kind, ns, name string, labels map[string]string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{"apiVersion": apiVersion, "kind": kind,
		"metadata": map[string]any{"name": name}}}
	if ns != "" {
		u.SetNamespace(ns)
	}
	if labels != nil {
		u.SetLabels(labels)
	}
	return u
}

// The kinds the fake has to be able to list.
var crdTestLists = map[schema.GroupVersionResource]string{
	gvrCRD:         "CustomResourceDefinitionList",
	gvrDeployments: "DeploymentList",
	gvrCNEInstance: "CNEInstanceList",
	{Group: "k8s.f5.com", Version: "v1", Resource: "f5tmms"}:                     "F5TmmList",
	{Group: "k8s.f5.com", Version: "v1", Resource: "cnemanifests"}:               "CNEManifestList",
	{Group: "gateway.k8s.f5.com", Version: "v1", Resource: "gatewaysettings"}:    "GatewaySettingsList",
	{Group: "k8s.f5net.com", Version: "v1", Resource: "licenses"}:                "LicenseList",
	{Group: gatewayAPIGroup, Version: "v1", Resource: "tcproutes"}:               "TCPRouteList",
	{Group: gatewayAPIGroup, Version: "v1", Resource: "gateways"}:                "GatewayList",
	{Group: "cert-manager.io", Version: "v1", Resource: "certificates"}:          "CertificateList",
	{Group: "monitoring.coreos.com", Version: "v1", Resource: "servicemonitors"}: "ServiceMonitorList",
}

func fakeCluster(objs ...runtime.Object) *dynamicfake.FakeDynamicClient {
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), crdTestLists, objs...)
}

func crdNames(t *testing.T, dyn *dynamicfake.FakeDynamicClient) map[string]bool {
	t.Helper()
	l, err := dyn.Resource(gvrCRD).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	for _, c := range l.Items {
		out[c.GetName()] = true
	}
	return out
}

// After a clean uninstall: BNK's CRDs with nothing left go; one with an object
// left stays and says why; OpenShift's standard Gateway API CRDs and unrelated
// CRDs are never touched; cert-manager's only go when the workspace installed it.
func TestRemoveCRDsRemovesOnlyBNKsEmptyCRDs(t *testing.T) {
	for _, certManagerOurs := range []bool{true, false} {
		dyn := fakeCluster(
			crd("k8s.f5.com", "f5tmms", ""),
			crd("k8s.f5.com", "cnemanifests", ""),
			crd("gateway.k8s.f5.com", "gatewaysettings", ""),
			crd("k8s.f5net.com", "licenses", ""),
			crd(gatewayAPIGroup, "tcproutes", "experimental"),
			crd(gatewayAPIGroup, "gateways", "standard"),
			crd("cert-manager.io", "certificates", ""),
			crd("monitoring.coreos.com", "servicemonitors", ""),
			// an object left of one F5 kind: its CRD must stay
			obj("k8s.f5.com/v1", "CNEManifest", "", "bnk-2.4.2", nil),
		)
		res, err := removeCRDs(context.Background(), dyn, certManagerOurs, time.Second)
		if err != nil || res.Skipped != "" {
			t.Fatalf("certManagerOurs=%v: %v / skipped %q", certManagerOurs, err, res.Skipped)
		}
		left := crdNames(t, dyn)
		for _, gone := range []string{"f5tmms.k8s.f5.com", "gatewaysettings.gateway.k8s.f5.com", "licenses.k8s.f5net.com", "tcproutes." + gatewayAPIGroup} {
			if left[gone] {
				t.Errorf("certManagerOurs=%v: %s should have been removed", certManagerOurs, gone)
			}
		}
		for _, stays := range []string{"cnemanifests.k8s.f5.com", "gateways." + gatewayAPIGroup, "servicemonitors.monitoring.coreos.com"} {
			if !left[stays] {
				t.Errorf("certManagerOurs=%v: %s should have stayed", certManagerOurs, stays)
			}
		}
		if left["certificates.cert-manager.io"] == certManagerOurs {
			t.Errorf("certManagerOurs=%v: cert-manager's CRD present=%v", certManagerOurs, left["certificates.cert-manager.io"])
		}
		if k := strings.Join(res.Kept, "\n"); !strings.Contains(k, "cnemanifests.k8s.f5.com: F5 objects remain (bnk-2.4.2)") {
			t.Errorf("kept should say why the CNEManifest CRD stayed:\n%s", k)
		}
	}
}

// While BNK is anywhere on the cluster, nothing is removed: deleting a CRD would
// delete its objects, and an operator's finalizers would hang them.
func TestRemoveCRDsRemovesNothingWhileBNKRemains(t *testing.T) {
	for name, extra := range map[string]*unstructured.Unstructured{
		"FLO running":      obj("apps/v1", "Deployment", "other-bnk", "flo-f5-lifecycle-operator", map[string]string{"app.kubernetes.io/name": "f5-lifecycle-operator"}),
		"CNEInstance left": obj("k8s.f5.com/v1", "CNEInstance", "other-bnk", "other-bnk-f5-cne-controller", nil),
	} {
		dyn := fakeCluster(crd("k8s.f5.com", "f5tmms", ""), crd(gatewayAPIGroup, "tcproutes", "experimental"), extra)
		res, err := removeCRDs(context.Background(), dyn, true, time.Second)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if res.Skipped == "" || !strings.Contains(res.Skipped, "other-bnk") {
			t.Errorf("%s: want skipped naming other-bnk, got %+v", name, res)
		}
		if left := crdNames(t, dyn); !left["f5tmms.k8s.f5.com"] || !left["tcproutes."+gatewayAPIGroup] {
			t.Errorf("%s: CRDs removed while BNK remains: %v", name, left)
		}
	}
}

// A Deployment that is not FLO does not block it.
func TestRemoveCRDsIgnoresOtherDeployments(t *testing.T) {
	dyn := fakeCluster(crd("k8s.f5.com", "f5tmms", ""),
		obj("apps/v1", "Deployment", "app", "web", map[string]string{"app.kubernetes.io/name": "web"}))
	res, err := removeCRDs(context.Background(), dyn, true, time.Second)
	if err != nil || res.Skipped != "" || len(res.Removed) != 1 {
		t.Fatalf("got %+v, %v", res, err)
	}
}

func TestCRDKind(t *testing.T) {
	for _, tc := range []struct {
		group, channel string
		ours           bool
		want           string
	}{
		{"k8s.f5.com", "", false, "F5"},
		{"gateway.k8s.f5.com", "", false, "F5"},
		{"fic.f5.com", "", false, "F5"},
		{"k8s.f5net.com", "", false, "F5"},
		{"notf5.com", "", false, ""},
		{"evil-f5.com.example.org", "", false, ""},
		{gatewayAPIGroup, "experimental", false, "Gateway API (experimental)"},
		{gatewayAPIGroup, "standard", false, ""},
		{gatewayAPIGroup, "", false, ""},
		{"cert-manager.io", "", true, "cert-manager"},
		{"acme.cert-manager.io", "", true, "cert-manager"},
		{"cert-manager.io", "", false, ""},
		{"monitoring.coreos.com", "", true, ""},
	} {
		if got := crdKind(tc.group, tc.channel, tc.ours); got != tc.want {
			t.Errorf("crdKind(%q, %q, %v) = %q, want %q", tc.group, tc.channel, tc.ours, got, tc.want)
		}
	}
}

// Removal is the default: --keep-crds is off unless asked for.
func TestUninstallRemovesCRDsByDefault(t *testing.T) {
	f := newUninstallCmd().Flags().Lookup("keep-crds")
	if f == nil || f.DefValue != "false" {
		t.Fatalf("--keep-crds: %+v, want a flag defaulting to false", f)
	}
}

// runUninstall needs a cluster, so its order is pinned from the source: the CRDs
// are removed only after the uninstall steps (which fail while a BNK namespace
// remains), only unless --keep-crds, and with BNK's own cert-manager decision.
func TestUninstallRemovesCRDsOnlyAfterBNKIsGone(t *testing.T) {
	b, err := os.ReadFile("install.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	start := strings.Index(src, "func runUninstall(")
	body := src[start:]
	body = body[:strings.Index(body, "\nfunc ")]
	steps := strings.Index(body, "collectCheckLogsDuring(ctx, logs, uninstall)")
	call := strings.Index(body, "removeCRDs(ctx, k.Dynamic, c.CertManagerInstall(),")
	guard := strings.Index(body, "if o.keepCRDs {")
	switch {
	case steps < 0 || call < 0 || guard < 0:
		t.Fatalf("runUninstall changed shape (steps %d, removeCRDs %d, keepCRDs guard %d): update this test", steps, call, guard)
	case call < steps:
		t.Error("runUninstall removes CRDs before the uninstall steps confirm BNK is gone")
	case !(guard < call):
		t.Error("removeCRDs is not under the --keep-crds guard")
	}
}
