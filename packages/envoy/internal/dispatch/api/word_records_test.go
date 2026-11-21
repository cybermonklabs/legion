package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/agentloop"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/modeltest"
	"github.com/sjawhar/envoy/internal/dispatch/reviewer"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/dispatch/wordrecords"
)

// The records job of INFRA-3231's R8 against this package's server, with the stand-in records
// model (modeltest) in place of its model group.

// recordsDelay is how long the stand-in records model waits before it answers.
const recordsDelay = 5 * time.Second

// The stand-in's replies.
const (
	aboutStore = `{"verdict":"asks_word_meaning","words":["store"]}`
	aboutLane  = `{"verdict":"asks_word_meaning","words":["lane"]}`
	aboutShard = `{"verdict":"asks_word_meaning","words":["shard map"]}`
	notAbout   = `{"verdict":"none","words":[]}`
)

type recordsRig struct {
	t        *testing.T
	handler  http.Handler
	database *store.Store
	srv      *server
	issue    string
}

func newRecordsRig(t *testing.T) *recordsRig {
	t.Helper()
	handler, database, _, srv := newRegisteredTestServer(t, testServerOptions{writingRules: WritingRulesConfig{
		Refusal: true, Extra: []reviewer.RuleSet{reviewer.RuleReaderWords},
	}})
	issue := createInteractionIssue(t, handler, "WRD", "Records issue", "Seed paragraph here.\n")
	return &recordsRig{t: t, handler: handler, database: database, srv: srv, issue: issue.Key}
}

// job is the records job reading from from, its model the stand-in, at a test's pace.
func (rig *recordsRig) job(stand *modeltest.Model, from time.Time) *wordrecords.Job {
	return wordrecords.New(wordrecords.Config{
		Store:   rig.database,
		Horizon: rig.srv.horizon(),
		Classifier: wordrecords.Classifier{Model: agentloop.Model{
			Endpoint: agentloop.Endpoint{Kind: agentloop.AnthropicMessages, BaseURL: stand.URL,
				Token: func(context.Context) (string, error) { return "stand-in", nil }},
			ID: "stand-in-records", Effort: "off",
		}},
		From:       from,
		Interval:   100 * time.Millisecond,
		Backoff:    200 * time.Millisecond,
		MaxBackoff: 400 * time.Millisecond,
	})
}

// run runs job under its lease until the test ends.
func (rig *recordsRig) run(job *wordrecords.Job) {
	rig.t.Helper()
	leases := storetest.Leases(rig.t, rig.database.Pool)
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		job.Run(ctx, leases)
	}()
	rig.t.Cleanup(func() {
		cancel()
		<-stopped
	})
}

// agentAsk is an ask the agent session s-writer opens on the rig's issue.
func (rig *recordsRig) agentAsk(question string, options ...string) string {
	rig.t.Helper()
	body := map[string]any{"question": question, "actor": map[string]any{"kind": "session", "id": "s-writer"}}
	if len(options) > 0 {
		var labels []map[string]any
		for _, option := range options {
			labels = append(labels, map[string]any{"label": option})
		}
		body["options"] = labels
	}
	response := agentRequest(rig.t, rig.handler, http.MethodPost, "/api/v1/issues/"+rig.issue+"/asks", body, "agent-token")
	if response.Code != http.StatusCreated {
		rig.t.Fatalf("the agent's ask: %d %s", response.Code, response.Body.String())
	}
	return decodeBody[model.Ask](rig.t, response).ID
}

// timedWrite makes alice's write and answers how long it took.
func (rig *recordsRig) timedWrite(what, method, target string, body map[string]any, status int) time.Duration {
	rig.t.Helper()
	started := time.Now()
	response := dispatchRequest(rig.t, rig.handler, method, target, body, "alice")
	took := time.Since(started)
	if response.Code != status {
		rig.t.Fatalf("%s: %d %s, want %d", what, response.Code, response.Body.String(), status)
	}
	return took
}

// writesWithinTheModelsWait makes alice's two texts, "What is a store?" on the ask and "Approved,
// thanks." as a comment on the issue, and fails a test where either took the stand-in's wait.
func (rig *recordsRig) writesWithinTheModelsWait(ask, phase string) {
	rig.t.Helper()
	comments := "/api/v1/issues/" + rig.issue + "/comments"
	for _, write := range []struct {
		what string
		body map[string]any
	}{
		{"What is a store? on the agent's ask", map[string]any{"body": "What is a store?", "ask_id": ask}},
		{"Approved, thanks. as a comment", map[string]any{"body": "Approved, thanks."}},
	} {
		if took := rig.timedWrite(write.what, http.MethodPost, comments, write.body, http.StatusCreated); took >= recordsDelay {
			rig.t.Errorf("%s, the job %s: the write took %s, want it to return before the model's %s", write.what, phase, took, recordsDelay)
		}
	}
}

type wordRecord struct {
	EventID                        int64
	Person, State, Verdict, Answer string
	Day                            string
	Words                          []string
	Attempts                       int
}

func (rig *recordsRig) records() []wordRecord {
	rig.t.Helper()
	rows, err := rig.database.Pool.Query(context.Background(), `
		select coalesce(event_id, 0), coalesce(person, ''), state, coalesce(verdict, ''), coalesce(answers, ''),
			day::text, words, attempts
		from word_questions order by id`)
	if err != nil {
		rig.t.Fatalf("read the records: %v", err)
	}
	defer rows.Close()
	var out []wordRecord
	for rows.Next() {
		var r wordRecord
		if err := rows.Scan(&r.EventID, &r.Person, &r.State, &r.Verdict, &r.Answer, &r.Day, &r.Words, &r.Attempts); err != nil {
			rig.t.Fatalf("read a record: %v", err)
		}
		out = append(out, r)
	}
	return out
}

// done is how many records hold a verdict.
func (rig *recordsRig) done() int {
	n := 0
	for _, r := range rig.records() {
		if r.State == store.WordQuestionDone {
			n++
		}
	}
	return n
}

// counts reads the counts of the days first to last as bob, who is any signed-in person.
func (rig *recordsRig) counts(first, last string) store.WritingCheckCounts {
	rig.t.Helper()
	response := dispatchRequest(rig.t, rig.handler, http.MethodGet, "/api/v1/writing-check/counts?first="+first+"&last="+last, nil, "bob")
	if response.Code != http.StatusOK {
		rig.t.Fatalf("counts: %d %s", response.Code, response.Body.String())
	}
	return decodeBody[store.WritingCheckCounts](rig.t, response)
}

// row is the counts' row of day, project and person, zero where there is none.
func row(counts store.WritingCheckCounts, day, project, person string) store.WritingCheckRow {
	for _, r := range counts.Items {
		if r.Day == day && r.Project == project && r.Person == person {
			return r
		}
	}
	return store.WritingCheckRow{}
}

// words reads a reader's words with the agent token.
func (rig *recordsRig) words(login string) []string {
	rig.t.Helper()
	response := agentRequest(rig.t, rig.handler, http.MethodGet, "/api/v1/readers/"+login+"/words", nil, "agent-token")
	if response.Code != http.StatusOK {
		rig.t.Fatalf("%s's words: %d %s", login, response.Code, response.Body.String())
	}
	var words []string
	for _, word := range decodeBody[readerWordsResponse](rig.t, response).Items {
		words = append(words, word.Word)
	}
	return words
}

// A person's "What is a store?" on an agent's ask and "Approved, thanks." as a comment each return
// before the stand-in records model's 5 seconds, with the job off and with it on and calling that
// model. With it on, the job records each text, the question with the word it asks about and the
// ask it answers; the day's counts give alice her texts, her question and the ask she answered
// after a reply; the reader's words route answers "store" to an agent's token and to alice, and
// refuses bob; and the written rules show alice's word.
func TestWordRecordsRecordAQuestionAndNeverDelayAPersonsWrite(t *testing.T) {
	rig := newRecordsRig(t)
	stand := modeltest.New(t,
		modeltest.Rule{Contains: "What is a store?", Reply: modeltest.Reply{Text: aboutStore, Delay: recordsDelay}},
		modeltest.Rule{Contains: "Approved, thanks.", Reply: modeltest.Reply{Text: notAbout, Delay: recordsDelay}},
	)
	before := rig.agentAsk("Which store should hold the cache?", "Disk", "Memory")
	rig.writesWithinTheModelsWait(before, "off")
	if records := rig.records(); len(records) != 0 {
		t.Fatalf("records with the job off = %+v, want none", records)
	}

	// The job on: its backfill reads the two texts written while it was off, and holds the
	// stand-in's call while alice writes two more.
	today := time.Now().UTC()
	rig.run(rig.job(stand, today.Add(-time.Hour)))
	pollUntil(t, 10*time.Second, "the job's first call to the model", func() bool { return len(stand.Requests()) > 0 })
	ask := rig.agentAsk("Which store should hold the logs?", "Disk", "Memory")
	rig.writesWithinTheModelsWait(ask, "on")
	answer := map[string]any{"selected": []string{"Disk"}, "expected_edited_at": nil}
	rig.timedWrite("alice answers the ask", http.MethodPost, "/api/v1/asks/"+ask+"/answer", answer, http.StatusOK)
	pollUntil(t, 60*time.Second, "four records with a verdict", func() bool { return rig.done() == 4 })

	day := today.Format(time.DateOnly)
	var questions, others int
	for _, r := range rig.records() {
		switch {
		case r.Person != "alice" || r.Day != day:
			t.Errorf("record %+v, want alice's of %s", r, day)
		case r.Verdict == wordrecords.WordMeaning && slices.Equal(r.Words, []string{"store"}) && strings.HasPrefix(r.Answer, "ask:"):
			questions++
		case r.Verdict == wordrecords.None && len(r.Words) == 0:
			others++
		default:
			t.Errorf("record %+v", r)
		}
	}
	if questions != 2 || others != 2 {
		t.Errorf("%d questions about store and %d other texts, want 2 of each", questions, others)
	}
	if got := row(rig.counts(day, day), day, "WRD", "alice"); got.WordQuestions != 2 || got.Texts != 4 || got.TextsUnread != 0 ||
		got.AsksAnswered != 1 || got.AsksRepliedBeforeAnswer != 1 {
		t.Errorf("alice's counts of %s = %+v, want 2 word questions of 4 texts, and one ask answered after a reply", day, got)
	}

	if words := rig.words("alice"); !slices.Equal(words, []string{"store"}) {
		t.Errorf("alice's words to an agent's token = %v, want [store]", words)
	}
	if response := dispatchRequest(t, rig.handler, http.MethodGet, "/api/v1/readers/Alice/words", nil, "alice"); response.Code != http.StatusOK ||
		len(decodeBody[readerWordsResponse](t, response).Items) != 1 {
		t.Errorf("alice's own words: %d %s", response.Code, response.Body.String())
	}
	if response := dispatchRequest(t, rig.handler, http.MethodGet, "/api/v1/readers/alice/words", nil, "bob"); response.Code != http.StatusForbidden ||
		!strings.Contains(response.Body.String(), codeReaderWordsForbidden) {
		t.Errorf("bob reads alice's words: %d %s, want 403 %s", response.Code, response.Body.String(), codeReaderWordsForbidden)
	}
	token := mintPersonalToken(t, rig.handler, "bob", "bob's agent", []string{model.ScopePerson})
	if response := agentRequest(t, rig.handler, http.MethodGet, "/api/v1/readers/alice/words", nil, token.Token); response.Code != http.StatusOK {
		t.Errorf("bob's agent's token reads alice's words: %d %s, want 200", response.Code, response.Body.String())
	}
	if words := rig.words("bob"); len(words) != 0 {
		t.Errorf("bob's words = %v, want none", words)
	}
	if response := agentRequest(t, rig.handler, http.MethodGet, "/api/v1/readers/alice/words?before=yesterday", nil, "agent-token"); response.Code != http.StatusBadRequest {
		t.Errorf("a malformed before: %d, want 400", response.Code)
	}

	// The written rules for a comment alice reads show her word, as the dispatch command reads
	// them for a session's write.
	rules := agentRequest(t, rig.handler, http.MethodPost, "/api/v1/writing-rules", map[string]any{
		"route": "POST /api/v1/issues/" + rig.issue + "/comments",
		"body":  map[string]any{"body": "The logs go to disk.", "actor": map[string]any{"kind": "session", "id": "s-writer"}},
	}, "agent-token")
	if rules.Code != http.StatusOK {
		t.Fatalf("the written rules: %d %s", rules.Code, rules.Body.String())
	}
	if got := decodeBody[writingRulesResponse](t, rules); got.Reader != "alice" || !strings.Contains(got.Text, "## Words that this reader asked about") || !strings.Contains(got.Text, "- store\n") {
		t.Errorf("the written rules for alice: reader %q, %d characters, want her word store", got.Reader, len(got.Text))
	}
}

// The negative control: while the stand-in fails, the text is kept, failed, and no word is
// recorded; once it answers, the text the job could not read is recorded.
func TestWordRecordsRetryAFailedTextUntilTheModelAnswers(t *testing.T) {
	rig := newRecordsRig(t)
	failing := modeltest.Reply{Status: http.StatusInternalServerError, Text: "the stand-in is down"}
	stand := modeltest.New(t,
		modeltest.Rule{Contains: "What is a store?", Once: true, Reply: failing},
		modeltest.Rule{Contains: "What is a store?", Once: true, Reply: failing},
		modeltest.Rule{Contains: "What is a store?", Once: true, Reply: failing},
		modeltest.Rule{Contains: "What is a store?", Reply: modeltest.Reply{Text: aboutStore}},
	)
	ask := rig.agentAsk("Which store should hold the cache?")
	rig.timedWrite("What is a store?", http.MethodPost, "/api/v1/issues/"+rig.issue+"/comments", map[string]any{"body": "What is a store?", "ask_id": ask}, http.StatusCreated)
	job := rig.job(stand, time.Now().Add(-time.Hour))
	if err := job.Pass(t.Context()); err != nil {
		t.Fatalf("a pass: %v", err)
	}
	records := rig.records()
	if len(records) != 1 || records[0].State != store.WordQuestionFailed || records[0].Attempts != 1 || records[0].Verdict != "" || len(records[0].Words) != 0 {
		t.Fatalf("records while the model fails = %+v, want the text kept, failed once, with no verdict", records)
	}
	day := time.Now().UTC().Format(time.DateOnly)
	if words := rig.words("alice"); len(words) != 0 {
		t.Errorf("alice's words while the model fails = %v, want none", words)
	}
	if got := row(rig.counts(day, day), day, "WRD", "alice"); got.WordQuestions != 0 || got.Texts != 1 || got.TextsUnread != 1 {
		t.Errorf("counts while the model fails = %+v, want one text, unread", got)
	}
	pollUntil(t, 30*time.Second, "the failed text recorded once the model answers", func() bool {
		if err := job.Pass(t.Context()); err != nil {
			t.Fatalf("a pass: %v", err)
		}
		return rig.done() == 1
	})
	if records := rig.records(); records[0].Verdict != wordrecords.WordMeaning || records[0].Attempts < 2 || !slices.Equal(records[0].Words, []string{"store"}) {
		t.Errorf("the record once the model answers = %+v, want the question about store after its failures", records[0])
	}
	if words := rig.words("alice"); !slices.Equal(words, []string{"store"}) {
		t.Errorf("alice's words once the model answers = %v, want [store]", words)
	}
}

// A person's replies posted through the server that quote the agent's words are read by the
// records job with the quoted lines as the part the reply points to and the lines the person typed
// as their text (INFRA-3231's check 5fb14fc7): "> holds rows that a delete already removed", then
// "What does that mean?", is recorded as a word question about the quoted clause; "> What is a
// store?", then "Done.", is recorded with verdict none and no words, though the stand-in reads
// "What is a store?" in the person's own lines as a question about store. The reader's words route
// then answers the clause alone.
func TestWordRecordsReadAQuotedReplyByThePersonsOwnLines(t *testing.T) {
	const clause = "holds rows that a delete already removed"
	rig := newRecordsRig(t)
	// The person's part of a request: what stands between its tags.
	personPart := func(r modeltest.Request) string {
		_, rest, _ := strings.Cut(r.Text, "<person_text kind=\"comment\">\n")
		part, _, _ := strings.Cut(rest, "\n</person_text>")
		return part
	}
	stand := modeltest.New(t,
		modeltest.Rule{
			Match: func(r modeltest.Request) bool {
				return strings.Contains(personPart(r), "What does that mean?") &&
					strings.Contains(r.Text, "<quoted_part>\n"+clause+"\n</quoted_part>")
			},
			Reply: modeltest.Reply{Text: `{"verdict":"asks_word_meaning","words":["` + clause + `"]}`},
		},
		modeltest.Rule{
			Match: func(r modeltest.Request) bool { return strings.Contains(personPart(r), "What is a store?") },
			Reply: modeltest.Reply{Text: aboutStore},
		},
		modeltest.Rule{Reply: modeltest.Reply{Text: notAbout}},
	)
	ask := rig.agentAsk("The mirror " + clause + ". What is a store, and which one holds the cache?")
	replies := []struct {
		body, verdict string
		words         []string
		event         int64
	}{
		{body: "> " + clause + "\nWhat does that mean?", verdict: wordrecords.WordMeaning, words: []string{clause}},
		{body: "> What is a store?\nDone.", verdict: wordrecords.None},
	}
	for i := range replies {
		written := dispatchRequest(t, rig.handler, http.MethodPost, "/api/v1/issues/"+rig.issue+"/comments", map[string]any{
			"body": replies[i].body, "ask_id": ask,
		}, "alice")
		if written.Code != http.StatusCreated {
			t.Fatalf("alice's reply %q: %d %s", replies[i].body, written.Code, written.Body.String())
		}
		comment := decodeBody[model.Comment](t, written).ID
		if err := rig.database.Pool.QueryRow(context.Background(), `
			select id from events where type = 'comment.created' and payload->>'id' = $1`, comment).Scan(&replies[i].event); err != nil {
			t.Fatalf("read the comment.created event of %q: %v", replies[i].body, err)
		}
	}
	rig.run(rig.job(stand, time.Now().Add(-time.Hour)))
	pollUntil(t, 30*time.Second, "both replies recorded with a verdict", func() bool { return rig.done() == 2 })
	records := rig.records()
	if len(records) != 2 {
		t.Fatalf("records = %+v, want one for each reply", records)
	}
	for _, reply := range replies {
		at := slices.IndexFunc(records, func(r wordRecord) bool { return r.EventID == reply.event })
		if at < 0 {
			t.Errorf("no record of %q (event %d) among %+v", reply.body, reply.event, records)
			continue
		}
		if got := records[at]; got.Person != "alice" || got.Verdict != reply.verdict || !slices.Equal(got.Words, reply.words) {
			t.Errorf("the record of %q = %+v, want alice's %s with words %q", reply.body, got, reply.verdict, reply.words)
		}
	}
	if words := rig.words("alice"); !slices.Equal(words, []string{clause}) {
		t.Errorf("alice's words = %q, want the quoted clause alone", words)
	}
}

// fixtureEvent appends an event to the rig's issue as a write would, at a time of the fixture's.
func (rig *recordsRig) fixtureEvent(eventType string, actor, payload map[string]any, at time.Time) int64 {
	rig.t.Helper()
	actorJSON, _ := json.Marshal(actor)
	payloadJSON, _ := json.Marshal(payload)
	var id int64
	if err := rig.database.Pool.QueryRow(context.Background(), `
		insert into events (issue_key, seq, type, actor, payload, notify, created_at)
		values ($1, (select coalesce(max(seq), 0) + 1 from events where issue_key = $1), $2, $3, $4, false, $5)
		returning id`, rig.issue, eventType, actorJSON, payloadJSON, at).Scan(&id); err != nil {
		rig.t.Fatalf("append %s: %v", eventType, err)
	}
	return id
}

// fixtureComment stores a comment on the rig's issue and appends its event.
func (rig *recordsRig) fixtureComment(actor map[string]any, body, replyTo string, at time.Time) (string, int64) {
	rig.t.Helper()
	authorJSON, _ := json.Marshal(actor)
	var id string
	if err := rig.database.Pool.QueryRow(context.Background(), `
		insert into comments (issue_key, author, body, reply_to, created_at)
		values ($1, $2, $3, nullif($4, '')::uuid, $5) returning id::text`, rig.issue, authorJSON, body, replyTo, at).Scan(&id); err != nil {
		rig.t.Fatalf("store a comment: %v", err)
	}
	payload := map[string]any{"id": id, "body": body, "issue_key": rig.issue}
	if replyTo != "" {
		payload["reply_to"] = replyTo
	}
	return id, rig.fixtureEvent("comment.created", actor, payload, at)
}

// The backfill: a job whose first pass starts on 2026-10-16 reads a fixture event log from that
// day on, records its questions with the agent text each answers, and reads nothing written
// before it. A session's text that a person typed (person_typed: true) is the person's its
// credential names, or nobody named's where it names none; a session's text no person typed is
// not read.
func TestWordRecordsBackfillTheEventLogFromItsFirstDay(t *testing.T) {
	rig := newRecordsRig(t)
	stand := modeltest.New(t,
		modeltest.Rule{Contains: "What is the shard map?", Reply: modeltest.Reply{Text: aboutShard}},
		modeltest.Rule{Contains: "What is a lane?", Reply: modeltest.Reply{Text: aboutLane}},
		modeltest.Rule{Contains: "Thanks, that helps.", Reply: modeltest.Reply{Text: notAbout}},
	)
	from := time.Date(2026, 10, 16, 0, 0, 0, 0, time.UTC)
	alice := map[string]any{"kind": "user", "id": "alice"}
	agent := map[string]any{"kind": "session", "id": "s-writer"}
	typed := map[string]any{"kind": "session", "id": "s-typed", "owner": "carol@example.com"}
	pod := map[string]any{"kind": "session", "id": "s-pod"}
	_, early := rig.fixtureComment(alice, "What is the shard map?", "", from.Add(-time.Hour))
	agentComment, _ := rig.fixtureComment(agent, "The shard map is warm.", "", from.Add(9*time.Hour))
	_, question := rig.fixtureComment(alice, "What is the shard map?", agentComment, from.Add(10*time.Hour))
	_, thanks := rig.fixtureComment(alice, "Thanks, that helps.", agentComment, from.Add(26*time.Hour))
	_, carol := rig.fixtureComment(typed, "What is a lane?", "", from.Add(27*time.Hour))
	carolTyped := rig.fixtureEvent("comment.created", typed, map[string]any{"id": "00000000-0000-4000-8000-000000000001", "body": "What is a lane?", "person_typed": true}, from.Add(28*time.Hour))
	podTyped := rig.fixtureEvent("comment.created", pod, map[string]any{"id": "00000000-0000-4000-8000-000000000002", "body": "What is a lane?", "person_typed": true}, from.Add(29*time.Hour))
	job := rig.job(stand, from)
	pollUntil(t, 30*time.Second, "the fixture's texts recorded", func() bool {
		if err := job.Pass(t.Context()); err != nil {
			t.Fatalf("a pass: %v", err)
		}
		return rig.done() == 4
	})
	byEvent := map[int64]wordRecord{}
	for _, r := range rig.records() {
		byEvent[r.EventID] = r
	}
	if _, read := byEvent[early]; read {
		t.Errorf("the text written before 2026-10-16 was read: %+v", byEvent[early])
	}
	if _, read := byEvent[carol]; read {
		t.Errorf("a session's text no person typed was read: %+v", byEvent[carol])
	}
	if r := byEvent[question]; r.Person != "alice" || r.Day != "2026-10-16" || r.Verdict != wordrecords.WordMeaning ||
		!slices.Equal(r.Words, []string{"shard map"}) || r.Answer != "comment:"+agentComment {
		t.Errorf("the backfilled question = %+v, want alice's of 2026-10-16 about the shard map, answering the agent's comment", r)
	}
	if r := byEvent[thanks]; r.Verdict != wordrecords.None || r.Day != "2026-10-17" {
		t.Errorf("the backfilled thanks = %+v, want none of 2026-10-17", r)
	}
	if r := byEvent[carolTyped]; r.Person != "carol@example.com" || !slices.Equal(r.Words, []string{"lane"}) {
		t.Errorf("the text carol typed in a session = %+v, want carol's question about lane", r)
	}
	if r := byEvent[podTyped]; r.Person != "" || !slices.Equal(r.Words, []string{"lane"}) || r.State != store.WordQuestionDone {
		t.Errorf("the text typed in a session whose credential names nobody = %+v, want a question with nobody named", r)
	}
	counts := rig.counts("2026-10-15", "2026-10-17")
	if got := row(counts, "2026-10-16", "WRD", "alice"); got.WordQuestions != 1 || got.Texts != 1 {
		t.Errorf("alice's counts of 2026-10-16 = %+v, want her one question", got)
	}
	if got := row(counts, "2026-10-15", "WRD", "alice"); got != (store.WritingCheckRow{}) {
		t.Errorf("counts of 2026-10-15 = %+v, want none", got)
	}
	if got := row(counts, "2026-10-17", "WRD", ""); got.WordQuestions != 1 || got.Texts != 1 {
		t.Errorf("the counts of nobody named on 2026-10-17 = %+v, want the pod's typed question apart", got)
	}
	if words := rig.words("carol@example.com"); !slices.Equal(words, []string{"lane"}) {
		t.Errorf("carol's words = %v, want [lane]", words)
	}
}

// GET /api/v1/readers/{login}/words pages the reader's words, the most recently asked first:
// limit of them from offset, total counting every word, a page past the last one empty with the
// total still counted, and a limit or an offset out of its bounds 400 INVALID_QUERY.
func TestTheReaderWordsArePaged(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	ctx := context.Background()
	asked := time.Date(2026, 1, 5, 12, 0, 0, 0, time.UTC)
	for i, word := range []string{"store", "node", "lane"} {
		if err := database.RecordDefineUse(ctx, store.DefineUse{
			DefinitionID: fmt.Sprintf("paged-%d", i), Person: "alice", Words: []string{word}, At: asked.Add(time.Duration(i) * time.Hour),
		}); err != nil {
			t.Fatalf("record alice's Define of %s: %v", word, err)
		}
	}
	page := func(query string) readerWordsResponse {
		t.Helper()
		response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/readers/alice/words?"+query, nil, "alice")
		if response.Code != http.StatusOK {
			t.Fatalf("alice's words ?%s: %d %s", query, response.Code, response.Body.String())
		}
		return decodeBody[readerWordsResponse](t, response)
	}
	words := func(answer readerWordsResponse) []string {
		var listed []string
		for _, word := range answer.Items {
			listed = append(listed, word.Word)
		}
		return listed
	}
	if first := page("limit=2"); !slices.Equal(words(first), []string{"lane", "node"}) || first.Total != 3 || first.Limit != 2 || first.Offset != 0 {
		t.Errorf("the first page = %v total %d limit %d offset %d, want lane and node of 3 at limit 2 offset 0", words(first), first.Total, first.Limit, first.Offset)
	}
	if second := page("limit=2&offset=2"); !slices.Equal(words(second), []string{"store"}) || second.Total != 3 || second.Offset != 2 {
		t.Errorf("the second page = %v total %d offset %d, want store of 3 at offset 2", words(second), second.Total, second.Offset)
	}
	if past := page("offset=3"); len(past.Items) != 0 || past.Total != 3 || past.Limit != listDefaultLimit || past.Offset != 3 {
		t.Errorf("a page past the last = %+v, want no word, total 3, limit %d and offset 3", past, listDefaultLimit)
	}
	if before := page("before=" + asked.Add(90*time.Minute).Format(time.RFC3339)); !slices.Equal(words(before), []string{"node", "store"}) || before.Total != 2 {
		t.Errorf("the words before the last Define = %v total %d, want node and store of 2", words(before), before.Total)
	}
	for _, query := range []string{"limit=0", "limit=201", "offset=-1"} {
		if response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/readers/alice/words?"+query, nil, "alice"); response.Code != http.StatusBadRequest ||
			!strings.Contains(response.Body.String(), "INVALID_QUERY") {
			t.Errorf("?%s: %d %s, want 400 INVALID_QUERY", query, response.Code, response.Body.String())
		}
	}
}
