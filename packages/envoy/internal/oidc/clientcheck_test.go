package oidc

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/oidc/oidctest"
	"github.com/sjawhar/envoy/internal/testwait"
)

const (
	checkClientID     = "broker-client"
	checkClientSecret = "broker-client-secret"
)

// TestCheckClientReadsTheIssuersAnswerAboutTheClient pins the whole vocabulary of a client check
// against a real issuer: the client the issuer serves is ready, since the issuer gets as far as
// the code the check invents and refuses that alone; a client id it does not serve, and a secret
// that is not the client's, are client_rejected; and an issuer that will not answer at all is
// issuer_unreachable, which says nothing about the client either way.
func TestCheckClientReadsTheIssuersAnswerAboutTheClient(t *testing.T) {
	issuer := oidctest.New(t)
	issuer.EnableCodeFlow(issuer.PublishKey(t, "signing-key"), checkClientID, checkClientSecret)
	ctx := context.Background()

	for _, c := range []struct {
		name             string
		clientID, secret string
		want             ClientCheck
		wantErr          bool
	}{
		{"the issuer's own client", checkClientID, checkClientSecret, ClientCheckReady, false},
		{"a client id the issuer does not serve", "another-client", checkClientSecret, ClientCheckRejected, true},
		{"the client id with the wrong secret", checkClientID, "not-the-secret", ClientCheckRejected, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			check, err := CheckClient(ctx, issuer.URL(), c.clientID, c.secret, WithWaitArm(testwait.BoundArm(t)))
			if check != c.want || (err != nil) != c.wantErr {
				t.Fatalf("CheckClient = (%q, %v), want %q with an error: %t", check, err, c.want, c.wantErr)
			}
			if err != nil && strings.Contains(err.Error(), c.secret) {
				t.Fatalf("the check's error carries the client secret: %v", err)
			}
		})
	}

	t.Run("an issuer with no discovery document", func(t *testing.T) {
		down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "no", http.StatusInternalServerError)
		}))
		defer down.Close()
		check, err := CheckClient(ctx, down.URL, checkClientID, checkClientSecret)
		if check != ClientCheckIssuerUnreachable || err == nil {
			t.Fatalf("CheckClient against an issuer that answers no discovery document = (%q, %v), want %q and an error",
				check, err, ClientCheckIssuerUnreachable)
		}
	})

	t.Run("a token endpoint that answers something else", func(t *testing.T) {
		// Neither invalid_grant nor invalid_client: the check has no verdict on the client and
		// must not invent one.
		issuer.FailToken(http.StatusInternalServerError, "temporarily_unavailable")
		defer issuer.FailToken(0, "")
		check, err := CheckClient(ctx, issuer.URL(), checkClientID, checkClientSecret)
		if check != ClientCheckIssuerUnreachable || err == nil {
			t.Fatalf("CheckClient against a token endpoint answering 500 = (%q, %v), want %q and an error",
				check, err, ClientCheckIssuerUnreachable)
		}
	})

	t.Run("a token endpoint that answers nothing", func(t *testing.T) {
		issuer.DropToken()
		defer issuer.FailToken(0, "")
		check, err := CheckClient(ctx, issuer.URL(), checkClientID, checkClientSecret)
		if check != ClientCheckIssuerUnreachable || err == nil {
			t.Fatalf("CheckClient against a token endpoint that drops the connection = (%q, %v), want %q and an error",
				check, err, ClientCheckIssuerUnreachable)
		}
	})
}

// TestCheckClientIsOneTokenRequest pins that a check asks the token endpoint once, the way the
// issuer's discovery document lists, whatever the verdict: a ready client, whose invented code the
// issuer refuses, and a rejected one are each one request, by HTTP Basic under Google's list (the
// test issuer's own) and in the form under a list naming client_secret_post alone. A client that
// guessed its style would ask by HTTP Basic, be refused, and ask again in the form.
func TestCheckClientIsOneTokenRequest(t *testing.T) {
	for _, c := range []struct {
		name    string
		methods []string // nil keeps the test issuer's own list, Google's
		secret  string
		want    ClientCheck
		sent    string
	}{
		{"a ready client, Google's list", nil, checkClientSecret, ClientCheckReady, "client_secret_basic"},
		{"a rejected client, Google's list", nil, "not-the-secret", ClientCheckRejected, "client_secret_basic"},
		{"a ready client, client_secret_post alone", []string{"client_secret_post"}, checkClientSecret, ClientCheckReady, "client_secret_post"},
		{"a rejected client, client_secret_post alone", []string{"client_secret_post"}, "not-the-secret", ClientCheckRejected, "client_secret_post"},
	} {
		t.Run(c.name, func(t *testing.T) {
			issuer := oidctest.New(t)
			issuer.EnableCodeFlow(issuer.PublishKey(t, "signing-key"), checkClientID, checkClientSecret)
			if c.methods != nil {
				issuer.AdvertiseTokenAuthMethods(c.methods...)
			}
			if check, err := CheckClient(context.Background(), issuer.URL(), checkClientID, c.secret, WithWaitArm(testwait.BoundArm(t))); check != c.want {
				t.Fatalf("CheckClient = (%q, %v), want %q", check, err, c.want)
			}
			if got := issuer.TokenRequests(); !slices.Equal(got, []string{c.sent}) {
				t.Fatalf("the check's token requests = %q, want one, %s", got, c.sent)
			}
		})
	}
}

// TestCheckClientSpendsNoCodeOfItsOwnIssuer pins that the code a check invents is its own: a
// sign-in's code issued before the check is still exchangeable after it.
func TestCheckClientSpendsNoCodeOfItsOwnIssuer(t *testing.T) {
	issuer := oidctest.New(t)
	key := issuer.PublishKey(t, "signing-key")
	issuer.EnableCodeFlow(key, checkClientID, checkClientSecret)
	flow := NewCodeFlow(issuer.URL(), checkClientID, checkClientSecret, "", WithWaitArm(testwait.BoundArm(t)))
	ctx := context.Background()
	signIn := OnlineSignIn{State: "state", Nonce: "nonce", Verifier: "verifier-verifier-verifier-verifier-ve"}
	redirectURL := "https://broker.example/signin/callback"
	authURL, err := flow.OnlineAuthURL(ctx, redirectURL, signIn)
	if err != nil {
		t.Fatalf("OnlineAuthURL: %v", err)
	}
	issuer.SignInAs(map[string]any{"sub": "sub-ada", "email": "ada@example.com", "email_verified": true})
	code, _ := issuer.Authorize(t, authURL)
	if code == "" {
		t.Fatal("the issuer issued no code")
	}

	if check, err := CheckClient(ctx, issuer.URL(), checkClientID, checkClientSecret, WithWaitArm(testwait.BoundArm(t))); check != ClientCheckReady || err != nil {
		t.Fatalf("CheckClient = (%q, %v), want ready", check, err)
	}
	if _, err := flow.OnlineExchange(ctx, redirectURL, code, signIn); err != nil {
		t.Fatalf("the sign-in's own code after a client check: %v", err)
	}
}
