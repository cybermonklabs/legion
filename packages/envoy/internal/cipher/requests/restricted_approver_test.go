// restricted_approver_test.go holds the rule that a person Dispatch calls a restricted member is
// no approver of a secret. The broker's approval page signs in only its own domain, so such a
// person decides no secret request; and the routes Dispatch relays for its signed-in people, the
// pending list (PendingForApprover), the grant list (GrantsForApprover), the revoke
// (RevokeByApprover) and the dismissal (Dismiss), show them no secret request or grant as its
// approver and let them end or dismiss none, whichever arm would make them its approver: a Google
// group that owns the secret and lists them, the secret's own owner, or anyone, for a shared
// secret. What is theirs as a person stays theirs: their own machine login, and the grants of
// their own sessions. While Dispatch cannot say what kind of member a person is, the lists leave
// the approver rows off and the dismissal is refused, while the revoke goes ahead, since a revoke
// only takes access away; a revoke as the session's operator asks nothing.
package requests

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/policy/policytest"
	"github.com/sjawhar/envoy/internal/cipher/projectaccess"
	"github.com/sjawhar/envoy/internal/cipher/projectaccess/projectaccesstest"
	"github.com/sjawhar/envoy/internal/cipher/record"
	"github.com/sjawhar/envoy/internal/logtest"
)

// guestMember is a person an access list admitted from outside the sign-in domain, whom Dispatch
// calls a restricted member.
const guestMember = "guest@outside.example"

// approverKinds is what the stand-in Dispatch of these tests answers for each person: guestMember
// restricted, everyone else of the fixtures full.
func approverKinds() map[string]string {
	return map[string]string{
		guestMember:     projectaccess.MemberRestricted,
		secondMember:    projectaccess.MemberFull,
		groupMember:     projectaccess.MemberFull,
		groupAsker:      projectaccess.MemberFull,
		groupOutsider:   projectaccess.MemberFull,
		fixtureOperator: projectaccess.MemberFull,
	}
}

// approverRows is what one arm of approval puts in front of a person: a secret request still
// pending and one whose deadline passed with nobody deciding it, each waiting on the person
// through the arm, and a live grant the arm makes theirs to end. m asks dispatch, the stand-in,
// what kind of member each person is; everything was written before it first asked.
type approverRows struct {
	m                *Machine
	dispatch         *projectaccesstest.Server
	pending, expired string
	grant            string
}

// asks m's stand-in afresh: a client of its own, with no answer kept from an earlier read.
func (r approverRows) asks() {
	r.m.Projects = projectaccess.New(r.dispatch.URL, r.dispatch.Account, nil)
}

// requireRecordApprover fails t unless record recordID waits on approver.
func requireRecordApprover(t *testing.T, m *Machine, recordID, approver string) {
	t.Helper()
	var got string
	if err := m.Store.Pool.QueryRow(t.Context(), `select approver from credential_requests where id=$1`, recordID).Scan(&got); err != nil || got != approver {
		t.Fatalf("record %s's approver = %q, %v; want %q", recordID, got, err, approver)
	}
}

// requireGrantApprover fails t unless grant grantID records approver as its approver.
func requireGrantApprover(t *testing.T, m *Machine, grantID, approver string) {
	t.Helper()
	var got string
	if err := m.Store.Pool.QueryRow(t.Context(), `select coalesce(approver, '') from grants where id=$1`, grantID).Scan(&got); err != nil || got != approver {
		t.Fatalf("grant %s's approver = %q, %v; want %q", grantID, got, err, approver)
	}
}

// pendingRecord fails t unless req is pending on a record, and answers that record.
func pendingRecord(t *testing.T, what string, req Request) string {
	t.Helper()
	if req.State != "pending" || req.RecordID == nil {
		t.Fatalf("%s = %+v; want it pending on a record", what, req)
	}
	return *req.RecordID
}

// groupApproverRows is the group arm: requests for TEAM_KEY, which approversGroup owns, from
// groupAsker's sessions, the group listing guestMember and secondMember beside groupMember, and
// the grant of one of them groupMember approved.
func groupApproverRows(t *testing.T) approverRows {
	t.Helper()
	f := newGroupFixture(t)
	f.directory.Set(approversGroup, groupMember, secondMember, guestMember)
	ask := func(box string) Request {
		t.Helper()
		enr, key := newEnrollment(t, f.m.Store, "session", box+"-"+t.Name(), new(groupAsker), nil)
		return f.ask(t, enr, key, "TEAM_KEY")
	}
	rows := approverRows{m: f.m, dispatch: projectaccesstest.NewServer(t, nil, approverKinds())}
	rows.pending = pendingRecord(t, "TEAM_KEY asked for", ask("box-pending"))
	expired := ask("box-expired")
	rows.expired = pendingRecord(t, "TEAM_KEY asked for", expired)
	pastDeadline(t, f.m, expired.ID)
	decided := pendingRecord(t, "TEAM_KEY asked for", ask("box-decided"))
	dec, err := f.m.ApplyDecision(t.Context(), decided, true, groupMember)
	if err != nil || dec.GrantID == "" {
		t.Fatalf("ApplyDecision(TEAM_KEY) by %s = %+v, %v; want a grant", groupMember, dec, err)
	}
	rows.grant = dec.GrantID
	for _, recordID := range []string{rows.pending, rows.expired, decided} {
		requireRecordApprover(t, f.m, recordID, approversForm)
	}
	requireGrantApprover(t, f.m, rows.grant, approversForm)
	rows.asks()
	return rows
}

// ownerApproverRows is the owner arm: DEEL_API_KEY handed to owner, human tier, asked for by
// fixtureOperator's sessions, and a grant of it the model judge approved in owner's stead, which
// records owner as its approver as one owner approved would. The judge's hold of each request a
// person is to decide has ended (endHold), so it is on the person's list.
func ownerApproverRows(t *testing.T, owner string) approverRows {
	t.Helper()
	m, enr, key := newJudgeFixture(t)
	retag(t, m, policytest.Secret("DEEL_API_KEY", owner, policy.TierHuman, "deel-v1"))
	rows := approverRows{m: m, dispatch: projectaccesstest.NewServer(t, nil, approverKinds())}
	pending := newSecretRequest(t, m, "box-pending")
	rows.pending = *pending.RecordID
	endHold(t, m, pending.ID)
	expired := newSecretRequest(t, m, "box-expired")
	rows.expired = *expired.RecordID
	endHold(t, m, expired.ID)
	pastDeadline(t, m, expired.ID)
	rows.grant = judgedGrant(t, m, enr, key, 600, "DEEL_API_KEY")
	requireRecordApprover(t, m, rows.pending, owner)
	requireRecordApprover(t, m, rows.expired, owner)
	requireGrantApprover(t, m, rows.grant, owner)
	rows.asks()
	return rows
}

// anyoneApproverRows is the anyone arm: requests for SHARED_KEY, shared and human tier, from
// service pods, which name no person responsible for them, so they wait on everyone's list once
// the judge's hold has ended (endHold); and a grant of it the model judge approved on another such
// pod in anyone's stead.
func anyoneApproverRows(t *testing.T) approverRows {
	t.Helper()
	m, _, _ := newJudgeFixture(t)
	ask := func(runtimeID string) Request {
		t.Helper()
		pod, key := newServiceEnrollment(t, m.Store, fixtureService, fixtureSubject, runtimeID+"-"+t.Name())
		req, err := m.Create(t.Context(), pod, signRequest(t, m, key, "needs the shared key", "SHARED_KEY"), "", nil)
		if err != nil {
			t.Fatalf("Create(SHARED_KEY) on %s: %v", runtimeID, err)
		}
		endHold(t, m, req.ID)
		return req
	}
	rows := approverRows{m: m, dispatch: projectaccesstest.NewServer(t, nil, approverKinds())}
	rows.pending = pendingRecord(t, "SHARED_KEY asked for", ask("pod-pending"))
	expired := ask("pod-expired")
	rows.expired = pendingRecord(t, "SHARED_KEY asked for", expired)
	pastDeadline(t, m, expired.ID)
	pod, key := newServiceEnrollment(t, m.Store, fixtureService, fixtureSubject, "pod-judged-"+t.Name())
	rows.grant = judgedGrant(t, m, pod, key, 600, "SHARED_KEY")
	for _, recordID := range []string{rows.pending, rows.expired} {
		requireRecordApprover(t, m, recordID, record.AnyoneApprover)
		var notify string
		if err := m.Store.Pool.QueryRow(t.Context(), `select coalesce(notify, '') from credential_requests where id=$1`, recordID).Scan(&notify); err != nil || notify != "" {
			t.Fatalf("record %s names %q, %v as responsible; want nobody, so it waits on everyone's list", recordID, notify, err)
		}
	}
	requireGrantApprover(t, m, rows.grant, record.AnyoneApprover)
	rows.asks()
	return rows
}

// dismissals counts the dismissal rows of record recordID.
func dismissals(t *testing.T, m *Machine, recordID string) int {
	t.Helper()
	var n int
	if err := m.Store.Pool.QueryRow(t.Context(), `select count(*) from credential_request_dismissals where record_id=$1`, recordID).Scan(&n); err != nil {
		t.Fatalf("count the dismissals of %s: %v", recordID, err)
	}
	return n
}

// TestARestrictedMemberIsNoApprover drives each approver route as a restricted member who is a
// secret's approver by each arm — a member of the Google group that owns it, its owner, and
// anyone, for a shared secret — and as a full member in the same place, the control: the full
// member's lists hold the arm's pending and expired requests and its grant, and the full member
// dismisses the expired request and ends the grant; the restricted member's lists hold none of
// them, and their dismissal and their revoke are refused record.ErrNotApprover and ErrNotApprover,
// writing no dismissal and leaving the grant live.
func TestARestrictedMemberIsNoApprover(t *testing.T) {
	people := []struct {
		name, person string
		restricted   bool
	}{
		{"a restricted member", guestMember, true},
		{"a full member", secondMember, false},
	}
	for _, arm := range []struct {
		name  string
		build func(t *testing.T, person string) approverRows
	}{
		{"of the Google group that owns the secret", func(t *testing.T, _ string) approverRows { return groupApproverRows(t) }},
		{"as the secret's owner", ownerApproverRows},
		{"as anyone, of a shared secret", func(t *testing.T, _ string) approverRows { return anyoneApproverRows(t) }},
	} {
		t.Run(arm.name, func(t *testing.T) {
			for _, who := range people {
				t.Run(who.name, func(t *testing.T) {
					rows := arm.build(t, who.person)
					m, approves := rows.m, !who.restricted

					t.Run("its pending list", func(t *testing.T) {
						got := listedStates(t, m, who.person, bothStates)
						for _, row := range []string{listing(rows.pending, "pending"), listing(rows.expired, "expired")} {
							if listed := slices.Contains(got, row); listed != approves {
								t.Errorf("PendingForApprover(%s) = %v; holds %s: %v, want %v", who.person, got, row, listed, approves)
							}
						}
					})

					t.Run("its grant list", func(t *testing.T) {
						if listed := listsGrant(t, m, who.person, rows.grant); listed != approves {
							t.Errorf("GrantsForApprover(%s) holds grant %s: %v, want %v", who.person, rows.grant, listed, approves)
						}
					})

					t.Run("its dismissal of the expired request", func(t *testing.T) {
						d, err := m.Dismiss(t.Context(), rows.expired, who.person)
						switch {
						case approves && (err != nil || !d.Wrote || d.DismissedBy != who.person):
							t.Fatalf("Dismiss(%s) by %s = %+v, %v; want it written by them", rows.expired, who.person, d, err)
						case !approves && !errors.Is(err, record.ErrNotApprover):
							t.Fatalf("Dismiss(%s) by %s = %+v, %v; want record.ErrNotApprover", rows.expired, who.person, d, err)
						case !approves && dismissals(t, m, rows.expired) != 0:
							t.Fatalf("Dismiss(%s) by %s was refused yet wrote a dismissal", rows.expired, who.person)
						}
					})

					t.Run("its revoke of the grant", func(t *testing.T) {
						err := m.RevokeByApprover(t.Context(), rows.grant, who.person, record.HumanActor(who.person))
						if approves {
							if err != nil {
								t.Fatalf("RevokeByApprover(%s) by %s = %v; want it ended", rows.grant, who.person, err)
							}
							requireRevokedBy(t, m, rows.grant, who.person)
							return
						}
						if !errors.Is(err, ErrNotApprover) {
							t.Fatalf("RevokeByApprover(%s) by %s = %v; want ErrNotApprover", rows.grant, who.person, err)
						}
						requireGrantLive(t, m, rows.grant)
					})
				})
			}
		})
	}
}

// TestARestrictedMemberKeepsWhatIsTheirOwn: what is the person's own, rather than a secret's
// approver's, stays theirs whatever kind of member Dispatch calls them, a restricted member and a
// full one alike: their own machine logins, pending and expired, are on their pending list and
// theirs to dismiss, since a person's machine login is decided by that person alone; and a grant of
// their own session, taken before the broker asked Dispatch anything, is on their grant list and
// theirs to end as the session's operator, which withholds its name from the session. The same
// reads hold the requests and the grant of the secret the person owns (the owner arm) for the full
// member alone, so each read is one that asked what kind of member the person is.
func TestARestrictedMemberKeepsWhatIsTheirOwn(t *testing.T) {
	for _, who := range []struct {
		name, person string
		restricted   bool
	}{
		{"a restricted member", guestMember, true},
		{"a full member", secondMember, false},
	} {
		t.Run(who.name, func(t *testing.T) {
			rows := ownerApproverRows(t, who.person)
			m, approves := rows.m, !who.restricted
			// Their own session's grant is taken as the broker took it before it asked Dispatch
			// anything; a restricted member's session asks for nothing now.
			m.Projects = nil
			retag(t, m, policytest.Secret("SHARED_TOKEN", policy.OwnerShared, policy.TierAgent, "shared-token-v1"))
			session, key := newEnrollment(t, m.Store, "session", "own-box-"+t.Name(), new(who.person), nil)
			own, err := m.Create(t.Context(), session, signRequest(t, m, key, "needs it", "SHARED_TOKEN"), "", nil)
			if err != nil || own.GrantID == nil {
				t.Fatalf("Create(SHARED_TOKEN) on %s's session = %+v, %v; want it granted at once", who.person, own, err)
			}
			waiting := insertMachineLogin(t, m, who.person, "own-host-waiting", "BCDF-GHJK", time.Now().Add(time.Hour))
			lapsed := insertMachineLogin(t, m, who.person, "own-host-lapsed", "LMNP-QRST", time.Now().Add(-time.Hour))
			rows.asks()

			got := listedStates(t, m, who.person, bothStates)
			for _, row := range []string{listing(waiting, "pending"), listing(lapsed, "expired")} {
				if !slices.Contains(got, row) {
					t.Errorf("PendingForApprover(%s) = %v; want their own machine login %s", who.person, got, row)
				}
			}
			for _, row := range []string{listing(rows.pending, "pending"), listing(rows.expired, "expired")} {
				if listed := slices.Contains(got, row); listed != approves {
					t.Errorf("PendingForApprover(%s) = %v; holds the request %s for their secret: %v, want %v", who.person, got, row, listed, approves)
				}
			}
			if d, err := m.Dismiss(t.Context(), lapsed, who.person); err != nil || !d.Wrote {
				t.Errorf("Dismiss of their own expired machine login by %s = %+v, %v; want it written", who.person, d, err)
			}
			if !listsGrant(t, m, who.person, *own.GrantID) {
				t.Errorf("GrantsForApprover(%s) does not hold their own session's grant %s", who.person, *own.GrantID)
			}
			if listed := listsGrant(t, m, who.person, rows.grant); listed != approves {
				t.Errorf("GrantsForApprover(%s) holds the grant %s of their secret: %v, want %v", who.person, rows.grant, listed, approves)
			}
			if err := m.RevokeByApprover(t.Context(), *own.GrantID, who.person, record.HumanActor(who.person)); err != nil {
				t.Fatalf("RevokeByApprover of their own session's grant by %s = %v; want it ended", who.person, err)
			}
			requireRevokedBy(t, m, *own.GrantID, who.person)
			if names := withheldFrom(t, m, session); !slices.Equal(names, []string{"SHARED_TOKEN"}) {
				t.Errorf("withheld from %s's session = %v; want SHARED_TOKEN, the operator's revoke", who.person, names)
			}
		})
	}
}

// TestAnApproverWhoseMemberKindCannotBeReadIsNoApprover: while Dispatch cannot say what kind of
// member a person is, the lists and the dismissal close the approver arms to them, failing closed
// as every other read of a person's member kind does (projectaccess.Restricted): a full member of
// the group that owns a secret finds neither of its requests on their pending list nor its grant
// on their grant list, and their dismissal is refused *projectaccess.MemberKindUncheckable naming
// them, with no dismissal written. In the same outage their own machine login stays on their
// list, their revoke of their own session's grant ends it, since a revoke as the operator asks
// Dispatch nothing, and a person outside the group is refused ErrNotApprover, as before, having
// asked nothing either. The revoke alone fails open (revokeApproves): their revoke of the group's
// grant ends it, with one RevokeUncheckedMessage line naming them, since a revoke only takes access
// away. Once Dispatch answers, the lists hold the group's requests again and the dismissal is
// written.
func TestAnApproverWhoseMemberKindCannotBeReadIsNoApprover(t *testing.T) {
	rows := groupApproverRows(t)
	m := rows.m
	ctx := context.Background()
	session, key := newEnrollment(t, m.Store, "session", "own-box-"+t.Name(), new(secondMember), nil)
	own, err := m.Create(ctx, session, signRequest(t, m, key, "team work", "TEAM_TOKEN"), "", nil)
	if err != nil || own.GrantID == nil {
		t.Fatalf("Create(TEAM_TOKEN) on %s's session = %+v, %v; want it granted at once", secondMember, own, err)
	}
	login := insertMachineLogin(t, m, secondMember, "own-host", "BCDF-GHJK", time.Now().Add(time.Hour))

	rows.dispatch.SetDown(true)
	rows.asks()
	before := rows.dispatch.Calls()
	t.Run("while Dispatch cannot say, the lists and the dismissal are closed", func(t *testing.T) {
		got := listedStates(t, m, secondMember, bothStates)
		for _, row := range []string{listing(rows.pending, "pending"), listing(rows.expired, "expired")} {
			if slices.Contains(got, row) {
				t.Errorf("PendingForApprover(%s) while Dispatch is down = %v; want no %s", secondMember, got, row)
			}
		}
		if !slices.Contains(got, listing(login, "pending")) {
			t.Errorf("PendingForApprover(%s) while Dispatch is down = %v; want their own machine login %s still", secondMember, got, login)
		}
		if listsGrant(t, m, secondMember, rows.grant) {
			t.Errorf("GrantsForApprover(%s) while Dispatch is down holds the group's grant %s", secondMember, rows.grant)
		}
		_, err := m.Dismiss(ctx, rows.expired, secondMember)
		var refused *projectaccess.MemberKindUncheckable
		if !errors.As(err, &refused) || refused.Person != secondMember {
			t.Fatalf("Dismiss by %s while Dispatch is down = %v; want MemberKindUncheckable naming them", secondMember, err)
		}
		if n := dismissals(t, m, rows.expired); n != 0 {
			t.Fatalf("the refused dismissal wrote %d dismissals", n)
		}
		if rows.dispatch.Calls() == before {
			t.Fatal("the down Dispatch was never asked: the rows above are blind")
		}
	})
	t.Run("while Dispatch cannot say, a revoke that is no approver's asks nothing", func(t *testing.T) {
		if err := m.RevokeByApprover(ctx, rows.grant, groupOutsider, record.HumanActor(groupOutsider)); !errors.Is(err, ErrNotApprover) {
			t.Fatalf("RevokeByApprover by %s, outside the group, while Dispatch is down = %v; want ErrNotApprover", groupOutsider, err)
		}
		requireGrantLive(t, m, rows.grant)
		if !listsGrant(t, m, secondMember, *own.GrantID) {
			t.Errorf("GrantsForApprover(%s) while Dispatch is down does not hold their own session's grant", secondMember)
		}
		if err := m.RevokeByApprover(ctx, *own.GrantID, secondMember, record.HumanActor(secondMember)); err != nil {
			t.Fatalf("RevokeByApprover of their own session's grant by %s while Dispatch is down = %v; want it ended", secondMember, err)
		}
		requireRevokedBy(t, m, *own.GrantID, secondMember)
	})
	t.Run("while Dispatch cannot say, a member's revoke of the group's grant goes ahead", func(t *testing.T) {
		capture := &logtest.Recorder{}
		logtest.SetDefault(t, capture)
		asked := rows.dispatch.Calls()
		// A fresh client, so the read is a call of its own and not the failure the reads above
		// kept (projectaccess's failureTTL): the revoke is seen to ask.
		rows.asks()
		if err := m.RevokeByApprover(ctx, rows.grant, secondMember, record.HumanActor(secondMember)); err != nil {
			t.Fatalf("RevokeByApprover of the group's grant by %s while Dispatch is down = %v; want it ended", secondMember, err)
		}
		requireRevokedBy(t, m, rows.grant, secondMember)
		if rows.dispatch.Calls() == asked {
			t.Fatal("the revoke asked the down Dispatch nothing: it never met the outage")
		}
		if lines := linesOf(capture, RevokeUncheckedMessage); len(lines) != 1 || lines[0]["person"] != secondMember || lines[0]["level"] != "WARN" {
			t.Fatalf("the log holds %v of %q; want one WARN line naming %s", lines, RevokeUncheckedMessage, secondMember)
		}
	})

	rows.dispatch.SetDown(false)
	rows.asks()
	t.Run("once Dispatch answers, the lists and the dismissal are theirs again", func(t *testing.T) {
		got := listedStates(t, m, secondMember, bothStates)
		for _, row := range []string{listing(rows.pending, "pending"), listing(rows.expired, "expired")} {
			if !slices.Contains(got, row) {
				t.Errorf("PendingForApprover(%s) once Dispatch answers = %v; want %s", secondMember, got, row)
			}
		}
		if d, err := m.Dismiss(ctx, rows.expired, secondMember); err != nil || !d.Wrote {
			t.Fatalf("Dismiss by %s once Dispatch answers = %+v, %v; want it written", secondMember, d, err)
		}
	})
}
