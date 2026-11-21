package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// answerSurvivesQuestion is the question a person answers, and answerSurvivesRemovedQuestion the
// question the upload of the second case removes on purpose.
const (
	answerSurvivesQuestion        = "Which transport should we expose?"
	answerSurvivesRemovedQuestion = "When do we roll out?"
)

// answerSurvivesSpec is the document the agent opens the issue with: the question a person
// answers, under the block id the document gives it, and a second question beside it.
const answerSurvivesSpec = "Context\n\n" +
	":::ask{#transport urgency=\"med\" multiple=\"false\"}\n" + answerSurvivesQuestion +
	"\n\n- REST: Matches the platform.\n- gRPC: Streams.\n:::\n\n" +
	":::ask{#rollout urgency=\"med\" multiple=\"false\"}\n" + answerSurvivesRemovedQuestion + "\n:::\n"

// answerSurvivesComposedOpener is the opener an agent composing the body writes in place of the
// one it reads: the block names no id of its own, since a body written rather than rendered names
// none, and none of the attributes the server keeps, which the text it reads prints none of (a
// person's answer is held once, on the ask, and the text read answers it in its asks member). Both
// ask blocks of the document are written with it, their urgency and their multiple being the same.
const answerSurvivesComposedOpener = `:::ask{urgency="med" multiple="false"}`

// answerSurvivesSettleMargin is how long a case waits, after the settlement of an upload it can
// see, before it says the upload opened no second ask: ten of the fixture's settlement periods,
// in which a settlement that was going to open one would have.
const answerSurvivesSettleMargin = 200 * time.Millisecond

// An agent that composes a document's body again writes each ask it holds from the question
// alone: no id, since a body written rather than rendered names none, and none of the attributes
// the server keeps, since the agent cannot know the answer a person gave. Driven over the routes
// the agent and the person use, that upload leaves the answered ask where it stands: the issue
// holds the ask rows it held, the person's row still answered and still carrying her answer, on
// the block of the document it was answered on, and her answer still on the ask's node and in the
// asks member of the text read; no second row opens, and no second item appears in her Inbox.
func TestAnAgentsComposedUploadLeavesTheAnsweredAskAndOpensNoSecondInboxItem(t *testing.T) {
	fixture := seedAnswerSurvives(t)
	before := fixture.answered

	fixture.upload(t, composedBody(t, before.text))
	fixture.await(t, "the settlement of the body the agent composed", func(state answerSurvivesState) bool {
		return state.settled()
	})
	// A second ask row is opened by a settlement of its own, so the measurement that says none
	// was opened is taken after the settlements that would have.
	time.Sleep(answerSurvivesSettleMargin)
	state := fixture.measure(t)

	// Each measurement is reported, so a run against a server that gives the block a fresh id
	// names that id, the rows it opened and the item it put in the person's Inbox, rather than
	// the first of them alone.
	if want := []string{"transport", "rollout"}; !slices.Equal(state.blocks, want) {
		t.Errorf("the document after the upload holds the ask blocks %v, want the blocks %v the questions were asked on\n%s", state.blocks, want, state)
	}
	if got, want := rowIDs(state.rows), rowIDs(before.rows); !slices.Equal(got, want) {
		t.Errorf("the issue holds the ask rows %v after the upload, want the rows %v it held before it\n%s", got, want, state)
	}
	if answered, held := state.row(fixture.answeredAsk); !held {
		t.Errorf("the ask row %s the person answered is gone after the upload\n%s", fixture.answeredAsk, state)
	} else {
		if answered.State != "answered" {
			t.Errorf("the ask the person answered reads %s after the upload, want it still answered\n%s", askRowLine(answered), state)
		}
		if answered.Question != answerSurvivesQuestion {
			t.Errorf("the ask the person answered reads %s after the upload, want it on the question the agent composed again, %q\n%s", askRowLine(answered), answerSurvivesQuestion, state)
		}
		if answered.BlockID == nil || *answered.BlockID != "transport" {
			t.Errorf("the ask the person answered reads %s after the upload, want it on the block transport the document holds\n%s", askRowLine(answered), state)
		}
		if answered.Answer == nil || answered.Answer.User != "alice" || !slices.Equal(answered.Answer.Selected, []string{"REST"}) {
			t.Errorf("the ask the person answered reads %s after the upload, want alice's answer selecting REST\n%s", askRowLine(answered), state)
		}
	}
	if got, want := rowIDs(state.inbox), rowIDs(before.inbox); !slices.Equal(got, want) {
		t.Errorf("alice's Inbox holds the asks %v after the upload, want the %v it held before it, the upload opening no question of its own\n%s", got, want, state)
	}
	if state.text != before.text {
		t.Errorf("the document after the upload and its settlement reads\n%s\nwant it as it read before the upload, each ask block under its id\n%s", state.text, before.text)
	}
	if !state.holdsAlicesAnswer("transport") || !reflect.DeepEqual(state.asks, before.asks) {
		t.Errorf("the text read after the upload answers the asks %s, want the person's answer still answered for the block transport, as before the upload: %s\n%s",
			textAskLines(state.asks), textAskLines(before.asks), state)
	}
	requireTheAnswerOnTheNode(t, fixture.database, "after the agent's composed upload", fixture.answeredAsk, "ask.answered")
}

// A question the body an agent uploads removes on purpose is still retracted. That body drops one
// ask block and writes the other from its question alone, so the carry that keeps the answered
// block's id is at work in the same upload: the ask whose block left the document is resolved as
// the settlement's own retraction, naming the version it left in, and it leaves the person's
// Inbox.
func TestAnUploadThatDropsAnAskBlockStillRetractsItsAsk(t *testing.T) {
	fixture := seedAnswerSurvives(t)

	fixture.upload(t, composedBody(t, withoutAskBlock(t, fixture.answered.text, "rollout")))
	fixture.await(t, "the settlement of the body the agent composed", func(state answerSurvivesState) bool {
		return state.settled()
	})
	time.Sleep(answerSurvivesSettleMargin)
	state := fixture.measure(t)

	// Each measurement is reported, as above; the two readings the others stand on are fatal.
	if slices.Contains(state.blocks, "rollout") {
		t.Errorf("the document after the upload still holds the ask block rollout the body removed\n%s", state)
	}
	if slices.Contains(rowIDs(state.inbox), fixture.removedAsk) {
		t.Errorf("alice's Inbox still shows the ask %s of the question the body removed\n%s", fixture.removedAsk, state)
	}
	removed, held := state.row(fixture.removedAsk)
	if !held {
		t.Fatalf("the ask row %s of the question the body removed is gone, want it kept as the record of that question\n%s", fixture.removedAsk, state)
	}
	if removed.State != "resolved" || removed.Resolution == nil || removed.Resolution.Kind != "retracted" {
		t.Fatalf("the ask of the question the body removed reads %s, want it retracted\n%s", askRowLine(removed), state)
	}
	if !removed.Resolution.Actor.SameAs(docs.SettlementActor) {
		t.Errorf("the ask of the question the body removed reads %s, want its retraction credited to the settlement itself, %s %q\n%s",
			askRowLine(removed), docs.SettlementActor.Kind, docs.SettlementActor.ID, state)
	}
	if !strings.HasPrefix(removed.Resolution.Reason, docs.SettlementRetractionReason) {
		t.Errorf("the ask of the question the body removed reads %s, want a reason naming the version the block left in, which begins %q\n%s",
			askRowLine(removed), docs.SettlementRetractionReason, state)
	}
}

// answerSurvivesFixture is the issue the cases above drive: a spec holding two questions, the
// first of them answered by a person.
type answerSurvivesFixture struct {
	handler     http.Handler
	database    *store.Store
	issueKey    string
	artifactID  string
	answeredAsk string
	removedAsk  string
	// answered is what the issue and its document read once the person's answer was held on its
	// ask and the text read answered it, which is the state the upload must leave as it is.
	answered answerSurvivesState
}

// seedAnswerSurvives opens an issue on answerSurvivesSpec, lets the settlement index both of its
// ask blocks, and answers the first of them as alice through the route the dashboard answers
// with. It leaves the issue with two ask rows, one answered and one open, and alice's Inbox
// holding the open one alone.
func seedAnswerSurvives(t *testing.T) answerSurvivesFixture {
	t.Helper()
	handler, database := blockAskHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "A body the agent composed", answerSurvivesSpec)
	awaitIndexedAskBlock(t, handler, issue.PrimaryArtifactID, "transport", answerSurvivesQuestion)
	awaitIndexedAskBlock(t, handler, issue.PrimaryArtifactID, "rollout", answerSurvivesRemovedQuestion)
	fixture := answerSurvivesFixture{
		handler:     handler,
		database:    database,
		issueKey:    issue.Key,
		artifactID:  issue.PrimaryArtifactID,
		answeredAsk: blockAskID(t, handler, issue.Key, "transport"),
		removedAsk:  blockAskID(t, handler, issue.Key, "rollout"),
	}
	answered := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+fixture.answeredAsk+"/answer", map[string]any{
		"selected": []string{"REST"},
	}, "alice")
	if answered.Code != http.StatusOK {
		t.Fatalf("answer the ask as the person: status=%d body=%s", answered.Code, answered.Body.String())
	}
	fixture.answered = fixture.await(t, "the person's answer held for the block she answered", func(state answerSurvivesState) bool {
		return state.settled() && state.holdsAlicesAnswer("transport")
	})
	seeded := []string{fixture.answeredAsk, fixture.removedAsk}
	slices.Sort(seeded)
	if got := rowIDs(fixture.answered.rows); !slices.Equal(got, seeded) {
		t.Fatalf("the issue holds the ask rows %v before the upload, want the two %v its spec asked\n%s", got, seeded, fixture.answered)
	}
	if got := rowIDs(fixture.answered.inbox); !slices.Equal(got, []string{fixture.removedAsk}) {
		t.Fatalf("alice's Inbox holds the asks %v before the upload, want the open ask %s alone\n%s", got, fixture.removedAsk, fixture.answered)
	}
	return fixture
}

// upload sends content as the agent's whole new body of the issue's spec, naming the base it
// read, which is the upload `dispatch artifact` makes.
func (f answerSurvivesFixture) upload(t *testing.T, content string) {
	t.Helper()
	uploaded := sessionRequest(t, f.handler, http.MethodPost, "/api/v1/issues/"+f.issueKey+"/artifacts", map[string]any{
		"actor":        sessionActor(),
		"name":         "spec.md",
		"content":      content,
		"precondition": uploadBase(t, f.handler, f.artifactID),
	})
	if uploaded.Code != http.StatusCreated {
		t.Fatalf("upload the body the agent composed: status=%d body=%s", uploaded.Code, uploaded.Body.String())
	}
}

// await polls the issue and its document until holds is true of what they read, and answers that
// state, so a case waits for a settlement rather than for a duration. A settlement that never
// reaches it fails the case with everything the wait read.
func (f answerSurvivesFixture) await(t *testing.T, what string, holds func(answerSurvivesState) bool) answerSurvivesState {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		state := f.measure(t)
		if holds(state) {
			return state
		}
		if time.Now().After(deadline) {
			t.Fatalf("the server never showed %s; it reads\n%s", what, state)
		}
	}
}

// measure reads what every assertion above is made of: every ask row of the issue, the ask block
// ids its document holds, that document's markdown and the asks member of its text read, whether
// it owes a settlement, and the asks alice's Inbox shows her.
func (f answerSurvivesFixture) measure(t *testing.T) answerSurvivesState {
	t.Helper()
	asks := dispatchRequest(t, f.handler, http.MethodGet, "/api/v1/issues/"+f.issueKey+"/asks?state=all", nil, "alice")
	if asks.Code != http.StatusOK {
		t.Fatalf("list the issue's asks: status=%d body=%s", asks.Code, asks.Body.String())
	}
	// The Inbox is the person's own read of the asks waiting on her, GET /api/v1/inbox, which is
	// every open ask row of an open issue; an ask reaches it by being an open row, and leaves it
	// by being answered or resolved. So an item appearing there is a row opening, and the issue
	// holds no asks but this document's.
	inbox := dispatchRequest(t, f.handler, http.MethodGet, "/api/v1/inbox", nil, "alice")
	if inbox.Code != http.StatusOK {
		t.Fatalf("read alice's Inbox: status=%d body=%s", inbox.Code, inbox.Body.String())
	}
	owed := documentOwesSettlement(t, f.database, f.artifactID)
	read := dispatchRequest(t, f.handler, http.MethodGet, "/api/v1/artifacts/"+f.artifactID+"/text", nil, "alice")
	if read.Code != http.StatusOK {
		t.Fatalf("read the document's text: status=%d body=%s", read.Code, read.Body.String())
	}
	text := decodeBody[struct {
		Markdown string          `json:"markdown"`
		Asks     []model.TextAsk `json:"asks"`
	}](t, read)
	state := answerSurvivesState{
		rows:  decodeBody[[]model.Ask](t, asks),
		inbox: decodeBody[[]model.Ask](t, inbox),
		text:  text.Markdown,
		asks:  text.Asks,
		owed:  owed,
	}
	tree, err := pmdoc.ParseRendering(state.text)
	if err != nil {
		t.Fatalf("read the document's markdown back: %v\n%s", err, state.text)
	}
	pmdoc.WalkAskBlocks(tree, func(id string) { state.blocks = append(state.blocks, id) })
	return state
}

// answerSurvivesState is what a case measured at one moment: every ask row of the issue, the ask
// block ids its document holds in document order, that document's markdown and the asks member of
// its text read, whether it owes a settlement, and the asks alice's Inbox shows her. Every failure
// message above prints it, so a run that reads something else says what it read.
type answerSurvivesState struct {
	rows   []model.Ask
	blocks []string
	text   string
	asks   []model.TextAsk
	owed   bool
	inbox  []model.Ask
}

func (state answerSurvivesState) String() string {
	return fmt.Sprintf("the issue's ask rows: %s\nthe document's ask blocks: %v\nits asks member: %s\nit owes a settlement: %t\nalice's Inbox: %s\nthe document:\n%s",
		askRowLines(state.rows), state.blocks, textAskLines(state.asks), state.owed, askRowLines(state.inbox), state.text)
}

// settled reports whether the document owes no settlement: a write that changes it marks it owing
// one in the write's own transaction, and the settlement clears that mark once it has run over
// what the write left, the body the agent uploaded included.
func (state answerSurvivesState) settled() bool {
	return !state.owed
}

// holdsAlicesAnswer reports whether the text read answers the ask block blockID with alice's
// answer selecting REST, which is where an agent reads a person's answer: the asks member, read
// from the ask, since the markdown holds none of it.
func (state answerSurvivesState) holdsAlicesAnswer(blockID string) bool {
	for _, entry := range state.asks {
		if entry.BlockID == blockID {
			return entry.State == "answered" && entry.AnsweredBy == "alice" &&
				entry.Selected != nil && slices.Equal(*entry.Selected, []string{"REST"})
		}
	}
	return false
}

// row is the ask row of the issue with this id, and whether the issue holds one.
func (state answerSurvivesState) row(id string) (model.Ask, bool) {
	for _, ask := range state.rows {
		if ask.ID == id {
			return ask, true
		}
	}
	return model.Ask{}, false
}

// composedBody is markdown as an agent composing the document's body writes it again: every ask
// block's opener written from its question alone (answerSurvivesComposedOpener), and the rest of
// the document word for word as it reads now.
func composedBody(t *testing.T, markdown string) string {
	t.Helper()
	lines := strings.Split(markdown, "\n")
	composed := 0
	for index, line := range lines {
		if !strings.HasPrefix(line, ":::ask{#") {
			continue
		}
		lines[index] = answerSurvivesComposedOpener
		composed++
	}
	if composed == 0 {
		t.Fatalf("the document holds no ask block for the agent to compose again:\n%s", markdown)
	}
	return strings.Join(lines, "\n")
}

// withoutAskBlock is markdown with the whole ask block blockID taken out, the blank line after it
// included: the body of an agent that removes a question on purpose.
func withoutAskBlock(t *testing.T, markdown, blockID string) string {
	t.Helper()
	lines := strings.Split(markdown, "\n")
	opener := ":::ask{#" + blockID
	start := slices.IndexFunc(lines, func(line string) bool {
		return strings.HasPrefix(line, opener+" ") || line == opener+"}"
	})
	if start < 0 {
		t.Fatalf("the document holds no ask block %q to remove:\n%s", blockID, markdown)
	}
	end := start
	for end < len(lines) && lines[end] != ":::" {
		end++
	}
	if end == len(lines) {
		t.Fatalf("the ask block %q is never closed:\n%s", blockID, markdown)
	}
	end++
	if end < len(lines) && lines[end] == "" {
		end++
	}
	return strings.Join(slices.Concat(lines[:start], lines[end:]), "\n")
}

// rowIDs is the id of every ask of a set, sorted, which is what two sets holding the same asks
// means.
func rowIDs(asks []model.Ask) []string {
	ids := make([]string, len(asks))
	for index, ask := range asks {
		ids[index] = ask.ID
	}
	slices.Sort(ids)
	return ids
}

// textAskLines names the entries of a text read's asks member in a failure message.
func textAskLines(asks []model.TextAsk) string {
	encoded, err := json.Marshal(asks)
	if err != nil {
		return fmt.Sprintf("%v (unencodable: %v)", asks, err)
	}
	return string(encoded)
}

// askRowLines names every ask of a set in a failure message.
func askRowLines(asks []model.Ask) string {
	if len(asks) == 0 {
		return "none"
	}
	lines := make([]string, len(asks))
	for index, ask := range asks {
		lines[index] = askRowLine(ask)
	}
	return strings.Join(lines, "; ")
}

// askRowLine names one ask row in a failure message: what it is, where it is, what it asks, and
// what it holds, which is its answer or the record of its closure.
func askRowLine(ask model.Ask) string {
	block := "no block"
	if ask.BlockID != nil {
		block = *ask.BlockID
	}
	held := "carrying no answer"
	if ask.Answer != nil {
		held = fmt.Sprintf("carrying %s's answer selecting %v", ask.Answer.User, ask.Answer.Selected)
	}
	if ask.Resolution != nil {
		held = fmt.Sprintf("%s by %s %q, %q", ask.Resolution.Kind, ask.Resolution.Actor.Kind, ask.Resolution.Actor.ID, ask.Resolution.Reason)
	}
	return fmt.Sprintf("row %s on block %s asking %q, %s, %s", ask.ID, block, ask.Question, ask.State, held)
}
