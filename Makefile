BIN := bin/containerd-shim-gcr-v2
CLI := bin/gcr
# libcontainer only applies seccomp profiles when built with this tag.
TAGS := seccomp
VM  := gcr
# Run a command in the dev VM, in the same directory as here.
IN_VM = limactl shell --workdir $(CURDIR) $(VM) --

.PHONY: build install vet test-e2e vm criu

build:
	CGO_ENABLED=1 go build -tags $(TAGS) -o $(BIN) ./cmd/containerd-shim-gcr-v2
	CGO_ENABLED=1 go build -tags $(TAGS) -o $(CLI) ./cmd/gcr

install: build
	sudo install -m 0755 $(BIN) $(CLI) /usr/local/bin/

vet:
	go vet -tags $(TAGS) ./...

# Builds and installs our CRIU fork (seccomp listener restore) as `criu`,
# from a checkout next to this repo.
CRIU_SRC ?= ../criu
criu:
	$(MAKE) -C $(CRIU_SRC) criu
	sudo install -m 0755 $(CRIU_SRC)/criu/criu /usr/local/sbin/criu

test-e2e: install
	sudo ./hack/e2e.sh
	sudo ./hack/e2e-timetravel.sh
	sudo ./hack/e2e-migrate.sh

# From macOS: run any target inside the VM, e.g. `make vm T=test-e2e`.
vm:
	$(IN_VM) make $(T)
