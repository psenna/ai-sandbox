package storage

import (
	"context"
	"io"
	"time"
)

// ObjectInfo describes an object independent of which Backend produced it.
type ObjectInfo struct {
	// Key is the object's key (S3) or backend-relative path (FS), always
	// forward-slash separated.
	Key string
	// Size is the object's size in bytes.
	Size int64
	// SHA256 is a lowercase-hex SHA-256 digest of the object's bytes,
	// populated by Put and by ArchiveTo/RestoreFrom's own verification
	// paths. Left empty by Stat and List: computing a digest there would
	// require reading the whole object, defeating the point of a
	// metadata-only call. S3's ETag is deliberately never substituted here
	// -- for a multipart upload it is not a SHA-256 (and not even a valid
	// MD5 of the whole object), so treating it as one would be silently
	// wrong.
	SHA256 string
	// ModTime is the object's last-modified time.
	ModTime time.Time
}

// PutOptions configures a Put call.
type PutOptions struct {
	// ContentType is the object's content type, if the backend supports
	// storing one. Ignored by the FS backend.
	ContentType string
}

// Backend is the storage backend seam: one implementation per
// BackendSpec.Type ("s3" -> S3Backend, "pvc" -> FSBackend). Every method is
// streaming-only -- no signature accepts or returns a []byte -- so a
// multi-gigabyte workspace archive never has to fit in memory. Both
// implementations satisfy an identical behavioral contract, verified by the
// shared conformance_test.go suite: NotFound vs. Unreachable are always
// distinguishable, deleting a missing key is not an error, List's prefix is
// a string prefix (not a directory boundary), and List results are always
// sorted by Key.
type Backend interface {
	// Put streams r to key, returning the actually-written size and
	// SHA-256. Put is never wrapped by retryDo: r is consumed as it is
	// read, so a retried Put after a partial read would silently upload
	// truncated data. See retry.go's doc comment.
	Put(ctx context.Context, key string, r io.Reader, opts PutOptions) (ObjectInfo, error)

	// Get returns a streaming reader for key's contents plus its metadata.
	// The caller must Close the returned ReadCloser. Returns an
	// ErrNotFound-kinded error if key does not exist.
	Get(ctx context.Context, key string) (io.ReadCloser, ObjectInfo, error)

	// Stat returns key's metadata without reading its body. Returns an
	// ErrNotFound-kinded error if key does not exist.
	Stat(ctx context.Context, key string) (ObjectInfo, error)

	// List returns every object whose key has the given string prefix,
	// sorted by Key. A prefix that matches nothing returns an empty slice
	// and a nil error -- this is not treated as ErrNotFound.
	List(ctx context.Context, prefix string) ([]ObjectInfo, error)

	// Delete removes key. Deleting a key that does not exist is NOT an
	// error -- Delete is idempotent.
	Delete(ctx context.Context, key string) error

	// DeletePrefix removes every object whose key has the given string
	// prefix, returning the count removed. Refuses an empty prefix with an
	// ErrInvalid-kinded error (a bug that passes "" must never wipe an
	// entire backend).
	DeletePrefix(ctx context.Context, prefix string) (int, error)
}
