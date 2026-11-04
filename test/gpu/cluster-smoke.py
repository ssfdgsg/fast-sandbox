#!/usr/bin/env python3
"""Bounded native Kubernetes GPU acceptance. Run on the single dedicated GPU node as root."""
import argparse
import json
import os
from pathlib import Path
import subprocess
import sys
import time

p = argparse.ArgumentParser()
p.add_argument('--fixtures', type=Path, required=True)
p.add_argument('--output', type=Path, required=True)
p.add_argument('--include-faults', action='store_true')
p.add_argument('--include-capacity', action='store_true')
p.add_argument('--timeout', type=int, default=180)
p.add_argument('--budget-seconds', type=int, default=1200)
a = p.parse_args()
a.output.mkdir(parents=True, exist_ok=True)
ns = json.loads((a.fixtures / 'namespace.json').read_text())['metadata']['name']
assert ns.startswith('fsb-gpu-'), 'Use an isolated fsb-gpu-* test namespace'
assert os.geteuid() == 0, 'Root is required to compare the node journal with actual probes'
start = time.monotonic()
report = {'namespace': ns, 'gates': [], 'result': 'RUNNING', 'scope': 'single-native-GPU-node', 'pending': ['full PLAN.md fault matrix beyond selected gates', 'multi-node recovery']}
owned = False
uids = set()


def save():
    report['elapsed_seconds'] = round(time.monotonic() - start, 2)
    (a.output / 'cluster-report.json').write_text(json.dumps(report, indent=2) + '\n')


def cmd(args, *, obj=None, allow=False):
    if time.monotonic() - start > a.budget_seconds:
        raise TimeoutError('Overall test budget exceeded; inspect retained diagnostics')
    r = subprocess.run(args, input=json.dumps(obj) if obj is not None else None, text=True, capture_output=True, timeout=25)
    with (a.output / 'commands.log').open('a') as log:
        log.write(json.dumps({'args': args, 'rc': r.returncode, 'stdout': r.stdout, 'stderr': r.stderr}) + '\n')
    if r.returncode and not allow:
        raise RuntimeError(f'{args}: {r.stderr[-1500:]}')
    return r


def k(*args, **kwargs):
    return cmd(['kubectl', '--request-timeout=15s', '-n', ns, *args], **kwargs)


def get(kind, name=None):
    return json.loads(k('get', kind, *([name] if name else []), '-o', 'json').stdout)


def apply_file(name):
    k('apply', '-f', str(a.fixtures / (name + '.json')))


def wait(label, predicate, timeout=None):
    end = time.monotonic() + (timeout or a.timeout)
    last = None
    while time.monotonic() < end:
        try:
            value = predicate()
            if value:
                return value
        except (RuntimeError, FileNotFoundError, json.JSONDecodeError) as e:
            last = str(e)
        time.sleep(1)
    raise TimeoutError(f'{label}: {last}')


def gate(name, data=None):
    report['gates'].append({'name': name, 'status': 'PASS', 'evidence': data})
    save()
    print('PASS', name, flush=True)


def ready(pod):
    return any(c['type'] == 'Ready' and c['status'] == 'True' for c in pod.get('status', {}).get('conditions', []))


def pool_pods(pool):
    return [x for x in get('pods')['items'] if x['metadata'].get('labels', {}).get('fast-sandbox.io/pool') == pool and not x['metadata'].get('deletionTimestamp')]


def wait_pool(pool, count):
    def check():
        pods = [x for x in pool_pods(pool) if ready(x)]
        return pods if len(pods) == count else None
    return wait('ready Pool '+pool, check)


def journal():
    path = Path('/var/lib/fast-sandbox/gpu/leases.json')
    return json.loads(path.read_text()).get('leases', {}) if path.exists() else {}


def task_pids():
    socket = os.environ.get('FSB_GPU_E2E_SOCKET', '/run/containerd/containerd.sock')
    lines = cmd(['ctr', '--address', socket, '-n', 'k8s.io', 'tasks', 'ls']).stdout.splitlines()[1:]
    return {parts[0]: parts[1] for line in lines if len(parts := line.split()) >= 2}


def sandbox_probe(name, expected):
    def check():
        sb = get('sandbox', name)
        if sb.get('status', {}).get('runtime', {}).get('state') != 'Ready':
            return None
        uid = sb['metadata']['uid']
        uids.add(uid)
        pod_uid = sb['status']['placement']['fastletPodUID']
        pod = next((x for x in get('pods')['items'] if x['metadata']['uid'] == pod_uid), None)
        if not pod:
            return None
        r = k('exec', pod['metadata']['name'], '-c', 'fastlet', '--', 'cat', f'/var/log/fast-sandbox/{uid}.log', allow=True)
        for line in r.stdout.splitlines():
            if line.startswith('FSB_GPU_DRIVER_OK '):
                value = json.loads(line.split(' ', 1)[1])
                assert len(value['uuids']) == expected, value
                leases = [x for x in journal().values() if x['owner']['sandboxUid'] == uid]
                if expected:
                    assert len(leases) == 1, (uid, leases)
                    assigned = [x.removeprefix('nvidia.com/gpu=') for x in leases[0]['devices']]
                    assert sorted(assigned) == sorted(value['uuids']), (assigned, value)
                else:
                    assert not leases, leases
                (a.output / (name + '-probe.json')).write_text(json.dumps(value, indent=2))
                return sb, value
        return None
    return wait('CUDA probe '+name, check)


def delete_sandbox(name):
    r = k('get', 'sandbox', name, '-o', 'json', allow=True)
    if r.returncode:
        assert 'NotFound' in r.stderr, r.stderr
        return
    uid = json.loads(r.stdout)['metadata']['uid']
    uids.add(uid)
    k('delete', 'sandbox', name, '--wait=false')
    def gone():
        obj = k('get', 'sandbox', name, '-o', 'json', allow=True)
        return obj.returncode and 'NotFound' in obj.stderr and uid not in task_pids() and all(x['owner']['sandboxUid'] != uid for x in journal().values())
    wait('runtime and lease removal '+name, gone)


try:
    # Creating the namespace, rather than applying, prevents touching another test run.
    k('create', '-f', str(a.fixtures / 'namespace.json'))
    owned = True
    apply_file('gpu-pool')
    pods = wait_pool('gpu-test-pool', 1)
    gpu_container = next(x for x in pods[0]['spec']['containers'] if x['name'] == 'fastlet')
    for field in ['requests', 'limits']:
        assert str(gpu_container['resources'][field]['nvidia.com/gpu']) == '2'
    gate('Kubernetes reserves exactly two GPUs for the two-slot Fastlet')
    apply_file('gpu-sandbox-1')
    apply_file('gpu-sandbox-2')
    s1, v1 = sandbox_probe('gpu-test-1', 1)
    s2, v2 = sandbox_probe('gpu-test-2', 1)
    assert set(v1['uuids']).isdisjoint(v2['uuids'])
    gate('Two Sandboxes execute CUDA on distinct physical allocated UUIDs', [v1, v2])
    apply_file('gpu-sandbox-3')
    for _ in range(15):
        overflow = get('sandbox', 'gpu-test-3')
        assert overflow.get('status', {}).get('runtime', {}).get('state') != 'Ready'
        overflow_uid = overflow['metadata']['uid']
        assert overflow_uid not in task_pids()
        assert all(x['owner']['sandboxUid'] != overflow_uid for x in journal().values())
        time.sleep(1)
    gate('Overflow creates no task or GPU lease while all slots are occupied')
    delete_sandbox('gpu-test-1')
    s3, v3 = sandbox_probe('gpu-test-3', 1)
    assert v3['uuids'] == v1['uuids']
    gate('Deleting a Sandbox restores capacity and reuses only the freed GPU')
    apply_file('cpu-pool')
    wait_pool('cpu-test-pool', 1)
    apply_file('cpu-sandbox')
    sandbox_probe('cpu-test', 0)
    gate('CPU Sandbox has no usable GPU or per-card device node')
    if a.include_faults:
        pod = wait_pool('gpu-test-pool', 1)[0]
        assert not pod['spec'].get('hostPID'), 'Refuse PID-1 fault in a hostPID Pod'
        name, old_uid = pod['metadata']['name'], pod['metadata']['uid']
        old_restarts = next(x for x in pod['status']['containerStatuses'] if x['name'] == 'fastlet')['restartCount']
        before = task_pids()
        # PID 1 is only the Fastlet process inside its own PID namespace.
        k('exec', name, '-c', 'fastlet', '--', 'sh', '-c', 'kill -KILL 1', allow=True)
        def restarted():
            pod = get('pod', name)
            statuses = pod.get('status', {}).get('containerStatuses', [])
            return pod['metadata']['uid'] == old_uid and ready(pod) and any(x['name'] == 'fastlet' and x['restartCount'] > old_restarts for x in statuses)
        wait('same-Pod Fastlet process restart', restarted)
        sandbox_probe('gpu-test-2', 1)
        sandbox_probe('gpu-test-3', 1)
        after = task_pids()
        for uid in [s2['metadata']['uid'], s3['metadata']['uid']]:
            assert uid in before and after.get(uid) == before[uid], (uid, before, after)
        gate('Actual Fastlet SIGKILL restart preserves exact GPU task PIDs and allocation')
        k('delete', 'pod', name, '--wait=false')
        def replaced():
            return [x for x in pool_pods('gpu-test-pool') if x['metadata']['uid'] != old_uid and ready(x)]
        replacement = wait('Pod replacement and orphan cleanup', replaced, timeout=300)
        old_sb_uids = {s2['metadata']['uid'], s3['metadata']['uid']}
        assert not old_sb_uids.intersection(task_pids())
        assert all(x['owner']['sandboxUid'] not in old_sb_uids for x in journal().values())
        gate('Fastlet Pod replacement becomes Ready only after prior tasks and leases are absent', replacement[0]['metadata']['uid'])
    for name in ['gpu-test-1', 'gpu-test-2', 'gpu-test-3']:
        delete_sandbox(name)
    if a.include_capacity:
        k('delete', 'sandboxpool', 'gpu-test-pool', '--wait=false')
        wait('old GPU Pool removal', lambda: not [x for x in get('pods')['items'] if x['metadata'].get('labels', {}).get('fast-sandbox.io/pool') == 'gpu-test-pool'])
        scale = json.loads((a.fixtures / 'gpu-pool.json').read_text())
        scale['metadata']['name'] = 'gpu-scale-pool'
        scale['spec']['maxSandboxesPerPod'] = 1
        scale['spec']['capacity'].update(poolMin=1, poolMax=1)
        k('apply', '-f', '-', obj=scale)
        wait_pool('gpu-scale-pool', 1)
        for index in [1, 2]:
            if index == 2:
                k('patch', 'sandboxpool', 'gpu-scale-pool', '--type=merge', '-p', json.dumps({'spec': {'capacity': {'poolMin': 2, 'poolMax': 2}}}))
                wait_pool('gpu-scale-pool', 2)
            sb = json.loads((a.fixtures / 'gpu-sandbox-1.json').read_text())
            sb['metadata']['name'] = f'gpu-scale-{index}'
            sb['spec']['poolRef'] = 'gpu-scale-pool'
            k('apply', '-f', '-', obj=sb)
        _, scale1 = sandbox_probe('gpu-scale-1', 1)
        _, scale2 = sandbox_probe('gpu-scale-2', 1)
        assert set(scale1['uuids']).isdisjoint(scale2['uuids'])
        gate('Scale-out adds a real second Fastlet and exclusive GPU capacity')
        k('patch', 'sandboxpool', 'gpu-scale-pool', '--type=merge', '-p', json.dumps({'spec': {'capacity': {'poolMin': 3, 'poolMax': 3}}}))
        wait('third Fastlet scheduled intent', lambda: len(pool_pods('gpu-scale-pool')) == 3)
        for _ in range(15):
            assert sum(ready(x) for x in pool_pods('gpu-scale-pool')) == 2
            time.sleep(1)
        events = get('events')['items']
        assert any('Insufficient nvidia.com/gpu' in x.get('message', '') for x in events), 'Need explicit scheduler GPU-exhaustion evidence'
        gate('Scale-out without free GPU remains Pending and does not fabricate capacity')
        delete_sandbox('gpu-scale-2')
        k('patch', 'sandboxpool', 'gpu-scale-pool', '--type=merge', '-p', json.dumps({'spec': {'capacity': {'poolMin': 1, 'poolMax': 1}}}))
        wait('scale-in removal', lambda: len([x for x in get('pods')['items'] if x['metadata'].get('labels', {}).get('fast-sandbox.io/pool') == 'gpu-scale-pool']) == 1)
        wait_pool('gpu-scale-pool', 1)
        _, remaining = sandbox_probe('gpu-scale-1', 1)
        apply_file('external-gpu-pod')
        def external_probe():
            result = k('logs', 'external-gpu-test', allow=True)
            for line in result.stdout.splitlines():
                if line.startswith('FSB_GPU_DRIVER_OK '):
                    return json.loads(line.split(' ', 1)[1])
            return None
        external = wait('external Kubernetes CUDA workload', external_probe)
        assert len(external['uuids']) == 1 and set(external['uuids']).isdisjoint(remaining['uuids'])
        gate('Scale-in releases GPU capacity to an independent Kubernetes CUDA Pod', external)
        k('delete', 'pod', 'external-gpu-test', '--wait=false')
        wait('external Pod removal', lambda: k('get', 'pod', 'external-gpu-test', allow=True).stderr.find('NotFound') >= 0)
        delete_sandbox('gpu-scale-1')
        k('delete', 'sandboxpool', 'gpu-scale-pool', '--wait=false')
        wait('scale Pool removal', lambda: not [x for x in get('pods')['items'] if x['metadata'].get('labels', {}).get('fast-sandbox.io/pool') == 'gpu-scale-pool'])
    delete_sandbox('cpu-test')
    report['result'] = 'PASS_FOR_SELECTED_GATES'
except Exception as e:
    report['result'] = 'FAIL'
    report['error'] = str(e)
    print('FAIL', e, file=sys.stderr)
finally:
    save()
    if owned:
        # Retain resources on failure for diagnosis; a clean PASS removes exact test ownership.
        for resource in ['sandboxpools', 'sandboxes', 'pods', 'events']:
            try:
                data = k('get', resource, '-o', 'json', allow=True)
                (a.output / (resource + '.json')).write_text(data.stdout or data.stderr)
            except Exception:
                pass
        if report['result'] == 'PASS_FOR_SELECTED_GATES':
            try:
                k('delete', 'sandboxpool', 'gpu-test-pool', 'cpu-test-pool', '--ignore-not-found', '--wait=false')
                wait('Fastlet removal', lambda: not [x for x in get('pods')['items'] if x['metadata'].get('labels', {}).get('fast-sandbox.io/pool') in ['gpu-test-pool', 'cpu-test-pool']])
                assert all(x['owner']['sandboxUid'] not in uids for x in journal().values())
                k('delete', 'namespace', ns, '--wait=false')
                gate('Exact test tasks, GPU leases and Pool Pods cleaned')
            except Exception as e:
                report['result'], report['cleanup_error'] = 'FAIL', str(e)
        save()
sys.exit(0 if report['result'] == 'PASS_FOR_SELECTED_GATES' else 1)
