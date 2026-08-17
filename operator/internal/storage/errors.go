package storage

import "errors"

// Sentinel error kinds. Backend implementations classify every I/O failure
// into exactly one of these via errors.Is/errors.As on the underlying
// transport error (see classifyS3Error in s3.go and classifyFSError in
// fs.go) -- callers branch on these, never on backend-specific error types.
var (
	// ErrNotFound means the requested key does not exist. This is a
	// definite, positive answer -- not "we couldn't tell".
	ErrNotFound = errors.New("storage: object not found")

	// ErrUnreachable means the backend could not be reached, or answered in
	// a way that could not be distinguished from "not reachable" (a 5xx, a
	// dial failure, a timeout, an ENOTDIR/ENOSPC/EIO on the mounted
	// filesystem's root). Errors of this kind ARE retryable
	// (see IsRetryable) -- an operation that failed to reach the backend
	// might succeed on a later attempt, unlike a definite NotFound.
	ErrUnreachable = errors.New("storage: backend unreachable")

	// ErrCorrupt means an object was read (or restored) but failed
	// integrity verification -- a size or SHA-256 mismatch against what was
	// expected. Never returned as a successful read.
	ErrCorrupt = errors.New("storage: object failed integrity verification")

	// ErrPermission means the backend reached the object/bucket/root but
	// refused the operation for lack of authorization.
	ErrPermission = errors.New("storage: permission denied")

	// ErrInvalid means the caller passed a structurally invalid argument
	// (an empty prefix to DeletePrefix, a key that escapes the configured
	// root, a malformed identity segment, ...). Never retried.
	ErrInvalid = errors.New("storage: invalid argument")
)

// Error is the concrete error type every Backend method returns on failure.
// It never carries a credential value -- Key is an object key or prefix,
// never a secret.
type Error struct {
	// Op is the operation that failed: "Put", "Get", "Stat", "List",
	// "Delete", or "DeletePrefix".
	Op string
	// Backend identifies which backend implementation produced the error:
	// "s3" or "fs".
	Backend string
	// Key is the object key or prefix the operation was acting on. Never a
	// credential.
	Key string
	// Kind is one of the sentinel errors above -- what errors.Is/IsNotFound/
	// IsRetryable etc. match against.
	Kind error
	// Err is the underlying error, if any (e.g. the wrapped transport
	// error). May be nil when Kind alone is sufficient.
	Err error
}

func (e *Error) Error() string {
	msg := e.Backend + ": " + e.Op + " " + e.Key + ": " + e.Kind.Error()
	if e.Err != nil {
		msg += ": " + e.Err.Error()
	}
	return msg
}

// Unwrap returns both Kind and Err (when non-nil) so errors.Is matches
// either -- errors.Is(err, ErrNotFound) works, and so does
// errors.Is(err, someWrappedTransportErr) when Err is set.
func (e *Error) Unwrap() []error {
	if e.Err != nil {
		return []error{e.Kind, e.Err}
	}
	return []error{e.Kind}
}

// IsNotFound reports whether err is (or wraps) ErrNotFound.
func IsNotFound(err error) bool { return errors.Is(err, ErrNotFound) }

// IsUnreachable reports whether err is (or wraps) ErrUnreachable.
func IsUnreachable(err error) bool { return errors.Is(err, ErrUnreachable) }

// IsCorrupt reports whether err is (or wraps) ErrCorrupt.
func IsCorrupt(err error) bool { return errors.Is(err, ErrCorrupt) }

// IsPermission reports whether err is (or wraps) ErrPermission.
func IsPermission(err error) bool { return errors.Is(err, ErrPermission) }

// IsInvalid reports whether err is (or wraps) ErrInvalid.
func IsInvalid(err error) bool { return errors.Is(err, ErrInvalid) }

// IsRetryable reports whether err is worth retrying: true only for
// Unreachable-kinded errors. NotFound, Invalid, Permission and Corrupt are
// never retried -- retrying them can only waste time (NotFound/Invalid/
// Permission) or, worse, paper over real corruption (Corrupt).
func IsRetryable(err error) bool { return errors.Is(err, ErrUnreachable) }
