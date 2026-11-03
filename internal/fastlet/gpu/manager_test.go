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
const deviceC = "GPU-00000000-0000-0000-0000-000000000003"
const deviceD = "GPU-00000000-0000-0000-0000-000000000004"

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

func TestMultiGPUSandboxAllocationAndAvailability(t *testing.T) {
	root := t.TempDir()
	// 4 devices total, 2 GPUs per sandbox -> capacity = 2 sandboxes
	m, err := New(root, []string{deviceA, deviceB, deviceC, deviceD}, 2, "pod-a", "k8s.io", DefaultContainerdSocket)
	require.NoError(t, err)
	defer m.Close()

	require.True(t, m.Available())
	require.Len(t, m.Devices(), 4)

	// First sandbox takes 2 devices
	alloc1, err := m.Acquire(owner("sb-1"))
	require.NoError(t, err)
	require.Len(t, alloc1.Devices, 2)
	require.True(t, m.Available(), "2 devices remaining out of 4; should still be available")

	// Second sandbox takes remaining 2 devices
	alloc2, err := m.Acquire(owner("sb-2"))
	require.NoError(t, err)
	require.Len(t, alloc2.Devices, 2)
	require.False(t, m.Available(), "0 devices remaining; must be exhausted")

	// Devices must be disjoint
	for _, d := range alloc1.Devices {
		require.NotContains(t, alloc2.Devices, d)
	}

	// Third sandbox request must fail
	_, err = m.Acquire(owner("sb-3"))
	require.ErrorIs(t, err, ErrExhausted)

	// Release sb-1, freeing 2 devices
	require.NoError(t, m.Release("sb-1"))
	require.True(t, m.Available())

	// sb-3 can now acquire
	alloc3, err := m.Acquire(owner("sb-3"))
	require.NoError(t, err)
	require.Len(t, alloc3.Devices, 2)
	require.False(t, m.Available())
	require.Len(t, m.Leases(), 2)
}

func TestMultiGPUSlotBoundaryUneven(t *testing.T) {
	root := t.TempDir()
	// 3 devices total, 2 GPUs per sandbox -> can only satisfy 1 sandbox; 1 remainder device cannot form a slot
	m, err := New(root, []string{deviceA, deviceB, deviceC}, 2, "pod-a", "k8s.io", DefaultContainerdSocket)
	require.NoError(t, err)
	defer m.Close()

	require.True(t, m.Available())

	alloc1, err := m.Acquire(owner("sb-1"))
	require.NoError(t, err)
	require.Len(t, alloc1.Devices, 2)

	// Exactly 1 device remains free, but perSandbox is 2: Available() must return false!
	require.False(t, m.Available())

	// Second sandbox must be rejected even though 1 physical device is free
	_, err = m.Acquire(owner("sb-2"))
	require.ErrorIs(t, err, ErrExhausted)

	require.NoError(t, m.Release("sb-1"))
	require.True(t, m.Available())
}

func TestManagerEdgeCasesAndPoisoning(t *testing.T) {
	root := t.TempDir()

	// 1. New fails when assigned device count is less than perSandbox
	_, err := New(t.TempDir(), []string{deviceA}, 2, "pod-a", "k8s.io", DefaultContainerdSocket)
	require.ErrorContains(t, err, "sufficient assigned devices are required")

	m, err := New(root, []string{deviceA, deviceB}, 1, "pod-a", "k8s.io", DefaultContainerdSocket)
	require.NoError(t, err)

	// 2. Devices() returns clone; mutating caller's slice does not mutate inventory
	devs := m.Devices()
	require.Len(t, devs, 2)
	devs[0] = "mutated"
	require.NotEqual(t, devs[0], m.Devices()[0])

	// 3. Acquire with foreign FastletPodUID
	foreignOwner := owner("sb-foreign")
	foreignOwner.FastletPodUID = "pod-other"
	_, err = m.Acquire(foreignOwner)
	require.ErrorContains(t, err, "outside this Fastlet allocation")

	// 4. Release on unknown UID returns nil (idempotent)
	require.NoError(t, m.Release("sb-nonexistent"))

	// 5. ReleaseVerified on unknown UID returns nil (idempotent)
	require.NoError(t, m.ReleaseVerified(Lease{Owner: owner("sb-nonexistent")}))

	// 6. Acquire a device and test ReleaseVerified mismatches
	alloc, err := m.Acquire(owner("sb-test"))
	require.NoError(t, err)

	activeLease := Lease{
		Owner:     owner("sb-test"),
		Devices:   alloc.Devices,
		Namespace: "k8s.io",
		Endpoint:  DefaultContainerdSocket,
	}

	// Mismatched namespace
	badNS := activeLease
	badNS.Namespace = "other-ns"
	require.ErrorContains(t, m.ReleaseVerified(badNS), "GPU owner changed")

	// Mismatched endpoint
	badEndpoint := activeLease
	badEndpoint.Endpoint = "/var/run/other.sock"
	require.ErrorContains(t, m.ReleaseVerified(badEndpoint), "GPU owner changed")

	// Mismatched devices
	badDevices := activeLease
	badDevices.Devices = []string{ResourceName + "=" + deviceB}
	require.ErrorContains(t, m.ReleaseVerified(badDevices), "GPU owner changed")

	// 7. Recover with foreign namespace or endpoint rejected
	badRecover := activeLease
	badRecover.Namespace = "foreign"
	require.ErrorContains(t, m.Recover(badRecover), "cannot adopt another Fastlet runtime")

	badRecoverEndpoint := activeLease
	badRecoverEndpoint.Endpoint = "foreign-endpoint"
	require.ErrorContains(t, m.Recover(badRecoverEndpoint), "cannot adopt another Fastlet runtime")

	// 8. Close poisons the manager
	require.NoError(t, m.Close())
	_, err = m.Acquire(owner("sb-after-close"))
	require.ErrorContains(t, err, "GPU manager is closed")
	require.ErrorContains(t, m.Release("sb-test"), "GPU manager is closed")
}
