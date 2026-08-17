package storage

import "fmt"

const redacted = "[REDACTED]"

// Fixed Secret data keys a future Secret-resolver (controller-side, #28/#29)
// reads S3 credentials from, given only S3Backend.CredentialsSecretRef.Name
// -- see doc.go's G1 gap note for why: the CRD's CredentialsSecretRef is a
// single-key SecretKeyRef{Name, Key}, but S3 needs two or three values.
// .Key is ignored for S3; these are the fixed keys instead.
const (
	SecretKeyAccessKeyID     = "accessKeyID"
	SecretKeySecretAccessKey = "secretAccessKey"
	SecretKeySessionToken    = "sessionToken"
)

// Secret is a string that structurally refuses to print its own value.
// Every stringification path (fmt's %v/%s/%q/%#v/%+v, encoding/json,
// encoding/text, and logr's Marshaler interface) is overridden to emit
// "[REDACTED]" instead of the real value. Reveal is the ONLY way to recover
// it -- see credentials_test.go's TestRevealCallSitesAreMinimal, which
// enforces that call sites of Reveal stay confined to s3.go.
type Secret string

// String implements fmt.Stringer.
func (Secret) String() string { return redacted }

// GoString implements fmt.GoStringer, covering %#v.
func (Secret) GoString() string { return redacted }

// MarshalJSON implements json.Marshaler.
func (Secret) MarshalJSON() ([]byte, error) { return []byte(`"` + redacted + `"`), nil }

// MarshalText implements encoding.TextMarshaler.
func (Secret) MarshalText() ([]byte, error) { return []byte(redacted), nil }

// MarshalLog implements logr's Marshaler interface, so a Secret passed as a
// structured logging value is redacted even when the sink doesn't go
// through fmt or encoding/json.
func (Secret) MarshalLog() any { return redacted }

// Reveal returns the real value. The only sanctioned call site is s3.go's
// credential-construction code.
func (s Secret) Reveal() string { return string(s) }

// IsZero reports whether the secret is empty.
func (s Secret) IsZero() bool { return s == "" }

// Credentials carries S3 access credentials resolved by the CALLER (future
// controller code, out of scope for this issue -- see doc.go's
// credential-resolution-boundary section) from a Secret named by
// S3Backend.CredentialsSecretRef.Name, using the fixed keys above.
// internal/storage never reads a Secret itself.
type Credentials struct {
	AccessKeyID     string
	SecretAccessKey Secret
	SessionToken    Secret
}

// String never includes any field value, even AccessKeyID (which is not
// secret on its own, but is withheld here anyway so a %v of the whole
// struct can never be mistaken for including one).
func (c Credentials) String() string { return "storage.Credentials{[REDACTED]}" }

// GoString covers %#v.
func (c Credentials) GoString() string { return c.String() }

// MarshalJSON never includes any field value.
func (c Credentials) MarshalJSON() ([]byte, error) { return []byte(`"` + redacted + `"`), nil }

// MarshalLog implements logr's Marshaler interface.
func (c Credentials) MarshalLog() any { return redacted }

// Validate checks that both AccessKeyID and SecretAccessKey are non-empty.
// SessionToken is optional (only required for temporary/STS credentials).
func (c Credentials) Validate() error {
	if c.AccessKeyID == "" {
		return fmt.Errorf("%w: credentials accessKeyID must not be empty", ErrInvalid)
	}
	if c.SecretAccessKey.IsZero() {
		return fmt.Errorf("%w: credentials secretAccessKey must not be empty", ErrInvalid)
	}
	return nil
}
