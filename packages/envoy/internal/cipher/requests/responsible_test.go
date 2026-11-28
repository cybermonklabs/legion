// responsible_test.go pins whose Dispatch Inbox lists a request for a shared human-tier secret:
// the person responsible for the agent that made it, found in four steps (the session's operator,
// a fleet machine's person, a Legion pod's issue assignee, else everyone), while anyone signed in
// still decides it from its page.
package requests

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/record"
	"github.com/sjawhar/envoy/internal/logtest"
)

const (
	// alice and dave are people the fixture's secrets name nowhere; bob and carol are the
	// package's own (approvers_test.go, withheld_test.go).
	alice = "alice@example.com"
	dave  = "dave@example.com"
	// legionPlanner is the claim token the Legion daemon's launcher states for the planner agent
	// of issue ACME-1 (packages/daemon/internal/claim.AgentToken), which B4's enrollments.claim
	// keeps.
	legionPlanner = "legion-acme-acme-1-planner"
)

// standInAssignees answers every issue's assignee as answer, or err, recording each key it was
// asked for. With block set it answers only once block is closed, which the row that sets it does
// when it ends. It ignores its context on purpose, as a Dispatch that never answers does, so a row
// that waits on it proves the caller's own bound and not the stand-in's courtesy. With panics set
// it panics with that value instead of answering.
type standInAssignees struct {
	answer string
	err    error
	block  chan struct{}
	panics any

	mu    sync.Mutex
	asked []string
}

func (s *standInAssignees) Assignee(_ context.Context, issueKey string) (string, error) {
	s.mu.Lock()
	s.asked = append(s.asked, issueKey)
	s.mu.Unlock()
	if s.block != nil {
		<-s.block
	}
	if s.panics != nil {
		panic(s.panics)
	}
	return s.answer, s.err
}

func (s *standInAssignees) keys() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.asked)
}

// newClaimPod inserts a live pod enrollment with no operator whose launcher stated claim, as B4's
// enroll.Service.Create keeps it in enrollments.claim; "" states none.
func newClaimPod(t *testing.T, m *Machine, claim string) (string, *ecdsa.PrivateKey) {
	t.Helper()
	id, key := newEnrollment(t, m.Store, "pod", "pod-"+t.Name(), nil, nil)
	if claim != "" {
		if _, err := m.Store.Pool.Exec(context.Background(), `update enrollments set claim=$1 where id=$2`, claim, id); err != nil {
			t.Fatalf("state the pod's claim: %v", err)
		}
	}
	return id, key
}

// askShared asks for SHARED_KEY, the fixture's shared human-tier secret, as enrollment, and
// fails t unless the request is pending on record.AnyoneApprover.
func askShared(t *testing.T, m *Machine, enrollment string, key *ecdsa.PrivateKey) Request {
	t.Helper()
	req, err := m.Create(context.Background(), enrollment, signRequest(t, m, key, "need the shared key", "SHARED_KEY"), "")
	if err != nil || req.State != "pending" || req.RecordID == nil || req.Approver == nil || *req.Approver != record.AnyoneApprover {
		t.Fatalf("Create(SHARED_KEY) = %+v, %v; want it pending on %s", req, err, record.AnyoneApprover)
	}
	return req
}

// listsFor reports whether GET /v1/pending's list for login holds recordID. It reads both of
// PendingForApprover's statements, the one with no state and Dispatch's (pending and expired),
// and fails when they disagree: the notify rule is one filter both read.
func listsFor(t *testing.T, m *Machine, login, recordID string) bool {
	t.Helper()
	holds := func(states PendingStates) bool {
		pending, err := m.PendingForApprover(context.Background(), login, states)
		if err != nil {
			t.Fatalf("PendingForApprover(%s, %+v): %v", login, states, err)
		}
		return slices.ContainsFunc(pending, func(p PendingSummary) bool { return p.RecordID == recordID })
	}
	plain, byState := holds(PendingStates{}), holds(PendingStates{Pending: true, Expired: true})
	if plain != byState {
		t.Fatalf("PendingForApprover(%s) holds %s %t with no state and %t for pending and expired; want the same", login, recordID, plain, byState)
	}
	return plain
}

// notifyOf reads the person recordID's row names as the one whose Inbox lists it, "" for none.
func notifyOf(t *testing.T, m *Machine, recordID string) string {
	t.Helper()
	var notify *string
	if err := m.Store.Pool.QueryRow(context.Background(), `select notify from credential_requests where id=$1`, recordID).Scan(&notify); err != nil {
		t.Fatalf("read the record's notify: %v", err)
	}
	return deref(notify)
}

// requireListedFor fails t unless recordID is listed for exactly the logins in want among
// everyone in people, and its row names notify.
func requireListedFor(t *testing.T, m *Machine, recordID, notify string, people []string, want ...string) {
	t.Helper()
	for _, login := range people {
		if got, should := listsFor(t, m, login, recordID), slices.Contains(want, login); got != should {
			t.Errorf("PendingForApprover(%s) lists record %s: %v; want %v", login, recordID, got, should)
		}
	}
	if got := notifyOf(t, m, recordID); got != notify {
		t.Errorf("record %s notifies %q; want %q", recordID, got, notify)
	}
}

// TestASharedRequestIsListedForTheSessionsOperator is case 1: a session acting for alice, a
// person's sign-in or a machine login she approved, asks for a shared human-tier secret. Her
// Inbox lists it, and bob's does not.
func TestASharedRequestIsListedForTheSessionsOperator(t *testing.T) {
	m, _, _, _ := newFixture(t)
	session, key := newEnrollment(t, m.Store, "session", "box-"+t.Name(), new(alice), nil)
	req := askShared(t, m, session, key)
	requireListedFor(t, m, *req.RecordID, alice, []string{alice, bob}, alice)
}

// TestASharedRequestIsListedForTheFleetsPerson is case 2: a fleet machine's own session carries no
// operator, and its account's BROKER_VM_EXCHANGE_ACCOUNTS entry names carol, so carol's Inbox
// lists it and bob's does not.
func TestASharedRequestIsListedForTheFleetsPerson(t *testing.T) {
	m, _, _, _ := newFixture(t)
	m.VMServicesByAccount = map[string]string{fixtureMachineAccount: fixtureService}
	m.VMPersonsByAccount = map[string]string{fixtureMachineAccount: carol}
	machine, key := newMachineEnrollment(t, m, fixtureService, fixtureMachineSubject, "machine-"+t.Name())
	req := askShared(t, m, machine, key)
	requireListedFor(t, m, *req.RecordID, carol, []string{carol, bob}, carol)
}

// TestASharedRequestIsListedForTheClaimIssuesAssignee is case 3: a Legion pod whose claim names
// issue ACME-1, whose Dispatch assignee is dave. Dave's Inbox lists it, bob's does not, and
// Dispatch was asked once, for ACME-1.
func TestASharedRequestIsListedForTheClaimIssuesAssignee(t *testing.T) {
	m, _, _, _ := newFixture(t)
	assignees := &standInAssignees{answer: dave}
	m.Assignees = assignees
	pod, key := newClaimPod(t, m, legionPlanner)
	req := askShared(t, m, pod, key)
	requireListedFor(t, m, *req.RecordID, dave, []string{dave, bob}, dave)
	if asked := assignees.keys(); !slices.Equal(asked, []string{"ACME-1"}) {
		t.Errorf("Dispatch was asked for %v; want exactly one read, of ACME-1", asked)
	}
}

// TestASharedRequestWithNoResponsiblePersonIsListedForEveryone is case 4: wherever no step names
// a person, the request is everyone's to see, as it was before, so no request waits where nobody
// sees it. A Dispatch that does not answer in time names nobody too, and Create does not wait for
// it much past its bound. A read that failed, ran out of its bound or panicked logs one
// AssigneeUnreadMessage WARN naming the issue, the enrollment, the error and whether the bound was
// reached, so an operator sees that the step is off, and the line holds nothing of what the read
// held; a row that read nothing, or read an issue nobody is assigned to, logs none.
func TestASharedRequestWithNoResponsiblePersonIsListedForEveryone(t *testing.T) {
	for _, c := range []struct {
		name      string
		claim     string
		assignees *standInAssignees
		noReader  bool
		machine   bool
		// warned is whether the row logs the WARN, and boundReached what it says of the bound.
		warned, boundReached bool
		// errorText is a part of the error the WARN names.
		errorText string
	}{
		{name: "a pod with no claim", assignees: &standInAssignees{answer: dave}},
		{name: "the controller's claim", claim: "legion-acme-controller", assignees: &standInAssignees{answer: dave}},
		{name: "no Dispatch to read", claim: legionPlanner, noReader: true},
		{name: "Dispatch refuses", claim: legionPlanner, assignees: &standInAssignees{err: errors.New("dispatch answered 503 for issue ACME-1")},
			warned: true, errorText: "dispatch answered 503 for issue ACME-1"},
		{name: "the issue has no assignee", claim: legionPlanner, assignees: &standInAssignees{}},
		{name: "Dispatch answers too late", claim: legionPlanner, assignees: &standInAssignees{answer: dave, block: make(chan struct{})},
			warned: true, boundReached: true, errorText: context.DeadlineExceeded.Error()},
		// The read panics with a value naming the reader's token, as a defect could: the request
		// is listed as a failed read leaves it, and the line says the read panicked and nothing of
		// the value. Unrecovered, the panic ends the test binary, as it would the broker.
		{name: "the read panics", claim: legionPlanner, assignees: &standInAssignees{panics: "the reader's token is the-dispatch-token"},
			warned: true, errorText: "the assignee read panicked: a panic of type string"},
		{name: "a machine whose fleet names no person", machine: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			capture := &logtest.Recorder{}
			logtest.SetDefault(t, capture)
			m, _, _, _ := newFixture(t)
			if !c.noReader && c.assignees != nil {
				m.Assignees = c.assignees
				if c.assignees.block != nil {
					t.Cleanup(func() { close(c.assignees.block) })
				}
			}
			var enrollment string
			var key *ecdsa.PrivateKey
			if c.machine {
				m.VMServicesByAccount = map[string]string{fixtureMachineAccount: fixtureService}
				enrollment, key = newMachineEnrollment(t, m, fixtureService, fixtureMachineSubject, "machine-"+t.Name())
			} else {
				enrollment, key = newClaimPod(t, m, c.claim)
			}
			start := time.Now()
			req := askShared(t, m, enrollment, key)
			if took, most := time.Since(start), assigneeBound+3*time.Second; took > most {
				t.Errorf("Create took %s; want it back within %s, whatever Dispatch does", took, most)
			}
			requireListedFor(t, m, *req.RecordID, "", []string{alice, bob}, alice, bob)
			if c.assignees != nil && (c.claim == "" || c.claim == "legion-acme-controller") {
				if asked := c.assignees.keys(); len(asked) != 0 {
					t.Errorf("Dispatch was asked for %v; want no read for a claim that names no issue", asked)
				}
			}
			lines := loggedLines(t, capture, AssigneeUnreadMessage)
			if !c.warned {
				if len(lines) != 0 {
					t.Fatalf("Create logged %d %q lines %v; want none: nothing failed", len(lines), AssigneeUnreadMessage, lines)
				}
				return
			}
			if len(lines) != 1 {
				t.Fatalf("Create logged %d %q lines; want one", len(lines), AssigneeUnreadMessage)
			}
			line := lines[0]
			errorText, _ := line.attrs["error"].(string)
			if line.level != slog.LevelWarn || line.attrs["issue"] != "ACME-1" || line.attrs["enrollment"] != enrollment ||
				line.attrs["bound_reached"] != c.boundReached || !strings.Contains(errorText, c.errorText) {
				t.Fatalf("the %q line is %v at %s; want a WARN naming issue ACME-1, enrollment %s, bound_reached=%t and an error holding %q",
					AssigneeUnreadMessage, line.attrs, line.level, enrollment, c.boundReached, c.errorText)
			}
			if strings.Contains(fmt.Sprint(line.attrs), "the-dispatch-token") {
				t.Fatalf("the %q line is %v; want no part of what the read held", AssigneeUnreadMessage, line.attrs)
			}
		})
	}
}

// TestTheFirstResponsiblePersonWins: a session whose operator is alice and whose subject also
// reads as a machine of a fleet that names carol is alice's, the first step of the order.
func TestTheFirstResponsiblePersonWins(t *testing.T) {
	m, _, _, _ := newFixture(t)
	m.VMServicesByAccount = map[string]string{fixtureMachineAccount: fixtureService}
	m.VMPersonsByAccount = map[string]string{fixtureMachineAccount: carol}
	service, subject := fixtureService, fixtureMachineSubject
	session, key := insertEnrollment(t, m.Store, "session", "signin-"+t.Name(), new(alice), &service, &subject)
	req := askShared(t, m, session, key)
	requireListedFor(t, m, *req.RecordID, alice, []string{alice, carol, bob}, alice)
}

// TestIssueKeyOfClaim reads a Dispatch issue key out of a Legion claim token of an issue and
// nothing else. The token ends in its agent's name: the role of the agent's first step, which is
// one of Legion's six or a role the deployment defines (`checker`, `code-checker`, `janitor`), or a
// numbered name such as `implementer-2` for a second agent of one role, so any name of the role
// grammar names the issue, one of 32 characters included. The controller's token, a token with no
// name after its number, the name `controller` after an issue's number, a name outside the grammar
// and a token whose issue part is not one name no issue.
func TestIssueKeyOfClaim(t *testing.T) {
	for claim, want := range map[string]string{
		"legion-acme-acme-12-planner":                    "ACME-12",
		"legion-widget-widget-12-merger":                 "WIDGET-12",
		"legion-acme-acme7-3-architect":                  "ACME7-3",
		"legion-acme-acme-12-checker":                    "ACME-12",
		"legion-acme-acme-12-code-checker":               "ACME-12",
		"legion-acme-acme-12-janitor":                    "ACME-12",
		"legion-acme-acme-12-" + strings.Repeat("a", 32): "ACME-12",
		"legion-acme-acme-12-" + strings.Repeat("a", 33): "",
		"legion-acme-acme-12-Planner":                    "",
		"legion-acme-controller":                         "",
		"legion-acme-acme-12-controller":                 "",
		"legion-acme-acme-12":                            "",
		"legion-acme-12-planner":                         "",
		"":                                               "",
	} {
		if got := issueKeyOfClaim(claim); got != want {
			t.Errorf("issueKeyOfClaim(%q) = %q; want %q", claim, got, want)
		}
	}
}

// TestAPersonsOwnSecretIsListedForItsOwnerAlone: a request the policy sends to a person, the
// secret's owner, is that owner's alone whoever asks, as before. A Legion pod's request for it
// reads no assignee, since its approver is no sentinel.
func TestAPersonsOwnSecretIsListedForItsOwnerAlone(t *testing.T) {
	m, _, _, owner := newFixture(t)
	assignees := &standInAssignees{answer: dave}
	m.Assignees = assignees
	session, sessionKey := newEnrollment(t, m.Store, "session", "box-"+t.Name(), new(alice), nil)
	pod, podKey := newClaimPod(t, m, legionPlanner)
	for _, asker := range []struct {
		name, enrollment string
		key              *ecdsa.PrivateKey
	}{{"alice's session", session, sessionKey}, {"a Legion pod", pod, podKey}} {
		req, err := m.Create(context.Background(), asker.enrollment, signRequest(t, m, asker.key, "need the owner's key", "DEEL_API_KEY"), "")
		if err != nil || req.State != "pending" || req.RecordID == nil {
			t.Fatalf("Create(DEEL_API_KEY) as %s = %+v, %v; want it pending on %s", asker.name, req, err, owner)
		}
		requireListedFor(t, m, *req.RecordID, "", []string{owner, alice, dave, bob}, owner)
	}
	if asked := assignees.keys(); len(asked) != 0 {
		t.Errorf("Dispatch was asked for %v; want no read for a request a person owns", asked)
	}
}

// TestAnAgentTierPodRequestReadsNoAssignee: a request the policy grants at once waits on nobody,
// so a Legion pod's request for a shared agent-tier secret makes no Dispatch read.
func TestAnAgentTierPodRequestReadsNoAssignee(t *testing.T) {
	m, _, _, _ := newFixture(t)
	withSharedToken(t, m)
	assignees := &standInAssignees{answer: dave}
	m.Assignees = assignees
	pod, key := newClaimPod(t, m, legionPlanner)
	req, err := m.Create(context.Background(), pod, signRequest(t, m, key, "need the shared token", sharedToken), "")
	if err != nil || req.State != "granted" {
		t.Fatalf("Create(%s) = %+v, %v; want it granted at once", sharedToken, req, err)
	}
	if asked := assignees.keys(); len(asked) != 0 {
		t.Errorf("Dispatch was asked for %v; want no read for a request granted at once", asked)
	}
}

// TestAWithheldSharedAgentTierRequestIsListedForTheResponsiblePerson: a shared agent-tier secret
// is granted at once until a session's operator withholds it from that session; the session's next
// request for it then waits on record.AnyoneApprover, as a shared human-tier secret's does, and is
// listed by the same rule, for the person responsible for the agent. That is intended: the rule
// chooses whose Inbox lists a request anyone may decide, whatever the secret's tier, and here that
// person is the operator who withheld it. Anyone signed in still decides it.
func TestAWithheldSharedAgentTierRequestIsListedForTheResponsiblePerson(t *testing.T) {
	m, enr, key, operator := newFixture(t)
	withSharedToken(t, m)
	ctx := context.Background()
	auto, err := m.Create(ctx, enr, signRequest(t, m, key, "need it", sharedToken), "")
	if err != nil || auto.GrantID == nil {
		t.Fatalf("Create(%s) = %+v, %v; want granted at once", sharedToken, auto, err)
	}
	if err := m.RevokeByApprover(ctx, *auto.GrantID, operator, record.HumanActor(operator)); err != nil {
		t.Fatalf("RevokeByApprover: %v", err)
	}
	again, err := m.Create(ctx, enr, signRequest(t, m, key, "again", sharedToken), "")
	if err != nil || again.State != "pending" || again.RecordID == nil || recordApprover(t, m, again) != record.AnyoneApprover {
		t.Fatalf("Create(%s) after the withhold = %+v, %v; want it pending on %s", sharedToken, again, err, record.AnyoneApprover)
	}
	requireListedFor(t, m, *again.RecordID, operator, []string{operator, carol, bob}, operator)
	if dec, err := m.ApplyDecision(ctx, *again.RecordID, true, carol); err != nil || dec.GrantID == "" {
		t.Fatalf("ApplyDecision(%s) = %+v, %v; want anyone signed in to decide it", carol, dec, err)
	}
}
