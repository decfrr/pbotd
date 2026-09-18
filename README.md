# pbotd

[日本語利用ガイド：必要環境・インストール・一般ユーザーでの利用](README.ja.md)

**PBS On The Desktop** is a rootless, single-host, single-user PBS Professional-style batch scheduler written in Go for Linux and WSL. Queue `.pbs` scripts and start work when CPU, memory, and exclusive NVIDIA GPUs become available.

- Repository and Go module: [`github.com/decfrr/pbotd`](https://github.com/decfrr/pbotd)
- Binary: `pbotd`
- Command aliases: `qsub`, `qstat`, `qdel`, `qhold`, `qrls`, `qalter`, `qrerun`, and `pbsnodes`
- Reference project: [`slotd`](https://github.com/ymgaq/slotd)

Supports durable queues, daemon restart recovery, arrays, `afterok` dependencies, hold/release, alteration, cancellation, walltime, and running-job reruns. This is a practical PBS subset, not a PBS server replacement.

## Build and install

Requires Linux/WSL, Bash, and Go 1.27 or newer. NVIDIA hardware is optional for CPU jobs.

```sh
make build                        # produces a CGO-free bin/pbotd
./scripts/install.sh              # ~/.local/bin/pbotd and PBS aliases
export PATH="$HOME/.local/bin:$PATH"
qsub examples/cpu.pbs             # starts the daemon automatically
qstat -x
pbotd doctor
```

The installer refuses conflicting PBS aliases. Use `./scripts/install.sh --no-aliases` and `pbotd qsub ...` if PBS commands already exist, or choose `--bin-dir DIR`. Running jobs retain their existing runner executable during an upgrade; restart the daemon to use the new version.

Configuration is optional. Copy [examples/config.toml](examples/config.toml) to `~/.config/pbotd/config.toml` to customize it. Auto capacity reserves 2 GiB for the host; on a small machine, lower `resources.memory_reserve` and `defaults.mem`. See [operations](docs/operations.md) for foreground and systemd operation.

## Documentation

- [Japanese user guide](README.ja.md): requirements, installation changes, and rootless operation.
- [Design specification](pbotd-design-spec.md): architecture, state, scheduling, and recovery.
- [PBS compatibility](docs/compatibility.md): supported syntax, defaults, output, arrays, and command behavior.
- [Operations](docs/operations.md): configuration, paths, GPU failure behavior, and validation.
- [Implementation TODO](TODO.md): ordered milestones and acceptance checks.
- [Testing](docs/testing.md): Linux checks and optional CUDA, cgroup, WSL, and systemd smoke tests.

## Usage

```bash
#!/bin/bash
#PBS -N train
#PBS -q gpu
#PBS -l select=1:ncpus=4:mem=8gb:ngpus=1
#PBS -l walltime=24:00:00
#PBS -j oe

python train.py
```

Save this as `train.pbs`, then run:

```console
qsub train.pbs
qstat
qstat -f 123.localhost
```

Jobs run in the directory where `qsub` was invoked. Without resource options, a job requests one CPU and 1 GiB of memory, uses no GPU, and has no walltime limit. Selecting `-q gpu` defaults to one GPU. Default output files are `<name>.o<sequence>` and `<name>.e<sequence>`; `-j oe` combines output in the stdout file.

CPU and memory are reservations by default; delegated cgroup v2 enforcement and CPU affinity are optional. GPU allocation uses full UUIDs and is exclusive within pbotd. Outside processes can still use the hardware. The default GPU policy avoids foreign compute processes and waits when occupancy cannot be determined.

`qstat -f --json JOB` includes attempt history, usage, and events. Scripts and environments are snapshotted at submission, and reruns append to existing logs. Runners survive daemon stop/restart and enforce walltime independently.

```sh
make check  # Linux unit/integration tests, race detector, vet, static build
```
