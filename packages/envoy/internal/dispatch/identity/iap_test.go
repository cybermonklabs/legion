package identity

import (
	"bytes"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/oidc"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
)

// iapAudience is the audience IAP signs for the test's load balancer backend service.
const iapAudience = "/projects/123456789012/global/backendServices/9876543210"

// iapRig is an IAPIdentity admitting the people of example.com, whose verifier reads a key set
// served on this machine in IAP's place, so no test calls Google.
type iapRig struct {
	keys     *oidctest.Issuer
	signing  *oidctest.Key
	people   *testPeopleStore
	identity IAPIdentity
}

func newIAPRig(t *testing.T) iapRig {
	t.Helper()
	keys := oidctest.New(t)
	signing := oidctest.NewES256Key(t, "iap-key")
	keys.Publish(signing)
	people := newTestPeopleStore(nil)
	return iapRig{keys: keys, signing: signing, people: people, identity: IAPIdentity{
		Verifier: oidc.NewIAPVerifier(iapAudience, oidc.NewKeysAt(keys.URL()+"/keys")),
		Rule:     DomainRule{Domain: "example.com"},
		People:   people,
	}}
}

// assertion is an assertion IAP signs for a Google account of hd, with change applied to its
// claims, signed by key.
func (r iapRig) assertion(t *testing.T, key *oidctest.Key, hd string, change func(map[string]any)) string {
	t.Helper()
	now := time.Now()
	claims := map[string]any{
		"iss":   oidc.IAPIssuer,
		"aud":   iapAudience,
		"sub":   "accounts.google.com:118000000000000000001",
		"email": "A.Person@" + hd,
		"hd":    hd,
		"iat":   now.Add(-time.Minute).Unix(),
		"exp":   now.Add(9 * time.Minute).Unix(),
	}
	if change != nil {
		change(claims)
	}
	return r.keys.Mint(t, key, claims)
}

func (r iapRig) login(assertion string, headers map[string]string) (string, error) {
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	if assertion != "" {
		request.Header.Set(IAPAssertionHeader, assertion)
	}
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	return r.identity.Login(request)
}

// A member's assertion, signed by IAP's key for this audience, names them, lowercased, and records
// them as a person.
func TestIAPIdentityAdmitsAMemberAndRecordsThem(t *testing.T) {
	rig := newIAPRig(t)
	email, err := rig.login(rig.assertion(t, rig.signing, "example.com", nil), nil)
	if err != nil || email != "a.person@example.com" {
		t.Fatalf("Login = %q, %v; want a.person@example.com", email, err)
	}
	if _, found := rig.people.get("a.person@example.com"); !found {
		t.Errorf("the member was not recorded: %#v", rig.people.people)
	}
}

// Every request IAP did not vouch for a member of the domain is refused as no identity, and records
// nobody: no assertion (the unsigned email header alone included), one signed by another key, one
// for another audience, an expired one, and a verified assertion of an account outside the domain.
// Only an expired assertion that IAP signed for a member, with nothing else wrong, is
// ErrCredentialExpired, which a socket's heartbeat reads as a rotation rather than as its person
// gone, and it alone is logged at DEBUG; an expired one that is also wrong (signed by any key but
// IAP's, RS256, its person outside the domain, no iat) is refused for that, with a WARN line, as a
// fresh one is.
func TestIAPIdentityRefusesAllButAVerifiedMember(t *testing.T) {
	var logged bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })

	rig := newIAPRig(t)
	forger := oidctest.NewES256Key(t, rig.signing.KeyID())
	forgedKid := oidctest.NewES256Key(t, "forged-kid")
	rsaSameKid := oidctest.NewKey(t, rig.signing.KeyID())
	expired := func(change func(map[string]any)) func(map[string]any) {
		return func(c map[string]any) {
			c["iat"], c["exp"] = time.Now().Add(-20*time.Minute).Unix(), time.Now().Add(-10*time.Minute).Unix()
			if change != nil {
				change(c)
			}
		}
	}
	parts := strings.Split(rig.assertion(t, rig.signing, "example.com", expired(nil)), ".")
	zeroed := parts[0] + "." + parts[1] + "." + strings.Repeat("A", len(parts[2]))
	for _, tc := range []struct {
		name      string
		assertion string
		headers   map[string]string
		// expired is whether the refusal is ErrCredentialExpired; warns, whether it logs a WARN.
		expired, warns bool
	}{
		{"no assertion", "", nil, false, false},
		{"the unsigned email header alone", "", map[string]string{
			"X-Goog-Authenticated-User-Email": "accounts.google.com:a.person@example.com",
			"X-Goog-Authenticated-User-Id":    "accounts.google.com:118000000000000000001",
		}, false, false},
		{"an assertion signed by another key under IAP's key id", rig.assertion(t, forger, "example.com", nil), nil, false, true},
		{"an assertion for another audience", rig.assertion(t, rig.signing, "example.com", func(c map[string]any) {
			c["aud"] = "/projects/123456789012/global/backendServices/1"
		}), nil, false, true},
		{"an expired assertion", rig.assertion(t, rig.signing, "example.com", expired(nil)), nil, true, false},
		{"an expired assertion signed by another key under IAP's key id", rig.assertion(t, forger, "example.com", expired(nil)), nil, false, true},
		{"an expired assertion under a key id IAP never published", rig.assertion(t, forgedKid, "example.com", expired(nil)), nil, false, true},
		{"an expired assertion of IAP's payload with its signature zeroed", zeroed, nil, false, true},
		{"an expired RS256 assertion under IAP's key id", rig.assertion(t, rsaSameKid, "example.com", expired(nil)), nil, false, true},
		{"an expired assertion IAP signed for an account outside the domain", rig.assertion(t, rig.signing, "other.example", expired(nil)), nil, false, true},
		{"an expired assertion IAP signed with no iat", rig.assertion(t, rig.signing, "example.com", expired(func(c map[string]any) {
			delete(c, "iat")
		})), nil, false, true},
		{"a verified assertion of an account outside the domain", rig.assertion(t, rig.signing, "other.example", nil), nil, false, true},
		{"a verified assertion of a personal account at the domain's address", rig.assertion(t, rig.signing, "example.com", func(c map[string]any) {
			delete(c, "hd")
		}), nil, false, true},
	} {
		logged.Reset()
		email, err := rig.login(tc.assertion, tc.headers)
		if !errors.Is(err, ErrNoIdentity) {
			t.Errorf("%s: Login = %q, %v; want ErrNoIdentity", tc.name, email, err)
		}
		if errors.Is(err, ErrCredentialExpired) != tc.expired {
			t.Errorf("%s: Login = %v; ErrCredentialExpired %t, want %t", tc.name, err, !tc.expired, tc.expired)
		}
		if warned := strings.Contains(logged.String(), "level=WARN"); warned != tc.warns {
			t.Errorf("%s: logged %q; a WARN line %t, want %t", tc.name, strings.TrimSpace(logged.String()), warned, tc.warns)
		}
	}
	if len(rig.people.people) != 0 {
		t.Errorf("refused requests recorded people: %#v", rig.people.people)
	}
	// The control: the same rig admits a member's assertion signed by IAP's key.
	if _, err := rig.login(rig.assertion(t, rig.signing, "example.com", nil), nil); err != nil {
		t.Fatalf("control: a member's assertion: %v", err)
	}
}

// IAP's key set that cannot be read decides nothing about the person: the request is answered as
// the sign-in issuer being unavailable (503), never as a refusal.
func TestIAPIdentityAnswersAnUnreadableKeySetAsSignInUnavailable(t *testing.T) {
	rig := newIAPRig(t)
	rig.keys.FailKeys(http.StatusServiceUnavailable)
	_, err := rig.login(rig.assertion(t, rig.signing, "example.com", nil), nil)
	if !errors.Is(err, ErrSignInUnavailable) || errors.Is(err, ErrNoIdentity) {
		t.Fatalf("Login while IAP's key set answers 503: err = %v, want ErrSignInUnavailable", err)
	}
}
