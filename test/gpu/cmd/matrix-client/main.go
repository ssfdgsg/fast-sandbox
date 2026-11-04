package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	pb "fast-sandbox/api/proto/v2"
	gputest "fast-sandbox/test/gpu"
)

func main() {
	endpoint := flag.String("endpoint", "", "FastPath endpoint")
	namespace := flag.String("namespace", "fsb-gpu-matrix", "test namespace")
	pool := flag.String("pool", "gpu-matrix-pool", "GPU Pool")
	cpuPool := flag.String("cpu-pool", "cpu-test-pool", "CPU Pool")
	mode := flag.String("mode", "burst", "burst, contracts, stale-uid or one")
	image := flag.String("image", "docker.io/library/python:3.11-slim-bookworm", "image")
	name := flag.String("name", "same-name", "Sandbox name")
	expectError := flag.Bool("expect-error", false, "expect a rejected create")
	oldUID := flag.String("old-uid", "", "old UID to fence")
	flag.Parse()
	result := map[string]any{"mode": *mode}
	fail := func(err error) {
		result["error"] = err.Error()
		b, _ := json.Marshal(result)
		fmt.Println(string(b))
		os.Exit(1)
	}
	conn, err := grpc.NewClient(*endpoint, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		fail(err)
	}
	defer conn.Close()
	client := pb.NewFastPathServiceClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	r := qualification{ctx: ctx, client: client, result: result, fail: fail,
		namespace: *namespace, pool: *pool, cpuPool: *cpuPool, image: *image,
		name: *name, oldUID: *oldUID, expectError: *expectError}
	switch *mode {
	case "one":
		r.one()
	case "contracts":
		r.contracts()
	case "stale-uid":
		r.staleUID()
	case "burst":
		r.burst()
	default:
		fail(fmt.Errorf("unknown mode"))
	}

	result["status"] = "PASS"
	b, _ := json.Marshal(result)
	fmt.Println(string(b))
}

type qualification struct {
	ctx                                           context.Context
	client                                        pb.FastPathServiceClient
	result                                        map[string]any
	fail                                          func(error)
	namespace, pool, cpuPool, image, name, oldUID string
	expectError                                   bool
}

func (r *qualification) one() {
	request := &pb.CreateSandboxRequest{Namespace: r.namespace, PoolRef: r.pool, RequestId: r.name, Image: r.image, Command: []string{"python", "-u", "-c", gputest.DriverProbe, "--expect-count", "1", "--hold-seconds", "600"}, Completion: pb.CreateCompletion_CREATE_COMPLETION_READY}
	callCtx, done := context.WithTimeout(r.ctx, 15*time.Second)
	defer done()
	value, e := r.client.CreateSandbox(callCtx, request)
	if r.expectError {
		if e == nil {
			r.fail(fmt.Errorf("create unexpectedly succeeded"))
		}
		r.result["expected_error"] = status.Code(e).String()
	} else {
		if e != nil {
			r.fail(e)
		}
		r.result["uid"] = value.GetSandbox().GetIdentity().GetUid()
	}
}

func (r *qualification) contracts() {
	gpu, err := r.client.GetPool(r.ctx, &pb.GetPoolRequest{Namespace: r.namespace, PoolName: r.pool})
	if err != nil {
		r.fail(err)
	}
	cpu, err := r.client.GetPool(r.ctx, &pb.GetPoolRequest{Namespace: r.namespace, PoolName: r.cpuPool})
	if err != nil {
		r.fail(err)
	}
	if gpu.GetSandboxResourceProfile().GetGpus() != 1 || cpu.GetSandboxResourceProfile().GetGpus() != 0 {
		r.fail(fmt.Errorf("wrong GPU counts"))
	}
	pools, err := r.client.ListPools(r.ctx, &pb.ListPoolsRequest{Namespace: r.namespace})
	if err != nil {
		r.fail(err)
	}
	counts := map[string]int32{}
	for _, p := range pools.Items {
		counts[p.Name] = p.GetSandboxResourceProfile().GetGpus()
	}
	gpuCount, hasGPU := counts[r.pool]
	cpuCount, hasCPU := counts[r.cpuPool]
	if !hasGPU || !hasCPU || gpuCount != 1 || cpuCount != 0 {
		r.fail(fmt.Errorf("ListPools mismatch: %v", counts))
	}
	r.result["gpu_count"] = 1
	r.result["cpu_count"] = 0
	r.result["list_counts"] = counts
}

func (r *qualification) staleUID() {
	ref := &pb.SandboxReference{NamespacedName: &pb.NamespacedName{Namespace: r.namespace, Name: r.name}, ExpectedUid: r.oldUID}
	_, err := r.client.GetSandbox(r.ctx, &pb.GetSandboxRequest{Sandbox: ref})
	if err == nil {
		r.fail(fmt.Errorf("old UID accessed replacement"))
	}
	r.result["old_uid_error"] = status.Code(err).String()
	ref.ExpectedUid = ""
	current, err := r.client.GetSandbox(r.ctx, &pb.GetSandboxRequest{Sandbox: ref})
	if err != nil {
		r.fail(err)
	}
	if current.GetSandbox().GetIdentity().GetUid() == r.oldUID {
		r.fail(fmt.Errorf("replacement has old UID"))
	}
	r.result["new_uid"] = current.GetSandbox().GetIdentity().GetUid()
}

func (r *qualification) burst() {
	type item struct {
		Name  string `json:"name"`
		UID   string `json:"uid,omitempty"`
		Error string `json:"error,omitempty"`
	}
	results := make(chan item, 20)
	var wg sync.WaitGroup
	request := func(name string) *pb.CreateSandboxRequest {
		return &pb.CreateSandboxRequest{Namespace: r.namespace, PoolRef: r.pool, RequestId: name, Image: r.image, Command: []string{"python", "-u", "-c", gputest.DriverProbe, "--expect-count", "1", "--hold-seconds", "600"}, Completion: pb.CreateCompletion_CREATE_COMPLETION_READY}
	}
	for i := range 20 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			n := fmt.Sprintf("burst-%02d", i)
			response, e := r.client.CreateSandbox(r.ctx, request(n))
			v := item{Name: n}
			if e != nil {
				v.Error = status.Code(e).String()
			} else {
				v.UID = response.GetSandbox().GetIdentity().GetUid()
			}
			results <- v
		}(i)
	}
	wg.Wait()
	close(results)
	items := []item{}
	successes := []item{}
	for v := range results {
		items = append(items, v)
		if v.Error == "" {
			successes = append(successes, v)
		}
	}
	r.result["requests"] = items
	r.result["success_count"] = len(successes)
	if len(successes) != 2 {
		r.fail(fmt.Errorf("expected exactly 2 successful creates, got %d", len(successes)))
	}
	for _, v := range successes {
		response, e := r.client.CreateSandbox(r.ctx, request(v.Name))
		if e != nil {
			r.fail(e)
		}
		if response.GetSandbox().GetIdentity().GetUid() != v.UID {
			r.fail(fmt.Errorf("idempotent retry changed UID"))
		}
	}
	_, err := r.client.CreateSandbox(r.ctx, request("burst-overflow"))
	if err == nil {
		r.fail(fmt.Errorf("overflow create succeeded"))
	}
	r.result["overflow_error"] = status.Code(err).String()
}
