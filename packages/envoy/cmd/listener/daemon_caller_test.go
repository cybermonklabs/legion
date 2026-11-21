package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/ciphertoken"
	"github.com/sjawhar/envoy/internal/id"
)

// daemonAnswer is the broker's answer about a Legion daemon's own token: the session the daemon's
// exchange row fixes, legion-daemon:<project>, that one project under the named scope an account
// entry mints, and the person the row names, as the real broker answers it for the daemon's pod
// (ciphertest.DaemonExchangeAccount; packages/daemon's TestTheDaemonsListenerTokenIsItsOwnDaemonToken
// reads that answer back from the real broker).
func daemonAnswer(project, person, jkt string) ciphertoken.Introspection {
	return ciphertoken.Introspection{
		Active:       true,
		Session:      "legion-daemon:" + project,
		Person:       person,
		ProjectScope: ciphertoken.ProjectScopeNamed,
		Projects:     []string{project},
		ClientID:     ciphertoken.ExchangeClientID,
		Thumbprint:   jkt,
	}
}

// notDaemonAnswers are tokens that look like a Legion daemon's in all but one way each, and so
// are no daemon's: daemonCaller must refuse every one, or a session could take a daemon's powers
// by the name it chose or the projects its person could see. A named-scope answer listing two
// projects is not among them: the verifier refuses that shape before any route (not_a_session).
func notDaemonAnswers(jkt string) map[string]ciphertoken.Introspection {
	personScoped := daemonAnswer("EXAMPLE", "ada@example.com", jkt)
	personScoped.ProjectScope = ciphertoken.ProjectScopePerson
	otherProject := daemonAnswer("EXAMPLE", "ada@example.com", jkt)
	otherProject.Projects = []string{"OTHER"}
	noProject := daemonAnswer("", "ada@example.com", jkt)
	noProject.Projects = nil
	return map[string]ciphertoken.Introspection{
		"access-token-person-scoped-daemon-name": personScoped,
		"access-token-daemon-name-other-project": otherProject,
		"access-token-daemon-name-no-project":    noProject,
		"access-token-of-an-agent-session":       sessionAnswer("ses_daemon_bystander", "ada@example.com", jkt),
	}
}

// cleanStopPath is the Legion daemon's clean stop of session (packages/daemon's
// notify.HTTPPublisher.DeleteSession): the moment the stop belongs to, a minute ahead of a
// registration written now, and no client instance.
func cleanStopPath(session string) string {
	return "/v1/sessions/" + session + "?if_not_registered_after=" + strconv.FormatInt(time.Now().Add(time.Minute).UnixMilli(), 10)
}

// TestADaemonsTokenMakesTheCleanStopOfAnotherSession: the Legion daemon stops the registration of a
// role whose process ended (in the daemon's words, the clean stop), and that
// role's session is never the daemon's own, so a listener that holds every DELETE to the token's
// session would refuse every clean stop a daemon makes with its broker token. A daemon's own token
// (daemonCaller) may therefore make the conditional clean stop, if_not_registered_after present,
// for another session, as the shared bearer may today, and nothing more: the same token's
// unconditional DELETE of another session, a stop naming a client instance alone, and its DELETE
// of another session's interests are each refused 403, and so is the clean stop under every token
// that is no daemon's (notDaemonAnswers). The control is the rule as it stood: sessionNotBound
// alone refuses the daemon's clean stop too.
func TestADaemonsTokenMakesTheCleanStopOfAnotherSession(t *testing.T) {
	rig := newBrokerRig(t)
	const daemonToken = "access-token-of-the-daemon"
	rig.broker.Says(daemonToken, daemonAnswer("EXAMPLE", "ada@example.com", rig.caller.JKT()))
	controls := notDaemonAnswers(rig.caller.JKT())
	for token, answer := range controls {
		rig.broker.Says(token, answer)
	}
	registered := func(session string) bool {
		t.Helper()
		_, err := rig.deps.sessions.RefreshFromLeader(context.Background(), session)
		return err == nil
	}
	refused := func(t *testing.T, answer *httptest.ResponseRecorder, what, session string) {
		t.Helper()
		if answer.Code != http.StatusForbidden {
			t.Fatalf("%s: status = %d, want 403; body = %s", what, answer.Code, answer.Body.String())
		}
		if !registered(session) {
			t.Fatalf("%s removed %s's registration, want it kept", what, session)
		}
	}

	t.Run("the daemon's clean stop removes another session's registration", func(t *testing.T) {
		const role = "ses_daemon_clean_stop"
		rig.register(t, role)
		answer := rig.call(t, rig.handler, http.MethodDelete, daemonToken, cleanStopPath(role), "")
		if answer.Code != http.StatusOK || !strings.Contains(answer.Body.String(), `"deleted":true`) {
			t.Fatalf("the daemon's clean stop of %s: status = %d, body = %s; want 200 and the registration gone", role, answer.Code, answer.Body.String())
		}
		if registered(role) {
			t.Fatalf("the daemon's clean stop of %s answered deleted, and the registration is still there", role)
		}
	})

	t.Run("the daemon's unconditional delete of another session is refused", func(t *testing.T) {
		const role = "ses_daemon_unconditional"
		rig.register(t, role)
		refused(t, rig.call(t, rig.handler, http.MethodDelete, daemonToken, "/v1/sessions/"+role, ""), "an unconditional DELETE under the daemon's token", role)
		refused(t, rig.call(t, rig.handler, http.MethodDelete, daemonToken, "/v1/sessions/"+role+"?client_instance_id=4f1c2a9e8b7d6c5a", ""),
			"a DELETE naming a client instance alone under the daemon's token", role)
	})

	// A clean stop's moment is the daemon's own clock when the process ended, so it is never far
	// ahead of this listener's: a moment more than five minutes past the listener's clock would
	// remove whatever registration stands, which is the unconditional delete under another name,
	// and is refused as that delete is. The bound is written here as five minutes, not read from the
	// constant, so widening the constant fails this row; the five seconds on each side leave room for
	// the time between the test's clock read and the handler's.
	moment := func(ahead time.Duration) string {
		return strconv.FormatInt(time.Now().Add(ahead).UnixMilli(), 10)
	}
	t.Run("the daemon's stop dated just under five minutes ahead removes the registration", func(t *testing.T) {
		const role = "ses_daemon_near_moment"
		rig.register(t, role)
		answer := rig.call(t, rig.handler, http.MethodDelete, daemonToken, "/v1/sessions/"+role+"?if_not_registered_after="+moment(5*time.Minute-5*time.Second), "")
		if answer.Code != http.StatusOK || !strings.Contains(answer.Body.String(), `"deleted":true`) || registered(role) {
			t.Fatalf("a clean stop dated 4m55s ahead under the daemon's token: status = %d, body = %s, still registered %v; want 200 and the registration gone",
				answer.Code, answer.Body.String(), registered(role))
		}
	})
	t.Run("the daemon's stop dated just over five minutes ahead is refused", func(t *testing.T) {
		const role = "ses_daemon_far_moment"
		rig.register(t, role)
		refused(t, rig.call(t, rig.handler, http.MethodDelete, daemonToken, "/v1/sessions/"+role+"?if_not_registered_after="+moment(5*time.Minute+5*time.Second), ""),
			"a clean stop dated 5m05s ahead under the daemon's token", role)
	})

	t.Run("the daemon's token removes no other session's interests", func(t *testing.T) {
		const role = "ses_daemon_interests"
		rig.register(t, role)
		answer := rig.call(t, rig.handler, http.MethodDelete, daemonToken, "/v1/interests/"+role, "")
		if answer.Code != http.StatusForbidden {
			t.Fatalf("DELETE /v1/interests/%s under the daemon's token: status = %d, want 403; body = %s", role, answer.Code, answer.Body.String())
		}
	})

	for token := range controls {
		t.Run("the clean stop under "+token+" is refused", func(t *testing.T) {
			role := "ses_daemon_control_" + id.New()
			rig.register(t, role)
			refused(t, rig.call(t, rig.handler, http.MethodDelete, token, cleanStopPath(role), ""), "the clean stop under "+token, role)
		})
	}
}

// TestTheSenderLimitDoesNotCountTheDaemonsNotices: the Legion daemon publishes its workflow notices
// with no source and no source_session (packages/daemon's notify.HTTPPublisher.Publish), which the
// sender limit has never counted, since such a message has no sender. Under the daemon's own broker
// token the token's session would be the sender, and the operator's limit (4 a minute in
// production) would hold back the daemon's whole outbox; so a daemon's token (daemonCaller) is not
// counted, whatever the burst. Every token that is no daemon's (notDaemonAnswers) is counted as
// before: its message past the burst is refused 429 naming its session.
func TestTheSenderLimitDoesNotCountTheDaemonsNotices(t *testing.T) {
	rig := newBrokerRig(t)
	const daemonToken = "access-token-of-the-publishing-daemon"
	rig.broker.Says(daemonToken, daemonAnswer("EXAMPLE", "ada@example.com", rig.caller.JKT()))
	controls := notDaemonAnswers(rig.caller.JKT())
	for token, answer := range controls {
		rig.broker.Says(token, answer)
	}
	publish := func(t *testing.T, token, topic string) limitedAnswer {
		t.Helper()
		body := messageJSON(t, map[string]any{"topic": topic, "message": countedMessage}, nil)
		return serveLimited(t, rig.handler, rig.logs.String, brokerTokenRequest(t, rig.caller, http.MethodPost, "/v1/messages/publish", token, body))
	}

	t.Run("the daemon's notices past the burst are each published and counted for nobody", func(t *testing.T) {
		rig.deps.senderLimit = newSenderLimit(4, 1, func() time.Time { return senderLimitStart })
		topic := "notifications.team.daemon-notices-" + id.New()
		for i := range 3 {
			notice := publish(t, daemonToken, topic)
			notice.requireStatus(t, "the daemon's notice "+strconv.Itoa(i+1), http.StatusOK)
			notice.requireUnnamed(t, "the daemon's notice "+strconv.Itoa(i+1))
		}
	})

	for token, answer := range controls {
		t.Run("a message under "+token+" is counted", func(t *testing.T) {
			rig.deps.senderLimit = newSenderLimit(4, 1, func() time.Time { return senderLimitStart })
			topic := "notifications.team.daemon-notices-control-" + id.New()
			first := publish(t, token, topic)
			first.requireStatus(t, "the first message under "+token, http.StatusOK)
			first.requireNamed(t, "the first message under "+token, answer.Session)
			publish(t, token, topic).requireRefused(t, "the next message under "+token, answer.Session, 4, 1, 15)
		})
	}
}
