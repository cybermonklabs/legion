// restricted_approver.go is the rule that a person Dispatch calls a restricted member
// (projectaccess.MemberRestricted) is no approver of a secret. Such a person was admitted by a
// project's access list from outside the deployment's sign-in domain and sees only the projects
// that name them. The broker's approval page signs in only its own domain, so they decide no
// secret request; and the routes Dispatch relays for its signed-in people show them no secret
// request or grant as its approver, and let them end or dismiss none, whichever arm would make
// them its approver: the record's or the grant's own login (a secret they own, or a grant the
// model judge approved in their stead), record.AnyoneApprover (a shared secret), or a Google group
// the group reader lists them in. Those routes are the pending list (PendingForApprover), the
// grant list (GrantsForApprover), the revoke (RevokeByApprover) and the dismissal (Dismiss). What
// is theirs as a person stays theirs, and asks Dispatch nothing here: their own machine logins,
// which a person decides alone, and the grants of their own sessions, which they end as the
// session's operator.
//
// The member kind is asked through m.Projects (projectaccess.Restricted) on no transaction, and
// only where an approver arm would admit the person, so a route that admits or refuses them on
// other grounds asks nothing. A failure to ask fails closed, as every other read of a person's
// member kind does: the revoke and the dismissal return the *projectaccess.MemberKindUncheckable,
// which the API answers 503 MEMBER_KIND_UNCHECKABLE, and the two lists leave the approver rows of
// that read off and log it (approverKindUnreadMessage), as they leave off a row whose project
// Dispatch could not be asked about. A machine with no client of Dispatch asks nothing, and every
// approver stands as before.
package requests

import (
	"context"
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

// approvesSecrets reports whether login, the person a route acts for, may stand as a secret's
// approver: false for a person Dispatch calls a restricted member, and the
// *projectaccess.MemberKindUncheckable restrictedPerson returns when that cannot be asked.
func (m *Machine) approvesSecrets(ctx context.Context, login string) (bool, error) {
	restricted, err := m.restrictedPerson(ctx, record.CanonicalLogin(login))
	if err != nil {
		return false, err
	}
	return !restricted, nil
}

// approverKindUnreadMessage is the log line for an approver's list that leaves out every secret
// request and grant it would hold the person as an approver of, because the broker could not ask
// Dispatch whether that person is a restricted member: the list fails closed for those rows of
// that read alone, and lists the rest.
const approverKindUnreadMessage = "an approver's list leaves out every secret request and grant it holds the person as an approver of, since Dispatch could not say whether the person is a restricted member; they are listed again once Dispatch answers"

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
