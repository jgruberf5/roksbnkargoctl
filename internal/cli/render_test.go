package cli

import "testing"

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
