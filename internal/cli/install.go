package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/jgruberf5/roksbnkargoctl/internal/argocd"
	"github.com/jgruberf5/roksbnkargoctl/internal/config"
	"github.com/jgruberf5/roksbnkargoctl/internal/gitpub"
	"github.com/jgruberf5/roksbnkargoctl/internal/ibm"
	"github.com/jgruberf5/roksbnkargoctl/internal/kube"
	"github.com/jgruberf5/roksbnkargoctl/internal/render"
)

// MinArgoCD is the oldest Argo CD with PreDelete hooks, which the uninstall
// checks depend on.
const MinArgoCD = "3.3.0"

func newInstallCmd() *cobra.Command {
	var noSync bool
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:   "install",
		Short: "Publish the install to Git, create the Argo CD Application and sync it",
		Long: `install is idempotent; re-run it after fixing whatever stopped it.

  1. checks Argo CD is ≥ 3.3 (PreDelete hooks) and the token works
  2. attaches the cluster VPC to the transit gateway, if it is not attached
  3. creates the IAM trusted profile BNK's CNE controller uses (VPC + cluster scope)
  4. renders every manifest (see ` + "`render`" + `)
  5. writes the out-of-band objects into ROKS: Secrets (FAR or mirror pull
     secret, subscription JWT, FLP CA), the check namespace and its RBAC
  6. registers ROKS with Argo CD (a cluster-admin ServiceAccount token)
  7. publishes manifests/git/ to the Git repo
  8. creates the Application and, unless --no-sync, syncs it and waits

The sync runs the pre-install check in ROKS first; nothing of BNK is applied if
it fails.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := newSession(cmd)
			if err != nil {
				return err
			}
			return runInstall(cmd.Context(), s, noSync, timeout)
		},
	}
	cmd.Flags().BoolVar(&noSync, "no-sync", false, "create the Application but do not sync it (sync from the Argo CD UI)")
	cmd.Flags().DurationVar(&timeout, "timeout", 75*time.Minute, "how long to wait for the sync")
	return cmd
}

func runInstall(ctx context.Context, s *session, noSync bool, timeout time.Duration) error {
	c, p := s.cfg, s.p
	if err := c.Validate(); err != nil {
		return err
	}
	r, err := s.resolved()
	if err != nil {
		return err
	}
	gitToken, sshKey, err := gitCredentials(c)
	if err != nil {
		return err
	}

	// 1. Argo CD.
	ac, err := s.ArgoCD()
	if err != nil {
		return err
	}
	if err := ac.RequireAtLeast(ctx, MinArgoCD); err != nil {
		return fmt.Errorf("checking the Argo CD at %s: %w", c.ArgoCD.Server, err)
	}
	v, _ := ac.Version(ctx)
	p.ok("Argo CD %s at %s", v, c.ArgoCD.Server)

	ibmc, err := s.IBM()
	if err != nil {
		return err
	}

	// 2. Transit gateway attachment.
	if err := ensureClusterOnTGW(ctx, s, ibmc); err != nil {
		return err
	}

	// 3. Trusted profile.
	if err := ensureTrustedProfile(ctx, s, ibmc); err != nil {
		return err
	}
	if err := s.save(); err != nil {
		return err
	}

	// 4. Render.
	o, err := renderAll(ctx, s, true)
	if err != nil {
		return err
	}
	if err := render.Write(o, s.ws.ManifestsDir()); err != nil {
		return err
	}
	p.ok("rendered %d Git objects, %d direct objects", len(o.Git), len(o.Direct))

	// 5. Direct objects.
	k, err := s.Kube(ctx)
	if err != nil {
		return err
	}
	p.step("writing %d out-of-band objects into ROKS (Secrets, check namespace + RBAC)", len(o.Direct))
	for _, obj := range orderDirect(o.Direct) {
		if err := k.Apply(ctx, obj); err != nil {
			return err
		}
	}
	p.ok("out-of-band objects applied")

	// 6. Register ROKS with Argo CD.
	p.step("registering %s with Argo CD (%s endpoint %s)", r.ClusterName, c.ArgoCD.ClusterEndpoint, r.ArgoCDClusterServer)
	token, clusterCA, err := k.EnsureArgoCDManager(ctx)
	if err != nil {
		return err
	}
	if _, err := ac.UpsertCluster(ctx, argocd.Cluster{
		Name: r.ClusterName, Server: r.ArgoCDClusterServer, BearerToken: token,
		CAData: registrationCA(c.ArgoCD.ClusterEndpoint, clusterCA),
		Labels: map[string]string{render.LabelManagedBy: render.ManagedByValue},
	}); err != nil {
		return fmt.Errorf("registering the cluster with Argo CD: %w", err)
	}
	p.ok("cluster registered")

	// 7. Git.
	p.step("publishing manifests/git to %s (%s:%s)", c.Git.URL, c.Git.Branch, c.Git.Path)
	sha, changed, err := gitpub.Publish(ctx, gitpub.Options{
		URL: c.Git.URL, Branch: c.Git.Branch, Path: c.Git.Path,
		Username: c.Git.Username, Token: gitToken, SSHKeyPEM: sshKey, InsecureIgnoreHostKey: os.Getenv("ROKSBNKARGOCTL_GIT_INSECURE_HOSTKEY") == "1",
		AuthorName: c.Git.AuthorName, AuthorEmail: c.Git.AuthorEmail,
		Message: fmt.Sprintf("roksbnkargoctl: BNK %s on %s (%s mode, %s registry)", c.BNK.Version, r.ClusterName, c.BNK.Mode, c.Registry.Source),
		SrcDir:  s.ws.ManifestsDir() + "/git",
	})
	if err != nil {
		return fmt.Errorf("publishing to Git: %w", err)
	}
	r.LastPublishedCommitSHA = sha
	_ = s.save()
	if changed {
		p.ok("published commit %s", short(sha))
	} else {
		p.ok("Git already up to date at %s", short(sha))
	}

	// 8. Repository + Application.
	repo := argocd.Repo{URL: c.Git.URL, Username: c.Git.Username, Password: gitToken, SSHPrivateKey: string(sshKey), Project: c.ArgoCD.Project}
	if _, err := ac.UpsertRepository(ctx, repo); err != nil {
		return fmt.Errorf("adding the Git repo to Argo CD: %w", err)
	}
	app, err := toArgoApp(o.Application)
	if err != nil {
		return err
	}
	if _, err := ac.UpsertApplication(ctx, app); err != nil {
		return fmt.Errorf("creating the Application: %w", err)
	}
	p.ok("Application %s created in project %s", c.ArgoCD.Application, c.ArgoCD.Project)
	if noSync {
		p.info("sync it from the Argo CD UI, or run `roksbnkargoctl install` without --no-sync")
		return nil
	}
	return syncAndWait(ctx, s, ac, sha, timeout)
}

func syncAndWait(ctx context.Context, s *session, ac *argocd.Client, revision string, timeout time.Duration) error {
	c, p := s.cfg, s.p
	p.step("syncing %s (pre-install check → cert-manager → FLO → CNEInstance → license)", c.ArgoCD.Application)
	if _, err := ac.Sync(ctx, c.ArgoCD.Application, argocd.SyncOptions{Revision: revision}); err != nil {
		return fmt.Errorf("sync: %w", err)
	}
	last := ""
	res, err := ac.WaitOperation(ctx, c.ArgoCD.Application, timeout, func(a *argocd.Application) {
		msg := progressLine(a)
		if msg != last {
			p.info("%s", msg)
			last = msg
		}
	})
	if err != nil {
		p.warn("sync did not succeed — `roksbnkargoctl status` shows the check results, `roksbnkargoctl diagnose` collects logs")
		return err
	}
	p.ok("sync %s: %s", res.Phase, res.Message)
	return nil
}

func progressLine(a *argocd.Application) string {
	st := a.Status
	phase, msg := "", ""
	if st.OperationState != nil {
		phase, msg = st.OperationState.Phase, st.OperationState.Message
	}
	if len(msg) > 140 {
		msg = msg[:140] + "…"
	}
	return fmt.Sprintf("[%s] sync=%s health=%s  %s", time.Now().Format("15:04:05"), st.Sync.Status, st.Health.Status, strings.TrimSpace(phase+" "+msg))
}

func short(sha string) string {
	if len(sha) > 10 {
		return sha[:10]
	}
	return sha
}

// orderDirect applies namespaces first, then RBAC, then everything else.
func orderDirect(objs []render.Object) []map[string]any {
	rank := func(k string) int {
		switch k {
		case "Namespace":
			return 0
		case "ServiceAccount", "ClusterRole", "ClusterRoleBinding":
			return 1
		}
		return 2
	}
	var out []map[string]any
	for r := 0; r <= 2; r++ {
		for _, o := range objs {
			if rank(o.Kind()) == r {
				out = append(out, map[string]any(o))
			}
		}
	}
	return out
}

func toArgoApp(o render.Object) (*argocd.Application, error) {
	b, err := json.Marshal(map[string]any(o))
	if err != nil {
		return nil, err
	}
	var app argocd.Application
	if err := json.Unmarshal(b, &app); err != nil {
		return nil, err
	}
	return &app, nil
}

func gitCredentials(c *config.Config) (token string, sshKey []byte, err error) {
	if c.Git.SSHKeyFile != "" {
		b, err := os.ReadFile(c.Git.SSHKeyFile)
		if err != nil {
			return "", nil, fmt.Errorf("git.ssh_key_file: %w", err)
		}
		return "", b, nil
	}
	t, err := config.Env(c.Git.TokenEnv, "Git token")
	return t, nil, err
}

// registrationCA is the CA Argo CD verifies the ROKS API with. The two ROKS
// endpoints differ, verified live: the public endpoint presents a publicly
// trusted certificate (no CA: Argo CD's system roots verify it, and a cluster CA
// would REPLACE those roots and fail), while the private endpoint's certificate
// chains to the cluster's own root CA — the same CA as the ServiceAccount token
// Secret's ca.crt. Without it: "x509: certificate signed by unknown authority".
func registrationCA(endpoint string, clusterCA []byte) []byte {
	if endpoint == "private" {
		return clusterCA
	}
	return nil
}

// ensureClusterOnTGW attaches the cluster VPC to the transit gateway when it is
// not already, after checking no attached VPC overlaps its prefixes (a gateway
// silently blackholes one of two overlapping VPCs).
func ensureClusterOnTGW(ctx context.Context, s *session, ibmc *ibm.Client) error {
	r, p := s.cfg.Resolved, s.p
	conn, err := ibmc.FindConnectionForVPC(ctx, r.TransitGatewayID, r.VPCCRN)
	if err == nil && conn != nil {
		if conn.Status != "attached" {
			if _, err := ibmc.WaitConnectionAttached(ctx, r.TransitGatewayID, conn.ID, 10*time.Minute); err != nil {
				return err
			}
		}
		r.ClusterAttachedToTGW = true
		p.ok("cluster VPC attached to transit gateway %s", r.TransitGatewayName)
		return nil
	}
	p.step("attaching cluster VPC %s to transit gateway %s", r.VPCName, r.TransitGatewayName)
	prefixes, err := ibmc.ListAddressPrefixes(ctx, r.VPCID)
	if err != nil {
		return err
	}
	var mine []string
	for _, x := range prefixes {
		mine = append(mine, x.CIDR)
	}
	attached, err := ibmc.GatewayAttachedPrefixes(ctx, r.TransitGatewayID)
	if err != nil {
		return err
	}
	if conflicts, err := ibm.FindPrefixConflicts(mine, attached); err != nil {
		return err
	} else if len(conflicts) > 0 {
		var ss []string
		for _, c := range conflicts {
			ss = append(ss, c.String())
		}
		return fmt.Errorf("refusing to attach: the cluster VPC overlaps VPCs already on %s:\n  %s", r.TransitGatewayName, strings.Join(ss, "\n  "))
	}
	nc, err := ibmc.CreateVPCConnection(ctx, r.TransitGatewayID, r.ClusterName, r.VPCCRN)
	if err != nil {
		return err
	}
	if _, err := ibmc.WaitConnectionAttached(ctx, r.TransitGatewayID, nc.ID, 10*time.Minute); err != nil {
		return err
	}
	r.ClusterAttachedToTGW = true
	r.TGWConnectionCreatedID = nc.ID
	p.ok("attached (connection %s)", nc.ID)
	return nil
}

// trustedProfileName is the profile BNK's CNE controller assumes. Named as
// roksbnkctl names it, so a cluster moved between the tools keeps one profile.
func trustedProfileName(c *config.Config) string {
	return c.Resolved.ClusterName + "-f5-cne-controller-" + c.BNK.Namespace
}

// ensureTrustedProfile creates the IAM trusted profile the 2.4 CNE controller
// uses to program VPC routes: linked to ServiceAccount <ns>/f5-cne-controller in
// this cluster, Viewer+Editor on the cluster's VPC and Viewer on the cluster.
func ensureTrustedProfile(ctx context.Context, s *session, ibmc *ibm.Client) error {
	c, p := s.cfg, s.p
	r := c.Resolved
	name := trustedProfileName(c)
	tp, err := ibmc.FindTrustedProfileByName(ctx, name)
	if err != nil && !ibm.IsNotFound(err) {
		return err
	}
	if tp == nil {
		p.step("creating IAM trusted profile %s", name)
		if tp, err = ibmc.CreateTrustedProfile(ctx, name, "BNK CNE controller for ROKS cluster "+r.ClusterName+" (roksbnkargoctl)"); err != nil {
			return err
		}
	}
	r.TrustedProfileID = tp.ID
	links, err := ibmc.ListProfileLinks(ctx, tp.ID)
	if err != nil {
		return err
	}
	linked := false
	for _, l := range links {
		if l.Link.Namespace == c.BNK.Namespace && l.Link.Name == render.CNEControllerSA && strings.Contains(l.Link.CRN, r.ClusterID) {
			linked = true
		}
	}
	if !linked {
		if _, err := ibmc.LinkROKSServiceAccount(ctx, tp.ID, r.ClusterCRN, c.BNK.Namespace, render.CNEControllerSA, "f5-cne-controller-roks-link"); err != nil {
			return fmt.Errorf("linking the trusted profile to %s/%s: %w", c.BNK.Namespace, render.CNEControllerSA, err)
		}
	}
	pols, err := ibmc.ListProfilePolicies(ctx, tp.ID)
	if err != nil {
		return err
	}
	if len(pols) < 2 {
		if _, err := ibmc.CreatePolicy(ctx, tp.ID, []string{"Viewer", "Editor"}, []ibm.PolicyAttribute{
			{Name: "serviceName", Value: "is"}, {Name: "vpcId", Value: r.VPCID}}); err != nil && !ibm.IsConflict(err) {
			return fmt.Errorf("trusted profile VPC policy: %w", err)
		}
		if _, err := ibmc.CreatePolicy(ctx, tp.ID, []string{"Viewer"}, []ibm.PolicyAttribute{
			{Name: "serviceName", Value: "containers-kubernetes"}, {Name: "serviceInstance", Value: r.ClusterID}}); err != nil && !ibm.IsConflict(err) {
			return fmt.Errorf("trusted profile cluster policy: %w", err)
		}
	}
	p.ok("trusted profile %s (%s)", name, tp.ID)
	return nil
}

// ---- uninstall ------------------------------------------------------------------

func newUninstallCmd() *cobra.Command {
	var timeout time.Duration
	var purgeGit, keepProfile, detachTGW, removeRepo bool
	cmd := &cobra.Command{
		Use:   "uninstall",
		Short: "Delete the Application; the uninstall checks drain BNK in ROKS",
		Long: `uninstall deletes the Argo CD Application with foreground cascading. Argo CD
runs the PreDelete check in ROKS (drains F5 resources while FLO still runs,
CNEInstance last), deletes everything it synced, then the PostDelete check
(license secrets, namespaces, stuck F5 finalizers). Then uninstall removes what
install wrote out of band: the Secrets, the check namespace and RBAC, the
Argo CD cluster registration and its ServiceAccount, and the trusted profile.

F5 CRDs stay (deleting a CRD deletes every CR and can hang namespaces).
The Git history stays unless --purge-git.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := newSession(cmd)
			if err != nil {
				return err
			}
			if !confirm(cmd, fmt.Sprintf("Uninstall BNK from %s (Application %s)?", s.cfg.Cluster, s.cfg.ArgoCD.Application)) {
				return errors.New("not confirmed (pass --yes to skip the prompt)")
			}
			return runUninstall(cmd.Context(), s, uninstallOpts{timeout, purgeGit, keepProfile, detachTGW, removeRepo})
		},
	}
	cmd.Flags().DurationVar(&timeout, "timeout", 45*time.Minute, "how long to wait for Argo CD to finish deleting")
	cmd.Flags().BoolVar(&purgeGit, "purge-git", false, "also remove the manifests from the Git repo")
	cmd.Flags().BoolVar(&keepProfile, "keep-trusted-profile", false, "keep the IAM trusted profile")
	cmd.Flags().BoolVar(&detachTGW, "detach-tgw", false, "detach the cluster VPC from the transit gateway if install attached it")
	cmd.Flags().BoolVar(&removeRepo, "remove-repo", false, "remove the Git repo credential from Argo CD (other Applications may use it)")
	return cmd
}

type uninstallOpts struct {
	timeout                                      time.Duration
	purgeGit, keepProfile, detachTGW, removeRepo bool
}

func runUninstall(ctx context.Context, s *session, o uninstallOpts) error {
	c, p := s.cfg, s.p
	r, err := s.resolved()
	if err != nil {
		return err
	}
	ac, err := s.ArgoCD()
	if err != nil {
		return err
	}
	if _, err := ac.GetApplication(ctx, c.ArgoCD.Application, ""); err == nil {
		p.step("deleting Application %s (PreDelete check → prune → PostDelete check)", c.ArgoCD.Application)
		if err := ac.Delete(ctx, c.ArgoCD.Application, true, "foreground"); err != nil {
			return err
		}
		last := ""
		if err := ac.WaitGone(ctx, c.ArgoCD.Application, o.timeout, func(a *argocd.Application) {
			if m := progressLine(a); m != last {
				p.info("%s", m)
				last = m
			}
		}); err != nil {
			p.warn("the Application has not finished deleting — `roksbnkargoctl diagnose` collects the uninstall check logs")
			return err
		}
		p.ok("Application deleted")
	} else if argocd.IsNotFound(err) {
		p.info("Application %s does not exist; cleaning up what install wrote out of band", c.ArgoCD.Application)
	} else {
		return err
	}

	k, err := s.Kube(ctx)
	if err != nil {
		return err
	}
	var errs []error
	// Out-of-band objects, in reverse: Secrets, then RBAC, then the namespace.
	p.step("removing out-of-band objects")
	for _, obj := range directObjectsOnDisk(s) {
		if obj["kind"] == "Namespace" && obj["metadata"].(map[string]any)["name"] != render.CheckNamespace {
			continue // BNK namespaces are the PostDelete check's to delete
		}
		if err := k.Delete(ctx, obj); err != nil {
			errs = append(errs, err)
		}
	}
	if err := k.RemoveArgoCDManager(ctx); err != nil {
		errs = append(errs, err)
	}
	if err := ac.DeleteCluster(ctx, r.ArgoCDClusterServer); err != nil && !argocd.IsNotFound(err) {
		errs = append(errs, fmt.Errorf("removing the Argo CD cluster registration: %w", err))
	}
	if o.removeRepo {
		if err := ac.DeleteRepository(ctx, c.Git.URL); err != nil && !argocd.IsNotFound(err) {
			errs = append(errs, err)
		}
	}
	ibmc, err := s.IBM()
	if err != nil {
		return err
	}
	if !o.keepProfile && r.TrustedProfileID != "" {
		if err := ibmc.DeleteTrustedProfile(ctx, r.TrustedProfileID); err != nil && !ibm.IsNotFound(err) {
			errs = append(errs, fmt.Errorf("deleting trusted profile: %w", err))
		} else {
			r.TrustedProfileID = ""
		}
	}
	if o.detachTGW && r.TGWConnectionCreatedID != "" {
		if err := ibmc.DeleteConnection(ctx, r.TransitGatewayID, r.TGWConnectionCreatedID, 10*time.Minute); err != nil {
			errs = append(errs, err)
		} else {
			r.TGWConnectionCreatedID, r.ClusterAttachedToTGW = "", false
		}
	}
	if o.purgeGit {
		tok, key, err := gitCredentials(c)
		if err == nil {
			_, _, err = gitpub.Remove(ctx, gitpub.Options{URL: c.Git.URL, Branch: c.Git.Branch, Path: c.Git.Path,
				Username: c.Git.Username, Token: tok, SSHKeyPEM: key, AuthorName: c.Git.AuthorName, AuthorEmail: c.Git.AuthorEmail,
				Message: "roksbnkargoctl: remove BNK from " + r.ClusterName})
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("purging Git: %w", err))
		}
	}
	_ = s.save()
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	p.ok("uninstalled")
	return nil
}

// directObjectsOnDisk re-reads what install applied out of band. Secret values
// are redacted on disk, which is fine: deleting needs only kind and name.
func directObjectsOnDisk(s *session) []map[string]any {
	objs, _ := readYAMLDir(s.ws.ManifestsDir() + "/direct")
	// Delete in reverse of apply order.
	ordered := orderDirect(objs)
	for i, j := 0, len(ordered)-1; i < j; i, j = i+1, j-1 {
		ordered[i], ordered[j] = ordered[j], ordered[i]
	}
	return ordered
}

var _ = kube.FieldManager
