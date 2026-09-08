package storage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"strings"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/sajanv88/bankstmt-analyzer/internal/config"
)

// maxObjectBytes is the ceiling on a single stored object. It sits well
// above the API's own 20 MB per-file limit so that it never rejects an
// upload the API accepted; it exists to bound Put's buffer against a
// caller that does not impose a limit of its own.
const maxObjectBytes = 256 << 20

// S3 is a BlobStore backed by an S3-compatible object store: MinIO, AWS
// S3, or anything else speaking the same API.
//
// This is the implementation to use whenever the API and the worker run as
// separate processes. They reach the same bucket over the network, so
// neither needs to see the other's filesystem — which the local
// implementation cannot offer without a shared ReadWriteMany volume.
type S3 struct {
	client *s3.Client
	bucket string
	prefix string
	// maxObjectBytes bounds how much of an object Put will buffer. Uploads
	// are already capped well below it by the API; this is the backstop.
	maxObjectBytes int64
}

// NewS3 builds a client for cfg and verifies the bucket is reachable, so a
// misconfigured endpoint or a missing bucket fails at startup rather than
// on the first upload.
func NewS3(ctx context.Context, cfg config.S3Config) (*S3, error) {
	if strings.TrimSpace(cfg.Bucket) == "" {
		return nil, errors.New("storage: S3 bucket must not be empty")
	}

	awsCfg, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithRegion(cfg.Region),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(
			cfg.AccessKeyID, cfg.SecretAccessKey, "",
		)),
		awsconfig.WithHTTPClient(&http.Client{Timeout: cfg.Timeout}),
	)
	if err != nil {
		return nil, fmt.Errorf("storage: load AWS configuration: %w", err)
	}

	client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		if endpoint := strings.TrimSpace(cfg.Endpoint); endpoint != "" {
			o.BaseEndpoint = &endpoint
		}
		// MinIO and most other S3-compatible stores serve buckets as a
		// path segment rather than a subdomain.
		o.UsePathStyle = cfg.UsePathStyle
	})

	store := &S3{
		client:         client,
		bucket:         cfg.Bucket,
		prefix:         strings.Trim(strings.TrimSpace(cfg.Prefix), "/"),
		maxObjectBytes: maxObjectBytes,
	}
	if err := store.checkBucket(ctx); err != nil {
		return nil, err
	}
	return store, nil
}

// Bucket returns the bucket the store writes to.
func (s *S3) Bucket() string { return s.bucket }

// checkBucket confirms the bucket exists and the credentials can see it.
func (s *S3) checkBucket(ctx context.Context) error {
	if _, err := s.client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: &s.bucket}); err != nil {
		return fmt.Errorf("storage: bucket %q is not reachable: %w", s.bucket, err)
	}
	return nil
}

// Put writes r to the bucket as a single object.
//
// The body is read into memory first, deliberately. Request signing needs
// either a seekable payload or a known length, and handing the SDK a plain
// reader makes it buffer anyway — doing it here keeps the memory bound
// explicit and yields the exact byte count the caller needs to enforce the
// per-file limit. Uploads are capped at 20 MB by default and S3 accepts a
// single part up to 5 GB, so no multipart machinery is involved.
func (s *S3) Put(ctx context.Context, key string, r io.Reader) (int64, error) {
	objectKey, err := s.objectKey(key)
	if err != nil {
		return 0, err
	}

	// One byte past the cap, so an oversized body is reported rather than
	// silently truncated.
	body, err := io.ReadAll(io.LimitReader(r, s.maxObjectBytes+1))
	if err != nil {
		return 0, fmt.Errorf("storage: put %q: %w", key, err)
	}
	if int64(len(body)) > s.maxObjectBytes {
		return 0, fmt.Errorf("storage: put %q: object exceeds the %d byte limit", key, s.maxObjectBytes)
	}

	size := int64(len(body))
	if _, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        &s.bucket,
		Key:           &objectKey,
		Body:          bytes.NewReader(body),
		ContentLength: &size,
	}); err != nil {
		return 0, fmt.Errorf("storage: put %q: %w", key, err)
	}
	return size, nil
}

// Get opens the object at key. The caller closes the returned reader.
func (s *S3) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	objectKey, err := s.objectKey(key)
	if err != nil {
		return nil, err
	}

	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: &s.bucket,
		Key:    &objectKey,
	})
	if err != nil {
		if isNotFound(err) {
			return nil, fmt.Errorf("storage: get %q: %w", key, ErrNotFound)
		}
		return nil, fmt.Errorf("storage: get %q: %w", key, err)
	}
	return out.Body, nil
}

// Delete removes the object at key.
//
// S3's DeleteObject succeeds whether or not the key existed, but the
// BlobStore contract distinguishes the two, so the object is checked
// first. That costs a second round trip on a path that only runs when an
// upload is being cleaned up after a rejection, and it keeps both
// implementations behaving identically.
func (s *S3) Delete(ctx context.Context, key string) error {
	objectKey, err := s.objectKey(key)
	if err != nil {
		return err
	}

	if _, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: &s.bucket,
		Key:    &objectKey,
	}); err != nil {
		if isNotFound(err) {
			return fmt.Errorf("storage: delete %q: %w", key, ErrNotFound)
		}
		return fmt.Errorf("storage: delete %q: %w", key, err)
	}

	if _, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: &s.bucket,
		Key:    &objectKey,
	}); err != nil {
		return fmt.Errorf("storage: delete %q: %w", key, err)
	}
	return nil
}

// objectKey validates a store key and applies the configured prefix.
//
// Object stores have no directories to escape, but the same keys are
// accepted by the local implementation, so they are validated the same way
// in both: a key that one store rejects must not be silently accepted by
// the other.
func (s *S3) objectKey(key string) (string, error) {
	clean, err := validateKey(key)
	if err != nil {
		return "", err
	}
	if s.prefix == "" {
		return clean, nil
	}
	return path.Join(s.prefix, clean), nil
}

// isNotFound reports whether err is the object store saying the key or
// bucket does not exist, in any of the shapes it says it.
func isNotFound(err error) bool {
	var noSuchKey *types.NoSuchKey
	if errors.As(err, &noSuchKey) {
		return true
	}
	var notFound *types.NotFound
	if errors.As(err, &notFound) {
		return true
	}
	// HeadObject reports a missing key as a bare 404 with no typed error,
	// because HEAD responses carry no body for the SDK to unmarshal.
	var responseError interface {
		HTTPStatusCode() int
	}
	if errors.As(err, &responseError) {
		return responseError.HTTPStatusCode() == http.StatusNotFound
	}
	return false
}

// S3 satisfies BlobStore.
var _ BlobStore = (*S3)(nil)
