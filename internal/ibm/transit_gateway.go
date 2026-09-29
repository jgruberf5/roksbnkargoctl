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

// TransitGateway is a Transit Gateway (a global resource).
type TransitGateway struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	CRN      string `json:"crn"`
	Location string `json:"location"`
	Status   string `json:"status"`
	Global   bool   `json:"global"`
}

// TGWConnection is one network attached to a gateway.
type TGWConnection struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	NetworkType string `json:"network_type"`
	// NetworkID is the attached network's CRN (the VPC CRN for type "vpc").
	NetworkID string `json:"network_id"`
	// Status: attached | pending | deleting | detaching | failed | suspended.
	Status string `json:"status"`
}

// ListTransitGateways returns every Transit Gateway in the account.
func (c *Client) ListTransitGateways(ctx context.Context) ([]TransitGateway, error) {
	var out []TransitGateway
	err := c.listPaged(ctx, c.tgwURLf("/transit_gateways?limit=50"), func(raw []byte) (string, error) {
		var page struct {
			nextHref
			TransitGateways []TransitGateway `json:"transit_gateways"`
		}
		if err := json.Unmarshal(raw, &page); err != nil {
			return "", err
		}
		out = append(out, page.TransitGateways...)
		return page.href(), nil
	})
	if err != nil {
		return nil, fmt.Errorf("listing transit gateways: %w", err)
	}
	return out, nil
}

// ResolveTransitGateway finds the gateway whose id or name is nameOrID. An
// ambiguous name is an error rather than an arbitrary pick.
func (c *Client) ResolveTransitGateway(ctx context.Context, nameOrID string) (*TransitGateway, error) {
	gws, err := c.ListTransitGateways(ctx)
	if err != nil {
		return nil, err
	}
	return matchTransitGateway(gws, nameOrID)
}

func matchTransitGateway(gws []TransitGateway, nameOrID string) (*TransitGateway, error) {
	for _, g := range gws {
		if g.ID == nameOrID {
			return &g, nil
		}
	}
	var matches []TransitGateway
	for _, g := range gws {
		if g.Name == nameOrID {
			matches = append(matches, g)
		}
	}
	switch len(matches) {
	case 1:
		return &matches[0], nil
	case 0:
		return nil, fmt.Errorf("no Transit Gateway named or with id %q in this account", nameOrID)
	default:
		return nil, fmt.Errorf("transit gateway %q is ambiguous: matched %d gateways; pass the gateway id", nameOrID, len(matches))
	}
}

// ListConnections returns the networks attached to a gateway.
func (c *Client) ListConnections(ctx context.Context, gatewayID string) ([]TGWConnection, error) {
	var out []TGWConnection
	u := c.tgwURLf("/transit_gateways/%s/connections?limit=50", url.PathEscape(gatewayID))
	err := c.listPaged(ctx, u, func(raw []byte) (string, error) {
		var page struct {
			nextHref
			Connections []TGWConnection `json:"connections"`
		}
		if err := json.Unmarshal(raw, &page); err != nil {
			return "", err
		}
		out = append(out, page.Connections...)
		return page.href(), nil
	})
	if err != nil {
		return nil, fmt.Errorf("listing connections of transit gateway %s: %w", gatewayID, err)
	}
	return out, nil
}

// GetConnection fetches one connection.
func (c *Client) GetConnection(ctx context.Context, gatewayID, connectionID string) (*TGWConnection, error) {
	var conn TGWConnection
	u := c.tgwURLf("/transit_gateways/%s/connections/%s", url.PathEscape(gatewayID), url.PathEscape(connectionID))
	if err := c.do(ctx, http.MethodGet, u, nil, nil, &conn); err != nil {
		return nil, err
	}
	return &conn, nil
}

// FindConnectionForVPC returns the gateway's connection to the VPC with CRN
// vpcCRN, or (nil, nil) when the VPC is not attached.
func (c *Client) FindConnectionForVPC(ctx context.Context, gatewayID, vpcCRN string) (*TGWConnection, error) {
	conns, err := c.ListConnections(ctx, gatewayID)
	if err != nil {
		return nil, err
	}
	for _, conn := range conns {
		if strings.EqualFold(conn.NetworkID, vpcCRN) {
			return &conn, nil
		}
	}
	return nil, nil
}

// CreateVPCConnection attaches a VPC (by CRN) to a gateway. The connection
// starts "pending"; use WaitConnectionAttached.
func (c *Client) CreateVPCConnection(ctx context.Context, gatewayID, name, vpcCRN string) (*TGWConnection, error) {
	if gatewayID == "" || vpcCRN == "" {
		return nil, errors.New("CreateVPCConnection needs a gateway id and a VPC CRN")
	}
	body := map[string]any{"network_type": "vpc", "network_id": vpcCRN}
	if name != "" {
		body["name"] = name
	}
	var conn TGWConnection
	u := c.tgwURLf("/transit_gateways/%s/connections", url.PathEscape(gatewayID))
	if err := c.do(ctx, http.MethodPost, u, nil, body, &conn); err != nil {
		return nil, fmt.Errorf("attaching VPC %s to transit gateway %s: %w", vpcCRN, gatewayID, err)
	}
	return &conn, nil
}

// WaitConnectionAttached waits for a connection to reach "attached". "failed"
// is an error.
func (c *Client) WaitConnectionAttached(ctx context.Context, gatewayID, connectionID string, timeout time.Duration) (*TGWConnection, error) {
	var conn *TGWConnection
	err := c.poll(ctx, timeout, "transit gateway connection "+connectionID+" to attach", func() (bool, error) {
		got, err := c.GetConnection(ctx, gatewayID, connectionID)
		if err != nil {
			return false, err
		}
		conn = got
		switch strings.ToLower(got.Status) {
		case "attached":
			return true, nil
		case "failed":
			return false, fmt.Errorf("transit gateway connection %s failed to attach", connectionID)
		}
		return false, nil
	})
	return conn, err
}

// tgwConnectionArriving — still attaching. IBM refuses a DELETE from this
// state with 409 invalid_state (roksbnkctl#87).
func tgwConnectionArriving(status string) bool {
	return strings.EqualFold(strings.TrimSpace(status), "pending")
}

// tgwConnectionDeparting — on its way out, under either of IBM's spellings. A
// second DELETE is pointless and risks a non-404 rejection (roksbnkctl#85).
func tgwConnectionDeparting(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "deleting", "detaching":
		return true
	}
	return false
}

// DeleteConnection detaches a connection and waits until the gateway no longer
// lists it. Settle logic ported from roksbnkctl's teardown:
//
//   - a connection still "pending" is waited out first, because IBM refuses a
//     DELETE from that state;
//   - one already "deleting"/"detaching" is not deleted again, only waited for;
//   - otherwise DELETE (404 is success), then poll until it is gone.
//
// timeout bounds each of the two waits. Deletion is asynchronous: the DELETE
// returns while the connection sits in "deleting", and whatever depends on it
// being gone (deleting the VPC, or the gateway) fails until it is.
func (c *Client) DeleteConnection(ctx context.Context, gatewayID, connectionID string, timeout time.Duration) error {
	var status string
	err := c.poll(ctx, timeout, "transit gateway connection "+connectionID+" to settle before delete", func() (bool, error) {
		conn, err := c.GetConnection(ctx, gatewayID, connectionID)
		if IsNotFound(err) {
			status = "gone"
			return true, nil
		}
		if err != nil {
			return false, err
		}
		status = conn.Status
		return !tgwConnectionArriving(conn.Status), nil
	})
	if err != nil {
		return err
	}
	if status == "gone" {
		return nil
	}
	if !tgwConnectionDeparting(status) {
		u := c.tgwURLf("/transit_gateways/%s/connections/%s", url.PathEscape(gatewayID), url.PathEscape(connectionID))
		if err := c.del(ctx, u); err != nil {
			return fmt.Errorf("detaching connection %s from transit gateway %s: %w", connectionID, gatewayID, err)
		}
	}
	return c.WaitConnectionGone(ctx, gatewayID, connectionID, timeout)
}

// WaitConnectionGone polls until the connection returns 404.
func (c *Client) WaitConnectionGone(ctx context.Context, gatewayID, connectionID string, timeout time.Duration) error {
	return c.poll(ctx, timeout, "transit gateway connection "+connectionID+" to detach", func() (bool, error) {
		_, err := c.GetConnection(ctx, gatewayID, connectionID)
		if IsNotFound(err) {
			return true, nil
		}
		return false, err
	})
}
