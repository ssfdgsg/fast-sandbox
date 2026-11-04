"""Regression checks for GPU qualification decisions without a live cluster."""
import ast
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import types
import unittest
from unittest import mock

KIT = Path(__file__).parent


def runner_functions():
    tree = ast.parse((KIT / 'cluster-smoke.py').read_text())
    # Load helpers without executing CLI parsing or the hardware runner's main block.
    body = [node for node in tree.body if isinstance(node, (ast.Import, ast.ImportFrom, ast.FunctionDef))]
    scope = {'__file__': str(KIT / 'cluster-smoke.py')}
    exec(compile(ast.Module(body=body, type_ignores=[]), str(KIT / 'cluster-smoke.py'), 'exec'), scope)
    return scope


class RunnerRegressionTest(unittest.TestCase):
    def test_restart_executes_a_fresh_cuda_kernel_without_reading_old_logs(self):
        scope = runner_functions()
        with tempfile.TemporaryDirectory() as tmp:
            scope['a'] = types.SimpleNamespace(output=Path(tmp))
            scope['uids'] = set()
            scope['get'] = lambda kind, name=None: ({'metadata': {'uid': 'sandbox'}, 'status': {'runtime': {'state': 'Ready'}, 'placement': {'fastletPodUID': 'pod'}}} if kind == 'sandbox' else {'items': [{'metadata': {'uid': 'pod', 'name': 'fastlet'}}]})
            scope['journal'] = lambda: {'sandbox': {'owner': {'sandboxUid': 'sandbox'}, 'devices': ['nvidia.com/gpu=GPU-a']}}
            scope['k'] = lambda *args, **kwargs: self.fail('Restart verification must not read the previous Fastlet log')
            calls = []
            def execute(args, **kwargs):
                calls.append(args)
                return types.SimpleNamespace(stdout='FSB_GPU_DRIVER_OK ' + json.dumps({'uuids': ['GPU-a']}))
            scope['cmd'] = execute
            scope['wait'] = lambda label, check: check()
            _, result = scope['sandbox_probe']('test', 1, fresh=True)
            self.assertEqual(result['uuids'], ['GPU-a'])
            self.assertIn('exec', calls[0])
            self.assertIn('sandbox', calls[0])
            self.assertEqual(calls[0][-2:], ['--hold-seconds', '0'])
            self.assertIn('cuLaunchKernel', calls[0][calls[0].index('-c') + 1])
            scope['cmd'] = lambda *args, **kwargs: types.SimpleNamespace(stdout='FSB_GPU_DRIVER_OK ' + json.dumps({'uuids': ['GPU-other']}))
            with self.assertRaises(AssertionError):
                scope['sandbox_probe']('test', 1, fresh=True)

    def test_host_kill_requires_exact_container_cgroup_and_uses_pidfd(self):
        scope = runner_functions()
        scope['task_pids'] = lambda: {'container-id': '1234'}
        pod = {'spec': {}, 'status': {'containerStatuses': [{'name': 'fastlet', 'containerID': 'containerd://container-id'}]}}
        scope['Path'] = lambda name: types.SimpleNamespace(read_text=lambda: '/cri-containerd-container-id.scope', read_bytes=lambda: b'/workspace/fastlet')
        with mock.patch.object(scope['os'], 'pidfd_open', return_value=91, create=True) as opened, mock.patch.object(scope['os'], 'close') as closed, mock.patch.object(scope['signal'], 'pidfd_send_signal', create=True) as sent:
            scope['kill_fastlet_process'](pod)
            opened.assert_called_once_with(1234)
            sent.assert_called_once_with(91, scope['signal'].SIGKILL)
            closed.assert_called_once_with(91)
            sent.reset_mock()
            scope['Path'] = lambda name: types.SimpleNamespace(read_text=lambda: '/unrelated-container.scope', read_bytes=lambda: b'/workspace/fastlet')
            with self.assertRaises(AssertionError):
                scope['kill_fastlet_process'](pod)
            sent.assert_not_called()

    def test_existing_allocator_outside_kube_system_blocks_before_mutation(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            log = root / 'calls.jsonl'
            kubectl = root / 'kubectl'
            kubectl.write_text('#!' + sys.executable + '\n' + '''import json,os,sys
with open(os.environ['FSB_FAKE_COMMAND_LOG'],'a') as f:f.write(json.dumps(sys.argv[1:])+'\\n')
print(json.dumps({'items':[{'metadata':{'name':'nvidia-device-plugin','namespace':'gpu-operator'},'spec':{'template':{'spec':{'containers':[{'image':'nvcr.io/nvidia/k8s-device-plugin:v0.17.1'}]}}}}]}))
''')
            kubectl.chmod(0o755)
            env = dict(os.environ, PATH=str(root) + os.pathsep + os.environ['PATH'], FSB_FAKE_COMMAND_LOG=str(log))
            result = subprocess.run([sys.executable, str(KIT / 'deploy-test-stack.py'), '--config', str(root), '--socket', '/test/containerd.sock', '--containerd-root', '/test/containerd', '--output', str(root / 'out')], env=env, text=True, capture_output=True, timeout=10)
            self.assertNotEqual(result.returncode, 0)
            self.assertIn('gpu-operator', result.stderr)
            calls = [json.loads(line) for line in log.read_text().splitlines()]
            self.assertEqual(len(calls), 1)
            self.assertIn('--all-namespaces', calls[0])
            self.assertNotIn('apply', calls[0])


if __name__ == '__main__':
    unittest.main()
