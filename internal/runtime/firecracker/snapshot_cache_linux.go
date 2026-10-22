//go:build linux

package firecracker

import "golang.org/x/sys/unix"

// installSnapshotCache atomically exposes a complete entry without
// replacing an existing directory, including an empty one.
func installSnapshotCache(source, target string) error {
	return unix.Renameat2(unix.AT_FDCWD, source, unix.AT_FDCWD, target, unix.RENAME_NOREPLACE)
}
