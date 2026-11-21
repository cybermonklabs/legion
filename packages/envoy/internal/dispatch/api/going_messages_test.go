package api

// going_messages_test.go holds a message to an agent as a row of a command-bar plan (R7, check
// c8): the agent's real loop, on a fake chat model, proposes the send; the person approves it; and
// the server posts it on the issue the session works on, or in the session's own conversation for
// a session that works on none (going_messages.go).

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/going/agent/agenttest"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// goingListenedSession is one session the fake listener holds: its title, the delivery modes it
// takes, and whether a send to it is stored for it because it is not running.
type goingListenedSession struct {
	title        string
	capabilities []string
	stopped      bool
}

// goingMessageListener is a fake Envoy listener holding the sessions and the role holders a row
// names, which answers each send to a running session as handed over and each send to a stopped
// one as stored for it.
type goingMessageListener struct {
	mu       sync.Mutex
	sessions map[string]goingListenedSession
	roles    map[string]string
}

// session adds a running session id, titled title, taking the delivery modes capabilities.
func (l *goingMessageListener) session(id, title string, capabilities ...string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sessions[id] = goingListenedSession{title: title, capabilities: capabilities}
}

// stoppedSession adds a session id the listener knows and that is not running.
func (l *goingMessageListener) stoppedSession(id, title string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sessions[id] = goingListenedSession{title: title, capabilities: []string{"aside", "btw", "steer"}, stopped: true}
}

// hold makes session the holder of role.
func (l *goingMessageListener) hold(role, session string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.roles[role] = session
}

func (l *goingMessageListener) record(id string, session goingListenedSession) map[string]any {
	return map[string]any{"session_id": id, "title": session.title, "dir": "/w/legion", "machine_id": "m1", "roles": []string{},
		"capabilities": session.capabilities, "last_seen": 42}
}

func (l *goingMessageListener) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	l.mu.Lock()
	defer l.mu.Unlock()
	write := func(status int, body any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(body)
	}
	switch {
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/registry/"):
		id := strings.TrimPrefix(r.URL.Path, "/v1/registry/")
		session, held := l.sessions[id]
		if !held {
			write(http.StatusNotFound, map[string]any{"error": "no live session " + id})
			return
		}
		write(http.StatusOK, l.record(id, session))
	case r.Method == http.MethodGet && r.URL.Path == "/v1/sessions":
		records := []map[string]any{}
		for id, session := range l.sessions {
			if !session.stopped {
				records = append(records, l.record(id, session))
			}
		}
		write(http.StatusOK, records)
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/roles/"):
		role := strings.TrimPrefix(r.URL.Path, "/v1/roles/")
		holder, held := l.roles[role]
		if !held {
			write(http.StatusNotFound, map[string]any{"error": "no session holds " + role, "reason": "no_holder"})
			return
		}
		session := l.sessions[holder]
		write(http.StatusOK, map[string]any{"role": role, "holder": holder, "holder_state": "live", "title": session.title,
			"capabilities": session.capabilities, "dir": "/w/legion", "machine_id": "m1", "host": "", "last_seen": 42, "claim_revision": 1})
	case r.Method == http.MethodPost && r.URL.Path == "/v1/messages/send":
		var send struct {
			TargetSession string `json:"target_session"`
		}
		if err := json.NewDecoder(r.Body).Decode(&send); err != nil {
			write(http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		state := "connected"
		if l.sessions[send.TargetSession].stopped {
			state = "stopped"
		}
		write(http.StatusOK, map[string]any{"event_id": "envelope-" + send.TargetSession, "recipient": send.TargetSession, "state": state})
	default:
		write(http.StatusNotFound, map[string]any{"error": "not found"})
	}
}

// newGoingMessageRig is the plan rig over a server whose Envoy listener is the fake one it answers.
func newGoingMessageRig(t *testing.T) (goingPlanRig, *goingMessageListener) {
	t.Helper()
	listener := &goingMessageListener{sessions: map[string]goingListenedSession{}, roles: map[string]string{}}
	served := httptest.NewServer(listener)
	t.Cleanup(served.Close)
	agentRig := newGoingAgentRigOver(t, newGoingWritesRigOn(t, served.URL, func(database *store.Store) *store.Store { return database }), nil)
	agentRig.served.deps.Identity = headerIdentity(agentRig.database)
	return goingPlanRig{goingAgentRig: agentRig, loop: newGoingLoop(t, agentRig)}, listener
}

// claimBy makes session the holder of issue's claim, taken ago.
func (rig goingPlanRig) claimBy(t *testing.T, issue, session string, ago time.Duration) {
	t.Helper()
	rig.exec(t, `update issues set claimed_by = jsonb_build_object('kind', 'session', 'id', $2::text), claimed_at = now() - $3::interval
		where key = $1`, issue, session, ago.String())
}

// sendTo is the change that sends body to session in delivery.
func sendTo(session, body, delivery string) map[string]any {
	return goingChange(goingSendToAgent, map[string]any{"session": session, "body": body, "delivery": delivery})
}

// planned scripts words to propose changes and then to answer, starts alice's run of words on
// the page of issue page ("" for none), and answers the run and its plan once it waits for her.
func (rig goingPlanRig) planned(t *testing.T, words, page string, changes ...map[string]any) (string, goingPlanAnswer) {
	t.Helper()
	rig.loop.script.set(words, func(n int) agenttest.Reply {
		if n == 1 {
			return proposing(changes...)
		}
		return answered("Sent.")
	})
	run := rig.begin(t, goingLoopRun{login: "alice", words: words, page: page})
	return run, rig.waiting(t, run)
}

// approved approves run's plan with ticks and answers the plan once the run is done.
func (rig goingPlanRig) approved(t *testing.T, run string, ticks ...map[string]any) goingPlanAnswer {
	t.Helper()
	rig.answer(t, run, "approve", map[string]any{"plan": 1, "rows": append([]map[string]any{}, ticks...)})
	if record := rig.ended(t, run, time.Minute); record.state != "done" {
		t.Fatalf("the run ended %s with %q, want done; its calls were told %q", record.state, record.reason, rig.toolResults(t, run))
	}
	return rig.plan(t, run)
}

// sessionMessages is every conversation of session's own, issue-less, as alice reads them.
func (rig goingPlanRig) sessionMessages(t *testing.T, session string) []model.Message {
	t.Helper()
	response := dispatchRequest(t, rig.mux, http.MethodGet, "/api/v1/agents/"+session+"/messages", nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("GET the messages of %s answered %d %s", session, response.Code, response.Body.String())
	}
	var own []model.Message
	for _, read := range decodeBody[[]messageRead](t, response) {
		if read.Message.IssueKey == nil {
			own = append(own, read.Message.Message)
		}
	}
	return own
}

// posted is the one message of issue whose body is body, failing t unless exactly one is.
func (rig goingPlanRig) posted(t *testing.T, issue, body string) model.Message {
	t.Helper()
	var found []model.Message
	for _, read := range issueMessageRows(t, rig.mux, issue) {
		if read.Message.Body == body {
			found = append(found, read.Message.Message)
		}
	}
	if len(found) != 1 {
		t.Fatalf("%s's conversation holds %d messages %q, want one", issue, len(found), body)
	}
	return found[0]
}

// stored is how many messages hold body anywhere, on an issue or not.
func (rig goingPlanRig) stored(t *testing.T, body string) int {
	t.Helper()
	return rig.count(t, `select count(*) from messages where body = $1`, body)
}

// sendRow is the one row of plan, failing t unless it is a send to session with body in delivery,
// shown on issue.
func sendRow(t *testing.T, plan goingPlanAnswer, issue, session, body, delivery string) goingPlanRow {
	t.Helper()
	if len(plan.Rows) != 1 {
		t.Fatalf("the plan holds %d rows, want the one message", len(plan.Rows))
	}
	row := plan.Rows[0]
	var arguments map[string]any
	if err := json.Unmarshal(row.Arguments, &arguments); err != nil {
		t.Fatalf("the row's arguments %s: %v", row.Arguments, err)
	}
	if row.Tool != goingSendToAgent || row.Class != string(goingClassMessage) || row.Issue != issue ||
		arguments["session"] != session || arguments["body"] != body || arguments["delivery"] != delivery {
		t.Fatalf("the row is %s (%s) on issue %q with %s, want a message to %s on issue %q, %s, with the whole text %q",
			row.Tool, row.Class, row.Issue, row.Arguments, session, issue, delivery, body)
	}
	return row
}

// requireByThePerson fails t unless message's author is alice, acting through the command bar in
// run, and it targets session in delivery.
func requireByThePerson(t *testing.T, message model.Message, run, session, delivery string) {
	t.Helper()
	author := message.Author
	if author.Kind != "user" || author.ID != "alice" || author.Act == nil || author.Act.Agent != model.ActAgentCommandBar || author.Act.Run != run {
		t.Errorf("the message's author is %+v (act %+v), want alice through the command bar in run %s", author, author.Act, run)
	}
	if message.Target == nil || *message.Target != "session:"+session || len(message.Deliveries) == 0 || message.Deliveries[0].Delivery != delivery {
		t.Errorf("the message targets %v with deliveries %+v, want session:%s in %s", message.Target, message.Deliveries, session, delivery)
	}
}

// TestAMessageIsARowOfAPlan (check c8): a message to an agent is one row of a plan that names the
// issue, the session, the delivery and the whole text; nothing is sent before the person approves
// it, and then it is posted on the issue the session works on - the page's, else its newest claim,
// else the issue its route names, a role named by its holder - by the person through the command
// bar, even where the model named the session alone. A session that works on no issue gets it in
// its own conversation, a broadcast is one row the broadcasts list holds, and each row's result
// says whether its message was sent, queued or refused.
func TestAMessageIsARowOfAPlan(t *testing.T) {
	rig, listener := newGoingMessageRig(t)

	t.Run("a message to the agent of an issue is one row and lands on the issue after approval", func(t *testing.T) {
		issue := rig.issue(t, "Worked on by the implementer")
		listener.session("s-impl", "implementer", "aside", "btw", "steer")
		rig.claimBy(t, issue, "s-impl", time.Minute)
		const words = "tell the agent of this issue to rebase on main"
		const body = "Please rebase on main before you push, and say when it is done."
		run, plan := rig.planned(t, words, issue, sendTo("s-impl", body, "aside"))
		row := sendRow(t, plan, issue, "s-impl", body, "aside")
		if row.Claim == nil || row.Claim.ID != "s-impl" {
			t.Errorf("the row shows the claim %+v, want s-impl's", row.Claim)
		}
		if held, anywhere := len(issueMessageRows(t, rig.mux, issue)), rig.stored(t, body); held != 0 || anywhere != 0 {
			t.Fatalf("before approval the issue holds %d messages and %d hold the words, want none", held, anywhere)
		}
		rig.approved(t, run)
		requireByThePerson(t, rig.posted(t, issue, body), run, "s-impl", "aside")
	})

	t.Run("a send to the session alone lands on its issue", func(t *testing.T) {
		issue := rig.issue(t, "Claimed by a solo session")
		listener.session("s-solo", "solo", "aside", "btw", "steer")
		rig.claimBy(t, issue, "s-solo", time.Minute)
		const body = "The flaky test is yours: look at it before the release."
		run, plan := rig.planned(t, "tell s-solo the flaky test is theirs", "", sendTo("s-solo", body, "btw"))
		sendRow(t, plan, issue, "s-solo", body, "btw")
		rig.approved(t, run)
		requireByThePerson(t, rig.posted(t, issue, body), run, "s-solo", "btw")
		if own := rig.sessionMessages(t, "s-solo"); len(own) != 0 {
			t.Errorf("s-solo's own conversation holds %d messages, want none: the send is on its issue", len(own))
		}
	})

	t.Run("a session that works on no issue gets it in its own messages", func(t *testing.T) {
		listener.session("s-free", "free", "aside", "btw", "steer")
		const body = "When you are free, read the release notes."
		run, plan := rig.planned(t, "tell s-free to read the release notes", "", sendTo("s-free", body, "btw"))
		sendRow(t, plan, "", "s-free", body, "btw")
		rig.approved(t, run)
		own := rig.sessionMessages(t, "s-free")
		if len(own) != 1 || own[0].Body != body {
			t.Fatalf("s-free's own conversation holds %+v, want the one message", own)
		}
		requireByThePerson(t, own[0], run, "s-free", "btw")
		if on := rig.count(t, `select count(*) from messages where body = $1 and issue_key is not null`, body); on != 0 {
			t.Errorf("%d issues hold the message, want none", on)
		}
	})

	t.Run("a session the issue's route names", func(t *testing.T) {
		issue := rig.issue(t, "Routed to a session")
		listener.session("s-routed", "routed", "aside", "btw", "steer")
		rig.exec(t, `update issues set route = 'session:s-routed' where key = $1`, issue)
		const body = "This issue is routed to you: start when you can."
		run, plan := rig.planned(t, "tell s-routed to start", "", sendTo("s-routed", body, "btw"))
		sendRow(t, plan, issue, "s-routed", body, "btw")
		rig.approved(t, run)
		requireByThePerson(t, rig.posted(t, issue, body), run, "s-routed", "btw")
	})

	t.Run("a role target resolves to its holder", func(t *testing.T) {
		issue := rig.issue(t, "Reviewed by the role's holder")
		listener.session("s-rev", "reviewer", "aside", "btw", "steer")
		listener.hold("reviewer-demo", "s-rev")
		rig.claimBy(t, issue, "s-rev", time.Minute)
		const body = "Review the suggestion in the spec, please."
		run, plan := rig.planned(t, "ask the reviewer to review the suggestion", "", sendTo("role:reviewer-demo", body, "aside"))
		sendRow(t, plan, issue, "s-rev", body, "aside")
		rig.approved(t, run)
		requireByThePerson(t, rig.posted(t, issue, body), run, "s-rev", "aside")
	})

	t.Run("a session that works on two issues gets it on the page's issue, else on its newest claim", func(t *testing.T) {
		older, newer := rig.issue(t, "Claimed first"), rig.issue(t, "Claimed last")
		listener.session("s-two", "two", "aside", "btw", "steer")
		rig.claimBy(t, older, "s-two", 2*time.Hour)
		rig.claimBy(t, newer, "s-two", time.Hour)

		const paged = "On this page's issue: write down the rota."
		run, plan := rig.planned(t, "tell its agent to write the rota", older, sendTo("s-two", paged, "btw"))
		sendRow(t, plan, older, "s-two", paged, "btw")
		rig.approved(t, run)
		requireByThePerson(t, rig.posted(t, older, paged), run, "s-two", "btw")

		const unpaged = "With no page: close the stale branches."
		run, plan = rig.planned(t, "tell s-two to close the stale branches", "", sendTo("s-two", unpaged, "btw"))
		sendRow(t, plan, newer, "s-two", unpaged, "btw")
		rig.approved(t, run)
		requireByThePerson(t, rig.posted(t, newer, unpaged), run, "s-two", "btw")
	})

	t.Run("the person changes the issue the message goes on", func(t *testing.T) {
		claimed, chosen := rig.issue(t, "Claimed by the moved session"), rig.issue(t, "Chosen by the person")
		listener.session("s-moved", "moved", "aside", "btw", "steer")
		rig.claimBy(t, claimed, "s-moved", time.Minute)
		const body = "Post your notes here, where the person wants them."
		run, plan := rig.planned(t, "tell s-moved to post its notes", "", sendTo("s-moved", body, "btw"))
		sendRow(t, plan, claimed, "s-moved", body, "btw")
		// The chosen issue's claim is another than the one shown, so the row shows it and waits.
		moved := rig.answer(t, run, "approve", map[string]any{"plan": 1, "rows": []map[string]any{
			{"row": 0, "tick": true, "values": map[string]any{"issue": chosen}},
		}})
		if row := moved.Rows[0]; row.State != "waiting" || row.Issue != chosen || row.Claim != nil {
			t.Fatalf("after the change the row is %s on %q with the claim %+v, want it waiting on %s, which nobody holds", row.State, row.Issue, row.Claim, chosen)
		}
		rig.approved(t, run)
		requireByThePerson(t, rig.posted(t, chosen, body), run, "s-moved", "btw")
		if held := len(issueMessageRows(t, rig.mux, claimed)); held != 0 {
			t.Errorf("the issue the session claims holds %d messages, want none", held)
		}
	})

	t.Run("a broadcast is one row, and GET /api/v1/broadcasts holds it", func(t *testing.T) {
		listener.session("s-cast-1", "first listener", "aside", "btw", "steer")
		listener.session("s-cast-2", "second listener", "aside", "btw", "steer")
		const body = "The deploy is paused until 14:00."
		run, plan := rig.planned(t, "tell both listeners the deploy is paused", "",
			goingChange("dispatch_broadcast", map[string]any{"session_ids": []string{"s-cast-1", "s-cast-2"}, "body": body, "delivery": "aside"}))
		if len(plan.Rows) != 1 || plan.Rows[0].Tool != "dispatch_broadcast" {
			t.Fatalf("the plan holds %v, want one broadcast row", plan.Rows)
		}
		broadcasts := func() []broadcastSummary {
			response := dispatchRequest(t, rig.mux, http.MethodGet, "/api/v1/broadcasts", nil, "alice")
			if response.Code != http.StatusOK {
				t.Fatalf("GET /api/v1/broadcasts answered %d %s", response.Code, response.Body.String())
			}
			var held []broadcastSummary
			for _, summary := range decodeBody[[]broadcastSummary](t, response) {
				if summary.Body == body {
					held = append(held, summary)
				}
			}
			return held
		}
		if held := broadcasts(); len(held) != 0 {
			t.Fatalf("before approval the broadcasts hold %+v, want none", held)
		}
		done := rig.approved(t, run)
		held := broadcasts()
		if len(held) != 1 || held[0].Recipients != 2 || held[0].Author.ID != "alice" {
			t.Fatalf("the broadcasts hold %+v, want alice's one broadcast to the two sessions", held)
		}
		if result := done.Rows[0].Result; result == nil || result.Delivery == nil || *result.Delivery != goingDeliveryQueued {
			t.Errorf("the broadcast row's result is %+v, want it queued: its deliveries are sent after its route answered", result)
		}
	})

	t.Run("each row's result says sent, queued or refused", func(t *testing.T) {
		listener.session("s-live", "live", "aside", "btw", "steer")
		listener.stoppedSession("s-stopped", "stopped")
		listener.session("s-btw", "btw only", "btw")
		run, _ := rig.planned(t, "tell the three sessions the build is green", "",
			sendTo("s-live", "The build is green: reached.", "aside"),
			sendTo("s-stopped", "The build is green: stored.", "aside"),
			sendTo("s-btw", "The build is green: refused.", "steer"))
		done := rig.approved(t, run)
		if said, want := rowDeliveries(done), []string{goingDeliverySent, goingDeliveryQueued, goingDeliveryRefused}; !slices.Equal(said, want) {
			t.Errorf("the rows' results say %v, want %v", said, want)
		}
	})

	t.Run("a send whose answer was lost after its commit says what its delivery record says", func(t *testing.T) {
		listener.session("s-lost-live", "lost live", "aside", "btw", "steer")
		listener.stoppedSession("s-lost-stopped", "lost stopped")
		for _, session := range []string{"s-lost-live", "s-lost-stopped"} {
			// The route stores the message and makes its first attempt, and its answer is lost:
			// the call is answered from what its landing recorded, before that attempt.
			rig.hooks.set(t, http.MethodPost, "/api/v1/agents/"+session+"/messages", func(w http.ResponseWriter, r *http.Request, inner http.Handler) {
				inner.ServeHTTP(httptest.NewRecorder(), r)
				writeError(w, "SERVICE_UNAVAILABLE", http.StatusServiceUnavailable, "the send committed and its answer was lost")
			})
		}
		run, _ := rig.planned(t, "tell the two sessions their answers were lost", "",
			sendTo("s-lost-live", "The answer was lost: reached.", "aside"),
			sendTo("s-lost-stopped", "The answer was lost: stored.", "aside"))
		done := rig.approved(t, run)
		if said, want := rowDeliveries(done), []string{goingDeliverySent, goingDeliveryQueued}; !slices.Equal(said, want) {
			t.Errorf("the rows' results say %v, want %v: the stopped session's message is stored for it, not sent", said, want)
		}
	})
}

// rowDeliveries is what each row of plan says became of its message, "nothing" for a row that says
// nothing.
func rowDeliveries(plan goingPlanAnswer) []string {
	said := make([]string, 0, len(plan.Rows))
	for _, row := range plan.Rows {
		if row.Result == nil || row.Result.Delivery == nil {
			said = append(said, "nothing")
			continue
		}
		said = append(said, *row.Result.Delivery)
	}
	return said
}
