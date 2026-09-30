package vsi_test

import (
	"context"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jgruberf5/roksbnkargoctl/internal/vsi"
	"github.com/jgruberf5/roksbnkargoctl/internal/vsi/vsitest"
)

func spec(name string) vsi.Spec {
	return vsi.Spec{Name: name, Zone: "us-east-1", CIDR: "10.248.0.0/28", Profile: "bx2-4x16", FloatingIP: true,
		Inbound: []vsi.Rule{{PortMin: 8443, PortMax: 8443, CIDR: "10.0.0.0/8"}}, TransitGateway: "tgw-1"}
}

func kinds(found []vsi.Found) []string {
	var out []string
	for _, f := range found {
		out = append(out, f.Kind+":"+f.Name)
	}
	sort.Strings(out)
	return out
}

// What Provision builds, Discover finds by name alone, and Teardown of the
// discovered record removes all of it.
func TestDiscoverFindsWhatProvisionBuilt(t *testing.T) {
	ctx := context.Background()
	c := vsitest.New("us-east")
	rec, err := vsi.Provision(ctx, c, spec("t-flp"))
	if err != nil {
		t.Fatal(err)
	}
	got, found, err := vsi.Discover(ctx, c, vsi.DiscoverSpec{Name: "t-flp", Zone: "us-east-1"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"VPC:t-flp-vpc", "floating IP:t-flp-fip", "instance:t-flp", "public gateway:t-flp-pgw",
		"security group:t-flp-sg", "subnet:t-flp-subnet", "transit gateway connection:t-flp"}
	if k := kinds(found); !reflect.DeepEqual(k, want) {
		t.Fatalf("found %v\nwant  %v", k, want)
	}
	// The discovered record names the same objects as the one Provision returned.
	got.Zone, got.PrivateIP = rec.Zone, rec.PrivateIP
	if !reflect.DeepEqual(got, rec) {
		t.Errorf("discovered %+v\nprovisioned %+v", got, rec)
	}
	if err := vsi.Teardown(ctx, c, got, nil); err != nil {
		t.Fatal(err)
	}
	if n := c.Names(); len(n) != 0 {
		t.Errorf("left behind: %v", n)
	}
}

// Nothing that is not named exactly as Provision names it is ever found:
// not a longer name, not a prefix, not a connection or instance of the right
// name that belongs to another VPC, not a public gateway Provision adopted.
func TestDiscoverTakesOnlyExactNames(t *testing.T) {
	ctx := context.Background()
	c := vsitest.New("us-east")
	c.AddGateway("tgw-0", "other")
	// Decoys with no FLP at all.
	other := c.AddVPC("t-flp-vpc-old")
	c.AddVPC("xt-flp-vpc")
	c.AddSubnet("t-flp-subnet", other.ID, "us-east-1", "")
	c.AddSecurityGroup("t-flp-sg", other.ID)
	c.AddFloatingIP("t-flp-fip-2", "us-east-1")
	c.AddInstance("t-flp-2", other.ID, "us-east-1")
	c.AddConnection("tgw-0", "t-flp", other.CRN)
	if _, found, err := vsi.Discover(ctx, c, vsi.DiscoverSpec{Name: "t-flp", Zone: "us-east-1"}); err != nil || len(found) != 0 {
		t.Fatalf("decoys only: found %v, err %v", kinds(found), err)
	}

	// Now the FLP's VPC exists, with a gateway it adopted, a subnet of a
	// longer name, and an instance of the exact name that lives elsewhere.
	vpc := c.AddVPC("t-flp-vpc")
	pgw := c.AddPublicGateway("adopted-pgw", vpc.ID, "us-east-1")
	c.AddSubnet("t-flp-subnet", vpc.ID, "us-east-1", pgw.ID)
	c.AddSubnet("t-flp-subnet-2", vpc.ID, "us-east-1", pgw.ID)
	c.AddInstance("t-flp", other.ID, "us-east-1")
	c.AddConnection("tgw-1", "t-flp-2", vpc.CRN)
	rec, found, err := vsi.Discover(ctx, c, vsi.DiscoverSpec{Name: "t-flp", Zone: "us-east-1"})
	if err != nil {
		t.Fatal(err)
	}
	if k, want := kinds(found), []string{"VPC:t-flp-vpc", "subnet:t-flp-subnet"}; !reflect.DeepEqual(k, want) {
		t.Fatalf("found %v, want %v", k, want)
	}
	if rec.PublicGatewayID != "" || rec.InstanceID != "" || rec.TGWConnectionID != "" {
		t.Errorf("record holds something not named exactly: %+v", rec)
	}

	// Two VPCs of the exact name: refuse rather than pick one.
	c.AddVPC("t-flp-vpc")
	if _, _, err := vsi.Discover(ctx, c, vsi.DiscoverSpec{Name: "t-flp"}); err == nil || !strings.Contains(err.Error(), "2 VPCs are named t-flp-vpc") {
		t.Errorf("ambiguous VPC: %v", err)
	}
}

// In a VPC Provision adopted, the VPC is never taken; what Provision named
// inside it is, and so is the gateway connection it made for it (it used to be
// left attached, holding the VPC on the gateway after flp down).
func TestDiscoverInAnExistingVPCLeavesTheVPC(t *testing.T) {
	ctx := context.Background()
	c := vsitest.New("us-east")
	c.AddVPC("shared")
	sp := spec("t-flp")
	sp.ExistingVPC = "shared"
	if _, err := vsi.Provision(ctx, c, sp); err != nil {
		t.Fatal(err)
	}
	rec, found, err := vsi.Discover(ctx, c, vsi.DiscoverSpec{Name: "t-flp", Zone: "us-east-1", ExistingVPC: "shared"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"floating IP:t-flp-fip", "instance:t-flp", "public gateway:t-flp-pgw", "security group:t-flp-sg",
		"subnet:t-flp-subnet", "transit gateway connection:t-flp"}
	if k := kinds(found); !reflect.DeepEqual(k, want) {
		t.Fatalf("found %v, want %v", k, want)
	}
	if rec.VPCCreated {
		t.Fatalf("an adopted VPC must not be deleted: %+v", rec)
	}
	if err := vsi.Teardown(ctx, c, rec, nil); err != nil {
		t.Fatal(err)
	}
	if n, want := c.Names(), []string{"vpc:shared"}; !reflect.DeepEqual(n, want) {
		t.Errorf("left %v, want %v", n, want)
	}
}

// Provision's own record of an adopted VPC holds the connection it created,
// so flp down detaches it; a connection the VPC already had is left alone.
func TestProvisionInAnExistingVPCRecordsOnlyItsOwnConnection(t *testing.T) {
	ctx := context.Background()
	c := vsitest.New("us-east")
	c.AddVPC("shared")
	sp := spec("t-flp")
	sp.ExistingVPC = "shared"
	rec, err := vsi.Provision(ctx, c, sp)
	if err != nil {
		t.Fatal(err)
	}
	if rec.TGWConnectionID == "" {
		t.Fatalf("the connection Provision created is not recorded: %+v", rec)
	}
	if err := vsi.Teardown(ctx, c, rec, nil); err != nil {
		t.Fatal(err)
	}
	if n, want := c.Names(), []string{"vpc:shared"}; !reflect.DeepEqual(n, want) {
		t.Errorf("left %v, want %v", n, want)
	}

	c = vsitest.New("us-east")
	v := c.AddVPC("shared")
	c.AddConnection("tgw-1", "someone-elses", v.CRN)
	if rec, err = vsi.Provision(ctx, c, sp); err != nil {
		t.Fatal(err)
	}
	if rec.TGWConnectionID != "" {
		t.Fatalf("a connection the VPC already had is recorded for removal: %+v", rec)
	}
	if err := vsi.Teardown(ctx, c, rec, nil); err != nil {
		t.Fatal(err)
	}
	if n, want := c.Names(), []string{"conn:tgw-1:someone-elses", "vpc:shared"}; !reflect.DeepEqual(n, want) {
		t.Errorf("left %v, want %v", n, want)
	}
}

// A detach IBM holds in "deleting" for over 10 minutes must not fail flp down:
// Teardown gives the detach longer than that.
func TestTeardownWaitsLongerThanIBMHasHeldADetach(t *testing.T) {
	ctx := context.Background()
	c := vsitest.New("us-east")
	rec, err := vsi.Provision(ctx, c, spec("t-flp"))
	if err != nil {
		t.Fatal(err)
	}
	if err := vsi.Teardown(ctx, c, rec, nil); err != nil {
		t.Fatal(err)
	}
	if len(c.DetachTimeouts) != 1 || c.DetachTimeouts[0] < 20*time.Minute {
		t.Errorf("detach timeouts %v: IBM has held a detach in deleting for more than 10 minutes", c.DetachTimeouts)
	}
}
