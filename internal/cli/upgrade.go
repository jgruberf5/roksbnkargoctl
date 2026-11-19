package cli

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/jgruberf5/roksbnkargoctl/internal/config"
	"github.com/jgruberf5/roksbnkargoctl/internal/kube"
	"github.com/jgruberf5/roksbnkargoctl/internal/render"
)

// Upgrades within the BNK 2.4 line.
//
// Measured live (2.4.1 -> 2.4.2, one namespace, run 93 on bnkargo): changing
// the CNEManifest and the CNEInstance's manifestVersion is all FLO needs. It
// rolled crd-installer, fluentd, the TMM pod manager, f5ingress, coremond and
// qkview, then TMM one pod at a time; the CNEInstance's Available condition was
// False during the roll and every *Available condition True 3m38s after the
// sync started; the License stayed Active throughout. Two things a plain
// install does not do: the old CNEManifest (bnk-<old>) was left behind and the
// Application OutOfSync, because install syncs without prune; and the sync
// finishing says nothing about FLO having finished. upgrade prunes, and waits
// on the CNEInstance and the License.
//
// What is running is read from the cluster (the CNEInstance's
// spec.manifestVersion), not from the workspace: a workspace can be edited, an
// install made before this record existed has none, and the cluster is what
// would be rolled.

func newUpgradeCmd() *cobra.Command {
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:   "upgrade",
		Short: "Upgrade the running BNK, in place, to this build's 2.4 release",
		Long: `upgrade moves a running install to the BNK release this binary installs
(` + "`roksbnkargoctl version`" + ` names it), within the same 2.4 line: it sets
bnk.version, renders, publishes to Git and syncs the Application with prune.
FLO then rolls every component to the new release; upgrade waits until the
CNEInstance reports the new manifestVersion with every *Available condition
True and the License is Active.

It refuses when nothing is installed (use install), when the running release
is already this one, when it is newer (no downgrades), or when it is another
BNK line. In mirror mode, replicate the new release first:
` + "`roksbnkargoctl registry replicate`" + ` with this binary.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := newSession(cmd)
			if err != nil {
				return err
			}
			if !confirm(cmd, fmt.Sprintf("Upgrade BNK on %s to %s (Application %s)?", s.cfg.Cluster, config.BNKVersion, s.cfg.ArgoCD.Application)) {
				return errors.New("not confirmed (pass --yes to skip the prompt)")
			}
			if s.cfg.BNK.Version != config.BNKVersion {
				from := s.cfg.BNK.Version
				s.update(func(c *config.Config) { c.BNK.Version = config.BNKVersion })
				s.p.info("bnk.version %s -> %s", from, config.BNKVersion)
			}
			return runInstall(cmd.Context(), s, installOpts{timeout: timeout, upgrade: true})
		},
	}
	cmd.Flags().DurationVar(&timeout, "timeout", 75*time.Minute, "how long to wait for the sync, and then for FLO to finish rolling")
	return cmd
}

// parseRelease reads "2.4.3" as [2 4 3].
func parseRelease(v string) ([3]int, error) {
	var out [3]int
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return out, fmt.Errorf("%q is not a BNK release like 2.4.3", v)
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return out, fmt.Errorf("%q is not a BNK release like 2.4.3", v)
		}
		out[i] = n
	}
	return out, nil
}

// checkRelease decides whether install (upgrade=false) or upgrade may proceed,
// given the release running in the cluster ("" when none) and the one this
// build installs.
func checkRelease(running, target string, upgrade bool) error {
	if running == "" {
		if upgrade {
			return errors.New("BNK is not installed on this cluster: run `roksbnkargoctl install`")
		}
		return nil
	}
	if running == target {
		if upgrade {
			return fmt.Errorf("BNK %s is already running: nothing to upgrade", running)
		}
		return nil
	}
	r, err := parseRelease(running)
	if err != nil {
		return fmt.Errorf("the running CNEInstance's manifestVersion: %w", err)
	}
	t, err := parseRelease(target)
	if err != nil {
		return err
	}
	switch {
	case r[0] != t[0] || r[1] != t[1]:
		return fmt.Errorf("BNK %s is running and this build installs %s: moving between BNK lines is not supported (uninstall, then install)", running, target)
	case r[2] > t[2]:
		return fmt.Errorf("BNK %s is running and this build installs the older %s: downgrades are not supported; use a build for %s", running, target, running)
	case !upgrade:
		return fmt.Errorf("BNK %s is running and this build installs %s: install would upgrade it; run `roksbnkargoctl upgrade` to do that deliberately", running, target)
	}
	return nil
}

// runningRelease is the CNEInstance's spec.manifestVersion, or "" when there is
// no CNEInstance.
func runningRelease(ctx context.Context, k *kube.Client, c *config.Config) (string, error) {
	u, err := k.Get(ctx, "k8s.f5.com/v1", "CNEInstance", c.BNK.Namespace, render.CNEInstanceName(c.BNK.Namespace))
	if kube.IsNotFound(err) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("reading the CNEInstance: %w", err)
	}
	v, _, _ := unstructuredString(u.Object, "spec", "manifestVersion")
	return v, nil
}

// upgradeDone reports whether FLO has finished rolling to target: the
// CNEInstance asks for target and every *Available condition is True (Available
// itself among them), and the License is Active. The string says what is still
// pending.
func upgradeDone(cne, license map[string]any, target string) (bool, string) {
	if cne == nil {
		return false, "no CNEInstance"
	}
	if v, _, _ := unstructuredString(cne, "spec", "manifestVersion"); v != target {
		return false, fmt.Sprintf("CNEInstance manifestVersion %q, want %q", v, target)
	}
	var pending []string
	sawAvailable := false
	conds, _ := nested(cne, "status", "conditions").([]any)
	for _, x := range conds {
		m, _ := x.(map[string]any)
		t, _ := m["type"].(string)
		if !strings.HasSuffix(t, "Available") {
			continue
		}
		if t == "Available" {
			sawAvailable = true
		}
		if st, _ := m["status"].(string); st != "True" {
			pending = append(pending, t)
		}
	}
	if !sawAvailable {
		pending = append(pending, "Available (not reported yet)")
	}
	if len(pending) > 0 {
		sort.Strings(pending)
		return false, "waiting for " + strings.Join(pending, ", ")
	}
	if license == nil {
		return false, "no License"
	}
	if st, _, _ := unstructuredString(license, "status", "state"); st != "Active" {
		return false, fmt.Sprintf("License state %q", st)
	}
	return true, "every component Available, License Active"
}

// waitUpgraded polls the CNEInstance and License until upgradeDone.
func waitUpgraded(ctx context.Context, s *session, k *kube.Client, target string, timeout time.Duration) error {
	c, p := s.cfg, s.p
	p.step("waiting for FLO to roll every component to %s", target)
	deadline := time.Now().Add(timeout)
	last := ""
	for {
		var cne, lic map[string]any
		if u, err := k.Get(ctx, "k8s.f5.com/v1", "CNEInstance", c.BNK.Namespace, render.CNEInstanceName(c.BNK.Namespace)); err == nil {
			cne = u.Object
		}
		if u, err := k.Get(ctx, "k8s.f5net.com/v1", "License", c.BNK.UtilsNamespace, render.LicenseName); err == nil {
			lic = u.Object
		}
		done, msg := upgradeDone(cne, lic, target)
		if done {
			p.ok("BNK %s: %s", target, msg)
			return nil
		}
		if msg != last {
			p.info("%s", msg)
			last = msg
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("BNK has not finished rolling to %s after %s: %s (`roksbnkargoctl status` and `diagnose` show why)", target, timeout, msg)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Second):
		}
	}
}

func nested(m map[string]any, path ...string) any {
	var cur any = m
	for _, k := range path {
		mm, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = mm[k]
	}
	return cur
}

func unstructuredString(m map[string]any, path ...string) (string, bool, error) {
	v := nested(m, path...)
	if v == nil {
		return "", false, nil
	}
	s, ok := v.(string)
	if !ok {
		return "", true, fmt.Errorf("%s is not a string", strings.Join(path, "."))
	}
	return s, true, nil
}
