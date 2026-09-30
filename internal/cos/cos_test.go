package cos

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/jgruberf5/roksbnkargoctl/internal/cos/costest"
)

const testInstance = "crn:v1:bluemix:public:cloud-object-storage:global:a/acct:guid::"

// newFakeS3 is a fake COS serving regions (us-south when none), with a bucket
// "existing" in us-south, and a client for it in us-south.
func newFakeS3(t *testing.T, regions ...string) (*costest.Fake, *Client) {
	t.Helper()
	f := costest.New(t, regions...)
	f.AddBucket(testInstance, "existing", "us-south-smart", nil)
	c, err := NewWith(context.Background(), "key", testInstance, "us-south", Options{EndpointFor: f.Endpoint, TokenURL: f.TokenURL()})
	if err != nil {
		t.Fatal(err)
	}
	return f, c
}

func TestObjectRoundTrip(t *testing.T) {
	f, c := newFakeS3(t)
	ctx := context.Background()

	created, err := c.EnsureBucket(ctx, "bnk")
	if err != nil || !created {
		t.Fatalf("EnsureBucket new: created=%v err=%v", created, err)
	}
	if len(f.Created) != 1 || !strings.Contains(f.Created[0], "<LocationConstraint>us-south-smart</LocationConstraint>") {
		t.Errorf("create body = %q", f.Created)
	}
	if created, err := c.EnsureBucket(ctx, "existing"); err != nil || created {
		t.Errorf("EnsureBucket existing: created=%v err=%v", created, err)
	}

	payload := []byte("eyJhbGciOi.jwt.sig")
	if err := c.PutObject(ctx, "bnk", "far/jwt.txt", payload); err != nil {
		t.Fatal(err)
	}
	if err := c.PutObject(ctx, "bnk", "other/x", []byte("x")); err != nil {
		t.Fatal(err)
	}
	got, err := c.GetObject(ctx, "bnk", "far/jwt.txt")
	if err != nil || string(got) != string(payload) {
		t.Fatalf("GetObject = %q, %v", got, err)
	}
	objs, err := c.ListObjects(ctx, "bnk", "far/")
	if err != nil {
		t.Fatal(err)
	}
	if len(objs) != 1 || objs[0].Key != "far/jwt.txt" || objs[0].Size != int64(len(payload)) || objs[0].Modified.IsZero() {
		t.Errorf("ListObjects = %+v", objs)
	}
	buckets, err := c.ListBuckets(ctx)
	if err != nil || strings.Join(buckets, ",") != "bnk,existing" {
		t.Errorf("ListBuckets = %v, %v", buckets, err)
	}
	if err := c.DeleteObject(ctx, "bnk", "far/jwt.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.GetObject(ctx, "bnk", "far/jwt.txt"); !errors.Is(err, ErrNotFound) {
		t.Errorf("after delete err = %v, want ErrNotFound", err)
	}
	for i, r := range f.Requests {
		if r.Authorization != "Bearer tok" {
			t.Fatalf("request %d Authorization = %q", i, r.Authorization)
		}
	}
}

func TestNewValidates(t *testing.T) {
	ctx := context.Background()
	for _, tc := range [][3]string{{"", "crn", "us-south"}, {"k", "", "us-south"}, {"k", "crn", ""}} {
		if _, err := New(ctx, tc[0], tc[1], tc[2]); err == nil {
			t.Errorf("New%v accepted", tc)
		}
	}
	if got := Endpoint("eu-de"); got != "https://s3.eu-de.cloud-object-storage.appdomain.cloud" {
		t.Errorf("Endpoint = %s", got)
	}
}

func TestRegionOf(t *testing.T) {
	for in, want := range map[string]string{
		"us-south-smart": "us-south", "eu-de-standard": "eu-de", "us-standard": "us", "eu-cold": "eu",
		"ams03-vault": "ams03", "jp-tok-flex": "jp-tok", "au-syd-onerate_active": "au-syd",
		"us-south": "us-south", "ca-tor": "ca-tor", "": "", " br-sao-smart ": "br-sao",
	} {
		if got := RegionOf(in); got != want {
			t.Errorf("RegionOf(%q) = %q, want %q", in, got, want)
		}
	}
}

// A bucket in another region is found from the us-south endpoint, with its
// region; a client for that region then reaches it, and the us-south client
// does not (COS, and the fake, answer a wrong-region request NoSuchBucket).
func TestFindBucketAndInRegion(t *testing.T) {
	f, c := newFakeS3(t, "us-south", "eu-de")
	f.AddBucket(testInstance, "far", "eu-de-smart", map[string][]byte{"k": []byte("v")})
	f.AddBucket("someone-else", "theirs", "us-south-smart", nil)
	ctx := context.Background()

	all, err := c.ListBucketsExtended(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, b := range all {
		got = append(got, b.Name+"@"+b.Region+"/"+b.LocationConstraint)
	}
	if strings.Join(got, " ") != "existing@us-south/us-south-smart far@eu-de/eu-de-smart" {
		t.Errorf("buckets %v", got)
	}
	b, err := c.FindBucket(ctx, "far")
	if err != nil || b.Region != "eu-de" {
		t.Fatalf("FindBucket: %+v %v", b, err)
	}
	if _, err := c.GetObject(ctx, "far", "k"); !errors.Is(err, ErrNotFound) {
		t.Errorf("the us-south client must not reach an eu-de bucket: %v", err)
	}
	eu, err := c.InRegion(b.Region)
	if err != nil || eu.Region() != "eu-de" || eu.InstanceCRN() != testInstance {
		t.Fatalf("InRegion: %v", err)
	}
	if v, err := eu.GetObject(ctx, "far", "k"); err != nil || string(v) != "v" {
		t.Errorf("eu-de client: %q %v", v, err)
	}
	if same, _ := c.InRegion("us-south"); same != c {
		t.Error("InRegion(own region) must return the client itself")
	}
	_, err = c.FindBucket(ctx, "theirs")
	if !errors.Is(err, ErrBucketNotFound) || !strings.Contains(err.Error(), "existing, far") {
		t.Errorf("another instance's bucket: %v", err)
	}
	for _, r := range f.Requests {
		if r.Method == http.MethodGet && r.Path == "/" && r.Instance != testInstance {
			t.Errorf("ListBuckets sent instance %q", r.Instance)
		}
	}
}

func TestExists(t *testing.T) {
	f, c := newFakeS3(t)
	f.AddBucket(testInstance, "b", "us-south-smart", map[string][]byte{"here": []byte("x")})
	ctx := context.Background()
	if ok, err := c.Exists(ctx, "b", "here"); !ok || err != nil {
		t.Errorf("here: %v %v", ok, err)
	}
	if ok, err := c.Exists(ctx, "b", "gone"); ok || err != nil {
		t.Errorf("gone: %v %v", ok, err)
	}
}

func TestParseBucketCRN(t *testing.T) {
	g, b, ok := ParseBucketCRN("crn:v1:bluemix:public:cloud-object-storage:global:a/acct:1234-guid:bucket:my-bucket")
	if !ok || g != "1234-guid" || b != "my-bucket" {
		t.Errorf("got %q %q %v", g, b, ok)
	}
	for _, bad := range []string{
		"my-bucket",
		"crn:v1:bluemix:public:cloud-object-storage:global:a/acct:1234-guid::",
		"crn:v1:bluemix:public:kms:global:a/acct:1234-guid:bucket:my-bucket",
		"crn:v1:bluemix:public:cloud-object-storage:global:a/acct::bucket:my-bucket",
		"crn:v1:bluemix:public:cloud-object-storage:global:a/acct:g:object:my-bucket",
	} {
		if _, _, ok := ParseBucketCRN(bad); ok {
			t.Errorf("%q parsed as a bucket CRN", bad)
		}
	}
}
