// Package singlecert holds what both the check binary and the CLI need to
// know about F5's single certificate: the names it must carry, and how an
// operator's CA or certificate is parsed and checked. Standard library only
// (the check binary must stay that way).
package singlecert

import (
	"crypto"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"
)

// names are the DNS names F5's procedure puts in the certificate, with <ns>
// for each BNK namespace (F5's list, its duplicates removed).
var names = []string{
	"*.f5-observer.<ns>.svc.cluster.local", "dssm-f5-dssm.<ns>", "dssm-svc", "f5-access-renderer",
	"f5-afm.<ns>", "f5-analyzer-grpc-svc", "f5-analyzer.<ns>", "f5-bdosd", "f5-bdosd.<ns>",
	"f5-csrc-grpc-svc", "f5-downloader.<ns>", "f5-dssm-db.<ns>", "f5-dssm-sentinel.<ns>", "f5-dssm.<ns>",
	"f5-dwbld.<ns>", "f5-ipam-ctlr.<ns>", "f5-ipsd.<ns>", "f5-observer", "f5-observer.<ns>",
	"f5-rabbit.<ns>", "f5-spk-csrc.<ns>", "f5-spk-cwc", "f5-spk-cwc.<ns>", "f5-spk-cwc.<ns>.svc",
	"f5-tmm", "f5-tmm.<ns>", "f5-toda-fluentd-external.<ns>", "f5-toda-fluentd.<ns>",
	"f5-validation-svc.<ns>.svc", "f5dr-svc-f5dr", "f5net", "grpc-apmd-svc", "grpc-bdosd-svc",
	"grpc-bdosd-svc.<ns>", "grpc-downloader-svc", "grpc-dwbld-svc", "grpc-ipsd-svc", "grpc-pccd-svc",
	"grpc-stream-ipsd-svc", "grpc-svc", "grpc-svc-f5dr", "grpc-svc-f5dr.<ns>",
	"grpc-svc-f5dr.<ns>.svc.cluster.local", "grpc-svc.<ns>", "grpc-urlcat-svc", "observer",
	"otel-collector", "otel-collector-svc", "otel-collector-svc.<ns>", "otel-collector.<ns>",
	"rabbitmq-server.<ns>", "f5-cne-controller", "f5-cne-controller.<ns>", "f5-cne-controller.<ns>.svc",
	"f5-coremond.<ns>", "f5-coremond.<ns>.svc", "f5-coremond.<ns>.svc.cluster.local",
	"f5-observer-operator", "f5-observer-operator.<ns>", "f5-observer-receiver", "f5-observer-receiver.<ns>",
	"*.f5-observer-operator.<ns>.svc.cluster.local", "*.f5-observer-receiver.<ns>.svc.cluster.local",
	"f5-ebc-grpc-svc", "f5-ebc-grpc-svc.<ns>", "f5-ebc-grpc-svc.<ns>.svc", "f5-ext-bigip-controller.<ns>",
	"f5-ipsec-qkview", "f5-ipsec-qkview.<ns>", "f5-ipsec-qkview.<ns>.svc.cluster.local", "f5-ipsec", "f5-ipsec.<ns>",
}

// SANs is F5's name list for every namespace, plus extra names, deduplicated
// in order.
func SANs(namespaces, extra []string) []string {
	var out []string
	seen := map[string]bool{}
	add := func(s string) {
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	for _, n := range names {
		if !strings.Contains(n, "<ns>") {
			add(n)
			continue
		}
		for _, ns := range namespaces {
			add(strings.ReplaceAll(n, "<ns>", ns))
		}
	}
	for _, e := range extra {
		add(e)
	}
	return out
}

// Certs parses every CERTIFICATE block of a PEM bundle, in order.
func Certs(bundle []byte) ([]*x509.Certificate, error) {
	var out []*x509.Certificate
	for rest := bundle; ; {
		var b *pem.Block
		b, rest = pem.Decode(rest)
		if b == nil {
			break
		}
		if b.Type != "CERTIFICATE" {
			continue
		}
		c, err := x509.ParseCertificate(b.Bytes)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	if len(out) == 0 {
		return nil, errors.New("no PEM certificate")
	}
	return out, nil
}

// Key parses a PEM private key: PKCS#1, SEC 1 or PKCS#8, skipping the
// "EC PARAMETERS" block `openssl ecparam -genkey` writes first.
func Key(keyPEM []byte) (crypto.Signer, error) {
	for rest := keyPEM; ; {
		var b *pem.Block
		b, rest = pem.Decode(rest)
		if b == nil {
			return nil, errors.New("no PEM private key")
		}
		var key any
		var err error
		switch b.Type {
		case "EC PARAMETERS":
			continue
		case "RSA PRIVATE KEY":
			key, err = x509.ParsePKCS1PrivateKey(b.Bytes)
		case "EC PRIVATE KEY":
			key, err = x509.ParseECPrivateKey(b.Bytes)
		case "PRIVATE KEY":
			key, err = x509.ParsePKCS8PrivateKey(b.Bytes)
		case "ENCRYPTED PRIVATE KEY":
			return nil, errors.New("the private key is encrypted; give an unencrypted one")
		default:
			return nil, fmt.Errorf("unsupported PEM block %q", b.Type)
		}
		if err != nil {
			return nil, err
		}
		s, ok := key.(crypto.Signer)
		if !ok {
			return nil, errors.New("unsupported key type")
		}
		return s, nil
	}
}

// Pair parses a certificate (its first block; any further blocks are its
// chain) and its private key, and checks that they belong together.
func Pair(certPEM, keyPEM []byte) (leaf *x509.Certificate, chain []*x509.Certificate, key crypto.Signer, err error) {
	certs, err := Certs(certPEM)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("tls.crt: %w", err)
	}
	if key, err = Key(keyPEM); err != nil {
		return nil, nil, nil, fmt.Errorf("tls.key: %w", err)
	}
	pub, _ := x509.MarshalPKIXPublicKey(key.Public())
	certPub, _ := x509.MarshalPKIXPublicKey(certs[0].PublicKey)
	if string(pub) != string(certPub) {
		return nil, nil, nil, errors.New("tls.key does not match tls.crt")
	}
	return certs[0], certs[1:], key, nil
}

// Skew is how far a CA's notBefore may be ahead of this clock (a CA minted a
// moment ago on a host whose clock runs ahead).
const Skew = 5 * time.Minute

// CA parses an operator's CA and checks it can sign now and for at least
// renewBefore: an expired (or about to expire) CA would issue certificates
// that are invalid at once and be reissued on every sync. A CA whose key usage
// lacks certSign is refused too: certificates it signs do not verify.
func CA(certPEM, keyPEM []byte, now time.Time, renewBefore time.Duration) (*x509.Certificate, crypto.Signer, error) {
	ca, _, key, err := Pair(certPEM, keyPEM)
	if err != nil {
		return nil, nil, err
	}
	if !ca.IsCA {
		return nil, nil, errors.New("the certificate is not a CA (basicConstraints CA:false)")
	}
	if ca.KeyUsage != 0 && ca.KeyUsage&x509.KeyUsageCertSign == 0 {
		return nil, nil, errors.New("the CA's key usage lacks keyCertSign, so certificates it signs do not verify")
	}
	if now.Add(Skew).Before(ca.NotBefore) {
		return nil, nil, fmt.Errorf("the CA is not valid until %s", ca.NotBefore.Format(time.DateOnly))
	}
	if !now.Before(ca.NotAfter) {
		return nil, nil, fmt.Errorf("the CA expired %s: renew it", ca.NotAfter.Format(time.DateOnly))
	}
	if !now.Add(renewBefore).Before(ca.NotAfter) {
		return nil, nil, fmt.Errorf("the CA expires %s, within the renewal window (%s): renew it first",
			ca.NotAfter.Format(time.DateOnly), renewBefore)
	}
	return ca, key, nil
}

// Provided checks an operator's own certificate before it is used: key
// matches, valid now for both server and client auth (one certificate serves
// both ends of every mTLS connection), carries every name BNK uses, and
// chains to a self-signed root in ca.crt. The chain is judged as OpenSSL
// (without -partial_chain) judges it, the strictest of the TLS stacks that may
// read ca.crt: it must end at a self-signed root there; intermediates come
// from ca.crt or after the leaf in tls.crt; and anyExtendedKeyUsage is not
// server or client auth (Go accepts all three of these where OpenSSL does not).
func Provided(certPEM, keyPEM, caPEM []byte, sans []string, now time.Time) error {
	leaf, chain, _, err := Pair(certPEM, keyPEM)
	if err != nil {
		return err
	}
	cas, err := Certs(caPEM)
	if err != nil {
		return fmt.Errorf("ca.crt: %w", err)
	}
	roots, inter := x509.NewCertPool(), x509.NewCertPool()
	hasRoot := false
	for _, c := range cas {
		if c.CheckSignatureFrom(c) == nil {
			roots.AddCert(c)
			hasRoot = true
		} else {
			inter.AddCert(c)
		}
	}
	if !hasRoot {
		return errors.New("ca.crt holds no self-signed root certificate: give the root of the chain (OpenSSL accepts nothing else as an anchor)")
	}
	for _, c := range chain {
		inter.AddCert(c)
	}
	if len(leaf.ExtKeyUsage) > 0 || len(leaf.UnknownExtKeyUsage) > 0 {
		if !slices.Contains(leaf.ExtKeyUsage, x509.ExtKeyUsageServerAuth) || !slices.Contains(leaf.ExtKeyUsage, x509.ExtKeyUsageClientAuth) {
			return errors.New("tls.crt's extended key usage must name both serverAuth and clientAuth (one certificate serves both ends of every mTLS connection)")
		}
	}
	for _, u := range []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth} {
		if _, err := leaf.Verify(x509.VerifyOptions{Roots: roots, Intermediates: inter, CurrentTime: now,
			KeyUsages: []x509.ExtKeyUsage{u}}); err != nil {
			return fmt.Errorf("tls.crt does not verify against ca.crt for %s: %w", map[x509.ExtKeyUsage]string{
				x509.ExtKeyUsageServerAuth: "server auth", x509.ExtKeyUsageClientAuth: "client auth"}[u], err)
		}
	}
	var missing []string
	for _, n := range sans {
		if leaf.VerifyHostname(n) != nil {
			missing = append(missing, n)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		if len(missing) > 8 {
			missing = append(missing[:8], fmt.Sprintf("… %d more", len(missing)-8))
		}
		return fmt.Errorf("tls.crt does not cover names BNK uses: %s", strings.Join(missing, ", "))
	}
	return nil
}
