package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/jgruberf5/roksbnkargoctl/internal/config"
	"github.com/jgruberf5/roksbnkargoctl/internal/ibm"
)

// isolate points the tool at an empty home and clears every variable that
// could select a workspace or override a setting from the developer's shell.
func isolate(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("ROKSBNKARGOCTL_HOME", home)
	t.Setenv("ROKSBNKARGOCTL_WORKSPACE", "")
	for _, k := range config.Keys() {
		t.Setenv(k.Env, "")
	}
	t.Cleanup(func() { flagWorkspace, flagNoWorkspace = "", false })
	return home
}

func writeWorkspace(t *testing.T, home, name, yaml string) string {
	t.Helper()
	dir := filepath.Join(home, name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(p, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func readSaved(t *testing.T, path string) *config.Config {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	c, err := config.Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// probe runs `roksbnkargoctl probe <args>` through the real root command (so
// the global -w / --no-workspace flags and flag parsing are the product's),
// with bind registering config flags on probe, and returns the session probe
// opened.
func probe(t *testing.T, optional bool, bind func(*cobra.Command), args ...string) (*session, string, error) {
	t.Helper()
	var s *session
	cmd := &cobra.Command{Use: "probe", RunE: func(cmd *cobra.Command, _ []string) error {
		var err error
		if optional {
			s, err = newSessionOptional(cmd)
		} else {
			s, err = newSession(cmd)
		}
		return err
	}}
	if bind != nil {
		bind(cmd)
	}
	root := newRoot()
	root.AddCommand(cmd)
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(append([]string{"probe"}, args...))
	err := root.Execute()
	return s, out.String(), err
}

// ---- flags -------------------------------------------------------------------

func TestBindConfigFlagsNamesEveryKeyInTheSection(t *testing.T) {
	cmd := &cobra.Command{Use: "x"}
	got := strings.Join(bindConfigFlags(cmd, "flp.vsi", ""), " ")
	if want := "zone vpc cidr profile allowed-cidrs ssh-key floating-ip"; got != want {
		t.Fatalf("flags %q, want %q", got, want)
	}
	f := cmd.Flags().Lookup("allowed-cidrs")
	for _, want := range []string{"CIDRs allowed to reach the proxy", "flp.vsi.allowed_cidrs", "ROKSBNKARGOCTL_FLP_VSI_ALLOWED_CIDRS"} {
		if !strings.Contains(f.Usage, want) {
			t.Errorf("help %q lacks %q", f.Usage, want)
		}
	}
	// A prefix, a skipped key, and an extra key from another section.
	cmd = &cobra.Command{Use: "y"}
	got = strings.Join(bindConfigFlags(cmd, "flp", "flp-", "vsi.ssh_key"), " ")
	if want := "flp-vsi-zone flp-vsi-vpc flp-vsi-cidr flp-vsi-profile flp-vsi-allowed-cidrs flp-vsi-floating-ip flp-external-url flp-external-root-ca-file"; got != want {
		t.Fatalf("flags %q, want %q", got, want)
	}
	bindConfigFlag(cmd, "region", "ibmcloud.region")
	bindConfigFlag(cmd, "ssh-key", "flp.vsi.ssh_key")
	if cmd.Flags().Lookup("region") == nil || cmd.Flags().Lookup("ssh-key") == nil {
		t.Fatal("extra flags not registered")
	}
}

// Precedence, through a real command: flag > env > config.yaml > defaults, and
// a flag the command line did not set never replaces anything.
func TestConfigPrecedenceFlagEnvFileDefault(t *testing.T) {
	home := isolate(t)
	writeWorkspace(t, home, "p", `
cluster: file-cluster
flp:
  vsi:
    cidr: 10.1.0.0/28
    zone: file-zone
    vpc: file-vpc
    ssh_key: file-key
`)
	t.Setenv("ROKSBNKARGOCTL_FLP_VSI_CIDR", "10.2.0.0/28")
	t.Setenv("ROKSBNKARGOCTL_FLP_VSI_ZONE", "env-zone")
	t.Setenv("ROKSBNKARGOCTL_CLUSTER", "env-cluster")
	bind := func(c *cobra.Command) { bindConfigFlags(c, "flp.vsi", "") }
	s, out, err := probe(t, false, bind, "-w", "p", "--cidr", "10.3.0.0/28", "--allowed-cidrs", " 10.0.0.0/8, ,172.16.0.0/12", "--floating-ip=false")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	v := s.cfg.FLP.VSI
	checks := []struct{ what, got, want string }{
		{"flag over env", v.CIDR, "10.3.0.0/28"},
		{"env over file", v.Zone, "env-zone"},
		{"env over file (top-level key)", s.cfg.Cluster, "env-cluster"},
		{"file over default", v.VPC, "file-vpc"},
		{"unset flag keeps the file", v.SSHKey, "file-key"},
		{"default when nothing sets it", v.Profile, "bx2-4x16"},
		{"list flag trimmed, empties dropped", strings.Join(v.AllowedCIDRs, ","), "10.0.0.0/8,172.16.0.0/12"},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s: got %q, want %q", c.what, c.got, c.want)
		}
	}
	if v.FloatingIP == nil || *v.FloatingIP {
		t.Errorf("--floating-ip=false: got %v", v.FloatingIP)
	}
	// The file copy is untouched by all of it.
	if f := s.file.FLP.VSI; f.CIDR != "10.1.0.0/28" || f.Zone != "file-zone" || s.file.Cluster != "file-cluster" || f.FloatingIP != nil {
		t.Errorf("overrides leaked into the file copy: %+v cluster=%s", f, s.file.Cluster)
	}
}

// A default that depends on another key sees the override: defaults are the
// lowest precedence, so they are filled after the overrides, not before.
func TestDefaultsApplyAfterOverrides(t *testing.T) {
	home := isolate(t)
	writeWorkspace(t, home, "d", "cluster: c\n")
	t.Setenv("ROKSBNKARGOCTL_COS_LOCAL_FAR_AUTH_FILE", "/tmp/far.tgz")
	t.Setenv("ROKSBNKARGOCTL_GIT_SSH_KEY_FILE", "/tmp/id")
	s, out, err := probe(t, false, nil, "-w", "d")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if s.cfg.COS.Instance != "" || s.cfg.Git.TokenEnv != "" {
		t.Errorf("defaults ignored the overrides they depend on: cos.instance=%q git.token_env=%q", s.cfg.COS.Instance, s.cfg.Git.TokenEnv)
	}
	if s.file.COS.Instance != "bnk-supply-chain" {
		t.Errorf("the file copy keeps its own defaults, got cos.instance=%q", s.file.COS.Instance)
	}
}

func TestMalformedOverrideFailsTheCommand(t *testing.T) {
	home := isolate(t)
	writeWorkspace(t, home, "m", "cluster: c\n")
	t.Setenv("ROKSBNKARGOCTL_BNK_TMM_REPLICAS", "lots")
	_, _, err := probe(t, false, nil, "-w", "m")
	if err == nil || !strings.Contains(err.Error(), "ROKSBNKARGOCTL_BNK_TMM_REPLICAS") {
		t.Fatalf("want an error naming the variable, got %v", err)
	}
}

func TestTwoFlagsForOneKeyIsAnError(t *testing.T) {
	isolate(t)
	bind := func(c *cobra.Command) {
		bindConfigFlag(c, "region", "ibmcloud.region")
		bindConfigFlag(c, "ibm-region", "ibmcloud.region")
	}
	_, _, err := probe(t, true, bind, "--no-workspace", "--region", "a", "--ibm-region", "b")
	if err == nil || !strings.Contains(err.Error(), "both set ibmcloud.region") {
		t.Fatalf("got %v", err)
	}
}

// ---- save -----------------------------------------------------------------------

// The contract: an env or flag value is never written to config.yaml, while an
// intended change and resolved state are. Driven through `init -f` (the real
// command, with only the IBM Cloud lookups faked) and then argocd up's
// recording step.
func TestOverridesAreNeverSaved(t *testing.T) {
	home := isolate(t)
	src := filepath.Join(t.TempDir(), "in.yaml")
	if err := os.WriteFile(src, []byte(`
ibmcloud: {region: us-south}
cluster: file-cluster
transit_gateway: tgw
cos: {instance: ci, bucket: b}
argocd: {server: "https://file.example"}
git: {url: "https://git.example/r.git"}
`), 0o600); err != nil {
		t.Fatal(err)
	}
	var sawCluster string
	resolveWorkspace = func(_ context.Context, s *session) error {
		sawCluster = s.cfg.Cluster
		s.cfg.Resolved = &config.Resolved{ClusterID: "id-1", ClusterName: s.cfg.Cluster, TransitGatewayID: "tg-1", TransitGatewayName: "tgw"}
		return nil
	}
	t.Cleanup(func() { resolveWorkspace = resolve })
	t.Setenv("ROKSBNKARGOCTL_CLUSTER", "env-cluster")
	t.Setenv("ROKSBNKARGOCTL_BNK_TMM_REPLICAS", "7")
	t.Setenv("ROKSBNKARGOCTL_GIT_BRANCH", "env-branch")

	root := newRoot()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"init", "-w", "ov", "-f", src})
	if err := root.Execute(); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if sawCluster != "env-cluster" {
		t.Errorf("init resolved %q; the override must apply to the lookups", sawCluster)
	}
	if !strings.Contains(out.String(), "ROKSBNKARGOCTL_CLUSTER") || !strings.Contains(out.String(), "not saved") {
		t.Errorf("init must say which overrides were not saved:\n%s", out.String())
	}
	cfgPath := filepath.Join(home, "ov", "config.yaml")
	saved := readSaved(t, cfgPath)
	if saved.Cluster != "file-cluster" || saved.BNK.TMMReplicas != 3 || saved.Git.Branch != "main" {
		t.Errorf("init saved an override: cluster=%q tmm=%d branch=%q", saved.Cluster, saved.BNK.TMMReplicas, saved.Git.Branch)
	}
	if saved.Resolved == nil || saved.Resolved.ClusterID != "id-1" {
		t.Errorf("resolved state was not saved: %+v", saved.Resolved)
	}

	// argocd up: env overrides argocd.server and git.branch; the hub URL is an
	// intended change and must be saved, the env values must not.
	t.Setenv("ROKSBNKARGOCTL_ARGOCD_SERVER", "https://env.example")
	s, sout, err := probe(t, false, nil, "-w", "ov")
	if err != nil {
		t.Fatalf("%v\n%s", err, sout)
	}
	var warned bytes.Buffer
	s.p = printer{&warned}
	if err := recordTestHub(s, "https://hub.example:30443"); err != nil {
		t.Fatal(err)
	}
	saved = readSaved(t, cfgPath)
	if saved.ArgoCD.Server != "https://hub.example:30443" || !saved.ArgoCD.Insecure {
		t.Errorf("the intended change was not saved: %+v", saved.ArgoCD)
	}
	if saved.Cluster != "file-cluster" || saved.BNK.TMMReplicas != 3 || saved.Git.Branch != "main" {
		t.Errorf("save wrote an override: cluster=%q tmm=%d branch=%q", saved.Cluster, saved.BNK.TMMReplicas, saved.Git.Branch)
	}
	if saved.Resolved == nil || saved.Resolved.ClusterID != "id-1" {
		t.Errorf("save lost resolved state: %+v", saved.Resolved)
	}
	// The settings, that is: resolved rightly records the cluster init resolved.
	b, _ := os.ReadFile(cfgPath)
	settings, _, _ := strings.Cut(string(b), "\nresolved:")
	for _, leak := range []string{"env-cluster", "env-branch", "env.example", "tmm_replicas: 7"} {
		if strings.Contains(settings, leak) {
			t.Errorf("config.yaml contains the override %q:\n%s", leak, b)
		}
	}
	if s.cfg.ArgoCD.Server != "https://hub.example:30443" {
		t.Errorf("the rest of the command must see the change, got %s", s.cfg.ArgoCD.Server)
	}
	if !strings.Contains(warned.String(), "ROKSBNKARGOCTL_ARGOCD_SERVER overrides it") {
		t.Errorf("no warning that the env var still overrides the saved server: %q", warned.String())
	}
}

// A setting changed on the effective config without update cannot be told
// from an override, so save refuses rather than guess.
func TestSaveRefusesASettingChangedWithoutUpdate(t *testing.T) {
	home := isolate(t)
	p := writeWorkspace(t, home, "g", "cluster: c\n")
	s, out, err := probe(t, false, nil, "-w", "g")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	s.cfg.ArgoCD.Server = "https://direct.example"
	err = s.save()
	if err == nil || !strings.Contains(err.Error(), "argocd.server") {
		t.Fatalf("save must refuse and name the setting, got %v", err)
	}
	if b, _ := os.ReadFile(p); string(b) != "cluster: c\n" {
		t.Errorf("config.yaml was written:\n%s", b)
	}
	// Resolved state alone is fine.
	s.cfg.ArgoCD.Server = s.pinned.ArgoCD.Server
	s.cfg.Resolved = &config.Resolved{ClusterID: "x"}
	if err := s.save(); err != nil {
		t.Fatal(err)
	}
	if readSaved(t, p).Resolved.ClusterID != "x" {
		t.Error("resolved not saved")
	}
}

// ---- workspace-optional sessions ---------------------------------------------------

type fakeLookup struct {
	calls map[string]int
}

func (f *fakeLookup) ResolveResourceGroup(_ context.Context, n string) (string, string, error) {
	f.calls["rg:"+n]++
	return "rg-id-" + n, n, nil
}

func (f *fakeLookup) ResolveTransitGateway(_ context.Context, n string) (*ibm.TransitGateway, error) {
	f.calls["tgw:"+n]++
	return &ibm.TransitGateway{ID: "tgw-id-" + n, Name: n}, nil
}

func (f *fakeLookup) FindServiceInstance(_ context.Context, n, svc, _ string) (*ibm.ServiceInstance, error) {
	f.calls["si:"+n+":"+svc]++
	return &ibm.ServiceInstance{CRN: "crn-" + n}, nil
}

func farTarball(t *testing.T, sa string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: "key.json", Mode: 0o600, Size: int64(len(sa)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(sa)); err != nil {
		t.Fatal(err)
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

// Without a workspace the session starts from the defaults, every helper the
// cos/flp/registry commands need works from the effective config, and nothing
// that needs a workspace panics.
func TestSessionWithoutAWorkspace(t *testing.T) {
	isolate(t)
	dir := t.TempDir()
	far := filepath.Join(dir, "far.tgz")
	jwt := filepath.Join(dir, "sub.jwt")
	if err := os.WriteFile(far, farTarball(t, `{"sa":"x"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(jwt, []byte("a.b.c\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ROKSBNKARGOCTL_COS_LOCAL_FAR_AUTH_FILE", far)
	t.Setenv("IBMCLOUD_API_KEY", "not-a-real-key")
	bind := func(c *cobra.Command) { bindConfigFlag(c, "jwt-file", "cos.local_jwt_file") }
	s, out, err := probe(t, true, bind, "--jwt-file", jwt)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if s.ws != nil || s.file != nil || s.hasWorkspace() {
		t.Fatalf("no workspace selected, yet the session has one: %+v", s.ws)
	}
	if s.cfg.BNK.Namespace != "f5-bnk" || s.cfg.ArgoCD.Application != "bnk-"+noWorkspaceName {
		t.Errorf("defaults not applied: ns=%q app=%q", s.cfg.BNK.Namespace, s.cfg.ArgoCD.Application)
	}
	ctx := context.Background()
	if sa, err := s.FARServiceAccount(ctx); err != nil || sa != `{"sa":"x"}` {
		t.Errorf("FARServiceAccount: %q %v", sa, err)
	}
	if j, err := s.JWT(ctx); err != nil || j != "a.b.c" {
		t.Errorf("JWT: %q %v", j, err)
	}
	if _, err := s.Puller(ctx, true); err != nil {
		t.Errorf("Puller: %v", err)
	}
	if _, err := s.IBM(); err != nil {
		t.Errorf("IBM: %v", err)
	}
	if err := s.save(); !errors.Is(err, errNoWorkspaceToSave) {
		t.Errorf("save without a workspace: %v", err)
	}
	if _, err := s.resolved(); err == nil || !strings.Contains(err.Error(), "needs a resolved workspace") {
		t.Errorf("resolved without a workspace: %v", err)
	}
	if _, err := s.workspace("flp up"); err == nil || !strings.Contains(err.Error(), "flp up needs a workspace") {
		t.Errorf("workspace(): %v", err)
	}
	s.update(func(c *config.Config) { c.ArgoCD.Server = "https://x" }) // must not panic

	// On-the-fly lookups from the effective config, cached for the session.
	fl := &fakeLookup{calls: map[string]int{}}
	s.lookup = fl
	s.cfg.COS.Instance, s.cfg.TransitGateway, s.cfg.IBMCloud.ResourceGroup = "my-cos", "my-tgw", "my-rg"
	for i := 0; i < 2; i++ {
		if crn, err := s.COSInstanceCRN(ctx); err != nil || crn != "crn-my-cos" {
			t.Errorf("COSInstanceCRN: %q %v", crn, err)
		}
		if id, err := s.TransitGatewayID(ctx); err != nil || id != "tgw-id-my-tgw" {
			t.Errorf("TransitGatewayID: %q %v", id, err)
		}
		if id, err := s.ResourceGroupID(ctx); err != nil || id != "rg-id-my-rg" {
			t.Errorf("ResourceGroupID: %q %v", id, err)
		}
	}
	for k, n := range fl.calls {
		if n != 1 {
			t.Errorf("%s looked up %d times; want once, then cached", k, n)
		}
	}
	if len(fl.calls) != 3 {
		t.Errorf("lookups %v", fl.calls)
	}
}

// With a workspace the recorded facts are used, not looked up — unless an
// override names something else, which is then looked up.
func TestRecordedFactsUnlessOverridden(t *testing.T) {
	home := isolate(t)
	writeWorkspace(t, home, "r", `
cluster: c
transit_gateway: tgw
cos: {instance: ci, bucket: b}
resolved:
  cluster_id: cid
  cluster_name: c
  cluster_crn: ""
  vpc_id: ""
  vpc_name: ""
  vpc_crn: ""
  zones: []
  transit_gateway_id: rec-tgw
  transit_gateway_name: tgw
  cluster_attached_to_tgw: true
  cos_instance_crn: rec-crn
  resource_group_id: rec-rg
`)
	s, out, err := probe(t, false, nil, "-w", "r")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	fl := &fakeLookup{calls: map[string]int{}}
	s.lookup = fl
	ctx := context.Background()
	crn, _ := s.COSInstanceCRN(ctx)
	tg, _ := s.TransitGatewayID(ctx)
	rg, _ := s.ResourceGroupID(ctx)
	if crn != "rec-crn" || tg != "rec-tgw" || rg != "rec-rg" || len(fl.calls) != 0 {
		t.Errorf("recorded facts not used: %s %s %s lookups=%v", crn, tg, rg, fl.calls)
	}

	t.Setenv("ROKSBNKARGOCTL_COS_INSTANCE", "other-cos")
	t.Setenv("ROKSBNKARGOCTL_TRANSIT_GATEWAY", "other-tgw")
	t.Setenv("ROKSBNKARGOCTL_IBMCLOUD_RESOURCE_GROUP", "other-rg")
	s, out, err = probe(t, false, nil, "-w", "r")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	s.lookup = fl
	crn, _ = s.COSInstanceCRN(ctx)
	tg, _ = s.TransitGatewayID(ctx)
	rg, _ = s.ResourceGroupID(ctx)
	if crn != "crn-other-cos" || tg != "tgw-id-other-tgw" || rg != "rg-id-other-rg" {
		t.Errorf("an override must be looked up, not answered from the record: %s %s %s", crn, tg, rg)
	}
	// ...and resolved() refuses to act on a record for another gateway.
	if _, err := s.resolved(); err == nil || !strings.Contains(err.Error(), "ROKSBNKARGOCTL_TRANSIT_GATEWAY overrides transit_gateway") {
		t.Errorf("resolved() with a gateway override it does not match: %v", err)
	}
}

// resolved() accepts an override that names the recorded cluster (by id or
// name) and refuses one that names another; with no override nothing changes.
func TestResolvedRefusesAnOverrideItWasNotResolvedFor(t *testing.T) {
	home := isolate(t)
	writeWorkspace(t, home, "rc", `
cluster: c
transit_gateway: tgw
argocd: {cluster_endpoint: private}
resolved:
  cluster_id: cid
  cluster_name: c
  cluster_crn: ""
  vpc_id: ""
  vpc_name: ""
  vpc_crn: ""
  zones: []
  private_endpoint: https://priv
  public_endpoint: https://pub
  argocd_cluster_server: https://priv
  transit_gateway_id: tid
  transit_gateway_name: tgw
  cluster_attached_to_tgw: true
`)
	for _, tc := range []struct {
		env, val string
		ok       bool
	}{
		{"", "", true},
		{"ROKSBNKARGOCTL_CLUSTER", "cid", true},
		{"ROKSBNKARGOCTL_CLUSTER", "other", false},
		{"ROKSBNKARGOCTL_TRANSIT_GATEWAY", "tid", true},
		{"ROKSBNKARGOCTL_ARGOCD_CLUSTER_ENDPOINT", "public", false},
	} {
		if tc.env != "" {
			t.Setenv(tc.env, tc.val)
		}
		s, out, err := probe(t, false, nil, "-w", "rc")
		if err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		_, err = s.resolved()
		if tc.ok != (err == nil) {
			t.Errorf("%s=%s: resolved() err=%v, want ok=%v", tc.env, tc.val, err, tc.ok)
		}
		if tc.env != "" {
			t.Setenv(tc.env, "")
		}
	}
}

func TestNoWorkspaceFlagIgnoresTheCurrentWorkspace(t *testing.T) {
	home := isolate(t)
	writeWorkspace(t, home, "cur", "cluster: from-file\n")
	if err := setCurrent("cur"); err != nil {
		t.Fatal(err)
	}
	s, out, err := probe(t, true, nil)
	if err != nil || s.ws == nil || s.cfg.Cluster != "from-file" {
		t.Fatalf("optional session must load the current workspace: %v %v\n%s", s, err, out)
	}
	s, out, err = probe(t, true, nil, "--no-workspace")
	if err != nil || s.ws != nil || s.cfg.Cluster != "" {
		t.Fatalf("--no-workspace must ignore the current pointer: ws=%v cluster=%q err=%v\n%s", s.ws, s.cfg.Cluster, err, out)
	}
	t.Setenv("ROKSBNKARGOCTL_WORKSPACE", "cur")
	if s, _, err = probe(t, true, nil, "--no-workspace"); err != nil || s.ws != nil {
		t.Fatalf("--no-workspace must ignore ROKSBNKARGOCTL_WORKSPACE: %v %v", s.ws, err)
	}
	if _, _, err = probe(t, true, nil, "--no-workspace", "-w", "cur"); err == nil {
		t.Fatal("-w with --no-workspace must be an error")
	}
	// A workspace that is named but missing is an error even for an optional session.
	t.Setenv("ROKSBNKARGOCTL_WORKSPACE", "gone")
	if _, _, err = probe(t, true, nil); err == nil || !strings.Contains(err.Error(), "gone") {
		t.Fatalf("a named, missing workspace must be an error: %v", err)
	}
}

// Workspace commands keep requiring one, with the message they always had.
func TestWorkspaceCommandsStillRequireAWorkspace(t *testing.T) {
	home := isolate(t)
	for _, args := range [][]string{{"status"}, {"render"}, {"install"}, {"uninstall"}, {"cos", "list"}, {"export"}} {
		root := newRoot()
		var out bytes.Buffer
		root.SetOut(&out)
		root.SetErr(&out)
		root.SetArgs(args)
		err := root.Execute()
		if err == nil || err.Error() != "no workspace selected: pass -w <name> or run `roksbnkargoctl init -w <name>`" {
			t.Errorf("%v: got %v", args, err)
		}
	}
	writeWorkspace(t, home, "cur", "cluster: c\n")
	if err := setCurrent("cur"); err != nil {
		t.Fatal(err)
	}
	_, _, err := probe(t, false, nil, "--no-workspace")
	if !errors.Is(err, errWorkspaceRefused) {
		t.Fatalf("a workspace command with --no-workspace: %v", err)
	}
	root := newRoot()
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"init", "--no-workspace"})
	if err := root.Execute(); err == nil || !strings.Contains(err.Error(), "--no-workspace") {
		t.Fatalf("init --no-workspace: %v", err)
	}
}
