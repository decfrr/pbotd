# Testing

## Automated Linux checks

Requires Go 1.27+, Bash, and at least two allowed logical CPUs. No NVIDIA hardware, root privileges, or systemd is required for the default suite.

```sh
go test ./...
go test -race ./...
go vet ./...
CGO_ENABLED=0 go build -trimpath -o bin/pbotd ./cmd/pbotd
sh tests/install.sh
```

The integration harness builds a real binary and isolates configuration, state, socket, scripts, and output in temporary directories. It verifies SQLite/IPC, one/two-GPU overlap using static UUIDs, arrays/dependencies, reruns, cancellation, walltime without a daemon, output/snapshot semantics, hooks, affinity, singleton/autostart races, and launch crash boundaries. The latency smoke check allows 1.5 seconds from release to successor execution under a 50 ms test tick. Linux CI runs the commands above. macOS can build and run portable unit tests, but execution requires Linux.

Run Linux tests from macOS using Docker:

```sh
docker run --rm --init \
  --mount type=bind,source="$PWD",target=/workspace \
  -w /workspace golang:1.27.1 go test -race ./...
```

## Delegated cgroup v2

Provide an empty delegated subtree with `cpu` and `memory` enabled. The test user needs permission to migrate a child from the test process's cgroup into that subtree. The test verifies actual kernel limits and cleanup of a `setsid` descendant:

```sh
PBOTD_TEST_CGROUP_BASE=/sys/fs/cgroup/your-delegation/jobs \
  go test -v -run TestDelegatedCgroupEnforcement ./tests/integration
```

Without that variable this test skips; automatic fallback and required-mode failure tests always run. For an isolated Docker check (privileged container, private cgroup namespace; no host cgroup bind mount):

```sh
docker run --rm --privileged \
  --mount type=bind,source="$PWD",target=/workspace -w /workspace \
  golang:1.27.1 sh -ec '
    mkdir /sys/fs/cgroup/control /sys/fs/cgroup/jobs
    echo $$ > /sys/fs/cgroup/control/cgroup.procs
    echo "+cpu +memory" > /sys/fs/cgroup/cgroup.subtree_control
    echo "+cpu +memory" > /sys/fs/cgroup/jobs/cgroup.subtree_control
    PBOTD_TEST_CGROUP_BASE=/sys/fs/cgroup/jobs \
      go test -v -run TestDelegatedCgroupEnforcement ./tests/integration
  '
```

## systemd user service

With a running user manager:

```sh
PBOTD_TEST_SYSTEMD=1 \
  go test -v -run TestSystemdUserServicePreservesRunners ./tests/integration
```

The test creates a uniquely named temporary unit from the supplied service template, checks restart preserves the same attempt, checks completion while stopped, and removes its unit afterward. It does not modify an installed `pbotd.service`.

An isolated Docker fixture is included:

```sh
docker build -f tests/systemd.Dockerfile -t pbotd-systemd-test .
docker run -d --privileged --name pbotd-systemd-test \
  --mount type=bind,source="$PWD",target=/workspace \
  --tmpfs /run --tmpfs /run/lock -w /workspace pbotd-systemd-test
docker exec pbotd-systemd-test systemctl start user@1000.service
docker exec -w /workspace pbotd-systemd-test runuser -u pbotd-test -- \
  env XDG_RUNTIME_DIR=/run/user/1000 \
  DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/1000/bus \
  GOCACHE=/tmp/pbotd-go-cache GOMODCACHE=/tmp/pbotd-go-modules \
  PBOTD_TEST_SYSTEMD=1 /usr/local/go/bin/go test -v \
  -run TestSystemdUserServicePreservesRunners ./tests/integration
docker stop pbotd-systemd-test
docker rm pbotd-systemd-test
```

## NVIDIA/CUDA and WSL

On a host with NVIDIA drivers and the CUDA toolkit, compile the opt-in visibility test and submit it from the repository root:

```sh
mkdir -p bin
nvcc -std=c++17 tests/cuda-visible.cu -o bin/cuda-visible
printf '%s\n' '#!/bin/bash' 'exec ./bin/cuda-visible' > bin/cuda-visible.pbs
pbotd qsub -q gpu -l ngpus=1,mem=1gb,walltime=60 bin/cuda-visible.pbs
pbotd qstat -f JOB_ID
```

Require `COMPLETED`, exit code zero, and identical allocated/visible UUIDs in stdout. Repeat with `ngpus=2` on a two-GPU host. The test uses CUDA's [device UUID](https://docs.nvidia.com/cuda/cuda-runtime-api/cuda_runtime_api/structcudaDeviceProp.html), allocates device memory, and performs a device operation on each visible GPU. `nvidia-smi` alone does not verify CUDA visibility.

On WSL, run `pbotd doctor --json`, `pbotd pbsnodes -a --json`, the CPU example, and the same CUDA test. Check that `/usr/lib/wsl/lib/nvidia-smi` is discovered and examine compute-process query capability. If occupancy is unsupported, default `avoid` must keep GPU work pending; use `ignore` only when unchecked occupancy is an explicit operational choice. Run the ordinary Linux suite inside WSL to verify filesystem and process behavior there.

## Validation record

The implementation was checked on Linux ARM64 in Docker using Go 1.27.1: normal and race-enabled suites, vet, CGO-free build, installer collision handling, execution as UID 65534, actual cgroup v2 limits/cleanup, and an actual systemd user service as UID 1000. Real NVIDIA/CUDA and WSL execution require those environments and are not claimed by the hardware-independent suite.
