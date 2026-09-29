package config

import (
	"strings"
	"testing"
)

// There is no way to ask for a deploymentSize other than Tiny: the key does not
// exist, and strict parsing rejects it rather than silently ignoring it.
func TestNoDeploymentSizeKnob(t *testing.T) {
	_, err := Parse([]byte("bnk:\n  deployment_size: Small\n"))
	if err == nil || !strings.Contains(err.Error(), "deployment_size") {
		t.Fatalf("deployment_size must be rejected, got %v", err)
	}
}
