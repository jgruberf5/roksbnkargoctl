package gitpub

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCheckFindsTheBranchOrAnEmptyRepo(t *testing.T) {
	_, url := bareRepo(t)
	for _, write := range []bool{false, true} {
		a, err := Check(context.Background(), Options{URL: url, Branch: "main"}, write)
		if err != nil || !a.Empty || a.HasBranch {
			t.Fatalf("empty repo (write=%v): %+v %v", write, a, err)
		}
	}
	src := writeTree(t, map[string]string{"a.yaml": "x: 1\n"})
	if _, _, err := Publish(context.Background(), Options{URL: url, Branch: "main", Path: "bnk/demo", SrcDir: src, AuthorName: "t", AuthorEmail: "t@example.com"}); err != nil {
		t.Fatal(err)
	}
	for _, write := range []bool{false, true} {
		a, err := Check(context.Background(), Options{URL: url, Branch: "main"}, write)
		if err != nil || a.Empty || !a.HasBranch {
			t.Fatalf("after a publish (write=%v): %+v %v", write, a, err)
		}
		a, err = Check(context.Background(), Options{URL: url, Branch: "release"}, write)
		if err != nil || a.Empty || a.HasBranch {
			t.Fatalf("another branch (write=%v): %+v %v", write, a, err)
		}
	}
}

func TestCheckReportsAMissingRepository(t *testing.T) {
	_, url := bareRepo(t)
	_, err := Check(context.Background(), Options{URL: url + "-nope"}, false)
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("want not found, got %v", err)
	}
}

// Over smart HTTP: a push check asks for the receive-pack service (the one a
// host only offers to a credential that may push), a read check for
// upload-pack, and the host's 401/403/404 become messages that say what to fix.
func TestCheckAsksForTheRightServiceAndExplainsRefusals(t *testing.T) {
	var service string
	status := http.StatusForbidden
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		service = r.URL.Query().Get("service")
		w.WriteHeader(status)
	}))
	defer srv.Close()
	url := srv.URL + "/org/repo.git"
	o := Options{URL: url, Token: "tok"}

	_, err := Check(context.Background(), o, true)
	if service != "git-receive-pack" {
		t.Errorf("a push check asked for %q", service)
	}
	if err == nil || !strings.Contains(err.Error(), "may not push to") {
		t.Errorf("403 on a push check: %v", err)
	}
	_, err = Check(context.Background(), o, false)
	if service != "git-upload-pack" {
		t.Errorf("a read check asked for %q", service)
	}
	if err == nil || !strings.Contains(err.Error(), "may not read") {
		t.Errorf("403 on a read check: %v", err)
	}
	status = http.StatusUnauthorized
	if _, err = Check(context.Background(), o, true); err == nil || !strings.Contains(err.Error(), "needs a credential") {
		t.Errorf("401: %v", err)
	}
	status = http.StatusNotFound
	if _, err = Check(context.Background(), o, true); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("404: %v", err)
	}
}
