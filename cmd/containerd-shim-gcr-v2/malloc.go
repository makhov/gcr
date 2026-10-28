//go:build linux

package main

/*
#include <malloc.h>

// One malloc arena for the whole process. The shim's C code (libseccomp's
// notification buffers, among others) runs on whichever thread the Go
// scheduler picks, and glibc would give each thread an arena of its own:
// a 2 MiB huge page apiece where transparent huge pages are on, most of an
// idle shim's memory. Runs before any thread starts; containers the shim
// execs start with fresh defaults.
__attribute__((constructor)) static void gcr_one_malloc_arena(void)
{
	mallopt(M_ARENA_MAX, 1);
}
*/
import "C"
