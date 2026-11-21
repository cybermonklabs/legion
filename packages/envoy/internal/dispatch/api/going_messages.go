package api

// going_messages.go is where a command-bar plan's message to an agent session goes (R7). The
// person asks the bar to message an agent, and the plan holds one row for it: a send to the
// session (dispatch_send_to_agent) that names the session, the delivery and the whole text, and
// the issue the server finds that the session works on, which the person may change before they
// approve it. Once approved, the send is posted on that issue as a message that targets the
// session (POST /api/v1/issues/{key}/messages), so it is kept and threaded in the issue's
// conversation with its replies; only a send to a session that works on no issue goes to the
// session's own conversation (POST /api/v1/agents/{session_id}/messages). The server does the
// same with a send the model proposes naming the session alone, so no message to an agent of an
// issue leaves its issue. Each row that sends a message says what became of it: sent, queued or
// refused (goingRowDelivery).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/going/agentwire"
	dispatchenvoy "github.com/sjawhar/envoy/internal/dispatch/envoy"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// goingSendToAgent is the tool a message to one agent session is proposed as.
const goingSendToAgent = "dispatch_send_to_agent"

// goingPlaceSend is where a proposed send to an agent session goes, read before the plan keeps its
// row: the session the send names, put in its arguments by its id - the holder of a role it names
// as `role:<name>`, the session it names by its title (goingReader.recipient) - so the row names
// the session it sends to; and the issue it is posted on, which answers. That issue is the one the
// session works on - an open issue whose claim it holds or whose route names it - preferring the
// issue of the page the run was asked on (the run's context chip), then the issue whose claim it
// took last, then the issue its route names that changed last; "" for a session that works on no
// issue, and for a send that answers a message (in_reply_to), which stays in that message's own
// conversation. doer reads the roster as the run's person.
func (s *server) goingPlaceSend(ctx context.Context, doer GoingDoer, run string, arguments map[string]any) (string, error) {
	name := strings.TrimSpace(goingText(arguments, "session"))
	if name == "" {
		// The tool's schema refuses the send, as its row's read says.
		return "", nil
	}
	session, err := s.goingSendSession(ctx, goingReader{doer: doer, knownSession: s.goingKnownSession}, name)
	if err != nil {
		return "", err
	}
	arguments["session"] = session
	if goingText(arguments, "in_reply_to") != "" {
		return "", nil
	}
	// The run's page is read in the same statement: its issue sorts first among the session's.
	var issue string
	err = s.deps.Store.Pool.QueryRow(ctx, `
		select key from (
		  select key, updated_at,
		    key = coalesce((select context->>'issue' from going_runs where id = $2::uuid), '') as paged,
		    coalesce(claimed_by->>'kind' = 'session' and claimed_by->>'id' = $1, false) as claims,
		    claimed_at
		  from issues
		  where closed_at is null
		    and (claimed_by @> jsonb_build_object('kind', 'session', 'id', $1::text) or route = 'session:' || $1)
		) works
		order by paged desc, claims desc, case when claims then claimed_at end desc nulls last, updated_at desc, key
		limit 1
	`, session, run).Scan(&issue)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return "", nil
	case err != nil:
		return "", fmt.Errorf("read the issue session %s works on: %w", session, err)
	}
	return issue, nil
}

// goingSendSession is the id of the session a send names: the live holder of the role a
// `role:<name>` names, as the Envoy listener answers it; the session a `session:<id>` names; and
// otherwise the session named by its id or its title (goingReader.recipient). A role no session
// holds is 404 GOING_SESSION_NOT_FOUND, and a listener that could not be asked 503
// ENVOY_UNAVAILABLE.
func (s *server) goingSendSession(ctx context.Context, read goingReader, name string) (string, error) {
	route, err := model.ParseRoute(name)
	switch {
	case err == nil && route.Kind == "role":
		if s.deps.Envoy == nil {
			return "", goingRefusal{Status: http.StatusServiceUnavailable, Code: "ENVOY_UNAVAILABLE",
				Message: "ENVOY_URL is not configured, so the holder of the role " + route.ID + " cannot be read"}
		}
		holder, err := s.deps.Envoy.Role(ctx, route.ID)
		switch {
		case errors.Is(err, dispatchenvoy.ErrNoRoleHolder):
			return "", goingRefusal{Status: http.StatusNotFound, Code: codeGoingSessionNotFound,
				Message: fmt.Sprintf("no live session holds the role %s", route.ID)}
		case err != nil:
			return "", goingRefusal{Status: http.StatusServiceUnavailable, Code: "ENVOY_UNAVAILABLE", Message: err.Error()}
		}
		return holder.SessionID, nil
	case err == nil && route.Kind == "session":
		name = route.ID
	}
	return read.recipient(ctx, name)
}

// goingEditSend puts the person's edits (values) of a row's send to an agent session over its
// arguments, and answers the issue the send goes on: the issue an `issue` value names, which the
// send's arguments do not hold; else, where a value names another session, the issue that session
// works on (goingPlaceSend), the session put in the arguments by its id; else issue, the row's own.
func (s *server) goingEditSend(ctx context.Context, doer GoingDoer, run string, arguments, values map[string]any, issue string) (string, error) {
	for name, value := range values {
		if name != "issue" {
			arguments[name] = value
		}
	}
	if _, named := values["session"]; named {
		placed, err := s.goingPlaceSend(ctx, doer, run, arguments)
		if err != nil {
			return "", err
		}
		issue = placed
	}
	if value, named := values["issue"]; named {
		key, isText := value.(string)
		if !isText {
			return "", GoingArgumentError{Action: goingSendToAgent, Reason: `value "issue" must be the key of the issue the message is posted on`}
		}
		issue = strings.TrimSpace(key)
	}
	return issue, nil
}

// sendIssue is the issue the row's send to an agent session is posted on, as the plan shows it:
// "" for a row of any other tool, and for a send to a session that works on no issue.
func (row goingPlanRowStored) sendIssue() string {
	if row.tool != goingSendToAgent {
		return ""
	}
	return row.shown.Issue
}

// The words a row that sends a message says of it (goingRowResult.Delivery).
const (
	goingDeliverySent    = "sent"
	goingDeliveryQueued  = "queued"
	goingDeliveryRefused = "refused"
)

// goingRowDelivery is what became of the message the row of request sent, as the answer of its
// write says, "" for a tool that sends neither a message nor a broadcast: refused where the call or
// its route refused it (failed), or where a message it sent failed its last delivery; queued where
// a delivery is still to be made - the listener stored the message for a session that is not
// running, which gets it when it runs again, or the attempt was not yet confirmed when the route
// answered, as a broadcast's are, which it sends once it answered; and sent where each message
// reached its session, or targets none, a message posted on an issue for whoever reads it. A
// replayed answer (agentwire.CallAnswer.Replayed) is what the route recorded in the transaction that
// stored the message, before its first delivery attempt, so it says nothing of a delivery: the
// deliveries are read from the store, for each message the call's landings name
// (goingLandedDeliveries), and a read that fails says queued.
func (s *server) goingRowDelivery(ctx context.Context, request agentwire.CallRequest, answer agentwire.CallAnswer, failed bool) string {
	switch goingWriteTools[request.Tool].Class {
	case goingClassMessage, goingClassBroadcast:
	default:
		return ""
	}
	if failed {
		return goingDeliveryRefused
	}
	if answer.Replayed {
		states, named, err := s.goingLandedDeliveries(ctx, goingCallKey{run: request.Run, step: request.Step, call: request.Call})
		if err != nil {
			slog.Error("dispatch: read the deliveries of a command-bar row's message", "run", request.Run,
				"step", request.Step, "call", request.Call, "error", err)
			return goingDeliveryQueued
		}
		if named {
			return goingDeliveryOf(states)
		}
	}
	// A message answers its deliveries, a broadcast each recipient's message, and a redelivery
	// the attempt it made.
	var sent struct {
		State      string          `json:"state"`
		Deliveries []goingDelivery `json:"deliveries"`
		Recipients []struct {
			Message struct {
				Deliveries []goingDelivery `json:"deliveries"`
			} `json:"message"`
		} `json:"recipients"`
	}
	if answer.Cut || json.Unmarshal(answer.Body, &sent) != nil {
		// An answer that does not read confirms no delivery.
		return goingDeliveryQueued
	}
	var states []string
	if sent.State != "" {
		states = append(states, sent.State)
	}
	last := func(deliveries []goingDelivery) {
		if len(deliveries) > 0 {
			states = append(states, deliveries[len(deliveries)-1].State)
		}
	}
	last(sent.Deliveries)
	for _, recipient := range sent.Recipients {
		last(recipient.Message.Deliveries)
	}
	return goingDeliveryOf(states)
}

// goingDeliveryOf is the word for the last delivery states of the messages a row sent: queued
// while one is pending or stored for its session, refused where one failed, and sent otherwise.
func goingDeliveryOf(states []string) string {
	switch {
	case slices.ContainsFunc(states, func(state string) bool {
		return state == model.DeliveryStatePending || state == model.DeliveryStateQueued
	}):
		return goingDeliveryQueued
	case slices.Contains(states, model.DeliveryStateFailed):
		return goingDeliveryRefused
	}
	return goingDeliverySent
}

// goingLandedDeliveries is the state of the last delivery attempt of each targeted message that
// the landings of key name in their recovery points (goingRecordRecovery): the message a send
// stored, and each recipient's message of a broadcast. A targeted message with no attempt yet is
// pending; a message that targets none has no delivery and adds no state. named reports whether
// the landings name any message.
func (s *server) goingLandedDeliveries(ctx context.Context, key goingCallKey) (states []string, named bool, err error) {
	rows, err := s.deps.Store.Pool.Query(ctx, `
		select m.target is not null, coalesce(d.state, $4)
		from going_landings l
		join messages m on m.id = (l.recovery->>'message_id')::uuid or m.broadcast_id = (l.recovery->>'broadcast_id')::uuid
		left join lateral (
			select state from message_deliveries where message_id = m.id order by attempt desc limit 1
		) d on true
		where l.run_id = $1::uuid and l.phase = 'do' and l.step = $2 and l.call = $3
	`, key.run, key.step, key.call, model.DeliveryStatePending)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	for rows.Next() {
		var targeted bool
		var state string
		if err := rows.Scan(&targeted, &state); err != nil {
			return nil, false, err
		}
		named = true
		if targeted {
			states = append(states, state)
		}
	}
	return states, named, rows.Err()
}
