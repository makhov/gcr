# Quickstart

By the end of this page you will have forked a running container and
undone a command inside one. It takes about ten minutes, on one Linux
machine, with Docker.

What you need:

- a Linux machine with kernel 6.12 or later and cgroup v2, and root on it;
- containerd 1.7 or later, with Docker using it. Distribution packages
  (`docker.io`, `docker-ce`) do; Docker's own static binaries bring a
  containerd of their own, which gcr is not installed for.

No spare machine? [A VM on your laptop](install.md#a-vm-on-your-laptop)
is two commands. Prefer Kubernetes? [A throwaway
cluster](install.md#a-throwaway-cluster) is one.

## 1. Install gcr

A release is a single file that carries everything else it needs, its
CRIU included:

```sh
arch=$(uname -m | sed -e s/x86_64/amd64/ -e s/aarch64/arm64/)
v=$(curl -fsSLo /dev/null -w '%{url_effective}' https://github.com/makhov/gcr/releases/latest | sed 's#.*/##')
curl -fLo gcr "https://github.com/makhov/gcr/releases/download/$v/gcr-$v-linux-$arch" && chmod +x gcr

sudo ./gcr setup -docker
```

`gcr setup` installs the `gcr` command and its shim, registers the
runtime with containerd (and, with `-docker`, with Docker), restarts both,
and then reports on the host:

```
installed /usr/local/bin/gcr
installed /usr/local/bin/containerd-shim-gcr-v2
installed /var/lib/gcr/bin/criu
wrote /etc/containerd/conf.d/gcr.toml
restarted containerd (running containers carry on)
registered runtime gcr in /etc/docker/daemon.json
restarted docker

ok   kernel          6.17.0-40-generic
ok   cgroup v2       unified hierarchy
ok   shim            /usr/local/bin/containerd-shim-gcr-v2 v0.1.6
ok   containerd      2.2.1, runtime gcr registered
ok   docker          runtime gcr registered
ok   CRIU            /var/lib/gcr/bin/criu 4.2.1 (v4.2.1+gcr.1)
ok   Landlock        ABI 7
off  netlink policy  no BPF LSM: containers' network configuration is limited by seccomp alone
                     -> add bpf to the kernel's lsm= list (...) and reboot
off  lazy restore    vm.unprivileged_userfaultfd = 0: no gcr.dev/lazy-restore or post-copy migration
                     -> sysctl vm.unprivileged_userfaultfd=1
off  dirty memory    not tracked (arm64): migration is stop-and-copy or post-copy
off  memory merge    KSM is not running: no gcr.dev/memory-merge
```

That is a stock Ubuntu host. Everything on this page works there, and the
four `off` lines are extras you can turn on later.

`ok` works. `off` is a feature this host cannot do, and everything else
still works without it. `MISSING` means gcr will not work properly until
you deal with it. `gcr doctor` shows the report again any time;
[what each line means](install.md#checking-a-node).

Containers already running are not disturbed by the restart, and runc is
still there. Only containers you start with `--runtime gcr` use gcr.

## 2. Run a container

```sh
docker run --rm --runtime gcr busybox echo hello
```

That is the whole integration. Docker's commands work as they do with
runc: `exec`, `logs`, `cp`, `top`, ports, volumes, Compose. What differs
is that the container is [hardened](hardening.md) and runs in a [user
namespace](user-namespaces.md), where root inside is an unprivileged user
outside. [Differences from runc](runc-differences.md) lists what that
changes in practice.

## 3. Fork a running container

Start something that takes a while to become useful. This Python process
spends a few seconds building a large list, then waits:

```sh
docker run -d --runtime gcr --name warm python:3.13-alpine python3 -c \
  'import time; data = list(range(5_000_000)); print("ready", flush=True); time.sleep(1e9)'

docker logs -f warm        # wait for "ready", then Ctrl-C
```

Snapshot it. The container keeps running:

```sh
id=$(docker inspect -f '{{.Id}}' warm)
sudo gcr -n moby snapshot "$id" /var/lib/gcr/snapshots/warm
```

Docker keeps its containers in containerd's `moby` namespace and knows
them by their long IDs, which is what `-n moby` and `$id` are for. Under
plain containerd it is just `gcr snapshot warm DIR`.

Now start two containers from that snapshot:

```sh
for i in 1 2; do
  docker run -d --runtime gcr --name fork-$i \
    --annotation gcr.dev/fork-from=/var/lib/gcr/snapshots/warm \
    python:3.13-alpine true
done
```

Both begin at the moment of the snapshot, with `data` already built, a
few tens of milliseconds after the `docker run`. The image must be the
one the snapshot came from, but the command (`true`) is never run: a fork
restores the snapshot's processes instead of starting the image's
entrypoint. Docker shows what you asked for, and `docker top` shows what
is actually running:

```console
$ docker ps --format '{{.Names}}\t{{.Command}}'
fork-2	"true"
fork-1	"true"
warm	"python3 -c 'import …"

$ docker top fork-1
UID     PID      PPID     CMD
1074069 3714144  3714119  python3 -c import time; data = list(range(5_000_000)); ...
```

Each fork is a container like any other: `docker exec fork-1 python3 -c
'print(1+1)'` works, and what one fork writes no other fork and not the
snapshot ever sees.

Clean up the forks, then the snapshot. gcr refuses to delete a snapshot
something still depends on, and says what:

```console
$ docker rm -f fork-1 fork-2 warm
$ sudo gcr snapshot-rm /var/lib/gcr/snapshots/warm
```

Had you tried that with the forks still up:

```
gcr: snapshot /var/lib/gcr/snapshots/warm is in use by container moby/aeb11218…,
container moby/d133f4c5…: failed precondition
```

`gcr snapshot-users DIR` lists who holds one
([snapshot lifetime](fork.md#snapshot-lifetime)).

## 4. Undo a command

Time travel keeps points on a container's timeline and puts the container
back on any of them: its files, its processes, and what those processes
held in memory.

```sh
docker run -d --runtime gcr --name undo --network none \
  --annotation gcr.dev/time-travel=manual \
  python:3.13-alpine python3 -c 'import time; time.sleep(1e9)'

docker exec undo sh -c 'mkdir /work && echo "the plan" > /work/plan.txt'
```

Take a point, then do something destructive:

```console
$ id=$(docker inspect -f '{{.Id}}' undo)
$ sudo gcr -n moby point "$id" "rm -rf /work"
#1
$ docker exec undo rm -rf /work
$ docker exec undo ls /work
ls: /work: No such file or directory
```

Rewind to the point it printed:

```console
$ sudo gcr -n moby timeline "$id"
POINT  TIME          BEFORE
#1     22:09:10.842  rm -rf /work

$ sudo gcr -n moby rewind "$id" 1
$ docker exec undo cat /work/plan.txt
the plan
```

Docker never saw the container stop, and nothing had to be recreated. Its
uptime does restart (`docker ps` then says "Up Less than a second"),
because the processes running now are the restored ones. Clean up with
`docker rm -f undo`.

!!! note "Why `--network none`"

    A rewind restores the processes' network as it was, which only works
    where Docker did not set one up: `--network none` or `--network
    host`. On a bridge network gcr refuses the rewind rather than
    half-doing it. [Docker networking](docker.md#networking) explains it.

!!! tip "The first one is the slow one"

    A container's first point and first rewind pay to read CRIU and the
    pages behind them: a few seconds here. Afterwards both settle at
    about 0.1 s for a small container. Size matters more than count:
    [what time travel costs](time-travel.md#what-it-costs).

## What just happened

- **A snapshot** is a container's processes (through CRIU) plus its
  writable filesystem layer, taken while it keeps running. It is a
  directory you name.
- **A fork** mounts that layer read-only underneath its own empty one and
  restores the snapshot's processes into a new container. Nothing is
  copied, so a fork costs the same whether the snapshot wrote one file or
  a gigabyte, and each fork's writes stay its own.
- **A point** is the same kind of dump, kept on the container's timeline.
  A rewind swaps the processes and the writable layer back in, in place,
  so whoever started the container never sees it stop.

That is why memory comes back, not just files: these are dumps of running
processes. A database that held rows only in memory has them again.

## Where next

- [Examples](examples/index.md) for whole use cases: agent sandboxes with
  undo and branching, warm starts for model servers, draining a node
  without restarting work.
- [Configuration](configuration.md) for what you would change, and why.
- [Fork](fork.md) for lazy restore (forks that pay only for the memory
  they touch) and for sharing memory between forks.
- [Time travel](time-travel.md) for points after every `exec`.
- Other ways to run it: [ctr and containerd](ctr.md),
  [Kubernetes](kubernetes.md), [CRI-O](crio.md).
