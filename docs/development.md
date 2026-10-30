# Development

## A development VM

Everything builds and runs in a Linux VM (kernel 6.12+, containerd 2.4, CRIU):

```sh
limactl start --name=gcr hack/lima.yaml   # once
limactl restart gcr                       # once: boots with the BPF LSM (netlink policy)
make vm T=criu                            # once: build and install the CRIU fork from ../criu
make vm T=test-e2e                        # build, install, run the end-to-end tests
```

The CRIU fork is expected next to this repository (`../criu`): a clone of
[makhov/criu](https://github.com/makhov/criu) at the tag in
`hack/criu-version` (`git -C ../criu checkout v4.2.1+gcr.0`), or on its
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

## Releases

`make release` (`hack/release.sh`) builds, for linux/amd64 and
linux/arm64, one file each, `dist/gcr-VERSION-linux-ARCH`, and
`dist/SHA256SUMS`. Each is the `gcr` command, static, carrying what
`gcr setup` installs (`internal/payload`, compressed): the shim, static,
and gcr's CRIU with its CUDA plugin. All are stamped with the version;
`gcr version` says which CRIU commit a release carries.

- **CRIU** is makhov/criu at the tag in `hack/criu-version`
  (`RELEASE_CRIU_SRC=DIR` builds a tree instead, to try a change). It is built in
  an Ubuntu 22.04 container (`hack/criu-build.sh`, `CRIU_IMAGE`) with its
  libraries linked in and glibc dynamic (`STATIC=libs`), so it runs on
  glibc 2.35 and later and loads its plugins.
- **The host** needs Docker (`DOCKER="sudo docker"` where its socket is
  root's), `zstd`, and Go with cgo for both architectures. On the arm64 dev
  VM, building amd64 takes Ubuntu's cross compiler and amd64 libseccomp:

  ```sh
  sudo dpkg --add-architecture amd64 && sudo apt-get update
  sudo apt-get install gcc-x86-64-linux-gnu libc6-dev-amd64-cross libseccomp-dev:amd64
  ```

  (on an amd64 host, `gcc-aarch64-linux-gnu`, `libc6-dev-arm64-cross` and
  `libseccomp-dev:arm64`).

It needs no CI. To cut one:

```sh
git tag v0.1.0 && git push origin v0.1.0
make vm T='release VERSION=v0.1.0 DOCKER="sudo docker"'
make release-publish VERSION=v0.1.0   # a draft GitHub release, to look over and publish
```

