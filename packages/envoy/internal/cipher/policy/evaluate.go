package policy

import (
	"errors"
	"fmt"
	"slices"

	"github.com/sjawhar/envoy/internal/cipher/record"
)

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
	// Restricted is true when Dispatch calls the person the session was made for a restricted
	// member: a person an access list admitted from outside the deployment's sign-in domain, who
	// sees only the projects that name them (projectaccess.MemberRestricted). That person is the
	// session's enrollment's operator, which is not Operator for a person's pod that
	// BROKER_POD_OPERATORS no longer maps (requests.Machine.restrictedOperator). The request
	// machine asks Dispatch before any transaction opens (requests.Machine.Projects), so the
	// policy reads it as plain data. Evaluate denies such a requester every secret,
	// ReasonRestrictedMember.
	Restricted bool
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

// ReasonRestrictedMember is the Reason of a Deny for a requester whose person Dispatch calls a
// restricted member (Requester.Restricted): such a person's sessions are released no secret,
// whatever the secret's owner, tier, requesters list and project would have given them, because
// the broker's secrets are the deployment's own people's and a restricted member is a guest of the
// projects that name them.
const ReasonRestrictedMember = "restricted-member"

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

// Evaluate answers r's ask for name. A name the set does not serve is ErrUnknownSecret. A
// requester whose person Dispatch calls a restricted member (Requester.Restricted) is denied
// every name the set serves, ReasonRestrictedMember, before anything else is read. A secret
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
	if r.Restricted {
		return Decision{Outcome: Deny, Reason: ReasonRestrictedMember, Source: secret.Source}, nil
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
