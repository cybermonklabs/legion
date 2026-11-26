package api

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"
)

// openAsksMembersBefore are the members GET /api/v1/asks/open answered before default_assignee,
// and openAskRowMembersBefore those of each of its rows: what a reader built before the field
// reads, which every answer still carries.
var (
	openAsksMembersBefore = []string{
		"session_id", "as_of", "opened_since", "count", "waiting_on_human", "waiting_on_agent", "asks",
	}
	openAskRowMembersBefore = []string{
		"id", "ref", "question", "kind", "urgency", "created_at", "age_seconds", "priority", "owner",
		"human_replied", "last_reply", "waiting_on", "waiting_on_person",
	}
)

// openAsksMembers is one answer of GET /api/v1/asks/open by member, each as the JSON it holds, so
// that a row tells a member the answer left out from one it answered null.
func openAsksMembers(t *testing.T, response *httptest.ResponseRecorder) map[string]json.RawMessage {
	t.Helper()
	if response.Code != http.StatusOK {
		t.Fatalf("read open asks: status=%d body=%s", response.Code, response.Body.String())
	}
	return decodeBody[map[string]json.RawMessage](t, response)
}

// openAskRowsOf is the asks member of an answer of GET /api/v1/asks/open, each row by member.
func openAskRowsOf(t *testing.T, members map[string]json.RawMessage) []map[string]json.RawMessage {
	t.Helper()
	var rows []map[string]json.RawMessage
	if err := json.Unmarshal(members["asks"], &rows); err != nil {
		t.Fatalf("decode the asks member %s: %v", members["asks"], err)
	}
	return rows
}

// requireDefaultAssignee fails t unless the answer carries default_assignee as want: the login,
// or JSON null where want is nil.
func requireDefaultAssignee(t *testing.T, members map[string]json.RawMessage, want *string) {
	t.Helper()
	raw, carried := members["default_assignee"]
	if !carried {
		t.Fatalf("the answer carries no default_assignee: %v", members)
	}
	var got *string
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("default_assignee is %s, neither a login nor null: %v", raw, err)
	}
	switch {
	case want == nil && got != nil:
		t.Fatalf("default_assignee is %q, want null", *got)
	case want != nil && got == nil:
		t.Fatalf("default_assignee is null, want %q", *want)
	case want != nil && *got != *want:
		t.Fatalf("default_assignee is %q, want %q", *got, *want)
	}
}

// requireMembersBefore fails t unless the answer carries every member a reader before
// default_assignee reads, its own and each row's.
func requireMembersBefore(t *testing.T, members map[string]json.RawMessage) {
	t.Helper()
	for _, member := range openAsksMembersBefore {
		if _, carried := members[member]; !carried {
			t.Fatalf("the answer lost %s, which a reader before default_assignee reads: %v", member, members)
		}
	}
	for _, row := range openAskRowsOf(t, members) {
		for _, member := range openAskRowMembersBefore {
			if _, carried := row[member]; !carried {
				t.Fatalf("an ask row lost %s, which a reader before default_assignee reads: %v", member, row)
			}
		}
	}
}

// GET /api/v1/asks/open?project=P answers, beside P's open asks, the default assignee P stores
// (default_assignee): the person an ask on P reaches while nobody handed it on and its issue
// holds nobody, which a Legion daemon reads when it boots. The daemon's credential holds no
// `person` scope, so the read gives it that person and no power: the routes that list every
// project's default and move one still refuse it. Each row reads through the daemon's own broker
// token (liveDaemonToken), the credential class the daemon presents, except where it says.
//
//   - a: a project with a default and an open ask answers that default, which is not the person
//     its ask waits on;
//   - b: a project with a default and no open ask (its one ask answered) still answers it;
//   - c: each project answers its own stored default, never another project's: three projects
//     with three defaults, the first made and the first by key among them;
//   - d: a key no project has answers default_assignee as null, not left out;
//   - e: an author's read (?author_session=) spans projects and leaves the member out, where the
//     project read of the same ask carries it;
//   - f: the daemon's broker token and the deployment's shared token, neither holding the
//     `person` scope, read the default here and are refused the list of every project's default
//     and the move of one, which stores nothing;
//   - g: the answer that carries default_assignee still carries every member an older reader
//     reads, and decodes into that reader's shape (openAsksRead) with the values it had.
func TestOpenAsksProjectDefault(t *testing.T) {
	t.Parallel()
	rig := newBrokerTokenRig(t)
	handler, database := rig.handler, rig.database
	const carol = "carol@example.com"
	seedPeople(t, database, carol)

	// QUIET is the first project made, BRK (brokerTestProject, the daemon's) the first by key,
	// and each of the three names another default, so a read that answered the oldest project's
	// default, or the first one by key, answers the wrong person for the other two.
	quiet := createInteractionIssue(t, handler, "QUIET", "An issue whose question is answered", "A spec\n")
	answered := createIssueAsk(t, handler, quiet.Key, "Ship the answered one?")
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+answered+"/answer",
		map[string]any{"selected": []string{"Yes"}}, "alice"); response.Code != http.StatusOK {
		t.Fatalf("answer the QUIET question: status=%d body=%s", response.Code, response.Body.String())
	}
	putDefaultAssignee(t, handler, "QUIET", carol)
	// The issue alice created holds alice, so its open ask waits on her while BRK defaults to bob.
	held := createInteractionIssue(t, handler, brokerTestProject, "An issue alice holds", "A spec\n")
	waiting := createIssueAsk(t, handler, held.Key, "Which way on the issue alice holds?")
	putDefaultAssignee(t, handler, brokerTestProject, "bob")
	// NEXT keeps the default its create gave it: alice, who made it.
	next := createInteractionIssue(t, handler, "NEXT", "An issue of a project nobody moved", "A spec\n")
	nextAsk := createIssueAsk(t, handler, next.Key, "Which way on NEXT?")

	daemon := newBrokerCaller(t)
	rig.broker.Says("the-daemon-token", liveDaemonToken(daemon.jkt))
	asDaemon := func(t *testing.T, method, target string, body any) *httptest.ResponseRecorder {
		t.Helper()
		return rig.serve(daemon.presenting(t, method, target, "the-daemon-token", body))
	}
	projectRead := func(t *testing.T, project string) map[string]json.RawMessage {
		t.Helper()
		return openAsksMembers(t, asDaemon(t, http.MethodGet, "/api/v1/asks/open?project="+url.QueryEscape(project), nil))
	}
	stored := func(t *testing.T, project string) string {
		t.Helper()
		var login string
		if err := database.Pool.QueryRow(context.Background(),
			`select default_assignee from projects where key = $1`, project).Scan(&login); err != nil {
			t.Fatalf("read %s's stored default: %v", project, err)
		}
		return login
	}
	askIDs := func(t *testing.T, members map[string]json.RawMessage) []string {
		t.Helper()
		ids := []string{}
		for _, row := range openAskRowsOf(t, members) {
			var id string
			if err := json.Unmarshal(row["id"], &id); err != nil {
				t.Fatalf("decode an ask row's id %s: %v", row["id"], err)
			}
			ids = append(ids, id)
		}
		slices.Sort(ids)
		return ids
	}
	bob := "bob"

	t.Run("a: a project with a default and an open ask answers the default", func(t *testing.T) {
		members := projectRead(t, brokerTestProject)
		requireDefaultAssignee(t, members, &bob)
		rows := openAskRowsOf(t, members)
		if len(rows) != 1 {
			t.Fatalf("%s answers %d open asks, want its one: %v", brokerTestProject, len(rows), rows)
		}
		var person string
		if err := json.Unmarshal(rows[0]["waiting_on_person"], &person); err != nil || person != "alice" {
			t.Fatalf("the open ask waits on %s (%v), want alice, the issue's assignee, so that the "+
				"default is not read off the ask", rows[0]["waiting_on_person"], err)
		}
	})

	t.Run("b: a project with a default and no open ask still answers the default", func(t *testing.T) {
		members := projectRead(t, "QUIET")
		if ids := askIDs(t, members); len(ids) != 0 {
			t.Fatalf("QUIET answers open asks %v, want none: its one question is answered", ids)
		}
		login := carol
		requireDefaultAssignee(t, members, &login)
	})

	t.Run("c: each project answers its own stored default and never another's", func(t *testing.T) {
		projects := []string{"QUIET", brokerTestProject, "NEXT"}
		defaults := map[string]string{}
		for _, project := range projects {
			defaults[project] = stored(t, project)
		}
		if distinct := slices.Compact(slices.Sorted(maps.Values(defaults))); len(distinct) != len(projects) {
			t.Fatalf("the projects store defaults %v, which do not differ, so a read answering "+
				"another project's default would pass", defaults)
		}
		for _, project := range projects {
			login := defaults[project]
			requireDefaultAssignee(t, projectRead(t, project), &login)
		}
	})

	t.Run("d: a key no project has answers null", func(t *testing.T) {
		members := projectRead(t, "NOPE")
		if ids := askIDs(t, members); len(ids) != 0 {
			t.Fatalf("NOPE answers open asks %v, want none", ids)
		}
		requireDefaultAssignee(t, members, nil)
	})

	t.Run("e: an author's read leaves the member out", func(t *testing.T) {
		author := sessionActor()["id"].(string)
		requireDefaultAssignee(t, projectRead(t, brokerTestProject), &bob)
		members := openAsksMembers(t, sessionRequest(t, handler, http.MethodGet,
			"/api/v1/asks/open?author_session="+url.QueryEscape(author), nil))
		if raw, carried := members["default_assignee"]; carried {
			t.Fatalf("the author's read answers default_assignee %s, want it left out: an author's "+
				"asks span projects", raw)
		}
		requireMembersBefore(t, members)
		want := []string{waiting, nextAsk}
		slices.Sort(want)
		if got := askIDs(t, members); !slices.Equal(got, want) {
			t.Fatalf("the author's read lists %v, want the two open asks %v", got, want)
		}
	})

	t.Run("f: a credential without the person scope reads the default and moves nothing", func(t *testing.T) {
		requireDefaultAssignee(t, projectRead(t, brokerTestProject), &bob)
		for _, credential := range []struct {
			name string
			call func(t *testing.T, method, target string, body any) *httptest.ResponseRecorder
		}{
			{"the daemon's broker token", asDaemon},
			{"the deployment's shared token", func(t *testing.T, method, target string, body any) *httptest.ResponseRecorder {
				return agentRequest(t, handler, method, target, body, "agent-token")
			}},
		} {
			listed := credential.call(t, http.MethodGet, "/api/v1/settings/project-default-assignees", nil)
			if listed.Code != http.StatusForbidden || !strings.Contains(listed.Body.String(), `"code":"`+codeScopeRequired+`"`) {
				t.Errorf("%s lists every project's default: status=%d body=%s, want 403 %s",
					credential.name, listed.Code, listed.Body.String(), codeScopeRequired)
			}
			moved := credential.call(t, http.MethodPut, "/api/v1/projects/"+brokerTestProject+"/default-assignee",
				map[string]any{"assignee": carol})
			if moved.Code != http.StatusForbidden || !strings.Contains(moved.Body.String(), `"code":"`+codeScopeRequired+`"`) {
				t.Errorf("%s moves %s's default: status=%d body=%s, want 403 %s",
					credential.name, brokerTestProject, moved.Code, moved.Body.String(), codeScopeRequired)
			}
		}
		if login := stored(t, brokerTestProject); login != bob {
			t.Fatalf("%s stores %q after the refused moves, want bob", brokerTestProject, login)
		}
	})

	t.Run("g: the answer with the member still decodes for an older reader", func(t *testing.T) {
		response := asDaemon(t, http.MethodGet, "/api/v1/asks/open?project="+brokerTestProject, nil)
		members := openAsksMembers(t, response)
		requireDefaultAssignee(t, members, &bob)
		requireMembersBefore(t, members)
		older := decodeBody[openAsksRead](t, response)
		if _, err := time.Parse(time.RFC3339Nano, older.AsOf); err != nil {
			t.Fatalf("an older reader reads as_of %q, not an RFC3339 time: %v", older.AsOf, err)
		}
		if older.SessionID != "" || older.OpenedSince || older.Count != 1 || older.WaitingOnHuman != 1 ||
			older.WaitingOnAgent != 0 || len(older.Asks) != 1 {
			t.Fatalf("an older reader reads %#v, want no session, one open ask waiting on a human", older)
		}
		row := older.Asks[0]
		if row.ID != waiting || row.Question != "Which way on the issue alice holds?" || row.WaitingOn != "human" ||
			row.Owner.Issue == nil || row.Owner.Issue.Key != held.Key {
			t.Fatalf("an older reader reads the row %#v, want the open ask on %s", row, held.Key)
		}
	})
}
