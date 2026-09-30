// Package costest is a fake IBM Cloud Object Storage for tests: one S3 server
// per region over a shared store, plus the IAM token endpoint, enough of the
// wire protocol for ibm-cos-sdk-go's real request/response path to run end to
// end.
//
// A bucket lives in one region, and only that region's endpoint serves it.
// Any other answers 404 NoSuchBucket, as COS does (seen live: an eu-de bucket
// listed through the us-south endpoint is "NoSuchBucket: The specified bucket
// does not exist", status 404). That is why region discovery matters: an
// idempotent delete through the wrong endpoint reads "absent" and reports
// success while the objects stay, so tests must check the store, not the exit
// status.
package costest

import (
	"encoding/xml"
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

// Bucket is one fake bucket.
type Bucket struct {
	// Instance is the service instance (the ibm-service-instance-id it was
	// created with) whose ListBuckets shows it.
	Instance string
	// LocationConstraint is e.g. "eu-de-smart"; its region serves the bucket.
	LocationConstraint string
	Objects            map[string][]byte
}

// Request is one S3 request the fake served.
type Request struct {
	Region, Method, Path, Query string
	Instance, Authorization     string
}

// Fake is the fake COS.
type Fake struct {
	mu       sync.Mutex
	servers  map[string]*httptest.Server
	Buckets  map[string]*Bucket
	Created  []string // CreateBucket request bodies
	Requests []Request
	// ListErr, when non-zero, is the status every ListBuckets answers.
	ListErr int
}

// New starts one server per region (at least one) and stops them when the
// test ends.
func New(t testing.TB, regions ...string) *Fake {
	t.Helper()
	if len(regions) == 0 {
		regions = []string{"us-south"}
	}
	f := &Fake{servers: map[string]*httptest.Server{}, Buckets: map[string]*Bucket{}}
	for _, r := range regions {
		region := r
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) { f.serve(region, w, req) }))
		t.Cleanup(srv.Close)
		f.servers[region] = srv
	}
	return f
}

// Endpoint is the S3 endpoint for region. A region the fake does not serve
// gets an address nothing listens on.
func (f *Fake) Endpoint(region string) string {
	if s, ok := f.servers[region]; ok {
		return s.URL
	}
	return "http://127.0.0.1:1"
}

// TokenURL is the IAM token endpoint.
func (f *Fake) TokenURL() string {
	names := make([]string, 0, len(f.servers))
	for n := range f.servers {
		names = append(names, n)
	}
	sort.Strings(names)
	return f.servers[names[0]].URL + "/identity/token"
}

// AddBucket creates a bucket directly (no request recorded).
func (f *Fake) AddBucket(instance, name, locationConstraint string, objects map[string][]byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if objects == nil {
		objects = map[string][]byte{}
	}
	f.Buckets[name] = &Bucket{Instance: instance, LocationConstraint: locationConstraint, Objects: objects}
}

// Object returns an object's contents and whether it exists.
func (f *Fake) Object(bucket, key string) ([]byte, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.Buckets[bucket]
	if !ok {
		return nil, false
	}
	v, ok := b.Objects[key]
	return v, ok
}

// Served lists the requests, as "REGION METHOD /path".
func (f *Fake) Served() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.Requests))
	for _, r := range f.Requests {
		out = append(out, r.Region+" "+r.Method+" "+r.Path)
	}
	return out
}

// regionOf mirrors cos.RegionOf without importing it (the fake must not agree
// with the code under test by construction).
func regionOf(lc string) string {
	for _, class := range []string{"-standard", "-vault", "-cold", "-flex", "-smart", "-onerate_active"} {
		if strings.HasSuffix(lc, class) {
			return strings.TrimSuffix(lc, class)
		}
	}
	return lc
}

func xmlError(w http.ResponseWriter, status int, code string) {
	w.WriteHeader(status)
	fmt.Fprintf(w, `<Error><Code>%s</Code><Message>%s</Message></Error>`, code, code)
}

func (f *Fake) serve(region string, w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.URL.Path == "/identity/token" {
		exp := time.Now().Add(time.Hour).Unix()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"access_token":"tok","refresh_token":"r","token_type":"Bearer","expires_in":3600,"expiration":%d}`, exp)
		return
	}
	inst := r.Header.Get("ibm-service-instance-id")
	f.Requests = append(f.Requests, Request{Region: region, Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery, Instance: inst, Authorization: r.Header.Get("Authorization")})
	parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/"), "/", 2)
	name, key := parts[0], ""
	if len(parts) == 2 {
		key = parts[1]
	}

	if name == "" {
		if r.Method != http.MethodGet {
			xmlError(w, http.StatusMethodNotAllowed, "MethodNotAllowed")
			return
		}
		if f.ListErr != 0 {
			xmlError(w, f.ListErr, "AccessDenied")
			return
		}
		_, extended := r.URL.Query()["extended"]
		var names []string
		for n, b := range f.Buckets {
			if b.Instance == inst {
				names = append(names, n)
			}
		}
		sort.Strings(names)
		fmt.Fprint(w, `<ListAllMyBucketsResult><IsTruncated>false</IsTruncated><Buckets>`)
		for _, n := range names {
			lc := ""
			if extended {
				lc = "<LocationConstraint>" + f.Buckets[n].LocationConstraint + "</LocationConstraint>"
			}
			fmt.Fprintf(w, `<Bucket><Name>%s</Name><CreationDate>2026-01-01T00:00:00.000Z</CreationDate>%s</Bucket>`, n, lc)
		}
		fmt.Fprint(w, `</Buckets></ListAllMyBucketsResult>`)
		return
	}

	b, exists := f.Buckets[name]
	if key == "" && r.Method == http.MethodPut {
		body, _ := io.ReadAll(r.Body)
		f.Created = append(f.Created, string(body))
		if exists {
			code := "BucketAlreadyExists"
			if b.Instance == inst {
				code = "BucketAlreadyOwnedByYou"
			}
			xmlError(w, http.StatusConflict, code)
			return
		}
		var cfg struct {
			LocationConstraint string `xml:"LocationConstraint"`
		}
		_ = xml.Unmarshal(body, &cfg)
		if regionOf(cfg.LocationConstraint) != region {
			xmlError(w, http.StatusBadRequest, "InvalidLocationConstraint")
			return
		}
		f.Buckets[name] = &Bucket{Instance: inst, LocationConstraint: cfg.LocationConstraint, Objects: map[string][]byte{}}
		return
	}
	if !exists {
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		xmlError(w, http.StatusNotFound, "NoSuchBucket")
		return
	}
	if regionOf(b.LocationConstraint) != region {
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		xmlError(w, http.StatusNotFound, "NoSuchBucket")
		return
	}
	switch {
	case key == "" && r.Method == http.MethodHead:
	case key == "" && r.Method == http.MethodGet:
		prefix := r.URL.Query().Get("prefix")
		var keys []string
		for k := range b.Objects {
			if strings.HasPrefix(k, prefix) {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		fmt.Fprintf(w, `<ListBucketResult><Name>%s</Name><KeyCount>%d</KeyCount><IsTruncated>false</IsTruncated>`, name, len(keys))
		for _, k := range keys {
			fmt.Fprintf(w, `<Contents><Key>%s</Key><Size>%d</Size><LastModified>2026-09-01T10:00:00.000Z</LastModified></Contents>`, k, len(b.Objects[k]))
		}
		fmt.Fprint(w, `</ListBucketResult>`)
	case key == "" && r.Method == http.MethodDelete:
		delete(f.Buckets, name)
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodPut:
		body, _ := io.ReadAll(r.Body)
		b.Objects[key] = body
	case r.Method == http.MethodHead:
		if v, ok := b.Objects[key]; ok {
			w.Header().Set("Content-Length", fmt.Sprint(len(v)))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	case r.Method == http.MethodGet:
		v, ok := b.Objects[key]
		if !ok {
			xmlError(w, http.StatusNotFound, "NoSuchKey")
			return
		}
		_, _ = w.Write(v)
	case r.Method == http.MethodDelete:
		delete(b.Objects, key)
		w.WriteHeader(http.StatusNoContent)
	default:
		xmlError(w, http.StatusMethodNotAllowed, "MethodNotAllowed")
	}
}
