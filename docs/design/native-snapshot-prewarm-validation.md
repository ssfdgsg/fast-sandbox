# Native snapshot prewarm: remote validation

Validated on 2026-10-24, using runtime revision `94c0d2f` and the added
`TestFirecrackerSnapshotPrewarmE2E` harness. The uploaded Linux test binary
had SHA-256 `22cb1f5380a9dcc89613bdb22b38a8c26b01ef8d6d9513ed9e45d7b6d66e07d3`.

## Hosts and isolation

| SSH alias | Environment | Validation |
| --- | --- | --- |
| `gcp` | 2 vCPUs, about 1 GiB RAM, no `/dev/kvm` | 11 selected top-level cache/integration tests passed |
| `ciallo` | 1 vCPU, 1,966,060 KiB RAM, Linux 6.8.0-142-generic, AMD nested KVM | Real Firecracker VM snapshot, publication, restore and eviction fallback passed |

`azure` and `aly` were checked for KVM availability; neither exposed KVM.
The VM test used Firecracker/jailer v1.16.1, the project script's Linux
6.1.176 kernel, a 300 MiB Bionic rootfs, and 1-vCPU/512-MiB guests.
StateRoot was a private 6 GiB sparse loop-backed XFS filesystem with reflink.

The runner creates private mount, network and PID namespaces, preserves the
resolver symlink inside its private `/run`, supplies a dummy default egress
interface, and binds a short disk-backed temp path for Unix sockets.
It does not install a toolchain or edit `/etc/fstab`. After execution, no
Firecracker processes, test mounts or loop-device references remained, and
the host route table matched the preflight route table.

## Measured results

Both snapshots contained **851,455,362 artifact bytes**, about **812.01 MiB**.
The real runtime-agent served the Driver over a Unix socket and streamed
publication/pulls through an HTTP file-backed object-store fixture.

| Metric | Prewarm disabled | Prewarm enabled |
| --- | ---: | ---: |
| Entire `CreateSnapshot` call | 9,570.951 ms | 9,331.981 ms |
| Cold `PinImage`, including download and verification | 6,278.668 ms | Not needed |
| Local cache installation after publication | Not enabled | 1.024658 ms |
| Driver clone create, through VM Running | 54.214 ms | 45.794 ms |
| Origin GET requests before/through first clone restore | 5 | 0 |
| Downloaded artifact bytes | 851,455,362 | 0 |

The disabled case fetched index + manifest + three artifacts. The enabled
case restored directly from the newly installed cache, with zero GETs.
The benefit is eliminating the repeated download and verification; these
single samples do not establish an improvement in the VM restore itself.

Both source VMs resumed after snapshot creation, and both restored clones
answered ICMP through their independent slot data planes. With prewarm
enabled, the test then deleted the clone, evicted the template through the
Driver GC, re-pulled all 851,455,362 bytes, and restored a reachable clone
again. That refill + restore + reachability check took **6,244.110 ms**, with
5 origin GETs. The complete paired VM test passed in **40.36 s**.

## Scope

These are functional checks and one cold/warm pair on a small nested-VM
host. The object store was on loopback; the fixture does not verify S3
credentials or measure cloud object-store latency. The test exercises the
real Driver, Agent, Firecracker and Linux network namespaces, with an idle
Linux guest and ICMP reachability. It does not exercise Kubernetes placement,
execd readiness, concurrent guest writes, or crash recovery.

GCP separately passed the prewarm on/off, busy/conflict/capacity handling,
staging cleanup, concurrent pull/GC, active reader, checkpoint reference,
instance rootfs independence and real Agent/UDS/HTTP round-trip checks.

## Reproduce

Build the Linux test binary on a development machine:

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go test -tags firecracker -c \
  -o .build/prewarm-validation/prewarm.test ./internal/runtime/firecracker
```

After authorizing upload to a dedicated KVM host, stage `prewarm.test`,
`scripts/firecracker-prewarm-remote.sh`, and a `task-marker` file under a
new WORK directory. Download the assets used by `scripts/firecracker-e2e.sh`
under `WORK/assets`, retaining the release tarball's
`release-v1.16.1-x86_64` directory, `vmlinux.bin` and `bionic.rootfs.ext4`.
Then run as root:

```bash
bash WORK/firecracker-prewarm-remote.sh WORK
```

The runner checks rootfs download completeness before allocating its test
filesystem. Results are written to `WORK/e2e.log` and `WORK/e2e.exit`.
Artifact GET counters and durations appear as `PREWARM_RESULT` and
`PREWARM_FALLBACK`; cache installation time appears in the Driver log.
