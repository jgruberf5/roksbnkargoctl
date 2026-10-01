package cli

import (
	"context"
	"errors"
	"os"
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
