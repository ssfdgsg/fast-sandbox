#!/usr/bin/env bash
# Read-only host checks; writes evidence only to the explicitly selected output directory.
set -euo pipefail
out=${1:?Usage: preflight.sh OUTPUT_DIRECTORY}
mkdir -p "$out"
exec > >(tee "$out/preflight.log") 2>&1
failures=0
check() { if "$@"; then printf 'OK: %s\n' "$*"; else printf 'FAIL: %s\n' "$*"; failures=$((failures + 1)); fi; }
check test "$(uname -s)" = Linux
check test "$(uname -m)" = x86_64
check test "$(id -u)" = 0
for tool in python3 nvidia-smi nvidia-ctk ctr runc unshare ip iptables timeout; do check command -v "$tool"; done
socket=${FSB_GPU_E2E_SOCKET:-/run/containerd/containerd.sock}
check test -S "$socket"
check test -w /sys/fs/cgroup
check unshare --mount true
uname -a
cat /etc/os-release || true
cat /proc/self/status | sed -n '/^Cap/p; /^Seccomp/p; /^NoNewPrivs/p'
findmnt /sys/fs/cgroup || true
if command -v nvidia-smi >/dev/null; then
    nvidia-smi -L || true
    nvidia-smi --query-gpu=uuid,name,driver_version,memory.total --format=csv > "$out/gpus.csv" || failures=$((failures + 1))
    cat "$out/gpus.csv"
    nvidia-smi --query-compute-apps=gpu_uuid,pid,process_name --format=csv > "$out/compute-apps.csv" || failures=$((failures + 1))
fi
if command -v nvidia-ctk >/dev/null; then
    nvidia-ctk --version
    nvidia-ctk cdi list > "$out/cdi.txt" || failures=$((failures + 1))
    cat "$out/cdi.txt"
fi
if command -v ctr >/dev/null; then
    check timeout 15 ctr --address "$socket" version
    ctr --address "$socket" plugins ls > "$out/containerd-plugins.txt" 2>&1 || true
    ctr --address "$socket" -n k8s.io images ls > "$out/images.txt" 2>&1 || true
fi
if command -v runc >/dev/null; then runc --version; fi
if command -v python3 >/dev/null; then
    export FSB_GPU_E2E_DEVICES
    check python3 - "$out" <<'PY'
import csv, os, pathlib, re, sys
out = pathlib.Path(sys.argv[1])
ids = os.environ.get('FSB_GPU_E2E_DEVICES', '').split(',')
assert ids != [''], 'Supply explicitly reserved, idle full GPU UUIDs in FSB_GPU_E2E_DEVICES'
assert len(ids) == len(set(ids)), 'Duplicate GPU UUID'
assert all(re.fullmatch(r'GPU-[0-9a-fA-F]{8}(?:-[0-9a-fA-F]{4}){3}-[0-9a-fA-F]{12}', x) for x in ids), 'Only whole-device UUIDs are accepted'
with (out / 'gpus.csv').open() as f:
    inventory = {row[0].strip() for row in list(csv.reader(f))[1:]}
assert set(ids) <= inventory, 'Selected UUID not in nvidia-smi inventory'
cdi = (out / 'cdi.txt').read_text().splitlines()
for uuid in ids:
    assert 'nvidia.com/gpu=' + uuid in [x.strip() for x in cdi], 'Missing exact UUID CDI name: ' + uuid
with (out / 'compute-apps.csv').open() as f:
    busy = {row[0].strip() for row in list(csv.reader(f))[1:] if row}
assert not set(ids) & busy, 'Selected GPU has active CUDA processes; reserve an idle device first'
print('Selected UUIDs exist, have CDI entries, and have no reported CUDA processes.')
print('This is not proof of reservation: stop GPU Pools/other allocators before the direct-driver test.')
PY
fi
if grep -qi microsoft /proc/sys/kernel/osrelease; then
    echo 'LIMITED: WSL cannot qualify native Linux Kubernetes GPU isolation; Tesla P40 is not an officially supported WSL configuration.'
fi
printf 'Preflight failures: %s\n' "$failures"
(( failures == 0 ))
