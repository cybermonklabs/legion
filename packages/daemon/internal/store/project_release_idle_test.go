package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/pods"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// A tick of the idle rules that read the daemon's mark just before the project release set it can
// still reach ReleaseIdleIssue after the release's own transaction committed. The two take one
// serializer, so the tick then reads the release's row and starts nothing — neither the idle
// release nor the release of a pod no claim owns — and it asks its rule nothing. Another project's
// release holds none of this project's pods, and once a boot has ended the release the same
// release starts as before.
func TestAnIdleReleaseIsRefusedWhileAProjectReleaseNoBootEndedRuns(t *testing.T) {
	t.Parallel()
	st := migratedStore(t)
	ctx := context.Background()
	openRunningTree(t, st)
	token := claim.Token("legion-legion-legion-208-implementer")
	if err := st.PutClaim(ctx, supervise.Claim{Token: token, Project: "legion", Tree: rootIssue, Issue: rootIssue,
		Role: claim.RoleImplementer, TreeEpoch: 1, Generation: 1, State: supervise.StateIdle}); err != nil {
		t.Fatal(err)
	}
	idleRows := func(view pods.IdleView) IdleReleaseMark {
		return IdleReleaseMark{Generation: view.Generation, Role: claim.RoleImplementer, Rows: []IdleReleaseRow{
			{Kind: "issue_bundle", Payload: []byte(`{"tree":"LEGION-208","generation":1,"treeGeneration":1,"role":"implementer"}`)},
			{Kind: "supervise", Payload: []byte(`{"op":"release","tree":"LEGION-208","role":"implementer","generation":1}`), Boundary: true},
			{Kind: "issue_release", Payload: []byte(`{"tree":"LEGION-208","generation":1,"treeGeneration":1}`)},
		}}
	}
	unownedRows := func(view pods.IdleView) IdleReleaseMark {
		return IdleReleaseMark{Generation: view.Generation, SuspendsNoRole: true, Rows: []IdleReleaseRow{
			{Kind: "issue_release", Payload: []byte(`{"tree":"LEGION-208","generation":1,"treeGeneration":1}`), Boundary: true},
		}}
	}
	written := func() (rows, marks int) {
		t.Helper()
		if err := st.Pool().QueryRow(ctx, `select count(*) from outbox where issue = $1`, rootIssue).Scan(&rows); err != nil {
			t.Fatal(err)
		}
		if err := st.Pool().QueryRow(ctx, `select count(*) from issue_releases where project = 'legion'`).Scan(&marks); err != nil {
			t.Fatal(err)
		}
		return rows, marks
	}

	if _, _, _, err := st.StartProjectRelease(ctx, "other", atReleaseStart, releaseLength); err != nil {
		t.Fatalf("start another project's release: %v", err)
	}
	if _, _, _, err := st.StartProjectRelease(ctx, "legion", atReleaseStart, releaseLength); err != nil {
		t.Fatalf("start the project release: %v", err)
	}

	for _, rule := range []struct {
		name string
		rows func(pods.IdleView) IdleReleaseMark
	}{{"the idle release", idleRows}, {"the release of a pod no claim owns", unownedRows}} {
		t.Run(rule.name+" is refused while the project release runs", func(t *testing.T) {
			asked := false
			marked, err := st.ReleaseIdleIssue(ctx, "legion", rootIssue, func(view pods.IdleView) (IdleReleaseMark, error) {
				asked = true
				return rule.rows(view), nil
			})
			if !errors.Is(err, errProjectReleaseRunning) || marked {
				t.Fatalf("ReleaseIdleIssue under a running project release = %t, %v; want it refused with errProjectReleaseRunning", marked, err)
			}
			if asked {
				t.Fatal("the refused release asked its rule to decide")
			}
			if rows, marks := written(); rows != 0 || marks != 0 {
				t.Fatalf("the refused release wrote %d outbox rows and %d marks, want none", rows, marks)
			}
		})
	}

	t.Run("the boot that ends the project release lets the idle release start", func(t *testing.T) {
		if _, err := st.EndProjectReleasesAtBoot(ctx, "legion", releaseBound.Add(time.Hour)); err != nil {
			t.Fatalf("end the project release at the boot: %v", err)
		}
		// The other project's release still runs, and holds none of this project's pods.
		marked, err := st.ReleaseIdleIssue(ctx, "legion", rootIssue, func(view pods.IdleView) (IdleReleaseMark, error) {
			return idleRows(view), nil
		})
		if err != nil || !marked {
			t.Fatalf("ReleaseIdleIssue once the boot ended the project release = %t, %v; want the release marked", marked, err)
		}
		if rows, marks := written(); rows != 3 || marks != 1 {
			t.Fatalf("the release wrote %d outbox rows and %d marks, want its three rows and its mark", rows, marks)
		}
	})
}
