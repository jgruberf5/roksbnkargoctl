// Package vsi stands up one Ubuntu VSI in its own VPC, attached to a transit
// gateway, through the IBM Cloud APIs — the shape both optional components
// (the F5 License Proxy and the test Argo CD hub) need. Everything it creates is
// recorded, so Teardown removes exactly that and nothing else.
package vsi

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jgruberf5/roksbnkargoctl/internal/ibm"
)

// Rule is one inbound TCP port range opened to a CIDR.
type Rule struct {
	PortMin, PortMax int
	CIDR             string
}

// Spec describes the VSI and its network.
type Spec struct {
	Name            string // base name: <name>-vpc, -subnet, -pgw, -sg, -fip, <name>
	Zone            string
	CIDR            string // the VPC's single address prefix and the subnet (/24–/28)
	ExistingVPC     string // adopt this VPC instead of creating one (name or id)
	Profile         string
	UserData        string
	BootVolumeGB    int
	SSHKeyName      string // existing VPC key; empty = no SSH
	FloatingIP      bool
	Inbound         []Rule
	ResourceGroupID string
	TransitGateway  string // id; empty = no attachment
	Log             func(format string, a ...any)
	// BeforeInstance runs after the floating IP is reserved (its address is
	// known) and may return the final user data (certificate SANs need it).
	BeforeInstance func(floatingIP string) (userData string, err error)
}

// Record is what was created, persisted by the caller for Teardown.
type Record struct {
	Name            string `json:"name"`
	Region          string `json:"region"`
	Zone            string `json:"zone"`
	VPCID           string `json:"vpc_id"`
	VPCCRN          string `json:"vpc_crn"`
	VPCCreated      bool   `json:"vpc_created"`
	SubnetID        string `json:"subnet_id,omitempty"`
	PublicGatewayID string `json:"public_gateway_id,omitempty"`
	PGWCreated      bool   `json:"public_gateway_created,omitempty"`
	SecurityGroupID string `json:"security_group_id,omitempty"`
	FloatingIPID    string `json:"floating_ip_id,omitempty"`
	FloatingIP      string `json:"floating_ip,omitempty"`
	InstanceID      string `json:"instance_id,omitempty"`
	PrivateIP       string `json:"private_ip,omitempty"`
	TGWID           string `json:"tgw_id,omitempty"`
	TGWConnectionID string `json:"tgw_connection_id,omitempty"`
}

func (s *Spec) log(f string, a ...any) {
	if s.Log != nil {
		s.Log(f, a...)
	}
}

// Provision creates (or reuses, by name) every piece and returns the record.
// On failure the partial record is returned too, so the caller can persist it
// and Teardown what was made.
func Provision(ctx context.Context, c *ibm.Client, spec Spec) (*Record, error) {
	rec := &Record{Name: spec.Name, Region: c.Region(), Zone: spec.Zone}
	err := provision(ctx, c, &spec, rec)
	return rec, err
}

func provision(ctx context.Context, c *ibm.Client, spec *Spec, rec *Record) error {
	if spec.Zone == "" || spec.CIDR == "" || spec.Profile == "" {
		return errors.New("vsi: zone, CIDR and profile are required")
	}
	// VPC.
	if spec.ExistingVPC != "" {
		v, err := c.GetVPCByNameOrID(ctx, spec.ExistingVPC)
		if err != nil {
			return fmt.Errorf("VPC %s: %w", spec.ExistingVPC, err)
		}
		rec.VPCID, rec.VPCCRN = v.ID, v.CRN
	} else {
		if v, err := c.GetVPCByNameOrID(ctx, spec.Name+"-vpc"); err == nil {
			rec.VPCID, rec.VPCCRN, rec.VPCCreated = v.ID, v.CRN, true
			spec.log("reusing VPC %s", v.Name)
		} else {
			if spec.TransitGateway != "" {
				if err := checkOverlap(ctx, c, spec.TransitGateway, spec.CIDR); err != nil {
					return err
				}
			}
			spec.log("creating VPC %s-vpc (%s in %s)", spec.Name, spec.CIDR, spec.Zone)
			v, err := c.CreateVPC(ctx, ibm.VPCSpec{Name: spec.Name + "-vpc", ResourceGroupID: spec.ResourceGroupID, ManualPrefixes: true})
			if err != nil {
				return err
			}
			rec.VPCID, rec.VPCCRN, rec.VPCCreated = v.ID, v.CRN, true
			if _, err := c.WaitVPCAvailable(ctx, v.ID, 5*time.Minute); err != nil {
				return err
			}
			if _, err := c.CreateAddressPrefix(ctx, v.ID, spec.Name+"-prefix", spec.CIDR, spec.Zone); err != nil {
				return err
			}
		}
	}

	// Public gateway (egress: package installs, image pulls, F5 licensing).
	pgw, created, err := c.EnsurePublicGateway(ctx, spec.Name+"-pgw", rec.VPCID, spec.Zone, spec.ResourceGroupID)
	if err != nil {
		return err
	}
	// In a VPC we created, the gateway is ours even when a re-run finds it
	// already there; otherwise Teardown would leave it and the VPC delete fails.
	rec.PublicGatewayID, rec.PGWCreated = pgw.ID, created || rec.VPCCreated

	// Subnet.
	subnets, err := c.ListSubnets(ctx, rec.VPCID)
	if err != nil {
		return err
	}
	for _, sn := range subnets {
		if sn.Name == spec.Name+"-subnet" {
			rec.SubnetID = sn.ID
		}
	}
	if rec.SubnetID == "" {
		spec.log("creating subnet %s-subnet", spec.Name)
		sn, err := c.CreateSubnet(ctx, ibm.SubnetSpec{Name: spec.Name + "-subnet", VPCID: rec.VPCID, Zone: spec.Zone,
			CIDR: spec.CIDR, PublicGatewayID: pgw.ID, ResourceGroupID: spec.ResourceGroupID})
		if err != nil {
			return err
		}
		rec.SubnetID = sn.ID
		if _, err := c.WaitSubnetAvailable(ctx, sn.ID, 5*time.Minute); err != nil {
			return err
		}
	}

	// Security group.
	sg, err := c.FindSecurityGroupByName(ctx, rec.VPCID, spec.Name+"-sg")
	if err != nil && !ibm.IsNotFound(err) {
		return err
	}
	if sg == nil {
		if sg, err = c.CreateSecurityGroup(ctx, spec.Name+"-sg", rec.VPCID, spec.ResourceGroupID); err != nil {
			return err
		}
		rules := []ibm.SecurityGroupRule{{Direction: "outbound", Protocol: "all", RemoteCIDR: "0.0.0.0/0"}}
		for _, r := range spec.Inbound {
			rules = append(rules, ibm.SecurityGroupRule{Direction: "inbound", Protocol: "tcp", PortMin: r.PortMin, PortMax: r.PortMax, RemoteCIDR: r.CIDR})
		}
		for _, r := range rules {
			if _, err := c.AddSecurityGroupRule(ctx, sg.ID, r); err != nil {
				return err
			}
		}
	}
	rec.SecurityGroupID = sg.ID

	// Floating IP first, so its address can go into certificates.
	if spec.FloatingIP {
		fip, err := c.FindFloatingIPByName(ctx, spec.Name+"-fip")
		if err != nil && !ibm.IsNotFound(err) {
			return err
		}
		if fip == nil {
			if fip, err = c.ReserveFloatingIP(ctx, spec.Name+"-fip", spec.Zone, spec.ResourceGroupID); err != nil {
				return err
			}
		}
		rec.FloatingIPID, rec.FloatingIP = fip.ID, fip.Address
	}
	userData := spec.UserData
	if spec.BeforeInstance != nil {
		if userData, err = spec.BeforeInstance(rec.FloatingIP); err != nil {
			return err
		}
	}

	// Instance.
	inst, err := c.FindInstanceByName(ctx, spec.Name)
	if err != nil && !ibm.IsNotFound(err) {
		return err
	}
	if inst == nil {
		img, err := c.LatestPublicImage(ctx, ibm.UbuntuMinimalAMD64)
		if err != nil {
			return err
		}
		var keys []string
		if spec.SSHKeyName != "" {
			k, err := c.GetSSHKeyByName(ctx, spec.SSHKeyName)
			if err != nil {
				return fmt.Errorf("SSH key %s: %w", spec.SSHKeyName, err)
			}
			keys = []string{k.ID}
		}
		spec.log("creating instance %s (%s, %s)", spec.Name, spec.Profile, img.Name)
		if inst, err = c.CreateInstance(ctx, ibm.InstanceSpec{
			Name: spec.Name, Profile: spec.Profile, Zone: spec.Zone, VPCID: rec.VPCID, ImageID: img.ID,
			SubnetID: rec.SubnetID, SecurityGroupIDs: []string{sg.ID}, SSHKeyIDs: keys,
			UserData: userData, BootVolumeGB: spec.BootVolumeGB, ResourceGroupID: spec.ResourceGroupID,
		}); err != nil {
			return err
		}
	}
	rec.InstanceID = inst.ID
	if inst, err = c.WaitInstanceRunning(ctx, inst.ID, 10*time.Minute); err != nil {
		return err
	}
	rec.PrivateIP = inst.PrimaryIP
	if rec.FloatingIPID != "" {
		if _, err := c.BindFloatingIP(ctx, rec.FloatingIPID, inst); err != nil {
			return err
		}
	}

	// Transit gateway.
	if spec.TransitGateway != "" {
		rec.TGWID = spec.TransitGateway
		conn, err := c.FindConnectionForVPC(ctx, spec.TransitGateway, rec.VPCCRN)
		if err != nil && !ibm.IsNotFound(err) {
			return err
		}
		if conn == nil {
			spec.log("attaching %s to the transit gateway", spec.Name+"-vpc")
			if conn, err = c.CreateVPCConnection(ctx, spec.TransitGateway, spec.Name, rec.VPCCRN); err != nil {
				return err
			}
		}
		if rec.VPCCreated {
			rec.TGWConnectionID = conn.ID // ours to remove with the VPC
		}
		if _, err := c.WaitConnectionAttached(ctx, spec.TransitGateway, conn.ID, 10*time.Minute); err != nil {
			return err
		}
	}
	return nil
}

func checkOverlap(ctx context.Context, c *ibm.Client, gatewayID, cidr string) error {
	attached, err := c.GatewayAttachedPrefixes(ctx, gatewayID)
	if err != nil {
		return err
	}
	conflicts, err := ibm.FindPrefixConflicts([]string{cidr}, attached)
	if err != nil {
		return err
	}
	if len(conflicts) > 0 {
		var ss []string
		for _, x := range conflicts {
			ss = append(ss, x.String())
		}
		return fmt.Errorf("%s overlaps a VPC already on the transit gateway (the gateway would blackhole one of them):\n  %s", cidr, strings.Join(ss, "\n  "))
	}
	return nil
}

// Teardown deletes what rec says was created, in dependency order, and waits
// for each to go. It is safe to re-run.
func Teardown(ctx context.Context, c *ibm.Client, rec *Record, log func(string, ...any)) error {
	if log == nil {
		log = func(string, ...any) {}
	}
	var errs []error
	step := func(what string, fn func() error) {
		if err := fn(); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", what, err))
		}
	}
	const wait = 10 * time.Minute
	if rec.TGWConnectionID != "" && rec.TGWID != "" {
		log("detaching from the transit gateway")
		step("transit gateway connection", func() error { return c.DeleteConnection(ctx, rec.TGWID, rec.TGWConnectionID, wait) })
	}
	if rec.InstanceID != "" {
		log("deleting instance")
		step("instance", func() error { return c.DeleteAndWait(ctx, ibm.KindInstance, rec.InstanceID, wait) })
	}
	if rec.FloatingIPID != "" {
		step("floating IP", func() error { return c.DeleteAndWait(ctx, ibm.KindFloatingIP, rec.FloatingIPID, wait) })
	}
	if rec.SubnetID != "" {
		log("deleting subnet")
		step("subnet", func() error { return c.DeleteAndWait(ctx, ibm.KindSubnet, rec.SubnetID, wait) })
	}
	if rec.SecurityGroupID != "" {
		step("security group", func() error { return c.DeleteAndWait(ctx, ibm.KindSecurityGroup, rec.SecurityGroupID, wait) })
	}
	if rec.PGWCreated && rec.PublicGatewayID != "" {
		step("public gateway", func() error { return c.DeleteAndWait(ctx, ibm.KindPublicGateway, rec.PublicGatewayID, wait) })
	}
	if rec.VPCCreated && rec.VPCID != "" && len(errs) == 0 {
		log("deleting VPC")
		step("VPC", func() error { return c.DeleteAndWait(ctx, ibm.KindVPC, rec.VPCID, wait) })
	}
	return errors.Join(errs...)
}
