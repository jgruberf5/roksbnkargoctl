package checks

import (
	"context"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jgruberf5/roksbnkargoctl/cmd/check/internal/kube/kubefake"
)

func uninstallCfg() PreUninstallConfig {
	return PreUninstallConfig{
		BNKNamespace: "f5-bnk", UtilsNamespace: "f5-utils",
		DrainTimeout: 2 * time.Second, Poll: 10 * time.Millisecond, RefusalGrace: 200 * time.Millisecond,
		WebhookInterval: 20 * time.Millisecond, LicenseTimeout: time.Second, CNETimeout: 2 * time.Second,
	}
}

// installedBNK seeds a 2.4 install: the CNEInstance (with its finalizer), a
// FLO-managed component, gateway CRs, controller-generated IPAM owned by the
// Infra, the License, an F5 product default in k8s.f5net.com, and FLO's webhook.
func installedBNK(s *kubefake.Server) {
	for _, r := range []struct{ g, res, kind string }{
		{"k8s.f5.com", "cneinstances", "CNEInstance"},
		{"k8s.f5.com", "f5tmms", "F5Tmm"},
		{"k8s.f5.com", "dssms", "Dssm"},
		{"gateway.k8s.f5.com", "gatewaysettings", "GatewaySettings"},
		{"gateway.k8s.f5.com", "infras", "Infra"},
		{"fic.f5.com", "ipams", "IPAM"},
		{"k8s.f5net.com", "licenses", "License"},
		{"k8s.f5net.com", "f5-big-tcp-settings", "F5BigTcpSetting"},
	} {
		s.AddResource(r.g, "v1", r.res, r.kind, true)
	}
	flo := floDeployment()
	with(flo, []string{"status", "availableReplicas"}, float64(1))
	s.Put("apps", "v1", "deployments", flo)
	cne := obj("k8s.f5.com/v1", "CNEInstance", "f5-bnk", "f5-bnk-f5-cne-controller")
	with(cne, []string{"metadata", "finalizers"}, []any{"k8s.f5.com/CNEInstanceFinalizer"})
	s.Put("k8s.f5.com", "v1", "cneinstances", cne)
	tmm := obj("k8s.f5.com/v1", "F5Tmm", "f5-bnk", "f5-tmm")
	with(tmm, []string{"metadata", "ownerReferences"}, []any{map[string]any{"kind": "CNEInstance", "name": "f5-bnk-f5-cne-controller"}})
	s.Put("k8s.f5.com", "v1", "f5tmms", tmm)
	// dssms carry NO ownerReference, yet FLO re-creates them just the same: only
	// the group rule keeps the drain off them (roksbnkctl floManagedComponent).
	s.Put("k8s.f5.com", "v1", "dssms", obj("k8s.f5.com/v1", "Dssm", "f5-bnk", "f5-dssm"))
	gs := obj("gateway.k8s.f5.com/v1", "GatewaySettings", "f5-bnk", "gs")
	with(gs, []string{"metadata", "finalizers"}, []any{"gateway.k8s.f5.com/cleanup"})
	s.Put("gateway.k8s.f5.com", "v1", "gatewaysettings", gs)
	s.Put("gateway.k8s.f5.com", "v1", "infras", obj("gateway.k8s.f5.com/v1", "Infra", "f5-bnk", "infra"))
	ipam := obj("fic.f5.com/v1", "IPAM", "f5-bnk", "ipam-infra")
	with(ipam, []string{"metadata", "ownerReferences"}, []any{map[string]any{"kind": "Infra", "name": "infra"}})
	s.Put("fic.f5.com", "v1", "ipams", ipam)
	lic := obj("k8s.f5net.com/v1", "License", "f5-utils", "bnk-license")
	with(lic, []string{"metadata", "finalizers"}, []any{"k8s.f5net.com/uninstall"})
	s.Put("k8s.f5net.com", "v1", "licenses", lic)
	s.Put("k8s.f5net.com", "v1", "f5-big-tcp-settings", obj("k8s.f5net.com/v1", "F5BigTcpSetting", "f5-bnk", "sys-default-tcp"))
	vwc := obj("admissionregistration.k8s.io/v1", "ValidatingWebhookConfiguration", "", "f5validate-f5-bnk")
	s.Put("admissionregistration.k8s.io", "v1", "validatingwebhookconfigurations", vwc)
	s.Put("admissionregistration.k8s.io", "v1", "validatingwebhookconfigurations", obj("admissionregistration.k8s.io/v1", "ValidatingWebhookConfiguration", "", "someone-elses-webhook"))
}

// operator plays FLO/CWC: finalizers clear a moment after delete, and the IPAM
// goes when its Infra does. The first GatewaySettings delete is refused by the
// dead webhook, which only a neutralise-and-retry gets past.
func operator(s *kubefake.Server) {
	var refused atomic.Bool
	s.Hook = func(s *kubefake.Server, r kubefake.Request) *kubefake.Reply {
		if r.Method == "DELETE" && strings.HasSuffix(r.Path, "/gatewaysettings/gs") && !refused.Load() {
			refused.Store(true)
			rep := kubefake.Status(500, "InternalError", `Internal error occurred: failed calling webhook "f5validate.f5net.com": no endpoints available for service "f5-validation"`)
			return &rep
		}
		return nil
	}
	s.AfterDelete = func(s *kubefake.Server, r kubefake.Request) {
		switch {
		case strings.HasSuffix(r.Path, "/infras/infra"):
			s.Remove("fic.f5.com", "v1", "ipams", "f5-bnk", "ipam-infra")
		case strings.HasSuffix(r.Path, "/gatewaysettings/gs"):
			go func() {
				time.Sleep(30 * time.Millisecond)
				s.Remove("gateway.k8s.f5.com", "v1", "gatewaysettings", "f5-bnk", "gs")
			}()
		case strings.HasSuffix(r.Path, "/licenses/bnk-license"):
			go func() {
				time.Sleep(30 * time.Millisecond)
				s.Remove("k8s.f5net.com", "v1", "licenses", "f5-utils", "bnk-license")
			}()
		case strings.HasSuffix(r.Path, "/cneinstances/f5-bnk-f5-cne-controller"):
			go func() {
				time.Sleep(30 * time.Millisecond)
				s.Remove("k8s.f5.com", "v1", "cneinstances", "f5-bnk", "f5-bnk-f5-cne-controller")
				s.Remove("k8s.f5.com", "v1", "f5tmms", "f5-bnk", "f5-tmm")
			}()
		}
	}
}

func crDeletes(reqs []kubefake.Request) []string {
	var out []string
	for _, r := range reqs {
		if r.Method != "DELETE" || !strings.Contains(r.Path, "f5") || strings.Contains(r.Path, "validatingwebhookconfigurations") {
			continue
		}
		out = append(out, r.Path[strings.LastIndex(r.Path[:strings.LastIndex(r.Path, "/")], "/")+1:])
	}
	return out
}

// The bug it names (roksbnkctl #217 and the 2.4 guide): the CNEInstance must be
// deleted LAST — after every drained CR and after the License — so FLO is still
// there to finalize everything it owns.
func TestPreUninstallDeletesCNEInstanceLast(t *testing.T) {
	s, env, res := newFake(t)
	installedBNK(s)
	operator(s)
	if err := PreUninstall(context.Background(), env, uninstallCfg(), res); err != nil {
		t.Fatal(err)
	}
	if res.Failed() {
		t.Fatalf("failed: %v", res.Failures())
	}
	got := crDeletes(s.Requests())
	if len(got) == 0 || got[len(got)-1] != "cneinstances/f5-bnk-f5-cne-controller" {
		t.Fatalf("CNEInstance not deleted last: %v", got)
	}
	idx := map[string]int{}
	for i, d := range got {
		if _, seen := idx[d]; !seen {
			idx[d] = i
		}
	}
	for _, before := range []string{"gatewaysettings/gs", "infras/infra", "licenses/bnk-license"} {
		i, ok := idx[before]
		if !ok || i > idx["cneinstances/f5-bnk-f5-cne-controller"] {
			t.Errorf("%s not deleted before the CNEInstance: %v", before, got)
		}
	}
	if idx["licenses/bnk-license"] < idx["infras/infra"] {
		t.Errorf("License deleted before the gateway CRs: %v", got)
	}
	// Never touched: FLO-managed components, product defaults, owned children.
	for _, never := range []string{"f5tmms/f5-tmm", "dssms/f5-dssm", "f5-big-tcp-settings/sys-default-tcp", "ipams/ipam-infra"} {
		if _, ok := idx[never]; ok {
			t.Errorf("deleted %s, which it must leave alone: %v", never, got)
		}
	}
	// The refused delete got past the dead webhook by removing it.
	if len(deletes(s.Requests(), "validatingwebhookconfigurations/f5validate-f5-bnk")) == 0 {
		t.Error("f5validate webhook never removed")
	}
	if len(deletes(s.Requests(), "someone-elses-webhook")) != 0 {
		t.Error("deleted a webhook that is not F5's")
	}
	if s.Get("k8s.f5.com", "v1", "cneinstances", "f5-bnk", "f5-bnk-f5-cne-controller") != nil {
		t.Error("CNEInstance still present")
	}
}

// If the leaves cannot drain, the CNEInstance must NOT be deleted: it would
// orphan exactly the objects still waiting.
func TestPreUninstallKeepsCNEInstanceWhenLeavesStick(t *testing.T) {
	s, env, res := newFake(t)
	installedBNK(s)
	s.Hook = func(s *kubefake.Server, r kubefake.Request) *kubefake.Reply {
		if r.Method == "DELETE" && strings.HasSuffix(r.Path, "/infras/infra") {
			rep := kubefake.Status(500, "InternalError", "etcdserver: request timed out")
			return &rep
		}
		return nil
	}
	s.AfterDelete = func(s *kubefake.Server, r kubefake.Request) {
		if strings.HasSuffix(r.Path, "/gatewaysettings/gs") {
			s.Remove("gateway.k8s.f5.com", "v1", "gatewaysettings", "f5-bnk", "gs")
		}
	}
	_ = PreUninstall(context.Background(), env, uninstallCfg(), res)
	if !hasSev(res, "drain", SevFail) || !strings.Contains(detail(res, "drain"), "infras/f5-bnk/infra") {
		t.Fatalf("stuck leaf not reported: %+v", res.Findings)
	}
	for _, d := range crDeletes(s.Requests()) {
		if strings.HasPrefix(d, "cneinstances/") || strings.HasPrefix(d, "licenses/") {
			t.Fatalf("deleted %s although the drain did not finish", d)
		}
	}
}

// A deliberate webhook DENIAL is respected, not retried past or counted stuck.
func TestPreUninstallRespectsWebhookDenial(t *testing.T) {
	s, env, res := newFake(t)
	installedBNK(s)
	operator(s)
	inner := s.Hook
	s.Hook = func(s *kubefake.Server, r kubefake.Request) *kubefake.Reply {
		if r.Method == "DELETE" && strings.HasSuffix(r.Path, "/infras/infra") {
			rep := kubefake.Status(403, "Forbidden", `admission webhook "f5validate.f5net.com" denied the request: Default Infra cannot be deleted!`)
			return &rep
		}
		return inner(s, r)
	}
	// The IPAM would otherwise never go (its owner stays); drop it.
	s.Remove("fic.f5.com", "v1", "ipams", "f5-bnk", "ipam-infra")
	_ = PreUninstall(context.Background(), env, uninstallCfg(), res)
	if res.Failed() {
		t.Fatalf("a product-denied delete failed the drain: %v", res.Failures())
	}
	if n := len(deletes(s.Requests(), "/infras/infra")); n != 1 {
		t.Errorf("denied delete retried %d times", n)
	}
}

// --- post-uninstall ---------------------------------------------------------

func TestRetainNonF5Finalizers(t *testing.T) {
	keep, n := RetainNonF5Finalizers([]string{
		"k8s.f5.com/CNEInstanceFinalizer", "k8s.f5net.com/uninstall", "handletmmconfig_inconsistency",
		"gateway.k8s.f5.com/cleanup", "f5.com/x",
		"kubernetes.io/pvc-protection", "foregroundDeletion", "orphan", "notf5.com/x", "f5.com.evil.io/x", "example.com/f5.com",
	})
	want := []string{"kubernetes.io/pvc-protection", "foregroundDeletion", "orphan", "notf5.com/x", "f5.com.evil.io/x", "example.com/f5.com"}
	if n != 5 || !reflect.DeepEqual(keep, want) {
		t.Fatalf("removed %d, kept %v", n, keep)
	}
}

// The bug it names: stripping finalizers must remove ONLY F5's and write every
// other one back (roksbnkctl's first version wrote finalizers: null).
func TestPostUninstallStripsOnlyF5Finalizers(t *testing.T) {
	s, env, res := newFake(t)
	s.AddResource("k8s.f5.com", "v1", "f5tmms", "F5Tmm", true)
	s.AddResource("k8s.f5net.com", "v1", "f5-big-tcp-settings", "F5BigTcpSetting", true)
	for _, ns := range []string{"f5-bnk", "f5-utils"} {
		s.Put("", "v1", "namespaces", obj("v1", "Namespace", "", ns))
	}
	tmm := obj("k8s.f5.com/v1", "F5Tmm", "f5-bnk", "f5-tmm")
	with(tmm, []string{"metadata", "finalizers"}, []any{"handletmmconfig_inconsistency", "kubernetes.io/pvc-protection", "k8s.f5.com/CNEInstanceFinalizer"})
	s.Put("k8s.f5.com", "v1", "f5tmms", tmm)
	tcp := obj("k8s.f5net.com/v1", "F5BigTcpSetting", "f5-bnk", "sys-default-tcp")
	with(tcp, []string{"metadata", "finalizers"}, []any{"k8s.f5net.com/uninstall"})
	s.Put("k8s.f5net.com", "v1", "f5-big-tcp-settings", tcp)
	// CWC secrets and an unrelated one.
	for _, n := range []string{"cwcstate", "jwtsecret", "my-app-secret"} {
		s.Put("", "v1", "secrets", obj("v1", "Secret", "f5-utils", n))
	}
	// The namespace controller: deleting a namespace marks it Terminating; it
	// goes once nothing in it carries an F5 finalizer.
	s.Hook = func(s *kubefake.Server, r kubefake.Request) *kubefake.Reply {
		const p = "/api/v1/namespaces/"
		if !strings.HasPrefix(r.Path, p) || strings.Count(r.Path, "/") != 4 {
			return nil
		}
		ns := strings.TrimPrefix(r.Path, p)
		n := s.Get("", "v1", "namespaces", "", ns)
		if n == nil {
			return nil
		}
		if r.Method == "DELETE" {
			s.Put("", "v1", "namespaces", with(n, []string{"status", "phase"}, "Terminating"))
			rep := kubefake.Reply{Code: 200, Body: n}
			return &rep
		}
		if r.Method == "GET" && n.String("status", "phase") == "Terminating" {
			for _, o := range []struct{ g, res, name string }{{"k8s.f5.com", "f5tmms", "f5-tmm"}, {"k8s.f5net.com", "f5-big-tcp-settings", "sys-default-tcp"}} {
				if x := s.Get(o.g, "v1", o.res, ns, o.name); x != nil {
					for _, f := range x.Finalizers() {
						if IsF5Finalizer(f) {
							return nil // still held
						}
					}
				}
			}
			s.Remove("", "v1", "namespaces", "", ns)
		}
		return nil
	}
	cfg := PostUninstallConfig{BNKNamespace: "f5-bnk", UtilsNamespace: "f5-utils", FinalizerGrace: 50 * time.Millisecond, Timeout: 3 * time.Second, Poll: 10 * time.Millisecond}
	if err := PostUninstall(context.Background(), env, cfg, res); err != nil {
		t.Fatal(err)
	}
	if res.Failed() {
		t.Fatalf("failed: %v", res.Failures())
	}
	if got := s.Get("k8s.f5.com", "v1", "f5tmms", "f5-bnk", "f5-tmm").Finalizers(); !reflect.DeepEqual(got, []string{"kubernetes.io/pvc-protection"}) {
		t.Errorf("f5-tmm finalizers = %v, want only kubernetes.io/pvc-protection kept", got)
	}
	if got := s.Get("k8s.f5net.com", "v1", "f5-big-tcp-settings", "f5-bnk", "sys-default-tcp").Finalizers(); len(got) != 0 {
		t.Errorf("sys-default-tcp finalizers = %v", got)
	}
	if s.Get("", "v1", "secrets", "f5-utils", "cwcstate") != nil || s.Get("", "v1", "secrets", "f5-utils", "jwtsecret") != nil {
		t.Error("CWC secrets not deleted")
	}
	if s.Get("", "v1", "secrets", "f5-utils", "my-app-secret") == nil {
		t.Error("deleted a secret that is not on the CWC list")
	}
	if n := len(deletes(s.Requests(), "/api/v1/namespaces/f5-utils/secrets/")); n != len(LicenseSecretNames) {
		t.Errorf("%d secret deletes, want exactly the %d listed", n, len(LicenseSecretNames))
	}
}

// Finalizers are left alone while the namespace is still terminating on its own
// (inside the grace), and an extra namespace like cert-manager is never stripped.
func TestPostUninstallGraceAndScope(t *testing.T) {
	s, env, res := newFake(t)
	s.AddResource("k8s.f5.com", "v1", "f5tmms", "F5Tmm", true)
	s.Put("", "v1", "namespaces", with(obj("v1", "Namespace", "", "cert-manager"), []string{"status", "phase"}, "Terminating"))
	tmm := obj("k8s.f5.com/v1", "F5Tmm", "cert-manager", "odd")
	with(tmm, []string{"metadata", "finalizers"}, []any{"k8s.f5.com/CNEInstanceFinalizer"})
	s.Put("k8s.f5.com", "v1", "f5tmms", tmm)
	s.Hook = func(s *kubefake.Server, r kubefake.Request) *kubefake.Reply {
		if r.Method == "DELETE" && strings.HasSuffix(r.Path, "/namespaces/cert-manager") {
			rep := kubefake.Reply{Code: 200, Body: s.Get("", "v1", "namespaces", "", "cert-manager")}
			return &rep
		}
		return nil
	}
	cfg := PostUninstallConfig{BNKNamespace: "f5-bnk", UtilsNamespace: "f5-utils", DeleteNamespaces: []string{"cert-manager"}, FinalizerGrace: 10 * time.Millisecond, Timeout: 200 * time.Millisecond, Poll: 10 * time.Millisecond}
	_ = PostUninstall(context.Background(), env, cfg, res)
	if !hasSev(res, "namespaces", SevFail) || !strings.Contains(detail(res, "namespaces"), "cert-manager") {
		t.Fatalf("stuck cert-manager not reported: %+v", res.Findings)
	}
	if got := s.Get("k8s.f5.com", "v1", "f5tmms", "cert-manager", "odd").Finalizers(); len(got) != 1 {
		t.Errorf("stripped finalizers in a namespace that is not F5's: %v", got)
	}
}

// FLO restores its webhook within a second of removal (measured 375-815ms), so a
// timer sweep alone almost never coincides with a delete. Modelled here as: the
// webhook is back by the time the drain next lists. Only neutralise-and-retry
// inside the same pass gets the delete through.
func TestPreUninstallNeutralisesWebhookOnRefusal(t *testing.T) {
	s, env, res := newFake(t)
	installedBNK(s)
	operator(s)
	vwc := obj("admissionregistration.k8s.io/v1", "ValidatingWebhookConfiguration", "", "f5validate-f5-bnk")
	s.Hook = func(s *kubefake.Server, r kubefake.Request) *kubefake.Reply {
		if r.Method == "GET" && strings.HasSuffix(r.Path, "/namespaces/f5-bnk/gatewaysettings") {
			s.Put("admissionregistration.k8s.io", "v1", "validatingwebhookconfigurations", vwc) // FLO restores it
		}
		if r.Method == "DELETE" && strings.HasSuffix(r.Path, "/gatewaysettings/gs") &&
			s.Get("admissionregistration.k8s.io", "v1", "validatingwebhookconfigurations", "", "f5validate-f5-bnk") != nil {
			rep := kubefake.Status(500, "InternalError", `failed calling webhook "f5validate.f5net.com": no endpoints available for service "f5-validation"`)
			return &rep
		}
		return nil
	}
	cfg := uninstallCfg()
	cfg.WebhookInterval = time.Hour // the timer sweep never helps
	_ = PreUninstall(context.Background(), env, cfg, res)
	if res.Failed() {
		t.Fatalf("refused delete never got through: %v", res.Failures())
	}
}

// Even when an operator widens --drain-group to every F5 group, the k8s.f5.com
// components FLO re-creates are never drained (roksbnkctl #266: that drain can
// never finish), and the License still has its own step.
func TestPreUninstallWideDrainStillSkipsFLOComponents(t *testing.T) {
	s, env, res := newFake(t)
	installedBNK(s)
	operator(s)
	cfg := uninstallCfg()
	cfg.DrainGroups = F5Groups
	_ = PreUninstall(context.Background(), env, cfg, res)
	if res.Failed() {
		t.Fatalf("failed: %v", res.Failures())
	}
	got := crDeletes(s.Requests())
	for _, d := range got {
		if d == "dssms/f5-dssm" || d == "f5tmms/f5-tmm" {
			t.Fatalf("drained FLO component %s: %v", d, got)
		}
	}
	if got[len(got)-1] != "cneinstances/f5-bnk-f5-cne-controller" {
		t.Fatalf("CNEInstance not last: %v", got)
	}
}

// IPAM is controller-generated and owned: the drain leaves it to garbage
// collection, but the checkpoint must still SEE it. If it outlives its Infra,
// the CNEInstance is not deleted.
func TestPreUninstallOwnedIPAMBlocksCNEInstance(t *testing.T) {
	s, env, res := newFake(t)
	installedBNK(s)
	operator(s)
	inner := s.AfterDelete
	s.AfterDelete = func(s *kubefake.Server, r kubefake.Request) {
		if strings.HasSuffix(r.Path, "/infras/infra") {
			return // GC never removes the IPAM
		}
		inner(s, r)
	}
	cfg := uninstallCfg()
	cfg.DrainTimeout = 200 * time.Millisecond
	_ = PreUninstall(context.Background(), env, cfg, res)
	if !hasSev(res, "ipam", SevFail) || !strings.Contains(detail(res, "ipam"), "ipams/f5-bnk/ipam-infra") {
		t.Fatalf("owned IPAM leftover not caught: %+v", res.Findings)
	}
	for _, d := range crDeletes(s.Requests()) {
		if strings.HasPrefix(d, "cneinstances/") {
			t.Fatal("CNEInstance deleted with IPAM still present")
		}
	}
}
