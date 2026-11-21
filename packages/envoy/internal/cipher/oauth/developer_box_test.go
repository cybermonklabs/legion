package oauth_test

import (
	"context"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/exchange"
	"github.com/sjawhar/envoy/internal/cipher/oauth"
	"github.com/sjawhar/envoy/internal/cipher/oauthwire"
	"github.com/sjawhar/envoy/internal/cipher/projectaccess"
	"github.com/sjawhar/envoy/internal/cipher/store"
)

// closedRefusal is what the exchange says of a developer box's trade naming PRIV while Dispatch
// answers it closed to developer boxes.
const closedRefusal = "The token names the project PRIV, which is private or turning private, and a developer box gets no token of such a project"

// TestADeveloperBoxGetsNoTokenOfAPrivateProject: the exchange refuses a developer box's token on
// Dispatch's closed_to_developer_boxes alone. While PRIV is public, and while it is private on its
// first day, before its full switch (the field false), a full member's machine-login trade and a
// person's sign-in on a machine naming PRIV are each answered. While Dispatch answers PRIV closed
// to developer boxes, in its closing state (public) and after its full switch (private), the
// machine-login trade naming PRIV is refused invalid_target naming it, and so are its renewal (a
// fresh trade of the same machine login, session and list as the tokens minted before), a list
// naming PRIV among others, and the sign-in naming PRIV; nothing of them is stored. The same box's
// trade naming another project is still answered, and so is a restricted member's named trade
// naming PRIV, whose list names them: that token is the outside person's own, not a developer
// box's. The broker's count of PRIV's live developer-box tokens reads the four minted before, and
// neither named token, and falls to 0 once those four are revoked.
func TestADeveloperBoxGetsNoTokenOfAPrivateProject(t *testing.T) {
	const bob = "bob@outside.example"
	access := newFakeAccess()
	access.sees[bob] = []string{"PRIV"}
	access.kinds[bob] = projectaccess.MemberRestricted
	f := newSignInFixtureWith(t, func(c *oauth.Config) {
		c.Exchange.Policy = newPolicy(t, []exchange.AccountEntry{{Account: account, Project: "PRIV", Person: bob}}, nil,
			exchange.GoogleEntry{Account: signInMachine, Service: signInFleet})
		c.Exchange.Projects = access
	})
	ctx := context.Background()
	counted := &store.Store{Pool: f.pool}
	live := func(t *testing.T) int {
		t.Helper()
		tokens, err := counted.LiveDeveloperBoxTokens(ctx, "PRIV")
		if err != nil {
			t.Fatalf("LiveDeveloperBoxTokens(PRIV): %v", err)
		}
		return tokens.Live
	}
	key, signInKey := newKey(t), newKey(t)
	machine := f.devBoxToken(t, key, person)
	machineTrade := func(raw string) (int, map[string]any) {
		return f.exchange(t, withProjects(exchangeForm(machine, oauthwire.SubjectTokenTypeAccessToken, "ada-devbox"), raw), key)
	}
	signInTrade := func(raw string) (int, map[string]any) {
		form := signInForm(f.personToken(t, person, deviceAudience, true, signInDomain), f.machineToken(t, signInMachine, machineAudience, true), "ada-box-1")
		return f.exchange(t, withProjects(form, raw), signInKey)
	}
	f.registers(t, account, "pod-uid-bob", "bob-agent")
	namedTrade := func() url.Values {
		return exchangeForm(f.podToken(t, "pod-uid-bob", nil), oauthwire.SubjectTokenTypeKubernetes, "bob-agent")
	}

	mintBoth := func(t *testing.T) {
		t.Helper()
		status, body := machineTrade("PRIV")
		f.introspectsProjects(t, accessTokenOf(t, status, body), oauthwire.ProjectScopePerson, []string{"PRIV"})
		status, body = signInTrade("PRIV")
		f.introspectsProjects(t, accessTokenOf(t, status, body), oauthwire.ProjectScopePerson, []string{"PRIV"})
	}
	mintBoth(t)
	if got := live(t); got != 2 {
		t.Fatalf("PRIV's live developer-box tokens while it is public = %d, want the 2 just minted", got)
	}
	t.Run("private on its first day", func(t *testing.T) {
		access.set(func(a *fakeAccess) { a.private["PRIV"] = true })
		mintBoth(t)
		if got := live(t); got != 4 {
			t.Fatalf("PRIV's live developer-box tokens while it is private before its full switch = %d, want 4", got)
		}
	})

	for _, state := range []struct {
		name string
		set  func(*fakeAccess)
	}{
		{"closing", func(a *fakeAccess) { a.private["PRIV"], a.closed["PRIV"] = false, true }},
		{"private after its full switch", func(a *fakeAccess) { a.private["PRIV"], a.closed["PRIV"] = true, true }},
	} {
		t.Run(state.name, func(t *testing.T) {
			access.set(state.set)
			stored := f.count(t, "oauth_access_tokens")
			status, body := machineTrade("PRIV")
			refusedSaying(t, status, body, http.StatusBadRequest, "invalid_target", closedRefusal)
			status, body = machineTrade("PRIV " + project)
			refusedSaying(t, status, body, http.StatusBadRequest, "invalid_target", closedRefusal)
			status, body = signInTrade("PRIV")
			refusedSaying(t, status, body, http.StatusBadRequest, "invalid_target", closedRefusal)
			if after := f.count(t, "oauth_access_tokens"); after != stored {
				t.Fatalf("access tokens after the refused trades = %d, want the %d there were", after, stored)
			}

			status, body = machineTrade(project)
			f.introspectsProjects(t, accessTokenOf(t, status, body), oauthwire.ProjectScopePerson, []string{project})
			f.introspectsProjects(t, f.exchanged(t, namedTrade(), key), oauthwire.ProjectScopeNamed, []string{"PRIV"})
			if got := live(t); got != 4 {
				t.Fatalf("PRIV's live developer-box tokens while it is %s = %d, want the 4 minted before, and no named token", state.name, got)
			}
		})
	}

	tag, err := f.pool.Exec(ctx, `update oauth_access_tokens set active = false
		where active and request #>> '{session,extra,project_scope}' = 'person' and request #>> '{session,extra,projects}' = 'PRIV'`)
	if err != nil || tag.RowsAffected() != 4 {
		t.Fatalf("revoke the four developer-box tokens of PRIV: %v, %d rows", err, tag.RowsAffected())
	}
	if got := live(t); got != 0 {
		t.Fatalf("PRIV's live developer-box tokens once both are revoked = %d, want 0", got)
	}
}

// TestADeveloperBoxTokenIsStoredOnlyWhileItsAnswerIsYoung: a developer box's token is committed
// only while the oldest of Dispatch's answers it rests on is younger than projectaccess.CacheTTL.
// The trade below passes its check on an answer asked ten seconds short of that bound, and is
// held before it stores its token (a share lock on oauth_access_tokens) until the answer has aged
// past it: it is refused temporarily_unavailable and stores nothing, since a token stored then
// might rest on an answer from before its project's closing state began. Released at once, the
// same trade is answered.
func TestADeveloperBoxTokenIsStoredOnlyWhileItsAnswerIsYoung(t *testing.T) {
	access := newFakeAccess()
	const slack = 10 * time.Second
	access.askedAt = func() time.Time { return time.Now().Add(-projectaccess.CacheTTL + slack) }
	f := newExchangeFixture(t, func(c *oauth.Config) { c.Exchange.Projects = access })
	ctx := context.Background()
	key := newKey(t)
	machine := f.devBoxToken(t, key, person)
	form := withProjects(exchangeForm(machine, oauthwire.SubjectTokenTypeAccessToken, "ada-devbox"), "PRIV")

	stored := f.count(t, "oauth_access_tokens")
	holder, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback(ctx)
	if _, err := holder.Exec(ctx, `lock table oauth_access_tokens in share mode`); err != nil {
		t.Fatalf("hold oauth_access_tokens: %v", err)
	}
	proof := dpopProof(t, key, f.url+"/token")
	exchanged := make(chan tokenAnswer, 1)
	go func() { exchanged <- postToken(f.url+"/token", form, proof) }()
	waitForBlocked(t, f.pool, holder.Conn().PgConn().PID(), "insert into oauth_access_tokens%")
	access.mu.Lock()
	aged := access.lastAsked.Add(projectaccess.CacheTTL)
	access.mu.Unlock()
	time.Sleep(time.Until(aged))
	if err := holder.Rollback(ctx); err != nil {
		t.Fatalf("release oauth_access_tokens: %v", err)
	}
	answer := <-exchanged
	if answer.err != nil {
		t.Fatalf("post the trade: %v", answer.err)
	}
	refusedSaying(t, answer.status, answer.body, http.StatusServiceUnavailable, "temporarily_unavailable", "aged out before the token was stored")
	if after := f.count(t, "oauth_access_tokens"); after != stored {
		t.Fatalf("access tokens after the refused trade = %d, want the %d there were", after, stored)
	}

	f.introspectsProjects(t, f.exchanged(t, form, key), oauthwire.ProjectScopePerson, []string{"PRIV"})
}
