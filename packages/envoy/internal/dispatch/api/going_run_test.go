package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/agentloop"
	"github.com/sjawhar/envoy/going"
	"github.com/sjawhar/envoy/going/goingtest"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// goingDecisionModel is the measured model the decision tests configure: the one the committed
// record names chosen, so the tests run what production runs.
const goingDecisionModel = "jev-preview"

// decisionStandIn is a System One endpoint that answers each decision with the answer the test
// last set, counts the calls and keeps each body it was sent. It calls nothing and holds no
// credential.
type decisionStandIn struct {
	url   string
	calls atomic.Int32

	mu     sync.Mutex
	reply  string
	bodies []map[string]json.RawMessage
}

func newDecisionStandIn(t *testing.T) *decisionStandIn {
	t.Helper()
	standIn := &decisionStandIn{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]json.RawMessage
		_ = json.Unmarshal(raw, &body)
		standIn.calls.Add(1)
		standIn.mu.Lock()
		standIn.bodies = append(standIn.bodies, body)
		reply := standIn.reply
		standIn.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, reply)
	}))
	t.Cleanup(server.Close)
	standIn.url = server.URL
	return standIn
}

// decidedAnswer is what the stand-in answers the three questions with: the action chosen with
// its confidence and the probability of each choice, what the words are about, and the display
// with its confidence.
type decidedAnswer struct {
	action            string
	confidence        float64
	probabilities     map[string]float64
	about             string
	display           string
	displayConfidence float64
}

// answer sets what the stand-in answers every decision with from now on.
func (s *decisionStandIn) answer(t *testing.T, answer decidedAnswer) {
	t.Helper()
	reply, err := json.Marshal(map[string]any{
		"model": goingDecisionModel,
		"answers": map[string]any{
			going.QuestionAction:  map[string]any{"type": "choice", "choice": answer.action, "confidence": answer.confidence, "probabilities": answer.probabilities},
			going.QuestionContext: map[string]any{"type": "choice", "choice": answer.about, "confidence": 0.9},
			going.QuestionDisplay: map[string]any{"type": "choice", "choice": answer.display, "confidence": answer.displayConfidence},
		},
		"usage": map[string]any{"input_tokens": 100, "output_tokens": 10},
	})
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.reply = string(reply)
	s.mu.Unlock()
}

// decider is the measured decider at this stand-in, its token answered by token.
func (s *decisionStandIn) decider(t *testing.T, token func(context.Context) (string, error)) *Decider {
	t.Helper()
	decider, measured := NewDecider(agentloop.Model{
		Endpoint: agentloop.Endpoint{Kind: agentloop.SystemOne, BaseURL: s.url, Token: token},
		ID:       goingDecisionModel,
	})
	if !measured {
		t.Fatalf("NewDecider refused %s, which the record holds", goingDecisionModel)
	}
	return decider
}

// offeredActions is every action choice the last decision put to the model, none among them.
func (s *decisionStandIn) offeredActions(t *testing.T) []string {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.bodies) == 0 {
		t.Fatal("no decision was asked")
	}
	var questions map[string]agentloop.Question
	if err := json.Unmarshal(s.bodies[len(s.bodies)-1]["questions"], &questions); err != nil {
		t.Fatalf("decode the decision's questions: %v", err)
	}
	var criteria map[string]string
	if err := json.Unmarshal(questions[going.QuestionAction].Criteria, &criteria); err != nil {
		t.Fatalf("decode the action question's criteria: %v", err)
	}
	return slices.Sorted(maps.Keys(criteria))
}

// storedGoingFrames waits for the run to end and answers every frame it stored, in order.
func storedGoingFrames(t *testing.T, rig goingRoutesRig, id string) []goingEvent {
	t.Helper()
	rig.awaitTerminal(t, id)
	frames, _, err := rig.server.readGoingStream(context.Background(), id, 0)
	if err != nil {
		t.Fatalf("read the run's frames: %v", err)
	}
	return frames
}

// frameNames is each stored frame's name, in order.
func frameNames(frames []goingEvent) []string {
	names := make([]string, len(frames))
	for index, frame := range frames {
		names[index] = frame.Event.Name
	}
	return names
}

// frameData decodes the first stored frame of that name into value, and reports whether the run
// stored one.
func frameData(t *testing.T, frames []goingEvent, name string, value any) bool {
	t.Helper()
	for _, frame := range frames {
		if frame.Event.Name != name {
			continue
		}
		raw, err := json.Marshal(frame.Event.Data)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, value); err != nil {
			t.Fatalf("decode the %s frame %s: %v", name, raw, err)
		}
		return true
	}
	return false
}

// askRun asks one question as login and answers the run's id.
func askRun(t *testing.T, rig goingRoutesRig, login string, body map[string]any) string {
	t.Helper()
	asked := rig.ask(t, login, body)
	if asked.Code != http.StatusCreated {
		t.Fatalf("ask: status=%d body=%s", asked.Code, asked.Body.String())
	}
	return decodeBody[askedRun](t, asked).ID
}

// A run with a measured decision model decides first: the decision is the stream's first frame,
// then the answer's text, and the end last. The answer only reads: the pause writes nothing, no
// landing and no run of the bar's agent (going_runs).
func TestGoingRunDecidesBeforeItAnswers(t *testing.T) {
	standIn := newDecisionStandIn(t)
	standIn.answer(t, decidedAnswer{
		action: "dispatch_issue_update", confidence: 0.9,
		probabilities: map[string]float64{"dispatch_issue_update": 0.7, "dispatch_comment": 0.2, going.ActionNone: 0.1},
		about:         going.ContextNamed, display: going.DisplayProse, displayConfidence: 0.9,
	})
	answerer, chat := newGoingModelAnswerer(t, goingtest.Reply{Final: &goingtest.Final{Answer: "Both close."}})
	handler, database, deps, _ := newRegisteredTestServer(t, testServerOptions{
		answerer: answerer, answerModel: "going-test-deployment", decider: standIn.decider(t, staticToken),
	})
	rig := goingRoutesRig{handler: handler, server: directServer(deps), database: database}

	id := askRun(t, rig, "alice", map[string]any{"words": "close DEMO-1 and DEMO-2"})
	frames := storedGoingFrames(t, rig, id)
	names := frameNames(frames)
	if names[0] != contracts.GoingEventDecision || frames[0].Sequence != 1 {
		t.Fatalf("the run stored %v, want the decision first", names)
	}
	if len(names) < 3 || names[len(names)-1] != contracts.GoingEventEnd {
		t.Fatalf("the run stored %v, want the answer after the decision and the end last", names)
	}
	for _, name := range names[1 : len(names)-1] {
		if name != contracts.GoingEventText {
			t.Fatalf("the run stored %v, want only the answer's text between the decision and the end", names)
		}
	}
	if calls := standIn.calls.Load(); calls != 1 {
		t.Fatalf("the decision model was called %d times, want once", calls)
	}

	var decision goingDecision
	frameData(t, frames, contracts.GoingEventDecision, &decision)
	if decision.Action != "dispatch_issue_update" || decision.Line != goingLineDo || decision.Context != going.ContextNamed {
		t.Fatalf("the decision is %+v, want dispatch_issue_update, the line Do, about items the words name", decision)
	}

	if requests := chat.Requests(); len(requests) != 1 {
		t.Fatalf("the answer made %d calls, want one", len(requests))
	}

	// With no run row a pause lands no write either: going_landings.run_id references going_runs.
	var runs int
	if err := rig.database.Pool.QueryRow(context.Background(), `select count(*) from going_runs where id = $1::uuid`, id).Scan(&runs); err != nil || runs != 0 {
		t.Errorf("the pause wrote %d runs (%v), want none: a pause is no run of the bar's agent", runs, err)
	}
}

// The actions a run offers are the ones the caller's credential may run, read from the route
// table's own auth class: a person at a browser is never offered a handoff, which
// only a bearer may write, while the same person's own token is.
func TestGoingDecisionOffersOnlyTheActionsTheCallersCredentialMayRun(t *testing.T) {
	standIn := newDecisionStandIn(t)
	standIn.answer(t, decidedAnswer{action: going.ActionNone, confidence: 1, about: going.ContextNone, display: going.DisplayProse, displayConfidence: 1})
	answerer := &fakeAnswerer{frames: []going.Event{going.End(0, 0)}}
	handler, database, deps, _ := newRegisteredTestServer(t, testServerOptions{answerer: answerer, decider: standIn.decider(t, staticToken)})
	rig := goingRoutesRig{handler: handler, server: directServer(deps), database: database}

	rig.awaitTerminal(t, askRun(t, rig, "alice", map[string]any{"words": "what is open?"}))
	browser := standIn.offeredActions(t)
	if slices.Contains(browser, "dispatch_handoff_write") || !slices.Contains(browser, "dispatch_claim") || !slices.Contains(browser, going.ActionNone) {
		t.Fatalf("a browser's run was offered %v, want the claim and none and no handoff", browser)
	}

	token := personalTokenOwnedBy(t, database, "alice", model.ScopePerson)
	asked := rig.askUnder(t, token, map[string]any{"words": "what is open?"})
	if asked.Code != http.StatusCreated {
		t.Fatalf("ask under alice's token: status=%d body=%s", asked.Code, asked.Body.String())
	}
	rig.awaitTerminal(t, decodeBody[askedRun](t, asked).ID)
	if bearer := standIn.offeredActions(t); !slices.Contains(bearer, "dispatch_handoff_write") {
		t.Fatalf("a bearer's run was offered %v, want the handoff among them", bearer)
	}
}

// The line on top (R2): words that are an issue's key, or an issue's exact title whatever the
// case, are answered by their hit alone and ask the decision model nothing, nor the answer; for
// any other words the decision model's action names the line the decision frame carries - none
// gives Ask, the create of an issue gives File as a new issue, and any other action gives Do.
func TestTheLineOnTop(t *testing.T) {
	standIn := newDecisionStandIn(t)
	answerer := &fakeAnswerer{frames: []going.Event{going.Text("The answer."), going.End(2, 0)}}
	handler, database, deps, _ := newRegisteredTestServer(t, testServerOptions{answerer: answerer, decider: standIn.decider(t, staticToken)})
	rig := goingRoutesRig{handler: handler, server: directServer(deps), database: database}
	key := createTestIssue(t, handler, "SKY", "Polish the telescope")

	for _, words := range []string{key, "Polish the telescope", "polish THE Telescope"} {
		frames := storedGoingFrames(t, rig, askRun(t, rig, "alice", map[string]any{"words": words}))
		var item going.ItemData
		if got, want := frameNames(frames), []string{contracts.GoingEventItem, contracts.GoingEventEnd}; !slices.Equal(got, want) ||
			!frameData(t, frames, contracts.GoingEventItem, &item) || item.Kind != "issue" || item.ID != key {
			t.Fatalf("%q stored %v naming %+v, want the item of %s and the end: its hit, and no answer", words, got, item, key)
		}
	}
	if calls := standIn.calls.Load(); calls != 0 {
		t.Fatalf("a key and an exact title called the decision model %d times, want none", calls)
	}

	for _, tc := range []struct{ action, line string }{
		{going.ActionNone, goingLineAsk},
		{goingIssueCreateTool, goingLineFile},
		{"dispatch_issue_update", goingLineDo},
	} {
		standIn.answer(t, decidedAnswer{action: tc.action, confidence: 0.99, about: going.ContextNamed, display: going.DisplayProse, displayConfidence: 0.9})
		frames := storedGoingFrames(t, rig, askRun(t, rig, "alice", map[string]any{"words": "we need a dome for the telescope"}))
		var decision goingDecision
		if !frameData(t, frames, contracts.GoingEventDecision, &decision) || decision.Action != tc.action || decision.Line != tc.line {
			t.Errorf("Jev answering %s stored the decision %+v, want the line %q", tc.action, decision, tc.line)
		}
	}
	if calls := standIn.calls.Load(); calls != 3 {
		t.Fatalf("three questions called the decision model %d times, want once each", calls)
	}
}

// With no decision model configured, and with one whose token cannot be read, the answer streams
// as it always did: no decision frame, and no call to the decision endpoint.
func TestGoingRunWithNoDecisionModelOrAnUnreachableOneAnswersWithNoDecision(t *testing.T) {
	standIn := newDecisionStandIn(t)
	standIn.answer(t, decidedAnswer{action: "dispatch_claim", confidence: 1, about: going.ContextPage, display: going.DisplayProse, displayConfidence: 1})
	unreadable := func(context.Context) (string, error) { return "", errors.New("the token file is absent") }
	for name, decider := range map[string]*Decider{
		"no decision model": nil,
		"unreachable":       standIn.decider(t, unreadable),
	} {
		t.Run(name, func(t *testing.T) {
			answerer := &fakeAnswerer{frames: []going.Event{going.Text("The answer."), going.End(2, 0)}}
			handler, database, deps, _ := newRegisteredTestServer(t, testServerOptions{answerer: answerer, decider: decider})
			rig := goingRoutesRig{handler: handler, server: directServer(deps), database: database}
			frames := storedGoingFrames(t, rig, askRun(t, rig, "alice", map[string]any{
				"words": "claim this", "context": map[string]any{"path": "/issues/DEMO-12", "issue": "DEMO-12", "project": "DEMO"},
			}))
			if got, want := frameNames(frames), []string{contracts.GoingEventText, contracts.GoingEventEnd}; !slices.Equal(got, want) {
				t.Fatalf("the run stored %v, want %v: the answer alone", got, want)
			}
		})
	}
	if calls := standIn.calls.Load(); calls != 0 {
		t.Fatalf("the decision endpoint was called %d times, want none", calls)
	}
}

// A cancelled run stops at its next frame, wherever its answer is being written: the model, which
// streams slowly, sees its reply abandoned; nothing the answer writes after the cancel is stored;
// a frame appended after it is refused. "this process" cancels through the route, which ends the
// run here at once; "another process" marks the record alone, as a cancel landing on another pod
// does, and the run's next frame write is what stops it.
func TestGoingCancelStopsARunAtItsNextFrame(t *testing.T) {
	for _, where := range []string{"this process", "another process"} {
		t.Run(where, func(t *testing.T) {
			answer := strings.Repeat("The pelicans arrive in spring and leave in autumn. ", 12)
			answerer, chat := newGoingModelAnswerer(t, goingtest.Reply{
				Final: &goingtest.Final{Answer: answer}, Chunk: 4, Pause: 100 * time.Millisecond,
			})
			rig := newGoingRoutesRig(t, answerer)
			id := askRun(t, rig, "alice", map[string]any{"words": "when do the pelicans arrive?"})

			awaitGoing(t, "the answer's first text frame", func() bool {
				frames, _, err := rig.server.readGoingStream(context.Background(), id, 0)
				return err == nil && slices.Contains(frameNames(frames), contracts.GoingEventText)
			})
			switch where {
			case "this process":
				cancelled := dispatchRequest(t, rig.handler, http.MethodPost, "/api/v1/going/requests/"+id+"/cancel", nil, "alice")
				if cancelled.Code != http.StatusOK {
					t.Fatalf("cancel: status=%d body=%s", cancelled.Code, cancelled.Body.String())
				}
				if body := decodeBody[goingCancellation](t, cancelled); body.ID != id || body.CancelledAt.IsZero() {
					t.Fatalf("cancel answered %+v, want the run and its moment", body)
				}
			default:
				if _, err := rig.server.cancelGoingRun(context.Background(), id); err != nil {
					t.Fatalf("mark the run cancelled: %v", err)
				}
			}
			atCancel, _, err := rig.server.readGoingStream(context.Background(), id, 0)
			if err != nil {
				t.Fatal(err)
			}

			awaitGoing(t, "the model to see its reply abandoned", func() bool { return chat.Abandoned() == 1 })
			// Past the model's own next frame, so a frame the answer still had in hand has had
			// every chance to be stored.
			time.Sleep(300 * time.Millisecond)
			after, _, err := rig.server.readGoingStream(context.Background(), id, 0)
			if err != nil {
				t.Fatal(err)
			}
			if len(after) != len(atCancel) {
				t.Fatalf("the run stored %v by the cancel and %v after it, want nothing more", frameNames(atCancel), frameNames(after))
			}
			if chat.Calls() != 1 {
				t.Errorf("the model was called %d times, want the one call it abandoned", chat.Calls())
			}
			if _, err := rig.server.appendGoingEvent(context.Background(), id, going.Text("late")); !errors.Is(err, pgx.ErrNoRows) {
				t.Errorf("a frame appended after the cancel: %v, want it refused (pgx.ErrNoRows)", err)
			}
		})
	}
}

// Only a run's author may cancel it, and a stranger is answered as the stream answers one: 404,
// the run left running. An id that is no run, and one that is not a uuid, are answered alike.
func TestGoingCancelOfAnotherCallersRunIsNotFound(t *testing.T) {
	rig := newGoingRoutesRig(t, &fakeAnswerer{frames: []going.Event{going.End(0, 0)}})
	id := askRun(t, rig, "alice", map[string]any{"words": "whose run is this?"})
	rig.awaitTerminal(t, id)
	for name, target := range map[string]string{
		"another caller's run": id,
		"no run":               "0199e2a0-3333-7000-8000-000000000003",
		"no uuid":              "not-a-run",
	} {
		response := dispatchRequest(t, rig.handler, http.MethodPost, "/api/v1/going/requests/"+target+"/cancel", nil, "bob")
		if response.Code != http.StatusNotFound {
			t.Fatalf("%s: status=%d body=%s, want 404", name, response.Code, response.Body.String())
		}
		if refusal := decodeBody[refusalAnswer](t, response); refusal.Code != codeGoingRequestNotFound {
			t.Fatalf("%s: code=%q, want %s", name, refusal.Code, codeGoingRequestNotFound)
		}
	}
	var cancelled *time.Time
	if err := rig.database.Pool.QueryRow(context.Background(), `select cancelled_at from going_requests where id = $1::uuid`, id).Scan(&cancelled); err != nil {
		t.Fatal(err)
	}
	if cancelled != nil {
		t.Fatalf("a stranger's cancel marked alice's run cancelled at %v", *cancelled)
	}
}

// awaitGoing polls until done holds, failing the test after ten seconds.
func awaitGoing(t *testing.T, what string, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !done() {
		if time.Now().After(deadline) {
			t.Fatalf("waited 10s for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
