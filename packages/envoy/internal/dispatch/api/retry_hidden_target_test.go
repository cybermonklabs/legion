package api

import (
	"context"
	"crypto/sha256"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// retriedItems is what the Retry rows retry, all carol's and all on PUB's issue nobody claimed
// (seedHiddenHolders' unclaimed), by name: comments that mention nothing, a role of PRIV
// (role:legion-priv-architect, which session-pub holds), session-priv (which acts in PRIV), a role
// session-priv holds (role:reviewer, so the mention resolved to session-priv), session-pub and that
// role of PRIV together, and session-pub alone; and messages with no target, to that role of PRIV,
// and to session-priv. carol sees PRIV, so each mention and each message reached its session.
type retriedItems struct {
	comments map[string]string
	messages map[string]string
}

// The names of the retried items, and the targets they name.
const (
	noMention          = "no mention"
	privateRoleMention = "a role of PRIV"
	hiddenSession      = "session-priv"
	hiddenHolderMntn   = "a role session-priv holds"
	visibleAndHidden   = "session-pub and a role of PRIV"
	visibleOnly        = "session-pub alone"
	noTarget           = "no target"
	unmentionedTarget  = "role:nobody-mentions"
)

// writeRetriedItems writes retriedItems through the routes as carol and waits for every mention's
// send to settle, so a Retry finds no attempt pending.
func writeRetriedItems(t *testing.T, seed hiddenHolderSeed) retriedItems {
	t.Helper()
	items := retriedItems{comments: map[string]string{}, messages: map[string]string{}}
	key := seed.unclaimed
	for name, targets := range map[string][]string{
		noMention:          nil,
		privateRoleMention: {"role:" + privateProjectRole},
		hiddenSession:      {"session:" + sessionOfPrivate},
		hiddenHolderMntn:   {"role:" + hiddenHolderRole},
		visibleAndHidden:   {"session:" + sessionOfPublic, "role:" + privateProjectRole},
		visibleOnly:        {"session:" + sessionOfPublic},
	} {
		body := map[string]any{"body": "A remark carol makes."}
		if len(targets) > 0 {
			mentions := []map[string]string{}
			for _, target := range targets {
				mentions = append(mentions, map[string]string{"target": target})
			}
			body["mentions"] = mentions
		}
		items.comments[name] = createdID(t, seed.request(t, callerCarol, http.MethodPost, "/api/v1/issues/"+key+"/comments", body))
	}
	for name, target := range map[string]string{
		noTarget:           "",
		privateRoleMention: "role:" + privateProjectRole,
		hiddenSession:      "session:" + sessionOfPrivate,
	} {
		body := map[string]any{"body": "A note carol sends."}
		if target != "" {
			body["target"], body["delivery"] = target, "btw"
		}
		items.messages[name] = createdID(t, seed.request(t, callerCarol, http.MethodPost, "/api/v1/issues/"+key+"/messages", body))
	}
	awaitBackgroundWrites(t, seed.server)
	return items
}

// retry is caller's Retry of comment or message id (kind "comments" or "messages") with body, and
// the statements the request made.
func retry(t *testing.T, seed hiddenHolderSeed, counter *contextStatements, caller privateCaller, kind, id string, body map[string]any) (*httptest.ResponseRecorder, int64) {
	t.Helper()
	before := counter.sent.Load()
	answer := seed.requestIn(t, context.WithValue(t.Context(), contextStatementsKey{}, true), caller, http.MethodPost, "/api/v1/"+kind+"/"+id+"/deliveries", body)
	sent := counter.sent.Load() - before
	awaitBackgroundWrites(t, seed.server)
	return answer, sent
}

// retryBodies are the three Retries the rows make of a comment whose mention its caller does not
// read: with no target, naming the hidden target, and naming a target nobody mentions.
func retryBodies(hidden string) map[string]map[string]any {
	return map[string]map[string]any{
		"no target":                {"delivery": "btw"},
		"the hidden target":        {"delivery": "btw", "target": hidden},
		"a target nobody mentions": {"delivery": "btw", "target": unmentionedTarget},
	}
}

// TestARetryNamesNoMentionItsCallerDoesNotRead: alice, an open member who does not see PRIV, reads
// none of three of carol's mentions on PUB's issue - a role of PRIV, session-priv, and a role
// session-priv holds - since GET /api/v1/comments leaves each out for her
// (leaveOutHiddenCommentSessions). So POST /api/v1/comments/{id}/deliveries answers each such comment
// byte for byte as it answers carol's comment that mentions nothing - with no target, naming the
// hidden target, and naming a target nobody mentions - at the same cost in statements, and names
// neither the role nor the session. Of a comment mentioning session-pub and that role of PRIV, a
// Retry with no target takes session-pub, the one mention she reads, as a comment mentioning
// session-pub alone answers it, and naming the role answers as that comment answers it. carol, who
// sees PRIV, retries the role's mention with no target and reaches session-pub: the listener
// answers, so alice's answers are not a listener's silence.
//
// Control: retryableMentionTargets without leaveOutHiddenCommentSessions (the unfiltered read
// restored); alice's Retry then answers 201 naming role:legion-priv-architect and session-priv, and
// 400 for a target nobody mentions, and each row fails naming the target.
func TestARetryNamesNoMentionItsCallerDoesNotRead(t *testing.T) {
	t.Parallel()
	counter := &contextStatements{}
	seed := seedHiddenHolders(t, testServerOptions{store: storetest.TracedStore(t, storetest.Open(t), counter)})
	items := writeRetriedItems(t, seed)
	comment := func(caller privateCaller, name string, body map[string]any) (*httptest.ResponseRecorder, int64) {
		t.Helper()
		return retry(t, seed, counter, caller, "comments", items.comments[name], body)
	}

	if reached, _ := comment(callerCarol, privateRoleMention, map[string]any{"delivery": "btw"}); reached.Code != http.StatusCreated ||
		!strings.Contains(reached.Body.String(), `"session_id":"`+sessionOfPublic+`"`) || !strings.Contains(reached.Body.String(), `"state":"sent"`) {
		t.Fatalf("carol's Retry of her mention of role:%s answers %d %s, want it sent to %s", privateProjectRole, reached.Code, reached.Body.String(), sessionOfPublic)
	}

	for name, hidden := range map[string]string{
		privateRoleMention: "role:" + privateProjectRole,
		hiddenSession:      "session:" + sessionOfPrivate,
		hiddenHolderMntn:   "role:" + hiddenHolderRole,
	} {
		for retried, body := range retryBodies(hidden) {
			// Each pair is counted after a first call of the same pair, so neither pays a cache's
			// first fill.
			comment(callerAlice, name, body)
			comment(callerAlice, noMention, body)
			got, gotCost := comment(callerAlice, name, body)
			want, wantCost := comment(callerAlice, noMention, body)
			if got.Code != want.Code || got.Body.String() != want.Body.String() {
				t.Errorf("alice's Retry with %s of carol's mention of %s answers %d %s; of carol's comment that mentions nothing, %d %s: want one answer",
					retried, hidden, got.Code, strings.TrimSpace(got.Body.String()), want.Code, strings.TrimSpace(want.Body.String()))
			}
			for _, named := range []string{privateProjectRole, sessionOfPrivate, hiddenHolderRole} {
				if strings.Contains(got.Body.String(), named) {
					t.Errorf("alice's Retry with %s of carol's mention of %s names %s: %s", retried, hidden, named, strings.TrimSpace(got.Body.String()))
				}
			}
			if gotCost != wantCost {
				t.Errorf("alice's Retry with %s of carol's mention of %s costs %d statements; of carol's comment that mentions nothing, %d: want the same",
					retried, hidden, gotCost, wantCost)
			}
		}
	}

	for retried, body := range retryBodies("role:" + privateProjectRole) {
		got, _ := comment(callerAlice, visibleAndHidden, body)
		want, _ := comment(callerAlice, visibleOnly, body)
		gotText := timestampsAndIDs.ReplaceAllString(got.Body.String(), "<varies>")
		wantText := timestampsAndIDs.ReplaceAllString(want.Body.String(), "<varies>")
		if got.Code != want.Code || gotText != wantText {
			t.Errorf("alice's Retry with %s of carol's mentions of session-pub and role:%s answers %d %s; of her mention of session-pub alone, %d %s: want one answer",
				retried, privateProjectRole, got.Code, strings.TrimSpace(gotText), want.Code, strings.TrimSpace(wantText))
		}
		if retried == "no target" && (got.Code != http.StatusCreated || !strings.Contains(got.Body.String(), `"target":"session:`+sessionOfPublic+`"`)) {
			t.Errorf("alice's Retry with no target of carol's mentions of session-pub and role:%s answers %d %s, want it to take session:%s, the one mention she reads",
				privateProjectRole, got.Code, strings.TrimSpace(got.Body.String()), sessionOfPublic)
		}
	}
}

// TestARetryOfAMessageNamesNoTargetItsCallerDoesNotRead: alice reads carol's messages on PUB's
// issue to a role of PRIV and to session-priv with no target (leaveOutHiddenMessageSessions), so
// POST /api/v1/messages/{id}/deliveries answers each byte for byte as it answers carol's message
// with no target, at the same cost in statements, and names neither the role nor the session. The
// seed's listener answers: carol's Retry of the same two messages reaches session-pub and
// session-priv, so alice's answers are not a listener's silence.
//
// Control: createDelivery without leaveOutHiddenMessageSessions (the unfiltered target restored);
// alice's Retry then answers 201, its error naming role:legion-priv-architect and its session
// session-priv, and the row fails naming each.
func TestARetryOfAMessageNamesNoTargetItsCallerDoesNotRead(t *testing.T) {
	t.Parallel()
	counter := &contextStatements{}
	seed := seedHiddenHolders(t, testServerOptions{store: storetest.TracedStore(t, storetest.Open(t), counter)})
	items := writeRetriedItems(t, seed)
	message := func(caller privateCaller, name string) (*httptest.ResponseRecorder, int64) {
		t.Helper()
		return retry(t, seed, counter, caller, "messages", items.messages[name], map[string]any{"delivery": "btw"})
	}
	for name, session := range map[string]string{privateRoleMention: sessionOfPublic, hiddenSession: sessionOfPrivate} {
		if reached, _ := message(callerCarol, name); reached.Code != http.StatusCreated ||
			!strings.Contains(reached.Body.String(), `"session_id":"`+session+`"`) || !strings.Contains(reached.Body.String(), `"state":"sent"`) {
			t.Fatalf("carol's Retry of her message to %s answers %d %s, want it sent to %s", name, reached.Code, reached.Body.String(), session)
		}
	}
	for name, hidden := range map[string]string{privateRoleMention: "role:" + privateProjectRole, hiddenSession: "session:" + sessionOfPrivate} {
		message(callerAlice, name)
		message(callerAlice, noTarget)
		got, gotCost := message(callerAlice, name)
		want, wantCost := message(callerAlice, noTarget)
		if got.Code != want.Code || got.Body.String() != want.Body.String() {
			t.Errorf("alice's Retry of carol's message to %s answers %d %s; of carol's message with no target, %d %s: want one answer",
				hidden, got.Code, strings.TrimSpace(got.Body.String()), want.Code, strings.TrimSpace(want.Body.String()))
		}
		for _, named := range []string{privateProjectRole, sessionOfPrivate, sessionOfPublic} {
			if strings.Contains(got.Body.String(), named) {
				t.Errorf("alice's Retry of carol's message to %s names %s: %s", hidden, named, strings.TrimSpace(got.Body.String()))
			}
		}
		if gotCost != wantCost {
			t.Errorf("alice's Retry of carol's message to %s costs %d statements; of carol's message with no target, %d: want the same", hidden, gotCost, wantCost)
		}
	}
}

// TestWithNoPrivateProjectARetryAnswersAsBefore (R9): once PRIV is public again the deployment
// holds no private project and alice sees every object, so each Retry of the rows above answers
// her as the base did: a mention of the role of PRIV, of session-priv and of the role it holds each
// reaches its session, the comment mentioning session-pub and the role refuses a Retry with no
// target, and naming that role or a target nobody mentions answers as the base answered it; each
// message reaches its session. Each answer is logged with its times and ids blanked, beside the
// sha256 of what it logs, so a run at the base and a run at the head compare byte for byte.
//
// Control: retryableMentionTargets answering no target at all, then createDelivery reading every
// message as one with no target; alice's Retries of the comments, then of the messages, answer 404
// and the row fails.
func TestWithNoPrivateProjectARetryAnswersAsBefore(t *testing.T) {
	t.Parallel()
	counter := &contextStatements{}
	seed := seedHiddenHolders(t, testServerOptions{store: storetest.TracedStore(t, storetest.Open(t), counter)})
	items := writeRetriedItems(t, seed)
	seed.setVisibility(t, seedPrivate, model.ProjectPublic)
	for _, row := range []struct {
		kind, name string
		body       map[string]any
		status     int
		holds      string
	}{
		{"comments", privateRoleMention, map[string]any{"delivery": "btw"}, http.StatusCreated, `"session_id":"` + sessionOfPublic + `"`},
		{"comments", privateRoleMention, map[string]any{"delivery": "btw", "target": unmentionedTarget}, http.StatusBadRequest, "target must name one of the comment mentions"},
		{"comments", hiddenSession, map[string]any{"delivery": "btw", "target": "session:" + sessionOfPrivate}, http.StatusCreated, `"session_id":"` + sessionOfPrivate + `"`},
		{"comments", hiddenHolderMntn, map[string]any{"delivery": "btw"}, http.StatusCreated, `"session_id":"` + sessionOfPrivate + `"`},
		{"comments", visibleAndHidden, map[string]any{"delivery": "btw"}, http.StatusBadRequest, "target is required when a comment has multiple mentions"},
		{"comments", visibleAndHidden, map[string]any{"delivery": "btw", "target": "role:" + privateProjectRole}, http.StatusCreated, `"session_id":"` + sessionOfPublic + `"`},
		{"messages", privateRoleMention, map[string]any{"delivery": "btw"}, http.StatusCreated, `"session_id":"` + sessionOfPublic + `"`},
		{"messages", hiddenSession, map[string]any{"delivery": "btw"}, http.StatusCreated, `"session_id":"` + sessionOfPrivate + `"`},
	} {
		id := items.comments[row.name]
		if row.kind == "messages" {
			id = items.messages[row.name]
		}
		answer, _ := retry(t, seed, counter, callerAlice, row.kind, id, row.body)
		blanked := timestampsAndIDs.ReplaceAllString(answer.Body.String(), "<varies>")
		t.Logf("R9 %s of %s with %v: %d sha256 %x: %s", row.kind, row.name, row.body, answer.Code, sha256.Sum256([]byte(blanked)), strings.TrimSpace(blanked))
		if answer.Code != row.status || !strings.Contains(answer.Body.String(), row.holds) {
			t.Errorf("PRIV public: alice's Retry of the %s %q with %v answers %d %s, want %d holding %s, as before",
				row.kind, row.name, row.body, answer.Code, strings.TrimSpace(answer.Body.String()), row.status, row.holds)
		}
	}
}

// strandFirstAttempt puts message id's attempt 1 back as a process that died inside its send
// leaves it: pending, pinned to the session it went to, made and claimed long enough ago that the
// delivery sweep owes it (owedMessageAttempt).
func strandFirstAttempt(t *testing.T, seed hiddenHolderSeed, id string) {
	t.Helper()
	if _, err := seed.database.Pool.Exec(t.Context(), `
		update message_deliveries
		set state = 'pending', error = null, envelope_id = null,
		    claimed_at = now() - interval '10 minutes', created_at = now() - interval '10 minutes'
		where message_id = $1::uuid and attempt = 1`, id); err != nil {
		t.Fatalf("strand attempt 1 of %s: %v", id, err)
	}
}

// attemptRows is message id's attempts as "<attempt> <state> <session> <claim lapsed>", in order.
func attemptRows(t *testing.T, seed hiddenHolderSeed, id string) []string {
	t.Helper()
	rows, err := seed.database.Pool.Query(t.Context(), `
		select attempt || ' ' || state || ' ' || coalesce(nullif(session_id, ''), '-') || ' ' ||
		       (claimed_at is null or claimed_at < now() - interval '1 minute')
		from message_deliveries where message_id = $1::uuid order by attempt`, id)
	if err != nil {
		t.Fatalf("read the attempts of %s: %v", id, err)
	}
	defer rows.Close()
	var attempts []string
	for rows.Next() {
		var attempt string
		if err := rows.Scan(&attempt); err != nil {
			t.Fatalf("read the attempts of %s: %v", id, err)
		}
		attempts = append(attempts, attempt)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the attempts of %s: %v", id, err)
	}
	return attempts
}

// TestARetryLeavesAnAttemptPinnedToASessionItsCallerDoesNotSee: carol messages PUB's issue to
// role:reviewer, which session-priv holds, and her attempt 1 to session-priv is stranded by a
// process that died inside its send. alice reads the message's target and not that attempt
// (leaveOutHiddenMessageSessions), so her Retry leaves the attempt as it stands and opens her own,
// resolved as she reads the role - no holder she sees - and answers byte for byte as her Retry of
// carol's twin message whose attempt 1 was sent, naming nothing of session-priv. The delivery
// sweep then still sends carol's attempt 1 to session-priv, since alice's attempt failed and so
// stands in no attempt's place (owedMessageAttempt). carol, who sees session-priv, resumes her own
// stranded attempt 1 as before; and with no private project (R9) so does alice.
//
// Control: resumableBySQL dropped from messageDeliveryRoute and recordPendingMessageDelivery (the
// unfiltered resume restored); alice's Retry then resumes carol's attempt 1, answers it naming
// session-priv and settles it failed, and the row fails.
func TestARetryLeavesAnAttemptPinnedToASessionItsCallerDoesNotSee(t *testing.T) {
	t.Parallel()
	seed := seedHiddenHolders(t, testServerOptions{})
	send := func() string {
		t.Helper()
		return createdID(t, seed.request(t, callerCarol, http.MethodPost, "/api/v1/issues/"+seed.unclaimed+"/messages",
			map[string]any{"body": "A note for the reviewer.", "target": "role:" + hiddenHolderRole, "delivery": "btw"}))
	}
	stranded, twin, carols, publicLater := send(), send(), send(), send()
	awaitBackgroundWrites(t, seed.server)
	strandFirstAttempt(t, seed, stranded)
	strandFirstAttempt(t, seed, carols)
	retryOf := func(caller privateCaller, id string) *httptest.ResponseRecorder {
		t.Helper()
		answer := seed.request(t, caller, http.MethodPost, "/api/v1/messages/"+id+"/deliveries", map[string]any{"delivery": "btw"})
		awaitBackgroundWrites(t, seed.server)
		return answer
	}

	got, want := retryOf(callerAlice, stranded), retryOf(callerAlice, twin)
	gotText := timestampsAndIDs.ReplaceAllString(got.Body.String(), "<varies>")
	wantText := timestampsAndIDs.ReplaceAllString(want.Body.String(), "<varies>")
	if got.Code != want.Code || gotText != wantText {
		t.Errorf("alice's Retry of carol's message whose attempt to session-priv is stranded answers %d %s; of its twin whose attempt was sent, %d %s: want one answer",
			got.Code, strings.TrimSpace(gotText), want.Code, strings.TrimSpace(wantText))
	}
	if strings.Contains(got.Body.String(), sessionOfPrivate) {
		t.Errorf("alice's Retry names %s, a session she does not see: %s", sessionOfPrivate, strings.TrimSpace(got.Body.String()))
	}
	if attempts := attemptRows(t, seed, stranded); len(attempts) == 0 || attempts[0] != "1 pending "+sessionOfPrivate+" true" {
		t.Errorf("after alice's Retry carol's attempts are %v, want attempt 1 left pending to %s, its claim lapsed", attempts, sessionOfPrivate)
	}

	if resumed := retryOf(callerCarol, carols); resumed.Code != http.StatusCreated ||
		!strings.Contains(resumed.Body.String(), `"attempt":1,`) || !strings.Contains(resumed.Body.String(), `"session_id":"`+sessionOfPrivate+`"`) ||
		!strings.Contains(resumed.Body.String(), `"state":"sent"`) {
		t.Errorf("carol's Retry of her stranded attempt answers %d %s, want attempt 1 resumed and sent to %s", resumed.Code, strings.TrimSpace(resumed.Body.String()), sessionOfPrivate)
	}

	if err := seed.server.resumeOwedDeliveries(t.Context()); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	awaitBackgroundWrites(t, seed.server)
	if attempts := attemptRows(t, seed, stranded); len(attempts) == 0 || !strings.HasPrefix(attempts[0], "1 sent "+sessionOfPrivate+" ") {
		t.Errorf("after the delivery sweep carol's attempts are %v, want attempt 1 sent to %s", attempts, sessionOfPrivate)
	}

	seed.setVisibility(t, seedPrivate, model.ProjectPublic)
	strandFirstAttempt(t, seed, publicLater)
	if resumed := retryOf(callerAlice, publicLater); resumed.Code != http.StatusCreated ||
		!strings.Contains(resumed.Body.String(), `"attempt":1,`) || !strings.Contains(resumed.Body.String(), `"session_id":"`+sessionOfPrivate+`"`) ||
		!strings.Contains(resumed.Body.String(), `"state":"sent"`) {
		t.Errorf("PRIV public: alice's Retry of carol's stranded attempt answers %d %s, want attempt 1 resumed and sent to %s, as before",
			resumed.Code, strings.TrimSpace(resumed.Body.String()), sessionOfPrivate)
	}
}

// TestTheSweepSendsAMessageAttemptNoLaterAttemptStandsInThePlaceOf: a message's stranded attempt 1
// (mode btw) is the delivery sweep's to send while no later attempt stands in its place
// (owedMessageAttempt). A later attempt that failed with a refusal, in either mode, or that failed
// in the same mode with a cause that says it may have landed (the same idempotency key, which the
// listener drops as a repeat), stands in no attempt's place, and the sweep sends attempt 1. A later
// attempt that failed in another mode with such a cause, one still in flight, and one sent each
// stand in its place, and attempt 1 stays pending.
//
// Controls: owedMessageAttempt owing only a message's latest attempt (the rule before); the three
// first cases then leave attempt 1 pending and fail. And the rule without its arm for a later
// attempt in another mode that may have landed; the fourth case then sends attempt 1 and fails.
func TestTheSweepSendsAMessageAttemptNoLaterAttemptStandsInThePlaceOf(t *testing.T) {
	t.Parallel()
	seed := seedHiddenHolders(t, testServerOptions{})
	const refusal = "no holder for role reviewer"
	cases := []struct {
		name, state, delivery string
		failure               *string
		liveClaim             bool
		swept                 bool
	}{
		{"a later attempt refused in the same mode", "failed", "btw", new(refusal), false, true},
		{"a later attempt refused in another mode", "failed", "steer", new(refusal), false, true},
		{"a later attempt that may have landed, in the same mode", "failed", "btw", new(receiptTimeoutText), false, true},
		{"a later attempt that may have landed, in another mode", "failed", "steer", new(storeUnconfirmedText), false, false},
		{"a later attempt in flight", "pending", "btw", nil, true, false},
		{"a later attempt sent", "sent", "btw", nil, false, false},
	}
	ids := make([]string, len(cases))
	for index, test := range cases {
		ids[index] = createdID(t, seed.request(t, callerCarol, http.MethodPost, "/api/v1/issues/"+seed.unclaimed+"/messages",
			map[string]any{"body": "A note for session-pub.", "target": "session:" + sessionOfPublic, "delivery": "btw"}))
		awaitBackgroundWrites(t, seed.server)
		// carol's Retry opens attempt 2, sent; it is then written as the case's later attempt.
		seed.mustRequest(t, callerCarol, http.MethodPost, "/api/v1/messages/"+ids[index]+"/deliveries", map[string]any{"delivery": "btw"}, http.StatusCreated)
		awaitBackgroundWrites(t, seed.server)
		if _, err := seed.database.Pool.Exec(t.Context(), `
			update message_deliveries
			set state = $2, delivery = $3, error = $4,
			    envelope_id = case when $2 = 'sent' then envelope_id end,
			    claimed_at = case when $5 then now() else now() - interval '10 minutes' end
			where message_id = $1::uuid and attempt = 2`, ids[index], test.state, test.delivery, test.failure, test.liveClaim); err != nil {
			t.Fatalf("%s: write attempt 2: %v", test.name, err)
		}
		strandFirstAttempt(t, seed, ids[index])
	}
	if err := seed.server.resumeOwedDeliveries(t.Context()); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	awaitBackgroundWrites(t, seed.server)
	for index, test := range cases {
		attempts := attemptRows(t, seed, ids[index])
		want := "1 pending " + sessionOfPublic + " true"
		if test.swept {
			want = "1 sent " + sessionOfPublic + " "
		}
		if len(attempts) == 0 || !strings.HasPrefix(attempts[0], want) {
			t.Errorf("%s: after the sweep the attempts are %v, want attempt 1 %q", test.name, attempts, want)
		}
	}
}

// writeRepliedComments writes, as carol on PUB's issue nobody claimed, a comment that mentions
// nothing, one that mentions a role of PRIV (role:legion-priv-architect, whose attempt 1 reaches
// session-pub), one that mentions session-priv and one that mentions session-pub, by name, and
// waits for each mention's send.
func writeRepliedComments(t *testing.T, seed hiddenHolderSeed) map[string]string {
	t.Helper()
	ids := map[string]string{}
	for name, target := range map[string]string{
		noMention:          "",
		privateRoleMention: "role:" + privateProjectRole,
		hiddenSession:      "session:" + sessionOfPrivate,
		visibleOnly:        "session:" + sessionOfPublic,
	} {
		body := map[string]any{"body": "A remark carol makes."}
		if target != "" {
			body["mentions"] = []map[string]string{{"target": target}}
		}
		ids[name] = createdID(t, seed.request(t, callerCarol, http.MethodPost, "/api/v1/issues/"+seed.unclaimed+"/comments", body))
	}
	awaitBackgroundWrites(t, seed.server)
	return ids
}

// replyAs is caller's reply, as session, to attempt 1 of comment id with body, naming target where
// it is not empty.
func replyAs(t *testing.T, seed hiddenHolderSeed, caller privateCaller, session, id, target string) *httptest.ResponseRecorder {
	t.Helper()
	body := map[string]any{"actor": map[string]string{"kind": "session", "id": session}, "attempt": 1, "body": "Done."}
	if target != "" {
		body["target"] = target
	}
	answer := seed.request(t, caller, http.MethodPost, "/api/v1/comments/"+id+"/reply", body)
	awaitBackgroundWrites(t, seed.server)
	return answer
}

// TestAReplyAnswersAMentionItsCallerDoesNotReadAsOneTheCommentDoesNotHave: the deployment's shared
// token sees PUB alone, so it reads none of carol's mentions on PUB's issue of a role of PRIV and of
// session-priv (leaveOutHiddenCommentSessions), and it may not act for them. Its reply as the
// mentioned session - session-pub, which attempt 1 of the role's mention reached, and session-priv
// - with no target, naming the hidden target, and naming a target nobody mentions, each answers
// byte for byte as the same reply to carol's comment that mentions nothing does, and posts no
// reply. The same token's reply as session-pub to carol's mention of session-pub, which it reads,
// is posted, so the refusal is the target's alone.
//
// Control: replyComment reading the mentions as the server's reader (the unfiltered reads
// restored); the shared token's reply with no target or naming the target is then posted, 201, and
// the row fails naming the target.
func TestAReplyAnswersAMentionItsCallerDoesNotReadAsOneTheCommentDoesNotHave(t *testing.T) {
	t.Parallel()
	seed := seedHiddenHolders(t, testServerOptions{})
	ids := writeRepliedComments(t, seed)
	for name, mention := range map[string]struct{ session, target string }{
		privateRoleMention: {sessionOfPublic, "role:" + privateProjectRole},
		hiddenSession:      {sessionOfPrivate, "session:" + sessionOfPrivate},
	} {
		for _, target := range []string{"", mention.target, unmentionedTarget} {
			got := replyAs(t, seed, callerSharedToken, mention.session, ids[name], target)
			want := replyAs(t, seed, callerSharedToken, mention.session, ids[noMention], target)
			if got.Code != want.Code || got.Body.String() != want.Body.String() {
				t.Errorf("the shared token's reply as %s naming %q to carol's mention of %s answers %d %s; to her comment that mentions nothing, %d %s: want one answer",
					mention.session, target, mention.target, got.Code, strings.TrimSpace(got.Body.String()), want.Code, strings.TrimSpace(want.Body.String()))
			}
			if got.Code < http.StatusBadRequest {
				t.Errorf("the shared token's reply as %s naming %q to carol's mention of %s was taken: %d %s", mention.session, target, mention.target, got.Code, strings.TrimSpace(got.Body.String()))
			}
		}
	}
	if taken := replyAs(t, seed, callerSharedToken, sessionOfPublic, ids[visibleOnly], ""); taken.Code != http.StatusCreated {
		t.Errorf("the shared token's reply as session-pub to carol's mention of session-pub answers %d %s, want it posted", taken.Code, strings.TrimSpace(taken.Body.String()))
	}
}

// TestWithNoPrivateProjectAReplyAnswersAsBefore (R9): once PRIV is public again the shared token
// sees every object, so its replies as session-pub and session-priv to carol's mentions of the role
// of PRIV and of session-priv are posted as the base posted them. Each answer is logged with its
// times and ids blanked, beside the sha256 of what it logs, so a run at the base and a run at the
// head compare byte for byte.
//
// Control: mentionsReadBy leaving every mention out; the replies then answer 400 and the row fails.
func TestWithNoPrivateProjectAReplyAnswersAsBefore(t *testing.T) {
	t.Parallel()
	seed := seedHiddenHolders(t, testServerOptions{})
	ids := writeRepliedComments(t, seed)
	seed.setVisibility(t, seedPrivate, model.ProjectPublic)
	for _, reply := range []struct{ name, session string }{{privateRoleMention, sessionOfPublic}, {hiddenSession, sessionOfPrivate}} {
		answer := replyAs(t, seed, callerSharedToken, reply.session, ids[reply.name], "")
		blanked := timestampsAndIDs.ReplaceAllString(answer.Body.String(), "<varies>")
		t.Logf("R9 reply as %s to %s: %d sha256 %x: %s", reply.session, reply.name, answer.Code, sha256.Sum256([]byte(blanked)), strings.TrimSpace(blanked))
		if answer.Code != http.StatusCreated {
			t.Errorf("PRIV public: the shared token's reply as %s to carol's mention of %s answers %d %s, want 201, as before",
				reply.session, reply.name, answer.Code, strings.TrimSpace(answer.Body.String()))
		}
	}
}

// writeRepliedMessages writes, as carol on PUB's issue nobody claimed, a message with no target,
// one to a role of PRIV (role:legion-priv-architect, whose attempt 1 reaches session-pub), one to
// session-priv and one to session-pub, by name, and waits for each send.
func writeRepliedMessages(t *testing.T, seed hiddenHolderSeed) map[string]string {
	t.Helper()
	ids := map[string]string{}
	for name, target := range map[string]string{
		noTarget:           "",
		privateRoleMention: "role:" + privateProjectRole,
		hiddenSession:      "session:" + sessionOfPrivate,
		visibleOnly:        "session:" + sessionOfPublic,
	} {
		body := map[string]any{"body": "A note carol sends."}
		if target != "" {
			body["target"], body["delivery"] = target, "btw"
		}
		ids[name] = createdID(t, seed.request(t, callerCarol, http.MethodPost, "/api/v1/issues/"+seed.unclaimed+"/messages", body))
	}
	awaitBackgroundWrites(t, seed.server)
	return ids
}

// replyToMessageAs is caller's reply, as session, to attempt 1 of message id.
func replyToMessageAs(t *testing.T, seed hiddenHolderSeed, caller privateCaller, session, id string) *httptest.ResponseRecorder {
	t.Helper()
	answer := seed.request(t, caller, http.MethodPost, "/api/v1/messages/"+id+"/reply",
		map[string]any{"actor": map[string]string{"kind": "session", "id": session}, "attempt": 1, "body": "Done."})
	awaitBackgroundWrites(t, seed.server)
	return answer
}

// TestAReplyToAMessageWhoseTargetItsCallerDoesNotReadAnswersAsNoTarget: the shared token sees PUB
// alone, so it reads carol's messages on PUB's issue to a role of PRIV and to session-priv with no
// target (leaveOutHiddenMessageSessions), and it may not act for them. Its reply as
// legion-priv-architect's holder, session-pub, which attempt 1 of that message reached, and as
// session-priv, each answers byte for byte as the same reply to carol's message with no target does,
// and posts no reply. The same token's reply as session-pub to carol's message to session-pub, which
// it reads, is posted.
//
// Control: replyMessage reading the target as the server's reader (the unfiltered reads restored);
// the shared token's replies are then posted, 201, and the row fails naming the target.
func TestAReplyToAMessageWhoseTargetItsCallerDoesNotReadAnswersAsNoTarget(t *testing.T) {
	t.Parallel()
	seed := seedHiddenHolders(t, testServerOptions{})
	ids := writeRepliedMessages(t, seed)
	for name, session := range map[string]string{privateRoleMention: sessionOfPublic, hiddenSession: sessionOfPrivate} {
		got := replyToMessageAs(t, seed, callerSharedToken, session, ids[name])
		want := replyToMessageAs(t, seed, callerSharedToken, session, ids[noTarget])
		if got.Code != want.Code || got.Body.String() != want.Body.String() {
			t.Errorf("the shared token's reply as %s to carol's message to %s answers %d %s; to her message with no target, %d %s: want one answer",
				session, name, got.Code, strings.TrimSpace(got.Body.String()), want.Code, strings.TrimSpace(want.Body.String()))
		}
		if got.Code < http.StatusBadRequest {
			t.Errorf("the shared token's reply as %s to carol's message to %s was taken: %d %s", session, name, got.Code, strings.TrimSpace(got.Body.String()))
		}
	}
	if taken := replyToMessageAs(t, seed, callerSharedToken, sessionOfPublic, ids[visibleOnly]); taken.Code != http.StatusCreated {
		t.Errorf("the shared token's reply as session-pub to carol's message to session-pub answers %d %s, want it posted", taken.Code, strings.TrimSpace(taken.Body.String()))
	}
}

// TestWithNoPrivateProjectAReplyToAMessageAnswersAsBefore (R9): once PRIV is public again the
// shared token sees every object, so its replies as session-pub and session-priv to carol's messages
// to the role of PRIV and to session-priv are posted as the base posted them. Each answer is logged
// with its times and ids blanked, beside the sha256 of what it logs, so a run at the base and a run
// at the head compare byte for byte.
//
// Control: replyMessage reading every target as none; the replies then answer 404 and the row fails.
func TestWithNoPrivateProjectAReplyToAMessageAnswersAsBefore(t *testing.T) {
	t.Parallel()
	seed := seedHiddenHolders(t, testServerOptions{})
	ids := writeRepliedMessages(t, seed)
	seed.setVisibility(t, seedPrivate, model.ProjectPublic)
	for _, reply := range []struct{ name, session string }{{privateRoleMention, sessionOfPublic}, {hiddenSession, sessionOfPrivate}} {
		answer := replyToMessageAs(t, seed, callerSharedToken, reply.session, ids[reply.name])
		blanked := timestampsAndIDs.ReplaceAllString(answer.Body.String(), "<varies>")
		t.Logf("R9 message reply as %s to %s: %d sha256 %x: %s", reply.session, reply.name, answer.Code, sha256.Sum256([]byte(blanked)), strings.TrimSpace(blanked))
		if answer.Code != http.StatusCreated {
			t.Errorf("PRIV public: the shared token's reply as %s to carol's message to %s answers %d %s, want 201, as before",
				reply.session, reply.name, answer.Code, strings.TrimSpace(answer.Body.String()))
		}
	}
}
