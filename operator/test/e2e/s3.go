package e2e

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	gomega "github.com/onsi/gomega"
)

// S3Object is one entry in a ListObjectsV2 response.
type S3Object struct {
	Key          string    `xml:"Key"`
	Size         int64     `xml:"Size"`
	LastModified time.Time `xml:"LastModified"`
	ETag         string    `xml:"ETag"`
}

// listBucketResult is the subset of the ListObjectsV2 XML response this
// package needs.
type listBucketResult struct {
	XMLName               xml.Name   `xml:"ListBucketResult"`
	Contents              []S3Object `xml:"Contents"`
	IsTruncated           bool       `xml:"IsTruncated"`
	NextContinuationToken string     `xml:"NextContinuationToken"`
}

// S3BaseURL is the MinIO endpoint the TEST reaches from outside the
// cluster (via the NodePort mapping in test/e2e/manifests/minio.yaml). The
// operator itself, and anything running inside the cluster, uses the
// in-cluster DNS name instead (see CreateClass's WithS3Backend default).
func (h *Harness) S3BaseURL() string {
	return fmt.Sprintf("http://%s:%d", h.Cfg.Host, h.Cfg.MinIOPort)
}

// S3Get performs an unsigned GET of bucket/key. MinIO serves this without
// credentials because minio-bootstrap ran `mc anonymous set download` on
// both e2e buckets (see test/e2e/manifests/minio.yaml).
func (h *Harness) S3Get(ctx context.Context, bucket, key string) ([]byte, error) {
	u := fmt.Sprintf("%s/%s/%s", h.S3BaseURL(), bucket, key)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: unexpected status %d: %s", u, resp.StatusCode, body)
	}
	return body, nil
}

// S3List performs an unsigned ListObjectsV2 of bucket, filtered to prefix,
// paginating via continuation-token until the result is no longer
// truncated.
func (h *Harness) S3List(ctx context.Context, bucket, prefix string) ([]S3Object, error) {
	var all []S3Object
	token := ""
	for {
		q := url.Values{}
		q.Set("list-type", "2")
		if prefix != "" {
			q.Set("prefix", prefix)
		}
		if token != "" {
			q.Set("continuation-token", token)
		}
		u := fmt.Sprintf("%s/%s?%s", h.S3BaseURL(), bucket, q.Encode())

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, err
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, err
		}
		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("GET %s: unexpected status %d: %s", u, resp.StatusCode, body)
		}

		var parsed listBucketResult
		if err := xml.Unmarshal(body, &parsed); err != nil {
			return nil, fmt.Errorf("parsing ListObjectsV2 response: %w", err)
		}
		all = append(all, parsed.Contents...)
		if !parsed.IsTruncated || parsed.NextContinuationToken == "" {
			break
		}
		token = parsed.NextContinuationToken
	}
	return all, nil
}

// S3Put writes body to key in Cfg.FixtureBucket ONLY -- it refuses any
// other bucket, since the fixture bucket is the sole bucket the e2e suite
// itself is meant to write test data into (the snapshot bucket is written
// by the operator, once #28/#29 exist).
func (h *Harness) S3Put(ctx context.Context, key string, body []byte) error {
	u := fmt.Sprintf("%s/%s/%s", h.S3BaseURL(), h.Cfg.FixtureBucket, key)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, u, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.ContentLength = int64(len(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("PUT %s: unexpected status %d: %s", u, resp.StatusCode, respBody)
	}
	return nil
}

// WaitForS3Object polls bucket/key until S3Get succeeds, returning its
// body, or fails the spec after timeout.
func (h *Harness) WaitForS3Object(ctx context.Context, bucket, key string, timeout time.Duration) []byte {
	var data []byte
	gomega.EventuallyWithOffset(1, func() error {
		d, err := h.S3Get(ctx, bucket, key)
		if err != nil {
			return err
		}
		data = d
		return nil
	}, timeout, h.Cfg.Poll).Should(gomega.Succeed(), "object %s/%s never appeared", bucket, key)
	return data
}
