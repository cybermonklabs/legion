package docs

import (
	"context"
	"slices"
	"testing"
)

// Two project sets whose keys collide in hashtext in opposite key order - hashtext('HNDC') =
// hashtext('KLVN') and hashtext('CCKH') = hashtext('MBHM') - must take their copy locks in one
// id order, or two holders would wait on each other in a cycle. Both sets put the shared id
// -2012274736 first.
func TestCopyProjectsInLockOrderFollowsTheLockID(t *testing.T) {
	service, _ := newTestService(t)
	ctx := context.Background()
	tx, err := service.store.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)
	for _, c := range []struct{ in, want []string }{
		{[]string{"CCKH", "KLVN", "CCKH"}, []string{"KLVN", "CCKH"}},
		{[]string{"MBHM", "HNDC"}, []string{"HNDC", "MBHM"}},
	} {
		got, err := copyProjectsInLockOrder(ctx, tx, c.in)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(got, c.want) {
			t.Fatalf("copyProjectsInLockOrder(%v) = %v, want %v", c.in, got, c.want)
		}
	}
}
