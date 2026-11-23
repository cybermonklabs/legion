package oauth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"testing"
	"time"

	"authelia.com/provider/oauth2"
	"authelia.com/provider/oauth2/handler/rfc8693"
	"authelia.com/provider/oauth2/token/jwt"
	gojose "github.com/go-jose/go-jose/v4"

	"github.com/sjawhar/envoy/internal/cipher/oauthstore"
	"github.com/sjawhar/envoy/internal/oidc"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
)

// tokenShape is a JWT token type as the library is configured with it: its issuer ("" for the
// cluster type, which the library compares with nothing), its lifetime window and whether it holds a
// token to one presentation.
type tokenShape struct {
	name, issuer string
	window       time.Duration
	jti          bool
}

// The three shapes the exchange registers: GitHub's (an issuer, an hour, one presentation),
// Google's (an issuer, an hour; the person and machine-actor types are the same shape) and a
// cluster's (no issuer, kubernetesTolerance).
var tokenShapes = []tokenShape{
	{name: "github", issuer: "https://token.actions.githubusercontent.example", window: githubTolerance, jti: true},
	{name: "google", issuer: "https://accounts.google.example", window: googleTolerance},
	{name: "cluster", window: kubernetesTolerance},
}

// libraryStorage is the replay store and the subject decision the library's handler asks for, and
// nothing else: spent is the jtis already presented, and every subject token is "subject".
type libraryStorage struct {
	rfc8693.Storage
	spent map[string]bool
}

func (s *libraryStorage) SetTokenExchangeCustomJWT(_ context.Context, jti string, _ time.Time) error {
	if s.spent[jti] {
		return errors.New("presented before")
	}
	s.spent[jti] = true
	return nil
}

func (*libraryStorage) GetSubjectForTokenExchange(context.Context, oauth2.Requester, map[string]any) (string, error) {
	return "subject", nil
}

// judgedByTheLibrary runs the library's own handler (authelia.com/provider/oauth2 v0.3.3,
// rfc8693.CustomJWTTypeHandler) on a token of shape carrying claims, signed with key, its key
// check stubbed so the signature always verifies, and answers the library's verdict. The stub runs
// refusedWithoutKey and refusedForLifetime on the very claims the library parsed, at the moment it
// judges them, and answers theirs too. spent is the jtis the replay store holds already.
func judgedByTheLibrary(t *testing.T, key *rsa.PrivateKey, shape tokenShape, claims map[string]any, spent ...string) (library, mirror, lifetime error) {
	t.Helper()
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal %v: %v", claims, err)
	}
	signer, err := gojose.NewSigner(gojose.SigningKey{Algorithm: gojose.RS256, Key: gojose.JSONWebKey{Key: key, KeyID: "signing-key"}}, (&gojose.SignerOptions{}).WithType("JWT"))
	if err != nil {
		t.Fatal(err)
	}
	signed, err := signer.Sign(payload)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := signed.CompactSerialize()
	if err != nil {
		t.Fatal(err)
	}
	typ := &rfc8693.JWTType{Name: shape.name, Issuer: shape.issuer, JWTValidationConfig: rfc8693.JWTValidationConfig{
		ValidateJTI: shape.jti, JWTLifetimeToleranceWindow: shape.window,
	}}
	judged := false
	typ.ValidateFunc = func(token *jwt.Token) (any, error) {
		now := time.Now()
		mirror = refusedWithoutKey(token.Claims, typ, now)
		lifetime = refusedForLifetime(token.Claims, typ.JWTLifetimeToleranceWindow, now)
		judged = true
		return &key.PublicKey, nil
	}
	storage := &libraryStorage{spent: map[string]bool{}}
	for _, jti := range spent {
		storage.spent[jti] = true
	}
	handler := &rfc8693.CustomJWTTypeHandler{Config: &oauth2.Config{RFC8693TokenTypes: map[string]oauth2.RFC8693TokenType{shape.name: typ}}, Storage: storage}
	request := oauth2.NewAccessRequest(oauthstore.NewExchangeSession())
	request.GrantTypes = oauth2.Arguments{string(oauth2.GrantTypeTokenExchange)}
	request.Form = url.Values{"subject_token": {raw}, "subject_token_type": {shape.name}}
	library = handler.HandleTokenEndpointRequest(context.Background(), request)
	if !judged {
		t.Fatalf("the library refused %v before its key function ran (%v), so nothing was compared", claims, library)
	}
	return library, mirror, lifetime
}

// claimsRow is one claim set the tests below send through the library: change spoils a valid
// token's claims, and spent names a jti the replay store holds already.
type claimsRow struct {
	name   string
	change func(claims map[string]any, shape tokenShape, now time.Time)
	spent  string
}

// validClaims is a token of shape that every check admits: issued a minute ago, expiring in ten,
// with a jti of its own.
func validClaims(shape tokenShape, now time.Time, jti string) map[string]any {
	iss := shape.issuer
	if iss == "" {
		iss = "https://cluster.example"
	}
	return map[string]any{
		"iss": iss, "sub": "example-subject", "aud": "example-audience", "jti": jti,
		"iat": now.Add(-time.Minute).Unix(), "exp": now.Add(10 * time.Minute).Unix(),
	}
}

// setClaim is a change setting the claim name to value, a JSON number written as given where value
// is a json.Number, so the token carries exactly that text.
func setClaim(name string, value any) func(map[string]any, tokenShape, time.Time) {
	return func(claims map[string]any, _ tokenShape, _ time.Time) { claims[name] = value }
}

// TestRefusedWithoutKeyMirrorsTheLibrarysChecks: for each claim set, on each shape the exchange
// registers, refusedWithoutKey refuses exactly when the library's own handler refuses the token
// with its key check stubbed, so a hand table of what the library does is never what is compared.
// The rows are the conversions the library makes of exp (a value under 1, 0, 0.0, null, one that
// overflows time.Unix, MaxInt64, a negative, a string), of iat and nbf, iss and jti. Its residual
// rows are the library's refusals refusedWithoutKey leaves to it on purpose, and each must be one
// the library makes: an iat or an nbf in the future, which a wait can make valid, and a spent jti,
// which the replay store judges (unjudgedSubject).
func TestRefusedWithoutKeyMirrorsTheLibrarysChecks(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	rows := []claimsRow{
		{name: "a live token", change: func(map[string]any, tokenShape, time.Time) {}},
		{name: "an exp of 0.4", change: setClaim("exp", json.Number("0.4"))},
		{name: "an exp of 0.999", change: setClaim("exp", json.Number("0.999"))},
		{name: "an exp of 0.5", change: setClaim("exp", json.Number("0.5"))},
		{name: "an exp of 9223371974719179008", change: setClaim("exp", json.Number("9223371974719179008"))},
		{name: "an exp of 9223372000000000000", change: setClaim("exp", json.Number("9223372000000000000"))},
		{name: "an exp of 9223372036854774784", change: setClaim("exp", json.Number("9223372036854774784"))},
		{name: "an exp of null", change: setClaim("exp", nil)},
		{name: "an exp of 0", change: setClaim("exp", json.Number("0"))},
		{name: "an exp of 0.0", change: setClaim("exp", json.Number("0.0"))},
		{name: "an exp of MaxInt64", change: setClaim("exp", json.Number("9223372036854775807"))},
		{name: "an exp of 1e19", change: setClaim("exp", json.Number("1e19"))},
		{name: "a negative exp", change: setClaim("exp", json.Number("-5"))},
		{name: "an exp of -0.4", change: setClaim("exp", json.Number("-0.4"))},
		{name: "an exp that is a word", change: setClaim("exp", "tomorrow")},
		{name: "an exp that is a numeric string", change: setClaim("exp", "1800000000")},
		{name: "an exp that is a bool", change: setClaim("exp", true)},
		{name: "no exp", change: func(claims map[string]any, _ tokenShape, _ time.Time) { delete(claims, "exp") }},
		{name: "an exp five minutes past", change: func(claims map[string]any, _ tokenShape, now time.Time) {
			claims["iat"], claims["exp"] = now.Add(-20*time.Minute).Unix(), now.Add(-5*time.Minute).Unix()
		}},
		{name: "an exp with a fraction, live", change: func(claims map[string]any, _ tokenShape, now time.Time) {
			claims["exp"] = json.Number(fmt.Sprintf("%d.75", now.Add(10*time.Minute).Unix()))
		}},
		{name: "an exp a minute inside the window", change: func(claims map[string]any, shape tokenShape, now time.Time) {
			claims["exp"] = now.Add(shape.window - time.Minute).Unix()
		}},
		{name: "an exp a minute past the window", change: func(claims map[string]any, shape tokenShape, now time.Time) {
			claims["exp"] = now.Add(shape.window + time.Minute).Unix()
		}},
		{name: "an iat older than the window", change: func(claims map[string]any, shape tokenShape, now time.Time) {
			claims["iat"] = now.Add(-shape.window - time.Minute).Unix()
		}},
		{name: "an iat of null", change: setClaim("iat", nil)},
		{name: "an iat that is a word", change: setClaim("iat", "yesterday")},
		{name: "no iat", change: func(claims map[string]any, _ tokenShape, _ time.Time) { delete(claims, "iat") }},
		{name: "an nbf that is a word", change: setClaim("nbf", "soon")},
		{name: "an nbf a minute past", change: func(claims map[string]any, _ tokenShape, now time.Time) { claims["nbf"] = now.Add(-time.Minute).Unix() }},
		{name: "an iss of another issuer", change: setClaim("iss", "https://issuer.elsewhere.example")},
		{name: "an iss with a trailing slash", change: func(claims map[string]any, shape tokenShape, _ time.Time) { claims["iss"] = shape.issuer + "/" }},
		{name: "no iss", change: func(claims map[string]any, _ tokenShape, _ time.Time) { delete(claims, "iss") }},
		{name: "an iss that is a number", change: setClaim("iss", 7)},
		{name: "no jti", change: func(claims map[string]any, _ tokenShape, _ time.Time) { delete(claims, "jti") }},
		{name: "an empty jti", change: setClaim("jti", "")},
		{name: "a jti that is a number", change: setClaim("jti", 4242)},
		{name: "residual: an iat ten minutes ahead", change: func(claims map[string]any, _ tokenShape, now time.Time) {
			claims["iat"] = now.Add(10 * time.Minute).Unix()
		}},
		{name: "residual: an nbf ten minutes ahead", change: func(claims map[string]any, _ tokenShape, now time.Time) {
			claims["nbf"] = now.Add(10 * time.Minute).Unix()
		}},
		{name: "residual: a spent jti", spent: "spent-jti", change: setClaim("jti", "spent-jti")},
	}
	for _, shape := range tokenShapes {
		for i, row := range rows {
			t.Run(shape.name+": "+row.name, func(t *testing.T) {
				now := time.Now()
				claims := validClaims(shape, now, fmt.Sprintf("jti-%s-%d", shape.name, i))
				row.change(claims, shape, now)
				var spent []string
				if row.spent != "" {
					spent = append(spent, row.spent)
				}
				library, mirror, _ := judgedByTheLibrary(t, key, shape, claims, spent...)
				switch {
				case row.name == "a live token" && library != nil:
					t.Fatalf("the library refused the live token (%v), so no row tells a refusal from an admission", library)
				case len(row.name) > len("residual: ") && row.name[:len("residual: ")] == "residual: ":
					residual := row.spent == "" || shape.jti
					if residual && (library == nil || mirror != nil) {
						t.Fatalf("library = %v, refusedWithoutKey = %v; want the library to refuse a token refusedWithoutKey leaves to it", library, mirror)
					}
					if !residual && (library == nil) != (mirror == nil) {
						t.Fatalf("library = %v, refusedWithoutKey = %v; want the same verdict", library, mirror)
					}
				case (library == nil) != (mirror == nil):
					t.Fatalf("library = %v, refusedWithoutKey = %v; want the same verdict", library, mirror)
				}
			})
		}
	}
}

// TestRefusedForLifetimeRefusesATokenThatNeverExpires: refusedForLifetime refuses a token whose
// exp the library reads as no exp (missing, null, 0, 0.0, any value under 1) or as further ahead
// than the type's longest lifetime, the values that overflow the library's window check
// (time.Unix) included. For each of those but the missing exp and the exp past the window, the
// library's own handler admits the token, so without refusedForLifetime the broker would mint an
// access token for a token that never expires. A live token, one with a fraction, and one a minute
// inside the window it admits.
//
// Two overflow rows are there to tell refusedForLifetime's whole-second comparison from a
// time.Unix one. 9223371974719179008 overflows the library's integer reading, but refusedForLifetime
// reads exp through a float64 (jwt.NumericDate), which rounds it to 9223371974719178752, below the
// point where time.Unix overflows (MaxInt64 - 62135596800), so a time.Unix comparison refuses it
// too. 9223372000000000000 and 9223372036854774784 stay above that point after the rounding, so a
// time.Unix comparison overflows and admits them, as the library does.
func TestRefusedForLifetimeRefusesATokenThatNeverExpires(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	for _, shape := range tokenShapes {
		for i, row := range []struct {
			claimsRow
			refused, libraryAdmits bool
		}{
			{claimsRow{name: "a live token", change: func(map[string]any, tokenShape, time.Time) {}}, false, true},
			{claimsRow{name: "an exp with a fraction, live", change: func(claims map[string]any, _ tokenShape, now time.Time) {
				claims["exp"] = json.Number(fmt.Sprintf("%d.75", now.Add(10*time.Minute).Unix()))
			}}, false, true},
			{claimsRow{name: "an exp a minute inside the window", change: func(claims map[string]any, shape tokenShape, now time.Time) {
				claims["exp"] = now.Add(shape.window - time.Minute).Unix()
			}}, false, true},
			{claimsRow{name: "an exp of null", change: setClaim("exp", nil)}, true, true},
			{claimsRow{name: "an exp of 0", change: setClaim("exp", json.Number("0"))}, true, true},
			{claimsRow{name: "an exp of 0.0", change: setClaim("exp", json.Number("0.0"))}, true, true},
			{claimsRow{name: "an exp of 0.4", change: setClaim("exp", json.Number("0.4"))}, true, true},
			{claimsRow{name: "an exp of 0.999", change: setClaim("exp", json.Number("0.999"))}, true, true},
			{claimsRow{name: "an exp of 9223371974719179008", change: setClaim("exp", json.Number("9223371974719179008"))}, true, true},
			{claimsRow{name: "an exp of 9223372000000000000", change: setClaim("exp", json.Number("9223372000000000000"))}, true, true},
			{claimsRow{name: "an exp of 9223372036854774784", change: setClaim("exp", json.Number("9223372036854774784"))}, true, true},
			{claimsRow{name: "no exp", change: func(claims map[string]any, _ tokenShape, _ time.Time) { delete(claims, "exp") }}, true, false},
			{claimsRow{name: "an exp a minute past the window", change: func(claims map[string]any, shape tokenShape, now time.Time) {
				claims["exp"] = now.Add(shape.window + time.Minute).Unix()
			}}, true, false},
		} {
			t.Run(shape.name+": "+row.name, func(t *testing.T) {
				now := time.Now()
				claims := validClaims(shape, now, fmt.Sprintf("lifetime-%s-%d", shape.name, i))
				row.change(claims, shape, now)
				library, _, lifetime := judgedByTheLibrary(t, key, shape, claims)
				if (library == nil) != row.libraryAdmits {
					t.Fatalf("the library's verdict is %v, want it to admit the token: %v", library, row.libraryAdmits)
				}
				if (lifetime != nil) != row.refused {
					t.Fatalf("refusedForLifetime = %v, want refused %v", lifetime, row.refused)
				}
				if lifetime != nil && !errors.Is(lifetime, errNeverExpires) {
					t.Fatalf("refusedForLifetime = %v, want errNeverExpires", lifetime)
				}
			})
		}
	}
}

// TestAClusterTokenIsJudgedThroughTheLazyVerifiersErrors: the cluster type's judge reads a cluster's
// keys through oidc.LazyVerifier in production (loggedIssuers.keys, IssuerSet.Key), whose error is
// not the plain key set's: it wraps oidc.ErrKeySet in oidc.ErrIssuerUnavailable once the discovery
// document has been read, and is oidc.ErrIssuerUnavailable alone before. Through both, a live
// token is answered ErrIssuerUnavailable, which exchangeGrant answers 503, and an expired token is
// refused (errRefusedWithoutKey) without ErrIssuerUnavailable, which it answers 400.
func TestAClusterTokenIsJudgedThroughTheLazyVerifiersErrors(t *testing.T) {
	keysDown, discoveryDown := oidctest.New(t), oidctest.New(t)
	keysDown.PublishKey(t, "cluster-key")
	keysDown.FailKeys(http.StatusServiceUnavailable)
	discoveryDown.FailDiscovery(http.StatusServiceUnavailable)
	for _, source := range []struct {
		name   string
		issuer *oidctest.Issuer
		keySet bool
	}{
		{"its key set cannot be read", keysDown, true},
		{"its discovery document cannot be read", discoveryDown, false},
	} {
		judge := newTokenJudge(&rfc8693.JWTType{Name: "cluster", JWTValidationConfig: rfc8693.JWTValidationConfig{JWTLifetimeToleranceWindow: kubernetesTolerance}},
			[]IssuerConfig{{Name: "c", Issuer: source.issuer.URL(), Keys: oidc.NewLazyVerifier(source.issuer.URL(), "example-audience")}}, nil)
		now := time.Now()
		for _, row := range []struct {
			name      string
			exp       time.Time
			answer503 bool
		}{
			{"a live token", now.Add(10 * time.Minute), true},
			{"an expired token", now.Add(-10 * time.Minute), false},
		} {
			t.Run(source.name+": "+row.name, func(t *testing.T) {
				token := &jwt.Token{KeyID: "unseen-key", SignatureAlgorithm: subjectTokenAlgorithms[0], Claims: jwt.MapClaims{
					"iss": source.issuer.URL(), "iat": now.Add(-20 * time.Minute).Unix(), "exp": row.exp.Unix(),
				}}
				_, err := judge.validate(token)
				if got := errors.Is(err, oidc.ErrIssuerUnavailable); got != row.answer503 {
					t.Fatalf("validate = %v; matches oidc.ErrIssuerUnavailable %v, want %v", err, got, row.answer503)
				}
				if row.answer503 && errors.Is(err, oidc.ErrKeySet) != source.keySet {
					t.Fatalf("validate = %v; matches oidc.ErrKeySet %v, want %v: the LazyVerifier answered another shape of error than the row names", err, !source.keySet, source.keySet)
				}
				if !row.answer503 && !errors.Is(err, errRefusedWithoutKey) {
					t.Fatalf("validate = %v, want errRefusedWithoutKey", err)
				}
			})
		}
	}
}
