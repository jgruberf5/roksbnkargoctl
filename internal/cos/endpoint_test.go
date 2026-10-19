package cos

import (
	"context"
	"testing"
)

func TestEndpointFunc(t *testing.T) {
	for _, tc := range []struct {
		kind, region, want string
	}{
		{"", "eu-fr2", "https://s3.eu-fr2.cloud-object-storage.appdomain.cloud"},
		{"public", "eu-fr2", "https://s3.eu-fr2.cloud-object-storage.appdomain.cloud"},
		{"private", "eu-fr2", "https://s3.private.eu-fr2.cloud-object-storage.appdomain.cloud"},
		{"direct", "eu-fr2", "https://s3.direct.eu-fr2.cloud-object-storage.appdomain.cloud"},
		{"direct", "us-south", "https://s3.direct.us-south.cloud-object-storage.appdomain.cloud"},
		// A URL is used as is, whatever the region.
		{"https://cos-vpe.example.internal", "us-south", "https://cos-vpe.example.internal"},
		{"https://cos-vpe.example.internal:8443", "eu-fr2", "https://cos-vpe.example.internal:8443"},
	} {
		f, err := EndpointFunc(tc.kind)
		if err != nil {
			t.Errorf("EndpointFunc(%q): %v", tc.kind, err)
			continue
		}
		if got := f(tc.region); got != tc.want {
			t.Errorf("EndpointFunc(%q)(%q) = %q, want %q", tc.kind, tc.region, got, tc.want)
		}
	}
	for _, bad := range []string{"dirct", "Direct", "http://cos.example", "https://", "s3.direct.eu-fr2.cloud-object-storage.appdomain.cloud"} {
		if _, err := EndpointFunc(bad); err == nil {
			t.Errorf("EndpointFunc(%q) accepted", bad)
		}
	}
}

// A bucket in another region is read through a client InRegion makes: it must
// keep the endpoint kind, or a direct-only network fails on the second hop
// (listing buckets works in cos.region, reading the bucket does not).
func TestInRegionKeepsTheEndpointKind(t *testing.T) {
	ep, err := EndpointFunc(EndpointDirect)
	if err != nil {
		t.Fatal(err)
	}
	c, err := NewWith(context.Background(), "key", testInstance, "us-south", Options{EndpointFor: ep})
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Endpoint(); got != "https://s3.direct.us-south.cloud-object-storage.appdomain.cloud" {
		t.Errorf("us-south client endpoint = %q", got)
	}
	rc, err := c.InRegion("eu-fr2")
	if err != nil {
		t.Fatal(err)
	}
	if got := rc.Endpoint(); got != "https://s3.direct.eu-fr2.cloud-object-storage.appdomain.cloud" {
		t.Errorf("eu-fr2 client endpoint = %q", got)
	}
}

func TestIsUnreachable(t *testing.T) {
	f, c := newFakeS3(t, "us-south")
	ctx := context.Background()

	// COS answered (a 404 for a bucket it does not have): reachable.
	_, err := c.ListObjects(ctx, "no-such-bucket", "")
	if err == nil {
		t.Fatal("listing a missing bucket succeeded")
	}
	if IsUnreachable(err) {
		t.Errorf("an answer from COS reads as unreachable: %v", err)
	}

	// Nothing listens at the endpoint of a region the fake does not serve.
	off, err := NewWith(ctx, "key", testInstance, "eu-fr2", Options{EndpointFor: f.Endpoint, TokenURL: f.TokenURL()})
	if err != nil {
		t.Fatal(err)
	}
	_, err = off.ListObjects(ctx, "existing", "")
	if err == nil {
		t.Fatal("listing through a dead endpoint succeeded")
	}
	if !IsUnreachable(err) {
		t.Errorf("a request that never reached COS does not read as unreachable: %v", err)
	}
}
