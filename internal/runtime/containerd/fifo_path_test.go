package containerd

import (
	"io"
	"path/filepath"
	"testing"

	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/stretchr/testify/require"
)

func TestCustomSocketFIFOsAreVisibleUnderMountedRunDirectory(t *testing.T) {
	root := t.TempDir()
	driver := &Driver{socketPath: filepath.Join(root, "custom", "containerd.sock")}
	creator := cio.NewCreator(cio.WithFIFODir(driver.fifoDir()), cio.WithStreams(nil, io.Discard, io.Discard))
	taskIO, err := creator("sandbox")
	require.NoError(t, err)
	defer taskIO.Close()
	defer taskIO.Cancel()
	require.Equal(t, filepath.Join(root, "custom", "fifo"), filepath.Dir(filepath.Dir(taskIO.Config().Stdout)))
	require.Equal(t, filepath.Join(root, "custom", "fifo"), filepath.Dir(filepath.Dir(taskIO.Config().Stderr)))
	require.Equal(t, "/run/containerd/fifo", (&Driver{}).fifoDir())
}
