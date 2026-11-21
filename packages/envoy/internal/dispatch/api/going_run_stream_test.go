package api

// going_run_stream_test.go holds a run's life as History and its stream meet it: a conversation is
// deleted whole once its newest run is seven days old, and a stream of a run's events resumes from
// a reader's last sequence and refuses a reader past its bound.

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/going/agentwire"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// conversationRun records an ended run of login's, started age ago, following continues in its
// conversation, or the first of a conversation of its own where continues is empty, with a step,
// an event, a grant, a plan with one row and a landing of its own, and answers its id.
func (rig goingAgentRig) conversationRun(t *testing.T, login, words, continues string, age time.Duration) string {
	t.Helper()
	id := uuid.NewString()
	var follows *string
	if continues != "" {
		follows = &continues
	}
	rig.exec(t, `
		insert into going_runs (id, author_login, words, conversation_id, continues, level, mode, state, shape, created_at, finished_at)
		values ($1::uuid, $2, $3, coalesce((select coalesce(conversation_id, id) from going_runs where id = $4::uuid), $1::uuid), $4::uuid,
		  'standard', 'ask', 'done', $5, now() - $6::interval, now() - $6::interval)
	`, id, login, words, follows, goingRunShape, age.String())
	rig.exec(t, `insert into going_run_steps (run_id, step, kind, body, note) values ($1::uuid, 1, 'model', '{}'::jsonb, 'a step')`, id)
	rig.exec(t, `insert into going_run_events (run_id, sequence, name, data) values ($1::uuid, 1, 'text', '{}'::jsonb)`, id)
	rig.exec(t, `insert into going_run_grants (run_id, kind, login) values ($1::uuid, 'header', $2)`, id, login)
	rig.exec(t, `insert into going_plans (run_id, plan, state) values ($1::uuid, 1, 'done')`, id)
	rig.exec(t, `insert into going_plan_rows (run_id, plan, row, change, state) values ($1::uuid, 1, 0, '{}'::jsonb, 'done')`, id)
	rig.exec(t, `
		insert into going_landings (run_id, phase, step, call, request, method, path, body_digest, answer)
		values ($1::uuid, 'do', 1, 1, 1, 'POST', '/api/v1/issues/DEMO-1/comments', 'digest', '{}'::jsonb)
	`, id)
	return id
}

// goingRunHeld counts what the store holds of run: its row, and its steps, events, grants, plans,
// plan rows and landings together.
func (rig goingAgentRig) goingRunHeld(t *testing.T, run string) (row, below int) {
	t.Helper()
	row = rig.count(t, `select count(*) from going_runs where id = $1::uuid`, run)
	below = rig.count(t, `
		select (select count(*) from going_run_steps where run_id = $1::uuid) + (select count(*) from going_run_events where run_id = $1::uuid)
		  + (select count(*) from going_run_grants where run_id = $1::uuid) + (select count(*) from going_plans where run_id = $1::uuid)
		  + (select count(*) from going_plan_rows where run_id = $1::uuid) + (select count(*) from going_landings where run_id = $1::uuid)
	`, run)
	return row, below
}

// TestAConversationIsDeletedWhole: the sweep deletes a conversation whole, every run of it with
// its steps, events, grant, plans and landings, once its newest run is seven days old; and a
// follow-up's earlier run is kept, with everything below it, while the conversation's newest run
// is younger than seven days, so History lists the conversation whole and the earlier run reads.
func TestAConversationIsDeletedWhole(t *testing.T) {
	rig := newGoingAgentRig(t, nil)
	rig.served.deps.Identity = headerIdentity(rig.database)
	const day = 24 * time.Hour
	keptFirst := rig.conversationRun(t, "alice", "asked eight days ago", "", 8*day)
	keptNext := rig.conversationRun(t, "alice", "followed up yesterday", keptFirst, day)
	alone := rig.conversationRun(t, "alice", "asked eight days ago, never followed up", "", 8*day)
	oldFirst := rig.conversationRun(t, "alice", "asked nine days ago", "", 9*day)
	oldNext := rig.conversationRun(t, "alice", "followed up eight days ago", oldFirst, 8*day)

	if err := sweepGoingRuns(context.Background(), rig.database.Pool); err != nil {
		t.Fatalf("sweep: %v", err)
	}

	for name, run := range map[string]string{"the earlier run": keptFirst, "the follow-up": keptNext} {
		if row, below := rig.goingRunHeld(t, run); row != 1 || below != 6 {
			t.Errorf("%s of the conversation whose newest run is a day old holds %d rows and %d below them, want 1 and 6", name, row, below)
		}
	}
	if response := dispatchRequest(t, rig.mux, http.MethodGet, "/api/v1/going/runs/"+keptFirst, nil, "alice"); response.Code != http.StatusOK {
		t.Errorf("GET the earlier run 8 days old answered %d %s, want 200 while its conversation is listed", response.Code, response.Body.String())
	}
	page := goingHistory(t, rig.mux, "alice", "")
	if len(page.Rows) != 1 || page.Rows[0].ID != keptFirst || len(page.Rows[0].Runs) != 2 ||
		page.Rows[0].Runs[0].ID != keptNext || page.Rows[0].Runs[1].ID != keptFirst {
		t.Errorf("History lists %+v, want the one conversation %s with its follow-up and its earlier run", page.Rows, keptFirst)
	}
	for name, run := range map[string]string{
		"the run of a conversation of its own": alone, "the earlier run of the old conversation": oldFirst, "its follow-up": oldNext,
	} {
		if row, below := rig.goingRunHeld(t, run); row != 0 || below != 0 {
			t.Errorf("%s, 8 days old and newest of its conversation, holds %d rows and %d below them, want none", name, row, below)
		}
	}
}

// goingStreamFrame is one frame a stream of a run's events wrote: its id, its name and its data.
type goingStreamFrame struct {
	id, name, data string
}

// goingRunStream is one open stream of a run's events: its frames as the server writes them,
// closed when the stream ends, and close, which ends it from the reader's side.
type goingRunStream struct {
	frames <-chan goingStreamFrame
	close  func()
}

// openGoingRunStream opens GET /api/v1/going/runs/{run}/events on server as login, resuming after
// last where it is set, and answers the status with the stream, or the refusal's code.
func openGoingRunStream(t *testing.T, server *httptest.Server, run, login, last string) (int, string, goingRunStream) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/api/v1/going/runs/"+run+"/events", nil)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	request.Header.Set("X-Dispatch-User", login)
	if last != "" {
		request.Header.Set("Last-Event-ID", last)
	}
	response, err := server.Client().Do(request)
	if err != nil {
		cancel()
		t.Fatalf("open the stream of %s: %v", run, err)
	}
	if response.StatusCode != http.StatusOK {
		var refusal refusalAnswer
		_ = json.NewDecoder(response.Body).Decode(&refusal)
		_ = response.Body.Close()
		cancel()
		return response.StatusCode, refusal.Code, goingRunStream{}
	}
	frames := make(chan goingStreamFrame, 64)
	go func() {
		defer close(frames)
		defer response.Body.Close()
		reader := bufio.NewReader(response.Body)
		var frame goingStreamFrame
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				return
			}
			if frame.take(strings.TrimSuffix(line, "\n")) {
				if frame.name != "" {
					frames <- frame
				}
				frame = goingStreamFrame{}
			}
		}
	}()
	t.Cleanup(cancel)
	return http.StatusOK, "", goingRunStream{frames: frames, close: cancel}
}

// take reads one line of a stream's frame into f, and reports whether the line ends the frame: the
// empty line after it.
func (f *goingStreamFrame) take(line string) bool {
	switch {
	case line == "":
		return true
	case strings.HasPrefix(line, "id: "):
		f.id = strings.TrimPrefix(line, "id: ")
	case strings.HasPrefix(line, "event: "):
		f.name = strings.TrimPrefix(line, "event: ")
	case strings.HasPrefix(line, "data: "):
		f.data = strings.TrimPrefix(line, "data: ")
	}
	return false
}

// next is the stream's next frame, which must come within within.
func (s goingRunStream) next(t *testing.T, within time.Duration, what string) goingStreamFrame {
	t.Helper()
	select {
	case frame, open := <-s.frames:
		if !open {
			t.Fatalf("the stream ended before %s", what)
		}
		return frame
	case <-time.After(within):
		t.Fatalf("%s did not come within %s", what, within)
		return goingStreamFrame{}
	}
}

// TestTheRunStreamResumes: a stream of a run's events writes each frame the run stores, as it
// stores it, woken by the run's NOTIFY well before its own re-read, whatever form of the run's id
// the writer named it by; a reader that drops and comes back with its Last-Event-ID is written the
// frames after it and none before; past 4 streams of one run the next is refused 429
// GOING_RUN_READERS_FULL, and once one closes another is admitted; and the run's Stop ends every
// stream with the run's end frame.
func TestTheRunStreamResumes(t *testing.T) {
	rig := newGoingAgentRig(t, nil)
	rig.served.deps.Identity = headerIdentity(rig.database)
	server := httptest.NewServer(rig.mux)
	t.Cleanup(server.Close)
	// At a shape no build reads, so nothing claims it while the streams read it.
	run := rig.queued(t, "alice", goingRunShape+1, "a run the streams read")
	stored := 0
	// store appends count frames to the run its writer names id.
	store := func(t *testing.T, id string, count int) {
		t.Helper()
		ctx := context.Background()
		tx, err := rig.database.Pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		frames := make([]agentwire.Frame, 0, count)
		for range count {
			stored++
			frames = append(frames, agentwire.Frame{Name: "text", Data: json.RawMessage(fmt.Sprintf(`{"n": %d}`, stored))})
		}
		if err := appendGoingRunFrames(ctx, tx, id, frames); err != nil {
			t.Fatalf("store frames: %v", err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}
	read := func(t *testing.T, stream goingRunStream, within time.Duration, ids ...string) {
		t.Helper()
		for _, id := range ids {
			if frame := stream.next(t, within, "frame "+id); frame.id != id || frame.name != "text" {
				t.Fatalf("the stream wrote the frame %+v, want frame %s", frame, id)
			}
		}
	}

	t.Run("a reader resumes from its last sequence", func(t *testing.T) {
		store(t, run, 3)
		status, code, first := openGoingRunStream(t, server, run, "alice", "")
		if status != http.StatusOK {
			t.Fatalf("the stream answered %d %s", status, code)
		}
		read(t, first, 5*time.Second, "1", "2", "3")
		store(t, run, 1)
		// Woken by the run's NOTIFY, well before the stream would read again of itself.
		read(t, first, goingRunStreamCheck/3, "4")
		first.close()
		store(t, run, 2)
		status, code, again := openGoingRunStream(t, server, run, "alice", "4")
		if status != http.StatusOK {
			t.Fatalf("the stream resumed after 4 answered %d %s", status, code)
		}
		read(t, again, 5*time.Second, "5", "6")
		again.close()
	})

	t.Run("a writer that names the run in capitals wakes its stream", func(t *testing.T) {
		status, code, stream := openGoingRunStream(t, server, run, "alice", fmt.Sprint(stored))
		if status != http.StatusOK {
			t.Fatalf("the stream answered %d %s", status, code)
		}
		// Stored once the stream waits for the run's next frame, so only a NOTIFY wakes it.
		goingEventually(t, "the stream to wait for the run's next frame", func() bool {
			rig.served.goingRunEvents.mu.Lock()
			defer rig.served.goingRunEvents.mu.Unlock()
			return rig.served.goingRunEvents.runs[run] != nil
		})
		store(t, strings.ToUpper(run), 1)
		// Woken by the run's NOTIFY, which names the run as its stream waits on it.
		read(t, stream, goingRunStreamCheck/3, fmt.Sprint(stored))
		stream.close()
	})

	t.Run("the fifth reader of a run is refused", func(t *testing.T) {
		goingEventually(t, "the streams of the row before to close", func() bool {
			rig.served.goingReaders.mu.Lock()
			defer rig.served.goingReaders.mu.Unlock()
			return rig.served.goingReaders.byRun[run] == 0
		})
		var open []goingRunStream
		for reader := 1; reader <= goingReadersPerRun; reader++ {
			status, code, stream := openGoingRunStream(t, server, run, "alice", fmt.Sprint(stored))
			if status != http.StatusOK {
				t.Fatalf("reader %d of the run answered %d %s, want 200", reader, status, code)
			}
			open = append(open, stream)
		}
		if status, code, _ := openGoingRunStream(t, server, run, "alice", ""); status != http.StatusTooManyRequests || code != codeGoingRunReadersFull {
			t.Fatalf("the fifth reader of the run answered %d %s, want 429 %s", status, code, codeGoingRunReadersFull)
		}
		open[0].close()
		goingEventually(t, "a reader admitted once one closed", func() bool {
			status, _, stream := openGoingRunStream(t, server, run, "alice", fmt.Sprint(stored))
			if status == http.StatusOK {
				open[0] = stream
				return true
			}
			return false
		})
		if response := dispatchRequest(t, rig.mux, http.MethodPost, "/api/v1/going/runs/"+run+"/stop", nil, "alice"); response.Code != http.StatusOK {
			t.Fatalf("stop the run answered %d %s", response.Code, response.Body.String())
		}
		for index, stream := range open {
			frame := stream.next(t, goingRunStreamCheck/3, fmt.Sprintf("reader %d's end frame", index+1))
			var end agentwire.EndFrame
			if frame.name != agentwire.FrameEnd || json.Unmarshal([]byte(frame.data), &end) != nil || end.State != agentwire.StateStopped || end.Reason != goingStoppedByPerson {
				t.Errorf("reader %d was written %+v after the stop, want the end frame of the stopped run", index+1, frame)
			}
			select {
			case _, more := <-stream.frames:
				if more {
					t.Errorf("reader %d's stream went on past the run's end", index+1)
				}
			case <-time.After(goingRunStreamCheck / 3):
				t.Errorf("reader %d's stream did not end within %s of the run's end frame", index+1, goingRunStreamCheck/3)
			}
		}
	})
}

// TestTheInboxReadsAPageOfWaitingPlansInTwoRoundTrips: GET /api/v1/me/going-plans reads a page of
// the person's waiting plans, each with its version, in two round trips however many plans the
// page holds: the list, and every listed plan's rows in one read.
func TestTheInboxReadsAPageOfWaitingPlansInTwoRoundTrips(t *testing.T) {
	database := storetest.Open(t)
	counter := &roundTrips{}
	traced := &store.Store{Pool: storetest.Traced(t, database.Pool, counter, nil)}
	// The caller is named on the untraced pool, so the count is the read's own statements.
	handler, _, _ := newTestServer(t, testServerOptions{store: traced, identity: headerIdentity(database)})
	waiting := func(t *testing.T, rows int) {
		t.Helper()
		id := uuid.NewString()
		for _, write := range []struct {
			statement string
			arguments []any
		}{
			{`insert into going_runs (id, author_login, words, conversation_id, level, mode, state, shape, created_at)
			  values ($1::uuid, 'alice', 'a plan to approve', $1::uuid, 'standard', 'ask', 'waiting', $2, now())`, []any{id, goingRunShape}},
			{`insert into going_plans (run_id, plan, state, summary) values ($1::uuid, 1, 'waiting', 'The changes the words ask for.')`, []any{id}},
			{`insert into going_plan_rows (run_id, plan, row, change, state)
			  select $1::uuid, 1, n, jsonb_build_object('tool', 'dispatch_issue_update', 'arguments', jsonb_build_object('issue', 'DEMO-' || n)), 'waiting'
			  from generate_series(0, $2::int - 1) n`, []any{id, rows}},
		} {
			if _, err := database.Pool.Exec(context.Background(), write.statement, write.arguments...); err != nil {
				t.Fatalf("record a waiting plan: %v", err)
			}
		}
	}
	read := func(t *testing.T) (goingWaitingPlansPage, []string) {
		t.Helper()
		ctx := context.WithValue(context.Background(), countedRead{}, true)
		request := httptest.NewRequestWithContext(ctx, http.MethodGet, "/api/v1/me/going-plans", nil)
		request.Header.Set("X-Dispatch-User", "alice")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		trips := counter.take()
		if response.Code != http.StatusOK {
			t.Fatalf("GET /api/v1/me/going-plans answered %d %s", response.Code, response.Body.String())
		}
		return decodeBody[goingWaitingPlansPage](t, response), trips
	}
	waiting(t, 2)
	one, oneTrips := read(t)
	waiting(t, 1)
	waiting(t, 3)
	three, threeTrips := read(t)
	if len(one.Rows) != 1 || len(three.Rows) != 3 {
		t.Fatalf("the Inbox lists %d and then %d waiting plans, want 1 and then 3", len(one.Rows), len(three.Rows))
	}
	for _, row := range three.Rows {
		if row.Version == "" {
			t.Fatalf("the Inbox lists run %s's plan with no version", row.Run)
		}
	}
	if len(oneTrips) != 2 || len(threeTrips) != 2 {
		t.Fatalf("a page of one plan made %d round trips and one of three %d, want 2 each: %v, then %v",
			len(oneTrips), len(threeTrips), oneTrips, threeTrips)
	}
}
