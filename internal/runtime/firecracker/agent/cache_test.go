package agent

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"fast-sandbox/internal/runtime/firecracker/imagecache"
)

// TestFileMatchesSizeFastPath verifies the stat-first size check: a file
// with the wrong size is rejected without reading (the fast path), a file
// with the right size but wrong content is rejected by the digest, and an
// exact match passes.
func TestFileMatchesSizeFastPath(t *testing.T) {
	content := bytes.Repeat([]byte{0xab}, 1<<20)
	file := nativeFile{publish: "rootfs.ext4", cache: "rootfs.img", sha256: sha256Hex(content), sizeBytes: int64(len(content))}
	path := filepath.Join(t.TempDir(), "rootfs.img")

	// Missing file.
	match, err := fileMatches(path, file)
	require.ErrorIs(t, err, os.ErrNotExist)
	require.False(t, match)

	// Wrong size: rejected by stat, no content read needed.
	require.NoError(t, os.WriteFile(path, []byte("tiny"), cacheFileMode))
	match, err = fileMatches(path, file)
	require.NoError(t, err)
	require.False(t, match)

	// Right size, wrong content: caught by the digest.
	wrong := bytes.Repeat([]byte{0x00}, len(content))
	require.NoError(t, os.WriteFile(path, wrong, cacheFileMode))
	match, err = fileMatches(path, file)
	require.NoError(t, err)
	require.False(t, match)

	// Exact match.
	require.NoError(t, os.WriteFile(path, content, cacheFileMode))
	match, err = fileMatches(path, file)
	require.NoError(t, err)
	require.True(t, match)
}

func TestCachedManifestDigestWaitsForWriter(t *testing.T) {
	root, image := t.TempDir(), "contended-template"
	dir := imageDir(root, image)
	require.NoError(t, os.MkdirAll(dir, cacheDirMode))
	payload := testManifest(map[string][]byte{
		"rootfs.ext4": []byte("rootfs"), "vmstate.snap": []byte("state"), "memory.snap": []byte("memory"),
	})
	require.NoError(t, os.WriteFile(filepath.Join(dir, manifestName), payload, cacheFileMode))
	lock, err := imagecache.Acquire(context.Background(), root, imageKey(image), true, false)
	require.NoError(t, err)
	t.Cleanup(func() { _ = lock.Close() })
	type result struct {
		digest string
		found  bool
		err    error
	}
	done := make(chan result, 1)
	go func() {
		digest, found, err := CachedManifestDigest(root, image)
		done <- result{digest, found, err}
	}()
	select {
	case result := <-done:
		t.Fatalf("manifest read returned while the cache was busy: %+v", result)
	case <-time.After(60 * time.Millisecond):
	}
	require.NoError(t, lock.Close())
	select {
	case result := <-done:
		require.NoError(t, result.err)
		require.True(t, result.found)
		require.Equal(t, sha256Hex(payload), result.digest)
	case <-time.After(5 * time.Second):
		t.Fatal("manifest read did not complete after the writer released the cache")
	}
}

func TestCacheReadWaitHonorsContext(t *testing.T) {
	root, image := t.TempDir(), "busy-template"
	lock, err := imagecache.Acquire(context.Background(), root, imageKey(image), true, false)
	require.NoError(t, err)
	defer lock.Close()
	for name, read := range map[string]func(context.Context) error{
		"readiness": func(ctx context.Context) error {
			_, err := ImageReadyContext(ctx, root, image)
			return err
		},
		"manifest": func(ctx context.Context) error {
			_, _, err := CachedManifestDigestContext(ctx, root, image)
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
			defer cancel()
			require.ErrorIs(t, read(ctx), context.DeadlineExceeded)
		})
	}
}

// TestCacheComplete checks the committed-pull predicate: any missing,
// resized, or corrupted file (or a missing manifest) invalidates the cache.
func TestCacheComplete(t *testing.T) {
	artifacts := map[string][]byte{
		"rootfs.ext4":  bytes.Repeat([]byte{0xab}, 1<<16),
		"vmstate.snap": []byte("vmstate"),
		"memory.snap":  []byte("memory"),
	}
	manifestPayload := testManifest(artifacts)
	document, err := parseManifest(manifestPayload)
	require.NoError(t, err)
	files, err := document.nativeFiles()
	require.NoError(t, err)

	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(dir, cacheDirMode))

	// No manifest: not complete.
	complete, err := cacheComplete(dir)
	require.NoError(t, err)
	require.False(t, complete)

	// Manifest but no files: not complete.
	require.NoError(t, os.WriteFile(filepath.Join(dir, manifestName), manifestPayload, cacheFileMode))
	complete, err = cacheComplete(dir)
	require.NoError(t, err)
	require.False(t, complete)

	// A file with the wrong size: not complete (fast path).
	for _, file := range files[:1] {
		require.NoError(t, os.WriteFile(filepath.Join(dir, file.cache), []byte("tiny"), cacheFileMode))
	}
	complete, err = cacheComplete(dir)
	require.NoError(t, err)
	require.False(t, complete)

	// All files exact: complete.
	for _, file := range files {
		require.NoError(t, os.WriteFile(filepath.Join(dir, file.cache), artifacts[file.publish], cacheFileMode))
	}
	complete, err = cacheComplete(dir)
	require.NoError(t, err)
	require.True(t, complete)

	// Any single corrupt file breaks the commit.
	require.NoError(t, os.WriteFile(filepath.Join(dir, files[1].cache), []byte("tampered"), cacheFileMode))
	complete, err = cacheComplete(dir)
	require.NoError(t, err)
	require.False(t, complete)
}
