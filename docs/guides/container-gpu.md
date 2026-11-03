# NVIDIA GPU Sandboxes with containerd

GPU Pools use `runtime: container` and exclusively allocate whole NVIDIA GPUs. Each Sandbox consumes the fixed `sandboxResources.gpus` count. CPU-only Pools omit this field and retain their existing resource hashes, settings and runtime behavior. GPU time slicing, MPS, MIG, GPU memory quotas, VM passthrough and other runtime profiles are not supported by this implementation.

## Ownership and prerequisites

Kubernetes owns node-to-Fastlet GPU allocation through the NVIDIA device plugin. Fastlet owns the subdivision of its assigned devices among Sandboxes. The runtime uses native CDI injection with the existing runc v2 handler; configuring the CRI plugin alone does not inject devices into FastSandbox's direct containerd client calls.

Install a compatible NVIDIA host driver, NVIDIA Container Toolkit, and NVIDIA Kubernetes device plugin. Configure the plugin with UUID device IDs and whole-GPU exclusive allocation; disable time slicing, MPS and MIG for these Pools. Fastlet rejects non-UUID IDs, duplicate IDs, a different assigned device count, and visible duplicate ownership in kubelet PodResources inventory. This check does not establish a hardware isolation boundary against a privileged host process or a misconfigured external device allocator.

Generate CDI definitions with UUID device names using a supported toolkit, for example:

```bash
sudo nvidia-ctk cdi generate --device-name-strategy=uuid --output=/etc/cdi/nvidia.yaml
nvidia-ctk cdi list
```

Every assigned UUID must resolve as `nvidia.com/gpu=GPU-<uuid>`. Keep CDI definitions current after driver or device changes. The controller mounts `/etc/cdi`, `/var/run/cdi`, a PodResources socket directory derived from the selected runtime environment's kubelet root, and the GPU lease directory into GPU-enabled Fastlets. No cluster API credential is required for device discovery. The privileged Fastlet and its containerd daemon must resolve the CDI host device, driver-library and hook paths consistently; verify this with the actual node driver installation, including GPU Operator driver-container layouts when used.

## Pool configuration

Add the field to an otherwise valid container Pool:

```yaml
spec:
  runtime: container
  maxSandboxesPerPod: 2
  sandboxResources:
    cpu: "1"
    memory: 4Gi
    pids: 512
    gpus: 1
```

The controller requests and limits `nvidia.com/gpu: 2` on the runtime-owner Fastlet container. Do not separately set that resource in the GPU Pool's container templates. Each admitted Sandbox receives one of those devices. Existing slot-based placement and Pool scaling retain an exact fixed GPU cost per slot; a replacement or additional Fastlet must obtain its GPU allocation from Kubernetes before it can serve work. Increasing Pool size cannot manufacture node GPU capacity.

`GetPool` and `ListPools` expose the fixed count as `sandbox_resource_profile.gpus`. A per-Sandbox CPU/memory override in ResourceAware mode does not change its GPU count. GPU resources are exclusive and are not divided by CPU or memory overcommit ratios. Use a separate Pool for CPU-only work or a different GPU count. GPU configuration is part of the immutable resource profile; create a new Pool to change it.

## Lifecycle and recovery

Fastlet persists a device lease before containerd runtime creation. Leases bind the Sandbox UID, Fastlet Pod UID, runtime instance ID, instance generation and assignment attempt. Concurrent creation cannot allocate one device twice. A failed create retains its lease until runtime absence and local cleanup are proven. Runtime device labels and the journal are checked against each other during restart recovery; malformed labels, missing devices and conflicting owners prevent readiness instead of silently assigning a replacement.

A startup reservation without a container is cleaned by exact runtime ID before its device is released. The node-shared journal lives in the node-local host directory `/var/lib/fast-sandbox/gpu`. Each Fastlet holds a process-lifetime lock on its assigned GPU UUIDs; journal updates from Fastlets with disjoint devices are serialized. After the previous Fastlet exits, its persistent leases still block reuse until the replacement verifies that both the old container and task are absent in their recorded containerd namespace. Existing foreign runtimes are left for NodeJanitor, and readiness remains blocked. Leases also record the containerd endpoint; a changed endpoint requires operator reconciliation rather than assuming the previous daemon has no tasks. Do not remove this journal while Sandboxes are running. These node-local fences complement Kubernetes allocation; they do not replace it. Live CUDA memory/process state is not checkpointed or migrated.

Upgrade the control-plane replicas and Fastlet image before creating a GPU Pool. Install the generated CRD before submitting the new field. Existing CPU-only Pools need no GPU dependencies. An older Fastlet cannot match the new GPU resource-profile hash. Do not roll the controller back while GPU Pools exist: drain/delete those Pools before removing the schema or implementation. GPU nodes remain trusted infrastructure; container/runc shares the host kernel.

## Direct driver test on Linux or WSL2

The opt-in driver test launches a real CUDA kernel, checks idempotent creation, reloads the lease manager while the task remains alive, recovers from containerd labels and verifies device reclamation after deletion. It uses a unique containerd namespace and its own runtime IDs. It does not exercise Kubernetes placement, device-plugin assignment, multi-Fastlet exclusivity or a full Fastlet process crash.

Prepare a GPU host with root access, working containerd/runc and UUID-named CDI definitions. The image must contain Python and a CUDA-enabled PyTorch build compatible with the hardware; RTX 5090 requires an appropriately recent CUDA/PyTorch build. Pre-pull the image into containerd's `k8s.io` namespace to keep image download time outside runtime startup validation.

```bash
sudo ctr -n k8s.io images pull docker.io/pytorch/pytorch:2.7.1-cuda12.8-cudnn9-runtime
sudo env FSB_GPU_E2E_DEVICES=GPU-REPLACE_WITH_ASSIGNED_UUID \
  FSB_GPU_E2E_IMAGE=docker.io/pytorch/pytorch:2.7.1-cuda12.8-cudnn9-runtime \
  go test ./internal/runtime/containerd -run '^TestGPUContainerdCUDA$' -v -count=1 -timeout=10m
```

`FSB_GPU_E2E_SOCKET` and `FSB_GPU_E2E_SNAPSHOTTER` select an isolated containerd endpoint or snapshotter. The test requires root and skips unless device UUIDs are explicitly supplied. Do not give it devices currently allocated to unrelated workloads. WSL2 requires GPU-enabled Windows drivers and working NVIDIA CDI support for the WSL device layout. A passing WSL test proves CUDA execution through that runtime configuration; it does not prove physical per-device isolation on `/dev/dxg` or Linux Kubernetes GPU allocation.

Before production acceptance, validate a real Kubernetes GPU Pool: two Sandboxes on a two-GPU Fastlet must see different assigned devices, a third must be rejected without leakage, CPU-only Sandboxes must not gain GPU access, deletion must restore capacity, and an external Kubernetes GPU Pod must retain its independently assigned card. Repeat with Fastlet restart, failed Create cleanup and unavailable CDI definitions. These hardware gates are required separately from unit tests and the standalone driver test.

## Qualification tools

See [GPU qualification plan](../../test/gpu/PLAN.md) for the hardware acceptance matrix, host preflight, CUDA probe, Kubernetes fixtures, and result template. Build a portable Linux/amd64 driver test bundle with `bash test/gpu/build-kit.sh /tmp/fsb-gpu-delivery`. Hardware gates remain separate from unit tests and must be recorded explicitly.
