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
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/sjawhar/envoy/internal/cipher/githubname"
	"github.com/sjawhar/envoy/internal/cipher/proxyrules"
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
	// TypeGitHubInstallations hands over installation tokens a minter outside the broker minted
	// ahead of any request, from an App key the broker never holds, and stores as one value:
	// {"installations": {"<account login>": {"installation_id", "token", "expires_at",
	// "permissions", "repository_selection" and, for a token narrowed to repositories,
	// "repositories"}}} (secrets.ParseValue). It is served as a github-app secret is, only through
	// profiles on the accounts its TagGitHubOwners lists, and released in the github-app release's
	// form; the broker mints nothing for it, and a token cannot be cut once it is minted, so a
	// release serves a profile only the token whose permissions are within the profile's and whose
	// repositories are the profile's exactly (none, for a token the seed minter marked
	// "all-except-protected"), and a target of it names no repository
	// (secrets.GitHubInstallations).
	TypeGitHubInstallations = "github-installations"
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
	// TagGitHubOwners lists the GitHub accounts a github-app secret mints installation tokens for,
	// or a github-installations secret hands stored ones over for (ParseGitHubOwners). Such a
	// secret without it is refused, since a public App can be installed by any account, and an
	// installation on an account it does not list gets no token.
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
	// other type is refused (ReasonScopesType), since no other minter reads it. It is in the
	// fingerprint of every name that mints at it (Secret.ProfileDigest): the secret by its own name
	// and each profile of it with no google block of its own; so a change of it, the tag added or
	// taken off among them, ends each grant decided under the old one, which would otherwise mint at
	// the new scopes at its next release.
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
	// TagJudge makes a key one the model judge decides: whole seconds from 1 to MaxJudgeSeconds
	// (ParseJudge), the longest grant of it the judge makes, and the lifetime a request for it
	// that names none gets. Any other value refuses the key (ReasonJudgeOutOfRange). A key with
	// the tag and an admin tag is no judge key: a person approves every release of an
	// administrator's credential (Secret.PersonApprovesEveryRelease). A key without it is never
	// the judge's.
	TagJudge = "judge"
	// TagGuidance is the key owner's instruction to the model judge: when to grant a request for
	// the key. The judge shows it to its model whole, so a value longer than MaxGuidanceRunes
	// refuses the key (ReasonGuidanceTooLong). It decides nothing a live grant may do, so it is
	// in no policy version.
	TagGuidance = "guidance"
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
	// TagSupersededBy names the profile source that replaces a stored secret under the stored
	// secret's own name: a profile of that name whose source is exactly this value is served by the
	// name, and the stored secret is not (supersededBy), whether the policy serves the stored
	// secret, refuses it or holds it waiting for its value, so retiring the old secret's value
	// leaves the profile served. Any other value, and a stored secret with no such tag, leaves a
	// profile of its name refused profile-name-taken. A broker before this tag reads no such tag and
	// refuses that profile as before, so a deployment can add the tag first.
	TagSupersededBy = "superseded-by"
	// TagProxy names the kind of the profiles file's kinds (Profiles.Kinds) a secret is served
	// behind: a secret that carries it is behind Cipher's proxy, which puts its credential into
	// each call to its kind's hosts, and its credential is never released to a command. A tag
	// naming no kind of the file refuses the secret (ReasonProxyKindUnknown), and so does a kind
	// whose ways do not hold the way the secret's type takes (WayOf, ReasonProxyWay), or whose
	// max_grant the secret's max-grant tag is absent under or above (ReasonProxyMaxGrant). It is in
	// the secret's fingerprint (Secret.ProfileDigest), so retagging it ends the grants of its names.
	TagProxy = "proxy"
	// TagProxyNarrowed, set to ProxyNarrowedTrue, marks a secret whose one profile's allow narrows
	// it at the proxy (Profile.Allow): the policy serves it through exactly one profile, and that
	// profile has an allow, and refuses it, and every profile naming it, otherwise
	// (ReasonProxyNarrowedProfiles). A profile with an allow is served only from a source carrying
	// it (ReasonProfileAllowNotNarrowed), so no narrowing is in force that the store does not also
	// hold, and taking it off takes a change of the file and a change of the tag. Any other value
	// refuses the secret (ReasonProxyNarrowedMalformed).
	TagProxyNarrowed = "proxy-narrowed"
	// ProxyNarrowedTrue is TagProxyNarrowed's one value, as AdminTrue is TagAdmin's.
	ProxyNarrowedTrue = "true"
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
	// requesters tag beside a dispatch-project or a project-channels tag (WithGuards), and takes
	// out once the secret carries neither. This broker takes it out before it reads the list
	// (SplitGuards), so it means nothing here: a requesters tag holding it alone is no
	// list, and a list beside it reads as it would without it. Brokers before the project tags
	// read every requesters entry as a person's email or a registered service, and no service
	// name holds a colon (record.ValidService, which BROKER_SERVICES is held to), so they refuse
	// the secret (requesters-malformed) rather than serve it by owner and tier to every session,
	// which is what they do with a project tag they cannot read. A project tag written without
	// it, by a console or an infra-applier, leaves that rollback open: never run a broker from
	// before the project tags against a store holding one.
	RequestersRollbackGuard = "rollback-guard:project-tags"
	// KindsRollbackGuard is the entry `agent-secrets secret` writes into a secret's requesters
	// tag beside a proxy or a proxy-narrowed tag (TagProxy, TagProxyNarrowed), after the project
	// tags' guard where both are written (WithGuards), and takes out once the secret carries
	// neither, as RequestersRollbackGuard is beside the project tags. This broker takes it out
	// before it reads the list (SplitGuards), so it means nothing here.
	// Brokers before the kinds read neither proxy tag, and would serve a secret behind the proxy
	// by its tag alone as its value, to a command; they read the guard as a requester, which is
	// no person's email and no registered service (it holds a colon, which record.ValidService
	// refuses), so they refuse the secret (requesters-malformed) instead. A proxy tag written
	// without it, by a console or an infra-applier, leaves that rollback open, and `agent-secrets
	// secret list --json` shows such a secret.
	KindsRollbackGuard = "rollback-guard:kinds"
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
var Types = []string{TypeGitHubApp, TypeGitHubInstallations, TypeGoogleLogin, TypeAzureLogin, TypeGoogleServiceAccount, TypeAnthropicFederation}

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

// MaxJudgeSeconds is the longest grant the model judge makes of any key, and the most a judge tag
// may name (ParseJudge): 15 minutes, the class's bound.
const MaxJudgeSeconds = 900

// MaxGuidanceRunes is the longest guidance tag a key may carry (TagRules,
// ReasonGuidanceTooLong). The judge shows the whole text to its model beside the request's reason
// and what Dispatch holds of it, all within one bounded prompt, so a text past this would be cut
// rather than read; the key is refused instead, and `agent-secrets secret` refuses the write.
const MaxGuidanceRunes = 2000

// ErrJudgeOutOfRange is a judge tag that is not a whole number of seconds from 1 to
// MaxJudgeSeconds.
var ErrJudgeOutOfRange = fmt.Errorf("judge is a whole number of seconds from 1 to %d", MaxJudgeSeconds)

// ParseJudge reads a judge tag: decimal digits alone, no sign, unit or white space, naming whole
// seconds from 1 to MaxJudgeSeconds, so 0 is free to mean that a key has no judge. Every other
// value is ErrJudgeOutOfRange, which the loader answers as ReasonJudgeOutOfRange.
func ParseJudge(raw string) (uint32, error) {
	seconds, err := strconv.ParseUint(raw, 10, 32)
	if err != nil || seconds < 1 || seconds > MaxJudgeSeconds {
		return 0, ErrJudgeOutOfRange
	}
	return uint32(seconds), nil
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

// ParseGitHubLogin reads one GitHub account login, as written: up to 39 ASCII letters, digits,
// hyphens and underscores, starting with a letter or digit (githubname.ValidLogin), and not the
// word none (ErrGitHubOwnersLogin). It answers it lowercased, since GitHub compares logins without
// regard to case. White space is no part of a login, so "ada ada" is refused rather than read as
// one.
func ParseGitHubLogin(raw string) (string, error) {
	if !githubname.ValidLogin(raw) {
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
	// Guidance is the secret's guidance tag as written (TagGuidance), "" for a secret carrying
	// none.
	Guidance string
}

// TagRules is the one statement of what a secret's tags demand of each other, which the loader
// applies to every secret it reads and `agent-secrets secret` create and retag apply before they
// write (so it asks for no registered service list of its own). It takes a TagSet rather than a
// positional list because the rules weigh nine values — the type, owner, tier, requesters,
// scopes, admin, max-grant and guidance tags, and the deployment's binding of the secret to an
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
//   - ReasonGuidanceTooLong: the guidance tag is longer than MaxGuidanceRunes. The judge shows it
//     whole to its model, and a cut instruction is not the one its owner wrote.
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
	if utf8.RuneCountInString(t.Guidance) > MaxGuidanceRunes {
		return ReasonGuidanceTooLong
	}
	return ""
}

// ServesGitHubTokens reports whether a secret of type is released as GitHub installation tokens:
// a github-app secret, whose tokens the broker mints from the App's key, and a
// github-installations one, whose tokens a minter outside the broker stored. Both are served only
// through profiles with a github block on the accounts their github-owners tag lists, in the one
// release form, and both are taken apart alike behind the airlock.
func ServesGitHubTokens(secretType string) bool {
	return secretType == TypeGitHubApp || secretType == TypeGitHubInstallations
}

// WayOf is the way a credential of a secret of secretType goes into a call under kind, and whether
// kind's ways hold it. A secret of no type, a stored value, is swapped in (proxyrules.WaySwap), or,
// under a kind whose ways hold sign and not swap, signed with again (proxyrules.WaySign): that is a
// stored AWS key pair under the aws kind, whose credential goes into no header as it is. A
// github-app, github-installations, google-login, google-service-account or anthropic-federation
// secret's token is minted, or taken from its stored tokens, and swapped in (proxyrules.WayMint),
// and an azure-login secret's is exchanged at Microsoft's token endpoint
// (proxyrules.WayExchange). A type it does not know takes no way. The policy load refuses a secret
// whose way its kind's ways do not hold (ReasonProxyWay): its credential would go into no call.
func WayOf(kind proxyrules.Kind, secretType string) (string, bool) {
	var way string
	switch secretType {
	case "":
		way = proxyrules.WaySwap
		if !slices.Contains(kind.Ways, proxyrules.WaySwap) && slices.Contains(kind.Ways, proxyrules.WaySign) {
			way = proxyrules.WaySign
		}
	case TypeGitHubApp, TypeGitHubInstallations, TypeGoogleLogin, TypeGoogleServiceAccount, TypeAnthropicFederation:
		way = proxyrules.WayMint
	case TypeAzureLogin:
		way = proxyrules.WayExchange
	default:
		return "", false
	}
	return way, slices.Contains(kind.Ways, way)
}

// TypeNeedsGitHubOwners reports whether a secret of type must carry a github-owners tag naming at
// least one account: a secret released as GitHub installation tokens (ServesGitHubTokens), whose
// tokens are handed over for the accounts the tag lists and no other.
func TypeNeedsGitHubOwners(secretType string) bool {
	return ServesGitHubTokens(secretType)
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

// ErrServedOnlyAsProfiles is a request for a source by its own name while profiles of other names
// use it (Set.ProfilesOf) and none holds that name: the source is served only through them, so no
// caller gets the whole credential by the source's name. Evaluate's error names the profiles of it
// the set serves, to ask for instead, or, where the set refuses each of them, says so and names
// them.
var ErrServedOnlyAsProfiles = errors.New("is served only through its profiles")

// ErrProfileRefused is a request for a name the profiles file decides and the policy refuses: a
// profile that failed against its source, or a github-app or github-installations secret no
// profile names (ReasonGitHubAppNoProfile, ReasonGitHubInstallationsNoProfile). Evaluate's error
// gives the reason and its detail.
var ErrProfileRefused = errors.New("is refused by the profiles file")

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
	// GitHubOwners is the github-owners tag of a secret released as GitHub installation tokens
	// (ServesGitHubTokens) as ParseGitHubOwners answers it, and "" for every other secret. A
	// string, not a slice, for the same reason as MaxGrant.
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
	// refuses one that is not served behind the proxy (ReasonProjectChannelsNeedAirlock). It is in
	// Version, so the airlock proxy reads again a value it read before the tag was added.
	ProjectChannels string
	// Admin is true when the secret carries admin: "true" (TagAdmin): its grant is an
	// administrator's, which the rules hold to human tier, a requesters list and an hour's cap
	// (TagRules).
	Admin bool
	// Judge is the secret's judge tag as ParseJudge answers it, whole seconds from 1 to
	// MaxJudgeSeconds, and 0 for a secret carrying none, which the model judge never decides
	// (Set.JudgeSeconds). A value, never a pointer, for the same reason as MaxGrant. It is in
	// Version, so a judge tag added, changed or removed makes the broker check each live judged
	// grant of the secret again at its next release (requests' judgeStillDecides).
	Judge uint32
	// Guidance is the secret's guidance tag (TagGuidance) as written, "" for a secret carrying
	// none: the instruction the model judge follows when it decides a request for the secret. It
	// is not in Version: it tells the judge how to decide requests to come and changes nothing a
	// live grant may do, so writing it ends no grant.
	Guidance string
	// ProfileDigest is the fingerprint of what a name is served as, for each name a grant of which
	// a tag or the profiles file can widen:
	// a profile's entry's, with the kind it is behind, its source's proxy-narrowed tag and, behind
	// the proxy, its source's judge seconds, and its source's scopes tag where it has no google
	// block of its own (Profiles.servedDigest); a stored secret's served by its own name behind the
	// proxy, its proxy, proxy-narrowed and scopes tags (proxyDigest); a stored secret's served by
	// its own name behind no proxy, its scopes tag (scopesDigest); and "" for such a secret with no
	// scopes tag. A string, not a pointer, for the same reason as MaxGrant. A request records it
	// (request_secrets.airlock_digest), and a release ends a grant whose name's fingerprint moved
	// (requests.Machine's checkRelease), so an edit of one profile's entry, or a change of the
	// proxy, proxy-narrowed, judge or scopes tag it reads, ends the grants of that name and no
	// others, across a restart too. It is in Version.
	ProfileDigest string
	// Profile is the profiles file's entry this secret is served as, and nil for a stored secret
	// served by its own name. It points into the one Profiles the boot read, which never changes,
	// so a Secret stays comparable and two publishes of one profile hold the same pointer.
	Profile *Profile
	// Proxy is the kind the secret is served behind (TagProxy), and "" for a secret not behind
	// Cipher's proxy, whose credential the release hands to an approved command. A profile is
	// behind its source's tag's kind, or, where its source carries none, its own proxy string's
	// (Profile.ProxyKind), until the string goes, once every source a profile names carries its
	// own proxy tag.
	Proxy string
	// ProxyNarrowed is true when the secret, or a profile's source, carries proxy-narrowed: "true"
	// (TagProxyNarrowed): the one profile it is served through narrows it at the proxy.
	ProxyNarrowed bool
	// Way is the way the secret's credential goes into a call under its kind (WayOf), and "" for a
	// secret not behind the proxy.
	Way string
	// SessionDefault is true when the secret's default-grant tag is "session": a request naming
	// no lifetime gets a grant that lasts until its session ends instead of DefaultGrantLifetime.
	SessionDefault bool
	// SupersededBy is the stored secret's superseded-by tag (TagSupersededBy) as written, "" for a
	// secret carrying none. No rule reads it but a profile's claim on the secret's name, which is
	// why it is not in Version: what it changes, the profile served or refused, is.
	SupersededBy string
	ownership    ownerKind
}

// ProxyAllow is the allow of the profile the secret is served as (Profile.Allow), which narrows its
// kind's at the proxy, and nil for a stored secret served by its own name, which its kind's allow
// alone bounds. A present empty list allows nothing. It is a method of the profile the Secret holds
// rather than a field of its own, so a Secret stays comparable (Current.RefreshOne's answers): the
// list points into the one Profiles the boot read, which never changes.
func (secret Secret) ProxyAllow() []proxyrules.Allow {
	if secret.Profile == nil {
		return nil
	}
	return secret.Profile.Allow
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
// the model judge decides none of its requests whatever its judge tag says: a secret tagged admin
// (TagAdmin), and an anthropic-federation secret (TypeAnthropicFederation, read from its Source
// with ParseTypeSource), whose token can be an organization administrator's whether or not the
// tag says so. Each of the judge's gates reads it (requests' judgeKey, judgeStillDecides,
// currentPolicyAdmits and JudgePolicy).
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
	// github-owners, requesters, scopes, admin, project, project-channels and judge, and of the
	// fingerprint of each profile it serves and of each name it serves behind the proxy
	// (Secret.ProfileDigest): equal versions are equal policies. A request records
	// the version it was decided under, so a live grant is re-evaluated only once the policy
	// changed. The project-channels tag is in it because the airlock proxy holds a grant's
	// released values per pod and reads them again only when one of the grant's names is reread
	// or this version moves (internal/cipher/proxy/bound.go): a project-channels retag a
	// console or an infra-applier made reaches a pod that already holds values by no other road,
	// and that pod would go on spending the Slack token with no channel limit. Whether a project
	// is private is Dispatch's state and not in it, so a caller re-checks a grant of a tagged name
	// on every use.
	Version string
	Secrets map[string]Secret
	// Refused is every secret under the prefix the policy refuses to serve, keyed by its whole
	// name in the store (the name its RefusedMessage line carries), and every profile it refuses,
	// keyed "profile:" and the profile's name (profileKey), with why. It is not in
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
	// profiles is the profiles file applied to the set (Profiles.apply), and the zero Profiles for
	// a set none were applied to. Every reading of a profile's source and of a source's profiles
	// is its (Profiles.SourceOf, Profiles.Of), built once as the file was read.
	profiles Profiles
	// profileRefused is, for each name a request may ask for that the set refuses because of the
	// profiles file, its refusal as Refused holds it: each failed profile's name, and each
	// github-app or github-installations secret no profile names (ReasonGitHubAppNoProfile,
	// ReasonGitHubInstallationsNoProfile). Evaluate answers it
	// ErrProfileRefused. Refused itself is keyed by store id, which Evaluate, holding no prefix,
	// cannot form.
	profileRefused map[string]Refusal
	// groups is the group reader of the Current that published the set (Loader.Groups), which
	// every decision on the set asks who is a member of a group, and nil for a set no Current
	// published. It is in neither Version nor any digest: who is a member changes with no change
	// of the policy's tags.
	groups record.Directory
	// rules is ProxyRules, built once with the set (setOf, apply), since the proxy reads it on
	// every request and the release on every use of a grant.
	rules proxyrules.Rules
	// redundantProxy is each profile the set serves whose proxy string names the kind its source's
	// proxy tag names too, in the file's order: Current's reload writes ProxyKindRedundantMessage
	// for each, once per load. It goes with the proxy string, once every source a profile names
	// carries its own proxy tag.
	redundantProxy []string
}

// ProfilesOf is the names of the profiles of other names the set's profiles file makes from
// source, in the file's order, whether they pass or fail (Profiles.Of): such a source is served
// only through them, so Evaluate refuses its own name (ErrServedOnlyAsProfiles), naming those
// that pass, unless a profile holds that name too. It is nil for a source no profile of another
// name uses, and for every name of a set no profiles were applied to (Profiles.apply).
func (s *Set) ProfilesOf(source string) []string { return s.profiles.Of(source) }

// ProxyRules is the airlock's rules as the set serves them: the profiles file's kinds and its
// dpop blocks' inline bindings (Profiles.FileRules), and one binding for each name the set serves
// behind the proxy, in name order, with its kind (Secret.Proxy), its way (Secret.Way) and its
// profile's own allow (Secret.ProxyAllow). It is built once when the set is (a load, a reread's
// merge, a value refusal's withhold, the profiles' apply), since the proxy reads it on every
// request and the release on every use of a grant, so it allocates nothing here; a set built with
// no profiles applied answers the file's kinds and the bindings of its tagged secrets all the
// same. proxyrules.Validate accepts it for every set the policy builds from a file ParseProfiles
// accepted. The answer is the set's own and is never changed. A set NewSet builds, outside a
// load, has none.
func (s *Set) ProxyRules() proxyrules.Rules { return s.rules }

// ValueVersion is the version of the type secret name's value as s lists it, and "" for a name s
// lists no type secret's value for: what a mint the decoder refuses hands Current.RefuseValue, so
// the refusal is keyed on the version the policy listed when the value was read.
func (s *Set) ValueVersion(name string) string { return s.versions[name] }

// Refusal is why the policy refuses to serve one secret under the prefix, as a load found it.
type Refusal struct {
	// Reason is one of the Reason* refusal constants.
	Reason string
	// Owner is the secret's owner tag as the store holds it, and "" for ReasonOwnerTagMissing and
	// ReasonOwnerTagMalformed, whose tag is the fault and names nobody the broker knows. A
	// profile's is its source's, "" when the source is not one the store holds.
	Owner string
	// Detail says which part of a profile, or of the store's secret beside it, a profile reason
	// is about (the source's own reason, the account a github block names that the source does
	// not), or, for ReasonValueMalformed, the decoder's own words of what is wrong with the value
	// (secrets.ParseValue, which never quotes the value), or, for a secret's proxy tags, what of
	// its kind they do not fit (the kind its tag names that the file does not declare, the way its
	// type takes that its kind's ways do not hold, the max_grant it is above), and is "" for every
	// other reason that is a secret's own tag's. RefusedMessage writes it as detail.
	Detail string
	// SupersededBy is the refused secret's superseded-by tag (TagSupersededBy) as written, "" for
	// none: a refused secret gives its name to the profile of that source as a served one does
	// (supersededBy), so retiring the old secret's value leaves the profile served.
	SupersededBy string
}

// Pending is one secret under the prefix waiting for its first value (ReasonPendingValue), as a
// load found it. It is no refusal, and so no Refusal.
type Pending struct {
	// Owner is the secret's owner tag as the store holds it.
	Owner string
	// Created is when the store created the secret, so a caller can tell the wait has ended
	// (Current.PendingValue, pendingAt) without a reload.
	Created time.Time
	// SupersededBy is the secret's superseded-by tag (TagSupersededBy) as written, "" for none, as
	// Refusal's is.
	SupersededBy string
}

// setOf is NewSet(secrets) carrying refused, pending and versions, which NewSet's Version leaves
// out, and the proxy rules of profiles' kinds over the secrets it serves (Profiles.proxyRules).
func setOf(profiles Profiles, secrets map[string]Secret, refused map[string]Refusal, pending map[string]Pending, versions map[string]string) *Set {
	set := NewSet(secrets)
	set.Refused, set.Pending, set.versions = refused, pending, versions
	set.rules = profiles.proxyRules(secrets)
	return set
}

// NewSet builds the one Set shape both Load and the single-name merges produce, keeping secrets
// (keyed by Name) as its Secrets. A secret with no max-grant, no github-owners, no requesters, no
// scopes, no admin, no project, no project-channels and no judge tag writes the pre-type digest
// bytes, "<Name>\t<Owner>\t<Tier>\t<Source>\n" (a type secret's type is in its Source); one with
// any of them adds the first two, as a fifth field (time.Duration's form, 0s for none) and a sixth
// (empty for none), and then ends its line with "\trequesters=<list>", "\tscopes=<list>",
// "\tadmin=true", "\tproject=<KEY>", "\tproject-channels=slack" and "\tjudge=<seconds>", in that
// order, for each of those six it carries. The project, project-channels and judge tags are
// written only by a secret that carries one, so the secrets of a deployment that tags none write
// the bytes they wrote before the tag existed and no deployed version shifts. The guidance tag is
// in no line (Secret.Guidance). A secret served as a profile, by its own name behind the proxy, or
// by its own name with a scopes tag, adds one line more, "<Name>\tprofile=<digest>\n"
// (Secret.ProfileDigest), rather than a field of the line above it, so every line a stored secret
// served by its own name behind no proxy and with no scopes tag writes is byte-identical to the one
// it wrote before profiles existed, and its Version does not shift.
// Entries are ascending by the secret's name in the store
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
		judge := ""
		if s.secret.Judge != 0 {
			judge = fmt.Sprintf("\tjudge=%d", s.secret.Judge)
		}
		switch {
		case s.secret.MaxGrant == 0 && s.secret.GitHubOwners == "" && !s.secret.SessionDefault && s.secret.Requesters == "" && s.secret.Scopes == "" && !s.secret.Admin && projectTags == "" && judge == "":
			fmt.Fprintf(digest, "%s\t%s\t%s\t%s\n", s.secret.Name, s.secret.Owner, s.secret.Tier, s.secret.Source)
		case s.secret.SessionDefault:
			fmt.Fprintf(digest, "%s\t%s\t%s\t%s\t%s\t%s\tdefault-grant=session%s%s%s%s%s\n", s.secret.Name, s.secret.Owner, s.secret.Tier, s.secret.Source, s.secret.MaxGrant, s.secret.GitHubOwners, requesters, scopes, admin, projectTags, judge)
		default:
			fmt.Fprintf(digest, "%s\t%s\t%s\t%s\t%s\t%s%s%s%s%s%s\n", s.secret.Name, s.secret.Owner, s.secret.Tier, s.secret.Source, s.secret.MaxGrant, s.secret.GitHubOwners, requesters, scopes, admin, projectTags, judge)
		}
		if s.secret.ProfileDigest != "" {
			fmt.Fprintf(digest, "%s\tprofile=%s\n", s.secret.Name, s.secret.ProfileDigest)
		}
	}
	return &Set{Version: hex.EncodeToString(digest.Sum(nil)), Secrets: secrets}
}

// ServiceOwned reports whether the set serves name as a registered service's secret. Who is a
// service's own session rests on BROKER_SERVICES' accounts, which are not in Version, so a caller
// re-checks a grant of such a name on every use rather than only once Version moves.
func (s *Set) ServiceOwned(name string) bool {
	return s.Secrets[name].ownership == ownerService
}

// JudgeSeconds is the longest grant the model judge may make of name, and whether its key lets the
// judge decide it at all: the judge tag of the secret s serves as name (Secret.Judge), a profile's
// being its source's, and (0, false) for a name s does not serve or whose secret carries no judge
// tag. It reads the tag alone: whether a person approves every release of the secret all the same
// (Secret.PersonApprovesEveryRelease) is each caller's to weigh, since a judged grant of a secret
// tagged an administrator's later is ended by the release (requests' judgeStillDecides) and not
// refused as a broken chain. Every reader of the bound goes through it, and what it answers is at
// most MaxJudgeSeconds, since the loader refuses a judge tag above that (ReasonJudgeOutOfRange).
func (s *Set) JudgeSeconds(name string) (uint32, bool) {
	seconds := s.Secrets[name].Judge
	return seconds, seconds != 0
}
