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
// (groups.Reader). Each method takes a group's bare address, never the group: form. A group the
// reader has not read within two hours admits nobody and is not fresh. A nil Groups admits nobody
// to a group approver.
type Groups interface {
	// HasMember answers whether login is a member of group by its last good read, when that read
	// is fresh.
	HasMember(group, login string) bool
	// Fresh answers whether group's last good read is two hours old or newer: false for a group
	// the reader never read, or no longer reads.
	Fresh(group string) bool
}

// Directory is Groups with what the policy and the request machine ask of the reader beside it:
// GroupsOf, every fresh group that lists a login, sorted, which the pending and grants lists of a
// member match on; and Want, the groups a full policy load names, which the reader reads.
type Directory interface {
	Groups
	GroupsOf(login string) []string
	Want(groups []string)
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
