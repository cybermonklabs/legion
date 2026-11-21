package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/modeltest"
	"github.com/sjawhar/envoy/internal/dispatch/reviewer"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// A person's Rewrite and Define on the server (INFRA-3231's R7 and Decisions 23 and 24,
// INFRA-3552's R1 to R4): a stand-in model that rewrites and defines, a fake listener whose
// writer session advertises the capabilities a test gives it and keeps every send, and an issue
// alice holds, on which the writer session asks.

const (
	rewriteMarker = "You write a text again for its reader."
	defineMarker  = "You define words for a reader."
	// rewriteQuestion is the question the writer asks, and rewriteBody its rewrite.
	rewriteQuestion = "Ship the reviewer?\nWe spin up the node."
	rewriteBody     = "Ship the reviewer?\nThe held ask's test fails on its first call; ship it today or wait for a fix."
	agentAnswer     = "The node is the test runner. Its first call fails until the store answers."
)

// rewriteListener is the Envoy listener of the Rewrite and Define tests: the writer session is
// live with the capabilities a test gives it, any other is unknown, and every send is kept.
type rewriteListener struct {
	mu           sync.Mutex
	capabilities []string
	sends        []map[string]any
}

func newRewriteListener(t *testing.T, capabilities ...string) (string, *rewriteListener) {
	t.Helper()
	listener := &rewriteListener{capabilities: capabilities}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/registry/", func(w http.ResponseWriter, r *http.Request) {
		if strings.TrimPrefix(r.URL.Path, "/v1/registry/") != heldWriter {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{
			"session_id": heldWriter, "state": "live", "machine_id": "m1", "host": "m1",
			"capabilities": listener.capabilities, "roles": []string{},
		})
	})
	mux.HandleFunc("/v1/messages/send", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		listener.mu.Lock()
		listener.sends = append(listener.sends, body)
		count := len(listener.sends)
		listener.mu.Unlock()
		WriteJSON(w, http.StatusOK, map[string]any{"event_id": "env-" + string(rune('a'+count)), "recipient": body["target_session"]})
	})
	mux.HandleFunc("/v1/sessions", func(w http.ResponseWriter, _ *http.Request) { WriteJSON(w, http.StatusOK, []any{}) })
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server.URL, listener
}

// frame is one send's delivered frame: its message id, its delivery mode and the actor it is
// from.
type sentFrame struct {
	id, mode, body string
	actor          model.Actor
}

func (l *rewriteListener) frames() []sentFrame {
	l.mu.Lock()
	defer l.mu.Unlock()
	var frames []sentFrame
	for _, send := range l.sends {
		payload, _ := send["payload"].(string)
		var frame struct {
			Event struct {
				Actor   model.Actor    `json:"actor"`
				Payload map[string]any `json:"payload"`
			} `json:"event"`
			Delivery struct {
				Mode string `json:"mode"`
			} `json:"delivery"`
		}
		if json.Unmarshal([]byte(payload), &frame) != nil {
			continue
		}
		id, _ := frame.Event.Payload["id"].(string)
		body, _ := frame.Event.Payload["body"].(string)
		frames = append(frames, sentFrame{id: id, mode: frame.Delivery.Mode, body: body, actor: frame.Event.Actor})
	}
	return frames
}

// rewriteRig is a server with Rewrite and Define on, its stand-in model, its listener and an
// issue alice holds.
type rewriteRig struct {
	t        *testing.T
	handler  http.Handler
	database *store.Store
	served   *server
	stand    *modeltest.Model
	listener *rewriteListener
	issue    string
}

// rewriteJSON is a rewrite's reply: the body and the options, each {label, description}, as the
// reply's schema has a model write them, with no id.
func rewriteJSON(t *testing.T, body string, multiple bool, options ...map[string]string) string {
	t.Helper()
	encoded, err := json.Marshal(map[string]any{"body": body, "options": options, "multiple": multiple})
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func option(label, description string) map[string]string {
	return map[string]string{"label": label, "description": description}
}

// keptRewrite is the stand-in's rewrite of rewriteQuestion, which keeps what a rewrite keeps.
func keptRewrite(t *testing.T) string {
	return rewriteJSON(t, rewriteBody, false,
		option("Ship it", "Release it today, with the failing test."), option("Do not file", "Wait until the test passes."))
}

// newRewriteRig builds the rig: the writer session advertises capabilities, and the stand-in
// answers rules before its default rewrite of rewriteQuestion and its definition of `store`.
func newRewriteRig(t *testing.T, capabilities []string, rules ...modeltest.Rule) *rewriteRig {
	t.Helper()
	rules = append(rules,
		modeltest.Rule{Contains: rewriteMarker, Reply: modeltest.Reply{Text: keptRewrite(t)}},
		modeltest.Rule{Contains: defineMarker, Reply: modeltest.Reply{Text: `{"definitions":[{"words":"store","meaning":"The table where a held text waits until it posts.","links":[]}]}`}},
	)
	stand := modeltest.New(t, rules...)
	listenerURL, listener := newRewriteListener(t, capabilities...)
	handler, database, _, served := newRegisteredTestServer(t, testServerOptions{
		rewriteDefine: NewRewriteDefine(RewriteDefineConfig{Model: reviewerModel(stand.URL), Rules: reviewer.DefaultRules, SideQuestionWait: 15 * time.Second}),
		envoyURL:      listenerURL, envoyTimeout: 2 * time.Second,
	})
	rig := &rewriteRig{t: t, handler: handler, database: database, served: served, stand: stand, listener: listener}
	rig.issue = createInteractionIssue(t, handler, "RWD", "Rewrite issue", "The store holds the held texts.\n").Key
	return rig
}

// ask opens a question as the writer session and answers its id.
func (rig *rewriteRig) ask(question string, options ...map[string]string) string {
	rig.t.Helper()
	if len(options) == 0 {
		options = []map[string]string{
			{"label": "Ship it", "description": "Release it today."}, {"label": "Do not file", "description": "Wait."},
		}
	}
	response := agentRequest(rig.t, rig.handler, http.MethodPost, "/api/v1/issues/"+rig.issue+"/asks", map[string]any{
		"question": question, "options": options, "actor": map[string]any{"kind": "session", "id": heldWriter},
	}, "agent-token")
	if response.Code != http.StatusCreated {
		rig.t.Fatalf("the writer's ask: %d %s", response.Code, response.Body.String())
	}
	return decodeBody[struct {
		ID string `json:"id"`
	}](rig.t, response).ID
}

// rewrite is alice's Rewrite of the ask id, which must answer 202.
func (rig *rewriteRig) rewrite(id string) askRewriteAnswer {
	rig.t.Helper()
	response := dispatchRequest(rig.t, rig.handler, http.MethodPost, "/api/v1/asks/"+id+"/rewrites", nil, "alice")
	if response.Code != http.StatusAccepted {
		rig.t.Fatalf("alice's Rewrite of %s: %d %s", id, response.Code, response.Body.String())
	}
	return decodeBody[askRewriteAnswer](rig.t, response)
}

// waitRewrite waits for the ask's run to end in state, and answers the run's reason.
func (rig *rewriteRig) waitRewrite(id, state string) string {
	rig.t.Helper()
	var current, reason string
	deadline := time.Now().Add(30 * time.Second)
	for {
		if err := rig.database.Pool.QueryRow(context.Background(), `
			select state, coalesce(reason, '') from ask_rewrites where ask_id = $1::uuid`, id).Scan(&current, &reason); err == nil && current == state {
			return reason
		}
		if time.Now().After(deadline) {
			rig.t.Fatalf("ask %s's rewrite is %q (%q), want %s", id, current, reason, state)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func (rig *rewriteRig) question(id string) string {
	rig.t.Helper()
	var question string
	if err := rig.database.Pool.QueryRow(context.Background(), `select question from asks where id = $1::uuid`, id).Scan(&question); err != nil {
		rig.t.Fatal(err)
	}
	return question
}

// options are the ask id's options as alice reads them, each with the id a read shows.
func (rig *rewriteRig) options(id string) []model.AskOption {
	rig.t.Helper()
	response := dispatchRequest(rig.t, rig.handler, http.MethodGet, "/api/v1/asks/"+id, nil, "alice")
	if response.Code != http.StatusOK {
		rig.t.Fatalf("alice's read of ask %s: %d %s", id, response.Code, response.Body.String())
	}
	return decodeBody[struct {
		Ask model.Ask `json:"ask"`
	}](rig.t, response).Ask.Options
}

// rewriteOf is the `rewrite` of the object with the id id in what login reads at target: an ask,
// a comment, a message or an issue (whose id is its key), wherever the answer holds it.
func rewriteOf(t *testing.T, handler http.Handler, target, login, id string) *model.TextRewrite {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, target, nil, login)
	if response.Code != http.StatusOK {
		t.Fatalf("%s's read of %s: %d %s", login, target, response.Code, response.Body.String())
	}
	var tree any
	if err := json.Unmarshal(response.Body.Bytes(), &tree); err != nil {
		t.Fatal(err)
	}
	var found *model.TextRewrite
	var walk func(any)
	walk = func(node any) {
		switch value := node.(type) {
		case map[string]any:
			if value["id"] == id || value["key"] == id {
				if raw, held := value["rewrite"]; held && found == nil {
					encoded, _ := json.Marshal(raw)
					found = &model.TextRewrite{}
					if err := json.Unmarshal(encoded, found); err != nil {
						t.Fatalf("decode the rewrite of %s in %s: %v", id, target, err)
					}
				}
			}
			for _, child := range value {
				walk(child)
			}
		case []any:
			for _, child := range value {
				walk(child)
			}
		}
	}
	walk(tree)
	return found
}

// A person's Rewrite answers at once and rewrites in the background: the writer session, whose
// host advertises btw, gets one side question from Dispatch as a message on its own conversation,
// linked to the ask by its address; a second press while it runs starts nothing and gets the
// first's result; the rewrite reads the agent's answer and is applied as a quiet ask.edited from
// Dispatch that holds the old wording, who asked and that the answer was read; each request is a
// rewrite_requested row; the four ask reads carry the rewrite; and an answer to the old wording is
// refused ASK_EDITED.
func TestARewriteAsksTheWriterOnceAndAppliesAQuietEdit(t *testing.T) {
	rig := newRewriteRig(t, []string{"btw", "aside", "steer"})
	id := rig.ask(rewriteQuestion)
	first := rig.rewrite(id)
	if !first.Started || first.Rewrite.State != model.RewriteRewriting || first.Rewrite.By != model.RewriteByPerson ||
		first.Rewrite.Original.Body != rewriteQuestion {
		t.Fatalf("the first press answered %+v, want a rewrite started and running", first)
	}
	second := rig.rewrite(id)
	if second.Started || second.Rewrite.State != model.RewriteRewriting || !second.Rewrite.At.Equal(first.Rewrite.At) {
		t.Fatalf("the second press answered %+v, want the first's run", second)
	}
	if running := rewriteOf(t, rig.handler, "/api/v1/asks/"+id, "alice", id); running == nil || running.State != model.RewriteRewriting {
		t.Fatalf("the ask's read while the rewrite runs carries %+v, want rewriting", running)
	}

	// The side question: one message from Dispatch to the writer session, on no issue, naming the
	// ask by its address, sent in btw mode.
	var question string
	pollUntil(t, 20*time.Second, "the side question, sent to the writer session", func() bool {
		return rig.database.Pool.QueryRow(context.Background(), `
			select m.id::text from messages m join message_deliveries d on d.message_id = m.id
			where m.author->>'kind' = 'system' and m.target = 'session:'||$1 and d.session_id = $1 and d.state = 'sent'`,
			heldWriter).Scan(&question) == nil
	})
	var issueKey *string
	var body string
	if err := rig.database.Pool.QueryRow(context.Background(), `select issue_key, body from messages where id = $1::uuid`, question).Scan(&issueKey, &body); err != nil {
		t.Fatal(err)
	}
	if issueKey != nil || !strings.Contains(body, "dispatch://"+rig.issue+"/ask/"+id) {
		t.Fatalf("the side question is on issue %v and reads %q, want no issue and the ask's address", issueKey, body)
	}
	if frames := rig.listener.frames(); len(frames) != 1 || frames[0].mode != "btw" || frames[0].actor != reviewerActor || frames[0].id != question {
		t.Fatalf("the listener's frames = %+v, want the one side question in btw mode from Dispatch", frames)
	}
	reply := agentRequest(t, rig.handler, http.MethodPost, "/api/v1/messages/"+question+"/reply", map[string]any{
		"actor": map[string]any{"kind": "session", "id": heldWriter}, "attempt": 1, "body": agentAnswer,
	}, "agent-token")
	if reply.Code != http.StatusCreated {
		t.Fatalf("the writer's answer: %d %s", reply.Code, reply.Body.String())
	}

	rig.waitRewrite(id, model.RewriteRewritten)
	if got := rig.question(id); got != rewriteBody {
		t.Fatalf("the rewritten ask asks %q, want %q", got, rewriteBody)
	}
	var read bool
	for _, request := range rig.stand.Requests() {
		if strings.Contains(request.Text, rewriteMarker) {
			read = read || (strings.Contains(request.Text, "<writer_answer>\n"+agentAnswer) && strings.Contains(request.Text, "<question_to_writer>"))
		}
	}
	if !read {
		t.Fatal("the rewrite's model call did not read the writer's answer")
	}
	var payload, actor []byte
	if err := rig.database.Pool.QueryRow(context.Background(), `
		select payload, actor from events where type = 'ask.edited' and payload->>'id' = $1 order by id desc limit 1`, id).Scan(&payload, &actor); err != nil {
		t.Fatal(err)
	}
	var edited model.AskEditEventPayload
	if err := json.Unmarshal(payload, &edited); err != nil {
		t.Fatal(err)
	}
	if edited.Rewrite == nil || edited.Rewrite.By != model.RewriteByPerson || edited.Rewrite.AskedBy.ID != "alice" ||
		!edited.Rewrite.AgentAnswer || edited.Rewrite.QuestionMessage != question || !edited.Quiet ||
		edited.Previous.Question != rewriteQuestion || edited.EditedBy != reviewerActor {
		t.Fatalf("the rewrite's ask.edited = rewrite %+v quiet %v previous %q edited by %+v", edited.Rewrite, edited.Quiet, edited.Previous.Question, edited.EditedBy)
	}
	var from model.Actor
	if err := json.Unmarshal(actor, &from); err != nil || from != reviewerActor {
		t.Fatalf("the rewrite's ask.edited is from %s, want Dispatch", actor)
	}
	if loud := countRows(t, rig.database, `
		select count(*) from events where type in ('ask.rewriting', 'ask.edited', 'ask.rewrite_failed') and payload->>'id' = $1 and notify`, id); loud != 0 {
		t.Fatalf("%d of the rewrite's events notify, want none", loud)
	}
	if started := countRows(t, rig.database, `select count(*) from events where type = 'ask.rewriting' and payload->>'id' = $1`, id); started != 1 {
		t.Fatalf("%d ask.rewriting events, want the one run's", started)
	}
	if requested := countRows(t, rig.database, `
		select count(*) from writing_check_events where outcome = 'rewrite_requested' and reader = 'alice' and project = 'RWD'`); requested != 2 {
		t.Fatalf("%d rewrite_requested rows with alice as reader, want one per press", requested)
	}
	// The side question and its answer are kept as one conversation of the writer session's.
	if thread := dispatchRequest(t, rig.handler, http.MethodGet, "/api/v1/messages/"+question, nil, "alice"); thread.Code != http.StatusOK ||
		!strings.Contains(thread.Body.String(), agentAnswer) {
		t.Fatalf("the side question's conversation: %d %s", thread.Code, thread.Body.String())
	}

	for _, target := range []string{
		"/api/v1/asks/" + id, "/api/v1/inbox", "/api/v1/issues/" + rig.issue, "/api/v1/issues/" + rig.issue + "/asks",
	} {
		rewrite := rewriteOf(t, rig.handler, target, "alice", id)
		if rewrite == nil || rewrite.State != model.RewriteRewritten || rewrite.By != model.RewriteByPerson ||
			rewrite.Original.Body != rewriteQuestion || len(rewrite.Original.Options) != 2 {
			t.Errorf("%s carries the ask's rewrite %+v, want the person's rewrite with the original", target, rewrite)
		}
	}

	// An answer to the wording alice was shown before the rewrite is refused.
	answer := dispatchRequest(t, rig.handler, http.MethodPost, "/api/v1/asks/"+id+"/answer", map[string]any{
		"selected": []string{"Ship it"}, "expected_edited_at": nil,
	}, "alice")
	if answer.Code != http.StatusConflict || !strings.Contains(answer.Body.String(), "ASK_EDITED") {
		t.Fatalf("an answer to the old wording: %d %s, want 409 ASK_EDITED", answer.Code, answer.Body.String())
	}
}

// A writer session whose host does not advertise btw gets no side question: the rewrite reads
// the record alone, and its ask.edited says the agent's answer was not read.
func TestARewriteSendsNoSideQuestionToAHostWithoutBtw(t *testing.T) {
	rig := newRewriteRig(t, []string{"steer"})
	id := rig.ask(rewriteQuestion)
	rig.rewrite(id)
	rig.waitRewrite(id, model.RewriteRewritten)
	if frames := rig.listener.frames(); len(frames) != 0 {
		t.Fatalf("the listener was sent %+v, want no side question", frames)
	}
	if asked := countRows(t, rig.database, `select count(*) from messages where author->>'kind' = 'system'`); asked != 0 {
		t.Fatalf("%d side question messages, want none", asked)
	}
	var agentAnswered bool
	if err := rig.database.Pool.QueryRow(context.Background(), `
		select (payload->'rewrite'->>'agent_answer')::boolean from events where type = 'ask.edited' and payload->>'id' = $1`, id).Scan(&agentAnswered); err != nil {
		t.Fatal(err)
	}
	if agentAnswered {
		t.Fatal("the rewrite says it read an answer no side question asked for")
	}
}

// A rewrite's reply writes no option id, and an applied rewrite keeps each option's id all the
// same: the part index keeps an option's part through an edit that rewords it in its place, so
// the ask's read shows the ids it showed before, beside the rewrite's descriptions.
func TestAnAppliedRewriteKeepsTheAsksOptionIDs(t *testing.T) {
	rig := newRewriteRig(t, []string{"steer"})
	id := rig.ask(rewriteQuestion)
	before := rig.options(id)
	rig.rewrite(id)
	rig.waitRewrite(id, model.RewriteRewritten)
	after := rig.options(id)
	ids := func(options []model.AskOption) []string {
		read := make([]string, len(options))
		for i, option := range options {
			read[i] = option.ID
		}
		return read
	}
	if len(before) != 2 || slices.Contains(ids(before), "") || !slices.Equal(ids(after), ids(before)) {
		t.Fatalf("the options read %+v before the rewrite and %+v after, want the same ids", before, after)
	}
	if after[0].Description != "Release it today, with the failing test." || after[1].Description != "Wait until the test passes." {
		t.Fatalf("the rewritten options are %+v, want the rewrite's descriptions", after)
	}
}

// preserveQuestion is a question with every line a rewrite keeps: a short first line, a link of
// each kind, a picture line and a `Ref: ` line. marker tells the stand-in which case it is.
func preserveQuestion(marker string) string {
	return "Finding from tester " + marker + "\nThe chart shows the wait. See https://ci.test/run/7 and [the log](https://ci.test/log/7).\n" +
		"![the wait](https://ci.test/chart.png)\nRef: finding 7"
}

// Each break of what a rewrite keeps leaves the ask as it was: the run fails with the rule's
// reason, a quiet ask.rewrite_failed carries it, and the ask's read carries the failed rewrite.
func TestEachBreakOfWhatARewriteKeepsLeavesTheAskAsItWas(t *testing.T) {
	ship, hold := option("Ship", "Release it."), option("Hold", "Wait.")
	body := func(marker string) string { return preserveQuestion(marker) }
	cases := []struct {
		marker, reason string
		reply          func(t *testing.T, body string) string
	}{
		{"count", "the rewrite has 1 option, and the ask has 2", func(t *testing.T, b string) string { return rewriteJSON(t, b, false, ship) }},
		{"order", `the rewrite puts the options in the order "Hold", "Ship"`, func(t *testing.T, b string) string { return rewriteJSON(t, b, false, hold, ship) }},
		{"labels", `option 2 has the label "Hold it" in the rewrite, and "Hold" in the ask`, func(t *testing.T, b string) string {
			return rewriteJSON(t, b, false, ship, option("Hold it", "Wait."))
		}},
		{"choice", "the rewrite makes the single-choice ask a multiple-choice ask", func(t *testing.T, b string) string { return rewriteJSON(t, b, true, ship, hold) }},
		{"firstline", "the rewrite changes the first line", func(t *testing.T, b string) string {
			return rewriteJSON(t, strings.Replace(b, "Finding from tester", "A finding from the tester", 1), false, ship, hold)
		}},
		{"refline", `the rewrite drops or changes the line "Ref: finding 7"`, func(t *testing.T, b string) string {
			return rewriteJSON(t, strings.Replace(b, "\nRef: finding 7", "", 1), false, ship, hold)
		}},
		{"picture", "the rewrite drops or changes the picture line", func(t *testing.T, b string) string {
			return rewriteJSON(t, strings.Replace(b, "![the wait](https://ci.test/chart.png)", "The chart is at https://ci.test/chart.png.", 1), false, ship, hold)
		}},
		{"links", `the rewrite drops the link "https://ci.test/run/7"`, func(t *testing.T, b string) string {
			return rewriteJSON(t, strings.Replace(b, "See https://ci.test/run/7 and", "See", 1), false, ship, hold)
		}},
	}
	var rules []modeltest.Rule
	for _, c := range cases {
		marker, reply := "case-"+c.marker, c.reply(t, body("case-"+c.marker))
		rules = append(rules, modeltest.Rule{
			Match: func(request modeltest.Request) bool {
				return strings.Contains(request.Text, rewriteMarker) && strings.Contains(request.Text, marker)
			},
			Reply: modeltest.Reply{Text: reply},
		})
	}
	rig := newRewriteRig(t, []string{"steer"}, rules...)
	for _, c := range cases {
		t.Run(c.marker, func(t *testing.T) {
			question := body("case-" + c.marker)
			id := rig.ask(question, map[string]string{"label": "Ship", "description": "Release it."}, map[string]string{"label": "Hold", "description": "Wait."})
			rig.rewrite(id)
			reason := rig.waitRewrite(id, model.RewriteFailed)
			if !strings.Contains(reason, c.reason) {
				t.Fatalf("the failed rewrite's reason is %q, want %q", reason, c.reason)
			}
			if got := rig.question(id); got != question {
				t.Fatalf("the ask asks %q after a rewrite that was not applied", got)
			}
			if failed := countRows(t, rig.database, `
				select count(*) from events where type = 'ask.rewrite_failed' and payload->>'id' = $1 and not notify
					and payload->'rewrite'->>'reason' = $2`, id, reason); failed != 1 {
				t.Fatalf("%d quiet ask.rewrite_failed events carrying the reason, want 1", failed)
			}
			if edits := countRows(t, rig.database, `select count(*) from events where type = 'ask.edited' and payload->>'id' = $1`, id); edits != 0 {
				t.Fatalf("%d ask.edited events of a rewrite not applied, want none", edits)
			}
			if rewrite := rewriteOf(t, rig.handler, "/api/v1/asks/"+id, "alice", id); rewrite == nil ||
				rewrite.State != model.RewriteFailed || rewrite.Reason != reason || rewrite.Original.Body != question {
				t.Fatalf("the ask's read carries %+v, want the failed rewrite with its reason", rewrite)
			}
		})
	}
}

// rewriteDocument is a document whose decision block the person rewrites; holding is the second
// option's line.
func rewriteDocument(holding string) string {
	return "# Rollout\n\nThe plan for the rollout.\n\n:::ask{urgency=\"med\"}\nShip the reviewer?\nWe spin up the node.\n\n" +
		"- Ship it: Release it today.\n- Do not file: " + holding + "\n:::\n"
}

// uploadBlock uploads markdown as the writer and answers the id of the decision block's ask.
func (rig *rewriteRig) uploadBlock(markdown string) string {
	rig.t.Helper()
	response := agentRequest(rig.t, rig.handler, http.MethodPost, "/api/v1/issues/"+rig.issue+"/artifacts", map[string]any{
		"name": "rollout.md", "content": markdown, "actor": map[string]any{"kind": "session", "id": heldWriter},
	}, "agent-token")
	if response.Code != http.StatusCreated {
		rig.t.Fatalf("the document with a decision block: %d %s", response.Code, response.Body.String())
	}
	var askID string
	pollUntil(rig.t, 20*time.Second, "the decision block's ask", func() bool {
		return rig.database.Pool.QueryRow(context.Background(), `
			select id::text from asks where block_id is not null and issue_key = $1`, rig.issue).Scan(&askID) == nil
	})
	return askID
}

// A person's Rewrite of a decision block writes a new version of its document, through the one
// decision-block rewrite path, whose quiet ask.edited holds the original wording.
func TestARewriteOfADecisionBlockWritesANewVersionOfItsDocument(t *testing.T) {
	rig := newRewriteRig(t, []string{"steer"})
	id := rig.uploadBlock(rewriteDocument("Wait."))
	original := rig.question(id)
	versions := func() int64 {
		return countRows(t, rig.database, `select count(*) from artifact_versions v join artifacts a on a.id = v.artifact_id where a.name = 'rollout.md'`)
	}
	before := versions()
	rig.rewrite(id)
	rig.waitRewrite(id, model.RewriteRewritten)
	pollUntil(t, 20*time.Second, "a new version of the document holding the rewrite", func() bool {
		var markdown string
		err := rig.database.Pool.QueryRow(context.Background(), `
			select coalesce(v.markdown, '') from artifact_versions v join artifacts a on a.id = v.artifact_id
			where a.name = 'rollout.md' order by v.number desc limit 1`).Scan(&markdown)
		return err == nil && strings.Contains(markdown, "ship it today or wait for a fix") && versions() > before
	})
	var previous, by, edited string
	var quiet bool
	if err := rig.database.Pool.QueryRow(context.Background(), `
		select payload->'previous'->>'question', payload->'rewrite'->>'by', payload->'edited_by'->>'kind', coalesce((payload->>'quiet')::boolean, false)
		from events where type = 'ask.edited' and payload->>'id' = $1 order by id desc limit 1`, id).Scan(&previous, &by, &edited, &quiet); err != nil {
		t.Fatal(err)
	}
	if previous != original || by != model.RewriteByPerson || edited != "system" || !quiet {
		t.Fatalf("the block's ask.edited: previous %q by %q edited by %q quiet %v", previous, by, edited, quiet)
	}
	if rewrite := rewriteOf(t, rig.handler, "/api/v1/asks/"+id, "alice", id); rewrite == nil || rewrite.State != model.RewriteRewritten || rewrite.Original.Body != original {
		t.Fatalf("the block's read carries %+v, want the rewrite with the original", rewrite)
	}
}

// A decision block's option line that holds more than its plain text is one the document's edit
// path does not rewrite, so a rewrite that changes it is not applied.
func TestARewriteOfADecisionBlockKeepsItsOptionLines(t *testing.T) {
	rig := newRewriteRig(t, []string{"steer"})
	id := rig.uploadBlock(rewriteDocument("see [the memo](https://memo.test)"))
	original := rig.question(id)
	rig.rewrite(id)
	if reason := rig.waitRewrite(id, model.RewriteFailed); !strings.Contains(reason, `"Do not file"`) {
		t.Fatalf("the failed rewrite's reason is %q, want the option line the document keeps", reason)
	}
	if got := rig.question(id); got != original {
		t.Fatalf("the block asks %q after a rewrite that was not applied", got)
	}
}

// A person's Rewrite of a decision block whose question holds a link and a picture reads the
// question as the document holds it, in markdown, and writes the rewrite back as markdown: the
// document's new version keeps the picture and the link, and the row takes the plain text. A
// rewrite that writes the picture as a bare address is not applied, and its reason names the
// picture.
func TestARewriteOfADecisionBlockKeepsItsPicture(t *testing.T) {
	const (
		link    = "[the run](https://ci.test/runs/7)"
		picture = "![the wait](https://ci.test/chart.png)"
		asked   = "Ship the reviewer? See " + link + " " + picture
	)
	document := "# Rollout\n\nThe plan for the rollout.\n\n:::ask{urgency=\"med\"}\n" + asked +
		"\n\n- Ship it: Release it today.\n- Do not file: Wait.\n:::\n"
	options := []map[string]string{option("Ship it", "Release it today."), option("Do not file", "Wait.")}
	for name, test := range map[string]struct {
		body, state, reason string
	}{
		"kept": {body: "Ship it today or wait for a fix? See " + link + " " + picture, state: model.RewriteRewritten},
		"dropped": {
			body: "Ship it today or wait for a fix? See " + link + " https://ci.test/chart.png", state: model.RewriteFailed,
			reason: `it drops the picture "https://ci.test/chart.png"`,
		},
	} {
		t.Run(name, func(t *testing.T) {
			rig := newRewriteRig(t, []string{"steer"}, modeltest.Rule{
				Contains: rewriteMarker, Reply: modeltest.Reply{Text: rewriteJSON(t, test.body, false, options...)},
			})
			id := rig.uploadBlock(document)
			original := rig.question(id)
			rig.rewrite(id)
			reason := rig.waitRewrite(id, test.state)
			if test.state == model.RewriteFailed {
				if !strings.Contains(reason, test.reason) {
					t.Fatalf("the failed rewrite's reason is %q, want it to name the picture", reason)
				}
				if got := rig.question(id); got != original {
					t.Fatalf("the block asks %q after a rewrite that was not applied", got)
				}
				return
			}
			var markdown string
			pollUntil(t, 20*time.Second, "a new version of the document holding the rewrite", func() bool {
				err := rig.database.Pool.QueryRow(context.Background(), `
					select coalesce(v.markdown, '') from artifact_versions v join artifacts a on a.id = v.artifact_id
					where a.name = 'rollout.md' order by v.number desc limit 1`).Scan(&markdown)
				return err == nil && strings.Contains(markdown, "Ship it today or wait for a fix?")
			})
			for _, kept := range []string{link, picture} {
				if !strings.Contains(markdown, kept) {
					t.Fatalf("the new version holds no %q:\n%s", kept, markdown)
				}
			}
			if got := rig.question(id); got != "Ship it today or wait for a fix? See the run" {
				t.Fatalf("the row asks %q, want the plain text of the rewrite", got)
			}
		})
	}
}

// The asking agent's edit while a rewrite runs is never written over: the rewrite fails and the
// ask keeps the agent's words.
func TestARewriteIsNotAppliedOverTheAgentsEdit(t *testing.T) {
	rig := newRewriteRig(t, []string{"steer"}, modeltest.Rule{
		Contains: rewriteMarker, Reply: modeltest.Reply{Text: keptRewrite(t), Delay: 2 * time.Second},
	})
	id := rig.ask(rewriteQuestion)
	rig.rewrite(id)
	pollUntil(t, 10*time.Second, "the rewrite's model call", func() bool {
		for _, request := range rig.stand.Requests() {
			if strings.Contains(request.Text, rewriteMarker) {
				return true
			}
		}
		return false
	})
	agentsEdit := "Ship the reviewer?\nThe node is the test runner, and its first call fails."
	edit := agentRequest(t, rig.handler, http.MethodPatch, "/api/v1/asks/"+id, map[string]any{
		"question": agentsEdit, "actor": map[string]any{"kind": "session", "id": heldWriter},
	}, "agent-token")
	if edit.Code != http.StatusOK {
		t.Fatalf("the agent's edit: %d %s", edit.Code, edit.Body.String())
	}
	if reason := rig.waitRewrite(id, model.RewriteFailed); reason != "the asking agent changed the question while it was rewritten" {
		t.Fatalf("the rewrite failed with %q", reason)
	}
	if got := rig.question(id); got != agentsEdit {
		t.Fatalf("the ask asks %q, want the agent's edit", got)
	}
}

// A Rewrite needs an open question that is no approval request, and the person scope.
func TestARewriteRefusesWhatItDoesNotRewrite(t *testing.T) {
	rig := newRewriteRig(t, []string{"steer"})
	id := rig.ask(rewriteQuestion)
	if response := agentRequest(t, rig.handler, http.MethodPost, "/api/v1/asks/"+id+"/rewrites", map[string]any{}, "agent-token"); response.Code != http.StatusForbidden {
		t.Fatalf("an agent's Rewrite: %d %s, want 403", response.Code, response.Body.String())
	}
	if response := dispatchRequest(t, rig.handler, http.MethodPost, "/api/v1/asks/"+id+"/answer", map[string]any{"selected": []string{"Ship it"}}, "alice"); response.Code != http.StatusOK {
		t.Fatalf("alice's answer: %d %s", response.Code, response.Body.String())
	}
	if response := dispatchRequest(t, rig.handler, http.MethodPost, "/api/v1/asks/"+id+"/rewrites", nil, "alice"); response.Code != http.StatusConflict ||
		!strings.Contains(response.Body.String(), "ASK_NOT_OPEN") {
		t.Fatalf("a Rewrite of an answered ask: %d %s, want 409 ASK_NOT_OPEN", response.Code, response.Body.String())
	}
	if requested := countRows(t, rig.database, `select count(*) from writing_check_events where outcome = 'rewrite_requested'`); requested != 1 {
		t.Fatalf("%d rewrite_requested rows, want the person's one request", requested)
	}
}

// define is alice's Define of words in the ask askID or the document document, which must
// answer 201.
func (rig *rewriteRig) define(login string, body map[string]any) definitionRead {
	rig.t.Helper()
	response := dispatchRequest(rig.t, rig.handler, http.MethodPost, "/api/v1/definitions", body, login)
	if response.Code != http.StatusCreated {
		rig.t.Fatalf("%s's Define: %d %s", login, response.Code, response.Body.String())
	}
	return decodeBody[definitionRead](rig.t, response)
}

// A Define answers at once with the definition read from the record, records the use in the
// person's word records with source define, and sends the live writer session one side question
// as a frame from Dispatch carrying the definition's id. The session's answer is kept with the
// definition and writes no message row and no event; another session's answer is refused.
func TestADefinitionKeepsTheAgentsAnswerWithNoMessageAndNoEvent(t *testing.T) {
	rig := newRewriteRig(t, []string{"btw", "steer"})
	askID := rig.ask(rewriteQuestion + " The store holds it.")
	messagesBefore := countRows(t, rig.database, `select count(*) from messages`)
	eventsBefore := countRows(t, rig.database, `select coalesce(max(id), 0) from events`)
	defined := rig.define("alice", map[string]any{"ask_id": askID, "words": []string{"store"}})
	if len(defined.Definitions) != 1 || defined.Definitions[0].Words != "store" || defined.Agent.State != definitionAgentWaiting ||
		defined.Agent.Session != heldWriter || defined.Person != "alice" || defined.AskID == nil || *defined.AskID != askID {
		t.Fatalf("the definition = %+v, want the record's meaning and a side question waiting on the writer", defined)
	}
	frames := rig.listener.frames()
	if len(frames) != 1 || frames[0].id != defined.ID || frames[0].mode != "btw" || frames[0].actor != reviewerActor || !strings.Contains(frames[0].body, "store") {
		t.Fatalf("the listener's frames = %+v, want one side question carrying the definition's id", frames)
	}
	if other := agentRequest(t, rig.handler, http.MethodPost, "/api/v1/messages/"+defined.ID+"/reply", map[string]any{
		"actor": map[string]any{"kind": "session", "id": "s-other"}, "attempt": 1, "body": "Not mine.",
	}, "agent-token"); other.Code != http.StatusForbidden {
		t.Fatalf("another session's answer: %d %s, want 403", other.Code, other.Body.String())
	}
	answered := agentRequest(t, rig.handler, http.MethodPost, "/api/v1/messages/"+defined.ID+"/reply", map[string]any{
		"actor": map[string]any{"kind": "session", "id": heldWriter}, "attempt": 1, "body": agentAnswer,
	}, "agent-token")
	if answered.Code != http.StatusAccepted || decodeBody[definitionAnswerRead](t, answered).State != definitionAgentAnswered {
		t.Fatalf("the writer's answer: %d %s", answered.Code, answered.Body.String())
	}
	read := dispatchRequest(t, rig.handler, http.MethodGet, "/api/v1/definitions/"+defined.ID, nil, "alice")
	if read.Code != http.StatusOK {
		t.Fatalf("alice's read of her definition: %d %s", read.Code, read.Body.String())
	}
	if got := decodeBody[definitionRead](t, read); got.Agent.State != definitionAgentAnswered || got.Agent.Answer != agentAnswer {
		t.Fatalf("the definition's agent = %+v, want the writer's answer", got.Agent)
	}
	if messages := countRows(t, rig.database, `select count(*) from messages`); messages != messagesBefore {
		t.Fatalf("the definition wrote %d message rows, want none", messages-messagesBefore)
	}
	if count := countRows(t, rig.database, `select count(*) from messages where id = $1::uuid`, defined.ID); count != 0 {
		t.Fatal("the definition's id names a message row")
	}
	if events := countRows(t, rig.database, `select count(*) from events where id > $1`, eventsBefore); events != 0 {
		t.Fatalf("the definition and its answer appended %d events, want none", events)
	}
	if recorded := countRows(t, rig.database, `
		select count(*) from word_questions where source = 'define' and person = 'alice' and words = '{store}'
			and project = 'RWD' and verdict = 'asks_word_meaning' and text_key = 'define:' || $1`, defined.ID); recorded != 1 {
		t.Fatalf("%d word_questions rows of the Define use, want 1 with source define", recorded)
	}
}

// A Define use is counted on its day: GET /api/v1/writing-check/counts for that day gives the
// person define_uses 1 on the ask's project, beside no text and no word question, which count the
// texts the person wrote.
func TestADefineUseIsCountedOnItsDay(t *testing.T) {
	rig := newRewriteRig(t, nil)
	askID := rig.ask(rewriteQuestion + " The store holds it.")
	rig.define("alice", map[string]any{"ask_id": askID, "words": []string{"store"}})
	day := time.Now().UTC().Format(time.DateOnly)
	response := dispatchRequest(t, rig.handler, http.MethodGet, "/api/v1/writing-check/counts?first="+day+"&last="+day, nil, "bob")
	if response.Code != http.StatusOK {
		t.Fatalf("the counts of %s: %d %s", day, response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `"define_uses":1`) {
		t.Fatalf("the counts of %s name no define_uses of 1: %s", day, response.Body.String())
	}
	if got := row(decodeBody[store.WritingCheckCounts](t, response), day, "RWD", "alice"); got.DefineUses != 1 || got.Texts != 0 || got.WordQuestions != 0 {
		t.Fatalf("alice's counts of %s on RWD = %+v, want one Define use and no text", day, got)
	}
}

// Definitions are kept for their person alone: another person reads none of them, by id or in
// their own list, and a Define of words in a document reads the record alone, with no side
// question.
func TestTheDefinitionsRoutesServeNoOtherPerson(t *testing.T) {
	rig := newRewriteRig(t, []string{"btw"})
	var document string
	if err := rig.database.Pool.QueryRow(context.Background(), `select id::text from artifacts where issue_key = $1 and is_primary`, rig.issue).Scan(&document); err != nil {
		t.Fatal(err)
	}
	defined := rig.define("alice", map[string]any{"document": document, "words": []string{"store"}})
	if defined.Agent.State != definitionAgentNone || defined.ArtifactID == nil || defined.Version == nil {
		t.Fatalf("a document's definition = %+v, want the record's alone at a version", defined)
	}
	if frames := rig.listener.frames(); len(frames) != 0 {
		t.Fatalf("a document's definition sent %+v, want no side question", frames)
	}
	if response := dispatchRequest(t, rig.handler, http.MethodGet, "/api/v1/definitions/"+defined.ID, nil, "bob"); response.Code != http.StatusNotFound {
		t.Fatalf("bob's read of alice's definition: %d %s, want 404", response.Code, response.Body.String())
	}
	if response := dispatchRequest(t, rig.handler, http.MethodGet, "/api/v1/me/definitions", nil, "bob"); response.Code != http.StatusOK ||
		len(decodeBody[definitionsPage](t, response).Items) != 0 {
		t.Fatalf("bob's definitions: %d %s, want none", response.Code, response.Body.String())
	}
	mine := dispatchRequest(t, rig.handler, http.MethodGet, "/api/v1/me/definitions", nil, "alice")
	if list := decodeBody[definitionsPage](t, mine); len(list.Items) != 1 || list.Items[0].ID != defined.ID || list.Total != 1 {
		t.Fatalf("alice's definitions = %+v, want hers", list)
	}
	if response := agentRequest(t, rig.handler, http.MethodGet, "/api/v1/me/definitions", nil, "agent-token"); response.Code != http.StatusForbidden {
		t.Fatalf("an agent's read of the definitions: %d, want 403", response.Code)
	}
}

// GET /api/v1/me/definitions pages the caller's definitions, the newest first: limit of them from
// offset, total counting every one, a page past the last one empty with the total still counted,
// and a limit or an offset out of its bounds 400 INVALID_QUERY.
func TestTheCallersDefinitionsArePaged(t *testing.T) {
	rig := newRewriteRig(t, []string{"btw"})
	var document string
	if err := rig.database.Pool.QueryRow(context.Background(), `select id::text from artifacts where issue_key = $1 and is_primary`, rig.issue).Scan(&document); err != nil {
		t.Fatal(err)
	}
	var newestFirst []string
	for _, words := range []string{"store", "node", "lane"} {
		newestFirst = append([]string{rig.define("alice", map[string]any{"document": document, "words": []string{words}}).ID}, newestFirst...)
	}
	page := func(query string) definitionsPage {
		t.Helper()
		response := dispatchRequest(t, rig.handler, http.MethodGet, "/api/v1/me/definitions?"+query, nil, "alice")
		if response.Code != http.StatusOK {
			t.Fatalf("alice's definitions ?%s: %d %s", query, response.Code, response.Body.String())
		}
		return decodeBody[definitionsPage](t, response)
	}
	ids := func(p definitionsPage) []string {
		var listed []string
		for _, read := range p.Items {
			listed = append(listed, read.ID)
		}
		return listed
	}
	if first := page("limit=2"); !slices.Equal(ids(first), newestFirst[:2]) || first.Total != 3 || first.Limit != 2 || first.Offset != 0 {
		t.Errorf("the first page = %v total %d limit %d offset %d, want %v of 3 at limit 2 offset 0", ids(first), first.Total, first.Limit, first.Offset, newestFirst[:2])
	}
	if second := page("limit=2&offset=2"); !slices.Equal(ids(second), newestFirst[2:]) || second.Total != 3 || second.Offset != 2 {
		t.Errorf("the second page = %v total %d offset %d, want %v of 3 at offset 2", ids(second), second.Total, second.Offset, newestFirst[2:])
	}
	if past := page("offset=5"); len(past.Items) != 0 || past.Total != 3 || past.Limit != listDefaultLimit || past.Offset != 5 {
		t.Errorf("a page past the last = %+v, want no item, total 3, limit %d and offset 5", past, listDefaultLimit)
	}
	for _, query := range []string{"limit=0", "limit=201", "offset=-1", "limit=two"} {
		if response := dispatchRequest(t, rig.handler, http.MethodGet, "/api/v1/me/definitions?"+query, nil, "alice"); response.Code != http.StatusBadRequest ||
			!strings.Contains(response.Body.String(), "INVALID_QUERY") {
			t.Errorf("?%s: %d %s, want 400 INVALID_QUERY", query, response.Code, response.Body.String())
		}
	}
}

// An ask, a comment, a message and a new issue the reviewer held, whose writer left them past
// the window, post the reviewer's rewrite, and every read of each carries it as the reviewer's,
// with the held wording as the original and the findings as why; a text nobody rewrote carries
// none.
func TestEveryReadCarriesTheReviewersRewriteOfAHeldText(t *testing.T) {
	rig := newReviewRig(t, ReviewerConfig{
		Asks: ReviewHold, Comments: ReviewHold, Messages: ReviewHold, Issues: ReviewHold,
		WriterWindow: 300 * time.Millisecond, Unedited: UneditedRewrite,
	}, nil)
	ask := rig.held(rig.asWriter(http.MethodPost, "/api/v1/issues/"+rig.issue+"/asks", map[string]any{
		"question": "Ship the reviewer?\n" + vacuousPhrase,
		"options":  []map[string]string{{"label": "Ship it", "description": "Release it today."}, {"label": "Do not file", "description": "Wait."}},
	}), "the ask").ID
	comment := rig.held(rig.asWriter(http.MethodPost, "/api/v1/issues/"+rig.issue+"/comments", map[string]any{
		"body": "Status\nWe spin up the node. " + vacuousPhrase,
	}), "the comment").ID
	message := rig.held(rig.asWriter(http.MethodPost, "/api/v1/issues/"+rig.issue+"/messages", map[string]any{
		"body": "Status\nWe spin up the node. " + vacuousPhrase,
	}), "the message").ID
	issue := rig.held(rig.asWriter(http.MethodPost, "/api/v1/issues", map[string]any{
		"project": "RVW", "title": "Held issue", "spec": vacuousPhrase,
	}), "the issue").ID
	for _, id := range []string{ask, comment, message, issue} {
		rig.waitHeld(id, heldAwaitingWriter)
	}
	time.Sleep(400 * time.Millisecond)
	rig.served.sweepHeld(t.Context())
	for _, id := range []string{ask, comment, message, issue} {
		if posted := rig.waitHeld(id, heldPosted); posted.outcome != reviewRewritePosted {
			t.Fatalf("held text %s posted %s (%s), want rewrite_posted", id, posted.outcome, posted.detail)
		}
	}
	for _, read := range []struct{ target, id, original string }{
		{"/api/v1/asks/" + ask, ask, "Ship the reviewer?\n" + vacuousPhrase},
		{"/api/v1/comments/" + comment, comment, "Status\nWe spin up the node. " + vacuousPhrase},
		{"/api/v1/messages/" + message, message, "Status\nWe spin up the node. " + vacuousPhrase},
		{"/api/v1/issues/" + issue, issue, vacuousPhrase},
		{"/api/v1/issues/" + rig.issue + "/comments", comment, "Status\nWe spin up the node. " + vacuousPhrase},
		{"/api/v1/issues/" + rig.issue + "/messages", message, "Status\nWe spin up the node. " + vacuousPhrase},
	} {
		rewrite := rewriteOf(t, rig.handler, read.target, "alice", read.id)
		if rewrite == nil || rewrite.State != model.RewriteRewritten || rewrite.By != model.RewriteByReviewer ||
			rewrite.Original.Body != read.original || !strings.Contains(rewrite.Reason, vacuousPhrase) {
			t.Errorf("%s carries %+v, want the reviewer's rewrite with the original and its findings", read.target, rewrite)
		}
	}
	if rewrite := rewriteOf(t, rig.handler, "/api/v1/issues/"+issue, "alice", issue); rewrite == nil || rewrite.Original.Title != "Held issue" {
		t.Errorf("the issue's rewrite = %+v, want the original title", rewrite)
	}
	// A text the reviewer did not rewrite carries no rewrite: the rig's own issue.
	if rewrite := rewriteOf(t, rig.handler, "/api/v1/issues/"+rig.issue, "alice", rig.issue); rewrite != nil {
		t.Errorf("an issue nobody rewrote carries %+v", rewrite)
	}
}
