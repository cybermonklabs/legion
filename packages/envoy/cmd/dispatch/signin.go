package main

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/clock"
	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/identity"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/oidc"
	"github.com/sjawhar/envoy/internal/redact"
	"github.com/sjawhar/envoy/internal/weborigin"
)

// plainRefreshTokenRetireTimeout bounds the boot's retirement of the refresh tokens people holds
// in plain text, so a sign-in pool that does not answer delays the start by this much at most.
const plainRefreshTokenRetireTimeout = 30 * time.Second

// googleIssuer is Google's OpenID Connect issuer, the one issuer whose ID tokens carry `hd`, the
// Workspace domain of the account. go-oidc also accepts a token whose `iss` is Google's schemeless
// `accounts.google.com` when this is the configured issuer, as Google documents.
const googleIssuer = "https://accounts.google.com"

// iapAudienceShape is the start every audience Identity-Aware Proxy signs for shares,
// /projects/<project number>/ (https://cloud.google.com/iap/docs/signed-headers-howto): a load
// balancer's backend service, an App Engine app or a Cloud Run service follows it.
var iapAudienceShape = regexp.MustCompile(`^/projects/[0-9]+/\S+$`)

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
	if err := oidc.IssuerRefusal("DISPATCH_SIGNIN_ISSUER", boot.SignInIssuer); err != nil {
		return err
	}
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

// checkDomainShape refuses a DISPATCH_SIGNIN_DOMAIN that is not a lowercase DNS name
// (oidc.IsDomainName, the rule the secrets broker holds its Workspace domains to), naming its
// length and never its value.
func checkDomainShape(domain string) error {
	if !oidc.IsDomainName(domain) {
		return oidc.DomainNameRefusal("DISPATCH_SIGNIN_DOMAIN", domain)
	}
	return nil
}

// parseOriginList reads value, the setting name holds (DISPATCH_SERVED_ORIGINS or
// DISPATCH_FORMER_ORIGINS): each comma-separated entry, trimmed, as the origin weborigin.Parse
// returns for it, an absolute http or https URL with no path, user, query or fragment, written as
// a browser writes an origin in its Origin header. A value of nothing but whitespace is unset. It
// refuses an empty entry and any other, naming the entry by its position and echoing no user or
// query, which weborigin.Parse never does; nil for none. Their hosts beside the server's other
// origins are checkServedOrigins'.
func parseOriginList(name, value string) ([]string, error) {
	if strings.TrimSpace(value) == "" {
		return nil, nil
	}
	entries := strings.Split(value, ",")
	origins := make([]string, len(entries))
	for index, entry := range entries {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			return nil, fmt.Errorf("%s entry %d of %d is empty", name, index+1, len(entries))
		}
		origin, err := weborigin.Parse(entry)
		if err != nil {
			return nil, fmt.Errorf("%s entry %d of %d: %w", name, index+1, len(entries), err)
		}
		origins[index] = origin.String()
	}
	return origins, nil
}

// parseAddressLists reads DISPATCH_SERVED_ORIGINS and DISPATCH_FORMER_ORIGINS from env, each list
// as parseOriginList reads it, for main's boot (resolveBootConfig) and rebuild-refs
// (rebuildRefsOrigins) alike.
func parseAddressLists(env settingValues) (served, former []string, err error) {
	if served, err = parseOriginList("DISPATCH_SERVED_ORIGINS", env.get("DISPATCH_SERVED_ORIGINS")); err != nil {
		return nil, nil, err
	}
	if former, err = parseOriginList("DISPATCH_FORMER_ORIGINS", env.get("DISPATCH_FORMER_ORIGINS")); err != nil {
		return nil, nil, err
	}
	return served, former, nil
}

// checkServedOrigins refuses boot's served and former addresses (DISPATCH_SERVED_ORIGINS and
// DISPATCH_FORMER_ORIGINS, as parseOriginList reads them) beside serverURL, the canonical origin
// (DISPATCH_SERVER_URL, or dispatch.serverUrl in envoy.json), and the origins a broker token's
// proof may name (proofOrigins: serverURL's and DISPATCH_PROOF_ORIGINS'). Every pair is compared
// by host name alone, at any port (weborigin.SameHostName: the name without regard to case, an
// IPv6 literal's brackets or a trailing dot): a browser keeps one host name's cookies for every
// port of it, and a former address is fenced at its host name at every port. It refuses:
//   - either list under dev sign-in (DISPATCH_DEV_SIGNIN=1), whose fence answers 421 to every Host
//     but the canonical origin's (routes' requireHost), so no address of either could be reached;
//   - either list with no canonical origin to be beside;
//   - a served or a former address on the host name of the canonical origin or of an address
//     listed before it, served or former, since each address of Dispatch's must be on a host name
//     of its own;
//   - a former address on the host name of a proof origin, since a former address passes no proof
//     check.
//
// Main checks it once the canonical origin resolves, and rebuild-refs before it opens the database
// (rebuildRefsOrigins). Two empty lists pass.
func checkServedOrigins(boot bootConfig, serverURL string) error {
	lists := []struct {
		name    string
		entries []string
	}{{"DISPATCH_SERVED_ORIGINS", boot.ServedOrigins}, {"DISPATCH_FORMER_ORIGINS", boot.FormerOrigins}}
	var set []string
	for _, list := range lists {
		if len(list.entries) > 0 {
			set = append(set, list.name)
		}
	}
	if len(set) == 0 {
		return nil
	}
	if boot.DevSignIn {
		return fmt.Errorf("%s must be unset with DISPATCH_DEV_SIGNIN=1, which answers every Host but DISPATCH_SERVER_URL's 421 HOST_MISMATCH, so Dispatch could serve no other address", strings.Join(set, " and "))
	}
	canonical, err := url.Parse(serverURL)
	if err != nil || canonical.Hostname() == "" {
		return fmt.Errorf("%s: needs DISPATCH_SERVER_URL (or dispatch.serverUrl in envoy.json), the canonical origin each address is beside", set[0])
	}
	// taken is an origin whose host no later address may have: whose names it in a refusal, and
	// why says why.
	type taken struct {
		origin     *url.URL
		whose, why string
	}
	const ownHost = "each address of Dispatch's must be on a host name of its own"
	held := []taken{{canonical, "DISPATCH_SERVER_URL's", ownHost}}
	// The proof origins after the first, which is serverURL's and held already; each is one
	// proofOriginEntries parsed.
	var proofs []taken
	for _, origin := range proofOrigins(serverURL, boot.ProofOrigins)[1:] {
		parsed, _ := url.Parse(origin)
		proofs = append(proofs, taken{parsed, "that of the proof origin " + origin, "a former address passes no proof check"})
	}
	for _, list := range lists {
		former := list.name == "DISPATCH_FORMER_ORIGINS"
		for index, entry := range list.entries {
			// Each entry is one weborigin.Parse returned (parseOriginList), so it parses.
			origin, _ := url.Parse(entry)
			others := held
			if former {
				others = slices.Concat(held, proofs)
			}
			for _, other := range others {
				if weborigin.SameHostName(origin, other.origin) {
					return fmt.Errorf("%s entry %d of %d: its host name %s is %s; %s", list.name, index+1, len(list.entries), origin.Hostname(), other.whose, other.why)
				}
			}
			held = append(held, taken{origin, fmt.Sprintf("%s entry %d's", list.name, index+1), ownHost})
		}
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
	return signInFlowOn(boot, clock.Real())
}

// signInFlowOn is signInFlow with each revocation's bound, oidc.TokenTimeout, armed on c
// (oidc.NewCodeFlowOn): clock.Real from signInFlow, and a test's clocktest.Fake, which ends a
// revocation the pool holds only where the test moves it.
func signInFlowOn(boot bootConfig, c clock.Clock) *oidc.CodeFlow {
	if boot.SignInIssuer == "" {
		return nil
	}
	// A zero OIDCDiscoveryRetry, every deployment's, leaves discovery's waits as they are.
	return oidc.NewCodeFlowOn(boot.SignInIssuer, boot.SignInClientID, boot.SignInClientSecret, boot.SignInDomain, c,
		oidc.WithDiscoveryRetry(boot.OIDCDiscoveryRetry))
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
// text (store.PgPeopleStore.RetirePlainRefreshTokens), bounded by timeout on clk (bootTask): a pool
// that does not answer, or has not been discovered, fails each token not yet revoked, and the start
// goes on. It logs the counts, never a token; each token the pool did not revoke has its own ERROR
// line, and stays for the next boot.
func retirePlainRefreshTokens(ctx context.Context, people *store.PgPeopleStore, clk bootClock, timeout time.Duration) {
	bootTask(ctx, clk, timeout, "dispatch: retire the refresh tokens stored in plain text", "dispatch: retired the refresh tokens stored in plain text",
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
		return identity.HeaderIdentity{People: people}
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
