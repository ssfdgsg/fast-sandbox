# Containerd GPU qualification plan

This plan qualifies containerd NVIDIA GPU support; it is not a statement of completed hardware validation. Use the matching `REPORT.md` for each host. Record the exact source revision, working-tree changes and component image digests. The portable bundle records source identity and checksums.

## 1. Rental requirements and coverage

Use an Ubuntu 22.04/24.04 Linux x86_64 host or VM with directly exposed NVIDIA GPUs, root/sudo, writable cgroup v2, functioning mount namespaces, and permission to run privileged Kubernetes Pods. Suggested test budget: 8 vCPUs, 32 GiB RAM, 100 GiB free disk, and registry access. These are test resource estimates, not minimum product requirements. KVM and nested virtualization are unnecessary. Do not rent an ordinary GPU development container that only offers root inside an unprivileged container.

| Available hardware | Coverage |
| --- | --- |
| 1 P40 | CUDA, CDI, one-slot exhaustion, cleanup/reuse, restart/failure tests, CPU regression; external Pod and Pool must take turns |
| 2 P40s | Add simultaneous per-Sandbox physical UUID isolation, multi-Fastlet ownership, two-GPU Sandbox, GPU-capacity scaling |
| 3 GPUs | Add external Kubernetes GPU workload concurrently with a two-GPU Fastlet |
| 2 GPU nodes | Optional cross-node scheduling, failed-node recovery and new-node capacity; persistent GPU memory migration is not supported |

Prefer two cards for complete single-node qualification. P40 is Pascal (compute capability 6.1); use a compatible driver and PyTorch CUDA build. The included candidate image pins PyTorch 2.7.1/cu126, based on the official previous-version distribution. This candidate has NOT yet been built or executed on a P40. Record `torch.cuda.get_arch_list()` and actual successful CUDA execution; `nvidia-smi` alone is insufficient. Do not reuse the RTX 5090 cu128 example without validating Pascal support. P40 under WSL is not an officially supported qualification platform. Other WSL setups are supplemental smoke tests only, not native Linux device-isolation acceptance.

## 2. Prepare locally before renting

```bash
bash test/gpu/build-kit.sh /tmp/fsb-gpu-delivery
```

The archive contains a Linux/amd64 direct-driver test binary, preflight/runner/collection scripts, a CUDA probe, fixture renderer, candidate P40 Dockerfile, this plan, a report template, checksums, and source identity. The driver binary requires no Go installation on the rented host. Kubernetes deployment still requires this exact source snapshot and component images built from it; the archive alone does not deploy FastSandbox. Build remote images from the exact tested checkout, including any recorded uncommitted changes. Do not publish or push images to a registry without choosing the authorized test registry.

Local gates from the source checkout:

```bash
env -u GOROOT go test ./api/v1alpha2 ./internal/fastlet/gpu ./internal/runtime/containerd ./internal/controlplane/reconciler ./internal/controlplane/fastpath ./cmd/fastlet
env -u GOROOT go test -race ./internal/fastlet/gpu ./internal/runtime/containerd ./internal/controlplane/reconciler ./cmd/fastlet
make test SCOPE=python
```

On the Linux host run `make test SCOPE=unit`, `make lint`, and `make verify` with the toolchain in `go.mod` and the pinned generators. Report any baseline failure separately with the same command on the clean base revision. Platform-specific failures in a macOS run are not waivers for the Linux run. An unselected hardware test reports SKIP and is not hardware acceptance.

## 3. Inventory and install prerequisites

After SSH access is provided, first record OS/kernel, privilege, cgroup mounts, GPU UUIDs, driver/toolkit/containerd/runc versions, free disk and existing workloads. The preflight script collects these without modifying the host. Do not run the standalone driver against GPUs concurrently managed by an active Pool or another allocator.

Install host-compatible NVIDIA driver, containerd/runc, NVIDIA Container Toolkit, `iproute2`, `iptables`, `util-linux`, Python 3 and GNU coreutils. Use NVIDIA's supported package instructions after inspecting the rented image; there is intentionally no blind driver-replacement script. Keep the host's existing functional driver when compatible. A driver mismatch is an environment failure, not a reason to loosen the GPU allocator.

Choose one CDI-generation owner. If `nvidia-cdi-refresh` already generates a spec under `/var/run/cdi`, configure/refresh it with UUID device names and avoid conflicting definitions under `/etc/cdi`. Otherwise generate one UUID-named spec:

```bash
sudo mkdir -p /etc/cdi
sudo nvidia-ctk cdi generate --device-name-strategy=uuid --output=/etc/cdi/nvidia.yaml
nvidia-ctk cdi list
```

Every selected device must resolve as `nvidia.com/gpu=GPU-<uuid>`. All host library/device/hook paths in the selected CDI spec must exist in the Fastlet/containerd execution context. Merely configuring NVIDIA support in containerd's CRI plugin does not qualify FastSandbox's direct client injection.

## 4. Build and stage the CUDA image

On the test host, or a Linux image builder:

```bash
docker build -f gpu-test-kit/Dockerfile.p40 -t fsb-gpu-p40:cu126 gpu-test-kit
docker save fsb-gpu-p40:cu126 -o fsb-gpu-p40.tar
sudo ctr --address /run/containerd/containerd.sock -n k8s.io images import fsb-gpu-p40.tar
sudo ctr --address /run/containerd/containerd.sock -n k8s.io images ls
```

Use the exact imported reference in `FSB_GPU_E2E_IMAGE` (normally `docker.io/library/fsb-gpu-p40:cu126`; verify the listing). Record its content digest and keep image download/build time separate from Sandbox startup latency. A preexisting compatible image is also acceptable. FastSandbox first looks up Sandbox images in its resolved containerd namespace and pulls only when missing; Sandbox has no `imagePullPolicy` field. Import the exact image reference on every eligible node before running. The renderer sets the external ordinary Pod's `imagePullPolicy: Never`; use `--pod-pull-policy IfNotPresent` for a registry image and record the resolved digest. No live acceptance has been completed for this candidate image.

## 5. Standalone containerd gate

Select one idle, explicitly reserved device from `nvidia-smi -L`; do not use GPU indexes or `all`.

```bash
cd gpu-test-kit
sha256sum -c SHA256SUMS
sudo env FSB_GPU_E2E_DEVICES=GPU-REPLACE_WITH_RESERVED_UUID \
  FSB_GPU_E2E_IMAGE=docker.io/library/fsb-gpu-p40:cu126 \
  bash run-driver.sh /tmp/fsb-gpu-results/driver-01
```

Optional overrides: `FSB_GPU_E2E_SOCKET`, `FSB_GPU_E2E_SNAPSHOTTER`, `FSB_GPU_TEST_BINARY`. The preflight refuses missing UUID CDI entries, insufficient host privileges, nonexistent devices and reported active CUDA processes. Absence of reported compute processes is not a reservation guarantee; disable competing allocators for this phase. The runner rejects a skipped test, preserves its exit code, records evidence, and never deletes unrelated tasks. Repeat ten times in distinct result directories, serially. Successful hardware qualification must show ten PASS records, no leaked test containers/tasks and stable free capacity. Images and content caches may remain intentionally and must be reported.

The current automatic driver test covers a real CUDA matmul, one visible GPU, idempotent Ensure, lease-manager close/reload while the task stays alive, recovery from labels, deletion and lease release. It does NOT automate multi-GPU exhaustion, full process SIGKILL, Kubernetes allocation, failure injection or node reboot. Those are separate gates below. It currently uses a private temporary journal, not the production node journal.

## 6. Kubernetes setup on the native GPU node

Use a disposable native Kubernetes installation (kubeadm or an existing dedicated GPU cluster), not an unconfigured Kind node. Containerd, kubelet PodResources, GPU device files, driver libraries and CDI paths must all refer to the actual runtime host. If using K3s or custom paths, update the platform runtime-environment ConfigMap before creating Pools and restart NodeJanitor as required by `docs/guides/runtime-environments.md`. Do not assume `/run/containerd/containerd.sock` or `/var/lib/kubelet` on those distributions.

Install a pinned NVIDIA device-plugin version. Record its image digest and full effective config: `deviceIDStrategy: uuid`, `migStrategy: none`, no `sharing` section/time-slicing/MPS. Configure the device-list strategy for the actual CRI setup: either functioning NVIDIA runtime/envvar integration or tested native CDI/CRI support. Confirm a normal one-GPU Kubernetes Pod runs the included CUDA probe before deploying FastSandbox. Confirm Node allocatable GPUs equals the physical exclusive GPU count, and the kubelet PodResources socket exists. Avoid `--gpus all` as evidence of exclusive assignment.

Build all five core images from the synchronized branch with `make images COMPONENT=core`, then import their image archives into the node's actual containerd `k8s.io` namespace or publish to the selected test registry. Keep Controller, Fastlet, Fastlet Proxy, Sandbox Proxy and Janitor image IDs in the report. Use `make build COMPONENT=fastctl` for client checks. Apply generated CRDs and `config/dev` only to this dedicated test cluster; its public development route keys must not be used on a production cluster. Confirm all system workloads become Ready. Do not modify Pool-supplied runtime handler/security fields to work around a readiness failure.

```bash
kubectl apply -k config/crd
kubectl apply -k config/dev
kubectl -n fast-sandbox-system get deployments,daemonsets,pods -o wide
python3 gpu-test-kit/render.py \
  --image docker.io/library/fsb-gpu-p40:cu126 \
  --slots 2 --namespace fsb-gpu-e2e --output /tmp/fsb-gpu-fixtures
kubectl apply -f /tmp/fsb-gpu-fixtures/namespace.json
kubectl apply --dry-run=server -f /tmp/fsb-gpu-fixtures/gpu-pool.json
kubectl apply -f /tmp/fsb-gpu-fixtures/gpu-pool.json
kubectl -n fsb-gpu-e2e get pods -l fast-sandbox.io/pool=gpu-test-pool -o yaml
```

For one GPU use `--slots 1`. The GPU owner container must have both GPU request and limit equal to `slots * gpus-per-sandbox`, UUID discovery settings, CDI mounts, PodResources mount and durable lease mount. Wait for the Fastlet Pod Ready condition, then apply Sandbox fixtures one at a time. Do NOT apply the entire fixture directory, which intentionally contains overflow and optional external workloads. Inspect Sandbox runtime state and the actual CUDA marker in `/var/log/fast-sandbox/<Sandbox-UID>.log` on its assigned node. Status Ready alone is not CUDA evidence. Snapshot lease journal, runtime labels/spec and task PID before failure experiments.

For the two-slot case, execute and retain the following basic evidence on the single GPU node (use the actual runtime node for multi-node tests):

```bash
kubectl -n fsb-gpu-e2e wait --for=condition=Ready pod -l fast-sandbox.io/pool=gpu-test-pool --timeout=180s
kubectl apply -f /tmp/fsb-gpu-fixtures/gpu-sandbox-1.json
kubectl apply -f /tmp/fsb-gpu-fixtures/gpu-sandbox-2.json
kubectl -n fsb-gpu-e2e wait --for=jsonpath='{.status.runtime.state}'=Ready sandbox/gpu-test-1 sandbox/gpu-test-2 --timeout=180s
for name in gpu-test-1 gpu-test-2; do
  uid=$(kubectl -n fsb-gpu-e2e get sandbox "$name" -o jsonpath='{.metadata.uid}')
  sudo cat "/var/log/fast-sandbox/$uid.log"
done
kubectl apply -f /tmp/fsb-gpu-fixtures/gpu-sandbox-3.json
kubectl -n fsb-gpu-e2e get sandboxes -o yaml
```

Poll the log markers for up to 90 seconds after runtime Ready; image/CUDA startup may lag status. Require both `FSB_GPU_PROBE_OK` JSON outputs, correct expected counts, different UUID sets, and agreement with the durable allocations. The overflow object must never acquire a live runtime while both slots remain occupied. Delete `gpu-test-1`, wait for its exact task/container disappearance, and verify `gpu-test-3` then produces the correct CUDA marker. Apply `cpu-pool.json` and `cpu-sandbox.json` separately and require the zero-device marker. For G08/G19/G20 and external-workload topology changes, remove the previous GPU Pool only after its Sandboxes are cleaned; never overlap test configurations that together request more cards than intended. Save full before/after inventory rather than inferring capacity from a Ready condition.

For tests with remote execution, add the existing canonical Execd component from `config/samples/pool-container-execd.yaml` to a fresh GPU Pool before creation and use `fastctl opensandbox exec` with the probe's `--hold-seconds 0`; do not change GPU allocation to make Execd work. This separately qualifies route authentication and the user protocol.

## 7. Acceptance matrix

Status codes: A = existing automation, F = rendered fixture plus explicit hardware procedure, M = manual fault experiment. Every hardware row starts PENDING. A unit test is supporting evidence, not a substitute for its hardware row.

| ID | Gate / operation | Pass condition and evidence | Method |
| --- | --- | --- | --- |
| G01 | Valid P40 host/image | CUDA matmul completes; versions, SM architecture and UUID recorded | A driver + F probe |
| G02 | Repeated direct lifecycle | Ten driver passes; no remaining test task/container/lease | A runner, serial repeats |
| G03 | Kubernetes allocation | GPU requests=limits=`gpus*slots`; PodResources UUIDs match exactly; ordinary GPU Pod passes | F |
| G04 | Two Sandboxes / one two-GPU Fastlet | Apply first two Sandbox fixtures; each CUDA driver enumerates exactly its reserved UUID, sets disjoint, no `all` | F, 2 GPUs |
| G05 | Capacity exhaustion | Apply overflow fixture; no third task/lease. Imperative Create must return capacity failure; declarative CR may stay pending | F; test both API paths |
| G06 | Delete/reallocate | Delete first Sandbox, wait for runtime absence; pending/new Sandbox obtains released GPU and CUDA succeeds | F |
| G07 | Concurrent Create | Burst 20 distinct requests into 2 slots and retry request IDs; at most 2 live runtimes, no duplicated UUID, retries do not leak leases | M + allocator race unit tests |
| G08 | Multi-GPU Sandbox | Fresh `--slots 1 --gpus-per-sandbox 2` Pool; sees exactly 2 reserved UUIDs and executes CUDA on both | F, 2 GPUs |
| G09 | CPU isolation | Apply CPU Pool/Sandbox; no per-card device nodes or usable CUDA GPU, no GPU request/CDI GPU edits | F CPU probe + runtime spec inspection |
| G10 | External workload | With 3 GPUs run external Pod alongside 2-slot Pool; all UUID sets disjoint. With 2 GPUs use 1 slot plus external Pod | F |
| G11 | Same-Pod Fastlet container restart | Record live task PID/UUID/journal, terminate only Fastlet process to restart its container; task survives if platform lifecycle allows, recovered ownership unchanged, never double allocate | M; distinguish from manager reload |
| G12 | Fastlet Pod replacement | Delete exact Fastlet Pod; new Pod UID cannot reuse device while old task remains. Janitor removes orphans, then capacity recovers | M; preserve old/new UID and timelines |
| G13 | Failure before runtime creation | Use a deliberately nonexistent image tag, or an invalid executable in a cached image; prove the failure occurred before a live task. Reservation releases only after runtime absence; valid retry succeeds | F/M |
| G14 | Failure after task creation / before response | Repeatedly interrupt the exact Fastlet during a create until logs/runtime inventory prove this window; if no window observed mark NOT EXERCISED | M; no coverage claim from random restart |
| G15 | Runtime daemon unavailable during cleanup | On dedicated node stop containerd, request deletion, verify lease retained and no GPU reuse; restart daemon and converge cleanup/reuse | M; not a shared-cluster experiment |
| G16 | Missing/stale/conflicting CDI | In maintenance window with no active GPU jobs, move selected spec out, prevent automatic regeneration, start fresh GPU Pool; refusal/no allocation; restore spec/service and recover | M; record/restore both CDI dirs |
| G17 | Invalid plugin identities/sharing | Unit fixtures cover index, duplicate and wrong count. Dedicated-node plugin misconfiguration must fail readiness, never double assign | A units + optional M plugin experiment |
| G18 | Corrupt journal / lease-runtime mismatch | Stop all Fastlet journal writers on the dedicated node, save the exact journal, corrupt one test record; restart refuses unsafe allocation. Restore after proving runtime ownership; never delete production journal as a fix | A units + M |
| G19 | Two Fastlets, disjoint GPUs | Two one-slot Fastlets create/delete concurrently; shared journal retains both owners, different UUIDs, no lost update | M, 2 GPUs + A unit/race |
| G20 | Scale out with available GPU | Increase poolMin/poolMax from 1 to 2 on a one-slot Pool; second GPU Pod becomes Ready, second Sandbox runs | F, 2 GPUs |
| G21 | Scale out without GPU | Request another Fastlet after node GPUs reserved; Pod Pending/insufficient GPU, existing CUDA continues, no fake capacity | F |
| G22 | Drain / scale in | Delete extra Sandbox then shrink Pool using existing drain flow; no unproven reuse; GPU becomes allocatable to external Pod | F |
| G23 | Reset / same-name recreate | Record UID, instance, UUID. Reset cannot double charge; deleted old object/runtime cannot affect replacement; CUDA still works | M fastctl + metadata |
| G24 | Client contract | GetPool/ListPools show fixed GPU count; CPU-only count zero, CPU/memory override never changes GPU cost | M Go/CLI client + API units |
| G25 | Validation / unsupported modes | Negative gpus rejected by schema; unsupported runtime/template GPU override rejected by validation or reported as invalid without serving work. Profile change fenced; CPU ratios do not overcommit GPU | A API/controller units + server dry-run |
| G26 | CPU upgrade regression | GPU dependencies absent on CPU node/Pool; historic CPU profile hashes unchanged; CPU create/delete and existing lifecycle/proxy suites pass | A hash + Linux regression suites |
| G27 | Remote agent execution | Through Execd route execute CUDA probe and a CPU file operation; protocol readiness and actual output both pass | M canonical Execd Pool |
| G28 | Reboot and final cleanup | Only dedicated host: reboot with test ownership recorded; recover or explicitly block unsafe reuse. Delete test resources, confirm no test tasks, GPU processes or leases | M |
| G29 | Optional second node | New GPU node adds real capacity; node loss does not imply preserved CUDA state; replacement cannot access stale identity | M, 2 nodes |

G01-G28 are release-qualification requirements for the supported configuration; a one-GPU rental leaves the multi-device gates explicitly incomplete. WSL cannot substitute for G03-G10 native isolation gates. Unsupported MIG/MPS/time slicing/VM passthrough are negative/scope checks, not features being qualified. Physical isolation means unprivileged Sandbox access is restricted to its assigned device; it does not claim protection from privileged host administrators or host-kernel compromise.

## 8. Fault procedure and observability

Before each fault save test UID, Pool UID, Fastlet Pod UID/node/container restart count, runtime instance/attempt, containerd endpoint/namespace, runtime/task IDs/PIDs, GPU UUIDs, actual OCI device nodes/cgroup rules/mounts, and relevant lease records. Use `/var/lib/fast-sandbox/gpu/leases.json` for the node journal and compare complete identities, not names. Take a backup before edits. Use exact test resources, never `pkill`, `ctr ... rm` over all tasks, blanket namespace cleanup, or `rm -rf /var/lib/fast-sandbox`.

After each failure collect Fastlet current/previous logs, controller and Janitor logs, node events, runtime inspection and journal snapshots. Ensure device claims remain conservative during uncertainty. A task might not be listed as a container; inspect both task and container inventory. After recovery prove the old runtime is absent before accepting a new owner. If journal/containerd endpoints differ, expect refusal/operator reconciliation instead of release.

```bash
sudo env FSB_GPU_K8S_NAMESPACE=fsb-gpu-e2e \
  bash gpu-test-kit/collect.sh /tmp/fsb-gpu-results/cluster-after
kubectl -n fsb-gpu-e2e get sandboxes,sandboxpools,pods -o yaml
```

The collector deliberately does not copy Kubernetes Secrets or private route keys. Store diagnostics privately: workloads and logs may contain application data. It collects test-namespace Fastlet logs; collect controller/Janitor current and previous container logs explicitly for each fault as above. Record detection and convergence times separately. A 180-second no-progress observation is a failure to investigate, not permission to remove fences or delete the journal. Keep warm-create, cold-image preparation, device-plugin wait and cleanup latency separate; record p50/p95/max for 30 serial create/delete cycles without claiming P40 measures modern training throughput.

## 9. Cleanup and result criteria

Delete only test Sandboxes, wait for their exact runtime containers/tasks to disappear, then delete test Pools and external Pod, wait for Fastlets/Janitor to finish, then remove the test namespace. Confirm no lease references the test Sandbox or Pod UIDs and no corresponding CUDA processes remain. Check that external jobs were unaffected and node GPU capacity has returned to baseline. Keep cached images intentionally or remove only exact test image references after all test tasks are absent. Do not run a global image prune. Restore any temporarily changed driver/plugin/CDI/containerd settings and verify them.

A complete PASS requires all applicable required gates with evidence, no unexpected skips, no double allocation, no CPU GPU exposure, and no residual test task/lease after cleanup. Any unsafe reuse or access to an unassigned physical GPU is a hard failure. Any unrun required gate produces PARTIAL, not PASS. Record limitations, reproducible failures, cleanup status and exact source/image identity before ending the rental.

## Primary references

- [NVIDIA CDI generation and refresh](https://docs.nvidia.com/datacenter/cloud-native/container-toolkit/latest/cdi-support.html)
- [NVIDIA device plugin allocation/configuration](https://github.com/NVIDIA/k8s-device-plugin)
- [NVIDIA WSL constraints](https://docs.nvidia.com/cuda/wsl-user-guide/index.html)
- [Official PyTorch previous-version installation commands](https://pytorch.org/get-started/previous-versions/)
- [PyTorch CUDA legacy architecture support](https://dev-discuss.pytorch.org/t/introducing-cuda-13-2-and-deprecating-cuda-12-8-release-2-12/3337)
