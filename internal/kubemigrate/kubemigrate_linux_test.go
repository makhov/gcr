package kubemigrate

import (
	"encoding/json"
	"testing"

	"github.com/makhov/gcr/internal/control"
	"github.com/makhov/gcr/internal/migrate"
	"github.com/makhov/gcr/internal/userns"
)

// The names this package keeps to itself are gcr's.
func TestAgreesWithGCR(t *testing.T) {
	for got, want := range map[string]string{
		annotationMigrateIn:    migrate.AnnotationIn,
		annotationMigrateToken: migrate.AnnotationToken,
		annotationUsernsBase:   userns.AnnotationBase,
		annotationHostUsers:    userns.AnnotationHostUsers,
	} {
		if got != want {
			t.Errorf("%q here, %q in gcr", got, want)
		}
	}
	b, _ := json.Marshal(control.Info{IDBase: 7})
	var inf info
	if err := json.Unmarshal(b, &inf); err != nil || inf.IDBase != 7 {
		t.Errorf("gcr info's %s reads as %+v, %v", b, inf, err)
	}
}
