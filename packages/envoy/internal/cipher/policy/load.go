package policy

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/record"
	"github.com/sjawhar/envoy/internal/cipher/secrets"
	"github.com/sjawhar/envoy/internal/metrics"
)

// RefusedMessage is the line Current writes, at ERROR, when a secret under the prefix becomes one
// the policy refuses to serve, or is refused for another reason than before: name (the secret's
// whole name in the store, or "profile:" and a profile's name for a profile the policy refuses),
// reason (one of the Reason* values), owner (Refusal.Owner: its owner tag, "" for
// ReasonOwnerTagMissing and ReasonOwnerTagMalformed), first_load (true on every line of the pod's
// first load, which writes one for each secret it refuses, false after it), on a change of
// reason, was (the reason before), and, for a refusal that carries one, detail (Refusal.Detail). A
// load or a reread that refuses a secret for the reason the live policy already held writes
// nothing: the line is the record of when the state changed, and broker_unservable_secrets
// (NewUnservableSecrets) is what holds the standing state. The operate guide documents its text,
// so the text changes only with that guide.
const RefusedMessage = "agent secret policy refused"

// ProfilesAppliedMessage is the line Current writes, at INFO, once, at its first publish: served
// and refused, the counts of the profiles file's entries it serves and refuses, and file_digest,
// the SHA-256 of the file's bytes as the boot read it ("" with no profiles file). Each refused
// profile also writes RefusedMessage, named "profile:" and its name. A reader of a pod's log then
// tells a boot that refused no profile from one that read no file, or never started. The operate
// guide documents its text, and the production check reads it.
const ProfilesAppliedMessage = "broker: profiles applied"

// ServableMessage is the line Current writes, at INFO, when a secret the live policy refused is
// refused no longer: name, reason (the reason it had) and owner as its refusal last held them,
// and now, which is servable when the policy serves it and absent when the store no longer holds
// it (deleted, or scheduled for deletion).
const ServableMessage = "agent secret policy no longer refused"

// PendingMessage is the line Current writes, at INFO, when a secret starts waiting for its value
// (ReasonPendingValue): name, reason, owner and first_load, as RefusedMessage carries them. The
// operate guide documents its text, so the text changes only with that guide.
const PendingMessage = "agent secret policy waiting for a value"

// PendingValueWindow is how long after its creation a secret `agent-secrets secret create` made
// before its value (secrets.Listing.Unfilled) is waiting for one (ReasonPendingValue) rather than
// refused (ReasonNoCurrentValue). It is also the create's value deadline: a create with nothing of
// the entry typed by then ends its read and discards the secret, so no secret is pending that its
// create has given up on. A create whose entry is under way at the deadline reads on until the
// entry ends - at Enter, or at a signal that ends the process, such as a hang-up, or Ctrl-C typed
// outside a paste where it still sends SIGINT - and discards the secret only then, so the broker
// may refuse it no-current-value before the create deletes it.
const PendingValueWindow = 10 * time.Minute

// pendingAt is the one statement of the window: whether a secret the store created at created,
// made before its value, still waits for it at now. A secret whose creation time the store did
// not name waits for nothing. Load and LoadOne weigh it at the loader's clock (Loader.clock), and
// Current.PendingValue at Current's, which NewCurrent makes the loader's, so the load, the reread
// and the PENDING_VALUE answer read one clock.
func pendingAt(created, now time.Time) bool {
	return !created.IsZero() && now.Sub(created) < PendingValueWindow
}

// LoadFailedMessage is written, at ERROR, by the reload NewCurrent runs every interval, on the
// first reload of a run that fails while its context is live; the previous policy stays in force,
// and the reloads of the run after it write nothing until one loads again (LoadRecoveredMessage).
// requests writes it too, with the name, for one request whose reread of a name failed.
const LoadFailedMessage = "agent secret policy load failed; previous policy kept"

// LoadRecoveredMessage is written, at INFO, by the reload NewCurrent runs, on the first reload
// that succeeds after a run of failed reloads that wrote LoadFailedMessage.
const LoadRecoveredMessage = "agent secret policy load succeeded again"

// ValueReadFailedMessage is the line Current writes, at WARN, when it reads a type secret's stored
// value to decode it (ReasonValueMalformed) and the read fails while that secret's reads had not
// been failing: name (the secret's whole name in the store), version (the version of the value it
// read, secrets.Listing.Version) and error (the store's error, which names the call and the class
// of the failure, such as AccessDeniedException, PermissionDenied or a deadline, and never a byte
// of the value). A read whose value is not text (secrets.ErrValueNotText) is no failed read but
// the decoder's case, ReasonValueMalformed. A failed read decodes nothing and keeps a
// value-malformed refusal of the version it read, but a failed read of a version listed since
// that refusal ends it, the secret servable again until a read decodes the new version. Each load
// after a failed read reads the value again and writes nothing more until a read succeeds
// (ValueReadRecoveredMessage), so a failure of any length is two lines.
const ValueReadFailedMessage = "agent secret value read failed; its state kept"

// ValueReadRecoveredMessage is the line Current writes, at INFO, when a read of a type secret's
// value succeeds after a run of failed reads that wrote ValueReadFailedMessage: name and version,
// as that line names them.
const ValueReadRecoveredMessage = "agent secret value read succeeded again"

// Why a secret under the prefix is refused, as its Refusal's Reason and its RefusedMessage line's
// reason.
const (
	// ReasonNameMalformed: the name under the prefix is not lowercase letters, digits and single
	// hyphens starting with a letter, so it maps to no name a session can ask for.
	ReasonNameMalformed = "name-malformed"
	// ReasonOwnerTagMissing: the secret has no owner tag.
	ReasonOwnerTagMissing = "owner-tag-missing"
	// ReasonOwnerTagMalformed: the owner tag is not shared, a lowercase email, a group as
	// group:<lowercase email>, or a registered service.
	ReasonOwnerTagMalformed = "owner-tag-malformed"
	// ReasonTierTagMissing: the secret has no tier tag.
	ReasonTierTagMissing = "tier-tag-missing"
	// ReasonTierTagMalformed: the tier tag is not agent or human.
	ReasonTierTagMalformed = "tier-tag-malformed"
	// ReasonServiceOwnerHumanTier: a service owns a human-tier secret, which nobody could approve.
	ReasonServiceOwnerHumanTier = "service-owner-human-tier"
	// ReasonNotOnAgentSecretsKey: the secret is encrypted with a key other than the agent-secrets
	// key, the AWS-managed key included.
	ReasonNotOnAgentSecretsKey = "not-on-agent-secrets-key"
	// ReasonNoCurrentValue: no version of the secret carries the AWSCURRENT staging label, the one
	// GetSecretValue reads: it was created without a value, or its only versions are pending or
	// previous. A grant of it would release nothing, so it is refused before anyone approves it.
	// Checked after every other reason, which each name something its owner must fix before a
	// value would make it servable.
	ReasonNoCurrentValue = "no-current-value"
	// ReasonPendingValue: a secret `agent-secrets secret create` made before its value
	// (secrets.Listing.Unfilled: the create label and no version) that the store created less than
	// PendingValueWindow ago, still waiting for that value. It is no refusal: the secret is in
	// Set.Pending, not Set.Refused, so it is in no broker_unservable_secrets series, and Current
	// writes PendingMessage for it, once. A request for it is refused ErrPendingValue. Once the
	// window has passed, a secret still without a value is refused ReasonNoCurrentValue.
	ReasonPendingValue = "pending-value"
	// ReasonTypeUnknown: the type tag names no type this broker can mint (ValidType).
	ReasonTypeUnknown = "type-unknown"
	// ReasonMaxGrantMalformed: max-grant is not a Go duration of at least one second
	// (ParseMaxGrant).
	ReasonMaxGrantMalformed = "max-grant-malformed"
	// ReasonGitHubOwnersMissing: a github-app or github-installations secret has no github-owners
	// tag, or one that names no account (ParseGitHubOwners). A public App can be installed on any
	// account, so the broker hands over tokens only for the accounts the deployment lists.
	ReasonGitHubOwnersMissing = "github-owners-missing"
	// ReasonGitHubOwnersMalformed: a github-app or github-installations secret's github-owners
	// tag holds a comma, its logins being separated by spaces, or an entry that is not a GitHub
	// login (ParseGitHubOwners).
	ReasonGitHubOwnersMalformed = "github-owners-malformed"
	// ReasonDefaultGrantMalformed: default-grant is set to anything but "session".
	ReasonDefaultGrantMalformed = "default-grant-malformed"
	// ReasonRequestersMalformed: the requesters tag holds a comma, names nobody, names a person
	// by anything but their lowercase sign-in email, or names a service the deployment does not
	// register (ParseRequesters).
	ReasonRequestersMalformed = "requesters-malformed"
	// ReasonMaxGrantRequired: an admin secret, a google-service-account one or an
	// anthropic-federation one sets no max-grant, or one longer than an hour (TagRules). Each
	// releases a credential whose grant is reviewed while it is live, so it lasts an hour at most.
	ReasonMaxGrantRequired = "max-grant-required"
	// ReasonRequestersNotOwner: a google-service-account or anthropic-federation secret whose
	// requesters are not its owner, once — for a person owner, no list, no entry naming the owner,
	// more than one person, a person who is not the owner, or a group; for a group owner, no list,
	// another group, more than one group, or a person — or which lists a service at agent tier,
	// where no person approves the pod that asks (TagRules). A human-tier secret may list
	// registered services beside its owner, unless the deployment binds it to a dev-<name>
	// account, whose token is that one person's.
	ReasonRequestersNotOwner = "requesters-not-owner"
	// ReasonAdminMalformed: the admin tag is set to anything but "true" (TagAdmin), the empty
	// string included. A tag whose value says nothing is a mistake, not a secret that is no
	// administrator's.
	ReasonAdminMalformed = "admin-malformed"
	// ReasonAdminTier: an admin secret is agent tier (TagRules). An administrator's credential is
	// released only by a person's approval, which agent tier never asks for.
	ReasonAdminTier = "admin-tier"
	// ReasonAdminRequesters: an admin secret carries no requesters tag (TagRules). Who may ask for
	// an administrator's credential is named, never left to owner and tier alone.
	ReasonAdminRequesters = "admin-requesters"
	// ReasonTypeHumanTier: a secret of a type the broker serves at human tier alone,
	// anthropic-federation, is agent tier (TagRules). Its token can be an organization
	// administrator's, which a person approves each release of.
	ReasonTypeHumanTier = "type-human-tier"
	// ReasonScopesMalformed: the scopes tag holds a comma, names no scope, names one twice, or
	// holds an entry that is no scope name — a whole https://www.googleapis.com/auth/… URL
	// included (ParseScopes).
	ReasonScopesMalformed = "scopes-malformed"
	// ReasonScopesType: a secret that is not a google-service-account one carries a scopes tag
	// (TagRules). No other minter reads it, so the tag would narrow nothing while saying it had.
	ReasonScopesType = "scopes-type"
	// ReasonProjectMalformed: the dispatch-project tag is not one Dispatch project key
	// (ParseProject): another case, white space, an empty value or the word none. A tag the
	// broker cannot read names no project it could check, so the secret goes to nobody rather
	// than to every session as an untagged one would.
	ReasonProjectMalformed = "project-malformed"
	// ReasonProjectUnchecked: the secret carries a dispatch-project tag and the broker holds no
	// client of Dispatch's project access (Loader.ProjectChecks is false), so it cannot tell
	// whether the project is private. It is refused rather than served by owner and tier alone,
	// which would release a private project's secret to every session.
	ReasonProjectUnchecked = "project-unchecked"
	// ReasonProjectChannelsMalformed: the project-channels tag is not ProjectChannelsSlack, its
	// one value. A value the broker cannot read names no limit it could apply, so the secret goes
	// to nobody rather than out unlimited to a private project's credential.
	ReasonProjectChannelsMalformed = "project-channels-malformed"
	// ReasonProjectTagMisspelled: a tag key differs from dispatch-project or project-channels
	// only in case or in its separators (Dispatch-Project, dispatch_project, PROJECT-CHANNELS,
	// dispatchproject). The broker reads tag keys as written, so such a secret would otherwise
	// load as one carrying no project tag and go by owner and tier to every session, which is
	// what the owner who wrote it meant to stop.
	ReasonProjectTagMisspelled = "project-tag-misspelled"
	// ReasonValueMalformed: a type secret's stored value is one secrets.ParseValue refuses under
	// its type, or one that is not text (secrets.ErrValueNotText), so no release could
	// mint from it. It is no tag's: a load sets it from the value Current reads and decodes once
	// for each version, type it lists, and a mint sets it from its own decode
	// (Current.RefuseValue). It is keyed on the secret and the version refused: later loads keep
	// it while they list that version, a new version is decoded afresh and keeps it only when
	// that value is refused too, and a value that decodes or a secret that leaves the listing
	// ends it.
	ReasonValueMalformed = "value-malformed"
	// ReasonProfileSourceMissing: a profile's source is not a stored secret the policy serves:
	// none of that name is under the prefix (a profile is never the source of another), or the one
	// there is refused or waits for its value. Its detail names the source and that reason.
	ReasonProfileSourceMissing = "profile-source-missing"
	// ReasonProfileNameTaken: a profile's name is a stored secret's other than its source, and that
	// stored secret is not superseded by the profile's source (TagSupersededBy). The profile is
	// refused, and so is that stored secret, by its own id, until one of the two goes: a name a
	// profile holds is never released as a stored value.
	ReasonProfileNameTaken = "profile-name-taken"
	// ReasonProfileBlockMismatch: a profile's github or google block does not fit its source's
	// type and tags: a github block on a source that is no github-app or github-installations
	// secret (ServesGitHubTokens), or naming an account the source's github-owners does not; such a
	// source's profile with no github block; or a google block on a source that is no
	// google-service-account secret, or naming a scope the source's scopes tag does not. Its detail
	// says which.
	ReasonProfileBlockMismatch = "profile-block-mismatch"
	// ReasonProfileLooser: a profile is looser than its source: agent tier on a human-tier source,
	// a requester the source's requesters tag does not name (a source with no such tag serves
	// every session and pod, so any list narrows it), or a maxGrant above the source's max-grant.
	// Its detail says which.
	ReasonProfileLooser = "profile-looser"
	// ReasonProfileNeedsProxy: a profile with no proxy block on a stored value, a google-login or
	// an azure-login source, whose credential a profile cannot narrow at a mint, so only the
	// proxy's hosts, methods and paths scope it.
	ReasonProfileNeedsProxy = "profile-needs-proxy"
	// ReasonProfileMinted: a profile whose proxy block is of kind dpop. It is proxy configuration
	// for a capability the broker mints for a grant of its own and holds, never a secret the
	// policy serves, so serve answers it whatever the store holds - with no stored secret of its
	// name, and with one, which would otherwise be requested and approved like any stored secret -
	// and apply serves it as nothing and records it in no Refused, since it is no fault to report.
	ReasonProfileMinted = "profile-minted"
	// ReasonGitHubAppNoProfile: a github-app secret no profile names, as its own name or as its
	// source. Every GitHub token the broker hands over is a profile's, held to that profile's
	// accounts and permissions, so such a secret is refused by its name rather than released with
	// every permission its installations hold. Such a secret profiles of other names use is no
	// refusal: it is served through them (ErrServedOnlyAsProfiles).
	ReasonGitHubAppNoProfile = "github-app-no-profile"
	// ReasonGitHubInstallationsNoProfile: a github-installations secret no profile names, as its
	// own name or as its source, refused by its name for the reason ReasonGitHubAppNoProfile gives:
	// every GitHub token the broker hands over is a profile's.
	ReasonGitHubInstallationsNoProfile = "github-installations-no-profile"
)

// slugPattern is a secret's name under the prefix: the lowercase form of an environment-variable
// name, with each underscore a hyphen.
var slugPattern = regexp.MustCompile(`^[a-z][a-z0-9]*(-[a-z0-9]+)*$`)

// emailPattern is an owner tag naming a person: their lowercase email, as their sign-in names them.
var emailPattern = regexp.MustCompile(`^[a-z0-9._%+'-]+@[a-z0-9-]+(\.[a-z0-9-]+)+$`)

// ValidPersonOwner reports whether an owner tag names a person by lowercase email.
func ValidPersonOwner(owner string) bool {
	return emailPattern.MatchString(owner)
}

// ValidGroupOwner reports whether an owner tag, or a requesters entry, names a Google group:
// record.GroupPrefix and the group's lowercase email, group:<address>.
func ValidGroupOwner(owner string) bool {
	address, ok := strings.CutPrefix(owner, record.GroupPrefix)
	return ok && emailPattern.MatchString(address)
}

// Loader reads the policy from the secret store: every secret whose name starts with Prefix, with
// its owner and tier tags, whether it is on the agent-secrets key, and whether it has a current
// value.
type Loader struct {
	// Store lists, describes and reads the secrets: secrets.AWS over Secrets Manager and KMS (in
	// local development and tests, over a secrets.Local), or secrets.GCP over Secret Manager. Its
	// metadata may answer a write's previous state for up to its ListLag, for which Current keeps
	// and re-describes what a single-name read settled. Secrets Manager's DescribeSecret shows a
	// write at once; Secret Manager documents its GetSecret as eventually consistent too, so
	// there a reread settles a name only once Secret Manager does.
	Store secrets.Store
	// Prefix is the namespace every agent secret's name starts with: a Secrets Manager name
	// prefix ending in "/", or a Secret Manager one, projects/<number>/secrets/<id prefix>-.
	Prefix string
	// Services are the registered services a secret's owner tag may name: the broker's
	// BROKER_SERVICES. An owner tag naming any other service is refused as malformed.
	Services []string
	// ServiceAccounts is the deployment's BROKER_SERVICE_ACCOUNT_SECRETS: each google-service-account
	// and anthropic-federation secret's Name to the one account email the deployment binds to it.
	// The loader reads it for one rule alone — a secret bound to a dev-<name> account, which is
	// one person's own, may list no service among its requesters (TagRules) — and the minter is
	// what refuses a mint for any account but the bound one, so a stored value rewritten to name
	// another account releases no token. An empty or nil map binds nothing, and every such secret
	// then stands on its tags alone.
	ServiceAccounts map[string]string
	// Profiles is the deployment's profiles file (BROKER_PROFILES_FILE), the zero Profiles for a
	// deployment with none. Load and LoadOne read the store alone, and Current applies the
	// profiles to every set it makes live (withhold): each is served by its name as its source
	// allows, or refused, and a source profiles of other names use is served only through them.
	Profiles Profiles
	// StoreReads counts each call to the store this loader makes, and Current makes through it,
	// in broker_policy_store_reads_total (NewStoreReads): read=list for the List a Load takes,
	// read=describe for the Describe a LoadOne takes, and read=value for each Read of a type
	// secret's value Current decodes, one for each version, type it lists. Each is
	// counted as it is made, so a call the store fails is counted too: it was asked of the store
	// all the same. In production: NewStoreReads on the registry the metrics port serves. A nil
	// counter counts nothing, which is every Loader a test builds that is not about the count.
	StoreReads *metrics.Counter
	// Groups is the broker's group reader (groups.Reader): Current stamps it on every set it
	// makes live (Set.Groups), so each decision asks it who is a member of a group. A nil
	// directory admits nobody to any group.
	Groups record.Directory
	// ProjectChecks is true when the broker holds a client of Dispatch's project access
	// (BROKER_PROJECT_ACCESS_URL), which resolves whether a tagged secret's project is private
	// before Evaluate reads it. A loader without one refuses every secret carrying a
	// dispatch-project tag (ReasonProjectUnchecked) and serves every other secret as before.
	ProjectChecks bool
	// now is the clock the loader weighs a secret waiting for its value by (pendingAt): Current's
	// own, which NewCurrent sets, and time.Now while it is nil, for a Loader no Current holds.
	now func() time.Time
}

// clock is the time by l's clock (Loader.now).
func (l Loader) clock() time.Time {
	if l.now == nil {
		return time.Now()
	}
	return l.now()
}

// Load reads the namespace into a Set: each secret it serves, each it refuses with why
// (Set.Refused, refusalOf), each waiting for its value (Set.Pending) by the loader's clock
// (pendingAt), and the version of each served type secret's value as the listing names it, which
// Current decodes. It reads no value, and writes no line but LegacyTypeTagMessage, once per
// secret per process, for a secret it reads through the old kind tag (listingType): Current,
// which keeps the set before, writes the lines a change of state calls for, and reads the values.
// A failed store call fails the whole load.
func (l Loader) Load(ctx context.Context) (*Set, error) {
	now := l.clock()
	l.StoreReads.Inc(readList)
	listings, err := l.Store.List(ctx, l.Prefix)
	if err != nil {
		return nil, err
	}
	served, refused, pending, versions := map[string]Secret{}, map[string]Refusal{}, map[string]Pending{}, map[string]string{}
	for _, ls := range listings {
		// The namespace is the prefix as written: a name outside it is skipped, whatever the
		// lister answered.
		slug, ok := strings.CutPrefix(ls.ID, l.Prefix)
		if !ok {
			continue
		}
		secret, reason, err := l.secret(ctx, slug, ls, now)
		if err != nil {
			return nil, err
		}
		switch reason {
		case "":
		case ReasonPendingValue:
			pending[ls.ID] = pendingOf(ls)
			continue
		default:
			refused[ls.ID] = refusalOf(ls, reason)
			continue
		}
		served[secret.Name] = secret
		if secret.Type() != "" {
			versions[secret.Name] = ls.Version
		}
	}
	return setOf(served, refused, pending, versions), nil
}

// refusalOf is the Refusal of the secret ls lists for reason: its owner tag as the store holds
// it, or "" for the two reasons that tag is itself the fault, missing or malformed, and its
// superseded-by tag as written.
func refusalOf(ls secrets.Listing, reason string) Refusal {
	switch reason {
	case ReasonOwnerTagMissing, ReasonOwnerTagMalformed:
		return Refusal{Reason: reason, SupersededBy: ls.Tags[TagSupersededBy]}
	}
	return Refusal{Reason: reason, Owner: ls.Tags[TagOwner], SupersededBy: ls.Tags[TagSupersededBy]}
}

// pendingOf is the Pending of the secret ls lists waiting for its value: its owner tag, when
// the store created it, and its superseded-by tag.
func pendingOf(ls secrets.Listing) Pending {
	return Pending{Owner: ls.Tags[TagOwner], Created: ls.Created, SupersededBy: ls.Tags[TagSupersededBy]}
}

// Lookup is LoadOne's answer: Served with the Secret; or Reason, a Reason* refusal constant, with
// Owner as a Refusal carries it; or ReasonAbsent or ReasonPendingValue, neither of which is a
// refusal. Current.RefreshOne answers one too, with Moved.
type Lookup struct {
	Secret Secret
	Served bool
	Reason string
	Owner  string
	// Moved is, in Current.RefreshOne's answer, every name whose value that reread may have
	// moved (movedByReread): the names the airlock's proxy drops what it holds of. LoadOne
	// leaves it nil.
	Moved []string
	// version is a served type secret's value's version, as Set's versions holds it, and "" for
	// every other answer.
	version string
	// created is, for ReasonPendingValue, when the store created the secret.
	created time.Time
	// supersededBy is, for a refusal or ReasonPendingValue, the secret's superseded-by tag, as
	// Refusal and Pending carry it.
	supersededBy string
}

// refusal is the Refusal lk answers, and the zero Refusal for a secret served, absent or pending.
func (lk Lookup) refusal() Refusal {
	if lk.Served || lk.Reason == ReasonAbsent || lk.Reason == ReasonPendingValue {
		return Refusal{}
	}
	return Refusal{Reason: lk.Reason, Owner: lk.Owner, SupersededBy: lk.supersededBy}
}

// pending is the Set.Pending entry lk answers, and the zero Pending for a secret not pending.
func (lk Lookup) pending() Pending {
	if lk.Reason != ReasonPendingValue {
		return Pending{}
	}
	return Pending{Owner: lk.Owner, Created: lk.created, SupersededBy: lk.supersededBy}
}

// secretName is the one place that answers whether name can name a secret under the prefix: its
// slug and its whole name in the store when it can, and ErrNameInvalid when it is not of
// namePattern's form or is a name the store refuses (Store.ValidName: past its length limit, or
// outside its character set), so no lookup is made for one.
func (l Loader) secretName(name string) (slug, id string, err error) {
	slug, err = NameToSlug(name)
	if err != nil {
		return "", "", err
	}
	id = l.Prefix + slug
	if err := l.Store.ValidName(id); err != nil {
		return "", "", fmt.Errorf("%w: %v", ErrNameInvalid, err)
	}
	return slug, id, nil
}

// LoadOne reads the one secret a session asks for as name, by the rules Load applies to each
// secret it lists, answering the refusal Load would hand to the Set when it refuses it; like Load,
// it writes no line but LegacyTypeTagMessage. A name no secret can carry (secretName) is
// ErrNameInvalid, with no call made; a failed store call is an error.
func (l Loader) LoadOne(ctx context.Context, name string) (Lookup, error) {
	slug, id, err := l.secretName(name)
	if err != nil {
		return Lookup{}, err
	}
	l.StoreReads.Inc(readDescribe)
	ls, err := l.Store.Describe(ctx, id)
	switch {
	case errors.Is(err, secrets.ErrNotFound):
		return Lookup{Reason: ReasonAbsent}, nil
	case err != nil:
		return Lookup{}, err
	}
	secret, reason, err := l.secret(ctx, slug, ls, l.clock())
	if err != nil {
		return Lookup{}, err
	}
	if reason == ReasonPendingValue {
		waiting := pendingOf(ls)
		return Lookup{Reason: reason, Owner: waiting.Owner, created: waiting.Created, supersededBy: waiting.SupersededBy}, nil
	}
	if reason != "" {
		refusal := refusalOf(ls, reason)
		return Lookup{Reason: refusal.Reason, Owner: refusal.Owner, supersededBy: refusal.SupersededBy}, nil
	}
	lk := Lookup{Secret: secret, Served: true}
	if secret.Type() != "" {
		lk.version = ls.Version
	}
	return lk, nil
}

// secret reads one secret under the prefix at now, answering the reason it is refused when it is.
// It asks the store whether the secret is on the agent-secrets key only after every tag check, so
// a secret refused for its name or tags costs no key lookup; a failed lookup is an error.
func (l Loader) secret(ctx context.Context, slug string, ls secrets.Listing, now time.Time) (Secret, string, error) {
	if !slugPattern.MatchString(slug) {
		return Secret{}, ReasonNameMalformed, nil
	}
	s := Secret{Name: SlugToName(slug), Source: ls.Source}
	var ok bool
	if s.Owner, ok = ls.Tags[TagOwner]; !ok {
		return Secret{}, ReasonOwnerTagMissing, nil
	}
	switch {
	case s.Owner == OwnerShared:
		s.ownership = ownerShared
	case ValidGroupOwner(s.Owner):
		s.ownership = ownerGroup
	case ValidPersonOwner(s.Owner):
		s.ownership = ownerPerson
	case slices.Contains(l.Services, s.Owner):
		s.ownership = ownerService
	default:
		return Secret{}, ReasonOwnerTagMalformed, nil
	}
	if s.Tier, ok = ls.Tags[TagTier]; !ok {
		return Secret{}, ReasonTierTagMissing, nil
	}
	if s.Tier != TierAgent && s.Tier != TierHuman {
		return Secret{}, ReasonTierTagMalformed, nil
	}
	if s.ownership == ownerService && s.Tier == TierHuman {
		return Secret{}, ReasonServiceOwnerHumanTier, nil
	}
	onKey, err := ls.OnKey(ctx)
	if err != nil {
		return Secret{}, "", err
	}
	if !onKey {
		return Secret{}, ReasonNotOnAgentSecretsKey, nil
	}
	if raw, tagged := ls.Tags[TagMaxGrant]; tagged {
		if s.MaxGrant, err = ParseMaxGrant(raw); err != nil {
			return Secret{}, ReasonMaxGrantMalformed, nil
		}
	}
	if raw, tagged := ls.Tags[TagDefaultGrant]; tagged {
		if raw != DefaultGrantSession {
			return Secret{}, ReasonDefaultGrantMalformed, nil
		}
		s.SessionDefault = true
	}
	if secretType := listingType(ls); secretType != "" {
		if !ValidType(secretType) {
			return Secret{}, ReasonTypeUnknown, nil
		}
		if s.GitHubOwners, err = GitHubOwnersFor(secretType, ls.Tags[TagGitHubOwners]); errors.Is(err, ErrGitHubOwnersMissing) {
			return Secret{}, ReasonGitHubOwnersMissing, nil
		} else if err != nil {
			return Secret{}, ReasonGitHubOwnersMalformed, nil
		}
		s.Source = TypeSource(secretType, ls.Source)
	}
	// Read after max-grant and type, both of which the tag rules weigh: Secret.Type reads the type
	// back off the Source the branch above froze it into. The admin and scopes tags are
	// read here too, since the rules weigh them against the type, the tier and the requesters
	// alike. A type whose own rules demand a list is refused for a missing one, so TagRules runs
	// whether the tags are there or not.
	// The project tags' rollback guard is no requester (RequestersRollbackGuard): it is taken out
	// before the list is read, and a tag that held it alone is no list.
	if raw, tagged := ls.Tags[TagRequesters]; tagged {
		if rest, guarded := SplitRequestersGuard(raw); !guarded || rest != "" {
			if s.Requesters, err = ParseRequesters(rest, l.Services); err != nil {
				return Secret{}, ReasonRequestersMalformed, nil
			}
		}
	}
	if raw, tagged := ls.Tags[TagAdmin]; tagged {
		if raw != AdminTrue {
			return Secret{}, ReasonAdminMalformed, nil
		}
		s.Admin = true
	}
	if raw, tagged := ls.Tags[TagScopes]; tagged {
		if s.Scopes, err = ParseScopes(raw); err != nil {
			return Secret{}, ReasonScopesMalformed, nil
		}
	}
	if reason := TagRules(TagSet{
		Type:         s.Type(),
		Owner:        s.Owner,
		Tier:         s.Tier,
		Requesters:   s.Requesters,
		Scopes:       s.Scopes,
		Admin:        s.Admin,
		MaxGrant:     s.MaxGrant,
		BoundAccount: l.ServiceAccounts[s.Name],
	}); reason != "" {
		return Secret{}, reason, nil
	}
	// Read after the rules that weigh the other tags: no rule of TagRules reads the project.
	// A misspelled key, and then a malformed tag, is the tag's own fault, refused before whether
	// the broker could check it.
	if misspelledProjectKey(ls.Tags) {
		return Secret{}, ReasonProjectTagMisspelled, nil
	}
	if raw, tagged := ls.Tags[TagProject]; tagged {
		if s.Project, err = ParseProject(raw); err != nil {
			return Secret{}, ReasonProjectMalformed, nil
		}
		if !l.ProjectChecks {
			return Secret{}, ReasonProjectUnchecked, nil
		}
	}
	// The same place, for the same reason: no rule of TagRules reads it, and a value the broker
	// cannot read names no limit it could apply.
	if raw, tagged := ls.Tags[TagProjectChannels]; tagged {
		if raw != ProjectChannelsSlack {
			return Secret{}, ReasonProjectChannelsMalformed, nil
		}
		s.ProjectChannels = raw
	}
	// Read as written: only a profile's claim on this secret's name reads it (supersededBy), and a
	// value that is no profile's source matches none, so the name stays the stored secret's.
	s.SupersededBy = ls.Tags[TagSupersededBy]
	if !ls.HasCurrentValue {
		// A secret `agent-secrets secret create` made before its value (Unfilled: the create label,
		// no version) is a create waiting for that value for PendingValueWindow (pendingAt), whose
		// comment says when the create deletes it; past the window it is refused here
		// no-current-value until then. Any other secret without a value, and one whose creation
		// time the store did not name, is refused so at once.
		if ls.Unfilled && pendingAt(ls.Created, now) {
			return Secret{}, ReasonPendingValue, nil
		}
		return Secret{}, ReasonNoCurrentValue, nil
	}
	return s, "", nil
}

// misspelledProjectKey reports whether tags hold a key that is not TagProject or
// TagProjectChannels as written but is one of them once both are read without case and without
// any character that is not a letter or a digit (ReasonProjectTagMisspelled).
func misspelledProjectKey(tags map[string]string) bool {
	fold := func(key string) string {
		return strings.Map(func(r rune) rune {
			switch {
			case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
				return r
			case r >= 'A' && r <= 'Z':
				return r + ('a' - 'A')
			}
			return -1
		}, key)
	}
	for key := range tags {
		if key == TagProject || key == TagProjectChannels {
			continue
		}
		if folded := fold(key); folded == fold(TagProject) || folded == fold(TagProjectChannels) {
			return true
		}
	}
	return false
}
