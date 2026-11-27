package api

import (
	"cmp"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/asks"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

type threadPageJSON struct {
	Threads   []map[string]any `json:"threads"`
	Before    *string          `json:"before"`
	Remaining int              `json:"remaining"`
}

func pageJSON(t *testing.T, handler http.Handler, target, query string) threadPageJSON {
	t.Helper()
	response := listRead(t, handler, target+"?order=activity"+query, listReadOptions{})
	if !strings.HasSuffix(target, "/comments") && response.Header().Get("ETag") != "" {
		t.Fatalf("GET %s unexpectedly carries an ETag", target)
	}
	return decodeThreadPage(t, response.Body.Bytes(), response.Code, target+query)
}

func decodeThreadPage(t *testing.T, body []byte, status int, target string) threadPageJSON {
	t.Helper()
	var page threadPageJSON
	if err := json.Unmarshal(body, &page); status != 200 || err != nil || page.Threads == nil {
		t.Fatalf("GET %s: want 200 with a threads page, got %d (%v): %.240s", target, status, err, body)
	}
	var members map[string]json.RawMessage
	if err := json.Unmarshal(body, &members); err != nil || len(members) != 3 || members["before"] == nil || members["remaining"] == nil {
		t.Fatalf("page members: %v (%v)", members, err)
	}
	return page
}

func pageThreadID(route string, row map[string]any) string {
	switch route {
	case "comments":
		row, _ = row["comment"].(map[string]any)
	case "messages":
		row, _ = row["message"].(map[string]any)
	}
	id, _ := row["id"].(string)
	return id
}

type expectedThread struct{ id, at string }

func checkThreadPage(t *testing.T, route string, page threadPageJSON, want []expectedThread, remaining int) {
	t.Helper()
	if len(page.Threads) != len(want) || page.Remaining != remaining {
		t.Fatalf("%s: page ids %v, remaining %d; want %d rows, remaining %d", route, pageIDs(route, page), page.Remaining, len(want), remaining)
	}
	for i, row := range page.Threads {
		if id, at := pageThreadID(route, row), row["last_activity_at"]; id != want[i].id || at != want[i].at {
			t.Fatalf("%s: row %d at (%s, %v), want %+v", route, i, id, at, want[i])
		}
	}
	if remaining == 0 {
		if page.Before != nil {
			t.Fatalf("last page before = %q, want null", *page.Before)
		}
	} else {
		last := want[len(want)-1]
		if cursor := last.at + "," + last.id; page.Before == nil || *page.Before != cursor {
			t.Fatalf("before = %q, want %q", deref(page.Before), cursor)
		}
	}
}

func pageIDs(route string, page threadPageJSON) []string {
	ids := make([]string, 0, len(page.Threads))
	for _, row := range page.Threads {
		ids = append(ids, pageThreadID(route, row))
	}
	return ids
}

func requirePageIDs(t *testing.T, h http.Handler, target, route, filters string, limit int, want ...string) {
	t.Helper()
	before := ""
	for start := 0; ; {
		p := pageJSON(t, h, target, filters+"&limit="+strconv.Itoa(limit)+before)
		end := min(start+limit, len(want))
		if ids := pageIDs(route, p); !slices.Equal(ids, want[start:end]) || p.Remaining != len(want)-end {
			t.Fatalf("%s%s: ids %v remaining %d; want %v remaining %d", filters, before, ids, p.Remaining, want[start:end], len(want)-end)
		}
		if end == len(want) {
			if p.Before != nil {
				t.Fatalf("final cursor = %q", *p.Before)
			}
			return
		}
		if p.Before == nil {
			t.Fatal("no cursor for the next page")
		}
		before = "&before=" + url.QueryEscape(*p.Before)
		start = end
	}
}

const walkTimeLayout = "2006-01-02T15:04:05.000000Z"

func fixtureTime(n int) time.Time {
	return time.Date(2025, 1, 1, 0, 0, 0, 123456000, time.UTC).Add(time.Duration(n) * time.Second)
}

func timeFixtureEntry(t *testing.T, db *store.Store, table, id string, n int) string {
	t.Helper()
	at := fixtureTime(n)
	legionExec(t, db, "update "+table+" set created_at = $2 where id = $1", id, at)
	return at.Format(walkTimeLayout)
}

func walkThreadPages(t *testing.T, h http.Handler, target, route, filter string, limit int, want []expectedThread, listRows map[string]map[string]any) *string {
	t.Helper()
	var first *string
	before := ""
	for start := 0; start < len(want); {
		p := pageJSON(t, h, target, filter+"&limit="+strconv.Itoa(limit)+before)
		end := min(start+limit, len(want))
		checkThreadPage(t, route, p, want[start:end], len(want)-end)
		if start == 0 {
			first = p.Before
		}
		for i, row := range p.Threads {
			if listRows == nil {
				break
			}
			delete(row, "last_activity_at")
			if expected := listRows[want[start+i].id]; !reflect.DeepEqual(row, expected) {
				t.Fatalf("%s: thread %s differs from the list row\n got: %#v\nwant: %#v", route, want[start+i].id, row, expected)
			}
		}
		if p.Before != nil {
			before = "&before=" + url.QueryEscape(*p.Before)
		}
		start = end
	}
	return first
}

func sortExpectedThreads(want []expectedThread) {
	slices.SortFunc(want, func(a, b expectedThread) int {
		if order := cmp.Compare(b.at, a.at); order != 0 {
			return order
		}
		return cmp.Compare(b.id, a.id)
	})
}

func verifyThreadWalk(t *testing.T, h http.Handler, target, route, filter string, want []expectedThread, list map[string]map[string]any, write func(string) string) {
	t.Helper()
	sortExpectedThreads(want)
	first := walkThreadPages(t, h, target, route, filter, 20, want, list)
	walkThreadPages(t, h, target, route, filter, 15, want, nil)
	checkThreadPage(t, route, pageJSON(t, h, target, filter), want[:20], 25)
	checkThreadPage(t, route, pageJSON(t, h, target, filter+"&limit=100"), want, 0)
	moved := expectedThread{id: want[20].id, at: write(want[20].id)}
	second := pageJSON(t, h, target, filter+"&limit=20&before="+url.QueryEscape(*first))
	checkThreadPage(t, route, second, want[21:41], 4)
	checkThreadPage(t, route, pageJSON(t, h, target, filter+"&limit=20&before="+url.QueryEscape(*second.Before)), want[41:], 0)
	checkThreadPage(t, route, pageJSON(t, h, target, filter+"&limit=1"), []expectedThread{moved}, 44)
}

func TestThreadPagesWalkEveryThreadOnceNewestActivityFirst(t *testing.T) {
	t.Parallel()
	h, db := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, h, "PROJ", "Walking thread pages", "A spec with selected words.")
	t.Run("asks", func(t *testing.T) { walkAskPages(t, h, db, issue.Key) })
	t.Run("comments", func(t *testing.T) { walkCommentPages(t, h, db, issue.Key, issue.PrimaryArtifactID) })
	t.Run("messages", func(t *testing.T) { walkMessagePages(t, h, db, issue.Key) })
}

func walkCommentPages(t *testing.T, h http.Handler, db *store.Store, key, artifact string) {
	t.Helper()
	target := commentListOf(key)
	want := make([]expectedThread, 45)
	for i := range want {
		input := map[string]any{"body": fmt.Sprintf("Comment %d", i)}
		if i < 2 {
			input["anchor"] = map[string]any{"artifact": "spec", "quote": "selected words"}
		}
		root := createThreadComment(t, h, key, input, "alice")
		want[i] = expectedThread{root.ID, timeFixtureEntry(t, db, "comments", root.ID, max(3, i))}
	}
	reply := func(root string, n int) (string, string) {
		added := createThreadComment(t, h, key, map[string]any{"body": fmt.Sprintf("Reply %d", n), "reply_to": root}, "alice")
		return added.ID, timeFixtureEntry(t, db, "comments", added.ID, n)
	}
	_, want[0].at = reply(want[0].id, 51)
	deleted, at := reply(want[1].id, 50)
	want[1].at = at
	_, _ = reply(want[0].id, 47)
	deleteThreadComment(t, h, deleted, "alice")
	editThreadComment(t, h, want[2].id, "Edited after the newest reply.", "alice")
	ask := createPartAsk(t, h, key, "An ask among the comments?")
	askReply := createThreadComment(t, h, key, map[string]any{"body": "Only in the ask thread", "ask_id": ask.ID}, "alice")
	timeFixtureEntry(t, db, "comments", askReply.ID, 70)
	flat := decodeBody[[]map[string]any](t, treesRequest(t, h, target, false))
	expected := make(map[string]map[string]any, len(want))
	for _, row := range flat {
		if row["ask_id"] != nil {
			continue
		}
		if _, reply := row["reply_to"].(string); !reply {
			expected[row["id"].(string)] = map[string]any{"comment": row, "replies": []any{}}
		}
	}
	for _, row := range flat {
		if root, reply := row["reply_to"].(string); reply && row["ask_id"] == nil {
			expected[root]["replies"] = append(expected[root]["replies"].([]any), row)
		}
	}
	verifyThreadWalk(t, h, target, "comments", "", want, expected, func(id string) string { _, at := reply(id, 80); return at })
	walkThreadPages(t, h, target, "comments", "&artifact="+artifact, 1, want[:2], expected)
}

func TestCommentThreadPagePersonInHoldsThePeoplesAndTheTextsThreads(t *testing.T) {
	t.Parallel()
	h, db := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, h, "PROJ", "People and selected text", "A spec with selected words.")
	target := commentListOf(issue.Key)
	create := func(body, login string, fields map[string]any, n int) string {
		if fields == nil {
			fields = map[string]any{}
		}
		fields["body"] = body
		if login == "" && fields["actor"] == nil {
			fields["actor"] = sessionActor()
		}
		row := createThreadComment(t, h, issue.Key, fields, login)
		timeFixtureEntry(t, db, "comments", row.ID, n)
		return row.ID
	}
	person := create("A person's root", "alice", nil, 1)
	answered := create("An agent's root", "", nil, 2)
	create("A person's reply", "alice", map[string]any{"reply_to": answered}, 3)
	anchored := create("On the spec", "", map[string]any{"anchor": map[string]any{"artifact": "spec", "quote": "selected words"}}, 4)
	quoted := create("An agent's quoted words", "", nil, 5)
	replier := model.Actor{Kind: "session", ID: "agent-replier"}
	create("On the words", "", withQuote(map[string]any{"reply_to": quoted, "actor": replier}, "reply_to_block", "b1", "quoted words", 0), 6)
	block := create("An agent's whole block", "", nil, 7)
	create("On the block alone", "", map[string]any{"reply_to": block, "reply_to_block": "b1", "actor": replier}, 8)
	create("An unanchored agent root", "", nil, 9)
	token := mintPersonalToken(t, h, "alice", "Thread page fixture", nil)
	owned := agentRequest(t, h, http.MethodPost, target, map[string]any{
		"body": "A session with a personal token", "actor": model.Actor{Kind: "session", ID: "agent-for-alice"},
	}, token.Token)
	timeFixtureEntry(t, db, "comments", createdID(t, owned), 10)
	for i := range 25 {
		create(fmt.Sprintf("New agent comment %d", i), "", nil, 11+i)
	}
	requirePageIDs(t, h, target, "comments", "&person=in", 2, quoted, anchored, answered, person)
	deleteThreadComment(t, h, quoted, "")
	requirePageIDs(t, h, target, "comments", "&person=in", 2, quoted, anchored, answered, person)
}

func TestThreadPageAuthorKeepsTheThreadsTheActorWroteIn(t *testing.T) {
	t.Parallel()
	h, db := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, h, "PROJ", "Threads by an author", "A spec")
	t.Run("comments", func(t *testing.T) {
		target := commentListOf(issue.Key)
		create := func(login, root string, n int) string {
			input := map[string]any{"body": fmt.Sprintf("Author comment %d", n)}
			if root != "" {
				input["reply_to"] = root
			}
			if login == "" {
				input["actor"] = sessionActor()
			}
			id := createThreadComment(t, h, issue.Key, input, login).ID
			timeFixtureEntry(t, db, "comments", id, n)
			return id
		}
		person := create("alice", "", 1)
		answered := create("bob", "", 2)
		create("alice", answered, 3)
		session := create("", "", 4)
		sessionReply := create("bob", "", 5)
		create("", sessionReply, 6)
		requireThreadAuthors(t, h, target, "comments", []string{answered, person}, []string{sessionReply, session})
		requirePageIDs(t, h, target, "comments", "&person=in&author=alice", 1, answered, person)
		requirePageIDs(t, h, target, "comments", "&person=in&author="+authorSession(), 1, sessionReply)
	})
	t.Run("messages", func(t *testing.T) {
		target := "/api/v1/issues/" + issue.Key + "/messages"
		user := func(id string) model.Actor { return model.Actor{Kind: "user", ID: id} }
		agent := model.Actor{Kind: "session", ID: authorSession()}
		person := seedPageMessage(t, db, issue.Key, user("alice"), nil, nil, 1)
		answered := seedPageMessage(t, db, issue.Key, user("bob"), nil, nil, 2)
		seedPageMessage(t, db, issue.Key, user("alice"), &answered, nil, 3)
		session := seedPageMessage(t, db, issue.Key, agent, nil, new("role:reviewer"), 4)
		sessionReply := seedPageMessage(t, db, issue.Key, user("bob"), nil, nil, 5)
		seedPageMessage(t, db, issue.Key, agent, &sessionReply, nil, 6)
		requireThreadAuthors(t, h, target, "messages", []string{answered, person}, []string{sessionReply, session})
		requirePageIDs(t, h, target, "messages", "&person=in&author=alice", 1, answered, person)
		requirePageIDs(t, h, target, "messages", "&person=in&author="+authorSession(), 1, sessionReply)
	})
	t.Run("asks", func(t *testing.T) {
		target := "/api/v1/issues/" + issue.Key + "/asks"
		person := createdID(t, dispatchRequest(t, h, http.MethodPost, target, map[string]any{"question": "Alice's question?"}, "alice"))
		timeFixtureEntry(t, db, "asks", person, 1)
		create := func(n int) string {
			id := createPartAsk(t, h, issue.Key, fmt.Sprintf("Ask by session %d?", n)).ID
			timeFixtureEntry(t, db, "asks", id, n)
			return id
		}
		replied := create(2)
		reply := createThreadComment(t, h, issue.Key, map[string]any{"body": "A person's reply", "ask_id": replied}, "alice")
		timeFixtureEntry(t, db, "comments", reply.ID, 3)
		answered := create(4)
		answerSearchAsk(t, h, answered, map[string]any{"text": "An answer by Alice."})
		setAskActivity(t, db, answered, "answer", fixtureTime(5))
		declined := create(6)
		declinePageAsk(t, h, declined)
		setAskActivity(t, db, declined, "resolution", fixtureTime(7))
		session := create(8)
		sessionReply := createdID(t, dispatchRequest(t, h, http.MethodPost, target, map[string]any{"question": "Bob's question?"}, "bob"))
		timeFixtureEntry(t, db, "asks", sessionReply, 9)
		agentReply := createThreadComment(t, h, issue.Key, map[string]any{"body": "A session's reply", "ask_id": sessionReply, "actor": sessionActor()}, "")
		timeFixtureEntry(t, db, "comments", agentReply.ID, 10)
		requireThreadAuthors(t, h, target, "asks", []string{declined, answered, replied, person}, []string{sessionReply, session, declined, answered, replied})
	})
}

func authorSession() string { return sessionActor()["id"].(string) }

func requireThreadAuthors(t *testing.T, h http.Handler, target, route string, people, sessions []string) {
	t.Helper()
	for _, login := range []string{"alice", "ALICE"} {
		requirePageIDs(t, h, target, route, "&author="+login, 1, people...)
	}
	requirePageIDs(t, h, target, route, "&author="+authorSession(), 1, sessions...)
	requirePageIDs(t, h, target, route, "&author=SESSION-0123456789ABCDEF", 1)
}

func walkMessagePages(t *testing.T, h http.Handler, db *store.Store, key string) {
	t.Helper()
	target := "/api/v1/issues/" + key + "/messages"
	write := func(parent *string, n int) expectedThread {
		input := map[string]any{"body": fmt.Sprintf("Message %d", n)}
		if parent != nil {
			input["in_reply_to"] = *parent
		}
		row := createIssueMessage(t, h, key, input, "alice")
		return expectedThread{row.ID, timeFixtureEntry(t, db, "messages", row.ID, n)}
	}
	want := make([]expectedThread, 45)
	for i := range want {
		want[i] = write(nil, max(3, i))
	}
	child := write(&want[0].id, 46)
	want[0].at = write(&child.id, 51).at
	deleted := write(&want[1].id, 50)
	want[1].at = deleted.at
	deleteIssueMessage(t, h, deleted.id, "alice")
	editIssueMessage(t, h, want[2].id, "Edited after the newest message.", "alice")
	verifyThreadWalk(t, h, target, "messages", "", want, threadListRows(t, h, target, "messages"), func(id string) string { return write(&id, 80).at })
}

func threadListRows(t *testing.T, h http.Handler, target, route string) map[string]map[string]any {
	t.Helper()
	rows := decodeBody[[]map[string]any](t, treesRequest(t, h, target, false))
	byID := make(map[string]map[string]any, len(rows))
	for _, row := range rows {
		byID[pageThreadID(route, row)] = row
	}
	return byID
}

func seedPageMessage(t *testing.T, db *store.Store, key string, author model.Actor, parent, target *string, n int) string {
	t.Helper()
	var id string
	err := db.Pool.QueryRow(t.Context(), `insert into messages (issue_key, author, body, in_reply_to, target, created_at)
  values ($1,$2,$3,$4,$5,$6) returning id::text`, key, author, fmt.Sprintf("Message %d", n), parent, target, fixtureTime(n)).Scan(&id)
	if err != nil {
		t.Fatalf("seed message: %v", err)
	}
	return id
}

func TestMessageThreadPagePersonSplitsThreadsAndLeavesOutTheDaemons(t *testing.T) {
	t.Parallel()
	h, db := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, h, "PROJ", "People in message threads", "A spec")
	target := "/api/v1/issues/" + issue.Key + "/messages"
	daemon := model.Actor{Kind: "session", ID: "legion-daemon:PROJ"}
	other := model.Actor{Kind: "session", ID: "legion-daemon:OTHER"}
	agent := model.Actor{Kind: "session", ID: authorSession()}
	person := model.Actor{Kind: "user", ID: "alice"}
	write := func(actor model.Actor, parent, target *string, n int) string {
		return seedPageMessage(t, db, issue.Key, actor, parent, target, n)
	}
	untargetedDaemon := write(daemon, nil, nil, 1)
	answeredDaemon := write(daemon, nil, nil, 2)
	write(person, &answeredDaemon, nil, 3)
	otherRoot := write(other, nil, nil, 4)
	agentRoot := write(agent, nil, nil, 5)
	roleRoot := write(agent, nil, new("role:reviewer"), 6)
	write(agent, &roleRoot, nil, 7)
	sessionRoot := write(agent, nil, new("session:another"), 8)
	child := write(agent, &sessionRoot, nil, 9)
	write(person, &child, nil, 10)
	personRoot := write(person, nil, new("session:another"), 11)
	var newer []string
	for i := range 25 {
		newer = append(newer, write(daemon, nil, nil, 12+i))
	}
	requirePageIDs(t, h, target, "messages", "&person=none", 2, roleRoot)
	requirePageIDs(t, h, target, "messages", "&person=in", 2, personRoot, sessionRoot, agentRoot, otherRoot)
	slices.Reverse(newer)
	requirePageIDs(t, h, target, "messages", "", 100, append(newer, personRoot, sessionRoot, roleRoot, agentRoot, otherRoot, answeredDaemon, untargetedDaemon)...)
}

func declinePageAsk(t *testing.T, h http.Handler, id string) {
	t.Helper()
	response := dispatchRequest(t, h, http.MethodPost, "/api/v1/asks/"+id+"/decline", declineBody(nil), "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("decline ask: %d %s", response.Code, response.Body)
	}
}

// setAskActivity moves the time of the ask's current answer or resolution, as column names, to
// at. The page reads the answer off the ask's node (R3), so the move is written as the decisions
// step's catch-up states a write (settleAskAsTheCatchUp): the ask's whole answer state as its row
// holds it, with that one time moved, on its node and its row both.
func setAskActivity(t *testing.T, db *store.Store, id, column string, at time.Time) {
	t.Helper()
	var state string
	var storedAnswer, storedResolution []byte
	if err := db.Pool.QueryRow(t.Context(), `select state, answer, resolution from asks where id = $1`, id).
		Scan(&state, &storedAnswer, &storedResolution); err != nil {
		t.Fatalf("read the answer state of ask %s: %v", id, err)
	}
	var answer *model.AskAnswer
	var resolution *model.AskResolution
	if storedAnswer != nil {
		answer = new(model.AskAnswer)
		if err := json.Unmarshal(storedAnswer, answer); err != nil {
			t.Fatalf("decode the answer of ask %s: %v", id, err)
		}
	}
	if storedResolution != nil {
		resolution = new(model.AskResolution)
		if err := json.Unmarshal(storedResolution, resolution); err != nil {
			t.Fatalf("decode the resolution of ask %s: %v", id, err)
		}
	}
	var who model.Actor
	switch {
	case column == "answer" && answer != nil:
		answer.At, who = at, model.Actor{Kind: "user", ID: answer.User}
	case column == "resolution" && resolution != nil:
		resolution.At, who = at, resolution.Actor
	default:
		t.Fatalf("ask %s holds no %s to move", id, column)
	}
	change := asks.Recorded(state, answer, resolution)
	change.Who, change.At, change.Event = who, at, graphstore.NoEvent
	settleAskAsTheCatchUp(t, db, id, change)
}

func walkAskPages(t *testing.T, h http.Handler, db *store.Store, key string) {
	t.Helper()
	target := "/api/v1/issues/" + key + "/asks"
	want := make([]expectedThread, 45)
	for i := range want {
		ask := createPartAsk(t, h, key, fmt.Sprintf("Question %d?", i))
		want[i] = expectedThread{ask.ID, timeFixtureEntry(t, db, "asks", ask.ID, max(5, i))}
	}
	answerSearchAsk(t, h, want[0].id, map[string]any{"text": "The current answer."})
	firstAnswer := fixtureTime(55)
	setAskActivity(t, db, want[0].id, "answer", firstAnswer)
	want[0].at = firstAnswer.Format(walkTimeLayout)
	declinePageAsk(t, h, want[1].id)
	// PostgreSQL rounds this timestamp if the cast sees its seventh digit.
	declineAt := fixtureTime(54).Add(900 * time.Nanosecond)
	setAskActivity(t, db, want[1].id, "resolution", declineAt)
	want[1].at = fixtureTime(54).Format(walkTimeLayout)
	reply := func(id string, n int) expectedThread {
		row := createThreadComment(t, h, key, map[string]any{"body": fmt.Sprintf("Ask reply %d", n), "ask_id": id}, "alice")
		return expectedThread{row.ID, timeFixtureEntry(t, db, "comments", row.ID, n)}
	}
	want[2].at = reply(want[2].id, 52).at
	deleted := reply(want[3].id, 53)
	want[3].at = deleted.at
	deleteThreadComment(t, h, deleted.id, "alice")
	edited := sessionRequest(t, h, http.MethodPatch, "/api/v1/asks/"+want[4].id, map[string]any{"actor": sessionActor(), "question": "Edited after every entry?"})
	if edited.Code != http.StatusOK {
		t.Fatalf("edit ask: %d %s", edited.Code, edited.Body)
	}
	list := threadListRows(t, h, target+"?thread=1", "asks")
	answered, declined := want[0], want[1]
	checkThreadPage(t, "asks", pageJSON(t, h, target, "&thread=1&limit=2"), []expectedThread{answered, declined}, 43)
	if _, held := pageJSON(t, h, target, "&limit=1").Threads[0]["thread"]; held {
		t.Fatal("ask page without thread=1 has a thread member")
	}
	open := slices.Clone(want[2:])
	sortExpectedThreads(open)
	walkThreadPages(t, h, target, "asks", "&state=open&thread=1", 20, open, list)
	checkThreadPage(t, "asks", pageJSON(t, h, target, "&state=answered"), []expectedThread{answered}, 0)
	verifyThreadWalk(t, h, target, "asks", "&thread=1", want, list, func(id string) string { return reply(id, 80).at })
	changedAt := answerSearchAsk(t, h, answered.id, map[string]any{
		"text": "The answerer changed the answer.", "expected_answer_at": firstAnswer.Format(time.RFC3339Nano),
	}).At
	if !changedAt.After(firstAnswer) {
		t.Fatalf("changed answer at %v, want after %v", changedAt, firstAnswer)
	}
	checkThreadPage(t, "asks", pageJSON(t, h, target, "&limit=1"), []expectedThread{{answered.id, changedAt.UTC().Format(walkTimeLayout)}}, 44)
}
