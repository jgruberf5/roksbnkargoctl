package ibm

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
)

func cosInstanceCRN(guid string) string {
	return "crn:v1:bluemix:public:cloud-object-storage:global:a/acct-1:" + guid + "::"
}

// Every live COS instance in every group, across pages, sorted; other services
// and removed instances are left out.
func TestListServiceInstances(t *testing.T) {
	f, c := newFake(t)
	f.mux.HandleFunc("GET /rc/v2/resource_instances", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("name") != "" {
			http.Error(w, "unexpected name filter", http.StatusBadRequest)
			return
		}
		if r.URL.Query().Get("start") == "" {
			writeJSON(w, map[string]any{
				"resources": []map[string]string{
					{"name": "zeta", "crn": cosInstanceCRN("g9"), "guid": "g9", "resource_group_id": "rg-a", "state": "active"},
					{"name": "kms", "crn": "crn:v1:bluemix:public:kms:us-south:a/acct-1:k1::", "guid": "k1", "state": "active"},
				},
				"next_url": "/v2/resource_instances?type=service_instance&start=2",
			})
			return
		}
		writeJSON(w, map[string]any{"resources": []map[string]string{
			{"name": "alpha", "crn": cosInstanceCRN("g2"), "guid": "g2", "resource_group_id": "rg-b", "state": "active"},
			{"name": "gone", "crn": cosInstanceCRN("g3"), "guid": "g3", "state": "pending_reclamation"},
			{"name": "alpha", "crn": cosInstanceCRN("g1"), "guid": "g1", "resource_group_id": "rg-a", "state": "active"},
		}})
	})
	got, err := c.ListServiceInstances(context.Background(), "cloud-object-storage")
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, si := range got {
		ids = append(ids, si.Name+"/"+si.GUID)
	}
	if strings.Join(ids, " ") != "alpha/g1 alpha/g2 zeta/g9" {
		t.Errorf("instances %v", ids)
	}
	if q := f.requests("GET", "/rc/v2/resource_instances")[0].Query; !strings.Contains(q, "type=service_instance") {
		t.Errorf("first query %q", q)
	}
}

func TestMatchServiceInstance(t *testing.T) {
	list := []ServiceInstance{
		{Name: "dup", GUID: "g1", CRN: cosInstanceCRN("g1"), ResourceGroupID: "rg-a"},
		{Name: "dup", GUID: "g2", CRN: cosInstanceCRN("g2"), ResourceGroupID: "rg-b"},
		{Name: "solo", GUID: "g3", CRN: cosInstanceCRN("g3")},
		// A name that is another instance's GUID: the GUID wins.
		{Name: "g3", GUID: "g4", CRN: cosInstanceCRN("g4")},
	}
	for ref, want := range map[string]string{
		"solo": "g3", "g1": "g1", cosInstanceCRN("g2"): "g2", " g3 ": "g3",
	} {
		si, err := MatchServiceInstance(list, ref)
		if err != nil || si.GUID != want {
			t.Errorf("%q: %+v %v, want %s", ref, si, err, want)
		}
	}
	_, err := MatchServiceInstance(list, "dup")
	if err == nil {
		t.Fatal("an ambiguous name matched")
	}
	for _, want := range []string{"ambiguous", "guid g1", "guid g2", "rg-a", "rg-b", cosInstanceCRN("g1")} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("ambiguous error lacks %q: %v", want, err)
		}
	}
	if _, err := MatchServiceInstance(list, "absent"); !errors.Is(err, ErrServiceInstanceNotFound) {
		t.Errorf("absent: %v", err)
	}
	if _, err := MatchServiceInstance(list, " "); err == nil {
		t.Error("empty ref matched")
	}
}
