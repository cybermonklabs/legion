package record

import (
	"errors"
	"fmt"
	"strings"
)

// GroupPrefix opens an approver, an owner tag or a requesters entry that names a Google group:
// group:<address>, where the address is the group's lowercase email. A record whose approver is a
// group is decided by any member of that group, by the members the broker's group reader last
// read (Groups).
const GroupPrefix = "group:"

// GroupOf answers the bare address of a group approver, group:<address>, and false for any other
// approver: a person, AnyoneApprover, or the prefix with no address after it.
func GroupOf(approver string) (address string, ok bool) {
	address, ok = strings.CutPrefix(approver, GroupPrefix)
	if !ok || address == "" {
		return "", false
	}
	return address, true
}

// Groups answers who is a member of a Google group, as the broker's group reader last read it
// (groups.Reader). It takes a group's bare address, never the group: form. A nil Groups admits
// nobody to a group approver.
type Groups interface {
	// Membership answers, from one look at group, whether its last good read listed login,
	// whatever that read's age, and whether that read is fresh: two hours old or newer. A group
	// the reader never read, or no longer reads, answers false and false.
	Membership(group, login string) (listed, fresh bool)
}

// Directory is Groups with what the policy and the request machine ask of the reader beside it:
// GroupsOf, every fresh group that lists a login, sorted, which the pending and grants lists of a
// member match on; and Want, the groups a full policy load names, which the reader reads.
type Directory interface {
	Groups
	GroupsOf(login string) []string
	Want(groups []string)
}

// GroupAdmits reports whether groups admits login to the group approver names (group:<address>)
// now: the group's last good read lists login and is fresh. An approver that is no group, an empty
// login and a nil groups admit nobody. It is the one rule every decision site holds a group to:
// MayDecide's group branch, and a requesters entry naming a group (policy).
func GroupAdmits(groups Groups, approver, login string) bool {
	group, ok := GroupOf(CanonicalLogin(approver))
	login = CanonicalLogin(login)
	if !ok || login == "" || groups == nil {
		return false
	}
	listed, fresh := groups.Membership(group, login)
	return listed && fresh
}

// CheckDecider answers why login may not decide a record of kind whose approver is approver and
// whose signed request names service, and nil when it may (MayDecide's rule). For an agent_secret
// record whose approver is a group the group does not admit login to now, it is an
// ApproverNotAdmittedError whose Fresh is read in the same look as the membership, so a read
// landing between two looks cannot turn a lapse into a broken chain; a nil groups is a group never
// read. Every other refusal is ErrNotApprover.
func CheckDecider(kind, approver, service, login string, groups Groups) error {
	login, approver = CanonicalLogin(login), CanonicalLogin(approver)
	_, judge := JudgeLogin(login)
	switch {
	case login == "" || login == AnyoneApprover || judge:
		return ErrNotApprover
	case kind == KindLauncherCredential && service != "":
		return nil
	case kind == KindAgentSecret && approver == AnyoneApprover:
		return nil
	}
	if group, ok := GroupOf(approver); ok {
		if kind != KindAgentSecret {
			return ErrNotApprover
		}
		var listed, fresh bool
		if groups != nil {
			listed, fresh = groups.Membership(group, login)
		}
		if listed && fresh {
			return nil
		}
		return ApproverNotAdmittedError{Group: group, Login: login, Fresh: fresh}
	}
	if login == approver {
		return nil
	}
	return ErrNotApprover
}

// ErrApproverNotAdmittedNow is a grant approved by a member of a group approver whose group cannot
// vouch for that member now: the group's last good read is more than two hours old, or it was
// never read. The grant has lapsed: it is refused as a grant no longer live, and no broken chain
// is written for it, since nobody can tell whether the decider is still a member.
var ErrApproverNotAdmittedNow = errors.New("the group that approved this grant cannot say its decider is a member now")

// ApproverNotAdmittedError is a group-approved record whose decider the record's group does not
// admit now: Group is the group's bare address, Login the decider's login, and Fresh whether the
// group's last good read is two hours old or newer. A stale or never-read group is a lapsed grant,
// which errors.Is matches as ErrApproverNotAdmittedNow; a fresh group that does not list the
// decider is a decision nobody in the group made now, which ChainVerifier.Verify wraps in
// ErrChainBroken and which ErrApproverNotAdmittedNow does not match.
type ApproverNotAdmittedError struct {
	Group, Login string
	Fresh        bool
}

func (e ApproverNotAdmittedError) Error() string {
	if e.Fresh {
		return fmt.Sprintf("decided by %q, who is not a member of %s now", e.Login, e.Group)
	}
	return fmt.Sprintf("decided by %q as a member of %s, whose members were not read in the last two hours", e.Login, e.Group)
}

// Is matches ErrApproverNotAdmittedNow for a group that is not fresh.
func (e ApproverNotAdmittedError) Is(target error) bool {
	return target == ErrApproverNotAdmittedNow && !e.Fresh
}
