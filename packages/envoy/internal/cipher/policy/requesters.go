package policy

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/sjawhar/envoy/internal/cipher/record"
)

// ReasonRequesterNotListed is the Reason of a Deny the requesters tag decided: neither the
// person the asking session is signed in as nor the service the asking pod proves is on the
// secret's list, so it is refused whatever its owner and tier would have given them.
const ReasonRequesterNotListed = "requester-not-listed"

// ReasonRequesterGroupUnread is the Reason of a Deny the requesters tag decided while it cannot
// say whether the requester is on it: no entry names the session's person or the pod's service,
// and a group entry names a group whose members the broker has not read in the last two hours, so
// the person may be a member. A new request is refused for it until the group is read; a live
// grant is paused, not ended (requests' stillAllowed), and releases again once a read lists the
// person; and no approval of a pending request is recorded while it stands.
const ReasonRequesterGroupUnread = "requester-group-unread"

// requesterNames is Requesters as a list of the emails, groups and service names it holds, nil
// when it is "". It is unexported because lists, just below, is its one caller: nothing outside
// this package reads a secret's requesters as a list.
func (secret Secret) requesterNames() []string {
	if secret.Requesters == "" {
		return nil
	}
	return strings.Fields(secret.Requesters)
}

// lists reports whether secret's requesters name r: the person whose session it is, a group
// groups admits that person to (record.GroupAdmits), or the service of the pod it is. Each sort is
// matched only by an entry of its own sort: a group entry only by a member of that group, and of
// the rest the @ tells a person from a service (ParseRequesters makes every person entry hold one
// and every service entry hold none): a login record.CanonicalLogin answers may be a GitHub login
// rather than an email, for a record decided before people were named by email, and such a login
// has a service name's own alphabet, so an operator named "legion-daemon" would otherwise be
// served every secret that lists the legion-daemon service. An empty Operator and an empty Service
// (a session has no service) match nothing, never an entry. A pod's Operator is empty unless
// BROKER_POD_OPERATORS maps the pod's account to the person its login carried
// (requests.Machine.requester).
//
// unread reports, for a requester the list does not name, whether it cannot say so: the requester
// has a person, and a group entry names a group whose last good read is not fresh (or a set with
// no group reader), so the person may be on it. A grant is paused for that and not ended
// (ReasonRequesterGroupUnread).
func (secret Secret) lists(r Requester, groups record.Groups) (listed, unread bool) {
	operator := record.CanonicalLogin(r.Operator)
	for _, entry := range secret.requesterNames() {
		if group, ok := record.GroupOf(entry); ok {
			if record.GroupAdmits(groups, entry, operator) {
				return true, false
			}
			if operator != "" && (groups == nil || !groupFresh(groups, group, operator)) {
				unread = true
			}
			continue
		}
		person := strings.Contains(entry, "@")
		if person && operator != "" && entry == operator {
			return true, false
		}
		if !person && r.Service != "" && entry == r.Service {
			return true, false
		}
	}
	return false, unread
}

// groupFresh reports whether group's last good read is fresh, as groups answers it for login.
func groupFresh(groups record.Groups, group, login string) bool {
	_, fresh := groups.Membership(group, login)
	return fresh
}

// RequesterFault is which condition of the requesters rules refuses a secret's requesters tag,
// as RequesterRule answers it. TagRules turns every fault but RequesterFaultNone into the one
// load reason ReasonRequestersNotOwner, so the fault is what tells those conditions apart for a
// caller that must say which flag fixes a write.
type RequesterFault int

const (
	// RequesterFaultNone is a tag set no requesters condition refuses.
	RequesterFaultNone RequesterFault = iota
	// RequesterFaultOwnerNotPerson is a google-service-account secret whose owner is neither a
	// person (ValidPersonOwner) nor a group (ValidGroupOwner). The token it mints acts as a
	// service account, which is given to the one person who owns the secret, or to the members of
	// the group that does, so a secret owned by shared or by a service can carry no such type and
	// no requesters list could make it servable.
	RequesterFaultOwnerNotPerson
	// RequesterFaultServiceBelowHumanTier is a google-service-account secret whose requesters
	// name a service at a tier that is not human. A service's pod may ask for such a token only
	// where a person approves each release of it, which human tier is.
	RequesterFaultServiceBelowHumanTier
	// RequesterFaultNotTheOwnerOnce is a google-service-account secret owned by a person whose
	// requesters do not name that person exactly once: no list at all, a list naming no person,
	// one naming somebody else, one naming more than one person, or one naming a group. The
	// secret is served to that one person, who is therefore on its list, beside the registered
	// services a human-tier secret may name as well.
	RequesterFaultNotTheOwnerOnce
	// RequesterFaultServiceOnDevAccount is a secret the deployment binds to a dev-<name> account
	// (TagSet.BoundAccount, devServiceAccount) whose requesters name a service, at either tier.
	// Such an account is one person's own, so no service may ask for its token, whatever type
	// the secret is.
	RequesterFaultServiceOnDevAccount
	// RequesterFaultNotTheOwnerGroupOnce is a google-service-account secret owned by a group whose
	// requesters do not name that group exactly once and no person: no list, another group, a
	// second group, or a person. The secret is served to the group's members, so the list names
	// the group, beside the registered services a human-tier secret may name as well.
	RequesterFaultNotTheOwnerGroupOnce
)

// RequesterRule answers which condition refuses t's requesters, and RequesterFaultNone for a set
// no such condition refuses. It holds the conditions most fundamental first: the owner is neither
// a person nor a group, so no list could make the secret servable; the list names a service below
// human tier; the list does not name that owner exactly once (a group owner: that group, once,
// and no person; a person owner: that person, once, and no group); the secret is bound to a
// dev-<name> account and the list names a service, which holds for every type.
//
// The loader reads none of these apart: it reads TagRules' reason, and every fault here is the
// one reason ReasonRequestersNotOwner. The fault exists for `agent-secrets`' tagRefusal, which
// says which flag fixes the write and so must name the condition that refused it rather than
// another that happens to be true beside it — a secret owned by shared is answered for its owner
// however its list and tier read, and a list naming a service at agent tier is answered for the
// tier and not for the person it leaves out.
func RequesterRule(t TagSet) RequesterFault {
	// Each entry's own shape is ParseRequesters' and ParseRequesterShapes' business, and they
	// answer the entries sorted and separated by single spaces, so the conditions below read
	// only what tells a person, a service and a group apart (SplitRequesters), and read it once
	// for all of them.
	people, services, groups := SplitRequesters(t.Requesters)
	if t.Type == TypeGoogleServiceAccount || t.Type == TypeAnthropicFederation {
		switch {
		case !ValidPersonOwner(t.Owner) && !ValidGroupOwner(t.Owner):
			return RequesterFaultOwnerNotPerson
		case len(services) > 0 && t.Tier != TierHuman:
			return RequesterFaultServiceBelowHumanTier
		case ValidGroupOwner(t.Owner) && (len(people) > 0 || len(groups) != 1 || groups[0] != t.Owner):
			return RequesterFaultNotTheOwnerGroupOnce
		case ValidPersonOwner(t.Owner) && (len(people) != 1 || people[0] != t.Owner || len(groups) > 0):
			return RequesterFaultNotTheOwnerOnce
		}
	}
	if id, _, bound := strings.Cut(t.BoundAccount, "@"); bound && devServiceAccount.MatchString(id) && len(services) > 0 {
		return RequesterFaultServiceOnDevAccount
	}
	return RequesterFaultNone
}

// ErrRequestersMissing is a requesters tag that names nobody: a tag that let no one ask would
// serve the secret to no session at all, which is a mistake rather than a policy.
var ErrRequestersMissing = errors.New("requesters names at least one person or service")

// ErrRequestersComma is a requesters tag that holds a comma: its entries are separated by spaces,
// as github-owners' and BROKER_SERVICES' are, since an AWS tag value takes a space and no comma.
var ErrRequestersComma = errors.New(`requesters separates its entries with spaces, not commas ("<email> <service>")`)

// ErrRequestersEntry is a requesters entry that names neither a person, a group nor a service:
// one holding an @ that is not a lowercase email of the form a person signs in under (an
// uppercase one is refused rather than lowercased, since the tag is compared against the sign-in
// as it is written), one starting group: whose address is no lowercase email, or one naming a
// service the deployment does not register.
var ErrRequestersEntry = errors.New("requesters names people by their lowercase sign-in email, groups as group:<lowercase email>, and services by their registered name")

// ParseRequesters reads a requesters tag for a caller that holds the deployment's registered
// service list — the loader (Loader.Services), the one reader whose answer decides whether the
// secret is served. People are named by lowercase email, groups as group:<lowercase email>, and
// services by name, separated by white space. It refuses a comma anywhere (ErrRequestersComma), a
// list that names nobody (ErrRequestersMissing), and an entry that is none of them
// (ErrRequestersEntry). An entry starting group: is a group, read before the @ it holds, and its
// address must be a lowercase email (ValidGroupOwner); any other entry holding an @ is a person,
// and must be the lowercase email their sign-in names them by (emailPattern), since Evaluate
// compares it against record.CanonicalLogin of the session's operator; any other entry is a
// service, and must be one of services, ALWAYS, so a nil or empty list refuses every service
// entry, exactly as the owner tag already refuses a name the deployment does not register
// (Loader.secret). ParseRequesterShapes checks a service entry's shape alone, for the one caller
// that holds no registered list. It answers the entries sorted, each once and joined by single
// spaces, so a Secret holding them stays comparable, two spellings of one list are one policy,
// and the value is one an AWS tag and a Secret Manager annotation both hold.
func ParseRequesters(raw string, services []string) (string, error) {
	return parseRequesters(raw, services, true)
}

// ParseRequesterShapes reads a requesters tag for a caller that holds no registered service list:
// `agent-secrets`, which never sees BROKER_SERVICES. It is ParseRequesters in every respect but
// one — an entry naming a service is checked for a service name's shape alone
// (record.ValidService) rather than against the registered list — so the CLI refuses a malformed
// tag before it writes, and the broker's answer to the write's reread (ReasonRequestersMalformed)
// is what catches a name no deployment registers. ParseRequesters exists for the loader, which
// holds that list and must apply it.
func ParseRequesterShapes(raw string) (string, error) {
	return parseRequesters(raw, nil, false)
}

// parseRequesters is the one reading behind both exported functions. registered says which check
// a service entry gets: membership of services, or a service name's shape alone.
func parseRequesters(raw string, services []string, registered bool) (string, error) {
	if strings.Contains(raw, ",") {
		return "", ErrRequestersComma
	}
	var requesters []string
	for _, entry := range strings.Fields(raw) {
		switch {
		case groupEntry(entry):
			if !ValidGroupOwner(entry) {
				return "", fmt.Errorf("%w, not %q", ErrRequestersEntry, entry)
			}
		case strings.Contains(entry, "@"):
			if !emailPattern.MatchString(entry) {
				return "", fmt.Errorf("%w, not %q", ErrRequestersEntry, entry)
			}
		case registered:
			if !slices.Contains(services, entry) {
				return "", fmt.Errorf("%w, not %q: no registered service has that name", ErrRequestersEntry, entry)
			}
		default:
			if !record.ValidService(entry) {
				return "", fmt.Errorf("%w, not %q", ErrRequestersEntry, entry)
			}
		}
		requesters = append(requesters, entry)
	}
	if len(requesters) == 0 {
		return "", ErrRequestersMissing
	}
	slices.Sort(requesters)
	return strings.Join(slices.Compact(requesters), " "), nil
}

// SplitRequesters splits a requesters tag into the people, the services and the groups it names,
// each list in the order the tag holds them and all nil for a tag that names nobody. An entry
// starting group: is a group, read first, since its address holds an @; of the rest the @ is the
// whole of the test: both parsers behind the tag make every person entry a lowercase email, which
// holds one, and every service entry a service name, which holds none (ParseRequesters against
// the deployment's registered list, ParseRequesterShapes against a service name's shape alone),
// so a reader of the split needs neither alphabet and no entry can be two of them. An entry is
// answered as it was written, a group's with its group: prefix, since a tag the loader read is
// already sorted, compacted and single-spaced and a caller comparing an entry against an owner
// tag compares it as the tag writes it.
func SplitRequesters(requesters string) (people, services, groups []string) {
	for _, entry := range strings.Fields(requesters) {
		switch {
		case groupEntry(entry):
			groups = append(groups, entry)
		case strings.Contains(entry, "@"):
			people = append(people, entry)
		default:
			services = append(services, entry)
		}
	}
	return people, services, groups
}

// groupEntry reports whether entry names a group (record.GroupOf): an entry that opens with the
// group form and names no address after it is no group's, and the service branch refuses it as no
// registered service and no service name's shape.
func groupEntry(entry string) bool {
	_, ok := record.GroupOf(entry)
	return ok
}

// devServiceAccount is the id, the part before the @, of a service account the deployment binds
// to one person: it binds each person's DEV_<NAME> secret to their own dev-<name> account, whose
// token therefore goes to that person and to nobody else. RequesterRule reads it to refuse a
// service entry on such a secret's requesters list.
var devServiceAccount = regexp.MustCompile(`^dev-[a-z0-9-]+$`)
