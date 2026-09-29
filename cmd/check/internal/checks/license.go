package checks

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jgruberf5/roksbnkargoctl/cmd/check/internal/kube"
)

// License modes and FLP constants.
const (
	ModeConnected      = "connected"
	ModeF5LicenseProxy = "f5licenseproxy"
	// DefaultFLPCAPath is where CWC mounts Secret licenseserver-rootca.
	DefaultFLPCAPath = "/etc/cm20/licenseserver-rootca/licenseserver-rootca.txt"
	// FLPAPIPath is the FLP licensing base — NOT the /ee/v1 path the FLP uses
	// toward F5 upstream. CWC appends its operations to it.
	FLPAPIPath      = "/license-proxy/v1"
	FieldManager    = "roksbnkargoctl-check"
	FLPRootCASecret = "licenseserver-rootca"
	FLPRootCAKey    = "licenseserver-rootca.txt"
	CWCDeployment   = "f5-spk-cwc"
	CWCCAHashAnnot  = "roksbnkargoctl.io/flp-ca-hash"
)

// LicenseConfig configures the license mode.
type LicenseConfig struct {
	Namespace          string // License namespace (f5-utils)
	CNENamespace       string // CNEInstance namespace (f5-bnk)
	Name               string // bnk-license
	JWTSecret          string
	JWTSecretNamespace string
	JWTKey             string
	Mode               string // connected | f5licenseproxy
	FLPURL             string // https://<ip>:8443
	FLPCAPath          string
	RestartCWC         bool // FLP mode: roll CWC so it trusts the CA it may have started without

	CRDTimeout     time.Duration // 10m
	ApplyRetry     time.Duration // 5m
	LicenseTimeout time.Duration // 15m
	CNETimeout     time.Duration // 15m
	Poll           time.Duration // 5s
}

// NormaliseMode maps the config vocabulary onto License.spec.operationMode.
func NormaliseMode(m string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(m)) {
	case "", ModeConnected:
		return ModeConnected, nil
	case ModeF5LicenseProxy, "disconnected", "flp":
		return ModeF5LicenseProxy, nil
	}
	return "", fmt.Errorf("unknown license mode %q (want connected or f5licenseproxy)", m)
}

// FLPTeemURL is the teem*Url for an FLP base URL, accepting the URL with or
// without the API path already on it.
func FLPTeemURL(base string) string {
	b := strings.TrimRight(strings.TrimSpace(base), "/")
	if strings.HasSuffix(b, FLPAPIPath) {
		return b
	}
	return b + FLPAPIPath
}

// BuildLicense renders the License CR. The JWT is inline because the 2.4 CRD
// requires it inline (no Secret reference) — which is why this is built in a
// hook from a Secret instead of living in Git.
func BuildLicense(cfg LicenseConfig, jwt string) (kube.Object, error) {
	mode, err := NormaliseMode(cfg.Mode)
	if err != nil {
		return nil, err
	}
	spec := map[string]any{"jwt": jwt, "operationMode": mode}
	if mode == ModeF5LicenseProxy {
		if cfg.FLPURL == "" {
			return nil, errors.New("--flp-url is required in f5licenseproxy mode")
		}
		u := FLPTeemURL(cfg.FLPURL)
		ca := cfg.FLPCAPath
		if ca == "" {
			ca = DefaultFLPCAPath
		}
		spec["teemCertUrl"] = u
		spec["teemEntitlementUrl"] = u
		spec["teemInitialConfigUrl"] = u
		spec["licenseProxyServerRootCaPath"] = ca
	}
	return kube.Object{
		"apiVersion": GVRLicense.Group + "/" + GVRLicense.Version,
		"kind":       GVRLicense.Kind,
		"metadata":   map[string]any{"name": cfg.Name, "namespace": cfg.Namespace},
		"spec":       spec,
	}, nil
}

// LicenseActive reports whether a License is live: status.state Active, or
// condition LicenseActive=True.
func LicenseActive(l kube.Object) bool {
	return l.String("status", "state") == "Active" || l.ConditionTrue("LicenseActive")
}

func secretValue(s kube.Object, key string) (string, error) {
	enc := s.String("data", key)
	if enc == "" {
		if v := s.String("stringData", key); v != "" {
			return v, nil
		}
		return "", fmt.Errorf("secret %s has no key %q", s.Ref(), key)
	}
	b, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		return "", fmt.Errorf("secret %s key %q is not base64: %w", s.Ref(), key, err)
	}
	return strings.TrimSpace(string(b)), nil
}

// License creates the License from the JWT Secret and waits for BNK to come up.
func License(ctx context.Context, env *Env, cfg LicenseConfig, res *Result) error {
	defaults(&cfg)
	sec, err := env.Kube.Get(ctx, GVRSecret.Path(cfg.JWTSecretNamespace, cfg.JWTSecret))
	if err != nil {
		res.Fail("jwt-secret", "reading Secret %s/%s: %v", cfg.JWTSecretNamespace, cfg.JWTSecret, err)
		return nil
	}
	jwt, err := secretValue(sec, cfg.JWTKey)
	if err != nil || jwt == "" {
		res.Fail("jwt-secret", "%v", orErr(err, "empty JWT"))
		return nil
	}
	res.Pass("jwt-secret", "read %s/%s key %s (%d bytes; never logged)", cfg.JWTSecretNamespace, cfg.JWTSecret, cfg.JWTKey, len(jwt))
	lic, err := BuildLicense(cfg, jwt)
	if err != nil {
		res.Fail("license-spec", "%v", err)
		return nil
	}

	// The License CRD is installed at runtime by FLO's crd-installer, not the chart.
	crd := "licenses." + GVRLicense.Group
	if !waitFor(ctx, env, cfg.CRDTimeout, cfg.Poll, "CRD "+crd+" Established", func() (bool, string) {
		o, err := env.Kube.Get(ctx, GVRCRD.Path("", crd))
		if err != nil {
			return false, err.Error()
		}
		return o.ConditionTrue("Established"), "not Established yet"
	}) {
		res.Fail("license-crd", "CRD %s did not appear within %s; FLO's crd-installer has not run (check the FLO pods and the CNEInstance)", crd, cfg.CRDTimeout)
		return nil
	}
	res.Pass("license-crd", "%s is Established", crd)

	if lic.String("spec", "operationMode") == ModeF5LicenseProxy {
		if !prepareFLPTrust(ctx, env, cfg, res) {
			return nil
		}
	}

	if err := applyLicense(ctx, env, cfg, lic); err != nil {
		res.Fail("license-apply", "%v", err)
		return nil
	}
	res.Pass("license-apply", "applied License %s/%s (operationMode %s)", cfg.Namespace, cfg.Name, lic.String("spec", "operationMode"))

	last := ""
	if !waitFor(ctx, env, cfg.LicenseTimeout, cfg.Poll, "License Active", func() (bool, string) {
		o, err := env.Kube.Get(ctx, GVRLicense.Path(cfg.Namespace, cfg.Name))
		if err != nil {
			return false, err.Error()
		}
		st := o.String("status", "state")
		if st != last {
			env.logf("  License state: %q", st)
			last = st
		}
		return LicenseActive(o), "state " + orNone(st)
	}) {
		hint := "check CWC logs in " + cfg.Namespace + " and node egress to F5 licensing"
		if lic.String("spec", "operationMode") == ModeF5LicenseProxy {
			hint = "check the FLP is reachable from the nodes, that Secret " + FLPRootCASecret + " holds its root CA, and CWC logs"
		}
		res.Fail("license-active", "License did not reach state Active within %s (last state %q); %s", cfg.LicenseTimeout, last, hint)
		return nil
	}
	res.Pass("license-active", "License %s/%s is Active", cfg.Namespace, cfg.Name)

	cne := CNEInstanceName(cfg.CNENamespace)
	lastMsg := ""
	if !waitFor(ctx, env, cfg.CNETimeout, cfg.Poll, "CNEInstance Available", func() (bool, string) {
		o, err := env.Kube.Get(ctx, GVRCNEInstance.Path(cfg.CNENamespace, cne))
		if err != nil {
			return false, err.Error()
		}
		st, reason, msg, _ := o.Condition("Available")
		lastMsg = fmt.Sprintf("Available=%s %s %s", orNone(st), reason, msg)
		return o.ConditionTrue("Available"), lastMsg
	}) {
		res.Fail("cneinstance-available", "CNEInstance %s/%s not Available within %s (%s)", cfg.CNENamespace, cne, cfg.CNETimeout, lastMsg)
		return nil
	}
	res.Pass("cneinstance-available", "CNEInstance %s/%s is Available", cfg.CNENamespace, cne)
	return nil
}

func defaults(cfg *LicenseConfig) {
	if cfg.Name == "" {
		cfg.Name = "bnk-license"
	}
	if cfg.JWTKey == "" {
		cfg.JWTKey = "jwt"
	}
	if cfg.CRDTimeout <= 0 {
		cfg.CRDTimeout = 10 * time.Minute
	}
	for _, d := range []*time.Duration{&cfg.LicenseTimeout, &cfg.CNETimeout} {
		if *d <= 0 {
			*d = 15 * time.Minute
		}
	}
	if cfg.ApplyRetry <= 0 {
		cfg.ApplyRetry = 5 * time.Minute
	}
	if cfg.Poll <= 0 {
		cfg.Poll = 5 * time.Second
	}
}

func orErr(err error, fallback string) string {
	if err != nil {
		return err.Error()
	}
	return fallback
}

// waitFor polls cond until true or timeout; it logs the reason at most once a
// minute so a 15-minute wait does not bury the log.
func waitFor(ctx context.Context, env *Env, timeout, poll time.Duration, what string, cond func() (bool, string)) bool {
	deadline := time.Now().Add(timeout)
	var lastLog time.Time
	for {
		ok, why := cond()
		if ok {
			return true
		}
		if time.Since(lastLog) > time.Minute {
			env.logf("waiting for %s: %s", what, why)
			lastLog = time.Now()
		}
		if !time.Now().Before(deadline) {
			return false
		}
		if sleep(ctx, poll) != nil {
			return false
		}
	}
}

// applyLicense server-side applies the License, retrying the admission races.
//
// THE QUOTA RACE (roksbnkctl #90). BNK installs ResourceQuota
// f5-single-license-quota counting count/licenses.k8s.f5net.com. Admission is
// REFUSED while the quota's status is uncomputed, and the quota controller only
// sees a new CRD on its resync — so the first install on a cluster gets
//
//	licenses.k8s.f5net.com "bnk-license" is forbidden: status unknown for quota
//
// for up to a few minutes. 404 (discovery has not caught up with the CRD) and
// 5xx/429 are retried too; anything else is a real error.
func applyLicense(ctx context.Context, env *Env, cfg LicenseConfig, lic kube.Object) error {
	deadline := time.Now().Add(cfg.ApplyRetry)
	path := GVRLicense.Path(cfg.Namespace, cfg.Name)
	for attempt := 1; ; attempt++ {
		_, err := env.Kube.Apply(ctx, path, lic, FieldManager, true)
		if err == nil {
			return nil
		}
		var se *kube.StatusError
		retry := errors.As(err, &se) && (se.Code == http.StatusForbidden || se.Code == http.StatusNotFound ||
			se.Code == http.StatusTooManyRequests || se.Code >= 500 || se.Code == http.StatusConflict)
		retry = retry || WebhookRefusal(err)
		if !retry {
			return fmt.Errorf("applying License: %w", err)
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("applying License: still refused after %s: %w", cfg.ApplyRetry, err)
		}
		env.logf("License apply refused (attempt %d, retrying — the quota controller may not have observed the new CRD yet): %v", attempt, err)
		if err := sleep(ctx, cfg.Poll); err != nil {
			return err
		}
	}
}

// prepareFLPTrust makes sure CWC can verify the FLP before the License points
// it there.
//
// roksbnkctl learned both halves live: a License pointing at the proxy with an
// EMPTY licenseserver-rootca never reaches Active while the apply looks fine; and
// CWC builds its trust store at pod start, so a CA written after it started is
// ignored until it restarts. The restart is keyed on the CA hash so a re-sync
// does not bounce CWC.
func prepareFLPTrust(ctx context.Context, env *Env, cfg LicenseConfig, res *Result) bool {
	var ca string
	ok := waitFor(ctx, env, cfg.CRDTimeout, cfg.Poll, "Secret "+cfg.Namespace+"/"+FLPRootCASecret, func() (bool, string) {
		s, err := env.Kube.Get(ctx, GVRSecret.Path(cfg.Namespace, FLPRootCASecret))
		if err != nil {
			return false, err.Error()
		}
		v, err := secretValue(s, FLPRootCAKey)
		if err != nil || !strings.Contains(v, "BEGIN CERTIFICATE") {
			return false, "no PEM certificate under key " + FLPRootCAKey
		}
		ca = v
		return true, ""
	})
	if !ok {
		res.Fail("flp-rootca", "Secret %s/%s has no PEM under %q: CWC cannot verify the FLP and the License would never go Active. `roksbnkargoctl install` writes it from `flp up`", cfg.Namespace, FLPRootCASecret, FLPRootCAKey)
		return false
	}
	res.Pass("flp-rootca", "Secret %s/%s holds the FLP root CA", cfg.Namespace, FLPRootCASecret)
	if !cfg.RestartCWC {
		return true
	}
	sum := sha256.Sum256([]byte(ca))
	hash := hex.EncodeToString(sum[:])[:16]
	var dep kube.Object
	if !waitFor(ctx, env, cfg.CRDTimeout, cfg.Poll, "Deployment "+cfg.Namespace+"/"+CWCDeployment, func() (bool, string) {
		d, err := env.Kube.Get(ctx, GVRDeployment.Path(cfg.Namespace, CWCDeployment))
		if err != nil {
			return false, err.Error()
		}
		dep = d
		return true, ""
	}) {
		res.Warn("cwc-restart", "Deployment %s/%s did not appear; not restarting CWC", cfg.Namespace, CWCDeployment)
		return true
	}
	if dep.StringMap("spec", "template", "metadata", "annotations")[CWCCAHashAnnot] == hash {
		res.Info("cwc-restart", "CWC already trusts this CA (hash %s)", hash)
		return true
	}
	patch := map[string]any{"spec": map[string]any{"template": map[string]any{"metadata": map[string]any{"annotations": map[string]any{CWCCAHashAnnot: hash}}}}}
	if _, err := env.Kube.Patch(ctx, GVRDeployment.Path(cfg.Namespace, CWCDeployment), kube.MergePatch, patch); err != nil {
		res.Warn("cwc-restart", "could not roll CWC to pick up the FLP CA: %v", err)
		return true
	}
	res.Info("cwc-restart", "rolled %s so it rebuilds its trust store with the FLP CA (hash %s)", CWCDeployment, hash)
	return true
}
