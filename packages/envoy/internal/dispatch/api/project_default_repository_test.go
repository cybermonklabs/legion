package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/deliveryseed"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// setDefaultRepository sets a project's default repository as a person does, through PUT
// /api/v1/projects/{key}/default-repository. repo is an `owner/name` string, or nil to clear the
// default. A write the route refuses fails the test, so a setup that did not happen cannot pass
// for one that did.
func setDefaultRepository(t *testing.T, handler http.Handler, project string, repo any) projectDefaultRepository {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodPut, "/api/v1/projects/"+project+"/default-repository",
		map[string]any{"repo": repo}, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("set %s's default repository to %v: status=%d body=%s", project, repo, response.Code, response.Body.String())
	}
	return decodeBody[projectDefaultRepository](t, response)
}

// listedDefaultRepository reads one project's default_repo member off GET /api/v1/projects as
// the bytes the list sends, and whether the member is there at all: a project with no default
// carries no member, not a null one.
func listedDefaultRepository(t *testing.T, handler http.Handler, project string) (string, bool) {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/projects", nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("list projects: status=%d body=%s", response.Code, response.Body.String())
	}
	for _, row := range decodeBody[[]map[string]json.RawMessage](t, response) {
		var key string
		if err := json.Unmarshal(row["key"], &key); err != nil {
			t.Fatalf("decode a listed project's key %s: %v", row["key"], err)
		}
		if key != project {
			continue
		}
		raw, present := row["default_repo"]
		return string(raw), present
	}
	t.Fatalf("project %s is not in the list: %s", project, response.Body.String())
	return "", false
}

// lastProjectUpdate is the payload of the project's newest project.updated event, member by
// member, so a test can tell a member that is absent from one that is null.
func lastProjectUpdate(t *testing.T, database *store.Store, project string) map[string]json.RawMessage {
	t.Helper()
	var payload []byte
	if err := database.Pool.QueryRow(context.Background(), `
		select payload from events where project_key = $1 and type = 'project.updated'
		order by id desc limit 1
	`, project).Scan(&payload); err != nil {
		t.Fatalf("read %s's newest project event: %v", project, err)
	}
	var members map[string]json.RawMessage
	if err := json.Unmarshal(payload, &members); err != nil {
		t.Fatalf("decode %s's project event %s: %v", project, payload, err)
	}
	return members
}

// A project's default repository is set, read back off the project list, written again with the
// same value, and cleared. The value is stored in the canonical form an issue's own repository
// takes. A change appends one project.updated that carries the project row, and a write of the
// value the row holds appends none. A project with no default carries no default_repo member, in
// the list and in the event alike.
func TestAProjectsDefaultRepositoryIsSetAndCleared(t *testing.T) {
	t.Parallel()
	handler, database := newTestHandlerWithStore(t)
	createTestProject(t, handler, "CORE", "BARE")

	if answer := setDefaultRepository(t, handler, "CORE", "Acme/Widgets.git"); answer.Project != "CORE" ||
		answer.Repo == nil || *answer.Repo != "acme/widgets" {
		t.Fatalf("the set answered %+v, want CORE holding acme/widgets", answer)
	}
	if listed, present := listedDefaultRepository(t, handler, "CORE"); !present || listed != `"acme/widgets"` {
		t.Fatalf("CORE lists default_repo %s (present %v), want \"acme/widgets\"", listed, present)
	}
	if listed, present := listedDefaultRepository(t, handler, "BARE"); present {
		t.Fatalf("BARE lists default_repo %s, want no member for a project with no default", listed)
	}
	if updates := projectUpdates(t, database, "CORE"); updates != 1 {
		t.Fatalf("the set appended %d project events, want one", updates)
	}
	event := lastProjectUpdate(t, database, "CORE")
	if string(event["key"]) != `"CORE"` || string(event["default_repo"]) != `"acme/widgets"` {
		t.Fatalf("the project event carries %v, want CORE's row holding acme/widgets", event)
	}
	if _, present := event["gated_steps"]; !present {
		t.Fatalf("the project event carries %v, want the whole project row with its gated_steps", event)
	}

	// The same value again, in another spelling of it, changes no row and appends no event.
	if answer := setDefaultRepository(t, handler, "CORE", "acme/widgets"); answer.Repo == nil || *answer.Repo != "acme/widgets" {
		t.Fatalf("the unchanged write answered %+v, want CORE still holding acme/widgets", answer)
	}
	if updates := projectUpdates(t, database, "CORE"); updates != 1 {
		t.Fatalf("an unchanged write left %d project events, want the one already there", updates)
	}

	if answer := setDefaultRepository(t, handler, "CORE", nil); answer.Project != "CORE" || answer.Repo != nil {
		t.Fatalf("the clear answered %+v, want CORE holding no default", answer)
	}
	if listed, present := listedDefaultRepository(t, handler, "CORE"); present {
		t.Fatalf("CORE lists default_repo %s after the clear, want no member", listed)
	}
	if updates := projectUpdates(t, database, "CORE"); updates != 2 {
		t.Fatalf("the clear left %d project events, want two", updates)
	}
	if cleared, present := lastProjectUpdate(t, database, "CORE")["default_repo"]; present {
		t.Fatalf("the clear's project event carries default_repo %s, want no member", cleared)
	}
	setDefaultRepository(t, handler, "CORE", nil)
	if updates := projectUpdates(t, database, "CORE"); updates != 2 {
		t.Fatalf("a second clear left %d project events, want the two already there", updates)
	}
	if updates := projectUpdates(t, database, "BARE"); updates != 0 {
		t.Fatalf("BARE holds %d project events, want none: no write named it", updates)
	}
}

// A body that names no repository, or names something GitHub could hold no repository at, is
// refused naming repo, and the project keeps the default it had. A body without repo is not
// read as a clear, so a malformed write cannot remove a default. An unknown project is 404.
func TestADefaultRepositoryThatIsNoRepositoryIsRefused(t *testing.T) {
	t.Parallel()
	handler, database := newTestHandlerWithStore(t)
	createTestProject(t, handler, "CORE")
	setDefaultRepository(t, handler, "CORE", "acme/widgets")

	for name, body := range map[string]map[string]any{
		"a body without repo":   {},
		"a name with no owner":  {"repo": "acme"},
		"a name with two slash": {"repo": "a/b/c"},
		"a path out of a tree":  {"repo": "../x"},
	} {
		response := dispatchRequest(t, handler, http.MethodPut, "/api/v1/projects/CORE/default-repository", body, "alice")
		code, text := refusalOf(t, response)
		if response.Code != http.StatusBadRequest || code != "INVALID_PROJECT" {
			t.Errorf("%s: status=%d body=%s, want 400 INVALID_PROJECT", name, response.Code, response.Body.String())
		}
		if !strings.Contains(text, "repo") {
			t.Errorf("%s: the refusal %q does not name repo", name, text)
		}
	}
	if listed, present := listedDefaultRepository(t, handler, "CORE"); !present || listed != `"acme/widgets"` {
		t.Fatalf("CORE lists default_repo %s (present %v) after the refusals, want \"acme/widgets\"", listed, present)
	}
	if updates := projectUpdates(t, database, "CORE"); updates != 1 {
		t.Fatalf("the refusals left %d project events, want the set's one", updates)
	}

	unknown := dispatchRequest(t, handler, http.MethodPut, "/api/v1/projects/NOPE/default-repository",
		map[string]any{"repo": "acme/widgets"}, "alice")
	if code, _ := refusalOf(t, unknown); unknown.Code != http.StatusNotFound || code != "PROJECT_NOT_FOUND" {
		t.Fatalf("an unknown project: status=%d body=%s, want 404 PROJECT_NOT_FOUND", unknown.Code, unknown.Body.String())
	}
}

// The default belongs to the deployment: a credential that names one project is refused it by
// name, though the path names that very project. A person writes it, and so does the
// credential the deployment configured, which acts for no person and names no project.
func TestTheDefaultRepositoryIsADeploymentWrite(t *testing.T) {
	t.Parallel()
	rig := newBrokerTokenRig(t)
	caller := newBrokerCaller(t)
	const token = "a-token-that-may-not-move-a-projects-default-repository"
	rig.broker.Says(token, liveNamedToken(caller.jkt))
	seedProject(t, rig, brokerTestProject, "Broker")
	path := "/api/v1/projects/" + brokerTestProject + "/default-repository"

	refused := rig.serve(caller.presenting(t, http.MethodPut, path, token, map[string]any{"repo": "acme/widgets"}))
	if refused.Code != http.StatusForbidden {
		t.Fatalf("a project-scoped token: status=%d body=%s, want 403", refused.Code, refused.Body.String())
	}
	refusal := decodeBody[refusalAnswer](t, refused)
	if refusal.Code != codeProjectScopeDeployment {
		t.Fatalf("refusal = %+v, want %s", refusal, codeProjectScopeDeployment)
	}
	if !strings.Contains(refusal.Error, brokerTestProject) || !strings.Contains(refusal.Error, "PUT /api/v1/projects/{key}/default-repository") {
		t.Fatalf("refusal %q names neither the project the credential covers nor the route", refusal.Error)
	}
	if listed, present := listedDefaultRepository(t, rig.handler, brokerTestProject); present {
		t.Fatalf("the refused write left default_repo %s, want none", listed)
	}

	person := rig.asPerson(t, http.MethodPut, path, map[string]any{"repo": "acme/widgets"})
	if person.Code != http.StatusOK {
		t.Fatalf("a person: status=%d body=%s, want 200", person.Code, person.Body.String())
	}
	if answer := decodeBody[projectDefaultRepository](t, person); answer.Repo == nil || *answer.Repo != "acme/widgets" {
		t.Fatalf("a person's write answered %+v, want acme/widgets", answer)
	}

	deployment := agentRequest(t, rig.handler, http.MethodPut, path,
		map[string]any{"repo": "acme/gadgets", "actor": sessionActor()}, "agent-token")
	if deployment.Code != http.StatusOK {
		t.Fatalf("the deployment's token: status=%d body=%s, want 200", deployment.Code, deployment.Body.String())
	}
	if listed, _ := listedDefaultRepository(t, rig.handler, brokerTestProject); listed != `"acme/gadgets"` {
		t.Fatalf("after the deployment's write the project lists default_repo %s, want \"acme/gadgets\"", listed)
	}
	var actor string
	if err := rig.database.Pool.QueryRow(context.Background(), `
		select actor->>'id' from events where project_key = $1 and type = 'project.updated'
		order by id desc limit 1
	`, brokerTestProject).Scan(&actor); err != nil {
		t.Fatalf("read the deployment write's event: %v", err)
	}
	if actor != sessionActor()["id"] {
		t.Fatalf("the deployment write's event names actor %q, want the session it named", actor)
	}
}

// An issue that names no repository, under no issue that names one, resolves to its project's
// default; an issue that names its own keeps it; and a child takes the repository of the issue
// above it before the default. The resolution reads the default as it stands, so a moved default
// moves every issue that takes it and no other, and a cleared one leaves them with none.
func TestAnIssueResolvesToItsProjectsDefaultRepository(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)
	createTestProject(t, handler, "CORE")
	setDefaultRepository(t, handler, "CORE", "acme/widgets")

	create := func(body map[string]any) string {
		t.Helper()
		body["project"] = "CORE"
		response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", body, "alice")
		if response.Code != http.StatusCreated {
			t.Fatalf("create %v: status=%d body=%s", body["title"], response.Code, response.Body.String())
		}
		return decodeBody[model.Issue](t, response).Key
	}
	plain := create(map[string]any{"title": "Names no repository"})
	plainChild := create(map[string]any{"title": "Under one that names none", "parent": plain})
	owning := create(map[string]any{"title": "Names its own", "repo": "acme/gadgets"})
	owningChild := create(map[string]any{"title": "Under one that names its own", "parent": owning})

	resolved := func(key string) string {
		t.Helper()
		response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+key, nil, "alice")
		if response.Code != http.StatusOK {
			t.Fatalf("read %s: status=%d body=%s", key, response.Code, response.Body.String())
		}
		return repoName(decodeBody[model.Issue](t, response).ResolvedRepo)
	}
	listed := func() map[string]string {
		t.Helper()
		response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues?project=CORE", nil, "alice")
		if response.Code != http.StatusOK {
			t.Fatalf("list CORE: status=%d body=%s", response.Code, response.Body.String())
		}
		answers := map[string]string{}
		for _, issue := range decodeBody[[]model.IssueSummary](t, response) {
			answers[issue.Key] = repoName(issue.ResolvedRepo)
		}
		return answers
	}
	for _, step := range []struct {
		name    string
		repo    any
		answers map[string]string
	}{
		{"the default set", "acme/widgets", map[string]string{
			plain: "acme/widgets", plainChild: "acme/widgets", owning: "acme/gadgets", owningChild: "acme/gadgets",
		}},
		{"the default moved", "acme/tools", map[string]string{
			plain: "acme/tools", plainChild: "acme/tools", owning: "acme/gadgets", owningChild: "acme/gadgets",
		}},
		{"the default cleared", nil, map[string]string{
			plain: "none", plainChild: "none", owning: "acme/gadgets", owningChild: "acme/gadgets",
		}},
	} {
		setDefaultRepository(t, handler, "CORE", step.repo)
		list := listed()
		for key, want := range step.answers {
			if got := resolved(key); got != want {
				t.Errorf("%s: %s reads resolved_repo %s, want %s", step.name, key, got, want)
			}
			if got := list[key]; got != want {
				t.Errorf("%s: %s lists resolved_repo %s, want %s", step.name, key, got, want)
			}
		}
	}
}

// repoName renders a resolved repository in a failure message, "none" for no repository.
func repoName(repo *string) string {
	if repo == nil {
		return "none"
	}
	return *repo
}

// A repository given under the name a rename pair renames away is stored under its new name, so
// no write of the route stores an old name. A name that only begins like the old one is no name
// of the pair and is stored as it is given.
func TestADefaultRepositoryUnderARenamedNameIsStoredUnderItsNewName(t *testing.T) {
	t.Parallel()
	handler, database, _ := newTestServer(t, testServerOptions{
		renames: refs.NewRenames([]store.RepoRename{{From: "sjawhar/legion", To: "cybermonklabs/legion"}}),
	})
	createTestProject(t, handler, "INFRA", "SMOKE")

	if answer := setDefaultRepository(t, handler, "INFRA", "SJawhar/Legion"); answer.Repo == nil || *answer.Repo != "cybermonklabs/legion" {
		t.Fatalf("the write of sjawhar/legion answered %+v, want cybermonklabs/legion", answer)
	}
	if answer := setDefaultRepository(t, handler, "SMOKE", "sjawhar/legion-smoke"); answer.Repo == nil || *answer.Repo != "sjawhar/legion-smoke" {
		t.Fatalf("the write of sjawhar/legion-smoke answered %+v, want it as given", answer)
	}
	for project, want := range map[string]string{"INFRA": "cybermonklabs/legion", "SMOKE": "sjawhar/legion-smoke"} {
		var stored string
		if err := database.Pool.QueryRow(context.Background(),
			`select default_repo from projects where key = $1`, project).Scan(&stored); err != nil {
			t.Fatalf("read %s's stored default: %v", project, err)
		}
		if stored != want {
			t.Fatalf("%s stores default_repo %q, want %q", project, stored, want)
		}
	}
}

// dispatch://INFRA-4411 drops repo_projects. Nothing this test drives reads the table: the
// route writes the project's own column, the issue reads resolve the default from it, and a
// results read resolves the check's issue to it, compares at that repository and answers 200.
func TestAResultsReadResolvesTheDefaultRepositoryWithRepoProjectsDropped(t *testing.T) {
	t.Parallel()
	handler, database := resultsGitHub(t, map[string]bool{"acme/widgets": true}, func(base, head string) string {
		if base == "main" && head == mergeCommit {
			return "behind"
		}
		return "diverged"
	})
	if _, err := database.Pool.Exec(context.Background(), `drop table repo_projects`); err != nil {
		t.Fatalf("drop repo_projects: %v", err)
	}
	createTestProject(t, handler, "RESX")
	setDefaultRepository(t, handler, "RESX", "acme/widgets")
	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues",
		map[string]any{"project": "RESX", "title": "Resolved with repo_projects gone", "spec": "A spec\n"}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("create the issue: status=%d body=%s", created.Code, created.Body.String())
	}
	issue := decodeBody[model.Issue](t, created)
	if issue.ResolvedRepo == nil || *issue.ResolvedRepo != "acme/widgets" {
		t.Fatalf("%s resolves to %s, want its project's default acme/widgets", issue.Key, repoName(issue.ResolvedRepo))
	}
	uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", twoChecks)
	writeKnownMergeOnBase(t, database, "acme/widgets", 73, headCommit, issue.Key, "main", mergeCommit)
	recorded := recordResult(t, handler, issue.Key, resultBody("k2", "pass", mergeCommit, nil))
	if recorded.Code != http.StatusCreated {
		t.Fatalf("record a result: status=%d body=%s", recorded.Code, recorded.Body.String())
	}
	result := decodeBody[recordedResult](t, recorded)
	standsAt(t, readResults(t, handler, issue.Key), "k2", result.Result.ID,
		"a result at a default-branch commit of the default repository", model.CommitAtBasis, false)
}

// deployRule is a rule of one deploy source reading repo, with job as its production job.
func deployRule(repo, job string) delivery.DeliverySettings {
	return delivery.DeliverySettings{
		DeployRepo: repo, DeployWorkflowPath: ".github/workflows/deploy.yml", ProductionJobNames: []string{job},
		PRChecksWorkflowPath: ".github/workflows/ci.yml", PopulationAuthors: []string{"octocat"},
	}
}

// saveProjectRules saves rules as project's delivery settings (deliveryseed.SaveRules: delivery
// on, a rule whose repository is the project's default names none).
func saveProjectRules(t *testing.T, database *store.Store, project string, rules ...delivery.DeliverySettings) {
	t.Helper()
	if _, err := deliveryseed.SaveRules(t.Context(), database.Pool, project, rules...); err != nil {
		t.Fatalf("save the delivery settings of %s: %v", project, err)
	}
}

// progressSteps is the reconcile progress steps the store holds, sorted.
func progressSteps(t *testing.T, database *store.Store) []string {
	t.Helper()
	rows, err := database.Pool.Query(t.Context(), `select step from delivery_reconcile_progress order by step`)
	if err != nil {
		t.Fatalf("read the reconcile progress: %v", err)
	}
	defer rows.Close()
	var steps []string
	for rows.Next() {
		var step string
		if err := rows.Scan(&step); err != nil {
			t.Fatalf("read the reconcile progress: %v", err)
		}
		steps = append(steps, step)
	}
	return steps
}

// A delivery-on project whose deploy source names no repository reads its default repository, so
// its default may not move onto a repository another source of this project or another delivery-on
// project reads (409 DELIVERY_REPOSITORY_SHARED naming that project) and may not be cleared (409
// DELIVERY_NEEDS_REPOSITORY). Each refusal leaves the settings and defaults as they were. A project whose every source
// names its own repository moves and clears its default freely; and a move the rules allow clears
// the reconcile progress of the old and the new repository, and no other.
func TestTheDefaultRepositoryOfADeliveryProjectKeepsEachRepositoryOneProjects(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	createTestProject(t, handler, "ALPHA", "BETA")
	setDefaultRepository(t, handler, "ALPHA", "acme/alpha")
	setDefaultRepository(t, handler, "BETA", "acme/beta")
	saveProjectRules(t, database, "ALPHA", deployRule("acme/alpha", "alpha / release"), deployRule("acme/alpha-deploy", "explicit / release"))
	saveProjectRules(t, database, "BETA", deployRule("acme/beta-deploy", "beta / release"))
	put := func(project string, repo any) *http.Response {
		t.Helper()
		return dispatchRequest(t, handler, http.MethodPut, "/api/v1/projects/"+project+"/default-repository",
			map[string]any{"repo": repo}, "alice").Result()
	}
	for _, row := range []struct {
		name, code, project string
		repo                any
		source              int
	}{
		{"a repository another source of ALPHA reads", delivery.CodeDeliveryRepositoryShared, "ALPHA", "acme/alpha-deploy", 1},
		{"a repository a source of BETA reads", delivery.CodeDeliveryRepositoryShared, "BETA", "acme/beta-deploy", 0},
		{"clearing the default", delivery.CodeDeliveryNeedsRepository, "ALPHA", nil, 0},
	} {
		t.Run(row.name, func(t *testing.T) {
			alpha := readProjectDelivery(t, handler, "ALPHA")
			beta := readProjectDelivery(t, handler, "BETA")
			response := dispatchRequest(t, handler, http.MethodPut, "/api/v1/projects/ALPHA/default-repository",
				map[string]any{"repo": row.repo}, "alice")
			got := refusedSettings(t, response, http.StatusConflict, row.code)
			if got.Project != row.project || got.Source == nil || *got.Source != row.source || !strings.Contains(got.Message, row.project) {
				t.Errorf("refusal %+v, want project %s and source %d named", got, row.project, row.source)
			}
			for project, before := range map[string][]byte{"ALPHA": alpha, "BETA": beta} {
				if after := readProjectDelivery(t, handler, project); string(after) != string(before) {
					t.Errorf("refused move left %s's settings and default %s, want %s", project, after, before)
				}
			}
		})
	}
	for _, repo := range []any{"acme/beta-two", nil} {
		if response := put("BETA", repo); response.StatusCode != http.StatusOK {
			t.Fatalf("BETA, whose source names its own repository, moving its default to %v: status %d", repo, response.StatusCode)
		}
	}

	at := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	for _, step := range []string{"runs/deploy/acme/alpha", "runs/deploy/acme/alpha-two", "runs/deploy/acme/beta-deploy"} {
		if err := delivery.RecordReconcileProgress(t.Context(), database.Pool, step, "scope", at); err != nil {
			t.Fatalf("record %s: %v", step, err)
		}
	}
	if moved := setDefaultRepository(t, handler, "ALPHA", "acme/alpha-two"); moved.Repo == nil || *moved.Repo != "acme/alpha-two" {
		t.Fatalf("ALPHA's move answered %+v, want acme/alpha-two", moved)
	}
	if steps := progressSteps(t, database); strings.Join(steps, " ") != "runs/deploy/acme/beta-deploy" {
		t.Fatalf("after ALPHA's move the progress holds %v, want BETA's repository's alone", steps)
	}
	rules := seededDeliveryRules(t, database.Pool)
	if rule, read := rules.For("acme/alpha-two"); !read || rule.Project != "ALPHA" {
		t.Fatalf("after the move acme/alpha-two is read by %+v (%v), want ALPHA's source", rule, read)
	}
	if _, read := rules.For("acme/alpha"); read {
		t.Fatal("after the move a source still reads acme/alpha")
	}
}
