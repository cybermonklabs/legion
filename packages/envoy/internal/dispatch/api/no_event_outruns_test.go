package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/testwait"
)

// commentCommitHold is the comment write's test seam (server.beforeCommentCommit). It hands the
// test the events a write appended while that write's transaction is still open, and holds the
// write there until the test sends how it ends: nil commits it, an error rolls it back. done
// ends a hold the test never released, so a test that failed while a write was held leaves no
// transaction open under the cleanups after it.
type commentCommitHold struct {
	held    chan []model.Event
	release chan error
	done    chan struct{}
}

func newCommentCommitHold(t *testing.T) *commentCommitHold {
	hold := &commentCommitHold{held: make(chan []model.Event), release: make(chan error), done: make(chan struct{})}
	t.Cleanup(func() { close(hold.done) })
	return hold
}

func (h *commentCommitHold) beforeCommit(ctx context.Context, events []model.Event) error {
	select {
	case h.held <- events:
	case <-h.done:
		return errorf(http.StatusServiceUnavailable, "TEST_ENDED", "the test ended while this write was held")
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case err := <-h.release:
		return err
	case <-h.done:
		return errorf(http.StatusServiceUnavailable, "TEST_ENDED", "the test ended while this write was held")
	case <-ctx.Done():
		return ctx.Err()
	}
}

// heldCommentCreated is the comment.created event among the events a held write appended, with
// the id of the comment it announces, read from its payload as a subscriber reads it.
func heldCommentCreated(t *testing.T, held []model.Event) (model.Event, string) {
	t.Helper()
	for _, event := range held {
		if event.Type != "comment.created" {
			continue
		}
		encoded, err := json.Marshal(event.Payload)
		if err != nil {
			t.Fatalf("encode the payload of event %d: %v", event.ID, err)
		}
		var payload struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(encoded, &payload); err != nil || payload.ID == "" {
			t.Fatalf("read the comment out of event %d: %v: %s", event.ID, err, encoded)
		}
		return event, payload.ID
	}
	t.Fatalf("the held write appended %+v, and no comment.created among them", held)
	return model.Event{}, ""
}

// heldEventOnTheStream waits for an event of held to arrive on published, and answers the first
// that does. Events the server's other work publishes meanwhile are passed over: what is held
// against the stream is this write's own events, not an empty stream. The wait has no bound of
// its own: it answers false when the stream closes or the test's give-up comes first
// (testwait.Context).
func heldEventOnTheStream(t *testing.T, published <-chan model.Event, held []model.Event) (model.Event, bool) {
	t.Helper()
	giveUp := testwait.Context(t).Done()
	for {
		select {
		case event, open := <-published:
			if !open {
				return model.Event{}, false
			}
			if isHeldEvent(held, event) {
				return event, true
			}
		case <-giveUp:
			return model.Event{}, false
		}
	}
}

// heldEventPublished answers the first event of held among those published already holds, and
// waits for none. Events the server's other work published are passed over, as
// heldEventOnTheStream passes them. It is the proof of an absence read from state: the server
// hands each event to the broker's subscribers on the write's own goroutine (server.publish,
// events.Stream.Publish's send into each subscriber's buffer), so once the test has heard from
// that goroutine after a point of its run, every event it published before that point is in the
// buffer already, and no span is needed for it to arrive.
func heldEventPublished(published <-chan model.Event, held []model.Event) (model.Event, bool) {
	for {
		select {
		case event, open := <-published:
			if !open {
				return model.Event{}, false
			}
			if isHeldEvent(held, event) {
				return event, true
			}
		default:
			return model.Event{}, false
		}
	}
}

// isHeldEvent reports whether event is one of held, by its id.
func isHeldEvent(held []model.Event, event model.Event) bool {
	return slices.ContainsFunc(held, func(appended model.Event) bool { return appended.ID == event.ID })
}

// commentNodeReadable reports whether the API answers the comment's node, read as any reader
// reads one (GET /api/v1/references?from=), on a request of its own and so on a connection of
// the pool that the held write's transaction is not on.
func commentNodeReadable(t *testing.T, handler http.Handler, ref string) bool {
	t.Helper()
	response := graphRequest(t, handler, url.Values{"from": {ref}})
	switch response.Code {
	case http.StatusOK:
		return true
	case http.StatusNotFound:
		return false
	}
	t.Fatalf("read the node %s through GET /api/v1/references: status=%d body=%s", ref, response.Code, response.Body.String())
	return false
}

// No event outruns its node: an event the server's write path appends reaches no subscriber of
// its broker before the transaction that appended it commits, it arrives with its node readable
// through the API the moment that transaction has committed, and a write that rolls back
// publishes nothing at all.
//
// The write is the server's own comment write, driven over its route, and the stream is the
// broker that write publishes into (Deps.Events), the fan-out every in-process consumer of a
// committed event reads. The write is held through the route's own seam
// (server.beforeCommentCommit), once its comment, its node and its comment.created event are
// written and before its commit. While it is held the run reads the stream and finds nothing of
// this write's events there, and reads the comment's node through the API on a request of its
// own and finds it absent. Released, the write commits, and the comment.created event arrives
// on the stream with its node readable at the moment it arrives. The run does the whole sequence
// again with the hold ending the write in a rollback, and no event of it ever arrives. So a
// publish before the commit, and a publish for a write that rolled back, each fail here, and the
// failure prints the event and the node address it could not read.
//
// Each absence is read from the stream's buffer, with no span (heldEventPublished): the write
// publishes on its own goroutine, which reaches the hold, and later writes its answer, after any
// publish of its own before either. Every wait is the shared wait's, whose one bound is the
// test's give-up.
func TestNoEventOutrunsItsNode(t *testing.T) {
	t.Parallel()
	handler, _, deps, served := newRegisteredTestServer(t, testServerOptions{})
	createReferenceAPIProject(t, handler, "CORE")
	issue := createReferenceAPIIssue(t, handler, "CORE")
	hold := newCommentCommitHold(t)
	served.beforeCommentCommit = hold.beforeCommit
	published, unsubscribe := deps.Events.Subscribe()
	defer unsubscribe()

	for _, ending := range []string{"commit", "rollback"} {
		answered := make(chan *httptest.ResponseRecorder, 1)
		go func() {
			answered <- dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments",
				map[string]string{"body": "The write c-read-cost holds open, ending in a " + ending + "."}, "alice")
		}()
		var held []model.Event
		var early *httptest.ResponseRecorder
		testwait.Until(t, ending+": the comment write to reach its hold before the commit, or to answer", func() (string, bool) {
			select {
			case held = <-hold.held:
				return "held", true
			case early = <-answered:
				return "answered", true
			default:
				return "neither held nor answered", false
			}
		})
		if early != nil {
			t.Fatalf("%s: the comment write answered %d %s without reaching its hold before the commit",
				ending, early.Code, early.Body.String())
		}
		created, comment := heldCommentCreated(t, held)
		node := "dispatch://" + issue.Key + "/comment/" + comment
		t.Logf("%s: the write is held open with comment.created %d appended for the comment %s", ending, created.ID, node)

		// The transaction is open. Nothing of it is on the stream, and its node is not readable.
		// The write reached its hold on its own goroutine, after anything it published before its
		// commit, so such an event is in the stream's buffer by now (heldEventPublished).
		if event, arrived := heldEventPublished(published, held); arrived {
			readable := commentNodeReadable(t, handler, node)
			t.Fatalf("%s: event %d (%s) arrived on the event stream while the write that appended it was still open, and its node %s read as readable=%t: an event outran its commit",
				ending, event.ID, event.Type, node, readable)
		}
		if commentNodeReadable(t, handler, node) {
			t.Fatalf("%s: the node %s of a write not yet committed is readable through the API", ending, node)
		}
		t.Logf("%s: while the write is held no event of it is on the stream and its node %s is not readable", ending, node)

		if ending == "commit" {
			hold.release <- nil
			event, arrived := heldEventOnTheStream(t, published, []model.Event{created})
			if !arrived {
				t.Fatalf("commit: the committed comment.created %d had not arrived on the event stream by the test's give-up", created.ID)
			}
			// Read at once, before anything else this goroutine does: the event is the reader's
			// cue to read the node it announces.
			if !commentNodeReadable(t, handler, node) {
				t.Fatalf("commit: event %d (%s) arrived on the event stream with its node %s unreadable through the API: an event outran its node",
					event.ID, event.Type, node)
			}
			if response := awaitResponse(t, answered); response.Code != http.StatusCreated {
				t.Fatalf("commit: the released comment write answered %d %s, want 201", response.Code, response.Body.String())
			}
			t.Logf("commit: event %d (%s) arrived the moment the write committed, and its node %s was readable at once", event.ID, event.Type, node)
			continue
		}

		hold.release <- errorf(http.StatusServiceUnavailable, "TEST_ROLLBACK", "the test ends this write in a rollback")
		if response := awaitResponse(t, answered); response.Code != http.StatusServiceUnavailable {
			t.Fatalf("rollback: the comment write the hold rolled back answered %d %s, want the hold's 503", response.Code, response.Body.String())
		}
		// The write wrote its answer on its own goroutine, after anything it published there
		// (server.publish), so an event it published for the rolled-back transaction is in the
		// stream's buffer by now; and the node read below shows the transaction committed nothing,
		// so there is no committed event of it for anything else to publish.
		if event, arrived := heldEventPublished(published, held); arrived {
			t.Fatalf("rollback: event %d (%s) of a write that rolled back arrived on the event stream, announcing the node %s, which readable=%t",
				event.ID, event.Type, node, commentNodeReadable(t, handler, node))
		}
		if commentNodeReadable(t, handler, node) {
			t.Fatalf("rollback: the node %s of a write that rolled back is readable through the API", node)
		}
		t.Logf("rollback: the same write ending in a rollback published none of its events (comment.created would have been %d) and left no node at %s",
			created.ID, node)
	}
}
