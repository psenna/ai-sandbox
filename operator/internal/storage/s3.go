package storage

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"sort"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	smithy "github.com/aws/smithy-go"
	smithyhttp "github.com/aws/smithy-go/transport/http"
)

// S3Config configures the S3 backend. Never holds credentials -- see
// Credentials in credentials.go, passed separately to NewS3 and never
// retained on S3Backend, so a %+v of a backend or its config can never
// print one.
type S3Config struct {
	Endpoint              string
	Bucket                string
	Region                string // default "us-east-1" when empty
	ForcePathStyle        bool
	InsecureSkipTLSVerify bool
	PartSizeBytes         int64 // default 16<<20
	Concurrency           int   // default 4
	Retry                 RetryPolicy
	HTTPClient            *http.Client // optional
}

func (c S3Config) withDefaults() S3Config {
	if c.Region == "" {
		c.Region = "us-east-1"
	}
	if c.PartSizeBytes == 0 {
		c.PartSizeBytes = 16 << 20
	}
	if c.Concurrency == 0 {
		c.Concurrency = 4
	}
	return c
}

// Validate checks that Endpoint is a parseable http(s) URL and Bucket is
// non-empty.
func (c S3Config) Validate() error {
	if c.Endpoint == "" {
		return fmt.Errorf("%w: s3 endpoint is required", ErrInvalid)
	}
	u, err := url.Parse(c.Endpoint)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("%w: s3 endpoint %q must be a parseable http(s) URL", ErrInvalid, c.Endpoint)
	}
	if c.Bucket == "" {
		return fmt.Errorf("%w: s3 bucket is required", ErrInvalid)
	}
	return nil
}

// S3Backend implements Backend against an S3-compatible object store. It
// deliberately holds no credentials field.
type S3Backend struct {
	client      *s3.Client
	bucket      string
	partSize    int64
	concurrency int
	retry       RetryPolicy
}

var _ Backend = (*S3Backend)(nil)

// NewS3 constructs an S3Backend from cfg and creds. creds is used only to
// build the client's static credentials provider; it is never stored on
// S3Backend.
func NewS3(cfg S3Config, creds Credentials) (*S3Backend, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if err := creds.Validate(); err != nil {
		return nil, err
	}
	cfg = cfg.withDefaults()

	httpClient := cfg.HTTPClient
	if httpClient == nil {
		transport, ok := http.DefaultTransport.(*http.Transport)
		if !ok {
			transport = &http.Transport{}
		} else {
			transport = transport.Clone()
		}
		if cfg.InsecureSkipTLSVerify {
			transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // G402: opt-in, off by default, exists solely for in-cluster MinIO with a self-signed cert
		}
		httpClient = &http.Client{Transport: transport}
	}

	client := s3.New(s3.Options{
		BaseEndpoint: aws.String(cfg.Endpoint),
		Region:       cfg.Region,
		UsePathStyle: cfg.ForcePathStyle,
		Credentials: credentials.NewStaticCredentialsProvider(
			creds.AccessKeyID, creds.SecretAccessKey.Reveal(), creds.SessionToken.Reveal()),
		HTTPClient: httpClient,
	})

	return &S3Backend{client: client, bucket: cfg.Bucket, partSize: cfg.PartSizeBytes, concurrency: cfg.Concurrency, retry: cfg.Retry}, nil
}

// NewS3WithClient constructs an S3Backend from an already-built *s3.Client
// -- for tests that need full control over client construction (custom
// endpoint resolution, injected transports, ...).
func NewS3WithClient(c *s3.Client, cfg S3Config) *S3Backend {
	cfg = cfg.withDefaults()
	return &S3Backend{client: c, bucket: cfg.Bucket, partSize: cfg.PartSizeBytes, concurrency: cfg.Concurrency, retry: cfg.Retry}
}

// realSleep is retryDo's production sleep implementation: a context-aware
// timer, never a bare time.Sleep (which would ignore cancellation).
func realSleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// realRand is retryDo's production jitter source: math/rand/v2's top-level
// Float64, safe for concurrent use with no seeding required.
func realRand() float64 { return rand.Float64() } //nolint:gosec // G404: retry-backoff jitter, not a security-sensitive use of randomness

// classifyS3Error maps an S3 client error to an error Kind. NoSuchKey/
// NotFound/NoSuchBucket (as typed errors or by API error code) and an HTTP
// 404 all mean ErrNotFound. AccessDenied/InvalidAccessKeyId/
// SignatureDoesNotMatch and an HTTP 403 mean ErrPermission. Everything else
// -- including an HTTP status of 0 (no response received: dial refused,
// net.Error, a context deadline hit before any response), a 5xx, SlowDown,
// RequestTimeout, and any error this function doesn't specifically
// recognize -- defaults to ErrUnreachable. This default is deliberately
// fail-safe: treating an unrecognized error as "recoverable, try again" is
// always safer than treating it as "definitely missing", which could drive
// a caller into producing a silently empty restore.
func classifyS3Error(op, key string, err error) error {
	if err == nil {
		return nil
	}

	var notFoundKey *types.NoSuchKey
	var notFound *types.NotFound
	var noBucket *types.NoSuchBucket
	if errors.As(err, &notFoundKey) || errors.As(err, &notFound) || errors.As(err, &noBucket) {
		return &Error{Op: op, Backend: "s3", Key: key, Kind: ErrNotFound, Err: err}
	}

	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.ErrorCode() {
		case "NoSuchKey", "NotFound", "NoSuchBucket":
			return &Error{Op: op, Backend: "s3", Key: key, Kind: ErrNotFound, Err: err}
		case "AccessDenied", "InvalidAccessKeyId", "SignatureDoesNotMatch", "Forbidden":
			return &Error{Op: op, Backend: "s3", Key: key, Kind: ErrPermission, Err: err}
		}
	}

	var respErr *smithyhttp.ResponseError
	if errors.As(err, &respErr) {
		switch respErr.HTTPStatusCode() {
		case http.StatusNotFound:
			return &Error{Op: op, Backend: "s3", Key: key, Kind: ErrNotFound, Err: err}
		case http.StatusForbidden:
			return &Error{Op: op, Backend: "s3", Key: key, Kind: ErrPermission, Err: err}
		}
	}

	return &Error{Op: op, Backend: "s3", Key: key, Kind: ErrUnreachable, Err: err}
}

// Put uploads r to key via a multipart-aware manager.Uploader (NOT
// manager.Downloader's mirror-image on read -- see Get -- an Uploader takes
// a plain io.Reader, no io.WriterAt requirement, so this stays genuinely
// streaming). r is wrapped in a verifyingReader so the returned
// ObjectInfo.Size/SHA256 reflect exactly the bytes that were streamed, not
// whatever the caller claims. Put is never retried at this layer -- see
// retry.go's doc comment.
func (b *S3Backend) Put(ctx context.Context, key string, r io.Reader, opts PutOptions) (ObjectInfo, error) {
	vr := newVerifyingReader(r)
	//nolint:staticcheck // SA1019: feature/s3/manager is deprecated in favor of feature/s3/transfermanager,
	// which is still pre-1.0 on this environment's module proxy (see operator/README.md's dependency-pinning
	// notes) -- manager.Uploader remains the correct streaming multipart-upload choice for a plain io.Reader
	// until that replacement stabilizes.
	uploader := manager.NewUploader(b.client, func(u *manager.Uploader) {
		u.PartSize = b.partSize
		u.Concurrency = b.concurrency
	})

	input := &s3.PutObjectInput{
		Bucket: aws.String(b.bucket),
		Key:    aws.String(key),
		Body:   vr,
	}
	if opts.ContentType != "" {
		input.ContentType = aws.String(opts.ContentType)
	}

	if _, err := uploader.Upload(ctx, input); err != nil { //nolint:staticcheck // SA1019: see manager.NewUploader's nolint above
		return ObjectInfo{}, classifyS3Error("Put", key, err)
	}
	return ObjectInfo{Key: key, Size: vr.Size(), SHA256: vr.SHA256(), ModTime: time.Now().UTC()}, nil
}

// Get returns a streaming reader via plain s3.GetObject -- NOT
// manager.Downloader, which requires an io.WriterAt and so defeats
// streaming. SHA256 is left empty: verifying a stream requires reading all
// of it, which Get must not do on the caller's behalf; callers verify via
// the manifest (see manifest.go's Verify, and archive.go's RestoreFrom).
func (b *S3Backend) Get(ctx context.Context, key string) (io.ReadCloser, ObjectInfo, error) {
	var out *s3.GetObjectOutput
	err := retryDo(ctx, b.retry, realSleep, realRand, func(int) error {
		var gerr error
		out, gerr = b.client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(b.bucket), Key: aws.String(key)})
		if gerr != nil {
			return classifyS3Error("Get", key, gerr)
		}
		return nil
	})
	if err != nil {
		return nil, ObjectInfo{}, err
	}

	info := ObjectInfo{Key: key}
	if out.ContentLength != nil {
		info.Size = *out.ContentLength
	}
	if out.LastModified != nil {
		info.ModTime = out.LastModified.UTC()
	}
	return out.Body, info, nil
}

// Stat calls HeadObject.
func (b *S3Backend) Stat(ctx context.Context, key string) (ObjectInfo, error) {
	var out *s3.HeadObjectOutput
	err := retryDo(ctx, b.retry, realSleep, realRand, func(int) error {
		var herr error
		out, herr = b.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(b.bucket), Key: aws.String(key)})
		if herr != nil {
			return classifyS3Error("Stat", key, herr)
		}
		return nil
	})
	if err != nil {
		return ObjectInfo{}, err
	}

	info := ObjectInfo{Key: key}
	if out.ContentLength != nil {
		info.Size = *out.ContentLength
	}
	if out.LastModified != nil {
		info.ModTime = out.LastModified.UTC()
	}
	return info, nil
}

// List drains a full s3.NewListObjectsV2Paginator for prefix, sorted by
// Key.
func (b *S3Backend) List(ctx context.Context, prefix string) ([]ObjectInfo, error) {
	var out []ObjectInfo
	err := retryDo(ctx, b.retry, realSleep, realRand, func(int) error {
		out = out[:0]
		paginator := s3.NewListObjectsV2Paginator(b.client, &s3.ListObjectsV2Input{
			Bucket: aws.String(b.bucket),
			Prefix: aws.String(prefix),
		})
		for paginator.HasMorePages() {
			page, perr := paginator.NextPage(ctx)
			if perr != nil {
				return classifyS3Error("List", prefix, perr)
			}
			for _, obj := range page.Contents {
				info := ObjectInfo{Key: aws.ToString(obj.Key)}
				if obj.Size != nil {
					info.Size = *obj.Size
				}
				if obj.LastModified != nil {
					info.ModTime = obj.LastModified.UTC()
				}
				out = append(out, info)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	if out == nil {
		out = []ObjectInfo{}
	}
	return out, nil
}

// Delete calls DeleteObject -- S3 already no-ops on a missing key, matching
// Backend's "deleting a missing key is not an error" contract for free.
func (b *S3Backend) Delete(ctx context.Context, key string) error {
	return retryDo(ctx, b.retry, realSleep, realRand, func(int) error {
		_, err := b.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(b.bucket), Key: aws.String(key)})
		if err != nil {
			return classifyS3Error("Delete", key, err)
		}
		return nil
	})
}

// DeletePrefix lists then batch-deletes (1000 keys per DeleteObjects call,
// S3's own limit). Refuses an empty prefix.
func (b *S3Backend) DeletePrefix(ctx context.Context, prefix string) (int, error) {
	if prefix == "" {
		return 0, &Error{Op: "DeletePrefix", Backend: "s3", Key: prefix, Kind: ErrInvalid, Err: fmt.Errorf("empty prefix refused")}
	}
	objs, err := b.List(ctx, prefix)
	if err != nil {
		return 0, err
	}

	const batchSize = 1000
	total := 0
	for i := 0; i < len(objs); i += batchSize {
		end := i + batchSize
		if end > len(objs) {
			end = len(objs)
		}
		batch := objs[i:end]
		ids := make([]types.ObjectIdentifier, len(batch))
		for j, o := range batch {
			ids[j] = types.ObjectIdentifier{Key: aws.String(o.Key)}
		}

		err := retryDo(ctx, b.retry, realSleep, realRand, func(int) error {
			_, derr := b.client.DeleteObjects(ctx, &s3.DeleteObjectsInput{
				Bucket: aws.String(b.bucket),
				Delete: &types.Delete{Objects: ids},
			})
			if derr != nil {
				return classifyS3Error("DeletePrefix", prefix, derr)
			}
			return nil
		})
		if err != nil {
			return total, err
		}
		total += len(batch)
	}
	return total, nil
}
