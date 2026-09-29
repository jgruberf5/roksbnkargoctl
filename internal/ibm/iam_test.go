package ibm

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
)

func TestCreatePolicyBodies(t *testing.T) {
	cases := []struct {
		name      string
		profileID string
		roles     []string
		attrs     []PolicyAttribute
		wantRoles []string
		wantAttrs map[string]string
	}{
		{
			name:      "cluster viewer uses serviceInstance",
			profileID: "Profile-abc",
			roles:     []string{"Viewer"},
			attrs:     ClusterPolicyAttributes("cl123"),
			wantRoles: []string{"crn:v1:bluemix:public:iam::::role:Viewer"},
			wantAttrs: map[string]string{"accountId": "acct-1", "serviceName": "containers-kubernetes", "serviceInstance": "cl123"},
		},
		{
			name:      "vpc viewer+editor",
			profileID: "iam-Profile-abc",
			roles:     []string{"Viewer", "Editor"},
			attrs:     VPCPolicyAttributes("r006-vpc"),
			wantRoles: []string{"crn:v1:bluemix:public:iam::::role:Viewer", "crn:v1:bluemix:public:iam::::role:Editor"},
			wantAttrs: map[string]string{"accountId": "acct-1", "serviceName": "is", "vpcId": "r006-vpc"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, c := newFake(t)
			f.mux.HandleFunc("POST /iam/v1/policies", func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusCreated)
				writeJSON(w, map[string]string{"id": "pol-1"})
			})
			id, err := c.CreatePolicy(context.Background(), tc.profileID, tc.roles, tc.attrs)
			if err != nil || id != "pol-1" {
				t.Fatalf("id=%q err=%v", id, err)
			}
			b := decodeBody(t, f.requests("POST", "/iam/v1/policies")[0].Body)
			if b["type"] != "access" {
				t.Errorf("type = %v", b["type"])
			}
			if dig(t, b, "subjects", 0, "attributes", 0, "name") != "iam_id" ||
				dig(t, b, "subjects", 0, "attributes", 0, "value") != "iam-Profile-abc" {
				t.Errorf("subjects = %v", b["subjects"])
			}
			roles := dig(t, b, "roles").([]any)
			if len(roles) != len(tc.wantRoles) {
				t.Fatalf("roles = %v", roles)
			}
			for i, want := range tc.wantRoles {
				if dig(t, roles[i], "role_id") != want {
					t.Errorf("role %d = %v, want %s", i, roles[i], want)
				}
			}
			got := map[string]string{}
			for _, a := range dig(t, b, "resources", 0, "attributes").([]any) {
				got[dig(t, a, "name").(string)] = dig(t, a, "value").(string)
			}
			if len(got) != len(tc.wantAttrs) {
				t.Errorf("attributes = %v, want %v", got, tc.wantAttrs)
			}
			for k, v := range tc.wantAttrs {
				if got[k] != v {
					t.Errorf("attribute %s = %q, want %q", k, got[k], v)
				}
			}
			if _, bad := got["clusterId"]; bad {
				t.Error("clusterId attribute sent; IAM rejects it with HTTP 400")
			}
		})
	}
}

func TestLinkROKSServiceAccount(t *testing.T) {
	f, c := newFake(t)
	f.mux.HandleFunc("POST /iam/v1/profiles/Profile-1/links", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"errors":[{"code":"conflict"}]}`, http.StatusConflict)
	})
	f.mux.HandleFunc("GET /iam/v1/profiles/Profile-1/links", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"links": []map[string]any{
			{"id": "link-other", "cr_type": "ROKS_SA", "link": map[string]string{"crn": "crn:c", "namespace": "other", "name": "f5-cne-controller"}},
			{"id": "link-1", "cr_type": "ROKS_SA", "link": map[string]string{"crn": "crn:c", "namespace": "f5-bnk", "name": "f5-cne-controller"}},
		}})
	})
	id, err := c.LinkROKSServiceAccount(context.Background(), "Profile-1", "crn:c", "f5-bnk", "f5-cne-controller", "cne")
	if err != nil || id != "link-1" {
		t.Fatalf("id=%q err=%v", id, err)
	}
	b := decodeBody(t, f.requests("POST", "/iam/v1/profiles/Profile-1/links")[0].Body)
	if b["cr_type"] != "ROKS_SA" || dig(t, b, "link", "namespace") != "f5-bnk" ||
		dig(t, b, "link", "name") != "f5-cne-controller" || dig(t, b, "link", "crn") != "crn:c" || b["name"] != "cne" {
		t.Errorf("link body = %v", b)
	}
}

func TestTrustedProfileCreateFindDelete(t *testing.T) {
	f, c := newFake(t)
	f.mux.HandleFunc("POST /iam/v1/profiles", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]string{"id": "Profile-1", "iam_id": "iam-Profile-1", "name": "x-f5-cne-controller"})
	})
	f.mux.HandleFunc("GET /iam/v1/profiles", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("account_id") != "acct-1" {
			http.Error(w, "no account", http.StatusBadRequest)
			return
		}
		writeJSON(w, map[string]any{"profiles": []map[string]string{{"id": "Profile-1", "name": "x-f5-cne-controller"}}})
	})
	f.mux.HandleFunc("GET /iam/v1/policies", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("iam_id") != "iam-Profile-1" {
			http.Error(w, "wrong subject", http.StatusBadRequest)
			return
		}
		writeJSON(w, map[string]any{"policies": []map[string]string{{"id": "pol-1"}, {"id": "pol-gone"}}})
	})
	f.mux.HandleFunc("DELETE /iam/v1/policies/pol-1", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	f.mux.HandleFunc("DELETE /iam/v1/policies/pol-gone", http.NotFound)
	f.mux.HandleFunc("DELETE /iam/v1/profiles/Profile-1", http.NotFound)

	ctx := context.Background()
	tp, err := c.CreateTrustedProfile(ctx, "x-f5-cne-controller", "desc")
	if err != nil || tp.IAMID != "iam-Profile-1" {
		t.Fatalf("tp=%+v err=%v", tp, err)
	}
	if b := decodeBody(t, f.requests("POST", "/iam/v1/profiles")[0].Body); b["account_id"] != "acct-1" {
		t.Errorf("create body = %v", b)
	}
	found, err := c.FindTrustedProfileByName(ctx, "x-f5-cne-controller")
	if err != nil || found == nil || found.ID != "Profile-1" {
		t.Fatalf("found=%+v err=%v", found, err)
	}
	if missing, err := c.FindTrustedProfileByName(ctx, "other"); err != nil || missing != nil {
		t.Errorf("missing=%+v err=%v", missing, err)
	}
	if err := c.DeleteTrustedProfile(ctx, "Profile-1"); err != nil {
		t.Fatal(err)
	}
	if len(f.requests("DELETE", "/iam/v1/policies/pol-1")) != 1 || len(f.requests("DELETE", "/iam/v1/profiles/Profile-1")) != 1 {
		t.Error("policies and profile were not both deleted")
	}
	// The account id is fetched once and cached.
	if n := len(f.requests("GET", "/iam/v1/apikeys/details")); n != 1 {
		t.Errorf("apikeys/details called %d times", n)
	}
}

func TestIAMForbiddenIsPermDenied(t *testing.T) {
	f, c := newFake(t)
	f.mux.HandleFunc("POST /iam/v1/profiles", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "forbidden", http.StatusForbidden)
	})
	_, err := c.CreateTrustedProfile(context.Background(), "p", "")
	if !errors.Is(err, ErrIAMPermDenied) {
		t.Fatalf("err = %v, want ErrIAMPermDenied", err)
	}
}

func TestFindServiceInstance(t *testing.T) {
	cosCRN := func(guid string) string {
		return "crn:v1:bluemix:public:cloud-object-storage:global:a/acct-1:" + guid + "::"
	}
	f, c := newFake(t)
	f.mux.HandleFunc("GET /rc/v2/resource_instances", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		all := []map[string]string{
			{"name": "bnk-supply-chain", "crn": cosCRN("g1"), "guid": "g1", "resource_group_id": "rg-default", "state": "active"},
			{"name": "bnk-supply-chain", "crn": cosCRN("g2"), "guid": "g2", "resource_group_id": "rg-work", "state": "active"},
			{"name": "bnk-supply-chain", "crn": cosCRN("g3"), "guid": "g3", "resource_group_id": "rg-work", "state": "removed"},
			{"name": "bnk-supply-chain", "crn": "crn:v1:bluemix:public:kms:us-south:a/acct-1:k1::", "resource_group_id": "rg-kms", "state": "active"},
			{"name": "single", "crn": cosCRN("g4"), "guid": "g4", "resource_group_id": "rg-work", "state": "active"},
		}
		var out []map[string]string
		for _, r := range all {
			if r["name"] == q.Get("name") && (q.Get("resource_group_id") == "" || q.Get("resource_group_id") == r["resource_group_id"]) {
				out = append(out, r)
			}
		}
		// Paginate one item per page with a RELATIVE next_url, as the
		// Resource Controller does.
		start := 0
		if s := q.Get("start"); s != "" {
			start = int(s[0] - '0')
		}
		resp := map[string]any{"resources": []map[string]string{}}
		if start < len(out) {
			resp["resources"] = out[start : start+1]
			if start+1 < len(out) {
				nq := r.URL.Query()
				nq.Set("start", string(rune('0'+start+1)))
				resp["next_url"] = "/v2/resource_instances?" + nq.Encode()
			}
		}
		writeJSON(w, resp)
	})
	ctx := context.Background()

	_, err := c.FindServiceInstance(ctx, "bnk-supply-chain", "cloud-object-storage", "")
	if err == nil || !strings.Contains(err.Error(), "ambiguous") || !strings.Contains(err.Error(), "rg-default, rg-work") {
		t.Errorf("unscoped duplicate: err = %v", err)
	}
	si, err := c.FindServiceInstance(ctx, "bnk-supply-chain", "cloud-object-storage", "rg-work")
	if err != nil || si.GUID != "g2" {
		t.Errorf("scoped: si=%+v err=%v", si, err)
	}
	si, err = c.FindServiceInstance(ctx, "single", "cloud-object-storage", "")
	if err != nil || si.CRN != cosCRN("g4") {
		t.Errorf("single: si=%+v err=%v", si, err)
	}
	_, err = c.FindServiceInstance(ctx, "absent", "cloud-object-storage", "")
	if !errors.Is(err, ErrServiceInstanceNotFound) {
		t.Errorf("absent: err = %v", err)
	}
}

func TestResolveResourceGroup(t *testing.T) {
	f, c := newFake(t)
	f.mux.HandleFunc("GET /rc/v2/resource_groups", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"resources": []map[string]string{{"id": "rg1", "name": "default"}, {"id": "rg2", "name": "bnk"}}})
	})
	for in, want := range map[string]string{"default": "rg1", "rg2": "rg2"} {
		id, _, err := c.ResolveResourceGroup(context.Background(), in)
		if err != nil || id != want {
			t.Errorf("%s: id=%q err=%v", in, id, err)
		}
	}
	if _, _, err := c.ResolveResourceGroup(context.Background(), "nope"); err == nil {
		t.Error("unknown group accepted")
	}
}

func TestWithRegionSharesAuthAndChangesVPCHost(t *testing.T) {
	c, err := New("key", "us-south")
	if err != nil {
		t.Fatal(err)
	}
	eu := c.WithRegion("eu-de")
	u, _ := eu.vpcURLf("/vpcs")
	if !strings.HasPrefix(u, "https://eu-de.iaas.cloud.ibm.com/v1/vpcs?version="+vpcAPIVersion) {
		t.Errorf("url = %s", u)
	}
	if eu.auth != c.auth || eu.acct != c.acct || c.region != "us-south" {
		t.Error("WithRegion did not share auth/cache or mutated the original")
	}
	if _, err := New("", "us-south"); err == nil {
		t.Error("empty key accepted")
	}
}
