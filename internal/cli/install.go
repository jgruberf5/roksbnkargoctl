package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"golang.org/x/crypto/ssh"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"sigs.k8s.io/yaml"

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
	var noSync, noPublish bool
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
  7. publishes manifests/git/ to the Git repo (skipped with --no-publish)
  8. creates the Application and, unless --no-sync, syncs it and waits

--no-publish is for a repository roksbnkargoctl should not push to: commit the
output of ` + "`roksbnkargoctl export`" + ` yourself first. install then compares what the
Application would sync (Argo CD reads git.url, git.branch, git.path) with this
workspace's render, refuses to sync if they differ, and otherwise syncs exactly
the revision it compared. With no Git token or SSH key set, Argo CD reads the
repository with the credential already registered in it, or anonymously, and step
1 asks Argo CD, not your machine, whether it can read git.branch.

The sync runs the pre-install check in ROKS first; nothing of BNK is applied if
it fails.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := newSession(cmd)
			if err != nil {
				return err
			}
			return runInstall(cmd.Context(), s, noSync, noPublish, timeout)
		},
	}
	cmd.Flags().BoolVar(&noSync, "no-sync", false, "create the Application but do not sync it (sync from the Argo CD UI)")
	cmd.Flags().BoolVar(&noPublish, "no-publish", false, "do not push to Git: sync what you committed from `export`, after checking it matches this render")
	cmd.Flags().DurationVar(&timeout, "timeout", 75*time.Minute, "how long to wait for the sync")
	return cmd
}

func runInstall(ctx context.Context, s *session, noSync, noPublish bool, timeout time.Duration) error {
	c, p := s.cfg, s.p
	if err := c.Validate(); err != nil {
		return err
	}
	r, err := s.resolved()
	if err != nil {
		return err
	}
	if err := checkLayout(r, c); err != nil {
		return err
	}
	gitOpts, err := gitOptions(c, p.warn)
	if err != nil && !(noPublish && missingGitCredential(err)) {
		return err
	}

	// 1. Argo CD.
	ac, err := s.ArgoCD()
	if err != nil {
		return err
	}
	if err := ac.CheckServer(ctx, MinArgoCD); err != nil {
		return fmt.Errorf("checking the Argo CD at %s: %w", c.ArgoCD.Server, err)
	}
	v, _ := ac.Version(ctx)
	p.ok("Argo CD %s at %s", v, c.ArgoCD.Server)
	// Git access, before anything changes (#18): push rights for a normal
	// install, read access and the branch for --no-publish.
	if noPublish && gitOpts.Token == "" && len(gitOpts.SSHKeyPEM) == 0 {
		// No local credential: Argo CD reads the repository with its own
		// registration, so ask Argo CD rather than reading anonymously from here.
		if err := checkGitThroughArgoCD(ctx, p, c, ac); err != nil {
			return err
		}
	} else if err := checkGitAccess(ctx, p, c, gitOpts, !noPublish, noPublish); err != nil {
		return err
	}

	ibmc, err := s.IBM()
	if err != nil {
		return err
	}

	// Before the first change in the cloud, a workspace with nothing installed
	// records the install as pending: a first install that fails from here on
	// (and leaves a trusted profile) is not then mistaken for one made before
	// 0.7.0, whose record is a profile and no layout. Pending locks nothing;
	// the layout itself is recorded once BNK's objects are applied (step 5).
	if !layoutInstalled(r.InstalledLayout) {
		r.InstalledLayout = layoutPending + installLayout(c)
		if err := s.save(); err != nil {
			return err
		}
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
	p.ok("rendered %d Git objects, %d Helm charts, %d direct objects", len(o.Git), len(o.Charts), len(o.Direct))

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
	if r.InstalledLayout != installLayout(c) {
		r.InstalledLayout = installLayout(c)
		if err := s.save(); err != nil {
			return err
		}
	}

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
	var sha string
	if noPublish {
		p.info("--no-publish: not pushing to %s; the Application syncs what is at %s:%s after a check", c.Git.URL, c.Git.Branch, c.Git.Path)
	} else {
		p.step("publishing manifests/git to %s (%s:%s)", c.Git.URL, c.Git.Branch, c.Git.Path)
		pubOpts := gitOpts
		pubOpts.Message = fmt.Sprintf("roksbnkargoctl: BNK %s on %s (%s mode, %s registry)", c.BNK.Version, r.ClusterName, c.BNK.Mode, c.Registry.Source)
		pubOpts.SrcDir = s.ws.ManifestsDir() + "/git"
		var changed bool
		sha, changed, err = gitpub.Publish(ctx, pubOpts)
		if err != nil {
			return fmt.Errorf("publishing to Git: %w", err)
		}
		r.LastPublishedCommitSHA = sha
		if err := s.save(); err != nil {
			p.warn("%v", err)
		}
		if changed {
			p.ok("published commit %s", short(sha))
		} else {
			p.ok("Git already up to date at %s", short(sha))
		}
	}

	// 8. Repository + Application.
	if gitOpts.SSHKnownHosts != "" {
		n, skipped, err := ac.UpsertSSHKnownHosts(ctx, gitOpts.SSHKnownHosts)
		if err != nil {
			return fmt.Errorf("adding git.known_hosts_file to Argo CD: %w", err)
		}
		p.ok("added %d SSH known-host key(s) to Argo CD", n)
		for _, sk := range skipped {
			p.warn("not added to Argo CD (add it there by hand): %s", sk)
		}
	}
	if err := registerRepo(p, c, gitOpts, noPublish, func(r argocd.Repo) error {
		_, err := ac.UpsertRepository(ctx, r)
		return err
	}); err != nil {
		return err
	}
	host, user, pass, err := registryLogin(ctx, s)
	if err != nil {
		return err
	}
	mirrorCA, err := s.MirrorCA()
	if err != nil {
		return err
	}
	if err := registerChartSources(ctx, p, c, ac, o.Charts, registryAccess{host, user, pass, mirrorCA}); err != nil {
		return err
	}
	app, err := toArgoApp(o.Application)
	if err != nil {
		return err
	}
	// Argo CD's upsert REPLACES metadata.finalizers. The controller adds its
	// pre-delete/post-delete finalizers itself once it sees the hooks; sending
	// only ours on a re-install would strip them, and a delete before the next
	// reconcile would then skip the uninstall checks.
	if cur, err := ac.GetApplication(ctx, app.Metadata.Name, ""); err == nil {
		app.Metadata.Finalizers = mergeFinalizers(cur.Metadata.Finalizers, app.Metadata.Finalizers)
	} else if !argocd.IsNotFound(err) {
		return fmt.Errorf("reading the Application: %w", err)
	}
	if _, err := ac.UpsertApplication(ctx, app); err != nil {
		return fmt.Errorf("creating the Application: %w", err)
	}
	p.ok("Application %s created in project %s", c.ArgoCD.Application, c.ArgoCD.Project)
	gitPos, err := gitSourcePosition(o.Application)
	if err != nil {
		return err
	}
	if sha, err = revisionToSync(noPublish, sha, func() (string, error) { return checkGitMatchesRender(ctx, s, ac, gitPos) }); err != nil {
		return err
	}
	// Only now, with the Application on a revision that no longer reads them:
	// before, a refused --no-publish compare left Argo CD on the old revision,
	// whose hooks require the Secret just deleted.
	for _, obj := range o.Stale {
		if err := k.Delete(ctx, obj); err != nil {
			return err
		}
	}
	if noSync {
		p.info("sync it from the Argo CD UI, or run `roksbnkargoctl install` without --no-sync")
		return nil
	}
	return syncAndWait(ctx, s, ac, sha, gitPos, timeout)
}

// chartRepoAPI is the part of the Argo CD client the chart registration uses.
type chartRepoAPI interface {
	UpsertRepository(ctx context.Context, r argocd.Repo) (*argocd.RepoInfo, error)
	UpsertTLSCert(ctx context.Context, serverName, pemData string) error
	DeleteRepository(ctx context.Context, repoURL string) error
}

// registryAccess is how the chart registries are reached: the login (host,
// user, password) and a private mirror's CA.
type registryAccess struct {
	host, user, pass, mirrorCA string
}

// registerChartSources makes the charts' registries pullable by Argo CD: a
// private mirror's CA as a TLS certificate for its host, and each registry as
// an OCI Helm repository.
func registerChartSources(ctx context.Context, p printer, c *config.Config, ac chartRepoAPI, charts []render.Chart, ra registryAccess) error {
	if ra.mirrorCA != "" && c.Registry.Source == config.SourceMirror {
		h := strings.SplitN(mirrorRegistryHost(c), ":", 2)[0]
		if err := ac.UpsertTLSCert(ctx, h, ra.mirrorCA); err != nil {
			return fmt.Errorf("adding the mirror's CA (registry.mirror.ca_file) to Argo CD: %w", err)
		}
		p.ok("Argo CD trusts the mirror's CA for %s", h)
	}
	for _, r := range chartRepos(c, charts, ra.host, ra.user, ra.pass) {
		if _, err := ac.UpsertRepository(ctx, r); err != nil {
			return fmt.Errorf("adding the chart registry %s to Argo CD: %w", r.URL, err)
		}
		p.ok("Argo CD pulls the %s chart from %s", r.Name, r.URL)
	}
	return nil
}

// removeChartRepos deletes the chart registries' entries (uninstall
// --remove-repo); one already gone is not an error.
func removeChartRepos(ctx context.Context, ac chartRepoAPI, urls []string) []error {
	var errs []error
	for _, u := range urls {
		if err := ac.DeleteRepository(ctx, u); err != nil && !argocd.IsNotFound(err) {
			errs = append(errs, err)
		}
	}
	return errs
}

// chartRepos are the Argo CD repository entries the charts' Helm sources pull
// from: OCI registries, with the registry login where the chart lives on the
// registry it is for (FAR or the mirror), anonymous otherwise (quay.io).
func chartRepos(c *config.Config, charts []render.Chart, loginHost, user, pass string) []argocd.Repo {
	var out []argocd.Repo
	seen := map[string]bool{}
	for _, ch := range charts {
		if seen[ch.RepoURL] {
			continue
		}
		seen[ch.RepoURL] = true
		r := argocd.Repo{URL: ch.RepoURL, HelmOCI: true, Name: ch.Chart, Project: c.ArgoCD.Project}
		if user != "" && strings.SplitN(ch.RepoURL, "/", 2)[0] == loginHost {
			r.Username, r.Password = user, pass
		}
		out = append(out, r)
	}
	return out
}

// chartRepoURLs are the chart registries the workspace's Application pulls
// from, as the last render wrote them into application.yaml.
func chartRepoURLs(s *session) []string {
	b, err := os.ReadFile(filepath.Join(s.ws.ManifestsDir(), "application.yaml"))
	if err != nil {
		return nil
	}
	var app struct {
		Spec struct {
			Sources []struct {
				RepoURL string `json:"repoURL"`
				Chart   string `json:"chart"`
			} `json:"sources"`
		} `json:"spec"`
	}
	if yaml.Unmarshal(b, &app) != nil {
		return nil
	}
	var out []string
	for _, src := range app.Spec.Sources {
		if src.Chart != "" && !slices.Contains(out, src.RepoURL) {
			out = append(out, src.RepoURL)
		}
	}
	return out
}

// registerRepo registers the Git repository in Argo CD with the credential
// install holds. With --no-publish and none, it leaves Argo CD's registration
// alone: an upsert would replace a registration that has a credential with an
// anonymous one.
func registerRepo(p printer, c *config.Config, o gitpub.Options, noPublish bool, upsert func(argocd.Repo) error) error {
	if noPublish && o.Token == "" && len(o.SSHKeyPEM) == 0 {
		p.info("no Git credential set: leaving Argo CD's registration of %s as it is (it reads the repository anonymously if none exists)", c.Git.URL)
		return nil
	}
	repo := argocd.Repo{URL: c.Git.URL, Username: c.Git.Username, Password: o.Token, SSHPrivateKey: string(o.SSHKeyPEM), Project: c.ArgoCD.Project}
	if err := upsert(repo); err != nil {
		return fmt.Errorf("adding the Git repo to Argo CD: %w", err)
	}
	return nil
}

// revisionToSync is the revision install syncs: the commit it pushed, or with
// --no-publish exactly the revision compared with the render, so a push that
// lands between the comparison and the sync is not synced unchecked.
func revisionToSync(noPublish bool, pushed string, compare func() (string, error)) (string, error) {
	if !noPublish {
		return pushed, nil
	}
	return compare()
}

// gitSourcePosition is the 1-based position of the Application's Git source
// (the one that is not a chart), which install pins to the commit it syncs.
func gitSourcePosition(app render.Object) (int, error) {
	spec, _ := app["spec"].(map[string]any)
	srcs, _ := spec["sources"].([]any)
	pos := 0
	for i, x := range srcs {
		if m, ok := x.(map[string]any); ok && m["chart"] == nil {
			if pos != 0 {
				return 0, errors.New("the Application has more than one Git source")
			}
			pos = i + 1
		}
	}
	if pos == 0 {
		return 0, errors.New("the Application has no Git source")
	}
	return pos, nil
}

// syncAndWait syncs the Application with its Git source (position gitPos) at
// revision, the commit published or compared; the charts at their versions.
func syncAndWait(ctx context.Context, s *session, ac *argocd.Client, revision string, gitPos int, timeout time.Duration) error {
	c, p := s.cfg, s.p
	p.step("syncing %s (pre-install check → cert-manager → FLO → CNEInstance → license)", c.ArgoCD.Application)
	if _, err := ac.Sync(ctx, c.ArgoCD.Application, argocd.SyncOptions{Revision: revision, SourcePosition: gitPos}); err != nil {
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

// gitOptions is the one place Git credentials and SSH host-key policy are
// resolved; install's publish and uninstall's purge both start from it, so they
// cannot drift apart (review finding: each call site built its own options and
// no test noticed one dropping the known_hosts).
//
// Host keys: the built-in keys of github.com, gitlab.com and bitbucket.org
// always apply (gitpub); git.known_hosts_file adds hosts and is validated here,
// before anything changes. Checking is switched off only by the explicit,
// loudly-warned ROKSBNKARGOCTL_GIT_INSECURE_HOSTKEY=1 (issue #2).
func gitOptions(c *config.Config, warn func(string, ...any)) (gitpub.Options, error) {
	o := gitpub.Options{URL: c.Git.URL, Branch: c.Git.Branch, Path: c.Git.Path, Username: c.Git.Username,
		AuthorName: c.Git.AuthorName, AuthorEmail: c.Git.AuthorEmail}
	var err error
	if o.Token, o.SSHKeyPEM, err = gitCredentials(c); err != nil {
		return o, err
	}
	if f := c.Git.KnownHostsFile; f != "" {
		b, err := os.ReadFile(f)
		if err != nil {
			return o, fmt.Errorf("git.known_hosts_file: %w", err)
		}
		if err := validKnownHosts(b); err != nil {
			return o, fmt.Errorf("git.known_hosts_file %s: %w", f, err)
		}
		o.SSHKnownHosts = string(b)
	}
	if os.Getenv("ROKSBNKARGOCTL_GIT_INSECURE_HOSTKEY") == "1" {
		warn("ROKSBNKARGOCTL_GIT_INSECURE_HOSTKEY=1: the Git server's SSH host key is NOT verified — anyone on the path can impersonate it. Use git.known_hosts_file instead.")
		o.InsecureIgnoreHostKey = true
	}
	return o, nil
}

// validKnownHosts parses every entry, so a malformed file fails before install
// has changed anything rather than at the Git push.
func validKnownHosts(b []byte) error {
	for rest := b; len(rest) > 0; {
		var err error
		_, _, _, _, rest, err = ssh.ParseKnownHosts(rest)
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
	return nil
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
	if err != nil {
		return "", nil, fmt.Errorf("%w: %w", errNoGitCredential, err)
	}
	return t, nil, nil
}

// errNoGitCredential: neither git.ssh_key_file nor a token in git.token_env.
var errNoGitCredential = errors.New("no Git credential")

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

// tgwAPI is the part of the IBM Cloud client ensureClusterOnTGW uses; tests
// substitute a fake.
type tgwAPI interface {
	FindConnectionForVPC(ctx context.Context, gatewayID, vpcCRN string) (*ibm.TGWConnection, error)
	WaitConnectionAttached(ctx context.Context, gatewayID, connectionID string, timeout time.Duration) (*ibm.TGWConnection, error)
	ListAddressPrefixes(ctx context.Context, vpcID string) ([]ibm.AddressPrefix, error)
	GatewayAttachedPrefixes(ctx context.Context, gatewayID string) (map[string][]string, error)
	CreateVPCConnection(ctx context.Context, gatewayID, name, vpcCRN string) (*ibm.TGWConnection, error)
}

// profileAPI is the part of the IBM Cloud client ensureTrustedProfile uses.
type profileAPI interface {
	FindTrustedProfileByName(ctx context.Context, name string) (*ibm.TrustedProfile, error)
	CreateTrustedProfile(ctx context.Context, name, description string) (*ibm.TrustedProfile, error)
	ListProfileLinks(ctx context.Context, profileID string) ([]ibm.ProfileLink, error)
	LinkROKSServiceAccount(ctx context.Context, profileID, clusterCRN, namespace, saName, linkName string) (string, error)
	ListProfilePolicies(ctx context.Context, profileID string) ([]string, error)
	CreatePolicy(ctx context.Context, profileID string, roles []string, attrs []ibm.PolicyAttribute) (string, error)
}

// ensureClusterOnTGW attaches the cluster VPC to the transit gateway when it is
// not already, after checking no attached VPC overlaps its prefixes (a gateway
// silently blackholes one of two overlapping VPCs).
func ensureClusterOnTGW(ctx context.Context, s *session, ibmc tgwAPI) error {
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
	// Recorded and saved before the wait (#28): if attaching fails or times
	// out, uninstall --detach-tgw and workspaces delete still know about it.
	r.TGWConnectionCreatedID = nc.ID
	if err := s.save(); err != nil {
		return fmt.Errorf("created transit gateway connection %s but could not record it: %w "+
			"(remove it with `ibmcloud tg connection-delete %s %s` if you do not keep it)", nc.ID, err, r.TransitGatewayID, nc.ID)
	}
	if _, err := ibmc.WaitConnectionAttached(ctx, r.TransitGatewayID, nc.ID, 10*time.Minute); err != nil {
		return err
	}
	r.ClusterAttachedToTGW = true
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
func ensureTrustedProfile(ctx context.Context, s *session, ibmc profileAPI) error {
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
	// Recorded and saved before linking and policies (#28): if one of those
	// fails, uninstall and workspaces delete still know about the profile.
	if r.TrustedProfileID != tp.ID {
		r.TrustedProfileID = tp.ID
		if err := s.save(); err != nil {
			return fmt.Errorf("trusted profile %s exists but could not be recorded: %w "+
				"(remove it with `ibmcloud iam trusted-profile-delete %s` if you do not keep it)", tp.ID, err, tp.ID)
		}
	}
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
	var purgeGit, keepProfile, detachTGW, removeRepo, force bool
	cmd := &cobra.Command{
		Use:   "uninstall",
		Short: "Delete the Application; the uninstall checks drain BNK in ROKS",
		Long: `uninstall runs the pre-uninstall check in ROKS (drains F5 resources while
FLO still runs, CNEInstance last), deletes the Argo CD Application with
foreground cascading so Argo CD prunes everything it synced, then runs the
post-uninstall check (license secrets, stuck F5 finalizers, namespaces) and
confirms the BNK namespaces are gone. The checks run as Jobs uninstall waits on:
Argo CD can skip its own PreDelete/PostDelete hooks (argoproj/argo-cd#29100).
Then uninstall removes what install wrote out of band: the Secrets, the check namespace and RBAC, the
Argo CD cluster registration and its ServiceAccount, and the trusted profile.

F5 CRDs stay (deleting a CRD deletes every CR and can hang namespaces).
The Git history stays unless --purge-git. SSH host keys added to Argo CD from
git.known_hosts_file stay too: Argo CD's known-hosts list is shared.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := newSession(cmd)
			if err != nil {
				return err
			}
			if !confirm(cmd, fmt.Sprintf("Uninstall BNK from %s (Application %s)?", s.cfg.Cluster, s.cfg.ArgoCD.Application)) {
				return errors.New("not confirmed (pass --yes to skip the prompt)")
			}
			return runUninstall(cmd.Context(), s, uninstallOpts{timeout, purgeGit, keepProfile, detachTGW, removeRepo, force})
		},
	}
	cmd.Flags().DurationVar(&timeout, "timeout", 45*time.Minute, "how long to wait for Argo CD to finish deleting")
	cmd.Flags().BoolVar(&purgeGit, "purge-git", false, "also remove the manifests from the Git repo")
	cmd.Flags().BoolVar(&keepProfile, "keep-trusted-profile", false, "keep the IAM trusted profile")
	cmd.Flags().BoolVar(&detachTGW, "detach-tgw", false, "detach the cluster VPC from the transit gateway if install attached it")
	cmd.Flags().BoolVar(&removeRepo, "remove-repo", false, "remove the Git repo credential and the chart registries' entries from Argo CD (other Applications may use them)")
	cmd.Flags().BoolVar(&force, "force", false, "delete the Application even when the pre-uninstall check fails or cannot run")
	return cmd
}

type uninstallOpts struct {
	timeout                                             time.Duration
	purgeGit, keepProfile, detachTGW, removeRepo, force bool
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
	// Collect the uninstall checks' logs while they run: a hook pod is deleted
	// with the Application (issue #5).
	k, kerr := s.Kube(ctx)
	if kerr != nil {
		if !o.force {
			return fmt.Errorf("cannot reach the cluster to run the uninstall checks: %w (--force deletes the Application anyway)", kerr)
		}
		p.warn("cannot reach the cluster (%v); deleting the Application without the uninstall checks (--force)", kerr)
	}
	var logs *checkLogCollector
	if kerr == nil {
		if stream, err := k.Streaming(); err == nil {
			logs = newCheckLogCollector(k.Typed, stream, filepath.Join(s.ws.Dir, "diagnostics", "uninstall-"+time.Now().UTC().Format("20060102-150405")))
		}
	}
	steps := uninstallSteps{p: p, force: o.force,
		appExists: func() (bool, error) {
			_, err := ac.GetApplication(ctx, c.ArgoCD.Application, "")
			if argocd.IsNotFound(err) {
				return false, nil
			}
			return err == nil, err
		},
		deleteApp: func(tick func()) error {
			p.step("deleting Application %s (Argo CD prunes what it synced)", c.ArgoCD.Application)
			if err := ac.Delete(ctx, c.ArgoCD.Application, true, "foreground"); err != nil {
				return err
			}
			last := ""
			if err := ac.WaitGone(ctx, c.ArgoCD.Application, o.timeout, func(a *argocd.Application) {
				tick()
				if m := progressLine(a); m != last {
					p.info("%s", m)
					last = m
				}
			}); err != nil {
				p.warn("the Application has not finished deleting — the saved check logs, and `roksbnkargoctl diagnose`, show why")
				return err
			}
			p.ok("Application deleted")
			return nil
		},
	}
	if kerr == nil {
		steps.runCheck = func(mode string) error { return runCheckJob(ctx, s, k.Typed, mode) }
		steps.remaining = func() ([]string, error) { return namespacesRemaining(ctx, k.Typed, bnkNamespaces(c)) }
	}
	uninstall := steps.run
	if logs != nil {
		n, err := collectCheckLogsDuring(ctx, logs, uninstall)
		if n > 0 {
			p.ok("uninstall check logs (%d pods) saved to %s", n, logs.dir)
		}
		if err != nil {
			return err
		}
	} else if err := uninstall(func() {}); err != nil {
		return err
	}
	if kerr != nil {
		return kerr
	}
	r.InstalledLayout = layoutNone // BNK is gone: the next install may choose another layout

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
		errs = append(errs, removeChartRepos(ctx, ac, chartRepoURLs(s))...)
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
		ro, err := gitOptions(c, p.warn)
		if err == nil {
			ro.Message = "roksbnkargoctl: remove BNK from " + r.ClusterName
			_, _, err = gitpub.Remove(ctx, ro)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("purging Git: %w", err))
		}
	}
	if err := s.save(); err != nil {
		p.warn("%v", err)
	}
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

// installLayout names what cannot change under a running install: the
// namespaces and the certificate mode.
func installLayout(c *config.Config) string {
	nss := c.BNK.Namespace
	if c.BNK.UtilsNamespace != c.BNK.Namespace {
		nss += "," + c.BNK.UtilsNamespace
	}
	return "namespaces=" + nss + " certificates=" + c.BNK.Certificates.Mode
}

// checkLayout refuses to install with a different layout than the one BNK is
// installed with: moving to one namespace would delete the utilities
// namespace with CWC, RabbitMQ and the License in it (it did, on roksbnkctl),
// and switching certificate modes swaps every component's certificates under it.
// layoutNone records that uninstall removed BNK; layoutPending prefixes the
// layout of an install that has not yet applied anything of BNK. An empty
// record is a workspace never installed by 0.7.0: with a trusted profile, one
// installed before 0.7.0, which recorded no layout.
const (
	layoutNone    = "none"
	layoutPending = "pending: "
)

// layoutInstalled reports whether the record names a layout BNK is installed
// with.
func layoutInstalled(l string) bool {
	return l != "" && l != layoutNone && !strings.HasPrefix(l, layoutPending)
}

func checkLayout(r *config.Resolved, c *config.Config) error {
	// An install made before 0.7.0: a trusted profile and no record at all
	// (0.7.0 records pending before it creates the profile).
	if r.InstalledLayout == "" && r.TrustedProfileID != "" && c.BNK.Certificates.Mode != config.CertModeCertManager {
		// Installed before 0.7.0 (the trusted profile is install's): with
		// cert-manager, the only certificates there were; its namespaces are
		// not known, so only the certificate switch is refused.
		return fmt.Errorf("this workspace was installed before 0.7.0, with cert-manager, and config.yaml now asks for "+
			"certificates=%s: changing it under a running install is not supported. Run `roksbnkargoctl uninstall`, "+
			"then install again", c.BNK.Certificates.Mode)
	}
	if !layoutInstalled(r.InstalledLayout) || r.InstalledLayout == installLayout(c) {
		return nil
	}
	return fmt.Errorf("BNK is installed with %s, and config.yaml now asks for %s: changing either under a running "+
		"install is not supported. Run `roksbnkargoctl uninstall`, then install again", r.InstalledLayout, installLayout(c))
}
