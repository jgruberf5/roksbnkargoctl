package forge

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// #113. --insecure is genuinely opt-in and the comment is right about its
// purpose — self-signed lab installs. What it does not account for is what
// travels over the connection: the session token goes on every request, so
// disabling verification leaves it encrypted but UNAUTHENTICATED. Anyone on the
// path can present a certificate for the Forge host and read the token.
//
// The setting persists in config.yaml, so it is typically enabled once for a
// lab and forgotten — including when the workspace is later pointed at a
// production Forge. Warning at construction would miss that; the warning has to
// attach to the requests that actually carry the credential.
func TestInsecureClientWarnsOnEveryRunButOnlyOnce(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	var warn bytes.Buffer
	c := mustNew(t, srv.URL, Options{Insecure: true})
	c.Log = &warn
	c.Token = "a-session-token"

	if warn.Len() != 0 {
		t.Error("a client that has sent nothing has disclosed nothing — no warning until a request goes out")
	}

	for i := 0; i < 3; i++ {
		_, _, _ = c.do(context.Background(), http.MethodGet, "/anything", nil)
	}

	got := warn.String()
	if got == "" {
		t.Fatal("a request carrying the token over an unverified connection must warn")
	}
	for _, want := range []string{"DISABLED", "token", "ca_file"} {
		if !strings.Contains(got, want) {
			t.Errorf("the warning should mention %q, got:\n%s", want, got)
		}
	}
	if n := strings.Count(got, "DISABLED"); n != 1 {
		t.Errorf("three requests produced %d warnings — once per client is enough to be seen without becoming noise", n)
	}
}

// A verified client says nothing. A warning on the normal path is noise that
// trains people to ignore the one that matters.
func TestVerifiedClientIsSilent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	var warn bytes.Buffer
	c := mustNew(t, srv.URL, Options{})
	c.Log = &warn
	c.Token = "a-session-token"
	_, _, _ = c.do(context.Background(), http.MethodGet, "/anything", nil)

	if warn.Len() != 0 {
		t.Errorf("a verified connection must not warn, got:\n%s", warn.String())
	}
}

// Pinning the lab CA is the answer --insecure was reaching for: it
// authenticates the connection instead of abandoning authentication. This
// drives a real TLS handshake against a server whose certificate chains only to
// the pinned CA — the system roots cannot verify it.
func TestPinnedCAVerifiesASelfSignedForge(t *testing.T) {
	caPEM, certPEM, keyPEM := selfSignedCA(t)

	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}))
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	srv.StartTLS()
	defer srv.Close()

	t.Run("without the CA the handshake fails", func(t *testing.T) {
		c := mustNew(t, srv.URL, Options{})
		if _, _, err := c.do(context.Background(), http.MethodGet, "/x", nil); err == nil {
			t.Fatal("a self-signed Forge must not verify against the system roots")
		}
	})

	t.Run("with the CA pinned it succeeds, and is silent", func(t *testing.T) {
		var warn bytes.Buffer
		c := mustNew(t, srv.URL, Options{CAPEM: caPEM})
		c.Log = &warn
		if _, _, err := c.do(context.Background(), http.MethodGet, "/x", nil); err != nil {
			t.Fatalf("pinned CA should verify this server: %v", err)
		}
		if warn.Len() != 0 {
			t.Errorf("a pinned CA is a verified connection — it must not warn, got:\n%s", warn.String())
		}
	})

	// A pinned CA must WIN over Insecure, and proving that needs a server the
	// pin should REJECT. Asserting only that the matching server still connects
	// passes either way — InsecureSkipVerify accepts it too — which is a test
	// that restates the implementation instead of constraining it.
	t.Run("a pinned CA wins over insecure, and still rejects a foreign cert", func(t *testing.T) {
		otherCA, otherCert, otherKey := selfSignedCA(t)
		if bytes.Equal(otherCA, caPEM) {
			t.Fatal("the two CAs must differ for this to prove anything")
		}
		other := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{}`))
		}))
		pair, err := tls.X509KeyPair(otherCert, otherKey)
		if err != nil {
			t.Fatal(err)
		}
		other.TLS = &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS12}
		other.StartTLS()
		defer other.Close()

		var warn bytes.Buffer
		c := mustNew(t, other.URL, Options{CAPEM: caPEM, Insecure: true})
		c.Log = &warn
		if _, _, err := c.do(context.Background(), http.MethodGet, "/x", nil); err == nil {
			t.Fatal("Insecure took precedence over the pinned CA — a certificate from a " +
				"different authority was accepted, which is the whole failure the pin exists to prevent")
		}
		if warn.Len() != 0 {
			t.Errorf("pinning authenticates the connection, so insecure is moot — no warning expected, got:\n%s", warn.String())
		}

		// And the matching server still connects, so the pin is not simply
		// rejecting everything.
		ok := mustNew(t, srv.URL, Options{CAPEM: caPEM, Insecure: true})
		ok.Log = &bytes.Buffer{}
		if _, _, err := ok.do(context.Background(), http.MethodGet, "/x", nil); err != nil {
			t.Fatalf("the pinned CA's own server should verify: %v", err)
		}
	})
}

// A CA that is not a CA has to be rejected where it is supplied, not at the
// next connection — otherwise the failure surfaces far from its cause.
func TestNewRejectsAPEMWithNoCertificate(t *testing.T) {
	for _, body := range [][]byte{
		[]byte("not pem at all"),
		[]byte("-----BEGIN RSA PRIVATE KEY-----\nZm9v\n-----END RSA PRIVATE KEY-----\n"),
	} {
		if _, err := New("https://forge.example.com", Options{CAPEM: body}); err == nil {
			t.Errorf("expected a rejection for %q", string(body[:min(20, len(body))]))
		}
	}
}

// The extra "this leaves your network" line only fires where the answer is
// certain — an IP literal. A hostname cannot be classified without resolving
// it, and calling a lab name public would be a guess presented as a fact.
func TestIsPublicIPOnlyClassifiesLiterals(t *testing.T) {
	for _, tc := range []struct {
		host string
		want bool
	}{
		{"203.0.113.10", true},
		{"203.0.113.10:8443", true},
		// IPv6: only the ULA range fc00::/7 counts as private, so anything
		// outside it reads as public. 2001:db8::/32 is reserved for
		// documentation rather than routable, but classifying it as public
		// only makes the warning louder — the safe direction for an address
		// we cannot confirm is contained.
		{"[2001:db8::1]:443", true},
		{"[fd00::1]:443", false},
		{"[::1]:443", false},
		// Bracketed v6 WITHOUT a port: SplitHostPort fails, so the brackets
		// must be stripped explicitly or a public address reads as not-public.
		{"[2600:1f18::1]", true},
		{"[fd00::1]", false},
		{"2600:1f18::1", true},
		{"10.241.0.4", false},
		{"192.168.1.10:8443", false},
		{"172.16.0.1", false},
		{"127.0.0.1", false},
		{"169.254.1.1", false},
		{"forge.example.com", false},
		{"forge.lab.internal:8443", false},
		{"localhost", false},
	} {
		if got := isPublicIP(tc.host); got != tc.want {
			t.Errorf("isPublicIP(%q) = %v, want %v", tc.host, got, tc.want)
		}
	}
}

// ── helpers ─────────────────────────────────────────────────────────────────

func selfSignedCA(t *testing.T) (caPEM, certPEM, keyPEM []byte) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "roksbnkargoctl test forge CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	pemCert := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	pemKey := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	return pemCert, pemCert, pemKey
}
