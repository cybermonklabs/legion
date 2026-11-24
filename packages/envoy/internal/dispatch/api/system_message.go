package api

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// postSystemMessage posts body as an untargeted message on issue issueKey under the id messageID,
// written by actor, a system actor of Dispatch's own (Background.PostSystemMessage), and answers
// the message's id. It is the issue message route's own write (createStoredMessage) under the same
// check of the body (validateCreateMessage), so the message, its blocks, its references and its
// message.created event are written exactly as a person's are. A system actor's event notifies
// nobody (events.Broker.Notify), so the message wakes no route, the issue's role among them: it is
// read on the issue, in its history, and by the sessions that follow the issue's topic. An actor
// of any other kind is refused, since this write checks no credential: a person or a session posts
// through the route.
//
// The caller names the id, so a post made again - by a process that died between the post and its
// own record of it, or by two processes at once - writes no second message: a message already
// stored under messageID by this actor on this issue is answered as posted, whether this call found
// it first or lost the insert to it. One stored under that id by anyone else, or on another issue,
// is refused.
func (s *server) postSystemMessage(ctx context.Context, actor model.Actor, messageID, issueKey, body string) (string, error) {
	if actor.Kind != "system" || actor.ID == "" {
		return "", fmt.Errorf("a system message's author must be a system actor, not %q %q", actor.Kind, actor.ID)
	}
	if posted, err := s.systemMessagePosted(ctx, actor, messageID, issueKey); err != nil || posted {
		return messageID, err
	}
	input := createMessageInput{Body: body, id: messageID}
	delivery, err := validateCreateMessage(&input)
	if err != nil {
		return "", err
	}
	message, _, err := s.createStoredMessage(store.WithTransactionTracking(ctx), &issueKey, input, delivery, false, actor)
	if isUniqueViolation(err) {
		if posted, readErr := s.systemMessagePosted(ctx, actor, messageID, issueKey); readErr != nil || posted {
			return messageID, readErr
		}
	}
	if err != nil {
		return "", fmt.Errorf("post a message on %s: %w", issueKey, err)
	}
	return message.ID, nil
}

// systemMessagePosted reports whether actor already posted a message under messageID on issueKey,
// and refuses a message under that id that is not that post.
func (s *server) systemMessagePosted(ctx context.Context, actor model.Actor, messageID, issueKey string) (bool, error) {
	var issue *string
	var kind, id string
	err := s.deps.Store.Pool.QueryRow(ctx, `
		select issue_key, coalesce(author ->> 'kind', ''), coalesce(author ->> 'id', '') from messages where id = $1::uuid
	`, messageID).Scan(&issue, &kind, &id)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("read message %s: %w", messageID, err)
	case issue == nil || *issue != issueKey || kind != actor.Kind || id != actor.ID:
		return false, fmt.Errorf("message %s is not %s's post on %s", messageID, actor.ID, issueKey)
	}
	return true, nil
}
