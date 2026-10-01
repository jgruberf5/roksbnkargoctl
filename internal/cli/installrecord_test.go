package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/jgruberf5/roksbnkargoctl/internal/config"
	"github.com/jgruberf5/roksbnkargoctl/internal/ibm"
)

type fakeTGW struct {
	existing *ibm.TGWConnection
	waitErr  error
	created  []string
}

func (f *fakeTGW) FindConnectionForVPC(context.Context, string, string) (*ibm.TGWConnection, error) {
	return f.existing, nil
}
func (f *fakeTGW) WaitConnectionAttached(_ context.Context, _, id string, _ time.Duration) (*ibm.TGWConnection, error) {
	return &ibm.TGWConnection{ID: id, Status: "attached"}, f.waitErr
}
func (f *fakeTGW) ListAddressPrefixes(context.Context, string) ([]ibm.AddressPrefix, error) {
	return []ibm.AddressPrefix{{CIDR: "10.247.0.0/18"}}, nil
}
func (f *fakeTGW) GatewayAttachedPrefixes(context.Context, string) (map[string][]string, error) {
	return map[string][]string{}, nil
}
func (f *fakeTGW) CreateVPCConnection(_ context.Context, _, name, _ string) (*ibm.TGWConnection, error) {
	f.created = append(f.created, name)
	return &ibm.TGWConnection{ID: "conn-new", Status: "pending"}, nil
}

type fakeProfiles struct {
	existing *ibm.TrustedProfile
	linksErr error
	created  int
}

func (f *fakeProfiles) FindTrustedProfileByName(context.Context, string) (*ibm.TrustedProfile, error) {
	if f.existing == nil {
		return nil, &ibm.APIError{StatusCode: 404}
	}
	return f.existing, nil
}
func (f *fakeProfiles) CreateTrustedProfile(context.Context, string, string) (*ibm.TrustedProfile, error) {
	f.created++
	return &ibm.TrustedProfile{ID: "Profile-new"}, nil
}
func (f *fakeProfiles) ListProfileLinks(context.Context, string) ([]ibm.ProfileLink, error) {
	return nil, f.linksErr
}
func (f *fakeProfiles) LinkROKSServiceAccount(context.Context, string, string, string, string, string) (string, error) {
	return "link", nil
}
func (f *fakeProfiles) ListProfilePolicies(context.Context, string) ([]string, error) {
	return []string{"p1", "p2"}, nil
}
func (f *fakeProfiles) CreatePolicy(context.Context, string, []string, []ibm.PolicyAttribute) (string, error) {
	return "p", nil
}

// recordSession is a session on a resolved workspace, for install's steps.
func recordSession(t *testing.T) *session {
	t.Helper()
	home := isolate(t)
	writeWorkspace(t, home, "rec", `
cluster: c
transit_gateway: t
resolved: {cluster_id: cid, cluster_name: c, vpc_id: v, vpc_crn: crn:vpc, transit_gateway_id: tid, transit_gateway_name: t}
`)
	s, out, err := probe(t, false, nil, "-w", "rec")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	return s
}

// onDisk is the resolved record as config.yaml holds it now.
func onDisk(t *testing.T, s *session) *config.Resolved {
	t.Helper()
	c, err := s.ws.LoadFile()
	if err != nil {
		t.Fatal(err)
	}
	if c.Resolved == nil {
		return &config.Resolved{}
	}
	return c.Resolved
}

// #28: the transit gateway connection install creates is saved before the
// wait for it to attach, so a failed or timed-out wait does not lose it.
func TestInstallRecordsTheTGWConnectionBeforeWaiting(t *testing.T) {
	s := recordSession(t)
	f := &fakeTGW{waitErr: errors.New("timed out waiting for attached")}
	err := ensureClusterOnTGW(context.Background(), s, f)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("got %v, want the wait's error", err)
	}
	if len(f.created) != 1 {
		t.Fatalf("created %v", f.created)
	}
	if id := onDisk(t, s).TGWConnectionCreatedID; id != "conn-new" {
		t.Errorf("config.yaml records connection %q, want conn-new after a failed wait", id)
	}
	if onDisk(t, s).ClusterAttachedToTGW {
		t.Error("recorded as attached although the wait failed")
	}
}

// A connection the cluster VPC already had is not install's to remove.
func TestInstallDoesNotRecordAConnectionItFound(t *testing.T) {
	s := recordSession(t)
	f := &fakeTGW{existing: &ibm.TGWConnection{ID: "conn-old", Status: "attached"}}
	if err := ensureClusterOnTGW(context.Background(), s, f); err != nil {
		t.Fatal(err)
	}
	if len(f.created) != 0 || s.cfg.Resolved.TGWConnectionCreatedID != "" || onDisk(t, s).TGWConnectionCreatedID != "" {
		t.Errorf("an existing connection was created or recorded: created %v, record %q", f.created, s.cfg.Resolved.TGWConnectionCreatedID)
	}
}

// #28: the trusted profile is saved before linking and policies, so a failure
// there does not lose it.
func TestInstallRecordsTheTrustedProfileBeforeLinking(t *testing.T) {
	s := recordSession(t)
	f := &fakeProfiles{linksErr: errors.New("iam: 500")}
	if err := ensureTrustedProfile(context.Background(), s, f); err == nil {
		t.Fatal("the link error was swallowed")
	}
	if f.created != 1 {
		t.Fatalf("created %d profiles", f.created)
	}
	if id := onDisk(t, s).TrustedProfileID; id != "Profile-new" {
		t.Errorf("config.yaml records trusted profile %q, want Profile-new after a failed link", id)
	}

	// Found rather than created (the name is deterministic): recorded too,
	// since uninstall removes the profile install uses.
	s = recordSession(t)
	f = &fakeProfiles{existing: &ibm.TrustedProfile{ID: "Profile-found"}, linksErr: errors.New("iam: 500")}
	_ = ensureTrustedProfile(context.Background(), s, f)
	if f.created != 0 || onDisk(t, s).TrustedProfileID != "Profile-found" {
		t.Errorf("found profile: created %d, record %q", f.created, onDisk(t, s).TrustedProfileID)
	}
}

// When the record cannot be saved, the error names the resource install just
// created and how to remove it, since nothing else will know about it.
func TestInstallNamesTheResourceWhenItCannotRecordIt(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs a directory the test user cannot write")
	}
	s := recordSession(t)
	if err := os.Chmod(s.ws.Dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(s.ws.Dir, 0o700) })
	err := ensureClusterOnTGW(context.Background(), s, &fakeTGW{})
	if err == nil || !strings.Contains(err.Error(), "created transit gateway connection conn-new but could not record it") ||
		!strings.Contains(err.Error(), "ibmcloud tg connection-delete tid conn-new") {
		t.Errorf("TGW: %v", err)
	}
	err = ensureTrustedProfile(context.Background(), s, &fakeProfiles{})
	if err == nil || !strings.Contains(err.Error(), "trusted profile Profile-new exists but could not be recorded") ||
		!strings.Contains(err.Error(), "ibmcloud iam trusted-profile-delete Profile-new") {
		t.Errorf("profile: %v", err)
	}
}

// install refuses to switch an installed BNK between one and two namespaces,
// or between certificate modes; a fresh workspace (nothing installed) and the
// same layout pass. init --refresh keeps the record.
func TestInstallRefusesAnotherLayoutUnderAnInstall(t *testing.T) {
	c := &config.Config{BNK: config.BNK{Namespace: "f5-bnk", UtilsNamespace: "f5-utils", Certificates: config.Certificates{Mode: config.CertModeCertManager}}}
	installed := installLayout(c)
	if installed != "namespaces=f5-bnk,f5-utils certificates=cert-manager" {
		t.Fatalf("layout %q", installed)
	}
	if err := checkLayout(&config.Resolved{}, c); err != nil {
		t.Errorf("nothing installed yet: %v", err)
	}
	if err := checkLayout(&config.Resolved{InstalledLayout: installed}, c); err != nil {
		t.Errorf("same layout: %v", err)
	}
	one := *c
	one.BNK.UtilsNamespace = "f5-bnk"
	if err := checkLayout(&config.Resolved{InstalledLayout: installed}, &one); err == nil || !strings.Contains(err.Error(), "uninstall") ||
		!strings.Contains(err.Error(), "namespaces=f5-bnk certificates=cert-manager") {
		t.Errorf("one namespace under a two-namespace install: %v", err)
	}
	single := *c
	single.BNK.Certificates.Mode = config.CertModeSingle
	if err := checkLayout(&config.Resolved{InstalledLayout: installed}, &single); err == nil {
		t.Error("certificate mode switched under an install")
	}
	if carriedOver(&config.Resolved{InstalledLayout: installed}).InstalledLayout != installed {
		t.Error("init --refresh dropped the installed layout")
	}
}

// init --config-file on an installed workspace keeps what install recorded
// (the trusted profile, the TGW connection, the installed layout): it used to
// drop the workspace's record before the re-resolve, so uninstall, workspaces
// delete and the layout guard forgot them. A resolved section in the file
// itself is still ignored.
func TestInitFromAFileKeepsTheWorkspaceRecord(t *testing.T) {
	home := isolate(t)
	cfgYAML := `
ibmcloud: {region: us-south}
cluster: c
transit_gateway: t
cos: {instance: ci, bucket: b}
argocd: {server: "https://a.example"}
git: {url: "https://git.example/r.git"}
`
	writeWorkspace(t, home, "w", cfgYAML+`resolved: {cluster_id: cid, cluster_name: c, transit_gateway_id: tid, transit_gateway_name: t,
  trusted_profile_id: Profile-1, tgw_connection_created_id: conn-9, installed_layout: "namespaces=f5-bnk certificates=single"}
`)
	src := filepath.Join(t.TempDir(), "in.yaml")
	if err := os.WriteFile(src, []byte(cfgYAML+"resolved: {trusted_profile_id: Profile-FROM-FILE}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	resolveWorkspace = func(_ context.Context, s *session) error {
		r := carriedOver(s.cfg.Resolved) // what resolve does with the old record
		r.ClusterID, r.ClusterName, r.TransitGatewayID, r.TransitGatewayName = "cid", "c", "tid", "t"
		s.cfg.Resolved = r
		return nil
	}
	initRemoteChecks = func(context.Context, *session) error { return nil }
	t.Cleanup(func() { resolveWorkspace, initRemoteChecks = resolve, defaultInitRemoteChecks })
	if out, err := runRoot(t, "init", "-w", "w", "-f", src); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	ws, _ := config.Open("w")
	c, err := ws.LoadFile()
	if err != nil {
		t.Fatal(err)
	}
	r := c.Resolved
	if r.TrustedProfileID != "Profile-1" || r.TGWConnectionCreatedID != "conn-9" || r.InstalledLayout != "namespaces=f5-bnk certificates=single" {
		t.Errorf("init -f lost the install record: %+v", r)
	}
}

// init (with a file or --refresh after an edit) naming another cluster must not
// carry one cluster's install record onto the other: it refuses while an
// install is recorded, and starts afresh when none is.
func TestInitRefusesToMoveAnInstallRecordToAnotherCluster(t *testing.T) {
	home := isolate(t)
	cfgYAML := `
ibmcloud: {region: us-south}
cluster: b
transit_gateway: t
cos: {instance: ci, bucket: b}
argocd: {server: "https://a.example"}
git: {url: "https://git.example/r.git"}
`
	resolveWorkspace = func(_ context.Context, s *session) error {
		r := carriedOver(s.cfg.Resolved)
		r.ClusterID, r.ClusterName, r.TransitGatewayID, r.TransitGatewayName = "cid-b", "b", "tid", "t"
		s.cfg.Resolved = r
		return nil
	}
	initRemoteChecks = func(context.Context, *session) error { return nil }
	t.Cleanup(func() { resolveWorkspace, initRemoteChecks = resolve, defaultInitRemoteChecks })

	writeWorkspace(t, home, "w", cfgYAML+`resolved: {cluster_id: cid-a, cluster_name: a, tgw_connection_created_id: conn-a,
  installed_layout: "namespaces=f5-bnk,f5-utils certificates=cert-manager"}
`)
	out, err := runRoot(t, "init", "-w", "w", "--refresh")
	if err == nil || !strings.Contains(err.Error(), "records what install created for cluster a") ||
		!strings.Contains(err.Error(), "uninstall --detach-tgw") || !strings.Contains(err.Error(), "BNK (namespaces=f5-bnk,f5-utils") {
		t.Fatalf("moved an install record to another cluster: %v\n%s", err, out)
	}
	ws, _ := config.Open("w")
	if c, _ := ws.LoadFile(); c.Resolved.TGWConnectionCreatedID != "conn-a" || c.Resolved.ClusterID != "cid-a" {
		t.Errorf("the refused re-resolve changed the record: %+v", c.Resolved)
	}

	writeWorkspace(t, home, "w", cfgYAML+`resolved: {cluster_id: cid-a, cluster_name: a, trusted_profile_id: Profile-a, installed_layout: none}
`)
	if _, err := runRoot(t, "init", "-w", "w", "--refresh"); err == nil || !strings.Contains(err.Error(), "without --keep-trusted-profile") ||
		strings.Contains(err.Error(), "BNK (") {
		t.Errorf("uninstalled, profile kept: %v", err)
	}
	// A record without a cluster ID (never resolved) is not another cluster's.
	writeWorkspace(t, home, "w", cfgYAML+`resolved: {trusted_profile_id: Profile-x}
`)
	if out, err := runRoot(t, "init", "-w", "w", "--refresh"); err != nil {
		t.Errorf("a record without a cluster: %v\n%s", err, out)
	}

	writeWorkspace(t, home, "w", cfgYAML+`resolved: {cluster_id: cid-a, cluster_name: a, last_published_commit: abc}
`)
	if out, err := runRoot(t, "init", "-w", "w", "--refresh"); err != nil {
		t.Fatalf("nothing installed, yet refused: %v\n%s", err, out)
	}
	if c, _ := ws.LoadFile(); c.Resolved.ClusterID != "cid-b" || c.Resolved.LastPublishedCommitSHA != "" {
		t.Errorf("not started afresh for the new cluster: %+v", c.Resolved)
	}
}

// The guard as `install` runs it: refused before anything is touched (no IBM
// Cloud, Argo CD or cluster call is reachable from this test), for a recorded
// layout and for a workspace installed before 0.7.0 switching to single
// certificates. A workspace uninstalled (none) or never installed passes the
// guard (and then fails later, on the missing API key, which is the point).
func TestInstallCommandRefusesAnotherLayout(t *testing.T) {
	home := isolate(t)
	t.Setenv("IBMCLOUD_API_KEY", "")
	cfgYAML := `
ibmcloud: {region: us-south}
cluster: c
transit_gateway: t
cos: {instance: ci, bucket: b}
argocd: {server: "https://a.example"}
git: {url: "https://git.example/r.git"}
`
	rec := `resolved: {cluster_id: cid, cluster_name: c, vpc_id: v, vpc_crn: crn:vpc, transit_gateway_id: tid, transit_gateway_name: t`
	for name, tc := range map[string]struct {
		extra, record string
		refused       bool
	}{
		"other layout":        {"", `, installed_layout: "namespaces=f5-bnk certificates=single"}`, true},
		"pre-0.7.0 to single": {"bnk: {certificates: {mode: single}}\n", `, trusted_profile_id: Profile-1, last_published_commit: abc}`, true},
		// --no-publish installs never recorded a commit; still caught.
		"pre-0.7.0 no-publish": {"bnk: {certificates: {mode: single}}\n", `, trusted_profile_id: Profile-1}`, true},
		"pre-0.7.0 unchanged":  {"", `, trusted_profile_id: Profile-1, last_published_commit: abc}`, false},
		// A first 0.7.0 install that failed after creating the profile: pending
		// locks nothing, so another layout may be tried.
		"failed first install": {"bnk: {certificates: {mode: single}}\n", `, trusted_profile_id: Profile-1, installed_layout: "pending: namespaces=f5-bnk,f5-utils certificates=cert-manager"}`, false},
		"uninstalled":          {"bnk: {certificates: {mode: single}}\n", `, trusted_profile_id: Profile-1, installed_layout: none}`, false},
		"never installed":      {"bnk: {certificates: {mode: single}}\n", `}`, false},
		"same layout recorded": {"", `, installed_layout: "namespaces=f5-bnk,f5-utils certificates=cert-manager"}`, false},
	} {
		writeWorkspace(t, home, "w", cfgYAML+tc.extra+rec+tc.record+"\n")
		_, err := runRoot(t, "install", "-w", "w")
		refused := err != nil && strings.Contains(err.Error(), "under a running install is not supported")
		if refused != tc.refused {
			t.Errorf("%s: refused=%v, want %v (%v)", name, refused, tc.refused, err)
		}
	}
}
