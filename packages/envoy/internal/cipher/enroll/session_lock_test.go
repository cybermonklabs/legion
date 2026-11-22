package enroll

import (
	"context"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/cipher/store"
	"github.com/sjawhar/envoy/internal/cipher/store/storetest"
)

// TestAnAdoptionReadsItsPredecessorsUnderTheSessionsLock: the box a session ran on ended for
// good holding nothing (an operator's revoke of its automatic grant leaves it so). An operator's
// revoke of another grant of the session is in flight: its transaction holds the session's lock
// (store.LockSession, which requests.Machine.RevokeByApprover takes before it locks the session's
// live boxes) and has not yet written the name it withholds. The session enrols on a new box. Its
// adoption waits for the session's lock, so it reads its predecessors only once the revoke has
// committed, finds the box that holds the withheld name, and takes the name: the new box holds it
// back. The row fails when adopt takes no session lock: the enrolment reads its predecessors before
// the name is withheld, takes nothing, and never waits. It runs on the server's default level and
// on a pool whose transactions default to REPEATABLE READ (storetest.RepeatableReadByDefault):
// createAttempt names READ COMMITTED, and without the name the second case's enrolment waits but
// reads its predecessors in a snapshot taken before the wait.
func TestAnAdoptionReadsItsPredecessorsUnderTheSessionsLock(t *testing.T) {
	for _, repeatableRead := range []bool{false, true} {
		name := "the server's default level"
		if repeatableRead {
			name = "REPEATABLE READ by default"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			svc := newService(t)
			issuer, key := withPodVerifier(t, svc)
			session := "session-lock-" + uuid.NewString()[:8]
			cred, old := carryEnrollment(t, svc, issuer, key, "session", session)
			if err := svc.Revoke(ctx, cred, old.ID.String(), "launcher"); err != nil {
				t.Fatalf("Revoke: %v", err)
			}
			carry := carryPrincipalOf(t, svc, old.ID)
			if carry == "" {
				t.Fatal("the session's first box records no principal, so no adoption would match it and the row proves nothing")
			}

			revoking, err := svc.Store.Pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer revoking.Rollback(ctx)
			if err := store.LockSession(ctx, revoking, session, carry); err != nil {
				t.Fatal(err)
			}
			if repeatableRead {
				serverDefault := svc.Store.Pool
				svc.Store.Pool = storetest.RepeatableReadByDefault(t, svc.Store)
				t.Cleanup(func() { svc.Store.Pool = serverDefault })
			}

			runtime := session + "-2"
			in := Enrollment{Kind: "session", RuntimeID: runtime, Thumbprint: "tp-" + runtime, SessionID: str(session)}
			next := mintCredential(t, svc, str("ada@example.com"), nil, "devbox-"+runtime)
			type created struct {
				enr Enrollment
				err error
			}
			done := make(chan created, 1)
			go func() {
				enr, err := svc.Create(ctx, next, in)
				done <- created{enr, err}
			}()
			storetest.AwaitLockWaiters(t, svc.Store.Pool, revoking, 1)
			if _, err := revoking.Exec(ctx, `insert into withheld_secrets (enrollment_id, name) values ($1,$2)`, old.ID, "AUTO_TOKEN"); err != nil {
				t.Fatalf("withhold the name as the revoke does: %v", err)
			}
			if err := revoking.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			got := <-done
			if got.err != nil {
				t.Fatalf("the enrolment of the new box: %v", got.err)
			}
			if names := withheldNames(t, svc, got.enr.ID); !slices.Equal(names, []string{"AUTO_TOKEN"}) {
				t.Errorf("the new box withholds %v; want [AUTO_TOKEN], which the revoke it waited for withheld from the session", names)
			}
		})
	}
}
