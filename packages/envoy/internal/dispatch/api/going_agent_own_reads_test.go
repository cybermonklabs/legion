package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/going"
	"github.com/sjawhar/envoy/going/agent/agenttest"
	"github.com/sjawhar/envoy/going/agentwire"
	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// TestTheAgentReadsTheAnswerRunsReadsAsThePersonDoes: each of the answer run's own reads that the
// bar's agent is offered (going.ReadSpecs), called by call_tool, reaches the route the person
// reads it by, with each argument where that route reads it, and is answered 200 (R12):
// read_progress of an issue reads the issue, whatever project it also names, of a project the
// project's issues, and naming neither the page's issue; read_architecture and
// read_project_activity read the project's components and its weeks; read_delivery reads the route
// its view names, the measures and the timeline over the window from and to name and the pipeline
// of the seven days ending on its to. A call that names no project reads the page's project, and
// one that names no item on a run whose page names none is refused.
func TestTheAgentReadsTheAnswerRunsReadsAsThePersonDoes(t *testing.T) {
	t.Parallel()
	writes := newGoingWritesRig(t)
	record := &goingServedRecord{mux: writes.mux.(*http.ServeMux)}
	writes.served.inProcess = record.wrap(writes.served.inProcess)
	rig := newGoingAgentRigOver(t, writes, nil)
	loop := newGoingLoop(t, rig)

	mill := rig.issue(t, "The quernstone mill")
	for _, statement := range []string{
		`insert into architecture_sources (project_key, id, repo, branch, installation_id, created_by)
			values ('DEMO', 1, 'acme/arch', 'main', 1, '{"kind":"system","id":"test"}')`,
		`insert into architecture_snapshots (id, project_key, commit, files) values (900, 'DEMO', 'c1', '{}')`,
		`insert into components (project_key, id, title, prose, parent, snapshot_id, source_id)
			values ('DEMO', 'millstone', 'The millstone', '', null, 900, 1)`,
	} {
		rig.exec(t, statement)
	}
	if _, err := delivery.PutSettings(t.Context(), rig.database.Pool, delivery.DeliverySettings{
		DeployRepo: "acme/widgets", DeployWorkflowPath: ".github/workflows/deploy.yml",
		ProductionJobNames: []string{"release"}, PRChecksWorkflowPath: ".github/workflows/pr-checks.yml",
		PopulationAuthors: []string{"octocat"},
	}, model.Actor{Kind: "system", ID: "test"}); err != nil {
		t.Fatalf("seed the delivery settings: %v", err)
	}
	now := time.Now().UTC()
	from, to := now.Add(-48*time.Hour).Format(time.RFC3339), now.Format(time.RFC3339)

	// read runs a run of alice's, on the page of the issue page names where it is set, whose model
	// calls call_tool with tool and arguments and then answers, and answers what the model was told
	// of the call and the last request the route table served for the run: the call's, after the
	// read of the page's issue the run makes first.
	read := func(t *testing.T, words, page, tool string, arguments map[string]any) (string, goingServedRequest) {
		t.Helper()
		loop.script.set(words, func(n int) agenttest.Reply {
			if n == 1 {
				return toolCall("call_tool", map[string]any{"tool": tool, "arguments": arguments})
			}
			return answered("Read.")
		})
		record.take()
		run := rig.begin(t, goingLoopRun{login: "alice", words: words, page: page})
		if ended := rig.ended(t, run, time.Minute); ended.state != "done" {
			t.Fatalf("the run ended %s with %q, want done", ended.state, ended.reason)
		}
		results, served := rig.toolResults(t, run), record.take()
		if len(results) == 0 || len(served) == 0 {
			t.Fatalf("the run saved the tool results %q and reached %d routes, want the call's", results, len(served))
		}
		return results[len(results)-1], served[len(served)-1]
	}

	for _, row := range []struct {
		name, page, tool string
		arguments        map[string]any
		// route is the row of the table the call reads, path the value of each placeholder of its
		// path, and query what its request holds under each key named, nil where it holds none.
		route string
		path  map[string]string
		query url.Values
		holds []string
	}{
		{name: "read_progress of an issue reads the issue with its progress", tool: going.ReadProgress,
			arguments: map[string]any{"issue": mill, "project": "DEMO"},
			route:     "GET /api/v1/issues/{key}", path: map[string]string{"key": mill}, query: url.Values{"project": nil, "issue": nil},
			holds: []string{"The quernstone mill", `"progress"`}},
		{name: "read_progress of a project lists the project's issues", tool: going.ReadProgress,
			arguments: map[string]any{"project": "DEMO"},
			route:     "GET /api/v1/issues", query: url.Values{"project": {"DEMO"}}, holds: []string{mill}},
		{name: "read_progress naming nothing reads the page's issue", page: mill, tool: going.ReadProgress,
			arguments: map[string]any{},
			route:     "GET /api/v1/issues/{key}", path: map[string]string{"key": mill}, holds: []string{"The quernstone mill"}},
		{name: "read_architecture reads the project's components", tool: going.ReadArchitecture,
			arguments: map[string]any{"project": "DEMO"},
			route:     "GET /api/v1/projects/{key}/architecture", path: map[string]string{"key": "DEMO"}, holds: []string{`"millstone"`}},
		{name: "read_project_activity reads the project's weeks", tool: going.ReadProjectActivity,
			arguments: map[string]any{"project": "DEMO", "weeks": 4},
			route:     "GET /api/v1/projects/{key}/activity", path: map[string]string{"key": "DEMO"}, query: url.Values{"weeks": {"4"}},
			holds: []string{`"weeks"`}},
		{name: "read_delivery of the measures reads its window's measures", tool: going.ReadDelivery,
			arguments: map[string]any{"view": "measures", "from": from, "to": to},
			route:     "GET /api/v1/delivery/measures", query: url.Values{"from": {from}, "to": {to}, "view": nil}},
		{name: "read_delivery of the timeline reads its window's timeline", tool: going.ReadDelivery,
			arguments: map[string]any{"view": "timeline", "from": from, "to": to},
			route:     "GET /api/v1/delivery/timeline", query: url.Values{"from": {from}, "to": {to}, "view": nil}},
		{name: "read_delivery of the pipeline reads the seven days ending on its to", tool: going.ReadDelivery,
			arguments: map[string]any{"view": "pipeline", "to": to},
			route:     "GET /api/v1/delivery/pipeline", query: url.Values{"at": {to}, "to": nil, "view": nil}},
	} {
		t.Run(row.name, func(t *testing.T) {
			result, served := read(t, row.name, row.page, row.tool, row.arguments)
			if served.route != row.route {
				t.Fatalf("the call read %s?%s, want %s; the model was told %q", served.route, served.query.Encode(), row.route, result)
			}
			for placeholder, want := range row.path {
				if got := served.path[placeholder]; got != want {
					t.Errorf("the read's {%s} is %q, want %q", placeholder, got, want)
				}
			}
			for key, want := range row.query {
				if got := served.query[key]; !slices.Equal(got, want) {
					t.Errorf("the read's query holds %s=%q, want %q", key, got, want)
				}
			}
			if !strings.Contains(result, "status 200") {
				t.Errorf("the model was told %q, want status 200", result)
			}
			for _, held := range row.holds {
				if !strings.Contains(result, held) {
					t.Errorf("the model was told %q, which does not hold %q", result, held)
				}
			}
		})
	}

	// working records a run of alice's that an agent works on under a lease, on a page whose
	// context chip is page ("" for none), in one statement, so the test's agent loop never claims
	// it, and answers the run and its lease token.
	working := func(t *testing.T, words, page string) (string, string) {
		t.Helper()
		run, token := uuid.NewString(), uuid.NewString()
		var chip *string
		if page != "" {
			chip = &page
		}
		rig.exec(t, `
			insert into going_runs (id, author_login, words, conversation_id, level, mode, state, shape, created_at, context,
			  lease_token, lease_owner, lease_ends_at)
			values ($1::uuid, 'alice', $3, $1::uuid, 'standard', 'ask', 'working', $4, clock_timestamp(), $5::jsonb,
			  $2::uuid, 'the-agent-of-this-test', now() + interval '10 minutes')
		`, run, token, words, goingRunShape, chip)
		rig.exec(t, `insert into going_run_grants (run_id, kind, login) values ($1::uuid, 'header', 'alice')`, run)
		return run, token
	}

	t.Run("a read naming no project reads the page's project", func(t *testing.T) {
		run, token := working(t, "read the page's project", `{"path":"/projects/DEMO","project":"DEMO"}`)
		for call, want := range []struct {
			tool, route, key string
			query            url.Values
		}{
			{tool: going.ReadArchitecture, route: "GET /api/v1/projects/{key}/architecture", key: "DEMO"},
			{tool: going.ReadProjectActivity, route: "GET /api/v1/projects/{key}/activity", key: "DEMO"},
			{tool: going.ReadProgress, route: "GET /api/v1/issues", query: url.Values{"project": {"DEMO"}}},
		} {
			record.take()
			answer, err := rig.call(goingRouteCall(run, token, 1, call+1, want.tool, map[string]any{}))
			served := record.take()
			if err != nil || answer.Status != http.StatusOK || len(served) != 1 {
				t.Fatalf("%s naming nothing on DEMO's page answered %d %s, %v, reaching %d routes; want 200 from one", want.tool, answer.Status, answer.Body, err, len(served))
			}
			if served[0].route != want.route || served[0].path["key"] != want.key {
				t.Errorf("%s naming nothing on DEMO's page read %s with {key} %q, want %s of %q", want.tool, served[0].route, served[0].path["key"], want.route, want.key)
			}
			for key, value := range want.query {
				if got := served[0].query[key]; !slices.Equal(got, value) {
					t.Errorf("%s naming nothing on DEMO's page sent %s=%q, want %q", want.tool, key, got, value)
				}
			}
		}
	})

	t.Run("a read naming no item on a page that names none is refused", func(t *testing.T) {
		run, token := working(t, "read nothing named", "")
		for call, tool := range []string{going.ReadProgress, going.ReadArchitecture, going.ReadProjectActivity} {
			record.take()
			_, err := rig.call(goingRouteCall(run, token, 1, call+1, tool, map[string]any{}))
			requireAgentCode(t, err, agentwire.CodeCallRefused, tool+" naming nothing on a run with no page")
			if served := record.take(); len(served) != 0 {
				t.Errorf("the refused %s reached %d routes, want none", tool, len(served))
			}
		}
	})
}

// TestTheAgentsRosterReadStatesEverySession: the bar agent's roster read of a listener that lists
// 73 sessions lists the goingReadSessionMax seen last, the newest first, and the total the model
// is told before them, which its answer states, is 73: every session, as the answer run's read
// counts them, and not the rows the bound left.
func TestTheAgentsRosterReadStatesEverySession(t *testing.T) {
	t.Parallel()
	const sessions = 73
	listed := make([]map[string]any, 0, sessions)
	for index := range sessions {
		listed = append(listed, map[string]any{
			"session_id": fmt.Sprintf("ses-%02d", index), "title": fmt.Sprintf("Worker %02d", index), "last_seen": 1000 + index, "state": "live",
		})
	}
	answer, err := json.Marshal(listed)
	if err != nil {
		t.Fatal(err)
	}
	listener := newStatusListener(t, func(_ int, w http.ResponseWriter) { _, _ = w.Write(answer) })
	rig := newGoingAgentRigOver(t, newGoingWritesRigOn(t, listener.url, func(database *store.Store) *store.Store { return database }), nil)
	loop := newGoingLoop(t, rig)
	const words = "how many agent sessions are there"
	loop.script.set(words, func(n int) agenttest.Reply {
		if n == 1 {
			return toolCall("call_tool", map[string]any{"tool": going.ReadRoster, "arguments": map[string]any{}})
		}
		return answered("Read.")
	})
	run := rig.begin(t, goingLoopRun{login: "alice", words: words})
	if ended := rig.ended(t, run, time.Minute); ended.state != "done" {
		t.Fatalf("the run ended %s with %q, want done", ended.state, ended.reason)
	}
	results := rig.toolResults(t, run)
	// The total stands before the list, in the label's noun (the agent's resultCount).
	line := fmt.Sprintf(": status 200\n[Total: %d matching sessions; the list below holds %d of them.]\n", sessions, goingReadSessionMax)
	_, body, told := strings.Cut(strings.Join(results, ""), line)
	var roster struct {
		Total    int              `json:"total"`
		Sessions []map[string]any `json:"sessions"`
	}
	if len(results) != 1 || !told || json.Unmarshal([]byte(body), &roster) != nil {
		t.Fatalf("the run saved the tool results %q, want the roster read whole after %q", results, line)
	}
	if roster.Total != sessions || len(roster.Sessions) != goingReadSessionMax {
		t.Fatalf("the roster read answered a total of %d and %d sessions, want %d and %d", roster.Total, len(roster.Sessions), sessions, goingReadSessionMax)
	}
	if newest := roster.Sessions[0][going.FieldSessionID]; newest != fmt.Sprintf("ses-%02d", sessions-1) {
		t.Errorf("the roster read lists %v first, want the session seen last", newest)
	}
}
