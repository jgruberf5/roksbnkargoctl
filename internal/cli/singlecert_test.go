package cli

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jgruberf5/roksbnkargoctl/internal/config"
)

// writeCA writes a CA certificate and key valid until notAfter.
func writeCA(t *testing.T, notAfter time.Time) (certFile, keyFile string) {
	t.Helper()
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "operator-ca"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: notAfter, IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, k.Public(), k)
	if err != nil {
		t.Fatal(err)
	}
	kb, _ := x509.MarshalECPrivateKey(k)
	dir := t.TempDir()
	certFile, keyFile = filepath.Join(dir, "ca.pem"), filepath.Join(dir, "ca.key")
	_ = os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600)
	_ = os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}), 0o600)
	return certFile, keyFile
}

// The operator's CA (issuer ca) is read and checked on this machine, before
// anything is published: an expiring CA is refused here, not by the sync hook
// after the commit is in Git.
func TestSingleCertSourceIsCheckedBeforePublishing(t *testing.T) {
	s := recordSession(t)
	s.cfg.BNK.Certificates = config.Certificates{Mode: config.CertModeSingle, Issuer: "ca"}
	s.cfg.Defaults("rec")
	s.cfg.BNK.Certificates.CACertFile, s.cfg.BNK.Certificates.CAKeyFile = writeCA(t, time.Now().Add(5*365*24*time.Hour))
	cert, key, _, err := s.SingleCertSource()
	if err != nil || !strings.Contains(cert, "BEGIN CERTIFICATE") || !strings.Contains(key, "PRIVATE KEY") {
		t.Fatalf("a valid CA: err %v, cert %d bytes, key %d bytes", err, len(cert), len(key))
	}
	s.cfg.BNK.Certificates.CACertFile, s.cfg.BNK.Certificates.CAKeyFile = writeCA(t, time.Now().Add(10*24*time.Hour))
	if _, _, _, err := s.SingleCertSource(); err == nil || !strings.Contains(err.Error(), "renewal window") {
		t.Errorf("a CA expiring in 10 days: %v", err)
	}
	s.cfg.BNK.Certificates.Issuer = "provided"
	s.cfg.BNK.Certificates.CertFile, s.cfg.BNK.Certificates.KeyFile = writeCA(t, time.Now().Add(365*24*time.Hour))
	s.cfg.BNK.Certificates.CAFile = s.cfg.BNK.Certificates.CertFile
	if _, _, _, err := s.SingleCertSource(); err == nil || !strings.Contains(err.Error(), "does not cover names BNK uses") {
		t.Errorf("a provided certificate without BNK's names: %v", err)
	}
}

// diagnose collects each namespace once, and cert-manager's only when it is
// installed.
func TestDiagnoseNamespaces(t *testing.T) {
	c := &config.Config{}
	c.Defaults("w")
	if got := strings.Join(diagnoseNamespaces(c), ","); got != "roksbnkargoctl-check,f5-bnk,f5-utils,cert-manager" {
		t.Errorf("default: %s", got)
	}
	c.BNK.UtilsNamespace = c.BNK.Namespace
	c.BNK.Certificates.Mode = config.CertModeSingle
	if got := strings.Join(diagnoseNamespaces(c), ","); got != "roksbnkargoctl-check,f5-bnk" {
		t.Errorf("one namespace, single certificate: %s", got)
	}
}
