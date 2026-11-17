package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// TestAnAbsentAskOrProjectIsNoServerError: a token that lists PUB, of carol's who owns PRIV,
// handing an ask nobody holds to a person, or setting the default assignee of a key no project
// holds, is answered as a missing object and never as the server's error.
func TestAnAbsentAskOrProjectIsNoServerError(t *testing.T) {
	seed := seedPrivateProject(t)
	const caller privateCaller = "carol's token that lists PUB"
	mintListingToken(t, seed, caller, seedCarol, []string{seedPublic})
	for _, call := range []struct{ path, body string }{
		{"/api/v1/asks/" + uuid.NewString() + "/asked-of", `{"actor":{"kind":"session","id":"s-probe"},"asked_of":"alice"}`},
		{"/api/v1/projects/NOPE/default-assignee", `{"actor":{"kind":"session","id":"s-probe"},"assignee":"alice"}`},
	} {
		answer := seed.request(t, caller, http.MethodPut, call.path, json.RawMessage(call.body))
		t.Logf("PUT %s as %s: %d %s", call.path, caller, answer.Code, strings.TrimSpace(answer.Body.String()))
		if answer.Code >= http.StatusInternalServerError {
			t.Errorf("PUT %s as %s: %d %s, want the missing object's answer", call.path, caller, answer.Code, strings.TrimSpace(answer.Body.String()))
		}
	}
}
