# pbotd implementation record

Implementation baseline: [pbotd-design-spec.md](pbotd-design-spec.md), [PBS compatibility](docs/compatibility.md), and [operations](docs/operations.md), updated on 2026-09-18.
The module is `github.com/decfrr/pbotd`. Implementation and automated validation are complete. The checked items below record the delivered scope; hardware-specific smoke tests remain opt-in as documented in [testing](docs/testing.md).

## Reference review

Reviewed `slotd` at commit [`219bdb88d56f43ef25a7facfa7caf5d7de0cea10`](https://github.com/ymgaq/slotd/tree/219bdb88d56f43ef25a7facfa7caf5d7de0cea10): command dispatch, directive parsing, daemon scheduling, allocation accounting, process launch/cancellation, restart recovery, and integration tests.

Useful patterns are the single binary with command aliases, local JSON IPC, SQLite persistence, detached execution, and a test harness with an isolated runtime directory. Its interactive Slurm commands, allocations/steps, and accounting formats are outside the pbotd scope.

The reference's [launch implementation](https://github.com/ymgaq/slotd/blob/219bdb88d56f43ef25a7facfa7caf5d7de0cea10/src/runtime/runner_launch.rs) records the running state after spawning; its [resource accounting](https://github.com/ymgaq/slotd/blob/219bdb88d56f43ef25a7facfa7caf5d7de0cea10/src/store/resource_accounting.rs) counts `RUNNING` jobs. For pbotd, persist reservations before spawning and retain them through process cleanup. Its [recovery tests](https://github.com/ymgaq/slotd/blob/219bdb88d56f43ef25a7facfa7caf5d7de0cea10/tests/recovery.rs) permit either completion or failure after restart; pbotd tests should require the actual exit result when the runner survives.

## Selected design rules

1. **Include recovery in the first working milestone.** Implement the documented launch/recovery protocol before adding GPU execution. A daemon crash between allocation, spawn, and PID persistence must not launch the same work twice.
2. **Account for every live reservation.** CPU, memory, GPU, and array concurrency accounting must include `STARTING`, `RUNNING`, and `EXITING`. Release only after execution has stopped, including remaining managed child processes.
3. **Separate the supervisor from the workload's process group.** The runner survives `qdel` and timeout signals, reaps the workload, and atomically publishes its result. It enforces walltime even while the daemon is down. Cancellation grace periods must not block the scheduler.
4. **Identify execution attempts explicitly.** Store an attempt ID, runner/workload process identities, and Linux boot ID alongside `/proc` start times. Scope launch metadata and result files to the attempt so an old result cannot finish a rerun.
5. **Use GPU UUIDs throughout allocation and execution.** Set `CUDA_VISIBLE_DEVICES` to full allocated UUIDs and use indices for display. NVIDIA documents UUID support; this keeps execution identifiers consistent with persisted ownership. For zero-GPU jobs, explicitly set an empty value. See the [CUDA environment variable reference](https://docs.nvidia.com/cuda/cuda-programming-guide/05-appendices/environment-variables.html).
6. **Keep extension work after the core acceptance criteria.** Optional cgroups, affinity, notification hooks, and detailed accounting follow the usable batch scheduler. Add docs and tests with each milestone.

## Settled product decisions

Use these choices directly during implementation; the linked contracts contain the detailed rules.

| Decision | Selected behavior |
| --- | --- |
| Go module path | `github.com/decfrr/pbotd`; binary and runtime name `pbotd`. |
| Working directory, shell, and output | Submission directory and configured shell (Bash by default); `<name>.o<sequence>` / `<name>.e<sequence>` logs; create missing parents and append on all attempts. Append `.<index>` to array output paths. |
| Queues and resource defaults | Shared `cpu`/`gpu` queues; one CPU, 1 GiB memory, zero GPUs, unlimited walltime. Infer queue from explicit GPU requests; `-q gpu` defaults to one GPU. |
| PBS subset | Literal quoted directive arguments, no multiline continuations, field-wise resource merging with CLI precedence, documented array/ID/state syntax, explicit errors for unsupported options. |
| `qrerun` | Running ordinary jobs and individual subjobs only; same logical ID and immutable submission snapshot, fresh execution attempt, preserved logs. Reject parent/terminal reruns; resubmit terminal work with `qsub`. |
| GPU discovery failure | Static UUID inventory is an explicit provider/fallback. Under `avoid`, unknown occupancy blocks GPU starts; only explicit `ignore` permits unchecked operation. Preserve active allocations regardless of probe failure. |
| Dependency failure | Reject unknown IDs. An unsuccessful terminal predecessor causes `FAILED` / `DependencyFailed` without execution; dependents wait through a running predecessor's rerun. |
| Priority and delayed start | Submission-order backfill plus two-hour `reservation_after`; user priorities and delayed starts are deferred beyond v0.1. |

## Implementation order

### 0. Document the compatibility contract (complete)

- [x] Resolve the module path and initial submission/default behavior.
- [x] Record supported PBS options and deliberate differences in `docs/compatibility.md`.
- [x] Define state transitions, cancellation precedence, public job IDs, and execution-attempt metadata.
- [x] Define the launch handshake and recovery actions before implementing process launch.

The compatibility contract is implemented and covered by the tests listed below.

### 1. Build the daemon and persistence foundation

- [x] Initialize `github.com/decfrr/pbotd` and `cmd/pbotd`; add command alias dispatch and direct `pbotd <command>` dispatch for convenient testing.
- [x] Add TOML configuration, XDG paths and unset-variable fallbacks, private state/job directories, and explicit test path overrides.
- [x] Add model/state/resource types and the `jobs`, `attempts`, `allocations`, and `events` SQLite schema using a pure Go driver, WAL, foreign keys, and migrations.
- [x] Keep job state, active reservations, and events consistent in transactions. Enforce unique active GPU UUID ownership in the database; keep CPU/memory accounting independent of the number of GPU rows.
- [x] Add Unix socket JSON requests/responses, protocol versioning, same-UID peer checks, socket permissions, request size limits, and read/write deadlines.
- [x] Acquire a lock for the state directory before recovery or socket replacement. Prevent competing daemons even if clients use different socket paths.
- [x] Route all scheduling/state mutations through one owner goroutine; perform slow IPC, GPU probes, and process waits outside that loop.
- [x] Add `pbotd daemon --foreground` and basic `pbotd status`.

Checks: database migration/transaction rollback, conflicting GPU reservation rejection, duplicate-daemon startup, and malformed IPC handling without killing or blocking the daemon.

### 2. Complete the CPU job lifecycle, including restart recovery

- [x] Implement the initial `#PBS`/CLI parser: `-N`, `-q`, single-chunk `-l`, `-o`, `-e`, `-j`, `-V`, and `-v`; CLI values take precedence over directives.
- [x] Parse memory and walltime units without overflow; reject unsupported options, multiple chunks, and permanently impossible resource requests.
- [x] Implement `qsub`, `qstat [-f] [-x]`, and `qdel`, including actionable pending/failure reasons.
- [x] Snapshot the script and submission environment before acknowledging submission; handle interrupted snapshot/DB creation. Do not execute directive text while parsing it.
- [x] Build the job environment explicitly. Apply managed `PBS_*`, `PBOTD_*`, and CUDA values after imported environment values; generate node/GPU files.
- [x] Implement `pbotd runner` with detached lifetime, a separate workload process group, output redirection, process identity metadata, and atomic attempt-specific result files.
- [x] Commit `STARTING` and reservations before spawn. Use a durable handshake/start authorization so recovery can distinguish a waiting runner, a launched workload, and a failed launch.
- [x] Implement asynchronous TERM/grace/KILL cancellation and runner-owned walltime enforcement. Preserve cancellation/timeout reasons across daemon restart.
- [x] Recover queued/held jobs and every active state before scheduling. Read valid results, adopt identified runners, reconcile leftover workloads, and finalize releases idempotently.
- [x] Requeue an interrupted launch only when it is known not to have executed. If execution may have started and no result exists, record an explicit failure/uncertain outcome and reconcile remaining processes before releasing resources.
- [x] Implement cleanup of background children when the main shell exits, preserving the shell result after group cleanup unless cancellation/timeout already won. Respect the documented process-group containment limit.

Checks: submission through completion, nonzero exit, immutable script snapshot, merged/separate output, cancellation of a child tree, forced kill, timeout while the daemon is down, and exact result recovery after restart.

### 3. Add GPU scheduling and deliver the first usable milestone

- [x] Detect usable CPU/memory capacity, apply configured limits/reserves, and support CPU-only hosts.
- [x] Add an injectable GPU provider, a static provider, and `nvidia-smi` inventory/process queries with timeouts and fixture-based parsing tests.
- [x] Implement UUID allow/deny filtering, foreign-process avoidance, periodic refresh, and conservative treatment of missing/unknown devices. Inventory refresh must preserve active ownership.
- [x] Implement work-conserving backfill against CPU, memory, and available GPU UUIDs; reserve all requested resources atomically before runner launch.
- [x] Trigger scheduling on submissions, releases, cancellations, completions, and inventory changes; retain the one-second tick as reconciliation fallback.
- [x] Implement `reservation_after` for an eligible, feasible waiting job. Account for CPU/memory bottlenecks as well as GPU count so smaller jobs cannot indefinitely prevent it from starting.
- [x] Add `pbsnodes -a` and resource/queue diagnostics to `pbotd status` and `pbotd doctor`.
- [x] Add a short README quickstart, example TOML, and CPU/GPU `.pbs` examples.

**Milestone A:** real `qsub → queue → resource reservation → runner → result → release → next job`, with durable recovery. Acceptance criteria 1–5 and 8 from the design spec pass. Target approximately one second from resource release to the next launch, measured under a controlled smoke test.

### 4. Add queue control

- [x] Implement `qsub -h`, `qhold`, and `qrls`, including persistence across restart.
- [x] Implement `qalter` for queued/held jobs only; revalidate changes against total capacity and apply them atomically.
- [x] Implement the documented invalid/terminal-state errors and per-target behavior for mixed-success operations on multiple job IDs.
- [x] Add CLI help matching the compatibility contract.

Checks: hold prevents launch, release resumes scheduling, held jobs survive restart, and invalid alterations leave the job unchanged.

### 5. Add arrays, dependencies, and reruns

- [x] Implement `-J` parsing, parent/subjob IDs, `PBS_ARRAY_INDEX`, and durable `%N` concurrency limits; the parent must not reserve resources itself.
- [x] Implement the documented array status aggregation and parent/subjob targeting for query, delete, hold, release, alteration, and rerun.
- [x] Add the `.<index>` suffix to default and explicit array output paths.
- [x] Implement `-W depend=afterok:...`, including multiple predecessors, unsuccessful predecessor behavior, and the supported array dependency semantics.
- [x] Implement running-job `qrerun` with a new attempt ID and fresh queue position. Fully stop the previous attempt before releasing/reallocating resources; ignore stale completion messages/results.
- [x] Preserve the history needed to distinguish attempts without adding a full accounting subsystem.

**Milestone B:** all eight design-spec acceptance criteria pass, together with combined array/dependency/restart cases. This is the v0.1 functional scope.

### 6. Finish installation and operational documentation

- [x] Provide a rootless build/install path for the binary and aliases. Do not overwrite existing PBS executables silently.
- [x] Implement concurrency-safe daemon autostart using the same singleton lock; support foreground use without systemd for Linux/WSL.
- [x] Provide an optional systemd user unit whose stop/restart behavior preserves running jobs; verify this explicitly with the final process/cgroup layout.
- [x] Complete `pbotd doctor`: configuration, socket, database, GPU availability, and degraded probe capability diagnostics.
- [x] Finish the minimal docs and Linux CI checks below.

### 7. Optional features completed after the core milestone

- [x] Delegated cgroup v2 CPU/memory enforcement and whole-cgroup cleanup, with distinct automatic fallback and explicitly-required modes.
- [x] CPU affinity respecting the process's permitted CPU set.
- [x] Completion hooks with a documented environment and failure policy.
- [x] Resolve the accounting scope: retain per-attempt CPU time, maximum child RSS, cgroup memory peak, timestamps, and raw JSON history. Continuous sampling, GPU utilization, and a separate accounting subsystem remain outside the selected scope.

## Minimal documentation and tests

Keep the existing README, compatibility, operations, and design documents synchronized with implementation. Add actual install/test instructions and example configuration/scripts as the corresponding features become available.

Use Go's standard testing tools, table-driven parser/scheduler tests, and a small integration harness that builds the binary and runs a real daemon/runner in temporary directories. Fake only hardware discovery and time where useful; exercise SQLite, IPC, and Linux process behavior directly.

| Test group | Minimum assertions |
| --- | --- |
| PBS parsing/environment | CLI precedence, quoting, repeated resources, units, invalid chunks/options, environment export, protected runtime values, and output behavior. |
| Scheduling | CPU/memory/GPU capacity, backfill, starvation prevention, foreign GPUs, and reservations retained during startup/termination. |
| One/two GPUs | Three one-GPU jobs serialize on one device; two run concurrently on two devices; the third starts after release. Assert actual execution overlap and UUID ownership, not just final states. |
| Lifecycle | Normal/nonzero exit, failed launch, cancellation of descendants, TERM-resistant workload, walltime, and resource release only after cleanup. |
| Recovery | Kill the daemon before/after launch authorization, after spawn but before PID acknowledgement, while running/exiting, and after result publication but before DB commit. Assert no duplicate workload or lost reservation; verify PID/boot/attempt mismatch handling. |
| Arrays/dependencies | `%N` across restart; `afterok` starts only after success; failure/cancellation does not unblock it; stale rerun results cannot finish the new attempt. |
| Daemon/CLI | Singleton lock, autostart race, private socket, malformed requests, and end-to-end command output/status. |

Run `go test ./...`, `go test -race ./...`, and `go vet ./...` in Linux CI, plus a `CGO_ENABLED=0` build. Linux-specific tests are required because `/proc`, peer credentials, and process-group behavior are part of the contract; a macOS-only run is insufficient. Keep real NVIDIA, WSL, delegated-cgroup, and systemd smoke tests opt-in and document their commands. Real CUDA execution should verify that the selected UUIDs match visible devices.

Use synchronization markers and deadline-based polling in process tests instead of long fixed sleeps. Keep a separate timing smoke test for the approximate one-second scheduling target rather than making every integration test depend on exact wall-clock timing.

## Package boundaries

Start with the spec's `cmd/pbotd` and `internal/{cli,ipc,pbs,model,store,scheduler,resource,gpu,executor}` boundaries as those responsibilities are implemented. Keep daemon orchestration small; split out `internal/recovery` only when the recovery implementation benefits from its own package. Avoid empty package scaffolding and one-use helper layers. Interfaces are most useful around GPU discovery, process observation, and the scheduler clock, where they enable meaningful tests.

## Completion evidence

| Delivered area | Verification |
| --- | --- |
| Configuration, PBS syntax, resources | `internal/{config,pbs,resource}/*_test.go` |
| Persistence and admission | `internal/{store,scheduler}/*_test.go` |
| GPU discovery and bounded failure | `internal/gpu/provider_test.go`; `tests/integration/gpu_linux_test.go` |
| Runner, descendants, identity, walltime | `internal/executor/*_test.go`; lifecycle/recovery integration tests |
| Crash boundaries and control precedence | `tests/integration/recovery_linux_test.go`; `internal/daemon/recovery_linux_test.go` |
| Arrays, dependencies, reruns, outputs | `tests/integration/{arrays,lifecycle,output}_linux_test.go` |
| Hooks, affinity, diagnostics, singleton/autostart | `tests/integration/operations_linux_test.go` |
| Actual cgroup v2 and systemd user service | Opt-in tests executed in isolated Linux containers; commands in [testing](docs/testing.md) |
| Distribution | `scripts/install.sh`, `tests/install.sh`, `contrib/systemd/pbotd.service`, `.github/workflows/ci.yml` |

Normal and race-enabled Linux suites, vet, a CGO-free build, and installer checks pass. Rootless execution was verified as UID 65534 and UID 1000. Real NVIDIA/CUDA and WSL smoke tests are supplied/documented but were not executed in this environment. The core one/two-GPU acceptance tests use synthetic UUID inventory with real workload processes.
