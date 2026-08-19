package sandboxctl

import (
	"fmt"
	"os"
	"path/filepath"
)

// purgeRoot removes every entry directly under root except those named in
// keep (matching by base name), so a cold restore truly replaces the tree
// rather than merging into it -- storage.ReadArchive overwrites files
// present in the archive but never removes files absent from it.
// "lost+found" is always implicitly kept: it is root-owned 0700 on an ext4
// PVC and the sidecar runs as uid 1000 (see exclusions.go's own
// documentation of this exact hazard on the write path).
func purgeRoot(root string, keep ...string) (removed int, err error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("sandboxctl: reading %s: %w", root, err)
	}

	keepSet := make(map[string]struct{}, len(keep)+1)
	keepSet["lost+found"] = struct{}{}
	for _, k := range keep {
		keepSet[k] = struct{}{}
	}

	for _, e := range entries {
		if _, ok := keepSet[e.Name()]; ok {
			continue
		}
		target := filepath.Join(root, e.Name())
		if rmErr := os.RemoveAll(target); rmErr != nil {
			return removed, fmt.Errorf("sandboxctl: removing %s: %w", target, rmErr)
		}
		removed++
	}
	return removed, nil
}
