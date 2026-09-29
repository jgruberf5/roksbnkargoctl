package ibm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
)

// VPC is a regional VPC.
type VPC struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	CRN    string `json:"crn"`
	Status string `json:"status"`
	// DefaultSecurityGroup is the group IBM creates with the VPC.
	DefaultSecurityGroup struct {
		ID string `json:"id"`
	} `json:"default_security_group"`
	ResourceGroup struct {
		ID string `json:"id"`
	} `json:"resource_group"`
}

// GetVPC fetches a VPC by id in the client's region.
func (c *Client) GetVPC(ctx context.Context, id string) (*VPC, error) {
	u, err := c.vpcURLf("/vpcs/%s", url.PathEscape(id))
	if err != nil {
		return nil, err
	}
	var v VPC
	if err := c.do(ctx, http.MethodGet, u, nil, nil, &v); err != nil {
		return nil, fmt.Errorf("getting VPC %s: %w", id, err)
	}
	return &v, nil
}

// ListVPCs returns every VPC in the client's region.
func (c *Client) ListVPCs(ctx context.Context) ([]VPC, error) {
	u, err := c.vpcURLf("/vpcs?limit=100")
	if err != nil {
		return nil, err
	}
	var out []VPC
	err = c.listPaged(ctx, u, func(raw []byte) (string, error) {
		var page struct {
			nextHref
			VPCs []VPC `json:"vpcs"`
		}
		if err := json.Unmarshal(raw, &page); err != nil {
			return "", err
		}
		out = append(out, page.VPCs...)
		return page.href(), nil
	})
	return out, err
}

// GetVPCByNameOrID resolves a VPC in the client's region by id or name. Errors
// on no match and on an ambiguous name.
func (c *Client) GetVPCByNameOrID(ctx context.Context, nameOrID string) (*VPC, error) {
	vpcs, err := c.ListVPCs(ctx)
	if err != nil {
		return nil, err
	}
	var matches []VPC
	for _, v := range vpcs {
		if v.ID == nameOrID {
			return &v, nil
		}
		if v.Name == nameOrID {
			matches = append(matches, v)
		}
	}
	switch len(matches) {
	case 1:
		return &matches[0], nil
	case 0:
		return nil, fmt.Errorf("no VPC named or with id %q in %s", nameOrID, c.region)
	default:
		return nil, fmt.Errorf("VPC name %q is ambiguous in %s (%d matches); pass the id", nameOrID, c.region, len(matches))
	}
}

// Subnet is a VPC subnet.
type Subnet struct {
	ID                     string
	Name                   string
	CRN                    string
	CIDR                   string
	Zone                   string
	VPCID                  string
	PublicGatewayID        string
	Status                 string
	AvailableIPv4AddrCount int
	TotalIPv4AddrCount     int
}

type subnetWire struct {
	ID                        string  `json:"id"`
	Name                      string  `json:"name"`
	CRN                       string  `json:"crn"`
	IPv4CIDRBlock             string  `json:"ipv4_cidr_block"`
	Zone                      nameRef `json:"zone"`
	VPC                       ref     `json:"vpc"`
	PublicGateway             *ref    `json:"public_gateway"`
	Status                    string  `json:"status"`
	AvailableIPv4AddressCount int     `json:"available_ipv4_address_count"`
	TotalIPv4AddressCount     int     `json:"total_ipv4_address_count"`
}

func (w subnetWire) toSubnet() Subnet {
	s := Subnet{
		ID: w.ID, Name: w.Name, CRN: w.CRN, CIDR: w.IPv4CIDRBlock, Zone: w.Zone.Name,
		VPCID: w.VPC.ID, Status: w.Status,
		AvailableIPv4AddrCount: w.AvailableIPv4AddressCount, TotalIPv4AddrCount: w.TotalIPv4AddressCount,
	}
	if w.PublicGateway != nil {
		s.PublicGatewayID = w.PublicGateway.ID
	}
	return s
}

// ListSubnets returns the subnets of one VPC (every subnet in the region when
// vpcID is empty).
func (c *Client) ListSubnets(ctx context.Context, vpcID string) ([]Subnet, error) {
	path := "/subnets?limit=100"
	if vpcID != "" {
		path += "&vpc.id=" + url.QueryEscape(vpcID)
	}
	u, err := c.vpcURLf("%s", path)
	if err != nil {
		return nil, err
	}
	var out []Subnet
	err = c.listPaged(ctx, u, func(raw []byte) (string, error) {
		var page struct {
			nextHref
			Subnets []subnetWire `json:"subnets"`
		}
		if err := json.Unmarshal(raw, &page); err != nil {
			return "", err
		}
		for _, s := range page.Subnets {
			// Filter client-side too: the vpc.id query is a server-side
			// convenience, the result must not depend on it.
			if vpcID == "" || s.VPC.ID == vpcID {
				out = append(out, s.toSubnet())
			}
		}
		return page.href(), nil
	})
	return out, err
}

// GetSubnet fetches a subnet by id.
func (c *Client) GetSubnet(ctx context.Context, id string) (*Subnet, error) {
	u, err := c.vpcURLf("/subnets/%s", url.PathEscape(id))
	if err != nil {
		return nil, err
	}
	var w subnetWire
	if err := c.do(ctx, http.MethodGet, u, nil, nil, &w); err != nil {
		return nil, fmt.Errorf("getting subnet %s: %w", id, err)
	}
	s := w.toSubnet()
	return &s, nil
}

// AddressPrefix is one of a VPC's per-zone address prefixes.
type AddressPrefix struct {
	ID   string
	Name string
	CIDR string
	Zone string
}

type addressPrefixWire struct {
	ID   string  `json:"id"`
	Name string  `json:"name"`
	CIDR string  `json:"cidr"`
	Zone nameRef `json:"zone"`
}

// ListAddressPrefixes returns a VPC's address prefixes. A transit gateway
// routes on these, not on subnets.
func (c *Client) ListAddressPrefixes(ctx context.Context, vpcID string) ([]AddressPrefix, error) {
	u, err := c.vpcURLf("/vpcs/%s/address_prefixes?limit=100", url.PathEscape(vpcID))
	if err != nil {
		return nil, err
	}
	var out []AddressPrefix
	err = c.listPaged(ctx, u, func(raw []byte) (string, error) {
		var page struct {
			nextHref
			AddressPrefixes []addressPrefixWire `json:"address_prefixes"`
		}
		if err := json.Unmarshal(raw, &page); err != nil {
			return "", err
		}
		for _, p := range page.AddressPrefixes {
			out = append(out, AddressPrefix{ID: p.ID, Name: p.Name, CIDR: p.CIDR, Zone: p.Zone.Name})
		}
		return page.href(), nil
	})
	return out, err
}

// ListZones returns the available zone names of region, sorted. region "" means
// the client's region.
func (c *Client) ListZones(ctx context.Context, region string) ([]string, error) {
	if region == "" {
		region = c.region
	}
	if region == "" {
		return nil, errors.New("no region given for ListZones")
	}
	u, err := c.vpcURLf("/regions/%s/zones", url.PathEscape(region))
	if err != nil {
		return nil, err
	}
	var out struct {
		Zones []struct {
			Name   string `json:"name"`
			Status string `json:"status"`
		} `json:"zones"`
	}
	if err := c.do(ctx, http.MethodGet, u, nil, nil, &out); err != nil {
		return nil, fmt.Errorf("listing zones of %s: %w", region, err)
	}
	var zones []string
	for _, z := range out.Zones {
		if z.Status == "" || strings.EqualFold(z.Status, "available") {
			zones = append(zones, z.Name)
		}
	}
	sort.Strings(zones)
	return zones, nil
}
