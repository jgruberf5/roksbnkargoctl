package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/jgruberf5/roksbnkargoctl/internal/config"
	"github.com/jgruberf5/roksbnkargoctl/internal/hub"
	"github.com/jgruberf5/roksbnkargoctl/internal/ibm"
	"github.com/jgruberf5/roksbnkargoctl/internal/vsi"
)

// ---- argocd (test hub) --------------------------------------------------------------

type hubRecord struct {
	vsi.Record
	URL           string `json:"url"`
	Version       string `json:"version"`
	AdminPassword string `json:"admin_password"`
}

func newArgoCDCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "argocd",
		Short: "A TEST Argo CD hub (k3s + Argo CD on a VSI) for exercising the install",
		Long: `The install targets the customer's existing Argo CD. For testing without one,
` + "`argocd up`" + ` builds a hub like roksbnkctl's demo: an Ubuntu VSI with k3s and upstream
Argo CD (test_hub.version), exposed on its floating IP at test_hub.node_port,
in its own VPC (test_hub.cidr) attached to the transit gateway so it can reach
the ROKS private endpoint. It then points this workspace's argocd.server at it
and prints an API token for ARGOCD_AUTH_TOKEN.

The admin password is generated here and kept in argocd-hub.json (mode 0600).`,
	}
	up := &cobra.Command{Use: "up", Short: "Build the test Argo CD hub", RunE: func(cmd *cobra.Command, _ []string) error {
		s, err := newSession(cmd)
		if err != nil {
			return err
		}
		return runHubUp(cmd.Context(), s, cmd)
	}}
	down := &cobra.Command{Use: "down", Short: "Delete the test Argo CD hub", RunE: func(cmd *cobra.Command, _ []string) error {
		s, err := newSession(cmd)
		if err != nil {
			return err
		}
		if !confirm(cmd, "Delete the test Argo CD hub VSI and its network?") {
			return errors.New("not confirmed (pass --yes)")
		}
		var rec hubRecord
		if err := readJSON(s.ws.HubOutputsPath(), &rec); err != nil {
			return err
		}
		c, err := s.IBM()
		if err != nil {
			return err
		}
		if err := vsi.Teardown(cmd.Context(), c.WithRegion(rec.Region), &rec.Record, s.p.step); err != nil {
			return err
		}
		_ = os.Remove(s.ws.HubOutputsPath())
		s.p.ok("test hub removed")
		return nil
	}}
	status := &cobra.Command{Use: "status", Short: "Show the test hub", RunE: func(cmd *cobra.Command, _ []string) error {
		s, err := newSession(cmd)
		if err != nil {
			return err
		}
		var rec hubRecord
		if err := readJSON(s.ws.HubOutputsPath(), &rec); err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "URL:       %s  (admin password in %s)\nVersion:   %s\nInstance:  %s\n", rec.URL, s.ws.HubOutputsPath(), rec.Version, rec.InstanceID)
		return hub.WaitReady(cmd.Context(), rec.URL, time.Second, nil)
	}}
	cmd.AddCommand(up, down, status)
	return cmd
}

// errTestHubCIDR is what `argocd up` says without test_hub.cidr. Operators
// reach it while meaning to use the Argo CD they already run, so it says
// first what `argocd up` is for and how to skip it.
var errTestHubCIDR = errors.New("`argocd up` builds a NEW test Argo CD (k3s + Argo CD on a VSI in its own VPC); it is not needed to use an Argo CD you already have.\n" +
	"  To use an existing Argo CD: skip `argocd up`, set argocd.server to its URL (config.yaml, " + config.EnvName("argocd.server") + ", or `init`), and put its API token in ARGOCD_AUTH_TOKEN (argocd.token_env).\n" +
	"  To build the test Argo CD: set test_hub.cidr, a /24–/28 not used by any VPC on the transit gateway (e.g. 10.248.1.0/28)")

func runHubUp(ctx context.Context, s *session, cmd *cobra.Command) error {
	c, p := s.cfg, s.p
	r, err := s.resolved()
	if err != nil {
		return err
	}
	h := c.TestHub
	if h.CIDR == "" {
		return errTestHubCIDR
	}
	region := h.Region
	if region == "" {
		region = c.IBMCloud.Region
	}
	zone := h.Zone
	if zone == "" {
		zone = region + "-1"
	}
	base, err := s.IBM()
	if err != nil {
		return err
	}
	ibmc := base.WithRegion(region)
	var prev hubRecord
	_ = readJSON(s.ws.HubOutputsPath(), &prev)
	pw := prev.AdminPassword
	if pw == "" {
		if pw, err = hub.NewPassword(); err != nil {
			return err
		}
	}
	ud, err := hub.CloudInit(hub.Params{ArgoCDVersion: h.Version, K3sChannel: h.K3sChannel, NodePort: h.NodePort, AdminPassword: pw})
	if err != nil {
		return err
	}
	inbound := []vsi.Rule{{PortMin: h.NodePort, PortMax: h.NodePort, CIDR: h.AllowedCIDR}}
	if h.SSHKey != "" {
		inbound = append(inbound, vsi.Rule{PortMin: 22, PortMax: 22, CIDR: h.AllowedCIDR})
	}
	tgw := ""
	if h.AttachToTGW == nil || *h.AttachToTGW {
		tgw = r.TransitGatewayID
	}
	rec, perr := vsi.Provision(ctx, ibmc, vsi.Spec{
		Name: h.ResourceName, Zone: zone, CIDR: h.CIDR, Profile: h.Profile, UserData: ud, BootVolumeGB: 100,
		SSHKeyName: h.SSHKey, FloatingIP: true, Inbound: inbound, ResourceGroupID: r.ResourceGroupID,
		TransitGateway: tgw, Log: p.step,
	})
	out := hubRecord{Record: *rec, Version: h.Version, AdminPassword: pw}
	if rec.FloatingIP != "" {
		out.URL = hub.URL(rec.FloatingIP, h.NodePort)
	}
	if err := writeJSON(s.ws.HubOutputsPath(), out); err != nil {
		return err
	}
	if perr != nil {
		return fmt.Errorf("%w (partial state saved; `argocd down` cleans it up)", perr)
	}
	p.step("waiting for Argo CD %s at %s (k3s + Argo CD install takes ~6 minutes)", h.Version, out.URL)
	if err := hub.WaitReady(ctx, out.URL, 20*time.Minute, p.info); err != nil {
		return err
	}
	tok, err := hub.MintToken(ctx, out.URL, pw, 5*time.Minute)
	if err != nil {
		return err
	}
	if err := recordTestHub(s, out.URL); err != nil {
		return err
	}
	p.ok("test Argo CD %s at %s; argocd.server updated", h.Version, out.URL)
	p.info("UI login: admin / (admin_password in %s)", s.ws.HubOutputsPath())
	fmt.Fprintf(cmd.OutOrStdout(), "export %s=%s\n", c.ArgoCD.TokenEnv, tok)
	return nil
}

// recordTestHub points the workspace's Argo CD at the test hub: a change to
// saved settings, made through update so that save writes it (and nothing an
// override supplied).
func recordTestHub(s *session, url string) error {
	s.update(func(c *config.Config) { c.ArgoCD.Server, c.ArgoCD.Insecure = url, true })
	return s.save()
}

// ---- small JSON file helpers -----------------------------------------------------------

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%s does not exist (nothing was built)", path)
		}
		return err
	}
	return json.Unmarshal(b, v)
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return config.WriteFileAtomic(path, b, 0o600)
}

var _ = ibm.IsNotFound
