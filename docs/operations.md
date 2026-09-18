# Operations contract

[日本語での導入・一般ユーザー向け案内](../README.ja.md)

This document describes the implemented v0.1 behavior. Build with `make build`; install without sudo using `./scripts/install.sh`. See [testing](testing.md) for reproducible verification.

## Configuration

Use `$XDG_CONFIG_HOME/pbotd/config.toml`, falling back to `~/.config/pbotd/config.toml`. The following is the complete default configuration. Reject unknown keys and invalid values at startup. Read configuration once per daemon start; restart to apply changes.

```toml
[scheduler]
policy = "backfill"
tick = "1s"
reservation_after = "2h"

[resources]
cpus = "auto"
memory = "auto"
memory_reserve = "2GiB"

[defaults]
ncpus = 1
mem = "1GiB"
walltime = "unlimited"

[gpu]
provider = "nvidia-smi"
foreign_process_policy = "avoid"
allow_uuids = []
deny_uuids = []
static_uuids = []
refresh = "5s"
probe_timeout = "2s"

[execution]
shell = "/bin/bash"
term_grace = "10s"
cgroup = "off"
cgroup_base = ""
cpu_affinity = false
completion_hook = []
hook_timeout = "10s"

[daemon]
autostart = true
```

`resources.cpus` accepts `"auto"` or a positive integer. Auto uses the process's allowed logical CPU set and accounts for an enclosing CPU quota. `resources.memory` accepts `"auto"` or a positive size string. Auto uses the smaller of host physical memory and any enclosing memory limit, then subtracts `memory_reserve`; an explicit memory value is already the schedulable budget and is not reduced again. Reject a nonpositive result. Reservations do not track fluctuating free RAM or external CPU usage.

The `defaults` table defines per-job requests, separate from host capacity. Its walltime accepts `"unlimited"` or a positive `HH:MM:SS`/integer-seconds string. A configuration whose default job does not fit its CPU/memory budget is an error. `scheduler.policy` accepts only `"backfill"`; `reservation_after = "0s"` disables starvation prevention. GPU refresh, probe timeout, and hook timeout must be positive. `term_grace = "0s"` requests immediate escalation.

## GPU discovery and failure behavior

GPU exclusivity is mandatory. Full physical GPU UUIDs identify devices; indices are display metadata. Empty `allow_uuids` permits all discovered devices, and deny always wins. Search `PATH`, `/usr/bin/nvidia-smi`, `/usr/lib/wsl/lib/nvidia-smi`, and `/bin/nvidia-smi` for discovery. `gpu.command` optionally selects an explicit executable. Query UUID, index, model, total device memory, and compute processes. MIG devices/partitions are unsupported.

`gpu.provider` accepts `"nvidia-smi"` or `"static"`. `static_uuids` is a list of full physical GPU UUIDs, for example:

```toml
[gpu]
provider = "static"
static_uuids = ["GPU-8932f937-d72c-4106-c12f-20bd9faed9f6"]
foreign_process_policy = "ignore"
```

With the default provider, a failed inventory probe falls back to configured static UUIDs if present. Otherwise retain the last inventory for identity/capacity reporting and mark its devices unavailable; do not release active reservations. With no known inventory, CPU jobs continue and GPU jobs wait with `GPUUnavailable`. A successful discovery of zero allowed devices establishes zero GPU capacity, so new GPU requests are rejected.

`foreign_process_policy = "avoid"` requires a fresh successful compute-process query before a new GPU launch, including when inventory is static. Existing pbotd workload processes are recognized by stored process identities; occupied or unclassifiable devices are unavailable to new work. Failed/unsupported queries are unknown, never an empty process list. If query capabilities are missing on a WSL installation, `doctor` reports that condition explicitly.

The explicit `"ignore"` policy permits scheduling without foreign-process checks, including static operation when `nvidia-smi` is absent. It does not weaken exclusive ownership between pbotd jobs. Static device existence is the user's configuration responsibility. Missing model/index/memory metadata is displayed as unknown. Automatic fallback does not change the configured policy.

Polling and launch checks cannot prevent a process outside pbotd from starting a competing GPU workload immediately afterward. Environment-based GPU allocation is cooperative.

## Paths and daemon lifecycle

| Item | Location |
| --- | --- |
| Binary / aliases | `~/.local/bin/pbotd` and PBS command symlinks |
| Configuration | `$XDG_CONFIG_HOME/pbotd/config.toml` |
| State | `$XDG_STATE_HOME/pbotd/<hostname>/pbotd.db` |
| Job snapshots | `$XDG_STATE_HOME/pbotd/<hostname>/jobs/<job-id>/` |
| Attempt metadata/results | `jobs/<job-id>/attempts/<attempt-id>/` |
| Socket | `$XDG_RUNTIME_DIR/pbotd/pbotd.sock` |

Unset XDG config/state paths default to `~/.config` and `~/.local/state`. Without `XDG_RUNTIME_DIR`, use `run/pbotd.sock` under the host state directory. The state directory is host-specific while public IDs retain the fixed `.localhost` suffix. Require local storage for the SQLite database. `PBOTD_CONFIG`, `PBOTD_STATE_DIR`, and `PBOTD_SOCKET` override the exact config file, host state directory, and socket path for isolated deployments/tests; autostarted daemons inherit them.

Internal directories use mode `0700`, internal files and the socket use `0600`, and IPC checks the peer UID. Output directories/files created by pbotd use `0700`/`0600` subject to the process umask; existing permissions are retained. Credentials in exported environments remain private and must not appear in diagnostic logs or routine status output.

`pbotd daemon` runs in the foreground; `--foreground` is an explicit synonym. Client autostart spawns a detached daemon when no instance is running. All startup paths acquire the same state-directory lock before replacing a stale socket or recovering jobs. A live but unresponsive daemon is an error, not a reason to spawn another. Protocol/version errors never trigger autostart.

Stopping the daemon stops scheduling and closes its socket; runners continue jobs and enforce walltime. For foreground operation, run `pbotd daemon --foreground` and stop it with Ctrl-C. The private `daemon.json` records its PID, boot ID, and start time for diagnostics; `daemon.lock` is the authoritative singleton lock. No automatic history deletion is performed.

To use the optional user service, first stop any foreground/autostarted daemon, set `daemon.autostart = false`, then:

```sh
mkdir -p ~/.config/systemd/user
cp contrib/systemd/pbotd.service ~/.config/systemd/user/
systemctl --user daemon-reload
systemctl --user enable --now pbotd.service
systemctl --user restart pbotd.service
journalctl --user -u pbotd.service
```

The unit deliberately uses `KillMode=process`: systemd stops only the daemon, preserving runners and workloads for adoption after restart. This differs from normal service-wide cleanup, as described by [systemd's kill-mode documentation](https://www.freedesktop.org/software/systemd/man/latest/systemd.kill.html). Use `qdel` to cancel jobs. User logout/host shutdown policies may still stop all user processes; enable user lingering through your system administrator when required. The supplied unit does not configure cgroup delegation.

## Optional execution controls

`execution.cpu_affinity = true` assigns each active job distinct logical CPUs from the daemon's permitted CPU set. Child processes inherit the mask; a cooperative job can change its own affinity.

`execution.cgroup` accepts `off`, `auto`, or `required`. Set `cgroup_base` to an existing delegated cgroup v2 subtree with `cpu` and `memory` enabled in `cgroup.subtree_control`. The caller must have permission to create child cgroups and migrate workloads from the runner's subtree. pbotd does not create delegation, move unrelated processes, or enable host controllers. See the kernel's [delegation and no-internal-process rules](https://docs.kernel.org/admin-guide/cgroup-v2.html).

Each attempt receives a child with `cpu.max`, `memory.max`, and `memory.oom.group=1`; the workload enters it before script execution. Cancellation cleans the entire subtree, including descendants that call `setsid`. The runner remains outside the job cgroup. `auto` falls back to process groups and records the reason in the attempt's `warnings.json`; `required` fails the job before execution if setup fails. An OOM kill is reported as `OutOfMemory`. Without cgroups, descendants that deliberately leave the process group are outside containment.

`execution.completion_hook` is an argv array beginning with an absolute executable path; arguments are literal. Hooks run once at most for each completed execution attempt, including failures and attempts ended by rerun. Array parents and jobs cancelled/failed before any attempt do not emit hooks. Enabling hooks also processes retained attempts with no prior hook claim. Hooks run after resource release and cannot change job outcomes.

Hooks execute in the submission directory with a minimal `PATH`, managed PBS identity variables, `PBOTD_ATTEMPT_ID`, `PBOTD_JOB_STATE`, `PBOTD_JOB_REASON`, `PBOTD_JOB_SIGNAL`, and `PBOTD_JOB_EXIT_CODE` when known. They do not inherit the submitted environment or GPU allocation. Their detached supervisor enforces `hook_timeout` even while the daemon is stopped, with 100 ms TERM grace. Output/results are under `attempts/<attempt-id>/hook/`; `qstat -f --json` exposes hook state. A durable claim precedes spawn: a crash in that gap can lose a notification, and claimed hooks are never retried automatically. Hooks should be short and use no scheduler resources.

Attempt results retain elapsed timestamps, user/system CPU time, maximum child RSS, and cgroup memory peak when available. CPU/RSS usage uses Linux `getrusage(RUSAGE_CHILDREN)` after reaping. RSS is a maximum, not a sum across concurrent processes. `qstat -f --json` preserves the raw values; no sampling daemon or separate accounting service is required.

## Diagnostics and validation

`pbotd status` reports daemon connectivity, job counts, and resource availability. `pbotd doctor` checks configuration, directory/socket permissions, SQLite access/schema, GPU probe support, and degraded capabilities without launching workloads or changing job state. `pbsnodes -a` includes unavailable-device reasons. Requests that can never fit have a submission error; temporary capacity/configuration reductions leave existing jobs pending with a visible reason and do not kill running jobs.

`doctor` opens SQLite read-only, checks schema 1 and `quick_check`, and verifies private ownership/permissions without repairing them. It returns nonzero for invalid configuration, unsafe/missing runtime paths, database failure, or daemon connection failure. Missing NVIDIA support is reported as a degraded GPU capability; a valid CPU-only runtime remains usable. `doctor` and `status` never autostart the daemon. Use `--json` for machine-readable diagnostics.

Linux CI runs unit/integration tests, the race detector, `go vet`, a CGO-free build, and installer checks. Real NVIDIA/CUDA, WSL, cgroup delegation, and systemd checks are opt-in; commands and environment requirements are in [testing](testing.md). A macOS-only test run cannot validate Linux process/IPC behavior.

IPC requests are limited to 8 MiB (scripts to 4 MiB); responses allow 128 MiB for large array listings. Narrow `qstat -f` to specific job IDs if accumulated history exceeds that response limit.
