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
