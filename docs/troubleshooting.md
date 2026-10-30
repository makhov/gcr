# Troubleshooting

## Where to look

- **The shim's log** is containerd's (`journalctl -u containerd`), each
  line tagged with the container. At debug level (containerd's
  `[debug] level = 'debug'`) the shim says, once per container, why it
  left out a user namespace, hardening or Landlock
  ([configuration](configuration.md#seeing-what-applied)).
- **CRIU's log.** A failed checkpoint, restore, fork or rewind leaves its
  log in the container's bundle, and the error names the directory
  (`restore (criu log in .../criu-restore-123): ...`). The bundle is
  under `/run/containerd/io.containerd.runtime.v2.task/<namespace>/<id>/`
  while the container exists.
- **What the netlink policy refused:**
  `bpftool map dump pinned /sys/fs/bpf/gcr/netlink/stats` (key 0:
  checked, key 1: refused, per CPU).

## A program fails with EPERM or ENOSYS

A call one of gcr's layers refused. [Compatibility](compatibility.md)
lists what each layer refuses, the programs known to use it, and the way
around each. To find the call, run the program under `strace -f` in the
container (or `strace -p` from an `exec`), or compare with a handler that
has `Hardening = 'off'` and `Landlock = 'off'`.

## Which layer refused it

- **seccomp** refusals show nowhere but in the program's error. Compare
  under a handler with `Hardening = 'off'`.
- **Landlock** refusals are in the kernel's audit log, where audit is
  on (auditd running, or `audit=1` on the kernel command line) and the
  kernel has Landlock ABI 7 (Linux 6.15 or later):

  ```
  type=LANDLOCK_ACCESS msg=audit(...): domain=16b69b9aa blockers=fs.change_topology path="/mnt" dev="overlay" ino=2097465
  ```

  Older audit tools print the type as `UNKNOWN[1423]`.
  `fs.change_topology` is a mount; other `fs.` blockers name the file
  access refused, at the path given. Containers restored from a
  checkpoint are confined again, but their refusals are not logged.
- **Supervised mounts** the supervisor refused are in the shim's log, at
  info level (`mount refused`, with the filesystem type and flags).
- **The netlink policy** counts its refusals (above).

## Errors gcr returns

| Error says | Why | What to do |
| --- | --- | --- |
| `... is not allowed for Kubernetes pods on this runtime handler (PodAnnotations)` | a pod used a restricted annotation | list it in the handler's `PodAnnotations`, on a handler meant for it |
| `gcr.dev/fork-from in a Kubernetes pod needs the handler's SnapshotRoots` | pods may fork only from known directories | set `SnapshotRoots` |
| `gcr.dev/fork-from: ...` (owner, mode, symlink) | the snapshot or one of its layers could have been written by someone other than root | keep snapshots under a root-owned directory nobody else can write; reach it without symlinks |
| `... is not a snapshot` | `fork-from` names a directory without `images/` and `upper/` | check the path; `gcr snapshot` makes them |
| `... is in use by container ..., snapshot ...` | `gcr snapshot-rm` of a snapshot something mounts | remove the forks, and the snapshots of them, first (`gcr snapshot-users`) |
| `time travel every ... keeping ... points: the handler allows at most ...` | the annotations ask for more than `TimeTravel` allows | ask for less, or raise the handler's limits |
| `time travel in a container CRI-O runs` | CRI-O tracks a container by its process, which a rewind replaces | run it under containerd ([getting started](getting-started.md#with-cri-o)) |
| `live migration into a container CRI-O runs` | CRI-O wants the process when it creates the container, before the source has sent one | make the target a containerd container |
| `rewind of a container on a network its caller set up` | time travel under Docker on a bridge network | run it with `--network none` or `host` ([Docker](docker.md#networking)) |
| `gcr's user namespaces need the rootfs as overlay mounts from containerd` | `gcr.dev/host-users=false` or `UserNamespaces = 'always'` under CRI-O | use `hostUsers: false` (CRI-O's user namespaces) |
| `time travel for containers with GPUs` | CRIU's CUDA plugin cannot pre-dump, so every point would copy GPU memory | none yet |
| `time travel needs a container without a terminal` | | run without `-t` |
| `time travel is off (start the container with gcr.dev/time-travel)` | `gcr timeline` or `rewind` on a container without it | recreate it with the annotation |
| `this node has vm.unprivileged_userfaultfd = 0` | `gcr.dev/lazy-restore` needs it | set the sysctl, or restore eagerly ([fork](fork.md#lazy-restore)) |
| `KSM is not running (/sys/kernel/mm/ksm/run is 0, not 1)` | `gcr.dev/memory-merge` needs KSM | `echo 1 > /sys/kernel/mm/ksm/run` |
| a migration refused with a user namespace range | the target was created with another range | recreate it with `gcr.dev/userns-base=N`, N from the error |
| `post-copy: this node has vm.unprivileged_userfaultfd = 0` | the target cannot restore a container in a user namespace lazily | set the sysctl on the target, or migrate without `-post-copy` |
| `target is running but serving it its memory failed` | a post-copy migration lost the source or the connection after the target ran | the container is lost; restore it from a snapshot or checkpoint |
| `gcr.dev/migrate-in needs gcr.dev/migrate-token of at least 16 characters` | the token is missing or too short | use a longer one |
| GPU driver, model or minor mismatch on restore | the node cannot take the checkpoint | restore on a node with the same driver and GPUs at the same indexes ([GPUs](gpus.md)) |

## The netlink policy is off

The shim logs `netlink policy off: containers' network configuration is
limited by seccomp alone` when the kernel has no BPF LSM. Add `bpf` to
the kernel's `lsm=` list (keep the others: `cat /sys/kernel/security/lsm`)
and reboot. `hack/lima.yaml` shows how.

## Landlock is lost after a restore

The shim logs `CRIU cannot restore Landlock (no --landlock-fs)` when
`criu` is not the fork. Install it (`make criu`).

## After upgrading gcr

Restart containerd: it reads the runtime's features (user namespaces,
idmapped mounts) at startup. Running containers keep the shim they were
started with. The netlink policy is replaced in place by the first new
shim.

## Leftovers from a failed run

A failed test run, or a shim killed by hand, can leave containers,
mounts and state behind. `sudo hack/clean-e2e.sh` removes what the
end-to-end tests leave. Elsewhere, remove containers through containerd
(`ctr task kill`, `ctr task rm`, `ctr container rm`). Stale snapshot
references are cleared whenever they are read, and user namespace
ranges of snapshots deleted by hand are freed on the next allocation.
