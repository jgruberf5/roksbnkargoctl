package ibm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// UbuntuMinimalAMD64 matches IBM's stock Ubuntu 24.04 minimal amd64 images,
// e.g. "ibm-ubuntu-24-04-3-minimal-amd64-2".
var UbuntuMinimalAMD64 = regexp.MustCompile(`^ibm-ubuntu-24-04-\d+-minimal-amd64-\d+$`)

// Image is a VPC boot image.
type Image struct {
	ID           string
	Name         string
	CreatedAt    time.Time
	Architecture string
}

// LatestPublicImage returns the newest available public image whose name
// matches pattern. IBM publishes a new minor for each point release and
// deprecates old ones, so pinning an image id goes stale; a pattern does not.
func (c *Client) LatestPublicImage(ctx context.Context, pattern *regexp.Regexp) (*Image, error) {
	if pattern == nil {
		return nil, errors.New("image pattern is nil")
	}
	u, err := c.vpcURLf("/images?limit=100&visibility=public&status=available")
	if err != nil {
		return nil, err
	}
	var best *Image
	err = c.listPaged(ctx, u, func(raw []byte) (string, error) {
		var page struct {
			nextHref
			Images []struct {
				ID              string    `json:"id"`
				Name            string    `json:"name"`
				Status          string    `json:"status"`
				CreatedAt       time.Time `json:"created_at"`
				OperatingSystem struct {
					Architecture string `json:"architecture"`
				} `json:"operating_system"`
			} `json:"images"`
		}
		if err := json.Unmarshal(raw, &page); err != nil {
			return "", err
		}
		for _, im := range page.Images {
			if !pattern.MatchString(im.Name) || (im.Status != "" && im.Status != "available") {
				continue
			}
			if best == nil || im.CreatedAt.After(best.CreatedAt) {
				best = &Image{ID: im.ID, Name: im.Name, CreatedAt: im.CreatedAt, Architecture: im.OperatingSystem.Architecture}
			}
		}
		return page.href(), nil
	})
	if err != nil {
		return nil, fmt.Errorf("listing public images: %w", err)
	}
	if best == nil {
		return nil, fmt.Errorf("no available public image in %s matches %s", c.region, pattern)
	}
	return best, nil
}

// SSHKey is a regional VPC SSH key.
type SSHKey struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	PublicKey   string `json:"public_key"`
	Type        string `json:"type"`
	Fingerprint string `json:"fingerprint"`
}

// GetSSHKeyByName returns the region's SSH key called name, or (nil, nil).
func (c *Client) GetSSHKeyByName(ctx context.Context, name string) (*SSHKey, error) {
	u, err := c.vpcURLf("/keys?limit=100")
	if err != nil {
		return nil, err
	}
	var found *SSHKey
	err = c.listPaged(ctx, u, func(raw []byte) (string, error) {
		var page struct {
			nextHref
			Keys []SSHKey `json:"keys"`
		}
		if err := json.Unmarshal(raw, &page); err != nil {
			return "", err
		}
		for i := range page.Keys {
			if page.Keys[i].Name == name {
				k := page.Keys[i]
				found = &k
				return "", nil
			}
		}
		return page.href(), nil
	})
	return found, err
}

// CreateSSHKey uploads an OpenSSH public key. The type (rsa / ed25519) is
// inferred from the key.
func (c *Client) CreateSSHKey(ctx context.Context, name, publicKeyOpenSSH, resourceGroupID string) (*SSHKey, error) {
	body := map[string]any{
		"name":       name,
		"public_key": strings.TrimSpace(publicKeyOpenSSH),
		"type":       keyTypeFromPublic(publicKeyOpenSSH),
	}
	if resourceGroupID != "" {
		body["resource_group"] = ref{ID: resourceGroupID}
	}
	u, err := c.vpcURLf("/keys")
	if err != nil {
		return nil, err
	}
	var k SSHKey
	if err := c.do(ctx, http.MethodPost, u, nil, body, &k); err != nil {
		return nil, fmt.Errorf("creating SSH key %q in %s: %w", name, c.region, err)
	}
	return &k, nil
}

func keyTypeFromPublic(pub string) string {
	if strings.HasPrefix(strings.TrimSpace(pub), "ssh-rsa") {
		return "rsa"
	}
	return "ed25519"
}

// InstanceSpec describes a VSI to create.
type InstanceSpec struct {
	Name             string
	Profile          string // e.g. "bx2-2x8"
	Zone             string
	VPCID            string
	ImageID          string
	SubnetID         string
	SecurityGroupIDs []string
	SSHKeyIDs        []string
	UserData         string // cloud-init, sent verbatim
	BootVolumeGB     int    // 0 keeps the image's minimum (100 GB)
	ResourceGroupID  string
	// LegacyNetworkInterface creates the primary NIC with the pre-VNI
	// primary_network_interface model instead of a primary_network_attachment
	// with a virtual network interface (the default).
	LegacyNetworkInterface bool
}

// Instance is a VSI with the identifiers needed to bind a floating IP and to
// reach it.
type Instance struct {
	ID     string
	Name   string
	CRN    string
	Status string
	Zone   string
	VPCID  string
	// PrimaryNetworkAttachmentID and PrimaryVNIID are set for a VNI-model
	// instance; PrimaryNetworkInterfaceID for a legacy one. Exactly one model
	// applies.
	PrimaryNetworkAttachmentID string
	PrimaryVNIID               string
	PrimaryNetworkInterfaceID  string
	// PrimaryIP is the private IPv4 address; "0.0.0.0" or empty until the
	// instance has one (use WaitInstanceRunning).
	PrimaryIP string
}

type instanceWire struct {
	ID       string  `json:"id"`
	Name     string  `json:"name"`
	CRN      string  `json:"crn"`
	Status   string  `json:"status"`
	Zone     nameRef `json:"zone"`
	VPC      ref     `json:"vpc"`
	Attached *struct {
		ID        string `json:"id"`
		PrimaryIP struct {
			Address string `json:"address"`
		} `json:"primary_ip"`
		VNI struct {
			ID string `json:"id"`
		} `json:"virtual_network_interface"`
	} `json:"primary_network_attachment"`
	NIC *struct {
		ID        string `json:"id"`
		PrimaryIP struct {
			Address string `json:"address"`
		} `json:"primary_ip"`
		PrimaryIPv4Address string `json:"primary_ipv4_address"`
	} `json:"primary_network_interface"`
}

func (w instanceWire) toInstance() *Instance {
	in := &Instance{ID: w.ID, Name: w.Name, CRN: w.CRN, Status: w.Status, Zone: w.Zone.Name, VPCID: w.VPC.ID}
	switch {
	case w.Attached != nil && w.Attached.ID != "":
		in.PrimaryNetworkAttachmentID = w.Attached.ID
		in.PrimaryVNIID = w.Attached.VNI.ID
		in.PrimaryIP = w.Attached.PrimaryIP.Address
	case w.NIC != nil:
		in.PrimaryNetworkInterfaceID = w.NIC.ID
		in.PrimaryIP = firstNonEmpty(w.NIC.PrimaryIP.Address, w.NIC.PrimaryIPv4Address)
	}
	return in
}

func instanceBody(spec InstanceSpec) (map[string]any, error) {
	switch {
	case spec.Name == "", spec.Profile == "", spec.Zone == "", spec.VPCID == "", spec.ImageID == "", spec.SubnetID == "":
		return nil, errors.New("instance needs a name, profile, zone, VPC, image and subnet")
	}
	sgs := make([]ref, 0, len(spec.SecurityGroupIDs))
	for _, id := range spec.SecurityGroupIDs {
		sgs = append(sgs, ref{ID: id})
	}
	keys := make([]ref, 0, len(spec.SSHKeyIDs))
	for _, id := range spec.SSHKeyIDs {
		keys = append(keys, ref{ID: id})
	}
	b := map[string]any{
		"name":    spec.Name,
		"profile": nameRef{Name: spec.Profile},
		"zone":    nameRef{Name: spec.Zone},
		"vpc":     ref{ID: spec.VPCID},
		"image":   ref{ID: spec.ImageID},
		"keys":    keys,
	}
	if spec.UserData != "" {
		b["user_data"] = spec.UserData
	}
	if spec.ResourceGroupID != "" {
		b["resource_group"] = ref{ID: spec.ResourceGroupID}
	}
	if spec.BootVolumeGB > 0 {
		b["boot_volume_attachment"] = map[string]any{
			"delete_volume_on_instance_delete": true,
			"volume": map[string]any{
				"name":     spec.Name + "-boot",
				"capacity": spec.BootVolumeGB,
				"profile":  nameRef{Name: "general-purpose"},
			},
		}
	}
	if spec.LegacyNetworkInterface {
		nic := map[string]any{"name": "eth0", "subnet": ref{ID: spec.SubnetID}}
		if len(sgs) > 0 {
			nic["security_groups"] = sgs
		}
		b["primary_network_interface"] = nic
	} else {
		vni := map[string]any{
			"name":                      spec.Name + "-vni",
			"subnet":                    ref{ID: spec.SubnetID},
			"auto_delete":               true,
			"allow_ip_spoofing":         false,
			"enable_infrastructure_nat": true,
		}
		if len(sgs) > 0 {
			vni["security_groups"] = sgs
		}
		b["primary_network_attachment"] = map[string]any{
			"name":                      "eth0",
			"virtual_network_interface": vni,
		}
	}
	return b, nil
}

// CreateInstance creates a VSI. It returns as soon as IBM accepts the request;
// use WaitInstanceRunning for the private IP to be final.
func (c *Client) CreateInstance(ctx context.Context, spec InstanceSpec) (*Instance, error) {
	body, err := instanceBody(spec)
	if err != nil {
		return nil, err
	}
	u, err := c.vpcURLf("/instances")
	if err != nil {
		return nil, err
	}
	var w instanceWire
	if err := c.do(ctx, http.MethodPost, u, nil, body, &w); err != nil {
		return nil, fmt.Errorf("creating instance %q: %w", spec.Name, err)
	}
	return w.toInstance(), nil
}

// GetInstance fetches an instance by id.
func (c *Client) GetInstance(ctx context.Context, id string) (*Instance, error) {
	u, err := c.vpcURLf("/instances/%s", url.PathEscape(id))
	if err != nil {
		return nil, err
	}
	var w instanceWire
	if err := c.do(ctx, http.MethodGet, u, nil, nil, &w); err != nil {
		return nil, fmt.Errorf("getting instance %s: %w", id, err)
	}
	return w.toInstance(), nil
}

// FindInstanceByName returns the region's instance called name, or (nil, nil).
func (c *Client) FindInstanceByName(ctx context.Context, name string) (*Instance, error) {
	u, err := c.vpcURLf("/instances?limit=100&name=%s", url.QueryEscape(name))
	if err != nil {
		return nil, err
	}
	var found *Instance
	err = c.listPaged(ctx, u, func(raw []byte) (string, error) {
		var page struct {
			nextHref
			Instances []instanceWire `json:"instances"`
		}
		if err := json.Unmarshal(raw, &page); err != nil {
			return "", err
		}
		for _, w := range page.Instances {
			if w.Name == name {
				found = w.toInstance()
				return "", nil
			}
		}
		return page.href(), nil
	})
	return found, err
}

// WaitInstanceRunning waits for status "running" and a real private IP. A
// "failed" status is an error.
func (c *Client) WaitInstanceRunning(ctx context.Context, id string, timeout time.Duration) (*Instance, error) {
	var in *Instance
	err := c.poll(ctx, timeout, "instance "+id+" to be running", func() (bool, error) {
		got, err := c.GetInstance(ctx, id)
		if err != nil {
			return false, err
		}
		in = got
		if strings.EqualFold(got.Status, "failed") {
			return false, fmt.Errorf("instance %s failed to start", id)
		}
		return strings.EqualFold(got.Status, "running") && got.PrimaryIP != "" && got.PrimaryIP != "0.0.0.0", nil
	})
	return in, err
}

// FloatingIP is a reserved public address.
type FloatingIP struct {
	ID      string
	Name    string
	Address string
	Zone    string
	Status  string
	// TargetID is what it is bound to (a network interface or a virtual
	// network interface id); empty when unbound.
	TargetID string
}

type floatingIPWire struct {
	ID      string  `json:"id"`
	Name    string  `json:"name"`
	Address string  `json:"address"`
	Zone    nameRef `json:"zone"`
	Status  string  `json:"status"`
	Target  *ref    `json:"target"`
}

func (w floatingIPWire) toFIP() *FloatingIP {
	f := &FloatingIP{ID: w.ID, Name: w.Name, Address: w.Address, Zone: w.Zone.Name, Status: w.Status}
	if w.Target != nil {
		f.TargetID = w.Target.ID
	}
	return f
}

// ReserveFloatingIP reserves an unbound floating IP in zone. Reserving before
// the instance exists means the address is known early (for a certificate SAN,
// say); BindFloatingIP attaches it once the instance is up.
func (c *Client) ReserveFloatingIP(ctx context.Context, name, zone, resourceGroupID string) (*FloatingIP, error) {
	body := map[string]any{"zone": nameRef{Name: zone}}
	if name != "" {
		body["name"] = name
	}
	if resourceGroupID != "" {
		body["resource_group"] = ref{ID: resourceGroupID}
	}
	u, err := c.vpcURLf("/floating_ips")
	if err != nil {
		return nil, err
	}
	var w floatingIPWire
	if err := c.do(ctx, http.MethodPost, u, nil, body, &w); err != nil {
		return nil, fmt.Errorf("reserving floating IP in %s: %w", zone, err)
	}
	return w.toFIP(), nil
}

// GetFloatingIP fetches a floating IP by id.
func (c *Client) GetFloatingIP(ctx context.Context, id string) (*FloatingIP, error) {
	u, err := c.vpcURLf("/floating_ips/%s", url.PathEscape(id))
	if err != nil {
		return nil, err
	}
	var w floatingIPWire
	if err := c.do(ctx, http.MethodGet, u, nil, nil, &w); err != nil {
		return nil, fmt.Errorf("getting floating IP %s: %w", id, err)
	}
	return w.toFIP(), nil
}

// FindFloatingIPByName returns the region's floating IP called name, or
// (nil, nil).
func (c *Client) FindFloatingIPByName(ctx context.Context, name string) (*FloatingIP, error) {
	u, err := c.vpcURLf("/floating_ips?limit=100")
	if err != nil {
		return nil, err
	}
	var found *FloatingIP
	err = c.listPaged(ctx, u, func(raw []byte) (string, error) {
		var page struct {
			nextHref
			FloatingIPs []floatingIPWire `json:"floating_ips"`
		}
		if err := json.Unmarshal(raw, &page); err != nil {
			return "", err
		}
		for _, w := range page.FloatingIPs {
			if w.Name == name {
				found = w.toFIP()
				return "", nil
			}
		}
		return page.href(), nil
	})
	return found, err
}

// BindFloatingIP binds a reserved floating IP to the instance's primary
// network interface. Both network models are handled:
//
//   - VNI (primary_network_attachment, the default since API 2024-04-30):
//     PUT /virtual_network_interfaces/{vni}/floating_ips/{fip} — what
//     `ibmcloud is virtual-network-interface-floating-ip-add` does.
//   - legacy (primary_network_interface):
//     PUT /instances/{id}/network_interfaces/{nic}/floating_ips/{fip}.
func (c *Client) BindFloatingIP(ctx context.Context, floatingIPID string, inst *Instance) (*FloatingIP, error) {
	if inst == nil {
		return nil, errors.New("BindFloatingIP: instance is nil")
	}
	var (
		u   string
		err error
	)
	switch {
	case inst.PrimaryVNIID != "":
		u, err = c.vpcURLf("/virtual_network_interfaces/%s/floating_ips/%s",
			url.PathEscape(inst.PrimaryVNIID), url.PathEscape(floatingIPID))
	case inst.PrimaryNetworkInterfaceID != "":
		u, err = c.vpcURLf("/instances/%s/network_interfaces/%s/floating_ips/%s",
			url.PathEscape(inst.ID), url.PathEscape(inst.PrimaryNetworkInterfaceID), url.PathEscape(floatingIPID))
	default:
		return nil, fmt.Errorf("instance %s has neither a virtual network interface nor a network interface to bind to", inst.ID)
	}
	if err != nil {
		return nil, err
	}
	var w floatingIPWire
	if err := c.do(ctx, http.MethodPut, u, nil, nil, &w); err != nil {
		return nil, fmt.Errorf("binding floating IP %s to instance %s: %w", floatingIPID, inst.ID, err)
	}
	return w.toFIP(), nil
}
