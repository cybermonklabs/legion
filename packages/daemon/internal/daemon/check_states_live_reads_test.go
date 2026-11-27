//go:build e2e

package daemon

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// statesMoveDecided is the daemon's line of the testing move, written where it writes the
// tester's notice of a wait or of a move back, and the move to reviewing (workflow's
// logTestingMove).
const statesMoveDecided = "workflow: the testing move decided"

// moveLine is the newest line of the testing move of issue whose move is move, read as
// recordAfterRead reads; a line the daemon's log does not hold by then is UNKNOWN and fails the
// case at assertion.
func (r *statesProof) moveLine(issue *statesIssue, move, assertion string) *loggedRecord {
	r.t.Helper()
	line := r.recordAfterRead(func(l loggedRecord) bool {
		return l.Message == statesMoveDecided && l.Attrs["issue"] == issue.key && l.Attrs["move"] == move
	})
	if line == nil {
		r.fail(assertion, "UNKNOWN: the daemon's log holds no %q line of %s with move %s %s after the read, so what the move names cannot be read",
			statesMoveDecided, issue.key, move, statesRecordWait)
	}
	return line
}

// attrStrings is a list attribute of a line of the daemon's log, as strings; nil where the line has
// none or the list is empty. The driver keeps a list attribute as the text the log shows
// (recordingLog's decodeAttr), "[k1 k2]" for the checks k1 and k2, so the list is that text's
// words between its brackets; a check id holds no blank.
func attrStrings(value any) []string {
	text, _ := value.(string)
	inner, opened := strings.CutPrefix(text, "[")
	inner, closed := strings.CutSuffix(inner, "]")
	if !opened || !closed {
		return nil
	}
	return strings.Fields(inner)
}

// heldStates is what the daemon's store holds of issue's checks at its head (record.Store's
// HeadStates), read in a transaction of its own.
func (r *statesProof) heldStates(issue *statesIssue, assertion string) record.HeadStates {
	r.t.Helper()
	var held record.HeadStates
	if err := store.Transact(context.Background(), r.st.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		var err error
		held, err = r.records.HeadStates(ctx, tx, issue.key, liveRepo, issue.head)
		return err
	}); err != nil {
		r.fail(assertion, "read the states the daemon holds of %s at %s: %v", issue.key, issue.head, err)
	}
	return held
}

// phaseOf is issue's phase as GET /legion/v1/state shows it now. An answer that holds no such issue
// is UNKNOWN and fails the case at assertion: the daemon's phase of it cannot be read.
func (r *statesProof) phaseOf(issue *statesIssue, assertion string) phase.Phase {
	r.t.Helper()
	held, ok := r.d.state().Issues[issue.key]
	if !ok {
		r.fail(assertion, "UNKNOWN: GET /legion/v1/state holds no issue %s, so its phase cannot be read", issue.key)
	}
	return held.Phase
}

// statesPhaseChanged and statesLaunched are the daemon's records of a move (workflow's transition)
// and of a worker's start (supervise's launch, written once the runtime started its process).
const (
	statesPhaseChanged = "workflow: phase changed"
	statesLaunched     = "supervise: launched"
)

// movesWithin holds that GET /legion/v1/state shows issue in want, and that the daemon's own
// record of that move was written within statesWithin of from, the moment of trigger, the
// operation that moves it. The bound is judged on the record's time, not on when the read saw the
// phase, so a read that comes late neither passes a late move nor fails a timely one. It fails at
// assertion at once when the issue leaves testing for another phase, and when it is still in
// testing statesLate after from; a move recorded before from, or later than the bound, fails at
// `<assertion>-within`; a move the daemon's log holds no record of is UNKNOWN.
func (r *statesProof) movesWithin(issue *statesIssue, want phase.Phase, assertion string, from time.Time, trigger string) {
	r.t.Helper()
	got := r.phaseOf(issue, assertion)
	for got == phase.Testing && time.Since(from) < statesLate {
		time.Sleep(100 * time.Millisecond)
		got = r.phaseOf(issue, assertion)
	}
	seen := time.Since(from).Round(time.Millisecond)
	if got != want {
		r.fail(assertion, "GET /legion/v1/state shows %s in %s %s after %s, want %s", issue.key, got, seen, trigger, want)
	}
	moved := r.recordAfterRead(func(l loggedRecord) bool {
		return l.Message == statesPhaseChanged && l.Attrs["issue"] == issue.key && l.Attrs["to"] == string(want)
	})
	if moved == nil {
		r.fail(assertion, "UNKNOWN: GET /legion/v1/state shows %s in %s, and the daemon's log holds no %q record of that move %s later, so when it moved cannot be read",
			issue.key, want, statesPhaseChanged, statesRecordWait)
	}
	took := moved.at.Sub(from)
	if took < 0 || took > statesWithin {
		r.fail(assertion+"-within", "the daemon recorded the move of %s to %s at %s, %s after %s, outside the bound of %s",
			issue.key, want, moved.at.UTC().Format(time.RFC3339Nano), took.Round(time.Millisecond), trigger, statesWithin)
	}
	r.note("read back: GET /legion/v1/state shows %s in %s (read %s after %s); the daemon recorded the move %s after %s (bound %s)",
		issue.key, got, seen, trigger, took.Round(time.Millisecond), trigger, statesWithin)
	for _, state := range r.publishedStates(issue) {
		r.note("  Dispatch's published state: %s", state)
	}
}

// reviewerLaunched waits, until statesLate has passed from from, the moment of trigger, for the
// daemon to start issue's reviewer: to hold its claim and to have asked the runtime to launch its
// pane, and fails the case at assertion when the reviewer has not started by then. It answers how
// long after from the daemon recorded the launch (statesLaunched), the record's own time, and a
// launch the daemon's log holds no record of is UNKNOWN.
func (r *statesProof) reviewerLaunched(issue *statesIssue, assertion string, from time.Time, trigger string) time.Duration {
	r.t.Helper()
	reviewer := r.token(issue.key, claim.RoleReviewer)
	started := func() bool {
		return len(r.rt.LaunchesOf(reviewer)) > 0 && slices.ContainsFunc(r.d.claims(), func(c api.OperatorClaim) bool { return c.Token == reviewer })
	}
	held := started()
	for !held && time.Since(from) < statesLate {
		time.Sleep(100 * time.Millisecond)
		held = started()
	}
	seen := time.Since(from).Round(time.Millisecond)
	launches := r.rt.LaunchesOf(reviewer)
	if !held {
		r.fail(assertion, "the daemon did not start %s within %s of %s: %d launches of its pane", reviewer, statesLate, trigger, len(launches))
	}
	launched := r.recordAfterRead(func(l loggedRecord) bool {
		return l.Message == statesLaunched && l.Attrs["claim"] == string(reviewer)
	})
	if launched == nil {
		r.fail(assertion, "UNKNOWN: the runtime launched %s, and the daemon's log holds no %q record of it %s later, so when it started cannot be read",
			reviewer, statesLaunched, statesRecordWait)
	}
	took := launched.at.Sub(from)
	r.note("read back: the daemon started the reviewer %s (%d launches of its pane, generation %d; read %s after %s); the daemon recorded the launch %s after %s",
		reviewer, len(launches), launches[len(launches)-1].Generation, seen, trigger, took.Round(time.Millisecond), trigger)
	return took
}

// statesRecordWait is how long recordAfterRead waits for a record of what a read already saw: the
// daemon writes a move's record once its transaction commits and a launch's once the runtime's
// start returns, so a read can see either a moment before its record exists.
const statesRecordWait = 5 * time.Second

// recordAfterRead is the newest record of the daemon's log that want matches, read until one
// exists or statesRecordWait has passed; nil when none came.
func (r *statesProof) recordAfterRead(want func(loggedRecord) bool) *loggedRecord {
	deadline := time.Now().Add(statesRecordWait)
	for {
		if found := r.lastLog(want); found != nil || time.Now().After(deadline) {
			return found
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// reviewerStarted holds that the daemon recorded the start of issue's reviewer within
// statesWithin of from, the moment of trigger; a start recorded before from or later than the
// bound fails at reviewer-started-within.
func (r *statesProof) reviewerStarted(issue *statesIssue, from time.Time, trigger string) {
	r.t.Helper()
	if took := r.reviewerLaunched(issue, "reviewer-started", from, trigger); took < 0 || took > statesWithin {
		r.fail("reviewer-started-within", "the daemon recorded the start of %s %s after %s, outside the bound of %s",
			r.token(issue.key, claim.RoleReviewer), took.Round(time.Millisecond), trigger, statesWithin)
	}
}

// implementerTask is the task the daemon queued for issue's implementer when the move sent the
// issue back, read from the claim's pending delivery in the daemon's store, which the implementers'
// held turn keeps pending.
func (r *statesProof) implementerTask(issue *statesIssue) string {
	r.t.Helper()
	token := r.token(issue.key, claim.RoleImplementer)
	deadline := time.Now().Add(time.Minute)
	for {
		var task string
		err := r.st.Pool().QueryRow(context.Background(),
			`select task from pending_task_deliveries where claim_token = $1`, string(token)).Scan(&task)
		switch {
		case err == nil && task != "":
			return task
		case err != nil && !errors.Is(err, pgx.ErrNoRows):
			r.fail("task", "read the pending task of %s from the daemon's store: %v", token, err)
		}
		if time.Now().After(deadline) {
			r.fail("task", "UNKNOWN: the daemon's store holds no pending task of %s within a minute of the move, so what the task names cannot be read", token)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// noImplementerTask holds that the daemon gave issue's implementer nothing: its store holds no
// pending task of the implementer's claim, and the runtime was never asked to launch the
// implementer's pane, which a task taken already would have needed.
func (r *statesProof) noImplementerTask(issue *statesIssue) {
	r.t.Helper()
	token := r.token(issue.key, claim.RoleImplementer)
	var task string
	err := r.st.Pool().QueryRow(context.Background(),
		`select task from pending_task_deliveries where claim_token = $1`, string(token)).Scan(&task)
	switch {
	case err == nil:
		r.fail("no-task", "the daemon's store holds a pending task of %s, whose issue did not go back: %q", token, task)
	case !errors.Is(err, pgx.ErrNoRows):
		r.fail("no-task", "read the pending task of %s from the daemon's store: %v", token, err)
	}
	if launches := r.rt.LaunchesOf(token); len(launches) > 0 {
		r.fail("no-task", "the daemon launched the pane of %s %d times, whose issue did not go back", token, len(launches))
	}
	r.note("read back: the daemon's store holds no pending task of %s, and the runtime never launched its pane", token)
}

// resultsHeld reads every result Dispatch's database holds of issue, in check_results and
// ci_check_results, and holds that they are exactly the results the case recorded on it, each by
// the role that recorded it at the issue's head. An issue the case recorded nothing on must hold
// none, and the case says so.
func (r *statesProof) resultsHeld(issue *statesIssue) {
	r.t.Helper()
	var held []string
	var rows []statesStored
	for _, table := range []string{"check_results", "ci_check_results"} {
		for _, stored := range r.storedResults(table, "issue_key = $1", issue.key) {
			held = append(held, statesRecordedAs(stored.role, stored.check, stored.verdict, stored.commit))
			rows = append(rows, stored)
		}
	}
	want := slices.Clone(issue.recorded)
	slices.Sort(held)
	slices.Sort(want)
	if !slices.Equal(held, want) {
		r.fail("results", "Dispatch's check_results and ci_check_results hold %d results of %s, %v, and the case recorded %d, %q",
			len(rows), issue.key, rows, len(want), want)
	}
	if len(held) == 0 {
		r.note("read back: Dispatch's check_results and ci_check_results hold no result of %s, and the case recorded none", issue.key)
		return
	}
	r.note("read back: Dispatch's check_results and ci_check_results hold of %s exactly the results the case recorded (%d): %s", issue.key, len(held), strings.Join(held, "; "))
}

// noBeforeMergeCheck reads issue's checks as Dispatch answers them (GET /api/v1/issues/<key>/results)
// once it has indexed the document's blocks, and holds that the issue has a Verification document
// whose checks hold no before_merge one. It answers that document's id.
func (r *statesProof) noBeforeMergeCheck(issue *statesIssue) string {
	r.t.Helper()
	type answer struct {
		VerificationArtifactID string `json:"verification_artifact_id"`
		Checks                 []struct {
			BlockID string `json:"block_id"`
			When    string `json:"when"`
		} `json:"checks"`
	}
	var read answer
	for deadline := time.Now().Add(statesLate); ; time.Sleep(100 * time.Millisecond) {
		read = answer{}
		r.dispatch.agentGet("/api/v1/issues/"+issue.key+"/results", &read)
		if len(read.Checks) > 0 || time.Now().After(deadline) {
			break
		}
	}
	if read.VerificationArtifactID == "" || len(read.Checks) == 0 {
		r.fail("document", "UNKNOWN: Dispatch answers %s with the Verification document %q and %d checks %s after the case asked, so it cannot be read as a document that holds checks, none before_merge",
			issue.key, read.VerificationArtifactID, len(read.Checks), statesLate)
	}
	var checks []string
	for _, check := range read.Checks {
		if check.When == "before_merge" {
			r.fail("document", "Dispatch answers %s's check %s as before_merge: the case's document must hold none", issue.key, check.BlockID)
		}
		checks = append(checks, check.BlockID+" "+check.When)
	}
	r.note("read back: Dispatch answers %s's Verification document %s with the checks [%s], none before_merge", issue.key, read.VerificationArtifactID, strings.Join(checks, ", "))
	return read.VerificationArtifactID
}

// emptySet holds that Dispatch publishes of issue no row of check_states and one row of
// check_state_sets, the set at its head, with no check, computed from the Verification document
// document: the published state of a head whose issue has a document with no before_merge check,
// and not of an issue with no document.
func (r *statesProof) emptySet(issue *statesIssue, document string) {
	r.t.Helper()
	published := r.publishedStates(issue)
	if len(published) != 1 || published[0].check != "" || published[0].head != issue.head || len(published[0].checks) != 0 || published[0].document != document {
		r.fail("empty-set", "Dispatch's check_states and check_state_sets hold %v of %s, want only the set at %s with no check, from the Verification document %s", published, issue.key, issue.head, document)
	}
	r.note("read back: Dispatch's check_states and check_state_sets hold of %s only the set at %s, with no check, computed from the Verification document %s", issue.key, issue.head, document)
}

// issueClaims is the tokens of every claim the daemon holds of issue, sorted.
func (r *statesProof) issueClaims(issue *statesIssue) []string {
	var tokens []string
	for _, held := range r.d.claims() {
		if held.Issue == issue.key {
			tokens = append(tokens, string(held.Token))
		}
	}
	slices.Sort(tokens)
	return tokens
}

// roleSessions is every session a tester's or an implementer's result of the run may be written
// under: each tester's pane's own, and the implementer's session pass-goes-to-review records as.
func (r *statesProof) roleSessions() []string {
	var sessions []string
	for _, issue := range r.allIssues() {
		if issue.tester != "" {
			sessions = append(sessions, issue.tester)
		}
		sessions = append(sessions, "implementer-"+strings.ToLower(issue.key))
	}
	return sessions
}
