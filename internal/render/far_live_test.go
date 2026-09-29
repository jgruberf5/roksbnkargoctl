//go:build far

// Live render against the real BNK 2.4 GA charts on FAR. F5's charts are
// proprietary, so they are pulled at test time and never committed.
//
//	ROKSBNKARGOCTL_FAR_TGZ=/path/f5-far-auth-key.tgz go test -tags far ./internal/render/ -run Live -v
package render

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/jgruberf5/roksbnkargoctl/internal/config"
	"github.com/jgruberf5/roksbnkargoctl/internal/far"
)

func TestLiveRenderGA(t *testing.T) {
	tgzPath := os.Getenv("ROKSBNKARGOCTL_FAR_TGZ")
	if tgzPath == "" {
		t.Skip("ROKSBNKARGOCTL_FAR_TGZ not set")
	}
	tgz, err := os.ReadFile(tgzPath)
	if err != nil {
		t.Fatal(err)
	}
	sa, err := far.ServiceAccountFromTarball(tgz)
	if err != nil {
		t.Fatal(err)
	}
	p, err := far.NewPuller([]far.Credential{{Host: "repo.f5.com", Username: far.FARUsername, Password: sa}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	m, err := p.FetchManifest(ctx, "repo.f5.com", config.BNKVersion)
	if err != nil {
		t.Fatal(err)
	}
	floVer, ok := m.Chart("f5-lifecycle-operator")
	if !ok {
		t.Fatal("manifest has no f5-lifecycle-operator chart")
	}
	flo, err := p.PullChart(ctx, "repo.f5.com/charts/f5-lifecycle-operator:"+floVer)
	if err != nil {
		t.Fatal(err)
	}
	cm, err := p.PullChart(ctx, "quay.io/jetstack/charts/cert-manager:v1.17.3")
	if err != nil {
		t.Fatal(err)
	}
	c := &config.Config{IBMCloud: config.IBMCloud{Region: "us-east"}, Cluster: "x", TransitGateway: "y",
		Git: config.Git{URL: "https://example.invalid/r.git"}, ArgoCD: config.ArgoCD{Server: "https://a"},
		COS: config.COS{Bucket: "b"}}
	c.Defaults("live")
	c.Resolved = &config.Resolved{VPCName: "vpc-x", TrustedProfileID: "Profile-123", ArgoCDClusterServer: "https://roks.invalid:6443"}
	out, err := Render(Inputs{Config: c, Workspace: "live", Manifest: m, CertManagerChart: cm, FLOChart: flo,
		KubeVersion: "v1.34.9", CheckImage: "ghcr.io/jgruberf5/roksbnkargoctl-check:dev", RunID: "abc",
		Secrets: Secrets{PullHost: "repo.f5.com", PullUsername: far.FARUsername, PullPassword: sa, JWT: "eyJhbGciOiJSUzI1NiJ9.fake.jwt"}})
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]int{}
	for _, o := range out.Git {
		kinds[o.Kind()]++
		b, _ := o.YAML()
		if strings.Contains(string(b), sa) {
			t.Fatalf("FAR key leaked into %s/%s", o.Kind(), o.Name())
		}
	}
	t.Logf("git objects: %d, direct: %d", len(out.Git), len(out.Direct))
	t.Logf("kinds: %v", kinds)
	for _, l := range Summary(out.Git) {
		t.Log(l)
	}
	if kinds["CustomResourceDefinition"] < 26+6 {
		t.Errorf("expected FLO's 26 CRDs plus cert-manager's 6, got %d", kinds["CustomResourceDefinition"])
	}
	for _, want := range []string{"Deployment", "CNEInstance", "CNEManifest", "ClusterIssuer", "NetworkAttachmentDefinition"} {
		if kinds[want] == 0 {
			t.Errorf("no %s rendered", want)
		}
	}
	if dir := os.Getenv("ROKSBNKARGOCTL_RENDER_DIR"); dir != "" {
		if err := Write(out, dir); err != nil {
			t.Fatal(err)
		}
	}
}
