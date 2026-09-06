package filestore

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"sort"
	"strings"
	"syscall"
	"time"
)

// Sentinel errors. Callers match with the Is* helpers below, never on message
// text. internal/api maps them to HTTP status codes.
var (
	// ErrInvalidPath reports a relative path that failed segment validation
	// (empty, "..", absolute, a backslash or control byte, or too long) --
	// rejected before any syscall runs.
	ErrInvalidPath = errors.New("invalid file-store path")
	// ErrNotFound reports that the named entry does not exist.
	ErrNotFound = errors.New("file-store entry not found")
	// ErrExists reports that an entry already exists where one was not
	// expected.
	ErrExists = errors.New("file-store entry already exists")
	// ErrNotDir reports that a path component that had to be a directory was
	// not one -- e.g. listing a regular file, or descending through one.
	ErrNotDir = errors.New("file-store entry is not a directory")
	// ErrIsDir reports that a directory was passed where a file was required
	// (Open, download).
	ErrIsDir = errors.New("file-store entry is a directory")
	// ErrTooLarge reports that an upload exceeded the caller's byte cap.
	ErrTooLarge = errors.New("file exceeds the maximum allowed size")
)

// IsInvalidPath reports whether err was caused by a rejected path.
func IsInvalidPath(err error) bool { return errors.Is(err, ErrInvalidPath) }

// IsNotFound reports whether err was caused by a missing entry.
func IsNotFound(err error) bool { return errors.Is(err, ErrNotFound) }

// IsTooLarge reports whether err was caused by an over-cap upload.
func IsTooLarge(err error) bool { return errors.Is(err, ErrTooLarge) }

// IsExists reports whether err was caused by an entry already existing.
func IsExists(err error) bool { return errors.Is(err, ErrExists) }

// IsNotDir reports whether err was caused by treating a non-directory as a
// directory (or a non-regular file as a downloadable one).
func IsNotDir(err error) bool { return errors.Is(err, ErrNotDir) }

// AgentsDir is the top-level directory under the store root that holds every
// agent's private files, at AgentsDir/<id>/. SharedDir is the other top-level
// directory: a common area mounted read-only into every agent and writable
// only by the operator (through this package's own API).
const (
	AgentsDir = "agents"
	SharedDir = "shared"
)

// maxSegmentLen and maxPathLen bound a single path segment and the whole
// joined relative path.
const (
	maxSegmentLen = 255
	maxPathLen    = 1024
)

// Store is a hardened view over one root directory. It is safe for concurrent
// use (os.Root is, and Store adds no mutable shared state).
type Store struct {
	root string
	r    *os.Root
}

// Entry is one file or directory in the store.
type Entry struct {
	Name string `json:"name"`
	// Path is slash-separated and relative to the store root.
	Path    string    `json:"path"`
	IsDir   bool      `json:"is_dir"`
	Size    int64     `json:"size"`
	ModTime time.Time `json:"mod_time"`
}

// New opens root as a store. It creates root if it is missing and its parent
// exists, requires it to be a directory, and probes that the operator can
// write to it (create a temp file, then remove it). Any failure is returned;
// callers WARN and run without the file store rather than aborting startup.
func New(root string) (*Store, error) {
	info, err := os.Stat(root)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if mkErr := os.Mkdir(root, 0o750); mkErr != nil {
			return nil, fmt.Errorf("creating the file-store root %q: %w", root, mkErr)
		}
	case err != nil:
		return nil, fmt.Errorf("stat-ing the file-store root %q: %w", root, err)
	case !info.IsDir():
		return nil, fmt.Errorf("the file-store root %q: %w", root, ErrNotDir)
	}

	r, err := os.OpenRoot(root)
	if err != nil {
		return nil, fmt.Errorf("opening the file-store root %q: %w", root, err)
	}

	probe, err := os.CreateTemp(root, ".writable-probe-*") //nolint:gosec // G304: root is operator config, and this only proves the dir is writable
	if err != nil {
		_ = r.Close()
		return nil, fmt.Errorf("the file-store root %q is not writable: %w", root, err)
	}
	probeName := probe.Name()
	_ = probe.Close()
	_ = os.Remove(probeName)

	s := &Store{root: root, r: r}

	// shared/ must exist before any agent container is created: the agent
	// mounts it as a read-only volume subpath, and the daemon requires a
	// subpath to already exist. Created here (not per agent) because it is a
	// singleton common area, not per-agent.
	if err := s.EnsureSharedDir(); err != nil {
		_ = r.Close()
		return nil, err
	}
	return s, nil
}

// EnsureSharedDir creates the shared/ common area if it does not exist. It is
// idempotent and never touches existing contents. Mode 0o755: every agent
// mounts it read-only, so only the operator (running as root, writing through
// this package) ever needs to write here.
func (s *Store) EnsureSharedDir() error {
	if err := s.r.MkdirAll(SharedDir, 0o755); err != nil {
		return fmt.Errorf("creating the shared file-store directory %q: %w", SharedDir, err)
	}
	if err := s.r.Chmod(SharedDir, 0o755); err != nil {
		return fmt.Errorf("setting the mode of the shared file-store directory %q: %w", SharedDir, err)
	}
	return nil
}

// Close releases the store's directory handle.
func (s *Store) Close() error { return s.r.Close() }

// Root returns the absolute path the store is rooted at.
func (s *Store) Root() string { return s.root }

// List returns the entries directly under rel, directories first then files,
// each group sorted by name. rel == "" lists the root. The result is never
// nil.
func (s *Store) List(rel string) ([]Entry, error) {
	segs, err := cleanRel(rel)
	if err != nil {
		return nil, err
	}
	name := openName(segs)

	f, err := s.r.Open(name)
	if err != nil {
		return nil, s.wrapOpenErr(err)
	}
	defer func() { _ = f.Close() }()

	dirents, err := f.ReadDir(-1)
	if err != nil {
		if errors.Is(err, syscall.ENOTDIR) {
			return nil, fmt.Errorf("%q is not a directory: %w", relDisplay(segs), ErrNotDir)
		}
		return nil, fmt.Errorf("reading directory %q: %w", relDisplay(segs), err)
	}

	out := make([]Entry, 0, len(dirents))
	for _, de := range dirents {
		fi, infoErr := de.Info()
		if infoErr != nil {
			// The entry vanished between ReadDir and Info; skip it.
			continue
		}
		out = append(out, Entry{
			Name:    de.Name(),
			Path:    joinRel(segs, de.Name()),
			IsDir:   fi.IsDir(),
			Size:    fi.Size(),
			ModTime: fi.ModTime(),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].IsDir != out[j].IsDir {
			return out[i].IsDir
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

// Stat returns the entry at rel.
func (s *Store) Stat(rel string) (Entry, error) {
	segs, err := cleanRel(rel)
	if err != nil {
		return Entry{}, err
	}
	if len(segs) == 0 {
		fi, statErr := s.r.Stat(".")
		if statErr != nil {
			return Entry{}, fmt.Errorf("stat-ing the store root: %w", statErr)
		}
		return Entry{Name: "", Path: "", IsDir: fi.IsDir(), Size: fi.Size(), ModTime: fi.ModTime()}, nil
	}
	fi, err := s.r.Stat(openName(segs))
	if err != nil {
		return Entry{}, s.wrapOpenErr(err)
	}
	return Entry{
		Name:    segs[len(segs)-1],
		Path:    relDisplay(segs),
		IsDir:   fi.IsDir(),
		Size:    fi.Size(),
		ModTime: fi.ModTime(),
	}, nil
}

// Open opens the file at rel for reading. The caller must Close it. A
// directory yields ErrIsDir; a missing entry yields ErrNotFound.
func (s *Store) Open(rel string) (*os.File, Entry, error) {
	segs, err := cleanRel(rel)
	if err != nil {
		return nil, Entry{}, err
	}
	if len(segs) == 0 {
		return nil, Entry{}, fmt.Errorf("%q: %w", "", ErrIsDir)
	}
	// O_NONBLOCK so that opening an entry an agent planted as a FIFO/device
	// inside its own store dir returns immediately instead of blocking this
	// request goroutine forever waiting for a peer; the mode check below then
	// rejects anything that is not a plain file. On a regular file O_NONBLOCK
	// is a no-op on Linux.
	f, err := s.r.OpenFile(openName(segs), os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, Entry{}, s.wrapOpenErr(err)
	}
	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, Entry{}, fmt.Errorf("stat-ing %q: %w", relDisplay(segs), err)
	}
	if fi.IsDir() {
		_ = f.Close()
		return nil, Entry{}, fmt.Errorf("%q: %w", relDisplay(segs), ErrIsDir)
	}
	if !fi.Mode().IsRegular() {
		_ = f.Close()
		return nil, Entry{}, fmt.Errorf("%q is not a regular file: %w", relDisplay(segs), ErrNotFound)
	}
	return f, Entry{
		Name:    segs[len(segs)-1],
		Path:    relDisplay(segs),
		IsDir:   false,
		Size:    fi.Size(),
		ModTime: fi.ModTime(),
	}, nil
}

// Save writes r to rel atomically: it streams into a temp file in the same
// directory, then renames it into place, replacing any existing file. At most
// maxBytes are accepted; a larger stream yields ErrTooLarge and leaves no
// partial file behind. The parent directory must already exist (ErrNotFound
// otherwise).
func (s *Store) Save(rel string, r io.Reader, maxBytes int64) (Entry, error) {
	segs, err := cleanRel(rel)
	if err != nil {
		return Entry{}, err
	}
	if len(segs) == 0 {
		return Entry{}, fmt.Errorf("%q: %w", "", ErrIsDir)
	}

	dirSegs := segs[:len(segs)-1]
	if len(dirSegs) > 0 {
		fi, statErr := s.r.Stat(openName(dirSegs))
		if statErr != nil {
			return Entry{}, s.wrapOpenErr(statErr)
		}
		if !fi.IsDir() {
			return Entry{}, fmt.Errorf("%q: %w", relDisplay(dirSegs), ErrNotDir)
		}
	}

	tmpSegs := append(append([]string(nil), dirSegs...), ".upload-"+randToken()+".tmp")
	tmpName := openName(tmpSegs)
	cleanupTmp := true
	defer func() {
		if cleanupTmp {
			_ = s.r.Remove(tmpName)
		}
	}()

	tmp, err := s.r.OpenFile(tmpName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return Entry{}, fmt.Errorf("creating a temp file in %q: %w", relDisplay(dirSegs), err)
	}

	n, err := io.Copy(tmp, io.LimitReader(r, maxBytes+1))
	closeErr := tmp.Close()
	if err != nil {
		return Entry{}, fmt.Errorf("writing %q: %w", relDisplay(segs), err)
	}
	if closeErr != nil {
		return Entry{}, fmt.Errorf("closing the temp file for %q: %w", relDisplay(segs), closeErr)
	}
	if n > maxBytes {
		return Entry{}, fmt.Errorf("%q is larger than %d bytes: %w", relDisplay(segs), maxBytes, ErrTooLarge)
	}

	if err := s.r.Chmod(tmpName, 0o644); err != nil {
		return Entry{}, fmt.Errorf("setting permissions on %q: %w", relDisplay(segs), err)
	}
	if err := s.r.Rename(tmpName, openName(segs)); err != nil {
		return Entry{}, fmt.Errorf("moving the upload into place at %q: %w", relDisplay(segs), err)
	}
	cleanupTmp = false

	fi, err := s.r.Stat(openName(segs))
	if err != nil {
		return Entry{}, fmt.Errorf("stat-ing %q after write: %w", relDisplay(segs), err)
	}
	return Entry{
		Name:    segs[len(segs)-1],
		Path:    relDisplay(segs),
		IsDir:   false,
		Size:    fi.Size(),
		ModTime: fi.ModTime(),
	}, nil
}

// Mkdir creates rel and any missing parents. It is idempotent; a plain file
// in the way of any component is an error.
func (s *Store) Mkdir(rel string) error {
	segs, err := cleanRel(rel)
	if err != nil {
		return err
	}
	if len(segs) == 0 {
		return nil
	}
	if err := s.r.MkdirAll(openName(segs), 0o755); err != nil {
		return fmt.Errorf("creating directory %q: %w", relDisplay(segs), err)
	}
	return nil
}

// Remove deletes rel and everything under it. A missing entry is success
// (idempotent). It refuses to remove the root or the top-level agents/
// directory (ErrInvalidPath) -- those are structural, not user data.
func (s *Store) Remove(rel string) error {
	segs, err := cleanRel(rel)
	if err != nil {
		return err
	}
	if len(segs) == 0 {
		return fmt.Errorf("refusing to remove the store root: %w", ErrInvalidPath)
	}
	if len(segs) == 1 && (segs[0] == AgentsDir || segs[0] == SharedDir) {
		return fmt.Errorf("refusing to remove the top-level %q directory: %w", segs[0], ErrInvalidPath)
	}
	if err := s.r.RemoveAll(openName(segs)); err != nil {
		return fmt.Errorf("removing %q: %w", relDisplay(segs), err)
	}
	return nil
}

// AgentSubpath returns the volume-relative subpath an agent's files live at,
// "agents/<id>". The caller is expected to pass a store-generated agent ID
// (one safe segment); EnsureAgentDir validates it before this matters.
func AgentSubpath(id string) string {
	return AgentsDir + "/" + id
}

// EnsureAgentDir creates agents/<id>/ if it does not exist and makes it
// world-writable, so the agent container (uid 1000) can write into the
// subpath mount while the operator (root) created it. It is idempotent and
// never touches existing contents. id must be a single safe path segment.
func (s *Store) EnsureAgentDir(id string) error {
	if !validSegment(id) {
		return fmt.Errorf("agent id %q is not a single safe path segment: %w", id, ErrInvalidPath)
	}
	name := AgentsDir + "/" + id
	// G301/G302: the agent process runs as uid 1000 and the operator as root;
	// this directory is per-agent, on a volume no unprivileged process can
	// otherwise reach, and 0777 is what lets the agent write its own files
	// through the subpath mount. MkdirAll's mode is masked by umask, so the
	// explicit Chmod below is what actually guarantees it.
	if err := s.r.MkdirAll(name, 0o777); err != nil { //nolint:gosec // G301: intentional world-writable per-agent dir, see comment
		return fmt.Errorf("creating the agent file-store directory %q: %w", name, err)
	}
	if err := s.r.Chmod(name, 0o777); err != nil { //nolint:gosec // G302: intentional world-writable per-agent dir, see comment
		return fmt.Errorf("making the agent file-store directory %q writable: %w", name, err)
	}
	return nil
}

// RemoveAgentDir deletes agents/<id>/ and everything in it. A missing
// directory is success. id must be a single safe path segment.
func (s *Store) RemoveAgentDir(id string) error {
	if !validSegment(id) {
		return fmt.Errorf("agent id %q is not a single safe path segment: %w", id, ErrInvalidPath)
	}
	name := AgentsDir + "/" + id
	if err := s.r.RemoveAll(name); err != nil {
		return fmt.Errorf("removing the agent file-store directory %q: %w", name, err)
	}
	return nil
}

// wrapOpenErr normalises os.Root's open/stat errors to the package sentinels.
func (s *Store) wrapOpenErr(err error) error {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("%w", ErrNotFound)
	case errors.Is(err, fs.ErrInvalid) || errors.Is(err, os.ErrInvalid):
		// os.Root rejects an absolute path passed to Open/Stat this way.
		return fmt.Errorf("path escapes the store root: %w", ErrInvalidPath)
	case isPathEscapes(err):
		// os.Root refused to follow a symlink whose target leaves the root
		// (its errPathEscapes is an unexported, unwrapped errors.New, so this
		// matches on the message -- the only option the stdlib leaves).
		return fmt.Errorf("path escapes the store root: %w", ErrInvalidPath)
	case errors.Is(err, syscall.ENOTDIR):
		return fmt.Errorf("a path component is not a directory: %w", ErrNotDir)
	default:
		return err
	}
}

// isPathEscapes matches os.Root's unexported errPathEscapes ("path escapes
// from parent"), returned when a symlink component's target would leave the
// root. There is no exported sentinel for it as of Go 1.25.
func isPathEscapes(err error) bool {
	return err != nil && strings.Contains(err.Error(), "path escapes from parent")
}

// cleanRel splits rel into validated path segments. It never uses
// path.Clean/filepath.Join (which would collapse "..") -- it validates each
// literal segment instead. An empty rel yields an empty slice, which is valid
// only for List/Stat/Mkdir/Remove's own root handling.
func cleanRel(rel string) ([]string, error) {
	if strings.HasPrefix(rel, "/") {
		return nil, fmt.Errorf("path %q is absolute: %w", rel, ErrInvalidPath)
	}
	rel = strings.Trim(rel, "/")
	if rel == "" {
		return nil, nil
	}
	if len(rel) > maxPathLen {
		return nil, fmt.Errorf("path is longer than %d bytes: %w", maxPathLen, ErrInvalidPath)
	}
	parts := strings.Split(rel, "/")
	for _, p := range parts {
		if !validSegment(p) {
			return nil, fmt.Errorf("path segment %q is not allowed: %w", p, ErrInvalidPath)
		}
	}
	return parts, nil
}

// validSegment reports whether one path segment is safe: non-empty, not "."
// or "..", no separators or control bytes, within the length cap.
func validSegment(seg string) bool {
	if seg == "" || seg == "." || seg == ".." {
		return false
	}
	if len(seg) > maxSegmentLen {
		return false
	}
	for _, r := range seg {
		if r == '/' || r == '\\' || r == 0x00 || r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// openName renders validated segments as the name os.Root wants. An empty
// slice means the root itself, which os.Root spells ".".
func openName(segs []string) string {
	if len(segs) == 0 {
		return "."
	}
	return path.Join(segs...)
}

// relDisplay renders validated segments as a slash-joined relative path, for
// error messages and Entry.Path.
func relDisplay(segs []string) string { return strings.Join(segs, "/") }

// randToken returns a short random hex string for the ".upload-<token>.tmp"
// name, so two concurrent Saves to the same target never collide on the temp
// file.
func randToken() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand.Read never fails on the platforms this runs on; fall
		// back to a time-based token rather than panic.
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

// joinRel appends one more validated name to segs and renders the result.
func joinRel(segs []string, name string) string {
	if len(segs) == 0 {
		return name
	}
	return strings.Join(segs, "/") + "/" + name
}
