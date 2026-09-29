package checks

import (
	"context"
	"strings"
	"testing"

	"github.com/jgruberf5/roksbnkargoctl/cmd/check/internal/kube/kubefake"
)

func tmmPod(s *kubefake.Server, name, node string, ready bool) {
	p := obj("v1", "Pod", "f5-bnk", name)
	with(p, []string{"metadata", "labels"}, map[string]any{"app": "f5-tmm"})
	with(p, []string{"spec", "nodeName"}, node)
	st := "False"
	if ready {
		st = "True"
	}
	with(p, []string{"status", "phase"}, "Running")
	with(p, []string{"status", "conditions"}, conditions("Ready", st))
	s.Put("", "v1", "pods", p)
}

func installedHealthy(s *kubefake.Server) {
	healthyCluster(s)
	s.AddResource("k8s.f5.com", "v1", "cneinstances", "CNEInstance", true)
	s.AddResource("k8s.f5net.com", "v1", "licenses", "License", true)
	flo := floDeployment()
	with(flo, []string{"status", "availableReplicas"}, float64(1))
	with(flo, []string{"status", "conditions"}, conditions("Available", "True"))
	s.Put("apps", "v1", "deployments", flo)
	s.Put("k8s.f5.com", "v1", "cneinstances", with(with(obj("k8s.f5.com/v1", "CNEInstance", "f5-bnk", "f5-bnk-f5-cne-controller"),
		[]string{"status", "conditions"}, conditions("Available", "True")), []string{"spec", "deploymentSize"}, "Tiny"))
	s.Put("k8s.f5net.com", "v1", "licenses", with(obj("k8s.f5net.com/v1", "License", "f5-utils", "bnk-license"), []string{"status", "state"}, "Active"))
	tmmPod(s, "tmm-a", "10.0.0.4", true)
	tmmPod(s, "tmm-b", "10.0.1.4", true)
}

func TestPostInstallHealthy(t *testing.T) {
	s, env, res := newFake(t)
	installedHealthy(s)
	_ = PostInstall(context.Background(), env, PostInstallConfig{BNKNamespace: "f5-bnk", UtilsNamespace: "f5-utils", TMMReplicas: 2}, res)
	if res.Failed() {
		t.Fatalf("%v", res.Failures())
	}
}

func TestPostInstallFindsProblems(t *testing.T) {
	s, env, res := newFake(t)
	installedHealthy(s)
	tmmPod(s, "tmm-b", "10.0.0.4", true) // both in one zone
	bad := obj("v1", "Pod", "f5-utils", "f5-spk-cwc-0")
	with(bad, []string{"status", "containerStatuses"}, []any{map[string]any{"name": "cwc", "state": map[string]any{"waiting": map[string]any{"reason": "ImagePullBackOff", "message": "pull access denied"}}}})
	s.Put("", "v1", "pods", bad)
	s.Put("k8s.f5net.com", "v1", "licenses", with(obj("k8s.f5net.com/v1", "License", "f5-utils", "bnk-license"), []string{"status", "state"}, "Verification In Progress"))
	_ = PostInstall(context.Background(), env, PostInstallConfig{BNKNamespace: "f5-bnk", UtilsNamespace: "f5-utils", TMMReplicas: 3}, res)
	for _, c := range []string{"tmm", "tmm-zones", "pods", "license"} {
		if !hasSev(res, c, SevFail) {
			t.Errorf("%s not failed: %s", c, detail(res, c))
		}
	}
	if !strings.Contains(detail(res, "pods"), "f5-utils/f5-spk-cwc-0 cwc: ImagePullBackOff") {
		t.Errorf("pod problem not named: %s", detail(res, "pods"))
	}
}

// Only Tiny runs on ROKS; a hand-edited size in Git must fail post-install and
// say why, not sit Pending on hugepages.
func TestPostInstallRequiresTiny(t *testing.T) {
	for _, size := range []string{"Tiny", "Small", "Max", ""} {
		res := NewResult("post-install", nil)
		o := obj("k8s.f5.com/v1", "CNEInstance", "f5-bnk", "f5-bnk-f5-cne-controller")
		o["spec"] = map[string]any{"deploymentSize": size}
		checkDeploymentSize(o, res)
		wantFail := size != "Tiny"
		if hasSev(res, "deployment-size", SevFail) != wantFail {
			t.Errorf("deploymentSize %q: fail=%v, want %v (%s)", size, !wantFail, wantFail, detail(res, "deployment-size"))
		}
		if wantFail && !strings.Contains(detail(res, "deployment-size"), "hugepages") {
			t.Errorf("deploymentSize %q: the failure must name the hugepages reason: %s", size, detail(res, "deployment-size"))
		}
	}
}
