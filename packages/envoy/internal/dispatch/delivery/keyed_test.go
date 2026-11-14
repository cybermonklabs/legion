package delivery

import (
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

// keyedItem is one item a keyedRunner test hands its runner: the key and a number naming it.
type keyedItem struct {
	key string
	n   int
}

// keyedRecorder is a keyedRunner handler that records when each item starts and ends, and holds
// any item whose number has a gate until the gate is closed.
type keyedRecorder struct {
	mu     sync.Mutex
	events []string
	gates  map[int]chan struct{}
	start  chan keyedItem
}

func (r *keyedRecorder) handle(item keyedItem) {
	r.record("start", item)
	r.start <- item
	if gate, ok := r.gates[item.n]; ok {
		<-gate
	}
	r.record("end", item)
}

func (r *keyedRecorder) record(what string, item keyedItem) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, fmt.Sprintf("%s %s%d", what, item.key, item.n))
}

// recorded is the events of items whose key is key, in the order they happened.
func (r *keyedRecorder) recorded(key string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var events []string
	for _, event := range r.events {
		if strings.Contains(event, " "+key) {
			events = append(events, event)
		}
	}
	return events
}

func awaitStart(t *testing.T, starts <-chan keyedItem) keyedItem {
	t.Helper()
	select {
	case item := <-starts:
		return item
	case <-time.After(eventWait):
		t.Fatalf("no item started within %s", eventWait)
		return keyedItem{}
	}
}

// startedAlready is the item that has started by now, inside a synctest bubble after
// synctest.Wait: the runner has nothing left to do then but wait, so an item it was given and
// could start has started, and one it holds back has not.
func startedAlready(t *testing.T, starts <-chan keyedItem, what string) keyedItem {
	t.Helper()
	select {
	case item := <-starts:
		return item
	default:
		t.Fatalf("%s had not started once the runner had nothing left to do but wait", what)
		return keyedItem{}
	}
}

// Items of one key run in the order they were given, each once the one before it has ended, while
// an item of another key runs as soon as it is given; once every key's queue is empty no goroutine
// is left behind.
//
// It runs on a fake clock (testing/synctest): the runner has no timer and does no I/O, so once
// synctest.Wait returns every item it can start has started, and "as soon as it is given" is read
// as started by then rather than within a span of wall time.
func TestAKeyedRunnerRunsOneKeyInOrderAndKeysAtOnce(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		recorder := &keyedRecorder{gates: map[int]chan struct{}{1: make(chan struct{})}, start: make(chan keyedItem, 8)}
		runner := newKeyedRunner(recorder.handle)
		// a1 is held until released, and on every way out a1 is released and every handler waited
		// for on the bubble's clock, so none outlives the bubble: a runner that started an item
		// late then ends the bubble cleanly rather than as a deadlock that stops the binary.
		release := sync.OnceFunc(func() { close(recorder.gates[1]) })
		defer func() {
			release()
			runner.wait()
		}()

		runner.run("a", keyedItem{"a", 1})
		synctest.Wait()
		if item := startedAlready(t, recorder.start, "a1"); item.n != 1 {
			t.Fatalf("first start = %+v, want a1", item)
		}
		runner.run("a", keyedItem{"a", 2})
		runner.run("b", keyedItem{"b", 3})
		synctest.Wait()
		if item := startedAlready(t, recorder.start, "b3, given while a1 is held"); item.n != 3 {
			t.Fatalf("start while a1 is held = %+v, want b3 (a2 waits behind a1)", item)
		}
		select {
		case item := <-recorder.start:
			t.Fatalf("%+v started while a1 was still in its handler", item)
		default:
		}

		release()
		synctest.Wait()
		if item := startedAlready(t, recorder.start, "a2, once a1 ended"); item.n != 2 {
			t.Fatalf("start after a1 ended = %+v, want a2", item)
		}
		runner.wait()
		if events, want := recorder.recorded("a"), []string{"start a1", "end a1", "start a2", "end a2"}; !slices.Equal(events, want) {
			t.Fatalf("key a's events = %q, want %q", events, want)
		}
		runner.mu.Lock()
		defer runner.mu.Unlock()
		if len(runner.queued) != 0 {
			t.Fatalf("keys still active after every item ended: %v", runner.queued)
		}
	})
}

// stop hands back every item still waiting behind its key, in each key's order, and refuses every
// later item; the item in its handler runs on, and wait returns once it has ended.
func TestAStoppedKeyedRunnerHandsBackWhatWaits(t *testing.T) {
	t.Parallel()
	recorder := &keyedRecorder{gates: map[int]chan struct{}{1: make(chan struct{})}, start: make(chan keyedItem, 8)}
	runner := newKeyedRunner(recorder.handle)
	runner.run("a", keyedItem{"a", 1})
	awaitStart(t, recorder.start)
	runner.run("a", keyedItem{"a", 2})
	runner.run("a", keyedItem{"a", 3})

	waiting := runner.stop()
	if want := []keyedItem{{"a", 2}, {"a", 3}}; !slices.Equal(waiting, want) {
		t.Fatalf("stop handed back %+v, want %+v", waiting, want)
	}
	if runner.run("b", keyedItem{"b", 4}) {
		t.Fatal("a stopped runner took an item")
	}
	close(recorder.gates[1])
	runner.wait()
	if events, want := recorder.recorded("a"), []string{"start a1", "end a1"}; !slices.Equal(events, want) {
		t.Fatalf("events = %q, want %q: only the item in its handler runs after stop", events, want)
	}
}
