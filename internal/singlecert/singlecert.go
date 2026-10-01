// Package singlecert holds what both the check binary and the CLI need to
// know about F5's single certificate: the names it must carry, and how an
// operator's CA or certificate is parsed and checked. Standard library only
// (the check binary must stay that way).
package singlecert

import (
	"bytes"
	"crypto"
	"crypto/x509"
	"encoding/asn1"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode/utf16"
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
	// ca.crt is this certificate alone, and OpenSSL takes nothing but a
	// self-signed root for an anchor: an issuing (intermediate) CA's
	// certificates would fail at every peer.
	if !selfSigned(ca) {
		return nil, nil, errors.New("the CA is not a self-signed root (an issuing CA's certificates would not verify against it alone): " +
			"give the root, or issue the certificate from your intermediate yourself and use issuer provided, with the intermediate after it in tls.crt and the root in ca.crt")
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
		// An anchor is self-signed: a root, or a certificate given as its own
		// ca.crt. See selfSigned. Whether it may sign is judged by Verify
		// below, not here.
		if selfSigned(c) {
			roots.AddCert(c)
			hasRoot = true
		} else {
			inter.AddCert(c)
		}
	}
	// Every name, not only the anchor's: OpenSSL will not load a certificate
	// with a malformed one, though Go's parser does.
	for _, c := range append(append([]*x509.Certificate{leaf}, chain...), cas...) {
		if _, err := canonName(c.RawSubject); err != nil {
			return fmt.Errorf("a certificate's subject (%s) is malformed", c.Subject)
		}
		if _, err := canonName(c.RawIssuer); err != nil {
			return fmt.Errorf("a certificate's issuer (%s) is malformed", c.Issuer)
		}
	}
	if !hasRoot {
		return errors.New("ca.crt holds no self-signed root certificate: give the root of the chain (OpenSSL accepts nothing else as an anchor)")
	}
	for _, c := range chain {
		inter.AddCert(c)
	}
	// Both ends sign with the one certificate: as a TLS 1.2 ECDHE server it
	// signs the key exchange, and as a client it signs CertificateVerify.
	// Measured with OpenSSL 3.5.5: an RSA keyEncipherment-only certificate is
	// refused for client auth ("unsuitable certificate purpose"); an EC
	// keyAgreement-only one passes `verify` but a TLS 1.2 server holding it
	// has "no shared cipher".
	if leaf.KeyUsage != 0 && leaf.KeyUsage&x509.KeyUsageDigitalSignature == 0 {
		return errors.New("tls.crt's key usage lacks digitalSignature: the certificate signs the handshake at both ends of every mTLS connection")
	}
	if len(leaf.ExtKeyUsage) > 0 || len(leaf.UnknownExtKeyUsage) > 0 {
		if !slices.Contains(leaf.ExtKeyUsage, x509.ExtKeyUsageServerAuth) || !slices.Contains(leaf.ExtKeyUsage, x509.ExtKeyUsageClientAuth) {
			return errors.New("tls.crt's extended key usage must name both serverAuth and clientAuth (one certificate serves both ends of every mTLS connection)")
		}
	}
	for _, u := range []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth} {
		chains, err := leaf.Verify(x509.VerifyOptions{Roots: roots, Intermediates: inter, CurrentTime: now,
			KeyUsages: []x509.ExtKeyUsage{u}})
		if err == nil && !slices.ContainsFunc(chains, keyIDsLink) {
			err = errors.New("no chain whose authority key identifiers match their issuers' subject key identifiers (OpenSSL requires that)")
		}
		if err != nil {
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

// selfSigned: it names itself as its issuer (compared as OpenSSL compares
// names, see sameName) and is signed by its own key. The
// names: OpenSSL takes nothing else for an anchor ("unable to get local issuer
// certificate" for a certificate signed by its own key under another issuer's
// name). The signature: stricter than OpenSSL, which takes a certificate
// naming itself but signed by another key unless -check_ss_sig. It is checked
// with CheckSignature, not CheckSignatureFrom, which also demands a CA and so
// refused a self-signed leaf given as its own ca.crt (OpenSSL and Go's Verify
// accept it). An algorithm Go will not check at all (MD5; SHA-1 it does) is
// left to the names: OpenSSL does not check a trusted root's own signature.
func selfSigned(c *x509.Certificate) bool {
	if !sameName(c.RawSubject, c.RawIssuer) || !keyIDLinks(c, c) {
		return false
	}
	err := c.CheckSignature(c.SignatureAlgorithm, c.RawTBSCertificate, c.Signature)
	var insecure x509.InsecureAlgorithmError
	return err == nil || errors.As(err, &insecure)
}

// sameName compares two DER names as OpenSSL does (x509_name_canon): RDN by
// RDN in order (empty RDNs dropped), the attributes of each RDN as a set
// (OpenSSL re-encodes them as a DER SET OF, which sorts them), the same type,
// and directory string values (UTF8, BMP, Universal, Printable, T61, IA5,
// Visible: ASN1_MASK_CANON, which excludes NumericString) equal once converted
// to UTF-8, trimmed of ASCII whitespace, internal runs of it collapsed to one
// space and ASCII lowercased; other values byte for byte, with their type.
// pkix.Name.String() is not that form: it re-orders attributes, flattens
// multi-valued RDNs, folds non-ASCII case, and hex-encodes DC or emailAddress.
func sameName(a, b []byte) bool {
	ca, errA := canonName(a)
	cb, errB := canonName(b)
	return errA == nil && errB == nil && slices.EqualFunc(ca, cb, func(x, y []string) bool { return slices.Equal(x, y) })
}

// canonName is a DER name as RDNs, each a list of "OID=canonical value".
func canonName(der []byte) ([][]string, error) {
	var seq asn1.RawValue
	if rest, err := asn1.Unmarshal(der, &seq); err != nil || len(rest) > 0 {
		return nil, errors.New("malformed name")
	}
	var out [][]string
	for rest := seq.Bytes; len(rest) > 0; {
		var set asn1.RawValue
		var err error
		if rest, err = asn1.Unmarshal(rest, &set); err != nil {
			return nil, err
		}
		var rdn []string
		for in := set.Bytes; len(in) > 0; {
			var seq asn1.RawValue
			if in, err = asn1.Unmarshal(in, &seq); err != nil {
				return nil, err
			}
			var atv struct {
				Type  asn1.ObjectIdentifier
				Value asn1.RawValue
			}
			// Exactly a type and a value: OpenSSL will not load a name whose
			// attribute carries more.
			// (Re-encoding is what catches a third element: decoding into the
			// struct ignores trailing fields.)
			if _, err := asn1.Unmarshal(seq.FullBytes, &atv); err != nil {
				return nil, errors.New("malformed name attribute")
			}
			if b, _ := asn1.Marshal(atv); !bytes.Equal(b, seq.FullBytes) {
				return nil, errors.New("malformed name attribute")
			}
			rdn = append(rdn, atv.Type.String()+"="+canonValue(atv.Value))
		}
		if len(rdn) > 0 {
			slices.Sort(rdn)
			out = append(out, rdn)
		}
	}
	return out, nil
}

// canonValue marks its two forms apart: "s:" a canonical directory string,
// "x:" the hex of anything else with its tag, so that no string can read as
// another type's encoding.
func canonValue(v asn1.RawValue) string {
	raw := "x:" + hex.EncodeToString(v.FullBytes)
	var s string
	switch {
	case v.Class != asn1.ClassUniversal:
		return raw
	case v.Tag == asn1.TagUTF8String || v.Tag == asn1.TagPrintableString || v.Tag == asn1.TagIA5String ||
		v.Tag == 26 /* VisibleString */ :
		s = string(v.Bytes)
	case v.Tag == asn1.TagT61String: // read as Latin-1, as OpenSSL does
		r := make([]rune, len(v.Bytes))
		for i, c := range v.Bytes {
			r[i] = rune(c)
		}
		s = string(r)
	case v.Tag == asn1.TagBMPString && len(v.Bytes)%2 == 0:
		u := make([]uint16, len(v.Bytes)/2)
		for i := range u {
			u[i] = uint16(v.Bytes[2*i])<<8 | uint16(v.Bytes[2*i+1])
		}
		s = string(utf16.Decode(u))
	case v.Tag == 28 /* UniversalString */ && len(v.Bytes)%4 == 0:
		r := make([]rune, len(v.Bytes)/4)
		for i := range r {
			b := v.Bytes[4*i:]
			r[i] = rune(b[0])<<24 | rune(b[1])<<16 | rune(b[2])<<8 | rune(b[3])
		}
		s = string(r)
	default:
		return raw
	}
	var b strings.Builder
	space := false
	for _, r := range strings.Trim(s, " \t\n\v\f\r") {
		if r == ' ' || r == '\t' || r == '\n' || r == '\v' || r == '\f' || r == '\r' {
			space = true
			continue
		}
		if space {
			b.WriteByte(' ')
			space = false
		}
		if r >= 'A' && r <= 'Z' {
			r += 'a' - 'A'
		}
		b.WriteRune(r)
	}
	return "s:" + b.String()
}

// keyIDLinks is OpenSSL's X509_check_akid, which Go's Verify ignores:
// parent is child's issuer only when child's authority key identifier
// agrees with it — its keyIdentifier with parent's subject key identifier
// (when both are present), its authorityCertSerialNumber with parent's
// serial, and a directoryName in its authorityCertIssuer with parent's
// issuer.
func keyIDLinks(child, parent *x509.Certificate) bool {
	if len(child.AuthorityKeyId) > 0 && len(parent.SubjectKeyId) > 0 && !bytes.Equal(child.AuthorityKeyId, parent.SubjectKeyId) {
		return false
	}
	for _, e := range child.Extensions {
		if !e.Id.Equal(oidAuthorityKeyID) {
			continue
		}
		var akid asn1.RawValue
		if _, err := asn1.Unmarshal(e.Value, &akid); err != nil {
			return false
		}
		for in := akid.Bytes; len(in) > 0; {
			var f asn1.RawValue
			var err error
			if in, err = asn1.Unmarshal(in, &f); err != nil || f.Class != asn1.ClassContextSpecific {
				return false
			}
			switch f.Tag {
			case 1: // authorityCertIssuer: GeneralNames
				for gn := f.Bytes; len(gn) > 0; {
					var n asn1.RawValue
					if gn, err = asn1.Unmarshal(gn, &n); err != nil {
						return false
					}
					if n.Class == asn1.ClassContextSpecific && n.Tag == 4 && !sameName(n.Bytes, parent.RawIssuer) {
						return false
					}
				}
			case 2: // authorityCertSerialNumber
				if new(big.Int).SetBytes(f.Bytes).Cmp(parent.SerialNumber) != 0 {
					return false
				}
			}
		}
	}
	return true
}

var oidAuthorityKeyID = asn1.ObjectIdentifier{2, 5, 29, 35}

// keyIDsLink: every certificate of the chain links to the next. The anchor
// itself is in the pool only if it links to itself (selfSigned).
func keyIDsLink(chain []*x509.Certificate) bool {
	for i := 0; i+1 < len(chain); i++ {
		if !keyIDLinks(chain[i], chain[i+1]) {
			return false
		}
	}
	return true
}
