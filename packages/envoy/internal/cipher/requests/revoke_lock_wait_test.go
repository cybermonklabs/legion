package requests

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/cipher/projectaccess/projectaccesstest"
	"github.com/sjawhar/envoy/internal/cipher/proxyrules"
	"github.com/sjawhar/envoy/internal/cipher/record"
	"github.com/sjawhar/envoy/internal/cipher/store/storetest"
)

// adoptionInFlight is an adoption of enrollment from, held open: a transaction that locks from's
// row for update, as enroll.adopt locks its predecessors, and runs move under that lock, not yet
// committed, so a test can wait for another backend to block on it (waitBlockedBy) before it
// commits.
func adoptionInFlight(t *testing.T, m *Machine, from string, move func(tx pgx.Tx) error) pgx.Tx {
	t.Helper()
	tx, err := m.Store.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	if _, err := tx.Exec(t.Context(), `select 1 from enrollments where id=$1 for update`, from); err != nil {
		t.Fatal(err)
	}
	if err := move(tx); err != nil {
		t.Fatalf("the adoption's move: %v", err)
	}
	return tx
}

// waitBlockedBy waits until some backend waits on a lock behind holder's backend
// (storetest.LockWaitersBehind), failing t when done answers first, since then the call under
// test never waited on the lock and the row proves nothing.
func waitBlockedBy(t *testing.T, m *Machine, holder pgx.Tx, done <-chan error) {
	t.Helper()
	pid := holder.Conn().PgConn().PID()
	deadline := time.Now().Add(60 * time.Second)
	for len(storetest.LockWaitersBehind(t, m.Store.Pool, pid)) == 0 {
		select {
		case err := <-done:
			t.Fatalf("the revoke returned %v before it waited on the adoption's lock", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("the revoke never waited on the adoption's lock")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestARevokeWaitingOnAnAdoptionDecidesOnWhereTheAdoptionLeftTheGrant: the case
// TestARevokeDecidesOnlyOnTheGrantTheRuleJudged holds, with the move committing while the revoke's
// transaction waits for the lock of the enrollment the rule judged, the window inside the
// transaction. The model judge granted SHARED_KEY to the fixture's session in anyone's stead,
// its reason names INFRA-1353, and INFRA is private with carol alone on its list. An adoption
// holds the session's row and has moved the grant to a service's pod, which names no operator. The
// session's operator, exempt from the rule as the old holder's operator, revokes; the revoke waits
// on the adoption's lock, and the adoption commits. The transaction must see the grant where the
// adoption left it, start again from the rule, and answer the operator as for an unknown grant,
// with the grant live. It holds on the server's default level and on a pool whose transactions
// default to REPEATABLE READ (storetest.RepeatableReadByDefault), since revokeJudged names READ
// COMMITTED: without the name, the second case's transaction reads the grant as it was before the
// move, and the grant's update fails with a serialization error instead.
func TestARevokeWaitingOnAnAdoptionDecidesOnWhereTheAdoptionLeftTheGrant(t *testing.T) {
	for _, repeatableRead := range []bool{false, true} {
		name := "the server's default level"
		if repeatableRead {
			name = "REPEATABLE READ by default"
		}
		t.Run(name, func(t *testing.T) {
			m, session, key := newJudgeFixture(t)
			grantID := judgedGrant(t, m, session, key, 600, "SHARED_KEY")
			answerFrom(t, m, map[string]projectaccesstest.Row{"INFRA": {Visibility: "private", Owner: otherPerson, Listed: []string{insider}}})
			var operator *string
			if err := m.Store.Pool.QueryRow(t.Context(), `select e.operator from grants g join enrollments e on e.id=g.enrollment_id where g.id=$1`, grantID).Scan(&operator); err != nil || deref(operator) == "" {
				t.Fatalf("the judged grant's holder names operator %q, %v; want one", deref(operator), err)
			}
			login := *operator
			pod, _ := newServiceEnrollment(t, m.Store, fixtureService, fixtureSubject, "pod-"+t.Name())
			adopt := adoptionInFlight(t, m, session, func(tx pgx.Tx) error {
				_, err := tx.Exec(t.Context(), `with moved as (update grants set enrollment_id=$2 where id=$1 returning request_id)
					update requests set enrollment_id=$2 where id=(select request_id from moved)`, grantID, pod)
				return err
			})
			if repeatableRead {
				serverDefault := m.Store.Pool
				m.Store.Pool = storetest.RepeatableReadByDefault(t, m.Store)
				t.Cleanup(func() { m.Store.Pool = serverDefault })
			}

			done := make(chan error, 1)
			go func() { done <- m.RevokeByApprover(t.Context(), grantID, login, record.HumanActor(login)) }()
			waitBlockedBy(t, m, adopt, done)
			if err := adopt.Commit(t.Context()); err != nil {
				t.Fatal(err)
			}
			if err := <-done; !errors.Is(err, pgx.ErrNoRows) {
				t.Errorf("RevokeByApprover(the judged grant, its former operator %s, moved by the adoption it waited on) = %v; want the answer an unknown grant gets (pgx.ErrNoRows)", login, err)
			}
			requireGrantLive(t, m, grantID)
		})
	}
}

// TestAnOperatorsRevokeDuringTheirSessionsAdoptionWithholdsWhereTheSessionRuns: the operator ends
// an automatic grant of their own session, so its name is withheld from the session from then on
// (withhold), on the enrollment the session runs on. The session's process has restarted on a
// second box, and its adoption moves the session's grants and requests there. Whether the adoption
// committed before the revoke (the control) or commits while the revoke waits on its lock, the
// name is withheld on the second box, where the session's next request asks, and not on the box
// it left.
func TestAnOperatorsRevokeDuringTheirSessionsAdoptionWithholdsWhereTheSessionRuns(t *testing.T) {
	for _, inFlight := range []bool{false, true} {
		name := "the adoption committed before the revoke"
		if inFlight {
			name = "the adoption commits while the revoke waits on its lock"
		}
		t.Run(name, func(t *testing.T) {
			m, enr, key, _ := newProjectFixture(t, proxyrules.Rules{}, publicTeam)
			_, grantID := grantOf(t, m, enr, key, "deploy the worker", "AUTO_TOKEN")
			second, _ := newEnrollment(t, m.Store, "session", "box-b-"+t.Name(), new(fixtureOperator), nil)
			adopt := adoptionInFlight(t, m, enr, func(tx pgx.Tx) error {
				if _, err := tx.Exec(t.Context(), `update grants set enrollment_id=$2 where enrollment_id=$1`, enr, second); err != nil {
					return err
				}
				_, err := tx.Exec(t.Context(), `update requests set enrollment_id=$2 where enrollment_id=$1`, enr, second)
				return err
			})
			if !inFlight {
				if err := adopt.Commit(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
			done := make(chan error, 1)
			go func() {
				done <- m.RevokeByApprover(t.Context(), grantID, fixtureOperator, record.HumanActor(fixtureOperator))
			}()
			if inFlight {
				waitBlockedBy(t, m, adopt, done)
				if err := adopt.Commit(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
			if err := <-done; err != nil {
				t.Fatalf("the operator's revoke of their session's grant = %v; want it ended", err)
			}
			var onLeft, onRuns int
			if err := m.Store.Pool.QueryRow(t.Context(), `select (select count(*) from withheld_secrets where enrollment_id=$1), (select count(*) from withheld_secrets where enrollment_id=$2)`, enr, second).Scan(&onLeft, &onRuns); err != nil {
				t.Fatal(err)
			}
			if onLeft != 0 || onRuns != 1 {
				t.Errorf("withheld names on the box the session left %d, on the box it runs on %d; want 0 and 1", onLeft, onRuns)
			}
		})
	}
}
