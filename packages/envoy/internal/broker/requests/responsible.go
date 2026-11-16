// responsible.go finds the person a request for a shared human-tier secret is listed for: the
// person responsible for the agent that made it. Anyone signed in still decides such a request
// (record.MayDecide); this only chooses whose Dispatch Inbox lists it (PendingForApprover), so a
// person who has nothing to do with the agent is not asked.
package requests

import (
	"context"
	"regexp"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/broker/exchange"
	"github.com/sjawhar/envoy/internal/broker/policy"
	"github.com/sjawhar/envoy/internal/broker/record"
)

// Assignees answers the person a Dispatch issue is assigned to, "" for an issue nobody is. The
// broker's model judge's read of Dispatch is the one in production (judge.Dispatch.Assignee),
// which cmd/broker wires where BROKER_JUDGE_DISPATCH_URL is set; it is declared here, where it is
// used, since the judge imports this package.
type Assignees interface {
	Assignee(ctx context.Context, issueKey string) (string, error)
}

// assigneeBound is how long Create waits for Dispatch to name a Legion pod's issue's assignee. A
// read that has not answered by then names nobody, and the request is listed for everyone, so a
// slow or absent Dispatch delays a request by this much at most and hides it from no one.
const assigneeBound = 3 * time.Second

// claimToken is a Legion role claim token, legion-<project>-<issue project, lowercased>-<issue
// number>-<role> (packages/daemon/internal/claim.NewToken). A project token holds no "-", so the
// issue's project and number are the two groups. The controller's token, legion-<project>-
// controller, names no issue and does not match.
var claimToken = regexp.MustCompile(`^legion-[a-z0-9]+-([a-z][a-z0-9]*)-([0-9]+)-(architect|planner|implementer|tester|reviewer|merger)$`)

// issueKeyOfClaim is the Dispatch issue key a pod's claim (enrollments.claim) names, such as
// ACME-7 for legion-acme-acme-7-planner, and "" for a claim that is no Legion role
// claim token.
func issueKeyOfClaim(claim string) string {
	match := claimToken.FindStringSubmatch(claim)
	if match == nil {
		return ""
	}
	return strings.ToUpper(match[1]) + "-" + match[2]
}

// responsiblePerson is whose Inbox lists a request req's enrollment e made for a shared
// human-tier secret, as a canonical login, in this order: the person the session acts for (a
// person's sign-in, or the person who approved its machine login); for a fleet machine's own
// session, the person its account's BROKER_VM_EXCHANGE_ACCOUNTS entry names; for a pod, assignee,
// the person its claim's issue is assigned to. "" when none names a person, and then the request
// is listed for everyone signed in, as it was before, so no request waits where nobody sees it.
func (m *Machine) responsiblePerson(e enrollmentRow, req policy.Requester, assignee string) string {
	person := req.Operator
	if person == "" {
		switch e.Kind {
		case "session":
			if e.Subject != nil {
				if read := exchange.ReadSubject(*e.Subject); read.Kind == exchange.SubjectMachine {
					person = m.VMPersonsByAccount[read.Account]
				}
			}
		case "pod":
			person = assignee
		}
	}
	login := record.CanonicalLogin(person)
	if login == record.AnyoneApprover {
		return ""
	}
	return login
}

// claimAssignee is the assignee of the issue a pod's claim names, read before Create's
// transaction so no network call holds a row lock. It reads Dispatch only for a pod whose claim
// names an issue and whose request the policy, read now, would leave waiting on
// record.AnyoneApprover. Any failure, a Dispatch that answers nothing within assigneeBound, and an
// issue nobody is assigned to give "". A decision inside the transaction that differs from this
// read can only leave the request listed for everyone, never for nobody.
func (m *Machine) claimAssignee(ctx context.Context, e enrollmentRow, set *policy.Set, names []string, reason string) string {
	if e.Kind != "pod" || m.Assignees == nil {
		return ""
	}
	key := issueKeyOfClaim(e.Claim)
	if key == "" {
		return ""
	}
	requester, err := m.requester(ctx, m.Store.Pool, e)
	if err != nil {
		return ""
	}
	if decided, err := evaluate(set, names, requester, reason); err != nil || decided.state != "pending" || decided.approver != record.AnyoneApprover {
		return ""
	}
	bounded, cancel := context.WithTimeout(ctx, assigneeBound)
	defer cancel()
	// The read runs on its own goroutine so the bound holds even for a reader that does not
	// watch its context; its answer, if it comes later, goes into the buffered channel and is
	// dropped.
	answered := make(chan string, 1)
	go func() {
		assignee, err := m.Assignees.Assignee(bounded, key)
		if err != nil {
			assignee = ""
		}
		answered <- assignee
	}()
	select {
	case assignee := <-answered:
		return assignee
	case <-bounded.Done():
		return ""
	}
}
