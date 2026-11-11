// ask_decline_test.go proves what POST /api/v1/asks/{id}/decline is: the one act a person
// closes a question with when they will not answer it. Only a credential acting as a person
// reaches it, an approval request is answered and never declined, and the record it writes keeps
// the person's note apart from the words Dispatch writes beside it.
package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// declineBody is one decline request's body: the note where the person wrote one, and nothing
// at all where they wrote none.
func declineBody(note *string) map[string]any {
	if note == nil {
		return map[string]any{}
	}
	return map[string]any{"note": *note}
}

// storedAskResolution is the resolution the ask's row holds, as every reader of it decodes it.
func storedAskResolution(t *testing.T, database *store.Store, askID string) model.AskResolution {
	t.Helper()
	var raw []byte
	if err := database.Pool.QueryRow(context.Background(), `select resolution from asks where id = $1`, askID).Scan(&raw); err != nil {
		t.Fatalf("read ask %s's resolution: %v", askID, err)
	}
	var resolution model.AskResolution
	if err := json.Unmarshal(raw, &resolution); err != nil {
		t.Fatalf("decode ask %s's resolution %s: %v", askID, raw, err)
	}
	return resolution
}

// requireAskState fails t unless the ask's row stands in state.
func requireAskState(t *testing.T, database *store.Store, what, askID, state string) {
	t.Helper()
	var stored string
	if err := database.Pool.QueryRow(context.Background(), `select state from asks where id = $1`, askID).Scan(&stored); err != nil {
		t.Fatalf("read ask %s's state: %v", askID, err)
	}
	if stored != state {
		t.Fatalf("%s: the ask is %q, want it %q", what, stored, state)
	}
}

// requireNotDeclinable fails t unless response is the refusal an approval request answers a
// decline with.
func requireNotDeclinable(t *testing.T, what string, response *httptest.ResponseRecorder) {
	t.Helper()
	if response.Code != http.StatusConflict {
		t.Fatalf("%s: status=%d body=%s, want 409", what, response.Code, response.Body.String())
	}
	body := decodeBody[refusalAnswer](t, response)
	if body.Code != "ASK_NOT_DECLINABLE" {
		t.Fatalf("%s: the refusal = %+v, want ASK_NOT_DECLINABLE", what, body)
	}
	if !strings.Contains(body.Error, docs.ApprovalOptionApprove) || !strings.Contains(body.Error, docs.ApprovalOptionRequestChanges) {
		t.Fatalf("%s: the refusal says %q, want it to name %q and %q",
			what, body.Error, docs.ApprovalOptionApprove, docs.ApprovalOptionRequestChanges)
	}
}

// openApprovalAsk requests approval of a document and answers the approval ask that opens.
func openApprovalAsk(t *testing.T, handler http.Handler, artifactID string) string {
	t.Helper()
	requested := sessionRequest(t, handler, http.MethodPost,
		"/api/v1/artifacts/"+artifactID+"/approval-requests", map[string]any{"actor": sessionActor()})
	if requested.Code != http.StatusCreated {
		t.Fatalf("request approval: status=%d body=%s", requested.Code, requested.Body.String())
	}
	return decodeBody[struct {
		Ask struct {
			ID string `json:"id"`
		} `json:"ask"`
	}](t, requested).Ask.ID
}

// A decline is a person's word that they will not answer, so the route is theirs: a credential
// that acts for nobody is refused it and the question stays open, the person at their browser
// makes it, and so does an agent holding that person's own credential, whose act is recorded
// beside them exactly as an answer by the same credential records it. An approval request is no
// question to decline: it is refused whoever sends it and keeps the two review options. The
// resolve route is unchanged beside it, so the room a decline makes for a person who writes
// nothing is the decline's alone.
func TestOnlyAPersonScopeRecordsADeclineAndNeverOnAnApprovalRequest(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "DEC", "Declining a question", "A spec")
	note := "I have no view on this."

	// The deployment's shared token holds no scope at all, so it acts for nobody.
	scopeless := openAskAs(t, handler, issue.Key, "session-asking", "Which transport, asked of a scopeless caller?")
	refused := sessionRequest(t, handler, http.MethodPost, "/api/v1/asks/"+scopeless+"/decline", declineBody(&note))
	if refused.Code != http.StatusForbidden || decodeBody[refusalAnswer](t, refused).Code != codeScopeRequired {
		t.Fatalf("a decline by the shared token: status=%d body=%s, want 403 %s", refused.Code, refused.Body.String(), codeScopeRequired)
	}
	requireAskState(t, database, "after the shared token's decline", scopeless, "open")

	// A person at a browser: the act Dispatch was built for.
	browser := openAskAs(t, handler, issue.Key, "session-asking", "Which transport, asked of a browser?")
	if declined := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+browser+"/decline",
		declineBody(&note), "alice"); declined.Code != http.StatusOK {
		t.Fatalf("a person's decline: status=%d body=%s", declined.Code, declined.Body.String())
	}
	requireAskState(t, database, "after the person's decline", browser, "resolved")

	// That person's own credential, presented by an agent naming its session.
	token := mintPersonalToken(t, handler, "alice", actingTokenName, nil)
	byAgent := openAskAs(t, handler, issue.Key, "session-asking", "Which transport, asked of an agent?")
	if declined := agentRequest(t, handler, http.MethodPost,
		"/api/v1/asks/"+byAgent+"/decline?session="+actingSession, declineBody(&note), token.Token); declined.Code != http.StatusOK {
		t.Fatalf("a decline with the person's own token: status=%d body=%s", declined.Code, declined.Body.String())
	}
	resolution := storedAskResolution(t, database, byAgent)
	requirePersonActor(t, "the stored resolution's", resolution.Actor, "alice")
	requireActingToken(t, "the stored resolution's", resolution.Actor.Act, actingSession, token, model.ScopePerson, model.ScopeApprove)
	requirePersonActor(t, "the ask.resolved event's", latestAskEventActor(t, database, byAgent, "ask.resolved"), "alice")

	// An approval request is answered with its two options, by whoever may answer it.
	approval := openApprovalAsk(t, handler, issue.PrimaryArtifactID)
	requireNotDeclinable(t, "a person's decline of an approval request",
		dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+approval+"/decline", declineBody(&note), "alice"))
	requireNotDeclinable(t, "an agent's decline of an approval request with the person's token",
		agentRequest(t, handler, http.MethodPost, "/api/v1/asks/"+approval+"/decline?session="+actingSession,
			declineBody(nil), token.Token))
	requireAskState(t, database, "after the refused declines", approval, "open")
	options := []string{}
	for _, option := range readBlockAsk(t, handler, approval).Options {
		options = append(options, option.Label)
	}
	if len(options) != 2 || options[0] != docs.ApprovalOptionApprove || options[1] != docs.ApprovalOptionRequestChanges {
		t.Fatalf("the approval request's options = %v, want %q and %q",
			options, docs.ApprovalOptionApprove, docs.ApprovalOptionRequestChanges)
	}

	// The resolve route still requires a reason of both its kinds, so nothing a decline allows
	// has leaked into it.
	for _, kind := range []string{"retracted", "resolved"} {
		ask := openAskAs(t, handler, issue.Key, "session-asking", "Which transport, "+kind+" with no reason?")
		response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+ask+"/resolve",
			map[string]any{"kind": kind, "reason": "  "}, "alice")
		if response.Code != http.StatusBadRequest || decodeBody[refusalAnswer](t, response).Code != "INVALID_RESOLUTION" {
			t.Fatalf("a %s with a blank reason: status=%d body=%s, want 400 INVALID_RESOLUTION",
				kind, response.Code, response.Body.String())
		}
		requireAskState(t, database, "after the blank-reason "+kind, ask, "open")
	}

	// A credential the broker scoped to one project holds no scope of the person's either, so
	// it too acts for nobody here. It runs on its own server, the one this package drives the
	// broker's tokens through.
	t.Run("a token scoped to one project", func(t *testing.T) {
		rig := newBrokerTokenRig(t)
		caller := newBrokerCaller(t)
		const token = "a-token-scoped-to-one-project"
		rig.broker.Says(token, liveNamedToken(caller.jkt))
		fixture := seedProject(t, rig, brokerTestProject, "Broker")

		refused := rig.serve(caller.presenting(t, http.MethodPost,
			"/api/v1/asks/"+fixture.ask+"/decline", token, map[string]any{"note": "Not mine to say."}))
		if refused.Code != http.StatusForbidden || decodeBody[refusalAnswer](t, refused).Code != codeScopeRequired {
			t.Fatalf("a decline by a project-scoped token: status=%d body=%s, want 403 %s",
				refused.Code, refused.Body.String(), codeScopeRequired)
		}
		requireAskState(t, rig.database, "after the project-scoped token's decline", fixture.ask, "open")
	})
}

// What a decline writes: the kind declined, the person who declined, the time, Dispatch's four
// sentences word for word, and the person's note on its own beside them. A decline with no note
// still carries the sentences. The note takes the bound every event body takes, and one unit
// over is refused by name with nothing written. Neither declined ask holds an answer, neither
// is on the person's answers page, each has one ask.resolved event, and a declined block ask
// reads resolved in its document.
func TestADeclinedAskReadsDeclinedAndWritesNoAnswer(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "DEC", "What a decline records", "A spec")
	note := "The team decides this one, not me."

	withNote := openAskAs(t, handler, issue.Key, "session-asking", "Which transport, declined with a note?")
	// WriteAskResolution stamps the resolution with the writing process's own clock, which is
	// this test's clock too.
	before := time.Now().UTC()
	if declined := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+withNote+"/decline",
		declineBody(&note), "alice"); declined.Code != http.StatusOK {
		t.Fatalf("decline with a note: status=%d body=%s", declined.Code, declined.Body.String())
	}
	after := time.Now().UTC()
	resolution := storedAskResolution(t, database, withNote)
	if resolution.Kind != "declined" {
		t.Fatalf("the resolution's kind = %q, want declined", resolution.Kind)
	}
	requirePersonActor(t, "the resolution's", resolution.Actor, "alice")
	if resolution.At.Before(before) || resolution.At.After(after) {
		t.Fatalf("the resolution's at = %s, want it between %s and %s", resolution.At, before, after)
	}
	if resolution.DispatchWords != model.DeclineDispatchWords {
		t.Fatalf("the resolution's dispatch_words = %q, want %q", resolution.DispatchWords, model.DeclineDispatchWords)
	}
	if resolution.Reason != note {
		t.Fatalf("the resolution's reason = %q, want the note %q alone", resolution.Reason, note)
	}
	if strings.Contains(resolution.Reason, "The person chose not to answer") {
		t.Fatalf("the resolution's reason = %q, want it free of the words Dispatch writes", resolution.Reason)
	}
	// The wire carries the two apart under their own names, which is what every reader reads.
	served := map[string]any{}
	if err := json.Unmarshal([]byte(declinedAskJSON(t, handler, withNote)), &served); err != nil {
		t.Fatalf("decode the served ask: %v", err)
	}
	if served["dispatch_words"] != model.DeclineDispatchWords || served["reason"] != note {
		t.Fatalf("the served resolution = %v, want the sentences in dispatch_words and the note in reason", served)
	}

	withoutNote := openAskAs(t, handler, issue.Key, "session-asking", "Which transport, declined with no note?")
	if declined := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+withoutNote+"/decline",
		declineBody(nil), "alice"); declined.Code != http.StatusOK {
		t.Fatalf("decline with no note: status=%d body=%s", declined.Code, declined.Body.String())
	}
	bare := storedAskResolution(t, database, withoutNote)
	if bare.Kind != "declined" || bare.DispatchWords != model.DeclineDispatchWords || bare.Reason != "" {
		t.Fatalf("the resolution of a decline with no note = %+v, want the kind declined, the sentences and no reason", bare)
	}

	// The note rides the ask.resolved event, so it takes that event's bound and no more.
	atTheBound := openAskAs(t, handler, issue.Key, "session-asking", "Which transport, declined at the bound?")
	longest := strings.Repeat("a", contracts.DispatchEventBodyMax)
	if declined := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+atTheBound+"/decline",
		declineBody(&longest), "alice"); declined.Code != http.StatusOK {
		t.Fatalf("decline with a note at the bound: status=%d body=%.200s", declined.Code, declined.Body.String())
	}
	if stored := storedAskResolution(t, database, atTheBound); stored.Reason != longest {
		t.Fatalf("the note at the bound was stored as %d characters, want all %d", len(stored.Reason), len(longest))
	}
	overTheBound := openAskAs(t, handler, issue.Key, "session-asking", "Which transport, declined over the bound?")
	over := longest + "a"
	refused := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+overTheBound+"/decline",
		declineBody(&over), "alice")
	assertCapExceeded(t, "a decline's note", refused, "note",
		contracts.DispatchEventBodyMax+1, contracts.DispatchEventBodyMax, "")
	requireAskState(t, database, "after the note over the bound", overTheBound, "open")

	// A decline is no answer: neither ask holds one, and neither is on the person's page of
	// what she has answered.
	for _, ask := range []string{withNote, withoutNote} {
		if read := readBlockAsk(t, handler, ask); read.State != "resolved" || read.Answer != nil {
			t.Fatalf("the declined ask reads state %q with answer %+v, want resolved and no answer", read.State, read.Answer)
		}
	}
	for _, row := range getMyAnswers(t, handler, "alice", "").Rows {
		if row.AskID == withNote || row.AskID == withoutNote {
			t.Fatalf("a declined ask is on the answers page: %#v", row)
		}
	}

	// One ask.resolved event for each decline, and none for the ask the bound refused.
	resolved := map[string]int{}
	for _, payload := range eventPayloads(t, handler, issue.Key, "ask.resolved") {
		id, _ := payload["id"].(string)
		resolved[id]++
	}
	for _, ask := range []string{withNote, withoutNote, atTheBound} {
		if resolved[ask] != 1 {
			t.Fatalf("ask %s has %d ask.resolved events, want exactly one", ask, resolved[ask])
		}
	}
	if resolved[overTheBound] != 0 {
		t.Fatalf("the ask the bound refused has %d ask.resolved events, want none", resolved[overTheBound])
	}

	// A declined ask whose question is a document's decision block leaves that block closed, as
	// every other resolution leaves it. It runs on a server whose documents settle fast enough
	// to watch.
	t.Run("a declined block ask reads resolved in its document", func(t *testing.T) {
		handler, _ := blockAskHandler(t)
		issue, askID := seedBlockAsk(t, handler, "Declined block", "decision", transportAsk, "Which transport?")
		if declined := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+askID+"/decline",
			map[string]any{"note": "Whoever builds it decides."}, "alice"); declined.Code != http.StatusOK {
			t.Fatalf("decline a block ask: status=%d body=%s", declined.Code, declined.Body.String())
		}
		awaitDocumentText(t, handler, issue.PrimaryArtifactID, `the block reading state="resolved"`,
			contains(`#decision urgency="high" multiple="false" state="resolved"`), nil)
	})
}

// declinedAskJSON is the resolution the served ask carries, as the wire writes it.
func declinedAskJSON(t *testing.T, handler http.Handler, askID string) string {
	t.Helper()
	read := dispatchRequest(t, handler, http.MethodGet, "/api/v1/asks/"+askID, nil, "alice")
	if read.Code != http.StatusOK {
		t.Fatalf("read ask %s: status=%d body=%s", askID, read.Code, read.Body.String())
	}
	resolution := decodeBody[struct {
		Ask struct {
			Resolution json.RawMessage `json:"resolution"`
		} `json:"ask"`
	}](t, read).Ask.Resolution
	if len(resolution) == 0 {
		t.Fatalf("the served ask %s carries no resolution", askID)
	}
	return string(resolution)
}
