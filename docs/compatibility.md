# PBS compatibility contract

This is the implemented v0.1 behavior for `github.com/decfrr/pbotd`. It provides a practical PBS-style subset, including deliberate local conveniences; it does not claim complete PBS Professional compatibility. `qstat`, `pbsnodes -a`, `status`, and `doctor` accept `--json` for structured output.

## Submission and resources

| Setting | v0.1 behavior |
| --- | --- |
| Script | One local script file; snapshot its contents at submission. Stdin submission and interactive jobs are unsupported. |
| Job name | `-N`, otherwise the script basename without its final extension. Reject empty names, path separators, and control characters. |
| Working directory | Absolute submission directory, also exported as `PBS_O_WORKDIR`. |
| Shell | Configured `execution.shell`, default `/bin/bash`; the shebang does not override it. |
| CPU | One logical CPU by default; positive integer requests. |
| Memory | 1 GiB by default; a positive byte count. A reservation by default, enforced when delegated cgroups are enabled. |
| GPU | Zero by default; `-q gpu` defaults to one when `ngpus` is omitted. Requests are whole physical GPUs. |
| Queue | `cpu` or `gpu`, both sharing one CPU/memory/GPU pool. If omitted, infer `gpu` from explicit `ngpus > 0`, otherwise `cpu`. Reject GPUs on `cpu`, explicit `ngpus=0` on `gpu`, and unknown queue names. |
| Walltime | Unlimited by default. Explicit `walltime` is positive integer seconds or `HH:MM:SS`; hours may exceed 23, minutes/seconds must be below 60. |
| Capacity | Reject requests exceeding known configured total capacity. Temporary occupancy, unavailable probes, and offline GPUs cause a visible pending reason. |

CPU, memory, and walltime defaults are configurable as documented in [operations](operations.md). Queue behavior and GPU defaults are fixed in v0.1. Resource values are resolved and saved at submission; later configuration changes do not rewrite accepted jobs.

Supported submission options are `-N`, `-q`, `-l`, `-o`, `-e`, `-j`, `-V`, `-v`, `-J`, `-W depend=afterok:...`, and `-h`. Options accepting values support both separated and attached values, such as `-l select=1:ncpus=2` and `-lselect=1:ncpus=2`. `--` ends option parsing. Unsupported flags and resource keys are errors.

Accept `select=1:ncpus=N:mem=M:ngpus=G` and direct `ncpus=N,mem=M,ngpus=G` resource lists. `walltime` is a top-level resource. Normalize both forms into the same fields; merge repeated `-l` arguments by field, with the last explicit value winning within a source. CLI fields override directive fields; omitted fields retain their directive values before defaults are applied. This field-wise merge is a deliberate simplification. Reject `select` other than `1`, multiple chunks separated by `+`, and unknown chunk attributes.

Memory units are case-insensitive: an integer without a suffix means bytes; `b` means bytes; `k/kb/kib`, `m/mb/mib`, `g/gb/gib`, and `t/tb/tib` use powers of 1024. Reject fractions, negatives, and overflow.

Read `#PBS` directives only before the first executable line, allowing blank lines, comments, and a shebang. Recognize the exact `#PBS` token, followed by whitespace or end of line. Support single/double quoting and backslash escaping for literal arguments; do not perform variable, command, glob, or tilde expansion. Reject multiline directive continuations and unmatched quotes. Within one source, repeated scalar options use the last value. Directive and CLI parsing share the same option/resource rules.

## Environment and output

The baseline environment contains submission values for `HOME`, `USER`, `LOGNAME`, `PATH`, `LANG`, `LC_*`, and `TZ` when present; set `SHELL` to the configured execution shell. Use `/usr/local/bin:/usr/bin:/bin` if submission `PATH` is absent. `-V` adds the submission environment; `-v KEY=value,OTHER` then overrides selected values, resolving bare names from that environment and rejecting missing names. Commas always separate entries in v0.1; values containing commas are unsupported. CLI `-v` entries override matching directive entries while retaining other keys.

Apply managed `PBS_*`, `PBOTD_*`, and `CUDA_VISIBLE_DEVICES` values last. Explicit `-v` assignments to these names are rejected; inherited values from `-V` are replaced. Export `PBS_JOBID`, `PBS_JOBNAME`, `PBS_QUEUE`, `PBS_O_WORKDIR`, `PBS_O_HOME`, `PBS_O_PATH`, `PBS_NCPUS`, `PBS_NGPUS`, `PBS_NODEFILE`, `PBS_GPUFILE`, and `PBOTD_GPU_UUIDS`. Set `PBS_ARRAY_INDEX` only for subjobs. `PBS_NODEFILE` contains the local hostname once per allocated CPU; `PBS_GPUFILE` contains one allocated UUID per line, and `PBOTD_GPU_UUIDS` is a comma-separated UUID list.

Set `CUDA_VISIBLE_DEVICES` to the allocated full UUIDs, or an empty string for a zero-GPU job. UUIDs and the empty-string behavior are supported by the [NVIDIA CUDA environment variable reference](https://docs.nvidia.com/cuda/cuda-programming-guide/05-appendices/environment-variables.html).

Default output paths are `<submission-dir>/<name>.o<sequence>` and `<submission-dir>/<name>.e<sequence>`. Resolve explicit relative `-o`/`-e` paths against the submission directory. For arrays, append `.<index>` to both default and explicitly supplied paths so subjobs have distinct files. No path-template expansion or remote `host:path` output destinations are supported.

Create missing output parent directories. Open output files in append mode on every attempt, preserving existing content and rerun output; failure to open an output is a launch failure. `-j oe` sends both streams to the stdout path, `-j eo` to the stderr path, and `-j n` keeps them separate (the default). If both paths resolve to the same file, use a shared file description. Resolve and freeze output paths at submission; `qalter -N` does not rename them. Keep attempt identity/times in job events, without injecting scheduler messages into job output.

## IDs, status, and control

Use `123.localhost` for ordinary jobs, `123[].localhost` for array parents, and `123[4].localhost` for subjobs. A bare sequence or bracketed ID may omit `.localhost`; a bare array sequence resolves to its parent. Reject remote server suffixes and range operands such as `123[1-4]`. Quote brackets in shell commands.

| Internal state | `qstat` code |
| --- | --- |
| `QUEUED`, `STARTING` | `Q` |
| `HELD` | `H` |
| `RUNNING` | `R` |
| `EXITING` | `E` |
| `COMPLETED`, `FAILED`, `CANCELLED`, `TIMEOUT` | `F` |

`SUBMITTED` is the submission event, not a persisted scheduling state. `qstat` lists nonterminal jobs; `-x` includes finished jobs. Explicit IDs select matching jobs including finished ones. `-f` includes the internal state, pending/failure reason, requested resources, current attempt, allocated UUIDs, timestamps, exit code/signal, and output paths. Default array listing shows the parent; querying its ID expands the subjobs as well. Table layout is pbotd-specific.

| Command | Behavior |
| --- | --- |
| `qsub` | Print the durable job ID after the snapshot and job record are committed. `-h` initially holds the job or all array subjobs. |
| `qdel` | Cancel queued/held work immediately; request termination of starting/running work. A successful response acknowledges the durable request; cleanup remains visible as `E`. During rerun cleanup, deletion cancels the pending requeue; an existing cancellation is a no-op and other exiting outcomes are unchanged. Repeating deletion of a cancelled job is a no-op; other terminal jobs report an error. |
| `qhold` / `qrls` | Hold/release queued/held jobs only. Repeating the same hold/release is a no-op. Releasing a hold does not bypass resource or dependency conditions. |
| `qalter` | Change `-N` and `-l` for queued/held jobs only; merge fields as at submission, validate, and commit atomically for each target. Queue, dependencies, array membership, and output paths cannot be changed. |
| `qrerun` | Terminate and requeue a running ordinary job or individual subjob with the same public ID, snapshot, environment, resource request, and output paths. Rejoin at the end of the queue with a fresh waiting-age clock. Allocate a new attempt ID only after prior execution has stopped. Reject queued/held/starting/exiting/terminal jobs and array parent operands. |
| `pbsnodes -a` | Show one local node, configured/reserved/free resources, GPU UUIDs, and availability reasons. |

To run a terminal job again, submit its script with `qsub` to create a new logical job. There is no automatic retry. Limiting `qrerun` to running individual jobs follows the principal PBS behavior while deliberately omitting its broader array operations; see the [PBS Professional reference](https://help.altair.com/2024.1.0/PBS%20Professional/PBSReferenceGuide2024.1.pdf).

Array-parent `qdel` cancels all nonterminal subjobs and leaves finished subjobs intact. Parent `qhold`/`qrls` affects only queued/held subjobs; active and terminal subjobs remain unchanged. Parent `qalter` is allowed only before any subjob has started and while all subjobs are queued/held, and updates the whole array atomically. Individual queued/held subjobs can be altered separately.

Commands accepting multiple job IDs process each target independently, report failures on stderr, and return nonzero if any target fails. Zero IDs is a usage error for mutating commands. `qstat` with zero IDs lists jobs. Usage/configuration errors return 2; other request errors return 1; success returns 0. Read-only `status`/`doctor` do not autostart the daemon.

## Arrays and dependencies

Accept `-J start-end[:step][%limit]`, including a single index and comma-separated ranges/indices, with nonnegative 32-bit indices and positive step/limit. Deduplicate and sort indices. Limit an array to 10,000 subjobs in v0.1. Without `%limit`, host resources determine concurrency. Count `STARTING`, `RUNNING`, and `EXITING` against the limit. Parents never reserve resources.

For parent display, active subjobs take precedence: `R` if any subjob is starting/running, otherwise `E` if any is exiting, otherwise `Q` if any is queued, otherwise `H` if any is held. When all are terminal, show `F`; aggregate the result as `COMPLETED` if all succeeded, otherwise `FAILED` if any failed, otherwise `TIMEOUT` if any timed out, otherwise `CANCELLED`. Include state counts in detailed output.

Support only `afterok` with one or more colon-separated predecessor IDs. Reject unknown IDs, self references, ranges, and other dependency types. Dependencies refer to logical jobs: an ordinary job or subjob must complete successfully; an array parent succeeds only when all its subjobs succeed. Because dependencies refer to existing jobs and cannot be altered, cycles cannot be introduced.

If any predecessor becomes terminal without success, mark the dependent `FAILED` with reason `DependencyFailed`, without launching it and without inventing an exit code. Apply this even to held dependents and to submissions naming an already unsuccessful predecessor. A rerun of a running predecessor keeps dependents waiting for that logical job's final result. Deleting a dependent does not affect its predecessors.

## Deliberate scope limits

v0.1 omits user priorities, delayed start (`-a`), interactive jobs, multi-node chunks, MPI task placement, remote submission/output, custom resources, complex queue administration, and byte-for-byte PBS output formats. Submission-directory execution, field-wise resource merging, zero-based arrays, array log suffixes, and single-user control privileges are explicit local conventions.
