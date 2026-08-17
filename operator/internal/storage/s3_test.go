package storage

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// minioEnv holds the SANDBOX_TEST_MINIO_* environment variables this
// package's S3 conformance leg is gated on. See doc.go and
// operator/README.md: this is an env-var skip gate, deliberately NOT a
// build tag, so the file always compiles/vets/lints/gofmts regardless of
// whether a MinIO instance is available -- only execution is conditional.
type minioEnv struct {
	endpoint  string
	accessKey string
	secretKey string
}

func loadMinioEnv(t *testing.T) (minioEnv, bool) {
	t.Helper()
	ep := os.Getenv("SANDBOX_TEST_MINIO_ENDPOINT")
	if ep == "" {
		return minioEnv{}, false
	}
	return minioEnv{
		endpoint:  ep,
		accessKey: os.Getenv("SANDBOX_TEST_MINIO_ACCESS_KEY"),
		secretKey: os.Getenv("SANDBOX_TEST_MINIO_SECRET_KEY"),
	}, true
}

// rawS3Client builds a *s3.Client directly (bypassing NewS3, which returns
// only the Backend interface) so tests can perform admin-only operations
// (CreateBucket/DeleteBucket) the Backend interface deliberately doesn't
// expose.
func rawS3Client(env minioEnv) *s3.Client {
	return s3.New(s3.Options{
		BaseEndpoint: aws.String(env.endpoint),
		Region:       "us-east-1",
		UsePathStyle: true,
		Credentials:  credentials.NewStaticCredentialsProvider(env.accessKey, env.secretKey, ""),
	})
}

// freshS3Backend skips unless SANDBOX_TEST_MINIO_ENDPOINT is set, then
// creates a fresh, uniquely-named bucket (cleaned up, including its
// contents, via t.Cleanup) and returns a Backend against it.
func freshS3Backend(t *testing.T) Backend {
	t.Helper()
	env, ok := loadMinioEnv(t)
	if !ok {
		t.Skip("SANDBOX_TEST_MINIO_ENDPOINT not set; skipping S3 conformance leg")
	}

	client := rawS3Client(env)
	bucket := fmt.Sprintf("sbx-conf-%d", time.Now().UnixNano())

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(bucket)}); err != nil {
		t.Fatalf("CreateBucket(%s): %v", bucket, err)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		be := NewS3WithClient(client, S3Config{Bucket: bucket})
		if n, err := be.DeletePrefix(cleanupCtx, ""); err != nil {
			t.Logf("cleanup: DeletePrefix: %v", err)
		} else {
			t.Logf("cleanup: removed %d objects from bucket %s", n, bucket)
		}
		if _, err := client.DeleteBucket(cleanupCtx, &s3.DeleteBucketInput{Bucket: aws.String(bucket)}); err != nil {
			t.Logf("cleanup: DeleteBucket(%s): %v", bucket, err)
		}
	})

	be, err := NewS3(S3Config{
		Endpoint:       env.endpoint,
		Bucket:         bucket,
		ForcePathStyle: true,
	}, Credentials{AccessKeyID: env.accessKey, SecretAccessKey: Secret(env.secretKey)})
	if err != nil {
		t.Fatalf("NewS3: %v", err)
	}
	return be
}

// brokenS3Backend returns an S3Backend pointed at an address nothing
// listens on, with a single attempt so tests fail fast.
func brokenS3Backend(t *testing.T) Backend {
	t.Helper()
	be, err := NewS3(S3Config{
		Endpoint:       "http://127.0.0.1:1",
		Bucket:         "unreachable-bucket",
		ForcePathStyle: true,
		Retry:          RetryPolicy{MaxAttempts: 1},
	}, Credentials{AccessKeyID: "broken", SecretAccessKey: Secret("broken")})
	if err != nil {
		t.Fatalf("NewS3: %v", err)
	}
	return be
}

func TestS3Config_Validate(t *testing.T) {
	cases := []struct {
		name    string
		cfg     S3Config
		wantErr bool
	}{
		{"valid http", S3Config{Endpoint: "http://minio:9000", Bucket: "b"}, false},
		{"valid https", S3Config{Endpoint: "https://minio:9000", Bucket: "b"}, false},
		{"missing endpoint", S3Config{Bucket: "b"}, true},
		{"missing bucket", S3Config{Endpoint: "http://minio:9000"}, true},
		{"unparseable endpoint", S3Config{Endpoint: "not a url", Bucket: "b"}, true},
		{"non-http scheme", S3Config{Endpoint: "ftp://minio:9000", Bucket: "b"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.Validate()
			if tc.wantErr && !IsInvalid(err) {
				t.Errorf("Validate() = %v, want ErrInvalid-kinded", err)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("Validate() = %v, want nil", err)
			}
		})
	}
}

func TestNewS3_RequiresValidCredentials(t *testing.T) {
	cfg := S3Config{Endpoint: "http://minio:9000", Bucket: "b"}
	if _, err := NewS3(cfg, Credentials{}); !IsInvalid(err) {
		t.Errorf("NewS3 with empty credentials: err = %v, want ErrInvalid", err)
	}
}

func TestS3Backend_ForcePathStyleDefaultingViaConfig(t *testing.T) {
	// FromS3Backend's *bool defaulting is exercised in config_test-equivalent
	// coverage below (TestFromS3Backend_ForcePathStyleDefault in
	// config_test.go); this just confirms S3Config.withDefaults leaves an
	// explicit false alone (it's a plain bool on S3Config, not a pointer --
	// the *bool/CRD-default translation happens one layer up, in
	// FromS3Backend).
	cfg := S3Config{Endpoint: "http://minio:9000", Bucket: "b", ForcePathStyle: false}.withDefaults()
	if cfg.ForcePathStyle {
		t.Error("withDefaults must not flip an explicit false to true")
	}
}
