package checks

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"math/big"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jgruberf5/roksbnkargoctl/cmd/check/internal/kube"
	"github.com/jgruberf5/roksbnkargoctl/cmd/check/internal/kube/kubefake"
	"github.com/jgruberf5/roksbnkargoctl/internal/singlecert"
)

var certNow = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

func certCfg(issuer string, nss ...string) SingleCertConfig {
	return SingleCertConfig{
		Issuer: issuer, SecretName: "bnk-single-cert", Namespaces: nss, StateNS: "roksbnkargoctl-check",
		Subject:    pkix.Name{Country: []string{"US"}, Organization: []string{"F5 Networks"}, OrganizationalUnit: []string{"PD"}},
		CommonName: "f5net", CACommonName: "f5net-ca", KeyType: "ecdsa",
		Validity: 3650 * 24 * time.Hour, RenewBefore: 30 * 24 * time.Hour, Now: func() time.Time { return certNow },
	}
}

// secretTLS reads a Secret's tls.crt/tls.key/ca.crt from the fake.
func secretTLS(t *testing.T, s *kubefake.Server, ns, name string) (*x509.Certificate, *tlsMaterial, kube.Object) {
	t.Helper()
	o := s.Get("", "v1", "secrets", ns, name)
	if o == nil {
		t.Fatalf("no Secret %s/%s", ns, name)
	}
	m, err := tlsFromSecret(o, true)
	if err != nil {
		t.Fatalf("%s/%s: %v", ns, name, err)
	}
	leaf, _, _, err := singlecert.Pair(m.cert, m.key)
	if err != nil {
		t.Fatal(err)
	}
	return leaf, m, o
}

func runCert(t *testing.T, s *kubefake.Server, env *Env, cfg SingleCertConfig) *Result {
	t.Helper()
	res := NewResult("cert", &tlog{t: t})
	if err := SingleCert(context.Background(), env, cfg, res); err != nil {
		t.Fatal(err)
	}
	return res
}

// Self-signed, as F5's procedure: a CA (CN f5net-ca) signs one certificate
// (CN f5net) carrying F5's names for every BNK namespace, written identically
// into each namespace as a kubernetes.io/tls Secret with tls.crt, tls.key and
// ca.crt; the CA itself is kept in the check namespace for renewals.
func TestSingleCertSelfSigned(t *testing.T) {
	s, env, _ := newFake(t)
	res := runCert(t, s, env, certCfg(IssuerSelfSigned, "f5-bnk", "f5-utils"))
	if res.Failed() {
		t.Fatal(res.Failures())
	}
	leaf, m, o := secretTLS(t, s, "f5-bnk", "bnk-single-cert")
	_, m2, _ := secretTLS(t, s, "f5-utils", "bnk-single-cert")
	if string(m.cert) != string(m2.cert) || string(m.key) != string(m2.key) || string(m.ca) != string(m2.ca) {
		t.Fatal("the namespaces hold different certificates")
	}
	if o.String("type") != "kubernetes.io/tls" {
		t.Errorf("type %q", o.String("type"))
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(m.ca)
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: pool, CurrentTime: certNow, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err != nil {
		t.Fatalf("tls.crt does not verify against ca.crt: %v", err)
	}
	ca := mustCert(t, m.ca)
	if leaf.Subject.CommonName != "f5net" || ca.Subject.CommonName != "f5net-ca" || !ca.IsCA {
		t.Errorf("subjects: leaf %q, ca %q (IsCA %v)", leaf.Subject.CommonName, ca.Subject.CommonName, ca.IsCA)
	}
	if leaf.Subject.String() == ca.Subject.String() {
		t.Error("leaf and CA share a subject: the leaf would look self-signed (F5's procedure warns against it)")
	}
	for _, want := range []string{"f5-tmm", "f5-spk-cwc.f5-bnk.svc", "f5-spk-cwc.f5-utils.svc", "*.f5-observer.f5-utils.svc.cluster.local", "rabbitmq-server.f5-utils"} {
		if !slices.Contains(leaf.DNSNames, want) {
			t.Errorf("no SAN %s", want)
		}
	}
	if !slices.Contains(leaf.ExtKeyUsage, x509.ExtKeyUsageServerAuth) || !slices.Contains(leaf.ExtKeyUsage, x509.ExtKeyUsageClientAuth) {
		t.Errorf("ext key usage %v: mTLS needs server and client", leaf.ExtKeyUsage)
	}
	if s.Get("", "v1", "secrets", "roksbnkargoctl-check", "bnk-single-cert-ca") == nil {
		t.Error("the self-signed CA is not kept for renewals")
	}
}

// A second run with the same settings keeps the certificate: a re-sync must
// not rotate it (every component would restart).
func TestSingleCertIsKeptWhenCurrent(t *testing.T) {
	s, env, _ := newFake(t)
	cfg := certCfg(IssuerSelfSigned, "f5-bnk", "f5-utils")
	runCert(t, s, env, cfg)
	_, before, _ := secretTLS(t, s, "f5-bnk", "bnk-single-cert")
	res := runCert(t, s, env, cfg)
	_, after, _ := secretTLS(t, s, "f5-bnk", "bnk-single-cert")
	if string(before.cert) != string(after.cert) || string(before.key) != string(after.key) {
		t.Fatal("an unchanged re-run reissued the certificate")
	}
	if !strings.Contains(strings.Join(passes(res), "\n"), "kept") {
		t.Errorf("did not report keeping it: %v", passes(res))
	}
}

// It is reissued — by the same CA — when a namespace lacks it, when the
// settings change, or when it is within the renewal window.
func TestSingleCertIsReissuedWhenNeeded(t *testing.T) {
	cases := map[string]func(*kubefake.Server, *SingleCertConfig){
		"missing in one namespace": func(s *kubefake.Server, _ *SingleCertConfig) {
			s.Remove("", "v1", "secrets", "f5-utils", "bnk-single-cert")
		},
		"settings changed": func(_ *kubefake.Server, c *SingleCertConfig) { c.ExtraDNS = []string{"bnk.example.com"} },
		"within renewal window": func(_ *kubefake.Server, c *SingleCertConfig) {
			c.Validity = 40 * 24 * time.Hour
			later := certNow.Add(20 * 24 * time.Hour)
			c.Now = func() time.Time { return later }
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			s, env, _ := newFake(t)
			cfg := certCfg(IssuerSelfSigned, "f5-bnk", "f5-utils")
			if name == "within renewal window" {
				cfg.Validity = 40 * 24 * time.Hour
			}
			runCert(t, s, env, cfg)
			_, before, _ := secretTLS(t, s, "f5-bnk", "bnk-single-cert")
			mutate(s, &cfg)
			runCert(t, s, env, cfg)
			_, after, _ := secretTLS(t, s, "f5-bnk", "bnk-single-cert")
			if string(before.cert) == string(after.cert) {
				t.Fatal("not reissued")
			}
			if name != "within renewal window" && string(before.ca) != string(after.ca) {
				t.Error("the CA changed although it was still valid")
			}
			_, other, _ := secretTLS(t, s, "f5-utils", "bnk-single-cert")
			if string(other.cert) != string(after.cert) {
				t.Error("the namespaces were left with different certificates")
			}
		})
	}
	// The extra name is in the reissued certificate.
	s, env, _ := newFake(t)
	cfg := certCfg(IssuerSelfSigned, "f5-bnk")
	cfg.ExtraDNS = []string{"bnk.example.com"}
	cfg.IPs = []string{"10.0.0.7"}
	runCert(t, s, env, cfg)
	leaf, _, _ := secretTLS(t, s, "f5-bnk", "bnk-single-cert")
	if !slices.Contains(leaf.DNSNames, "bnk.example.com") || len(leaf.IPAddresses) != 1 || leaf.IPAddresses[0].String() != "10.0.0.7" {
		t.Errorf("extra names: %v %v", leaf.DNSNames[len(leaf.DNSNames)-1], leaf.IPAddresses)
	}
}

// ca: the operator's CA signs the certificate; ca.crt is that CA.
func TestSingleCertSignedByTheOperatorsCA(t *testing.T) {
	s, env, _ := newFake(t)
	caPEM, keyPEM := operatorCA(t, true)
	s.Put("", "v1", "secrets", secretObj("roksbnkargoctl-check", "bnk-single-cert-source", map[string][]byte{"tls.crt": caPEM, "tls.key": keyPEM}))
	cfg := certCfg(IssuerCA, "f5-bnk")
	cfg.SourceSecret = "roksbnkargoctl-check/bnk-single-cert-source"
	if res := runCert(t, s, env, cfg); res.Failed() {
		t.Fatal(res.Failures())
	}
	leaf, m, _ := secretTLS(t, s, "f5-bnk", "bnk-single-cert")
	if string(m.ca) != string(caPEM) {
		t.Error("ca.crt is not the operator's CA")
	}
	if err := leaf.CheckSignatureFrom(mustCert(t, caPEM)); err != nil {
		t.Errorf("not signed by the operator's CA: %v", err)
	}
	// A certificate that is not a CA is refused.
	notCA, notCAKey := operatorCA(t, false)
	s.Put("", "v1", "secrets", secretObj("roksbnkargoctl-check", "bnk-single-cert-source", map[string][]byte{"tls.crt": notCA, "tls.key": notCAKey}))
	res := runCert(t, s, env, cfg)
	if !res.Failed() || !strings.Contains(strings.Join(res.Failures(), " "), "is not a CA") {
		t.Errorf("non-CA source: %v", res.Failures())
	}
}

// provided: the operator's certificate is checked (key, chain, names) and
// copied unchanged; one that lacks BNK's names is refused.
func TestSingleCertProvided(t *testing.T) {
	s, env, _ := newFake(t)
	ca, caKey := testCAKey(t)
	good := issueFor(t, ca, caKey, SingleCertSANs([]string{"f5-bnk"}, nil))
	s.Put("", "v1", "secrets", secretObj("roksbnkargoctl-check", "mine", map[string][]byte{"tls.crt": good.cert, "tls.key": good.key, "ca.crt": good.ca}))
	cfg := certCfg(IssuerProvided, "f5-bnk")
	cfg.SourceSecret = "roksbnkargoctl-check/mine"
	if res := runCert(t, s, env, cfg); res.Failed() {
		t.Fatal(res.Failures())
	}
	_, m, _ := secretTLS(t, s, "f5-bnk", "bnk-single-cert")
	if string(m.cert) != string(good.cert) || string(m.key) != string(good.key) || string(m.ca) != string(good.ca) {
		t.Error("the provided certificate was not copied unchanged")
	}

	short := issueFor(t, ca, caKey, []string{"f5-tmm"})
	s.Put("", "v1", "secrets", secretObj("roksbnkargoctl-check", "mine", map[string][]byte{"tls.crt": short.cert, "tls.key": short.key, "ca.crt": short.ca}))
	res := runCert(t, s, env, cfg)
	if !res.Failed() || !strings.Contains(strings.Join(res.Failures(), " "), "does not cover names BNK uses") {
		t.Errorf("a certificate without BNK's names was accepted: %v", res.Failures())
	}
	_, still, _ := secretTLS(t, s, "f5-bnk", "bnk-single-cert")
	if string(still.cert) != string(good.cert) {
		t.Error("a refused certificate replaced the good one")
	}
}

// F5's list, for one namespace or two, without duplicates.
func TestSingleCertSANs(t *testing.T) {
	one := SingleCertSANs([]string{"f5-bnk"}, nil)
	two := SingleCertSANs([]string{"f5-bnk", "f5-utils"}, []string{"f5-tmm", "x.example.com"})
	seen := map[string]bool{}
	for _, n := range two {
		if seen[n] {
			t.Fatalf("duplicate SAN %s", n)
		}
		seen[n] = true
	}
	if len(one) < 70 || len(two) <= len(one) || two[len(two)-1] != "x.example.com" {
		t.Errorf("one namespace %d names, two %d, last %q", len(one), len(two), two[len(two)-1])
	}
	for _, n := range one {
		if strings.Contains(n, "f5-utils") || strings.Contains(n, "<ns>") {
			t.Errorf("one-namespace list has %s", n)
		}
	}
}

// The default is F5's RSA 4096, encoded PKCS#1 as cert-manager writes RSA keys.
func TestSingleCertDefaultKeyIsRSA4096PKCS1(t *testing.T) {
	s, env, _ := newFake(t)
	cfg := certCfg(IssuerSelfSigned, "f5-bnk")
	cfg.KeyType, cfg.KeyBits = "rsa", 4096
	runCert(t, s, env, cfg)
	leaf, m, _ := secretTLS(t, s, "f5-bnk", "bnk-single-cert")
	if k, ok := leaf.PublicKey.(*rsa.PublicKey); !ok || k.N.BitLen() != 4096 {
		t.Errorf("key %T", leaf.PublicKey)
	}
	if b, _ := pem.Decode(m.key); b == nil || b.Type != "RSA PRIVATE KEY" {
		t.Errorf("key block %v", b)
	}
}

func passes(r *Result) []string {
	var out []string
	for _, f := range r.Findings {
		out = append(out, f.Detail)
	}
	return out
}

func mustCert(t *testing.T, p []byte) *x509.Certificate {
	t.Helper()
	b, _ := pem.Decode(p)
	c, err := x509.ParseCertificate(b.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func secretObj(ns, name string, data map[string][]byte) kube.Object {
	o := kubefake.Obj("v1", "Secret", ns, name)
	d := map[string]any{}
	for k, v := range data {
		d[k] = base64.StdEncoding.EncodeToString(v)
	}
	o["data"] = d
	return o
}

func testCAKey(t *testing.T) (*x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "operator-ca"},
		NotBefore: certNow.Add(-time.Hour), NotAfter: certNow.Add(5 * 365 * 24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, k.Public(), k)
	if err != nil {
		t.Fatal(err)
	}
	c, _ := x509.ParseCertificate(der)
	return c, k
}

func operatorCA(t *testing.T, isCA bool) (certPEM, keyPEM []byte) {
	t.Helper()
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "operator-ca"},
		NotBefore: certNow.Add(-time.Hour), NotAfter: certNow.Add(5 * 365 * 24 * time.Hour),
		IsCA: isCA, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, k.Public(), k)
	if err != nil {
		t.Fatal(err)
	}
	return pemCert(der), pemKey(k)
}

func issueFor(t *testing.T, ca *x509.Certificate, caKey *ecdsa.PrivateKey, sans []string) *tlsMaterial {
	t.Helper()
	cfg := certCfg(IssuerCA)
	cfg.CommonName = "operator"
	m, err := issueLeaf(cfg, sans, ca, caKey)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// The renewal window alone reissues the certificate: a long-lived CA (yours)
// stays, the short-lived certificate is renewed once within renew-before.
func TestSingleCertRenewsWithinTheWindow(t *testing.T) {
	s, env, _ := newFake(t)
	caPEM, keyPEM := operatorCA(t, true) // valid five years
	s.Put("", "v1", "secrets", secretObj("roksbnkargoctl-check", "src", map[string][]byte{"tls.crt": caPEM, "tls.key": keyPEM}))
	cfg := certCfg(IssuerCA, "f5-bnk")
	cfg.SourceSecret, cfg.Validity = "roksbnkargoctl-check/src", 40*24*time.Hour
	runCert(t, s, env, cfg)
	_, first, _ := secretTLS(t, s, "f5-bnk", "bnk-single-cert")

	early := certNow.Add(5 * 24 * time.Hour) // 35 days left, outside the 30-day window
	cfg.Now = func() time.Time { return early }
	runCert(t, s, env, cfg)
	if _, m, _ := secretTLS(t, s, "f5-bnk", "bnk-single-cert"); string(m.cert) != string(first.cert) {
		t.Fatal("reissued outside the renewal window")
	}
	late := certNow.Add(15 * 24 * time.Hour) // 25 days left, inside it
	cfg.Now = func() time.Time { return late }
	runCert(t, s, env, cfg)
	if _, m, _ := secretTLS(t, s, "f5-bnk", "bnk-single-cert"); string(m.cert) == string(first.cert) {
		t.Fatal("not renewed inside the renewal window")
	}
}

// Two namespaces holding different certificates for the same settings are
// made identical again (every component must present and trust the same one).
func TestSingleCertEqualisesTheNamespaces(t *testing.T) {
	s, env, _ := newFake(t)
	cfg := certCfg(IssuerSelfSigned, "f5-bnk", "f5-utils")
	runCert(t, s, env, cfg)
	// Replace f5-utils' certificate with another valid one, same annotation.
	other := s.Get("", "v1", "secrets", "f5-utils", "bnk-single-cert")
	ca, caKey := testCAKey(t)
	m := issueFor(t, ca, caKey, SingleCertSANs(cfg.Namespaces, nil))
	other["data"].(map[string]any)["tls.crt"] = base64.StdEncoding.EncodeToString(m.cert)
	other["data"].(map[string]any)["tls.key"] = base64.StdEncoding.EncodeToString(m.key)
	s.Put("", "v1", "secrets", other)
	runCert(t, s, env, cfg)
	_, a, _ := secretTLS(t, s, "f5-bnk", "bnk-single-cert")
	_, b, _ := secretTLS(t, s, "f5-utils", "bnk-single-cert")
	if string(a.cert) != string(b.cert) || string(a.key) != string(b.key) {
		t.Fatal("the namespaces were left holding different certificates")
	}
}

// A Secret edited in place (kubectl edit keeps the annotation) holding another
// CA's certificate is not kept and copied: it is reissued, and every namespace
// ends with a certificate that verifies against its own ca.crt.
func TestSingleCertReplacesAnEditedSecret(t *testing.T) {
	s, env, _ := newFake(t)
	cfg := certCfg(IssuerSelfSigned, "f5-bnk", "f5-utils")
	runCert(t, s, env, cfg)
	edited := s.Get("", "v1", "secrets", "f5-bnk", "bnk-single-cert")
	ca, caKey := testCAKey(t)
	m := issueFor(t, ca, caKey, SingleCertSANs(cfg.Namespaces, nil))
	edited["data"].(map[string]any)["tls.crt"] = base64.StdEncoding.EncodeToString(m.cert)
	edited["data"].(map[string]any)["tls.key"] = base64.StdEncoding.EncodeToString(m.key)
	s.Put("", "v1", "secrets", edited)
	res := runCert(t, s, env, cfg)
	if !strings.Contains(strings.Join(passes(res), "\n"), "not signed by the current CA") {
		t.Fatalf("an edited Secret was kept: %v", passes(res))
	}
	for _, ns := range cfg.Namespaces {
		leaf, got, _ := secretTLS(t, s, ns, "bnk-single-cert")
		if string(got.cert) == string(m.cert) {
			t.Errorf("%s holds the edited certificate", ns)
		}
		if err := leaf.CheckSignatureFrom(mustCert(t, got.ca)); err != nil {
			t.Errorf("%s: tls.crt does not verify against its ca.crt: %v", ns, err)
		}
	}
}

// issuer ca: a CA that has expired, or expires within the renewal window, is
// refused with a failure and nothing is written: it would issue a certificate
// already (or soon) invalid, and reissue it on every sync.
func TestSingleCertRefusesAnExpiringCA(t *testing.T) {
	for name, notAfter := range map[string]time.Time{"expired": certNow.Add(-24 * time.Hour), "within the window": certNow.Add(20 * 24 * time.Hour)} {
		s, env, _ := newFake(t)
		k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		tmpl := &x509.Certificate{SerialNumber: big.NewInt(3), Subject: pkix.Name{CommonName: "old-ca"},
			NotBefore: certNow.Add(-365 * 24 * time.Hour), NotAfter: notAfter, IsCA: true, BasicConstraintsValid: true,
			KeyUsage: x509.KeyUsageCertSign}
		der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, k.Public(), k)
		s.Put("", "v1", "secrets", secretObj("roksbnkargoctl-check", "src", map[string][]byte{"tls.crt": pemCert(der), "tls.key": pemKey(k)}))
		cfg := certCfg(IssuerCA, "f5-bnk")
		cfg.SourceSecret = "roksbnkargoctl-check/src"
		res := runCert(t, s, env, cfg)
		if !res.Failed() || !strings.Contains(strings.Join(res.Failures(), " "), "renewal window") {
			t.Errorf("%s: %v", name, res.Failures())
		}
		if s.Get("", "v1", "secrets", "f5-bnk", "bnk-single-cert") != nil {
			t.Errorf("%s: a certificate was written from a CA that cannot sign", name)
		}
	}
}

// self-signed: the kept CA is renewed within its renewal window, and the
// certificate is reissued by the new CA.
func TestSingleCertRenewsTheSelfSignedCA(t *testing.T) {
	s, env, _ := newFake(t)
	cfg := certCfg(IssuerSelfSigned, "f5-bnk")
	cfg.Validity = 40 * 24 * time.Hour
	runCert(t, s, env, cfg)
	_, first, _ := secretTLS(t, s, "f5-bnk", "bnk-single-cert")
	later := certNow.Add(15 * 24 * time.Hour)
	cfg.Now = func() time.Time { return later }
	runCert(t, s, env, cfg)
	leaf, m, _ := secretTLS(t, s, "f5-bnk", "bnk-single-cert")
	if string(m.ca) == string(first.ca) {
		t.Fatal("the CA was not renewed within its window")
	}
	if err := leaf.CheckSignatureFrom(mustCert(t, m.ca)); err != nil {
		t.Errorf("not issued by the renewed CA: %v", err)
	}
	kept := s.Get("", "v1", "secrets", "roksbnkargoctl-check", "bnk-single-cert-ca")
	ks, _ := tlsFromSecret(kept, false)
	if string(ks.cert) != string(m.ca) {
		t.Error("the renewed CA is not the one kept for the next run")
	}
}

// issuer ca: replacing the CA (a renewed or another one) reissues the
// certificate, signed by the new CA.
func TestSingleCertFollowsANewCA(t *testing.T) {
	s, env, _ := newFake(t)
	put := func() []byte {
		c, k := operatorCA(t, true)
		s.Put("", "v1", "secrets", secretObj("roksbnkargoctl-check", "src", map[string][]byte{"tls.crt": c, "tls.key": k}))
		return c
	}
	put()
	cfg := certCfg(IssuerCA, "f5-bnk")
	cfg.SourceSecret = "roksbnkargoctl-check/src"
	runCert(t, s, env, cfg)
	newCA := put()
	runCert(t, s, env, cfg)
	leaf, m, _ := secretTLS(t, s, "f5-bnk", "bnk-single-cert")
	if string(m.ca) != string(newCA) || leaf.CheckSignatureFrom(mustCert(t, newCA)) != nil {
		t.Error("the certificate was not reissued by the new CA")
	}
}

// A certificate never outlives its CA; a bad --ip is an error, not dropped.
func TestSingleCertLimits(t *testing.T) {
	s, env, _ := newFake(t)
	caPEM, keyPEM := operatorCA(t, true) // five years
	s.Put("", "v1", "secrets", secretObj("roksbnkargoctl-check", "src", map[string][]byte{"tls.crt": caPEM, "tls.key": keyPEM}))
	cfg := certCfg(IssuerCA, "f5-bnk") // ten years asked
	cfg.SourceSecret = "roksbnkargoctl-check/src"
	runCert(t, s, env, cfg)
	leaf, _, _ := secretTLS(t, s, "f5-bnk", "bnk-single-cert")
	if !leaf.NotAfter.Equal(mustCert(t, caPEM).NotAfter) {
		t.Errorf("certificate valid until %s, CA until %s", leaf.NotAfter, mustCert(t, caPEM).NotAfter)
	}
	if k, ok := leaf.PublicKey.(*ecdsa.PublicKey); !ok || leaf.KeyUsage&x509.KeyUsageKeyEncipherment != 0 {
		t.Errorf("ECDSA certificate with key usage %v (%T)", leaf.KeyUsage, k)
	}
	cfg.IPs = []string{"10.0.0.300"}
	if err := SingleCert(context.Background(), env, cfg, NewResult("cert", &tlog{t: t})); err == nil || !strings.Contains(err.Error(), "not an IP address") {
		t.Errorf("bad --ip: %v", err)
	}
}

// A namespace the Secret cannot be written to fails the check: it is not
// reported current.
func TestSingleCertFailsWhenAWriteFails(t *testing.T) {
	s, env, _ := newFake(t)
	s.Hook = func(_ *kubefake.Server, r kubefake.Request) *kubefake.Reply {
		if r.Method == "PATCH" && strings.Contains(r.Path, "/namespaces/f5-utils/secrets/") {
			rep := kubefake.Status(403, "Forbidden", "no")
			return &rep
		}
		return nil
	}
	res := runCert(t, s, env, certCfg(IssuerSelfSigned, "f5-bnk", "f5-utils"))
	if !res.Failed() || !strings.Contains(strings.Join(res.Failures(), " "), "f5-utils/bnk-single-cert") {
		t.Errorf("a failed write passed: %v", res.Failures())
	}
}

// provided: a copy edited in place (annotation kept) is replaced by the
// operator's certificate again.
func TestSingleCertRestoresAnEditedProvidedCopy(t *testing.T) {
	s, env, _ := newFake(t)
	ca, caKey := testCAKey(t)
	good := issueFor(t, ca, caKey, SingleCertSANs([]string{"f5-bnk"}, nil))
	s.Put("", "v1", "secrets", secretObj("roksbnkargoctl-check", "mine", map[string][]byte{"tls.crt": good.cert, "tls.key": good.key, "ca.crt": good.ca}))
	cfg := certCfg(IssuerProvided, "f5-bnk")
	cfg.SourceSecret = "roksbnkargoctl-check/mine"
	runCert(t, s, env, cfg)
	other := issueFor(t, ca, caKey, SingleCertSANs([]string{"f5-bnk"}, nil))
	edited := s.Get("", "v1", "secrets", "f5-bnk", "bnk-single-cert")
	edited["data"].(map[string]any)["tls.crt"] = base64.StdEncoding.EncodeToString(other.cert)
	edited["data"].(map[string]any)["tls.key"] = base64.StdEncoding.EncodeToString(other.key)
	s.Put("", "v1", "secrets", edited)
	runCert(t, s, env, cfg)
	if _, m, _ := secretTLS(t, s, "f5-bnk", "bnk-single-cert"); string(m.cert) != string(good.cert) {
		t.Error("an edited copy of the provided certificate was kept")
	}
}

// issuer ca: a CA renewed with the same key (a new certificate, old key) still
// reaches ca.crt; the leaf's signature alone would not tell.
func TestSingleCertFollowsACARenewedWithTheSameKey(t *testing.T) {
	s, env, _ := newFake(t)
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	caPEM := func(serial int64, years int) []byte {
		tmpl := &x509.Certificate{SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: "operator-ca"},
			NotBefore: certNow.Add(-time.Hour), NotAfter: certNow.Add(time.Duration(years) * 365 * 24 * time.Hour),
			IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
		der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, k.Public(), k)
		return pemCert(der)
	}
	s.Put("", "v1", "secrets", secretObj("roksbnkargoctl-check", "src", map[string][]byte{"tls.crt": caPEM(10, 2), "tls.key": pemKey(k)}))
	cfg := certCfg(IssuerCA, "f5-bnk")
	cfg.SourceSecret = "roksbnkargoctl-check/src"
	runCert(t, s, env, cfg)
	renewed := caPEM(11, 5)
	s.Put("", "v1", "secrets", secretObj("roksbnkargoctl-check", "src", map[string][]byte{"tls.crt": renewed, "tls.key": pemKey(k)}))
	runCert(t, s, env, cfg)
	if _, m, _ := secretTLS(t, s, "f5-bnk", "bnk-single-cert"); string(m.ca) != string(renewed) {
		t.Error("ca.crt still holds the CA certificate before renewal")
	}
}
