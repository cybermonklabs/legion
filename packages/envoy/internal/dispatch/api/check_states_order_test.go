// check_states_order_test.go holds the settler to one outcome whatever order the records of a
// pull request's history commit and are applied in: the same writes, applied in the order of their
// places or with a close of an earlier place applied late, end with the same candidates, the same
// history and the same published states. The rig is check_states_test.go's.
package api

import (
	"maps"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// TestALateCloseAndAResultAfterItEndAsInTheOrderOfTheirPlaces: #70 is stored under its repository's
// old name, open at H2 and credited to no issue, and under its canonical name, credited to the
// issue, where it visits H5 among other heads; the tester records k2 passing at H5. The old name's
// row is then closed, a close of #70 under any name, and after the close's place the tester records
// k1 passing at H1, a head #70 reached before the close. A result recorded after the close makes
// its commit a candidate of the lifecycle that follows, and a result before it does not, so H1 is a
// candidate once more and, GitHub listing it, a former head, and k1 is published yes there, while
// H5 is no former head and nothing is published there. Two schedules give H1 that result:
//   - H1 is a head #70 left before the close, a former head the close ends;
//   - #70 stands at H1 when the result is recorded, so the result's commit is its open head and no
//     candidate, and moves on to H2 after it: the head it leaves was reached before the close, so
//     its visit keeps nothing, and the result after the close keeps it.
//
// In the order of their places the close is applied first, and then the result and the move. Late,
// the close's transaction stays open while a settle drains the result and applies the move, H1
// still a former head of the earlier lifecycle, and the close commits and is applied after: it
// ends the history before its place, H1 and H5 with it, and must leave again what a result after it
// supplied, as the order of the places would have. Both end with the same candidates and the same
// published states.
func TestALateCloseAndAResultAfterItEndAsInTheOrderOfTheirPlaces(t *testing.T) {
	const oldRepo = "oldorg/example-repo"
	schedules := []struct {
		name string
		// visits are the heads #70 is written at under its canonical name before the close, the
		// last the head it stands at; then are the heads it moves to after the result.
		visits, then []string
		// before are the former heads #70 holds before the close.
		before map[string]bool
	}{
		{
			name:   "at a head left before the close",
			visits: []string{eachHeadH1, eachHeadH5, eachHeadH2},
			before: map[string]bool{eachHeadH1: true, eachHeadH5: true},
		},
		{
			name:   "at the head it stands at, left after the result",
			visits: []string{eachHeadH5, eachHeadH1},
			then:   []string{eachHeadH2},
			before: map[string]bool{eachHeadH5: true},
		},
	}
	for _, schedule := range schedules {
		for _, late := range []bool{false, true} {
			name := schedule.name + ", the close applied before the result"
			if late {
				name = schedule.name + ", the close committed after a settle drained the result"
			}
			t.Run(name, func(t *testing.T) {
				rig := newCheckStatesRig(t, checkStatesTwoChecks)
				learnRename(t, rig, oldRepo, ciJobRepository)
				writeHead(t, rig.database, oldRepo, 70, eachHeadH2, "open", "")
				for _, head := range schedule.visits {
					upsertHead(t, rig, 70, head, rig.issue, "A change")
				}
				eachHeadResult(t, rig.handler, rig.issue, "k2", "tester", "pass", eachHeadH5)
				settleOwed(t, rig)
				if got := candidatesOf(t, rig); !maps.Equal(got, schedule.before) {
					t.Fatalf("#70 holds the former heads %v before the close, want %v: this test holds no history to end", got, schedule.before)
				}
				rig.github.setPullCommits(70, eachHeadH1, eachHeadH5, eachHeadH2)
				applyLeftRecords := func() {
					t.Helper()
					rig.settler.pass(store.WithTransactionTracking(t.Context()))
					settleOwed(t, rig)
				}

				closing, err := rig.database.Pool.Begin(t.Context())
				if err != nil {
					t.Fatalf("begin the close: %v", err)
				}
				defer closing.Rollback(t.Context())
				if _, err := closing.Exec(t.Context(), `update pull_request_heads set state = 'closed' where repo = $1 and number = 70`, oldRepo); err != nil {
					t.Fatalf("close %s#70: %v", oldRepo, err)
				}
				if !late {
					if err := closing.Commit(t.Context()); err != nil {
						t.Fatalf("commit the close: %v", err)
					}
					applyLeftRecords()
				}
				eachHeadResult(t, rig.handler, rig.issue, "k1", "tester", "pass", eachHeadH1)
				settleOwed(t, rig)
				for _, head := range schedule.then {
					upsertHead(t, rig, 70, head, rig.issue, "A change")
					settleOwed(t, rig)
				}
				if late {
					var pending, moves int
					if err := rig.database.Pool.QueryRow(t.Context(), `
						select (select count(*) from pull_request_former_heads_pending where issue_key = $1),
							(select count(*) from pull_request_former_heads_moves where number = 70)`, rig.issue).Scan(&pending, &moves); err != nil {
						t.Fatalf("count the pending rows of %s and #70's records: %v", rig.issue, err)
					}
					if got := candidatesOf(t, rig); pending != 0 || moves != 0 || !got[eachHeadH1] {
						t.Fatalf("before the close commits #70 holds %v, %d rows are pending and %d records unapplied, want H1 a former head and every record applied: this test holds no late close", got, pending, moves)
					}
					if err := closing.Commit(t.Context()); err != nil {
						t.Fatalf("commit the close: %v", err)
					}
					applyLeftRecords()
				}
				settleEveryMark(t, rig)
				if got := candidatesOf(t, rig); !maps.Equal(got, map[string]bool{eachHeadH1: true}) {
					t.Fatalf("#70 holds the former heads %v once every record is applied, want H1 alone: the result after the close makes H1 a candidate, the one before it leaves H5 out", got)
				}
				if got := stateAt(t, rig, rig.issue, eachHeadH1, "k1"); got != model.PassStandsYes {
					t.Fatalf("k1 at H1 is published %q, want yes: a result after the close counts in the lifecycle that follows, whatever order the close is applied in", got)
				}
				if _, held := publishedRow(t, rig.database, rig.issue, ciJobRepository, eachHeadH5, ""); held {
					t.Fatalf("H5 holds published states once the close is applied, want none: its result is before the close")
				}
			})
		}
	}
}

// TestAJoinOfTwoNamesAppliedApartEndsAsInTheOrderOfTheirPlaces: the first schedule of
// TestALateCloseAndAResultAfterItEndAsInTheOrderOfTheirPlaces, with Dispatch learning the rename
// only after each name's records were applied apart: the close under the old name ends that name's
// pull request alone, and the tester's result at H1 after the close finds H1 in the canonical
// name's history, a head left before the close, which keeps it as it stands. Learning the rename
// joins the two into one pull request, whose lifecycle the close ended: the join drops the history
// before the close, H1 and H5 with it, and must leave again what the result after the close
// supplied, as one pull request from the start does. The issue is then owed, as a change of its
// checks' words would owe it, and its settle publishes k1 yes at H1 and nothing at H5.
func TestAJoinOfTwoNamesAppliedApartEndsAsInTheOrderOfTheirPlaces(t *testing.T) {
	const oldRepo = "oldorg/example-repo"
	rig := newCheckStatesRig(t, checkStatesTwoChecks)
	writeHead(t, rig.database, oldRepo, 70, eachHeadH2, "open", "")
	for _, head := range []string{eachHeadH1, eachHeadH5, eachHeadH2} {
		upsertHead(t, rig, 70, head, rig.issue, "A change")
	}
	eachHeadResult(t, rig.handler, rig.issue, "k2", "tester", "pass", eachHeadH5)
	settleOwed(t, rig)
	rig.github.setPullCommits(70, eachHeadH1, eachHeadH5, eachHeadH2)
	if _, err := rig.database.Pool.Exec(t.Context(), `update pull_request_heads set state = 'closed' where repo = $1 and number = 70`, oldRepo); err != nil {
		t.Fatalf("close %s#70: %v", oldRepo, err)
	}
	rig.settler.pass(store.WithTransactionTracking(t.Context()))
	settleOwed(t, rig)
	eachHeadResult(t, rig.handler, rig.issue, "k1", "tester", "pass", eachHeadH1)
	settleOwed(t, rig)
	if got := candidatesOf(t, rig); !maps.Equal(got, map[string]bool{eachHeadH1: true, eachHeadH5: true}) {
		t.Fatalf("before the rename is known #70 holds %v under its canonical name, want H1 and H5: this test holds no history to end", got)
	}
	var identities, endedOld, endedCanonical, moves, pending int
	if err := rig.database.Pool.QueryRow(t.Context(), `
		select count(distinct alias.pull),
			count(distinct alias.pull) filter (where alias.repo = lower($1) and pulls.ended is not null),
			count(distinct alias.pull) filter (where alias.repo = lower($2) and pulls.ended is not null),
			(select count(*) from pull_request_former_heads_moves where number = 70),
			(select count(*) from pull_request_former_heads_pending where issue_key = $3)
		from pull_request_former_heads_aliases alias join pull_request_former_heads_pulls pulls on pulls.pull = alias.pull
		where alias.number = 70`, oldRepo, ciJobRepository, rig.issue).Scan(&identities, &endedOld, &endedCanonical, &moves, &pending); err != nil {
		t.Fatalf("read #70's identities: %v", err)
	}
	if identities != 2 || endedOld != 1 || endedCanonical != 0 || moves != 0 || pending != 0 {
		t.Fatalf("before the rename is known #70 has %d identities, %d ended under the old name and %d under the canonical one, %d records unapplied and %d rows pending; want two, the old one ended, everything applied: this test holds no join", identities, endedOld, endedCanonical, moves, pending)
	}

	learnRename(t, rig, oldRepo, ciJobRepository)
	if _, err := rig.database.Pool.Exec(t.Context(), `select check_states_owe($1)`, rig.issue); err != nil {
		t.Fatalf("owe %s: %v", rig.issue, err)
	}
	settleEveryMark(t, rig)
	if got := candidatesOf(t, rig); !maps.Equal(got, map[string]bool{eachHeadH1: true}) {
		t.Fatalf("#70 holds the former heads %v once its names are one pull request, want H1 alone: the result after the close makes H1 a candidate, the one before it leaves H5 out", got)
	}
	if got := stateAt(t, rig, rig.issue, eachHeadH1, "k1"); got != model.PassStandsYes {
		t.Fatalf("k1 at H1 is published %q, want yes: a result after the close counts in the lifecycle that follows, whenever the rename is learned", got)
	}
	if _, held := publishedRow(t, rig.database, rig.issue, ciJobRepository, eachHeadH5, ""); held {
		t.Fatalf("H5 holds published states once the names are joined, want none: its result is before the close")
	}
}
