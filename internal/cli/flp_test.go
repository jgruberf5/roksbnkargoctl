package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/jgruberf5/roksbnkargoctl/internal/config"
	"github.com/jgruberf5/roksbnkargoctl/internal/vsi"
	"github.com/jgruberf5/roksbnkargoctl/internal/vsi/vsitest"
)

// flpFake replaces IBM Cloud and FAR for the flp commands: every region's VPC
// API is cloud (the region asked for is recorded), names resolve through
// fakeLookup, and the JWKS is canned. It returns the scratch directory the
// test runs in, with the F5 files in it.
func flpFake(t *testing.T) (cloud *vsitest.Cloud, lookups *fakeLookup, regions *[]string, dir string) {
	t.Helper()
	cloud = vsitest.New("us-east")
	cloud.AddGateway("tgw-id-mytgw", "mytgw")
	lookups = &fakeLookup{calls: map[string]int{}}
	regions = &[]string{}
	oldAPI, oldJWKS, oldLookup := flpVSIAPI, flpProdJWKS, newIBMLookup
	flpVSIAPI = func(_ *session, region string) (vsi.API, error) {
		*regions = append(*regions, region)
		return cloud, nil
	}
	flpProdJWKS = func(context.Context, *session, string) ([]byte, error) { return []byte(`{"keys":[]}`), nil }
	newIBMLookup = func(*session) (ibmLookup, error) { return lookups, nil }
	t.Cleanup(func() { flpVSIAPI, flpProdJWKS, newIBMLookup, flagYes = oldAPI, oldJWKS, oldLookup, false })
	dir = t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "far.tgz"), farTarball(t, `{"sa":"x"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sub.jwt"), []byte("a.b.c\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	return cloud, lookups, regions, dir
}

// runFLP executes the real root command, with stdout and stderr apart.
func runFLP(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	defer func() { flagWorkspace, flagYes, flagNoWorkspace, flagVerbose = "", false, false, false }()
	root := newRoot()
	var o, e bytes.Buffer
	root.SetOut(&o)
	root.SetErr(&e)
	root.SetArgs(args)
	err = root.Execute()
	return o.String(), e.String(), err
}

// flpUpArgs is a no-workspace flp up with everything on the command line.
func flpUpArgs(extra ...string) []string {
	return append([]string{"flp", "up", "--no-workspace", "--name", "flptest", "--region", "us-east",
		"--resource-group", "myrg", "--transit-gateway", "mytgw", "--cidr", "10.248.0.0/28",
		"--far-auth-file", "far.tgz", "--jwt-file", "sub.jwt"}, extra...)
}

func flpMode(t *testing.T, path string) os.FileMode {
	t.Helper()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return st.Mode().Perm()
}

// Every flp.vsi key, and the placement and F5-file inputs, are flags of flp up.
func TestFLPUpHasAFlagForEveryInput(t *testing.T) {
	up, _, err := newFLPCmd().Find([]string{"up"})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"region": "ibmcloud.region", "resource-group": "ibmcloud.resource_group", "transit-gateway": "transit_gateway",
		"far-auth-file": "cos.local_far_auth_file", "jwt-file": "cos.local_jwt_file", "cos-instance": "cos.instance",
		"cos-bucket": "cos.bucket", "cos-region": "cos.region", "far-auth-object": "cos.far_auth_object", "jwt-object": "cos.jwt_object",
	}
	for _, k := range config.KeysUnder("flp.vsi") {
		want[strings.ReplaceAll(strings.TrimPrefix(k.Path, "flp.vsi."), "_", "-")] = k.Path
	}
	for name, path := range want {
		f := up.Flags().Lookup(name)
		if f == nil {
			t.Errorf("flp up has no --%s", name)
			continue
		}
		if got := f.Annotations[configKeyAnnotation]; len(got) != 1 || got[0] != path {
			t.Errorf("--%s overrides %v, want %s", name, got, path)
		}
	}
	for _, name := range []string{"name", "state", "ca-out"} {
		if up.Flags().Lookup(name) == nil {
			t.Errorf("flp up has no --%s", name)
		}
	}
}

// Precedence through the real flp up: flag > env > config.yaml > default, seen
// in what it builds. With a workspace the base name stays <workspace>-flp.
func TestFLPUpFlagEnvFilePrecedence(t *testing.T) {
	for _, tc := range []struct {
		name      string
		flag, env bool
		wantCIDR  string
	}{
		{"flag", true, true, "10.3.0.0/28"},
		{"env", false, true, "10.2.0.0/28"},
		{"file", false, false, "10.1.0.0/28"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := isolate(t)
			cloud, lookups, regions, _ := flpFake(t)
			writeWorkspace(t, home, "p", `
cluster: c
transit_gateway: mytgw
ibmcloud: {region: us-east}
cos: {local_far_auth_file: far.tgz, local_jwt_file: sub.jwt}
flp: {vsi: {cidr: 10.1.0.0/28, zone: us-east-2}}
`)
			args := []string{"flp", "up", "-w", "p"}
			if tc.env {
				t.Setenv("ROKSBNKARGOCTL_FLP_VSI_CIDR", "10.2.0.0/28")
			}
			if tc.flag {
				args = append(args, "--cidr", "10.3.0.0/28", "--region", "eu-de", "--zone", "eu-de-3", "--profile", "cx2-2x4")
			}
			if _, e, err := runFLP(t, args...); err != nil {
				t.Fatalf("%v\n%s", err, e)
			}
			var sn, inst string
			var profile string
			for _, s := range cloud.Subnets {
				sn = s.Name + " " + s.CIDR + " " + s.Zone
			}
			for id, in := range cloud.Instances {
				inst, profile = in.Name, cloud.InstanceSpecs[id].Profile
			}
			region, zone, wantProfile := "us-east", "us-east-2", "bx2-4x16" // file, and the default profile
			if tc.flag {
				region, zone, wantProfile = "eu-de", "eu-de-3", "cx2-2x4"
			}
			if want := "p-flp-subnet " + tc.wantCIDR + " " + zone; sn != want {
				t.Errorf("subnet %q, want %q", sn, want)
			}
			if inst != "p-flp" || profile != wantProfile {
				t.Errorf("instance %q profile %q, want p-flp %s", inst, profile, wantProfile)
			}
			if len(*regions) == 0 || (*regions)[0] != region {
				t.Errorf("regions %v, want %s", *regions, region)
			}
			if lookups.calls["tgw:mytgw"] != 1 || lookups.calls["rg:default"] != 1 {
				t.Errorf("lookups %v", lookups.calls)
			}
			if _, err := os.Stat(filepath.Join(home, "p", "flp-outputs.json")); err != nil {
				t.Errorf("with a workspace the state stays in it: %v", err)
			}
			if _, err := os.Stat("p-flp-ca.pem"); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("with a workspace no CA file is written unless --ca-out: %v", err)
			}
		})
	}
}

func TestFLPUpNeedsANameWithoutAWorkspace(t *testing.T) {
	isolate(t)
	cloud, _, _, _ := flpFake(t)
	args := flpUpArgs()
	noName := append(append([]string{}, args[:3]...), args[5:]...) // drop --name flptest
	_, _, err := runFLP(t, noName...)
	if err == nil || !strings.HasPrefix(err.Error(), "--name is required without a workspace") {
		t.Fatalf("flp up without a workspace or --name: %v", err)
	}
	_, _, err = runFLP(t, append(noName, "--name", "Bad_Name")...)
	if err == nil || !strings.Contains(err.Error(), `--name "Bad_Name"`) {
		t.Fatalf("an invalid --name: %v", err)
	}
	for _, sub := range [][]string{{"flp", "status", "--no-workspace"}, {"flp", "down", "--no-workspace", "--yes"}} {
		if _, _, err := runFLP(t, sub...); err == nil || !strings.Contains(err.Error(), "pass --state <file> or --name <name>") {
			t.Errorf("%v: %v", sub, err)
		}
	}
	if n := cloud.Names(); len(n) != 0 {
		t.Errorf("created %v", n)
	}
}

// The whole no-workspace life cycle through the real root command, with
// ROKSBNKARGOCTL_HOME an empty directory: up writes the state and the CA
// (0600), prints the settings to use it, reuses the CA on a re-run; status
// and down read the same state; nothing is left and HOME stays empty.
func TestFLPWithoutAWorkspace(t *testing.T) {
	home := isolate(t)
	cloud, lookups, _, dir := flpFake(t)
	out, errOut, err := runFLP(t, flpUpArgs()...)
	if err != nil {
		t.Fatalf("%v\n%s", err, errOut)
	}
	if lookups.calls["tgw:mytgw"] != 1 || lookups.calls["rg:myrg"] != 1 {
		t.Errorf("lookups %v", lookups.calls)
	}
	var rec flpRecord
	if err := readJSON("flptest-flp.json", &rec); err != nil {
		t.Fatal(err)
	}
	if rec.Name != "flptest-flp" || rec.Region != "us-east" || rec.InstanceID == "" || rec.TGWConnectionID == "" || rec.RootCAKeyPEM == "" {
		t.Fatalf("state %+v", rec)
	}
	ca, err := os.ReadFile("flptest-flp-ca.pem")
	if err != nil || string(ca) != rec.RootCAPEM {
		t.Fatalf("CA file %q (%v) is not the state's CA", ca, err)
	}
	if runtime.GOOS != "windows" {
		for _, f := range []string{"flptest-flp.json", "flptest-flp-ca.pem"} {
			if m := flpMode(t, f); m != 0o600 {
				t.Errorf("%s mode %o, want 600", f, m)
			}
		}
	}
	caAbs := filepath.Join(dir, "flptest-flp-ca.pem")
	for _, want := range []string{"url: " + rec.URL, "root_ca_file: " + caAbs,
		"export ROKSBNKARGOCTL_FLP_EXTERNAL_URL='" + rec.URL + "'", "export ROKSBNKARGOCTL_FLP_EXTERNAL_ROOT_CA_FILE='" + caAbs + "'"} {
		if runtime.GOOS == "windows" {
			break
		}
		if !strings.Contains(out, want) {
			t.Errorf("stdout lacks %q:\n%s", want, out)
		}
	}
	if !strings.HasPrefix(rec.URL, "https://10.248.") {
		t.Errorf("URL %q", rec.URL)
	}
	built := cloud.Names()

	// A re-run keeps the CA (BNK trusts it) and builds nothing new.
	if _, e, err := runFLP(t, flpUpArgs()...); err != nil {
		t.Fatalf("%v\n%s", err, e)
	}
	var again flpRecord
	if err := readJSON("flptest-flp.json", &again); err != nil {
		t.Fatal(err)
	}
	if again.RootCAPEM != rec.RootCAPEM || again.RootCAKeyPEM != rec.RootCAKeyPEM {
		t.Fatal("a re-run minted a new CA")
	}
	if ca2, _ := os.ReadFile("flptest-flp-ca.pem"); string(ca2) != rec.RootCAPEM {
		t.Error("the CA file changed on a re-run")
	}
	if !reflect.DeepEqual(cloud.Names(), built) {
		t.Errorf("re-run changed the cloud: %v -> %v", built, cloud.Names())
	}

	out, e, err := runFLP(t, "flp", "status", "--no-workspace", "--name", "flptest")
	if err != nil || !strings.Contains(out, "flptest-flp ("+rec.InstanceID+") running") || !strings.Contains(out, "CA:         flptest-flp-ca.pem") {
		t.Fatalf("status: %v\n%s%s", err, out, e)
	}

	if _, _, err := runFLP(t, "flp", "down", "--no-workspace", "--name", "flptest"); err == nil {
		t.Fatal("down without --yes on a non-terminal must refuse")
	}
	if !reflect.DeepEqual(cloud.Names(), built) {
		t.Fatal("an unconfirmed down deleted something")
	}
	if _, e, err := runFLP(t, "flp", "down", "--no-workspace", "--name", "flptest", "--yes"); err != nil {
		t.Fatalf("%v\n%s", err, e)
	}
	if n := cloud.Names(); len(n) != 0 {
		t.Errorf("left behind: %v", n)
	}
	for _, f := range []string{"flptest-flp.json", "flptest-flp-ca.pem"} {
		if _, err := os.Stat(f); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s not removed: %v", f, err)
		}
	}
	if ents, _ := os.ReadDir(home); len(ents) != 0 {
		t.Errorf("ROKSBNKARGOCTL_HOME was written: %v", ents)
	}
}

// With the state lost, down --name finds what up named, asks, and removes it
// and nothing else — not a longer name, not a connection of the right name on
// another VPC. An explicit --state that is missing is an error, not a search.
func TestFLPDownByNameWhenTheStateIsLost(t *testing.T) {
	isolate(t)
	cloud, _, _, _ := flpFake(t)
	cloud.AddGateway("tgw-0", "first")
	other := cloud.AddVPC("flptest-flp-vpc-old")
	cloud.AddConnection("tgw-0", "flptest-flp", other.CRN) // listed before the real one
	cloud.AddSubnet("flptest-flp-subnet", other.ID, "us-east-1", "")
	cloud.AddSecurityGroup("flptest-flp-sg", other.ID)
	cloud.AddVPC("xflptest-flp-vpc")
	cloud.AddFloatingIP("flptest-flp-fip-2", "us-east-1")
	cloud.AddInstance("flptest-flp-2", other.ID, "us-east-1")
	decoys := cloud.Names()

	if _, e, err := runFLP(t, flpUpArgs()...); err != nil {
		t.Fatalf("%v\n%s", err, e)
	}
	if err := os.Rename("flptest-flp.json", "aside.json"); err != nil {
		t.Fatal(err)
	}
	built := cloud.Names()

	if _, _, err := runFLP(t, "flp", "down", "--no-workspace", "--state", "missing.json", "--region", "us-east", "--yes"); err == nil || !strings.Contains(err.Error(), "missing.json does not exist") {
		t.Fatalf("an explicit, missing --state: %v", err)
	}
	_, e, err := runFLP(t, "flp", "down", "--no-workspace", "--name", "flptest", "--region", "us-east")
	if err == nil || !strings.Contains(e, "Found, by exact name:") {
		t.Fatalf("an unconfirmed discovery must list and refuse: %v\n%s", err, e)
	}
	if !reflect.DeepEqual(cloud.Names(), built) {
		t.Fatal("an unconfirmed discovery deleted something")
	}
	_, e, err = runFLP(t, "flp", "down", "--no-workspace", "--name", "flptest", "--region", "us-east", "--yes")
	if err != nil {
		t.Fatalf("%v\n%s", err, e)
	}
	if n := cloud.Names(); !reflect.DeepEqual(n, decoys) {
		t.Errorf("after down:\n  %v\nwant exactly the decoys:\n  %v", n, decoys)
	}
	// Nothing left to find is not an error.
	if _, e, err := runFLP(t, "flp", "down", "--no-workspace", "--name", "flptest", "--region", "us-east", "--yes"); err != nil || !strings.Contains(e, "nothing named flptest-flp*") {
		t.Errorf("second down: %v\n%s", err, e)
	}
}

// A state file for another FLP is never overwritten: that would orphan it.
func TestFLPUpRefusesAnotherFLPsState(t *testing.T) {
	isolate(t)
	cloud, _, _, _ := flpFake(t)
	if err := writeJSON("flptest-flp.json", flpRecord{Record: vsi.Record{Name: "someone-flp", Region: "us-east"}}); err != nil {
		t.Fatal(err)
	}
	_, _, err := runFLP(t, flpUpArgs()...)
	if err == nil || !strings.Contains(err.Error(), "records license proxy someone-flp in us-east, not flptest-flp") {
		t.Fatalf("got %v", err)
	}
	if n := cloud.Names(); len(n) != 0 {
		t.Errorf("created %v", n)
	}
}

// argocd up without test_hub.cidr says it builds a NEW Argo CD, and how to use
// an existing one instead.
func TestArgoCDUpSaysItBuildsANewArgoCD(t *testing.T) {
	home := isolate(t)
	writeWorkspace(t, home, "h", `
cluster: c
transit_gateway: tgw
resolved:
  cluster_id: cid
  cluster_name: c
  cluster_crn: ""
  vpc_id: ""
  vpc_name: ""
  vpc_crn: ""
  zones: []
  transit_gateway_id: tid
  transit_gateway_name: tgw
  cluster_attached_to_tgw: true
`)
	_, _, err := runFLP(t, "argocd", "up", "-w", "h")
	if err == nil {
		t.Fatal("argocd up without test_hub.cidr succeeded")
	}
	for _, want := range []string{"builds a NEW test Argo CD", "skip `argocd up`, set argocd.server", "test_hub.cidr"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("message lacks %q:\n%v", want, err)
		}
	}
}

// A workspace name may start with a digit; an IBM Cloud VPC resource name may
// not. flp up says so before building anything, and --name gets around it.
func TestFLPUpChecksAWorkspaceNameToo(t *testing.T) {
	home := isolate(t)
	cloud, _, _, _ := flpFake(t)
	writeWorkspace(t, home, "1ws", `
cluster: c
transit_gateway: mytgw
ibmcloud: {region: us-east}
cos: {local_far_auth_file: far.tgz, local_jwt_file: sub.jwt}
flp: {vsi: {cidr: 10.1.0.0/28}}
`)
	_, _, err := runFLP(t, "flp", "up", "-w", "1ws")
	if err == nil || !strings.Contains(err.Error(), `workspace name "1ws" cannot name IBM Cloud resources`) {
		t.Fatalf("got %v", err)
	}
	if n := cloud.Names(); len(n) != 0 {
		t.Fatalf("created %v", n)
	}
	if _, e, err := runFLP(t, "flp", "up", "-w", "1ws", "--name", "ws1"); err != nil {
		t.Fatalf("%v\n%s", err, e)
	}
	if in, _ := cloud.FindInstanceByName(context.Background(), "ws1-flp"); in == nil || len(cloud.Instances) != 1 {
		t.Errorf("instances %v", cloud.Names())
	}
}

// A CA file that cannot be written does not lose the license proxy: its state
// (every resource ID Provision built) is saved first.
func TestFLPUpSavesItsStateWhenTheCACannotBeWritten(t *testing.T) {
	home := isolate(t)
	cloud, _, _, dir := flpFake(t)
	writeWorkspace(t, home, "p", `
cluster: c
transit_gateway: mytgw
ibmcloud: {region: us-east}
cos: {local_far_auth_file: far.tgz, local_jwt_file: sub.jwt}
flp: {vsi: {cidr: 10.1.0.0/28}}
`)
	blocked := filepath.Join(dir, "ca-is-a-directory") // a file cannot replace it
	if err := os.MkdirAll(filepath.Join(blocked, "x"), 0o700); err != nil {
		t.Fatal(err)
	}
	_, e, err := runFLP(t, "flp", "up", "-w", "p", "--ca-out", blocked)
	if err == nil || !strings.Contains(err.Error(), "the license proxy is recorded in") {
		t.Fatalf("got %v\n%s", err, e)
	}
	b, rerr := os.ReadFile(filepath.Join(home, "p", "flp-outputs.json"))
	if rerr != nil || !strings.Contains(string(b), "instance_id") {
		t.Fatalf("no state after a failed CA write (%v): %s", rerr, b)
	}
	if len(cloud.Names()) == 0 {
		t.Fatal("nothing was built")
	}
}
