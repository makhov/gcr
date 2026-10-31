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
