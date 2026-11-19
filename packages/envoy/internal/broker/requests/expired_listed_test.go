package requests

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/broker/enroll"
	"github.com/sjawhar/envoy/internal/broker/machine"
	"github.com/sjawhar/envoy/internal/broker/record"
	"github.com/sjawhar/envoy/internal/logtest"
)

// bothStates asks PendingForApprover for the pending records and the expired ones alike, as
// Dispatch's list asks.
var bothStates = PendingStates{Pending: true, Expired: true}

// listedStates is login's list in the states asked, each row as "<record id>=<state>" in the
// list's own order.
func listedStates(t *testing.T, m *Machine, login string, states PendingStates) []string {
	t.Helper()
	rows, err := m.PendingForApprover(context.Background(), login, states)
	if err != nil {
		t.Fatalf("PendingForApprover(%s, %+v): %v", login, states, err)
	}
	out := make([]string, len(rows))
	for i, row := range rows {
		out[i] = listing(row.RecordID, row.State)
	}
	return out
}

// listedRow is login's row for recordID in the states asked; ok is false when the list does not
// hold it.
func listedRow(t *testing.T, m *Machine, login string, states PendingStates, recordID string) (row PendingSummary, ok bool) {
	t.Helper()
	rows, err := m.PendingForApprover(context.Background(), login, states)
	if err != nil {
		t.Fatalf("PendingForApprover(%s, %+v): %v", login, states, err)
	}
	for _, r := range rows {
		if r.RecordID == recordID {
			return r, true
		}
	}
	return PendingSummary{}, false
}

// listing is one "<record id>=<state>" row as listedStates writes it.
func listing(recordID, state string) string { return recordID + "=" + state }

// pastDeadline moves request requestID's pending_expires_at an hour into the past, as though its
// deadline had passed with no sweep run since, and answers the deadline it now has.
func pastDeadline(t *testing.T, m *Machine, requestID string) time.Time {
	t.Helper()
	var deadline time.Time
	if err := m.Store.Pool.QueryRow(context.Background(), `update requests set pending_expires_at = now() - interval '1 hour' where id=$1
		returning pending_expires_at`, requestID).Scan(&deadline); err != nil {
		t.Fatalf("move request %s past its deadline: %v", requestID, err)
	}
	return deadline
}

// requestRow is request requestID's whole row as text, which a change to any of its columns
// changes.
func requestRow(t *testing.T, m *Machine, requestID string) string {
	t.Helper()
	var row string
	if err := m.Store.Pool.QueryRow(context.Background(), `select r::text from requests r where r.id=$1`, requestID).Scan(&row); err != nil {
		t.Fatalf("read request %s: %v", requestID, err)
	}
	return row
}

// decisionTrail counts the events of record recordID and the grants of request requestID ("" for
// a machine login, which has no request).
func decisionTrail(t *testing.T, m *Machine, recordID, requestID string) (events, grants int) {
	t.Helper()
	if err := m.Store.Pool.QueryRow(context.Background(), `select (select count(*) from credential_request_events where record_id=$1),
		(select count(*) from grants where request_id::text=$2)`, recordID, requestID).Scan(&events, &grants); err != nil {
		t.Fatalf("count the events and grants of %s: %v", recordID, err)
	}
	return events, grants
}

// newSecretRequest creates a pending request for DEEL_API_KEY, the fixture operator's own
// human-tier secret, on a live enrollment of its own, so no two of them coalesce.
func newSecretRequest(t *testing.T, m *Machine, box string) Request {
	t.Helper()
	enr, key := newEnrollment(t, m.Store, "session", box+"-"+t.Name(), new(fixtureOperator), nil)
	req, err := m.Create(context.Background(), enr, signRequest(t, m, key, "need it on "+box, "DEEL_API_KEY"), "", nil)
	if err != nil || req.State != "pending" || req.RecordID == nil {
		t.Fatalf("Create on %s = %+v, %v; want it pending on a record", box, req, err)
	}
	return req
}

// insertMachineLogin writes a person's machine login for approver with the deadline expires, as
// machine.Service.LoginDevice writes one; its record is append-only, so its deadline is set as it
// is written. It answers the record's id; code is the login's user code.
func insertMachineLogin(t *testing.T, m *Machine, approver, host, code string, expires time.Time) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate the machine's key: %v", err)
	}
	compact, err := record.Sign(key, m.Audience, []record.AuthorizationDetail{{Type: record.KindLauncherCredential, Identifier: host}},
		"", approver, nil, time.Now())
	if err != nil {
		t.Fatalf("sign the machine login: %v", err)
	}
	body := record.Body{
		Request:         compact,
		Approver:        approver,
		Enrollment:      record.Enrollment{Kind: "-", RuntimeID: "-"},
		LifetimeSeconds: new(3600),
		RulesVersion:    "rules-v1",
		ExpiresAt:       expires.UTC().Truncate(time.Second),
		Code:            code,
	}
	if _, err := m.Store.Pool.Exec(context.Background(), `insert into credential_requests (id, body, kind, approver, code, expires_at)
		values ($1,$2,'launcher_credential',$3,$4,$5)`, body.ID(), body.Canonical(), body.Approver, code, body.ExpiresAt); err != nil {
		t.Fatalf("insert the machine login for %s: %v", host, err)
	}
	return body.ID()
}

// machineLogins is the machine-login service over m's store, as the broker wires it for decisions
// and for the sweep.
func machineLogins(m *Machine) *machine.Service {
	return &machine.Service{Store: m.Store, Audience: m.Audience, Skew: m.Skew}
}

// linesOf are the lines capture holds whose message is message, each as its attributes and its
// level under "level".
func linesOf(capture *logtest.Recorder, message string) []map[string]string {
	var lines []map[string]string
	for _, r := range capture.Records() {
		if r.Message != message {
			continue
		}
		attrs := map[string]string{"level": r.Level.String()}
		r.Attrs(func(a slog.Attr) bool { attrs[a.Key] = a.Value.String(); return true })
		lines = append(lines, attrs)
	}
	return lines
}

// TestExpiredSecretRequestStaysListedUntilDismissed is check c-expired-request-listed's sequence
// for a secret request. Two requests wait on one approver, both enrollments live, and one is moved
// past its deadline with no sweep run. The list that asks for both states holds both, the moved one
// expired and the other pending, the pending one first; approving or denying the moved one is
// ErrExpired and changes nothing. After the sweep both are still listed as they were, and the other
// request is untouched. The approver's dismissal takes the moved one out, and a second dismissal
// answers the first one's values and writes nothing; each is logged once. The list that asks for
// no state holds the overdue request before the sweep and not after it, as it always did.
func TestExpiredSecretRequestStaysListedUntilDismissed(t *testing.T) {
	capture := &logtest.Recorder{}
	logtest.SetDefault(t, capture)
	m, _, _, approver := newFixture(t)
	ctx := context.Background()
	moved := newSecretRequest(t, m, "box-a")
	other := newSecretRequest(t, m, "box-b")
	deadline := pastDeadline(t, m, moved.ID)
	movedID, otherID := *moved.RecordID, *other.RecordID

	want := []string{listing(otherID, "pending"), listing(movedID, "expired")}
	if got := listedStates(t, m, approver, bothStates); !slices.Equal(got, want) {
		t.Fatalf("the list of both states before the sweep = %q; want %q", got, want)
	}
	if row, ok := listedRow(t, m, approver, bothStates, movedID); !ok || !row.ExpiresAt.Equal(deadline) {
		t.Fatalf("the overdue row = %+v (listed %v); want its deadline %s, its request's pending_expires_at", row, ok, deadline)
	}
	if row, ok := listedRow(t, m, approver, bothStates, otherID); !ok || !row.ExpiresAt.After(time.Now()) {
		t.Fatalf("the live row = %+v (listed %v); want a deadline still ahead", row, ok)
	}
	if got := listedStates(t, m, approver, PendingStates{Pending: true}); !slices.Equal(got, want[:1]) {
		t.Fatalf("the list of pending records = %q; want %q", got, want[:1])
	}
	if got := listedStates(t, m, approver, PendingStates{Expired: true}); !slices.Equal(got, want[1:]) {
		t.Fatalf("the list of expired records = %q; want %q", got, want[1:])
	}
	if got := listedStates(t, m, approver, PendingStates{}); !slices.Contains(got, listing(movedID, "expired")) || len(got) != 2 {
		t.Fatalf("the list of no state before the sweep = %q; want both records, the overdue one read expired", got)
	}

	movedRow, otherRow := requestRow(t, m, moved.ID), requestRow(t, m, other.ID)
	for _, approve := range []bool{true, false} {
		if dec, err := m.ApplyDecision(ctx, movedID, approve, approver); !errors.Is(err, ErrExpired) {
			t.Fatalf("ApplyDecision(approve=%v) of the overdue request = %+v, %v; want ErrExpired", approve, dec, err)
		}
	}
	if got := requestRow(t, m, moved.ID); got != movedRow {
		t.Fatalf("the refused decisions changed the overdue request from %s to %s", movedRow, got)
	}
	if events, grants := decisionTrail(t, m, movedID, moved.ID); events != 0 || grants != 0 {
		t.Fatalf("the refused decisions wrote %d events and %d grants; want none", events, grants)
	}

	if n, err := m.ExpirePending(ctx, time.Now()); err != nil || n != 1 {
		t.Fatalf("ExpirePending = %d, %v; want the overdue request alone expired", n, err)
	}
	if got := listedStates(t, m, approver, bothStates); !slices.Equal(got, want) {
		t.Fatalf("the list of both states after the sweep = %q; want %q", got, want)
	}
	if got := requestRow(t, m, other.ID); got != otherRow {
		t.Fatalf("the sweep changed the live request from %s to %s", otherRow, got)
	}
	if got := listedStates(t, m, approver, PendingStates{}); !slices.Equal(got, want[:1]) {
		t.Fatalf("the list of no state after the sweep = %q; want only %q", got, want[:1])
	}

	first, err := m.Dismiss(ctx, movedID, approver)
	if err != nil || !first.Wrote || first.RecordID != movedID || first.DismissedBy != approver || first.DismissedAt.IsZero() {
		t.Fatalf("Dismiss by the approver = %+v, %v; want this call's dismissal by %s", first, err, approver)
	}
	if got := listedStates(t, m, approver, bothStates); !slices.Equal(got, want[:1]) {
		t.Fatalf("the list of both states after the dismissal = %q; want only %q", got, want[:1])
	}
	second, err := m.Dismiss(ctx, movedID, approver)
	if err != nil || second.Wrote || second.RecordID != first.RecordID || second.DismissedBy != first.DismissedBy || !second.DismissedAt.Equal(first.DismissedAt) {
		t.Fatalf("a second Dismiss = %+v, %v; want the first one's values %+v, written by neither call again", second, err, first)
	}
	var rows int
	if err := m.Store.Pool.QueryRow(ctx, `select count(*) from credential_request_dismissals`).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("credential_request_dismissals holds %d rows (%v); want the one", rows, err)
	}
	lines := linesOf(capture, DismissedMessage)
	wantLines := []string{"true", "false"}
	if len(lines) != len(wantLines) {
		t.Fatalf("%d %q lines after two dismissals; want one each: %v", len(lines), DismissedMessage, lines)
	}
	for i, line := range lines {
		if line["level"] != slog.LevelInfo.String() || line["record_id"] != movedID || line["kind"] != record.KindAgentSecret ||
			line["login"] != approver || line["wrote"] != wantLines[i] {
			t.Fatalf("dismissal line %d = %v; want INFO naming record_id %s, kind %s, login %s and wrote=%s", i, line, movedID, record.KindAgentSecret, approver, wantLines[i])
		}
	}
}

// TestExpiredMachineLoginStaysListedUntilDismissed is the same sequence for a machine login, as
// Verification v2 asks: two machine logins wait on one approver, one written with a deadline
// already passed. Its decisions are machine.ErrLoginExpired, the machine-login form of the refusal
// the check calls ErrExpired, and write no event; the sweep is machine.Service.ExpirePending.
func TestExpiredMachineLoginStaysListedUntilDismissed(t *testing.T) {
	m, _, _, approver := newFixture(t)
	ctx := context.Background()
	deadline := time.Now().Add(-time.Hour)
	expiredID := insertMachineLogin(t, m, approver, "example-host-a", "BCDF-GHJK", deadline)
	liveID := insertMachineLogin(t, m, approver, "example-host-b", "LMNP-QRST", time.Now().Add(time.Hour))
	logins := machineLogins(m)

	want := []string{listing(liveID, "pending"), listing(expiredID, "expired")}
	if got := listedStates(t, m, approver, bothStates); !slices.Equal(got, want) {
		t.Fatalf("the list of both states before the sweep = %q; want %q", got, want)
	}
	if row, ok := listedRow(t, m, approver, bothStates, expiredID); !ok || !row.ExpiresAt.Equal(deadline.UTC().Truncate(time.Second)) ||
		row.Kind != record.KindLauncherCredential || !slices.Equal(row.Identifiers, []string{"example-host-a"}) {
		t.Fatalf("the expired login's row = %+v (listed %v); want the host and its deadline %s", row, ok, deadline)
	}
	if got := listedStates(t, m, approver, PendingStates{}); !slices.Contains(got, listing(expiredID, "expired")) || len(got) != 2 {
		t.Fatalf("the list of no state before the sweep = %q; want both logins, the overdue one read expired", got)
	}

	for _, approve := range []bool{true, false} {
		if state, credential, err := logins.ApplyDecision(ctx, expiredID, approve, approver, "BCDF-GHJK"); !errors.Is(err, machine.ErrLoginExpired) {
			t.Fatalf("ApplyDecision(approve=%v) of the overdue login = %q, %q, %v; want machine.ErrLoginExpired", approve, state, credential, err)
		}
	}
	if events, _ := decisionTrail(t, m, expiredID, ""); events != 0 {
		t.Fatalf("the refused decisions wrote %d events; want none", events)
	}
	var credentials int
	if err := m.Store.Pool.QueryRow(ctx, `select count(*) from launcher_credentials where record_id=$1`, expiredID).Scan(&credentials); err != nil || credentials != 0 {
		t.Fatalf("the refused decisions minted %d credentials (%v); want none", credentials, err)
	}

	if err := logins.ExpirePending(ctx, time.Now()); err != nil {
		t.Fatalf("machine.Service.ExpirePending: %v", err)
	}
	if events, _ := decisionTrail(t, m, expiredID, ""); events != 1 {
		t.Fatalf("the sweep wrote %d events for the overdue login; want its expired event", events)
	}
	if events, _ := decisionTrail(t, m, liveID, ""); events != 0 {
		t.Fatalf("the sweep wrote %d events for the live login; want none", events)
	}
	if got := listedStates(t, m, approver, bothStates); !slices.Equal(got, want) {
		t.Fatalf("the list of both states after the sweep = %q; want %q", got, want)
	}
	if got := listedStates(t, m, approver, PendingStates{}); !slices.Equal(got, want[:1]) {
		t.Fatalf("the list of no state after the sweep = %q; want only %q", got, want[:1])
	}

	first, err := m.Dismiss(ctx, expiredID, approver)
	if err != nil || !first.Wrote || first.DismissedBy != approver {
		t.Fatalf("Dismiss by the approver = %+v, %v; want this call's dismissal by %s", first, err, approver)
	}
	if got := listedStates(t, m, approver, bothStates); !slices.Equal(got, want[:1]) {
		t.Fatalf("the list of both states after the dismissal = %q; want only %q", got, want[:1])
	}
	second, err := m.Dismiss(ctx, expiredID, approver)
	if err != nil || second.Wrote || second.DismissedBy != first.DismissedBy || !second.DismissedAt.Equal(first.DismissedAt) {
		t.Fatalf("a second Dismiss = %+v, %v; want the first one's values %+v", second, err, first)
	}
}

// TestExpiredRowsAreListedAfterThePendingOnesEachNewestFirst pins the list's order for a caller
// that asks for both states (decision 8): every pending row before every expired one, whatever
// their times, and each newest first, so a page cut from the front never pushes a record a person
// can decide off it. The records are written pending, expired, pending, expired, so the order by
// time alone would interleave them.
func TestExpiredRowsAreListedAfterThePendingOnesEachNewestFirst(t *testing.T) {
	m, _, _, approver := newFixture(t)
	olderPending := newSecretRequest(t, m, "box-a")
	olderExpired := newSecretRequest(t, m, "box-b")
	newerPending := newSecretRequest(t, m, "box-c")
	newerExpired := newSecretRequest(t, m, "box-d")
	pastDeadline(t, m, olderExpired.ID)
	pastDeadline(t, m, newerExpired.ID)

	want := []string{
		listing(*newerPending.RecordID, "pending"), listing(*olderPending.RecordID, "pending"),
		listing(*newerExpired.RecordID, "expired"), listing(*olderExpired.RecordID, "expired"),
	}
	if got := listedStates(t, m, approver, bothStates); !slices.Equal(got, want) {
		t.Fatalf("the list of both states = %q; want %q", got, want)
	}
	wantByTime := []string{want[2], want[0], want[3], want[1]}
	if got := listedStates(t, m, approver, PendingStates{}); !slices.Equal(got, wantByTime) {
		t.Fatalf("the list of no state = %q; want every record newest first, as before states, %q", got, wantByTime)
	}
}

// TestExpiredDismissRefusesWhatIsNotAnExpiredRecordOfTheLogin pins Dismiss's refusals: an id that
// names no record is pgx.ErrNoRows; a login whose list does not hold the record, an empty login
// and the anyone sentinel are record.ErrNotApprover; a record whose deadline is still ahead is
// ErrNotExpired; and a decided record is ErrTerminal, whether its deadline has passed since or
// not. None of them writes a dismissal.
func TestExpiredDismissRefusesWhatIsNotAnExpiredRecordOfTheLogin(t *testing.T) {
	m, _, _, approver := newFixture(t)
	ctx := context.Background()

	if d, err := m.Dismiss(ctx, strings.Repeat("0", 64), approver); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("Dismiss of an id no record has = %+v, %v; want pgx.ErrNoRows", d, err)
	}
	overdue := newSecretRequest(t, m, "box-a")
	pastDeadline(t, m, overdue.ID)
	for _, login := range []string{otherPerson, "", "  ", record.AnyoneApprover, " ANYONE "} {
		if d, err := m.Dismiss(ctx, *overdue.RecordID, login); !errors.Is(err, record.ErrNotApprover) {
			t.Fatalf("Dismiss by %q of a record that waits on %s = %+v, %v; want record.ErrNotApprover", login, approver, d, err)
		}
	}
	live := newSecretRequest(t, m, "box-b")
	if d, err := m.Dismiss(ctx, *live.RecordID, approver); !errors.Is(err, ErrNotExpired) {
		t.Fatalf("Dismiss of a request before its deadline = %+v, %v; want ErrNotExpired", d, err)
	}
	liveLogin := insertMachineLogin(t, m, approver, "example-host", "BCDF-GHJK", time.Now().Add(time.Hour))
	if d, err := m.Dismiss(ctx, liveLogin, approver); !errors.Is(err, ErrNotExpired) {
		t.Fatalf("Dismiss of a machine login before its deadline = %+v, %v; want ErrNotExpired", d, err)
	}
	for _, approve := range []bool{false, true} {
		decided := newSecretRequest(t, m, "box-decided-"+map[bool]string{true: "approved", false: "denied"}[approve])
		if dec, err := m.ApplyDecision(ctx, *decided.RecordID, approve, approver); err != nil {
			t.Fatalf("ApplyDecision(approve=%v) = %+v, %v", approve, dec, err)
		}
		if d, err := m.Dismiss(ctx, *decided.RecordID, approver); !errors.Is(err, ErrTerminal) {
			t.Fatalf("Dismiss of a request decided (approve=%v) = %+v, %v; want ErrTerminal", approve, d, err)
		}
		pastDeadline(t, m, decided.ID)
		if d, err := m.Dismiss(ctx, *decided.RecordID, approver); !errors.Is(err, ErrTerminal) {
			t.Fatalf("Dismiss of a request decided (approve=%v) before a deadline since passed = %+v, %v; want ErrTerminal", approve, d, err)
		}
	}
	var rows int
	if err := m.Store.Pool.QueryRow(ctx, `select count(*) from credential_request_dismissals`).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("the refused dismissals left %d rows (%v); want none", rows, err)
	}
	if !IsRefusal(ErrNotExpired) {
		t.Fatal("ErrNotExpired is not among the package's refusals")
	}
}

// TestExpiredSharedRequestOneLoginDismissesLeavesEveryList pins decision 3: a request for a shared
// secret waits on anyone, so every signed-in person's list holds it once it expires, and the
// first person's dismissal takes it out of every list. A second person's dismissal answers the
// first one's.
func TestExpiredSharedRequestOneLoginDismissesLeavesEveryList(t *testing.T) {
	m, enr, key, _ := newFixture(t)
	ctx := context.Background()
	shared, err := m.Create(ctx, enr, signRequest(t, m, key, "need the shared one", "SHARED_KEY"), "", nil)
	if err != nil || shared.State != "pending" || shared.RecordID == nil {
		t.Fatalf("Create(SHARED_KEY) = %+v, %v; want it pending on a record", shared, err)
	}
	pastDeadline(t, m, shared.ID)
	const bob, carol = "bob@example.com", "carol@example.com"
	want := []string{listing(*shared.RecordID, "expired")}
	for _, person := range []string{bob, carol} {
		if got := listedStates(t, m, person, bothStates); !slices.Equal(got, want) {
			t.Fatalf("%s's list = %q; want %q", person, got, want)
		}
	}
	first, err := m.Dismiss(ctx, *shared.RecordID, "Bob@Example.com")
	if err != nil || !first.Wrote || first.DismissedBy != bob {
		t.Fatalf("Dismiss by bob = %+v, %v; want this call's dismissal by %s", first, err, bob)
	}
	for _, person := range []string{bob, carol} {
		if got := listedStates(t, m, person, bothStates); len(got) != 0 {
			t.Fatalf("%s's list after bob dismissed = %q; want it empty", person, got)
		}
	}
	if again, err := m.Dismiss(ctx, *shared.RecordID, carol); err != nil || again.Wrote || again.DismissedBy != bob || !again.DismissedAt.Equal(first.DismissedAt) {
		t.Fatalf("Dismiss by carol after bob = %+v, %v; want bob's dismissal %+v", again, err, first)
	}
}

// TestExpiredRequestWhoseSessionEndedIsListedAndDismissed pins decision 6 for both ways a session
// ends its request before any expiry sweep: its own cancel (Machine.Cancel), and its enrollment's
// end (enroll.Service.EndLapsed), each of which cancels a pending request whatever its deadline.
// A request cancelled after its deadline had passed is still listed expired, a decision of it
// is ErrTerminal, its record reads cancelled, and the approver dismisses it like any other.
func TestExpiredRequestWhoseSessionEndedIsListedAndDismissed(t *testing.T) {
	ends := map[string]func(t *testing.T, m *Machine, enrollmentID, requestID string){
		"Cancel": func(t *testing.T, m *Machine, enrollmentID, requestID string) {
			if err := m.Cancel(context.Background(), requestID, enrollmentID); err != nil {
				t.Fatalf("Cancel: %v", err)
			}
		},
		"EndLapsed": func(t *testing.T, m *Machine, enrollmentID, _ string) {
			ctx := context.Background()
			if _, err := m.Store.Pool.Exec(ctx, `update enrollments set lease_expires_at = now() - interval '1 second' where id=$1`, enrollmentID); err != nil {
				t.Fatalf("lapse the lease: %v", err)
			}
			if ended, err := (&enroll.Service{Store: m.Store}).EndLapsed(ctx); err != nil || len(ended) != 1 {
				t.Fatalf("EndLapsed = %+v, %v; want the one lapsed enrollment ended", ended, err)
			}
		},
	}
	for name, end := range ends {
		t.Run(name, func(t *testing.T) {
			m, enr, key, approver := newFixture(t)
			ctx := context.Background()
			req, err := m.Create(ctx, enr, signRequest(t, m, key, "need it", "DEEL_API_KEY"), "", nil)
			if err != nil || req.RecordID == nil {
				t.Fatalf("Create = %+v, %v; want it pending on a record", req, err)
			}
			pastDeadline(t, m, req.ID)
			end(t, m, enr, req.ID)
			if got, err := m.Get(ctx, req.ID); err != nil || got.State != "cancelled" {
				t.Fatalf("the request after its session ended = %+v, %v; want cancelled", got, err)
			}

			want := []string{listing(*req.RecordID, "expired")}
			if got := listedStates(t, m, approver, bothStates); !slices.Equal(got, want) {
				t.Fatalf("the list of both states = %q; want %q", got, want)
			}
			if got := listedStates(t, m, approver, PendingStates{}); len(got) != 0 {
				t.Fatalf("the list of no state = %q; want it empty, as before states", got)
			}
			for _, approve := range []bool{true, false} {
				if dec, err := m.ApplyDecision(ctx, *req.RecordID, approve, approver); !errors.Is(err, ErrTerminal) {
					t.Fatalf("ApplyDecision(approve=%v) = %+v, %v; want ErrTerminal", approve, dec, err)
				}
			}
			if detail, err := m.ReadRecord(ctx, *req.RecordID); err != nil || detail.State != "cancelled" {
				t.Fatalf("ReadRecord = %q, %v; want cancelled", detail.State, err)
			}
			if d, err := m.Dismiss(ctx, *req.RecordID, approver); err != nil || !d.Wrote {
				t.Fatalf("Dismiss = %+v, %v; want it dismissed", d, err)
			}
			if got := listedStates(t, m, approver, bothStates); len(got) != 0 {
				t.Fatalf("the list of both states after the dismissal = %q; want it empty", got)
			}
		})
	}
}

// TestExpiredRequestCancelledBeforeItsDeadlineIsNotListed is decision 6's other half: a request
// its session cancelled while its deadline was still ahead never reached the deadline, so no list
// holds it and it has nothing to dismiss.
func TestExpiredRequestCancelledBeforeItsDeadlineIsNotListed(t *testing.T) {
	m, enr, key, approver := newFixture(t)
	ctx := context.Background()
	req, err := m.Create(ctx, enr, signRequest(t, m, key, "need it", "DEEL_API_KEY"), "", nil)
	if err != nil || req.RecordID == nil {
		t.Fatalf("Create = %+v, %v; want it pending on a record", req, err)
	}
	if err := m.Cancel(ctx, req.ID, enr); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	for _, states := range []PendingStates{bothStates, {Expired: true}, {}} {
		if got := listedStates(t, m, approver, states); len(got) != 0 {
			t.Fatalf("the list of %+v = %q; want it empty", states, got)
		}
	}
	if d, err := m.Dismiss(ctx, *req.RecordID, approver); !errors.Is(err, ErrTerminal) {
		t.Fatalf("Dismiss = %+v, %v; want ErrTerminal", d, err)
	}
}

// TestExpiredRecordLeftOutOfAStatesListIsNotLogged pins leaveOut's one exception: a list that
// asks for states reads an expired record until somebody dismisses it, and nobody can dismiss a
// record no list shows, so an expired record whose signed request does not verify for this broker
// is left out with no line, list after list. A pending record left out is logged once, as it
// always was.
func TestExpiredRecordLeftOutOfAStatesListIsNotLogged(t *testing.T) {
	capture := &logtest.Recorder{}
	logtest.SetDefault(t, capture)
	m, enr, key, approver := newFixture(t)
	ctx := context.Background()

	current := m.Audience
	m.Audience = "https://the-old-public-url.example"
	live, err := m.Create(ctx, enr, signRequest(t, m, key, "asked before the URL moved", "DEEL_API_KEY"), "", nil)
	if err != nil || live.RecordID == nil {
		t.Fatalf("Create under the old audience = %+v, %v", live, err)
	}
	expired := insertMachineLogin(t, m, approver, "example-host", "BCDF-GHJK", time.Now().Add(-time.Hour))
	m.Audience = current

	for range 2 {
		if got := listedStates(t, m, approver, bothStates); len(got) != 0 {
			t.Fatalf("the list of both states = %q; want neither record that does not verify", got)
		}
	}
	warned := map[string]int{}
	for _, line := range linesOf(capture, PendingLeftOutMessage) {
		warned[line["record_id"]]++
	}
	if warned[*live.RecordID] != 1 || warned[expired] != 0 {
		t.Fatalf("%q lines after two lists = %v; want one for the pending record %s and none for the expired %s", PendingLeftOutMessage, warned, *live.RecordID, expired)
	}
}

// TestReadRecordReadsAnOverdueRecordExpiredBeforeTheSweep pins t4: a record still pending past its
// deadline reads expired with no decision, before the sweep has ended it, for a secret request and
// for a machine login, and a record before its deadline reads pending. A secret request's
// ExpiresAt is its request's pending_expires_at, the deadline its decision compares. Once the
// sweep has run, the secret request's expired event says the same.
func TestReadRecordReadsAnOverdueRecordExpiredBeforeTheSweep(t *testing.T) {
	m, _, _, approver := newFixture(t)
	ctx := context.Background()
	overdue := newSecretRequest(t, m, "box-a")
	live := newSecretRequest(t, m, "box-b")
	deadline := pastDeadline(t, m, overdue.ID)
	overdueLogin := insertMachineLogin(t, m, approver, "example-host-a", "BCDF-GHJK", time.Now().Add(-time.Hour))
	liveLogin := insertMachineLogin(t, m, approver, "example-host-b", "LMNP-QRST", time.Now().Add(time.Hour))

	for _, c := range []struct {
		what, recordID, state string
	}{
		{"the overdue secret request", *overdue.RecordID, "expired"},
		{"the live secret request", *live.RecordID, "pending"},
		{"the overdue machine login", overdueLogin, "expired"},
		{"the live machine login", liveLogin, "pending"},
	} {
		detail, err := m.ReadRecord(ctx, c.recordID)
		if err != nil || detail.State != c.state || detail.Decided != nil {
			t.Fatalf("ReadRecord(%s) = state %q, decided %+v, %v; want %s with no decision", c.what, detail.State, detail.Decided, err, c.state)
		}
	}
	if detail, err := m.ReadRecord(ctx, *overdue.RecordID); err != nil || !detail.ExpiresAt.Equal(deadline) {
		t.Fatalf("ReadRecord(the overdue secret request).ExpiresAt = %s, %v; want its request's pending_expires_at %s", detail.ExpiresAt, err, deadline)
	}
	if _, err := m.ExpirePending(ctx, time.Now()); err != nil {
		t.Fatalf("ExpirePending: %v", err)
	}
	if detail, err := m.ReadRecord(ctx, *overdue.RecordID); err != nil || detail.State != "expired" || detail.Decided == nil || detail.Decided.Event != "expired" {
		t.Fatalf("ReadRecord(the overdue secret request) after the sweep = %q, %+v, %v; want its expired event", detail.State, detail.Decided, err)
	}
}

// TestCoalescingSkipsARequestPastItsDeadline pins pendingRequests' deadline: a request made again
// once the first one's deadline has passed, with no sweep run, opens a record of its own rather
// than joining one every decision refuses, and the approver's list holds it pending beside the
// expired one.
func TestCoalescingSkipsARequestPastItsDeadline(t *testing.T) {
	m, enr, key, approver := newFixture(t)
	ctx := context.Background()
	first, err := m.Create(ctx, enr, signRequest(t, m, key, "need it", "DEEL_API_KEY"), "", nil)
	if err != nil || first.RecordID == nil {
		t.Fatalf("Create(first) = %+v, %v", first, err)
	}
	pastDeadline(t, m, first.ID)
	again, err := m.Create(ctx, enr, signRequest(t, m, key, "need it", "DEEL_API_KEY"), "", nil)
	if err != nil || again.Coalesced || again.State != "pending" || again.RecordID == nil || again.ID == first.ID || *again.RecordID == *first.RecordID {
		t.Fatalf("Create(again) past the first one's deadline = %+v, %v; want a new pending request on a record of its own, not %s", again, err, *first.RecordID)
	}
	want := []string{listing(*again.RecordID, "pending"), listing(*first.RecordID, "expired")}
	if got := listedStates(t, m, approver, bothStates); !slices.Equal(got, want) {
		t.Fatalf("the list of both states = %q; want %q", got, want)
	}
}
