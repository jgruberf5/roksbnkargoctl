package cli

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/random"

	"github.com/jgruberf5/roksbnkargoctl/internal/config"
	"github.com/jgruberf5/roksbnkargoctl/internal/far"
)

// Only a release version names a published check image; everything else must
// fall back to :dev, or the check pods cannot pull.
func TestCheckImageTag(t *testing.T) {
	for v, want := range map[string]string{
		"v0.1.0": "v0.1.0", "v1.12.3": "v1.12.3",
		"2ed6902": "dev", "v0.1.0-3-gabc1234": "dev", "v0.1.0-dirty": "dev", "dev": "dev", "": "dev",
	} {
		if got := checkImageTag(v); got != want {
			t.Errorf("checkImageTag(%q) = %q, want %q", v, got, want)
		}
	}
}

// Found live on bnkargo: registering the private endpoint without the cluster
// CA fails x509; the public endpoint must NOT get it (it would replace the
// public roots its certificate chains to).
func TestRegistrationCA(t *testing.T) {
	ca := []byte("-----BEGIN CERTIFICATE-----\nX\n-----END CERTIFICATE-----\n")
	if got := registrationCA("private", ca); string(got) != string(ca) {
		t.Fatal("private endpoint must be registered with the cluster CA")
	}
	if got := registrationCA("public", ca); got != nil {
		t.Fatal("public endpoint must use system roots, not the cluster CA")
	}
}

// A digest reference passes through untouched (no network call is made: the
// puller is nil and would panic if used).
func TestPinDigestKeepsDigests(t *testing.T) {
	ref := "ghcr.io/jgruberf5/roksbnkargoctl-check@sha256:8682f90a728c4ee70bfea6c468c237e302d6ec8ce0e37b750ca711d528252573"
	got, err := pinDigest(context.Background(), nil, ref)
	if err != nil || got != ref {
		t.Fatalf("pinDigest(%s) = %s, %v", ref, got, err)
	}
}

// A tag resolves to the digest the registry serves, keeping the repository.
func TestPinDigestResolvesTag(t *testing.T) {
	srv := httptest.NewServer(registry.New())
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")
	img, err := random.Image(256, 1)
	if err != nil {
		t.Fatal(err)
	}
	ref := host + "/jgruberf5/roksbnkargoctl-check:dev"
	if err := crane.Push(img, ref); err != nil {
		t.Fatal(err)
	}
	want, _ := img.Digest()
	pl, err := far.NewPuller(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	got, err := pinDigest(context.Background(), pl, ref)
	if err != nil {
		t.Fatal(err)
	}
	if got != host+"/jgruberf5/roksbnkargoctl-check@"+want.String() {
		t.Fatalf("pinDigest = %s, want repo@%s", got, want)
	}
}

// Replication happens while registry.source is still "far"; the mirror login
// must be sent anyway, to the mirror host only.
func TestMirrorCredentialsIndependentOfSource(t *testing.T) {
	c := &config.Config{Registry: config.Registry{Source: config.SourceFAR,
		Mirror: config.Mirror{Host: "artifactory.example.org", Prefix: "bnk-mirror", Username: "admin"}}}
	got := mirrorCredentials(c, "tok")
	if len(got) != 1 || got[0].Host != "artifactory.example.org" || got[0].Username != "admin" || got[0].Password != "tok" {
		t.Fatalf("source=far with a mirror configured: got %+v", got)
	}
	c.Registry.Mirror.Username = ""
	if got := mirrorCredentials(c, "tok"); got != nil {
		t.Fatalf("no username: want no credential, got %+v", got)
	}
}
