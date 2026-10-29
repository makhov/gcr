# Security model

gcr's containers share the host's kernel, as runc's do. This page says
whom gcr trusts, what it defends against, and what it does not. The
layers themselves are in [hardening](hardening.md) and
[user namespaces](user-namespaces.md); what they break is in
[compatibility](compatibility.md).

## Who is trusted

| Party | Trusted? | What it controls |
| --- | --- | --- |
| Node operator | yes | containerd's config, the runtime handler options, the kernel, which handlers a RuntimeClass names |
| Anyone who can reach containerd's socket, or run `gcr` | yes: they are root | everything; annotations from `ctr` or the containerd client are not restricted |
| Pod author | no | the pod spec and its annotations, within what the handler allows |
| The workload | no | whatever runs in the container, including nested containers and user namespaces it makes |
| A registry, and what is pulled from it | no | the bytes of snapshots pulled with `gcr snapshot-pull` |
| A migration peer | only with the token (and, with `MigrationTLS`, a certificate from the cluster's CA) | the checkpoint it sends |

## What gcr defends against

**A workload reaching the host's kernel.** The calls behind most
container escapes and kernel exploits reachable from containers are
refused whatever seccomp profile the workload brings (`io_uring`, `bpf`,
`userfaultfd`, `perf_event_open`, the keyring, the new mount API, ...).
Mounts are checked against an allowlist by a supervisor that performs
them as the caller. Network objects created over netlink are checked by
a BPF LSM program, so a user namespace the container makes for itself
does not open the kernel's rarer drivers to it.

**A workload reaching the host's files.** Landlock confines each
container to its root filesystem, so a leaked host file descriptor or
working directory (runc's CVE-2024-21626) leads nowhere. Root inside is
an unprivileged range of host IDs by default.

**A pod author choosing what the node does as root.** Four annotations
would let a pod pick a host directory to restore from, a port to listen
on, the token it accepts, or its host IDs. Pods may use them only where
the handler lists them (`PodAnnotations`), and a fork's snapshot must be
under the handler's `SnapshotRoots`, owned by root and writable by no
one else ([configuration](configuration.md#podannotations)). Handler
limits also bound what a pod may ask of the node's disk (`TimeTravel`).

**Untrusted CRIU images.** A snapshot from a registry or a checkpoint
from a migration source is input to CRIU, which restores as root.
Before every restore gcr checks the images' user namespace maps against
the range the container was given. Archives are extracted with every
path resolved beneath the target directory (no symlinks, no
overwriting, only regular files and directories), and pulled blobs are
checked against their digests.

**A rogue migration source.** The target accepts a checkpoint only with
its token (16 characters or more), each part once, and closes the
listener once restored. With `MigrationTLS`, both sides present
certificates from the cluster's CA over TLS 1.3, and memory never
crosses the network in clear text.

**The shim's helpers.** The process that performs supervised mounts in
a container's namespaces is not dumpable, takes the container's AppArmor
profile, and closes the shim's descriptors before it acts. Control
sockets are reachable by root only.

## What gcr does not defend against

- **Kernel bugs in what stays reachable.** A shared kernel is a shared
  attack surface. gcr narrows it; a VM-based runtime removes it. For
  hostile multi-tenant code, use one ([tradeoffs](tradeoffs.md#isolation)).
- **Pods under Kubernetes defaults.** `Unconfined` is Kubernetes'
  default seccomp profile, and CRI conformance requires that such pods
  get no filter. Set the kubelet's `seccompDefault: true`, or a handler
  with `Hardening = 'denied'` or `'always'`.
- **Forks of one snapshot from each other.** They share a user
  namespace range, and identical memory at start (random seeds, tokens)
  until they re-seed ([fork](fork.md)).
- **Side channels from memory merging.** `gcr.dev/memory-merge` lets
  containers that opt in learn whether another holds a page with given
  contents. Use it among containers that trust each other.
- **`vm.unprivileged_userfaultfd = 1`**, which lazy restore needs,
  opens userfaultfd to every unprivileged process on the node, not just
  gcr's.
- **SELinux.** The mount helper takes the container's AppArmor profile,
  but not yet its SELinux label.
- **Privileged containers.** They are not hardened, as with runc.

## Reporting a vulnerability

Report it privately to the maintainers, not in a public issue.
