package checks

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"time"

	"github.com/jgruberf5/roksbnkargoctl/cmd/check/internal/kube"
)

// CertManagerReadyConfig configures cert-manager-ready.
type CertManagerReadyConfig struct {
	Interval time.Duration // 5s
	Timeout  time.Duration // 10m
}

// clusterIssuersPath is the collection a dry-run create goes to.
const clusterIssuersPath = "/apis/cert-manager.io/v1/clusterissuers"

// CertManagerReady waits until cert-manager's validating webhook actually
// admits a ClusterIssuer, by server-side dry-run creating one.
//
// Found live on a reinstall: Argo CD moves to the issuer wave once cert-manager's
// Deployments are healthy, but the cainjector may not yet have written the new
// CA into the webhook configuration, and the ClusterIssuer apply fails
// "x509: certificate signed by unknown authority". Deployment health is not
// webhook readiness; this is. A dry run creates nothing.
func CertManagerReady(ctx context.Context, env *Env, cfg CertManagerReadyConfig, res *Result) error {
	if cfg.Interval <= 0 {
		cfg.Interval = 5 * time.Second
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 10 * time.Minute
	}
	body, _ := json.Marshal(map[string]any{
		"apiVersion": "cert-manager.io/v1", "kind": "ClusterIssuer",
		"metadata": map[string]any{"name": "roksbnkargoctl-webhook-probe"},
		"spec":     map[string]any{"selfSigned": map[string]any{}},
	})
	q := url.Values{"dryRun": {"All"}}
	deadline := time.Now().Add(cfg.Timeout)
	var last error
	for attempt := 1; ; attempt++ {
		err := env.Kube.Do(ctx, "POST", clusterIssuersPath, q, "application/json", body, nil)
		if err == nil || kube.IsAlreadyExists(err) {
			res.Pass("cert-manager-webhook", "the cert-manager webhook admits a ClusterIssuer (dry run, attempt %d)", attempt)
			return nil
		}
		if !webhookNotReady(err) {
			res.Fail("cert-manager-webhook", "a dry-run ClusterIssuer was refused for a reason retrying will not fix: %v", err)
			return nil
		}
		if last == nil || last.Error() != err.Error() {
			env.logf("cert-manager webhook not ready yet (attempt %d): %v", attempt, err)
		}
		last = err
		if time.Now().Add(cfg.Interval).After(deadline) {
			res.Fail("cert-manager-webhook", "the cert-manager webhook did not admit a ClusterIssuer within %s; last error: %v", cfg.Timeout, last)
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(cfg.Interval):
		}
	}
}

// webhookNotReady reports errors that mean "not yet": the webhook's CA not yet
// injected, its Service not yet serving, or the CRD not yet established.
func webhookNotReady(err error) bool {
	var se *kube.StatusError
	if errors.As(err, &se) {
		if se.Code == 404 || se.Code >= 500 {
			return true
		}
	} else {
		return true // transport error: the API server itself flickered
	}
	m := err.Error()
	for _, s := range []string{"failed calling webhook", "x509", "connection refused", "no endpoints available", "context deadline exceeded"} {
		if strings.Contains(m, s) {
			return true
		}
	}
	return false
}
