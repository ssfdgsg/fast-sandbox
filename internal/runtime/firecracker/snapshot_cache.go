package firecracker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"syscall"

	"golang.org/x/sys/unix"

	"fast-sandbox/internal/artifacts"
	"fast-sandbox/internal/runtime/firecracker/imagecache"
)

// snapshotPrewarmEnv enables same-node caching after template publication.
// Checkpoints retain their existing best-effort local caching independently.
const snapshotPrewarmEnv = "FAST_SANDBOX_SNAPSHOT_PREWARM"

type cacheCommitOutcome string

const (
	cacheCommitted       cacheCommitOutcome = "committed"
	cacheAlreadyPresent  cacheCommitOutcome = "already_cached"
	cacheSkippedBusy     cacheCommitOutcome = "skipped_busy"
	cacheSkippedConflict cacheCommitOutcome = "skipped_conflict"
	cacheSkippedCapacity cacheCommitOutcome = "skipped_capacity"
	cacheSkippedFS       cacheCommitOutcome = "skipped_filesystem"
	cacheCommitFailed    cacheCommitOutcome = "failed"
)

func snapshotPrewarmEnabled() bool {
	enabled, _ := strconv.ParseBool(os.Getenv(snapshotPrewarmEnv))
	return enabled
}

// commitPublishedCache consumes staging on EVERY path. The caller has already
// published successfully, and still owns these immutable, producer-hashed
// files. Only metadata is read here; artifact bytes are neither rehashed nor
// copied. Another filesystem, an active cache user, or a conflicting alias
// causes a best-effort skip. In particular, this does not implement tag refresh.
func (d *Driver) commitPublishedCache(ctx context.Context, reference, digest, staging string) (cacheCommitOutcome, error) {
	defer os.RemoveAll(staging)
	if reference == "" || !imagecache.ValidKey(digest) {
		return cacheCommitFailed, fmt.Errorf("invalid published cache reference or artifact digest")
	}
	d.mu.RLock()
	stateRoot, limit := d.config.StateRoot, d.imageCacheLimitBytes
	d.mu.RUnlock()
	if limit <= 0 {
		limit = defaultImageCacheLimitBytes
	}
	lock, err := imagecache.Acquire(ctx, stateRoot, imageKey(reference), true, false)
	if errors.Is(err, imagecache.ErrBusy) {
		return cacheSkippedBusy, nil
	}
	if err != nil {
		return cacheCommitFailed, err
	}
	defer lock.Close()

	cacheRoot := filepath.Join(stateRoot, imageCacheDir)
	cacheDir := filepath.Join(cacheRoot, imageKey(reference))
	if _, err := os.Lstat(cacheDir); err == nil {
		payload, files, readErr := imagecache.ReadManifest(cacheDir)
		if readErr == nil && artifacts.SHA256Of(payload) == digest && imagecache.CheckFiles(cacheDir, files, false) == nil {
			d.touchImage(reference)
			return cacheAlreadyPresent, nil
		}
		// Even an incomplete entry belongs to the pull/repair path; never
		// overwrite it just because a new snapshot reused its template name.
		return cacheSkippedConflict, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return cacheCommitFailed, err
	}

	payload, files, err := imagecache.ReadManifest(staging)
	if err != nil {
		return cacheCommitFailed, err
	}
	if artifacts.SHA256Of(payload) != digest {
		return cacheCommitFailed, fmt.Errorf("published digest differs from staged manifest")
	}
	if err := imagecache.CheckFiles(staging, files, true); err != nil {
		return cacheCommitFailed, err
	}
	need, err := cachedImageBytes(staging)
	if err != nil {
		return cacheCommitFailed, err
	}
	if err := os.MkdirAll(cacheRoot, 0o750); err != nil {
		return cacheCommitFailed, err
	}
	// Moving already allocated files needs directory metadata, not another
	// full set's free blocks. Leave a small metadata reserve. This is an
	// admission hint, not a physical-space reservation against other writers.
	if stagingFree(cacheRoot) < 1<<20 {
		return cacheSkippedCapacity, nil
	}
	cached, err := listCachedImages(stateRoot)
	if err != nil {
		return cacheCommitFailed, err
	}
	for _, key := range cached {
		size, sizeErr := cachedImageBytes(filepath.Join(cacheRoot, key))
		if errors.Is(sizeErr, os.ErrNotExist) {
			continue // a concurrent GC finished this entry
		}
		if sizeErr != nil {
			return cacheCommitFailed, sizeErr
		}
		if need > limit || size > limit-need {
			return cacheSkippedCapacity, nil
		}
		need += size
	}
	if need > limit {
		return cacheSkippedCapacity, nil
	}
	tmp, err := os.MkdirTemp(cacheRoot, ".snapshot-*")
	if err != nil {
		return cacheCommitFailed, err
	}
	defer os.RemoveAll(tmp)
	for _, file := range files {
		if err := os.Rename(filepath.Join(staging, file.Published), filepath.Join(tmp, file.Cached)); err != nil {
			if errors.Is(err, syscall.EXDEV) {
				return cacheSkippedFS, nil
			}
			return cacheCommitFailed, err
		}
	}
	if err := os.Rename(filepath.Join(staging, snapshotManifestName), filepath.Join(tmp, snapshotManifestName)); err != nil {
		return cacheCommitFailed, err
	}
	// Unlike Rename, NOREPLACE also refuses an existing empty directory.
	// A reader sees either no entry or the entire ready set. This provides
	// atomic visibility, not crash durability; the remote set is authoritative.
	if err := unix.Renameat2(unix.AT_FDCWD, tmp, unix.AT_FDCWD, cacheDir, unix.RENAME_NOREPLACE); err != nil {
		if errors.Is(err, unix.EEXIST) {
			return cacheSkippedConflict, nil
		}
		if errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EINVAL) || errors.Is(err, unix.EOPNOTSUPP) {
			return cacheSkippedFS, nil
		}
		return cacheCommitFailed, err
	}
	d.touchImage(reference)
	return cacheCommitted, nil
}
