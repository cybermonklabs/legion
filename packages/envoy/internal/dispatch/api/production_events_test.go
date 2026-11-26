package api

// production_events_test.go holds the three routes of the record of deploys and failed changes
// (production_events.go; dispatch://INFRA-978, R6, R8 and R10) to what they answer and to what
// they leave in the table: each refusal is read by its code and by the row count, or the row's
// state, it left unchanged.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/legion"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/prodevents"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// productionEventsPath is the record's path of one project.
func productionEventsPath(project string) string {
	return "/api/v1/projects/" + project + "/production-events"
}

// productionEventStatePath is the state route of one row of one project.
func productionEventStatePath(project, id string) string {
	return productionEventsPath(project) + "/" + id + "/state"
}

// writeProductionDeploy stores a deploy of project through the store, as the GitHub adapter does:
// source github, source_ref github:<run>, finished at finished.
func writeProductionDeploy(t *testing.T, database *store.Store, project string, run int64, finished time.Time, commits ...string) prodevents.Event {
	t.Helper()
	stored, _, err := prodevents.Write(context.Background(), database.Pool, prodevents.Event{
		ProjectKey: project, Kind: prodevents.KindDeploy, Source: prodevents.SourceGitHub,
		SourceRef: prodevents.GitHubSourceRef(run), StartedAt: &finished, FinishedAt: &finished,
		Repo: "acme/app", Commits: commits, Actor: model.Actor{Kind: "session", ID: "ses-adapter"},
	})
	if err != nil {
		t.Fatalf("write deploy run %d of %s: %v", run, project, err)
	}
	return stored
}

// writeProductionFailure stores a failure of project blaming deploy, detected at detected.
func writeProductionFailure(t *testing.T, database *store.Store, project, ref, deploy string, detected time.Time) prodevents.Event {
	t.Helper()
	signal := prodevents.SignalAPI
	stored, _, err := prodevents.Write(context.Background(), database.Pool, prodevents.Event{
		ProjectKey: project, Kind: prodevents.KindFailure, Source: prodevents.SourceAPI, SourceRef: ref,
		DeployID: &deploy, Signal: &signal, DetectedAt: &detected, Actor: model.Actor{Kind: "session", ID: "ses-alerts"},
	})
	if err != nil {
		t.Fatalf("write failure %s of %s: %v", ref, project, err)
	}
	return stored
}

// readProductionEvent reads one row back through the store.
func readProductionEvent(t *testing.T, database *store.Store, project, id string) prodevents.Event {
	t.Helper()
	event, err := prodevents.Get(context.Background(), database.Pool, project, id)
	if err != nil {
		t.Fatalf("read production event %s of %s: %v", id, project, err)
	}
	return event
}

// TestASystemPostsDeploysAndFailuresAndReadsThemBack: an agent token posts a deploy under
// OpenTelemetry's names for its environment, status and url, and the row holds them under the
// plain names; a repeat of its source_ref answers the first row, 200, and writes nothing; a
// failure names its deploy by OpenTelemetry's run id (the GitHub adapter's row of that run), by a
// commit and by the row's id, each the signal api; and the list read answers the envelope with
// its window, kind, state, deploy_id and page applied.
func TestASystemPostsDeploysAndFailuresAndReadsThemBack(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	createTestProject(t, handler, "PRD")
	now := time.Now().UTC().Truncate(time.Second)
	adapter := writeProductionDeploy(t, database, "PRD", 4242, now.Add(-3*time.Hour), "1111111111111111111111111111111111111111")
	agent := map[string]any{"kind": "session", "id": "ses-ci"}

	finished := now.Add(-2 * time.Hour)
	posted := bearerRequest(t, handler, http.MethodPost, productionEventsPath("PRD"), map[string]any{
		"kind": "deploy", "source_ref": "ci:deploy-17", "started_at": now.Add(-150 * time.Minute), "finished_at": finished,
		"deployment.environment.name": "staging", "deployment.status": "succeeded",
		"cicd.pipeline.run.url.full": "https://ci.example.com/runs/17",
		"repo":                       "acme/app", "commits": []string{"ABCDEF0123456789ABCDEF0123456789ABCDEF01"}, "pull_requests": []string{"acme/app#7"},
		"actor": agent,
	})
	if posted.Code != http.StatusCreated {
		t.Fatalf("post a deploy: status=%d body=%s, want 201", posted.Code, posted.Body.String())
	}
	deploy := decodeBody[prodevents.Event](t, posted)
	switch {
	case deploy.Kind != prodevents.KindDeploy || deploy.Source != prodevents.SourceAPI || deploy.ProjectKey != "PRD":
		t.Fatalf("the posted deploy is %s from %s of %s, want an api deploy of PRD", deploy.Kind, deploy.Source, deploy.ProjectKey)
	case deploy.Environment != "staging" || deploy.Status != "succeeded" || deploy.URL != "https://ci.example.com/runs/17":
		t.Fatalf("the OTel names stored environment %q, status %q, url %q; want staging, succeeded and the run's page",
			deploy.Environment, deploy.Status, deploy.URL)
	case len(deploy.Commits) != 1 || deploy.Commits[0] != "abcdef0123456789abcdef0123456789abcdef01":
		t.Fatalf("the deploy's commits are %v, want the one commit in lower case", deploy.Commits)
	case deploy.Actor.Kind != "session" || deploy.Actor.ID != "ses-ci" || deploy.State != prodevents.StateCounted:
		t.Fatalf("the deploy's actor is %s:%s and its state %s, want the session ses-ci and counted", deploy.Actor.Kind, deploy.Actor.ID, deploy.State)
	}

	repeat := bearerRequest(t, handler, http.MethodPost, productionEventsPath("PRD"), map[string]any{
		"kind": "deploy", "source_ref": "ci:deploy-17", "finished_at": now, "status": "failed", "actor": agent,
	})
	if repeat.Code != http.StatusOK {
		t.Fatalf("repeat the deploy: status=%d body=%s, want 200", repeat.Code, repeat.Body.String())
	}
	if again := decodeBody[prodevents.Event](t, repeat); again.ID != deploy.ID || again.Status != "succeeded" || !again.FinishedAt.Equal(finished) {
		t.Fatalf("the repeat answered row %s with status %q, finished %v; want the first row %s as it was stored", again.ID, again.Status, again.FinishedAt, deploy.ID)
	}
	if rows := countRows(t, database, `select count(*) from production_events`); rows != 2 {
		t.Fatalf("the record holds %d rows after the repeat, want the adapter's deploy and the posted one", rows)
	}

	type failurePost struct {
		name string
		body map[string]any
		want string
	}
	for _, post := range []failurePost{
		{"by OTel's run id", map[string]any{
			"cicd.pipeline.run.id": "4242", "repo": "acme/app", "deployment.status": "degraded",
			"deployment.environment.name": "production", "cicd.pipeline.run.url.full": "https://ci.example.com/runs/4242",
		}, adapter.ID},
		{"by a commit", map[string]any{"commit_sha": "ABCDEF0123456789ABCDEF0123456789ABCDEF01"}, deploy.ID},
		{"by the row's id", map[string]any{"deploy_id": deploy.ID, "detected_at": now.Add(-time.Hour)}, deploy.ID},
	} {
		body := map[string]any{"kind": "failure", "source_ref": "alerts:" + post.name, "actor": agent}
		for key, value := range post.body {
			body[key] = value
		}
		response := bearerRequest(t, handler, http.MethodPost, productionEventsPath("PRD"), body)
		if response.Code != http.StatusCreated {
			t.Fatalf("post a failure %s: status=%d body=%s, want 201", post.name, response.Code, response.Body.String())
		}
		failure := decodeBody[prodevents.Event](t, response)
		if failure.DeployID == nil || *failure.DeployID != post.want || failure.Signal == nil || *failure.Signal != prodevents.SignalAPI ||
			failure.Source != prodevents.SourceAPI || failure.DetectedAt == nil {
			t.Fatalf("the failure %s blames %v with signal %v from %s, detected %v; want deploy %s, signal api, a detected time",
				post.name, failure.DeployID, failure.Signal, failure.Source, failure.DetectedAt, post.want)
		}
		if post.name == "by OTel's run id" && (failure.Status != "degraded" || failure.Environment != "production" || failure.URL != "https://ci.example.com/runs/4242") {
			t.Fatalf("the failure by run id stored status %q, environment %q, url %q; want what its OTel names carried", failure.Status, failure.Environment, failure.URL)
		}
	}
	if rows := countRows(t, database, `select count(*) from production_events`); rows != 5 {
		t.Fatalf("the record holds %d rows, want two deploys and three failures", rows)
	}

	conflict := bearerRequest(t, handler, http.MethodPost, productionEventsPath("PRD"), map[string]any{
		"kind": "deploy", "source_ref": "ci:deploy-18", "finished_at": now, "status": "succeeded", "deployment.status": "failed", "actor": agent,
	})
	expectRefusal(t, "a status named two ways", conflict, http.StatusBadRequest, prodevents.CodeInput, "status", "deployment.status")
	if rows := countRows(t, database, `select count(*) from production_events`); rows != 5 {
		t.Fatalf("a refused post left %d rows, want 5", rows)
	}

	window := url.Values{"from": {now.Add(-24 * time.Hour).Format(time.RFC3339)}, "to": {now.Add(time.Hour).Format(time.RFC3339)}}
	read := func(extra url.Values) productionEventList {
		t.Helper()
		query := url.Values{}
		for key, values := range window {
			query[key] = values
		}
		for key, values := range extra {
			query[key] = values
		}
		response := dispatchRequest(t, handler, http.MethodGet, productionEventsPath("PRD")+"?"+query.Encode(), nil, "alice")
		if response.Code != http.StatusOK {
			t.Fatalf("read %v: status=%d body=%s", query, response.Code, response.Body.String())
		}
		return decodeBody[productionEventList](t, response)
	}
	if page := read(nil); page.Total != 5 || len(page.Items) != 5 || page.Limit != 50 || page.Offset != 0 {
		t.Fatalf("the window answered total %d, %d items, limit %d, offset %d; want 5, 5, 50, 0", page.Total, len(page.Items), page.Limit, page.Offset)
	}
	if page := read(url.Values{"kind": {"deploy"}}); page.Total != 2 || page.Items[0].ID != deploy.ID || page.Items[1].ID != adapter.ID {
		t.Fatalf("the deploys answered total %d, want the posted deploy and then the adapter's, newest first", page.Total)
	}
	if page := read(url.Values{"kind": {"failure"}, "limit": {"1"}, "offset": {"1"}}); page.Total != 3 || len(page.Items) != 1 || page.Limit != 1 || page.Offset != 1 {
		t.Fatalf("the second page of one failure answered total %d, %d items, limit %d, offset %d", page.Total, len(page.Items), page.Limit, page.Offset)
	}
	if page := read(url.Values{"state": {"rejected"}}); page.Total != 0 || len(page.Items) != 0 {
		t.Fatalf("no row is rejected, and the read answered total %d", page.Total)
	}
	if page := read(url.Values{"deploy_id": {deploy.ID}}); page.Total != 2 {
		t.Fatalf("the failures that blame deploy %s answered total %d, want the two posted by its commit and its id", deploy.ID, page.Total)
	} else {
		for _, item := range page.Items {
			if item.Kind != prodevents.KindFailure || item.DeployID == nil || *item.DeployID != deploy.ID {
				t.Fatalf("the deploy_id read answered %s %s blaming %v, want only failures that blame %s", item.Kind, item.ID, item.DeployID, deploy.ID)
			}
		}
	}
	if page := read(url.Values{"deploy_id": {"0195f2a8-0000-7000-8000-000000000000"}}); page.Total != 0 || len(page.Items) != 0 {
		t.Fatalf("a deploy_id of no row answered total %d, want an empty page", page.Total)
	}
	old := url.Values{"from": {now.Add(-30 * 24 * time.Hour).Format(time.RFC3339)}, "to": {now.Add(-29 * 24 * time.Hour).Format(time.RFC3339)}}
	response := dispatchRequest(t, handler, http.MethodGet, productionEventsPath("PRD")+"?"+old.Encode(), nil, "alice")
	if page := decodeBody[productionEventList](t, response); response.Code != http.StatusOK || page.Total != 0 {
		t.Fatalf("a window a month back answered %d with total %d, want 200 and none", response.Code, page.Total)
	}

	for _, refused := range []struct{ query, names string }{
		{"deploy_id=not-a-uuid", "deploy_id"},
		{"kind=rollback", "kind"},
		{"state=pending", "state"},
		{"scope=roots", "scope"},
		{"kind=deploy&kind=failure", "kind"},
	} {
		response := dispatchRequest(t, handler, http.MethodGet, productionEventsPath("PRD")+"?"+refused.query, nil, "alice")
		expectRefusal(t, "the read of ?"+refused.query, response, http.StatusBadRequest, "INVALID_QUERY", refused.names)
	}
	expectRefusal(t, "the read of an unknown project",
		dispatchRequest(t, handler, http.MethodGet, productionEventsPath("NOPE"), nil, "alice"), http.StatusNotFound, "PROJECT_NOT_FOUND")
	if anonymous := dispatchRequest(t, handler, http.MethodGet, productionEventsPath("PRD"), nil, ""); anonymous.Code != http.StatusUnauthorized {
		t.Fatalf("an anonymous read: status=%d body=%s, want 401", anonymous.Code, anonymous.Body.String())
	}
}

// TestAPersonFlagsAFailureAndPostsNoDeploy: a person's credential posts a failure, the drill-down's
// flag, as the signal person with the person as actor and the server's source_ref, so a second flag
// of the deploy, its id spelled in capitals included, answers the first row and writes nothing; a
// source_ref other than the server's is refused; and a deploy from a person's credential is
// refused.
func TestAPersonFlagsAFailureAndPostsNoDeploy(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	createTestProject(t, handler, "PRD")
	deploy := writeProductionDeploy(t, database, "PRD", 7, time.Now().UTC().Add(-time.Hour))

	const note = "The deploy broke the sign-in page; reverted by hand."
	flag := map[string]any{"kind": "failure", "deploy_id": deploy.ID, "note": note}
	flagged := dispatchRequest(t, handler, http.MethodPost, productionEventsPath("PRD"), flag, "alice")
	if flagged.Code != http.StatusCreated {
		t.Fatalf("a person's flag: status=%d body=%s, want 201", flagged.Code, flagged.Body.String())
	}
	failure := decodeBody[prodevents.Event](t, flagged)
	if failure.Signal == nil || *failure.Signal != prodevents.SignalPerson || failure.Source != prodevents.SourcePerson ||
		failure.Actor.Kind != "user" || failure.Actor.ID != "alice" || failure.SourceRef != "person:"+deploy.ID+":alice" {
		t.Fatalf("the flag is signal %v from %s by %s:%s under %q; want signal person by alice under person:%s:alice",
			failure.Signal, failure.Source, failure.Actor.Kind, failure.Actor.ID, failure.SourceRef, deploy.ID)
	}
	if failure.Note != note {
		t.Fatalf("the flag's note reads back %q, want %q", failure.Note, note)
	}
	long := dispatchRequest(t, handler, http.MethodPost, productionEventsPath("PRD"), map[string]any{
		"kind": "failure", "deploy_id": deploy.ID, "note": strings.Repeat("x", productionEventNoteBytes+1),
	}, "bob")
	expectRefusal(t, "a note past its bound", long, http.StatusBadRequest, prodevents.CodeInput, "note")
	again := dispatchRequest(t, handler, http.MethodPost, productionEventsPath("PRD"), flag, "alice")
	if again.Code != http.StatusOK || decodeBody[prodevents.Event](t, again).ID != failure.ID {
		t.Fatalf("a second flag: status=%d body=%s, want 200 and the first flag", again.Code, again.Body.String())
	}
	capitals := dispatchRequest(t, handler, http.MethodPost, productionEventsPath("PRD"), map[string]any{
		"kind": "failure", "deploy_id": strings.ToUpper(deploy.ID), "note": note,
	}, "alice")
	if capitals.Code != http.StatusOK || decodeBody[prodevents.Event](t, capitals).ID != failure.ID {
		t.Fatalf("a second flag naming the deploy in capitals: status=%d body=%s, want 200 and the first flag", capitals.Code, capitals.Body.String())
	}
	other := dispatchRequest(t, handler, http.MethodPost, productionEventsPath("PRD"), map[string]any{
		"kind": "failure", "deploy_id": deploy.ID, "source_ref": "mine",
	}, "alice")
	expectRefusal(t, "a person's flag under its own source_ref", other, http.StatusBadRequest, prodevents.CodeInput, "person:"+deploy.ID+":alice")
	deployed := dispatchRequest(t, handler, http.MethodPost, productionEventsPath("PRD"), map[string]any{
		"kind": "deploy", "source_ref": "by-hand", "finished_at": time.Now().UTC(),
	}, "alice")
	expectRefusal(t, "a person's deploy", deployed, http.StatusForbidden, codeProductionEventDeployForbidden)
	if rows := countRows(t, database, `select count(*) from production_events`); rows != 2 {
		t.Fatalf("the record holds %d rows, want the deploy and the one flag", rows)
	}
}

// TestEveryOtherCallerOfTheWritesIsRefusedAndWritesNothing: no credential, a broker token scoped
// to another project, a service-only broker token and a read-only token are each refused both
// writes by their own code; an unknown project is 404; a system's post without source_ref and a
// failure whose deploy no row matches (by commit, by run and by id) are refused. None of them
// writes a row or moves the failure's state, and the read-only token still reads the list.
func TestEveryOtherCallerOfTheWritesIsRefusedAndWritesNothing(t *testing.T) {
	rig := newBrokerTokenRig(t)
	caller := newBrokerCaller(t)
	const scoped, serviceOnly, readOnly = "a-token-of-another-project", "a-token-naming-no-person", "a-token-that-only-reads"
	rig.broker.Says(scoped, liveNamedToken(caller.jkt))
	rig.broker.Says(serviceOnly, serviceOnlyToken(caller.jkt))
	rig.broker.Says(readOnly, readOnlyToken(caller.jkt))
	for _, key := range []string{brokerTestProject, "OTHER"} {
		if created := rig.asPerson(t, http.MethodPost, "/api/v1/projects", map[string]any{"key": key, "name": key}); created.Code != http.StatusCreated {
			t.Fatalf("create project %s: status=%d body=%s", key, created.Code, created.Body.String())
		}
	}
	now := time.Now().UTC()
	deploy := writeProductionDeploy(t, rig.database, "OTHER", 1, now.Add(-time.Hour), "2222222222222222222222222222222222222222")
	failure := writeProductionFailure(t, rig.database, "OTHER", "alerts:1", deploy.ID, now.Add(-time.Minute))
	rows := countRows(t, rig.database, `select count(*) from production_events`)

	post := map[string]any{"kind": "failure", "source_ref": "alerts:2", "deploy_id": deploy.ID}
	state := map[string]any{"state": "rejected", "reason": "Not a failure."}
	postPath, statePath := productionEventsPath("OTHER"), productionEventStatePath("OTHER", failure.ID)
	for _, refused := range []struct {
		name   string
		send   func(method, path string, body any) *httptest.ResponseRecorder
		status int
		code   string
	}{
		{"no credential", func(method, path string, body any) *httptest.ResponseRecorder {
			return rig.serve(brokerRequest(t, method, path, body, "", ""))
		}, http.StatusUnauthorized, "NO_IDENTITY"},
		{"a broker token scoped to another project", func(method, path string, body any) *httptest.ResponseRecorder {
			return rig.serve(caller.presenting(t, method, path, scoped, body))
		}, http.StatusForbidden, codeProjectScope},
		{"a service-only broker token", func(method, path string, body any) *httptest.ResponseRecorder {
			return rig.serve(caller.presenting(t, method, path, serviceOnly, body))
		}, http.StatusForbidden, contracts.DispatchSignInRequired},
		{"a read-only token", func(method, path string, body any) *httptest.ResponseRecorder {
			return rig.serve(caller.presenting(t, method, path, readOnly, body))
		}, http.StatusForbidden, contracts.DispatchReadOnlyToken},
	} {
		expectRefusal(t, refused.name+" posting", refused.send(http.MethodPost, postPath, post), refused.status, refused.code)
		expectRefusal(t, refused.name+" writing a state", refused.send(http.MethodPut, statePath, state), refused.status, refused.code)
	}

	shared := func(method, path string, body map[string]any) *httptest.ResponseRecorder {
		withActor := map[string]any{"actor": map[string]any{"kind": "session", "id": "ses-ci"}}
		for key, value := range body {
			withActor[key] = value
		}
		return rig.serve(brokerRequest(t, method, path, withActor, "Bearer agent-token", ""))
	}
	expectRefusal(t, "a post to an unknown project", shared(http.MethodPost, productionEventsPath("NOPE"), post), http.StatusNotFound, "PROJECT_NOT_FOUND")
	expectRefusal(t, "a state write in an unknown project", rig.asPerson(t, http.MethodPut, productionEventStatePath("NOPE", failure.ID), state),
		http.StatusNotFound, "PROJECT_NOT_FOUND")
	expectRefusal(t, "a system's post without source_ref", shared(http.MethodPost, postPath, map[string]any{"kind": "failure", "deploy_id": deploy.ID}),
		http.StatusBadRequest, prodevents.CodeInput, "source_ref")
	for name, names := range map[string]map[string]any{
		"a commit no deploy shipped": {"commit_sha": "3333333333333333333333333333333333333333"},
		"a run no deploy row is":     {"cicd.pipeline.run.id": "999", "repo": "acme/app"},
		"an id no row has":           {"deploy_id": "0195f2a8-0000-7000-8000-000000000000"},
	} {
		body := map[string]any{"kind": "failure", "source_ref": "alerts:" + name}
		for key, value := range names {
			body[key] = value
		}
		expectRefusal(t, "a failure blaming "+name, shared(http.MethodPost, postPath, body), http.StatusConflict, prodevents.CodeNoDeploy)
	}

	if after := countRows(t, rig.database, `select count(*) from production_events`); after != rows {
		t.Fatalf("the refused writes left %d rows, want the %d there were", after, rows)
	}
	if stored := readProductionEvent(t, rig.database, "OTHER", failure.ID); stored.State != prodevents.StateCounted || stored.StateReason != nil {
		t.Fatalf("a refused state write moved the failure to %s (%v), want it counted as it was", stored.State, stored.StateReason)
	}
	listed := rig.serve(caller.presenting(t, http.MethodGet, productionEventsPath("OTHER"), readOnly, nil))
	if page := decodeBody[productionEventList](t, listed); listed.Code != http.StatusOK || page.Total != 2 {
		t.Fatalf("the read-only token's read: status=%d body=%s, want 200 and both rows", listed.Code, listed.Body.String())
	}
}

// TestTheStateRouteTakesAPersonAndTheProvenControllerAlone: a person rejects and restores a
// failure; the project's controller, the session the daemon's newest read names and a broker
// token proves, re-counts it and may not restore it; once a later read names another session the
// same token is refused; a session that is neither, and the shared token, which proves none, are
// refused; and a row of another project is not the path's project's to write.
func TestTheStateRouteTakesAPersonAndTheProvenControllerAlone(t *testing.T) {
	rig := newBrokerTokenRig(t)
	caller := newBrokerCaller(t)
	const controller, neither = "the-controllers-token", "a-token-of-another-session"
	rig.broker.Says(controller, liveNamedToken(caller.jkt))
	another := liveNamedToken(caller.jkt)
	another.Session = "ses_neither"
	rig.broker.Says(neither, another)
	for _, key := range []string{brokerTestProject, "OTHER"} {
		if created := rig.asPerson(t, http.MethodPost, "/api/v1/projects", map[string]any{"key": key, "name": key}); created.Code != http.StatusCreated {
			t.Fatalf("create project %s: status=%d body=%s", key, created.Code, created.Body.String())
		}
	}
	now := time.Now().UTC()
	deploy := writeProductionDeploy(t, rig.database, brokerTestProject, 1, now.Add(-time.Hour))
	failure := writeProductionFailure(t, rig.database, brokerTestProject, "alerts:1", deploy.ID, now.Add(-time.Minute))
	path := productionEventStatePath(brokerTestProject, failure.ID)
	readDaemon := func(at time.Time, session string) {
		t.Helper()
		state := legion.State{ControllerLocator: &legion.ControllerLocator{SessionID: session}}
		if _, err := legion.RecordRead(context.Background(), rig.database.Pool, brokerTestProject, at, 1, state.Observations()); err != nil {
			t.Fatalf("record a read naming controller %s: %v", session, err)
		}
	}
	stateOf := func() prodevents.Event {
		t.Helper()
		return readProductionEvent(t, rig.database, brokerTestProject, failure.ID)
	}

	rejected := rig.asPerson(t, http.MethodPut, path, map[string]any{"state": "rejected", "reason": "A flaky probe."})
	if rejected.Code != http.StatusOK {
		t.Fatalf("a person's reject: status=%d body=%s, want 200", rejected.Code, rejected.Body.String())
	}
	if row := decodeBody[prodevents.Event](t, rejected); row.State != prodevents.StateRejected || row.StateReason == nil ||
		*row.StateReason != "A flaky probe." || row.StateBy == nil || row.StateBy.Kind != "user" || row.StateBy.ID != brokerTestPerson || row.StateAt == nil {
		t.Fatalf("the reject answered state %s, reason %v, writer %v at %v; want rejected with its reason, by %s, with a time",
			row.State, row.StateReason, row.StateBy, row.StateAt, brokerTestPerson)
	}
	restored := rig.asPerson(t, http.MethodPut, path, map[string]any{"state": "restored"})
	if row := decodeBody[prodevents.Event](t, restored); restored.Code != http.StatusOK || row.RestoredAt == nil || row.RestoredBy == nil || row.RestoredBy.ID != brokerTestPerson {
		t.Fatalf("a person's restore: status=%d body=%s, want 200 with its time and %s", restored.Code, restored.Body.String(), brokerTestPerson)
	}

	// Before any read names it, the controller's session is no controller.
	expectRefusal(t, "a session no read names", rig.serve(caller.presenting(t, http.MethodPut, path, controller, map[string]any{
		"state": "counted", "reason": "It was real.",
	})), http.StatusForbidden, codeProductionEventStateForbidden, brokerTestSession)
	readDaemon(now.Add(-2*time.Minute), brokerTestSession)
	counted := rig.serve(caller.presenting(t, http.MethodPut, path, controller, map[string]any{"state": "counted", "reason": "It was real."}))
	if counted.Code != http.StatusOK {
		t.Fatalf("the controller's re-count: status=%d body=%s, want 200", counted.Code, counted.Body.String())
	}
	if row := decodeBody[prodevents.Event](t, counted); row.State != prodevents.StateCounted || row.StateBy == nil ||
		row.StateBy.Kind != "session" || row.StateBy.ID != brokerTestSession || row.StateReason == nil || *row.StateReason != "It was real." {
		t.Fatalf("the re-count answered state %s by %v for %v; want counted by the session %s with its reason", row.State, row.StateBy, row.StateReason, brokerTestSession)
	}
	expectRefusal(t, "the controller's restore", rig.serve(caller.presenting(t, http.MethodPut, path, controller, map[string]any{"state": "restored"})),
		http.StatusForbidden, codeProductionEventStateForbidden)
	expectRefusal(t, "a session that is neither", rig.serve(caller.presenting(t, http.MethodPut, path, neither, map[string]any{
		"state": "rejected", "reason": "Says another session.",
	})), http.StatusForbidden, codeProductionEventStateForbidden, "ses_neither")
	expectRefusal(t, "the shared token", rig.serve(brokerRequest(t, http.MethodPut, path, map[string]any{
		"state": "rejected", "reason": "Says the shared token.",
	}, "Bearer agent-token", "")), http.StatusForbidden, codeProductionEventStateForbidden)

	readDaemon(now.Add(-time.Minute), "ses_successor")
	expectRefusal(t, "the controller after a read names another session", rig.serve(caller.presenting(t, http.MethodPut, path, controller, map[string]any{
		"state": "rejected", "reason": "Too late.",
	})), http.StatusForbidden, codeProductionEventStateForbidden, brokerTestSession)
	if row := stateOf(); row.State != prodevents.StateCounted || row.StateBy == nil || row.StateBy.ID != brokerTestSession {
		t.Fatalf("the refused writes moved the failure to %s by %v, want the controller's re-count to stand", row.State, row.StateBy)
	}

	otherDeploy := writeProductionDeploy(t, rig.database, "OTHER", 2, now.Add(-time.Hour))
	otherFailure := writeProductionFailure(t, rig.database, "OTHER", "alerts:2", otherDeploy.ID, now.Add(-time.Minute))
	expectRefusal(t, "a row of another project", rig.asPerson(t, http.MethodPut, productionEventStatePath(brokerTestProject, otherFailure.ID),
		map[string]any{"state": "rejected", "reason": "Not this project's."}), http.StatusNotFound, codeProductionEventNotFound)
	if row := readProductionEvent(t, rig.database, "OTHER", otherFailure.ID); row.State != prodevents.StateCounted {
		t.Fatalf("the refused write moved OTHER's failure to %s, want counted", row.State)
	}
}

// TestAReadOnlyTokensReadOfTheRecordWritesOnlyItsProof: the list read is a read (readClasses,
// readWritesNothing), so it is held to what every read is held to: for a read-only token it
// writes nothing but the row that spends its DPoP proof. The positive controls are that insert and
// the handler's own read of production_events.
func TestAReadOnlyTokensReadOfTheRecordWritesOnlyItsProof(t *testing.T) {
	database := storetest.Open(t)
	marked := &markedStatements{}
	rig := newBrokerTokenRigOn(t, testServerOptions{store: storetest.TracedStore(t, database, marked)})
	caller := newBrokerCaller(t)
	const token = "a-read-only-token-reading-the-record"
	rig.broker.Says(token, readOnlyToken(caller.jkt))
	if created := rig.asPerson(t, http.MethodPost, "/api/v1/projects", map[string]any{"key": "RDR", "name": "Reader"}); created.Code != http.StatusCreated {
		t.Fatalf("create project RDR: status=%d body=%s", created.Code, created.Body.String())
	}
	writeProductionDeploy(t, database, "RDR", 1, time.Now().UTC().Add(-time.Hour))
	marked.take()

	request := caller.presenting(t, http.MethodGet, productionEventsPath("RDR"), token, nil)
	request = request.WithContext(context.WithValue(request.Context(), countedRead{}, true))
	response := rig.serve(request)
	if page := decodeBody[productionEventList](t, response); response.Code != http.StatusOK || page.Total != 1 {
		t.Fatalf("the read-only token's read: status=%d body=%s, want 200 and the one deploy", response.Code, response.Body.String())
	}
	requireOnlyProofAndCaches(t, marked.take(), regexp.MustCompile(`(?i)\bfrom\s+production_events\b`))
}

// requireOnlyProofAndCaches fails unless every write of statements is to the proof's replay row or
// a cache Dispatch rebuilds, the proof's insert among them, and handlerRead, the handler's own
// read, is among them too. Each statement is read by writesOf (read_only_token_test.go).
func requireOnlyProofAndCaches(t *testing.T, statements []string, handlerRead *regexp.Regexp) {
	t.Helper()
	allowed := map[string]bool{"dpop_proofs": true, "text_trees": true, "route_roster": true}
	var proofInserted, readByHandler bool
	var written []string
	for _, statement := range statements {
		writes, readable := writesOf(statement)
		if !readable {
			t.Errorf("the request ran a write this test cannot read a table for: %s", statement)
		}
		for _, write := range writes {
			written = append(written, write)
			verb, table, _ := strings.Cut(write, " ")
			if !allowed[table] {
				t.Errorf("the request wrote %s, which is neither its proof's replay row nor a cache Dispatch rebuilds: %s", table, statement)
			}
			proofInserted = proofInserted || (verb == "insert" && table == "dpop_proofs")
		}
		readByHandler = readByHandler || handlerRead.MatchString(statement)
	}
	if !proofInserted {
		t.Errorf("the tracer saw no insert into dpop_proofs among %d statements: it did not see the request's proof spent", len(statements))
	}
	if !readByHandler {
		t.Errorf("the tracer saw no statement matching %s among %d statements: it did not see the handler's reads", handlerRead, len(statements))
	}
	t.Logf("the request ran %d statements, whose writes were %v", len(statements), written)
}
