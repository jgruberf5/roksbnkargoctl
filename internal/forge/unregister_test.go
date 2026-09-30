package forge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// A teardown runs when things are already partly gone, so the cases that matter
// are the absent ones: they must succeed, not error.
func TestUnregisterCluster(t *testing.T) {
	for _, tc := range []struct {
		name       string
		clusters   string
		deleteCode int
		wantID     int
		wantErr    bool
		wantDelete bool
	}{
		{name: "removes the named cluster", clusters: `{"clusters":[{"id":7,"name":"fdisco"}]}`,
			deleteCode: 200, wantID: 7, wantDelete: true},
		{name: "no cluster of that name is not an error", clusters: `{"clusters":[{"id":9,"name":"other"}]}`,
			wantID: 0},
		{name: "empty project is not an error", clusters: `{"clusters":[]}`, wantID: 0},
		{name: "already deleted (404) is success", clusters: `{"clusters":[{"id":7,"name":"fdisco"}]}`,
			deleteCode: 404, wantID: 7, wantDelete: true},
		{name: "a real delete failure surfaces", clusters: `{"clusters":[{"id":7,"name":"fdisco"}]}`,
			deleteCode: 500, wantErr: true, wantDelete: true},
		// Ported strictness: roksbnkctl read an unparseable cluster list as "not
		// registered" and exited 0 with the cluster still there.
		{name: "an unparseable cluster list is an error, not absence", clusters: `<html>502 Bad Gateway</html>`,
			wantErr: true},
		{name: "an object without clusters is an error", clusters: `{"data":[]}`, wantErr: true},
		{name: "null clusters is an empty project", clusters: `{"clusters":null}`, wantID: 0},
		{name: "a bare array is accepted", clusters: `[{"id":7,"name":"fdisco"}]`,
			deleteCode: 200, wantID: 7, wantDelete: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			deleted := false
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodGet:
					_, _ = w.Write([]byte(tc.clusters))
				case r.Method == http.MethodDelete:
					deleted = true
					w.WriteHeader(tc.deleteCode)
				}
			}))
			defer srv.Close()

			id, err := mustNew(t, srv.URL, Options{Insecure: true}).UnregisterCluster(context.Background(), 1, "fdisco")
			if tc.wantErr && err == nil {
				t.Fatal("expected an error")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !tc.wantErr && id != tc.wantID {
				t.Fatalf("id: got %d, want %d", id, tc.wantID)
			}
			if deleted != tc.wantDelete {
				t.Fatalf("delete issued=%v, want %v", deleted, tc.wantDelete)
			}
		})
	}
}

// Looking up a project must never create one — a destroy asking "is this still
// here?" should not bring it into being.
func TestProjectIDByNameNeverCreates(t *testing.T) {
	posted := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			posted = true
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"projects": []any{}})
	}))
	defer srv.Close()

	id, err := mustNew(t, srv.URL, Options{Insecure: true}).ProjectIDByName(context.Background(), "nope")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != 0 {
		t.Fatalf("id: got %d, want 0", id)
	}
	if posted {
		t.Fatal("ProjectIDByName created a project")
	}
}

// ProjectIDByName must accept both response shapes, and must NOT silently report
// "no project" for a body it could not parse. For EnsureProject a mis-parse just
// creates a duplicate — loud. Here it would print "nothing to unregister" and exit
// 0 with the cluster still registered, which is the one outcome a teardown must
// never produce quietly.
func TestProjectIDByNameResponseShapes(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		wantID  int
		wantErr bool
	}{
		{"wrapped object", `{"projects":[{"id":7,"name":"acme"}]}`, 7, false},
		{"bare array", `[{"id":9,"name":"acme"}]`, 9, false},
		{"wrapped, absent", `{"projects":[{"id":7,"name":"other"}]}`, 0, false},
		{"bare, absent", `[{"id":9,"name":"other"}]`, 0, false},
		{"empty wrapped", `{"projects":[]}`, 0, false},
		{"empty array", `[]`, 0, false},
		// An empty Forge answers {"projects": null}. roksbnkctl's ProjectIDByName
		// read that as unparseable, so unregister failed against a fresh Forge.
		{"null projects", `{"projects":null}`, 0, false},
		// The ones that matter: unparseable must ERROR, not read as absence.
		{"html error page", `<html><body>502 Bad Gateway</body></html>`, 0, true},
		{"unexpected object", `{"data":{"items":[]}}`, 0, true},
		{"empty body", ``, 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			id, err := mustNew(t, srv.URL, Options{Insecure: true}).ProjectIDByName(context.Background(), "acme")
			if tc.wantErr {
				if err == nil {
					t.Fatalf("body %q: want an error, got id=%d nil", tc.body, id)
				}
				return
			}
			if err != nil {
				t.Fatalf("body %q: unexpected error: %v", tc.body, err)
			}
			if id != tc.wantID {
				t.Fatalf("body %q: id = %d, want %d", tc.body, id, tc.wantID)
			}
		})
	}
}
