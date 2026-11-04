#!/usr/bin/env bash
# Run on a dedicated native-Linux GPU host after preloading the selected image.
set -euo pipefail
kit=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
out=${1:?Usage: run-dual.sh OUTPUT_DIRECTORY}
: "${FSB_GPU_E2E_DEVICES:?Reserve exactly two idle GPU UUIDs, separated by comma}"
: "${FSB_GPU_E2E_IMAGE:?Set a preloaded image containing Python; PyTorch is not required}"
binary=${FSB_GPU_TEST_BINARY:-$kit/containerd-gpu.test}
[[ -x "$binary" ]] || { echo "Missing test binary: $binary" >&2; exit 2; }
export FSB_GPU_E2E_MULTI=1
rounds=${FSB_GPU_E2E_ROUNDS:-1}
[[ $rounds =~ ^[1-9][0-9]*$ ]] || exit 2
mkdir -p "$out"
out=$(cd "$out" && pwd)
trap 'rc=$?; trap - EXIT; bash "$kit/collect.sh" "$out/after"; exit "$rc"' EXIT
bash "$kit/preflight.sh" "$out/preflight"
python3 - <<'PY'
import os
ids=os.environ['FSB_GPU_E2E_DEVICES'].split(',')
assert len(ids)==2 and len(set(ids))==2, 'Exactly two distinct UUIDs required'
PY
socket=${FSB_GPU_E2E_SOCKET:-/run/containerd/containerd.sock}
ctr --address "$socket" -n k8s.io images ls -q | grep -Fx -- "$FSB_GPU_E2E_IMAGE" > "$out/selected-image.txt"
start=$SECONDS
for ((round=1; round<=rounds; round++)); do
    "$binary" -test.run '^(TestGPUContainerdDualCUDA|TestGPUContainerdMultiFastletCUDA)$' -test.v -test.count=1 -test.timeout=10m 2>&1 | tee "$out/dual-$round.log"
    for name in TestGPUContainerdDualCUDA TestGPUContainerdMultiFastletCUDA; do
        grep -q -- "--- PASS: $name" "$out/dual-$round.log" || { echo "Missing hardware PASS for $name" >&2; exit 1; }
    done
done
printf 'PASS: %s round(s), elapsed=%ss. Native CUDA, UUID isolation, exhaustion/reuse, CPU isolation, two-GPU Sandbox, and independent driver ownership.\n' "$rounds" "$((SECONDS-start))" | tee "$out/summary.txt"
printf 'PENDING: Kubernetes allocation, Fastlet process/Pod crash, and cluster scaling.\n' | tee -a "$out/summary.txt"
