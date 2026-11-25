package daemon

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/oauth2call/oauth2calltest"
)

// TestTheBootsWhoamiRefusalPrintsDispatchsWordsOnlyAsTheRuleReadsThem: whatever answers
// GET /api/v1/whoami in Dispatch's place answers a session, or a project, that echoes the broker
// token the read presented, so the boot refuses the entry, quoting the field. The refusal holds no
// run of the token (captures daemon.whoami-words/<field>): it prints Dispatch's words as the rule
// reads them, the read's credentials redacted. The twin shows the stand-in's answer holds the
// token.
func TestTheBootsWhoamiRefusalPrintsDispatchsWordsOnlyAsTheRuleReadsThem(t *testing.T) {
	const token = "dpop-" + oauth2calltest.Token + "-7f3a"
	broker := newCredentialBroker(t, func(int64) (int, map[string]any) {
		return http.StatusOK, map[string]any{
			"access_token": token, "token_type": "DPoP", "expires_in": 3600,
			"issued_token_type": "urn:ietf:params:oauth:token-type:access_token",
		}
	})
	for _, field := range []string{"session", "project"} {
		t.Run(field, func(t *testing.T) {
			var answered strings.Builder
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/v1/whoami" {
					_, _ = io.WriteString(w, "{}")
					return
				}
				whoami := daemonWhoami("EXAMPLE")
				whoami[field] = "echo " + strings.TrimPrefix(r.Header.Get("Authorization"), "DPoP ")
				body, _ := json.Marshal(whoami)
				answered.Write(body)
				_, _ = w.Write(body)
			}))
			defer server.Close()

			_, err := dispatchCredential(context.Background(), brokerConfig(server.URL), "", getenvOf(podIdentity(t, broker.url)), quietLogger())
			if err == nil || !strings.Contains(err.Error(), "answers the "+field) {
				t.Fatalf("dispatchCredential = %v; want the boot refused naming the %s", err, field)
			}
			if !strings.Contains(answered.String(), token) {
				t.Fatalf("twin: the stand-in's answer %s holds no echo of the token, so this row proves nothing", answered.String())
			}
			oauth2calltest.RequireNoPiece(t, "daemon.whoami-words/"+field, err.Error(), token)
		})
	}
}
