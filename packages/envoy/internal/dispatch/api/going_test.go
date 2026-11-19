package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/going"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/explaintest"
)

// goingRig is the record under test: a server on the harness's database, called directly. These
// are the functions the answer's routes call, not the routes, so nothing here goes through the
// mux.
type goingRig struct {
	server   *server
	database *store.Store
}

func newGoingRig(t *testing.T) goingRig {
	t.Helper()
	_, database, deps, _ := newRegisteredTestServer(t, testServerOptions{})
	return goingRig{server: directServer(deps), database: database}
}

// goingAsker is the actor every run here is recorded for.
var goingAsker = model.Actor{Kind: "user", ID: "alice"}

// record stores one run, failing the test if it cannot.
func (rig goingRig) record(t *testing.T, words string) goingRequest {
	t.Helper()
	request, err := rig.server.recordGoingRequest(context.Background(), goingAsker, "TEST", words)
	if err != nil {
		t.Fatalf("record the run: %v", err)
	}
	return request
}

// frames is every frame of the run, in stream order, rendered "<sequence> <name> <data>" so one
// comparison says what the stream holds. It reads them as a reader of the stream does, through
// readGoingStream, so no test here runs a read the server does not.
func (rig goingRig) frames(t *testing.T, requestID string) []string {
	t.Helper()
	stored, _, err := rig.server.readGoingStream(context.Background(), requestID, 0)
	if err != nil {
		t.Fatalf("read the run's frames: %v", err)
	}
	rendered := make([]string, 0, len(stored))
	for _, frame := range stored {
		data, err := json.Marshal(frame.Event.Data)
		if err != nil {
			t.Fatalf("marshal frame %d: %v", frame.Sequence, err)
		}
		rendered = append(rendered, fmt.Sprintf("%d %s %s", frame.Sequence, frame.Event.Name, data))
	}
	return rendered
}

// age moves the run's created_at and its ends_at back by the two durations, so a test can hold a
// run its deadline has passed, or one hours old, without waiting for either.
func (rig goingRig) age(t *testing.T, requestID string, created, ends time.Duration) {
	t.Helper()
	if _, err := rig.database.Pool.Exec(context.Background(), `
		update going_requests set created_at = now() - $2::interval, ends_at = now() - $3::interval where id = $1::uuid
	`, requestID, created.String(), ends.String()); err != nil {
		t.Fatalf("age the run: %v", err)
	}
}

// A run records what was asked and the window it has: ends_at is contracts.GoingRunWindow past
// created_at, written by the POST and not by the process running the answer, and the row reads
// back to whoever holds the id, on any pod. An id the record does not hold is pgx.ErrNoRows, the
// absence its caller decides what to answer.
func TestAGoingRequestRecordsItsWindowAndReadsBack(t *testing.T) {
	rig := newGoingRig(t)
	recorded := rig.record(t, "what is going on?")
	if window := recorded.EndsAt.Sub(recorded.CreatedAt); window != contracts.GoingRunWindow {
		t.Fatalf("the run's window is %v (created_at %v, ends_at %v), want %v",
			window, recorded.CreatedAt, recorded.EndsAt, contracts.GoingRunWindow)
	}
	read, err := rig.server.readGoingRequest(context.Background(), recorded.ID)
	if err != nil {
		t.Fatalf("read the run back: %v", err)
	}
	if !read.Author.SameAs(goingAsker) || read.Project != "TEST" || read.Words != "what is going on?" {
		t.Fatalf("read back author %+v project %q words %q, want alice's TEST question", read.Author, read.Project, read.Words)
	}
	if !read.CreatedAt.Equal(recorded.CreatedAt) || !read.EndsAt.Equal(recorded.EndsAt) || read.ID != recorded.ID {
		t.Fatalf("read back %+v, want the row the POST stored %+v", read, recorded)
	}
	_, err = rig.server.readGoingRequest(context.Background(), "00000000-0000-4000-8000-00000000000a")
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("reading a run nobody asked for: %v, want pgx.ErrNoRows", err)
	}
}

// The frames of one stream take their sequences one after another, and a reader resuming from
// Last-Event-ID is answered the frames past the one it holds, in order.
func TestGoingEventsAppendInSequenceAndReadAfterOne(t *testing.T) {
	rig := newGoingRig(t)
	run := rig.record(t, "who closed INFRA-1?")
	ctx := context.Background()
	for number, event := range []going.Event{going.Text("Nobody "), going.Text("has."), going.Item(going.Hit{Kind: "issue", ID: "INFRA-1", Title: "One", Href: "/issues/INFRA-1"})} {
		sequence, err := appendGoingEvent(ctx, rig.database.Pool, run.ID, event)
		if err != nil {
			t.Fatalf("append %s: %v", event.Name, err)
		}
		if sequence != number+1 {
			t.Fatalf("%s took sequence %d, want %d: the frames follow one another", event.Name, sequence, number+1)
		}
	}
	ended, err := endGoingRun(ctx, rig.database.Pool, run.ID, going.End(2, 1))
	if err != nil {
		t.Fatalf("end the run: %v", err)
	}
	if ended.Sequence != 4 {
		t.Fatalf("the end took sequence %d, want 4", ended.Sequence)
	}

	after, _, err := rig.server.readGoingStream(ctx, run.ID, 2)
	if err != nil {
		t.Fatalf("read the frames after 2: %v", err)
	}
	if len(after) != 2 || after[0].Sequence != 3 || after[0].Event.Name != contracts.GoingEventItem ||
		after[1].Sequence != 4 || after[1].Event.Name != contracts.GoingEventEnd {
		t.Fatalf("the frames after 2 are %+v, want the item then the end", after)
	}
	data, ok := after[0].Event.Data.(json.RawMessage)
	if !ok {
		t.Fatalf("the item's payload is %T, want the JSON the row holds", after[0].Event.Data)
	}
	if !jsonEqual(t, data, `{"kind":"issue","id":"INFRA-1","title":"One","href":"/issues/INFRA-1"}`) {
		t.Fatalf("the item frame carries %s, want the hit it named", data)
	}
	if frames := rig.frames(t, run.ID); len(frames) != 4 {
		t.Fatalf("the stream holds %v, want four frames from sequence 1", frames)
	}
}

// A run is ended once, and the table says by whom: whichever terminal frame is stored first is
// the run's, and a second terminal write - the reader at the deadline, the sweep, or the answer
// itself - stores nothing and is answered that stored frame, with its sequence, rather than
// storing a second ending at the next sequence.
func TestASecondTerminalWriteOfAGoingRunAnswersTheStoredFrame(t *testing.T) {
	for name, test := range map[string]struct{ first, second going.Event }{
		"the answer ends, then the deadline": {going.End(12, 2), going.Fault(contracts.GoingErrorAnswerDidNotArrive)},
		"the deadline ends, then the answer": {going.Fault(contracts.GoingErrorAnswerFailed), going.End(3, 0)},
	} {
		t.Run(name, func(t *testing.T) {
			rig := newGoingRig(t)
			run := rig.record(t, "what happened?")
			ctx := context.Background()
			if _, err := appendGoingEvent(ctx, rig.database.Pool, run.ID, going.Text("Something")); err != nil {
				t.Fatalf("append the answer's first frame: %v", err)
			}
			first, err := endGoingRun(ctx, rig.database.Pool, run.ID, test.first)
			if err != nil {
				t.Fatalf("end the run with %s: %v", test.first.Name, err)
			}
			if first.Sequence != 2 || first.Event.Name != test.first.Name {
				t.Fatalf("the first ending is %+v, want %s at sequence 2", first, test.first.Name)
			}
			stored := rig.frames(t, run.ID)

			second, err := endGoingRun(ctx, rig.database.Pool, run.ID, test.second)
			if err != nil {
				t.Fatalf("the second ending (%s): %v", test.second.Name, err)
			}
			if second.Sequence != first.Sequence || second.Event.Name != test.first.Name {
				t.Fatalf("the second ending answered %+v, want the stored %s at sequence %d",
					second, test.first.Name, first.Sequence)
			}
			data, err := json.Marshal(second.Event.Data)
			if err != nil {
				t.Fatalf("marshal the answered payload: %v", err)
			}
			wanted, err := json.Marshal(test.first.Data)
			if err != nil {
				t.Fatalf("marshal the stored payload: %v", err)
			}
			if !jsonEqual(t, data, string(wanted)) {
				t.Fatalf("the second ending answered the payload %s, want the stored %s", data, wanted)
			}
			if frames := rig.frames(t, run.ID); !equalStrings(frames, stored) {
				t.Fatalf("the second ending left %v, want the stream it found %v", frames, stored)
			}
		})
	}
}

// A run that has ended takes no further frame: an answer still writing when the deadline's
// reader or the sweep ended its run is told so (pgx.ErrNoRows) rather than appending after the
// frame a reader stopped on. And a frame that ends no stream is no way to end a run.
func TestAGoingRunTakesNoFrameOnceItHasEnded(t *testing.T) {
	rig := newGoingRig(t)
	ctx := context.Background()
	run := rig.record(t, "is it over?")
	if _, err := endGoingRun(ctx, rig.database.Pool, run.ID, going.Fault(contracts.GoingErrorAnswerFailed)); err != nil {
		t.Fatalf("end the run: %v", err)
	}
	ended := rig.frames(t, run.ID)
	if _, err := appendGoingEvent(ctx, rig.database.Pool, run.ID, going.Text("more")); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("appending to an ended run: %v, want pgx.ErrNoRows", err)
	}
	if _, err := endGoingRun(ctx, rig.database.Pool, run.ID, going.Text("more")); err == nil {
		t.Fatal("ending a run with a text frame was accepted; only end and error end a run")
	}
	if frames := rig.frames(t, run.ID); !equalStrings(frames, ended) {
		t.Fatalf("the ended run holds %v, want the one ending %v", frames, ended)
	}
}

// A run past its ends_at with no terminal frame is ended by the sweep, since the pod that was
// answering it may have died and nobody is reading it: exactly the deadline's error, at the next
// sequence. A second pass stores nothing more, a run already ended is untouched, and a run still
// inside its window is left to run.
func TestTheGoingSweepEndsARunPastItsWindowOnce(t *testing.T) {
	rig := newGoingRig(t)
	ctx := context.Background()
	abandoned := rig.record(t, "who is answering this?")
	if _, err := appendGoingEvent(ctx, rig.database.Pool, abandoned.ID, going.Text("Half an ")); err != nil {
		t.Fatalf("append the abandoned run's frame: %v", err)
	}
	rig.age(t, abandoned.ID, time.Minute, time.Second)

	ended := rig.record(t, "and this one?")
	if _, err := endGoingRun(ctx, rig.database.Pool, ended.ID, going.End(4, 1)); err != nil {
		t.Fatalf("end the finished run: %v", err)
	}
	rig.age(t, ended.ID, time.Minute, time.Second)
	endedFrames := rig.frames(t, ended.ID)
	live := rig.record(t, "still thinking")

	if err := sweepGoingRequests(ctx, rig.database.Pool); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	deadline := fmt.Sprintf(`2 %s {"code":%q}`, contracts.GoingEventError, contracts.GoingErrorAnswerDidNotArrive)
	swept := rig.frames(t, abandoned.ID)
	if len(swept) != 2 || swept[1] != deadline {
		t.Fatalf("the swept run holds %v, want its own frame and %s", swept, deadline)
	}
	if frames := rig.frames(t, ended.ID); !equalStrings(frames, endedFrames) {
		t.Fatalf("the sweep left the finished run holding %v, want %v", frames, endedFrames)
	}
	if frames := rig.frames(t, live.ID); len(frames) != 0 {
		t.Fatalf("the sweep ended a run still inside its window: %v", frames)
	}

	if err := sweepGoingRequests(ctx, rig.database.Pool); err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	if frames := rig.frames(t, abandoned.ID); !equalStrings(frames, swept) {
		t.Fatalf("a second sweep left %v, want the one ending %v", frames, swept)
	}

	_, state, err := rig.server.readGoingStream(ctx, abandoned.ID, 0)
	if err != nil {
		t.Fatalf("read the swept run's state: %v", err)
	}
	if state.Terminal != 2 || !state.PastDeadline {
		t.Fatalf("the swept run reads %+v, want its ending at sequence 2 and past its deadline", state)
	}
	if _, state, err := rig.server.readGoingStream(ctx, live.ID, 0); err != nil || state.Terminal != 0 || state.PastDeadline {
		t.Fatalf("the live run reads %+v (%v), want no ending stored and not past its deadline", state, err)
	}
}

// TestARunAndItsFramesAreKeptPastTheHour is c-run-kept: a run and its frames are kept past the hour
// after which the release before deleted them (contracts.GoingRequestTTL, which this build reads
// nowhere). It records two runs of one person, appends one frame to each and ends neither, dates
// the first two hours back, and sweeps once. The first run's row, its frame and its interaction
// node all stand, the node at no state gone, and the run now holds a terminal frame, because the
// sweep still ends each overdue run. The second run is not overdue and holds no terminal frame.
func TestARunAndItsFramesAreKeptPastTheHour(t *testing.T) {
	rig := newGoingRig(t)
	ctx := context.Background()
	dated := rig.record(t, "asked two hours ago")
	recent := rig.record(t, "asked just now")
	for _, run := range []goingRequest{dated, recent} {
		if _, err := appendGoingEvent(ctx, rig.database.Pool, run.ID, going.Text("Half an ")); err != nil {
			t.Fatalf("append a frame of run %s: %v", run.ID, err)
		}
	}
	if _, err := rig.database.Pool.Exec(ctx, `
		update going_requests set created_at = now() - interval '2 hours', ends_at = now() - interval '2 hours'
		where id = $1::uuid
	`, dated.ID); err != nil {
		t.Fatalf("date run %s two hours back: %v", dated.ID, err)
	}
	datedFrames, recentFrames := rig.frames(t, dated.ID), rig.frames(t, recent.ID)
	if len(datedFrames) != 1 || len(recentFrames) != 1 {
		t.Fatalf("the two runs hold %v and %v before the sweep, want one frame each", datedFrames, recentFrames)
	}

	if err := sweepGoingRequests(ctx, rig.database.Pool); err != nil {
		t.Fatalf("sweep: %v", err)
	}

	if _, err := rig.server.readGoingRequest(ctx, dated.ID); err != nil {
		t.Fatalf("run %s, dated two hours back, reads %v after the sweep: want its row kept", dated.ID, err)
	}
	deadline := fmt.Sprintf(`2 %s {"code":%q}`, contracts.GoingEventError, contracts.GoingErrorAnswerDidNotArrive)
	if frames := rig.frames(t, dated.ID); len(frames) != 2 || frames[0] != datedFrames[0] || frames[1] != deadline {
		t.Fatalf("run %s holds %v after the sweep, want its own frame %s kept and the deadline's ending %s",
			dated.ID, frames, datedFrames[0], deadline)
	}
	node, held, err := graphstore.ReadNode(ctx, rig.database.Pool, graphstore.KindInteraction, dated.ID)
	if err != nil || !held || node.State == graphstore.StateGone {
		t.Fatalf("the interaction node of run %s reads held %t at state %q (%v) after the sweep, want it standing and not gone",
			dated.ID, held, node.State, err)
	}
	requireFramesHeldAsValues(t, rig.database, dated.ID, 2)

	if frames := rig.frames(t, recent.ID); !equalStrings(frames, recentFrames) {
		t.Fatalf("run %s, not overdue, holds %v after the sweep, want its one frame %v and no ending", recent.ID, frames, recentFrames)
	}
	if _, state, err := rig.server.readGoingStream(ctx, recent.ID, 0); err != nil || state.Terminal != 0 {
		t.Fatalf("run %s, not overdue, reads %+v (%v) after the sweep, want no terminal frame", recent.ID, state, err)
	}
}

// The sweep reads back goingSweepWindow from now and no further. A run whose ends_at passed inside
// that window, an hour short of its far end, with no ending stored is ended by one pass with the
// deadline's error, as every overdue run the sweep reads is. A run whose ends_at passed an hour
// before the window opens is not read by the sweep: it holds no frame after the pass, still past
// its deadline with no ending stored, and its reader ends it, since the stream ends a run past its
// ends_at with no terminal frame stored (streamGoingAnswer) with that same error, and stores it.
// A run inside its own window is left to run.
func TestTheGoingSweepEndsTheRunsOfItsWindowAndLeavesAnOlderOneToItsReader(t *testing.T) {
	routes := newGoingRoutesRig(t, nil)
	rig := goingRig{server: routes.server, database: routes.database}
	ctx := context.Background()
	insideBy, outsideBy := goingSweepWindow-time.Hour, goingSweepWindow+time.Hour
	inside := rig.record(t, "asked most of a day ago")
	rig.age(t, inside.ID, insideBy+time.Minute, insideBy)
	older := rig.record(t, "asked more than a day ago")
	rig.age(t, older.ID, outsideBy+time.Minute, outsideBy)
	live := rig.record(t, "asked just now")

	if err := sweepGoingRequests(ctx, rig.database.Pool); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	deadline := fmt.Sprintf(`1 %s {"code":%q}`, contracts.GoingEventError, contracts.GoingErrorAnswerDidNotArrive)
	if frames := rig.frames(t, inside.ID); len(frames) != 1 || frames[0] != deadline {
		t.Fatalf("run %s, past its deadline by %v, inside the sweep's window of %v, holds %v after the sweep, want the deadline's ending %s",
			inside.ID, insideBy, goingSweepWindow, frames, deadline)
	}
	_, state, err := rig.server.readGoingStream(ctx, older.ID, 0)
	if err != nil || state.Terminal != 0 || !state.PastDeadline {
		t.Fatalf("run %s, past its deadline by %v, before the sweep's window of %v, reads %+v (%v) after the sweep, want it past its deadline with no ending stored: the sweep reads no run older than its window",
			older.ID, outsideBy, goingSweepWindow, state, err)
	}
	if frames := rig.frames(t, older.ID); len(frames) != 0 {
		t.Fatalf("run %s, before the sweep's window, holds %v after the sweep, want no frame", older.ID, frames)
	}
	if frames := rig.frames(t, live.ID); len(frames) != 0 {
		t.Fatalf("the sweep ended a run still inside its own window: %v", frames)
	}

	response := routes.stream(t, older.ID, goingAsker.ID, "")
	want := fmt.Sprintf("id: 1\nevent: %s\ndata: {\"code\":\"%s\"}\n\n", contracts.GoingEventError, contracts.GoingErrorAnswerDidNotArrive)
	if response.Code != http.StatusOK || response.Body.String() != want {
		t.Fatalf("the stream of run %s, which the sweep left, answers %d %q, want 200 and the deadline's error %q",
			older.ID, response.Code, response.Body.String(), want)
	}
	if frames := rig.frames(t, older.ID); len(frames) != 1 || frames[0] != deadline {
		t.Fatalf("run %s holds %v once its reader read it, want the ending the reader wrote stored, %s", older.ID, frames, deadline)
	}
}

// TestTheOverdueReadsGoThroughTheEndsAtIndex holds each pass's read of the runs it ends to
// going_requests_ends_at (migration 20261116232634): under the plan Postgres builds for the values
// the pass sends, and under the generic plan a prepared statement settles on, it reads
// going_requests through that index alone, never a scan of the table, with its bounds in the
// index condition. The sweep's read (goingOverdueRuns) carries both bounds of its window - ends_at
// at or before now, and after now less the window - so it is that index's range for the last day
// and not every run ever kept: a condition holding the upper bound alone would read the index from
// its first entry, every run kept, which is the cost the window is there to bound. The boot pass's
// read (goingEveryOverdueRun) carries that upper bound alone, every run whose deadline has passed,
// which is why the pass runs once a boot and not once a maintenance pass.
func TestTheOverdueReadsGoThroughTheEndsAtIndex(t *testing.T) {
	rig := newGoingRig(t)
	ctx := context.Background()
	const index = "going_requests_ends_at"
	terminal := "'{" + strings.Join(goingTerminalNames, ",") + "}'"
	connection, err := rig.database.Pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire a connection for the generic plans: %v", err)
	}
	defer connection.Release()
	firstKeys := explaintest.NewIndexKeys(rig.database.Pool)
	for _, read := range []struct {
		name, prepared, statement string
		args                      []any
		types, literals           []string
		bounds                    []string
	}{
		{
			name: "the sweep's read", prepared: "going_overdue_runs", statement: goingOverdueRuns,
			args:  []any{goingTerminalNames, goingSweepWindow.Seconds()},
			types: []string{"text[]", "double precision"}, literals: []string{terminal, fmt.Sprint(goingSweepWindow.Seconds())},
			bounds: []string{"ends_at <=", "ends_at >"},
		},
		{
			name: "the boot pass's read", prepared: "going_every_overdue_run", statement: goingEveryOverdueRun,
			args: []any{goingTerminalNames}, types: []string{"text[]"}, literals: []string{terminal},
			bounds: []string{"ends_at <="},
		},
	} {
		custom := planWithoutSeqScans(t, rig.database, read.statement, read.args...)
		generic := explaintest.GenericPlan(t, ctx, connection, read.prepared, read.statement, read.types, read.literals,
			"set local enable_seqscan = off")
		for _, planned := range []struct {
			name string
			plan json.RawMessage
		}{{read.name, custom}, {read.name + " under a generic plan", generic}} {
			if explaintest.SeqScansRelation(t, planned.plan, "going_requests") {
				t.Errorf("%s scans going_requests:\n%s", planned.name, planned.plan)
			}
			firstKeys.RequireIndexKeyed(t, planned.plan, index, planned.name, explaintest.CondRequired)
			for _, scan := range explaintest.IndexScans(t, planned.plan) {
				for _, bound := range read.bounds {
					if scan.Index == index && !strings.Contains(scan.Cond, bound) {
						t.Errorf("%s reads %s on `%s`, want its bound `%s` in the index condition:\n%s",
							planned.name, scan, scan.Cond, bound, planned.plan)
					}
				}
			}
		}
	}
}

// TestTheNextFrameReadsTwoIndexProbes holds the read every frame of a run makes once it holds the
// run's node (goingNextFrame) to two probes of an index each, under the plan Postgres builds for
// the values a frame sends and under the generic plan a prepared statement settles on: the run's
// newest sequence, read on the (request_id, sequence) key, and whether it has stored its ending,
// read on going_events_terminal, each keyed by the run. Neither reads going_events by a scan of the
// table, and no aggregate runs over the run's frames, so a frame costs the same however many frames
// its run already holds.
func TestTheNextFrameReadsTwoIndexProbes(t *testing.T) {
	rig := newGoingRig(t)
	ctx := context.Background()
	run := rig.record(t, "how many frames does this read?")
	custom := planWithoutSeqScans(t, rig.database, goingNextFrame, run.ID, contracts.GoingEventText, []byte(`{"text":"a"}`))
	connection, err := rig.database.Pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire a connection for the generic plan: %v", err)
	}
	defer connection.Release()
	generic := explaintest.GenericPlan(t, ctx, connection, "going_next_frame", goingNextFrame,
		[]string{"text", "text", "jsonb"}, []string{"'" + run.ID + "'", "'" + contracts.GoingEventText + "'", `'{"text":"a"}'`},
		"set local enable_seqscan = off")
	for _, planned := range []struct {
		name string
		plan json.RawMessage
	}{{"the next frame's read", custom}, {"the next frame's read under a generic plan", generic}} {
		for _, relation := range []string{"going_events", "going_requests"} {
			if explaintest.SeqScansRelation(t, planned.plan, relation) {
				t.Errorf("%s scans %s:\n%s", planned.name, relation, planned.plan)
			}
		}
		probed := map[string]bool{}
		for _, scan := range explaintest.IndexScans(t, planned.plan) {
			if scan.Relation == "going_events" && strings.Contains(scan.Cond, "request_id") {
				probed[scan.Index] = true
			}
		}
		for _, index := range []string{"going_events_request_id_sequence_key", "going_events_terminal"} {
			if !probed[index] {
				t.Errorf("%s reads no probe of %s keyed by the run:\n%s", planned.name, index, planned.plan)
			}
		}
		for _, node := range explaintest.Nodes(t, planned.plan) {
			if node.Type == "Aggregate" {
				t.Errorf("%s runs %s over the run's frames:\n%s", planned.name, node, planned.plan)
			}
		}
	}
}

// TestABootEndsARunOverdueByMoreThanADay is the boot pass: a run whose ends_at passed more than a
// day ago with no terminal frame stored - one whose window passed while every process was down for
// longer than goingSweepWindow - is read by no periodic pass,
// and the boot pass ends it with the deadline's error, once. The run holds one frame of its own
// and is dated an hour past the window. A periodic pass (sweepGoingRequests, which each tick of
// RunMaintenanceSweep calls) leaves it holding that frame alone; the boot pass
// (EndGoingRunsOverdueAtBoot, the function cmd/dispatch calls once at boot before that loop's
// first tick) stores the deadline's error after it and logs that it ended one run, with how long
// it took; and a second boot pass, as a second process booting runs it, stores nothing more and
// logs that it ended none.
func TestABootEndsARunOverdueByMoreThanADay(t *testing.T) {
	rig := newGoingRig(t)
	ctx := context.Background()
	logs := captureLogs(t)
	overdueBy := goingSweepWindow + time.Hour
	run := rig.record(t, "asked before every process went down")
	if _, err := appendGoingEvent(ctx, rig.database.Pool, run.ID, going.Text("Half an ")); err != nil {
		t.Fatalf("append the run's own frame: %v", err)
	}
	rig.age(t, run.ID, overdueBy+time.Minute, overdueBy)
	held := rig.frames(t, run.ID)
	if len(held) != 1 {
		t.Fatalf("the run holds %v before any pass, want its one frame", held)
	}
	// bootLines is every line the boot pass logged so far, in order.
	bootLines := func() []string {
		var lines []string
		for line := range strings.Lines(logs.String()) {
			if strings.Contains(line, `msg="dispatch: the boot pass ended the overdue answer runs"`) {
				lines = append(lines, line)
			}
		}
		return lines
	}

	if err := sweepGoingRequests(ctx, rig.database.Pool); err != nil {
		t.Fatalf("periodic pass: %v", err)
	}
	if frames := rig.frames(t, run.ID); !equalStrings(frames, held) {
		t.Fatalf("run %s, past its deadline by %v, holds %v after a periodic pass, want its own frame %v alone: the sweep reads no run older than its window of %v",
			run.ID, overdueBy, frames, held, goingSweepWindow)
	}

	EndGoingRunsOverdueAtBoot(ctx, rig.database.Pool)
	deadline := fmt.Sprintf(`2 %s {"code":%q}`, contracts.GoingEventError, contracts.GoingErrorAnswerDidNotArrive)
	ended := rig.frames(t, run.ID)
	if len(ended) != 2 || ended[0] != held[0] || ended[1] != deadline {
		t.Fatalf("run %s holds %v after the boot pass, want its own frame %s and the deadline's ending %s", run.ID, ended, held[0], deadline)
	}
	if _, state, err := rig.server.readGoingStream(ctx, run.ID, 0); err != nil || state.Terminal != 2 || !state.PastDeadline {
		t.Fatalf("run %s reads %+v (%v) after the boot pass, want its ending at sequence 2 and past its deadline", run.ID, state, err)
	}
	if lines := bootLines(); len(lines) != 1 || !strings.Contains(lines[0], " ended=1 ") || !strings.Contains(lines[0], " duration=") {
		t.Fatalf("the boot pass logged %q, want one line saying it ended 1 run and how long it took", lines)
	}

	EndGoingRunsOverdueAtBoot(ctx, rig.database.Pool)
	if frames := rig.frames(t, run.ID); !equalStrings(frames, ended) {
		t.Fatalf("a second boot pass left run %s holding %v, want the one ending %v", run.ID, frames, ended)
	}
	if lines := bootLines(); len(lines) != 2 || !strings.Contains(lines[1], " ended=0 ") {
		t.Fatalf("the two boot passes logged %q, want the second saying it ended 0 runs", lines)
	}
}

// The maintenance pass sweeps the runs beside the keys and the owed deliveries, so a run nobody
// reads is ended with no loop of its own.
func TestTheMaintenancePassEndsAGoingRunNobodyReads(t *testing.T) {
	rig := newGoingRig(t)
	abandoned := rig.record(t, "nobody is answering this")
	rig.age(t, abandoned.ID, time.Minute, time.Second)
	leases := sweepLeases(t, rig.database)
	runSweep(t, func(ctx context.Context) {
		RunMaintenanceSweep(ctx, rig.database.Pool, maintenanceTestInterval, leases, nil)
	})
	waitUntil(t, 30*time.Second, "the abandoned run ended by a maintenance pass", func() bool {
		_, state, err := rig.server.readGoingStream(context.Background(), abandoned.ID, 0)
		return err == nil && state.Terminal > 0
	})
	deadline := fmt.Sprintf(`1 %s {"code":%q}`, contracts.GoingEventError, contracts.GoingErrorAnswerDidNotArrive)
	if frames := rig.frames(t, abandoned.ID); len(frames) != 1 || frames[0] != deadline {
		t.Fatalf("the maintenance pass left %v, want %s", frames, deadline)
	}
}

// Another pod's delete - an erase, or during a roll a process of the release before deleting a run
// an hour old - may remove a run between the sweep's read of the overdue ids and its write of
// that run's ending, and the run's interaction node still stands then. The sweep ends each run in
// a transaction of its own, so that write stores nothing - no frame value and no row
// (errGoingRunGone) - and the pass goes on: the run read after it keeps its ending, and a run two
// hours old is ended and kept, as this build deletes no run. The delete here is held uncommitted
// until the sweep's write of that run waits on the run's row, which the write takes before it
// reads the next sequence (goingNextFrame), so the run is gone exactly between the read and the
// write. A frame of a run nobody recorded stores nothing either, and writes no node for it.
func TestAGoingTerminalWriteForAMissingRunFailsNoSweep(t *testing.T) {
	rig := newGoingRig(t)
	ctx := context.Background()
	stale := rig.record(t, "asked an age ago")
	rig.age(t, stale.ID, 2*time.Hour+time.Minute, 2*time.Hour)
	deleted := rig.record(t, "and this one, which is gone by the time it is ended")
	rig.age(t, deleted.ID, 2*time.Minute, 2*time.Second)
	overdue := rig.record(t, "who ends this one?")
	rig.age(t, overdue.ID, time.Minute, time.Second)

	deleting, err := rig.database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the delete of the run under the sweep: %v", err)
	}
	defer deleting.Rollback(ctx)
	if _, err := deleting.Exec(ctx, `delete from going_requests where id = $1::uuid`, deleted.ID); err != nil {
		t.Fatalf("delete the run under the sweep: %v", err)
	}
	var holder uint32
	if err := deleting.QueryRow(ctx, `select pg_backend_pid()`).Scan(&holder); err != nil {
		t.Fatalf("read the deleting backend: %v", err)
	}
	swept := make(chan error, 1)
	go func() { swept <- sweepGoingRequests(ctx, rig.database.Pool) }()
	waitUntil(t, 30*time.Second, "the sweep's write of the deleted run waiting on its row", func() bool {
		var waiting bool
		if err := rig.database.Pool.QueryRow(ctx, `
			select exists (select 1 from pg_stat_activity where $1::integer = any(pg_blocking_pids(pid)))
		`, int64(holder)).Scan(&waiting); err != nil {
			t.Fatalf("read what waits on the delete: %v", err)
		}
		return waiting
	})
	if err := deleting.Commit(ctx); err != nil {
		t.Fatalf("commit the delete: %v", err)
	}
	select {
	case err := <-swept:
		if err != nil {
			t.Fatalf("the sweep failed over one run deleted under it: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the sweep did not finish within 30s of the delete committing")
	}

	ending := fmt.Sprintf(`1 %s {"code":%q}`, contracts.GoingEventError, contracts.GoingErrorAnswerDidNotArrive)
	if frames := rig.frames(t, overdue.ID); len(frames) != 1 || frames[0] != ending {
		t.Fatalf("the run read after the deleted one holds %v, want %s", frames, ending)
	}
	requireNoFrameOf(t, rig.database, deleted.ID, "the run deleted under the sweep")
	if _, held, err := graphstore.ReadNode(ctx, rig.database.Pool, graphstore.KindInteraction, deleted.ID); err != nil || !held {
		t.Fatalf("the deleted run's interaction node reads held %t (%v), want it standing: the delete removes the row alone", held, err)
	}
	if frames := rig.frames(t, stale.ID); len(frames) != 1 || frames[0] != ending {
		t.Fatalf("the run two hours old holds %v, want it kept and ended %s: this build deletes no run", frames, ending)
	}

	// A run nobody recorded: neither path stores a frame, and neither writes it a node.
	const nobodys = "00000000-0000-4000-8000-00000000000b"
	if _, err := appendGoingEvent(ctx, rig.database.Pool, nobodys, going.Text("to nobody")); !errors.Is(err, errGoingRunGone) {
		t.Fatalf("a frame of a run nobody recorded: %v, want %v", err, errGoingRunGone)
	}
	if _, err := endGoingRun(ctx, rig.database.Pool, nobodys, going.Fault(contracts.GoingErrorAnswerDidNotArrive)); !errors.Is(err, errGoingRunGone) {
		t.Fatalf("ending a run nobody recorded: %v, want %v", err, errGoingRunGone)
	}
	if _, held, err := graphstore.ReadNode(ctx, rig.database.Pool, graphstore.KindInteraction, nobodys); err != nil || held {
		t.Fatalf("a run nobody recorded reads a node held %t (%v), want none", held, err)
	}
	requireNoFrameOf(t, rig.database, nobodys, "a run nobody recorded")
}

// requireNoFrameOf fails unless the run holds no going_events row and its interaction node no
// frame value.
func requireNoFrameOf(t *testing.T, database *store.Store, run, what string) {
	t.Helper()
	var rows, values int
	if err := database.Pool.QueryRow(context.Background(), `
		select (select count(*) from going_events where request_id = $1::uuid),
		       (select count(*) from graph_tuples where from_kind = $2 and from_id = $3 and kind = $4)
	`, run, graphstore.KindInteraction, run, graphstore.AttributeFrame).Scan(&rows, &values); err != nil {
		t.Fatalf("count the frames of %s: %v", what, err)
	}
	if rows != 0 || values != 0 {
		t.Fatalf("%s stored %d going_events rows and %d frame values, want none", what, rows, values)
	}
}

// jsonEqual reports whether got and want are the same JSON value, whatever order their keys were
// written in.
func jsonEqual(t *testing.T, got json.RawMessage, want string) bool {
	t.Helper()
	var a, b any
	if err := json.Unmarshal(got, &a); err != nil {
		t.Fatalf("payload %q is not JSON: %v", got, err)
	}
	if err := json.Unmarshal([]byte(want), &b); err != nil {
		t.Fatalf("want %q is not JSON: %v", want, err)
	}
	gotJSON, _ := json.Marshal(a)
	wantJSON, _ := json.Marshal(b)
	return string(gotJSON) == string(wantJSON)
}
