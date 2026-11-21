// restricted_approver_test.go holds the rule that a person Dispatch calls a restricted member is
// no approver of a secret, nor of a service's machine login. The broker's approval page signs in
// only its own domain and refuses such a person as a decider too; and the routes Dispatch relays
// for its signed-in people, the pending list (PendingForApprover), the grant list
// (GrantsForApprover), the revoke (RevokeByApprover) and the dismissal (Dismiss), show them no
// secret request or grant as its approver and let them end or dismiss none, whichever arm would
// make them its approver: a Google group that owns the secret and lists them, the secret's own
// owner, or anyone, for a shared secret. What is theirs as a person stays theirs: their own machine
// login, and the grants of the sessions that act for them, and nothing else a list holds. While
// Dispatch cannot say what kind of member a person is, the lists leave the approver rows off and
// the dismissal and the decision are refused, while the revoke goes ahead as long as Dispatch does
// not answer at all, since a revoke only takes access away; a revoke as the session's operator
// asks nothing. Every such read ends before Dispatch stops waiting for the route.
package requests

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/dispatchread"
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
// each in words that say why (requireRestrictedRefusal), writing no dismissal and leaving the grant
// live.
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
						case !approves:
							requireRestrictedRefusal(t, "Dismiss("+rows.expired+") by "+who.person, err, record.ErrNotApprover)
						}
						if !approves && dismissals(t, m, rows.expired) != 0 {
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
						requireRestrictedRefusal(t, "RevokeByApprover("+rows.grant+") by "+who.person, err, ErrNotApprover)
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
// list, their own expired machine login is dismissed, asking Dispatch nothing, since a person's
// machine login is theirs alone to decide, their revoke of their own session's grant ends it, since
// a revoke as the operator asks Dispatch nothing, and a person outside the group is refused
// ErrNotApprover, as before, having asked nothing either. The revoke alone fails open
// (revokeApproves), and only while Dispatch does not answer: their revoke of the group's
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
	lapsed := insertMachineLogin(t, m, secondMember, "own-host-lapsed", "LMNP-QRST", time.Now().Add(-time.Hour))

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
	t.Run("while Dispatch cannot say, their own expired machine login is dismissed, asking nothing", func(t *testing.T) {
		// A fresh client, so a read here would be a call of its own and not a failure the reads
		// above kept (projectaccess's failureTTL).
		rows.asks()
		asked := rows.dispatch.Calls()
		if d, err := m.Dismiss(ctx, lapsed, secondMember); err != nil || !d.Wrote {
			t.Fatalf("Dismiss of their own expired machine login by %s while Dispatch is down = %+v, %v; want it written", secondMember, d, err)
		}
		if calls := rows.dispatch.Calls() - asked; calls != 0 {
			t.Fatalf("the dismissal of their own machine login asked the down Dispatch %d times; want none", calls)
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

// requireRestrictedRefusal fails t unless err, what answered, is sentinel in words of its own that
// say the person is a restricted member, so the person refused reads why, and not the refusal of a
// person the row does not name.
func requireRestrictedRefusal(t *testing.T, what string, err, sentinel error) {
	t.Helper()
	if !errors.Is(err, sentinel) || !strings.Contains(err.Error(), "restricted member") {
		t.Fatalf("%s = %v; want %v, saying the person is a restricted member", what, err, sentinel)
	}
}

// insertServiceLogin writes a service's machine login a broker from before token logins opened,
// for service on host, with the deadline expires: its approver is record.AnyoneApprover, so it
// waits on every signed-in person's list. It answers the record's id.
func insertServiceLogin(t *testing.T, m *Machine, host, service, code string, expires time.Time) string {
	t.Helper()
	return insertLoginRecord(t, m, record.AnyoneApprover, "", record.AuthorizationDetail{Type: record.KindLauncherCredential, Identifier: host, Service: service}, code, expires)
}

// TestARestrictedMemberIsNoApproverOfAServicesLegacyLogin: a service's machine login a broker from
// before token logins opened waits on record.AnyoneApprover, so on every signed-in person's list,
// here one still pending and one whose deadline passed. A person Dispatch calls a restricted member
// is no approver of it: their list holds neither, and their dismissal of the expired one is
// refused record.ErrNotApprover in words of its own, writing nothing. While Dispatch cannot say
// what kind of member a person is, their list holds neither, and their dismissal is refused
// *projectaccess.MemberKindUncheckable naming them. The control: a full member's list holds both,
// and the full member dismisses the expired one.
func TestARestrictedMemberIsNoApproverOfAServicesLegacyLogin(t *testing.T) {
	m, _, _, _ := newFixture(t)
	dispatch := projectaccesstest.NewServer(t, nil, approverKinds())
	asks := func() { m.Projects = projectaccess.New(dispatch.URL, dispatch.Account, nil) }
	asks()
	pending := insertServiceLogin(t, m, "cluster.example", fixtureService, "WXYZ-ABCD", time.Now().Add(15*time.Minute))
	expired := insertServiceLogin(t, m, "cluster.example", fixtureService, "QRST-UVWX", time.Now().Add(-time.Hour))
	rows := []string{listing(pending, "pending"), listing(expired, "expired")}
	listsNone := func(t *testing.T, person, when string) {
		t.Helper()
		got := listedStates(t, m, person, bothStates)
		for _, row := range rows {
			if slices.Contains(got, row) {
				t.Errorf("PendingForApprover(%s)%s = %v; want no %s", person, when, got, row)
			}
		}
	}
	refusedWritingNothing := func(t *testing.T) {
		t.Helper()
		if n := dismissals(t, m, expired); n != 0 {
			t.Fatalf("the refused dismissal wrote %d dismissals", n)
		}
	}

	t.Run("a restricted member's list holds neither, and their dismissal is refused", func(t *testing.T) {
		listsNone(t, guestMember, "")
		_, err := m.Dismiss(t.Context(), expired, guestMember)
		requireRestrictedRefusal(t, "Dismiss of the expired service login by "+guestMember, err, record.ErrNotApprover)
		refusedWritingNothing(t)
	})

	dispatch.SetDown(true)
	asks()
	t.Run("while Dispatch cannot say, a full member's list holds neither, and their dismissal is refused", func(t *testing.T) {
		listsNone(t, secondMember, " while Dispatch is down")
		_, err := m.Dismiss(t.Context(), expired, secondMember)
		var refused *projectaccess.MemberKindUncheckable
		if !errors.As(err, &refused) || refused.Person != secondMember {
			t.Fatalf("Dismiss of the expired service login by %s while Dispatch is down = %v; want MemberKindUncheckable naming them", secondMember, err)
		}
		refusedWritingNothing(t)
	})

	dispatch.SetDown(false)
	asks()
	t.Run("a full member's list holds both, and they dismiss the expired one", func(t *testing.T) {
		got := listedStates(t, m, secondMember, bothStates)
		for _, row := range rows {
			if !slices.Contains(got, row) {
				t.Errorf("PendingForApprover(%s) = %v; want %s", secondMember, got, row)
			}
		}
		if d, err := m.Dismiss(t.Context(), expired, secondMember); err != nil || !d.Wrote || d.DismissedBy != secondMember {
			t.Fatalf("Dismiss of the expired service login by %s = %+v, %v; want it written by them", secondMember, d, err)
		}
	})
}

// guestPod is guestMember's own account, which BROKER_POD_OPERATORS maps to them while its pod
// takes its grants, as samisPod is fixtureOperator's.
var guestPod = personPod{person: guestMember, service: "person-guest", account: "system:serviceaccount:people:guest"}

// TestARestrictedMembersGrantListHoldsOnlyTheirOwnSessionsGrants: a person Dispatch calls a
// restricted member is no approver of a secret and sees nothing beyond their own projects, so
// their grant list holds the grants of the sessions that act for them, which they end as the
// operator, and no other: a grant someone else approved on their own session is on it, and none of
// a session that does not act for them now, read by its operator (operatorOf) and not by its row:
// not a service pod's grant it took without asking, though the person rule shows that grant to
// anyone, and not the grants of their own pod once BROKER_POD_OPERATORS no longer maps it to them,
// though the list still finds that pod by its row. The control is a full member in the same place:
// their own session's approved grant, the service pod's grant and their former pod's grants are
// all on their list.
func TestARestrictedMembersGrantListHoldsOnlyTheirOwnSessionsGrants(t *testing.T) {
	ctx := context.Background()
	m, _, _ := newPersonPodFixture(t)
	m.PodOperators[guestPod.account] = guestPod.person
	servicePod, serviceKey := newServiceEnrollment(t, m.Store, fixtureService, fixtureSubject, "pod-service-"+t.Name())
	_, serviceGrant := grantOf(t, m, servicePod, serviceKey, "no issue named", "SERVICE_KEY")
	type theirs struct{ own, podApproved, podAutomatic string }
	grants := map[string]theirs{}
	for _, p := range []personPod{guestPod, alicesPod} {
		approved := func(enrollmentID string, key *ecdsa.PrivateKey, box string) string {
			t.Helper()
			req, err := m.Create(ctx, enrollmentID, signRequest(t, m, key, "needs it on "+box, "DEEL_API_KEY"), "", nil)
			if err != nil {
				t.Fatalf("Create(DEEL_API_KEY) on %s's %s: %v", p.person, box, err)
			}
			return approveAsPerson(t, m, req)
		}
		session, key := newEnrollment(t, m.Store, "session", "own-box-"+p.person+"-"+t.Name(), new(p.person), nil)
		pod, podKey := p.enroll(t, m.Store, "pod-"+p.person+"-"+t.Name())
		g := theirs{own: approved(session, key, "session"), podApproved: approved(pod, podKey, "pod")}
		_, g.podAutomatic = grantOf(t, m, pod, podKey, "shared work", personPodShared)
		grants[p.person] = g
	}
	delete(m.PodOperators, guestPod.account)
	delete(m.PodOperators, alicesPod.account)
	dispatch := projectaccesstest.NewServer(t, nil, approverKinds())
	m.Projects = projectaccess.New(dispatch.URL, dispatch.Account, nil)

	for _, who := range []struct {
		name, person string
		restricted   bool
	}{
		{"a restricted member", guestMember, true},
		{"a full member", otherPerson, false},
	} {
		t.Run(who.name, func(t *testing.T) {
			g := grants[who.person]
			for _, row := range []struct {
				what, grant string
				want        bool
			}{
				{"their own session's grant, which someone else approved", g.own, true},
				{"a service pod's grant, which it took without asking", serviceGrant, !who.restricted},
				{"the approved grant of their pod, which acts for no person now", g.podApproved, !who.restricted},
				{"the automatic grant of their pod, which acts for no person now", g.podAutomatic, !who.restricted},
			} {
				if listed := listsGrant(t, m, who.person, row.grant); listed != row.want {
					t.Errorf("GrantsForApprover(%s) holds %s (%s): %v, want %v", who.person, row.what, row.grant, listed, row.want)
				}
			}
			if err := m.RevokeByApprover(ctx, g.own, who.person, record.HumanActor(who.person)); err != nil {
				t.Fatalf("RevokeByApprover of their own session's grant by %s = %v; want it ended", who.person, err)
			}
			requireRevokedBy(t, m, g.own, who.person)
		})
	}
}

// TestAnApproversRevokeGoesAheadOnlyWhileDispatchDoesNotAnswer: an approver's revoke fails open
// only while Dispatch does not answer, a failure that clears by itself. A failure that lasts until
// somebody fixes it refuses the revoke, as it refuses every other read of a person's member kind:
// Dispatch answering a member kind this broker does not know, and Dispatch refusing the broker's
// own account. The grant's named approver is then refused *projectaccess.MemberKindUncheckable
// naming them, the grant stays live, and no RevokeUncheckedMessage line is written. A failure an
// earlier read kept is no answer for the revoke either: it asks Dispatch itself, so a restricted
// member whose grant list failed to read their kind a moment before is refused once Dispatch
// answers.
func TestAnApproversRevokeGoesAheadOnlyWhileDispatchDoesNotAnswer(t *testing.T) {
	for _, fault := range []struct {
		name string
		set  func(*projectaccesstest.Server)
		is   error
	}{
		{"Dispatch answers a member kind this broker does not know", func(s *projectaccesstest.Server) { s.SetMemberKind(secondMember, "guest-of-honour") }, projectaccess.ErrUnknownMemberKind},
		{"Dispatch refuses the broker's own account", func(s *projectaccesstest.Server) {
			s.SetAnswer(http.StatusForbidden, `{"code":"ACCOUNT_NOT_LISTED","error":"this account may not ask"}`)
		}, dispatchread.ErrBrokerRefused},
	} {
		t.Run(fault.name, func(t *testing.T) {
			rows := ownerApproverRows(t, secondMember)
			fault.set(rows.dispatch)
			rows.asks()
			capture := &logtest.Recorder{}
			logtest.SetDefault(t, capture)
			err := rows.m.RevokeByApprover(t.Context(), rows.grant, secondMember, record.HumanActor(secondMember))
			var refused *projectaccess.MemberKindUncheckable
			if !errors.As(err, &refused) || refused.Person != secondMember || !errors.Is(err, fault.is) {
				t.Fatalf("RevokeByApprover by %s, the grant's named approver = %v; want MemberKindUncheckable naming them, for %v", secondMember, err, fault.is)
			}
			requireGrantLive(t, rows.m, rows.grant)
			if lines := linesOf(capture, RevokeUncheckedMessage); len(lines) != 0 {
				t.Fatalf("the log holds %v of %q; want none: nothing went ahead", lines, RevokeUncheckedMessage)
			}
		})
	}
	t.Run("a failure an earlier read kept is asked again", func(t *testing.T) {
		rows := ownerApproverRows(t, guestMember)
		rows.dispatch.SetDown(true)
		// The grant list asks what kind of member guestMember is, and the client keeps the failure
		// as the answer to that person's next reads for a while (projectaccess's failureTTL).
		if listsGrant(t, rows.m, guestMember, rows.grant) {
			t.Fatalf("GrantsForApprover(%s) while Dispatch is down holds the grant of their secret", guestMember)
		}
		rows.dispatch.SetDown(false)
		err := rows.m.RevokeByApprover(t.Context(), rows.grant, guestMember, record.HumanActor(guestMember))
		requireRestrictedRefusal(t, "RevokeByApprover by "+guestMember+" once Dispatch answers", err, ErrNotApprover)
		requireGrantLive(t, rows.m, rows.grant)
	})
}

// TestARestrictedMemberDecidesNoSecretRequest: the broker's approval page signs in only its own
// domain, and the decision it makes does not rest on that alone: a person Dispatch calls a
// restricted member decides no secret request, approving or denying, even one for a secret they
// own, refused record.ErrNotApprover in words of its own with the request still pending. While
// Dispatch cannot say what kind of member the deciding person is, the decision is refused
// *projectaccess.MemberKindUncheckable naming them, and decides nothing. The control: a full
// member who owns the secret approves it and denies it.
func TestARestrictedMemberDecidesNoSecretRequest(t *testing.T) {
	requestState := func(t *testing.T, m *Machine, recordID string) string {
		t.Helper()
		var state string
		if err := m.Store.Pool.QueryRow(t.Context(), `select state from requests where record_id=$1`, recordID).Scan(&state); err != nil {
			t.Fatalf("read the state of %s's request: %v", recordID, err)
		}
		return state
	}
	for _, who := range []struct {
		name, person string
		restricted   bool
	}{
		{"a restricted member", guestMember, true},
		{"a full member", secondMember, false},
	} {
		for _, decision := range []struct {
			verb, state string
			approve     bool
		}{{"approval", "granted", true}, {"denial", "denied", false}} {
			t.Run(who.name+"'s "+decision.verb, func(t *testing.T) {
				rows := ownerApproverRows(t, who.person)
				got, err := rows.m.ApplyDecision(t.Context(), rows.pending, decision.approve, who.person)
				if who.restricted {
					requireRestrictedRefusal(t, "ApplyDecision by "+who.person, err, record.ErrNotApprover)
					if state := requestState(t, rows.m, rows.pending); state != "pending" {
						t.Fatalf("the request after the refused %s is %s; want pending", decision.verb, state)
					}
					return
				}
				if err != nil || got.State != decision.state {
					t.Fatalf("ApplyDecision by %s = %+v, %v; want %s", who.person, got, err, decision.state)
				}
			})
		}
	}
	t.Run("while Dispatch cannot say, a full member's denial decides nothing", func(t *testing.T) {
		rows := ownerApproverRows(t, secondMember)
		rows.dispatch.SetDown(true)
		rows.asks()
		_, err := rows.m.ApplyDecision(t.Context(), rows.pending, false, secondMember)
		var refused *projectaccess.MemberKindUncheckable
		if !errors.As(err, &refused) || refused.Person != secondMember {
			t.Fatalf("ApplyDecision(deny) by %s while Dispatch is down = %v; want MemberKindUncheckable naming them", secondMember, err)
		}
		if state := requestState(t, rows.m, rows.pending); state != "pending" {
			t.Fatalf("the request after the refused denial is %s; want pending", state)
		}
	})
}

// dispatchRelayWait is how long Dispatch's server waits for the broker's answer to a route it
// relays (internal/dispatch/cipher's defaultTimeout): a read of Dispatch that such a route makes
// must end before it, or the route answers nobody.
const dispatchRelayWait = 5 * time.Second

// stalledKind is a projectaccess.Access whose member-kind reads Dispatch never answers: each waits
// until its caller's context ends, and records how long that context had left. A read whose
// context has no deadline would wait as long as its caller, so it is answered at once with an
// error and counted as unbounded, which the rows read as the fault. Its project questions are
// projects's.
type stalledKind struct {
	projects  projectaccess.Access
	mu        sync.Mutex
	unbounded int
	bounds    []time.Duration
}

func (s *stalledKind) Access(ctx context.Context, project, person string) (projectaccess.Answer, error) {
	return s.projects.Access(ctx, project, person)
}

func (s *stalledKind) States(ctx context.Context, projects []string) (map[string]projectaccess.Answer, error) {
	return s.projects.States(ctx, projects)
}

func (s *stalledKind) MemberKind(ctx context.Context, _ string) (string, error) {
	deadline, bounded := ctx.Deadline()
	s.mu.Lock()
	if !bounded {
		s.unbounded++
		s.mu.Unlock()
		return "", errors.New("stalledKind: a member-kind read with no deadline")
	}
	s.bounds = append(s.bounds, time.Until(deadline))
	s.mu.Unlock()
	<-ctx.Done()
	return "", ctx.Err()
}

// requireBounded fails t unless what asked s at least one member-kind read, and every read it
// asked had a deadline short of dispatchRelayWait.
func (s *stalledKind) requireBounded(t *testing.T, what string) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.unbounded != 0 || len(s.bounds) == 0 {
		t.Fatalf("%s asked %d member-kind reads with no deadline and %d with one; want at least one read, each bounded", what, s.unbounded, len(s.bounds))
	}
	for _, bound := range s.bounds {
		if bound >= dispatchRelayWait {
			t.Fatalf("%s read a member kind with %s to run; want less than the %s Dispatch waits for the route", what, bound, dispatchRelayWait)
		}
	}
}

// TestAMemberKindReadDispatchDoesNotAnswerEndsWithinTheRelaysWait: Dispatch's server waits
// dispatchRelayWait for the broker's answer to a route it relays, and one read of Dispatch may take
// longer (projectaccess's requestTimeout), so every member-kind read of such a route has a bound
// of its own below that wait. A read Dispatch does not answer within it is one Dispatch did not
// answer: the named approver's revoke goes ahead and ends the grant; the pending list answers with
// the person's own machine login on it and the approver rows left off; the grant list answers
// with the approver grant left off; and the dismissal is refused
// *projectaccess.MemberKindUncheckable.
func TestAMemberKindReadDispatchDoesNotAnswerEndsWithinTheRelaysWait(t *testing.T) {
	t.Run("the named approver's revoke", func(t *testing.T) {
		rows := ownerApproverRows(t, secondMember)
		stalled := &stalledKind{projects: rows.m.Projects}
		rows.m.Projects = stalled
		if err := rows.m.RevokeByApprover(t.Context(), rows.grant, secondMember, record.HumanActor(secondMember)); err != nil {
			t.Fatalf("RevokeByApprover by %s, the grant's named approver, while Dispatch does not answer = %v; want it ended", secondMember, err)
		}
		requireRevokedBy(t, rows.m, rows.grant, secondMember)
		stalled.requireBounded(t, "the revoke")
	})
	t.Run("the lists and the dismissal", func(t *testing.T) {
		rows := anyoneApproverRows(t)
		login := insertMachineLogin(t, rows.m, secondMember, "own-host", "BCDF-GHJK", time.Now().Add(time.Hour))
		stalled := &stalledKind{projects: rows.m.Projects}
		rows.m.Projects = stalled
		got := listedStates(t, rows.m, secondMember, bothStates)
		if !slices.Contains(got, listing(login, "pending")) || slices.Contains(got, listing(rows.pending, "pending")) || slices.Contains(got, listing(rows.expired, "expired")) {
			t.Errorf("PendingForApprover(%s) while Dispatch does not answer = %v; want their own machine login %s, and neither shared request", secondMember, got, login)
		}
		if listsGrant(t, rows.m, secondMember, rows.grant) {
			t.Errorf("GrantsForApprover(%s) while Dispatch does not answer holds the shared grant %s", secondMember, rows.grant)
		}
		_, err := rows.m.Dismiss(t.Context(), rows.expired, secondMember)
		var refused *projectaccess.MemberKindUncheckable
		if !errors.As(err, &refused) || refused.Person != secondMember {
			t.Errorf("Dismiss by %s while Dispatch does not answer = %v; want MemberKindUncheckable naming them", secondMember, err)
		}
		stalled.requireBounded(t, "the lists and the dismissal")
	})
}
