package identity

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/oidc"
)

type testSessionStore struct {
	mu                     sync.Mutex
	generations            map[string]int64
	currentGenerationCalls int
	ensureSessionCalls     int
}

func (s *testSessionStore) CurrentSessionGeneration(_ context.Context, login string) (int64, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.currentGenerationCalls++
	generation, found := s.generations[login]
	return generation, found, nil
}

func (s *testSessionStore) EnsureSession(_ context.Context, login string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureSessionCalls++
	generation, found := s.generations[login]
	if !found {
		s.generations[login] = 0
	}
	return generation, nil
}

func (s *testSessionStore) RevokeSessions(_ context.Context, login string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.generations[login]++
	return nil
}

func (s *testSessionStore) generation(login string) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.generations[login]
}

// testPeopleStore is auth.PeopleStore in memory. A sign-in under another subject than the one it
// holds for the address ends the address's sessions in sessions, as store.PgPeopleStore does. now
// stands in for the database's clock: it dates each sign-in and confirmation, and Membership
// answers a confirmation's age by it; nil is time.Now.
type testPeopleStore struct {
	mu       sync.Mutex
	people   map[string]testPerson
	sessions *testSessionStore
	now      func() time.Time
}

// testPerson is one person's row in testPeopleStore: ConfirmedAt is the store clock's time of
// the last confirmation, zero for none, Kind the member kind the last sign-in or confirmation
// wrote, HostedDomain the `hd` it recorded, Held a recorded hold, SignedOut a sign-out or an
// EndKeep that kept the refresh token, and SignedInAt the store clock's time of the last sign-in.
type testPerson struct {
	RefreshToken string
	Subject      string
	ConfirmedAt  time.Time
	Kind         model.MemberKind
	HostedDomain string
	Held         bool
	SignedOut    bool
	SignedInAt   time.Time
}

func newTestPeopleStore(sessions *testSessionStore) *testPeopleStore {
	return &testPeopleStore{people: map[string]testPerson{}, sessions: sessions}
}

func (s *testPeopleStore) clock() time.Time {
	if s.now == nil {
		return time.Now()
	}
	return s.now()
}

func (s *testPeopleStore) Record(_ context.Context, email string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, found := s.people[email]; !found {
		s.people[email] = testPerson{}
	}
	return nil
}

func (s *testPeopleStore) SignIn(_ context.Context, email, refreshToken string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	person := s.people[email]
	person.RefreshToken, person.ConfirmedAt, person.Kind = refreshToken, s.clock(), model.MemberFull
	person.SignedInAt, person.Held, person.SignedOut = s.clock(), false, false
	s.people[email] = person
	return nil
}

func (s *testPeopleStore) SignInSubject(ctx context.Context, email, subject, refreshToken, hostedDomain string, kind model.MemberKind) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	person, existed := s.people[email]
	if existed && person.Subject != subject {
		if err := s.sessions.RevokeSessions(ctx, email); err != nil {
			return err
		}
	}
	person.Subject = subject
	person.RefreshToken, person.ConfirmedAt, person.Kind, person.HostedDomain = refreshToken, s.clock(), kind, hostedDomain
	person.SignedInAt, person.Held, person.SignedOut = s.clock(), false, false
	s.people[email] = person
	return nil
}

func (s *testPeopleStore) Membership(_ context.Context, email string) (auth.PersonMembership, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	person, found := s.people[email]
	membership := auth.PersonMembership{
		RefreshToken: person.RefreshToken, Subject: person.Subject, Kind: person.Kind, HostedDomain: person.HostedDomain,
		Held: person.Held, SignedOut: person.SignedOut, SignedInAt: person.SignedInAt, ReadAt: s.clock(),
	}
	if !person.ConfirmedAt.IsZero() {
		membership.Confirmed, membership.ConfirmedAge = true, s.clock().Sub(person.ConfirmedAt)
	}
	return membership, found, nil
}

func (s *testPeopleStore) ConfirmSubject(_ context.Context, email, subject, readToken, refreshToken, hostedDomain string, kind model.MemberKind) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	person := s.people[email]
	if person.Subject != subject || person.RefreshToken != readToken {
		return nil
	}
	person.RefreshToken, person.ConfirmedAt, person.Kind, person.HostedDomain = refreshToken, s.clock(), kind, hostedDomain
	s.people[email] = person
	return nil
}

func (s *testPeopleStore) End(_ context.Context, email string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if person, found := s.people[email]; found {
		person.RefreshToken, person.ConfirmedAt, person.SignedOut = "", time.Time{}, false
		s.people[email] = person
	}
	return nil
}

func (s *testPeopleStore) KeepAfterSignOut(_ context.Context, email string, signedInAt time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if person, found := s.people[email]; found && person.RefreshToken != "" && person.SignedInAt.Equal(signedInAt) {
		person.SignedOut = true
		s.people[email] = person
	}
	return nil
}

func (s *testPeopleStore) ForgetAfterSignOut(_ context.Context, email string, signedInAt time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if person, found := s.people[email]; found && person.SignedInAt.Equal(signedInAt) {
		person.RefreshToken, person.ConfirmedAt, person.SignedOut = "", time.Time{}, false
		s.people[email] = person
	}
	return nil
}

func (s *testPeopleStore) get(email string) (testPerson, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	person, found := s.people[email]
	return person, found
}

// testSigningKey is the one-key set the package's session cookies are signed with.
var testSigningKey = auth.SingleSigningKey("signing-key")

func cookieRequest(t *testing.T, email string, generation int64) *http.Request {
	t.Helper()
	cookie := strings.SplitN(auth.IssueSessionCookie(email, generation, testSigningKey, true), ";", 2)[0]
	name, value, ok := strings.Cut(cookie, "=")
	if !ok {
		t.Fatalf("invalid session cookie %q", cookie)
	}
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.AddCookie(&http.Cookie{Name: name, Value: value})
	return request
}

// A test or local harness header names a person by email; Dispatch's form of it is lowercased,
// and the person is recorded as having signed in, which is what makes them assignable.
func TestHeaderIdentityNamesThePersonLowercasedAndRecordsThem(t *testing.T) {
	people := newTestPeopleStore(nil)
	identity := HeaderIdentity{Header: "X-Dispatch-User", People: people}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Dispatch-User", " Sami@Example.com ")
	email, err := identity.Login(req)
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if email != "sami@example.com" {
		t.Errorf("login: got %q, want sami@example.com", email)
	}
	if _, found := people.get("sami@example.com"); !found {
		t.Errorf("the person the header named was not recorded: %#v", people.people)
	}
}

func TestHeaderIdentityRejectsAMissingHeader(t *testing.T) {
	people := newTestPeopleStore(nil)
	identity := HeaderIdentity{Header: "X-Dispatch-User", People: people}
	for _, value := range []string{"", "   "} {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("X-Dispatch-User", value)
		if _, err := identity.Login(req); !errors.Is(err, ErrNoIdentity) {
			t.Errorf("header %q error: got %v, want ErrNoIdentity", value, err)
		}
	}
	if len(people.people) != 0 {
		t.Errorf("an unnamed request recorded people: %#v", people.people)
	}
}

func TestCookieIdentityNamesThePersonOfAValidCookie(t *testing.T) {
	sessions := &testSessionStore{generations: map[string]int64{"a.b+c@d.example": 0}}
	identity := CookieIdentity{SigningKey: testSigningKey, Sessions: sessions}
	if _, err := identity.Login(httptest.NewRequest(http.MethodGet, "/", nil)); !errors.Is(err, ErrNoIdentity) {
		t.Errorf("missing cookie error: got %v, want ErrNoIdentity", err)
	}

	request := cookieRequest(t, "a.b+c@d.example", 0)
	email, err := identity.Login(request)
	if err != nil {
		t.Fatalf("valid cookie: %v", err)
	}
	if email != "a.b+c@d.example" {
		t.Errorf("login: got %q, want a.b+c@d.example", email)
	}

	if _, err := (CookieIdentity{SigningKey: testSigningKey}).Login(request); !errors.Is(err, ErrNoIdentity) {
		t.Errorf("cookie identity without session store error: got %v, want ErrNoIdentity", err)
	}
	if err := sessions.RevokeSessions(context.Background(), "a.b+c@d.example"); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, err := identity.Login(request); !errors.Is(err, ErrNoIdentity) {
		t.Errorf("cookie of a revoked generation error: got %v, want ErrNoIdentity", err)
	}
}

func TestCookieIdentityReadsGenerationWithoutWriting(t *testing.T) {
	sessions := &testSessionStore{generations: map[string]int64{"sami@example.com": 0}}
	email, err := (CookieIdentity{SigningKey: testSigningKey, Sessions: sessions}).Login(cookieRequest(t, "sami@example.com", 0))
	if err != nil || email != "sami@example.com" {
		t.Fatalf("cookie login = %q, %v; want sami@example.com, nil", email, err)
	}
	if sessions.ensureSessionCalls != 0 {
		t.Fatalf("cookie identity created the session row %d time(s)", sessions.ensureSessionCalls)
	}
	if sessions.currentGenerationCalls != 1 {
		t.Fatalf("cookie identity generation reads = %d, want 1", sessions.currentGenerationCalls)
	}
}

func TestCookieIdentityRejectsCookieWithoutSessionRow(t *testing.T) {
	sessions := &testSessionStore{generations: map[string]int64{}}
	if _, err := (CookieIdentity{SigningKey: testSigningKey, Sessions: sessions}).Login(cookieRequest(t, "sami@example.com", 0)); !errors.Is(err, ErrNoIdentity) {
		t.Fatalf("cookie without session row error = %v, want ErrNoIdentity", err)
	}
}

func TestWriteErrorMapsNoIdentityToUnauthorized(t *testing.T) {
	w := httptest.NewRecorder()
	WriteError(w, ErrNoIdentity)
	if w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), `"code":"NO_IDENTITY"`) {
		t.Errorf("no identity: status %d body %s, want 401 NO_IDENTITY", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	WriteError(w, fmt.Errorf("%w: %w", ErrSignInUnavailable, fmt.Errorf("oidc: refresh: %w: oauth2: 503", oidc.ErrIssuerUnavailable)))
	if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), `"code":"SIGNIN_UNAVAILABLE"`) || !strings.Contains(w.Body.String(), "try again in a minute") {
		t.Errorf("sign-in issuer unreachable: status %d body %s, want 503 SIGNIN_UNAVAILABLE", w.Code, w.Body.String())
	}
	// An issuer outage that is not the sign-in's (a service-account token's issuer) is not
	// SIGNIN_UNAVAILABLE: only ErrSignInUnavailable answers that, whatever else the error carries.
	w = httptest.NewRecorder()
	WriteError(w, fmt.Errorf("verify: %w", oidc.ErrIssuerUnavailable))
	if w.Code != http.StatusInternalServerError || strings.Contains(w.Body.String(), "SIGNIN_UNAVAILABLE") {
		t.Errorf("another issuer unreachable: status %d body %s, want 500 and not SIGNIN_UNAVAILABLE", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	WriteError(w, errors.New("database down"))
	if w.Code != http.StatusInternalServerError {
		t.Errorf("other error: status %d, want 500", w.Code)
	}
}

// A session the sign-in rule says why it ended is still Dispatch's 401 NO_IDENTITY, which every
// caller refusing no identity matches, now with the reason and the address it ended for.
func TestOutsideSignInWriteErrorTellsTheReason(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{
		{&SignedOut{Email: "outsider@other.example", Reason: SignedOutNotListed},
			`{"code":"NO_IDENTITY","error":"signed out: no Dispatch project's access list names outsider@other.example any more","login":"outsider@other.example","reason":"NOT_LISTED"}` + "\n"},
		{fmt.Errorf("resolve the caller: %w", &SignedOut{Email: "outsider@other.example", Reason: SignedOutOutsideOff}),
			`{"code":"NO_IDENTITY","error":"signed out: Dispatch no longer signs in outsider@other.example or any other account outside its Google Workspace domain","login":"outsider@other.example","reason":"OUTSIDE_SIGNIN_OFF"}` + "\n"},
		{&SignedOut{Email: "outsider@second.example", Reason: SignedOutWorkspaceAddress},
			`{"code":"NO_IDENTITY","error":"signed out: outsider@second.example is an address on a domain the Google Workspace owns, which signs in only as a Workspace account","login":"outsider@second.example","reason":"WORKSPACE_ADDRESS"}` + "\n"},
		{ErrNoIdentity, `{"code":"NO_IDENTITY","error":"unauthorized"}` + "\n"},
	} {
		if !errors.Is(tc.err, ErrNoIdentity) {
			t.Errorf("%v does not match ErrNoIdentity", tc.err)
		}
		w := httptest.NewRecorder()
		WriteError(w, tc.err)
		if w.Code != http.StatusUnauthorized || w.Body.String() != tc.want {
			t.Errorf("WriteError(%v) = %d %s, want 401 %s", tc.err, w.Code, w.Body.String(), tc.want)
		}
	}
}

func (s *testPeopleStore) EndSubject(ctx context.Context, email, subject, refreshToken string, mode auth.EndMode) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	person, found := s.people[email]
	if !found || person.Subject != subject || person.RefreshToken != refreshToken {
		return false, nil
	}
	if err := s.sessions.RevokeSessions(ctx, email); err != nil {
		return false, err
	}
	switch mode {
	case auth.EndKeep:
		person.ConfirmedAt, person.SignedOut = s.clock(), true
	case auth.EndHold:
		person.RefreshToken, person.ConfirmedAt, person.Held = "", time.Time{}, true
	default:
		person.RefreshToken, person.ConfirmedAt = "", time.Time{}
	}
	s.people[email] = person
	return true, nil
}
