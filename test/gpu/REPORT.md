# GPU qualification report

Overall result: NOT RUN (change to PASS / FAIL / PARTIAL only with evidence).

- Date / operator:
- Source base commit, branch, dirty-source fingerprint (`source.json`):
- Test-bundle checksums:
- Host/cloud, OS/kernel, vCPU/RAM/free disk, native Linux / WSL / container:
- GPUs: model, UUIDs, count, compute capability:
- Driver / NVIDIA toolkit / CDI generation strategy and spec hashes:
- containerd / runc / snapshotter / endpoint / namespace:
- Kubernetes / kubelet root / PodResources socket / device-plugin version and effective config:
- Component image references and digests:
- CUDA image digest / PyTorch / CUDA / supported architecture list:
- Test namespace and external workloads/baseline:

| Gate | PASS / FAIL / PENDING / NOT APPLICABLE | Exact command, evidence path, observations |
| --- | --- | --- |
| G01 | PENDING | |
| G02 | PENDING | |
| G03 | PENDING | |
| G04 | PENDING | |
| G05 | PENDING | |
| G06 | PENDING | |
| G07 | PENDING | |
| G08 | PENDING | |
| G09 | PENDING | |
| G10 | PENDING | |
| G11 | PENDING | |
| G12 | PENDING | |
| G13 | PENDING | |
| G14 | PENDING | |
| G15 | PENDING | |
| G16 | PENDING | |
| G17 | PENDING | |
| G18 | PENDING | |
| G19 | PENDING | |
| G20 | PENDING | |
| G21 | PENDING | |
| G22 | PENDING | |
| G23 | PENDING | |
| G24 | PENDING | |
| G25 | PENDING | |
| G26 | PENDING | |
| G27 | PENDING | |
| G28 | PENDING | |
| G29 | PENDING | |

## Local and Linux regression checks

| Check | Result / exit status / evidence |
| --- | --- |
| Affected Go packages | PENDING |
| Race tests | PENDING |
| Python SDK | PENDING |
| Full Linux unit tests | PENDING |
| Generation / lint / verify | PENDING |
| Existing CPU lifecycle / proxy suites | PENDING |

## Performance observations

Image preparation time, warm create p50/p95/max, device-plugin wait, deletion/reuse convergence, and sample count:

## Failures and limitations

Reproduction, expected/actual behavior, error logs and any baseline reproduction:

## Cleanup

Exact test UIDs removed, remaining namespaces/containers/tasks/GPU processes/leases, intentionally retained images, and restored fault settings:

## Decision

Required gates still pending, risks, and follow-up changes. Never turn a SKIP or an unobserved fault window into PASS.
