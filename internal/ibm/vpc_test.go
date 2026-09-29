package ibm

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

func instanceResponse(vni bool) map[string]any {
	r := map[string]any{
		"id": "inst-1", "name": "flp", "status": "pending",
		"zone": map[string]string{"name": "us-south-1"}, "vpc": map[string]string{"id": "vpc-1"},
	}
	if vni {
		r["primary_network_attachment"] = map[string]any{
			"id":                        "att-1",
			"primary_ip":                map[string]string{"address": "10.243.0.4"},
			"virtual_network_interface": map[string]string{"id": "vni-1"},
		}
	} else {
		r["primary_network_interface"] = map[string]any{
			"id": "nic-1", "primary_ip": map[string]string{"address": "10.243.0.5"},
		}
	}
	return r
}

func TestCreateInstanceBody(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(map[bool]string{false: "vni", true: "legacy"}[legacy], func(t *testing.T) {
			f, c := newFake(t)
			f.mux.HandleFunc("POST /vpc/instances", func(w http.ResponseWriter, r *http.Request) {
				q := r.URL.Query()
				if q.Get("version") != vpcAPIVersion || q.Get("generation") != "2" {
					http.Error(w, "bad version", http.StatusBadRequest)
					return
				}
				w.WriteHeader(http.StatusCreated)
				writeJSON(w, instanceResponse(!legacy))
			})
			spec := InstanceSpec{
				Name: "flp", Profile: "bx2-2x8", Zone: "us-south-1", VPCID: "vpc-1",
				ImageID: "img-1", SubnetID: "sub-1", SecurityGroupIDs: []string{"sg-1", "sg-2"},
				SSHKeyIDs: []string{"key-1"}, UserData: "#cloud-config\nruncmd: [echo hi]\n",
				BootVolumeGB: 120, ResourceGroupID: "rg-1", LegacyNetworkInterface: legacy,
			}
			in, err := c.CreateInstance(context.Background(), spec)
			if err != nil {
				t.Fatal(err)
			}
			b := decodeBody(t, f.requests("POST", "/vpc/instances")[0].Body)
			checks := map[string][2]any{
				"user_data":     {dig(t, b, "user_data"), spec.UserData},
				"profile":       {dig(t, b, "profile", "name"), "bx2-2x8"},
				"zone":          {dig(t, b, "zone", "name"), "us-south-1"},
				"vpc":           {dig(t, b, "vpc", "id"), "vpc-1"},
				"image":         {dig(t, b, "image", "id"), "img-1"},
				"key":           {dig(t, b, "keys", 0, "id"), "key-1"},
				"rg":            {dig(t, b, "resource_group", "id"), "rg-1"},
				"boot capacity": {dig(t, b, "boot_volume_attachment", "volume", "capacity"), float64(120)},
			}
			for name, pair := range checks {
				if pair[0] != pair[1] {
					t.Errorf("%s = %v, want %v", name, pair[0], pair[1])
				}
			}
			if legacy {
				if _, ok := b["primary_network_attachment"]; ok {
					t.Error("legacy body carries primary_network_attachment")
				}
				if dig(t, b, "primary_network_interface", "subnet", "id") != "sub-1" ||
					dig(t, b, "primary_network_interface", "security_groups", 1, "id") != "sg-2" {
					t.Errorf("primary_network_interface = %v", b["primary_network_interface"])
				}
				if in.PrimaryNetworkInterfaceID != "nic-1" || in.PrimaryVNIID != "" || in.PrimaryIP != "10.243.0.5" {
					t.Errorf("instance = %+v", in)
				}
				return
			}
			if _, ok := b["primary_network_interface"]; ok {
				t.Error("VNI body carries primary_network_interface")
			}
			vni := dig(t, b, "primary_network_attachment", "virtual_network_interface")
			if dig(t, vni, "subnet", "id") != "sub-1" || dig(t, vni, "security_groups", 0, "id") != "sg-1" ||
				dig(t, vni, "security_groups", 1, "id") != "sg-2" || dig(t, vni, "auto_delete") != true {
				t.Errorf("virtual_network_interface = %v", vni)
			}
			if in.PrimaryVNIID != "vni-1" || in.PrimaryNetworkAttachmentID != "att-1" || in.PrimaryIP != "10.243.0.4" {
				t.Errorf("instance = %+v", in)
			}
		})
	}
}

func TestCreateInstanceRequiresFields(t *testing.T) {
	_, c := newFake(t)
	if _, err := c.CreateInstance(context.Background(), InstanceSpec{Name: "x"}); err == nil {
		t.Fatal("want error for missing fields")
	}
}

func TestWaitInstanceRunningWaitsForIP(t *testing.T) {
	f, c := newFake(t)
	steps := []map[string]any{
		{"status": "starting", "ip": "0.0.0.0"},
		{"status": "running", "ip": "0.0.0.0"},
		{"status": "running", "ip": "10.243.0.4"},
	}
	i := 0
	f.mux.HandleFunc("GET /vpc/instances/inst-1", func(w http.ResponseWriter, r *http.Request) {
		s := steps[min(i, len(steps)-1)]
		i++
		resp := instanceResponse(true)
		resp["status"] = s["status"]
		resp["primary_network_attachment"].(map[string]any)["primary_ip"] = map[string]any{"address": s["ip"]}
		writeJSON(w, resp)
	})
	in, err := c.WaitInstanceRunning(context.Background(), "inst-1", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if in.PrimaryIP != "10.243.0.4" || i != 3 {
		t.Errorf("ip = %s after %d polls", in.PrimaryIP, i)
	}
}

func TestBindFloatingIP(t *testing.T) {
	cases := []struct {
		name string
		inst *Instance
		path string
	}{
		{"vni", &Instance{ID: "inst-1", PrimaryVNIID: "vni-1", PrimaryNetworkAttachmentID: "att-1"}, "/vpc/virtual_network_interfaces/vni-1/floating_ips/fip-1"},
		{"legacy", &Instance{ID: "inst-1", PrimaryNetworkInterfaceID: "nic-1"}, "/vpc/instances/inst-1/network_interfaces/nic-1/floating_ips/fip-1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, c := newFake(t)
			f.mux.HandleFunc("PUT "+tc.path, func(w http.ResponseWriter, r *http.Request) {
				writeJSON(w, map[string]any{"id": "fip-1", "address": "169.1.2.3", "target": map[string]string{"id": "x"}})
			})
			fip, err := c.BindFloatingIP(context.Background(), "fip-1", tc.inst)
			if err != nil {
				t.Fatal(err)
			}
			if fip.Address != "169.1.2.3" || fip.TargetID != "x" {
				t.Errorf("fip = %+v", fip)
			}
			if n := len(f.requests("PUT", tc.path)); n != 1 {
				t.Errorf("PUT %s called %d times", tc.path, n)
			}
		})
	}
	_, c := newFake(t)
	if _, err := c.BindFloatingIP(context.Background(), "fip-1", &Instance{ID: "i"}); err == nil {
		t.Error("want error for an instance with no interface")
	}
}

func TestDeletes404IsSuccess(t *testing.T) {
	kinds := []VPCKind{KindInstance, KindFloatingIP, KindSubnet, KindPublicGateway, KindSecurityGroup, KindSSHKey, KindVPC}
	for _, k := range kinds {
		t.Run(string(k), func(t *testing.T) {
			f, c := newFake(t)
			f.mux.HandleFunc("DELETE /vpc/"+string(k)+"/gone", http.NotFound)
			f.mux.HandleFunc("DELETE /vpc/"+string(k)+"/busy", func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, "in use", http.StatusConflict)
			})
			f.mux.HandleFunc("GET /vpc/"+string(k)+"/gone", http.NotFound)
			if err := c.Delete(context.Background(), k, "gone"); err != nil {
				t.Errorf("404 delete: %v", err)
			}
			if err := c.DeleteAndWait(context.Background(), k, "gone", time.Second); err != nil {
				t.Errorf("404 delete+wait: %v", err)
			}
			if err := c.Delete(context.Background(), k, "busy"); !IsConflict(err) {
				t.Errorf("409 delete err = %v, want a conflict", err)
			}
		})
	}
	// The typed wrappers hit the right collection.
	f, c := newFake(t)
	f.mux.HandleFunc("DELETE /vpc/keys/k1", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	if err := c.DeleteSSHKey(context.Background(), "k1"); err != nil {
		t.Fatal(err)
	}
	if len(f.requests("DELETE", "/vpc/keys/k1")) != 1 {
		t.Error("DeleteSSHKey did not DELETE /keys/k1")
	}
}

func TestWaitGonePollsUntil404(t *testing.T) {
	f, c := newFake(t)
	n := 0
	f.mux.HandleFunc("GET /vpc/vpcs/v1", func(w http.ResponseWriter, r *http.Request) {
		n++
		if n < 3 {
			writeJSON(w, map[string]string{"id": "v1", "status": "deleting"})
			return
		}
		http.NotFound(w, r)
	})
	if err := c.WaitGone(context.Background(), KindVPC, "v1", time.Second); err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Errorf("polled %d times", n)
	}
}

func TestSecurityGroupRuleBody(t *testing.T) {
	cases := []struct {
		name    string
		rule    SecurityGroupRule
		want    map[string]any
		wantErr bool
	}{
		{"tcp single port", SecurityGroupRule{Direction: "inbound", Protocol: "tcp", PortMin: 8443, RemoteCIDR: "10.0.0.0/8"},
			map[string]any{"direction": "inbound", "protocol": "tcp", "port_min": float64(8443), "port_max": float64(8443), "cidr": "10.0.0.0/8"}, false},
		{"udp range", SecurityGroupRule{Direction: "outbound", Protocol: "udp", PortMin: 53, PortMax: 54},
			map[string]any{"direction": "outbound", "protocol": "udp", "port_min": float64(53), "port_max": float64(54)}, false},
		{"all", SecurityGroupRule{Direction: "outbound", Protocol: "all", RemoteCIDR: "0.0.0.0/0"},
			map[string]any{"direction": "outbound", "protocol": "all", "cidr": "0.0.0.0/0"}, false},
		{"ports on all", SecurityGroupRule{Direction: "inbound", Protocol: "all", PortMin: 22}, nil, true},
		{"bad direction", SecurityGroupRule{Direction: "in", Protocol: "tcp"}, nil, true},
		{"inverted range", SecurityGroupRule{Direction: "inbound", Protocol: "tcp", PortMin: 10, PortMax: 5}, nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, c := newFake(t)
			f.mux.HandleFunc("POST /vpc/security_groups/sg-1/rules", func(w http.ResponseWriter, r *http.Request) {
				writeJSON(w, map[string]string{"id": "rule-1"})
			})
			id, err := c.AddSecurityGroupRule(context.Background(), "sg-1", tc.rule)
			if tc.wantErr {
				if err == nil {
					t.Fatal("want error")
				}
				if len(f.requests("POST", "/vpc/security_groups/sg-1/rules")) != 0 {
					t.Error("invalid rule was sent")
				}
				return
			}
			if err != nil || id != "rule-1" {
				t.Fatalf("id=%q err=%v", id, err)
			}
			b := decodeBody(t, f.requests("POST", "/vpc/security_groups/sg-1/rules")[0].Body)
			for k, v := range tc.want {
				got := b[k]
				if k == "cidr" {
					got = dig(t, b, "remote", "cidr_block")
				}
				if got != v {
					t.Errorf("%s = %v, want %v", k, got, v)
				}
			}
			if _, ok := tc.want["port_min"]; !ok {
				if _, sent := b["port_min"]; sent {
					t.Error("port_min sent for a rule without ports")
				}
			}
		})
	}
}

func TestEnsurePublicGatewayReusesZoneGateway(t *testing.T) {
	f, c := newFake(t)
	f.mux.HandleFunc("GET /vpc/public_gateways", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"public_gateways": []map[string]any{
			{"id": "pgw-other-vpc", "vpc": map[string]string{"id": "vpc-2"}, "zone": map[string]string{"name": "us-south-1"}},
			{"id": "pgw-1", "vpc": map[string]string{"id": "vpc-1"}, "zone": map[string]string{"name": "us-south-1"}},
		}})
	})
	f.mux.HandleFunc("POST /vpc/public_gateways", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"id": "pgw-new", "vpc": map[string]string{"id": "vpc-1"}, "zone": map[string]string{"name": "us-south-2"}})
	})
	pgw, created, err := c.EnsurePublicGateway(context.Background(), "gw", "vpc-1", "us-south-1", "")
	if err != nil || created || pgw.ID != "pgw-1" {
		t.Fatalf("reuse: pgw=%+v created=%v err=%v", pgw, created, err)
	}
	pgw, created, err = c.EnsurePublicGateway(context.Background(), "gw", "vpc-1", "us-south-2", "")
	if err != nil || !created || pgw.ID != "pgw-new" {
		t.Fatalf("create: pgw=%+v created=%v err=%v", pgw, created, err)
	}
	if n := len(f.requests("POST", "/vpc/public_gateways")); n != 1 {
		t.Errorf("POSTs = %d, want 1", n)
	}
}

func TestLatestPublicImage(t *testing.T) {
	f, c := newFake(t)
	f.mux.HandleFunc("GET /vpc/images", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("visibility") != "public" {
			http.Error(w, "want public", http.StatusBadRequest)
			return
		}
		writeJSON(w, map[string]any{"images": []map[string]any{
			{"id": "old", "name": "ibm-ubuntu-24-04-2-minimal-amd64-1", "status": "available", "created_at": "2025-01-01T00:00:00Z"},
			{"id": "new", "name": "ibm-ubuntu-24-04-3-minimal-amd64-2", "status": "available", "created_at": "2025-09-01T00:00:00Z"},
			{"id": "arm", "name": "ibm-ubuntu-24-04-3-minimal-arm64-2", "status": "available", "created_at": "2026-01-01T00:00:00Z"},
			{"id": "dep", "name": "ibm-ubuntu-24-04-4-minimal-amd64-1", "status": "deprecated", "created_at": "2026-02-01T00:00:00Z"},
		}})
	})
	im, err := c.LatestPublicImage(context.Background(), UbuntuMinimalAMD64)
	if err != nil {
		t.Fatal(err)
	}
	if im.ID != "new" {
		t.Errorf("picked %s", im.ID)
	}
}

func TestCreateVPCPrefixMode(t *testing.T) {
	for _, manual := range []bool{true, false} {
		f, c := newFake(t)
		f.mux.HandleFunc("POST /vpc/vpcs", func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, map[string]any{"id": "vpc-1", "crn": "crn:x"})
		})
		if _, err := c.CreateVPC(context.Background(), VPCSpec{Name: "v", ManualPrefixes: manual}); err != nil {
			t.Fatal(err)
		}
		b := decodeBody(t, f.requests("POST", "/vpc/vpcs")[0].Body)
		want := map[bool]string{true: "manual", false: "auto"}[manual]
		if b["address_prefix_management"] != want {
			t.Errorf("manual=%v: address_prefix_management = %v", manual, b["address_prefix_management"])
		}
	}
}

func TestCreateSubnetExactlyOneSizing(t *testing.T) {
	f, c := newFake(t)
	f.mux.HandleFunc("POST /vpc/subnets", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"id": "sub-1", "ipv4_cidr_block": "10.243.0.0/24", "vpc": map[string]string{"id": "vpc-1"}, "zone": map[string]string{"name": "z"}})
	})
	ctx := context.Background()
	if _, err := c.CreateSubnet(ctx, SubnetSpec{VPCID: "vpc-1", Zone: "z", CIDR: "10.243.0.0/24", AddressCount: 256}); err == nil {
		t.Error("both sizings accepted")
	}
	if _, err := c.CreateSubnet(ctx, SubnetSpec{VPCID: "vpc-1", Zone: "z"}); err == nil {
		t.Error("no sizing accepted")
	}
	s, err := c.CreateSubnet(ctx, SubnetSpec{VPCID: "vpc-1", Zone: "z", AddressCount: 256, PublicGatewayID: "pgw-1"})
	if err != nil {
		t.Fatal(err)
	}
	b := decodeBody(t, f.requests("POST", "/vpc/subnets")[0].Body)
	if b["total_ipv4_address_count"] != float64(256) || dig(t, b, "public_gateway", "id") != "pgw-1" {
		t.Errorf("body = %v", b)
	}
	if _, has := b["ipv4_cidr_block"]; has {
		t.Error("ipv4_cidr_block sent with an address count")
	}
	if s.CIDR != "10.243.0.0/24" || s.VPCID != "vpc-1" {
		t.Errorf("subnet = %+v", s)
	}
}

func TestFindPrefixConflicts(t *testing.T) {
	got, err := FindPrefixConflicts(
		[]string{"10.243.0.0/18", "10.250.0.0/24"},
		map[string][]string{"forge": {"10.242.0.0/16"}, "cluster": {"10.243.0.0/16"}, "flp": {"10.250.0.128/25"}},
	)
	if err != nil {
		t.Fatal(err)
	}
	var s []string
	for _, c := range got {
		s = append(s, c.String())
	}
	want := `10.243.0.0/18 overlaps 10.243.0.0/16 on VPC "cluster"|10.250.0.0/24 overlaps 10.250.0.128/25 on VPC "flp"`
	if strings.Join(s, "|") != want {
		t.Errorf("got %q", strings.Join(s, "|"))
	}
	if _, err := FindPrefixConflicts([]string{"nope"}, map[string][]string{"x": {"10.0.0.0/8"}}); err == nil {
		t.Error("bad CIDR accepted")
	}
}

func TestGatewayAttachedPrefixesUsesCRNRegion(t *testing.T) {
	f, c := newFake(t)
	f.mux.HandleFunc("GET /tgw/transit_gateways/gw-1/connections", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"connections": []map[string]any{
			{"id": "c1", "name": "roks", "network_type": "vpc", "status": "attached", "network_id": "crn:v1:bluemix:public:is:eu-de:a/acct-1::vpc:r010-aaa"},
			{"id": "c2", "name": "gone", "network_type": "vpc", "status": "deleting", "network_id": "crn:v1:bluemix:public:is:us-south:a/acct-1::vpc:r006-bbb"},
			{"id": "c3", "name": "dl", "network_type": "directlink", "status": "attached", "network_id": "crn:dl"},
		}})
	})
	f.mux.HandleFunc("GET /vpc/vpcs/r010-aaa/address_prefixes", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"address_prefixes": []map[string]any{{"cidr": "10.243.0.0/18"}}})
	})
	got, err := c.GatewayAttachedPrefixes(context.Background(), "gw-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got["roks (eu-de)"][0] != "10.243.0.0/18" {
		t.Errorf("got %v", got)
	}
}

// Found live: IBM's next.href carries no version parameter, and the VPC API
// answers a version-less page two with 400 missing_version. The fake behaves
// the same way, so this fails if paging stops carrying version/generation.
func TestPagingCarriesVersionToNextPage(t *testing.T) {
	f, c := newFake(t)
	f.mux.HandleFunc("GET /vpc/images", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("version") == "" || q.Get("generation") == "" {
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(w, map[string]any{"errors": []map[string]any{{"code": "missing_version"}}})
			return
		}
		if q.Get("start") == "" {
			writeJSON(w, map[string]any{
				"images": []map[string]any{{"id": "p1", "name": "ibm-ubuntu-24-04-1-minimal-amd64-1", "status": "available", "created_at": "2025-01-01T00:00:00Z"}},
				"next":   map[string]any{"href": f.URL + "/vpc/images?limit=100&start=page2"},
			})
			return
		}
		writeJSON(w, map[string]any{"images": []map[string]any{
			{"id": "p2", "name": "ibm-ubuntu-24-04-5-minimal-amd64-1", "status": "available", "created_at": "2026-05-01T00:00:00Z"}}})
	})
	im, err := c.LatestPublicImage(context.Background(), UbuntuMinimalAMD64)
	if err != nil {
		t.Fatal(err)
	}
	if im.ID != "p2" {
		t.Fatalf("page two was not read: picked %s", im.ID)
	}
}
