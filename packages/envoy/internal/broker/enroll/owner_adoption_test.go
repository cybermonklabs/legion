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
	plannerClaim     = "legion-acme-acme-12-planner"
	implementerClaim = "legion-acme-acme-12-implementer"
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
// claim is that claim's under the account the launcher's login proved; a pod that states none is
// its own pod's, and each slot of it its own slot's; and a machine's own session is that
// machine's, by the subject its credential proved.
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
	// A second role of the same pod, stating no claim: the slot the daemon gives each role process.
	unclaimedSlot := r.podEnrollment(t, launcher, workerPodAccount, "pod-uid-unclaimed")
	unclaimedSlot.SessionID, unclaimedSlot.Slot, unclaimedSlot.Thumbprint = str("unclaimed-slot-session"), "planner-g1", unclaimed.Thumbprint+"-planner-g1"

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
			name:  "a pod a launcher enrols under a claim belongs to that claim of the launcher's login",
			cred:  launcher,
			in:    claimed,
			owner: "claim:" + launcherAccount + "/" + plannerClaim, claim: plannerClaim, principal: "account:" + workerPodAccount,
		},
		{
			name:  "a pod that states no claim belongs to its own pod",
			cred:  launcher,
			in:    unclaimed,
			owner: "machine:legion-daemon/pod-uid-unclaimed", principal: "account:" + workerPodAccount,
		},
		{
			name:  "a slot of a pod that states no claim belongs to that slot of its pod",
			cred:  launcher,
			in:    unclaimedSlot,
			owner: "machine:legion-daemon/pod-uid-unclaimed/planner-g1", principal: "account:" + workerPodAccount,
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
		box := mintCredential(t, svc, str("ada@example.com"), nil, "example-host-1")
		live, err := svc.Create(ctx, box, Enrollment{Kind: "session", RuntimeID: "box-1", Thumbprint: "tp-box-1", SessionID: str("box-1-session")})
		if err != nil {
			t.Fatalf("Create on box 1: %v", err)
		}
		grant := insertLiveGrant(t, svc, live.ID)
		signIn := mintCredential(t, svc, str("ada@example.com"), nil, "example-host-2")
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

// TestAnEnrollmentWithNoOwnerKeepsTodaysRule: every enrollment live when migration 0034 applies
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
	// A service's login a person approved through a machine-login code: no subject, so no token
	// proved whose launcher it is.
	approved := mintCredential(t, r.svc, nil, str(podLoginService), "legion/legion-daemon")
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
		"a pod under a service login a person approved": func() (Credential, Enrollment) {
			in := r.podEnrollment(t, approved, workerPodAccount, "pod-uid-approved")
			in.Claim = plannerClaim
			return approved, in
		},
		"capital letters":     func() (Credential, Enrollment) { return pod("Legion-acme-acme-12-planner") },
		"a leading digit":     func() (Credential, Enrollment) { return pod("12-planner") },
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

// requestSessionOf is the session a request names for its wake (requests.session_id), "" for none.
func requestSessionOf(t *testing.T, svc *Service, request string) string {
	t.Helper()
	var session string
	if err := svc.Store.Pool.QueryRow(context.Background(), `select coalesce(session_id,'') from requests where id=$1`, request).Scan(&session); err != nil {
		t.Fatalf("read the session of request %s: %v", request, err)
	}
	return session
}

// adoptedFrom is the predecessor and the rule of the one enrollment.adopted row on enrollment to.
func adoptedFrom(t *testing.T, svc *Service, to uuid.UUID) (from uuid.UUID, rule string, dormant bool) {
	t.Helper()
	if err := svc.Store.Pool.QueryRow(context.Background(), `select (detail->>'from')::uuid, coalesce(detail->>'rule',''), (detail->>'dormant')::boolean
		from audit where kind='enrollment.adopted' and enrollment_id=$1`, to).Scan(&from, &rule, &dormant); err != nil {
		t.Fatalf("read the enrollment.adopted row of %s: %v", to, err)
	}
	return from, rule, dormant
}

// TestTheNextPodOfAClaimIsWokenForTheRequestItTook: a request names the session its expiry wakes
// (requests.session_id, which agent-secrets fills with the asking process's session). The claim
// rule moves a pending request to the next pod of the claim, which runs under a session of its
// own, so the request then names that pod's session: the expiry wakes the pod that is waiting on
// it, not the session of the pod whose box ended. A request that named no session still names
// none, and its wake falls back to the session of the enrollment it hangs off, the next pod's.
func TestTheNextPodOfAClaimIsWokenForTheRequestItTook(t *testing.T) {
	ctx := context.Background()
	r := newPodRig(t)
	launcher := r.login(t, launcherAccount, "pod-uid-daemon")
	first := claimPod(t, r, launcher, plannerClaim, "planner-session-1", "pod-uid-a")
	named := insertPendingRequest(t, r.svc, first.ID)
	if _, err := r.svc.Store.Pool.Exec(ctx, `update requests set session_id='planner-session-1' where id=$1`, named); err != nil {
		t.Fatalf("name the asking session on request %s: %v", named, err)
	}
	unnamed := insertPendingRequest(t, r.svc, first.ID)
	if err := r.svc.Revoke(ctx, launcher, first.ID.String(), "launcher"); err != nil {
		t.Fatalf("Revoke: %v", err)
	}

	next := claimPod(t, r, launcher, plannerClaim, "planner-session-2", "pod-uid-b")
	if holder := requestHolder(t, r.svc, named); holder != next.ID {
		t.Fatalf("request %s hangs off %s, want pod B's %s: this test measures what the move does to its wake", named, holder, next.ID)
	}
	if got := requestSessionOf(t, r.svc, named); got != "planner-session-2" {
		t.Fatalf("request %s names session %q after the move, want pod B's %q: its expiry would wake the session of the pod whose box ended", named, got, "planner-session-2")
	}
	if got := requestSessionOf(t, r.svc, unnamed); got != "" {
		t.Fatalf("request %s names session %q after the move, want none, as it was asked", unnamed, got)
	}
}

// TestAPodOfTheClaimUpBeforeTheLastOneEndedTakesWhatItLeft: the next pod of a claim can enrol
// before the broker hears that the pod before it is gone — the daemon's revoke lands late, or
// never, and the old row only lapses. The claim rule at enrolment then finds no dormant row to
// take from, so the end of the old row, when it comes, hands what it holds to the live pod of the
// claim instead of leaving it dormant for a pod that may never come. An approval given in between
// lands on the old row and moves with it. A live pod of another claim takes nothing, and with no
// live pod of the claim the end leaves the row dormant, as before.
func TestAPodOfTheClaimUpBeforeTheLastOneEndedTakesWhatItLeft(t *testing.T) {
	ctx := context.Background()
	t.Run("the daemon's revoke lands after the next pod enrolled", func(t *testing.T) {
		r := newPodRig(t)
		launcher := r.login(t, launcherAccount, "pod-uid-daemon")
		first := claimPod(t, r, launcher, plannerClaim, "planner-session-1", "pod-uid-a")
		request := insertPendingRequest(t, r.svc, first.ID)
		grant := insertLiveGrant(t, r.svc, first.ID)
		other := claimPod(t, r, launcher, implementerClaim, "implementer-session-1", "pod-uid-c")
		next := claimPod(t, r, launcher, plannerClaim, "planner-session-2", "pod-uid-b")
		if holder := requestHolder(t, r.svc, request); holder != first.ID {
			t.Fatalf("request %s hangs off %s before pod A ended, want pod A's %s: a live pod of the claim keeps what it holds", request, holder, first.ID)
		}

		if err := r.svc.Revoke(ctx, launcher, first.ID.String(), "launcher"); err != nil {
			t.Fatalf("Revoke: %v", err)
		}
		if holder := requestHolder(t, r.svc, request); holder != next.ID {
			t.Fatalf("request %s hangs off %s after pod A ended, want pod B's %s", request, holder, next.ID)
		}
		if holder := grantHolder(t, r.svc, grant); holder != next.ID {
			t.Fatalf("grant %s hangs off %s after pod A ended, want pod B's %s", grant, holder, next.ID)
		}
		if left := readCarryState(t, r.svc, first.ID); !left.Ended || left.Dormant || left.LiveGrants != 0 || left.Pending != 0 {
			t.Fatalf("pod A is %+v after its end, want it ended, not dormant and holding nothing", left)
		}
		if held := readCarryState(t, r.svc, other.ID); held.LiveGrants != 0 || held.Pending != 0 {
			t.Fatalf("the other claim's live pod holds %+v, want nothing", held)
		}
		if from, rule, dormant := adoptedFrom(t, r.svc, next.ID); from != first.ID || rule != "claim" || dormant {
			t.Fatalf("pod B's enrollment.adopted row names %s by rule %q (dormant %t), want pod A's %s by the claim rule off a row that never went dormant", from, rule, dormant, first.ID)
		}
		var handedTo string
		if err := r.svc.Store.Pool.QueryRow(ctx, `select coalesce(detail->>'handed_to','') from audit where kind='enrollment.revoked' and enrollment_id=$1`, first.ID).Scan(&handedTo); err != nil || handedTo != next.ID.String() {
			t.Fatalf("pod A's enrollment.revoked row hands to %q (%v), want pod B's %s", handedTo, err, next.ID)
		}
	})
	t.Run("the revoke never lands, an approval grants onto the lapsed row and the sweep ends it", func(t *testing.T) {
		r := newPodRig(t)
		launcher := r.login(t, launcherAccount, "pod-uid-daemon")
		first := claimPod(t, r, launcher, plannerClaim, "planner-session-1", "pod-uid-a")
		request := insertPendingRequest(t, r.svc, first.ID)
		next := claimPod(t, r, launcher, plannerClaim, "planner-session-2", "pod-uid-b")
		lapseLease(t, r.svc, first.ID)
		grant := approveOnTheDormantRow(t, r.svc, first.ID, request)

		if _, err := r.svc.EndLapsed(ctx); err != nil {
			t.Fatalf("EndLapsed: %v", err)
		}
		if holder := grantHolder(t, r.svc, grant); holder != next.ID {
			t.Fatalf("grant %s hangs off %s after the sweep ended pod A, want pod B's %s: only a third pod would take it from a dormant row", grant, holder, next.ID)
		}
		if left := readCarryState(t, r.svc, first.ID); !left.Ended || left.Dormant || left.LiveGrants != 0 {
			t.Fatalf("pod A is %+v after the sweep, want it ended, not dormant and holding nothing", left)
		}
	})
	t.Run("with no live pod of the claim the end leaves the row dormant", func(t *testing.T) {
		r := newPodRig(t)
		launcher := r.login(t, launcherAccount, "pod-uid-daemon")
		first := claimPod(t, r, launcher, plannerClaim, "planner-session-1", "pod-uid-a")
		grant := insertLiveGrant(t, r.svc, first.ID)
		other := claimPod(t, r, launcher, implementerClaim, "implementer-session-1", "pod-uid-c")
		if err := r.svc.Revoke(ctx, launcher, first.ID.String(), "launcher"); err != nil {
			t.Fatalf("Revoke: %v", err)
		}
		if left := readCarryState(t, r.svc, first.ID); !left.Dormant || left.LiveGrants != 1 {
			t.Fatalf("pod A is %+v after its end, want it dormant holding its grant for the claim's next pod", left)
		}
		if holder := grantHolder(t, r.svc, grant); holder != first.ID {
			t.Fatalf("grant %s hangs off %s, want pod A's %s: another claim's live pod takes nothing", grant, holder, first.ID)
		}
		if held := readCarryState(t, r.svc, other.ID); held.LiveGrants != 0 {
			t.Fatalf("the other claim's live pod holds %+v, want nothing", held)
		}
	})
}

// TestTheSameClaimUnderAnotherServiceTakesNothing: a claim is an owner under the login whose
// launcher stated it, claim:<login subject>/<claim>, so a pod another service's launcher enrols
// under the same claim token is another owner: it takes nothing a dormant pod of the first
// service's claim left, at its enrolment or at that pod's end.
func TestTheSameClaimUnderAnotherServiceTakesNothing(t *testing.T) {
	ctx := context.Background()
	const otherService, otherPodAccount = "other-service", "system:serviceaccount:other:other-worker"
	r := newPodRig(t)
	r.svc.ServiceLogins[otherLauncherAccount] = otherService
	r.svc.ServicesByAccount[otherPodAccount] = otherService
	launcher := r.login(t, launcherAccount, "pod-uid-daemon")
	theirs := r.login(t, otherLauncherAccount, "pod-uid-other-daemon")
	theirPod := func(session, podUID string) Enrollment {
		t.Helper()
		in := r.podEnrollment(t, theirs, otherPodAccount, podUID)
		in.SessionID, in.Claim = str(session), plannerClaim
		enr, err := r.svc.Create(ctx, theirs, in)
		if err != nil {
			t.Fatalf("enrol the other service's pod %s: %v", podUID, err)
		}
		return enr
	}

	live := theirPod("other-planner-session-1", "pod-uid-other-a")
	first := claimPod(t, r, launcher, plannerClaim, "planner-session-1", "pod-uid-a")
	grant := insertLiveGrant(t, r.svc, first.ID)
	if err := r.svc.Revoke(ctx, launcher, first.ID.String(), "launcher"); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	later := theirPod("other-planner-session-2", "pod-uid-other-b")
	if owner, _ := ownerAndClaimOf(t, r.svc, later.ID); owner != "claim:"+otherLauncherAccount+"/"+plannerClaim {
		t.Fatalf("the other service's pod records owner %q, want %q", owner, "claim:"+otherLauncherAccount+"/"+plannerClaim)
	}
	if holder := grantHolder(t, r.svc, grant); holder != first.ID {
		t.Fatalf("grant %s hangs off %s, want the dormant pod's %s: the same claim token under another service is another owner", grant, holder, first.ID)
	}
	for _, e := range []Enrollment{live, later} {
		if held := readCarryState(t, r.svc, e.ID); held.LiveGrants != 0 {
			t.Fatalf("the other service's pod %s holds %+v, want nothing", e.ID, held)
		}
	}
}

// TestAnotherDaemonOfTheServiceStatingTheClaimTakesNothing: every project's Legion daemon logs in
// as the one service legion-daemon, each under an account of its own, and their pods may run as
// one account. A claim is an owner under the login that stated it, so one project's daemon whose
// pod states another project's claim token is another owner, recorded under its own login: its
// pods take nothing a pod of the first daemon's claim left, neither when that pod's end looks for
// a live pod of the claim to hand its grant to (handOver) nor when they enrol after it (adopt).
// The first daemon's own next pod of the claim still takes it.
func TestAnotherDaemonOfTheServiceStatingTheClaimTakesNothing(t *testing.T) {
	ctx := context.Background()
	const otherDaemonAccount = "system:serviceaccount:legion-other:legion-daemon"
	rig := func(t *testing.T) (podRig, Credential, Credential) {
		t.Helper()
		r := newPodRig(t)
		r.svc.ServiceLogins[otherDaemonAccount] = podLoginService
		launcher := r.login(t, launcherAccount, "pod-uid-daemon")
		theirs := r.login(t, otherDaemonAccount, "pod-uid-other-daemon")
		if launcher.Service == nil || theirs.Service == nil || *launcher.Service != *theirs.Service {
			t.Fatalf("the two daemons log in as services %v and %v, want the one service %q: this test measures two logins of one service", launcher.Service, theirs.Service, podLoginService)
		}
		return r, launcher, theirs
	}
	theirsHoldNothing := func(t *testing.T, r podRig, pods ...Enrollment) {
		t.Helper()
		for _, e := range pods {
			if held := readCarryState(t, r.svc, e.ID); held.LiveGrants != 0 || held.Pending != 0 {
				t.Fatalf("the other daemon's pod %s holds %+v, want nothing of the first daemon's claim", e.ID, held)
			}
			if owner, _ := ownerAndClaimOf(t, r.svc, e.ID); owner != "claim:"+otherDaemonAccount+"/"+plannerClaim {
				t.Fatalf("the other daemon's pod %s records owner %q, want %q", e.ID, owner, "claim:"+otherDaemonAccount+"/"+plannerClaim)
			}
		}
	}
	t.Run("its live pod is handed nothing at the end of the first daemon's pod", func(t *testing.T) {
		r, launcher, theirs := rig(t)
		live := claimPod(t, r, theirs, plannerClaim, "other-planner-session-1", "pod-uid-other-a")
		first := claimPod(t, r, launcher, plannerClaim, "planner-session-1", "pod-uid-a")
		grant := insertLiveGrant(t, r.svc, first.ID)
		request := insertPendingRequest(t, r.svc, first.ID)
		if err := r.svc.Revoke(ctx, launcher, first.ID.String(), "launcher"); err != nil {
			t.Fatalf("Revoke: %v", err)
		}
		if left := readCarryState(t, r.svc, first.ID); !left.Dormant || left.LiveGrants != 1 || left.Pending != 1 {
			t.Fatalf("the first daemon's pod is %+v after its end, want it dormant holding its grant and its request: the other daemon's live pod is no pod of this claim", left)
		}
		theirsHoldNothing(t, r, live)
		if holder := requestHolder(t, r.svc, request); holder != first.ID {
			t.Fatalf("request %s hangs off %s, want the dormant pod's %s", request, holder, first.ID)
		}
		next := claimPod(t, r, launcher, plannerClaim, "planner-session-2", "pod-uid-b")
		if holder := grantHolder(t, r.svc, grant); holder != next.ID {
			t.Fatalf("grant %s hangs off %s, want the first daemon's next pod %s: the claim's own login still takes what its dormant pod left", grant, holder, next.ID)
		}
	})
	t.Run("its pod that enrols after takes nothing the first daemon's dormant pod left", func(t *testing.T) {
		r, launcher, theirs := rig(t)
		first := claimPod(t, r, launcher, plannerClaim, "planner-session-1", "pod-uid-a")
		grant := insertLiveGrant(t, r.svc, first.ID)
		request := insertPendingRequest(t, r.svc, first.ID)
		if err := r.svc.Revoke(ctx, launcher, first.ID.String(), "launcher"); err != nil {
			t.Fatalf("Revoke: %v", err)
		}
		later := claimPod(t, r, theirs, plannerClaim, "other-planner-session-2", "pod-uid-other-b")
		theirsHoldNothing(t, r, later)
		if holder := grantHolder(t, r.svc, grant); holder != first.ID {
			t.Fatalf("grant %s hangs off %s, want the dormant pod's %s: another daemon's login stating the claim is another owner", grant, holder, first.ID)
		}
		if holder := requestHolder(t, r.svc, request); holder != first.ID {
			t.Fatalf("request %s hangs off %s, want the dormant pod's %s", request, holder, first.ID)
		}
	})
}

// TestAMachinesGrantStaysWithItsSession: a machine's own requests are the machine's to read and
// cancel, but its grants follow the session that asked, as a person's do. A second session of the
// same machine, live when the first one's box ends, is handed nothing, a third enrolling after
// takes nothing, and the first session, resumed, gets its grant back by the session carry.
func TestAMachinesGrantStaysWithItsSession(t *testing.T) {
	ctx := context.Background()
	r := newPodRig(t)
	subject := exchange.MachineSubject(exchange.GoogleIdentity{Account: machineAccountEmail}, "tp-machine")
	machine := machineCredentialFor(t, r.svc, subject, "tp-machine")
	session := func(name, runtime string) Enrollment {
		t.Helper()
		enr, err := r.svc.Create(ctx, machine, Enrollment{Kind: "session", RuntimeID: runtime, Thumbprint: "tp-" + runtime, SessionID: str(name)})
		if err != nil {
			t.Fatalf("enrol the machine's session %s on %s: %v", name, runtime, err)
		}
		return enr
	}
	first := session("machine-session-1", "machine-box-1")
	grant := insertLiveGrant(t, r.svc, first.ID)
	sibling := session("machine-session-2", "machine-box-2")
	if err := r.svc.Revoke(ctx, machine, first.ID.String(), "launcher"); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if left := readCarryState(t, r.svc, first.ID); !left.Dormant || left.LiveGrants != 1 {
		t.Fatalf("the first session's end left %+v, want it dormant holding its grant: a machine's grant is not handed to another session of the machine", left)
	}
	later := session("machine-session-3", "machine-box-3")
	if holder := grantHolder(t, r.svc, grant); holder != first.ID {
		t.Fatalf("grant %s hangs off %s after another session of the machine enrolled, want the dormant first session's %s", grant, holder, first.ID)
	}
	resumed := session("machine-session-1", "machine-box-4")
	if holder := grantHolder(t, r.svc, grant); holder != resumed.ID {
		t.Fatalf("grant %s hangs off %s, want the resumed session's %s", grant, holder, resumed.ID)
	}
	for _, other := range []Enrollment{sibling, later} {
		if held := readCarryState(t, r.svc, other.ID); held.LiveGrants != 0 {
			t.Fatalf("the machine's other session %s holds %+v, want no grant", other.ID, held)
		}
	}
}
