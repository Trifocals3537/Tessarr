package manager

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/Trifocals3537/tessarr/internal/config"
	"github.com/Trifocals3537/tessarr/internal/safepath"
	"github.com/Trifocals3537/tessarr/pkg/storage"
)

// Only symlink actions may encounter an existing symlink leaf. They must use
// createOwnedUsenetSymlink to prove its owner and exact current mount target.
// File-writing actions retain the stricter no-symlink path validation.
func usenetActionFilePath(downloadRoot string, entry *storage.Entry, name string) (string, error) {
	switch entry.Action {
	case config.DownloadActionDownload, config.DownloadActionStrm, config.DownloadActionNone:
		return safeUsenetFilePath(downloadRoot, entry, name)
	default:
		return usenetSymlinkPath(downloadRoot, entry, name)
	}
}

func usenetSymlinkPath(downloadRoot string, entry *storage.Entry, name string) (string, error) {
	entryPath, err := safeUsenetEntryDownloadPath(downloadRoot, entry)
	if err != nil {
		return "", err
	}
	if isReservedUsenetPrivateName(name) {
		return "", fmt.Errorf("NZB file name %q is reserved for internal ownership state", name)
	}
	if err := safepath.ValidateIdentifier(name); err != nil {
		return "", fmt.Errorf("invalid NZB file name: %w", err)
	}
	return filepath.Join(entryPath, name), nil
}

func openOwnedUsenetEntryRoot(downloadRoot string, entry *storage.Entry) (string, *os.Root, error) {
	entryPath, err := safeUsenetEntryDownloadPath(downloadRoot, entry)
	if err != nil {
		return "", nil, err
	}
	ownerID, err := canonicalUsenetOwnerID(entry.InfoHash)
	if err != nil {
		return "", nil, err
	}
	root, _, err := safepath.OpenRoot(entryPath)
	if err != nil {
		return "", nil, fmt.Errorf("open owned NZB release directory: %w", err)
	}
	owner, err := readUsenetOwnerMarker(root)
	if err != nil || owner != ownerID {
		_ = root.Close()
		if err != nil {
			return "", nil, fmt.Errorf("verify NZB ownership marker: %w", err)
		}
		return "", nil, fmt.Errorf("NZB release directory is owned by %q, not %q", owner, ownerID)
	}
	return entryPath, root, nil
}

// An interrupted symlink action is resumed only through a pinned, owned
// release directory. Matching absolute or relative links are reused; other
// links and regular files are preserved and rejected. Never remove an existing
// destination to make a retry succeed.
func createOwnedUsenetSymlink(downloadRoot string, entry *storage.Entry, name, target string, relative bool) (string, error) {
	linkPath, err := usenetSymlinkPath(downloadRoot, entry, name)
	if err != nil {
		return "", err
	}
	if err := safepath.RejectSymlinks(target); err != nil {
		return "", fmt.Errorf("refusing NZB mount source: %w", err)
	}
	entryPath, root, err := openOwnedUsenetEntryRoot(downloadRoot, entry)
	if err != nil {
		return "", err
	}
	defer root.Close()
	if err := rejectPortableSiblingAlias(root, ".", name); err != nil {
		return "", fmt.Errorf("NZB symlink destination: %w", err)
	}
	storedTarget, err := symlinkTarget(linkPath, target, relative)
	if err != nil {
		return "", err
	}
	if err := root.Symlink(storedTarget, name); err != nil && !os.IsExist(err) {
		return "", fmt.Errorf("create NZB symlink %q: %w", name, err)
	}
	before, err := root.Lstat(name)
	if err != nil || before.Mode()&os.ModeSymlink == 0 {
		if err != nil {
			return "", fmt.Errorf("inspect NZB symlink %q: %w", name, err)
		}
		return "", fmt.Errorf("NZB symlink destination %q is not a symlink", name)
	}
	actualTarget, err := root.Readlink(name)
	if err != nil {
		return "", fmt.Errorf("read NZB symlink %q: %w", name, err)
	}
	after, err := root.Lstat(name)
	if err != nil || !os.SameFile(before, after) || !sameSymlinkTarget(linkPath, actualTarget, storedTarget) {
		if err != nil {
			return "", fmt.Errorf("reinspect NZB symlink %q: %w", name, err)
		}
		return "", fmt.Errorf("NZB symlink %q changed or points to a different source", name)
	}
	if err := safepath.VerifyOpenRoot(root, entryPath); err != nil {
		return "", fmt.Errorf("verify NZB release directory: %w", err)
	}
	if err := usenetSyncDirectory(root); err != nil {
		return "", fmt.Errorf("sync NZB symlink directory: %w", err)
	}
	return linkPath, nil
}
