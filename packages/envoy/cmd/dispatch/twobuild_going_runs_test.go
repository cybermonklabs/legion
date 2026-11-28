//go:build twobuild

// The command-bar half of the two-build suite: a run of this tree's command bar outlives the
// maintenance sweep of a release that sweeps the bar's earlier record, the going requests, while
// that release and this tree serve one database (check c25). The release is a fixed one,
// goingReleaseCommit, and not the previous release the rest of the suite runs beside this tree
// (twobuild_harness_test.go holds the harness the builds start with).

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/going/agent/agenttest"
	"github.com/sjawhar/envoy/going/agentwire"
	"github.com/sjawhar/envoy/internal/dispatch/api"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// goingReleaseBuildVariable names the directory build-dispatch-build.sh wrote for the release
// goingReleaseCommit, which TestARunSurvivesTheReleaseBeforeIt runs beside this tree.
const goingReleaseBuildVariable = "DISPATCH_GOING_RELEASE_BUILD"

// goingReleaseCommit is the commit of the release tagged legion-envoy-v32.15.0. Its maintenance
// sweep (sweepGoingRequests in its internal/dispatch/api/going.go) ends each going request past
// its answer window with an error frame, and deletes each going request older than an hour with
// its frames. The row runs this release, and not the newest one, because
// cybermonklabs/legion#4193 removes that delete, and a release without it could not delete a
// run's record whatever this tree keys it to. The tag is lightweight, and a lightweight tag can be
// moved, so the pin is the commit.
const goingReleaseCommit = "6c46f6bb7dd6a1ac9b6235fb3bce990ec82eae86"

// What goingReleaseCommit's sweep reads a going request by (GOING_RUN_MS and GOING_REQUEST_TTL_MS
// in its packages/contracts/src/going.ts): it ends a request releaseAnswerWindow after its start
// with an error frame, and deletes one older than releaseRequestTTL. runAge is how far back the
// row dates the head's runs: past that hour.
const (
	releaseAnswerWindow = 30 * time.Second
	releaseRequestTTL   = time.Hour
	runAge              = 2 * releaseRequestTTL
)

// releaseErrorFrame is the name of the frame goingReleaseCommit's sweep ends an overdue going
// request with.
const releaseErrorFrame = "error"

// The maintenance sweep's period in each build (DISPATCH_TEST_MAINTENANCE_SWEEP_INTERVAL). The
// release sweeps each second. This tree's first pass comes an hour after its start, so in this
// row only the release sweeps: this tree's sweep deletes old going requests too, and a pass of it
// would leave no way to tell which build swept.
const (
	releaseSweepInterval = time.Second
	headSweepInterval    = time.Hour
)

// unknownTool is the tool a run's saved steps name in the last row: a tool no build of Dispatch
// has, as a tool an earlier build had and this one dropped would be.
const unknownTool = "a_tool_no_build_of_dispatch_knows"

// openGoingReleaseBuild reads DISPATCH_GOING_RELEASE_BUILD (openBuild), and fails the test when the
// build's COMMIT is another commit than goingReleaseCommit, before any server starts.
func openGoingReleaseBuild(t *testing.T) previousBuild {
	t.Helper()
	buildArguments := "--commit " + goingReleaseCommit
	build := openBuild(t, goingReleaseBuildVariable, buildArguments)
	if build.commit != goingReleaseCommit {
		t.Fatalf("%s=%s is a build of commit %s, but the release this row runs is commit %s (goingReleaseCommit). Build it again with packages/envoy/scripts/build-dispatch-build.sh %s --out <dir>.",
			goingReleaseBuildVariable, build.dir, build.commit, goingReleaseCommit, buildArguments)
	}
	return build
}

// startGoingRun starts a run of words on server as the test's person, as the bar's Enter does
// (POST /api/v1/going/runs), and answers its id.
func startGoingRun(t *testing.T, server *dispatchProcess, words string) string {
	t.Helper()
	created := server.call(t, http.MethodPost, "/api/v1/going/runs", jsonBody(t, map[string]string{"words": words}), http.StatusCreated)
	var run struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(created, &run); err != nil || run.ID == "" {
		t.Fatalf("POST /api/v1/going/runs answered %s (%v), want the run's id", created, err)
	}
	return run.ID
}

// runFrame is one frame of a run's event stream: its name and its data.
type runFrame struct {
	name, data string
}

// frameNames is the name of each of frames, in order.
func frameNames(frames []runFrame) []string {
	names := make([]string, 0, len(frames))
	for _, frame := range frames {
		names = append(names, frame.name)
	}
	return names
}

// streamedFrames reads run's frames from the start of its event stream on server
// (GET /api/v1/going/runs/{id}/events) as its person does, until the stream ends, until want
// frames have arrived where want is above 0, or until within has passed. The stream of a run that
// has ended ends once it has written every frame; that of a run that waits does not.
func streamedFrames(t *testing.T, server *dispatchProcess, run string, want int, within time.Duration) []runFrame {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), within)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, server.url("/api/v1/going/runs/"+run+"/events"), nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("X-Dispatch-User", dispatchTestLogin)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("open the event stream of run %s: %v", run, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("the event stream of run %s answered %d %s, want 200", run, response.StatusCode, body)
	}
	var frames []runFrame
	var frame runFrame
	scanner := bufio.NewScanner(response.Body)
	scanner.Buffer(make([]byte, 0, 64<<10), 4<<20)
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case line == "":
			if frame.name != "" {
				frames = append(frames, frame)
				if want > 0 && len(frames) >= want {
					return frames
				}
			}
			frame = runFrame{}
		case strings.HasPrefix(line, "event: "):
			frame.name = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			frame.data = strings.TrimPrefix(line, "data: ")
		}
	}
	return frames
}

// storedFrameNames is the name of each frame the database holds of run, in sequence.
func storedFrameNames(t *testing.T, database *store.Store, run string) []string {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `select name from going_run_events where run_id = $1::uuid order by sequence`, run)
	if err != nil {
		t.Fatalf("read run %s's frames: %v", run, err)
	}
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read run %s's frames: %v", run, err)
	}
	return names
}

// registeredGoingRunShape waits for the one process of the registry that registered a run shape
// (server_processes.going_run_shape), which is the head's, since the release writes none, and
// answers that shape.
func registeredGoingRunShape(t *testing.T, head agentServer) int {
	t.Helper()
	var shapes []int
	head.eventually(t, 30*time.Second, "the head's run shape in the registry", func() bool {
		rows, err := head.database.Pool.Query(context.Background(), `select going_run_shape from server_processes where going_run_shape is not null`)
		if err != nil {
			t.Fatalf("read the registered run shapes: %v", err)
		}
		if shapes, err = pgx.CollectRows(rows, pgx.RowTo[int]); err != nil {
			t.Fatalf("read the registered run shapes: %v", err)
		}
		return len(shapes) > 0
	})
	if len(shapes) != 1 {
		t.Fatalf("the registry holds the run shapes %v, want the head's alone", shapes)
	}
	return shapes[0]
}

// agedTable is a table and its timestamptz columns.
type agedTable struct {
	name    string
	columns []string
}

// ageGoingTables moves back by age every time the going tables hold, each timestamptz column of
// each table whose name begins going_, in one transaction, as if every row of them had been
// written age earlier: the runs and their steps, events, grants, landings and plans, and the going
// requests and their frames. So the row dates a run back with every record of it, whichever table a
// build keeps that record in.
func ageGoingTables(t *testing.T, database *store.Store, age time.Duration) {
	t.Helper()
	ctx := context.Background()
	rows, err := database.Pool.Query(ctx, `
		select c.table_name::text, array_agg(c.column_name::text order by c.ordinal_position)
		from information_schema.columns c
		join information_schema.tables b on b.table_schema = c.table_schema and b.table_name = c.table_name and b.table_type = 'BASE TABLE'
		where c.table_schema = current_schema() and left(c.table_name::text, 6) = 'going_' and c.data_type = 'timestamp with time zone'
		group by c.table_name
		order by c.table_name
	`)
	if err != nil {
		t.Fatalf("read the going tables' times: %v", err)
	}
	tables, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (agedTable, error) {
		var table agedTable
		err := row.Scan(&table.name, &table.columns)
		return table, err
	})
	if err != nil {
		t.Fatalf("read the going tables' times: %v", err)
	}
	if !slices.ContainsFunc(tables, func(table agedTable) bool { return table.name == "going_runs" }) {
		t.Fatalf("the going tables with times are %v, which leaves out going_runs", tables)
	}
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for _, table := range tables {
		sets := make([]string, len(table.columns))
		for i, column := range table.columns {
			name := pgx.Identifier{column}.Sanitize()
			sets[i] = name + " = " + name + " - make_interval(secs => $1)"
		}
		if _, err := tx.Exec(ctx, "update "+pgx.Identifier{table.name}.Sanitize()+" set "+strings.Join(sets, ", "), age.Seconds()); err != nil {
			t.Fatalf("date %s back %s: %v", table.name, age, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

// releaseSwept waits for a pass of the release's maintenance sweep that starts after it is
// called. It records a going request runAge old, which only a sweep deletes and only the release
// sweeps here (headSweepInterval), and waits until the request is gone.
func releaseSwept(t *testing.T, head agentServer) {
	t.Helper()
	var request string
	if err := head.database.Pool.QueryRow(context.Background(), `
		insert into going_requests (author, project, words, created_at, ends_at)
		values ('{}'::jsonb, '', 'a pause of the bar the release sweeps', now() - make_interval(secs => $1),
		  now() - make_interval(secs => $1) + make_interval(secs => $2))
		returning id::text
	`, runAge.Seconds(), releaseAnswerWindow.Seconds()).Scan(&request); err != nil {
		t.Fatalf("record a going request for the release's sweep: %v", err)
	}
	head.eventually(t, time.Minute, "a pass of the release's sweep, which deletes a going request "+runAge.String()+" old", func() bool {
		var left int
		if err := head.database.Pool.QueryRow(context.Background(), `select count(*) from going_requests where id = $1::uuid`, request).Scan(&left); err != nil {
			t.Fatal(err)
		}
		return left == 0
	})
}

// recordEarlierRun records a run of words at shape, queued with the test's person's grant, whose
// saved steps are a model call that called tool and that call's result, as a build that has tool
// saves them, and announces it, in one transaction; it answers the run's id.
func recordEarlierRun(t *testing.T, database *store.Store, words string, shape int, tool string) string {
	t.Helper()
	ctx := context.Background()
	call := map[string]any{"id": "call-1", "tool": tool, "arguments": map[string]any{}}
	model, err := json.Marshal(map[string]any{"text": "", "calls": []any{call}, "output_tokens": 0})
	if err != nil {
		t.Fatal(err)
	}
	result, err := json.Marshal(map[string]any{"call": call, "result": "step 1, call 1: status 200\n{}", "error": false})
	if err != nil {
		t.Fatal(err)
	}
	id := uuid.NewString()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for _, statement := range []struct {
		sql       string
		arguments []any
	}{
		{`insert into going_runs (id, author_login, words, conversation_id, level, mode, state, shape, created_at)
			values ($1::uuid, $2, $3, $1::uuid, 'standard', 'ask', 'queued', $4, clock_timestamp())`, []any{id, dispatchTestLogin, words, shape}},
		{`insert into going_run_grants (run_id, kind, login) values ($1::uuid, 'header', $2)`, []any{id, dispatchTestLogin}},
		{`insert into going_run_steps (run_id, step, kind, body, note) values ($1::uuid, 1, $2, $3::jsonb, $4), ($1::uuid, 2, $5, $6::jsonb, $7)`,
			[]any{id, agentwire.StepModel, string(model), "called " + tool, agentwire.StepTool, string(result), "the result of " + tool}},
		{`select pg_notify($1, 'ready')`, []any{api.GoingRunsChannel}},
	} {
		if _, err := tx.Exec(ctx, statement.sql, statement.arguments...); err != nil {
			t.Fatalf("record the run an earlier build saved: %v", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return id
}

// TestARunSurvivesTheReleaseBeforeIt: a run that one build saved goes on, or fails with its reason,
// on the next build (check c25). The release goingReleaseCommit and this tree, the head, serve one
// database, and the head's agent calls a fake chat model. The head's runs, a finished one in
// History and one whose plan waits, are dated 2 h back with every record of them
// (ageGoingTables), past the hour after which the release's sweep deletes a going request with
// its frames; a third run works past the release's 30 s answer window, after which that sweep ends
// a going request with an error frame. After a pass of the release's sweep (releaseSwept):
//   - History lists the two runs, and the head streams each one's frames as it stored them;
//   - the third run gets no error frame, and ends done when its model answers;
//   - the waiting plan runs on the head once it is approved;
//   - a run saved at a going_run_shape above the head's stays queued, unclaimed by the head, while
//     a run queued after it is worked.
//
// Then, on the head alone, a run whose saved steps name a tool the head does not know fails with a
// reason that names the tool.
func TestARunSurvivesTheReleaseBeforeIt(t *testing.T) {
	builds := newTwoBuildsOf(t, openGoingReleaseBuild(t))
	builds.startPrevious(t, map[string]string{"DISPATCH_TEST_MAINTENANCE_SWEEP_INTERVAL": releaseSweepInterval.String()})
	fake, script := newFakeModel(t)
	env := modelEnv(t, fake)
	env["DISPATCH_TEST_MAINTENANCE_SWEEP_INTERVAL"] = headSweepInterval.String()
	head := agentServer{dispatchProcess: builds.startCurrent(t, env), database: builds.database}
	shape := registeredGoingRunShape(t, head)
	project := uniqueProjectKey(t, "RUN")
	head.createProject(t, project, "Runs beside the release")
	issue, _ := head.createIssueIn(t, project, "The issue the plan comments on", "Context.\n")
	const comment = "The approved comment."

	const finishedWords, waitingWords, workingWords = "answer from History", "comment on the issue once I approve", "work while the release sweeps"
	script.set(finishedWords, func(int) agenttest.Reply { return answer("The answer History keeps.") })
	script.set(waitingWords, func(n int) agenttest.Reply {
		if n == 1 {
			return toolCall("propose_plan", map[string]any{"summary": "One comment on the issue.", "changes": []map[string]any{
				{"tool": "dispatch_comment", "arguments": map[string]any{"issue": issue, "body": comment}},
			}})
		}
		return answer("Commented.")
	})
	held := make(chan struct{})
	var answered sync.Once
	letTheWorkingRunAnswer := func() { answered.Do(func() { close(held) }) }
	t.Cleanup(letTheWorkingRunAnswer)
	script.set(workingWords, func(n int) agenttest.Reply {
		if n == 1 {
			return agenttest.Reply{Text: "Worked while the release swept.", Hold: held}
		}
		return answer("Worked.")
	})

	finished := startGoingRun(t, head.dispatchProcess, finishedWords)
	if row := head.ended(t, finished, time.Minute); row.state != agentwire.StateDone {
		t.Fatalf("the finished run ended %s with %q, want done", row.state, row.reason)
	}
	waiting := startGoingRun(t, head.dispatchProcess, waitingWords)
	head.eventually(t, time.Minute, "the plan to wait for its person", func() bool { return head.run(t, waiting).state == agentwire.StateWaiting })
	finishedFrames, waitingFrames := storedFrameNames(t, head.database, finished), storedFrameNames(t, head.database, waiting)
	if len(finishedFrames) == 0 || finishedFrames[len(finishedFrames)-1] != agentwire.FrameEnd || len(waitingFrames) == 0 {
		t.Fatalf("the finished run stored the frames %q and the waiting one %q, want an end frame last of the first and a frame of each", finishedFrames, waitingFrames)
	}
	ageGoingTables(t, head.database, runAge)

	working := startGoingRun(t, head.dispatchProcess, workingWords)
	started := time.Now()
	head.eventually(t, time.Minute, "the working run's model call", func() bool { return len(fake.Asking(workingWords)) == 1 })
	// Its model call is held past the release's answer window, so a sweep after it would end a going
	// request of this run with an error frame.
	time.Sleep(time.Until(started.Add(releaseAnswerWindow + time.Second)))
	releaseSwept(t, head)
	if row := head.run(t, working); row.state != "working" {
		t.Fatalf("the third run was %s with %q once the release swept, want working", row.state, row.reason)
	}

	t.Run("the head's runs dated 2 h back are still there after the release's sweep", func(t *testing.T) {
		var history struct {
			Rows []struct {
				Runs []struct {
					ID    string `json:"id"`
					State string `json:"state"`
				} `json:"runs"`
			} `json:"rows"`
		}
		getJSON(t, head.dispatchProcess, "/api/v1/me/going-runs", &history)
		listed := map[string]string{}
		for _, conversation := range history.Rows {
			for _, run := range conversation.Runs {
				listed[run.ID] = run.State
			}
		}
		for _, run := range []struct {
			name, id, state string
			frames          []string
		}{
			{"finished run", finished, agentwire.StateDone, finishedFrames},
			{"run whose plan waits", waiting, agentwire.StateWaiting, waitingFrames},
		} {
			if state, shown := listed[run.id]; !shown || state != run.state {
				t.Errorf("History lists the %s as %q (listed: %t), want %s", run.name, state, shown, run.state)
			}
			if streamed := frameNames(streamedFrames(t, head.dispatchProcess, run.id, len(run.frames), 15*time.Second)); !slices.Equal(streamed, run.frames) {
				t.Errorf("after the release's sweep the head streams the %s's frames %q, want the %q it stored before the sweep", run.name, streamed, run.frames)
			}
		}
		var plan struct {
			State string            `json:"state"`
			Rows  []json.RawMessage `json:"rows"`
		}
		getJSON(t, head.dispatchProcess, "/api/v1/going/runs/"+waiting+"/plan", &plan)
		if plan.State != agentwire.StateWaiting || len(plan.Rows) != 1 {
			t.Errorf("the waiting run's plan is %s with %d rows after the release's sweep, want waiting with its 1 row", plan.State, len(plan.Rows))
		}
	})

	t.Run("a run that works while the release sweeps gets no error frame", func(t *testing.T) {
		letTheWorkingRunAnswer()
		if row := head.ended(t, working, time.Minute); row.state != agentwire.StateDone {
			t.Errorf("the run that worked while the release swept ended %s with %q, want done", row.state, row.reason)
		}
		frames := streamedFrames(t, head.dispatchProcess, working, 0, 30*time.Second)
		for _, frame := range frames {
			if frame.name == releaseErrorFrame {
				t.Errorf("the run that worked while the release swept was streamed an %s frame: %s", frame.name, frame.data)
			}
		}
		var end agentwire.EndFrame
		if len(frames) == 0 || frames[len(frames)-1].name != agentwire.FrameEnd || json.Unmarshal([]byte(frames[len(frames)-1].data), &end) != nil || end.State != agentwire.StateDone {
			t.Errorf("the run that worked while the release swept streamed the frames %q, want an end frame of state done last", frameNames(frames))
		}
	})

	t.Run("the plan runs on the head when it is approved", func(t *testing.T) {
		head.call(t, http.MethodPost, "/api/v1/going/runs/"+waiting+"/plan/approve", `{"plan":1}`, http.StatusOK)
		if row := head.ended(t, waiting, time.Minute); row.state != agentwire.StateDone {
			t.Errorf("the approved run ended %s with %q, want done", row.state, row.reason)
		}
		var made int
		if err := head.database.Pool.QueryRow(context.Background(),
			`select count(*) from comments where issue_key = $1 and body like $2`, issue, comment+"%").Scan(&made); err != nil {
			t.Fatal(err)
		}
		if made != 1 {
			t.Errorf("%s holds the approved comment %d times, want once", issue, made)
		}
	})

	t.Run("a run saved at a going_run_shape above the head's stays unclaimed by the head", func(t *testing.T) {
		above := head.recordRun(t, dispatchTestLogin, "queued", shape+1)
		head.exec(t, `select pg_notify($1, 'ready')`, api.GoingRunsChannel)
		const laterWords = "a run queued after the one the head does not read"
		script.set(laterWords, func(int) agenttest.Reply { return answer("Read.") })
		later := startGoingRun(t, head.dispatchProcess, laterWords)
		if row := head.ended(t, later, time.Minute); row.state != agentwire.StateDone {
			t.Fatalf("the run queued after it ended %s with %q, want done", row.state, row.reason)
		}
		if row := head.run(t, above); row.state != "queued" || row.owner != "" {
			t.Errorf("the run at shape %d, above the head's %d, is %s under the lease of %q, want queued with no lease", shape+1, shape, row.state, row.owner)
		}
	})

	stop(t, builds.previous())
	t.Run("on the head alone, a saved step that names a tool the head does not know fails its run with that reason", func(t *testing.T) {
		const words = "go on from the steps an earlier build saved"
		script.set(words, func(int) agenttest.Reply { return answer("Went on.") })
		run := recordEarlierRun(t, head.database, words, shape, unknownTool)
		if row := head.ended(t, run, time.Minute); row.state != agentwire.StateFailed || !strings.Contains(row.reason, unknownTool) {
			t.Errorf("the run whose saved steps name the tool %q ended %s with %q, want failed with a reason that names the tool", unknownTool, row.state, row.reason)
		}
	})
}
