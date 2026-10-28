module github.com/makhov/gcr

go 1.27.0

require (
	github.com/checkpoint-restore/go-criu/v8 v8.3.0
	github.com/containerd/cgroups/v3 v3.1.3
	github.com/containerd/console v1.0.5
	github.com/containerd/containerd/api v1.12.0
	github.com/containerd/containerd/v2 v2.4.2
	github.com/containerd/errdefs v1.0.0
	github.com/containerd/errdefs/pkg v0.3.0
	github.com/containerd/fifo v1.1.0
	github.com/containerd/log v0.2.0
	github.com/containerd/plugin v1.1.0
	github.com/containerd/ttrpc v1.2.9
	github.com/containerd/typeurl/v2 v2.3.0
	github.com/cyphar/filepath-securejoin v0.7.0
	github.com/opencontainers/cgroups v0.1.0
	github.com/opencontainers/runc v1.5.2
	github.com/opencontainers/runtime-spec v1.3.0
	github.com/pelletier/go-toml/v2 v2.4.3
	github.com/seccomp/libseccomp-golang v0.12.0
	github.com/sirupsen/logrus v1.10.2
	golang.org/x/sys v0.48.0
	google.golang.org/grpc v1.83.2
	google.golang.org/protobuf v1.36.12
	k8s.io/cri-api v0.37.0
)

require (
	cyphar.com/go-pathrs v0.2.6 // indirect
	github.com/Microsoft/go-winio v0.6.3-0.20251027160822-ad3df93bed29 // indirect
	github.com/Microsoft/hcsshim v0.15.0-rc.4 // indirect
	github.com/aperturerobotics/protobuf-go-lite v0.14.0 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/cilium/ebpf v0.17.3 // indirect
	github.com/containerd/continuity v0.5.0 // indirect
	github.com/containerd/go-runc v1.2.1 // indirect
	github.com/containerd/log/otel v0.1.0 // indirect
	github.com/containerd/platforms v1.0.0-rc.5 // indirect
	github.com/coreos/go-systemd/v22 v22.7.0 // indirect
	github.com/distribution/reference v0.6.0 // indirect
	github.com/felixge/httpsnoop v1.1.0 // indirect
	github.com/go-logr/logr v1.4.4 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	github.com/godbus/dbus/v5 v5.2.2 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/klauspost/compress v1.20.0 // indirect
	github.com/mdlayher/socket v0.6.0 // indirect
	github.com/mdlayher/vsock v1.3.0 // indirect
	github.com/moby/locker v1.0.1 // indirect
	github.com/moby/sys/capability v0.4.0 // indirect
	github.com/moby/sys/mountinfo v0.7.2 // indirect
	github.com/moby/sys/signal v0.7.1 // indirect
	github.com/moby/sys/user v0.4.1 // indirect
	github.com/moby/sys/userns v0.2.1 // indirect
	github.com/mrunalp/fileutils v0.5.1 // indirect
	github.com/opencontainers/go-digest v1.0.0 // indirect
	github.com/opencontainers/image-spec v1.1.1 // indirect
	github.com/opencontainers/selinux v1.15.1 // indirect
	github.com/pkg/errors v0.9.1 // indirect
	github.com/vishvananda/netlink v1.3.1 // indirect
	github.com/vishvananda/netns v0.0.5 // indirect
	go.opentelemetry.io/auto/sdk v1.2.1 // indirect
	go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp v0.71.0 // indirect
	go.opentelemetry.io/otel v1.46.0 // indirect
	go.opentelemetry.io/otel/metric v1.46.0 // indirect
	go.opentelemetry.io/otel/sdk v1.46.0 // indirect
	go.opentelemetry.io/otel/trace v1.46.0 // indirect
	golang.org/x/net v0.58.0 // indirect
	golang.org/x/sync v0.23.0 // indirect
	golang.org/x/text v0.41.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260825221802-da73d73af1c5 // indirect
)

replace github.com/opencontainers/runc => github.com/makhov/runc v0.0.0-20261028125522-2e2f678fc660
