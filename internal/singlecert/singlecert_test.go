package singlecert

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"strings"
	"testing"
	"time"
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

// provided: one certificate serves both ends of mTLS, so server-auth-only is
// refused; ca.crt holding an unrelated root next to the issuing intermediate
// verifies, as it does for the components that read ca.crt.
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
	server := leaf(x509.ExtKeyUsageServerAuth)
	if err := Provided(server.certPEM(), server.keyPEM(), inter.certPEM(), []string{"f5-tmm"}, now); err == nil || !strings.Contains(err.Error(), "client auth") {
		t.Errorf("server auth only: %v", err)
	}
	both := leaf(x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth)
	unrelated := issue(t, nil, "unrelated", true, now.Add(-day), now.Add(5*year))
	if err := Provided(both.certPEM(), both.keyPEM(), append(unrelated.certPEM(), inter.certPEM()...), []string{"f5-tmm"}, now); err != nil {
		t.Errorf("an unrelated root beside the issuing intermediate: %v", err)
	}
}
