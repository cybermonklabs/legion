// restricted_approver.go is the rule that a person Dispatch calls a restricted member
// (projectaccess.MemberRestricted) is no approver of a secret. Such a person was admitted by a
// project's access list from outside the deployment's sign-in domain and sees only the projects
// that name them. They decide no secret request on the broker's approval page, which signs in only
// its own domain and asks the deciding person's member kind too (ApplyDecision); and the routes
// Dispatch relays for its signed-in people show them no secret request or grant as its approver,
// and let them end or dismiss none, whichever arm would make them its approver: the record's or the
// grant's own login (a secret they own, or a grant the model judge approved in their stead),
// record.AnyoneApprover (a shared secret), or a Google group the group reader lists them in. Those
// routes are the pending list (PendingForApprover), the grant list (GrantsForApprover), the revoke
// (RevokeByApprover) and the dismissal (Dismiss). Nor are they an approver of a service's machine
// login, which waits on record.AnyoneApprover: a legacy one is on no list of theirs and theirs to
// dismiss nowhere, and the machine package holds its deny to the same rule
// (machine.Service.ApplyDecision). What is theirs as a person stays theirs, and asks Dispatch
// nothing here: their own machine logins, which a person decides alone, and the grants of the
// sessions that act for them now, which they end as the session's operator. Nothing else a list
// holds is theirs: not a service pod's grant, and not a grant of a pod BROKER_POD_OPERATORS no
// longer maps to them.
//
// The member kind is asked through m.Projects (projectaccess.Restricted) on no transaction, and
// only where an approver arm would admit the person, so a route that admits or refuses them on
// other grounds asks nothing. A route Dispatch relays bounds that read below Dispatch's own wait
// (projectaccess.RelayedReadBound, approvesSecrets), so a Dispatch that does not answer leaves the
// route its answer. A failure to ask fails closed, as every other read of a person's member kind
// does, everywhere but the revoke: the dismissal and the decision return the
// *projectaccess.MemberKindUncheckable, which the API answers 503 MEMBER_KIND_UNCHECKABLE, and the
// two lists leave the approver rows of that read off and log it (approverKindUnreadMessage), as
// they leave off a row whose project Dispatch could not be asked about. The revoke goes ahead while
// Dispatch does not answer, and logs it (revokeApproves): a revoke only takes access away; a
// failure that lasts until somebody fixes it refuses it as it refuses the rest. A machine with no
// client of Dispatch asks nothing, and every approver stands as before.
package requests

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/sjawhar/envoy/internal/cipher/projectaccess"
	"github.com/sjawhar/envoy/internal/cipher/record"
)

// restrictedPerson reports whether Dispatch calls person a restricted member
// (projectaccess.Restricted), asked through m.Projects on no transaction: false, asking nothing,
// for a machine with no client of Dispatch and for no person. A failure to ask is the
// *projectaccess.MemberKindUncheckable projectaccess.Restricted returns.
func (m *Machine) restrictedPerson(ctx context.Context, person string) (bool, error) {
	if m.Projects == nil || person == "" {
		return false, nil
	}
	return projectaccess.Restricted(ctx, m.Projects, person)
}

// approvesSecrets reports whether login, the person a route Dispatch relays acts for, may stand as
// a secret's approver: false for a person Dispatch calls a restricted member, and the
// *projectaccess.MemberKindUncheckable restrictedPerson returns when that cannot be asked. The read
// runs within projectaccess.RelayedReadBound, and one it cuts short is one Dispatch did not answer.
func (m *Machine) approvesSecrets(ctx context.Context, login string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, projectaccess.RelayedReadBound)
	defer cancel()
	restricted, err := m.restrictedPerson(ctx, record.CanonicalLogin(login))
	if err != nil {
		return false, err
	}
	return !restricted, nil
}

// approverStanding is what a route that may act as an approver read of its person's member kind
// before its transaction (revokeApproves, dismissApproves): whether they may stand as an approver
// of what the row is, and whether that is false because Dispatch calls them a restricted member,
// whose refusal says so in its own words.
type approverStanding struct {
	approves, restricted bool
}

// approvesAsReadBy is the approverStanding of approvesSecrets' answer.
func approvesAsReadBy(approves bool) approverStanding {
	return approverStanding{approves: approves, restricted: !approves}
}

// approverRow reports whether a record of kind, waiting on approver, waits on login, canonical,
// only as one of its approvers, which no restricted member is: every secret request, whichever arm
// names login, and a machine login that waits on someone other than login, a service's legacy one
// waiting on record.AnyoneApprover. A person's own machine login waits on that person, and is theirs
// to decide as a person.
func approverRow(kind, approver, login string) bool {
	return kind == record.KindAgentSecret || record.CanonicalLogin(approver) != login
}

// ErrRestrictedDismissal is Dismiss's refusal of a person Dispatch calls a restricted member, of a
// record whose list holds them only as one of its approvers (approverRow): record.ErrNotApprover,
// which the API answers 403 NOT_APPROVER with these words.
var ErrRestrictedDismissal = fmt.Errorf("%w: a person Dispatch calls a restricted member is no approver of a secret or of a service's machine login, and dismisses only their own machine logins", record.ErrNotApprover)

// ErrRestrictedDecider is ApplyDecision's refusal of a person Dispatch calls a restricted member,
// who decides no secret request, approving or denying, even one for a secret they own:
// record.ErrNotApprover, in these words.
var ErrRestrictedDecider = fmt.Errorf("%w: a person Dispatch calls a restricted member is no approver of a secret, and decides no secret request", record.ErrNotApprover)

// errRestrictedRevoker is RevokeByApprover's refusal of a person Dispatch calls a restricted member,
// of a grant that admits them only as an approver: ErrNotApprover, which the API answers 403
// NOT_APPROVER with these words.
var errRestrictedRevoker = fmt.Errorf("%w: a person Dispatch calls a restricted member is no approver of a secret, and ends a grant only as the person its session acts for", ErrNotApprover)

// approverKindUnreadMessage is the log line for an approver's list that leaves out every row it
// would hold the person as an approver of, because the broker could not ask Dispatch whether that
// person is a restricted member: the list fails closed for those rows of that read alone, and
// lists the rest.
const approverKindUnreadMessage = "an approver's list leaves out every row it holds the person as an approver of, a secret request, a grant or a service's machine login, since Dispatch could not say whether the person is a restricted member; they are listed again once Dispatch answers"

// listApprovesSecrets is approvesSecrets for one read of login's list, which list names
// ("pending" or "grants"): false for a restricted member, and false too, failing closed for that
// read alone, when Dispatch could not be asked, which it logs once per person per
// projectaccess.CacheTTL however often the list is read (approverKindUnreadMessage). A read whose
// caller went away is read by nobody, so it logs nothing.
func (m *Machine) listApprovesSecrets(ctx context.Context, login, list string) bool {
	approves, err := m.approvesSecrets(ctx, login)
	if err == nil {
		return approves
	}
	if ctx.Err() == nil && m.rowsUnansweredLogged.due("member kind "+login) {
		slog.Warn(approverKindUnreadMessage, "list", list, "person", login, "error", err)
	}
	return false
}
