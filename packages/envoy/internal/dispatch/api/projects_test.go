package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

type projectResponse struct {
	Key             string `json:"key"`
	Name            string `json:"name"`
	DefaultAssignee string `json:"default_assignee"`
	CreatedAt       string `json:"created_at"`
}

func TestCreateProjectRejectsDuplicateKeyWithProjectExists(t *testing.T) {
	handler := newTestHandler(t)

	first := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "CORE", "name": "Core",
	}, "alice")
	if first.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", first.Code, first.Body.String())
	}
	created := decodeBody[projectResponse](t, first)
	if created.CreatedAt == "" {
		t.Fatalf("created project missing created_at: %#v", created)
	}

	duplicate := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "CORE", "name": "Core Again",
	}, "alice")
	if duplicate.Code != http.StatusConflict || !strings.Contains(duplicate.Body.String(), `"code":"PROJECT_EXISTS"`) {
		t.Fatalf("duplicate project: status=%d body=%s", duplicate.Code, duplicate.Body.String())
	}
}

// Every project names a person, whoever creates it and however the create is made: the column
// refuses nothing (store/migrations/20261111210103_projects_default_assignee_not_null.up.sql),
// because the person a question reaches ends at the project's default. The create resolves it in
// three steps — the login the call names, the person creating the project, the person its
// credential acts for — a caller that reaches the end of them is refused, whichever step answers
// must name somebody who has signed in, and no write clears a stored default.
func TestEveryProjectNamesAPerson(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)

	named := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "NAMED", "name": "Named", "default_assignee": " Bob ",
	}, "alice")
	if named.Code != http.StatusCreated {
		t.Fatalf("a create naming a default: status=%d body=%s", named.Code, named.Body.String())
	}
	if created := decodeBody[projectResponse](t, named); created.DefaultAssignee != "bob" {
		t.Fatalf("a create naming a default = %#v, want bob", created)
	}

	own := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "OWN", "name": "Own",
	}, "alice")
	if own.Code != http.StatusCreated {
		t.Fatalf("a person creating a project with no default: status=%d body=%s", own.Code, own.Body.String())
	}
	if created := decodeBody[projectResponse](t, own); created.DefaultAssignee != "alice" {
		t.Fatalf("a person's own project = %#v, want it defaulting to alice", created)
	}

	// A personal token proves no session of its own, so the call names one in actor; the person
	// it acts for is the token's owner, which is the second step the create falls through to.
	acting := agentRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]any{
		"key": "ACTS", "name": "Acts for bob", "actor": sessionActor(),
	}, personalTokenOwnedBy(t, database, "bob"))
	if acting.Code != http.StatusCreated {
		t.Fatalf("a session acting for a person: status=%d body=%s", acting.Code, acting.Body.String())
	}
	if created := decodeBody[projectResponse](t, acting); created.DefaultAssignee != "bob" {
		t.Fatalf("a session acting for bob created %#v, want it defaulting to bob", created)
	}

	nobody := sessionRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]any{
		"key": "NOBODY", "name": "For nobody", "actor": sessionActor(),
	})
	refusal := nobody.Body.String()
	if nobody.Code != http.StatusBadRequest || !strings.Contains(refusal, `"code":"INVALID_PROJECT"`) ||
		!strings.Contains(refusal, "default_assignee") {
		t.Fatalf("a session acting for nobody and naming none: status=%d body=%s, want 400 INVALID_PROJECT naming default_assignee", nobody.Code, refusal)
	}

	unknown := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "UNKNOWN", "name": "Unknown", "default_assignee": "mallory",
	}, "alice")
	if unknown.Code != http.StatusBadRequest || !strings.Contains(unknown.Body.String(), `"code":"ASSIGNEE_NOT_ALLOWED"`) {
		t.Fatalf("a create naming a login nobody has signed in with: status=%d body=%s, want 400 ASSIGNEE_NOT_ALLOWED", unknown.Code, unknown.Body.String())
	}

	// The person a credential acts for is held to the check a named login is: a broker-issued
	// token's owner comes from the broker rather than from the people table, so a derived login
	// nobody has signed in with is refused, never stored as the default. A personal token owned
	// by somebody who has never signed in stands in for it here.
	unseen := agentRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]any{
		"key": "UNSEEN", "name": "Acts for mallory", "actor": sessionActor(),
	}, personalTokenOwnedBy(t, database, "mallory"))
	if unseen.Code != http.StatusBadRequest || !strings.Contains(unseen.Body.String(), `"code":"ASSIGNEE_NOT_ALLOWED"`) {
		t.Fatalf("a session acting for a person nobody has signed in as: status=%d body=%s, want 400 ASSIGNEE_NOT_ALLOWED", unseen.Code, unseen.Body.String())
	}

	var refused int
	if err := database.Pool.QueryRow(context.Background(),
		`select count(*) from projects where key in ('NOBODY', 'UNKNOWN', 'UNSEEN')`).Scan(&refused); err != nil {
		t.Fatalf("count the refused projects: %v", err)
	}
	if refused != 0 {
		t.Fatalf("a refused create stored %d project(s)", refused)
	}

	cleared := dispatchRequest(t, handler, http.MethodPut, "/api/v1/projects/NAMED/default-assignee",
		map[string]any{"assignee": nil}, "alice")
	if cleared.Code != http.StatusBadRequest || !strings.Contains(cleared.Body.String(), `"code":"INVALID_PROJECT"`) {
		t.Fatalf("clearing a stored default: status=%d body=%s, want 400 INVALID_PROJECT", cleared.Code, cleared.Body.String())
	}
	var stored string
	if err := database.Pool.QueryRow(context.Background(),
		`select default_assignee from projects where key = 'NAMED'`).Scan(&stored); err != nil {
		t.Fatalf("read the stored default: %v", err)
	}
	if stored != "bob" {
		t.Fatalf("stored default = %q, want bob: a refused clear writes nothing", stored)
	}

	// And under every route, the column itself: a project with no default is not storable.
	_, err := database.Pool.Exec(context.Background(),
		`insert into projects (key, name) values ('DIRECT', 'Written straight to the table')`)
	if err == nil {
		t.Fatal("the database stored a project naming no default assignee")
	}
	if !strings.Contains(err.Error(), `null value in column "default_assignee"`) ||
		!strings.Contains(err.Error(), "23502") {
		t.Fatalf("a direct insert with no default was refused with %v, want the not-null violation on default_assignee", err)
	}
}

// An agent session creates a project with its bearer, as it creates an issue: the project is
// listed like any other, and its project.created event names the session that created it. The
// session acts for nobody, so the call names the person the project's unowned work lands on,
// which every project has (createProject, projects.go); the create's answer names that person,
// and the list does not, since the person-scoped GET /api/v1/settings/project-default-assignees
// is the one read of a project's default.
func TestASessionBearerCreatesAProject(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)

	created := sessionRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]any{
		"key": "AGENT", "name": "Agent project", "default_assignee": "alice", "actor": sessionActor(),
	})
	if created.Code != http.StatusCreated {
		t.Fatalf("a session creates a project: status=%d body=%s", created.Code, created.Body.String())
	}
	project := decodeBody[projectResponse](t, created)
	if project.Key != "AGENT" || project.Name != "Agent project" || project.DefaultAssignee != "alice" || project.CreatedAt == "" {
		t.Fatalf("created project = %#v", project)
	}

	var actor []byte
	if err := database.Pool.QueryRow(context.Background(), `
		select actor from events where type = 'project.created' and project_key = 'AGENT'
	`).Scan(&actor); err != nil {
		t.Fatalf("read the project.created event: %v", err)
	}
	var by model.Actor
	if err := json.Unmarshal(actor, &by); err != nil {
		t.Fatalf("decode the project.created actor %s: %v", actor, err)
	}
	if by.Kind != "session" || by.ID != sessionActor()["id"] {
		t.Fatalf("project.created actor = %s, want the creating session", actor)
	}

	listed := sessionRequest(t, handler, http.MethodGet, "/api/v1/projects", nil)
	if listed.Code != http.StatusOK {
		t.Fatalf("list projects: status=%d body=%s", listed.Code, listed.Body.String())
	}
	projects := decodeBody[[]projectResponse](t, listed)
	if len(projects) != 1 || projects[0].Key != "AGENT" || projects[0].Name != "Agent project" {
		t.Fatalf("listed projects = %#v, want the session's AGENT", projects)
	}
	if listedProjects := decodeBody[[]map[string]any](t, listed); len(listedProjects) != 1 {
		t.Fatalf("listed projects = %#v, want one", listedProjects)
	} else if _, carried := listedProjects[0]["default_assignee"]; carried {
		t.Fatalf("the project list carried default_assignee: %s", listed.Body.String())
	}
}

// A key that already exists is refused whoever asks: a session's bearer and a signed-in person's
// cookie each get 409 PROJECT_EXISTS, and the project keeps the name it was created with. The
// bearer acts for nobody, so it names the person whose cookie this server issues; that person is
// seeded here because establishing their session records no `people` row (newCookieServerOn),
// and a create names somebody who has signed in.
func TestCreatingATakenProjectKeyIsRefusedForABearerAndACookie(t *testing.T) {
	database := storetest.Open(t)
	seedPeople(t, database, "alice@d.example")
	handler, _, _, withCookie, _ := newCookieServerOn(t, database, 0)

	if first := sessionRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]any{
		"key": "TAKEN", "name": "First", "default_assignee": "alice@d.example", "actor": sessionActor(),
	}); first.Code != http.StatusCreated {
		t.Fatalf("create TAKEN: status=%d body=%s", first.Code, first.Body.String())
	}
	for _, attempt := range []struct {
		caller   string
		response *httptest.ResponseRecorder
	}{
		{"bearer", sessionRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]any{
			"key": "TAKEN", "name": "Again", "default_assignee": "alice@d.example", "actor": sessionActor(),
		})},
		{"cookie", withCookie(http.MethodPost, "/api/v1/projects", map[string]string{"key": "TAKEN", "name": "Again"})},
	} {
		if attempt.response.Code != http.StatusConflict || !strings.Contains(attempt.response.Body.String(), `"code":"PROJECT_EXISTS"`) {
			t.Fatalf("a %s creating a taken key: status=%d body=%s", attempt.caller, attempt.response.Code, attempt.response.Body.String())
		}
	}

	listed := sessionRequest(t, handler, http.MethodGet, "/api/v1/projects", nil)
	projects := decodeBody[[]projectResponse](t, listed)
	if len(projects) != 1 || projects[0].Name != "First" {
		t.Fatalf("listed projects = %#v, want TAKEN still named First", projects)
	}
}

// Every route that takes a project key refuses a bad one with the same description of the rule,
// so an agent learns the rule once, whichever route it called. The rule is contracts'
// PROJECT_KEY_RULE, generated into Go, which the dispatch command's own refusal gives too.
func TestEveryRouteRefusesABadProjectKeyInOneWording(t *testing.T) {
	handler := newTestHandler(t)
	rule := contracts.ProjectKeyRule
	for _, probe := range []struct {
		name, method, target, code string
		body                       any
	}{
		{"create project", http.MethodPost, "/api/v1/projects", "INVALID_PROJECT", map[string]string{"key": "bad", "name": "Bad"}},
		{"create issue", http.MethodPost, "/api/v1/issues", "INVALID_ISSUE", map[string]string{"project": "bad", "title": "Title"}},
		{"map a repository", http.MethodPut, "/api/v1/settings/repo-projects/owner/repo", "INVALID_REPO_PROJECT", map[string]string{"project": "bad"}},
		{"search", http.MethodGet, "/api/v1/search?q=ok&project=bad", "INVALID_PROJECT", nil},
		{"related-item check", http.MethodGet, "/api/v1/projects/bad/related?title=ok", "INVALID_PROJECT", nil},
		{"related-item check by POST", http.MethodPost, "/api/v1/projects/bad/related", "INVALID_PROJECT", map[string]string{"title": "ok"}},
		{"related-check records", http.MethodGet, "/api/v1/related-checks?project=bad", "INVALID_PROJECT", nil},
	} {
		response := dispatchRequest(t, handler, probe.method, probe.target, probe.body, "alice")
		body := response.Body.String()
		if response.Code != http.StatusBadRequest || !strings.Contains(body, `"code":"`+probe.code+`"`) || !strings.Contains(body, rule) {
			t.Errorf("%s with key \"bad\": status=%d body=%s, want 400 %s describing the rule %q", probe.name, response.Code, body, probe.code, rule)
		}
	}
}

// A bad key or an empty name is 400 INVALID_PROJECT for a session as for a person, and the
// refusal names which of the two is wrong: an agent reads it as the whole answer.
func TestAnInvalidProjectIsRefusedForABearerAndAPerson(t *testing.T) {
	handler := newTestHandler(t)
	badKey := func(key string) string {
		return fmt.Sprintf(`key \"%s\" is not a project key: %s`, key, contracts.ProjectKeyRule)
	}
	for _, input := range []struct{ name, key, projectName, says string }{
		{"lowercase key", "agent", "Agent project", badKey("agent")},
		{"one-character key", "A", "Agent project", badKey("A")},
		{"key starting with a digit", "1AGENT", "Agent project", badKey("1AGENT")},
		{"key over ten characters", "ABCDEFGHIJK", "Agent project", badKey("ABCDEFGHIJK")},
		{"empty name", "AGENT", "", "project name is required"},
		{"blank name", "AGENT", "   ", "project name is required"},
		{"a bidi override", "AGENT", "Agent \u202eproject", `name holds U+202E, a bidi control that reorders the text a reader sees`},
		{"a bidi isolate", "AGENT", "Agent \u2066project", `name holds U+2066, a bidi control that reorders the text a reader sees`},
		{"zero-width characters only", "AGENT", "\u200b\u200d", "name has no visible character"},
		{"zero-width characters and a space only", "AGENT", "\u200b \u2060", "name has no visible character"},
		{"a control character", "AGENT", "Agent\u0007project", "name holds U+0007, a control character"},
		{"a Hangul filler", "AGENT", "\u3164", "name has no visible character"},
		{"Hangul choseong and jungseong fillers", "AGENT", "\u115f\u1160", "name has no visible character"},
		{"a halfwidth Hangul filler, a space and a filler", "AGENT", "\uffa0 \u3164", "name has no visible character"},
		{"Braille pattern blanks", "AGENT", "\u2800\u2800", "name has no visible character"},
		{"a bare combining mark and a variation selector", "AGENT", "\u0301\ufe0f", "name has no visible character"},
		{"a spacing combining mark alone", "AGENT", "\u093e", "name has no visible character"},
		// Characters that stay invisible however new the reader's Unicode is: a reserved
		// default-ignorable (U+2065, U+FFF0, U+E0000-U+E0FFF) renders as nothing, and a
		// noncharacter is never assigned. U+E0080 is unassigned; the language tag U+E0001 beside
		// it is an assigned format character.
		{"a reserved default-ignorable in the general punctuation block", "AGENT", "\u2065", "name has no visible character"},
		{"a reserved default-ignorable before the specials", "AGENT", "\ufff0", "name has no visible character"},
		{"a reserved default-ignorable in the tag block", "AGENT", "\U000E0080", "name has no visible character"},
		{"a language tag", "AGENT", "\U000E0001", "name has no visible character"},
		{"a noncharacter", "AGENT", "\ufdd0", "name has no visible character"},
	} {
		t.Run(input.name, func(t *testing.T) {
			for _, attempt := range []struct {
				caller   string
				response *httptest.ResponseRecorder
			}{
				{"bearer", sessionRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]any{
					"key": input.key, "name": input.projectName, "actor": sessionActor(),
				})},
				{"person", dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
					"key": input.key, "name": input.projectName,
				}, "alice")},
			} {
				body := attempt.response.Body.String()
				if attempt.response.Code != http.StatusBadRequest || !strings.Contains(body, `"code":"INVALID_PROJECT"`) || !strings.Contains(body, input.says) {
					t.Fatalf("a %s creating key %q name %q: status=%d body=%s, want 400 INVALID_PROJECT saying %s", attempt.caller, input.key, input.projectName, attempt.response.Code, body, input.says)
				}
			}
		})
	}
}

// A name a reader sees whole is accepted even where it holds a format character doing visible
// work - a joiner building an emoji, a flag's tag characters, a direction mark in mixed text, a
// soft hyphen - or a combining mark on a letter, and in any script, Hangul included. No route
// renames a project, so refusing one leaves its maker no way to the name they meant.
func TestAVisibleNameWithFormatCharactersIsAcceptedForABearerAndAPerson(t *testing.T) {
	handler := newTestHandler(t)
	for index, name := range []string{
		"Team 👨\u200d👩\u200d👧\u200d👦",
		"👩\u200d💻 Squad",
		"🏴\U000E0067\U000E0062\U000E0065\U000E006E\U000E0067\U000E007F Team",
		"Project שלום\u200f",
		"Super\u00adcalifragilistic",
		"한국 팀",
		"cafe\u0301",
		"Cafe\u0301",
		// Scripts whose names lean on combining marks and conjoining letters: Devanagari, Thai
		// with a tone mark, and a Hangul syllable written as its conjoining jamo.
		"नमस्ते",
		"น้ำ",
		"\u1112\u1161\u11ab",
		// A character from a recent Unicode release (U+1FAE9, Unicode 16, sent by phones now): a
		// reader's device draws it, so it is not "no visible character".
		"🫩",
		"Team 🫩",
	} {
		bearer := sessionRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]any{
			"key": fmt.Sprintf("VISB%d", index), "name": name,
			"default_assignee": "alice", "actor": sessionActor(),
		})
		if bearer.Code != http.StatusCreated {
			t.Errorf("a bearer creating name %q: status=%d body=%s, want 201", name, bearer.Code, bearer.Body.String())
		}
		person := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
			"key": fmt.Sprintf("VISP%d", index), "name": name,
		}, "alice")
		if person.Code != http.StatusCreated {
			t.Errorf("a person creating name %q: status=%d body=%s, want 201", name, person.Code, person.Body.String())
		}
	}
}

// The 🫩 row above tests the unassigned-code-point rule only while Go's tables predate Unicode 16;
// Go 1.27's assign U+1FAE9 as a symbol. This test finds, in the running Go's own tables, a code
// point of plane 3 (outside the noncharacter and default-ignorable ranges) they leave unassigned,
// so it keeps testing that rule on whatever toolchain runs it, and fails if none is left to find.
func TestACodePointGoLeavesUnassignedIsVisibleForABearerAndAPerson(t *testing.T) {
	unassigned := rune(-1)
	for candidate := rune(0x30000); candidate <= 0x3FFFD; candidate++ {
		if unicode.Is(unicode.Cn, candidate) {
			unassigned = candidate
			break
		}
	}
	if unassigned < 0 {
		t.Fatal("Go's Unicode tables assign every code point in U+30000-U+3FFFD; pick another unassigned range for this test")
	}
	if !visibleCharacter(unassigned) {
		t.Fatalf("visibleCharacter(U+%04X) = false for a code point Go's Unicode %s tables leave unassigned", unassigned, unicode.Version)
	}
	handler := newTestHandler(t)
	name := string(unassigned)
	if bearer := sessionRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]any{
		"key": "UNASB", "name": name, "default_assignee": "alice", "actor": sessionActor(),
	}); bearer.Code != http.StatusCreated {
		t.Errorf("a bearer creating name U+%04X: status=%d body=%s, want 201", unassigned, bearer.Code, bearer.Body.String())
	}
	if person := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "UNASP", "name": name,
	}, "alice"); person.Code != http.StatusCreated {
		t.Errorf("a person creating name U+%04X: status=%d body=%s, want 201", unassigned, person.Code, person.Body.String())
	}
}

// A project name is capped as an issue title is: any bearer can write one into the project list
// every dashboard reads, and no route renames a project. The cap counts UTF-16 units after the
// trim, so a name at it is created and one unit over is CAP_EXCEEDED, for a session as for a person.
func TestAProjectNameOverTheCapIsCapExceededForABearerAndAPerson(t *testing.T) {
	handler := newTestHandler(t)
	for _, caller := range []struct {
		name, key string
		send      func(body map[string]any) *httptest.ResponseRecorder
	}{
		{"bearer", "CAPB", func(body map[string]any) *httptest.ResponseRecorder {
			body["actor"] = sessionActor()
			// The session acts for nobody, so it names the person the project defaults to.
			body["default_assignee"] = "alice"
			return sessionRequest(t, handler, http.MethodPost, "/api/v1/projects", body)
		}},
		{"person", "CAPP", func(body map[string]any) *httptest.ResponseRecorder {
			return dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", body, "alice")
		}},
	} {
		for name, test := range overCapCases(contracts.ProjectNameMax) {
			refused := caller.send(map[string]any{"key": caller.key, "name": "  " + test.text + "  "})
			assertCapExceeded(t, caller.name+" "+name, refused, "name", test.length, contracts.ProjectNameMax, "")
		}
		atCap := "  " + strings.Repeat("n", contracts.ProjectNameMax) + "  "
		if created := caller.send(map[string]any{"key": caller.key, "name": atCap}); created.Code != http.StatusCreated {
			t.Fatalf("a %s's name at the cap: status=%d body=%s, want 201", caller.name, created.Code, created.Body.String())
		}
	}
}

func TestListProjectsIncludesCreatedAt(t *testing.T) {
	handler := newTestHandler(t)

	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "CORE", "name": "Core",
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", response.Code, response.Body.String())
	}

	listed := dispatchRequest(t, handler, http.MethodGet, "/api/v1/projects", nil, "alice")
	if listed.Code != http.StatusOK {
		t.Fatalf("list projects: status=%d body=%s", listed.Code, listed.Body.String())
	}
	projects := decodeBody[[]projectResponse](t, listed)
	if len(projects) != 1 || projects[0].Key != "CORE" || projects[0].CreatedAt == "" {
		t.Fatalf("listed projects: got %#v", projects)
	}
}

func TestProjectAndSettingsMutationsAppendLiveEvents(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "CORE", "name": "Core",
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", response.Code, response.Body.String())
	}
	if response := dispatchRequest(t, handler, http.MethodPut, "/api/v1/settings/repo-projects/owner/repo", map[string]string{
		"project": "CORE",
	}, "alice"); response.Code != http.StatusOK {
		t.Fatalf("map repository: status=%d body=%s", response.Code, response.Body.String())
	}
	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{
		"project": "CORE", "title": "State owner",
	}, "alice")
	issue := decodeBody[struct {
		Key string `json:"key"`
	}](t, created)
	if created.Code != http.StatusCreated {
		t.Fatalf("create issue: status=%d body=%s", created.Code, created.Body.String())
	}
	if response := dispatchRequest(t, handler, http.MethodPut, "/api/v1/me/issues/"+issue.Key+"/state", map[string]any{
		"pinned": true,
	}, "alice"); response.Code != http.StatusOK {
		t.Fatalf("write user state: status=%d body=%s", response.Code, response.Body.String())
	}

	rows, err := database.Pool.Query(t.Context(), `
		select type, coalesce(project_key, ''), coalesce(issue_key, ''), notify
		from events
		where type in ('project.created', 'settings.repo_project.updated', 'user_state.updated')
		order by id
	`)
	if err != nil {
		t.Fatalf("read settings events: %v", err)
	}
	defer rows.Close()
	type persistedEvent struct {
		Type    string
		Project string
		Issue   string
		Notify  bool
	}
	var got []persistedEvent
	for rows.Next() {
		var event persistedEvent
		if err := rows.Scan(&event.Type, &event.Project, &event.Issue, &event.Notify); err != nil {
			t.Fatalf("scan settings event: %v", err)
		}
		got = append(got, event)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate settings events: %v", err)
	}
	want := []persistedEvent{
		{Type: "project.created", Project: "CORE"},
		{Type: "settings.repo_project.updated", Project: "CORE"},
		{Type: "user_state.updated", Project: "CORE"},
	}
	if len(got) != len(want) {
		t.Fatalf("settings events = %#v, want %#v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("settings event %d = %#v, want %#v", index, got[index], want[index])
		}
	}
}

func TestListProjectsCountsOpenAsksOnIssuesAndDocuments(t *testing.T) {
	handler := newTestHandler(t)
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "CORE", "name": "Core",
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", response.Code, response.Body.String())
	}
	issueResponse := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{
		"project": "CORE", "title": "Open issue",
	}, "alice")
	if issueResponse.Code != http.StatusCreated {
		t.Fatalf("create open issue: status=%d body=%s", issueResponse.Code, issueResponse.Body.String())
	}
	openIssue := decodeBody[model.Issue](t, issueResponse)
	if response := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+openIssue.Key+"/asks", map[string]any{
		"question": "Issue ask", "actor": sessionActor(),
	}); response.Code != http.StatusCreated {
		t.Fatalf("create issue ask: status=%d body=%s", response.Code, response.Body.String())
	}
	document := createProjectDocument(t, handler, "CORE", "Design notes", "# Design notes\n")
	openDocumentAsk := sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+document.ID+"/asks", map[string]any{
		"question": "Document ask", "actor": sessionActor(),
	})
	if openDocumentAsk.Code != http.StatusCreated {
		t.Fatalf("create document ask: status=%d body=%s", openDocumentAsk.Code, openDocumentAsk.Body.String())
	}
	answeredDocumentAsk := sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+document.ID+"/asks", map[string]any{
		"question": "Answered document ask", "actor": sessionActor(),
	})
	if answeredDocumentAsk.Code != http.StatusCreated {
		t.Fatalf("create answered document ask: status=%d body=%s", answeredDocumentAsk.Code, answeredDocumentAsk.Body.String())
	}
	answered := decodeBody[model.Ask](t, answeredDocumentAsk)
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+answered.ID+"/answer", map[string]string{"text": "Done"}, "alice"); response.Code != http.StatusOK {
		t.Fatalf("answer document ask: status=%d body=%s", response.Code, response.Body.String())
	}
	closedResponse := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{
		"project": "CORE", "title": "Closed issue",
	}, "alice")
	if closedResponse.Code != http.StatusCreated {
		t.Fatalf("create closed issue: status=%d body=%s", closedResponse.Code, closedResponse.Body.String())
	}
	closedIssue := decodeBody[model.Issue](t, closedResponse)
	if response := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+closedIssue.Key+"/asks", map[string]any{
		"question": "Closed issue ask", "actor": sessionActor(),
	}); response.Code != http.StatusCreated {
		t.Fatalf("create closed issue ask: status=%d body=%s", response.Code, response.Body.String())
	}
	if response := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+closedIssue.Key, map[string]string{"status": "done"}, "alice"); response.Code != http.StatusOK {
		t.Fatalf("close issue: status=%d body=%s", response.Code, response.Body.String())
	}

	listed := dispatchRequest(t, handler, http.MethodGet, "/api/v1/projects", nil, "alice")
	if listed.Code != http.StatusOK {
		t.Fatalf("list projects: status=%d body=%s", listed.Code, listed.Body.String())
	}
	projects := decodeBody[[]model.Project](t, listed)
	if len(projects) != 1 || projects[0].Key != "CORE" || projects[0].OpenAsks != 2 {
		t.Fatalf("listed projects = %#v, want CORE with two open asks", projects)
	}
}
