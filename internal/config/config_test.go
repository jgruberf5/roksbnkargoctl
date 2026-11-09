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

func TestExternalBIGIPValidate(t *testing.T) {
	bad := func(e ExternalBIGIP, want string) {
		t.Helper()
		c := &Config{}
		c.Defaults("w")
		c.BNK.ExternalBIGIP = e
		err := c.Validate()
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%+v: %v, want %q", e, err, want)
		}
	}
	bad(ExternalBIGIP{GSLBDatacenter: "dc1"}, "need enabled")
	bad(ExternalBIGIP{Env: []string{"A=b"}}, "need enabled")
	bad(ExternalBIGIP{Enabled: true, Env: []string{"NOVALUE"}}, "entry 1: must be NAME=value")
	bad(ExternalBIGIP{Enabled: true, Env: []string{"=b"}}, "must be NAME=value")
	bad(ExternalBIGIP{Enabled: true, Env: []string{"A=b", "1A=b"}}, "entry 2: must be NAME=value")
	bad(ExternalBIGIP{Enabled: true, Env: []string{"A=1", "A=2"}}, "A is set twice")
	bad(ExternalBIGIP{Enabled: true, Env: []string{"GSLB_DATACENTER_NAME=dc1"}}, "set GSLB_DATACENTER_NAME with bnk.external_bigip.gslb_datacenter")
	for _, v := range []string{"EXTERNAL_BIGIP_LOGIN_SECRET=Sup3r!Pass", "client_secret=abc/DEF+123=", "SECRET_NAME=", "X_SECRET=-a", "X_SECRET=a-", "X_SECRET=Abc", "X_SECRET=" + strings.Repeat("a", 254)} {
		bad(ExternalBIGIP{Enabled: true, Env: []string{v}}, "must hold the name of a Secret")
	}
	for _, n := range []string{"BIGIP_PASSWORD", "bigip_passwd", "AUTH_TOKEN", "API_KEY", "PRIVATEKEY", "BIGIP_PASS", "PASS", "bigip_pwd", "PWD_FILE", "BIGIP_PASSPHRASE", "BIGIP_CREDENTIALS"} {
		bad(ExternalBIGIP{Enabled: true, Env: []string{n + "=hunter2"}}, n+" would put a credential in Git")
	}

	// The customer's own variables, including the one that names a Secret.
	c := &Config{}
	c.Defaults("w")
	c.BNK.ExternalBIGIP = ExternalBIGIP{Enabled: true, GSLBDatacenter: "dc1", Env: []string{
		"ENABLE_EXT_BIGIP_DATASERVER_MONITOR=true", "ENABLE_EXT_BIGIP_POOL_MONITOR=true",
		"EXTERNAL_BIGIP_LOGIN_SECRET=f5-bigip-ctlr-login", "CLUSTER_IDENTIFIER=cluster-lab2", "EMPTY=",
		"EXT_BIGIP_REGISTRY_CR=external-bigip-registry", "COMPASS_MODE=on", "BYPASS_CACHE=1", "SECRET_NAME=a.b-c", "ONE_SECRET=a", "LONG_SECRET=" + strings.Repeat("a", 253),
	}}
	if err := c.Validate(); err != nil && strings.Contains(err.Error(), "external_bigip") {
		t.Errorf("valid settings refused: %v", err)
	}
	// A value never reaches the error: it may be the credential.
	c.BNK.ExternalBIGIP.Env = []string{"BIGIP_PASSWORD=hunter2"}
	if err := c.Validate(); err == nil || strings.Contains(err.Error(), "hunter2") {
		t.Errorf("error holds the value: %v", err)
	}
	c.BNK.ExternalBIGIP.Env = []string{"LOGIN_SECRET=hunter2!"}
	if err := c.Validate(); err == nil || strings.Contains(err.Error(), "hunter2") {
		t.Errorf("error holds a Secret variable's value: %v", err)
	}
	c.BNK.ExternalBIGIP.Env = []string{"hunter2"}
	if err := c.Validate(); err == nil || strings.Contains(err.Error(), "hunter2") {
		t.Errorf("error holds a malformed entry: %v", err)
	}
}

func TestCOSEndpoint(t *testing.T) {
	c := &Config{}
	c.Defaults("w")
	if c.COS.Endpoint != "public" {
		t.Errorf("default cos.endpoint = %q, want public", c.COS.Endpoint)
	}
	for _, v := range []string{"public", "private", "direct", "https://cos-vpe.example.internal"} {
		c.COS.Endpoint = v
		if err := c.Validate(); err != nil && strings.Contains(err.Error(), "cos.endpoint") {
			t.Errorf("cos.endpoint %q refused: %v", v, err)
		}
	}
	for _, v := range []string{"dirct", "http://cos.example", "https://"} {
		c.COS.Endpoint = v
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "cos.endpoint") {
			t.Errorf("cos.endpoint %q accepted: %v", v, err)
		}
	}
}
