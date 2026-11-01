# gcr documentation

Also as a site: [makhov.github.io/gcr](https://makhov.github.io/gcr/).

**Start here**

- [Quickstart](getting-started.md): install gcr, fork a running
  container, undo a command inside one
- [Installation](install.md): one host, a VM on your laptop, a throwaway
  cluster, your own cluster, the kubectl plugin, gcr-operator
- [What gcr is good for, and where it falls short](tradeoffs.md)
- [Differences from runc](runc-differences.md): what will not work as on
  runc, what is on by default, and a runc-compatible handler

**Examples**

- [Agent sandboxes](examples/agent-sandbox.md): undo and branching
- [Warm starts](examples/warm-starts.md): snapshot a loaded model server,
  start the next one from there
- [Node maintenance](examples/node-maintenance.md): drain a Kubernetes
  node without restarting what runs on it
- [A warm pool across the fleet](examples/warm-pool.md): one snapshot, a
  registry, forks everywhere

**Running containers**

- [Docker](docker.md): `docker run --runtime gcr`, and snapshots, forks
  and time travel on Docker's containers
- [ctr and containerd](ctr.md): the shortest path, and checkpoint/restore
- [Kubernetes](kubernetes.md): pods on gcr, their annotations, time travel
  and snapshots of pods, moving a pod
- [CRI-O](crio.md): what works there and what does not
- [gcr-operator](operator.md): pods that move instead of restarting when
  nodes are drained or shut down

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

- [Configuration](configuration.md): what to change and why, recipes,
  then every handler option and annotation
- [The `gcr` command](cli.md)
- [Troubleshooting](troubleshooting.md)

**Inside gcr**

- [Architecture](architecture.md): the shim, CRIU, the supervisor, where
  state lives
- [Development](development.md): the VM, the tests, CI
