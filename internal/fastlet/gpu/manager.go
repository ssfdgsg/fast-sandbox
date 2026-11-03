// Package gpu owns exclusive device leases inside one Kubernetes GPU allocation.
package gpu

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"

	"golang.org/x/sys/unix"

	fastletapi "fast-sandbox/internal/protocol/fastlet"
)

const ResourceName = "nvidia.com/gpu"
const DefaultContainerdSocket = "/run/containerd/containerd.sock"
const StateRoot = "/var/lib/fast-sandbox/gpu"
const PodResourcesSocket = "/run/fast-sandbox/pod-resources/kubelet.sock"

var ErrExhausted = errors.New("no unallocated GPUs")
var uuidPattern = regexp.MustCompile(`^GPU-[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

type Lease struct {
	Endpoint  string                     `json:"endpoint"`
	Namespace string                     `json:"namespace"`
	Owner     fastletapi.SandboxIdentity `json:"owner"`
	Devices   []string                   `json:"devices"`
}

type state struct {
	Version int              `json:"version"`
	Leases  map[string]Lease `json:"leases"`
}

// Manager persists reservations before runtime side effects. A failed cleanup
// retains its lease; only the runtime driver may release after proving absence.
type Manager struct {
	mu        sync.Mutex
	inventory []string
	count     int
	path      string
	state     state
	locks     []*os.File
	podUID    string
	namespace string
	endpoint  string
	poisoned  error
}

func New(root string, ids []string, perSandbox int, podUID, namespace, endpoint string) (*Manager, error) {
	if root == "" || podUID == "" || namespace == "" || endpoint == "" || perSandbox <= 0 || len(ids) < perSandbox {
		return nil, errors.New("GPU state root and sufficient assigned devices are required")
	}
	inventory := make([]string, 0, len(ids))
	for _, id := range ids {
		if !uuidPattern.MatchString(id) {
			return nil, fmt.Errorf("GPU %q is not an exclusive full-device UUID; index, MIG and replicated devices are unsupported", id)
		}
		device := ResourceName + "=" + id
		if slices.Contains(inventory, device) {
			return nil, fmt.Errorf("duplicate GPU %q", id)
		}
		inventory = append(inventory, device)
	}
	sort.Strings(inventory)
	if err := os.MkdirAll(root, 0700); err != nil {
		return nil, err
	}
	m := &Manager{inventory: inventory, count: perSandbox, path: filepath.Join(root, "leases.json"), podUID: podUID, namespace: namespace, endpoint: endpoint}
	// Hold one node-wide lock per GPU for the lifetime of the Fastlet process.
	// Other Fastlets with disjoint Kubernetes assignments may run concurrently.
	for _, id := range ids {
		lock, err := os.OpenFile(filepath.Join(root, id+".lock"), os.O_CREATE|os.O_RDWR, 0600)
		if err != nil {
			_ = m.Close()
			return nil, err
		}
		if err = unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
			_ = lock.Close()
			_ = m.Close()
			return nil, fmt.Errorf("GPU %s still has an active Fastlet owner: %w", id, err)
		}
		m.locks = append(m.locks, lock)
	}
	unlock, err := m.readLocked()
	if err != nil {
		_ = m.Close()
		return nil, err
	}
	unlock()
	return m, nil
}

// readLocked serializes node journal updates between independent Fastlets.
// Callers hold m.mu; the file lock stays held through commit.
func (m *Manager) readLocked() (func(), error) {
	if m.poisoned != nil {
		return nil, m.poisoned
	}
	f, err := os.OpenFile(filepath.Join(filepath.Dir(m.path), "leases.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, err
	}
	unlock := func() { _ = f.Close() }
	next := state{Version: 1, Leases: map[string]Lease{}}
	raw, err := os.ReadFile(m.path)
	if err == nil {
		err = json.Unmarshal(raw, &next)
	} else if errors.Is(err, os.ErrNotExist) {
		err = nil
	}
	if err == nil && (next.Version != 1 || next.Leases == nil) {
		err = errors.New("unsupported node GPU journal")
	}
	if err == nil {
		err = m.validate(next.Leases)
	}
	if err != nil {
		unlock()
		return nil, fmt.Errorf("read node GPU journal: %w", err)
	}
	m.state = next
	return unlock, nil
}

func sameOwner(a, b fastletapi.SandboxIdentity) bool {
	return a.SandboxUID == b.SandboxUID && a.FastletPodUID == b.FastletPodUID && a.RuntimeInstanceID == b.RuntimeInstanceID && a.InstanceGeneration == b.InstanceGeneration && a.AssignmentAttempt == b.AssignmentAttempt
}

func (m *Manager) validate(leases map[string]Lease) error {
	used := map[string]string{}
	for uid, lease := range leases {
		if uid == "" || uid != lease.Owner.SandboxUID || lease.Owner.FastletPodUID == "" || lease.Owner.RuntimeInstanceID == "" || lease.Owner.InstanceGeneration <= 0 || lease.Owner.AssignmentAttempt <= 0 || lease.Namespace == "" || lease.Endpoint == "" {
			return errors.New("GPU lease has an incomplete owner")
		}
		if len(lease.Devices) == 0 || (lease.Owner.FastletPodUID == m.podUID && (len(lease.Devices) != m.count || lease.Namespace != m.namespace || lease.Endpoint != m.endpoint)) {
			return fmt.Errorf("GPU lease %s has the wrong device count", uid)
		}
		for _, device := range lease.Devices {
			if !strings.HasPrefix(device, ResourceName+"=") || !uuidPattern.MatchString(strings.TrimPrefix(device, ResourceName+"=")) || (lease.Owner.FastletPodUID == m.podUID && !slices.Contains(m.inventory, device)) {
				return fmt.Errorf("GPU lease %s references unassigned device %s", uid, device)
			}
			if previous, exists := used[device]; exists {
				return fmt.Errorf("GPU %s is leased twice (%s, %s)", device, previous, uid)
			}
			used[device] = uid
		}
	}
	return nil
}

func (m *Manager) Acquire(owner fastletapi.SandboxIdentity) (*fastletapi.GPUAllocation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	unlock, err := m.readLocked()
	if err != nil {
		return nil, err
	}
	defer unlock()
	if owner.FastletPodUID != m.podUID {
		return nil, errors.New("GPU request is outside this Fastlet allocation")
	}
	if lease, exists := m.state.Leases[owner.SandboxUID]; exists {
		if !sameOwner(lease.Owner, owner) {
			return nil, errors.New("GPU lease belongs to a different runtime instance")
		}
		return &fastletapi.GPUAllocation{Devices: slices.Clone(lease.Devices)}, nil
	}
	used := map[string]bool{}
	for _, lease := range m.state.Leases {
		for _, device := range lease.Devices {
			used[device] = true
		}
	}
	devices := []string{}
	for _, device := range m.inventory {
		if !used[device] {
			devices = append(devices, device)
			if len(devices) == m.count {
				break
			}
		}
	}
	if len(devices) != m.count {
		return nil, ErrExhausted
	}
	next := m.cloneLeases()
	next[owner.SandboxUID] = Lease{Owner: owner, Devices: devices, Namespace: m.namespace, Endpoint: m.endpoint}
	if err := m.commit(next); err != nil {
		return nil, err
	}
	return &fastletapi.GPUAllocation{Devices: slices.Clone(devices)}, nil
}

// Recover adopts only allocations verified against containerd inventory. Pending
// leases are retained until the driver has explicitly cleaned their runtime IDs.
func (m *Manager) Recover(lease Lease) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	unlock, err := m.readLocked()
	if err != nil {
		return err
	}
	defer unlock()
	if lease.Owner.FastletPodUID != m.podUID || lease.Namespace != m.namespace || lease.Endpoint != m.endpoint {
		return errors.New("cannot adopt another Fastlet runtime")
	}
	if previous, ok := m.state.Leases[lease.Owner.SandboxUID]; ok {
		if !sameOwner(previous.Owner, lease.Owner) || !slices.Equal(previous.Devices, lease.Devices) || previous.Namespace != lease.Namespace || previous.Endpoint != lease.Endpoint {
			return errors.New("runtime and durable GPU lease disagree")
		}
		return nil
	}
	next := m.cloneLeases()
	next[lease.Owner.SandboxUID] = lease
	return m.commit(next)
}

// Release must be called only after runtime task/container absence is proven.
func (m *Manager) Release(uid string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	unlock, err := m.readLocked()
	if err != nil {
		return err
	}
	defer unlock()
	if lease, exists := m.state.Leases[uid]; exists && lease.Owner.FastletPodUID != m.podUID {
		return errors.New("cannot release another Fastlet runtime without verified absence")
	}
	if _, exists := m.state.Leases[uid]; !exists {
		return m.poisoned
	}
	next := m.cloneLeases()
	delete(next, uid)
	return m.commit(next)
}

// Records returns leases relevant to this assignment, including abandoned prior owners.
func (m *Manager) Records() ([]Lease, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	unlock, err := m.readLocked()
	if err != nil {
		return nil, err
	}
	defer unlock()
	result := []Lease{}
	for _, lease := range m.state.Leases {
		relevant := lease.Owner.FastletPodUID == m.podUID
		for _, device := range lease.Devices {
			relevant = relevant || slices.Contains(m.inventory, device)
		}
		if relevant {
			lease.Devices = slices.Clone(lease.Devices)
			result = append(result, lease)
		}
	}
	return result, nil
}

func (m *Manager) Leases() []Lease {
	records, err := m.Records()
	if err != nil {
		return nil
	}
	result := []Lease{}
	for _, lease := range records {
		if lease.Owner.FastletPodUID == m.podUID {
			result = append(result, lease)
		}
	}
	return result
}

// ReleaseVerified uses a complete lease comparison after the caller has proven
// both the container and task absent, including when its old Fastlet is gone.
func (m *Manager) ReleaseVerified(expected Lease) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	unlock, err := m.readLocked()
	if err != nil {
		return err
	}
	defer unlock()
	current, exists := m.state.Leases[expected.Owner.SandboxUID]
	if !exists {
		return nil
	}
	if !sameOwner(current.Owner, expected.Owner) || current.Namespace != expected.Namespace || current.Endpoint != expected.Endpoint || !slices.Equal(current.Devices, expected.Devices) {
		return errors.New("GPU owner changed during absence verification")
	}
	next := m.cloneLeases()
	delete(next, expected.Owner.SandboxUID)
	return m.commit(next)
}

func (m *Manager) Available() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	unlock, err := m.readLocked()
	if err != nil {
		return false
	}
	defer unlock()
	free := len(m.inventory)
	for _, lease := range m.state.Leases {
		for _, device := range lease.Devices {
			if slices.Contains(m.inventory, device) {
				free--
			}
		}
	}
	return free >= m.count
}

func (m *Manager) cloneLeases() map[string]Lease {
	next := map[string]Lease{}
	for uid, lease := range m.state.Leases {
		lease.Devices = slices.Clone(lease.Devices)
		next[uid] = lease
	}
	return next
}

func (m *Manager) commit(next map[string]Lease) error {
	if m.poisoned != nil {
		return m.poisoned
	}
	if err := m.validate(next); err != nil {
		return err
	}
	raw, err := json.Marshal(state{Version: 1, Leases: next})
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(m.path), ".leases-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(raw)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = os.Rename(f.Name(), m.path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(m.path))
	if err == nil {
		err = dir.Sync()
		_ = dir.Close()
	}
	if err != nil {
		m.poisoned = fmt.Errorf("GPU lease durability is uncertain: %w", err)
		return m.poisoned
	}
	m.state = state{Version: 1, Leases: next}
	return nil
}

func (m *Manager) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	var result error
	for _, lock := range m.locks {
		result = errors.Join(result, lock.Close())
	}
	m.locks = nil
	m.poisoned = errors.New("GPU manager is closed")
	return result
}

func (m *Manager) Devices() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.inventory)
}
