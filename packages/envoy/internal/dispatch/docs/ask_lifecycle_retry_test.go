package docs

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/clock/clocktest"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/testwait"
)

// stopSettlementShort makes each settlement of room that writes a version stop short before its
// commit, while stop answers true: its transaction rolls back having written nothing into the
// room, so the document still owes the settlement (settleRoomWithin).
func stopSettlementShort(service *Service, room string, stop func() bool) {
	service.afterSettleVersionWrite = func(settled string) error {
		if settled == room && stop() {
			return errors.New("injected settlement stop")
		}
		return nil
	}
}

// newSettleRetryService is newClockedTestService on fake, with its compaction queue paused from
// the start, as Quiesce pauses it. SettleIssueDocuments' budget and its pause between attempts run
// on the service's clock (Deps.Clock), a fake that moves only as the test moves it; so does the
// compaction queue, which waits on it for a room's first check compactFirstCheck after a commit
// notes the room (noteStoredUpdates), as a test's write does. Paused, the queue takes no note and
// arms nothing, so what is armed on fake is the call's alone. The queue stays paused: Shutdown
// stops it as it is.
func newSettleRetryService(t *testing.T, fake *clocktest.Fake) (*Service, string) {
	t.Helper()
	service, artifactID := newClockedTestService(t, fake)
	service.compactions.pause(context.Background())
	return service, artifactID
}

// settleInBackground runs SettleIssueDocuments for the issue DOC-1 on a goroutine of its own, so
// the test moves the service's clock while the call waits on it, and returns the channel the
// call's result is sent on.
func settleInBackground(service *Service) <-chan error {
	settled := make(chan error, 1)
	go func() {
		settled <- service.SettleIssueDocuments(store.WithTransactionTracking(context.Background()), "DOC-1")
	}()
	return settled
}

// passRetryPause waits for the pause after attempt, and nothing else, to be armed on fake,
// issueSettleRetryPause from its present - the whole pause, which the budget would cut short once
// nearly spent - and moves fake past it, which starts the next attempt.
func passRetryPause(t *testing.T, fake *clocktest.Fake, attempt int) {
	t.Helper()
	due := fake.Now().Add(issueSettleRetryPause)
	testwait.Until(t, fmt.Sprintf("the pause after attempt %d, and nothing else, to be armed on the service's clock at %v", attempt, due), func() ([]time.Time, bool) {
		armed := fake.Armed()
		return armed, len(armed) == 1 && armed[0].Equal(due)
	})
	fake.Advance(issueSettleRetryPause)
}

// A settlement that stops short is run again by SettleIssueDocuments: the block written within the
// settle delay is an ask, credited to its writer, when it returns, for the close that called it to
// withdraw. Settled once and given up on, the block would be no ask when the close committed.
//
// The budget and the pause run on the service's clock (newSettleRetryService), which the test
// moves only past the pause after the first attempt, once that pause alone is armed: the budget
// is far from spent when the second attempt starts, however long the first took.
func TestSettlingAnIssuesDocumentsRetriesASettlementThatStoppedShort(t *testing.T) {
	t.Parallel()
	fake := clocktest.New(time.Now())
	service, artifactID := newSettleRetryService(t, fake)
	seedServiceText(t, service, artifactID, "# Decision\n\nContext.\n")
	actor := model.Actor{Kind: "session", ID: "retried-asking-session"}
	writeThroughLedger(t, service, artifactID, actor, freshBlock, withoutVersion)

	// The first attempt stops short before its commit; the next commits.
	var attempts atomic.Int32
	service.afterSettleWarm = func(room string) {
		if room == artifactID {
			attempts.Add(1)
		}
	}
	stopSettlementShort(service, artifactID, func() bool { return attempts.Load() == 1 })
	settled := settleInBackground(service)
	passRetryPause(t, fake, 1)
	err := testwait.Receive(t, "settling the issue's documents to return after its second attempt", settled)
	service.afterSettleVersionWrite, service.afterSettleWarm = nil, nil
	if err != nil {
		t.Fatalf("settle the issue's documents: %v", err)
	}
	if got := attempts.Load(); got != 2 {
		t.Fatalf("the document's settlement ran %d times, want 2: the one that stopped short and the one after it", got)
	}
	ask := blockAskOn(t, service, artifactID, "fresh")
	if ask.State != "open" || ask.Author != actor {
		t.Fatalf("the settled block's ask is %s by %+v, want open by %+v", ask.State, ask.Author, actor)
	}
	requireSettledAtLatestVersion(t, service, artifactID)
}

// A settlement that stops short every time is given up after issueSettleAttempts attempts, well
// within issueSettleBudget, so the close waiting on it goes ahead; the document keeps its debt
// for the room's next load or the resumption, and its block is no ask yet.
//
// The budget and the pause between attempts run on the service's clock (newSettleRetryService):
// the test moves it past each pause once that pause alone is armed, issueSettleRetryPause on, and
// the call returns with the clock far short of the budget, having armed nothing else it waits on.
func TestSettlingAnIssuesDocumentsGivesUpOnASettlementThatNeverLands(t *testing.T) {
	t.Parallel()
	fake := clocktest.New(time.Now())
	service, artifactID := newSettleRetryService(t, fake)
	seedServiceText(t, service, artifactID, "# Decision\n\nContext.\n")
	writeThroughLedger(t, service, artifactID, model.Actor{Kind: "session", ID: "stalled-session"}, freshBlock, withoutVersion)

	var attempts atomic.Int32
	service.afterSettleWarm = func(room string) {
		if room == artifactID {
			attempts.Add(1)
		}
	}
	stopSettlementShort(service, artifactID, func() bool { return true })
	settled := settleInBackground(service)
	for attempt := 1; attempt < issueSettleAttempts; attempt++ {
		passRetryPause(t, fake, attempt)
	}
	err := testwait.Receive(t, "settling the issue's documents to give up after its last attempt", settled)
	service.afterSettleVersionWrite, service.afterSettleWarm = nil, nil
	if err == nil {
		t.Fatal("settling a document whose settlement never lands succeeded")
	}
	if got := attempts.Load(); got != issueSettleAttempts {
		t.Fatalf("the document's settlement ran %d times, want %d", got, issueSettleAttempts)
	}
	if armed := fake.Armed(); len(armed) != 0 {
		t.Fatalf("settling the issue's documents returned leaving %v armed on the service's clock", armed)
	}
	owed, pendingErr := settlementPending(context.Background(), service.store.Pool, artifactID)
	if pendingErr != nil || !owed {
		t.Fatalf("the document owes a settlement = %t (%v), want true", owed, pendingErr)
	}
	if count := asksOn(t, service, artifactID); count != 0 {
		t.Fatalf("a settlement that never landed indexed %d asks", count)
	}
}
