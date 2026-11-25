package workflow

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/pods"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// The CI news of LEGION-208's task keeps three purposes apart: the listener's report of its new
// runs and statuses (runs), a prompt to read Dispatch again when what Dispatch holds of a check at
// the task's head changed (state), and the fix rule's one repair assignment (assignment). These
// tests read the rows' kinds, purposes, receivers, readers and the values they carry, never a
// sentence.

// newsRoles are the roles a CI news row of LEGION-208 is published to.
var newsRoles = []claim.Role{claim.RoleArchitect, claim.RolePlanner, claim.RoleImplementer, claim.RoleTester, claim.RoleReviewer, claim.RoleMerger}

// roleOf is the role of LEGION-208 whose claim token is token, failing t for any other token.
func roleOf(t *testing.T, token string) claim.Role {
	t.Helper()
	for _, role := range newsRoles {
		if roleToken(t, role) == token {
			return role
		}
	}
	t.Fatalf("the token %q is no role of LEGION-208", token)
	return ""
}

// readerLine is the work reader names: `<phase>@<entry>` of a step, `fix-<n>@<entry>` of a fix,
// and "" where the row names no reader.
func readerLine(reader *record.CIReader) string {
	switch {
	case reader == nil:
		return ""
	case reader.Fix > 0:
		return fmt.Sprintf("fix-%d@%d", reader.Fix, reader.Entry)
	default:
		return fmt.Sprintf("%s@%d", reader.Phase, reader.Entry)
	}
}

// newsOf is each CI news row the outbox holds for LEGION-208, oldest first.
func (r *fixRig) newsOf() []record.CINews {
	r.t.Helper()
	var news []record.CINews
	for _, row := range issueOutboxOf(r.t, r.pool, "LEGION-208", string(record.OutboxKindCINews)) {
		var n record.CINews
		if err := json.Unmarshal(row.payload, &n); err != nil {
			r.t.Fatalf("decode the CI news row %s: %v", row.payload, err)
		}
		news = append(news, n)
	}
	return news
}

// news is the line of each CI news row of purpose the outbox holds for LEGION-208, sorted:
// `<role>`, then ` <reader>` where the row names one (readerLine), then ` next=<next>` where it
// names what Legion does next.
func (r *fixRig) news(purpose record.CINewsPurpose) []string {
	r.t.Helper()
	var lines []string
	for _, n := range r.newsOf() {
		if n.Purpose != purpose {
			continue
		}
		line := string(roleOf(r.t, n.Role))
		if reader := readerLine(n.Reader); reader != "" {
			line += " " + reader
		}
		if n.Next != record.CINextNone {
			line += " next=" + string(n.Next)
		}
		lines = append(lines, line)
	}
	slices.Sort(lines)
	return lines
}

// assignments is the line of each repair assignment of LEGION-208, oldest first:
// `<role> <reader> <check>@<head> <ciCase> fix <fix>`.
func (r *fixRig) assignments() []string {
	r.t.Helper()
	var lines []string
	for _, n := range r.newsOf() {
		if n.Purpose != record.CINewsAssignment || n.Assigned == nil {
			continue
		}
		a := n.Assigned
		lines = append(lines, fmt.Sprintf("%s %s %s@%s %s fix %d", roleOf(r.t, n.Role), readerLine(n.Reader), a.Check, a.Head, a.CICase, a.Fix))
	}
	return lines
}

// reportedRuns is, for each report of LEGION-208, its role and the ids of the runs it carries,
// sorted: `<role> <id>,<id>`.
func (r *fixRig) reportedRuns() []string {
	r.t.Helper()
	var lines []string
	for _, n := range r.newsOf() {
		if n.Purpose != record.CINewsRuns {
			continue
		}
		ids := make([]string, len(n.Runs))
		for i, run := range n.Runs {
			ids[i] = fmt.Sprint(run.ID)
		}
		slices.Sort(ids)
		lines = append(lines, string(roleOf(r.t, n.Role))+" "+fmt.Sprint(ids))
	}
	slices.Sort(lines)
	return lines
}

// receivers is the role of each CI news row of purpose the outbox holds for LEGION-208, sorted.
func (r *fixRig) receivers(purpose record.CINewsPurpose) []string {
	r.t.Helper()
	var roles []string
	for _, n := range r.newsOf() {
		if n.Purpose == purpose {
			roles = append(roles, string(roleOf(r.t, n.Role)))
		}
	}
	slices.Sort(roles)
	return roles
}

// delivered takes every CI news row and every role message of LEGION-208 out, as the lanes take a
// row out once it is delivered, so a later read finds only what the next fact writes.
func (r *fixRig) delivered() {
	r.t.Helper()
	if _, err := r.pool.Exec(r.t.Context(), `delete from outbox where issue = $1 and kind in ($2, $3)`, "LEGION-208",
		string(record.OutboxKindCINews), string(record.OutboxKindRoleMessage)); err != nil {
		r.t.Fatalf("take the CI news and the role messages of LEGION-208 out: %v", err)
	}
}

// settles applies the listener's settlement fact as its own event.
func (r *fixRig) settles(fact intake.Fact) {
	r.t.Helper()
	applyRefusingNothing(r.t, r.pool, r.engine, fact)
}

// stepReader is the reader line of role's own step in p, at the entry its phases row holds now.
func (r *fixRig) stepReader(role claim.Role, p phase.Phase) string {
	r.t.Helper()
	return fmt.Sprintf("%s@%d", p, roleRowOf(r.t, r.pool, r.engine, "LEGION-208", role).Entry)
}

// fixReader is the reader line of fix 1, at the entry it raised.
func (r *fixRig) fixReader() string {
	r.t.Helper()
	return fmt.Sprintf("fix-1@%d", r.latestFix().Entry)
}

// heldState is the state the daemon holds of check at fixHead.
func (r *fixRig) heldState(check string) record.CheckState {
	r.t.Helper()
	var held record.HeadStates
	applyInTx(r.t, r.pool, r.engine, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		held, err = r.engine.store.HeadStates(ctx, tx, "LEGION-208", statesRepo, fixHead)
		return err
	})
	return held.States[check]
}

// testerCompletedAt writes LEGION-208's tester's row completed with its test record naming head,
// as its completion writes it, while the issue stays in testing.
func (r *fixRig) testerCompletedAt(head string) {
	r.t.Helper()
	row := roleRowOf(r.t, r.pool, r.engine, "LEGION-208", claim.RoleTester)
	row.Issue, row.Role = "LEGION-208", claim.RoleTester
	row.HandoffRecord, row.HandoffVersion, row.HandoffCommit, row.Summary = "test-record", 1, head, "tested"
	applyInTx(r.t, r.pool, r.engine, func(ctx context.Context, tx pgx.Tx) error { return r.engine.store.PutPhase(ctx, tx, row) })
}

// heldAtTheEnd is LEGION-208's task held before its retro for its CI: its review approved while
// k-ci's job runs at fixHead.
func heldAtTheEnd(t *testing.T) *fixRig {
	t.Helper()
	r := reviewing(t)
	r.ciRunsAtTheEnd()
	r.approves()
	r.assertEndHold(record.HoldForCI)
	r.delivered()
	return r
}

// mergingOn is LEGION-208 on the steps words, each CI check passed at fixHead, at its merge, with
// moves the moves before its review.
func mergingOn(t *testing.T, words []string, moves ...TriggerKind) *fixRig {
	t.Helper()
	r := newFixRig(t, returnEngine(), words, moves...)
	r.at(fixHead, noCIJob("k-unit"), ciPassing("k-ci2"), ciPassing("k-ci"))
	r.move(TriggerReviewApproved, "", record.PhaseRow{})
	r.move(TriggerRetroCompleted, "", record.PhaseRow{})
	assertPhase(t, r.pool, phase.Merging)
	r.delivered()
	return r
}

// rawSuccess is the listener's settlement that k-ci's run succeeded at fixHead, a run whose name
// is the check's.
var rawSuccess = settledRunsAt(fixHead, 1, []record.AttemptRun{{Name: "k-ci", ID: 7}})

// A raw success authorizes nothing: at the end of a task held for its CI while k-ci's job runs,
// the listener's settlement that k-ci succeeded changes no state the daemon holds, ends no hold and
// starts no fix, and assigns nothing: only Dispatch's state of the check decides. The control:
// with a state written from the settlement (the seam stateFromSettlement) the held state reads
// yes, which the subtest asserts.
func TestARawSuccessAuthorizesNothing(t *testing.T) {
	t.Run("the held state stands", func(t *testing.T) {
		r := heldAtTheEnd(t)
		before := r.heldState("k-ci")
		r.settles(rawSuccess)
		if after := r.heldState("k-ci"); !sameHeld(before, after) || after.State == stateYes {
			t.Fatalf("k-ci after the raw success = %+v, want the state held before, %+v", after, before)
		}
		r.assertEndHold(record.HoldForCI)
		assertStrings(t, "the fixes", r.fixes(), nil)
		assertStrings(t, "the assignments", r.assignments(), nil)
	})

	t.Run("the control: a state written from the settlement reads yes", func(t *testing.T) {
		r := heldAtTheEnd(t)
		r.engine.stateFromSettlement = true
		r.settles(rawSuccess)
		if after := r.heldState("k-ci"); after.State != stateYes {
			t.Fatalf("k-ci after the raw success with a state written from it = %+v, want yes", after)
		}
	})
}

// sameHeld says whether a and b are one held state: its state, reason, CI case and sequence.
func sameHeld(a, b record.CheckState) bool {
	return a.State == b.State && a.Reason == b.Reason && a.CICase == b.CICase && a.Seq == b.Seq
}

// Dispatch's state decides against a raw success: where Dispatch holds k-ci skipped, unreadable,
// or no for a pass and a fail at one commit, the listener's settlement that k-ci succeeded leaves
// the state Dispatch published, and the end stays held as that state holds it (for the fix a
// failure starts, or for CI while the read waits). The control: with a state written from the
// settlement (the seam stateFromSettlement) the held state reads yes in each case.
func TestDispatchsStateDecidesAgainstARawSuccess(t *testing.T) {
	for _, tc := range []struct {
		name  string
		state intake.DispatchCheckState
		hold  record.HoldReason
	}{
		{"skipped", inCI(stateOf("k-ci", "no", "skipped", "ci"), "skipped"), record.HoldForFix},
		{"unreadable", inCI(stateOf("k-ci", "unknown", "read_failed", "ci"), "github_unreadable"), record.HoldForCI},
		{"a pass and a fail at one commit", inCI(stateOf("k-ci", "no", "pass_and_fail", "ci", "tester"), "success"), record.HoldForFix},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := heldAtTheEnd(t)
			r.at(fixHead, tc.state)
			r.assertEndHold(tc.hold)
			before := r.heldState("k-ci")
			r.settles(rawSuccess)
			if after := r.heldState("k-ci"); !sameHeld(before, after) {
				t.Fatalf("k-ci after the raw success = %+v, want Dispatch's %+v", after, before)
			}
			r.assertEndHold(tc.hold)
		})

		t.Run("the control: "+tc.name+" overwritten by a state written from the settlement", func(t *testing.T) {
			r := heldAtTheEnd(t)
			r.at(fixHead, tc.state)
			r.engine.stateFromSettlement = true
			r.settles(rawSuccess)
			if after := r.heldState("k-ci"); after.State != stateYes {
				t.Fatalf("k-ci after the raw success with a state written from it = %+v, want yes", after)
			}
		})
	}
}

// A failed rerun with no state event is reported: run 2 of unit fails again at the head, the
// settlement's text is the text before, and Dispatch publishes no state, yet each receiver gets
// one report carrying run 2 alone. The control: with the text compared first (the seam
// textDedupe) run 2 is reported to nobody, which the subtest asserts.
func TestARerunThatFailsAgainIsReportedWithNoStateEvent(t *testing.T) {
	rerun := settledRunsAt(fixHead, 2, []record.AttemptRun{{Name: "build", ID: 1}, {Name: "unit", ID: 2}}, "unit")
	t.Run("run 2 is reported", func(t *testing.T) {
		r := reviewing(t)
		r.settles(settledAt(fixHead, 1, "unit"))
		r.delivered()
		r.settles(rerun)
		assertStrings(t, "the reports of run 2", r.reportedRuns(), []string{"architect [2]", "implementer [2]", "reviewer [2]"})
		assertStrings(t, "the prompts", r.news(record.CINewsState), nil)
	})

	t.Run("the control: with the text compared first run 2 reaches nobody", func(t *testing.T) {
		r := reviewing(t)
		r.engine.textDedupe = true
		r.settles(settledAt(fixHead, 1, "unit"))
		r.delivered()
		r.settles(rerun)
		assertStrings(t, "the reports of run 2 with the text compared", r.reportedRuns(), nil)
	})
}

// One settlement gives one row to each receiver: a passing rerun of unit, and a first run at the
// merge, each reach each receiver once as a report, and no role message beside it. The control:
// with main's settlement message kept beside the report (the seam alsoRoleMessage) the reviewer
// gets two rows of one settlement, which the subtest asserts.
func TestOneSettlementGivesOneRowToEachReceiver(t *testing.T) {
	t.Run("a passing rerun", func(t *testing.T) {
		r := reviewing(t)
		r.settles(settledAt(fixHead, 1, "unit"))
		r.delivered()
		r.settles(settledRunsAt(fixHead, 2, []record.AttemptRun{{Name: "build", ID: 1}, {Name: "unit", ID: 2}}))
		assertStrings(t, "the reports of the passing rerun", r.reportedRuns(), []string{"architect [2]", "implementer [2]", "reviewer [2]"})
		for _, role := range []claim.Role{claim.RoleArchitect, claim.RoleImplementer, claim.RoleReviewer} {
			assertStrings(t, "the "+string(role)+"'s role messages", r.told(role), nil)
		}
	})

	t.Run("a first run at the merge", func(t *testing.T) {
		r := mergingOn(t, []string{"implementer", "reviewer"}, TriggerImplementationReady)
		r.settles(settledAt(fixHead, 1))
		assertStrings(t, "the reports at the merge", r.reportedRuns(), []string{"architect [1 1]", "merger [1 1]"})
		for _, role := range []claim.Role{claim.RoleArchitect, claim.RoleMerger} {
			assertStrings(t, "the "+string(role)+"'s role messages", r.told(role), nil)
		}
	})

	t.Run("the control: main's message beside the report is a second row", func(t *testing.T) {
		r := reviewing(t)
		r.engine.alsoRoleMessage = true
		r.settles(settledAt(fixHead, 1, "unit"))
		if reports, told := r.news(record.CINewsRuns), r.told(claim.RoleReviewer); !slices.Contains(reports, "reviewer "+r.stepReader(claim.RoleReviewer, phase.Reviewing)) || len(told) != 1 {
			t.Fatalf("the reviewer's rows with main's message kept = reports %q and messages %q, want one of each", reports, told)
		}
	})
}

// mixedSettlement is a settlement at fixHead of three runs: unit, the fix's failed job, lint, a
// failed job no check names, and build, a required job that succeeded.
var mixedSettlement = settledRunsAt(fixHead, 1, []record.AttemptRun{{Name: "unit", ID: 1}, {Name: "lint", ID: 2}, {Name: "build", ID: 3}}, "unit", "lint")

// A mixed settlement reaches each receiver whole: beside a running fix, the fix's failure, a
// failure no check names and a success each reach each receiver in one report, with no run mapped
// to a check, and a fix that names no job (still reading it, or its read failed) holds nothing
// back. The controls: with the settlement dropped while a fix is open (the seam dropBesideFix), and
// with the report waiting for the fix's job (the seam waitForJobName), nobody gets it, which the
// subtests assert.
func TestAMixedSettlementReachesEachReceiverWhole(t *testing.T) {
	whole := []string{"architect [1 2 3]", "implementer [1 2 3]", "reviewer [1 2 3]"}
	t.Run("beside a running fix", func(t *testing.T) {
		r := startedFix(t, intake.FixJobRead{Name: "unit", URL: fixPage})
		r.delivered()
		r.settles(mixedSettlement)
		assertStrings(t, "the reports beside the fix", r.reportedRuns(), whole)
		assertStrings(t, "the reports' readers", r.news(record.CINewsRuns), []string{"architect next=fix-runs",
			"implementer " + r.fixReader() + " next=fix-runs", "reviewer " + r.stepReader(claim.RoleReviewer, phase.Reviewing) + " next=fix-runs"})
	})

	t.Run("the control: dropped while a fix is open", func(t *testing.T) {
		r := startedFix(t, intake.FixJobRead{Name: "unit", URL: fixPage})
		r.engine.dropBesideFix = true
		r.delivered()
		r.settles(mixedSettlement)
		assertStrings(t, "the reports with the settlement dropped beside the fix", r.reportedRuns(), nil)
	})

	for _, tc := range []struct {
		name  string
		start func(r *fixRig)
	}{
		{"a fix still reading its job", func(r *fixRig) {}},
		{"a fix whose read found no job", func(r *fixRig) { r.readJob(intake.FixJobRead{ReadFailed: "no run of the check at the head"}) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := reviewing(t)
			r.fails(fixHead, "k-ci")
			tc.start(r)
			r.delivered()
			r.settles(mixedSettlement)
			assertStrings(t, "the reports beside "+tc.name, r.reportedRuns(), whole)
		})

		t.Run("the control: "+tc.name+", the report waiting for the job's name", func(t *testing.T) {
			r := reviewing(t)
			r.fails(fixHead, "k-ci")
			tc.start(r)
			r.engine.waitForJobName = true
			r.delivered()
			r.settles(mixedSettlement)
			assertStrings(t, "the reports waiting for the job's name", r.reportedRuns(), nil)
		})
	}
}

// The same runs are reported once: the settlement delivered again under its fact id, again under
// a new fact id, and afresh at a later generation with the same runs, each reports nothing more.
// The control: with the runs the record held before the settlement thrown away (the seam
// runsBeforeLost) the settlement afresh is reported a second time, which the subtest asserts.
func TestTheSameRunsAreReportedOnce(t *testing.T) {
	afresh := settledRunsAt(fixHead, 2, []record.AttemptRun{{Name: "build", ID: 1}, {Name: "unit", ID: 1}}, "unit")
	reported := []string{"architect [1 1]", "implementer [1 1]", "reviewer [1 1]"}
	t.Run("once", func(t *testing.T) {
		r := reviewing(t)
		apply := applyFacts(t, r.pool, r.engine)
		apply("settled", settledAt(fixHead, 1, "unit"))
		assertStrings(t, "the reports of the settlement", r.reportedRuns(), reported)
		apply("settled", settledAt(fixHead, 1, "unit"))
		apply("settled-again", settledAt(fixHead, 1, "unit"))
		apply("settled-afresh", afresh)
		assertStrings(t, "the reports after the same runs again", r.reportedRuns(), reported)
	})

	t.Run("the control: with the runs before thrown away the same runs are reported again", func(t *testing.T) {
		r := reviewing(t)
		apply := applyFacts(t, r.pool, r.engine)
		apply("settled", settledAt(fixHead, 1, "unit"))
		r.engine.runsBeforeLost = true
		apply("settled-afresh", afresh)
		assertStrings(t, "the reports with the runs before lost", r.reportedRuns(), slices.Sorted(slices.Values(slices.Concat(reported, reported))))
	})
}

// acceptedChangeCases is the table every rule of an accepted change of a check's state is held
// to: the store's answer and what it held before, the state written, and whether that is a change.
var acceptedChangeCases = []struct {
	name          string
	written, held bool
	before, after record.CheckState
	want          bool
}{
	{"a first state", true, false, record.CheckState{}, record.CheckState{State: "yes", Roles: []string{"tester"}, Seq: 1}, true},
	{"a lower sequence the store refused", false, true, record.CheckState{State: "yes", Seq: 4}, record.CheckState{State: "no", Reason: "fail", Seq: 3}, false},
	{"the same content at a higher sequence", true, true, record.CheckState{State: "yes", Roles: []string{"tester"}, Seq: 1},
		record.CheckState{State: "yes", Roles: []string{"tester"}, Seq: 2}, false},
	{"yes then no", true, true, record.CheckState{State: "yes", Seq: 2}, record.CheckState{State: "no", Reason: "fail", Seq: 3}, true},
	{"no then yes again", true, true, record.CheckState{State: "no", Reason: "fail", Seq: 3}, record.CheckState{State: "yes", Seq: 4}, true},
	{"a change of reason", true, true, record.CheckState{State: "no", Reason: "fail", Seq: 1}, record.CheckState{State: "no", Reason: "read_failed", Seq: 2}, true},
	{"a change of CI case", true, true, record.CheckState{State: "no", Reason: reasonNoResult, CICase: ciNotCompleted, Seq: 1},
		record.CheckState{State: "no", Reason: reasonNoResult, CICase: "absent", Seq: 2}, true},
	{"a change of roles", true, true, record.CheckState{State: "yes", Roles: []string{"tester"}, Seq: 1},
		record.CheckState{State: "yes", Roles: []string{"tester", "reviewer"}, Seq: 2}, true},
	{"the same roles in another order", true, true, record.CheckState{State: "yes", Roles: []string{"reviewer", "tester"}, Seq: 1},
		record.CheckState{State: "yes", Roles: []string{"tester", "reviewer"}, Seq: 2}, false},
}

// Only an accepted change of state prompts. The rule (acceptedChange) is a pure function held to a
// table of a lower sequence, the same content at a higher one, yes then no then yes, and a change
// of reason, CI case or roles; each broken copy of it (the store's answer ignored, the answer
// alone compared, one key for ever) fails the table, which the subtests assert. Through the
// engine, each event of that table gives the reviewer one prompt where it is a change and none
// where it is not.
func TestOnlyAnAcceptedChangeOfStatePrompts(t *testing.T) {
	type rule func(written, held bool, before, after record.CheckState) bool
	misses := func(change rule) []string {
		var missed []string
		for _, tc := range acceptedChangeCases {
			if change(tc.written, tc.held, tc.before, tc.after) != tc.want {
				missed = append(missed, tc.name)
			}
		}
		return missed
	}
	t.Run("the rule holds to the table", func(t *testing.T) {
		assertStrings(t, "the cases acceptedChange misses", misses(acceptedChange), nil)
	})
	for name, broken := range map[string]rule{
		"the store's answer ignored": func(_, held bool, before, after record.CheckState) bool {
			return acceptedChange(true, held, before, after)
		},
		"the answer alone compared": func(written, _ bool, _, _ record.CheckState) bool { return written },
		"one key for ever":          func(written, held bool, _, _ record.CheckState) bool { return written && !held },
	} {
		t.Run("the control: "+name+" fails the table", func(t *testing.T) {
			if missed := misses(broken); len(missed) == 0 {
				t.Fatalf("the rule with %s holds to the whole table, want a case it misses", name)
			}
		})
	}

	t.Run("through the engine", func(t *testing.T) {
		r := reviewing(t)
		reviewer := []string{"reviewer " + r.stepReader(claim.RoleReviewer, phase.Reviewing)}
		for _, step := range []struct {
			name   string
			serial int64
			state  intake.DispatchCheckState
			want   []string
		}{
			{"a first state", 10, stateOf("k-unit", stateYes, "", "tester"), reviewer},
			{"the same content at a higher sequence", 20, stateOf("k-unit", stateYes, "", "tester"), nil},
			{"yes then no", 30, stateOf("k-unit", "no", "fail", "tester"), reviewer},
			{"a lower sequence", 25, stateOf("k-unit", stateYes, "", "tester"), nil},
			{"no then yes again", 40, stateOf("k-unit", stateYes, "", "tester"), reviewer},
			{"a change of roles", 50, stateOf("k-unit", stateYes, "", "tester", "reviewer"), reviewer},
			{"a change of reason", 60, stateOf("k-unit", "no", "fail", "tester"), reviewer},
			{"a change of reason again", 70, stateOf("k-unit", "no", "read_failed", "tester"), reviewer},
			{"a first CI state", 80, jobRunning("k-ci2"), reviewer},
			{"a change of CI case", 90, inCI(stateOf("k-ci2", "no", reasonNoResult, "ci"), "absent"), reviewer},
		} {
			applyStates(t, r.pool, r.engine, "LEGION-208", statesRepo, fixHead, step.serial, ofSet(step.state, fixChecks...))
			assertStrings(t, "the prompts after "+step.name, r.news(record.CINewsState), step.want)
			r.delivered()
		}
	})
}

// A state at a former head prompts nobody: Dispatch keeps publishing states at a head the pull
// request has left, and the prompt is of the task's head alone; the same state at the head prompts
// the reviewer. The control: with the head fence taken out (the seam noHeadFence) the former
// head's state prompts the reviewer, which the subtest asserts.
func TestAStateAtAFormerHeadPromptsNobody(t *testing.T) {
	t.Run("nobody at a former head", func(t *testing.T) {
		r := reviewing(t)
		r.at(formerHead, ciPassing("k-ci"))
		assertStrings(t, "the prompts of a former head", r.news(record.CINewsState), nil)
		r.at(fixHead, ciPassing("k-ci"))
		assertStrings(t, "the prompts of the head", r.news(record.CINewsState), []string{"reviewer " + r.stepReader(claim.RoleReviewer, phase.Reviewing)})
	})

	t.Run("the control: with no head fence a former head prompts", func(t *testing.T) {
		r := reviewing(t)
		r.engine.noHeadFence = true
		r.at(formerHead, ciPassing("k-ci"))
		assertStrings(t, "the prompts of a former head with no fence", r.news(record.CINewsState), []string{"reviewer " + r.stepReader(claim.RoleReviewer, phase.Reviewing)})
	})
}

// A step that starts between the two sources misses nothing: the listener's settlement comes
// while the implementer works, the review starts, and then Dispatch's state changes: the prompt
// goes to the reviewer, the reader when the state changed. The control: with the readers chosen at
// the settlement only (the seam readersAtSettlement) the prompt goes to the implementer and the
// reviewer misses it, which the subtest asserts.
func TestAStepThatStartsBetweenTheSourcesMissesNothing(t *testing.T) {
	between := func(t *testing.T, seam bool) (*fixRig, string) {
		r := newFixRig(t, returnEngine(), []string{"implementer", "reviewer"})
		r.engine.readersAtSettlement = seam
		implementer := "implementer " + r.stepReader(claim.RoleImplementer, phase.Implementing)
		r.settles(settledAt(fixHead, 1))
		r.move(TriggerImplementationReady, "", record.PhaseRow{})
		assertPhase(t, r.pool, phase.Reviewing)
		r.delivered()
		r.at(fixHead, ciPassing("k-ci"))
		return r, implementer
	}
	t.Run("the reviewer is prompted", func(t *testing.T) {
		r, _ := between(t, false)
		assertStrings(t, "the prompts", r.news(record.CINewsState), []string{"reviewer " + r.stepReader(claim.RoleReviewer, phase.Reviewing)})
	})

	t.Run("the control: readers chosen at the settlement miss the reviewer", func(t *testing.T) {
		r, implementer := between(t, true)
		assertStrings(t, "the prompts to the settlement's readers", r.news(record.CINewsState), []string{implementer})
	})
}

// An end hold prompts no retro that was never made: the task held before its retro for its CI
// runs no step, so a change of what Dispatch holds there prompts nobody; the hold's own release
// decides. The control: with the readers chosen as the report's receivers (the seam
// readersAsReceivers) the retro's implementer is prompted, which the subtest asserts.
func TestAnEndHoldPromptsNoRetroThatWasNeverMade(t *testing.T) {
	absent := inCI(stateOf("k-ci", "no", reasonNoResult, "ci"), "absent")
	t.Run("nobody is prompted", func(t *testing.T) {
		r := heldAtTheEnd(t)
		r.at(fixHead, absent)
		r.assertEndHold(record.HoldForCI)
		assertStrings(t, "the prompts at the end hold", r.news(record.CINewsState), nil)
	})

	t.Run("the control: readers chosen as the receivers prompt the retro", func(t *testing.T) {
		r := heldAtTheEnd(t)
		r.engine.readersAsReceivers = true
		r.at(fixHead, absent)
		if prompts := r.news(record.CINewsState); !slices.Contains(prompts, "implementer "+r.stepReader(claim.RoleImplementer, phase.Retro)) {
			t.Fatalf("the prompts with the readers chosen as receivers = %q, want one to the retro's implementer", prompts)
		}
	})
}

// A failure at the merge reaches the merger and starts no fix: its failure is the merger's, the
// merger, which runs the merge, is prompted to read Dispatch, and nothing is assigned, on a task
// with an implementation step and on one with none. The control: with a fix started in merging
// (the seam fixInMerging) the same failure opens a fix, which the subtest asserts.
func TestAFailureAtTheMergeReachesTheMergerAndStartsNoFix(t *testing.T) {
	for _, tc := range []struct {
		name  string
		words []string
		moves []TriggerKind
	}{
		{"a task with an implementation step", []string{"implementer", "reviewer"}, []TriggerKind{TriggerImplementationReady}},
		{"a task with no implementation step", []string{"tester", "reviewer"}, []TriggerKind{TriggerTesterPassed}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := mergingOn(t, tc.words, tc.moves...)
			r.fails(fixHead, "k-ci")
			assertStrings(t, "the fixes", r.fixes(), nil)
			assertStrings(t, "the failures", r.failures(), []string{failureOf("k-ci", "merger", 0)})
			assertStrings(t, "the prompts", r.news(record.CINewsState), []string{"merger " + r.stepReader(claim.RoleMerger, phase.Merging)})
			assertStrings(t, "the assignments", r.assignments(), nil)
		})
	}

	t.Run("the control: a fix started in merging", func(t *testing.T) {
		r := mergingOn(t, []string{"implementer", "reviewer"}, TriggerImplementationReady)
		r.engine.fixInMerging = true
		r.fails(fixHead, "k-ci")
		assertStrings(t, "the fixes with a fix started in merging", r.fixes(), []string{fixOf(1, "reading", "k-ci")})
	})
}

// The report goes to main's receivers: the owner, the role of the step the issue stands in and the
// implementer, each once by its token; the owner once at acceptance, where it runs the step; the
// retro's implementer once; and no implementer in merging. Each receiver that is a current reader
// names its work. The control: with the implementer told in merging too (the seam
// implementerInMerging) the implementer gets a report there, which the subtest asserts.
func TestTheReportGoesToMainsReceivers(t *testing.T) {
	for _, tc := range []struct {
		name string
		rig  func(t *testing.T) *fixRig
		want func(r *fixRig) []string
	}{
		{"in review", reviewing, func(r *fixRig) []string {
			return []string{"architect", "implementer", "reviewer " + r.stepReader(claim.RoleReviewer, phase.Reviewing)}
		}},
		{"at acceptance", func(t *testing.T) *fixRig {
			r := newFixRig(t, returnEngine(), []string{"implementer", "reviewer", "acceptance:owner"}, TriggerImplementationReady)
			r.approves()
			assertPhase(t, r.pool, phase.Accepting)
			return r
		}, func(r *fixRig) []string { return []string{"architect accepting@0", "implementer"} }},
		{"at the retro", func(t *testing.T) *fixRig {
			r := newFixRig(t, returnEngine(), []string{"implementer", "reviewer"}, TriggerImplementationReady)
			r.at(fixHead, noCIJob("k-unit"), ciPassing("k-ci2"), ciPassing("k-ci"))
			r.move(TriggerReviewApproved, "", record.PhaseRow{})
			assertPhase(t, r.pool, phase.Retro)
			return r
		}, func(r *fixRig) []string {
			return []string{"architect", "implementer " + r.stepReader(claim.RoleImplementer, phase.Retro)}
		}},
		{"at the merge", func(t *testing.T) *fixRig { return mergingOn(t, []string{"implementer", "reviewer"}, TriggerImplementationReady) },
			func(r *fixRig) []string { return []string{"architect", "merger " + r.stepReader(claim.RoleMerger, phase.Merging)} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := tc.rig(t)
			r.delivered()
			r.settles(settledAt(fixHead, 1, "unit"))
			assertStrings(t, "the reports' receivers", r.news(record.CINewsRuns), tc.want(r))
		})
	}

	t.Run("the control: the implementer told in merging", func(t *testing.T) {
		r := mergingOn(t, []string{"implementer", "reviewer"}, TriggerImplementationReady)
		r.engine.implementerInMerging = true
		r.settles(settledAt(fixHead, 1, "unit"))
		if reports := r.news(record.CINewsRuns); !slices.Contains(reports, "implementer") {
			t.Fatalf("the reports in merging with the implementer told = %q, want one to the implementer", reports)
		}
	})
}

// actsNext is one case of Legion acting next for LEGION-208's task when a settlement comes: the
// rig, and the reports each receiver gets, by role, reader and next act.
type actsNext struct {
	name string
	rig  func(t *testing.T) *fixRig
	want func(r *fixRig, fixesCI bool) []string
}

// heldForTheFixFromReview is LEGION-208 held for its fix from reviewing: k-ci failed at fixHead
// while the review ran, fix 1 runs, and the review requested changes, whose report the fix holds.
func heldForTheFixFromReview(t *testing.T) *fixRig {
	t.Helper()
	r := startedFix(t, intake.FixJobRead{Name: "unit", URL: fixPage})
	r.move(TriggerReviewRejected, "", reviewReport("the widget renders twice"))
	r.assertHeldForTheFix(phase.Reviewing)
	return r
}

// testedOffHead is LEGION-208 in testing with its tester completed at headTwo, a head no pull
// request heads and that holds no set, while the pull request stands at fixHead.
func testedOffHead(t *testing.T) *fixRig {
	t.Helper()
	r := newFixRig(t, returnEngine(), []string{"implementer", "tester", "reviewer"}, TriggerImplementationReady)
	assertPhase(t, r.pool, phase.Testing)
	r.testerCompletedAt(headTwo)
	return r
}

// readerIf is " "+reader where the task takes a fix, the only task whose reports name a reader,
// and "" otherwise.
func readerIf(fixesCI bool, reader string) string {
	if !fixesCI {
		return ""
	}
	return " " + reader
}

// actsNextCases are the cases of TestASettlementWhileLegionActsNextNamesItsActAndNoOther.
var actsNextCases = []actsNext{
	{"held at the end for CI", func(t *testing.T) *fixRig {
		r := reviewing(t)
		r.ciRunsAtTheEnd()
		r.approves()
		r.assertEndHold(record.HoldForCI)
		return r
	}, func(r *fixRig, _ bool) []string {
		return []string{"architect next=end-hold-ci", "implementer next=end-hold-ci"}
	}},
	{"held at the end for fix 1", runningAtTheEnd, func(r *fixRig, fixesCI bool) []string {
		return []string{"architect next=end-hold-fix", "implementer" + readerIf(fixesCI, "fix-1@2") + " next=fix-runs"}
	}},
	{"held for fix 1 from the review", heldForTheFixFromReview, func(r *fixRig, fixesCI bool) []string {
		return []string{"architect next=fix-runs", "implementer" + readerIf(fixesCI, r.fixReader()) + " next=fix-runs", "reviewer next=fix-runs"}
	}},
	{"tested at a head of no pull request", testedOffHead, func(r *fixRig, _ bool) []string {
		return []string{"architect next=testing-move", "implementer next=testing-move", "tester next=testing-move"}
	}},
}

// A settlement while Legion itself acts next names that act, for each receiver, on a task that
// takes a fix and on one that takes none: held at the end for CI (end-hold-ci); held at the end for
// fix 1, where the implementer, which runs the fix, has fix-runs and the owner end-hold-fix; held
// for fix 1 from the review (a hold beside the fix), where every receiver has fix-runs; and in
// testing, with the tester completed at a head no pull request heads and that holds no set, where
// the row names the pull request's head and the tested head. The controls, each a subtest: with no
// read of what Legion does next (the seam noNextRead) no row names an act, and a task that takes no
// fix gets main's role message; with the act read from a hold for the fix alone (the seam
// nextByHoldAlone) the fix's own row and the rows beside the hold from the review name
// end-hold-fix; and with no tested head read (the seam noTestedHeadRead) the row names none.
func TestASettlementWhileLegionActsNextNamesItsActAndNoOther(t *testing.T) {
	settle := func(r *fixRig, fixesCI bool) {
		if !fixesCI {
			r.releaseBefore()
		}
		r.delivered()
		r.settles(settledAt(fixHead, 1, "unit"))
	}
	for _, tc := range actsNextCases {
		for _, fixesCI := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s, fixes_ci %t", tc.name, fixesCI), func(t *testing.T) {
				r := tc.rig(t)
				settle(r, fixesCI)
				assertStrings(t, "the reports", r.news(record.CINewsRuns), tc.want(r, fixesCI))
				for _, role := range newsRoles {
					assertStrings(t, "the "+string(role)+"'s role messages", r.told(role), nil)
				}
			})
		}
	}

	t.Run("the reports name the tested head", func(t *testing.T) {
		r := testedOffHead(t)
		settle(r, true)
		for _, n := range r.newsOf() {
			if n.Purpose == record.CINewsRuns && (n.Head != fixHead || n.Tested != headTwo) {
				t.Fatalf("the report %+v names head %q and tested head %q, want %q and %q", n, n.Head, n.Tested, fixHead, headTwo)
			}
		}
	})

	t.Run("the control: with no read of the next act", func(t *testing.T) {
		for _, fixesCI := range []bool{true, false} {
			r := actsNextCases[0].rig(t)
			r.engine.noNextRead = true
			settle(r, fixesCI)
			if fixesCI {
				assertStrings(t, "the reports with no next act read", r.news(record.CINewsRuns), []string{"architect", "implementer"})
				continue
			}
			assertStrings(t, "the reports of a task that takes no fix with no next act read", r.news(record.CINewsRuns), nil)
			if len(r.told(claim.RoleArchitect)) != 1 || len(r.told(claim.RoleImplementer)) != 1 {
				t.Fatalf("the role messages with no next act read = %q and %q, want main's one to each", r.told(claim.RoleArchitect), r.told(claim.RoleImplementer))
			}
		}
	})

	t.Run("the control: the next act read from a hold for the fix alone", func(t *testing.T) {
		r := runningAtTheEnd(t)
		r.engine.nextByHoldAlone = true
		settle(r, true)
		assertStrings(t, "the reports at the end hold for the fix", r.news(record.CINewsRuns), []string{"architect next=end-hold-fix", "implementer fix-1@2 next=end-hold-fix"})

		r = heldForTheFixFromReview(t)
		r.engine.nextByHoldAlone = true
		settle(r, true)
		assertStrings(t, "the reports beside the hold from the review", r.news(record.CINewsRuns), []string{"architect next=end-hold-fix",
			"implementer " + r.fixReader() + " next=end-hold-fix", "reviewer next=end-hold-fix"})
	})

	t.Run("the control: with no tested head read", func(t *testing.T) {
		r := testedOffHead(t)
		r.engine.noTestedHeadRead = true
		settle(r, true)
		for _, n := range r.newsOf() {
			if n.Purpose == record.CINewsRuns && n.Tested != "" {
				t.Fatalf("the report %+v with no tested head read names tested head %q, want none", n, n.Tested)
			}
		}
	})
}

// Ten checks of one run changing before the prompt is delivered give the reviewer one prompt,
// holding each check's state and the head's set. The control: with the join taken out (the seam
// noPromptJoin) they give ten, which the subtest asserts.
func TestTenChecksOfOneRunGiveOneWaitingPrompt(t *testing.T) {
	checks := make([]string, 10)
	for i := range checks {
		checks[i] = fmt.Sprintf("k-ci-%d", i)
	}
	tenChanges := func(r *fixRig) {
		for i, check := range checks {
			applyStates(r.t, r.pool, r.engine, "LEGION-208", statesRepo, fixHead, int64(100+i), ofSet(ciPassing(check), checks...))
		}
	}
	t.Run("one prompt", func(t *testing.T) {
		r := reviewing(t)
		tenChanges(r)
		prompts := r.newsOf()
		if len(prompts) != 1 || prompts[0].Purpose != record.CINewsState || len(prompts[0].States) != 10 ||
			prompts[0].Checks == nil || !slices.Equal(*prompts[0].Checks, checks) {
			t.Fatalf("the prompts of ten checks = %+v, want one holding ten states and the head's set", prompts)
		}
	})

	t.Run("the control: with no join, ten prompts", func(t *testing.T) {
		r := reviewing(t)
		r.engine.noPromptJoin = true
		tenChanges(r)
		if prompts := r.news(record.CINewsState); len(prompts) != 10 {
			t.Fatalf("the prompts of ten checks with no join = %q, want ten", prompts)
		}
	})
}

// The settlement's wake names what Legion does next: a released tester in testing, its row
// completed, on a task that takes no fix, and a settlement whose runs all succeeded: the
// settlement wakes the tester once, and the tester's report of that settlement names testing-move,
// the act its wake's detail is built from too. The control: with no read of the next act (the
// seam noNextRead) the wake stands and the tester gets no report naming testing-move, since main
// tells no role this settlement woke.
func TestTheSettlementWakeNamesWhatLegionDoesNext(t *testing.T) {
	woken := func(t *testing.T, seam bool) *fixRig {
		r := newFixRig(t, returnEngine(), []string{"implementer", "tester", "reviewer"}, TriggerImplementationReady)
		r.releaseBefore()
		r.testerCompletedAt(fixHead)
		r.engine.noNextRead = seam
		r.engine.cfg.Pods = wakePods(t, r.pool)
		r.releasedBesideTheFix(claim.RoleTester)
		r.settles(settledAt(fixHead, 1))
		resumes := wakeResumes(t, r.pool)
		if len(resumes) != 1 || resumes[0] != [2]string{string(claim.RoleTester), pods.WakeChecks} {
			t.Fatalf("pod resumes = %v, want the tester's settlement wake alone", resumes)
		}
		return r
	}
	t.Run("the tester's report names testing-move", func(t *testing.T) {
		r := woken(t, false)
		if reports := r.news(record.CINewsRuns); !slices.Contains(reports, "tester next=testing-move") {
			t.Fatalf("the reports = %q, want the tester's naming testing-move", reports)
		}
	})

	t.Run("the control: with no read of the next act", func(t *testing.T) {
		r := woken(t, true)
		if reports := r.news(record.CINewsRuns); slices.Contains(reports, "tester next=testing-move") {
			t.Fatalf("the reports with no next act read = %q, want none to the tester naming testing-move", reports)
		}
	})
}
