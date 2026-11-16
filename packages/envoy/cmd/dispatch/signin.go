package main

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/sjawhar/envoy/internal/dispatch/access"
	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/identity"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/oidc"
	"github.com/sjawhar/envoy/internal/weborigin"
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

// signInClientSettings are the three settings every Google sign-in takes, each with boot's value:
// the issuer and Dispatch's confidential client there.
func signInClientSettings(boot bootConfig) []struct{ name, value string } {
	return []struct{ name, value string }{
		{"DISPATCH_SIGNIN_ISSUER", boot.SignInIssuer},
		{"DISPATCH_SIGNIN_CLIENT_ID", boot.SignInClientID},
		{"DISPATCH_SIGNIN_CLIENT_SECRET", boot.SignInClientSecret},
	}
}

// signInSettings are all six sign-in settings, each with boot's value: the client's three, then
// the group a Cognito sign-in pool admits and the Workspace domain Google admits, of which a
// sign-in takes exactly one, and the switch that admits Google accounts outside that domain too
// ("1" when on).
func signInSettings(boot bootConfig) []struct{ name, value string } {
	outside := ""
	if boot.SignInOutside {
		outside = "1"
	}
	return append(signInClientSettings(boot),
		struct{ name, value string }{"DISPATCH_SIGNIN_GROUP", boot.SignInGroup},
		struct{ name, value string }{"DISPATCH_SIGNIN_DOMAIN", boot.SignInDomain},
		struct{ name, value string }{"DISPATCH_SIGNIN_OUTSIDE", outside},
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
// first. Then the first of these refuses: outside sign-in without a domain, whose Google sign-in
// it widens; a group and a domain both set; some of the settings but not the issuer, client ID,
// client secret and one of the group or domain; a domain under an issuer other than Google's,
// whose tokens carry no `hd`, unless the test hooks put the dev OIDC issuer in Google's place
// (DISPATCH_TEST_SIGNIN_DOMAIN_ISSUER); a domain that is not a lowercase DNS name; under Google's
// issuer, an outside origin whose host Google refuses in an OAuth redirect URI, since the Google
// client must list `<origin>/auth/callback` (weborigin.GoogleRedirectHost, the rule gcp:Dispatch
// holds its outsideOrigin input to with sign-in on); a group under Google's issuer, whose tokens
// carry no `cognito:groups`. A deployment with the issuer, client ID, client secret and group of
// a Cognito pool reaches none of them.
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
	if boot.SignInOutside && boot.SignInDomain == "" {
		return errors.New("DISPATCH_SIGNIN_OUTSIDE needs DISPATCH_SIGNIN_DOMAIN: it admits Google accounts outside the Workspace domain Dispatch signs people in for with Google, so set DISPATCH_SIGNIN_DOMAIN under DISPATCH_SIGNIN_ISSUER https://accounts.google.com, or unset DISPATCH_SIGNIN_OUTSIDE")
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
		if boot.SignInIssuer != googleIssuer && !boot.TestSignInDomainIssuer {
			return fmt.Errorf("DISPATCH_SIGNIN_DOMAIN needs DISPATCH_SIGNIN_ISSUER %s, since only Google's tokens carry hd; DISPATCH_SIGNIN_ISSUER is %s", googleIssuer, boot.SignInIssuer)
		}
		if len(boot.SignInDomain) > maxDomainLength || !domainShape.MatchString(boot.SignInDomain) {
			return fmt.Errorf("DISPATCH_SIGNIN_DOMAIN must be a lowercase domain name such as example.com; got a value of %d characters", utf8.RuneCountInString(boot.SignInDomain))
		}
		if boot.OutsideOrigin != "" && boot.SignInIssuer == googleIssuer {
			// parseOutsideOrigin stored the origin weborigin.Parse returned, so it parses.
			outside, _ := url.Parse(boot.OutsideOrigin)
			if err := weborigin.GoogleRedirectHost(outside.Hostname()); err != nil {
				return fmt.Errorf("DISPATCH_OUTSIDE_ORIGIN's %w", err)
			}
		}
	}
	if boot.SignInGroup != "" && boot.SignInIssuer == googleIssuer {
		return fmt.Errorf("DISPATCH_SIGNIN_GROUP is a Cognito pool's group, and a token from %s carries none; set DISPATCH_SIGNIN_DOMAIN instead", googleIssuer)
	}
	return nil
}

// signInRule is who boot's sign-in admits: the members of a Cognito pool's group; the people of a
// Google Workspace domain; or, with DISPATCH_SIGNIN_OUTSIDE=1, those people and the verified
// Google accounts outside the domain that lister says a project names directly. It is nil when
// boot configures neither a group nor a domain (header identity, dev sign-in, or a cookie server
// with no DISPATCH_SIGNIN_* setting, which admits nobody at all), matching signInFlow. Outside
// sign-in with no lister is refused, so no path builds a rule that cannot read the access lists.
func signInRule(boot bootConfig, lister identity.Lister) (identity.SignInRule, error) {
	switch {
	case boot.SignInDomain != "" && boot.SignInOutside:
		if lister == nil {
			return nil, errors.New("DISPATCH_SIGNIN_OUTSIDE=1 reads the projects' access lists, and this server has no database to read them from")
		}
		return identity.OutsideRule{Domain: identity.DomainRule{Domain: boot.SignInDomain}, People: lister}, nil
	case boot.SignInDomain != "":
		return identity.DomainRule{Domain: boot.SignInDomain}, nil
	case boot.SignInGroup != "":
		return identity.GroupRule{Group: boot.SignInGroup}, nil
	default:
		return nil, nil
	}
}

// accessLister is who outside sign-in asks whether some project names an email directly, as its
// owner or on its access list as a person and never through a group: access.Listed over database's
// pool. It is nil with no database.
func accessLister(database *store.Store) identity.Lister {
	if database == nil {
		return nil
	}
	return identity.ListerFunc(func(ctx context.Context, email string) (bool, error) {
		return access.Listed(ctx, database.Pool, email)
	})
}

// parseSignInOutside reads DISPATCH_SIGNIN_OUTSIDE's value: "1" turns outside sign-in on, unset or
// empty leaves it off, and any other value refuses startup, without echoing it.
func parseSignInOutside(raw string) (bool, error) {
	switch value := strings.TrimSpace(raw); value {
	case "":
		return false, nil
	case "1":
		return true, nil
	default:
		return false, fmt.Errorf("DISPATCH_SIGNIN_OUTSIDE must be 1 or unset; got a value of %d characters", utf8.RuneCountInString(value))
	}
}

// parseOutsideOrigin reads DISPATCH_OUTSIDE_ORIGIN's value, raw: empty for none; else the origin
// weborigin.Parse returns for it, the rule gcp:Dispatch's outsideOrigin input is judged by, so a
// value written by hand is refused exactly as the component refuses it, with its trailing "/"
// trimmed as the component trims it. Beside that rule the origin must be https, or http only with
// the test hooks on (testHooks), for a local harness. Each refusal names the setting. Its host
// under Google sign-in, which the component judges too, is checkSignInSettings', which reads the
// sign-in settings.
func parseOutsideOrigin(raw string, testHooks bool) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	origin, err := weborigin.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("DISPATCH_OUTSIDE_ORIGIN: %w", err)
	}
	if origin.Scheme != "https" && !testHooks {
		return "", errors.New("DISPATCH_OUTSIDE_ORIGIN: must be an https origin; http is for a local harness with DISPATCH_TEST_HOOKS=1")
	}
	return origin.String(), nil
}

// checkOutsideOrigin refuses an outside origin that is not a second origin beside serverURL, the
// canonical one (DISPATCH_SERVER_URL, or dispatch.serverUrl in envoy.json): one with no canonical
// origin, and one on the canonical origin's host, whatever the port, since a browser keeps one
// host's cookies for every port of it and a request's Host decides which origin answers. Main
// checks it once the canonical origin resolves; an empty outsideOrigin passes.
func checkOutsideOrigin(outsideOrigin, serverURL string) error {
	if outsideOrigin == "" {
		return nil
	}
	canonical, err := url.Parse(serverURL)
	if err != nil || canonical.Hostname() == "" {
		return errors.New("DISPATCH_OUTSIDE_ORIGIN: needs DISPATCH_SERVER_URL (or dispatch.serverUrl in envoy.json), the canonical origin it is the second of")
	}
	outside, err := url.Parse(outsideOrigin)
	if err != nil {
		return fmt.Errorf("DISPATCH_OUTSIDE_ORIGIN: %w", err)
	}
	if strings.EqualFold(outside.Hostname(), canonical.Hostname()) {
		return fmt.Errorf("DISPATCH_OUTSIDE_ORIGIN: its host %s is DISPATCH_SERVER_URL's; the outside origin must be on another host", outside.Hostname())
	}
	return nil
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
// server takes the header's email; a cookie server verifies its signed session cookie and, with
// sign-in configured, confirms at least hourly that rule, boot's sign-in rule (signInRule), still
// admits the person: still in DISPATCH_SIGNIN_GROUP or DISPATCH_SIGNIN_DOMAIN, or, from outside
// the domain, still named by a project. A cookie server with no sign-in setting names nobody
// (identity.NoBrowserIdentity): nobody can sign in there, and a cookie minted while sign-in was on
// would otherwise go on naming its person for its thirty days, signed by the deployment's own
// DISPATCH_SIGNING_KEY and confirmed by nobody. Dev sign-in has no issuer either and keeps its
// cookie: it mints that cookie itself. That case is read from boot, which says what the
// deployment configured, and not from signIn.
func requestIdentityFor(boot bootConfig, signingKey auth.SigningKeys, people auth.PeopleStore, sessions auth.SessionStore, signIn *oidc.CodeFlow, rule identity.SignInRule) identity.Identity {
	if boot.IdentityHeader != "" {
		return identity.HeaderIdentity{Header: boot.IdentityHeader, People: people}
	}
	if boot.SignInIssuer == "" && !boot.DevSignIn {
		return identity.NoBrowserIdentity{}
	}
	cookieIdentity := identity.CookieIdentity{SigningKey: signingKey, Sessions: sessions}
	if signIn != nil {
		cookieIdentity.Membership = &identity.Membership{People: people, Sessions: sessions, SignIn: signIn, Rule: rule}
	}
	return cookieIdentity
}
