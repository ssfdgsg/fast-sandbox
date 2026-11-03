package gpu

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	fastletapi "fast-sandbox/internal/protocol/fastlet"
)

const deviceA = "GPU-00000000-0000-0000-0000-000000000001"
const deviceB = "GPU-00000000-0000-0000-0000-000000000002"

func owner(uid string) fastletapi.SandboxIdentity {
	return fastletapi.SandboxIdentity{SandboxUID: uid, FastletPodUID: "pod-a", RuntimeInstanceID: "runtime-" + uid, InstanceGeneration: 1, AssignmentAttempt: 1}
}

func TestConcurrentAcquireAndRecovery(t *testing.T) {
	root := t.TempDir()
	m, err := New(root, []string{deviceA, deviceB}, 1, "pod-a", "k8s.io", DefaultContainerdSocket)
	require.NoError(t, err)
	var wg sync.WaitGroup
	successes := make(chan string, 20)
	failures := make(chan error, 20)
	for i := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id := fmt.Sprint(i)
			_, err := m.Acquire(owner(id))
			if err == nil {
				successes <- id
			} else {
				failures <- err
			}
		}()
	}
	wg.Wait()
	close(successes)
	close(failures)
	require.Len(t, successes, 2)
	for err := range failures {
		require.ErrorIs(t, err, ErrExhausted)
	}
	ids := []string{}
	for id := range successes {
		ids = append(ids, id)
		lease, err := m.Acquire(owner(id))
		require.NoError(t, err)
		require.Len(t, lease.Devices, 1)
	}
	require.False(t, m.Available())
	require.NoError(t, m.Close())
	recovered, err := New(root, []string{deviceA, deviceB}, 1, "pod-a", "k8s.io", DefaultContainerdSocket)
	require.NoError(t, err)
	defer recovered.Close()
	require.False(t, recovered.Available())
	require.Len(t, recovered.Leases(), 2)
	for _, id := range ids {
		_, err = recovered.Acquire(owner(id))
		require.NoError(t, err)
	}
	require.NoError(t, recovered.Release(ids[0]))
	require.True(t, recovered.Available())
	_, err = recovered.Acquire(owner("replacement"))
	require.NoError(t, err)
}

func TestLeaseFencesAndAllocationValidation(t *testing.T) {
	m, err := New(t.TempDir(), []string{deviceA, deviceB}, 1, "pod-a", "k8s.io", DefaultContainerdSocket)
	require.NoError(t, err)
	defer m.Close()
	first, err := m.Acquire(owner("a"))
	require.NoError(t, err)
	next := owner("a")
	next.RuntimeInstanceID = "replacement"
	_, err = m.Acquire(next)
	require.ErrorContains(t, err, "different runtime")
	require.Error(t, m.Recover(Lease{Owner: owner("b"), Devices: first.Devices, Namespace: "k8s.io", Endpoint: DefaultContainerdSocket}))
	require.Error(t, m.Recover(Lease{Owner: owner("b"), Devices: []string{"nvidia.com/gpu=all"}, Namespace: "k8s.io", Endpoint: DefaultContainerdSocket}))
	require.Error(t, m.Recover(Lease{Owner: next, Devices: first.Devices, Namespace: "k8s.io", Endpoint: DefaultContainerdSocket}))
	first.Devices[0] = "mutated" // The returned allocation must not mutate the ledger.
	require.NoError(t, m.Recover(m.Leases()[0]))
	_, err = m.Acquire(fastletapi.SandboxIdentity{SandboxUID: "incomplete"})
	require.Error(t, err)
}

func TestPendingReservationsSurviveRestartAndWriterIsExclusive(t *testing.T) {
	root := t.TempDir()
	m, err := New(root, []string{deviceA}, 1, "pod-a", "k8s.io", DefaultContainerdSocket)
	require.NoError(t, err)
	_, err = New(root, []string{deviceA}, 1, "pod-a", "k8s.io", DefaultContainerdSocket)
	require.Error(t, err)
	_, err = m.Acquire(owner("pending"))
	require.NoError(t, err)
	require.NoError(t, m.Close())
	m, err = New(root, []string{deviceA}, 1, "pod-a", "k8s.io", DefaultContainerdSocket)
	require.NoError(t, err)
	defer m.Close()
	_, err = m.Acquire(owner("other"))
	require.ErrorIs(t, err, ErrExhausted)
	require.NoError(t, m.Release("pending"))
	_, err = m.Acquire(owner("other"))
	require.NoError(t, err)
}

func TestInvalidStateAndDeviceInventoryFailClosed(t *testing.T) {
	for _, ids := range [][]string{{"0"}, {"nvidia.com/gpu=all"}, {"MIG-abc"}, {deviceA, deviceA}} {
		_, err := New(t.TempDir(), ids, 1, "pod-a", "k8s.io", DefaultContainerdSocket)
		require.Error(t, err)
	}
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "leases.json"), []byte("broken"), 0600))
	_, err := New(root, []string{deviceA}, 1, "pod-a", "k8s.io", DefaultContainerdSocket)
	require.Error(t, err)
	root = t.TempDir()
	m, err := New(root, []string{deviceA}, 1, "pod-a", "k8s.io", DefaultContainerdSocket)
	require.NoError(t, err)
	_, err = m.Acquire(owner("live"))
	require.NoError(t, err)
	require.NoError(t, m.Close())
	_, err = New(root, []string{deviceB}, 1, "pod-a", "k8s.io", DefaultContainerdSocket)
	require.ErrorContains(t, err, "unassigned device")
}

func TestFailedPersistenceDoesNotGrantADevice(t *testing.T) {
	root := t.TempDir()
	m, err := New(root, []string{deviceA}, 1, "pod-a", "k8s.io", DefaultContainerdSocket)
	require.NoError(t, err)
	defer m.Close()
	// A destination directory makes atomic rename fail without relying on uid permissions.
	require.NoError(t, os.Mkdir(filepath.Join(root, "leases.json"), 0700))
	_, err = m.Acquire(owner("a"))
	require.Error(t, err)
	require.Empty(t, m.Leases())
	require.NoError(t, os.Remove(filepath.Join(root, "leases.json")))
	_, err = m.Acquire(owner("b"))
	require.NoError(t, err)
}

func TestNodeJournalPreservesOtherFastletsAndPreviousPodOwners(t *testing.T) {
	root := t.TempDir()
	a, err := New(root, []string{deviceA}, 1, "pod-a", "ns-a", DefaultContainerdSocket)
	require.NoError(t, err)
	b, err := New(root, []string{deviceB}, 1, "pod-b", "ns-b", DefaultContainerdSocket)
	require.NoError(t, err)
	defer b.Close()
	_, err = a.Acquire(owner("sandbox-a"))
	require.NoError(t, err)
	ownerB := owner("sandbox-b")
	ownerB.FastletPodUID = "pod-b"
	_, err = b.Acquire(ownerB)
	require.NoError(t, err)
	require.Len(t, a.Leases(), 1)
	require.Len(t, b.Leases(), 1)
	// Even a different Pod identity cannot reuse a GPU held by a live process.
	_, err = New(root, []string{deviceA}, 1, "pod-c", "ns-c", DefaultContainerdSocket)
	require.Error(t, err)
	require.NoError(t, a.Close())
	c, err := New(root, []string{deviceA}, 1, "pod-c", "ns-c", DefaultContainerdSocket)
	require.NoError(t, err)
	defer c.Close()
	require.False(t, c.Available(), "the old process is gone but its task has not been proven absent")
	records, err := c.Records()
	require.NoError(t, err)
	require.Len(t, records, 1)
	require.Error(t, c.Release("sandbox-a"), "ordinary release must not cross Pod ownership")
	altered := records[0]
	altered.Owner.RuntimeInstanceID = "wrong"
	require.Error(t, c.ReleaseVerified(altered))
	require.False(t, c.Available())
	require.NoError(t, c.ReleaseVerified(records[0]))
	require.True(t, c.Available())
	require.Len(t, b.Leases(), 1, "reconciling an old GPU owner must retain unrelated Fastlet leases")
}
