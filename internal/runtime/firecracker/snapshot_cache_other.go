//go:build !linux

package firecracker

import "syscall"

// Native VM execution requires Linux. Other platforms support compilation
// and unit tests; publication can succeed while local installation skips.
func installSnapshotCache(_, _ string) error {
	return syscall.EOPNOTSUPP
}
