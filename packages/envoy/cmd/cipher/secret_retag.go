// packages/envoy/cmd/cipher/secret_retag.go
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

	"github.com/sjawhar/envoy/internal/cipher/policy"
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
// marks it so (tagsLanded).
var errTagsChanged = errors.New("another write changed the secret after this retag read it")

// tagsLanded is a tag write's error that came after the write itself landed: an AWS tag call takes
// no condition, so the write is made first and read back after (awsBackend.raceCheck), and a race
// the read-back finds, or a read-back that fails, is answered with the write already in the store.
// Its text is the error's own; it only marks the error, so a message can say the write landed
// (writeLanded) rather than describe the tags as they were before it.
type tagsLanded struct{ err error }

func (e tagsLanded) Error() string { return e.err.Error() }

// Unwrap is the error, so errors.Is still finds errTagsChanged through it.
func (e tagsLanded) Unwrap() error { return e.err }

// writeLanded reports whether err, a tag write's error, came after the write landed (tagsLanded).
func writeLanded(err error) bool {
	var landed tagsLanded
	return errors.As(err, &landed)
}

// tagsUnknown is a tag write's error that leaves open whether the write landed: the store answered
// with no definite refusal, a service fault, an outage or a lost connection, which may follow a
// call it carried out (an AWS call is a definite refusal only when every attempt it made ended in
// a code awsDefiniteRefusals lists, awsAttempts.definiteRefusal; a GCP update only for a code
// gcpDefiniteRefusal names). Its text is the store's error with that said after it. It marks the
// error so a message says the outcome is unknown (writeUnknown) rather than describe the tags as
// they were before the write. It has no Unwrap: the store's error is in its text alone, so no reader
// of a refusal (isAccessDenied, refusedAs's sign-in advice, the shared-secret line) takes an open
// outcome for one. An access denial on an SDK retry after an attempt that may have applied refuses
// nothing that is known to be undone.
type tagsUnknown struct{ err error }

func (e tagsUnknown) Error() string {
	return e.err.Error() + "; the store's answer leaves open whether this write landed"
}

// writeUnknown reports whether err, a tag write's error, leaves open whether the write landed
// (tagsUnknown).
func writeUnknown(err error) bool {
	var unknown tagsUnknown
	return errors.As(err, &unknown)
}

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
// mark, project, project channels, max grant or default grant, in one cloud write; a removal that
// moves the project tags' rollback guard writes the requesters tag in a second write after it
// (guardRetag, writeGuardRewrite). A retag that names none (--type none, --github-owners none,
// --requesters none, --scopes none, --admin none, --project none, --project-channels none,
// --max-grant none, --default-grant none) removes those tags and sets none of the user's; --type
// none also removes the old kind tag and the rollback guard written with the type. Any other retag
// sets tags. A retag that does both is refused before anything is read or written: AWS removes
// tags only with UntagResource, so it would be two calls, and a refused second call would leave it
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
// write another writer raced answers errTagsChanged, which the retag prints, exiting 1. A write
// that landed with an error (AWS: a race beside that writer's, or a read-back that failed;
// tagsLanded), and one whose answer leaves open whether it landed (a service fault, an outage, a
// lost connection; tagsUnknown), also ask the broker to reread, and the retag says what it knows
// of the write and never what the tags held before it.
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
	newOwner := flags.String("owner", "", "the new owner: me (the email of the person your AWS or Google sign-in names), shared, a person's email, or group: and a Google group's lowercase email")
	newTier := flags.String("tier", "", "the new tier: agent or human")
	newType := flags.String("type", "", strings.Join(policy.Types, ", ")+" or none")
	newGitHubOwners := flags.String("github-owners", "", `the GitHub accounts a github-app secret mints for, or a github-installations secret hands stored tokens over for, separated by spaces ("<login> <login>"), or none`)
	newRequesters := flags.String("requesters", "", `the people (each by their lowercase sign-in email), Google groups (each as group: and its lowercase email) and registered services that may ask for this secret at all, separated by spaces ("<email> group:<email> <service>"), or none; an admin secret, a google-service-account one and an anthropic-federation one each need a --max-grant of an hour or less, and a secret that only lists who may ask keeps its own cap, or none`)
	newScopes := flags.String("scopes", "", `the OAuth scopes the token a google-service-account secret mints carries, each the part of a Google scope after https://www.googleapis.com/auth/, separated by spaces ("cloud-platform bigquery"), or none; read on that type alone, and a secret without it mints cloud-platform`)
	newAdmin := flags.String("admin", "", "true marks a secret whose grant is an administrator's, or none removes the mark: such a secret is human tier, so a person approves each release, names its requesters, and bounds its grants to an hour with --max-grant")
	newProject := flags.String("project", "", "the key of the Dispatch project the secret belongs to (PRIV), whose private secret goes only to a session naming the project in AGENT_SECRETS_PROJECTS, or none: the secret then belongs to no project")
	newProjectChannels := flags.String("project-channels", "", "slack (a session or pod whose credential names a private Dispatch project spends the secret only through the airlock, on a Slack post to a channel that project's owner named) or none")
	newMaxGrant := flags.String("max-grant", "", "the longest grant duration this secret allows, or none")
	newDefaultGrant := flags.String("default-grant", "", "session (no --ttl lasts until the session ends) or none (12 hours)")
	if err := flags.Parse(flagArgs); err != nil {
		return exitUsage(err)
	}
	name, slug, err := secretName(positional)
	if err == nil && *newOwner == "" && *newTier == "" && *newType == "" && *newGitHubOwners == "" && *newRequesters == "" && *newScopes == "" && *newAdmin == "" && *newProject == "" && *newProjectChannels == "" && *newMaxGrant == "" && *newDefaultGrant == "" {
		err = usageErr{errors.New("--owner, --tier, --type, --github-owners, --requesters, --scopes, --admin, --project, --project-channels, --max-grant, --default-grant or a combination is required")}
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
		err = checkProject(*newProject, true)
	}
	if err == nil {
		err = checkProjectChannels(*newProjectChannels, true)
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
	if *newProject == tagNone {
		remove = append(remove, policy.TagProject)
	}
	if *newProjectChannels == tagNone {
		remove = append(remove, policy.TagProjectChannels)
	}
	if *newMaxGrant == tagNone {
		remove = append(remove, policy.TagMaxGrant)
	}
	if *newDefaultGrant == tagNone {
		remove = append(remove, policy.TagDefaultGrant)
	}
	set := optionalTags(tagFlags{
		Type:            *newType,
		GitHubOwners:    *newGitHubOwners,
		Requesters:      *newRequesters,
		Scopes:          *newScopes,
		Admin:           *newAdmin,
		Project:         *newProject,
		ProjectChannels: *newProjectChannels,
		MaxGrant:        *newMaxGrant,
		DefaultGrant:    *newDefaultGrant,
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
	rewrite, err := guardRetag(held.tags, after, set, &remove)
	if err != nil {
		return secretFailIn(stderr, form, s, err)
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
			reportRemovalFailure(s, stderr, form, name, rewrite, err)
			rereadIfLanded(s, stdout, stderr, form, name, err)
			return 1
		}
		fmt.Fprintf(stdout, "retagged %s in %s (removed %s)\n", name, s.storeName(), strings.Join(remove, ", "))
		if unchecked != "" {
			fmt.Fprintf(stderr, "agent-secrets %s: %s\n", form, unchecked)
		}
		if rewrite != "" {
			if err := writeGuardRewrite(s, slug, after, rewrite); err != nil {
				reportRewriteFailure(s, stderr, form, name, rewrite, held, err)
				if writeUnknown(err) {
					// The removal landed, and whether the rewrite did is open, so the reread
					// claims nothing of a write: rereadRetag would say "the write … stands", and a
					// person who reads that may leave the secret without its guard.
					rereadOpen(s, stdout, stderr, form, name)
					return 1
				}
				// The removal landed, so the broker is asked to reread whatever the rewrite did.
				rereadRetag(s, stdout, stderr, form, name, policy.TypeFromTags(after))
				return 1
			}
			fmt.Fprintf(stdout, "retagged %s in %s (requesters=%s)\n", name, s.storeName(), rewrite)
		}
		return rereadRetag(s, stdout, stderr, form, name, policy.TypeFromTags(after))
	}
	set[policy.TagOwner], set[policy.TagTier] = owner, tier
	if err := s.backend.setTags(s.ctx, slug, held, set); err != nil {
		reportSetFailure(s, stderr, form, name, unchecked, owner, held, err)
		rereadIfLanded(s, stdout, stderr, form, name, err)
		return 1
	}
	fmt.Fprintf(stdout, "retagged %s in %s (owner=%s, tier=%s)\n", name, s.storeName(), owner, tier)
	if unchecked != "" {
		fmt.Fprintf(stderr, "agent-secrets %s: %s\n", form, unchecked)
	}
	return rereadRetag(s, stdout, stderr, form, name, policy.TypeFromTags(after))
}

// guardRetag applies guardRequesters to after, the tags a retag leaves, and folds the change it
// makes to the requesters tag into the retag's own write: set and remove are the tags the flags
// set and remove, and heldTags what the secret holds now. A retag that sets tags writes the
// requesters tag guardRequesters leaves in the same write, the list a command names with the
// guard after it. A retag that removes tags removes the requesters tag too where it held nothing
// but the guard, and answers rewrite, the value the requesters tag must then be written as, where
// the guard leaves a list or joins one: a second write after the removal, in that order, so a
// second write that fails leaves the guard on a secret that no longer needs it, which only a
// broker from legion-envoy-v29.23.0 (the first to read requesters) up to the release before the
// project tags reads, as a refusal. A retag that sets tags never removes
// the requesters tag: a guard left on a secret with no project tag, by a console's removal of the
// tag, stays until a removal retag takes it out.
//
// --requesters none on a secret that keeps a project tag removes the list and keeps the guard, so
// the requesters tag is written as the guard alone: a write, not a removal. Beside another
// removal that is two writes of the user's own asking, which a retag refuses, so it is refused
// here too, before any write.
func guardRetag(heldTags, after, set map[string]string, remove *[]string) (rewrite string, err error) {
	guardRequesters(after)
	heldList, heldListed := heldTags[policy.TagRequesters]
	afterList, afterListed := after[policy.TagRequesters]
	removing := slices.Contains(*remove, policy.TagRequesters)
	switch {
	case afterListed && removing:
		*remove = slices.DeleteFunc(*remove, func(key string) bool { return key == policy.TagRequesters })
		if len(*remove) > 0 {
			return "", usageErr{fmt.Errorf("--requesters none on a secret that keeps a %s or %s tag writes the requesters tag as %s alone, the rollback guard, which is a write and no removal: run --requesters none in a retag of its own", policy.TagProject, policy.TagProjectChannels, policy.RequestersRollbackGuard)}
		}
		set[policy.TagRequesters] = afterList
	case afterListed && len(*remove) == 0:
		if _, named := set[policy.TagRequesters]; named || !heldListed || afterList != heldList {
			set[policy.TagRequesters] = afterList
		}
	case afterListed:
		if !heldListed || afterList != heldList {
			rewrite = afterList
		}
	case heldListed && !removing && len(*remove) > 0:
		*remove = append(*remove, policy.TagRequesters)
	case heldListed && !removing:
		after[policy.TagRequesters] = heldList
	}
	return rewrite, nil
}

// writeGuardRewrite writes a removal retag's second write, the requesters tag guardRetag leaves as
// rewrite, against the tags the store holds once the removal landed (a GCP write is conditioned on
// the tags it read), with the owner and tier the secret holds, which an AWS tag write sends.
func writeGuardRewrite(s *secretSession, slug string, after map[string]string, rewrite string) error {
	held, err := s.backend.currentTags(s.ctx, slug)
	if err != nil {
		return err
	}
	tags := map[string]string{policy.TagRequesters: rewrite}
	for _, key := range []string{policy.TagOwner, policy.TagTier} {
		if value := after[key]; value != "" {
			tags[key] = value
		}
	}
	return s.backend.setTags(s.ctx, slug, held, tags)
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

// printUnknownRetag says what to do after a tag write whose outcome is open (writeUnknown): read
// the tags with `secret show`, under the sign-in the retag used, and retag again where they do not
// hold the change. Running the same retag again is safe either way, as it writes against the tags
// it then reads.
func printUnknownRetag(s *secretSession, stderr io.Writer, form, name string) {
	fmt.Fprintf(stderr, "agent-secrets %s: read the tags as they now stand with `%s`, and run this retag again if they do not hold its change\n",
		form, secretShowCommand(s, name))
}

// rereadIfLanded asks the broker to reread name after a tag write whose tags changed, or may
// have. One that landed with an error (writeLanded, AWS: a race beside another writer's, or a
// read-back that failed) is reread as a landed write is (rereadAfter), which says the write
// stands. One whose answer leaves open whether it landed (writeUnknown) is reread by rereadOpen,
// which prints the broker's answer and claims nothing of the write: it may not have landed, and a
// person told it stands may skip the `secret show` that would find it did not. A write the store
// refused before acting wrote nothing (GCP's errTagsChanged alone, or a definite refusal), and
// needs no reread.
func rereadIfLanded(s *secretSession, stdout, stderr io.Writer, form, name string, err error) {
	switch {
	case writeLanded(err):
		rereadAfter(s, stdout, stderr, form, name, true, nil)
	case writeUnknown(err):
		rereadOpen(s, stdout, stderr, form, name)
	}
}

// reportRemovalFailure prints what a removal retag's removal that failed leaves: the store's
// error, and where the removal landed or may have (another writer changed the tags beside it,
// they could not be read back, or the store's answer was lost), that the requesters rewrite, its
// second write, was not made, since it would send this retag's owner and tier over whatever the
// store now holds. A removal that needs no rewrite and whose outcome is open names the
// `secret show` that reads the tags (printUnknownRetag).
func reportRemovalFailure(s *secretSession, stderr io.Writer, form, name, rewrite string, err error) {
	fmt.Fprintf(stderr, "agent-secrets %s: %v; looked in %s\n", form, s.refusedAs(err), s.storeName())
	switch {
	case rewrite != "" && (writeLanded(err) || writeUnknown(err)):
		outcome := "the removal landed"
		if writeUnknown(err) {
			outcome = "whether the removal landed is unknown"
		}
		fmt.Fprintf(stderr, "agent-secrets %s: %s, so its second write, %s's requesters as %q, was not made; read the tags as they now stand with `%s`, and run this retag again\n",
			form, outcome, name, rewrite, secretShowCommand(s, name))
	case writeUnknown(err):
		printUnknownRetag(s, stderr, form, name)
	}
}

// reportRewriteFailure prints what a requesters rewrite that failed after its removal landed
// leaves, by what the store answered the rewrite.
func reportRewriteFailure(s *secretSession, stderr io.Writer, form, name, rewrite string, held heldTags, err error) {
	show := fmt.Sprintf("looked in %s. Read the tags as they now stand with `%s`", s.storeName(), secretShowCommand(s, name))
	switch {
	case writeLanded(err):
		// AWS: the rewrite landed, and its read-back then found another writer's retag beside
		// it, or could not be read (awsBackend.raceCheck). The requesters tag holds what this
		// retag wrote.
		fmt.Fprintf(stderr, "agent-secrets %s: the removal stands, and %s's requesters were written as %q, but %v; %s\n", form, name, rewrite, s.refusedAs(err), show)
	case writeUnknown(err):
		// The store's answer to the rewrite leaves open whether it landed: the requesters tag
		// holds what this retag wrote or what it held before, and only reading it tells which.
		fmt.Fprintf(stderr, "agent-secrets %s: the removal stands, but whether %s's requesters were written as %q is unknown: %v; %s\n", form, name, rewrite, err, show)
	case errors.Is(err, errTagsChanged):
		// GCP: the store refused the rewrite before acting, because another writer changed the
		// secret after the removal. The requesters tag holds what that writer left, which only
		// reading it tells.
		fmt.Fprintf(stderr, "agent-secrets %s: the removal stands, but another writer changed %s's tags after it, so its requesters were not written as %q: %v; %s\n", form, name, rewrite, s.refusedAs(err), show)
	default:
		// The store refused the rewrite before acting on it, or the read made before it failed
		// and nothing was sent, so what the requesters tag holds now is what it held before this
		// retag: the removal did not touch it. The guard was either leaving it (harmless to
		// leave) or joining it (the secret keeps a project tag with no guard, which a broker from
		// before the project tags would serve), and the two need different words.
		_, guarded := policy.SplitRequestersGuard(held.tags[policy.TagRequesters])
		state := fmt.Sprintf("The requesters tag still holds %s, which only a broker from legion-envoy-v29.23.0 up to the release before the project tags reads, as a refusal", policy.RequestersRollbackGuard)
		if !guarded {
			state = fmt.Sprintf("The requesters tag does not hold %s yet, so a broker from before the project tags would serve this secret by owner and tier alone", policy.RequestersRollbackGuard)
		}
		fmt.Fprintf(stderr, "agent-secrets %s: the removal stands, but writing %s's requesters as %q failed: %v; looked in %s. %s; run this retag again to write it\n", form, name, rewrite, s.refusedAs(err), s.storeName(), state)
	}
}

// reportSetFailure prints what a retag that sets tags leaves when its write failed: the store's
// error; the unchecked-value line where the write landed, since its value then went unchecked as
// surely as after a write with no error; the `secret show` to read where its outcome is open; and
// the shared-secret line for a definite refusal of a shared secret's write. AWS: IAM conditioned
// on the request tags leaves a shared secret's owner and tier to administrators specifically. GCP
// has no per-secret condition on secretmanager.secrets.update, so a deployment instead grants that
// permission at all only to administrators for the secrets it protects this way. A write that
// landed was not refused, whatever its read-back answered, and isAccessDenied does not read a
// write whose outcome is open (tagsUnknown) as one.
func reportSetFailure(s *secretSession, stderr io.Writer, form, name, unchecked, owner string, held heldTags, err error) {
	fmt.Fprintf(stderr, "agent-secrets %s: %v; looked in %s\n", form, s.refusedAs(err), s.storeName())
	if unchecked != "" && writeLanded(err) {
		fmt.Fprintf(stderr, "agent-secrets %s: %s\n", form, unchecked)
	}
	if writeUnknown(err) {
		printUnknownRetag(s, stderr, form, name)
	}
	if s.backend.isAccessDenied(err) && !writeLanded(err) && (held.tags[policy.TagOwner] == policy.OwnerShared || owner == policy.OwnerShared) {
		fmt.Fprintf(stderr, "agent-secrets %s: %s\n", form, sharedRetagRefused)
	}
}

// rereadRetag is rereadAfter for a retag, which also exits 1 when the secret does not hold type,
// the one the retag left it (after's), as rereadValue does for a value write: the store's own type
// tag is read back whatever the broker answers (reread). Where a session's reread was answered in
// full, the broker must serve the secret as that type too: a reread can read the secret's metadata
// stale (Secret Manager's is eventually consistent for up to hours), and a secret the broker still
// serves as stored releases its stored value raw, so a key stored in it then reaches every grant
// of it. Only an exit 0 with `broker: serving … (type …)`, not an exit 0 after the line saying the
// broker's view was not checked, says the broker reads the new tags.
func rereadRetag(s *secretSession, stdout, stderr io.Writer, form, name, secretType string) int {
	r, ok := reread(s, stdout, stderr, form, name, secretType, true, nil)
	if !ok {
		return 1
	}
	if r.Told && r.Type != secretType {
		fmt.Fprintf(stderr, "agent-secrets %s: the retag in %s stands, but %s serves %s as %s, not %s: it still sees the old tags. Run this retag again to ask it to reread them, and store no value in %s until a retag's `broker: serving %s` line says it serves %s as %s\n", form, s.storeName(), s.src.brokerAt(), name, servedTypeName(s, r.Type), typeName(secretType), name, name, name, typeName(secretType))
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

// servedTypeName is t, the type the broker's reread answer says it serves a secret as, as a line
// names it: "a stored secret" for none (typeName), and otherwise the broker's text as its address
// lets its answer print (Settings.words).
func servedTypeName(s *secretSession, t string) string {
	if t == "" {
		return typeName(t)
	}
	return s.settings.words(t)
}
