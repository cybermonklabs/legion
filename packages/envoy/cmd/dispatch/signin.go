package main

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/identity"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/oidc"
	"github.com/sjawhar/envoy/internal/redact"
)

// plainRefreshTokenRetireTimeout bounds the boot's retirement of the refresh tokens people holds
// in plain text, so a sign-in pool that does not answer delays the start by this much at most.
const plainRefreshTokenRetireTimeout = 30 * time.Second

// googleIssuer is Google's OpenID Connect issuer, the one issuer whose ID tokens carry `hd`, the
// Workspace domain of the account. go-oidc also accepts a token whose `iss` is Google's schemeless
// `accounts.google.com` when this is the configured issuer, as Google documents.
const googleIssuer = "https://accounts.google.com"

// domainShape is a lowercase DNS name of two or more labels, each 1 to 63 letters, digits and
// inner hyphens: the shape of a Google Workspace domain.
var domainShape = regexp.MustCompile(`^(?:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)

// maxDomainLength is the longest DNS name, in its dotted text form.
const maxDomainLength = 253

// notALowercaseDomain is what a refusal of DISPATCH_SIGNIN_DOMAIN names, after the value's length
// (redact.Refusal): the value is never echoed, so a secret pasted into the wrong variable stays out
// of the boot log.
const notALowercaseDomain redact.Misreading = "is not a lowercase domain name such as example.com"

// iapAudienceShape is the shape of every audience Identity-Aware Proxy signs for,
// /projects/<project number>/ (https://cloud.google.com/iap/docs/signed-headers-howto) and then a
// load balancer's backend service, an App Engine app or a Cloud Run service, named in letters,
// digits, '.', '_', '-' and '/'. Nothing after the project number may hold an @ or a :, so a URL
// with a user part written after it (/projects/<n>/https://<user>:<password>@host) is refused like
// one pasted at the start, and the boot line, which logs the audience, never holds a user part.
var iapAudienceShape = regexp.MustCompile(`^/projects/[0-9]+/[A-Za-z0-9._/-]+$`)

// notAnIAPAudience is what a refusal of DISPATCH_IAP_AUDIENCE names, after the value's length
// (redact.Refusal): a value of another shape may be a URL pasted into the wrong variable, user part
// and all, so it is never echoed.
const notAnIAPAudience redact.Misreading = "is not the audience IAP signs its assertions for, which begins /projects/<project number>/ and holds only letters, digits, ., _, - and / after it, such as /projects/<project number>/global/backendServices/<backend service id>"

// signInClientSettings are the three settings every Google sign-in takes, each with boot's value:
// the issuer and Dispatch's confidential client there.
func signInClientSettings(boot bootConfig) []struct{ name, value string } {
	return []struct{ name, value string }{
		{"DISPATCH_SIGNIN_ISSUER", boot.SignInIssuer},
		{"DISPATCH_SIGNIN_CLIENT_ID", boot.SignInClientID},
		{"DISPATCH_SIGNIN_CLIENT_SECRET", boot.SignInClientSecret},
	}
}

// signInSettings are all five sign-in settings, each with boot's value: the client's three, then
// the group a Cognito sign-in pool admits and the Workspace domain Google admits, of which a
// sign-in takes exactly one.
func signInSettings(boot bootConfig) []struct{ name, value string } {
	return append(signInClientSettings(boot),
		struct{ name, value string }{"DISPATCH_SIGNIN_GROUP", boot.SignInGroup},
		struct{ name, value string }{"DISPATCH_SIGNIN_DOMAIN", boot.SignInDomain},
	)
}

// signInRuleChoice is how a refusal names the group-or-domain pair as one item.
const signInRuleChoice = "DISPATCH_SIGNIN_GROUP or DISPATCH_SIGNIN_DOMAIN"

// signInSettingNames is what a Google sign-in takes, as a refusal lists it: "A, B, C and one of D
// or E".
func signInSettingNames() string {
	settings := signInClientSettings(bootConfig{})
	names := make([]string, len(settings))
	for i, setting := range settings {
		names[i] = setting.name
	}
	return strings.Join(names, ", ") + " and one of " + signInRuleChoice
}

// checkSignInSettings refuses the sign-in settings identityMode (DISPATCH_IDENTITY) cannot take.
// Any of them beside header identity, which is only for tests and local harnesses, is refused
// first. IAP identity takes DISPATCH_SIGNIN_DOMAIN alone (checkIAPSignInSettings). Then the first
// of these refuses: a group and a domain both set; some of the settings but not the issuer, client
// ID, client secret and one of the group or domain; a domain under an issuer other than Google's,
// whose tokens carry no `hd`; a domain that is not a lowercase DNS name; a group under Google's
// issuer, whose tokens carry no `cognito:groups`. A deployment with the issuer, client ID, client
// secret and group of a Cognito pool reaches none of them.
func checkSignInSettings(boot bootConfig, identityMode string) error {
	var set []string
	for _, setting := range signInSettings(boot) {
		if setting.value != "" {
			set = append(set, setting.name)
		}
	}
	if strings.HasPrefix(identityMode, "header:") && len(set) > 0 {
		return fmt.Errorf("%s must be unset because header identity and Google sign-in cannot share a deployment: header identity is only for tests and local harnesses", strings.Join(set, ", "))
	}
	if identityMode == "iap" {
		return checkIAPSignInSettings(boot)
	}
	if boot.SignInGroup != "" && boot.SignInDomain != "" {
		return errors.New("DISPATCH_SIGNIN_GROUP and DISPATCH_SIGNIN_DOMAIN are both set; set one: a Cognito pool's group, or a Google Workspace domain")
	}
	if len(set) > 0 {
		var missing []string
		for _, setting := range signInClientSettings(boot) {
			if setting.value == "" {
				missing = append(missing, setting.name)
			}
		}
		if boot.SignInGroup == "" && boot.SignInDomain == "" {
			if len(missing) == 0 {
				return fmt.Errorf("Google sign-in needs %s: %s set, DISPATCH_SIGNIN_GROUP and DISPATCH_SIGNIN_DOMAIN missing", signInSettingNames(), strings.Join(set, ", "))
			}
			missing = append(missing, signInRuleChoice)
		}
		if len(missing) > 0 {
			return fmt.Errorf("Google sign-in needs %s: %s set, %s missing", signInSettingNames(), strings.Join(set, ", "), strings.Join(missing, ", "))
		}
	}
	if boot.SignInDomain != "" {
		if boot.SignInIssuer != googleIssuer {
			return fmt.Errorf("DISPATCH_SIGNIN_DOMAIN needs DISPATCH_SIGNIN_ISSUER %s, since only Google's tokens carry hd; DISPATCH_SIGNIN_ISSUER is %s", googleIssuer, redact.URL(boot.SignInIssuer))
		}
		if err := checkDomainShape(boot.SignInDomain); err != nil {
			return err
		}
	}
	if boot.SignInGroup != "" && boot.SignInIssuer == googleIssuer {
		return fmt.Errorf("DISPATCH_SIGNIN_GROUP is a Cognito pool's group, and a token from %s carries none; set DISPATCH_SIGNIN_DOMAIN instead", googleIssuer)
	}
	return nil
}

// checkIAPSignInSettings refuses the sign-in settings IAP identity cannot take. Identity-Aware
// Proxy signs people in, so Dispatch runs no sign-in of its own: the issuer, client ID, client
// secret and group are refused, each by name. DISPATCH_SIGNIN_DOMAIN is required, a lowercase DNS
// name, since it is who may use Dispatch: an account whose assertion's hd is that domain.
func checkIAPSignInSettings(boot bootConfig) error {
	var refused []string
	for _, setting := range append(signInClientSettings(boot), struct{ name, value string }{"DISPATCH_SIGNIN_GROUP", boot.SignInGroup}) {
		if setting.value != "" {
			refused = append(refused, setting.name)
		}
	}
	if len(refused) > 0 {
		return fmt.Errorf("%s must be unset under DISPATCH_IDENTITY=iap: Identity-Aware Proxy signs people in, so Dispatch runs no sign-in of its own and admits the people of DISPATCH_SIGNIN_DOMAIN", strings.Join(refused, ", "))
	}
	if boot.SignInDomain == "" {
		return errors.New("DISPATCH_IDENTITY=iap needs DISPATCH_SIGNIN_DOMAIN: the Google Workspace domain whose accounts may use Dispatch, which each assertion's hd must name")
	}
	return checkDomainShape(boot.SignInDomain)
}

// checkDomainShape refuses a DISPATCH_SIGNIN_DOMAIN that is not a lowercase DNS name, naming its
// length and never its value (redact.Refusal).
func checkDomainShape(domain string) error {
	if len(domain) > maxDomainLength || !domainShape.MatchString(domain) {
		return redact.Refusal("DISPATCH_SIGNIN_DOMAIN", domain, notALowercaseDomain)
	}
	return nil
}

// signInRule is who boot's sign-in admits: the members of a Cognito pool's group, or the people of
// a Google Workspace domain, whether Google signs them in to Dispatch or Identity-Aware Proxy signs
// them in to the load balancer in front of it; nil when boot configures neither (header identity,
// dev sign-in, or a cookie server with no DISPATCH_SIGNIN_* setting, which admits nobody at all),
// matching signInFlow.
func signInRule(boot bootConfig) identity.SignInRule {
	switch {
	case boot.SignInDomain != "":
		return identity.DomainRule{Domain: boot.SignInDomain}
	case boot.SignInGroup != "":
		return identity.GroupRule{Group: boot.SignInGroup}
	default:
		return nil
	}
}

// signInFlow is the sign-in issuer's code flow for boot's DISPATCH_SIGNIN_* settings, asking
// Google for boot's Workspace domain in domain mode; nil when boot configures no sign-in (header
// identity, dev sign-in, or a cookie server with no DISPATCH_SIGNIN_* setting, which signs nobody
// in). It reads nothing: the issuer's discovery document is read when first needed
// (oidc.CodeFlow.Run, which main starts at boot, or the first request), so a sign-in issuer that
// is down at boot delays sign-ins and nothing else.
func signInFlow(boot bootConfig) *oidc.CodeFlow {
	if boot.SignInIssuer == "" {
		return nil
	}
	return oidc.NewCodeFlow(boot.SignInIssuer, boot.SignInClientID, boot.SignInClientSecret, boot.SignInDomain)
}

// openPeople is the people store main serves boot with over pool, and the keys session cookies are
// signed with (sessionSigningKey): the store seals each refresh token under the current key and
// opens one sealed under any of them, and revokes a token stored in plain text at signIn's pool,
// nil when boot configures none. A revocation the undiscovered issuer cannot take fails that one
// token, which stays for the next boot.
func openPeople(boot bootConfig, dataDir string, pool *store.Pool, signIn *oidc.CodeFlow) (*store.PgPeopleStore, auth.SigningKeys, error) {
	signingKey, err := sessionSigningKey(boot, dataDir)
	if err != nil {
		return nil, auth.SigningKeys{}, err
	}
	var revoker store.RefreshTokenRevoker
	if signIn != nil {
		revoker = signIn
	}
	return store.NewPgPeopleStore(pool, signingKey, revoker), signingKey, nil
}

// retirePlainRefreshTokens is the boot's retirement of the refresh tokens people holds in plain
// text (store.PgPeopleStore.RetirePlainRefreshTokens), bounded by timeout (bootTask): a pool that
// does not answer, or has not been discovered, fails each token not yet revoked, and the start goes
// on. It logs the counts, never a token; each token the pool did not revoke has its own ERROR line,
// and stays for the next boot.
func retirePlainRefreshTokens(ctx context.Context, people *store.PgPeopleStore, timeout time.Duration) {
	bootTask(ctx, timeout, "dispatch: retire the refresh tokens stored in plain text", "dispatch: retired the refresh tokens stored in plain text",
		func(ctx context.Context) ([]any, error) {
			retired, failed, err := people.RetirePlainRefreshTokens(ctx)
			return []any{"retired", retired, "failed", failed}, err
		})
}

// requestIdentityFor is how a browser request names its person under boot: a header-identity
// server takes the header's email; an IAP server verifies the assertion Identity-Aware Proxy signs
// for each request, against IAP's key set at boot.IAPKeySetURL, and admits the people of
// DISPATCH_SIGNIN_DOMAIN (signInRule); a cookie server verifies its signed session cookie and, with
// sign-in configured, confirms at least hourly that the person is still in DISPATCH_SIGNIN_GROUP
// or DISPATCH_SIGNIN_DOMAIN (signInRule). A cookie server with no sign-in setting names nobody
// (identity.NoBrowserIdentity): nobody can sign in there, and a cookie minted while sign-in was on
// would otherwise go on naming its person for its thirty days, signed by the deployment's own
// DISPATCH_SIGNING_KEY and confirmed by nobody. Dev sign-in has no issuer either and keeps its
// cookie: it mints that cookie itself. That case is read from boot, which says what the
// deployment configured, and not from signIn.
func requestIdentityFor(boot bootConfig, signingKey auth.SigningKeys, people auth.PeopleStore, sessions auth.SessionStore, signIn *oidc.CodeFlow) identity.Identity {
	if boot.IdentityHeader != "" {
		return identity.HeaderIdentity{Header: boot.IdentityHeader, People: people}
	}
	if boot.IAPAudience != "" {
		return identity.IAPIdentity{
			Verifier: oidc.NewIAPVerifier(boot.IAPAudience, oidc.NewKeysAt(boot.IAPKeySetURL)),
			Rule:     signInRule(boot),
			People:   people,
		}
	}
	if boot.SignInIssuer == "" && !boot.DevSignIn {
		return identity.NoBrowserIdentity{}
	}
	cookieIdentity := identity.CookieIdentity{SigningKey: signingKey, Sessions: sessions}
	if signIn != nil {
		cookieIdentity.Membership = &identity.Membership{People: people, Sessions: sessions, SignIn: signIn, Rule: signInRule(boot)}
	}
	return cookieIdentity
}
