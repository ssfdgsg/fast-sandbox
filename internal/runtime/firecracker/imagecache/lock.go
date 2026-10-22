// Package imagecache shares the native cache protocol between the driver
// and the node runtime-agent, which may run in different processes.
package imagecache

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"
)

// ErrBusy means a nonblocking lock attempt met an active reader or writer.
var ErrBusy = errors.New("image cache entry is busy")

// Lock holds an advisory flock. All cache mutators must participate. Lock
// files live outside evictable entries and MUST NOT be unlinked: replacing
// a lock inode would let two processes acquire different locks for one key.
type Lock struct {
	file *os.File
}

// Acquire locks a reference hash (64 lowercase hex characters). Readers use
// exclusive=false; pull, commit and eviction use exclusive=true. wait=false
// is for best-effort work and hot-path readiness checks. Waiting is context
// cancellable, including when another process is downloading an image.
func Acquire(ctx context.Context, stateRoot, key string, exclusive, wait bool) (*Lock, error) {
	if !ValidKey(key) {
		return nil, fmt.Errorf("invalid image cache key %q", key)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	dir := filepath.Join(stateRoot, "image-locks")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(filepath.Join(dir, key+".lock"), os.O_CREATE|os.O_RDWR, 0o640)
	if err != nil {
		return nil, err
	}
	operation := unix.LOCK_SH | unix.LOCK_NB
	if exclusive {
		operation = unix.LOCK_EX | unix.LOCK_NB
	}
	for {
		err = unix.Flock(int(file.Fd()), operation)
		if err == nil {
			return &Lock{file: file}, nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) && !errors.Is(err, unix.EINTR) {
			_ = file.Close()
			return nil, err
		}
		if !wait {
			_ = file.Close()
			return nil, ErrBusy
		}
		timer := time.NewTimer(20 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			_ = file.Close()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

// Close releases the lock by closing its file description.
func (lock *Lock) Close() error { return lock.file.Close() }
