package firecracker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"k8s.io/klog/v2"

	runtimecontract "fast-sandbox/internal/runtime/contract"
	"fast-sandbox/internal/runtime/firecracker/imagecache"
)

// imageCacheDir holds content-addressed rootfs images:
//
//	<StateRoot>/images/<digest>/rootfs.img
//
// Images are produced by the OCI conversion pipeline (out of band); the
// driver never converts an image on the Sandbox create hot path.
const imageCacheDir = "images"

// rootfsImageName is the converted rootfs file inside each image directory.
const rootfsImageName = "rootfs.img"

// ErrImageNotReady reports that a rootfs image has not been converted and
// cached yet; the conversion build job must run before the Sandbox can boot.
// The sentinel lives in the runtime contract so the firecracker runtime-agent
// pull layer can reuse it; this alias keeps the driver's exported identity.
var ErrImageNotReady = runtimecontract.ErrImageNotReady

// imageKey derives the content-addressed cache key of an image reference.
func imageKey(image string) string {
	digest := sha256.Sum256([]byte(image))
	return hex.EncodeToString(digest[:])
}

// imageCachePath returns the rootfs image path for an image reference. The
// cache key is the reference digest, not the reference string, so cache
// entries are stable across naming schemes.
func imageCachePath(stateRoot, image string) (string, error) {
	if strings.TrimSpace(image) == "" {
		return "", fmt.Errorf("%w: image reference is required", ErrInvalidConfig)
	}
	return filepath.Join(stateRoot, imageCacheDir, imageKey(image), rootfsImageName), nil
}

// resolveRootfsImage returns the cached rootfs image path or ErrImageNotReady
// when the conversion pipeline has not produced it yet.
func resolveRootfsImage(stateRoot, image string) (string, error) {
	path, err := imageCachePath(stateRoot, image)
	if err != nil {
		return "", err
	}
	if info, err := os.Stat(path); err != nil || info.IsDir() {
		return "", fmt.Errorf("%w: %q", ErrImageNotReady, image)
	}
	return path, nil
}

// verifyRestorableImage verifies the COMPLETE restore set of an image is
// committed in the local cache: rootfs plus both golden snapshots. This is
// the single readiness criterion shared by async delivery and EnsureSandbox,
// so a partially delivered cache (rootfs committed while vmstate/memory are
// still transferring) never reports ready. Without it, a parked boot worker
// polls EnsureSandbox in the delivery window, and every attempt acquires a
// network slot only to release-destroy it on the snapshot check — burning
// the slot pool faster than replenishment and failing the cold create.
func verifyRestorableImage(stateRoot, image string) error {
	if _, err := resolveRootfsImage(stateRoot, image); err != nil {
		return err
	}
	if _, _, err := resolveRestoreSnapshotFiles(stateRoot, image); err != nil {
		return err
	}
	// Hand-seeded local images may have no artifact manifest. Published
	// entries must have every advertised native artifact at the right size.
	dir := filepath.Join(stateRoot, imageCacheDir, imageKey(image))
	payload, err := os.ReadFile(filepath.Join(dir, snapshotManifestName))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("%w: read manifest: %v", ErrImageNotReady, err)
	}
	var document struct {
		Files map[string]imagecache.File `json:"files"`
	}
	if err := json.Unmarshal(payload, &document); err != nil {
		return fmt.Errorf("%w: decode manifest: %v", ErrImageNotReady, err)
	}
	if document.Files != nil {
		files, err := imagecache.NativeFiles(document.Files)
		if err == nil {
			err = imagecache.CheckFiles(dir, files, false)
		}
		if err != nil {
			return fmt.Errorf("%w: %v", ErrImageNotReady, err)
		}
	}
	return nil
}

// listCachedImages returns the converted image references stored under the
// StateRoot, keyed by their content-addressed cache directory.
func listCachedImages(stateRoot string) ([]string, error) {
	base := filepath.Join(stateRoot, imageCacheDir)
	entries, err := os.ReadDir(base)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	images := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() || !validImageDigest(entry.Name()) {
			continue
		}
		if _, err := os.Stat(filepath.Join(base, entry.Name(), rootfsImageName)); err == nil {
			images = append(images, entry.Name())
		}
	}
	return images, nil
}

// instanceRootfsName is the per-Sandbox writable copy of the cached rootfs.
// The cached image stays immutable and content-addressed; the instance copy
// is the VM root drive.
const instanceRootfsName = "rootfs.img"

// prepareInstanceRootfs copies the cached rootfs image into the Sandbox state
// directory so the VM root drive is writable without mutating the cache. A
// reflink (CoW) copy is preferred: it shares extents with the cache and leaves
// no dirty pages, so the first fsync on the instance image (from debugfs
// delivery or the VM) is instant instead of forcing the full dirty writeback.
func prepareInstanceRootfs(stateRoot, image, instanceDir string) (string, error) {
	cached, err := resolveRootfsImage(stateRoot, image)
	if err != nil {
		return "", err
	}
	target := filepath.Join(instanceDir, instanceRootfsName)
	if err := copyReflinkOrCopy(cached, target); err != nil {
		return "", err
	}
	return target, nil
}

// reflinkOnly attempts a metadata-only CoW clone of source to target. It
// never falls back to a full copy: callers that need consistency (e.g. the
// snapshot rootfs clone) decide where a non-atomic full copy may run.
func reflinkOnly(source, target string) error {
	return exec.Command("cp", "--reflink=always", source, target).Run()
}

// copyReflinkOrCopy attempts a CoW reflink copy and falls back to a plain
// copy when the host filesystem does not support reflinks (e.g. ext4): the
// fallback pays a full rootfs copy (~1.7 GB/s) per create.
func copyReflinkOrCopy(source, target string) error {
	if reflinkOnly(source, target) == nil {
		return nil
	}
	klog.V(4).InfoS("reflink copy unavailable, falling back to a full copy", "source", source)
	_ = os.Remove(target)
	return copyFile(source, target)
}

func copyFile(source, target string) error {
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	output, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o640)
	if err != nil {
		return err
	}
	if _, err := io.Copy(output, input); err != nil {
		_ = output.Close()
		return err
	}
	return output.Close()
}

// defaultImageCacheLimitBytes bounds the converted-image cache. When the
// cache exceeds it, the GC evicts unreferenced images in least-frequently-used
// order. The quota includes rootfs, memory, VM state and metadata logical bytes.
const defaultImageCacheLimitBytes = 20 << 30

// referencedImages reads the effective restore references from durable
// Sandbox state. An unreadable record aborts collection rather than risking
// eviction of a recoverable VM's files.
func referencedImages(stateRoot string) (map[string]struct{}, error) {
	referenced := make(map[string]struct{})
	directories, err := listSandboxDirs(stateRoot)
	if err != nil {
		return nil, err
	}
	for _, directory := range directories {
		state, err := loadState(directory)
		if err != nil {
			// A directory without durable state is a deletion in progress;
			// it pins no image. Any other read failure aborts the whole
			// collection: the Sandbox might reference an image, and keeping a
			// few hundred MiB beats breaking a recoverable Sandbox.
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, fmt.Errorf("read Sandbox state %s for image GC: %w", directory, err)
		}
		reference, err := restoreReference(state.Config.Spec)
		if err != nil {
			return nil, err
		}
		referenced[imageKey(reference)] = struct{}{}
	}
	return referenced, nil
}

// cachedImageBytes counts all regular files, including sparse logical
// memory size. Logical quota and available filesystem blocks are different
// measures: reflink/shared extents cannot be summed as physical usage.
func cachedImageBytes(dir string) (int64, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, err
	}
	var total int64
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			return 0, err
		}
		if info.Mode().IsRegular() {
			total += info.Size()
		}
	}
	return total, nil
}

// garbageCollectImages enforces the logical-byte quota in LFU order.
// Pulls and active restores are protected by locks; durable references are
// checked again under each eviction lock, after any concurrent create.
func garbageCollectImages(stateRoot string, limitBytes int64, useCount map[string]int64) ([]string, error) {
	referenced, err := referencedImages(stateRoot)
	if err != nil {
		return nil, err
	}
	cached, err := listCachedImages(stateRoot)
	if err != nil {
		return nil, err
	}
	type entry struct {
		digest string
		bytes  int64
		uses   int64
	}
	entries := make([]entry, 0, len(cached))
	total := int64(0)
	for _, digest := range cached {
		// Only content-addressed cache directories (64 lowercase hex chars)
		// are eligible; anything else in the cache root is left untouched.
		if !validImageDigest(digest) {
			continue
		}
		size, err := cachedImageBytes(filepath.Join(stateRoot, imageCacheDir, digest))
		if err != nil {
			continue
		}
		total += size
		if _, used := referenced[digest]; used {
			continue
		}
		entries = append(entries, entry{digest: digest, bytes: size, uses: useCount[digest]})
	}
	// Least-frequently-used first; ties resolve deterministically by digest.
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].uses != entries[j].uses {
			return entries[i].uses < entries[j].uses
		}
		return entries[i].digest < entries[j].digest
	})
	removed := make([]string, 0, len(entries))
	for _, candidate := range entries {
		if total <= limitBytes {
			break
		}
		lock, err := imagecache.Acquire(context.Background(), stateRoot, candidate.digest, true, false)
		if errors.Is(err, imagecache.ErrBusy) {
			continue
		}
		if err != nil {
			return removed, err
		}
		// A create may have persisted a reference after the initial scan.
		// It holds a shared lock until that state is visible, so this scan
		// under the exclusive lock closes the scan/create/delete race.
		current, err := referencedImages(stateRoot)
		if err != nil {
			_ = lock.Close()
			return removed, err
		}
		if _, used := current[candidate.digest]; used {
			_ = lock.Close()
			continue
		}
		err = os.RemoveAll(filepath.Join(stateRoot, imageCacheDir, candidate.digest))
		_ = lock.Close()
		if err != nil {
			return removed, err
		}
		total -= candidate.bytes
		removed = append(removed, candidate.digest)
	}
	return removed, nil
}

// validImageDigest reports whether name is a lowercase sha256 hex digest.
func validImageDigest(name string) bool {
	if len(name) != sha256.Size*2 {
		return false
	}
	for _, character := range name {
		if !((character >= '0' && character <= '9') || (character >= 'a' && character <= 'f')) {
			return false
		}
	}
	return true
}

// ListImages implements RuntimeArtifactCache over the converted-image cache.
func (d *Driver) ListImages(_ context.Context) ([]string, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return listCachedImages(d.config.StateRoot)
}

// PullImage ensures the rootfs image is present in the local cache. With a
// runtime-agent configured, the pull proxies to the agent's PinImage (the
// node-level pull chain image -> index -> manifest -> digest-verified
// cache); an unreachable agent degrades to the local cache check so warm
// images never depend on agent availability. Conversion is out of band; an
// unconverted reference is reported as ErrImageNotReady instead of blocking
// the create path.
func (d *Driver) PullImage(ctx context.Context, image string) error {
	d.mu.RLock()
	stateRoot := d.config.StateRoot
	d.mu.RUnlock()
	client, err := d.agentClientOrNil()
	if err == nil && client != nil {
		if _, err := client.PinImage(ctx, d.warmPullRequestID(image), image); err != nil {
			if errors.Is(err, errAgentUnreachable) {
				// The agent is absent: fall through to the local cache.
			} else {
				return err
			}
		} else {
			d.touchImage(image)
			return nil
		}
	}
	if _, err := resolveRootfsImage(stateRoot, image); err != nil {
		return err
	}
	d.touchImage(image)
	return nil
}
