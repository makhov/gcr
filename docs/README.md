# gcr documentation

**Start here**

- [Getting started](getting-started.md): requirements, install, running
  containers with `ctr`, Docker, Kubernetes (containerd) and CRI-O, a
  first fork
- [Docker](docker.md): `docker run --runtime gcr`, and snapshots, forks
  and time travel on Docker's containers
- [What gcr is good for, and where it falls short](tradeoffs.md)
- [Differences from runc](runc-differences.md): what will not work as on
  runc, what is on by default, and a runc-compatible handler

**Features**

- [Fork](fork.md): snapshots of running containers, forks, snapshot
  lifetime, lazy restore, shared memory
- [Moving snapshots between hosts](snapshot-registry.md): through OCI
  registries
- [Time travel](time-travel.md): a timeline of points and rewinds
- [Live migration](migration.md): pre-copy, TLS
- [GPUs](gpus.md): NVIDIA through CDI, CUDA state in checkpoints and forks

**Isolation**

- [User namespaces](user-namespaces.md)
- [Hardening](hardening.md): system calls, supervised mounts, the netlink
  policy, Landlock, Docker-in-Docker
- [What the hardening breaks](compatibility.md), and the way around each
- [Security model](security.md): whom gcr trusts, and what it does not
  defend against

**Reference**

- [Configuration](configuration.md): runtime handler options and
  annotations
- [The `gcr` command](cli.md)
- [Troubleshooting](troubleshooting.md)

**Inside gcr**

- [Architecture](architecture.md): the shim, CRIU, the supervisor, where
  state lives
- [Development](development.md): the VM, the tests, CI
