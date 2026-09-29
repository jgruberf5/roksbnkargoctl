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
// another region is reached with a client made for that region.
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
	s3          *s3.S3
}

// Endpoint returns the regional public S3 endpoint for region.
func Endpoint(region string) string {
	return fmt.Sprintf("https://s3.%s.cloud-object-storage.appdomain.cloud", region)
}

// New constructs a client for the COS instance instanceCRN (its CRN, or GUID)
// in region. No network call is made until the first operation.
func New(ctx context.Context, apiKey, instanceCRN, region string) (*Client, error) {
	return newClient(ctx, apiKey, instanceCRN, region, Endpoint(region), iamTokenURL)
}

// newClient is New with the S3 endpoint and IAM token URL injectable, so tests
// can serve both from an httptest server.
func newClient(_ context.Context, apiKey, instanceCRN, region, endpoint, tokenURL string) (*Client, error) {
	switch {
	case strings.TrimSpace(apiKey) == "":
		return nil, errors.New("COS: API key is empty")
	case instanceCRN == "":
		return nil, errors.New("COS: instance CRN is empty")
	case region == "":
		return nil, errors.New("COS: region is empty")
	}
	creds := ibmiam.NewStaticCredentials(aws.NewConfig(), tokenURL, apiKey, instanceCRN)
	sess, err := session.NewSession()
	if err != nil {
		return nil, fmt.Errorf("creating COS session: %w", err)
	}
	conf := aws.NewConfig().
		WithRegion(region).
		WithEndpoint(endpoint).
		WithCredentials(creds).
		WithS3ForcePathStyle(true)
	return &Client{region: region, instanceCRN: instanceCRN, s3: s3.New(sess, conf)}, nil
}

// Region is the client's region.
func (c *Client) Region() string { return c.region }

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

// isNotFound reports an HTTP 404. NoSuchKey, NoSuchBucket and HeadBucket's
// body-less "NotFound" all arrive as a RequestFailure with status 404.
func isNotFound(err error) bool {
	var rf awserr.RequestFailure
	return errors.As(err, &rf) && rf.StatusCode() == http.StatusNotFound
}
