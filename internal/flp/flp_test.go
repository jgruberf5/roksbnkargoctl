package flp

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"strings"
	"testing"
)

func chartTGZ(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		_ = tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg})
		_, _ = tw.Write([]byte(body))
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

func TestProdJWKSExtractsTheChartKeySet(t *testing.T) {
	jwks := `{"keys":[{"kid":"v1","alg":"RS512"}]}`
	tgz := chartTGZ(t, map[string]string{
		"f5-license-proxy/templates/flp-vault-postgresql.yaml": "data:\n  prod_jwks.txt: " + base64.StdEncoding.EncodeToString([]byte(jwks)) + "\n",
		"f5-license-proxy/values.yaml":                         "x: 1\n",
	})
	got, err := ProdJWKS(tgz)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != jwks {
		t.Fatalf("got %q", got)
	}
	if _, err := ProdJWKS(chartTGZ(t, map[string]string{"c/values.yaml": "x: 1\n"})); err == nil {
		t.Fatal("a chart without prod_jwks must be an error, not an empty key set")
	}
}

func TestCAIsACertSignCA(t *testing.T) {
	ca, err := NewCA()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := pem.Decode(ca.CertPEM)
	cert, err := x509.ParseCertificate(b.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if !cert.IsCA || cert.KeyUsage&x509.KeyUsageCertSign == 0 || cert.Subject.CommonName != "flp.ca" {
		t.Fatalf("CA shape wrong: isCA=%v usage=%v cn=%s", cert.IsCA, cert.KeyUsage, cert.Subject.CommonName)
	}
}

func TestCloudInitCarriesTheInputsAndRefusesMultilineSecrets(t *testing.T) {
	ca, _ := NewCA()
	in := CloudInitInput{JWT: "a.b.c", FARServiceAccount: "SAKEY", ExternalIP: "1.2.3.4", ProdJWKS: []byte(`{"keys":[]}`), CA: ca}
	ci, err := CloudInit(in)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"JWT_TOKEN=a.b.c", "EXTERNAL_IP=1.2.3.4", "TAG=" + DefaultVersion, "REG=repo.f5.com/images", "SAKEY", "#cloud-config"} {
		if !strings.Contains(ci, want) {
			t.Errorf("cloud-init missing %q", want)
		}
	}
	in.JWT = "a.b.c\nEVIL=1"
	if _, err := CloudInit(in); err == nil {
		t.Fatal("a newline in the JWT would inject lines into answer.env")
	}
}
