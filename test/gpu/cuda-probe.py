"""Real CUDA probe for test Sandboxes; reports physical UUIDs through the CUDA driver."""
import argparse
import ctypes
import glob
import json
import os
import time

parser = argparse.ArgumentParser()
parser.add_argument('--expect-count', type=int, default=1)
parser.add_argument('--hold-seconds', type=int, default=600)
args = parser.parse_args()
info = {'expected_count': args.expect_count, 'device_nodes': glob.glob('/dev/nvidia*'),
        'CUDA_VISIBLE_DEVICES': os.environ.get('CUDA_VISIBLE_DEVICES'), 'uuids': []}
try:
    driver = ctypes.CDLL('libcuda.so.1')
except OSError:
    if args.expect_count:
        raise
    driver = None
count = ctypes.c_int()
if driver is not None:
    init = driver.cuInit(0)
    # CUDA_ERROR_NO_DEVICE is expected for a CPU-only container.
    if init == 0:
        assert driver.cuDeviceGetCount(ctypes.byref(count)) == 0
    elif args.expect_count or init != 100:
        raise RuntimeError('cuInit failed: ' + str(init))
assert count.value == args.expect_count, info | {'actual_count': count.value}
if args.expect_count:
    import torch
    assert torch.cuda.is_available()
    assert torch.cuda.device_count() == args.expect_count
    info.update(torch=torch.__version__, cuda=torch.version.cuda, arches=torch.cuda.get_arch_list())
    nvml = None
    try:
        nvml = ctypes.CDLL('libnvidia-ml.so.1')
        if nvml.nvmlInit() != 0:
            nvml = None
    except Exception:
        nvml = None
    for ordinal in range(count.value):
        dev = ctypes.c_int()
        assert driver.cuDeviceGet(ctypes.byref(dev), ordinal) == 0
        resolved_uuid = None
        if nvml is not None:
            handle = ctypes.c_void_p()
            if nvml.nvmlDeviceGetHandleByIndex(ordinal, ctypes.byref(handle)) == 0:
                buf = ctypes.create_string_buffer(96)
                if nvml.nvmlDeviceGetUUID(handle, buf, 96) == 0:
                    resolved_uuid = buf.value.decode('utf-8')
        if not resolved_uuid:
            uuid = (ctypes.c_ubyte * 16)()
            assert driver.cuDeviceGetUuid(ctypes.byref(uuid), dev) == 0
            h = bytes(uuid).hex()
            resolved_uuid = 'GPU-' + '-'.join((h[:8], h[8:12], h[12:16], h[16:20], h[20:]))
        info['uuids'].append(resolved_uuid)
        x = torch.ones((32, 32), device=f'cuda:{ordinal}')
        assert (x @ x).sum().item() == 32768
        torch.cuda.synchronize(ordinal)
    if nvml is not None:
        try:
            nvml.nvmlShutdown()
        except Exception:
            pass
else:
    assert not glob.glob('/dev/nvidia[0-9]*'), info
print('FSB_GPU_PROBE_OK ' + json.dumps(info, sort_keys=True), flush=True)
time.sleep(args.hold_seconds)
