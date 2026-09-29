package checks

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jgruberf5/roksbnkargoctl/cmd/check/internal/kube"
)

// PreInstallConfig configures pre-install.
type PreInstallConfig struct {
	BNKNamespace   string
	UtilsNamespace string
	MinOpenShift   string // "4.16"
	MinZones       int
	MinWorkers     int
	// RequiredSecrets are "namespace/name" of the out-of-band Secrets install writes.
	RequiredSecrets []string
	TMMReplicas     int
	StorageClass    string // "" = the default class
	// ArgoApp, when set, lets a RE-SYNC of that Application pass the
	// "no existing BNK" check: objects it tracks are this install, not a stale one.
	ArgoApp string

	ProbeDaemonSet string
	ProbeNamespace string
	RunID          string
	SkipProbe      bool
	Timeout        time.Duration // for the probe wait
	PollInterval   time.Duration
}

// IBM Cloud VPC CSI provisioners (reported in findings).
const (
	ProvisionerVPCFile  = "vpc.file.csi.ibm.io"
	ProvisionerVPCBlock = "vpc.block.csi.ibm.io"
)

// PreInstall runs every static check, then waits for the node probes. It
// reports all problems in one run rather than stopping at the first: a hook
// that fails five times for five reasons costs five syncs.
func PreInstall(ctx context.Context, env *Env, cfg PreInstallConfig, res *Result) error {
	checkOpenShiftVersion(ctx, env, cfg, res)
	checkNodes(ctx, env, cfg, res)
	checkNoExistingBNK(ctx, env, cfg, res)
	checkSecrets(ctx, env, cfg, res)
	checkStorageClass(ctx, env, cfg, res)
	if cfg.SkipProbe {
		res.Info("node-probe", "skipped (--skip-probe)")
		return nil
	}
	return waitNodeProbes(ctx, env, cfg, res)
}

func parseMajorMinor(v string) (int, int, bool) {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	parts := strings.SplitN(v, ".", 3)
	if len(parts) < 2 {
		return 0, 0, false
	}
	maj, e1 := strconv.Atoi(parts[0])
	min, e2 := strconv.Atoi(strings.SplitN(parts[1], "-", 2)[0])
	return maj, min, e1 == nil && e2 == nil
}

func checkOpenShiftVersion(ctx context.Context, env *Env, cfg PreInstallConfig, res *Result) {
	cv, err := env.Kube.Get(ctx, GVRClusterVer.Path("", "version"))
	if err != nil {
		res.Fail("openshift-version", "cannot read clusterversions.config.openshift.io/version (is this OpenShift?): %v", err)
		return
	}
	v := cv.String("status", "desired", "version")
	if v == "" {
		if h := cv.Slice("status", "history"); len(h) > 0 {
			if m, ok := h[0].(map[string]any); ok {
				v, _ = m["version"].(string)
			}
		}
	}
	maj, min, ok := parseMajorMinor(v)
	wMaj, wMin, _ := parseMajorMinor(cfg.MinOpenShift)
	switch {
	case !ok:
		res.Fail("openshift-version", "could not parse the cluster version %q", v)
	case maj < wMaj || (maj == wMaj && min < wMin):
		res.Fail("openshift-version", "OpenShift %s is older than the required %s", v, cfg.MinOpenShift)
	default:
		res.Pass("openshift-version", "OpenShift %s (>= %s)", v, cfg.MinOpenShift)
	}
}

// NodeZone is the node's zone label.
func NodeZone(n kube.Object) string {
	l := n.Labels()
	if z := l["topology.kubernetes.io/zone"]; z != "" {
		return z
	}
	return l["failure-domain.beta.kubernetes.io/zone"]
}

// isWorker: labelled worker, or carrying no role at all. ROKS labels workers
// with both master and worker roles on some versions, so master is NOT used to
// exclude a node.
func isWorker(n kube.Object) bool {
	l := n.Labels()
	if _, ok := l["node-role.kubernetes.io/worker"]; ok {
		return true
	}
	for k := range l {
		if strings.HasPrefix(k, "node-role.kubernetes.io/") {
			return false
		}
	}
	return true
}

func schedulable(n kube.Object) (bool, string) {
	if n.Bool("spec", "unschedulable") {
		return false, "cordoned"
	}
	for _, t := range n.Slice("spec", "taints") {
		m, _ := t.(map[string]any)
		if e, _ := m["effect"].(string); e == "NoSchedule" || e == "NoExecute" {
			return false, fmt.Sprintf("tainted %v:%s", m["key"], e)
		}
	}
	if !n.ConditionTrue("Ready") {
		return false, "not Ready"
	}
	return true, ""
}

func checkNodes(ctx context.Context, env *Env, cfg PreInstallConfig, res *Result) {
	nodes, err := env.Kube.List(ctx, GVRNode.Path("", ""), kube.ListOptions{})
	if err != nil {
		res.Fail("nodes", "listing nodes: %v", err)
		return
	}
	zones := map[string]int{}
	usable := 0
	var skipped []string
	for _, n := range nodes {
		if !isWorker(n) {
			continue
		}
		if ok, why := schedulable(n); !ok {
			skipped = append(skipped, n.Name()+" ("+why+")")
			continue
		}
		usable++
		zones[NodeZone(n)]++
	}
	if len(skipped) > 0 {
		res.Info("workers", "not counted: %s", joinMax(skipped, 10))
	}
	if usable < cfg.MinWorkers {
		res.Fail("workers", "%d schedulable Ready worker(s); at least %d required", usable, cfg.MinWorkers)
	} else {
		res.Pass("workers", "%d schedulable Ready worker(s)", usable)
	}
	var zl []string
	for z, c := range zones {
		if z == "" {
			z = "<no zone label>"
		}
		zl = append(zl, fmt.Sprintf("%s=%d", z, c))
	}
	sort.Strings(zl)
	nz := len(zones)
	if _, unlabelled := zones[""]; unlabelled {
		nz--
	}
	if nz < cfg.MinZones {
		res.Fail("zones", "schedulable workers span %d zone(s) [%s]; %d required", nz, strings.Join(zl, " "), cfg.MinZones)
	} else {
		res.Pass("zones", "schedulable workers span %d zones [%s]", nz, strings.Join(zl, " "))
	}
}

// checkNoExistingBNK refuses to install over a previous BNK. Each leftover fails
// the NEXT install rather than its own teardown (roksbnkctl #172): a live FLO or
// CNEInstance collides, a stale License or CWC secret makes licensing find a
// previous activation and never re-activate, and a Terminating namespace refuses
// every create with "being terminated".
func checkNoExistingBNK(ctx context.Context, env *Env, cfg PreInstallConfig, res *Result) {
	floDeps, err := findFLO(ctx, env, cfg.BNKNamespace)
	if err != nil {
		res.Fail("existing-bnk", "cannot look for an F5 Lifecycle Operator: %v", err)
		return
	}
	// A re-sync of THIS Application is not a pre-existing install.
	for _, d := range floDeps {
		if managedBy(d, cfg.ArgoApp) {
			res.Info("existing-bnk", "FLO %s is tracked by Argo CD Application %q: this is a re-sync, not a stale install; existing-BNK checks skipped", d.Ref(), cfg.ArgoApp)
			return
		}
	}
	var found []string
	for _, d := range floDeps {
		found = append(found, "FLO Deployment "+d.Ref())
	}
	cnes, err := env.Kube.List(ctx, GVRCNEInstance.Path("", ""), kube.ListOptions{})
	switch {
	case err == nil:
		for _, c := range cnes {
			found = append(found, "CNEInstance "+c.Ref())
		}
	case !kube.IsNotFound(err): // 404 = CRD absent = none
		res.Fail("existing-bnk", "listing CNEInstances: %v", err)
	}
	lics, err := env.Kube.List(ctx, GVRLicense.Path("", ""), kube.ListOptions{})
	switch {
	case err == nil:
		for _, l := range lics {
			found = append(found, "License "+l.Ref())
		}
	case !kube.IsNotFound(err):
		res.Fail("existing-bnk", "listing Licenses: %v", err)
	}
	for _, ns := range dedupe(cfg.BNKNamespace, cfg.UtilsNamespace) {
		n, err := env.Kube.Get(ctx, GVRNamespace.Path("", ns))
		if err != nil {
			if !kube.IsNotFound(err) {
				res.Fail("existing-bnk", "reading namespace %s: %v", ns, err)
			}
			continue
		}
		if n.String("status", "phase") == "Terminating" {
			found = append(found, "namespace "+ns+" is Terminating (run post-uninstall, or clear its F5 finalizers)")
		}
	}
	if cfg.UtilsNamespace != "" {
		secs, err := env.Kube.List(ctx, GVRSecret.Path(cfg.UtilsNamespace, ""), kube.ListOptions{})
		if err == nil {
			want := map[string]bool{}
			for _, s := range LicenseSecretNames {
				want[s] = true
			}
			var stale []string
			for _, s := range secs {
				if want[s.Name()] {
					stale = append(stale, s.Name())
				}
			}
			if len(stale) > 0 {
				found = append(found, fmt.Sprintf("%d CWC license secret(s) in %s [%s] (licensing would find the old activation)", len(stale), cfg.UtilsNamespace, joinMax(stale, 6)))
			}
		} else if !kube.IsNotFound(err) {
			res.Fail("existing-bnk", "listing secrets in %s: %v", cfg.UtilsNamespace, err)
		}
	}
	if len(found) > 0 {
		sort.Strings(found)
		res.Fail("existing-bnk", "BNK is already (or partly) installed: %s", strings.Join(found, "; "))
		return
	}
	res.Pass("existing-bnk", "no FLO, CNEInstance, License, Terminating BNK namespace or CWC license secret")
}

// findFLO finds the F5 Lifecycle Operator Deployment by label anywhere, or by
// name in the BNK namespace (a chart with overridden labels still has the name).
func findFLO(ctx context.Context, env *Env, ns string) ([]kube.Object, error) {
	byLabel, err := env.Kube.List(ctx, GVRDeployment.Path("", ""), kube.ListOptions{LabelSelector: FLOLabelSelector})
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []kube.Object
	for _, d := range byLabel {
		seen[d.Ref()] = true
		out = append(out, d)
	}
	if ns != "" {
		inNS, err := env.Kube.List(ctx, GVRDeployment.Path(ns, ""), kube.ListOptions{})
		if err != nil && !kube.IsNotFound(err) {
			return nil, err
		}
		for _, d := range inNS {
			if strings.Contains(d.Name(), "f5-lifecycle-operator") && !seen[d.Ref()] {
				out = append(out, d)
			}
		}
	}
	return out, nil
}

func dedupe(vals ...string) []string {
	seen := map[string]bool{}
	var out []string
	for _, v := range vals {
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}

func checkSecrets(ctx context.Context, env *Env, cfg PreInstallConfig, res *Result) {
	if len(cfg.RequiredSecrets) == 0 {
		res.Info("secrets", "no --require-secret given")
		return
	}
	var missing, present []string
	for _, ref := range cfg.RequiredSecrets {
		ns, name, ok := strings.Cut(ref, "/")
		if !ok || ns == "" || name == "" {
			res.Fail("secrets", "bad --require-secret %q (want namespace/name)", ref)
			continue
		}
		s, err := env.Kube.Get(ctx, GVRSecret.Path(ns, name))
		switch {
		case kube.IsNotFound(err):
			missing = append(missing, ref)
		case err != nil:
			res.Fail("secrets", "reading %s: %v", ref, err)
		case len(s.Map("data")) == 0:
			missing = append(missing, ref+" (exists but is empty)")
		default:
			present = append(present, ref)
		}
	}
	if len(missing) > 0 {
		res.Fail("secrets", "missing: %s — `roksbnkargoctl install` writes these out of band; they are never in Git", strings.Join(missing, ", "))
	} else if len(present) > 0 {
		res.Pass("secrets", "present: %s", strings.Join(present, ", "))
	}
}

func isDefaultClass(sc kube.Object) bool {
	a := sc.Annotations()
	return a["storageclass.kubernetes.io/is-default-class"] == "true" || a["storageclass.beta.kubernetes.io/is-default-class"] == "true"
}

// checkStorageClass: a default class must exist (BNK's DSSM, controller and
// downloader claim volumes), and a named --storage-class must exist.
//
// There is deliberately NO ReadWriteMany requirement for multiple TMM replicas.
// An earlier version failed tmm_replicas > 1 on VPC block storage, believing the
// replicas share one tmm-pvc. roksbnkctl#197 measured otherwise on live 2.4
// clusters: at deploymentSize Tiny TMM mounts no PVC at all (3 and 9 replicas
// Running on block storage, one per node). Tiny is the only size ROKS can run
// (#203: larger sizes need hugepages, and ROKS has no Machine Config Operator to
// allocate them), and the only size this tool renders — so the gate blocked
// F5's reference 3-replica install on a stock cluster for no reason.
func checkStorageClass(ctx context.Context, env *Env, cfg PreInstallConfig, res *Result) {
	scs, err := env.Kube.List(ctx, GVRStorageClass.Path("", ""), kube.ListOptions{})
	if err != nil {
		res.Fail("storageclass", "listing StorageClasses: %v", err)
		return
	}
	var def []kube.Object
	var named kube.Object
	for _, sc := range scs {
		if isDefaultClass(sc) {
			def = append(def, sc)
		}
		if cfg.StorageClass != "" && sc.Name() == cfg.StorageClass {
			named = sc
		}
	}
	if len(def) == 0 {
		res.Fail("storageclass", "no default StorageClass (annotation storageclass.kubernetes.io/is-default-class=true)")
	} else {
		res.Pass("storageclass", "default StorageClass %s (%s)", def[0].Name(), def[0].String("provisioner"))
	}
	if cfg.StorageClass != "" && named == nil {
		res.Fail("storageclass", "StorageClass %q (--storage-class) does not exist", cfg.StorageClass)
		return
	}
	if cfg.TMMReplicas > 1 {
		res.Info("tmm-storage", "%d TMM replicas: at deploymentSize Tiny TMM mounts no PVC, so any StorageClass works (roksbnkctl#197)", cfg.TMMReplicas)
	}
}

// probeClockSkew tolerates this pod's clock running ahead of the API server's.
// Stale pods are excluded exactly by UID, so the tolerance cannot readmit them.
const probeClockSkew = 30 * time.Second

func waitNodeProbes(ctx context.Context, env *Env, cfg PreInstallConfig, res *Result) error {
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	poll := cfg.PollInterval
	if poll <= 0 {
		poll = 5 * time.Second
	}
	// Server timestamps have one-second resolution; the UID exclusion below is
	// the exact guard, this one only has to not reject a new pod.
	start := time.Now().Truncate(time.Second).Add(-probeClockSkew)
	deadline := time.Now().Add(timeout)
	old, err := restartProbePods(ctx, env, cfg.ProbeNamespace, cfg.ProbeDaemonSet)
	if err != nil {
		res.Fail("node-probe", "restarting the probe pods so their verdict is fresh: %v", err)
		return nil
	}
	env.logf("restarted %d node-probe pod(s); waiting up to %s for every %s/%s pod to report (run %s)", len(old), timeout, cfg.ProbeNamespace, cfg.ProbeDaemonSet, orNone(cfg.RunID))
	var v probeView
	var lastErr error
	for {
		v, lastErr = collectProbeReports(ctx, env, cfg.ProbeNamespace, cfg.ProbeDaemonSet, cfg.RunID, old, start)
		if lastErr == nil && v.complete() {
			break
		}
		if !time.Now().Before(deadline) {
			break
		}
		if err := sleep(ctx, poll); err != nil {
			break
		}
	}
	for _, l := range ProbeSummary(v.reports) {
		env.logf("  %s", l)
	}
	res.Set("probeReports", v.reports)
	fails := ProbeFailures(v.reports)
	if len(fails) > 0 {
		res.Fail("node-probe", "unreachable from %d node/target pair(s): %s. dns=FAILED is a resolver problem; tcp failed is routing (transit gateway, security groups, overlapping prefixes); a per-zone split means a subnet or security group fixed in only some zones",
			len(fails), strings.Join(fails, "; "))
	}
	for _, b := range v.badJSON {
		res.Fail("node-probe", "unreadable probe result: %s", b)
	}
	if lastErr != nil {
		res.Fail("node-probe", "%v", lastErr)
		return nil
	}
	if !v.complete() {
		res.Fail("node-probe", "only %d of %d node(s) reported within %s; a node that never reports is indistinguishable from one that would have failed. Waiting on: %s",
			len(v.reports), v.desired, timeout, joinMax(v.pending, 10))
		return nil
	}
	if len(fails) == 0 {
		res.Pass("node-probe", "%d node(s) reached every target: %s", len(v.reports), strings.Join(ProbeSummary(v.reports), "; "))
	}
	return nil
}
