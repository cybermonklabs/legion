// packages/envoy/internal/broker/requests/judge_decision_test.go
// The model judge's own decisions against the real database: what an approval of its leaves on
// the record, the request and the grant, what a verdict that decides nothing leaves, what the
// airlock rules refuse it, and what happens when a person and the judge both answer one request.
package requests

import (
	"crypto/ecdsa"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/broker/airlock"
	"github.com/sjawhar/envoy/internal/broker/policy"
	"github.com/sjawhar/envoy/internal/broker/policy/policytest"
	"github.com/sjawhar/envoy/internal/broker/record"
)

// judgeModel is the model deciding in these tests; every judge actor they expect is
// record.JudgeActor of it.
const judgeModel = "claude-opus-4-6"

// newJudgeFixture is newFixture with airlock rules behind it that let the judge decide up to 900
// seconds of DEEL_API_KEY, the operator's own human-tier secret, and of SHARED_KEY, the shared
// one: the two records' approvers are a person's login and record.AnyoneApprover, which is the
// difference the grant's approver turns on. The chain verifier is rebuilt against those rules,
// since it holds a judge's approval to the same bound (record.ChainVerifier.JudgeSeconds).
func newJudgeFixture(t *testing.T) (*Machine, string, *ecdsa.PrivateKey) {
	t.Helper()
	m, enrollmentID, key, _ := newFixture(t)
	m.Airlock = airlock.Rules{Secrets: []airlock.SecretRule{
		{Name: "DEEL_API_KEY", Hosts: []string{"api.deel.example"}, Judge: new(uint32(900))},
		{Name: "SHARED_KEY", Hosts: []string{"shared.example"}, Judge: new(uint32(900))},
	}}
	m.Chain = NewChainVerifier(m.Store, testAudience, time.Minute, m.Airlock)
	return m, enrollmentID, key
}

// signJudgeable signs an agent_secret request object over names with the reason and the lifetime
// the judge's rules read: a reason naming a Dispatch issue and a lifetime inside the entry's
// judge bound are two of the three things record.MayJudge asks for, and the tests below move each
// of them.
func signJudgeable(t *testing.T, m *Machine, key *ecdsa.PrivateKey, reason string, lifetime int, names ...string) string {
	t.Helper()
	details := make([]record.AuthorizationDetail, len(names))
	for i, n := range names {
		details[i] = record.AuthorizationDetail{Type: record.KindAgentSecret, Identifier: n, Actions: []string{"inject"}}
	}
	compact, err := record.Sign(key, m.Audience, details, reason, "", &lifetime, time.Now())
	if err != nil {
		t.Fatalf("record.Sign: %v", err)
	}
	return compact
}

// pendingJudgeable creates one request for names that waits on a person, the state the judge
// decides from.
func pendingJudgeable(t *testing.T, m *Machine, enrollmentID string, key *ecdsa.PrivateKey, reason string, lifetime int, names ...string) Request {
	t.Helper()
	req, err := m.Create(t.Context(), enrollmentID, signJudgeable(t, m, key, reason, lifetime, names...), "")
	if err != nil || req.RecordID == nil || req.State != "pending" {
		t.Fatalf("Create(%v) = %+v, %v; want a pending request", names, req, err)
	}
	return req
}

// decided is everything one record's decision wrote that a judge's approval must get right: its
// terminal decision events, the login and actor of the one it has (login nil for the judge), the
// request's own state and decided_by, and the approver of every grant the request produced.
type decided struct {
	events    int
	login     *string
	actor     string
	state     string
	decidedBy string
	approvers []string
}

func readDecided(t *testing.T, m *Machine, recordID string) decided {
	t.Helper()
	var d decided
	if err := m.Store.Pool.QueryRow(t.Context(),
		`select count(*), min(login), coalesce(min(actor),'') from credential_request_events where record_id=$1`, recordID).
		Scan(&d.events, &d.login, &d.actor); err != nil {
		t.Fatalf("read decision events: %v", err)
	}
	if err := m.Store.Pool.QueryRow(t.Context(),
		`select state, coalesce(decided_by,'') from requests where record_id=$1`, recordID).
		Scan(&d.state, &d.decidedBy); err != nil {
		t.Fatalf("read request row: %v", err)
	}
	rows, err := m.Store.Pool.Query(t.Context(),
		`select g.approver from grants g join requests r on r.id=g.request_id where r.record_id=$1 order by g.id`, recordID)
	if err != nil {
		t.Fatalf("read grants: %v", err)
	}
	if d.approvers, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
		t.Fatalf("read grants: %v", err)
	}
	return d
}

// judgedRows is every request.judged audit row of a record's request, as actor, the grant it
// names (empty for none), the verdict its detail carries and what that detail says the
// transaction applied ("true", "false", or "" for a row with no such key).
type judgedRow struct {
	actor   string
	grantID string
	verdict string
	applied string
}

func readJudged(t *testing.T, m *Machine, recordID string) []judgedRow {
	t.Helper()
	rows, err := m.Store.Pool.Query(t.Context(),
		`select a.actor, coalesce(a.grant_id::text,''), coalesce(a.detail->>'verdict',''), coalesce(a.detail->>'applied','') from audit a
			join requests r on r.id=a.request_id where r.record_id=$1 and a.kind='request.judged' order by a.id`, recordID)
	if err != nil {
		t.Fatalf("read request.judged rows: %v", err)
	}
	judged, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (judgedRow, error) {
		var j judgedRow
		return j, row.Scan(&j.actor, &j.grantID, &j.verdict, &j.applied)
	})
	if err != nil {
		t.Fatalf("read request.judged rows: %v", err)
	}
	return judged
}

// auditKinds is the audit kinds one record's request carries, in the order they were written.
func auditKinds(t *testing.T, m *Machine, recordID string) []string {
	t.Helper()
	rows, err := m.Store.Pool.Query(t.Context(),
		`select a.kind from audit a join requests r on r.id=a.request_id where r.record_id=$1 order by a.id`, recordID)
	if err != nil {
		t.Fatalf("read audit kinds: %v", err)
	}
	kinds, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read audit kinds: %v", err)
	}
	return kinds
}

// TestTheJudgesApprovalGrantsUnderTheRecordsOwnApprover is D1 at the database: the judge's
// approval writes one decision event with no login and the judge's actor, records that actor as
// having decided the request, and mints a grant whose approver is the record's own — the owner's
// login for an owned secret, record.AnyoneApprover for a shared one — never the judge. The
// verdict that produced it is in the same transaction (D11), naming the grant it granted, and the
// chain the release re-checks verifies under the judge's arm.
func TestTheJudgesApprovalGrantsUnderTheRecordsOwnApprover(t *testing.T) {
	for name, c := range map[string]struct{ secret, approver string }{
		"an owned secret": {"DEEL_API_KEY", fixtureOperator},
		"a shared secret": {"SHARED_KEY", record.AnyoneApprover},
	} {
		t.Run(name, func(t *testing.T) {
			m, enr, key := newJudgeFixture(t)
			req := pendingJudgeable(t, m, enr, key, "INFRA-1353 wants the key", 600, c.secret)

			dec, err := m.ApplyJudgeDecision(t.Context(), *req.RecordID, judgeModel, map[string]any{"verdict": "approve", "issue": "INFRA-1353"})
			if err != nil || dec.State != "granted" || dec.GrantID == "" {
				t.Fatalf("ApplyJudgeDecision = %+v, %v; want a granted decision", dec, err)
			}

			got := readDecided(t, m, *req.RecordID)
			judge := record.JudgeActor(judgeModel)
			if got.events != 1 || got.login != nil || got.actor != judge {
				t.Errorf("decision events = %d rows, login %v, actor %q; want one row, no login, actor %q", got.events, got.login, got.actor, judge)
			}
			if got.state != "granted" || got.decidedBy != judge {
				t.Errorf("request row = %q decided by %q; want granted, decided by %q", got.state, got.decidedBy, judge)
			}
			if len(got.approvers) != 1 || got.approvers[0] != c.approver {
				t.Errorf("grant approvers = %q; want one grant approved by %q, the record's own approver", got.approvers, c.approver)
			}

			judged := readJudged(t, m, *req.RecordID)
			if len(judged) != 1 || judged[0].actor != judge || judged[0].grantID != dec.GrantID || judged[0].verdict != "approve" {
				t.Errorf("request.judged rows = %+v; want one by %q naming grant %s and the verdict", judged, judge, dec.GrantID)
			}
			if kinds := auditKinds(t, m, *req.RecordID); len(kinds) != 3 || kinds[1] != "request.granted" || kinds[2] != judgedKind {
				t.Errorf("audit kinds = %q; want the creation, then request.granted and request.judged of the one approval", kinds)
			}
			if err := m.VerifyChain(t.Context(), dec.GrantID); err != nil {
				t.Errorf("VerifyChain of the judge's grant = %v; want it to verify", err)
			}
		})
	}
}

// TestAVerdictThatDecidesNothingWritesOnlyItself is the refer verdict and every approval the
// state machine refused: LogJudged keeps what the judge answered and changes nothing else — no
// decision event, no grant, and the request still waiting for the person who decides it today.
func TestAVerdictThatDecidesNothingWritesOnlyItself(t *testing.T) {
	m, enr, key := newJudgeFixture(t)
	req := pendingJudgeable(t, m, enr, key, "INFRA-1353 wants the key", 600, "DEEL_API_KEY")

	if err := m.LogJudged(t.Context(), *req.RecordID, judgeModel, map[string]any{"verdict": "refer", "why": "the reason does not say what it reads"}); err != nil {
		t.Fatalf("LogJudged: %v", err)
	}

	judged := readJudged(t, m, *req.RecordID)
	judge := record.JudgeActor(judgeModel)
	if len(judged) != 1 || judged[0].actor != judge || judged[0].grantID != "" || judged[0].verdict != "refer" {
		t.Fatalf("request.judged rows = %+v; want one by %q naming no grant and carrying the verdict", judged, judge)
	}
	got := readDecided(t, m, *req.RecordID)
	if got.events != 0 || got.state != "pending" || got.decidedBy != "" || len(got.approvers) != 0 {
		t.Fatalf("after a referred verdict = %+v; want the request untouched and pending", got)
	}

	// A verdict a caller left nil is a row all the same: audit.detail is NOT NULL, so it is
	// written as the empty object rather than refused.
	bare := pendingJudgeable(t, m, enr, key, "INFRA-1353 wants the key too", 600, "SHARED_KEY")
	if err := m.LogJudged(t.Context(), *bare.RecordID, judgeModel, nil); err != nil {
		t.Fatalf("LogJudged(no detail): %v", err)
	}
	if judged := readJudged(t, m, *bare.RecordID); len(judged) != 1 || judged[0].actor != judge || judged[0].verdict != "" {
		t.Fatalf("request.judged rows of a verdict with no detail = %+v; want one by %q", judged, judge)
	}
}

// TestApplyJudgeDecisionRefusesARecordTheRulesDoNotAdmit drives each reason record.MayJudge
// refuses a record — more than one secret, a lifetime past the entry's judge bound, a reason
// naming no Dispatch issue — and the one no caller may ask for, a denial. Each comes back to the
// caller as a refusal it can test and leaves the request exactly as it found it.
func TestApplyJudgeDecisionRefusesARecordTheRulesDoNotAdmit(t *testing.T) {
	for name, c := range map[string]struct {
		reason   string
		lifetime int
		names    []string
		deny     bool
	}{
		"two secrets":                     {"INFRA-1353 wants both", 600, []string{"DEEL_API_KEY", "DEEL_OTHER_KEY"}, false},
		"a lifetime past the judge bound": {"INFRA-1353 wants the key", 1200, []string{"DEEL_API_KEY"}, false},
		"a reason naming no issue":        {"need it", 600, []string{"DEEL_API_KEY"}, false},
		"a denial":                        {"INFRA-1353 wants the key", 600, []string{"DEEL_API_KEY"}, true},
	} {
		t.Run(name, func(t *testing.T) {
			m, enr, key := newJudgeFixture(t)
			// DEEL_OTHER_KEY is the operator's second human-tier secret, so a request for both
			// of its names waits on one approver and is refused for its two names rather than
			// for ErrMixedApprovers.
			retag(t, m, policytest.Secret("DEEL_OTHER_KEY", fixtureOperator, policy.TierHuman, "other-v1"))
			req := pendingJudgeable(t, m, enr, key, c.reason, c.lifetime, c.names...)

			var err error
			if c.deny {
				_, _, err = m.applyDecisionAttempt(t.Context(), *req.RecordID, false, byJudge(judgeModel, auditDetail{"verdict": "deny"}))
				if err == nil {
					t.Fatal("a judge's denial was accepted; the judge never denies")
				}
			} else {
				_, err = m.ApplyJudgeDecision(t.Context(), *req.RecordID, judgeModel, map[string]any{"verdict": "approve"})
				if !errors.Is(err, record.ErrNotJudgeable) {
					t.Fatalf("ApplyJudgeDecision = %v; want record.ErrNotJudgeable", err)
				}
			}

			got := readDecided(t, m, *req.RecordID)
			if got.events != 0 || got.state != "pending" || got.decidedBy != "" || len(got.approvers) != 0 {
				t.Errorf("after the refusal = %+v; want the request untouched and pending", got)
			}
			if judged := readJudged(t, m, *req.RecordID); len(judged) != 0 {
				t.Errorf("request.judged rows after the refusal = %+v; want none: the transaction wrote nothing", judged)
			}
		})
	}
}

// TestOneDecisionBetweenThePersonAndTheJudge: whichever of them answers first decides the
// request, in both orders. The second caller gets ErrTerminal, and the record keeps exactly one
// terminal event and one grant, under the actor of whoever was first (D10).
func TestOneDecisionBetweenThePersonAndTheJudge(t *testing.T) {
	judge := record.JudgeActor(judgeModel)
	for name, judgeFirst := range map[string]bool{"the judge first": true, "the person first": false} {
		t.Run(name, func(t *testing.T) {
			m, enr, key := newJudgeFixture(t)
			req := pendingJudgeable(t, m, enr, key, "INFRA-1353 wants the key", 600, "DEEL_API_KEY")
			verdict := map[string]any{"verdict": "approve"}

			if judgeFirst {
				if dec, err := m.ApplyJudgeDecision(t.Context(), *req.RecordID, judgeModel, verdict); err != nil || dec.GrantID == "" {
					t.Fatalf("the judge's approval = %+v, %v; want a grant", dec, err)
				}
				if dec, err := m.ApplyDecision(t.Context(), *req.RecordID, true, fixtureOperator); !errors.Is(err, ErrTerminal) {
					t.Fatalf("the person deciding after the judge = %+v, %v; want ErrTerminal", dec, err)
				}
			} else {
				if dec, err := m.ApplyDecision(t.Context(), *req.RecordID, true, fixtureOperator); err != nil || dec.GrantID == "" {
					t.Fatalf("the person's approval = %+v, %v; want a grant", dec, err)
				}
				if dec, err := m.ApplyJudgeDecision(t.Context(), *req.RecordID, judgeModel, verdict); !errors.Is(err, ErrTerminal) {
					t.Fatalf("the judge deciding after the person = %+v, %v; want ErrTerminal", dec, err)
				}
			}

			got := readDecided(t, m, *req.RecordID)
			wantActor, wantApprover, wantLogin := judge, fixtureOperator, (*string)(nil)
			if !judgeFirst {
				wantActor, wantLogin = record.HumanActor(fixtureOperator), new(fixtureOperator)
			}
			switch {
			case got.events != 1 || len(got.approvers) != 1:
				t.Fatalf("after both answered = %d decision events and %d grants; want one of each", got.events, len(got.approvers))
			case got.actor != wantActor || got.approvers[0] != wantApprover:
				t.Errorf("the decision = actor %q, grant approver %q; want %q and %q", got.actor, got.approvers[0], wantActor, wantApprover)
			case (got.login == nil) != (wantLogin == nil) || got.login != nil && *got.login != *wantLogin:
				t.Errorf("the decision event's login = %v; want %v", got.login, wantLogin)
			}
			// The judge's verdict is written with its own approval and with nothing else: the
			// attempt the terminal request refused wrote no row at all.
			wantJudged := 0
			if judgeFirst {
				wantJudged = 1
			}
			if judged := readJudged(t, m, *req.RecordID); len(judged) != wantJudged {
				t.Errorf("request.judged rows = %+v; want %d", judged, wantJudged)
			}
		})
	}
}

// TestTheJudgedRowSaysWhatTheTransactionDid: an approval the judge makes can commit and grant
// nothing — the current policy may have dropped the session from the secret's requesters list,
// which is recorded as the request's own denial, and the requesting enrollment may have gone,
// which is recorded approved and then denied. Either way the request.judged row beside that
// decision commits with it, and what it says it applied is what the transaction did and not
// what the judge asked for. A reader of GET /v1/grant-log reads the row to learn whether the
// judge's verdict released a credential, so a row saying applied where nothing was granted is
// the trail telling them something untrue.
func TestTheJudgedRowSaysWhatTheTransactionDid(t *testing.T) {
	for name, c := range map[string]struct {
		// before runs between the pending request and the judge's approval.
		before      func(t *testing.T, m *Machine, enrollmentID string)
		wantState   string
		wantApplied string
		wantDetail  string
	}{
		"an approval that grants": {wantState: "granted", wantApplied: "true"},
		"an enrollment that has gone": {
			before: func(t *testing.T, m *Machine, enrollmentID string) {
				t.Helper()
				if _, err := m.Store.Pool.Exec(t.Context(),
					`update enrollments set lease_expires_at = now() - interval '1 minute' where id=$1`, enrollmentID); err != nil {
					t.Fatalf("lapse the enrollment's lease: %v", err)
				}
			},
			wantState: "denied", wantApplied: "false",
			wantDetail: "the requesting enrollment is no longer live",
		},
		"a requesters list that dropped the session": {
			before: func(t *testing.T, m *Machine, _ string) {
				t.Helper()
				retag(t, m, listedSecret("DEEL_API_KEY", fixtureOperator, policy.TierHuman, otherPerson))
			},
			wantState: "denied", wantApplied: "false",
			wantDetail: "DEEL_API_KEY: " + policy.ReasonRequesterNotListed,
		},
	} {
		t.Run(name, func(t *testing.T) {
			m, enr, key := newJudgeFixture(t)
			req := pendingJudgeable(t, m, enr, key, "INFRA-1353 wants the key", 600, "DEEL_API_KEY")
			if c.before != nil {
				c.before(t, m, enr)
			}

			dec, err := m.ApplyJudgeDecision(t.Context(), *req.RecordID, judgeModel, map[string]any{"verdict": "approve"})
			if err != nil {
				t.Fatalf("ApplyJudgeDecision = %+v, %v; want the decision made", dec, err)
			}
			if dec.State != c.wantState || dec.Detail != c.wantDetail {
				t.Fatalf("the decision = state %q detail %q; want %q and %q", dec.State, dec.Detail, c.wantState, c.wantDetail)
			}

			judged := readJudged(t, m, *req.RecordID)
			if len(judged) != 1 {
				t.Fatalf("request.judged rows = %+v; want the one the decision wrote beside itself", judged)
			}
			if judged[0].applied != c.wantApplied {
				t.Errorf("the row reads applied %q; want %q, which is what the transaction did (%s, grant %q)",
					judged[0].applied, c.wantApplied, dec.State, dec.GrantID)
			}
			if (judged[0].grantID != "") != (c.wantState == "granted") {
				t.Errorf("the row names grant %q for a %s request; want it to name one only where the approval granted", judged[0].grantID, dec.State)
			}
		})
	}
}

// TestTheJudgeSettlesNoWithheldName: the judge's run checks what the session's operator withheld
// before it asks a model anything, and then spends up to twenty seconds on that model call. The
// decision transaction checks it again, so a name withheld in those seconds is never settled by
// the judge — neither granted nor denied — and the request waits for the person who decides it
// today. The guard is the judge's alone: the same withheld name, on the same request, is still
// the owner's to approve, which is the path a person has had all along.
func TestTheJudgeSettlesNoWithheldName(t *testing.T) {
	m, enr, key := newJudgeFixture(t)
	req := pendingJudgeable(t, m, enr, key, "INFRA-1353 wants the key", 600, "DEEL_API_KEY")
	if _, err := m.Store.Pool.Exec(t.Context(),
		`insert into withheld_secrets (enrollment_id, name) values ($1,'DEEL_API_KEY')`, enr); err != nil {
		t.Fatalf("withhold the name: %v", err)
	}

	dec, err := m.ApplyJudgeDecision(t.Context(), *req.RecordID, judgeModel, map[string]any{"verdict": "approve"})
	if !errors.Is(err, record.ErrNotApprover) {
		t.Fatalf("the judge's approval of a withheld name = %+v, %v; want record.ErrNotApprover", dec, err)
	}
	got := readDecided(t, m, *req.RecordID)
	if got.events != 0 || got.state != "pending" || got.decidedBy != "" || len(got.approvers) != 0 {
		t.Fatalf("after the refusal = %+v; want the request untouched and pending", got)
	}
	if judged := readJudged(t, m, *req.RecordID); len(judged) != 0 {
		t.Errorf("request.judged rows = %+v; want none: the transaction wrote nothing", judged)
	}

	person, err := m.ApplyDecision(t.Context(), *req.RecordID, true, fixtureOperator)
	if err != nil || person.State != "granted" || person.GrantID == "" {
		t.Fatalf("the owner's approval of the same withheld name = %+v, %v; want it granted, as it always was", person, err)
	}
}
