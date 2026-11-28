package api

import (
	"net/http"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
)

// GET /api/v2/me answers sign_in_required as GET /api/v1/whoami does (#4873), from the one
// decision writeNeedsASignedInPerson refuses a write on (signInRequired): true for a machine's
// broker token that names a service and no person, whose /api/v2 write is refused 403
// SIGN_IN_REQUIRED, and false for an account entry's token naming no person, whose /api/v2 write
// is recorded under its account, and for a person at a browser.
func TestTheV2MeSaysWhetherTheCredentialMustSignInToWrite(t *testing.T) {
	rig := newBrokerTokenRig(t)
	caller := newBrokerCaller(t)
	const machine, account = "a-machine-token-naming-no-person", "an-account-token-naming-no-person"
	rig.broker.Says(machine, serviceOnlyToken(caller.jkt))
	rig.broker.Says(account, personlessAccountToken(caller.jkt))
	own := seedProject(t, rig, brokerTestProject, "Broker")

	me := func(token string) meV2 {
		t.Helper()
		response := rig.serve(caller.presenting(t, http.MethodGet, "/api/v2/me", token, nil))
		if response.Code != http.StatusOK {
			t.Fatalf("GET /api/v2/me under %s: status=%d body=%s", token, response.Code, response.Body.String())
		}
		return decodeBody[meV2](t, response)
	}
	comment := func(token string) (int, string) {
		t.Helper()
		response := rig.serve(caller.presenting(t, http.MethodPost, "/api/v2/issues/"+own.issue+"/comments", token,
			map[string]any{"body": "Written under " + token + "."}))
		if response.Code == http.StatusCreated {
			return response.Code, ""
		}
		code, _ := refusalOf(t, response)
		return response.Code, code
	}

	if answer := me(machine); !answer.SignInRequired || answer.Person != nil || answer.Service == nil || *answer.Service != brokerTestService {
		t.Errorf("GET /api/v2/me under a machine's token naming no person = %+v, want sign_in_required true for the service %s", answer, brokerTestService)
	}
	if status, code := comment(machine); status != http.StatusForbidden || code != apicode.SignInRequired.Name() {
		t.Errorf("a /api/v2 comment under a machine's token naming no person answered %d %s, want 403 %s", status, code, apicode.SignInRequired.Name())
	}
	if answer := me(account); answer.SignInRequired || answer.Person != nil || answer.Service == nil || *answer.Service != brokerTestAccount {
		t.Errorf("GET /api/v2/me under an account entry's token naming no person = %+v, want sign_in_required false for the account %s", answer, brokerTestAccount)
	}
	if status, code := comment(account); status != http.StatusCreated {
		t.Errorf("a /api/v2 comment under an account entry's token naming no person answered %d %s, want 201, recorded under the account", status, code)
	}
	browser := dispatchRequest(t, rig.handler, http.MethodGet, "/api/v2/me", nil, "alice")
	if browser.Code != http.StatusOK || decodeBody[meV2](t, browser).SignInRequired {
		t.Errorf("GET /api/v2/me for a person at a browser answered %d %s, want sign_in_required false", browser.Code, browser.Body.String())
	}
}
