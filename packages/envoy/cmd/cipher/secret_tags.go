// packages/envoy/cmd/cipher/secret_tags.go
//
// The secret forms' tag rules: what each tag flag accepts, under the broker's own rules, and the
// tags a write sends.
package main

import (
	"errors"
	"fmt"
	"strings"

	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/contracts"
)

// ownerMe is the --owner value that names the person the sign-in names (secretSession.ownerTag).
const ownerMe = "me"

// checkOwnerTier answers the owner --owner names - me, shared, a person's email, lowercased,
// which must be one the broker accepts as a person owner (policy.ValidPersonOwner), or a group as
// group: and its lowercase email (policy.ValidGroupOwner) - and refuses any other --owner and a
// --tier that is neither agent nor human; "" passes either, for a form where it is optional. A
// group is refused rather than lowercased when it holds an upper-case letter, as a requesters
// entry naming it is (policy.ErrRequestersEntry): a group-owned google-service-account secret's
// list names its owner group exactly as the owner tag does (policy.RequesterRule), so the two are
// written alike. A value it refuses is not printed, as no tag flag's is (valueNotPrinted).
func checkOwnerTier(owner, tier string) (string, error) {
	var errs []error
	switch lower := strings.ToLower(owner); {
	case owner == "", owner == ownerMe, owner == policy.OwnerShared, policy.ValidGroupOwner(owner):
	case policy.ValidGroupOwner(lower):
		errs = append(errs, fmt.Errorf("--owner group:EMAIL names a group by its lowercase email, as every requesters entry naming the group does, so it is written in lower case %s", valueNotPrinted))
	case policy.ValidPersonOwner(lower):
		owner = lower
	default:
		errs = append(errs, fmt.Errorf("--owner is %s, %s, a person's email or group:EMAIL, a group's lowercase email %s", ownerMe, policy.OwnerShared, valueNotPrinted))
	}
	if tier != "" && tier != policy.TierAgent && tier != policy.TierHuman {
		errs = append(errs, fmt.Errorf("--tier is %s or %s %s", policy.TierAgent, policy.TierHuman, valueNotPrinted))
	}
	if len(errs) > 0 {
		return "", usageErr{errors.Join(errs...)}
	}
	return owner, nil
}

// valueNotPrinted is how a refusal of a tag flag's value, or of a NAME, stands in for that value:
// a flag's value is where a person who pasted a credential in the wrong place puts it, so every
// such refusal names the flag and the rule the value broke, never the value.
const valueNotPrinted = "(the value is not printed)"

// ruleOf is err, a refusal of one of internal/cipher/policy's tag parsers, as a form prints it: the
// first of rules err is, the parser's own sentinel naming the rule, and never the parser's text,
// which quotes the entry it refused ("not %q"). An error that is none of rules prints as fixed
// text.
func ruleOf(err error, rules ...error) string {
	for _, rule := range rules {
		if errors.Is(err, rule) {
			return rule.Error() + " " + valueNotPrinted
		}
	}
	return "the value does not parse " + valueNotPrinted
}

// tagNone is the value of --type, --github-owners, --requesters, --scopes, --admin, --project,
// --project-channels, --max-grant and --default-grant that removes the tag, the one the broker
// refuses as a github-owners login (policy.TagRemoved).
const tagNone = policy.TagRemoved

// checkType is --type's rule: a type the broker mints (policy.ValidType), or none.
func checkType(secretType string) error {
	if secretType == "" || secretType == tagNone || policy.ValidType(secretType) {
		return nil
	}
	return usageErr{fmt.Errorf("--type is %s or %s %s", strings.Join(policy.Types, ", "), tagNone, valueNotPrinted)}
}

// checkMaxGrant is --max-grant's rule, the broker's own (policy.ParseMaxGrant), or none where
// orNone allows it.
func checkMaxGrant(maxGrant string, orNone bool) error {
	if maxGrant == "" || (orNone && maxGrant == tagNone) {
		return nil
	}
	if _, err := policy.ParseMaxGrant(maxGrant); err != nil {
		return usageErr{fmt.Errorf("--max-grant: %w", err)}
	}
	return nil
}

// checkGitHubOwners is --github-owners' rule, the broker's own (policy.ParseGitHubOwners): GitHub
// account logins separated by spaces, which the shell quotes as one argument; a comma is refused.
// none is a sentinel, not a login: alone, and only where orNone allows it (retag), it removes the
// tag; given on create, or beside a login, or in another case, it is refused, since it would
// otherwise be written as a login or dropped as the removal (optionalTags), leaving a github-app
// secret the broker refuses. It answers the list as the broker reads it, lowercased, sorted and
// single-spaced, so the tag is written as it will be served.
func checkGitHubOwners(owners string, orNone bool) (string, error) {
	if owners == "" || (orNone && owners == tagNone) {
		return owners, nil
	}
	for _, field := range strings.Fields(owners) {
		if !strings.EqualFold(field, tagNone) {
			continue
		}
		if orNone {
			return "", usageErr{fmt.Errorf("--github-owners: none removes the tag and is given alone, as --github-owners none; a GitHub login list names no none %s", valueNotPrinted)}
		}
		return "", usageErr{fmt.Errorf("--github-owners: none names no GitHub account, and removes the tag only on retag; a github-app secret needs at least one login %s", valueNotPrinted)}
	}
	parsed, err := policy.ParseGitHubOwners(owners)
	if err != nil {
		return "", usageErr{errors.New("--github-owners: " + ruleOf(err, policy.ErrGitHubOwnersComma, policy.ErrGitHubOwnersLogin, policy.ErrGitHubOwnersMissing))}
	}
	return parsed, nil
}

// checkGitHubOwnersFor applies the broker's github-owners rule (policy.GitHubOwnersFor) to the
// type and owners a secret will hold once a write lands, so create and retag refuse, before any
// write, a github-app secret the broker would then refuse as github-owners-missing or
// github-owners-malformed, each named as the broker names it. named says the command itself named
// --github-owners: owners named for a type that does not read them are refused too, since the
// broker would ignore them, while owners the secret already holds are left alone whatever its type
// becomes.
func checkGitHubOwnersFor(secretType, owners string, named bool) error {
	if !policy.TypeNeedsGitHubOwners(secretType) {
		if named {
			return usageErr{fmt.Errorf("--github-owners is read only on a %s secret, and this one's type is %q", policy.TypeGitHubApp, secretType)}
		}
		return nil
	}
	_, err := policy.GitHubOwnersFor(secretType, owners)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, policy.ErrGitHubOwnersMissing):
		return usageErr{fmt.Errorf("%s secret needs --github-owners naming at least one GitHub account, or the broker refuses it as %s", withArticle(secretType), policy.ReasonGitHubOwnersMissing)}
	default:
		return usageErr{fmt.Errorf("the secret's github-owners tag %q is malformed (%w), and the broker refuses it as %s: name the accounts with --github-owners", owners, err, policy.ReasonGitHubOwnersMalformed)}
	}
}

// checkRequesters is --requesters' rule, the broker's own (policy.ParseRequesterShapes): people
// by their lowercase sign-in email, Google groups as group: and the group's lowercase email, and
// services by their registered name, separated by spaces, which the shell quotes as one argument;
// a comma is refused. This CLI checks shapes rather than the registered list, since it holds
// none: an entry naming a service is checked for a service name's shape alone here, and a name
// the deployment does not register is caught by the broker's answer to the reread that follows
// the write. Nor does it read a group's members: an entry naming a group is checked for the
// form's shape alone, and the broker reads who is in the group. none is a sentinel, not a
// requester: alone, and only where orNone allows it (retag), it removes the tag; given on
// create, or beside a requester, it is refused, since it would otherwise be written as a service
// named none or dropped as the removal (optionalTags), leaving a secret nobody but none may ask
// for. It answers the list as the broker reads it, sorted, each entry once and single-spaced, so
// the tag is written as it will be served.
func checkRequesters(list string, orNone bool) (string, error) {
	if list == "" || (orNone && list == tagNone) {
		return list, nil
	}
	for _, field := range strings.Fields(list) {
		if !strings.EqualFold(field, tagNone) {
			continue
		}
		if orNone {
			return "", usageErr{fmt.Errorf("--requesters: none removes the tag and is given alone, as --requesters none; a requesters list names no none %s", valueNotPrinted)}
		}
		return "", usageErr{fmt.Errorf("--requesters: none names no person or service, and removes the tag only on retag; leave --requesters off to let every session the owner and tier allow ask %s", valueNotPrinted)}
	}
	parsed, err := policy.ParseRequesterShapes(list)
	if err != nil {
		return "", usageErr{errors.New("--requesters: " + ruleOf(err, policy.ErrRequestersComma, policy.ErrRequestersEntry, policy.ErrRequestersMissing))}
	}
	return parsed, nil
}

// adminTrue is the one value the admin tag holds, the policy's own AdminTrue: anything else, the
// empty string included, the broker refuses as policy.ReasonAdminMalformed, so --admin takes this
// one word, checkAdmin refuses the rest before any write, and checkTagsFor reads the mark off the
// tag by comparing against it. Aliased rather than restated, as tagNone aliases policy.TagRemoved,
// so this CLI cannot drift from the loader on what the tag says.
const adminTrue = policy.AdminTrue

// checkScopes is --scopes' rule, the broker's own (policy.ParseScopes): the names of the OAuth
// scopes a google-service-account secret's token carries, each the part of a Google scope that
// follows https://www.googleapis.com/auth/, separated by spaces, which the shell quotes as one
// argument; a comma, a whole scope URL, an upper-case name, a name given twice and a list naming
// no scope at all are each refused. none is a sentinel, not a scope name: alone, and only where
// orNone allows it (retag), it removes the tag; given on create, or beside a scope, it is
// refused, since "none" passes the broker's own pattern for a scope name and would otherwise be
// written as a scope by that name, or dropped as the removal (optionalTags), leaving a secret
// minting a token good for no API. It answers the list as the broker reads it, sorted and
// single-spaced, so the tag is written as it will be served.
func checkScopes(scopes string, orNone bool) (string, error) {
	if scopes == "" || (orNone && scopes == tagNone) {
		return scopes, nil
	}
	for _, field := range strings.Fields(scopes) {
		if !strings.EqualFold(field, tagNone) {
			continue
		}
		if orNone {
			return "", usageErr{fmt.Errorf("--scopes: none removes the tag and is given alone, as --scopes none; a scope list names no none %s", valueNotPrinted)}
		}
		return "", usageErr{fmt.Errorf("--scopes: none names no OAuth scope, and removes the tag only on retag; leave --scopes off to mint the cloud-platform scope alone %s", valueNotPrinted)}
	}
	parsed, err := policy.ParseScopes(scopes)
	if err != nil {
		return "", usageErr{errors.New("--scopes: " + ruleOf(err, policy.ErrScopesComma, policy.ErrScopesEntry, policy.ErrScopesDuplicate, policy.ErrScopesMissing))}
	}
	return parsed, nil
}

// checkAdmin is --admin's rule: adminTrue, the one value the broker reads (policy.TagAdmin), or
// none where orNone allows it (retag) to remove the tag. Any other value is refused here, before
// anything is read or written, as the broker refuses it on the reread, policy.ReasonAdminMalformed,
// named as the broker names it: the mark says an administrator's credential is behind the secret,
// and a value saying anything else is a mistake rather than a secret that is no administrator's.
func checkAdmin(admin string, orNone bool) error {
	if admin == "" || admin == adminTrue || (orNone && admin == tagNone) {
		return nil
	}
	if orNone {
		return usageErr{fmt.Errorf("--admin is %s or %s %s, or the broker refuses the secret as %s", adminTrue, tagNone, valueNotPrinted, policy.ReasonAdminMalformed)}
	}
	return usageErr{fmt.Errorf("--admin is %s %s, or the broker refuses the secret as %s; leave --admin off for a secret whose grant is no administrator's", adminTrue, valueNotPrinted, policy.ReasonAdminMalformed)}
}

// checkProject is --project's rule, the broker's own (policy.ParseProject): one Dispatch project
// key as written, an upper-case letter then one to nine upper-case letters or digits, or none
// where orNone allows it (retag) to remove the tag. Any other value — a key in lower case, two
// keys, white space around one, and on create the word none — is refused here, before anything
// is read or written, as the broker refuses it on the reread, policy.ReasonProjectMalformed,
// named as the broker names it: a tag the broker cannot read names no project it could check,
// and a secret carrying one is served to nobody. The value it refuses is not printed
// (valueNotPrinted).
func checkProject(project string, orNone bool) error {
	if project == "" || (orNone && project == tagNone) {
		return nil
	}
	if _, err := policy.ParseProject(project); err == nil {
		return nil
	}
	if orNone {
		return usageErr{fmt.Errorf("--project is one Dispatch project key (%s) or %s %s, or the broker refuses the secret as %s", contracts.ProjectKeyRule, tagNone, valueNotPrinted, policy.ReasonProjectMalformed)}
	}
	return usageErr{fmt.Errorf("--project is one Dispatch project key (%s) %s, or the broker refuses the secret as %s; leave --project off for a secret of no project", contracts.ProjectKeyRule, valueNotPrinted, policy.ReasonProjectMalformed)}
}

// checkProjectChannels is --project-channels' rule: slack, the one value the broker reads
// (policy.ProjectChannelsSlack), or none where orNone allows it (retag) to remove the tag. Any
// other value — slack in another case, white space around it, a channel id, and on create the word
// none — is refused here, before anything is read or written, as the broker refuses it on the
// reread, policy.ReasonProjectChannelsMalformed, named as the broker names it: a value the broker
// cannot read names no limit it could apply, and a secret carrying one is served to nobody. The
// value it refuses is not printed (valueNotPrinted).
func checkProjectChannels(channels string, orNone bool) error {
	if channels == "" || channels == policy.ProjectChannelsSlack || (orNone && channels == tagNone) {
		return nil
	}
	if orNone {
		return usageErr{fmt.Errorf("--project-channels is %s or %s %s, or the broker refuses the secret as %s", policy.ProjectChannelsSlack, tagNone, valueNotPrinted, policy.ReasonProjectChannelsMalformed)}
	}
	return usageErr{fmt.Errorf("--project-channels is %s %s, or the broker refuses the secret as %s; leave --project-channels off for a secret no project's owner limits", policy.ProjectChannelsSlack, valueNotPrinted, policy.ReasonProjectChannelsMalformed)}
}

// checkTagsFor applies the broker's cross-tag rules (policy.TagRules) to the tags a secret will
// hold once a write lands — its type, owner, tier, requesters, scopes, admin mark and
// max grant — so create (cmdSecretCreate) and retag (cmdSecretRetag) refuse, before any write,
// a secret the broker would then refuse to load. It builds the one policy.TagSet those rules
// weigh and hands it, beside the reason they answered and the map it was built from, to
// tagRefusal, which says that refusal as the flag that fixes it.
//
// It reads those tags out of the map the write leaves behind — optionalTags with the owner and
// tier create will send, the after map retag computes — rather than out of the flags, because
// optionalTags has already dropped a flag left unset and a flag set to none, so the check reads
// the tags the broker will read: `--type none` leaves a stored secret, which carries no type at
// all and is not a secret of a type called none. Reading the map also leaves the check and the
// write one value, so no tag can be weighed in one form and written in another.
//
// The tier is one of the tags read because two of those rules turn on it: an admin secret is
// human tier, and a google-service-account secret may list a registered service beside its
// owner at human tier alone, so a check passing no tier would refuse a write the broker
// accepts. The max-grant tag is parsed here (policy.ParseMaxGrant) rather than by a second
// --max-grant rule: a malformed one is no duration at all, which the flag's own rule
// (checkMaxGrant) refused for a command that named it, and which the broker refuses as
// max-grant-malformed before it weighs policy.TagRules at all. The admin mark is the tag being
// adminTrue; a stored admin tag holding anything else is no rule of TagRules but
// policy.ReasonAdminMalformed, which the broker answers to the reread that follows the write,
// exactly as it answers for a stored requesters or scopes tag this CLI cannot repair. The
// requesters are read without the project tags' rollback guard (policy.SplitRequestersGuard), as
// the broker reads them: the guard has no @, so the rules would otherwise weigh it as a service on
// the list. The one
// field of the set left empty is BoundAccount, the deployment's BROKER_SERVICE_ACCOUNT_SECRETS
// binding, which `agent-secrets` never sees, just as it never sees BROKER_SERVICES and so calls
// ParseRequesterShapes rather than ParseRequesters: the rule it feeds, the one refusing a
// service on a secret bound to a dev-<name> account, is the broker's to apply on that reread.
func checkTagsFor(tags map[string]string) error {
	duration, _ := policy.ParseMaxGrant(tags[policy.TagMaxGrant])
	requesters, _ := policy.SplitRequestersGuard(tags[policy.TagRequesters])
	set := policy.TagSet{
		Type:         policy.TypeFromTags(tags),
		Owner:        tags[policy.TagOwner],
		Tier:         tags[policy.TagTier],
		Requesters:   requesters,
		Scopes:       tags[policy.TagScopes],
		BoundAccount: "",
		Admin:        tags[policy.TagAdmin] == adminTrue,
		MaxGrant:     duration,
	}
	return tagRefusal(policy.TagRules(set), set, tags)
}

// guardRequesters puts the project tags' rollback guard (policy.RequestersRollbackGuard) into the
// requesters tag of tags, the tags a secret holds once a write lands, when they carry a
// dispatch-project or a project-channels tag, after the list the tag holds and never in place of
// it; and takes it out when they carry neither, removing the requesters tag when the guard was
// all it held. A requesters tag no guard was written into is left exactly as it is, a malformed
// one included, which the broker refuses on the reread. A broker from before the project tags
// reads the guard as a service no deployment can register and refuses the secret, rather than
// serve a project's secret by owner and tier to every session; this broker takes it out before
// it reads the list.
func guardRequesters(tags map[string]string) {
	raw, listed := tags[policy.TagRequesters]
	list, guarded := policy.SplitRequestersGuard(raw)
	_, project := tags[policy.TagProject]
	_, channels := tags[policy.TagProjectChannels]
	switch {
	case project || channels:
		if !listed {
			list = ""
		}
		tags[policy.TagRequesters] = policy.WithRequestersGuard(list)
	case guarded && list == "":
		delete(tags, policy.TagRequesters)
	case guarded:
		tags[policy.TagRequesters] = list
	}
}

// tagRefusal says one cross-tag refusal as the flag that fixes it: reason is what
// policy.TagRules answered for set, and tags is the map set was built from. It answers nil for
// "", the answer for a secret the rules allow, and a usage error for every other reason, so it
// is this CLI's whole mirror of TagRules: an arm for
// policy.ReasonScopesType, policy.ReasonAdminTier, policy.ReasonAdminRequesters,
// policy.ReasonTypeHumanTier, policy.ReasonMaxGrantRequired and policy.ReasonRequestersNotOwner,
// and one fallback refusal
// after the switch, which quotes the reason TagRules gave. That reason is this binary's own
// TagRules answer, so a reason, or a requesters fault, with no arm here is a gap in this build
// of agent-secrets, which the fallback refuses the write for rather than leave it to be written
// and refused on the reread (TestTagRefusalRefusesAReasonNoArmNames). A broker newer than this
// client, holding a rule this client's TagRules does not know, is caught by that reread alone.
//
// Every arm reads the set rather than a flag, so no message can name tags other than the ones
// the rules judged; the max-grant arm reads tags as well, to quote the cap too long as whoever
// named it wrote it. It takes the reason as an argument, rather than calling TagRules itself,
// so a test can hand it one.
func tagRefusal(reason string, set policy.TagSet, tags map[string]string) error {
	switch reason {
	case "":
		return nil
	case policy.ReasonScopesType:
		holder := "a stored secret"
		if set.Type != "" {
			holder = withArticle(set.Type) + " secret"
		}
		return usageErr{fmt.Errorf("--scopes narrows the token a %s secret mints, and no other type's minter reads it, so %s holds none: remove the scopes tag with --scopes none, in a retag of its own, or write --type %s, or the broker refuses it as %s", policy.TypeGoogleServiceAccount, holder, policy.TypeGoogleServiceAccount, policy.ReasonScopesType)}
	case policy.ReasonAdminTier:
		return usageErr{fmt.Errorf("an admin secret releases an administrator's credential, which a person approves each release of, so it is human tier, not %q: write --tier %s, or the broker refuses it as %s", set.Tier, policy.TierHuman, policy.ReasonAdminTier)}
	case policy.ReasonAdminRequesters:
		return usageErr{fmt.Errorf("an admin secret names who may ask for an administrator's credential, never leaving it to owner and tier alone: write --requesters, or the broker refuses it as %s", policy.ReasonAdminRequesters)}
	case policy.ReasonTypeHumanTier:
		return usageErr{fmt.Errorf("%s secret can release an organization administrator's credential, which a person approves each release of, so it is human tier, not %q: write --tier %s, or the broker refuses it as %s", withArticle(set.Type), set.Tier, policy.TierHuman, policy.ReasonTypeHumanTier)}
	case policy.ReasonMaxGrantRequired:
		demands := withArticle(set.Type) + " secret"
		if set.Admin {
			demands = "an admin secret"
		}
		if set.MaxGrant == 0 {
			return usageErr{fmt.Errorf("%s releases a credential whose grant is reviewed while it is live, so it bounds its grants to an hour: name one with --max-grant 1h or less, or the broker refuses it as %s", demands, policy.ReasonMaxGrantRequired)}
		}
		// The cap too long is quoted as the tag holds it, which is how whoever named it wrote
		// it ("90m"), and not as the duration it parsed to ("1h30m0s"), which is the one thing
		// the set says less plainly than the map.
		return usageErr{fmt.Errorf("%s releases a credential whose grant is reviewed while it is live, so it bounds its grants to an hour, not %s: write --max-grant 1h or less, or the broker refuses it as %s", demands, tags[policy.TagMaxGrant], policy.ReasonMaxGrantRequired)}
	case policy.ReasonRequestersNotOwner:
		// Which of the requesters conditions refused the write is policy.RequesterRule's
		// answer, weighed there in the policy's own order, most fundamental first, so the flag
		// each arm names fixes the condition that refused it and not another that happens to be
		// true beside it: a secret owned by shared is answered for its owner however its list
		// and tier read, and a list naming a service at agent tier is answered for the tier and
		// not for the person it leaves out.
		switch fault := policy.RequesterRule(set); fault {
		case policy.RequesterFaultOwnerNotPerson:
			return usageErr{fmt.Errorf("%s secret mints a token acting as that account, so it is served to the one person who owns it, or to the members of the one group that does, and %q is neither a person nor a group: own it with --owner me or --owner group:EMAIL, or the broker refuses it as %s", withArticle(set.Type), set.Owner, policy.ReasonRequestersNotOwner)}
		case policy.RequesterFaultServiceBelowHumanTier:
			_, services, _ := policy.SplitRequesters(set.Requesters)
			return usageErr{fmt.Errorf("%s secret lists a registered service, here %q, only at %s tier, where a person approves each pod that asks: write --tier %s, or leave the service out of --requesters, or the broker refuses it as %s", withArticle(set.Type), services[0], policy.TierHuman, policy.TierHuman, policy.ReasonRequestersNotOwner)}
		case policy.RequesterFaultNotTheOwnerOnce, policy.RequesterFaultNotTheOwnerGroupOnce:
			// One message for every way a list fails to name the owner exactly once — no list
			// at all, a list of services alone, a list naming another person or a group, and
			// for a group owner a list naming a person or another group — because one write
			// fixes them all: the owner, beside the registered services the list already
			// names, which a human-tier secret of this type may keep. A list the secret holds
			// is quoted too, so the reader sees what the suggestion replaces.
			_, services, _ := policy.SplitRequesters(set.Requesters)
			wanted := strings.Join(append([]string{set.Owner}, services...), " ")
			held := ""
			if set.Requesters != "" {
				held = fmt.Sprintf(", not %q", set.Requesters)
			}
			if fault == policy.RequesterFaultNotTheOwnerGroupOnce {
				return usageErr{fmt.Errorf("%s secret owned by a group is served to the group's members, beside the registered services a %s-tier one may list, so its requesters name %s once and no person%s: write --requesters %q, or the broker refuses it as %s", withArticle(set.Type), policy.TierHuman, set.Owner, held, wanted, policy.ReasonRequestersNotOwner)}
			}
			return usageErr{fmt.Errorf("%s secret is served to the one person who owns it, beside the registered services a %s-tier one may list, so its requesters name %s once%s: write --requesters %q, or the broker refuses it as %s", withArticle(set.Type), policy.TierHuman, set.Owner, held, wanted, policy.ReasonRequestersNotOwner)}
		}
	}
	// Two things reach here: a reason no case above names, and a policy.RequesterFault no case of
	// the inner switch names, policy.RequesterFaultServiceOnDevAccount among them. That fault
	// turns on the deployment's binding of the secret to an account, which checkTagsFor never
	// holds and always passes as an empty BoundAccount, so no run of this CLI reaches it that
	// way, while a caller handing tagRefusal a set of its own can.
	return usageErr{fmt.Errorf("the broker refuses a secret carrying these tags as %s, and this agent-secrets has no message naming the flag that fixes it: write tags the broker accepts", reason)}
}

// tagFlags is the value of each tag flag a create or a retag names, one field per flag. It is a
// struct rather than ten string parameters for the reason policy.TagSet is one: a caller
// passing that many values in order would transpose two of them, and every transposition
// compiles and writes each value as another tag.
type tagFlags struct {
	Type            string
	GitHubOwners    string
	Requesters      string
	Scopes          string
	Admin           string
	Project         string
	ProjectChannels string
	MaxGrant        string
	DefaultGrant    string
}

// optionalTags is the type, github-owners, requesters, scopes, admin, dispatch-project,
// project-channels, max-grant and default-grant tags the flags set: each flag's value when it
// names one, and nothing for one left unset or set to none. A type brings the rollback guard with
// it (policy.TagRollbackGuard), which only the brokers from legion-envoy-v13.19.0 to the release
// before v16.0.0 read, and refuse: every broker from v16.0.0 to the release before the type tag
// ignores it and serves a secret that carries type alone as a stored secret. What keeps a type
// secret's key from such a broker is requireTypeTag, before this CLI writes, and a rollback floor
// at the release that reads the type tag, which the deployment keeps.
func optionalTags(f tagFlags) map[string]string {
	named := func(value string) bool { return value != "" && value != tagNone }
	tags := map[string]string{}
	if named(f.Type) {
		tags[policy.TagType] = f.Type
		tags[policy.TagRollbackGuard] = policy.RollbackGuardValue
	}
	if named(f.GitHubOwners) {
		tags[policy.TagGitHubOwners] = f.GitHubOwners
	}
	if named(f.Requesters) {
		tags[policy.TagRequesters] = f.Requesters
	}
	if named(f.Scopes) {
		tags[policy.TagScopes] = f.Scopes
	}
	if named(f.Admin) {
		tags[policy.TagAdmin] = f.Admin
	}
	if named(f.Project) {
		tags[policy.TagProject] = f.Project
	}
	if named(f.ProjectChannels) {
		tags[policy.TagProjectChannels] = f.ProjectChannels
	}
	if named(f.MaxGrant) {
		tags[policy.TagMaxGrant] = f.MaxGrant
	}
	if named(f.DefaultGrant) {
		tags[policy.TagDefaultGrant] = f.DefaultGrant
	}
	return tags
}

// checkDefaultGrant is --default-grant's rule: "session", or none where orNone allows it.
func checkDefaultGrant(value string, orNone bool) error {
	if value == "" || value == policy.DefaultGrantSession || (orNone && value == tagNone) {
		return nil
	}
	return usageErr{fmt.Errorf("--default-grant is %s %s", policy.DefaultGrantSession, valueNotPrinted)}
}
