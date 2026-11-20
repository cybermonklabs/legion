package oauth_test

// The roles of one issue pod read one exchange token and are registered for one pod uid, so a
// registration names the key its own role proves: the exchange refuses a trade for a session
// proved with a sibling's key, and a registration that names none stays the pod's.

import (
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/exchange"
	"github.com/sjawhar/envoy/internal/cipher/oauth"
	"github.com/sjawhar/envoy/internal/cipher/oauthwire"
)

// The pod both roles run in, and the two roles' sessions.
const (
	issuePod = "pod-uid-issue"
	roleA    = "implementer-g1"
	roleB    = "reviewer-g1"
)

// registersBound records that the pod uid of the account of may exchange under session with a
// DPoP proof of the key whose thumbprint is jkt, as the Legion daemon (registrar) registers each
// role.
func (f *exchangeFixture) registersBound(t *testing.T, of, uid, session, jkt string) {
	t.Helper()
	if _, err := f.provider.RegisterPodSession(context.Background(), registrar, of, uid, session, jkt, 0); err != nil {
		t.Fatalf("register %s's pod %s for %s bound to %s: %v", of, uid, session, jkt, err)
	}
}

// podTrade is the issue pod's trade for session, presenting a fresh projected token.
func (f *exchangeFixture) podTrade(t *testing.T, session string) url.Values {
	t.Helper()
	return exchangeForm(f.podToken(t, issuePod, nil), oauthwire.SubjectTokenTypeKubernetes, session)
}

// TestASiblingRoleCannotTradeForAnotherRolesSession: pod P is registered for A with A's key and
// for B with B's. A trade presenting P's token for B, proved with A's key, is refused
// invalid_grant naming B; the same trade proved with B's key is admitted, as is A's own with A's.
func TestASiblingRoleCannotTradeForAnotherRolesSession(t *testing.T) {
	f := newExchangeFixture(t, func(*oauth.Config) {})
	kA, kB := newKey(t), newKey(t)
	f.registersBound(t, account, issuePod, roleA, thumbprint(t, kA))
	f.registersBound(t, account, issuePod, roleB, thumbprint(t, kB))

	status, body := f.exchange(t, f.podTrade(t, roleB), kA)
	if status != http.StatusBadRequest || body["error"] != "invalid_grant" || body["access_token"] != nil {
		t.Fatalf("role A's trade for %s's session = %d %v, want 400 invalid_grant and no token: a sibling role trades only for its own session", roleB, status, body)
	}
	want := "this pod's registration for session '" + roleB + "' is bound to another key; a role trades only for its own session"
	if description, _ := body["error_description"].(string); !strings.Contains(description, want) {
		t.Errorf("the refusal reads %q, want it to say %q", description, want)
	}

	f.introspectsAs(t, f.exchanged(t, f.podTrade(t, roleB), kB), roleB, person, project)
	f.introspectsAs(t, f.exchanged(t, f.podTrade(t, roleA), kA), roleA, person, project)
}

// TestASiblingRolesRefusedTradeLogsALineOfItsOwn: the trade a sibling role makes for another
// role's session, refused because the registration is bound to another key, logs one WARN line of
// its own naming the principal, the pod, the session, the key the registration is bound to and
// the key the trade proved. writeError logs only a server error, so without it the broker's log
// shows this refusal as it shows an expired grant or a registration not yet written. Neither
// admitted trade logs it, and no line repeats the subject token.
func TestASiblingRolesRefusedTradeLogsALineOfItsOwn(t *testing.T) {
	logs := capturedLog(t)
	f := newExchangeFixture(t, func(*oauth.Config) {})
	kA, kB := newKey(t), newKey(t)
	f.registersBound(t, account, issuePod, roleA, thumbprint(t, kA))
	f.registersBound(t, account, issuePod, roleB, thumbprint(t, kB))

	sibling := f.podTrade(t, roleB)
	if status, body := f.exchange(t, sibling, kA); status != http.StatusBadRequest || body["error"] != "invalid_grant" {
		t.Fatalf("role A's trade for %s's session = %d %v, want 400 invalid_grant", roleB, status, body)
	}
	f.exchanged(t, f.podTrade(t, roleB), kB)
	f.exchanged(t, f.podTrade(t, roleA), kA)

	const message = "broker: the token exchange refused a pod's trade proved with a key other than the one its registration for the session is bound to"
	lines := logs.lines(`"msg":"` + message + `"`)
	if len(lines) != 1 {
		t.Fatalf("the refused sibling trade and the two admitted trades logged %d lines %q, want exactly 1, the refusal's own:\n%s",
			len(lines), message, strings.Join(logs.lines(), "\n"))
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &got); err != nil {
		t.Fatalf("the logged line %s: %v", lines[0], err)
	}
	delete(got, "time")
	want := map[string]any{
		"level": "WARN", "msg": message, "principal": exchange.AccountPrincipal(account), "pod": issuePod, "session": roleB,
		"bound_thumbprint": thumbprint(t, kB), "presented_thumbprint": thumbprint(t, kA),
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("the refusal's line = %v, want %v", got, want)
	}
	if subject := sibling.Get("subject_token"); len(logs.lines(subject)) != 0 {
		t.Errorf("a logged line repeats the refused trade's subject token:\n%s", strings.Join(logs.lines(subject), "\n"))
	}
}

// TestARegistrationWithNoThumbprintIsPodBound: a registration that names no key, as a session
// watch registers a session pod's one session, admits that pod's trade for that session proved
// with any key, as every registration did before keys were named.
func TestARegistrationWithNoThumbprintIsPodBound(t *testing.T) {
	f := newExchangeFixture(t, func(*oauth.Config) {})
	f.registers(t, account, issuePod, roleA)
	for _, key := range []*ecdsa.PrivateKey{newKey(t), newKey(t)} {
		f.introspectsAs(t, f.exchanged(t, f.podTrade(t, roleA), key), roleA, person, project)
	}
}
