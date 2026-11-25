package api

import (
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// roleOfPUB is a role of PUB by its name (contracts.RoleProject), which the rows below move from
// formerHolder to newHolder.
const roleOfPUB = "legion-pub-x"

// The two sessions of carol's that hold roleOfPUB in turn: formerHolder acts in PRIV, so a
// credential of carol's that lists PUB alone does not see it (access.SessionClause), and newHolder
// acts in PUB alone.
const (
	formerHolder = "ses-role-former"
	newHolder    = "ses-role-new"
)

// movedRoleSessions is what the stand-in listener lists for the moved-role rows.
const movedRoleSessions = `[
	{"session_id":"` + formerHolder + `","title":"Former holder","capabilities":["btw","steer"],"last_seen":900,"projects":["PRIV"]},
	{"session_id":"` + newHolder + `","title":"New holder","capabilities":["btw","steer"],"last_seen":800,"projects":["PUB"]}
]`

// movedRoleSeed is verification v16's seed with roleOfPUB held by formerHolder, which the test
// hands to newHolder by moveRole, as the listener moves a role when another session claims it.
type movedRoleSeed struct {
	privateSeed
	holder *atomic.Pointer[string]
}

// seedMovedRole writes the seed: formerHolder comments on PRIV's issue and newHolder on PUB's,
// each under a broker token of carol's that names that project, so each has acted in its project
// (session_projects), and roleOfPUB is formerHolder's.
func seedMovedRole(t *testing.T) movedRoleSeed {
	t.Helper()
	holder := &atomic.Pointer[string]{}
	holder.Store(new(formerHolder))
	roles := func(w http.ResponseWriter, r *http.Request) bool {
		role, asked := strings.CutPrefix(r.URL.Path, "/v1/roles/")
		if r.Method != http.MethodGet || !asked {
			return false
		}
		if role != roleOfPUB {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"no holder for role ` + role + `"}`))
			return true
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"role": role, "holder": *holder.Load(), "title": "Holder of " + role, "capabilities": []string{"btw", "steer"},
		})
		return true
	}
	listener, _ := newBroadcastListener(t, movedRoleSessions, roles)
	seed := movedRoleSeed{privateSeed: seedPrivateProjectWith(t, testServerOptions{envoyURL: listener.URL}), holder: holder}
	seed.asSession(t, formerHolder, seedPrivate, http.MethodPost, "/api/v1/issues/"+seed.issue[seedPrivate]+"/comments",
		map[string]any{"body": "Done on PRIV."}, http.StatusCreated)
	seed.asSession(t, newHolder, seedPublic, http.MethodPost, "/api/v1/issues/"+seed.issue[seedPublic]+"/comments",
		map[string]any{"body": "Done on PUB."}, http.StatusCreated)
	return seed
}

// moveRole hands roleOfPUB to newHolder.
func (s movedRoleSeed) moveRole() { s.holder.Store(new(newHolder)) }

// asNewHolder is newHolder's request under a broker token of carol's that lists PUB alone, which
// does not see formerHolder.
func (s movedRoleSeed) asNewHolder(t *testing.T, method, target string, body any) *httptest.ResponseRecorder {
	t.Helper()
	answer := s.sessionRequest(t, seedCarol, newHolder, seedPublic, method, target, body)
	awaitBackgroundWrites(t, s.server)
	return answer
}

// retriedToNewHolder writes, as carol on PUB's issue, a comment that mentions roleOfPUB and a
// message to it, each of whose attempt 1 reaches formerHolder; moves the role to newHolder; and
// Retries each as carol, so attempt 2 reaches newHolder. It answers the comment's id and the
// message's.
func retriedToNewHolder(t *testing.T, seed movedRoleSeed) (comment, message string) {
	t.Helper()
	issue := seed.issue[seedPublic]
	comment = createdID(t, seed.request(t, callerCarol, http.MethodPost, "/api/v1/issues/"+issue+"/comments", map[string]any{
		"body": "Who works PUB's role?", "mentions": []map[string]string{{"target": "role:" + roleOfPUB}},
	}))
	message = createdID(t, seed.request(t, callerCarol, http.MethodPost, "/api/v1/issues/"+issue+"/messages", map[string]any{
		"body": "A note to PUB's role.", "target": "role:" + roleOfPUB, "delivery": "btw",
	}))
	awaitBackgroundWrites(t, seed.server)
	seed.moveRole()
	for kind, id := range map[string]string{"comments": comment, "messages": message} {
		body := map[string]any{"delivery": "btw"}
		if kind == "comments" {
			body["target"] = "role:" + roleOfPUB
		}
		retried := seed.request(t, callerCarol, http.MethodPost, "/api/v1/"+kind+"/"+id+"/deliveries", body)
		awaitBackgroundWrites(t, seed.server)
		attempt := decodeBody[struct {
			Attempt   int     `json:"attempt"`
			SessionID *string `json:"session_id"`
		}](t, retried)
		if retried.Code != http.StatusCreated || attempt.Attempt != 2 || attempt.SessionID == nil || *attempt.SessionID != newHolder {
			t.Fatalf("carol's Retry of her %s to role:%s answers %d %s, want attempt 2 to %s: the row proves nothing",
				strings.TrimSuffix(kind, "s"), roleOfPUB, retried.Code, strings.TrimSpace(retried.Body.String()), newHolder)
		}
	}
	return comment, message
}

// TestAReplyFromAMentionedRolesNewHolderIsTaken: a comment's mention resolves once, when the comment
// is written (comment_mentions.resolved_session_id), and a role passes from session to session.
// carol mentions roleOfPUB, a role of PUB, on PUB's issue while formerHolder, which acted in PRIV,
// holds it; the role passes to newHolder, whose token lists PUB alone and so does not see
// formerHolder; carol's Retry sends attempt 2 to newHolder. newHolder's reply to its own attempt 2
// is posted, 201, and names no "multiple mentions": a reply reads a mention by its target, which
// newHolder sees (mentionsRepliedToBy), and not by the session it resolved to when the comment was
// written. Its reply to attempt 2 of carol's message to the role is posted too, as it was before.
//
// Control: replyComment and replyRoleHolderOf reading the mentions as every read of the comment
// does (mentionsReadBy, the rule before); the comment reply then answers 400 MENTION_INPUT "target
// is required when a comment has multiple mentions", and the row fails.
func TestAReplyFromAMentionedRolesNewHolderIsTaken(t *testing.T) {
	seed := seedMovedRole(t)
	comment, message := retriedToNewHolder(t, seed)
	for name, path := range map[string]string{
		"carol's mention of role:" + roleOfPUB: "/api/v1/comments/" + comment + "/reply",
		"carol's message to role:" + roleOfPUB: "/api/v1/messages/" + message + "/reply",
	} {
		answer := seed.asNewHolder(t, http.MethodPost, path, map[string]any{"attempt": 2, "body": "Done."})
		if answer.Code != http.StatusCreated {
			t.Errorf("%s's reply to attempt 2 of %s answers %d %s, want 201: the role's holder now answers the attempt sent to it",
				newHolder, name, answer.Code, strings.TrimSpace(answer.Body.String()))
		}
		if strings.Contains(answer.Body.String(), "multiple mentions") {
			t.Errorf("%s's reply to attempt 2 of %s answers %s, which names multiple mentions of a comment that has one",
				newHolder, name, strings.TrimSpace(answer.Body.String()))
		}
	}
}

// TestAReplyToAnAttemptPinnedToASessionItsCallerDoesNotSeeAnswersAsNoAttempt: every read of an
// item's deliveries leaves out an attempt pinned to a session its reader does not see
// (leaveOutHiddenCommentSessions, leaveOutHiddenMessageSessions), so a reply to such an attempt
// answers byte for byte as a reply to an attempt no row holds, posts nothing and names nothing of
// the session (replyAttemptHidden). Two callers meet one:
//
//   - newHolder, which holds roleOfPUB now and does not see formerHolder, sends an error report for
//     attempt 1 of carol's mention of the role and of her message to it, which reached formerHolder.
//     The listener moves what a role's holder had not read to the next holder, so newHolder may
//     answer it, but an answer of that attempt is the attempt, which names formerHolder.
//   - the shared token, which sees PUB alone, replies as session-priv, which acted in PRIV, to
//     attempt 1 of carol's mention of role:reviewer and of her message to it, each pinned to
//     session-priv, the role's holder: a credential that does not see PRIV acts for none of PRIV's
//     sessions.
//
// Each answer must equal the same caller's reply to attempt 9, which no row holds. With PRIV public
// (R9) each of newHolder's error reports is taken, as before, naming formerHolder.
//
// Control: replyAttemptHidden answering false; each of the four replies is then taken, newHolder's
// error reports answering 200 with the attempt that names formerHolder and the shared token's
// replies 201, and the row fails naming each.
func TestAReplyToAnAttemptPinnedToASessionItsCallerDoesNotSeeAnswersAsNoAttempt(t *testing.T) {
	seed := seedMovedRole(t)
	comment, message := retriedToNewHolder(t, seed)
	for kind, id := range map[string]string{"comments": comment, "messages": message} {
		path := "/api/v1/" + kind + "/" + id + "/reply"
		got := seed.asNewHolder(t, http.MethodPost, path, map[string]any{"attempt": 1, "error": "Not mine."})
		want := seed.asNewHolder(t, http.MethodPost, path, map[string]any{"attempt": 9, "error": "Not mine."})
		requireAnswersAsNoAttempt(t, newHolder+"'s error report on attempt 1 of "+path, got, want, formerHolder)
	}

	hidden := seedHiddenHolders(t, testServerOptions{})
	issue := hidden.unclaimed
	reviewerComment := createdID(t, hidden.request(t, callerCarol, http.MethodPost, "/api/v1/issues/"+issue+"/comments", map[string]any{
		"body": "Who reviews this?", "mentions": []map[string]string{{"target": "role:" + hiddenHolderRole}},
	}))
	reviewerMessage := createdID(t, hidden.request(t, callerCarol, http.MethodPost, "/api/v1/issues/"+issue+"/messages", map[string]any{
		"body": "A note to the reviewer.", "target": "role:" + hiddenHolderRole, "delivery": "btw",
	}))
	awaitBackgroundWrites(t, hidden.server)
	for kind, id := range map[string]string{"comments": reviewerComment, "messages": reviewerMessage} {
		path := "/api/v1/" + kind + "/" + id + "/reply"
		reply := func(attempt int) *httptest.ResponseRecorder {
			t.Helper()
			answer := hidden.request(t, callerSharedToken, http.MethodPost, path, map[string]any{
				"actor": map[string]string{"kind": "session", "id": sessionOfPrivate}, "attempt": attempt, "body": "Done.",
			})
			awaitBackgroundWrites(t, hidden.server)
			return answer
		}
		requireAnswersAsNoAttempt(t, "the shared token's reply as "+sessionOfPrivate+" to attempt 1 of "+path, reply(1), reply(9), sessionOfPrivate)
	}

	seed.setVisibility(t, seedPrivate, model.ProjectPublic)
	for kind, id := range map[string]string{"comments": comment, "messages": message} {
		answer := seed.asNewHolder(t, http.MethodPost, "/api/v1/"+kind+"/"+id+"/reply", map[string]any{"attempt": 1, "error": "Not mine."})
		blanked := timestampsAndIDs.ReplaceAllString(answer.Body.String(), "<varies>")
		t.Logf("R9 error report on attempt 1 of the %s: %d sha256 %x: %s", kind, answer.Code, sha256.Sum256([]byte(blanked)), strings.TrimSpace(blanked))
		if answer.Code != http.StatusOK || !strings.Contains(answer.Body.String(), formerHolder) {
			t.Errorf("PRIV public: %s's error report on attempt 1 of carol's %s answers %d %s, want 200 naming %s, as before",
				newHolder, kind, answer.Code, strings.TrimSpace(answer.Body.String()), formerHolder)
		}
	}
}

// requireAnswersAsNoAttempt fails, naming what, unless got, a reply to an attempt pinned to a
// session its caller does not see, answers as want, the same reply to an attempt no row holds,
// with a refusal, and names hidden nowhere.
func requireAnswersAsNoAttempt(t *testing.T, what string, got, want *httptest.ResponseRecorder, hidden string) {
	t.Helper()
	if got.Code != want.Code || got.Body.String() != want.Body.String() {
		t.Errorf("%s answers %d %s; to an attempt no row holds, %d %s: want one answer",
			what, got.Code, strings.TrimSpace(got.Body.String()), want.Code, strings.TrimSpace(want.Body.String()))
	}
	if got.Code < http.StatusBadRequest {
		t.Errorf("%s was taken: %d %s", what, got.Code, strings.TrimSpace(got.Body.String()))
	}
	if strings.Contains(got.Body.String(), hidden) {
		t.Errorf("%s names %s, a session its caller does not see: %s", what, hidden, strings.TrimSpace(got.Body.String()))
	}
}
