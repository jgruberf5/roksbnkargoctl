package checks

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jgruberf5/roksbnkargoctl/cmd/check/internal/kube"
)

// DefaultDrainGroups are the groups pre-uninstall deletes from before the
// CNEInstance, on BNK 2.4.
//
// NOT every F5 group, on evidence (roksbnkctl #266, drainGroupsForLine):
//   - k8s.f5.com holds the CNEInstance and the components FLO derives from it
//     (cnecontrollers, f5tmms, dssms, afms, downloaders, cnemanifests). FLO
//     re-creates each within ~5s while the CNEInstance exists, so draining them
//     never finishes; they go with the CNEInstance.
//   - k8s.f5net.com on 2.4 is product defaults the webhook refuses to delete
//     (f5-big-tcp-settings/sys-default-tcp) or re-creates on sight
//     (f5-big-dns-caches/security-fqdn-dns-resolver), plus the License, which is
//     deleted explicitly. Draining it was what made roksbnkctl's drain time out.
//   - metrics.f5.com is left to the namespace delete, as roksbnkctl does.
//
// The guide's order — use-case CRs, GatewaySettings, Infra, License, VERIFY IPAM
// gone, CNEInstance — is covered by gateway.k8s.f5.com + fic.f5.com, then the
// License, then the CNEInstance.
var DefaultDrainGroups = []string{"gateway.k8s.f5.com", "fic.f5.com"}

// PreUninstallConfig configures pre-uninstall.
type PreUninstallConfig struct {
	BNKNamespace    string
	UtilsNamespace  string
	DrainGroups     []string
	DrainTimeout    time.Duration // per namespace, 4m
	Poll            time.Duration // 3s
	RefusalGrace    time.Duration // 45s
	WebhookInterval time.Duration // 3s
	LicenseTimeout  time.Duration // 3m
	CNETimeout      time.Duration // 10m
}

// WebhookSweeper removes F5's validating webhooks.
//
// FLO re-creates f5validate-<ns> about ten seconds into a teardown, and its
// backend is already going, so every delete it guards is refused with "failed
// calling webhook". Measured: FLO restores it 375-815ms after removal, so it is
// swept on a timer AND on demand by a refused delete (Neutralise), which turns
// the race into a sequence (roksbnkctl #241).
type WebhookSweeper struct {
	Env        *Env
	Namespaces []string
	mu         sync.Mutex
	removed    int
	errLogged  bool
}

// isF5Webhook: named f5validate-*, or served from a BNK namespace.
func (w *WebhookSweeper) isF5Webhook(o kube.Object) bool {
	if strings.HasPrefix(o.Name(), "f5validate-") {
		return true
	}
	for _, wh := range o.Slice("webhooks") {
		m, _ := wh.(map[string]any)
		ns := kube.Object(m).String("clientConfig", "service", "namespace")
		for _, n := range w.Namespaces {
			if ns != "" && strings.EqualFold(ns, n) {
				return true
			}
		}
	}
	return false
}

// Neutralise deletes every F5 webhook once and reports how many it removed.
func (w *WebhookSweeper) Neutralise(ctx context.Context) int {
	list, err := w.Env.Kube.List(ctx, GVRVWC.Path("", ""), kube.ListOptions{})
	if err != nil {
		w.logOnce("listing validating webhooks: %v", err)
		return 0
	}
	n := 0
	for _, o := range list {
		if !w.isF5Webhook(o) {
			continue
		}
		did, err := w.Env.Kube.DeleteIfExists(ctx, GVRVWC.Path("", o.Name()), "")
		if err != nil {
			w.logOnce("deleting webhook %s: %v", o.Name(), err)
			continue
		}
		if did {
			n++
		}
	}
	w.mu.Lock()
	w.removed += n
	w.mu.Unlock()
	return n
}

func (w *WebhookSweeper) logOnce(format string, args ...any) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.errLogged {
		w.errLogged = true
		w.Env.logf(format, args...)
	}
}

// Removed is the running total.
func (w *WebhookSweeper) Removed() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.removed
}

// Start sweeps every interval until the returned stop is called.
func (w *WebhookSweeper) Start(ctx context.Context, interval time.Duration) (stop func()) {
	sctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	w.Neutralise(sctx)
	go func() {
		defer close(done)
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-sctx.Done():
				return
			case <-t.C:
				w.Neutralise(sctx)
			}
		}
	}()
	return func() { cancel(); <-done }
}

// drainer deletes objects and waits for them to go, retrying refused deletes.
type drainer struct {
	env         *Env
	neutralise  func(context.Context) int
	poll, grace time.Duration
	logged      map[string]bool
	denied      map[string]bool
	deletedUID  map[string]string
	deleteOrder []string // "resource/ns/name" in the order deletes were accepted
	skipOwned   bool
	propagation string
}

func newDrainer(env *Env, neutralise func(context.Context) int, poll, grace time.Duration) *drainer {
	return &drainer{env: env, neutralise: neutralise, poll: poll, grace: grace,
		logged: map[string]bool{}, denied: map[string]bool{}, deletedUID: map[string]string{}, skipOwned: true}
}

type pass struct{ accepted, present, refused, marked, unknown int }

func (d *drainer) logOnce(key, format string, args ...any) {
	if !d.logged[key] {
		d.logged[key] = true
		d.env.logf(format, args...)
	}
}

// once issues a delete for every object not already being deleted.
func (d *drainer) once(ctx context.Context, gvrs []kube.GVR, ns string) pass {
	var p pass
	for _, g := range gvrs {
		items, err := d.env.Kube.List(ctx, g.Path(ns, ""), kube.ListOptions{})
		if err != nil {
			// A type whose CRD is gone lists 404 — that IS drained. Anything else
			// is unknown, never "zero objects": an unreachable API server must
			// not read as a successful drain.
			if !kube.IsNotFound(err) {
				p.unknown++
			}
			continue
		}
		for _, it := range items {
			key := g.Resource + "/" + ns + "/" + it.Name()
			// DO NOT DELETE WHAT AN OWNER WILL DELETE: a child deleted under a
			// live owner is reconciled back, a fight the drain cannot win.
			if d.skipOwned && len(it.OwnerReferences()) > 0 {
				d.logOnce("owned:"+key, "  %s is owned by %s; it goes with its owner", key, strings.Join(it.OwnerReferences(), ","))
				continue
			}
			if d.denied[key] {
				continue
			}
			// Re-created by its controller after we deleted it (new UID): it
			// cannot be drained; leave it to the namespace delete.
			if prev, seen := d.deletedUID[key]; seen && prev != it.UID() {
				d.logOnce("remade:"+key, "  %s was re-created by its controller after deletion; leaving it to the namespace delete", key)
				continue
			}
			p.present++
			if it.Deleting() {
				p.marked++
				continue
			}
			err := d.env.Kube.Delete(ctx, g.Path(ns, it.Name()), d.propagation)
			// A REFUSAL IS THE SIGNAL TO ACT: the error proves the webhook is
			// back, so remove it and retry at once, inside the window before FLO
			// restores it. Retry regardless of whether the neutraliser found
			// anything — the timer sweep may have beaten it by milliseconds.
			if err != nil && !kube.IsNotFound(err) && WebhookRefusal(err) && d.neutralise != nil {
				d.neutralise(ctx)
				err = d.env.Kube.Delete(ctx, g.Path(ns, it.Name()), d.propagation)
			}
			switch {
			case err == nil:
				p.accepted++
				d.deletedUID[key] = it.UID()
				d.deleteOrder = append(d.deleteOrder, key)
				d.logOnce("del:"+key, "  deleted %s", key)
			case kube.IsNotFound(err):
				p.present--
			case WebhookDenial(err):
				// The product states a rule ("cannot be deleted!"); forcing it
				// would delete what BNK says must not be. It goes with the namespace.
				p.present--
				d.denied[key] = true
				d.logOnce("denied:"+key, "  %s cannot be deleted (the product's webhook forbids it); leaving it to the namespace delete", key)
			default:
				p.refused++
				d.logOnce("refused:"+key, "  could not delete %s: %v", key, err)
			}
		}
	}
	return p
}

// remaining names what is still present (same exclusions as once).
func (d *drainer) remaining(ctx context.Context, gvrs []kube.GVR, ns string) []string {
	var out []string
	for _, g := range gvrs {
		items, err := d.env.Kube.List(ctx, g.Path(ns, ""), kube.ListOptions{})
		if err != nil {
			if !kube.IsNotFound(err) {
				out = append(out, g.String()+" in "+ns+" (unlistable: "+err.Error()+")")
			}
			continue
		}
		for _, it := range items {
			key := g.Resource + "/" + ns + "/" + it.Name()
			if d.denied[key] || (d.skipOwned && len(it.OwnerReferences()) > 0) {
				continue
			}
			if prev, seen := d.deletedUID[key]; seen && prev != it.UID() {
				continue
			}
			out = append(out, key)
		}
	}
	sort.Strings(out)
	return out
}

// drain deletes and waits until nothing is left or the deadline passes,
// RE-ISSUING deletes every poll (roksbnkctl #241: a delete refused once and
// never retried waited out its whole budget for nothing). It gives up early
// only when every delete has been refused, with nothing finalizing, for the
// whole refusal grace — FLO's webhook re-creation window is ~34s, so one
// refused pass proves nothing.
func (d *drainer) drain(ctx context.Context, gvrs []kube.GVR, ns string, deadline time.Time) []string {
	var refusingSince time.Time
	for {
		p := d.once(ctx, gvrs, ns)
		if p.present <= 0 && p.unknown == 0 {
			return nil
		}
		if p.accepted == 0 && p.marked == 0 && p.refused > 0 && p.unknown == 0 {
			if refusingSince.IsZero() {
				refusingSince = time.Now()
				d.env.logf("  every delete in %s is being refused; retrying for up to %s in case it is the webhook being re-created", ns, d.grace)
			}
			if time.Since(refusingSince) >= d.grace {
				d.env.logf("  every delete in %s was refused for %s; waiting cannot help", ns, d.grace)
				return d.remaining(ctx, gvrs, ns)
			}
		} else {
			refusingSince = time.Time{}
		}
		if !time.Now().Before(deadline) {
			return d.remaining(ctx, gvrs, ns)
		}
		if sleep(ctx, d.poll) != nil {
			return d.remaining(ctx, gvrs, ns)
		}
	}
}

// SplitDrain returns the resources drained before the CNEInstance, and the
// CNEInstance resource itself.
func SplitDrain(gvrs []kube.GVR, groups []string) (leaves []kube.GVR, cne *kube.GVR) {
	want := map[string]bool{}
	for _, g := range groups {
		want[g] = true
	}
	for i, g := range gvrs {
		if !g.Namespaced || !g.CanDelete {
			continue
		}
		if g.Group == GVRCNEInstance.Group && g.Resource == GVRCNEInstance.Resource {
			cne = &gvrs[i]
			continue
		}
		// k8s.f5.com components are FLO-managed, and the License has its own step.
		if g.Group == "k8s.f5.com" || (g.Group == GVRLicense.Group && g.Resource == GVRLicense.Resource) {
			continue
		}
		if want[g.Group] {
			leaves = append(leaves, g)
		}
	}
	return leaves, cne
}

// PreUninstall drains BNK while FLO is still running to finalize it.
//
// THE OPERATOR MUST OUTLIVE THE OBJECTS IT HAS TO FINALIZE (roksbnkctl #217).
// Deleting FLO and the CNEInstance together leaves the CNEInstance's children
// with F5 finalizers nothing can clear, and the namespace hangs Terminating.
// This runs as the PreDelete hook, before Argo CD prunes FLO.
func PreUninstall(ctx context.Context, env *Env, cfg PreUninstallConfig, res *Result) error {
	if len(cfg.DrainGroups) == 0 {
		cfg.DrainGroups = DefaultDrainGroups
	}
	setDur(&cfg.DrainTimeout, 4*time.Minute)
	setDur(&cfg.Poll, 3*time.Second)
	setDur(&cfg.RefusalGrace, 45*time.Second)
	setDur(&cfg.WebhookInterval, 3*time.Second)
	setDur(&cfg.LicenseTimeout, 3*time.Minute)
	setDur(&cfg.CNETimeout, 10*time.Minute)
	nss := dedupe(cfg.BNKNamespace, cfg.UtilsNamespace)

	if flos, err := findFLO(ctx, env, cfg.BNKNamespace); err == nil {
		running := false
		for _, d := range flos {
			if d.Int("status", "availableReplicas") > 0 {
				running = true
			}
		}
		if running {
			res.Info("flo", "FLO is running and can finalize what is deleted")
		} else {
			res.Warn("flo", "no available FLO: F5 finalizers may not clear; post-uninstall strips them if the namespaces stick")
		}
	}

	sw := &WebhookSweeper{Env: env, Namespaces: nss}
	stop := sw.Start(ctx, cfg.WebhookInterval)
	defer func() {
		stop()
		res.Set("webhooksRemoved", sw.Removed())
		if n := sw.Removed(); n > 0 {
			res.Info("webhooks", "removed F5 validating webhooks %d time(s) (FLO re-creates them during teardown)", n)
		}
	}()

	gvrs, derrs := env.Kube.DiscoverGroups(ctx, F5Groups)
	for _, e := range derrs {
		res.Warn("discovery", "%v", e)
	}
	leaves, cne := SplitDrain(gvrs, cfg.DrainGroups)
	res.Info("discovery", "draining %d resource type(s) before the CNEInstance: %s", len(leaves), gvrNames(leaves))
	d := newDrainer(env, sw.Neutralise, cfg.Poll, cfg.RefusalGrace)

	// 1. Everything in the drain groups, per namespace, each with its own budget.
	blocked := false
	for _, ns := range nss {
		left := d.drain(ctx, leaves, ns, time.Now().Add(cfg.DrainTimeout))
		if len(left) > 0 {
			blocked = true
			res.Fail("drain", "%d F5 object(s) in %s did not finalize within %s: %s", len(left), ns, cfg.DrainTimeout, joinMax(left, 12))
		} else {
			res.Pass("drain", "F5 custom resources in %s drained", ns)
		}
	}
	// 2. IPAM/IPAMRange must be CONFIRMED gone before the CNEInstance: they are
	// controller-generated, so removing the CNEInstance first removes the thing
	// that would clean them up, and the leftovers break a reinstall.
	var ipam []kube.GVR
	for _, g := range gvrs {
		if g.Group == "fic.f5.com" {
			ipam = append(ipam, g)
		}
	}
	// Counted WITH owned objects: an IPAM owned by an Infra is exactly the
	// controller-generated kind the guide means, and it goes by garbage
	// collection, so it is waited for rather than deleted.
	verify := newDrainer(env, nil, cfg.Poll, cfg.RefusalGrace)
	verify.skipOwned = false
	var ipamLeft []string
	ipamDeadline := time.Now().Add(cfg.DrainTimeout)
	for {
		ipamLeft = ipamLeft[:0]
		for _, ns := range nss {
			ipamLeft = append(ipamLeft, verify.remaining(ctx, ipam, ns)...)
		}
		if len(ipamLeft) == 0 || !time.Now().Before(ipamDeadline) || sleep(ctx, cfg.Poll) != nil {
			break
		}
	}
	if len(ipamLeft) > 0 {
		blocked = true
		res.Fail("ipam", "fic.f5.com objects remain: %s", joinMax(ipamLeft, 12))
	} else {
		res.Pass("ipam", "no fic.f5.com IPAM/IPAMRange objects remain")
	}
	if blocked {
		// The CNEInstance is deliberately NOT deleted: it would orphan exactly the
		// objects still waiting. Failing the PreDelete hook stops Argo CD from
		// pruning FLO; fix what is named above and delete the Application again.
		res.Fail("cneinstance", "not deleted, because the objects above have not gone; FLO is left running to finalize them")
		return nil
	}

	// 3. The License (its k8s.f5net.com/uninstall finalizer needs CWC alive).
	lic := GVRLicense
	lic.CanDelete, lic.Namespaced = true, true
	ld := newDrainer(env, sw.Neutralise, cfg.Poll, cfg.RefusalGrace)
	ld.skipOwned = false // deleted explicitly whatever owns it
	var licLeft []string
	for _, ns := range nss {
		licLeft = append(licLeft, ld.drain(ctx, []kube.GVR{lic}, ns, time.Now().Add(cfg.LicenseTimeout))...)
	}
	if len(licLeft) > 0 {
		res.Warn("license", "License not gone within %s: %s; continuing (post-uninstall strips its finalizer)", cfg.LicenseTimeout, strings.Join(licLeft, ", "))
	} else {
		res.Pass("license", "License deleted")
	}

	// 4. LAST: the CNEInstance, waited for while FLO still runs.
	if cne == nil {
		res.Info("cneinstance", "no CNEInstance CRD served; nothing to delete")
		return nil
	}
	cd := newDrainer(env, sw.Neutralise, cfg.Poll, cfg.RefusalGrace)
	cd.skipOwned = false
	left := cd.drain(ctx, []kube.GVR{*cne}, cfg.BNKNamespace, time.Now().Add(cfg.CNETimeout))
	if len(left) > 0 {
		res.Fail("cneinstance", "%s not gone within %s; FLO did not finish finalizing it", strings.Join(left, ", "), cfg.CNETimeout)
		return nil
	}
	res.Pass("cneinstance", "CNEInstance deleted and finalized while FLO was running")
	return nil
}

func setDur(d *time.Duration, def time.Duration) {
	if *d <= 0 {
		*d = def
	}
}

func gvrNames(gs []kube.GVR) string {
	if len(gs) == 0 {
		return "(none)"
	}
	var s []string
	for _, g := range gs {
		s = append(s, g.String())
	}
	return strings.Join(s, ", ")
}
