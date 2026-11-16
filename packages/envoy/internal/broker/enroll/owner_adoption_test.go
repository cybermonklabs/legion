// owner_adoption_test.go drives the principal an enrollment's requests belong to (ownerKey) and
// the one move it adds to the carry: the next enrollment of a Legion claim takes what a dormant
// enrollment of that claim still holds, after the session carry, and nothing else moves by owner
// beyond that. The session carry itself is session_adoption_test.go's.
package enroll

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/broker/exchange"
)

// plannerClaim and implementerClaim are two role claims of one issue, as the Legion daemon's
// claim tokens spell them (packages/daemon/internal/claim/token.go): the same service launches
// both, so only the claim tells their requests apart.
const (
	plannerClaim     = "legion-infra-infra-2653-planner"
	implementerClaim = "legion-infra-infra-2653-implementer"
)

// claimPod enrols one role process as the Legion daemon does: under the launcher's own token
// login, a pod uid of its own, a session of its own and the role's claim.
func claimPod(t *testing.T, r podRig, launcher Credential, claim, session, podUID string) Enrollment {
	t.Helper()
	in := r.podEnrollment(t, launcher, workerPodAccount, podUID)
	in.SessionID, in.Claim = str(session), claim
	enr, err := r.svc.Create(context.Background(), launcher, in)
	if err != nil {
		t.Fatalf("enrol pod %s of claim %q under session %s: %v", podUID, claim, session, err)
	}
	if enr.Existing {
		t.Fatalf("pod %s was answered an existing enrollment %s, want one of its own", podUID, enr.ID)
	}
	return enr
}

// ownerAndClaimOf are the owner and the claim an enrollment records, "" for a null.
func ownerAndClaimOf(t *testing.T, svc *Service, id uuid.UUID) (owner, claim string) {
	t.Helper()
	if err := svc.Store.Pool.QueryRow(context.Background(), `select coalesce(owner,''), coalesce(claim,'') from enrollments where id=$1`, id).
		Scan(&owner, &claim); err != nil {
		t.Fatalf("read the owner of %s: %v", id, err)
	}
	return owner, claim
}

// requestHolder is the enrollment a request hangs off now.
func requestHolder(t *testing.T, svc *Service, request string) uuid.UUID {
	t.Helper()
	var holder uuid.UUID
	if err := svc.Store.Pool.QueryRow(context.Background(), `select enrollment_id from requests where id=$1`, request).Scan(&holder); err != nil {
		t.Fatalf("read the enrollment of request %s: %v", request, err)
	}
	return holder
}

// approveOnTheDormantRow is what an approval given while no process of the session is up leaves:
// the request granted and its grant hung off the row the carry keeps for the session
// (requests.Machine.ApplyDecision, requests/dormant_approval_test.go). requests imports this
// package, so this test writes the two rows that decision writes rather than calling it.
func approveOnTheDormantRow(t *testing.T, svc *Service, enrollmentID uuid.UUID, request string) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	if _, err := svc.Store.Pool.Exec(ctx, `update requests set state='granted', decided_at=now(), decided_by='ada@example.com' where id=$1`, request); err != nil {
		t.Fatalf("grant request %s: %v", request, err)
	}
	grant := uuid.New()
	if _, err := svc.Store.Pool.Exec(ctx, `insert into grants (id, request_id, enrollment_id, expires_at) values ($1,$2,$3, now() + interval '1 hour')`,
		grant, request, enrollmentID); err != nil {
		t.Fatalf("insert the grant of request %s: %v", request, err)
	}
	return grant
}

// TestAnEnrollmentRecordsTheOwnerItsCallerProved: the owner is read from what the caller proved
// and from the claim a pod's launcher stated, and from nothing else the caller says. A person's
// sessions are the person's whether or not they name a session; a pod a launcher enrols under a
// claim is that claim's within the launcher's service; a pod that states none is its own pod's;
// and a machine's own session is that machine's, by the subject its credential proved.
func TestAnEnrollmentRecordsTheOwnerItsCallerProved(t *testing.T) {
	ctx := context.Background()
	r := newPodRig(t)
	launcher := r.login(t, launcherAccount, "pod-uid-daemon")
	machineSubject := exchange.MachineSubject(exchange.GoogleIdentity{Account: machineAccountEmail}, "tp-machine")
	machine := machineCredentialFor(t, r.svc, machineSubject, "tp-machine")
	person := mintCredential(t, r.svc, str("ada@example.com"), nil, "devbox")

	claimed := r.podEnrollment(t, launcher, workerPodAccount, "pod-uid-claimed")
	claimed.SessionID, claimed.Claim = str("planner-session"), plannerClaim
	unclaimed := r.podEnrollment(t, launcher, workerPodAccount, "pod-uid-unclaimed")
	unclaimed.SessionID = str("unclaimed-session")

	for _, c := range []struct {
		name      string
		cred      Credential
		in        Enrollment
		owner     string
		claim     string
		principal string
	}{
		{
			name:  "a person's session belongs to the person",
			cred:  person,
			in:    Enrollment{Kind: "session", RuntimeID: "box-1", Thumbprint: "tp-box-1", SessionID: str("ada-session")},
			owner: "person:ada@example.com", principal: "person:ada@example.com",
		},
		{
			name:  "a person's session that names no session belongs to the person too",
			cred:  person,
			in:    Enrollment{Kind: "session", RuntimeID: "box-2", Thumbprint: "tp-box-2"},
			owner: "person:ada@example.com",
		},
		{
			name:  "a pod a launcher enrols under a claim belongs to that claim of the launcher's service",
			cred:  launcher,
			in:    claimed,
			owner: "claim:legion-daemon/" + plannerClaim, claim: plannerClaim, principal: "account:" + workerPodAccount,
		},
		{
			name:  "a pod that states no claim belongs to its own pod",
			cred:  launcher,
			in:    unclaimed,
			owner: "machine:legion-daemon/pod-uid-unclaimed", principal: "account:" + workerPodAccount,
		},
		{
			name:  "a machine's own session belongs to the machine its credential proved",
			cred:  machine,
			in:    Enrollment{Kind: "session", RuntimeID: "machine-1", Thumbprint: "tp-machine-session", SessionID: str("machine-session")},
			owner: "machine:" + machineFleet + "/" + machineSubject, principal: "google:" + machineAccountEmail,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			enr, err := r.svc.Create(ctx, c.cred, c.in)
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			if owner, claim := ownerAndClaimOf(t, r.svc, enr.ID); owner != c.owner || claim != c.claim {
				t.Fatalf("the enrollment records owner %q and claim %q, want %q and %q", owner, claim, c.owner, c.claim)
			}
			if got := carryPrincipalOf(t, r.svc, enr.ID); got != c.principal {
				t.Fatalf("the enrollment's carry principal = %q, want %q: the owner changes nothing the session carry reads", got, c.principal)
			}
		})
	}
}

// TestTheNextPodOfAClaimTakesWhatTheLastOneLeft is the case this change exists for: a role's pod asks for a
// secret and its box ends before a person answers; the person approves it while no pod of the
// role is up; the role's next pod comes up under a session of its own, which the session carry
// cannot follow. It takes the grant and the request off the dormant pod because it states the
// same claim, and another role of the same service, which enrols first, takes nothing. The value
// reaching that next pod over HTTP is api's TestTheNextPodOfAClaimReceivesTheValueAPersonApprovedForTheLastOne.
func TestTheNextPodOfAClaimTakesWhatTheLastOneLeft(t *testing.T) {
	ctx := context.Background()
	r := newPodRig(t)
	launcher := r.login(t, launcherAccount, "pod-uid-daemon")
	first := claimPod(t, r, launcher, plannerClaim, "planner-session-1", "pod-uid-a")
	request := insertPendingRequest(t, r.svc, first.ID)

	if err := r.svc.Revoke(ctx, launcher, first.ID.String(), "launcher"); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if left := readCarryState(t, r.svc, first.ID); !left.Ended || !left.Dormant || left.Pending != 1 {
		t.Fatalf("the end of pod A left %+v, want it ended and dormant holding its pending request: this test measures what the next pod takes", left)
	}
	grant := approveOnTheDormantRow(t, r.svc, first.ID, request)

	other := claimPod(t, r, launcher, implementerClaim, "implementer-session-1", "pod-uid-c")
	if holder := grantHolder(t, r.svc, grant); holder != first.ID {
		t.Fatalf("grant %s hangs off %s after another claim's pod enrolled, want pod A's %s: a claim takes only its own", grant, holder, first.ID)
	}

	next := claimPod(t, r, launcher, plannerClaim, "planner-session-2", "pod-uid-b")
	if holder := grantHolder(t, r.svc, grant); holder != next.ID {
		t.Fatalf("grant %s hangs off %s, want pod B's %s: the next pod of the claim takes the grant pod A left", grant, holder, next.ID)
	}
	if holder := requestHolder(t, r.svc, request); holder != next.ID {
		t.Fatalf("request %s hangs off %s, want pod B's %s", request, holder, next.ID)
	}
	if left := readCarryState(t, r.svc, first.ID); !left.Ended || left.Dormant || left.LiveGrants != 0 {
		t.Fatalf("pod A is %+v after the move, want it ended, no longer dormant and holding nothing, so the carry sweep has nothing of it to revoke", left)
	}
	if held := readCarryState(t, r.svc, other.ID); held.LiveGrants != 0 || held.Pending != 0 {
		t.Fatalf("the other claim's pod holds %+v, want nothing", held)
	}
	var from uuid.UUID
	var rule string
	if err := r.svc.Store.Pool.QueryRow(ctx, `select (detail->>'from')::uuid, coalesce(detail->>'rule','') from audit where kind='enrollment.adopted' and enrollment_id=$1`, next.ID).
		Scan(&from, &rule); err != nil || from != first.ID || rule != "claim" {
		t.Fatalf("pod B's enrollment.adopted row names %s by rule %q (%v), want pod A's %s by the claim rule", from, rule, err, first.ID)
	}
}

// TestAResumedSessionGetsItsOwnGrantBackAfterAnotherSessionOfThePersonEnrolled: a person's
// grants follow the session that asked for them and never the person, so a second session of
// the same person enrolling on another box first takes nothing, and the first session, resumed on
// a third box, gets its grant back by the session carry.
func TestAResumedSessionGetsItsOwnGrantBackAfterAnotherSessionOfThePersonEnrolled(t *testing.T) {
	ctx := context.Background()
	svc := newService(t)
	issuer, key := withPodVerifier(t, svc)
	cred, first := carryEnrollment(t, svc, issuer, key, "session", "ada-session-1")
	grant := insertLiveGrant(t, svc, first.ID)
	if err := svc.Revoke(ctx, cred, first.ID.String(), "launcher"); err != nil {
		t.Fatalf("Revoke: %v", err)
	}

	_, second := movedTo(t, svc, issuer, key, "session", "ada-session-2", "ada-box-2")
	if holder := grantHolder(t, svc, grant); holder != first.ID {
		t.Fatalf("grant %s hangs off %s after the person's other session enrolled, want the dormant first session's %s: a person's grant is the session's", grant, holder, first.ID)
	}

	_, resumed := movedTo(t, svc, issuer, key, "session", "ada-session-1", "ada-box-3")
	if holder := grantHolder(t, svc, grant); holder != resumed.ID {
		t.Fatalf("grant %s hangs off %s, want the resumed session's %s", grant, holder, resumed.ID)
	}
	if held := readCarryState(t, svc, second.ID); held.LiveGrants != 0 {
		t.Fatalf("the person's other session holds %+v, want no grant", held)
	}
}

// TestALiveEnrollmentOfTheSameOwnerKeepsItsGrants: the claim rule takes only from a dormant
// enrollment, whose box has ended, and the person's and the machine's rows move by the session
// carry alone. A live enrollment of the same owner keeps what it holds when another enrollment of
// that owner comes up: a person signing in on a second box, and a second pod of one claim beside a
// first that is still running.
func TestALiveEnrollmentOfTheSameOwnerKeepsItsGrants(t *testing.T) {
	ctx := context.Background()
	t.Run("a person's sign-in on another box", func(t *testing.T) {
		svc := newService(t)
		box := mintCredential(t, svc, str("ada@example.com"), nil, "devbox-1")
		live, err := svc.Create(ctx, box, Enrollment{Kind: "session", RuntimeID: "box-1", Thumbprint: "tp-box-1", SessionID: str("box-1-session")})
		if err != nil {
			t.Fatalf("Create on box 1: %v", err)
		}
		grant := insertLiveGrant(t, svc, live.ID)
		signIn := mintCredential(t, svc, str("ada@example.com"), nil, "devbox-2")
		if _, err := svc.Create(ctx, signIn, Enrollment{Kind: "session", RuntimeID: "box-2", Thumbprint: "tp-box-2", SessionID: str("box-2-session")}); err != nil {
			t.Fatalf("Create on box 2: %v", err)
		}
		if holder := grantHolder(t, svc, grant); holder != live.ID {
			t.Fatalf("grant %s hangs off %s, want box 1's %s", grant, holder, live.ID)
		}
	})
	t.Run("a second pod of a claim beside a live one", func(t *testing.T) {
		r := newPodRig(t)
		launcher := r.login(t, launcherAccount, "pod-uid-daemon")
		live := claimPod(t, r, launcher, plannerClaim, "planner-session-1", "pod-uid-a")
		grant := insertLiveGrant(t, r.svc, live.ID)
		request := insertPendingRequest(t, r.svc, live.ID)
		claimPod(t, r, launcher, plannerClaim, "planner-session-2", "pod-uid-b")
		if holder := grantHolder(t, r.svc, grant); holder != live.ID {
			t.Fatalf("grant %s hangs off %s, want the live pod's %s", grant, holder, live.ID)
		}
		if holder := requestHolder(t, r.svc, request); holder != live.ID {
			t.Fatalf("request %s hangs off %s, want the live pod's %s", request, holder, live.ID)
		}
	})
}

// TestAnEnrollmentWithNoOwnerKeepsTodaysRule: every enrollment live when migration 0032 applies
// records no owner, and a null owner is owned by its own enrollment alone. A dormant pod of a claim
// whose row records none is taken by no later pod of the claim: its grant waits for its own
// session's next enrollment, or for the carry sweep, as before.
func TestAnEnrollmentWithNoOwnerKeepsTodaysRule(t *testing.T) {
	ctx := context.Background()
	r := newPodRig(t)
	launcher := r.login(t, launcherAccount, "pod-uid-daemon")
	old := claimPod(t, r, launcher, plannerClaim, "planner-session-1", "pod-uid-a")
	if _, err := r.svc.Store.Pool.Exec(ctx, `update enrollments set owner=null where id=$1`, old.ID); err != nil {
		t.Fatalf("clear the owner of %s: %v", old.ID, err)
	}
	grant := insertLiveGrant(t, r.svc, old.ID)
	request := insertPendingRequest(t, r.svc, old.ID)
	if err := r.svc.Revoke(ctx, launcher, old.ID.String(), "launcher"); err != nil {
		t.Fatalf("Revoke: %v", err)
	}

	claimPod(t, r, launcher, plannerClaim, "planner-session-2", "pod-uid-b")
	if holder := grantHolder(t, r.svc, grant); holder != old.ID {
		t.Fatalf("grant %s hangs off %s, want the ownerless pod's %s", grant, holder, old.ID)
	}
	if holder := requestHolder(t, r.svc, request); holder != old.ID {
		t.Fatalf("request %s hangs off %s, want the ownerless pod's %s", request, holder, old.ID)
	}
	if left := readCarryState(t, r.svc, old.ID); !left.Dormant {
		t.Fatalf("the ownerless pod is %+v, want it still dormant", left)
	}
}

// TestAClaimIsRefusedOffAPodAndWhenMalformed: a claim is the launcher's word for what a pod runs
// for, so it is refused on a session, on a pod enrolling itself (which no more names whose it is
// than it names a session), and in any shape a claim token cannot take. Each refusal writes no
// row; a claim of the longest shape is accepted.
func TestAClaimIsRefusedOffAPodAndWhenMalformed(t *testing.T) {
	ctx := context.Background()
	r := newPodRig(t)
	launcher := r.login(t, launcherAccount, "pod-uid-daemon")
	self := r.login(t, selfAccount, "pod-uid-self")
	person := mintCredential(t, r.svc, str("ada@example.com"), nil, "devbox")
	rows := func() int {
		t.Helper()
		var n int
		if err := r.svc.Store.Pool.QueryRow(ctx, `select count(*) from enrollments`).Scan(&n); err != nil {
			t.Fatalf("count enrollments: %v", err)
		}
		return n
	}

	pod := func(claim string) (Credential, Enrollment) {
		in := r.podEnrollment(t, launcher, workerPodAccount, "pod-uid-worker")
		in.Claim = claim
		return launcher, in
	}
	refused := map[string]func() (Credential, Enrollment){
		"a claim on a session": func() (Credential, Enrollment) {
			return person, Enrollment{Kind: "session", RuntimeID: "box-1", Thumbprint: "tp-box-1", Claim: plannerClaim}
		},
		"a pod enrolling itself": func() (Credential, Enrollment) {
			in := r.podEnrollment(t, self, selfAccount, "pod-uid-self")
			in.Claim = plannerClaim
			return self, in
		},
		"capital letters":     func() (Credential, Enrollment) { return pod("Legion-infra-infra-2653-planner") },
		"a leading digit":     func() (Credential, Enrollment) { return pod("2653-planner") },
		"a leading hyphen":    func() (Credential, Enrollment) { return pod("-legion-planner") },
		"an underscore":       func() (Credential, Enrollment) { return pod("legion_planner") },
		"a slash":             func() (Credential, Enrollment) { return pod("legion/planner") },
		"129 characters long": func() (Credential, Enrollment) { return pod("l" + strings.Repeat("a", 128)) },
	}
	for name, build := range refused {
		t.Run(name, func(t *testing.T) {
			before := rows()
			cred, in := build()
			enr, err := r.svc.Create(ctx, cred, in)
			if !errors.Is(err, ErrInvalidClaim) {
				t.Fatalf("Create = enrollment %s, %v; want ErrInvalidClaim", enr.ID, err)
			}
			if after := rows(); after != before {
				t.Fatalf("the refusal left %d enrollments, want %d: it writes no row", after, before)
			}
		})
	}
	t.Run("128 characters long is accepted", func(t *testing.T) {
		longest := "l" + strings.Repeat("a", 127)
		cred, in := pod(longest)
		enr, err := r.svc.Create(ctx, cred, in)
		if err != nil {
			t.Fatalf("Create with a 128-character claim: %v", err)
		}
		if _, claim := ownerAndClaimOf(t, r.svc, enr.ID); claim != longest {
			t.Fatalf("the enrollment records claim %q, want the one stated", claim)
		}
	})
}
