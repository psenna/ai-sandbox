package sandboxctl

import (
	"io/fs"
	"strings"
)

// cacheExcludePaths are container image/layer cache roots, anchored
// relative to the archived tree's root. #24's graph root, per the epic's
// explicit exclusion: with engine: none (the only implemented engine)
// none of these paths can actually exist inside an archived root today --
// this list is the FORWARD guard for #24, and what exclusions_test.go
// asserts against.
var cacheExcludePaths = []string{
	".local/share/containers", // #24's graph root, per the epic's explicit exclusion
	".config/containers",      // #24's graph root, per the epic's explicit exclusion
	".cache/containers",       // #24's graph root, per the epic's explicit exclusion
	".local/share/docker",     // #24's graph root, per the epic's explicit exclusion
	".docker/buildx",          // #24's graph root, per the epic's explicit exclusion
	"var/lib/containers",      // #24's graph root, per the epic's explicit exclusion
	"var/lib/docker",          // #24's graph root, per the epic's explicit exclusion
}

// SnapshotExclude is storage.ArchiveOptions.Exclude for every freeze
// archive. It is an EXPLICIT, TESTED list, not an implicit consequence of
// which volumes happen to be mounted (per the epic). Two of these rules
// guard against real bugs in internal/storage.WriteArchive/ReadArchive
// found while planning this issue:
//
//  1. lost+found: an ext4-backed PVC has a root-owned 0700 lost+found at
//     /workspace root. storage.WriteArchive uses filepath.WalkDir and
//     ABORTS THE ENTIRE ARCHIVE on the first walk error -- the sidecar
//     runs as UID 1000, so lost+found would make every freeze on such a
//     cluster fail with a permission error.
//  2. non-regular files: WriteArchive will happily write a socket/FIFO/
//     device header via tar.FileInfoHeader, but storage.ReadArchive
//     REFUSES anything that isn't TypeReg/TypeDir/TypeSymlink. A stray
//     unix socket under /workspace would produce a snapshot that uploads
//     successfully and can NEVER be restored -- exactly the "half-written
//     snapshot a restore would accept" failure class this issue exists to
//     prevent.
//
// #29 adds two more exclusions, both under .sandbox/:
//
//  3. warm-cache.json: a marker from freeze N-1 must never be baked into
//     snapshot N's tarball. It is written fresh by THIS freeze (snapshot.go's
//     final step) after the manifest it describes already exists, so an
//     archived copy would always describe a manifest one freeze stale --
//     exactly the kind of self-referential staleness ValidateWarmMarker
//     exists to catch, but which is simplest to never create in the first
//     place.
//  4. restore-in-progress: the sentinel restore.go writes as its literal
//     first action and removes on success. If a snapshot somehow captured
//     it (it shouldn't -- Freeze itself refuses to run while the sentinel
//     exists, see snapshot.go's top-of-function check), restoring that
//     snapshot into a fresh workspace would make the NEXT freeze believe a
//     restore was still in progress and refuse to run.
func SnapshotExclude(rel string, fi fs.FileInfo) bool {
	mode := fi.Mode()

	// (1) non-regular entries -- unrestorable by storage.ReadArchive;
	// excluded, not tolerated. Regular files, directories, and symlinks are
	// the only types ReadArchive accepts.
	if !mode.IsRegular() && mode&fs.ModeSymlink == 0 && !fi.IsDir() {
		return true
	}

	// (2) lost+found at the archived tree's root -- root-owned on ext4
	// PVCs; would abort the whole walk.
	if rel == "lost+found" {
		return true
	}

	// (3) container image/layer cache paths.
	for _, p := range cacheExcludePaths {
		if rel == p || strings.HasPrefix(rel, p+"/") {
			return true
		}
	}

	// (4) reserved scratch under the marker directory.
	if rel == ".sandbox/tmp" || strings.HasPrefix(rel, ".sandbox/tmp/") {
		return true
	}

	// (5) the warm-cache marker: never bake a stale one into the archive it
	// describes. See the doc comment above.
	if rel == ".sandbox/"+warmMarkerFileName {
		return true
	}

	// (6) the restore-in-progress sentinel: never restore this into a fresh
	// workspace. See the doc comment above.
	if rel == ".sandbox/"+restoreInProgressFileName {
		return true
	}

	return false
}
