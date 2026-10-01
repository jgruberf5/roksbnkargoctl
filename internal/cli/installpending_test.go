package cli

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/go-git/go-git/v5"

	"github.com/jgruberf5/roksbnkargoctl/internal/config"
)

// The record as `install` itself writes it. A first install that fails after
// its first cloud change (here IBM Cloud is unreachable, at the transit
// gateway step) records the install as pending: not a layout (a retry may
// choose another one), and not nothing (a trusted profile left behind would
// then read as an install made before 0.7.0). No cluster is needed: Argo CD
// is a fake, Git a local bare repository, IBM Cloud a dead proxy.
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
	t.Setenv("IBMCLOUD_API_KEY", "not-a-real-key")
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1") // IBM Cloud unreachable; the local fakes are not proxied
	t.Setenv("NO_PROXY", "")
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
	if _, err := runRoot(t, "install", "-w", "w"); err == nil {
		t.Fatal("install succeeded with IBM Cloud unreachable")
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
