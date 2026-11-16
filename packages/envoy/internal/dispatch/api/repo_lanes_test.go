package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/access"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// sharedLane is the GitHub subject lane acme/a.b and acme/a_b both publish on.
const sharedLane = "notifications.github.acme.a_b"

// newLaneServer is newPrivateProjectsServer's server - PRIV and OTHER, alice's, and HIDE, carol's
// private project alice cannot see, with PRIV on the provisioned list - with PUB, alice's public
// project, beside them, writing the private-projects record through bus.
func newLaneServer(t *testing.T, bus *recordingBus) (http.Handler, *store.Store) {
	t.Helper()
	handler, database := newPrivateProjectsServer(t, testServerOptions{privateProjects: []string{"PRIV"}}, bus)
	createProjects(t, handler, "PUB")
	return handler, database
}

// mapRepo maps repo as login with body, failing the test unless the write answers 200.
func mapRepo(t *testing.T, handler http.Handler, login, repo string, body map[string]any) {
	t.Helper()
	if response := dispatchRequest(t, handler, http.MethodPut, "/api/v1/settings/repo-projects/"+repo, body, login); response.Code != http.StatusOK {
		t.Fatalf("%s maps %s %v: %d %s", login, repo, body, response.Code, response.Body.String())
	}
}

// insertMapping writes repo's mapping to project by direct SQL, as a row stored before the lane
// rule, or written by an operator, stands: no route's check has read it.
func insertMapping(t *testing.T, database *store.Store, repo, project string) {
	t.Helper()
	if _, err := database.Pool.Exec(context.Background(), `
		insert into repo_projects (repo, project, created_by) values ($1, $2, '{"kind":"user","id":"operator"}')
	`, repo, project); err != nil {
		t.Fatalf("map %s to %s by direct SQL: %v", repo, project, err)
	}
}

// mappedTo is the project repo's row maps it to, "" for a repository no row holds.
func mappedTo(t *testing.T, database *store.Store, repo string) string {
	t.Helper()
	var project string
	err := database.Pool.QueryRow(context.Background(), `select project from repo_projects where repo = $1`, repo).Scan(&project)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("read %s's mapping: %v", repo, err)
	}
	return project
}

// requireLaneRefusal fails, naming what, unless response is 409 REPOSITORY_LANE_SHARED whose body
// holds each of naming and none of hiding.
func requireLaneRefusal(t *testing.T, what string, response *httptest.ResponseRecorder, naming, hiding []string) {
	t.Helper()
	body := strings.TrimSpace(response.Body.String())
	if response.Code != http.StatusConflict || !strings.Contains(body, `"code":"`+codeRepositoryLaneShared+`"`) {
		t.Fatalf("%s = %d %s, want 409 %s", what, response.Code, body, codeRepositoryLaneShared)
	}
	for _, name := range naming {
		if !strings.Contains(body, name) {
			t.Errorf("%s: the refusal %s does not name %q", what, body, name)
		}
	}
	for _, name := range hiding {
		if strings.Contains(body, name) {
			t.Errorf("%s: the refusal %s names %q, which the caller cannot see", what, body, name)
		}
	}
}

// TestARepositorySharingALaneWithAnotherProjectCannotBePrivate: a switch to private is refused
// while one of the project's repositories shares a GitHub subject lane with another project's,
// whether the other's name or its renamed_to is on the lane. The other project and its
// repository are named only to a caller who sees that project. The refusal leaves PRIV public
// and calls no writer of the private-projects record.
func TestARepositorySharingALaneWithAnotherProjectCannotBePrivate(t *testing.T) {
	for _, row := range []struct {
		name           string
		seed           func(t *testing.T, handler http.Handler, database *store.Store)
		naming, hiding []string
	}{
		{"a public project's repository on the lane", func(t *testing.T, handler http.Handler, _ *store.Store) {
			mapRepo(t, handler, "alice", "acme/a.b", map[string]any{"project": "PUB"})
			mapRepo(t, handler, "alice", "acme/a_b", map[string]any{"project": "PRIV"})
		}, []string{"acme/a.b (PUB) and acme/a_b (PRIV) share the GitHub subject lane " + sharedLane, laneRule}, nil},
		{"a public project's renamed_to on the lane", func(t *testing.T, handler http.Handler, _ *store.Store) {
			mapRepo(t, handler, "alice", "acme/x", map[string]any{"project": "PUB", "renamed_to": "acme/a.b"})
			mapRepo(t, handler, "alice", "acme/a_b", map[string]any{"project": "PRIV"})
		}, []string{"acme/x renamed to acme/a.b (PUB) and acme/a_b (PRIV) share the GitHub subject lane " + sharedLane, laneRule}, nil},
		{"a private project its owner cannot see on the lane", func(t *testing.T, handler http.Handler, database *store.Store) {
			mapRepo(t, handler, "alice", "acme/a_b", map[string]any{"project": "PRIV"})
			// No route writes HIDE's neighbour now; a row stored before the rule does.
			insertMapping(t, database, "acme/a.b", "HIDE")
		}, []string{"the GitHub subject lane " + sharedLane + " is held by a project you cannot see; " + laneRule}, []string{"HIDE", "acme/a.b"}},
	} {
		t.Run(row.name, func(t *testing.T) {
			bus := &recordingBus{}
			handler, database := newLaneServer(t, bus)
			row.seed(t, handler, database)
			before := allEvents(t, database)
			response := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/projects/PRIV/access",
				map[string]string{"visibility": model.ProjectPrivate}, "alice")
			requireRefused(t, database, response, "PRIV", before, http.StatusConflict, codeRepositoryLaneShared, row.naming...)
			requireLaneRefusal(t, row.name+": PATCH PRIV private", response, nil, row.hiding)
			if calls := bus.taken(); len(calls) != 0 {
				t.Fatalf("%s: the refused switch called the writer: %+v", row.name, calls)
			}
		})
	}
}

// TestAMappingIntoAPrivateProjectsLaneIsRefused: a mapping PUT whose repository, or whose
// renamed_to, is on the GitHub subject lane of another project's mapping, where either project is
// private, is refused 409 REPOSITORY_LANE_SHARED and writes nothing: into a public project beside
// a private project's repository, into a private project beside a public one's, and beside a row
// direct SQL wrote in capitals, which the lane compares without case.
func TestAMappingIntoAPrivateProjectsLaneIsRefused(t *testing.T) {
	for _, row := range []struct {
		name           string
		seed           func(t *testing.T, handler http.Handler, database *store.Store)
		login, repo    string
		body           map[string]any
		naming, hiding []string
	}{
		{"into a public project, beside a private project's repository alice cannot see",
			func(t *testing.T, handler http.Handler, _ *store.Store) {
				mapRepo(t, handler, accessCarol, "acme/a_b", map[string]any{"project": "HIDE"})
			}, "alice", "acme/a.b", map[string]any{"project": "PUB"},
			[]string{"the GitHub subject lane " + sharedLane + " is held by a project you cannot see; " + laneRule}, []string{"HIDE", "acme/a_b"}},
		{"into a private project, beside a public project's repository",
			func(t *testing.T, handler http.Handler, _ *store.Store) {
				mapRepo(t, handler, "alice", "acme/a_b", map[string]any{"project": "PUB"})
			}, accessCarol, "acme/a.b", map[string]any{"project": "HIDE"},
			[]string{"acme/a_b (PUB) and acme/a.b (HIDE) share the GitHub subject lane " + sharedLane + "; " + laneRule}, nil},
		{"a renamed_to onto a private project's lane",
			func(t *testing.T, handler http.Handler, _ *store.Store) {
				mapRepo(t, handler, accessCarol, "acme/a_b", map[string]any{"project": "HIDE"})
			}, "alice", "acme/y", map[string]any{"project": "PUB", "renamed_to": "acme/a.b"},
			[]string{"the GitHub subject lane " + sharedLane + " is held by a project you cannot see"}, []string{"HIDE", "acme/a_b"}},
		{"beside a private project's row direct SQL wrote in capitals",
			func(t *testing.T, _ http.Handler, database *store.Store) {
				insertMapping(t, database, "ACME/A_B", "HIDE")
			}, "alice", "acme/a.b", map[string]any{"project": "PUB"},
			[]string{"the GitHub subject lane " + sharedLane + " is held by a project you cannot see"}, []string{"HIDE", "A_B"}},
	} {
		t.Run(row.name, func(t *testing.T) {
			handler, database := newLaneServer(t, &recordingBus{})
			row.seed(t, handler, database)
			before := allEvents(t, database)
			response := dispatchRequest(t, handler, http.MethodPut, "/api/v1/settings/repo-projects/"+row.repo, row.body, row.login)
			if project := mappedTo(t, database, row.repo); project != "" || allEvents(t, database) != before {
				t.Fatalf("%s: %s's PUT %s %v answered %d %s and wrote it, mapped to %q, beside a private project's lane",
					row.name, row.login, row.repo, row.body, response.Code, strings.TrimSpace(response.Body.String()), project)
			}
			requireLaneRefusal(t, row.name, response, row.naming, row.hiding)
		})
	}
}

// TestAPrivateProjectsOwnMappingsStayWritable: a private project's own mappings never refuse each
// other, so PRIV keeps writing acme/a_b, adds acme/a.b beside it on the same lane, and moves
// acme/a_b to PUB where no mapping of PUB's neighbours it.
func TestAPrivateProjectsOwnMappingsStayWritable(t *testing.T) {
	for _, row := range []struct {
		name, repo string
		body       map[string]any
	}{
		{"its own mapping made its default", "acme/a_b", map[string]any{"project": "PRIV", "default": true}},
		{"a second name on its own lane", "acme/a.b", map[string]any{"project": "PRIV"}},
		{"its mapping moved to a public project with no neighbour", "acme/a_b", map[string]any{"project": "PUB"}},
	} {
		t.Run(row.name, func(t *testing.T) {
			handler, database := newLaneServer(t, &recordingBus{})
			mapRepo(t, handler, "alice", "acme/a_b", map[string]any{"project": "PRIV"})
			makePrivate(t, database, "PRIV")
			response := dispatchRequest(t, handler, http.MethodPut, "/api/v1/settings/repo-projects/"+row.repo, row.body, "alice")
			if response.Code != http.StatusOK || mappedTo(t, database, row.repo) != row.body["project"] {
				t.Fatalf("%s: PUT %s %v with PRIV private = %d %s, want 200 and %s mapped to %v",
					row.name, row.repo, row.body, response.Code, strings.TrimSpace(response.Body.String()), row.repo, row.body["project"])
			}
		})
	}
}

// createdAtMember is a mapping's created_at, the one member whose bytes differ run to run.
var createdAtMember = regexp.MustCompile(`"created_at":"[^"]*"`)

// TestNoPrivateProjectRefusesNoMapping (R9): with no private project two public projects map two
// names of one lane, each answered with the bytes a build before the lane rule answers, created_at
// masked. The control in the same test makes PUB2 private and writes acme/a.b again: it is
// refused, which proves the fixture's two names share a lane.
func TestNoPrivateProjectRefusesNoMapping(t *testing.T) {
	handler, database, deps := newTestServer(t, testServerOptions{})
	createProjects(t, handler, "PUB", "PUB2")
	for _, row := range []struct{ repo, project, want string }{
		{"acme/a.b", "PUB", `{"repo":"acme/a.b","project":"PUB","default":false,"renamed_to":null,"created_by":{"kind":"user","id":"alice"},"created_at":"<masked>"}`},
		{"acme/a_b", "PUB2", `{"repo":"acme/a_b","project":"PUB2","default":false,"renamed_to":null,"created_by":{"kind":"user","id":"alice"},"created_at":"<masked>"}`},
	} {
		response := dispatchRequest(t, handler, http.MethodPut, "/api/v1/settings/repo-projects/"+row.repo, map[string]any{"project": row.project}, "alice")
		got := createdAtMember.ReplaceAllString(strings.TrimSpace(response.Body.String()), `"created_at":"<masked>"`)
		if response.Code != http.StatusOK || got != row.want {
			t.Errorf("with no private project, PUT %s → %s = %d %s, want 200 %s", row.repo, row.project, response.Code, got, row.want)
		}
	}

	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin making PUB2 private: %v", err)
	}
	defer tx.Rollback(ctx)
	if _, _, err := access.SetVisibility(ctx, tx, deps.Events, "PUB2", model.ProjectPrivate, model.Actor{Kind: "user", ID: "alice"}); err != nil {
		t.Fatalf("make PUB2 private: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit making PUB2 private: %v", err)
	}
	again := dispatchRequest(t, handler, http.MethodPut, "/api/v1/settings/repo-projects/acme/a.b", map[string]any{"project": "PUB"}, "alice")
	requireLaneRefusal(t, "the control: PUT acme/a.b → PUB once PUB2, which maps acme/a_b, is private", again,
		[]string{"acme/a_b (PUB2) and acme/a.b (PUB) share the GitHub subject lane " + sharedLane}, nil)
}

// mappingCall is one EnsurePrivate a mapping write made: the project, what its q was (the write's
// transaction, or the Go type of anything else), and the project's repositories as q read them
// and as another connection read them during the call. Through the transaction a write into the
// project reads its repository already; from another connection it does not, since the write
// has not committed.
type mappingCall struct {
	project, through         string
	inTransaction, committed string
}

// mappingBus is a test's writer of the private-projects record that notes, of every call, what
// mappingCall holds, and answers each with err.
type mappingBus struct {
	pool *store.Pool
	err  error

	mu    sync.Mutex
	calls []mappingCall
}

func (b *mappingBus) EnsurePrivate(ctx context.Context, q access.Querier, project string) error {
	through := fmt.Sprintf("%T", q)
	if _, ok := q.(pgx.Tx); ok {
		through = "the write's transaction"
	}
	// The other read takes a connection of the bus's own: the call's context holds the write's
	// transaction, under which the pool refuses it a second one (store.ErrNestedAcquire).
	call := mappingCall{project: project, through: through, inTransaction: readMappedRepos(ctx, q, project),
		committed: readMappedRepos(context.Background(), b.pool, project)}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls = append(b.calls, call)
	return b.err
}

// since is every call the bus took after its first n.
func (b *mappingBus) since(n int) []mappingCall {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.calls[n:])
}

func (b *mappingBus) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.calls)
}

// readMappedRepos is project's repositories as q reads them, comma-separated, or why q could not
// read them.
func readMappedRepos(ctx context.Context, q access.Querier, project string) string {
	rows, err := q.Query(ctx, `select repo from repo_projects where project = $1 order by repo`, project)
	if err != nil {
		return "unread: " + err.Error()
	}
	repos, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return "unread: " + err.Error()
	}
	return strings.Join(repos, ",")
}

// TestAMappingIntoAPrivateProjectReachesTheBusBeforeItCommits: a mapping write whose target
// project is private writes the project's private-projects record through its own transaction,
// as its last step before the commit, so the record names the repository before any other
// connection can read the mapping; a refused record refuses the write, 409
// PRIVATE_PROJECTS_UNAVAILABLE naming the bucket, and writes nothing. A move out of the project
// and a DELETE make no call, and no write makes one through the pool: only the writer's own pass
// removes a name (K7). The boot seed into a private project calls it through the seed's own
// transaction, and with no writer at all leaves that entry out, logged, and goes on.
func TestAMappingIntoAPrivateProjectReachesTheBusBeforeItCommits(t *testing.T) {
	options := testServerOptions{store: storetest.Open(t)}
	bus := &mappingBus{pool: options.store.Pool}
	options.busProjects = bus
	handler, database, _ := newTestServer(t, options)
	createProjects(t, handler, "PRIV", "PUB")
	makePrivate(t, database, "PRIV")
	if _, err := database.Pool.Exec(context.Background(), `
		create function refuse_mapping_commit() returns trigger language plpgsql as $$
		begin
			raise exception 'the commit of % refused by the test', new.repo;
		end $$;
		create constraint trigger refuse_mapping_commit after insert on repo_projects
			deferrable initially deferred for each row when (new.repo = 'acme/f')
			execute function refuse_mapping_commit();
	`); err != nil {
		t.Fatalf("make the commit of acme/f fail: %v", err)
	}
	inPRIV := func(inTransaction, committed string) []mappingCall {
		return []mappingCall{{"PRIV", "the write's transaction", inTransaction, committed}}
	}
	put := func(repo, project string) *httptest.ResponseRecorder {
		return dispatchRequest(t, handler, http.MethodPut, "/api/v1/settings/repo-projects/"+repo, map[string]any{"project": project}, "alice")
	}
	for _, row := range []struct {
		name  string
		write func() *httptest.ResponseRecorder
		// status is the write's answer, and 0 for a failure of any kind; mapped is where the
		// repository is mapped after it, "" for nowhere.
		status       int
		repo, mapped string
		calls        []mappingCall
		refuse       error
	}{
		{"PUT acme/z → PRIV", func() *httptest.ResponseRecorder { return put("acme/z", "PRIV") },
			http.StatusOK, "acme/z", "PRIV", inPRIV("acme/z", ""), nil},
		{"PUT acme/w → PRIV, refused by the writer", func() *httptest.ResponseRecorder { return put("acme/w", "PRIV") },
			http.StatusConflict, "acme/w", "", inPRIV("acme/w,acme/z", "acme/z"),
			errors.New(`nats: permissions violation for publish to "$KV.envoy_private_projects.PRIV"`)},
		{"PUT acme/f → PRIV, whose commit fails", func() *httptest.ResponseRecorder { return put("acme/f", "PRIV") },
			0, "acme/f", "", inPRIV("acme/f,acme/z", "acme/z"), nil},
		{"PUT acme/z → PUB, a move out of PRIV", func() *httptest.ResponseRecorder { return put("acme/z", "PUB") },
			http.StatusOK, "acme/z", "PUB", nil, nil},
		{"PUT acme/d → PRIV", func() *httptest.ResponseRecorder { return put("acme/d", "PRIV") },
			http.StatusOK, "acme/d", "PRIV", inPRIV("acme/d", ""), nil},
		{"DELETE acme/d", func() *httptest.ResponseRecorder {
			return dispatchRequest(t, handler, http.MethodDelete, "/api/v1/settings/repo-projects/acme/d", nil, "alice")
		}, http.StatusNoContent, "acme/d", "", nil, nil},
	} {
		bus.mu.Lock()
		bus.err = row.refuse
		bus.mu.Unlock()
		before := bus.count()
		response := row.write()
		failed := response.Code >= http.StatusBadRequest
		if row.status == 0 && !failed || row.status != 0 && response.Code != row.status {
			t.Errorf("%s answered %d %s, want %d (0: a failure)", row.name, response.Code, strings.TrimSpace(response.Body.String()), row.status)
		}
		if row.refuse != nil && (!strings.Contains(response.Body.String(), `"code":"PRIVATE_PROJECTS_UNAVAILABLE"`) ||
			!strings.Contains(response.Body.String(), privateProjectsBucket)) {
			t.Errorf("%s answered %s, want PRIVATE_PROJECTS_UNAVAILABLE naming %s", row.name, strings.TrimSpace(response.Body.String()), privateProjectsBucket)
		}
		if project := mappedTo(t, database, row.repo); project != row.mapped {
			t.Errorf("%s left %s mapped to %q, want %q", row.name, row.repo, project, row.mapped)
		}
		requireMappingCalls(t, row.name, bus.since(before), row.calls)
	}

	logged := captureLog(t)
	before := bus.count()
	if err := SeedRepoProjects(context.Background(), database, events.NewBroker(), bus, "acme/s=PRIV"); err != nil {
		t.Fatalf("seed acme/s=PRIV: %v", err)
	}
	requireMappingCalls(t, "the seed of acme/s=PRIV", bus.since(before), inPRIV("acme/s", ""))
	if project := mappedTo(t, database, "acme/s"); project != "PRIV" {
		t.Errorf("the seed of acme/s=PRIV left it mapped to %q, want PRIV", project)
	}
	// A deployment with no writer at all answers as busprojects.ErrNoBus: the entry is left out
	// and logged, naming it and the bucket, and the seed goes on.
	if err := SeedRepoProjects(context.Background(), database, events.NewBroker(), nil, "acme/n=PRIV,acme/p=PUB"); err != nil {
		t.Fatalf("seed acme/n=PRIV with no writer: %v, want the entry left out and the seed gone on", err)
	}
	if project := mappedTo(t, database, "acme/n"); project != "" {
		t.Errorf("the seed of acme/n=PRIV with no writer mapped it to %q, want it left out", project)
	}
	if project := mappedTo(t, database, "acme/p"); project != "PUB" {
		t.Errorf("the seed of acme/p=PUB after a refused entry mapped it to %q, want PUB", project)
	}
	if line := logged.Lines(`repo=acme/n`); !strings.Contains(line, "level=ERROR") || !strings.Contains(line, privateProjectsBucket) {
		t.Errorf("the seed of acme/n=PRIV with no writer logged %q, want an ERROR line naming acme/n and %s", line, privateProjectsBucket)
	}
}

// requireMappingCalls fails, naming the write, unless calls are want, naming first any call made
// through anything but the write's own transaction.
func requireMappingCalls(t *testing.T, write string, calls, want []mappingCall) {
	t.Helper()
	for _, call := range calls {
		if call.through != "the write's transaction" {
			t.Errorf("%s: EnsurePrivate(%s) was called through %s, outside the write's transaction", write, call.project, call.through)
		}
	}
	if !slices.Equal(calls, want) {
		t.Errorf("%s: the writer took %+v, want %+v", write, calls, want)
	}
}
