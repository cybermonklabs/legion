package oauthstore_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/oauthstore"
	"github.com/sjawhar/envoy/internal/cipher/store/storetest"
)

// moveAnswer is what one MovePodSessionBinding call answered.
type moveAnswer struct {
	binding  oauthstore.PodSessionBinding
	previous string
	err      error
}

// TestAMoveRacingANamesFirstRegistrationNamesItsRegistrar: the operator's move of a session name
// that no registrar is bound to yet, racing a registrar's first registration of it, binds the name
// to the registrar the move names and answers the racing registrar as the one it replaced.
// registrarA's first write of the name is held open once it has bound the name and inserted its
// row, before it commits (the store's write hook), and the move of the name to registrarB starts
// then; A's write is let go once the move has returned or is seen waiting on a lock behind A's
// backend. The move's read finds no binding, since A's is not committed, and its insert of one
// conflicts with A's on the name's primary key, so it waits; once A's commits, it reads the name
// again, finds A's binding and moves it. It answers the binding to B and A as the registrar it
// replaced, A's write stands as applied, and the table binds the name to B.
//
// Control: make the move's insert, when its read found no binding, the upsert it runs on a binding
// it found (on conflict (session_name) do update) with no second read. The move still waits on A's
// write and still binds the name to B, but answers no registrar replaced, so the log line of the
// move would hide the binding it took from A.
func TestAMoveRacingANamesFirstRegistrationNamesItsRegistrar(t *testing.T) {
	ctx := context.Background()
	_, pool := newStore(t)
	registering, moving := oauthstore.New(pool), oauthstore.New(pool)
	const session, podA = "implementer-g1", "pod-uid-of-a"
	heldBy := make(chan uint32, 1)
	resume := make(chan struct{})
	letGo := sync.OnceFunc(func() { close(resume) })
	defer letGo()
	oauthstore.SetPodSessionWriteHook(registering, func(ctx context.Context) {
		tx, _ := oauthstore.TxFrom(ctx)
		heldBy <- tx.Conn().PgConn().PID()
		<-resume
	})

	registered, moved := make(chan writeAnswer, 1), make(chan moveAnswer, 1)
	go func() {
		stored, err := registering.RegisterPodSession(ctx, registrarA, podA, session, "", 0)
		registered <- writeAnswer{stored, err}
	}()
	var pid uint32
	select {
	case pid = <-heldBy:
	case a := <-registered:
		t.Fatalf("%s's first write of %s = %+v, %v before it was held open, want it held after its insert", registrarA, session, a.stored, a.err)
	case <-time.After(10 * time.Second):
		t.Fatalf("%s's first write of %s was not held open after its insert within ten seconds", registrarA, session)
	}

	go func() {
		binding, previous, err := moving.MovePodSessionBinding(ctx, session, registrarB)
		moved <- moveAnswer{binding, previous, err}
	}()
	var answer moveAnswer
	returnedWhileHeld := false
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		select {
		case answer = <-moved:
			returnedWhileHeld = true
		default:
		}
		if returnedWhileHeld || len(storetest.LockWaitersBehind(t, pool, pid)) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the move of %s to %s had neither returned nor waited on a lock behind %s's held first write (backend %d) after ten seconds", session, registrarB, registrarA, pid)
		}
	}
	letGo()
	held := <-registered
	if !returnedWhileHeld {
		answer = <-moved
	}

	if held.err != nil || !held.stored.Applied {
		t.Errorf("%s's first write of %s for %s = %+v, %v; want it applied", registrarA, session, podA, held.stored, held.err)
	}
	if answer.err != nil || answer.binding.Session != session || answer.binding.Registrar != registrarB || answer.binding.RegisteredAt.IsZero() || answer.previous != registrarA {
		t.Errorf("the move of %s to %s, racing %s's first write of it = %+v, previous %q, %v; want %s bound to %s, replacing %s",
			session, registrarB, registrarA, answer.binding, answer.previous, answer.err, session, registrarB, registrarA)
	}
	if returnedWhileHeld {
		t.Errorf("the move of %s returned while %s's first write of it was held open, uncommitted; want it waiting on the name's binding until that write committed", session, registrarA)
	}
	if holder, _ := bindingOf(t, pool, session); holder != registrarB {
		t.Errorf("after the move, %s is bound to %q, want %s", session, holder, registrarB)
	}
}

// TestAMoveOpensATransactionOfItsOwn: MovePodSessionBinding, given a ctx that already carries a
// transaction, refuses and writes nothing: it reads the binding locked and moves it in a
// read-committed transaction of its own, whose level and lifetime a caller's transaction would
// choose instead.
func TestAMoveOpensATransactionOfItsOwn(t *testing.T) {
	ctx := context.Background()
	s, pool := newStore(t)
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, _, err := s.MovePodSessionBinding(oauthstore.WithTx(ctx, tx), "implementer-g1", registrarB); err == nil {
		t.Fatal("MovePodSessionBinding inside a caller's transaction = nil, want it refused")
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("roll back: %v", err)
	}
	if holder, _ := bindingOf(t, pool, "implementer-g1"); holder != "" {
		t.Fatalf("the refused move bound implementer-g1 to %q, want no binding", holder)
	}
}
