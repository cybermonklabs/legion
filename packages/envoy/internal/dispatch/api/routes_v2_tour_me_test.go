package api

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/cipher/wire"
	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

func init() { addTourSteps("me", meTourSteps) }

// meTourSteps are the route tour's steps of the caller's own routes and the credential routes:
// each route, GET /api/v2/me for a person and for a bearer, every kind of mark read and
// written, a thread's dismissal and restore, the bulk write in each of its forms, and a credential
// request's approve, its deny, and the decision the broker already holds.
func meTourSteps() []tourStep {
	alice := tourPerson("alice")
	tokenMissing := []tourMissing{{Param: "id", Value: uuid.NewString(), Code: apicode.AgentTokenNotFound}}
	onToken := func(body any) func(*testing.T, *routeTour) tourRequest {
		return func(t *testing.T, tour *routeTour) tourRequest {
			return tourRequest{Path: map[string]string{"id": tour.agentToken(t)}, Body: body, As: alice}
		}
	}
	// The target of each kind of mark, made afresh for each send, and the 404 of one that is
	// missing: every kind but agent, whose session keeps today's answer (ruling A).
	targets := []struct {
		kind    string
		target  func(*testing.T, *routeTour) string
		missing []tourMissing
	}{
		{"issue", func(t *testing.T, tour *routeTour) string { return tour.issue(t).Key }, []tourMissing{
			{Param: "id", Value: "NOPE-1", Code: apicode.IssueNotFound},
		}},
		{"agent", func(*testing.T, *routeTour) string { return tourRecipient }, nil},
		{"document", func(t *testing.T, tour *routeTour) string { return tour.document(t) }, []tourMissing{
			{Param: "id", Value: uuid.NewString(), Code: apicode.ArtifactNotFound},
		}},
		{"thread", func(t *testing.T, tour *routeTour) string { return tour.comment(t, tour.issue(t)) }, []tourMissing{
			{Param: "id", Value: uuid.NewString(), Code: apicode.ThreadNotFound},
		}},
		{"message", func(t *testing.T, tour *routeTour) string { return tour.directMessage(t) }, []tourMissing{
			{Param: "id", Value: uuid.NewString(), Code: apicode.MessageNotFound},
		}},
		{"ask", func(t *testing.T, tour *routeTour) string { return tour.ask(t, tour.issue(t)) }, []tourMissing{
			{Param: "id", Value: uuid.NewString(), Code: apicode.AskNotFound},
		}},
	}
	// The body each kind of mark is written with.
	writes := map[string]func(t *testing.T, tour *routeTour, target string) any{
		"issue": func(*testing.T, *routeTour, string) any {
			return map[string]any{"pinned": true, "last_read_seq": 1, "pinned_items": []string{"pinned_items:event:1"}}
		},
		"agent":    func(*testing.T, *routeTour, string) any { return map[string]any{"pinned": true} },
		"document": func(*testing.T, *routeTour, string) any { return map[string]any{"read_version": 1} },
		"thread": func(*testing.T, *routeTour, string) any {
			return map[string]any{"read_through": time.Now().UTC().Format(time.RFC3339Nano)}
		},
		"message": func(*testing.T, *routeTour, string) any { return map[string]any{"dismissed": false} },
		"ask": func(*testing.T, *routeTour, string) any {
			return map[string]any{"snoozed_until": time.Now().Add(time.Hour).UTC().Format(time.RFC3339)}
		},
	}
	onMark := func(kind string, target func(*testing.T, *routeTour) string, body func(*testing.T, *routeTour, string) any) func(*testing.T, *routeTour) tourRequest {
		return func(t *testing.T, tour *routeTour) tourRequest {
			id := target(t, tour)
			request := tourRequest{Path: map[string]string{"kind": kind, "id": id}, As: alice}
			if body != nil {
				request.Body = body(t, tour, id)
			}
			return request
		}
	}
	thread := func(t *testing.T, tour *routeTour) string { return tour.comment(t, tour.issue(t)) }
	ask := func(t *testing.T, tour *routeTour) string { return tour.ask(t, tour.issue(t)) }
	now := func() string { return time.Now().UTC().Format(time.RFC3339Nano) }
	steps := []tourStep{
		{
			Method: http.MethodGet, Pattern: "/me", Status: http.StatusOK,
			Setup: func(*testing.T, *routeTour) tourRequest { return tourRequest{As: alice} },
		},
		{
			Method: http.MethodGet, Pattern: "/me", Name: "a person's token", Status: http.StatusOK,
			Setup: func(_ *testing.T, tour *routeTour) tourRequest { return tourRequest{As: tourBearer(tour.personToken)} },
		},
		{
			Method: http.MethodGet, Pattern: "/me/agent-tokens", Status: http.StatusOK,
			Setup: func(t *testing.T, tour *routeTour) tourRequest {
				tour.agentToken(t)
				return tourRequest{Query: url.Values{"limit": {"5"}}, As: alice}
			},
		},
		{
			Method: http.MethodPost, Pattern: "/me/agent-tokens", Status: http.StatusCreated,
			Setup: func(*testing.T, *routeTour) tourRequest {
				return tourRequest{Body: map[string]any{"name": "a token the tour mints", "scopes": []string{model.ScopePerson}}, As: alice}
			},
		},
		{
			Method: http.MethodDelete, Pattern: "/me/agent-tokens/{id}", Status: http.StatusNoContent,
			Setup: onToken(nil), Missing: tokenMissing,
		},
		{
			Method: http.MethodPatch, Pattern: "/me/agent-tokens/{id}", Status: http.StatusOK,
			Setup: onToken(map[string]any{"scopes": []string{model.ScopePerson}}), Missing: tokenMissing,
		},
		{
			Method: http.MethodGet, Pattern: "/me/answers", Status: http.StatusOK,
			Setup: func(*testing.T, *routeTour) tourRequest { return tourRequest{As: alice} },
		},
		{
			Method: http.MethodGet, Pattern: "/me/marks", Status: http.StatusOK,
			Setup: func(t *testing.T, tour *routeTour) tourRequest {
				tour.pinIssue(t)
				return tourRequest{As: alice}
			},
		},
		{
			Method: http.MethodGet, Pattern: "/me/marks", Name: "one kind on the targets named", Status: http.StatusOK,
			Setup: func(t *testing.T, tour *routeTour) tourRequest {
				return tourRequest{Query: url.Values{"kind": {"issue"}, "id": {tour.pinIssue(t), "NOPE-1"}}, As: alice}
			},
		},
		{
			Method: http.MethodPatch, Pattern: "/me/marks", Name: "named marks of several kinds", Status: http.StatusOK,
			Setup: func(t *testing.T, tour *routeTour) tourRequest {
				comment := thread(t, tour)
				return tourRequest{Body: map[string]any{"items": []map[string]any{
					{"kind": "issue", "id": tour.issue(t).Key, "pinned": true},
					{"kind": "agent", "id": tourRecipient, "read_through": now()},
					{"kind": "thread", "id": comment, "dismissed": true, "version": tour.threadVersion(t, comment)},
					{"kind": "ask", "id": ask(t, tour), "snoozed_until": time.Now().Add(time.Hour).UTC().Format(time.RFC3339)},
				}}, As: alice}
			},
		},
		{
			Method: http.MethodPatch, Pattern: "/me/marks", Name: "the narrowing read", Status: http.StatusOK,
			Setup: func(t *testing.T, tour *routeTour) tourRequest {
				thread(t, tour)
				return tourRequest{Body: map[string]any{"filter": map[string]any{"kind": []string{"reply"}}, "set": map[string]any{"read_through": now()}}, As: alice}
			},
		},
		{
			Method: http.MethodPatch, Pattern: "/me/marks", Name: "the narrowing dismissed", Status: http.StatusOK,
			Setup: func(t *testing.T, tour *routeTour) tourRequest {
				thread(t, tour)
				return tourRequest{Body: map[string]any{"set": map[string]any{"dismissed_through": now()}}, As: alice}
			},
		},
		{
			Method: http.MethodPatch, Pattern: "/me/marks", Name: "the narrowing put back", Status: http.StatusOK,
			Setup: func(*testing.T, *routeTour) tourRequest {
				return tourRequest{Body: map[string]any{"filter": map[string]any{}, "set": map[string]any{"dismissed": false}}, As: alice}
			},
		},
		{
			Method: http.MethodPatch, Pattern: "/me/marks/{kind}/{id}", Name: "thread dismissed under its version", Status: http.StatusOK,
			Setup: onMark("thread", thread, func(t *testing.T, tour *routeTour, id string) any {
				return map[string]any{"dismissed": true, "version": tour.threadVersion(t, id)}
			}),
			Missing: []tourMissing{{Param: "id", Value: uuid.NewString(), Code: apicode.ThreadNotFound}},
		},
		{
			Method: http.MethodPatch, Pattern: "/me/marks/{kind}/{id}", Name: "thread put back", Status: http.StatusOK,
			Setup:   onMark("thread", thread, func(*testing.T, *routeTour, string) any { return map[string]any{"dismissed": false} }),
			Missing: []tourMissing{{Param: "id", Value: uuid.NewString(), Code: apicode.ThreadNotFound}},
		},
		{
			Method: http.MethodPatch, Pattern: "/me/marks/{kind}/{id}", Name: "message dismissed as the page drew it", Status: http.StatusOK,
			Setup: onMark("message", func(t *testing.T, tour *routeTour) string { return tour.directMessage(t) },
				func(*testing.T, *routeTour, string) any {
					return map[string]any{"dismissed": true, "newest_reply": uuid.NewString(), "unread_replies": []string{}}
				}),
			Missing: []tourMissing{{Param: "id", Value: uuid.NewString(), Code: apicode.MessageNotFound}},
		},
		{
			Method: http.MethodPatch, Pattern: "/me/marks/{kind}/{id}", Name: "ask un-snoozed", Status: http.StatusOK,
			Setup:   onMark("ask", ask, func(*testing.T, *routeTour, string) any { return map[string]any{"snoozed_until": nil} }),
			Missing: []tourMissing{{Param: "id", Value: uuid.NewString(), Code: apicode.AskNotFound}},
		},
	}
	for _, target := range targets {
		steps = append(steps,
			tourStep{
				Method: http.MethodGet, Pattern: "/me/marks/{kind}/{id}", Name: target.kind, Status: http.StatusOK,
				Setup: onMark(target.kind, target.target, nil), Missing: target.missing,
			},
			tourStep{
				Method: http.MethodPatch, Pattern: "/me/marks/{kind}/{id}", Name: target.kind, Status: http.StatusOK,
				Setup: onMark(target.kind, target.target, writes[target.kind]), Missing: target.missing,
			},
		)
	}
	return append(steps, credentialTourSteps()...)
}

// credentialTourSteps are the steps of the credential routes, each against the tour's secrets
// broker, which a step's setup tells what to answer: a list's rows, a record a code selects, and
// the broker's own 404 for the id a missing-objects send names.
func credentialTourSteps() []tourStep {
	alice := tourPerson("alice")
	brokerMissing := func(path, value string) func(*routeTour) {
		return func(tour *routeTour) {
			tour.secrets.answer(http.MethodPost, path, http.StatusNotFound, map[string]string{"code": "NOT_FOUND", "error": "no such " + value})
		}
	}
	onID := func(missing func(*routeTour)) func(*testing.T, *routeTour) tourRequest {
		return func(_ *testing.T, tour *routeTour) tourRequest {
			missing(tour)
			return tourRequest{Path: map[string]string{"id": uuid.NewString()}, As: alice}
		}
	}
	// decide tells the broker what a credential request the code selects holds - its id, its state
	// and its approver - and what to answer its approve and its deny, and answers the request that
	// moves it to state.
	decide := func(state, holds string) func(*testing.T, *routeTour) tourRequest {
		return func(_ *testing.T, tour *routeTour) tourRequest {
			record := "tour-record-" + uuid.NewString()
			tour.secrets.answer(http.MethodPost, "/v1/machine-logins/lookup", http.StatusOK, tourLoginRecord(record, holds))
			tour.secrets.answer(http.MethodPost, "/v1/credential-requests/"+record+"/approve", http.StatusOK,
				map[string]any{"credential_id": "tour-credential", "state": "approved"})
			tour.secrets.answer(http.MethodPost, "/v1/credential-requests/"+record+"/deny", http.StatusOK, map[string]any{"state": "denied"})
			brokerMissing("/v1/credential-requests/no-such-request/"+map[string]string{"approved": "approve", "denied": "deny"}[state],
				"credential request")(tour)
			return tourRequest{Path: map[string]string{"id": record}, Body: map[string]any{"state": state, "code": "ABCD-1234"}, As: alice}
		}
	}
	// dismiss tells the broker what to answer the dismissal of an expired request - the
	// dismissal it records, which a second dismissal answers again as it stands - and answers the
	// request that dismisses it.
	dismiss := func(_ *testing.T, tour *routeTour) tourRequest {
		record := "tour-record-" + uuid.NewString()
		tour.secrets.answer(http.MethodPost, "/v1/credential-requests/"+record+"/dismiss", http.StatusOK, map[string]any{
			"record_id": record, "dismissed_by": "alice", "dismissed_at": "2026-11-19T00:00:00Z",
		})
		brokerMissing("/v1/credential-requests/no-such-request/dismiss", "credential request")(tour)
		return tourRequest{Path: map[string]string{"id": record}, Body: map[string]any{"state": "dismissed"}, As: alice}
	}
	requestMissing := []tourMissing{{Param: "id", Value: "no-such-request", Code: apicode.CredentialRequestNotFound}}
	return []tourStep{
		{
			Method: http.MethodGet, Pattern: "/credential-grants", Status: http.StatusOK,
			Setup: func(_ *testing.T, tour *routeTour) tourRequest {
				tour.secrets.answer(http.MethodGet, "/v1/grants", http.StatusOK, map[string]any{"grants": []map[string]any{
					{"grant_id": "tour-grant-1", "granted": "automatic", "names": []string{"TOUR_SECRET"}, "approver": "alice"},
					{"grant_id": "tour-grant-2", "granted": "approval", "names": []string{"TOUR_SECRET"}, "approver": "alice"},
				}})
				return tourRequest{Query: url.Values{"limit": {"1"}, "offset": {"1"}}, As: alice}
			},
		},
		{
			Method: http.MethodDelete, Pattern: "/credential-grants/{id}", Status: http.StatusNoContent,
			Setup:   onID(brokerMissing("/v1/grants/no-such-grant/revoke-by-approver", "grant")),
			Missing: []tourMissing{{Param: "id", Value: "no-such-grant", Code: apicode.CredentialGrantNotFound}},
		},
		{
			Method: http.MethodGet, Pattern: "/credential-requests", Status: http.StatusOK,
			Setup: func(_ *testing.T, tour *routeTour) tourRequest {
				tour.secrets.answer(http.MethodGet, "/v1/pending", http.StatusOK, map[string]any{"pending": []map[string]any{
					{"record_id": "tour-record", "kind": "launcher_credential", "identifiers": []string{"tour-host"}, "session": map[string]any{}},
				}})
				return tourRequest{As: alice}
			},
		},
		{
			Method: http.MethodPut, Pattern: "/credential-requests/{id}/state", Status: http.StatusOK,
			Setup: decide("approved", "pending"), Missing: requestMissing,
		},
		{
			Method: http.MethodPut, Pattern: "/credential-requests/{id}/state", Name: "denied", Status: http.StatusOK,
			Setup: decide("denied", "pending"), Missing: requestMissing,
		},
		{
			Method: http.MethodPut, Pattern: "/credential-requests/{id}/state", Name: "approved already by its approver",
			Status: http.StatusOK, Setup: decide("approved", "approved"),
		},
		{
			Method: http.MethodPut, Pattern: "/credential-requests/{id}/state", Name: "dismissed", Status: http.StatusOK,
			Setup: dismiss, Missing: requestMissing,
		},
		{
			Method: http.MethodPost, Pattern: "/machine-login-lookups", Status: http.StatusOK,
			Setup: func(_ *testing.T, tour *routeTour) tourRequest {
				tour.secrets.answer(http.MethodPost, "/v1/machine-logins/lookup", http.StatusOK,
					tourLoginRecord("tour-record-"+uuid.NewString(), "pending"))
				return tourRequest{Body: map[string]any{"code": "ABCD-1234"}, As: alice}
			},
		},
		{
			Method: http.MethodGet, Pattern: "/machine-logins", Status: http.StatusOK,
			Setup: func(_ *testing.T, tour *routeTour) tourRequest {
				tour.secrets.answer(http.MethodGet, "/v1/launcher-credentials", http.StatusOK,
					wire.LauncherCredentialsResponse{Credentials: tourMachineLogins("alice")})
				return tourRequest{As: alice}
			},
		},
		{
			Method: http.MethodDelete, Pattern: "/machine-logins/{id}", Status: http.StatusNoContent,
			Setup:   onID(brokerMissing("/v1/launcher-credentials/no-such-login/revoke-by-approver", "machine login")),
			Missing: []tourMissing{{Param: "id", Value: "no-such-login", Code: apicode.MachineLoginNotFound}},
		},
		{
			Method: http.MethodGet, Pattern: "/me/signins", Status: http.StatusOK,
			Setup: func(*testing.T, *routeTour) tourRequest { return tourRequest{As: alice} },
		},
		{
			Method: http.MethodDelete, Pattern: "/me/signins/{id}", Status: http.StatusOK,
			Setup:   onID(brokerMissing("/v1/person-signins/no-such-signin/revoke", "sign-in")),
			Missing: []tourMissing{{Param: "id", Value: "no-such-signin", Code: apicode.SigninNotFound}},
		},
	}
}

// tourLoginRecord is a credential request as the broker's machine-login lookup answers it: a
// launcher credential alice approves, for tour-host, holding state, every member the broker's
// wire type writes (wire.RecordResponse), which Dispatch relays as the broker answered it.
func tourLoginRecord(record, state string) wire.RecordResponse {
	return wire.RecordResponse{
		RecordID: record, Kind: "launcher_credential", State: state, Approver: "alice",
		Identifiers: []string{"tour-host"}, Reason: "the tour's machine", RulesVersion: "tour",
		ExpiresAt: time.Now().UTC().Add(time.Hour), RequestedAt: time.Now().UTC(),
	}
}

// tourMachineLogins are the machine logins the broker lists for person: a service's token login,
// whose person is null, and a person's pod login, whose person is person, each holding every
// member the broker's wire type writes (wire.LauncherCredentialResp), so the tour holds a person
// set and a person null to the /api/v2 document.
func tourMachineLogins(person string) []wire.LauncherCredentialResp {
	issued := time.Now().UTC().Truncate(time.Second)
	service, subject := "legion-daemon", "system:serviceaccount:legion-system:legion-daemon"
	podService, podSubject := "person-pods", "system:serviceaccount:people:"+person
	return []wire.LauncherCredentialResp{
		{
			CredentialID: "tour-person-pod-login", Host: "people/" + person, Service: &podService, Subject: &podSubject,
			Person: &person, IssuedAt: issued, ExpiresAt: issued.Add(time.Hour),
		},
		{
			CredentialID: "tour-service-login", Host: "legion-system/legion-daemon", Service: &service, Subject: &subject,
			IssuedAt: issued, ExpiresAt: issued.Add(time.Hour),
		},
	}
}

// agentToken mints a personal agent token of alice's holding no scope, answering its id.
func (tour *routeTour) agentToken(t *testing.T) string {
	t.Helper()
	created := dispatchRequest(t, tour.handler, http.MethodPost, "/api/v1/me/agent-tokens", map[string]any{
		"name": "a token the tour holds", "scopes": []string{},
	}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("mint an agent token: %d %s", created.Code, created.Body.String())
	}
	return decodeBody[struct {
		ID string `json:"id"`
	}](t, created).ID
}

// pinIssue pins an issue of its own for alice, answering its key.
func (tour *routeTour) pinIssue(t *testing.T) string {
	t.Helper()
	key := tour.issue(t).Key
	if pinned := dispatchRequest(t, tour.handler, http.MethodPut, "/api/v1/me/issues/"+key+"/state", map[string]any{
		"pinned": true,
	}, "alice"); pinned.Code != http.StatusOK {
		t.Fatalf("pin issue %s: %d %s", key, pinned.Code, pinned.Body.String())
	}
	return key
}

// threadVersion is the version of alice's row of the thread whose root is root, which a dismissal
// is written under.
func (tour *routeTour) threadVersion(t *testing.T, root string) int64 {
	t.Helper()
	var version int64
	if err := tour.database.Pool.QueryRow(t.Context(),
		`select version from user_threads where login = 'alice' and thread_id = $1`, root).Scan(&version); err != nil {
		t.Fatalf("read the version of alice's thread %s: %v", root, err)
	}
	return version
}

// TestV2MarksAnswerWhatEachKindStores holds the marks of the route map's section (e) to what each
// kind stores: a mark nobody wrote reads as its target's default; a mark written through PATCH
// /api/v2/me/marks/{kind}/{id} answers, and reads back through GET of the same mark, its kind's own
// fields as written (an issue's pinned items, a document's read version, an agent's pin, an ask's
// snooze and its un-snooze, a thread's dismissal and its restore); an issue mark whose seq is not
// above the stored one is 409 STATE_STALE carrying the stored mark and changes nothing; and GET
// /api/v2/me/marks lists a mark under its kind and its id alone.
func TestV2MarksAnswerWhatEachKindStores(t *testing.T) {
	tour := newRouteTour(t)
	alice := tourPerson("alice")
	patch, get := tour.requireMounted(t, http.MethodPatch, "/me/marks/{kind}/{id}"), tour.requireMounted(t, http.MethodGet, "/me/marks/{kind}/{id}")
	list := tour.requireMounted(t, http.MethodGet, "/me/marks")
	write := func(kind, id string, body any) markV2 {
		t.Helper()
		response := tour.send(t, patch, tourRequest{Path: map[string]string{"kind": kind, "id": id}, Body: body, As: alice}, "")
		if response.Code != http.StatusOK {
			t.Fatalf("write the %s mark on %s: %d %s", kind, id, response.Code, response.Body.String())
		}
		written := decodeBody[markV2](t, response)
		if read := tour.send(t, get, tourRequest{Path: map[string]string{"kind": kind, "id": id}, As: alice}, ""); read.Body.String() != response.Body.String() {
			t.Fatalf("the %s mark on %s answered %s and reads back %d %s", kind, id, response.Body.String(), read.Code, read.Body.String())
		}
		return written
	}

	issue := tour.issue(t).Key
	if fresh := decodeBody[markV2](t, tour.send(t, get, tourRequest{Path: map[string]string{"kind": "issue", "id": issue}, As: alice}, "")); fresh.Pinned == nil || *fresh.Pinned ||
		fresh.Seq == nil || *fresh.Seq != 0 || fresh.PinnedItems == nil || len(*fresh.PinnedItems) != 0 {
		t.Fatalf("an issue mark nobody wrote reads %+v, want the default: unpinned, seq 0, no pinned items", fresh)
	}
	pinned := write("issue", issue, map[string]any{"pinned": true, "last_read_seq": 3, "pinned_items": []string{"pinned_items:event:7"}, "seq": 5})
	if !*pinned.Pinned || *pinned.LastReadSeq != 3 || *pinned.Seq != 5 || len(*pinned.PinnedItems) != 1 || (*pinned.PinnedItems)[0] != "pinned_items:event:7" {
		t.Fatalf("the issue mark written answered %+v", pinned)
	}
	stale := tour.send(t, patch, tourRequest{Path: map[string]string{"kind": "issue", "id": issue}, Body: map[string]any{"pinned": false, "seq": 5}, As: alice}, "")
	refusal := decodeBody[markStaleV2](t, stale)
	if stale.Code != http.StatusConflict || refusal.Code != "STATE_STALE" || refusal.State.Seq == nil || *refusal.State.Seq != 5 || !*refusal.State.Pinned {
		t.Fatalf("an issue mark at a seq not above the stored one: %d %s, want 409 STATE_STALE with the stored mark", stale.Code, stale.Body.String())
	}

	document := tour.document(t)
	if read := write("document", document, map[string]any{"read_version": 2}); read.ReadVersion == nil || *read.ReadVersion != 2 || read.ReadAt == nil {
		t.Fatalf("the document mark written answered %+v, want read_version 2 and its read_at", read)
	}
	if agent := write("agent", tourRecipient, map[string]any{"pinned": true}); !*agent.Pinned || agent.PinnedAt == nil || *agent.UnreadReplies != 0 {
		t.Fatalf("the agent mark written answered %+v, want the session pinned", agent)
	}

	ask := tour.ask(t, tour.issue(t))
	until := time.Now().Add(2 * time.Hour).UTC().Truncate(time.Second)
	snoozed := write("ask", ask, map[string]any{"snoozed_until": until.Format(time.RFC3339)})
	if at, err := time.Parse(time.RFC3339, *snoozed.SnoozedUntil); err != nil || !at.Equal(until) {
		t.Fatalf("the ask mark snoozed answered %+v, want snoozed_until %s", snoozed, until)
	}
	if woken := write("ask", ask, map[string]any{"snoozed_until": nil}); woken.SnoozedUntil != nil {
		t.Fatalf("the ask mark un-snoozed answered %+v, want no snooze", woken)
	}

	thread := tour.comment(t, tour.issue(t))
	if dismissed := write("thread", thread, map[string]any{"dismissed": true, "version": tour.threadVersion(t, thread)}); !*dismissed.Dismissed || dismissed.DismissedAt == nil {
		t.Fatalf("the thread mark dismissed under its version answered %+v, want it dismissed", dismissed)
	}
	if restored := write("thread", thread, map[string]any{"dismissed": false}); *restored.Dismissed || restored.DismissedAt != nil {
		t.Fatalf("the thread mark put back answered %+v, want it in the Inbox again", restored)
	}

	for _, query := range []url.Values{{"kind": {"issue"}, "id": {issue}}, {"id": {issue}}} {
		listed := decodeBody[page[markV2]](t, tour.send(t, list, tourRequest{Query: query, As: alice}, ""))
		if listed.Total != 1 || len(listed.Items) != 1 || listed.Items[0].Kind != "issue" || listed.Items[0].ID != issue || !*listed.Items[0].Pinned {
			t.Fatalf("GET /api/v2/me/marks?%s answered %+v, want the one issue mark written", query.Encode(), listed)
		}
	}
}

// TestV2BulkMarksWriteAllOrNothing: PATCH /api/v2/me/marks writes every item in one transaction, so
// a request whose second item the database refuses - a snooze already past by the database's
// clock, a reply id that is no message of the session, neither of which any check before the
// write can see - answers that refusal and leaves its first item, an issue's pin, as it was, with
// no event appended for it; the same request with a second item the database takes writes both.
func TestV2BulkMarksWriteAllOrNothing(t *testing.T) {
	tour := newRouteTour(t)
	alice := tourPerson("alice")
	bulk, get := tour.requireMounted(t, http.MethodPatch, "/me/marks"), tour.requireMounted(t, http.MethodGet, "/me/marks/{kind}/{id}")
	pinnedNow := func(t *testing.T, key string) bool {
		t.Helper()
		response := tour.send(t, get, tourRequest{Path: map[string]string{"kind": "issue", "id": key}, As: alice}, "")
		if response.Code != http.StatusOK {
			t.Fatalf("read the issue mark on %s: %d %s", key, response.Code, response.Body.String())
		}
		return *decodeBody[markV2](t, response).Pinned
	}
	for _, refused := range []struct {
		name   string
		second func(t *testing.T) map[string]any
		code   string
		taken  func(t *testing.T) map[string]any
	}{
		{
			name: "a snooze already past",
			second: func(t *testing.T) map[string]any {
				return map[string]any{"kind": "ask", "id": tour.ask(t, tour.issue(t)), "snoozed_until": time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)}
			},
			code: "SNOOZE_INPUT",
			taken: func(t *testing.T) map[string]any {
				return map[string]any{"kind": "ask", "id": tour.ask(t, tour.issue(t)), "snoozed_until": time.Now().Add(time.Hour).UTC().Format(time.RFC3339)}
			},
		},
		{
			name: "a reply id that is no message of the session",
			second: func(*testing.T) map[string]any {
				return map[string]any{"kind": "agent", "id": tourRecipient, "read_replies": []string{uuid.NewString()}}
			},
			code: "INVALID_STATE",
			taken: func(*testing.T) map[string]any {
				return map[string]any{"kind": "agent", "id": tourRecipient, "read_through": time.Now().UTC().Format(time.RFC3339Nano)}
			},
		},
	} {
		t.Run(refused.name, func(t *testing.T) {
			issue, second := tour.issue(t).Key, refused.second(t)
			events := tour.eventCount(t, "", "")
			first := map[string]any{"kind": "issue", "id": issue, "pinned": true}
			response := tour.send(t, bulk, tourRequest{Body: map[string]any{"items": []map[string]any{first, second}}, As: alice}, "")
			if code, _ := refusalOf(t, response); response.Code != http.StatusBadRequest || code != refused.code {
				t.Fatalf("a bulk write whose second item the database refuses: %d %s, want 400 %s", response.Code, response.Body.String(), refused.code)
			}
			if pinnedNow(t, issue) {
				t.Fatalf("the refused bulk write left its first item written: issue %s is pinned", issue)
			}
			if after := tour.eventCount(t, "", ""); after != events {
				t.Fatalf("the refused bulk write appended %d events, want none", after-events)
			}
			written := tour.send(t, bulk, tourRequest{Body: map[string]any{"items": []map[string]any{first, refused.taken(t)}}, As: alice}, "")
			if updated := decodeBody[markUpdatesV2](t, written); written.Code != http.StatusOK || updated.Updated != 2 || !pinnedNow(t, issue) {
				t.Fatalf("the same bulk write with a second item the database takes: %d %s, want 200 {updated: 2} and the issue pinned",
					written.Code, written.Body.String())
			}
		})
	}
}

// TestV2BulkMarksOfOnePersonAtOnceBothLand: two bulk writes by one person that name the same marks
// in opposite orders, sent at once, both land, round after round: the caller's own lock orders the
// two (writeMarksIn), where each taking the first of its rows and waiting for the one the other
// took first would end one of them in a deadlock Postgres breaks with a 500.
func TestV2BulkMarksOfOnePersonAtOnceBothLand(t *testing.T) {
	tour := newRouteTour(t)
	bulk := tour.requireMounted(t, http.MethodPatch, "/me/marks")
	issues := []string{tour.issue(t).Key, tour.issue(t).Key}
	threads := []string{tour.comment(t, tour.issue(t)), tour.comment(t, tour.issue(t))}
	for round := range 10 {
		marks := func(order ...int) tourRequest {
			var items []map[string]any
			for _, at := range order {
				items = append(items,
					map[string]any{"kind": "issue", "id": issues[at], "pinned": round%2 == at},
					map[string]any{"kind": "thread", "id": threads[at], "read_through": time.Now().UTC().Format(time.RFC3339Nano)},
				)
			}
			return tourRequest{Body: map[string]any{"items": items}, As: tourPerson("alice")}
		}
		// Each request is built here, on the test's goroutine, which alone may fail the test.
		requests := []*http.Request{tour.request(t, bulk, marks(0, 1), ""), tour.request(t, bulk, marks(1, 0), "")}
		responses := make([]*httptest.ResponseRecorder, len(requests))
		var sending sync.WaitGroup
		for index, request := range requests {
			sending.Go(func() { responses[index] = tour.serve(request) })
		}
		sending.Wait()
		for index, response := range responses {
			if response.Code != http.StatusOK {
				t.Fatalf("round %d: the bulk write %d of two sent at once answered %d %s, want 200", round, index, response.Code, response.Body.String())
			}
		}
	}
}

// TestV2BulkMarksAndTheV1InboxWritesOfOnePersonAtOnceAllLand: a bulk write of one person's marks
// under /api/v2, their Mark all read, their Dismiss (or its restore) and their conversation state
// under /api/v1, all over the same Reply threads and the same session's Agent threads with
// replies to read and to prune, sent at once, all land, round after round. Each takes that
// person's mark rows in an order of its own; the person's marks lock (lockPersonMarks) holds the
// bulk write and the conversation state alone, and lets the mark-all and the dismissal, which the
// Inbox's lock order already orders, run beside each other.
func TestV2BulkMarksAndTheV1InboxWritesOfOnePersonAtOnceAllLand(t *testing.T) {
	// A server whose session s1 is live, so each Agent thread's message reaches it and its replies
	// come back, as seedInboxRead builds one.
	live := true
	sent := []map[string]any{}
	listener := sessionListener(t, &live, &sent)
	t.Cleanup(listener.Close)
	handler, _ := newTargetedMessageHandler(t, listener.URL)
	issue := createInteractionIssue(t, handler, "RACE", "Marks at once", "A spec")
	comments := "/api/v1/issues/" + issue.Key + "/comments"
	threads := []string{seedUnreadThread(t, handler, comments, "Which way?"), seedUnreadThread(t, handler, comments, "And then?")}
	var roots []string
	for index := range 2 {
		root, _ := agentThread(t, handler, "btw", fmt.Sprintf("Ask %d", index), fmt.Sprintf("Answer %d", index))
		roots = append(roots, root.ID)
	}
	for round := range 10 {
		// Each round makes every thread unread again: a session's reply under each Reply thread
		// and under each Agent thread, whose id is read by id below and pruned once a read mark
		// passes it.
		var replies []string
		for _, thread := range threads {
			sessionComment(t, handler, comments, map[string]any{"body": fmt.Sprintf("Round %d.", round), "reply_to": thread})
		}
		for _, root := range roots {
			replied := replyTo(t, handler, root, fmt.Sprintf("Round %d, %s.", round, root))
			if replied.Code != http.StatusCreated {
				t.Fatalf("round %d: the session's reply under %s: %d %s", round, root, replied.Code, replied.Body.String())
			}
			replies = append(replies, decodeBody[model.Message](t, replied).ID)
		}
		now := time.Now().UTC().Format(time.RFC3339Nano)
		reversed := slices.Clone(replies)
		slices.Reverse(reversed)
		dismissal := map[string]any{"dismiss_through": now, "restore": round%2 == 1}
		// Each request is built here, on the test's goroutine, which alone may fail the test, and
		// served on a goroutine of its own.
		writes := map[string]*http.Request{
			"the bulk write under /api/v2": newDispatchRequest(t, http.MethodPatch, "/api/v2/me/marks", map[string]any{"items": []map[string]any{
				{"kind": "agent", "id": "s1", "read_through": now, "read_replies": replies},
				{"kind": "thread", "id": threads[1], "read_through": now},
				{"kind": "issue", "id": issue.Key, "pinned": round%2 == 0},
				{"kind": "thread", "id": threads[0], "read_through": now},
			}}, "alice"),
			"the mark-all":  newDispatchRequest(t, http.MethodPost, "/api/v1/me/inbox/read", map[string]any{"read_through": now}, "alice"),
			"the dismissal": newDispatchRequest(t, http.MethodPost, "/api/v1/me/inbox/dismiss", dismissal, "alice"),
			"the conversation's state": newDispatchRequest(t, http.MethodPut, "/api/v1/me/agents/s1/state", map[string]any{
				"read_through": now, "read_replies": reversed,
			}, "alice"),
		}
		responses := make(map[string]*httptest.ResponseRecorder, len(writes))
		var mu sync.Mutex
		var sending sync.WaitGroup
		for name, write := range writes {
			sending.Go(func() {
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, write)
				mu.Lock()
				defer mu.Unlock()
				responses[name] = response
			})
		}
		sending.Wait()
		for name, response := range responses {
			if response.Code != http.StatusOK {
				t.Fatalf("round %d: %s, sent beside the others, answered %d %s, want 200", round, name, response.Code, response.Body.String())
			}
		}
	}
}
