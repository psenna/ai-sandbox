package storage

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
)

// verifyingReader wraps an io.Reader, accumulating a running SHA-256 digest
// and byte count of everything read through it. Used on both the write path
// (Put/WriteArchive: the digest of exactly what was streamed becomes the
// returned ObjectInfo.SHA256) and the read path (RestoreFrom: the digest of
// exactly what was streamed back is checked against the manifest).
type verifyingReader struct {
	r io.Reader
	h hash.Hash
	n int64
}

func newVerifyingReader(r io.Reader) *verifyingReader {
	return &verifyingReader{r: r, h: sha256.New()}
}

func (v *verifyingReader) Read(p []byte) (int, error) {
	n, err := v.r.Read(p)
	if n > 0 {
		v.h.Write(p[:n])
		v.n += int64(n)
	}
	return n, err
}

// Size returns the number of bytes read so far.
func (v *verifyingReader) Size() int64 { return v.n }

// SHA256 returns the lowercase-hex digest of everything read so far.
func (v *verifyingReader) SHA256() string { return hex.EncodeToString(v.h.Sum(nil)) }

// Verify compares what was actually read (Size/SHA256) against want,
// returning an ErrCorrupt-kinded *Error on any mismatch.
func (v *verifyingReader) Verify(op, key string, want FileEntry) error {
	if v.Size() != want.Size || v.SHA256() != want.SHA256 {
		return &Error{Op: op, Backend: "verify", Key: key, Kind: ErrCorrupt,
			Err: fmt.Errorf("want size=%d sha256=%s, got size=%d sha256=%s", want.Size, want.SHA256, v.Size(), v.SHA256())}
	}
	return nil
}
