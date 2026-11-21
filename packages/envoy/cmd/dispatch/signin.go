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

	"golang.org/x/net/idna"

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

// workspaceDomainALabel is entry, an entry of DISPATCH_SIGNIN_WORKSPACE_DOMAINS, as IDNA lookup
// writes it (idna.Lookup.ToASCII), the form identity.WorkspaceAddress compares an address's domain
// in, and whether the setting takes entry: lowercase, taken by the lookup (a label such as xn--a,
// which decodes to nothing, is not), and so written a lowercase DNS name (domainShape) of at most
// maxDomainLength characters. An internationalized domain is written in Unicode, bücher.example,
// or as its A-labels, xn--bcher-kva.example, and is the same entry either way.
func workspaceDomainALabel(entry string) (string, bool) {
	ascii, err := idna.Lookup.ToASCII(entry)
	return ascii, err == nil && entry == strings.ToLower(entry) && len(ascii) <= maxDomainLength && domainShape.MatchString(ascii)
}

// workspaceDomainALabels is domains, entries of DISPATCH_SIGNIN_WORKSPACE_DOMAINS
// checkSignInSettings took, each as its A-label (workspaceDomainALabel), the form
// identity.WorkspaceAddress takes them in; nil for none.
func workspaceDomainALabels(domains []string) []string {
	if len(domains) == 0 {
		return nil
	}
	labels := make([]string, len(domains))
	for index, domain := range domains {
		labels[index], _ = workspaceDomainALabel(domain)
	}
	return labels
}

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

// signInSettings are all seven sign-in settings, each with boot's value: the client's three, then
// the group a Cognito sign-in pool admits and the Workspace domain Google admits, of which a
// sign-in takes exactly one, the Workspace's other domains, as written, and the switch that signs
// people outside the domain in with a passkey ("1" when on).
func signInSettings(boot bootConfig) []struct{ name, value string } {
	on := func(set bool) string {
		if set {
			return "1"
		}
		return ""
	}
	return append(signInClientSettings(boot),
		struct{ name, value string }{"DISPATCH_SIGNIN_GROUP", boot.SignInGroup},
		struct{ name, value string }{"DISPATCH_SIGNIN_DOMAIN", boot.SignInDomain},
		struct{ name, value string }{"DISPATCH_SIGNIN_WORKSPACE_DOMAINS", strings.Join(boot.SignInWorkspaceDomains, ",")},
		struct{ name, value string }{"DISPATCH_SIGNIN_PASSKEY", on(boot.SignInPasskey)},
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
// of these refuses: passkey sign-in without a domain, or without an outside origin, the only origin
// a passkey signs in at; the Workspace's other domains without the domain they are listed beside; a
// group and a domain both set; some of the settings but not the issuer, client ID, client secret
// and one of the group or domain; a domain under an issuer other than Google's, whose tokens carry
// no `hd`, unless the test hooks put the dev OIDC issuer in Google's place
// (DISPATCH_TEST_SIGNIN_DOMAIN_ISSUER); a domain that is not a lowercase DNS name; an entry of the
// Workspace's other domains that is empty or not a lowercase domain name IDNA lookup takes
// (workspaceDomainALabel), named by its position; under Google's issuer, an outside origin whose
// host is no public domain name, since people from outside sign in there with a passkey whose
// relying party is that host, a registrable domain name and never an IP address, while the Google
// client lists no outside callback (weborigin.PasskeyHost, the rule gcp:Dispatch holds its
// outsideOrigin input to with sign-in on); a group under Google's issuer, whose tokens carry no
// `cognito:groups`. A deployment with the issuer, client ID, client secret and group of a Cognito
// pool reaches none of them.
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
	if boot.SignInPasskey && boot.SignInDomain == "" {
		return errors.New("DISPATCH_SIGNIN_PASSKEY needs DISPATCH_SIGNIN_DOMAIN: it signs people outside the Workspace domain Dispatch signs people in for with Google in with a passkey, so set DISPATCH_SIGNIN_DOMAIN under DISPATCH_SIGNIN_ISSUER https://accounts.google.com, or unset DISPATCH_SIGNIN_PASSKEY")
	}
	if boot.SignInPasskey && boot.OutsideOrigin == "" {
		return errors.New("DISPATCH_SIGNIN_PASSKEY needs DISPATCH_OUTSIDE_ORIGIN: a person from outside signs in with a passkey at the outside origin alone, whose host is the passkey's relying party, so set DISPATCH_OUTSIDE_ORIGIN, or unset DISPATCH_SIGNIN_PASSKEY")
	}
	if len(boot.SignInWorkspaceDomains) > 0 && boot.SignInDomain == "" {
		return errors.New("DISPATCH_SIGNIN_WORKSPACE_DOMAINS needs DISPATCH_SIGNIN_DOMAIN: it lists the other domains of the Workspace whose people sign in with Google, so set DISPATCH_SIGNIN_DOMAIN under DISPATCH_SIGNIN_ISSUER https://accounts.google.com, or unset DISPATCH_SIGNIN_WORKSPACE_DOMAINS")
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
		if err := checkDomainShape(boot.SignInDomain); err != nil {
			return err
		}
		for index, domain := range boot.SignInWorkspaceDomains {
			if domain == "" {
				return fmt.Errorf("DISPATCH_SIGNIN_WORKSPACE_DOMAINS entry %d of %d is empty", index+1, len(boot.SignInWorkspaceDomains))
			}
			if _, ok := workspaceDomainALabel(domain); !ok {
				return fmt.Errorf("DISPATCH_SIGNIN_WORKSPACE_DOMAINS entry %d of %d must be a lowercase domain name such as example.com; got a value of %d characters", index+1, len(boot.SignInWorkspaceDomains), utf8.RuneCountInString(domain))
			}
		}
		if boot.OutsideOrigin != "" && boot.SignInIssuer == googleIssuer {
			// parseOutsideOrigin stored the origin weborigin.Parse returned, so it parses.
			outside, _ := url.Parse(boot.OutsideOrigin)
			if err := weborigin.PasskeyHost(outside.Hostname()); err != nil {
				return fmt.Errorf("DISPATCH_OUTSIDE_ORIGIN's %w", err)
			}
		}
	}
	if boot.SignInGroup != "" && boot.SignInIssuer == googleIssuer {
		return fmt.Errorf("DISPATCH_SIGNIN_GROUP is a Cognito pool's group, and a token from %s carries none; set DISPATCH_SIGNIN_DOMAIN instead", googleIssuer)
	}
	return nil
}

// checkIAPSignInSettings refuses the sign-in and origin settings IAP identity cannot take.
// Identity-Aware Proxy signs people in at IAP's one origin, so Dispatch runs no sign-in of its own:
// the issuer, client ID, client secret and group are refused, each by name, and so are the outside
// origin, passkey sign-in and the Workspace's other domains, which widen Dispatch's own sign-in.
// DISPATCH_SIGNIN_DOMAIN is required, a lowercase DNS name, since it is who may use Dispatch: an
// account whose assertion's hd is that domain.
func checkIAPSignInSettings(boot bootConfig) error {
	var refused []string
	for _, setting := range append(signInClientSettings(boot), struct{ name, value string }{"DISPATCH_SIGNIN_GROUP", boot.SignInGroup}) {
		if setting.value != "" {
			refused = append(refused, setting.name)
		}
	}
	if boot.OutsideOrigin != "" {
		refused = append(refused, "DISPATCH_OUTSIDE_ORIGIN")
	}
	if boot.SignInPasskey {
		refused = append(refused, "DISPATCH_SIGNIN_PASSKEY")
	}
	if len(boot.SignInWorkspaceDomains) > 0 {
		refused = append(refused, "DISPATCH_SIGNIN_WORKSPACE_DOMAINS")
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
// length and never its value.
func checkDomainShape(domain string) error {
	if len(domain) > maxDomainLength || !domainShape.MatchString(domain) {
		return fmt.Errorf("DISPATCH_SIGNIN_DOMAIN must be a lowercase domain name such as example.com; got a value of %d characters", utf8.RuneCountInString(domain))
	}
	return nil
}

// signInRule is who boot's sign-in admits: the members of a Cognito pool's group; the people of a
// Google Workspace domain, whether Google signs them in to Dispatch or Identity-Aware Proxy signs
// them in to the load balancer in front of it; or, with DISPATCH_SIGNIN_PASSKEY=1 (which IAP
// identity refuses), the domain's people with Google and, with a passkey, the people outside it
// that lister says a project names directly, at an address on no domain the Workspace owns (the
// domain and DISPATCH_SIGNIN_WORKSPACE_DOMAINS, each entry as its A-label: workspaceDomainALabels;
// identity.PasskeyRule). No Google sign-in admits an account outside the domain. It is nil when
// boot configures neither a group nor a domain (header identity, dev sign-in, or a cookie server
// with no DISPATCH_SIGNIN_* setting, which admits nobody at all), matching signInFlow. Passkey
// sign-in with no lister is refused, so no path builds a rule that cannot read the access lists.
func signInRule(boot bootConfig, lister identity.Lister) (identity.SignInRule, error) {
	switch {
	case boot.SignInDomain != "" && boot.SignInPasskey:
		if lister == nil {
			return nil, errors.New("DISPATCH_SIGNIN_PASSKEY=1 reads the projects' access lists, and this server has no database to read them from")
		}
		return identity.PasskeyRule{OutsideRule: identity.OutsideRule{
			DomainRule:       identity.DomainRule{Domain: boot.SignInDomain},
			People:           lister,
			WorkspaceDomains: workspaceDomainALabels(boot.SignInWorkspaceDomains),
		}}, nil
	case boot.SignInDomain != "":
		return identity.DomainRule{Domain: boot.SignInDomain}, nil
	case boot.SignInGroup != "":
		return identity.GroupRule{Group: boot.SignInGroup}, nil
	default:
		return nil, nil
	}
}

// passkeySignIn is boot's passkey sign-in under rule, with DISPATCH_SIGNIN_PASSKEY=1: the store of
// the passkeys and the owners' links over people's database, recording each passkey's subject
// under the outside origin, and the ceremonies of that origin, which admit a person as rule does.
// Both are nil with the setting unset.
func passkeySignIn(boot bootConfig, people *store.PgPeopleStore, rule identity.SignInRule) (*store.PgPasskeys, *identity.Passkeys, error) {
	passkeyRule, ok := rule.(identity.PasskeyRule)
	if !boot.SignInPasskey || !ok {
		return nil, nil, nil
	}
	passkeys := store.NewPgPasskeys(people, oidc.CanonicalIssuer(boot.OutsideOrigin))
	ceremonies, err := identity.NewPasskeys(boot.OutsideOrigin, passkeys, passkeyRule)
	if err != nil {
		return nil, nil, err
	}
	return passkeys, ceremonies, nil
}

// accessLister is who passkey sign-in asks whether some project names an email directly, as its
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

// devRestrictedAdmission is what a dev sign-in server (DISPATCH_DEV_SIGNIN=1, which refuses every
// DISPATCH_SIGNIN_* setting and so has no sign-in rule) holds a restricted member's credentials to:
// the outside rule's list arm over database's access lists (identity.ListedAdmission,
// accessLister), so a restricted member's token acts there while a project names them directly, as
// it does under passkey sign-in. It is nil for every other server, whose sign-in rule, or none,
// holds them, and with no database.
func devRestrictedAdmission(boot bootConfig, database *store.Store) identity.RestrictedAdmission {
	if !boot.DevSignIn || database == nil {
		return nil
	}
	return identity.ListedAdmission{People: accessLister(database)}
}

// parseSignInSwitch reads the value of name, an on-or-off sign-in setting (DISPATCH_SIGNIN_PASSKEY):
// "1" turns it on, unset or empty leaves it off, and any other value refuses startup, without
// echoing it.
func parseSignInSwitch(name, raw string) (bool, error) {
	switch value := strings.TrimSpace(raw); value {
	case "":
		return false, nil
	case "1":
		return true, nil
	default:
		return false, fmt.Errorf("%s must be 1 or unset; got a value of %d characters", name, utf8.RuneCountInString(value))
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
// token, which stays for the next boot. Its options are peopleOptions(boot).
func openPeople(boot bootConfig, dataDir string, pool *store.Pool, signIn *oidc.CodeFlow) (*store.PgPeopleStore, auth.SigningKeys, error) {
	signingKey, err := sessionSigningKey(boot, dataDir)
	if err != nil {
		return nil, auth.SigningKeys{}, err
	}
	var revoker store.RefreshTokenRevoker
	if signIn != nil {
		revoker = signIn
	}
	return store.NewPgPeopleStore(pool, signingKey, revoker, peopleOptions(boot)...), signingKey, nil
}

// peopleOptions is the options of the people store main serves boot with: with a secrets broker
// configured (DISPATCH_AGENT_SECRETS_URL) and a sign-in issuer, a sign-in that replaces the
// account an address records, whatever member kind that account's row held, records the broker
// revocation of that account the server's loop pays, naming it under boot's sign-in issuer
// (store.WithBrokerRevocations); with no broker it records nothing,
// since only a process with a broker pays one. Every process of a deployment with a broker sets
// DISPATCH_AGENT_SECRETS_URL: a replacement a process without it serves records nothing, and the
// replaced account's broker credentials are never revoked.
func peopleOptions(boot bootConfig) []store.PeopleOption {
	if boot.AgentSecretsURL == "" || boot.SignInIssuer == "" {
		return nil
	}
	return []store.PeopleOption{store.WithBrokerRevocations(boot.SignInIssuer)}
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
// DISPATCH_SIGNIN_DOMAIN (rule, boot's sign-in rule: signInRule); a cookie server verifies its
// signed session cookie and, with sign-in configured, confirms at least hourly that rule still
// admits the person: still in DISPATCH_SIGNIN_GROUP or DISPATCH_SIGNIN_DOMAIN, or, from outside
// the domain, still named by a project. A cookie server with no sign-in setting names nobody
// (identity.NoBrowserIdentity): nobody can sign in there, and a cookie minted while sign-in was on
// would otherwise go on naming its person for its thirty days, signed by the deployment's own
// DISPATCH_SIGNING_KEY and confirmed by nobody. Dev sign-in has no issuer either and keeps its
// cookie: it mints that cookie itself. That case is read from boot, which says what the
// deployment configured, and not from signIn. passkeys is the passkey store where passkey sign-in
// is on (passkeySignIn), which confirms a passkey's sign-in hourly, and nil elsewhere, where a
// passkey's cookie is refused at its next use.
func requestIdentityFor(boot bootConfig, signingKey auth.SigningKeys, people auth.PeopleStore, sessions auth.SessionStore, signIn *oidc.CodeFlow, rule identity.SignInRule, passkeys *store.PgPasskeys) identity.Identity {
	if boot.IdentityHeader != "" {
		return identity.HeaderIdentity{Header: boot.IdentityHeader, People: people}
	}
	if boot.IAPAudience != "" {
		return identity.IAPIdentity{
			Verifier: oidc.NewIAPVerifier(boot.IAPAudience, oidc.NewKeysAt(boot.IAPKeySetURL)),
			Rule:     rule,
			People:   people,
		}
	}
	if boot.SignInIssuer == "" && !boot.DevSignIn {
		return identity.NoBrowserIdentity{}
	}
	cookieIdentity := identity.CookieIdentity{SigningKey: signingKey, Sessions: sessions}
	if signIn != nil {
		cookieIdentity.Membership = &identity.Membership{People: people, SignIn: signIn, Rule: rule}
		// Set only when on: a nil *store.PgPasskeys in the interface field would read as a store.
		if passkeys != nil {
			cookieIdentity.Membership.Passkeys = passkeys
		}
	}
	return cookieIdentity
}
