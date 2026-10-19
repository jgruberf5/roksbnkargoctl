package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/jgruberf5/roksbnkargoctl/internal/config"
	"github.com/jgruberf5/roksbnkargoctl/internal/cos"
)

// recordEndpoints wraps the fake COS client builder and records the
// cos.endpoint every client is built with.
func recordEndpoints(t *testing.T) *[]string {
	t.Helper()
	var got []string
	fake := newCOSClient
	newCOSClient = func(ctx context.Context, key, crn, region, endpoint string) (*cos.Client, error) {
		got = append(got, endpoint)
		return fake(ctx, key, crn, region, endpoint)
	}
	return &got
}

// cos.endpoint reaches the client from the workspace, the environment and the
// flag, in that order of precedence; unset, it is public.
func TestCOSEndpointReachesTheClient(t *testing.T) {
	e := newCOSEnv(t)
	e.s3.AddBucket(testCOSCRN("g-supply"), "f5-files", "eu-de-smart", map[string][]byte{"x": nil})
	got := recordEndpoints(t)
	last := func() string {
		if len(*got) == 0 {
			t.Fatal("no COS client was built")
		}
		return (*got)[len(*got)-1]
	}

	e.mustRun(t, "cos", "list", "--bucket", "f5-files")
	if ep := last(); ep != "public" {
		t.Errorf("default endpoint = %q, want public", ep)
	}

	writeWorkspace(t, e.home, "ws", "cos: {bucket: f5-files, endpoint: direct}\n")
	if err := setCurrent("ws"); err != nil {
		t.Fatal(err)
	}
	e.mustRun(t, "cos", "list")
	if ep := last(); ep != "direct" {
		t.Errorf("workspace cos.endpoint direct reached the client as %q", ep)
	}

	t.Setenv("ROKSBNKARGOCTL_COS_ENDPOINT", "private")
	e.mustRun(t, "cos", "list")
	if ep := last(); ep != "private" {
		t.Errorf("ROKSBNKARGOCTL_COS_ENDPOINT=private reached the client as %q", ep)
	}

	e.mustRun(t, "cos", "list", "--endpoint", "https://cos-vpe.example.internal")
	if ep := last(); ep != "https://cos-vpe.example.internal" {
		t.Errorf("--endpoint reached the client as %q", ep)
	}

	// The fake above ignores the endpoint: the product's builder must use it,
	// and refuse a value it cannot map, before any request.
	for kind, want := range map[string]string{
		"public":                           "https://s3.eu-fr2.cloud-object-storage.appdomain.cloud",
		"direct":                           "https://s3.direct.eu-fr2.cloud-object-storage.appdomain.cloud",
		"https://cos-vpe.example.internal": "https://cos-vpe.example.internal",
	} {
		cc, err := defaultCOSClient(context.Background(), "key", testCOSCRN("g-supply"), "eu-fr2", kind)
		if err != nil {
			t.Errorf("endpoint %s: %v", kind, err)
		} else if cc.Endpoint() != want {
			t.Errorf("endpoint %s: client sends to %s, want %s", kind, cc.Endpoint(), want)
		}
	}
	if _, err := defaultCOSClient(context.Background(), "key", testCOSCRN("g-supply"), "us-south", "dirct"); err == nil || !strings.Contains(err.Error(), "dirct") {
		t.Errorf("endpoint dirct: %v", err)
	}
}

// The commands that read the FAR key outside `cos` take the endpoint too.
func TestCOSEndpointFlagOnEveryCOSReader(t *testing.T) {
	for _, args := range [][]string{
		{"cos", "list"}, {"cos", "buckets"}, {"cos", "publish"}, {"cos", "delete"}, {"cos", "verify"},
	} {
		if !commandHasFlag(t, args, "endpoint") {
			t.Errorf("%s has no --endpoint", strings.Join(args, " "))
		}
	}
	for _, args := range [][]string{{"registry", "bom"}, {"registry", "replicate"}, {"registry", "verify"}, {"flp", "up"}} {
		if !commandHasFlag(t, args, "cos-endpoint") {
			t.Errorf("%s has no --cos-endpoint", strings.Join(args, " "))
		}
	}
}

func commandHasFlag(t *testing.T, path []string, flag string) bool {
	t.Helper()
	cmd, _, err := newRoot().Find(path)
	if err != nil {
		t.Fatalf("%v: %v", path, err)
	}
	return cmd.Flags().Lookup(flag) != nil
}

// config validates cos.endpoint by its own rule (it does not import the COS
// SDK): hold it to exactly what the client accepts.
func TestCOSEndpointValuesAgree(t *testing.T) {
	for _, v := range []string{
		"", "public", "private", "direct", "Direct", "dirct", "vpe",
		"https://cos-vpe.example.internal", "https://10.0.0.5:8443", "https://",
		"http://cos.example", "s3.direct.eu-fr2.cloud-object-storage.appdomain.cloud",
	} {
		_, err := cos.EndpointFunc(v)
		if cfgOK, cosOK := config.ValidCOSEndpoint(v), err == nil; cfgOK != cosOK {
			t.Errorf("%q: config accepts=%v, cos accepts=%v", v, cfgOK, cosOK)
		}
	}
}

// init's COS check names cos.endpoint when the request never reached COS (the
// customer's case: the public name does not resolve behind a VPE), and not
// when COS answered.
func TestResolveCOSNamesTheEndpointWhenUnreachable(t *testing.T) {
	e := newCOSEnv(t)
	e.s3.AddBucket(testCOSCRN("g-supply"), "f5-files", "us-south-smart", map[string][]byte{"f5-far-auth-key.tgz": nil})
	c := &config.Config{COS: config.COS{Bucket: "f5-files"}}
	c.Defaults("w")

	fake := newCOSClient
	newCOSClient = func(ctx context.Context, key, crn, region, endpoint string) (*cos.Client, error) {
		return cos.NewWith(ctx, key, crn, region, cos.Options{
			EndpointFor: func(string) string { return "http://127.0.0.1:1" }, TokenURL: e.s3.TokenURL()})
	}
	s, _ := initSession(c)
	err := resolveCOS(context.Background(), s, &config.Resolved{})
	if err == nil || !strings.Contains(err.Error(), `cos.endpoint "public"`) || !strings.Contains(err.Error(), "private or direct") {
		t.Errorf("unreachable COS: %v", err)
	}

	// COS answers (the JWT is missing): no endpoint advice.
	newCOSClient = fake
	s, _ = initSession(c)
	err = resolveCOS(context.Background(), s, &config.Resolved{})
	if err == nil || !strings.Contains(err.Error(), `no object "subscription.jwt"`) || strings.Contains(err.Error(), "cos.endpoint") {
		t.Errorf("reachable COS: %v", err)
	}
}
