package cli

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/jgruberf5/roksbnkargoctl/internal/config"
	"github.com/jgruberf5/roksbnkargoctl/internal/forge"
	"github.com/jgruberf5/roksbnkargoctl/internal/ibm"
)

// BNK Forge's own connection variables, the names roksbnkctl reads. They are
// not config overrides: a --url/--username flag or its ROKSBNKARGOCTL_*
// variable beats them, and they beat config.yaml's forge.url and
// forge.username (see forgeSetting). The password has no config key and no
// flag, so it is never on a command line or in a file.
const (
	envForgeURL      = "BNK_FORGE_URL"
	envForgeUser     = "BNK_FORGE_USER"
	envForgePassword = "BNK_FORGE_PASSWORD"
)

// forgeIBM is the part of the IBM Cloud client forge uses.
type forgeIBM interface {
	GetCluster(ctx context.Context, nameOrID string) (*ibm.Cluster, error)
	FetchAdminKubeconfig(ctx context.Context, nameOrID string, private bool) ([]byte, error)
}

// forgeIBMFor returns the IBM Cloud client for a session; tests substitute a fake.
var forgeIBMFor = func(s *session) (forgeIBM, error) { return s.IBM() }

// forgeTerminal reports whether prompts can be shown; tests substitute it.
var forgeTerminal = isTTY

// forgeReadPassword reads a password without echo; tests substitute it.
var forgeReadPassword = func() ([]byte, error) { return term.ReadPassword(int(os.Stdin.Fd())) }

func newForgeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "forge",
		Short: "Register the ROKS cluster with BNK Forge",
		Long: `Registers the ROKS cluster with a BNK Forge (v3) over its REST API, or removes it.
Works with or without a workspace: without one, name the cluster with --cluster
and --region.

  register    log in; ensure the IBM Cloud credential template and the project;
              register the cluster with a certificate-based admin kubeconfig
  unregister  remove the cluster from the project (absence is success)

Forge is found with --url (forge.url) or BNK_FORGE_URL, the login with --username
(forge.username) or BNK_FORGE_USER, and the password in BNK_FORGE_PASSWORD, else a
prompt on a terminal. The password is never a flag and never stored; the session
is not cached, so every run logs in. The IBM Cloud API key comes from
IBMCLOUD_API_KEY (or the variable ibmcloud.api_key_env names).`,
	}

	var force bool
	reg := &cobra.Command{
		Use:   "register",
		Short: "Register the ROKS cluster with BNK Forge (updates an existing registration in place)",
		Long: `Registers the cluster in a BNK Forge project (default: the cluster's name).

Re-registering a cluster this project already holds updates it in place and keeps
its Forge cluster id. A cluster held by ANOTHER project is refused, naming that
project, unless --force: that moves it here, and its cluster id changes.

Forge gets an IBM Cloud credential template holding the API key, and the
cluster's admin kubeconfig: self-contained, with the admin client certificate
and key (ROKS does not accept IBM IAM tokens), for the endpoint --endpoint names.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := newSessionOptional(cmd)
			if err != nil {
				return err
			}
			return runForgeRegister(cmd, s, force)
		},
	}
	bindForgeFlags(reg)
	reg.Flags().BoolVar(&force, "force", false, "take over a cluster registered to ANOTHER Forge project (moves it; its cluster id changes)")

	unreg := &cobra.Command{
		Use:   "unregister",
		Short: "Remove the ROKS cluster from its BNK Forge project",
		Long: `Removes the cluster from its BNK Forge project, the inverse of register. It never
creates anything, and absence is success: no project, no cluster of that name, or
one already deleted all exit 0, so a teardown can run it twice or late.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := newSessionOptional(cmd)
			if err != nil {
				return err
			}
			return runForgeUnregister(cmd, s)
		},
	}
	bindForgeFlags(unreg, "endpoint")

	cmd.AddCommand(reg, unreg)
	return cmd
}

func bindForgeFlags(cmd *cobra.Command, skip ...string) {
	bindConfigFlags(cmd, "forge", "", skip...)
	bindConfigFlag(cmd, "cluster", "cluster")
	bindConfigFlag(cmd, "region", "ibmcloud.region")
}

// forgeCluster is the cluster as Forge is told about it.
type forgeCluster struct {
	ID, Name, Region, ResourceGroup string
	PublicURL, PrivateURL           string
}

// forgeTarget identifies the cluster: the workspace's resolved facts, or,
// without a workspace, a lookup of --cluster in --region.
func forgeTarget(ctx context.Context, s *session) (forgeCluster, error) {
	if s.hasWorkspace() {
		r, err := s.resolved()
		if err != nil {
			return forgeCluster{}, err
		}
		// The workspace was resolved in config.yaml's region.
		if src := s.overrideSource("ibmcloud.region"); src != "" && s.file != nil && s.file.IBMCloud.Region != s.cfg.IBMCloud.Region {
			return forgeCluster{}, fmt.Errorf("%s sets region %s, but cluster %s was resolved in %s: Forge would be told the wrong region", src, s.cfg.IBMCloud.Region, r.ClusterName, s.file.IBMCloud.Region)
		}
		return forgeCluster{
			ID: r.ClusterID, Name: r.ClusterName, Region: s.cfg.IBMCloud.Region,
			ResourceGroup: s.cfg.IBMCloud.ResourceGroup,
			PublicURL:     r.PublicEndpoint, PrivateURL: r.PrivateEndpoint,
		}, nil
	}
	if err := forgeNeedsClusterAndRegion(s.cfg); err != nil {
		return forgeCluster{}, err
	}
	c, err := forgeIBMFor(s)
	if err != nil {
		return forgeCluster{}, err
	}
	cl, err := c.GetCluster(ctx, s.cfg.Cluster)
	if err != nil {
		return forgeCluster{}, err
	}
	if cl.Region != "" && cl.Region != s.cfg.IBMCloud.Region {
		return forgeCluster{}, fmt.Errorf("cluster %s is in region %s, not %s (--region): Forge would be told the wrong region", cl.Name, cl.Region, s.cfg.IBMCloud.Region)
	}
	rg := s.cfg.IBMCloud.ResourceGroup
	if cl.ResourceGroupName != "" && !s.overridden("ibmcloud.resource_group") {
		rg = cl.ResourceGroupName
	}
	return forgeCluster{
		ID: cl.ID, Name: cl.Name, Region: s.cfg.IBMCloud.Region, ResourceGroup: rg,
		PublicURL: cl.PublicServiceEndpointURL, PrivateURL: cl.PrivateServiceEndpointURL,
	}, nil
}

func forgeNeedsClusterAndRegion(c *config.Config) error {
	var missing []string
	if c.Cluster == "" {
		missing = append(missing, "--cluster (or "+config.EnvName("cluster")+")")
	}
	if c.IBMCloud.Region == "" {
		missing = append(missing, "--region (or "+config.EnvName("ibmcloud.region")+")")
	}
	if len(missing) > 0 {
		return fmt.Errorf("without a workspace, name the cluster: %s", strings.Join(missing, " and "))
	}
	return nil
}

// forgeProject is the Forge project: forge.project, else the cluster name.
func forgeProject(s *session, clusterName string) string {
	if p := s.cfg.Forge.Project; p != "" {
		return p
	}
	return clusterName
}

// forgeSetting is a forge.* value: a flag or ROKSBNKARGOCTL_* override wins,
// then the BNK_FORGE_* variable, then config.yaml.
func forgeSetting(s *session, path, env string) string {
	v, _ := s.cfg.Get(path)
	str, _ := v.(string)
	if s.overrideFor(path) != "" {
		return str
	}
	if e := strings.TrimSpace(os.Getenv(env)); e != "" {
		return e
	}
	return str
}

// forgeConn is a Forge client with the login it will use. It is prepared
// before anything is contacted, so a missing setting fails fast.
type forgeConn struct {
	c               *forge.Client
	url, user, pass string
}

// forgeConnect resolves the connection settings and builds the client.
func forgeConnect(cmd *cobra.Command, s *session) (*forgeConn, error) {
	url := forgeSetting(s, "forge.url", envForgeURL)
	if url == "" {
		return nil, fmt.Errorf("no BNK Forge URL: pass --url, or set %s or %s", envForgeURL, config.EnvName("forge.url"))
	}
	if !strings.HasPrefix(url, "https://") && !strings.HasPrefix(url, "http://") {
		return nil, fmt.Errorf("BNK Forge URL %q must start with https:// (or http://)", url)
	}
	if strings.HasPrefix(url, "http://") {
		s.p.warn("forge: %s is plain http: the Forge password and the IBM Cloud API key in the credential template go to it unencrypted", url)
	}
	user := forgeSetting(s, "forge.username", envForgeUser)
	if user == "" && forgeTerminal() {
		user = ask("BNK Forge username", "")
	}
	if user == "" {
		return nil, fmt.Errorf("no BNK Forge username: pass --username, or set %s or %s", envForgeUser, config.EnvName("forge.username"))
	}
	pass, err := forgePassword(cmd.ErrOrStderr())
	if err != nil {
		return nil, err
	}
	opts := forge.Options{Insecure: s.cfg.Forge.Insecure}
	if f := s.cfg.Forge.CAFile; f != "" {
		if opts.CAPEM, err = os.ReadFile(f); err != nil {
			return nil, fmt.Errorf("forge.ca_file: %w", err)
		}
		if opts.Insecure {
			s.p.info("forge: a CA is pinned (forge.ca_file), so insecure is ignored and the certificate is verified")
		}
	}
	c, err := forge.New(url, opts)
	if err != nil {
		return nil, err
	}
	c.Log = cmd.ErrOrStderr()
	return &forgeConn{c: c, url: url, user: user, pass: pass}, nil
}

// login logs in. Nothing is cached: there is no workspace to keep a token in,
// so every run logs in.
func (fc *forgeConn) login(ctx context.Context) (*forge.Client, error) {
	if err := fc.c.Login(ctx, fc.user, fc.pass); err != nil {
		return nil, fmt.Errorf("BNK Forge login as %s failed: %w", fc.user, err)
	}
	return fc.c, nil
}

// forgePassword is BNK_FORGE_PASSWORD, else a no-echo prompt on a terminal.
func forgePassword(w io.Writer) (string, error) {
	if p := os.Getenv(envForgePassword); strings.TrimSpace(p) != "" {
		return strings.TrimSpace(p), nil
	}
	if !forgeTerminal() {
		return "", fmt.Errorf("no BNK Forge password: set %s (there is no terminal to prompt on)", envForgePassword)
	}
	fmt.Fprint(w, "BNK Forge password: ")
	b, err := forgeReadPassword()
	fmt.Fprintln(w)
	if err != nil {
		return "", fmt.Errorf("reading the BNK Forge password: %w", err)
	}
	p := strings.TrimSpace(string(b))
	if p == "" {
		return "", errors.New("empty BNK Forge password")
	}
	return p, nil
}

func forgeEndpoint(s *session) (string, error) {
	switch e := s.cfg.Forge.Endpoint; e {
	case "", "public":
		return "public", nil
	case "private":
		return e, nil
	default:
		return "", fmt.Errorf("forge.endpoint %q: must be public or private", e)
	}
}

func runForgeRegister(cmd *cobra.Command, s *session, force bool) error {
	ctx := cmd.Context()
	endpoint, err := forgeEndpoint(s)
	if err != nil {
		return err
	}
	// The API key is stored in Forge as the credential template; check it
	// before anything is contacted.
	apiKey, err := s.cfg.APIKey()
	if err != nil {
		return err
	}
	fc, err := forgeConnect(cmd, s)
	if err != nil {
		return err
	}
	cl, err := forgeTarget(ctx, s)
	if err != nil {
		return err
	}
	if endpoint == "public" && cl.PublicURL == "" && cl.PrivateURL != "" {
		return fmt.Errorf("cluster %s has no public service endpoint; pass --endpoint private (Forge must then reach %s)", cl.Name, cl.PrivateURL)
	}
	if endpoint == "private" && cl.PrivateURL == "" && cl.PublicURL != "" {
		return fmt.Errorf("cluster %s has no private service endpoint; use --endpoint public", cl.Name)
	}

	ic, err := forgeIBMFor(s)
	if err != nil {
		return err
	}
	s.p.step("fetching the admin kubeconfig for %s (%s endpoint)", cl.Name, endpoint)
	admin, err := ic.FetchAdminKubeconfig(ctx, cl.ID, endpoint == "private")
	if err != nil {
		return fmt.Errorf("admin kubeconfig for cluster %s: %w", cl.Name, err)
	}
	kc, err := forge.CertKubeconfig(admin, cl.Name+"-admin")
	if err != nil {
		return fmt.Errorf("cluster %s: %w", cl.Name, err)
	}

	c, err := fc.login(ctx)
	if err != nil {
		return err
	}
	project := forgeProject(s, cl.Name)
	s.p.step("registering cluster %s with BNK Forge %s (project %s)", cl.Name, fc.url, project)

	cos := ""
	if s.hasWorkspace() || s.overridden("cos.instance") {
		cos = s.cfg.COS.Instance
	}
	tid, err := c.EnsureIBMCredentialTemplate(ctx, forge.IBMCredentialTemplate{
		Name:          "roksbnkargoctl-" + project,
		APIKey:        apiKey,
		ResourceGroup: cl.ResourceGroup,
		Region:        cl.Region,
		COSInstance:   cos,
	})
	if err != nil {
		return fmt.Errorf("ensuring the IBM Cloud credential template: %w", err)
	}
	pid, err := c.EnsureProject(ctx, project)
	if err != nil {
		return fmt.Errorf("ensuring BNK Forge project %q: %w", project, err)
	}
	fid, err := c.RegisterCluster(ctx, pid, forge.RegisterRequest{
		Name:          cl.Name,
		Provider:      "IBM",
		CloudProvider: "ibm",
		ClusterID:     cl.ID,
		Region:        cl.Region,
		TemplateID:    tid,
		Kubeconfig:    base64.StdEncoding.EncodeToString(kc),
	}, force)
	if err != nil {
		return fmt.Errorf("registering cluster %s with BNK Forge: %w", cl.Name, err)
	}
	s.p.ok("registered cluster %s with BNK Forge (project %q, Forge cluster id %d)", cl.Name, project, fid)
	return nil
}

func runForgeUnregister(cmd *cobra.Command, s *session) error {
	ctx := cmd.Context()
	fc, err := forgeConnect(cmd, s)
	if err != nil {
		return err
	}
	name, err := forgeUnregisterName(ctx, s)
	if err != nil {
		return err
	}
	c, err := fc.login(ctx)
	if err != nil {
		return err
	}
	project := forgeProject(s, name)
	pid, err := c.ProjectIDByName(ctx, project)
	if err != nil {
		return fmt.Errorf("looking up BNK Forge project %q: %w", project, err)
	}
	if pid == 0 {
		s.p.ok("no BNK Forge project %q: nothing to unregister", project)
		return nil
	}
	id, err := c.UnregisterCluster(ctx, pid, name)
	if err != nil {
		return fmt.Errorf("unregistering cluster %s from BNK Forge: %w", name, err)
	}
	if id == 0 {
		s.p.ok("cluster %s is not registered in BNK Forge project %q: nothing to do", name, project)
		return nil
	}
	s.p.ok("unregistered cluster %s (Forge cluster id %d) from BNK Forge project %q", name, id, project)
	return nil
}

// forgeUnregisterName is the cluster's name, which is what Forge knows it by.
// A cluster already deleted from IBM Cloud is still unregistered, by the name
// given: a teardown often runs after the cluster is gone.
func forgeUnregisterName(ctx context.Context, s *session) (string, error) {
	if s.hasWorkspace() {
		r, err := s.resolved()
		if err != nil {
			return "", err
		}
		return r.ClusterName, nil
	}
	if err := forgeNeedsClusterAndRegion(s.cfg); err != nil {
		return "", err
	}
	c, err := forgeIBMFor(s)
	if err != nil {
		return "", err
	}
	cl, err := c.GetCluster(ctx, s.cfg.Cluster)
	if errors.Is(err, ibm.ErrClusterNotFound) {
		s.p.warn("cluster %s is not in IBM Cloud; unregistering it from BNK Forge by that name", s.cfg.Cluster)
		return s.cfg.Cluster, nil
	}
	if err != nil {
		return "", err
	}
	return cl.Name, nil
}
