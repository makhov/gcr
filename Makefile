BIN := bin/containerd-shim-gcr-v2
VM  := gcr
# Run a command in the dev VM, in the same directory as here.
IN_VM = limactl shell --workdir $(CURDIR) $(VM) --

.PHONY: build install vet test-e2e vm

build:
	CGO_ENABLED=1 go build -o $(BIN) ./cmd/containerd-shim-gcr-v2

install: build
	sudo install -m 0755 $(BIN) /usr/local/bin/

vet:
	go vet ./...

test-e2e: install
	sudo ./hack/e2e.sh

# From macOS: run any target inside the VM, e.g. `make vm T=test-e2e`.
vm:
	$(IN_VM) make $(T)
