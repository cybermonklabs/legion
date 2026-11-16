package store

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/pods"
)

// A release that suspends no role is no wake's to end, and the two reads that pair a resume with
// a release say so. The release of a pod no claim owns changes no claim and saves no bundle, so
// nothing of it comes back on a wake: its pod comes back at the next start of a claim of the
// issue, and no resume measures it.
//
// The state both readings seed is one an operator reaches in a few minutes. An issue with a live
// architect and a failed implementer is released by the idle rule, which suspends the architect
// and records that role. An operator retries the failed implementer (`legion claims retry`),
// which rebuilds the pod and writes no resume. The implementer fails again, no claim of the
// issue is live, and the second rule releases the pod and records a release with no role in it.
// The architect is still `released` through all of it, and its wake, whenever it comes, ends the
// idle release and nothing else.
//
// Pairing that wake with the newest row instead would charge the idle release pod hours from its
// own moment to the moment of every later read, for ever, and count it as a release no wake ever
// ended — the distortion the records exist to remove, moved onto the row beside it.
func TestAReleaseThatSuspendsNoRoleEndsNoWake(t *testing.T) {
	t.Run("a wake pairs with the release that suspended its role", func(t *testing.T) {
		st := migratedStore(t)
		ctx := context.Background()
		at := time.Now().UTC().Truncate(time.Millisecond)
		idle, err := st.RecordPodRelease(ctx, PodRelease{Project: "legion", Issue: rootIssue,
			Generation: 1, Roles: []string{"architect"}, At: at.Add(-time.Hour)})
		if err != nil {
			t.Fatalf("record the idle release: %v", err)
		}
		unowned, err := st.RecordPodRelease(ctx, PodRelease{Project: "legion", Issue: rootIssue,
			Generation: 1, At: at.Add(-30 * time.Minute)})
		if err != nil {
			t.Fatalf("record the release of the pod no claim owns: %v", err)
		}

		// Every wake runs through here, the workflow's four inside a fact's transaction and the
		// outbox's delivery wake in one of its own, so one pairing holds for all of them.
		records := podRecords(t, st)
		inTx(t, st, func(tx pgx.Tx) error {
			taken, err := records.TakePodWake(ctx, tx, rootIssue, claim.RoleArchitect, pods.WakeDelivery, at)
			if err != nil || !taken {
				t.Fatalf("TakePodWake = %t, %v; want the architect's wake taken", taken, err)
			}
			return nil
		})

		written, err := st.PodReleaseRecords(ctx, "legion", at.Add(-2*time.Hour))
		if err != nil {
			t.Fatalf("read the release records: %v", err)
		}
		if len(written.Resumes) != 1 || written.Resumes[0].Release != idle {
			t.Fatalf("resumes = %+v, want the one wake paired with the idle release %d rather than the release %d, which suspended no role",
				written.Resumes, idle, unowned)
		}
	})

	t.Run("the stranded read keeps a resume a release of no role came after", func(t *testing.T) {
		st := migratedStore(t)
		ctx := context.Background()
		at := time.Now().UTC().Add(-time.Hour).Truncate(time.Millisecond)
		idle, err := st.RecordPodRelease(ctx, PodRelease{Project: "legion", Issue: rootIssue,
			Generation: 1, Roles: []string{"architect"}, At: at})
		if err != nil {
			t.Fatalf("record the idle release: %v", err)
		}
		if err := st.Tx(ctx, func(tx pgx.Tx) error {
			_, _, err := RecordPodWakeTx(ctx, tx, PodResume{Release: idle, Project: "legion", Issue: rootIssue,
				Role: "architect", Kind: pods.WakeDelivery, At: at.Add(time.Minute)})
			return err
		}); err != nil {
			t.Fatalf("record the wake of the architect: %v", err)
		}

		// The premise of reading the newest release alone is that the claim of an older one has
		// been released again since. A release that changes no claim makes that premise false:
		// the architect is still released, still woken, and still without a first turn, which is
		// exactly the stranded claim the report is for.
		if _, err := st.RecordPodRelease(ctx, PodRelease{Project: "legion", Issue: rootIssue,
			Generation: 1, At: at.Add(30 * time.Minute)}); err != nil {
			t.Fatalf("record the release of the pod no claim owns: %v", err)
		}
		unfinished, err := unfinishedResumes(ctx, st, "legion", time.Now().UTC())
		if err != nil || len(unfinished) != 1 || unfinished[0].Release != idle {
			t.Fatalf("unfinished wakes = %+v, %v; want the architect's wake of release %d, which no later release ended",
				unfinished, err, idle)
		}
	})
}
