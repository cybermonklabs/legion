// restricted_approver_test.go holds the rule that a person Dispatch calls a restricted member is
// no approver of a secret, nor of a service's machine login. The broker's approval page signs in
// only its own domain and refuses such a person as a decider too; and the routes Dispatch relays
// for its signed-in people, the pending list (PendingForApprover), the grant list
// (GrantsForApprover), the revoke (RevokeByApprover) and the dismissal (Dismiss), show them no
// secret request or grant as its approver and let them end or dismiss none, whichever arm would
// make them its approver: a Google group that owns the secret and lists them, the secret's own
// owner, or anyone, for a shared secret. What is theirs as a person stays theirs: their own machine
// login, and the grants of the sessions that act for them, and nothing else a list holds; a
// service's machine login is never theirs, even one that names them. While Dispatch cannot say
// what kind of member a person is, the lists leave the approver rows off and the dismissal and the
// decision are refused, while the revoke of the person a grant names as its approver goes ahead as
// long as Dispatch does not answer at all, since a revoke only takes access away; every revoke
// through a group or as anyone is refused then, whatever session holds the grant, and a revoke as
// the session's operator asks nothing. Every such read ends before Dispatch stops waiting for the
// route.
package requests

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
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
	retag(t, m, policytest.Judged(policytest.Secret("DEEL_API_KEY", owner, policy.TierHuman, "deel-v1"), 900, ""))
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
		req, err := m.Create(t.Context(), pod, signRequest(t, m, key, "needs the shared key", "SHARED_KEY"), "")
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
	t.Parallel()
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
							requireRestrictedRefusal(t, "Dismiss("+rows.expired+") by "+who.person, err, record.ErrNotApprover, "dismiss")
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
						requireRestrictedRefusal(t, "RevokeByApprover("+rows.grant+") by "+who.person, err, ErrNotApprover, "end a grant")
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
	t.Parallel()
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
			own, err := m.Create(t.Context(), session, signRequest(t, m, key, "needs it", "SHARED_TOKEN"), "")
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
// ErrNotApprover, as before, having asked nothing either. Their revoke of the group's grant is
// refused *projectaccess.MemberKindUncheckable naming them, with the grant left live and no
// RevokeUncheckedMessage line: the grant's session acts for groupAsker, who ends it as its
// operator asking nothing, so a revoke through its group does not go ahead (revokeApproves). Once
// Dispatch answers, the lists hold the group's requests again, the dismissal is written, and their
// revoke ends the group's grant.
func TestAnApproverWhoseMemberKindCannotBeReadIsNoApprover(t *testing.T) {
	// Not parallel: it captures the process's default logger, whose records are every test's that logs meanwhile (logtest.SetDefault, logtest.Swap, slog.SetDefault).
	rows := groupApproverRows(t)
	m := rows.m
	ctx := context.Background()
	session, key := newEnrollment(t, m.Store, "session", "own-box-"+t.Name(), new(secondMember), nil)
	own, err := m.Create(ctx, session, signRequest(t, m, key, "team work", "TEAM_TOKEN"), "")
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
	t.Run("while Dispatch cannot say, a member's revoke of the group's grant is refused", func(t *testing.T) {
		capture := &logtest.Recorder{}
		logtest.SetDefault(t, capture)
		asked := rows.dispatch.Calls()
		// A fresh client, so the read is a call of its own and not the failure the reads above
		// kept (projectaccess's failureTTL): the revoke is seen to ask.
		rows.asks()
		err := m.RevokeByApprover(ctx, rows.grant, secondMember, record.HumanActor(secondMember))
		var refused *projectaccess.MemberKindUncheckable
		if !errors.As(err, &refused) || refused.Person != secondMember {
			t.Fatalf("RevokeByApprover of the group's grant by %s while Dispatch is down = %v; want MemberKindUncheckable naming them", secondMember, err)
		}
		requireGrantLive(t, m, rows.grant)
		if rows.dispatch.Calls() == asked {
			t.Fatal("the revoke asked the down Dispatch nothing: it never met the outage")
		}
		if lines := linesOf(capture, RevokeUncheckedMessage); len(lines) != 0 {
			t.Fatalf("the log holds %v of %q; want none: nothing went ahead", lines, RevokeUncheckedMessage)
		}
	})

	rows.dispatch.SetDown(false)
	rows.asks()
	t.Run("once Dispatch answers, the lists, the dismissal and the revoke are theirs again", func(t *testing.T) {
		got := listedStates(t, m, secondMember, bothStates)
		for _, row := range []string{listing(rows.pending, "pending"), listing(rows.expired, "expired")} {
			if !slices.Contains(got, row) {
				t.Errorf("PendingForApprover(%s) once Dispatch answers = %v; want %s", secondMember, got, row)
			}
		}
		if d, err := m.Dismiss(ctx, rows.expired, secondMember); err != nil || !d.Wrote {
			t.Fatalf("Dismiss by %s once Dispatch answers = %+v, %v; want it written", secondMember, d, err)
		}
		if err := m.RevokeByApprover(ctx, rows.grant, secondMember, record.HumanActor(secondMember)); err != nil {
			t.Fatalf("RevokeByApprover of the group's grant by %s once Dispatch answers = %v; want it ended", secondMember, err)
		}
		requireRevokedBy(t, m, rows.grant, secondMember)
	})
}

// restrictedOpening is how each refusal of a person Dispatch calls a restricted member opens: by
// saying so, before anything of whose row it is, so the person refused reads why first.
const restrictedOpening = "a person Dispatch calls a restricted member is no approver of"

// requireRestrictedRefusal fails t unless err, what answered, is sentinel in words of its own that
// open by saying the person is a restricted member (restrictedOpening) and say they may not verb,
// so the person refused reads why, in the verb of what they did, and not the refusal of a person
// the row does not name.
func requireRestrictedRefusal(t *testing.T, what string, err, sentinel error, verb string) {
	t.Helper()
	if !errors.Is(err, sentinel) || !strings.HasPrefix(err.Error(), restrictedOpening) || !strings.Contains(err.Error(), verb) {
		t.Fatalf("%s = %v; want %v, opening %q and saying what they %s", what, err, sentinel, restrictedOpening, verb)
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
	t.Parallel()
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
		requireRestrictedRefusal(t, "Dismiss of the expired service login by "+guestMember, err, record.ErrNotApprover, "dismiss")
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

// TestALegacyServiceLoginNamingAPersonIsNoLoginOfTheirOwn: a broker from before token logins could
// open a service's machine login naming a person as its approver, which waits on that person's
// list alone. It is a service's login all the same, which no person holds as their own: its signed
// request names the service (approverRow), so it holds the person it names only as its approver,
// as a legacy login waiting on anyone does. A restricted member it names finds neither the pending
// one nor the expired one on their list, and their dismissal of the expired one is refused
// record.ErrNotApprover in words of its own, writing nothing. While Dispatch cannot say what kind
// of member the person it names is, that person's list holds neither, and their dismissal is
// refused *projectaccess.MemberKindUncheckable naming them. The control: a full member it names
// lists both, once Dispatch answers, and dismisses the expired one.
func TestALegacyServiceLoginNamingAPersonIsNoLoginOfTheirOwn(t *testing.T) {
	t.Parallel()
	m, _, _, _ := newFixture(t)
	dispatch := projectaccesstest.NewServer(t, nil, approverKinds())
	asks := func() { m.Projects = projectaccess.New(dispatch.URL, dispatch.Account, nil) }
	asks()
	named := func(person, code string, expires time.Time) string {
		t.Helper()
		return insertLoginRecord(t, m, person, "", record.AuthorizationDetail{Type: record.KindLauncherCredential, Identifier: "cluster.example", Service: fixtureService}, code, expires)
	}
	type logins struct{ pending, expired string }
	of := map[string]logins{
		guestMember:  {named(guestMember, "WXYZ-BCDF", time.Now().Add(15*time.Minute)), named(guestMember, "QRST-GHJK", time.Now().Add(-time.Hour))},
		secondMember: {named(secondMember, "LMNP-WXYZ", time.Now().Add(15*time.Minute)), named(secondMember, "BCDF-QRST", time.Now().Add(-time.Hour))},
	}
	for person, l := range of {
		requireRecordApprover(t, m, l.pending, person)
		requireRecordApprover(t, m, l.expired, person)
	}
	rows := func(person string) []string {
		return []string{listing(of[person].pending, "pending"), listing(of[person].expired, "expired")}
	}
	listsNone := func(t *testing.T, person, when string) {
		t.Helper()
		got := listedStates(t, m, person, bothStates)
		for _, row := range rows(person) {
			if slices.Contains(got, row) {
				t.Errorf("PendingForApprover(%s)%s = %v; want no %s, a service's login naming them", person, when, got, row)
			}
		}
	}
	refusedWritingNothing := func(t *testing.T, person string) {
		t.Helper()
		if n := dismissals(t, m, of[person].expired); n != 0 {
			t.Fatalf("the refused dismissal by %s wrote %d dismissals", person, n)
		}
	}

	t.Run("a restricted member it names lists neither, and their dismissal is refused", func(t *testing.T) {
		listsNone(t, guestMember, "")
		_, err := m.Dismiss(t.Context(), of[guestMember].expired, guestMember)
		requireRestrictedRefusal(t, "Dismiss of the expired service login naming "+guestMember, err, record.ErrNotApprover, "dismiss")
		refusedWritingNothing(t, guestMember)
	})

	dispatch.SetDown(true)
	asks()
	t.Run("while Dispatch cannot say, a full member it names lists neither, and their dismissal is refused", func(t *testing.T) {
		listsNone(t, secondMember, " while Dispatch is down")
		_, err := m.Dismiss(t.Context(), of[secondMember].expired, secondMember)
		var refused *projectaccess.MemberKindUncheckable
		if !errors.As(err, &refused) || refused.Person != secondMember {
			t.Fatalf("Dismiss of the expired service login naming %s while Dispatch is down = %v; want MemberKindUncheckable naming them", secondMember, err)
		}
		refusedWritingNothing(t, secondMember)
	})

	dispatch.SetDown(false)
	asks()
	t.Run("a full member it names lists both, and dismisses the expired one", func(t *testing.T) {
		got := listedStates(t, m, secondMember, bothStates)
		for _, row := range rows(secondMember) {
			if !slices.Contains(got, row) {
				t.Errorf("PendingForApprover(%s) = %v; want %s", secondMember, got, row)
			}
		}
		if d, err := m.Dismiss(t.Context(), of[secondMember].expired, secondMember); err != nil || !d.Wrote || d.DismissedBy != secondMember {
			t.Fatalf("Dismiss of the expired service login naming %s = %+v, %v; want it written by them", secondMember, d, err)
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
	t.Parallel()
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
			req, err := m.Create(ctx, enrollmentID, signRequest(t, m, key, "needs it on "+box, "DEEL_API_KEY"), "")
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

// hangingUp is the base URL of a server that closes every connection it accepts before it answers
// anything: a Dispatch the broker reaches, but whose transport fails before any answer arrives.
func hangingUp(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for the server that hangs up: %v", err)
	}
	t.Cleanup(func() { listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()
	return "http://" + listener.Addr().String()
}

// untrustedCertificate is the base URL of an HTTPS server whose certificate no client of the
// broker's trusts (httptest's own): a Dispatch the broker reaches whose certificate does not
// verify, so every call fails in its TLS handshake before any answer.
func untrustedCertificate(t *testing.T) string {
	t.Helper()
	server := httptest.NewUnstartedServer(http.NotFoundHandler())
	server.Config.ErrorLog = log.New(io.Discard, "", 0)
	server.StartTLS()
	t.Cleanup(server.Close)
	return server.URL
}

// resolvedAs is an HTTP client whose every dial fails as the dialer fails on a name it could not
// resolve, with failure: a Dispatch whose name the broker cannot look up.
func resolvedAs(failure *net.DNSError) *http.Client {
	return &http.Client{Transport: &http.Transport{DialContext: func(context.Context, string, string) (net.Conn, error) {
		return nil, &net.OpError{Op: "dial", Net: "tcp", Err: failure}
	}}}
}

// servicePodGroupRows is the group arm's grant on a session that acts for no person: TEAM_KEY,
// which approversGroup owns, asked for by a pod of fixtureService and approved by groupMember, the
// group listing secondMember and guestMember beside them. Only its grant is filled in.
func servicePodGroupRows(t *testing.T) approverRows {
	t.Helper()
	f := newGroupFixture(t)
	f.directory.Set(approversGroup, groupMember, secondMember, guestMember)
	pod, key := newServiceEnrollment(t, f.m.Store, fixtureService, fixtureSubject, "pod-team-"+t.Name())
	asked := pendingRecord(t, "TEAM_KEY asked for by a service's pod", f.ask(t, pod, key, "TEAM_KEY"))
	dec, err := f.m.ApplyDecision(t.Context(), asked, true, groupMember)
	if err != nil || dec.GrantID == "" {
		t.Fatalf("ApplyDecision(TEAM_KEY on a service's pod) by %s = %+v, %v; want a grant", groupMember, dec, err)
	}
	rows := approverRows{m: f.m, dispatch: projectaccesstest.NewServer(t, nil, approverKinds()), grant: dec.GrantID}
	requireGrantApprover(t, f.m, rows.grant, approversForm)
	rows.asks()
	return rows
}

// personSessionAnyoneRows is the anyone arm's grant on a person's session: SHARED_KEY, shared and
// human tier, asked for by a session of fixtureOperator's and approved by the model judge in
// anyone's stead. Only its grant is filled in.
func personSessionAnyoneRows(t *testing.T) approverRows {
	t.Helper()
	m, session, key := newJudgeFixture(t)
	rows := approverRows{m: m, dispatch: projectaccesstest.NewServer(t, nil, approverKinds()), grant: judgedGrant(t, m, session, key, 600, "SHARED_KEY")}
	requireGrantApprover(t, m, rows.grant, record.AnyoneApprover)
	rows.asks()
	return rows
}

// TestAnApproversRevokeGoesAheadOnlyWhileDispatchDoesNotAnswer: an approver's revoke fails open
// only while Dispatch does not answer, a failure that clears by itself, and only for the person the
// grant names as its approver. Dispatch not answering is the read's own bound passing, a transport
// failure before Dispatch's whole answer arrived (a connection it hangs up, an answer cut short, a
// name server that does not answer in time), a 502, 503 or 504 that names no code of Dispatch's, a
// gateway's page in front of it, or one of the codes Dispatch answers for its own outage
// (DATABASE_UNAVAILABLE, OIDC_ISSUER_UNAVAILABLE), and a 500 that names no code or Dispatch's
// INTERNAL, which is how Dispatch answers a read its database failed. The grant's named approver's
// revoke then ends the grant, and writes one RevokeUncheckedMessage line naming them. Every other
// failure is an answer that lasts until somebody fixes it, and refuses the revoke as it refuses
// every other read of a person's member kind: Dispatch answering a member kind this broker does not
// know, refusing the broker's own account, answering 400, a 500 naming any other code of
// Dispatch's (IDENTITY_ERROR), a 404 that is no member kind, a 502, 503 or 504 naming any other
// code of Dispatch's (MEMBER_KIND_UNKNOWN, FEATURE_OFF, GITHUB_APP_UNCONFIGURED), or an answer that
// does not decode; a certificate that does not verify; and a name that does not resolve. The
// grant's named approver is then refused *projectaccess.MemberKindUncheckable naming them, in words
// that end by saying Dispatch answered what the broker cannot read, the grant stays live, and no
// line is written. So is a revoke whose caller hung up while it read: that says nothing of
// Dispatch. A revoke through a group or as anyone does not go ahead while Dispatch's person route
// does not answer, whether the grant's session acts for a person, who ends it as its operator
// asking nothing, or for nobody, a service's pod: a full member's and a restricted member's alike
// are refused, in words that say the broker could not ask Dispatch. Once Dispatch answers, a full
// member ends a service pod's grant through its group or as anyone, as before. A failure an
// earlier read kept is no answer for the revoke either: it asks Dispatch itself, so a restricted
// member whose grant list failed to read their kind a moment before is refused once Dispatch
// answers.
func TestAnApproversRevokeGoesAheadOnlyWhileDispatchDoesNotAnswer(t *testing.T) {
	// Not parallel: it captures the process's default logger, whose records are every test's that logs meanwhile (logtest.SetDefault, logtest.Swap, slog.SetDefault).
	answers := func(status int, body string) func(*testing.T, approverRows) {
		return func(_ *testing.T, r approverRows) { r.dispatch.SetAnswer(status, body); r.asks() }
	}
	reads := func(base func(*testing.T) string) func(*testing.T, approverRows) {
		return func(t *testing.T, r approverRows) { r.m.Projects = projectaccess.New(base(t), r.dispatch.Account, nil) }
	}
	readsThrough := func(client *http.Client) func(*testing.T, approverRows) {
		return func(_ *testing.T, r approverRows) {
			r.m.Projects = projectaccess.New("http://dispatch.example", r.dispatch.Account, client)
		}
	}
	wentAhead := func(t *testing.T, rows approverRows, person string) {
		t.Helper()
		capture := &logtest.Recorder{}
		logtest.SetDefault(t, capture)
		if err := rows.m.RevokeByApprover(t.Context(), rows.grant, person, record.HumanActor(person)); err != nil {
			t.Fatalf("RevokeByApprover by %s = %v; want it ended", person, err)
		}
		requireRevokedBy(t, rows.m, rows.grant, person)
		if lines := linesOf(capture, RevokeUncheckedMessage); len(lines) != 1 || lines[0]["person"] != person || lines[0]["level"] != "WARN" {
			t.Fatalf("the log holds %v of %q; want one WARN line naming %s", lines, RevokeUncheckedMessage, person)
		}
	}
	for _, fault := range []struct {
		name string
		set  func(*testing.T, approverRows)
	}{
		{"a 503 that names no code of Dispatch's", func(_ *testing.T, r approverRows) { r.dispatch.SetDown(true); r.asks() }},
		{"a gateway in front of Dispatch answers 502", answers(http.StatusBadGateway, `<html>bad gateway</html>`)},
		{"a gateway in front of Dispatch answers 504", answers(http.StatusGatewayTimeout, `<html>gateway timeout</html>`)},
		{"Dispatch answers 503 DATABASE_UNAVAILABLE", answers(http.StatusServiceUnavailable, `{"code":"DATABASE_UNAVAILABLE","error":"database unavailable"}`)},
		{"Dispatch answers 503 OIDC_ISSUER_UNAVAILABLE", answers(http.StatusServiceUnavailable, `{"code":"OIDC_ISSUER_UNAVAILABLE","error":"the service-account token issuer cannot be reached; try again in a minute"}`)},
		{"Dispatch answers after the read's bound", func(_ *testing.T, r approverRows) { r.dispatch.SetStall(dispatchRelayWait); r.asks() }},
		{"Dispatch answers 500 INTERNAL", answers(http.StatusInternalServerError, `{"code":"INTERNAL","error":"internal server error"}`)},
		{"Dispatch answers 500 naming no code", answers(http.StatusInternalServerError, `<html>internal server error</html>`)},
		{"Dispatch hangs up before it answers", reads(hangingUp)},
		{"Dispatch's answer is cut short", reads(func(t *testing.T) string {
			return projectaccesstest.NewRefusing(t, http.StatusOK, projectaccesstest.RefusalCutShort)
		})},
		{"the name server does not answer in time", readsThrough(resolvedAs(&net.DNSError{Err: "i/o timeout", Name: "dispatch.example", IsTimeout: true}))},
	} {
		t.Run("the named approver's revoke goes ahead: "+fault.name, func(t *testing.T) {
			rows := ownerApproverRows(t, secondMember)
			fault.set(t, rows)
			wentAhead(t, rows, secondMember)
		})
	}
	// unread, cannotRead and the two words after them are how a MemberKindUncheckable ends: the
	// broker could not ask Dispatch, Dispatch answered what the broker cannot read, Dispatch refused
	// the broker's own account, and Dispatch answered a member kind this broker does not know.
	const (
		unread     = "the broker could not ask Dispatch"
		cannotRead = "Dispatch answered what the broker cannot read"
		refusedUs  = "Dispatch refused the broker's own account"
		unknownTo  = "Dispatch answered a member kind this broker does not know"
	)
	refused := func(t *testing.T, rows approverRows, person string, is error, why string) {
		t.Helper()
		capture := &logtest.Recorder{}
		logtest.SetDefault(t, capture)
		err := rows.m.RevokeByApprover(t.Context(), rows.grant, person, record.HumanActor(person))
		var uncheckable *projectaccess.MemberKindUncheckable
		if !errors.As(err, &uncheckable) || uncheckable.Person != person || is != nil && !errors.Is(err, is) {
			t.Fatalf("RevokeByApprover by %s = %v; want MemberKindUncheckable naming them, for %v", person, err, is)
		}
		if !strings.HasSuffix(uncheckable.Error(), ": "+why) {
			t.Fatalf("the refusal of %s's revoke says %q; want it to end %q", person, uncheckable.Error(), why)
		}
		requireGrantLive(t, rows.m, rows.grant)
		if lines := linesOf(capture, RevokeUncheckedMessage); len(lines) != 0 {
			t.Fatalf("the log holds %v of %q; want none: nothing went ahead", lines, RevokeUncheckedMessage)
		}
	}
	for _, fault := range []struct {
		name string
		set  func(*testing.T, approverRows)
		is   error
		why  string
	}{
		{"Dispatch answers a member kind this broker does not know", func(_ *testing.T, r approverRows) {
			r.dispatch.SetMemberKind(secondMember, "guest-of-honour")
			r.asks()
		}, projectaccess.ErrUnknownMemberKind, unknownTo},
		{"Dispatch refuses the broker's own account", answers(http.StatusForbidden, `{"code":"ACCOUNT_NOT_LISTED","error":"this account may not ask"}`), dispatchread.ErrBrokerRefused, refusedUs},
		{"Dispatch answers 404 with a code that is no member kind", answers(http.StatusNotFound, `{"code":"NOT_FOUND","error":"/api/v1/access/person"}`), nil, cannotRead},
		{"Dispatch answers 404 for a missing project", answers(http.StatusNotFound, `{"code":"PROJECT_NOT_FOUND","error":"no such project"}`), nil, cannotRead},
		{"Dispatch answers 400", answers(http.StatusBadRequest, `{"code":"INVALID_QUERY","error":"person is required"}`), nil, cannotRead},
		{"Dispatch answers 500 IDENTITY_ERROR", answers(http.StatusInternalServerError, `{"code":"IDENTITY_ERROR","error":"identity service unavailable"}`), nil, cannotRead},
		{"Dispatch answers 503 MEMBER_KIND_UNKNOWN", answers(http.StatusServiceUnavailable, `{"code":"MEMBER_KIND_UNKNOWN","error":"no such member kind"}`), nil, cannotRead},
		{"Dispatch answers 503 FEATURE_OFF", answers(http.StatusServiceUnavailable, `{"code":"FEATURE_OFF","error":"this route is off on this Dispatch"}`), nil, cannotRead},
		{"Dispatch answers 503 GITHUB_APP_UNCONFIGURED", answers(http.StatusServiceUnavailable, `{"code":"GITHUB_APP_UNCONFIGURED","error":"no GitHub App is configured"}`), nil, cannotRead},
		{"Dispatch's answer does not decode", answers(http.StatusOK, `{"person":"second@example.com","member_kind":`), nil, cannotRead},
		{"Dispatch's certificate does not verify", reads(untrustedCertificate), nil, cannotRead},
		{"Dispatch's name does not resolve", readsThrough(resolvedAs(&net.DNSError{Err: "no such host", Name: "dispatch.example", IsNotFound: true})), nil, cannotRead},
	} {
		t.Run("the named approver's revoke is refused: "+fault.name, func(t *testing.T) {
			rows := ownerApproverRows(t, secondMember)
			fault.set(t, rows)
			refused(t, rows, secondMember, fault.is, fault.why)
		})
	}
	t.Run("the named approver's revoke whose caller hung up while it read is refused", func(t *testing.T) {
		rows := ownerApproverRows(t, secondMember)
		ctx, hangUp := context.WithCancel(t.Context())
		// Dispatch has the request when the caller goes: the transport fails with the caller's
		// own cancellation, as when Dispatch's server hangs up on the broker's route.
		hanging := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			hangUp()
			<-r.Context().Done()
		}))
		t.Cleanup(hanging.Close)
		rows.m.Projects = projectaccess.New(hanging.URL, rows.dispatch.Account, nil)
		capture := &logtest.Recorder{}
		logtest.SetDefault(t, capture)
		err := rows.m.RevokeByApprover(ctx, rows.grant, secondMember, record.HumanActor(secondMember))
		var uncheckable *projectaccess.MemberKindUncheckable
		if !errors.As(err, &uncheckable) || uncheckable.Person != secondMember || !uncheckable.CallerEnded() {
			t.Fatalf("RevokeByApprover by %s whose caller hung up = %v; want MemberKindUncheckable naming them, its caller ended", secondMember, err)
		}
		requireGrantLive(t, rows.m, rows.grant)
		if lines := linesOf(capture, RevokeUncheckedMessage); len(lines) != 0 {
			t.Fatalf("the log holds %v of %q; want none: nothing went ahead", lines, RevokeUncheckedMessage)
		}
	})
	// personRouteFails is a Dispatch whose person route alone fails as fail makes it, while its
	// project route answers, so a row the person rule shows meets the member-kind read and nothing
	// else of the outage. It answers the stand-in that fails, for a row to check it was asked.
	personRouteFails := func(t *testing.T, rows approverRows, fail func(*projectaccesstest.Server)) *projectaccesstest.Server {
		t.Helper()
		kinds := projectaccesstest.NewServer(t, nil, approverKinds())
		fail(kinds)
		rows.m.Projects = kindsAskedOf{
			projects: projectaccess.New(rows.dispatch.URL, rows.dispatch.Account, nil),
			kinds:    projectaccess.New(kinds.URL, kinds.Account, nil),
		}
		return kinds
	}
	servicePodArms := []struct {
		name  string
		build func(*testing.T) approverRows
	}{
		{"a service pod's grant of the group's secret, through its group", servicePodGroupRows},
		{"a service pod's grant of a shared secret, as anyone", anyoneApproverRows},
	}
	everyArm := append(slices.Clone(servicePodArms), []struct {
		name  string
		build func(*testing.T) approverRows
	}{
		{"a person's session's grant of the group's secret, through its group", groupApproverRows},
		{"a person's session's grant of a shared secret, as anyone", personSessionAnyoneRows},
	}...)
	for _, outage := range []struct {
		name string
		fail func(*projectaccesstest.Server)
	}{
		{"answers 503 naming no code", func(s *projectaccesstest.Server) { s.SetDown(true) }},
		{"answers 500 INTERNAL", func(s *projectaccesstest.Server) {
			s.SetAnswer(http.StatusInternalServerError, `{"code":"INTERNAL","error":"internal server error"}`)
		}},
	} {
		t.Run("while Dispatch's person route "+outage.name+", the named approver's revoke goes ahead", func(t *testing.T) {
			rows := ownerApproverRows(t, secondMember)
			down := personRouteFails(t, rows, outage.fail)
			wentAhead(t, rows, secondMember)
			if down.Calls() == 0 {
				t.Fatal("the failing person route was never asked: the row is blind")
			}
		})
		for _, arm := range everyArm {
			for _, person := range []string{secondMember, guestMember} {
				t.Run("while Dispatch's person route "+outage.name+", "+person+"'s revoke of "+arm.name+" is refused", func(t *testing.T) {
					rows := arm.build(t)
					down := personRouteFails(t, rows, outage.fail)
					refused(t, rows, person, nil, unread)
					if down.Calls() == 0 {
						t.Fatal("the failing person route was never asked: the row is blind")
					}
				})
			}
		}
	}
	for _, arm := range servicePodArms {
		t.Run("while Dispatch answers, a full member's revoke of "+arm.name+" ends it", func(t *testing.T) {
			rows := arm.build(t)
			capture := &logtest.Recorder{}
			logtest.SetDefault(t, capture)
			if err := rows.m.RevokeByApprover(t.Context(), rows.grant, secondMember, record.HumanActor(secondMember)); err != nil {
				t.Fatalf("RevokeByApprover by %s while Dispatch answers = %v; want it ended", secondMember, err)
			}
			requireRevokedBy(t, rows.m, rows.grant, secondMember)
			if lines := linesOf(capture, RevokeUncheckedMessage); len(lines) != 0 {
				t.Fatalf("the log holds %v of %q; want none: Dispatch answered", lines, RevokeUncheckedMessage)
			}
		})
		for _, fault := range []struct {
			name string
			set  func(*projectaccesstest.Server)
			is   error
			why  string
		}{
			{"a member kind this broker does not know", func(s *projectaccesstest.Server) { s.SetMemberKind(secondMember, "guest-of-honour") }, projectaccess.ErrUnknownMemberKind, unknownTo},
			{"a refusal of the broker's own account", func(s *projectaccesstest.Server) {
				s.SetAnswer(http.StatusForbidden, `{"code":"ACCOUNT_NOT_LISTED","error":"this account may not ask"}`)
			}, dispatchread.ErrBrokerRefused, refusedUs},
		} {
			t.Run("a member's revoke of "+arm.name+" is refused while Dispatch's person route answers "+fault.name, func(t *testing.T) {
				rows := arm.build(t)
				personRouteFails(t, rows, fault.set)
				refused(t, rows, secondMember, fault.is, fault.why)
			})
		}
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
		requireRestrictedRefusal(t, "RevokeByApprover by "+guestMember+" once Dispatch answers", err, ErrNotApprover, "end a grant")
		requireGrantLive(t, rows.m, rows.grant)
	})
}

// kindsAskedOf is a projectaccess.Access that asks its member-kind reads of kinds and every other
// question of projects: a Dispatch whose person route alone fails while its project route answers,
// so a row the person rule shows meets the member-kind read and nothing else of the outage.
type kindsAskedOf struct {
	projects, kinds projectaccess.Access
}

func (k kindsAskedOf) Access(ctx context.Context, project, person string) (projectaccess.Answer, error) {
	return k.projects.Access(ctx, project, person)
}

func (k kindsAskedOf) States(ctx context.Context, projects []string) (map[string]projectaccess.Answer, error) {
	return k.projects.States(ctx, projects)
}

func (k kindsAskedOf) MemberKind(ctx context.Context, person string) (string, error) {
	return k.kinds.MemberKind(ctx, person)
}

// TestARestrictedMemberDecidesNoSecretRequest: the broker's approval page signs in only its own
// domain, and the decision it makes does not rest on that alone: a person Dispatch calls a
// restricted member decides no secret request, approving or denying, even one for a secret they
// own, refused record.ErrNotApprover in words of its own with the request still pending. While
// Dispatch cannot say what kind of member the deciding person is, the decision is refused
// *projectaccess.MemberKindUncheckable naming them, and decides nothing. The control: a full
// member who owns the secret approves it and denies it.
func TestARestrictedMemberDecidesNoSecretRequest(t *testing.T) {
	t.Parallel()
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
					requireRestrictedRefusal(t, "ApplyDecision by "+who.person, err, record.ErrNotApprover, "decide")
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
	t.Parallel()
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
