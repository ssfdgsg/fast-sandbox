//go:build !linux

package firecracker

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPublishedCacheSkipsInstallationOnUnsupportedPlatform(t *testing.T) {
	root := t.TempDir()
	driver := &Driver{config: firecrackerConfigForTest(t, root)}
	staging, digest := stageNativeSnapshot(t, root, "rootfs")
	outcome, err := driver.commitPublishedCache(context.Background(), "template", digest, staging)
	require.NoError(t, err)
	require.Equal(t, cacheSkippedFS, outcome)
	require.NoDirExists(t, staging)
	require.NoDirExists(t, filepath.Join(root, imageCacheDir, imageKey("template")))
	entries, err := filepath.Glob(filepath.Join(root, imageCacheDir, ".snapshot-*"))
	require.NoError(t, err)
	require.Empty(t, entries)
}
