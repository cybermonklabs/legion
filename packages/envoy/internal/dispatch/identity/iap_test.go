package identity

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/admission"
	"github.com/sjawhar/envoy/internal/oidc"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
	"github.com/sjawhar/envoy/internal/testwait"
)

// iapAudience is the audience IAP signs for the test's load balancer backend service.
const iapAudience = "/projects/123456789012/global/backendServices/9876543210"

// iapRig is an IAPIdentity admitting the people of example.com, whose verifier reads a key set
// served on this machine in IAP's place, so no test calls Google. source is that verifier's key
// source.
type iapRig struct {
	keys     *oidctest.Issuer
	signing  *oidctest.Key
	people   *testPeopleStore
	source   *oidc.Keys
	identity IAPIdentity
}

func newIAPRig(t *testing.T) iapRig {
	t.Helper()
	keys := oidctest.New(t)
	signing := oidctest.NewES256Key(t, "iap-key")
	keys.Publish(signing)
	people := newTestPeopleStore(nil)
	source := oidc.NewKeysAt(keys.URL() + "/keys")
	return iapRig{keys: keys, signing: signing, people: people, source: source, identity: IAPIdentity{
		Verifier: oidc.NewIAPVerifier(iapAudience, source),
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

// A refused assertion's WARN line names its class and carries none of the token's text. Login runs
// before any authorization, so the iss, aud, sub and email of an assertion nobody signed are
// whatever its sender chose: here long runs of marker text with control characters. Each is
// refused with exactly one WARN line, `reason=<class>`, of a bounded length, that holds no marker.
// The control, IAP's own assertion for another audience, is refused the same way.
func TestIAPIdentityLogsARefusalsClassAndNoneOfItsText(t *testing.T) {
	var logged bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })

	rig := newIAPRig(t)
	forger := oidctest.NewES256Key(t, rig.signing.KeyID())
	chosen := func(marker string) string {
		return "https://" + strings.Repeat(marker, 2000) + "\x00\x07\x1b[31m\r\nlevel=ERROR msg=" + marker
	}
	for _, tc := range []struct {
		name, reason, marker string
		key                  *oidctest.Key
		change               func(map[string]any)
	}{
		{"a forged iss", "issuer", "ISS-MARKER", forger, func(c map[string]any) { c["iss"] = chosen("ISS-MARKER") }},
		{"a forged aud", "audience", "AUD-MARKER", forger, func(c map[string]any) { c["aud"] = chosen("AUD-MARKER") }},
		{"a forged aud array", "audience", "AUDS-MARKER", forger, func(c map[string]any) {
			c["aud"] = []string{chosen("AUDS-MARKER"), chosen("AUDS-MARKER")}
		}},
		{"a forged sub and email", "signature", "WHO-MARKER", forger, func(c map[string]any) {
			c["sub"], c["email"] = chosen("WHO-MARKER"), chosen("WHO-MARKER")+"@example.com"
		}},
		{"an expired forged iss", "issuer", "OLD-MARKER", forger, func(c map[string]any) {
			c["iss"] = chosen("OLD-MARKER")
			c["iat"], c["exp"] = time.Now().Add(-20*time.Minute).Unix(), time.Now().Add(-10*time.Minute).Unix()
		}},
		{"control: IAP's assertion for another audience", "audience", "CONTROL-MARKER", rig.signing, func(c map[string]any) {
			c["aud"] = chosen("CONTROL-MARKER")
		}},
	} {
		logged.Reset()
		if _, err := rig.login(rig.assertion(t, tc.key, "example.com", tc.change), nil); !errors.Is(err, ErrNoIdentity) || errors.Is(err, ErrCredentialExpired) {
			t.Errorf("%s: Login = %v, want ErrNoIdentity", tc.name, err)
		}
		lines := strings.Split(strings.TrimSpace(logged.String()), "\n")
		if len(lines) != 1 || !strings.Contains(lines[0], "level=WARN") || !strings.Contains(lines[0], "reason="+tc.reason) {
			t.Errorf("%s: logged %d lines %q, want one WARN line with reason=%s", tc.name, len(lines), lines, tc.reason)
		}
		if strings.Contains(logged.String(), tc.marker) || logged.Len() > 200 {
			t.Errorf("%s: the log carries the token's own text: %d bytes, starting %.300q", tc.name, logged.Len(), logged.String())
		}
	}
}

// heldIAPKeyReads is a key-set transport that holds each read at its start until letGo, and says on
// entered that a read has begun, so a test can verify an assertion while a read is under way.
type heldIAPKeyReads struct {
	entered chan struct{}
	release chan struct{}
	let     bool
}

func newHeldIAPKeyReads() *heldIAPKeyReads {
	return &heldIAPKeyReads{entered: make(chan struct{}, 1), release: make(chan struct{})}
}

// letGo lets every held read go on, once; the test's goroutine alone calls it.
func (h *heldIAPKeyReads) letGo() {
	if !h.let {
		h.let = true
		close(h.release)
	}
}

func (h *heldIAPKeyReads) RoundTrip(request *http.Request) (*http.Response, error) {
	select {
	case h.entered <- struct{}{}:
	default:
	}
	select {
	case <-h.release:
	case <-request.Context().Done():
		return nil, request.Context().Err()
	}
	return http.DefaultTransport.RoundTrip(request)
}

// loginAnswer is what one Login answered.
type loginAnswer struct {
	email string
	err   error
}

// A verification that will wait on IAP's key set gives its place of the request pool's share
// outside admission back before it waits: here an assertion signed by a key the cached set does
// not hold arrives while another assertion's read of the set, which closed the miss window, is
// under way, so the verification joins that read (oidc.Keys.WouldRead answers true, and Key
// waits). The place is free for as long as the read is held, and Login takes it again for the
// person's record once the read answers; the set is read once for both assertions.
func TestIAPIdentityGivesItsPlaceBackBeforeWaitingOnAReadUnderWay(t *testing.T) {
	rig := newIAPRig(t)
	if _, err := rig.login(rig.assertion(t, rig.signing, "example.com", nil), nil); err != nil {
		t.Fatalf("a member's assertion, which reads the set: %v", err)
	}
	rotated := oidctest.NewES256Key(t, "iap-key-rotated")
	rig.keys.Publish(rotated)
	held := newHeldIAPKeyReads()
	rig.source.HTTPClient = &http.Client{Transport: held}
	defer held.letGo()
	first := make(chan error, 1)
	go func() {
		_, err := rig.source.Key(context.Background(), rotated.KeyID(), "ES256")
		first <- err
	}()
	select {
	case <-held.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the first lookup of the rotated key prompted no read within 5 s")
	}

	assertion := rig.assertion(t, rotated, "example.com", nil)
	if !rig.identity.Verifier.WouldReadKeySet(assertion) {
		t.Error("WouldReadKeySet for the rotated key while the read it needs is under way = false, want true")
	}
	outside := admission.New(admission.DefaultLimits()).Outside()
	place, err := outside.Hold(context.Background(), admission.HeartbeatCheck)
	if err != nil {
		t.Fatalf("a place for the heartbeat check: %v", err)
	}
	defer place.Release()
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request = request.WithContext(admission.WithPlace(request.Context(), place))
	request.Header.Set(IAPAssertionHeader, assertion)
	answered := make(chan loginAnswer, 1)
	go func() {
		email, err := rig.identity.Login(request)
		answered <- loginAnswer{email: email, err: err}
	}()
	if !testwait.Holds(2*time.Second, time.Millisecond, func() bool { return outside.Stats().InUse == 0 }) {
		t.Fatalf("%d places held while the verification waits on the read under way, want none: Login gives its place back before the wait", outside.Stats().InUse)
	}
	select {
	case answer := <-answered:
		t.Fatalf("Login answered %q, %v while the read it waits on was held", answer.email, answer.err)
	default:
	}

	held.letGo()
	answer := testwait.Receive(t, "Login once the read answers", answered)
	if answer.err != nil || answer.email != "a.person@example.com" {
		t.Fatalf("Login once the read answers = %q, %v; want a.person@example.com", answer.email, answer.err)
	}
	if err := testwait.Receive(t, "the first lookup of the rotated key", first); err != nil {
		t.Fatalf("the first lookup of the rotated key: %v", err)
	}
	if held := outside.Stats().InUse; held != 1 {
		t.Errorf("%d places held once Login answered, want 1: the place taken again for the record", held)
	}
	if fetches := rig.keys.JWKSFetches(); fetches != 2 {
		t.Errorf("key set read %d times, want 2: the first assertion's and the one the rotated key prompted, which Login joined", fetches)
	}
}

// A verification answered from a key set already read and fresh keeps its place through the
// verification: WouldReadKeySet answers false, so Login neither gives the place back nor waits to
// take it again. Every other place of the share is held and one more work waits for one, so a
// place given back would go to that work and the record's retake would wait out the share's wait
// behind it and be answered a busy pool.
func TestIAPIdentityKeepsItsPlaceThroughAVerificationFromTheCachedSet(t *testing.T) {
	rig := newIAPRig(t)
	if _, err := rig.login(rig.assertion(t, rig.signing, "example.com", nil), nil); err != nil {
		t.Fatalf("a member's assertion, which reads the set: %v", err)
	}
	limits := admission.DefaultLimits()
	limits.OutsideWait = 2 * time.Second
	outside := admission.New(limits).Outside()
	places := make([]*admission.Place, 0, admission.OutsideLimit)
	for range admission.OutsideLimit {
		place, err := outside.Hold(context.Background(), admission.HeartbeatCheck)
		if err != nil {
			t.Fatalf("place %d of %d: %v", len(places)+1, admission.OutsideLimit, err)
		}
		places = append(places, place)
	}
	waiting, stop := context.WithCancel(context.Background())
	waited := make(chan error, 1)
	go func() { waited <- outside.Take(waiting, admission.SocketAdmission) }()
	t.Cleanup(func() {
		stop()
		for _, place := range places {
			place.Release()
		}
		if err := <-waited; err == nil {
			outside.Release()
		}
	})
	testwait.WithinPolling(t, 2*time.Second, time.Millisecond, "the waiting work in the queue", func() bool {
		return outside.Stats().Queued == 1
	})

	assertion := rig.assertion(t, rig.signing, "example.com", nil)
	if rig.identity.Verifier.WouldReadKeySet(assertion) {
		t.Error("WouldReadKeySet for the cached key of a fresh set = true, want false")
	}
	before := rig.keys.JWKSFetches()
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request = request.WithContext(admission.WithPlace(request.Context(), places[0]))
	request.Header.Set(IAPAssertionHeader, assertion)
	if email, err := rig.identity.Login(request); err != nil || email != "a.person@example.com" {
		t.Fatalf("Login from the cached set = %q, %v; want a.person@example.com with the place kept", email, err)
	}
	if stats := outside.Stats(); stats.InUse != admission.OutsideLimit || stats.Queued != 1 {
		t.Errorf("after Login, %d places held and %d works waiting, want %d and 1: the place was kept, not handed to the waiting work", stats.InUse, stats.Queued, admission.OutsideLimit)
	}
	if fetches := rig.keys.JWKSFetches(); fetches != before {
		t.Errorf("Login from the cached set read the key set %d times, want none", fetches-before)
	}
}
