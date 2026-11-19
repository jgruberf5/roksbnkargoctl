package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jgruberf5/roksbnkargoctl/internal/argocd"
	"github.com/jgruberf5/roksbnkargoctl/internal/config"
)

// Every running/target combination install and upgrade can meet. An install
// that would change the running release is refused and pointed at upgrade:
// run 93 rolled 2.4.1 to 2.4.2 through install, unasked.
func TestCheckRelease(t *testing.T) {
	for _, tc := range []struct {
		running, target string
		upgrade         bool
		wantErr         string // "" = allowed
	}{
		{"", "2.4.3", false, ""},
		{"", "2.4.3", true, "not installed"},
		{"2.4.3", "2.4.3", false, ""}, // a re-sync
		{"2.4.3", "2.4.3", true, "already running"},
		{"2.4.2", "2.4.3", false, "run `roksbnkargoctl upgrade`"},
		{"2.4.2", "2.4.3", true, ""},
		{"2.4.0", "2.4.3", true, ""},
		{"2.4.3", "2.4.2", false, "downgrades are not supported"},
		{"2.4.3", "2.4.2", true, "downgrades are not supported"},
		{"2.3.4", "2.4.3", true, "between BNK lines"},
		{"2.5.0", "2.4.3", false, "between BNK lines"},
		{"2.4", "2.4.3", true, "not a BNK release"},
		{"2.4.x", "2.4.3", false, "not a BNK release"},
	} {
		err := checkRelease(tc.running, tc.target, tc.upgrade)
		switch {
		case tc.wantErr == "" && err != nil:
			t.Errorf("running %q target %q upgrade=%v: want allowed, got %v", tc.running, tc.target, tc.upgrade, err)
		case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
			t.Errorf("running %q target %q upgrade=%v: want error containing %q, got %v", tc.running, tc.target, tc.upgrade, tc.wantErr, err)
		}
	}
}

// cneAt is a CNEInstance in the shape FLO 2.30 wrote it during run 93.
func cneAt(version string, conds map[string]string) map[string]any {
	var cs []any
	for _, t := range []string{"Accepted", "Reconciled", "Available", "CRDInstallerAvailable", "CNEControllerAvailable",
		"F5TmmAvailable", "AfmAvailable", "FluentdAvailable", "RabbitmqAvailable", "CRDConversionAvailable",
		"CwcAvailable", "IPAMControllerAvailable"} {
		st, ok := conds[t]
		if !ok {
			st = "True"
		}
		if st == "-" {
			continue // not reported
		}
		cs = append(cs, map[string]any{"type": t, "status": st})
	}
	return map[string]any{"spec": map[string]any{"manifestVersion": version}, "status": map[string]any{"conditions": cs}}
}

func licenseIn(state string) map[string]any {
	return map[string]any{"status": map[string]any{"state": state}}
}

// The states run 93 passed through, in order, then the ways it can still be
// unfinished.
func TestUpgradeDone(t *testing.T) {
	for _, tc := range []struct {
		name string
		cne  map[string]any
		lic  map[string]any
		want bool
		says string
	}{
		{"old release still asked for", cneAt("2.4.1", nil), licenseIn("Active"), false, `manifestVersion "2.4.1"`},
		{"14:42:45 rolling, CRD installer pending", cneAt("2.4.2", map[string]string{"Available": "False", "CRDInstallerAvailable": "False"}), licenseIn("Active"), false, "CRDInstallerAvailable"},
		{"14:43:20 TMM and CWC pending", cneAt("2.4.2", map[string]string{"Available": "False", "F5TmmAvailable": "False", "CwcAvailable": "False"}), licenseIn("Active"), false, "F5TmmAvailable"},
		{"14:45:05 all Available", cneAt("2.4.2", nil), licenseIn("Active"), true, "License Active"},
		{"Available not reported yet", cneAt("2.4.2", map[string]string{"Available": "-"}), licenseIn("Active"), false, "Available (not reported yet)"},
		{"a non-Available condition does not hold it", cneAt("2.4.2", map[string]string{"Reconciled": "False"}), licenseIn("Active"), true, ""},
		{"License not Active", cneAt("2.4.2", nil), licenseIn("RegistrationFailed"), false, "RegistrationFailed"},
		{"no License", cneAt("2.4.2", nil), nil, false, "no License"},
		{"no CNEInstance", nil, licenseIn("Active"), false, "no CNEInstance"},
	} {
		got, says := upgradeDone(tc.cne, tc.lic, "2.4.2")
		if got != tc.want || !strings.Contains(says, tc.says) {
			t.Errorf("%s: got %v %q, want %v containing %q", tc.name, got, says, tc.want, tc.says)
		}
	}
}

// fakeSyncArgo records sync request bodies and answers the Application as a
// finished, successful sync.
func fakeSyncArgo(t *testing.T) (*argocd.Client, func() []map[string]any) {
	var mu sync.Mutex
	var bodies []map[string]any
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/sync") {
			b, _ := io.ReadAll(r.Body)
			var m map[string]any
			_ = json.Unmarshal(b, &m)
			mu.Lock()
			bodies = append(bodies, m)
			mu.Unlock()
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"metadata": map[string]any{"name": "bnk-demo"}, "spec": map[string]any{},
			"status": map[string]any{"sync": map[string]any{"status": "Synced"}, "health": map[string]any{"status": "Healthy"},
				"operationState": map[string]any{"phase": "Succeeded", "message": "successfully synced"}},
		})
	}))
	t.Cleanup(srv.Close)
	return argocd.New(srv.URL, "tok", true, nil), func() []map[string]any { mu.Lock(); defer mu.Unlock(); return bodies }
}

// An upgrade's sync prunes (the old CNEManifest bnk-<old> goes; run 93 left it
// and the Application OutOfSync without); an install's does not.
func TestSyncPrunesOnlyForUpgrade(t *testing.T) {
	for _, upgrade := range []bool{false, true} {
		ac, bodies := fakeSyncArgo(t)
		var out bytes.Buffer
		s := &session{cfg: &config.Config{ArgoCD: config.ArgoCD{Application: "bnk-demo"}}, p: printer{&out}}
		if err := syncAndWait(context.Background(), s, ac, "abc123", 1, upgrade, time.Minute); err != nil {
			t.Fatalf("upgrade=%v: %v\n%s", upgrade, err, out.String())
		}
		b := bodies()
		if len(b) != 1 {
			t.Fatalf("upgrade=%v: %d sync requests", upgrade, len(b))
		}
		if b[0]["prune"] != upgrade {
			t.Errorf("upgrade=%v: sync sent prune=%v", upgrade, b[0]["prune"])
		}
	}
}

// runInstall needs a cluster, so this pins its order from the source: the
// release check comes before the first change it makes in the cloud or Git
// (the transit gateway, the trusted profile, the out-of-band objects, the
// push, the Application). Moved after them, a refused upgrade would already
// have changed things. The pending install record is written before it on
// purpose: it is local, locks nothing, and must survive a first install that
// cannot reach the cluster (TestInstallRecordsAFailedFirstInstallAsPending).
func TestInstallChecksTheReleaseBeforeChangingAnything(t *testing.T) {
	b, err := os.ReadFile("install.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	start := strings.Index(src, "func runInstall(")
	if start < 0 {
		t.Fatal("no runInstall in install.go")
	}
	body := src[start:]
	check := strings.Index(body, "checkRelease(running, c.BNK.Version, opt.upgrade)")
	if check < 0 {
		t.Fatal("runInstall does not call checkRelease with the running release")
	}
	for _, change := range []string{"ensureClusterOnTGW(", "ensureTrustedProfile(", "k.Apply(", "gitpub.Publish(", "ac.UpsertApplication("} {
		i := strings.Index(body, change)
		if i < 0 {
			t.Fatalf("runInstall no longer calls %s: update this test", change)
		}
		if i < check {
			t.Errorf("runInstall calls %s before checkRelease", change)
		}
	}
}
