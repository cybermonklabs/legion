package workflow

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// firstConflictRead is when the first CONFLICTING read of these tests is made. Each later read is
// applied through an engine whose clock stands that long after it (mergingEngineAt): a fact a test
// mints carries no read time, so the engine's clock times both reads (mergeability).
var firstConflictRead = time.Date(2026, 11, 19, 1, 0, 0, 0, time.UTC)

// conflictHead is the head of #42 GitHub answers CONFLICTING with main (seedConflictIssue), and
// forwardHead the head the implementer's merge of main forward onto it leaves. Each is spelled as
// GitHub spells a commit (headOne, headTwo), so a text that names a head names it by a word that
// says nothing else.
const (
	conflictHead = headOne
	forwardHead  = headTwo
)

// readsAfter is an engine whose clock stands d after the first read.
func readsAfter(d time.Duration) *Engine { return mergingEngineAt(firstConflictRead.Add(d)) }

// seedConflictIssue seeds LEGION-208 standing in at with Dispatch status status (standsIn), its
// open pull request #42 at conflictHead onto main as pr records it otherwise, the implementer's
// phases row, and the phases row of the phase's own worker where that is another role, as the
// issue stands when GitHub first answers its head CONFLICTING.
func seedConflictIssue(t *testing.T, at phase.Phase, status string, pr record.PullRequest) *pgxpool.Pool {
	t.Helper()
	pool := migratedPool(t)
	standsIn(t, pool, at, status)
	pr.State, pr.Issue, pr.Repo, pr.Number, pr.Branch, pr.Base, pr.HeadSHA =
		record.PullRequestOpen, "LEGION-208", "sjawhar/legion", 42, "legion/LEGION-208", "main", conflictHead
	if pr.Failing == nil {
		pr.Failing = []string{}
	}
	seedPR(t, pool, pr)
	seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: "implement-claim", Entry: 1})
	if worker := RoleFor(at); worker != "" && worker != claim.RoleImplementer && worker != claim.RoleArchitect {
		seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: worker, Claim: claim.Token(string(worker) + "-claim"), Entry: 1})
	}
	return pool
}

// settledGreen is a pull request whose CI settled at conflictHead with a run of each name given,
// every one of them succeeded, as the record keeps a settlement (classify.ApplySettlement).
func settledGreen(names ...string) record.PullRequest {
	runs := make([]record.AttemptRun, len(names))
	for i, name := range names {
		runs[i] = record.AttemptRun{Name: name, ID: int64(i + 1), Head: conflictHead, Conclusion: record.RunSucceeded}
	}
	return record.PullRequest{CheckedHead: conflictHead, CheckRuns: runs, Failing: []string{}, Cancelled: []string{}}
}

// conflictingRead is the daemon's read of #42 finding its head CONFLICTING with main, carrying the
// base's rules as the daemon read them.
func conflictingRead(rules *record.BaseRules) intake.PullRequestMergeability {
	return intake.PullRequestMergeability{Repo: "sjawhar/legion", Number: 42, Base: "main", HeadSHA: conflictHead,
		Mergeable: record.MergeabilityConflicting, MergeState: record.MergeStateDirty, Rules: rules}
}

// confirmsConflict is a first CONFLICTING read and the second read that confirms it, one
// conflictConfirmWait later.
func confirmsConflict(t *testing.T, pool *pgxpool.Pool, rules *record.BaseRules) {
	t.Helper()
	applyRefusingNothing(t, pool, readsAfter(0), conflictingRead(rules))
	applyRefusingNothing(t, pool, readsAfter(conflictConfirmWait), conflictingRead(rules))
}

// mergeForwardMessages is every merge-forward message the outbox holds, in the order written, each
// decoded as the outbox decodes it (queuedRows).
func mergeForwardMessages(t *testing.T, pool *pgxpool.Pool) []record.MergeForwardMessage {
	t.Helper()
	messages := []record.MergeForwardMessage{}
	for _, row := range queuedRows(t, pool) {
		if message, ok := row.payload.(record.MergeForwardMessage); ok {
			messages = append(messages, message)
		}
	}
	return messages
}

// conflictConfirms is the due time of every second read of a conflict the outbox holds, in the
// order written, each of them a read of #42 at conflictHead due a minute after the first read it
// carries (record.ConflictConfirm.FirstReadAt).
func conflictConfirms(t *testing.T, pool *pgxpool.Pool) []time.Time {
	t.Helper()
	due := []time.Time{}
	for _, row := range issueOutboxOf(t, pool, "LEGION-208", string(record.OutboxKindConflictConfirm)) {
		payload, err := record.DecodeOutboxPayload(record.OutboxRow{ID: row.id, Kind: record.OutboxKindConflictConfirm, Payload: row.payload})
		if err != nil {
			t.Fatalf("decode the second read %d: %v", row.id, err)
		}
		if read := payload.(record.ConflictConfirm); read.Number != 42 || read.Head != conflictHead || !row.nextAt.Equal(read.FirstReadAt.Add(conflictConfirmWait)) {
			t.Fatalf("a second read due at %s reads %+v, want #42 at %s, due a minute after the first read it carries", row.nextAt, read, conflictHead)
		}
		due = append(due, row.nextAt)
	}
	return due
}

// assertNothingActed is a conflict the daemon acted on in no way: no merge-forward message, no
// notice to the architect, no worker started, no status written, and the issue in the phase it
// stood in.
func assertNothingActed(t *testing.T, pool *pgxpool.Pool, at phase.Phase) {
	t.Helper()
	if messages := mergeForwardMessages(t, pool); len(messages) != 0 {
		t.Fatalf("merge-forward messages %+v, want none", messages)
	}
	if notices := noticeKinds(t, pool, "LEGION-208"); len(notices) != 0 {
		t.Fatalf("notices %v, want none", notices)
	}
	assertOutboxCount(t, pool, "supervise", 0)
	assertOutboxCount(t, pool, "dispatch_status", 0)
	if got := issuePhase(t, pool); got != at {
		t.Fatalf("the issue is in %s, want it still in %s", got, at)
	}
}

// askedOnce is the one merge forward the record and the outbox hold, asked of conflictHead onto
// main in at for why: the record's ask, the ask to the role asked - the merger in merging, the
// implementer in every phase before it (cybermonklabs/legion#4979) - and, where hold names a role,
// that role's hold, and the architect's one checks-red notice outside implementing, which names the
// role asked. Nothing moved: the issue is in at with the status it had, and no worker was started.
func askedOnce(t *testing.T, pool *pgxpool.Pool, at phase.Phase, status string, why record.ConflictWhy, hold claim.Role) {
	t.Helper()
	// Written out rather than read from record.MergeForwardAsker, so an asker that routes wrong
	// fails here instead of setting the expectation it is checked against.
	asker := claim.RoleImplementer
	if at == phase.Merging {
		asker = claim.RoleMerger
	}
	asked := recordedPR(t, pool, "LEGION-208").MergeForward
	if asked == nil || asked.Head != conflictHead || asked.Base != "main" || asked.Role != asker || asked.Phase != at || asked.Why != why || asked.Landed != "" || asked.AskedAt.IsZero() {
		t.Fatalf("the record's merge forward = %+v, want %s onto main asked of the %s in %s for %s, not landed", asked, conflictHead, asker, at, why)
	}
	want := []record.MergeForwardMessage{{Role: asker, Step: record.MergeForwardAsk, Head: conflictHead, Base: "main", Phase: at, PullRequest: 42, Why: why}}
	if hold != "" {
		want = append(want, record.MergeForwardMessage{Role: hold, Step: record.MergeForwardHold, Head: conflictHead, Base: "main", Phase: at, PullRequest: 42, Why: why})
	}
	if got := mergeForwardMessages(t, pool); !reflect.DeepEqual(got, want) {
		t.Fatalf("merge-forward messages %+v, want %+v", got, want)
	}
	notices := noticeKinds(t, pool, "LEGION-208")
	if at == phase.Implementing {
		if len(notices) != 0 {
			t.Fatalf("notices %v, want none: a merge forward in implementing tells the implementer alone", notices)
		}
	} else {
		if len(notices) != 1 || notices[0] != "checks-red" {
			t.Fatalf("notices %v, want the one checks-red", notices)
		}
		if reason, stays := checksRedNotice(t, pool); stays != at || !strings.Contains(reason, "the "+string(asker)+" is asked to merge main forward") {
			t.Fatalf("the checks-red notice names phase %q with reason %q, want %s, the phase the issue stays in, and a reason naming the %s", stays, reason, at, asker)
		}
	}
	assertOutboxCount(t, pool, "supervise", 0)
	assertOutboxCount(t, pool, "dispatch_status", 0)
	assertOutboxCount(t, pool, "dispatch_message", 0)
	if got, gotStatus := issuePhase(t, pool), recordedStatus(t, pool, "LEGION-208"); got != at || gotStatus != status {
		t.Fatalf("the issue is in %s with status %q, want it still in %s with %q", got, gotStatus, at, status)
	}
}

// GitHub computes mergeability in a background job and can answer CONFLICTING about a base it has
// not caught up with, so one read acts on nothing (dispatch://INFRA-4374 R2). The first read
// records itself and queues the second read a minute after it; a MERGEABLE read releases it and
// logs both reads; a CONFLICTING read sooner than the minute changes nothing, and the one a minute
// after the first confirms the conflict and acts; the second read's row ending on GitHub's UNKNOWN
// releases the first read, and the next CONFLICTING read is a first read again; a read against
// another base branch is a first read of its own; and a read a minute later of another head than
// the record stands at confirms nothing.
func TestAConflictIsActedOnOnlyAfterASecondRead(t *testing.T) {
	// The rules are unread, so a confirmed conflict is acted on at once (classify.ConflictNeedsMerge):
	// what holds the act back below is the second read alone.
	unread := &record.BaseRules{Unread: "HTTP 502"}
	firstRead := func(t *testing.T, pool *pgxpool.Pool) {
		t.Helper()
		applyRefusingNothing(t, pool, readsAfter(0), conflictingRead(unread))
		if due := conflictConfirms(t, pool); len(due) != 1 || !due[0].Equal(firstConflictRead.Add(time.Minute)) {
			t.Fatalf("second reads due at %v, want exactly one, a minute after the first read at %s", due, firstConflictRead)
		}
		if first := recordedPR(t, pool, "LEGION-208").ConflictRead; first == nil ||
			first.Head != conflictHead || first.Base != "main" || !first.At.Equal(firstConflictRead) {
			t.Fatalf("the first read = %+v, want head on main read at %s", first, firstConflictRead)
		}
		if held := recordedPR(t, pool, "LEGION-208").ConflictingHead; held != "" {
			t.Fatalf("the record holds %q conflicting after one read, want none", held)
		}
		assertNothingActed(t, pool, phase.Testing)
	}

	t.Run("a mergeable read a minute later releases the first read", func(t *testing.T) {
		pool := seedConflictIssue(t, phase.Testing, "testing", record.PullRequest{})
		firstRead(t, pool)
		var logged bytes.Buffer
		engine := readsAfter(time.Minute)
		engine.log = slog.New(slog.NewJSONHandler(&logged, nil))
		applyRefusingNothing(t, pool, engine, intake.PullRequestMergeability{Repo: "sjawhar/legion", Number: 42,
			Base: "main", HeadSHA: conflictHead, Mergeable: record.MergeabilityMergeable, MergeState: record.MergeStateClean})
		if pr := recordedPR(t, pool, "LEGION-208"); pr.ConflictRead != nil || pr.ConflictingHead != "" {
			t.Fatalf("after the mergeable read the record holds first read %+v and %q conflicting, want neither", pr.ConflictRead, pr.ConflictingHead)
		}
		assertNothingActed(t, pool, phase.Testing)
		var released []map[string]any
		for _, line := range strings.Split(strings.TrimSpace(logged.String()), "\n") {
			var entry map[string]any
			if json.Unmarshal([]byte(line), &entry) == nil && entry["msg"] == "workflow: a second read did not confirm the conflict" {
				released = append(released, entry)
			}
		}
		loggedAt := func(field string, want time.Time) bool {
			text, _ := released[0][field].(string)
			at, err := time.Parse(time.RFC3339Nano, text)
			return err == nil && at.Equal(want)
		}
		if len(released) != 1 || released[0]["head"] != conflictHead || released[0]["base"] != "main" ||
			released[0]["mergeable"] != string(record.MergeabilityMergeable) || released[0]["mergeState"] != string(record.MergeStateClean) ||
			!loggedAt("firstReadAt", firstConflictRead) || !loggedAt("readAt", firstConflictRead.Add(time.Minute)) {
			t.Fatalf("the engine logged the release as %v, want one line naming %s on main, read first at %s and again at %s, GitHub's MERGEABLE and clean",
				released, conflictHead, firstConflictRead, firstConflictRead.Add(time.Minute))
		}
	})

	t.Run("a conflicting read sooner than a minute waits, and the one a minute after the first acts", func(t *testing.T) {
		pool := seedConflictIssue(t, phase.Testing, "testing", record.PullRequest{})
		firstRead(t, pool)
		applyRefusingNothing(t, pool, readsAfter(30*time.Second), conflictingRead(unread))
		if first := recordedPR(t, pool, "LEGION-208").ConflictRead; first == nil || !first.At.Equal(firstConflictRead) {
			t.Fatalf("after a read 30 seconds on the first read = %+v, want the one read at %s", first, firstConflictRead)
		}
		if due := conflictConfirms(t, pool); len(due) != 1 {
			t.Fatalf("second reads due at %v, want still the one", due)
		}
		assertNothingActed(t, pool, phase.Testing)

		applyRefusingNothing(t, pool, readsAfter(time.Minute), conflictingRead(unread))
		if pr := recordedPR(t, pool, "LEGION-208"); pr.ConflictRead != nil || pr.ConflictingHead != conflictHead {
			t.Fatalf("after the confirming read the record holds first read %+v and %q conflicting, want the conflict held for head alone", pr.ConflictRead, pr.ConflictingHead)
		}
		askedOnce(t, pool, phase.Testing, "testing", record.ConflictRulesUnread, claim.RoleTester)
	})

	t.Run("the second read's row ending on unknown releases the first read", func(t *testing.T) {
		pool := seedConflictIssue(t, phase.Testing, "testing", record.PullRequest{})
		firstRead(t, pool)
		applyRefusingNothing(t, pool, readsAfter(3*time.Minute), intake.ConflictConfirmDone{Issue: "LEGION-208", Number: 42, Head: conflictHead, FirstReadAt: firstConflictRead})
		if pr := recordedPR(t, pool, "LEGION-208"); pr.ConflictRead != nil || pr.ConflictingHead != "" {
			t.Fatalf("after the second read ended the record holds first read %+v and %q conflicting, want neither", pr.ConflictRead, pr.ConflictingHead)
		}
		assertNothingActed(t, pool, phase.Testing)

		// The next CONFLICTING read is a first read again, with a second read of its own.
		applyRefusingNothing(t, pool, readsAfter(4*time.Minute), conflictingRead(unread))
		if due := conflictConfirms(t, pool); len(due) != 2 || !due[1].Equal(firstConflictRead.Add(5*time.Minute)) {
			t.Fatalf("second reads due at %v, want a second one a minute after the read that followed the release", due)
		}
		assertNothingActed(t, pool, phase.Testing)
	})

	t.Run("a read against another base branch is a first read of its own", func(t *testing.T) {
		pool := seedConflictIssue(t, phase.Testing, "testing", record.PullRequest{})
		firstRead(t, pool)
		retargeted := conflictingRead(unread)
		retargeted.Base = "release"
		applyRefusingNothing(t, pool, readsAfter(time.Minute), retargeted)
		if first := recordedPR(t, pool, "LEGION-208").ConflictRead; first == nil ||
			first.Head != conflictHead || first.Base != "release" || !first.At.Equal(firstConflictRead.Add(time.Minute)) {
			t.Fatalf("after a read against release the first read = %+v, want that read's own", first)
		}
		assertNothingActed(t, pool, phase.Testing)
	})

	t.Run("a read a minute later of another head confirms nothing", func(t *testing.T) {
		pool := seedConflictIssue(t, phase.Testing, "testing", record.PullRequest{})
		firstRead(t, pool)
		other := conflictingRead(unread)
		other.HeadSHA = headThree
		applyRefusingNothing(t, pool, readsAfter(time.Minute), other)
		pr := recordedPR(t, pool, "LEGION-208")
		if first := pr.ConflictRead; first == nil || first.Head != conflictHead || first.Base != "main" || !first.At.Equal(firstConflictRead) {
			t.Fatalf("after a read of another head the first read = %+v, want the first read of %s on main at %s as it was", first, conflictHead, firstConflictRead)
		}
		if pr.ConflictingHead != "" {
			t.Fatalf("after a read of another head the record holds %q conflicting, want nothing confirmed", pr.ConflictingHead)
		}
		assertNothingActed(t, pool, phase.Testing)
	})
}

// A confirmed conflict that needs a merge keeps the issue in its phase (dispatch://INFRA-4374 R1).
// Before the review the work is still the implementer's: in testing and reviewing, where the need
// is a check the base requires that is not green, the implementer is asked to merge the base
// forward on its own role topic, the worker of the phase - the tester or the reviewer - is told to
// hold while the merge runs, and the architect gets one checks-red notice naming the phase the
// issue stays in. No worker starts, no status is written and no phase ends. The same read again
// asks nothing a second time.
func TestAConflictKeepsPhaseAndAsksTheImplementerToMergeForward(t *testing.T) {
	requiresCI := &record.BaseRules{Required: []string{"ci"}}
	for _, tc := range []struct {
		at     phase.Phase
		status string
		why    record.ConflictWhy
		hold   claim.Role
	}{
		{phase.Testing, "testing", record.ConflictCheckNotGreen, claim.RoleTester},
		{phase.Reviewing, "needs_review", record.ConflictCheckNotGreen, claim.RoleReviewer},
	} {
		t.Run(string(tc.at), func(t *testing.T) {
			pool := seedConflictIssue(t, tc.at, tc.status, settledGreen("lint"))
			confirmsConflict(t, pool, requiresCI)
			askedOnce(t, pool, tc.at, tc.status, tc.why, tc.hold)

			applyRefusingNothing(t, pool, readsAfter(2*time.Minute), conflictingRead(requiresCI))
			askedOnce(t, pool, tc.at, tc.status, tc.why, tc.hold)
		})
	}
}

// Past the review a conflict is the merger's (cybermonklabs/legion#4979). In merging the merger is
// asked, on its own role topic, to merge the base forward and resolve the conflict itself: no ask
// and no hold reach the implementer or anyone else, the architect's one checks-red notice names the
// merger, and the issue stays in merging. In accepting and retro nothing is asked of anyone, whatever
// the base's rules: the conflict is recorded for the move into merging, where the merger makes
// that merge, and it never goes back to the implementer. The same read again asks nothing more.
func TestAConflictPastTheReviewIsTheMergersToMergeForward(t *testing.T) {
	t.Run("merging", func(t *testing.T) {
		requiresCI := &record.BaseRules{Required: []string{"ci"}}
		pool := seedConflictIssue(t, phase.Merging, "retro", settledGreen("lint"))
		confirmsConflict(t, pool, requiresCI)
		askedOnce(t, pool, phase.Merging, "retro", record.ConflictMergeNext, "")

		applyRefusingNothing(t, pool, readsAfter(2*time.Minute), conflictingRead(requiresCI))
		askedOnce(t, pool, phase.Merging, "retro", record.ConflictMergeNext, "")
	})
	for _, at := range []struct {
		phase  phase.Phase
		status string
	}{{phase.Accepting, "needs_review"}, {phase.Retro, "retro"}} {
		for _, rules := range []struct {
			name  string
			rules *record.BaseRules
		}{
			{"a required check not green", &record.BaseRules{Required: []string{"ci"}}},
			{"rules unread", &record.BaseRules{Unread: "HTTP 502"}},
			{"a strict ruleset", &record.BaseRules{Required: []string{"lint"}, Strict: true}},
			// A read that carried no rules at all, which the log of the decision must survive.
			{"no rules read", nil},
		} {
			t.Run(string(at.phase)+", "+rules.name, func(t *testing.T) {
				pool := seedConflictIssue(t, at.phase, at.status, settledGreen("lint"))
				confirmsConflict(t, pool, rules.rules)
				pr := recordedPR(t, pool, "LEGION-208")
				if pr.ConflictingHead != conflictHead || pr.MergeForward != nil {
					t.Fatalf("the record holds %q conflicting and the merge forward %+v, want the conflict at %s recorded and nothing asked",
						pr.ConflictingHead, pr.MergeForward, conflictHead)
				}
				assertNothingActed(t, pool, at.phase)
			})
		}
	}
}

// The role first asked to merge a head forward keeps that merge until it lands, since it may
// already be at work on it in the workspace the roles share, so two roles are never sent to merge
// one base forward (mergeForward). An ask of the implementer that has not landed when the issue
// stands in merging - one a daemon before cybermonklabs/legion#4979 made there, which names no role and
// so reads as the implementer's (record.MergeForward.Asked) - stays the implementer's: a later
// confirmed read asks nobody, and its rows, the implementer's ask and the merger's hold, are still
// delivered. An ask of the implementer made before the review that has not landed when the issue
// moves into merging stays the implementer's too: the move holds the merger (holdMerger) rather
// than asking it, and once the implementer's merge lands the merger is told the new head and the
// reviewer is asked to read it.
func TestAnAskOfTheImplementerThatHasNotLandedKeepsItsMergeIntoMerging(t *testing.T) {
	requiresCI := &record.BaseRules{Required: []string{"ci"}}
	left := func(t *testing.T, pool *pgxpool.Pool, role claim.Role, step record.MergeForwardStep) string {
		t.Helper()
		pr := recordedPR(t, pool, "LEGION-208")
		return leftMergeForwardAt(storedIssue(t, pool, "LEGION-208"), false, &pr, role, step, conflictHead)
	}

	t.Run("an ask the release before made in merging", func(t *testing.T) {
		pool := seedConflictIssue(t, phase.Merging, "retro", askedBeforeTheMerger(phase.Merging))
		confirmsConflict(t, pool, requiresCI)
		if messages := mergeForwardMessages(t, pool); len(messages) != 0 {
			t.Fatalf("merge-forward messages %+v, want none: the implementer's ask of the head stands", messages)
		}
		if asked := recordedPR(t, pool, "LEGION-208").MergeForward; asked == nil || asked.Asked() != claim.RoleImplementer || asked.Head != conflictHead {
			t.Fatalf("the record's merge forward = %+v, want the implementer's ask of %s kept", asked, conflictHead)
		}
		if why := left(t, pool, claim.RoleImplementer, record.MergeForwardAsk); why != "" {
			t.Fatalf("the implementer's ask is undelivered (%s), want it delivered: the ask the record holds is its own", why)
		}
		if why := left(t, pool, claim.RoleMerger, record.MergeForwardHold); why != "" {
			t.Fatalf("the merger's hold is undelivered (%s), want it delivered: the implementer is the one merging", why)
		}
	})

	t.Run("an ask made in reviewing, carried into merging", func(t *testing.T) {
		pr := settledGreen("lint")
		pr.MergeForward = &record.MergeForward{Head: conflictHead, Base: "main", Role: claim.RoleImplementer, Phase: phase.Reviewing,
			Why: record.ConflictCheckNotGreen, AskedAt: firstConflictRead}
		pool := seedConflictIssue(t, phase.Retro, "retro", pr)
		confirmsConflict(t, pool, requiresCI)
		applyRefusingNothing(t, pool, readsAfter(2*time.Minute), intake.HandoffComplete{Generation: 1, Issue: "LEGION-208",
			Role: claim.RoleImplementer, Claim: "implement-claim", Summary: "retro done", Commit: conflictHead})
		if got := issuePhase(t, pool); got != phase.Merging {
			t.Fatalf("after retro's completion the issue is in %s, want merging", got)
		}
		hold := record.MergeForwardMessage{Role: claim.RoleMerger, Step: record.MergeForwardHold, Head: conflictHead, Base: "main",
			Phase: phase.Merging, PullRequest: 42, Why: record.ConflictCheckNotGreen}
		if got := mergeForwardMessages(t, pool); !reflect.DeepEqual(got, []record.MergeForwardMessage{hold}) {
			t.Fatalf("the move into merging queued %+v, want the merger's one hold %+v and no ask", got, hold)
		}
		if asked := recordedPR(t, pool, "LEGION-208").MergeForward; asked == nil || asked.Asked() != claim.RoleImplementer || asked.Phase != phase.Reviewing {
			t.Fatalf("the record's merge forward = %+v, want the implementer's ask made in reviewing kept", asked)
		}

		landsMergeForward(t, pool)
		landed := func(role claim.Role, step record.MergeForwardStep) record.MergeForwardMessage {
			return record.MergeForwardMessage{Role: role, Step: step, Head: conflictHead, Landed: forwardHead, Base: "main",
				Phase: phase.Merging, PullRequest: 42, Why: record.ConflictCheckNotGreen}
		}
		want := []record.MergeForwardMessage{hold, landed(claim.RoleMerger, record.MergeForwardLanded), landed(claim.RoleReviewer, record.MergeForwardReview)}
		if got := mergeForwardMessages(t, pool); !reflect.DeepEqual(got, want) {
			t.Fatalf("after the implementer's merge landed the messages are %+v, want %+v", got, want)
		}
	})
}

// The acceptance of cybermonklabs/legion#4979: an issue whose pull request conflicts with its base
// after the reviewer approved it reaches the merge through the merger, and the work never goes back
// to the implementer. Every check the base requires passed on the head, so in reviewing the
// conflict is recorded and the reviewer's approval ends the round; accepting and retro ask nothing
// of anyone, and the architect signs off and retro completes at that head as at any other. The
// move into merging starts the merger and asks it, once, to merge the base forward. Its merge
// landing tells it so and asks the reviewer to read the merge and approve the new head, and
// GitHub's merge of that head closes the issue. Across the whole run the implementer is sent no
// merge-forward message, is started for no phase but retro, and counts no move back.
func TestAConflictAfterTheReviewLandsThroughTheMerger(t *testing.T) {
	every := &record.BaseRules{Required: []string{"ci", "pr-title"}}
	pool := seedConflictIssue(t, phase.Reviewing, "needs_review", settledGreen("ci", "pr-title"))
	confirmsConflict(t, pool, every)
	apply := func(after time.Duration, fact intake.Fact) {
		t.Helper()
		applyRefusingNothing(t, pool, readsAfter(after), fact)
	}
	nothingAskedIn := func(at phase.Phase) {
		t.Helper()
		if got := issuePhase(t, pool); got != at {
			t.Fatalf("the issue is in %s, want %s", got, at)
		}
		if messages := mergeForwardMessages(t, pool); len(messages) != 0 {
			t.Fatalf("merge-forward messages %+v in %s, want none: the conflict waits for the merger", messages, at)
		}
		if told := capNotices(t, pool, "checks-red"); told != 0 {
			t.Fatalf("%d checks-red notices in %s, want none: nothing was asked", told, at)
		}
	}

	apply(2*time.Minute, intake.PullRequestReview{Repo: "sjawhar/legion", Number: 42, ID: 1, State: "approved", CommitID: conflictHead,
		HeadSHA: conflictHead, Author: "a-writer", AuthorCanWrite: true, Body: "ship it", SubmittedAt: firstConflictRead.Add(2 * time.Minute)})
	nothingAskedIn(phase.Accepting)
	// The architect's sign-off names the claim, the generation and the run of accepting its route
	// read, which the workflow judges again (rootsSignOff).
	apply(3*time.Minute, rootsSignOff(t, pool, readsAfter(3*time.Minute), "LEGION-208"))
	nothingAskedIn(phase.Retro)
	// A base push in retro reads the head again, and a read that finds it still conflicting asks
	// nothing either.
	apply(4*time.Minute, conflictingRead(every))
	nothingAskedIn(phase.Retro)
	apply(5*time.Minute, intake.HandoffComplete{Generation: 1, Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: "implement-claim",
		Summary: "retro done", Commit: conflictHead})
	if got := issuePhase(t, pool); got != phase.Merging {
		t.Fatalf("after retro's completion the issue is in %s, want merging", got)
	}
	if task := startTask(t, pool, claim.RoleMerger); task == "" {
		t.Fatal("the move into merging started no merger")
	}
	ask := record.MergeForwardMessage{Role: claim.RoleMerger, Step: record.MergeForwardAsk, Head: conflictHead, Base: "main",
		Phase: phase.Merging, PullRequest: 42, Why: record.ConflictMergeNext}
	if got := mergeForwardMessages(t, pool); !reflect.DeepEqual(got, []record.MergeForwardMessage{ask}) {
		t.Fatalf("merge-forward messages %+v after the move into merging, want the one ask to the merger %+v", got, ask)
	}
	if reason, stays := checksRedNotice(t, pool); stays != phase.Merging || !strings.Contains(reason, "the merger is asked to merge main forward") {
		t.Fatalf("the checks-red notice names %s with reason %q, want merging and the merger asked", stays, reason)
	}

	// The merger merges main forward and pushes, as the implement App.
	landsMergeForward(t, pool)
	landed := func(role claim.Role, step record.MergeForwardStep) record.MergeForwardMessage {
		return record.MergeForwardMessage{Role: role, Step: step, Head: conflictHead, Landed: forwardHead, Base: "main",
			Phase: phase.Merging, PullRequest: 42, Why: record.ConflictMergeNext}
	}
	want := []record.MergeForwardMessage{ask, landed(claim.RoleMerger, record.MergeForwardLanded), landed(claim.RoleReviewer, record.MergeForwardReview)}
	if got := mergeForwardMessages(t, pool); !reflect.DeepEqual(got, want) {
		t.Fatalf("merge-forward messages %+v after the merger's merge landed, want %+v", got, want)
	}
	if mf := recordedPR(t, pool, "LEGION-208").MergeForward; mf == nil || mf.Role != claim.RoleMerger || mf.Landed != forwardHead || !mf.ReviewAsked {
		t.Fatalf("the record's merge forward = %+v, want the merger's, landed at %s with the review asked", mf, forwardHead)
	}
	if got := issuePhase(t, pool); got != phase.Merging {
		t.Fatalf("after the merge landed the issue is in %s, want merging", got)
	}

	apply(6*time.Minute, intake.PullRequestMerged{Repo: "sjawhar/legion", Number: 42, MergeSHA: "merge"})
	if got := issuePhase(t, pool); got != phase.Done {
		t.Fatalf("after GitHub merged the pull request the issue is in %s, want done", got)
	}
	for _, message := range mergeForwardMessages(t, pool) {
		if message.Role == claim.RoleImplementer {
			t.Fatalf("merge-forward message %+v went to the implementer, want none: the conflict was the merger's", message)
		}
	}
	// A move into implementing writes in_progress (PhaseStatus), so a status write of it is the
	// trace of any phase handed back to the implementer.
	if statuses := queuedStatuses(t, pool); slices.Contains(statuses, "in_progress") || !slices.Contains(statuses, "retro") || !slices.Contains(statuses, "done") {
		t.Fatalf("the run wrote the statuses %q, want retro and done and never in_progress: no phase went back to the implementer", statuses)
	}
	if pass := implementerPass(t, pool); pass != 0 {
		t.Fatalf("the implementer counts %d moves back, want none", pass)
	}
}

// queuedStatuses is the status of every Dispatch status write the outbox holds for LEGION-208,
// oldest first.
func queuedStatuses(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(t.Context(), `select payload->>'status' from outbox where kind = 'dispatch_status' and issue = 'LEGION-208' order by id`)
	if err != nil {
		t.Fatalf("read the status writes: %v", err)
	}
	defer rows.Close()
	statuses := []string{}
	for rows.Next() {
		var status string
		if err := rows.Scan(&status); err != nil {
			t.Fatalf("read a status write: %v", err)
		}
		statuses = append(statuses, status)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the status writes: %v", err)
	}
	return statuses
}

// A confirmed conflict is acted on only when the issue's next step needs a head that merges
// (dispatch://INFRA-4374 R0). Where every check the base requires is green on the head and no rule
// of the base requires the head to be current, the conflict is recorded and nothing is sent: the
// tester and the reviewer go on at that head. The tester's next completion moves the issue as
// before, and the reviewer's, with no approval, is recorded while its round waits on a decision,
// as before. The reviewer's approval of that head ends its round whatever GitHub's merge state, and
// the issue's move into accepting asks nothing: the conflict waits for merging, where the merger
// merges the base forward (cybermonklabs/legion#4979, TestAConflictAfterTheReviewLandsThroughTheMerger).
// Any required check not green, a strict ruleset, and rules the daemon could not read each act at once.
// GitHub's refusal of rulesets to a plan that has none is no failed read: it reads as no rules, and
// so records only. A classic protection rule is read as not strict, so its contexts decide; and a
// required context is green by a success status the record keeps for the head as well as by a
// check run.
func TestAConflictIsActedOnOnlyWhenNeeded(t *testing.T) {
	greenStatus := settledGreen("lint")
	greenStatus.Statuses = []record.CommitStatus{{Context: "ci", State: record.CommitStatusSuccess,
		CreatedAt: firstConflictRead.Add(-time.Hour), Head: conflictHead}}
	for _, tc := range []struct {
		name   string
		at     phase.Phase
		status string
		pr     record.PullRequest
		rules  *record.BaseRules
		why    record.ConflictWhy
	}{
		{"every required check green in testing", phase.Testing, "testing", settledGreen("ci", "pr-title"),
			&record.BaseRules{Required: []string{"ci", "pr-title"}}, ""},
		{"every required check green in reviewing with no approval", phase.Reviewing, "needs_review", settledGreen("ci", "pr-title"),
			&record.BaseRules{Required: []string{"ci", "pr-title"}}, ""},
		{"no required check green", phase.Testing, "testing", settledGreen("lint"),
			&record.BaseRules{Required: []string{"ci", "pr-title"}}, record.ConflictCheckNotGreen},
		{"one of two required checks green", phase.Testing, "testing", settledGreen("ci"),
			&record.BaseRules{Required: []string{"ci", "pr-title"}}, record.ConflictCheckNotGreen},
		{"a strict ruleset with every check green in testing", phase.Testing, "testing", settledGreen("ci"),
			&record.BaseRules{Required: []string{"ci"}, Strict: true}, record.ConflictRequiresCurrent},
		{"a strict ruleset with every check green in reviewing with no approval", phase.Reviewing, "needs_review", settledGreen("ci"),
			&record.BaseRules{Required: []string{"ci"}, Strict: true}, record.ConflictRequiresCurrent},
		{"rules unread", phase.Testing, "testing", settledGreen("ci"),
			&record.BaseRules{Unread: "HTTP 502"}, record.ConflictRulesUnread},
		{"GitHub's plan refusal of rulesets", phase.Testing, "testing", settledGreen(),
			&record.BaseRules{}, ""},
		{"a classic rule's contexts all green", phase.Reviewing, "needs_review", settledGreen("ci", "lint"),
			&record.BaseRules{Required: []string{"ci", "lint"}}, ""},
		{"a required context green by a success status of the head", phase.Testing, "testing", greenStatus,
			&record.BaseRules{Required: []string{"ci"}}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := seedConflictIssue(t, tc.at, tc.status, tc.pr)
			confirmsConflict(t, pool, tc.rules)
			if held := recordedPR(t, pool, "LEGION-208").ConflictingHead; held != conflictHead {
				t.Fatalf("the record holds %q conflicting, want the confirmed conflict at head recorded", held)
			}
			if tc.why == "" {
				if asked := recordedPR(t, pool, "LEGION-208").MergeForward; asked != nil {
					t.Fatalf("the record's merge forward = %+v, want none asked", asked)
				}
				assertNothingActed(t, pool, tc.at)
				return
			}
			askedOnce(t, pool, tc.at, tc.status, tc.why, RoleFor(tc.at))
		})
	}

	every := &record.BaseRules{Required: []string{"ci", "pr-title"}}
	t.Run("every required check green in testing, then the tester's pass at that head", func(t *testing.T) {
		pool := seedConflictIssue(t, phase.Testing, "testing", settledGreen("ci", "pr-title"))
		confirmsConflict(t, pool, every)
		applyRefusingNothing(t, pool, readsAfter(2*time.Minute), intake.HandoffComplete{Generation: 1, Issue: "LEGION-208",
			Role: claim.RoleTester, Claim: "tester-claim", Summary: "tested", Verdict: "pass", Commit: conflictHead, HandoffRecord: "test-1", HandoffVersion: 1})
		if got := issuePhase(t, pool); got != phase.Reviewing {
			t.Fatalf("after the tester's pass the issue is in %s, want reviewing", got)
		}
		if messages := mergeForwardMessages(t, pool); len(messages) != 0 {
			t.Fatalf("merge-forward messages %+v, want none: reviewing needs no merge while every required check is green", messages)
		}
	})

	t.Run("every required check green in reviewing with no approval, then the reviewer's completion at that head", func(t *testing.T) {
		pool := seedConflictIssue(t, phase.Reviewing, "needs_review", settledGreen("ci", "pr-title"))
		confirmsConflict(t, pool, every)
		applyRefusingNothing(t, pool, readsAfter(2*time.Minute), intake.HandoffComplete{Generation: 1, Issue: "LEGION-208",
			Role: claim.RoleReviewer, Claim: "reviewer-claim", Summary: "reviewed", Commit: conflictHead, HandoffRecord: "review-1", HandoffVersion: 1})
		// No review decided the round, so the completion is recorded and the round waits on a
		// decision of the head, as it does at a head that merges.
		if got := issuePhase(t, pool); got != phase.Reviewing {
			t.Fatalf("after the reviewer's completion with no decision the issue is in %s, want reviewing", got)
		}
		if row := entryRow(t, pool, claim.RoleReviewer); !row.Completed() || row.HandoffCommit != conflictHead {
			t.Fatalf("the reviewer's row = %+v, want its completion at %s recorded", row, conflictHead)
		}
		if messages := mergeForwardMessages(t, pool); len(messages) != 0 {
			t.Fatalf("merge-forward messages %+v, want none: reviewing needs no merge while every required check is green", messages)
		}
	})

	t.Run("every required check green in reviewing, then the reviewer's approval of that head", func(t *testing.T) {
		pool := seedConflictIssue(t, phase.Reviewing, "needs_review", settledGreen("ci", "pr-title"))
		confirmsConflict(t, pool, every)
		assertNothingActed(t, pool, phase.Reviewing)
		applyRefusingNothing(t, pool, readsAfter(2*time.Minute), intake.PullRequestReview{Repo: "sjawhar/legion", Number: 42, ID: 1,
			State: "approved", CommitID: conflictHead, HeadSHA: conflictHead, Author: "a-writer", AuthorCanWrite: true, Body: "ship it",
			SubmittedAt: firstConflictRead.Add(2 * time.Minute)})
		if got := issuePhase(t, pool); got != phase.Accepting {
			t.Fatalf("after the approval the issue is in %s, want accepting: an approval of a conflicting head ends its round", got)
		}
		recordedNotAsked := func() {
			t.Helper()
			pr := recordedPR(t, pool, "LEGION-208")
			if pr.ConflictingHead != conflictHead || pr.MergeForward != nil {
				t.Fatalf("the record holds %q conflicting and the merge forward %+v, want the conflict recorded and nothing asked in accepting",
					pr.ConflictingHead, pr.MergeForward)
			}
			if messages := mergeForwardMessages(t, pool); len(messages) != 0 {
				t.Fatalf("merge-forward messages %+v, want none in accepting: the merger merges the base forward in merging", messages)
			}
			if told := capNotices(t, pool, "checks-red"); told != 0 {
				t.Fatalf("%d checks-red notices, want none: nothing was asked", told)
			}
		}
		recordedNotAsked()

		applyRefusingNothing(t, pool, readsAfter(3*time.Minute), conflictingRead(every))
		recordedNotAsked()
	})
}

// In implementing the work a conflict needs is where the issue already stands, so a confirmed
// conflict that needs a merge sends the implementer the one ask and nothing else: no hold, no
// notice to the architect, no worker started, and the implementer's own phase entry as it was, so
// its assignment and its completion stand (dispatch://INFRA-4374 R0). A second confirmed read of
// the same head asks nothing more. A strict ruleset needs the merge though every check is green;
// with every required check green and no strict rule nothing is sent. An ask in implementing
// covers implementing alone: the implementer's completion at that head without the merge moves
// the issue to testing, and testing asks again, with its hold and its notice.
func TestAConflictInImplementingIsToldOnlyWhenNeeded(t *testing.T) {
	for _, tc := range []struct {
		name  string
		pr    record.PullRequest
		rules *record.BaseRules
		why   record.ConflictWhy
	}{
		{"a required check not green", settledGreen("lint"), &record.BaseRules{Required: []string{"ci"}}, record.ConflictCheckNotGreen},
		{"a strict ruleset with every check green", settledGreen("ci"), &record.BaseRules{Required: []string{"ci"}, Strict: true}, record.ConflictRequiresCurrent},
		{"every required check green and no strict rule", settledGreen("ci"), &record.BaseRules{Required: []string{"ci"}}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := seedConflictIssue(t, phase.Implementing, "in_progress", tc.pr)
			entry := entryRow(t, pool, claim.RoleImplementer)
			confirmsConflict(t, pool, tc.rules)
			if tc.why == "" {
				assertNothingActed(t, pool, phase.Implementing)
			} else {
				askedOnce(t, pool, phase.Implementing, "in_progress", tc.why, "")
			}
			if after := entryRow(t, pool, claim.RoleImplementer); !reflect.DeepEqual(after, entry) {
				t.Fatalf("the implementer's phases row went from %+v to %+v, want it as it was: its assignment stands", entry, after)
			}

			applyRefusingNothing(t, pool, readsAfter(2*time.Minute), conflictingRead(tc.rules))
			if tc.why == "" {
				assertNothingActed(t, pool, phase.Implementing)
				return
			}
			askedOnce(t, pool, phase.Implementing, "in_progress", tc.why, "")
		})
	}

	t.Run("the implementer completes at that head without the merge", func(t *testing.T) {
		rules := &record.BaseRules{Required: []string{"ci"}}
		pool := seedConflictIssue(t, phase.Implementing, "in_progress", settledGreen("lint"))
		confirmsConflict(t, pool, rules)
		askedOnce(t, pool, phase.Implementing, "in_progress", record.ConflictCheckNotGreen, "")
		applyRefusingNothing(t, pool, readsAfter(2*time.Minute), intake.HandoffComplete{Generation: 1, Issue: "LEGION-208",
			Role: claim.RoleImplementer, Claim: "implement-claim", Summary: "implemented", Commit: conflictHead, HandoffRecord: "implement-1", HandoffVersion: 1})
		if got := issuePhase(t, pool); got != phase.Testing {
			t.Fatalf("after the implementer's completion the issue is in %s, want testing: its completion stands", got)
		}
		asked := func(role claim.Role, step record.MergeForwardStep, at phase.Phase) record.MergeForwardMessage {
			return record.MergeForwardMessage{Role: role, Step: step, Head: conflictHead, Base: "main", Phase: at, PullRequest: 42, Why: record.ConflictCheckNotGreen}
		}
		want := []record.MergeForwardMessage{asked(claim.RoleImplementer, record.MergeForwardAsk, phase.Implementing),
			asked(claim.RoleImplementer, record.MergeForwardAsk, phase.Testing), asked(claim.RoleTester, record.MergeForwardHold, phase.Testing)}
		if got := mergeForwardMessages(t, pool); !reflect.DeepEqual(got, want) {
			t.Fatalf("merge-forward messages %+v, want %+v: testing asks again what implementing asked", got, want)
		}
		if mf := recordedPR(t, pool, "LEGION-208").MergeForward; mf == nil || mf.Phase != phase.Testing || mf.Why != record.ConflictCheckNotGreen {
			t.Fatalf("the record's merge forward = %+v, want the one asked in testing", mf)
		}
		if told := capNotices(t, pool, "checks-red"); told != 1 {
			t.Fatalf("%d checks-red notices, want one", told)
		}
		if _, stays := checksRedNotice(t, pool); stays != phase.Testing {
			t.Fatalf("the checks-red notice names %s, want testing, the phase the issue stays in", stays)
		}
	})
}

// landsMergeForward is GitHub's synchronize of #42 from conflictHead to forwardHead: the merge
// forward landing, three minutes after the first conflicting read.
func landsMergeForward(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	applyRefusingNothing(t, pool, readsAfter(3*time.Minute), intake.PullRequestSynchronized{Repo: "sjawhar/legion", Number: 42,
		Branch: "legion/LEGION-208", Base: "main", HeadSHA: forwardHead, Before: conflictHead, Sender: "legion-implementer[bot]"})
}

// askedBeforeTheMerger is a pull request settled green on lint at conflictHead whose record holds
// the ask a daemon before cybermonklabs/legion#4979 made in at: of the implementer, naming no role, not
// landed. That daemon asked the implementer in accepting and retro too, and an ask it made stands
// across the upgrade until it lands, so the completion fence and the landing's review still hold
// for it, while this daemon makes no ask in either phase (TestAConflictPastTheReviewIsTheMergersToMergeForward).
func askedBeforeTheMerger(at phase.Phase) record.PullRequest {
	pr := settledGreen("lint")
	pr.MergeForward = &record.MergeForward{Head: conflictHead, Base: "main", Phase: at, Why: record.ConflictMergeNext, AskedAt: firstConflictRead}
	return pr
}

// readingEngine is the engine of these tests (mergingEngineAt, at the first conflicting read) with
// a reader of GitHub's merge state, which appends the issue of each read the engine asks for to
// asked once the fact's transaction commits (Config.ReadMergeState).
func readingEngine(asked *[]string) *Engine {
	engine := mergingEngineAt(firstConflictRead)
	engine.cfg.ReadMergeState = func(issue string) { *asked = append(*asked, issue) }
	return engine
}

// basePushOntoMain is a push onto main, the base #42 sits on.
var basePushOntoMain = intake.BasePush{Repo: "sjawhar/legion", Branch: "main", After: "base-2"}

// A merge forward keeps the issue in its phase, so the work a role did at the head the merge
// replaced is work on code the pull request no longer stands on (dispatch://INFRA-4374 R1). Once
// the merge lands, a completion of testing, reviewing or retro at the replaced head is refused
// HANDOFF_HEAD_MERGED_FORWARD, naming the new head, and records and moves nothing; so is one that
// names no head, which says nothing about which head its work judged. The completion at the new
// head moves the issue as it would have: the tester's pass to reviewing, the reviewer's as its
// round decides, retro to merging. The merger's completion moves nothing in merging, as ever, and
// in accepting nothing moves at all. Retro and accepting ask nothing now, so their rows land an
// ask a daemon before cybermonklabs/legion#4979 made there (askedBeforeTheMerger).
func TestAConflictKeepsPhaseAndRefusesACompletionAtTheReplacedHead(t *testing.T) {
	requiresCI := &record.BaseRules{Required: []string{"ci"}}
	for _, tc := range []struct {
		at       phase.Phase
		status   string
		role     claim.Role
		complete func(commit, handoff string) intake.HandoffComplete
		// moved checks what the completion at the new head did.
		moved func(t *testing.T, pool *pgxpool.Pool)
	}{
		{phase.Testing, "testing", claim.RoleTester, func(commit, handoff string) intake.HandoffComplete {
			return intake.HandoffComplete{Generation: 1, Issue: "LEGION-208", Role: claim.RoleTester, Claim: "tester-claim",
				Summary: "tested", Verdict: "pass", Commit: commit, HandoffRecord: handoff, HandoffVersion: 1}
		}, func(t *testing.T, pool *pgxpool.Pool) {
			if got := issuePhase(t, pool); got != phase.Reviewing {
				t.Fatalf("after the tester's pass at the new head the issue is in %s, want reviewing", got)
			}
		}},
		{phase.Reviewing, "needs_review", claim.RoleReviewer, func(commit, handoff string) intake.HandoffComplete {
			return intake.HandoffComplete{Generation: 1, Issue: "LEGION-208", Role: claim.RoleReviewer, Claim: "reviewer-claim",
				Summary: "reviewed", Commit: commit, HandoffRecord: handoff, HandoffVersion: 1}
		}, func(t *testing.T, pool *pgxpool.Pool) {
			// No review decided the round, so the completion is recorded and the round waits on a
			// decision of the new head.
			if got := issuePhase(t, pool); got != phase.Reviewing {
				t.Fatalf("after the reviewer's completion with no decision the issue is in %s, want reviewing", got)
			}
			if row := entryRow(t, pool, claim.RoleReviewer); !row.Completed() || row.HandoffCommit != forwardHead {
				t.Fatalf("the reviewer's row = %+v, want its completion at %s recorded", row, forwardHead)
			}
		}},
		{phase.Retro, "retro", claim.RoleImplementer, func(commit, _ string) intake.HandoffComplete {
			return intake.HandoffComplete{Generation: 1, Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: "implement-claim",
				Summary: "retro done", Commit: commit}
		}, func(t *testing.T, pool *pgxpool.Pool) {
			if got := issuePhase(t, pool); got != phase.Merging {
				t.Fatalf("after retro's completion at the new head the issue is in %s, want merging", got)
			}
		}},
	} {
		t.Run(string(tc.at), func(t *testing.T) {
			pr := settledGreen("lint")
			if tc.at == phase.Retro {
				pr = askedBeforeTheMerger(phase.Retro)
			}
			pool := seedConflictIssue(t, tc.at, tc.status, pr)
			confirmsConflict(t, pool, requiresCI)
			landsMergeForward(t, pool)
			if asked := recordedPR(t, pool, "LEGION-208").MergeForward; asked == nil || asked.Head != conflictHead || asked.Landed != forwardHead {
				t.Fatalf("the record's merge forward = %+v, want head landed as %s", asked, forwardHead)
			}
			row, rows := entryRow(t, pool, tc.role), len(queuedRows(t, pool))
			refusedAt := func(id, commit string, names ...string) {
				t.Helper()
				result := applyFacts(t, pool, readsAfter(4*time.Minute))(id, tc.complete(commit, "handoff-1"))
				if code := refusalCode(result); code != "HANDOFF_HEAD_MERGED_FORWARD" {
					t.Fatalf("the completion %s was answered %q (%+v), want HANDOFF_HEAD_MERGED_FORWARD", id, code, result.Refusal)
				}
				for _, head := range names {
					if msg := result.Refusal.Message; !strings.Contains(msg, head) {
						t.Fatalf("the refusal of the completion %s reads %q, want it to name %s", id, msg, head)
					}
				}
				if after := entryRow(t, pool, tc.role); !reflect.DeepEqual(after, row) {
					t.Fatalf("the completion %s took the %s's row from %+v to %+v, want nothing recorded", id, tc.role, row, after)
				}
				if after := len(queuedRows(t, pool)); after != rows {
					t.Fatalf("the completion %s took the outbox from %d rows to %d, want nothing queued", id, rows, after)
				}
				if got := issuePhase(t, pool); got != tc.at {
					t.Fatalf("after the refused completion %s the issue is in %s, want %s", id, got, tc.at)
				}
			}
			refusedAt("at the replaced head", conflictHead, conflictHead, forwardHead)
			refusedAt("naming no head", "", forwardHead)

			applyRefusingNothing(t, pool, readsAfter(5*time.Minute), tc.complete(forwardHead, "handoff-2"))
			tc.moved(t, pool)
		})
	}

	t.Run("merging", func(t *testing.T) {
		pool := seedConflictIssue(t, phase.Merging, "retro", settledGreen("lint"))
		confirmsConflict(t, pool, requiresCI)
		landsMergeForward(t, pool)
		result := applyFacts(t, pool, readsAfter(4*time.Minute))("the merger's completion", intake.HandoffComplete{Generation: 1,
			Issue: "LEGION-208", Role: claim.RoleMerger, Claim: "merger-claim", Summary: "merged", Commit: conflictHead})
		if result.Refusal != nil {
			t.Fatalf("the merger's completion was refused %+v, want it recorded: the fence is of testing, reviewing and retro", result.Refusal)
		}
		if got := issuePhase(t, pool); got != phase.Merging {
			t.Fatalf("after the merger's completion the issue is in %s, want merging", got)
		}
	})

	t.Run("accepting", func(t *testing.T) {
		pool := seedConflictIssue(t, phase.Accepting, "needs_review", askedBeforeTheMerger(phase.Accepting))
		confirmsConflict(t, pool, requiresCI)
		landsMergeForward(t, pool)
		if got := issuePhase(t, pool); got != phase.Accepting {
			t.Fatalf("after the merge landed the issue is in %s, want accepting", got)
		}
		assertOutboxCount(t, pool, "supervise", 0)
	})
}

// The merge forward's completion fence is of the phase the issue stands in (dispatch://INFRA-4374
// R1). The reviewer's approval moves the issue on past reviewing before the reviewer completes,
// and in merging a conflict asks the merger for the merge forward (cybermonklabs/legion#4979), which
// lands; the review the landing then asks of the reviewer is made with no completion
// (record.MergeForwardReview). The reviewer's completion of reviewing that arrives after it, at the
// head the merge replaced, is the report of the review it made of that head, which the next phase
// reads: it is recorded on the reviewer's own row and moves nothing (engine's leftItsPhase), and
// the issue stays where it stood. An ask a daemon before cybermonklabs/legion#4979 made in accepting
// (askedBeforeTheMerger) lands the same way and is held to the same fence.
func TestAConflictKeepsPhaseAndRecordsALateCompletionOfALeftPhase(t *testing.T) {
	for _, tc := range []struct {
		at     phase.Phase
		status string
		pr     record.PullRequest
	}{
		{phase.Merging, "retro", settledGreen("lint")},
		{phase.Accepting, "needs_review", askedBeforeTheMerger(phase.Accepting)},
	} {
		t.Run(string(tc.at), func(t *testing.T) {
			pool := seedConflictIssue(t, tc.at, tc.status, tc.pr)
			// The reviewer's row as its approval left it: entered, with no completion of its own,
			// under a claim the daemon started on the issue (record.Store.RoleRun), which
			// leftItsPhase asks for.
			seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleReviewer, Entry: 1})
			seedClaimSession(t, pool, "reviewer-claim", "ses-reviewer")
			confirmsConflict(t, pool, &record.BaseRules{Required: []string{"ci"}})
			landsMergeForward(t, pool)
			if asked := recordedPR(t, pool, "LEGION-208").MergeForward; asked == nil || asked.Head != conflictHead || asked.Landed != forwardHead || !asked.ReviewAsked {
				t.Fatalf("the record's merge forward = %+v, want head landed as %s with the review asked", asked, forwardHead)
			}

			result := applyFacts(t, pool, readsAfter(4*time.Minute))("the reviewer's late completion", intake.HandoffComplete{Generation: 1,
				Issue: "LEGION-208", Role: claim.RoleReviewer, Claim: "reviewer-claim", Summary: "reviewed", Commit: conflictHead,
				HandoffRecord: "review-1", HandoffVersion: 1})
			if result.Refusal != nil {
				t.Fatalf("the reviewer's late completion of reviewing at %s was refused %+v, want it recorded: the fence is of the phase the issue stands in", conflictHead, result.Refusal)
			}
			if row := entryRow(t, pool, claim.RoleReviewer); !row.Completed() || row.HandoffCommit != conflictHead || row.HandoffRecord != "review-1" {
				t.Fatalf("the reviewer's row = %+v, want its completion at %s naming review-1 recorded", row, conflictHead)
			}
			if got := issuePhase(t, pool); got != tc.at {
				t.Fatalf("after the reviewer's late completion the issue is in %s, want %s", got, tc.at)
			}
		})
	}
}

// Once a merge forward lands on an issue past its review, the approval the issue stands on is of
// the head the merge replaced, so the reviewer is asked to read the merge and approve the new head
// or request changes (dispatch://INFRA-4374 R1), one review message naming both heads, and the
// record keeps that the review was asked for. In merging the merger made the merge
// (cybermonklabs/legion#4979), and as the worker of merging it is told the merge landed, which holds
// its merge to the reviewer's approval of the new head. An ask a daemon before that change made in
// accepting or retro lands the same way (askedBeforeTheMerger): the architect in accepting and the
// implementer in retro are told nothing, since the one asked for the merge and the other made it.
// A merger that enters merging while that review stands for the head is told to merge the head only
// once the reviewer's approval of it is on the pull request.
func TestAConflictKeepsPhaseAndAsksTheReviewerToReadTheMerge(t *testing.T) {
	requiresCI := &record.BaseRules{Required: []string{"ci"}}
	landing := func(role claim.Role, step record.MergeForwardStep, at phase.Phase) record.MergeForwardMessage {
		return record.MergeForwardMessage{Role: role, Step: step, Head: conflictHead, Landed: forwardHead, Base: "main", Phase: at,
			PullRequest: 42, Why: record.ConflictMergeNext}
	}
	for _, tc := range []struct {
		at     phase.Phase
		status string
		worker claim.Role
	}{
		{phase.Accepting, "needs_review", ""},
		{phase.Retro, "retro", ""},
		{phase.Merging, "retro", claim.RoleMerger},
	} {
		t.Run(string(tc.at), func(t *testing.T) {
			pr := settledGreen("lint")
			if tc.at != phase.Merging {
				pr = askedBeforeTheMerger(tc.at)
			}
			pool := seedConflictIssue(t, tc.at, tc.status, pr)
			confirmsConflict(t, pool, requiresCI)
			asked := len(mergeForwardMessages(t, pool))
			landsMergeForward(t, pool)

			want := []record.MergeForwardMessage{}
			if tc.worker != "" {
				want = append(want, landing(tc.worker, record.MergeForwardLanded, tc.at))
			}
			want = append(want, landing(claim.RoleReviewer, record.MergeForwardReview, tc.at))
			if got := mergeForwardMessages(t, pool)[asked:]; !reflect.DeepEqual(got, want) {
				t.Fatalf("the landing queued %+v, want %+v", got, want)
			}
			if mf := recordedPR(t, pool, "LEGION-208").MergeForward; mf == nil || mf.Landed != forwardHead || !mf.ReviewAsked {
				t.Fatalf("the record's merge forward = %+v, want it landed at %s with the review asked", mf, forwardHead)
			}
			if got := issuePhase(t, pool); got != tc.at {
				t.Fatalf("after the merge landed the issue is in %s, want %s", got, tc.at)
			}

			// A later head is no landing of its own: the review asked stands.
			applyRefusingNothing(t, pool, readsAfter(4*time.Minute), intake.PullRequestSynchronized{Repo: "sjawhar/legion", Number: 42,
				Branch: "legion/LEGION-208", Base: "main", HeadSHA: headThree, Before: forwardHead, Sender: "legion-implementer[bot]"})
			if got := mergeForwardMessages(t, pool)[asked:]; !reflect.DeepEqual(got, want) {
				t.Fatalf("after a later head the landing messages are %+v, want still %+v", got, want)
			}
		})
	}

	t.Run("a merger entering merging while the review is owed", func(t *testing.T) {
		pool := seedConflictIssue(t, phase.Retro, "retro", askedBeforeTheMerger(phase.Retro))
		confirmsConflict(t, pool, requiresCI)
		landsMergeForward(t, pool)
		applyRefusingNothing(t, pool, readsAfter(4*time.Minute), intake.HandoffComplete{Generation: 1, Issue: "LEGION-208",
			Role: claim.RoleImplementer, Claim: "implement-claim", Summary: "retro done", Commit: forwardHead})
		if got := issuePhase(t, pool); got != phase.Merging {
			t.Fatalf("after retro's completion the issue is in %s, want merging", got)
		}
		if task := startTask(t, pool, claim.RoleMerger); !strings.Contains(task, "merge head "+forwardHead+" only once the reviewer's approval of it is on the pull request") {
			t.Fatalf("the merger's task reads %q, want it to hold the merge of %s to the reviewer's approval", task, forwardHead)
		}
	})
}

// A push onto the base can make a head conflict with it, and GitHub sends no event of the pull
// request while it computes that (dispatch://INFRA-4374 R0, R4). So a base push reads the merge
// state of each open pull request on the base whose issue stands in testing through merging, an
// issue past its review among them, where the merge is next; an issue in implementing is read
// for at its next head push instead, and a base push reads nothing for it.
func TestAConflictKeepsPhaseWhenABasePushReadsAnIssuePastReview(t *testing.T) {
	for _, tc := range []struct {
		at     phase.Phase
		status string
		want   []string
	}{
		{phase.Testing, "testing", []string{"LEGION-208"}},
		{phase.Reviewing, "needs_review", []string{"LEGION-208"}},
		{phase.Accepting, "needs_review", []string{"LEGION-208"}},
		{phase.Implementing, "in_progress", []string{}},
	} {
		t.Run(string(tc.at), func(t *testing.T) {
			pool := seedConflictIssue(t, tc.at, tc.status, settledGreen("ci"))
			asked := []string{}
			applyRefusingNothing(t, pool, readingEngine(&asked), basePushOntoMain)
			if !reflect.DeepEqual(asked, tc.want) {
				t.Fatalf("the base push asked reads of %v in %s, want %v", asked, tc.at, tc.want)
			}
			if moved := recordedPR(t, pool, "LEGION-208").BaseMoved; moved != "base-2" {
				t.Fatalf("the record holds the base moved to %q, want base-2", moved)
			}
		})
	}
}

// GitHub runs no checks on a head that conflicts with its base, so a head pushed in implementing
// is read for a conflict at once (dispatch://INFRA-4374 R0): without the read, a conflicting push
// stands with no CI and nobody is told. The synchronize that carries the pushed head is the fact
// that asks: the push of the same commits, delivered before it, moves no head of the record and
// asks nothing, so one push to the branch asks for one ladder. Before implementing there is no
// pull request work to merge forward, and a head push reads nothing.
func TestAConflictKeepsPhaseWhenAHeadPushInImplementingAsksARead(t *testing.T) {
	for _, tc := range []struct {
		at     phase.Phase
		status string
		want   []string
	}{
		{phase.Implementing, "in_progress", []string{"LEGION-208"}},
		{phase.Planning, "in_progress", []string{}},
	} {
		t.Run(string(tc.at), func(t *testing.T) {
			pool := seedConflictIssue(t, tc.at, tc.status, settledGreen("ci"))
			asked := []string{}
			engine := readingEngine(&asked)
			applyRefusingNothing(t, pool, engine, intake.Push{Repo: "sjawhar/legion", Branch: "legion/LEGION-208",
				Before: conflictHead, After: headTwo, Pusher: "legion-implementer[bot]"})
			if len(asked) != 0 {
				t.Fatalf("the push to the branch asked reads of %v in %s, want none: its synchronize is what moves the head", asked, tc.at)
			}
			applyRefusingNothing(t, pool, engine, intake.PullRequestSynchronized{Repo: "sjawhar/legion", Number: 42,
				Branch: "legion/LEGION-208", Base: "main", HeadSHA: headTwo, Before: conflictHead, Sender: "legion-implementer[bot]"})
			if !reflect.DeepEqual(asked, tc.want) {
				t.Fatalf("the head push asked reads of %v in %s, want %v", asked, tc.at, tc.want)
			}
		})
	}
}

// A base push can leave the record reading a merge state GitHub no longer answers: GitHub
// recomputes whether the head still merges into the moved base and sends no event of the pull
// request while it does, so a record read `blocked` stood while GitHub answered DIRTY
// (INFRA-4315). So a base push reads the merge state of an issue in reviewing though no round
// waits on it - no review has approved the head (dispatch://INFRA-4374 R4) - and the record then
// holds what GitHub answered for the head. An issue in implementing is read for at its next head
// push, and the same base push reads nothing for it.
func TestTheMergeStateFollowsGitHubWhenABasePushReadsAnIssueInReview(t *testing.T) {
	blocked := settledGreen("ci")
	blocked.Mergeability, blocked.MergeState = record.MergeabilityMergeable, record.MergeStateBlocked

	t.Run("reviewing with no approval", func(t *testing.T) {
		pool := seedConflictIssue(t, phase.Reviewing, "needs_review", blocked)
		asked := []string{}
		applyRefusingNothing(t, pool, readingEngine(&asked), basePushOntoMain)
		if !reflect.DeepEqual(asked, []string{"LEGION-208"}) {
			t.Fatalf("the base push asked reads of %v, want one of LEGION-208", asked)
		}

		applyRefusingNothing(t, pool, readsAfter(0), conflictingRead(&record.BaseRules{Required: []string{"ci"}}))
		pr := recordedPR(t, pool, "LEGION-208")
		if pr.Mergeability != record.MergeabilityConflicting || pr.MergeState != record.MergeStateDirty || pr.BaseMoved != "" {
			t.Fatalf("after the read the record holds %s and %s with the base moved to %q, want GitHub's CONFLICTING and dirty, the push answered",
				pr.Mergeability, pr.MergeState, pr.BaseMoved)
		}
		if pr.ConflictRead == nil || pr.ConflictRead.Head != conflictHead {
			t.Fatalf("the first conflicting read = %+v, want one of head", pr.ConflictRead)
		}
		assertNothingActed(t, pool, phase.Reviewing)
	})

	t.Run("implementing", func(t *testing.T) {
		pool := seedConflictIssue(t, phase.Implementing, "in_progress", blocked)
		asked := []string{}
		applyRefusingNothing(t, pool, readingEngine(&asked), basePushOntoMain)
		if len(asked) != 0 {
			t.Fatalf("the base push asked reads of %v in implementing, want none", asked)
		}
	})
}

// A read GitHub answered before the first CONFLICTING read of a head is no second read of it
// (dispatch://INFRA-4374 R2: the second read is made at least a minute after the first), however
// late it is applied. The per-issue read dates its answer before it follows the head, which waits
// on the fact lock (daemon's readIssuePullRequest), so a MERGEABLE answer it had before a push to
// the base can be applied after the ladder's CONFLICTING answer made since. That older answer
// must leave the first read standing: released, the second read's row finds no first read and
// finishes without a read (daemon's conflictReadLeft), and the conflict GitHub still answers is
// read again only at the issue's next fact. The second read a minute after the first acts.
func TestAConflictSecondReadIsNeverOneMadeBeforeTheFirst(t *testing.T) {
	unread := &record.BaseRules{Unread: "HTTP 502"}
	pool := seedConflictIssue(t, phase.Testing, "testing", record.PullRequest{})
	first := conflictingRead(unread)
	first.ReadAt = firstConflictRead
	applyRefusingNothing(t, pool, readsAfter(0), first)
	older := intake.PullRequestMergeability{Repo: "sjawhar/legion", Number: 42, Base: "main", HeadSHA: conflictHead,
		Mergeable: record.MergeabilityMergeable, MergeState: record.MergeStateClean, ReadAt: firstConflictRead.Add(-5 * time.Second)}
	applyRefusingNothing(t, pool, readsAfter(2*time.Second), older)
	if got := recordedPR(t, pool, "LEGION-208").ConflictRead; got == nil || got.Head != conflictHead || !got.At.Equal(firstConflictRead) {
		t.Fatalf("after a MERGEABLE read made 5s before the first CONFLICTING read and applied after it, the first read = %+v, want the first read of %s at %s still held: a read older than the first is no second read",
			got, conflictHead, firstConflictRead)
	}
	second := conflictingRead(unread)
	second.ReadAt = firstConflictRead.Add(time.Minute)
	applyRefusingNothing(t, pool, readsAfter(time.Minute), second)
	askedOnce(t, pool, phase.Testing, "testing", record.ConflictRulesUnread, claim.RoleTester)
}

// A completion made for a head a merge forward replaced does not advance the issue
// (dispatch://INFRA-4374 R1), whichever merge forward replaced it. The base can move again while
// the issue stands in testing, so the head the first merge forward left can conflict in its turn
// and be merged forward again: the tester's completion at the head the first merge replaced is
// refused HANDOFF_HEAD_MERGED_FORWARD while the second merge forward is asked and after it lands,
// and records and moves nothing.
func TestAConflictKeepsPhaseAndRefusesACompletionAtAHeadAnEarlierMergeReplaced(t *testing.T) {
	requiresCI := &record.BaseRules{Required: []string{"ci"}}
	secondMergeForward := func(t *testing.T) *pgxpool.Pool {
		t.Helper()
		pool := seedConflictIssue(t, phase.Testing, "testing", settledGreen("lint"))
		confirmsConflict(t, pool, requiresCI)
		landsMergeForward(t, pool)
		again := conflictingRead(requiresCI)
		again.HeadSHA = forwardHead
		applyRefusingNothing(t, pool, readsAfter(4*time.Minute), again)
		applyRefusingNothing(t, pool, readsAfter(5*time.Minute), again)
		if asked := recordedPR(t, pool, "LEGION-208").MergeForward; asked == nil || asked.Head != forwardHead {
			t.Fatalf("the record's merge forward = %+v, want the second ask, of %s", asked, forwardHead)
		}
		return pool
	}
	refusedAtTheFirstReplacedHead := func(t *testing.T, pool *pgxpool.Pool) {
		t.Helper()
		row := entryRow(t, pool, claim.RoleTester)
		result := applyFacts(t, pool, readsAfter(7*time.Minute))("at the head the first merge replaced", intake.HandoffComplete{Generation: 1,
			Issue: "LEGION-208", Role: claim.RoleTester, Claim: "tester-claim", Summary: "tested", Verdict: "pass",
			Commit: conflictHead, HandoffRecord: "handoff-1", HandoffVersion: 1})
		if got := issuePhase(t, pool); got != phase.Testing {
			t.Fatalf("the tester's pass at %s, the head the first merge forward replaced, moved the issue to %s (answered %q), want it refused and the issue still in testing",
				conflictHead, got, refusalCode(result))
		}
		if code := refusalCode(result); code != "HANDOFF_HEAD_MERGED_FORWARD" {
			t.Fatalf("the completion at %s was answered %q, want HANDOFF_HEAD_MERGED_FORWARD", conflictHead, code)
		}
		if after := entryRow(t, pool, claim.RoleTester); !reflect.DeepEqual(after, row) {
			t.Fatalf("the tester's row went from %+v to %+v, want nothing recorded", row, after)
		}
	}

	t.Run("while the second merge forward is asked", func(t *testing.T) {
		refusedAtTheFirstReplacedHead(t, secondMergeForward(t))
	})

	t.Run("after the second merge forward lands", func(t *testing.T) {
		pool := secondMergeForward(t)
		applyRefusingNothing(t, pool, readsAfter(6*time.Minute), intake.PullRequestSynchronized{Repo: "sjawhar/legion", Number: 42,
			Branch: "legion/LEGION-208", Base: "main", HeadSHA: headThree, Before: forwardHead, Sender: "legion-implementer[bot]"})
		if asked := recordedPR(t, pool, "LEGION-208").MergeForward; asked == nil || asked.Landed != headThree {
			t.Fatalf("the record's merge forward = %+v, want the second ask landed as %s", asked, headThree)
		}
		refusedAtTheFirstReplacedHead(t, pool)
	})
}

// A completion that names no head says nothing about which head its work judged, so once a merge
// forward landed it is refused (dispatch://INFRA-4374 R1). When the head that merge left conflicts
// in its turn and a second merge forward is asked of it, the hold of that ask tells the tester not
// to complete at the head the pull request stands at. So the tester's completion naming no head
// then is refused HANDOFF_HEAD_MERGED_FORWARD with an answer that names that head, says its merge
// forward is asked and has not landed, and tells the tester to wait for the daemon's message that
// it landed, never to do its work at that head; and it records and moves nothing. The answer
// reaches the merge forward that landed through the record's earlier ones
// (record.MergeForward.Earlier), since the ask the record holds has not landed.
func TestAConflictKeepsPhaseAndRefusesACompletionNamingNoHeadWhileASecondMergeForwardIsAsked(t *testing.T) {
	requiresCI := &record.BaseRules{Required: []string{"ci"}}
	pool := seedConflictIssue(t, phase.Testing, "testing", settledGreen("lint"))
	confirmsConflict(t, pool, requiresCI)
	landsMergeForward(t, pool)
	again := conflictingRead(requiresCI)
	again.HeadSHA = forwardHead
	applyRefusingNothing(t, pool, readsAfter(4*time.Minute), again)
	applyRefusingNothing(t, pool, readsAfter(5*time.Minute), again)
	if asked := recordedPR(t, pool, "LEGION-208").MergeForward; asked == nil || asked.Head != forwardHead || asked.Landed != "" ||
		!reflect.DeepEqual(asked.Earlier, []record.LandedMergeForward{{Head: conflictHead, Landed: forwardHead}}) {
		t.Fatalf("the record's merge forward = %+v, want the second ask, of %s and not landed, after the first, of %s, landed as %s",
			asked, forwardHead, conflictHead, forwardHead)
	}

	row, rows := entryRow(t, pool, claim.RoleTester), len(queuedRows(t, pool))
	result := applyFacts(t, pool, readsAfter(6*time.Minute))("naming no head", intake.HandoffComplete{Generation: 1,
		Issue: "LEGION-208", Role: claim.RoleTester, Claim: "tester-claim", Summary: "tested", Verdict: "pass",
		HandoffRecord: "handoff-1", HandoffVersion: 1})
	if code := refusalCode(result); code != "HANDOFF_HEAD_MERGED_FORWARD" {
		t.Fatalf("the tester's completion naming no head was answered %q (%+v), want HANDOFF_HEAD_MERGED_FORWARD", code, result.Refusal)
	}
	msg := result.Refusal.Message
	for _, says := range []string{"onto head " + forwardHead + " is asked and has not landed", "Wait for the daemon's message that this merge landed"} {
		if !strings.Contains(msg, says) {
			t.Fatalf("the refusal reads %q, want it to say %q", msg, says)
		}
	}
	if says := "do the phase's work at head " + forwardHead; strings.Contains(msg, says) {
		t.Fatalf("the refusal reads %q, want it not to say %q: the hold of the merge forward asked of %s tells the tester not to complete at it", msg, says, forwardHead)
	}
	if after := entryRow(t, pool, claim.RoleTester); !reflect.DeepEqual(after, row) {
		t.Fatalf("the completion took the tester's row from %+v to %+v, want nothing recorded", row, after)
	}
	if after := len(queuedRows(t, pool)); after != rows {
		t.Fatalf("the completion took the outbox from %d rows to %d, want nothing queued", rows, after)
	}
	if got := issuePhase(t, pool); got != phase.Testing {
		t.Fatalf("after the refused completion the issue is in %s, want testing", got)
	}
}

// The merge forward's fence refuses a completion that names no head only once a merge forward of
// the record's stretch of work landed (dispatch://INFRA-4374 R1): before that, no merge forward
// replaced any head, so the head such a completion judged is no head a merge replaced. A tester's
// pass naming no head while the merge forward is asked and nothing landed passes the fence: it is
// recorded on the tester's row with no commit, and it moves the issue to reviewing, as a pass does.
func TestAConflictKeepsPhaseAndTakesACompletionNamingNoHeadWhileNoMergeForwardLanded(t *testing.T) {
	pool := seedConflictIssue(t, phase.Testing, "testing", settledGreen("lint"))
	confirmsConflict(t, pool, &record.BaseRules{Required: []string{"ci"}})
	if asked := recordedPR(t, pool, "LEGION-208").MergeForward; asked == nil || asked.Head != conflictHead || asked.Landed != "" || len(asked.Earlier) != 0 {
		t.Fatalf("the record's merge forward = %+v, want one ask, of %s, with nothing landed", asked, conflictHead)
	}

	result := applyFacts(t, pool, readsAfter(4*time.Minute))("naming no head", intake.HandoffComplete{Generation: 1,
		Issue: "LEGION-208", Role: claim.RoleTester, Claim: "tester-claim", Summary: "tested", Verdict: "pass",
		HandoffRecord: "handoff-1", HandoffVersion: 1})
	if result.Refusal != nil {
		t.Fatalf("the tester's completion naming no head was refused %+v, want it recorded", result.Refusal)
	}
	if row := entryRow(t, pool, claim.RoleTester); !row.Completed() || row.HandoffCommit != "" || row.Verdict != "pass" || row.HandoffRecord != "handoff-1" {
		t.Fatalf("the tester's row = %+v, want its pass naming no commit and handoff-1 recorded", row)
	}
	if got := issuePhase(t, pool); got != phase.Reviewing {
		t.Fatalf("after the tester's pass naming no head the issue is in %s, want reviewing", got)
	}
}

// A completion at a head an earlier merge forward replaced is refused (dispatch://INFRA-4374 R1),
// and the refusal tells the role where to do its work. When the head that merge left conflicts in
// its turn and a second merge forward is asked of it, the hold of that ask tells the tester not to
// complete at the head the pull request stands at. So the refusal of the tester's completion at
// the head the first merge replaced must not send it to that head either: as the refusal of a
// completion naming no head does then, it says the second merge forward is asked and has not
// landed and tells the tester to wait for the daemon's message that it landed. It records and
// moves nothing.
func TestAConflictKeepsPhaseAndRefusesACompletionAtAReplacedHeadWhileASecondMergeForwardIsAsked(t *testing.T) {
	requiresCI := &record.BaseRules{Required: []string{"ci"}}
	pool := seedConflictIssue(t, phase.Testing, "testing", settledGreen("lint"))
	confirmsConflict(t, pool, requiresCI)
	landsMergeForward(t, pool)
	again := conflictingRead(requiresCI)
	again.HeadSHA = forwardHead
	applyRefusingNothing(t, pool, readsAfter(4*time.Minute), again)
	applyRefusingNothing(t, pool, readsAfter(5*time.Minute), again)
	if asked := recordedPR(t, pool, "LEGION-208").MergeForward; asked == nil || asked.Head != forwardHead || asked.Landed != "" {
		t.Fatalf("the record's merge forward = %+v, want the second ask, of %s and not landed", asked, forwardHead)
	}

	row, rows := entryRow(t, pool, claim.RoleTester), len(queuedRows(t, pool))
	result := applyFacts(t, pool, readsAfter(6*time.Minute))("at the head the first merge replaced", intake.HandoffComplete{Generation: 1,
		Issue: "LEGION-208", Role: claim.RoleTester, Claim: "tester-claim", Summary: "tested", Verdict: "pass",
		Commit: conflictHead, HandoffRecord: "handoff-1", HandoffVersion: 1})
	if code := refusalCode(result); code != "HANDOFF_HEAD_MERGED_FORWARD" {
		t.Fatalf("the tester's completion at %s was answered %q (%+v), want HANDOFF_HEAD_MERGED_FORWARD", conflictHead, code, result.Refusal)
	}
	msg := result.Refusal.Message
	if says := "do the phase's work at head " + forwardHead; strings.Contains(msg, says) {
		t.Fatalf("the refusal reads %q, want it not to say %q: the hold of the merge forward asked of %s tells the tester not to complete at it", msg, says, forwardHead)
	}
	for _, says := range []string{"onto head " + forwardHead + " is asked and has not landed", "Wait for the daemon's message that this merge landed"} {
		if !strings.Contains(msg, says) {
			t.Fatalf("the refusal reads %q, want it to say %q", msg, says)
		}
	}
	if after := entryRow(t, pool, claim.RoleTester); !reflect.DeepEqual(after, row) {
		t.Fatalf("the completion took the tester's row from %+v to %+v, want nothing recorded", row, after)
	}
	if after := len(queuedRows(t, pool)); after != rows {
		t.Fatalf("the completion took the outbox from %d rows to %d, want nothing queued", rows, after)
	}
	if got := issuePhase(t, pool); got != phase.Testing {
		t.Fatalf("after the refused completion the issue is in %s, want testing", got)
	}
}

// A completion at a head an earlier merge forward replaced is refused (dispatch://INFRA-4374 R1),
// and the refusal tells the role where to do its work. In retro the role that completes is the
// implementer, which no hold and no landed message reaches (record.MergeForwardReaches), so the
// refusal sends it to do the phase's work at the head the pull request stands at, never to wait for
// a message it is not sent, and it records and moves nothing. When the head an ask made before
// cybermonklabs/legion#4979 left in retro (askedBeforeTheMerger) conflicts in its turn, retro asks
// nothing of anyone: retro's completion at that head moves the issue to merging, where the merger
// is asked to merge the base forward onto it, and the implementer is asked nothing.
func TestAConflictKeepsPhaseSendsARetroCompletionAtAReplacedHeadToTheCurrentHead(t *testing.T) {
	requiresCI := &record.BaseRules{Required: []string{"ci"}}
	pool := seedConflictIssue(t, phase.Retro, "retro", askedBeforeTheMerger(phase.Retro))
	confirmsConflict(t, pool, requiresCI)
	landsMergeForward(t, pool)
	again := conflictingRead(requiresCI)
	again.HeadSHA = forwardHead
	applyRefusingNothing(t, pool, readsAfter(4*time.Minute), again)
	applyRefusingNothing(t, pool, readsAfter(5*time.Minute), again)
	pr := recordedPR(t, pool, "LEGION-208")
	if pr.ConflictingHead != forwardHead || pr.MergeForward == nil || pr.MergeForward.Head != conflictHead || pr.MergeForward.Landed != forwardHead {
		t.Fatalf("the record holds %q conflicting and the merge forward %+v, want %s's conflict recorded and no second ask in retro, the first landed as %s",
			pr.ConflictingHead, pr.MergeForward, forwardHead, forwardHead)
	}
	asked := len(mergeForwardMessages(t, pool))

	row, rows := entryRow(t, pool, claim.RoleImplementer), len(queuedRows(t, pool))
	result := applyFacts(t, pool, readsAfter(6*time.Minute))("at the head the first merge replaced", intake.HandoffComplete{Generation: 1,
		Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: "implement-claim", Summary: "retro done", Commit: conflictHead})
	if code := refusalCode(result); code != "HANDOFF_HEAD_MERGED_FORWARD" {
		t.Fatalf("the implementer's retro completion at %s was answered %q (%+v), want HANDOFF_HEAD_MERGED_FORWARD", conflictHead, code, result.Refusal)
	}
	msg := result.Refusal.Message
	if says := "do the phase's work at head " + forwardHead; !strings.Contains(msg, says) {
		t.Fatalf("the refusal reads %q, want it to say %q: no hold of the merge forward asked of %s reaches the implementer", msg, says, forwardHead)
	}
	if says := "Wait for the daemon's message"; strings.Contains(msg, says) {
		t.Fatalf("the refusal reads %q, want it not to say %q: no landed message of a merge forward goes to the implementer", msg, says)
	}
	if after := entryRow(t, pool, claim.RoleImplementer); !reflect.DeepEqual(after, row) {
		t.Fatalf("the completion took the implementer's row from %+v to %+v, want nothing recorded", row, after)
	}
	if after := len(queuedRows(t, pool)); after != rows {
		t.Fatalf("the completion took the outbox from %d rows to %d, want nothing queued", rows, after)
	}
	if got := issuePhase(t, pool); got != phase.Retro {
		t.Fatalf("after the refused completion the issue is in %s, want retro", got)
	}

	applyRefusingNothing(t, pool, readsAfter(7*time.Minute), intake.HandoffComplete{Generation: 1, Issue: "LEGION-208",
		Role: claim.RoleImplementer, Claim: "implement-claim", Summary: "retro done", Commit: forwardHead})
	if got := issuePhase(t, pool); got != phase.Merging {
		t.Fatalf("after retro's completion at %s the issue is in %s, want merging", forwardHead, got)
	}
	want := []record.MergeForwardMessage{{Role: claim.RoleMerger, Step: record.MergeForwardAsk, Head: forwardHead, Base: "main",
		Phase: phase.Merging, PullRequest: 42, Why: record.ConflictMergeNext}}
	if got := mergeForwardMessages(t, pool)[asked:]; !reflect.DeepEqual(got, want) {
		t.Fatalf("the move into merging queued %+v, want the merger's one ask %+v", got, want)
	}
	if mf := recordedPR(t, pool, "LEGION-208").MergeForward; mf == nil || mf.Head != forwardHead || mf.Role != claim.RoleMerger ||
		!reflect.DeepEqual(mf.Earlier, []record.LandedMergeForward{{Head: conflictHead, Landed: forwardHead}}) {
		t.Fatalf("the record's merge forward = %+v, want the merger's ask of %s after the first, of %s, landed as %s", mf, forwardHead, conflictHead, forwardHead)
	}
}

// An ask made in implementing covers implementing alone and sends no hold, and the next phase
// asks it again only at a head the record holds in a confirmed conflict (mergeForwardOnEntry).
// When the head a first merge forward left conflicts in its turn and is asked of again in
// implementing, a MERGEABLE read of that head releases the conflict, and the implementer completes
// at it without the merge, the move into testing asks nothing: the ask of implementing stands, and
// no hold reached the tester. So the tester's completion naming no head is refused
// HANDOFF_HEAD_MERGED_FORWARD (dispatch://INFRA-4374 R1) and sent to do the phase's work at the
// head the pull request stands at, never to wait for the landed message of an ask whose hold it
// was not sent. It records and moves nothing.
func TestAConflictKeepsPhaseSendsACompletionToTheCurrentHeadWhenTheAskOfImplementingWasNotAskedAgain(t *testing.T) {
	requiresCI := &record.BaseRules{Required: []string{"ci"}}
	pool := seedConflictIssue(t, phase.Implementing, "in_progress", settledGreen("lint"))
	confirmsConflict(t, pool, requiresCI)
	landsMergeForward(t, pool)
	again := conflictingRead(requiresCI)
	again.HeadSHA = forwardHead
	applyRefusingNothing(t, pool, readsAfter(4*time.Minute), again)
	applyRefusingNothing(t, pool, readsAfter(5*time.Minute), again)
	applyRefusingNothing(t, pool, readsAfter(6*time.Minute), intake.PullRequestMergeability{Repo: "sjawhar/legion", Number: 42,
		Base: "main", HeadSHA: forwardHead, Mergeable: record.MergeabilityMergeable, MergeState: record.MergeStateClean})
	if pr := recordedPR(t, pool, "LEGION-208"); pr.ConflictingHead != "" {
		t.Fatalf("after the MERGEABLE read of %s the record holds %q conflicting, want none", forwardHead, pr.ConflictingHead)
	}
	applyRefusingNothing(t, pool, readsAfter(7*time.Minute), intake.HandoffComplete{Generation: 1, Issue: "LEGION-208",
		Role: claim.RoleImplementer, Claim: "implement-claim", Summary: "implemented", Commit: forwardHead, HandoffRecord: "implement-1", HandoffVersion: 1})
	if got := issuePhase(t, pool); got != phase.Testing {
		t.Fatalf("after the implementer's completion the issue is in %s, want testing", got)
	}
	if asked := recordedPR(t, pool, "LEGION-208").MergeForward; asked == nil || asked.Head != forwardHead || asked.Phase != phase.Implementing || asked.Landed != "" ||
		!reflect.DeepEqual(asked.Earlier, []record.LandedMergeForward{{Head: conflictHead, Landed: forwardHead}}) {
		t.Fatalf("the record's merge forward = %+v, want the ask of implementing, of %s and not landed, after the first, of %s, landed as %s: the move into testing asks nothing",
			asked, forwardHead, conflictHead, forwardHead)
	}
	for _, message := range mergeForwardMessages(t, pool) {
		if message.Role == claim.RoleTester {
			t.Fatalf("merge-forward message %+v went to the tester, want none: an ask of implementing sends no hold", message)
		}
	}

	row, rows := entryRow(t, pool, claim.RoleTester), len(queuedRows(t, pool))
	result := applyFacts(t, pool, readsAfter(8*time.Minute))("naming no head", intake.HandoffComplete{Generation: 1,
		Issue: "LEGION-208", Role: claim.RoleTester, Claim: "tester-claim", Summary: "tested", Verdict: "pass",
		HandoffRecord: "handoff-1", HandoffVersion: 1})
	if code := refusalCode(result); code != "HANDOFF_HEAD_MERGED_FORWARD" {
		t.Fatalf("the tester's completion naming no head was answered %q (%+v), want HANDOFF_HEAD_MERGED_FORWARD", code, result.Refusal)
	}
	msg := result.Refusal.Message
	if says := "do the phase's work at head " + forwardHead; !strings.Contains(msg, says) {
		t.Fatalf("the refusal reads %q, want it to say %q: no hold of the merge forward asked of %s in implementing reached the tester", msg, says, forwardHead)
	}
	if says := "Wait for the daemon's message"; strings.Contains(msg, says) {
		t.Fatalf("the refusal reads %q, want it not to say %q: the ask of implementing sent the tester no hold", msg, says)
	}
	if after := entryRow(t, pool, claim.RoleTester); !reflect.DeepEqual(after, row) {
		t.Fatalf("the completion took the tester's row from %+v to %+v, want nothing recorded", row, after)
	}
	if after := len(queuedRows(t, pool)); after != rows {
		t.Fatalf("the completion took the outbox from %d rows to %d, want nothing queued", rows, after)
	}
	if got := issuePhase(t, pool); got != phase.Testing {
		t.Fatalf("after the refused completion the issue is in %s, want testing", got)
	}
}

// A backward move gives the workspace to the earlier phase's worker. A queued message must not
// wake the merger to write or merge there, but the same ask still reaches a hold from merging.
func TestAMergersMergeForwardMessageEndsWhenTheIssueMovesBack(t *testing.T) {
	for _, target := range []phase.Phase{phase.Implementing, phase.Testing, phase.Reviewing, phase.Accepting, phase.Retro} {
		t.Run(string(target), func(t *testing.T) {
			pool := seedConflictIssue(t, phase.Merging, "retro", settledGreen("lint"))
			confirmsConflict(t, pool, &record.BaseRules{Required: []string{"ci"}})
			pr := recordedPR(t, pool, "LEGION-208")
			issue := storedIssue(t, pool, "LEGION-208")
			if why := leftMergeForwardAt(issue, false, &pr, claim.RoleMerger, record.MergeForwardAsk, conflictHead); why != "" {
				t.Fatalf("the merger's ask in merging is undelivered: %s", why)
			}
			issue.Phase, issue.Hold = phase.Held, &record.Hold{From: phase.Merging}
			if why := leftMergeForwardAt(issue, false, &pr, claim.RoleMerger, record.MergeForwardAsk, conflictHead); why != "" {
				t.Fatalf("the merger's ask in a hold from merging is undelivered: %s", why)
			}

			applyRefusingNothing(t, pool, readsAfter(2*time.Minute), intake.BackwardMove{Issue: "LEGION-208", Requester: claim.RoleMerger,
				To: target, Reason: "the phase's work needs another read before this head can merge"})
			pr = recordedPR(t, pool, "LEGION-208")
			issue = storedIssue(t, pool, "LEGION-208")
			if issue.Phase != target {
				t.Fatalf("the backward move left the issue in %s, want %s", issue.Phase, target)
			}
			if why := leftMergeForwardAt(issue, false, &pr, claim.RoleMerger, record.MergeForwardAsk, conflictHead); why == "" {
				t.Errorf("a queued merger ask still reaches %s after the backward move", target)
			}
		})
	}
}

// Sending a merged-forward head back to retro does not erase the old-head completion fence or
// the review the new head needs when it returns to merging. Only the merger's stale message ends.
func TestALandedMergeForwardKeepsItsReviewAfterABackwardMove(t *testing.T) {
	pool := seedConflictIssue(t, phase.Merging, "retro", settledGreen("lint"))
	confirmsConflict(t, pool, &record.BaseRules{Required: []string{"ci"}})
	landsMergeForward(t, pool)
	applyRefusingNothing(t, pool, readsAfter(4*time.Minute), intake.BackwardMove{Issue: "LEGION-208", Requester: claim.RoleMerger,
		To: phase.Retro, Reason: "retro needs to read the merged code"})
	pr := recordedPR(t, pool, "LEGION-208")
	issue := storedIssue(t, pool, "LEGION-208")
	if why := leftMergeForwardAt(issue, false, &pr, claim.RoleMerger, record.MergeForwardLanded, conflictHead); why == "" {
		t.Fatal("the queued merger landing still reaches retro after the backward move")
	}
	if why := leftMergeForwardAt(issue, false, &pr, claim.RoleReviewer, record.MergeForwardReview, conflictHead); why != "" {
		t.Fatalf("the review of the merge went undelivered: %s", why)
	}
	if !mergeForwardReviewOwed(pr) {
		t.Fatalf("the backward move lost the review of the landed merge: %+v", pr.MergeForward)
	}
	complete := intake.HandoffComplete{Generation: 1, Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: "implement-claim",
		Summary: "retro read the merge", Commit: conflictHead}
	result := applyFacts(t, pool, readsAfter(5*time.Minute))("retro at the replaced head", complete)
	if code := refusalCode(result); code != "HANDOFF_HEAD_MERGED_FORWARD" {
		t.Fatalf("retro at the replaced head was answered %q, want HANDOFF_HEAD_MERGED_FORWARD", code)
	}
	complete.Commit = forwardHead
	applyRefusingNothing(t, pool, readsAfter(6*time.Minute), complete)
	if got := issuePhase(t, pool); got != phase.Merging {
		t.Fatalf("retro at the current head left the issue in %s, want merging", got)
	}
	if task := startTask(t, pool, claim.RoleMerger); !strings.Contains(task, "merge head "+forwardHead+" only once the reviewer's approval of it is on the pull request") {
		t.Fatalf("the merger's new task lost the review of the merged head: %q", task)
	}
}
