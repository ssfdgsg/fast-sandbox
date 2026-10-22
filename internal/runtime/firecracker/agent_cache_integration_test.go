package firecracker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"fast-sandbox/internal/registryconfig"
	"fast-sandbox/internal/runtime/firecracker/agent"
)

// TestAgentPullFeedsDriverCache wires the runtime-agent pull layer to the
// driver's existing cache consumer: after PullImage, the restore path
// (resolveRootfsImage) resolves the pulled artifact without any driver
// change, and the per-build digest namespace and file mapping
// (rootfs.ext4 -> rootfs.img) match the published layout.
func TestAgentPullFeedsDriverCache(t *testing.T) {
	for _, mode := range []string{"cold", "prewarm-then-evict", "concurrent-pull"} {
		t.Run(mode, func(t *testing.T) {
			testAgentPullFeedsDriverCache(t, mode)
		})
	}
}

func testAgentPullFeedsDriverCache(t *testing.T, mode string) {
	t.Helper()
	if mode == "prewarm-then-evict" {
		requireNativeCacheInstallation(t)
	}
	const (
		image       = "registry.example.com/sandbox:v1.0.21"
		bucket      = "sandbox-images"
		storePrefix = "publish"
	)
	artifacts := map[string][]byte{
		"rootfs.ext4":  []byte("rootfs-content"),
		"vmstate.snap": []byte("vmstate-content"),
		"memory.snap":  []byte("memory-content"),
	}
	files := map[string]any{}
	for name, content := range artifacts {
		files[name] = map[string]any{
			"sha256":    sha256HexTest(content),
			"sizeBytes": len(content),
		}
	}
	manifestBytes, err := json.Marshal(map[string]any{"schemaVersion": 1, "runtime": "firecracker", "files": files})
	require.NoError(t, err)
	digest16 := sha256HexTest(manifestBytes)[:16]

	objects := map[string][]byte{
		digest16 + "/manifest.json": manifestBytes,
	}
	for name, content := range artifacts {
		objects[digest16+"/"+name] = content
	}
	indexBytes, err := json.Marshal(map[string]string{
		"image":          image,
		"manifestRef":    "s3://" + bucket + "/" + storePrefix + "/" + digest16 + "/manifest.json",
		"artifactDigest": sha256HexTest(manifestBytes),
		"updatedAt":      "2026-08-27T00:00:00Z",
	})
	require.NoError(t, err)
	objects["index/"+imageKey(image)+".json"] = indexBytes

	var requests atomic.Int64
	memoryStarted, releaseMemory := make(chan struct{}), make(chan struct{})
	var blockOnce, releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(releaseMemory) }) }
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if mode == "concurrent-pull" && strings.HasSuffix(r.URL.Path, "/memory.snap") {
			blockOnce.Do(func() {
				close(memoryStarted)
				<-releaseMemory
			})
		}
		content, ok := objects[strings.TrimPrefix(r.URL.Path, "/"+bucket+"/"+storePrefix+"/")]
		if !ok {
			http.NotFound(w, nil)
			return
		}
		_, _ = w.Write(content)
	}))
	defer server.Close()
	defer unblock()

	client, err := agent.NewClient("s3://"+bucket+"/"+storePrefix,
		registryconfig.Credential{Host: server.URL, Username: "ak", Password: "sk"})
	require.NoError(t, err)

	stateRoot := t.TempDir()
	if mode == "concurrent-pull" {
		pulled := make(chan error, 1)
		go func() { pulled <- client.PullImage(context.Background(), stateRoot, image) }()
		select {
		case <-memoryStarted:
		case <-time.After(5 * time.Second):
			t.Fatal("pull did not reach memory download")
		}
		driver := &Driver{config: firecrackerConfigForTest(t, stateRoot)}
		staging, digest := stageNativeSnapshot(t, stateRoot, "new-snapshot")
		outcome, err := driver.commitPublishedCache(context.Background(), image, digest, staging)
		require.NoError(t, err)
		require.Equal(t, cacheSkippedBusy, outcome)
		require.NoDirExists(t, staging)
		removed, err := garbageCollectImages(stateRoot, 0, nil)
		require.NoError(t, err)
		require.Empty(t, removed, "GC must not remove the partial set owned by the pull")
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
		defer cancel()
		require.ErrorIs(t, client.PullImage(ctx, stateRoot, image), context.DeadlineExceeded)
		unblock()
		require.NoError(t, <-pulled)
	}
	if mode == "prewarm-then-evict" {
		staging, err := os.MkdirTemp(stateRoot, "snapshot-*")
		require.NoError(t, err)
		for name, content := range artifacts {
			require.NoError(t, os.WriteFile(filepath.Join(staging, name), content, 0o640))
		}
		require.NoError(t, os.WriteFile(filepath.Join(staging, "manifest.json"), manifestBytes, 0o640))
		driver := &Driver{config: firecrackerConfigForTest(t, stateRoot)}
		outcome, err := driver.commitPublishedCache(context.Background(), image, sha256HexTest(manifestBytes), staging)
		require.NoError(t, err)
		require.Equal(t, cacheCommitted, outcome)
		require.NoError(t, client.PullImage(context.Background(), stateRoot, image))
		require.Zero(t, requests.Load(), "a prewarmed image needs no metadata or artifact GET")
		first, second := t.TempDir(), t.TempDir()
		firstRoot, err := prepareInstanceRootfs(stateRoot, image, first)
		require.NoError(t, err)
		secondRoot, err := prepareInstanceRootfs(stateRoot, image, second)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(firstRoot, []byte("instance mutation"), 0o640))
		content, err := os.ReadFile(secondRoot)
		require.NoError(t, err)
		require.Equal(t, artifacts["rootfs.ext4"], content, "instances must have independent writable rootfs")
		removed, err := garbageCollectImages(stateRoot, 0, nil)
		require.NoError(t, err)
		require.Equal(t, []string{imageKey(image)}, removed)
	}
	require.NoError(t, client.PullImage(context.Background(), stateRoot, image))

	require.Positive(t, requests.Load(), "cold cache must fetch the published set")

	// The driver's existing restore consumer resolves the pulled rootfs
	// image under the shared cache layout.
	resolved, err := resolveRootfsImage(stateRoot, image)
	require.NoError(t, err)
	expected, err := imageCachePath(stateRoot, image)
	require.NoError(t, err)
	require.Equal(t, expected, resolved)
	content, err := os.ReadFile(resolved)
	require.NoError(t, err)
	require.Equal(t, artifacts["rootfs.ext4"], content)

	// A second pull is a no-op for the store, and the driver's idempotent
	// PullImage also sees the image as ready.
	require.NoError(t, client.PullImage(context.Background(), stateRoot, image))
	driver := &Driver{config: firecrackerConfigForTest(t, stateRoot)}
	require.NoError(t, driver.PullImage(context.Background(), image))
}

func sha256HexTest(payload []byte) string {
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:])
}
