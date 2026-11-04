# Native dual-V100S qualification, November 4, 2026

The hardware campaign exercised all 28 required single-node gates, including the supplied PyTorch image. It is not an unconditional release PASS: fractional reset timestamps fail G23. G29 requires an optional second physical node, which was not supplied. Individual gates combine the initial cluster campaign and supplemental runs; this was not one uninterrupted green suite.

## Environment and identities

The dedicated host used Ubuntu 22.04.4, Linux 5.15.0-113/amd64, cgroup v2, two Tesla V100S-PCIE-32GB GPUs (sm_70), NVIDIA driver 550.127.08, NVIDIA Container Toolkit 1.16.2, and UUID CDI schema 0.8.0. Direct tests used host containerd 1.7.22/runc 1.1.14. Kubernetes tests used native K3s v1.35.0+k3s1/containerd 2.1.5-k3s1 and NVIDIA device plugin v0.17.1 with UUID identities and exclusive whole GPUs.

Production binaries include the FIFO-directory fix in `19a26e4`; the initial published test baseline is `d9af76e`. The corrected Fastlet image manifest is `sha256:de423a24af9f0077f50a4b7ee2ce3b2112991bd1b3eb57f37525f4abe0d873e5`. Supplemental changes add test tooling and an opt-in image-namespace reuse mode, without another production runtime change.

The Python image manifest is `sha256:061c190f3d5815b341a18dfe8a45771f8ba68c15a607ea56997fde3009ac4eec`. The user-supplied PyTorch image is `swr.cn-north-4.myhuaweicloud.com/ddn-k8s/docker.io/pytorch/pytorch:2.4.0-cuda12.4-cudnn9-devel`, manifest `sha256:e96c6896ecfbb50d89c87bf94110206ef444f27268c5f72201eb29fba9c90331`.

## Matrix

| Gate | Result | Actual execution and evidence artifact |
| --- | --- | --- |
| G01 | PASS | Both V100S UUIDs execute PTX matrix multiplication; supplied PyTorch image also executes CUDA. `dual-ten-rounds`, `fsb-gpu-pytorch-test-02.log`. |
| G02 | PASS | Ten real dual-driver rounds, 229 seconds; exact task/container/lease cleanup checked. `dual-ten-rounds`. |
| G03 | PASS | Kubernetes requests/limits reserve exactly two cards; PodResources and real CUDA UUIDs agree. Initial `cluster-02`, `cluster-03`. |
| G04 | PASS | Two simultaneous one-GPU Sandboxes see disjoint exact UUID sets and execute kernels. Initial cluster reports and direct rounds. |
| G05 | PASS | Declarative overflow never obtains another task/lease; imperative overflow returns ResourceExhausted. `matrix-api`, final strengthened rerun `matrix-api-final`. |
| G06 | PASS | Delete, verify absence, reallocate, execute CUDA. Initial cluster campaign and direct rounds. |
| G07 | PASS | Twenty real concurrent gRPC requests admit exactly two; repeated request IDs preserve UID and overflow is rejected. `matrix-api`, final strengthened rerun `matrix-api-final`. |
| G08 | PASS | A native Kubernetes Sandbox reserves both cards, sees exactly two UUIDs, and computes on both. `matrix-multi`. |
| G09 | PASS | CPU Sandbox sees zero GPU devices and has no GPU allocation. Initial cluster reports and direct rounds. |
| G10 | PASS | Independent ordinary GPU Pod executes concurrently with a one-GPU Fastlet; UUIDs remain disjoint. Initial `cluster-03`. |
| G11 | PASS | Host pidfd SIGKILL restarts only Fastlet; existing Sandbox task PIDs/UUID ownership survive and fresh kernels run. Initial `cluster-03`. |
| G12 | PASS | Replacement Pod cannot reuse old ownership; Janitor proves old task/container absence before capacity returns. Initial `cluster-03`. |
| G13 | PASS | Invalid executable fails creation; exact cleanup precedes a successful CUDA retry. `matrix-api`, final strengthened rerun `matrix-api-final`. |
| G14 | PASS | Real task exists; temporary HTTP proxy withholds its create acknowledgement while exact Fastlet is killed. Replay retains one UID/lease. `matrix-window-02`. |
| G15 | PASS | Suspend actual containerd socket server, request deletion, verify unchanged leases/no reuse, resume, clean and reallocate. `matrix-outage-02`. |
| G16 | PASS | Fresh GPU Fastlets refuse missing, stale and conflicting CDI; restored configuration recovers. `matrix-maintenance`. |
| G17 | PASS | Real index-identity plugin misconfiguration never serves GPU work; duplicate/count/sharing identities also have negative unit coverage. `matrix-maintenance`, full Linux unit log. |
| G18 | PASS | Stopped writer, deliberately mismatched journal/runtime identity, refusal of readiness while original task remains, exact journal restoration, fresh CUDA. `matrix-maintenance`. |
| G19 | PASS | Two actual Fastlet Pods create/delete concurrently with disjoint UUIDs and no lost shared-journal records. `matrix-multi`. |
| G20 | PASS | Pool scale-out adds a second real GPU Fastlet and CUDA workload. Initial `cluster-03`. |
| G21 | PASS | Third Fastlet stays Pending with explicit insufficient-GPU scheduler evidence; existing capacity is unchanged. Initial `cluster-03`. |
| G22 | PASS | Scale-in returns GPU to an independent Kubernetes CUDA Pod. Initial `cluster-03`. |
| G23 | FAIL | Second-resolution reset and old-UID fencing pass, but fractional reset tokens livelock. `matrix-api`, `matrix-api-tail`; see finding below. |
| G24 | PASS | Public GetPool/ListPools report GPU 1 and CPU 0. CPU/memory overrides and overcommit ratios preserve fixed exclusive GPU cost. `matrix-api-final`, `matrix-api-final`, `matrix-api-rest-02`. |
| G25 | PASS | Negative GPU schema validation, unsupported runtime/template override rejection, and new GPU resource requirements on profile changes. `matrix-api-final`, `matrix-api-rest-02`. |
| G26 | PASS, scoped | Fresh CPU Pool/Sandbox executes with both CDI directories empty; complete Linux unit suite passes. This used a GPU-capable host, not a separate physical CPU-only node. `matrix-api-rest-02`, `fsb-linux-unit-04.log`. |
| G27 | PASS | Official pinned Execd artifact executes CUDA and writes/reads a file through the authenticated Sandbox proxy. `matrix-execd`. |
| G28 | PASS safety, assisted cleanup | Boot ID changed; Pool readiness blocked while original lease remained. A terminal admission-failed Fastlet Pod required operator replacement before normal deletion/Janitor cleanup converged. `matrix-reboot-check`. |
| G29 | NOT APPLICABLE | Optional second-node qualification requires a second real node. No cross-node recovery claim. |

G14 observes acknowledgement loss after the real Fastlet finishes creating its task; it does not claim to intercept an earlier internal pre-commit window. G15 uses SIGSTOP/SIGCONT to make the real daemon unavailable while preserving the API server and all workload processes. G17's hardware negative test uses index identities; unsupported sharing variants were checked in unit fixtures rather than enabling hardware time slicing.

## PyTorch and timing

The PyTorch mirror was fully pulled and unpacked: approximately 7.43 GiB compressed, 324 seconds downloading plus 113 seconds unpacking. The first isolated-namespace preparation attempt timed out while exporting/unpacking another full copy; it did not create a GPU runtime. The opt-in shared source namespace then ran `TestGPUContainerdCUDA` successfully in 3.14 seconds, including real PyTorch CUDA matrix multiplication, idempotent Ensure, manager/journal reload and recovery, deletion, and lease reclamation. These are warm functional timings, not model-training throughput results.

Thirty serial native Kubernetes lifecycle cycles completed in 98.98 seconds. Create-to-observed-CUDA latency was p50 1.616 seconds, p95 1.674 seconds, maximum 1.679 seconds. Delete-to-confirmed-runtime/lease-absence latency was p50 1.492 seconds, p95 1.539 seconds, maximum 1.542 seconds. These measurements include kubectl/process/polling overhead and are not raw containerd startup latency. Samples are retained in `matrix-latency/latency-samples.json`.

## Fractional reset-token failure

A raw API patch setting `spec.resetRevision` to `2026-11-04T12:57:50.536117+00:00` retained fractional seconds in the desired object, while controller status persisted `acceptedResetRevision` as `2026-11-04T12:57:50Z`. The controller repeatedly considered the request newer than its acknowledgement, leaving the Sandbox Pending and clearing placement instead of converging. GPU leases remained empty during this loop; no duplicate allocation was observed. Replacing the token with a whole-second timestamp restored execution. Reset and delete/same-name recreation then passed, including rejection of the previous UID through the public API.

The affected `resetPending`/`reconcileReset` lifecycle implementation is unchanged from baseline `4fe01ba`; this campaign found the issue but does not fix it as part of GPU support. Reproduce on a fresh test namespace with `matrix-extension.py --phase api --reset-precision fractional` and the required fixture/client/output arguments. Keep this failure distinct from the successful second-resolution run.

## Reboot recovery limitation

The node rebooted and the K3s service restarted automatically. During boot the NVIDIA plugin was not yet ready when kubelet attempted to admit the existing GPU Fastlet Pod; kubelet marked it Failed/UnexpectedAdmissionError with no healthy GPU devices. FastSandbox projected the Sandbox as Unavailable, retained its exact pre-reboot lease, and did not serve or reuse that GPU. The reboot safety gate passed through its explicitly blocked recovery path, not automatic CUDA task resumption.

Normal Sandbox deletion initially waited on that failed Fastlet Pod. After saving the Pod, Sandbox and event evidence, the operator deleted only the exact terminal test Pod. Normal Janitor/deletion processing then removed the old runtime ownership and all test resources. This exposes a recovery limitation: the current Pool controller does not automatically replace that terminal admission-failed Pod. No lease journal was deleted or edited to force reboot cleanup. Do not describe this result as unattended reboot recovery.

## Validation and setup failures

The complete Linux/amd64 `make test SCOPE=unit` passed with Go 1.25.5. Early attempts failed because the transferred offline module cache omitted dependencies; jq and missing archives were supplied before the successful full run. Affected runtime and GPU client packages also passed local tests, Linux-target lint, Python runner regression tests, Python static checks, and shell syntax checks.

Retained failed attempts include the fractional reset, an incorrect test assumption that GPU profiles were immutable, a cross-filesystem rename in the CDI backup fixture, initial multicall-daemon PID detection, and a temporary Service that had not yet become connectable. The latter fixture/setup failures were corrected and their successful reruns are listed above. They must not be presented as production GPU failures or hidden as one clean passing run.

Detailed private evidence is retained locally under `.dev/gpu-v100-2026-11-04/`, including the original campaign, the supplemental reports/command traces, framework/driver/unit logs, and the pre-reboot archive copied off the rented host. No credentials or Kubernetes Secret contents are included.

## Final cleanup

Final inspection found no Sandbox or SandboxPool CRs, no test Fastlet Pods, no managed Sandbox containers on either runtime, an empty GPU lease journal, no CUDA compute processes, and no remaining isolated direct-test containerd namespaces. Device-plugin UUID configuration and CDI were restored. The failed large-image preparation's exact extraction lease was removed after verifying no task/container/image references; normal GC removed its snapshots before the empty namespace was removed. Shared source images were preserved.

K3s/system deployments, the device plugin, toolkit, CDI definitions and image caches remain installed on the disposable host; they hold no Sandbox GPU leases. Both pre-reboot and post-reboot evidence archives were copied to the local workspace before ending the rental dependency. The host can be released.
