package kube

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestListFollowsContinueAndSendsSelectors(t *testing.T) {
	var queries []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			t.Errorf("auth header %q", r.Header.Get("Authorization"))
		}
		queries = append(queries, r.URL.RawQuery)
		if r.URL.Query().Get("labelSelector") != "app=x" || r.URL.Query().Get("fieldSelector") != "spec.nodeName=n1" {
			t.Errorf("selectors lost: %s", r.URL.RawQuery)
		}
		if r.URL.Query().Get("continue") == "" {
			_, _ = io.WriteString(w, `{"metadata":{"continue":"p2"},"items":[{"metadata":{"name":"a"}}]}`)
			return
		}
		_, _ = io.WriteString(w, `{"metadata":{},"items":[{"metadata":{"name":"b"}}]}`)
	}))
	defer srv.Close()
	c := New(srv.URL, srv.Client(), "tok")
	items, err := c.List(context.Background(), Path("", "v1", "pods", "ns", ""), ListOptions{LabelSelector: "app=x", FieldSelector: "spec.nodeName=n1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 || items[0].Name() != "a" || items[1].Name() != "b" || len(queries) != 2 {
		t.Fatalf("items=%v queries=%v", items, queries)
	}
}

func TestErrorsApplyAndDelete(t *testing.T) {
	var got struct {
		method, ct, query string
		body              map[string]any
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.method, got.ct, got.query = r.Method, r.Header.Get("Content-Type"), r.URL.RawQuery
		b, _ := io.ReadAll(r.Body)
		got.body = nil
		_ = json.Unmarshal(b, &got.body)
		switch r.URL.Path {
		case "/api/v1/namespaces/ns/secrets/missing":
			w.WriteHeader(404)
			_, _ = io.WriteString(w, `{"kind":"Status","reason":"NotFound","message":"secrets \"missing\" not found"}`)
		case "/apis/k8s.f5net.com/v1/namespaces/ns/licenses/l":
			w.WriteHeader(403)
			_, _ = io.WriteString(w, `{"kind":"Status","reason":"Forbidden","message":"exceeded quota"}`)
		default:
			_, _ = io.WriteString(w, `{}`)
		}
	}))
	defer srv.Close()
	c := New(srv.URL, srv.Client(), "")
	ctx := context.Background()

	_, err := c.Get(ctx, Path("", "v1", "secrets", "ns", "missing"))
	if !IsNotFound(err) || IsForbidden(err) {
		t.Fatalf("404 not classified: %v", err)
	}
	if ok, err := c.Exists(ctx, Path("", "v1", "secrets", "ns", "missing")); ok || err != nil {
		t.Fatalf("Exists on 404 = %v, %v", ok, err)
	}
	if did, err := c.DeleteIfExists(ctx, Path("", "v1", "secrets", "ns", "missing"), ""); did || err != nil {
		t.Fatalf("DeleteIfExists on 404 = %v, %v", did, err)
	}

	_, err = c.Apply(ctx, Path("k8s.f5net.com", "v1", "licenses", "ns", "l"), map[string]any{"kind": "License"}, "mgr", true)
	if !IsForbidden(err) || got.ct != ApplyPatch || got.query != "fieldManager=mgr&force=true" || got.method != "PATCH" {
		t.Fatalf("apply: err=%v ct=%s q=%s", err, got.ct, got.query)
	}

	if err := c.Delete(ctx, Path("", "v1", "namespaces", "", "f5-bnk"), "Foreground"); err != nil {
		t.Fatal(err)
	}
	if got.method != "DELETE" || got.body["propagationPolicy"] != "Foreground" || got.body["kind"] != "DeleteOptions" {
		t.Fatalf("delete body %v", got.body)
	}

	if _, err := c.Patch(ctx, Path("", "v1", "pods", "ns", "p"), MergePatch, map[string]any{"a": 1}); err != nil || got.ct != MergePatch {
		t.Fatalf("patch: %v %s", err, got.ct)
	}
}

func TestPathAndDiscovery(t *testing.T) {
	if p := Path("", "v1", "namespaces", "", "f5-bnk"); p != "/api/v1/namespaces/f5-bnk" {
		t.Error(p)
	}
	if p := Path("k8s.f5.com", "v1", "cneinstances", "f5-bnk", ""); p != "/apis/k8s.f5.com/v1/namespaces/f5-bnk/cneinstances" {
		t.Error(p)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/apis":
			_, _ = io.WriteString(w, `{"groups":[{"name":"k8s.f5.com","preferredVersion":{"version":"v1"}},{"name":"fic.f5.com","preferredVersion":{"version":"v1"}},{"name":"apps","preferredVersion":{"version":"v1"}}]}`)
		case "/apis/k8s.f5.com/v1":
			_, _ = io.WriteString(w, `{"resources":[{"name":"cneinstances","namespaced":true,"kind":"CNEInstance","verbs":["get","list","delete"]},{"name":"cneinstances/status","namespaced":true,"verbs":["get"]},{"name":"cnemanifests","namespaced":false,"kind":"CNEManifest","verbs":["get","list","deletecollection"]},{"name":"nolist","verbs":["get"]}]}`)
		default: // fic.f5.com mid-deletion
			w.WriteHeader(503)
		}
	}))
	defer srv.Close()
	c := New(srv.URL, srv.Client(), "")
	gvrs, errs := c.DiscoverGroups(context.Background(), []string{"k8s.f5.com", "fic.f5.com"})
	if len(errs) != 1 {
		t.Errorf("want the fic.f5.com failure reported, got %v", errs)
	}
	if len(gvrs) != 2 || gvrs[0].Resource != "cneinstances" || !gvrs[0].CanDelete || gvrs[1].Resource != "cnemanifests" || gvrs[1].CanDelete || gvrs[1].Namespaced {
		t.Fatalf("%+v", gvrs)
	}
}
