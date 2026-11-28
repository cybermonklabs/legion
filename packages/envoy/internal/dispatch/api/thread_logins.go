package api

import (
	"context"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// movedLogins adds to logins the canonical login of each person a write moved an Inbox row
// between, sorted and without a duplicate; naming the same person twice names them once. Each
// person is resolved before it reaches here, so none is nobody: every question reaches a person
// (askedOfPersonExpression). The result is never nil, so an event that moved nobody's rows says
// so as [] rather than leaving the field out.
func movedLogins(logins []string, moved ...string) []string {
	if logins == nil {
		logins = []string{}
	}
	for _, person := range moved {
		login := model.CanonicalLogin(person)
		if login == "" || slices.Contains(logins, login) {
			continue
		}
		logins = append(logins, login)
	}
	slices.Sort(logins)
	return logins
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
