package forge

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const adminKubeconfig = `apiVersion: v1
kind: Config
clusters:
- name: other
  cluster:
    server: https://wrong.example.com:1
- name: mycluster/abc
  cluster:
    server: https://c1.example.com:31234
    certificate-authority-data: Q0FEQVRB
contexts:
- name: mycluster/abc
  context:
    cluster: mycluster/abc
    user: admin
    namespace: openshift-config
current-context: mycluster/abc
users:
- name: someone
  user:
    token: not-this-one
- name: admin
  user:
    client-certificate-data: Q0VSVA==
    client-key-data: S0VZ
`

func TestCertKubeconfig(t *testing.T) {
	out, err := CertKubeconfig([]byte(adminKubeconfig), "mycluster-admin")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Clusters []struct {
			Name    string
			Cluster map[string]string
		}
		Users []struct {
			Name string
			User map[string]string
		}
		Contexts []struct {
			Name    string
			Context map[string]string
		}
		Current string `yaml:"current-context"`
	}
	if err := yaml.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Clusters) != 1 || len(doc.Users) != 1 || len(doc.Contexts) != 1 {
		t.Fatalf("want one cluster, user and context:\n%s", out)
	}
	// The current context's cluster and user, not the first entries.
	if got := doc.Clusters[0].Cluster; got["server"] != "https://c1.example.com:31234" || got["certificate-authority-data"] != "Q0FEQVRB" {
		t.Errorf("cluster %v", got)
	}
	u := doc.Users[0]
	if u.Name != "mycluster-admin" || u.User["client-certificate-data"] != "Q0VSVA==" || u.User["client-key-data"] != "S0VZ" || len(u.User) != 2 {
		t.Errorf("user %+v", u)
	}
	if strings.Contains(string(out), "token") {
		t.Errorf("a Forge kubeconfig must carry no token:\n%s", out)
	}
	c := doc.Contexts[0]
	if doc.Current != c.Name || c.Context["user"] != "mycluster-admin" || c.Context["cluster"] != doc.Clusters[0].Name || c.Context["namespace"] != "openshift-config" {
		t.Errorf("context %+v current %q", c, doc.Current)
	}
}

// ROKS public masters carry no CA; the field is omitted, never emitted empty.
func TestCertKubeconfigNoCA(t *testing.T) {
	const roks = `apiVersion: v1
clusters:
- name: roks/abc
  cluster:
    server: https://c1.us-south.containers.cloud.ibm.com:31517
contexts:
- name: roks/abc-ctx
  context: {cluster: roks/abc, user: admin}
current-context: roks/abc-ctx
users:
- name: admin
  user:
    client-certificate-data: Q0VSVA==
    client-key-data: S0VZ
`
	out, err := CertKubeconfig([]byte(roks), "")
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if strings.Contains(s, "certificate-authority") {
		t.Errorf("no-CA kubeconfig must omit certificate-authority:\n%s", s)
	}
	if !strings.Contains(s, "name: roks/abc-admin") || !strings.Contains(s, "namespace: default") {
		t.Errorf("default user name / namespace missing:\n%s", s)
	}
}

// Forge cannot authenticate to ROKS with an IAM token, and it cannot read a
// file on this machine: both are refused rather than sent.
func TestCertKubeconfigRefusesWhatForgeCannotUse(t *testing.T) {
	for name, kc := range map[string]string{
		"token only": `clusters: [{name: c, cluster: {server: "https://x:6443"}}]
users: [{name: u, user: {token: abc}}]`,
		"cert by file reference": `clusters: [{name: c, cluster: {server: "https://x:6443"}}]
users: [{name: u, user: {client-certificate: admin.pem, client-key: admin-key.pem}}]`,
		"CA by file reference": `clusters: [{name: c, cluster: {server: "https://x:6443", certificate-authority: ca.pem}}]
users: [{name: u, user: {client-certificate-data: Q0VSVA==, client-key-data: S0VZ}}]`,
		"no server": `clusters: [{name: c, cluster: {}}]
users: [{name: u, user: {client-certificate-data: Q0VSVA==, client-key-data: S0VZ}}]`,
		"no clusters": `users: [{name: u, user: {client-certificate-data: Q0VSVA==, client-key-data: S0VZ}}]`,
	} {
		if out, err := CertKubeconfig([]byte(kc), "u"); err == nil {
			t.Errorf("%s: accepted:\n%s", name, out)
		}
	}
}
