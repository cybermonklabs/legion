package daemon

import (
	"context"
	"errors"
	"fmt"
	"strings"
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
// carries (hookPromptParagraph). It is a failure no retry repairs (permanentRefusal), so the row
// is finished with that message rather than retried.
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
//     own, as the kept copy's was counted from its publish. While the role's wake is in flight
//     (its claim released, or launching with the wake pacing holding the launch), the row waits
//     for the woken session, as the architect's ruling has it: the row waits while the role is
//     being woken, and the hour does not end it then.
//
// Either way the row ends undelivered (hookPromptUndelivered) with the message on the issue. So a
// session that held the role all along is not handed a prompt past its hour, and a failed or
// retired role is never published to.
func (r *outbox) hookPromptEnded(row record.OutboxRow, payload record.HookPrompt, token claim.Token) *hookPromptUndelivered {
	start := row.CreatedAt
	if r.supervisor != nil {
		held := r.supervisedClaim(token)
		if supervise.FailedOrRetired(held.State) {
			return &hookPromptUndelivered{cause: errHookPromptRoleNotComing,
				why: fmt.Sprintf("the %s's claim is %s, so no session of it will hold the role again", payload.Role, held.State)}
		}
		if held.Released() || held.State == supervise.StateLaunching {
			return nil
		}
		if held.LastReady.After(start) {
			start = held.LastReady
		}
	}
	if r.now().Sub(start) >= hookPromptHoldBound {
		return &hookPromptUndelivered{cause: errHookPromptPastItsHour,
			why: fmt.Sprintf("the hour after the hook fired passed and no session of the %s took it", payload.Role)}
	}
	return nil
}

// hookPromptMessageWindow is how long the message on an issue that names its undelivered hook
// prompts to one role is held before it posts, counted from the end of the first of them: every
// prompt of that role on that issue that ends undelivered inside it is named in that one message
// (tellHookPromptUndelivered), and one that ends after it opens a message of its own.
const hookPromptMessageWindow = time.Hour

// tellHookPromptUndelivered names prompt, which ended undelivered for why, in the message on issue
// for its role's undelivered prompts, inside tx, and answers when that message posts. The Dispatch
// message row of issue that is that role's open window (record.Store.HookPromptsMessage: its
// payload names the role's undelivered prompts, no post has tried it, and it was made in the last
// hookPromptMessageWindow) gets the prompt's paragraph added to its data and its body rendered
// again (hookPromptsMessage). With none, a new message row is written, made now on the daemon's
// clock and due hookPromptMessageWindow from now, which opens the role's window. The row is the
// window's whole state, so a daemon that restarts inside the window adds to the same message, and
// the window's lock, which HookPromptsMessage takes first, keeps two finishes from opening two. A
// row a post already tried is closed, even while it is held for its retry, since that retry may
// find the post made.
func (r *outbox) tellHookPromptUndelivered(ctx context.Context, tx pgx.Tx, issue string, prompt record.HookPrompt, why string) (time.Time, error) {
	now := r.now().UTC()
	paragraph := hookPromptParagraph(prompt, why)
	held, open, err := r.records.HookPromptsMessage(ctx, tx, issue, prompt.Role, now, now.Add(-hookPromptMessageWindow))
	if err != nil {
		return time.Time{}, err
	}
	if open {
		payload, err := record.DecodeOutboxPayload(held)
		if err != nil {
			return time.Time{}, err
		}
		message, ok := payload.(record.MessagePost)
		if !ok || message.HookPrompts == nil {
			return time.Time{}, fmt.Errorf("outbox row %d names no undelivered hook prompts of the %s on %s", held.ID, prompt.Role, issue)
		}
		prompts := *message.HookPrompts
		prompts.Paragraphs = append(prompts.Paragraphs, paragraph)
		return held.NextAt, r.records.ReplaceMessage(ctx, tx, held.ID, issue, hookPromptsMessage(issue, prompts))
	}
	row, err := record.NewOutboxRow(issue, hookPromptsMessage(issue, record.UndeliveredHookPrompts{Role: prompt.Role, Paragraphs: []string{paragraph}}),
		now.Add(hookPromptMessageWindow))
	if err != nil {
		return time.Time{}, err
	}
	row.CreatedAt = now
	return row.NextAt, r.records.Enqueue(ctx, tx, row)
}

// hookPromptsMessage is the message on issue that names prompts, one role's undelivered hook
// prompts: the data the message is found and added to by (record.UndeliveredHookPrompts), and the
// body rendered from it, which is an opening, each prompt's paragraph (hookPromptParagraph) in the
// order they ended, and a closing, a blank line between each. The body is rendered at each write,
// so the row posts as any message posts, and a row whose data a rollback stripped posts the body
// it holds.
func hookPromptsMessage(issue string, prompts record.UndeliveredHookPrompts) record.MessagePost {
	opening := fmt.Sprintf("Legion hook prompts to the %s of %s were not delivered. This message names every prompt to the %s that ended undelivered in the hour after the first of them did.",
		prompts.Role, issue, prompts.Role)
	closing := fmt.Sprintf("No session of the %s received these words, and the daemon will not send them again.", prompts.Role)
	body := opening + "\n\n" + strings.Join(prompts.Paragraphs, "\n\n") + "\n\n" + closing
	return record.MessagePost{Body: body, HookPrompts: &prompts}
}

// hookPromptParagraph is the paragraph of the message on an issue that names one undelivered
// prompt: written in the form of the message the hook's firing posts (workflow's Engine.fireHook:
// "Legion hook <name> fired at <n> moves back: <from> to <to>."), so the two read as a pair, then
// why it was not delivered and the words it carried, so a person or the issue's architect can give
// them to the role another way.
func hookPromptParagraph(prompt record.HookPrompt, why string) string {
	return fmt.Sprintf("Legion hook %s fired at %d moves back: %s to %s. Its prompt for that round was not delivered, because %s. The prompt told the %s: %q",
		prompt.Hook, prompt.Count, prompt.From, prompt.To, why, prompt.Role, prompt.Prompt)
}
