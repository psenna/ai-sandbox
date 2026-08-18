package sandboxctl

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/psenna/ai-sandbox/operator/internal/storage"
)

// ReadCredentialsDir reads S3 credentials from the Secret volume the
// operator mounts into THIS container only. File names are exactly
// internal/storage's fixed data keys (SecretKeyAccessKeyID,
// SecretKeySecretAccessKey, SecretKeySessionToken) -- resolving
// storage/doc.go's gap G1. sessionToken is optional. Never logs a value;
// every error names a path only.
func ReadCredentialsDir(dir string) (storage.Credentials, error) {
	accessKeyID, err := readCredFile(dir, storage.SecretKeyAccessKeyID, true)
	if err != nil {
		return storage.Credentials{}, err
	}
	secretAccessKey, err := readCredFile(dir, storage.SecretKeySecretAccessKey, true)
	if err != nil {
		return storage.Credentials{}, err
	}
	sessionToken, err := readCredFile(dir, storage.SecretKeySessionToken, false)
	if err != nil {
		return storage.Credentials{}, err
	}
	return storage.Credentials{
		AccessKeyID:     accessKeyID,
		SecretAccessKey: storage.Secret(secretAccessKey),
		SessionToken:    storage.Secret(sessionToken),
	}, nil
}

// readCredFile reads and trims a single trailing newline from
// <dir>/<name>. required=false makes a missing file return "" rather than
// an error (the optional sessionToken case).
func readCredFile(dir, name string, required bool) (string, error) {
	path := filepath.Join(dir, name)
	b, err := os.ReadFile(path) //nolint:gosec // G304: path is built from an operator-controlled, fixed mount directory and fixed file names
	if err != nil {
		if os.IsNotExist(err) && !required {
			return "", nil
		}
		return "", fmt.Errorf("sandboxctl: reading credential file %s: %w", path, err)
	}
	return strings.TrimSuffix(string(b), "\n"), nil
}
