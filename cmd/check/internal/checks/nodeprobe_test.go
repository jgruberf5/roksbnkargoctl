package checks

import (
	"context"
	"crypto/x509"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestParseTarget(t *testing.T) {
	cases := map[string]ProbeTarget{
		"cr.f5.com":                       {Host: "cr.f5.com", Port: "443"},
		"cr.f5.com:443,tls":               {Host: "cr.f5.com", Port: "443", TLS: true},
		"10.243.0.5:8443,insecure":        {Host: "10.243.0.5", Port: "8443", TLS: true, Insecure: true},
		"10.243.0.5:8443,tls,insecure":    {Host: "10.243.0.5", Port: "8443", TLS: true, Insecure: true},
		"https://mirror.example:5000/v2/": {Host: "mirror.example", Port: "5000", TLS: true},
		"[fd00::1]:443":                   {Host: "fd00::1", Port: "443"},
	}
	for in, want := range cases {
		got, err := ParseTarget(in)
		if err != nil || got != want {
			t.Errorf("ParseTarget(%q) = %+v, %v; want %+v", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "h:0", "h:99999", "h:443,bogus"} {
		if _, err := ParseTarget(bad); err == nil {
			t.Errorf("ParseTarget(%q) accepted", bad)
		}
	}
}

func hostPort(t *testing.T, raw string) (string, string) {
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	h, p, _ := net.SplitHostPort(u.Host)
	return h, p
}

func TestProbeTLSReportsSubjectAndVerification(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()
	h, p := hostPort(t, srv.URL)
	pr := &Prober{DialTimeout: 2 * time.Second}

	// Self-signed and not trusted: handshake ok, verification fails, subject reported.
	r := pr.ProbeOnce(context.Background(), ProbeTarget{Host: h, Port: p, TLS: true})
	if r.OK || r.TCP != "ok" || r.Handshake != "ok" || r.Verified != "no" || r.CertIssuer == "" {
		t.Fatalf("untrusted chain: %+v", r)
	}
	// Insecure: passes, still reports who answered.
	r = pr.ProbeOnce(context.Background(), ProbeTarget{Host: h, Port: p, TLS: true, Insecure: true})
	if !r.OK || !strings.HasPrefix(r.Verified, "skipped") || r.CertIssuer == "" {
		t.Fatalf("insecure: %+v", r)
	}
	// Trusted via --ca-file roots.
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	pr.ExtraRoots = pool
	r = pr.ProbeOnce(context.Background(), ProbeTarget{Host: h, Port: p, TLS: true})
	if !r.OK || r.Verified != "yes" {
		t.Fatalf("trusted: %+v", r)
	}
}

func TestProbeNonTLSPortFailsHandshake(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_, _ = c.Write([]byte("SSH-2.0-not-tls\r\n"))
			c.Close()
		}
	}()
	h, p, _ := net.SplitHostPort(ln.Addr().String())
	pr := &Prober{DialTimeout: 2 * time.Second}
	if r := pr.ProbeOnce(context.Background(), ProbeTarget{Host: h, Port: p}); !r.OK {
		t.Fatalf("plain tcp: %+v", r)
	}
	if r := pr.ProbeOnce(context.Background(), ProbeTarget{Host: h, Port: p, TLS: true}); r.OK || r.Handshake != "FAILED" {
		t.Fatalf("non-TLS port passed a TLS probe: %+v", r)
	}
}

func TestProbeDNSFailureIsReportedAsDNS(t *testing.T) {
	// A resolver whose DNS server is unreachable: deterministic, unlike a real
	// lookup of .invalid, which some networks answer.
	res := &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return nil, errors.New("no DNS server")
	}}
	dialed := false
	pr := &Prober{DialTimeout: 2 * time.Second, Resolver: res, Dial: func(context.Context, string, string) (net.Conn, error) {
		dialed = true
		return nil, errors.New("unreachable")
	}}
	r := pr.ProbeOnce(context.Background(), ProbeTarget{Host: "far.example.com", Port: "443"})
	if r.OK || r.DNS != "FAILED" || r.TCP != "skipped" || dialed {
		t.Fatalf("%+v (dialed=%v)", r, dialed)
	}
}

// Retries until success inside the window (the TGW-routes-not-programmed-yet
// case, roksbnkctl #57), and reports the attempts.
func TestProbeRetriesUntilReachable(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	calls := 0
	pr := &Prober{DialTimeout: time.Second, Window: 3 * time.Second, Interval: 20 * time.Millisecond,
		Dial: func(ctx context.Context, n, _ string) (net.Conn, error) {
			calls++
			if calls < 3 {
				return nil, errors.New("connect: no route to host")
			}
			return (&net.Dialer{}).DialContext(ctx, n, ln.Addr().String())
		}}
	r := pr.Probe(context.Background(), ProbeTarget{Host: "10.243.0.5", Port: "8443"})
	if !r.OK || r.Attempts != 3 {
		t.Fatalf("did not retry to success: %+v", r)
	}
	// And gives up at the window, saying so.
	calls = -1000
	pr.Window = 60 * time.Millisecond
	r = pr.Probe(context.Background(), ProbeTarget{Host: "10.243.0.5", Port: "8443"})
	if r.OK || r.Attempts < 2 || !strings.Contains(r.Error, "still failing after") {
		t.Fatalf("window not honoured: %+v", r)
	}
}
