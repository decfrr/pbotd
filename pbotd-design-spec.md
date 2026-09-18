# pbotd design specification (v0.1)

**PBS On The Desktop** is a rootless, single-host, single-user PBS Professional-style batch scheduler written in Go. Its primary purpose is to queue `.pbs` scripts and start the next job when GPUs become available.

Repository and Go module: `github.com/decfrr/pbotd`. Binary and runtime directory name: `pbotd`.

This specification records the implemented contract as of 2026-09-18. [PBS compatibility](docs/compatibility.md) defines user-visible syntax and defaults; [operations](docs/operations.md) defines configuration and deployment; [TODO.md](TODO.md) records implementation and validation.

## 1. Scope

v0.1 supports Linux/WSL on one host under one UID, without sudo; exclusive whole NVIDIA GPU allocation; CPU/memory reservations; durable script/environment snapshots; SQLite persistence; detached runners and daemon restart recovery; arrays; `afterok` dependencies; hold/release; walltime; and explicit reruns of running jobs.

It excludes multiple hosts/users, separate PBS Server/Scheduler/MOM services, fairshare, advanced accounting, reservations/ACLs, complex queues, multi-node MPI, GPU memory sharing, preemption, MIG management, and full PBS compatibility. User priority, delayed starts, interactive jobs, and automatic retries are also outside v0.1. Optional delegated cgroup v2 enforcement, CPU affinity, completion hooks, and per-attempt usage are implemented after the core scheduler; their configuration is in operations.

## 2. Architecture

```text
PBS aliases or pbotd <command>
             |
    versioned JSON / Unix socket
             |
        pbotd daemon
    +---------------------------+
    | parser and IPC handlers   |
    | CPU/RAM/GPU inventory     |
    | scheduler owner goroutine |
    | SQLite WAL                |
    +---------------------------+
             |
       detached pbotd runner (one per attempt)
             |
       workload process group
             |
       configured shell <snapshot.pbs>
```

One goroutine owns scheduling decisions and state mutations. Slow client IO, GPU probes, and process waits run outside that loop and return events. The daemon is the only SQLite writer; runners publish attempt metadata and results to private files and notify the daemon when reachable. Durable files are authoritative across disconnection; notifications accelerate reconciliation.

The runner survives daemon termination. It supervises a separate workload process group so job cancellation does not kill the process responsible for cleanup and result publication.

## 3. Layout and IPC

```text
Binary:   ~/.local/bin/pbotd
Aliases:  qsub qstat qdel qhold qrls qalter qrerun pbsnodes
Config:   $XDG_CONFIG_HOME/pbotd/config.toml
Socket:   $XDG_RUNTIME_DIR/pbotd/pbotd.sock
State:    $XDG_STATE_HOME/pbotd/<hostname>/pbotd.db
Jobs:     $XDG_STATE_HOME/pbotd/<hostname>/jobs/<job-id>/
Attempts: <job-dir>/attempts/<attempt-id>/
```

Use the XDG fallbacks and explicit test/deployment overrides in [operations](docs/operations.md). Directories are private (`0700`), internal files/socket use `0600`, and Linux `SO_PEERCRED` rejects a different UID. Versioned IPC has request size limits, deadlines, and structured errors; malformed clients must not terminate or stall the daemon.

A state-directory lock prevents competing daemons, including clients using different socket paths. Acquire it before recovery or stale-socket replacement. Client autostart uses the same lock. Foreground operation requires no systemd.

## 4. Submission and execution contract

```console
qsub train.pbs
123.localhost
```

Snapshot the script and resolved environment before acknowledging a committed job. Resolve CLI fields over `#PBS` fields, then apply defaults. Reject unknown options/resources, multi-chunk selections, invalid values, and requests exceeding known total capacity. Repeated resource options merge by field, as specified in [compatibility](docs/compatibility.md).

```bash
#PBS -N train
#PBS -q gpu
#PBS -l select=1:ncpus=8:mem=32gb:ngpus=1
#PBS -l walltime=24:00:00
#PBS -o logs/train.out
#PBS -e logs/train.err
#PBS -j oe
#PBS -V
#PBS -v KEY=value,OTHER=value
#PBS -J 0-99%4
#PBS -W depend=afterok:123.localhost
#PBS -h
```

Jobs execute in the submission directory with `execution.shell` (default `/bin/bash`). The defaults are one CPU, 1 GiB memory, zero GPUs, and unlimited walltime. The `cpu` and `gpu` queues share host resources; queue inference and the one-GPU default for `-q gpu` are fixed in the compatibility contract. `select` supports exactly one chunk with count `1`.

Output defaults to `<name>.o<sequence>` and `<name>.e<sequence>` in the submission directory. Resolve explicit relative paths there, create missing parent directories, and append on every attempt. Array subjobs append `.<index>` to default and explicit output paths. Freeze paths at submission; changing the job name does not rename output.

Generate the managed PBS environment and node/GPU files after importing the selected submission environment. `PBS_NODEFILE` contains one hostname line per CPU, `PBS_GPUFILE` one full GPU UUID per line. `CUDA_VISIBLE_DEVICES` contains allocated UUIDs or an empty string for CPU-only work. Runtime-owned variables cannot be overridden by `-v`.

## 5. GPU management

Inventory records UUID, current index, model, total device memory, and observed compute processes. Persist ownership by UUID; use indices only as display metadata. Apply allow/deny filters with deny taking precedence. Exclusivity within pbotd is mandatory.

Reserve every requested GPU in the database before starting execution. Keep reservations during `STARTING`, `RUNNING`, and `EXITING`; release only after managed execution is gone. Inventory refresh and failed queries must never erase reservations for active attempts.

The default provider is `nvidia-smi`, refreshed every five seconds with a two-second probe timeout. Before a GPU launch under `foreign_process_policy = "avoid"`, refresh process occupancy. Distinguish pbotd workloads from foreign/unclassifiable processes using stored process identities. Busy, missing, and unknown devices are unavailable for new work.

Static UUID inventory is supported explicitly and as fallback for failed discovery. It does not bypass the configured foreign-process policy. With `avoid`, an unavailable/unsupported process query blocks new GPU starts; only explicit `ignore` permits unchecked static/manual operation. The exact configuration is in [operations](docs/operations.md).

GPU isolation is cooperative. Processes outside pbotd can ignore environment variables or race the occupancy check. No device-level enforcement is promised. NVIDIA supports UUID values and an empty value in [`CUDA_VISIBLE_DEVICES`](https://docs.nvidia.com/cuda/cuda-programming-guide/05-appendices/environment-variables.html).

## 6. Scheduling

Use work-conserving backfill. Iterate eligible queued jobs by `queued_at`, using ID/index as stable tie-breakers, and start each one that fits current CPU/memory/GPU availability. Initial queue entry uses submission time; rerun uses its requeue time, while hold/release preserves queue position. Held jobs and unresolved dependencies are ineligible. There is no user priority or delayed-start mechanism in v0.1.

Run scheduling after submission/control changes, runner completion, and inventory changes. A one-second tick is the reconciliation fallback, not the sole completion detector. Continue until no further job fits.

```text
active = STARTING + RUNNING + EXITING
sum(active.requested_cpus)   <= configured CPUs
sum(active.requested_memory) <= configured memory
at most one active allocation per GPU UUID
active subjobs per array    <= its concurrency limit
```

If configuration is reduced below existing reservations, preserve running jobs and stop further over-budget admission until usage falls. Array parents do not reserve resources.

`reservation_after` defaults to two hours. Protect the oldest eligible waiting job over that age which can fit total configured capacity and the currently usable GPU inventory, including devices held by pbotd jobs. Reserve up to its requested amount of currently free CPU, memory, and GPUs; allow backfill only from the remaining surplus until it can start. Exclude held/dependency-blocked jobs, arrays already at their concurrency limit, and requests made infeasible by configuration/offline devices. This is resource draining, not a predicted start-time reservation. Zero disables it; running jobs are never preempted.

## 7. State and control

```text
submission -> QUEUED <-> HELD
                 |
              STARTING -> RUNNING -> EXITING
                                       |-- normal finish --> terminal outcome
                                       `-- accepted rerun -> QUEUED

terminal outcome = COMPLETED / FAILED / CANCELLED / TIMEOUT
```

The rerun branch leaves `EXITING` after cleanup and does not pass through a terminal job state. Queued/held jobs may directly become `CANCELLED`, or `FAILED` with `DependencyFailed`. Pre-execution launch failure becomes `FAILED` after cleanup. `SUBMITTED` is an event, not a persisted state.

Public `qstat` states are `Q` for queued/starting, `H` for held, `R` for running, `E` for exiting, and `F` for terminal jobs. Detailed status preserves the internal state, outcome, and reason.

`qrerun` accepts only a running ordinary job or individual subjob. Stop the prior attempt fully, preserve its result/history, allocate a fresh attempt ID, and requeue the same logical job at the end of the queue with a fresh waiting-age clock. Preserve its public ID, script, environment, requests, and output paths. Reject array-parent and terminal-job reruns; terminal work is resubmitted using `qsub`. There are no automatic execution retries.

A valid published result takes precedence over a subsequent control request. Otherwise the first accepted termination cause determines cancellation versus timeout; record the cause durably and preserve it during recovery. An accepted rerun is a requeue intent, not job failure. `qdel` during rerun cleanup supersedes requeue and leaves the job cancelled. Stop requests remain asynchronous so the scheduler can service other jobs.

Array aggregation, targeting, `qalter` restrictions, multi-ID errors, and dependency failure rules are fixed in [compatibility](docs/compatibility.md). Dependencies refer to logical jobs; a failed/cancelled/timed-out predecessor fails its dependents without execution. A running predecessor's rerun leaves its dependents waiting.

## 8. Process supervision

The runner is detached from the daemon's terminal/session and remains outside the workload's process group. Save runner and workload PID, PGID, boot ID, and `/proc/<pid>/stat` start times. Check identities before adoption or signalling; PID presence alone is insufficient.

Cancellation sends `SIGTERM` to the workload group, then `SIGKILL` after the configured grace period (default ten seconds). The runner enforces walltime from workload start, including during daemon downtime. Reap the main process and clean up remaining group members before publishing completion or freeing resources. When the main shell exits with background children still in its group, terminate those children using the same grace/escalation procedure and preserve the main shell's exit result unless cancellation/timeout already won.

A process-group supervisor cannot contain a descendant that deliberately creates a new session/group. Optional delegated cgroups contain these descendants and enforce CPU/memory limits. Optional affinity allocates distinct CPUs from the permitted set. Both are disabled by default.

## 9. Persistence, launch, and recovery

Use a pure Go SQLite driver with WAL, foreign keys, versioned migrations, and transactional updates. Four tables keep logical jobs, execution history, current GPU ownership, and events separate:

```text
jobs
  id, public_id (unique), parent_id, state, queued_at, data (JSON)

attempts
  id, job_id, data (JSON)

allocations
  gpu_uuid (unique), job_id, attempt_id, created_at

events
  id, job_id, attempt_id, timestamp, type, message
```

Count CPU/memory from active jobs once per job, independently of GPU allocation rows. Terminal state/result and allocation release commit together. Attempt directories retain launch identity, start authorization, control data, and result metadata; old attempts cannot update the current one. Atomic file publication uses a temporary file, sync, rename, and directory sync where durability is required.

Schema 1 indexes queue order and parent relationships while storing typed job/attempt records as JSON. Job records contain requests, immutable snapshot paths/environment, timestamps, current attempt, control intent, dependencies, and array fields. Attempt records contain authorization, runner/workload identities, PGID, result/usage, and hook state. Foreign keys bind allocation ownership to the matching job and attempt; the GPU UUID primary key prevents duplicate allocation.

Launch protocol:

1. Persist a new attempt, `STARTING`, and all resource reservations in one transaction.
2. Spawn a runner that acquires an exclusive per-attempt lifetime lock, publishes its identity/readiness, and waits for start authorization. Competing runner candidates cannot execute the same attempt.
3. Verify identity, commit authorization, and durably publish it for the runner. A runner without authorization never executes the script.
4. The runner records workload identity before allowing the script to execute, using a gated workload launch. It publishes start metadata; the daemon records `RUNNING`. Lost notifications are reconciled from files.
5. The runner enforces control/walltime, cleans up, and atomically publishes the attempt's `result.json`. The daemon finalizes the job and releases reservations transactionally before scheduling successors.

Recovery runs under the daemon lock before new scheduling:

1. Restore queued/held jobs and examine all starting/running/exiting attempts, not only `RUNNING`.
2. Read valid current-attempt results and reconcile them with durable control intent. Finalize idempotently only after cleanup is established.
3. Adopt live runners only after boot/start-time/attempt identity checks, retaining their reservations; resume pending control requests and incomplete authorization publication.
4. An unauthorized attempt can be requeued only after confirming the old runner/workload is absent or stopped. An authorized attempt with no result is never automatically rerun if execution may have started.
5. If a runner is lost, reconcile and terminate any positively identified remaining workload. Once it is gone, record `FAILED` with `LostRunner`/`UnknownExecution`; retain reservations and report cleanup trouble while ownership remains uncertain.
6. Treat a host reboot as loss of execution, not as a live process with reused PID. Resume scheduling after reconciliation; runners survive daemon restarts, not host reboots.

Remove orphan numeric job directories from interrupted submission without adopting uncommitted jobs. Attempt directories are created only after the corresponding reservation commits, so interrupted attempts remain associated with durable history.

## 10. CLI

```text
pbotd daemon [--foreground]
pbotd status
pbotd doctor
pbotd <PBS-command> [arguments...]

qsub   [supported PBS options] file.pbs
qstat  [-f] [-x] [job-id...]
qdel   job-id...
qhold  job-id...
qrls   job-id...
qalter [-N name] [-l resources] job-id...
qrerun job-id...
pbsnodes -a
```

Aliases dispatch through `argv[0]`; direct subcommands provide identical behavior without installed symlinks. `runner` is an internal command. `qstat -x` includes history; `qalter` changes only queued/held jobs. Full command semantics and exit codes are in [compatibility](docs/compatibility.md).

## 11. Configuration and operations

The canonical default TOML and static GPU example are in [operations](docs/operations.md). Defaults include a one-second scheduler tick, two-hour starvation threshold, two-GiB host memory reserve, one-CPU/one-GiB job request, five-second GPU refresh, two-second probe timeout, Bash execution, ten-second termination grace, and enabled client autostart.

Only `cpu` and `gpu` queues exist; GPU exclusivity is always enabled. Optional cgroup, affinity, and hook settings are documented in operations. A restart applies configuration changes without rewriting stored job requests. Missing runtime-directory environment variables and absence of systemd have documented fallbacks.

## 12. Package layout

```text
cmd/pbotd/               main and argv[0] dispatch
internal/cli/            commands and output
internal/ipc/            Unix socket and versioned JSON
internal/pbs/            directives and resource parsing
internal/model/          jobs, attempts, states, resources
internal/store/          SQLite and migrations
internal/scheduler/      queue, backfill, admission
internal/resource/       CPU/memory inventory
internal/gpu/            nvidia-smi and static providers
internal/executor/       runner, process control, cgroups, affinity
internal/daemon/         owner loop, recovery, controls, hooks
internal/fsutil/         private files, atomic publication, locks
```

Create packages when implemented, without empty scaffolding. Extract `internal/recovery` only if it materially improves readability. Use interfaces where they isolate hardware/process observation or scheduling time for meaningful tests. Target a single distributable binary with `CGO_ENABLED=0`.

## 13. Minimum acceptance criteria

1. With one GPU, three one-GPU jobs run sequentially without overlap and all finish.
2. Under a controlled smoke test, the next job starts approximately within one second of resource release.
3. With two GPUs, two one-GPU jobs run concurrently and a third starts on the next released device.
4. `qdel` terminates the workload and descendants remaining in its process group before releasing GPU ownership.
5. Daemon restart restores the queue, preserves surviving workloads, and records their actual results, including completion while the daemon was down.
6. Array `%N` limits include startup/cleanup and survive restart.
7. `afterok` jobs start only after every predecessor succeeds; unsuccessful predecessors produce `DependencyFailed` without execution.
8. Integration tests prove that the same GPU UUID is never assigned to overlapping active attempts.

Additional regression checks cover launch crash boundaries, duplicate daemon prevention, stale attempt results, walltime during daemon downtime, failed output setup, and configuration/probe failures. Tests use fake GPU discovery with real Linux IPC, SQLite, and runner processes; real hardware smoke tests remain opt-in.

## 14. Implementation sequence

```text
0: Compatibility/defaults/recovery contract (documented)
1: Module, configuration, model, SQLite, IPC, singleton daemon
2: CPU qsub/qstat/qdel, runner, walltime, crash recovery
3: GPU discovery/exclusivity, backfill, starvation prevention
4: Hold/release and queued-job alteration
5: Arrays, afterok dependencies, running-job reruns
6: Installation, autostart, diagnostics, completed operational docs
7: Optional cgroups, affinity, completion hooks, per-attempt usage
```

Add docs/tests with each milestone. Milestone A (through step 3) satisfies acceptance criteria 1–5 and 8; milestone B (through step 5) satisfies all eight. Distribution work completes v0.1. The detailed checklist is [TODO.md](TODO.md).
