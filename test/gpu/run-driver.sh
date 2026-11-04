#!/usr/bin/env bash
set -euo pipefail
kit=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
out=${1:?Usage: run-driver.sh OUTPUT_DIRECTORY}
: "${FSB_GPU_E2E_DEVICES:?Explicitly select reserved, idle GPU UUIDs}"
: "${FSB_GPU_E2E_IMAGE:?Supply a pre-pulled compatible PyTorch image reference}"
binary=${FSB_GPU_TEST_BINARY:-$kit/containerd-gpu.test}
[[ -x "$binary" ]] || { echo "Missing Linux test binary: $binary" >&2; exit 2; }
mkdir -p "$out"
out=$(cd "$out" && pwd)
trap 'rc=$?; trap - EXIT; bash "$kit/collect.sh" "$out/after"; exit "$rc"' EXIT
bash "$kit/preflight.sh" "$out/preflight"
socket=${FSB_GPU_E2E_SOCKET:-/run/containerd/containerd.sock}
ctr --address "$socket" -n k8s.io images ls > "$out/images.txt"
ctr --address "$socket" -n k8s.io images ls -q | grep -Fx -- "$FSB_GPU_E2E_IMAGE" > "$out/selected-image.txt"
python3 - "$out/invocation.json" <<'PY'
import json, os, sys
keys = ('FSB_GPU_E2E_DEVICES', 'FSB_GPU_E2E_IMAGE', 'FSB_GPU_E2E_SOCKET', 'FSB_GPU_E2E_SNAPSHOTTER', 'FSB_GPU_E2E_REUSE_SOURCE_NAMESPACE')
with open(sys.argv[1], 'w') as f:
    json.dump({k: os.environ.get(k) for k in keys}, f, indent=2)
PY
set +e
"$binary" -test.run '^TestGPUContainerdCUDA$' -test.v -test.count=1 -test.timeout=10m 2>&1 | tee "$out/driver.log"
rc=${PIPESTATUS[0]}
set -e
[[ "$rc" == 0 ]] || exit "$rc"
grep -q -- '--- PASS: TestGPUContainerdCUDA' "$out/driver.log" || { echo 'Hardware test did not pass (a skip is not success).' >&2; exit 1; }
echo 'PASS: direct CUDA, idempotent Ensure, manager reload/recovery, and successful runtime cleanup.'
echo 'PENDING: Kubernetes allocation, physical multi-GPU isolation, and actual process-crash recovery.'
