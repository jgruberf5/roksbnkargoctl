package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"github.com/spf13/cobra"

	"github.com/jgruberf5/roksbnkargoctl/internal/config"
	"github.com/jgruberf5/roksbnkargoctl/internal/flp"
	"github.com/jgruberf5/roksbnkargoctl/internal/vsi"
)

// flpRecord is the FLP state: what `flp up` built, and the CA BNK trusts.
type flpRecord struct {
	vsi.Record
	URL       string `json:"url"`
	RootCAPEM string `json:"root_ca_pem"`
	// The CA key stays with the record (mode 0600, like roksbnkctl's Terraform
	// state) so a re-run reuses the CA BNK already trusts instead of re-keying.
	RootCAKeyPEM string `json:"root_ca_key_pem"`
	Version      string `json:"version"`
	// CAFile is where the CA certificate alone was written (--ca-out), if anywhere.
	CAFile string `json:"ca_file,omitempty"`
}

// Seams for tests: the IBM Cloud VPC API for a region, and F5's JWKS, which
// comes from the f5-license-proxy chart on FAR.
var (
	flpVSIAPI = func(s *session, region string) (vsi.API, error) {
		c, err := s.IBM()
		if err != nil {
			return nil, err
		}
		return c.WithRegion(region), nil
	}
	flpProdJWKS = func(ctx context.Context, s *session, version string) ([]byte, error) {
		pl, err := s.Puller(ctx, true)
		if err != nil {
			return nil, err
		}
		s.p.step("reading prod_jwks from f5-license-proxy %s", version)
		chart, err := pl.PullChart(ctx, s.cfg.Registry.FARHost+"/charts/f5-license-proxy:"+version)
		if err != nil {
			return nil, err
		}
		return flp.ProdJWKS(chart)
	}
)

// flpOpts are the flp flags that are not config keys.
type flpOpts struct {
	name  string // --name: the FLP's resources are <name>-flp-*
	state string // --state
	caOut string // --ca-out
}

func (o *flpOpts) bind(cmd *cobra.Command, caOut bool) {
	cmd.Flags().StringVar(&o.name, "name", "", "base name: resources are named <name>-flp, <name>-flp-vpc, … (default: the workspace name; required without a workspace)")
	cmd.Flags().StringVar(&o.state, "state", "", "FLP state file: resource ids, URL, CA and CA key (default: flp-outputs.json in the workspace, or ./<name>-flp.json without one)")
	if caOut {
		cmd.Flags().StringVar(&o.caOut, "ca-out", "", "write the CA certificate alone here, for flp.external.root_ca_file (default without a workspace: ./<name>-flp-ca.pem)")
	}
}

// flpNameRE is what IBM Cloud accepts as a VPC resource name, less the
// "-flp-subnet" suffix every one of them gets.
var flpNameRE = regexp.MustCompile(`^[a-z]([a-z0-9-]{0,40}[a-z0-9])?$`)

// name is the FLP's --name: the flag, else the workspace's name.
func (o *flpOpts) nameFor(s *session) (string, error) {
	n, what := o.name, "--name"
	if n == "" {
		if s.ws == nil {
			return "", errors.New("--name is required without a workspace: the license proxy's resources are named <name>-flp-*, and `flp down --name` finds them by it")
		}
		n, what = s.ws.Name, "workspace name"
	}
	if !flpNameRE.MatchString(n) {
		return "", fmt.Errorf("%s %q cannot name IBM Cloud resources: use lowercase letters, digits and hyphens, starting with a letter (at most 42 characters), in --name", what, n)
	}
	return n, nil
}

// statePath is where the FLP state is: --state, else the workspace's
// flp-outputs.json, else ./<name>-flp.json. explicit reports --state.
func (o *flpOpts) statePath(s *session) (path string, explicit bool, err error) {
	if o.state != "" {
		return o.state, true, nil
	}
	if s.ws != nil {
		return s.ws.FLPOutputsPath(), false, nil
	}
	if o.name == "" {
		return "", false, errors.New("without a workspace, pass --state <file> or --name <name> (the state is ./<name>-flp.json)")
	}
	n, err := o.nameFor(s)
	if err != nil {
		return "", false, err
	}
	return n + "-flp.json", false, nil
}

// spareClusterAttachment drops the gateway connection from rec when the
// license proxy lives in the ROKS cluster's own VPC (flp.vsi.vpc): Argo CD
// reaches the cluster through that connection, install adopts it, and flp down
// must not detach it. Only a workspace knows the cluster's VPC.
func spareClusterAttachment(s *session, rec *vsi.Record) {
	r := s.cfg.Resolved
	if rec.VPCCreated || rec.TGWConnectionID == "" || r == nil || r.VPCID == "" || rec.VPCID != r.VPCID {
		return
	}
	s.p.warn("the license proxy is in the cluster's VPC; its transit gateway connection is the cluster's too and is left attached")
	rec.TGWConnectionID = ""
}

// matchesRecord refuses a --name that is not the license proxy the state
// records: with a current workspace the state is the workspace's, and acting on
// it would delete (or report) a different proxy than the one named.
func (o *flpOpts) matchesRecord(rec *flpRecord, path string) error {
	if o.name == "" || rec.Name == o.name+"-flp" {
		return nil
	}
	return fmt.Errorf("--name %s is not the license proxy recorded in %s (%s): pass --no-workspace (or --state) to act on %s-flp", o.name, path, rec.Name, o.name)
}

// caPath is where up writes the CA certificate: --ca-out, else
// ./<name>-flp-ca.pem without a workspace. With a workspace install reads the
// CA from the state, so no file is written unless asked for.
func (o *flpOpts) caPath(s *session, name string) string {
	if o.caOut != "" || s.ws != nil {
		return o.caOut
	}
	return name + "-flp-ca.pem"
}

func newFLPCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "flp",
		Short: "F5 License Proxy VSI for disconnected mode",
		Long: `In disconnected mode BNK licenses through an F5 License Proxy instead of reaching
F5 directly. ` + "`flp up`" + ` builds one: an Ubuntu VSI running the f5-license-proxy stack as a
podman pod, in its own VPC (flp.vsi.cidr) with a public gateway for egress to F5,
attached to the transit gateway so the ROKS workers reach it privately on :8443.
BNK trusts it through a CA generated here; install writes that CA into ROKS.

Every command works without a workspace (--no-workspace): pass --name, and the
settings as flags or ROKSBNKARGOCTL_* variables. The state (resource ids, URL,
CA and CA key) is then ./<name>-flp.json (--state), and ` + "`flp up`" + ` also writes the CA
certificate to ./<name>-flp-ca.pem (--ca-out) and prints the flp.external settings
an install uses it with.`,
	}
	var upO, downO, statusO flpOpts
	up := &cobra.Command{Use: "up", Short: "Build the license proxy VSI", RunE: func(cmd *cobra.Command, _ []string) error {
		s, err := newSessionOptional(cmd)
		if err != nil {
			return err
		}
		return runFLPUp(cmd.Context(), s, &upO, cmd.OutOrStdout())
	}}
	upO.bind(up, true)
	bindConfigFlags(up, "flp.vsi", "")
	bindFLPPlacementFlags(up)
	for name, path := range map[string]string{
		"far-auth-file": "cos.local_far_auth_file", "jwt-file": "cos.local_jwt_file",
		"cos-instance": "cos.instance", "cos-bucket": "cos.bucket", "cos-region": "cos.region",
		"far-auth-object": "cos.far_auth_object", "jwt-object": "cos.jwt_object",
	} {
		bindConfigFlag(up, name, path)
	}

	down := &cobra.Command{Use: "down", Short: "Delete what flp up built (from its state, or by --name when the state is lost)", RunE: func(cmd *cobra.Command, _ []string) error {
		s, err := newSessionOptional(cmd)
		if err != nil {
			return err
		}
		return runFLPDown(cmd.Context(), s, &downO, cmd)
	}}
	downO.bind(down, false)
	bindFLPPlacementFlags(down)
	bindConfigFlag(down, "zone", "flp.vsi.zone")
	bindConfigFlag(down, "vpc", "flp.vsi.vpc")

	status := &cobra.Command{Use: "status", Short: "Show the license proxy VSI", RunE: func(cmd *cobra.Command, _ []string) error {
		s, err := newSessionOptional(cmd)
		if err != nil {
			return err
		}
		return runFLPStatus(cmd.Context(), s, &statusO, cmd.OutOrStdout())
	}}
	statusO.bind(status, false)
	cmd.AddCommand(up, down, status)
	return cmd
}

// bindFLPPlacementFlags are where the FLP goes: region, resource group and gateway.
func bindFLPPlacementFlags(cmd *cobra.Command) {
	bindConfigFlag(cmd, "region", "ibmcloud.region")
	bindConfigFlag(cmd, "resource-group", "ibmcloud.resource_group")
	bindConfigFlag(cmd, "transit-gateway", "transit_gateway")
}

func runFLPUp(ctx context.Context, s *session, o *flpOpts, stdout io.Writer) error {
	c, p := s.cfg, s.p
	name, err := o.nameFor(s)
	if err != nil {
		return err
	}
	statePath, _, err := o.statePath(s)
	if err != nil {
		return err
	}
	caPath := o.caPath(s, name)
	v := c.FLP.VSI
	if v.CIDR == "" {
		return errors.New("flp.vsi.cidr is required (--cidr): a /24–/28 not used by any VPC on the transit gateway (e.g. 10.248.0.0/28)")
	}
	region := c.IBMCloud.Region
	if region == "" {
		return fmt.Errorf("ibmcloud.region is required (--region or %s)", config.EnvName("ibmcloud.region"))
	}
	base := name + "-flp"
	// Reuse the CA from an earlier run: BNK may already trust it. A state file
	// for another FLP is refused rather than overwritten, which would orphan
	// that FLP's resources.
	var prev flpRecord
	if _, err := readFLPState(statePath, &prev); err != nil {
		return err
	}
	if prev.Name != "" && (prev.Name != base || prev.Region != region) {
		return fmt.Errorf("%s records license proxy %s in %s, not %s in %s: pass another --state, or `flp down` that one first", statePath, prev.Name, prev.Region, base, region)
	}
	zone := v.Zone
	if zone == "" {
		zone = region + "-1"
	}
	rg, err := s.ResourceGroupID(ctx)
	if err != nil {
		return err
	}
	tgw, err := s.TransitGatewayID(ctx)
	if err != nil {
		return err
	}
	api, err := flpVSIAPI(s, region)
	if err != nil {
		return err
	}
	sa, err := s.FARServiceAccount(ctx)
	if err != nil {
		return err
	}
	jwt, err := s.JWT(ctx)
	if err != nil {
		return err
	}
	version := flp.DefaultVersion
	jwks, err := flpProdJWKS(ctx, s, version)
	if err != nil {
		return err
	}
	ca := &flp.CA{CertPEM: []byte(prev.RootCAPEM), KeyPEM: []byte(prev.RootCAKeyPEM)}
	if prev.RootCAPEM == "" || prev.RootCAKeyPEM == "" {
		if ca, err = flp.NewCA(); err != nil {
			return err
		}
	} else {
		p.info("keeping the CA from the previous flp up (%s)", statePath)
	}
	var inbound []vsi.Rule
	for _, cidr := range v.AllowedCIDRs {
		inbound = append(inbound, vsi.Rule{PortMin: flp.Port, PortMax: flp.Port, CIDR: cidr})
		if v.SSHKey != "" {
			inbound = append(inbound, vsi.Rule{PortMin: 22, PortMax: 22, CIDR: cidr})
		}
	}
	floating := v.FloatingIP == nil || *v.FloatingIP
	rec, perr := vsi.Provision(ctx, api, vsi.Spec{
		Name: base, Zone: zone, CIDR: v.CIDR, ExistingVPC: v.VPC, Profile: v.Profile, BootVolumeGB: 100,
		SSHKeyName: v.SSHKey, FloatingIP: floating, Inbound: inbound, ResourceGroupID: rg,
		TransitGateway: tgw, Log: p.step,
		BeforeInstance: func(fip string) (string, error) {
			return flp.CloudInit(flp.CloudInitInput{JWT: jwt, FARServiceAccount: sa, FARHost: c.Registry.FARHost,
				Version: version, ExternalIP: fip, ProdJWKS: jwks, CA: ca})
		},
	})
	spareClusterAttachment(s, rec)
	out := flpRecord{Record: *rec, RootCAPEM: string(ca.CertPEM), RootCAKeyPEM: string(ca.KeyPEM), Version: version, CAFile: prev.CAFile}
	if rec.PrivateIP != "" {
		out.URL = flp.URL(rec.PrivateIP)
	}
	// The state first: it records what Provision built (#28's class of bug:
	// a failed CA write returned before it, losing every resource ID).
	if err := writeJSON(statePath, out); err != nil {
		return err
	}
	if caPath != "" {
		if err := config.WriteFileAtomic(caPath, ca.CertPEM, 0o600); err != nil {
			return fmt.Errorf("writing the CA to %s: %w (the license proxy is recorded in %s)", caPath, err, statePath)
		}
		out.CAFile = caPath
		if err := writeJSON(statePath, out); err != nil {
			return err
		}
	}
	if perr != nil {
		return fmt.Errorf("%w (partial state saved in %s; `flp down` cleans it up)", perr, statePath)
	}
	p.ok("license proxy VSI %s at %s (floating %s)", base, out.URL, rec.FloatingIP)
	p.info("state (with the CA key, keep it): %s", statePath)
	p.info("the pod takes ~5 minutes to come up; `check pre-install` verifies every node reaches it")
	if caPath != "" {
		printFLPExternal(stdout, out.URL, caPath)
	}
	return nil
}

// printFLPExternal prints the settings that point an install at this FLP.
func printFLPExternal(w io.Writer, url, caPath string) {
	if abs, err := filepath.Abs(caPath); err == nil {
		caPath = abs
	}
	fmt.Fprintf(w, `# To install against this license proxy (bnk.mode: disconnected), set in config.yaml:
flp:
  external:
    url: %s
    root_ca_file: %s
# or in the environment:
`, url, caPath)
	for _, kv := range [][2]string{{config.EnvName("flp.external.url"), url}, {config.EnvName("flp.external.root_ca_file"), caPath}} {
		if runtime.GOOS == "windows" {
			fmt.Fprintf(w, "$env:%s = '%s'\n", kv[0], strings.ReplaceAll(kv[1], "'", "''"))
		} else {
			fmt.Fprintf(w, "export %s='%s'\n", kv[0], strings.ReplaceAll(kv[1], "'", `'\''`))
		}
	}
}

func runFLPStatus(ctx context.Context, s *session, o *flpOpts, stdout io.Writer) error {
	path, _, err := o.statePath(s)
	if err != nil {
		return err
	}
	var rec flpRecord
	if err := readJSON(path, &rec); err != nil {
		return err
	}
	if err := o.matchesRecord(&rec, path); err != nil {
		return err
	}
	api, err := flpVSIAPI(s, rec.Region)
	if err != nil {
		return err
	}
	inst, err := api.GetInstance(ctx, rec.InstanceID)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Instance:   %s (%s) %s\nURL:        %s   (what BNK uses)\nFloating:   %s\nVersion:    %s\nState:      %s\n",
		inst.Name, inst.ID, inst.Status, rec.URL, rec.FloatingIP, rec.Version, path)
	if rec.CAFile != "" {
		fmt.Fprintf(stdout, "CA:         %s\n", rec.CAFile)
	}
	fmt.Fprintln(stdout, "Reachability from the ROKS nodes is checked by `check pre-install` on every sync.")
	return nil
}

func runFLPDown(ctx context.Context, s *session, o *flpOpts, cmd *cobra.Command) error {
	path, explicit, err := o.statePath(s)
	if err != nil {
		return err
	}
	var rec flpRecord
	exists, err := readFLPState(path, &rec)
	switch {
	case err != nil:
		return err
	case exists:
		if err := o.matchesRecord(&rec, path); err != nil {
			return err
		}
		s.p.step("deleting license proxy %s in %s, recorded in %s", rec.Name, rec.Region, path)
		if !confirm(cmd, "Delete the license proxy VSI and its network?") {
			return errors.New("not confirmed (pass --yes)")
		}
	case !explicit:
		// The state is lost: find what up named deterministically.
		name, nerr := o.nameFor(s)
		if nerr != nil {
			return nerr
		}
		s.p.warn("%s does not exist; looking for the resources `flp up` names %s-flp*", path, name)
		r, found, derr := discoverFLP(ctx, s, name+"-flp")
		if derr != nil {
			return derr
		}
		if len(found) == 0 {
			s.p.ok("nothing named %s-flp* in %s", name, r.Region)
			return nil
		}
		fmt.Fprintf(cmd.ErrOrStderr(), "Found, by exact name:\n")
		for _, f := range found {
			fmt.Fprintf(cmd.ErrOrStderr(), "  %s\n", f)
		}
		if !confirm(cmd, fmt.Sprintf("Delete these %d resources?", len(found))) {
			return errors.New("not confirmed (pass --yes)")
		}
		rec.Record = *r
	default:
		return fmt.Errorf("%s does not exist (nothing was built, or it is elsewhere: pass the right --state, or --name to find the resources by name)", path)
	}
	spareClusterAttachment(s, &rec.Record)
	api, err := flpVSIAPI(s, rec.Region)
	if err != nil {
		return err
	}
	if err := vsi.Teardown(ctx, api, &rec.Record, s.p.step); err != nil {
		return err
	}
	_ = os.Remove(path)
	if rec.CAFile != "" {
		_ = os.Remove(rec.CAFile)
	}
	s.p.ok("license proxy removed")
	return nil
}

// readFLPState reads the FLP state; a missing file is (false, nil).
func readFLPState(path string, rec *flpRecord) (bool, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := json.Unmarshal(b, rec); err != nil {
		return false, fmt.Errorf("%s: %w", path, err)
	}
	return true, nil
}

// discoverFLP finds the FLP named base in ibmcloud.region, searching the
// configured transit gateway, or every gateway when none is configured.
func discoverFLP(ctx context.Context, s *session, base string) (*vsi.Record, []vsi.Found, error) {
	c := s.cfg
	region := c.IBMCloud.Region
	if region == "" {
		return nil, nil, fmt.Errorf("finding the license proxy by name needs ibmcloud.region (--region or %s)", config.EnvName("ibmcloud.region"))
	}
	var tgw string
	if c.TransitGateway != "" {
		id, err := s.TransitGatewayID(ctx)
		if err != nil {
			return nil, nil, err
		}
		tgw = id
	}
	zone := c.FLP.VSI.Zone
	if zone == "" {
		zone = region + "-1"
	}
	api, err := flpVSIAPI(s, region)
	if err != nil {
		return nil, nil, err
	}
	return vsi.Discover(ctx, api, vsi.DiscoverSpec{Name: base, Zone: zone, ExistingVPC: c.FLP.VSI.VPC, TransitGateway: tgw})
}
