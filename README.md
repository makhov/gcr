# gcr

A containerd runtime (`io.containerd.gcr.v2`) that treats a running
container's processes as something a platform can work with: snapshot it
without stopping it, fork it any number of times, rewind it to an earlier
point, move it to another host. It runs containers in-process through
libcontainer, with no runc binary, and hardens them by default beyond
what runc does. `gcr` is a placeholder name.

```sh
gcr snapshot warm /var/lib/gcr/snapshots/warm          # the parent keeps running
ctr run -d --runtime io.containerd.gcr.v2 \
  --annotation gcr.dev/fork-from=/var/lib/gcr/snapshots/warm IMAGE fork-1 CMD
```

## What it does

- **Fork.** Start new containers from a snapshot of a running one, with
  its memory (an interpreter up, a model loaded) and its files. Forks
  share the snapshot's files without copying them, and, opt-in, its
  memory. [Fork](docs/fork.md)
- **Snapshots between hosts** through any OCI registry, including ones
  backed by S3. [Moving snapshots](docs/snapshot-registry.md)
- **Time travel.** A point on the container's timeline at every `exec`,
  incremental where the kernel tracks dirty pages, and a rewind to any of
  them in place. [Time travel](docs/time-travel.md)
- **Live migration** to another host, with pre-copy where the kernel
  tracks dirty memory, optionally over mutual TLS.
  [Live migration](docs/migration.md)
- **GPUs.** NVIDIA GPUs through CDI, with CUDA state in checkpoints and
  forks. [GPUs](docs/gpus.md)
- **Hardening.** User namespaces by default, a denied set of system calls
  on top of any seccomp profile, supervised mounts, a netlink policy in
  the kernel (BPF LSM), and Landlock confining each container to its
  root. Docker-in-Docker runs unprivileged.
  [User namespaces](docs/user-namespaces.md), [hardening](docs/hardening.md)
- **A drop-in runtime** for containerd 1.7 to 2.4 and Kubernetes: the
  whole task API, CRI conformance. CRI-O runs it too, without the
  features that move process state.

Not yet: the NVIDIA ioctl proxy, restoring onto another GPU index,
SELinux labels for supervised mounts, a Kubernetes operator.

## Compared with runc

Same host (arm64, Linux 7.0), containerd 2.4.2, runc v1.5.2 (the release
gcr's libcontainer comes from), both runtimes on their defaults (gcr's
hardening and Landlock on):

| | gcr | runc |
| --- | --- | --- |
| critest | 123 passed, 0 failed | 113 passed, 0 failed |
| critest NRI tests | 11 of 11 | 11 of 11 |
| StartContainer, median / p90 | 16.1 / 19.5 ms | 21.9 / 26.7 ms |
| StopContainer, median / p90 | 16.5 / 21.3 ms | 30.7 / 34.1 ms |
| CreatePod / StopPod, median | 52.2 / 53.4 ms | 54.8 / 65.2 ms |
| Shim memory per pod (PSS, 10 pods) | ~6 MiB | ~4 MiB |

runc skips the user namespace tests, which gcr passes; the rest of critest's
skips (NRI without a socket, benchmarks, an image volume field containerd
does not report) are the same for both. Latencies are critest's
benchmarks, 50 sequential runs: gcr runs libcontainer in the shim instead of
executing a runc binary per operation. gcr's shim is the bigger binary
(more of it resident, shared between shims).

## Requirements

- Linux 6.12+ with cgroup v2, run as root
- containerd 1.7 or later, or CRI-O with some features missing
- libseccomp
- for checkpoints, forks, time travel and migration: CRIU, the fork at
  [makhov/criu](https://github.com/makhov/criu) for time travel and for
  Landlock across restores

[Getting started](docs/getting-started.md) installs it and runs a first
fork.

## Documentation

- [Getting started](docs/getting-started.md): install, and run with
  `ctr`, Kubernetes or CRI-O
- [What gcr is good for, and where it falls short](docs/tradeoffs.md)
- [Differences from runc](docs/runc-differences.md): what will not work
  as on runc, and how to turn each protection off
- Features: [fork](docs/fork.md), [moving snapshots](docs/snapshot-registry.md),
  [time travel](docs/time-travel.md), [live migration](docs/migration.md),
  [GPUs](docs/gpus.md)
- Isolation: [user namespaces](docs/user-namespaces.md),
  [hardening](docs/hardening.md), [what the hardening breaks](docs/compatibility.md),
  [security model](docs/security.md)
- Reference: [configuration and annotations](docs/configuration.md),
  [the `gcr` command](docs/cli.md)
- [Troubleshooting](docs/troubleshooting.md)
- [Architecture](docs/architecture.md), [development and tests](docs/development.md)
