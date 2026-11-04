#!/usr/bin/env python3
"""Additional real GPU fault/API qualification on the dedicated native node."""

import argparse
import ast
import copy
import datetime
import json
import os
import shutil
import signal
import subprocess
import time
from pathlib import Path

p = argparse.ArgumentParser()
p.add_argument("--output", type=Path, required=True)
p.add_argument(
    "--phase",
    choices=[
        "api",
        "api-tail",
        "maintenance",
        "outage",
        "execd",
        "window",
        "latency",
        "api-rest",
        "reboot-prepare",
        "reboot-check",
        "multi",
    ],
    required=True,
)
p.add_argument("--fixtures", type=Path, required=True)
p.add_argument("--namespace", default="fsb-gpu-complete")
p.add_argument(
    "--reset-precision", choices=["seconds", "fractional"], default="seconds"
)
p.add_argument("--client", default=str(Path(__file__).with_name("gpu-matrix-client")))
p.add_argument("--fastctl", default="/tmp/fastctl")
a = p.parse_args()
a.output.mkdir(parents=True, exist_ok=True)
kit = Path(__file__).parent
scope = {"__file__": str(kit / "cluster-smoke.py")}
tree = ast.parse((kit / "cluster-smoke.py").read_text())
exec(
    compile(
        ast.Module(
            body=[
                x
                for x in tree.body
                if isinstance(x, (ast.Import, ast.ImportFrom, ast.FunctionDef))
            ],
            type_ignores=[],
        ),
        "cluster-helpers",
        "exec",
    ),
    scope,
)
ns = a.namespace
assert ns.startswith("fsb-gpu-"), "Use a dedicated GPU test namespace"
assert os.geteuid() == 0, "Fault qualification requires root on the dedicated test node"
settings = argparse.Namespace(output=a.output, timeout=120, budget_seconds=1800)
report = {"namespace": ns, "gates": [], "result": "RUNNING", "phase": a.phase}
scope.update(a=settings, ns=ns, start=time.monotonic(), report=report, uids=set())
k, get, wait, probe, delete_sb, wait_pool, gate, journal, task_pids = [
    scope[x]
    for x in [
        "k",
        "get",
        "wait",
        "sandbox_probe",
        "delete_sandbox",
        "wait_pool",
        "gate",
        "journal",
        "task_pids",
    ]
]
socket = os.environ.get("FSB_GPU_E2E_SOCKET", "/run/k3s/containerd/containerd.sock")
os.environ.setdefault("FSB_GPU_E2E_SOCKET", socket)
client = str(Path(a.client).resolve())
fastctl = str(Path(a.fastctl).resolve())
services = json.loads(
    subprocess.check_output(
        [
            "kubectl",
            "-n",
            "fast-sandbox-system",
            "get",
            "service",
            "fast-sandbox-fastpath",
            "fast-sandbox-proxy",
            "-o",
            "json",
        ]
    )
)["items"]
addresses = {x["metadata"]["name"]: x["spec"]["clusterIP"] for x in services}
endpoint = addresses["fast-sandbox-fastpath"] + ":9090"
proxy_endpoint = "http://" + addresses["fast-sandbox-proxy"] + ":8080"
base = json.loads((a.fixtures / "gpu-pool.json").read_text())
base["metadata"]["namespace"] = ns
base["metadata"]["name"] = "gpu-matrix-pool"
base["spec"]["maxSandboxesPerPod"] = 2
probe_source = (kit / "cuda-driver-probe.py").read_text()


def apply(obj):
    return k("apply", "-f", "-", obj=obj)


def pool(name="gpu-matrix-pool", slots=2, gpu=1, aware=False):
    x = copy.deepcopy(base)
    x["metadata"]["name"] = name
    x["spec"]["maxSandboxesPerPod"] = slots
    if gpu:
        x["spec"]["sandboxResources"]["gpus"] = gpu
    else:
        x["spec"]["sandboxResources"].pop("gpus", None)
    if aware:
        x["spec"]["sandboxResources"].update(
            mode="ResourceAware", overcommitRatio={"cpu": "2", "memory": "2"}
        )
    return x


def sb(name, poolname="gpu-matrix-pool", count=1, command=None, resources=None):
    x = {
        "apiVersion": "sandbox.fast.io/v1alpha2",
        "kind": "Sandbox",
        "metadata": {"namespace": ns, "name": name, "labels": {"fsb-gpu-test": "true"}},
        "spec": {
            "poolRef": poolname,
            "image": "docker.io/library/python:3.11-slim-bookworm",
            "command": command
            or [
                "python",
                "-u",
                "-c",
                probe_source,
                "--expect-count",
                str(count),
                "--hold-seconds",
                "1800",
            ],
        },
    }
    if resources:
        x["spec"]["resources"] = {"limits": resources}
    return x


def external(args, timeout=120, allow=False):
    r = subprocess.run(args, text=True, capture_output=True, timeout=timeout)
    with (a.output / "external.log").open("a") as f:
        f.write(
            json.dumps(
                {
                    "args": args,
                    "rc": r.returncode,
                    "stdout": r.stdout,
                    "stderr": r.stderr,
                }
            )
            + "\n"
        )
    if r.returncode and not allow:
        raise RuntimeError(r.stderr[-1500:] or r.stdout[-1500:])
    return r


def api(mode, **kwargs):
    args = [client, "-endpoint", endpoint, "-namespace", ns, "-mode", mode]
    for key, value in kwargs.items():
        args.extend(["-" + key, str(value)])
    r = external(args)
    x = json.loads(r.stdout)
    assert x["status"] == "PASS", x
    return x


def all_sandboxes_gone():
    for obj in get("sandboxes")["items"]:
        delete_sb(obj["metadata"]["name"])
    assert not [x for x in journal().values() if x["owner"]["namespace"] == ns]


def remove_pool(name="gpu-matrix-pool"):
    k("delete", "sandboxpool", name, "--ignore-not-found", "--wait=false")
    wait(
        "Pool removal " + name,
        lambda: (
            not [
                x
                for x in get("pods")["items"]
                if x["metadata"].get("labels", {}).get("fast-sandbox.io/pool") == name
            ]
        ),
    )


def stop_pod(pod):
    scope["kill_fastlet_process"](pod)


def gpu_pod():
    return wait_pool("gpu-matrix-pool", 1)[0]


def fail_closed(label):
    pod = wait("GPU Pod exists", lambda: scope["pool_pods"]("gpu-matrix-pool"))[0]
    for _ in range(10):
        pods = scope["pool_pods"]("gpu-matrix-pool")
        assert not any(scope["ready"](x) for x in pods), label
        assert not [x for x in journal().values() if x["owner"]["namespace"] == ns], (
            label
        )
        time.sleep(1)
    logs = k("logs", pod["metadata"]["name"], "-c", "fastlet", "--tail=30", allow=True)
    (a.output / (label + ".log")).write_text(logs.stdout + logs.stderr)
    return pod


try:
    nodes = json.loads(external(["kubectl", "get", "nodes", "-o", "json"]).stdout)[
        "items"
    ]
    assert (
        len(nodes) == 1
        and int(nodes[0]["status"]["allocatable"].get("nvidia.com/gpu", 0)) == 2
    ), "A dedicated two-GPU node is required"
    all_pods = json.loads(
        external(["kubectl", "get", "pods", "-A", "-o", "json"]).stdout
    )["items"]
    for item in all_pods:
        if item["metadata"]["namespace"] != ns and item.get("status", {}).get(
            "phase"
        ) not in ["Succeeded", "Failed"]:
            assert not any(
                int(x.get("resources", {}).get("requests", {}).get("nvidia.com/gpu", 0))
                for x in item["spec"]["containers"]
            ), "An unrelated GPU workload is present"
    assert all(x["owner"]["namespace"] == ns for x in journal().values()), (
        "An unrelated GPU lease is present"
    )
    existing = k("get", "namespace", ns, "-o", "json", allow=True)
    if existing.returncode == 0:
        assert (
            json.loads(existing.stdout)["metadata"]
            .get("labels", {})
            .get("fsb-gpu-test")
            == "true"
        ), "Refuse an unrelated namespace"
    if existing.returncode:
        apply(
            {
                "apiVersion": "v1",
                "kind": "Namespace",
                "metadata": {
                    "name": ns,
                    "labels": {
                        "fsb-gpu-test": "true",
                        "pod-security.kubernetes.io/enforce": "privileged",
                    },
                },
            }
        )
    if a.phase in ["api", "api-tail", "api-rest"]:
        if a.phase == "api":
            apply(pool())
            wait_pool("gpu-matrix-pool", 1)
            apply(pool("cpu-test-pool", slots=1, gpu=0))
            wait_pool("cpu-test-pool", 1)
            gate("G24 GetPool/ListPools public GPU counts", api("contracts"))
            burst = api("burst")
            admitted = [x for x in burst["requests"] if x.get("uid")]
            observed = [probe(x["name"], 1)[1] for x in admitted]
            assert len(journal()) == 2 and set(observed[0]["uuids"]).isdisjoint(
                observed[1]["uuids"]
            )
            gate(
                "G05/G07 20 real gRPC creates, exact 2 admits, idempotent retry and overflow rejection",
                burst,
            )
            all_sandboxes_gone()
            apply(sb("bad-executable", command=["/fsb-nonexistent-executable"]))
            wait(
                "failed create observed",
                lambda: (
                    get("sandbox", "bad-executable")
                    .get("status", {})
                    .get("runtime", {})
                    .get("message")
                ),
            )
            state = get("sandbox", "bad-executable")
            assert state["status"]["runtime"]["state"] != "Ready"
            delete_sb("bad-executable")
            apply(sb("valid-after-failure"))
            probe("valid-after-failure", 1)
            gate("G13 failed task creation cleaned before valid CUDA retry")
            original = get("sandbox", "valid-after-failure")
            oldgen = original["status"]["runtime"]["generation"]
            stamp = (
                datetime.datetime.now(datetime.timezone.utc)
                if a.reset_precision == "fractional"
                else datetime.datetime.now(datetime.timezone.utc).replace(microsecond=0)
            ).isoformat()
            k(
                "patch",
                "sandbox",
                "valid-after-failure",
                "--type=merge",
                "-p",
                json.dumps({"spec": {"resetRevision": stamp}}),
            )
            wait(
                "reset generation",
                lambda: (
                    get("sandbox", "valid-after-failure")["status"]["runtime"].get(
                        "generation", 0
                    )
                    > oldgen
                ),
            )
            reset, _ = probe("valid-after-failure", 1, fresh=True)
            assert reset["metadata"]["uid"] == original["metadata"]["uid"]
            assert (
                len(
                    [
                        x
                        for x in journal().values()
                        if x["owner"]["sandboxUid"] == reset["metadata"]["uid"]
                    ]
                )
                == 1
            )
        elif a.phase == "api-tail":
            stamp = (
                datetime.datetime.now(datetime.timezone.utc)
                .replace(microsecond=0)
                .isoformat()
            )
            k(
                "patch",
                "sandbox",
                "valid-after-failure",
                "--type=merge",
                "-p",
                json.dumps({"spec": {"resetRevision": stamp}}),
            )
            reset, _ = probe("valid-after-failure", 1, fresh=True)
            gate(
                "G23 canonical second-resolution reset recovered CUDA; fractional-token livelock retained as separate failed finding"
            )
        if a.phase != "api-rest":
            olduid = reset["metadata"]["uid"]
            delete_sb("valid-after-failure")
            apply(sb("valid-after-failure"))
            probe("valid-after-failure", 1)
            time.sleep(2)
            gate(
                "G23 reset and same-name recreation fence old UID",
                api("stale-uid", name="valid-after-failure", **{"old-uid": olduid}),
            )
        all_sandboxes_gone()
        if a.phase == "api-rest":
            remove_pool()
            apply(pool())
            wait_pool("gpu-matrix-pool", 1)
        for tag, change, expect_server_reject in [
            ("negative", lambda x: x["spec"]["sandboxResources"].update(gpus=-1), True),
            ("wrong-runtime", lambda x: x["spec"].update(runtime="gvisor"), False),
            (
                "template-override",
                lambda x: x["spec"]["fastletTemplate"]["spec"]["containers"][0].update(
                    resources={"limits": {"nvidia.com/gpu": "2"}}
                ),
                False,
            ),
        ]:
            obj = pool("invalid-" + tag)
            change(obj)
            r = k("apply", "-f", "-", obj=obj, allow=True)
            if expect_server_reject:
                assert r.returncode != 0
            elif r.returncode == 0:
                wait(
                    "invalid resource condition",
                    lambda: (
                        get("sandboxpool", obj["metadata"]["name"])
                        .get("status", {})
                        .get("conditions")
                    ),
                )
                assert not scope["pool_pods"](obj["metadata"]["name"])
                remove_pool(obj["metadata"]["name"])
        mutation = k(
            "patch",
            "sandboxpool",
            "gpu-matrix-pool",
            "--type=merge",
            "-p",
            json.dumps({"spec": {"sandboxResources": {"gpus": 2}}}),
            allow=True,
        )
        if mutation.returncode == 0:
            pods = wait(
                "new profile replacement Pod",
                lambda: [
                    x
                    for x in scope["pool_pods"]("gpu-matrix-pool")
                    if x["spec"]["containers"][0]
                    .get("resources", {})
                    .get("limits", {})
                    .get("nvidia.com/gpu")
                    == "4"
                ],
            )
            assert not any(scope["ready"](x) for x in pods), (
                "Four-GPU replacement served on a two-GPU node"
            )
            assert not journal(), "Profile update fabricated GPU leases"
        if mutation.returncode == 0:
            api("one", name="profile-fenced", **{"expect-error": "true"})
            delete_sb("profile-fenced")
            assert not journal()
        gate(
            "G25 invalid configurations refused; profile change requires a new GPU-resourced Pod"
        )
        remove_pool()
        apply(pool(aware=True))
        wait_pool("gpu-matrix-pool", 1)
        for i in [1, 2]:
            apply(
                sb("override-" + str(i), resources={"cpu": "500m", "memory": "512Mi"})
            )
            probe("override-" + str(i), 1)
        assert len(journal()) == 2
        gate(
            "G24 CPU/memory overrides and overcommit ratios preserve exclusive one-GPU cost",
            api("one", name="override-overflow", **{"expect-error": "true"}),
        )
        all_sandboxes_gone()
        remove_pool()
        # A CPU-only Fastlet starts and executes with both node CDI directories empty.
        remove_pool("cpu-test-pool")
        backup = Path("/tmp/fsb-cdi-cpu-backup")
        backup.mkdir(exist_ok=True)
        moved = []
        try:
            for directory in ["/etc/cdi", "/var/run/cdi"]:
                for f in Path(directory).glob("*"):
                    if f.is_file():
                        target = backup / (str(len(moved)) + "-" + f.name)
                        shutil.move(str(f), str(target))
                        moved.append((f, target))
            apply(pool("cpu-test-pool", slots=1, gpu=0))
            wait_pool("cpu-test-pool", 1)
            apply(sb("cpu-without-cdi", poolname="cpu-test-pool", count=0))
            probe("cpu-without-cdi", 0)
            gate("G26 CPU Sandbox executes without any node CDI specifications")
            delete_sb("cpu-without-cdi")
        finally:
            for original, saved in moved:
                shutil.move(str(saved), str(original))
        remove_pool("cpu-test-pool")
    elif a.phase == "maintenance":
        all_sandboxes_gone()
        remove_pool()
        original = Path("/etc/cdi/nvidia.yaml")
        saved = original.read_bytes()
        for mode in ["missing", "stale", "conflicting"]:
            duplicate = Path("/etc/cdi/fsb-duplicate.yaml")
            try:
                if mode == "missing":
                    original.rename(original.with_suffix(".backup"))
                elif mode == "stale":
                    original.write_bytes(saved.replace(b"GPU-", b"GPU-stale-", 2))
                else:
                    duplicate.write_bytes(saved)
                apply(pool())
                fail_closed("G16-" + mode)
                remove_pool()
            finally:
                if original.with_suffix(".backup").exists():
                    original.with_suffix(".backup").rename(original)
                original.write_bytes(saved)
                duplicate.unlink(missing_ok=True)
        apply(pool())
        wait_pool("gpu-matrix-pool", 1)
        gate("G16 missing/stale/conflicting CDI fails closed and restored CDI recovers")
        remove_pool()
        plugin = json.loads(
            external(
                [
                    "/usr/local/bin/k3s",
                    "kubectl",
                    "-n",
                    "kube-system",
                    "get",
                    "ds",
                    "fsb-gpu-device-plugin",
                    "-o",
                    "json",
                ]
            ).stdout
        )
        original_spec = copy.deepcopy(plugin["spec"])
        try:
            changed = copy.deepcopy(original_spec)
            args = changed["template"]["spec"]["containers"][0]["args"]
            args[:] = [
                x.replace("--device-id-strategy=uuid", "--device-id-strategy=index")
                for x in args
            ]
            external(
                [
                    "/usr/local/bin/k3s",
                    "kubectl",
                    "-n",
                    "kube-system",
                    "patch",
                    "ds",
                    "fsb-gpu-device-plugin",
                    "--type=json",
                    "-p",
                    json.dumps([{"op": "replace", "path": "/spec", "value": changed}]),
                ]
            )
            external(
                [
                    "/usr/local/bin/k3s",
                    "kubectl",
                    "-n",
                    "kube-system",
                    "rollout",
                    "status",
                    "ds/fsb-gpu-device-plugin",
                    "--timeout=90s",
                ]
            )
            apply(pool())
            fail_closed("G17-index")
            remove_pool()
            gate(
                "G17 real index identity misconfiguration never becomes a serving GPU Fastlet"
            )
        finally:
            external(
                [
                    "/usr/local/bin/k3s",
                    "kubectl",
                    "-n",
                    "kube-system",
                    "patch",
                    "ds",
                    "fsb-gpu-device-plugin",
                    "--type=json",
                    "-p",
                    json.dumps(
                        [{"op": "replace", "path": "/spec", "value": original_spec}]
                    ),
                ]
            )
            external(
                [
                    "/usr/local/bin/k3s",
                    "kubectl",
                    "-n",
                    "kube-system",
                    "rollout",
                    "status",
                    "ds/fsb-gpu-device-plugin",
                    "--timeout=90s",
                ]
            )
        apply(pool())
        wait_pool("gpu-matrix-pool", 1)
        apply(sb("journal-owner"))
        item, _ = probe("journal-owner", 1)
        uid = item["metadata"]["uid"]
        path = Path("/var/lib/fast-sandbox/gpu/leases.json")
        backup = path.read_bytes()
        pod = gpu_pod()
        status = next(
            x for x in pod["status"]["containerStatuses"] if x["name"] == "fastlet"
        )
        pid = int(task_pids()[status["containerID"].removeprefix("containerd://")])
        fd = os.pidfd_open(pid)
        try:
            signal.pidfd_send_signal(fd, signal.SIGSTOP)
            broken = json.loads(backup)
            broken["leases"][uid]["owner"]["runtimeInstanceId"] = (
                "fsb-corrupted-runtime"
            )
            path.write_text(json.dumps(broken))
            signal.pidfd_send_signal(fd, signal.SIGKILL)
        finally:
            os.close(fd)
        wait(
            "corrupt journal refused",
            lambda: any(
                x.get("restartCount", 0) > status["restartCount"]
                for x in get("pod", pod["metadata"]["name"])["status"].get(
                    "containerStatuses", []
                )
                if x["name"] == "fastlet"
            ),
        )
        time.sleep(4)
        assert not scope["ready"](get("pod", pod["metadata"]["name"]))
        assert uid in task_pids(), (
            "Original CUDA task must remain present while journal mismatch blocks readiness"
        )
        current = get("pod", pod["metadata"]["name"])
        current_status = next(
            x for x in current["status"]["containerStatuses"] if x["name"] == "fastlet"
        )
        current_pid = int(
            task_pids()[current_status["containerID"].removeprefix("containerd://")]
        )
        current_fd = os.pidfd_open(current_pid)
        try:
            signal.pidfd_send_signal(current_fd, signal.SIGSTOP)
            path.write_bytes(backup)
            signal.pidfd_send_signal(current_fd, signal.SIGKILL)
        finally:
            os.close(current_fd)
        wait_pool("gpu-matrix-pool", 1)
        probe("journal-owner", 1, fresh=True)
        gate(
            "G18 real journal/runtime identity mismatch blocks readiness; exact journal restore recovers existing CUDA task"
        )
        all_sandboxes_gone()
        remove_pool()
    elif a.phase == "outage":
        apply(pool())
        wait_pool("gpu-matrix-pool", 1)
        jobs = []
        for i in [1, 2]:
            apply(sb("outage-" + str(i)))
            jobs.append(probe("outage-" + str(i), 1)[0])
        import socket as unix_socket
        import struct

        # K3s uses a multicall binary and rewrites argv, so identify the actual
        # socket server through Linux peer credentials rather than executable names.
        with unix_socket.socket(unix_socket.AF_UNIX) as connection:
            connection.connect(socket)
            pid, owner_uid, _ = struct.unpack(
                "3i",
                connection.getsockopt(
                    unix_socket.SOL_SOCKET, unix_socket.SO_PEERCRED, 12
                ),
            )
        command = Path(f"/proc/{pid}/cmdline").read_bytes().split(b"\0")[0].strip()
        assert owner_uid == 0 and (
            command == b"containerd" or command.endswith(b"/containerd")
        ), (pid, command)
        fd = os.pidfd_open(pid)
        old = copy.deepcopy(journal())
        try:
            signal.pidfd_send_signal(fd, signal.SIGSTOP)
            k("delete", "sandbox", "outage-1", "--wait=false")
            time.sleep(3)
            assert journal() == old, (
                "Lease changed while runtime absence could not be established"
            )
            rejected = api("one", name="outage-overflow", **{"expect-error": "true"})
            assert journal() == old, "GPU was reused while runtime was unavailable"
        finally:
            signal.pidfd_send_signal(fd, signal.SIGCONT)
            os.close(fd)
        delete_sb("outage-overflow")
        delete_sb("outage-1")
        probe("outage-2", 1, fresh=True)
        apply(sb("outage-recovered"))
        probe("outage-recovered", 1)
        gate(
            "G15 live containerd outage retains exact GPU leases and recovers cleanup/reuse",
            rejected,
        )
        all_sandboxes_gone()
        remove_pool()
    elif a.phase == "multi":
        apply(pool(slots=1, gpu=2))
        pods = wait_pool("gpu-matrix-pool", 1)
        container = next(
            x for x in pods[0]["spec"]["containers"] if x["name"] == "fastlet"
        )
        assert all(
            str(container["resources"][key]["nvidia.com/gpu"]) == "2"
            for key in ["limits", "requests"]
        )
        apply(sb("two-gpu-sandbox", count=2))
        probe("two-gpu-sandbox", 2)
        gate("G08 native Kubernetes Sandbox reserves and computes on exactly two UUIDs")
        all_sandboxes_gone()
        remove_pool()
        obj = pool(slots=1)
        obj["spec"]["capacity"].update(poolMin=2, poolMax=2)
        apply(obj)
        wait_pool("gpu-matrix-pool", 2)
        from concurrent.futures import ThreadPoolExecutor

        def create_one(index):
            apply(sb("parallel-" + str(index)))
            return probe("parallel-" + str(index), 1)

        with ThreadPoolExecutor(max_workers=2) as executor:
            values = list(executor.map(create_one, [1, 2]))
        assert set(values[0][1]["uuids"]).isdisjoint(values[1][1]["uuids"])
        assert len(journal()) == 2
        assert (
            values[0][0]["status"]["placement"]["fastletPodUID"]
            != values[1][0]["status"]["placement"]["fastletPodUID"]
        )
        with ThreadPoolExecutor(max_workers=2) as executor:
            list(executor.map(delete_sb, ["parallel-1", "parallel-2"]))
        assert not journal()
        gate(
            "G19 two real Fastlet Pods create/delete concurrently with disjoint UUIDs and no lost journal updates"
        )
        remove_pool()
    elif a.phase == "reboot-prepare":
        all_sandboxes_gone()
        remove_pool()
        apply(pool())
        wait_pool("gpu-matrix-pool", 1)
        apply(sb("reboot-owned"))
        item, value = probe("reboot-owned", 1)
        state = {
            "boot_id": Path("/proc/sys/kernel/random/boot_id").read_text().strip(),
            "sandbox": item,
            "probe": value,
            "journal": journal(),
            "task_pids": task_pids(),
        }
        (a.output / "before-reboot.json").write_text(json.dumps(state, indent=2))
        external(["systemctl", "enable", "fsb-gpu-k3s.service"])
        gate(
            "G28 live CUDA ownership and journal persisted before dedicated-node reboot"
        )
    elif a.phase == "reboot-check":
        before = json.loads(
            (
                a.output.parent / "matrix-reboot-prepare" / "before-reboot.json"
            ).read_text()
        )
        assert (
            Path("/proc/sys/kernel/random/boot_id").read_text().strip()
            != before["boot_id"]
        ), "Host did not reboot"
        uid = before["sandbox"]["metadata"]["uid"]
        outcome = None
        end = time.monotonic() + 90
        while time.monotonic() < end:
            item = get("sandbox", "reboot-owned")
            if (
                item.get("status", {}).get("runtime", {}).get("state") == "Ready"
                and uid in task_pids()
            ):
                probe("reboot-owned", 1, fresh=True)
                outcome = "recovered CUDA with the same Sandbox UID"
                break
            time.sleep(2)
        if outcome is None:
            leases = journal()
            pids = task_pids()
            assert not any(
                scope["ready"](x) for x in scope["pool_pods"]("gpu-matrix-pool")
            ), "Serving pool failed to recover sandbox"
            assert all(x["owner"]["sandboxUid"] == uid for x in leases.values()), leases
            outcome = "explicitly blocked pool readiness without unproven GPU reuse"
        gate(
            "G28 real node reboot: " + outcome,
            {
                "before": before,
                "after_sandbox": get("sandbox", "reboot-owned"),
                "after_journal": journal(),
                "after_task_pids": task_pids(),
            },
        )
        all_sandboxes_gone()
        remove_pool()
    elif a.phase == "latency":
        import statistics

        apply(pool())
        wait_pool("gpu-matrix-pool", 1)
        samples = []
        for index in range(30):
            before = time.monotonic()
            name = "latency-" + str(index)
            apply(sb(name))
            probe(name, 1)
            created = time.monotonic()
            delete_sb(name)
            deleted = time.monotonic()
            samples.append(
                {
                    "create_to_cuda_s": created - before,
                    "delete_to_absence_s": deleted - created,
                }
            )
        data = {}
        for key in ["create_to_cuda_s", "delete_to_absence_s"]:
            values = sorted(x[key] for x in samples)
            data[key] = {
                "p50": statistics.median(values),
                "p95": values[28],
                "max": max(values),
            }
        (a.output / "latency-samples.json").write_text(json.dumps(samples, indent=2))
        gate(
            "30 warm serial lifecycle cycles (API/CUDA and cleanup, not raw driver latency)",
            data,
        )
        remove_pool()
    elif a.phase == "window":
        import http.client
        import http.server
        import socket as sockets
        import threading
        import urllib.parse

        apply(pool())
        wait_pool("gpu-matrix-pool", 1)
        pod = gpu_pod()
        pod_ip = pod["status"]["podIP"]
        nodes = json.loads(external(["kubectl", "get", "nodes", "-o", "json"]).stdout)[
            "items"
        ]
        node_ip = next(
            x["address"]
            for x in nodes[0]["status"]["addresses"]
            if x["type"] == "InternalIP"
        )
        event = threading.Event()
        cut = {}
        intercepted = False

        class Proxy(http.server.BaseHTTPRequestHandler):
            def log_message(self, *args):
                pass

            def do_GET(self):
                self.forward()

            def do_POST(self):
                self.forward()

            def forward(self):
                global intercepted
                parsed = urllib.parse.urlsplit(self.path)
                if parsed.hostname != pod_ip or parsed.port != 5758:
                    self.send_error(403)
                    return
                body = self.rfile.read(int(self.headers.get("Content-Length", "0")))
                headers = {
                    k: v
                    for k, v in self.headers.items()
                    if k.lower()
                    not in ["proxy-authorization", "proxy-connection", "connection"]
                }
                connection = http.client.HTTPConnection(pod_ip, 5758, timeout=120)
                try:
                    connection.request(
                        self.command, parsed.path, body=body, headers=headers
                    )
                    response = connection.getresponse()
                    raw = response.read()
                    request = json.loads(body) if body else {}
                    if (
                        parsed.path.endswith("/create")
                        and request.get("identity", {}).get("name") == "crash-window"
                        and not intercepted
                    ):
                        intercepted = True
                        uid = request["identity"]["sandboxUid"]
                        pids = task_pids()
                        assert uid in pids
                        cut.update(
                            sandbox_uid=uid,
                            task_pid=pids[uid],
                            fastlet_pod_uid=pod["metadata"]["uid"],
                            backend_http_status=response.status,
                            caller_response_forwarded=False,
                        )
                        stop_pod(pod)
                        event.set()
                        self.connection.shutdown(sockets.SHUT_RDWR)
                        self.connection.close()
                        return
                    self.send_response(response.status)
                    for k, v in response.getheaders():
                        if k.lower() not in [
                            "connection",
                            "transfer-encoding",
                            "content-length",
                        ]:
                            self.send_header(k, v)
                    self.send_header("Content-Length", str(len(raw)))
                    self.end_headers()
                    self.wfile.write(raw)
                except Exception as error:
                    cut.setdefault("proxy_error", str(error))
                    try:
                        self.connection.close()
                    except Exception:
                        pass
                finally:
                    connection.close()

        server = http.server.ThreadingHTTPServer((node_ip, 19991), Proxy)
        server.daemon_threads = True
        threading.Thread(target=server.serve_forever, daemon=True).start()
        deployment = json.loads(
            external(
                [
                    "kubectl",
                    "-n",
                    "fast-sandbox-system",
                    "get",
                    "deployment",
                    "fast-sandbox-fastpath",
                    "-o",
                    "json",
                ]
            ).stdout
        )
        spec = copy.deepcopy(deployment["spec"])
        spec["replicas"] = 1
        spec["selector"] = {"matchLabels": {"app": "fsb-gpu-fault-fastpath"}}
        spec["template"]["metadata"]["labels"]["app"] = "fsb-gpu-fault-fastpath"
        env = spec["template"]["spec"]["containers"][0].setdefault("env", [])
        env[:] = [
            x
            for x in env
            if x["name"].upper() not in ["HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY"]
        ]
        env.extend(
            [
                {"name": "HTTP_PROXY", "value": "http://" + node_ip + ":19991"},
                {"name": "NO_PROXY", "value": ""},
            ]
        )
        objects = [
            {
                "apiVersion": "apps/v1",
                "kind": "Deployment",
                "metadata": {
                    "name": "fsb-gpu-fault-fastpath",
                    "namespace": "fast-sandbox-system",
                },
                "spec": spec,
            },
            {
                "apiVersion": "v1",
                "kind": "Service",
                "metadata": {
                    "name": "fsb-gpu-fault-fastpath",
                    "namespace": "fast-sandbox-system",
                },
                "spec": {
                    "selector": {"app": "fsb-gpu-fault-fastpath"},
                    "ports": [{"port": 9090, "targetPort": "grpc"}],
                },
            },
        ]
        try:
            for obj in objects:
                scope["cmd"](
                    ["kubectl", "-n", "fast-sandbox-system", "apply", "-f", "-"],
                    obj=obj,
                )
            external(
                [
                    "kubectl",
                    "-n",
                    "fast-sandbox-system",
                    "rollout",
                    "status",
                    "deployment/fsb-gpu-fault-fastpath",
                    "--timeout=90s",
                ]
            )
            service = json.loads(
                external(
                    [
                        "kubectl",
                        "-n",
                        "fast-sandbox-system",
                        "get",
                        "svc",
                        "fsb-gpu-fault-fastpath",
                        "-o",
                        "json",
                    ]
                ).stdout
            )

            def service_listening():
                try:
                    with sockets.create_connection(
                        (service["spec"]["clusterIP"], 9090), timeout=2
                    ):
                        return True
                except OSError:
                    return False

            wait("fault FastPath service data plane", service_listening)
            args = [
                client,
                "-endpoint",
                service["spec"]["clusterIP"] + ":9090",
                "-namespace",
                ns,
                "-pool",
                "gpu-matrix-pool",
                "-mode",
                "one",
                "-name",
                "crash-window",
            ]
            result = external(args, allow=True, timeout=100)
            assert event.wait(3), cut
            wait_pool("gpu-matrix-pool", 1)
            item, _ = probe("crash-window", 1, fresh=True)
            assert item["metadata"]["uid"] == cut["sandbox_uid"]
            assert (
                len(
                    [
                        x
                        for x in journal().values()
                        if x["owner"]["sandboxUid"] == cut["sandbox_uid"]
                    ]
                )
                == 1
            )
            replay = api("one", name="crash-window")
            assert replay["uid"] == cut["sandbox_uid"]
            gate(
                "G14 real task exists, Fastlet crashes before create acknowledgement is forwarded; replay preserves one UID/lease",
                cut,
            )
        finally:
            server.shutdown()
            server.server_close()
            external(
                [
                    "kubectl",
                    "-n",
                    "fast-sandbox-system",
                    "delete",
                    "deployment,service",
                    "fsb-gpu-fault-fastpath",
                    "--ignore-not-found",
                    "--wait=false",
                ],
                allow=True,
            )
        all_sandboxes_gone()
        remove_pool()
    elif a.phase == "execd":
        obj = pool("gpu-execd-pool", slots=1)
        obj["spec"]["infraComponents"] = [
            {
                "name": "execd",
                "artifact": {
                    "source": {
                        "image": {
                            "reference": "sandbox-registry.cn-zhangjiakou.cr.aliyuncs.com/opensandbox/execd@sha256:1dc98c7de10b9a73450ac75aa0f200ad7972f2c40f5225f6a8998e166b45d6dd"
                        }
                    },
                    "mappings": [
                        {
                            "sourcePath": "/execd",
                            "targetPath": "/.fast/components/execd/execd",
                        }
                    ],
                },
                "process": {
                    "command": ["/.fast/components/execd/execd"],
                    "restartPolicy": "OnFailure",
                    "healthCheck": {"httpGet": {"path": "/ping"}, "timeoutSeconds": 10},
                },
                "endpoint": {"protocol": "HTTP", "port": 44772},
            }
        ]
        apply(obj)
        wait_pool("gpu-execd-pool", 1)
        apply(sb("agent-protocol", poolname="gpu-execd-pool"))
        probe("agent-protocol", 1)
        prefix = [
            fastctl,
            "--endpoint",
            endpoint,
            "--namespace",
            ns,
            "--proxy-endpoint",
            proxy_endpoint,
            "opensandbox",
            "exec",
            "agent-protocol",
            "--",
        ]
        cuda = external(
            prefix
            + [
                "python",
                "-u",
                "-c",
                probe_source,
                "--expect-count",
                "1",
                "--hold-seconds",
                "0",
            ]
        )
        assert "FSB_GPU_DRIVER_OK" in cuda.stdout, cuda.stdout
        file = external(
            prefix
            + [
                "sh",
                "-c",
                "printf fsb-agent-file-ok >/tmp/fsb-agent-file; cat /tmp/fsb-agent-file",
            ]
        )
        assert "fsb-agent-file-ok" in file.stdout, file.stdout
        gate(
            "G27 official Execd adapter executes real CUDA and a file operation through authenticated proxy"
        )
        all_sandboxes_gone()
        remove_pool("gpu-execd-pool")
    report["result"] = "PASS"
    scope["save"]()
except Exception as error:
    report["result"] = "FAIL"
    report["error"] = str(error)
    scope["save"]()
    raise
