package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// replySession is the session the direct conversations of these rows are with. It acts in PUB under
// alice's broker-issued token, which proves it (asSessionOf) and ties it to alice, who sees no
// private project: so a caller that sees PUB may read it, its conversations included
// (access.SessionContentClause), and send to it.
const replySession = "s-reply"

// carolsWords is the body of carol's own direct message to replySession, which no reply of
// another person's may preview.
const carolsWords = "Carol's own word to s-reply, for her conversation alone."

// replySeed is verification v16's seed with replySession acting in PUB and carol's direct message
// to it, whose id it answers.
func replySeed(t *testing.T) (privateSeed, string) {
	t.Helper()
	seed := seedPrivateProject(t)
	mintListingToken(t, seed, callerAlicePubToken, seedAlice, []string{seedPublic})
	seed.asSessionOf(t, seedAlice, replySession, seedPublic, http.MethodPost, "/api/v1/issues/"+seed.issue[seedPublic]+"/comments",
		map[string]any{"body": "s-reply works on PUB."}, http.StatusCreated)
	carols := createdID(t, seed.request(t, callerCarol, http.MethodPost, "/api/v1/agents/"+replySession+"/messages",
		map[string]any{"body": carolsWords, "delivery": "btw"}))
	return seed, carols
}

// directReply is caller's reply to the direct message parent, sent to replySession.
func directReply(t *testing.T, seed privateSeed, caller privateCaller, parent string) *httptest.ResponseRecorder {
	t.Helper()
	return seed.request(t, caller, http.MethodPost, "/api/v1/agents/"+replySession+"/messages", map[string]any{
		"body": "A reply to " + parent + ".", "delivery": "btw", "in_reply_to": parent,
	})
}

// carolsWordsCarried counts what holds carol's words beyond her own message: a message replying to
// it, and an event whose payload carries her words, which her own message's events do.
func carolsWordsCarried(t *testing.T, seed privateSeed, carols string) (replies, events int) {
	t.Helper()
	ctx := context.Background()
	if err := seed.database.Pool.QueryRow(ctx, `select count(*) from messages where in_reply_to = $1::uuid`, carols).Scan(&replies); err != nil {
		t.Fatalf("count the replies to carol's message: %v", err)
	}
	if err := seed.database.Pool.QueryRow(ctx, `select count(*) from events where strpos(payload::text, $1) > 0`, carolsWords).Scan(&events); err != nil {
		t.Fatalf("count the events carrying carol's words: %v", err)
	}
	return replies, events
}

// TestAReplyToADirectMessageItsWriterMayNotReadAnswersAsAMissingParent: a reply whose in_reply_to
// names a direct message its writer may not read (access.MessageClause) - carol's message to a
// session, named by bob at his browser, who is no party of that conversation and is not open, a
// restricted member - answers exactly as a reply naming an id no message holds, the id written as
// carol's, and writes nothing: no reply joins carol's conversation and no event previews her
// words. The session acts in PRIV too, so bob may see it and send to it, and only the
// conversation's rule keeps carol's message from him. His token is no writer here: a restricted
// member's token holds no scope (R16), and the route needs the person scope.
func TestAReplyToADirectMessageItsWriterMayNotReadAnswersAsAMissingParent(t *testing.T) {
	t.Parallel()
	seed, carols := replySeed(t)
	seed.asSession(t, replySession, seedPrivate, http.MethodPost, "/api/v1/issues/"+seed.issue[seedPrivate]+"/comments",
		map[string]any{"body": "s-reply works on PRIV too."}, http.StatusCreated)
	replies, events := carolsWordsCarried(t, seed, carols)

	hidden := directReply(t, seed, callerBob, carols)
	nobodys := uuid.NewString()
	missing := directReply(t, seed, callerBob, nobodys)
	if missing.Code != http.StatusBadRequest {
		t.Fatalf("a reply naming an id no message holds answered %d %s, want the 400 of a missing parent", missing.Code, missing.Body.String())
	}
	want := strings.ReplaceAll(strings.TrimSpace(missing.Body.String()), nobodys, carols)
	if got := strings.TrimSpace(hidden.Body.String()); hidden.Code != missing.Code || got != want {
		t.Errorf("a reply naming carol's direct message answered %d %s, and one naming an id no message holds %d %s",
			hidden.Code, got, missing.Code, strings.TrimSpace(missing.Body.String()))
	}
	if afterReplies, afterEvents := carolsWordsCarried(t, seed, carols); afterReplies != replies || afterEvents != events {
		t.Errorf("after the reply, %d messages reply to carol's and %d events carry her words, where %d and %d did before",
			afterReplies, afterEvents, replies, events)
	}
}

// TestAReplyToADirectMessageItsWriterMayReadIsTaken: the same caller replying to its own direct
// message to the session, and carol replying to hers, are taken: the read binds who may read the
// parent, a party of its conversation among them, and refuses no reply for being one.
func TestAReplyToADirectMessageItsWriterMayReadIsTaken(t *testing.T) {
	t.Parallel()
	seed, carols := replySeed(t)
	own := createdID(t, seed.request(t, callerAlicePubToken, http.MethodPost, "/api/v1/agents/"+replySession+"/messages",
		map[string]any{"body": "Alice's token's own word to s-reply.", "delivery": "btw"}))
	for _, reply := range []struct {
		name   string
		caller privateCaller
		parent string
	}{
		{"alice's token that lists PUB, to its own message", callerAlicePubToken, own},
		{"carol, to her own message", callerCarol, carols},
	} {
		if response := directReply(t, seed, reply.caller, reply.parent); response.Code != http.StatusCreated {
			t.Errorf("%s: the reply answered %d %s, want 201", reply.name, response.Code, strings.TrimSpace(response.Body.String()))
		}
	}
}

// TestAnOpenViewerRepliesToADirectMessageAsBeforeWithNoPrivateProject: in a deployment that holds
// no private project (PRIV made public again), alice at a browser, an open viewer, replies to
// carol's direct message to the session and is taken, as at 99e11cdd50: an open viewer reads every
// conversation whose sessions it may read (access.PartyClause).
func TestAnOpenViewerRepliesToADirectMessageAsBeforeWithNoPrivateProject(t *testing.T) {
	t.Parallel()
	seed, carols := replySeed(t)
	seed.setVisibility(t, seedPrivate, "public")
	if response := directReply(t, seed, callerAlice, carols); response.Code != http.StatusCreated {
		t.Errorf("alice's reply to carol's direct message answered %d %s, want 201", response.Code, strings.TrimSpace(response.Body.String()))
	}
}
