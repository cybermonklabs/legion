package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/sjawhar/envoy/internal/dispatch/access"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// privateOffBody is the answer to a switch to private where the deployment names no private
// project, byte for byte, the bytes it answered before a deployment could name one (R9).
const privateOffBody = `{"code":"PRIVATE_PROJECTS_OFF","error":"this deployment cannot make a project private yet: nothing was changed"}`

// busCall is one call a recordingBus took: the project, what its q was (the route's transaction,
// or the Go type of anything else), the project's visibility as q read it, and as another
// connection read it during the call. Through the transaction a switch to private reads private
// already; from another connection it still reads public, since the write has not committed.
type busCall struct {
	project, through, inTransaction, committed string
}

// recordingBus is a test's writer of the private-projects record (BusProjects). It records every
// call and answers each with privateErr.
type recordingBus struct {
	pool       *store.Pool
	privateErr error

	mu    sync.Mutex
	calls []busCall
}

func (b *recordingBus) EnsurePrivate(ctx context.Context, q access.Querier, project string) error {
	through := fmt.Sprintf("%T", q)
	if _, ok := q.(pgx.Tx); ok {
		through = "the route's transaction"
	}
	// The other read takes a connection of the bus's own: the call's context holds the write's
	// transaction, under which the pool refuses it a second one (store.ErrNestedAcquire).
	call := busCall{project: project, through: through, inTransaction: readVisibility(ctx, q, project),
		committed: readVisibility(context.Background(), b.pool, project)}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls = append(b.calls, call)
	return b.privateErr
}

func (b *recordingBus) taken() []busCall {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.calls)
}

// readVisibility is project's visibility as q reads it, or why q could not read it.
func readVisibility(ctx context.Context, q access.Querier, project string) string {
	var visibility string
	if err := q.QueryRow(ctx, `select visibility from projects where key = $1`, project).Scan(&visibility); err != nil {
		return "unread: " + err.Error()
	}
	return visibility
}

// newPrivateProjectsServer is a test server holding PRIV and OTHER, both alice's, and HIDE,
// carol's private project, which alice cannot see, under options, writing the private-projects
// record through bus, or through no writer when bus is nil. Its store is complete, the sessions'
// projects filled included.
func newPrivateProjectsServer(t *testing.T, options testServerOptions, bus *recordingBus) (http.Handler, *store.Store) {
	t.Helper()
	options.store = storetest.Open(t)
	if bus != nil {
		bus.pool = options.store.Pool
		options.busProjects = bus
	}
	handler, database, _ := newTestServer(t, options)
	seedPeople(t, database, accessCarol)
	for _, project := range []struct{ key, owner string }{{"PRIV", "alice"}, {"OTHER", "alice"}, {"HIDE", accessCarol}} {
		if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{"key": project.key, "name": project.key}, project.owner); response.Code != http.StatusCreated {
			t.Fatalf("create %s: status=%d body=%s", project.key, response.Code, response.Body.String())
		}
	}
	if _, err := database.Pool.Exec(context.Background(), `
		update projects set visibility = 'private' where key = 'HIDE';
		insert into session_projects_filled default values;
		insert into access_events_scrubbed default values;
	`); err != nil {
		t.Fatalf("make HIDE private and fill the sessions' projects: %v", err)
	}
	return handler, database
}

func visibilityOf(t *testing.T, database *store.Store, key string) string {
	t.Helper()
	return readVisibility(context.Background(), database.Pool, key)
}

// requireSwitchRefused fails, naming key, unless PATCH key {visibility: private, ...} answered
// status with code and a message naming each of naming, and left key as it was: public, alice's,
// and with no event appended past before.
func requireSwitchRefused(t *testing.T, database *store.Store, response *httptest.ResponseRecorder, key string, before int64, status int, code string, naming ...string) {
	t.Helper()
	visibility, owner, appended := visibilityOf(t, database, key), projectOwnerOf(t, database, key), allEvents(t, database)-before
	body := strings.TrimSpace(response.Body.String())
	named := !slices.ContainsFunc(naming, func(name string) bool { return !strings.Contains(body, name) })
	if response.Code != status || !strings.Contains(body, `"code":"`+code+`"`) || !named ||
		visibility != model.ProjectPublic || owner != "alice" || appended != 0 {
		t.Fatalf("PATCH %s private = %d %s, leaving %s %s, owned by %s, with %d events appended; want %d %s naming %q, and %s public and alice's as before",
			key, response.Code, body, key, visibility, owner, appended, status, code, naming, key)
	}
}

// requireAnIncompleteStoreRefusesTheSwitch runs, on a deployment under options that lets PRIV be
// made private, one row for each way the store cannot hold PRIV private yet: each refuses PRIV's
// switch 409 PRIVATE_PROJECTS_INCOMPLETE, changes nothing and calls no writer. The full switch's
// deployment (TestAProjectBecomesPrivateOnlyWhenTheDeploymentAllows) and the one before the full
// switch (TestAProjectBecomesPrivateBeforeItsFullSwitch) both run this one table, so a marker
// privateProjectIncomplete gains is added once and reaches both.
func requireAnIncompleteStoreRefusesTheSwitch(t *testing.T, options testServerOptions) {
	t.Helper()
	private := map[string]string{"visibility": model.ProjectPrivate}
	// Each marker a backfill writes once it is done: without it the switch is refused, naming the
	// command that writes it.
	for _, marker := range []struct{ table, command string }{
		{"session_projects_filled", "envoy-dispatch backfill-session-projects"},
		{"access_events_scrubbed", "envoy-dispatch scrub-access-events"},
	} {
		t.Run(marker.table+" empty", func(t *testing.T) {
			bus := &recordingBus{}
			handler, database := newPrivateProjectsServer(t, options, bus)
			if _, err := database.Pool.Exec(context.Background(), `delete from `+marker.table); err != nil {
				t.Fatalf("empty %s: %v", marker.table, err)
			}
			before := allEvents(t, database)
			response := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/projects/PRIV/access", private, "alice")
			requireSwitchRefused(t, database, response, "PRIV", before, http.StatusConflict, "PRIVATE_PROJECTS_INCOMPLETE", marker.table, marker.command)
			if calls := bus.taken(); len(calls) != 0 {
				t.Fatalf("%s empty: the refused write called the writer: %+v", marker.table, calls)
			}
		})
	}

	// A row of PRIV's whose node names another project, or whose node is gone, is a row a private
	// project's reads would answer wrongly: the switch is refused, naming it with its own cure
	// (graphcensus.ProjectCompleteFor), and naming no cure of its own: a node that names another
	// project is no backfill's to mend.
	for _, broken := range []struct{ name, sql, line, refuse string }{
		{"PRIV-1's node names another project", `update graph_nodes set project_key = 'OTHER' where kind = 'issue' and id = 'PRIV-1'`,
			"issue PRIV-1: differs; no command cures it: compare the row with its node", "backfill-graph-store"},
		{"PRIV-1's node deleted", `delete from graph_nodes where kind = 'issue' and id = 'PRIV-1'`,
			"issue PRIV-1: missing; run envoy-dispatch backfill-graph-store", "writes their nodes"},
	} {
		t.Run(broken.name, func(t *testing.T) {
			bus := &recordingBus{}
			handler, database := newPrivateProjectsServer(t, options, bus)
			if created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{"project": "PRIV", "title": "One"}, "alice"); created.Code != http.StatusCreated {
				t.Fatalf("create PRIV-1: %d %s", created.Code, created.Body.String())
			}
			if tag, err := database.Pool.Exec(context.Background(), broken.sql); err != nil || tag.RowsAffected() != 1 {
				t.Fatalf("%s: %v, %d rows", broken.name, err, tag.RowsAffected())
			}
			before := allEvents(t, database)
			response := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/projects/PRIV/access", private, "alice")
			requireSwitchRefused(t, database, response, "PRIV", before, http.StatusConflict, "PRIVATE_PROJECTS_INCOMPLETE", broken.line)
			if body := response.Body.String(); strings.Contains(body, broken.refuse) {
				t.Fatalf("%s: the refusal %s holds %q, a cure its line does not name", broken.name, body, broken.refuse)
			}
			if calls := bus.taken(); len(calls) != 0 {
				t.Fatalf("%s: the refused write called the writer: %+v", broken.name, calls)
			}
		})
	}
}

// TestAProjectBecomesPrivateOnlyWhenTheDeploymentAllows is c-private-needs-callout's Dispatch row
// (K7). A deployment that names no private project answers a switch to private 409
// PRIVATE_PROJECTS_OFF with privateOffBody's bytes; one that names others answers 409
// PRIVATE_PROJECT_NOT_PROVISIONED and calls no writer. For a project the list names, the record
// is written through the route's own transaction as its last step, and any refusal rolls the
// whole write back and answers 409 PRIVATE_PROJECTS_UNAVAILABLE naming the bucket. The route
// makes no other bus call: not after a commit that failed, and not for public, whose key is the
// writer's own pass to delete.
func TestAProjectBecomesPrivateOnlyWhenTheDeploymentAllows(t *testing.T) {
	private := map[string]string{"visibility": model.ProjectPrivate}
	public := map[string]string{"visibility": model.ProjectPublic}
	refusal := errors.New(`nats: permissions violation for publish to "$KV.envoy_private_projects.PRIV"`)
	provisioned := []string{"PRIV"}
	// The record written last before the commit: through the transaction PRIV reads private
	// already, and from another connection still public.
	recorded := busCall{"PRIV", "the route's transaction", model.ProjectPrivate, model.ProjectPublic}

	t.Run("an empty list", func(t *testing.T) {
		bus := &recordingBus{}
		handler, database := newPrivateProjectsServer(t, testServerOptions{}, bus)
		before := allEvents(t, database)
		// The owner, and alice on a key she cannot see, are answered alike, before any read.
		hidden := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/projects/HIDE/access", private, "alice")
		response := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/projects/PRIV/access", private, "alice")
		for call, answer := range map[string]*httptest.ResponseRecorder{"PRIV, which alice owns": response, "HIDE, which alice cannot see": hidden} {
			if body := strings.TrimSpace(answer.Body.String()); answer.Code != http.StatusConflict || body != privateOffBody {
				t.Fatalf("PATCH %s private with an empty list answered %d %s, want 409 and %s", call, answer.Code, body, privateOffBody)
			}
		}
		requireSwitchRefused(t, database, response, "PRIV", before, http.StatusConflict, "PRIVATE_PROJECTS_OFF")
		if calls := bus.taken(); len(calls) != 0 {
			t.Fatalf("an empty list: the refused write called the writer: %+v", calls)
		}
	})

	requireAnIncompleteStoreRefusesTheSwitch(t, testServerOptions{privateProjects: provisioned})

	t.Run("a project the list does not name", func(t *testing.T) {
		bus := &recordingBus{}
		handler, database := newPrivateProjectsServer(t, testServerOptions{privateProjects: provisioned}, bus)
		before := allEvents(t, database)
		response := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/projects/OTHER/access", private, "alice")
		requireSwitchRefused(t, database, response, "OTHER", before, http.StatusConflict, "PRIVATE_PROJECT_NOT_PROVISIONED", "OTHER", "DISPATCH_PRIVATE_PROJECTS")
		if calls := bus.taken(); len(calls) != 0 {
			t.Fatalf("OTHER, which the list does not name: the refused write called the writer: %+v", calls)
		}
	})

	t.Run("a writer that refuses", func(t *testing.T) {
		bus := &recordingBus{privateErr: refusal}
		handler, database := newPrivateProjectsServer(t, testServerOptions{privateProjects: provisioned}, bus)
		before := allEvents(t, database)
		// The owner change in the same body goes back with it: the record is the write's last step.
		response := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/projects/PRIV/access",
			map[string]string{"visibility": model.ProjectPrivate, "owner": accessCarol}, "alice")
		requireSwitchRefused(t, database, response, "PRIV", before, http.StatusConflict, "PRIVATE_PROJECTS_UNAVAILABLE", privateProjectsBucket)
		if calls, want := bus.taken(), []busCall{recorded}; !slices.Equal(calls, want) {
			t.Fatalf("a writer that refuses: the write called %+v, want %+v", calls, want)
		}
	})

	t.Run("no bus", func(t *testing.T) {
		handler, database := newPrivateProjectsServer(t, testServerOptions{privateProjects: provisioned}, nil)
		before := allEvents(t, database)
		response := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/projects/PRIV/access", private, "alice")
		requireSwitchRefused(t, database, response, "PRIV", before, http.StatusConflict, "PRIVATE_PROJECTS_UNAVAILABLE", privateProjectsBucket)
	})

	t.Run("a writer that accepts", func(t *testing.T) {
		bus := &recordingBus{}
		handler, database := newPrivateProjectsServer(t, testServerOptions{privateProjects: provisioned}, bus)
		made := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/projects/PRIV/access", private, "alice")
		if made.Code != http.StatusOK || decodeBody[model.ProjectAccess](t, made).Visibility != model.ProjectPrivate ||
			visibilityOf(t, database, "PRIV") != model.ProjectPrivate {
			t.Fatalf("PATCH PRIV private with a writer that accepts = %d %s, PRIV %s; want 200 and PRIV private",
				made.Code, made.Body.String(), visibilityOf(t, database, "PRIV"))
		}
		// public makes no bus call: deleting the key is the writer's own pass.
		made = dispatchRequest(t, handler, http.MethodPatch, "/api/v1/projects/PRIV/access", public, "alice")
		if made.Code != http.StatusOK || visibilityOf(t, database, "PRIV") != model.ProjectPublic {
			t.Fatalf("PATCH PRIV public = %d %s; want 200 and PRIV public", made.Code, made.Body.String())
		}
		requireOnlyTheRecord(t, bus, recorded, "making PRIV private, then public")
	})

	t.Run("a commit that fails after the write", func(t *testing.T) {
		bus := &recordingBus{}
		handler, database := newPrivateProjectsServer(t, testServerOptions{privateProjects: provisioned}, bus)
		// A deferred trigger refuses the commit of any write that makes a project private, after
		// every statement of it, the record's included, has run. The route answers that refused
		// commit 500, so the request log line `PATCH /api/v1/projects/PRIV/access ... status=500`
		// this subtest prints is the fault it plants here, not a defect.
		if _, err := database.Pool.Exec(context.Background(), `
			create function refuse_private_commit() returns trigger language plpgsql as $$
			begin
				raise exception 'the commit of % refused by the test', new.key;
			end $$;
			create constraint trigger refuse_private_commit after update of visibility on projects
				deferrable initially deferred for each row when (new.visibility = 'private')
				execute function refuse_private_commit();
		`); err != nil {
			t.Fatalf("make every commit that makes a project private fail: %v", err)
		}
		failed := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/projects/PRIV/access", private, "alice")
		if failed.Code == http.StatusOK || visibilityOf(t, database, "PRIV") != model.ProjectPublic {
			t.Fatalf("PATCH PRIV private whose commit fails = %d %s, PRIV %s; want a failure and PRIV public",
				failed.Code, failed.Body.String(), visibilityOf(t, database, "PRIV"))
		}
		requireOnlyTheRecord(t, bus, recorded, "a commit that failed after the record was written")
	})
}

// requireOnlyTheRecord fails unless bus took exactly one call, want, through the route's
// transaction, naming any call made through anything else.
func requireOnlyTheRecord(t *testing.T, bus *recordingBus, want busCall, row string) {
	t.Helper()
	calls := bus.taken()
	for _, call := range calls {
		if call.through != want.through {
			t.Errorf("%s: EnsurePrivate(%s) was called through %s, outside the route's transaction", row, call.project, call.through)
		}
	}
	if !slices.Equal(calls, []busCall{want}) {
		t.Fatalf("%s: the routes called %+v, want only %+v", row, calls, want)
	}
}

// The maintenance sweep logs, on every pass, an error for each private project neither list
// names, and, where either list names any, at its first pass and then once an hour, an error
// naming any row a project owns whose node does not name its project. With both lists empty it
// reads no row of the census.
func TestTheMaintenanceSweepNamesARowAPrivateProjectWouldAnswerWrongly(t *testing.T) {
	for _, row := range []struct {
		name                   string
		list, beforeFullSwitch []string
		incomplete             int
	}{
		{"a list", []string{"PRIV"}, nil, 1},
		{"a list before the full switch", nil, []string{"PRIV"}, 1},
		{"an empty list", nil, nil, 0},
	} {
		t.Run(row.name, func(t *testing.T) {
			logged := captureLog(t)
			database := storetest.Open(t)
			// PRIV-1 is an issue a build before the node writers stored with no node, and OFFL a
			// project direct SQL made private, which the route would have refused.
			if _, err := database.Pool.Exec(context.Background(), `
				insert into projects (key, name, default_assignee) values ('PRIV', 'Private', 'alice');
				insert into issues (key, project_key, number, title, created_by, rank)
				values ('PRIV-1', 'PRIV', 1, 'One', '{"kind":"user","id":"alice"}', 'U');
				insert into projects (key, name, default_assignee, visibility) values ('OFFL', 'Off the list', 'alice', 'private');
			`); err != nil {
				t.Fatalf("seed PRIV-1 with no node, and OFFL: %v", err)
			}
			var passes atomic.Int64
			leases := sweepLeases(t, database)
			runSweep(t, func(ctx context.Context) {
				RunMaintenanceSweep(ctx, database.Pool, maintenanceTestInterval, leases, func(context.Context) error {
					passes.Add(1)
					return nil
				}, row.list, row.beforeFullSwitch, nil)
			})
			waitUntil(t, 30*time.Second, "three passes of the sweep", func() bool { return passes.Load() >= 3 })
			incomplete := logged.Lines("issue PRIV-1: missing")
			if got := strings.Count(incomplete, "\n"); got != row.incomplete || (got > 0 && !strings.Contains(incomplete, "level=ERROR")) {
				t.Errorf("%s: three passes logged %d lines naming PRIV-1:\n%s\nwant %d ERROR lines", row.name, got, incomplete, row.incomplete)
			}
			unprovisioned := logged.Lines("private project not provisioned: OFFL is private")
			if got := strings.Count(unprovisioned, "\n"); got < 3 || !strings.Contains(unprovisioned, "level=ERROR") ||
				!strings.Contains(unprovisioned, "envoy-dispatch project-visibility OFFL public") {
				t.Errorf("%s: three passes logged %d lines naming OFFL:\n%s\nwant an ERROR line naming it and its repairs on every pass", row.name, got, unprovisioned)
			}
		})
	}
}

// TestAnUnprovisionedPrivateProjectFailsClosed: a project the database holds private whose key
// the list does not name stays private. alice, a full member PRIV's list does not name, reads its
// issue, document and project row as missing, as before the list changed; the owner's switch to
// private is refused; the owner's switch to public is the one visibility write it takes, after
// which alice reads the issue.
func TestAnUnprovisionedPrivateProjectFailsClosed(t *testing.T) {
	for _, row := range []struct {
		list    []string
		refusal string
	}{
		{nil, "PRIVATE_PROJECTS_OFF"},
		{[]string{"OTHER"}, "PRIVATE_PROJECT_NOT_PROVISIONED"},
	} {
		t.Run(fmt.Sprintf("the list %v", row.list), func(t *testing.T) {
			handler, database, _ := newTestServer(t, testServerOptions{privateProjects: row.list})
			seedPeople(t, database, accessCarol)
			// bob's PRIV: carol on its access list, an issue whose spec is a document, and a default
			// repository; then private, as a deployment whose list held it left it.
			for _, call := range []struct {
				method, path string
				body         any
				want         int
			}{
				{http.MethodPost, "/api/v1/projects", map[string]string{"key": "PRIV", "name": "Private"}, http.StatusCreated},
				{http.MethodPut, "/api/v1/projects/PRIV/access/people/" + accessCarol, nil, http.StatusOK},
			} {
				if response := dispatchRequest(t, handler, call.method, call.path, call.body, "bob"); response.Code != call.want {
					t.Fatalf("%s %s as bob: %d %s", call.method, call.path, response.Code, response.Body.String())
				}
			}
			created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{"project": "PRIV", "title": "Hidden", "spec": "The plan."}, "bob")
			if created.Code != http.StatusCreated {
				t.Fatalf("create PRIV's issue: %d %s", created.Code, created.Body.String())
			}
			issue := decodeBody[struct {
				Key               string `json:"key"`
				PrimaryArtifactID string `json:"primary_artifact_id"`
			}](t, created)
			if response := dispatchRequest(t, handler, http.MethodPut, "/api/v1/projects/PRIV/default-repository", map[string]any{"repo": "acme/priv"}, "bob"); response.Code != http.StatusOK {
				t.Fatalf("make acme/priv PRIV's default repository: %d %s", response.Code, response.Body.String())
			}
			if _, err := database.Pool.Exec(context.Background(), `update projects set visibility = 'private' where key = 'PRIV'`); err != nil {
				t.Fatalf("make PRIV private: %v", err)
			}

			issueRead, documentRead := "/api/v1/issues/"+issue.Key, "/api/v1/artifacts/"+issue.PrimaryArtifactID
			// listsPRIV is whether alice's project list, the read of a project's row, holds PRIV.
			listsPRIV := func() bool {
				t.Helper()
				response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/projects", nil, "alice")
				if response.Code != http.StatusOK {
					t.Fatalf("alice's GET /api/v1/projects: %d %s", response.Code, response.Body.String())
				}
				return slices.ContainsFunc(decodeBody[[]model.Project](t, response), func(p model.Project) bool { return p.Key == "PRIV" })
			}
			for _, read := range []struct{ what, path string }{
				{"PRIV's issue " + issue.Key, issueRead},
				{"PRIV's document " + issue.PrimaryArtifactID, documentRead},
			} {
				if response := dispatchRequest(t, handler, http.MethodGet, read.path, nil, "alice"); response.Code != http.StatusNotFound {
					t.Errorf("alice reads %s, private and off the list: %d %s, want it missing as before the list changed", read.what, response.Code, response.Body.String())
				}
			}
			if listsPRIV() {
				t.Errorf("alice's project list holds PRIV, private and off the list; want it missing as before the list changed")
			}
			refused := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/projects/PRIV/access", map[string]string{"visibility": model.ProjectPrivate}, "bob")
			if refused.Code != http.StatusConflict || !strings.Contains(refused.Body.String(), `"code":"`+row.refusal+`"`) {
				t.Errorf("bob's PATCH PRIV private off the list = %d %s, want 409 %s", refused.Code, refused.Body.String(), row.refusal)
			}
			if response := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/projects/PRIV/access", map[string]string{"visibility": model.ProjectPublic}, "bob"); response.Code != http.StatusOK {
				t.Fatalf("bob's PATCH PRIV public off the list = %d %s, want 200", response.Code, response.Body.String())
			}
			if response := dispatchRequest(t, handler, http.MethodGet, issueRead, nil, "alice"); response.Code != http.StatusOK {
				t.Errorf("alice reads PRIV's issue %s once PRIV is public: %d %s, want 200", issue.Key, response.Code, response.Body.String())
			}
			if !listsPRIV() {
				t.Errorf("alice's project list lacks PRIV once PRIV is public")
			}
		})
	}
}
