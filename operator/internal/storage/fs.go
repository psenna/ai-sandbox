package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

// FSConfig configures the PVC/mounted-filesystem backend. Root must already
// be mounted (this package never mounts a volume itself -- see gap G5 in
// doc.go).
type FSConfig struct {
	Root     string
	DirPerm  fs.FileMode // default 0o755
	FilePerm fs.FileMode // default 0o644
	Retry    RetryPolicy
}

func (c FSConfig) withDefaults() FSConfig {
	if c.DirPerm == 0 {
		c.DirPerm = 0o755
	}
	if c.FilePerm == 0 {
		c.FilePerm = 0o644
	}
	return c
}

// Validate checks that Root is an absolute path that exists and is a
// directory.
func (c FSConfig) Validate() error {
	if c.Root == "" || !filepath.IsAbs(c.Root) {
		return fmt.Errorf("%w: fs backend root must be an absolute path, got %q", ErrInvalid, c.Root)
	}
	fi, err := os.Stat(c.Root)
	if err != nil {
		return fmt.Errorf("%w: fs backend root %q: %v", ErrInvalid, c.Root, err)
	}
	if !fi.IsDir() {
		return fmt.Errorf("%w: fs backend root %q is not a directory", ErrInvalid, c.Root)
	}
	return nil
}

// FSBackend implements Backend against a mounted filesystem (a
// PersistentVolumeClaim in production; any local directory in tests).
type FSBackend struct {
	root     string
	dirPerm  fs.FileMode
	filePerm fs.FileMode
	retry    RetryPolicy
}

var _ Backend = (*FSBackend)(nil)

// NewFS constructs an FSBackend from cfg.
func NewFS(cfg FSConfig) (*FSBackend, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	cfg = cfg.withDefaults()
	return &FSBackend{root: cfg.Root, dirPerm: cfg.DirPerm, filePerm: cfg.FilePerm, retry: cfg.Retry}, nil
}

// keyToPath is the single choke point translating an object key into a
// filesystem path: rejects "..", absolute keys and empty keys, then
// re-verifies the joined result is still under root. Every method that
// touches the filesystem goes through this.
func (b *FSBackend) keyToPath(key string) (string, error) {
	if key == "" {
		return "", &Error{Op: "keyToPath", Backend: "fs", Key: key, Kind: ErrInvalid, Err: fmt.Errorf("empty key")}
	}
	if strings.HasPrefix(key, "/") {
		return "", &Error{Op: "keyToPath", Backend: "fs", Key: key, Kind: ErrInvalid, Err: fmt.Errorf("absolute key")}
	}
	for _, seg := range strings.Split(key, "/") {
		if seg == ".." {
			return "", &Error{Op: "keyToPath", Backend: "fs", Key: key, Kind: ErrInvalid, Err: fmt.Errorf("key contains \"..\"")}
		}
	}
	full := filepath.Join(b.root, filepath.FromSlash(key))
	if full != b.root && !strings.HasPrefix(full, b.root+string(filepath.Separator)) {
		return "", &Error{Op: "keyToPath", Backend: "fs", Key: key, Kind: ErrInvalid, Err: fmt.Errorf("key escapes root")}
	}
	return full, nil
}

// classifyFSError maps a local-filesystem error to an error Kind:
// fs.ErrNotExist -> ErrNotFound, fs.ErrPermission -> ErrPermission,
// ENOTDIR on the root / a missing root / ENOSPC / EIO -> ErrUnreachable
// (the same taxonomy the S3 backend uses for "reached the service but it
// said no" vs "couldn't reach it at all" -- for a mounted filesystem, an
// unusable root/mount plays the "unreachable" role). op and key are
// accepted for a uniform call signature with the *_test.go helpers, but
// this function only inspects err.
func classifyFSError(op, key string, err error) error {
	if err == nil {
		return nil
	}
	kind := ErrUnreachable
	switch {
	case errors.Is(err, fs.ErrNotExist):
		kind = ErrNotFound
	case errors.Is(err, fs.ErrPermission):
		kind = ErrPermission
	case errors.Is(err, syscall.ENOTDIR), errors.Is(err, syscall.ENOSPC), errors.Is(err, syscall.EIO):
		kind = ErrUnreachable
	}
	return &Error{Op: op, Backend: "fs", Key: key, Kind: kind, Err: err}
}

// Put writes r to key atomically: a temp file in the target directory,
// streamed through a verifyingReader, fsync'd, chmod'd, then renamed onto
// the final path. The temp file is removed on any failure path.
func (b *FSBackend) Put(ctx context.Context, key string, r io.Reader, _ PutOptions) (ObjectInfo, error) {
	if err := ctx.Err(); err != nil {
		return ObjectInfo{}, err
	}
	full, err := b.keyToPath(key)
	if err != nil {
		return ObjectInfo{}, err
	}
	dir := filepath.Dir(full)
	if err := os.MkdirAll(dir, b.dirPerm); err != nil { //nolint:gosec // G301: dirPerm defaults to 0o755, caller-configurable via FSConfig
		return ObjectInfo{}, classifyFSError("Put", key, err)
	}

	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return ObjectInfo{}, classifyFSError("Put", key, err)
	}
	tmpPath := tmp.Name()
	succeeded := false
	defer func() {
		if !succeeded {
			_ = os.Remove(tmpPath)
		}
	}()

	vr := newVerifyingReader(r)
	if _, err := io.Copy(tmp, vr); err != nil {
		_ = tmp.Close()
		return ObjectInfo{}, classifyFSError("Put", key, err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return ObjectInfo{}, classifyFSError("Put", key, err)
	}
	if err := tmp.Close(); err != nil {
		return ObjectInfo{}, classifyFSError("Put", key, err)
	}
	if err := os.Chmod(tmpPath, b.filePerm); err != nil { //nolint:gosec // G302: filePerm defaults to 0o644, caller-configurable via FSConfig
		return ObjectInfo{}, classifyFSError("Put", key, err)
	}
	if err := os.Rename(tmpPath, full); err != nil {
		return ObjectInfo{}, classifyFSError("Put", key, err)
	}
	succeeded = true

	info := ObjectInfo{Key: key, Size: vr.Size(), SHA256: vr.SHA256(), ModTime: time.Now().UTC()}
	if fi, statErr := os.Stat(full); statErr == nil { //nolint:gosec // G304: full comes from keyToPath, which rejects "..", absolute keys and any result outside root
		info.ModTime = fi.ModTime().UTC()
	}
	return info, nil
}

// Get opens key for streaming reads.
func (b *FSBackend) Get(ctx context.Context, key string) (io.ReadCloser, ObjectInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, ObjectInfo{}, err
	}
	full, err := b.keyToPath(key)
	if err != nil {
		return nil, ObjectInfo{}, err
	}
	f, err := os.Open(full) //nolint:gosec // G304: full comes from keyToPath, which rejects "..", absolute keys and any result outside root
	if err != nil {
		return nil, ObjectInfo{}, classifyFSError("Get", key, err)
	}
	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, ObjectInfo{}, classifyFSError("Get", key, err)
	}
	return f, ObjectInfo{Key: key, Size: fi.Size(), ModTime: fi.ModTime().UTC()}, nil
}

// Stat returns key's metadata without opening its body.
func (b *FSBackend) Stat(ctx context.Context, key string) (ObjectInfo, error) {
	if err := ctx.Err(); err != nil {
		return ObjectInfo{}, err
	}
	full, err := b.keyToPath(key)
	if err != nil {
		return ObjectInfo{}, err
	}
	fi, err := os.Stat(full) //nolint:gosec // G304: full comes from keyToPath, which rejects "..", absolute keys and any result outside root
	if err != nil {
		return ObjectInfo{}, classifyFSError("Stat", key, err)
	}
	if fi.IsDir() {
		return ObjectInfo{}, &Error{Op: "Stat", Backend: "fs", Key: key, Kind: ErrNotFound, Err: fmt.Errorf("key is a directory")}
	}
	return ObjectInfo{Key: key, Size: fi.Size(), ModTime: fi.ModTime().UTC()}, nil
}

// List returns every object whose key has the given string prefix, sorted
// by key. prefix is a STRING prefix, not a directory boundary: List scans
// the deepest directory that could contain a match (prefix's directory
// portion) and filters entries with strings.HasPrefix. A scan directory
// that simply doesn't exist yet returns an empty list, not an error (a
// fresh environment with no snapshots is not a failure); a scan directory
// that exists but is not a directory (a file blocking the path, or -- at
// the top -- a structurally broken root) is ErrUnreachable, the same
// taxonomy the S3 backend uses for "reached the service but it said no"
// vs. "couldn't use it at all". ".tmp-*" temp files are skipped.
func (b *FSBackend) List(ctx context.Context, prefix string) ([]ObjectInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	scanDir, err := b.listScanDir(prefix)
	if err != nil {
		return nil, err
	}

	fi, statErr := os.Stat(scanDir) //nolint:gosec // G304: scanDir derives from b.root joined with a validated prefix
	switch {
	case statErr != nil && errors.Is(statErr, fs.ErrNotExist):
		return []ObjectInfo{}, nil
	case statErr != nil:
		return nil, classifyFSError("List", prefix, statErr)
	case !fi.IsDir():
		return nil, classifyFSError("List", prefix, &fs.PathError{Op: "stat", Path: scanDir, Err: syscall.ENOTDIR})
	}

	var out []ObjectInfo
	err = filepath.WalkDir(scanDir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if errors.Is(walkErr, fs.ErrNotExist) {
				return nil
			}
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		if strings.HasPrefix(d.Name(), ".tmp-") {
			return nil
		}
		rel, relErr := filepath.Rel(b.root, path)
		if relErr != nil {
			return relErr
		}
		key := filepath.ToSlash(rel)
		if !strings.HasPrefix(key, prefix) {
			return nil
		}
		info, infoErr := d.Info()
		if infoErr != nil {
			return infoErr
		}
		out = append(out, ObjectInfo{Key: key, Size: info.Size(), ModTime: info.ModTime().UTC()})
		return nil
	})
	if err != nil {
		return nil, classifyFSError("List", prefix, err)
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	if out == nil {
		out = []ObjectInfo{}
	}
	return out, nil
}

// listScanDir validates prefix and returns the deepest directory that could
// contain a matching entry: prefix itself when it names a directory
// (ends with "/" or is empty), else prefix's directory portion.
func (b *FSBackend) listScanDir(prefix string) (string, error) {
	if strings.HasPrefix(prefix, "/") || strings.Contains(prefix, "..") {
		return "", &Error{Op: "List", Backend: "fs", Key: prefix, Kind: ErrInvalid, Err: fmt.Errorf("invalid prefix")}
	}
	full := filepath.Join(b.root, filepath.FromSlash(prefix))
	if full != b.root && !strings.HasPrefix(full, b.root+string(filepath.Separator)) {
		return "", &Error{Op: "List", Backend: "fs", Key: prefix, Kind: ErrInvalid, Err: fmt.Errorf("prefix escapes root")}
	}
	if prefix == "" || strings.HasSuffix(prefix, "/") {
		return full, nil
	}
	return filepath.Dir(full), nil
}

// Delete removes key. Missing is not an error. After removing, prunes
// now-empty parent directories up to (not including) root.
func (b *FSBackend) Delete(ctx context.Context, key string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	full, err := b.keyToPath(key)
	if err != nil {
		return err
	}
	if err := os.Remove(full); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return classifyFSError("Delete", key, err)
	}
	b.pruneEmptyParents(filepath.Dir(full))
	return nil
}

// pruneEmptyParents removes dir and its ancestors, stopping at the first
// non-empty directory or at root.
func (b *FSBackend) pruneEmptyParents(dir string) {
	for dir != b.root && strings.HasPrefix(dir, b.root+string(filepath.Separator)) {
		entries, err := os.ReadDir(dir) //nolint:gosec // G304: dir derives from a path already validated by keyToPath
		if err != nil || len(entries) > 0 {
			return
		}
		if err := os.Remove(dir); err != nil {
			return
		}
		dir = filepath.Dir(dir)
	}
}

// DeletePrefix removes every object under prefix. Refuses an empty prefix.
func (b *FSBackend) DeletePrefix(ctx context.Context, prefix string) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if prefix == "" {
		return 0, &Error{Op: "DeletePrefix", Backend: "fs", Key: prefix, Kind: ErrInvalid, Err: fmt.Errorf("empty prefix refused")}
	}
	objs, err := b.List(ctx, prefix)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, o := range objs {
		if err := b.Delete(ctx, o.Key); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}
