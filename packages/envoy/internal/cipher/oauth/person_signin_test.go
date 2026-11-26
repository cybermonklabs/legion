package oauth_test

import (
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/cipher/enroll"
	"github.com/sjawhar/envoy/internal/cipher/exchange"
	"github.com/sjawhar/envoy/internal/cipher/oauth"
	"github.com/sjawhar/envoy/internal/cipher/oauthstore"
	"github.com/sjawhar/envoy/internal/cipher/oauthwire"
	"github.com/sjawhar/envoy/internal/cipher/store"
	"github.com/sjawhar/envoy/internal/oidc"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
)

// The machine a person signs in on in these tests and the fleet its entry lists it for. The
// fleet's entry names no person, so nothing but the sign-in decides who the token acts for.
const (
	signInMachine = "example-fleet@example-project.iam.gserviceaccount.com"
	signInFleet   = "example-fleet"
	// strangerMachine is a Google account no entry lists: a box the deployment does not run.
	strangerMachine = "example-stranger@example-project.iam.gserviceaccount.com"
	// otherPersonDomain is any Workspace but signInDomain, and domainStranger a person of it.
	otherPersonDomain = "other.example"
	domainStranger    = "mallory@other.example"
	// machineAudience is what a machine's own Google identity token must carry, beside
	// deviceAudience, which a person's carries.
	machineAudience = "https://broker.example/machines"
)

// signInFixture is an exchange fixture with the Google half and the person half configured: a
// third issuer standing in for Google, the one fleet above listed, the device-flow client's
// audience, and a recorder in place of the broker's sign-in table, so what the exchange handed
// it is what this suite reads.
type signInFixture struct {
	*exchangeFixture
	google    *oidctest.Issuer
	googleKey *oidctest.Key
	// issuer is the `iss` the fixture's tokens carry and the exchange is configured to take: the
	// stand-in issuer's own URL unless a test names another spelling (newSignInFixtureAt).
	issuer string

	mu sync.Mutex
	// recorded is every sign-in the exchange asked to record, in order.
	recorded []oauth.PersonSignIn
	// revoked is the box key thumbprints the recorder answers ErrSignInRevoked for, as the
	// broker's own table answers a sign-in the person has revoked, moved those it answers
	// ErrSignInMachineChanged for, as it answers a sign-in's key presented from elsewhere,
	// otherAccount those it answers ErrSignInSubjectChanged for, as it answers a key another
	// account signed in with, and replaced the `sub`s it answers ErrSignInSubjectReplaced for, as
	// it answers an account that no longer holds the person's address.
	revoked      map[string]bool
	moved        map[string]bool
	otherAccount map[string]bool
	replaced     map[string]bool
}

// newSignInFixture starts the Google issuer and builds a provider that takes a person's sign-in
// on a machine of the one listed fleet.
func newSignInFixture(t *testing.T) *signInFixture {
	t.Helper()
	return newSignInFixtureAt(t, "")
}

// newSignInFixtureAt is newSignInFixture with the exchange taking, and the fixture's tokens
// carrying, issuer as their `iss` in place of the stand-in issuer's URL (empty keeps the URL); the
// tokens are still signed by the stand-in's key, which is all the exchange checks a signature by.
func newSignInFixtureAt(t *testing.T, issuer string) *signInFixture {
	t.Helper()
	return newSignInFixtureAtWith(t, issuer, func(*oauth.Config) {})
}

// newSignInFixtureWith is newSignInFixture with adjust applied to the configuration last, so a
// test can give the exchange a project-access check or entries of its own.
func newSignInFixtureWith(t *testing.T, adjust func(*oauth.Config)) *signInFixture {
	t.Helper()
	return newSignInFixtureAtWith(t, "", adjust)
}

// newSignInFixtureAtWith is the fixture both of those build: its tokens carrying issuer
// (newSignInFixtureAt), and adjust applied to the configuration last (newSignInFixtureWith).
func newSignInFixtureAtWith(t *testing.T, issuer string, adjust func(*oauth.Config)) *signInFixture {
	t.Helper()
	google := oidctest.New(t)
	googleKey := google.PublishKey(t, "google-key")
	googleKeys, err := oidc.NewKeys(context.Background(), google.URL())
	if err != nil {
		t.Fatalf("Google keys: %v", err)
	}
	if issuer == "" {
		issuer = google.URL()
	}
	f := &signInFixture{google: google, googleKey: googleKey, issuer: issuer, revoked: map[string]bool{}, moved: map[string]bool{},
		otherAccount: map[string]bool{}, replaced: map[string]bool{}}
	f.exchangeFixture = newExchangeFixture(t, func(c *oauth.Config) {
		c.Exchange.Policy = newPolicy(t, nil, nil, exchange.GoogleEntry{Account: signInMachine, Service: signInFleet})
		c.Exchange.Google = oauth.IssuerConfig{Issuer: issuer, Audience: machineAudience, Keys: googleKeys}
		c.Exchange.PersonAudience = deviceAudience
		// The credential a trade mints, written in the exchange's own transaction as
		// cmd/cipherd writes it, so the access token's row may name it.
		c.Exchange.MachineCredentials = recordCredential
		c.Exchange.PersonSignIns = f.record
		adjust(c)
	})
	return f
}

// record is oauth.ExchangeConfig.PersonSignIns over the fixture: it keeps what the exchange
// handed it and refuses a box key the test has revoked.
func (f *signInFixture) record(_ context.Context, in oauth.PersonSignIn) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case f.replaced[in.Subject]:
		return fmt.Errorf("%w: another account holds the address", oauth.ErrSignInSubjectReplaced)
	case f.revoked[in.Thumbprint]:
		return fmt.Errorf("%w: the person revoked it", oauth.ErrSignInRevoked)
	case f.moved[in.Thumbprint]:
		return fmt.Errorf("%w: it was made on another machine", oauth.ErrSignInMachineChanged)
	case f.otherAccount[in.Thumbprint]:
		return fmt.Errorf("%w: another account made it", oauth.ErrSignInSubjectChanged)
	}
	f.recorded = append(f.recorded, in)
	return nil
}

// signIns is every sign-in the exchange asked to record so far.
func (f *signInFixture) signIns() []oauth.PersonSignIn {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]oauth.PersonSignIn(nil), f.recorded...)
}

// revoke makes the recorder refuse key's thumbprint from now on, as the broker's table does
// once the person has revoked that sign-in.
func (f *signInFixture) revoke(t *testing.T, key *ecdsa.PrivateKey) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	f.revoked[thumbprint(t, key)] = true
}

// move makes the recorder refuse key's thumbprint as a sign-in made on another machine, as the
// broker's table does when the box key of one machine's sign-in is presented from another.
func (f *signInFixture) move(t *testing.T, key *ecdsa.PrivateKey) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	f.moved[thumbprint(t, key)] = true
}

// personToken is a person's own Google identity token as the device grant answers one: the
// account Google gives it, the audience of the device-flow client, whether Google holds the
// address verified, and the Workspace domain in `hd`.
func (f *signInFixture) personToken(t *testing.T, login, audience string, verified any, domain string) string {
	t.Helper()
	now := time.Now()
	claims := map[string]any{
		"iss":            f.issuer,
		"sub":            "109" + login,
		"aud":            audience,
		"iat":            now.Add(-time.Minute).Unix(),
		"exp":            now.Add(30 * time.Minute).Unix(),
		"email":          login,
		"email_verified": verified,
	}
	if domain != "" {
		claims["hd"] = domain
	}
	return f.google.Mint(t, f.googleKey, claims)
}

// machineToken is the machine's own Google identity token, the one its metadata server answers
// and the one it presents as the actor token while the person signed in on it trades theirs.
func (f *signInFixture) machineToken(t *testing.T, account, audience string, verified any) string {
	t.Helper()
	now := time.Now()
	return f.google.Mint(t, f.googleKey, map[string]any{
		"iss":            f.issuer,
		"sub":            "110" + account,
		"aud":            audience,
		"iat":            now.Add(-time.Minute).Unix(),
		"exp":            now.Add(30 * time.Minute).Unix(),
		"email":          account,
		"email_verified": verified,
	})
}

// signInForm is a complete sign-in trade: the person's token as the subject and the machine's as
// the actor, under the two types the exchange client is registered for.
func signInForm(personToken, machineToken, session string) url.Values {
	form := exchangeForm(personToken, oauthwire.SubjectTokenTypePerson, session)
	form.Set("actor_token", machineToken)
	form.Set("actor_token_type", oauthwire.ActorTokenTypeMachine)
	return form
}

// TestAPersonSignedInOnAMachineExchangesForATokenNamingBoth is the whole of the grant at this
// layer: the person's own Google identity token with the machine's as the actor is answered an
// access token bound to the box key the proof named, which introspects as the person, the
// machine and the session asked for, held to no one project; and the sign-in the exchange
// recorded names that person, that box key and that machine.
func TestAPersonSignedInOnAMachineExchangesForATokenNamingBoth(t *testing.T) {
	f := newSignInFixture(t)
	key := newKey(t)
	token := f.exchanged(t, signInForm(
		f.personToken(t, person, deviceAudience, true, signInDomain),
		f.machineToken(t, signInMachine, machineAudience, true),
		"ada-box-1"), key)

	answer, err := f.provider.Introspect(context.Background(), token)
	if err != nil {
		t.Fatalf("Introspect: %v", err)
	}
	for _, field := range []struct {
		name      string
		got, want any
		proves    string
	}{
		{"active", answer.Active, true, "the token the trade minted is live"},
		{"session", answer.Session, "ada-box-1", "the session the person asked to write under"},
		{"person", answer.Person, person, "the person whose own token was the subject"},
		{"machine", answer.Machine, signInMachine, "the machine whose token acted for them"},
		{"service", answer.Service, "", "no service: a sign-in's token names a person"},
		{"project_scope", answer.ProjectScope, oauthwire.ProjectScopePerson, "that the token acts as its person"},
		{"projects", len(answer.Projects), 0, "no project, which the trade asked for none of"},
		{"jkt", answer.Thumbprint, thumbprint(t, key), "the box key the request's proof presented"},
	} {
		if field.got != field.want {
			t.Errorf("introspection answered %s %v, want %v: it must name %s", field.name, field.got, field.want, field.proves)
		}
	}

	want := []oauth.PersonSignIn{{Person: person, Thumbprint: thumbprint(t, key), Machine: signInMachine, Issuer: f.google.URL(), Subject: "109" + person}}
	if got := f.signIns(); len(got) != 1 || got[0] != want[0] {
		t.Fatalf("the exchange recorded %+v, want %+v: the first trade of a person and a box key is the sign-in, made by the account the person's token names", got, want)
	}
}

// A person's token whose `iss` is Google's scheme-less spelling, accounts.google.com, which Google
// documents beside https://accounts.google.com and a broker configured with that spelling takes,
// records its sign-in under the one spelling Dispatch names a replaced account by
// (oidc.CanonicalIssuer): a revoke naming https://accounts.google.com must find it, and a sign-in
// recorded under the raw spelling would be found by no revoke and refused by no record of a
// replaced account.
func TestASignInUnderGooglesSchemelessIssuerIsRecordedUnderTheCanonicalOne(t *testing.T) {
	f := newSignInFixtureAt(t, "accounts.google.com")
	key := newKey(t)
	f.exchanged(t, signInForm(
		f.personToken(t, person, deviceAudience, true, signInDomain),
		f.machineToken(t, signInMachine, machineAudience, true),
		"ada-box-1"), key)
	want := oauth.PersonSignIn{Person: person, Thumbprint: thumbprint(t, key), Machine: signInMachine, Issuer: oidc.GoogleIssuer, Subject: "109" + person}
	if got := f.signIns(); len(got) != 1 || got[0] != want {
		t.Fatalf("a trade of a token issued by accounts.google.com recorded %+v, want %+v: the issuer as oidc.CanonicalIssuer spells it", got, want)
	}
}

// TestEachTradeOfOneSignInRecordsTheSameKeyAndASecondBoxIsItsOwn: the second trade of one box is
// the same sign-in used again, and the person signing in on a second box records a second one,
// so revoking either leaves the other to be decided on its own row.
func TestEachTradeOfOneSignInRecordsTheSameKeyAndASecondBoxIsItsOwn(t *testing.T) {
	f := newSignInFixture(t)
	first, second := newKey(t), newKey(t)
	for _, c := range []struct {
		key     *ecdsa.PrivateKey
		session string
	}{{first, "ada-box-1"}, {first, "ada-box-1"}, {second, "ada-box-2"}} {
		f.exchanged(t, signInForm(
			f.personToken(t, person, deviceAudience, true, signInDomain),
			f.machineToken(t, signInMachine, machineAudience, true),
			c.session), c.key)
	}
	got := f.signIns()
	want := []oauth.PersonSignIn{
		{Person: person, Thumbprint: thumbprint(t, first), Machine: signInMachine, Issuer: f.google.URL(), Subject: "109" + person},
		{Person: person, Thumbprint: thumbprint(t, first), Machine: signInMachine, Issuer: f.google.URL(), Subject: "109" + person},
		{Person: person, Thumbprint: thumbprint(t, second), Machine: signInMachine, Issuer: f.google.URL(), Subject: "109" + person},
	}
	if len(got) != len(want) {
		t.Fatalf("the exchange recorded %d sign-ins, want %d: every trade names the key it proved", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("sign-in %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// TestARevokedSignInIsRefusedAndNamesTheSignInCommand: once the person has revoked the box key,
// the very next trade of it is invalid_grant and the refusal says what to run, while the
// person's other box keeps trading.
func TestARevokedSignInIsRefusedAndNamesTheSignInCommand(t *testing.T) {
	f := newSignInFixture(t)
	revoked, kept := newKey(t), newKey(t)
	trade := func(key *ecdsa.PrivateKey, session string) (int, map[string]any) {
		return f.exchange(t, signInForm(
			f.personToken(t, person, deviceAudience, true, signInDomain),
			f.machineToken(t, signInMachine, machineAudience, true),
			session), key)
	}
	f.exchanged(t, signInForm(
		f.personToken(t, person, deviceAudience, true, signInDomain),
		f.machineToken(t, signInMachine, machineAudience, true),
		"ada-box-1"), revoked)
	f.revoke(t, revoked)

	status, body := trade(revoked, "ada-box-1")
	description, _ := body["error_description"].(string)
	if status != http.StatusBadRequest || body["error"] != "invalid_grant" || body["access_token"] != nil {
		t.Fatalf("the revoked box's trade = %d %v, want 400 invalid_grant and no access token", status, body)
	}
	for _, names := range []string{"agent-secrets login", person} {
		if !strings.Contains(description, names) {
			t.Errorf("the refusal reads %q, which does not name %q", description, names)
		}
	}
	if status, body := trade(kept, "ada-box-2"); status != http.StatusOK {
		t.Fatalf("the person's other box = %d %v, want 200: revoking one sign-in leaves the others working", status, body)
	}
}

// TestASignInsKeyPresentedFromAnotherMachineIsRefusedAndNamesTheSignInCommand: a sign-in
// belongs to the machine it was made on, because the credential it holds is keyed by the
// person, the machine and the key together. A trade of that key from another machine is
// invalid_grant naming the command that makes a key of its own — not a 500, and not a quiet
// move that would collide on every trade after it.
func TestASignInsKeyPresentedFromAnotherMachineIsRefusedAndNamesTheSignInCommand(t *testing.T) {
	f := newSignInFixture(t)
	key := newKey(t)
	f.exchanged(t, signInForm(
		f.personToken(t, person, deviceAudience, true, signInDomain),
		f.machineToken(t, signInMachine, machineAudience, true),
		"ada-box-1"), key)
	f.move(t, key)

	status, body := f.exchange(t, signInForm(
		f.personToken(t, person, deviceAudience, true, signInDomain),
		f.machineToken(t, signInMachine, machineAudience, true),
		"ada-box-1"), key)
	if status != http.StatusBadRequest || body["error"] != "invalid_grant" || body["access_token"] != nil {
		t.Fatalf("a sign-in's key from another machine = %d %v, want 400 invalid_grant and no access token", status, body)
	}
	description, _ := body["error_description"].(string)
	for _, names := range []string{"agent-secrets login", "another machine"} {
		if !strings.Contains(description, names) {
			t.Errorf("the refusal reads %q, which does not name %q", description, names)
		}
	}
}

// TestATradeByAnotherOrAReplacedAccountIsRefusedSayingWhich: the two refusals the account a
// sign-in records makes are invalid_grant, each with its own words. A box key another account
// signed in with names the sign-in command, which makes a key of its own; an account that no
// longer holds the person's address says so and names the address, since signing in again as
// that account fixes nothing. Neither mints anything.
func TestATradeByAnotherOrAReplacedAccountIsRefusedSayingWhich(t *testing.T) {
	f := newSignInFixture(t)
	key := newKey(t)
	trade := func() (int, map[string]any) {
		return f.exchange(t, signInForm(
			f.personToken(t, person, deviceAudience, true, signInDomain),
			f.machineToken(t, signInMachine, machineAudience, true),
			"ada-box-1"), key)
	}
	f.mu.Lock()
	f.otherAccount[thumbprint(t, key)] = true
	f.mu.Unlock()
	status, body := trade()
	changed, _ := body["error_description"].(string)
	if status != http.StatusBadRequest || body["error"] != "invalid_grant" || body["access_token"] != nil {
		t.Fatalf("a box key another account signed in with = %d %v, want 400 invalid_grant and no access token", status, body)
	}
	for _, names := range []string{"agent-secrets login", "another Google account"} {
		if !strings.Contains(changed, names) {
			t.Errorf("the refusal reads %q, which does not name %q", changed, names)
		}
	}

	f.mu.Lock()
	f.replaced["109"+person] = true
	f.mu.Unlock()
	status, body = trade()
	replaced, _ := body["error_description"].(string)
	if status != http.StatusBadRequest || body["error"] != "invalid_grant" || body["access_token"] != nil {
		t.Fatalf("a replaced account's trade = %d %v, want 400 invalid_grant and no access token", status, body)
	}
	for _, names := range []string{person, "no longer holds", "another account holds that address now"} {
		if !strings.Contains(replaced, names) {
			t.Errorf("the refusal reads %q, which does not name %q", replaced, names)
		}
	}
	if strings.Contains(replaced, "agent-secrets login") {
		t.Errorf("the refusal reads %q, which sends the person to sign in again, and that account's sign-in fixes nothing", replaced)
	}
	if got := f.signIns(); len(got) != 0 {
		t.Errorf("the refused trades recorded %+v, want nothing", got)
	}
}

// TestASignInTradeIsRefusedWhatItCannotDecide walks the refusals this layer owns, each the trade
// that works but for one thing: another domain, an unverified address, either token minted for
// the other's audience, a machine no entry lists, and no actor token at all. Each gives its own
// message, so the person reading it knows which of the five to fix.
func TestASignInTradeIsRefusedWhatItCannotDecide(t *testing.T) {
	f := newSignInFixture(t)
	key := newKey(t)
	said := map[string]string{}
	for _, c := range []struct {
		name   string
		form   url.Values
		code   string
		says   string
		proves string
	}{
		{
			name: "a person of another Workspace domain",
			form: signInForm(f.personToken(t, domainStranger, deviceAudience, true, otherPersonDomain),
				f.machineToken(t, signInMachine, machineAudience, true), "ada-box"),
			code: "invalid_request", says: domainStranger,
			proves: "the domain comes from the hd claim, which BROKER_SIGNIN_DOMAIN must equal",
		},
		{
			name: "a person's address Google has not verified",
			form: signInForm(f.personToken(t, person, deviceAudience, false, signInDomain),
				f.machineToken(t, signInMachine, machineAudience, true), "ada-box"),
			code: "invalid_request", says: "email_verified",
			proves: "the broker checks email_verified itself, since a token for an address nobody verified proves no person",
		},
		{
			name: "a person's token minted for the machines' audience",
			form: signInForm(f.personToken(t, person, machineAudience, true, signInDomain),
				f.machineToken(t, signInMachine, machineAudience, true), "ada-box"),
			code: "invalid_request", says: deviceAudience,
			proves: "a person's token is spendable only at the device-flow client's audience",
		},
		{
			name: "a machine's token minted for the device client's audience",
			form: signInForm(f.personToken(t, person, deviceAudience, true, signInDomain),
				f.machineToken(t, signInMachine, deviceAudience, true), "ada-box"),
			code: "invalid_request", says: machineAudience,
			proves: "the actor token is held to the audience a machine's own token carries",
		},
		{
			name: "an actor token of an account no entry lists",
			form: signInForm(f.personToken(t, person, deviceAudience, true, signInDomain),
				f.machineToken(t, strangerMachine, machineAudience, true), "ada-box"),
			code: "invalid_request", says: strangerMachine,
			proves: "a person signs in only from a box the deployment lists",
		},
		{
			name: "no actor token at all",
			form: exchangeForm(f.personToken(t, person, deviceAudience, true, signInDomain),
				oauthwire.SubjectTokenTypePerson, "ada-box"),
			code: "invalid_request", says: "actor",
			proves: "a person's token alone says nothing about which box is asking",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			status, body := f.exchange(t, c.form, key)
			description, _ := body["error_description"].(string)
			if status != http.StatusBadRequest || body["error"] != c.code || body["access_token"] != nil {
				t.Fatalf("that trade = %d %v, want 400 %s and no access token: %s", status, body, c.code, c.proves)
			}
			if !strings.Contains(description, c.says) {
				t.Fatalf("the refusal reads %q, which does not carry %q: %s", description, c.says, c.proves)
			}
			said[c.name] = description
		})
	}
	for name, text := range said {
		for other, otherText := range said {
			if name != other && text == otherText {
				t.Errorf("the refusals of %s and of %s read alike: %q", name, other, text)
			}
		}
	}
	if got := f.signIns(); len(got) != 0 {
		t.Errorf("a refused trade recorded %+v, want nothing: a sign-in is written only for a trade the policy answered", got)
	}
}

// TestABrokerWithNoDeviceAudienceRegistersNeitherSignInType: with
// BROKER_SIGNIN_DEVICE_AUDIENCE unset, neither the person's subject type nor the machine's actor
// type is registered, so the library refuses both by name before any handler of the broker's
// runs and nothing of the sign-in path is reachable.
func TestABrokerWithNoDeviceAudienceRegistersNeitherSignInType(t *testing.T) {
	f := newSignInFixture(t)
	plain := newExchangeFixtureWithoutPerson(t, f)
	key := newKey(t)
	status, body := plain.exchange(t, signInForm(
		f.personToken(t, person, deviceAudience, true, signInDomain),
		f.machineToken(t, signInMachine, machineAudience, true),
		"ada-box"), key)
	description, _ := body["error_description"].(string)
	if status != http.StatusBadRequest || body["error"] != "invalid_request" {
		t.Fatalf("the trade = %d %v, want 400 invalid_request: a broker told about no device audience takes no person's token", status, body)
	}
	if !strings.Contains(description, oauthwire.SubjectTokenTypePerson) {
		t.Errorf("the refusal reads %q, which does not name the type it refuses", description)
	}
}

// newExchangeFixtureWithoutPerson is a second provider on a database of its own, listing the same
// fleet and the same Google issuer as f and no device-flow audience at all.
func newExchangeFixtureWithoutPerson(t *testing.T, f *signInFixture) *exchangeFixture {
	t.Helper()
	googleKeys, err := oidc.NewKeys(context.Background(), f.google.URL())
	if err != nil {
		t.Fatalf("Google keys: %v", err)
	}
	return newExchangeFixture(t, func(c *oauth.Config) {
		c.Exchange.Policy = newPolicy(t, nil, nil, exchange.GoogleEntry{Account: signInMachine, Service: signInFleet})
		c.Exchange.Google = oauth.IssuerConfig{Issuer: f.google.URL(), Audience: machineAudience, Keys: googleKeys}
		c.Exchange.MachineCredentials = recordCredential
	})
}

// recordCredential is oauth.ExchangeConfig.MachineCredentials over the fixture's own database:
// the launcher credential a trade mints, written in the exchange's transaction exactly as
// cmd/cipherd writes it (its machineCredentials: enroll.Service.IssueTokenLoginTx, with a key
// another live credential holds answered oauth.ErrMachineKeyTaken, and a key a live read token is
// bound to answered oauth.ErrMachineKeyBindsReadToken), so the token row that names it has a row
// to name and the trade meets the same refusals. IssueTokenLoginTx reads nothing of its service's
// fields.
func recordCredential(ctx context.Context, cred oauth.MachineCredential) (string, error) {
	tx, ok := oauthstore.TxFrom(ctx)
	if !ok {
		return "", errors.New("the credential is minted in the exchange's transaction")
	}
	id, err := (&enroll.Service{}).IssueTokenLoginTx(ctx, tx, enroll.TokenLogin{
		Subject: cred.Subject, Service: cred.Service, Host: cred.Host, Operator: cred.Operator,
		TokenExpires: cred.Expires, Thumbprint: cred.Thumbprint, JWK: cred.JWK, Expires: cred.Expires,
	})
	if enroll.KeyHeld(err) {
		return "", fmt.Errorf("%w: %v", oauth.ErrMachineKeyTaken, err)
	}
	if errors.Is(err, enroll.ErrKeyBindsReadToken) {
		return "", fmt.Errorf("%w: %v", oauth.ErrMachineKeyBindsReadToken, err)
	}
	if err != nil {
		return "", err
	}
	return id.String(), nil
}

// realSignIns is oauth.ExchangeConfig.PersonSignIns over the broker's own person_signins table, as
// cmd/cipherd wires it where BROKER_PROJECT_ACCESS_URL is set (enroll.PersonSignIns(true)): the
// sign-in recorded in the exchange's transaction, which holds the sign-in's row locked until the
// trade commits.
var realSignIns = enroll.PersonSignIns(true)

// enrollments is the enrollment service over f's database, which ends a sign-in and a credential
// as the broker's routes do.
func (f *fixture) enrollments() *enroll.Service {
	return &enroll.Service{Store: &store.Store{Pool: f.pool}}
}

// revokeSignIn revokes the sign-in of person on box as the sign-ins route does
// (enroll.Service.RevokePersonSignIn): its row, written here where the trade's own recorder stood
// in for it, its tokens bound to the box key, and the credential it holds.
func (f *signInFixture) revokeSignIn(t *testing.T, box *ecdsa.PrivateKey) {
	t.Helper()
	id := recordSignIn(t, f, person, thumbprint(t, box), signInMachine)
	if _, err := f.enrollments().RevokePersonSignIn(context.Background(), id, person, person); err != nil {
		t.Fatalf("RevokePersonSignIn: %v", err)
	}
}

// signedProof is an RFC 9449 proof of key for htm on htu, as a DPoP-Holder header carries one.
func signedProof(t *testing.T, key *ecdsa.PrivateKey, htm, htu string) string {
	t.Helper()
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.ES256, Key: key},
		(&jose.SignerOptions{EmbedJWK: true}).WithType("dpop+jwt"))
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(map[string]any{"jti": uuid.NewString(), "htm": htm, "htu": htu, "iat": time.Now().Unix()})
	if err != nil {
		t.Fatal(err)
	}
	signed, err := signer.Sign(payload)
	if err != nil {
		t.Fatal(err)
	}
	compact, err := signed.CompactSerialize()
	if err != nil {
		t.Fatal(err)
	}
	return compact
}

// TestAReadTradeBindsItsTokenToTheHoldersKey: a person's read trade proves its sign-in with the
// box key's DPoP proof and sends a DPoP-Holder proof by a key made for the token's holder; the
// token it mints is the holder's, in its introspection's jkt and in its stored row, names the
// person and the sign-in's credential. Each of these is refused and mints nothing, each with its
// own refusal: no DPoP-Holder; the DPoP proof signed by the holder key and no proof by the box
// key, which names a sign-in of another key, one that holds no launcher credential; a holder
// proof signed by the box key; a holder proof of another method, and of another URL; and the
// holder proof already spent, replayed. A machine login's read trade proved with machine.json's
// key, which holds a launcher credential, binds a holder key it proves, and without one is
// refused; a DPoP-Holder on a trade that asks for no scope is refused. Once the sign-in is
// revoked, its read token introspects not live.
func TestAReadTradeBindsItsTokenToTheHoldersKey(t *testing.T) {
	f := newNarrowingFixture(t, func(*oauth.Config) {})
	ctx := context.Background()
	tokenURL := f.url + "/token"
	box, holder := newKey(t), newKey(t)
	status, body := f.exchange(t, f.personTrade(t, narrowedBoxSession), box)
	accessTokenOf(t, status, body)
	credential, _ := body[oauthwire.ExtraCredentialID].(string)
	readTrade := func(headers http.Header) (int, map[string]any) {
		form := f.personTrade(t, narrowedBoxSession)
		form.Set("scope", oauthwire.ScopeRead)
		return f.postWithHeaders(t, form, headers)
	}
	spent := dpopProof(t, holder, tokenURL)
	status, body = readTrade(http.Header{"DPoP": {dpopProof(t, box, tokenURL)}, oauthwire.HeaderDPoPHolder: {spent}})
	token := accessTokenOf(t, status, body)
	answer := f.introspected(t, token)
	if answer.Thumbprint != thumbprint(t, holder) || answer.Person != person {
		t.Errorf("the read token introspects with jkt %q for %q, want the holder's key %q and %s", answer.Thumbprint, answer.Person, thumbprint(t, holder), person)
	}
	var stored, session, named string
	if err := f.pool.QueryRow(ctx, `select jwk_thumbprint, request #>> '{session,jwk_thumbprint}', coalesce(credential_id::text, '')
		from oauth_narrowed_access_tokens`).Scan(&stored, &session, &named); err != nil {
		t.Fatalf("read the read token's row: %v", err)
	}
	if stored != thumbprint(t, holder) || session != stored || named != credential {
		t.Errorf("the read token's row is bound to %q (its session to %q) and names the credential %q, want the holder's key %q and the sign-in's credential %q",
			stored, session, named, thumbprint(t, holder), credential)
	}

	for _, c := range []struct {
		name    string
		headers http.Header
		// code and says are the refusal: its error code, and what its description holds.
		code, says string
	}{
		{"with no DPoP-Holder", http.Header{"DPoP": {dpopProof(t, box, tokenURL)}},
			"invalid_request", "A person's read trade sends a DPoP-Holder proof"},
		{"with its DPoP proof signed by the holder key and no proof by the box key",
			http.Header{"DPoP": {dpopProof(t, holder, tokenURL)}, oauthwire.HeaderDPoPHolder: {dpopProof(t, newKey(t), tokenURL)}},
			"invalid_grant", "holds no launcher credential yet"},
		{"with a holder proof signed by the box key", http.Header{"DPoP": {dpopProof(t, box, tokenURL)}, oauthwire.HeaderDPoPHolder: {dpopProof(t, box, tokenURL)}},
			"invalid_request", "The DPoP-Holder proof is signed with the key the DPoP proof is signed with"},
		{"with a holder proof of another method", http.Header{"DPoP": {dpopProof(t, box, tokenURL)}, oauthwire.HeaderDPoPHolder: {signedProof(t, newKey(t), http.MethodGet, tokenURL)}},
			"invalid_request", "The DPoP-Holder proof was refused"},
		{"with a holder proof of another URL", http.Header{"DPoP": {dpopProof(t, box, tokenURL)}, oauthwire.HeaderDPoPHolder: {signedProof(t, newKey(t), http.MethodPost, f.url+"/other")}},
			"invalid_request", "The DPoP-Holder proof was refused"},
		{"with a holder proof replayed", http.Header{"DPoP": {dpopProof(t, box, tokenURL)}, oauthwire.HeaderDPoPHolder: {spent}},
			"invalid_request", "The DPoP-Holder proof was refused"},
	} {
		t.Run("a person's read trade "+c.name, func(t *testing.T) {
			before := f.tokenRows(t)
			status, body := readTrade(c.headers)
			refusedSaying(t, status, body, http.StatusBadRequest, c.code, c.says)
			if after := f.tokenRows(t); after != before {
				t.Errorf("the refused trade left %d access tokens, want the %d there were", after, before)
			}
		})
	}

	t.Run("a machine login's read trade binds the holder's key, and without one is refused", func(t *testing.T) {
		machine := newKey(t)
		subject := f.devBoxTokenOf(t, machine, person, f.credentialOn(t, machine))
		trade := narrowingTrade{form: exchangeForm(subject, oauthwire.SubjectTokenTypeAccessToken, narrowedDevBoxSession), key: machine}
		before := f.tokenRows(t)
		status, body := f.send(t, trade, oauthwire.ScopeRead, "")
		refusedSaying(t, status, body, http.StatusBadRequest, "invalid_request", "signs launcher proofs")
		if after := f.tokenRows(t); after != before {
			t.Errorf("the refused trade left %d access tokens, want the %d there were", after, before)
		}
		trade.holder = newKey(t)
		status, body = f.send(t, trade, oauthwire.ScopeRead, "")
		if answer := f.introspected(t, accessTokenOf(t, status, body)); answer.Thumbprint != thumbprint(t, trade.holder) {
			t.Errorf("the machine login's read token is bound to %q, want the holder's key %q", answer.Thumbprint, thumbprint(t, trade.holder))
		}
	})

	t.Run("a DPoP-Holder on a trade that asks for no scope is refused", func(t *testing.T) {
		before := f.tokenRows(t)
		status, body := f.postWithHeaders(t, f.personTrade(t, narrowedBoxSession),
			http.Header{"DPoP": {dpopProof(t, box, tokenURL)}, oauthwire.HeaderDPoPHolder: {dpopProof(t, newKey(t), tokenURL)}})
		refusedSaying(t, status, body, http.StatusBadRequest, "invalid_request", oauthwire.HeaderDPoPHolder)
		if after := f.tokenRows(t); after != before {
			t.Errorf("the refused trade left %d access tokens, want the %d there were", after, before)
		}
	})

	f.revokeSignIn(t, box)
	if answer := f.introspected(t, token); answer.Active {
		t.Errorf("the read token introspects live after its sign-in was revoked: %+v", answer)
	}
}

// postTokenWith posts form to the token endpoint at tokenURL with headers, for a goroutine to run:
// it fails nothing itself and answers what came back.
func postTokenWith(tokenURL string, form url.Values, headers http.Header) tokenAnswer {
	req, err := http.NewRequest(http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return tokenAnswer{err: err}
	}
	req.Header = headers.Clone()
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return tokenAnswer{err: err}
	}
	defer resp.Body.Close()
	answer := tokenAnswer{status: resp.StatusCode}
	answer.err = json.NewDecoder(resp.Body).Decode(&answer.body)
	return answer
}

// blockedPID is the backend running a statement matching like that waits on a lock blocker
// holds, which waitForBlocked has already seen. It reads the wait through pg_blocking_pids, the
// queue pg_locks holds, and not through wait_event_type, which reads nothing each time the
// waiting backend is woken (packages/envoy/lockwaittest, "Why pg_locks").
func blockedPID(t *testing.T, f *fixture, blocker uint32, like string) uint32 {
	t.Helper()
	var pid int32
	if err := f.pool.QueryRow(context.Background(), `select pid from pg_stat_activity
		where $1 = any(pg_blocking_pids(pid)) and query like $2 limit 1`, int32(blocker), like).Scan(&pid); err != nil {
		t.Fatalf("find the backend waiting on %d: %v", blocker, err)
	}
	return uint32(pid)
}

// TestANarrowedTokenEndsWithWhatItWasTradedFrom: a narrowed token never outlives what it was
// traded from. A machine login's narrowed token introspects not live once its subject token is
// revoked (POST /v1/oauth/revoke), once its launcher credential is ended (an enroll revoke), once
// its subject expires, and once its credential is ended by the one statement a broker build
// before the narrowed table runs (oauthstore.EndCredential alone, as an older replica does). A
// person's narrowed token introspects not live once the sign-in is revoked, and so does one whose
// trade commits while the revoke waits on the sign-in's row lock, once the revoke commits.
func TestANarrowedTokenEndsWithWhatItWasTradedFrom(t *testing.T) {
	f := newNarrowingFixture(t, func(*oauth.Config) {})
	ctx := context.Background()
	machineRead := func(t *testing.T) (token, credential, subject string) {
		t.Helper()
		machine := newKey(t)
		credential = f.newCredential(t)
		subject = f.devBoxTokenOf(t, machine, person, credential)
		status, body := f.send(t, narrowingTrade{form: exchangeForm(subject, oauthwire.SubjectTokenTypeAccessToken, narrowedDevBoxSession), key: machine, holder: newKey(t)},
			oauthwire.ScopeRead, "")
		token = accessTokenOf(t, status, body)
		if !f.introspected(t, token).Active {
			t.Fatal("the narrowed token is not live as it is minted")
		}
		return token, credential, subject
	}
	inTx := func(t *testing.T, write func(pgx.Tx) error) {
		t.Helper()
		tx, err := f.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		if err := write(tx); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}
	notLive := func(t *testing.T, token, after string) {
		t.Helper()
		if answer := f.introspected(t, token); answer.Active {
			t.Fatalf("the narrowed token introspects live after %s: %+v", after, answer)
		}
	}
	t.Run("a machine login's, once its subject token is revoked", func(t *testing.T) {
		token, _, subject := machineRead(t)
		req := httptest.NewRequest(http.MethodPost, "/revoke", strings.NewReader(url.Values{
			"token": {subject}, "token_type_hint": {"access_token"}, "client_id": {oauthwire.ClientID},
		}.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		recorder := httptest.NewRecorder()
		f.provider.Revoke(recorder, req)
		if recorder.Code != http.StatusOK {
			t.Fatalf("revoking the machine login's token = %d %s", recorder.Code, recorder.Body)
		}
		notLive(t, token, "its subject token was revoked")
	})
	t.Run("a machine login's, once its credential is ended", func(t *testing.T) {
		token, credential, _ := machineRead(t)
		inTx(t, func(tx pgx.Tx) error { return enroll.RevokeReusedCredential(ctx, tx, credential) })
		notLive(t, token, "its credential was revoked")
	})
	t.Run("a machine login's, once its subject expires", func(t *testing.T) {
		token, credential, _ := machineRead(t)
		if _, err := f.pool.Exec(ctx, `update oauth_access_tokens set expires_at = now() - interval '1 second' where credential_id = $1`, credential); err != nil {
			t.Fatal(err)
		}
		notLive(t, token, "its subject expired")
	})
	t.Run("a machine login's, once an older build ends its credential", func(t *testing.T) {
		token, credential, _ := machineRead(t)
		inTx(t, func(tx pgx.Tx) error { _, err := oauthstore.EndCredential(ctx, tx, credential); return err })
		notLive(t, token, "the statement an older replica runs ended its credential's tokens")
	})
	t.Run("a person's, once the sign-in is revoked", func(t *testing.T) {
		tr := narrowingRows["person-sign-in"].trade(t, f)
		status, body := f.send(t, tr, oauthwire.ScopeRead, "")
		token := accessTokenOf(t, status, body)
		f.revokeSignIn(t, tr.key)
		notLive(t, token, "its sign-in was revoked")
	})
	t.Run("a person's, whose trade commits while the revoke waits on the sign-in's row", func(t *testing.T) {
		g := newNarrowingFixture(t, func(c *oauth.Config) { c.Exchange.PersonSignIns = realSignIns })
		box := newKey(t)
		g.exchanged(t, g.personTrade(t, narrowedBoxSession), box)
		var signIn string
		if err := g.pool.QueryRow(ctx, `select id::text from person_signins where thumbprint = $1`, thumbprint(t, box)).Scan(&signIn); err != nil {
			t.Fatalf("read the sign-in's row: %v", err)
		}
		holder, err := g.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer holder.Rollback(ctx)
		if _, err := holder.Exec(ctx, `lock table oauth_narrowed_access_tokens in share mode`); err != nil {
			t.Fatalf("hold oauth_narrowed_access_tokens: %v", err)
		}
		form := g.personTrade(t, narrowedBoxSession)
		form.Set("scope", oauthwire.ScopeRead)
		headers := http.Header{"DPoP": {dpopProof(t, box, g.url+"/token")}, oauthwire.HeaderDPoPHolder: {dpopProof(t, newKey(t), g.url+"/token")}}
		traded := make(chan tokenAnswer, 1)
		go func() { traded <- postTokenWith(g.url+"/token", form, headers) }()
		const insert = "insert into oauth_narrowed_access_tokens%"
		waitForBlocked(t, g.pool, holder.Conn().PgConn().PID(), insert)
		trade := blockedPID(t, g.fixture, holder.Conn().PgConn().PID(), insert)
		revoked := make(chan error, 1)
		go func() {
			_, err := g.enrollments().RevokePersonSignIn(ctx, signIn, person, person)
			revoked <- err
		}()
		waitForBlocked(t, g.pool, trade, "%person_signins%")
		if err := holder.Rollback(ctx); err != nil {
			t.Fatalf("release oauth_narrowed_access_tokens: %v", err)
		}
		answer := <-traded
		if answer.err != nil {
			t.Fatalf("the read trade: %v", answer.err)
		}
		token := accessTokenOf(t, answer.status, answer.body)
		if err := <-revoked; err != nil {
			t.Fatalf("the revoke: %v", err)
		}
		if got := g.introspected(t, token); got.Active {
			t.Fatalf("the read token whose trade committed while the revoke waited introspects live: %+v", got)
		}
	})
}

// TestAProviderNeedsItsSignInRecorder: a configuration that takes a person's token and has
// nowhere to record the sign-in is refused at build, as one that takes a machine's and cannot
// mint its credential is, since a sign-in nothing records could never be revoked.
func TestAProviderNeedsItsSignInRecorder(t *testing.T) {
	for _, c := range []struct {
		name   string
		adjust func(*oauth.ExchangeConfig)
		says   string
	}{
		{name: "no recorder", adjust: func(e *oauth.ExchangeConfig) { e.PersonSignIns = nil }, says: "PersonSignIns"},
		{name: "no Google issuer", adjust: func(e *oauth.ExchangeConfig) {
			e.Google = oauth.IssuerConfig{}
			e.MachineCredentials = nil
		}, says: "Google issuer"},
	} {
		t.Run(c.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("building the provider panicked rather than refusing: %v", r)
				}
			}()
			err := buildRefusal(t, c.adjust)
			if err == nil || !strings.Contains(err.Error(), c.says) {
				t.Fatalf("oauth.New = %v, want a refusal naming %q", err, c.says)
			}
		})
	}
}

// buildRefusal builds a provider whose exchange takes a person's token, with adjust applied to
// the exchange's configuration last, and answers whatever oauth.New said.
func buildRefusal(t *testing.T, adjust func(*oauth.ExchangeConfig)) error {
	t.Helper()
	google := oidctest.New(t)
	google.PublishKey(t, "google-key")
	keys, err := oidc.NewKeys(context.Background(), google.URL())
	if err != nil {
		t.Fatalf("Google keys: %v", err)
	}
	cfg := oauth.ExchangeConfig{
		Policy:             newPolicy(t, nil, nil, exchange.GoogleEntry{Account: signInMachine, Service: signInFleet}),
		Google:             oauth.IssuerConfig{Issuer: google.URL(), Audience: machineAudience, Keys: keys},
		PersonAudience:     deviceAudience,
		MachineCredentials: func(context.Context, oauth.MachineCredential) (string, error) { return "", nil },
		PersonSignIns:      func(context.Context, oauth.PersonSignIn) error { return nil },
	}
	adjust(&cfg)
	_, err = oauth.New(oauth.Config{
		Secret:          make([]byte, 32),
		DeviceLifetime:  time.Minute,
		AccessLifetime:  time.Minute,
		RefreshLifetime: time.Minute,
		ProofSkew:       time.Second,
		VerificationURL: "https://dispatch.example/credentials/machine",
		Exchange:        cfg,
	}, nil)
	return err
}
