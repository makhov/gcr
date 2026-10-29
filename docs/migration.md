# Live migration

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
gcr migrate -token $TOKEN ID http://TARGET-HOST:7781      # https:// with MigrationTLS (below)
```

With user namespaces, create the target with the source's range
(`--annotation gcr.dev/userns-base=N`; a refused migration prints N).

When the kernel tracks dirty memory (x86, s390, powerpc; not arm64), the
source pre-copies memory in rounds to CRIU page servers on the target while
it keeps running. It then freezes, sends the remaining memory, the rest of
the checkpoint and the writable layer, and the target restores. The source
stays frozen until the target runs and is thawed if anything fails before;
afterwards it is killed. Open TCP connections reset.

Everything goes over the one address the target listens on. Its page
servers listen on loopback only, and each round's memory is carried to
them over the migration's connection (an HTTP Upgrade), after the token
is checked. Without TLS that connection is in clear text, memory
included. To encrypt it and have each side verify the other, give both
runtime handlers certificates from one CA, and migrate to `https://`:

```toml
[plugins.'io.containerd.cri.v1.runtime'.containerd.runtimes.gcr.options.MigrationTLS]
  Cert = '/etc/gcr/tls/node.crt'   # valid for the address sources connect to
  Key = '/etc/gcr/tls/node.key'
  CA = '/etc/gcr/tls/ca.crt'       # the CA both sides trust
```

The target then serves only TLS 1.3, and asks every source for a
certificate signed by the CA. A source with `MigrationTLS` set refuses
`http://` targets. The token still has to match: the certificate says
the source is a node of the cluster, and the token that it is the one
meant to send this container.
