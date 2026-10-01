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

// Single-certificate settings are checked before anything is installed.
func TestValidateSingleCertificate(t *testing.T) {
	base := func() *Config {
		c := &Config{IBMCloud: IBMCloud{Region: "us-east"}, Cluster: "c", TransitGateway: "t",
			COS: COS{Instance: "i", Bucket: "b"}, ArgoCD: ArgoCD{Server: "https://a"}, Git: Git{URL: "https://g/r.git", TokenEnv: "T"}}
		return c
	}
	for name, tc := range map[string]struct {
		ct   Certificates
		want string
	}{
		"bad mode":            {Certificates{Mode: "vault"}, "bnk.certificates.mode"},
		"ca without files":    {Certificates{Mode: CertModeSingle, Issuer: "ca", CACertFile: "ca.pem"}, "needs ca_cert_file and ca_key_file"},
		"provided incomplete": {Certificates{Mode: CertModeSingle, Issuer: "provided", CertFile: "c", KeyFile: "k"}, "needs cert_file, key_file and ca_file"},
		"bad issuer":          {Certificates{Mode: CertModeSingle, Issuer: "acme"}, "must be self-signed, ca or provided"},
		"bad key type":        {Certificates{Mode: CertModeSingle, KeyType: "dsa"}, "must be rsa or ecdsa"},
		"short key":           {Certificates{Mode: CertModeSingle, KeyBits: 1024}, "at least 2048"},
		"renew too late":      {Certificates{Mode: CertModeSingle, ValidityDays: 30, RenewBeforeDays: 30}, "renew_before_days must be less than validity_days"},
	} {
		c := base()
		c.BNK.Certificates = tc.ct
		c.Defaults("ws")
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v", name, err)
		}
	}
	ok := base()
	ok.BNK.Certificates = Certificates{Mode: CertModeSingle, Issuer: "ca", CACertFile: "ca.pem", CAKeyFile: "ca.key"}
	ok.Defaults("ws")
	if err := ok.Validate(); err != nil {
		t.Errorf("valid single-certificate config refused: %v", err)
	}
}
