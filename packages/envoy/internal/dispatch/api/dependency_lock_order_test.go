package api

import (
	"context"
	"slices"
	"testing"
	"time"
)

// Two project sets whose keys collide in hashtext in opposite key order - hashtext('HNDC') =
// hashtext('KLVN') and hashtext('CCKH') = hashtext('MBHM') - must take their dependency locks in
// one id order, or two writes would wait on each other in a cycle. Both sets put the shared id
// -2012274736 first.
func TestProjectsInLockOrderFollowsTheDependencyLockID(t *testing.T) {
	_, database, _ := newTestServer(t, testServerOptions{settle: time.Hour})
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	for _, c := range []struct{ in, want []string }{
		{[]string{"CCKH", "KLVN", "CCKH"}, []string{"KLVN", "CCKH"}},
		{[]string{"MBHM", "HNDC"}, []string{"HNDC", "MBHM"}},
	} {
		got, err := projectsInLockOrder(ctx, tx, c.in)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(got, c.want) {
			t.Fatalf("projectsInLockOrder(%v) = %v, want %v", c.in, got, c.want)
		}
	}
}
