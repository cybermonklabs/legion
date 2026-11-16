package store

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/pods"
)

// seenReleaseVersion is the version of the migration this file tests, so the test can migrate
// through everything before it whatever lands beside it.
const seenReleaseVersion = 20261111181500

// A resume records the release it was taken after, and the heal of a stuck resume reads that
// column (ClosePodGoneResume). The rows a deployment is upgraded with were written before the
// column existed, so what the backfill gives them decides, at the deploy, which of them the first
// wake heals: it is the writer's own definition — the issue's newest release at that resume's own
// moment — and not the newest release now, which would call every row fresh and leave a role
// whose pod went away before the deploy refusing every workflow wake it is sent.
//
// The negative control is the column's absence, read before the migration: a write naming it is
// refused, so no row could have carried one.
//
// The six rows are the readings a deployment's table holds. Three are what the backfill writes:
// a resume a release of the issue came after, which is stale and is healed by its first wake;
// one that saw every release there is, which is a pod on its way back and is left alone; and one
// of an issue with no release at all, which keeps the default and waits for that issue's first
// release to make it stale. Two pin the halves of the open-rows where clause, a resume with a
// first turn and one already closed, each of which keeps the default because no wake heals it.
// The sixth pins the floor: a wake whose own clock stands before the release it paired with,
// which the `p.at <= r.at` proxy would stamp below its own release_id.
func TestAResumeTakesTheReleaseItSawWith20261111181500(t *testing.T) {
	ctx := context.Background()
	store := emptyStore(t)
	migrateThrough(t, store, seenReleaseVersion-1)

	at := time.Now().UTC().Truncate(time.Millisecond)
	releaseIn := func(project, issue string, when time.Time) int64 {
		t.Helper()
		var id int64
		if err := store.Pool().QueryRow(ctx, `insert into pod_releases (project, issue, generation, roles, at, bundle_bytes)
			values ($1, $2, 1, 'implementer', $3, 0) returning id`, project, issue, when).Scan(&id); err != nil {
			t.Fatalf("seed the release of %s's %s: %v", project, issue, err)
		}
		return id
	}
	release := func(issue string, when time.Time) int64 {
		t.Helper()
		return releaseIn("legion", issue, when)
	}
	resumeIn := func(project, issue string, role claim.Role, releaseID int64, when time.Time) int64 {
		t.Helper()
		var id int64
		if err := store.Pool().QueryRow(ctx, `insert into pod_resumes (release_id, project, issue, role, kind, at)
			values ($1, $2, $3, $4, $5, $6) returning id`,
			releaseID, project, issue, string(role), pods.WakeRoleMessage, when).Scan(&id); err != nil {
			t.Fatalf("seed the resume of %s's %s %s: %v", project, issue, role, err)
		}
		return id
	}
	resumeOf := func(issue string, role claim.Role, releaseID int64, when time.Time) int64 {
		t.Helper()
		return resumeIn("legion", issue, role, releaseID, when)
	}
	resume := func(issue string, releaseID int64, when time.Time) int64 {
		t.Helper()
		return resumeOf(issue, claim.RoleImplementer, releaseID, when)
	}

	if _, err := store.Pool().Exec(ctx, `update pod_resumes set seen_release = 1`); err == nil ||
		!strings.Contains(err.Error(), "42703") {
		t.Fatalf("writing seen_release before the migration = %v, want SQLSTATE 42703, the undefined column: it is what the migration adds, and any other error would satisfy a weaker control",
			err)
	}

	// LEGION-208's pod went away under its resume: the wake came, and a release of the issue
	// followed it. LEGION-209's resume saw that issue's only release and is the pod coming back.
	// LEGION-210 has no release at all.
	stale := release("LEGION-208", at.Add(-3*time.Hour))
	staleResume := resume("LEGION-208", stale, at.Add(-2*time.Hour))
	// A second project holding the same issue key, which is the state a deployment of two projects
	// is in: every table here is keyed by project as well as issue, and nothing else in the
	// package writes two, so the project scoping is a predicate no row observes. This resume is
	// stale within its own project and its anchor stands below legion's release below, so a heal
	// that stopped asking which project a row is of would close it.
	otherStale := releaseIn("widgets", "LEGION-208", at.Add(-3*time.Hour))
	otherResume := resumeIn("widgets", "LEGION-208", claim.RoleImplementer, otherStale, at.Add(-2*time.Hour))
	gone := release("LEGION-208", at.Add(-time.Hour))
	// widgets' own later release of LEGION-208, which does two jobs. It makes otherResume stale
	// within its own project, which is what the comment above says of it. And it is the newest
	// LEGION-208 release of any project, so a reading that stopped asking which project it reads
	// answers it rather than `gone` at "a release came after the wake" below: it is one of the two
	// observers of newestPodRelease's project clause, beside the LEGION-209 release further down,
	// and either one alone keeps that clause guarded.
	releaseIn("widgets", "LEGION-208", at.Add(-time.Hour))
	live := release("LEGION-209", at.Add(-3*time.Hour))
	liveResume := resume("LEGION-209", live, at.Add(-2*time.Hour))
	noRelease := resume("LEGION-210", 0, at.Add(-2*time.Hour))
	// A resume no wake can heal is left alone by the backfill although a release of its issue came
	// after it, which is what would stamp it otherwise. There are two such rows, one for each half
	// of the where clause: a resume whose role took its turn, and one something closed.
	turned := release("LEGION-211", at.Add(-3*time.Hour))
	finished := resume("LEGION-211", turned, at.Add(-2*time.Hour))
	if _, err := store.Pool().Exec(ctx, `update pod_resumes set first_turn_at = $2 where id = $1`,
		finished, at.Add(-80*time.Minute)); err != nil {
		t.Fatalf("stamp the finished resume's first turn: %v", err)
	}
	release("LEGION-211", at.Add(-time.Hour))
	shut := release("LEGION-212", at.Add(-3*time.Hour))
	closedResume := resume("LEGION-212", shut, at.Add(-2*time.Hour))
	if _, err := store.Pool().Exec(ctx, `update pod_resumes set closed_at = $2 where id = $1`,
		closedResume, at.Add(-80*time.Minute)); err != nil {
		t.Fatalf("close the resume a release ended: %v", err)
	}
	release("LEGION-212", at.Add(-time.Hour))

	// The row a daemon really writes when the wake is a listener's: pod_resumes.at carries the
	// listener's clock at the report and pod_releases.at the daemon's at the teardown, so a wake
	// consumed after a release committed can stand before it in the table. Its own release_id is
	// the committed-order fact, and the floor is what keeps the backfill from stamping it below
	// that and sending the first wake after the deploy at a pod already coming back.
	listener := release("LEGION-213", at.Add(200*time.Millisecond))
	listenerResume := resume("LEGION-213", listener, at)

	// A second role of LEGION-208, whose resume is stale by the same reading as the implementer's:
	// it saw the first release and another has been recorded since. A heal is one role's, so the
	// implementer's leaves this row standing and the architect's own wake is what closes it. The
	// row is what observes that: with the role filter dropped, the implementer's heal would close
	// a resume the architect's pod is coming back for and cost it a duplicate start row.
	architectResume := resumeOf("LEGION-208", claim.RoleArchitect, stale, at.Add(-90*time.Minute))

	// The other project's release of the same key as legion's LEGION-209, recorded last so it is
	// the newest row of that key anywhere: a reading that stopped asking which project it reads
	// would make legion's own LEGION-209 resume, the pod coming back, look stale.
	releaseIn("widgets", "LEGION-209", at)

	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate through the resume's own anchor: %v", err)
	}

	seen := func(id int64) int64 {
		t.Helper()
		var value int64
		if err := store.Pool().QueryRow(ctx, `select seen_release from pod_resumes where id = $1`, id).Scan(&value); err != nil {
			t.Fatalf("read the anchor of resume %d: %v", id, err)
		}
		return value
	}
	if got := seen(staleResume); got != stale {
		t.Fatalf("the anchor of the resume its pod went away under = %d, want %d, the release it saw: the backfill gives the newest release at the resume's own moment, never the newest release now (%d)",
			got, stale, gone)
	}
	if got := seen(liveResume); got != live {
		t.Fatalf("the anchor of the resume whose pod is coming back = %d, want %d, the release it saw", got, live)
	}
	if got := seen(noRelease); got != 0 {
		t.Fatalf("the anchor of the resume of an issue with no release = %d, want 0, the default", got)
	}
	if got := seen(finished); got != 0 {
		t.Fatalf("the anchor of a resume with a first turn = %d, want 0: the backfill writes the open rows alone", got)
	}
	if got := seen(closedResume); got != 0 {
		t.Fatalf("the anchor of a resume something closed = %d, want 0: the backfill writes the open rows alone", got)
	}
	if got := seen(listenerResume); got != listener {
		t.Fatalf("the anchor of the resume whose wake stands before its own release = %d, want %d, its release_id: the two columns are different clocks, and the row's own release is the committed-order fact",
			got, listener)
	}

	// The invariant over every backfilled row, not the seeded cases alone: no row is stamped below
	// the release it paired with, which is the release the writer read in the take's own
	// transaction, so the heal reads nothing above a resume whose pod is coming back.
	var below int64
	if err := store.Pool().QueryRow(ctx, `select count(*) from pod_resumes
		where first_turn_at is null and closed_at is null and seen_release < release_id`).Scan(&below); err != nil {
		t.Fatalf("read the backfilled rows: %v", err)
	}
	if below != 0 {
		t.Fatalf("%d backfilled rows are stamped below their own release: every one must satisfy seen_release >= release_id, or the first wake after the deploy closes a live resume", below)
	}

	// What those anchors decide, driven through the heal itself rather than read off the column.
	// The stale row is closed at its first wake, and every other shape survives it: the pod coming
	// back, the issue with no release, the row the floor exists for, whose wake stands before the
	// release it paired with, and the two rows no wake can heal, one whose role took its turn and
	// one something has closed. Each is a predicate of the heal's own `where` with nothing else to
	// observe it — ClosePodGoneResume has two callers, the workflow's wake and this test — and a
	// value read off the column holds none of them: a heal whose predicate changed closes a live
	// row with the stored number still right.
	records := podRecords(t, store)
	for _, tc := range []struct {
		name   string
		issue  string
		id     int64
		closed bool
		gone   int64
	}{
		{"a release came after the wake", "LEGION-208", staleResume, true, gone},
		{"the pod is coming back", "LEGION-209", liveResume, false, 0},
		{"the issue has no release", "LEGION-210", noRelease, false, 0},
		{"the wake stands before its own release", "LEGION-213", listenerResume, false, 0},
		{"the role took its turn", "LEGION-211", finished, false, 0},
		{"the resume is already closed", "LEGION-212", closedResume, true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inTx(t, store, func(tx pgx.Tx) error {
				answered, err := records.ClosePodGoneResume(ctx, tx, tc.issue, claim.RoleImplementer)
				if err != nil {
					t.Fatalf("the heal of %s: %v", tc.issue, err)
				}
				if answered != tc.gone {
					t.Fatalf("the heal of %s answered release %d, want %d: it names the issue's newest release, which stands above the row it closed, and 0 where it closed none",
						tc.issue, answered, tc.gone)
				}
				return nil
			})
			var closed *time.Time
			if err := store.Pool().QueryRow(ctx, `select closed_at from pod_resumes where id = $1`, tc.id).Scan(&closed); err != nil {
				t.Fatalf("read the resume of %s: %v", tc.issue, err)
			}
			if (closed != nil) != tc.closed {
				t.Fatalf("the resume of %s closed = %t, want %t", tc.issue, closed != nil, tc.closed)
			}
		})
	}

	// The implementer's heal of LEGION-208 above closed the implementer's row and left the
	// architect's, which is as stale by the anchor and is the architect's own wake to close.
	var architectClosed *time.Time
	if err := store.Pool().QueryRow(ctx, `select closed_at from pod_resumes where id = $1`, architectResume).Scan(&architectClosed); err != nil {
		t.Fatalf("read the architect's resume of LEGION-208: %v", err)
	}
	if architectClosed != nil {
		t.Fatalf("the architect's resume of LEGION-208 was closed by the implementer's heal: a heal is one role's, and closing another role's resume costs the pod coming back for it a second start row")
	}

	// The same of the second project: a heal is one project's, and the row of another project's
	// issue of the same key is no row of this one's.
	var otherClosed *time.Time
	if err := store.Pool().QueryRow(ctx, `select closed_at from pod_resumes where id = $1`, otherResume).Scan(&otherClosed); err != nil {
		t.Fatalf("read the other project's resume of LEGION-208: %v", err)
	}
	if otherClosed != nil {
		t.Fatalf("the other project's resume of LEGION-208 was closed by legion's heal: a heal is one project's, and two projects holding one issue key are two issues")
	}
}
