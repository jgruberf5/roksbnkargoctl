package cos

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeS3 is a minimal path-style S3 + IAM token server: enough of the wire
// protocol for the SDK's real request/response path to run end to end.
type fakeS3 struct {
	mu        sync.Mutex
	buckets   map[string]map[string][]byte
	created   []string // LocationConstraint bodies of CreateBucket
	authSeen  []string
	instances []string
}

func newFakeS3(t *testing.T) (*fakeS3, *Client) {
	t.Helper()
	f := &fakeS3{buckets: map[string]map[string][]byte{"existing": {}}}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	c, err := newClient(context.Background(), "key", "crn:v1:bluemix:public:cloud-object-storage:global:a/acct:guid::", "us-south", srv.URL, srv.URL+"/identity/token")
	if err != nil {
		t.Fatal(err)
	}
	return f, c
}

func (f *fakeS3) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.URL.Path == "/identity/token" {
		exp := time.Now().Add(time.Hour).Unix()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"access_token":"tok","refresh_token":"r","token_type":"Bearer","expires_in":3600,"expiration":%d}`, exp)
		return
	}
	f.authSeen = append(f.authSeen, r.Header.Get("Authorization"))
	f.instances = append(f.instances, r.Header.Get("ibm-service-instance-id"))
	parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/"), "/", 2)
	bucket := parts[0]
	key := ""
	if len(parts) == 2 {
		key = parts[1]
	}
	notFound := func(code string) {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprintf(w, `<Error><Code>%s</Code><Message>nope</Message></Error>`, code)
	}
	switch {
	case bucket == "" && r.Method == http.MethodGet:
		var names []string
		for b := range f.buckets {
			names = append(names, b)
		}
		sort.Strings(names)
		fmt.Fprint(w, `<ListAllMyBucketsResult><Buckets>`)
		for _, n := range names {
			fmt.Fprintf(w, `<Bucket><Name>%s</Name><CreationDate>2026-01-01T00:00:00.000Z</CreationDate></Bucket>`, n)
		}
		fmt.Fprint(w, `</Buckets></ListAllMyBucketsResult>`)
	case key == "" && r.Method == http.MethodHead:
		if _, ok := f.buckets[bucket]; !ok {
			w.WriteHeader(http.StatusNotFound)
		}
	case key == "" && r.Method == http.MethodPut:
		body, _ := io.ReadAll(r.Body)
		f.created = append(f.created, string(body))
		f.buckets[bucket] = map[string][]byte{}
	case key == "" && r.Method == http.MethodGet:
		objs, ok := f.buckets[bucket]
		if !ok {
			notFound("NoSuchBucket")
			return
		}
		prefix := r.URL.Query().Get("prefix")
		var keys []string
		for k := range objs {
			if strings.HasPrefix(k, prefix) {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		fmt.Fprintf(w, `<ListBucketResult><Name>%s</Name><KeyCount>%d</KeyCount><IsTruncated>false</IsTruncated>`, bucket, len(keys))
		for _, k := range keys {
			fmt.Fprintf(w, `<Contents><Key>%s</Key><Size>%d</Size><LastModified>2026-09-01T10:00:00.000Z</LastModified></Contents>`, k, len(objs[k]))
		}
		fmt.Fprint(w, `</ListBucketResult>`)
	case r.Method == http.MethodPut:
		body, _ := io.ReadAll(r.Body)
		f.buckets[bucket][key] = body
	case r.Method == http.MethodGet:
		b, ok := f.buckets[bucket][key]
		if !ok {
			notFound("NoSuchKey")
			return
		}
		_, _ = w.Write(b)
	case r.Method == http.MethodDelete:
		delete(f.buckets[bucket], key)
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func TestObjectRoundTrip(t *testing.T) {
	f, c := newFakeS3(t)
	ctx := context.Background()

	created, err := c.EnsureBucket(ctx, "bnk")
	if err != nil || !created {
		t.Fatalf("EnsureBucket new: created=%v err=%v", created, err)
	}
	if len(f.created) != 1 || !strings.Contains(f.created[0], "<LocationConstraint>us-south-smart</LocationConstraint>") {
		t.Errorf("create body = %q", f.created)
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
	for i, a := range f.authSeen {
		if a != "Bearer tok" {
			t.Fatalf("request %d Authorization = %q", i, a)
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
