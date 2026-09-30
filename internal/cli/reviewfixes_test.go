package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/jgruberf5/roksbnkargoctl/internal/argocd"
	"github.com/jgruberf5/roksbnkargoctl/internal/config"
	"github.com/jgruberf5/roksbnkargoctl/internal/gitpub"
	"github.com/jgruberf5/roksbnkargoctl/internal/ibm"
	"github.com/jgruberf5/roksbnkargoctl/internal/vsi"
)

// Findings of the PR #16 review, each pinned against the path the command takes.

// init under ROKSBNKARGOCTL_CLUSTER records the override's cluster while
// config.yaml keeps its own. A later command without the override must refuse
// the record: it would otherwise act on two clusters at once (the record's VPC
// and endpoints, config.yaml's cluster for the admin kubeconfig).
func TestAResolveUnderAnOverrideIsRefusedWithoutIt(t *testing.T) {
	isolate(t)
	src := filepath.Join(t.TempDir(), "in.yaml")
	os.WriteFile(src, []byte(`
ibmcloud: {region: us-south}
cluster: file-cluster
transit_gateway: tgw
cos: {instance: ci, bucket: b}
argocd: {server: "https://file.example"}
git: {url: "https://git.example/r.git"}
`), 0o600)
	resolveWorkspace = func(_ context.Context, s *session) error {
		s.cfg.Resolved = &config.Resolved{ClusterID: "id-env", ClusterName: s.cfg.Cluster, TransitGatewayID: "tg-1", TransitGatewayName: "tgw"}
		return nil
	}
	initRemoteChecks = func(context.Context, *session) error { return nil }
	t.Cleanup(func() { resolveWorkspace, initRemoteChecks = resolve, defaultInitRemoteChecks })
	t.Setenv("ROKSBNKARGOCTL_CLUSTER", "env-cluster")
	root := newRoot()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"init", "-w", "ov", "-f", src})
	if err := root.Execute(); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	flagWorkspace = ""
	t.Setenv("ROKSBNKARGOCTL_CLUSTER", "") // the next command, without the override
	s, sout, err := probe(t, false, nil, "-w", "ov")
	if err != nil {
		t.Fatalf("%v\n%s", err, sout)
	}
	r, err := s.resolved()
	if err == nil || !strings.Contains(err.Error(), `config.yaml sets cluster to "file-cluster"`) {
		t.Errorf("resolved() = %+v, %v; want a refusal naming config.yaml's cluster", r, err)
	}
}

// With a workspace, a --region other than the one it was resolved in is
// refused, as it is without one: Forge would be told the wrong region.
func TestForgeRefusesARegionTheWorkspaceWasNotResolvedIn(t *testing.T) {
	e := setupForge(t)
	writeWorkspace(t, e.home, "w", forgeWorkspaceYAML)
	out, err := runRoot(t, "-w", "w", "forge", "register", "--region", "eu-de")
	if err == nil || !strings.Contains(err.Error(), "Forge would be told the wrong region") {
		t.Errorf("--region eu-de for a workspace resolved in us-south: %v\n%s", err, out)
	}
	if len(e.forge.templates) != 0 {
		t.Errorf("a template was created: %v", e.forge.templates)
	}
}

// An http:// Forge URL is warned about: the password and the IBM Cloud API
// key go to it unencrypted.
func TestForgeWarnsAboutPlainHTTP(t *testing.T) {
	e := setupForge(t)
	out, err := runRoot(t, noWS...)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "is plain http") || !strings.Contains(out, "unencrypted") {
		t.Errorf("no warning for %s; output:\n%s", e.srv.URL, out)
	}
}

// `flp down --name other` with a current workspace used to ignore --name and
// tear down the workspace's license proxy. A --name the state does not record
// is refused, and nothing is deleted.
func TestFLPDownRefusesANameTheStateDoesNotRecord(t *testing.T) {
	home := isolate(t)
	cloud, _, _, _ := flpFake(t)
	writeWorkspace(t, home, "p", `
cluster: c
transit_gateway: mytgw
ibmcloud: {region: us-east}
cos: {local_far_auth_file: far.tgz, local_jwt_file: sub.jwt}
flp: {vsi: {cidr: 10.1.0.0/28}}
`)
	if _, e, err := runFLP(t, "flp", "up", "-w", "p"); err != nil {
		t.Fatalf("%v\n%s", err, e)
	}
	if err := setCurrent("p"); err != nil {
		t.Fatal(err)
	}
	built := cloud.Names()
	_, e, err := runFLP(t, "flp", "down", "--name", "other", "--region", "us-east", "--yes")
	if err == nil || !strings.Contains(err.Error(), "--name other is not the license proxy recorded") {
		t.Errorf("flp down --name other: %v\n%s", err, e)
	}
	if len(cloud.Names()) < len(built) {
		t.Errorf("`flp down --name other` deleted workspace p's license proxy (p-flp); before %d resources, after %d", len(built), len(cloud.Names()))
	}
}

// The POSIX export lines are single-quoted, so a path with a space or a quote
// pastes into a shell intact.
func TestPrintFLPExternalQuotesPOSIX(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PowerShell quoting is covered by TestFLPWithoutAWorkspace")
	}
	var b bytes.Buffer
	printFLPExternal(&b, "https://10.0.0.1:8443", "/home/u/it's my dir/x-flp-ca.pem")
	if want := `export ROKSBNKARGOCTL_FLP_EXTERNAL_ROOT_CA_FILE='/home/u/it'\''s my dir/x-flp-ca.pem'`; !strings.Contains(b.String(), want) {
		t.Errorf("got\n%s\nwant a line %s", b.String(), want)
	}
}

// With --no-publish and no local credential, install leaves Argo CD's
// registration alone (an upsert would replace a credentialed one with an
// anonymous one); with a credential it registers it.
func TestRegisterRepo(t *testing.T) {
	c := &config.Config{Git: config.Git{URL: "https://g/r.git", Username: "git"}, ArgoCD: config.ArgoCD{Project: "bnk"}}
	var got []argocd.Repo
	upsert := func(r argocd.Repo) error { got = append(got, r); return nil }
	s, _ := initSession(c)
	if err := registerRepo(s.p, c, gitpub.Options{}, true, upsert); err != nil || len(got) != 0 {
		t.Fatalf("--no-publish, no credential: upserted %+v (err %v)", got, err)
	}
	if err := registerRepo(s.p, c, gitpub.Options{Token: "tok"}, true, upsert); err != nil || len(got) != 1 || got[0].Password != "tok" || got[0].Project != "bnk" {
		t.Fatalf("--no-publish with a token: %+v (err %v)", got, err)
	}
	if err := registerRepo(s.p, c, gitpub.Options{}, false, func(argocd.Repo) error { return errors.New("boom") }); err == nil || !strings.Contains(err.Error(), "adding the Git repo to Argo CD: boom") {
		t.Errorf("a failed upsert: %v", err)
	}
}

// --no-publish syncs exactly the revision it compared, not the branch head.
func TestRevisionToSync(t *testing.T) {
	compared := func() (string, error) { return "abc123", nil }
	if rev, err := revisionToSync(true, "", compared); err != nil || rev != "abc123" {
		t.Errorf("--no-publish: %q %v, want the compared revision", rev, err)
	}
	if rev, _ := revisionToSync(false, "pushed1", func() (string, error) { t.Fatal("compared without --no-publish"); return "", nil }); rev != "pushed1" {
		t.Errorf("publish: %q, want the pushed commit", rev)
	}
	if _, err := revisionToSync(true, "", func() (string, error) { return "", errors.New("differs") }); err == nil {
		t.Error("a failed comparison was not an error")
	}
}

// A license proxy in the cluster's own VPC must not record the gateway
// connection: Argo CD reaches the cluster through it, and flp down would
// detach it. In any other adopted VPC the connection stays recorded.
func TestSpareClusterAttachment(t *testing.T) {
	c := &config.Config{Resolved: &config.Resolved{VPCID: "vpc-cluster"}}
	s, out := initSession(c)
	rec := &vsi.Record{VPCID: "vpc-cluster", TGWConnectionID: "conn-1"}
	spareClusterAttachment(s, rec)
	if rec.TGWConnectionID != "" || !strings.Contains(out.String(), "left attached") {
		t.Errorf("cluster VPC: record %+v, output %q", rec, out.String())
	}
	other := &vsi.Record{VPCID: "vpc-other", TGWConnectionID: "conn-2"}
	spareClusterAttachment(s, other)
	if other.TGWConnectionID != "conn-2" {
		t.Errorf("another VPC lost its connection: %+v", other)
	}
	created := &vsi.Record{VPCID: "vpc-cluster", VPCCreated: true, TGWConnectionID: "conn-3"}
	spareClusterAttachment(s, created)
	if created.TGWConnectionID != "conn-3" {
		t.Errorf("a created VPC lost its connection: %+v", created)
	}
}

// Without any override, a config.yaml whose gateway or cluster endpoint is not
// what the workspace was resolved for is refused too.
func TestResolvedComparesTheFileWithTheRecord(t *testing.T) {
	const rec = `
resolved:
  cluster_id: cid
  cluster_name: c
  private_endpoint: https://priv
  public_endpoint: https://pub
  argocd_cluster_server: https://priv
  transit_gateway_id: tid
  transit_gateway_name: tgw
`
	for _, tc := range []struct{ name, file, want string }{
		{"gw", "cluster: c\ntransit_gateway: other\nargocd: {cluster_endpoint: private}\n", `config.yaml sets transit_gateway to "other"`},
		{"ep", "cluster: c\ntransit_gateway: tgw\nargocd: {cluster_endpoint: public}\n", `config.yaml sets argocd.cluster_endpoint to "public"`},
		{"ok", "cluster: c\ntransit_gateway: tid\nargocd: {cluster_endpoint: private}\n", ""},
	} {
		home := isolate(t)
		writeWorkspace(t, home, tc.name, tc.file+rec)
		s, out, err := probe(t, false, nil, "-w", tc.name)
		if err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		_, err = s.resolved()
		if tc.want == "" && err != nil || tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
			t.Errorf("%s: resolved() err=%v, want %q", tc.name, err, tc.want)
		}
	}
}

// flp up in the cluster's own VPC leaves the gateway connection out of its
// state, and flp down leaves it attached: Argo CD reaches the cluster over it.
func TestFLPInTheClusterVPCLeavesItsConnection(t *testing.T) {
	home := isolate(t)
	cloud, _, _, _ := flpFake(t)
	v := cloud.AddVPC("cluster-vpc")
	writeWorkspace(t, home, "p", `
cluster: c
transit_gateway: mytgw
ibmcloud: {region: us-east}
cos: {local_far_auth_file: far.tgz, local_jwt_file: sub.jwt}
flp: {vsi: {cidr: 10.1.0.0/28, vpc: cluster-vpc}}
resolved: {cluster_id: cid, vpc_id: `+v.ID+`}
`)
	if _, e, err := runFLP(t, "flp", "up", "-w", "p"); err != nil {
		t.Fatalf("%v\n%s", err, e)
	} else if !strings.Contains(e, "left attached") {
		t.Errorf("flp up did not say the connection is left attached:\n%s", e)
	}
	b, err := os.ReadFile(filepath.Join(home, "p", "flp-outputs.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "tgw_connection_id") {
		t.Errorf("state records the cluster VPC's connection:\n%s", b)
	}
	// A state written before this fix recorded it; down must still spare it.
	st := strings.Replace(string(b), `"tgw_id"`, `"tgw_connection_id":"`+connID(t, cloud)+`","tgw_id"`, 1)
	if err := os.WriteFile(filepath.Join(home, "p", "flp-outputs.json"), []byte(st), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, e, err := runFLP(t, "flp", "down", "-w", "p", "--yes"); err != nil {
		t.Fatalf("%v\n%s", err, e)
	}
	if n, want := cloud.Names(), "conn:tgw-id-mytgw:p-flp"; !strings.Contains(strings.Join(n, " "), want) {
		t.Errorf("flp down detached the cluster VPC: left %v, want %s among them", n, want)
	}
}

func connID(t *testing.T, c interface {
	ListConnections(context.Context, string) ([]ibm.TGWConnection, error)
}) string {
	t.Helper()
	cs, err := c.ListConnections(context.Background(), "tgw-id-mytgw")
	if err != nil || len(cs) != 1 {
		t.Fatalf("connections %v %v", cs, err)
	}
	return cs[0].ID
}
