package checks

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jgruberf5/roksbnkargoctl/cmd/check/internal/kube"
	"github.com/jgruberf5/roksbnkargoctl/cmd/check/internal/kube/kubefake"
)

func flpCfg() LicenseConfig {
	return LicenseConfig{
		Namespace: "f5-utils", CNENamespace: "f5-bnk", Name: "bnk-license",
		JWTSecret: "bnk-license-jwt", JWTSecretNamespace: "roksbnkargoctl-check", JWTKey: "jwt",
		Mode: "f5licenseproxy", FLPURL: "https://10.243.0.5:8443/", RestartCWC: true,
		CRDTimeout: 2 * time.Second, ApplyRetry: 2 * time.Second, LicenseTimeout: 2 * time.Second, CNETimeout: 2 * time.Second,
		Poll: 10 * time.Millisecond,
	}
}

func TestBuildLicenseFLPSpec(t *testing.T) {
	l, err := BuildLicense(flpCfg(), "eyJ.jwt")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"jwt":                          "eyJ.jwt",
		"operationMode":                "f5licenseproxy",
		"teemCertUrl":                  "https://10.243.0.5:8443/license-proxy/v1",
		"teemEntitlementUrl":           "https://10.243.0.5:8443/license-proxy/v1",
		"teemInitialConfigUrl":         "https://10.243.0.5:8443/license-proxy/v1",
		"licenseProxyServerRootCaPath": "/etc/cm20/licenseserver-rootca/licenseserver-rootca.txt",
	}
	for k, v := range want {
		if got := l.String("spec", k); got != v {
			t.Errorf("spec.%s = %q, want %q", k, got, v)
		}
	}
	if l.String("apiVersion") != "k8s.f5net.com/v1" || l.Kind() != "License" || l.Ref() != "f5-utils/bnk-license" {
		t.Errorf("bad identity: %v", l)
	}
	// Already-suffixed URL is not doubled; "disconnected" is an alias.
	c := flpCfg()
	c.FLPURL, c.Mode = "https://flp:8443/license-proxy/v1", "disconnected"
	l, _ = BuildLicense(c, "j")
	if l.String("spec", "teemCertUrl") != "https://flp:8443/license-proxy/v1" || l.String("spec", "operationMode") != "f5licenseproxy" {
		t.Errorf("%v", l["spec"])
	}
	// Connected mode carries no FLP fields.
	c.Mode = "connected"
	l, _ = BuildLicense(c, "j")
	if len(l.Map("spec")) != 2 {
		t.Errorf("connected spec has extra fields: %v", l["spec"])
	}
	c.Mode, c.FLPURL = "f5licenseproxy", ""
	if _, err := BuildLicense(c, "j"); err == nil {
		t.Error("FLP mode without --flp-url accepted")
	}
}

const testCA = "-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----"

func licenseCluster(s *kubefake.Server) {
	sec := obj("v1", "Secret", "roksbnkargoctl-check", "bnk-license-jwt")
	sec["data"] = map[string]any{"jwt": b64("eyJ.secret.jwt\n")}
	s.Put("", "v1", "secrets", sec)
	ca := obj("v1", "Secret", "f5-utils", FLPRootCASecret)
	ca["data"] = map[string]any{FLPRootCAKey: b64(testCA)}
	s.Put("", "v1", "secrets", ca)
	s.Put("apps", "v1", "deployments", obj("apps/v1", "Deployment", "f5-utils", CWCDeployment))
	s.AddResource("k8s.f5.com", "v1", "cneinstances", "CNEInstance", true)
	cne := obj("k8s.f5.com/v1", "CNEInstance", "f5-bnk", "f5-bnk-f5-cne-controller")
	with(cne, []string{"status", "conditions"}, conditions("CNEControllerAvailable", "True", "Available", "False"))
	s.Put("k8s.f5.com", "v1", "cneinstances", cne)
}

// The CRD is installed at runtime; the apply hits the quota race twice; the
// License goes Active only after several polls; then the CNEInstance goes
// Available. Every wait must actually wait.
func TestLicenseAppliesFLPSpecAndWaitsActive(t *testing.T) {
	s, env, res := newFake(t)
	licenseCluster(s)
	var crdGets, quota403, licGets, cneGets int
	s.Hook = func(s *kubefake.Server, r kubefake.Request) *kubefake.Reply {
		switch {
		case r.Method == "GET" && strings.HasSuffix(r.Path, "/customresourcedefinitions/licenses.k8s.f5net.com"):
			crdGets++
			if crdGets == 3 { // crd-installer runs
				crd := obj("apiextensions.k8s.io/v1", "CustomResourceDefinition", "", "licenses.k8s.f5net.com")
				with(crd, []string{"status", "conditions"}, conditions("Established", "True"))
				s.Put("apiextensions.k8s.io", "v1", "customresourcedefinitions", crd)
				s.AddResource("k8s.f5net.com", "v1", "licenses", "License", true)
			}
		case r.Method == "PATCH" && strings.Contains(r.Path, "/licenses/bnk-license"):
			if quota403 < 2 {
				quota403++
				rep := kubefake.Status(403, "Forbidden", `licenses.k8s.f5net.com "bnk-license" is forbidden: status unknown for quota: f5-single-license-quota, resources: count/licenses.k8s.f5net.com`)
				return &rep
			}
		case r.Method == "GET" && strings.HasSuffix(r.Path, "/licenses/bnk-license"):
			licGets++
			l := s.Get("k8s.f5net.com", "v1", "licenses", "f5-utils", "bnk-license")
			state := "Verifying"
			if licGets >= 4 {
				state = "Active"
			}
			if l != nil {
				s.Put("k8s.f5net.com", "v1", "licenses", with(l, []string{"status", "state"}, state))
			}
		case r.Method == "GET" && strings.HasSuffix(r.Path, "/cneinstances/f5-bnk-f5-cne-controller"):
			cneGets++
			if cneGets == 3 {
				c := s.Get("k8s.f5.com", "v1", "cneinstances", "f5-bnk", "f5-bnk-f5-cne-controller")
				s.Put("k8s.f5.com", "v1", "cneinstances", with(c, []string{"status", "conditions"}, conditions("CNEControllerAvailable", "True", "Available", "True")))
			}
		}
		return nil
	}
	if err := License(context.Background(), env, flpCfg(), res); err != nil {
		t.Fatal(err)
	}
	if res.Failed() {
		t.Fatalf("failed: %v", res.Failures())
	}
	if crdGets < 3 || quota403 != 2 || licGets < 4 || cneGets < 3 {
		t.Fatalf("did not wait: crdGets=%d quota403=%d licGets=%d cneGets=%d", crdGets, quota403, licGets, cneGets)
	}
	// The apply that landed: server-side apply, our field manager, force, FLP spec.
	var applied *kubefake.Request
	for _, r := range s.Requests() {
		if r.Method == "PATCH" && strings.HasSuffix(r.Path, "/namespaces/f5-utils/licenses/bnk-license") {
			rr := r
			applied = &rr
		}
	}
	if applied == nil {
		t.Fatal("no apply")
	}
	if applied.ContentType != kube.ApplyPatch || applied.Query.Get("fieldManager") != FieldManager || applied.Query.Get("force") != "true" {
		t.Errorf("not a forced SSA as %s: %s %v", FieldManager, applied.ContentType, applied.Query)
	}
	var body kube.Object
	_ = json.Unmarshal(applied.Body, &body)
	if body.String("spec", "jwt") != "eyJ.secret.jwt" || body.String("spec", "teemEntitlementUrl") != "https://10.243.0.5:8443/license-proxy/v1" {
		t.Errorf("applied spec: %v", body["spec"])
	}
	// CWC rolled once for the CA.
	d := s.Get("apps", "v1", "deployments", "f5-utils", CWCDeployment)
	if d.StringMap("spec", "template", "metadata", "annotations")[CWCCAHashAnnot] == "" {
		t.Error("CWC not rolled for the FLP CA")
	}
	// The JWT never reaches the log or the result.
	out := string(res.Finish(nil))
	if strings.Contains(out, "eyJ.secret.jwt") {
		t.Error("JWT leaked into the result")
	}
}

func TestLicenseNeverActiveFails(t *testing.T) {
	s, env, res := newFake(t)
	licenseCluster(s)
	crd := obj("apiextensions.k8s.io/v1", "CustomResourceDefinition", "", "licenses.k8s.f5net.com")
	with(crd, []string{"status", "conditions"}, conditions("Established", "True"))
	s.Put("apiextensions.k8s.io", "v1", "customresourcedefinitions", crd)
	s.AddResource("k8s.f5net.com", "v1", "licenses", "License", true)
	cfg := flpCfg()
	cfg.LicenseTimeout = 100 * time.Millisecond
	_ = License(context.Background(), env, cfg, res)
	if !hasSev(res, "license-active", SevFail) {
		t.Fatalf("a License that never went Active passed: %+v", res.Findings)
	}
	if hasSev(res, "cneinstance-available", SevPass) {
		t.Fatal("went on to the CNEInstance")
	}
}

func TestLicenseFLPEmptyCAFails(t *testing.T) {
	s, env, res := newFake(t)
	licenseCluster(s)
	ca := obj("v1", "Secret", "f5-utils", FLPRootCASecret)
	ca["data"] = map[string]any{FLPRootCAKey: ""}
	s.Put("", "v1", "secrets", ca)
	crd := obj("apiextensions.k8s.io/v1", "CustomResourceDefinition", "", "licenses.k8s.f5net.com")
	with(crd, []string{"status", "conditions"}, conditions("Established", "True"))
	s.Put("apiextensions.k8s.io", "v1", "customresourcedefinitions", crd)
	s.AddResource("k8s.f5net.com", "v1", "licenses", "License", true)
	cfg := flpCfg()
	cfg.CRDTimeout = 100 * time.Millisecond
	_ = License(context.Background(), env, cfg, res)
	if !hasSev(res, "flp-rootca", SevFail) {
		t.Fatalf("empty FLP CA accepted: %+v", res.Findings)
	}
	for _, r := range s.Requests() {
		if r.Method == "PATCH" && strings.Contains(r.Path, "/licenses/") {
			t.Fatal("License applied although CWC could never verify the FLP")
		}
	}
}

func TestLicenseRealErrorNotRetried(t *testing.T) {
	s, env, res := newFake(t)
	licenseCluster(s)
	crd := obj("apiextensions.k8s.io/v1", "CustomResourceDefinition", "", "licenses.k8s.f5net.com")
	with(crd, []string{"status", "conditions"}, conditions("Established", "True"))
	s.Put("apiextensions.k8s.io", "v1", "customresourcedefinitions", crd)
	s.AddResource("k8s.f5net.com", "v1", "licenses", "License", true)
	patches := 0
	s.Hook = func(s *kubefake.Server, r kubefake.Request) *kubefake.Reply {
		if r.Method == "PATCH" && strings.Contains(r.Path, "/licenses/") {
			patches++
			rep := kubefake.Status(422, "Invalid", "spec.operationMode: Unsupported value")
			return &rep
		}
		return nil
	}
	cfg := flpCfg()
	cfg.Mode = "connected"
	_ = License(context.Background(), env, cfg, res)
	if !hasSev(res, "license-apply", SevFail) || patches != 1 {
		t.Fatalf("422 retried %d times / not failed: %+v", patches, res.Findings)
	}
}
