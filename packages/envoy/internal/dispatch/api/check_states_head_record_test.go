// check_states_head_record_test.go holds the settler to the record its former heads come from
// (formerHeadsOf, pull_request_former_heads): each head a pull request leaves while it is open is
// recorded in the transaction of the write that moved it, whether or not a settle ran while the
// pull request was there, and applied by the settler before it reads former heads, so the head
// stays a former head of that pull request until it closes, and a pull request that closes keeps
// none. The rig is check_states_test.go's.
package api

import (
	"context"
	"slices"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// wantTestersPassAt records the tester's pass of k1 at head and fails the test unless the settle
// that follows publishes k1 at head yes, and that alone, naming pullRequests.
func wantTestersPassAt(t *testing.T, rig *checkStatesRig, head string, pullRequests ...int) {
	t.Helper()
	eachHeadResult(t, rig.handler, rig.issue, "k1", "tester", "pass", head)
	events := settleOwed(t, rig)
	wantEvents(t, "the tester's pass of k1 at "+head[:6], events, stateWant{"k1", head, "yes", ""})
	if !slices.Equal(events[0].Payload.PullRequests, pullRequests) {
		t.Fatalf("the state of k1 at %s names the pull requests %v, want %v", head[:6], events[0].Payload.PullRequests, pullRequests)
	}
}

// TestAHeadLeftBeforeASettleIsAFormerHead: #70 opens at H1 and moves to H2 before the settler
// runs. The settle that follows publishes the states at H1 as at H2, and the tester's pass of k1 at
// H1 publishes yes there, naming #70.
func TestAHeadLeftBeforeASettleIsAFormerHead(t *testing.T) {
	rig := newCheckStatesRig(t, checkStatesTwoChecks)
	upsertHead(t, rig, 70, eachHeadH1, rig.issue, "A change")
	upsertHead(t, rig, 70, eachHeadH2, rig.issue, "A change")
	wantEventSet(t, "#70 at H1 and then at H2 before a settle", settleOwed(t, rig),
		stateWant{"k1", eachHeadH1, "no", "no_result"}, stateWant{"k2", eachHeadH1, "no", "no_result"},
		stateWant{"k1", eachHeadH2, "no", "no_result"}, stateWant{"k2", eachHeadH2, "no", "no_result"})
	wantTestersPassAt(t, rig, eachHeadH1, 70)
}

// TestAHeadLeftAfterAPublishedHeadIsAFormerHead: #70 opens at H3, whose states are published, then
// moves to H1 and on to H2 before the next settle. That settle publishes the states at H1 and H2,
// and the tester's pass of k1 at H1 publishes yes there, naming #70.
func TestAHeadLeftAfterAPublishedHeadIsAFormerHead(t *testing.T) {
	rig := newCheckStatesRig(t, checkStatesTwoChecks)
	upsertHead(t, rig, 70, eachHeadH3, rig.issue, "A change")
	wantEvents(t, "#70 opens at H3", settleOwed(t, rig),
		stateWant{"k1", eachHeadH3, "no", "no_result"}, stateWant{"k2", eachHeadH3, "no", "no_result"})
	upsertHead(t, rig, 70, eachHeadH1, rig.issue, "A change")
	upsertHead(t, rig, 70, eachHeadH2, rig.issue, "A change")
	wantEventSet(t, "#70 at H1 and then at H2 before a settle", settleOwed(t, rig),
		stateWant{"k1", eachHeadH1, "no", "no_result"}, stateWant{"k2", eachHeadH1, "no", "no_result"},
		stateWant{"k1", eachHeadH2, "no", "no_result"}, stateWant{"k2", eachHeadH2, "no", "no_result"})
	wantTestersPassAt(t, rig, eachHeadH1, 70)
}

// TestAVisitToAPublishedHeadBetweenSettlesIsKept: #71 opens at H1, where the implementer's passes
// publish yes, naming #71. #70 comes to H1 and moves on to H2, and #71 closes, all before the next
// settle. H1 stays a former head of #70, which is still open: the settle keeps H1's states, which
// did not change, and the tester's fail of k2 at H1 publishes there, naming #70.
func TestAVisitToAPublishedHeadBetweenSettlesIsKept(t *testing.T) {
	rig := newCheckStatesRig(t, checkStatesTwoChecks)
	upsertHead(t, rig, 70, eachHeadH3, rig.issue, "A change")
	settleOwed(t, rig)
	upsertHead(t, rig, 71, eachHeadH1, rig.issue, "A change")
	eachHeadResult(t, rig.handler, rig.issue, "k1", "implementer", "pass", eachHeadH1)
	eachHeadResult(t, rig.handler, rig.issue, "k2", "implementer", "pass", eachHeadH1)
	wantEvents(t, "#71 opens at H1 with the implementer's passes", settleOwed(t, rig),
		stateWant{"k1", eachHeadH1, "yes", ""}, stateWant{"k2", eachHeadH1, "yes", ""})

	upsertHead(t, rig, 70, eachHeadH1, rig.issue, "A change")
	upsertHead(t, rig, 70, eachHeadH2, rig.issue, "A change")
	writeHead(t, rig.database, ciJobRepository, 71, eachHeadH1, "closed", rig.issue)
	wantEventSet(t, "#70 visits H1 and moves to H2, and #71 closes, before a settle", settleOwed(t, rig),
		stateWant{"k1", eachHeadH2, "no", "no_result"}, stateWant{"k2", eachHeadH2, "no", "no_result"})

	eachHeadResult(t, rig.handler, rig.issue, "k2", "tester", "fail", eachHeadH1)
	events := settleOwed(t, rig)
	wantEvents(t, "the tester's fail of k2 at H1 while #70 is open", events, stateWant{"k2", eachHeadH1, "unknown", "pass_and_fail"})
	if !slices.Equal(events[0].Payload.PullRequests, []int{70}) {
		t.Fatalf("the state of k2 at H1 names the pull requests %v, want [70]", events[0].Payload.PullRequests)
	}
}

// TestAClosedPullRequestKeepsNoFormerHead: #70 opens at H1 and moves to H2 before a settle, and
// both heads' states are published. Once #70 closes, the settle deletes the rows of both, with no
// event, and the tester's pass at H1 publishes nothing. #70 reopens at H3: the heads it left before
// it closed are no former heads of it any more, so a result at H2 publishes nothing either.
func TestAClosedPullRequestKeepsNoFormerHead(t *testing.T) {
	rig := newCheckStatesRig(t, checkStatesTwoChecks)
	upsertHead(t, rig, 70, eachHeadH1, rig.issue, "A change")
	upsertHead(t, rig, 70, eachHeadH2, rig.issue, "A change")
	wantEventSet(t, "#70 at H1 and then at H2 before a settle", settleOwed(t, rig),
		stateWant{"k1", eachHeadH1, "no", "no_result"}, stateWant{"k2", eachHeadH1, "no", "no_result"},
		stateWant{"k1", eachHeadH2, "no", "no_result"}, stateWant{"k2", eachHeadH2, "no", "no_result"})

	writeHead(t, rig.database, ciJobRepository, 70, eachHeadH2, "closed", rig.issue)
	if events := settleOwed(t, rig); len(events) != 0 {
		t.Fatalf("#70 closed: the settler published %v, want nothing", eventLines(events))
	}
	wantNoRowAt(t, "#70 closed", rig, eachHeadH1, "", "k1", "k2")
	wantNoRowAt(t, "#70 closed", rig, eachHeadH2, "", "k1", "k2")
	eachHeadResult(t, rig.handler, rig.issue, "k1", "tester", "pass", eachHeadH1)
	if events := settleOwed(t, rig); len(events) != 0 {
		t.Fatalf("the tester's pass at H1 after #70 closed: the settler published %v, want nothing", eventLines(events))
	}

	upsertHead(t, rig, 70, eachHeadH3, rig.issue, "A change")
	wantEvents(t, "#70 reopens at H3", settleOwed(t, rig),
		stateWant{"k1", eachHeadH3, "no", "no_result"}, stateWant{"k2", eachHeadH3, "no", "no_result"})
	eachHeadResult(t, rig.handler, rig.issue, "k2", "tester", "fail", eachHeadH2)
	if events := settleOwed(t, rig); len(events) != 0 {
		t.Fatalf("a fail at H2, which #70 left before it closed, after #70 reopened: the settler published %v, want nothing", eventLines(events))
	}
	wantNoRowAt(t, "#70 reopened", rig, eachHeadH2, "", "k2")
}

// TestThePassAppliesTheRecordsOfAPullRequestNoIssueHolds: #90 opens at H1, credited to no issue,
// and moves to H2. The writes only append their records, and a settle applies those of the open
// pull requests of its own issue, so no settle reaches #90's. One pass of the settler applies them
// itself: H1 is #90's former head, verified, and no record of #90 is left to grow.
func TestThePassAppliesTheRecordsOfAPullRequestNoIssueHolds(t *testing.T) {
	rig := newCheckStatesRig(t, checkStatesTwoChecks)
	writeHead(t, rig.database, ciJobRepository, 90, eachHeadH1, "open", "")
	writeHead(t, rig.database, ciJobRepository, 90, eachHeadH2, "open", "")
	records := func() int {
		t.Helper()
		var count int
		if err := rig.database.Pool.QueryRow(context.Background(),
			`select count(*) from pull_request_former_heads_moves where number = 90`).Scan(&count); err != nil {
			t.Fatalf("count #90's records: %v", err)
		}
		return count
	}
	if count := records(); count == 0 {
		t.Fatal("#90's two writes appended no record, so this test holds nothing to the pass")
	}
	rig.settler.pass(store.WithTransactionTracking(t.Context()))
	if count := records(); count != 0 {
		t.Fatalf("one pass left %d records of #90, which no issue holds: they grow with each write and no settle applies them", count)
	}
	var former []string
	rows, err := rig.database.Pool.Query(context.Background(),
		`select head_sha || case when verified then '' else ' unverified' end from pull_request_former_heads where pull = pull_request_former_heads_pull_of($1, 90)`, ciJobRepository)
	if err != nil {
		t.Fatalf("read #90's former heads: %v", err)
	}
	for rows.Next() {
		var head string
		if err := rows.Scan(&head); err != nil {
			t.Fatalf("read #90's former heads: %v", err)
		}
		former = append(former, head)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read #90's former heads: %v", err)
	}
	if want := []string{eachHeadH1}; !slices.Equal(former, want) {
		t.Fatalf("#90 holds the former heads %v after the pass, want %v: the head it left, verified", former, want)
	}
}
