package checks

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jgruberf5/roksbnkargoctl/cmd/check/internal/kube/kubefake"
)

// The live failure: the webhook's CA is not injected yet, so the API server
// answers the ClusterIssuer create with a 500 "failed calling webhook ... x509".
// The gate must retry through that, dry-run only, then pass.
func TestCertManagerReadyRetriesThroughTheCARace(t *testing.T) {
	s, env, res := newFake(t)
	var posts atomic.Int32
	s.Hook = func(_ *kubefake.Server, r kubefake.Request) *kubefake.Reply {
		if r.Method != "POST" || r.Path != clusterIssuersPath {
			return nil
		}
		if r.Query.Get("dryRun") != "All" {
			rep := kubefake.Status(400, "BadRequest", "not a dry run: the gate must never create anything")
			return &rep
		}
		if posts.Add(1) <= 2 {
			rep := kubefake.Status(500, "InternalError", `Internal error occurred: failed calling webhook "webhook.cert-manager.io": failed to call webhook: Post "https://cert-manager-webhook.cert-manager.svc:443/validate?timeout=30s": tls: failed to verify certificate: x509: certificate signed by unknown authority`)
			return &rep
		}
		return &kubefake.Reply{Code: 201, Body: map[string]any{"kind": "ClusterIssuer"}}
	}
	if err := CertManagerReady(context.Background(), env, CertManagerReadyConfig{Interval: 10 * time.Millisecond, Timeout: 5 * time.Second}, res); err != nil {
		t.Fatal(err)
	}
	if res.Failed() {
		t.Fatalf("should pass once the webhook admits: %v", res.Failures())
	}
	if posts.Load() != 3 {
		t.Fatalf("expected 3 dry-run attempts, got %d", posts.Load())
	}
}

// A refusal retrying cannot fix (RBAC) must fail at once, naming it, rather than
// burn the whole timeout and report "not ready".
func TestCertManagerReadyFailsFastOnForbidden(t *testing.T) {
	s, env, res := newFake(t)
	var posts atomic.Int32
	s.Hook = func(_ *kubefake.Server, r kubefake.Request) *kubefake.Reply {
		if r.Method == "POST" && r.Path == clusterIssuersPath {
			posts.Add(1)
			rep := kubefake.Status(403, "Forbidden", `clusterissuers.cert-manager.io is forbidden: User "system:serviceaccount:roksbnkargoctl-check:check" cannot create resource`)
			return &rep
		}
		return nil
	}
	_ = CertManagerReady(context.Background(), env, CertManagerReadyConfig{Interval: 10 * time.Millisecond, Timeout: 5 * time.Second}, res)
	if !res.Failed() || posts.Load() != 1 {
		t.Fatalf("want one attempt and a failure, got %d attempts, failed=%v", posts.Load(), res.Failed())
	}
}

func TestCertManagerReadyTimesOut(t *testing.T) {
	s, env, res := newFake(t)
	s.Hook = func(_ *kubefake.Server, r kubefake.Request) *kubefake.Reply {
		if r.Method == "POST" && r.Path == clusterIssuersPath {
			rep := kubefake.Status(500, "InternalError", `failed calling webhook "webhook.cert-manager.io": x509`)
			return &rep
		}
		return nil
	}
	_ = CertManagerReady(context.Background(), env, CertManagerReadyConfig{Interval: 10 * time.Millisecond, Timeout: 60 * time.Millisecond}, res)
	if !res.Failed() {
		t.Fatal("a webhook that never admits must fail the gate")
	}
}

// A bare 5xx with no webhook wording (the API server itself restarting) is also
// "not yet". Without this case the 5xx rule was untested: removing it left the
// suite green, because the other test's message matched the string rules too.
func TestCertManagerReadyRetriesPlain5xx(t *testing.T) {
	s, env, res := newFake(t)
	var posts atomic.Int32
	s.Hook = func(_ *kubefake.Server, r kubefake.Request) *kubefake.Reply {
		if r.Method != "POST" || r.Path != clusterIssuersPath {
			return nil
		}
		if posts.Add(1) == 1 {
			rep := kubefake.Status(503, "ServiceUnavailable", "the server is currently unable to handle the request")
			return &rep
		}
		return &kubefake.Reply{Code: 201, Body: map[string]any{"kind": "ClusterIssuer"}}
	}
	_ = CertManagerReady(context.Background(), env, CertManagerReadyConfig{Interval: 10 * time.Millisecond, Timeout: 5 * time.Second}, res)
	if res.Failed() || posts.Load() != 2 {
		t.Fatalf("a plain 503 must be retried: attempts=%d failed=%v %v", posts.Load(), res.Failed(), res.Failures())
	}
}
