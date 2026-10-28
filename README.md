# gcr

A containerd runtime (`io.containerd.gcr.v2`) with checkpoint, restore and fork
built in. Containers run in-process through libcontainer; no runc binary.
`gcr` is a placeholder name.

## Status

- containerd task API: create, start, exec, terminals, attach/stdin close,
  resize, state, kill, wait, delete, pids, pause, resume, stats, resource
  updates, checkpoint, restore. Passes the CRI conformance suite (critest).
- User namespaces by default (below).
- Time travel and live migration (below).
- Not yet: GPUs.

## Requirements

- Linux 6.12+ with cgroup v2, run as root
- containerd 2.x
- libseccomp (runtime and, to build, headers)
- CRIU for checkpoint/restore; time travel needs the fork with
  `--seccomp-notify-socket` (see below)

## Install

Build on the Linux host (cgo is required) and install both binaries:

```sh
make install          # containerd-shim-gcr-v2 and gcr into /usr/local/bin
make criu             # optional: build and install the CRIU fork from ../criu
```

containerd finds the shim by name on `PATH`: the runtime type
`io.containerd.gcr.v2` maps to `containerd-shim-gcr-v2`.

## Running containers

### With ctr

No configuration is needed; name the runtime type directly:

```sh
ctr image pull docker.io/library/busybox:latest
ctr run -d --runtime io.containerd.gcr.v2 docker.io/library/busybox:latest demo sleep 3600
ctr task ls
ctr task kill -s KILL demo && ctr task rm demo && ctr container rm demo
```

Checkpoint and restore go through containerd as usual:

```sh
ctr container checkpoint --task --rw demo localhost/demo-ckpt:1
ctr task kill -s KILL demo && ctr task rm demo && ctr container rm demo
ctr container restore --live --rw demo localhost/demo-ckpt:1
```

### With Kubernetes (CRI)

Register the runtime with containerd's CRI plugin in
`/etc/containerd/config.toml` and restart containerd:

```toml
version = 3
[plugins.'io.containerd.cri.v1.runtime'.containerd.runtimes.gcr]
  runtime_type = 'io.containerd.gcr.v2'
  # Pass gcr's pod annotations (e.g. time travel) through to the runtime.
  pod_annotations = ['gcr.dev/*']
```

Restart containerd after installing a new gcr: it reads the runtime's
features (user namespaces, idmapped mounts) at startup.

Then add a RuntimeClass and select it from pods:

```yaml
apiVersion: node.k8s.io/v1
kind: RuntimeClass
metadata:
  name: gcr
handler: gcr
---
apiVersion: v1
kind: Pod
metadata:
  name: demo
  annotations:
    gcr.dev/time-travel: execve   # optional, see Time travel
spec:
  runtimeClassName: gcr
  containers:
    - name: demo
      image: busybox
      command: ["sleep", "3600"]
```

If the kubelet uses the systemd cgroup driver, gcr follows it (CRI sends
`slice:prefix:name` cgroup paths).

## User namespaces

Containers run in a user namespace by default: root inside is an
unprivileged range of 65536 host IDs, one range per pod (all containers of a
pod share the sandbox's user namespace, so they can share volumes and
PID/IPC namespaces).

Kubernetes pods follow Kubernetes: `hostUsers` defaults to true there, and
kubelet sends it like an explicit true, so a runtime cannot change it. Use
`hostUsers: false` (containerd's own user namespaces, supported by gcr and
the better path: the pod's network namespace is created inside the user
namespace), enforced cluster-wide by policy if wanted, or opt a pod into
gcr's with the annotation `gcr.dev/host-users: "false"`.

For gcr's own user namespaces:

- Image layers are mounted idmapped, so files look root-owned inside; files
  the container creates are stored as its mapped IDs.
- Bind mounts on filesystems that support it (ext4, xfs, btrfs, tmpfs, f2fs,
  vfat) are idmapped, so volumes keep their on-disk owners.
- In a pod network namespace gcr sets `ip_unprivileged_port_start=0` and an
  open `ping_group_range` (root can bind port 80 and ping), applies the
  pod's `net.*` sysctls itself, and bind-mounts `/sys` read-only.
- Raw sockets do not work in a pod network namespace (it belongs to the
  host's user namespace); containers adding `CAP_NET_ADMIN` there get host
  users.
- Skipped for privileged containers, containers sharing the host's network,
  PID or IPC namespace, and specs that bring their own user namespace
  (Kubernetes `hostUsers: false`). Opt out with `gcr.dev/host-users=true`.
- A restore claims the range the checkpoint was taken with. A migration
  target must be created with the source's range,
  `gcr.dev/userns-base=<first host ID>`; the source refuses otherwise.

## Time travel

A container started with `gcr.dev/time-travel=execve` gets a point on its
timeline just after each `execve` (at most one per `gcr.dev/time-travel-interval`,
default `1s`; the last `gcr.dev/time-travel-keep`, default 20, are kept). A
point is the processes (CRIU) plus the writable layer, taken with the new
program stopped before its first instruction.

```sh
ctr run -d --runtime io.containerd.gcr.v2 --annotation gcr.dev/time-travel=execve IMAGE ID CMD...
gcr timeline ID
gcr rewind ID 3
```

Rewind replaces the processes and the writable layer in place; containerd
keeps seeing one running task. It needs CRIU with `--seccomp-notify-socket`
([makhov/criu#1](https://github.com/makhov/criu/pull/1)) installed as `criu`.

## Live migration

The target is an ordinary gcr container with the same image and command,
created with `gcr.dev/migrate-in` (address to listen on) and
`gcr.dev/migrate-token`. It does not start its process; its `Start` waits for
the source and restores what it receives.

```sh
# target host
ctr run -d --runtime io.containerd.gcr.v2 \
  --annotation gcr.dev/migrate-in=0.0.0.0:7781 --annotation gcr.dev/migrate-token=$TOKEN \
  IMAGE ID CMD...
# source host
gcr migrate -token $TOKEN ID http://TARGET-HOST:7781
```

With user namespaces, create the target with the source's range
(`--annotation gcr.dev/userns-base=N`; a refused migration prints N).

When the kernel tracks dirty memory (x86, s390, powerpc; not arm64), the
source pre-copies memory in rounds to CRIU page servers on the target while
it keeps running. It then freezes, sends the remaining memory, the rest of
the checkpoint and the writable layer, and the target restores. The source
stays frozen until the target runs and is thawed if anything fails before;
afterwards it is killed. Open TCP connections reset. The token is sent in
clear text: use a trusted network.

## Develop

Everything builds and runs in a Linux VM (kernel 6.12+, containerd 2.4, CRIU):

```sh
limactl start --name=gcr hack/lima.yaml   # once
make vm T=criu                            # once: build and install the CRIU fork from ../criu
make vm T=test-e2e                        # build, install, run the end-to-end tests
```

`hack/e2e.sh` checkpoints and restores a counter with `ctr`;
`hack/e2e-timetravel.sh` records a timeline, rewinds it, and checks the
counter, the filesystem and the recording all carry on from the point;
`hack/e2e-migrate.sh` migrates a counter between two containerd namespaces
and checks a refused migration leaves the source running;
`hack/smoke-cri.sh` runs a pod through CRI (exec, terminal, logs, stats,
update, user namespace). For the CRI conformance suite:
`critest --runtime-handler gcr` (needs CNI plugins and the `gcr` runtime in
containerd's CRI config).
