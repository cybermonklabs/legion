package requests

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/broker/record"
	"github.com/sjawhar/envoy/internal/broker/store/storetest"
	"github.com/sjawhar/envoy/internal/explaintest"
	"github.com/sjawhar/envoy/internal/logtest"
)

// TestGrantsForApproverNamesWhoApproved pins that the approver list names each grant's approver:
// the operator's list holds a grant another login approved on their enrollment, and must say that
// login decided it, not leave the operator to assume they did.
func TestGrantsForApproverNamesWhoApproved(t *testing.T) {
	m, enr, key, operator := newFixture(t)
	ctx := context.Background()
	pending, err := m.Create(ctx, enr, signRequest(t, m, key, "need it", "ALICE_KEY"), "", nil)
	if err != nil || pending.RecordID == nil {
		t.Fatalf("Create(pending) = %+v, %v", pending, err)
	}
	dec, err := m.ApplyDecision(ctx, *pending.RecordID, true, "Alice@Example.com")
	if err != nil || dec.GrantID == "" {
		t.Fatalf("ApplyDecision(alice): %+v %v", dec, err)
	}
	for _, viewer := range []string{operator, otherPerson} {
		grants, err := m.GrantsForApprover(ctx, viewer)
		if err != nil {
			t.Fatalf("GrantsForApprover(%s): %v", viewer, err)
		}
		if len(grants) != 1 || grants[0].GrantID != dec.GrantID || grants[0].Approver != otherPerson {
			t.Fatalf("GrantsForApprover(%s) = %+v, want grant %s approved by %s", viewer, grants, dec.GrantID, otherPerson)
		}
	}
}

// TestEachSlotOfAPodSignsItsOwnRecordAndListsItsOwnGrant pins the slot through request creation,
// the record read and the approver's grant list: a slotless pod enrollment's record is stored with
// the three-field enrollment line and a slotted one's with its slot as a fourth field, both decide
// and release through the whole chain, and each read names the slot it came from.
func TestEachSlotOfAPodSignsItsOwnRecordAndListsItsOwnGrant(t *testing.T) {
	m, _, _, approver := newFixture(t)
	ctx := context.Background()
	const podUID = "pod-uid-slots"
	type pod struct {
		slot, enrollmentID, recordID, grantID string
	}
	var pods []pod
	for _, slot := range []string{"", "implementer-g3", "reviewer-g3"} {
		id, key := newEnrollment(t, m.Store, "pod", podUID, nil, new("system:serviceaccount:legion:worker"))
		if _, err := m.Store.Pool.Exec(ctx, `update enrollments set slot=$2 where id=$1`, id, slot); err != nil {
			t.Fatalf("set slot %q: %v", slot, err)
		}
		req, err := m.Create(ctx, id, signRequest(t, m, key, "role "+slot, "DEEL_API_KEY"), "", nil)
		if err != nil || req.State != "pending" || req.RecordID == nil {
			t.Fatalf("Create(slot %q) = %+v, %v", slot, req, err)
		}
		var body string
		if err := m.Store.Pool.QueryRow(ctx, `select body from credential_requests where id=$1`, *req.RecordID).Scan(&body); err != nil {
			t.Fatal(err)
		}
		line := "\nenrollment: pod\t" + podUID + "\t-\n"
		if slot != "" {
			line = "\nenrollment: pod\t" + podUID + "\t-\t" + slot + "\n"
		}
		if !strings.Contains(body, line) {
			t.Fatalf("stored record of slot %q:\n%q\nwant the enrollment line %q", slot, body, line)
		}
		detail, err := m.ReadRecord(ctx, *req.RecordID)
		if err != nil || detail.Enrollment == nil || detail.Enrollment.Slot != slot || detail.Enrollment.RuntimeID != podUID {
			t.Fatalf("ReadRecord(slot %q) = %+v, %v", slot, detail.Enrollment, err)
		}
		dec, err := m.ApplyDecision(ctx, *req.RecordID, true, approver)
		if err != nil || dec.GrantID == "" {
			t.Fatalf("ApplyDecision(slot %q) = %+v, %v", slot, dec, err)
		}
		release, err := m.Values(ctx, dec.GrantID, id, nil, ReleaseCall{})
		if err != nil || release.Values["DEEL_API_KEY"] != "deel-v1" {
			t.Fatalf("Values(slot %q) = %v, %v; want the chain to verify and release", slot, release.Values, err)
		}
		pods = append(pods, pod{slot: slot, enrollmentID: id, recordID: *req.RecordID, grantID: dec.GrantID})
	}
	grants, err := m.GrantsForApprover(ctx, approver)
	if err != nil || len(grants) != len(pods) {
		t.Fatalf("GrantsForApprover = %+v, %v; want one grant per slot", grants, err)
	}
	for _, g := range grants {
		for _, p := range pods {
			if p.grantID == g.GrantID && (g.Enrollment.Slot != p.slot || g.Enrollment.RuntimeID != podUID) {
				t.Fatalf("listed grant %s = %+v, want pod %s in slot %q", g.GrantID, g.Enrollment, podUID, p.slot)
			}
		}
	}
}

// TestPendingForApproverTerminalEventsLiteralMatchesTheList pins the literal terminal-event
// predicates of pendingForApproverQuery and pendingOrExpiredForApproverQuery — the no-states
// statement's NOT EXISTS, and recordStateSQL's subquery, which both statements read — against
// record.TerminalEventNames(), so a later addition or rename of a terminal event changes them all
// together: the literal is what lets Postgres recognize credential_request_events' partial index
// (pendingForApproverQuery's own doc comment says why a bound parameter cannot). Every
// terminal-event predicate of each statement must be that literal, so one copy that drifted fails
// the test even beside one that did not.
func TestPendingForApproverTerminalEventsLiteralMatchesTheList(t *testing.T) {
	names := record.TerminalEventNames()
	quoted := make([]string, len(names))
	for i, name := range names {
		quoted[i] = fmt.Sprintf("'%s'", name)
	}
	want := "event in (" + strings.Join(quoted, ",") + ")"
	for name, query := range map[string]string{
		"pendingForApproverQuery":          pendingForApproverQuery,
		"pendingOrExpiredForApproverQuery": pendingOrExpiredForApproverQuery,
	} {
		if spelled, all := strings.Count(query, want), strings.Count(query, "event in ("); spelled == 0 || spelled != all {
			t.Errorf("%s spells record.TerminalEventNames() as %q in %d of its %d terminal-event predicates; the query and the list have drifted:\n%s", name, want, spelled, all, query)
		}
	}
}

// TestPendingForApproverAvoidsSequentialScans pins the query plans of both of PendingForApprover's
// statements — pendingForApproverQuery for a list with no states, and
// pendingOrExpiredForApproverQuery for one with both — the way Dispatch's
// api.TestListIssueAsksOpenQueryUsesAsksOpenIndex pins queryOwnerAsks's: Postgres automatically
// switches a prepared statement from a per-execution custom plan (built for that call's actual
// bound values) to a cached generic plan starting on its 6th execution, which an empty test table
// cannot trigger organically, so force_generic_plan pins every execution (including the first) to
// that shape instead of relying on the cost-based switch. enable_seqscan = off then makes a
// sequential scan of any table the statement touches the planner's last resort rather than an
// ordinary choice it might make anyway for a near-empty table: with both set, this test fails the
// moment an index path a statement depends on stops applying — credential_requests_approver
// (migration 0011) for the approver filter, credential_request_decision (migration 0005) for the
// terminal-event reads, or credential_request_dismissals' primary key (migration 0033) for the
// expired rows' NOT EXISTS.
func TestPendingForApproverAvoidsSequentialScans(t *testing.T) {
	st := storetest.Open(t)
	ctx := context.Background()
	for _, statement := range []struct {
		name, types, query, args string
	}{
		{"pendingForApproverQuery", "text, text", pendingForApproverQuery,
			fmt.Sprintf("'nobody@example.com', '%s'", record.AnyoneApprover)},
		{"pendingOrExpiredForApproverQuery", "text, text, boolean, boolean", pendingOrExpiredForApproverQuery,
			fmt.Sprintf("'nobody@example.com', '%s', true, true", record.AnyoneApprover)},
	} {
		t.Run(statement.name, func(t *testing.T) {
			tx, err := st.Pool.Begin(ctx)
			if err != nil {
				t.Fatalf("begin explain transaction: %v", err)
			}
			defer tx.Rollback(ctx)
			if _, err := tx.Exec(ctx, "set local enable_seqscan = off"); err != nil {
				t.Fatalf("disable sequential scans: %v", err)
			}
			if _, err := tx.Exec(ctx, "set local plan_cache_mode = force_generic_plan"); err != nil {
				t.Fatalf("force generic plan mode: %v", err)
			}
			// A prepared statement outlives the transaction on its pooled connection, so each
			// statement is prepared under a name of its own.
			if _, err := tx.Exec(ctx, "prepare "+statement.name+" ("+statement.types+") as "+statement.query); err != nil {
				t.Fatalf("prepare pending query: %v", err)
			}
			execute := "execute " + statement.name + "(" + statement.args + ")"
			for i := range 6 {
				if _, err := tx.Exec(ctx, execute); err != nil {
					t.Fatalf("execute pending query %d: %v", i, err)
				}
			}
			var planJSON []byte
			if err := tx.QueryRow(ctx, "explain (format json) "+execute).Scan(&planJSON); err != nil {
				t.Fatalf("explain execute pending query: %v", err)
			}
			var plans []struct {
				Plan json.RawMessage `json:"Plan"`
			}
			if err := json.Unmarshal(planJSON, &plans); err != nil {
				t.Fatalf("decode explain output: %v", err)
			}
			if len(plans) != 1 {
				t.Fatalf("explain plans = %#v, want one plan", plans)
			}
			for _, relation := range []string{"credential_requests", "credential_request_events", "credential_request_dismissals"} {
				if explaintest.SeqScansRelation(t, plans[0].Plan, relation) {
					t.Fatalf("%s's generic plan sequentially scans %s; want an index scan:\n%s", statement.name, relation, planJSON)
				}
			}
		})
	}
}

// TestReadRecordAndPendingNameBothSessionIDsWhenTheyDiffer pins LEGION-587: an agent_secret
// record's session names the enrollment's own session_id (stated at enroll time) and the
// request's own override (stated in its body at Create) independently, through both reads that
// answer one (ReadRecord and PendingForApprover).
func TestReadRecordAndPendingNameBothSessionIDsWhenTheyDiffer(t *testing.T) {
	m, enr, key, approver := newFixture(t)
	ctx := context.Background()
	if _, err := m.Store.Pool.Exec(ctx, `update enrollments set session_id=$2 where id=$1`, enr, "sess-enrollment-1"); err != nil {
		t.Fatalf("set enrollment session_id: %v", err)
	}
	pending, err := m.Create(ctx, enr, signRequest(t, m, key, "need it", "DEEL_API_KEY"), "sess-request-2", nil)
	if err != nil || pending.RecordID == nil {
		t.Fatalf("Create = %+v, %v", pending, err)
	}
	want := Session{Request: "sess-request-2", Enrollment: "sess-enrollment-1"}

	detail, err := m.ReadRecord(ctx, *pending.RecordID)
	if err != nil || detail.Session != want {
		t.Fatalf("ReadRecord.Session = %+v, %v, want %+v", detail.Session, err, want)
	}

	rows, err := m.PendingForApprover(ctx, approver, PendingStates{})
	if err != nil || len(rows) != 1 || rows[0].Session != want {
		t.Fatalf("PendingForApprover = %+v, %v, want one row naming %+v", rows, err, want)
	}
}

// TestPendingForApproverLeavesOutARecordThatDoesNotVerify pins INFRA-3950's first part: a
// pending record signed for another audience — the broker's BROKER_PUBLIC_URL before it changed,
// or a second broker's on the same database — is left out of the approver's list and logged once
// with its id and the reason, and the other records are listed and can be decided. Before, the
// list returned that record's "aud does not match" and the approval page and GET /v1/pending
// answered 503 for every request of that approver until the record expired.
func TestPendingForApproverLeavesOutARecordThatDoesNotVerify(t *testing.T) {
	capture := &logtest.Recorder{}
	logtest.SetDefault(t, capture)
	m, first, firstKey, approver := newFixture(t)
	ctx := context.Background()

	current := m.Audience
	m.Audience = "https://the-old-public-url.example"
	stale, err := m.Create(ctx, first, signRequest(t, m, firstKey, "asked before the URL moved", "DEEL_API_KEY"), "", nil)
	if err != nil || stale.RecordID == nil {
		t.Fatalf("Create under the old audience = %+v, %v", stale, err)
	}
	m.Audience = current
	var valid []string
	for _, box := range []string{"box-b", "box-c"} {
		enr, key := newEnrollment(t, m.Store, "session", box+"-"+t.Name(), new(fixtureOperator), nil)
		req, err := m.Create(ctx, enr, signRequest(t, m, key, "asked after the move", "DEEL_API_KEY"), "", nil)
		if err != nil || req.RecordID == nil {
			t.Fatalf("Create(%s) = %+v, %v", box, req, err)
		}
		valid = append(valid, *req.RecordID)
	}

	// Twice, as the approval page's poll asks: the second list logs nothing new.
	for range 2 {
		rows, err := m.PendingForApprover(ctx, approver, PendingStates{})
		if err != nil {
			t.Fatalf("PendingForApprover = %v; want the records that verify, without the one that does not", err)
		}
		var listed []string
		for _, r := range rows {
			listed = append(listed, r.RecordID)
		}
		slices.Sort(listed)
		want := slices.Sorted(slices.Values(valid))
		if !slices.Equal(listed, want) {
			t.Fatalf("PendingForApprover listed %q; want %q, and never the stale record %s", listed, want, *stale.RecordID)
		}
	}
	var lines []slog.Record
	for _, r := range capture.Records() {
		if r.Message == PendingLeftOutMessage {
			lines = append(lines, r)
		}
	}
	if len(lines) != 1 {
		t.Fatalf("%d %q lines after two lists; want exactly one", len(lines), PendingLeftOutMessage)
	}
	attrs := map[string]string{}
	lines[0].Attrs(func(a slog.Attr) bool { attrs[a.Key] = a.Value.String(); return true })
	if lines[0].Level != slog.LevelWarn || attrs["record_id"] != *stale.RecordID || !strings.Contains(attrs["reason"], "aud does not match") {
		t.Fatalf("the line is %s %v; want WARN naming record_id %s and the reason aud does not match", lines[0].Level, attrs, *stale.RecordID)
	}

	dec, err := m.ApplyDecision(ctx, valid[0], true, approver)
	if err != nil || dec.State != "granted" {
		t.Fatalf("ApplyDecision on a listed record = %+v, %v; want granted", dec, err)
	}
}

// TestPendingForApproverWarnsOnceAboutALeftOutRecordPastItsExpiry: the list reads a pending
// record past its expires_at until the sweeper ends it, so a left-out record in that state must
// still be warned about once, not on every poll of every open tab. Before, leaveOut dropped its
// entry at expires_at and logged it again on each list.
func TestPendingForApproverWarnsOnceAboutALeftOutRecordPastItsExpiry(t *testing.T) {
	capture := &logtest.Recorder{}
	logtest.SetDefault(t, capture)
	m, enr, key, approver := newFixture(t)
	ctx := context.Background()

	current := m.Audience
	m.Audience = "https://the-old-public-url.example"
	request := signRequest(t, m, key, "login", "example-host")
	m.Audience = current
	// A machine login, which is pending while it carries no terminal event, so no sweep has to
	// be held off for it to stay listed past its expiry.
	body := record.Body{
		Approver:        approver,
		Enrollment:      record.Enrollment{Kind: "-", RuntimeID: "-"},
		ExpiresAt:       time.Now().Add(-time.Hour).UTC().Truncate(time.Second),
		LifetimeSeconds: new(3600),
		Request:         request,
		RulesVersion:    "rules-v1",
	}
	stale := body.ID()
	if _, err := m.Store.Pool.Exec(ctx, `insert into credential_requests (id, body, kind, approver, expires_at) values ($1,$2,'launcher_credential',$3,$4)`,
		stale, body.Canonical(), body.Approver, body.ExpiresAt); err != nil {
		t.Fatalf("insert the expired machine login record: %v", err)
	}
	valid, err := m.Create(ctx, enr, signRequest(t, m, key, "asked after the move", "DEEL_API_KEY"), "", nil)
	if err != nil || valid.RecordID == nil {
		t.Fatalf("Create = %+v, %v", valid, err)
	}

	for range 2 {
		rows, err := m.PendingForApprover(ctx, approver, PendingStates{})
		if err != nil || len(rows) != 1 || rows[0].RecordID != *valid.RecordID {
			t.Fatalf("PendingForApprover = %+v, %v; want only %s, never the stale record %s", rows, err, *valid.RecordID, stale)
		}
	}
	warned := 0
	for _, r := range capture.Records() {
		if r.Message != PendingLeftOutMessage {
			continue
		}
		r.Attrs(func(a slog.Attr) bool {
			if a.Key == "record_id" && a.Value.String() == stale {
				warned++
			}
			return true
		})
	}
	if warned != 1 {
		t.Fatalf("%d %q lines for the expired record %s after two lists; want exactly one", warned, PendingLeftOutMessage, stale)
	}
}

// TestReadRecordKeepsTheEnrollmentSessionIDAfterRevocation pins that an id outlives its
// enrollment's revocation: enrollments.session_id is never cleared when revoked_at is set, so a
// decided record's own read still names the session that enrolled.
func TestReadRecordKeepsTheEnrollmentSessionIDAfterRevocation(t *testing.T) {
	m, enr, key, approver := newFixture(t)
	ctx := context.Background()
	if _, err := m.Store.Pool.Exec(ctx, `update enrollments set session_id=$2 where id=$1`, enr, "sess-enrollment-revoked"); err != nil {
		t.Fatalf("set enrollment session_id: %v", err)
	}
	pending, err := m.Create(ctx, enr, signRequest(t, m, key, "need it", "DEEL_API_KEY"), "", nil)
	if err != nil || pending.RecordID == nil {
		t.Fatalf("Create = %+v, %v", pending, err)
	}
	dec, err := m.ApplyDecision(ctx, *pending.RecordID, true, approver)
	if err != nil || dec.GrantID == "" {
		t.Fatalf("ApplyDecision = %+v, %v", dec, err)
	}
	if _, err := m.Store.Pool.Exec(ctx, `update enrollments set revoked_at=now() where id=$1`, enr); err != nil {
		t.Fatalf("revoke enrollment: %v", err)
	}

	detail, err := m.ReadRecord(ctx, *pending.RecordID)
	if err != nil || detail.Session.Enrollment != "sess-enrollment-revoked" {
		t.Fatalf("ReadRecord.Session after revocation = %+v, %v, want enrollment sess-enrollment-revoked", detail.Session, err)
	}
}

// TestReadRecordNamesNoSessionForAMachineLogin pins that a launcher_credential record - which has
// no request row and no requesting enrollment at all - always answers both session ids empty.
func TestReadRecordNamesNoSessionForAMachineLogin(t *testing.T) {
	m, _, key, _ := newFixture(t)
	ctx := context.Background()
	body := record.Body{
		Approver:        "alice",
		Enrollment:      record.Enrollment{Kind: "-", RuntimeID: "-"},
		ExpiresAt:       time.Now().Add(time.Hour),
		LifetimeSeconds: new(3600),
		Request:         signRequest(t, m, key, "login", "example-host"),
		RulesVersion:    "rules-v1",
	}
	recordID := body.ID()
	if _, err := m.Store.Pool.Exec(ctx, `insert into credential_requests (id, body, kind, approver, expires_at) values ($1,$2,'launcher_credential',$3,$4)`,
		recordID, body.Canonical(), body.Approver, body.ExpiresAt); err != nil {
		t.Fatalf("insert machine login record: %v", err)
	}
	detail, err := m.ReadRecord(ctx, recordID)
	if err != nil || detail.Session != (Session{}) {
		t.Fatalf("ReadRecord(machine login).Session = %+v, %v, want both empty", detail.Session, err)
	}
}

// TestAJudgedRecordAndItsGrantNameTheJudge pins the two reads every surface that says who
// decided a request is built from, against a record the model judge approved. The judge writes
// no login at all (decider.eventLogin), so the decision event's actor is the only thing that
// names it, and the grant it mints records the record's own approver, a person who never saw
// the request (D1, decider.grantApprover). So ReadRecord must carry the event's actor beside
// its empty login, and GrantsForApprover the request's decided_by beside the grant's approver.
// Without the two, a page prints "approved" with nobody beside it and a grant list names the
// owner as the approver of something the judge decided.
//
// The owner's own approval of the same secret on another session is read the same way
// throughout, which is what says the two reads tell the decider from the approver rather than
// having swapped one for the other.
func TestAJudgedRecordAndItsGrantNameTheJudge(t *testing.T) {
	m, session, key := newJudgeFixture(t)
	judge := record.JudgeActor(judgeModel)

	judged := pendingJudgeable(t, m, session, key, "INFRA-1353 wants it", 600, "DEEL_API_KEY")
	byTheJudge, err := m.ApplyJudgeDecision(t.Context(), *judged.RecordID, judgeModel, map[string]any{"verdict": "approve"})
	if err != nil || byTheJudge.GrantID == "" {
		t.Fatalf("ApplyJudgeDecision = %+v, %v; want a grant", byTheJudge, err)
	}

	own, ownKey := newEnrollment(t, m.Store, "session", "box-b-"+t.Name(), new(fixtureOperator), nil)
	ownRequest := pendingJudgeable(t, m, own, ownKey, "INFRA-1353 wants it on the other box", 600, "DEEL_API_KEY")
	byOwner, err := m.ApplyDecision(t.Context(), *ownRequest.RecordID, true, fixtureOperator)
	if err != nil || byOwner.GrantID == "" {
		t.Fatalf("ApplyDecision by the owner = %+v, %v; want a grant", byOwner, err)
	}

	for _, c := range []struct{ what, recordID, wantLogin, wantActor string }{
		{"the judge's approval", *judged.RecordID, "", judge},
		{"the owner's own", *ownRequest.RecordID, fixtureOperator, record.HumanActor(fixtureOperator)},
	} {
		detail, err := m.ReadRecord(t.Context(), c.recordID)
		if err != nil || detail.Decided == nil {
			t.Fatalf("ReadRecord(%s) = %+v, %v; want a decided record", c.what, detail, err)
		}
		if detail.Decided.Login != c.wantLogin || detail.Decided.Actor != c.wantActor {
			t.Errorf("ReadRecord(%s).Decided = login %q actor %q; want login %q actor %q",
				c.what, detail.Decided.Login, detail.Decided.Actor, c.wantLogin, c.wantActor)
		}
	}

	listed, err := m.GrantsForApprover(t.Context(), fixtureOperator)
	if err != nil || len(listed) != 2 {
		t.Fatalf("GrantsForApprover(%s) = %+v, %v; want the judged grant and the owner's own", fixtureOperator, listed, err)
	}
	for _, g := range listed {
		want := judge
		switch g.GrantID {
		case byOwner.GrantID:
			want = fixtureOperator
		case byTheJudge.GrantID:
		default:
			t.Fatalf("GrantsForApprover(%s) listed %+v; want the judged grant %s and the owner's %s", fixtureOperator, g, byTheJudge.GrantID, byOwner.GrantID)
		}
		if g.Approver != fixtureOperator || g.DecidedBy != want {
			t.Errorf("the listed grant %+v has approver %q and decider %q; want the owner %q as its approver and %q as its decider",
				g, g.Approver, g.DecidedBy, fixtureOperator, want)
		}
	}
}
