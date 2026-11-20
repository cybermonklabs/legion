package oauth_test

import (
	"context"
	"crypto/ecdsa"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/broker/exchange"
	"github.com/sjawhar/envoy/internal/broker/oauth"
	"github.com/sjawhar/envoy/internal/broker/oauthwire"
	"github.com/sjawhar/envoy/internal/broker/projectaccess"
)

// restrictedLifetime is the longest a restricted member's machine-login token lives, whatever
// BROKER_ACCESS_TOKEN_SECONDS says: the bound on how long a person taken off a project's access
// list keeps a token for it.
const restrictedLifetime = exchange.RestrictedLifetime

// edgeLiveCheck is how long after a token ends the edge listener closes a socket it opened: its
// 20 s check of the token plus the 5 s its verifier keeps an answer (the relay socket's contract).
const edgeLiveCheck = 25 * time.Second

// TestARestrictedTokenEndsWithinTheBound holds the arithmetic of the bound on a removal from an
// access list: the broker's cache of Dispatch's answer that a person sees the project, then the
// restricted token's lifetime, then the edge listener's live check, end at most 325 seconds after
// the removal. It holds with no revoke at all, since every trade is checked against Dispatch; the
// revocation Dispatch pays after the removal commits only ends the person's tokens sooner.
//
// Control: RestrictedLifetime at five minutes, and the sum is 5m55s and the row fails naming it.
func TestARestrictedTokenEndsWithinTheBound(t *testing.T) {
	if sum := projectaccess.CacheTTL + exchange.RestrictedLifetime + edgeLiveCheck; sum > 325*time.Second {
		t.Fatalf("the cache (%s), the restricted lifetime (%s) and the edge's check (%s) sum to %s, want at most 5m25s",
			projectaccess.CacheTTL, exchange.RestrictedLifetime, edgeLiveCheck, sum)
	}
}

// oneProjectRule is what the refusal of a restricted member's trade that names no project, or
// several, says.
const oneProjectRule = "a restricted member's machine login names exactly one project"

// machineTrade is a dev box session's trade of its machine login's token, asking for the projects
// raw spells, with the key the machine logged in with.
func (f *exchangeFixture) machineTrade(t *testing.T, machine, raw string, key *ecdsa.PrivateKey) (int, map[string]any) {
	t.Helper()
	return f.exchange(t, withProjects(exchangeForm(machine, oauthwire.SubjectTokenTypeAccessToken, "ada-devbox"), raw), key)
}

// tokensOf is how many access tokens the broker stores for the session named, read from their rows.
func (f *exchangeFixture) tokensOf(t *testing.T, session string) int {
	t.Helper()
	var stored int
	if err := f.pool.QueryRow(context.Background(), `select count(*) from oauth_access_tokens where request #>> '{session,extra,session}' = $1`, session).Scan(&stored); err != nil {
		t.Fatalf("count the tokens of %s: %v", session, err)
	}
	return stored
}

// expiresIn is the expires_in of a token answer, in seconds, failing the test when there is none.
func expiresIn(t *testing.T, body map[string]any) time.Duration {
	t.Helper()
	seconds, ok := body["expires_in"].(float64)
	if !ok || seconds <= 0 {
		t.Fatalf("the token answer %v names no expires_in", body)
	}
	return time.Duration(seconds) * time.Second
}

// introspectedLifetime is how long the token has left by its introspection's exp, read now.
func (f *exchangeFixture) introspectedLifetime(t *testing.T, token string) time.Duration {
	t.Helper()
	answer, err := f.provider.Introspect(context.Background(), token)
	if err != nil || !answer.Active || answer.ExpiresAt == 0 {
		t.Fatalf("Introspect = %+v, %v; want a live token with an exp", answer, err)
	}
	return time.Until(time.Unix(answer.ExpiresAt, 0))
}

// TestARestrictedMembersMachineTradeIsNamedToItsOneProject: a machine login whose approver Dispatch
// calls a restricted member trades for a token of the named scope listing exactly the one project
// it asked for, which Dispatch gives neither the person nor the approve scope; and the token lives
// the lesser of exchange.RestrictedLifetime and BROKER_ACCESS_TOKEN_SECONDS, by its answer's
// expires_in and by its introspection's exp alike. A trade asking for no project, or for two, is
// refused naming the rule, and one asking for a project the person does not see is refused as
// every trade is.
//
// Controls: mint the person scope in Set.Machine for every approver, and the first row fails
// naming the scope; drop the lifetime cap with BROKER_ACCESS_TOKEN_SECONDS at 3600, and the
// lifetime row fails naming the hour.
func TestARestrictedMembersMachineTradeIsNamedToItsOneProject(t *testing.T) {
	for _, c := range []struct {
		name     string
		lifetime time.Duration
		want     time.Duration
	}{
		{"BROKER_ACCESS_TOKEN_SECONDS at 3600", time.Hour, restrictedLifetime},
		{"BROKER_ACCESS_TOKEN_SECONDS at 30", 30 * time.Second, 30 * time.Second},
	} {
		t.Run(c.name, func(t *testing.T) {
			access := newFakeAccess()
			access.kinds[person] = projectaccess.MemberRestricted
			f := newExchangeFixture(t, func(cfg *oauth.Config) {
				cfg.Exchange.Projects = access
				cfg.AccessLifetime = c.lifetime
			})
			key := newKey(t)
			machine := f.devBoxToken(t, key, person)

			status, body := f.machineTrade(t, machine, project, key)
			token := accessTokenOf(t, status, body)
			answer, err := f.provider.Introspect(context.Background(), token)
			if err != nil || !answer.Active || answer.ProjectScope != oauthwire.ProjectScopeNamed || !slices.Equal(answer.Projects, []string{project}) {
				t.Fatalf("a restricted member's trade asking for %s introspects as %+v (%v), want project scope %s naming exactly [%s]",
					project, answer, err, oauthwire.ProjectScopeNamed, project)
			}
			if answer.Person != person || answer.Session != "ada-devbox" {
				t.Fatalf("Introspect = %+v, want the session ada-devbox acting for %s", answer, person)
			}
			if got := expiresIn(t, body); got > c.want {
				t.Fatalf("the trade's expires_in is %s, want at most %s", got, c.want)
			}
			if got := f.introspectedLifetime(t, token); got > c.want {
				t.Fatalf("the token's introspected exp is %s away, want at most %s", got, c.want)
			}

			for _, asked := range []string{"", "PRIV " + project} {
				status, body := f.machineTrade(t, machine, asked, key)
				refusedSaying(t, status, body, http.StatusBadRequest, "invalid_request", oneProjectRule)
				if description, _ := body["error_description"].(string); !strings.Contains(description, "AGENT_SECRETS_PROJECTS") {
					t.Fatalf("the refusal of a trade asking for %q reads %q, want it to say how to name the one project", asked, description)
				}
			}
			status, body = f.machineTrade(t, machine, "SECRET", key)
			refusedSaying(t, status, body, http.StatusBadRequest, "invalid_request", "The token names a project its person cannot see: SECRET.")
		})
	}
}

// TestAMachineTradeOfAPersonNotRestrictedKeepsThePersonScope is the control the restricted rows
// run beside: a full member's machine login, and one of a person Dispatch holds no row for, trade
// as they always have, a token acting as its person that names the projects it asked for, none
// or several, living the broker's own BROKER_ACCESS_TOKEN_SECONDS.
func TestAMachineTradeOfAPersonNotRestrictedKeepsThePersonScope(t *testing.T) {
	for _, kind := range []string{projectaccess.MemberFull, projectaccess.MemberNone} {
		t.Run(kind, func(t *testing.T) {
			access := newFakeAccess()
			access.kinds[person] = kind
			f := newExchangeFixture(t, func(cfg *oauth.Config) {
				cfg.Exchange.Projects = access
				cfg.AccessLifetime = time.Hour
			})
			key := newKey(t)
			machine := f.devBoxToken(t, key, person)
			for _, c := range []struct {
				asked string
				want  []string
			}{
				{"", nil},
				{project, []string{project}},
				{"PRIV " + project, []string{project, "PRIV"}},
			} {
				status, body := f.machineTrade(t, machine, c.asked, key)
				token := accessTokenOf(t, status, body)
				f.introspectsProjects(t, token, oauthwire.ProjectScopePerson, c.want)
				if got := expiresIn(t, body); got <= restrictedLifetime {
					t.Fatalf("a %s member's trade asking for %q expires_in %s, want the broker's own hour", kind, c.asked, got)
				}
			}
		})
	}
}

// TestARestrictedMembersTradeIsRefusedWhileDispatchCannotBeAsked: the trade of a machine login
// asks Dispatch what kind of member its approver is before it decides the token's scope, and while
// Dispatch cannot be asked it is refused temporarily_unavailable, a list naming no project
// included: the broker cannot tell a restricted member's machine from anyone else's, so it mints
// no token acting as its person on a guess. A refusal of the broker's own account token, and a
// member kind the broker does not know, are refused server_error.
func TestARestrictedMembersTradeIsRefusedWhileDispatchCannotBeAsked(t *testing.T) {
	access := newFakeAccess()
	access.kinds[person] = projectaccess.MemberRestricted
	f := newExchangeFixture(t, func(cfg *oauth.Config) { cfg.Exchange.Projects = access })
	key := newKey(t)
	machine := f.devBoxToken(t, key, person)

	access.set(func(a *fakeAccess) { a.down = true })
	for _, asked := range []string{"", project} {
		status, body := f.machineTrade(t, machine, asked, key)
		refusedSaying(t, status, body, http.StatusServiceUnavailable, "temporarily_unavailable", "Dispatch could not be asked")
	}
	access.set(func(a *fakeAccess) { a.down, a.refused = false, true })
	status, body := f.machineTrade(t, machine, project, key)
	refusedSaying(t, status, body, http.StatusInternalServerError, "server_error", "BROKER_PROJECT_ACCESS_TOKEN_FILE")
	access.set(func(a *fakeAccess) { a.refused, a.kinds[person] = false, "guest" })
	status, body = f.machineTrade(t, machine, project, key)
	refusedSaying(t, status, body, http.StatusInternalServerError, "server_error", "a member kind this broker does not know")

	if stored := f.tokensOf(t, "ada-devbox"); stored != 0 {
		t.Fatalf("the refused trades stored %d tokens, want none", stored)
	}
	// The count's own control: the trade Dispatch answers again is stored, and counted.
	access.set(func(a *fakeAccess) { a.kinds[person] = projectaccess.MemberRestricted })
	status, body = f.machineTrade(t, machine, project, key)
	f.introspectsProjects(t, accessTokenOf(t, status, body), oauthwire.ProjectScopeNamed, []string{project})
	if stored := f.tokensOf(t, "ada-devbox"); stored != 1 {
		t.Fatalf("the trade answered once Dispatch answers again stored %d tokens, want 1", stored)
	}
}
