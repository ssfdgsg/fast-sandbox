//go:build linux

package firecracker

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestInstallSnapshotCacheDoesNotReplaceExistingDirectory(t *testing.T) {
	root := t.TempDir()
	source, target := filepath.Join(root, "source"), filepath.Join(root, "target")
	require.NoError(t, os.Mkdir(source, 0o750))
	require.NoError(t, os.Mkdir(target, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(source, "artifact"), []byte("snapshot"), 0o640))
	require.ErrorIs(t, installSnapshotCache(source, target), os.ErrExist)
	require.FileExists(t, filepath.Join(source, "artifact"))
	entries, err := os.ReadDir(target)
	require.NoError(t, err)
	require.Empty(t, entries, "even an empty existing entry must not be replaced")
}
