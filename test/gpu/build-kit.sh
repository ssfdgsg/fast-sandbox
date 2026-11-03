#!/usr/bin/env bash
# Run from a source checkout; no access to a GPU or remote host is needed.
set -euo pipefail
kit=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
repo=$(cd "$kit/../.." && pwd)
out=${1:?Usage: build-kit.sh OUTPUT_DIRECTORY_OUTSIDE_THIS_KIT}
mkdir -p "$out"
out=$(cd "$out" && pwd)
case "$out/" in "$kit/"*) echo 'Output must be outside the source kit.' >&2; exit 2;; esac
mkdir -p "$out/gpu-test-kit"
cp "$kit/"*.sh "$kit/"*.py "$kit/PLAN.md" "$kit/REPORT.md" "$kit/Dockerfile.p40" "$out/gpu-test-kit/"
cd "$repo"
env -u GOROOT CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go test -c ./internal/runtime/containerd -o "$out/gpu-test-kit/containerd-gpu.test"
python3 - "$out/gpu-test-kit/source.json" <<'PY'
import hashlib, json, pathlib, subprocess, sys
names = subprocess.check_output(['git', 'ls-files', '-z', '--cached', '--others', '--exclude-standard']).split(b'\0')
h = hashlib.sha256()
for name in sorted(set(names)):
    if not name:
        continue
    p = pathlib.Path(name.decode())
    if p.is_file():
        h.update(name + b'\0' + hashlib.sha256(p.read_bytes()).digest())
result = {'base_commit': subprocess.check_output(['git', 'rev-parse', 'HEAD'], text=True).strip(),
          'branch': subprocess.check_output(['git', 'branch', '--show-current'], text=True).strip(),
          'status': subprocess.check_output(['git', 'status', '--short'], text=True),
          'working_source_sha256': h.hexdigest(),
          'note': 'Includes tracked and nonignored untracked source; inspect status for uncommitted changes.'}
pathlib.Path(sys.argv[1]).write_text(json.dumps(result, indent=2) + '\n')
PY
python3 - "$out/gpu-test-kit" <<'PY'
import hashlib, pathlib, sys
root = pathlib.Path(sys.argv[1])
(root / 'SHA256SUMS').write_text(''.join(hashlib.sha256(p.read_bytes()).hexdigest() + '  ' + p.name + '\n' for p in sorted(root.iterdir()) if p.is_file() and p.name != 'SHA256SUMS'))
PY
tar -C "$out" -czf "$out/gpu-test-kit-linux-amd64.tar.gz" gpu-test-kit
printf 'Prepared: %s/gpu-test-kit-linux-amd64.tar.gz\n' "$out"
