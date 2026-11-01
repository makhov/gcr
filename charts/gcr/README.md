# gcr Helm chart

Installs [gcr](https://makhov.github.io/gcr/) on every node of a
Kubernetes cluster that runs containerd, and, optionally, gcr-operator.

```sh
helm install gcr oci://ghcr.io/makhov/charts/gcr \
  --namespace gcr-system --create-namespace \
  --set operator.enabled=true
```

Or from a checkout: `helm install gcr charts/gcr -n gcr-system --create-namespace`.

The chart is released on its own (`chart/vX.Y.Z` tags), and installs the
gcr release that was the latest then (its `appVersion`); `image.tag`
picks another.

Its cluster-wide objects have fixed names (the RuntimeClasses `gcr` and
`gcr-migrate`, the `gcr-migrate` admission policy, `gcr-operator`'s
webhooks): install it once per cluster. Install it in `gcr-system`, where
`kubectl gcr` looks for it by default.

## Values

| Value | Default | |
| --- | --- | --- |
| `image.repository` | `ghcr.io/makhov/gcr` | The installer's and the operator's image. |
| `image.tag` | the chart's `appVersion` | |
| `image.pullPolicy` | `IfNotPresent` | |
| `imagePullSecrets` | `[]` | |
| `runtime.enabled` | `true` | The RuntimeClasses, the admission policy guarding `gcr-migrate`, and the installer. |
| `runtime.readyNodesOnly` | `true` | The RuntimeClasses schedule pods only onto nodes labelled `gcr.dev/runtime=ready`, which the installer labels. `false` where gcr is on every node, put there otherwise. |
| `runtime.installer.enabled` | `true` | The DaemonSet that runs `gcr setup` on every node. Off where nodes get gcr otherwise (k0da), labelled `gcr.dev/runtime=ready`. |
| `runtime.installer.setupArgs` | `""` | Options for `gcr setup`: `-containerd-config FILE -containerd-unit UNIT` for k3s and RKE2. |
| `runtime.installer.nodeSelector` | `{}` | Nodes to install gcr on. |
| `runtime.installer.tolerations` | every taint | |
| `runtime.installer.resources` | 10m CPU, 16Mi | |
| `operator.enabled` | `false` | [gcr-operator](https://makhov.github.io/gcr/operator/): drains move pods instead of evicting them. |
| `operator.registry` | `""` | A registry repository (`host[:port]/path`) for StatefulSets' pods and node shutdowns. |
| `operator.plainHTTP` | `false` | The registry without TLS. |
| `operator.extraArgs` | `[]` | More of the operator's [flags](https://makhov.github.io/gcr/operator/#flags). |
| `operator.nodeSelector`, `.tolerations`, `.affinity` | | Where it runs: keep it off nodes that shut down for good. |
| `operator.resources` | `{}` | |
| `createNamespace` | `false` | Also create the namespace (for the generated `deploy/` manifests). With helm, use `--create-namespace`. |

`deploy/gcr.yaml` and `deploy/gcr-operator.yaml`, the manifests for
`kubectl apply`, are generated from this chart (`make manifests`).
