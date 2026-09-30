// Package cos reads and writes IBM Cloud Object Storage buckets and objects —
// where roksbnkargoctl publishes and discovers the FAR auth tarball and the
// subscription JWT.
//
// It wraps github.com/IBM/ibm-cos-sdk-go (S3-compatible, in-process, no
// shell-outs) with IAM API-key authentication against one COS service
// instance, over the regional public endpoint
// https://s3.<region>.cloud-object-storage.appdomain.cloud.
//
// Buckets are addressed in the client's region only; a bucket that lives in
// another region is reached with a client made for that region (InRegion).
// ListBuckets answers from any region, with each bucket's location, so
// BucketRegion finds the region a bucket must be addressed in: an operator
// never has to know it, and a wrong-region endpoint otherwise fails with an
// error that does not say the region is the problem.
package cos

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/IBM/ibm-cos-sdk-go/aws"
	"github.com/IBM/ibm-cos-sdk-go/aws/awserr"
	"github.com/IBM/ibm-cos-sdk-go/aws/credentials/ibmiam"
	"github.com/IBM/ibm-cos-sdk-go/aws/session"
	"github.com/IBM/ibm-cos-sdk-go/service/s3"
)

const iamTokenURL = "https://iam.cloud.ibm.com/identity/token"

// maxObjectBytes caps GetObject: the objects this tool handles are a small
// tarball and a JWT, so anything larger is a wrong key, not data.
const maxObjectBytes = 64 << 20

// Client is a COS client bound to one service instance and one region.
type Client struct {
	region      string
	instanceCRN string
	apiKey      string
	opts        Options
	s3          *s3.S3
}

// Options points a client somewhere other than IBM Cloud: tests serve S3 and
// the IAM token from local servers. The zero value is IBM Cloud.
type Options struct {
	// EndpointFor returns the S3 endpoint for a region (default Endpoint).
	EndpointFor func(region string) string
	// TokenURL is the IAM token endpoint.
	TokenURL string
}

func (o Options) withDefaults() Options {
	if o.EndpointFor == nil {
		o.EndpointFor = Endpoint
	}
	if o.TokenURL == "" {
		o.TokenURL = iamTokenURL
	}
	return o
}

// Endpoint returns the regional public S3 endpoint for region.
func Endpoint(region string) string {
	return fmt.Sprintf("https://s3.%s.cloud-object-storage.appdomain.cloud", region)
}

// New constructs a client for the COS instance instanceCRN (its CRN, or GUID)
// in region. No network call is made until the first operation.
func New(ctx context.Context, apiKey, instanceCRN, region string) (*Client, error) {
	return NewWith(ctx, apiKey, instanceCRN, region, Options{})
}

// NewWith is New with the endpoints set by o.
func NewWith(_ context.Context, apiKey, instanceCRN, region string, o Options) (*Client, error) {
	switch {
	case strings.TrimSpace(apiKey) == "":
		return nil, errors.New("COS: API key is empty")
	case instanceCRN == "":
		return nil, errors.New("COS: instance CRN is empty")
	case region == "":
		return nil, errors.New("COS: region is empty")
	}
	o = o.withDefaults()
	creds := ibmiam.NewStaticCredentials(aws.NewConfig(), o.TokenURL, apiKey, instanceCRN)
	sess, err := session.NewSession()
	if err != nil {
		return nil, fmt.Errorf("creating COS session: %w", err)
	}
	conf := aws.NewConfig().
		WithRegion(region).
		WithEndpoint(o.EndpointFor(region)).
		WithCredentials(creds).
		WithS3ForcePathStyle(true)
	return &Client{region: region, instanceCRN: instanceCRN, apiKey: apiKey, opts: o, s3: s3.New(sess, conf)}, nil
}

// Region is the client's region.
func (c *Client) Region() string { return c.region }

// InstanceCRN is the service instance the client acts on.
func (c *Client) InstanceCRN() string { return c.instanceCRN }

// InRegion returns a client for the same instance in region (c itself when
// that is already its region).
func (c *Client) InRegion(region string) (*Client, error) {
	if region == c.region {
		return c, nil
	}
	return NewWith(context.Background(), c.apiKey, c.instanceCRN, region, c.opts)
}

// Bucket is one bucket of the instance, with where it lives.
type Bucket struct {
	Name string
	// LocationConstraint is COS's location and storage class, e.g.
	// "us-south-smart" or "eu-standard".
	LocationConstraint string
	// Region is the location without the class: the region (or cross-region
	// or single-site location) whose endpoint serves the bucket.
	Region  string
	Created time.Time
}

// storageClasses are the LocationConstraint suffixes that name a storage class
// rather than a location.
var storageClasses = map[string]bool{
	"standard": true, "vault": true, "cold": true, "flex": true, "smart": true, "onerate_active": true,
}

// RegionOf is the location part of a LocationConstraint: "us-south-smart" is
// us-south, "eu-standard" is eu (cross-region), "ams03-vault" is ams03.
func RegionOf(locationConstraint string) string {
	lc := strings.TrimSpace(locationConstraint)
	if i := strings.LastIndex(lc, "-"); i > 0 && storageClasses[strings.ToLower(lc[i+1:])] {
		return lc[:i]
	}
	return lc
}

// ListBucketsExtended returns every bucket of the instance with its location,
// sorted by name. Any regional endpoint answers for all of the instance's
// buckets, wherever they live.
func (c *Client) ListBucketsExtended(ctx context.Context) ([]Bucket, error) {
	in := &s3.ListBucketsExtendedInput{IBMServiceInstanceId: aws.String(c.instanceCRN)}
	var out []Bucket
	err := c.s3.ListBucketsExtendedPagesWithContext(ctx, in, func(page *s3.ListBucketsExtendedOutput, _ bool) bool {
		for _, b := range page.Buckets {
			lc := aws.StringValue(b.LocationConstraint)
			out = append(out, Bucket{
				Name:               aws.StringValue(b.Name),
				LocationConstraint: lc,
				Region:             RegionOf(lc),
				Created:            aws.TimeValue(b.CreationDate),
			})
		}
		return true
	})
	if err != nil {
		return nil, fmt.Errorf("listing COS buckets: %w", err)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// ErrBucketNotFound is returned by FindBucket for a bucket the instance does
// not list.
var ErrBucketNotFound = errors.New("COS bucket not found")

// FindBucket returns the instance's bucket called name, with its region.
func (c *Client) FindBucket(ctx context.Context, name string) (*Bucket, error) {
	all, err := c.ListBucketsExtended(ctx)
	if err != nil {
		return nil, err
	}
	for i := range all {
		if all[i].Name == name {
			return &all[i], nil
		}
	}
	names := make([]string, 0, len(all))
	for _, b := range all {
		names = append(names, b.Name)
	}
	have := "it has no buckets"
	if len(names) > 0 {
		have = "it has: " + strings.Join(names, ", ")
	}
	return nil, fmt.Errorf("%w: the instance has no bucket %q (%s)", ErrBucketNotFound, name, have)
}

// ParseBucketCRN splits a bucket CRN
// (crn:v1:bluemix:public:cloud-object-storage:global:a/<account>:<instance-guid>:bucket:<name>)
// into its instance GUID and bucket name. ok is false for anything else.
func ParseBucketCRN(ref string) (instanceGUID, bucket string, ok bool) {
	p := strings.Split(strings.TrimSpace(ref), ":")
	if len(p) != 10 || p[0] != "crn" || p[4] != "cloud-object-storage" || p[8] != "bucket" || p[7] == "" || p[9] == "" {
		return "", "", false
	}
	return p[7], p[9], true
}

// ListBuckets returns the instance's bucket names, sorted.
func (c *Client) ListBuckets(ctx context.Context) ([]string, error) {
	out, err := c.s3.ListBucketsWithContext(ctx, &s3.ListBucketsInput{})
	if err != nil {
		return nil, fmt.Errorf("listing COS buckets: %w", err)
	}
	names := make([]string, 0, len(out.Buckets))
	for _, b := range out.Buckets {
		names = append(names, aws.StringValue(b.Name))
	}
	sort.Strings(names)
	return names, nil
}

// EnsureBucket creates bucket (Smart Tier, in the client's region) unless it
// already exists and is ours. created reports whether this call made it.
func (c *Client) EnsureBucket(ctx context.Context, bucket string) (created bool, err error) {
	_, err = c.s3.HeadBucketWithContext(ctx, &s3.HeadBucketInput{Bucket: aws.String(bucket)})
	if err == nil {
		return false, nil
	}
	if !isNotFound(err) {
		return false, fmt.Errorf("checking bucket %q: %w", bucket, err)
	}
	_, err = c.s3.CreateBucketWithContext(ctx, &s3.CreateBucketInput{
		Bucket: aws.String(bucket),
		CreateBucketConfiguration: &s3.CreateBucketConfiguration{
			LocationConstraint: aws.String(c.region + "-smart"),
		},
	})
	if err != nil {
		var ae awserr.Error
		if errors.As(err, &ae) && ae.Code() == s3.ErrCodeBucketAlreadyOwnedByYou {
			return false, nil
		}
		return false, fmt.Errorf("creating bucket %q in %s: %w", bucket, c.region, err)
	}
	return true, nil
}

// Object is one listed object.
type Object struct {
	Key      string
	Size     int64
	Modified time.Time
}

// ListObjects returns every object in bucket whose key starts with prefix.
func (c *Client) ListObjects(ctx context.Context, bucket, prefix string) ([]Object, error) {
	in := &s3.ListObjectsV2Input{Bucket: aws.String(bucket)}
	if prefix != "" {
		in.Prefix = aws.String(prefix)
	}
	var out []Object
	err := c.s3.ListObjectsV2PagesWithContext(ctx, in, func(page *s3.ListObjectsV2Output, _ bool) bool {
		for _, o := range page.Contents {
			out = append(out, Object{
				Key:      aws.StringValue(o.Key),
				Size:     aws.Int64Value(o.Size),
				Modified: aws.TimeValue(o.LastModified),
			})
		}
		return true
	})
	if err != nil {
		return nil, fmt.Errorf("listing objects in %q: %w", bucket, err)
	}
	return out, nil
}

// ErrNotFound is returned by GetObject for a missing bucket or key.
var ErrNotFound = errors.New("COS object not found")

// GetObject returns an object's contents.
func (c *Client) GetObject(ctx context.Context, bucket, key string) ([]byte, error) {
	out, err := c.s3.GetObjectWithContext(ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
	if err != nil {
		if isNotFound(err) {
			return nil, fmt.Errorf("%w: %s/%s", ErrNotFound, bucket, key)
		}
		return nil, fmt.Errorf("getting %s/%s: %w", bucket, key, err)
	}
	defer out.Body.Close()
	b, err := io.ReadAll(io.LimitReader(out.Body, maxObjectBytes+1))
	if err != nil {
		return nil, fmt.Errorf("reading %s/%s: %w", bucket, key, err)
	}
	if len(b) > maxObjectBytes {
		return nil, fmt.Errorf("%s/%s is larger than %d bytes", bucket, key, maxObjectBytes)
	}
	return b, nil
}

// PutObject writes an object, replacing any existing one.
func (c *Client) PutObject(ctx context.Context, bucket, key string, data []byte) error {
	_, err := c.s3.PutObjectWithContext(ctx, &s3.PutObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
		Body:   bytes.NewReader(data),
	})
	if err != nil {
		return fmt.Errorf("putting %s/%s: %w", bucket, key, err)
	}
	return nil
}

// DeleteObject deletes an object. Deleting a missing key succeeds (S3
// semantics).
func (c *Client) DeleteObject(ctx context.Context, bucket, key string) error {
	_, err := c.s3.DeleteObjectWithContext(ctx, &s3.DeleteObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
	if err != nil && !isNotFound(err) {
		return fmt.Errorf("deleting %s/%s: %w", bucket, key, err)
	}
	return nil
}

// Exists reports whether bucket holds key.
func (c *Client) Exists(ctx context.Context, bucket, key string) (bool, error) {
	_, err := c.s3.HeadObjectWithContext(ctx, &s3.HeadObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
	switch {
	case err == nil:
		return true, nil
	case isNotFound(err):
		return false, nil
	}
	return false, fmt.Errorf("checking %s/%s: %w", bucket, key, err)
}

// isNotFound reports an HTTP 404. NoSuchKey, NoSuchBucket and HeadBucket's
// body-less "NotFound" all arrive as a RequestFailure with status 404.
func isNotFound(err error) bool {
	var rf awserr.RequestFailure
	return errors.As(err, &rf) && rf.StatusCode() == http.StatusNotFound
}
