package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jgruberf5/roksbnkargoctl/internal/cos"
	"github.com/jgruberf5/roksbnkargoctl/internal/cos/costest"
	"github.com/jgruberf5/roksbnkargoctl/internal/ibm"
)

func testCOSCRN(guid string) string {
	return "crn:v1:bluemix:public:cloud-object-storage:global:a/acct:" + guid + "::"
}

func testBucketCRN(guid, bucket string) string {
	return "crn:v1:bluemix:public:cloud-object-storage:global:a/acct:" + guid + ":bucket:" + bucket
}

// cosEnv is a fake account: the Resource Controller (instances, groups) and
// COS in us-south and eu-de. ROKSBNKARGOCTL_HOME is an empty directory with no
// current workspace, so a command that needed one would fail.
type cosEnv struct {
	home string
	s3   *costest.Fake
	rc   *fakeLookup
}

func newCOSEnv(t *testing.T) *cosEnv {
	t.Helper()
	e := &cosEnv{home: isolate(t)}
	t.Setenv("IBMCLOUD_API_KEY", "test-key")
	t.Setenv("IC_API_KEY", "")
	e.s3 = costest.New(t, "us-south", "eu-de")
	e.rc = &fakeLookup{
		calls: map[string]int{},
		instances: []ibm.ServiceInstance{
			{Name: "bnk-supply-chain", GUID: "g-supply", CRN: testCOSCRN("g-supply"), ResourceGroupID: "rg-1", State: "active"},
			{Name: "dup", GUID: "g-d1", CRN: testCOSCRN("g-d1"), ResourceGroupID: "rg-1", State: "active"},
			{Name: "dup", GUID: "g-d2", CRN: testCOSCRN("g-d2"), ResourceGroupID: "rg-2", State: "active"},
		},
		groups: []ibm.ResourceGroup{{ID: "rg-1", Name: "default"}, {ID: "rg-2", Name: "work"}},
	}
	newCOSClient = func(ctx context.Context, key, crn, region string) (*cos.Client, error) {
		if key != "test-key" {
			t.Errorf("COS client built with API key %q", key)
		}
		return cos.NewWith(ctx, key, crn, region, cos.Options{EndpointFor: e.s3.Endpoint, TokenURL: e.s3.TokenURL()})
	}
	newIBMLookup = func(*session) (ibmLookup, error) { return e.rc, nil }
	t.Cleanup(func() {
		newCOSClient = cos.New
		newIBMLookup = defaultIBMLookup
	})
	return e
}

// run executes the real root command, returning stdout and stderr apart.
func (e *cosEnv) run(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	root := newRoot()
	var so, se bytes.Buffer
	root.SetOut(&so)
	root.SetErr(&se)
	root.SetIn(strings.NewReader(""))
	root.SetArgs(args)
	err := root.Execute()
	return so.String(), se.String(), err
}

func (e *cosEnv) mustRun(t *testing.T, args ...string) (string, string) {
	t.Helper()
	so, se, err := e.run(t, args...)
	if err != nil {
		t.Fatalf("%v: %v\nstdout:\n%s\nstderr:\n%s", args, err, so, se)
	}
	return so, se
}

func writeTemp(t *testing.T, name string, b []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// No workspace exists and none is created: every cos command works from
// flags and defaults.
func TestCOSInstancesWithoutAWorkspace(t *testing.T) {
	e := newCOSEnv(t)
	so, _ := e.mustRun(t, "cos", "instances")
	lines := strings.Split(strings.TrimSpace(so), "\n")
	if len(lines) != 4 || !strings.HasPrefix(lines[0], "NAME") || !strings.Contains(lines[0], "RESOURCE GROUP") {
		t.Fatalf("table:\n%s", so)
	}
	for _, want := range []string{"bnk-supply-chain  g-supply  default", "g-d2", "work", "active", testCOSCRN("g-d1")} {
		if !strings.Contains(so, want) {
			t.Errorf("table lacks %q:\n%s", want, so)
		}
	}
	if ents, _ := os.ReadDir(e.home); len(ents) != 0 {
		t.Errorf("a workspace command wrote into ROKSBNKARGOCTL_HOME: %v", ents)
	}
}

// The instance is found by name, GUID or CRN, from a flag or the environment;
// a name two instances share is an error that lists both.
func TestCOSInstanceByNameGUIDOrCRN(t *testing.T) {
	e := newCOSEnv(t)
	e.s3.AddBucket(testCOSCRN("g-supply"), "f5-files", "eu-de-smart", nil)
	e.s3.AddBucket(testCOSCRN("g-d2"), "other", "us-south-standard", nil)
	for _, ref := range []string{"bnk-supply-chain", "g-supply", testCOSCRN("g-supply")} {
		so, _ := e.mustRun(t, "cos", "buckets", "--instance", ref)
		if !strings.Contains(so, "f5-files  eu-de   eu-de-smart") || strings.Contains(so, "other") {
			t.Errorf("--instance %s:\n%s", ref, so)
		}
	}
	// The default instance, and the environment.
	if so, _ := e.mustRun(t, "cos", "buckets"); !strings.Contains(so, "f5-files") {
		t.Errorf("default instance:\n%s", so)
	}
	t.Setenv("ROKSBNKARGOCTL_COS_INSTANCE", "g-d2")
	if so, _ := e.mustRun(t, "cos", "buckets"); !strings.Contains(so, "other  us-south  us-south-standard") {
		t.Errorf("instance from the environment:\n%s", so)
	}
	t.Setenv("ROKSBNKARGOCTL_COS_INSTANCE", "")

	_, _, err := e.run(t, "cos", "buckets", "--instance", "dup")
	if err == nil {
		t.Fatal("an ambiguous instance name was accepted")
	}
	for _, want := range []string{"ambiguous", "g-d1", "g-d2", testCOSCRN("g-d2")} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("ambiguous error lacks %q: %v", want, err)
		}
	}
	if _, _, err := e.run(t, "cos", "buckets", "--instance", "absent"); err == nil || !strings.Contains(err.Error(), "absent") {
		t.Errorf("absent instance: %v", err)
	}
}

// A bucket's region comes from its location: an eu-de bucket is read from
// eu-de with cos.region left at us-south. A bucket CRN names its instance.
func TestCOSListFindsTheBucketRegion(t *testing.T) {
	e := newCOSEnv(t)
	e.s3.AddBucket(testCOSCRN("g-supply"), "f5-files", "eu-de-smart", map[string][]byte{
		"f5-far-auth-key.tgz": []byte("tgz"), "subscription.jwt": []byte("a.b.c"), "notes.txt": []byte("hello"),
	})
	e.s3.AddBucket(testCOSCRN("g-d2"), "far-away", "eu-de-standard", map[string][]byte{"x": []byte("1")})

	so, se := e.mustRun(t, "cos", "list", "--bucket", "f5-files")
	for _, want := range []string{"KEY", "f5-far-auth-key.tgz  3", "far-auth", "subscription.jwt     5", "jwt", "notes.txt"} {
		if !strings.Contains(so, want) {
			t.Errorf("list lacks %q:\n%s", want, so)
		}
	}
	if !strings.Contains(se, "f5-files (eu-de): 3 object(s)") {
		t.Errorf("stderr: %s", se)
	}
	n := 0
	for _, r := range e.s3.Served() {
		if strings.Contains(r, " /f5-files") {
			n++
			if !strings.HasPrefix(r, "eu-de ") {
				t.Errorf("bucket request %q not sent to eu-de", r)
			}
		}
	}
	if n == 0 {
		t.Errorf("no bucket requests: %v", e.s3.Served())
	}

	// A bucket CRN names the instance, over the default one.
	so, _ = e.mustRun(t, "cos", "list", "--bucket", testBucketCRN("g-d2", "far-away"))
	if !strings.Contains(so, "x    1 ") {
		t.Errorf("bucket CRN:\n%s", so)
	}
	// ...and an --instance that disagrees with it is an error, not a pick.
	_, _, err := e.run(t, "cos", "list", "--instance", "bnk-supply-chain", "--bucket", testBucketCRN("g-d2", "far-away"))
	if err == nil || !strings.Contains(err.Error(), "belongs to dup (g-d2)") {
		t.Errorf("conflicting --instance and bucket CRN: %v", err)
	}
	if _, _, err := e.run(t, "cos", "list", "--bucket", "nope"); err == nil || !strings.Contains(err.Error(), "no bucket \"nope\"") {
		t.Errorf("missing bucket: %v", err)
	}
}

func TestCOSPublish(t *testing.T) {
	e := newCOSEnv(t)
	farFile := writeTemp(t, "far.tgz", farTarball(t, `{"sa":"x"}`))
	jwtFile := writeTemp(t, "sub.jwt", []byte("h.p.s\n"))
	badJWT := writeTemp(t, "bad.jwt", []byte("not-a-jwt"))

	// Files are checked before anything is sent.
	if _, _, err := e.run(t, "cos", "publish", "--bucket", "new-b", "--far-auth", farFile, "--jwt", badJWT, "--create-bucket"); err == nil || !strings.Contains(err.Error(), "not a JWT") {
		t.Fatalf("bad JWT: %v", err)
	}
	if n := len(e.s3.Requests); n != 0 {
		t.Errorf("%d requests before the files were checked", n)
	}

	// A missing bucket without --create-bucket is an error, and nothing is created.
	_, _, err := e.run(t, "cos", "publish", "--bucket", "new-b", "--far-auth", farFile, "--jwt", jwtFile)
	if err == nil || !strings.Contains(err.Error(), "--create-bucket") {
		t.Fatalf("missing bucket: %v", err)
	}
	if len(e.s3.Created) != 0 || e.s3.Buckets["new-b"] != nil {
		t.Errorf("a bucket was created without --create-bucket: %v", e.s3.Created)
	}

	// --create-bucket --region eu-de creates it there, and the objects land in it.
	_, se := e.mustRun(t, "cos", "publish", "--bucket", "new-b", "--far-auth", farFile, "--jwt", jwtFile, "--create-bucket", "--region", "eu-de")
	if b := e.s3.Buckets["new-b"]; b == nil || b.LocationConstraint != "eu-de-smart" || b.Instance != testCOSCRN("g-supply") {
		t.Fatalf("created bucket %+v", b)
	}
	if !strings.Contains(se, "created bucket new-b in eu-de") {
		t.Errorf("stderr: %s", se)
	}
	if v, ok := e.s3.Object("new-b", "subscription.jwt"); !ok || string(v) != "h.p.s\n" {
		t.Errorf("jwt object %q %v", v, ok)
	}
	if _, ok := e.s3.Object("new-b", "f5-far-auth-key.tgz"); !ok {
		t.Error("FAR object missing")
	}

	// An existing bucket is used in its own region, --region notwithstanding,
	// with the object names from the flags; nothing is created.
	created := len(e.s3.Created)
	_, se = e.mustRun(t, "cos", "publish", "--bucket", "new-b", "--jwt", jwtFile, "--jwt-object", "jwt/custom.jwt", "--create-bucket", "--region", "us-south")
	if len(e.s3.Created) != created {
		t.Error("publish tried to create an existing bucket")
	}
	if _, ok := e.s3.Object("new-b", "jwt/custom.jwt"); !ok {
		t.Error("--jwt-object not honoured")
	}
	if !strings.Contains(se, "already exists in eu-de; --region us-south is ignored") {
		t.Errorf("stderr: %s", se)
	}
}

func TestCOSDelete(t *testing.T) {
	e := newCOSEnv(t)
	objs := func() map[string][]byte {
		return map[string][]byte{"f5-far-auth-key.tgz": []byte("t"), "subscription.jwt": []byte("j"), "keep": []byte("k")}
	}
	e.s3.AddBucket(testCOSCRN("g-supply"), "b", "eu-de-smart", objs())

	// Unconfirmed (no terminal, no --yes): nothing is deleted.
	_, _, err := e.run(t, "cos", "delete", "--bucket", "b")
	if err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("unconfirmed delete: %v", err)
	}
	if len(e.s3.Buckets["b"].Objects) != 3 {
		t.Fatalf("an unconfirmed delete deleted: %v", e.s3.Buckets["b"].Objects)
	}

	// The default is the two F5 objects; others stay.
	_, se := e.mustRun(t, "cos", "delete", "--bucket", "b", "--yes")
	if o := e.s3.Buckets["b"].Objects; len(o) != 1 || o["keep"] == nil {
		t.Errorf("after delete: %v", o)
	}
	if !strings.Contains(se, "deleted b/f5-far-auth-key.tgz") || !strings.Contains(se, "deleted b/subscription.jwt") {
		t.Errorf("stderr: %s", se)
	}

	// Absent objects are success, and need no confirmation.
	_, se = e.mustRun(t, "cos", "delete", "--bucket", "b")
	if !strings.Contains(se, "b/subscription.jwt is not there") || !strings.Contains(se, "nothing to delete in b") {
		t.Errorf("stderr: %s", se)
	}

	// --object picks keys; a mix of present and absent deletes the present one.
	_, _ = e.mustRun(t, "cos", "delete", "--bucket", "b", "--object", "keep", "--object", "never-was", "-y")
	if o := e.s3.Buckets["b"].Objects; len(o) != 0 {
		t.Errorf("after --object delete: %v", o)
	}
	// The object names follow cos.jwt_object from the environment too.
	e.s3.AddBucket(testCOSCRN("g-supply"), "c", "us-south-smart", map[string][]byte{"alt.jwt": []byte("j"), "subscription.jwt": []byte("j")})
	t.Setenv("ROKSBNKARGOCTL_COS_JWT_OBJECT", "alt.jwt")
	e.mustRun(t, "cos", "delete", "--bucket", "c", "--yes")
	if o := e.s3.Buckets["c"].Objects; len(o) != 1 || o["subscription.jwt"] == nil {
		t.Errorf("env object name not used: %v", o)
	}
}

// With a workspace selected its cos.* settings are the defaults, and flags
// override them.
func TestCOSWorkspaceSettingsAreDefaults(t *testing.T) {
	e := newCOSEnv(t)
	writeWorkspace(t, e.home, "ws", "cos: {instance: g-d2, bucket: ws-bucket}\n")
	if err := setCurrent("ws"); err != nil {
		t.Fatal(err)
	}
	e.s3.AddBucket(testCOSCRN("g-d2"), "ws-bucket", "us-south-smart", map[string][]byte{"from-ws": nil})
	e.s3.AddBucket(testCOSCRN("g-supply"), "flag-bucket", "eu-de-smart", map[string][]byte{"from-flag": nil})
	if so, _ := e.mustRun(t, "cos", "list"); !strings.Contains(so, "from-ws") {
		t.Errorf("workspace defaults not used:\n%s", so)
	}
	if so, _ := e.mustRun(t, "cos", "list", "--instance", "bnk-supply-chain", "--bucket", "flag-bucket"); !strings.Contains(so, "from-flag") {
		t.Errorf("flags must override the workspace:\n%s", so)
	}
	// `cos buckets --instance` asks about the instance: a bucket CRN in the
	// workspace does not override (or conflict with) it.
	writeWorkspace(t, e.home, "ws", "cos: {bucket: \""+testBucketCRN("g-d2", "ws-bucket")+"\"}\n")
	if so, _ := e.mustRun(t, "cos", "buckets", "--instance", "g-supply"); !strings.Contains(so, "flag-bucket") || strings.Contains(so, "ws-bucket") {
		t.Errorf("buckets --instance with a bucket CRN in the workspace:\n%s", so)
	}
	// --no-workspace drops back to the defaults (instance bnk-supply-chain, no bucket).
	if _, _, err := e.run(t, "cos", "list", "--no-workspace"); err == nil || !strings.Contains(err.Error(), "cos.bucket is not set") {
		t.Errorf("--no-workspace: %v", err)
	}
}

// The FAR key and JWT are read from the bucket's own region (cos verify and
// install read them this way); when the buckets cannot be listed, the read
// falls back to cos.region instead of failing.
func TestCOSReadsFollowTheBucketRegion(t *testing.T) {
	e := newCOSEnv(t)
	e.s3.AddBucket(testCOSCRN("g-supply"), "eu", "eu-de-smart", map[string][]byte{"subscription.jwt": []byte("a.b.c")})
	e.s3.AddBucket(testCOSCRN("g-supply"), "us", "us-south-smart", map[string][]byte{"subscription.jwt": []byte("d.e.f")})
	s, out, err := probe(t, true, bindCOSTarget, "--bucket", "eu")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if j, err := s.JWT(context.Background()); err != nil || j != "a.b.c" {
		t.Errorf("JWT from eu-de: %q %v", j, err)
	}
	e.s3.ListErr = 403
	s, _, _ = probe(t, true, bindCOSTarget, "--bucket", "us")
	if j, err := s.JWT(context.Background()); err != nil || j != "d.e.f" {
		t.Errorf("JWT with listing denied must fall back to cos.region: %q %v", j, err)
	}
	// The cos commands are strict: an unlocatable bucket is an error.
	if _, _, err := e.run(t, "cos", "list", "--bucket", "us"); err == nil || !strings.Contains(err.Error(), "listing COS buckets") {
		t.Errorf("strict list with listing denied: %v", err)
	}
}
