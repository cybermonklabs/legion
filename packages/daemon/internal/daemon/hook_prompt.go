package daemon

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/wait"
)

// hookPromptHoldBound is how long after its row was made a hook prompt waits for a session to hold
// its role (hookPromptUnheld, hookPromptEnded): the hour the Envoy listener kept the prompt for (its
// keep window, roleKeepWindow in packages/envoy/cmd/listener/role_keep.go) before it refused to
// keep a publish that names no source session. So a session of the role that comes within the hour
// gets the prompt, as it got the kept copy, and one that comes later does not, as it did not.
const hookPromptHoldBound = time.Hour

// hookPromptUndelivered is a hook prompt's row ending undelivered (hookPromptEnded): why is the
// clause that says why no session of its role will be handed it, which the message on the issue
// carries (record.HookPrompt.Undelivered). It is a failure no retry repairs (permanentRefusal), so
// the row is finished with that message rather than retried.
type hookPromptUndelivered struct {
	why   string
	cause error
}

func (e *hookPromptUndelivered) Error() string {
	return "the hook prompt is not delivered, because " + e.why + ": " + e.cause.Error()
}

func (e *hookPromptUndelivered) Unwrap() error { return e.cause }

// hookPromptUnheld is what a hook prompt's row does when no live session holds its role (cause):
// the listener refused the publish and kept nothing, its role unclaimed or its holder gone
// (notify.ErrNoHolder) or only unresponsive (notify.ErrHolderUnresponsive), or the gate published
// nothing to a claim recording a session it does not hold (publishKept). The row is the prompt's
// keeper, so it waits (wait.ErrWaiting), tried again on its backoff: a released role's woken
// session claims the role a few attempts after the resume launched it, and a role with no claim
// yet may be started inside the hour. The ends no attempt can change, a claim that failed or was
// retired and an hour that has passed (hookPromptEnded), are read at the top of every attempt,
// before anything wakes or publishes, so the next attempt after either ends the row. A refused
// publish stores nothing, so the one publish a session takes is the one it is handed, and a retry
// of a publish whose answer was lost is stored once, under the row's own key (record.OutboxKey),
// which every attempt sends.
func (r *outbox) hookPromptUnheld(row record.OutboxRow, payload record.HookPrompt, cause error) error {
	return wait.Errorf("the %s hook's prompt for %s waits for a session to hold its %s's role: %w", payload.Hook, row.Issue, payload.Role, cause)
}

// errHookPromptPastItsHour and errHookPromptRoleNotComing are the causes hookPromptEnded gives a
// prompt it ends before any attempt to publish it.
var (
	errHookPromptPastItsHour   = errors.New("the hour after the hook fired has passed")
	errHookPromptRoleNotComing = errors.New("the role's claim failed or was retired")
)

// hookPromptEnded is the end of a hook prompt's row that no attempt to publish it can change, and
// nil while the prompt may still be delivered. It runs at the top of every attempt (hookPrompt),
// before anything wakes the role or publishes. Two ends:
//   - A role whose claim has failed or was retired gets no session that holds the role again.
//   - A prompt whose hour has passed: hookPromptHoldBound from the later of the row's making and
//     the role's last ready (supervise.Claim.LastReady). So the hour a woken session has is its
//     own, as the kept copy's was counted from its publish. While the role is being woken
//     (wakeInFlight), the row waits for the woken session, as the architect's ruling has it: the
//     row waits while the role is being woken, and the hour does not end it then.
//
// Either way the row ends undelivered (hookPromptUndelivered) with the message on the issue. So a
// session that held the role all along is not handed a prompt past its hour, and a failed or
// retired role is never published to.
func (r *outbox) hookPromptEnded(ctx context.Context, row record.OutboxRow, payload record.HookPrompt, token claim.Token) (*hookPromptUndelivered, error) {
	start := row.CreatedAt
	if r.supervisor != nil {
		held := r.supervisedClaim(token)
		if supervise.FailedOrRetired(held.State) {
			return &hookPromptUndelivered{cause: errHookPromptRoleNotComing,
				why: fmt.Sprintf("the %s's claim is %s, so no session of it will hold the role again", payload.Role, held.State)}, nil
		}
		woken, err := r.wakeInFlight(ctx, row.Issue, held)
		if err != nil || woken {
			return nil, err
		}
		if held.LastReady.After(start) {
			start = held.LastReady
		}
	}
	if r.now().Sub(start) >= hookPromptHoldBound {
		return &hookPromptUndelivered{cause: errHookPromptPastItsHour,
			why: fmt.Sprintf("the hour after the hook fired passed and no session of the %s took it", payload.Role)}, nil
	}
	return nil, nil
}

// wakeInFlight is whether held's role on issue is being woken, which holds its hook prompt's row
// past the row's hour (hookPromptEnded): a released claim of an issue a wake can still reach
// (wakeableIssue), which the row's own attempt wakes (wakeForDelivery); a launch the wake pacing
// parked over no process (parkedLaunch); or a launch under a resume of the role that has had no
// first turn yet (store.Store.OpenPodResume), which a wake opened. A released claim of an issue
// that has left the workflow, or whose tree lingers, is a close's to stop and no wake comes to it,
// and a launch no wake opened is the role starting as any role starts, so the hour holds for both:
// without it, a row of an issue moved out of the workflow would be tried about once a minute for
// as long as the issue stayed out, and its message would never post.
func (r *outbox) wakeInFlight(ctx context.Context, issue string, held supervise.ClaimView) (bool, error) {
	switch {
	case held.Released():
		_, wakeable, err := r.wakeableIssue(ctx, issue)
		return wakeable, err
	case held.State != supervise.StateLaunching:
		return false, nil
	}
	if _, parked := parkedLaunch(held); parked {
		return true, nil
	}
	_, _, open, err := r.trees.OpenPodResume(ctx, r.project, issue, held.Role)
	return open, err
}

// hookPromptMessageWindow is how long the message on an issue that names its undelivered hook
// prompts to one role is held before it posts, counted from the end of the first of them: every
// prompt of that role on that issue that ends undelivered inside it is named in that one message
// (tellHookPromptUndelivered), and one that ends after it opens a message of its own.
const hookPromptMessageWindow = time.Hour

// tellHookPromptUndelivered names prompt, which ended undelivered for why, in the message on issue
// for its role's undelivered prompts, inside tx, and answers when that message posts
// (record.Store.NameUndeliveredHookPrompt, on the daemon's clock, over hookPromptMessageWindow).
// The message row is the window's whole state, so a daemon that restarts inside the window adds to
// the same message. The prompt that opens a message also tells the issue's architect, in the same
// transaction (record.HookPromptUndeliveredNotice, hookPromptUndeliveredNotice), so a role that is
// not coming makes one notice an hour, as it makes one message, and the architect can give the
// words to the role another way.
func (r *outbox) tellHookPromptUndelivered(ctx context.Context, tx pgx.Tx, issue string, prompt record.HookPrompt, why string) (time.Time, error) {
	now := r.now().UTC()
	posts, opened, err := r.records.NameUndeliveredHookPrompt(ctx, tx, issue, prompt.Role, prompt.Undelivered(why), now, hookPromptMessageWindow)
	if err != nil || !opened {
		return posts, err
	}
	notice, err := record.NewOutboxRow(issue, hookPromptUndeliveredNotice(issue, prompt, why, posts), now)
	if err != nil {
		return time.Time{}, err
	}
	return posts, r.records.Enqueue(ctx, tx, notice)
}

// hookPromptUndeliveredNotice is the architect's notice of prompt, a hook's prompt to a role of
// issue that ended undelivered for why and opened the message on issue that posts at posts: which
// hook and role, why, and where its words are, which the architect can give to the role another
// way.
func hookPromptUndeliveredNotice(issue string, prompt record.HookPrompt, why string, posts time.Time) record.Notice {
	return record.Notice{Kind: record.HookPromptUndeliveredNotice, Role: claim.RoleArchitect,
		Reason: fmt.Sprintf("Legion hook %s's prompt to the %s of %s was not delivered, because %s. "+
			"A message on %s names it with its words, with every other prompt to the %s that ends undelivered before that message posts at %s. "+
			"Give the words to the %s another way if its round still needs them.",
			prompt.Hook, prompt.Role, issue, why, issue, prompt.Role, posts.UTC().Format(time.RFC3339), prompt.Role)}
}
