#!/usr/bin/env python3
"""Render disposable test fixtures, without contacting a cluster."""
import argparse
import json
from pathlib import Path

p = argparse.ArgumentParser()
p.add_argument('--image', required=True, help='Compatible PyTorch image, preferably pinned by digest')
p.add_argument('--fastlet-image', default='fast-sandbox/fastlet:dev')
p.add_argument('--namespace', default='fsb-gpu-e2e')
p.add_argument('--slots', type=int, default=1)
p.add_argument('--gpus-per-sandbox', type=int, default=1)
p.add_argument('--pod-pull-policy', choices=['Never', 'IfNotPresent', 'Always'], default='Never')
p.add_argument('--output', type=Path, required=True)
a = p.parse_args()
assert a.slots > 0 and a.gpus_per_sandbox > 0
a.output.mkdir(parents=True, exist_ok=True)
probe = Path(__file__).with_name('cuda-probe.py').read_text()

def save(name, obj):
    (a.output / (name + '.json')).write_text(json.dumps(obj, indent=2) + '\n')

def meta(name):
    return {'name': name, 'namespace': a.namespace, 'labels': {'fsb-gpu-test': 'true'}}

def pool(name, gpu):
    resources = {'cpu': '1', 'memory': '4Gi', 'pids': 512}
    if gpu:
        resources['gpus'] = a.gpus_per_sandbox
    return {'apiVersion': 'sandbox.fast.io/v1alpha2', 'kind': 'SandboxPool', 'metadata': meta(name), 'spec': {
        'runtime': 'container', 'sandboxResources': resources, 'maxSandboxesPerPod': a.slots if gpu else 1,
        'capacity': {'poolMin': 1, 'poolMax': 1, 'bufferMin': 0, 'bufferMax': 0}, 'warmImages': [a.image],
        'fastletTemplate': {'spec': {'containers': [{'name': 'fastlet', 'image': a.fastlet_image, 'imagePullPolicy': 'IfNotPresent'}]}}}}

def sandbox(name, gpu):
    return {'apiVersion': 'sandbox.fast.io/v1alpha2', 'kind': 'Sandbox', 'metadata': meta(name), 'spec': {
        'image': a.image, 'poolRef': 'gpu-test-pool' if gpu else 'cpu-test-pool',
        'command': ['python', '-u', '-c', probe, '--expect-count', str(a.gpus_per_sandbox if gpu else 0), '--hold-seconds', '1800']}}

save('namespace', {'apiVersion': 'v1', 'kind': 'Namespace', 'metadata': {
    'name': a.namespace, 'labels': {'fsb-gpu-test': 'true', 'pod-security.kubernetes.io/enforce': 'privileged'}}})
save('gpu-pool', pool('gpu-test-pool', True))
save('cpu-pool', pool('cpu-test-pool', False))
for i in range(a.slots + 1):
    save(f'gpu-sandbox-{i+1}', sandbox(f'gpu-test-{i+1}', True))
save('cpu-sandbox', sandbox('cpu-test', False))
save('external-gpu-pod', {'apiVersion': 'v1', 'kind': 'Pod', 'metadata': meta('external-gpu-test'), 'spec': {
    'restartPolicy': 'Never', 'containers': [{'name': 'cuda', 'image': a.image, 'imagePullPolicy': a.pod_pull_policy,
    'command': ['python', '-u', '-c', probe, '--expect-count', '1', '--hold-seconds', '1800'],
    'resources': {'requests': {'nvidia.com/gpu': '1'}, 'limits': {'nvidia.com/gpu': '1'}}}]}})
print(f'Rendered fixtures to {a.output}. Apply individual files in the order in PLAN.md; do not apply the entire directory.')
