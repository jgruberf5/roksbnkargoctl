// Package vsitest is an in-memory IBM Cloud for tests of package vsi and the
// commands built on it. It implements vsi.API with the semantics vsi relies on:
// names are looked up exactly, creates are idempotent only where the real API
// is (they are not), and a delete removes the object at once.
package vsitest

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jgruberf5/roksbnkargoctl/internal/ibm"
	"github.com/jgruberf5/roksbnkargoctl/internal/vsi"
)

// Cloud is one region of fake VPC infrastructure plus the account's transit
// gateways. Its fields may be read and seeded directly between calls.
type Cloud struct {
	mu     sync.Mutex
	region string
	seq    int

	VPCs      map[string]*ibm.VPC
	Subnets   map[string]*ibm.Subnet
	PGWs      map[string]*ibm.PublicGateway
	SGs       map[string]*ibm.SecurityGroup
	FIPs      map[string]*ibm.FloatingIP
	Instances map[string]*ibm.Instance
	Gateways  map[string]*Gateway
	// DetachTimeouts is the timeout of every DeleteConnection call.
	DetachTimeouts []time.Duration
	SSHKeys        map[string]*ibm.SSHKey

	// InstanceSpecs is what each instance was created with, by id.
	InstanceSpecs map[string]ibm.InstanceSpec
	// Deleted lists every delete, as "<kind>/<id>", in order.
	Deleted []string
	// Created lists every create, as "<kind>/<name>", in order.
	Created []string
	// FailCreateInstance, when set, is returned by CreateInstance.
	FailCreateInstance error
}

// Gateway is a fake transit gateway and its connections.
type Gateway struct {
	ibm.TransitGateway
	Conns []ibm.TGWConnection
	// Prefixes is what GatewayAttachedPrefixes reports, by VPC name.
	Prefixes map[string][]string
}

var _ vsi.API = (*Cloud)(nil)

// New returns an empty cloud in region with one transit gateway, "tgw-1".
func New(region string) *Cloud {
	c := &Cloud{
		region: region, VPCs: map[string]*ibm.VPC{}, Subnets: map[string]*ibm.Subnet{},
		PGWs: map[string]*ibm.PublicGateway{}, SGs: map[string]*ibm.SecurityGroup{}, FIPs: map[string]*ibm.FloatingIP{},
		Instances: map[string]*ibm.Instance{}, Gateways: map[string]*Gateway{}, SSHKeys: map[string]*ibm.SSHKey{},
		InstanceSpecs: map[string]ibm.InstanceSpec{},
	}
	c.Gateways["tgw-1"] = &Gateway{TransitGateway: ibm.TransitGateway{ID: "tgw-1", Name: "tgw-one"}, Prefixes: map[string][]string{}}
	return c
}

func (c *Cloud) id(kind string) string {
	c.seq++
	return fmt.Sprintf("%s-%d", kind, c.seq)
}

func notFound(what string) error {
	return &ibm.APIError{Method: "GET", URL: what, StatusCode: http.StatusNotFound, Body: "not found"}
}

// Names lists every object's name, sorted, for "what is left" assertions.
func (c *Cloud) Names() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for _, v := range c.VPCs {
		out = append(out, "vpc:"+v.Name)
	}
	for _, v := range c.Subnets {
		out = append(out, "subnet:"+v.Name)
	}
	for _, v := range c.PGWs {
		out = append(out, "pgw:"+v.Name)
	}
	for _, v := range c.SGs {
		out = append(out, "sg:"+v.Name)
	}
	for _, v := range c.FIPs {
		out = append(out, "fip:"+v.Name)
	}
	for _, v := range c.Instances {
		out = append(out, "instance:"+v.Name)
	}
	for _, g := range c.Gateways {
		for _, cn := range g.Conns {
			out = append(out, "conn:"+g.ID+":"+cn.Name)
		}
	}
	sort.Strings(out)
	return out
}

// Region implements vsi.API.
func (c *Cloud) Region() string { return c.region }

// ---- VPCs ----------------------------------------------------------------------

// ListVPCs implements vsi.API.
func (c *Cloud) ListVPCs(context.Context) ([]ibm.VPC, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []ibm.VPC
	for _, v := range c.VPCs {
		out = append(out, *v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// GetVPCByNameOrID implements vsi.API.
func (c *Cloud) GetVPCByNameOrID(ctx context.Context, nameOrID string) (*ibm.VPC, error) {
	vpcs, _ := c.ListVPCs(ctx)
	var m []ibm.VPC
	for _, v := range vpcs {
		if v.ID == nameOrID {
			return &v, nil
		}
		if v.Name == nameOrID {
			m = append(m, v)
		}
	}
	if len(m) == 1 {
		return &m[0], nil
	}
	return nil, fmt.Errorf("no VPC named or with id %q in %s", nameOrID, c.region)
}

// AddVPC seeds a VPC and returns it.
func (c *Cloud) AddVPC(name string) *ibm.VPC {
	c.mu.Lock()
	defer c.mu.Unlock()
	id := c.id("vpc")
	v := &ibm.VPC{ID: id, Name: name, CRN: "crn:" + id, Status: "available"}
	c.VPCs[id] = v
	return v
}

// CreateVPC implements vsi.API.
func (c *Cloud) CreateVPC(_ context.Context, spec ibm.VPCSpec) (*ibm.VPC, error) {
	v := c.AddVPC(spec.Name)
	c.mu.Lock()
	c.Created = append(c.Created, "vpc/"+spec.Name)
	c.mu.Unlock()
	cp := *v
	return &cp, nil
}

// WaitVPCAvailable implements vsi.API.
func (c *Cloud) WaitVPCAvailable(_ context.Context, id string, _ time.Duration) (*ibm.VPC, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.VPCs[id]
	if !ok {
		return nil, notFound("vpc " + id)
	}
	cp := *v
	return &cp, nil
}

// CreateAddressPrefix implements vsi.API.
func (c *Cloud) CreateAddressPrefix(_ context.Context, vpcID, name, cidr, zone string) (*ibm.AddressPrefix, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return &ibm.AddressPrefix{ID: c.id("prefix"), Name: name, CIDR: cidr, Zone: zone}, nil
}

// ---- public gateways --------------------------------------------------------------

// GetPublicGateway implements vsi.API.
func (c *Cloud) GetPublicGateway(_ context.Context, id string) (*ibm.PublicGateway, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	g, ok := c.PGWs[id]
	if !ok {
		return nil, notFound("pgw " + id)
	}
	cp := *g
	return &cp, nil
}

// FindPublicGateway implements vsi.API.
func (c *Cloud) FindPublicGateway(_ context.Context, vpcID, zone string) (*ibm.PublicGateway, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, g := range c.PGWs {
		if g.VPCID == vpcID && g.Zone == zone {
			cp := *g
			return &cp, nil
		}
	}
	return nil, nil
}

// AddPublicGateway seeds a public gateway.
func (c *Cloud) AddPublicGateway(name, vpcID, zone string) *ibm.PublicGateway {
	c.mu.Lock()
	defer c.mu.Unlock()
	g := &ibm.PublicGateway{ID: c.id("pgw"), Name: name, VPCID: vpcID, Zone: zone, Status: "available"}
	c.PGWs[g.ID] = g
	return g
}

// EnsurePublicGateway implements vsi.API.
func (c *Cloud) EnsurePublicGateway(ctx context.Context, name, vpcID, zone, _ string) (*ibm.PublicGateway, bool, error) {
	if g, _ := c.FindPublicGateway(ctx, vpcID, zone); g != nil {
		return g, false, nil
	}
	g := c.AddPublicGateway(name, vpcID, zone)
	c.mu.Lock()
	c.Created = append(c.Created, "pgw/"+name)
	c.mu.Unlock()
	cp := *g
	return &cp, true, nil
}

// ---- subnets ------------------------------------------------------------------------

// ListSubnets implements vsi.API.
func (c *Cloud) ListSubnets(_ context.Context, vpcID string) ([]ibm.Subnet, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []ibm.Subnet
	for _, s := range c.Subnets {
		if s.VPCID == vpcID {
			out = append(out, *s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// AddSubnet seeds a subnet.
func (c *Cloud) AddSubnet(name, vpcID, zone, pgwID string) *ibm.Subnet {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := &ibm.Subnet{ID: c.id("subnet"), Name: name, VPCID: vpcID, Zone: zone, PublicGatewayID: pgwID, Status: "available"}
	c.Subnets[s.ID] = s
	return s
}

// CreateSubnet implements vsi.API.
func (c *Cloud) CreateSubnet(_ context.Context, spec ibm.SubnetSpec) (*ibm.Subnet, error) {
	s := c.AddSubnet(spec.Name, spec.VPCID, spec.Zone, spec.PublicGatewayID)
	c.mu.Lock()
	s.CIDR = spec.CIDR
	c.Created = append(c.Created, "subnet/"+spec.Name)
	c.mu.Unlock()
	cp := *s
	return &cp, nil
}

// WaitSubnetAvailable implements vsi.API.
func (c *Cloud) WaitSubnetAvailable(_ context.Context, id string, _ time.Duration) (*ibm.Subnet, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s, ok := c.Subnets[id]
	if !ok {
		return nil, notFound("subnet " + id)
	}
	cp := *s
	return &cp, nil
}

// ---- security groups ---------------------------------------------------------------

// FindSecurityGroupByName implements vsi.API.
func (c *Cloud) FindSecurityGroupByName(_ context.Context, vpcID, name string) (*ibm.SecurityGroup, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, g := range c.SGs {
		if g.VPCID == vpcID && g.Name == name {
			cp := *g
			return &cp, nil
		}
	}
	return nil, nil
}

// AddSecurityGroup seeds a security group.
func (c *Cloud) AddSecurityGroup(name, vpcID string) *ibm.SecurityGroup {
	c.mu.Lock()
	defer c.mu.Unlock()
	g := &ibm.SecurityGroup{ID: c.id("sg"), Name: name, VPCID: vpcID}
	c.SGs[g.ID] = g
	return g
}

// CreateSecurityGroup implements vsi.API.
func (c *Cloud) CreateSecurityGroup(_ context.Context, name, vpcID, _ string) (*ibm.SecurityGroup, error) {
	g := c.AddSecurityGroup(name, vpcID)
	c.mu.Lock()
	c.Created = append(c.Created, "sg/"+name)
	c.mu.Unlock()
	cp := *g
	return &cp, nil
}

// AddSecurityGroupRule implements vsi.API.
func (c *Cloud) AddSecurityGroupRule(context.Context, string, ibm.SecurityGroupRule) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.id("rule"), nil
}

// ---- floating IPs ----------------------------------------------------------------------

// FindFloatingIPByName implements vsi.API.
func (c *Cloud) FindFloatingIPByName(_ context.Context, name string) (*ibm.FloatingIP, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, f := range c.FIPs {
		if f.Name == name {
			cp := *f
			return &cp, nil
		}
	}
	return nil, nil
}

// AddFloatingIP seeds a floating IP.
func (c *Cloud) AddFloatingIP(name, zone string) *ibm.FloatingIP {
	c.mu.Lock()
	defer c.mu.Unlock()
	f := &ibm.FloatingIP{ID: c.id("fip"), Name: name, Zone: zone, Address: fmt.Sprintf("169.60.0.%d", c.seq), Status: "available"}
	c.FIPs[f.ID] = f
	return f
}

// ReserveFloatingIP implements vsi.API.
func (c *Cloud) ReserveFloatingIP(_ context.Context, name, zone, _ string) (*ibm.FloatingIP, error) {
	f := c.AddFloatingIP(name, zone)
	c.mu.Lock()
	c.Created = append(c.Created, "fip/"+name)
	c.mu.Unlock()
	cp := *f
	return &cp, nil
}

// BindFloatingIP implements vsi.API.
func (c *Cloud) BindFloatingIP(_ context.Context, id string, inst *ibm.Instance) (*ibm.FloatingIP, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	f, ok := c.FIPs[id]
	if !ok {
		return nil, notFound("fip " + id)
	}
	f.TargetID = inst.ID
	cp := *f
	return &cp, nil
}

// ---- instances ----------------------------------------------------------------------------

// LatestPublicImage implements vsi.API.
func (c *Cloud) LatestPublicImage(context.Context, *regexp.Regexp) (*ibm.Image, error) {
	return &ibm.Image{ID: "img-1", Name: "ibm-ubuntu-24-04-1-minimal-amd64-1"}, nil
}

// GetSSHKeyByName implements vsi.API.
func (c *Cloud) GetSSHKeyByName(_ context.Context, name string) (*ibm.SSHKey, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, k := range c.SSHKeys {
		if k.Name == name {
			cp := *k
			return &cp, nil
		}
	}
	return nil, fmt.Errorf("no SSH key %q", name)
}

// GetInstance implements vsi.API.
func (c *Cloud) GetInstance(_ context.Context, id string) (*ibm.Instance, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	in, ok := c.Instances[id]
	if !ok {
		return nil, notFound("instance " + id)
	}
	cp := *in
	return &cp, nil
}

// FindInstanceByName implements vsi.API.
func (c *Cloud) FindInstanceByName(_ context.Context, name string) (*ibm.Instance, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, in := range c.Instances {
		if in.Name == name {
			cp := *in
			return &cp, nil
		}
	}
	return nil, nil
}

// AddInstance seeds a running instance.
func (c *Cloud) AddInstance(name, vpcID, zone string) *ibm.Instance {
	c.mu.Lock()
	defer c.mu.Unlock()
	in := &ibm.Instance{ID: c.id("instance"), Name: name, VPCID: vpcID, Zone: zone, Status: "running", PrimaryIP: fmt.Sprintf("10.248.0.%d", 4+c.seq%200)}
	c.Instances[in.ID] = in
	return in
}

// CreateInstance implements vsi.API.
func (c *Cloud) CreateInstance(_ context.Context, spec ibm.InstanceSpec) (*ibm.Instance, error) {
	if c.FailCreateInstance != nil {
		return nil, c.FailCreateInstance
	}
	in := c.AddInstance(spec.Name, spec.VPCID, spec.Zone)
	c.mu.Lock()
	c.InstanceSpecs[in.ID] = spec
	c.Created = append(c.Created, "instance/"+spec.Name)
	c.mu.Unlock()
	cp := *in
	return &cp, nil
}

// WaitInstanceRunning implements vsi.API.
func (c *Cloud) WaitInstanceRunning(ctx context.Context, id string, _ time.Duration) (*ibm.Instance, error) {
	return c.GetInstance(ctx, id)
}

// ---- transit gateways ---------------------------------------------------------------------

// AddGateway seeds a transit gateway.
func (c *Cloud) AddGateway(id, name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Gateways[id] = &Gateway{TransitGateway: ibm.TransitGateway{ID: id, Name: name}, Prefixes: map[string][]string{}}
}

// ListTransitGateways implements vsi.API.
func (c *Cloud) ListTransitGateways(context.Context) ([]ibm.TransitGateway, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []ibm.TransitGateway
	for _, g := range c.Gateways {
		out = append(out, g.TransitGateway)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (c *Cloud) gw(id string) (*Gateway, error) {
	g, ok := c.Gateways[id]
	if !ok {
		return nil, notFound("transit gateway " + id)
	}
	return g, nil
}

// GatewayAttachedPrefixes implements vsi.API.
func (c *Cloud) GatewayAttachedPrefixes(_ context.Context, id string) (map[string][]string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	g, err := c.gw(id)
	if err != nil {
		return nil, err
	}
	return g.Prefixes, nil
}

// ListConnections implements vsi.API.
func (c *Cloud) ListConnections(_ context.Context, id string) ([]ibm.TGWConnection, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	g, err := c.gw(id)
	if err != nil {
		return nil, err
	}
	return append([]ibm.TGWConnection(nil), g.Conns...), nil
}

// FindConnectionForVPC implements vsi.API.
func (c *Cloud) FindConnectionForVPC(ctx context.Context, id, vpcCRN string) (*ibm.TGWConnection, error) {
	conns, err := c.ListConnections(ctx, id)
	if err != nil {
		return nil, err
	}
	for _, cn := range conns {
		if strings.EqualFold(cn.NetworkID, vpcCRN) {
			return &cn, nil
		}
	}
	return nil, nil
}

// AddConnection seeds a connection on gateway id.
func (c *Cloud) AddConnection(id, name, vpcCRN string) ibm.TGWConnection {
	c.mu.Lock()
	defer c.mu.Unlock()
	cn := ibm.TGWConnection{ID: c.id("conn"), Name: name, NetworkType: "vpc", NetworkID: vpcCRN, Status: "attached"}
	c.Gateways[id].Conns = append(c.Gateways[id].Conns, cn)
	return cn
}

// CreateVPCConnection implements vsi.API.
func (c *Cloud) CreateVPCConnection(_ context.Context, id, name, vpcCRN string) (*ibm.TGWConnection, error) {
	c.mu.Lock()
	_, err := c.gw(id)
	c.mu.Unlock()
	if err != nil {
		return nil, err
	}
	cn := c.AddConnection(id, name, vpcCRN)
	c.mu.Lock()
	c.Created = append(c.Created, "conn/"+name)
	c.mu.Unlock()
	return &cn, nil
}

// WaitConnectionAttached implements vsi.API.
func (c *Cloud) WaitConnectionAttached(ctx context.Context, id, connID string, _ time.Duration) (*ibm.TGWConnection, error) {
	conns, err := c.ListConnections(ctx, id)
	if err != nil {
		return nil, err
	}
	for _, cn := range conns {
		if cn.ID == connID {
			return &cn, nil
		}
	}
	return nil, notFound("connection " + connID)
}

// DeleteConnection implements vsi.API.
func (c *Cloud) DeleteConnection(_ context.Context, id, connID string, timeout time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.DetachTimeouts = append(c.DetachTimeouts, timeout)
	g, err := c.gw(id)
	if err != nil {
		return err
	}
	for i, cn := range g.Conns {
		if cn.ID == connID {
			g.Conns = append(g.Conns[:i], g.Conns[i+1:]...)
			c.Deleted = append(c.Deleted, "conn/"+connID)
			return nil
		}
	}
	return nil
}

// ---- deletes -------------------------------------------------------------------------------

// DeleteAndWait implements vsi.API. Like the real API, a VPC that still holds
// a subnet or is attached to a gateway cannot be deleted.
func (c *Cloud) DeleteAndWait(_ context.Context, kind ibm.VPCKind, id string, _ time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch kind {
	case ibm.KindInstance:
		delete(c.Instances, id)
	case ibm.KindFloatingIP:
		delete(c.FIPs, id)
	case ibm.KindSubnet:
		delete(c.Subnets, id)
	case ibm.KindPublicGateway:
		delete(c.PGWs, id)
	case ibm.KindSecurityGroup:
		delete(c.SGs, id)
	case ibm.KindVPC:
		v, ok := c.VPCs[id]
		if !ok {
			break
		}
		for _, s := range c.Subnets {
			if s.VPCID == id {
				return fmt.Errorf("VPC %s still has subnet %s", id, s.Name)
			}
		}
		for _, g := range c.Gateways {
			for _, cn := range g.Conns {
				if cn.NetworkID == v.CRN {
					return fmt.Errorf("VPC %s is still attached to %s", id, g.ID)
				}
			}
		}
		delete(c.VPCs, id)
	default:
		return fmt.Errorf("vsitest: unsupported kind %s", kind)
	}
	c.Deleted = append(c.Deleted, string(kind)+"/"+id)
	return nil
}
