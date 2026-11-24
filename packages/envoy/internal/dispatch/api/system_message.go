package api

import (
	"context"
	"fmt"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// postSystemMessage posts body as an untargeted message on issue issueKey, written by actor, a
// system actor of Dispatch's own (Background.PostSystemMessage), and answers the message's id. It
// is the issue message route's own write (createStoredMessage) under the same check of the body
// (validateCreateMessage), so the message, its blocks, its references and its message.created
// event are written exactly as a person's are. A system actor's event notifies nobody
// (events.Broker.Notify), so the message wakes no route, the issue's role among them: it is read on
// the issue, in its history, and by the sessions that follow the issue's topic. An actor of any
// other kind is refused, since this write checks no credential: a person or a session posts
// through the route.
func (s *server) postSystemMessage(ctx context.Context, actor model.Actor, issueKey, body string) (string, error) {
	if actor.Kind != "system" || actor.ID == "" {
		return "", fmt.Errorf("a system message's author must be a system actor, not %q %q", actor.Kind, actor.ID)
	}
	input := createMessageInput{Body: body}
	delivery, err := validateCreateMessage(&input)
	if err != nil {
		return "", err
	}
	message, _, err := s.createStoredMessage(store.WithTransactionTracking(ctx), &issueKey, input, delivery, false, actor)
	if err != nil {
		return "", fmt.Errorf("post a message on %s: %w", issueKey, err)
	}
	return message.ID, nil
}
