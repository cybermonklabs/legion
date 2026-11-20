package policy

import (
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
	// Restricted is true when Dispatch calls Operator a restricted member: a person an access
	// list admitted from outside the deployment's sign-in domain, who sees only the projects that
	// name them (projectaccess.MemberRestricted). The request machine asks Dispatch before any
	// transaction opens (requests.Machine.Projects), so the policy reads it as plain data.
	// Evaluate denies such a requester every secret, ReasonRestrictedMember.
	Restricted bool
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

// ReasonRequesterNotListed is the Reason of a Deny the requesters tag decided: neither the
// person the asking session is signed in as nor the service the asking pod proves is on the
// secret's list, so it is refused whatever its owner and tier would have given them.
const ReasonRequesterNotListed = "requester-not-listed"

// ReasonRestrictedMember is the Reason of a Deny for a requester whose person Dispatch calls a
// restricted member (Requester.Restricted): such a person's sessions are released no secret,
// whatever the secret's owner, tier and requesters list would have given them, because the
// broker's secrets are the deployment's own people's and a restricted member is a guest of the
// projects that name them.
const ReasonRestrictedMember = "restricted-member"

// ReasonNoReason is the Reason of a Deny the request machine decided: the request named a
// credential that would wait for a person's approval and carried no reason, so it is refused
// rather than put to anyone. Set.Evaluate never answers it, since the reason is not the
// policy's to see.
const ReasonNoReason = "a reason is necessary; ask again with --reason"

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
// requester the list does not name, before owner and tier are read at all. A name withheld from r
// (Requester.Withheld) is decided as a human-tier secret: what r would have got at once is an
// approval request to the owner, or to anyone for a shared secret, and a service's own secret,
// which no person approves, is denied.
func (s *Set) Evaluate(name string, r Requester) (Decision, error) {
	secret, ok := s.Secrets[name]
	if !ok {
		return Decision{}, ErrUnknownSecret
	}
	if r.Restricted {
		return Decision{Outcome: Deny, Reason: ReasonRestrictedMember, Source: secret.Source}, nil
	}
	if secret.Requesters != "" && !secret.lists(r) {
		return Decision{Outcome: Deny, Reason: ReasonRequesterNotListed, Source: secret.Source}, nil
	}
	withheld := slices.Contains(r.Withheld, name)
	d := Decision{Source: secret.Source}
	switch {
	case secret.ownership == ownerService:
		d.Outcome = Deny
		if r.Service == secret.Owner && !withheld {
			d.Outcome = Automatic
		}
	case secret.Tier == TierAgent && !withheld && (secret.ownership == ownerShared || record.CanonicalLogin(r.Operator) == secret.Owner):
		d.Outcome = Automatic
	default:
		d.Outcome, d.Approver = Approval, secret.approver()
	}
	return d, nil
}
