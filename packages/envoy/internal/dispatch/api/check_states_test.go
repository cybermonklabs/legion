// check_states_test.go holds the rig and the readers of the tests that hold the check states
// settler (check_states.go) to its rules: each write that can change the state of a
// `before_merge` check at an open head, or at a former head of an open pull request, marks the
// check's issue owed in its own transaction, and the settler publishes one check.state_changed
// event for each change of a state, compared with the state it published last, and none where
// nothing changed. A test handoff waits until the states its results caused are published. The
// tests are in check_states_publish_test.go (what is published), check_states_former_heads_test.go
// (the former heads of an open pull request), check_states_settle_test.go (how a settle starts and
// runs) and check_states_fence_test.go (the test handoff's wait and the serial).
//
// Each test drives the settler itself: settleOwed runs its pass until no mark is due, so a test
// reads the events one write caused before it makes the next. The tests of the loop
// (TestAMarkThatNoStartSettledIsSettledByTheBackgroundPass, TestTheWritersCommitStartsTheSettle,
// TestATestHandoffComesAfterTheStatesItsResultsCaused) run it as the server does.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/duty"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/pglisten"
)

// checkStatesBlock is check block id carrying attributes, with words as its text.
func checkStatesBlock(id, attributes, words string) string {
	return ":::check{#" + id + " " + attributes + "}\n" + words + "\n:::\n"
}

// checkStatesTwoChecks is the Verification document of most of these tests: k1 and k2, two
// ordinary `before_merge` checks, in that order.
var checkStatesTwoChecks = checkStatesBlock("k1", ciCheckOrdinary, "The server records a result against a check.") + "\n" +
	checkStatesBlock("k2", ciCheckOrdinary, "The server answers a read of the results.")

// checkStatesWithCI adds k3, a check CI proves: its ci_check names the rig's job, whose runs the
// rig's stand-in for GitHub lists at each head.
var checkStatesWithCI = checkStatesTwoChecks + "\n" +
	checkStatesBlock("k3", `negative_control="`+ciJobNegativeControl+`" ci_check="`+ciJobName+`"`, "CI builds the server at the head.")

// checkStatesTestSchedule is the production schedule with a short gap and a short poll, so a loop
// a test runs is quick to answer; the retry and its ceiling are production's, which the test of a
// failed read reads back.
var checkStatesTestSchedule = checkStatesSchedule{
	minGap: 50 * time.Millisecond, sweep: time.Minute, retry: 30 * time.Second, retryMax: 10 * time.Minute,
	fenceWait: 30 * time.Second, fencePoll: 20 * time.Millisecond, batch: 20, rounds: 5,
}

// checkStatesRig is the CI job rig (newCIJobRig), whose issue ACME-1 holds the Verification
// document a test gives it, with a settler of its own: the server under test's, on a lease
// connection of the rig's own lease pool.
type checkStatesRig struct {
	*ciJobRig
	settler *checkStatesSettler
	callers *store.LeaseCallers
}

func newCheckStatesRig(t *testing.T, verification string) *checkStatesRig {
	t.Helper()
	rig := newCIJobRig(t)
	uploadDocument(t, rig.handler, "/api/v1/issues/"+rig.issue, "verification.md", verification)
	callers, err := rig.database.Pool.Leases()
	if err != nil {
		t.Fatalf("open the lease pool: %v", err)
	}
	r := &checkStatesRig{ciJobRig: rig, callers: callers}
	r.settler = r.newSettler(t, "check states")
	r.served.checkStatesSchedule = checkStatesTestSchedule
	return r
}

// newSettler is a settler of the server under test on a reservation of its own, as another
// process's settler holds a lease connection of its own.
func (r *checkStatesRig) newSettler(t *testing.T, caller string) *checkStatesSettler {
	t.Helper()
	leases, err := r.callers.For(caller)
	if err != nil {
		t.Fatalf("reserve a lease connection for %s: %v", caller, err)
	}
	return &checkStatesSettler{server: r.served, leases: leases, exclusive: duty.Exclusive, schedule: checkStatesTestSchedule}
}

// oneRound is settler with a schedule of one round, sharing its lease reservation; a test runs it
// where settler itself runs nothing. A test that holds a settle and asserts what its round decided,
// or left undecided, settles with it, so the assertion reads the round the test held. A round that
// leaves a read unfinished keeps its marks due minGap on, from the start of its own transaction,
// and a settle runs another round while a mark is due: on a loaded machine the round's end can
// come later than that, and the next round reads GitHub again, deciding what the assertion means
// to find undecided, or starting a read the test holds and never releases.
func oneRound(settler *checkStatesSettler) *checkStatesSettler {
	held := *settler
	held.schedule.rounds = 1
	return &held
}

// fileIssue files one more issue of project ACME, with no Verification document, and answers it.
func (r *checkStatesRig) fileIssue(t *testing.T, title string) string {
	t.Helper()
	filed := r.asPerson(t, http.MethodPost, "/api/v1/issues", map[string]any{"project": ciJobProject, "title": title, "spec": "A spec\n"})
	if filed.Code != http.StatusCreated {
		t.Fatalf("file %q: status=%d body=%s", title, filed.Code, filed.Body.String())
	}
	return decodeBody[model.Issue](t, filed).Key
}

// checkStatesEvent is one stored check.state_changed event: its owner's sequence, its issue, its
// actor and its payload, as stored and as decoded.
type checkStatesEvent struct {
	Seq     int64
	Issue   string
	Actor   model.Actor
	Raw     json.RawMessage
	Payload model.CheckStateChangedEventPayload
}

// check is the payload's check, "" in the set form.
func (e checkStatesEvent) check() string {
	if e.Payload.Check == nil {
		return ""
	}
	return *e.Payload.Check
}

// state is the payload's state, "" in the set form.
func (e checkStatesEvent) state() string {
	if e.Payload.State == nil {
		return ""
	}
	return *e.Payload.State
}

// line is the event as one line a failure prints: issue, check, head, state and reason.
func (e checkStatesEvent) line() string {
	check := e.check()
	if check == "" {
		check = "(set " + strings.Join(e.Payload.Checks, ",") + ")"
	}
	return fmt.Sprintf("%s %s@%s %s/%s", e.Issue, check, e.Payload.Head[:6], e.state(), e.Payload.Reason)
}

func eventLines(events []checkStatesEvent) []string {
	lines := make([]string, 0, len(events))
	for _, event := range events {
		lines = append(lines, event.line())
	}
	return lines
}

// checkStateEventsAfter is every check.state_changed event above id, in id order.
func checkStateEventsAfter(t *testing.T, database *store.Store, id int64) []checkStatesEvent {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `
		select seq, issue_key, actor::text, payload::text from events
		where type = 'check.state_changed' and id > $1 order by id`, id)
	if err != nil {
		t.Fatalf("read the check.state_changed events: %v", err)
	}
	defer rows.Close()
	var events []checkStatesEvent
	for rows.Next() {
		var event checkStatesEvent
		var actor, payload string
		if err := rows.Scan(&event.Seq, &event.Issue, &actor, &payload); err != nil {
			t.Fatalf("read a check.state_changed event: %v", err)
		}
		if err := json.Unmarshal([]byte(actor), &event.Actor); err != nil {
			t.Fatalf("decode the actor %s: %v", actor, err)
		}
		event.Raw = json.RawMessage(payload)
		if err := json.Unmarshal(event.Raw, &event.Payload); err != nil {
			t.Fatalf("decode the payload %s: %v", payload, err)
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the check.state_changed events: %v", err)
	}
	return events
}

// dueCheckStateMarks is how many marks of any issue are due.
func dueCheckStateMarks(t *testing.T, database *store.Store) int {
	t.Helper()
	var count int
	if err := database.Pool.QueryRow(context.Background(),
		`select count(*) from check_states_owed where due_at <= now()`).Scan(&count); err != nil {
		t.Fatalf("count the due marks: %v", err)
	}
	return count
}

// marksOf is how many marks issue holds, due or not.
func marksOf(t *testing.T, database *store.Store, issue string) int {
	t.Helper()
	var count int
	if err := database.Pool.QueryRow(context.Background(),
		`select count(*) from check_states_owed where issue_key = $1`, issue).Scan(&count); err != nil {
		t.Fatalf("count the marks of %s: %v", issue, err)
	}
	return count
}

// settleOwed runs the rig's settler's pass until no mark is due, at most 20 passes, and answers
// the check.state_changed events those passes appended, oldest first. Marks still due after the
// 20th pass fail the test without ending it, so the test's own assertion on the events says what
// the settles did not publish.
func settleOwed(t *testing.T, rig *checkStatesRig) []checkStatesEvent {
	t.Helper()
	return settleOwedBy(t, rig, rig.settler)
}

// settleOwedBy is settleOwed with settler's passes.
func settleOwedBy(t *testing.T, rig *checkStatesRig, settler *checkStatesSettler) []checkStatesEvent {
	t.Helper()
	ctx := store.WithTransactionTracking(t.Context())
	after := lastEventID(t, rig.database)
	for pass := 0; dueCheckStateMarks(t, rig.database) > 0; pass++ {
		if pass == 20 {
			t.Errorf("20 passes of the settler left %d marks due", dueCheckStateMarks(t, rig.database))
			break
		}
		settler.pass(ctx)
	}
	return checkStateEventsAfter(t, rig.database, after)
}

// settleBy runs one settle of the rig's issue by settler, failing the test unless it ran and
// left no due mark, and answers the events it appended.
func settleBy(t *testing.T, rig *checkStatesRig, settler *checkStatesSettler) []checkStatesEvent {
	t.Helper()
	after := lastEventID(t, rig.database)
	ran, failed, owed := settler.settle(store.WithTransactionTracking(t.Context()), rig.issue)
	if !ran || failed || owed {
		t.Fatalf("the settle of %s answered ran=%v failed=%v owed=%v, want a settle that ran and left nothing owed", rig.issue, ran, failed, owed)
	}
	return checkStateEventsAfter(t, rig.database, after)
}

// stateWant is one event a test expects: the check ("" for the set form), the head, the state
// and the reason.
type stateWant struct {
	check, head, state, reason string
}

func (w stateWant) String() string {
	check := w.check
	if check == "" {
		check = "(set)"
	}
	return fmt.Sprintf("%s@%s %s/%s", check, w.head[:6], w.state, w.reason)
}

func wantOf(event checkStatesEvent) stateWant {
	return stateWant{check: event.check(), head: event.Payload.Head, state: event.state(), reason: event.Payload.Reason}
}

// wantEvents fails the test unless events are exactly want, in order.
func wantEvents(t *testing.T, what string, events []checkStatesEvent, want ...stateWant) {
	t.Helper()
	got := make([]stateWant, 0, len(events))
	for _, event := range events {
		got = append(got, wantOf(event))
	}
	if !slices.Equal(got, want) {
		t.Fatalf("%s: the events are %v, want %v (%v)", what, got, want, eventLines(events))
	}
}

// wantEventSet fails the test unless events are want in any order.
func wantEventSet(t *testing.T, what string, events []checkStatesEvent, want ...stateWant) {
	t.Helper()
	got := make([]string, 0, len(events))
	for _, event := range events {
		got = append(got, wantOf(event).String())
	}
	wanted := make([]string, 0, len(want))
	for _, one := range want {
		wanted = append(wanted, one.String())
	}
	slices.Sort(got)
	slices.Sort(wanted)
	if !slices.Equal(got, wanted) {
		t.Fatalf("%s: the events are %v, want %v in any order", what, got, wanted)
	}
}

// eventsOfCheck is the events of check at head among events.
func eventsOfCheck(events []checkStatesEvent, check, head string) []checkStatesEvent {
	var of []checkStatesEvent
	for _, event := range events {
		if event.check() == check && event.Payload.Head == head {
			of = append(of, event)
		}
	}
	return of
}

// publishedState is one row of check_states.
type publishedState struct {
	state           *string
	reason          string
	roles           []string
	ciCase          string
	checks          []string
	checkArtifactID *string
	checkVersion    *int
	eventSeq        int64
}

// publishedRow is the row of check_states the settler holds for check at head of repo on issue,
// "" for the set row, and false where it holds none.
func publishedRow(t *testing.T, database *store.Store, issue, repo, head, check string) (publishedState, bool) {
	t.Helper()
	var row publishedState
	err := database.Pool.QueryRow(context.Background(), `
		select state, reason, roles, ci_case, checks, check_artifact_id::text, check_version, event_seq
		from check_states where issue_key = $1 and repo = $2 and head_sha = $3 and check_block_id = $4`,
		issue, repo, head, check).Scan(&row.state, &row.reason, &row.roles, &row.ciCase, &row.checks,
		&row.checkArtifactID, &row.checkVersion, &row.eventSeq)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return publishedState{}, false
		}
		t.Fatalf("read the published state of %s at %s: %v", check, head, err)
	}
	return row, true
}

// computedSubject is the state a fresh computation of the settler's gives check at head of the
// rig's issue, at an open head or a former one: its subject there in answerIssueChecks's answer for
// the issue's `before_merge` checks, given the issue's former heads apart from its open heads
// (formerHeadsOf, formerHeadsApart).
func computedSubject(t *testing.T, rig *checkStatesRig, check, head string) model.PassStandsSubject {
	t.Helper()
	ctx := store.WithTransactionTracking(t.Context())
	heads, err := rig.served.deps.Attributor.HeadsAttributedTo(ctx, rig.database.Pool, rig.issue)
	if err != nil {
		t.Fatalf("read the heads of %s: %v", rig.issue, err)
	}
	former, err := formerHeadsOf(ctx, rig.database.Pool, rig.issue, heads)
	if err != nil {
		t.Fatalf("read the former heads of %s: %v", rig.issue, err)
	}
	answer, _, err := rig.served.answerIssueChecks(ctx, rig.issue, noCommitNamed, nil, formerHeadsApart(former, heads), docs.CheckWhenBeforeMerge)
	if err != nil {
		t.Fatalf("compute the checks of %s: %v", rig.issue, err)
	}
	for _, block := range answer.Checks {
		if block.BlockID != check {
			continue
		}
		for _, subject := range block.PassStands.Subjects {
			if subject.Basis.Commit == head {
				return subject
			}
		}
	}
	t.Fatalf("the computation of %s holds no state of %s at %s", rig.issue, check, head)
	return model.PassStandsSubject{}
}

// wantPublishedIsComputed fails the test unless the published state of check at head is the
// state a fresh computation gives it, and no mark of the rig's issue is left.
func wantPublishedIsComputed(t *testing.T, what string, rig *checkStatesRig, check, head string) {
	t.Helper()
	computed := computedSubject(t, rig, check, head)
	row, held := publishedRow(t, rig.database, rig.issue, ciJobRepository, head, check)
	if !held {
		t.Fatalf("%s: no state of %s at %s is published, and Dispatch computes %s/%s", what, check, head, computed.Answer, computed.Reason)
	}
	ciCase := ""
	if computed.CI != nil {
		ciCase = computed.CI.Case
	}
	if row.state == nil || *row.state != computed.Answer || row.reason != computed.Reason || row.ciCase != ciCase ||
		!slices.Equal(slices.Sorted(slices.Values(row.roles)), slices.Sorted(slices.Values(computed.Roles))) {
		state := "null"
		if row.state != nil {
			state = *row.state
		}
		t.Fatalf("%s: the published state of %s at %s is %s/%s roles %v, and Dispatch computes %s/%s roles %v",
			what, check, head, state, row.reason, row.roles, computed.Answer, computed.Reason, computed.Roles)
	}
	if left := marksOf(t, rig.database, rig.issue); left != 0 {
		t.Fatalf("%s: %d marks of %s are left", what, left, rig.issue)
	}
}

// writerFail inserts a tester's fail of check at commit on the rig's issue with plain SQL, in tx,
// as a writer that runs no code of this build writes a result: the check's version and digest
// read from the block index.
func writerFail(t *testing.T, rig *checkStatesRig, exec func(context.Context, string, ...any) error, check, commit string) {
	t.Helper()
	if err := exec(context.Background(), `
		insert into check_results (issue_key, check_issue_key, check_artifact_id, check_block_id, check_version,
			check_sha256, role, commit_sha, verdict, failed, actor)
		select $1, $1, artifact_id, block_id, changed_version, text_sha256, 'tester', $2, 'fail', 1,
			'{"kind":"user","id":"alice"}'::jsonb
		from document_blocks where artifact_id = $3::uuid and block_id = $4`,
		rig.issue, commit, rig.verification, check); err != nil {
		t.Fatalf("insert a fail of %s at %s: %v", check, commit, err)
	}
}

// holdOnce is a settler hook that holds the first settle that reaches it until release is
// called, and closes reached when it does and returned once the hook has returned. It also
// returns once its test's context ends, which comes before every cleanup of the test: a cleanup
// that stops a loop waits for the loop's settle, and a settle still held there would never end,
// whatever order the test registered its cleanups in.
type holdOnce struct {
	reached  chan struct{}
	unblock  chan struct{}
	returned chan struct{}
	ended    <-chan struct{}
	first    sync.Once
	once     sync.Once
}

func newHoldOnce(t *testing.T) *holdOnce {
	hold := &holdOnce{reached: make(chan struct{}), unblock: make(chan struct{}), returned: make(chan struct{}), ended: t.Context().Done()}
	t.Cleanup(hold.release)
	return hold
}

func (h *holdOnce) hook(string) {
	first := false
	h.first.Do(func() { first = true })
	if !first {
		return
	}
	defer close(h.returned)
	close(h.reached)
	select {
	case <-h.unblock:
	case <-h.ended:
	}
}

func (h *holdOnce) release() { h.once.Do(func() { close(h.unblock) }) }

func (h *holdOnce) await(t *testing.T, what string) {
	t.Helper()
	select {
	case <-h.reached:
	case <-time.After(10 * time.Second):
		t.Fatalf("%s did not reach its hook within 10s", what)
	}
}

// runCheckStatesLoop runs settler's loop on wake until the test ends or stop is called.
func runCheckStatesLoop(t *testing.T, settler *checkStatesSettler, wake <-chan struct{}) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		settler.run(ctx, wake)
	}()
	stop = sync.OnceFunc(func() {
		cancel()
		<-done
	})
	t.Cleanup(stop)
	return stop
}

// listenForCheckStates runs a pglisten.Hub on the rig's database, as each server process runs its
// one, and answers its wake on CheckStatesOwedChannel once the hub listens.
func listenForCheckStates(t *testing.T, rig *checkStatesRig) <-chan struct{} {
	t.Helper()
	hub := &pglisten.Hub{Owner: "check states test", Dial: rig.database.Pool.Listener}
	wake := hub.Wake(CheckStatesOwedChannel)
	connected := make(chan struct{})
	hub.OnConnected(sync.OnceFunc(func() { close(connected) }))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		hub.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	select {
	case <-connected:
	case <-time.After(10 * time.Second):
		t.Fatal("the hub did not connect within 10s")
	}
	return wake
}

// awaitEvents polls for up to within until the check.state_changed events above after hold one
// that matches, and answers them.
func awaitEvents(t *testing.T, rig *checkStatesRig, after int64, within time.Duration, matches func([]checkStatesEvent) bool) []checkStatesEvent {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		events := checkStateEventsAfter(t, rig.database, after)
		if matches(events) {
			return events
		}
		if time.Now().After(deadline) {
			return events
		}
		time.Sleep(20 * time.Millisecond)
	}
}
