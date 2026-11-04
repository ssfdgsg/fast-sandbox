// Package gputest provides hardware probes embedded in opt-in runtime tests.
package gputest

import _ "embed"

// DriverProbe executes a CUDA kernel using Python's standard library and CDI driver libraries.
//
//go:embed cuda-driver-probe.py
var DriverProbe string
