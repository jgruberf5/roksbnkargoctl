package checks

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jgruberf5/roksbnkargoctl/cmd/check/internal/kube"
)

// PostUninstallConfig configures post-uninstall.
type PostUninstallConfig struct {
	BNKNamespace   string
	UtilsNamespace string
	// DeleteNamespaces are extra namespaces to delete (e.g. cert-manager). The
	// BNK namespaces are always deleted: they carry Argo CD's Delete=false so the
	// prune never hangs on them, which makes deleting them this hook's job.
	DeleteNamespaces []string
	// FinalizerGrace is how long a namespace may sit Terminating on its own
	// before F5 finalizers are stripped: FLO's pods may still be finishing.
	FinalizerGrace time.Duration // 30s
	Timeout        time.Duration // 5m
	Poll           time.Duration // 3s
}

// Cluster-scoped leftovers reported (not deleted: shared, and CRDs are kept by design).
var reportClusterIssuers = []string{"sample-issuer", "selfsigned-cluster-issuer"}

// PostUninstall removes what the prune leaves behind and makes sure the BNK
// namespaces actually go.
func PostUninstall(ctx context.Context, env *Env, cfg PostUninstallConfig, res *Result) error {
	setDur(&cfg.FinalizerGrace, 30*time.Second)
	setDur(&cfg.Timeout, 5*time.Minute)
	setDur(&cfg.Poll, 3*time.Second)
	start := time.Now()

	// 1. The CWC license secrets, by exact name (never a prefix sweep).
	if cfg.UtilsNamespace != "" {
		deleted, failed := 0, []string{}
		for _, name := range LicenseSecretNames {
			did, err := env.Kube.DeleteIfExists(ctx, GVRSecret.Path(cfg.UtilsNamespace, name), "")
			if err != nil {
				failed = append(failed, name+": "+err.Error())
				continue
			}
			if did {
				deleted++
			}
		}
		if len(failed) > 0 {
			res.Fail("license-secrets", "could not delete %d CWC secret(s) in %s: %s", len(failed), cfg.UtilsNamespace, joinMax(failed, 5))
		} else {
			res.Pass("license-secrets", "deleted %d of %d CWC license secret(s) in %s (absent ones are fine)", deleted, len(LicenseSecretNames), cfg.UtilsNamespace)
		}
	}

	// 2. Delete the namespaces.
	nss := dedupe(append([]string{cfg.BNKNamespace, cfg.UtilsNamespace}, cfg.DeleteNamespaces...)...)
	for _, ns := range nss {
		did, err := env.Kube.DeleteIfExists(ctx, GVRNamespace.Path("", ns), "Background")
		switch {
		case err != nil:
			res.Fail("namespaces", "deleting namespace %s: %v", ns, err)
		case did:
			env.logf("deleting namespace %s", ns)
		}
	}

	// 3. Wait for them to go, stripping F5 finalizers once the grace has passed.
	// Only F5 namespaces are ever stripped: an extra namespace like cert-manager
	// is waited for, but its finalizers are not ours to remove.
	f5ns := map[string]bool{cfg.BNKNamespace: true, cfg.UtilsNamespace: true}
	deadline := time.Now().Add(cfg.Timeout)
	stripped := 0
	var left []string
	for {
		left = left[:0]
		for _, ns := range nss {
			n, err := env.Kube.Get(ctx, GVRNamespace.Path("", ns))
			if kube.IsNotFound(err) {
				continue
			}
			if err != nil {
				left = append(left, ns+" (unreadable: "+err.Error()+")")
				continue
			}
			left = append(left, ns)
			if f5ns[ns] && n.String("status", "phase") == "Terminating" && time.Since(start) >= cfg.FinalizerGrace {
				stripped += StripF5Finalizers(ctx, env, ns, namespaceMentionsF5Finalizer(n))
			}
		}
		if len(left) == 0 || !time.Now().Before(deadline) {
			break
		}
		if sleep(ctx, cfg.Poll) != nil {
			break
		}
	}
	if stripped > 0 {
		res.Warn("finalizers", "stripped F5 finalizers from %d object(s) whose operator was already gone", stripped)
	}
	if len(left) > 0 {
		var why []string
		for _, ns := range left {
			if n, err := env.Kube.Get(ctx, GVRNamespace.Path("", strings.Fields(ns)[0])); err == nil {
				for _, t := range []string{"NamespaceFinalizersRemaining", "NamespaceContentRemaining"} {
					if st, _, msg, ok := n.Condition(t); ok && st == "True" {
						why = append(why, ns+": "+msg)
					}
				}
			}
		}
		res.Fail("namespaces", "still present after %s: %s. %s", cfg.Timeout, strings.Join(left, ", "), strings.Join(why, "; "))
	} else {
		res.Pass("namespaces", "%s gone", strings.Join(nss, ", "))
	}

	reportLeftovers(ctx, env, res)
	return nil
}

// namespaceMentionsF5Finalizer: the namespace controller names the finalizers
// holding it; when one is F5's but lives on a non-F5 kind, the wide scan runs.
func namespaceMentionsF5Finalizer(n kube.Object) bool {
	_, _, msg, ok := n.Condition("NamespaceFinalizersRemaining")
	if !ok {
		return false
	}
	for _, f := range strings.FieldsFunc(msg, func(r rune) bool { return r == ' ' || r == ',' || r == ':' }) {
		if IsF5Finalizer(f) {
			return true
		}
	}
	return false
}

// StripF5Finalizers removes F5 finalizers from objects in ns and reports how
// many objects it changed.
//
// Only F5's entries are removed; the rest are written back (roksbnkctl's first
// version wrote `finalizers: null` and dropped everyone's). The patch carries
// resourceVersion so a concurrent writer wins and this retries next poll rather
// than clobbering it. wide scans every namespaced type, not just F5's groups.
func StripF5Finalizers(ctx context.Context, env *Env, ns string, wide bool) int {
	gvrs, _ := env.Kube.DiscoverGroups(ctx, F5Groups)
	if wide {
		all, _ := discoverAll(ctx, env)
		gvrs = append(gvrs, all...)
	}
	seen := map[string]bool{}
	changed := 0
	for _, g := range gvrs {
		if !g.Namespaced || seen[g.String()] {
			continue
		}
		seen[g.String()] = true
		items, err := env.Kube.List(ctx, g.Path(ns, ""), kube.ListOptions{})
		if err != nil {
			continue
		}
		for _, it := range items {
			keep, removed := RetainNonF5Finalizers(it.Finalizers())
			if removed == 0 {
				continue
			}
			patch := map[string]any{"metadata": map[string]any{"finalizers": keep, "resourceVersion": it.ResourceVersion()}}
			if _, err := env.Kube.Patch(ctx, g.Path(ns, it.Name()), kube.MergePatch, patch); err != nil {
				env.logf("  could not clear finalizers on %s/%s: %v", g.Resource, it.Name(), err)
				continue
			}
			changed++
			env.logf("  cleared F5 finalizer(s) on %s %s/%s (kept %v)", g.String(), ns, it.Name(), keep)
		}
	}
	return changed
}

// discoverAll resolves every listable resource the server serves.
func discoverAll(ctx context.Context, env *Env) ([]kube.GVR, error) {
	var groups struct {
		Groups []struct {
			Name string `json:"name"`
		} `json:"groups"`
	}
	if err := env.Kube.Do(ctx, "GET", "/apis", nil, "", nil, &groups); err != nil {
		return nil, err
	}
	var names []string
	for _, g := range groups.Groups {
		names = append(names, g.Name)
	}
	out, _ := env.Kube.DiscoverGroups(ctx, names)
	core, err := env.Kube.Resources(ctx, "", "v1")
	if err == nil {
		for _, r := range core {
			if r.HasVerb("list") {
				out = append(out, kube.GVR{Version: "v1", Resource: r.Name, Namespaced: r.Namespaced, Kind: r.Kind})
			}
		}
	}
	return out, nil
}

// reportLeftovers lists what survives by design, as info.
func reportLeftovers(ctx context.Context, env *Env, res *Result) {
	crds, err := env.Kube.List(ctx, GVRCRD.Path("", ""), kube.ListOptions{})
	if err == nil {
		want := map[string]bool{}
		for _, g := range F5Groups {
			want[g] = true
		}
		var f5 []string
		for _, c := range crds {
			if want[c.String("spec", "group")] {
				f5 = append(f5, c.Name())
			}
		}
		sort.Strings(f5)
		res.Set("f5CRDs", f5)
		if len(f5) > 0 {
			res.Info("crds", "%d F5 CRD(s) kept by design (cluster-scoped, shared by any BNK on the cluster): %s", len(f5), joinMax(f5, 8))
		}
	}
	gvrs, _ := env.Kube.DiscoverGroups(ctx, F5Groups)
	var objs []string
	for _, g := range gvrs {
		if g.Namespaced {
			continue
		}
		items, err := env.Kube.List(ctx, g.Path("", ""), kube.ListOptions{})
		if err != nil {
			continue
		}
		for _, it := range items {
			objs = append(objs, fmt.Sprintf("%s/%s", g.Kind, it.Name()))
		}
	}
	for _, n := range reportClusterIssuers {
		if ok, _ := env.Kube.Exists(ctx, GVRClusterIss.Path("", n)); ok {
			objs = append(objs, "ClusterIssuer/"+n)
		}
	}
	sort.Strings(objs)
	res.Set("clusterLeftovers", objs)
	if len(objs) > 0 {
		res.Info("cluster-objects", "cluster-scoped objects left: %s", strings.Join(objs, ", "))
	}
}
