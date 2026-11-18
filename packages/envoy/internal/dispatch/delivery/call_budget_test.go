package delivery

import (
	"sync"
	"testing"
	"testing/synctest"
)

// TestACallBudgetWaitsForAReservationThenGivesUpOnceSpent: a read that does not fit while another
// holds a reservation waits for it to settle, takes what the settle gave back, and a read that does
// not fit once nothing is in flight gives up at once rather than waiting forever. The budget does
// no I/O, so the test runs in a synctest bubble: synctest.Wait returns once every read has
// answered or blocks in reserve, and what has answered is then read from its channel.
func TestACallBudgetWaitsForAReservationThenGivesUpOnceSpent(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		budget := newCallBudget(10)
		if !budget.reserve(6) {
			t.Fatal("the first reservation of 6 from 10 was refused")
		}
		second := make(chan bool, 1)
		go func() { second <- budget.reserve(6) }()
		synctest.Wait()
		select {
		case got := <-second:
			t.Fatalf("the second reservation answered %v while the first held 6 of 10; want it to wait", got)
		default:
		}

		// The first read made 2 calls: 8 are left, so the waiting read takes its 6.
		budget.settle(6, 2)
		synctest.Wait()
		select {
		case got := <-second:
			if !got {
				t.Fatal("the second reservation was refused once the first gave 4 back (8 left)")
			}
		default:
			t.Fatal("the second reservation still waited after the first settled")
		}

		// It made all 6: 2 are left and nothing is in flight, so a third read gives up at once.
		budget.settle(6, 6)
		third := make(chan bool, 1)
		go func() { third <- budget.reserve(6) }()
		synctest.Wait()
		select {
		case got := <-third:
			if got {
				t.Fatal("a third reservation of 6 was granted with 2 of 10 left")
			}
		default:
			t.Fatal("a third reservation waited with nothing in flight to give calls back")
		}
		if budget.spent != 8 {
			t.Fatalf("spent = %d, want 8", budget.spent)
		}
	})
}

// TestConcurrentReadsNeverOverspendACallBudget: ten reads reserving 6 calls each against 20 (60
// reserved against 20 available) all start at once, and every granted read holds its reservation
// until the test releases them, so the rest must wait. Each makes 2 calls: exactly 8 are granted
// (the ninth would need 6 with 4 left), the other two give up, and no more than 20 calls are made.
// It runs in a synctest bubble, as TestACallBudgetWaitsForAReservationThenGivesUpOnceSpent does.
func TestConcurrentReadsNeverOverspendACallBudget(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const reads, cost, used, allowance = 10, 6, 2, 20
		budget := newCallBudget(allowance)
		gate := make(chan struct{})
		// Each read reports its reservation's answer the moment reserve returns.
		answered := make(chan bool, reads)
		var wg sync.WaitGroup
		for range reads {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if !budget.reserve(cost) {
					answered <- false
					return
				}
				answered <- true
				<-gate
				budget.settle(cost, used)
			}()
		}

		// The first three to ask fit (18 of 20). Nothing settles until the gate opens, so every
		// other read is waiting on a reservation and none may answer: once every read has answered
		// or blocks, exactly three answers are there.
		synctest.Wait()
		held := allowance / cost
		for i := range held {
			select {
			case granted := <-answered:
				if !granted {
					t.Fatalf("reservation %d of %d was refused with %d of %d calls free", i+1, held, allowance-i*cost, allowance)
				}
			default:
				t.Fatalf("only %d of %d reservations answered before any settled", i, held)
			}
		}
		select {
		case granted := <-answered:
			t.Fatalf("a reservation answered %v while %d reads held %d of %d calls; want it to wait", granted, held, held*cost, allowance)
		default:
		}

		close(gate)
		wg.Wait()
		close(answered)
		granted, refused := held, 0
		for answer := range answered {
			if answer {
				granted++
			} else {
				refused++
			}
		}
		if granted != 8 || refused != 2 {
			t.Fatalf("granted %d and refused %d of %d reads; want 8 and 2", granted, refused, reads)
		}
		if budget.spent > allowance || budget.spent != 16 {
			t.Fatalf("spent %d calls; want 16, never more than %d", budget.spent, allowance)
		}
	})
}
