package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	ggcrregistry "github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/static"
	"github.com/google/go-containerregistry/pkg/v1/types"
	"github.com/jgruberf5/roksbnkargoctl/internal/cos"
	"github.com/jgruberf5/roksbnkargoctl/internal/cos/costest"
	"github.com/jgruberf5/roksbnkargoctl/internal/ibm"
	"github.com/spf13/cobra"

	"github.com/jgruberf5/roksbnkargoctl/internal/config"
	"github.com/jgruberf5/roksbnkargoctl/internal/far"
)

// ---- fixtures ------------------------------------------------------------------

// authRegistry is an in-memory OCI registry that answers 401 with a Basic
// challenge unless the request carries user:pass, so a test proves which
// credential reached it (go-containerregistry only sends one after a challenge).
func authRegistry(t *testing.T, user, pass string) string {
	t.Helper()
	reg := ggcrregistry.New(ggcrregistry.Logger(log.New(io.Discard, "", 0)))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, p, ok := r.BasicAuth(); !ok || u != user || p != pass {
			w.Header().Set("WWW-Authenticate", `Basic realm="test"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		reg.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://")
}

// fakeFAR is a FAR at host holding a BNK manifest chart that lists one chart
// and one image, those two, and a check image: a 4-artifact BOM without
// cert-manager. It returns the check image reference.
func fakeFAR(t *testing.T, host, sa string) string {
	t.Helper()
	opt := remote.WithAuth(&authn.Basic{Username: far.FARUsername, Password: sa})
	push := func(ref string) {
		t.Helper()
		img, err := random.Image(128, 1)
		if err != nil {
			t.Fatal(err)
		}
		r, _ := name.ParseReference(ref)
		if err := remote.Write(r, img, opt); err != nil {
			t.Fatal(err)
		}
	}
	manifest := `f5_helm_repo: repo
f5_docker_repo: repo
releases:
- version: ` + config.BNKVersion + `
  helm_charts:
  - {name: charts/f5-lifecycle-operator, version: "1.0.0+0.1"}
  docker_images:
  - {name: images/f5-tmm, version: "2.0.0"}
`
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	fn := "f5-bigip-k8s-manifest/bigip-k8s-manifest-" + config.BNKVersion + ".yaml"
	if err := tw.WriteHeader(&tar.Header{Name: fn, Mode: 0o644, Size: int64(len(manifest)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	tw.Write([]byte(manifest))
	tw.Close()
	gz.Close()
	chart, err := mutate.AppendLayers(empty.Image, static.NewLayer(buf.Bytes(), types.MediaType("application/vnd.cncf.helm.chart.content.v1.tar+gzip")))
	if err != nil {
		t.Fatal(err)
	}
	r, _ := name.ParseReference(far.ManifestChartRef(host, config.BNKVersion))
	if err := remote.Write(r, chart, opt); err != nil {
		t.Fatal(err)
	}
	push(host + "/charts/f5-lifecycle-operator:1.0.0_0.1")
	push(host + "/images/f5-tmm:2.0.0")
	check := host + "/jgruberf5/roksbnkargoctl-check:dev"
	push(check)
	return check
}

// writeFARKey writes an F5 auth tarball holding sa and returns its path.
func writeFARKey(t *testing.T, sa string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "f5-far-auth-key.tgz")
	if err := os.WriteFile(p, farTarball(t, sa), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// registryEnv is an isolated home, a FAR holding a 4-artifact BOM behind the
// FAR key, and a mirror behind user "robot" and the password in $TEST_MIRROR_PW.
type registryEnv struct {
	home, far, mirror, farKey, check string
}

const testSA = `{"type":"service_account","private_key":"test"}`

func newRegistryEnv(t *testing.T) registryEnv {
	t.Helper()
	e := registryEnv{home: isolate(t)}
	e.far = authRegistry(t, far.FARUsername, testSA)
	e.check = fakeFAR(t, e.far, testSA)
	e.mirror = authRegistry(t, "robot", "mirror-secret")
	e.farKey = writeFARKey(t, testSA)
	t.Setenv("TEST_MIRROR_PW", "mirror-secret")
	return e
}

// bomArgs are the flags that build the fake FAR's BOM.
func (e registryEnv) bomArgs() []string {
	return []string{"--far-host", e.far, "--far-auth-file", e.farKey, "--check-image", e.check, "--cert-manager-install=false"}
}

func (e registryEnv) mirrorArgs() []string {
	return []string{"--mirror-host", e.mirror, "--mirror-prefix", "bnk-mirror", "--mirror-username", "robot", "--mirror-password-env", "TEST_MIRROR_PW"}
}

func args(parts ...[]string) []string {
	var out []string
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// ---- flags ---------------------------------------------------------------------

// Every registry.mirror key is a --mirror-* flag on replicate and verify, every
// BOM input is a flag on all three, each bound to its config key — and the
// mirror password is not a flag at all.
func TestRegistryFlagsCoverEveryInput(t *testing.T) {
	cmds := map[string]*cobra.Command{}
	for _, c := range newRegistryCmd().Commands() {
		cmds[c.Name()] = c
	}
	bomFlags := map[string]string{
		"far-host": "registry.far_host", "far-auth-file": "cos.local_far_auth_file",
		"cos-instance": "cos.instance", "cos-bucket": "cos.bucket", "cos-region": "cos.region",
		"far-auth-object": "cos.far_auth_object", "api-key-env": "ibmcloud.api_key_env",
		"cert-manager-install": "bnk.cert_manager.install", "cert-manager-version": "bnk.cert_manager.version",
		"check-image": "check.image",
	}
	mirrorFlags := map[string]string{}
	for _, k := range config.KeysUnder("registry.mirror") {
		mirrorFlags["mirror-"+strings.ReplaceAll(strings.TrimPrefix(k.Path, "registry.mirror."), "_", "-")] = k.Path
	}
	if len(mirrorFlags) != 5 {
		t.Fatalf("registry.mirror has %d keys; this test expects host, prefix, username, password_env, ca_file", len(mirrorFlags))
	}
	want := map[string]map[string]string{"bom": bomFlags, "replicate": {}, "verify": {}}
	for _, n := range []string{"replicate", "verify"} {
		for f, p := range bomFlags {
			want[n][f] = p
		}
		for f, p := range mirrorFlags {
			want[n][f] = p
		}
	}
	for n, flags := range want {
		c := cmds[n]
		if c == nil {
			t.Fatalf("no registry %s", n)
		}
		for f, p := range flags {
			fl := c.Flags().Lookup(f)
			if fl == nil {
				t.Errorf("registry %s has no --%s (for %s)", n, f, p)
				continue
			}
			if got := fl.Annotations[configKeyAnnotation]; len(got) != 1 || got[0] != p {
				t.Errorf("registry %s --%s is bound to %v, want %s", n, f, got, p)
			}
		}
		for _, bad := range []string{"mirror-password", "password"} {
			if c.Flags().Lookup(bad) != nil {
				t.Errorf("registry %s takes the password as --%s; it must come from the environment", n, bad)
			}
		}
	}
	if cmds["replicate"].Flags().Lookup("concurrency") == nil || cmds["replicate"].Flags().Lookup("state") == nil {
		t.Error("registry replicate lost --concurrency or --state")
	}
}

// The flags set the effective config, over the environment.
func TestRegistryFlagsReachTheConfig(t *testing.T) {
	isolate(t)
	t.Setenv("ROKSBNKARGOCTL_REGISTRY_MIRROR_HOST", "env.example")
	t.Setenv("ROKSBNKARGOCTL_REGISTRY_MIRROR_PREFIX", "env-prefix")
	bind := func(c *cobra.Command) { bindBOMFlags(c); bindMirrorFlags(c) }
	s, out, err := probe(t, true, bind, "--no-workspace",
		"--mirror-host", "m.example:5000", "--mirror-username", "robot", "--mirror-password-env", "PW",
		"--mirror-ca-file", "/ca.pem", "--far-host", "far.example", "--far-auth-file", "/k.tgz",
		"--cos-instance", "ci", "--cos-bucket", "b", "--cos-region", "eu-de", "--far-auth-object", "o.tgz",
		"--api-key-env", "MY_KEY", "--cert-manager-install=false", "--cert-manager-version", "v9", "--check-image", "x/check:1")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	c := s.cfg
	m := c.Registry.Mirror
	got := []string{m.Host, m.Prefix, m.Username, m.PasswordEnv, m.CAFile, c.Registry.FARHost, c.COS.LocalFARAuthFile,
		c.COS.Instance, c.COS.Bucket, c.COS.Region, c.COS.FARAuthObject, c.IBMCloud.APIKeyEnv, c.BNK.CertManager.Version, c.Check.Image}
	want := []string{"m.example:5000", "env-prefix", "robot", "PW", "/ca.pem", "far.example", "/k.tgz",
		"ci", "b", "eu-de", "o.tgz", "MY_KEY", "v9", "x/check:1"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("config\n got %v\nwant %v", got, want)
	}
	if c.CertManagerInstall() {
		t.Error("--cert-manager-install=false did not reach the config")
	}
	if c.MirrorBase() != "m.example:5000/env-prefix" {
		t.Errorf("MirrorBase %q", c.MirrorBase())
	}
}

// ---- no workspace, end to end ----------------------------------------------------

// bom, verify and replicate run through the real root command with an empty
// ROKSBNKARGOCTL_HOME, from flags alone, against a FAR and a mirror that both
// demand their own credentials. Nothing is written to the home.
func TestRegistryWithoutAWorkspace(t *testing.T) {
	e := newRegistryEnv(t)

	out, err := runRoot(t, args([]string{"registry", "bom", "--no-workspace"}, e.bomArgs())...)
	if err != nil {
		t.Fatalf("bom: %v\n%s", err, out)
	}
	for _, w := range []string{
		"chart  " + e.far + "/release/f5-bigip-k8s-manifest:" + config.BNKVersion,
		"chart  " + e.far + "/charts/f5-lifecycle-operator:1.0.0_0.1",
		"image  " + e.far + "/images/f5-tmm:2.0.0",
		"image  " + e.check,
		"4 artifacts",
	} {
		if !strings.Contains(out, w) {
			t.Errorf("bom output lacks %q:\n%s", w, out)
		}
	}

	// Without --no-workspace too: an empty home selects no workspace.
	if out, err := runRoot(t, args([]string{"registry", "verify"}, e.bomArgs(), e.mirrorArgs())...); err == nil || !strings.Contains(err.Error(), "4 of 4 artifacts missing") {
		t.Fatalf("verify before replicate: %v\n%s", err, out)
	}
	out, err = runRoot(t, args([]string{"registry", "replicate", "--no-workspace"}, e.bomArgs(), e.mirrorArgs())...)
	if err != nil || !strings.Contains(out, "mirror complete: 4 copied, 0 already present") {
		t.Fatalf("replicate: %v\n%s", err, out)
	}
	out, err = runRoot(t, args([]string{"registry", "verify", "--no-workspace"}, e.bomArgs(), e.mirrorArgs())...)
	if err != nil || !strings.Contains(out, "all 4 artifacts present in the mirror") {
		t.Fatalf("verify after replicate: %v\n%s", err, out)
	}
	out, err = runRoot(t, args([]string{"registry", "replicate", "--no-workspace"}, e.bomArgs(), e.mirrorArgs())...)
	if err != nil || !strings.Contains(out, "mirror complete: 0 copied, 4 already present") {
		t.Fatalf("second replicate: %v\n%s", err, out)
	}
	if ents := homeEntries(t, e.home); len(ents) != 0 {
		t.Errorf("a run without a workspace wrote into the home: %v", ents)
	}
}

// The mirror password comes from the variable --mirror-password-env names: with
// it unset the mirror refuses, which is what proves the variable was the source.
func TestRegistryMirrorPasswordComesFromTheNamedVariable(t *testing.T) {
	e := newRegistryEnv(t)
	t.Setenv("TEST_MIRROR_PW", "")
	out, err := runRoot(t, args([]string{"registry", "verify", "--no-workspace"}, e.bomArgs(), e.mirrorArgs())...)
	if err == nil || strings.Contains(out, "artifacts present") {
		t.Fatalf("verify passed with no mirror password: %v\n%s", err, out)
	}
}

// A missing or malformed mirror host is a clear error naming the flag, before
// anything is fetched.
func TestRegistryNeedsAMirrorHost(t *testing.T) {
	isolate(t)
	key := writeFARKey(t, testSA)
	// An unreachable FAR: reaching it would fail with a dial error instead.
	base := []string{"--no-workspace", "--far-host", "127.0.0.1:1", "--far-auth-file", key}
	for _, sub := range []string{"verify", "replicate"} {
		_, err := runRoot(t, args([]string{"registry", sub}, base)...)
		if err == nil || !strings.Contains(err.Error(), "registry.mirror.host is not set: pass --mirror-host") {
			t.Errorf("registry %s without a mirror host: %v", sub, err)
		}
		_, err = runRoot(t, args([]string{"registry", sub, "--mirror-host", "https://m.example"}, base)...)
		if err == nil || !strings.Contains(err.Error(), "without a scheme") {
			t.Errorf("registry %s with a scheme in the mirror host: %v", sub, err)
		}
	}
}

// ---- the FAR key ---------------------------------------------------------------

// Without --far-auth-file the key is read from COS: the instance found by
// --cos-instance, the object --far-auth-object in --cos-bucket, from the
// bucket's OWN region. The bucket is in eu-de while --cos-region says
// us-south: a read through us-south would get NoSuchBucket. The FAR then
// accepts only that key.
func TestRegistryFARKeyFromCOS(t *testing.T) {
	e := newRegistryEnv(t)
	t.Setenv("IBMCLOUD_API_KEY", "not-a-real-key")
	s3 := costest.New(t, "us-south", "eu-de")
	s3.AddBucket(testCOSCRN("g-supply"), "supply", "eu-de-smart", map[string][]byte{"keys/far.tgz": farTarball(t, testSA)})
	fl := &fakeLookup{calls: map[string]int{}, instances: []ibm.ServiceInstance{
		{Name: "supply-cos", GUID: "g-supply", CRN: testCOSCRN("g-supply"), ResourceGroupID: "rg-1", State: "active"},
	}}
	newIBMLookup = func(*session) (ibmLookup, error) { return fl, nil }
	newCOSClient = func(ctx context.Context, key, crn, region string) (*cos.Client, error) {
		if key != "not-a-real-key" {
			t.Errorf("COS opened with API key %q", key)
		}
		if crn != testCOSCRN("g-supply") {
			t.Errorf("COS opened for instance %q", crn)
		}
		return cos.NewWith(ctx, key, crn, region, cos.Options{EndpointFor: s3.Endpoint, TokenURL: s3.TokenURL()})
	}
	t.Cleanup(func() { newIBMLookup, newCOSClient = defaultIBMLookup, cos.New })
	out, err := runRoot(t, "registry", "bom", "--no-workspace", "--far-host", e.far, "--check-image", e.check, "--cert-manager-install=false",
		"--cos-instance", "supply-cos", "--cos-bucket", "supply", "--cos-region", "us-south", "--far-auth-object", "keys/far.tgz")
	if err != nil || !strings.Contains(out, "4 artifacts") {
		t.Fatalf("bom with the key from COS: %v\n%s", err, out)
	}
	var read bool
	for _, r := range s3.Served() {
		if strings.HasPrefix(r, "eu-de GET ") && strings.HasSuffix(r, "/supply/keys/far.tgz") {
			read = true
		}
		if strings.HasPrefix(r, "us-south GET ") && strings.Contains(r, "/supply/") {
			t.Errorf("an object read went to the wrong region: %s", r)
		}
	}
	if !read {
		t.Errorf("the FAR key was not read from the bucket's region (eu-de): %v", s3.Served())
	}

	// The wrong object: the FAR key is not found, and the error says so.
	_, err = runRoot(t, "registry", "bom", "--no-workspace", "--far-host", e.far, "--check-image", e.check, "--cert-manager-install=false",
		"--cos-instance", "supply-cos", "--cos-bucket", "supply", "--far-auth-object", "nope.tgz")
	if err == nil || !strings.Contains(err.Error(), "FAR auth tarball") {
		t.Errorf("a missing COS object: %v", err)
	}
}

// With --far-auth-file the key comes from the file (the FAR accepts only it),
// and a wrong key is refused by FAR rather than silently replaced.
func TestRegistryFARKeyFromAFile(t *testing.T) {
	e := newRegistryEnv(t)
	newCOSClient = func(context.Context, string, string, string) (*cos.Client, error) {
		t.Error("COS was opened although --far-auth-file was given")
		return nil, errors.New("no COS")
	}
	t.Cleanup(func() { newCOSClient = cos.New })
	if out, err := runRoot(t, args([]string{"registry", "bom", "--no-workspace"}, e.bomArgs())...); err != nil || !strings.Contains(out, "4 artifacts") {
		t.Fatalf("bom: %v\n%s", err, out)
	}
	wrong := writeFARKey(t, `{"other":"key"}`)
	_, err := runRoot(t, "registry", "bom", "--no-workspace", "--far-host", e.far, "--far-auth-file", wrong, "--check-image", e.check)
	if err == nil {
		t.Error("the FAR accepted a key it does not hold")
	}
}

// Nothing saying where the FAR key is: a clear error naming both flags.
func TestRegistryNeedsAFARKey(t *testing.T) {
	isolate(t)
	for _, sub := range []string{"bom", "verify", "replicate"} {
		a := []string{"registry", sub, "--no-workspace", "--far-host", "127.0.0.1:1"}
		if sub != "bom" {
			a = append(a, "--mirror-host", "m.example")
		}
		_, err := runRoot(t, a...)
		if err == nil || !strings.Contains(err.Error(), "no FAR key: pass --far-auth-file") {
			t.Errorf("registry %s with no FAR key: %v", sub, err)
		}
	}
}

// ---- the replication record ----------------------------------------------------

// replicate writes registry-mirror.json into the workspace, as before; with
// --state it writes there instead; without a workspace or --state it writes
// nothing (asserted in TestRegistryWithoutAWorkspace).
func TestRegistryReplicateRecord(t *testing.T) {
	e := newRegistryEnv(t)
	writeWorkspace(t, e.home, "ws", "registry:\n  mirror:\n    host: "+e.mirror+"\n    prefix: from-file\n    username: robot\n    password_env: TEST_MIRROR_PW\n")
	out, err := runRoot(t, args([]string{"registry", "replicate", "-w", "ws"}, e.bomArgs())...)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	rec := readRecord(t, filepath.Join(e.home, "ws", "registry-mirror.json"))
	if rec.Mirror != e.mirror+"/from-file" || rec.Artifacts != 4 || rec.Copied != 4 {
		t.Errorf("workspace record %+v", rec)
	}

	state := filepath.Join(t.TempDir(), "state.json")
	out, err = runRoot(t, args([]string{"registry", "replicate", "--no-workspace", "--state", state}, e.bomArgs(), e.mirrorArgs())...)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	rec = readRecord(t, state)
	if rec.Mirror != e.mirror+"/bnk-mirror" || rec.Artifacts != 4 || rec.Copied != 4 || rec.Version != config.BNKVersion {
		t.Errorf("--state record %+v", rec)
	}
	ents := homeEntries(t, e.home)
	sort.Strings(ents)
	if strings.Join(ents, ",") != strings.Join([]string{sep("ws"), sep("ws", "config.yaml"), sep("ws", "registry-mirror.json")}, ",") {
		t.Errorf("--state also wrote into the home: %v", ents)
	}

	// A --state that cannot be written fails the command.
	bad := filepath.Join(state, "not-a-dir", "x.json")
	if _, err := runRoot(t, args([]string{"registry", "replicate", "--no-workspace", "--state", bad}, e.bomArgs(), e.mirrorArgs())...); err == nil || !strings.Contains(err.Error(), "--state") {
		t.Errorf("unwritable --state: %v", err)
	}
}

func sep(parts ...string) string { return string(filepath.Separator) + filepath.Join(parts...) }

func readRecord(t *testing.T, p string) mirrorRecord {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var r mirrorRecord
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatal(err)
	}
	return r
}
