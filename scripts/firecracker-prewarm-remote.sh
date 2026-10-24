#!/usr/bin/env bash
# Run a precompiled snapshot-prewarm E2E binary in an isolated Linux host
# environment. WORK contains prewarm.test, task-marker and downloaded assets.
# Uploading the project binary requires authorization for the target host.
set -euo pipefail

work="$(realpath "${1:?usage: firecracker-prewarm-remote.sh WORK}")"
[[ -f "$work/task-marker" ]] || { echo "missing test-directory marker" >&2; exit 1; }
[[ $(id -u) -eq 0 ]] || { echo "requires root" >&2; exit 1; }
[[ -c /dev/kvm && -c /dev/net/tun ]] || { echo "requires KVM and TUN" >&2; exit 1; }
for tool in unshare mount umount ip iptables ping mkfs.xfs timeout python3; do
    command -v "$tool" >/dev/null
done

if [[ "${2:-}" != --isolated ]]; then
    exec unshare --mount --net --pid --fork --mount-proc bash "$0" "$work" --isolated
fi

# Mounts, network devices, routes, rules, process inspection and /run are
# private to this test. The existing E2E helpers can safely purge fsb netns.
mount --make-rprivate /
resolver_target="$(readlink -f /etc/resolv.conf)"
cp -L /etc/resolv.conf "$work/resolv.conf"
mount -t tmpfs -o mode=755 tmpfs /run
# Some distributions link /etc/resolv.conf into /run/systemd/resolve.
# Preserve the readable resolver inside the private tmpfs for slot setup.
if [[ "$resolver_target" == /run/* ]]; then
    mkdir -p "$(dirname "$resolver_target")"
    cp "$work/resolv.conf" "$resolver_target"
fi
mkdir -p /run/netns /run/fast-sandbox/netns "$work/state" "$work/tmp"
# Go's long test names plus WORK can exceed Unix socket's 108-byte limit.
# Give disk-backed temp files a short path inside the private mount namespace.
mkdir /run/t
mount --bind "$work/tmp" /run/t
ip link set lo up
# Slot setup discovers a default egress interface even though this test's
# object store and probes stay local. Supply one without host connectivity.
ip link add test-egress type dummy
ip addr add 192.0.2.1/24 dev test-egress
ip link set test-egress up
ip route add default dev test-egress

python3 - "$work/assets/bionic.rootfs.ext4" <<'PY'
import os, struct, sys
with open(sys.argv[1], 'rb') as source:
    source.seek(1024)
    sb = source.read(1024)
if len(sb) != 1024 or struct.unpack_from('<H', sb, 56)[0] != 0xef53:
    raise SystemExit('rootfs has no ext filesystem superblock')
blocks = struct.unpack_from('<I', sb, 4)[0] + (struct.unpack_from('<I', sb, 336)[0] << 32)
expected = blocks * (1024 << struct.unpack_from('<I', sb, 24)[0])
if os.path.getsize(sys.argv[1]) < expected:
    raise SystemExit(f'rootfs download is incomplete; expected at least {expected} bytes')
PY

# Format only this task's new regular file. Do not alter disks or /etc/fstab.
[[ ! -e "$work/state.xfs" ]] || { echo "state.xfs already exists; use a fresh test directory" >&2; exit 1; }
truncate -s 6G "$work/state.xfs"
mkfs.xfs -f -m reflink=1 "$work/state.xfs" > "$work/xfs-format.log"
mount -o loop,noatime "$work/state.xfs" "$work/state"
# mount's autoclear loop device is released when the final mount/FD closes.
trap 'umount /run/t 2>/dev/null || true; umount "$work/state" 2>/dev/null || true' EXIT

assets="$work/assets/release-v1.16.1-x86_64"
mkdir -p "$work/bin"
cp "$assets/firecracker-v1.16.1-x86_64" "$work/bin/firecracker"
cp "$assets/jailer-v1.16.1-x86_64" "$work/bin/jailer"
set +e
env FC_BINARY="$work/bin/firecracker" \
    FC_JAILER="$work/bin/jailer" \
    FC_KERNEL="$work/assets/vmlinux.bin" \
    FC_ROOTFS="$work/assets/bionic.rootfs.ext4" \
    FC_STATE_ROOT="$work/state" TMPDIR=/run/t GOMEMLIMIT=128MiB \
    timeout --signal=TERM --kill-after=15s 11m "$work/prewarm.test" \
    -test.run '^TestFirecrackerSnapshotPrewarmE2E$' \
    -test.count=1 -test.timeout=10m -test.v 2>&1 | tee "$work/e2e.log"
status=${PIPESTATUS[0]}
set -e
printf '%s\n' "$status" > "$work/e2e.exit"
exit "$status"
