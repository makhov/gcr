# Doom on the move

Walked through in the documentation:
[Doom on the move](../../docs/examples/doom.md).

A game of Doom that moves to another Kubernetes node every 20 seconds
while it is played, on a three-node k0da or kind cluster:

```sh
examples/doom/up.sh            # TOOL=kind for kind
open http://localhost:8090
```

- `cluster.yaml`, `kind.yaml`: the cluster, a controller and two
  workers, with the arcade's NodePort published on the host's port 8090.
- `up.sh`: creates it, installs gcr's latest release, and applies
  `doom.yaml`.
- `doom.yaml`: the game (`doom/`, on gcr) and the page it is played in
  (`arcade/`, noVNC, which also moves the game), on a NodePort.
- `doom/`, `arcade/`: their images, published as
  `ghcr.io/makhov/gcr-example-doom` and `gcr-example-arcade` by
  `.github/workflows/examples.yml`.
