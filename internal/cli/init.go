package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/jgruberf5/roksbnkargoctl/internal/config"
	"github.com/jgruberf5/roksbnkargoctl/internal/gitpub"
)

func newInitCmd() *cobra.Command {
	var configFile string
	var refresh bool
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Create or update a workspace (interview, or --config-file)",
		Long: `init gathers what the install needs and resolves it against IBM Cloud: the
ROKS cluster (name or id) and its VPC, the transit gateway (name or id), the COS
objects holding the FAR key and subscription JWT, the Argo CD server and the Git
repo Argo CD will sync from.

With --config-file it reads a config.yaml (strictly: unknown keys are errors)
and asks nothing. Without it, it interviews, offering choices from the account.
Re-running init on an existing workspace resumes the interview with the saved
answers as defaults. --refresh re-resolves an existing config without asking.

Secrets are never asked for or stored. Set them in the environment:
  IBMCLOUD_API_KEY, ARGOCD_AUTH_TOKEN, ROKSBNKARGOCTL_GIT_TOKEN
  (and ROKSBNKARGOCTL_MIRROR_PASSWORD with a private registry)`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runInit(cmd, configFile, refresh)
		},
	}
	cmd.Flags().StringVarP(&configFile, "config-file", "f", "", "read config.yaml from this path or http(s) URL instead of interviewing")
	cmd.Flags().BoolVar(&refresh, "refresh", false, "re-resolve the saved config without asking")
	return cmd
}

func runInit(cmd *cobra.Command, configFile string, refresh bool) error {
	ctx := cmd.Context()
	p := out(cmd)
	if flagNoWorkspace {
		return errors.New("init creates a workspace: drop --no-workspace")
	}
	ovs, err := commandOverrides(cmd)
	if err != nil {
		return err
	}
	name := flagWorkspace
	if name == "" {
		name = os.Getenv("ROKSBNKARGOCTL_WORKSPACE")
	}
	if name == "" {
		name = ask("Workspace name", "bnk")
	}
	ws, err := config.Open(name)
	if err != nil {
		return err
	}
	var c *config.Config
	switch {
	case configFile != "":
		data, err := readConfigSource(ctx, configFile)
		if err != nil {
			return err
		}
		if c, err = config.Parse(data); err != nil {
			return err
		}
		// A resolved section in the given file is not trusted; the
		// workspace's own record is kept, so what install created (the
		// trusted profile, the TGW connection, the installed layout) is
		// carried through the re-resolve as it is with --refresh.
		c.Resolved = nil
		if ws.Exists() {
			if old, err := ws.LoadFile(); err == nil {
				c.Resolved = old.Resolved
			}
		}
		c.Defaults(name)
	case ws.Exists():
		if c, err = ws.Load(); err != nil {
			return err
		}
		if !refresh {
			if err := interview(ctx, c, name); err != nil {
				return err
			}
		}
	default:
		if refresh {
			return fmt.Errorf("workspace %q has no config to refresh", name)
		}
		c = &config.Config{}
		c.Defaults(name)
		if err := interview(ctx, c, name); err != nil {
			return err
		}
	}
	c.Defaults(name)
	// c is what config.yaml will hold. The overrides apply on top of it for the
	// validation and the lookups, and are never saved: resolved is recorded
	// for the effective settings, so the same overrides must be set for later
	// commands (resolved() refuses a cluster/gateway override it does not match).
	s, err := buildSession(p, ws, name, c, ovs)
	if err != nil {
		return err
	}
	if err := s.cfg.Validate(); err != nil {
		return err
	}
	if err := initRemoteChecks(ctx, s); err != nil {
		return err
	}
	if err := resolveWorkspace(ctx, s); err != nil {
		return err
	}
	if err := s.save(); err != nil {
		return err
	}
	if err := setCurrent(name); err != nil {
		return err
	}
	p.ok("workspace %q saved to %s", name, ws.ConfigPath())
	for _, o := range ovs {
		if s.overridden(o.Key.Path) {
			p.warn("%s: %s was used but not saved (config.yaml keeps its own value)", o.Source, o.Key.Path)
		}
	}
	p.info("next: roksbnkargoctl render   (then review %s)", ws.ManifestsDir())
	return nil
}

func readConfigSource(ctx context.Context, src string) ([]byte, error) {
	if strings.HasPrefix(src, "https://") || strings.HasPrefix(src, "http://") {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, src, nil)
		if err != nil {
			return nil, err
		}
		resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("GET %s: %s", src, resp.Status)
		}
		return io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	}
	return os.ReadFile(src)
}

// interview asks for every field an operator must supply, offering account
// choices where the API can list them. Answers already in c are the defaults.
func interview(ctx context.Context, c *config.Config, ws string) error {
	fmt.Fprintln(os.Stderr, "roksbnkargoctl init — press Enter to accept a [default].")
	c.IBMCloud.Region = ask("IBM Cloud region of the ROKS cluster", firstOf(c.IBMCloud.Region, "us-east"))
	c.IBMCloud.ResourceGroup = ask("Resource group", c.IBMCloud.ResourceGroup)

	s := &session{cfg: c}
	ibmc, ibmErr := s.IBM()
	if ibmErr != nil {
		fmt.Fprintf(os.Stderr, "  (%v — answering without account lookups)\n", ibmErr)
	}

	// Cluster.
	var clusters []string
	if ibmErr == nil {
		if cl, err := ibmc.ListClusters(ctx); err == nil {
			for _, x := range cl {
				if x.Region == "" || x.Region == c.IBMCloud.Region {
					clusters = append(clusters, x.Name)
				}
			}
			sort.Strings(clusters)
		}
	}
	if len(clusters) > 0 {
		c.Cluster = choose("Existing ROKS cluster", clusters, firstOf(c.Cluster, clusters[0]))
	} else {
		c.Cluster = ask("Existing ROKS cluster (name or id)", c.Cluster)
	}

	// Transit gateway.
	var gws []string
	if ibmErr == nil {
		if g, err := ibmc.ListTransitGateways(ctx); err == nil {
			for _, x := range g {
				gws = append(gws, x.Name)
			}
			sort.Strings(gws)
		}
	}
	if len(gws) > 0 {
		c.TransitGateway = choose("Existing transit gateway", gws, firstOf(c.TransitGateway, gws[0]))
	} else {
		c.TransitGateway = ask("Existing transit gateway (name or id)", c.TransitGateway)
	}

	// Mode and registry.
	c.BNK.Mode = choose("Install mode (connected: nodes reach F5 licensing directly; disconnected: via an F5 License Proxy)",
		[]string{config.ModeConnected, config.ModeDisconnected}, c.BNK.Mode)
	if c.BNK.Mode == config.ModeDisconnected {
		if askYesNo("Use an existing F5 License Proxy (instead of `roksbnkargoctl flp up`)?", c.FLP.External.URL != "") {
			c.FLP.External.URL = ask("  FLP URL (https://<private-ip>:8443)", c.FLP.External.URL)
			c.FLP.External.RootCAFile = ask("  FLP root CA PEM file", c.FLP.External.RootCAFile)
		}
	}
	c.Registry.Source = choose("Where ROKS pulls BNK images from", []string{config.SourceFAR, config.SourceMirror}, c.Registry.Source)
	if c.Registry.Source == config.SourceMirror {
		m := &c.Registry.Mirror
		m.Host = ask("  Mirror host[:port]", m.Host)
		m.Prefix = ask("  Repository prefix under the host (e.g. bnk-mirror)", m.Prefix)
		m.Username = ask("  Mirror username (password from $"+m.PasswordEnv+")", m.Username)
		m.CAFile = ask("  Mirror CA PEM file (empty if publicly trusted)", m.CAFile)
	}
	interviewInstallShape(c)
	c.BNK.TMMReplicas = askInt("TMM replicas", c.BNK.TMMReplicas)
	c.BNK.StorageClass = ask("StorageClass for BNK volumes (empty = cluster default; any class works for any TMM replica count)", c.BNK.StorageClass)

	// Supply chain.
	c.COS.Instance = ask("COS instance holding the FAR key and JWT", c.COS.Instance)
	c.COS.Bucket = ask("  bucket", c.COS.Bucket)
	c.COS.Region = ask("  bucket region", c.COS.Region)
	c.COS.FARAuthObject = ask("  FAR auth tarball object", c.COS.FARAuthObject)
	c.COS.JWTObject = ask("  subscription JWT object", c.COS.JWTObject)

	// Argo CD and Git.
	if askYesNo("Use an existing Argo CD instance (3.3 or later)?", !usesTestHub(c)) {
		if usesTestHub(c) {
			c.ArgoCD.Server = ""
		}
		c.ArgoCD.Server = ask("  Argo CD server URL (https://…)", c.ArgoCD.Server)
		c.ArgoCD.Insecure = askYesNo("  Skip TLS verification (self-signed Argo CD)?", c.ArgoCD.Insecure)
		c.ArgoCD.Project = ask("  Argo CD project", c.ArgoCD.Project)
		fmt.Printf("  init checks $%s against it next: an API token for an account that can manage clusters, repositories and applications\n", c.ArgoCD.TokenEnv)
	} else {
		fmt.Println("  `roksbnkargoctl argocd up` builds a TEST Argo CD on a VSI after init; it is not for production")
		c.TestHub.CIDR = ask("  Address range for its VPC (a free /24–/28 on the transit gateway)", firstOf(c.TestHub.CIDR, "10.248.1.0/28"))
		c.TestHub.AllowedCIDR = ask("  Who may reach its UI and API (your address, e.g. 203.0.113.7/32)", firstOf(c.TestHub.AllowedCIDR, "0.0.0.0/0"))
		c.ArgoCD.Server, c.ArgoCD.Insecure = testHubPlaceholder, true
	}
	c.ArgoCD.ClusterEndpoint = choose("  Endpoint Argo CD uses to reach ROKS", []string{"private", "public"}, c.ArgoCD.ClusterEndpoint)
	c.Git.URL = ask("Git repo Argo CD syncs from (https://… or git@…)", c.Git.URL)
	c.Git.Branch = ask("  branch", c.Git.Branch)
	c.Git.Path = ask("  path inside the repo", c.Git.Path)
	if strings.HasPrefix(c.Git.URL, "https://") {
		c.Git.Username = ask("  username (token from $"+c.Git.TokenEnv+")", firstOf(c.Git.Username, "git"))
	} else {
		c.Git.SSHKeyFile = ask("  SSH private key file", c.Git.SSHKeyFile)
		c.Git.KnownHostsFile = ask("  known_hosts file for the Git host (empty for github.com / gitlab.com / bitbucket.org)", c.Git.KnownHostsFile)
	}
	_ = ws
	return nil
}

func firstOf(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// resolveWorkspace is resolve; tests substitute a fake that needs no IBM Cloud.
var resolveWorkspace = resolve

// resolve looks everything up and records it, so render and install work from
// recorded facts and a typo fails here rather than mid-install.
//
// resolve starts from a fresh record: only what install created survives a
// re-resolve (carriedOver).
func resolve(ctx context.Context, s *session) error {
	c := s.cfg
	p := s.p
	ibmc, err := s.IBM()
	if err != nil {
		return err
	}
	r := carriedOver(c.Resolved)

	p.step("resolving cluster %s", c.Cluster)
	cl, err := ibmc.GetCluster(ctx, c.Cluster)
	if err != nil {
		return fmt.Errorf("cluster %q: %w", c.Cluster, err)
	}
	if !cl.IsOpenShift() {
		return fmt.Errorf("cluster %s is not an OpenShift (ROKS) cluster", cl.Name)
	}
	if cl.Region != "" && cl.Region != c.IBMCloud.Region {
		return fmt.Errorf("cluster %s is in %s, but ibmcloud.region is %s", cl.Name, cl.Region, c.IBMCloud.Region)
	}
	if len(cl.VPCIDs) != 1 {
		return fmt.Errorf("cluster %s spans %d VPCs; expected exactly one", cl.Name, len(cl.VPCIDs))
	}
	vpc, err := ibmc.GetVPC(ctx, cl.VPCIDs[0])
	if err != nil {
		return fmt.Errorf("cluster VPC: %w", err)
	}
	r.ClusterID, r.ClusterName, r.ClusterCRN, r.ClusterVersion = cl.ID, cl.Name, cl.CRN, cl.OpenShiftVersion
	r.VPCID, r.VPCName, r.VPCCRN = vpc.ID, vpc.Name, vpc.CRN
	r.PrivateEndpoint, r.PublicEndpoint = cl.PrivateServiceEndpointURL, cl.PublicServiceEndpointURL
	r.Zones = cl.WorkerZones
	r.ResourceGroupID = cl.ResourceGroupID
	p.ok("cluster %s (%s), OpenShift %s, VPC %s, zones %s", cl.Name, cl.ID, cl.OpenShiftVersion, vpc.Name, strings.Join(cl.WorkerZones, ","))
	if len(cl.WorkerZones) < 3 {
		p.warn("the cluster has workers in %d zone(s); BNK's TMM placement spreads across 3", len(cl.WorkerZones))
	}

	p.step("resolving transit gateway %s", c.TransitGateway)
	gw, err := ibmc.ResolveTransitGateway(ctx, c.TransitGateway)
	if err != nil {
		return fmt.Errorf("transit gateway %q: %w", c.TransitGateway, err)
	}
	r.TransitGatewayID, r.TransitGatewayName = gw.ID, gw.Name
	conn, err := ibmc.FindConnectionForVPC(ctx, gw.ID, vpc.CRN)
	if err == nil && conn != nil && conn.Status == "attached" {
		r.ClusterAttachedToTGW = true
		p.ok("transit gateway %s: the cluster VPC is attached", gw.Name)
	} else {
		p.warn("transit gateway %s: the cluster VPC is NOT attached; `install` will attach it", gw.Name)
	}

	switch c.ArgoCD.ClusterEndpoint {
	case "private":
		r.ArgoCDClusterServer = cl.PrivateServiceEndpointURL
		if r.ArgoCDClusterServer == "" {
			return fmt.Errorf("cluster %s has no private service endpoint; set argocd.cluster_endpoint: public", cl.Name)
		}
	case "public":
		r.ArgoCDClusterServer = cl.PublicServiceEndpointURL
		if r.ArgoCDClusterServer == "" {
			return fmt.Errorf("cluster %s has no public service endpoint; set argocd.cluster_endpoint: private", cl.Name)
		}
	}

	if c.COS.LocalFARAuthFile == "" {
		p.step("resolving COS instance %s", c.COS.Instance)
		si, err := s.resolveCOSInstance(ctx)
		if err != nil {
			return err
		}
		r.COSInstanceCRN = si.CRN
		c.Resolved = r
		s.cosCRN = ""
		cc, bucket, err := s.cosBucket(ctx, false)
		if err != nil {
			return err
		}
		objs, err := cc.ListObjects(ctx, bucket, "")
		if err != nil {
			return fmt.Errorf("COS bucket %s: %w", c.COS.Bucket, err)
		}
		have := map[string]bool{}
		for _, o := range objs {
			have[o.Key] = true
		}
		for _, k := range []string{c.COS.FARAuthObject, c.COS.JWTObject} {
			if !have[k] {
				return fmt.Errorf("COS bucket %s has no object %q (publish it with `roksbnkargoctl cos publish`)", c.COS.Bucket, k)
			}
		}
		p.ok("COS %s/%s has %s and %s", c.COS.Instance, c.COS.Bucket, c.COS.FARAuthObject, c.COS.JWTObject)
	}
	c.Resolved = r
	return nil
}

// testHubPlaceholder is argocd.server until `argocd up` builds the test hub
// and records its URL. .invalid is reserved (RFC 2606): it never resolves.
const testHubPlaceholder = "https://argocd.placeholder.invalid"

// usesTestHub reports a workspace waiting for `argocd up`: a placeholder
// server, or none with a test_hub range set.
func usesTestHub(c *config.Config) bool {
	srv := strings.TrimSpace(c.ArgoCD.Server)
	if srv == "" {
		return c.TestHub.CIDR != ""
	}
	u, err := url.Parse(srv)
	return err == nil && strings.HasSuffix(strings.ToLower(u.Hostname()), ".invalid")
}

// checkArgoCDAtInit fails init early when the existing Argo CD it names cannot
// be used: no token in the environment, a token it rejects, or a version older
// than MinArgoCD. The same check runs first in install; finding out at init
// saves a half-configured workspace.
func checkArgoCDAtInit(ctx context.Context, s *session) error {
	c, p := s.cfg, s.p
	if usesTestHub(c) {
		p.info("argocd.server is the placeholder for a test hub: run `roksbnkargoctl argocd up` before install")
		return nil
	}
	if _, err := config.Env(c.ArgoCD.TokenEnv, "Argo CD API token"); err != nil {
		return fmt.Errorf("%w: an API token for the Argo CD at %s (Argo CD UI: Settings → Accounts → <account> → Tokens → Generate New)", err, c.ArgoCD.Server)
	}
	ac, err := s.ArgoCD()
	if err != nil {
		return err
	}
	if err := ac.CheckServer(ctx, MinArgoCD); err != nil {
		return fmt.Errorf("checking the Argo CD at %s: %w", c.ArgoCD.Server, err)
	}
	v, _ := ac.Version(ctx)
	p.ok("Argo CD %s at %s accepts the token in $%s", v, c.ArgoCD.Server, c.ArgoCD.TokenEnv)
	return nil
}

// initRemoteChecks are init's checks against Argo CD and Git. A variable so a
// test driving init through the root command need not reach either.
var initRemoteChecks = defaultInitRemoteChecks

func defaultInitRemoteChecks(ctx context.Context, s *session) error {
	if err := checkArgoCDAtInit(ctx, s); err != nil {
		return err
	}
	return checkGitAtInit(ctx, s)
}

// checkGitAtInit (#18): the repository must be readable. Push rights are only
// warned about: with export + install --no-publish nothing is pushed.
func checkGitAtInit(ctx context.Context, s *session) error {
	c, p := s.cfg, s.p
	o, err := gitOptions(c, p.warn)
	switch {
	case missingGitCredential(err):
		p.warn("no Git credential ($%s or git.ssh_key_file): install needs one to push, unless you commit `roksbnkargoctl export` yourself and run install --no-publish", c.Git.TokenEnv)
		if _, err := gitpub.Check(ctx, o, false); err != nil {
			p.warn("%v (Argo CD needs to read it; it may have its own credential)", err)
		}
		return nil
	case err != nil:
		return err
	}
	if err := checkGitAccess(ctx, p, c, o, false, false); err != nil {
		return err
	}
	if _, err := gitpub.Check(ctx, o, true); err != nil {
		p.warn("%v: install will fail at the push; fix the credential, or commit `roksbnkargoctl export` yourself and run install --no-publish", err)
		return nil
	}
	p.ok("Git %s: the credential can push", c.Git.URL)
	return nil
}

// carriedOver is what a re-resolve keeps from the old record: what install
// created, which init cannot look up again. The transit gateway connection
// install made was dropped before, so after `init --refresh` neither
// `uninstall --detach-tgw` nor `workspaces delete` knew about it.
func carriedOver(old *config.Resolved) *config.Resolved {
	r := &config.Resolved{}
	if old != nil {
		r.TrustedProfileID, r.ArgoCDClusterServer, r.LastPublishedCommitSHA = old.TrustedProfileID, old.ArgoCDClusterServer, old.LastPublishedCommitSHA
		r.TGWConnectionCreatedID = old.TGWConnectionCreatedID
		r.InstalledLayout = old.InstalledLayout
	}
	return r
}

// interviewInstallShape asks for the namespace layout and the certificate
// mode: one namespace for every BNK component (#31), and cert-manager or F5's
// single certificate (#30).
func interviewInstallShape(c *config.Config) {
	bnkNS := firstOf(c.BNK.Namespace, "f5-bnk")
	one := c.BNK.UtilsNamespace != "" && c.BNK.UtilsNamespace == bnkNS
	if askYesNo("Install every BNK component into one namespace ("+bnkNS+"), with no separate utilities namespace?", one) {
		c.BNK.Namespace, c.BNK.UtilsNamespace = bnkNS, bnkNS
	} else if one {
		c.BNK.UtilsNamespace = "" // back to the default f5-utils
	}
	ct := &c.BNK.Certificates
	ct.Mode = choose("Certificates for BNK's components (cert-manager: installed and managed by cert-manager; "+
		"single: F5's single certificate, no cert-manager)", []string{config.CertModeCertManager, config.CertModeSingle},
		firstOf(ct.Mode, config.CertModeCertManager))
	if ct.Mode != config.CertModeSingle {
		return
	}
	ct.Issuer = choose("  Who issues it (self-signed: a CA generated in the cluster; ca: your CA signs it; provided: your own certificate)",
		[]string{"self-signed", "ca", "provided"}, firstOf(ct.Issuer, "self-signed"))
	switch ct.Issuer {
	case "ca":
		ct.CACertFile = ask("  Your CA certificate (PEM file)", ct.CACertFile)
		ct.CAKeyFile = ask("  Your CA's private key (PEM file; written into the cluster, never to Git)", ct.CAKeyFile)
	case "provided":
		ct.CertFile = ask("  Your certificate (tls.crt, PEM file)", ct.CertFile)
		ct.KeyFile = ask("  Its private key (tls.key, PEM file; written into the cluster, never to Git)", ct.KeyFile)
		ct.CAFile = ask("  The CA that signed it (ca.crt, PEM file)", ct.CAFile)
	}
}
