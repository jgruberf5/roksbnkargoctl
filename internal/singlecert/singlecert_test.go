package singlecert

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"strings"
	"testing"
	"time"
	"unicode/utf16"
)

var now = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

type issued struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
}

func (i issued) certPEM() []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: i.cert.Raw})
}

func (i issued) keyPEM() []byte {
	b, _ := x509.MarshalECPrivateKey(i.key)
	return pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: b})
}

var serialN int64

// issue makes a certificate signed by parent (self-signed when parent is nil).
func issue(t *testing.T, parent *issued, cn string, isCA bool, from, to time.Time, dns ...string) issued {
	t.Helper()
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	serialN++
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(serialN), Subject: pkix.Name{CommonName: cn},
		NotBefore: from, NotAfter: to, IsCA: isCA, BasicConstraintsValid: true, DNSNames: dns,
		KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign}
	signer, signerKey := tmpl, crypto.Signer(k)
	if parent != nil {
		signer, signerKey = parent.cert, parent.key
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, signer, k.Public(), signerKey)
	if err != nil {
		t.Fatal(err)
	}
	c, _ := x509.ParseCertificate(der)
	return issued{c, k}
}

var (
	year = 365 * 24 * time.Hour
	day  = 24 * time.Hour
)

// The CA used must be valid now and beyond the renewal window: an expired one
// issued an already-expired certificate and reissued it on every sync.
func TestCARefusesWhatCannotSign(t *testing.T) {
	good := issue(t, nil, "ca", true, now.Add(-day), now.Add(5*year))
	if _, _, err := CA(good.certPEM(), good.keyPEM(), now, 30*day); err != nil {
		t.Fatalf("a valid CA was refused: %v", err)
	}
	for name, tc := range map[string]struct {
		c    issued
		want string
	}{
		"expired":           {issue(t, nil, "ca", true, now.Add(-year), now.Add(-day)), "the CA expired"},
		"within the window": {issue(t, nil, "ca", true, now.Add(-year), now.Add(20*day)), "within the renewal window"},
		"not yet valid":     {issue(t, nil, "ca", true, now.Add(day), now.Add(year)), "not valid until"},
		"not a CA":          {issue(t, nil, "leaf", false, now.Add(-day), now.Add(year)), "not a CA"},
	} {
		if _, _, err := CA(tc.c.certPEM(), tc.c.keyPEM(), now, 30*day); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v", name, err)
		}
	}
	other := issue(t, nil, "ca", true, now.Add(-day), now.Add(5*year))
	if _, _, err := CA(good.certPEM(), other.keyPEM(), now, 30*day); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Errorf("a key that is not the CA's: %v", err)
	}
}

// openssl ecparam -genkey writes an EC PARAMETERS block before the key; PKCS#8
// keys parse; an encrypted key is named as such.
func TestKeyFormats(t *testing.T) {
	c := issue(t, nil, "ca", true, now.Add(-day), now.Add(year))
	params := pem.EncodeToMemory(&pem.Block{Type: "EC PARAMETERS", Bytes: []byte{0x06, 0x08, 0x2a, 0x86, 0x48, 0xce, 0x3d, 0x03, 0x01, 0x07}})
	if _, _, _, err := Pair(c.certPEM(), append(params, c.keyPEM()...)); err != nil {
		t.Errorf("openssl ecparam -genkey output: %v", err)
	}
	p8, _ := x509.MarshalPKCS8PrivateKey(c.key)
	if _, _, _, err := Pair(c.certPEM(), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: p8})); err != nil {
		t.Errorf("PKCS#8: %v", err)
	}
	if _, err := Key(pem.EncodeToMemory(&pem.Block{Type: "ENCRYPTED PRIVATE KEY", Bytes: []byte{1}})); err == nil || !strings.Contains(err.Error(), "encrypted") {
		t.Errorf("encrypted key: %v", err)
	}
}

// provided: a full-chain tls.crt (leaf, then intermediate) against a root-only
// ca.crt verifies, as cert-manager and most CAs write it; a certificate from
// another CA, or one lacking names, is refused.
func TestProvided(t *testing.T) {
	names := []string{"f5-tmm", "f5-spk-cwc.f5-bnk.svc", "10.0.0.7"}
	root := issue(t, nil, "root", true, now.Add(-day), now.Add(5*year))
	inter := issue(t, &root, "intermediate", true, now.Add(-day), now.Add(4*year))
	leaf := issue(t, &inter, "f5net", false, now.Add(-day), now.Add(year), "f5-tmm", "*.f5-bnk.svc")
	withIP := func() issued {
		k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		tmpl := &x509.Certificate{SerialNumber: big.NewInt(99), Subject: pkix.Name{CommonName: "f5net"},
			NotBefore: now.Add(-day), NotAfter: now.Add(year), DNSNames: []string{"f5-tmm", "*.f5-bnk.svc"},
			IPAddresses: []net.IP{net.ParseIP("10.0.0.7")}}
		der, err := x509.CreateCertificate(rand.Reader, tmpl, inter.cert, k.Public(), inter.key)
		if err != nil {
			t.Fatal(err)
		}
		c, _ := x509.ParseCertificate(der)
		return issued{c, k}
	}()
	chain := append(withIP.certPEM(), inter.certPEM()...)
	if err := Provided(chain, withIP.keyPEM(), root.certPEM(), names, now); err != nil {
		t.Errorf("full chain against the root: %v", err)
	}
	if err := Provided(withIP.certPEM(), withIP.keyPEM(), append(inter.certPEM(), root.certPEM()...), names, now); err != nil {
		t.Errorf("leaf alone, intermediate and root in ca.crt: %v", err)
	}
	stranger := issue(t, nil, "other", true, now.Add(-day), now.Add(5*year))
	if err := Provided(chain, withIP.keyPEM(), stranger.certPEM(), names, now); err == nil || !strings.Contains(err.Error(), "does not verify") {
		t.Errorf("another CA's ca.crt: %v", err)
	}
	if err := Provided(chain, leaf.keyPEM(), root.certPEM(), names, now); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Errorf("a key that is not the certificate's: %v", err)
	}
	if err := Provided(chain, withIP.keyPEM(), root.certPEM(), append(names, "f5-observer"), now); err == nil || !strings.Contains(err.Error(), "f5-observer") {
		t.Errorf("a missing name: %v", err)
	}
	if err := Provided(chain, withIP.keyPEM(), root.certPEM(), append(names, "10.0.0.8"), now); err == nil || !strings.Contains(err.Error(), "10.0.0.8") {
		t.Errorf("a missing IP address: %v", err)
	}
	if err := Provided(chain, withIP.keyPEM(), root.certPEM(), names, now.Add(2*year)); err == nil {
		t.Error("an expired certificate was accepted")
	}
}

// A CA whose key usage lacks keyCertSign signs certificates nothing verifies
// (and the check would reissue on every sync); a CA minted a moment ahead of
// this clock is accepted within Skew.
func TestCAKeyUsageAndSkew(t *testing.T) {
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	mk := func(ku x509.KeyUsage, from time.Time) issued {
		tmpl := &x509.Certificate{SerialNumber: big.NewInt(50), Subject: pkix.Name{CommonName: "ca"},
			NotBefore: from, NotAfter: now.Add(5 * year), IsCA: true, BasicConstraintsValid: true, KeyUsage: ku}
		der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, k.Public(), k)
		c, _ := x509.ParseCertificate(der)
		return issued{c, k}
	}
	noSign := mk(x509.KeyUsageDigitalSignature|x509.KeyUsageCRLSign, now.Add(-day))
	if _, _, err := CA(noSign.certPEM(), noSign.keyPEM(), now, 30*day); err == nil || !strings.Contains(err.Error(), "keyCertSign") {
		t.Errorf("a CA without keyCertSign: %v", err)
	}
	ahead := mk(x509.KeyUsageCertSign, now.Add(2*time.Minute))
	if _, _, err := CA(ahead.certPEM(), ahead.keyPEM(), now, 30*day); err != nil {
		t.Errorf("a CA two minutes ahead: %v", err)
	}
	far := mk(x509.KeyUsageCertSign, now.Add(Skew+time.Minute))
	if _, _, err := CA(far.certPEM(), far.keyPEM(), now, 30*day); err == nil {
		t.Error("a CA beyond the skew was accepted")
	}
}

// provided, judged as OpenSSL judges it: both serverAuth and clientAuth
// (anyExtendedKeyUsage is neither to OpenSSL; no EKU at all is both), and a
// chain ending at a self-signed root in ca.crt — an intermediate alone, or
// beside an unrelated root, is "unable to get issuer certificate" there.
func TestProvidedUsageAndAnchors(t *testing.T) {
	root := issue(t, nil, "root", true, now.Add(-day), now.Add(5*year))
	inter := issue(t, &root, "intermediate", true, now.Add(-day), now.Add(4*year))
	leaf := func(eku ...x509.ExtKeyUsage) issued {
		k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		tmpl := &x509.Certificate{SerialNumber: big.NewInt(60), Subject: pkix.Name{CommonName: "f5net"},
			NotBefore: now.Add(-day), NotAfter: now.Add(year), DNSNames: []string{"f5-tmm"}, ExtKeyUsage: eku}
		der, _ := x509.CreateCertificate(rand.Reader, tmpl, inter.cert, k.Public(), inter.key)
		c, _ := x509.ParseCertificate(der)
		return issued{c, k}
	}
	full := append(inter.certPEM(), root.certPEM()...)
	names := []string{"f5-tmm"}
	for name, tc := range map[string]struct {
		l  issued
		ok bool
	}{
		"server and client": {leaf(x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth), true},
		"no EKU":            {leaf(), true},
		"server only":       {leaf(x509.ExtKeyUsageServerAuth), false},
		"client only":       {leaf(x509.ExtKeyUsageClientAuth), false},
		"any only":          {leaf(x509.ExtKeyUsageAny), false},
		// Go reads anyExtendedKeyUsage as client auth; OpenSSL does not.
		"any and server": {leaf(x509.ExtKeyUsageAny, x509.ExtKeyUsageServerAuth), false},
	} {
		err := Provided(tc.l.certPEM(), tc.l.keyPEM(), full, names, now)
		if (err == nil) != tc.ok {
			t.Errorf("%s: %v", name, err)
		}
	}
	both := leaf(x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth)
	unrelated := issue(t, nil, "unrelated", true, now.Add(-day), now.Add(5*year))
	if err := Provided(both.certPEM(), both.keyPEM(), inter.certPEM(), names, now); err == nil || !strings.Contains(err.Error(), "no self-signed root") {
		t.Errorf("intermediate-only ca.crt: %v", err)
	}
	if err := Provided(both.certPEM(), both.keyPEM(), append(unrelated.certPEM(), inter.certPEM()...), names, now); err == nil {
		t.Error("an unrelated root beside the issuing intermediate was accepted (OpenSSL: unable to get issuer certificate)")
	}
}

// A CA with no keyUsage extension at all (OpenSSL's default v3_ca writes
// none) may sign; the clock-skew allowance is five minutes.
func TestCAWithoutKeyUsageAndSkewWidth(t *testing.T) {
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	mk := func(from time.Time) issued {
		tmpl := &x509.Certificate{SerialNumber: big.NewInt(70), Subject: pkix.Name{CommonName: "ca"},
			NotBefore: from, NotAfter: now.Add(5 * year), IsCA: true, BasicConstraintsValid: true}
		der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, k.Public(), k)
		c, _ := x509.ParseCertificate(der)
		return issued{c, k}
	}
	if c := mk(now.Add(-day)); c.cert.KeyUsage != 0 {
		t.Fatalf("test CA has key usage %v", c.cert.KeyUsage)
	} else if _, _, err := CA(c.certPEM(), c.keyPEM(), now, 30*day); err != nil {
		t.Errorf("a CA without a keyUsage extension: %v", err)
	}
	if c := mk(now.Add(4*time.Minute + 50*time.Second)); true {
		if _, _, err := CA(c.certPEM(), c.keyPEM(), now, 30*day); err != nil {
			t.Errorf("a CA under five minutes ahead: %v", err)
		}
	}
	if c := mk(now.Add(5*time.Minute + 10*time.Second)); true {
		if _, _, err := CA(c.certPEM(), c.keyPEM(), now, 30*day); err == nil {
			t.Error("a CA over five minutes ahead was accepted")
		}
	}
}

// A root signed with SHA-1 (older corporate roots) is a root, as OpenSSL
// holds: Go's CheckSignature verifies SHA-1 (CheckSignatureFrom, used once,
// refused it and called the root an intermediate).
func TestProvidedSHA1Root(t *testing.T) {
	rk, _ := rsa.GenerateKey(rand.Reader, 2048)
	rt := &x509.Certificate{SerialNumber: big.NewInt(80), Subject: pkix.Name{CommonName: "old-root"},
		NotBefore: now.Add(-day), NotAfter: now.Add(5 * year), IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign, SignatureAlgorithm: x509.SHA1WithRSA}
	der, err := x509.CreateCertificate(rand.Reader, rt, rt, rk.Public(), rk)
	if err != nil {
		t.Fatal(err)
	}
	root, _ := x509.ParseCertificate(der)
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	lt := &x509.Certificate{SerialNumber: big.NewInt(81), Subject: pkix.Name{CommonName: "f5net"},
		NotBefore: now.Add(-day), NotAfter: now.Add(year), DNSNames: []string{"f5-tmm"}}
	ld, err := x509.CreateCertificate(rand.Reader, lt, root, k.Public(), rk)
	if err != nil {
		t.Fatal(err)
	}
	leaf, _ := x509.ParseCertificate(ld)
	l := issued{leaf, k}
	if err := Provided(l.certPEM(), l.keyPEM(), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), []string{"f5-tmm"}, now); err != nil {
		t.Errorf("a SHA-1 root with a SHA-256 leaf: %v", err)
	}
}

// A root is judged by its signature, not by its names: a root whose issuer
// differs from its subject only in case (OpenSSL compares canonical names, and
// accepts it) is a root; a certificate naming itself as issuer but signed by
// another key is not — stricter than OpenSSL, which accepts it unless
// -check_ss_sig.
func TestProvidedRootIsJudgedBySignature(t *testing.T) {
	leafFrom := func(parent *x509.Certificate, parentKey crypto.Signer) issued {
		k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		lt := &x509.Certificate{SerialNumber: big.NewInt(91), Subject: pkix.Name{CommonName: "f5net"},
			NotBefore: now.Add(-day), NotAfter: now.Add(year), DNSNames: []string{"f5-tmm"}}
		der, err := x509.CreateCertificate(rand.Reader, lt, parent, k.Public(), parentKey)
		if err != nil {
			t.Fatal(err)
		}
		c, _ := x509.ParseCertificate(der)
		return issued{c, k}
	}
	rootTmpl := func(cn string) *x509.Certificate {
		return &x509.Certificate{SerialNumber: big.NewInt(90), Subject: pkix.Name{CommonName: cn},
			NotBefore: now.Add(-day), NotAfter: now.Add(5 * year), IsCA: true, BasicConstraintsValid: true,
			KeyUsage: x509.KeyUsageCertSign}
	}
	// Issuer "CORP ROOT", subject "Corp Root", signed by its own key.
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	der, _ := x509.CreateCertificate(rand.Reader, rootTmpl("Corp Root"), rootTmpl("CORP ROOT"), k.Public(), k)
	caseRoot, _ := x509.ParseCertificate(der)
	l := leafFrom(caseRoot, k)
	if err := Provided(l.certPEM(), l.keyPEM(), issued{caseRoot, k}.certPEM(), []string{"f5-tmm"}, now); err != nil {
		t.Errorf("a root whose issuer differs only in case: %v", err)
	}
	// Subject = issuer "Fake Root", but signed by another key.
	other, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	der, _ = x509.CreateCertificate(rand.Reader, rootTmpl("Fake Root"), rootTmpl("Fake Root"), k.Public(), other)
	fake, _ := x509.ParseCertificate(der)
	l = leafFrom(fake, k)
	if err := Provided(l.certPEM(), l.keyPEM(), issued{fake, k}.certPEM(), []string{"f5-tmm"}, now); err == nil {
		t.Error("a certificate naming itself as issuer but signed by another key was taken for a root")
	}
}

// The SHA-1 allowance is for roots only: a SHA-1-signed intermediate (issuer
// another name) alone in ca.crt is no anchor, as OpenSSL holds.
func TestProvidedSHA1IntermediateIsNoRoot(t *testing.T) {
	rk, _ := rsa.GenerateKey(rand.Reader, 2048)
	rt := &x509.Certificate{SerialNumber: big.NewInt(100), Subject: pkix.Name{CommonName: "root"},
		NotBefore: now.Add(-day), NotAfter: now.Add(5 * year), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	rd, _ := x509.CreateCertificate(rand.Reader, rt, rt, rk.Public(), rk)
	root, _ := x509.ParseCertificate(rd)
	ik, _ := rsa.GenerateKey(rand.Reader, 2048)
	it := &x509.Certificate{SerialNumber: big.NewInt(101), Subject: pkix.Name{CommonName: "intermediate"},
		NotBefore: now.Add(-day), NotAfter: now.Add(4 * year), IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign, SignatureAlgorithm: x509.SHA1WithRSA}
	id, err := x509.CreateCertificate(rand.Reader, it, root, ik.Public(), rk)
	if err != nil {
		t.Fatal(err)
	}
	inter, _ := x509.ParseCertificate(id)
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	lt := &x509.Certificate{SerialNumber: big.NewInt(102), Subject: pkix.Name{CommonName: "f5net"},
		NotBefore: now.Add(-day), NotAfter: now.Add(year), DNSNames: []string{"f5-tmm"}}
	ld, _ := x509.CreateCertificate(rand.Reader, lt, inter, k.Public(), ik)
	leaf, _ := x509.ParseCertificate(ld)
	l := issued{leaf, k}
	if err := Provided(l.certPEM(), l.keyPEM(), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: id}), []string{"f5-tmm"}, now); err == nil || !strings.Contains(err.Error(), "no self-signed root") {
		t.Errorf("a SHA-1 intermediate alone in ca.crt: %v", err)
	}
}

// A self-signed certificate given as its own ca.crt is its own anchor, for
// OpenSSL (verify OK) and Go's Verify alike, whatever its CA constraints:
// judging "self-signed" with CheckSignatureFrom refused it, since that also
// demands the parent be a CA.
func TestProvidedSelfSignedLeafAsItsOwnCA(t *testing.T) {
	for name, tc := range map[string]struct {
		bc   bool
		ku   x509.KeyUsage
		isCA bool
	}{
		"CA:false":           {true, x509.KeyUsageDigitalSignature, false},
		"no basicConstraint": {false, x509.KeyUsageDigitalSignature, false},
		"no key usage":       {false, 0, false},
	} {
		k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		tmpl := &x509.Certificate{SerialNumber: big.NewInt(110), Subject: pkix.Name{CommonName: "f5net"},
			NotBefore: now.Add(-day), NotAfter: now.Add(year), DNSNames: []string{"f5-tmm"},
			BasicConstraintsValid: tc.bc, IsCA: tc.isCA, KeyUsage: tc.ku,
			ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}}
		der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, k.Public(), k)
		if err != nil {
			t.Fatal(err)
		}
		c, _ := x509.ParseCertificate(der)
		l := issued{c, k}
		if err := Provided(l.certPEM(), l.keyPEM(), l.certPEM(), []string{"f5-tmm"}, now); err != nil {
			t.Errorf("%s: a self-signed certificate as its own ca.crt: %v", name, err)
		}
	}
}

// An anchor names itself as its issuer: a certificate signed by its own key
// under another issuer's name is none ("unable to get local issuer
// certificate" in OpenSSL 3.5.5), nor is a CA issued with its issuer's reused
// key, alone in ca.crt.
func TestProvidedAnchorNamesItself(t *testing.T) {
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(120), Subject: pkix.Name{CommonName: "f5net"},
		NotBefore: now.Add(-day), NotAfter: now.Add(year), DNSNames: []string{"f5-tmm"}}
	parent := &x509.Certificate{Subject: pkix.Name{CommonName: "other-name"}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, parent, k.Public(), k)
	if err != nil {
		t.Fatal(err)
	}
	c, _ := x509.ParseCertificate(der)
	l := issued{c, k}
	if err := Provided(l.certPEM(), l.keyPEM(), l.certPEM(), []string{"f5-tmm"}, now); err == nil {
		t.Error("a certificate signed by its own key under another issuer's name was taken for an anchor")
	}
}

// MD5, which Go will not check at all, is left to the names: OpenSSL does not
// check a trusted root's own signature, and accepts the root. Go cannot make
// an MD5 certificate, so a SHA-256 root's algorithm identifiers are rewritten
// to md5WithRSAEncryption (same length); its own signature no longer matters.
func TestProvidedMD5Root(t *testing.T) {
	rk, _ := rsa.GenerateKey(rand.Reader, 2048)
	rt := &x509.Certificate{SerialNumber: big.NewInt(130), Subject: pkix.Name{CommonName: "md5-root"},
		NotBefore: now.Add(-day), NotAfter: now.Add(5 * year), IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign, SignatureAlgorithm: x509.SHA256WithRSA}
	der, err := x509.CreateCertificate(rand.Reader, rt, rt, rk.Public(), rk)
	if err != nil {
		t.Fatal(err)
	}
	sha256WithRSA := []byte{0x06, 0x09, 0x2a, 0x86, 0x48, 0x86, 0xf7, 0x0d, 0x01, 0x01, 0x0b}
	md5WithRSA := []byte{0x06, 0x09, 0x2a, 0x86, 0x48, 0x86, 0xf7, 0x0d, 0x01, 0x01, 0x04}
	md5DER := bytes.ReplaceAll(der, sha256WithRSA, md5WithRSA)
	root, err := x509.ParseCertificate(md5DER)
	if err != nil {
		t.Fatal(err)
	}
	var insecure x509.InsecureAlgorithmError
	if err := root.CheckSignature(root.SignatureAlgorithm, root.RawTBSCertificate, root.Signature); !errors.As(err, &insecure) {
		t.Fatalf("the rewritten root is not refused as an insecure algorithm (%v): the test no longer shows the case", err)
	}
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	lt := &x509.Certificate{SerialNumber: big.NewInt(131), Subject: pkix.Name{CommonName: "f5net"},
		NotBefore: now.Add(-day), NotAfter: now.Add(year), DNSNames: []string{"f5-tmm"}}
	ld, err := x509.CreateCertificate(rand.Reader, lt, root, k.Public(), rk)
	if err != nil {
		t.Fatal(err)
	}
	leaf, _ := x509.ParseCertificate(ld)
	l := issued{leaf, k}
	if err := Provided(l.certPEM(), l.keyPEM(), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: md5DER}), []string{"f5-tmm"}, now); err != nil {
		t.Errorf("an MD5 root: %v", err)
	}
}

// Client auth signs the handshake: a leaf whose key usage lacks
// digitalSignature is refused, as OpenSSL refuses it for sslclient.
func TestProvidedLeafNeedsDigitalSignature(t *testing.T) {
	root := issue(t, nil, "root", true, now.Add(-day), now.Add(5*year))
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	lt := &x509.Certificate{SerialNumber: big.NewInt(140), Subject: pkix.Name{CommonName: "f5net"},
		NotBefore: now.Add(-day), NotAfter: now.Add(year), DNSNames: []string{"f5-tmm"}, KeyUsage: x509.KeyUsageKeyEncipherment}
	der, _ := x509.CreateCertificate(rand.Reader, lt, root.cert, k.Public(), root.key)
	c, _ := x509.ParseCertificate(der)
	l := issued{c, k}
	if err := Provided(l.certPEM(), l.keyPEM(), root.certPEM(), []string{"f5-tmm"}, now); err == nil || !strings.Contains(err.Error(), "digitalSignature") {
		t.Errorf("keyEncipherment only: %v", err)
	}
}

// Names compared as OpenSSL 3.5.5 compares them (each case's verdict was
// taken from `openssl verify` on the same certificates): a root whose issuer
// name differs from its subject only as below.
func TestProvidedRootNamesAsOpenSSLComparesThem(t *testing.T) {
	ps := func(s string) asn1.RawValue { // PrintableString/UTF8String as Go marshals them
		b, _ := asn1.Marshal(s)
		var v asn1.RawValue
		_, _ = asn1.Unmarshal(b, &v)
		return v
	}
	ia5 := func(s string) asn1.RawValue {
		return asn1.RawValue{Class: asn1.ClassUniversal, Tag: asn1.TagIA5String, Bytes: []byte(s)}
	}
	tagged := func(tag int, b []byte) asn1.RawValue {
		return asn1.RawValue{Class: asn1.ClassUniversal, Tag: tag, Bytes: b}
	}
	bmp := func(s string) asn1.RawValue {
		var b []byte
		for _, u := range utf16.Encode([]rune(s)) {
			b = append(b, byte(u>>8), byte(u))
		}
		return tagged(asn1.TagBMPString, b)
	}
	var (
		oidCN = asn1.ObjectIdentifier{2, 5, 4, 3}
		oidO  = asn1.ObjectIdentifier{2, 5, 4, 10}
		oidDC = asn1.ObjectIdentifier{0, 9, 2342, 19200300, 100, 1, 25}
		oidE  = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 9, 1}
	)
	atv := func(oid asn1.ObjectIdentifier, v asn1.RawValue) pkix.AttributeTypeAndValue {
		return pkix.AttributeTypeAndValue{Type: oid, Value: v}
	}
	name := func(rdns ...[]pkix.AttributeTypeAndValue) []byte {
		seq := pkix.RDNSequence{}
		for _, r := range rdns {
			seq = append(seq, pkix.RelativeDistinguishedNameSET(r))
		}
		b, err := asn1.Marshal(seq)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	one := func(a ...pkix.AttributeTypeAndValue) []pkix.AttributeTypeAndValue { return a }
	for label, tc := range map[string]struct {
		subject, issuer []byte
		ok              bool
	}{
		"identical":         {name(one(atv(oidCN, ps("Root")))), name(one(atv(oidCN, ps("Root")))), true},
		"case and spaces":   {name(one(atv(oidCN, ps("Corp  Root")))), name(one(atv(oidCN, ps("corp root")))), true},
		"leading space":     {name(one(atv(oidCN, ps(" Root")))), name(one(atv(oidCN, ps("Root")))), true},
		"DC case":           {name(one(atv(oidDC, ia5("Corp"))), one(atv(oidCN, ps("Root")))), name(one(atv(oidDC, ia5("corp"))), one(atv(oidCN, ps("Root")))), true},
		"emailAddress case": {name(one(atv(oidE, ia5("PKI@corp.example")))), name(one(atv(oidE, ia5("pki@corp.example")))), true},
		// Directory strings of other types, canonicalised the same way.
		"T61 Latin-1 vs UTF8":  {name(one(atv(oidCN, tagged(asn1.TagT61String, []byte("\xc4rzte"))))), name(one(atv(oidCN, ps("Ärzte")))), true},
		"BMP vs UTF8 case":     {name(one(atv(oidCN, bmp("Root")))), name(one(atv(oidCN, ps("root")))), true},
		"leading vertical tab": {name(one(atv(oidCN, ps("\vRoot")))), name(one(atv(oidCN, ps("Root")))), true},
		// A multi-valued RDN is a set: valid DER order differs once canonical.
		"RDN as a set": {name(one(atv(oidCN, ps("a")), atv(oidCN, tagged(asn1.TagPrintableString, []byte("B"))))),
			name(one(atv(oidCN, ps("b")), atv(oidCN, tagged(asn1.TagPrintableString, []byte("A"))))), true},
		"empty RDN dropped": {name(one(atv(oidCN, ps("Root"))), one()), name(one(atv(oidCN, ps("Root")))), true},
		// NumericString is outside OpenSSL's canonical types: byte for byte, with its type.
		"NumericString vs UTF8": {name(one(atv(oidCN, tagged(asn1.TagNumericString, []byte("123"))))), name(one(atv(oidCN, ps("123")))), false},
		"NumericString spaces":  {name(one(atv(oidCN, tagged(asn1.TagNumericString, []byte("1  2"))))), name(one(atv(oidCN, tagged(asn1.TagNumericString, []byte("1 2"))))), false},
		"NumericString same":    {name(one(atv(oidCN, tagged(asn1.TagNumericString, []byte("12"))))), name(one(atv(oidCN, tagged(asn1.TagNumericString, []byte("12"))))), true},
		// A string never reads as another type's encoding.
		"string reads as hex": {name(one(atv(oidCN, ps("120131")))), name(one(atv(oidCN, tagged(asn1.TagNumericString, []byte("1"))))), false},
		"order":               {name(one(atv(oidCN, ps("Root"))), one(atv(oidO, ps("Acme")))), name(one(atv(oidO, ps("Acme"))), one(atv(oidCN, ps("Root")))), false},
		// Only the grouping differs (DER sorts a set: CN before O either way).
		"multi-valued RDN": {name(one(atv(oidCN, ps("Root")), atv(oidO, ps("Acme")))), name(one(atv(oidCN, ps("Root"))), one(atv(oidO, ps("Acme")))), false},
		"non-ASCII case":   {name(one(atv(oidCN, ps("Ärzte Root")))), name(one(atv(oidCN, ps("ärzte Root")))), false},
		"no-break space":   {name(one(atv(oidCN, ps("Corp Root")))), name(one(atv(oidCN, ps("Corp Root")))), false},
	} {
		k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		rt := &x509.Certificate{SerialNumber: big.NewInt(150), RawSubject: tc.subject,
			NotBefore: now.Add(-day), NotAfter: now.Add(5 * year), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
		parent := &x509.Certificate{RawSubject: tc.issuer}
		der, err := x509.CreateCertificate(rand.Reader, rt, parent, k.Public(), k)
		if err != nil {
			t.Fatal(label, err)
		}
		root, _ := x509.ParseCertificate(der)
		lk, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		lt := &x509.Certificate{SerialNumber: big.NewInt(151), Subject: pkix.Name{CommonName: "f5net"},
			NotBefore: now.Add(-day), NotAfter: now.Add(year), DNSNames: []string{"f5-tmm"}}
		ld, _ := x509.CreateCertificate(rand.Reader, lt, root, lk.Public(), k)
		leaf, _ := x509.ParseCertificate(ld)
		l := issued{leaf, lk}
		err = Provided(l.certPEM(), l.keyPEM(), issued{root, k}.certPEM(), []string{"f5-tmm"}, now)
		if (err == nil) != tc.ok {
			t.Errorf("%s: accepted=%v, OpenSSL: %v (%v)", label, err == nil, tc.ok, err)
		}
	}
}

// issuer ca: ca.crt is the CA certificate alone, and OpenSSL takes only a
// self-signed root for an anchor, so an issuing (intermediate) CA is refused.
func TestCARefusesAnIntermediate(t *testing.T) {
	root := issue(t, nil, "root", true, now.Add(-day), now.Add(5*year))
	inter := issue(t, &root, "issuing", true, now.Add(-day), now.Add(4*year))
	if _, _, err := CA(append(inter.certPEM(), root.certPEM()...), inter.keyPEM(), now, 30*day); err == nil || !strings.Contains(err.Error(), "not a self-signed root") {
		t.Errorf("an intermediate CA: %v", err)
	}
}

// Key identifiers link a chain for OpenSSL (X509_check_akid); Go ignores
// them. Each case's verdict was taken from openssl verify.
func TestProvidedKeyIdentifiersLink(t *testing.T) {
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	rootTmpl := func(skid, akid []byte) *x509.Certificate {
		return &x509.Certificate{SerialNumber: big.NewInt(160), Subject: pkix.Name{CommonName: "kid-root"},
			NotBefore: now.Add(-day), NotAfter: now.Add(5 * year), IsCA: true, BasicConstraintsValid: true,
			KeyUsage: x509.KeyUsageCertSign, SubjectKeyId: skid, AuthorityKeyId: akid}
	}
	leafFrom := func(parent *x509.Certificate) issued {
		lk, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		lt := &x509.Certificate{SerialNumber: big.NewInt(161), Subject: pkix.Name{CommonName: "f5net"},
			NotBefore: now.Add(-day), NotAfter: now.Add(year), DNSNames: []string{"f5-tmm"}}
		der, err := x509.CreateCertificate(rand.Reader, lt, parent, lk.Public(), k)
		if err != nil {
			t.Fatal(err)
		}
		c, _ := x509.ParseCertificate(der)
		return issued{c, lk}
	}
	mk := func(tmpl *x509.Certificate) *x509.Certificate {
		der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, k.Public(), k)
		if err != nil {
			t.Fatal(err)
		}
		c, _ := x509.ParseCertificate(der)
		return c
	}
	good := mk(rootTmpl([]byte{1, 2, 3, 4}, nil))
	l := leafFrom(good)
	if err := Provided(l.certPEM(), l.keyPEM(), issued{good, k}.certPEM(), []string{"f5-tmm"}, now); err != nil {
		t.Errorf("control: %v", err)
	}
	// The root's own authority key identifier is not its subject key identifier.
	odd := mk(rootTmpl([]byte{1, 2, 3, 4}, []byte{9, 9, 9, 9}))
	l = leafFrom(odd)
	if err := Provided(l.certPEM(), l.keyPEM(), issued{odd, k}.certPEM(), []string{"f5-tmm"}, now); err == nil {
		t.Error("a root whose AKID is not its SKID was taken for an anchor (OpenSSL: unable to get issuer certificate)")
	}
	// The leaf names another key identifier than the root's (same name, same key).
	other := *good
	other.SubjectKeyId = []byte{5, 6, 7, 8}
	l = leafFrom(&other)
	if err := Provided(l.certPEM(), l.keyPEM(), issued{good, k}.certPEM(), []string{"f5-tmm"}, now); err == nil {
		t.Error("a leaf whose AKID is not its issuer's SKID verified (OpenSSL: unable to get local issuer certificate)")
	}
}

// A name attribute carrying more than a type and a value is malformed:
// OpenSSL will not load the certificate, so it names nothing, not even itself.
func TestSameNameRefusesAMalformedAttribute(t *testing.T) {
	oid, _ := asn1.Marshal(asn1.ObjectIdentifier{2, 5, 4, 3})
	val, _ := asn1.Marshal("Root")
	extra, _ := asn1.Marshal("extra")
	wrap := func(tag int, parts ...[]byte) []byte {
		b, _ := asn1.Marshal(asn1.RawValue{Class: asn1.ClassUniversal, Tag: tag, IsCompound: true, Bytes: bytes.Join(parts, nil)})
		return b
	}
	good := wrap(asn1.TagSequence, wrap(asn1.TagSet, wrap(asn1.TagSequence, oid, val)))
	bad := wrap(asn1.TagSequence, wrap(asn1.TagSet, wrap(asn1.TagSequence, oid, val, extra)))
	if !sameName(good, good) {
		t.Fatal("a well-formed name is not the same as itself")
	}
	if sameName(bad, bad) {
		t.Error("a name attribute with a third element compared equal")
	}
}

// issuer ca: a root whose own AKID is not its SKID is no anchor for OpenSSL,
// so the certificates it would issue (ca.crt = this root) would not verify.
func TestCARefusesARootWhoseKeyIDsDisagree(t *testing.T) {
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(170), Subject: pkix.Name{CommonName: "kid-root"},
		NotBefore: now.Add(-day), NotAfter: now.Add(5 * year), IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign, SubjectKeyId: []byte{1, 2, 3, 4}, AuthorityKeyId: []byte{9, 9, 9, 9}}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, k.Public(), k)
	c, _ := x509.ParseCertificate(der)
	if _, _, err := CA(issued{c, k}.certPEM(), issued{c, k}.keyPEM(), now, 30*day); err == nil {
		t.Error("a root whose AKID is not its SKID was accepted as the CA")
	}
}
