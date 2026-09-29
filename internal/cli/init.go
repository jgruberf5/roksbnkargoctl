package cli

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/jgruberf5/roksbnkargoctl/internal/config"
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
		c.Resolved = nil
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
	if err := c.Validate(); err != nil {
		return err
	}
	s := &session{ws: ws, cfg: c, p: p}
	if err := resolve(ctx, s); err != nil {
		return err
	}
	if err := s.save(); err != nil {
		return err
	}
	if err := setCurrent(name); err != nil {
		return err
	}
	p.ok("workspace %q saved to %s", name, ws.ConfigPath())
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
	c.BNK.TMMReplicas = askInt("TMM replicas", c.BNK.TMMReplicas)
	c.BNK.StorageClass = ask("StorageClass for BNK volumes (empty = cluster default; any class works for any TMM replica count)", c.BNK.StorageClass)

	// Supply chain.
	c.COS.Instance = ask("COS instance holding the FAR key and JWT", c.COS.Instance)
	c.COS.Bucket = ask("  bucket", c.COS.Bucket)
	c.COS.Region = ask("  bucket region", c.COS.Region)
	c.COS.FARAuthObject = ask("  FAR auth tarball object", c.COS.FARAuthObject)
	c.COS.JWTObject = ask("  subscription JWT object", c.COS.JWTObject)

	// Argo CD and Git.
	c.ArgoCD.Server = ask("Argo CD server URL (https://…)", c.ArgoCD.Server)
	c.ArgoCD.Insecure = askYesNo("  Skip TLS verification (self-signed Argo CD)?", c.ArgoCD.Insecure)
	c.ArgoCD.Project = ask("  Argo CD project", c.ArgoCD.Project)
	c.ArgoCD.ClusterEndpoint = choose("  Endpoint Argo CD uses to reach ROKS", []string{"private", "public"}, c.ArgoCD.ClusterEndpoint)
	c.Git.URL = ask("Git repo Argo CD syncs from (https://… or git@…)", c.Git.URL)
	c.Git.Branch = ask("  branch", c.Git.Branch)
	c.Git.Path = ask("  path inside the repo", c.Git.Path)
	if strings.HasPrefix(c.Git.URL, "https://") {
		c.Git.Username = ask("  username (token from $"+c.Git.TokenEnv+")", firstOf(c.Git.Username, "git"))
	} else {
		c.Git.SSHKeyFile = ask("  SSH private key file", c.Git.SSHKeyFile)
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

// resolve looks everything up and records it, so render and install work from
// recorded facts and a typo fails here rather than mid-install.
func resolve(ctx context.Context, s *session) error {
	c := s.cfg
	p := s.p
	ibmc, err := s.IBM()
	if err != nil {
		return err
	}
	r := &config.Resolved{}
	if old := c.Resolved; old != nil {
		r.TrustedProfileID, r.ArgoCDClusterServer, r.LastPublishedCommitSHA = old.TrustedProfileID, old.ArgoCDClusterServer, old.LastPublishedCommitSHA
	}

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
		si, err := ibmc.FindServiceInstance(ctx, c.COS.Instance, "cloud-object-storage", "")
		if err != nil {
			return fmt.Errorf("COS instance %q: %w", c.COS.Instance, err)
		}
		r.COSInstanceCRN = si.CRN
		c.Resolved = r
		cc, err := s.COS(ctx)
		if err != nil {
			return err
		}
		objs, err := cc.ListObjects(ctx, c.COS.Bucket, "")
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
