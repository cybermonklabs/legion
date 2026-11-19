// packages/envoy/cmd/agent-secrets/secret_retag.go
//
// The retag form: a secret's tag change, written against the tags it was checked on.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"

	"github.com/sjawhar/envoy/internal/broker/policy"
)

// heldTags is a secret's tags as one read of the store held them: AWS tags or Secret Manager
// annotations, and, on GCP, that read's etag ("" on AWS), which a tag write sends so it lands only
// on the tags it was checked against.
type heldTags struct {
	tags map[string]string
	etag string
}

// errTagsChanged is a tag write another write raced: it changed the secret after the read the
// write was made against. GCP refuses the write (an etag that no longer matches), so nothing is
// written; AWS, whose tag calls take no condition, finds the race by reading the tags back
// (awsBackend.raceCheck), where its own write has already landed beside the other writer's, and
// wraps errTagsRaced too.
var errTagsChanged = errors.New("another write changed the secret after this retag read it")

// errTagsRaced is the part of an AWS errTagsChanged that says this retag's write landed and
// nothing was put back, so the secret now holds both writes and the broker must be asked to
// reread it. Nothing is restored: the tags this retag read are no more current than the other
// writer's, and restoring them would take that writer's retag away.
var errTagsRaced = errors.New("this retag's write landed beside another writer's, and nothing was restored")

// tagPairs is tags as sorted `key=value` pairs separated by single spaces, for a message that
// names one read of a secret's tags beside another. A tag is metadata, never a value.
func tagPairs(tags map[string]string) string {
	pairs := make([]string, 0, len(tags))
	for key, value := range tags {
		pairs = append(pairs, key+"="+value)
	}
	slices.Sort(pairs)
	if len(pairs) == 0 {
		return "no tags"
	}
	return strings.Join(pairs, " ")
}

// cmdSecretRetag changes a secret's owner, tier, type, GitHub owners, requesters, scopes, admin
// mark, max grant or default grant, in one cloud write. A retag that names none
// (--type none, --github-owners none, --requesters none, --scopes none, --admin none,
// --max-grant none, --default-grant none) removes those tags and does nothing else; --type none
// also removes the old kind tag and the rollback guard written with the type. Any other retag sets
// tags. A retag that does both is refused before anything is read or written: AWS removes tags
// only with UntagResource, so it would be two calls, and a refused second call would leave it
// half applied.
//
// Either kind of retag reads the tags the secret holds, builds from them the tags it will hold
// once the write lands (after), and checks the github-owners rule and the broker's cross-tag
// rules against those once (checkGitHubOwnersFor, checkTagsFor), so it leaves no github-app
// secret without GitHub owners, names no owners for a type that does not read them, and leaves
// no secret the broker would then refuse for its scopes, its admin mark, its max
// grant or who may ask for it. Whether an entry naming a service is one the deployment
// registers is the one thing neither check can answer, since this CLI holds no list of
// registered services: the broker's answer to the reread below is what catches it, and the
// retag exits 1 with the tag written. The write is made against that read (heldTags), and a
// write another writer raced answers errTagsChanged, which the retag prints, exiting 1; on AWS,
// whose own write landed beside that writer's and put nothing back (errTagsRaced), it also asks
// the broker to reread.
//
// A retag that leaves the secret a type is refused, before anything is written, by a broker whose
// settings do not say it reads the type tag (requireTypeTag). A retag that changes the type tag
// then reads the secret's value and decodes it as the broker will once the write lands
// (checkRetagValue): a value that type's minter refuses is refused before anything is written, and
// a read the store refuses as access denied leaves the retag to go ahead, saying on stderr that the
// value was not checked.
//
// A retag that sets tags always sends owner and tier, re-sending the one not named as the secret
// holds it: on AWS, the deployment repository's IAM policy that lets a person tag their own secret
// conditions on both request tags, so a request carrying one fails. GCP has no equivalent
// condition (annotations are not IAM-conditionable the way AWS tags are): anyone with
// secretmanager.secrets.update on the namespace can retag any secret under it, shared ones
// included; sending both tags there is for API symmetry with AWS, not an enforced shared-secret
// protection.
func cmdSecretRetag(args []string, stdout, stderr io.Writer) int {
	const form = "secret retag"
	flagArgs, positional := splitArgs(args, secretFlagValues)
	flags := secretFlags(form, stderr)
	newOwner := flags.String("owner", "", "the new owner: me (the email of the person your AWS or Google sign-in names), shared, or a person's email")
	newTier := flags.String("tier", "", "the new tier: agent or human")
	newType := flags.String("type", "", strings.Join(policy.Types, ", ")+" or none")
	newGitHubOwners := flags.String("github-owners", "", `the GitHub accounts a github-app secret mints for, separated by spaces ("<login> <login>"), or none`)
	newRequesters := flags.String("requesters", "", `the people (each by their lowercase sign-in email) and registered services that may ask for this secret at all, separated by spaces ("<email> <service>"), or none; an admin secret and a google-service-account one each need a --max-grant of an hour or less, and a secret that only lists who may ask keeps its own cap, or none`)
	newScopes := flags.String("scopes", "", `the OAuth scopes the token a google-service-account secret mints carries, each the part of a Google scope after https://www.googleapis.com/auth/, separated by spaces ("cloud-platform bigquery"), or none; read on that type alone, and a secret without it mints cloud-platform`)
	newAdmin := flags.String("admin", "", "true marks a secret whose grant is an administrator's, or none removes the mark: such a secret is human tier, so a person approves each release, names its requesters, and bounds its grants to an hour with --max-grant")
	newMaxGrant := flags.String("max-grant", "", "the longest grant duration this secret allows, or none")
	newDefaultGrant := flags.String("default-grant", "", "session (no --ttl lasts until the session ends) or none (12 hours)")
	if err := flags.Parse(flagArgs); err != nil {
		return exitUsage(err)
	}
	name, slug, err := secretName(positional)
	if err == nil && *newOwner == "" && *newTier == "" && *newType == "" && *newGitHubOwners == "" && *newRequesters == "" && *newScopes == "" && *newAdmin == "" && *newMaxGrant == "" && *newDefaultGrant == "" {
		err = usageErr{errors.New("--owner, --tier, --type, --github-owners, --requesters, --scopes, --admin, --max-grant, --default-grant or a combination is required")}
	}
	if err == nil {
		*newOwner, err = checkOwnerTier(*newOwner, *newTier)
	}
	if err == nil {
		err = checkType(*newType)
	}
	if err == nil {
		*newGitHubOwners, err = checkGitHubOwners(*newGitHubOwners, true)
	}
	if err == nil {
		*newRequesters, err = checkRequesters(*newRequesters, true)
	}
	if err == nil {
		*newScopes, err = checkScopes(*newScopes, true)
	}
	if err == nil {
		err = checkAdmin(*newAdmin, true)
	}
	if err == nil {
		err = checkMaxGrant(*newMaxGrant, true)
	}
	if err == nil {
		err = checkDefaultGrant(*newDefaultGrant, true)
	}
	var remove []string
	if *newType == tagNone {
		// The rollback guard goes with the type: a stored secret is safe on every broker.
		remove = append(remove, policy.TagType, policy.LegacyTypeTag, policy.TagRollbackGuard)
	}
	if *newGitHubOwners == tagNone {
		remove = append(remove, policy.TagGitHubOwners)
	}
	if *newRequesters == tagNone {
		remove = append(remove, policy.TagRequesters)
	}
	if *newScopes == tagNone {
		remove = append(remove, policy.TagScopes)
	}
	if *newAdmin == tagNone {
		remove = append(remove, policy.TagAdmin)
	}
	if *newMaxGrant == tagNone {
		remove = append(remove, policy.TagMaxGrant)
	}
	if *newDefaultGrant == tagNone {
		remove = append(remove, policy.TagDefaultGrant)
	}
	set := optionalTags(tagFlags{
		Type:         *newType,
		GitHubOwners: *newGitHubOwners,
		Requesters:   *newRequesters,
		Scopes:       *newScopes,
		Admin:        *newAdmin,
		MaxGrant:     *newMaxGrant,
		DefaultGrant: *newDefaultGrant,
	})
	if err == nil && len(remove) > 0 && (*newOwner != "" || *newTier != "" || len(set) > 0) {
		err = usageErr{errors.New("none removes a tag in a retag of its own: run the removal and the other changes as two retags")}
	}
	if err != nil {
		return secretFail(stderr, form, err)
	}
	s, err := openSession(context.Background(), stderr, form)
	if err != nil {
		return secretFail(stderr, form, err)
	}
	defer s.Close()
	held, err := s.backend.currentTags(s.ctx, slug)
	if err != nil {
		return secretFailIn(stderr, form, s, err)
	}
	// after is the secret's tags once this retag lands: the tags held, less those it removes, with
	// those it sets.
	after := maps.Clone(held.tags)
	if after == nil {
		after = map[string]string{}
	}
	for _, key := range remove {
		delete(after, key)
	}
	maps.Copy(after, set)
	if *newOwner != "" {
		if after[policy.TagOwner], err = s.ownerTag(*newOwner); err != nil {
			return secretFailIn(stderr, form, s, err)
		}
	}
	if *newTier != "" {
		after[policy.TagTier] = *newTier
	}
	named := *newGitHubOwners != "" && *newGitHubOwners != tagNone
	if err := checkGitHubOwnersFor(policy.TypeFromTags(after), after[policy.TagGitHubOwners], named); err != nil {
		return secretFailIn(stderr, form, s, err)
	}
	if err := checkTagsFor(after); err != nil {
		return secretFailIn(stderr, form, s, err)
	}
	owner, tier := after[policy.TagOwner], after[policy.TagTier]
	if len(remove) == 0 && (owner == "" || tier == "") {
		return secretFailIn(stderr, form, s, usageErr{fmt.Errorf("%s has no owner tag or no tier tag, and a retag sends both: name the missing one with --owner or --tier", name)})
	}
	if err := s.requireTypeTag(name, policy.TypeFromTags(after)); err != nil {
		return secretFailIn(stderr, form, s, err)
	}
	unchecked, err := checkRetagValue(s, name, slug, held, after)
	if err != nil {
		return secretFailIn(stderr, form, s, err)
	}
	if len(remove) > 0 {
		if err := s.backend.removeTags(s.ctx, slug, held, remove); err != nil {
			fmt.Fprintf(stderr, "agent-secrets %s: %v; looked in %s\n", form, s.refusedAs(err), s.storeName())
			rereadIfRaced(s, stdout, stderr, form, name, err)
			return 1
		}
		fmt.Fprintf(stdout, "retagged %s in %s (removed %s)\n", name, s.storeName(), strings.Join(remove, ", "))
		if unchecked != "" {
			fmt.Fprintf(stderr, "agent-secrets %s: %s\n", form, unchecked)
		}
		return rereadRetag(s, stdout, stderr, form, name, policy.TypeFromTags(after))
	}
	set[policy.TagOwner], set[policy.TagTier] = owner, tier
	if err := s.backend.setTags(s.ctx, slug, held, set); err != nil {
		fmt.Fprintf(stderr, "agent-secrets %s: %v; looked in %s\n", form, s.refusedAs(err), s.storeName())
		// AWS: IAM conditioned on the request tags leaves a shared secret's owner and tier to
		// administrators specifically. GCP has no per-secret condition on secretmanager.secrets.update,
		// so a deployment instead grants that permission at all only to administrators for the
		// secrets it protects this way.
		if s.backend.isAccessDenied(err) && (held.tags[policy.TagOwner] == policy.OwnerShared || owner == policy.OwnerShared) {
			fmt.Fprintf(stderr, "agent-secrets %s: %s\n", form, sharedRetagRefused)
		}
		rereadIfRaced(s, stdout, stderr, form, name, err)
		return 1
	}
	fmt.Fprintf(stdout, "retagged %s in %s (owner=%s, tier=%s)\n", name, s.storeName(), owner, tier)
	if unchecked != "" {
		fmt.Fprintf(stderr, "agent-secrets %s: %s\n", form, unchecked)
	}
	return rereadRetag(s, stdout, stderr, form, name, policy.TypeFromTags(after))
}

// checkRetagValue decodes the value of the secret name, slug under the prefix, as the broker will
// read it once a retag leaves it with after's tags, held being the tags it holds now, when the
// retag changes what the broker reads the value as: the type tag. A retag that changes it not at
// all reads no value, and one that leaves no type, or
// a type the broker does not mint (which it refuses as type-unknown), leaves nothing to decode.
// People commonly hold the right to retag a secret and not to read its value, so the read has
// three outcomes, each handled apart:
//
//   - the value reads: a value the type's minter refuses is refused (checkValueFor), and the
//     retag writes nothing;
//   - the store refuses the read as access denied, and only as that (isAccessDenied): the retag
//     goes ahead, and unchecked is the line it prints once its write lands, saying the value was
//     not checked, naming the permissions the read needs (valueReadPermission), and saying that
//     the broker refuses such a value itself and reports the secret unservable with the
//     decoder's reason;
//   - any other read error refuses the retag, and it writes nothing.
func checkRetagValue(s *secretSession, name, slug string, held heldTags, after map[string]string) (unchecked string, err error) {
	secretType := policy.TypeFromTags(after)
	if !policy.ValidType(secretType) || secretType == policy.TypeFromTags(held.tags) {
		return "", nil
	}
	value, err := s.backend.readValue(s.ctx, slug)
	switch {
	case err == nil:
		return "", checkValueFor(secretType, value)
	case s.backend.isAccessDenied(err):
		return fmt.Sprintf("the tags are written, but %s's value was not checked as %s secret's: the store refused this sign-in the read, which needs %s. The broker refuses a value its decoder refuses, and reports %s unservable with the decoder's reason", name, withArticle(secretType), s.backend.valueReadPermission(), name), nil
	default:
		return "", fmt.Errorf("read %s's value to check it as %s secret's, so nothing is written: %w", name, withArticle(secretType), err)
	}
}

// rereadIfRaced asks the broker to reread name after a tag write that landed beside another
// writer's, leaving a pair the broker refuses (errTagsRaced, AWS): the
// secret's tags changed twice, so the broker is asked to serve them as they now stand. A write the
// store refused wrote nothing (GCP's errTagsChanged alone), and needs no reread.
func rereadIfRaced(s *secretSession, stdout, stderr io.Writer, form, name string, err error) {
	if errors.Is(err, errTagsRaced) {
		rereadAfter(s, stdout, stderr, form, name, true, nil)
	}
}

// rereadRetag is rereadAfter for a retag, which also exits 1 when the secret does not hold type,
// the one the retag left it (after's). A retag that leaves a type reads its type tag back from
// the store under the sign-in that wrote it (typeTagHolds). The broker's own answer, to a
// session's reread, must serve the secret as that type: a reread can read the secret's metadata
// stale (Secret Manager's is eventually consistent for up to hours), and a secret the broker
// still serves as stored releases its stored value raw, so a key stored in it then reaches every
// grant of it. Only this exit, not `broker: serving`, says the broker reads the new tags. A reread
// the broker took and said no more of, all a call with no proof is answered, leaves that view
// unchecked: the retag says so, naming where to run it again, and exits 0 on the tag alone.
func rereadRetag(s *secretSession, stdout, stderr io.Writer, form, name, secretType string) int {
	r, unsigned, ok := reread(s, stdout, stderr, form, name, true, nil)
	if !ok {
		return 1
	}
	if secretType != "" && !typeTagHolds(s, stderr, form, name, secretType) {
		return 1
	}
	if !r.Told {
		if secretType == "" {
			sayRereadUnchecked(s, stderr, form, name, unsigned)
		} else {
			sayTypeUnserved(s, stderr, form, name, secretType, unsigned)
		}
		return 0
	}
	if r.Type != secretType {
		fmt.Fprintf(stderr, "agent-secrets %s: the retag in %s stands, but %s serves %s as %s, not %s: it still sees the old tags. Run this retag again to ask it to reread them, and store no value in %s until a retag exits 0\n", form, s.storeName(), s.src.brokerAt(), name, typeName(r.Type), typeName(secretType), name)
		return 1
	}
	return 0
}

// typeName is type as rereadRetag names it: itself, or "a stored secret" for none.
func typeName(secretType string) string {
	if secretType == "" {
		return "a stored secret"
	}
	return secretType
}
