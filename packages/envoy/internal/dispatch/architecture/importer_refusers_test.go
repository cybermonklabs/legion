package architecture

import (
	"context"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/testwait"
)

// beforeTheDeadline is how long this file's waits may take: what is left of the test binary's
// deadline, less a second (testwait.Bounded of a day). So how long a loaded host takes to end a
// flight decides no verdict here, and a wait whose event never comes still fails, naming it.
func beforeTheDeadline(t *testing.T) time.Duration {
	return testwait.Bounded(t, 24*time.Hour, time.Second)
}

// receiveBeforeTheDeadline is the next value of ch, or of its close, within beforeTheDeadline.
func receiveBeforeTheDeadline[T any](t *testing.T, what string, ch <-chan T) T {
	t.Helper()
	var got T
	testwait.WithinPolling(t, beforeTheDeadline(t), testwait.Poll, what, func() bool {
		select {
		case got = <-ch:
			return true
		default:
			return false
		}
	})
	return got
}

// refusersRig is one importer on archRepo's moving branch, which counts GitHub's head lookups
// and, through the importer's two test hooks, the flights its callers take and the flights they
// refuse. The caller of the refusal numbered heldRefusal waits there until releaseLate.
type refusersRig struct {
	branch   *movingBranch
	database *store.Store
	process  *Importer
	lookups  atomic.Int32
	taken    atomic.Int32
	refusals atomic.Int32
	// heldRefusal is the refusal, counted from 1, whose caller waits on late; 0 holds none.
	heldRefusal int32
	late        chan struct{}
	releaseLate func()
}

// newRefusersRig seeds CORE's source from archRepo, at commitOne with commitTwo to push, and
// starts an importer on it whose caller of the refusal numbered held waits until releaseLate.
func newRefusersRig(t *testing.T, held int32) *refusersRig {
	t.Helper()
	rig := &refusersRig{
		branch:      newMovingBranch(t, commitOne, map[string]map[string]string{commitOne: validFiles(), commitTwo: infraFiles()}),
		heldRefusal: held,
		late:        make(chan struct{}),
	}
	inner := rig.branch.handler()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/{owner}/{repo}/commits/{branch}", func(w http.ResponseWriter, r *http.Request) {
		rig.lookups.Add(1)
		inner.ServeHTTP(w, r)
	})
	mux.Handle("/", inner)
	client := newGitHubClient(t, mux)
	rig.database = storetest.Open(t)
	if _, err := rig.database.Pool.Exec(context.Background(), `
		insert into projects (key, name, default_assignee) values ('CORE', 'Core', 'alice');
		insert into architecture_sources (project_key, repo, branch, installation_id, created_by)
		values ('CORE', 'legion/arch', 'main', 11, '{"kind":"user","id":"alice"}');
	`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	rig.process = NewImporter(rig.database, client, events.NewBroker())
	t.Cleanup(rig.process.Close)
	rig.releaseLate = sync.OnceFunc(func() { close(rig.late) })
	// Cleanups run last registered first: a test that fails while a refuser is held lets it go
	// before Close waits for the flights.
	t.Cleanup(rig.releaseLate)
	rig.process.afterFlightTaken = func() { rig.taken.Add(1) }
	rig.process.afterSyncRefusal = func() {
		if rig.refusals.Add(1) == rig.heldRefusal {
			<-rig.late
		}
	}
	return rig
}

// syncOf starts the importer's sync of CORE's source from repo and answers it on the channel.
func (rig *refusersRig) syncOf(repo string, answers chan<- syncAnswer) {
	go func() {
		source, err := rig.process.Sync(context.Background(), "CORE", repo)
		answers <- syncAnswer{source: source, err: err}
	}()
}

// holdTheFirstFlight starts a sync of archRepo whose flight the branch holds in its blob reads,
// after its head lookup, and returns that caller's answer once the read reaches the hold.
func (rig *refusersRig) holdTheFirstFlight(t *testing.T) <-chan syncAnswer {
	t.Helper()
	rig.branch.holdBlobs(commitOne)
	held, _ := rig.branch.hold.channels(t)
	first := make(chan syncAnswer, 1)
	rig.syncOf(archRepo, first)
	receiveBeforeTheDeadline(t, "the first flight's blob read to reach the hold", held)
	return first
}

// joinTheFirstFlight starts n more syncs of archRepo and waits until each holds the first
// flight, which the branch still holds, so each refuses it once it ends.
func (rig *refusersRig) joinTheFirstFlight(t *testing.T, n int32) chan syncAnswer {
	t.Helper()
	refusers := make(chan syncAnswer, n)
	for range n {
		rig.syncOf(archRepo, refusers)
	}
	testwait.WithinPolling(t, beforeTheDeadline(t), testwait.Poll, "the later callers to join the held flight",
		func() bool { return rig.taken.Load() >= n+1 })
	return refusers
}

// commitOf is the commit the answer's row records, or <nil>.
func commitOf(answer syncAnswer) string {
	if answer.source.LastCommit == nil {
		return "<nil>"
	}
	return *answer.source.LastCommit
}

// Two callers that both refused one flight share the next one, however late each looks again.
// Both join a flight held in its blob reads after its head lookup, at commitOne, and the branch
// moves to commitTwo before the flight is released, so each refuses it once it ends and the
// flight after it reads commitTwo: commitOne is the first flight's row, commitTwo the second's.
// The second to refuse is held there until the first has started the next flight and been
// answered by it; released, it takes that flight's answer rather than starting a third, so the
// three callers make two head lookups: the first flight's, and the next one's, which the two
// refusers share.
func TestCallersThatRefusedOneFlightShareTheNextOneHoweverLateTheyLook(t *testing.T) {
	rig := newRefusersRig(t, 2)
	first := rig.holdTheFirstFlight(t)
	refusers := rig.joinTheFirstFlight(t, 2)
	rig.branch.moveHead(commitTwo)
	rig.branch.releaseHeld()

	if answer := receiveBeforeTheDeadline(t, "the first caller's answer", first); answer.err != nil || commitOf(answer) != commitOne {
		t.Fatalf("the first caller answered commit %s (err %v); want %s, the first flight's", commitOf(answer), answer.err, commitOne)
	}
	// The held refuser cannot answer before releaseLate, so the first answer is the other's.
	if answer := receiveBeforeTheDeadline(t, "the answer of the refuser the hold let through", refusers); answer.err != nil || commitOf(answer) != commitTwo {
		t.Fatalf("the refuser the hold let through answered commit %s (err %v); want %s, the next flight's", commitOf(answer), answer.err, commitTwo)
	}
	rig.releaseLate()
	answer := receiveBeforeTheDeadline(t, "the answer of the refuser held until the other was answered", refusers)
	if answer.err != nil || commitOf(answer) != commitTwo {
		t.Fatalf("the late refuser answered commit %s (err %v); want %s, the next flight's", commitOf(answer), answer.err, commitTwo)
	}

	t.Logf("refusals=%d head lookups=%d", rig.refusals.Load(), rig.lookups.Load())
	if got := rig.refusals.Load(); got != 2 {
		t.Fatalf("the two later callers refused %d flights between them, want 2: without both refusals the head lookups say nothing about refusers", got)
	}
	if got := rig.lookups.Load(); got != 2 {
		t.Fatalf("three callers, two of which refused the first flight, made %d head lookups, want 2: the first flight's and the one after it, which the two refusers share", got)
	}
}

// A flight that fails is shared as one that succeeds is: when the flight after a refused one
// fails at its head lookup (GitHub answers 502), the refuser that looks again after it has
// ended takes that failure, as a caller that joined the failed flight would, rather than
// starting a flight of its own. So the three callers make two head lookups, and both refusers
// answer the one failure.
func TestARefuserThatLooksAgainLateSharesTheNextFlightsFailure(t *testing.T) {
	rig := newRefusersRig(t, 2)
	first := rig.holdTheFirstFlight(t)
	refusers := rig.joinTheFirstFlight(t, 2)
	// The first flight has looked the head up, so the next lookup is the next flight's. Its hold
	// is the one releaseHeld opens below, so it answers at once.
	rig.branch.holdNextHeadLookupAnswering(http.StatusBadGateway)
	rig.branch.releaseHeld()

	if answer := receiveBeforeTheDeadline(t, "the first caller's answer", first); answer.err != nil {
		t.Fatalf("the first caller: %v", answer.err)
	}
	early := receiveBeforeTheDeadline(t, "the answer of the refuser the hold let through", refusers)
	if early.err == nil {
		t.Fatalf("the refuser that started the next flight answered %+v with no error; want that flight's failure", early.source)
	}
	rig.releaseLate()
	late := receiveBeforeTheDeadline(t, "the answer of the refuser held until the other was answered", refusers)

	t.Logf("refusals=%d head lookups=%d early=%q late=%v", rig.refusals.Load(), rig.lookups.Load(), early.err, late.err)
	if late.err == nil || late.err.Error() != early.err.Error() {
		t.Fatalf("the late refuser answered err %v; want the next flight's failure, %q", late.err, early.err)
	}
	if got := rig.lookups.Load(); got != 2 {
		t.Fatalf("three callers, two of which refused the first flight, made %d head lookups, want 2: the first flight's and the failed one after it, which the two refusers share", got)
	}
}

// A refuser of one source's flight is answered that source's row, never another source's: a
// second source of the project, acme/infra, syncs to its end while the refuser of archRepo's
// first flight is held, and the refuser, released, answers archRepo's row.
func TestARefuserOfOneSourceIsNeverAnsweredAnotherSourcesFlight(t *testing.T) {
	rig := newRefusersRig(t, 1)
	if _, err := rig.database.Pool.Exec(context.Background(), `
		insert into architecture_sources (project_key, repo, branch, installation_id, created_by)
		values ('CORE', 'acme/infra', 'main', 11, '{"kind":"user","id":"alice"}');
	`); err != nil {
		t.Fatalf("seed acme/infra: %v", err)
	}
	first := rig.holdTheFirstFlight(t)
	refuser := rig.joinTheFirstFlight(t, 1)
	rig.branch.releaseHeld()
	if answer := receiveBeforeTheDeadline(t, "the first caller's answer", first); answer.err != nil {
		t.Fatalf("the first caller: %v", answer.err)
	}
	testwait.WithinPolling(t, beforeTheDeadline(t), testwait.Poll, "the refusal of the first flight",
		func() bool { return rig.refusals.Load() >= 1 })

	other := make(chan syncAnswer, 1)
	rig.syncOf("acme/infra", other)
	otherAnswer := receiveBeforeTheDeadline(t, "the answer of the sync of acme/infra", other)
	rig.releaseLate()
	answer := receiveBeforeTheDeadline(t, "the answer of the refuser held while acme/infra synced", refuser)

	t.Logf("other=%s refuser=%s refuser err=%v", otherAnswer.source.Repo, answer.source.Repo, answer.err)
	if otherAnswer.source.Repo != "acme/infra" {
		t.Fatalf("the sync of acme/infra answered %q's row (err %v); want its own", otherAnswer.source.Repo, otherAnswer.err)
	}
	if answer.err != nil || answer.source.Repo != archRepo {
		t.Fatalf("the refuser of %s answered %q's row (err %v); want %s's", archRepo, answer.source.Repo, answer.err, archRepo)
	}
}
