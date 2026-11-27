package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/testwait"
)

// Closing an issue withdraws every open ask it holds, whoever closes it: each is recorded resolved
// as withdrawn by the close, naming the closer, and stops reading open in the documents' text too,
// which renders each ask's state from the store. Reopening the issue
// opens exactly those asks again.

// closeDecision is the ask block the closing tests' spec holds, and a copy of it a second
// document of the issue holds.
const closeDecision = "Context\n\n:::ask{#close-decision urgency=\"med\" multiple=\"false\"}\nShip it?\n\n- Yes: Now.\n- No: Later.\n:::\n"

// closeAsksFixture is an issue holding one open ask of every kind - a plain ask waiting on a
// person, a plain ask waiting on its agent, an ask block in the spec with a copy of it in a
// second document, an approval request on the spec - beside an answered and a retracted ask.
type closeAsksFixture struct {
	handler   http.Handler
	database  *store.Store
	documents *docs.Service
	issue     string
	spec      string
	copy      string
	// open are the open asks: the person's, the agent's, the ask block's, the approval's.
	personAsk, agentAsk, blockAsk, approvalAsk string
	answeredAsk, retractedAsk                  string
}

func (f closeAsksFixture) open() []string {
	return []string{f.personAsk, f.agentAsk, f.blockAsk, f.approvalAsk}
}

// newCloseAsksFixture is newCloseAsksFixtureSettlingAfter with a settle delay of 20 ms: the
// settlement a write arms runs while the test does.
func newCloseAsksFixture(t *testing.T) closeAsksFixture {
	t.Helper()
	return newCloseAsksFixtureSettlingAfter(t, 20*time.Millisecond)
}

// newCloseAsksFixtureSettlingAfter is closeAsksFixture over a document service whose settle delay
// is settle. Wherever the fixture needs a settlement it runs it itself (SettleIssueDocuments), so
// it is built the same whatever the delay, and it leaves no document owing one.
func newCloseAsksFixtureSettlingAfter(t *testing.T, settle time.Duration) closeAsksFixture {
	t.Helper()
	var documents *docs.Service
	handler, database := newInteractionHandler(t, func(database *store.Store) docs.API {
		documents = docs.New(docs.Deps{Store: database, Settle: settle})
		t.Cleanup(func() { _ = documents.Shutdown(context.Background()) })
		return documents
	})
	issue := createInteractionIssue(t, handler, "TEST", "Closing with asks", closeDecision)
	f := closeAsksFixture{handler: handler, database: database, documents: documents, issue: issue.Key, spec: issue.PrimaryArtifactID}
	settleDocuments := func(what string) {
		t.Helper()
		if err := documents.SettleIssueDocuments(context.Background(), f.issue); err != nil {
			t.Fatalf("settle %s: %v", what, err)
		}
	}
	settleDocuments("the spec")
	awaitIndexedAskBlock(t, handler, f.spec, "close-decision", "Ship it?")
	f.blockAsk = requireBlockAsk(t, handler, f.issue, f.spec, "close-decision")
	uploaded := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+f.issue+"/artifacts", map[string]any{
		"actor": sessionActor(), "name": "spec-record.md", "content": closeDecision,
	})
	if uploaded.Code != http.StatusCreated {
		t.Fatalf("upload the copy: status=%d body=%s", uploaded.Code, uploaded.Body.String())
	}
	f.copy = decodeBody[struct {
		Artifact model.Artifact `json:"artifact"`
	}](t, uploaded).Artifact.ID
	settleDocuments("the copy")
	awaitTextShows(t, handler, f.copy, "the copy of the open decision", contains(
		`#close-decision state="open" copied_from="`+f.blockAsk+`"`,
	), nil)
	awaitSettled(t, database, f.spec, f.copy)
	f.approvalAsk = requestApproval(t, handler, f.spec).ID

	f.personAsk = createSessionAsk(t, handler, f.issue, "Which region?")
	f.agentAsk = createSessionAsk(t, handler, f.issue, "Which owner?")
	if reply := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+f.issue+"/comments", map[string]any{
		"body": "Who are the candidates?", "ask_id": f.agentAsk,
	}, "alice"); reply.Code != http.StatusCreated {
		t.Fatalf("reply to the agent's ask: status=%d body=%s", reply.Code, reply.Body.String())
	}
	f.answeredAsk = createSessionAsk(t, handler, f.issue, "Ship on Monday?")
	if answered := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+f.answeredAsk+"/answer", map[string]any{
		"text": "Yes.",
	}, "alice"); answered.Code != http.StatusOK {
		t.Fatalf("answer: status=%d body=%s", answered.Code, answered.Body.String())
	}
	f.retractedAsk = createSessionAsk(t, handler, f.issue, "Still needed?")
	if retracted := sessionRequest(t, handler, http.MethodPost, "/api/v1/asks/"+f.retractedAsk+"/resolve", map[string]any{
		"kind": "retracted", "reason": "Not needed.", "actor": sessionActor(),
	}); retracted.Code != http.StatusOK {
		t.Fatalf("retract: status=%d body=%s", retracted.Code, retracted.Body.String())
	}
	for ask, want := range map[string]string{f.personAsk: "human", f.agentAsk: "agent", f.blockAsk: "human", f.approvalAsk: "human"} {
		if got := readBlockAsk(t, handler, ask); got.State != "open" || got.WaitingOn != want {
			t.Fatalf("ask %s = %s waiting on %q, want open waiting on %s", ask, got.State, got.WaitingOn, want)
		}
	}
	settleDocuments("the issue's documents")
	awaitSettled(t, database, f.spec, f.copy)
	return f
}

// closer is one of the writers that close issues, as their requests reach PATCH
// /api/v1/issues/{key}.
type closer struct {
	name  string
	actor model.Actor
	send  func(t *testing.T, handler http.Handler, issue string, body map[string]any) *httptest.ResponseRecorder
}

var closers = []closer{
	{
		name:  "a person",
		actor: model.Actor{Kind: "user", ID: "alice"},
		send: func(t *testing.T, handler http.Handler, issue string, body map[string]any) *httptest.ResponseRecorder {
			return dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue, body, "alice")
		},
	},
	sessionCloser("an agent session", "session-0123456789abcdef"),
	// The Legion daemon's request (packages/daemon/internal/dispatch/client.go SetStatus).
	sessionCloser("the Legion daemon", "legion-daemon:TEST"),
}

func sessionCloser(name, id string) closer {
	return closer{
		name:  name,
		actor: model.Actor{Kind: "session", ID: id},
		send: func(t *testing.T, handler http.Handler, issue string, body map[string]any) *httptest.ResponseRecorder {
			withActor := map[string]any{"actor": map[string]any{"kind": "session", "id": id}}
			for key, value := range body {
				withActor[key] = value
			}
			return sessionRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue, withActor)
		},
	}
}

// Closing an issue withdraws its open asks, on each ask's node and its row, and the documents'
// text, which renders each ask's state from the store, shows their blocks resolved. The close
// writes the withdrawn blocks' resolved state into each document, as a version keeps the state of
// an ask that is not open for the release before's readers, so the settlement after it, on the
// closed issue, saves one version of each document, differing from the one before only in those
// blocks' openers and appending only its quiet artifact.version event; it changes no ask row, no
// answer or review, and appends no ask event. The issue is born converted, so that settlement also
// marks its queries owed, and the drain after it writes one version of the issue's decisions
// query, whose answer holds the withdrawn asks, with its own quiet artifact.version (R7: "Every
// version the drain writes appends `artifact.version`"). Reopening restores exactly the withdrawn
// asks, the text shows their blocks open again, and the settlement after the reopen saves one more
// version of each document, the blocks' state taken off again and a copy keeping its source. The
// settle delay is an hour, so the test runs each settlement itself (SettleIssueDocuments, as the
// timer, the room's next load or the resumption would), and each read in between meets the issue
// as the write before it left it. The reopen is sent against a read taken after the closed
// issue's settlement and its drain, as a client that reads the issue before it writes sends it.
func TestClosingAnIssueWithdrawsItsOpenAsksAndReopeningRestoresThem(t *testing.T) {
	t.Parallel()
	for _, closer := range closers {
		t.Run(closer.name, func(t *testing.T) {
			f := newCloseAsksFixtureSettlingAfter(t, time.Hour)
			ctx := context.Background()
			documents := []string{f.spec, f.copy}
			resolved := map[string]string{
				f.spec: `#close-decision state="resolved"` + "\n",
				f.copy: `#close-decision state="resolved" copied_from="` + f.blockAsk + `"`,
			}
			preClose := readIssueDetail(t, f.handler, f.issue)
			if len(preClose.OpenAsks) != 4 {
				t.Fatalf("open asks before the close = %d, want 4", len(preClose.OpenAsks))
			}
			settledRows := askRowTexts(t, f.database, f.answeredAsk, f.retractedAsk)
			versions := map[string][]string{}
			latestBefore := map[string]int{}
			for _, document := range documents {
				versions[document] = versionHashes(t, f.database, document)
				stored := documentVersions(t, f.handler, document)
				latestBefore[document] = stored[len(stored)-1].Number
			}
			reviews := reviewCount(t, f.database, f.spec, f.copy)
			decisions := decisionEventCount(t, f.database, f.issue)
			// The decisions query as the fixture's writes left it, once no drain is owed.
			renderAtVersionQuiet(t, f.database, f.issue, f.spec)
			decisionsQuery, decisionsBefore := decisionsQueryVersions(t, f.database, f.issue)
			if decisionsQuery == "" {
				t.Fatalf("%s holds no decisions query node: the issue is not born converted, so this case proves nothing", f.issue)
			}
			openedBefore := map[string]int{}
			recordsBefore := map[string]model.Ask{}
			for _, ask := range f.open() {
				openedBefore[ask] = askEventCount(t, f.database, ask, "ask.opened")
				recordsBefore[ask] = readBlockAsk(t, f.handler, ask)
			}
			if inbox := inboxAskIDs(t, f.handler); !containsAll(inbox, f.open()) {
				t.Fatalf("inbox before the close = %v, want every open ask %v", inbox, f.open())
			}

			closed := closer.send(t, f.handler, f.issue, map[string]any{"status": "done"})
			if closed.Code != http.StatusOK {
				t.Fatalf("close: status=%d body=%s", closed.Code, closed.Body.String())
			}
			response := decodeBody[model.Issue](t, closed)
			closedBy := latestEventActor(t, f.database, f.issue, "issue.closed")
			if closedBy.Kind != closer.actor.Kind || closedBy.ID != closer.actor.ID {
				t.Fatalf("issue.closed actor = %#v, want %s %s", closedBy, closer.actor.Kind, closer.actor.ID)
			}

			detail := readIssueDetail(t, f.handler, f.issue)
			if len(detail.OpenAsks) != 0 {
				t.Fatalf("open asks after the close = %d, want none", len(detail.OpenAsks))
			}
			// The close withdraws the open questions, which the decisions query takes, so its write
			// marks the issue's queries owed and its drain, after the close's COMMIT, writes the
			// decisions query's next version (R14): that version's artifact.version is the one event
			// past the close's answer.
			if detail.LastSeq != response.LastSeq+1 {
				t.Fatalf("close response last_seq = %d, a read of the issue answers %d, want the one event the close's drain appends past it", response.LastSeq, detail.LastSeq)
			}
			requireDecisionsVersionEventAt(t, f.database, f.issue, detail.LastSeq, decisionsQuery)
			for _, ask := range f.open() {
				requireWithdrawn(t, f.handler, f.issue, ask, closedBy)
				if count := askEventCount(t, f.database, ask, "ask.resolved"); count != 1 {
					t.Fatalf("ask %s has %d ask.resolved events, want 1", ask, count)
				}
			}
			for _, document := range documents {
				requireLiveText(t, f.handler, document, resolved[document])
			}
			if got := askRowTexts(t, f.database, f.answeredAsk, f.retractedAsk); !reflect.DeepEqual(got, settledRows) {
				t.Fatalf("the answered and retracted asks changed:\n%v\nwant\n%v", got, settledRows)
			}
			if got := decisionEventCount(t, f.database, f.issue); got != decisions {
				t.Fatalf("the close appended %d answer or review events", got-decisions)
			}
			if got := reviewCount(t, f.database, f.spec, f.copy); got != reviews {
				t.Fatalf("the close wrote %d reviews", got-reviews)
			}
			if approval := readApproval(t, f.handler, f.spec).Approval; approval != nil && approval.State == "awaiting" {
				t.Fatalf("the spec's approval = %#v, want no request awaiting", approval)
			}
			for _, document := range documents {
				if got := versionHashes(t, f.database, document); !reflect.DeepEqual(got, versions[document]) {
					t.Fatalf("the close wrote versions of %s:\n%v\nwant\n%v", document, got, versions[document])
				}
			}

			// The settlement the close's write left owed, on the closed issue.
			asks := append(f.open(), f.answeredAsk, f.retractedAsk)
			rows := askRowTexts(t, f.database, asks...)
			askEvents := issueAskEventCount(t, f.database, f.issue)
			if err := f.documents.SettleIssueDocuments(ctx, f.issue); err != nil {
				t.Fatalf("settle the closed issue's documents: %v", err)
			}
			awaitSettled(t, f.database, documents...)
			// The settlement committed its versions and its query mark together; its drain finds the
			// decisions query's record as the close's drain left it.
			renderAtVersionQuiet(t, f.database, f.issue, f.spec)
			closedAt := map[string]int{}
			for _, document := range documents {
				closedAt[document] = requireStateVersionsPast(t, f.handler, f.database, document, latestBefore[document], 1)
				if stored := versionMarkdown(t, f.handler, document, closedAt[document]); !closeDecisionState.MatchString(stored) ||
					closeDecisionState.FindStringSubmatch(stored)[1] != "resolved" {
					t.Fatalf("the version the closed issue's settlement saved of %s = %s, want the decision's opener showing it resolved", document, stored)
				}
				requireLiveText(t, f.handler, document, resolved[document])
			}
			if _, drained := decisionsQueryVersions(t, f.database, f.issue); drained != decisionsBefore+1 {
				t.Fatalf("the decisions query of %s holds version %d after the closed issue's settlement, want %d: the close's drain writes one version of it, holding the withdrawn asks, and the settlement moves its record no further",
					f.issue, drained, decisionsBefore+1)
			}
			requireQuietVersionEvents(t, f.database, f.issue, response.LastSeq, len(documents)+1)
			versioned := map[string]int{f.spec: closedAt[f.spec], f.copy: closedAt[f.copy], decisionsQuery: decisionsBefore + 1}
			for artifact, number := range versioned {
				if _, _, held := documentEventOf(t, f.database, "artifact.version", artifact, number); !held {
					t.Fatalf("the events after the close hold no artifact.version of version %d of %s, want one for each of %v", number, artifact, versioned)
				}
			}
			if got := askRowTexts(t, f.database, asks...); !reflect.DeepEqual(got, rows) {
				t.Fatalf("the closed issue's settlement changed the asks:\n%v\nwant them as the close left them\n%v", got, rows)
			}
			if got := issueAskEventCount(t, f.database, f.issue); got != askEvents {
				t.Fatalf("the closed issue's settlement appended %d ask events", got-askEvents)
			}
			if got := decisionEventCount(t, f.database, f.issue); got != decisions {
				t.Fatalf("the closed issue's settlement appended %d answer or review events", got-decisions)
			}
			if got := reviewCount(t, f.database, f.spec, f.copy); got != reviews {
				t.Fatalf("the closed issue's settlement wrote %d reviews", got-reviews)
			}
			if approval := readApproval(t, f.handler, f.spec).Approval; approval != nil && approval.State == "awaiting" {
				t.Fatalf("the spec's approval after the closed issue's settlement = %#v, want no request awaiting", approval)
			}

			stale := dispatchRequest(t, f.handler, http.MethodPatch, "/api/v1/issues/"+f.issue, map[string]any{
				"status": "todo", "expected_sequence": preClose.LastSeq,
			}, "alice")
			if stale.Code != http.StatusConflict || !strings.Contains(stale.Body.String(), `"code":"ISSUE_CHANGED"`) {
				t.Fatalf("reopen against the pre-close sequence: status=%d body=%s", stale.Code, stale.Body.String())
			}
			settled := readIssueDetail(t, f.handler, f.issue)
			reopened := dispatchRequest(t, f.handler, http.MethodPatch, "/api/v1/issues/"+f.issue, map[string]any{
				"status": "todo", "expected_sequence": settled.LastSeq,
			}, "alice")
			if reopened.Code != http.StatusOK {
				t.Fatalf("reopen against the sequence read after the closed issue's settlement: status=%d body=%s", reopened.Code, reopened.Body.String())
			}
			reopenResponse := decodeBody[model.Issue](t, reopened)

			detail = readIssueDetail(t, f.handler, f.issue)
			if got, want := openAskIDs(detail), sorted(f.open()); !reflect.DeepEqual(got, want) {
				t.Fatalf("open asks after the reopen = %v, want exactly the withdrawn %v", got, want)
			}
			// The reopen restores the questions the close withdrew, so its drain writes the decisions
			// query's next version past its answer, as the close's did (R14).
			if detail.LastSeq != reopenResponse.LastSeq+1 {
				t.Fatalf("reopen response last_seq = %d, a read of the issue answers %d, want the one event the reopen's drain appends past it", reopenResponse.LastSeq, detail.LastSeq)
			}
			requireDecisionsVersionEventAt(t, f.database, f.issue, detail.LastSeq, decisionsQuery)
			for _, ask := range f.open() {
				got := readBlockAsk(t, f.handler, ask)
				before := recordsBefore[ask]
				if got.State != "open" || got.Resolution != nil || got.Answer != nil || got.WaitingOn != before.WaitingOn {
					t.Fatalf("restored ask %s = %s %#v waiting on %q, want open with no record waiting on %q",
						ask, got.State, got.Resolution, got.WaitingOn, before.WaitingOn)
				}
				if !reflect.DeepEqual(got.Approval, before.Approval) {
					t.Fatalf("restored ask %s approval = %#v, want %#v", ask, got.Approval, before.Approval)
				}
				if count := askEventCount(t, f.database, ask, "ask.opened"); count != openedBefore[ask]+1 {
					t.Fatalf("ask %s has %d ask.opened events, want %d", ask, count, openedBefore[ask]+1)
				}
				if actor := latestAskEventActor(t, f.database, ask, "ask.opened"); actor.Kind != "user" || actor.ID != "alice" {
					t.Fatalf("restoration of %s credited to %#v, want the reopener", ask, actor)
				}
			}
			if got := askRowTexts(t, f.database, f.answeredAsk, f.retractedAsk); !reflect.DeepEqual(got, settledRows) {
				t.Fatalf("the reopen changed the answered and retracted asks:\n%v\nwant\n%v", got, settledRows)
			}
			if approval := readApproval(t, f.handler, f.spec).Approval; approval == nil || approval.State != "awaiting" {
				t.Fatalf("the spec's approval after the reopen = %#v, want awaiting", approval)
			}
			requireLiveText(t, f.handler, f.spec, `#close-decision state="open"`)
			requireLiveText(t, f.handler, f.copy, `#close-decision state="open" copied_from="`+f.blockAsk+`"`)
			if inbox := inboxAskIDs(t, f.handler); !containsAll(inbox, f.open()) {
				t.Fatalf("inbox after the reopen = %v, want every restored ask %v", inbox, f.open())
			}
			// The settlement the reopen's write left owed saves one more version of each document:
			// the reopen takes the decision's state off again, as a version keeps no state of an
			// open ask, and the copy keeps its source.
			if err := f.documents.SettleIssueDocuments(ctx, f.issue); err != nil {
				t.Fatalf("settle the reopened issue's documents: %v", err)
			}
			awaitSettled(t, f.database, documents...)
			for _, document := range documents {
				reopenedAt := requireStateVersionsPast(t, f.handler, f.database, document, closedAt[document], 1)
				stored := versionMarkdown(t, f.handler, document, reopenedAt)
				if closeDecisionState.MatchString(stored) {
					t.Fatalf("the version the reopened issue's settlement saved of %s = %s, want the decision's opener holding no state", document, stored)
				}
				if document == f.copy && !strings.Contains(stored, `copied_from="`+f.blockAsk+`"`) {
					t.Fatalf("the version the reopened issue's settlement saved of the copy = %s, want its source kept", stored)
				}
			}
		})
	}
}

// closeDecisionState finds the state the close-decision block's opener holds in a stored version.
var closeDecisionState = regexp.MustCompile(`#close-decision[^}\n]* state="([a-z]+)"`)

// requireDecisionsVersionEventAt fails unless the event at seq of the issue is the artifact.version
// of decisions, the issue's decisions query: the version a write's drain appends once the write
// that moved the query's record has committed.
func requireDecisionsVersionEventAt(t *testing.T, database *store.Store, issue string, seq int, decisions string) {
	t.Helper()
	var kind, artifact string
	if err := database.Pool.QueryRow(context.Background(),
		`select type, coalesce(payload->>'artifact_id', '') from events where issue_key = $1 and seq = $2`, issue, seq).Scan(&kind, &artifact); err != nil {
		t.Fatalf("read the event at %d of %s: %v", seq, issue, err)
	}
	if kind != "artifact.version" || artifact != decisions {
		t.Errorf("the event at %d of %s is a %s of %q, want the artifact.version of the decisions query %s", seq, issue, kind, artifact, decisions)
	}
}

// failingSettlement is a document service whose settlement of an issue's documents before its
// close fails, leaving the blocks written since the last settlement without an ask.
type failingSettlement struct {
	docs.API
}

func (failingSettlement) SettleIssueDocuments(context.Context, string) error {
	return errors.New("settlement failed")
}

// An ask block written a moment before the close, which its settlement (an hour away) has not
// indexed, is still withdrawn by the close: the close settles the issue's documents first, so the
// block is an ask, credited to whoever wrote it. When that settlement fails, the block is no ask
// to withdraw, and the asks member of the text an agent reads, which the decision card reads a
// block with no ask from, still shows it resolved on the closed issue: while another build serves,
// the close's sweep writes it resolved for that build's pages (docs.WriteIssueAskStates); on a
// fleet of one build the sweep writes nothing, the block holds no state, and the asks member reads
// it resolved from the issue's close (docs.TextWithAsks). The reopen's settlement opens it as its
// author's ask.
func TestClosingAnIssueWithdrawsADecisionBlockNotYetSettled(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name                       string
		settlementFails, twoBuilds bool
	}{
		{"settled by the close", false, false},
		{"settlement fails while another build serves", true, true},
		{"settlement fails on a fleet of one build", true, false},
	} {
		settlementFails := test.settlementFails
		t.Run(test.name, func(t *testing.T) {
			var documentService *docs.Service
			handler, database := newInteractionHandler(t, func(database *store.Store) docs.API {
				documentService = docs.New(docs.Deps{Store: database, Settle: time.Hour})
				t.Cleanup(func() { _ = documentService.Shutdown(context.Background()) })
				if settlementFails {
					return failingSettlement{API: documentService}
				}
				return documentService
			})
			if test.twoBuilds {
				registerTwoBuilds(t, database)
			}
			issue := createInteractionIssue(t, handler, "TEST", "A late decision", "Context\n")
			edited := sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
				"actor": sessionActor(),
				"ops": []map[string]string{{
					"op": "insert", "after": "end",
					"markdown": ":::ask{#late-decision urgency=\"high\" multiple=\"false\"}\nShip late?\n:::\n",
				}},
			})
			if edited.Code != http.StatusOK {
				t.Fatalf("write the ask block: status=%d body=%s", edited.Code, edited.Body.String())
			}
			if id := findBlockAsk(t, handler, issue.Key, issue.PrimaryArtifactID, "late-decision"); id != "" {
				t.Fatalf("the ask block is ask %s before any settlement", id)
			}
			closed := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue.Key, map[string]string{"status": "done"}, "alice")
			if closed.Code != http.StatusOK {
				t.Fatalf("close: status=%d body=%s", closed.Code, closed.Body.String())
			}
			requireLiveText(t, handler, issue.PrimaryArtifactID, `#late-decision state="resolved"`)
			id := findBlockAsk(t, handler, issue.Key, issue.PrimaryArtifactID, "late-decision")
			if !settlementFails {
				if id == "" {
					t.Fatal("the close's settlement never indexed the ask block")
				}
				ask := readBlockAsk(t, handler, id)
				if ask.Author.Kind != "session" || ask.Author.ID != sessionActor()["id"] {
					t.Fatalf("withdrawn decision's author = %#v, want the session that wrote it", ask.Author)
				}
				requireWithdrawn(t, handler, issue.Key, id, model.Actor{Kind: "user", ID: "alice", Proven: true})
				return
			}
			if id != "" {
				t.Fatalf("the ask block is ask %s although its settlement failed", id)
			}
			if line := askDirective(t, liveTree(t, documentService, issue.PrimaryArtifactID), "late-decision"); !test.twoBuilds && strings.Contains(line, "state=") {
				t.Fatalf("on a fleet of one build the closed issue's unindexed block holds %.300s, want no state in the document", line)
			}
			if reopened := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue.Key, map[string]string{"status": "todo"}, "alice"); reopened.Code != http.StatusOK {
				t.Fatalf("reopen: status=%d body=%s", reopened.Code, reopened.Body.String())
			}
			if err := documentService.SettleIssueDocuments(context.Background(), issue.Key); err != nil {
				t.Fatalf("settle the reopened issue's documents: %v", err)
			}
			id = findBlockAsk(t, handler, issue.Key, issue.PrimaryArtifactID, "late-decision")
			if id == "" {
				t.Fatal("the reopen's settlement never indexed the ask block")
			}
			ask := readBlockAsk(t, handler, id)
			if ask.State != "open" || ask.Author.Kind != "session" || ask.Author.ID != sessionActor()["id"] {
				t.Fatalf("indexed decision = %s by %#v, want open by the session that wrote it", ask.State, ask.Author)
			}
			requireLiveText(t, handler, issue.PrimaryArtifactID, `#late-decision state="open"`)
		})
	}
}

// While another build serves, the close's final sweep writes each copy of an answered ask with its
// state, its answerer and its time, and leaves the answer's words out (docs' answeredCopyWrite):
// the sweep's write is weighed by no size bound, since a document's growth check passes over
// server attributes, so words written there could take a document past what one upload may hold.
// The settlement the sweep leaves owed writes the words into each copy whose document has room for
// them, and leaves them out of one whose document has none, as it writes any copy. Here the close's
// own settlement fails, so the copies still show the ask open when the sweep reaches them: one in
// a short document, one in a document all but full.
func TestClosingAnIssueWritesAnAnsweredAsksCopiesWithoutItsWordsWhileAnotherBuildServes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	var documentService *docs.Service
	handler, database := newInteractionHandler(t, func(database *store.Store) docs.API {
		documentService = docs.New(docs.Deps{Store: database, Settle: time.Hour})
		t.Cleanup(func() { _ = documentService.Shutdown(context.Background()) })
		return failingSettlement{API: documentService}
	})
	registerTwoBuilds(t, database)
	issue := createInteractionIssue(t, handler, "TEST", "Copies of a long answer", closeDecision)
	settle := func(what string) {
		t.Helper()
		if err := documentService.SettleIssueDocuments(ctx, issue.Key); err != nil {
			t.Fatalf("settle %s: %v", what, err)
		}
	}
	settle("the spec")
	source := requireBlockAsk(t, handler, issue.Key, issue.PrimaryArtifactID, "close-decision")
	block := strings.TrimPrefix(closeDecision, "Context\n\n")
	upload := func(name, content string) string {
		t.Helper()
		uploaded := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]any{
			"actor": sessionActor(), "name": name, "content": content,
		})
		if uploaded.Code != http.StatusCreated {
			t.Fatalf("upload %s: status=%d body=%.300s", name, uploaded.Code, uploaded.Body.String())
		}
		return decodeBody[struct {
			Artifact model.Artifact `json:"artifact"`
		}](t, uploaded).Artifact.ID
	}
	roomy := upload("roomy.md", "Notes\n\n"+block)
	// All but full: the prose leaves the document room for the block's attributes and not for an
	// answer at the ask bound.
	crowded := upload("crowded.md", strings.Repeat("word ", (pmdoc.MaxDocumentBytes-answerRoomHeadroom)/len("word "))+"\n\n"+block)
	settle("the copies")
	awaitSettled(t, database, roomy, crowded)
	for _, document := range []string{roomy, crowded} {
		if line := askDirective(t, liveTree(t, documentService, document), "close-decision"); !strings.Contains(line, `state="open" copied_from="`+source+`"`) {
			t.Fatalf("the copy on %s reads %.300s before the answer, want it open, naming its source", document, line)
		}
	}

	words := boundedAnswer()
	answered := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+source+"/answer", map[string]any{
		"selected": []string{"Yes"}, "text": words, "expected_edited_at": nil,
	}, "alice")
	if answered.Code != http.StatusOK {
		t.Fatalf("answer the source: status=%d body=%.300s", answered.Code, answered.Body.String())
	}
	if closed := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue.Key, map[string]string{"status": "done"}, "alice"); closed.Code != http.StatusOK {
		t.Fatalf("close: status=%d body=%s", closed.Code, closed.Body.String())
	}
	for _, document := range []string{roomy, crowded} {
		line := askDirective(t, liveTree(t, documentService, document), "close-decision")
		if !strings.Contains(line, `state="answered" answered_by="alice" answered_at="`) || strings.Contains(line, "answer=") || strings.Contains(line, "selected=") {
			t.Fatalf("the copy on %s reads %.300s after the close's sweep, want who answered and when, without the options chosen or the words", document, line)
		}
	}

	settle("the copies the sweep left owed")
	awaitSettled(t, database, roomy, crowded)
	if line := askDirective(t, liveTree(t, documentService, roomy), "close-decision"); !strings.Contains(line, `answer="`+words) || !strings.Contains(line, `selected="[&#x22;Yes&#x22;]"`) {
		t.Fatalf("the copy in the short document reads %.300s after its settlement, want the answer's words and choice", line)
	}
	text := liveTree(t, documentService, crowded)
	if line := askDirective(t, text, "close-decision"); !strings.Contains(line, `state="answered" answered_by="alice" answered_at="`) || strings.Contains(line, "answer=") || strings.Contains(line, "selected=") {
		t.Fatalf("the copy in the full document reads %.300s after its settlement, want who answered and when, without the words it has no room for", line)
	}
	if len(text) > pmdoc.MaxDocumentBytes {
		t.Fatalf("the full document is %d bytes after its settlement, past the %d one upload may hold", len(text), pmdoc.MaxDocumentBytes)
	}
}

// pastedCloseDecision is the spec's ask block as a browser's paste copies it, with the id
// and the state the block it copies carries.
const pastedCloseDecision = ":::ask{#close-decision urgency=\"med\" multiple=\"false\" state=\"open\"}\nShip it?\n\n- Yes: Now.\n- No: Later.\n:::\n"

// An ask block a browser pasted a moment before the close repeats the id of the block it
// copies, so it is no ask until a settlement stamps it an id of its own. The close settles the
// issue's documents first, so the pasted block is an ask under a new id, withdrawn with the block
// it copies, each with a record naming the closer. When that settlement fails, the pasted block is
// no ask to withdraw, and a write addressed by its id lands on the block it copies; the close's
// sweep still leaves it reading resolved beside that block, and the reopen's settlement stamps it
// and opens it as a new ask, beside the restored one. The blocks read their state from their
// attributes, which the close's sweep writes only while another build serves
// (docs.WriteIssueAskStates), so the test registers a process of each of two builds.
func TestClosingAnIssueWithdrawsADecisionBlockPastedUnderARepeatedID(t *testing.T) {
	t.Parallel()
	for _, settlementFails := range []bool{false, true} {
		name := "settled by the close"
		if settlementFails {
			name = "settlement fails"
		}
		t.Run(name, func(t *testing.T) {
			var documentService *docs.Service
			handler, database := newInteractionHandler(t, func(database *store.Store) docs.API {
				documentService = docs.New(docs.Deps{Store: database, Identity: headerIdentity(database), Settle: time.Hour})
				t.Cleanup(func() { _ = documentService.Shutdown(context.Background()) })
				if settlementFails {
					return failingSettlement{API: documentService}
				}
				return documentService
			})
			registerTwoBuilds(t, database)
			issue := createInteractionIssue(t, handler, "TEST", "A pasted decision", closeDecision)
			if err := documentService.SettleIssueDocuments(context.Background(), issue.Key); err != nil {
				t.Fatalf("settle the spec: %v", err)
			}
			awaitSettled(t, database, issue.PrimaryArtifactID)
			original := requireBlockAsk(t, handler, issue.Key, issue.PrimaryArtifactID, "close-decision")

			pasted, err := pmdoc.Parse(pastedCloseDecision)
			if err != nil {
				t.Fatal(err)
			}
			peer := connectBrowserPeer(t, handler, documentService, issue.PrimaryArtifactID)
			peer.edit(t, func(tree *pmdoc.Node) error {
				tree.Children = append(tree.Children, pasted.Children...)
				return nil
			})
			peer.barrier(t)
			if live := liveTree(t, documentService, issue.PrimaryArtifactID); strings.Count(live, `#close-decision urgency="med" multiple="false" state="open"`) != 2 {
				t.Fatalf("the document does not hold the pasted copy under the repeated id:\n%s", live)
			}
			if asks := documentBlockAsks(t, handler, issue.Key, issue.PrimaryArtifactID); len(asks) != 1 {
				t.Fatalf("the document holds %d asks before any settlement of the paste, want the spec's alone", len(asks))
			}

			closed := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue.Key, map[string]string{"status": "done"}, "alice")
			if closed.Code != http.StatusOK {
				t.Fatalf("close: status=%d body=%s", closed.Code, closed.Body.String())
			}
			closer := model.Actor{Kind: "user", ID: "alice", Proven: true}
			requireWithdrawn(t, handler, issue.Key, original, closer)
			live := liveTree(t, documentService, issue.PrimaryArtifactID)
			if strings.Contains(live, `state="open"`) {
				t.Fatalf("a block of the closed issue's live tree still reads open:\n%s", live)
			}
			if !settlementFails {
				pastedAsk := pastedBlockAsk(t, handler, issue.Key, issue.PrimaryArtifactID)
				requireWithdrawn(t, handler, issue.Key, pastedAsk.ID, closer)
				if strings.Count(live, `state="resolved"`) != 2 || !strings.Contains(live, "#"+*pastedAsk.BlockID+` urgency="med" multiple="false" state="resolved"`) {
					t.Fatalf("live tree:\n%s\nwant the spec's block and the pasted block %s resolved", live, *pastedAsk.BlockID)
				}
				return
			}
			if strings.Count(live, `#close-decision urgency="med" multiple="false" state="resolved"`) != 2 {
				t.Fatalf("live tree:\n%s\nwant both blocks under close-decision resolved", live)
			}
			if asks := documentBlockAsks(t, handler, issue.Key, issue.PrimaryArtifactID); len(asks) != 1 {
				t.Fatalf("the document holds %d asks although the paste's settlement failed, want the spec's alone", len(asks))
			}

			if reopened := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue.Key, map[string]string{"status": "todo"}, "alice"); reopened.Code != http.StatusOK {
				t.Fatalf("reopen: status=%d body=%s", reopened.Code, reopened.Body.String())
			}
			if err := documentService.SettleIssueDocuments(context.Background(), issue.Key); err != nil {
				t.Fatalf("settle the reopened issue's documents: %v", err)
			}
			if restored := readBlockAsk(t, handler, original); restored.State != "open" || restored.Resolution != nil {
				t.Fatalf("the spec's ask after the reopen = %s %#v, want open", restored.State, restored.Resolution)
			}
			pastedAsk := pastedBlockAsk(t, handler, issue.Key, issue.PrimaryArtifactID)
			if pastedAsk.State != "open" {
				t.Fatalf("the pasted block's ask after the reopen = %s, want open", pastedAsk.State)
			}
			requireLiveText(t, handler, issue.PrimaryArtifactID,
				`#close-decision state="open"`,
				"#"+*pastedAsk.BlockID+` state="open"`,
			)
		})
	}
}

// documentBlockAsks is every ask, in any state, indexed for a block of document.
func documentBlockAsks(t *testing.T, handler http.Handler, issue, document string) []model.Ask {
	t.Helper()
	listed := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue+"/asks?state=all", nil, "alice")
	if listed.Code != http.StatusOK {
		t.Fatalf("list asks: status=%d body=%s", listed.Code, listed.Body.String())
	}
	var asks []model.Ask
	for _, ask := range decodeBody[[]model.Ask](t, listed) {
		if ask.BlockID != nil && ask.BlockArtifact != nil && ask.BlockArtifact.ID == document {
			asks = append(asks, ask)
		}
	}
	return asks
}

// pastedBlockAsk is the one ask of document indexed for a block other than close-decision: the
// pasted copy of the spec's ask block, under the id a settlement stamped it.
func pastedBlockAsk(t *testing.T, handler http.Handler, issue, document string) model.Ask {
	t.Helper()
	var found []model.Ask
	for _, ask := range documentBlockAsks(t, handler, issue, document) {
		if *ask.BlockID != "close-decision" {
			found = append(found, ask)
		}
	}
	if len(found) != 1 || found[0].Question != "Ship it?" {
		t.Fatalf("asks for the pasted block = %#v, want one asking %q under an id of its own", found, "Ship it?")
	}
	return found[0]
}

// otherProcessHandler mounts the API stack of a second envoy-dispatch process on database, as
// secondProcessHandler does, with a document service whose settle delay is an hour: nothing the
// second process writes is settled there while a test runs.
func otherProcessHandler(t *testing.T, database *store.Store) http.Handler {
	t.Helper()
	second := storetest.SecondProcess(t, database)
	broker := events.NewBroker()
	documentService := docs.New(docs.Deps{Store: second, Events: broker, ServerURL: "https://dispatch.example", Settle: time.Hour})
	t.Cleanup(func() { _ = documentService.Shutdown(context.Background()) })
	deps, err := NewDeps(DepsInput{
		Store:       second,
		Identity:    headerIdentity(second),
		AgentTokens: sharedAgentTokens(t, "agent-token"),
		ServerURL:   "https://dispatch.example",
		Docs:        documentService,
		Events:      broker,
	})
	if err != nil {
		t.Fatalf("new API dependencies for the other process: %v", err)
	}
	mux := http.NewServeMux()
	t.Cleanup(Register(mux, t.Context(), deps).Wait)
	return mux
}

// An ask block written a moment before the close is withdrawn by it, with a record naming the
// closer, even when the agent's next edit landed on another envoy-dispatch process and left this
// process's room behind the document's durable head (no room sync runs here, so the room hears of
// nothing). A read is never older than the stored head all the same, and the close's settlement
// catches the room up under the document's lock and settles, so the block is an ask, credited to
// the session that wrote it, before the close withdraws it. Settled once and given up on, the
// block would only be marked resolved by the close's sweep, with no ask and no record of who
// closed it.
func TestClosingAnIssueWithdrawsADecisionBlockWhoseRoomAnotherProcessLeftBehind(t *testing.T) {
	t.Parallel()
	handler, database := newInteractionHandler(t, func(database *store.Store) docs.API {
		documentService := docs.New(docs.Deps{Store: database, Settle: time.Hour})
		t.Cleanup(func() { _ = documentService.Shutdown(context.Background()) })
		return documentService
	})
	other := otherProcessHandler(t, database)
	issue := createInteractionIssue(t, handler, "TEST", "A decision on two processes", "Context\n")
	insert := func(handler http.Handler, markdown string) {
		t.Helper()
		edited := sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
			"actor": sessionActor(),
			"ops":   []map[string]string{{"op": "insert", "after": "end", "markdown": markdown}},
		})
		if edited.Code != http.StatusOK {
			t.Fatalf("edit the document: status=%d body=%s", edited.Code, edited.Body.String())
		}
	}
	insert(handler, ":::ask{#late-decision urgency=\"high\" multiple=\"false\"}\nShip late?\n:::\n")
	insert(other, "Written on the other process.\n")
	if live, _ := documentTexts(t, handler, issue.PrimaryArtifactID); !strings.Contains(live, "Written on the other process.") {
		t.Fatalf("this process's read of the document is older than the other process's stored edit:\n%s", live)
	}
	if id := findBlockAsk(t, handler, issue.Key, issue.PrimaryArtifactID, "late-decision"); id != "" {
		t.Fatalf("the ask block is ask %s before any settlement", id)
	}

	closed := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue.Key, map[string]string{"status": "done"}, "alice")
	if closed.Code != http.StatusOK {
		t.Fatalf("close: status=%d body=%s", closed.Code, closed.Body.String())
	}
	id := findBlockAsk(t, handler, issue.Key, issue.PrimaryArtifactID, "late-decision")
	if id == "" {
		t.Fatal("the close's settlement never indexed the ask block")
	}
	ask := readBlockAsk(t, handler, id)
	if ask.Author.Kind != "session" || ask.Author.ID != sessionActor()["id"] {
		t.Fatalf("withdrawn decision's author = %#v, want the session that wrote it", ask.Author)
	}
	requireWithdrawn(t, handler, issue.Key, id, model.Actor{Kind: "user", ID: "alice", Proven: true})
	requireLiveText(t, handler, issue.PrimaryArtifactID,
		"Written on the other process.",
		`#late-decision state="resolved"`,
	)
}

// peerInsertDocs runs insert once, right after the close's transaction has written the issue's
// documents and before it commits: the window a browser connected to a document's room can still
// write in.
type peerInsertDocs struct {
	docs.API
	insert func()
	once   sync.Once
}

func (d *peerInsertDocs) WriteIssueAskStates(ctx context.Context, issueKey string, asks []model.Ask, actor model.Actor, markUnindexed bool) ([]docs.BlockRef, error) {
	written, err := d.API.WriteIssueAskStates(ctx, issueKey, asks, actor, markUnindexed)
	if err == nil && !markUnindexed && d.insert != nil {
		d.once.Do(d.insert)
	}
	return written, err
}

// A browser writing an ask block into a document after the close's transaction forked it and
// before it committed leaves the block in the room, where it gets no ask on the closed issue: the
// close's sweep, once the rooms are closed, leaves it reading resolved, and the settlement that
// then runs on the closed issue saves it so, still with no ask (reconcileAskBlocks). The sweep
// writes that state only while another build serves (docs.WriteIssueAskStates), so the test
// registers a process of each of two builds.
func TestABrowsersDecisionBlockWrittenDuringTheCloseReadsResolved(t *testing.T) {
	t.Parallel()
	var documentService *docs.Service
	wrapper := &peerInsertDocs{}
	handler, database := newInteractionHandler(t, func(database *store.Store) docs.API {
		documentService = docs.New(docs.Deps{Store: database, Identity: headerIdentity(database), Settle: 20 * time.Millisecond})
		t.Cleanup(func() { _ = documentService.Shutdown(context.Background()) })
		wrapper.API = documentService
		return wrapper
	})
	registerTwoBuilds(t, database)
	issue := createInteractionIssue(t, handler, "TEST", "A browser writes during the close", closeDecision)
	awaitIndexedAskBlock(t, handler, issue.PrimaryArtifactID, "close-decision", "Ship it?")
	awaitSettled(t, database, issue.PrimaryArtifactID)
	peer := connectBrowserPeer(t, handler, documentService, issue.PrimaryArtifactID)
	wrapper.insert = func() {
		peer.appendParagraph(t, ":::ask{#peer-decision urgency=\"low\" multiple=\"false\"}\nWritten during the close?\n:::\n")
		peer.barrier(t)
	}
	closed := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue.Key, map[string]string{"status": "done"}, "alice")
	if closed.Code != http.StatusOK {
		t.Fatalf("close: status=%d body=%s", closed.Code, closed.Body.String())
	}
	requireLiveText(t, handler, issue.PrimaryArtifactID,
		`#close-decision state="resolved"`,
		`#peer-decision state="resolved"`,
	)
	awaitSettled(t, database, issue.PrimaryArtifactID)
	if id := findBlockAsk(t, handler, issue.Key, issue.PrimaryArtifactID, "peer-decision"); id != "" {
		t.Fatalf("the browser's block is ask %s on a closed issue", id)
	}
	if _, version := documentTexts(t, handler, issue.PrimaryArtifactID); !contains(
		`#close-decision urgency="med" multiple="false" state="resolved"`,
		`#peer-decision urgency="low" multiple="false" state="resolved"`,
	)(version) {
		t.Fatalf("the closed issue's settlement left the newest version reading:\n%s\nwant both ask blocks resolved", version)
	}
}

// Every close withdraws the asks open at that close, and every reopen restores them, once each.
func TestEachCloseAndReopenWithdrawsAndRestoresOnce(t *testing.T) {
	t.Parallel()
	handler, database := newInteractionHandler(t, func(database *store.Store) docs.API {
		service := docs.New(docs.Deps{Store: database, Settle: 20 * time.Millisecond})
		t.Cleanup(func() { _ = service.Shutdown(context.Background()) })
		return service
	})
	issue := createInteractionIssue(t, handler, "TEST", "Closed and reopened twice", closeDecision)
	awaitIndexedAskBlock(t, handler, issue.PrimaryArtifactID, "close-decision", "Ship it?")
	asks := []string{requireBlockAsk(t, handler, issue.Key, issue.PrimaryArtifactID, "close-decision"), createSessionAsk(t, handler, issue.Key, "Which region?")}
	opened := map[string]int{}
	for _, ask := range asks {
		opened[ask] = askEventCount(t, database, ask, "ask.opened")
	}
	for cycle := 1; cycle <= 2; cycle++ {
		closer := closers[cycle]
		if closed := closer.send(t, handler, issue.Key, map[string]any{"status": "done"}); closed.Code != http.StatusOK {
			t.Fatalf("close %d: status=%d body=%s", cycle, closed.Code, closed.Body.String())
		}
		closedBy := latestEventActor(t, database, issue.Key, "issue.closed")
		for _, ask := range asks {
			requireWithdrawn(t, handler, issue.Key, ask, closedBy)
			if count := askEventCount(t, database, ask, "ask.resolved"); count != cycle {
				t.Fatalf("after close %d ask %s has %d ask.resolved events, want %d", cycle, ask, count, cycle)
			}
		}
		requireLiveText(t, handler, issue.PrimaryArtifactID, `#close-decision state="resolved"`)
		if reopened := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue.Key, map[string]string{"status": "in_progress"}, "alice"); reopened.Code != http.StatusOK {
			t.Fatalf("reopen %d: status=%d body=%s", cycle, reopened.Code, reopened.Body.String())
		}
		for _, ask := range asks {
			if got := readBlockAsk(t, handler, ask); got.State != "open" || got.Resolution != nil {
				t.Fatalf("after reopen %d ask %s = %s %#v, want open", cycle, ask, got.State, got.Resolution)
			}
			if count := askEventCount(t, database, ask, "ask.opened"); count != opened[ask]+cycle {
				t.Fatalf("after reopen %d ask %s has %d ask.opened events, want %d", cycle, ask, count, opened[ask]+cycle)
			}
		}
		requireLiveText(t, handler, issue.PrimaryArtifactID, `#close-decision state="open"`)
	}
}

// A retired document takes no new text (docs.RetireDocument), so the close and the reopen write
// their asks' states into every document of the issue but a retired one (docs'
// issueDocumentIDs): a retired document refuses every live write, under its owner's lock
// (joinLiveWrite), and a write into it would refuse the close or the reopen with it. An issue born
// converted, holding an ask block in its spec and a plain ask, has a document retired - a fragment
// of the issue's room, and a document that is its own room - and still closes, withdrawing both
// asks, with the spec's live tree showing the block resolved, and reopens, restoring both, with the
// block holding no state again.
func TestClosingAnIssueWithARetiredDocumentWithdrawsItsAsksAndReopeningRestoresThem(t *testing.T) {
	for _, test := range []struct {
		name, document string
		inRoom         bool
	}{
		{"a document of the issue's room", "verification.md", true},
		{"a document that is its own room", "notes.md", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			var documents *docs.Service
			handler, database := newInteractionHandler(t, func(database *store.Store) docs.API {
				documents = docs.New(docs.Deps{Store: database, Settle: 20 * time.Millisecond})
				t.Cleanup(func() { _ = documents.Shutdown(context.Background()) })
				return documents
			})
			issue := createInteractionIssue(t, handler, "TEST", "Closing beside a retired document", closeDecision)
			awaitIndexedAskBlock(t, handler, issue.PrimaryArtifactID, "close-decision", "Ship it?")
			asks := []string{requireBlockAsk(t, handler, issue.Key, issue.PrimaryArtifactID, "close-decision"), createSessionAsk(t, handler, issue.Key, "Which region?")}
			retired := uploadIssueDocument(t, handler, issue.Key, test.document, "Kept as it stood.\n")
			var specRoom, retiredRoom *string
			if err := database.Pool.QueryRow(ctx, `
				select (select room::text from document_rooms where artifact_id = $1), (select room::text from document_rooms where artifact_id = $2)`,
				issue.PrimaryArtifactID, retired.ID).Scan(&specRoom, &retiredRoom); err != nil {
				t.Fatalf("read the rooms of the spec and of %s: %v", test.document, err)
			}
			if specRoom == nil {
				t.Fatalf("the spec %s holds no document_rooms row, want the issue born converted", issue.PrimaryArtifactID)
			}
			if inRoom := retiredRoom != nil && *retiredRoom == *specRoom; inRoom != test.inRoom {
				t.Fatalf("%s is edited in room %v, the spec in %s: a fragment of the issue's room is %t, want %t",
					test.document, retiredRoom, *specRoom, inRoom, test.inRoom)
			}
			retireTestDocument(t, database, retired.ID)

			closed := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue.Key, map[string]string{"status": "done"}, "alice")
			if closed.Code != http.StatusOK {
				t.Fatalf("close beside the retired %s: status=%d body=%s", test.document, closed.Code, closed.Body.String())
			}
			closedBy := latestEventActor(t, database, issue.Key, "issue.closed")
			for _, ask := range asks {
				requireWithdrawn(t, handler, issue.Key, ask, closedBy)
			}
			requireLiveText(t, handler, issue.PrimaryArtifactID, `#close-decision state="resolved"`)
			requireLiveTree(t, documents, issue.PrimaryArtifactID, `#close-decision urgency="med" multiple="false" state="resolved"`)

			reopened := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue.Key, map[string]string{"status": "todo"}, "alice")
			if reopened.Code != http.StatusOK {
				t.Fatalf("reopen beside the retired %s: status=%d body=%s", test.document, reopened.Code, reopened.Body.String())
			}
			for _, ask := range asks {
				if got := readBlockAsk(t, handler, ask); got.State != "open" || got.Resolution != nil {
					t.Fatalf("ask %s after the reopen = %s %#v, want open with no record", ask, got.State, got.Resolution)
				}
			}
			requireLiveText(t, handler, issue.PrimaryArtifactID, `#close-decision state="open"`)
			requireLiveTree(t, documents, issue.PrimaryArtifactID, `#close-decision urgency="med" multiple="false"}`)
		})
	}
}

// requireWithdrawn fails t unless ask reads as withdrawn by the close of issue, made by closedBy.
func requireWithdrawn(t *testing.T, handler http.Handler, issue, ask string, closedBy model.Actor) {
	t.Helper()
	got := readBlockAsk(t, handler, ask)
	if got.State != "resolved" || got.Resolution == nil {
		t.Fatalf("ask %s = %s %#v, want resolved", ask, got.State, got.Resolution)
	}
	if got.Resolution.Kind != "withdrawn" || got.Resolution.Reason != "Withdrawn by the close of "+issue+"." {
		t.Fatalf("ask %s resolution = %#v, want withdrawn by the close of %s", ask, got.Resolution, issue)
	}
	if !reflect.DeepEqual(got.Resolution.Actor, closedBy) {
		t.Fatalf("ask %s withdrawn by %#v, want the closer %#v", ask, got.Resolution.Actor, closedBy)
	}
	if got.Resolution.At.IsZero() {
		t.Fatalf("ask %s resolution has no time", ask)
	}
}

func createSessionAsk(t *testing.T, handler http.Handler, issue, question string) string {
	t.Helper()
	created := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue+"/asks", map[string]any{
		"question": question, "actor": sessionActor(),
	})
	if created.Code != http.StatusCreated {
		t.Fatalf("create ask %q: status=%d body=%s", question, created.Code, created.Body.String())
	}
	return decodeBody[model.Ask](t, created).ID
}

// findBlockAsk is the id of the ask indexed for blockID on document, "" when none is.
func findBlockAsk(t *testing.T, handler http.Handler, issue, document, blockID string) string {
	t.Helper()
	listed := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue+"/asks?state=all", nil, "alice")
	if listed.Code != http.StatusOK {
		t.Fatalf("list asks: status=%d body=%s", listed.Code, listed.Body.String())
	}
	for _, ask := range decodeBody[[]model.Ask](t, listed) {
		if ask.BlockID != nil && *ask.BlockID == blockID && ask.BlockArtifact != nil && ask.BlockArtifact.ID == document {
			return ask.ID
		}
	}
	return ""
}

func requireBlockAsk(t *testing.T, handler http.Handler, issue, document, blockID string) string {
	t.Helper()
	id := findBlockAsk(t, handler, issue, document, blockID)
	if id == "" {
		t.Fatalf("no ask for block %s on %s", blockID, document)
	}
	return id
}

type issueDetailRead struct {
	LastSeq  int `json:"last_seq"`
	OpenAsks []struct {
		ID string `json:"id"`
	} `json:"open_asks"`
}

func readIssueDetail(t *testing.T, handler http.Handler, issue string) issueDetailRead {
	t.Helper()
	read := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue, nil, "alice")
	if read.Code != http.StatusOK {
		t.Fatalf("read issue: status=%d body=%s", read.Code, read.Body.String())
	}
	return decodeBody[issueDetailRead](t, read)
}

func openAskIDs(detail issueDetailRead) []string {
	ids := make([]string, 0, len(detail.OpenAsks))
	for _, ask := range detail.OpenAsks {
		ids = append(ids, ask.ID)
	}
	return sorted(ids)
}

func sorted(values []string) []string {
	values = slices.Clone(values)
	slices.Sort(values)
	return values
}

func containsAll(have, want []string) bool {
	for _, value := range want {
		if !slices.Contains(have, value) {
			return false
		}
	}
	return true
}

func inboxAskIDs(t *testing.T, handler http.Handler) []string {
	t.Helper()
	inbox := dispatchRequest(t, handler, http.MethodGet, "/api/v1/inbox", nil, "alice")
	if inbox.Code != http.StatusOK {
		t.Fatalf("read inbox: status=%d body=%s", inbox.Code, inbox.Body.String())
	}
	var ids []string
	for _, ask := range decodeBody[[]model.Ask](t, inbox) {
		ids = append(ids, ask.ID)
	}
	return ids
}

// requireLiveText fails t unless document's live text as an agent reads it (agentText: its
// markdown, which holds no ask's state, and one line per entry of its asks member, which holds
// each ask block's state and answer, read from the store) holds every one of parts.
func requireLiveText(t *testing.T, handler http.Handler, document string, parts ...string) {
	t.Helper()
	if live := agentText(t, handler, document); !contains(parts...)(live) {
		t.Fatalf("document %s live text:\n%s\nwant it to hold %q", document, live, parts)
	}
}

// liveTree is document's live tree as documents renders it, with each block's attributes as the
// writes to the tree left them: the attributes the release before's pages read an ask's state
// from, which settlement and the close's sweep keep in step with the asks rows only while another
// build serves (docs' rollWindowAskAttributes). The text an agent reads takes them off and answers
// the state in its asks member instead (requireLiveText), so it cannot tell whether a write put it
// on the block.
func liveTree(t *testing.T, documents *docs.Service, document string) string {
	t.Helper()
	markdown, _, err := documents.TextWithToken(context.Background(), document)
	if err != nil {
		t.Fatalf("read the live tree of %s: %v", document, err)
	}
	return markdown
}

// requireLiveTree fails t unless document's live tree (liveTree) holds every one of parts.
func requireLiveTree(t *testing.T, documents *docs.Service, document string, parts ...string) {
	t.Helper()
	if live := liveTree(t, documents, document); !contains(parts...)(live) {
		t.Fatalf("document %s live tree:\n%s\nwant it to hold %q", document, live, parts)
	}
}

// awaitSettled waits until none of documents owes a settlement: until no pending-settlement row
// stands for the room any of them is edited in, the key the settlement that clears it deletes
// (refs.DocumentRoomSQL). Its one bound is the test binary's deadline (testwait.UntilPolling): a
// settlement a loaded machine starts late still ends the wait, and one that never runs fails it by
// name, with the count still owed.
func awaitSettled(t *testing.T, database *store.Store, documents ...string) {
	t.Helper()
	testwait.UntilPolling(t, 20*time.Millisecond, fmt.Sprintf("documents %v to owe no settlement (the count of their pending settlements)", documents), func() (int, bool) {
		var pending int
		if err := database.Pool.QueryRow(context.Background(), `
			select count(*) from doc_settlements_pending
			where artifact_id in (select `+refs.DocumentRoomSQL("owed.id")+` from unnest($1::uuid[]) as owed (id))
		`, documents).Scan(&pending); err != nil {
			t.Fatalf("read pending settlements: %v", err)
		}
		return pending, pending == 0
	})
}

// askRowTexts is each ask's whole row as the database stores it.
func askRowTexts(t *testing.T, database *store.Store, asks ...string) []string {
	t.Helper()
	rows := make([]string, 0, len(asks))
	for _, ask := range asks {
		var row string
		if err := database.Pool.QueryRow(context.Background(), `select to_jsonb(a)::text from asks a where id = $1`, ask).Scan(&row); err != nil {
			t.Fatalf("read ask row %s: %v", ask, err)
		}
		rows = append(rows, row)
	}
	return rows
}

// versionHashes is every stored version of documents, each with its markdown's hash.
func versionHashes(t *testing.T, database *store.Store, documents ...string) []string {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `
		select artifact_id::text || ' v' || number || ' ' || md5(coalesce(markdown, ''))
		from artifact_versions where artifact_id = any($1::uuid[]) order by artifact_id, number
	`, documents)
	if err != nil {
		t.Fatalf("read versions: %v", err)
	}
	defer rows.Close()
	var hashes []string
	for rows.Next() {
		var hash string
		if err := rows.Scan(&hash); err != nil {
			t.Fatalf("read version: %v", err)
		}
		hashes = append(hashes, hash)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read versions: %v", err)
	}
	if len(hashes) == 0 {
		t.Fatalf("documents %v have no versions", documents)
	}
	return hashes
}

func reviewCount(t *testing.T, database *store.Store, documents ...string) int {
	t.Helper()
	var count int
	if err := database.Pool.QueryRow(context.Background(), `
		select count(*) from artifact_reviews where artifact_id = any($1::uuid[])
	`, documents).Scan(&count); err != nil {
		t.Fatalf("count reviews: %v", err)
	}
	return count
}

// decisionEventCount counts issue's events that record an answer or a review.
func decisionEventCount(t *testing.T, database *store.Store, issue string) int {
	t.Helper()
	var count int
	if err := database.Pool.QueryRow(context.Background(), `
		select count(*) from events
		where issue_key = $1 and type in ('ask.answered', 'artifact.approved', 'artifact.changes_requested')
	`, issue).Scan(&count); err != nil {
		t.Fatalf("count decision events: %v", err)
	}
	return count
}

func askEventCount(t *testing.T, database *store.Store, ask, eventType string) int {
	t.Helper()
	var count int
	if err := database.Pool.QueryRow(context.Background(), `
		select count(*) from events where type = $2 and payload->>'id' = $1
	`, ask, eventType).Scan(&count); err != nil {
		t.Fatalf("count %s events of %s: %v", eventType, ask, err)
	}
	return count
}

func latestEventActor(t *testing.T, database *store.Store, issue, eventType string) model.Actor {
	t.Helper()
	return scanEventActor(t, database, `
		select actor from events where issue_key = $1 and type = $2 order by seq desc limit 1
	`, issue, eventType)
}

func latestAskEventActor(t *testing.T, database *store.Store, ask, eventType string) model.Actor {
	t.Helper()
	return scanEventActor(t, database, `
		select actor from events where payload->>'id' = $1 and type = $2 order by id desc limit 1
	`, ask, eventType)
}

func scanEventActor(t *testing.T, database *store.Store, query string, args ...any) model.Actor {
	t.Helper()
	var raw []byte
	if err := database.Pool.QueryRow(context.Background(), query, args...).Scan(&raw); err != nil {
		t.Fatalf("read event actor: %v", err)
	}
	var actor model.Actor
	if err := json.Unmarshal(raw, &actor); err != nil {
		t.Fatalf("decode event actor: %v", err)
	}
	return actor
}

// countedAskStateWrites is the document service with its sweep writes counted: every call of
// WriteIssueAskStates, which is the one write a close's sweep makes (sweepIssueAskBlocksTx).
type countedAskStateWrites struct {
	docs.API
	writes atomic.Int32
}

func (c *countedAskStateWrites) WriteIssueAskStates(ctx context.Context, issueKey string, asks []model.Ask, actor model.Actor, markUnindexed bool) ([]docs.BlockRef, error) {
	c.writes.Add(1)
	return c.API.WriteIssueAskStates(ctx, issueKey, asks, actor, markUnindexed)
}

// A close's sweep decides whether the issue is still closed under the issue's owner lock
// (sweepClosedIssueAsksTx): a reopen committed while the sweep waited for that lock leaves the
// reopened issue's documents alone. The test holds the lock as the reopen does and commits the
// reopen while the sweep waits; a sweep that read the issue before the lock would write the
// closed issue's ask states into the documents of an issue that is open again.
func TestACloseSweepThatWaitedOutAReopenWritesNothing(t *testing.T) {
	t.Parallel()
	handler, database, _, served := newRegisteredTestServer(t, testServerOptions{settle: time.Hour})
	issue := createInteractionIssue(t, handler, "TEST", "Swept after a reopen", "Context\n")
	ctx := context.Background()
	if _, err := database.Pool.Exec(ctx, `update issues set status = 'done', closed_at = now() where key = $1`, issue.Key); err != nil {
		t.Fatalf("close the issue: %v", err)
	}
	counted := &countedAskStateWrites{API: served.deps.Docs}
	served.deps.Docs = counted

	holder, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the reopen: %v", err)
	}
	defer func() { _ = holder.Rollback(ctx) }()
	if err := store.LockOwner(ctx, holder, store.IssueOwner(issue.Key)); err != nil {
		t.Fatalf("take the issue's owner lock: %v", err)
	}
	swept := make(chan error, 1)
	go func() {
		swept <- served.sweepClosedIssueAsksTx(ctx, issue.Key, nil, model.Actor{Kind: "user", ID: "alice"})
	}()
	if err, early, _ := storetest.AwaitOwnerLockWaiter(t, database, "the close's sweep", swept); early {
		t.Fatalf("the sweep answered %v while this test held the issue's owner lock: it decided on the issue "+
			"under no lock", err)
	}
	if _, err := holder.Exec(ctx, `update issues set status = 'todo', closed_at = null where key = $1`, issue.Key); err != nil {
		t.Fatalf("reopen the issue: %v", err)
	}
	if err := holder.Commit(ctx); err != nil {
		t.Fatalf("commit the reopen: %v", err)
	}
	if err := testwait.Receive(t, "the sweep's answer once the reopen committed", swept); err != nil {
		t.Fatalf("the sweep: %v", err)
	}
	if writes := counted.writes.Load(); writes != 0 {
		t.Fatalf("the sweep wrote the ask states of %s %d time(s) after a reopen committed while it waited: it "+
			"read the issue as closed before the issue's owner lock", issue.Key, writes)
	}
}
