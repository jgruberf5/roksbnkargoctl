package ibm

import (
	"context"
	"fmt"
	"net"
	"sort"
	"strings"
)

// PrefixConflict is one overlapping pair: a CIDR we intend to use against one
// already present on a VPC attached to the gateway.
type PrefixConflict struct {
	Intended string // the prefix the new VPC would take
	Existing string // the prefix already attached
	VPCName  string // the VPC that holds it
}

func (p PrefixConflict) String() string {
	return fmt.Sprintf("%s overlaps %s on VPC %q", p.Intended, p.Existing, p.VPCName)
}

// cidrsOverlap reports whether two CIDR blocks share any address; either
// containing the other counts (a /18 inside a /16 is the case that bites).
func cidrsOverlap(a, b string) (bool, error) {
	_, na, err := net.ParseCIDR(strings.TrimSpace(a))
	if err != nil {
		return false, fmt.Errorf("parsing %q: %w", a, err)
	}
	_, nb, err := net.ParseCIDR(strings.TrimSpace(b))
	if err != nil {
		return false, fmt.Errorf("parsing %q: %w", b, err)
	}
	return na.Contains(nb.IP) || nb.Contains(na.IP), nil
}

// FindPrefixConflicts compares the prefixes a new VPC intends to take against
// the prefixes of VPCs already attached to a gateway (attached maps a VPC
// display name to its prefixes) and returns every overlapping pair, in
// intended order then VPC-name order.
//
// Two attached VPCs with overlapping prefixes make the gateway's routing
// ambiguous and it drops traffic for one of them, silently — so this runs
// before a VPC is created or attached.
func FindPrefixConflicts(intended []string, attached map[string][]string) ([]PrefixConflict, error) {
	names := make([]string, 0, len(attached))
	for n := range attached {
		names = append(names, n)
	}
	sort.Strings(names)
	var conflicts []PrefixConflict
	for _, want := range intended {
		for _, vpcName := range names {
			for _, got := range attached[vpcName] {
				overlap, err := cidrsOverlap(want, got)
				if err != nil {
					return nil, err
				}
				if overlap {
					conflicts = append(conflicts, PrefixConflict{Intended: want, Existing: got, VPCName: vpcName})
				}
			}
		}
	}
	return conflicts, nil
}

// GatewayAttachedPrefixes returns, for every VPC attached to a transit gateway,
// its address prefixes keyed by "<connection name> (<region>)". Each VPC's
// region comes from its CRN, so VPCs in other regions are read from their own
// regional endpoint. The result feeds FindPrefixConflicts.
func (c *Client) GatewayAttachedPrefixes(ctx context.Context, gatewayID string) (map[string][]string, error) {
	conns, err := c.ListConnections(ctx, gatewayID)
	if err != nil {
		return nil, err
	}
	out := map[string][]string{}
	for _, conn := range conns {
		if !strings.EqualFold(conn.NetworkType, "vpc") || tgwConnectionDeparting(conn.Status) {
			continue
		}
		region, vpcID, ok := parseVPCCRN(conn.NetworkID)
		if !ok {
			return nil, fmt.Errorf("connection %s: cannot parse VPC CRN %q", conn.ID, conn.NetworkID)
		}
		prefixes, err := c.WithRegion(region).ListAddressPrefixes(ctx, vpcID)
		if err != nil {
			return nil, fmt.Errorf("reading prefixes of VPC %s (connection %s): %w", vpcID, conn.Name, err)
		}
		key := fmt.Sprintf("%s (%s)", conn.Name, region)
		for _, p := range prefixes {
			out[key] = append(out[key], p.CIDR)
		}
	}
	return out, nil
}

// parseVPCCRN extracts region and VPC id from
// crn:v1:bluemix:public:is:<region>:a/<account>::vpc:<id>.
func parseVPCCRN(crn string) (region, id string, ok bool) {
	parts := strings.Split(crn, ":")
	if len(parts) < 10 || parts[0] != "crn" || parts[8] != "vpc" {
		return "", "", false
	}
	return parts[5], parts[9], parts[5] != "" && parts[9] != ""
}
