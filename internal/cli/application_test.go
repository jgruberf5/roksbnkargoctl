package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jgruberf5/roksbnkargoctl/internal/argocd"
)

// hubWith is a fake Argo CD holding one Application (nil: none), recording the
// Application install posts.
func hubWith(t *testing.T, held map[string]any) (*argocd.Client, *map[string]any) {
	t.Helper()
	var posted map[string]any
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && held == nil:
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":"applications.argoproj.io \"bnk-demo\" not found","code":5}`)
		case r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(held)
		case r.Method == http.MethodPost:
			_ = json.NewDecoder(r.Body).Decode(&posted)
			_ = json.NewEncoder(w).Encode(posted)
		}
	}))
	t.Cleanup(srv.Close)
	return argocd.New(srv.URL, "t", true, nil), &posted
}

func sources(flo, cm string) []map[string]any {
	return []map[string]any{
		{"repoURL": "quay.io/jetstack/charts", "chart": "cert-manager", "targetRevision": cm},
		{"repoURL": "repo.f5.com/charts", "chart": "f5-lifecycle-operator", "targetRevision": flo},
		{"repoURL": "https://g/r.git", "path": "bnk/demo", "targetRevision": "main", "ref": "values"},
	}
}

func renderedApp(flo string) *argocd.Application {
	return &argocd.Application{
		Metadata: argocd.ObjectMeta{Name: "bnk-demo", Namespace: "argocd", Finalizers: []string{"resources-finalizer.argocd.argoproj.io"}},
		Spec:     argocd.ApplicationSpec{Project: "default", Sources: sources(flo, "v1.17.3")},
	}
}

// The FLO chart's version lives in the Application on the hub, not in Git: an
// operator can change it in the Argo CD UI. install replaces the Application
// with the render, whose FLO is the release manifest's. Live (run 112) that
// edit was the only way to move FLO without the CLI; undoing it must not be
// silent.
func TestPutApplicationNamesAChartVersionItOverwrites(t *testing.T) {
	hub := map[string]any{
		"metadata": map[string]any{"name": "bnk-demo", "namespace": "argocd",
			"finalizers": []string{"resources-finalizer.argocd.argoproj.io", "post-delete-finalizer.argocd.argoproj.io"}},
		"spec": map[string]any{"project": "default", "sources": sources("v2.30.0-0.11.9", "v1.17.3")},
	}

	// install: the hub's FLO differs from the release's -> a warning naming both.
	ac, posted := hubWith(t, hub)
	var buf bytes.Buffer
	if err := putApplication(context.Background(), printer{&buf}, ac, renderedApp("v2.30.0-0.11.8"), false); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "⚠ the Application's f5-lifecycle-operator chart is v2.30.0-0.11.9 on the hub") ||
		!strings.Contains(out, "install sets it back to v2.30.0-0.11.8") {
		t.Errorf("install did not warn about the FLO version it overwrites:\n%s", out)
	}
	if strings.Contains(out, "cert-manager") {
		t.Errorf("an unchanged chart was reported:\n%s", out)
	}
	// What is posted is still the render, with the hub's hook finalizer kept.
	b, _ := json.Marshal(*posted)
	if !strings.Contains(string(b), `"targetRevision":"v2.30.0-0.11.8"`) || strings.Contains(string(b), "0.11.9") {
		t.Errorf("posted Application is not the render: %s", b)
	}
	if !strings.Contains(string(b), "post-delete-finalizer.argocd.argoproj.io") {
		t.Errorf("the hub's hook finalizer was dropped: %s", b)
	}

	// upgrade: the same difference is the change being made, reported as such.
	ac, _ = hubWith(t, hub)
	buf.Reset()
	if err := putApplication(context.Background(), printer{&buf}, ac, renderedApp("v2.30.0-0.11.8"), true); err != nil {
		t.Fatal(err)
	}
	if out := buf.String(); !strings.Contains(out, "✓ Application: chart f5-lifecycle-operator v2.30.0-0.11.9 -> v2.30.0-0.11.8") || strings.Contains(out, "⚠") {
		t.Errorf("upgrade did not report the FLO change as its own:\n%s", out)
	}

	// Same versions: nothing to say.
	ac, _ = hubWith(t, map[string]any{"metadata": hub["metadata"], "spec": map[string]any{"project": "default", "sources": sources("v2.30.0-0.11.8", "v1.17.3")}})
	buf.Reset()
	if err := putApplication(context.Background(), printer{&buf}, ac, renderedApp("v2.30.0-0.11.8"), false); err != nil {
		t.Fatal(err)
	}
	if buf.Len() != 0 {
		t.Errorf("unchanged Application reported something:\n%s", buf.String())
	}

	// First install: no Application on the hub, nothing to compare, still created.
	ac, posted = hubWith(t, nil)
	buf.Reset()
	if err := putApplication(context.Background(), printer{&buf}, ac, renderedApp("v2.30.0-0.11.8"), false); err != nil {
		t.Fatal(err)
	}
	if *posted == nil || buf.Len() != 0 {
		t.Errorf("first install: posted=%v output=%q", *posted != nil, buf.String())
	}
}
