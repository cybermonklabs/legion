package api

import (
	"context"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// issueThreadLogins is the status half of an issue event's thread_logins: whose Replies count a
// status move can change. Only a close or a reopen (closedMoved) changes one, for the logins with
// a Replies row on the issue's open asks (events.IssueThreadLogins); any other status move names
// nobody, as [] rather than left out, so a reader knows the event says so. A close withdraws the
// issue's open asks and a reopen restores them before this read, and each of those ask events
// names its own thread's logins, so after a close this names only an open ask a close by an older
// release left behind. tx must hold those ask writes: a writer that only queued them has the
// event's own insert read the logins instead (events.IssueLoginsOf).
func issueThreadLogins(ctx context.Context, tx pgx.Tx, issueKey string, closedMoved bool) ([]string, error) {
	if !closedMoved {
		return []string{}, nil
	}
	return events.IssueThreadLogins(ctx, tx, issueKey)
}

// movedAssigneeLogins is the canonical login of each assignee a write moved the issue between -
// the one it took it from and the one it gave it to, either of which may be nil for an issue
// nobody held or nobody holds - sorted and without a duplicate, and [] for a write that moved
// none. The Replies list leaves out an open ask on an open issue assigned to the reader
// (api/replies.go, myRepliesOwner), so moving the assignee takes those asks out of the new
// assignee's list and puts them into the old one's.
func movedAssigneeLogins(movedAssignees []*string) []string {
	logins := []string{}
	for _, assignee := range movedAssignees {
		if assignee == nil {
			continue
		}
		login := model.CanonicalLogin(*assignee)
		if login == "" || slices.Contains(logins, login) {
			continue
		}
		logins = append(logins, login)
	}
	slices.Sort(logins)
	return logins
}

// issueWriteThreadLogins is the thread_logins of an issue.updated or issue.closed: every login
// whose Inbox rows the write can have moved, sorted and without a duplicate.
//
// A status move names issueThreadLogins' logins, an assignee move movedAssigneeLogins', and a
// write that moved both the union of the two, which is why the result is sorted here rather than
// taken as events.IssueThreadLogins sorted it.
//
// movedAssignees is nil on a write that left the assignee alone, so a status-only move still
// answers exactly what it answered before. A writer whose ask writes are only queued cannot read
// the status half here: it hands the assignee half to the event's insert, which unions it into
// the read of the asks (events.IssueLoginsOf).
func issueWriteThreadLogins(
	ctx context.Context, tx pgx.Tx, issueKey string, statusMoved, closedMoved bool, movedAssignees []*string,
) ([]string, error) {
	logins := movedAssigneeLogins(movedAssignees)
	if !statusMoved {
		return logins, nil
	}
	moved, err := issueThreadLogins(ctx, tx, issueKey, closedMoved)
	if err != nil {
		return nil, err
	}
	for _, login := range logins {
		if !slices.Contains(moved, login) {
			moved = append(moved, login)
		}
	}
	slices.Sort(moved)
	return moved, nil
}

// commentThread is the thread the user_threads trigger files comment under (0095's
// user_threads_comment): its ask, else the root it replies to, else the comment itself.
func commentThread(comment model.Comment) string {
	switch {
	case comment.AskID != nil:
		return *comment.AskID
	case comment.ReplyTo != nil:
		return *comment.ReplyTo
	}
	return comment.ID
}

// commentThreadLogins is a written comment's thread_logins: the logins with a Replies row on its
// thread (commentThread). tx must hold the comment's insert.
func commentThreadLogins(ctx context.Context, tx pgx.Tx, comment model.Comment) ([]string, error) {
	return events.ThreadLogins(ctx, tx, commentThread(comment))
}

// messageThreadRoot is the thread root of message messageID, which replies to inReplyTo (nil: a
// root, the message itself), found up in_reply_to as the user_threads trigger finds it (0095's
// user_threads_message). The walk starts at inReplyTo, whose chain is the message's own above it,
// so it finds the root before the message is written.
func messageThreadRoot(ctx context.Context, tx pgx.Tx, messageID string, inReplyTo *string) (string, error) {
	if inReplyTo == nil {
		return messageID, nil
	}
	var root string
	if err := tx.QueryRow(ctx, messageThreadCTE+`
		select id::text from thread where in_reply_to is null
	`, *inReplyTo).Scan(&root); err != nil {
		return "", fmt.Errorf("find the thread root of message %s: %w", messageID, err)
	}
	return root, nil
}

// messageThreadLogins is a written message's thread_logins: every login whose Inbox the message
// can have moved. An issue message moves a Replies row, so it names the logins with a row on its
// thread root (messageThreadRoot), found up in_reply_to as the user_threads trigger finds it
// (0095's user_threads_message). An issue-less message is a direct conversation, which keeps no
// user_threads row: it moves the Agent row of the person who opened the thread
// (api/replies_agent.go, myAgentRootFilter), so it names that root author's canonical login, and
// [] when a session or the system opened the thread and no Agent row holds it
// (directThreadLogins). tx must hold the message's insert, or have queued it: neither half reads
// the message's own row.
func messageThreadLogins(ctx context.Context, tx pgx.Tx, message model.Message) ([]string, error) {
	if message.IssueKey == nil {
		return directThreadLogins(ctx, tx, message)
	}
	root, err := messageThreadRoot(ctx, tx, message.ID, message.InReplyTo)
	if err != nil {
		return nil, err
	}
	return events.ThreadLogins(ctx, tx, root)
}

// directThreadLogins is a direct (issue-less) message's thread_logins: the canonical login of its
// thread root's author, which is the one Inbox an Agent row of this thread can sit in, and [] when
// that author is not a person. The root is found with messageThreadCTE, the walk the issue
// messages take, from the message this one answers rather than from the message itself, whose
// chain is its own above it (messageThreadRoot): a writer whose insert is only queued reads the
// same root as one whose insert has run. A message that answers nothing is its own root, so its
// author needs no query.
func directThreadLogins(ctx context.Context, tx pgx.Tx, message model.Message) ([]string, error) {
	kind, id := message.Author.Kind, message.Author.ID
	if message.InReplyTo != nil {
		if err := tx.QueryRow(ctx, messageThreadCTE+`
			select coalesce(m.author ->> 'kind', ''), coalesce(m.author ->> 'id', '')
			from messages m join thread t on t.id = m.id
			where t.in_reply_to is null
		`, *message.InReplyTo).Scan(&kind, &id); err != nil {
			return nil, fmt.Errorf("find the thread root's author of message %s: %w", message.ID, err)
		}
	}
	login := model.CanonicalLogin(id)
	if kind != "user" || login == "" {
		return []string{}, nil
	}
	return []string{login}, nil
}
