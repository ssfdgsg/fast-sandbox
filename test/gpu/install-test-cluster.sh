#!/usr/bin/env bash
# Opt-in bootstrap for a disposable, otherwise empty GPU host. Never replaces an existing cluster or driver.
set -euo pipefail
assets=${1:?Usage: install-test-cluster.sh ASSET_DIRECTORY --dedicated-empty-host}
[[ ${2:-} == --dedicated-empty-host ]] || exit 2
[[ $(id -u) == 0 && $(uname -s) == Linux && $(uname -m) == x86_64 ]] || exit 2
command -v systemctl >/dev/null
unshare --mount true
for existing in /etc/kubernetes/kubelet.conf /etc/rancher/k3s/k3s.yaml /usr/local/bin/k3s; do
 [[ ! -e $existing ]] || { echo "Existing Kubernetes installation detected: $existing; use it explicitly instead." >&2; exit 2; }
done
python3 - "$assets" <<'PY'
import hashlib,pathlib,sys
root=pathlib.Path(sys.argv[1])
sums={x.split()[1].lstrip('*'):x.split()[0] for x in (root/'sha256sum-amd64.txt').read_text().splitlines()}
for name in ['k3s','k3s-airgap-images-amd64.tar.zst']:
 assert hashlib.sha256((root/name).read_bytes()).hexdigest()==sums[name],name
PY
install -m 0755 "$assets/k3s" /usr/local/bin/k3s
mkdir -p /var/lib/rancher/k3s/agent/images
cp "$assets/k3s-airgap-images-amd64.tar.zst" /var/lib/rancher/k3s/agent/images/
cat > /etc/systemd/system/fsb-gpu-k3s.service <<'UNIT'
[Unit]
Description=Disposable FastSandbox GPU test cluster
After=network-online.target
Wants=network-online.target
[Service]
Type=notify
Environment=PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
ExecStart=/usr/local/bin/k3s server --disable=traefik --disable=servicelb --disable=metrics-server --write-kubeconfig-mode=600
Restart=on-failure
RestartSec=3
KillMode=process
Delegate=yes
LimitNOFILE=1048576
TasksMax=infinity
[Install]
WantedBy=multi-user.target
UNIT
systemctl daemon-reload
systemctl start fsb-gpu-k3s.service
for tool in kubectl ctr; do
 command -v "$tool" >/dev/null || ln -s /usr/local/bin/k3s "/usr/local/bin/$tool"
done
export KUBECONFIG=/etc/rancher/k3s/k3s.yaml
for ((attempt=0;attempt<180;attempt++)); do
 if /usr/local/bin/k3s kubectl get nodes --no-headers 2>/dev/null | awk '$2=="Ready"{found=1} END{exit !found}'; then break; fi
 sleep 1
done
/usr/local/bin/k3s kubectl wait --for=condition=Ready nodes --all --timeout=30s
if ! command -v runc >/dev/null; then
 candidate=/var/lib/rancher/k3s/data/current/bin/runc
 [[ -x $candidate ]] && ln -s "$candidate" /usr/local/bin/runc
fi
printf 'KUBECONFIG=/etc/rancher/k3s/k3s.yaml\nFSB_GPU_E2E_SOCKET=/run/k3s/containerd/containerd.sock\n'
printf 'Containerd root: /var/lib/rancher/k3s/agent/containerd; inspect kubelet root and nvidia RuntimeClass before GPU cluster tests.\n'
