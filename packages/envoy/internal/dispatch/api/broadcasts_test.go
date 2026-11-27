package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/testwait"
)

// broadcastListener is a fake Envoy listener for the broadcast tests: it advertises the given
// sessions and records every send it is asked to make.
type broadcastListener struct {
	sync.Mutex
	sends []struct{ target, key, body string }
}

// newBroadcastListener's intercepts run first on every request, in order; one that answers the
// request returns true, and one that returns false lets the fake answer as usual - a test's own
// outage, or a barrier held before the registry read goes through.
func newBroadcastListener(
	t *testing.T, sessionsJSON string, intercepts ...func(http.ResponseWriter, *http.Request) bool,
) (*httptest.Server, *broadcastListener) {
	t.Helper()
	state := &broadcastListener{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, intercept := range intercepts {
			if intercept(w, r) {
				return
			}
		}
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/registry/"):
			writeSessionRecord(t, w, sessionsJSON, strings.TrimPrefix(r.URL.Path, "/v1/registry/"))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sessions":
			_, _ = w.Write([]byte(sessionsJSON))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/messages/send":
			var request struct {
				TargetSession  string `json:"target_session"`
				IdempotencyKey string `json:"idempotency_key"`
				Message        string `json:"message"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Errorf("decode broadcast send: %v", err)
			}
			state.Lock()
			state.sends = append(state.sends, struct{ target, key, body string }{
				request.TargetSession, request.IdempotencyKey, request.Message,
			})
			state.Unlock()
			_, _ = w.Write([]byte(`{"event_id":"envelope-` + request.TargetSession + `","recipient":"` + request.TargetSession + `"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	return server, state
}

func (l *broadcastListener) targets() []string {
	l.Lock()
	defer l.Unlock()
	targets := make([]string, 0, len(l.sends))
	for _, send := range l.sends {
		targets = append(targets, send.target)
	}
	sort.Strings(targets)
	return targets
}

// postBroadcast sends one create as login under a key of its own, as the dashboard sends every
// write; most tests have no interest in which.
func postBroadcast(t *testing.T, handler http.Handler, input map[string]any, login string) *httptest.ResponseRecorder {
	t.Helper()
	return postBroadcastKeyed(t, handler, input, login, uuid.NewString())
}

// postBroadcastKeyed sends one create as login under key.
func postBroadcastKeyed(t *testing.T, handler http.Handler, input map[string]any, login, key string) *httptest.ResponseRecorder {
	t.Helper()
	return keyedRequest(t, handler, http.MethodPost, "/api/v1/broadcasts", input, login, key)
}

// broadcastResponse mirrors the create and read shapes the browser consumes.
type broadcastResponse struct {
	ID         string `json:"id"`
	Body       string `json:"body"`
	Delivery   string `json:"delivery"`
	Recipients []struct {
		SessionID string          `json:"session_id"`
		Message   model.Message   `json:"message"`
		Replies   []model.Message `json:"replies"`
	} `json:"recipients"`
	Excluded []struct {
		SessionID string `json:"session_id"`
		Title     string `json:"title"`
		Reason    string `json:"reason"`
	} `json:"excluded"`
}

const broadcastSessions = `[
	{"session_id":"planner","title":"Planner","capabilities":["btw","steer"],"last_seen":300},
	{"session_id":"tester","title":"Tester","capabilities":["btw","steer"],"last_seen":200},
	{"session_id":"reviewer","title":"Reviewer","capabilities":["aside"],"last_seen":100}
]`

// A broadcast is one message per recipient: each live session that advertises the mode gets a
// targeted message and a delivery attempt of its own, and a selection that mixes capabilities
// excludes the sessions that cannot take the mode rather than switching them to another one.
func TestBroadcastSendsOneMessagePerRecipientAndExcludesTheRest(t *testing.T) {
	t.Parallel()
	listener, sends := newBroadcastListener(t, broadcastSessions)
	handler, database := newTargetedMessageHandler(t, listener.URL)

	response := postBroadcast(t, handler, map[string]any{
		"body":     "Stand down and report status.",
		"delivery": "steer",
		// "planner" twice: a selection that names a session more than once is one recipient.
		"session_ids": []string{"planner", "tester", "reviewer", "ghost", "planner"},
	}, "alice")
	if response.Code != http.StatusCreated {
		t.Fatalf("create broadcast: status=%d body=%s", response.Code, response.Body.String())
	}
	created := decodeBody[broadcastResponse](t, response)
	if len(created.Recipients) != 2 {
		t.Fatalf("recipients = %#v, want planner and tester once each", created.Recipients)
	}
	messageIDs := map[string]struct{}{}
	for _, recipient := range created.Recipients {
		if recipient.Message.Target == nil || *recipient.Message.Target != "session:"+recipient.SessionID {
			t.Fatalf("%s message target = %v", recipient.SessionID, recipient.Message.Target)
		}
		if recipient.Message.Body != "Stand down and report status." {
			t.Fatalf("%s body = %q", recipient.SessionID, recipient.Message.Body)
		}
		messageIDs[recipient.Message.ID] = struct{}{}
		// Each copy is a direct message the broadcast's transaction indexed: its blocks, and no
		// mention.
		if got := messageBlockLayout(t, database, recipient.Message.ID); got != "b1:paragraph:Stand down and report status." {
			t.Fatalf("%s message blocks = %q", recipient.SessionID, got)
		}
		if got := itemBlockRefs(t, database, "message", recipient.Message.ID); len(got) != 0 {
			t.Fatalf("%s message block mentions = %v, want none", recipient.SessionID, got)
		}
		// Its message.created names the sender, as every other direct root's event names its
		// author: the one Inbox an Agent row of this conversation can sit in once the recipient
		// replies. An event naming nobody would send every viewer's Inbox back for a refetch.
		requireThreadLogins(t, database, "message.created", recipient.Message.ID, "alice")
	}
	delivered := awaitBroadcastDeliveries(t, handler, created.ID)
	for _, recipient := range delivered.Recipients {
		attempt := recipient.Message.Deliveries[0]
		if attempt.State != "sent" || attempt.Delivery != "steer" || attempt.SessionID != recipient.SessionID {
			t.Fatalf("%s attempt = %#v, want a steer sent to that session", recipient.SessionID, attempt)
		}
	}
	if len(messageIDs) != 2 {
		t.Fatalf("recipients share a message: %#v", created.Recipients)
	}
	if got := sendTargets(created); !equalStrings(got, []string{"planner", "tester"}) {
		t.Fatalf("recipient sessions = %v", got)
	}
	if got := sends.targets(); !equalStrings(got, []string{"planner", "tester"}) {
		t.Fatalf("listener sends = %v, want one per recipient", got)
	}

	excluded := map[string]string{}
	for _, item := range created.Excluded {
		excluded[item.SessionID] = item.Reason
	}
	if excluded["reviewer"] != "does not advertise steer" || excluded["ghost"] != "no live session" {
		t.Fatalf("excluded = %#v", created.Excluded)
	}
	if len(excluded) != 2 {
		t.Fatalf("excluded = %#v, want the reviewer and the ghost only", created.Excluded)
	}

	read := dispatchRequest(t, handler, http.MethodGet, "/api/v1/broadcasts/"+created.ID, nil, "alice")
	if read.Code != http.StatusOK {
		t.Fatalf("read broadcast: status=%d body=%s", read.Code, read.Body.String())
	}
	stored := decodeBody[broadcastResponse](t, read)
	if stored.Delivery != "steer" || stored.Body != "Stand down and report status." {
		t.Fatalf("stored broadcast = %#v", stored)
	}
	if got := sendTargets(stored); !equalStrings(got, []string{"planner", "tester"}) {
		t.Fatalf("stored recipients = %v", got)
	}
	for _, recipient := range stored.Recipients {
		if len(recipient.Message.Deliveries) != 1 || recipient.Message.Deliveries[0].State != "sent" {
			t.Fatalf("%s stored attempts = %#v", recipient.SessionID, recipient.Message.Deliveries)
		}
	}
}

func sendTargets(response broadcastResponse) []string {
	targets := make([]string, 0, len(response.Recipients))
	for _, recipient := range response.Recipients {
		targets = append(targets, recipient.SessionID)
	}
	sort.Strings(targets)
	return targets
}

func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for index := range got {
		if got[index] != want[index] {
			return false
		}
	}
	return true
}

// LEGION-540. A broadcast's body is the text its author wrote, line breaks and Markdown
// included, in the broadcast read, in each recipient's stored message and on the listener's
// wire: a numbered list that reads as one line on the dashboard is the dashboard's rendering,
// never a body the server collapsed, and every recipient was sent the list.
func TestBroadcastBodyReachesStorageAndTheWireUnchanged(t *testing.T) {
	t.Parallel()
	listener, sends := newBroadcastListener(t, broadcastSessions)
	handler, _ := newTargetedMessageHandler(t, listener.URL)

	body := "Three things:\n\n1. **Stop** the deploy\n2. Check `dispatch-deploy.yml`\n3. Report on dispatch://CORE-1\n"
	created := decodeBody[broadcastResponse](t, postBroadcast(t, handler, map[string]any{
		"body": body, "delivery": "steer", "session_ids": []string{"planner", "tester"},
	}, "alice"))
	if created.Body != body {
		t.Fatalf("created body = %q, want the input unchanged", created.Body)
	}
	delivered := awaitBroadcastDeliveries(t, handler, created.ID)
	if delivered.Body != body {
		t.Fatalf("stored broadcast body = %q, want the input unchanged", delivered.Body)
	}
	for _, recipient := range delivered.Recipients {
		if recipient.Message.Body != body {
			t.Fatalf("%s stored message body = %q, want the input unchanged", recipient.SessionID, recipient.Message.Body)
		}
	}
	sends.Lock()
	defer sends.Unlock()
	if len(sends.sends) != 2 {
		t.Fatalf("listener sends = %d, want one per recipient", len(sends.sends))
	}
	for _, send := range sends.sends {
		if send.body != body {
			t.Fatalf("wire body to %s = %q, want the input unchanged", send.target, send.body)
		}
	}
}

// Each recipient answers its own delivery, and the broadcast view is where every answer is
// read back together.
func TestBroadcastRecipientRepliesAppearInTheBroadcastAndItsSummary(t *testing.T) {
	t.Parallel()
	listener, _ := newBroadcastListener(t, broadcastSessions)
	handler, _ := newTargetedMessageHandler(t, listener.URL)

	created := decodeBody[broadcastResponse](t, postBroadcast(t, handler, map[string]any{
		"body": "Status?", "delivery": "btw", "session_ids": []string{"planner", "tester"},
	}, "alice"))
	var plannerMessage string
	for _, recipient := range awaitBroadcastDeliveries(t, handler, created.ID).Recipients {
		if recipient.SessionID == "planner" {
			plannerMessage = recipient.Message.ID
		}
	}
	reply := bearerRequest(t, handler, http.MethodPost, "/api/v1/messages/"+plannerMessage+"/reply", map[string]any{
		"actor": map[string]any{"kind": "session", "id": "planner"}, "attempt": 1, "body": "Green.",
	})
	if reply.Code != http.StatusCreated {
		t.Fatalf("recipient reply: status=%d body=%s", reply.Code, reply.Body.String())
	}

	stored := decodeBody[broadcastResponse](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/broadcasts/"+created.ID, nil, "alice"))
	answers := map[string]string{}
	for _, recipient := range stored.Recipients {
		for _, answer := range recipient.Replies {
			answers[recipient.SessionID] = answer.Body
		}
	}
	if answers["planner"] != "Green." {
		t.Fatalf("recipient replies = %#v, want the planner's answer under the planner", answers)
	}
	if _, answered := answers["tester"]; answered {
		t.Fatalf("the tester has not answered: %#v", answers)
	}

	// The same recipient answers a second attempt: a retry in another mode opens attempt 2,
	// and a late answer to it must not make the list read two answers from one recipient.
	retry := dispatchRequest(t, handler, http.MethodPost, "/api/v1/messages/"+plannerMessage+"/deliveries", map[string]any{
		"delivery": "steer",
	}, "alice")
	if retry.Code != http.StatusCreated {
		t.Fatalf("retry the planner's delivery: status=%d body=%s", retry.Code, retry.Body.String())
	}
	second := bearerRequest(t, handler, http.MethodPost, "/api/v1/messages/"+plannerMessage+"/reply", map[string]any{
		"actor": map[string]any{"kind": "session", "id": "planner"}, "attempt": 2, "body": "Still green.",
	})
	if second.Code != http.StatusCreated {
		t.Fatalf("answer the second attempt: status=%d body=%s", second.Code, second.Body.String())
	}

	summaries := decodeBody[[]struct {
		ID         string `json:"id"`
		Body       string `json:"body"`
		Delivery   string `json:"delivery"`
		Recipients int    `json:"recipients"`
		Replies    int    `json:"replies"`
	}](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/broadcasts", nil, "alice"))
	if len(summaries) != 1 {
		t.Fatalf("broadcast list = %#v, want the one send", summaries)
	}
	if summaries[0].ID != created.ID || summaries[0].Recipients != 2 || summaries[0].Replies != 1 {
		t.Fatalf("summary = %#v, want two recipients and one of them answered", summaries[0])
	}
}

// TestABroadcastReplyWithNoNodeReachesAnOpenViewerWhereNoProjectIsPrivate (R9): a recipient's reply
// whose message node the graph store has not written yet (a row backfill-graph-store has not
// reached) is read under the one rule of a message on no issue (access.MessageClause): with no
// private project an open viewer reads it, in the broadcast and in its summary's count, as every
// reader did before a project could be private.
//
// Control: origin/main, where the reply's node is not read, passes; a rule that reads a reply with
// no node as nobody's fails, naming the planner's answer.
func TestABroadcastReplyWithNoNodeReachesAnOpenViewerWhereNoProjectIsPrivate(t *testing.T) {
	t.Parallel()
	listener, _ := newBroadcastListener(t, broadcastSessions)
	handler, database := newTargetedMessageHandler(t, listener.URL)
	created := decodeBody[broadcastResponse](t, postBroadcast(t, handler, map[string]any{
		"body": "Status?", "delivery": "btw", "session_ids": []string{"planner"},
	}, "alice"))
	plannerMessage := awaitBroadcastDeliveries(t, handler, created.ID).Recipients[0].Message.ID
	reply := decodeBody[struct {
		ID string `json:"id"`
	}](t, bearerRequest(t, handler, http.MethodPost, "/api/v1/messages/"+plannerMessage+"/reply", map[string]any{
		"actor": map[string]any{"kind": "session", "id": "planner"}, "attempt": 1, "body": "Green.",
	}))
	if reply.ID == "" {
		t.Fatalf("the planner's reply answered no id")
	}
	if tag, err := database.Pool.Exec(t.Context(), `delete from graph_nodes where kind = 'message' and id = $1`, reply.ID); err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("drop the reply's node: %v (%d rows)", err, tag.RowsAffected())
	}

	stored := decodeBody[broadcastResponse](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/broadcasts/"+created.ID, nil, "alice"))
	if len(stored.Recipients) != 1 || len(stored.Recipients[0].Replies) != 1 || stored.Recipients[0].Replies[0].Body != "Green." {
		t.Errorf("GET /api/v1/broadcasts/{id} as alice = %#v, want the planner's answer, whose node is not written yet", stored.Recipients)
	}
	summaries := decodeBody[[]struct {
		Replies int `json:"replies"`
	}](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/broadcasts", nil, "alice"))
	if len(summaries) != 1 || summaries[0].Replies != 1 {
		t.Errorf("GET /api/v1/broadcasts as alice = %#v, want one broadcast counting the planner's answer", summaries)
	}
}

// Every message read names the broadcast that sent the message, and says null for a message no
// broadcast sent, so a session reading a direct message back can tell a person's own message to
// it from one recipient's copy of a broadcast: the thread read a session confirms a delivery
// with, the Agents page's conversation list, and each reply in a thread.
func TestMessageReadsNameTheBroadcastThatSentThem(t *testing.T) {
	t.Parallel()
	listener, _ := newBroadcastListener(t, broadcastSessions)
	handler, _ := newTargetedMessageHandler(t, listener.URL)

	created := decodeBody[broadcastResponse](t, postBroadcast(t, handler, map[string]any{
		"body": "Status?", "delivery": "steer", "session_ids": []string{"planner"},
	}, "alice"))
	broadcastMessage := awaitBroadcastDeliveries(t, handler, created.ID).Recipients[0].Message.ID
	direct := postAgentMessage(t, handler, "planner", map[string]any{
		"body": "Where is the dashboard?", "delivery": "steer",
	}, "alice")
	if direct.Code != http.StatusCreated {
		t.Fatalf("direct message: status=%d body=%s", direct.Code, direct.Body.String())
	}
	directMessage := decodeBody[model.Message](t, direct).ID
	if reply := bearerRequest(t, handler, http.MethodPost, "/api/v1/messages/"+broadcastMessage+"/reply", map[string]any{
		"actor": map[string]any{"kind": "session", "id": "planner"}, "attempt": 1, "body": "Green.",
	}); reply.Code != http.StatusCreated {
		t.Fatalf("recipient reply: status=%d body=%s", reply.Code, reply.Body.String())
	}

	type rawRead struct {
		Message map[string]json.RawMessage   `json:"message"`
		Replies []map[string]json.RawMessage `json:"replies"`
	}
	broadcastID := `"` + created.ID + `"`
	threadRead := func(id string) rawRead {
		t.Helper()
		read := bearerRequest(t, handler, http.MethodGet, "/api/v1/messages/"+id+"?session=planner", nil)
		if read.Code != http.StatusOK {
			t.Fatalf("GET /api/v1/messages/%s: status=%d body=%s", id, read.Code, read.Body.String())
		}
		return decodeBody[rawRead](t, read)
	}
	fromBroadcast := threadRead(broadcastMessage)
	if got := string(fromBroadcast.Message["broadcast_id"]); got != broadcastID {
		t.Fatalf("a broadcast recipient's message reads broadcast_id %s, want %s", got, broadcastID)
	}
	if len(fromBroadcast.Replies) != 1 || string(fromBroadcast.Replies[0]["broadcast_id"]) != "null" {
		t.Fatalf("the session's reply in a broadcast thread = %v, want one reply with broadcast_id null", fromBroadcast.Replies)
	}
	if got, present := threadRead(directMessage).Message["broadcast_id"]; !present || string(got) != "null" {
		t.Fatalf("a direct message reads broadcast_id %q (present %v), want null", got, present)
	}

	listed := dispatchRequest(t, handler, http.MethodGet, "/api/v1/agents/planner/messages", nil, "alice")
	if listed.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/agents/planner/messages: status=%d body=%s", listed.Code, listed.Body.String())
	}
	byID := map[string]string{}
	for _, read := range decodeBody[[]rawRead](t, listed) {
		var id string
		if err := json.Unmarshal(read.Message["id"], &id); err != nil {
			t.Fatalf("decode a listed conversation's id: %v", err)
		}
		byID[id] = string(read.Message["broadcast_id"])
	}
	if byID[broadcastMessage] != broadcastID || byID[directMessage] != "null" {
		t.Fatalf("the Agents page's conversations read broadcast_id %v, want %s for the broadcast and null for the direct message", byID, broadcastID)
	}
}

// Nothing is written when no selected session can take the mode: the sender is told why
// rather than left with an empty broadcast to explain.
func TestBroadcastWithNoReachableRecipientIsRefusedAndWritesNothing(t *testing.T) {
	t.Parallel()
	listener, sends := newBroadcastListener(t, broadcastSessions)
	handler, _ := newTargetedMessageHandler(t, listener.URL)

	response := postBroadcast(t, handler, map[string]any{
		"body": "Anyone?", "delivery": "steer", "session_ids": []string{"reviewer", "ghost"},
	}, "alice")
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), `"code":"BROADCAST_EMPTY"`) {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if body := response.Body.String(); !strings.Contains(body, "Reviewer does not advertise steer") ||
		!strings.Contains(body, "ghost no live session") {
		t.Fatalf("the refusal must name each reason: %s", body)
	}
	if got := sends.targets(); len(got) != 0 {
		t.Fatalf("listener sends = %v, want none", got)
	}
	if summaries := decodeBody[[]json.RawMessage](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/broadcasts", nil, "alice")); len(summaries) != 0 {
		t.Fatalf("broadcast list = %v, want nothing written", summaries)
	}
}

// A broadcast leaves out a session its sender may not see exactly as one nobody holds, and asks the
// listener nothing about it. alice, an open viewer who does not see PRIV, sends to privateSession,
// which has acted in PRIV, beside s-pub, which she sees, and then to s-nobody, which nobody holds,
// beside s-pub: each send reaches s-pub alone and leaves the other out "no live session" with no
// title, and each reads s-pub's record from the listener and no other. The walk's listener holds
// privateSession live, so a send that asked it would have reached it.
func TestABroadcastLeavesOutASessionItsSenderMayNotSeeAsOneNobodyHolds(t *testing.T) {
	t.Parallel()
	walk := seedHiddenWrites(t)
	type exclusion struct{ SessionID, Title, Reason string }
	send := func(session string) {
		t.Helper()
		answer, _, calls := walk.recorded(t, callerAlice, http.MethodPost, "/api/v1/broadcasts", map[string]any{
			"body": "To walked sessions.", "delivery": "btw", "session_ids": []string{session, "s-pub"},
		})
		if answer.Code != http.StatusCreated {
			t.Fatalf("alice broadcasts to %s and s-pub: %d %s, want 201", session, answer.Code, strings.TrimSpace(answer.Body.String()))
		}
		sent := decodeBody[broadcastResponse](t, answer)
		excluded := []exclusion{}
		for _, left := range sent.Excluded {
			excluded = append(excluded, exclusion{left.SessionID, left.Title, left.Reason})
		}
		if want := []exclusion{{session, "", "no live session"}}; !slices.Equal(excluded, want) || !slices.Equal(sendTargets(sent), []string{"s-pub"}) {
			t.Errorf("alice's broadcast to %s and s-pub reaches %v and leaves out %+v; want s-pub reached and %+v", session, sendTargets(sent), excluded, want)
		}
		// The send's delivery runs behind its answer, so only the record reads it made before the
		// answer are its own here.
		reads := slices.DeleteFunc(calls, func(call string) bool { return !strings.HasPrefix(call, "GET /v1/registry/") })
		slices.Sort(reads)
		if want := []string{"GET /v1/registry/s-pub?include=unresponsive,stopped"}; !slices.Equal(reads, want) {
			t.Errorf("alice's broadcast to %s and s-pub read the listener's records %v, want %v", session, reads, want)
		}
		awaitBackgroundWrites(t, walk.server)
	}
	send(privateSession)
	send("s-nobody")
}

// A viewer that is not open - a restricted member, and his token - reads its own direct messages
// alone (R4), so a broadcast is one to it only when its person sent it. bob, a restricted member
// PRIV's list names, and bob's token that lists PRIV see session-priv, which has acted in PRIV, and
// are no party to carol's broadcast to it: the read GET /api/v1/broadcasts/{id} and a repeat of the
// send make (readBroadcast) finds no broadcast for either, as for an id nobody holds. carol reads
// it, and so does carol's token that lists PRIV, a full member's credential and so open. Both GET
// routes refuse a viewer that is not open before they read anything (deploymentReadRefusal), so the
// rule is read here, beneath that gate.
func TestABroadcastIsReadByAViewerThatIsNotOpenOnlyAsItsAuthors(t *testing.T) {
	t.Parallel()
	listener, _ := newBroadcastListener(t, privateRoster)
	seed := seedPrivateProjectWith(t, testServerOptions{envoyURL: listener.URL})
	seedSessionWork(t, seed)
	sent := keyedRequest(t, seed.handler, http.MethodPost, "/api/v1/broadcasts", map[string]any{
		"body": "Status?", "delivery": "btw", "session_ids": []string{sessionOfPrivate},
	}, seedCarol, uuid.NewString())
	if sent.Code != http.StatusCreated {
		t.Fatalf("carol broadcasts to %s: %d %s", sessionOfPrivate, sent.Code, sent.Body.String())
	}
	id := decodeBody[broadcastResponse](t, sent).ID
	for _, test := range []struct {
		caller privateCaller
		reads  bool
	}{
		{callerBob, false},
		{callerBobPrivToken, false},
		{callerCarol, true},
		{callerCarolPrivToken, true},
	} {
		recorder := httptest.NewRecorder()
		reader, ok := seed.server.readerOf(recorder, seed.readerRequest(t, test.caller))
		if !ok {
			t.Fatalf("resolve %s: %d %s", test.caller, recorder.Code, recorder.Body.String())
		}
		// What makes the row able to fail: the caller sees every recipient, so only the rule of
		// direct messages keeps the broadcast from it.
		if hidden, err := seed.server.sessionHiddenFrom(t.Context(), reader, sessionOfPrivate); err != nil || hidden {
			t.Fatalf("%s may not see %s (%v): the row holds the broadcast from no caller that sees its recipient", test.caller, sessionOfPrivate, err)
		}
		read, err := seed.server.readBroadcast(t.Context(), reader, id, false)
		switch {
		case test.reads && (err != nil || read.ID != id || len(read.Recipients) != 1):
			t.Errorf("%s reads carol's broadcast to %s as %+v, %v; want it, with its one recipient", test.caller, sessionOfPrivate, read, err)
		case !test.reads && !errors.Is(err, pgx.ErrNoRows):
			t.Errorf("%s, no party to carol's broadcast to %s, reads it as %+v, %v; want a broadcast nobody sent (pgx.ErrNoRows)", test.caller, sessionOfPrivate, read, err)
		}
	}
}

// A credential sees what its own list lets it see, not all its person sees (R3), so it reads its
// person's broadcast only where it may see every recipient. carol's token that lists no project sees
// the public projects alone and not session-priv, which has acted in PRIV: GET /api/v1/broadcasts
// leaves carol's broadcast to session-priv out, and GET /api/v1/broadcasts/{id} answers it in the
// bytes an id nobody holds gets. carol, at her browser, sees PRIV and reads it on both routes, and
// so does her token that lists PRIV, an open viewer that sees PRIV and session-priv with it.
func TestACredentialReadsItsPersonsBroadcastOnlyWhereItSeesEveryRecipient(t *testing.T) {
	t.Parallel()
	listener, _ := newBroadcastListener(t, privateRoster)
	seed := seedPrivateProjectWith(t, testServerOptions{envoyURL: listener.URL})
	seedSessionWork(t, seed)
	sent := keyedRequest(t, seed.handler, http.MethodPost, "/api/v1/broadcasts", map[string]any{
		"body": "Status?", "delivery": "btw", "session_ids": []string{sessionOfPrivate},
	}, seedCarol, uuid.NewString())
	if sent.Code != http.StatusCreated {
		t.Fatalf("carol broadcasts to %s: %d %s", sessionOfPrivate, sent.Code, sent.Body.String())
	}
	id := decodeBody[broadcastResponse](t, sent).ID
	reader := func(caller privateCaller) model.Actor {
		t.Helper()
		recorder := httptest.NewRecorder()
		resolved, ok := seed.server.readerOf(recorder, seed.readerRequest(t, caller))
		if !ok {
			t.Fatalf("resolve %s: %d %s", caller, recorder.Code, recorder.Body.String())
		}
		return resolved
	}
	// What makes the token's rows able to fail: it acts for carol, who sent the broadcast, and
	// the one recipient is a session it may not see.
	if hidden, err := seed.server.sessionHiddenFrom(t.Context(), reader(callerCarolToken), sessionOfPrivate); err != nil || !hidden {
		t.Fatalf("%s sees %s (%v): the row holds the author's broadcast from no credential that may not see its recipient", callerCarolToken, sessionOfPrivate, err)
	}
	listed := func(caller privateCaller) bool {
		t.Helper()
		return slices.ContainsFunc(decodeBody[[]broadcastSummary](t, seed.mustRequest(t, caller, http.MethodGet, "/api/v1/broadcasts", nil, http.StatusOK)),
			func(summary broadcastSummary) bool { return summary.ID == id })
	}
	missing := seed.request(t, callerCarolToken, http.MethodGet, "/api/v1/broadcasts/"+uuid.NewString(), nil)
	if read := seed.request(t, callerCarolToken, http.MethodGet, "/api/v1/broadcasts/"+id, nil); read.Code != missing.Code || read.Body.String() != missing.Body.String() {
		t.Errorf("%s: GET /api/v1/broadcasts/%s = %d %s, want what an id nobody holds answers: %d %s", callerCarolToken, id,
			read.Code, strings.TrimSpace(read.Body.String()), missing.Code, strings.TrimSpace(missing.Body.String()))
	}
	if listed(callerCarolToken) {
		t.Errorf("%s: GET /api/v1/broadcasts lists carol's broadcast to %s, which it may not see", callerCarolToken, sessionOfPrivate)
	}
	if !listed(callerCarol) {
		t.Errorf("carol: GET /api/v1/broadcasts leaves out her broadcast to %s", sessionOfPrivate)
	}
	seed.mustRequest(t, callerCarol, http.MethodGet, "/api/v1/broadcasts/"+id, nil, http.StatusOK)
	if !listed(callerCarolPrivToken) {
		t.Errorf("%s: GET /api/v1/broadcasts leaves out carol's broadcast to %s, which it sees", callerCarolPrivToken, sessionOfPrivate)
	}
	seed.mustRequest(t, callerCarolPrivToken, http.MethodGet, "/api/v1/broadcasts/"+id, nil, http.StatusOK)
	if read, err := seed.server.readBroadcast(t.Context(), reader(callerCarolPrivToken), id, false); err != nil || read.ID != id {
		t.Errorf("%s reads carol's broadcast to %s as %+v, %v; want it", callerCarolPrivToken, sessionOfPrivate, read, err)
	}
}

// With no private project every full member sees every session, so an author reads her broadcast
// as she did before a project could be private (R9): with PRIV made public, carol's token that
// lists no project lists carol's broadcast to session-priv and reads it, as carol does.
func TestWithNoPrivateProjectAnAuthorReadsHerBroadcastAsToday(t *testing.T) {
	t.Parallel()
	listener, _ := newBroadcastListener(t, privateRoster)
	seed := seedPrivateProjectWith(t, testServerOptions{envoyURL: listener.URL})
	seedSessionWork(t, seed)
	sent := keyedRequest(t, seed.handler, http.MethodPost, "/api/v1/broadcasts", map[string]any{
		"body": "Status?", "delivery": "btw", "session_ids": []string{sessionOfPrivate},
	}, seedCarol, uuid.NewString())
	if sent.Code != http.StatusCreated {
		t.Fatalf("carol broadcasts to %s: %d %s", sessionOfPrivate, sent.Code, sent.Body.String())
	}
	id := decodeBody[broadcastResponse](t, sent).ID
	seed.setVisibility(t, seedPrivate, model.ProjectPublic)
	for _, caller := range []privateCaller{callerCarolToken, callerCarol} {
		summaries := decodeBody[[]broadcastSummary](t, seed.mustRequest(t, caller, http.MethodGet, "/api/v1/broadcasts", nil, http.StatusOK))
		if !slices.ContainsFunc(summaries, func(summary broadcastSummary) bool { return summary.ID == id }) {
			t.Errorf("PRIV public: %s: GET /api/v1/broadcasts leaves out carol's broadcast to %s", caller, sessionOfPrivate)
		}
		if read := decodeBody[broadcastResponse](t, seed.mustRequest(t, caller, http.MethodGet, "/api/v1/broadcasts/"+id, nil, http.StatusOK)); !slices.Equal(sendTargets(read), []string{sessionOfPrivate}) {
			t.Errorf("PRIV public: %s reads carol's broadcast as sent to %v, want [%s]", caller, sendTargets(read), sessionOfPrivate)
		}
	}
}

// INFRA-249. The live list is the listener's in-memory cache of the registry, and a session
// working through a cache refill is missing from it. A send asks about each selected session
// by id instead, so a selection the list has dropped entirely still reaches every session the
// store holds a record for, with nothing excluded.
func TestBroadcastReachesSessionsTheLiveListNoLongerHolds(t *testing.T) {
	t.Parallel()
	listener, sends := newBroadcastListener(t, broadcastSessions, func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/sessions" {
			return false
		}
		// The listener's cache holds nobody: every recipient is absent from the list, and
		// present in the store.
		_, _ = w.Write([]byte(`[]`))
		return true
	})
	handler, _ := newTargetedMessageHandler(t, listener.URL)

	response := postBroadcast(t, handler, map[string]any{
		"body": "Status?", "delivery": "steer", "session_ids": []string{"planner", "tester"},
	}, "alice")
	if response.Code != http.StatusCreated {
		t.Fatalf("create broadcast: status=%d body=%s", response.Code, response.Body.String())
	}
	created := decodeBody[broadcastResponse](t, response)
	if got := sendTargets(created); !equalStrings(got, []string{"planner", "tester"}) {
		t.Fatalf("recipients = %v, want both sessions the store holds", got)
	}
	if len(created.Excluded) != 0 {
		t.Fatalf("excluded = %#v, want nothing excluded for a list that holds neither", created.Excluded)
	}
	awaitBroadcastDeliveries(t, handler, created.ID)
	if got := sends.targets(); !equalStrings(got, []string{"planner", "tester"}) {
		t.Fatalf("listener sends = %v, want one frame per recipient", got)
	}
}

// INFRA-249. A per-session read the listener did not answer is not an absence: the send is
// refused and nothing is written, not even the recipients whose reads answered. A broadcast
// commits under its idempotency key, and a replay of that key is answered "excluded by the
// original send" for every session the original missed, so a recipient dropped on a transient
// failure would be unreachable under that key for good - and the key itself must stay free, so
// the sender's retry is a fresh send rather than a replay of a broadcast that never happened.
func TestBroadcastWhoseSessionReadDoesNotAnswerWritesNothingAndLeavesItsKeyFree(t *testing.T) {
	t.Parallel()
	var refusing atomic.Bool
	refusing.Store(true)
	listener, sends := newBroadcastListener(t, broadcastSessions, func(w http.ResponseWriter, r *http.Request) bool {
		if !refusing.Load() || r.URL.Path != "/v1/registry/tester" {
			return false
		}
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":"envoy: the message bus did not answer: no responders"}`))
		return true
	})
	handler, database := newTargetedMessageHandler(t, listener.URL)

	send := func() *httptest.ResponseRecorder {
		t.Helper()
		return postBroadcastKeyed(t, handler, map[string]any{
			"body": "Status?", "delivery": "steer", "session_ids": []string{"planner", "tester"},
		}, "alice", "one-read-did-not-answer")
	}

	refused := send()
	if refused.Code != http.StatusServiceUnavailable ||
		!strings.Contains(refused.Body.String(), `"code":"ENVOY_UNAVAILABLE"`) {
		t.Fatalf("a send whose recipient read did not answer: status=%d body=%s, want 503 ENVOY_UNAVAILABLE",
			refused.Code, refused.Body.String())
	}
	if listed := broadcastCount(t, handler); listed != 0 {
		t.Fatalf("the broadcast list holds %d broadcasts, want nothing written", listed)
	}
	if messages := countMessages(t, database); messages != 0 {
		t.Fatalf("the database holds %d messages, want nothing written", messages)
	}
	if got := sends.targets(); len(got) != 0 {
		t.Fatalf("listener sends = %v, want none", got)
	}

	// The key is still the sender's to use: the same send, once the listener answers again,
	// is a broadcast rather than a replay of one that was never written.
	refusing.Store(false)
	retried := send()
	if retried.Code != http.StatusCreated {
		t.Fatalf("the same send once the listener answers: status=%d body=%s, want 201 under a free key",
			retried.Code, retried.Body.String())
	}
	created := decodeBody[broadcastResponse](t, retried)
	if got := sendTargets(created); !equalStrings(got, []string{"planner", "tester"}) {
		t.Fatalf("the retry's recipients = %v, want both selected sessions", got)
	}
	if len(created.Excluded) != 0 {
		t.Fatalf("the retry's exclusions = %#v, want the refused read to have left none behind", created.Excluded)
	}
	awaitBroadcastDeliveries(t, handler, created.ID)
}

// INFRA-249. A send answers only once it has decided every selected session. The resolution's
// budget can end after every read it launched has answered and before it launches the next,
// and a read that was never made returns no error; taken for an absence, each unread session
// would be excluded "no live session" and, with one recipient left, committed under the send's
// idempotency key, where every retry of that key is told the session was excluded by the
// original send. So a send whose budget ends first is refused, nothing is written, and the key
// is still free for the same send once the budget allows it.
func TestBroadcastWhoseBudgetEndsBeforeEverySessionIsReadWritesNothingAndLeavesItsKeyFree(t *testing.T) {
	t.Parallel()
	listener, sends := newBroadcastListener(t, broadcastSessions)
	handler, database, deps := newTestServer(t, testServerOptions{envoyURL: listener.URL})
	// A nanosecond has ended by the time the resolution first looks at its budget, so it
	// launches no read and no read fails: the count of decided sessions is all that stands
	// between this send and a selection it never read. A budget of a millisecond would race the
	// stub's answers, and a read cut off in flight is refused on its own error, whether or not
	// the count is checked.
	expired := &server{deps: deps, lifetime: t.Context(), broadcastResolveBudget: time.Nanosecond}
	send := func(handler http.Handler) *httptest.ResponseRecorder {
		t.Helper()
		return postBroadcastKeyed(t, handler, map[string]any{
			"body": "Status?", "delivery": "steer", "session_ids": []string{"planner", "tester"},
		}, "alice", "budget-ended-before-every-read")
	}
	// The send is made on the bare handler, so it carries the reader the route's project check
	// puts in its context (withinProjectScope): a handler handed no reader reads every session
	// as hidden (sessionsHiddenFromReader), and its send would be refused for that, never for its
	// budget.
	routed := func(w http.ResponseWriter, r *http.Request) {
		reader, ok := expired.readerOf(w, r)
		if !ok {
			return
		}
		expired.createBroadcast(w, r.WithContext(withReader(r.Context(), reader)))
	}

	refused := send(http.HandlerFunc(routed))
	if refused.Code != http.StatusServiceUnavailable ||
		!strings.Contains(refused.Body.String(), `"code":"ENVOY_UNAVAILABLE"`) {
		t.Fatalf("a send whose budget ended before every session was read: status=%d body=%s, want 503 ENVOY_UNAVAILABLE",
			refused.Code, refused.Body.String())
	}
	if listed := broadcastCount(t, handler); listed != 0 {
		t.Fatalf("the broadcast list holds %d broadcasts, want nothing written", listed)
	}
	if messages := countMessages(t, database); messages != 0 {
		t.Fatalf("the database holds %d messages, want nothing written", messages)
	}
	if got := sends.targets(); len(got) != 0 {
		t.Fatalf("listener sends = %v, want none", got)
	}

	// The key is still the sender's to use: the same send under the deployment's budget is a
	// broadcast to both sessions rather than a replay that excludes the ones never read.
	retried := send(handler)
	if retried.Code != http.StatusCreated {
		t.Fatalf("the same send under the deployment's budget: status=%d body=%s, want 201 under a free key",
			retried.Code, retried.Body.String())
	}
	created := decodeBody[broadcastResponse](t, retried)
	if got := sendTargets(created); !equalStrings(got, []string{"planner", "tester"}) {
		t.Fatalf("the retry's recipients = %v, want both selected sessions", got)
	}
	if len(created.Excluded) != 0 {
		t.Fatalf("the retry's exclusions = %#v, want the refused send to have left none behind", created.Excluded)
	}
	awaitBroadcastDeliveries(t, handler, created.ID)
}

// Broadcasting needs the person scope, like the one-session route it is built from, and so does
// reading one back: the deployment's shared token holds no scope and is turned away from all
// three rows, and the person's own token reads the list her browser reads.
func TestBroadcastRoutesRefuseACredentialWithoutThePersonScope(t *testing.T) {
	t.Parallel()
	listener, _ := newBroadcastListener(t, broadcastSessions)
	handler, database := newTargetedMessageHandler(t, listener.URL)

	created := decodeBody[broadcastResponse](t, postBroadcast(t, handler, map[string]any{
		"body": "Status?", "delivery": "btw", "session_ids": []string{"planner"},
	}, "alice"))
	// The send delivers behind its 201: settled before the test ends, it is never cut off
	// mid-send by the listener closing.
	awaitBroadcastDeliveries(t, handler, created.ID)

	for name, request := range map[string]struct {
		method string
		path   string
		body   any
	}{
		"create": {http.MethodPost, "/api/v1/broadcasts", map[string]any{
			"body": "From a session", "delivery": "btw", "session_ids": []string{"planner"},
			"actor": map[string]string{"kind": "session", "id": "tester"},
		}},
		"list": {http.MethodGet, "/api/v1/broadcasts", nil},
		"read": {http.MethodGet, "/api/v1/broadcasts/" + created.ID, nil},
	} {
		t.Run(name, func(t *testing.T) {
			response := bearerRequest(t, handler, request.method, request.path, request.body)
			if response.Code != http.StatusForbidden || !strings.Contains(response.Body.String(), `"code":"`+codeScopeRequired+`"`) {
				t.Fatalf("status=%d body=%s, want 403 %s", response.Code, response.Body.String(), codeScopeRequired)
			}
		})
	}
	own := agentRequest(t, handler, http.MethodGet, "/api/v1/broadcasts", nil,
		personalTokenHolding(t, database, model.ScopePerson))
	if own.Code != http.StatusOK {
		t.Fatalf("Alice's own token listing her broadcasts: status=%d body=%s", own.Code, own.Body.String())
	}
	if listed := decodeBody[[]struct {
		ID string `json:"id"`
	}](t, own); len(listed) != 1 || listed[0].ID != created.ID {
		t.Fatalf("Alice's own token listed %#v, want the one send her browser lists", listed)
	}
}

func TestBroadcastRejectsMalformedInput(t *testing.T) {
	t.Parallel()
	listener, _ := newBroadcastListener(t, broadcastSessions)
	handler, _ := newTargetedMessageHandler(t, listener.URL)

	tooMany := make([]string, contracts.MaxBroadcastRecipients+1)
	for index := range tooMany {
		tooMany[index] = "session-" + string(rune('a'+index%26)) + string(rune('a'+index/26))
	}
	for name, input := range map[string]struct {
		body map[string]any
		code string
	}{
		"empty body":   {map[string]any{"body": "   ", "delivery": "btw", "session_ids": []string{"planner"}}, "INVALID_MESSAGE"},
		"unknown mode": {map[string]any{"body": "hi", "delivery": "shout", "session_ids": []string{"planner"}}, "BROADCAST_INPUT"},
		"no sessions":  {map[string]any{"body": "hi", "delivery": "btw", "session_ids": []string{" "}}, "BROADCAST_INPUT"},
		"over the cap": {map[string]any{"body": "hi", "delivery": "btw", "session_ids": tooMany}, "BROADCAST_INPUT"},
		"over the block bound": {
			map[string]any{"body": strings.Repeat("x", contracts.DispatchBlockUnits+1), "delivery": "btw", "session_ids": []string{"planner"}},
			"CAP_EXCEEDED",
		},
		"over the block count": {
			map[string]any{"body": paragraphBody(contracts.DispatchItemBlocks+1, 1), "delivery": "btw", "session_ids": []string{"planner"}},
			"CAP_EXCEEDED",
		},
	} {
		t.Run(name, func(t *testing.T) {
			response := postBroadcast(t, handler, input.body, "alice")
			if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), `"code":"`+input.code+`"`) {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
		})
	}
	// The key is a header now (INFRA-258): a body still naming one, as a tab opened before the
	// cutover sends it with no header, is a member the route does not take.
	t.Run("a body key", func(t *testing.T) {
		response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/broadcasts", map[string]any{
			"body": "hi", "delivery": "btw", "session_ids": []string{"planner"}, "idempotency_key": "press-1",
		}, "alice")
		if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), `"code":"INVALID_JSON"`) ||
			!strings.Contains(response.Body.String(), "idempotency_key") {
			t.Fatalf("status=%d body=%s, want 400 INVALID_JSON naming idempotency_key", response.Code, response.Body.String())
		}
	})
	// A send without a key is sent, as any write without one is: it is only not replayed.
	keyless := dispatchRequest(t, handler, http.MethodPost, "/api/v1/broadcasts", map[string]any{
		"body": "hi", "delivery": "btw", "session_ids": []string{"planner"},
	}, "alice")
	if keyless.Code != http.StatusCreated {
		t.Fatalf("a keyless send: status=%d body=%s, want 201", keyless.Code, keyless.Body.String())
	}
	awaitBroadcastDeliveries(t, handler, decodeBody[broadcastResponse](t, keyless).ID)

	missing := dispatchRequest(t, handler, http.MethodGet, "/api/v1/broadcasts/5a660655-04ad-4ce0-8a9b-93dd03c412b7", nil, "alice")
	if missing.Code != http.StatusNotFound || !strings.Contains(missing.Body.String(), `"code":"BROADCAST_NOT_FOUND"`) {
		t.Fatalf("unknown broadcast: status=%d body=%s", missing.Code, missing.Body.String())
	}
	malformed := dispatchRequest(t, handler, http.MethodGet, "/api/v1/broadcasts/not-a-uuid", nil, "alice")
	if malformed.Code != http.StatusNotFound || !strings.Contains(malformed.Body.String(), `"code":"BROADCAST_NOT_FOUND"`) {
		t.Fatalf("malformed id: status=%d body=%s", malformed.Code, malformed.Body.String())
	}
}

// awaitBroadcastDeliveries reads the broadcast back until no recipient's attempt is pending:
// delivery runs behind the create response, so a test that asserts on settled attempts waits for it.
// The wait's one bound is the test binary's deadline less testwait's margin.
func awaitBroadcastDeliveries(t *testing.T, handler http.Handler, id string) broadcastResponse {
	t.Helper()
	return testwait.UntilPolling(t, 20*time.Millisecond, "every recipient of the broadcast to hold a settled attempt (the broadcast last read)", func() (broadcastResponse, bool) {
		read := dispatchRequest(t, handler, http.MethodGet, "/api/v1/broadcasts/"+id, nil, "alice")
		if read.Code != http.StatusOK {
			t.Fatalf("read broadcast: status=%d body=%s", read.Code, read.Body.String())
		}
		stored := decodeBody[broadcastResponse](t, read)
		settled := len(stored.Recipients) > 0
		for _, recipient := range stored.Recipients {
			if len(recipient.Message.Deliveries) == 0 || recipient.Message.Deliveries[0].State == "pending" {
				settled = false
			}
		}
		return stored, settled
	})
}

// LEGION-233 review, P1. The send answers as soon as the messages are committed and delivers
// behind the request, on the server's lifetime rather than the request's: delivering inside the
// create request would let a request cut off partway - a closed tab, a deploy's shutdown, the
// load balancer's idle timeout - strand one attempt pending and leave every later recipient with
// no attempt at all, with nothing to recover them.
func TestBroadcastAnswersBeforeDeliveringAndStillReachesEveryRecipient(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	var listenerState struct {
		sync.Mutex
		sends int
	}
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/registry/"):
			writeSessionRecord(t, w, broadcastSessions, strings.TrimPrefix(r.URL.Path, "/v1/registry/"))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sessions":
			_, _ = w.Write([]byte(broadcastSessions))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/messages/send":
			<-release
			listenerState.Lock()
			listenerState.sends++
			listenerState.Unlock()
			_, _ = w.Write([]byte(`{"event_id":"envelope","recipient":"s"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer listener.Close()
	handler, _ := newTargetedMessageHandler(t, listener.URL)

	// Every listener send is blocked, so a handler that delivered inside the request could not
	// answer at all.
	response := postBroadcast(t, handler, map[string]any{
		"body": "Status?", "delivery": "btw", "session_ids": []string{"planner", "tester"},
	}, "alice")
	if response.Code != http.StatusCreated {
		t.Fatalf("create broadcast: status=%d body=%s", response.Code, response.Body.String())
	}
	created := decodeBody[broadcastResponse](t, response)
	listenerState.Lock()
	sends := listenerState.sends
	listenerState.Unlock()
	if sends != 0 {
		t.Fatalf("%d sends completed before the create answered; delivery must run behind it", sends)
	}
	// Every recipient carries its attempt from the moment the broadcast is written, opened
	// pending and unclaimed: a recipient with no attempt could only be moved by a delivery in
	// another mode, which is a second frame wherever the first one landed.
	for _, recipient := range created.Recipients {
		if len(recipient.Message.Deliveries) != 1 {
			t.Fatalf("%s carries %#v, want one attempt opened with the broadcast",
				recipient.SessionID, recipient.Message.Deliveries)
		}
		attempt := recipient.Message.Deliveries[0]
		if attempt.Attempt != 1 || attempt.State != "pending" || attempt.Delivery != "btw" ||
			attempt.SessionID != recipient.SessionID {
			t.Fatalf("%s attempt = %#v, want attempt 1 pending in the broadcast's mode",
				recipient.SessionID, attempt)
		}
	}

	close(release)
	delivered := awaitBroadcastDeliveries(t, handler, created.ID)
	if len(delivered.Recipients) != 2 {
		t.Fatalf("recipients = %#v", delivered.Recipients)
	}
	for _, recipient := range delivered.Recipients {
		// The worker settles the attempt the write opened; it never opens a second one beside it.
		if len(recipient.Message.Deliveries) != 1 || recipient.Message.Deliveries[0].Attempt != 1 ||
			recipient.Message.Deliveries[0].State != "sent" {
			t.Fatalf("%s attempts = %#v, want the one attempt settled sent",
				recipient.SessionID, recipient.Message.Deliveries)
		}
	}
}

// LEGION-233 review, second round. A broadcast's worker is queued behind its own attempt, so a
// human who changes the mode from the agent card in the meantime settles attempt 1 and sends
// their own frame. The worker is bound to attempt 1, and skips the recipient once that attempt is
// no longer its to send; a worker that opened an attempt of its own would send a SECOND frame to
// the same session.
func TestBroadcastWorkerSkipsARecipientTakenOverFromTheAgentCard(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	// The worker announces itself when it parks, so the takeover below happens while it is
	// held rather than at a moment this test guessed.
	parked := make(chan struct{}, 1)
	var listenerState struct {
		sync.Mutex
		seenCreate bool
		holding    bool
		sends      []string
	}
	listenerState.holding = true
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/registry/"):
			listenerState.Lock()
			// The create handler judges the selection before it starts any worker, so its own
			// read of the planner's record is always the first one and is never held.
			first := !listenerState.seenCreate
			listenerState.seenCreate = true
			hold := listenerState.holding && !first
			listenerState.Unlock()
			if hold {
				parked <- struct{}{}
				<-release
			}
			writeSessionRecord(t, w, broadcastSessions, strings.TrimPrefix(r.URL.Path, "/v1/registry/"))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/messages/send":
			var request struct {
				IdempotencyKey string `json:"idempotency_key"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Errorf("decode send: %v", err)
			}
			listenerState.Lock()
			listenerState.sends = append(listenerState.sends, request.IdempotencyKey)
			listenerState.Unlock()
			_, _ = w.Write([]byte(`{"event_id":"envelope","recipient":"planner"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer listener.Close()
	handler, database, _, served := newRegisteredTestServer(t, testServerOptions{envoyURL: listener.URL})

	created := decodeBody[broadcastResponse](t, postBroadcast(t, handler, map[string]any{
		"body": "Status?", "delivery": "btw", "session_ids": []string{"planner"},
	}, "alice"))
	message := created.Recipients[0].Message.ID

	testwait.Receive(t, "the broadcast's worker to park on its attempt", parked)
	// The worker is held. From here the listener answers everyone else at once, so the human's
	// own send is never the one being held.
	listenerState.Lock()
	listenerState.holding = false
	listenerState.Unlock()

	// The human changes the mode from the agent card while the worker is held: this settles
	// attempt 1 as superseded and sends its own frame under attempt 2.
	change := dispatchRequest(t, handler, http.MethodPost, "/api/v1/messages/"+message+"/deliveries", map[string]any{
		"delivery": "steer",
	}, "alice")
	if change.Code != http.StatusCreated {
		t.Fatalf("mode change: status=%d body=%s", change.Code, change.Body.String())
	}
	close(release)

	// The broadcast's delivery runs as the server's detached work (broadcasts.go): once that has
	// returned, the worker has done all it will, a send of an attempt it wrongly opened included.
	idle := make(chan struct{})
	go func() {
		served.detached.wait()
		close(idle)
	}()
	testwait.Receive(t, "the broadcast's worker to finish", idle)
	var pending int
	if err := database.Pool.QueryRow(context.Background(),
		`select count(*) from message_deliveries where message_id = $1 and state = 'pending'`,
		message).Scan(&pending); err != nil {
		t.Fatalf("count pending attempts: %v", err)
	}
	if pending != 0 {
		t.Fatalf("%d attempts are still pending once the worker finished", pending)
	}

	listenerState.Lock()
	sends := append([]string(nil), listenerState.sends...)
	listenerState.Unlock()
	if len(sends) != 1 || sends[0] != message+":steer" {
		t.Fatalf("listener sends = %#v, want only the human's mode change (%s:steer)", sends, message)
	}
	var attempts int
	if err := database.Pool.QueryRow(context.Background(),
		`select count(*) from message_deliveries where message_id = $1`, message).Scan(&attempts); err != nil {
		t.Fatalf("count attempts: %v", err)
	}
	if attempts != 2 {
		t.Fatalf("attempts = %d, want the broadcast's and the human's, and none the worker opened", attempts)
	}
}

// TestBroadcastRecipientsReadBackInRequestOrder catches a change that falls back to the random
// message UUID when every recipient message has the transaction's same created_at timestamp.
func TestBroadcastRecipientsReadBackInRequestOrder(t *testing.T) {
	t.Parallel()
	requested := []string{"zeta", "alpha", "mid", "bravo", "echo", "delta", "hotel", "gamma"}
	for _, scenario := range []struct {
		name     string
		excluded string
	}{
		{name: "all requested sessions are live"},
		{name: "an excluded session leaves the remaining order intact", excluded: "bravo"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			live := make([]string, 0, len(requested))
			want := make([]string, 0, len(requested))
			for _, sessionID := range requested {
				if sessionID == scenario.excluded {
					continue
				}
				live = append(live, sessionID)
				want = append(want, sessionID)
			}
			listener, _ := newBroadcastListener(t, broadcastSessionsJSON(t, live))
			handler, database := newTargetedMessageHandler(t, listener.URL)
			installReverseBroadcastMessageIDDefault(t, database)

			created := decodeBody[broadcastResponse](t, postBroadcast(t, handler, map[string]any{
				"body": "Keep this recipient order.", "delivery": "btw", "session_ids": requested,
			}, "alice"))
			var createdAtCount int
			if err := database.Pool.QueryRow(context.Background(),
				`select count(distinct created_at) from messages where broadcast_id = $1`, created.ID).Scan(&createdAtCount); err != nil {
				t.Fatalf("count broadcast message timestamps: %v", err)
			}
			if createdAtCount != 1 {
				t.Fatalf("broadcast message timestamps = %d, want one transaction timestamp", createdAtCount)
			}

			stored := awaitBroadcastDeliveries(t, handler, created.ID)
			if got := broadcastRecipientSessionIDs(stored); !equalStrings(got, want) {
				t.Fatalf("recipient order = %v, want request order %v", got, want)
			}
		})
	}
}

func broadcastSessionsJSON(t *testing.T, sessionIDs []string) string {
	t.Helper()
	sessions := make([]map[string]any, 0, len(sessionIDs))
	for _, sessionID := range sessionIDs {
		sessions = append(sessions, map[string]any{
			"session_id": sessionID,
			"title":      sessionID,
			"capabilities": []string{
				"btw",
			},
			"last_seen": 1,
		})
	}
	encoded, err := json.Marshal(sessions)
	if err != nil {
		t.Fatalf("encode broadcast sessions: %v", err)
	}
	return string(encoded)
}

func installReverseBroadcastMessageIDDefault(t *testing.T, database *store.Store) {
	t.Helper()
	_, err := database.Pool.Exec(context.Background(), `
		create sequence broadcast_test_message_ids start with 1;
		create function broadcast_test_message_id() returns uuid
		language sql volatile as $$
			select case nextval('broadcast_test_message_ids')
				when 1 then '00000000-0000-0000-0000-000000000008'::uuid
				when 2 then '00000000-0000-0000-0000-000000000007'::uuid
				when 3 then '00000000-0000-0000-0000-000000000006'::uuid
				when 4 then '00000000-0000-0000-0000-000000000005'::uuid
				when 5 then '00000000-0000-0000-0000-000000000004'::uuid
				when 6 then '00000000-0000-0000-0000-000000000003'::uuid
				when 7 then '00000000-0000-0000-0000-000000000002'::uuid
				when 8 then '00000000-0000-0000-0000-000000000001'::uuid
				else '00000000-0000-0000-0000-000000000009'::uuid
			end
		$$;
		alter table messages alter column id set default broadcast_test_message_id();
	`)
	if err != nil {
		t.Fatalf("install reverse broadcast message id default: %v", err)
	}
}

func broadcastRecipientSessionIDs(response broadcastResponse) []string {
	sessionIDs := make([]string, 0, len(response.Recipients))
	for _, recipient := range response.Recipients {
		sessionIDs = append(sessionIDs, recipient.SessionID)
	}
	return sessionIDs
}

// A server that predates broadcast positions writes the same row shape, without the new column.
// The migrated schema must accept it and readers must retain the only fallback order those rows
// have: their timestamp and UUID.
func TestBroadcastReadsLegacyUnpositionedRecipientsByCreatedAtAndID(t *testing.T) {
	t.Parallel()
	handler, database := newTestHandlerWithStore(t)
	ctx := context.Background()
	author := []byte(`{"kind":"user","id":"alice"}`)
	createdAt := time.Date(2026, time.September, 29, 12, 0, 0, 0, time.UTC)
	var broadcastID string
	if err := database.Pool.QueryRow(ctx, `
		insert into broadcasts (author, body, delivery, created_at)
		values ($1, $2, $3, $4)
		returning id::text
	`, author, "Legacy broadcast.", "btw", createdAt).Scan(&broadcastID); err != nil {
		t.Fatalf("insert legacy broadcast: %v", err)
	}
	legacy := []struct {
		id        string
		sessionID string
	}{
		{id: "00000000-0000-0000-0000-000000000001", sessionID: "zeta"},
		{id: "00000000-0000-0000-0000-000000000002", sessionID: "alpha"},
	}
	for _, recipient := range legacy {
		if _, err := database.Pool.Exec(ctx, `
			insert into messages (id, issue_key, author, body, target, broadcast_id, created_at)
			values ($1, null, $2, $3, $4, $5, $6)
		`, recipient.id, author, "Legacy broadcast.", "session:"+recipient.sessionID, broadcastID, createdAt); err != nil {
			t.Fatalf("insert legacy message for %s: %v", recipient.sessionID, err)
		}
	}
	var position *int
	if err := database.Pool.QueryRow(ctx,
		`select broadcast_position from messages where id = $1`, legacy[0].id).Scan(&position); err != nil {
		t.Fatalf("read legacy broadcast position: %v", err)
	}
	if position != nil {
		t.Fatalf("legacy broadcast position = %d, want null", *position)
	}

	read := dispatchRequest(t, handler, http.MethodGet, "/api/v1/broadcasts/"+broadcastID, nil, "alice")
	if read.Code != http.StatusOK {
		t.Fatalf("read legacy broadcast: status=%d body=%s", read.Code, read.Body.String())
	}
	if got, want := broadcastRecipientSessionIDs(decodeBody[broadcastResponse](t, read)), []string{"zeta", "alpha"}; !equalStrings(got, want) {
		t.Fatalf("legacy recipient order = %v, want created_at and id order %v", got, want)
	}
}
