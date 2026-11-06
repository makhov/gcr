# Doom on the move

A game of Doom that moves to another Kubernetes node every 20 seconds
while you play it. Your health, ammo, position and the monsters chasing
you all go with it, and the pause is a fraction of a second.

It's a toy, but a demanding one: a live display, a game loop, a VNC
server with a client connected, and a person noticing every hitch. Live
migration that keeps up with this keeps up with a long-running job or a
cache.

*k0da on a laptop, three nodes, no GPU. 10 minutes.*

## Run it

You need Linux 6.12 or later, [k0da](https://github.com/makhov/k0da)
v0.5.0 or later, Docker or Podman, kubectl, and port 8090 free. k0da's
nodes are containers, so they run on your kernel. On a Mac, run all of
it in a Linux VM from a current image, such as Lima's
`template:ubuntu-26.04` or `template:debian-13`. An older Lima `default`
VM may still be on Ubuntu 24.04, whose 6.8 is too old.

```sh
git clone https://github.com/makhov/gcr && cd gcr
examples/doom/up.sh
```

`up.sh` creates a cluster of three nodes (a controller and two workers),
installs gcr from its latest release's manifests, and starts the game. When
it's done, open <http://localhost:8090>, click the game, and play.

The bar at the top shows the node the game runs on, how many times it has
moved, the last pause and the time until the next move. The edge of the
screen flashes red on each move.

## What happens

```
browser ──► Traefik ──► arcade (noVNC, websockify) ──► Service doom ──► doom (Xvfb, x11vnc, Chocolate Doom)
             :8090        runc, stays put                                gcr, moves
```

- **`doom`** is a Deployment on the `gcr` runtime class: Chocolate Doom
  playing Freedoom on a virtual X display, which x11vnc serves.
- **`arcade`** runs on the cluster's default runtime and stays put. It
  serves noVNC and proxies its WebSocket to the `doom` Service. A loop in it
  creates a [Migration](../operations.md#migration) for the doom pod every
  20 seconds and writes its result for the page.
- Each Migration runs as `kubectl gcr migrate` does
  ([Between Kubernetes nodes](../migration.md#between-kubernetes-nodes)).
  It makes a target pod on another node, freezes the game, sends its
  processes' state and files across, and restores it there before its
  memory moves: it's post-copy (`mode: PostCopy`), so the game runs on the
  new node at once and fetches each page from the old one as it touches
  it, and the rest in the background. Then it hands the target to the
  Deployment. The target keeps the pod's labels, so the Service finds it.
- The VNC connection doesn't survive the move. The pod's address changes,
  so gcr closes its TCP connections, and the page reconnects, showing the
  last frame dimmed meanwhile. It reconnects to the new pod directly: the
  mover finds it by its `gcr.dev/target-of` label as soon as the move
  starts, while the doom Service only finds it seconds after the game is
  back. The game's X server moves with any key held as the connection
  dropped still down, so the page then releases those let go of since.

Post-copy gives the shortest pause, but until all its memory has arrived
the game needs both nodes: one going down mid-move would lose it. Here
the nodes are containers in one VM, so that's no risk. Pre-copy
(`MODE=PreCopy`, below) leaves the game where it was if anything fails
first. On arm64, which can't track which pages changed, it moves all of
the memory during the pause: on an arm64 VM with three k0da nodes, moves
paused the game for 500–890 ms, and the page showed it again 0.65–0.75
seconds after its connection dropped.

## Things to try

Watch the moves from the cluster's side:

```sh
export KUBECONFIG=~/.kube/doom.kubeconfig
kubectl get pods -o wide -w
kubectl get migrations -w
```

Drain the node the game is on. The game moves off it rather than
restarting, since its pod opts in with `gcr.dev/migrate-on-evict`
([Node maintenance](node-maintenance.md)):

```sh
kubectl drain "$(kubectl get pod -l app=doom -o jsonpath='{.items[0].spec.nodeName}')" \
  --ignore-daemonsets --delete-emptydir-data
```

Stop the moves (`INTERVAL` 0), or make them more frequent:

```sh
kubectl set env deploy/arcade INTERVAL=5
```

Move it pre-copy instead, and compare the pause:

```sh
kubectl set env deploy/arcade MODE=PreCopy
```

## What it needs from the game

Nothing in Doom knows it moves. What the container needs:

- **No System V shared memory.** The pod's IPC namespace belongs to the
  pod, not the container, so its shared memory wouldn't move. Xvfb runs
  without MIT-SHM, and x11vnc with `-noshm`.
- **One container that moves.** Doom's pod has one; a pod with several
  names the one that moves, and the others start anew
  ([What can move](../migration.md#between-kubernetes-nodes)).
- **A gcr newer than v0.1.13.** The doom pod's TCP readiness probe nearly
  always leaves a connection x11vnc hasn't accepted yet, and gcr up to
  v0.1.13 refused to migrate a container while it had one.
- **A gcr newer than v0.1.14** for the post-copy moves: its Migration has
  `mode`. With v0.1.14 the game still moves, pre-copy, since the cluster
  drops the field it doesn't know.
- **Rootful Docker or Podman** for the post-copy moves. A post-copy
  restore needs userfaultfd, which a process in a rootless container
  can't open, not even privileged. On rootless nodes the arcade moves the
  game pre-copy instead, and the page's header says so; `up.sh` warns.

## Clean up

```sh
k0da delete -n doom
```
