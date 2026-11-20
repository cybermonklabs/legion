package architecture

import (
	"context"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/testwait"
)

// Two callers that both refused one flight share the next one, however late each looks again.
// Both join a flight held in its blob reads after its head lookup, so each refuses it once it
// ends. The second to refuse it is held there until the first has started the next flight and
// been answered by it; released, it takes that flight's answer rather than starting a third, so
// the three callers make two head lookups: the first flight's, and the next one's, which the two
// refusers share.
func TestCallersThatRefusedOneFlightShareTheNextOneHoweverLateTheyLook(t *testing.T) {
	branch := newMovingBranch(t, commitOne, map[string]map[string]string{commitOne: validFiles()})
	var lookups atomic.Int32
	inner := branch.handler()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/{owner}/{repo}/commits/{branch}", func(w http.ResponseWriter, r *http.Request) {
		lookups.Add(1)
		inner.ServeHTTP(w, r)
	})
	mux.Handle("/", inner)
	client := newGitHubClient(t, mux)
	database := storetest.Open(t)
	if _, err := database.Pool.Exec(context.Background(), `
		insert into projects (key, name, default_assignee) values ('CORE', 'Core', 'alice');
		insert into architecture_sources (project_key, repo, branch, installation_id, created_by)
		values ('CORE', 'legion/arch', 'main', 11, '{"kind":"user","id":"alice"}');
	`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	process := NewImporter(database, client, events.NewBroker())
	t.Cleanup(process.Close)

	var taken, refusals atomic.Int32
	late := make(chan struct{})
	releaseLate := sync.OnceFunc(func() { close(late) })
	// Cleanups run last registered first: a test that fails while the refuser is held lets it go
	// before Close waits for the flights.
	t.Cleanup(releaseLate)
	process.afterFlightTaken = func() { taken.Add(1) }
	process.afterSyncRefusal = func() {
		if refusals.Add(1) == 2 {
			<-late
		}
	}

	branch.holdBlobs(commitOne)
	first := syncInBackground(process)
	branch.waitHeld(t)
	refusers := make(chan syncAnswer, 2)
	for range 2 {
		go func() {
			source, err := process.Sync(context.Background(), "CORE", archRepo)
			refusers <- syncAnswer{source: source, err: err}
		}()
	}
	// The first flight is held until the release below, so a caller that holds a flight before
	// then holds that one.
	testwait.Eventually(t, "the two later callers to join the held flight", func() bool { return taken.Load() >= 3 })
	branch.releaseHeld()

	if answer := waitForSync(t, first, "the first caller"); answer.err != nil {
		t.Fatalf("the first caller: %v", answer.err)
	}
	// The held refuser cannot answer before releaseLate, so the first answer is the other's.
	if answer := waitForSync(t, refusers, "the refuser the hold let through"); answer.err != nil {
		t.Fatalf("the refuser the hold let through: %v", answer.err)
	}
	releaseLate()
	answer := waitForSync(t, refusers, "the refuser held until the other was answered")
	if answer.err != nil || answer.source.LastCommit == nil || *answer.source.LastCommit != commitOne {
		t.Fatalf("the late refuser answered %+v (err %v); want the row at %s with no error", answer.source, answer.err, commitOne)
	}

	t.Logf("refusals=%d head lookups=%d", refusals.Load(), lookups.Load())
	if got := refusals.Load(); got != 2 {
		t.Fatalf("the two later callers refused %d flights between them, want 2: without both refusals the head lookups say nothing about refusers", got)
	}
	if got := lookups.Load(); got != 2 {
		t.Fatalf("three callers, two of which refused the first flight, made %d head lookups, want 2: the first flight's and the one after it, which the two refusers share", got)
	}
}
