package workflow

import (
	"context"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// roleRecipient is one role a message is addressed to: issue, whose role it is - an architect's is
// the issue it is the architect of, its tree's root for the tree's own - role, and row, the issue
// whose outbox the message is written to, which differs from issue only for an architect told of
// an issue below its own.
type roleRecipient struct {
	issue string
	role  claim.Role
	row   string
}

// addressedRole is a recipient the daemon can name: the claim token whose role topic the message
// is published to, the role of the recipient that named it first, and the issue its outbox row
// belongs to. The role is the recipient's and never read off the token, which names the claim's
// agent (claim.Token.Slot), not the role of its step.
type addressedRole struct {
	token claim.Token
	role  claim.Role
	issue string
}

// addressRoles is the one collection the daemon's role messages are addressed by (tellChecks,
// tellGateMoved, mergeForward, mergeForwardLanded, holdMerger, awaitChildren): the claim of the
// worker on each recipient's role's step (record.WorkerClaim over the recipient issue's phases
// rows, read in tx), in the order given and each token once, so a role named twice - the
// implementer that also holds the phase, two roles one keyed agent works - is told once. A
// recipient with no role is a phase no worker holds, and is nobody.
//
// unnamable is the policy for a recipient whose claim token cannot be built, and the one thing the
// six senders choose of how the collection runs: nil ends the collection with that error, and so
// the fact's transaction, while a function is given the recipient and the error and may answer nil
// to leave that one untold and address the rest. tellChecks, mergeForward, mergeForwardLanded,
// holdMerger and awaitChildren pass nil, so a holder they cannot name rolls their fact back.
// tellGateMoved, whose message is secondary to the record the transaction writes, passes a
// function that logs (logUnnamable), since aborting would roll the gate's move back for a name the
// daemon could not spell.
func (e *Engine) addressRoles(ctx context.Context, tx pgx.Tx, project string, recipients []roleRecipient, unnamable func(roleRecipient, error) error) ([]addressedRole, error) {
	projectToken, err := claim.ProjectToken(project)
	if err != nil {
		return nil, err
	}
	addressed := make([]addressedRole, 0, len(recipients))
	for _, recipient := range recipients {
		if recipient.role == "" {
			continue
		}
		// A failed read is the transaction's failure, never a holder the daemon could not name.
		rows, err := e.store.Phases(ctx, tx, recipient.issue)
		if err != nil {
			return nil, err
		}
		token, err := record.WorkerClaim(projectToken, recipient.issue, recipient.role, rows)
		if err != nil {
			if unnamable == nil {
				return nil, err
			}
			if err := unnamable(recipient, err); err != nil {
				return nil, err
			}
			continue
		}
		if slices.ContainsFunc(addressed, func(a addressedRole) bool { return a.token == token }) {
			continue
		}
		addressed = append(addressed, addressedRole{token: token, role: recipient.role, issue: recipient.row})
	}
	return addressed, nil
}

// logUnnamable is the unnamable policy of a sender whose message is secondary to the record its
// transaction writes: the holder is logged, on the commit, with what the message was about, and
// the rest are told.
func (e *Engine) logUnnamable(ctx context.Context, what string, attributes ...any) func(roleRecipient, error) error {
	return func(recipient roleRecipient, err error) error {
		e.logOnCommit(ctx, what, append(slices.Clone(attributes), "issue", recipient.issue, "role", recipient.role, "error", err)...)
		return nil
	}
}
