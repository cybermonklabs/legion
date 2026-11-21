package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
)

// allPublicAnswers is what a listener answers a session listing PRIV, under a member token, over
// the routes whose answers this change could alter, and the subject its message is stored on.
type allPublicAnswers struct {
	registration, list, entry string
	stored                    string
}

func readAllPublicAnswers(t *testing.T, rig *privacyRig, sessionID string) allPublicAnswers {
	t.Helper()
	standIn := startSessionStandIn(t, rig.url, sessionID, answerDone)
	token := rig.memberToken(sessionID, false, "PRIV")
	registration := rig.subscribe(t, token, sessionID)
	requireStatus(t, registration, http.StatusOK, "the registration")
	list := rig.call(t, http.MethodGet, token, "/v1/sessions", "")
	requireStatus(t, list, http.StatusOK, "the session list")
	entry := rig.call(t, http.MethodGet, token, "/v1/registry/"+sessionID, "")
	requireStatus(t, entry, http.StatusOK, "the registry entry")
	send := rig.call(t, http.MethodPost, token, "/v1/messages/send", `{"target_session":"`+sessionID+`","message":"hello"}`)
	if send.Code >= 300 {
		t.Fatalf("the send answered %d: %s", send.Code, send.Body.String())
	}
	frame := standIn.collect(t, 1, 30*time.Second)[0]
	return allPublicAnswers{registration: registration.Body.String(), list: list.Body.String(), entry: entry.Body.String(), stored: frame.item.Topic}
}

// TestWithNoPrivateProjectEveryListenerAnswerIsAsBefore: a listener in the all-public mode (neither
// privacy variable set) answers a session's registration, the session list and its registry entry
// with no projects field - the keys the merge base answers, which the no-interest fixture rows pin
// - stores a send on the session's agent subject in the form the merge base writes, and reads the
// privacy source not once. Control: the same inputs to a required-mode listener answer projects
// and store the send under its project, so the row fails for the mode it names.
func TestWithNoPrivateProjectEveryListenerAnswerIsAsBefore(t *testing.T) {
	public := newPrivacyRig(t, false)
	answers := readAllPublicAnswers(t, public, "ses_all_public")
	for name, body := range map[string]string{"registration": answers.registration, "list": answers.list, "entry": answers.entry} {
		if strings.Contains(body, `"projects"`) || strings.Contains(body, `"restricted"`) {
			t.Errorf("all-public: the %s answer carries a field the merge base does not: %s", name, body)
		}
		if !json.Valid([]byte(body)) {
			t.Errorf("all-public: the %s answer is not JSON: %s", name, body)
		}
	}
	if answers.stored != contracts.LegacyAgentSubject("ses_all_public") {
		t.Errorf("all-public: the send was stored on %q, want %q, the merge base's subject", answers.stored, contracts.LegacyAgentSubject("ses_all_public"))
	}
	if reads := public.source.reads(); reads != 0 {
		t.Errorf("all-public: the listener read the privacy source %d times, want none", reads)
	}

	required := newPrivacyRig(t, true)
	control := readAllPublicAnswers(t, required, "ses_required")
	if !strings.Contains(control.registration, `"projects":["PRIV"]`) || !strings.Contains(control.entry, `"projects":["PRIV"]`) {
		t.Errorf("control, required mode: the answers %s and %s carry no projects", control.registration, control.entry)
	}
	if control.stored != contracts.AgentSubject("PRIV", "ses_required") {
		t.Errorf("control, required mode: the send was stored on %q, want %q", control.stored, contracts.AgentSubject("PRIV", "ses_required"))
	}
}
