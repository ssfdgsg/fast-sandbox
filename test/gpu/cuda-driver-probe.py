"""Small native-Linux CUDA kernel probe. Requires Python stdlib and CDI driver libraries only."""
import argparse
import ctypes as C
import glob
import json
import os
import time
import signal
import sys

# Cooperate with the runtime grace period; Python PID 1 otherwise ignores default SIGTERM.
signal.signal(signal.SIGTERM, lambda signum, frame: sys.exit(0))

# Do not let an image's CUDA_VISIBLE_DEVICES hide an accidentally exposed card.
os.environ.pop('CUDA_VISIBLE_DEVICES', None)

PTX = r'''
.version 6.0
.target sm_52
.address_size 64
.visible .entry fsb_matmul(.param .u64 out, .param .u64 a, .param .u64 b)
{
 .reg .pred %p<3>;
 .reg .b32 %r<16>;
 .reg .b64 %rd<10>;
 ld.param.u64 %rd1, [out];
 ld.param.u64 %rd2, [a];
 ld.param.u64 %rd3, [b];
 mov.u32 %r1, %tid.x;
 mov.u32 %r2, %ctaid.x;
 mov.u32 %r3, %ntid.x;
 mad.lo.s32 %r4, %r2, %r3, %r1;
 setp.ge.u32 %p1, %r4, 1024;
 @%p1 bra DONE;
 shr.u32 %r5, %r4, 5;
 and.b32 %r6, %r4, 31;
 mov.u32 %r7, 0;
 mov.u32 %r8, 0;
LOOP:
 mad.lo.s32 %r9, %r5, 32, %r7;
 mad.lo.s32 %r10, %r7, 32, %r6;
 mul.wide.u32 %rd4, %r9, 4;
 mul.wide.u32 %rd5, %r10, 4;
 add.s64 %rd6, %rd2, %rd4;
 add.s64 %rd7, %rd3, %rd5;
 ld.global.u32 %r11, [%rd6];
 ld.global.u32 %r12, [%rd7];
 mad.lo.s32 %r8, %r11, %r12, %r8;
 add.u32 %r7, %r7, 1;
 setp.lt.u32 %p2, %r7, 32;
 @%p2 bra LOOP;
 mul.wide.u32 %rd8, %r4, 4;
 add.s64 %rd9, %rd1, %rd8;
 st.global.u32 [%rd9], %r8;
DONE:
 ret;
}
'''


def bind(lib, name, args):
    f = getattr(lib, name)
    f.argtypes, f.restype = args, C.c_int
    return f


def check(rc, operation):
    if rc:
        raise RuntimeError(f'{operation}: status={rc}')


def run(expected, hold):
    info = {'expected_count': expected, 'device_nodes': glob.glob('/dev/nvidia*'), 'uuids': [], 'pci_bus_ids': [], 'devices': [], 'kernel': 'ptx-int32-matmul-32x32'}
    try:
        cuda = C.CDLL('libcuda.so.1')
    except OSError:
        if expected:
            raise
        cuda = None
    count = C.c_int()
    if cuda:
        init = bind(cuda, 'cuInit', [C.c_uint])(0)
        if init == 0:
            check(bind(cuda, 'cuDeviceGetCount', [C.POINTER(C.c_int)])(C.byref(count)), 'cuDeviceGetCount')
        elif expected or init != 100:
            check(init, 'cuInit')
    if count.value != expected:
        raise AssertionError(info | {'actual_count': count.value})
    if expected:
        # Match CUDA to NVML by PCI identity, never by ordinal; their indexes may differ.
        nvml = C.CDLL('libnvidia-ml.so.1')
        check(bind(nvml, 'nvmlInit_v2', [])(), 'nvmlInit_v2')
        try:
            for ordinal in range(expected):
                dev, ctx, module, fun = C.c_int(), C.c_void_p(), C.c_void_p(), C.c_void_p()
                check(bind(cuda, 'cuDeviceGet', [C.POINTER(C.c_int), C.c_int])(C.byref(dev), ordinal), 'cuDeviceGet')
                bus, uuid, handle = C.create_string_buffer(32), C.create_string_buffer(96), C.c_void_p()
                check(bind(cuda, 'cuDeviceGetPCIBusId', [C.c_void_p, C.c_int, C.c_int])(bus, len(bus), dev), 'cuDeviceGetPCIBusId')
                check(bind(nvml, 'nvmlDeviceGetHandleByPciBusId_v2', [C.c_char_p, C.POINTER(C.c_void_p)])(bus.value, C.byref(handle)), 'nvmlDeviceGetHandleByPciBusId_v2')
                check(bind(nvml, 'nvmlDeviceGetUUID', [C.c_void_p, C.c_void_p, C.c_uint])(handle, uuid, len(uuid)), 'nvmlDeviceGetUUID')
                info['uuids'].append(uuid.value.decode())
                info['pci_bus_ids'].append(bus.value.decode())
                name, major, minor = C.create_string_buffer(256), C.c_int(), C.c_int()
                check(bind(cuda, 'cuDeviceGetName', [C.c_void_p, C.c_int, C.c_int])(name, len(name), dev), 'cuDeviceGetName')
                attribute = bind(cuda, 'cuDeviceGetAttribute', [C.POINTER(C.c_int), C.c_int, C.c_int])
                check(attribute(C.byref(major), 75, dev), 'compute capability major')
                check(attribute(C.byref(minor), 76, dev), 'compute capability minor')
                info['devices'].append({'name': name.value.decode(), 'compute_capability': [major.value, minor.value]})
                check(bind(cuda, 'cuDevicePrimaryCtxRetain', [C.POINTER(C.c_void_p), C.c_int])(C.byref(ctx), dev), 'cuDevicePrimaryCtxRetain')
                pointers = []
                try:
                    check(bind(cuda, 'cuCtxSetCurrent', [C.c_void_p])(ctx), 'cuCtxSetCurrent')
                    ptx = C.create_string_buffer(PTX.encode())
                    check(bind(cuda, 'cuModuleLoadData', [C.POINTER(C.c_void_p), C.c_void_p])(C.byref(module), ptx), 'cuModuleLoadData')
                    check(bind(cuda, 'cuModuleGetFunction', [C.POINTER(C.c_void_p), C.c_void_p, C.c_char_p])(C.byref(fun), module, b'fsb_matmul'), 'cuModuleGetFunction')
                    # Varied inputs detect incorrect indexing as well as a failed launch.
                    a = (C.c_int32 * 1024)(*(i % 7 for i in range(1024)))
                    b = (C.c_int32 * 1024)(*(i % 5 for i in range(1024)))
                    output = (C.c_int32 * 1024)()
                    for data in (output, a, b):
                        ptr = C.c_uint64()
                        check(bind(cuda, 'cuMemAlloc_v2', [C.POINTER(C.c_uint64), C.c_size_t])(C.byref(ptr), C.sizeof(data)), 'cuMemAlloc_v2')
                        pointers.append(ptr)
                        check(bind(cuda, 'cuMemcpyHtoD_v2', [C.c_uint64, C.c_void_p, C.c_size_t])(ptr, data, C.sizeof(data)), 'cuMemcpyHtoD_v2')
                    params = (C.c_void_p * 3)(*(C.addressof(x) for x in pointers))
                    launch = bind(cuda, 'cuLaunchKernel', [C.c_void_p] + [C.c_uint] * 7 + [C.c_void_p, C.c_void_p, C.c_void_p])
                    check(launch(fun, 8, 1, 1, 128, 1, 1, 0, None, params, None), 'cuLaunchKernel')
                    check(bind(cuda, 'cuCtxSynchronize', [])(), 'cuCtxSynchronize')
                    check(bind(cuda, 'cuMemcpyDtoH_v2', [C.c_void_p, C.c_uint64, C.c_size_t])(output, pointers[0], C.sizeof(output)), 'cuMemcpyDtoH_v2')
                    reference = [sum(a[row * 32 + k] * b[k * 32 + col] for k in range(32)) for row in range(32) for col in range(32)]
                    assert list(output) == reference, 'GPU matmul differs from CPU reference'
                finally:
                    for ptr in pointers:
                        check(bind(cuda, 'cuMemFree_v2', [C.c_uint64])(ptr), 'cuMemFree_v2')
                    if module.value:
                        check(bind(cuda, 'cuModuleUnload', [C.c_void_p])(module), 'cuModuleUnload')
                    check(bind(cuda, 'cuDevicePrimaryCtxRelease_v2', [C.c_int])(dev), 'cuDevicePrimaryCtxRelease_v2')
        finally:
            check(bind(nvml, 'nvmlShutdown', [])(), 'nvmlShutdown')
        assert len(set(info['uuids'])) == expected, 'Duplicate physical GPU UUID'
    else:
        assert not glob.glob('/dev/nvidia[0-9]*'), info
    print('FSB_GPU_DRIVER_OK ' + json.dumps(info, sort_keys=True), flush=True)
    time.sleep(hold)


if __name__ == '__main__':
    parser = argparse.ArgumentParser()
    parser.add_argument('--expect-count', type=int, default=1)
    parser.add_argument('--hold-seconds', type=int, default=300)
    args = parser.parse_args()
    run(args.expect_count, args.hold_seconds)
