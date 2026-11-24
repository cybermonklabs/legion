package consumernames

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

func TestACreateReservesItsNameAndStartThenPublishesIt(t *testing.T) {
	h := newHarness()
	var written []Record
	h.store.beforeWrite = func(_ string, record Record) error {
		written = append(written, record.clone())
		return nil
	}
	sel, created, err := h.names("p1").Ensure(context.Background(), "ses-1", CreateRequest{Create: h.server.create("ses-1"), First: first(7)})
	if err != nil || !created {
		t.Fatalf("Ensure = %v, created %t; want a create", err, created)
	}
	if !strings.HasPrefix(sel.Record.Current, "session~") {
		t.Fatalf("current = %q, want a fresh session~<random> name", sel.Record.Current)
	}
	if len(written) != 2 {
		t.Fatalf("%d writes, want the reservation and the publish: %+v", len(written), written)
	}
	if op := written[0].Op; op == nil || op.Kind != OpCreate || op.Candidate != sel.Record.Current || written[0].Start != 7 || written[0].Current != "" {
		t.Fatalf("the first write is %+v, want the reservation of %s at start 7 with no current", written[0], sel.Record.Current)
	}
	if got := h.store.record(t, "ses-1"); got.Op != nil || got.Current != sel.Record.Current || got.Start != 7 {
		t.Fatalf("the record is %+v, want %s published at start 7 with no op", got, sel.Record.Current)
	}
	if start := h.server.startOf(sel.Record.Current); start.Seq != 7 {
		t.Fatalf("the consumer starts at %+v, want 7", start)
	}
}

func TestACallBeforeTheCutoverIsRefusedAndCreatesNothing(t *testing.T) {
	for _, marker := range []string{"", CutoverRolledBack} {
		t.Run("marker "+marker, func(t *testing.T) {
			h := newHarness()
			h.store.state.Cutover = marker
			_, _, err := h.names("p1").Ensure(context.Background(), "ses-1", CreateRequest{Create: h.server.create("ses-1"), First: first(1)})
			if !errors.Is(err, ErrCutoverNotRun) || err.Error() != "consumer names: the cutover has not run" {
				t.Fatalf("Ensure = %v, want %q", err, ErrCutoverNotRun)
			}
			if len(h.server.creates) != 0 || len(h.store.records) != 0 {
				t.Fatalf("created %v and wrote %v before the cutover", h.server.creates, h.store.records)
			}
		})
	}
}

func TestLostSelectionStateRefusesToCreate(t *testing.T) {
	for name, setup := range map[string]func(*memStore){
		"bucket missing":   func(m *memStore) { m.missing = fmt.Errorf("%w: KV_envoy_session_consumers", ErrNoStore) },
		"sentinel missing": func(m *memStore) { m.missing = fmt.Errorf("%w: envoy.bucket", ErrNoSentinel) },
		"key deleted":      func(m *memStore) { m.deleted["ses-1"] = true },
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness()
			setup(h.store)
			_, _, err := h.names("p1").Ensure(context.Background(), "ses-1", CreateRequest{Create: h.server.create("ses-1"), First: first(1)})
			if err == nil || !IsSelectionStateLost(err) {
				t.Fatalf("Ensure = %v, want a refusal for lost selection state", err)
			}
			if len(h.server.creates) != 0 {
				t.Fatalf("created %v with the selection state lost", h.server.creates)
			}
		})
	}
}

func TestAMissingRecordTheFamilyRefusesStops(t *testing.T) {
	h := newHarness()
	names := h.names("p1")
	names.cfg.MissingRecord = func(_ context.Context, logical string) error {
		return fmt.Errorf("a checkpoint of %s has no selection row", logical)
	}
	if _, _, err := names.Ensure(context.Background(), "legion-go-p-github", CreateRequest{Create: h.server.create("legion-go-p-github"), First: first(1)}); err == nil {
		t.Fatal("Ensure created with no selection row where the family refuses one")
	}
	if len(h.server.creates) != 0 {
		t.Fatalf("created %v", h.server.creates)
	}
}

func TestKeysAreRefusedAsTodaysNamesWereBeforeAnythingIsWritten(t *testing.T) {
	h := newHarness()
	names := h.names("p1")
	ok253 := strings.Repeat("a", 253)
	if _, _, err := names.Ensure(context.Background(), ok253, CreateRequest{Create: h.server.create(ok253), First: first(1)}); err != nil {
		t.Fatalf("a 253-byte id: %v, want it accepted", err)
	}
	long := strings.Repeat("a", 254)
	_, _, err := names.Ensure(context.Background(), long, CreateRequest{Create: h.server.create(long), First: first(1)})
	if !errors.Is(err, ErrKeyTooLong) || !strings.Contains(err.Error(), "consumer name is too long, maximum allowed is 255") {
		t.Fatalf("a 254-byte id: %v, want the server's 10102 answer", err)
	}
	for _, key := range []string{"", "a.b", "a*", "a>", "a b", "a\tb", "a\nb", "a\fb", "a\rb", "a/b", `a\b`} {
		if _, _, err := names.Ensure(context.Background(), key, CreateRequest{Create: h.server.create(key), First: first(1)}); !errors.Is(err, ErrInvalidKey) {
			t.Errorf("%q: %v, want ErrInvalidKey", key, err)
		}
	}
	if _, ok := h.store.records[long]; ok || len(h.store.records) != 1 {
		t.Fatalf("records %v; want only the 253-byte id's", h.store.records)
	}
	// Two 253-byte ids that share their first 250 bytes keep records and consumers of their own.
	other := strings.Repeat("a", 250) + "bbb"
	sel, _, err := names.Ensure(context.Background(), other, CreateRequest{Create: h.server.create(other), First: first(1)})
	if err != nil {
		t.Fatalf("the second 253-byte id: %v", err)
	}
	if sel.Record.Current == h.store.record(t, ok253).Current {
		t.Fatalf("two ids share the consumer %s", sel.Record.Current)
	}
}

func TestAnExistingCurrentIsKept(t *testing.T) {
	h := newHarness()
	names := h.names("p1")
	sel, _, err := names.Ensure(context.Background(), "ses-1", CreateRequest{Create: h.server.create("ses-1"), First: first(3)})
	if err != nil {
		t.Fatal(err)
	}
	again, created, err := names.Ensure(context.Background(), "ses-1", CreateRequest{Create: h.server.create("ses-1"), First: first(3)})
	if err != nil || created || again.Record.Current != sel.Record.Current || again.Info == nil {
		t.Fatalf("second Ensure = %+v, created %t, %v; want %s kept, with its info", again.Record, created, err, sel.Record.Current)
	}
	if len(h.server.creates) != 1 {
		t.Fatalf("creates %v, want one", h.server.creates)
	}
}

func TestASavedStartWinsUnlessTheCallerAsksLower(t *testing.T) {
	h := newHarness()
	h.store.put("ses-1", Record{Start: 40})
	names := h.names("p1")
	sel, _, err := names.Ensure(context.Background(), "ses-1", CreateRequest{Create: h.server.create("ses-1"), First: first(90), Lower: 60})
	if err != nil {
		t.Fatal(err)
	}
	if start := h.server.startOf(sel.Record.Current); start.Seq != 40 {
		t.Fatalf("created at %d, want the saved 40 over the caller's 60 and the first-create 90", start.Seq)
	}
	h2 := newHarness()
	h2.store.put("ses-1", Record{Start: 40})
	sel, _, err = h2.names("p1").Ensure(context.Background(), "ses-1", CreateRequest{Create: h2.server.create("ses-1"), First: first(90), Lower: 25})
	if err != nil {
		t.Fatal(err)
	}
	if start := h2.server.startOf(sel.Record.Current); start.Seq != 25 {
		t.Fatalf("created at %d, want the caller's lower 25", start.Seq)
	}
}

func TestAKeyWithNoRecordStartsNoLaterThanTheBoundary(t *testing.T) {
	h := newHarness()
	h.store.state.Boundary = 12
	sel, _, err := h.names("p1").Ensure(context.Background(), "ses-1", CreateRequest{Create: h.server.create("ses-1"), First: first(500)})
	if err != nil {
		t.Fatal(err)
	}
	if start := h.server.startOf(sel.Record.Current); start.Seq != 12 {
		t.Fatalf("created at %d, want the boundary 12 below the first-create 500", start.Seq)
	}
	// Below the boundary the first-create rule stands.
	h2 := newHarness()
	h2.store.state.Boundary = 12
	sel, _, err = h2.names("p1").Ensure(context.Background(), "ses-1", CreateRequest{Create: h2.server.create("ses-1"), First: first(5)})
	if err != nil {
		t.Fatal(err)
	}
	if start := h2.server.startOf(sel.Record.Current); start.Seq != 5 {
		t.Fatalf("created at %d, want the first-create 5", start.Seq)
	}
}

func TestInReplayModeAKeyWithNoRecordStartsAtReplayFrom(t *testing.T) {
	h := newHarness()
	h.store.state.Boundary = 12
	h.store.state.ReplayFrom = 30
	called := false
	sel, _, err := h.names("p1").Ensure(context.Background(), "ses-1", CreateRequest{Create: h.server.create("ses-1"), First: func(context.Context) (Start, error) {
		called = true
		return Start{Seq: 1}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	if start := h.server.startOf(sel.Record.Current); start.Seq != 30 || called {
		t.Fatalf("created at %d (first-create rule called: %t), want replay_from 30 and no first-create rule", start.Seq, called)
	}
}

func TestANativeFirstCreateLostBeforeItsPublishIsCreatedAgainAtItsBoundary(t *testing.T) {
	h := newHarness()
	h.server.lastSeq = 50
	// The create is applied and its answer lost, and the consumer is then lost before any publish.
	h.server.createFault = func(string) (bool, error) { return true, errAnswerLost }
	names := h.names("p1")
	names.cfg.Server = lostInfoServer{h.server}
	_, _, err := names.Ensure(context.Background(), "delivery-jobs", CreateRequest{Create: h.server.create("delivery-jobs"), First: func(context.Context) (Start, error) {
		return Start{Seq: 51, Native: NativeNew}, nil
	}})
	if err == nil {
		t.Fatal("Ensure succeeded with every info of its candidate lost")
	}
	rec := h.store.record(t, "delivery-jobs")
	if rec.Op == nil || rec.Op.Native != NativeNew || rec.Start != 51 {
		t.Fatalf("the record after the lost create is %+v, want the native op open at boundary 51", rec)
	}
	lost := rec.Op.Candidate
	h.server.mu.Lock()
	delete(h.server.consumers, lost)
	h.server.createFault = nil
	h.server.mu.Unlock()
	h.clock.advance(3 * time.Second)

	recovered, _, err := h.names("p2").Ensure(context.Background(), "delivery-jobs", CreateRequest{Create: h.server.create("delivery-jobs"), First: first(999)})
	if err != nil {
		t.Fatalf("recovery: %v", err)
	}
	if recovered.Record.Current == lost {
		t.Fatalf("recovery published the lost %s again", lost)
	}
	if start := h.server.startOf(recovered.Record.Current); start.Native != "" || start.Seq != 51 {
		t.Fatalf("N' starts at %+v, want by start sequence at the boundary 51", start)
	}
	if !contains(h.store.record(t, "delivery-jobs").Retired, lost) {
		t.Fatalf("the lost candidate %s is not retired", lost)
	}
	h.server.createdTwice(t)
}

// lostInfoServer answers no info, as a server whose answers are lost does.
type lostInfoServer struct{ *fakeServer }

func (lostInfoServer) Info(_ context.Context, name string) (*jetstream.ConsumerInfo, error) {
	return nil, fmt.Errorf("the info of %s timed out", name)
}

func TestACreateWhoseAnswerIsLostIsAdoptedAfterAnInfo(t *testing.T) {
	h := newHarness()
	h.server.createFault = func(string) (bool, error) { return true, errAnswerLost }
	sel, created, err := h.names("p1").Ensure(context.Background(), "ses-1", CreateRequest{Create: h.server.create("ses-1"), First: first(4)})
	if err != nil || !created {
		t.Fatalf("Ensure = %v; want the applied create adopted", err)
	}
	if !h.server.holds(sel.Record.Current) || len(h.server.creates) != 1 {
		t.Fatalf("current %s, creates %v; want the one create adopted", sel.Record.Current, h.server.creates)
	}
}

func TestACreateWhoseAnswerIsLostAndThatDidNotLandIsRetired(t *testing.T) {
	h := newHarness()
	fails := 1
	h.server.createFault = func(string) (bool, error) {
		if fails > 0 {
			fails--
			return false, errAnswerLost
		}
		return true, nil
	}
	sel, _, err := h.names("p1").Ensure(context.Background(), "ses-1", CreateRequest{Create: h.server.create("ses-1"), First: first(4)})
	if err != nil {
		t.Fatalf("Ensure = %v; want the second candidate created", err)
	}
	rec := h.store.record(t, "ses-1")
	if len(rec.Retired) != 1 || rec.Retired[0] == sel.Record.Current {
		t.Fatalf("record %+v; want the first candidate retired and the second current", rec)
	}
	h.server.createdTwice(t)
}

func TestAnOpenCreateYoungerThanTheSettleAgeIsWaitedFor(t *testing.T) {
	h := newHarness()
	h.store.put("ses-1", Record{Start: 9, Op: &Op{Kind: OpCreate, Owner: "p1", Candidate: "session~N", ReservedAt: h.clock.Now()}})
	names := h.names("p2")
	// The candidate lands while p2 waits.
	go func() {
		time.Sleep(20 * time.Millisecond)
		_, _ = h.server.create("ses-1")(context.Background(), "session~N", Start{Seq: 9})
		h.clock.advance(time.Second)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sel, err := names.Select(ctx, "ses-1", h.server.create("ses-1"))
	if err != nil {
		t.Fatal(err)
	}
	if sel.Record.Current != "session~N" {
		t.Fatalf("current %q; want the candidate published once it landed", sel.Record.Current)
	}
	h.server.createdTwice(t)
}

func TestEveryReplacementInterruptionPointRecoversInAnotherProcess(t *testing.T) {
	// Each point is where the replacing process stops; another process then calls Select, and
	// the session ends with one current consumer that starts at the saved start, every other
	// name retired, the predecessor deleted, and no name created twice.
	const savedStart = 21
	for _, point := range []string{"after the reservation", "after the create", "after the publish"} {
		t.Run(point, func(t *testing.T) {
			h := newHarness()
			names := h.names("p1")
			sel, _, err := names.Ensure(context.Background(), "ses-1", CreateRequest{Create: h.server.create("ses-1"), First: first(1)})
			if err != nil {
				t.Fatal(err)
			}
			old := sel.Record.Current
			h.server.setFloor(old, savedStart-1)
			killed := errors.New("killed")
			create := h.server.create("ses-1")
			switch point {
			case "after the reservation":
				create = func(context.Context, string, Start) (*jetstream.ConsumerInfo, error) { return nil, killed }
			case "after the create":
				h.store.beforeWrite = func(_ string, record Record) error {
					if record.Op == nil && record.Current != old {
						return killed
					}
					return nil
				}
			case "after the publish":
				names.cfg.Server = deleteFails{h.server, killed}
			}
			sel, err = names.Select(context.Background(), "ses-1", nil)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := names.Replace(context.Background(), sel, savedStart, create); point != "after the publish" && err == nil {
				t.Fatalf("Replace survived its kill %s", point)
			}
			h.store.beforeWrite = nil
			h.clock.advance(3 * time.Second)

			recovered, err := h.names("p2").Select(context.Background(), "ses-1", h.server.create("ses-1"))
			if err != nil {
				t.Fatalf("recovery: %v", err)
			}
			if _, err := h.names("p2").Cleanup(context.Background(), "ses-1"); err != nil {
				t.Fatalf("cleanup: %v", err)
			}
			current := recovered.Record.Current
			if current == old || !h.server.holds(current) {
				t.Fatalf("current %s (held %t); want a replacement the server holds", current, h.server.holds(current))
			}
			if start := h.server.startOf(current); start.Seq != savedStart {
				t.Fatalf("the replacement starts at %d, want the saved %d", start.Seq, savedStart)
			}
			if h.server.holds(old) {
				t.Fatalf("the predecessor %s is still held", old)
			}
			for name := range h.server.consumers {
				if name != current {
					t.Fatalf("the server holds %s beside the current %s", name, current)
				}
			}
			h.server.createdTwice(t)
		})
	}
}

// deleteFails fails every delete with err, as a process killed before its delete leaves it.
type deleteFails struct {
	*fakeServer
	err error
}

func (d deleteFails) Delete(context.Context, string) error { return d.err }

func TestTwoCompetingCreatorsMakeOneCurrentAndNoNameTwice(t *testing.T) {
	h := newHarness()
	h.clock.advance(0)
	var wg sync.WaitGroup
	results := make([]Selection, 2)
	errs := make([]error, 2)
	for i := range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], _, errs[i] = h.names(fmt.Sprintf("p%d", i)).Ensure(context.Background(), "ses-1", CreateRequest{Create: h.server.create("ses-1"), First: first(2)})
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("creator %d: %v", i, err)
		}
	}
	if results[0].Record.Current != results[1].Record.Current {
		t.Fatalf("the creators selected %s and %s", results[0].Record.Current, results[1].Record.Current)
	}
	h.server.createdTwice(t)
}

func TestAPausedCreatorThatFinishesAfterRetirementIsNotSelected(t *testing.T) {
	h := newHarness()
	release := make(chan struct{})
	reached := make(chan string, 1)
	paused := true
	h.server.createFault = func(name string) (bool, error) {
		if paused {
			paused = false
			reached <- name
			h.server.mu.Unlock()
			<-release
			h.server.mu.Lock()
		}
		return true, nil
	}
	done := make(chan error, 1)
	go func() {
		_, _, err := h.names("p1").Ensure(context.Background(), "ses-1", CreateRequest{Create: h.server.create("ses-1"), First: first(2)})
		done <- err
	}()
	pausedName := <-reached
	h.clock.advance(3 * time.Second)
	recovered, err := h.names("p2").Select(context.Background(), "ses-1", h.server.create("ses-1"))
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Record.Current == pausedName || !contains(recovered.Record.Retired, pausedName) {
		t.Fatalf("recovery selected %s with retired %v; want the paused %s retired", recovered.Record.Current, recovered.Record.Retired, pausedName)
	}
	close(release)
	<-done
	if rec := h.store.record(t, "ses-1"); rec.Current != recovered.Record.Current {
		t.Fatalf("after the paused creator finished, current is %s, want %s", rec.Current, recovered.Record.Current)
	}
	if _, err := h.names("p2").Cleanup(context.Background(), "ses-1"); err != nil {
		t.Fatal(err)
	}
	if h.server.holds(pausedName) {
		t.Fatalf("cleanup left the retired %s", pausedName)
	}
	h.server.createdTwice(t)
}

func TestASavedLowerStartWinsOverAPredecessorThatStillExists(t *testing.T) {
	h := newHarness()
	names := h.names("p1")
	sel, _, err := names.Ensure(context.Background(), "ses-1", CreateRequest{Create: h.server.create("ses-1"), First: first(1)})
	if err != nil {
		t.Fatal(err)
	}
	old := sel.Record.Current
	h.server.setFloor(old, 50)
	killed := errors.New("killed")
	if _, err := names.Replace(context.Background(), sel, 51, func(context.Context, string, Start) (*jetstream.ConsumerInfo, error) { return nil, killed }); !errors.Is(err, killed) {
		t.Fatalf("Replace = %v, want the kill", err)
	}
	// The predecessor goes on to floor 100 while the candidate never landed.
	h.server.setFloor(old, 100)
	h.clock.advance(3 * time.Second)
	recovered, err := h.names("p2").Select(context.Background(), "ses-1", h.server.create("ses-1"))
	if err != nil {
		t.Fatal(err)
	}
	if start := h.server.startOf(recovered.Record.Current); start.Seq != 51 {
		t.Fatalf("recovery starts at %d, want the saved 51, not the predecessor's later floor", start.Seq)
	}
}

func TestACurrentThatDisappearsRightAfterItsPublishRestartsAtTheSavedStart(t *testing.T) {
	h := newHarness()
	h.store.put("ses-1", Record{Current: "session~N", Start: 21, LastFloor: &Floor{Floor: 100, Name: "session~O", StreamID: "stream-1"}})
	sel, err := h.names("p1").Select(context.Background(), "ses-1", h.server.create("ses-1"))
	if err != nil {
		t.Fatal(err)
	}
	if start := h.server.startOf(sel.Record.Current); start.Seq != 21 {
		t.Fatalf("restarted at %d, want the saved 21 over a last floor read from another name", start.Seq)
	}
}

func TestAMissingCurrentStartsAtItsOwnLastFloorOnThisStream(t *testing.T) {
	for name, tc := range map[string]struct {
		record Record
		want   uint64
		err    error
	}{
		"its last floor":            {Record{Current: "session~N", Start: 3, LastFloor: &Floor{Floor: 40, Name: "session~N", StreamID: "stream-1"}}, 41, nil},
		"a last floor of 0":         {Record{Current: "session~N", Start: 3, LastFloor: &Floor{Floor: 0, Name: "session~N", StreamID: "stream-1"}}, 1, nil},
		"another stream's floor":    {Record{Current: "session~N", Start: 3, LastFloor: &Floor{Floor: 40, Name: "session~N", StreamID: "stream-0"}}, 3, nil},
		"another name's floor":      {Record{Current: "session~N", Start: 3, LastFloor: &Floor{Floor: 40, Name: "session~O", StreamID: "stream-1"}}, 3, nil},
		"no floor and no start":     {Record{Current: "session~N"}, 0, ErrNoSafeStart},
		"only another name's floor": {Record{Current: "session~N", LastFloor: &Floor{Floor: 40, Name: "session~O", StreamID: "stream-1"}}, 0, ErrNoSafeStart},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness()
			h.store.put("ses-1", tc.record)
			sel, err := h.names("p1").Select(context.Background(), "ses-1", h.server.create("ses-1"))
			if tc.err != nil {
				if !errors.Is(err, tc.err) || !strings.Contains(err.Error(), "selection of ses-1 names a missing consumer and no safe start is known") {
					t.Fatalf("Select = %v, want %v", err, tc.err)
				}
				if len(h.server.creates) != 0 {
					t.Fatalf("created %v with no safe start", h.server.creates)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if start := h.server.startOf(sel.Record.Current); start.Seq != tc.want {
				t.Fatalf("created at %d, want %d", start.Seq, tc.want)
			}
		})
	}
}

// A stream that carries no identity cannot be told from one recreated with its sequences begun
// again, so a last floor read on it never counts, and the saved start governs.
func TestALastFloorOnAStreamWithNoIdentityDoesNotCount(t *testing.T) {
	h := newHarness()
	h.store.put("ses-1", Record{Current: "session~N", Start: 3, LastFloor: &Floor{Floor: 40, Name: "session~N", StreamID: ""}})
	names := h.names("p1")
	names.cfg.StreamID = func(context.Context) (string, error) { return "", nil }
	sel, err := names.Select(context.Background(), "ses-1", h.server.create("ses-1"))
	if err != nil {
		t.Fatal(err)
	}
	if start := h.server.startOf(sel.Record.Current); start.Seq != 3 {
		t.Fatalf("created at %d, want the saved 3 over a last floor with no stream identity", start.Seq)
	}
}

func TestDeleteRecordsTheFloorBeforeItDeletesAndKeepsTheStart(t *testing.T) {
	h := newHarness()
	names := h.names("p1")
	var recorded []uint64
	names.cfg.RecordFloor = func(_ context.Context, _ string, floor uint64) (uint64, error) {
		recorded = append(recorded, floor)
		return floor, nil
	}
	sel, _, err := names.Ensure(context.Background(), "ses-1", CreateRequest{Create: h.server.create("ses-1"), First: first(1)})
	if err != nil {
		t.Fatal(err)
	}
	h.server.setFloor(sel.Record.Current, 33)
	floor, err := names.Delete(context.Background(), "ses-1", nil)
	if err != nil || floor != 33 {
		t.Fatalf("Delete = %d, %v; want floor 33", floor, err)
	}
	rec := h.store.record(t, "ses-1")
	if rec.Current != "" || rec.Op != nil || rec.Start != 34 || h.server.holds(sel.Record.Current) || len(recorded) != 1 {
		t.Fatalf("record %+v, held %t, recorded %v; want no current, start 34, the consumer gone and one floor recorded", rec, h.server.holds(sel.Record.Current), recorded)
	}
	// The next registration creates a fresh name at that start.
	next, _, err := names.Ensure(context.Background(), "ses-1", CreateRequest{Create: h.server.create("ses-1"), First: first(90)})
	if err != nil {
		t.Fatal(err)
	}
	if next.Record.Current == sel.Record.Current || h.server.startOf(next.Record.Current).Seq != 34 {
		t.Fatalf("the next create is %s at %+v, want a fresh name at 34", next.Record.Current, h.server.startOf(next.Record.Current))
	}
	h.server.createdTwice(t)
}

func TestADeleteKilledBeforeItRecordedTheFloorIsFinishedFromTheConsumersInfo(t *testing.T) {
	h := newHarness()
	names := h.names("p1")
	sel, _, err := names.Ensure(context.Background(), "ses-1", CreateRequest{Create: h.server.create("ses-1"), First: first(1)})
	if err != nil {
		t.Fatal(err)
	}
	h.server.setFloor(sel.Record.Current, 12)
	killed := errors.New("killed")
	names.cfg.RecordFloor = func(context.Context, string, uint64) (uint64, error) { return 0, killed }
	if _, err := names.Delete(context.Background(), "ses-1", nil); !errors.Is(err, killed) {
		t.Fatalf("Delete = %v, want the kill", err)
	}
	if rec := h.store.record(t, "ses-1"); rec.Op == nil || rec.Op.Kind != OpDelete || rec.Op.FloorRecorded {
		t.Fatalf("record %+v, want the delete open with no floor recorded", rec)
	}
	other := h.names("p2")
	other.cfg.RecordFloor = func(_ context.Context, _ string, floor uint64) (uint64, error) { return floor, nil }
	if _, err := other.Select(context.Background(), "ses-1", nil); err != nil {
		t.Fatal(err)
	}
	rec := h.store.record(t, "ses-1")
	if rec.Current != "" || rec.Op != nil || rec.Start != 13 || h.server.holds(sel.Record.Current) {
		t.Fatalf("record %+v; want the delete finished at start 13 and the consumer gone", rec)
	}
}

func TestADeleteWhoseFloorCannotBeReadStops(t *testing.T) {
	h := newHarness()
	names := h.names("p1")
	sel, _, err := names.Ensure(context.Background(), "ses-1", CreateRequest{Create: h.server.create("ses-1"), First: first(1)})
	if err != nil {
		t.Fatal(err)
	}
	unreadable := errors.New("info timed out")
	names.cfg.Server = infoFails{h.server, unreadable}
	if _, err := names.Delete(context.Background(), "ses-1", nil); !errors.Is(err, unreadable) {
		t.Fatalf("Delete = %v, want the unread floor to stop it", err)
	}
	if !h.server.holds(sel.Record.Current) {
		t.Fatal("the consumer was deleted with its floor unread")
	}
}

type infoFails struct {
	*fakeServer
	err error
}

func (i infoFails) Info(context.Context, string) (*jetstream.ConsumerInfo, error) { return nil, i.err }

func TestAFullCapDeletesRetiredNamesThenThePredecessorWithNoCapUpdate(t *testing.T) {
	h := newHarness()
	names := h.names("p1")
	sel, _, err := names.Ensure(context.Background(), "ses-1", CreateRequest{Create: h.server.create("ses-1"), First: first(1)})
	if err != nil {
		t.Fatal(err)
	}
	old := sel.Record.Current
	h.server.setFloor(old, 7)
	// The cap is full: the predecessor holds the one slot.
	h.server.capacity = 1
	sel, err = names.Select(context.Background(), "ses-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	replaced, err := names.Replace(context.Background(), sel, 8, h.server.create("ses-1"))
	if err != nil {
		t.Fatalf("Replace at a full cap = %v", err)
	}
	if h.server.holds(old) || h.server.startOf(replaced.Record.Current).Seq != 8 {
		t.Fatalf("predecessor held %t, replacement at %+v; want the predecessor deleted under the saved start and the replacement at 8", h.server.holds(old), h.server.startOf(replaced.Record.Current))
	}
	h.server.createdTwice(t)
}

func TestTheLastFloorIsWrittenOnlyForTheCurrentName(t *testing.T) {
	h := newHarness()
	names := h.names("p1")
	sel, _, err := names.Ensure(context.Background(), "ses-1", CreateRequest{Create: h.server.create("ses-1"), First: first(1)})
	if err != nil {
		t.Fatal(err)
	}
	if err := names.RecordLastFloor(context.Background(), "ses-1", sel.Record.Current, 17, "stream-1"); err != nil {
		t.Fatal(err)
	}
	if lf := h.store.record(t, "ses-1").LastFloor; lf == nil || lf.Floor != 17 || lf.Name != sel.Record.Current || lf.StreamID != "stream-1" {
		t.Fatalf("last floor %+v, want 17 of %s on stream-1", lf, sel.Record.Current)
	}
	if err := names.RecordLastFloor(context.Background(), "ses-1", "session~other", 99, "stream-1"); err != nil {
		t.Fatal(err)
	}
	if lf := h.store.record(t, "ses-1").LastFloor; lf.Floor != 17 {
		t.Fatalf("a floor of another name moved the last floor to %+v", lf)
	}
}

func TestTheCountersCallbackHearsEachStep(t *testing.T) {
	h := newHarness()
	names := h.names("p1")
	var heard []string
	names.cfg.Observe = func(op Operation, outcome Outcome) { heard = append(heard, string(op)+"/"+string(outcome)) }
	if _, _, err := names.Ensure(context.Background(), "ses-1", CreateRequest{Create: h.server.create("ses-1"), First: first(1)}); err != nil {
		t.Fatal(err)
	}
	h.store.state.Cutover = ""
	_, _, _ = names.Ensure(context.Background(), "ses-2", CreateRequest{Create: h.server.create("ses-2"), First: first(1)})
	want := []string{"create/ok", "select/refused"}
	if strings.Join(heard, ",") != strings.Join(want, ",") {
		t.Fatalf("heard %v, want %v", heard, want)
	}
}

func TestTheLogicalNameIsThePhysicalNameCutAtItsLastTilde(t *testing.T) {
	for physical, logical := range map[string]string{
		"legion-go-p-github~abc": "legion-go-p-github",
		"legion-go-p-github":     "legion-go-p-github",
		"delivery-jobs~a~b":      "delivery-jobs~a",
	} {
		if got := LogicalOf(physical); got != logical {
			t.Errorf("LogicalOf(%q) = %q, want %q", physical, got, logical)
		}
	}
	name := FreshName("session")
	if !strings.HasPrefix(name, "session~") || len(name) != len("session~")+22 || FreshName("session") == name {
		t.Fatalf("FreshName = %q, want session~ and a fresh 22-character suffix", name)
	}
}

// A fresh name cannot be told from the names one process made before it: each suffix is 22
// characters of [0-9A-Za-z] drawn from crypto/rand (about 131 bits), so a principal that can create
// consumers and sees one listener's names or reply inboxes cannot create its next candidate first.
// Of 2,000 successive names, no two share the first 12 characters of their suffix, which two random
// suffixes do with a chance near 2e-15; each character of the alphabet appears in some suffix; and
// every suffix is the 22 characters freshOf takes. Before, the suffix was a NUID: a random 12-
// character prefix shared by every name of the process and a sequence that advances by a fixed
// increment, so every pair shared those 12 characters.
func TestFreshNamesCannotBePredictedFromEachOther(t *testing.T) {
	const names = 2000
	seen := map[string]string{}
	used := map[rune]bool{}
	for range names {
		name := FreshName("session")
		suffix, ok := strings.CutPrefix(name, "session~")
		if !ok || len(suffix) != 22 || strings.ContainsFunc(suffix, func(r rune) bool {
			return !('0' <= r && r <= '9' || 'a' <= r && r <= 'z' || 'A' <= r && r <= 'Z')
		}) {
			t.Fatalf("FreshName = %q, want session~ and 22 characters of [0-9A-Za-z]", name)
		}
		if other, ok := seen[suffix[:12]]; ok {
			t.Fatalf("FreshName gave %q and %q, which share the first 12 characters of their suffix: the next name can be predicted from one before", other, name)
		}
		seen[suffix[:12]] = name
		for _, r := range suffix {
			used[r] = true
		}
	}
	if len(used) != 62 {
		t.Fatalf("%d names used %d of the 62 characters of [0-9A-Za-z]; want every one", names, len(used))
	}
}

func contains(names []string, name string) bool {
	for _, n := range names {
		if n == name {
			return true
		}
	}
	return false
}

func TestADurableKeyIsADotKeyNoSessionHoldsAndTwoNamesNeverShare(t *testing.T) {
	for durable, key := range map[string]string{
		"listener-host-a": "durable.listener-host-a",
		"listener-a.b":    "durable.listener-a=2Eb",
		"listener-a=b":    "durable.listener-a=3Db",
		"listener-a b":    "durable.listener-a=20b",
	} {
		if got := DurableKey(durable); got != key {
			t.Errorf("DurableKey(%q) = %q, want %q", durable, got, key)
		}
		if err := CheckDurableKey(key); err != nil {
			t.Errorf("CheckDurableKey(%q) = %v, want it accepted", key, err)
		}
		// No session key is a durable's: a session id holds no '.'.
		if err := CheckKey(key); !errors.Is(err, ErrInvalidKey) {
			t.Errorf("CheckKey(%q) = %v, want ErrInvalidKey", key, err)
		}
	}
	if DurableKey("listener-a=2Eb") == DurableKey("listener-a.b") {
		t.Fatal("two durable names share a key")
	}
	for _, key := range []string{"listener-host-a", "durable.", "durable.a.b", "durable.a b", "envoy.bucket"} {
		if err := CheckDurableKey(key); !errors.Is(err, ErrInvalidKey) {
			t.Errorf("CheckDurableKey(%q) = %v, want ErrInvalidKey", key, err)
		}
	}
}

func TestAFamilysOwnKeyCheckReplacesTheSessionCheck(t *testing.T) {
	h := newHarness()
	names := h.names("p1")
	names.cfg.CheckKey = CheckDurableKey
	key := DurableKey("listener-host-a")
	sel, _, err := names.Ensure(context.Background(), key, CreateRequest{Create: h.server.create(key), First: first(1)})
	if err != nil {
		t.Fatalf("Ensure of a durable key with the durable check = %v", err)
	}
	if !strings.HasPrefix(sel.Record.Current, "session~") {
		t.Fatalf("current %q", sel.Record.Current)
	}
	// Control: the session check refuses the same key.
	if _, _, err := h.names("p1").Ensure(context.Background(), key, CreateRequest{Create: h.server.create(key), First: first(1)}); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("Ensure of a durable key with the session check = %v, want ErrInvalidKey", err)
	}
}

// A first create at deliver policy all keeps it under the cutover boundary, since every message the
// stream holds starts no later than the boundary; one at deliver policy new starts at the boundary.
func TestANativeAllFirstCreateIsKeptUnderTheBoundary(t *testing.T) {
	for native, want := range map[Native]Start{NativeAll: {Seq: 1, Native: NativeAll}, NativeNew: {Seq: 12}} {
		h := newHarness()
		h.store.state.Boundary = 12
		sel, _, err := h.names("p1").Ensure(context.Background(), "ses-1", CreateRequest{Create: h.server.create("ses-1"), First: func(context.Context) (Start, error) {
			return Start{Seq: 1, Native: native}, nil
		}})
		if err != nil {
			t.Fatal(err)
		}
		if got := h.server.startOf(sel.Record.Current); got != want {
			t.Errorf("a %s first create under boundary 12 started at %+v, want %+v", native, got, want)
		}
	}
}

// A replacement whose predecessor the server confirmed deleted keeps no record of it: a session
// restarted many times keeps a record of one size. The control is a predecessor whose delete
// failed, which stays retired for the cleanup.
func TestAPredecessorTheReplacementDeletedIsRetiredNoLonger(t *testing.T) {
	h := newHarness()
	names := h.names("p1")
	sel, _, err := names.Ensure(context.Background(), "ses-1", CreateRequest{Create: h.server.create("ses-1"), First: first(1)})
	if err != nil {
		t.Fatal(err)
	}
	for round := range 5 {
		if sel, err = names.Replace(context.Background(), sel, 1, h.server.create("ses-1")); err != nil {
			t.Fatalf("round %d: %v", round, err)
		}
	}
	if retired := h.store.record(t, "ses-1").Retired; len(retired) != 0 {
		t.Fatalf("after five replacements whose predecessors were deleted the record retires %v, want none", retired)
	}
	// Control: a predecessor whose delete failed stays retired.
	old := sel.Record.Current
	names.cfg.Server = deleteFails{h.server, errors.New("the delete timed out")}
	if _, err := names.Replace(context.Background(), sel, 1, h.server.create("ses-1")); err != nil {
		t.Fatal(err)
	}
	if retired := h.store.record(t, "ses-1").Retired; !contains(retired, old) || len(retired) != 1 {
		t.Fatalf("after a replacement whose predecessor's delete failed the record retires %v, want only %s", retired, old)
	}
}

// nativeNew is a first-create rule that keeps deliver policy new and reads no stream: the daemon's.
func nativeNew(context.Context) (Start, error) { return Start{Native: NativeNew}, nil }

// With Config.NativeFirstCreate, a logical name that never existed is created at deliver policy
// new even when the cutover boundary is set, and the publish saves the start the created consumer
// reports. The control is the same first create without the option, which the boundary turns into
// a create by start sequence at B, as the sessions family keeps it.
func TestANativeFirstCreateKeepsDeliverNewUnderTheBoundaryWithTheOption(t *testing.T) {
	const key = "legion-go-p-github"
	h := newHarness()
	h.store.state.Boundary = 12
	sel, _, err := h.names("p1").Ensure(context.Background(), key, CreateRequest{Create: h.server.create(key), First: nativeNew})
	if err != nil {
		t.Fatal(err)
	}
	if start := h.server.startOf(sel.Record.Current); start != (Start{Seq: 12}) {
		t.Fatalf("control: without the option the first create started at %+v, want by start sequence at the boundary 12", start)
	}

	h = newHarness()
	h.store.state.Boundary = 12
	h.server.lastSeq = 100
	names := h.names("p1")
	names.cfg.NativeFirstCreate = true
	sel, created, err := names.Ensure(context.Background(), key, CreateRequest{Create: h.server.create(key), First: nativeNew})
	if err != nil || !created {
		t.Fatalf("Ensure with the option = %v, created %t", err, created)
	}
	if start := h.server.startOf(sel.Record.Current); start != (Start{Native: NativeNew}) {
		t.Fatalf("with the option the first create started at %+v, want deliver policy new", start)
	}
	if rec := h.store.record(t, key); rec.Start != 101 || sel.Record.Start != 101 {
		t.Fatalf("the published record's start is %d (selection %d), want 101, the start the consumer reports", rec.Start, sel.Record.Start)
	}
}

// A native first create whose candidate landed is adopted with the start it reports, whether its
// create's answer was lost or another process left it open for Step 0.
func TestAnAdoptedNativeFirstCreateSavesItsOwnStart(t *testing.T) {
	const key = "legion-go-p-github"
	t.Run("its create's answer lost", func(t *testing.T) {
		h := newHarness()
		h.store.state.Boundary = 12
		h.server.lastSeq = 100
		h.server.createFault = func(string) (bool, error) { return true, errAnswerLost }
		names := h.names("p1")
		names.cfg.NativeFirstCreate = true
		sel, created, err := names.Ensure(context.Background(), key, CreateRequest{Create: h.server.create(key), First: nativeNew})
		if err != nil || !created {
			t.Fatalf("Ensure = %v, created %t; want the landed candidate adopted", err, created)
		}
		if start := h.server.startOf(sel.Record.Current); start != (Start{Native: NativeNew}) || h.store.record(t, key).Start != 101 {
			t.Fatalf("adopted %+v with record start %d, want deliver policy new and start 101", start, h.store.record(t, key).Start)
		}
		h.server.createdTwice(t)
	})
	t.Run("Step 0", func(t *testing.T) {
		h := newHarness()
		h.store.state.Boundary = 12
		h.server.lastSeq = 100
		if _, err := h.server.create(key)(context.Background(), "session~N", Start{Native: NativeNew}); err != nil {
			t.Fatal(err)
		}
		h.store.put(key, Record{Op: &Op{Kind: OpCreate, Owner: "p1", Candidate: "session~N", Native: NativeNew, ReservedAt: h.clock.Now()}})
		names := h.names("p2")
		names.cfg.NativeFirstCreate = true
		sel, err := names.Select(context.Background(), key, h.server.create(key))
		if err != nil {
			t.Fatal(err)
		}
		if sel.Record.Current != "session~N" || h.store.record(t, key).Start != 101 {
			t.Fatalf("Step 0 selected %q with record start %d, want session~N at 101", sel.Record.Current, h.store.record(t, key).Start)
		}
		h.server.createdTwice(t)
	})
}

// A native first create whose candidate never landed, its start still unknown, is retired and
// created natively again, as today's first create is: never by start sequence at 0.
func TestANativeFirstCreateThatNeverLandedIsCreatedNativelyAgain(t *testing.T) {
	const key = "legion-go-p-github"
	check := func(t *testing.T, h *harness, sel Selection, lost string) {
		t.Helper()
		if sel.Record.Current == lost || h.server.startOf(sel.Record.Current) != (Start{Native: NativeNew}) {
			t.Fatalf("current %q started at %+v, want a fresh candidate at deliver policy new", sel.Record.Current, h.server.startOf(sel.Record.Current))
		}
		if rec := h.store.record(t, key); rec.Start != 101 || !contains(rec.Retired, lost) {
			t.Fatalf("record %+v, want start 101 and %s retired", rec, lost)
		}
		h.server.createdTwice(t)
	}
	t.Run("Step 0", func(t *testing.T) {
		h := newHarness()
		h.store.state.Boundary = 12
		h.server.lastSeq = 100
		h.store.put(key, Record{Op: &Op{Kind: OpCreate, Owner: "p1", Candidate: "session~lost", Native: NativeNew, ReservedAt: h.clock.Now()}})
		h.clock.advance(3 * time.Second)
		names := h.names("p2")
		names.cfg.NativeFirstCreate = true
		sel, err := names.Select(context.Background(), key, h.server.create(key))
		if err != nil {
			t.Fatal(err)
		}
		check(t, h, sel, "session~lost")
	})
	t.Run("its create's answer lost", func(t *testing.T) {
		h := newHarness()
		h.store.state.Boundary = 12
		h.server.lastSeq = 100
		var lost string
		h.server.createFault = func(name string) (bool, error) {
			if lost == "" {
				lost = name
				return false, errAnswerLost
			}
			return true, nil
		}
		names := h.names("p1")
		names.cfg.NativeFirstCreate = true
		sel, _, err := names.Ensure(context.Background(), key, CreateRequest{Create: h.server.create(key), First: nativeNew})
		if err != nil {
			t.Fatal(err)
		}
		check(t, h, sel, lost)
	})
}

// countingServer counts, by name, the Info and Delete calls its fakeServer receives.
type countingServer struct {
	*fakeServer
	mu    sync.Mutex
	calls map[string]int
}

func (c *countingServer) count(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.calls == nil {
		c.calls = map[string]int{}
	}
	c.calls[name]++
}

func (c *countingServer) Info(ctx context.Context, name string) (*jetstream.ConsumerInfo, error) {
	c.count(name)
	return c.fakeServer.Info(ctx, name)
}

func (c *countingServer) Delete(ctx context.Context, name string) error {
	c.count(name)
	return c.fakeServer.Delete(ctx, name)
}

// A family whose server cannot delete (Config.NoDelete) answers a full cap with ErrConsumerLimit
// and reads or deletes none of its retired names, which the server holds. The control is the same
// cap without the option: the retired name is read and deleted, which frees its slot, and the create
// is served.
func TestAFamilyThatCannotDeleteAnswersAFullCapWithoutTouchingItsRetiredNames(t *testing.T) {
	for _, noDelete := range []bool{false, true} {
		t.Run(fmt.Sprintf("NoDelete %t", noDelete), func(t *testing.T) {
			h := newHarness()
			ctx := context.Background()
			if _, err := h.server.create("ses-1")(ctx, "session~retired", Start{Seq: 1}); err != nil {
				t.Fatal(err)
			}
			h.store.put("ses-1", Record{Start: 5, Retired: []string{"session~retired"}})
			h.server.capacity = 1
			counting := &countingServer{fakeServer: h.server}
			names := h.names("p1")
			names.cfg.Server = counting
			names.cfg.NoDelete = noDelete
			_, _, err := names.Ensure(ctx, "ses-1", CreateRequest{Create: h.server.create("ses-1"), First: first(1)})
			calls := counting.calls["session~retired"]
			if noDelete {
				if !errors.Is(err, ErrConsumerLimit) || calls != 0 || !h.server.holds("session~retired") {
					t.Fatalf("Ensure = %v with %d calls on the retired name (held %t); want ErrConsumerLimit, no call and the name kept", err, calls, h.server.holds("session~retired"))
				}
				return
			}
			if err != nil || calls == 0 || h.server.holds("session~retired") {
				t.Fatalf("control: Ensure = %v with %d calls on the retired name (held %t); want it read and deleted and the create served", err, calls, h.server.holds("session~retired"))
			}
		})
	}
}
