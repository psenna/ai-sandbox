package storage

import (
	"archive/tar"
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/klauspost/compress/zstd"
)

// ArchiveOptions configures WriteArchive/ArchiveTo.
type ArchiveOptions struct {
	// Level is the zstd compression level, default zstd.SpeedDefault.
	Level zstd.EncoderLevel
	// Concurrency is the number of zstd encoder goroutines, default 2 --
	// deliberately NOT GOMAXPROCS: the operator pod is resource-limited and
	// may run alongside other work, so an archive write should not claim
	// every core by default.
	Concurrency int
	// Exclude, if non-nil, is called for every walked entry with its
	// root-relative slash-separated path and fs.FileInfo; returning true
	// skips it (and, for a directory, its entire subtree).
	Exclude func(rel string, fi fs.FileInfo) bool
}

func (o ArchiveOptions) withDefaults() ArchiveOptions {
	if o.Level == 0 {
		o.Level = zstd.SpeedDefault
	}
	if o.Concurrency == 0 {
		o.Concurrency = 2
	}
	return o
}

// ExtractOptions configures ReadArchive/RestoreFrom.
type ExtractOptions struct {
	Concurrency int
	// DirPerm is the permission mode used for created directories, default
	// 0o755.
	DirPerm fs.FileMode
	// MaxEntries caps the number of tar entries restored; 0 means
	// unlimited.
	MaxEntries int64
	// MaxTotalBytes caps the total uncompressed bytes restored; 0 means
	// unlimited.
	MaxTotalBytes int64
}

func (o ExtractOptions) withDefaults() ExtractOptions {
	if o.Concurrency == 0 {
		o.Concurrency = 2
	}
	if o.DirPerm == 0 {
		o.DirPerm = 0o755
	}
	return o
}

// countingWriter counts bytes written through it.
type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// WriteArchive walks root and writes a deterministic tar+zstd stream to w:
// entries in the lexical order filepath.WalkDir already guarantees, Uid/
// Gid/Uname/Gname zeroed, tar.FormatPAX forced (so long names and non-ASCII
// names round-trip without ustar's length limits). Returns the number of
// UNCOMPRESSED bytes written to the tar stream (i.e. the archive's logical
// size, not the compressed size on the wire). Memory cost is one 32KiB
// io.Copy buffer plus the zstd window, regardless of tree size -- nothing
// is buffered in memory or materialized as a temp file.
func WriteArchive(ctx context.Context, root string, w io.Writer, opts ArchiveOptions) (int64, error) {
	opts = opts.withDefaults()

	enc, err := zstd.NewWriter(w, zstd.WithEncoderLevel(opts.Level), zstd.WithEncoderConcurrency(opts.Concurrency))
	if err != nil {
		return 0, fmt.Errorf("storage: creating zstd encoder: %w", err)
	}
	counter := &countingWriter{w: enc}
	tw := tar.NewWriter(counter)

	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		if path == root {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		relSlash := filepath.ToSlash(rel)

		info, err := d.Info()
		if err != nil {
			return err
		}
		if opts.Exclude != nil && opts.Exclude(relSlash, info) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		var link string
		if info.Mode()&fs.ModeSymlink != 0 {
			link, err = os.Readlink(path) //nolint:gosec // G304: path comes from filepath.WalkDir(root, ...); root is caller-controlled and trusted
			if err != nil {
				return err
			}
		}

		hdr, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return err
		}
		hdr.Name = relSlash
		if d.IsDir() {
			hdr.Name += "/"
		}
		hdr.Uid, hdr.Gid = 0, 0
		hdr.Uname, hdr.Gname = "", ""
		hdr.Format = tar.FormatPAX

		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}

		if info.Mode().IsRegular() {
			f, ferr := os.Open(path) //nolint:gosec // G304: path comes from filepath.WalkDir(root, ...); root is caller-controlled and trusted
			if ferr != nil {
				return ferr
			}
			_, cerr := io.Copy(tw, f)
			_ = f.Close()
			if cerr != nil {
				return cerr
			}
		}
		return nil
	})

	if walkErr != nil {
		_ = tw.Close()
		_ = enc.Close()
		if ctxErr := ctx.Err(); ctxErr != nil && walkErr == ctxErr {
			return counter.n, ctxErr
		}
		return counter.n, &Error{Op: "Put", Backend: "archive", Key: root, Kind: classifyOSErrKind(walkErr), Err: walkErr}
	}
	if err := tw.Close(); err != nil {
		_ = enc.Close()
		return counter.n, &Error{Op: "Put", Backend: "archive", Key: root, Kind: ErrUnreachable, Err: err}
	}
	if err := enc.Close(); err != nil {
		return counter.n, &Error{Op: "Put", Backend: "archive", Key: root, Kind: ErrUnreachable, Err: err}
	}
	return counter.n, nil
}

// ReadArchive streams a zstd+tar archive from r into root. Every entry name
// is checked against root BEFORE any file is created: an absolute path, a
// ".." component, or a symlink target that would resolve outside root all
// abort with an ErrInvalid-kinded error (the zip-slip guard). Restores
// regular files, directories and symlinks; refuses hard links, device
// nodes, FIFOs and sockets. Returns the number of uncompressed bytes
// restored.
func ReadArchive(ctx context.Context, r io.Reader, root string, opts ExtractOptions) (int64, error) {
	opts = opts.withDefaults()

	dec, err := zstd.NewReader(r, zstd.WithDecoderConcurrency(opts.Concurrency))
	if err != nil {
		return 0, &Error{Op: "Get", Backend: "archive", Key: root, Kind: ErrCorrupt, Err: err}
	}
	defer dec.Close()

	tr := tar.NewReader(dec)
	var total int64
	var entries int64
	for {
		if cerr := ctx.Err(); cerr != nil {
			return total, cerr
		}
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return total, &Error{Op: "Get", Backend: "archive", Key: root, Kind: ErrCorrupt, Err: err}
		}

		entries++
		if opts.MaxEntries > 0 && entries > opts.MaxEntries {
			return total, &Error{Op: "Get", Backend: "archive", Key: root, Kind: ErrInvalid,
				Err: fmt.Errorf("archive exceeds max entries %d", opts.MaxEntries)}
		}

		target, err := safeJoin(root, hdr.Name)
		if err != nil {
			return total, err
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, opts.DirPerm); err != nil { //nolint:gosec // G301: DirPerm defaults to 0o755, caller-configurable
				return total, &Error{Op: "Get", Backend: "archive", Key: hdr.Name, Kind: classifyOSErrKind(err), Err: err}
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), opts.DirPerm); err != nil { //nolint:gosec // G301: DirPerm defaults to 0o755, caller-configurable
				return total, &Error{Op: "Get", Backend: "archive", Key: hdr.Name, Kind: classifyOSErrKind(err), Err: err}
			}
			n, err := restoreRegularFile(target, tr, hdr, opts, total)
			total += n
			if err != nil {
				return total, err
			}
		case tar.TypeSymlink:
			if err := validateSymlinkTarget(root, filepath.Dir(target), hdr.Linkname); err != nil {
				return total, err
			}
			_ = os.Remove(target) // allow re-restore to overwrite a previous symlink
			if err := os.Symlink(hdr.Linkname, target); err != nil {
				return total, &Error{Op: "Get", Backend: "archive", Key: hdr.Name, Kind: classifyOSErrKind(err), Err: err}
			}
		default:
			return total, &Error{Op: "Get", Backend: "archive", Key: hdr.Name, Kind: ErrInvalid,
				Err: fmt.Errorf("unsupported tar entry type %v", hdr.Typeflag)}
		}
	}
	return total, nil
}

// restoreRegularFile copies one regular file's content from tr to target,
// enforcing opts.MaxTotalBytes across the whole archive (totalSoFar is the
// cumulative count before this file).
func restoreRegularFile(target string, tr *tar.Reader, hdr *tar.Header, opts ExtractOptions, totalSoFar int64) (int64, error) {
	mode := fs.FileMode(hdr.Mode) & 0o777 //nolint:gosec // G115: tar mode bits masked to the low 9 bits before conversion
	if mode == 0 {
		mode = 0o644
	}
	f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode) //nolint:gosec // G304,G302: target comes from safeJoin, which rejects any path outside root
	if err != nil {
		return 0, &Error{Op: "Get", Backend: "archive", Key: hdr.Name, Kind: classifyOSErrKind(err), Err: err}
	}
	defer func() { _ = f.Close() }()

	var src io.Reader = tr
	if opts.MaxTotalBytes > 0 {
		remaining := opts.MaxTotalBytes - totalSoFar
		if remaining < 0 {
			remaining = 0
		}
		src = io.LimitReader(tr, remaining+1)
	}
	n, err := io.Copy(f, src)
	if err != nil {
		return n, &Error{Op: "Get", Backend: "archive", Key: hdr.Name, Kind: classifyOSErrKind(err), Err: err}
	}
	if opts.MaxTotalBytes > 0 && totalSoFar+n > opts.MaxTotalBytes {
		return n, &Error{Op: "Get", Backend: "archive", Key: hdr.Name, Kind: ErrInvalid,
			Err: fmt.Errorf("archive exceeds max total bytes %d", opts.MaxTotalBytes)}
	}
	return n, nil
}

// safeJoin validates name (a tar entry name) against root and returns the
// resulting filesystem path. Rejects an empty or absolute name, and any
// name whose cleaned form is ".." or escapes root.
func safeJoin(root, name string) (string, error) {
	if name == "" {
		return "", &Error{Op: "Get", Backend: "archive", Key: name, Kind: ErrInvalid, Err: fmt.Errorf("archive entry has empty name")}
	}
	if strings.HasPrefix(name, "/") || (len(name) >= 2 && name[1] == ':') {
		return "", &Error{Op: "Get", Backend: "archive", Key: name, Kind: ErrInvalid, Err: fmt.Errorf("archive entry has absolute path %q", name)}
	}
	clean := filepath.Clean(filepath.FromSlash(name))
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) || filepath.IsAbs(clean) {
		return "", &Error{Op: "Get", Backend: "archive", Key: name, Kind: ErrInvalid, Err: fmt.Errorf("archive entry escapes root: %q", name)}
	}
	full := filepath.Join(root, clean)
	if full != root && !strings.HasPrefix(full, root+string(filepath.Separator)) {
		return "", &Error{Op: "Get", Backend: "archive", Key: name, Kind: ErrInvalid, Err: fmt.Errorf("archive entry escapes root: %q", name)}
	}
	return full, nil
}

// validateSymlinkTarget rejects a symlink whose target -- resolved relative
// to linkDir (the symlink's own containing directory) -- would point
// outside root, or which is itself an absolute path.
func validateSymlinkTarget(root, linkDir, linkname string) error {
	if linkname == "" {
		return &Error{Op: "Get", Backend: "archive", Key: linkname, Kind: ErrInvalid, Err: fmt.Errorf("symlink has empty target")}
	}
	if filepath.IsAbs(filepath.FromSlash(linkname)) {
		return &Error{Op: "Get", Backend: "archive", Key: linkname, Kind: ErrInvalid, Err: fmt.Errorf("symlink has absolute target %q", linkname)}
	}
	resolved := filepath.Clean(filepath.Join(linkDir, filepath.FromSlash(linkname)))
	if resolved != root && !strings.HasPrefix(resolved, root+string(filepath.Separator)) {
		return &Error{Op: "Get", Backend: "archive", Key: linkname, Kind: ErrInvalid, Err: fmt.Errorf("symlink target escapes root: %q", linkname)}
	}
	return nil
}

// classifyOSErrKind maps a local-filesystem error (from the archive
// write/restore path, which always operates on a caller-controlled root,
// never a Backend) to an error Kind. Deliberately reuses classifyFSError's
// judgment (fs.go) rather than duplicating it.
func classifyOSErrKind(err error) error {
	classified := classifyFSError("", "", err)
	var se *Error
	if e, ok := classified.(*Error); ok {
		se = e
		return se.Kind
	}
	return ErrUnreachable
}

// ArchiveTo tars+zstds root directly into Backend at key via io.Pipe --
// never materialized on disk or in memory as a whole. On a Put error, the
// pipe's read side is closed with that error so the producer goroutine
// (walking the tree) unblocks on its next Write instead of leaking
// forever.
func ArchiveTo(ctx context.Context, b Backend, key, root string, opts ArchiveOptions) (ObjectInfo, error) {
	pr, pw := io.Pipe()

	writeErrCh := make(chan error, 1)
	go func() {
		_, err := WriteArchive(ctx, root, pw, opts)
		writeErrCh <- err
		_ = pw.CloseWithError(err)
	}()

	info, putErr := b.Put(ctx, key, pr, PutOptions{ContentType: "application/zstd"})
	if putErr != nil {
		_ = pr.CloseWithError(putErr)
		<-writeErrCh
		return ObjectInfo{}, putErr
	}
	if writeErr := <-writeErrCh; writeErr != nil {
		return ObjectInfo{}, writeErr
	}
	return info, nil
}

// RestoreFrom streams key from Backend into root, verifying the actually
// restored bytes' size+SHA256 against want. Never returns nil for a
// corrupted or truncated object -- see the doc comment on the ordering
// subtlety below.
func RestoreFrom(ctx context.Context, b Backend, key, root string, want FileEntry, opts ExtractOptions) error {
	rc, _, err := b.Get(ctx, key)
	if err != nil {
		return err
	}
	defer func() { _ = rc.Close() }()

	vr := newVerifyingReader(rc)
	if _, err := ReadArchive(ctx, vr, root, opts); err != nil {
		return err
	}

	// IMPORTANT ORDERING: tar's end-of-archive marker (two 512-byte zero
	// blocks) arrives before the underlying compressed stream is fully
	// exhausted -- zstd frame trailers, and in principle trailing padding
	// bytes, still follow it. Draining the rest of vr BEFORE calling
	// Verify ensures the digest covers everything actually streamed, not
	// just the bytes tar happened to consume; verifying too early would
	// check a partial digest and could pass on a truncated tail.
	if _, err := io.Copy(io.Discard, vr); err != nil {
		return &Error{Op: "Get", Backend: "archive", Key: key, Kind: ErrUnreachable, Err: err}
	}

	return vr.Verify("Get", key, want)
}
