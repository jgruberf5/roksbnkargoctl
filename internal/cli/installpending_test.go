package cli

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/go-git/go-git/v5"

	"github.com/jgruberf5/roksbnkargoctl/internal/config"
)

// The record as `install` itself writes it. A first install that fails from
// the IBM Cloud steps on (here the API key is malformed, refused locally, so
// nothing leaves this machine) records the install as pending: not a layout (a
// retry may choose another one), and not nothing (a trusted profile left
// behind would then read as an install made before 0.7.0). No cluster is
// needed: Argo CD is a fake, Git a local bare repository.
func TestInstallRecordsAFailedFirstInstallAsPending(t *testing.T) {
	home := isolate(t)
	repo := t.TempDir()
	if !strings.HasPrefix(repo, "/") {
		t.Skip("file:// URL form below assumes a Unix path")
	}
	if _, err := git.PlainInit(repo, true); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ROKSBNKARGOCTL_TEST_ARGO_TOKEN", "good")
	t.Setenv("ROKSBNKARGOCTL_TEST_GIT_TOKEN", "tok")
	t.Setenv("IBMCLOUD_API_KEY", "{malformed}") // the IAM authenticator refuses it before any request
	argo := fakeArgo(t)
	cfg := func(mode, extra string) string {
		return fmt.Sprintf(`
ibmcloud: {region: us-south}
cluster: c
transit_gateway: t
cos: {instance: ci, bucket: b}
argocd: {server: %q, insecure: true, token_env: ROKSBNKARGOCTL_TEST_ARGO_TOKEN}
git: {url: %q, branch: main, token_env: ROKSBNKARGOCTL_TEST_GIT_TOKEN}
bnk: {certificates: {mode: %s}}
resolved: {cluster_id: cid, cluster_name: c, vpc_id: v, vpc_crn: crn:vpc, transit_gateway_id: tid, transit_gateway_name: t%s}
`, argo, "file://"+repo, mode, extra)
	}
	record := func() string {
		ws, _ := config.Open("w")
		c, err := ws.LoadFile()
		if err != nil {
			t.Fatal(err)
		}
		return c.Resolved.InstalledLayout
	}
	writeWorkspace(t, home, "w", cfg("cert-manager", ""))
	if _, err := runRoot(t, "install", "-w", "w"); err == nil || !strings.Contains(err.Error(), "IBM Cloud API key") {
		t.Fatalf("install with a malformed API key: %v", err)
	}
	if got := record(); got != layoutPending+"namespaces=f5-bnk,f5-utils certificates=cert-manager" {
		t.Fatalf("after a failed first install the record is %q", got)
	}
	// Another layout may be tried: nothing of BNK was applied. The record is
	// kept across the config edit, as init would keep it.
	ws, _ := config.Open("w")
	c, _ := ws.LoadFile()
	pending := c.Resolved.InstalledLayout
	if err := os.WriteFile(ws.ConfigPath(), []byte(cfg("single", ", installed_layout: \""+pending+"\"")), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := runRoot(t, "install", "-w", "w")
	if err == nil || strings.Contains(err.Error(), "not supported") {
		t.Fatalf("a retry with another layout after a failed first install: %v", err)
	}
	if got := record(); got != layoutPending+"namespaces=f5-bnk,f5-utils certificates=single" {
		t.Errorf("the retry's record is %q", got)
	}
}

// A workspace installed before 0.7.0 (a trusted profile, no record) whose first
// 0.7.0 install fails must stay guarded: pending would erase the only sign of
// the running install and let the next one switch certificates under it, and
// recording the config's layout would guess its namespaces — a failed install
// asking for one namespace then refused the two actually running.
func TestAFailedInstallKeepsAPre070InstallGuarded(t *testing.T) {
	home := isolate(t)
	repo := t.TempDir()
	if !strings.HasPrefix(repo, "/") {
		t.Skip("file:// URL form below assumes a Unix path")
	}
	if _, err := git.PlainInit(repo, true); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ROKSBNKARGOCTL_TEST_ARGO_TOKEN", "good")
	t.Setenv("ROKSBNKARGOCTL_TEST_GIT_TOKEN", "tok")
	t.Setenv("IBMCLOUD_API_KEY", "{malformed}")
	argo := fakeArgo(t)
	cfg := func(mode string) string {
		return fmt.Sprintf(`
ibmcloud: {region: us-south}
cluster: c
transit_gateway: t
cos: {instance: ci, bucket: b}
argocd: {server: %q, insecure: true, token_env: ROKSBNKARGOCTL_TEST_ARGO_TOKEN}
git: {url: %q, branch: main, token_env: ROKSBNKARGOCTL_TEST_GIT_TOKEN}
bnk: {certificates: {mode: %s}}
resolved: {cluster_id: cid, cluster_name: c, vpc_id: v, vpc_crn: crn:vpc, transit_gateway_id: tid, transit_gateway_name: t, trusted_profile_id: Profile-1}
`, argo, "file://"+repo, mode)
	}
	writeWorkspace(t, home, "w", cfg("cert-manager"))
	if _, err := runRoot(t, "install", "-w", "w"); err == nil || strings.Contains(err.Error(), "not supported") {
		t.Fatalf("the first 0.7.0 install, with cert-manager: %v", err)
	}
	ws, _ := config.Open("w")
	c, _ := ws.LoadFile()
	c.BNK.Certificates.Mode = config.CertModeSingle
	if err := ws.Save(c); err != nil {
		t.Fatal(err)
	}
	if _, err := runRoot(t, "install", "-w", "w"); err == nil || !strings.Contains(err.Error(), "not supported") {
		t.Errorf("a switch to single under the pre-0.7.0 install, after a failed install: %v", err)
	}
	// A failed install asking for one namespace does not then refuse the two
	// that are running.
	c, _ = ws.LoadFile()
	c.BNK.Certificates.Mode = config.CertModeCertManager
	c.BNK.UtilsNamespace = c.BNK.Namespace
	_ = ws.Save(c)
	_, _ = runRoot(t, "install", "-w", "w")
	c, _ = ws.LoadFile()
	c.BNK.UtilsNamespace = "f5-utils"
	_ = ws.Save(c)
	if _, err := runRoot(t, "install", "-w", "w"); err == nil || strings.Contains(err.Error(), "not supported") {
		t.Errorf("the running layout refused after a failed install asked for another: %v", err)
	}
}
