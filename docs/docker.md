# Docker

gcr runs as a Docker runtime: `docker run --runtime gcr`. Docker starts
gcr's shim through containerd, as it starts runc's, so the Docker CLI,
Compose and anything built on the Docker API work unchanged, and gcr's
snapshots, forks and time travel work on the containers Docker runs.

## Setting it up

`sudo gcr setup -docker` does all of this: it registers the runtime with
containerd and with Docker, and restarts both. The rest of this section
is what it writes, for when you would rather do it yourself.

dockerd has to use the host's containerd, the one gcr is installed for
(Docker's static binaries bring a containerd of their own; packages use
the system's). Register the runtime in `/etc/docker/daemon.json` and
restart dockerd:

```json
{
  "runtimes": {"gcr": {"runtimeType": "io.containerd.gcr.v2"}},
  "ip-forward-no-drop": true
}
```

`"default-runtime": "gcr"` makes it the default. `ip-forward-no-drop`
keeps dockerd from setting the host's `FORWARD` policy to `DROP`, which
cuts off the CNI bridges of containerd's own pods; leave it out on a host
that runs only Docker. `hack/docker-setup.sh` installs Docker's static
binaries and starts dockerd this way.

## What you get

Containers run with gcr's defaults, as with `ctr`: a [user
namespace](user-namespaces.md) (root in the container is an unprivileged
range of host IDs), [hardening](hardening.md) and [Landlock](security.md).
Docker's operations work as with runc: run, exec (with a terminal),
logs, attach, stop and restart, pause, `docker cp`, `docker top` and
stats, published ports, bridge and user-defined networks, volumes,
health checks, `--init`, `--read-only`, `-u`, `-m`, `--privileged`.
Docker's own `docker checkpoint` is not supported.

gcr's features are reached through the `gcr` command, with `-n moby`
(Docker's containerd namespace) and the container's full ID:

```sh
id=$(docker inspect -f '{{.Id}}' web)

# Snapshot a running container, and start containers from the snapshot.
gcr -n moby snapshot $id /var/lib/gcr/snapshots/web
docker run -d --runtime gcr --annotation gcr.dev/fork-from=/var/lib/gcr/snapshots/web IMAGE

# Time travel.
docker run -d --runtime gcr --network none --annotation gcr.dev/time-travel=execve IMAGE CMD...
gcr -n moby timeline $id
gcr -n moby rewind $id 3
```

A fork is created from the same image as its snapshot's container (the
command is not run: the snapshot's processes are). Annotations are the
ones in [configuration](configuration.md#annotations).

[Agent sandboxes](examples/agent-sandbox.md) is a worked example of all
of this: a Python wrapper around `docker` and `gcr` that gives a sandbox
undo and branching.

## Networking

Docker sets up a container's network itself, once the runtime has
created the container: a veth pair to a bridge on the host, an address,
routes, its DNS. gcr leaves the network out of checkpoints and works with
what Docker sets up:

- **A fork** is restored with an empty network namespace (its loopback
  up), which Docker then sets up as for any new container: the fork gets
  an address of its own. Connections the snapshot had open are gone;
  listening sockets keep listening.
- **A rewind** keeps the processes' network as it was, which is
  possible only where Docker made none: `--network none`, or `--network
  host` (which also means no gcr user namespace). On a bridge network a
  rewind is refused before it stops anything.

## Under the hood

Docker mounts the container's overlay rootfs itself and hands the runtime
a path, under directories only root can enter (`/var/lib/docker` is
`0710`). gcr reads the overlay from that mount and mounts it again in the
container's bundle, with the image layers idmapped for the user namespace
and the writable layer handed to the container's root, and binds it back
where Docker expects it (`docker cp` reads it there). Bind mount sources
the container's root cannot reach (Docker's `/etc/hostname`, `hosts` and
`resolv.conf`) are bound into the bundle as well: CRIU opens them from
inside the user namespace when it restores.
