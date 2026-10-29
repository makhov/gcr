//go:build e2e && linux

// hog fills a chunk of anonymous memory with a pattern, then counts in a
// loop: a snapshot of it is as large as the chunk, so a fork of it shows
// what restoring memory costs, and the pattern shows the memory came back
// intact however it was restored. The size comes from the first argument,
// in MiB.
package main

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

const pageSize = 4096

func main() {
	mib, err := strconv.Atoi(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, "usage: hog MiB")
		os.Exit(2)
	}
	// One byte per page carries the pattern; every page is touched, so
	// the snapshot holds them all.
	chunk := make([]byte, mib<<20)
	for i := 0; i < len(chunk); i += pageSize {
		chunk[i] = byte(i / pageSize % 251)
	}
	write("/tmp/ready", strconv.Itoa(mib))

	for i := 1; ; i++ {
		// Read the pattern back from a page that moves through the chunk,
		// so a fork keeps faulting pages in as it runs, and say if the
		// memory came back wrong.
		at := (i * 1021 * pageSize) % len(chunk)
		if chunk[at] != byte(at/pageSize%251) {
			write("/tmp/count", "corrupt at "+strconv.Itoa(at))
			os.Exit(1)
		}
		write("/tmp/count", strconv.Itoa(i))
		time.Sleep(200 * time.Millisecond)
	}
}

// write replaces a file's contents in one step, so a reader never sees it
// empty.
func write(path, s string) {
	if err := os.WriteFile(path+".new", []byte(s), 0o644); err != nil {
		return
	}
	_ = os.Rename(path+".new", path)
}
