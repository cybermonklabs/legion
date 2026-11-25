package api

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// sessionOfBob is an agent of bob's, the restricted member PRIV's list names and PUB's does not: it
// sees PRIV and not PUB (access.SessionSeesProjectClause). sightSessions is the stand-in listener's
// record of it, live, taking btw and steer, and holding the role reviewer.
const (
	sessionOfBob  = "session-bob"
	sightSessions = `[{"session_id":"session-bob","title":"Bob's agent","capabilities":["btw","steer"],"roles":["reviewer"]}]`
)

// seedSessionOfBob ties sessionOfBob to bob, as the broker token that proved it records, and places
// it in PRIV, where it acted, so carol, who sees PRIV, sees the session (access.SessionClause) and
// may name it in a write.
func seedSessionOfBob(t *testing.T, database *store.Store) {
	t.Helper()
	ctx := context.Background()
	if _, err := database.Pool.Exec(ctx, `insert into session_owners (session_id, login) values ($1, $2)`, sessionOfBob, seedBob); err != nil {
		t.Fatalf("tie %s to bob: %v", sessionOfBob, err)
	}
	if _, err := database.Pool.Exec(ctx, `insert into session_projects (session_id, project_key) values ($1, $2)`, sessionOfBob, seedPrivate); err != nil {
		t.Fatalf("place %s in PRIV: %v", sessionOfBob, err)
	}
}

// namesTheRemedy reports whether a refusal names bob's session, PUB, and what proves a session's
// access to PUB, so the agent that wrote can put it right (sessionOutsideProjectText).
func namesTheRemedy(body string) bool {
	return strings.Contains(body, "session "+sessionOfBob+" cannot see project "+seedPublic) &&
		strings.Contains(body, "tie the session to a person who sees "+seedPublic)
}

// rowText is the one text a statement answers, failing the test when it answers none.
func rowText(t *testing.T, database *store.Store, sql string, args ...any) string {
	t.Helper()
	var text string
	if err := database.Pool.QueryRow(context.Background(), sql, args...).Scan(&text); err != nil {
		t.Fatalf("read %q: %v", sql, err)
	}
	return text
}

// TestASessionIsGivenNoWorkOfAProjectItDoesNotSee holds each write that gives a session work of an
// issue to the session's sight (refuseSessionOutsideProject): a session route set on the issue, a
// follower added to its ask, a claim made in the session's name, and a comment's mention of the
// session. Each row writes as a caller who sees both projects and the session - carol, or carol's
// token that lists PRIV for the claim a body names the session in - on PUB's issue, which bob's
// session does not see, and is refused 403 PROJECT_SCOPE with nothing written; and then on PRIV's
// issue, which it sees, the control in the same run, and is accepted. A row fails naming its write.
func TestASessionIsGivenNoWorkOfAProjectItDoesNotSee(t *testing.T) {
	t.Parallel()
	seed := seedPrivateProject(t)
	seedSessionOfBob(t, seed.database)
	asks := map[string]string{}
	for _, project := range []string{seedPublic, seedPrivate} {
		asks[project] = createdID(t, seed.request(t, callerCarol, http.MethodPost, "/api/v1/issues/"+seed.issue[project]+"/asks",
			map[string]any{"question": "Who works this?"}))
	}
	for _, row := range []struct {
		write  string
		caller privateCaller
		method string
		target func(project string) string
		body   map[string]any
		ok     int
		// written is what the row's write leaves behind for the project's issue or ask, "" for
		// nothing: a refused write must leave it "".
		written func(project string) string
	}{
		{
			write: "a session route on the issue", caller: callerCarol, method: http.MethodPatch,
			target: func(project string) string { return "/api/v1/issues/" + seed.issue[project] },
			body:   map[string]any{"route": "session:" + sessionOfBob}, ok: http.StatusOK,
			written: func(project string) string {
				return rowText(t, seed.database, `select coalesce(route, '') from issues where key = $1`, seed.issue[project])
			},
		},
		{
			write: "a follower added on the ask", caller: callerCarol, method: http.MethodPut,
			target: func(project string) string { return "/api/v1/asks/" + asks[project] + "/followers/" + sessionOfBob },
			ok:     http.StatusNoContent,
			written: func(project string) string {
				return rowText(t, seed.database, `select coalesce(max(session_id), '') from ask_followers where ask_id = $1 and session_id = $2`,
					asks[project], sessionOfBob)
			},
		},
		{
			write: "a claim made in the session's name", caller: callerCarolPrivToken, method: http.MethodPost,
			target: func(project string) string { return "/api/v1/issues/" + seed.issue[project] + "/claim" },
			body:   map[string]any{"actor": map[string]any{"kind": "session", "id": sessionOfBob}}, ok: http.StatusOK,
			written: func(project string) string {
				return rowText(t, seed.database, `select coalesce(claimed_by ->> 'id', '') from issues where key = $1`, seed.issue[project])
			},
		},
		{
			write: "a comment's mention of the session", caller: callerCarol, method: http.MethodPost,
			target: func(project string) string { return "/api/v1/issues/" + seed.issue[project] + "/comments" },
			body:   map[string]any{"body": "Over to you.", "mentions": []map[string]string{{"target": "session:" + sessionOfBob}}},
			ok:     http.StatusCreated,
			written: func(project string) string {
				return rowText(t, seed.database, `select coalesce(max(body), '') from comments where issue_key = $1 and body = 'Over to you.'`,
					seed.issue[project])
			},
		},
	} {
		refused := seed.request(t, row.caller, row.method, row.target(seedPublic), row.body)
		if refused.Code != http.StatusForbidden || decodeBody[refusalAnswer](t, refused).Code != codeSessionOutsideProject ||
			!namesTheRemedy(refused.Body.String()) {
			t.Errorf("%s on PUB, which %s does not see: %d %s, want 403 %s naming the session, PUB and what proves access",
				row.write, sessionOfBob, refused.Code, strings.TrimSpace(refused.Body.String()), codeSessionOutsideProject)
		}
		if written := row.written(seedPublic); written != "" {
			t.Errorf("%s on PUB was refused and left %q behind, want nothing written", row.write, written)
		}
		accepted := seed.request(t, row.caller, row.method, row.target(seedPrivate), row.body)
		if accepted.Code != row.ok {
			t.Errorf("%s on PRIV, which %s sees: %d %s, want %d", row.write, sessionOfBob, accepted.Code,
				strings.TrimSpace(accepted.Body.String()), row.ok)
		}
		if written := row.written(seedPrivate); written == "" {
			t.Errorf("%s on PRIV was accepted and left nothing behind", row.write)
		}
	}
	awaitBackgroundWrites(t, seed.server)
}

// TestASendToASessionThatDoesNotSeeTheProjectIsRefused holds the two writes that send a session an
// item to the session's sight before anything is written (refuseMentionSessionsOutsideProject,
// refuseMessageTargetOutsideProject, createStoredMessage): a comment's mention of the role
// reviewer, which bob's session holds, so only the mention's resolution names the session, a
// targeted message to that role, and a targeted message to the session itself. On PUB's issue,
// which bob's session does not see, each is refused 403 SESSION_OUTSIDE_PROJECT naming the
// session, no comment, message or attempt is written, and the listener is sent nothing; on PRIV's,
// the control in the same run, each is written and sent to the session.
func TestASendToASessionThatDoesNotSeeTheProjectIsRefused(t *testing.T) {
	t.Parallel()
	listener, sends := newBroadcastListener(t, sightSessions, answerHeldRoles(t, sightSessions))
	seed := seedPrivateProjectWith(t, testServerOptions{envoyURL: listener.URL})
	seedSessionOfBob(t, seed.database)
	writes := []struct {
		name, path string
		body       map[string]any
		table      string
	}{
		{"the mention of role:reviewer", "/comments", map[string]any{"body": "Reviewer, a look?",
			"mentions": []map[string]string{{"target": "role:reviewer"}}, "delivery": "btw"}, "comment_deliveries"},
		{"the message to role:reviewer", "/messages", map[string]any{"body": "A note for the reviewer.",
			"target": "role:reviewer", "delivery": "btw"}, "message_deliveries"},
		{"the message to the session", "/messages", map[string]any{"body": "A note for bob's agent.",
			"target": "session:" + sessionOfBob, "delivery": "btw"}, "message_deliveries"},
	}
	written := func(project string) string {
		return rowText(t, seed.database, `select (select count(*) from comments where issue_key = $1 and body = 'Reviewer, a look?')::text || ' ' ||
			(select count(*) from messages where issue_key = $1)::text || ' ' ||
			(select count(*) from comment_deliveries d join comments c on c.id = d.comment_id where c.issue_key = $1)::text || ' ' ||
			(select count(*) from message_deliveries d join messages m on m.id = d.message_id where m.issue_key = $1)::text`, seed.issue[project])
	}
	for _, write := range writes {
		refused := seed.request(t, callerCarol, http.MethodPost, "/api/v1/issues/"+seed.issue[seedPublic]+write.path, write.body)
		if refused.Code != http.StatusForbidden || decodeBody[refusalAnswer](t, refused).Code != codeSessionOutsideProject ||
			!namesTheRemedy(refused.Body.String()) {
			t.Errorf("%s on PUB, which %s does not see: %d %s, want 403 %s naming the session, PUB and what proves access",
				write.name, sessionOfBob, refused.Code, strings.TrimSpace(refused.Body.String()), codeSessionOutsideProject)
		}
	}
	awaitBackgroundWrites(t, seed.server)
	if got := written(seedPublic); got != "0 0 0 0" {
		t.Errorf("the refused writes on PUB left comments, messages and attempts %s, want none", got)
	}
	if sent := sends.targets(); len(sent) != 0 {
		t.Fatalf("the listener was sent %v for PUB's refused writes, want nothing", sent)
	}
	for _, write := range writes {
		createdID(t, seed.request(t, callerCarol, http.MethodPost, "/api/v1/issues/"+seed.issue[seedPrivate]+write.path, write.body))
	}
	awaitBackgroundWrites(t, seed.server)
	if sent := sends.targets(); len(sent) != len(writes) || slices.ContainsFunc(sent, func(target string) bool { return target != sessionOfBob }) {
		t.Fatalf("the listener was sent %v for PRIV's writes, want %s %d times", sent, sessionOfBob, len(writes))
	}
}

// TestASendWhoseSessionDoesNotSeeTheProjectRecordsWhy holds the send itself to the session's sight
// (sendResolvedDelivery, withheldFromSession), for the sends no write asks for at the moment they
// are made - a sender's lane, a Retry, the delivery sweep: to bob's session, a send of PUB's is
// withheld with the failure that names why, and the listener is sent nothing; the control in the
// same run, a send of PRIV's, reaches the listener.
//
// The send reads a listener list only from a listener record that has not expired, so it refreshes
// an expired one first where the verdict can depend on a list. session-listed is tied to nobody and
// its token lists PRIV. With the record expired, a send of PRIV's to it asks the listener again and
// is sent; with the record expired and the listener not answering, it fails with a failure a later
// try outlasts, withheld nowhere; and once PRIV is public, with the record expired and the
// listener still silent, no verdict depends on a list, so it is sent and the listener is not asked.
func TestASendWhoseSessionDoesNotSeeTheProjectRecordsWhy(t *testing.T) {
	t.Parallel()
	const listed = "session-listed"
	// silent makes the listener's session list answer 503, as a listener that does not answer, and
	// listings counts the times it was asked for that list.
	var silent atomic.Bool
	var listings atomic.Int32
	listener, sends := newBroadcastListener(t,
		strings.TrimSuffix(sightSessions, "]")+`,{"session_id":"`+listed+`","title":"An agent of nobody's","capabilities":["btw","steer"],"projects":["`+seedPrivate+`"]}]`,
		func(w http.ResponseWriter, r *http.Request) bool {
			if r.Method != http.MethodGet || r.URL.Path != "/v1/sessions" {
				return false
			}
			listings.Add(1)
			quiet := silent.Load()
			if quiet {
				w.WriteHeader(http.StatusServiceUnavailable)
			}
			return quiet
		})
	seed := seedPrivateProjectWith(t, testServerOptions{envoyURL: listener.URL})
	seedSessionOfBob(t, seed.database)
	send := func(session, project string) sendOutcome {
		target := ResolvedMention{Target: "session:" + session, Delivery: "btw", SessionID: &session}
		return seed.server.sendResolvedDelivery(t.Context(), target, target.Target, "body", "m1:btw", nil,
			contracts.ExpectsReplyOptional, []byte(`{}`), false, project)
	}
	if withheld := send(sessionOfBob, seedPublic); withheld.envelopeID != nil ||
		withheld.failure != projectWithheldPrefix+"session "+sessionOfBob+" does not see project "+seedPublic+", so nothing was sent" {
		t.Fatalf("a send of PUB's to %s = %+v, want it withheld naming why", sessionOfBob, withheld)
	}
	if sent := sends.targets(); len(sent) != 0 {
		t.Fatalf("the listener was sent %v for PUB's send, want nothing", sent)
	}
	if sent := send(sessionOfBob, seedPrivate); sent.failure != "" || sent.envelopeID == nil {
		t.Fatalf("a send of PRIV's to %s = %+v, want it sent", sessionOfBob, sent)
	}
	if retryableDeliveryFailure(projectWithheldPrefix + "anything") {
		t.Fatalf("a withheld send reads as one a later try outlasts, want it final")
	}

	expire := func() {
		t.Helper()
		if _, err := seed.database.Pool.Exec(t.Context(), `update route_roster set expires_at = now() - interval '1 minute'`); err != nil {
			t.Fatalf("expire the listener record: %v", err)
		}
	}
	expire()
	asked := listings.Load()
	if sent := send(listed, seedPrivate); sent.failure != "" || sent.envelopeID == nil || listings.Load() == asked {
		t.Fatalf("a send of PRIV's to %s with the listener record expired = %+v after %d reads of the listener's sessions, want the record refreshed and the send made",
			listed, sent, listings.Load()-asked)
	}
	expire()
	silent.Store(true)
	if failed := send(listed, seedPrivate); failed.envelopeID != nil || !strings.HasPrefix(failed.failure, listenerUnreachablePrefix) ||
		strings.Contains(failed.failure, projectWithheldPrefix) || !retryableDeliveryFailure(failed.failure) {
		t.Fatalf("a send of PRIV's to %s with the record expired and the listener silent = %+v, want a failure a later try outlasts, withheld nowhere",
			listed, failed)
	}
	expire()
	if _, err := seed.database.Pool.Exec(t.Context(), `update projects set visibility = 'public' where key = $1`, seedPrivate); err != nil {
		t.Fatalf("make PRIV public: %v", err)
	}
	asked = listings.Load()
	if sent := send(listed, seedPrivate); sent.failure != "" || sent.envelopeID == nil || listings.Load() != asked {
		t.Fatalf("with every project public, a send to %s with the record expired and the listener silent = %+v after %d reads of the listener's sessions, want it sent and the listener not asked",
			listed, sent, listings.Load()-asked)
	}
	if sent := sends.targets(); !slices.Equal(sent, []string{sessionOfBob, listed, listed}) {
		t.Fatalf("the listener was sent %v, want %s once and %s twice", sent, sessionOfBob, listed)
	}
}

// TestAQueuedSendWhoseSessionDoesNotSeeTheProjectIsWithheld holds the copy Dispatch queues itself
// for a session no store of the listener's holds (queueForUnknownSession) to the session's sight,
// as every other send is (withheldFromSession): a queued send of PUB's to bob's session fails with
// the failure that names why and publishes nothing on the session's own subject; the control in
// the same run, a queued send of PRIV's, is published there once.
func TestAQueuedSendWhoseSessionDoesNotSeeTheProjectIsWithheld(t *testing.T) {
	t.Parallel()
	seed := seedPrivateProject(t)
	seedSessionOfBob(t, seed.database)
	session := sessionOfBob
	pending := pendingMessageDelivery{frame: []byte(`{}`), resolved: ResolvedMention{
		Target: "session:" + sessionOfBob, Delivery: "btw", SessionID: &session, attemptSessionID: sessionOfBob,
	}}
	message := model.Message{ID: "m1", Body: "A note for bob's agent.", Target: new("session:" + sessionOfBob)}
	queue := func(project string) sendOutcome {
		return seed.server.queueForUnknownSession(t.Context(), message, pending, "m1:btw",
			contracts.ExpectsReplyOptional, noLiveSessionPrefix+" "+sessionOfBob, project)
	}
	if withheld := queue(seedPublic); withheld.queued || withheld.envelopeID != nil ||
		withheld.failure != projectWithheldPrefix+"session "+sessionOfBob+" does not see project "+seedPublic+", so nothing was sent" {
		t.Fatalf("a queued send of PUB's to %s = %+v, want it withheld naming why", sessionOfBob, withheld)
	}
	if published := publishedBy(t, seed.deps); len(published) != 0 {
		t.Fatalf("Dispatch published %d envelopes for PUB's queued send, want none", len(published))
	}
	if queued := queue(seedPrivate); !queued.queued || queued.failure != "" || queued.envelopeID == nil {
		t.Fatalf("a queued send of PRIV's to %s = %+v, want it queued", sessionOfBob, queued)
	}
	if published := publishedBy(t, seed.deps); len(published) != 1 || published[0].Topic != contracts.LegacyAgentSubject(sessionOfBob) {
		t.Fatalf("Dispatch published %+v for PRIV's queued send, want one envelope on %s", published, contracts.LegacyAgentSubject(sessionOfBob))
	}
}
