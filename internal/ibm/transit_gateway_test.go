package ibm

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestResolveTransitGateway(t *testing.T) {
	f, c := newFake(t)
	f.mux.HandleFunc("GET /tgw/transit_gateways", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("version") == "" {
			http.Error(w, "no version", http.StatusBadRequest)
			return
		}
		// Two pages: the resolver must follow next.href.
		if r.URL.Query().Get("start") == "" {
			writeJSON(w, map[string]any{
				"transit_gateways": []map[string]any{{"id": "gw-1", "name": "shared"}, {"id": "gw-2", "name": "dup"}},
				"next":             map[string]string{"href": f.URL + "/tgw/transit_gateways?start=p2&version=" + tgwAPIVersion},
			})
			return
		}
		writeJSON(w, map[string]any{
			"transit_gateways": []map[string]any{{"id": "gw-3", "name": "dup"}, {"id": "gw-4", "name": "gw-1"}},
		})
	})
	cases := []struct {
		in, wantID, wantErr string
	}{
		{in: "shared", wantID: "gw-1"},
		{in: "gw-3", wantID: "gw-3"},
		// An id wins over another gateway that happens to be NAMED like it.
		{in: "gw-1", wantID: "gw-1"},
		{in: "dup", wantErr: "ambiguous"},
		{in: "nope", wantErr: "no Transit Gateway"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			gw, err := c.ResolveTransitGateway(context.Background(), tc.in)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if gw.ID != tc.wantID {
				t.Errorf("got %s, want %s", gw.ID, tc.wantID)
			}
		})
	}
}

// connSim is a fake connection whose status advances one step per GET.
type connSim struct {
	mu       sync.Mutex
	statuses []string // successive GET results; "404" means gone
	deletes  int
	deleteAt int // index into statuses when DELETE arrived
}

func (s *connSim) next() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.statuses[0]
	if len(s.statuses) > 1 {
		s.statuses = s.statuses[1:]
	}
	return st
}

func TestCreateVPCConnectionAndWait(t *testing.T) {
	f, c := newFake(t)
	sim := &connSim{statuses: []string{"pending", "pending", "attached"}}
	f.mux.HandleFunc("POST /tgw/transit_gateways/gw-1/connections", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		writeJSON(w, map[string]any{"id": "conn-1", "name": "flp", "network_type": "vpc", "status": "pending"})
	})
	f.mux.HandleFunc("GET /tgw/transit_gateways/gw-1/connections/conn-1", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"id": "conn-1", "status": sim.next()})
	})
	crn := "crn:v1:bluemix:public:is:us-south:a/acct-1::vpc:r006-abc"
	conn, err := c.CreateVPCConnection(context.Background(), "gw-1", "flp", crn)
	if err != nil {
		t.Fatal(err)
	}
	body := decodeBody(t, f.requests("POST", "/tgw/transit_gateways/gw-1/connections")[0].Body)
	if body["network_type"] != "vpc" || body["network_id"] != crn || body["name"] != "flp" {
		t.Errorf("body = %v", body)
	}
	got, err := c.WaitConnectionAttached(context.Background(), "gw-1", conn.ID, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "attached" {
		t.Errorf("status = %s", got.Status)
	}
	if n := len(f.requests("GET", "/tgw/transit_gateways/gw-1/connections/conn-1")); n != 3 {
		t.Errorf("polled %d times, want 3", n)
	}
}

func TestWaitConnectionAttachedFailed(t *testing.T) {
	f, c := newFake(t)
	f.mux.HandleFunc("GET /tgw/transit_gateways/gw-1/connections/conn-1", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"id": "conn-1", "status": "failed"})
	})
	if _, err := c.WaitConnectionAttached(context.Background(), "gw-1", "conn-1", time.Second); err == nil {
		t.Fatal("want error for failed connection")
	}
}

func TestDeleteConnectionSettles(t *testing.T) {
	cases := []struct {
		name        string
		statuses    []string
		wantDeletes int
	}{
		// Pending must be waited out: IBM refuses DELETE from pending (409).
		{"pending then attached", []string{"pending", "pending", "attached", "deleting", "404"}, 1},
		{"attached", []string{"attached", "deleting", "404"}, 1},
		// Already departing: wait only, never a second DELETE.
		{"already deleting", []string{"deleting", "deleting", "404"}, 0},
		{"already gone", []string{"404"}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, c := newFake(t)
			sim := &connSim{statuses: tc.statuses}
			var mu sync.Mutex
			last := ""
			f.mux.HandleFunc("GET /tgw/transit_gateways/gw-1/connections/conn-1", func(w http.ResponseWriter, r *http.Request) {
				st := sim.next()
				mu.Lock()
				last = st
				mu.Unlock()
				if st == "404" {
					http.NotFound(w, r)
					return
				}
				writeJSON(w, map[string]any{"id": "conn-1", "status": st})
			})
			f.mux.HandleFunc("DELETE /tgw/transit_gateways/gw-1/connections/conn-1", func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				if last == "pending" {
					http.Error(w, `{"code":"invalid_state"}`, http.StatusConflict)
					return
				}
				w.WriteHeader(http.StatusNoContent)
			})
			if err := c.DeleteConnection(context.Background(), "gw-1", "conn-1", time.Second); err != nil {
				t.Fatal(err)
			}
			if n := len(f.requests("DELETE", "/tgw/transit_gateways/gw-1/connections/conn-1")); n != tc.wantDeletes {
				t.Errorf("DELETEs = %d, want %d", n, tc.wantDeletes)
			}
		})
	}
}
