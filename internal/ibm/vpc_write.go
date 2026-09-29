package ibm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// VPCSpec describes a VPC to create.
type VPCSpec struct {
	Name            string
	ResourceGroupID string
	// ManualPrefixes creates the VPC with address_prefix_management "manual":
	// no prefixes are added, and the caller adds its own with
	// CreateAddressPrefix. Otherwise IBM assigns its per-zone defaults
	// (10.241.0.0/18, 10.241.64.0/18, 10.241.128.0/18 in most regions), which
	// collide with any other auto-prefixed VPC on the same transit gateway.
	ManualPrefixes bool
}

// CreateVPC creates a VPC in the client's region.
func (c *Client) CreateVPC(ctx context.Context, spec VPCSpec) (*VPC, error) {
	if spec.Name == "" {
		return nil, errors.New("VPC name is empty")
	}
	body := map[string]any{"name": spec.Name}
	if spec.ManualPrefixes {
		body["address_prefix_management"] = "manual"
	} else {
		body["address_prefix_management"] = "auto"
	}
	if spec.ResourceGroupID != "" {
		body["resource_group"] = ref{ID: spec.ResourceGroupID}
	}
	u, err := c.vpcURLf("/vpcs")
	if err != nil {
		return nil, err
	}
	var v VPC
	if err := c.do(ctx, http.MethodPost, u, nil, body, &v); err != nil {
		return nil, fmt.Errorf("creating VPC %q in %s: %w", spec.Name, c.region, err)
	}
	return &v, nil
}

// WaitVPCAvailable waits for a VPC's status to become "available".
func (c *Client) WaitVPCAvailable(ctx context.Context, id string, timeout time.Duration) (*VPC, error) {
	var v *VPC
	err := c.poll(ctx, timeout, "VPC "+id+" to become available", func() (bool, error) {
		got, err := c.GetVPC(ctx, id)
		if err != nil {
			return false, err
		}
		v = got
		if strings.EqualFold(got.Status, "failed") {
			return false, fmt.Errorf("VPC %s failed", id)
		}
		return strings.EqualFold(got.Status, "available"), nil
	})
	return v, err
}

// CreateAddressPrefix adds an address prefix to a VPC in zone.
func (c *Client) CreateAddressPrefix(ctx context.Context, vpcID, name, cidr, zone string) (*AddressPrefix, error) {
	body := map[string]any{"cidr": cidr, "zone": nameRef{Name: zone}}
	if name != "" {
		body["name"] = name
	}
	u, err := c.vpcURLf("/vpcs/%s/address_prefixes", url.PathEscape(vpcID))
	if err != nil {
		return nil, err
	}
	var w addressPrefixWire
	if err := c.do(ctx, http.MethodPost, u, nil, body, &w); err != nil {
		return nil, fmt.Errorf("creating address prefix %s in %s on VPC %s: %w", cidr, zone, vpcID, err)
	}
	return &AddressPrefix{ID: w.ID, Name: w.Name, CIDR: w.CIDR, Zone: w.Zone.Name}, nil
}

// SubnetSpec describes a subnet to create. Exactly one of CIDR and
// AddressCount must be set.
type SubnetSpec struct {
	Name            string
	VPCID           string
	Zone            string
	CIDR            string // ipv4_cidr_block; must sit inside one of the zone's prefixes
	AddressCount    int    // total_ipv4_address_count (a power of two, e.g. 256)
	PublicGatewayID string // optional; attach at creation
	ResourceGroupID string
}

// CreateSubnet creates a subnet.
func (c *Client) CreateSubnet(ctx context.Context, spec SubnetSpec) (*Subnet, error) {
	if spec.VPCID == "" || spec.Zone == "" {
		return nil, errors.New("subnet needs a VPC id and a zone")
	}
	if (spec.CIDR == "") == (spec.AddressCount == 0) {
		return nil, errors.New("subnet needs exactly one of CIDR and AddressCount")
	}
	body := map[string]any{
		"vpc":  ref{ID: spec.VPCID},
		"zone": nameRef{Name: spec.Zone},
	}
	if spec.Name != "" {
		body["name"] = spec.Name
	}
	if spec.CIDR != "" {
		body["ipv4_cidr_block"] = spec.CIDR
	} else {
		body["total_ipv4_address_count"] = spec.AddressCount
	}
	if spec.PublicGatewayID != "" {
		body["public_gateway"] = ref{ID: spec.PublicGatewayID}
	}
	if spec.ResourceGroupID != "" {
		body["resource_group"] = ref{ID: spec.ResourceGroupID}
	}
	u, err := c.vpcURLf("/subnets")
	if err != nil {
		return nil, err
	}
	var w subnetWire
	if err := c.do(ctx, http.MethodPost, u, nil, body, &w); err != nil {
		return nil, fmt.Errorf("creating subnet %q in %s: %w", spec.Name, spec.Zone, err)
	}
	s := w.toSubnet()
	return &s, nil
}

// WaitSubnetAvailable waits for a subnet's status to become "available".
func (c *Client) WaitSubnetAvailable(ctx context.Context, id string, timeout time.Duration) (*Subnet, error) {
	var s *Subnet
	err := c.poll(ctx, timeout, "subnet "+id+" to become available", func() (bool, error) {
		got, err := c.GetSubnet(ctx, id)
		if err != nil {
			return false, err
		}
		s = got
		if strings.EqualFold(got.Status, "failed") {
			return false, fmt.Errorf("subnet %s failed", id)
		}
		return strings.EqualFold(got.Status, "available"), nil
	})
	return s, err
}

// PublicGateway is a zone's outbound gateway.
type PublicGateway struct {
	ID     string
	Name   string
	Zone   string
	VPCID  string
	Status string
}

type publicGatewayWire struct {
	ID     string  `json:"id"`
	Name   string  `json:"name"`
	Zone   nameRef `json:"zone"`
	VPC    ref     `json:"vpc"`
	Status string  `json:"status"`
}

func (w publicGatewayWire) toPGW() *PublicGateway {
	return &PublicGateway{ID: w.ID, Name: w.Name, Zone: w.Zone.Name, VPCID: w.VPC.ID, Status: w.Status}
}

// GetPublicGateway fetches a public gateway by id.
func (c *Client) GetPublicGateway(ctx context.Context, id string) (*PublicGateway, error) {
	u, err := c.vpcURLf("/public_gateways/%s", url.PathEscape(id))
	if err != nil {
		return nil, err
	}
	var w publicGatewayWire
	if err := c.do(ctx, http.MethodGet, u, nil, nil, &w); err != nil {
		return nil, fmt.Errorf("getting public gateway %s: %w", id, err)
	}
	return w.toPGW(), nil
}

// FindPublicGateway returns the VPC's public gateway in zone, or (nil, nil)
// when it has none. A VPC can hold at most one public gateway per zone, so a
// second create fails; callers reuse this one instead.
func (c *Client) FindPublicGateway(ctx context.Context, vpcID, zone string) (*PublicGateway, error) {
	u, err := c.vpcURLf("/public_gateways?limit=100")
	if err != nil {
		return nil, err
	}
	var found *PublicGateway
	err = c.listPaged(ctx, u, func(raw []byte) (string, error) {
		var page struct {
			nextHref
			PublicGateways []publicGatewayWire `json:"public_gateways"`
		}
		if err := json.Unmarshal(raw, &page); err != nil {
			return "", err
		}
		for _, g := range page.PublicGateways {
			if g.VPC.ID == vpcID && g.Zone.Name == zone {
				found = g.toPGW()
				return "", nil
			}
		}
		return page.href(), nil
	})
	return found, err
}

// CreatePublicGateway creates a public gateway for the VPC in zone.
func (c *Client) CreatePublicGateway(ctx context.Context, name, vpcID, zone, resourceGroupID string) (*PublicGateway, error) {
	body := map[string]any{"vpc": ref{ID: vpcID}, "zone": nameRef{Name: zone}}
	if name != "" {
		body["name"] = name
	}
	if resourceGroupID != "" {
		body["resource_group"] = ref{ID: resourceGroupID}
	}
	u, err := c.vpcURLf("/public_gateways")
	if err != nil {
		return nil, err
	}
	var w publicGatewayWire
	if err := c.do(ctx, http.MethodPost, u, nil, body, &w); err != nil {
		return nil, fmt.Errorf("creating public gateway in %s: %w", zone, err)
	}
	return w.toPGW(), nil
}

// EnsurePublicGateway returns the VPC's existing public gateway in zone, or
// creates one. created reports which happened, so teardown deletes only a
// gateway this run made (a reused one may serve subnets that are not ours).
func (c *Client) EnsurePublicGateway(ctx context.Context, name, vpcID, zone, resourceGroupID string) (pgw *PublicGateway, created bool, err error) {
	pgw, err = c.FindPublicGateway(ctx, vpcID, zone)
	if err != nil {
		return nil, false, err
	}
	if pgw != nil {
		return pgw, false, nil
	}
	pgw, err = c.CreatePublicGateway(ctx, name, vpcID, zone, resourceGroupID)
	return pgw, err == nil, err
}

// AttachPublicGateway sets a subnet's public gateway.
func (c *Client) AttachPublicGateway(ctx context.Context, subnetID, publicGatewayID string) error {
	u, err := c.vpcURLf("/subnets/%s/public_gateway", url.PathEscape(subnetID))
	if err != nil {
		return err
	}
	if err := c.do(ctx, http.MethodPut, u, nil, ref{ID: publicGatewayID}, nil); err != nil {
		return fmt.Errorf("attaching public gateway %s to subnet %s: %w", publicGatewayID, subnetID, err)
	}
	return nil
}

// DetachPublicGateway removes a subnet's public gateway (404 is success). A
// public gateway cannot be deleted while any subnet still uses it.
func (c *Client) DetachPublicGateway(ctx context.Context, subnetID string) error {
	u, err := c.vpcURLf("/subnets/%s/public_gateway", url.PathEscape(subnetID))
	if err != nil {
		return err
	}
	return c.del(ctx, u)
}

// SecurityGroup is a VPC security group.
type SecurityGroup struct {
	ID    string
	Name  string
	VPCID string
}

type securityGroupWire struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	VPC  ref    `json:"vpc"`
}

// CreateSecurityGroup creates an empty security group in a VPC. A new group
// has no rules: everything is denied until AddSecurityGroupRule.
func (c *Client) CreateSecurityGroup(ctx context.Context, name, vpcID, resourceGroupID string) (*SecurityGroup, error) {
	body := map[string]any{"vpc": ref{ID: vpcID}}
	if name != "" {
		body["name"] = name
	}
	if resourceGroupID != "" {
		body["resource_group"] = ref{ID: resourceGroupID}
	}
	u, err := c.vpcURLf("/security_groups")
	if err != nil {
		return nil, err
	}
	var w securityGroupWire
	if err := c.do(ctx, http.MethodPost, u, nil, body, &w); err != nil {
		return nil, fmt.Errorf("creating security group %q: %w", name, err)
	}
	return &SecurityGroup{ID: w.ID, Name: w.Name, VPCID: w.VPC.ID}, nil
}

// FindSecurityGroupByName returns the VPC's security group called name, or
// (nil, nil).
func (c *Client) FindSecurityGroupByName(ctx context.Context, vpcID, name string) (*SecurityGroup, error) {
	u, err := c.vpcURLf("/security_groups?limit=100&vpc.id=%s", url.QueryEscape(vpcID))
	if err != nil {
		return nil, err
	}
	var found *SecurityGroup
	err = c.listPaged(ctx, u, func(raw []byte) (string, error) {
		var page struct {
			nextHref
			SecurityGroups []securityGroupWire `json:"security_groups"`
		}
		if err := json.Unmarshal(raw, &page); err != nil {
			return "", err
		}
		for _, g := range page.SecurityGroups {
			if g.VPC.ID == vpcID && g.Name == name {
				found = &SecurityGroup{ID: g.ID, Name: g.Name, VPCID: g.VPC.ID}
				return "", nil
			}
		}
		return page.href(), nil
	})
	return found, err
}

// SecurityGroupRule is one rule to add.
type SecurityGroupRule struct {
	// Direction is "inbound" or "outbound".
	Direction string
	// Protocol is "tcp", "udp", "icmp" or "all".
	Protocol string
	// PortMin/PortMax bound a tcp/udp port range; both zero means every port.
	// PortMax zero with PortMin set means the single port PortMin.
	PortMin, PortMax int
	// RemoteCIDR is the peer ("0.0.0.0/0" for anywhere). Empty means any.
	RemoteCIDR string
}

func (r SecurityGroupRule) body() (map[string]any, error) {
	dir := strings.ToLower(r.Direction)
	if dir != "inbound" && dir != "outbound" {
		return nil, fmt.Errorf("security group rule direction %q: want inbound or outbound", r.Direction)
	}
	proto := strings.ToLower(r.Protocol)
	switch proto {
	case "tcp", "udp", "icmp", "all":
	case "":
		proto = "all"
	default:
		return nil, fmt.Errorf("security group rule protocol %q: want tcp, udp, icmp or all", r.Protocol)
	}
	b := map[string]any{"direction": dir, "ip_version": "ipv4", "protocol": proto}
	if r.PortMin != 0 || r.PortMax != 0 {
		if proto != "tcp" && proto != "udp" {
			return nil, fmt.Errorf("security group rule: ports apply only to tcp and udp, not %q", proto)
		}
		lo, hi := r.PortMin, r.PortMax
		if hi == 0 {
			hi = lo
		}
		if lo < 1 || hi > 65535 || lo > hi {
			return nil, fmt.Errorf("security group rule: invalid port range %d-%d", lo, hi)
		}
		b["port_min"], b["port_max"] = lo, hi
	}
	if r.RemoteCIDR != "" {
		b["remote"] = map[string]string{"cidr_block": r.RemoteCIDR}
	}
	return b, nil
}

// AddSecurityGroupRule adds a rule to a security group and returns the rule id.
func (c *Client) AddSecurityGroupRule(ctx context.Context, securityGroupID string, rule SecurityGroupRule) (string, error) {
	body, err := rule.body()
	if err != nil {
		return "", err
	}
	u, err := c.vpcURLf("/security_groups/%s/rules", url.PathEscape(securityGroupID))
	if err != nil {
		return "", err
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := c.do(ctx, http.MethodPost, u, nil, body, &out); err != nil {
		return "", fmt.Errorf("adding %s %s rule to security group %s: %w", rule.Direction, rule.Protocol, securityGroupID, err)
	}
	return out.ID, nil
}
