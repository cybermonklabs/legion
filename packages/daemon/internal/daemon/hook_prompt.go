package daemon

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/record"
)

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
