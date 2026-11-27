package api

// production_events_test.go holds the three routes of the record of deploys and failed changes
// (production_events.go; dispatch://INFRA-978, R6, R8 and R10) to what they answer and to what
// they leave in the table: each refusal is read by its code and by the row count, or the row's
// state, it left unchanged.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/ciphertoken"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/evals"
	"github.com/sjawhar/envoy/internal/dispatch/legion"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/prodevents"
	"github.com/sjawhar/envoy/internal/dispatch/prodevents/signals"
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
// source github, source_ref github:<run>, a run of acme/app finished at finished that ships no
// pull request. Such a deploy counts for the project whose deploy source reads its repository
// (evals' source key, INFRA-978 R12), so the helper also publishes acme/app as project's source
// repository (publishDeployRepo), the record a refresher's page leaves of it, and the deploy counts
// for project as the record's routes require of a deploy they name (evals.DeployCountsFor). A later
// call for another project moves acme/app's source to that project, as a source moved there does.
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
	publishDeployRepo(t, database, project, deployRule("acme/app", "production / apply"), finished)
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
// its window, kind, state, deploy_id and page applied. The other systems that write do so too: a
// pod's service-account token posts a deploy, and so does a broker token an account entry naming
// no person and no project minted, which writes as its account, as the sign-in rule of
// writeNeedsASignedInPerson lets it (a machine's token that names a service alone does not,
// which TestEveryOtherCallerOfTheWritesIsRefusedAndWritesNothing holds).
func TestASystemPostsDeploysAndFailuresAndReadsThemBack(t *testing.T) {
	issuer := newServiceTokenIssuer(t)
	rig := newBrokerTokenRigOn(t, testServerOptions{oidc: issuer.verifier(t)})
	handler, database := rig.handler, rig.database
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

	// A pod's service-account token holds no scope and proves no session, so it names the session
	// that writes, and its verified subject rides on the row as the service. It writes a deploy
	// as a system does, and a repeat of its source_ref answers the first row.
	podToken := issuer.mint(t, serviceTokenAudience)
	podDeploy := map[string]any{
		"kind": "deploy", "source_ref": "k8s:deploy-1", "finished_at": now.Add(-30 * time.Minute), "deployment.status": "succeeded",
		"actor": map[string]any{"kind": "session", "id": "ses-pod"},
	}
	podPosted := agentRequest(t, handler, http.MethodPost, productionEventsPath("PRD"), podDeploy, podToken)
	if podPosted.Code != http.StatusCreated {
		t.Fatalf("a service-account token's deploy: status=%d body=%s, want 201", podPosted.Code, podPosted.Body.String())
	}
	pod := decodeBody[prodevents.Event](t, podPosted)
	if pod.Kind != prodevents.KindDeploy || pod.Source != prodevents.SourceAPI || pod.ProjectKey != "PRD" || pod.State != prodevents.StateCounted ||
		pod.Actor.Kind != "session" || pod.Actor.ID != "ses-pod" || pod.Actor.Service == nil || *pod.Actor.Service != serviceTokenSubject {
		t.Fatalf("the pod's deploy is %s from %s of %s, state %s, by %+v; want an api deploy of PRD, counted, by the session ses-pod under the service %s",
			pod.Kind, pod.Source, pod.ProjectKey, pod.State, pod.Actor, serviceTokenSubject)
	}
	podAgain := agentRequest(t, handler, http.MethodPost, productionEventsPath("PRD"), podDeploy, podToken)
	if podAgain.Code != http.StatusOK || decodeBody[prodevents.Event](t, podAgain).ID != pod.ID {
		t.Fatalf("a repeat of the pod's deploy: status=%d body=%s, want 200 and the first row", podAgain.Code, podAgain.Body.String())
	}

	// A broker token an account entry naming no person and no project minted writes as its
	// account, recorded as the session the token proved (TestAPersonlessTokenWritesAsItsAccount).
	caller := newBrokerCaller(t)
	const accountToken = "a-token-of-an-account-entry-naming-no-person"
	rig.broker.Says(accountToken, personlessAccountToken(caller.jkt))
	accountPosted := rig.serve(caller.presenting(t, http.MethodPost, productionEventsPath("PRD"), accountToken, map[string]any{
		"kind": "deploy", "source_ref": "account:deploy-1", "finished_at": now.Add(-20 * time.Minute),
	}))
	if accountPosted.Code != http.StatusCreated {
		t.Fatalf("an account entry's token's deploy: status=%d body=%s, want 201", accountPosted.Code, accountPosted.Body.String())
	}
	account := decodeBody[prodevents.Event](t, accountPosted)
	if account.Kind != prodevents.KindDeploy || account.Source != prodevents.SourceAPI || account.Actor.Kind != "session" ||
		account.Actor.ID != brokerTestSession || !account.Actor.Proven || account.Actor.Service == nil || *account.Actor.Service != brokerTestAccount {
		t.Fatalf("the account's deploy is %s from %s by %+v; want an api deploy by the session %s the token proved, under the service %s",
			account.Kind, account.Source, account.Actor, brokerTestSession, brokerTestAccount)
	}
	if rows := countRows(t, database, `select count(*) from production_events`); rows != 7 {
		t.Fatalf("the record holds %d rows, want the five before, the pod's deploy and the account's", rows)
	}
}

// TestAPersonFlagsAFailureAndPostsNoDeploy: a person's credential posts a failure, the drill-down's
// flag, as the signal person with the person as actor and the server's source_ref; a flag naming
// two pull requests and carrying a note keeps both and the note as sent (R7), in its answer and
// in the stored row; a second flag of the deploy, its id spelled in capitals included, answers
// the first row and writes nothing; a source_ref other than the server's is refused; and a deploy
// from a person's credential is refused.
func TestAPersonFlagsAFailureAndPostsNoDeploy(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	createTestProject(t, handler, "PRD")
	deploy := writeProductionDeploy(t, database, "PRD", 7, time.Now().UTC().Add(-time.Hour))

	const note = "The deploy broke the sign-in page; reverted by hand."
	pulls := []string{"acme/app#7", "acme/app#9"}
	flag := map[string]any{"kind": "failure", "deploy_id": deploy.ID, "note": note, "pull_requests": pulls}
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
	if failure.Note != note || !slices.Equal(failure.PullRequests, pulls) {
		t.Fatalf("the flag's answer holds note %q and pull requests %v, want %q and %v", failure.Note, failure.PullRequests, note, pulls)
	}
	if stored := readProductionEvent(t, database, "PRD", failure.ID); stored.Note != note || !slices.Equal(stored.PullRequests, pulls) {
		t.Fatalf("the stored flag holds note %q and pull requests %v, want %q and %v", stored.Note, stored.PullRequests, note, pulls)
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

// TestASystemsPostUnderAnAutomaticSourcesKeyIsRefused: the record holds one row per project and
// source_ref, so a system's deploy or failure posted under the key an automatic source writes its
// own row under would leave that source's upsert writing nothing: the GitHub adapter's deploy of a
// run, a person's flag, and the check, revert and incident signals' failures. A post under each of
// those keys is refused 400 PRODUCTION_EVENT_INPUT naming the key's prefix and writes nothing; a
// plain key is still taken, and so is one that holds such a prefix past its start.
func TestASystemsPostUnderAnAutomaticSourcesKeyIsRefused(t *testing.T) {
	t.Parallel()
	handler, database := newTestHandlerWithStore(t)
	createTestProject(t, handler, "PRD")
	now := time.Now().UTC()
	deploy := writeProductionDeploy(t, database, "PRD", 7, now.Add(-time.Hour))
	agent := map[string]any{"kind": "session", "id": "ses-ci"}
	rows := countRows(t, database, `select count(*) from production_events`)
	for prefix, ref := range map[string]string{
		"github:":   prodevents.GitHubSourceRef(8),
		"person:":   prodevents.PersonSourceRef(deploy.ID, "alice"),
		"check:":    signals.CheckSourceRef("0195f2a8-0000-7000-8000-000000000001", "block-1", deploy.ID),
		"revert:":   signals.RevertSourceRef("acme/app", 9),
		"incident:": signals.IncidentSourceRef("PRD-1"),
	} {
		for kind, body := range map[string]map[string]any{
			prodevents.KindDeploy:  {"finished_at": now},
			prodevents.KindFailure: {"deploy_id": deploy.ID},
		} {
			body["kind"], body["source_ref"], body["actor"] = kind, ref, agent
			expectRefusal(t, "a system's "+kind+" under "+ref, bearerRequest(t, handler, http.MethodPost, productionEventsPath("PRD"), body),
				http.StatusBadRequest, prodevents.CodeInput, prefix)
		}
	}
	if after := countRows(t, database, `select count(*) from production_events`); after != rows {
		t.Fatalf("the refused posts left %d rows, want the %d there were", after, rows)
	}
	for _, ref := range []string{"ci:deploy-1", "ci:github:8"} {
		posted := bearerRequest(t, handler, http.MethodPost, productionEventsPath("PRD"), map[string]any{
			"kind": prodevents.KindDeploy, "source_ref": ref, "finished_at": now, "actor": agent,
		})
		if posted.Code != http.StatusCreated {
			t.Fatalf("a system's deploy under %s: status=%d body=%s, want 201", ref, posted.Code, posted.Body.String())
		}
	}
}

// TestEveryOtherCallerOfTheWritesIsRefusedAndWritesNothing: no credential, a broker token scoped
// to one project writing a private project it does not name (main's project-scope rule closes
// that one and no public project: project_scope.go), a machine's service-only broker token (a
// service and no person, under the project scope person, which writes nothing because nobody has
// signed in on that machine; a token an account entry naming no person minted writes as its
// account, as TestASystemPostsDeploysAndFailuresAndReadsThemBack holds), a token the broker
// revoked and a read-only token are each refused both writes by their own code; an unknown
// project is 404; a system's post without source_ref and a failure whose deploy no row matches
// (by commit, by run and by id) are refused. None of them writes a row or moves the failure's
// state, and the read-only token still reads the list.
func TestEveryOtherCallerOfTheWritesIsRefusedAndWritesNothing(t *testing.T) {
	rig := newBrokerTokenRig(t)
	caller := newBrokerCaller(t)
	const scoped, serviceOnly, revoked, readOnly = "a-token-of-another-project", "a-token-naming-no-person", "a-token-the-broker-revoked", "a-token-that-only-reads"
	rig.broker.Says(scoped, liveNamedToken(caller.jkt))
	rig.broker.Says(serviceOnly, serviceOnlyToken(caller.jkt))
	rig.broker.Says(revoked, ciphertoken.Introspection{Active: false})
	rig.broker.Says(readOnly, readOnlyToken(caller.jkt))
	for _, key := range []string{brokerTestProject, "OTHER", "HIDDEN"} {
		if created := rig.asPerson(t, http.MethodPost, "/api/v1/projects", map[string]any{"key": key, "name": key}); created.Code != http.StatusCreated {
			t.Fatalf("create project %s: status=%d body=%s", key, created.Code, created.Body.String())
		}
	}
	if _, err := rig.database.Pool.Exec(t.Context(), `update projects set visibility = 'private' where key = 'HIDDEN'`); err != nil {
		t.Fatalf("mark HIDDEN private: %v", err)
	}
	now := time.Now().UTC()
	deploy := writeProductionDeploy(t, rig.database, "OTHER", 1, now.Add(-time.Hour), "2222222222222222222222222222222222222222")
	failure := writeProductionFailure(t, rig.database, "OTHER", "alerts:1", deploy.ID, now.Add(-time.Minute))
	hiddenDeploy := writeProductionDeploy(t, rig.database, "HIDDEN", 2, now.Add(-time.Hour), "4444444444444444444444444444444444444444")
	hiddenFailure := writeProductionFailure(t, rig.database, "HIDDEN", "alerts:3", hiddenDeploy.ID, now.Add(-time.Minute))
	rows := countRows(t, rig.database, `select count(*) from production_events`)

	post := map[string]any{"kind": "failure", "source_ref": "alerts:2", "deploy_id": deploy.ID}
	state := map[string]any{"state": "rejected", "reason": "Not a failure."}
	postPath, statePath := productionEventsPath("OTHER"), productionEventStatePath("OTHER", failure.ID)
	for _, refused := range []struct {
		name   string
		send   func(method, path string, body any) *httptest.ResponseRecorder
		status int
		code   string
		// hidden is whether the row writes the private project: the one a token scoped to
		// another project is refused, where a public one it may write.
		hidden bool
	}{
		{"no credential", func(method, path string, body any) *httptest.ResponseRecorder {
			return rig.serve(brokerRequest(t, method, path, body, "", ""))
		}, http.StatusUnauthorized, "NO_IDENTITY", false},
		{"a broker token scoped to another project", func(method, path string, body any) *httptest.ResponseRecorder {
			return rig.serve(caller.presenting(t, method, path, scoped, body))
		}, http.StatusForbidden, codeProjectScope, true},
		{"a machine's service-only broker token", func(method, path string, body any) *httptest.ResponseRecorder {
			return rig.serve(caller.presenting(t, method, path, serviceOnly, body))
		}, http.StatusForbidden, contracts.DispatchSignInRequired, false},
		{"a revoked broker token", func(method, path string, body any) *httptest.ResponseRecorder {
			return rig.serve(caller.presenting(t, method, path, revoked, body))
		}, http.StatusUnauthorized, "BROKER_TOKEN_NOT_ACTIVE", false},
		{"a read-only token", func(method, path string, body any) *httptest.ResponseRecorder {
			return rig.serve(caller.presenting(t, method, path, readOnly, body))
		}, http.StatusForbidden, contracts.DispatchReadOnlyToken, false},
	} {
		posted, stated, body := postPath, statePath, post
		if refused.hidden {
			posted, stated = productionEventsPath("HIDDEN"), productionEventStatePath("HIDDEN", hiddenFailure.ID)
			body = map[string]any{"kind": "failure", "source_ref": "alerts:4", "deploy_id": hiddenDeploy.ID}
		}
		expectRefusal(t, refused.name+" posting", refused.send(http.MethodPost, posted, body), refused.status, refused.code)
		expectRefusal(t, refused.name+" writing a state", refused.send(http.MethodPut, stated, state), refused.status, refused.code)
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

// TestAFailurePostedToOneProjectCannotBlameAPrivateProjectsDeploy: a broker token scoped to BRK
// posts, to BRK's own route, a failure that blames a deploy of HIDDEN, a private project the token
// does not name, by the deploy's row id, by its GitHub run and by a commit it shipped. A failure
// counts in every project its deploy counts for (evals' failure_days.go), so such a row moves
// HIDDEN's change fail rate through a credential that may not write HIDDEN (project_scope.go), and
// an answer other than the one a deploy no row holds gets tells the token which commits and runs
// HIDDEN deployed. Each post is answered as a deploy no row holds is, and writes no row (found by
// INFRA-978's tester at a37567ad, where each was 201 and the private project's rate moved).
func TestAFailurePostedToOneProjectCannotBlameAPrivateProjectsDeploy(t *testing.T) {
	rig := newBrokerTokenRig(t)
	caller := newBrokerCaller(t)
	const scoped = "a-token-of-BRK"
	rig.broker.Says(scoped, liveNamedToken(caller.jkt))
	for _, key := range []string{brokerTestProject, "HIDDEN"} {
		if created := rig.asPerson(t, http.MethodPost, "/api/v1/projects", map[string]any{"key": key, "name": key}); created.Code != http.StatusCreated {
			t.Fatalf("create project %s: status=%d body=%s", key, created.Code, created.Body.String())
		}
	}
	if _, err := rig.database.Pool.Exec(t.Context(), `update projects set visibility = 'private' where key = 'HIDDEN'`); err != nil {
		t.Fatalf("mark HIDDEN private: %v", err)
	}
	const commit = "5555555555555555555555555555555555555555"
	hidden := writeProductionDeploy(t, rig.database, "HIDDEN", 77, time.Now().UTC().Add(-time.Hour), commit)
	post := func(ref string, names map[string]any) *httptest.ResponseRecorder {
		body := map[string]any{"kind": "failure", "source_ref": ref}
		for key, value := range names {
			body[key] = value
		}
		return rig.serve(caller.presenting(t, http.MethodPost, productionEventsPath(brokerTestProject), scoped, body))
	}
	none := post("alerts:none", map[string]any{"deploy_id": "0195f2a8-0000-7000-8000-000000000000"})
	if none.Code != http.StatusConflict {
		t.Fatalf("a failure blaming a deploy no row holds: status=%d body=%s, want 409", none.Code, none.Body.String())
	}
	want := decodeBody[map[string]any](t, none)["code"]
	rows := countRows(t, rig.database, `select count(*) from production_events`)
	for name, names := range map[string]map[string]any{
		"its row id": {"deploy_id": hidden.ID},
		"its run":    {"cicd.pipeline.run.id": "77", "repo": "acme/app"},
		"its commit": {"commit_sha": commit},
	} {
		got := post("alerts:"+name, names)
		if got.Code != none.Code || decodeBody[map[string]any](t, got)["code"] != want {
			t.Errorf("a failure blaming HIDDEN's deploy by %s: status=%d body=%s, want %d %v as for a deploy no row holds",
				name, got.Code, got.Body.String(), none.Code, want)
		}
	}
	if after := countRows(t, rig.database, `select count(*) from production_events`); after != rows {
		t.Fatalf("the posts left %d rows, want the %d there were", after, rows)
	}
}

// TestARestoreCannotNameADeployThatFinishedBeforeTheFailedDeploy: Mark restored naming, as the
// restoring deploy, one that finished before the deploy the failure blames is refused and leaves
// the failure unrestored, while one that finished after it restores it. A deploy cannot restore a
// failure of a deploy that came after it, and deployment-rework-rate counts the restoring deploy
// on its own day (evals' failure_days.go), so the earlier one would count as rework in a window
// the failure is not in (found by INFRA-978's tester at a37567ad, where the earlier deploy was
// taken, 200).
func TestARestoreCannotNameADeployThatFinishedBeforeTheFailedDeploy(t *testing.T) {
	rig := newBrokerTokenRig(t)
	if created := rig.asPerson(t, http.MethodPost, "/api/v1/projects", map[string]any{"key": brokerTestProject, "name": brokerTestProject}); created.Code != http.StatusCreated {
		t.Fatalf("create project %s: status=%d body=%s", brokerTestProject, created.Code, created.Body.String())
	}
	now := time.Now().UTC().Truncate(time.Second)
	earlier := writeProductionDeploy(t, rig.database, brokerTestProject, 1, now.Add(-3*time.Hour))
	failed := writeProductionDeploy(t, rig.database, brokerTestProject, 2, now.Add(-2*time.Hour))
	later := writeProductionDeploy(t, rig.database, brokerTestProject, 3, now.Add(-time.Hour))
	failure := writeProductionFailure(t, rig.database, brokerTestProject, "alerts:1", failed.ID, now.Add(-90*time.Minute))
	path := productionEventStatePath(brokerTestProject, failure.ID)
	refused := rig.asPerson(t, http.MethodPut, path, map[string]any{"state": "restored", "restored_by_deploy": earlier.ID})
	if refused.Code < 400 || refused.Code >= 500 {
		t.Errorf("a restore by a deploy that finished an hour before the failed one: status=%d body=%s, want a refusal", refused.Code, refused.Body.String())
	}
	if stored := readProductionEvent(t, rig.database, brokerTestProject, failure.ID); stored.RestoredAt != nil || stored.RestoredByDeploy != nil {
		t.Errorf("the refused restore left restored_at %v by deploy %v, want the failure unrestored", stored.RestoredAt, deref(stored.RestoredByDeploy))
	}
	restored := rig.asPerson(t, http.MethodPut, path, map[string]any{"state": "restored", "restored_by_deploy": later.ID})
	if stored := readProductionEvent(t, rig.database, brokerTestProject, failure.ID); restored.Code != http.StatusOK || stored.RestoredByDeploy == nil || *stored.RestoredByDeploy != later.ID {
		t.Fatalf("a restore by the deploy an hour after the failed one: status=%d body=%s, want 200 naming %s", restored.Code, restored.Body.String(), later.ID)
	}
}

// The same attribution rule holds for a public project's deploy, even for a caller that may
// write both projects. By row ID, run, or commit, PRD cannot blame a deploy attributed only to PUB.
// It gets the same response as a missing deploy and learns no ID from PUB's record.
// deploy it was not sent, and writes no row; posted to PUB's own route, the same failure is taken.
func TestAFailurePostedToOneProjectCannotBlameAPublicProjectsDeploy(t *testing.T) {
	t.Parallel()
	handler, database := newTestHandlerWithStore(t)
	createTestProject(t, handler, "PRD", "PUB")
	const commit = "6666666666666666666666666666666666666666"
	public := writeProductionDeploy(t, database, "PUB", 88, time.Now().UTC().Add(-time.Hour), commit)
	post := func(project, ref string, names map[string]any) *httptest.ResponseRecorder {
		t.Helper()
		body := map[string]any{"kind": "failure", "source_ref": ref, "actor": map[string]any{"kind": "session", "id": "ses-ci"}}
		for key, value := range names {
			body[key] = value
		}
		return bearerRequest(t, handler, http.MethodPost, productionEventsPath(project), body)
	}
	rows := countRows(t, database, `select count(*) from production_events`)
	for _, selector := range []struct {
		name, field, missing, public string
		with                         map[string]any
	}{
		{"its row id", "deploy_id", "0195f2a8-0000-7000-8000-000000000000", public.ID, nil},
		{"its run", "cicd.pipeline.run.id", "999", "88", map[string]any{"repo": "acme/app"}},
		{"its commit", "commit_sha", "3333333333333333333333333333333333333333", commit, nil},
	} {
		names := func(value string) map[string]any {
			named := map[string]any{selector.field: value}
			for key, with := range selector.with {
				named[key] = with
			}
			return named
		}
		missing := post("PRD", "alerts:no deploy by "+selector.name, names(selector.missing))
		want := decodeBody[apiErrorBody](t, missing)
		if missing.Code != http.StatusConflict || want.Code != prodevents.CodeNoDeploy {
			t.Fatalf("a failure blaming no deploy by %s: status=%d body=%s, want 409 %s", selector.name, missing.Code, missing.Body.String(), prodevents.CodeNoDeploy)
		}
		refused := post("PRD", "alerts:PUB's deploy by "+selector.name, names(selector.public))
		got, words := decodeBody[apiErrorBody](t, refused), strings.ReplaceAll(want.Error, selector.missing, selector.public)
		if refused.Code != missing.Code || got.Code != want.Code || got.Error != words {
			t.Errorf("a failure blaming PUB's deploy by %s: status=%d body=%s, want %d %s %q, as for a deploy no row holds",
				selector.name, refused.Code, refused.Body.String(), missing.Code, want.Code, words)
		}
		if selector.field != "deploy_id" && strings.Contains(refused.Body.String(), public.ID) {
			t.Errorf("the refusal of PUB's deploy by %s names its id %s: %s", selector.name, public.ID, refused.Body.String())
		}
	}
	if after := countRows(t, database, `select count(*) from production_events`); after != rows {
		t.Fatalf("the refused posts left %d rows, want the %d there were", after, rows)
	}
	if own := post("PUB", "alerts:PUB's own", map[string]any{"deploy_id": public.ID}); own.Code != http.StatusCreated {
		t.Fatalf("the failure posted to PUB's own route: status=%d body=%s, want 201", own.Code, own.Body.String())
	}
}

// TestAFailurePostedByACommitBlamesTheEarliestDeployOfTheRoutesProject: one commit reached
// production first in OTH's deploys, a GitHub deploy of OTH's source repository and then a deploy
// posted to OTH, and later in PRD's, a GitHub deploy of PRD's source repository and then a deploy
// posted to PRD. A failure posted to PRD's route by that commit is taken, 201, and blames PRD's
// GitHub deploy, the earliest of the deploys that count for PRD: OTH's earlier ones count for OTH
// alone, so they neither turn the post into a refusal nor take its blame, and PRD's later one is
// not the first. A repeat of the post answers that row, 200, and a person's flag by the commit is
// keyed by that deploy. The automatic signals still blame the first deploy of any project to ship
// the commit, OTH's (prodevents.DeployForCommit). A commit that OTH's deploys alone hold is
// answered at PRD's route word for word as a commit no deploy holds, naming none of them, and
// writes nothing.
func TestAFailurePostedByACommitBlamesTheEarliestDeployOfTheRoutesProject(t *testing.T) {
	t.Parallel()
	handler, database := newTestHandlerWithStore(t)
	createTestProject(t, handler, "PRD", "OTH")
	now := time.Now().UTC().Truncate(time.Second)
	const shared, theirs, nobodys = "4444444444444444444444444444444444444444", "7777777777777777777777777777777777777777",
		"3333333333333333333333333333333333333333"
	first := now.Add(-4 * time.Hour)
	foreign := writeProductionEvent(t, database.Pool, prodevents.Event{
		ProjectKey: "OTH", Kind: prodevents.KindDeploy, Source: prodevents.SourceGitHub, SourceRef: prodevents.GitHubSourceRef(41),
		StartedAt: &first, FinishedAt: &first, Repo: "acme/other", Commits: []string{shared, theirs},
	})
	publishDeployRepo(t, database, "OTH", deployRule("acme/other", "production / apply"), first)
	foreignPosted := writeProductionEvent(t, database.Pool, prodevents.Event{
		ProjectKey: "OTH", Kind: prodevents.KindDeploy, Source: prodevents.SourceAPI, SourceRef: "ci:oth-deploy",
		FinishedAt: new(now.Add(-3 * time.Hour)), Commits: []string{shared, theirs},
	})
	local := writeProductionDeploy(t, database, "PRD", 42, now.Add(-2*time.Hour), shared)
	writeProductionEvent(t, database.Pool, prodevents.Event{
		ProjectKey: "PRD", Kind: prodevents.KindDeploy, Source: prodevents.SourceAPI, SourceRef: "ci:prd-deploy",
		FinishedAt: new(now.Add(-time.Hour)), Commits: []string{shared},
	})
	if automatic, held, err := prodevents.DeployForCommit(t.Context(), database.Pool, shared); err != nil || !held || automatic.ID != foreign.ID {
		t.Fatalf("the automatic signals' deploy of the commit is %q (held %v, err %v), want OTH's first deploy %s", automatic.ID, held, err, foreign.ID)
	}
	post := func(ref, commit string) *httptest.ResponseRecorder {
		t.Helper()
		return bearerRequest(t, handler, http.MethodPost, productionEventsPath("PRD"), map[string]any{
			"kind": "failure", "source_ref": ref, "commit_sha": commit, "actor": map[string]any{"kind": "session", "id": "ses-ci"},
		})
	}

	posted := post("alerts:shared commit", shared)
	if posted.Code != http.StatusCreated {
		t.Fatalf("PRD's failure by the shared commit: status=%d body=%s, want 201", posted.Code, posted.Body.String())
	}
	failure := decodeBody[prodevents.Event](t, posted)
	if failure.ProjectKey != "PRD" || failure.DeployID == nil || *failure.DeployID != local.ID {
		t.Fatalf("PRD's failure by the shared commit is a row of %s blaming %q, want PRD's blaming its first deploy %s",
			failure.ProjectKey, deref(failure.DeployID), local.ID)
	}
	if repeat := post("alerts:shared commit", shared); repeat.Code != http.StatusOK || decodeBody[prodevents.Event](t, repeat).ID != failure.ID {
		t.Fatalf("a repeat of PRD's failure: status=%d body=%s, want 200 and row %s", repeat.Code, repeat.Body.String(), failure.ID)
	}
	key := prodevents.PersonSourceRef(local.ID, "alice")
	flagged := dispatchRequest(t, handler, http.MethodPost, productionEventsPath("PRD"), map[string]any{"kind": "failure", "commit_sha": shared}, "alice")
	if flagged.Code != http.StatusCreated {
		t.Fatalf("alice's flag by the shared commit: status=%d body=%s, want 201", flagged.Code, flagged.Body.String())
	}
	if flag := decodeBody[prodevents.Event](t, flagged); flag.DeployID == nil || *flag.DeployID != local.ID || flag.SourceRef != key {
		t.Fatalf("alice's flag by the shared commit blames %q under %q, want %s under %s", deref(flag.DeployID), flag.SourceRef, local.ID, key)
	}

	rows := countRows(t, database, `select count(*) from production_events`)
	missing := post("alerts:nobody's commit", nobodys)
	want := decodeBody[apiErrorBody](t, missing)
	if missing.Code != http.StatusConflict || want.Code != prodevents.CodeNoDeploy {
		t.Fatalf("a failure by a commit no deploy holds: status=%d body=%s, want 409 %s", missing.Code, missing.Body.String(), prodevents.CodeNoDeploy)
	}
	refused := post("alerts:OTH's commit", theirs)
	got, words := decodeBody[apiErrorBody](t, refused), strings.ReplaceAll(want.Error, nobodys, theirs)
	if refused.Code != missing.Code || got.Code != want.Code || got.Error != words {
		t.Errorf("a failure by a commit OTH's deploys alone hold: status=%d body=%s, want %d %s %q, as for a commit no deploy holds",
			refused.Code, refused.Body.String(), missing.Code, want.Code, words)
	}
	for _, id := range []string{foreign.ID, foreignPosted.ID} {
		if strings.Contains(refused.Body.String(), id) {
			t.Errorf("the refusal of OTH's commit names OTH's deploy %s: %s", id, refused.Body.String())
		}
	}
	if after := countRows(t, database, `select count(*) from production_events`); after != rows {
		t.Fatalf("the refused posts left %d rows, want the %d there were", after, rows)
	}
}

// TestARestoreCannotNameAnotherProjectsDeploy: Mark restored naming, as the restoring deploy, a
// deploy posted to PUB, which counts for PUB alone and finished after the failed deploy, is
// answered as a restoring deploy no row holds is, word for word but the id, and leaves the failure
// unrestored. A deploy of the route's own project that finished at the same instant restores it.
func TestARestoreCannotNameAnotherProjectsDeploy(t *testing.T) {
	t.Parallel()
	handler, database := newTestHandlerWithStore(t)
	createTestProject(t, handler, "PRD", "PUB")
	now := time.Now().UTC().Truncate(time.Second)
	public := writeProductionEvent(t, database.Pool, prodevents.Event{
		ProjectKey: "PUB", Kind: prodevents.KindDeploy, Source: prodevents.SourceAPI, SourceRef: "ci:deploy-1", FinishedAt: new(now.Add(-time.Hour)),
	})
	failed := writeProductionDeploy(t, database, "PRD", 1, now.Add(-2*time.Hour))
	same := writeProductionDeploy(t, database, "PRD", 2, now.Add(-2*time.Hour))
	failure := writeProductionFailure(t, database, "PRD", "alerts:1", failed.ID, now.Add(-90*time.Minute))
	path := productionEventStatePath("PRD", failure.ID)
	restore := func(deploy string) *httptest.ResponseRecorder {
		t.Helper()
		return dispatchRequest(t, handler, http.MethodPut, path, map[string]any{"state": "restored", "restored_by_deploy": deploy}, "alice")
	}
	const none = "0195f2a8-0000-7000-8000-00000000000a"
	missing := restore(none)
	want := decodeBody[apiErrorBody](t, missing)
	if missing.Code != http.StatusConflict || want.Code != prodevents.CodeNoDeploy {
		t.Fatalf("a restore by a deploy no row holds: status=%d body=%s, want 409 %s", missing.Code, missing.Body.String(), prodevents.CodeNoDeploy)
	}
	refused := restore(public.ID)
	got, words := decodeBody[apiErrorBody](t, refused), strings.ReplaceAll(want.Error, none, public.ID)
	if refused.Code != missing.Code || got.Code != want.Code || got.Error != words {
		t.Errorf("a restore by PUB's deploy: status=%d body=%s, want %d %s %q, as for a deploy no row holds",
			refused.Code, refused.Body.String(), missing.Code, want.Code, words)
	}
	if stored := readProductionEvent(t, database, "PRD", failure.ID); stored.RestoredAt != nil || stored.RestoredByDeploy != nil {
		t.Errorf("the refused restores left restored_at %v by deploy %v, want the failure unrestored", stored.RestoredAt, deref(stored.RestoredByDeploy))
	}
	restored := restore(same.ID)
	if stored := readProductionEvent(t, database, "PRD", failure.ID); restored.Code != http.StatusOK || stored.RestoredByDeploy == nil || *stored.RestoredByDeploy != same.ID {
		t.Fatalf("a restore by PRD's deploy that finished with the failed one: status=%d body=%s, want 200 naming %s", restored.Code, restored.Body.String(), same.ID)
	}
}

// Invalid production event times and pull request references are refused without writing a row.
// A post refuses backwards times, a finish or detection more than five minutes ahead of the
// server, and malformed pull request references. Equal start and finish, a near-future detection,
// and the largest stored pull request number are accepted.
func TestAPostRefusesAnImpossibleTimeAndAMalformedPullRequest(t *testing.T) {
	t.Parallel()
	handler, database := newTestHandlerWithStore(t)
	createTestProject(t, handler, "PRD")
	now := time.Now().UTC()
	deploy := writeProductionDeploy(t, database, "PRD", 7, now.Add(-time.Hour))
	event := func(base, fields map[string]any) map[string]any {
		base["actor"] = map[string]any{"kind": "session", "id": "ses-ci"}
		for key, value := range fields {
			base[key] = value
		}
		return base
	}
	deployOf := func(fields map[string]any) map[string]any {
		return event(map[string]any{"kind": "deploy", "source_ref": "ci:refused", "finished_at": now.Add(-time.Minute)}, fields)
	}
	failureOf := func(fields map[string]any) map[string]any {
		return event(map[string]any{"kind": "failure", "source_ref": "alerts:refused", "deploy_id": deploy.ID}, fields)
	}
	pulls := func(refs ...string) map[string]any { return map[string]any{"pull_requests": refs} }
	rows := countRows(t, database, `select count(*) from production_events`)
	for _, refused := range []struct {
		name, field string
		body        map[string]any
	}{
		{"a deploy that finished before it started", "finished_at",
			deployOf(map[string]any{"started_at": now.Add(-time.Minute), "finished_at": now.Add(-2 * time.Minute)})},
		{"a deploy that finishes ten minutes past the server's clock", "finished_at", deployOf(map[string]any{"finished_at": now.Add(10 * time.Minute)})},
		{"a failure found ten minutes past the server's clock", "detected_at", failureOf(map[string]any{"detected_at": now.Add(10 * time.Minute)})},
		{"a deploy naming a repository and no number", "pull_requests", deployOf(pulls("acme/app"))},
		{"a deploy naming a number and no owner", "pull_requests", deployOf(pulls("app#7"))},
		{"a deploy naming pull request 0", "pull_requests", deployOf(pulls("acme/app#0"))},
		{"a deploy naming a number no pull request's row holds", "pull_requests", deployOf(pulls("acme/app#2147483648"))},
		{"a deploy naming a repository with a space in it", "pull_requests", deployOf(pulls("acme/my app#7"))},
		{"a failure naming a pull request beside one that is no number", "pull_requests", failureOf(pulls("acme/app#7", "acme/app#seven"))},
	} {
		expectRefusal(t, refused.name, bearerRequest(t, handler, http.MethodPost, productionEventsPath("PRD"), refused.body),
			http.StatusBadRequest, prodevents.CodeInput, refused.field)
	}
	if after := countRows(t, database, `select count(*) from production_events`); after != rows {
		t.Fatalf("the refused posts left %d rows, want the %d there were", after, rows)
	}
	for name, body := range map[string]map[string]any{
		"a deploy that finishes at its start a minute past the server's clock": deployOf(map[string]any{
			"source_ref": "ci:taken", "started_at": now.Add(time.Minute), "finished_at": now.Add(time.Minute), "pull_requests": []string{" acme/app#2147483647 "},
		}),
		"a failure found a minute past the server's clock": failureOf(map[string]any{
			"source_ref": "alerts:taken", "detected_at": now.Add(time.Minute), "pull_requests": []string{"acme/app#2147483647"},
		}),
	} {
		posted := bearerRequest(t, handler, http.MethodPost, productionEventsPath("PRD"), body)
		if posted.Code != http.StatusCreated {
			t.Fatalf("%s: status=%d body=%s, want 201", name, posted.Code, posted.Body.String())
		}
		if row := decodeBody[prodevents.Event](t, posted); !slices.Equal(row.PullRequests, []string{"acme/app#2147483647"}) {
			t.Errorf("%s: the row names pull requests %q, want acme/app#2147483647 alone", name, row.PullRequests)
		}
	}
}

// TestAFailurePostedThroughOneRouteOnASharedDeployCountsForThatProjectAlone: a GitHub deploy stored
// under ALPHA's row ships ALPHA-1's and OMEGA-1's pull requests, so it counts for ALPHA and for
// OMEGA, which both count posted failures and failing checks. A system's failure posted to OMEGA's
// route that blames it by its GitHub run is taken though the row is ALPHA's. OMEGA reads 1 failed deploy
// of 1 with 1 failure not restored, and ALPHA still 0 of 1 with none. A failing check on it, an
// automatic signal written under ALPHA's row, counts for both: ALPHA then reads 1 of 1 with 1 not
// restored, and OMEGA 1 of 1 with 2.
func TestAFailurePostedThroughOneRouteOnASharedDeployCountsForThatProjectAlone(t *testing.T) {
	handler, database, deps := newTestServer(t, testServerOptions{})
	ctx, pool := t.Context(), database.Pool
	now := time.Now().UTC()
	today := now.Truncate(24 * time.Hour)
	since := today.AddDate(0, 0, -3)
	merged := now.Add(-3 * time.Hour)
	const repo = "acme/shared"
	if _, err := pool.Exec(ctx, `insert into projects (key, name, default_assignee, next_number)
		values ('ALPHA', 'Alpha', 'alice', 2), ('OMEGA', 'Omega', 'alice', 2)`); err != nil {
		t.Fatalf("seed the projects: %v", err)
	}
	if _, err := pool.Exec(ctx, `insert into issues (key, project_key, number, title, status, rank, created_by, created_at)
		values ('ALPHA-1', 'ALPHA', 1, 'Alpha work', 'in_progress', '00000001', '{"kind":"system","id":"shared-deploy-test"}', $1),
		       ('OMEGA-1', 'OMEGA', 1, 'Omega work', 'in_progress', '00000001', '{"kind":"system","id":"shared-deploy-test"}', $1)`,
		merged.AddDate(0, 0, -1)); err != nil {
		t.Fatalf("seed the issues: %v", err)
	}
	for number, issue := range map[int]string{1: "ALPHA-1", 2: "OMEGA-1"} {
		if err := delivery.UpsertPullRequest(ctx, pool, delivery.DeliveryPullRequest{
			Repo: repo, Number: number, Title: fmt.Sprintf("%s: change %d", issue, number),
			URL: fmt.Sprintf("https://github.com/%s/pull/%d", repo, number), Author: "octocat",
			CreatedAt: new(merged.Add(-time.Hour)), MergedAt: new(merged), IssueKey: new(issue), Sessions: []string{},
		}); err != nil {
			t.Fatalf("write %s#%d: %v", repo, number, err)
		}
	}
	for _, project := range []string{"ALPHA", "OMEGA"} {
		saveProjectDelivery(t, pool, delivery.ProjectSettingsInput{
			Project: project, Delivery: true, Sources: []delivery.SourceInput{{Kind: delivery.SourceKindEvents}},
			PopulationAuthors: []string{"octocat"}, ExcludedRepos: []string{},
			FailureSources: []string{delivery.FailureSourceEvents, delivery.FailureSourceCheckResults}, FailuresMeasuredSince: &since,
		})
	}
	finished := now.Add(-time.Hour)
	shared := writeProductionEvent(t, pool, prodevents.Event{
		ProjectKey: "ALPHA", Kind: prodevents.KindDeploy, Status: prodevents.StatusSucceeded, Source: prodevents.SourceGitHub,
		SourceRef: prodevents.GitHubSourceRef(31), StartedAt: new(finished.Add(-10 * time.Minute)), FinishedAt: &finished,
		Repo: repo, PullRequests: []string{repo + "#1", repo + "#2"},
	})

	// requireFigures builds the day rows and reads each project's change fail rate (failed and
	// total deploys) and failures not restored over the days around now, as the eval route answers
	// them.
	registry := evals.New(database, deps.Docs, deps.Events, deps.Attributor)
	window := url.Values{"from": {today.AddDate(0, 0, -2).Format(time.RFC3339)}, "to": {today.AddDate(0, 0, 1).Format(time.RFC3339)}}
	requireFigures := func(when string, want map[string][3]float64) {
		t.Helper()
		refreshEvalDays(t, registry, database, time.Now().UTC())
		for project, figures := range want {
			got := map[string]*float64{}
			for _, item := range evalGet[evalListResponse](t, handler, "/api/v1/projects/"+project+"/evals?limit=200&"+window.Encode()).Items {
				switch {
				case len(item.Series) == 0:
				case item.Definition.Slug == evals.SlugChangeFailRate:
					got["failed"], got["total"] = item.Series[0].Window["failed"], item.Series[0].Window["total"]
				case item.Definition.Slug == evals.SlugFailedDeployRecovery:
					got["not_restored"] = item.Series[0].Window["not_restored"]
				}
			}
			for i, name := range []string{"failed", "total", "not_restored"} {
				if value := got[name]; value == nil || *value != figures[i] {
					read := "null"
					if value != nil {
						read = fmt.Sprintf("%g", *value)
					}
					t.Errorf("%s: %s's %s reads %s, want %g", when, project, name, read, figures[i])
				}
			}
		}
	}

	requireFigures("before any failure", map[string][3]float64{"ALPHA": {0, 1, 0}, "OMEGA": {0, 1, 0}})
	posted := bearerRequest(t, handler, http.MethodPost, productionEventsPath("OMEGA"), map[string]any{
		"kind": "failure", "source_ref": "alerts:1", "cicd.pipeline.run.id": "31", "repo": repo,
		"actor": map[string]any{"kind": "session", "id": "ses-ci"},
	})
	if posted.Code != http.StatusCreated {
		t.Fatalf("OMEGA's failure on the shared deploy: status=%d body=%s, want 201", posted.Code, posted.Body.String())
	}
	if failure := decodeBody[prodevents.Event](t, posted); failure.ProjectKey != "OMEGA" || failure.DeployID == nil || *failure.DeployID != shared.ID {
		t.Fatalf("OMEGA's failure is a row of %s blaming %v, want OMEGA's blaming the shared deploy %s", failure.ProjectKey, failure.DeployID, shared.ID)
	}
	requireFigures("after OMEGA's posted failure", map[string][3]float64{"ALPHA": {0, 1, 0}, "OMEGA": {1, 1, 1}})
	writeProductionEvent(t, pool, prodevents.Event{
		ProjectKey: "ALPHA", Kind: prodevents.KindFailure, Source: prodevents.SourceCheckResults, SourceRef: "check:doc/c1@" + shared.ID,
		DeployID: new(shared.ID), Signal: new(prodevents.SignalCheckFail), DetectedAt: new(finished.Add(30 * time.Minute)),
	})
	requireFigures("after a failing check on it", map[string][3]float64{"ALPHA": {1, 1, 1}, "OMEGA": {1, 1, 2}})
}

// controllerDaemon is a Legion daemon stand-in serving GET /legion/v1/controller, as
// newFakeLegionDaemon serves the state route, and 404 to every other request: the route answers as
// the test last set it (answers), and 404 too where it is set to nothing, as a daemon of a build
// before the route answers it. It counts the controller reads it was sent (reads).
type controllerDaemon struct {
	*httptest.Server
	mu    sync.Mutex
	serve http.HandlerFunc
	asked int
}

// newControllerDaemon starts a daemon stand-in whose route names no registered controller.
func newControllerDaemon(t *testing.T) *controllerDaemon {
	t.Helper()
	daemon := &controllerDaemon{}
	daemon.names("")
	daemon.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		daemon.mu.Lock()
		serve := daemon.serve
		if r.URL.Path == legion.ControllerPath {
			daemon.asked++
		}
		daemon.mu.Unlock()
		if r.Method != http.MethodGet || r.URL.Path != legion.ControllerPath || serve == nil {
			http.NotFound(w, r)
			return
		}
		serve(w, r)
	}))
	t.Cleanup(daemon.Close)
	return daemon
}

// answers sets how the controller route answers from the next read on; nil answers it 404.
func (d *controllerDaemon) answers(serve http.HandlerFunc) {
	d.mu.Lock()
	d.serve = serve
	d.mu.Unlock()
}

// names has the route name session as the project's registered controller, in the members of the
// daemon's own answer (packages/daemon/internal/api's ControllerResponse), or `{}`, no registered
// controller, for "".
func (d *controllerDaemon) names(session string) {
	body := `{}`
	if session != "" {
		body = fmt.Sprintf(`{"controllerLocator":{"runtime":"kubernetes","external":true,"sessionId":%q,"registeredAt":"2026-11-27T09:00:00Z","pluginContract":276}}`, session)
	}
	d.answers(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	})
}

// stalls has the route hold each read until its caller hangs up, which is past
// legion.ControllerTimeout.
func (d *controllerDaemon) stalls() {
	d.answers(func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
}

// reads is how many controller reads the daemon was sent.
func (d *controllerDaemon) reads() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.asked
}

// TestTheStateRouteTakesAPersonAndTheProvenControllerAlone: a person rejects, re-counts and
// restores a failure, and asks the project's Legion daemon nothing; the project's controller, the
// session the daemon's GET /legion/v1/controller names at the write and a broker token proves,
// rejects and re-counts it and may not restore it. A controller's write is refused 403 where the
// project names no Legion daemon and where its daemon names no registered controller, 503
// LEGION_CONTROLLER_UNREAD where the daemon answers the route 404, a build before it, and where it
// does not answer within legion.ControllerTimeout; once the daemon names a successor, the same
// token is refused 403. A session that is neither, the shared token, which proves none, and a
// read-only token are refused 403, and every refused write leaves the row as it was, read back
// after each; a row of another project is not the path's project's to write. Each reject and each
// re-count answers 200 and keeps its reason, its writer and its time, the server's (R8), and the
// change fail rate, read after each, leaves the row out after a reject and puts it back in after a
// re-count. A restore keeps its time and its writer through every write after it (R7).
func TestTheStateRouteTakesAPersonAndTheProvenControllerAlone(t *testing.T) {
	rig := newBrokerTokenRig(t)
	caller := newBrokerCaller(t)
	const controller, neither, reader = "the-controllers-token", "a-token-of-another-session", "a-token-that-only-reads"
	rig.broker.Says(controller, liveNamedToken(caller.jkt))
	another := liveNamedToken(caller.jkt)
	another.Session = "ses_neither"
	rig.broker.Says(neither, another)
	rig.broker.Says(reader, readOnlyToken(caller.jkt))
	for _, key := range []string{brokerTestProject, "OTHER"} {
		if created := rig.asPerson(t, http.MethodPost, "/api/v1/projects", map[string]any{"key": key, "name": key}); created.Code != http.StatusCreated {
			t.Fatalf("create project %s: status=%d body=%s", key, created.Code, created.Body.String())
		}
	}
	now := time.Now().UTC()
	today := now.Truncate(24 * time.Hour)
	since := today.AddDate(0, 0, -3)
	saveProjectDelivery(t, rig.database.Pool, delivery.ProjectSettingsInput{
		Project: brokerTestProject, Delivery: true, Sources: []delivery.SourceInput{{Kind: delivery.SourceKindEvents}},
		PopulationAuthors: []string{"octocat"}, ExcludedRepos: []string{},
		FailureSources: []string{delivery.FailureSourceEvents}, FailuresMeasuredSince: &since,
	})
	finished := now.Add(-time.Hour)
	deploy := writeProductionEvent(t, rig.database.Pool, prodevents.Event{
		ProjectKey: brokerTestProject, Kind: prodevents.KindDeploy, Source: prodevents.SourceAPI, SourceRef: "ci:deploy-1", FinishedAt: &finished,
	})
	failure := writeProductionFailure(t, rig.database, brokerTestProject, "alerts:1", deploy.ID, now.Add(-time.Minute))
	path := productionEventStatePath(brokerTestProject, failure.ID)
	daemon := newControllerDaemon(t)
	stateOf := func() prodevents.Event {
		t.Helper()
		return readProductionEvent(t, rig.database, brokerTestProject, failure.ID)
	}
	asPerson := func(body map[string]any) *httptest.ResponseRecorder {
		return rig.asPerson(t, http.MethodPut, path, body)
	}
	asController := func(body map[string]any) *httptest.ResponseRecorder {
		return rig.serve(caller.presenting(t, http.MethodPut, path, controller, body))
	}
	person := model.Actor{Kind: "user", ID: brokerTestPerson}
	session := model.Actor{Kind: "session", ID: brokerTestSession}

	// requireChangeFailRate builds the day rows the writes so far logged changes for, reads the
	// project's change fail rate over the days around now as the eval route answers it, and fails
	// unless the one deploy is counted failed (failed 1, rate 1) or not (failed 0, rate 0).
	registry := evals.New(rig.database, rig.deps.Docs, rig.deps.Events, rig.deps.Attributor)
	window := url.Values{"from": {today.AddDate(0, 0, -2).Format(time.RFC3339)}, "to": {today.AddDate(0, 0, 1).Format(time.RFC3339)}}
	figure := func(value *float64) string {
		if value == nil {
			return "null"
		}
		return fmt.Sprintf("%g", *value)
	}
	requireChangeFailRate := func(when string, failed float64) {
		t.Helper()
		refreshEvalDays(t, registry, rig.database, time.Now().UTC())
		list := evalGet[evalListResponse](t, rig.handler, "/api/v1/projects/"+brokerTestProject+"/evals?limit=200&"+window.Encode())
		for _, item := range list.Items {
			if item.Definition.Slug != evals.SlugChangeFailRate || len(item.Series) == 0 {
				continue
			}
			got := item.Series[0].Window
			if failedGot, total, rate := got["failed"], got["total"], got["rate"]; failedGot == nil || total == nil || rate == nil ||
				*failedGot != failed || *total != 1 || *rate != failed {
				t.Fatalf("%s: the change fail rate reads %s failed of %s deploys, rate %s; want %g of 1, rate %g",
					when, figure(failedGot), figure(total), figure(rate), failed, failed)
			}
			return
		}
		t.Fatalf("%s: the evals of %s hold no change fail rate series", when, brokerTestProject)
	}
	// setState sends a reject or a re-count, which must answer 200, and holds the row it answered
	// and the row the store keeps to the state, the reason sent, the writer and a time inside the
	// request's own span: the server's.
	setState := func(what string, send func(map[string]any) *httptest.ResponseRecorder, state, reason string, by model.Actor) {
		t.Helper()
		before := time.Now().UTC().Add(-time.Millisecond)
		response := send(map[string]any{"state": state, "reason": reason})
		after := time.Now().UTC().Add(time.Millisecond)
		if response.Code != http.StatusOK {
			t.Fatalf("%s: status=%d body=%s, want 200", what, response.Code, response.Body.String())
		}
		for name, row := range map[string]prodevents.Event{"answered": decodeBody[prodevents.Event](t, response), "stored": stateOf()} {
			if row.State != state || row.StateReason == nil || *row.StateReason != reason || row.StateBy == nil ||
				row.StateBy.Kind != by.Kind || row.StateBy.ID != by.ID || row.StateAt == nil || row.StateAt.Before(before) || row.StateAt.After(after) {
				held := "none"
				if row.StateReason != nil {
					held = *row.StateReason
				}
				t.Fatalf("%s: the %s row is state %s, reason %q, writer %v at %v; want %s, %q, %s:%s, at a time between %s and %s",
					what, name, row.State, held, row.StateBy, row.StateAt, state, reason, by.Kind, by.ID,
					before.Format(time.RFC3339Nano), after.Format(time.RFC3339Nano))
			}
		}
	}
	// refused sends a write that must be refused with status and code, its message naming each of
	// names, and holds the row the store keeps to the row as it stood before the write.
	refused := func(what string, send func() *httptest.ResponseRecorder, status int, code string, names ...string) {
		t.Helper()
		before, err := json.Marshal(stateOf())
		if err != nil {
			t.Fatalf("encode the row: %v", err)
		}
		expectRefusal(t, what, send(), status, code, names...)
		if after, err := json.Marshal(stateOf()); err != nil || string(after) != string(before) {
			t.Fatalf("%s left the row %s (%v), want it as it was, %s", what, after, err, before)
		}
	}
	controllerRejects := func() *httptest.ResponseRecorder {
		return asController(map[string]any{"state": "rejected", "reason": "A duplicate of the first alert."})
	}

	requireChangeFailRate("before any state write", 1)
	// Before the project names a Legion daemon, no session is its controller.
	refused("the controller's reject while the project names no daemon", controllerRejects,
		http.StatusForbidden, codeProductionEventStateForbidden, brokerTestSession, "names no Legion daemon")
	if _, err := legion.PutSettings(context.Background(), rig.database.Pool, brokerTestProject, daemon.URL,
		model.Actor{Kind: "user", ID: brokerTestPerson}); err != nil {
		t.Fatalf("name %s's Legion daemon: %v", brokerTestProject, err)
	}

	// A person's writes ask the daemon nothing, and are taken while it answers the route 404 as a
	// daemon of an earlier build does.
	daemon.answers(nil)
	setState("a person's reject", asPerson, prodevents.StateRejected, "A flaky probe.", person)
	requireChangeFailRate("after the person's reject", 0)

	before := time.Now().UTC().Add(-time.Millisecond)
	restored := asPerson(map[string]any{"state": "restored"})
	after := time.Now().UTC().Add(time.Millisecond)
	if restored.Code != http.StatusOK {
		t.Fatalf("a person's restore: status=%d body=%s, want 200", restored.Code, restored.Body.String())
	}
	for name, row := range map[string]prodevents.Event{"answered": decodeBody[prodevents.Event](t, restored), "stored": stateOf()} {
		if row.RestoredAt == nil || row.RestoredAt.Before(before) || row.RestoredAt.After(after) || row.RestoredBy == nil ||
			row.RestoredBy.Kind != person.Kind || row.RestoredBy.ID != person.ID || row.State != prodevents.StateRejected {
			t.Fatalf("a person's restore: the %s row is restored at %v by %v in state %s; want a time between %s and %s, by %s:%s, the reject untouched",
				name, row.RestoredAt, row.RestoredBy, row.State, before.Format(time.RFC3339Nano), after.Format(time.RFC3339Nano), person.Kind, person.ID)
		}
	}
	restoredAt, restoredBy := *stateOf().RestoredAt, *stateOf().RestoredBy
	setState("a person's re-count", asPerson, prodevents.StateCounted, "The probe was right.", person)
	requireChangeFailRate("after the person's re-count", 1)
	if asked := daemon.reads(); asked != 0 {
		t.Fatalf("a person's writes asked the daemon for its controller %d times, want none", asked)
	}

	// The daemon's answer decides the controller's write: a daemon of a build before the route,
	// one that does not answer within the bound and one that names no registered controller are
	// each refused, and say which.
	refused("the controller's reject while the daemon answers the route 404", controllerRejects,
		http.StatusServiceUnavailable, codeLegionControllerUnread, "404", legion.ControllerPath)
	daemon.stalls()
	refused("the controller's reject while the daemon does not answer", controllerRejects,
		http.StatusServiceUnavailable, codeLegionControllerUnread, "no answer within")
	daemon.names("")
	refused("the controller's reject while the daemon names no controller", controllerRejects,
		http.StatusForbidden, codeProductionEventStateForbidden, brokerTestSession, "names no registered controller")
	if asked := daemon.reads(); asked != 3 {
		t.Fatalf("the controller's three writes asked the daemon %d times, want once each", asked)
	}

	daemon.names(brokerTestSession)
	setState("the controller's reject", asController, prodevents.StateRejected, "A duplicate of the first alert.", session)
	requireChangeFailRate("after the controller's reject", 0)
	setState("the controller's re-count", asController, prodevents.StateCounted, "It was real.", session)
	requireChangeFailRate("after the controller's re-count", 1)
	refused("the controller's restore", func() *httptest.ResponseRecorder { return asController(map[string]any{"state": "restored"}) },
		http.StatusForbidden, codeProductionEventStateForbidden)
	refused("a session that is neither", func() *httptest.ResponseRecorder {
		return rig.serve(caller.presenting(t, http.MethodPut, path, neither, map[string]any{
			"state": "rejected", "reason": "Says another session.",
		}))
	}, http.StatusForbidden, codeProductionEventStateForbidden, "ses_neither")
	refused("the shared token", func() *httptest.ResponseRecorder {
		return rig.serve(brokerRequest(t, http.MethodPut, path, map[string]any{
			"state": "rejected", "reason": "Says the shared token.",
		}, "Bearer agent-token", ""))
	}, http.StatusForbidden, codeProductionEventStateForbidden)
	// A reader is neither a person nor the controller: a read-only token reads the record and is
	// refused both writes of a reason.
	for _, state := range []string{prodevents.StateRejected, prodevents.StateCounted} {
		refused("a reader's "+state, func() *httptest.ResponseRecorder {
			return rig.serve(caller.presenting(t, http.MethodPut, path, reader, map[string]any{
				"state": state, "reason": "Says a reader.",
			}))
		}, http.StatusForbidden, contracts.DispatchReadOnlyToken)
	}

	daemon.names("ses_successor")
	refused("the controller once the daemon names a successor", func() *httptest.ResponseRecorder {
		return asController(map[string]any{"state": "rejected", "reason": "Too late."})
	}, http.StatusForbidden, codeProductionEventStateForbidden, brokerTestSession)
	if row := stateOf(); row.State != prodevents.StateCounted || row.StateBy == nil || row.StateBy.ID != brokerTestSession {
		t.Fatalf("the refused writes moved the failure to %s by %v, want the controller's re-count to stand", row.State, row.StateBy)
	}
	if row := stateOf(); row.RestoredAt == nil || !row.RestoredAt.Equal(restoredAt) || row.RestoredBy == nil ||
		row.RestoredBy.Kind != restoredBy.Kind || row.RestoredBy.ID != restoredBy.ID {
		t.Fatalf("after every later write the failure is restored at %v by %v, want the restore's own %v by %v", row.RestoredAt, row.RestoredBy, restoredAt, restoredBy)
	}
	requireChangeFailRate("after the refused writes", 1)

	otherDeploy := writeProductionDeploy(t, rig.database, "OTHER", 2, now.Add(-time.Hour))
	otherFailure := writeProductionFailure(t, rig.database, "OTHER", "alerts:2", otherDeploy.ID, now.Add(-time.Minute))
	expectRefusal(t, "a row of another project", rig.asPerson(t, http.MethodPut, productionEventStatePath(brokerTestProject, otherFailure.ID),
		map[string]any{"state": "rejected", "reason": "Not this project's."}), http.StatusNotFound, codeProductionEventNotFound)
	if row := readProductionEvent(t, rig.database, "OTHER", otherFailure.ID); row.State != prodevents.StateCounted {
		t.Fatalf("the refused write moved OTHER's failure to %s, want counted", row.State)
	}
}

// TestAReadOnlyTokensReadOfTheRecordWritesOnlyItsProof: the list read is a read (its declaration's
// Read, readWritesNothing), so it is held to what every read is held to: for a read-only token it
// writes nothing but the row that spends its DPoP proof. The positive controls are that insert and
// the handler's own read of production_events.
func TestAReadOnlyTokensReadOfTheRecordWritesOnlyItsProof(t *testing.T) {
	database := storetest.Open(t)
	traced := &poolStatements{}
	rig := newBrokerTokenRigOn(t, testServerOptions{store: storetest.TracedStore(t, database, traced)})
	caller := newBrokerCaller(t)
	const token = "a-read-only-token-reading-the-record"
	rig.broker.Says(token, readOnlyToken(caller.jkt))
	if created := rig.asPerson(t, http.MethodPost, "/api/v1/projects", map[string]any{"key": "RDR", "name": "Reader"}); created.Code != http.StatusCreated {
		t.Fatalf("create project RDR: status=%d body=%s", created.Code, created.Body.String())
	}
	writeProductionDeploy(t, database, "RDR", 1, time.Now().UTC().Add(-time.Hour))
	from := traced.count()

	request := caller.presenting(t, http.MethodGet, productionEventsPath("RDR"), token, nil)
	request = request.WithContext(context.WithValue(request.Context(), countedRead{}, true))
	response := rig.serve(request)
	if page := decodeBody[productionEventList](t, response); response.Code != http.StatusOK || page.Total != 1 {
		t.Fatalf("the read-only token's read: status=%d body=%s, want 200 and the one deploy", response.Code, response.Body.String())
	}
	statements := traced.since(from)
	written := requireReadsWrites(t, "GET "+productionEventsPath("RDR"), statements)
	if !holdsStatement(statements, proofInsert) {
		t.Errorf("the tracer saw no insert into dpop_proofs among %d statements: it did not see the request's proof spent", len(statements))
	}
	handlerRead := regexp.MustCompile(`(?i)\bfrom\s+production_events\b`)
	if !holdsStatement(statements, handlerRead) {
		t.Errorf("the tracer saw no statement matching %s among %d statements: it did not see the handler's reads", handlerRead, len(statements))
	}
	t.Logf("the request ran %d statements, whose writes were %v", len(statements), written)
}

// TestTheSettingsAndProductionEventRoutesAnswerNoFailureMeasure: the five routes under a project's
// delivery settings and its production-event record answer settings and facts, never a figure of
// the three failure measures. A new route under either path must get its own row here. The
// settings' eval targets are goals, not observed values.
func TestTheSettingsAndProductionEventRoutesAnswerNoFailureMeasure(t *testing.T) {
	t.Parallel()
	const settings = "/api/v1/projects/{key}/settings/delivery"
	const events = "/api/v1/projects/{key}/production-events"
	handler, database, deps := newTestServer(t, testServerOptions{})
	fixture := seedFailureHistory(t, database)
	registry := evals.New(database, deps.Docs, deps.Events, deps.Attributor)
	if _, err := registry.Ensure(t.Context(), "FAIL"); err != nil {
		t.Fatalf("seed FAIL's evals and targets: %v", err)
	}
	refreshEvalDays(t, registry, database, failuresNow)

	routes := []struct {
		method, pattern, suffix string
		body                    any
		status                  int
	}{
		{http.MethodGet, settings, "", nil, http.StatusOK},
		{http.MethodPut, settings, "", map[string]any{}, http.StatusOK},
		{http.MethodGet, events, "?" + failureWindowQuery(0, 2), nil, http.StatusOK},
		{http.MethodPost, events, "", map[string]any{"kind": "failure", "deploy_id": fixture["P2"].ID}, http.StatusCreated},
		{http.MethodPut, events + "/{id}/state", "", map[string]any{"state": "rejected", "reason": "Not a production failure."}, http.StatusOK},
	}
	want := make(map[string]bool, len(routes))
	for _, route := range routes {
		want[route.method+" "+route.pattern] = true
	}
	for _, route := range (&server{}).routes() {
		path := route.Pattern
		if path != settings && !strings.HasPrefix(path, settings+"/") && path != events && !strings.HasPrefix(path, events+"/") {
			continue
		}
		key := route.Method + " " + path
		if !want[key] {
			t.Errorf("the route table declares %s but no test row exercises it", key)
		}
		delete(want, key)
	}
	if len(want) != 0 {
		t.Errorf("the route table lacks the inventory routes %v", want)
	}

	// The fixture actually has a figure that a route could accidentally include.
	rate := getDeliveryMeasures(t, handler, failureWindowQuery(0, 2)).Measures.ChangeFailureRate.Rate
	if rate == nil {
		t.Fatal("the failure fixture must have a measured change fail rate")
	}
	for _, route := range routes {
		t.Run(route.method+" "+route.pattern, func(t *testing.T) {
			path := strings.NewReplacer("{key}", "FAIL", "{id}", fixture["FB"].ID).Replace(route.pattern)
			response := dispatchRequest(t, handler, route.method, path+route.suffix, route.body, "alice")
			if response.Code != route.status {
				t.Fatalf("status=%d body=%s, want %d", response.Code, response.Body.String(), route.status)
			}
			answer := decodeBody[map[string]any](t, response)
			if answer == nil {
				t.Fatal("the route answered null, want a record or list")
			}
			if field := failureFigureAt(answer, ""); field != "" {
				t.Errorf("answer carries a failure measure at %s", field)
			}
			if route.method != http.MethodGet || route.pattern != settings {
				return
			}
			targets, _ := answer["targets"].(map[string]any)
			for _, slug := range []string{evals.SlugChangeFailRate, evals.SlugFailedDeployRecovery} {
				if !isDeliveryGoal(targets[slug]) {
					t.Errorf("settings target for %s = %v, want a goal (a number or off)", slug, targets[slug])
				}
			}
			// Control: the settings answer with the measured rate beside its targets is a leak.
			withFigure := decodeBody[map[string]any](t, response)
			withFigure[evals.SlugChangeFailRate] = map[string]any{"value": *rate}
			if field := failureFigureAt(withFigure, ""); field != "."+evals.SlugChangeFailRate {
				t.Errorf("control found %q, want the injected change fail rate figure", field)
			}
		})
	}
}

func isDeliveryGoal(value any) bool {
	_, number := value.(float64)
	return number || value == deliveryTargetOff
}

// failureFigureAt walks the actual JSON, including records inside lists, so new response fields
// cannot disappear in a typed decode. It distinguishes counts from event facts (restored_at,
// state, and so on) and pagination's total. A top-level targets map may contain scalar goals,
// not a measured value or series.
func failureFigureAt(value any, path string) string {
	switch node := value.(type) {
	case []any:
		for i, item := range node {
			if field := failureFigureAt(item, fmt.Sprintf("%s[%d]", path, i)); field != "" {
				return field
			}
		}
	case map[string]any:
		for name, member := range node {
			field := path + "." + name
			slug := strings.ReplaceAll(name, "_", "-")
			switch slug {
			case evals.SlugChangeFailRate, evals.SlugFailedDeployRecovery, evals.SlugDeploymentReworkRate:
				if path != ".targets" || !isDeliveryGoal(member) {
					return field
				}
			case "change-failure-rate", "time-to-restore", "rate", "median", "median-minutes",
				"failure-count", "restore-count", "rework-count", "failed", "failures", "restored", "restores",
				"restoring", "not-restored", "rework", "per-deploy", "series", "value":
				return field
			}
			if nested := failureFigureAt(member, field); nested != "" {
				return nested
			}
		}
	}
	return ""
}
