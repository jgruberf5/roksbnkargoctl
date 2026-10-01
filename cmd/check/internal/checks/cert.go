package checks

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"slices"
	"strings"
	"time"

	"github.com/jgruberf5/roksbnkargoctl/cmd/check/internal/kube"
	"github.com/jgruberf5/roksbnkargoctl/internal/singlecert"
)

// Single-certificate mode (F5's "Single Certificate for BNK"): FLO runs with
// global.certmgr.enabled=false and mounts one kubernetes.io/tls Secret
// (tls.crt, tls.key, ca.crt) in place of every cert-manager Certificate
// Secret. The Secret must exist in every namespace a BNK component runs in.
const (
	IssuerSelfSigned = "self-signed" // a CA generated here, kept in the check namespace
	IssuerCA         = "ca"          // the operator's CA certificate and key
	IssuerProvided   = "provided"    // the operator's own tls.crt, tls.key and ca.crt

	// annoSingleCertSpec is the digest of the settings the Secret was issued
	// for: a settings change reissues it.
	annoSingleCertSpec = "roksbnkargoctl.io/single-cert-spec"
	// annoSingleCertRestarted names the certificate the namespace's pods were
	// restarted onto (certDigest). Written only once they were, so a restart
	// that failed, or a run that died before it, is finished by the next sync.
	annoSingleCertRestarted = "roksbnkargoctl.io/single-cert-restarted"
)

// SingleCertSANs is F5's name list for every namespace plus extra names.
func SingleCertSANs(namespaces, extra []string) []string { return singlecert.SANs(namespaces, extra) }

// SingleCertConfig configures `check cert`.
type SingleCertConfig struct {
	Issuer       string   // self-signed | ca | provided
	SecretName   string   // the Secret FLO mounts (global.certmgr.secretName)
	Namespaces   []string // every BNK namespace
	SourceSecret string   // namespace/name: the CA (ca) or the certificate (provided)
	StateNS      string   // where a self-signed CA is kept
	Subject      pkix.Name
	CommonName   string // the certificate's CN (F5: f5net)
	CACommonName string // a generated CA's CN (F5: f5net-ca)
	ExtraDNS     []string
	IPs          []string
	KeyType      string // rsa | ecdsa
	KeyBits      int    // RSA key size (F5: 4096)
	Validity     time.Duration
	RenewBefore  time.Duration
	Now          func() time.Time
}

// tlsMaterial is the content of the Secret.
type tlsMaterial struct{ cert, key, ca []byte }

// SingleCert makes sure the single-certificate Secret is current in every BNK
// namespace: kept when it is valid for these settings and not about to expire,
// issued (or copied) otherwise.
func SingleCert(ctx context.Context, env *Env, cfg SingleCertConfig, res *Result) error {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if len(cfg.Namespaces) == 0 || cfg.SecretName == "" {
		return errors.New("cert: --namespace and --secret-name are required")
	}
	sans := SingleCertSANs(cfg.Namespaces, cfg.ExtraDNS)

	var signer *x509.Certificate
	var signerKey crypto.Signer
	var provided *tlsMaterial
	switch cfg.Issuer {
	case IssuerSelfSigned:
		var err error
		if signer, signerKey, err = selfSignedCA(ctx, env, cfg, res); err != nil {
			return err
		}
	case IssuerCA:
		m, err := readTLS(ctx, env, cfg.SourceSecret, false)
		if err != nil {
			return err
		}
		// Refused within the renewal window too, not only once expired: the
		// certificate is capped at the CA's expiry, so it would be inside the
		// window as well and reissued on every sync.
		if signer, signerKey, err = singlecert.CA(m.cert, m.key, cfg.Now(), cfg.RenewBefore); err != nil {
			res.Fail("ca", "%s: %v", cfg.SourceSecret, err)
			return nil
		}
	case IssuerProvided:
		m, err := readTLS(ctx, env, cfg.SourceSecret, true)
		if err != nil {
			return err
		}
		if err := singlecert.Provided(m.cert, m.key, m.ca, append(slices.Clone(sans), cfg.IPs...), cfg.Now()); err != nil {
			res.Fail("provided", "%s: %v", cfg.SourceSecret, err)
			return nil
		}
		provided = m
	default:
		return fmt.Errorf("cert: --issuer %q: must be %s, %s or %s", cfg.Issuer, IssuerSelfSigned, IssuerCA, IssuerProvided)
	}

	spec := certSpec(cfg, sans, provided)
	cur, why := currentSecret(ctx, env, cfg, spec, signer, provided)
	var m *tlsMaterial
	switch {
	case cur != nil:
		m = cur
		res.Pass("certificate", "kept: valid until %s, issued for these settings", leafNotAfter(cur.cert))
	case provided != nil:
		m = provided
		res.Pass("certificate", "copying the provided certificate (%s)", why)
	default:
		var err error
		if m, err = issueLeaf(cfg, sans, signer, signerKey); err != nil {
			return err
		}
		res.Pass("certificate", "issued %s, valid until %s, %d DNS names (%s)", cfg.CommonName, leafNotAfter(m.cert), len(sans), why)
	}
	// A namespace's pods need a restart when its Secret existed and the pods
	// have not been restarted onto this certificate: it was replaced now, or
	// an earlier restart failed or never ran. On the first issue there is
	// nothing to restart (FLO starts the components on it).
	digest := certDigest(m)
	pending, failed := false, false
	for _, ns := range cfg.Namespaces {
		restarted := digest
		if old, err := env.Kube.Get(ctx, GVRSecret.Path(ns, cfg.SecretName)); err == nil && old.Annotations()[annoSingleCertRestarted] != digest {
			restarted = ""
		}
		if err := writeTLS(ctx, env, ns, cfg.SecretName, m, spec, restarted); err != nil {
			res.Fail("secret", "%s/%s: %v", ns, cfg.SecretName, err)
			failed = true
			continue
		}
		pending = pending || restarted == ""
		res.Pass("secret", "%s/%s is current", ns, cfg.SecretName)
	}
	// Only when every namespace holds the new certificate: restarting pods
	// onto a Secret that was not written is an outage for nothing, and with a
	// CA change would leave the namespaces on different CAs. The next sync
	// writes again and restarts then.
	switch {
	case pending && failed:
		res.Fail("restart", "not restarting the pods that mount %s: it could not be written into every namespace", cfg.SecretName)
	case pending:
		if restartMounting(ctx, env, cfg, res) {
			for _, ns := range cfg.Namespaces {
				if err := writeTLS(ctx, env, ns, cfg.SecretName, m, spec, digest); err != nil {
					res.Fail("restart", "%s/%s: recording the restart: %v", ns, cfg.SecretName, err)
				}
			}
		}
	}
	return nil
}

// certDigest identifies the certificate and trust a Secret serves.
func certDigest(m *tlsMaterial) string {
	h := sha256.New()
	h.Write(m.cert)
	h.Write([]byte{0})
	h.Write(m.ca)
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// restartMounting deletes every pod in the BNK namespaces that mounts the
// Secret, once a certificate it already held was replaced. Components load
// the certificate at start and do not all reload it: measured on BNK 2.4.0
// GA, after a CA change the DSSM Redis kept serving the old certificate and
// failed its own readiness probe ("certificate verify failed") against the
// new ca.crt, while the sentinels, restarted, trusted only the new CA. Every
// pod is restarted at once so they all hold the same CA; their controllers
// re-create them. It reports whether every pod was restarted.
func restartMounting(ctx context.Context, env *Env, cfg SingleCertConfig, res *Result) bool {
	ok := true
	for _, ns := range cfg.Namespaces {
		pods, err := env.Kube.List(ctx, GVRPod.Path(ns, ""), kube.ListOptions{})
		if err != nil {
			res.Fail("restart", "%s: listing pods: %v", ns, err)
			ok = false
			continue
		}
		n := 0
		for _, p := range pods {
			if !mountsSecret(p, cfg.SecretName) || p.Deleting() {
				continue
			}
			if _, err := env.Kube.DeleteIfExists(ctx, GVRPod.Path(ns, p.Name()), ""); err != nil {
				res.Fail("restart", "%s/%s: %v", ns, p.Name(), err)
				ok = false
				continue
			}
			n++
		}
		if n > 0 {
			res.Pass("restart", "%s: restarted %d pods that mount the replaced certificate", ns, n)
		}
	}
	return ok
}

// mountsSecret reports whether a pod mounts the Secret as a volume, directly
// or through a projected volume.
func mountsSecret(p kube.Object, name string) bool {
	for _, v := range p.Slice("spec", "volumes") {
		vol := kube.Object(asMap(v))
		if vol.String("secret", "secretName") == name {
			return true
		}
		for _, src := range vol.Slice("projected", "sources") {
			if kube.Object(asMap(src)).String("secret", "name") == name {
				return true
			}
		}
	}
	return false
}

func asMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

// certSpec digests the settings the Secret was issued for. The CA is not in
// it: currentSecret checks the signature and ca.crt directly, which also
// catches a Secret edited in place.
func certSpec(cfg SingleCertConfig, sans []string, provided *tlsMaterial) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s|%s|%s|%v|%s|%s|%d|%s|", cfg.Issuer, cfg.CommonName, cfg.Subject.String(), sans, strings.Join(cfg.IPs, ","), cfg.KeyType, cfg.KeyBits, cfg.Validity)
	if provided != nil {
		h.Write(provided.cert)
		h.Write(provided.key)
		h.Write(provided.ca)
	}
	return hex.EncodeToString(h.Sum(nil))[:32]
}

// currentSecret returns the Secret's material when every namespace already
// holds a copy that is valid for these settings: issued for this spec, the key
// matching, not within the renewal window, and signed by the current CA (or,
// provided, exactly the operator's certificate). Otherwise nil, and why it
// must be (re)written. The kept copy is written into every namespace, so
// namespaces holding different valid copies end up equal.
func currentSecret(ctx context.Context, env *Env, cfg SingleCertConfig, spec string, signer *x509.Certificate, provided *tlsMaterial) (*tlsMaterial, string) {
	var first *tlsMaterial
	for _, ns := range cfg.Namespaces {
		ref := ns + "/" + cfg.SecretName
		s, err := env.Kube.Get(ctx, GVRSecret.Path(ns, cfg.SecretName))
		if err != nil {
			return nil, ref + " missing"
		}
		if s.Annotations()[annoSingleCertSpec] != spec {
			return nil, "the settings changed"
		}
		m, err := tlsFromSecret(s, true)
		if err != nil {
			return nil, fmt.Sprintf("%s: %v", ref, err)
		}
		leaf, _, _, err := singlecert.Pair(m.cert, m.key)
		if err != nil {
			return nil, fmt.Sprintf("%s: %v", ref, err)
		}
		if cfg.Now().Add(cfg.RenewBefore).After(leaf.NotAfter) {
			return nil, fmt.Sprintf("it expires %s, within the renewal window", leaf.NotAfter.Format(time.DateOnly))
		}
		switch {
		case provided != nil:
			if string(m.cert) != string(provided.cert) || string(m.key) != string(provided.key) || string(m.ca) != string(provided.ca) {
				return nil, ref + " is not the provided certificate"
			}
		case signer != nil:
			// An edited Secret keeps the annotation; the signature tells.
			if leaf.CheckSignatureFrom(signer) != nil || string(m.ca) != string(pemCert(signer.Raw)) {
				return nil, ref + " is not signed by the current CA"
			}
		}
		if first == nil {
			first = m
		}
	}
	return first, ""
}

// selfSignedCA returns the CA kept in <StateNS>/<SecretName>-ca, generating
// it (or a fresh one when it is about to expire).
func selfSignedCA(ctx context.Context, env *Env, cfg SingleCertConfig, res *Result) (*x509.Certificate, crypto.Signer, error) {
	ref := cfg.StateNS + "/" + cfg.SecretName + "-ca"
	if s, err := env.Kube.Get(ctx, GVRSecret.Path(cfg.StateNS, cfg.SecretName+"-ca")); err == nil {
		if m, err := tlsFromSecret(s, false); err == nil {
			if ca, key, err := singlecert.CA(m.cert, m.key, cfg.Now(), cfg.RenewBefore); err == nil {
				res.Pass("ca", "self-signed CA %s kept (%s), valid until %s", ca.Subject.CommonName, ref, ca.NotAfter.Format(time.DateOnly))
				return ca, key, nil
			}
		}
	}
	key, err := newKey(cfg)
	if err != nil {
		return nil, nil, err
	}
	subj := cfg.Subject
	subj.CommonName = cfg.CACommonName
	now := cfg.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial(), Subject: subj,
		NotBefore: now.Add(-5 * time.Minute), NotAfter: now.Add(cfg.Validity),
		IsCA: true, BasicConstraintsValid: true,
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageCRLSign | x509.KeyUsageCertSign,
		SubjectKeyId: keyID(key.Public()),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	if err != nil {
		return nil, nil, err
	}
	ca, _ := x509.ParseCertificate(der)
	m := &tlsMaterial{cert: pemCert(der), key: pemKey(key)}
	if err := writeTLS(ctx, env, cfg.StateNS, cfg.SecretName+"-ca", m, "", ""); err != nil {
		return nil, nil, fmt.Errorf("keeping the CA in %s: %w", ref, err)
	}
	res.Pass("ca", "self-signed CA %s generated (%s), valid until %s", subj.CommonName, ref, ca.NotAfter.Format(time.DateOnly))
	return ca, key, nil
}

// issueLeaf issues the single certificate: F5's subject and names, for both
// server and client use (it serves every component's mTLS in both directions).
func issueLeaf(cfg SingleCertConfig, sans []string, ca *x509.Certificate, caKey crypto.Signer) (*tlsMaterial, error) {
	key, err := newKey(cfg)
	if err != nil {
		return nil, err
	}
	subj := cfg.Subject
	subj.CommonName = cfg.CommonName
	now := cfg.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial(), Subject: subj, DNSNames: sans,
		NotBefore: now.Add(-5 * time.Minute), NotAfter: now.Add(cfg.Validity),
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		SubjectKeyId:          keyID(key.Public()),
	}
	if _, ok := key.Public().(*rsa.PublicKey); ok {
		tmpl.KeyUsage |= x509.KeyUsageKeyEncipherment // RSA key exchange; not an ECDSA usage (RFC 5480)
	}
	for _, ip := range cfg.IPs {
		if p := net.ParseIP(ip); p != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, p)
		} else {
			return nil, fmt.Errorf("cert: --ip %q is not an IP address", ip)
		}
	}
	if tmpl.NotAfter.After(ca.NotAfter) {
		tmpl.NotAfter = ca.NotAfter // a certificate cannot outlive its CA
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, key.Public(), caKey)
	if err != nil {
		return nil, err
	}
	return &tlsMaterial{cert: pemCert(der), key: pemKey(key), ca: pemCert(ca.Raw)}, nil
}

func readTLS(ctx context.Context, env *Env, ref string, needCA bool) (*tlsMaterial, error) {
	ns, name, ok := strings.Cut(ref, "/")
	if !ok || ns == "" || name == "" {
		return nil, fmt.Errorf("cert: --source-secret %q: want namespace/name", ref)
	}
	s, err := env.Kube.Get(ctx, GVRSecret.Path(ns, name))
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", ref, err)
	}
	m, err := tlsFromSecret(s, needCA)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", ref, err)
	}
	return m, nil
}

func tlsFromSecret(s kube.Object, needCA bool) (*tlsMaterial, error) {
	get := func(k string) ([]byte, error) {
		enc := s.String("data", k)
		if enc == "" {
			return nil, fmt.Errorf("no %s", k)
		}
		return base64.StdEncoding.DecodeString(enc)
	}
	m := &tlsMaterial{}
	var err error
	if m.cert, err = get("tls.crt"); err != nil {
		return nil, err
	}
	if m.key, err = get("tls.key"); err != nil {
		return nil, err
	}
	if needCA {
		if m.ca, err = get("ca.crt"); err != nil {
			return nil, err
		}
	}
	return m, nil
}

func writeTLS(ctx context.Context, env *Env, ns, name string, m *tlsMaterial, spec, restarted string) error {
	data := map[string]any{
		"tls.crt": base64.StdEncoding.EncodeToString(m.cert),
		"tls.key": base64.StdEncoding.EncodeToString(m.key),
	}
	if m.ca != nil {
		data["ca.crt"] = base64.StdEncoding.EncodeToString(m.ca)
	}
	md := map[string]any{"name": name, "namespace": ns,
		"labels": map[string]any{"app.kubernetes.io/managed-by": "roksbnkargoctl"}}
	anno := map[string]any{}
	if spec != "" {
		anno[annoSingleCertSpec] = spec
	}
	if restarted != "" {
		anno[annoSingleCertRestarted] = restarted
	}
	if len(anno) > 0 {
		md["annotations"] = anno
	}
	obj := map[string]any{"apiVersion": "v1", "kind": "Secret", "type": "kubernetes.io/tls", "metadata": md, "data": data}
	_, err := env.Kube.Apply(ctx, GVRSecret.Path(ns, name), obj, FieldManager, true)
	return err
}

func newKey(cfg SingleCertConfig) (crypto.Signer, error) {
	switch cfg.KeyType {
	case "", "rsa":
		bits := cfg.KeyBits
		if bits == 0 {
			bits = 4096
		}
		if bits < 2048 {
			return nil, fmt.Errorf("cert: --key-bits %d: at least 2048", bits)
		}
		return rsa.GenerateKey(rand.Reader, bits)
	case "ecdsa":
		return ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	}
	return nil, fmt.Errorf("cert: --key-type %q: rsa or ecdsa", cfg.KeyType)
}

// pemKey encodes a key as cert-manager does (PKCS#1 for RSA, SEC 1 for EC),
// which is what BNK's components read from cert-manager's Secrets today.
func pemKey(k crypto.Signer) []byte {
	switch key := k.(type) {
	case *rsa.PrivateKey:
		return pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	case *ecdsa.PrivateKey:
		b, _ := x509.MarshalECPrivateKey(key)
		return pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: b})
	}
	return nil
}

func pemCert(der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func serial() *big.Int {
	n, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	return n
}

func keyID(pub crypto.PublicKey) []byte {
	b, _ := x509.MarshalPKIXPublicKey(pub)
	sum := sha256.Sum256(b)
	return sum[:20]
}

func leafNotAfter(certPEM []byte) string {
	b, _ := pem.Decode(certPEM)
	if b == nil {
		return "?"
	}
	c, err := x509.ParseCertificate(b.Bytes)
	if err != nil {
		return "?"
	}
	return c.NotAfter.Format(time.DateOnly)
}
