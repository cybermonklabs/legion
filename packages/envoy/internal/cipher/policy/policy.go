// Package policy decides who may have an agent secret from two tags on the secret itself in the
// secret store (AWS Secrets Manager's tags, or Google Secret Manager's annotations): owner
// (shared, a person's email, or a registered service) and tier (agent or human). Loader reads every secret under the namespace prefix into a Set, refusing by name each one
// whose tags or key the policy cannot serve, or reads one name alone (LoadOne); Current keeps the
// latest Set, rereads it on a ticker, and merges a single name's reread into it (RefreshOne);
// Set.Evaluate answers one requester's ask for one name.
//
// Who may ask follows from owner and tier, except where a secret's requesters tag names the only
// people and services that may ask for it at all (TagRequesters, checked before everything below,
// and a session or pod it does not name is denied whatever the owner and tier would have given
// it):
//
//   - a person's agent-tier secret goes to that person's sessions without asking, and any other
//     session's or pod's request is an approval request to the person;
//   - a shared agent-tier secret goes to any session or pod;
//   - a human-tier secret needs its owner's approval, or anyone's if it is shared;
//   - a service's secret goes only to that service's own sessions, and anyone else's request is
//     refused;
//   - a secret a person withheld from a session, by revoking a grant of it the session got
//     without asking, is human tier to that session: it asks its owner, or anyone for a shared
//     secret, and a service's own secret is refused.
package policy

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/record"
	"github.com/sjawhar/envoy/internal/cipher/secrets"
	"github.com/sjawhar/envoy/internal/contracts"
)

// The tags a namespace secret carries, and their fixed values.
const (
	// TagOwner names who owns the secret: OwnerShared, a person's lowercase email, a Google group
	// as group:<its lowercase email> (ValidGroupOwner), whose members each decide as the one owner
	// does, or a registered service's name.
	TagOwner = "owner"
	// TagTier is TierAgent or TierHuman.
	TagTier = "tier"
	// TagType selects the token type a secret mints on release. An absent or empty tag leaves the
	// secret's stored value unchanged.
	TagType = "type"
	// TypeGitHubApp mints one installation token for every installation of the stored GitHub App
	// on an account its TagGitHubOwners lists.
	TypeGitHubApp = "github-app"
	// TypeGoogleLogin exchanges the stored authorized-user refresh token for an access token.
	TypeGoogleLogin = "google-login"
	// TypeAzureLogin releases the broker's projected token with the stored Entra identity ids.
	TypeAzureLogin = "azure-login"
	// TypeGoogleServiceAccount mints a Google access token for the one service account its stored
	// value names, {"service_account": "<id>@<project>.iam.gserviceaccount.com"}. The token acts
	// as that account, which no person's sign-in bounds, so the type is served only to the one
	// person the secret's requesters tag names, beside the registered services that tag lists at
	// human tier, and only for grants of an hour at most (TagRules). The stored value does not
	// choose the account alone: the deployment's BROKER_SERVICE_ACCOUNT_SECRETS binds each such
	// secret to one account email, and a mint for any other account is refused, so a value
	// rewritten to name another account releases no token.
	TypeGoogleServiceAccount = "google-service-account"
	// TypeAnthropicFederation mints an Anthropic organization's token by Workload Identity
	// Federation: the Google identity token of the one service account the stored value names,
	// {"organization_id", "federation_rule_id", "service_account_id", "service_account" and,
	// for a rule enabled for more than one workspace, "workspace_id"}, traded at Anthropic's token
	// exchange for the token the federation rule grants. The deployment's
	// BROKER_SERVICE_ACCOUNT_SECRETS binds each such secret to one account, as it binds a
	// google-service-account secret. The token can be an organization administrator's, so the
	// type is human tier only, its grants an hour at most, and its requesters held as
	// google-service-account's are (TagRules).
	TypeAnthropicFederation = "anthropic-federation"
	// TagMaxGrant bounds a request's signed grant lifetime for this secret (ParseMaxGrant).
	TagMaxGrant = "max-grant"
	// TagDefaultGrant, set to DefaultGrantSession, keeps a request that names no lifetime for this
	// secret lasting until its session ends, rather than the broker's default grant lifetime.
	TagDefaultGrant = "default-grant"
	// DefaultGrantSession is the one value TagDefaultGrant takes.
	DefaultGrantSession = "session"
	// TagGitHubOwners lists the GitHub accounts a github-app secret mints installation tokens for
	// (ParseGitHubOwners). A github-app secret without it is refused, since a public App can be
	// installed by any account, and an installation on an account it does not list gets no token.
	TagGitHubOwners = "github-owners"
	// TagRequesters lists who may ask for the secret at all: people by lowercase email, Google
	// groups as group:<lowercase email>, whose members' sessions it admits, and registered
	// services by name, separated by white space (ParseRequesters). A session or pod the list
	// does not name is denied before owner and tier are read (ReasonRequesterNotListed), so the
	// tag narrows who may ask and never widens it. It bounds no grant by itself: the hour-long cap
	// is the business of TagAdmin and of the google-service-account and anthropic-federation
	// types, each of which demands one (TagRules).
	TagRequesters = "requesters"
	// TagScopes lists the OAuth scope names of the token a google-service-account secret mints,
	// separated by white space, each the part of a Google scope that follows
	// https://www.googleapis.com/auth/ (ParseScopes, Secret.ScopeURLs puts the prefix back). A
	// google-service-account secret without it mints the cloud-platform scope alone, which is
	// what every such secret minted before the tag existed. The names are short because the
	// broker's AWS store keeps its annotations as AWS tags, an AWS tag value holds at most 256
	// characters, and five full scope URLs are longer than that. The tag on a secret of any
	// other type is refused (ReasonScopesType), since no other minter reads it.
	TagScopes = "scopes"
	// TagAdmin marks a secret whose grant is an administrator's: "true" is the only value the
	// loader accepts, and anything else is refused (ReasonAdminMalformed). The broker cannot tell
	// such a secret from its type — an admin console password and a personal API key are both
	// stored strings — so the program that declares the secret says so. An admin secret must be
	// human tier, carry a requesters list and carry a max-grant of an hour or less, so every
	// release of it is approved by a person, asked for by someone it names, and short enough to
	// be reviewed while it is live (TagRules).
	TagAdmin = "admin"
	// AdminTrue is the one value TagAdmin takes, as DefaultGrantSession is TagDefaultGrant's: the
	// loader refuses every other value (ReasonAdminMalformed), and `agent-secrets` holds --admin
	// to it before it writes, so the two cannot drift on what the tag says.
	AdminTrue = "true"
	// TagProject names the one Dispatch project the secret belongs to, by its key as written
	// (ParseProject): a secret of a private project goes only to a credential that names that
	// project (Evaluate, ReasonProjectNotNamed), and one whose tag names a project Dispatch does
	// not hold goes to nobody (ReasonProjectMissing). A secret with no tag belongs to no project
	// and is decided by owner and tier alone, as before the tag existed. It is not "project",
	// which AWS accounts commonly use as a cost-allocation tag and which the loader would then
	// refuse as malformed. Only a broker that can ask Dispatch whether a project is private
	// serves a tagged secret (Loader.ProjectChecks, ReasonProjectUnchecked).
	TagProject = "dispatch-project"
	// TagProjectChannels marks a secret whose use a private project's owner limits to the places
	// they named. Its one value is ProjectChannelsSlack: the Legion Slack bot token, which a
	// credential of a private project may spend only on a post to a channel that project's owner
	// named in Dispatch (proxy's slackPostAllowed). The limit holds for a credential that
	// names a private project, whatever project the secret itself belongs to, so the tag is read
	// beside TagProject and never instead of it. A secret with no tag is unchanged.
	TagProjectChannels = "project-channels"
	// ProjectChannelsSlack is TagProjectChannels' one value. Any other is
	// ReasonProjectChannelsMalformed: a value the broker cannot read names no limit it could
	// apply, and such a secret goes to nobody rather than out unlimited.
	ProjectChannelsSlack = "slack"
	// typeSourcePrefix opens the Source of a type secret (TypeSource). The type and store source
	// are frozen into a request, so changing or clearing the type ends every live grant.
	typeSourcePrefix = "mint:"
	// TagRemoved is the value of `agent-secrets secret retag --type`, `--github-owners`,
	// `--requesters` and `--max-grant` that removes the tag. It is never a login a github-owners
	// tag names: an entry that reads `none` in any case was written by hand, and is refused
	// rather than read as an account (ParseGitHubOwners).
	TagRemoved = "none"
	// TagRollbackGuard and RollbackGuardValue are the tag `agent-secrets secret` writes beside every
	// type, and the value it writes. This broker never reads the tag, and neither does any broker
	// from legion-envoy-v16.0.0 to the release before this one, which read the old kind tag alone
	// and serve a secret that carries type alone as a stored secret, releasing its App key or
	// refresh token to every grant. Only brokers from legion-envoy-v13.19.0 to the release before
	// v16.0.0 read it, as the service account a secret mints for, and refuse it
	// (mint-target-malformed); brokers older than legion-envoy-v13.19.0 read no such tag. So the
	// guard does not stop a rollback past this release from serving a type secret's key: the
	// protection is a rollback floor at this release, which the deployment keeps, and
	// `agent-secrets`, which writes no typed secret through a broker that does not say it reads the
	// type tag (the settings' secret_type_tag).
	TagRollbackGuard   = "mint-service-account"
	RollbackGuardValue = "type-secret"
	// RequestersRollbackGuard is the entry `agent-secrets secret` writes into a secret's
	// requesters tag beside a dispatch-project or a project-channels tag, and takes out once the
	// secret carries neither. This broker takes it out before it reads the list
	// (SplitRequestersGuard), so it means nothing here: a requesters tag holding it alone is no
	// list, and a list beside it reads as it would without it. Brokers before the project tags
	// read every requesters entry as a person's email or a registered service, and no service
	// name holds a colon (record.ValidService, which BROKER_SERVICES is held to), so they refuse
	// the secret (requesters-malformed) rather than serve it by owner and tier to every session,
	// which is what they do with a project tag they cannot read. A project tag written without
	// it, by a console or an infra-applier, leaves that rollback open: never run a broker from
	// before the project tags against a store holding one.
	RequestersRollbackGuard = "rollback-guard:project-tags"
	// OwnerShared is the owner value of a secret nobody owns alone.
	OwnerShared = "shared"
	// TierAgent is a secret its owner's agents use without asking.
	TierAgent = "agent"
	// TierHuman is a secret every request for which a person approves.
	TierHuman = "human"
)

// The outcomes Evaluate answers, stored as each requested name's decision.
const (
	Automatic = "automatic"
	Approval  = "approval"
	Deny      = "deny"
)

// Types is every value a secret's type tag may name. The broker holds a minter for each.
var Types = []string{TypeGitHubApp, TypeGoogleLogin, TypeAzureLogin, TypeGoogleServiceAccount, TypeAnthropicFederation}

// ValidType reports whether type is one of Types.
func ValidType(secretType string) bool {
	return slices.Contains(Types, secretType)
}

// TrimLineEnding answers the value to spend for a stored value, and whether it changed it. Let
// rest be value with its whole trailing run of carriage returns and line feeds removed
// (strings.TrimRight of "\r\n"): the answer is rest, where it is not empty, holds no carriage
// return and no line feed, and differs from value, and value itself otherwise. A value with a
// line ending inside it, a PEM document for one, is left as it is, since nothing says its last
// line ending is not part of it, and a value that is only line endings keeps them, because
// releasing the empty string for a stored value that is not empty reads to a caller as a
// credential that is not set.
//
// This is the one statement of the rule, applied by the broker's release and by
// `agent-secrets`' value reader: two copies of it would drift, and a caller would be handed a
// value one of them had trimmed and the other had not. The value itself decides, and no tag
// does: a secret a writer left a `shape` tag on is read exactly as one without it.
func TrimLineEnding(value string) (string, bool) {
	rest := strings.TrimRight(value, "\r\n")
	if rest == "" || rest == value || strings.ContainsAny(rest, "\r\n") {
		return value, false
	}
	return rest, true
}

// DecodeValue refuses value, which a secret of type holds, when that type's minter would refuse
// it: it decodes the value a release hands the minter (TrimLineEnding) with secrets.ParseValue,
// the decoder every minter calls, so the error names what is wrong in the minter's own words and
// never quotes the value. It is the one statement of trim-then-decode, which Current's value
// reads and `agent-secrets`' writes both call.
func DecodeValue(secretType, value string) error {
	spend, _ := TrimLineEnding(value)
	_, err := secrets.ParseValue(secretType, spend)
	return err
}

// ErrMaxGrantMalformed is a max-grant tag that is not a Go duration of at least one second.
var ErrMaxGrantMalformed = errors.New("max-grant is a duration of at least 1s")

// ParseMaxGrant reads a max-grant tag: a Go duration (time.ParseDuration) of at least one second,
// so 0 is free to mean that a secret sets none.
func ParseMaxGrant(raw string) (time.Duration, error) {
	maxGrant, err := time.ParseDuration(raw)
	if err != nil || maxGrant < time.Second {
		return 0, ErrMaxGrantMalformed
	}
	return maxGrant, nil
}

// ErrProjectMalformed is a dispatch-project tag that is not one Dispatch project key.
var ErrProjectMalformed = errors.New("dispatch-project names one Dispatch project key: " + contracts.ProjectKeyRule)

// projectKey is a whole Dispatch project key (contracts.ProjectKeyPattern), the one pattern the
// server creates keys by and the dispatch command checks them against.
var projectKey = regexp.MustCompile(contracts.ProjectKeyPattern)

// ParseProject reads a dispatch-project tag (TagProject): one Dispatch project key, matched as
// written. A key in another case, one with white space around it, an empty tag and the word
// none are each ErrProjectMalformed rather than read as a key or as no project: a project key
// is upper case, and a tag that names no project is one the secret does not carry, which
// `agent-secrets secret retag --project none` removes rather than writes.
func ParseProject(raw string) (string, error) {
	if !projectKey.MatchString(raw) {
		return "", ErrProjectMalformed
	}
	return raw, nil
}

// ProjectState is what Dispatch holds of one project, as the broker resolved it before it
// evaluates (Requester.ProjectStates). It is plain data, so Evaluate does no I/O and is safe to
// call while a transaction holds a lock.
type ProjectState int

const (
	// ProjectStateUnknown is the zero value: no state was resolved, and Evaluate answers
	// ErrNoProjectState for a secret tagged with that project rather than guess.
	ProjectStateUnknown ProjectState = iota
	// ProjectPublic is a project every person and credential may see.
	ProjectPublic
	// ProjectPrivate is a project only its own people may see.
	ProjectPrivate
	// ProjectMissing is a key Dispatch holds no project for.
	ProjectMissing
)

// ErrGitHubOwnersMissing is a github-owners tag that names no account.
var ErrGitHubOwnersMissing = errors.New("github-owners names at least one GitHub account")

// ErrGitHubOwnersComma is a github-owners tag that holds a comma: its logins are separated by
// spaces, as BROKER_SERVICES separates its entries, since an AWS tag value takes a space and no
// comma.
var ErrGitHubOwnersComma = errors.New(`github-owners separates GitHub logins with spaces, not commas ("<login> <login>")`)

// ErrGitHubOwnersLogin is a github-owners entry that cannot be a GitHub login: one that does not
// start with an ASCII letter or digit, holds anything but ASCII letters, digits, hyphens and
// underscores, is longer than 39 characters, or is the word none.
var ErrGitHubOwnersLogin = errors.New("github-owners names GitHub logins: up to 39 ASCII letters, digits, hyphens and underscores, starting with a letter or digit")

// gitHubLogin is what a GitHub login can be: an ASCII letter or digit, then ASCII letters, digits,
// hyphens and underscores. It is matched against the entry as written, before it is lowercased,
// since Unicode lowercasing maps some non-ASCII letters onto ASCII ones (U+212A KELVIN SIGN to `k`,
// U+0130 to `i` with a combining dot). GitHub's sign-up rule today also refuses an underscore and
// a trailing or doubled hyphen, but accounts made under its older rules keep their hyphens (`x-`
// is an organization from 2011), and a managed user's login ends in `_<shortcode>`
// (`mona-cat_octo`), so this refuses only what no login can hold. A login is at most 39
// characters (maxGitHubLogin), which the pattern does not check.
var gitHubLogin = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*$`)

const maxGitHubLogin = 39

// ParseGitHubLogin reads one GitHub account login, as written: up to 39 ASCII letters, digits,
// hyphens and underscores, starting with a letter or digit, and not the word none
// (ErrGitHubOwnersLogin). It answers it lowercased, since GitHub compares logins without regard to
// case. White space is no part of a login, so "ada ada" is refused rather than read as one.
func ParseGitHubLogin(raw string) (string, error) {
	if len(raw) > maxGitHubLogin || !gitHubLogin.MatchString(raw) {
		return "", fmt.Errorf("%w, not %q", ErrGitHubOwnersLogin, raw)
	}
	login := strings.ToLower(raw)
	if login == TagRemoved {
		return "", fmt.Errorf("%w, not %q: none removes the tag through agent-secrets and names no account", ErrGitHubOwnersLogin, raw)
	}
	return login, nil
}

// ParseGitHubOwners reads a github-owners tag: GitHub account logins separated by white space,
// compared without regard to case. It refuses a comma anywhere (ErrGitHubOwnersComma), an entry that
// is not a GitHub login or that is the word none (ParseGitHubLogin's ErrGitHubOwnersLogin), and a
// list that names no account (ErrGitHubOwnersMissing). It answers them lowercased, sorted and
// joined by single spaces, so a Secret holding them stays comparable, two spellings of one list are
// one policy, and the value is one that both stores hold: an AWS tag value and a Secret Manager
// annotation both take a space, which a GitHub login never holds.
func ParseGitHubOwners(raw string) (string, error) {
	if strings.Contains(raw, ",") {
		return "", ErrGitHubOwnersComma
	}
	var owners []string
	for _, entry := range strings.Fields(raw) {
		owner, err := ParseGitHubLogin(entry)
		if err != nil {
			return "", err
		}
		owners = append(owners, owner)
	}
	if len(owners) == 0 {
		return "", ErrGitHubOwnersMissing
	}
	slices.Sort(owners)
	return strings.Join(slices.Compact(owners), " "), nil
}

// ErrScopesMissing is a scopes tag that names no scope: a tag that narrowed the token to nothing
// would mint a token good for no API at all, which is a mistake rather than a policy. A secret
// that wants the default mints it by carrying no scopes tag (TagScopes).
var ErrScopesMissing = errors.New("scopes names at least one OAuth scope")

// ErrScopesComma is a scopes tag that holds a comma: its names are separated by spaces, as
// github-owners' and requesters' entries are, since an AWS tag value takes a space and no comma.
var ErrScopesComma = errors.New(`scopes separates its names with spaces, not commas ("<scope> <scope>")`)

// ErrScopesEntry is a scopes entry that is not a scope name: one that does not start with a
// lowercase ASCII letter or digit, or that holds anything but lowercase ASCII letters, digits,
// '.', '_' and '-'. A whole https://www.googleapis.com/auth/… URL is the mistake a reader makes
// here, and fails the pattern on its slashes and colon; an upper-case name fails it too, rather
// than being lowercased, since a Google scope name holds no upper case at all and one written
// that way was written by hand.
var ErrScopesEntry = errors.New("scopes names the part of a Google scope after https://www.googleapis.com/auth/, in lowercase")

// ErrScopesDuplicate is a scopes tag that names one scope twice. ParseGitHubOwners quietly
// compacts a repeated login, since two spellings of one account list are one policy; a repeated
// scope is refused instead, because the value is short enough to read whole and a name written
// twice is a mistake rather than a second spelling of the same list.
var ErrScopesDuplicate = errors.New("scopes names each scope once")

// googleScopeName is what the part of a Google OAuth scope after googleScopePrefix can be: a
// lowercase ASCII letter or digit, then lowercase ASCII letters, digits, '.', '_' and '-'
// ("cloud-platform", "devstorage.read_only", "bigquery"). It is matched against the entry as
// written, so neither a full scope URL (its slashes and colon) nor an upper-case name passes.
var googleScopeName = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

// googleScopePrefix opens every Google OAuth scope a scopes tag names (Secret.ScopeURLs puts it
// back before the minter asks Google for the token). The tag holds the part after it alone so
// that a list of five scopes still fits an AWS tag value's 256 characters (TagScopes).
const googleScopePrefix = "https://www.googleapis.com/auth/"

// ParseScopes reads a scopes tag: the names of the OAuth scopes a google-service-account secret's
// token carries, separated by white space, each the part of a Google scope that follows
// googleScopePrefix. It refuses a comma anywhere (ErrScopesComma), a list that names no scope
// (ErrScopesMissing), an entry that is not a scope name (ErrScopesEntry, which a whole scope URL
// and an upper-case name each land on), and a name given twice (ErrScopesDuplicate). It answers
// them sorted and joined by single spaces, so a Secret holding them stays comparable, two
// spellings of one list are one policy, and the value is one that both stores hold: an AWS tag
// value and a Secret Manager annotation both take a space, which a scope name never holds.
func ParseScopes(raw string) (string, error) {
	if strings.Contains(raw, ",") {
		return "", ErrScopesComma
	}
	var scopes []string
	for _, entry := range strings.Fields(raw) {
		if !googleScopeName.MatchString(entry) {
			return "", fmt.Errorf("%w, not %q", ErrScopesEntry, entry)
		}
		if slices.Contains(scopes, entry) {
			return "", fmt.Errorf("%w, not %q twice", ErrScopesDuplicate, entry)
		}
		scopes = append(scopes, entry)
	}
	if len(scopes) == 0 {
		return "", ErrScopesMissing
	}
	slices.Sort(scopes)
	return strings.Join(scopes, " "), nil
}

// TagSet is one secret's tags as TagRules weighs them: the values the loader read from the store
// (Loader.secret) or the values `agent-secrets secret create` and `retag` will write, beside the
// one thing that is no tag at all, BoundAccount.
type TagSet struct {
	// Type is the secret's type tag, "" for a stored secret; Owner its owner tag; Tier its tier
	// tag; Requesters its requesters tag as ParseRequesters or ParseRequesterShapes answered it,
	// "" for a secret that lists nobody; Scopes its scopes tag as ParseScopes answered it, "" for
	// a secret that names none.
	Type, Owner, Tier, Requesters, Scopes string
	// BoundAccount is the account email the deployment's BROKER_SERVICE_ACCOUNT_SECRETS binds to
	// this secret, and "" when it binds none. It is "" as well for a caller that does not hold
	// the setting — `agent-secrets`, which never sees it, exactly as it never sees BROKER_SERVICES
	// and so calls ParseRequesterShapes rather than ParseRequesters — so the rule it feeds, the
	// one refusing a service entry on a secret bound to a dev-<name> account, is the broker's to
	// apply on the write's reread.
	BoundAccount string
	// Admin is true when the secret carries admin: "true" (TagAdmin).
	Admin bool
	// MaxGrant is the secret's max-grant tag as ParseMaxGrant answered it, and 0 when it sets
	// none.
	MaxGrant time.Duration
}

// TagRules is the one statement of what a secret's tags demand of each other, which the loader
// applies to every secret it reads and `agent-secrets secret` create and retag apply before they
// write (so it asks for no registered service list of its own). It takes a TagSet rather than a
// positional list because the rules weigh eight values — the type, owner, tier, requesters,
// scopes, admin and max-grant tags, and the deployment's binding of the secret to an
// account — which no caller could pass in order without mistaking one for another. It answers
// "" for a secret the rules allow, and otherwise the load reason that refuses it:
//
//   - ReasonScopesType: a secret that is not a google-service-account one carries a scopes tag.
//     No other minter reads it, so the tag would narrow nothing and say that it had.
//   - ReasonAdminTier: an admin secret is not human tier. An administrator's credential is
//     released only by a person's approval, which agent tier does not ask for.
//   - ReasonAdminRequesters: an admin secret carries no requesters tag. Who may ask for an
//     administrator's credential is named, never left to owner and tier alone.
//   - ReasonTypeHumanTier: an anthropic-federation secret is not human tier. Its token can be an
//     organization administrator's, which a person approves each release of.
//   - ReasonMaxGrantRequired: an admin secret, a google-service-account one or an
//     anthropic-federation one sets no max-grant or one longer than an hour. Such a grant is
//     reviewed while it is live, so it lasts an hour at most. A secret that merely lists its
//     requesters keeps its own cap, or none.
//   - ReasonRequestersNotOwner: a condition of RequesterRule refuses the secret's requesters —
//     a google-service-account or anthropic-federation secret whose owner is neither a person
//     nor a group, one whose list names a service below human tier, or one whose list does not
//     name its owner exactly once, and a secret of any type the deployment binds to a dev-<name>
//     account whose list names a service. The one reason is all a load answers; which of those
//     conditions refused it is RequesterRule's fault, which `agent-secrets` reads to name the
//     flag that fixes the write.
//
// The rules above are weighed in the order they are written, so a secret several of them refuse
// is refused by the first.
func TagRules(t TagSet) string {
	if t.Scopes != "" && t.Type != TypeGoogleServiceAccount {
		return ReasonScopesType
	}
	if t.Admin && t.Tier != TierHuman {
		return ReasonAdminTier
	}
	if t.Admin && t.Requesters == "" {
		return ReasonAdminRequesters
	}
	if t.Type == TypeAnthropicFederation && t.Tier != TierHuman {
		return ReasonTypeHumanTier
	}
	if (t.Admin || t.Type == TypeGoogleServiceAccount || t.Type == TypeAnthropicFederation) && (t.MaxGrant == 0 || t.MaxGrant > time.Hour) {
		return ReasonMaxGrantRequired
	}
	if RequesterRule(t) != RequesterFaultNone {
		return ReasonRequestersNotOwner
	}
	return ""
}

// gitHubRepositoryName is what a repository name GitHub creates can be: ASCII letters, digits,
// '.', '-' and '_', at most 100 of them. The names . and .. are refused beside it.
var gitHubRepositoryName = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}$`)

// NarrowingError is ParseGitHubNarrowing's refusal: Field is the narrowing's field it names,
// "owner" or "repositories", so a caller can name it in its own terms (the API's
// github_app.<field>, the CLI's flag), and Reason says what is wrong with it.
type NarrowingError struct {
	Field, Reason string
}

func (e *NarrowingError) Error() string {
	return e.Field + " " + e.Reason
}

// ParseGitHubNarrowing checks one release's narrowing of its GitHub App tokens as a caller wrote it
// and answers it in its canonical form: owner one GitHub login (ParseGitHubLogin, once white space
// around it is trimmed), lowercased, and repositories names GitHub can hold, without the owner, at
// most secrets.MaxNarrowedRepositories, sorted and each once. Repositories nil names none, so the
// token reaches every repository of the installation; present but empty is refused, since a caller
// that built it from a list that matched nothing would otherwise get that widest token. Nothing
// else reaches GitHub: the permissions a narrowing can name are fixed, so a caller can only
// narrow. It answers its refusal as a *NarrowingError, typed rather than error, so a caller names
// the field without reading the text. The broker applies a narrowing in this form and names it back in
// the release's answer, and the CLI builds its request with this function too, so the CLI's check
// that the broker applied the narrowing it asked for compares like with like.
func ParseGitHubNarrowing(owner string, repositories []string, readOnly bool) (secrets.GitHubNarrowing, *NarrowingError) {
	login, err := ParseGitHubLogin(strings.TrimSpace(owner))
	if err != nil {
		return secrets.GitHubNarrowing{}, &NarrowingError{"owner", fmt.Sprintf("must name one GitHub login, not %q", owner)}
	}
	if repositories != nil && len(repositories) == 0 {
		return secrets.GitHubNarrowing{}, &NarrowingError{"repositories", "is empty: name at least one repository, or leave it out for every repository of the installation"}
	}
	if len(repositories) > secrets.MaxNarrowedRepositories {
		return secrets.GitHubNarrowing{}, &NarrowingError{"repositories", fmt.Sprintf("names %d repositories; the broker takes at most %d", len(repositories), secrets.MaxNarrowedRepositories)}
	}
	for _, name := range repositories {
		if !gitHubRepositoryName.MatchString(name) || name == "." || name == ".." {
			return secrets.GitHubNarrowing{}, &NarrowingError{"repositories", fmt.Sprintf("names %s's repositories by name alone, in ASCII letters, digits, '.', '-' and '_', not %q", login, name)}
		}
	}
	sorted := slices.Clone(repositories)
	slices.Sort(sorted)
	return secrets.GitHubNarrowing{Owner: login, Repositories: slices.Compact(sorted), ReadOnly: readOnly}, nil
}

// TypeNeedsGitHubOwners reports whether a secret of type must carry a github-owners tag naming at
// least one account. Only a github-app secret reads the tag.
func TypeNeedsGitHubOwners(secretType string) bool {
	return secretType == TypeGitHubApp
}

// GitHubOwnersFor is the github-owners rule every reader and writer of a secret's tags applies:
// the loader, and `agent-secrets secret create` and `retag` before they write. For a type that
// needs owners (TypeNeedsGitHubOwners) it answers ParseGitHubOwners of raw, with its two errors;
// for any other type the tag is not read, and it answers "".
func GitHubOwnersFor(secretType, raw string) (string, error) {
	if !TypeNeedsGitHubOwners(secretType) {
		return "", nil
	}
	return ParseGitHubOwners(raw)
}

// TypeSource is the Source of a type secret: its type frozen beside the store's own source.
// A store's source never starts with typeSourcePrefix (an AWS source starts with "arn:", a
// Secret Manager one with "projects/").
func TypeSource(secretType, storeSource string) string {
	return typeSourcePrefix + secretType + ":" + storeSource
}

// ErrTypeSourceMalformed is a source that opens with typeSourcePrefix but names no type or no
// store source, which no TypeSource built.
var ErrTypeSourceMalformed = errors.New("a minted source names no type or no store source")

// ParseTypeSource splits a source TypeSource built into its type and store source. minted is false
// for a stored secret's source, which it answers whole as storeSource.
func ParseTypeSource(source string) (secretType, storeSource string, minted bool, err error) {
	rest, minted := strings.CutPrefix(source, typeSourcePrefix)
	if !minted {
		return "", source, false, nil
	}
	secretType, storeSource, ok := strings.Cut(rest, ":")
	if !ok || secretType == "" || storeSource == "" {
		return "", "", true, ErrTypeSourceMalformed
	}
	return secretType, storeSource, true, nil
}

// ErrUnknownSecret is a name no secret the policy serves carries: none exists under the namespace
// prefix, or the one that does was refused.
var ErrUnknownSecret = errors.New("no agent secret has this name")

// ErrPendingValue is a name whose secret exists and waits for its first value
// (ReasonPendingValue): `agent-secrets secret create` made it and its owner has not yet stored the
// value. Unlike ErrUnknownSecret it is not final, so a request for it is refused with this, naming
// the secret, never as unknown.
var ErrPendingValue = errors.New("exists and awaits its first value; ask again after its owner stores it")

// ReasonAbsent is LoadOne's answer for a name no secret carries: none exists under the prefix, or
// the one that does is scheduled for deletion. It is not a refusal, so it is never in
// Set.Refused nor written as RefusedMessage: a reread of a name nobody created adds no
// broker_unservable_secrets series, and a refused secret found absent writes ServableMessage
// with now=absent.
const ReasonAbsent = "absent"

type ownerKind int

const (
	ownerPerson ownerKind = iota
	ownerShared
	ownerService
	ownerGroup
)

// Secret is one namespace secret the policy serves.
type Secret struct {
	// Name is what a session asks for: the secret's name under the prefix, uppercased, each hyphen
	// an underscore (<prefix>deel-api-key is DEEL_API_KEY).
	Name string
	// Source is what the secret's value is read by: its ARN in Secrets Manager, or its resource
	// name in Secret Manager. A type secret's is TypeSource of its type and that store source, so
	// the type is frozen into every request (ParseTypeSource reads it back).
	Source string
	// Owner is OwnerShared, a person's email, a group as group:<its email>, or a registered
	// service.
	Owner string
	// Tier is TierAgent or TierHuman.
	Tier string
	// MaxGrant is the greatest grant lifetime a request may sign for this secret, or 0 when it sets
	// none (ParseMaxGrant refuses anything under a second). A value, never a pointer, so a Secret
	// compares equal to an unchanged reread of itself (Current.RefreshOne).
	MaxGrant time.Duration
	// GitHubOwners is a github-app secret's github-owners tag as ParseGitHubOwners answers it, and
	// "" for every other secret. A string, not a slice, for the same reason as MaxGrant.
	GitHubOwners string
	// Requesters is the secret's requesters tag as ParseRequesters answers it, and "" for a secret
	// that carries none, which every session and pod may ask for. A string, not a slice, for the
	// same reason as MaxGrant.
	Requesters string
	// Scopes is a google-service-account secret's scopes tag as ParseScopes answers it, and "" for
	// a secret that carries none, whose token the minter asks for with the cloud-platform scope
	// alone (TagScopes, Secret.ScopeURLs). A string, not a slice, for the same reason as MaxGrant.
	Scopes string
	// Project is the secret's dispatch-project tag as ParseProject answers it, and "" for a
	// secret carrying none, which belongs to no project and is decided by owner and tier alone.
	// A string, not a pointer, for the same reason as MaxGrant. It is in Version, so tagging a
	// secret, or moving it to another project, makes the broker evaluate each live grant of it
	// again at its next use; a grant the policy now refuses ends there, and one it still allows
	// goes on.
	Project string
	// ProjectChannels is the secret's project-channels tag (TagProjectChannels), ProjectChannelsSlack
	// or "" for a secret carrying none: a private project's credential may spend such a secret only
	// where that project's owner allows, which the airlock's proxy is what enforces, so Evaluate
	// refuses one that has no airlock entry (ReasonProjectChannelsNeedAirlock). It is in Version,
	// so the airlock proxy reads again a value it read before the tag was added.
	ProjectChannels string
	// Admin is true when the secret carries admin: "true" (TagAdmin): its grant is an
	// administrator's, which the rules hold to human tier, a requesters list and an hour's cap
	// (TagRules).
	Admin bool
	// AirlockDigest is the fingerprint of this secret's entry in the airlock's rules file
	// (proxyrules.Rules.Digest), and "" for a secret the rules do not name or a deployment with no
	// rules file. A string, not a pointer, for the same reason as MaxGrant. It is in Version, so
	// a change to one secret's entry moves the policy version and ends the grants of that name.
	AirlockDigest string
	// SessionDefault is true when the secret's default-grant tag is "session": a request naming
	// no lifetime gets a grant that lasts until its session ends instead of DefaultGrantLifetime.
	SessionDefault bool
	ownership      ownerKind
}

// Type is the type the secret is served as, read back from its Source (TypeSource), or "" for a
// stored secret.
func (secret Secret) Type() string {
	secretType, _, minted, err := ParseTypeSource(secret.Source)
	if !minted || err != nil {
		return ""
	}
	return secretType
}

// PersonApprovesEveryRelease reports whether a person approves every release of the secret, so
// the model judge decides none of its requests whatever seconds the airlock rules give the name: a
// secret tagged admin (TagAdmin), and an anthropic-federation secret (TypeAnthropicFederation,
// read from its Source with ParseTypeSource), whose token can be an organization administrator's
// whether or not the tag says so. Each of the judge's gates reads it (requests' judgeKey,
// judgeStillDecides, currentPolicyAdmits and JudgePolicy).
func (secret Secret) PersonApprovesEveryRelease() bool {
	return secret.Admin || secret.Type() == TypeAnthropicFederation
}

// GitHubOwnerLogins is GitHubOwners as a list of lowercase logins, nil when it is "".
func (secret Secret) GitHubOwnerLogins() []string {
	if secret.GitHubOwners == "" {
		return nil
	}
	return strings.Fields(secret.GitHubOwners)
}

// ScopeURLs is Scopes as the list of whole Google OAuth scope URLs the minter asks Google for,
// each name under googleScopePrefix, and nil when it is "" — a secret that names none, for which
// the minter chooses its own default rather than reading an empty list as no scope at all.
func (secret Secret) ScopeURLs() []string {
	if secret.Scopes == "" {
		return nil
	}
	names := strings.Fields(secret.Scopes)
	urls := make([]string, 0, len(names))
	for _, name := range names {
		urls = append(urls, googleScopePrefix+name)
	}
	return urls
}

// approver is who decides an approval request for secret: anyone signed in to Dispatch for a
// shared secret, its owner otherwise, a group owner as written (group:<address>), whose members
// each decide it (record.MayDecide).
func (secret Secret) approver() string {
	if secret.ownership == ownerShared {
		return record.AnyoneApprover
	}
	return secret.Owner
}

// Set is the policy as one load read it, or as a single name's reread left it (NewSet builds both).
type Set struct {
	// Version is the SHA-256 of every served secret's name, owner, tier, source, max-grant,
	// github-owners, requesters, scopes, admin, project and project-channels, and of the
	// fingerprint of each one's airlock entry: equal versions are equal policies. A request
	// records the version it was decided under, so a live grant is re-evaluated only once the
	// policy changed. The project-channels tag is in it because the airlock proxy holds a grant's
	// released values per pod and reads them again only when one of the grant's names is reread
	// or this version moves (internal/cipher/proxy/bound.go): a project-channels retag a
	// console or an infra-applier made reaches a pod that already holds values by no other road,
	// and that pod would go on spending the Slack token with no channel limit. Whether a project
	// is private is Dispatch's state and not in it, so a caller re-checks a grant of a tagged name
	// on every use.
	Version string
	Secrets map[string]Secret
	// Refused is every secret under the prefix the policy refuses to serve, keyed by its whole
	// name in the store (the name its RefusedMessage line carries), with why. It is not in
	// Version: a refused secret is served to nobody, so a refusal that comes, goes or changes its
	// reason re-evaluates no live grant the served secrets did not already move. Current compares
	// it with the set before to write the state-change lines, and broker_unservable_secrets
	// (NewUnservableSecrets) reads it at each scrape.
	Refused map[string]Refusal
	// Pending is every secret under the prefix waiting for its value (ReasonPendingValue), keyed
	// as Refused is. It is served to nobody and refused for nothing: it is in neither Version nor
	// broker_unservable_secrets, and Current writes PendingMessage for an id that enters it.
	Pending map[string]Pending
	// versions is the version of each served type secret's value as the load or reread that read
	// it listed it (secrets.Listing.Version), keyed by Name. It is in neither Secret nor Version,
	// so a write of a new value moves no Version and ends no grant: Current decodes the value of
	// each version and type it has not decoded, and refuses the secret only when the decoder does
	// (ReasonValueMalformed).
	versions map[string]string
	// groups is the group reader of the Current that published the set (Loader.Groups), which
	// every decision on the set asks who is a member of a group, and nil for a set no Current
	// published. It is in neither Version nor any digest: who is a member changes with no change
	// of the policy's tags.
	groups record.Directory
}

// ValueVersion is the version of the type secret name's value as s lists it, and "" for a name s
// lists no type secret's value for: what a mint the decoder refuses hands Current.RefuseValue, so
// the refusal is keyed on the version the policy listed when the value was read.
func (s *Set) ValueVersion(name string) string { return s.versions[name] }

// Refusal is why the policy refuses to serve one secret under the prefix, as a load found it.
type Refusal struct {
	// Reason is one of the Reason* refusal constants.
	Reason string
	// Owner is the secret's owner tag as the store holds it, and "" for ReasonOwnerTagMissing and
	// ReasonOwnerTagMalformed, whose tag is the fault and names nobody the broker knows.
	Owner string
}

// Pending is one secret under the prefix waiting for its first value (ReasonPendingValue), as a
// load found it. It is no refusal, and so no Refusal.
type Pending struct {
	// Owner is the secret's owner tag as the store holds it.
	Owner string
	// Created is when the store created the secret, so a caller can tell the wait has ended
	// (Current.PendingValue, pendingAt) without a reload.
	Created time.Time
}

// setOf is NewSet(secrets) carrying refused, pending and versions, which NewSet's Version leaves
// out.
func setOf(secrets map[string]Secret, refused map[string]Refusal, pending map[string]Pending, versions map[string]string) *Set {
	set := NewSet(secrets)
	set.Refused, set.Pending, set.versions = refused, pending, versions
	return set
}

// NewSet builds the one Set shape both Load and the single-name merges produce, keeping secrets
// (keyed by Name) as its Secrets. A secret with no max-grant, no github-owners, no requesters, no
// scopes, no admin, no project and no project-channels tag writes the pre-type digest bytes,
// "<Name>\t<Owner>\t<Tier>\t<Source>\n" (a type secret's type is in its Source); one with any of
// them adds the first two, as a fifth field (time.Duration's form, 0s for none) and a sixth
// (empty for none), and then ends its line with "\trequesters=<list>", "\tscopes=<list>",
// "\tadmin=true", "\tproject=<KEY>" and "\tproject-channels=slack", in that order, for each of
// those five it carries. The project and project-channels tags are written only by a secret
// that carries one, so the secrets of a deployment that tags none write the bytes they wrote
// before the tag existed and no deployed version shifts. A
// secret the airlock's rules name adds one line more, "<Name>\tairlock=<digest>\n", rather than a
// field of the line above it, so every line a deployment with no rules file writes is
// byte-identical to the one it writes
// today and its Version does not shift. Entries are ascending by the secret's name in the store
// (the slug) — the exact order Load wrote before NewSet existed, so deployed plain-secret digests
// do not shift. (Sorting by the uppercased Name would NOT be equivalent: '-' is 0x2D, '_' is
// 0x5F, so "a-b" < "a0" as slugs but "A0" < "A_B" as Names.)
func NewSet(secrets map[string]Secret) *Set {
	type bySlug struct {
		slug   string
		secret Secret
	}
	served := make([]bySlug, 0, len(secrets))
	for _, s := range secrets {
		served = append(served, bySlug{slug: slugOf(s.Name), secret: s})
	}
	slices.SortFunc(served, func(a, b bySlug) int { return strings.Compare(a.slug, b.slug) })
	digest := sha256.New()
	for _, s := range served {
		requesters := ""
		if s.secret.Requesters != "" {
			requesters = "\trequesters=" + s.secret.Requesters
		}
		scopes := ""
		if s.secret.Scopes != "" {
			scopes = "\tscopes=" + s.secret.Scopes
		}
		admin := ""
		if s.secret.Admin {
			admin = "\tadmin=true"
		}
		// The project and project-channels fields: both are written only by a secret that
		// carries the tag, project first.
		projectTags := ""
		if s.secret.Project != "" {
			projectTags = "\tproject=" + s.secret.Project
		}
		if s.secret.ProjectChannels != "" {
			projectTags += "\tproject-channels=" + s.secret.ProjectChannels
		}
		switch {
		case s.secret.MaxGrant == 0 && s.secret.GitHubOwners == "" && !s.secret.SessionDefault && s.secret.Requesters == "" && s.secret.Scopes == "" && !s.secret.Admin && projectTags == "":
			fmt.Fprintf(digest, "%s\t%s\t%s\t%s\n", s.secret.Name, s.secret.Owner, s.secret.Tier, s.secret.Source)
		case s.secret.SessionDefault:
			fmt.Fprintf(digest, "%s\t%s\t%s\t%s\t%s\t%s\tdefault-grant=session%s%s%s%s\n", s.secret.Name, s.secret.Owner, s.secret.Tier, s.secret.Source, s.secret.MaxGrant, s.secret.GitHubOwners, requesters, scopes, admin, projectTags)
		default:
			fmt.Fprintf(digest, "%s\t%s\t%s\t%s\t%s\t%s%s%s%s%s\n", s.secret.Name, s.secret.Owner, s.secret.Tier, s.secret.Source, s.secret.MaxGrant, s.secret.GitHubOwners, requesters, scopes, admin, projectTags)
		}
		if s.secret.AirlockDigest != "" {
			fmt.Fprintf(digest, "%s\tairlock=%s\n", s.secret.Name, s.secret.AirlockDigest)
		}
	}
	return &Set{Version: hex.EncodeToString(digest.Sum(nil)), Secrets: secrets}
}

// Requester is an enrolled session or pod as the policy sees it.
type Requester struct {
	// Operator is the email of the person whose machine login the session enrolled under. For a
	// pod it is the person a person's pod login carried, while BROKER_POD_OPERATORS still maps the
	// pod's account to that person (requests.Machine.requester), and empty for any other pod.
	Operator string
	// Service is the service of the machine login that enrolled the session, set only when the
	// session is a pod whose verified service account is one of the accounts BROKER_SERVICES maps
	// to that same service (requests.Machine.requester); empty otherwise. A service may list
	// several accounts, but each account is listed once and proves exactly one service, since a
	// machine login's service is its own claim and only the pod's account can prove it: an
	// account listed for another service, or for none, proves nothing here.
	Service string
	// Withheld is the names the session's operator withheld from it by revoking a grant of them
	// the session got without asking (requests.Machine.RevokeByApprover). Evaluate answers each as
	// a human-tier secret.
	Withheld []string
	// Projects is the projects the credential names that its person may see, as the caller
	// resolved them: a session's enrollment's list, or the project an operator's entry maps a
	// pod to. Evaluate reads it for a secret tagged with a private project alone.
	Projects []string
	// Named is the projects the credential names, before the visibility filter Projects is the
	// result of. ListsPrivate reads it, so a credential that named a private project stays
	// limited once its person can no longer see that project, which is exactly the moment its
	// limit must not fall away. Evaluate's dispatch-project rule reads Projects and never this.
	Named []string
	// ProjectStates is the state of each project a tagged name being evaluated carries, resolved
	// by the caller from Dispatch before any transaction begins. It is plain data, not a lookup,
	// so Evaluate stays pure and is safe to call while a lock is held. A secret tagged with a
	// project it holds no state for is ErrNoProjectState.
	ProjectStates map[string]ProjectState
}

// Decision is how the policy answers one requester's ask for one name.
type Decision struct {
	// Outcome is Automatic, Approval or Deny.
	Outcome string
	// Approver is who decides an Approval: the owner's email, or record.AnyoneApprover for a
	// shared secret.
	Approver string
	// Source is what the secret's value is read by (Secret.Source).
	Source string
	// Reason names the rule behind a Deny the policy can name, and is empty for every other
	// decision: a caller tells a session why it was refused without reading the policy itself.
	Reason string
}

// ServiceOwned reports whether the set serves name as a registered service's secret. Who is a
// service's own session rests on BROKER_SERVICES' accounts, which are not in Version, so a caller
// re-checks a grant of such a name on every use rather than only once Version moves.
func (s *Set) ServiceOwned(name string) bool {
	return s.Secrets[name].ownership == ownerService
}

// ReasonNoReason is the Reason of a Deny the request machine decided: the request named a
// credential that would wait for a person's approval and carried no reason, so it is refused
// rather than put to anyone. Set.Evaluate never answers it, since the reason is not the
// policy's to see.
const ReasonNoReason = "a reason is necessary; ask again with --reason"

// ReasonProjectNotNamed is the Reason of a Deny the project tag decided: the secret belongs to
// a private project and the asking credential does not name it, so it is refused whatever its
// owner and tier would have given it.
const ReasonProjectNotNamed = "project-not-named"

// ReasonProjectMissing is the Reason of a Deny for a secret whose project tag names a key
// Dispatch holds no project for: it goes to nobody, since the broker cannot tell whether the
// project it was meant for is private. A caller is told ReasonProjectNotNamed instead
// (CallerReason), so a refusal never says whether a project exists.
const ReasonProjectMissing = "project-missing"

// ReasonProjectChannelsNeedAirlock is the Reason of a Deny for a project-channels secret
// (TagProjectChannels) with no airlock entry, asked for by a credential that names a private
// project (Requester.ListsPrivate). Outside the airlock the release hands the session the real
// token, which it could then spend anywhere; behind it the proxy holds each post to the channels
// the project's owner named. A credential that names no private project is unchanged.
const ReasonProjectChannelsNeedAirlock = "project-channels-need-airlock"

// ListsPrivate reports whether any project the credential names is private: the one predicate
// the project-channels limit is read from, by Evaluate and by the release alike. It reads Named,
// the projects as named, so losing sight of a project never lifts the limit that project put on
// the credential.
func (r Requester) ListsPrivate() bool {
	for _, project := range r.Named {
		if r.ProjectStates[project] == ProjectPrivate {
			return true
		}
	}
	return false
}

// ErrNoProjectState is ProjectRule's answer, and so Evaluate's, for a secret tagged with a project
// the requester holds no state for (Requester.ProjectStates): the caller did not resolve it, and
// the policy does no I/O to resolve it itself.
var ErrNoProjectState = errors.New("policy: a project-tagged secret was evaluated with no state for its project")

// ProjectRule is the one statement of the dispatch-project rule (TagProject): what a secret
// tagged with project answers r, as far as that tag decides. It answers "" for a secret the tag
// lets through: one with no tag, one of a public project, and one of a private project r's
// Projects name. It answers ReasonProjectNotNamed for a private project r's Projects do not name,
// and ReasonProjectMissing for a key Dispatch holds no project for. A tagged project r holds no
// state for is ErrNoProjectState. Evaluate reads it before owner and tier, and the airlock's
// connection check (requests' airlockProjectRule) reads it for every name a held grant carries,
// so the release and the proxy apply one rule.
func ProjectRule(project string, r Requester) (string, error) {
	if project == "" {
		return "", nil
	}
	switch r.ProjectStates[project] {
	case ProjectPublic:
		return "", nil
	case ProjectPrivate:
		if slices.Contains(r.Projects, project) {
			return "", nil
		}
		return ReasonProjectNotNamed, nil
	case ProjectMissing:
		return ReasonProjectMissing, nil
	}
	return "", ErrNoProjectState
}

// CallerReason is the reason a session is told for a Deny whose Reason is reason. It is reason
// itself, except that ReasonProjectMissing is told as ReasonProjectNotNamed: the two differ only
// in whether Dispatch holds the project, which a caller who may not see the project must not
// learn. The broker's own log keeps the reason Evaluate answered.
func CallerReason(reason string) string {
	if reason == ReasonProjectMissing {
		return ReasonProjectNotNamed
	}
	return reason
}

// ReasonSignInOff is the Reason of a Deny the request machine decided on a broker that signs
// nobody in (BROKER_SIGNIN=off): the name's answer would be a person's approval, and no person
// can sign in to give one, so the request is refused at once rather than left pending for
// nobody. It is named before ReasonNoReason, since asking again with a reason would be refused
// the same way. Set.Evaluate never answers it, since whether anyone can sign in is not the
// policy's to see.
const ReasonSignInOff = "a person must approve it, and this broker signs nobody in (BROKER_SIGNIN=off)"

// Evaluate answers r's ask for name. A name the set does not serve is ErrUnknownSecret. A secret
// whose requesters are listed (Secret.Requesters) is denied, ReasonRequesterNotListed, to every
// requester the list does not name, a group entry naming those the set's group reader admits
// (Set.Groups), before owner and tier are read at all; ReasonRequesterGroupUnread where a group
// entry's group is not read fresh and might name the person. A secret tagged with a project
// (Secret.Project) is then read by ProjectRule: no state is ErrNoProjectState, a project Dispatch
// does not hold is denied to everyone (ReasonProjectMissing), and a private project is denied to a
// credential that does not name it (ReasonProjectNotNamed); a public project, and a private one r
// names, go on to owner and tier unchanged. An agent-tier secret a group owns goes to a session
// whose person the group reader admits to that group, and is an approval request to the group for
// anyone else. A name withheld from r (Requester.Withheld) is decided as a human-tier secret: what
// r would have got at once is an approval request to the owner, or to anyone for a shared secret,
// and a service's own secret, which no person approves, is denied.
func (s *Set) Evaluate(name string, r Requester) (Decision, error) {
	secret, ok := s.Secrets[name]
	if !ok {
		return Decision{}, ErrUnknownSecret
	}
	if secret.Requesters != "" {
		if listed, unread := secret.lists(r, s.Groups()); !listed {
			reason := ReasonRequesterNotListed
			if unread {
				reason = ReasonRequesterGroupUnread
			}
			return Decision{Outcome: Deny, Reason: reason, Source: secret.Source}, nil
		}
	}
	switch reason, err := ProjectRule(secret.Project, r); {
	case err != nil:
		return Decision{}, fmt.Errorf("%w: %s is tagged %s", err, name, secret.Project)
	case reason != "":
		return Decision{Outcome: Deny, Reason: reason, Source: secret.Source}, nil
	}
	// After the project rule: a secret whose use a private project's owner limits goes to such a
	// credential only behind the airlock, where the proxy holds each post to the channels that
	// owner named. Outside it the release hands over the real token, which the session could then
	// spend anywhere.
	if secret.ProjectChannels != "" && secret.AirlockDigest == "" && r.ListsPrivate() {
		return Decision{Outcome: Deny, Reason: ReasonProjectChannelsNeedAirlock, Source: secret.Source}, nil
	}
	withheld := slices.Contains(r.Withheld, name)
	d := Decision{Source: secret.Source}
	switch {
	case secret.ownership == ownerService:
		d.Outcome = Deny
		if r.Service == secret.Owner && !withheld {
			d.Outcome = Automatic
		}
	case secret.Tier == TierAgent && !withheld && (secret.ownership == ownerShared ||
		secret.ownership == ownerPerson && record.CanonicalLogin(r.Operator) == secret.Owner ||
		secret.ownership == ownerGroup && record.GroupAdmits(s.Groups(), secret.Owner, r.Operator)):
		d.Outcome = Automatic
	default:
		d.Outcome, d.Approver = Approval, secret.approver()
	}
	return d, nil
}
