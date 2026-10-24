//go:build firecracker && linux

package firecracker

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"fast-sandbox/internal/artifacts"
	"fast-sandbox/internal/registryconfig"
	runtimecontract "fast-sandbox/internal/runtime/contract"
	agentpull "fast-sandbox/internal/runtime/firecracker/agent"
	agentserver "fast-sandbox/internal/runtime/firecracker/agent/server"
	agentstate "fast-sandbox/internal/runtime/firecracker/agent/state"
)

// snapshotDiskStore exercises real HTTP streaming without retaining VM
// memory images in the test process. This is a path-style object-store
// fixture, not an S3 authentication or cloud-network benchmark.
type snapshotDiskStore struct {
	dir           string
	gets          atomic.Int64
	artifactBytes atomic.Int64
}

func (store *snapshotDiskStore) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	prefix := "/" + e2eBucket + "/" + e2ePrefix + "/"
	key := strings.TrimPrefix(r.URL.Path, prefix)
	if key == r.URL.Path || key == "" || strings.Contains(key, "..") {
		http.NotFound(w, r)
		return
	}
	target := filepath.Join(store.dir, filepath.FromSlash(key))
	switch r.Method {
	case http.MethodPut:
		if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		file, err := os.CreateTemp(filepath.Dir(target), ".upload-*")
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer os.Remove(file.Name())
		_, copyErr := io.Copy(file, r.Body)
		closeErr := file.Close()
		if copyErr != nil || closeErr != nil {
			http.Error(w, "upload failed", http.StatusInternalServerError)
			return
		}
		if err := os.Rename(file.Name(), target); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	case http.MethodGet:
		store.gets.Add(1)
		file, err := os.Open(target)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		defer file.Close()
		info, err := file.Stat()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Length", strconv.FormatInt(info.Size(), 10))
		n, _ := io.Copy(w, file)
		if strings.HasSuffix(key, ".snap") || strings.HasSuffix(key, ".ext4") {
			store.artifactBytes.Add(n)
		}
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func startSnapshotDiskAgent(t *testing.T, stateRoot, endpoint string) string {
	t.Helper()
	client, err := agentpull.NewClient("s3://"+e2eBucket+"/"+e2ePrefix, registryconfig.Credential{
		Host: endpoint, Username: "test-reader", Password: "test-reader-secret",
		WriteUsername: "test-writer", WritePassword: "test-writer-secret",
	})
	require.NoError(t, err)
	state, err := agentstate.New(stateRoot)
	require.NoError(t, err)
	t.Cleanup(func() { _ = state.Close() })
	socket := testAgentSocketPath(t)
	server := agentserver.New(agentserver.NewService(client, state, stateRoot), socket)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(6 * time.Second):
			t.Error("snapshot runtime-agent did not shut down")
		}
	})
	require.NoError(t, waitForAPISocket(ctx, socket, 5*time.Second))
	return socket
}

// Check the streaming store and real UDS agent before using them with
// expensive VM artifacts. This also runs on a Linux host without KVM.
func TestSnapshotDiskStoreRoundTrip(t *testing.T) {
	root := t.TempDir()
	staging, digest := stageNativeSnapshot(t, root, "rootfs")
	require.NoError(t, artifacts.WriteSHA256SUMS(staging,
		[]string{publishedRootfsName, vmstateSnapshotName, memorySnapshotName}, nil))
	store := &snapshotDiskStore{dir: t.TempDir()}
	origin := httptest.NewServer(store)
	defer origin.Close()
	socket := startSnapshotDiskAgent(t, root, origin.URL)
	client, err := NewAgentClient(socket, "e2e", "store-roundtrip")
	require.NoError(t, err)
	result, err := client.PublishImage(context.Background(), "publish-roundtrip", "template", "roundtrip", staging)
	require.NoError(t, err)
	require.Equal(t, digest, result.ArtifactDigest)
	require.Zero(t, store.gets.Load())
	pinned, err := client.PinImage(context.Background(), "pin-roundtrip", "roundtrip")
	require.NoError(t, err)
	require.Equal(t, digest, pinned)
	require.Positive(t, store.artifactBytes.Load())
	require.NoError(t, verifyRestorableImage(root, "roundtrip"))
}

// TestFirecrackerSnapshotPrewarmE2E requires the standard Firecracker E2E
// assets and an XFS StateRoot. Run in dedicated mount/network/PID namespaces:
// the existing E2E network helpers purge their fsb namespace family.
func TestFirecrackerSnapshotPrewarmE2E(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(fmt.Sprintf("prewarm=%t", enabled), func(t *testing.T) {
			t.Setenv(snapshotPrewarmEnv, strconv.FormatBool(enabled))
			env := newE2EEnvironment(t, 2, false)
			defer env.teardown()
			ctx := context.Background()
			source := env.spec(1)
			traceCtx, end := env.traceContext(source.Identity.SandboxUID)
			defer end()
			env.boot(traceCtx, source)
			env.assertGuestReachable(source.Identity.SandboxUID)

			store := &snapshotDiskStore{dir: t.TempDir()}
			origin := httptest.NewServer(store)
			t.Cleanup(origin.Close)
			socket := startSnapshotDiskAgent(t, env.stateRoot, origin.URL)
			env.driver.mu.Lock()
			env.driver.namespace = "e2e"
			env.driver.mu.Unlock()
			env.driver.SetFastletPodUID(env.podUID)
			env.driver.SetAgentSocket(socket)
			agent, err := NewAgentClient(socket, "e2e", env.podUID)
			require.NoError(t, err)
			template := fmt.Sprintf("prewarm-e2e-%t-%d", enabled, time.Now().UnixNano())
			started := time.Now()
			result, err := env.driver.CreateSnapshot(ctx, &runtimecontract.SnapshotInput{
				SandboxID: source.Identity.SandboxUID, SnapshotID: "snapshot-" + template, TemplateName: template,
			})
			require.NoError(t, err)
			publishTime := time.Since(started)
			inspected, err := env.driver.InspectSandbox(ctx, source.Identity.SandboxUID)
			require.NoError(t, err)
			require.Equal(t, string(PhaseRunning), inspected.Phase, "source must resume after snapshot")
			env.assertGuestReachable(source.Identity.SandboxUID)
			require.Zero(t, store.gets.Load(), "publication must not pull its own artifacts")
			var pullTime time.Duration
			if enabled {
				require.NoError(t, verifyRestorableImage(env.stateRoot, template))
			} else {
				require.ErrorIs(t, verifyRestorableImage(env.stateRoot, template), ErrImageNotReady)
				started = time.Now()
				_, err = agent.PinImage(ctx, "cold-"+template, template)
				require.NoError(t, err)
				pullTime = time.Since(started)
				require.Equal(t, result.SizeBytes, store.artifactBytes.Load())
			}
			status, err := env.driver.DeliverImage(ctx, template)
			require.NoError(t, err)
			require.Equal(t, runtimecontract.ImageDelivered, status)
			clone := env.spec(2)
			clone.Spec.Image = template
			cloneCtx, cloneEnd := env.traceContext(clone.Identity.SandboxUID)
			defer cloneEnd()
			started = time.Now()
			env.boot(cloneCtx, clone)
			restoreTime := time.Since(started)
			env.assertGuestReachable(clone.Identity.SandboxUID)
			if enabled {
				require.Zero(t, store.gets.Load(), "same-node template restore must make no origin GET")
				require.Zero(t, store.artifactBytes.Load())
			}
			t.Logf("PREWARM_RESULT enabled=%t bytes=%d snapshot_publish_ms=%.3f cold_pull_ms=%.3f restore_ms=%.3f origin_gets=%d artifact_get_bytes=%d",
				enabled, result.SizeBytes, float64(publishTime.Microseconds())/1000,
				float64(pullTime.Microseconds())/1000, float64(restoreTime.Microseconds())/1000,
				store.gets.Load(), store.artifactBytes.Load())
			if enabled {
				env.delete(clone.Identity.SandboxUID)
				removed, err := garbageCollectImages(env.stateRoot, 0, nil)
				require.NoError(t, err)
				require.Contains(t, removed, imageKey(template))
				started = time.Now()
				_, err = agent.PinImage(ctx, "fallback-"+template, template)
				require.NoError(t, err)
				require.Equal(t, result.SizeBytes, store.artifactBytes.Load())
				waitForCleanSlot(t, env.manager, 1)
				env.boot(cloneCtx, clone)
				env.assertGuestReachable(clone.Identity.SandboxUID)
				t.Logf("PREWARM_FALLBACK bytes=%d elapsed_ms=%.3f origin_gets=%d", result.SizeBytes,
					float64(time.Since(started).Microseconds())/1000, store.gets.Load())
			}
		})
	}
}
