#!/usr/bin/env bash
# Diagnostics only. Never kills tasks, deletes leases, or alters the cluster.
set -uo pipefail
out=${1:?Usage: collect.sh OUTPUT_DIRECTORY}
mkdir -p "$out"
socket=${FSB_GPU_E2E_SOCKET:-/run/containerd/containerd.sock}
capture() { local file=$1; shift; timeout 20 "$@" > "$out/$file" 2>&1; printf '%s rc=%s\n' "$file" "$?" >> "$out/collection-status.txt"; }
capture gpu-summary.txt nvidia-smi
capture gpu-processes.csv nvidia-smi --query-compute-apps=gpu_uuid,pid,process_name --format=csv
capture namespaces.txt ctr --address "$socket" namespaces ls
if command -v ctr >/dev/null; then
    while IFS= read -r ns; do
        [[ "$ns" == fsb-gpu-e2e-* ]] || continue
        capture "$ns-containers.txt" ctr --address "$socket" -n "$ns" containers ls
        capture "$ns-tasks.txt" ctr --address "$socket" -n "$ns" tasks ls
    done < <(timeout 10 ctr --address "$socket" namespaces ls -q)
fi
if command -v journalctl >/dev/null; then capture containerd-journal.txt journalctl -u containerd --since '-20 min' --no-pager -n 500; fi
if command -v kubectl >/dev/null && [[ -n ${FSB_GPU_K8S_NAMESPACE:-} ]]; then
    ns=$FSB_GPU_K8S_NAMESPACE
    capture cluster-nodes.txt kubectl get nodes -o wide
    capture test-resources.json kubectl -n "$ns" get sandboxpools,sandboxes,pods -o json
    capture test-events.txt kubectl -n "$ns" get events --sort-by=.lastTimestamp
    capture test-pods-describe.txt kubectl -n "$ns" describe pods
    capture fastlet-logs.txt kubectl -n "$ns" logs -l 'fast-sandbox.io/pool' -c fastlet --tail=500 --prefix=true --max-log-requests=20
fi
# Copy only uniquely prefixed direct-test logs, never credentials or Secrets.
for file in /var/log/fast-sandbox/fsb-gpu-e2e-*.log; do
    [[ -f "$file" ]] && cp "$file" "$out/"
done
exit 0
