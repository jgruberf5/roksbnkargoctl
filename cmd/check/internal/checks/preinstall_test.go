package checks

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jgruberf5/roksbnkargoctl/cmd/check/internal/kube"
	"github.com/jgruberf5/roksbnkargoctl/cmd/check/internal/kube/kubefake"
)

// healthyCluster seeds a cluster every static pre-install check passes on.
func healthyCluster(s *kubefake.Server) {
	s.Put("config.openshift.io", "v1", "clusterversions", with(obj("config.openshift.io/v1", "ClusterVersion", "", "version"), []string{"status", "desired", "version"}, "4.21.5"))
	for i, z := range []string{"us-south-1", "us-south-2", "us-south-3"} {
		n := obj("v1", "Node", "", fmt.Sprintf("10.0.%d.4", i))
		with(n, []string{"metadata", "labels"}, map[string]any{"topology.kubernetes.io/zone": z, "node-role.kubernetes.io/worker": ""})
		with(n, []string{"status", "conditions"}, conditions("Ready", "True"))
		s.Put("", "v1", "nodes", n)
	}
	sc := obj("storage.k8s.io/v1", "StorageClass", "", "ibmc-vpc-block-10iops-tier")
	with(sc, []string{"metadata", "annotations"}, map[string]any{"storageclass.kubernetes.io/is-default-class": "true"})
	sc["provisioner"] = ProvisionerVPCBlock
	s.Put("storage.k8s.io", "v1", "storageclasses", sc)
	fsc := obj("storage.k8s.io/v1", "StorageClass", "", "ibmc-vpc-file-500-iops")
	fsc["provisioner"] = ProvisionerVPCFile
	s.Put("storage.k8s.io", "v1", "storageclasses", fsc)
}

func baseCfg() PreInstallConfig {
	return PreInstallConfig{BNKNamespace: "f5-bnk", UtilsNamespace: "f5-utils", MinOpenShift: "4.16", MinZones: 3, MinWorkers: 3, SkipProbe: true, TMMReplicas: 1}
}

func floDeployment() kube.Object {
	d := obj("apps/v1", "Deployment", "f5-bnk", "flo-f5-lifecycle-operator")
	with(d, []string{"metadata", "labels"}, map[string]any{"app.kubernetes.io/name": "f5-lifecycle-operator"})
	return d
}

func TestPreInstallCleanClusterPasses(t *testing.T) {
	s, env, res := newFake(t)
	healthyCluster(s)
	if err := PreInstall(context.Background(), env, baseCfg(), res); err != nil {
		t.Fatal(err)
	}
	if res.Failed() {
		t.Fatalf("clean cluster failed: %v", res.Failures())
	}
}

// The bug it names: installing over a live FLO. Must fail, naming it.
func TestPreInstallDetectsExistingFLO(t *testing.T) {
	s, env, res := newFake(t)
	healthyCluster(s)
	s.Put("apps", "v1", "deployments", floDeployment())
	if err := PreInstall(context.Background(), env, baseCfg(), res); err != nil {
		t.Fatal(err)
	}
	if !hasSev(res, "existing-bnk", SevFail) {
		t.Fatalf("existing FLO not detected: %+v", res.Findings)
	}
	if !strings.Contains(detail(res, "existing-bnk"), "f5-bnk/flo-f5-lifecycle-operator") {
		t.Fatalf("finding does not name the FLO: %s", detail(res, "existing-bnk"))
	}
}

// A chart with overridden labels still has the name.
func TestPreInstallDetectsFLOByNameWithoutLabel(t *testing.T) {
	s, env, res := newFake(t)
	healthyCluster(s)
	s.Put("apps", "v1", "deployments", obj("apps/v1", "Deployment", "f5-bnk", "flo-f5-lifecycle-operator"))
	_ = PreInstall(context.Background(), env, baseCfg(), res)
	if !hasSev(res, "existing-bnk", SevFail) {
		t.Fatalf("unlabelled FLO not detected: %+v", res.Findings)
	}
}

func TestPreInstallDetectsLeftovers(t *testing.T) {
	s, env, res := newFake(t)
	healthyCluster(s)
	s.AddResource("k8s.f5.com", "v1", "cneinstances", "CNEInstance", true)
	s.AddResource("k8s.f5net.com", "v1", "licenses", "License", true)
	s.Put("k8s.f5.com", "v1", "cneinstances", obj("k8s.f5.com/v1", "CNEInstance", "f5-bnk", "f5-bnk-f5-cne-controller"))
	s.Put("k8s.f5net.com", "v1", "licenses", obj("k8s.f5net.com/v1", "License", "f5-utils", "bnk-license"))
	s.Put("", "v1", "namespaces", with(obj("v1", "Namespace", "", "f5-utils"), []string{"status", "phase"}, "Terminating"))
	s.Put("", "v1", "secrets", obj("v1", "Secret", "f5-utils", "cwcstate"))
	_ = PreInstall(context.Background(), env, baseCfg(), res)
	d := detail(res, "existing-bnk")
	for _, want := range []string{"CNEInstance f5-bnk/f5-bnk-f5-cne-controller", "License f5-utils/bnk-license", "f5-utils is Terminating", "cwcstate"} {
		if !strings.Contains(d, want) {
			t.Errorf("missing %q in %s", want, d)
		}
	}
}

// A re-sync of the same Application must not fail on its own FLO.
func TestPreInstallResyncOfSameAppPasses(t *testing.T) {
	s, env, res := newFake(t)
	healthyCluster(s)
	d := floDeployment()
	with(d, []string{"metadata", "annotations"}, map[string]any{"argocd.argoproj.io/tracking-id": "bnk-demo:apps/Deployment:f5-bnk/flo-f5-lifecycle-operator"})
	s.Put("apps", "v1", "deployments", d)
	cfg := baseCfg()
	cfg.ArgoApp = "bnk-demo"
	_ = PreInstall(context.Background(), env, cfg, res)
	if hasSev(res, "existing-bnk", SevFail) {
		t.Fatalf("re-sync of own app failed: %s", detail(res, "existing-bnk"))
	}
	// ...but another app's FLO still fails.
	res2 := NewResult("x", env.Log)
	cfg.ArgoApp = "other"
	_ = PreInstall(context.Background(), env, cfg, res2)
	if !hasSev(res2, "existing-bnk", SevFail) {
		t.Fatal("another Application's FLO was accepted")
	}
}

func TestPreInstallClusterShape(t *testing.T) {
	s, env, res := newFake(t)
	healthyCluster(s)
	s.Put("config.openshift.io", "v1", "clusterversions", with(obj("config.openshift.io/v1", "ClusterVersion", "", "version"), []string{"status", "desired", "version"}, "4.15.9"))
	// Cordon one node: 2 workers in 2 zones.
	n := s.Get("", "v1", "nodes", "", "10.0.2.4")
	with(n, []string{"spec", "unschedulable"}, true)
	s.Put("", "v1", "nodes", n)
	_ = PreInstall(context.Background(), env, baseCfg(), res)
	for _, c := range []string{"openshift-version", "workers", "zones"} {
		if !hasSev(res, c, SevFail) {
			t.Errorf("%s did not fail: %s", c, detail(res, c))
		}
	}
}

func TestPreInstallSecretsAndRWX(t *testing.T) {
	s, env, res := newFake(t)
	healthyCluster(s)
	sec := obj("v1", "Secret", "f5-bnk", "far-secret")
	sec["data"] = map[string]any{".dockerconfigjson": b64("{}")}
	s.Put("", "v1", "secrets", sec)
	cfg := baseCfg()
	cfg.RequiredSecrets = []string{"f5-bnk/far-secret", "roksbnkargoctl-check/bnk-license-jwt"}
	cfg.TMMReplicas = 2 // default class is block: must fail naming the add-on
	_ = PreInstall(context.Background(), env, cfg, res)
	if !hasSev(res, "secrets", SevFail) || !strings.Contains(detail(res, "secrets"), "roksbnkargoctl-check/bnk-license-jwt") {
		t.Errorf("missing secret not reported: %s", detail(res, "secrets"))
	}
	if strings.Contains(detail(res, "secrets"), "f5-bnk/far-secret") {
		t.Errorf("present secret reported missing: %s", detail(res, "secrets"))
	}
	if !hasSev(res, "storageclass-rwx", SevFail) || !strings.Contains(detail(res, "storageclass-rwx"), "vpc-file-csi-driver") {
		t.Errorf("block class with 2 TMM replicas not refused: %s", detail(res, "storageclass-rwx"))
	}
	// Named file class passes.
	res2 := NewResult("x", env.Log)
	cfg.StorageClass = "ibmc-vpc-file-500-iops"
	_ = PreInstall(context.Background(), env, cfg, res2)
	if !hasSev(res2, "storageclass-rwx", SevPass) {
		t.Errorf("file class refused: %s", detail(res2, "storageclass-rwx"))
	}
}

// --- node-probe aggregation -------------------------------------------------

func probeDS(s *kubefake.Server, desired int) {
	ds := obj("apps/v1", "DaemonSet", "roksbnkargoctl-check", "check-node-probe")
	with(ds, []string{"spec", "selector", "matchLabels"}, map[string]any{"app": "check-node-probe"})
	with(ds, []string{"status", "desiredNumberScheduled"}, float64(desired))
	with(ds, []string{"metadata", "generation"}, float64(2))
	s.Put("apps", "v1", "daemonsets", ds)
}

func probePodName(node string) string { return "check-node-probe-" + node }

func probePod(s *kubefake.Server, node string, rep *ProbeReport, created time.Time) {
	p := obj("v1", "Pod", "roksbnkargoctl-check", probePodName(node))
	with(p, []string{"metadata", "labels"}, map[string]any{"app": "check-node-probe"})
	with(p, []string{"metadata", "creationTimestamp"}, created.UTC().Format(time.RFC3339))
	ann := map[string]any{"deprecated.daemonset.template.generation": "2"}
	if rep != nil {
		b, _ := json.Marshal(rep)
		ann[ProbeAnnotation] = string(b)
	}
	with(p, []string{"metadata", "annotations"}, ann)
	with(p, []string{"spec", "nodeName"}, node)
	s.Put("", "v1", "pods", p)
}

// recreate plays the DaemonSet controller: a deleted probe pod comes back as a
// NEW pod (new UID, current creationTimestamp) carrying fresh[node].
func recreate(s *kubefake.Server, fresh map[string]*ProbeReport) {
	s.AfterDelete = func(s *kubefake.Server, r kubefake.Request) {
		if !strings.Contains(r.Path, "/pods/") {
			return
		}
		node := strings.TrimPrefix(r.Path[strings.LastIndex(r.Path, "/")+1:], "check-node-probe-")
		probePod(s, node, fresh[node], time.Now())
	}
}

func report(run, node string, ok bool) *ProbeReport {
	far := TargetResult{Target: "cr.f5.com:443", TLS: true, OK: true, DNS: "ok", TCP: "ok"}
	flp := TargetResult{Target: "10.243.0.5:8443", OK: ok, DNS: "skipped-ip", TCP: "ok"}
	if !ok {
		flp.TCP = "FAILED"
		flp.Error = "tcp connect failed (refused, filtered, or no route): i/o timeout"
	}
	return &ProbeReport{RunID: run, Node: node, OK: ok, Targets: []TargetResult{far, flp}}
}

func probeCfg() PreInstallConfig {
	c := baseCfg()
	c.SkipProbe = false
	c.ProbeDaemonSet, c.ProbeNamespace, c.RunID = "check-node-probe", "roksbnkargoctl-check", "run-7"
	c.Timeout, c.PollInterval = 300*time.Millisecond, 20*time.Millisecond
	return c
}

var anHourAgo = time.Now().Add(-time.Hour)

// The bug it names: one node that cannot reach a target must fail the gate,
// naming the node, the target and the error — even though the other two pass,
// and even though the stale verdicts all passed.
func TestNodeProbeAggregationFailsOnOneUnreachableNode(t *testing.T) {
	s, env, res := newFake(t)
	healthyCluster(s)
	probeDS(s, 3)
	for _, n := range []string{"node-a", "node-b", "node-c"} {
		probePod(s, n, report("run-7", n, true), anHourAgo)
	}
	recreate(s, map[string]*ProbeReport{
		"node-a": report("run-7", "node-a", true),
		"node-b": report("run-7", "node-b", false),
		"node-c": report("run-7", "node-c", true),
	})
	_ = PreInstall(context.Background(), env, probeCfg(), res)
	if !hasSev(res, "node-probe", SevFail) {
		t.Fatalf("unreachable node passed: %+v", res.Findings)
	}
	d := detail(res, "node-probe")
	for _, want := range []string{"node-b", "10.243.0.5:8443", "i/o timeout"} {
		if !strings.Contains(d, want) {
			t.Errorf("finding lacks %q: %s", want, d)
		}
	}
	if strings.Contains(d, "node-a ->") || strings.Contains(d, "node-c ->") {
		t.Errorf("healthy nodes reported as failures: %s", d)
	}
}

// Stale FAILED verdicts are replaced by fresh passing ones.
func TestNodeProbeAllReachablePasses(t *testing.T) {
	s, env, res := newFake(t)
	healthyCluster(s)
	probeDS(s, 2)
	probePod(s, "node-a", report("run-7", "node-a", false), anHourAgo)
	probePod(s, "node-b", report("run-7", "node-b", false), anHourAgo)
	recreate(s, map[string]*ProbeReport{"node-a": report("run-7", "node-a", true), "node-b": report("run-7", "node-b", true)})
	_ = PreInstall(context.Background(), env, probeCfg(), res)
	if res.Failed() {
		t.Fatalf("all-reachable failed: %v", res.Failures())
	}
}

// Coverage is required: two passing nodes of three must not pass.
func TestNodeProbeMissingNodeFails(t *testing.T) {
	s, env, res := newFake(t)
	healthyCluster(s)
	probeDS(s, 3)
	for _, n := range []string{"node-a", "node-b", "node-c"} {
		probePod(s, n, report("run-7", n, true), anHourAgo)
	}
	recreate(s, map[string]*ProbeReport{"node-a": report("run-7", "node-a", true), "node-b": report("run-7", "node-b", true)})
	_ = PreInstall(context.Background(), env, probeCfg(), res)
	if !hasSev(res, "node-probe", SevFail) || !strings.Contains(detail(res, "node-probe"), "node-c") {
		t.Fatalf("missing node not failed/named: %s", detail(res, "node-probe"))
	}
}

// roksbnkctl #57: a re-sync with an unchanged config keeps the same run id, so
// the old pod's verdict LOOKS current. pre-install must delete the probe pods
// BEFORE waiting, and must not accept the old pod's (passing) verdict even if
// nothing replaces it.
func TestPreInstallRestartsProbePodsBeforeWaiting(t *testing.T) {
	s, env, res := newFake(t)
	healthyCluster(s)
	probeDS(s, 1)
	probePod(s, "node-a", report("run-7", "node-a", true), anHourAgo) // same run id, passing
	// Pretend the delete is still finalizing: the old pod stays listed, same UID.
	s.Hook = func(s *kubefake.Server, r kubefake.Request) *kubefake.Reply {
		if r.Method == "DELETE" && strings.Contains(r.Path, "/pods/") {
			rep := kubefake.Reply{Code: 200, Body: map[string]any{"kind": "Status", "status": "Success"}}
			return &rep
		}
		return nil
	}
	_ = PreInstall(context.Background(), env, probeCfg(), res)
	if !hasSev(res, "node-probe", SevFail) {
		t.Fatalf("the pre-existing pod's verdict was accepted: %+v", res.Findings)
	}
	reqs := s.Requests()
	firstDelete, waitList := -1, -1
	podLists := 0
	for i, r := range reqs {
		if r.Method == "DELETE" && strings.HasSuffix(r.Path, "/pods/check-node-probe-node-a") && firstDelete < 0 {
			firstDelete = i
		}
		if r.Method == "GET" && strings.HasSuffix(r.Path, "/namespaces/roksbnkargoctl-check/pods") {
			podLists++
			if podLists == 2 && waitList < 0 { // the first list is the one the restart deletes from
				waitList = i
			}
		}
	}
	if firstDelete < 0 || waitList < 0 || firstDelete > waitList {
		t.Fatalf("delete (%d) did not precede the wait (%d)", firstDelete, waitList)
	}
}

// With the pod really replaced, the fresh verdict is waited for and used.
func TestNodeProbeWaitsForFreshVerdict(t *testing.T) {
	s, env, res := newFake(t)
	healthyCluster(s)
	probeDS(s, 1)
	probePod(s, "node-a", report("run-7", "node-a", false), anHourAgo) // stale failure
	recreate(s, map[string]*ProbeReport{"node-a": nil})                // new pod, not yet reported
	gets := 0
	s.Hook = func(s *kubefake.Server, r kubefake.Request) *kubefake.Reply {
		if r.Method == "GET" && strings.HasSuffix(r.Path, "/pods") {
			gets++
			if gets == 4 {
				p := s.Get("", "v1", "pods", "roksbnkargoctl-check", probePodName("node-a"))
				b, _ := json.Marshal(report("run-7", "node-a", true))
				p.Map("metadata", "annotations")[ProbeAnnotation] = string(b)
				s.Put("", "v1", "pods", p)
			}
		}
		return nil
	}
	cfg := probeCfg()
	cfg.Timeout = 2 * time.Second
	_ = PreInstall(context.Background(), env, cfg, res)
	if res.Failed() {
		t.Fatalf("fresh verdict not waited for: %v", res.Failures())
	}
	if gets < 4 {
		t.Fatalf("did not wait: %d polls", gets)
	}
}

// A result from another run id is ignored even on a new pod.
func TestNodeProbeIgnoresOtherRunID(t *testing.T) {
	s, env, res := newFake(t)
	healthyCluster(s)
	probeDS(s, 1)
	probePod(s, "node-a", nil, anHourAgo)
	recreate(s, map[string]*ProbeReport{"node-a": report("run-6", "node-a", true)})
	_ = PreInstall(context.Background(), env, probeCfg(), res)
	if !hasSev(res, "node-probe", SevFail) || !strings.Contains(detail(res, "node-probe"), "run-6") {
		t.Fatalf("other run's verdict accepted: %s", detail(res, "node-probe"))
	}
}

// End to end: a real node-probe publishes on its (re-created) pod; pre-install
// reads it back and fails naming the node.
func TestNodeProbePublishesAnnotation(t *testing.T) {
	s, env, res := newFake(t)
	probeDS(s, 1)
	probePod(s, "node-a", nil, anHourAgo)
	cfg := NodeProbeConfig{
		Targets:  []ProbeTarget{{Host: "127.0.0.1", Port: "1"}}, // closed port
		Prober:   Prober{Window: 0, DialTimeout: time.Second},
		RunID:    "run-7",
		NodeName: "node-a", PodName: probePodName("node-a"), PodNamespace: "roksbnkargoctl-check",
	}
	probeErr := make(chan error, 1)
	s.AfterDelete = func(s *kubefake.Server, r kubefake.Request) {
		probePod(s, "node-a", nil, time.Now())
		go func() { probeErr <- NodeProbe(context.Background(), env, cfg, res) }()
	}
	gate := NewResult("gate", env.Log)
	pc := probeCfg()
	pc.Timeout = 3 * time.Second
	if err := waitNodeProbes(context.Background(), env, pc, gate); err != nil {
		t.Fatal(err)
	}
	if err := <-probeErr; err != nil {
		t.Fatal(err)
	}
	if res.Failed() {
		t.Fatal("node-probe must not fail its own pod on an unreachable target")
	}
	var rep ProbeReport
	p := s.Get("", "v1", "pods", "roksbnkargoctl-check", probePodName("node-a"))
	if err := json.Unmarshal([]byte(p.Annotations()[ProbeAnnotation]), &rep); err != nil {
		t.Fatal(err)
	}
	if rep.OK || rep.RunID != "run-7" || rep.Node != "node-a" || rep.Targets[0].TCP != "FAILED" {
		t.Fatalf("bad report: %+v", rep)
	}
	if !hasSev(gate, "node-probe", SevFail) || !strings.Contains(detail(gate, "node-probe"), "node-a -> 127.0.0.1:1") {
		t.Fatalf("gate did not fail on the published failure: %s", detail(gate, "node-probe"))
	}
}

// The UID guard on its own: the old pod was created seconds ago (inside the
// clock-skew allowance) and its delete is still finalizing. Its verdict must
// not count.
func TestNodeProbeOldPodExcludedByUID(t *testing.T) {
	s, env, res := newFake(t)
	healthyCluster(s)
	probeDS(s, 1)
	probePod(s, "node-a", report("run-7", "node-a", true), time.Now())
	s.Hook = func(s *kubefake.Server, r kubefake.Request) *kubefake.Reply {
		if r.Method == "DELETE" && strings.Contains(r.Path, "/pods/") {
			rep := kubefake.Reply{Code: 200, Body: map[string]any{"kind": "Status", "status": "Success"}}
			return &rep
		}
		return nil
	}
	_ = PreInstall(context.Background(), env, probeCfg(), res)
	if !hasSev(res, "node-probe", SevFail) {
		t.Fatal("the pre-existing pod's verdict was accepted")
	}
}

// The timestamp guard on its own: an old pod the restart never saw (its list
// raced) is still not trusted.
func TestNodeProbeOldPodExcludedByCreationTime(t *testing.T) {
	s, env, res := newFake(t)
	healthyCluster(s)
	probeDS(s, 1)
	probePod(s, "node-a", report("run-7", "node-a", true), anHourAgo)
	lists := 0
	s.Hook = func(s *kubefake.Server, r kubefake.Request) *kubefake.Reply {
		if r.Method == "GET" && strings.HasSuffix(r.Path, "/pods") {
			lists++
			if lists == 1 {
				rep := kubefake.Reply{Code: 200, Body: map[string]any{"items": []any{}}}
				return &rep
			}
		}
		return nil
	}
	_ = PreInstall(context.Background(), env, probeCfg(), res)
	if !hasSev(res, "node-probe", SevFail) {
		t.Fatal("an hour-old verdict was accepted")
	}
}
