package registry

import (
	"testing"

	"github.com/jgruberf5/roksbnkargoctl/internal/far"
)

// The renderer points ROKS at <mirror>/<source path minus host>; if Dest ever
// drifts from that, every pull in mirror mode 404s. Pin the rule on the three
// source hosts an install uses.
func TestDestKeepsSourcePathUnderMirror(t *testing.T) {
	cases := map[string]string{
		"repo.f5.com/images/f5-tmm:1.2":                    "harbor.x:8443/bnk-mirror/images/f5-tmm:1.2",
		"quay.io/jetstack/cert-manager-controller:v1.17.3": "harbor.x:8443/bnk-mirror/jetstack/cert-manager-controller:v1.17.3",
		"ghcr.io/jgruberf5/roksbnkargoctl-check:v0.1.0":    "harbor.x:8443/bnk-mirror/jgruberf5/roksbnkargoctl-check:v0.1.0",
		"repo.f5.com/release/f5-bigip-k8s-manifest:2.4.0":  "harbor.x:8443/bnk-mirror/release/f5-bigip-k8s-manifest:2.4.0",
	}
	for src, want := range cases {
		if got := (Artifact{Source: src}).Dest("harbor.x:8443/bnk-mirror/"); got != want {
			t.Errorf("Dest(%s) = %s, want %s", src, got, want)
		}
	}
}

func TestBOMIncludesEverythingAnInstallPulls(t *testing.T) {
	m := &far.Manifest{Version: "2.4.0",
		Charts: []far.Artifact{{Name: "charts/f5-lifecycle-operator", Version: "v2.30.0-0.5.2"}},
		Images: []far.Artifact{{Name: "images/f5-lifecycle-operator", Version: "v2.30.0-0.5.2"}}}
	bom := BOM(m, "repo.f5.com", "v1.17.3", "ghcr.io/jgruberf5/roksbnkargoctl-check:v1", true)
	want := []string{
		"repo.f5.com/release/f5-bigip-k8s-manifest:2.4.0",
		"repo.f5.com/charts/f5-lifecycle-operator:v2.30.0-0.5.2",
		"repo.f5.com/images/f5-lifecycle-operator:v2.30.0-0.5.2",
		"quay.io/jetstack/charts/cert-manager:v1.17.3",
		"quay.io/jetstack/cert-manager-controller:v1.17.3",
		"quay.io/jetstack/cert-manager-webhook:v1.17.3",
		"quay.io/jetstack/cert-manager-cainjector:v1.17.3",
		"quay.io/jetstack/cert-manager-acmesolver:v1.17.3",
		"ghcr.io/jgruberf5/roksbnkargoctl-check:v1",
	}
	got := map[string]bool{}
	for _, a := range bom {
		got[a.Source] = true
	}
	for _, w := range want {
		if !got[w] {
			t.Errorf("BOM missing %s", w)
		}
	}
	if len(bom) != len(want) {
		t.Errorf("BOM has %d artifacts, want %d", len(bom), len(want))
	}
}
