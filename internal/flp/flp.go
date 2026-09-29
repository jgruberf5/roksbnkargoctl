// Package flp builds the F5 License Proxy for disconnected mode: a VSI running
// the f5-license-proxy stack as a podman pod, in its own VPC on the transit
// gateway, ported from roksbnkctl's proven flp_vsi module.
//
// BNK's License CR reaches it at https://<private-ip>:8443/license-proxy/v1 and
// trusts it through the CA generated here.
package flp

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	_ "embed"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"strings"
	"text/template"
	"time"

	"github.com/jgruberf5/roksbnkargoctl/internal/far"
)

// Versions. The BNK 2.4.0 GA manifest no longer lists the license proxy (2.3.x
// did — roksbnkctl#327), so it is pinned here, to the newest tag on FAR when
// 2.4.0 GA shipped. The proxy, vault-init and postgresql images share its tag.
const (
	DefaultVersion  = "1.29.0-0.10.40"
	DefaultVaultTag = "2.0.0"
	F5CertURL       = "https://product.apis.f5.com/ee/v1"
	F5EntitleURL    = "https://product-s.apis.f5.com/ee/v1"
	F5InitConfigURL = "https://product-s.apis.f5.com/ee/v1"
	Port            = 8443
)

//go:embed files/cloud-init.yaml.tmpl
var cloudInitTmpl string

//go:embed files/flp-pod-up.sh
var podUp []byte

// CA is the license proxy's root: BNK trusts it, the VSI signs its leaves with it.
type CA struct {
	CertPEM []byte
	KeyPEM  []byte
}

// NewCA mints the CA (ECDSA P-256, CN flp.ca, O=F5, 10 years) — the same shape
// roksbnkctl generated in Terraform.
func NewCA() (*CA, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "flp.ca", Organization: []string{"F5"}},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(10, 0, 0),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	kb, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, err
	}
	return &CA{
		CertPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		KeyPEM:  pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}),
	}, nil
}

var jwksRE = regexp.MustCompile(`(?m)^\s*prod_jwks\.txt:\s*([A-Za-z0-9+/=]+)\s*$`)

// ProdJWKS extracts F5's public JWT-verification key set that ships inside the
// f5-license-proxy chart (base64 under `prod_jwks.txt:` in a template).
func ProdJWKS(chartTGZ []byte) ([]byte, error) {
	files, err := far.ChartFiles(chartTGZ)
	if err != nil {
		return nil, err
	}
	for name, b := range files {
		if !strings.HasSuffix(name, ".yaml") {
			continue
		}
		if m := jwksRE.FindSubmatch(b); m != nil {
			out, err := base64.StdEncoding.DecodeString(string(m[1]))
			if err != nil {
				return nil, fmt.Errorf("prod_jwks.txt in %s: %w", name, err)
			}
			if !bytes.Contains(out, []byte(`"keys"`)) {
				return nil, fmt.Errorf("prod_jwks.txt in %s is not a JWKS", name)
			}
			return out, nil
		}
	}
	return nil, errors.New("the f5-license-proxy chart carries no prod_jwks.txt")
}

// CloudInitInput fills the cloud-init template.
type CloudInitInput struct {
	JWT               string
	FARServiceAccount string
	FARHost           string
	Version           string
	VaultTag          string
	ExternalIP        string
	ProdJWKS          []byte
	CA                *CA
}

// CloudInit renders the VSI user data.
func CloudInit(in CloudInitInput) (string, error) {
	if in.JWT == "" || in.FARServiceAccount == "" || len(in.ProdJWKS) == 0 || in.CA == nil {
		return "", errors.New("flp cloud-init: JWT, FAR key, prod_jwks and CA are required")
	}
	if strings.ContainsAny(in.JWT+in.FARServiceAccount, "\n\r") {
		return "", errors.New("flp cloud-init: JWT and FAR key must be single-line")
	}
	if in.Version == "" {
		in.Version = DefaultVersion
	}
	if in.VaultTag == "" {
		in.VaultTag = DefaultVaultTag
	}
	if in.FARHost == "" {
		in.FARHost = "repo.f5.com"
	}
	t, err := template.New("ci").Option("missingkey=error").Parse(cloudInitTmpl)
	if err != nil {
		return "", err
	}
	b64 := func(b []byte) string { return base64.StdEncoding.EncodeToString(b) }
	var buf bytes.Buffer
	err = t.Execute(&buf, map[string]string{
		"JWT": in.JWT, "ExternalIP": in.ExternalIP, "FARHost": in.FARHost,
		"Registry": in.FARHost + "/images", "Tag": in.Version, "VaultTag": in.VaultTag,
		"CertURL": F5CertURL, "EntitlementURL": F5EntitleURL, "InitialConfigURL": F5InitConfigURL,
		"FARServiceAccount": in.FARServiceAccount,
		"ProdJWKSB64":       b64(in.ProdJWKS), "CACertB64": b64(in.CA.CertPEM), "CAKeyB64": b64(in.CA.KeyPEM),
		"PodUpB64": b64(podUp),
	})
	if err != nil {
		return "", err
	}
	if buf.Len() > 64*1024 {
		return "", fmt.Errorf("flp cloud-init is %d bytes; IBM VPC user data is limited to 64 KiB", buf.Len())
	}
	return buf.String(), nil
}

// URL is the endpoint BNK's License CR uses.
func URL(privateIP string) string { return fmt.Sprintf("https://%s:%d", privateIP, Port) }
