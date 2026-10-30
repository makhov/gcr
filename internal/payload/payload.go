// Package payload is what a release build of gcr carries for gcr setup to
// install: the shim, gcr's CRIU and CRIU's CUDA plugin, each compressed
// (zstd) into files/ by hack/release.sh before gcr is built with the
// gcr_payload tag. Other builds carry nothing.
package payload

import (
	"bytes"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/klauspost/compress/zstd"
)

// The files a payload has, as installed.
const (
	Shim       = "containerd-shim-gcr-v2"
	CRIU       = "criu"
	CUDAPlugin = "cuda_plugin.so"
	// Versions says which commits the files were built from.
	Versions = "VERSIONS"
)

// Has says whether this build carries name.
func Has(name string) bool {
	_, err := fs.Stat(files, "files/"+name+".zst")
	return err == nil
}

// Read returns name's contents.
func Read(name string) ([]byte, error) {
	b, err := fs.ReadFile(files, "files/"+name+".zst")
	if err != nil {
		return nil, err
	}
	zr, err := zstd.NewReader(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	return io.ReadAll(zr)
}

// Install writes name to path with mode, atomically: a binary in use (a
// running shim's) is replaced, not overwritten.
func Install(name, path string, mode os.FileMode) error {
	b, err := Read(name)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
