// Package checkstatetest builds the check.state_changed events the daemon's tests apply. Each is
// the payload of packages/contracts/fixtures/dispatch-events/check-state-changed.json, which
// Envoy's golden test writes from the payload Dispatch publishes, with the fields of the state a
// test names written over it, wrapped as Dispatch's event log serves an event and its durable
// stream carries one, and decoded by the intake's own decoder of a Dispatch event (the one the
// live consumer's envelope decoder and the boot catch-up both call). So no test writes a state to
// the store: each goes through the decoder and intake.ApplyFact as a live one does. AfterAMove
// reads the states of a second fixture, the events Dispatch published in two cases of a pull
// request's head moving while its tester tested the head before, which a test applies the same
// way.
//
// It is an ordinary package rather than a _test.go file so the tests of api, daemon and workflow
// can share it; Go cannot share test-only code across packages.
package checkstatetest

//ci:reads packages/contracts/fixtures/dispatch-events/check-state-changed.json ReadFixture reads each state event the api, daemon and workflow tests apply, the payload Envoy's golden writes
//ci:reads packages/contracts/fixtures/dispatch-events/check-states-after-a-move.json AfterAMove reads the events Envoy's TestAFormerHeadOfAnOpenPullRequestKeepsItsStates writes, which workflow's TestAResultAtTheTestedHeadAfterAMoveDecidesTheMove applies

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// Fixture is the shared fixture of check.state_changed: the event's type and actor, and its
// payload in the check form and in the set form.
type Fixture struct {
	Type      string          `json:"type"`
	Actor     json.RawMessage `json:"actor"`
	CheckForm json.RawMessage `json:"check_form"`
	SetForm   json.RawMessage `json:"set_form"`
}

// ReadFixture reads the shared fixture.
func ReadFixture(t testing.TB) Fixture {
	t.Helper()
	var fixture Fixture
	readDispatchEvents(t, "check-state-changed.json", &fixture)
	if fixture.Type == "" || len(fixture.CheckForm) == 0 || len(fixture.SetForm) == 0 {
		t.Fatalf("the shared fixture of check.state_changed = %+v, want its type and both forms", fixture)
	}
	return fixture
}

// AfterAMoveEvent is one check.state_changed event of
// packages/contracts/fixtures/dispatch-events/check-states-after-a-move.json, which Envoy's
// TestAFormerHeadOfAnOpenPullRequestKeepsItsStates writes from the events Dispatch published when
// a pull request moved from H1 to H2 while its tester tested H1: each field of the payload the
// daemon reads.
type AfterAMoveEvent struct {
	Check        string   `json:"check"`
	Head         string   `json:"head"`
	PullRequests []int    `json:"pull_requests"`
	Checks       []string `json:"checks"`
	State        string   `json:"state"`
	Reason       string   `json:"reason"`
	Roles        []string `json:"roles"`
	CICase       string   `json:"ci_case"`
	Serial       int64    `json:"serial"`
}

// AfterAMove is the events of the case named in that fixture, oldest first: fail_after_the_move,
// where the tester records a fail of k2 at H1 after the move, or passes_after_the_move, where it
// records passes of k1 and k2 there.
func AfterAMove(t testing.TB, name string) []AfterAMoveEvent {
	t.Helper()
	var cases map[string][]AfterAMoveEvent
	readDispatchEvents(t, "check-states-after-a-move.json", &cases)
	events := cases[name]
	if len(events) == 0 {
		t.Fatalf("the fixture of the events after a move holds no events of %q", name)
	}
	return events
}

// readDispatchEvents decodes the file name of packages/contracts/fixtures/dispatch-events into
// into. The file is found from this file's own compiled path, not from the working directory: `go
// test` runs a package's tests in its own directory, but a live proof's driver, the daemon
// package's test binary, runs from wherever its script started it
// (scripts/e2e/lib/live-daemon-proof.sh).
func readDispatchEvents(t testing.TB, name string, into any) {
	t.Helper()
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatalf("locate the source of checkstatetest, from which %s is found", name)
	}
	encoded, err := os.ReadFile(filepath.Join(filepath.Dir(source), "..", "..", "..", "contracts", "fixtures", "dispatch-events", name))
	if err != nil {
		t.Fatalf("read the Dispatch events fixture %s: %v", name, err)
	}
	if err := json.Unmarshal(encoded, into); err != nil {
		t.Fatalf("decode the Dispatch events fixture %s: %v", name, err)
	}
}

// Seq is the sequence of an issue's Dispatch event log of its state event of serial: a later
// serial is a later event of the same log, as Dispatch appends them.
func Seq(serial int64) int64 { return 1000 + serial }

// EventID is the event id the live consumer applies the event of issue's log at seq under.
func EventID(issue string, seq int64) string { return fmt.Sprintf("dispatch-event:%s:%d", issue, seq) }

// Event is the event of issue's Dispatch event log at seq that carries state, with the Dispatch
// event id 5000+seq, appended by the fixture's actor: the fixture's payload of state's form (the
// set form where state names no check), at issue, repo and head, with state's set of checks (none
// where it names none) and serial, and, in the check form, its check, state, reason, roles and
// `ci_case`. Its pull request is #42: the daemon reads a state's head, never its pull request's
// number.
func Event(t testing.TB, issue, repo, head string, seq int64, state intake.DispatchCheckState) []byte {
	t.Helper()
	fixture := ReadFixture(t)
	form := fixture.CheckForm
	if state.Check == "" {
		form = fixture.SetForm
	}
	var payload map[string]any
	if err := json.Unmarshal(form, &payload); err != nil {
		t.Fatalf("decode the fixture's payload: %v", err)
	}
	checks := state.Checks
	if checks == nil {
		checks = []string{}
	}
	payload["issue"], payload["repo"], payload["head"], payload["checks"], payload["serial"] = issue, repo, head, checks, state.Serial
	payload["pull_requests"] = []int{42}
	if state.Check != "" {
		roles := state.Roles
		if roles == nil {
			roles = []string{}
		}
		payload["check"], payload["check_version"], payload["state"], payload["reason"] = state.Check, 1, state.State, state.Reason
		payload["roles"], payload["ci_case"] = roles, state.CICase
	}
	event, err := json.Marshal(map[string]any{
		"id": 5000 + seq, "issue_key": issue, "seq": seq, "type": fixture.Type, "notify": false,
		"created_at": "2026-11-21T06:00:00Z", "actor": fixture.Actor, "payload": payload,
	})
	if err != nil {
		t.Fatalf("encode the state event of %s at %d: %v", issue, seq, err)
	}
	return event
}

// Decoded is Event of state at Seq(state.Serial), decoded by the intake's decoder of a Dispatch
// event for the project issue's key names, and the event id the live consumer applies it under.
// It fails t unless the event decodes to a check state with nothing unread.
func Decoded(t testing.TB, issue, repo, head string, state intake.DispatchCheckState) (intake.DispatchCheckState, string) {
	t.Helper()
	seq := Seq(state.Serial)
	project, _, _ := strings.Cut(issue, "-")
	fact, unread, err := intake.DecodeDispatchEvent(project, issue, Event(t, issue, repo, head, seq, state))
	decoded, ok := fact.(intake.DispatchCheckState)
	if err != nil || len(unread) != 0 || !ok {
		t.Fatalf("the state event of %s of serial %d at %s decoded to %#v, unread %q, %v; want a check state", issue, state.Serial, head, fact, unread, err)
	}
	return decoded, EventID(issue, seq)
}

// Apply applies one check.state_changed event of issue at head of repo for each of states, the
// first at serial and each next one at the serial after, through intake.ApplyFact and handlers,
// as the live consumer applies a fact (Decoded). A state naming no set of checks carries the set
// of every check states name; one naming no check is the set form. No state at all is the set
// form of a head with no check.
func Apply(t testing.TB, pool store.TxPool, issue, repo, head string, serial int64, states []intake.DispatchCheckState, handlers ...intake.Handler) {
	t.Helper()
	if len(states) == 0 {
		states = []intake.DispatchCheckState{{Checks: []string{}}}
	}
	var named []string
	for _, state := range states {
		if state.Check != "" && !slices.Contains(named, state.Check) {
			named = append(named, state.Check)
		}
	}
	for i, state := range states {
		state.Serial = serial + int64(i)
		if state.Checks == nil {
			state.Checks = named
		}
		fact, id := Decoded(t, issue, repo, head, state)
		result, err := intake.ApplyFact(context.Background(), pool, "dispatch", id, fact, handlers...)
		if err != nil || result.Refusal != nil {
			t.Fatalf("apply the state event of serial %d: %+v, %v", state.Serial, result.Refusal, err)
		}
	}
}

// Tested is the state event that decides a tester's completion of issue's testing, decoded, and
// its event id: k1, the head's one check, yes from the tester where passes, and no for a fail
// where not. head is the head the completion tests, the commit its test record names: the move
// reads no other, so a test names it and this reads no head of its own. The event is on the
// repository of issue's pull request, or on repo where the record holds none, and takes the
// serial after the highest the daemon holds of issue, so it is the newest of its head.
func Tested(t testing.TB, pool store.TxPool, issue, repo, head string, passes bool) (intake.DispatchCheckState, string) {
	t.Helper()
	if head == "" {
		t.Fatal("a tested state needs the head the completion tests: the move reads the states at that head alone")
	}
	ctx := context.Background()
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatalf("begin the read of %s's pull request: %v", issue, err)
	}
	defer tx.Rollback(ctx)
	var serial int64
	pr, err := record.NewStore().PullRequest(ctx, tx, issue)
	if err == nil {
		err = tx.QueryRow(ctx, `select coalesce(max(serial), 0) from check_state_serials where issue = $1`, issue).Scan(&serial)
	}
	if err != nil {
		t.Fatalf("read the pull request and the serials of %s: %v", issue, err)
	}
	if pr != nil {
		repo = pr.Repo
	}
	state := intake.DispatchCheckState{Check: "k1", State: "yes", Roles: []string{"tester"}}
	if !passes {
		state = intake.DispatchCheckState{Check: "k1", State: "no", Reason: "fail"}
	}
	state.Checks, state.Serial = []string{"k1"}, serial+1
	return Decoded(t, issue, repo, head, state)
}
