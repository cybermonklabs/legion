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

	"github.com/sjawhar/envoy/internal/dispatch/access"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// privateOffBody is the answer to a switch to private where the deployment names no private
// project, byte for byte, the bytes it answered before a deployment could name one (R9).
const privateOffBody = `{"code":"PRIVATE_PROJECTS_OFF","error":"this deployment cannot make a project private yet: nothing was changed"}`

// proveCall is one Prove a recordingAdmission noted, read from a connection of its own as the call
// ran: whether the project's owner lock was held (TryLockOwners took nothing, so the switch's
// transaction holds it still), and the project's visibility as committed, which a switch to private
// still leaves public until it commits. A Prove inside the switch's transaction reads both.
type proveCall struct {
	project       string
	ownerLockHeld bool
	committed     string
}

// insideTheSwitch is a Prove made while a switch to private of project had not committed.
func insideTheSwitch(project string) proveCall {
	return proveCall{project: project, ownerLockHeld: true, committed: model.ProjectPublic}
}

// recordingAdmission is a test's private-hub admission proof (HubAdmission). It notes every Prove,
// reading project's owner lock and committed visibility, and answers each with proveErr.
type recordingAdmission struct {
	pool     *store.Pool
	project  string
	proveErr error

	mu    sync.Mutex
	calls []proveCall
}

func (a *recordingAdmission) Prove(context.Context) error {
	// Each read takes a connection of its own: the call's context holds the switch's transaction,
	// under which the pool refuses it a second one (store.ErrNestedAcquire).
	call := proveCall{project: a.project, committed: readVisibility(context.Background(), a.pool, a.project)}
	taken, err := store.TryLockOwners(context.Background(), a.pool, store.ProjectOwner(a.project))
	call.ownerLockHeld = err == nil && len(taken) == 0
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls = append(a.calls, call)
	return a.proveErr
}

func (a *recordingAdmission) taken() []proveCall {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.calls)
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
// carol's private project, which alice cannot see, under options, proving the hubs' admission
// through admission, or through no proof when admission is nil. Its store is complete, the
// sessions' projects filled included.
func newPrivateProjectsServer(t *testing.T, options testServerOptions, admission *recordingAdmission) (http.Handler, *store.Store) {
	t.Helper()
	options.store = storetest.Open(t)
	if admission != nil {
		admission.pool = options.store.Pool
		if admission.project == "" {
			admission.project = "PRIV"
		}
		options.hubAdmission = admission
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

// requireRefused fails, naming key, unless PATCH key {visibility: private, ...} answered status
// with code and a message naming each of naming, and left key as it was: public, alice's, and with
// no event appended past before.
func requireRefused(t *testing.T, database *store.Store, response *httptest.ResponseRecorder, key string, before int64, status int, code string, naming ...string) {
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

// TestAProjectBecomesPrivateOnlyWhenTheDeploymentAllows is c-dispatch-needs-callout's Dispatch row
// (R12). A deployment that names no private project answers a switch to private 409
// PRIVATE_PROJECTS_OFF with privateOffBody's bytes; one that names others answers 409
// PRIVATE_PROJECT_NOT_PROVISIONED and makes no proof. For a project the list names, the switch's
// last step before its commit is the hub-admission proof, made while its transaction holds the
// project, and a proof that fails, or no proof at all, rolls the whole write back and answers 409
// PRIVATE_PROJECTS_UNAVAILABLE naming what failed. No other write makes a proof: not public, and
// nothing after a commit that failed, since no copy of a project's privacy exists outside the
// database to repair.
func TestAProjectBecomesPrivateOnlyWhenTheDeploymentAllows(t *testing.T) {
	private := map[string]string{"visibility": model.ProjectPrivate}
	public := map[string]string{"visibility": model.ProjectPublic}
	provisioned := []string{"PRIV"}
	// R12's refusal: the second of the deployment's hubs, rendered without the private-projects
	// tag, refuses the proof's new connection, and the proof names it.
	const refusingHub = "nats://hub-b.example:4222"
	refusal := errors.New("hub " + refusingHub + " refused the admission proof: nats: authorization violation")
	proven := insideTheSwitch("PRIV")

	t.Run("an empty list", func(t *testing.T) {
		admission := &recordingAdmission{}
		handler, database := newPrivateProjectsServer(t, testServerOptions{}, admission)
		before := allEvents(t, database)
		// The owner, and alice on a key she cannot see, are answered alike, before any read.
		hidden := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/projects/HIDE/access", private, "alice")
		response := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/projects/PRIV/access", private, "alice")
		for call, answer := range map[string]*httptest.ResponseRecorder{"PRIV, which alice owns": response, "HIDE, which alice cannot see": hidden} {
			if body := strings.TrimSpace(answer.Body.String()); answer.Code != http.StatusConflict || body != privateOffBody {
				t.Fatalf("PATCH %s private with an empty list answered %d %s, want 409 and %s", call, answer.Code, body, privateOffBody)
			}
		}
		requireRefused(t, database, response, "PRIV", before, http.StatusConflict, "PRIVATE_PROJECTS_OFF")
		if calls := admission.taken(); len(calls) != 0 {
			t.Fatalf("an empty list: the refused switch made the proof: %+v", calls)
		}
	})

	// Each marker a backfill writes once it is done: without it the switch is refused, naming the
	// command that writes it.
	for _, marker := range []struct{ table, command string }{
		{"session_projects_filled", "envoy-dispatch backfill-session-projects"},
		{"access_events_scrubbed", "envoy-dispatch scrub-access-events"},
	} {
		t.Run(marker.table+" empty", func(t *testing.T) {
			admission := &recordingAdmission{}
			handler, database := newPrivateProjectsServer(t, testServerOptions{privateProjects: provisioned}, admission)
			if _, err := database.Pool.Exec(context.Background(), `delete from `+marker.table); err != nil {
				t.Fatalf("empty %s: %v", marker.table, err)
			}
			before := allEvents(t, database)
			response := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/projects/PRIV/access", private, "alice")
			requireRefused(t, database, response, "PRIV", before, http.StatusConflict, "PRIVATE_PROJECTS_INCOMPLETE", marker.table, marker.command)
			if calls := admission.taken(); len(calls) != 0 {
				t.Fatalf("%s empty: the refused switch made the proof: %+v", marker.table, calls)
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
			admission := &recordingAdmission{}
			handler, database := newPrivateProjectsServer(t, testServerOptions{privateProjects: provisioned}, admission)
			if created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{"project": "PRIV", "title": "One"}, "alice"); created.Code != http.StatusCreated {
				t.Fatalf("create PRIV-1: %d %s", created.Code, created.Body.String())
			}
			if tag, err := database.Pool.Exec(context.Background(), broken.sql); err != nil || tag.RowsAffected() != 1 {
				t.Fatalf("%s: %v, %d rows", broken.name, err, tag.RowsAffected())
			}
			before := allEvents(t, database)
			response := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/projects/PRIV/access", private, "alice")
			requireRefused(t, database, response, "PRIV", before, http.StatusConflict, "PRIVATE_PROJECTS_INCOMPLETE", broken.line)
			if body := response.Body.String(); strings.Contains(body, broken.refuse) {
				t.Fatalf("%s: the refusal %s holds %q, a cure its line does not name", broken.name, body, broken.refuse)
			}
			if calls := admission.taken(); len(calls) != 0 {
				t.Fatalf("%s: the refused switch made the proof: %+v", broken.name, calls)
			}
		})
	}

	t.Run("a project the list does not name", func(t *testing.T) {
		admission := &recordingAdmission{project: "OTHER"}
		handler, database := newPrivateProjectsServer(t, testServerOptions{privateProjects: provisioned}, admission)
		before := allEvents(t, database)
		response := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/projects/OTHER/access", private, "alice")
		requireRefused(t, database, response, "OTHER", before, http.StatusConflict, "PRIVATE_PROJECT_NOT_PROVISIONED", "OTHER", "DISPATCH_PRIVATE_PROJECTS")
		if calls := admission.taken(); len(calls) != 0 {
			t.Fatalf("OTHER, which the list does not name: the refused switch made the proof: %+v", calls)
		}
	})

	t.Run("a hub that refuses the proof", func(t *testing.T) {
		admission := &recordingAdmission{proveErr: refusal}
		handler, database := newPrivateProjectsServer(t, testServerOptions{privateProjects: provisioned}, admission)
		before := allEvents(t, database)
		// The owner change in the same body goes back with it: the proof is the switch's last step.
		response := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/projects/PRIV/access",
			map[string]string{"visibility": model.ProjectPrivate, "owner": accessCarol}, "alice")
		if visibilityOf(t, database, "PRIV") != model.ProjectPublic {
			t.Fatalf("a hub that refuses the proof: the switch made PRIV private (%d %s)", response.Code, response.Body.String())
		}
		requireRefused(t, database, response, "PRIV", before, http.StatusConflict, "PRIVATE_PROJECTS_UNAVAILABLE", refusingHub)
		requireOnlyTheProof(t, admission, proven, "a hub that refuses the proof")
	})

	t.Run("no proof", func(t *testing.T) {
		handler, database := newPrivateProjectsServer(t, testServerOptions{privateProjects: provisioned}, nil)
		before := allEvents(t, database)
		response := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/projects/PRIV/access", private, "alice")
		requireRefused(t, database, response, "PRIV", before, http.StatusConflict, "PRIVATE_PROJECTS_UNAVAILABLE", "DISPATCH_PRIVATE_HUBS")
	})

	t.Run("a proof that admits", func(t *testing.T) {
		admission := &recordingAdmission{}
		handler, database := newPrivateProjectsServer(t, testServerOptions{privateProjects: provisioned}, admission)
		made := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/projects/PRIV/access", private, "alice")
		if made.Code != http.StatusOK || decodeBody[model.ProjectAccess](t, made).Visibility != model.ProjectPrivate ||
			visibilityOf(t, database, "PRIV") != model.ProjectPrivate {
			t.Fatalf("PATCH PRIV private with a proof that admits = %d %s, PRIV %s; want 200 and PRIV private",
				made.Code, made.Body.String(), visibilityOf(t, database, "PRIV"))
		}
		// public makes no proof, and leaves nothing to remove: no copy of PRIV's privacy exists
		// outside the database.
		made = dispatchRequest(t, handler, http.MethodPatch, "/api/v1/projects/PRIV/access", public, "alice")
		if made.Code != http.StatusOK || visibilityOf(t, database, "PRIV") != model.ProjectPublic {
			t.Fatalf("PATCH PRIV public = %d %s; want 200 and PRIV public", made.Code, made.Body.String())
		}
		requireOnlyTheProof(t, admission, proven, "making PRIV private, then public")
	})

	t.Run("a commit that fails after the proof", func(t *testing.T) {
		admission := &recordingAdmission{}
		handler, database := newPrivateProjectsServer(t, testServerOptions{privateProjects: provisioned}, admission)
		// A deferred trigger refuses the commit of any write that makes a project private, after
		// every statement of it, the proof included, has run.
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
		requireOnlyTheProof(t, admission, proven, "a commit that failed after the proof")
	})
}

// requireOnlyTheProof fails unless admission noted exactly one Prove, want, made inside the
// switch's transaction, naming any call made outside one.
func requireOnlyTheProof(t *testing.T, admission *recordingAdmission, want proveCall, row string) {
	t.Helper()
	calls := admission.taken()
	for _, call := range calls {
		if call != want {
			t.Errorf("%s: a Prove outside the switch's transaction: %s's owner lock held %t, %s committed %s",
				row, call.project, call.ownerLockHeld, call.project, call.committed)
		}
	}
	if !slices.Equal(calls, []proveCall{want}) {
		t.Fatalf("%s: the routes made the proofs %+v, want only %+v", row, calls, want)
	}
}

// TestThePrivateListAnswersOnlyCommittedRows: every listener reads which projects are private, and
// their Slack channels, from GET /api/v1/access/private-projects through the broker, uncached, so
// no copy can lag behind a switch. While PRIV's switch to private is held at its commit, after its
// proof, the broker's read does not list PRIV; the read after the commit lists it; a channel write
// on PRIV shows in the next read; and the read after PRIV's switch to public does not list it.
// Control: answer the route from a copy read once and kept for 30 s, as privateProjectsState keeps
// the header's, and the read right after the commit lacks PRIV, and the test fails naming the read.
func TestThePrivateListAnswersOnlyCommittedRows(t *testing.T) {
	ctx := context.Background()
	server := newChannelSwitchServer(t, &recordingAdmission{})
	lists := func(t *testing.T) (model.PrivateProjectChannels, bool) {
		t.Helper()
		project, listed := server.privateList(t)["PRIV"]
		return project, listed
	}
	if _, listed := lists(t); listed {
		t.Fatal("the broker's read lists PRIV before its switch to private")
	}
	// PRIV's switch is held at its commit, after every statement of it, its proof included, until
	// the test releases the lock the trigger waits for.
	if _, err := server.database.Pool.Exec(ctx, `
		create function hold_priv_commit() returns trigger language plpgsql as $$
		begin
			perform pg_advisory_xact_lock(hashtext('the committed-rows row holds PRIV''s commit'));
			return null;
		end $$;
		create constraint trigger hold_priv_commit after update on projects
			deferrable initially deferred for each row when (new.key = 'PRIV')
			execute function hold_priv_commit();
	`); err != nil {
		t.Fatalf("hold every commit that writes PRIV: %v", err)
	}
	held, err := server.database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the transaction that holds PRIV's commit: %v", err)
	}
	defer held.Rollback(ctx)
	if _, err := held.Exec(ctx, `select pg_advisory_xact_lock(hashtext('the committed-rows row holds PRIV''s commit'))`); err != nil {
		t.Fatalf("hold PRIV's commit: %v", err)
	}
	switched := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		switched <- accessPatch(ctx, server.handler, "PRIV", `{"visibility":"private"}`, "alice")
	}()
	waitForDatabaseLocks(t, held, 1)
	if _, listed := lists(t); listed {
		t.Fatal("the broker's read while PRIV's switch is held at its commit lists PRIV: it must answer the committed rows")
	}
	if err := held.Rollback(ctx); err != nil {
		t.Fatalf("release PRIV's commit: %v", err)
	}
	select {
	case response := <-switched:
		if response.Code != http.StatusOK {
			t.Fatalf("PATCH PRIV private = %d %s, want 200", response.Code, response.Body.String())
		}
	case <-time.After(30 * time.Second):
		t.Fatal("PRIV's switch never answered once its commit was released")
	}
	if _, listed := lists(t); !listed {
		t.Fatal("the broker's read right after PRIV's switch committed does not list PRIV: it must answer the committed rows")
	}
	if response := server.patch(t, "PRIV", map[string]any{"slack_channels": []string{"C0PRIV00009"}}); response.Code != http.StatusOK {
		t.Fatalf("alice's PATCH of private PRIV's channels = %d %s, want 200", response.Code, response.Body.String())
	}
	if project, _ := lists(t); !slices.Equal(project.SlackChannels, []string{"C0PRIV00009"}) {
		t.Fatalf("the broker's read after PRIV's channel write lists %v for PRIV, want [C0PRIV00009]", project.SlackChannels)
	}
	if response := server.patch(t, "PRIV", map[string]string{"visibility": model.ProjectPublic}); response.Code != http.StatusOK {
		t.Fatalf("PATCH PRIV public = %d %s, want 200", response.Code, response.Body.String())
	}
	if _, listed := lists(t); listed {
		t.Fatal("the broker's read after PRIV's switch to public still lists PRIV")
	}
}

// fullSwitchOf is project key's full_switch_at as committed, nil while it is null.
func fullSwitchOf(t *testing.T, database *store.Store, key string) *time.Time {
	t.Helper()
	var at *time.Time
	if err := database.Pool.QueryRow(context.Background(), `select full_switch_at from projects where key = $1`, key).Scan(&at); err != nil {
		t.Fatalf("read %s's full switch: %v", key, err)
	}
	return at
}

// TestAFullSwitchRunsTheWholePathOnceForADayOnePrivateProject: a project can be private before the
// full switch (a deployment's day one, with no provisioning and no proof), and then it records no
// full switch. With its key on the list, its owner's switch to private runs the whole path as a
// switch from public does: a proof that fails answers 409 PRIVATE_PROJECTS_UNAVAILABLE and records
// nothing, and a proof that passes records the full switch (full_switch_at). The switch of a project
// the full switch made private is a no-op: no proof, no event, the record as it was. A switch to
// public clears the record, and a switch from public records it. Controls, each on its own: record
// nothing at the switch, and the passing row fails naming PRIV; read every private project as
// switched already, and the day-one row answers 200 with no proof; never read a switch as done, and
// the re-switch row makes a proof and appends an event.
func TestAFullSwitchRunsTheWholePathOnceForADayOnePrivateProject(t *testing.T) {
	private := map[string]string{"visibility": model.ProjectPrivate}
	admission := &recordingAdmission{proveErr: errors.New("hub nats://hub-b.example:4222 refused the admission proof")}
	handler, database := newPrivateProjectsServer(t, testServerOptions{privateProjects: []string{"OTHER", "PRIV"}}, admission)
	// Day one: PRIV went private with no full switch.
	if _, err := database.Pool.Exec(context.Background(), `update projects set visibility = 'private' where key = 'PRIV'`); err != nil {
		t.Fatalf("make PRIV private before its full switch: %v", err)
	}

	before := allEvents(t, database)
	refused := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/projects/PRIV/access", private, "alice")
	if body := refused.Body.String(); refused.Code != http.StatusConflict || !strings.Contains(body, `"code":"PRIVATE_PROJECTS_UNAVAILABLE"`) {
		t.Fatalf("day-one PRIV's full switch with a refusing proof = %d %s, want 409 PRIVATE_PROJECTS_UNAVAILABLE", refused.Code, body)
	}
	if at, calls, appended := fullSwitchOf(t, database, "PRIV"), len(admission.taken()), allEvents(t, database)-before; at != nil || calls != 1 || appended != 0 {
		t.Fatalf("day-one PRIV's refused full switch: full switch %v, %d proofs, %d events appended; want none recorded, one proof, nothing appended", at, calls, appended)
	}

	admission.proveErr = nil
	switched := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/projects/PRIV/access", private, "alice")
	at := fullSwitchOf(t, database, "PRIV")
	if switched.Code != http.StatusOK || at == nil || len(admission.taken()) != 2 {
		t.Fatalf("day-one PRIV's full switch with a passing proof = %d %s, full switch %v, %d proofs; want 200, the switch recorded, a second proof",
			switched.Code, switched.Body.String(), at, len(admission.taken()))
	}

	before = allEvents(t, database)
	again := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/projects/PRIV/access", private, "alice")
	if again.Code != http.StatusOK || decodeBody[model.ProjectAccess](t, again).Visibility != model.ProjectPrivate {
		t.Fatalf("fully switched PRIV's switch to private again = %d %s, want 200 and PRIV private", again.Code, again.Body.String())
	}
	if after, calls, appended := fullSwitchOf(t, database, "PRIV"), len(admission.taken()), allEvents(t, database)-before; after == nil || !after.Equal(*at) || calls != 2 || appended != 0 {
		t.Fatalf("fully switched PRIV's switch to private again: full switch %v (was %v), %d proofs, %d events appended; want a no-op", after, at, calls, appended)
	}

	if response := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/projects/PRIV/access", map[string]string{"visibility": model.ProjectPublic}, "alice"); response.Code != http.StatusOK {
		t.Fatalf("PATCH PRIV public = %d %s, want 200", response.Code, response.Body.String())
	}
	if at := fullSwitchOf(t, database, "PRIV"); at != nil {
		t.Fatalf("PRIV made public still records the full switch %v, want it cleared", at)
	}
	admission.project = "OTHER"
	if response := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/projects/OTHER/access", private, "alice"); response.Code != http.StatusOK {
		t.Fatalf("PATCH OTHER private = %d %s, want 200", response.Code, response.Body.String())
	}
	if at := fullSwitchOf(t, database, "OTHER"); at == nil {
		t.Fatal("OTHER's switch from public to private records no full switch")
	}
}

// The maintenance sweep logs, on every pass, an error for each private project the list does not
// name, and, where the list names any, at its first pass and then once an hour, an error naming
// any row a project owns whose node does not name its project. With an empty list it reads no row
// of the census.
func TestTheMaintenanceSweepNamesARowAPrivateProjectWouldAnswerWrongly(t *testing.T) {
	for _, row := range []struct {
		name       string
		list       []string
		incomplete int
	}{
		{"a list", []string{"PRIV"}, 1},
		{"an empty list", nil, 0},
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
				}, row.list)
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
			// bob's PRIV: carol on its access list, an issue whose spec is a document, and a mapped
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
			if response := agentRequest(t, handler, http.MethodPut, "/api/v1/settings/repo-projects/acme/priv", map[string]any{"project": "PRIV", "actor": sessionActor()}, "agent-token"); response.Code != http.StatusOK {
				t.Fatalf("map acme/priv to PRIV: %d %s", response.Code, response.Body.String())
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
