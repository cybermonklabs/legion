package api

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
)

// send is the next part of a body that is still arriving, as a client on a slow link sends it.
func (b *heldBackBody) send(part []byte) {
	b.t.Helper()
	if _, err := b.connection.Write(part); err != nil {
		b.t.Fatalf("send %d more bytes of the body: %v", len(part), err)
	}
}

// dispatch://INFRA-179, review 6's finding 5. The server bounds what a caller's request spends
// WAITING at the pod's bounds (docs.WriteWaitBound, measured from the request's start), and a
// slot that is free is no wait at all. With the bound wrapped around every acquire, a request's
// second transaction was refused SERVICE_BUSY once the request had been alive fifteen seconds,
// though all 48 slots stood free and nothing would have waited for anything.
//
// The fifteen seconds are spent for real: the mark the bound is measured from is docs-internal
// (callerRequestKey), so no hook backdates it from here.
func TestASecondTransactionTakesAFreeSlotPastTheRequestsWaitBound(t *testing.T) {
	_, _, _, served := newRegisteredTestServer(t, testServerOptions{})
	// A live caller's request has a Done channel; a context with none reads as the server's own
	// work, which the bound leaves alone (docs.BoundWriteWait).
	request, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx := docs.WithCallerRequest(request)
	first, err := served.begin(ctx)
	if err != nil {
		t.Fatalf("the request's first transaction: %v", err)
	}
	if err := first.Rollback(ctx); err != nil {
		t.Fatalf("roll the first transaction back: %v", err)
	}
	spent := docs.WriteWaitBound + 500*time.Millisecond
	time.Sleep(spent)
	free := served.writes.semaphore().TryAcquire(apiWriteLimit)
	if free {
		served.writes.semaphore().Release(apiWriteLimit)
	}
	second, err := served.begin(ctx)
	t.Logf("%s into the request, with all %d slots free=%t: the second transaction answered %v", spent, apiWriteLimit, free, err)
	if !free {
		t.Fatalf("the test wanted every one of the %d slots free before the second transaction", apiWriteLimit)
	}
	if err != nil {
		t.Fatalf("a second transaction %s into the request was refused with every slot free: %v", spent, err)
	}
	if err := second.Rollback(ctx); err != nil {
		t.Fatalf("roll the second transaction back: %v", err)
	}
}

// dispatch://INFRA-179, review 6's finding 6. The deadline a body is read under is an idle one:
// a body that is slow but always arriving is read to its end, however long the whole of it takes,
// while a body that stops arriving is cut. One absolute deadline at the first read cut this
// write - 41 bytes, three at a time every 150 ms, 2.0 s in all against a 1 s drain bound - and
// answered it 400 INVALID_JSON, blaming the caller's JSON for a read the server ended.
func TestABodyThatArrivesSlowlyButSteadilyIsReadToItsEnd(t *testing.T) {
	const drainLimit = time.Second
	const part, pause = 3, 150 * time.Millisecond
	handler, _, _, served := newRegisteredTestServer(t, testServerOptions{})
	served.drainReadLimit = drainLimit
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	issue := createArtifactIssue(t, handler)
	body := []byte(`{"body":"` + strings.Repeat("a", 30) + `"}`)
	header := http.Header{}
	header.Set("Content-Type", "application/json")
	header.Set("X-Dispatch-User", "alice")
	started := time.Now()
	held := sendPartOfBody(t, server.Listener.Addr().String(), "/api/v1/issues/"+issue.Key+"/messages", header, len(body), body[:part])
	for sent := part; sent < len(body); sent += part {
		time.Sleep(pause)
		held.send(body[sent:min(sent+part, len(body))])
	}
	response, answer := held.answer()
	elapsed := time.Since(started)
	t.Logf("a %d-byte body sent %d bytes every %s, under a drain bound of %s, was answered %d %s after %s",
		len(body), part, pause, drainLimit, response.StatusCode, answer, elapsed.Round(time.Millisecond))
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("a body that was always arriving was answered %d %s, want 201", response.StatusCode, answer)
	}
	if elapsed < drainLimit {
		t.Fatalf("the body arrived whole in %s, inside the %s bound, so this run says nothing about a body that outlasts it", elapsed, drainLimit)
	}
}

// dispatch://INFRA-179, review 6's finding 7. A request holds a slot of the write limit while it
// has a transaction open and at no other time, so a body still arriving holds nothing: 48
// signed-in callers whose bodies stop after the headers - the whole of the limit - leave every
// slot free, and a further signed-in write lands while they stall. Taken at the request's entry
// instead, those 48 held all 48 slots and every other write was refused 503 SERVICE_BUSY until
// their bodies' deadlines ran out.
//
// Each stalled request is proved to be inside the body read, and not merely dialled, by the 100
// Continue net/http writes at the body's first read (Expect: 100-continue), and each is answered
// at its own deadline by the refusal that names it (deadlinedBody).
func TestSignedInCallersWhoseBodiesStallHoldNoWriteSlot(t *testing.T) {
	const drainLimit = 5 * time.Second
	handler, _, _, served := newRegisteredTestServer(t, testServerOptions{})
	served.drainReadLimit = drainLimit
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	issue := createArtifactIssue(t, handler)
	target := "/api/v1/issues/" + issue.Key + "/messages"
	header := http.Header{}
	header.Set("Content-Type", "application/json")
	header.Set("X-Dispatch-User", "alice")
	header.Set("Expect", "100-continue")
	stalled := make([]*heldBackBody, apiWriteLimit)
	var stalling time.Time
	for index := range stalled {
		stalled[index] = sendPartOfBody(t, server.Listener.Addr().String(), target, header, 100_000, nil)
		interim, _ := stalled[index].answer()
		if interim.StatusCode != http.StatusContinue {
			t.Fatalf("the %dth stalled write was answered %d before it sent a byte; want the 100 Continue its handler's first read writes", index, interim.StatusCode)
		}
		if index == 0 {
			stalling = time.Now()
		}
	}
	landing := time.Now()
	request, err := http.NewRequest(http.MethodPost, server.URL+target, strings.NewReader(`{"body":"the write that must still land"}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Dispatch-User", "alice")
	client := &http.Client{Timeout: 30 * time.Second}
	landed, err := client.Do(request)
	if err != nil {
		t.Fatalf("the write made while %d bodies stalled: %v", len(stalled), err)
	}
	answer, err := io.ReadAll(landed.Body)
	if err != nil {
		t.Fatalf("read the answer's body: %v", err)
	}
	landed.Body.Close()
	t.Logf("with %d signed-in callers stalled inside their body reads, a further signed-in write was answered %d %s after %s",
		len(stalled), landed.StatusCode, answer, time.Since(landing).Round(time.Millisecond))
	if landed.StatusCode != http.StatusCreated {
		t.Fatalf("a signed-in write made while %d stalled bodies were in flight was answered %d %s, want 201", len(stalled), landed.StatusCode, answer)
	}
	if waited := time.Since(stalling); waited >= drainLimit {
		t.Fatalf("the write landed %s after the first body stalled, past the %s those bodies are cut at, so this run says nothing about what a stalled body holds", waited, drainLimit)
	}
	for index, body := range stalled {
		refused, refusal := body.answer()
		if refused.StatusCode != http.StatusServiceUnavailable || refusalCode(refusal) != "BODY_STALLED" {
			t.Fatalf("the %dth stalled body was answered %d %s, want 503 BODY_STALLED at its deadline", index, refused.StatusCode, refusal)
		}
		if !body.closedByServer(drainLimit) {
			t.Fatalf("the connection of the %dth stalled body was still open %s after its answer", index, drainLimit)
		}
	}
}

// stoppedBody is a connection whose read deadline has passed: every read of it fails as net's
// does.
type stoppedBody struct{}

func (stoppedBody) Read([]byte) (int, error) {
	return 0, &net.OpError{Op: "read", Err: os.ErrDeadlineExceeded}
}

func (stoppedBody) Close() error { return nil }

// A body read is bounded by two deadlines - the idle one, drainTime after the last bytes to
// arrive, and the ceiling, docs.WriteRequestBound after the first read - and the refusal names
// the one that ended it, so a caller is told whether to resend the body or to send it faster.
func TestABodyCutByADeadlineNamesWhichOfTheTwoEndedIt(t *testing.T) {
	for _, cut := range []struct {
		name        string
		idle, whole time.Duration
		code        string
	}{
		{name: "a body that stopped arriving meets the idle deadline", idle: time.Second, whole: docs.WriteRequestBound, code: "BODY_STALLED"},
		{name: "a trickle meets the ceiling", idle: bodyDrainLimit, whole: docs.WriteRequestBound, code: "BODY_TOO_SLOW"},
	} {
		t.Run(cut.name, func(t *testing.T) {
			body := &deadlinedBody{ReadCloser: stoppedBody{}, writer: httptest.NewRecorder(), idle: cut.idle, whole: cut.whole}
			if _, err := body.Read(make([]byte, 1)); !errors.Is(err, os.ErrDeadlineExceeded) {
				t.Fatalf("the read answered %v, want net's own deadline error", err)
			}
			refusal := body.cutBy()
			if refusal == nil {
				t.Fatal("a read the deadline ended left no refusal")
			}
			if refusal.status != http.StatusServiceUnavailable || refusal.code != cut.code {
				t.Fatalf("the refusal is %d %s (%q), want 503 %s", refusal.status, refusal.code, refusal.message, cut.code)
			}
		})
	}
}

// dispatch://INFRA-179, review 5820480859's finding 11. The slot a write holds is the one its
// transaction takes: server.begin takes one for every transaction it opens, and limitedTx gives
// it back as that transaction ends, so a request that opens two in turn holds one at a time and
// never none while one of them is open. Before, the release lived in the request's context and
// the first transaction's commit spent it, so every later transaction of that request ran
// outside the limit - adviceReadTx on an issue create, and the detached deliveries a broadcast
// and a comment make, which keep the request's context - and neither the cap of 48 API
// transactions nor the 16 connections kept free for the room workers' appends held.
//
// The limit's free slots are counted around each transaction. Ending one gives its slot back
// exactly once: the rollback every handler defers after a commit that landed releases nothing
// further, where a second release would take the limit past its own size (limitedTx,
// writeLimiter.take).
func TestEachTransactionOfARequestHoldsOneSlotAndGivesItBackOnce(t *testing.T) {
	_, _, _, served := newRegisteredTestServer(t, testServerOptions{})
	ctx := t.Context()
	if free := freeWriteSlots(t, &served.writes); free != apiWriteLimit {
		t.Fatalf("the limit has %d of its %d slots free before the request has a transaction, want all of them", free, apiWriteLimit)
	}
	for _, transaction := range []struct {
		name string
		// end is how the request ends this transaction: a commit, or the rollback a handler
		// defers after a commit that landed, which gives nothing back a second time.
		end func(tx pgx.Tx) error
	}{
		{
			name: "the request's first transaction, committed",
			end:  func(tx pgx.Tx) error { return tx.Commit(ctx) },
		},
		{
			name: "its second, committed and then rolled back",
			end: func(tx pgx.Tx) error {
				if err := tx.Commit(ctx); err != nil {
					return err
				}
				// pgx answers a rollback of a committed transaction ErrTxClosed, which every
				// handler's deferred rollback meets and ignores; what matters here is that the
				// slot is given back once.
				if err := tx.Rollback(ctx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
					return err
				}
				return nil
			},
		},
	} {
		tx, err := served.begin(ctx)
		if err != nil {
			t.Fatalf("%s: %v", transaction.name, err)
		}
		if free := freeWriteSlots(t, &served.writes); free != apiWriteLimit-1 {
			t.Fatalf("%s holds %d of the %d slots while it is open, want exactly one",
				transaction.name, apiWriteLimit-free, apiWriteLimit)
		}
		if err := transaction.end(tx); err != nil {
			t.Fatalf("ending %s: %v", transaction.name, err)
		}
		if free := freeWriteSlots(t, &served.writes); free != apiWriteLimit {
			t.Fatalf("once %s has ended the limit has %d of its %d slots free, want all of them",
				transaction.name, free, apiWriteLimit)
		}
	}
}

// freeWriteSlots is how many slots of the API's write limit stand free, counted by taking them
// one at a time and giving every one back, so the reading holds nothing of its own and disturbs
// no caller's slot.
func freeWriteSlots(t *testing.T, limiter *writeLimiter) int {
	t.Helper()
	free := limiter.semaphore()
	taken := 0
	for free.TryAcquire(1) {
		taken++
		if taken > apiWriteLimit {
			free.Release(int64(taken))
			t.Fatalf("the limit gave out %d slots, past the %d it holds", taken, apiWriteLimit)
		}
	}
	free.Release(int64(taken))
	return taken
}
