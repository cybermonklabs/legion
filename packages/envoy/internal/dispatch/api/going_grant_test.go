// going_grant_test.go holds what a command-bar run acts as: the grant the request that starts or
// approves a run records, resolved again at each call of the run as the person's own request
// would be, and the mark every act of the bar carries beside the person.
package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/ciphertoken"
	"github.com/sjawhar/envoy/internal/clock"
	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/identity"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/oidc"
)

// TestTheBarsMarkOnAWrite: a write the command bar makes for a run, through the route table called
// in-process as the person who pressed it, is recorded as that person - the event's actor is
// alice, proven - with the bar's act beside them: agent command-bar, the run's id, and the scopes
// her sign-in holds. An answer the bar gives for her to a decision block of a document she
// approved, the whole change since her approval, carries that approval to the version it settles
// into, as her own answer at a browser does, and the answer names the bar.
func TestTheBarsMarkOnAWrite(t *testing.T) {
	t.Parallel()
	rig := newGoingWritesRig(t)
	document := createInteractionIssue(t, rig.mux, "DEMO", "Carried through the bar", "Context\n")

	t.Run("a write of a run", func(t *testing.T) {
		issue := rig.issue(t, "Moved through the bar")
		doer := rig.served.goingDoer(goingCallerOf(t, rig, false), testServerURL)
		if status, body := goingInProcessSend(t, doer, goingJSONSend(t, http.MethodPatch, "/api/v1/issues/"+issue, map[string]any{
			"status": "in_progress",
		})); status != http.StatusOK {
			t.Fatalf("the bar's write: status=%d body=%s", status, body)
		}
		want := model.Actor{Kind: "user", ID: "alice", Proven: true, Act: &model.ActorAct{
			Scopes: model.Scopes, Agent: model.ActAgentCommandBar, Run: goingTestRun,
		}}
		if actor := newestEventActor(t, rig.mux, issue, "issue.updated"); !reflect.DeepEqual(actor, want) {
			t.Fatalf("the change's event is recorded as %+v (act %+v), want alice with the bar's act %+v", actor, actor.Act, want.Act)
		}
	})

	t.Run("an approval answered through the bar carries", func(t *testing.T) {
		artifact := document.PrimaryArtifactID
		reviewDocument(t, rig.mux, artifact, "alice", "approved", "")
		asks := insertDecisionBlocks(t, rig.mux, document.Key, artifact, waveAsk, 2, map[string]string{"wave": "How many regions take the first wave?"})
		doer := rig.served.goingDoer(goingCallerOf(t, rig, false), testServerURL)
		if status, body := goingInProcessSend(t, doer, goingJSONSend(t, http.MethodPost, "/api/v1/asks/"+asks["wave"]+"/answer", map[string]any{
			"selected": []string{"Two"}, "text": "Two regions first.", "expected_edited_at": nil,
		})); status != http.StatusOK {
			t.Fatalf("the bar's answer: status=%d body=%s", status, body)
		}
		waitForArtifactVersion(t, rig.mux, artifact, 3)
		requireCarriedReview(t, rig.mux, artifact, 3, 1, nil)
		read := decodeBody[struct {
			Ask struct {
				Answer *model.AskAnswer `json:"answer"`
			} `json:"ask"`
		}](t, rig.fixture(t, "alice", http.MethodGet, "/api/v1/asks/"+asks["wave"], nil))
		answer := read.Ask.Answer
		if answer == nil || answer.User != "alice" || answer.Act == nil || answer.Act.Agent != model.ActAgentCommandBar ||
			answer.Act.Run != goingTestRun || !slices.Equal(answer.Act.Scopes, model.Scopes) {
			t.Fatalf("the answer reads %+v, want alice's with the bar's act naming run %s and her scopes", answer, goingTestRun)
		}
	})
}

// goingGrantRefresher is the sign-in pool a lapsed confirmation's refresh meets: it answers every
// refresh with err.
type goingGrantRefresher struct {
	err error
}

func (r *goingGrantRefresher) Refresh(context.Context, string) (oidc.Session, error) {
	return oidc.Session{}, r.err
}

// goingGrantRig is a server whose people sign in with a session cookie, their membership confirmed
// through a sign-in pool (refresher), which also takes broker-issued and service-account tokens;
// beside it, on the same database, a server naming its people by the test header and one behind
// IAP.
type goingGrantRig struct {
	database  *store.Store
	sessions  *store.PgSessionStore
	people    *store.PgPeopleStore
	refresher *goingGrantRefresher
	issuer    *serviceTokenIssuer
	brokered  *brokerTokenRig
	cookies   *server
	header    *server
	iapKeys   iapSocketKeys
	iap       *server
}

func newGoingGrantRig(t *testing.T) goingGrantRig {
	t.Helper()
	database := storetest.Open(t)
	rig := goingGrantRig{
		database: database, sessions: store.NewPgSessionStore(database.Pool),
		people:    store.NewPgPeopleStore(database.Pool, testSigningKey, nil),
		refresher: &goingGrantRefresher{}, issuer: newServiceTokenIssuer(t),
	}
	cookies := identity.CookieIdentity{SigningKey: testSigningKey, Sessions: rig.sessions, Membership: &identity.Membership{
		People: rig.people, Sessions: rig.sessions, SignIn: rig.refresher, Rule: identity.GroupRule{Group: "dispatch-members"},
	}}
	rig.brokered = newBrokerTokenRigOn(t, testServerOptions{store: database, identity: cookies, oidc: rig.issuer.verifier(t)})
	rig.cookies = rig.brokered.served
	rig.header = directServer(Deps{Store: database, Identity: headerIdentity(database)})
	// The grant's IAP budget is judged against time.Now (rig.caller), so the assertion's verifier
	// runs on the real clock too.
	rig.iapKeys = newIAPSocketKeys(t, database, clock.Real())
	rig.iap = directServer(Deps{Store: database, Identity: rig.iapKeys.identity})
	return rig
}

// signedIn is login's session cookie, their sign-in through the pool confirmed now.
func (rig goingGrantRig) signedIn(t *testing.T, login string) *http.Cookie {
	t.Helper()
	ctx := context.Background()
	if err := rig.people.SignIn(ctx, login, "a-refresh-token-of-"+login); err != nil {
		t.Fatalf("record %s's sign-in: %v", login, err)
	}
	generation, err := rig.sessions.EnsureSession(ctx, login)
	if err != nil {
		t.Fatalf("sign %s in: %v", login, err)
	}
	cookie, err := http.ParseSetCookie(auth.IssueSessionCookie(login, generation, testSigningKey, true))
	if err != nil {
		t.Fatalf("parse %s's session cookie: %v", login, err)
	}
	return cookie
}

// lapse dates login's confirmation two hours back, past identity.MembershipMaxAge, so the next
// check of it asks the pool.
func (rig goingGrantRig) lapse(t *testing.T, login string) {
	t.Helper()
	rig.exec(t, `update people set confirmed_at = now() - interval '2 hours' where email = $1`, login)
}

func (rig goingGrantRig) exec(t *testing.T, statement string, arguments ...any) {
	t.Helper()
	if _, err := rig.database.Pool.Exec(context.Background(), statement, arguments...); err != nil {
		t.Fatalf("%s: %v", statement, err)
	}
}

// run records a queued run of login's, as the route that starts a run writes one, and answers its
// id.
func (rig goingGrantRig) run(t *testing.T, login string) string {
	t.Helper()
	id := uuid.NewString()
	rig.exec(t, `
		insert into going_runs (id, author_login, words, conversation_id, level, mode, state, shape)
		values ($1::uuid, $2, 'the words of a run', $1::uuid, 'standard', 'ask', 'queued', $3)
	`, id, login, goingRunShape)
	return id
}

// grant records run's grant from request as served resolves the request's credential, in a
// transaction of its own, as the request that starts or approves a run does, and answers the
// refusal of a credential that starts no run.
func (rig goingGrantRig) grant(t *testing.T, served *server, run string, request *http.Request) error {
	t.Helper()
	grant, err := served.goingGrantOf(request)
	if err != nil {
		return err
	}
	ctx := context.Background()
	tx, err := rig.database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := recordGoingGrant(ctx, tx, run, grant); err != nil {
		t.Fatalf("record the grant: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the grant: %v", err)
	}
	return nil
}

// granted is grant for a request whose credential must start the run.
func (rig goingGrantRig) granted(t *testing.T, served *server, run string, request *http.Request) {
	t.Helper()
	if err := rig.grant(t, served, run, request); err != nil {
		t.Fatalf("the request was refused its run: %v", err)
	}
}

// caller is the caller of run's next call as served resolves it, an hour of the run's working
// budget left.
func (rig goingGrantRig) caller(served *server, run string) (resolvedCaller, error) {
	return served.goingGrantCaller(context.Background(), run, time.Now().Add(time.Hour))
}

// tokenID is the agent_tokens row of the personal token plaintext.
func (rig goingGrantRig) tokenID(t *testing.T, plaintext string) string {
	t.Helper()
	hash := sha256.Sum256([]byte(plaintext))
	var id string
	if err := rig.database.Pool.QueryRow(context.Background(), `select id::text from agent_tokens where token_hash = $1`, hash[:]).Scan(&id); err != nil {
		t.Fatalf("read the token's row: %v", err)
	}
	return id
}

// goingStarting is a request that starts a run, presenting what present sets on it.
func goingStarting(present func(*http.Request)) *http.Request {
	request := httptest.NewRequest(http.MethodPost, "/api/v1/going/runs", nil)
	present(request)
	return request
}

// goingBearer presents token as a bearer.
func goingBearer(token string) func(*http.Request) {
	return func(request *http.Request) { request.Header.Set("Authorization", "Bearer "+token) }
}

// requireGrantEnded fails t unless err refuses the call because the run's grant ended for reason.
func requireGrantEnded(t *testing.T, err error, reason, what string) {
	t.Helper()
	var ended goingGrantEnded
	if !errors.As(err, &ended) || ended.Reason != reason {
		t.Fatalf("%s: the call was answered %v, want it refused %s", what, err, reason)
	}
}

// goingBarPerson is the caller a run of login's own sign-in calls as.
func goingBarPerson(login, run string) resolvedCaller {
	return resolvedCaller{human: true, actor: model.Actor{
		Kind: "user", ID: login, Credential: personCredential, Proven: true,
		Act: &model.ActorAct{Scopes: model.Scopes, Agent: model.ActAgentCommandBar, Run: run},
	}}
}

// TestTheGrantResolvesAsTheRequestWould: the grant a run's start records names the person and
// the credential and no secret of it, and each call of the run resolves the person from it again,
// against the store as it is at that call, with the checks the person's own request gets: a
// sign-out, a sign-in past its cookie's expiry, a revoked token and a lost membership each refuse
// the next call with their reason; a membership the sign-in pool could not confirm is an error to
// wait on and try again, which ends nothing; a sign-in through IAP lasts its run's working budget,
// and an approval through IAP moves the grant to it; a token's caller holds the scopes its row
// holds at the call; an approval under another token moves the grant to it, so revoking the first
// refuses nothing; and a broker-issued token, the shared token and a service-account token start
// no run.
func TestTheGrantResolvesAsTheRequestWould(t *testing.T) {
	t.Parallel()
	rig := newGoingGrantRig(t)
	ctx := context.Background()

	t.Run("a sign-out refuses the next call", func(t *testing.T) {
		run := rig.run(t, "alice")
		rig.granted(t, rig.cookies, run, goingStarting(func(r *http.Request) { r.AddCookie(rig.signedIn(t, "alice")) }))
		caller, err := rig.caller(rig.cookies, run)
		if want := goingBarPerson("alice", run); err != nil || !reflect.DeepEqual(caller, want) {
			t.Fatalf("the call while alice is signed in resolves %+v, %v; want %+v", caller, err, want)
		}
		if err := rig.sessions.RevokeSessions(ctx, "alice"); err != nil {
			t.Fatalf("sign alice out: %v", err)
		}
		_, err = rig.caller(rig.cookies, run)
		requireGrantEnded(t, err, goingGrantSessionEnded, "the call after alice signed out")
	})

	t.Run("a sign-in past its cookie's expiry refuses the next call", func(t *testing.T) {
		run := rig.run(t, "carol")
		rig.granted(t, rig.cookies, run, goingStarting(func(r *http.Request) { r.AddCookie(rig.signedIn(t, "carol")) }))
		if _, err := rig.caller(rig.cookies, run); err != nil {
			t.Fatalf("the call inside the cookie's 30 days: %v", err)
		}
		// The cookie's 30 days, gone by: the expiry the grant took from it is behind now.
		rig.exec(t, `update going_run_grants set expires_at = now() - interval '1 second' where run_id = $1`, run)
		_, err := rig.caller(rig.cookies, run)
		requireGrantEnded(t, err, goingGrantSignInExpired, "the call past the cookie's expiry")
	})

	t.Run("a lost membership refuses the next call", func(t *testing.T) {
		run := rig.run(t, "dana")
		rig.granted(t, rig.cookies, run, goingStarting(func(r *http.Request) { r.AddCookie(rig.signedIn(t, "dana")) }))
		rig.lapse(t, "dana")
		rig.refresher.err = fmt.Errorf("%w: the pool refused the refresh token", oidc.ErrGrantRefused)
		_, err := rig.caller(rig.cookies, run)
		requireGrantEnded(t, err, goingGrantMembershipEnded, "the call after dana left the group")
	})

	t.Run("a membership the pool could not confirm waits, and the next try runs", func(t *testing.T) {
		run := rig.run(t, "erin")
		rig.granted(t, rig.cookies, run, goingStarting(func(r *http.Request) { r.AddCookie(rig.signedIn(t, "erin")) }))
		rig.lapse(t, "erin")
		rig.refresher.err = fmt.Errorf("%w: the pool answered 503", oidc.ErrIssuerUnavailable)
		_, err := rig.caller(rig.cookies, run)
		var ended goingGrantEnded
		if !errors.Is(err, identity.ErrSignInUnavailable) || errors.As(err, &ended) {
			t.Fatalf("the call while the pool answers 503 was answered %v, want SIGNIN_UNAVAILABLE to wait on, ending nothing", err)
		}
		// The pool answers again and confirms erin: the same grant serves the next try.
		rig.exec(t, `update people set confirmed_at = now() where email = $1`, "erin")
		if caller, err := rig.caller(rig.cookies, run); err != nil || !reflect.DeepEqual(caller, goingBarPerson("erin", run)) {
			t.Fatalf("the try after the pool answered resolves %+v, %v; want erin", caller, err)
		}
	})

	t.Run("a token's call holds the scopes its row holds, until the token is revoked", func(t *testing.T) {
		// The widest set a person's token may hold (model.Scopes: person, approve and p0), so the
		// row holds the set the call is held to below, p0 among it.
		token := personalTokenOwnedBy(t, rig.database, "alice", model.Scopes...)
		id := rig.tokenID(t, token)
		run := rig.run(t, "alice")
		rig.granted(t, rig.cookies, run, goingStarting(goingBearer(token)))
		caller, err := rig.caller(rig.cookies, run)
		want := resolvedCaller{human: true, actor: model.Actor{
			Kind: "user", ID: "alice", Proven: true,
			Credential: &model.Credential{Scopes: model.Scopes, TokenID: id, TokenName: "probe"},
			Act:        &model.ActorAct{TokenID: id, TokenName: "probe", Scopes: model.Scopes, Agent: model.ActAgentCommandBar, Run: run},
		}}
		if err != nil || !reflect.DeepEqual(caller, want) {
			t.Fatalf("the call under alice's token resolves %+v (credential %+v, act %+v), %v; want %+v (credential %+v, act %+v)",
				caller, caller.actor.Credential, caller.actor.Act, err, want, want.actor.Credential, want.actor.Act)
		}
		// Narrowed to person: the next call holds person alone, and a route needing approve
		// refuses it as it refuses the token's own request.
		rig.exec(t, `update agent_tokens set scopes = '{person}' where id = $1::uuid`, id)
		caller, err = rig.caller(rig.cookies, run)
		if err != nil || caller.actor.Credential.Holds(model.ScopeApprove) || !caller.actor.Credential.Holds(model.ScopePerson) ||
			!slices.Equal(caller.actor.Act.Scopes, []string{model.ScopePerson}) {
			t.Fatalf("the call after the token was narrowed resolves %+v (act %+v), %v; want person alone", caller.actor.Credential, caller.actor.Act, err)
		}
		rig.exec(t, `update agent_tokens set revoked_at = now() where id = $1::uuid`, id)
		_, err = rig.caller(rig.cookies, run)
		requireGrantEnded(t, err, goingGrantTokenRevoked, "the call after the token was revoked")
	})

	t.Run("an approval by token B moves the grant, and revoking token A refuses nothing", func(t *testing.T) {
		first := personalTokenOwnedBy(t, rig.database, "alice", model.ScopePerson, model.ScopeApprove)
		second := personalTokenOwnedBy(t, rig.database, "alice", model.ScopePerson, model.ScopeApprove)
		run := rig.run(t, "alice")
		rig.granted(t, rig.cookies, run, goingStarting(goingBearer(first)))
		rig.granted(t, rig.cookies, run, goingStarting(goingBearer(second)))
		var grants int
		var token string
		if err := rig.database.Pool.QueryRow(ctx, `
			select count(*), max(token_id::text) from going_run_grants where run_id = $1::uuid
		`, run).Scan(&grants, &token); err != nil || grants != 1 || token != rig.tokenID(t, second) {
			t.Fatalf("the run holds %d grants naming token %s (%v), want one naming the approving token", grants, token, err)
		}
		rig.exec(t, `update agent_tokens set revoked_at = now() where id = $1::uuid`, rig.tokenID(t, first))
		if caller, err := rig.caller(rig.cookies, run); err != nil || caller.actor.Credential.TokenID != rig.tokenID(t, second) {
			t.Fatalf("the call after token A was revoked resolves %+v, %v; want token B", caller.actor.Credential, err)
		}
	})

	t.Run("a sign-in through IAP lasts its run's working budget, and an approval through IAP moves the grant to it", func(t *testing.T) {
		const person = "a.person@example.com"
		run := rig.run(t, person)
		assertion := rig.iapKeys.assertion(t, time.Now().Add(9*time.Minute)).Get(identity.IAPAssertionHeader)
		throughIAP := func(r *http.Request) { r.Header.Set(identity.IAPAssertionHeader, assertion) }
		rig.granted(t, rig.iap, run, goingStarting(throughIAP))
		caller, err := rig.iap.goingGrantCaller(ctx, run, time.Now().Add(time.Hour))
		if want := goingBarPerson(person, run); err != nil || !reflect.DeepEqual(caller, want) {
			t.Fatalf("the call inside the budget resolves %+v, %v; want %+v", caller, err, want)
		}
		_, err = rig.iap.goingGrantCaller(ctx, run, time.Now().Add(-time.Second))
		requireGrantEnded(t, err, goingGrantSignInExpired, "the call past the run's working budget")

		// A run the person started with a token of theirs, approved through IAP: the grant is the
		// approval's, so revoking the token refuses nothing, and the run's budget still ends it.
		started := personalTokenOwnedBy(t, rig.database, person, model.ScopePerson, model.ScopeApprove)
		moved := rig.run(t, person)
		rig.granted(t, rig.cookies, moved, goingStarting(goingBearer(started)))
		approval := httptest.NewRequest(http.MethodPost, "/api/v1/going/runs/"+moved+"/plan/approve", nil)
		throughIAP(approval)
		rig.granted(t, rig.iap, moved, approval)
		var kind string
		var token *string
		if err := rig.database.Pool.QueryRow(ctx, `select kind, token_id::text from going_run_grants where run_id = $1::uuid`, moved).Scan(&kind, &token); err != nil ||
			kind != goingGrantIAP || token != nil {
			t.Fatalf("the approved run's grant is %s naming token %v (%v), want the approval's sign-in through IAP", kind, token, err)
		}
		rig.exec(t, `update agent_tokens set revoked_at = now() where id = $1::uuid`, rig.tokenID(t, started))
		caller, err = rig.iap.goingGrantCaller(ctx, moved, time.Now().Add(time.Hour))
		if want := goingBarPerson(person, moved); err != nil || !reflect.DeepEqual(caller, want) {
			t.Fatalf("the call after the starting token was revoked resolves %+v, %v; want %+v", caller, err, want)
		}
		_, err = rig.iap.goingGrantCaller(ctx, moved, time.Now().Add(-time.Second))
		requireGrantEnded(t, err, goingGrantSignInExpired, "the approved run's call past its working budget")
	})

	t.Run("a credential that is no person's own starts no run", func(t *testing.T) {
		broker := newBrokerCaller(t)
		const brokerToken = "a-person-scoped-broker-token"
		answer := liveNamedToken(broker.jkt)
		answer.ProjectScope, answer.Projects = ciphertoken.ProjectScopePerson, nil
		rig.brokered.broker.Says(brokerToken, answer)
		for _, refused := range []struct {
			name    string
			request *http.Request
		}{
			{"a broker-issued token", broker.presenting(t, http.MethodPost, "/api/v1/going/runs", brokerToken, nil)},
			{"the shared token", goingStarting(goingBearer("agent-token"))},
			{"a service-account token", goingStarting(goingBearer(rig.issuer.mint(t, serviceTokenAudience)))},
		} {
			t.Run(refused.name, func(t *testing.T) {
				run := rig.run(t, "alice")
				err := rig.grant(t, rig.cookies, run, refused.request)
				var refusal *apiError
				if !errors.As(err, &refusal) || refusal.status != http.StatusForbidden || refusal.code != codeRunNeedsPersonCredential {
					t.Fatalf("the start under %s was answered %v, want 403 %s", refused.name, err, codeRunNeedsPersonCredential)
				}
				var grants int
				if err := rig.database.Pool.QueryRow(ctx, `select count(*) from going_run_grants where run_id = $1::uuid`, run).Scan(&grants); err != nil || grants != 0 {
					t.Fatalf("the refused start left %d grants (%v), want none", grants, err)
				}
			})
		}
	})

	t.Run("the stored grant holds no cookie, assertion, token or hash", func(t *testing.T) {
		cookie := rig.signedIn(t, "frank")
		session, signed := auth.VerifySession(cookie.Value, testSigningKey)
		if !signed {
			t.Fatal("frank's cookie does not verify")
		}
		assertion := rig.iapKeys.assertion(t, time.Now().Add(9*time.Minute)).Get(identity.IAPAssertionHeader)
		token := personalTokenOwnedBy(t, rig.database, "frank", model.ScopePerson, model.ScopeApprove)
		hash := sha256.Sum256([]byte(token))
		runs := map[string]string{
			goingGrantSignIn: rig.run(t, "frank"), goingGrantIAP: rig.run(t, "a.person@example.com"),
			goingGrantToken: rig.run(t, "frank"), goingGrantHeader: rig.run(t, "frank"),
		}
		rig.granted(t, rig.cookies, runs[goingGrantSignIn], goingStarting(func(r *http.Request) { r.AddCookie(cookie) }))
		rig.granted(t, rig.iap, runs[goingGrantIAP], goingStarting(func(r *http.Request) { r.Header.Set(identity.IAPAssertionHeader, assertion) }))
		rig.granted(t, rig.cookies, runs[goingGrantToken], goingStarting(goingBearer(token)))
		rig.granted(t, rig.header, runs[goingGrantHeader], goingStarting(func(r *http.Request) { r.Header.Set("X-Dispatch-User", "Frank") }))

		type stored struct {
			Kind, Login string
			Generation  *int64
			Expires     *time.Time
			Token       *string
		}
		generation, expires, tokenID := session.Generation, session.Expires, rig.tokenID(t, token)
		wants := map[string]stored{
			goingGrantSignIn: {Kind: goingGrantSignIn, Login: "frank", Generation: &generation, Expires: &expires},
			goingGrantIAP:    {Kind: goingGrantIAP, Login: "a.person@example.com"},
			goingGrantToken:  {Kind: goingGrantToken, Login: "frank", Token: &tokenID},
			goingGrantHeader: {Kind: goingGrantHeader, Login: "frank"},
		}
		secrets := []string{cookie.Value, assertion, token, strings.TrimPrefix(token, "dsp_"), hex.EncodeToString(hash[:])}
		for _, credential := range []string{cookie.Value, assertion} {
			parts := strings.FieldsFunc(credential, func(r rune) bool { return r == '.' })
			secrets = append(secrets, parts[len(parts)-1])
		}
		for kind, run := range runs {
			var got stored
			var row string
			if err := rig.database.Pool.QueryRow(ctx, `
				select kind, login, generation, expires_at, token_id::text, row_to_json(g)::text
				from going_run_grants g where run_id = $1::uuid
			`, run).Scan(&got.Kind, &got.Login, &got.Generation, &got.Expires, &got.Token, &row); err != nil {
				t.Fatalf("read the %s grant: %v", kind, err)
			}
			if want := wants[kind]; got.Kind != want.Kind || got.Login != want.Login ||
				!reflect.DeepEqual(got.Generation, want.Generation) || !reflect.DeepEqual(got.Token, want.Token) ||
				(got.Expires == nil) != (want.Expires == nil) || (got.Expires != nil && !got.Expires.Equal(*want.Expires)) {
				t.Fatalf("the %s grant holds %s, want what its credential names: %+v", kind, row, want)
			}
			for _, secret := range secrets {
				if strings.Contains(row, secret) {
					t.Fatalf("the %s grant %s holds a credential the request presented: %q", kind, row, secret)
				}
			}
		}
		if caller, err := rig.caller(rig.header, runs[goingGrantHeader]); err != nil || !reflect.DeepEqual(caller, goingBarPerson("frank", runs[goingGrantHeader])) {
			t.Fatalf("the header grant's call resolves %+v, %v; want frank", caller, err)
		}
	})
}
