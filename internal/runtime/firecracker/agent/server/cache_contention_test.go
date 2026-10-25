package server

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	agentprotocol "fast-sandbox/internal/runtime/firecracker/agent/protocol"
	agentstate "fast-sandbox/internal/runtime/firecracker/agent/state"
	"fast-sandbox/internal/runtime/firecracker/imagecache"
)

func TestPinImageWaitsForWriterAfterSuccessfulPull(t *testing.T) {
	for _, scenario := range []struct {
		name       string
		checkpoint bool
		cancel     bool
	}{
		{name: "image"},
		{name: "checkpoint", checkpoint: true},
		{name: "image-cancel", cancel: true},
		{name: "checkpoint-cancel", checkpoint: true, cancel: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			root := t.TempDir()
			state, err := agentstate.New(root)
			require.NoError(t, err)
			defer state.Close()
			writer := make(chan *imagecache.Lock, 1)
			puller := newFakePuller()
			puller.seed = func(root, image string) error {
				if err := seedCache(root, image); err != nil {
					return err
				}
				// Reproduce another warm pull taking the writer lock between
				// this successful pull and the service's readiness recheck.
				lock, err := imagecache.Acquire(context.Background(), root, imageKeyHex(image), true, false)
				if err != nil {
					return err
				}
				writer <- lock
				return nil
			}
			service := NewService(puller, state, root)
			request := agentprotocol.PinImageRequest{Identity: identity("contended-pin"), Image: testImage}
			if scenario.checkpoint {
				request.ManifestRef, request.ArtifactDigest = "s3://bucket/checkpoint/manifest.json", "checkpoint-digest"
			}
			type result struct {
				response agentprotocol.PinImageResponse
				err      error
			}
			done := make(chan result, 1)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			go func() {
				response, err := service.PinImage(ctx, request)
				done <- result{response, err}
			}()
			var lock *imagecache.Lock
			select {
			case lock = <-writer:
			case <-ctx.Done():
				t.Fatal("pull did not acquire the writer lock")
			}
			t.Cleanup(func() { _ = lock.Close() })
			if scenario.cancel {
				cancel()
				select {
				case result := <-done:
					require.ErrorIs(t, result.err, context.Canceled)
				case <-time.After(time.Second):
					t.Fatal("pin did not cancel while waiting for the cache writer")
				}
				return
			}
			select {
			case result := <-done:
				t.Fatalf("pin returned while the intact cache was busy: %v", result.err)
			case <-time.After(60 * time.Millisecond):
			}
			require.NoError(t, lock.Close())
			select {
			case result := <-done:
				require.NoError(t, result.err)
				require.True(t, result.response.Ready)
				require.NotEmpty(t, result.response.ManifestDigest)
			case <-ctx.Done():
				t.Fatal("pin did not complete after the writer released the cache")
			}
			require.Equal(t, 1, puller.pullCount(testImage))
		})
	}
}
