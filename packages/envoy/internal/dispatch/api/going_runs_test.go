package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/going/agent"
	"github.com/sjawhar/envoy/going/agentwire"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// goingRunsRig is a server on the harness's database: its handler, which History is read
// through, and the database the runs are written to as the route that starts a run writes them.
type goingRunsRig struct {
	handler  http.Handler
	database *store.Store
}

func newGoingRunsRig(t *testing.T) goingRunsRig {
	t.Helper()
	handler, database, _ := newTestServer(t, testServerOptions{})
	return goingRunsRig{handler: handler, database: database}
}

// run records one ended run of login's, the first of a conversation of its own, and answers its
// id.
func (rig goingRunsRig) run(t *testing.T, login, words string) string {
	t.Helper()
	return rig.follow(t, login, words, "")
}

// follow records one ended run of login's following continues, in its conversation, or the first
// of a conversation of its own where continues is empty, and answers its id.
func (rig goingRunsRig) follow(t *testing.T, login, words, continues string) string {
	t.Helper()
	id := uuid.NewString()
	var follows *string
	if continues != "" {
		follows = &continues
	}
	if _, err := rig.database.Pool.Exec(context.Background(), `
		insert into going_runs (id, author_login, words, context, conversation_id, continues, level, mode, state, shape, created_at, finished_at)
		values ($1::uuid, $2, $3, '{"path": "/issues/DEMO-12", "project": "DEMO", "issue": "DEMO-12", "block": ""}'::jsonb,
		  coalesce((select coalesce(conversation_id, id) from going_runs where id = $4::uuid), $1::uuid), $4::uuid,
		  'standard', 'ask', 'done', $5, clock_timestamp(), clock_timestamp())
	`, id, login, words, follows, goingRunShape); err != nil {
		t.Fatalf("record the run %q: %v", words, err)
	}
	return id
}

// age moves a run's created_at back by age.
func (rig goingRunsRig) age(t *testing.T, id string, age time.Duration) {
	t.Helper()
	if _, err := rig.database.Pool.Exec(context.Background(), `
		update going_runs set created_at = now() - $2::interval where id = $1::uuid
	`, id, age.String()); err != nil {
		t.Fatalf("age the run: %v", err)
	}
}

// touched adds issue to the run's issues, as a request of the run that landed on it does.
func (rig goingRunsRig) touched(t *testing.T, id, issue string) {
	t.Helper()
	if _, err := rig.database.Pool.Exec(context.Background(), `
		update going_runs set issues = array_append(issues, $2) where id = $1::uuid
	`, id, issue); err != nil {
		t.Fatalf("add %s to the run's issues: %v", issue, err)
	}
}

// list reads GET /api/v1/me/going-runs as login (goingHistory).
func (rig goingRunsRig) list(t *testing.T, login, query string) goingConversationsPage {
	t.Helper()
	return goingHistory(t, rig.handler, login, query)
}

// goingHistory reads History, GET /api/v1/me/going-runs with query, from handler as login.
func goingHistory(t *testing.T, handler http.Handler, login, query string) goingConversationsPage {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/me/going-runs"+query, nil, login)
	if response.Code != http.StatusOK {
		t.Fatalf("list %s's conversations%s: status=%d body=%s", login, query, response.Code, response.Body.String())
	}
	return decodeBody[goingConversationsPage](t, response)
}

// goingConversationIDs is the id of each conversation of page, in order.
func goingConversationIDs(page goingConversationsPage) []string {
	ids := make([]string, 0, len(page.Rows))
	for _, conversation := range page.Rows {
		ids = append(ids, conversation.ID)
	}
	return ids
}

// A run is stored under its caller and read back by that caller alone, its conversation whose
// run started last first; a second person reads none of it, and the route takes no login that
// could name another.
func TestGoingRunsAreReadByTheirCallerAloneNewestFirst(t *testing.T) {
	rig := newGoingRunsRig(t)
	older := rig.run(t, "alice", "what moved yesterday?")
	rig.age(t, older, time.Hour)
	newer := rig.run(t, "alice", "close DEMO-12")

	page := rig.list(t, "alice", "")
	if got := goingConversationIDs(page); !slices.Equal(got, []string{newer, older}) || page.Total != 2 || page.Limit != 50 || page.Offset != 0 {
		t.Fatalf("alice's conversations = %v (total %d, limit %d, offset %d), want [%s %s] newest first on the default page",
			got, page.Total, page.Limit, page.Offset, newer, older)
	}
	if runs := page.Rows[0].Runs; len(runs) != 1 || runs[0].ID != newer || runs[0].Words != "close DEMO-12" ||
		runs[0].Context == nil || runs[0].Context.Issue != "DEMO-12" || runs[0].State != "done" {
		t.Fatalf("the newest conversation holds %+v, want alice's run with its words and context", runs)
	}
	if got := rig.list(t, "alice", "?limit=1&offset=1"); !slices.Equal(goingConversationIDs(got), []string{older}) || got.Total != 2 {
		t.Fatalf("alice's second page of one = %v (total %d), want [%s] of 2", goingConversationIDs(got), got.Total, older)
	}
	if got := rig.list(t, "bob", ""); len(got.Rows) != 0 || got.Total != 0 {
		t.Fatalf("bob reads %v of alice's conversations, want none", goingConversationIDs(got))
	}
	if _, err := readGoingRun(context.Background(), rig.database.Pool, newer, "bob"); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("bob reading alice's run by id: %v, want pgx.ErrNoRows", err)
	}
}

// A follow-up is listed in the conversation of the run it follows, its runs newest first, and
// the conversation is listed by its newest run.
func TestGoingRunsAConversationHoldsItsRunsNewestFirst(t *testing.T) {
	rig := newGoingRunsRig(t)
	first := rig.run(t, "alice", "which colour are plums?")
	rig.age(t, first, 2*time.Hour)
	other := rig.run(t, "alice", "something else")
	rig.age(t, other, time.Hour)
	next := rig.follow(t, "alice", "and pears?", first)

	page := rig.list(t, "alice", "")
	if got := goingConversationIDs(page); !slices.Equal(got, []string{first, other}) {
		t.Fatalf("alice's conversations = %v, want [%s %s]: the one followed up now first", got, first, other)
	}
	runs := page.Rows[0].Runs
	if len(runs) != 2 || runs[0].ID != next || runs[1].ID != first || runs[0].Continues == nil || *runs[0].Continues != first ||
		runs[0].Conversation != first || runs[1].Conversation != first {
		t.Fatalf("the conversation holds %+v, want the follow-up %s, continuing %s, then %s", runs, next, first, first)
	}
}

// ?issue= keeps the caller's conversations with a run whose landed requests touched the issue,
// and a second person who has no such run reads nothing over the same issue.
func TestGoingRunsIssueFilterReadsThePersonsRunAlone(t *testing.T) {
	rig := newGoingRunsRig(t)
	wrote := rig.run(t, "alice", "start DEMO-12")
	rig.touched(t, wrote, "DEMO-12")
	rig.run(t, "alice", "nothing about either")
	bobs := rig.run(t, "bob", "start DEMO-99")
	rig.touched(t, bobs, "DEMO-99")

	if got := goingConversationIDs(rig.list(t, "alice", "?issue=DEMO-12")); !slices.Equal(got, []string{wrote}) {
		t.Fatalf("alice's conversations over DEMO-12 = %v, want the run that wrote it [%s]", got, wrote)
	}
	if got := goingConversationIDs(rig.list(t, "bob", "?issue=DEMO-12")); len(got) != 0 {
		t.Fatalf("bob's conversations over DEMO-12 = %v, want none: the run over it is alice's", got)
	}
	if got := goingConversationIDs(rig.list(t, "alice", "?issue=DEMO-99")); len(got) != 0 {
		t.Fatalf("alice's conversations over DEMO-99 = %v, want none: the run over it is bob's", got)
	}
}

// ?q= keeps the conversations with a run whose words hold the text in any case.
func TestGoingRunsWordsFilter(t *testing.T) {
	rig := newGoingRunsRig(t)
	for _, words := range []string{"Start DEMO-1", "start DEMO-2", "start DEMO-3", "start DEMO-4", "what is DEMO-5?"} {
		rig.run(t, "alice", words)
	}
	if got := rig.list(t, "alice", "?q=start%20demo"); len(got.Rows) != 4 || got.Total != 4 {
		t.Fatalf("alice's conversations whose words hold 'start demo' = %v, want the four in any case", goingConversationIDs(got))
	}
	if got := goingConversationIDs(rig.list(t, "alice", "?q=DEMO-5")); len(got) != 1 {
		t.Fatalf("alice's conversations whose words hold DEMO-5 = %v, want one", got)
	}
}

// A conversation whose newest run is older than seven days is not read, even before the sweep
// deletes it; one inside them is.
func TestGoingRunsOlderThanSevenDaysAreNotReturned(t *testing.T) {
	rig := newGoingRunsRig(t)
	old := rig.run(t, "alice", "eight days ago")
	rig.touched(t, old, "DEMO-12")
	rig.age(t, old, 8*24*time.Hour)
	kept := rig.run(t, "alice", "six days ago")
	rig.touched(t, kept, "DEMO-12")
	rig.age(t, kept, 6*24*time.Hour)

	for _, query := range []string{"", "?issue=DEMO-12"} {
		if got := goingConversationIDs(rig.list(t, "alice", query)); !slices.Equal(got, []string{kept}) {
			t.Fatalf("alice's conversations%s = %v, want the 6-day one alone [%s]", query, got, kept)
		}
	}
	if _, err := readGoingRun(context.Background(), rig.database.Pool, old, "alice"); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("reading the 8-day run: %v, want pgx.ErrNoRows", err)
	}
}

// The maintenance pass deletes a conversation whose newest run is older than seven days and keeps
// one inside them.
func TestGoingRunsSweepDeletesARunPastSevenDays(t *testing.T) {
	rig := newGoingRunsRig(t)
	old := rig.run(t, "alice", "eight days ago")
	rig.age(t, old, 8*24*time.Hour)
	kept := rig.run(t, "alice", "six days ago")
	rig.age(t, kept, 6*24*time.Hour)

	stored := func(id string) bool {
		var count int
		if err := rig.database.Pool.QueryRow(context.Background(), `select count(*) from going_runs where id = $1::uuid`, id).Scan(&count); err != nil {
			t.Fatalf("count the run: %v", err)
		}
		return count == 1
	}
	leases := sweepLeases(t, rig.database)
	runSweep(t, func(ctx context.Context) {
		RunMaintenanceSweep(ctx, rig.database.Pool, maintenanceTestInterval, leases, nil)
	})
	waitUntil(t, 30*time.Second, "the 8-day run deleted by a maintenance pass", func() bool { return !stored(old) })
	if !stored(kept) {
		t.Fatal("the maintenance pass deleted the 6-day run, want it kept")
	}
}

// TestTheReaperEndsOnlyARunPastItsBudgetWithNoLease: the reaper stops a working run whose lease ran
// out longer ago than what was left of its level's working time, so no agent took it up for all the
// time it had left: its person reads it stopped with the time budget's reason, its lease is freed,
// and its stream writes the end frame, which offers Continue. It leaves a working run an agent still
// holds, though that run has spent its working time and started an hour ago; a working run whose
// lease ran out with more of its working time left than has passed since; and a run that waits, is
// queued or is done, though its lease ran out an hour ago.
func TestTheReaperEndsOnlyARunPastItsBudgetWithNoLease(t *testing.T) {
	rig := newGoingRunsRig(t)
	budget := agent.Levels["standard"].Time
	// record stores a standard run of alice's in state, started started ago, that has worked worked
	// of its working time, under a lease that ends leaseEnds from now (that long ago where it is
	// negative), and answers the run and its lease token.
	record := func(t *testing.T, state string, started, worked, leaseEnds time.Duration) (run, token string) {
		t.Helper()
		run, token = uuid.NewString(), uuid.NewString()
		if _, err := rig.database.Pool.Exec(context.Background(), `
			insert into going_runs (id, author_login, words, conversation_id, level, mode, state, shape, created_at, finished_at,
			  budget_used, lease_token, lease_owner, lease_ends_at)
			values ($1::uuid, 'alice', 'tidy DEMO-12', $1::uuid, 'standard', 'ask', $2, $3, now() - make_interval(secs => $4),
			  case when $2 = 'done' then now() end, jsonb_build_object('calls', 3, 'working_ms', $5::bigint, 'tokens', 0),
			  $6::uuid, 'an agent of this test', now() + make_interval(secs => $7))
		`, run, state, goingRunShape, started.Seconds(), worked.Milliseconds(), token, leaseEnds.Seconds()); err != nil {
			t.Fatalf("record a %s run: %v", state, err)
		}
		return run, token
	}
	// read is the run as its person reads it, GET /api/v1/going/runs/{id}.
	read := func(t *testing.T, run string) string {
		t.Helper()
		response := dispatchRequest(t, rig.handler, http.MethodGet, "/api/v1/going/runs/"+run, nil, "alice")
		if response.Code != http.StatusOK {
			t.Fatalf("GET run %s answered %d %s", run, response.Code, response.Body.String())
		}
		return strings.TrimSpace(response.Body.String())
	}
	// lease is the run's lease token, empty where none holds it.
	lease := func(t *testing.T, run string) string {
		t.Helper()
		var token string
		if err := rig.database.Pool.QueryRow(context.Background(), `
			select coalesce(lease_token::text, '') from going_runs where id = $1::uuid
		`, run).Scan(&token); err != nil {
			t.Fatalf("read run %s's lease: %v", run, err)
		}
		return token
	}
	type leftRun struct{ state, run, token, before string }
	// left records a run the reaper must leave, with what its person reads of it before the reaper.
	left := func(t *testing.T, state string, started, worked, leaseEnds time.Duration) leftRun {
		t.Helper()
		run, token := record(t, state, started, worked, leaseEnds)
		return leftRun{state: state, run: run, token: token, before: read(t, run)}
	}
	// kept checks that the reaper left run: its person reads it as before, the same lease holds it,
	// and its stream holds no frame.
	kept := func(t *testing.T, run leftRun) {
		t.Helper()
		if after := read(t, run.run); after != run.before {
			t.Errorf("the %s run reads %s after the reaper, want it as before: %s", run.state, after, run.before)
		}
		if token := lease(t, run.run); token != run.token {
			t.Errorf("the %s run's lease is %q after the reaper, want the lease %s it held", run.state, token, run.token)
		}
		var frames int
		if err := rig.database.Pool.QueryRow(context.Background(), `
			select count(*) from going_run_events where run_id = $1::uuid
		`, run.run).Scan(&frames); err != nil || frames != 0 {
			t.Errorf("the %s run holds %d frames (%v) after the reaper, want none", run.state, frames, err)
		}
	}

	// Its lease ran out 2 minutes ago with 1 minute of its working time left.
	overdue, _ := record(t, "working", 10*time.Minute, budget-time.Minute, -2*time.Minute)
	// An agent holds it for another minute: that agent stops it at its bound itself.
	held := left(t, "working", time.Hour, budget, time.Minute)
	// Its lease ran out 30 seconds ago with all but a minute of its working time left.
	inside := left(t, "working", 2*time.Minute, time.Minute, -30*time.Second)
	var others []leftRun
	for _, state := range []string{"waiting", "queued", "done"} {
		others = append(others, left(t, state, time.Hour, budget, -time.Hour))
	}

	if err := reapGoingRuns(context.Background(), rig.database.Pool); err != nil {
		t.Fatalf("reap: %v", err)
	}

	t.Run("a working run with no live lease past its working budget is stopped TIME_BUDGET with an end frame that offers Continue", func(t *testing.T) {
		var run goingRun
		if err := json.Unmarshal([]byte(read(t, overdue)), &run); err != nil {
			t.Fatal(err)
		}
		if run.State != agentwire.StateStopped || run.StopReason == nil || !strings.HasPrefix(*run.StopReason, agent.ReasonTimeBudget+":") || run.FinishedAt == nil {
			t.Fatalf("the overdue run reads %s, stopped by %v, ended at %v; want it stopped with the reason %s, ended", run.State, run.StopReason, run.FinishedAt, agent.ReasonTimeBudget)
		}
		if token := lease(t, overdue); token != "" {
			t.Errorf("the stopped run is held by the lease %s, want it freed", token)
		}
		stream := dispatchRequest(t, rig.handler, http.MethodGet, "/api/v1/going/runs/"+overdue+"/events", nil, "alice")
		if stream.Code != http.StatusOK {
			t.Fatalf("the run's stream answered %d %s", stream.Code, stream.Body.String())
		}
		var frames []goingStreamFrame
		for _, block := range strings.Split(strings.TrimSpace(stream.Body.String()), "\n\n") {
			var frame goingStreamFrame
			for _, line := range strings.Split(block, "\n") {
				frame.take(line)
			}
			frames = append(frames, frame)
		}
		var end agentwire.EndFrame
		if len(frames) != 1 || frames[0].name != agentwire.FrameEnd || json.Unmarshal([]byte(frames[0].data), &end) != nil ||
			end.State != agentwire.StateStopped || end.Reason != *run.StopReason || !slices.Equal(end.Offers, []string{agentwire.OfferContinue}) {
			t.Errorf("the run's stream wrote %+v, want the one end frame of the stopped run with its reason, offering %s", frames, agentwire.OfferContinue)
		}
	})

	t.Run("a working run whose lease is live is left", func(t *testing.T) { kept(t, held) })

	t.Run("a working run with no lease inside its budget is left", func(t *testing.T) { kept(t, inside) })

	t.Run("a waiting, queued or done run is left", func(t *testing.T) {
		for _, run := range others {
			kept(t, run)
		}
	})
}
