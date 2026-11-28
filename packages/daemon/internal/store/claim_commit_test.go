package store

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testpg/testdb"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// A claim write that queues its issue's role-holder publication wakes the issue's lane once its
// transaction has committed (PublishRoleHolders), even when the reply to its commit comes after
// the caller's cancel: the row the lane runs is in the store, so the wake is owed.
func TestAClaimWriteWhoseCommitReplyIsLostWakesItsRoleHolders(t *testing.T) {
	t.Parallel()
	st := migratedStore(t)
	openIssueLifecycle(t, st, holderIssue)
	if _, err := st.AdmitClaim(context.Background(), holderClaim(supervise.StateLaunching, "")); err != nil {
		t.Fatalf("admit the claim: %v", err)
	}
	proxied, fault := testdb.CommitFaults(t, st.Pool().Config().ConnString())
	faulted, err := Open(context.Background(), proxied)
	if err != nil {
		t.Fatalf("open the store through the fault proxy: %v", err)
	}
	t.Cleanup(faulted.Close)
	var mu sync.Mutex
	var woken []string
	faulted.PublishRoleHolders(func(issue string) {
		mu.Lock()
		defer mu.Unlock()
		woken = append(woken, issue)
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fired := fault.HoldReply("insert into outbox", 300*time.Millisecond)
	done := make(chan error, 1)
	go func() { done <- faulted.PutClaim(ctx, holderClaim(supervise.StateRegistered, "ses-1")) }()
	testdb.Met(t, fired)
	testwait.Eventually(t, "the claim's role-holder publication to be visible", func() bool {
		return slices.Equal(queuedRoleHolders(t, st), []string{holderIssue})
	})
	select {
	case err := <-done:
		t.Fatalf("the claim write returned %v before the cancel: its commit's reply came first, so the cancel tested nothing", err)
	default:
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("the claim write whose commit landed = %v, want nil", err)
		}
	case <-time.After(testwait.Bounded(t, time.Minute, time.Second)):
		t.Fatal("the claim write did not return after its cancel")
	}
	mu.Lock()
	defer mu.Unlock()
	if want := []string{holderIssue}; !slices.Equal(woken, want) {
		t.Errorf("the claim write woke %v, want %v: its publication committed", woken, want)
	}
}
