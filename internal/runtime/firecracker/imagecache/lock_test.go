package imagecache

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestLockReadersWritersAndCancellation(t *testing.T) {
	root, key := t.TempDir(), strings.Repeat("a", 64)
	first, err := Acquire(context.Background(), root, key, false, false)
	require.NoError(t, err)
	t.Cleanup(func() { _ = first.Close() })
	second, err := Acquire(context.Background(), root, key, false, false)
	require.NoError(t, err)
	require.NoError(t, second.Close())
	_, err = Acquire(context.Background(), root, key, true, false)
	require.ErrorIs(t, err, ErrBusy)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_, err = Acquire(ctx, root, key, true, true)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	_, err = Acquire(context.Background(), root, "../escape", true, false)
	require.Error(t, err)
}

func TestLockAcrossProcesses(t *testing.T) {
	const childRootEnv = "FSB_CACHE_LOCK_TEST_ROOT"
	key := strings.Repeat("b", 64)
	if root := os.Getenv(childRootEnv); root != "" {
		_, err := Acquire(context.Background(), root, key, true, false)
		require.ErrorIs(t, err, ErrBusy, "another process holds the shared lock")
		return
	}
	root := t.TempDir()
	lock, err := Acquire(context.Background(), root, key, false, false)
	require.NoError(t, err)
	defer lock.Close()
	child := exec.Command(os.Args[0], "-test.run=^TestLockAcrossProcesses$")
	child.Env = append(os.Environ(), childRootEnv+"="+root)
	output, err := child.CombinedOutput()
	require.NoError(t, err, "%s", output)
}
