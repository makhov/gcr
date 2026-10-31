//go:build e2e && linux

package framework

import (
	"slices"
	"testing"
)

func TestEexistPages(t *testing.T) {
	log := `(00.215510) uffd: 1-9: uffd_copy: 0xc000123000/4096
(00.215524) uffd: 1-9: copy: mcopy_rc:-17, errno:17
(00.215530) uffd: 1-9: uffd_copy: 0xc000123000/4096
(00.215538) uffd: 1-9: copy: mcopy_rc:-17, errno:17
(00.215540) uffd: 1-9: uffd_copy: 0xc000200000/8192
(00.215541) uffd: 1-9: uffd_copy: 0xc000300000/4096
(00.215550) uffd: 1-9: copy: mcopy_rc:-17, errno:17`
	if got, want := eexistPages(log), []string{"c000123", "c000300"}; !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestRestoreLines(t *testing.T) {
	log := `(00.036577)      1: vma 0x2e388c000000 0x2e388dc00000
(00.036590)      1: vma 0x589000 0x5c0000
(00.034000)      1: 	premap 0x00000000589000-0x000000005c0000 -> 00007f0000000000
(00.037100) pr1-3 Read 58a000 1 pages
(00.037105) pr1-3 Read 58a000 3 pages
(00.037116)      1: Lazy restore skips 2 pages at 58b000
(00.037120) pr1-3 Read 58d000 1 pages`
	got := restoreLines(log, 0x58b000)
	want := []string{
		"(00.036590)      1: vma 0x589000 0x5c0000",
		"(00.034000)      1: 	premap 0x00000000589000-0x000000005c0000 -> 00007f0000000000",
		"(00.037105) pr1-3 Read 58a000 3 pages",
		"(00.037116)      1: Lazy restore skips 2 pages at 58b000",
	}
	if !slices.Equal(got, want) {
		t.Errorf("got\n%v\nwant\n%v", got, want)
	}
}
