package ibm

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// VPCKind names a VPC collection that the Delete* helpers remove and WaitGone
// polls. Its value is the collection path.
type VPCKind string

// The kinds roksbnkargoctl creates, in a safe teardown order: an instance
// before its floating IP is released, subnets before their public gateway and
// security groups, everything before the VPC.
const (
	KindInstance      VPCKind = "instances"
	KindFloatingIP    VPCKind = "floating_ips"
	KindSubnet        VPCKind = "subnets"
	KindPublicGateway VPCKind = "public_gateways"
	KindSecurityGroup VPCKind = "security_groups"
	KindSSHKey        VPCKind = "keys"
	KindVPC           VPCKind = "vpcs"
)

// Delete removes one VPC object by id. 404 is success. IBM deletes most kinds
// asynchronously; follow with WaitGone before deleting what it depended on.
func (c *Client) Delete(ctx context.Context, kind VPCKind, id string) error {
	if id == "" {
		return fmt.Errorf("delete %s: empty id", kind)
	}
	u, err := c.vpcURLf("/%s/%s", kind, url.PathEscape(id))
	if err != nil {
		return err
	}
	if err := c.del(ctx, u); err != nil {
		return fmt.Errorf("deleting %s %s: %w", kind, id, err)
	}
	return nil
}

// WaitGone polls until GET returns 404 for the object, or the timeout passes.
func (c *Client) WaitGone(ctx context.Context, kind VPCKind, id string, timeout time.Duration) error {
	u, err := c.vpcURLf("/%s/%s", kind, url.PathEscape(id))
	if err != nil {
		return err
	}
	return c.poll(ctx, timeout, fmt.Sprintf("%s %s to be deleted", kind, id), func() (bool, error) {
		_, err := c.doRaw(ctx, http.MethodGet, u, nil, nil)
		if IsNotFound(err) {
			return true, nil
		}
		return false, err
	})
}

// DeleteAndWait is Delete followed by WaitGone.
func (c *Client) DeleteAndWait(ctx context.Context, kind VPCKind, id string, timeout time.Duration) error {
	if err := c.Delete(ctx, kind, id); err != nil {
		return err
	}
	return c.WaitGone(ctx, kind, id, timeout)
}

// DeleteInstance deletes a VSI (its auto-delete VNI and boot volume go with it).
func (c *Client) DeleteInstance(ctx context.Context, id string) error {
	return c.Delete(ctx, KindInstance, id)
}

// DeleteFloatingIP releases a floating IP (unbinding it if bound).
func (c *Client) DeleteFloatingIP(ctx context.Context, id string) error {
	return c.Delete(ctx, KindFloatingIP, id)
}

// DeleteSubnet deletes a subnet. It fails while anything is still attached.
func (c *Client) DeleteSubnet(ctx context.Context, id string) error {
	return c.Delete(ctx, KindSubnet, id)
}

// DeletePublicGateway deletes a public gateway. It fails while a subnet still
// uses it.
func (c *Client) DeletePublicGateway(ctx context.Context, id string) error {
	return c.Delete(ctx, KindPublicGateway, id)
}

// DeleteSecurityGroup deletes a security group. It fails while a network
// interface still uses it.
func (c *Client) DeleteSecurityGroup(ctx context.Context, id string) error {
	return c.Delete(ctx, KindSecurityGroup, id)
}

// DeleteSSHKey deletes a VPC SSH key.
func (c *Client) DeleteSSHKey(ctx context.Context, id string) error {
	return c.Delete(ctx, KindSSHKey, id)
}

// DeleteVPC deletes a VPC. It fails while any subnet, gateway or non-default
// security group remains.
func (c *Client) DeleteVPC(ctx context.Context, id string) error {
	return c.Delete(ctx, KindVPC, id)
}
