package oauth_test

import (
	"context"
	"crypto/ecdsa"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/broker/hold"
	"github.com/sjawhar/envoy/internal/broker/oauth"
	"github.com/sjawhar/envoy/internal/broker/oauthwire"
	"github.com/sjawhar/envoy/internal/broker/projectaccess"
	"github.com/sjawhar/envoy/internal/broker/projectaccess/projectaccesstest"
	"github.com/sjawhar/envoy/internal/broker/store"
	"github.com/sjawhar/envoy/internal/broker/store/storetest"
)

// servedBob is a person the Dispatch stand-in never holds, the control a trade serves beside
// person (ada), whom it answers held once a test holds her.
const servedBob = "bob@example.com"

// heldWords is the broker's refusal of a held person's credential, naming person: the description
// of the token endpoint's membership_held.
func heldWords(person string) string {
	return person + "'s credentials are held by Dispatch; they work again when " + person +
		" signs in to Dispatch, or when Dispatch can confirm " + person + " with Google again"
}

// unkeptWords is the description of the token endpoint's server_error for a held person whose
// answer the broker could not keep: a fixed reason, never the error behind it.
func unkeptWords(person string) string {
	return "whether Dispatch holds " + person + " cannot be read: the broker could not keep Dispatch's answer that holds them"
}

// refusedAccount is what the token endpoint's server_error says while Dispatch refuses the
// broker's own account (refusedByDispatch's hint).
const refusedAccount = "Dispatch refused the broker's own read token"

// brokerAccount is the read token the broker presents to the Dispatch stand-in: the stand-in's own,
// or, once refused is set, one the stand-in holds live for no key and answers 401, as a Dispatch
// that does not take the broker's read token answers it.
type brokerAccount struct {
	*projectaccesstest.Account
	refused atomic.Bool
}

func (a *brokerAccount) Token(ctx context.Context) (string, error) {
	if a.refused.Load() {
		return "a read token Dispatch does not take", nil
	}
	return a.Account.Token(ctx)
}

// holdDispatch is the broker's hold of refused people as cmd/broker builds it where
// BROKER_PROJECT_ACCESS_URL is set: a real hold.Gate over the Dispatch stand-in, on the Dispatch
// client the exchange asks too, keeping Dispatch's answers in a broker database of its own. The
// gate writes apart from the token endpoint's transaction, so which database holds
// person_standing is no part of what a trade decides. The gate and its client read a clock the
// test moves past the client's cache of an answer.
type holdDispatch struct {
	standIn *projectaccesstest.Server
	account *brokerAccount
	access  *projectaccess.Client
	gate    *hold.Gate
	store   *store.Store
	ahead   atomic.Int64
}

// newHoldDispatch is the stand-in holding rows and each of people as a full member, and the gate
// over it.
func newHoldDispatch(t *testing.T, rows map[string]projectaccesstest.Row, people ...string) *holdDispatch {
	t.Helper()
	kinds := map[string]string{}
	for _, p := range people {
		kinds[p] = projectaccess.MemberFull
	}
	d := &holdDispatch{standIn: projectaccesstest.NewServer(t, rows, kinds)}
	d.account = &brokerAccount{Account: d.standIn.Account}
	d.access = projectaccess.New(d.standIn.URL, d.account, nil)
	d.access.UseClock(d.now)
	st, err := store.Open(context.Background(), storetest.URL(t))
	if err != nil {
		t.Fatalf("open the gate's store: %v", err)
	}
	t.Cleanup(st.Close)
	d.store = st
	d.gate = hold.New(hold.Config{Access: d.access, Store: st, Now: d.now})
	return d
}

func (d *holdDispatch) now() time.Time { return time.Now().Add(time.Duration(d.ahead.Load())) }

// configure gives the exchange the gate, beside the Dispatch client it is built on.
func (d *holdDispatch) configure(c *oauth.Config) {
	c.Exchange.Projects, c.Exchange.Hold = d.access, d.gate
}

// pastCache moves the clock past every answer the client keeps, so the gate's next check of a
// person asks Dispatch again.
func (d *holdDispatch) pastCache() { d.ahead.Add(int64(projectaccess.CacheTTL + time.Second)) }

// hold has Dispatch answer person held from the gate's next check of them.
func (d *holdDispatch) hold(person string) {
	d.standIn.SetHeld(person, true)
	d.pastCache()
}

// loseStore closes the gate's database, so from its next check on it can keep no answer Dispatch
// gives, while Dispatch answers as before.
func (d *holdDispatch) loseStore() {
	d.store.Close()
	d.pastCache()
}

// refuseAccount has Dispatch refuse the broker's own account from the gate's next check on.
func (d *holdDispatch) refuseAccount() {
	d.account.refused.Store(true)
	d.pastCache()
}

// expectTrade fails t unless the trade what was answered wantStatus: 200 with an access token, or
// the refusal code with no access token and a description holding says.
func expectTrade(t *testing.T, what string, status int, body map[string]any, wantStatus int, code, says string) {
	t.Helper()
	description, _ := body["error_description"].(string)
	switch {
	case wantStatus == http.StatusOK && (status != http.StatusOK || body["access_token"] == nil):
		t.Errorf("%s = %d %v %q, want 200 and an access token", what, status, body["error"], description)
	case wantStatus != http.StatusOK && (status != wantStatus || body["error"] != code || body["access_token"] != nil || !strings.Contains(description, says)):
		t.Errorf("%s = %d %v %q, want %d %s with no access token, saying %q", what, status, body["error"], description, wantStatus, code, says)
	}
}

// expectNothingMinted fails t unless the database holds the access tokens it held before what.
func expectNothingMinted(t *testing.T, f *exchangeFixture, what string, before int) {
	t.Helper()
	if after := f.count(t, "oauth_access_tokens"); after != before {
		t.Errorf("%s left %d access tokens, want the %d before it: a refused trade mints nothing", what, after, before)
	}
}

// TestHeldMachineLoginTokenExchangeIsRefused: a dev box session's trade of its machine login's
// token, which names the person who approved the login, is refused membership_held while Dispatch
// holds that person, minting nothing; a login that ended gets its own refusal first; bob's is
// served. While the gate can keep no answer the held person's trade is server_error, never
// membership_held, and bob's is served; while Dispatch refuses the broker's own account every
// trade is server_error.
func TestHeldMachineLoginTokenExchangeIsRefused(t *testing.T) {
	d := newHoldDispatch(t, nil, person, servedBob)
	f := newExchangeFixture(t, d.configure)
	adaKey, bobKey, endedKey := newKey(t), newKey(t), newKey(t)
	adaToken := f.devBoxToken(t, adaKey, person)
	bobToken := f.devBoxToken(t, bobKey, servedBob)
	ended := f.newCredential(t)
	endedToken := f.devBoxTokenOf(t, endedKey, person, ended)
	trade := func(token, session string, key *ecdsa.PrivateKey) (int, map[string]any) {
		t.Helper()
		return f.exchange(t, exchangeForm(token, oauthwire.SubjectTokenTypeAccessToken, session), key)
	}
	d.hold(person)

	before := f.count(t, "oauth_access_tokens")
	status, body := trade(adaToken, "ada-devbox", adaKey)
	expectTrade(t, "ada's machine token trade while Dispatch holds her", status, body, http.StatusBadRequest, oauthwire.MembershipHeld, heldWords(person))
	expectNothingMinted(t, f, "ada's refused trade", before)
	if _, err := f.pool.Exec(context.Background(), `update launcher_credentials set revoked_at = now() where id = $1`, ended); err != nil {
		t.Fatalf("revoke ada's second machine login: %v", err)
	}
	status, body = trade(endedToken, "ada-ended-devbox", endedKey)
	expectTrade(t, "the trade of ada's ended machine login while Dispatch holds her", status, body, http.StatusBadRequest, "invalid_grant",
		"The machine login the subject token belongs to has ended.")
	status, body = trade(bobToken, "bob-devbox", bobKey)
	expectTrade(t, "bob's machine token trade", status, body, http.StatusOK, "", "")

	d.loseStore()
	status, body = trade(adaToken, "ada-devbox", adaKey)
	expectTrade(t, "ada's machine token trade while the broker can keep no answer", status, body, http.StatusInternalServerError, "server_error", unkeptWords(person))
	status, body = trade(bobToken, "bob-devbox", bobKey)
	expectTrade(t, "bob's machine token trade while the broker can keep no answer", status, body, http.StatusOK, "", "")

	d.refuseAccount()
	status, body = trade(adaToken, "ada-devbox", adaKey)
	expectTrade(t, "ada's machine token trade while Dispatch refuses the broker's account", status, body, http.StatusInternalServerError, "server_error", refusedAccount)
	status, body = trade(bobToken, "bob-devbox", bobKey)
	expectTrade(t, "bob's machine token trade while Dispatch refuses the broker's account", status, body, http.StatusInternalServerError, "server_error", refusedAccount)
}

// TestHeldPersonSignInTradeIsRefused: a person's sign-in trade is refused membership_held while
// Dispatch holds them, minting nothing; a box key the person revoked gets its own refusal first;
// bob's trade is served. While the gate can keep no answer the held person's trade is
// server_error and bob's is served; while Dispatch refuses the broker's own account every trade
// is server_error, though a sign-in naming no project asks Dispatch nothing else.
func TestHeldPersonSignInTradeIsRefused(t *testing.T) {
	d := newHoldDispatch(t, nil, person, servedBob)
	f := newSignInFixtureWith(t, d.configure)
	trade := func(login, session string, key *ecdsa.PrivateKey) (int, map[string]any) {
		t.Helper()
		return f.exchange(t, signInForm(
			f.personToken(t, login, deviceAudience, true, signInDomain),
			f.machineToken(t, signInMachine, machineAudience, true),
			session), key)
	}
	adaKey, bobKey, revokedKey := newKey(t), newKey(t), newKey(t)
	f.revoke(t, revokedKey)
	d.hold(person)

	before := f.count(t, "oauth_access_tokens")
	status, body := trade(person, "ada-box-1", adaKey)
	expectTrade(t, "ada's sign-in trade while Dispatch holds her", status, body, http.StatusBadRequest, oauthwire.MembershipHeld, heldWords(person))
	expectNothingMinted(t, f.exchangeFixture, "ada's refused sign-in trade", before)
	status, body = trade(person, "ada-box-2", revokedKey)
	expectTrade(t, "ada's sign-in trade of a box key she revoked, while Dispatch holds her", status, body, http.StatusBadRequest, "invalid_grant", "agent-secrets login")
	status, body = trade(servedBob, "bob-box-1", bobKey)
	expectTrade(t, "bob's sign-in trade", status, body, http.StatusOK, "", "")

	d.loseStore()
	status, body = trade(person, "ada-box-1", adaKey)
	expectTrade(t, "ada's sign-in trade while the broker can keep no answer", status, body, http.StatusInternalServerError, "server_error", unkeptWords(person))
	status, body = trade(servedBob, "bob-box-1", bobKey)
	expectTrade(t, "bob's sign-in trade while the broker can keep no answer", status, body, http.StatusOK, "", "")

	d.refuseAccount()
	status, body = trade(person, "ada-box-1", adaKey)
	expectTrade(t, "ada's sign-in trade while Dispatch refuses the broker's account", status, body, http.StatusInternalServerError, "server_error", refusedAccount)
	status, body = trade(servedBob, "bob-box-1", bobKey)
	expectTrade(t, "bob's sign-in trade while Dispatch refuses the broker's account", status, body, http.StatusInternalServerError, "server_error", refusedAccount)
}

// TestHeldExchangeEntryIsRefused: a pod whose exchange entry names a person Dispatch holds is
// refused membership_held, minting nothing, so a Legion pod acting for a held person gets no new
// token naming them; a pod whose entry names another person is served, and so is one whose entry
// names nobody, which asks nothing even while Dispatch refuses the broker's own account. While the
// gate can keep no answer the held person's pod is server_error.
func TestHeldExchangeEntryIsRefused(t *testing.T) {
	d := newHoldDispatch(t, map[string]projectaccesstest.Row{
		project:      {Visibility: "private", Owner: person},
		otherProject: {Visibility: "private", Owner: otherPerson},
	}, person, otherPerson)
	f := newExchangeFixture(t, d.configure)
	adaKey, otherKey, readKey := newKey(t), newKey(t), newKey(t)
	f.registers(t, otherAccount, "pod-uid-other", "other-session")
	adaPod := exchangeForm(f.podTokenOf(t, daemonAccount, "pod-uid-ada"), oauthwire.SubjectTokenTypeKubernetes, daemonSession)
	otherPod := exchangeForm(f.podTokenOf(t, otherAccount, "pod-uid-other"), oauthwire.SubjectTokenTypeKubernetes, "other-session")
	readPod := f.readPodForm(t)
	d.hold(person)

	before := f.count(t, "oauth_access_tokens")
	status, body := f.exchange(t, adaPod, adaKey)
	expectTrade(t, "the trade of a pod whose entry names ada, while Dispatch holds her", status, body, http.StatusBadRequest, oauthwire.MembershipHeld, heldWords(person))
	expectNothingMinted(t, f, "the refused pod's trade", before)
	status, body = f.exchange(t, otherPod, otherKey)
	expectTrade(t, "the trade of a pod whose entry names "+otherPerson, status, body, http.StatusOK, "", "")
	status, body = f.exchange(t, readPod, readKey)
	expectTrade(t, "the trade of a pod whose entry names no person", status, body, http.StatusOK, "", "")

	d.loseStore()
	status, body = f.exchange(t, adaPod, adaKey)
	expectTrade(t, "the trade of ada's pod while the broker can keep no answer", status, body, http.StatusInternalServerError, "server_error", unkeptWords(person))
	status, body = f.exchange(t, otherPod, otherKey)
	expectTrade(t, "the trade of "+otherPerson+"'s pod while the broker can keep no answer", status, body, http.StatusOK, "", "")

	d.refuseAccount()
	status, body = f.exchange(t, adaPod, adaKey)
	expectTrade(t, "the trade of ada's pod while Dispatch refuses the broker's account", status, body, http.StatusInternalServerError, "server_error", refusedAccount)
	status, body = f.exchange(t, readPod, readKey)
	expectTrade(t, "the trade of a pod whose entry names no person, while Dispatch refuses the broker's account", status, body, http.StatusOK, "", "")
}
