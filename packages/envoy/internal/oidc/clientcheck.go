package oidc

import (
	"context"
	"errors"
	"fmt"

	gooidc "github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/sjawhar/envoy/internal/oauthrefusal"
)

// ClientCheck is what one check of a code flow's client at its issuer found. The three words are
// the answer a caller publishes, so they are a closed set: a check that cannot tell says
// ClientCheckIssuerUnreachable rather than inventing a fourth.
type ClientCheck string

const (
	// ClientCheckReady: the issuer knows the client and took its secret. It answered the check's
	// token request invalid_grant at a refusal's status (oauthrefusal.Refusal), which is a refusal
	// of the code alone (RFC 6749 section 5.2).
	ClientCheckReady ClientCheck = "ready"
	// ClientCheckRejected: the issuer refused at a refusal's status (oauthrefusal.Refused) for
	// any reason but the code itself - invalid_client, another OAuth error or none: the client was
	// deleted, its id is not this issuer's, or its secret has been rotated out from under the
	// caller. No person can sign in until it is fixed.
	ClientCheckRejected ClientCheck = "client_rejected"
	// ClientCheckIssuerUnreachable: the issuer could not be asked or could not answer, or answered
	// something the check cannot read as either of the two above. It says nothing about the
	// client; the error a check returns with it says which it was.
	ClientCheckIssuerUnreachable ClientCheck = "issuer_unreachable"
)

// CheckClient asks issuer whether it still accepts clientID with clientSecret: it reads the
// issuer's discovery document and sends its token endpoint one authorization_code request with
// the client's credentials and a code that does not exist.
//
// An issuer that knows the client and takes its secret gets as far as the code and refuses that,
// invalid_grant, which is ClientCheckReady. The status alone decides the rest (oauthrefusal). A
// refusal for any other reason or none, a 4xx that is no 408 or 429 naming invalid_client, another
// OAuth error or no error at all, is the issuer answering and refusing the client, which is
// ClientCheckRejected. A 5xx, a 408 or a 429, whatever error its body names, and a discovery or
// transport failure, are an issuer that could not answer; a 2xx naming an error and a 3xx are an
// answer that refuses nothing. Both are ClientCheckIssuerUnreachable, the closed set's one word for
// a check with no verdict, which must not report one; its error says whether the issuer could not
// answer or answered with none.
//
// The request names no redirect_uri, since no code of this client's exists for one to match, and
// the code is freshly random, so no live code is ever spent by a check. The discovery document is
// read on every call rather than kept, because a check an hour apart is asking, among other
// things, whether the issuer is still there.
//
// The error is nil only for ClientCheckReady. For the other two it says what happened, for the
// caller's log; it never carries the client secret, and a caller publishing the check publishes
// the word alone.
func CheckClient(ctx context.Context, issuer, clientID, clientSecret string) (ClientCheck, error) {
	// One bounded client for both requests: the issuer's discovery read and the token request.
	client := tokenClient()
	flow, err := discoverCodeFlow(gooidc.ClientContext(ctx, client), issuer, clientID, clientSecret)
	if err != nil {
		return ClientCheckIssuerUnreachable, err
	}
	// GenerateVerifier is PKCE's random high-entropy string, which is all this needs of it: a
	// code no issuer has ever issued.
	_, err = flow.config.Exchange(context.WithValue(ctx, oauth2.HTTPClient, client), oauth2.GenerateVerifier())
	var retrieve *oauth2.RetrieveError
	switch {
	case err == nil:
		// An issuer that exchanged a code it never issued has a problem of its own, but it has
		// plainly accepted the client, which is the one question this asks.
		return ClientCheckReady, nil
	case !errors.As(err, &retrieve) || retrieve.Response == nil:
		return ClientCheckIssuerUnreachable, fmt.Errorf("oidc: check the client at %s: %w", issuer, err)
	case oauthrefusal.Refusal(err) == "invalid_grant":
		return ClientCheckReady, nil
	case oauthrefusal.Refused(err):
		return ClientCheckRejected, fmt.Errorf("oidc: issuer %s refused the client %s: HTTP %d with the OAuth error %q",
			issuer, clientID, retrieve.Response.StatusCode, retrieve.ErrorCode)
	case oauthrefusal.Outage(retrieve.Response.StatusCode):
		return ClientCheckIssuerUnreachable, fmt.Errorf("oidc: issuer %s could not answer the client check: HTTP %d: %w", issuer, retrieve.Response.StatusCode, err)
	}
	return ClientCheckIssuerUnreachable, fmt.Errorf("oidc: issuer %s answered the client check HTTP %d with the OAuth error %q and no refusal, which says nothing about the client: %w",
		issuer, retrieve.Response.StatusCode, retrieve.ErrorCode, err)
}
