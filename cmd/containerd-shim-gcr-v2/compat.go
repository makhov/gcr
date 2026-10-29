//go:build linux

package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"

	bootapi "github.com/containerd/containerd/api/runtime/bootstrap/v1"
	"github.com/containerd/containerd/v2/pkg/protobuf/proto"
)

// adaptStart lets callers that predate the shim bootstrap protocol start
// the shim: containerd before 2.3, and CRI-O's VM runtime type. They run
// `start` with what the protocol's BootstrapParams carry in flags and
// the TTRPC_ADDRESS variable, the runtime options (or nothing) on stdin,
// and read back JSON naming the address and the task API version.
// containerd's shim library speaks only the protocol, so for them stdin
// is rewritten as BootstrapParams before the library reads it, and the
// BootstrapResult it writes is rewritten afterwards (finish).
//
// They are told task API version 2, which every one of them speaks; the
// shim serves version 3 as well, under its other name.
func adaptStart() (finish func() error, err error) {
	fs := flag.NewFlagSet(os.Args[0], flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var (
		debug         bool
		ns, id, addr  string
		ignoredString string
		ignoredBool   bool
	)
	fs.BoolVar(&debug, "debug", false, "")
	fs.StringVar(&ns, "namespace", "", "")
	fs.StringVar(&id, "id", "", "")
	fs.StringVar(&addr, "address", "", "")
	for _, f := range []string{"socket", "debug-socket", "bundle", "publish-binary"} {
		fs.StringVar(&ignoredString, f, "", "")
	}
	for _, f := range []string{"v", "version", "info"} {
		fs.BoolVar(&ignoredBool, f, false, "")
	}
	if err := fs.Parse(os.Args[1:]); err != nil || fs.Arg(0) != "start" {
		return nil, nil // the library says what is wrong
	}

	input, err := io.ReadAll(io.LimitReader(os.Stdin, 10<<20))
	if err != nil {
		return nil, err
	}
	var params bootapi.BootstrapParams
	if proto.Unmarshal(input, &params) == nil && params.InstanceID == id && params.Namespace == ns {
		return nil, replaceStdin(input) // the protocol: as it came
	}

	params = bootapi.BootstrapParams{
		InstanceID:             id,
		Namespace:              ns,
		ContainerdGrpcAddress:  addr,
		ContainerdTtrpcAddress: os.Getenv("TTRPC_ADDRESS"),
		LogLevel:               bootapi.LogLevel_LOG_LEVEL_INFO,
	}
	if debug {
		params.LogLevel = bootapi.LogLevel_LOG_LEVEL_DEBUG
	}
	b, err := proto.Marshal(&params)
	if err != nil {
		return nil, err
	}
	if err := replaceStdin(b); err != nil {
		return nil, err
	}
	stdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	os.Stdout = w
	return func() error {
		os.Stdout = stdout
		w.Close()
		b, err := io.ReadAll(r)
		if err != nil {
			return err
		}
		var res bootapi.BootstrapResult
		if err := proto.Unmarshal(b, &res); err != nil {
			return fmt.Errorf("bootstrap result: %w", err)
		}
		return json.NewEncoder(stdout).Encode(struct {
			Version  int    `json:"version"`
			Address  string `json:"address"`
			Protocol string `json:"protocol"`
		}{2, res.Address, res.Protocol})
	}, nil
}

// replaceStdin makes os.Stdin read b.
func replaceStdin(b []byte) error {
	r, w, err := os.Pipe()
	if err != nil {
		return err
	}
	go func() {
		_, _ = w.Write(b)
		w.Close()
	}()
	os.Stdin = r
	return nil
}
