package oidc

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/oidc/oidctest"
)

const (
	testAudience = "dispatch"
	testSubject  = "system:serviceaccount:legion:legion-worker"
)

// newVerifierFor discovers issuer and returns a verifier for testAudience,
// along with the published signing key.
func newVerifierFor(t *testing.T, issuer *oidctest.Issuer) (*Verifier, *oidctest.Key) {
	t.Helper()
	signing := oidctest.NewKey(t, "signing-key")
	issuer.Publish(signing)
	verifier, err := New(context.Background(), issuer.URL(), testAudience)
	if err != nil {
		t.Fatalf("New(%s, %s): %v", issuer.URL(), testAudience, err)
	}
	return verifier, signing
}

func TestVerifyAcceptsProjectedServiceAccountToken(t *testing.T) {
	issuer := oidctest.New(t)
	verifier, signing := newVerifierFor(t, issuer)

	arrayClaims := issuer.Claims(testSubject, testAudience)
	stringClaims := issuer.Claims(testSubject, testAudience)
	stringClaims["aud"] = testAudience

	tests := []struct {
		name   string
		claims map[string]any
	}{
		{name: "audience as a one-element array", claims: arrayClaims},
		{name: "audience as a bare string", claims: stringClaims},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			claims, err := verifier.Verify(context.Background(), issuer.Mint(t, signing, tt.claims))
			if err != nil {
				t.Fatalf("Verify: unexpected error: %v", err)
			}
			if claims.Subject != testSubject {
				t.Errorf("Subject = %q, want %q", claims.Subject, testSubject)
			}
			if claims.Issuer != issuer.URL() {
				t.Errorf("Issuer = %q, want %q", claims.Issuer, issuer.URL())
			}
			if len(claims.Audience) != 1 || claims.Audience[0] != testAudience {
				t.Errorf("Audience = %q, want [%q]", claims.Audience, testAudience)
			}
			wantExpiry := time.Unix(tt.claims["exp"].(int64), 0)
			if !claims.Expiry.Equal(wantExpiry) {
				t.Errorf("Expiry = %v, want %v", claims.Expiry, wantExpiry)
			}
		})
	}
}

func TestVerifyClassifiesRejection(t *testing.T) {
	issuer := oidctest.New(t)
	verifier, signing := newVerifierFor(t, issuer)
	// Same key id as the published key, different key material: the key set
	// resolves, only the signature fails.
	foreign := oidctest.NewKey(t, signing.KeyID())

	wrongAudience := issuer.Claims(testSubject, testAudience)
	wrongAudience["aud"] = []string{"envoy"}

	expired := issuer.Claims(testSubject, testAudience)
	expired["iat"] = time.Now().Add(-2 * time.Hour).Unix()
	expired["exp"] = time.Now().Add(-time.Hour).Unix()

	otherIssuer := issuer.Claims(testSubject, testAudience)
	otherIssuer["iss"] = "https://oidc.eks.example.invalid/id/OTHER"

	tests := []struct {
		name  string
		token string
		want  error
	}{
		{name: "audience the verifier was not configured for", token: issuer.Mint(t, signing, wrongAudience), want: ErrAudience},
		{name: "token past its expiry", token: issuer.Mint(t, signing, expired), want: ErrExpired},
		{name: "issuer the verifier was not configured for", token: issuer.Mint(t, signing, otherIssuer), want: ErrIssuer},
		{name: "signed by a key the issuer does not publish", token: issuer.Mint(t, foreign, issuer.Claims(testSubject, testAudience)), want: ErrSignature},
		{name: "three segments that are not a token", token: "not.a.jwt", want: ErrMalformed},
		{name: "no segments at all", token: "abc", want: ErrMalformed},
		{name: "three segments that are not base64url", token: "@@@.@@@.@@@", want: ErrMalformed},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			claims, err := verifier.Verify(context.Background(), tt.token)
			if !errors.Is(err, tt.want) {
				t.Fatalf("Verify error = %v, want %v", err, tt.want)
			}
			if !reflect.DeepEqual(claims, Claims{}) {
				t.Errorf("Verify returned claims %+v on a rejected token, want the zero value", claims)
			}
			if strings.Contains(err.Error(), tt.token) {
				t.Errorf("Verify error %q repeats the token", err)
			}
		})
	}
}

// A verifier of a list of audiences judges a token by the listed audience it carries, so a token
// fails for the reason a verifier of that one audience fails it: an expired token of the second
// listed audience is expired, and a token of an audience the list does not hold is for another
// audience, expired or not. A verifier of one audience judges as it always has. Each row is a row
// of the state table of dispatch://INFRA-5514/artifact/plan-md, O1 to O15: "list" is the verifier
// of a and b, "one" the verifier of a alone, both on the one key set.
func TestAVerifierOfAListOfAudiencesJudgesATokenByTheAudienceItCarries(t *testing.T) {
	issuer := oidctest.New(t)
	keys, signing := keysFor(t, issuer, "signing-key")
	list := NewWithKeysFor(issuer.URL(), []string{"a", "b"}, keys)
	one := NewWithKeys(issuer.URL(), "a", keys)
	// Same key id as the published key, different key material: the key set resolves, only the
	// signature fails.
	foreign := oidctest.NewKey(t, signing.KeyID())
	unpublished := oidctest.NewKey(t, "never-published")

	// mint signs a sound token whose aud is aud, a string or an array as the row writes it, once
	// change (when not nil) has spoiled one claim of it.
	mint := func(key *oidctest.Key, aud any, change func(claims map[string]any)) string {
		claims := issuer.Claims(testSubject, "")
		claims["aud"] = aud
		if change != nil {
			change(claims)
		}
		return issuer.Mint(t, key, claims)
	}
	expired := func(claims map[string]any) {
		claims["iat"] = time.Now().Add(-2 * time.Hour).Unix()
		claims["exp"] = time.Now().Add(-time.Hour).Unix()
	}
	otherIssuer := func(claims map[string]any) { claims["iss"] = "https://oidc.eks.example.invalid/id/OTHER" }

	tests := []struct {
		name     string
		verifier *Verifier
		token    string
		// wantAudience is an accepted token's Audience. want is a refused token's class, nil for
		// a token the verifier accepts, and wantText holds what the refusal's text must contain.
		wantAudience []string
		want         error
		wantText     []string
	}{
		{name: "O1 list: aud a", verifier: list, token: mint(signing, []string{"a"}, nil), wantAudience: []string{"a"}},
		{name: "O2 list: aud b", verifier: list, token: mint(signing, []string{"b"}, nil), wantAudience: []string{"b"}},
		{name: "O3 list: aud an array that holds b", verifier: list, token: mint(signing, []string{"x", "b"}, nil), wantAudience: []string{"x", "b"}},
		{name: "O4 list: aud b as a bare string", verifier: list, token: mint(signing, "b", nil), wantAudience: []string{"b"}},
		{name: "O5 list: aud x", verifier: list, token: mint(signing, []string{"x"}, nil), want: ErrAudience, wantText: []string{`"a"`, `"b"`}},
		{name: "O6 list: aud x, expired", verifier: list, token: mint(signing, []string{"x"}, expired), want: ErrAudience},
		{name: "O7 list: aud b, expired", verifier: list, token: mint(signing, []string{"b"}, expired), want: ErrExpired},
		{name: "O8 list: aud b, another issuer", verifier: list, token: mint(signing, []string{"b"}, otherIssuer), want: ErrIssuer},
		{name: "O9 list: aud x, another issuer", verifier: list, token: mint(signing, []string{"x"}, otherIssuer), want: ErrIssuer},
		{name: "O10 list: aud b, signed by a key the issuer does not publish", verifier: list, token: mint(foreign, []string{"b"}, nil), want: ErrSignature},
		{name: "O11 list: three segments that are not a token", verifier: list, token: "not.a.jwt", want: ErrMalformed},
		{name: "O12 list: aud b, signed under a key id the issuer does not publish", verifier: list, token: mint(unpublished, []string{"b"}, nil), want: ErrSignature},
		{name: "O13 one: aud x, expired", verifier: one, token: mint(signing, []string{"x"}, expired), want: ErrAudience, wantText: []string{`["x"], want "a"`}},
		{name: "O14 one: aud a, expired", verifier: one, token: mint(signing, []string{"a"}, expired), want: ErrExpired},
		{name: "O15 one: aud b", verifier: one, token: mint(signing, []string{"b"}, nil), want: ErrAudience},
	}

	classes := []error{ErrMalformed, ErrIssuer, ErrAudience, ErrExpired, ErrCancelled, ErrIssuerUnavailable, ErrSignature}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			claims, err := tt.verifier.Verify(context.Background(), tt.token)
			if tt.want == nil {
				if err != nil {
					t.Fatalf("Verify: %v, want the token accepted", err)
				}
				if claims.Subject != testSubject || claims.Issuer != issuer.URL() || !reflect.DeepEqual(claims.Audience, tt.wantAudience) {
					t.Errorf("Verify claims = %+v, want subject %q, issuer %q and audience %q", claims, testSubject, issuer.URL(), tt.wantAudience)
				}
				return
			}
			if !errors.Is(err, tt.want) {
				t.Fatalf("Verify error = %v, want %v", err, tt.want)
			}
			for _, class := range classes {
				if class != tt.want && errors.Is(err, class) {
					t.Errorf("Verify error %q is %v as well as %v, want one class", err, class, tt.want)
				}
			}
			for _, text := range tt.wantText {
				if !strings.Contains(err.Error(), text) {
					t.Errorf("Verify error %q does not contain %s", err, text)
				}
			}
			if !reflect.DeepEqual(claims, Claims{}) {
				t.Errorf("Verify returned claims %+v on a rejected token, want the zero value", claims)
			}
			if strings.Contains(err.Error(), tt.token) {
				t.Errorf("Verify error %q repeats the token", err)
			}
		})
	}
}

// Verify reads what it always has, whatever a token holds of the claims a domain-mode sign-in reads
// (email, email_verified, hd): a Cognito ID token whose email_verified is a number, null, an object
// or either spelling of true verifies with its Cognito fields, and the Workspace claims stay empty,
// since only the code flow's domain-mode session reads them.
func TestVerifyReadsACognitoTokenWhateverItsWorkspaceClaimsHold(t *testing.T) {
	issuer := oidctest.New(t)
	verifier, signing := newVerifierFor(t, issuer)
	for _, value := range []any{1, nil, map[string]any{"value": true}, "true", true} {
		claims := issuer.Claims("pool-subject", testAudience)
		claims["cognito:username"] = "ExampleIdP_a@d.example"
		claims["cognito:groups"] = []string{"dispatch-members"}
		claims["identities"] = []map[string]any{{"providerName": "ExampleIdP"}}
		claims["email"] = "someone-else@d.example"
		claims["email_verified"] = value
		claims["hd"] = "d.example"
		got, err := verifier.Verify(context.Background(), issuer.Mint(t, signing, claims))
		if err != nil {
			t.Errorf("email_verified %#v: Verify: %v", value, err)
			continue
		}
		if got.Subject != "pool-subject" || got.Username != "ExampleIdP_a@d.example" || !reflect.DeepEqual(got.Groups, []string{"dispatch-members"}) ||
			!reflect.DeepEqual(got.Providers, []string{"ExampleIdP"}) || got.Email != "" || got.EmailVerified || got.HostedDomain != "" {
			t.Errorf("email_verified %#v: claims = %#v, want the Cognito fields and no Workspace claims", value, got)
		}
	}
}

func TestVerifyRefreshesKeySetForRotatedKey(t *testing.T) {
	issuer := oidctest.New(t)
	verifier, signing := newVerifierFor(t, issuer)

	if _, err := verifier.Verify(context.Background(), issuer.Mint(t, signing, issuer.Claims(testSubject, testAudience))); err != nil {
		t.Fatalf("Verify with the published key: %v", err)
	}
	fetchesBefore := issuer.JWKSFetches()

	rotated := oidctest.NewKey(t, "rotated-key")
	token := issuer.Mint(t, rotated, issuer.Claims(testSubject, testAudience))
	issuer.Publish(rotated)

	claims, err := verifier.Verify(context.Background(), token)
	if err != nil {
		t.Fatalf("Verify with the rotated key: %v", err)
	}
	if claims.Subject != testSubject {
		t.Errorf("Subject = %q, want %q", claims.Subject, testSubject)
	}
	if got := issuer.JWKSFetches() - fetchesBefore; got != 1 {
		t.Errorf("key set fetched %d more times, want 1 refresh", got)
	}
}

// TestVerifyBoundsKeySetReadsUnderAnUnpublishedKeyID: go-oidc's own RemoteKeySet reads the
// issuer's key set again for every token naming a key id it does not hold, so one forged token
// replayed is one read of the issuer per copy. The verifier reads through *Keys instead, which
// bounds those reads at one per MinRefresh however many such tokens arrive.
func TestVerifyBoundsKeySetReadsUnderAnUnpublishedKeyID(t *testing.T) {
	issuer := oidctest.New(t)
	verifier, signing := newVerifierFor(t, issuer)
	if _, err := verifier.Verify(context.Background(), issuer.Mint(t, signing, issuer.Claims(testSubject, testAudience))); err != nil {
		t.Fatalf("Verify with the published key: %v", err)
	}
	if fetches := issuer.JWKSFetches(); fetches != 1 {
		t.Fatalf("key set read %d times for the first verification, want 1", fetches)
	}

	// One key the issuer never published, presented as a stream of distinct tokens: the shape a
	// replayed forgery takes. MinRefresh is the default minute here, which nothing in this test
	// passes.
	unpublished := oidctest.NewKey(t, "never-published")
	const tokens = 16
	for i := range tokens {
		claims := issuer.Claims(testSubject, testAudience)
		claims["jti"] = fmt.Sprintf("replay-%d", i)
		_, err := verifier.Verify(context.Background(), issuer.Mint(t, unpublished, claims))
		if !errors.Is(err, ErrSignature) {
			t.Fatalf("token %d, signed by a key the issuer never published = %v, want ErrSignature", i, err)
		}
	}
	if fetches := issuer.JWKSFetches(); fetches != 2 {
		t.Errorf("key set read %d times for %d tokens naming an unpublished key id, want 2: the first opens the refresh window and the rest fall inside it", fetches, tokens)
	}
}

// TestVerifyAcceptsAKeyRotatedInWithoutWaitingOutTheWindow is the other side of that bound: a key
// the issuer publishes after the cached set was read is accepted at once, because the first token
// naming a key id the set does not hold is allowed its one read. A rotation waits for no part of
// MinRefresh.
func TestVerifyAcceptsAKeyRotatedInWithoutWaitingOutTheWindow(t *testing.T) {
	issuer := oidctest.New(t)
	keys, signing := keysFor(t, issuer, "signing-key")
	// Longer than this test can take, so the rotation below is answered by the read a first miss
	// is allowed and not by the window reopening.
	keys.MinRefresh = time.Hour
	verifier := NewWithKeys(issuer.URL(), testAudience, keys)
	if _, err := verifier.Verify(context.Background(), issuer.Mint(t, signing, issuer.Claims(testSubject, testAudience))); err != nil {
		t.Fatalf("Verify with the published key: %v", err)
	}

	rotated := oidctest.NewKey(t, "rotated-key")
	token := issuer.Mint(t, rotated, issuer.Claims(testSubject, testAudience))
	issuer.Publish(rotated)

	claims, err := verifier.Verify(context.Background(), token)
	if err != nil {
		t.Fatalf("Verify with the key rotated in inside the window: %v", err)
	}
	if claims.Subject != testSubject {
		t.Errorf("Subject = %q, want %q", claims.Subject, testSubject)
	}
	if fetches := issuer.JWKSFetches(); fetches != 2 {
		t.Errorf("key set read %d times, want 2: the first verification and the rotation's one refresh", fetches)
	}
}

func TestNewReportsDiscoveryFailure(t *testing.T) {
	issuer := oidctest.New(t)
	issuer.FailDiscovery(http.StatusInternalServerError)

	_, err := New(context.Background(), issuer.URL(), testAudience)
	if err == nil {
		t.Fatal("New: expected an error when discovery fails")
	}
	if !strings.Contains(err.Error(), issuer.URL()) {
		t.Errorf("New error %q does not name the issuer URL %s", err, issuer.URL())
	}
}

func TestNewReportsIssuerMismatch(t *testing.T) {
	issuer := oidctest.New(t)
	issuer.AdvertiseIssuer("https://oidc.eks.example.invalid/id/OTHER")

	_, err := New(context.Background(), issuer.URL(), testAudience)
	if err == nil {
		t.Fatal("New: expected an error when the document names another issuer")
	}
	if !strings.Contains(err.Error(), issuer.URL()) {
		t.Errorf("New error %q does not name the issuer URL %s", err, issuer.URL())
	}
}

func TestLooksLikeJWT(t *testing.T) {
	issuer := oidctest.New(t)
	signing := oidctest.NewKey(t, "signing-key")
	issuer.Publish(signing)
	minted := issuer.Mint(t, signing, issuer.Claims(testSubject, testAudience))

	secret := []byte("0123456789abcdef0123456789abcdef")

	tests := []struct {
		name  string
		token string
		want  bool
	}{
		{name: "a minted token", token: minted, want: true},
		{name: "a Dispatch personal token", token: "dsp_" + base64.RawURLEncoding.EncodeToString(secret), want: false},
		{name: "a shared token", token: hex.EncodeToString(secret), want: false},
		{name: "an empty bearer", token: "", want: false},
		{name: "an empty segment", token: "aaa..ccc", want: false},
		{name: "four segments", token: "aaa.bbb.ccc.ddd", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := LooksLikeJWT(tt.token); got != tt.want {
				t.Errorf("LooksLikeJWT(%q) = %v, want %v", tt.token, got, tt.want)
			}
		})
	}
}
