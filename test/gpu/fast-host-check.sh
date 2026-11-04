#!/usr/bin/env bash
# First SSH action: detect unsuitable rentals before installing or pulling anything.
set -euo pipefail
[[ $(uname -s) == Linux && $(uname -m) == x86_64 && $(id -u) == 0 ]] || { echo 'Need native Linux/amd64 with root or sudo.' >&2; exit 2; }
python3 - <<'PY'
from pathlib import Path
caps=int(next(x.split()[1] for x in Path('/proc/self/status').read_text().splitlines() if x.startswith('CapEff:')),16)
missing=[name for bit,name in [(12,'CAP_NET_ADMIN'),(21,'CAP_SYS_ADMIN'),(27,'CAP_MKNOD')] if not caps & (1<<bit)]
assert not missing, 'Unsuitable restricted GPU container: missing '+','.join(missing)
assert 'microsoft' not in Path('/proc/sys/kernel/osrelease').read_text().lower(), 'Native Linux is required for dual-GPU physical isolation acceptance'
PY
[[ -w /sys/fs/cgroup ]] || { echo 'Read-only cgroup: unsuitable runtime test host.' >&2; exit 2; }
unshare --mount --net true
nvidia-smi --query-gpu=uuid,name,driver_version,memory.total --format=csv
nvidia-smi --query-compute-apps=gpu_uuid,pid,process_name --format=csv
printf 'HOST_PRIVILEGES_OK. Inspect the two reserved UUIDs and current GPU processes before testing.\n'
