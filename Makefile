BIN := bin/containerd-shim-gcr-v2
CLI := bin/gcr
# libcontainer only applies seccomp profiles when built with this tag.
TAGS := seccomp
VM  := gcr
# Run a command in the dev VM, in the same directory as here.
IN_VM = limactl shell --workdir $(CURDIR) $(VM) --

.PHONY: build static install vet test test-e2e test-k8s vm criu

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
E2E ?= ./test/e2e/...
test-e2e: install
	go test -exec sudo -p 1 -count 1 -timeout 30m -tags "e2e $(TAGS)" -v $(E2E)

# Kubernetes on gcr: a k0da (k0s in containers) cluster on the stock k0s
# image, with gcr bind-mounted and registered by a containerd drop-in.
test-k8s: static
	./hack/e2e-k8s.sh

# From macOS: run any target inside the VM, e.g. `make vm T=test-e2e`.
vm:
	$(IN_VM) make $(T)
