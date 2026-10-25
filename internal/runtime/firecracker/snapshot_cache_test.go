package firecracker

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"fast-sandbox/internal/artifacts"
	fastletapi "fast-sandbox/internal/protocol/fastlet"
	runtimecontract "fast-sandbox/internal/runtime/contract"
	"fast-sandbox/internal/runtime/firecracker/agent"
	"fast-sandbox/internal/runtime/firecracker/imagecache"
)

func requireNativeCacheInstallation(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("native cache installation requires Linux RENAME_NOREPLACE")
	}
}

func stageNativeSnapshot(t *testing.T, root, rootfs string) (string, string) {
	t.Helper()
	staging, err := os.MkdirTemp(root, "staged-*")
	require.NoError(t, err)
	files := map[string]imagecache.File{}
	for name, content := range map[string]string{
		publishedRootfsName: rootfs, vmstateSnapshotName: "state", memorySnapshotName: "memory",
	} {
		require.NoError(t, os.WriteFile(filepath.Join(staging, name), []byte(content), 0o640))
		files[name] = imagecache.File{SHA256: artifacts.SHA256Of([]byte(content)), SizeBytes: int64(len(content))}
	}
	document := testManifestDocument(t)
	document["files"] = files
	payload, err := json.Marshal(document)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(staging, snapshotManifestName), payload, 0o640))
	return staging, artifacts.SHA256Of(payload)
}

func TestPublishedCacheCommitAndConflict(t *testing.T) {
	requireNativeCacheInstallation(t)
	root := t.TempDir()
	driver := &Driver{config: firecrackerConfigForTest(t, root)}
	staging, digest := stageNativeSnapshot(t, root, "original-rootfs")
	before, err := os.Stat(filepath.Join(staging, publishedRootfsName))
	require.NoError(t, err)
	outcome, err := driver.commitPublishedCache(context.Background(), "template-v1", digest, staging)
	require.NoError(t, err)
	require.Equal(t, cacheCommitted, outcome)
	require.NoDirExists(t, staging)
	require.NoError(t, verifyRestorableImage(root, "template-v1"))
	ready, err := agent.ImageReady(root, "template-v1")
	require.NoError(t, err)
	require.True(t, ready, "the agent must accept the prewarmed layout and checksums")
	path, err := imageCachePath(root, "template-v1")
	require.NoError(t, err)
	after, err := os.Stat(path)
	require.NoError(t, err)
	require.True(t, os.SameFile(before, after), "installation must move, not copy, artifact bytes")

	staging, digest = stageNativeSnapshot(t, root, "original-rootfs")
	outcome, err = driver.commitPublishedCache(context.Background(), "template-v1", digest, staging)
	require.NoError(t, err)
	require.Equal(t, cacheAlreadyPresent, outcome)
	require.NoDirExists(t, staging)
	staging, digest = stageNativeSnapshot(t, root, "replacement-rootfs")
	outcome, err = driver.commitPublishedCache(context.Background(), "template-v1", digest, staging)
	require.NoError(t, err)
	require.Equal(t, cacheSkippedConflict, outcome)
	require.NoDirExists(t, staging)
	content, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, "original-rootfs", string(content))
}

func TestPublishedCacheSkipsAndCleansStaging(t *testing.T) {
	for _, scenario := range []string{"busy", "capacity", "incomplete-target", "truncated-source", "digest-mismatch", "cancelled"} {
		t.Run(scenario, func(t *testing.T) {
			root := t.TempDir()
			driver := &Driver{config: firecrackerConfigForTest(t, root)}
			staging, digest := stageNativeSnapshot(t, root, "rootfs")
			ctx := context.Background()
			want := cacheCommitFailed
			switch scenario {
			case "busy":
				lock, err := imagecache.Acquire(ctx, root, imageKey("template"), true, false)
				require.NoError(t, err)
				defer lock.Close()
				want = cacheSkippedBusy
			case "capacity":
				driver.imageCacheLimitBytes = 1
				want = cacheSkippedCapacity
			case "incomplete-target":
				require.NoError(t, os.MkdirAll(filepath.Join(root, imageCacheDir, imageKey("template")), 0o750))
				want = cacheSkippedConflict
			case "truncated-source":
				require.NoError(t, os.WriteFile(filepath.Join(staging, memorySnapshotName), nil, 0o640))
			case "digest-mismatch":
				digest = strings.Repeat("a", 64)
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			outcome, err := driver.commitPublishedCache(ctx, "template", digest, staging)
			require.Equal(t, want, outcome)
			if want == cacheCommitFailed {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			require.NoDirExists(t, staging)
			require.ErrorIs(t, verifyRestorableImage(root, "template"), ErrImageNotReady)
			entries, err := os.ReadDir(filepath.Join(root, imageCacheDir))
			if err == nil {
				for _, entry := range entries {
					require.False(t, strings.HasPrefix(entry.Name(), ".snapshot-"), "temporary cache directory leaked")
				}
			}
		})
	}
}

func TestCreateTemplateSnapshotPrewarm(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "disabled", true: "enabled"}[enabled], func(t *testing.T) {
			if enabled {
				requireNativeCacheInstallation(t)
			}
			fixture, _ := newSnapshotFixture(t)
			if enabled {
				t.Setenv(snapshotPrewarmEnv, "true")
			}
			sandboxID := seedRunningSandbox(t, fixture, PhaseRunning)
			result, err := fixture.driver.CreateSnapshot(context.Background(), &runtimecontract.SnapshotInput{
				SandboxID: sandboxID, SnapshotID: "snap-prewarm", TemplateName: "template-prewarm",
			})
			require.NoError(t, err)
			require.True(t, fixture.server.running)
			ready, err := agent.ImageReady(fixture.stateRoot, "template-prewarm")
			require.NoError(t, err)
			require.Equal(t, enabled, ready)
			if enabled {
				status, err := fixture.driver.DeliverImage(context.Background(), "template-prewarm")
				require.NoError(t, err)
				require.Equal(t, runtimecontract.ImageDelivered, status)
				digest, ok, err := agent.CachedManifestDigest(fixture.stateRoot, "template-prewarm")
				require.NoError(t, err)
				require.True(t, ok)
				require.Equal(t, result.ArtifactDigest, digest)
				// Exercise the driver's actual restore flow against the fake
				// Firecracker API, using the just-published template reference.
				clone := fixture.sandboxSpec
				clone.Spec.Image = "template-prewarm"
				clone.Identity.SandboxUID = "sandbox-clone"
				clone.Identity.Name = "sandbox-clone"
				clone.Identity.RuntimeInstanceID = "runtime-clone"
				require.NoError(t, fixture.driver.Initialize(context.Background(), ""))
				t.Cleanup(func() { _ = fixture.driver.Close() })
				metadata, err := fixture.driver.EnsureSandbox(context.Background(), ensureInput(&clone))
				require.NoError(t, err)
				require.NotNil(t, metadata)
			}
			require.ErrorIs(t, verifyRestorableImage(fixture.stateRoot, artifacts.CheckpointReference(result.ArtifactDigest)), ErrImageNotReady)
			require.NoDirExists(t, filepath.Join(fixture.stateRoot, snapshotStagingDir, "snap-prewarm"))
		})
	}
}

func TestEnsureSandboxParksWhileCacheWriterIsActive(t *testing.T) {
	fixture := newDriverFixture(t)
	fixture.prepareCachedImage(t, fixture.sandboxSpec.Spec.Image)
	lock, err := imagecache.Acquire(context.Background(), fixture.stateRoot, imageKey(fixture.sandboxSpec.Spec.Image), true, false)
	require.NoError(t, err)
	defer lock.Close()
	_, err = fixture.driver.EnsureSandbox(context.Background(), ensureInput(&fixture.sandboxSpec))
	require.ErrorIs(t, err, ErrImageNotReady)
	require.Empty(t, fixture.server.recordedCalls(), "a busy cache must park before any VM API call")
}

func TestCreateTemplateSnapshotPrewarmFailureKeepsPublishedResult(t *testing.T) {
	fixture, _ := newSnapshotFixture(t)
	t.Setenv(snapshotPrewarmEnv, "true")
	fixture.driver.imageCacheLimitBytes = 1
	sandboxID := seedRunningSandbox(t, fixture, PhaseRunning)
	result, err := fixture.driver.CreateSnapshot(context.Background(), &runtimecontract.SnapshotInput{
		SandboxID: sandboxID, SnapshotID: "snap-no-room", TemplateName: "template-no-room", Kind: fastletapi.SnapshotKindTemplate,
	})
	require.NoError(t, err)
	require.NotEmpty(t, result.ManifestRef)
	require.True(t, fixture.server.running)
	require.ErrorIs(t, verifyRestorableImage(fixture.stateRoot, "template-no-room"), ErrImageNotReady)
	require.NoDirExists(t, filepath.Join(fixture.stateRoot, snapshotStagingDir, "snap-no-room"))
}

func TestSnapshotPrewarmPreservesPlatformState(t *testing.T) {
	requireNativeCacheInstallation(t)
	for _, kind := range []fastletapi.SnapshotKind{fastletapi.SnapshotKindTemplate, fastletapi.SnapshotKindCheckpoint} {
		t.Run(string(kind), func(t *testing.T) {
			fixture, _ := newSnapshotFixture(t)
			t.Setenv(snapshotPrewarmEnv, "true")
			sandboxID := seedRunningSandbox(t, fixture, PhaseRunning)
			vault, policy := []byte(`{"snapshot":"vault"}`), []byte(`{"defaultAction":"deny"}`)
			fixture.driver.SetStateMover(&fakeTemplateCapableMover{fakeStateMover: &fakeStateMover{
				exportData: map[runtimecontract.StateKind][]byte{
					runtimecontract.StateKindVault: vault, runtimecontract.StateKindPolicy: policy,
				},
			}})
			input := &runtimecontract.SnapshotInput{SandboxID: sandboxID, SnapshotID: "snapshot-state", Kind: kind}
			if kind == fastletapi.SnapshotKindTemplate {
				input.TemplateName = "template-state"
			}
			result, err := fixture.driver.CreateSnapshot(context.Background(), input)
			require.NoError(t, err)
			reference := input.TemplateName
			if kind == fastletapi.SnapshotKindCheckpoint {
				reference = artifacts.CheckpointReference(result.ArtifactDigest)
			}
			cache := filepath.Join(fixture.stateRoot, imageCacheDir, imageKey(reference))
			for name, want := range map[string][]byte{vaultBundleName: vault, policyBundleName: policy} {
				got, err := os.ReadFile(filepath.Join(cache, name))
				require.NoError(t, err)
				require.Equal(t, want, got)
			}
			ready, err := agent.ImageReady(fixture.stateRoot, reference)
			require.NoError(t, err)
			require.True(t, ready)
			require.NoError(t, verifyRestorableImage(fixture.stateRoot, reference))
			require.NoError(t, os.Remove(filepath.Join(cache, vaultBundleName)))
			require.ErrorIs(t, verifyRestorableImage(fixture.stateRoot, reference), ErrImageNotReady)
			ready, err = agent.ImageReady(fixture.stateRoot, reference)
			require.NoError(t, err)
			require.False(t, ready, "missing platform state must not be a cache hit")
			require.NoError(t, os.WriteFile(filepath.Join(cache, vaultBundleName), vault, 0o600))
			require.NoError(t, os.WriteFile(filepath.Join(cache, policyBundleName), []byte(strings.Repeat("x", len(policy))), 0o600))
			ready, err = agent.ImageReady(fixture.stateRoot, reference)
			require.NoError(t, err)
			require.False(t, ready, "same-size corrupt platform state must fail Agent verification")
		})
	}
}
