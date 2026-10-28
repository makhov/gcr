BIN := bin/containerd-shim-gcr-v2
CLI := bin/gcr
# libcontainer only applies seccomp profiles when built with this tag.
TAGS := seccomp
VM  := gcr
# Run a command in the dev VM, in the same directory as here.
IN_VM = limactl shell --workdir $(CURDIR) $(VM) --

.PHONY: build static install vet test test-e2e test-k8s vm criu bpf

build:
	CGO_ENABLED=1 go build -tags $(TAGS) -o $(BIN) ./cmd/containerd-shim-gcr-v2
	CGO_ENABLED=1 go build -tags $(TAGS) -o $(CLI) ./cmd/gcr

# Static binaries, for nodes without glibc (k0s's image is Alpine).
STATIC_LDFLAGS := -linkmode external -extldflags "-static"
static:
	CGO_ENABLED=1 go build -tags "$(TAGS) netgo osusergo" -ldflags '$(STATIC_LDFLAGS)' -o bin/static/containerd-shim-gcr-v2 ./cmd/containerd-shim-gcr-v2
	CGO_ENABLED=1 go build -tags "$(TAGS) netgo osusergo" -ldflags '$(STATIC_LDFLAGS)' -o bin/static/gcr ./cmd/gcr

install: build
	sudo install -m 0755 $(BIN) $(CLI) /usr/local/bin/

vet:
	go vet -tags $(TAGS) ./...

# The netlink policy (BPF LSM), compiled with CO-RE so one object fits
# every kernel with BTF; it is embedded in the shim and checked in.
bpf:
	clang -O2 -g -Wall -target bpf -I/usr/include/$(shell uname -m)-linux-gnu -D__TARGET_ARCH_$(shell uname -m | sed 's/x86_64/x86/;s/aarch64/arm64/') \
		-c internal/netpolicy/bpf/netlink.bpf.c -o internal/netpolicy/netlink.bpf.o
	llvm-strip -g internal/netpolicy/netlink.bpf.o
	# The source the embedded object was built from, so a test can tell
	# when the two have drifted apart (the object itself differs between
	# clang versions, which says nothing).
	cd internal/netpolicy && sha256sum bpf/netlink.bpf.c > netlink.bpf.sha256

test:
	go test -tags $(TAGS) ./...

# Builds and installs our CRIU fork (seccomp listener restore) as `criu`,
# from a checkout next to this repo.
CRIU_SRC ?= ../criu
criu:
	$(MAKE) -C $(CRIU_SRC) criu
	sudo install -m 0755 $(CRIU_SRC)/criu/criu /usr/local/sbin/criu

# End-to-end tests (test/e2e, one directory per test) against the
# installed runtime, each package's binary run as root, one at a time.
# Pick tests with E2E, e.g. `make test-e2e E2E='-run Landlock ./test/e2e/landlock'`.
# The tests run containers, restart nothing but install binaries, and
# measure time; two runs at once on one machine make each other flaky, and
# the self-hosted runner shares the dev VM with whoever is using it. A lock
# makes them wait instead.
E2E ?= ./test/e2e/...
E2E_LOCK ?= /tmp/gcr-e2e.lock
test-e2e: install
	flock $(E2E_LOCK) go test -exec sudo -p 1 -count 1 -timeout 30m -tags "e2e $(TAGS)" -v $(E2E)

# Kubernetes on gcr: a k0da (k0s in containers) cluster on the stock k0s
# image, with gcr bind-mounted and registered by a containerd drop-in.
test-k8s: static
	./hack/e2e-k8s.sh

# From macOS: run any target inside the VM, e.g. `make vm T=test-e2e`.
vm:
	$(IN_VM) make $(T)
