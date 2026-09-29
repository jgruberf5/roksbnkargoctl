package checks

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/jgruberf5/roksbnkargoctl/cmd/check/internal/kube"
)

// PostInstallConfig configures post-install.
type PostInstallConfig struct {
	BNKNamespace   string
	UtilsNamespace string
	LicenseName    string // bnk-license
	TMMReplicas    int    // expected Ready TMM pods; 0 = at least one
	TMMSelector    string // app=f5-tmm
	TMMNamespace   string // "" = BNKNamespace
}

// Waiting reasons that mean a pod will not come up on its own.
var badWaitingReasons = map[string]bool{
	"ImagePullBackOff":           true,
	"ErrImagePull":               true,
	"CrashLoopBackOff":           true,
	"InvalidImageName":           true,
	"CreateContainerConfigError": true,
}

// PostInstall verifies the installed product end to end and reports.
func PostInstall(ctx context.Context, env *Env, cfg PostInstallConfig, res *Result) error {
	if cfg.LicenseName == "" {
		cfg.LicenseName = "bnk-license"
	}
	if cfg.TMMSelector == "" {
		cfg.TMMSelector = "app=f5-tmm"
	}
	if cfg.TMMNamespace == "" {
		cfg.TMMNamespace = cfg.BNKNamespace
	}

	// FLO.
	flos, err := findFLO(ctx, env, cfg.BNKNamespace)
	switch {
	case err != nil:
		res.Fail("flo", "looking for FLO: %v", err)
	case len(flos) == 0:
		res.Fail("flo", "no F5 Lifecycle Operator Deployment found (label %s, or name containing f5-lifecycle-operator in %s)", FLOLabelSelector, cfg.BNKNamespace)
	default:
		for _, d := range flos {
			want := d.Int("spec", "replicas")
			if want == 0 && d.Nested("spec", "replicas") == nil {
				want = 1
			}
			avail := d.Int("status", "availableReplicas")
			if d.ConditionTrue("Available") && avail >= want && avail > 0 {
				res.Pass("flo", "Deployment %s available (%d/%d)", d.Ref(), avail, want)
			} else {
				res.Fail("flo", "Deployment %s not available (%d/%d available)", d.Ref(), avail, want)
			}
		}
	}

	// CNEInstance.
	cne := CNEInstanceName(cfg.BNKNamespace)
	if o, err := env.Kube.Get(ctx, GVRCNEInstance.Path(cfg.BNKNamespace, cne)); err != nil {
		res.Fail("cneinstance", "reading CNEInstance %s/%s: %v", cfg.BNKNamespace, cne, err)
	} else if o.ConditionTrue("Available") {
		res.Pass("cneinstance", "%s/%s Available", cfg.BNKNamespace, cne)
	} else {
		st, reason, msg, _ := o.Condition("Available")
		res.Fail("cneinstance", "%s/%s not Available (status=%s reason=%s: %s)", cfg.BNKNamespace, cne, orNone(st), reason, msg)
	}

	// License.
	if o, err := env.Kube.Get(ctx, GVRLicense.Path(cfg.UtilsNamespace, cfg.LicenseName)); err != nil {
		res.Fail("license", "reading License %s/%s: %v", cfg.UtilsNamespace, cfg.LicenseName, err)
	} else if LicenseActive(o) {
		res.Pass("license", "%s/%s Active", cfg.UtilsNamespace, cfg.LicenseName)
	} else {
		res.Fail("license", "%s/%s is %q, not Active", cfg.UtilsNamespace, cfg.LicenseName, o.String("status", "state"))
	}

	checkTMM(ctx, env, cfg, res)
	checkBadPods(ctx, env, dedupe(cfg.BNKNamespace, cfg.UtilsNamespace), res)
	return nil
}

func podReady(p kube.Object) bool {
	return p.String("status", "phase") == "Running" && p.ConditionTrue("Ready") && !p.Deleting()
}

func checkTMM(ctx context.Context, env *Env, cfg PostInstallConfig, res *Result) {
	pods, err := env.Kube.List(ctx, GVRPod.Path(cfg.TMMNamespace, ""), kube.ListOptions{LabelSelector: cfg.TMMSelector})
	if err != nil {
		res.Fail("tmm", "listing TMM pods (%s in %s): %v", cfg.TMMSelector, cfg.TMMNamespace, err)
		return
	}
	nodes, err := env.Kube.List(ctx, GVRNode.Path("", ""), kube.ListOptions{})
	if err != nil {
		res.Fail("tmm", "listing nodes: %v", err)
		return
	}
	zoneOf := map[string]string{}
	for _, n := range nodes {
		zoneOf[n.Name()] = NodeZone(n)
	}
	ready := 0
	zones := map[string]int{}
	var notReady []string
	for _, p := range pods {
		if !podReady(p) {
			notReady = append(notReady, p.Name())
			continue
		}
		ready++
		z := zoneOf[p.String("spec", "nodeName")]
		if z == "" {
			z = "<unknown>"
		}
		zones[z]++
	}
	var zl []string
	for z, c := range zones {
		zl = append(zl, fmt.Sprintf("%s=%d", z, c))
	}
	sort.Strings(zl)
	switch {
	case cfg.TMMReplicas > 0 && ready != cfg.TMMReplicas:
		res.Fail("tmm", "%d TMM pod(s) Ready, %d expected (not ready: %s)", ready, cfg.TMMReplicas, joinMax(notReady, 8))
	case ready == 0:
		res.Fail("tmm", "no Ready TMM pod (%s in %s)", cfg.TMMSelector, cfg.TMMNamespace)
	default:
		res.Pass("tmm", "%d TMM pod(s) Ready", ready)
	}
	switch {
	case ready > 1 && len(zones) < 2:
		res.Fail("tmm-zones", "%d Ready TMM pods all in one zone [%s]: a zone outage takes the data plane down", ready, strings.Join(zl, " "))
	case ready > 1:
		res.Pass("tmm-zones", "TMM spread over %d zones [%s]", len(zones), strings.Join(zl, " "))
	case ready == 1:
		res.Info("tmm-zones", "single TMM replica [%s]; no zone spread to check", strings.Join(zl, " "))
	}
}

// PodProblems lists the containers of pods that are stuck in a state that will
// not resolve itself, as "ns/pod container: reason (message)".
func PodProblems(pods []kube.Object) []string {
	var out []string
	for _, p := range pods {
		for _, key := range []string{"initContainerStatuses", "containerStatuses"} {
			for _, cs := range p.Slice("status", key) {
				m, _ := cs.(map[string]any)
				c := kube.Object(m)
				reason := c.String("state", "waiting", "reason")
				if !badWaitingReasons[reason] {
					continue
				}
				msg := c.String("state", "waiting", "message")
				if len(msg) > 200 {
					msg = msg[:200] + "..."
				}
				out = append(out, fmt.Sprintf("%s %s: %s (%s)", p.Ref(), c.String("name"), reason, msg))
			}
		}
	}
	sort.Strings(out)
	return out
}

func checkBadPods(ctx context.Context, env *Env, nss []string, res *Result) {
	var bad []string
	for _, ns := range nss {
		pods, err := env.Kube.List(ctx, GVRPod.Path(ns, ""), kube.ListOptions{})
		if err != nil {
			res.Fail("pods", "listing pods in %s: %v", ns, err)
			continue
		}
		bad = append(bad, PodProblems(pods)...)
	}
	if len(bad) > 0 {
		res.Fail("pods", "%d container(s) stuck: %s", len(bad), strings.Join(bad, "; "))
		return
	}
	res.Pass("pods", "no ImagePullBackOff/ErrImagePull/CrashLoopBackOff in %s", strings.Join(nss, ", "))
}
