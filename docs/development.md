---
# The variables this page explains are left as they are written
# (mkdocs.yml's macros plugin).
render_macros: false
---

# Development

## A development VM

This VM is for building gcr. To *use* a release on a laptop instead,
`hack/try-vm.yaml` is a plain Ubuntu VM with containerd and Docker
([a VM on your laptop](install.md#a-vm-on-your-laptop)).

Everything builds and runs in a Linux VM (kernel 6.12+, containerd 2.4, CRIU):

```sh
limactl start --name=gcr hack/lima.yaml   # once
limactl restart gcr                       # once: boots with the BPF LSM (netlink policy)
make vm T=criu                            # once: build and install the CRIU fork from ../criu
make vm T=test-e2e                        # build, install, run the end-to-end tests
```

The CRIU fork is expected next to this repository (`../criu`): a clone of
[makhov/criu](https://github.com/makhov/criu) at the tag in
`hack/criu-version` (`git -C ../criu checkout v4.2.1+gcr.1`), or on its
`gcr` branch to change it. Changes go to that branch as pull requests, and
a new tag (`v4.2.1+gcr.1`, ...) in `hack/criu-version` releases them. The
runc fork comes in through `go.mod`. [Architecture](architecture.md) says what each package
does.

For the CRI conformance suite:
`critest --runtime-handler gcr` (needs CNI plugins and the `gcr` runtime in
containerd's CRI config).

## Tests

| | What | Needs |
| --- | --- | --- |
| `make test` | unit tests | Linux, libseccomp |
| `make test-e2e` | end-to-end tests in `test/e2e`, below | root (via sudo), containerd, CNI, the CRIU fork, network |
| `critest --runtime-handler gcr` | CRI conformance | root, containerd, CNI |
| `make test-k8s` | Kubernetes (stock k0s via k0da) with a gcr RuntimeClass | docker or podman, k0da >= v0.4.0 |

The end-to-end tests are Go tests behind the `e2e` build tag, one directory
per test, sharing a harness in `test/e2e/framework` (containerd's client,
CRI, checkpoint/restore, static helper programs to run in containers):

| `test/e2e/` | What |
| --- | --- |
| `checkpoint` | a counter carries on after checkpoint/restore, in the same user namespace |
| `timetravel` | a timeline, a rewind; the counter, the filesystem and the recording carry on from the point |
| `gpu` | checkpoint and restore of CUDA state, and forks of a model in GPU memory (needs a GPU node; skipped elsewhere); a GPU gcr cannot describe is refused checkpoints |
| `fork` | ten containers from one snapshot: each resumes its memory and mounts the snapshot's layer beneath its own; forks of forks; a fork's identity and signal; lazy restore, and what it saves; merged memory; a snapshot of a fork through a registry, forked after everything local is gone |
| `workloads` | real programs forked: Python, Node.js and JVM servers holding a model in memory, and Postgres with a table; each fork serves, keeps its state and diverges; a re-seed on the fork signal |
| `migrate` | live migration between containerd namespaces (pre-copy on x86); a refused one leaves the source running |
| `hardening` | refused system calls with hardening on and off; supervised mounts, by exact return value; nested user namespaces; the netlink policy (BPF LSM) |
| `landlock` | a leaked host descriptor, with Landlock on and off, across a restore |
| `dind` | Docker-in-Docker, unprivileged, in a user namespace |
| `cri` | a pod through CRI: exec, exit codes, logs, updates, stats, user namespace |
| `docker` | `docker run --runtime gcr`: run, exit codes, the user namespace, a published port, exec, stop and start; a snapshot on the bridge network forked into a container Docker gives an address; a rewind with `--network none`, and one refused on a bridge network; skipped without dockerd (`hack/docker-setup.sh`) |
| `crio` | a pod under CRI-O (runtime type `vm`): exec, exit codes, logs, the container is gcr's, stop and remove end the shim; a snapshot forked into another pod, a fork of the fork, and the snapshots held while forks use them; time travel refused; skipped without CRI-O (`hack/crio-setup.sh`) |

`sudo hack/clean-e2e.sh` removes what a failed run left behind (a leaked
counter container keeps writing, which slows the next run down). Runs
take a lock (`/tmp/gcr-e2e.lock`), so a run started while another is in
progress waits rather than competing with it for the machine.
`make test-e2e` runs them all, each package's test binary as root, one
package at a time; `make test-e2e E2E='-run Landlock ./test/e2e/landlock'`
picks some. `sudo hack/ci-setup.sh` prepares a host for them (containerd,
CNI, cri-tools, the CRIU fork). CI runs everything on every pull request,
except the netlink policy's own tests: they need the BPF LSM in the
kernel's `lsm=` list, which a GitHub-hosted runner cannot have, so the
`bpf-lsm` workflow runs them on a self-hosted runner (on main, on request,
and on a pull request labelled `run-bpf`). To set one up:

```sh
REPO=makhov/gcr TOKEN=$(gh api -X POST \
  repos/makhov/gcr/actions/runners/registration-token -q .token) \
  sudo -E ./hack/runner-setup.sh      # adds bpf to lsm=, registers a service
```

It is a host the repository's workflows execute code on, so use one you
are willing to give everyone with write access.

## Documentation

The site at [makhov.github.io/gcr](https://makhov.github.io/gcr/) is
built from `docs/` with mkdocs (`mkdocs.yml`). To see a change before
pushing it:

```sh
make docs-serve-dev        # http://localhost:8000/gcr/, rebuilt as you edit; needs Docker
make docs                  # builds site/ as CI does: broken links fail it; needs mkdocs
```

`DOCS_DEV_PORT=8001` serves it on another port. `make docs` needs
mkdocs on your machine (`pip install -r docs/requirements.txt`).

No page names a release. The version a page shows comes from the latest
release tag, through two variables (mkdocs.yml's macros plugin and
`hack/mkdocs_macros.py`):

| | |
| --- | --- |
| `{{{ gcr_version }}}` | the release, `v1.2.3` |
| `{{{ gcr_image }}}` | its installer image, `ghcr.io/makhov/gcr:v1.2.3` |

Use them wherever a version would otherwise be written in, output
pasted into a page included. `GCR_VERSION=v0.2.0 make docs` builds the
site for another one, and `hack/docs-no-version.sh` (which CI runs) fails
when a page names the current release instead of the variable. Publishing
a release rebuilds the site, so the pages follow it.

## Releases

`make release` (`hack/release.sh`) builds, for linux/amd64 and
linux/arm64, one file each, `dist/gcr-VERSION-linux-ARCH`, and
`dist/SHA256SUMS`; and the operator, the kubectl plugin, the Kubernetes
manifests and the node shutdown guard's systemd unit. Each is the `gcr` command, static, carrying what
`gcr setup` installs (`internal/payload`, compressed): the shim, static,
and gcr's CRIU with its CUDA plugin. All are stamped with the version;
`gcr version` says which CRIU commit a release carries.

- **CRIU** is makhov/criu at the tag in `hack/criu-version`
  (`RELEASE_CRIU_SRC=DIR` builds a tree instead, to try a change). It is built in
  an Ubuntu 22.04 container (`hack/criu-build.sh`, `CRIU_IMAGE`) with its
  libraries linked in and glibc dynamic (`STATIC=libs`), so it runs on
  glibc 2.35 and later and loads its plugins; and again wholly static
  (`STATIC=1`), for hosts without glibc, with a static GNU tar (1.35,
  checksum pinned) for CRIU, as busybox's tar lacks options CRIU uses.
- **The host** needs Docker (`DOCKER="sudo docker"` where its socket is
  root's), `zstd`, and Go with cgo for both architectures. On the arm64 dev
  VM, building amd64 takes Ubuntu's cross compiler and amd64 libseccomp:

  ```sh
  sudo dpkg --add-architecture amd64 && sudo apt-get update
  sudo apt-get install gcc-x86-64-linux-gnu libc6-dev-amd64-cross libseccomp-dev:amd64
  ```

  (on an amd64 host, `gcc-aarch64-linux-gnu`, `libc6-dev-arm64-cross` and
  `libseccomp-dev:arm64`).

A release is cut by a tag; CI does the rest
(`.github/workflows/release.yml`):

```sh
git tag -s v0.1.0 -m v0.1.0 && git push origin v0.1.0
```

What the tag starts, in order:

1. **CI has to pass on the tagged commit.** The tag may arrive before
   that run finishes: the release waits for it, and stops if it fails.
2. **Each architecture builds on a runner of its own**, amd64 and arm64,
   so nothing cross-compiles and CRIU is built natively.
3. **The installer image** `ghcr.io/makhov/gcr:vX.Y.Z` is pushed for both
   architectures and signed (cosign, keyless).
4. **A draft release** gets the files, and notes generated from the merged
   pull requests.
5. **Publishing that draft** is the one step left by hand, and the rest
   follows from it: `install.sh` installs the new release, the
   documentation site is rebuilt for it, and the version goes into the
   chart's `appVersion` and the manifests generated from it
   (`hack/stamp-version.sh`), as a commit on main.

A tag with a `-` in it (`v0.2.0-rc1`) is marked a pre-release, and is not
stamped into the chart: a clone keeps installing the last full release.
Nothing else in the repository names a version, so there is nothing to
bump before or after a release.

`make release VERSION=vX.Y.Z` builds the same files on a machine of your
own, for a release CI cannot build, and `make release-publish
VERSION=vX.Y.Z` uploads them to a draft release. That is what the dev VM
is for:

```sh
make vm T='release VERSION=v0.1.0 DOCKER="sudo docker"'
make vm T='image VERSION=v0.1.0 DOCKER="sudo docker" PUSH=1'
```

`make image` builds the installer image (`deploy/Dockerfile`: busybox and
the release's `gcr`) from `dist/` with buildx; it copies files only, so it
builds both architectures anywhere. `make test-install` builds a release
and image for the host's architecture and has the DaemonSet install them
on a one-node k0da cluster (`hack/e2e-install.sh`).

**The Helm chart** has releases of its own. A tag `chart/vX.Y.Z`
publishes `charts/gcr` to `oci://ghcr.io/makhov/charts/gcr` as version
`X.Y.Z` (`.github/workflows/chart-release.yml`), installing the latest
release's image (its `appVersion` is the latest `vX.Y.Z` tag, stamped at
publish time). After a release, tag a chart release too, so that the
published chart carries the new resources as well as the new image:

```sh
git tag chart/v0.2.0 && git push origin chart/v0.2.0
```

The Kubernetes manifests in `deploy/` (`gcr.yaml`, `gcr-operator.yaml`)
are generated from the chart in `charts/gcr`: change the chart, then run
`make manifests`. CI fails when they are out of date.

