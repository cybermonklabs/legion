package delivery

import (
	"sync"
	"testing"
	"testing/synctest"

	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
	"github.com/sjawhar/envoy/internal/testwait"
)

// reservationHeld is the span a read that must wait is read after (testwait.Idle), and
// reservationHeldWhy is why: callBudget arms no timer, and the reservation a read waits behind is
// held across the other read's whole attribution read (readAttributionInputs reserves before
// fetchAttributionFacts and settles after it), so the span is the longest that read can take on
// the bounds the product puts on it. Its token asks make at most attributionTokenCalls calls, one
// attempt each, and each of its maxAttributionCalls pages (the pull request, then up to
// maxCommitPages commit pages) is read through readGitHubPage, whose githubPageAttempts attempts
// take up to pageHeld. Each attempt and each token call is bounded by githubapp.RequestTimeout,
// the client's own bound on a call (Client.requestTimeout), which cmd/dispatch leaves at that.
// After a call GitHub answers with a rate limit, githubapp.Client.request writes that limit to
// the shared ledger, for up to githubapp.LimitRecordTimeout, before the call returns. githubapp
// reads an answer as a limit only on a 403 or a 429 (rateLimitAt), which readGitHubPage does not
// ask again (retryGitHubPage) and which ends the read: Client.installation and Client.token
// return the limit, and fetchPullRequestPayload and fetchCommitMessagesWithToken fail on every
// status but 200 they do not handle. So a read makes that write at most once, and the span counts
// it once. The read of the limit in force that Client.send makes before each call is a database
// statement no bound of the product's covers, and the span does not count it.
const (
	// attributionTokenCalls is the most GitHub calls fetchAttributionFacts' token asks make in one
	// read: githubapp.Client.RepositoryToken calls GitHub only to look the repository's
	// installation up (Client.installation) when no lookup is cached, and to mint the
	// installation's token (Client.token) when no token is cached. A lookup stays cached for
	// githubapp's installationsCacheTTL (10 min) and a minted token until githubapp's
	// tokenExpirySlack before its hour ends, both longer than reservationHeld, so a read that asks
	// for a token before each page makes each of the two calls at most once.
	attributionTokenCalls = 2
	// pageHeld is the longest readGitHubPage takes on one page: githubPageAttempts attempts of up
	// to githubapp.RequestTimeout each, and after every attempt but the last a wait of
	// githubPageRetryWait times the attempt just made (waitBeforeRetry), which add up to
	// githubPageRetryWait times 1 + 2 + ... + (githubPageAttempts-1).
	pageHeld = githubPageAttempts*githubapp.RequestTimeout +
		githubPageAttempts*(githubPageAttempts-1)/2*githubPageRetryWait
	reservationHeld = attributionTokenCalls*githubapp.RequestTimeout + maxAttributionCalls*pageHeld +
		githubapp.LimitRecordTimeout
	reservationHeldWhy = "callBudget arms no timer, and the read a reserve waits behind holds its reservation across its " +
		"whole fetchAttributionFacts: attributionTokenCalls token calls (the installation lookup and the mint) and " +
		"maxAttributionCalls pages, each read through readGitHubPage in up to githubPageAttempts attempts with " +
		"githubPageRetryWait times the attempt just made between them, each call and attempt bounded by " +
		"githubapp.RequestTimeout, and at most one write of a rate limit GitHub answered to the shared ledger, " +
		"bounded by githubapp.LimitRecordTimeout, since such an answer ends the read: a reserve that gave up at a " +
		"bound of its own shorter than that would give up while that read is still inside its calls"
)

// TestACallBudgetWaitsForAReservationThenGivesUpOnceSpent: a read that does not fit while another
// holds a reservation waits for it to settle, takes what the settle gave back, and a read that does
// not fit once nothing is in flight gives up at once rather than waiting forever. The budget does
// no I/O, so the test runs in a synctest bubble: synctest.Wait returns once every read has
// answered or blocks in reserve, and what has answered is then read from its channel. A read that
// must wait is read reservationHeld on the bubble's clock later (testwait.Idle), past any bound of
// its own up to that span.
func TestACallBudgetWaitsForAReservationThenGivesUpOnceSpent(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		budget := newCallBudget(10)
		if !budget.reserve(6) {
			t.Fatal("the first reservation of 6 from 10 was refused")
		}
		second := make(chan bool, 1)
		go func() { second <- budget.reserve(6) }()
		testwait.Idle(t, reservationHeld, reservationHeldWhy)
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
		// open opens the gate once, below or as the row ends: a row that failed while granted
		// reads still waited on the gate would otherwise leave them blocked in the bubble once the
		// test goroutine had ended, which synctest.Test ends in a panic rather than in the row's
		// failure.
		open := sync.OnceFunc(func() { close(gate) })
		// Each read reports its reservation's answer the moment reserve returns.
		answered := make(chan bool, reads)
		var wg sync.WaitGroup
		defer func() {
			open()
			wg.Wait()
		}()
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
		// other read is waiting on a reservation and none may answer: once every read has
		// answered or blocks, exactly three answers are there, and none more reservationHeld on
		// (testwait.Idle).
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
		testwait.Idle(t, reservationHeld, reservationHeldWhy)
		select {
		case granted := <-answered:
			t.Fatalf("a reservation answered %v while %d reads held %d of %d calls; want it to wait", granted, held, held*cost, allowance)
		default:
		}

		open()
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
