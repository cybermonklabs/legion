package signals

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/prodevents"
)

// TestAFailedAfterMergeCheckMakesOneCheckFailRow: a fail of an after_merge check, at a commit a
// deploy row holds, is one counted check_fail row of the result issue's project, blaming that
// deploy, naming the issue's pull requests, keyed check:<document>/<block>@<deploy>, with the
// result's actor and time; written by the record route's handler (RecordCheckFailure) and by the
// checks feed from the result.recorded event, a repeat of either writes no second row.
func TestAFailedAfterMergeCheckMakesOneCheckFailRow(t *testing.T) {
	r := newRig(t)
	issue := r.issue(1)
	r.pullRequest(8, at(30), sha("8"), issue)
	r.deployRun(200, at(60), sha("a"), prodJob, success, success)
	r.passDeploys()
	deploy := r.deployOf(200)

	result := r.checkResult(issue, "c1", sha("8"), verdictFail, model.CheckWhenAfterMerge)
	created, err := RecordCheckFailure(r.ctx, r.pool, result)
	if err != nil || !created {
		t.Fatalf("RecordCheckFailure = %v, %v; want a row", created, err)
	}
	again, err := RecordCheckFailure(r.ctx, r.pool, result)
	if err != nil || again {
		t.Fatalf("a repeat of RecordCheckFailure = %v, %v; want no second row", again, err)
	}
	r.passEvents()

	rows := r.failures()
	if len(rows) != 1 {
		t.Fatalf("failure rows = %+v, want one", rows)
	}
	row := rows[0]
	if row.ProjectKey != alpha || row.Source != prodevents.SourceCheckResults || *row.Signal != prodevents.SignalCheckFail ||
		row.SourceRef != "check:"+r.verification(issue)+"/c1@"+deploy.ID || *row.DeployID != deploy.ID || row.State != prodevents.StateCounted ||
		row.Actor != tester || !slices.Equal(row.PullRequests, []string{"acme/widgets#8"}) {
		t.Fatalf("check_fail row = %+v", row)
	}
	var recorded string
	if err := r.pool.QueryRow(r.ctx, `select created_at::text from check_results where id = $1`, result).Scan(&recorded); err != nil {
		t.Fatalf("read the result's time: %v", err)
	}
	var detected string
	if err := r.pool.QueryRow(r.ctx, `select detected_at::text from production_events where id = $1`, row.ID).Scan(&detected); err != nil || detected != recorded {
		t.Errorf("detected_at = %s (%v), want the result's own time %s", detected, err, recorded)
	}
}

// TestAPassABeforeMergeFailAndAnUnmatchedCommitMakeNoCheckFailRow: a passing result, a fail of a
// check run before the merge, and a fail of an after_merge check at a commit no deploy row holds
// make no row, through the handler's write and through the checks feed alike.
func TestAPassABeforeMergeFailAndAnUnmatchedCommitMakeNoCheckFailRow(t *testing.T) {
	r := newRig(t)
	issue := r.issue(2)
	r.deployRun(210, at(60), sha("b"), prodJob, success, success)
	r.passDeploys()

	for _, result := range []string{
		r.checkResult(issue, "c1", sha("b"), "pass", model.CheckWhenAfterMerge),
		r.checkResult(issue, "c2", sha("b"), verdictFail, "before_merge"),
		r.checkResult(issue, "c3", sha("e"), verdictFail, model.CheckWhenAfterMerge),
	} {
		if created, err := RecordCheckFailure(r.ctx, r.pool, result); err != nil || created {
			t.Errorf("RecordCheckFailure(%s) = %v, %v; want no row", result, created, err)
		}
	}
	r.passEvents()
	if rows := r.failures(); len(rows) != 0 {
		t.Fatalf("failure rows = %+v, want none", rows)
	}
}

// TestARetractionRejectsTheCheckFailRow: a retraction of the result that made a check_fail row
// rejects that row, with the retraction's reason and actor, from the result.retracted event the
// checks feed reads; the row stays, out of the numbers.
func TestARetractionRejectsTheCheckFailRow(t *testing.T) {
	r := newRig(t)
	issue := r.issue(3)
	r.deployRun(220, at(60), sha("c"), prodJob, success, success)
	r.passDeploys()
	result := r.checkResult(issue, "c1", sha("c"), verdictFail, model.CheckWhenAfterMerge)
	r.passEvents()
	if rows := r.failures(); len(rows) != 1 || rows[0].State != prodevents.StateCounted {
		t.Fatalf("before the retraction, failure rows = %+v, want one counted", rows)
	}

	const reason = "The check ran against the staging cluster, not production."
	r.exec("retract the result", `insert into check_result_retractions (result_id, reason, actor) values ($1, $2, $3)`,
		result, reason, `{"kind":"user","id":"alice"}`)
	r.event(issue, "result.retracted", person, model.CheckResultRetractedEventPayload{
		ResultID: result, Issue: issue, CheckIssue: issue, CheckBlockID: "c1", CommitSHA: sha("c"), Reason: reason,
	})
	r.passEvents()

	rows := r.failures()
	if len(rows) != 1 {
		t.Fatalf("after the retraction, failure rows = %+v, want the one row kept", rows)
	}
	if row := rows[0]; row.State != prodevents.StateRejected || row.StateReason == nil || *row.StateReason != reason ||
		row.StateBy == nil || *row.StateBy != person || row.StateAt == nil {
		t.Fatalf("the retracted result's row = state %q, reason %v, by %v; want rejected with the retraction's reason and actor",
			row.State, row.StateReason, row.StateBy)
	}
}

// TestAnEarlyCheckFailIsMatchedWhenItsDeployRowLands: a fail of an after_merge check whose commit no
// deploy row holds yet makes no row; the deploy row that holds the commit, once it lands, makes it,
// blaming that deploy.
func TestAnEarlyCheckFailIsMatchedWhenItsDeployRowLands(t *testing.T) {
	r := newRig(t)
	issue := r.issue(4)
	r.checkResult(issue, "c1", sha("d"), verdictFail, model.CheckWhenAfterMerge)
	r.passEvents()
	if rows := r.failures(); len(rows) != 0 {
		t.Fatalf("before any deploy holds the commit, failure rows = %+v, want none", rows)
	}

	r.deployRun(230, at(60), sha("d"), prodJob, success, success)
	r.passDeploys()
	deploy := r.deployOf(230)
	rows := r.failures()
	if len(rows) != 1 || *rows[0].DeployID != deploy.ID || rows[0].SourceRef != "check:"+r.verification(issue)+"/c1@"+deploy.ID {
		t.Fatalf("once the deploy row landed, failure rows = %+v, want the check_fail blaming %s", rows, deploy.ID)
	}
}

// failNextInsert makes the next row of table that when holds of refused, once: the interruption of
// a write between its first statement and its commit. A sequence counts the refusal, and no
// rollback takes it back, so the insert after it goes through.
func (r *rig) failNextInsert(table, when string) {
	r.t.Helper()
	r.exec("make the refusal's counter", `create sequence signals_test_refusals_`+table)
	r.exec("make the refusal", `create function signals_test_refuse_`+table+`() returns trigger language plpgsql as $$
		begin
			if nextval('signals_test_refusals_`+table+`') = 1 then
				raise exception 'the test refuses this % row once', tg_table_name;
			end if;
			return new;
		end $$`)
	r.exec("refuse the next row", `create trigger signals_test_refuse_`+table+` before insert on `+table+`
		for each row when (`+when+`) execute function signals_test_refuse_`+table+`()`)
}

// boundTo is the id of the check_fail row result is bound to, "" where none.
func (r *rig) boundTo(result string) string {
	r.t.Helper()
	var failure string
	if err := r.pool.QueryRow(r.ctx, `select coalesce((select failure_id::text from check_fail_bindings where result_id = $1), '')`,
		result).Scan(&failure); err != nil {
		r.t.Fatalf("read the binding of result %s: %v", result, err)
	}
	return failure
}

// retract retracts result, a fail of check block of issue at commit, for reason, as the retraction
// route writes it: its row, then its result.retracted event, by person.
func (r *rig) retract(issue, result, block, commit, reason string) {
	r.t.Helper()
	r.exec("retract result "+result, `insert into check_result_retractions (result_id, reason, actor) values ($1, $2, $3)`,
		result, reason, `{"kind":"user","id":"alice"}`)
	r.event(issue, "result.retracted", person, model.CheckResultRetractedEventPayload{
		ResultID: result, Issue: issue, CheckIssue: issue, CheckBlockID: block, CommitSHA: commit, Reason: reason,
	})
}

// awaitLockWait waits until a backend of the rig's database waits on a lock while it runs a
// statement whose text is like like, the write what names running on it, and fails the test where
// done answers first, the write having ended without waiting, or where 30 s pass.
func (r *rig) awaitLockWait(what, like string, done <-chan error) {
	r.t.Helper()
	for deadline := time.Now().Add(30 * time.Second); ; {
		var waiting bool
		if err := r.pool.QueryRow(r.ctx, `select exists (select from pg_stat_activity
			where datname = current_database() and wait_event_type = 'Lock' and query like $1)`, like).Scan(&waiting); err != nil {
			r.t.Fatalf("%s: read the lock waits: %v", what, err)
		}
		if waiting {
			return
		}
		select {
		case err := <-done:
			r.t.Fatalf("%s ended (%v) without waiting on a lock", what, err)
		default:
		}
		if time.Now().After(deadline) {
			r.t.Fatalf("%s did not wait on a lock within 30 s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// failingCheckFails is the error a write of a check_fail row gives while failNextInsert refuses it.
const failingCheckFails = "the test refuses this production_events row once"

// TestAnEarlyCheckFailSurvivesAPassThatFailsAfterItsDeployRowCommits: a fail of an after_merge
// check whose commit no deploy row held when the checks feed read it makes its row from the deploys
// feed alone. A pass whose write of that row fails after the deploy row committed leaves the derive
// that wrote the deploy row unsettled: a page of the change log leaves the cursor where it stood,
// and a whole derive of the repository leaves its rule unrecorded, and the result bound to no row.
// The next pass, which starts from what the database holds, derives the same deploy row again,
// which writes nothing, and matches it all the same: one check_fail row blames the deploy, bound to
// the result, and further passes of every feed write no second.
func TestAnEarlyCheckFailSurvivesAPassThatFailsAfterItsDeployRowCommits(t *testing.T) {
	// whole is whether the deploy row lands by a whole derive of the repository (the first pass,
	// which finds no rule recorded) rather than by a page of the change log after it.
	for _, tc := range []struct {
		name  string
		whole bool
	}{
		{name: "a page of the change log", whole: false},
		{name: "a whole derive", whole: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t)
			if !tc.whole {
				r.passDeploys()
			}
			issue := r.issue(7)
			result := r.checkResult(issue, "c1", sha("d"), verdictFail, model.CheckWhenAfterMerge)
			r.passEvents()
			r.deployRun(250, at(60), sha("d"), prodJob, success, success)
			before, err := readCursor(r.ctx, r.pool, deploysCursor)
			if err != nil {
				t.Fatal(err)
			}
			recorded := func() bool {
				t.Helper()
				records, err := readDeployRules(r.ctx, r.pool)
				if err != nil {
					t.Fatal(err)
				}
				_, held := records[widgets]
				return held
			}

			r.failNextInsert("production_events", "new.kind = 'failure'")
			if err := passDeploys(r.ctx, r.pool); err == nil || !strings.Contains(err.Error(), failingCheckFails) {
				t.Fatalf("the pass whose failure row the record refused = %v, want that refusal", err)
			}
			deploy := r.deployOf(250)
			if rows, bound := r.failures(), r.boundTo(result); len(rows) != 0 || bound != "" {
				t.Fatalf("with the failure row refused, failure rows = %+v and the result bound to %q, want neither", rows, bound)
			}
			stood, err := readCursor(r.ctx, r.pool, deploysCursor)
			if err != nil {
				t.Fatal(err)
			}
			if stood.held != before.held || stood.position != before.position {
				t.Errorf("the failed pass moved the deploys cursor from %d (held %v) to %d (held %v), want it where it stood",
					before.position, before.held, stood.position, stood.held)
			}
			if held := recorded(); held == tc.whole {
				t.Errorf("after the failed pass, the rule of %s is recorded: %v, want %v", widgets, held, !tc.whole)
			}

			r.passDeploys()
			rows := r.failures()
			ref := "check:" + r.verification(issue) + "/c1@" + deploy.ID
			if len(rows) != 1 || *rows[0].DeployID != deploy.ID || rows[0].SourceRef != ref || rows[0].State != prodevents.StateCounted ||
				r.boundTo(result) != rows[0].ID {
				t.Fatalf("after the next pass, failure rows = %+v and the result bound to %q, want one counted check_fail %s blaming %s, bound to it",
					rows, r.boundTo(result), ref, deploy.ID)
			}
			settled, err := readCursor(r.ctx, r.pool, deploysCursor)
			if err != nil {
				t.Fatal(err)
			}
			if !settled.held || settled.position <= before.position || !recorded() {
				t.Errorf("after the next pass, the deploys cursor is at %d (held %v) and the rule recorded: %v; want the cursor past %d and the rule recorded",
					settled.position, settled.held, recorded(), before.position)
			}

			r.passDeploys()
			r.passEvents()
			if again := r.failures(); len(again) != 1 || again[0].ID != rows[0].ID {
				t.Fatalf("after further passes, failure rows = %+v, want the one row %s", again, rows[0].ID)
			}
		})
	}
}

// TestACheckFailRowAndItsBindingCommitTogether: a result's check_fail row and its binding to it
// commit in one transaction: a write whose binding is refused leaves no row and no binding, and the
// next write makes both.
func TestACheckFailRowAndItsBindingCommitTogether(t *testing.T) {
	r := newRig(t)
	issue := r.issue(11)
	r.deployRun(290, at(60), sha("a"), prodJob, success, success)
	r.passDeploys()
	result := r.checkResult(issue, "c1", sha("a"), verdictFail, model.CheckWhenAfterMerge)

	r.failNextInsert("check_fail_bindings", "true")
	if created, err := RecordCheckFailure(r.ctx, r.pool, result); err == nil || created ||
		!strings.Contains(err.Error(), "the test refuses this check_fail_bindings row once") {
		t.Fatalf("RecordCheckFailure with the binding refused = %v, %v; want the refusal", created, err)
	}
	if rows, bound := r.failures(), r.boundTo(result); len(rows) != 0 || bound != "" {
		t.Fatalf("with the binding refused, failure rows = %+v and the result bound to %q, want neither", rows, bound)
	}
	if created, err := RecordCheckFailure(r.ctx, r.pool, result); err != nil || !created {
		t.Fatalf("the next RecordCheckFailure = %v, %v; want a row", created, err)
	}
	if rows, bound := r.failures(), r.boundTo(result); len(rows) != 1 || bound != rows[0].ID {
		t.Fatalf("after the next write, failure rows = %+v and the result bound to %q, want one row, bound", rows, bound)
	}
}

// TestACheckFailStaysOnTheRowItFirstMatchedWhenAnEarlierDeployLands: a fail of an after_merge check
// at a commit a deploy row holds makes its check_fail row blaming that deploy, and is bound to it.
// A deploy row of another run of the same head, which started first and finished first but whose
// production job's write came last, lands on a later page of the change log and is now the deploy
// that first shipped the commit. It makes the result no second row: neither the deploys feed's
// match, nor a repeat of the handler's write, nor the checks feed. The result's retraction then
// rejects the row it is bound to, with the retraction's reason and actor.
func TestACheckFailStaysOnTheRowItFirstMatchedWhenAnEarlierDeployLands(t *testing.T) {
	r := newRig(t)
	issue := r.issue(8)
	r.deployRun(261, at(60), sha("d"), prodJob, nil, nil)
	r.deployRun(262, at(120), sha("d"), prodJob, success, success)
	r.passDeploys()
	later := r.deployOf(262)
	result := r.checkResult(issue, "c1", sha("d"), verdictFail, model.CheckWhenAfterMerge)
	r.passEvents()
	first := r.failures()
	if ref := "check:" + r.verification(issue) + "/c1@" + later.ID; len(first) != 1 || first[0].SourceRef != ref || r.boundTo(result) != first[0].ID {
		t.Fatalf("failure rows = %+v, want one check_fail %s, bound to the result", first, ref)
	}

	r.deployRun(261, at(60), sha("d"), prodJob, success, success)
	r.passDeploys()
	earlier := r.deployOf(261)
	if blamed, held, err := prodevents.DeployForCommit(r.ctx, r.pool, sha("d")); err != nil || !held || blamed.ID != earlier.ID {
		t.Fatalf("the deploy that first shipped the commit = %q (%v, %v), want run 261's %s", blamed.ID, held, err, earlier.ID)
	}
	if created, err := RecordCheckFailure(r.ctx, r.pool, result); err != nil || created {
		t.Fatalf("a repeat of RecordCheckFailure once the earlier deploy landed = %v, %v; want no row", created, err)
	}
	r.passEvents()
	if rows := r.failures(); len(rows) != 1 || rows[0].ID != first[0].ID || rows[0].State != prodevents.StateCounted || r.boundTo(result) != first[0].ID {
		t.Fatalf("once the earlier deploy landed, failure rows = %+v, want the one counted row %s blaming run 262's deploy", rows, first[0].ID)
	}

	const reason = "The check read the staging cluster."
	r.retract(issue, result, "c1", sha("d"), reason)
	r.passEvents()
	rows := r.failures()
	if len(rows) != 1 || rows[0].ID != first[0].ID {
		t.Fatalf("after the retraction, failure rows = %+v, want the one row %s", rows, first[0].ID)
	}
	if row := rows[0]; row.State != prodevents.StateRejected || row.StateReason == nil || *row.StateReason != reason ||
		row.StateBy == nil || *row.StateBy != person {
		t.Fatalf("the retracted result's row = state %q, reason %v, by %v; want rejected with the retraction's reason and actor",
			row.State, row.StateReason, row.StateBy)
	}
}

// TestTwoResultsOfOneCheckStandBehindOneCheckFailRow: two fails of one after_merge check, at the
// two commits one deploy first shipped (its head and the merge commit of the pull request it
// ships), stand behind one check_fail row, each bound to it. A retraction of one keeps the row
// counted, since the other still stands; a retraction of the other then rejects it, with that
// retraction's reason and actor.
func TestTwoResultsOfOneCheckStandBehindOneCheckFailRow(t *testing.T) {
	r := newRig(t)
	issue := r.issue(9)
	r.pullRequest(12, at(30), sha("2"), issue)
	r.deployRun(270, at(60), sha("e"), prodJob, success, success)
	r.passDeploys()
	deploy := r.deployOf(270)
	atHead := r.checkResult(issue, "c1", sha("e"), verdictFail, model.CheckWhenAfterMerge)
	atMerge := r.checkResult(issue, "c1", sha("2"), verdictFail, model.CheckWhenAfterMerge)
	r.passEvents()
	rows := r.failures()
	if ref := "check:" + r.verification(issue) + "/c1@" + deploy.ID; len(rows) != 1 || rows[0].SourceRef != ref {
		t.Fatalf("failure rows = %+v, want one check_fail %s", rows, ref)
	}
	if head, merge := r.boundTo(atHead), r.boundTo(atMerge); head != rows[0].ID || merge != rows[0].ID {
		t.Fatalf("the results are bound to %q and %q, want both to %s", head, merge, rows[0].ID)
	}

	r.retract(issue, atHead, "c1", sha("e"), "The first run read a stale cache.")
	r.passEvents()
	if got := r.failures(); len(got) != 1 || got[0].State != prodevents.StateCounted {
		t.Fatalf("with one of its two results retracted, failure rows = %+v, want the one row counted", got)
	}

	const reason = "The second run read the same stale cache."
	r.retract(issue, atMerge, "c1", sha("2"), reason)
	r.passEvents()
	got := r.failures()
	if len(got) != 1 || got[0].ID != rows[0].ID || got[0].State != prodevents.StateRejected || got[0].StateReason == nil ||
		*got[0].StateReason != reason || got[0].StateBy == nil || *got[0].StateBy != person {
		t.Fatalf("with both results retracted, failure rows = %+v, want the one row rejected with the second retraction's reason and actor", got)
	}
}

// TestAFirstMatchAndARetractionOfOneResultTakeItInTurn: a result's first match and its
// retraction's reject take the result's row in turn. A retraction that commits while the first
// match waits for the row leaves the first match writing no row. A reject that starts while the
// first match is between its write and its commit waits for that commit, then rejects the row the
// first match bound the result to: no counted row of a retracted result is left, whichever runs
// first.
func TestAFirstMatchAndARetractionOfOneResultTakeItInTurn(t *testing.T) {
	const reason = "The check ran against the staging cluster."
	lockStatement := "%from check_results where id = $1::uuid for no key update%"
	setup := func(t *testing.T) (*rig, string, string) {
		r := newRig(t)
		issue := r.issue(12)
		r.deployRun(280, at(60), sha("f"), prodJob, success, success)
		r.passDeploys()
		return r, issue, r.checkResult(issue, "c1", sha("f"), verdictFail, model.CheckWhenAfterMerge)
	}
	// firstMatch runs result's first match (RecordCheckFailure) on a connection of its own and
	// answers its error once it ends.
	firstMatch := func(r *rig, result string) <-chan error {
		done := make(chan error, 1)
		go func() {
			_, err := RecordCheckFailure(r.ctx, r.pool, result)
			done <- err
		}()
		return done
	}

	t.Run("the retraction commits while the first match waits", func(t *testing.T) {
		r, issue, result := setup(t)
		gate, err := r.pool.Begin(r.ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer gate.Rollback(r.ctx)
		if _, err := gate.Exec(r.ctx, `select 1 from check_results where id = $1::uuid for no key update`, result); err != nil {
			t.Fatalf("hold the result's row: %v", err)
		}
		matched := firstMatch(r, result)
		r.awaitLockWait("the first match", lockStatement, matched)
		r.retract(issue, result, "c1", sha("f"), reason)
		if err := gate.Rollback(r.ctx); err != nil {
			t.Fatalf("release the result's row: %v", err)
		}
		if err := <-matched; err != nil {
			t.Fatalf("the first match: %v", err)
		}
		r.passEvents()
		if rows, bound := r.failures(), r.boundTo(result); len(rows) != 0 || bound != "" {
			t.Fatalf("after a first match that waited behind the retraction, failure rows = %+v and the result bound to %q, want neither", rows, bound)
		}
	})

	t.Run("the reject waits for the first match in flight", func(t *testing.T) {
		r, issue, result := setup(t)
		const pauseKey = 978_0712
		r.exec("make the pause", `create function signals_test_pause() returns trigger language plpgsql as $$
			begin
				perform pg_advisory_xact_lock(tg_argv[0]::bigint);
				return new;
			end $$`)
		r.exec("pause each failure row's write", fmt.Sprintf(`create trigger signals_test_pause before insert on production_events
			for each row when (new.kind = 'failure') execute function signals_test_pause('%d')`, pauseKey))
		gate, err := r.pool.Begin(r.ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer gate.Rollback(r.ctx)
		if _, err := gate.Exec(r.ctx, `select pg_advisory_xact_lock($1)`, pauseKey); err != nil {
			t.Fatalf("hold the pause: %v", err)
		}
		matched := firstMatch(r, result)
		r.awaitLockWait("the first match", "%insert into production_events%", matched)
		r.retract(issue, result, "c1", sha("f"), reason)
		rejected := make(chan error, 1)
		go func() {
			_, err := RejectRetractedCheck(r.ctx, r.pool, result, reason, person, time.Now())
			rejected <- err
		}()
		r.awaitLockWait("the retraction's reject", lockStatement, rejected)
		if err := gate.Rollback(r.ctx); err != nil {
			t.Fatalf("release the pause: %v", err)
		}
		if err := <-matched; err != nil {
			t.Fatalf("the first match: %v", err)
		}
		if err := <-rejected; err != nil {
			t.Fatalf("the retraction's reject: %v", err)
		}
		r.passEvents()
		rows := r.failures()
		if len(rows) != 1 || r.boundTo(result) != rows[0].ID || rows[0].State != prodevents.StateRejected ||
			rows[0].StateReason == nil || *rows[0].StateReason != reason || rows[0].StateBy == nil || *rows[0].StateBy != person {
			t.Fatalf("after the first match and the reject, failure rows = %+v, want the one row the result is bound to, rejected with the retraction's reason and actor", rows)
		}
	})
}

// TestTwoDocumentsChecksOfOneBlockIDMakeTwoCheckFailRows: a block id names a check within one
// Verification document alone, so fails of two issues' after_merge checks that share a block id,
// at commits one deploy shipped, are two check_fail rows, each keyed by its own document
// (check:<document>/<block>@<deploy>). A retraction of one result rejects its own row and leaves
// the other's counted, since the other's fail still stands.
func TestTwoDocumentsChecksOfOneBlockIDMakeTwoCheckFailRows(t *testing.T) {
	r := newRig(t)
	first, second := r.issue(5), r.issue(6)
	r.deployRun(240, at(60), sha("f"), prodJob, success, success)
	r.passDeploys()
	deploy := r.deployOf(240)
	retracted := r.checkResult(first, "c1", sha("f"), verdictFail, model.CheckWhenAfterMerge)
	r.checkResult(second, "c1", sha("f"), verdictFail, model.CheckWhenAfterMerge)
	r.passEvents()

	firstRef := "check:" + r.verification(first) + "/c1@" + deploy.ID
	secondRef := "check:" + r.verification(second) + "/c1@" + deploy.ID
	states := func() map[string]string {
		held := map[string]string{}
		for _, row := range r.failures() {
			held[row.SourceRef] = row.State
		}
		return held
	}
	if got := states(); len(got) != 2 || got[firstRef] != prodevents.StateCounted || got[secondRef] != prodevents.StateCounted {
		t.Fatalf("check_fail rows by source_ref = %v, want %s and %s, both counted", got, firstRef, secondRef)
	}

	const reason = "The check of the first issue read the wrong cluster."
	r.exec("retract the first result", `insert into check_result_retractions (result_id, reason, actor) values ($1, $2, $3)`,
		retracted, reason, `{"kind":"user","id":"alice"}`)
	r.event(first, "result.retracted", person, model.CheckResultRetractedEventPayload{
		ResultID: retracted, Issue: first, CheckIssue: first, CheckBlockID: "c1", CommitSHA: sha("f"), Reason: reason,
	})
	r.passEvents()
	if got := states(); len(got) != 2 || got[firstRef] != prodevents.StateRejected || got[secondRef] != prodevents.StateCounted {
		t.Fatalf("after the first result's retraction, check_fail rows by source_ref = %v, want %s rejected and %s counted",
			got, firstRef, secondRef)
	}
}
