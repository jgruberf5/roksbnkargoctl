// Package vsi stands up one Ubuntu VSI in its own VPC, attached to a transit
// gateway, through the IBM Cloud APIs — the shape both optional components
// (the F5 License Proxy and the test Argo CD hub) need. Everything it creates is
// recorded, so Teardown removes exactly that and nothing else.
package vsi

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jgruberf5/roksbnkargoctl/internal/ibm"
)

// API is the part of the IBM Cloud client this package drives, bound to one
// region. *ibm.Client satisfies it; tests substitute an in-memory cloud
// (package vsitest).
type API interface {
	Region() string

	ListVPCs(ctx context.Context) ([]ibm.VPC, error)
	GetVPCByNameOrID(ctx context.Context, nameOrID string) (*ibm.VPC, error)
	CreateVPC(ctx context.Context, spec ibm.VPCSpec) (*ibm.VPC, error)
	WaitVPCAvailable(ctx context.Context, id string, timeout time.Duration) (*ibm.VPC, error)
	CreateAddressPrefix(ctx context.Context, vpcID, name, cidr, zone string) (*ibm.AddressPrefix, error)

	GetPublicGateway(ctx context.Context, id string) (*ibm.PublicGateway, error)
	FindPublicGateway(ctx context.Context, vpcID, zone string) (*ibm.PublicGateway, error)
	EnsurePublicGateway(ctx context.Context, name, vpcID, zone, resourceGroupID string) (*ibm.PublicGateway, bool, error)

	ListSubnets(ctx context.Context, vpcID string) ([]ibm.Subnet, error)
	CreateSubnet(ctx context.Context, spec ibm.SubnetSpec) (*ibm.Subnet, error)
	WaitSubnetAvailable(ctx context.Context, id string, timeout time.Duration) (*ibm.Subnet, error)

	FindSecurityGroupByName(ctx context.Context, vpcID, name string) (*ibm.SecurityGroup, error)
	CreateSecurityGroup(ctx context.Context, name, vpcID, resourceGroupID string) (*ibm.SecurityGroup, error)
	AddSecurityGroupRule(ctx context.Context, securityGroupID string, rule ibm.SecurityGroupRule) (string, error)

	FindFloatingIPByName(ctx context.Context, name string) (*ibm.FloatingIP, error)
	ReserveFloatingIP(ctx context.Context, name, zone, resourceGroupID string) (*ibm.FloatingIP, error)
	BindFloatingIP(ctx context.Context, floatingIPID string, inst *ibm.Instance) (*ibm.FloatingIP, error)

	LatestPublicImage(ctx context.Context, pattern *regexp.Regexp) (*ibm.Image, error)
	GetSSHKeyByName(ctx context.Context, name string) (*ibm.SSHKey, error)
	GetInstance(ctx context.Context, id string) (*ibm.Instance, error)
	FindInstanceByName(ctx context.Context, name string) (*ibm.Instance, error)
	CreateInstance(ctx context.Context, spec ibm.InstanceSpec) (*ibm.Instance, error)
	WaitInstanceRunning(ctx context.Context, id string, timeout time.Duration) (*ibm.Instance, error)

	ListTransitGateways(ctx context.Context) ([]ibm.TransitGateway, error)
	GatewayAttachedPrefixes(ctx context.Context, gatewayID string) (map[string][]string, error)
	ListConnections(ctx context.Context, gatewayID string) ([]ibm.TGWConnection, error)
	FindConnectionForVPC(ctx context.Context, gatewayID, vpcCRN string) (*ibm.TGWConnection, error)
	CreateVPCConnection(ctx context.Context, gatewayID, name, vpcCRN string) (*ibm.TGWConnection, error)
	WaitConnectionAttached(ctx context.Context, gatewayID, connectionID string, timeout time.Duration) (*ibm.TGWConnection, error)
	DeleteConnection(ctx context.Context, gatewayID, connectionID string, timeout time.Duration) error

	DeleteAndWait(ctx context.Context, kind ibm.VPCKind, id string, timeout time.Duration) error
}

var _ API = (*ibm.Client)(nil)

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
func Provision(ctx context.Context, c API, spec Spec) (*Record, error) {
	rec := &Record{Name: spec.Name, Region: c.Region(), Zone: spec.Zone}
	err := provision(ctx, c, &spec, rec)
	return rec, err
}

func provision(ctx context.Context, c API, spec *Spec, rec *Record) error {
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
	// In an adopted VPC, one of the name we give is ours from an earlier run.
	rec.PublicGatewayID, rec.PGWCreated = pgw.ID, created || rec.VPCCreated || pgw.Name == spec.Name+"-pgw"

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
		created := false
		if conn == nil {
			spec.log("attaching %s to the transit gateway", spec.Name+"-vpc")
			if conn, err = c.CreateVPCConnection(ctx, spec.TransitGateway, spec.Name, rec.VPCCRN); err != nil {
				return err
			}
			created = true
		}
		// Ours to remove: the connection of a VPC we created, or one we made
		// for an adopted VPC, now or on an earlier run (it has the name we
		// give). A connection the adopted VPC already had is not.
		if rec.VPCCreated || created || conn.Name == spec.Name {
			rec.TGWConnectionID = conn.ID
		}
		if _, err := c.WaitConnectionAttached(ctx, spec.TransitGateway, conn.ID, 10*time.Minute); err != nil {
			return err
		}
	}
	return nil
}

func checkOverlap(ctx context.Context, c API, gatewayID, cidr string) error {
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

// DetachTimeout bounds each wait of a transit gateway detach. IBM has held a
// connection in "deleting" for more than 10 minutes, which failed flp down
// with the VPC still attached; 30 covers what has been seen.
const DetachTimeout = 30 * time.Minute

// Teardown deletes what rec says was created, in dependency order, and waits
// for each to go. It is safe to re-run.
func Teardown(ctx context.Context, c API, rec *Record, log func(string, ...any)) error {
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
		step("transit gateway connection", func() error {
			return c.DeleteConnection(ctx, rec.TGWID, rec.TGWConnectionID, DetachTimeout)
		})
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

// DiscoverSpec names what Provision built, for a Teardown when its record is
// lost. Everything is matched by the exact names Provision gives
// (<Name>-vpc, -subnet, -pgw, -sg, -fip, instance <Name>, transit gateway
// connection <Name>) and nothing else: a prefix or a near miss is never taken.
type DiscoverSpec struct {
	Name string
	// Zone locates the public gateway when the subnet is already gone.
	Zone string
	// ExistingVPC is the VPC Provision adopted (Spec.ExistingVPC), if any. Its
	// subnet, security group, floating IP and instance are searched inside it.
	// The VPC was not Provision's and is never taken; a public gateway named
	// <Name>-pgw and a gateway connection named <Name> were.
	ExistingVPC string
	// TransitGateway limits the connection search to one gateway (id); empty
	// searches every gateway in the account.
	TransitGateway string
}

// Found is one resource Discover matched.
type Found struct {
	Kind, Name, ID string
}

func (f Found) String() string { return fmt.Sprintf("%-26s %-32s %s", f.Kind, f.Name, f.ID) }

// Discover finds what Provision built for d.Name and returns a Record that
// Teardown removes, and the resources it holds, for the operator to confirm.
func Discover(ctx context.Context, c API, d DiscoverSpec) (*Record, []Found, error) {
	if d.Name == "" {
		return nil, nil, errors.New("vsi: discover needs a name")
	}
	rec := &Record{Name: d.Name, Region: c.Region(), Zone: d.Zone}
	var found []Found
	add := func(kind, name, id string) { found = append(found, Found{kind, name, id}) }

	var vpc *ibm.VPC
	if d.ExistingVPC != "" {
		v, err := c.GetVPCByNameOrID(ctx, d.ExistingVPC)
		if err != nil {
			return nil, nil, fmt.Errorf("VPC %s: %w", d.ExistingVPC, err)
		}
		vpc = v
		rec.VPCID, rec.VPCCRN = v.ID, v.CRN
	} else {
		vpcs, err := c.ListVPCs(ctx)
		if err != nil {
			return nil, nil, err
		}
		var matches []ibm.VPC
		for _, v := range vpcs {
			if v.Name == d.Name+"-vpc" {
				matches = append(matches, v)
			}
		}
		if len(matches) > 1 {
			return nil, nil, fmt.Errorf("%d VPCs are named %s-vpc in %s; delete by hand", len(matches), d.Name, c.Region())
		}
		if len(matches) == 1 {
			vpc = &matches[0]
			rec.VPCID, rec.VPCCRN, rec.VPCCreated = vpc.ID, vpc.CRN, true
			add("VPC", vpc.Name, vpc.ID)
		}
	}

	if vpc != nil {
		subnets, err := c.ListSubnets(ctx, vpc.ID)
		if err != nil {
			return nil, nil, err
		}
		var pgwID string
		for _, sn := range subnets {
			if sn.Name == d.Name+"-subnet" && sn.VPCID == vpc.ID {
				rec.SubnetID, pgwID = sn.ID, sn.PublicGatewayID
				add("subnet", sn.Name, sn.ID)
			}
		}
		// Provision reuses a zone's existing public gateway, whatever its name;
		// only one it named was one it created.
		var pgw *ibm.PublicGateway
		if pgwID != "" {
			if pgw, err = c.GetPublicGateway(ctx, pgwID); err != nil && !ibm.IsNotFound(err) {
				return nil, nil, err
			}
		} else if d.Zone != "" {
			if pgw, err = c.FindPublicGateway(ctx, vpc.ID, d.Zone); err != nil {
				return nil, nil, err
			}
		}
		if pgw != nil && pgw.Name == d.Name+"-pgw" && pgw.VPCID == vpc.ID {
			rec.PublicGatewayID, rec.PGWCreated = pgw.ID, true
			add("public gateway", pgw.Name, pgw.ID)
		}
		// FindSecurityGroupByName, FindFloatingIPByName and FindInstanceByName
		// match the name exactly (internal/ibm).
		sg, err := c.FindSecurityGroupByName(ctx, vpc.ID, d.Name+"-sg")
		if err != nil && !ibm.IsNotFound(err) {
			return nil, nil, err
		}
		if sg != nil {
			rec.SecurityGroupID = sg.ID
			add("security group", sg.Name, sg.ID)
		}
	}

	fip, err := c.FindFloatingIPByName(ctx, d.Name+"-fip")
	if err != nil && !ibm.IsNotFound(err) {
		return nil, nil, err
	}
	if fip != nil {
		rec.FloatingIPID, rec.FloatingIP = fip.ID, fip.Address
		add("floating IP", fip.Name, fip.ID)
	}
	inst, err := c.FindInstanceByName(ctx, d.Name)
	if err != nil && !ibm.IsNotFound(err) {
		return nil, nil, err
	}
	// An instance of that name in another VPC is not the one Provision built,
	// and without the VPC there is none: a VPC cannot be deleted under one.
	if inst != nil && vpc != nil && inst.VPCID == vpc.ID {
		rec.InstanceID, rec.PrivateIP = inst.ID, inst.PrimaryIP
		add("instance", inst.Name, inst.ID)
	}

	// Provision names the connections it creates <Name>, for a VPC it created
	// or one it adopted; a VPC that is gone cannot still be attached.
	if vpc != nil {
		gws := []string{d.TransitGateway}
		if d.TransitGateway == "" {
			all, err := c.ListTransitGateways(ctx)
			if err != nil {
				return nil, nil, err
			}
			gws = gws[:0]
			for _, g := range all {
				gws = append(gws, g.ID)
			}
		}
		for _, gw := range gws {
			conns, err := c.ListConnections(ctx, gw)
			if err != nil {
				return nil, nil, err
			}
			for _, conn := range conns {
				if conn.Name == d.Name && strings.EqualFold(conn.NetworkID, vpc.CRN) && rec.TGWConnectionID == "" {
					rec.TGWID, rec.TGWConnectionID = gw, conn.ID
					add("transit gateway connection", conn.Name, conn.ID)
				}
			}
		}
	}
	return rec, found, nil
}
