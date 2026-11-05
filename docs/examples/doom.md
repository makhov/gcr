# Doom on the move

A game of Doom that moves to another Kubernetes node every 20 seconds
while you play it. Your health, ammo, position and the monsters chasing
you all go with it, and the pause is about half a second.

It's a toy, but a demanding one: a live display, a game loop, a VNC
server with a client connected, and a person noticing every hitch. Live
migration that keeps up with this keeps up with a long-running job or a
cache.

*k0da on a laptop, three nodes, no GPU. 10 minutes.*

## Run it

You need [k0da](https://github.com/makhov/k0da) v0.5.0 or later, Docker
or Podman, kubectl, and port 8090 free:

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
  It makes a target pod on another node, copies the container's memory
  and files across, freezes it for the final copy, and hands the target to
  the Deployment. The target keeps the pod's labels, so the Service finds
  it.
- The VNC connection doesn't survive the move. The pod's address changes,
  so gcr closes its TCP connections, and noVNC reconnects. That's the blip
  you see on top of the pause.

On an arm64 VM with three k0da nodes, moves paused the game for 530–890 ms,
and VNC answered again within about half a second of each.

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

## What it needs from the game

Nothing in Doom knows it moves. What the container needs:

- **No System V shared memory.** The pod's IPC namespace belongs to the
  pod, not the container, so its shared memory wouldn't move. Xvfb runs
  without MIT-SHM, and x11vnc with `-noshm`.
- **One container**, as for any live migration for now
  ([What can move](../migration.md#between-kubernetes-nodes)).
- **gcr v0.1.14 or later.** The doom pod's TCP readiness probe nearly
  always leaves a connection x11vnc hasn't accepted yet, and earlier gcr
  refused to migrate a container while it had one.

## Clean up

```sh
k0da delete -n doom
```
