package api

// going_modes.go is which rows of a command-bar run's plan wait for the person (R5). In Ask, the
// default, every row waits. In Auto-review, a step of the run calls the chat model once with the
// person's words and the plan's rows (going/agent), and lets through only the rows of the classes
// goingAutoReviewClasses names that those words ask for, that wait for none of the reasons of
// goingRowWaits, and whose needed rows it lets through too. In both modes these wait: a row of a
// class goingClassesThatWait names, a row whose event the outbox would deliver to an agent session
// (outbox.WouldDeliver) - for a row that sets its issue's route, by the route it sets, where the
// outbox sends the write's own event - and a row whose issue's claim a session holds, read when the
// plan shows and again before the row's write (going_plan_run.go).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/outbox"
)

// The two modes of a person's setting (user_going_settings.mode) and of a run (going_runs.mode).
const (
	goingModeAsk        = "ask"
	goingModeAutoReview = "auto_review"
)

// goingClassesThatWait are the classes whose rows wait for the person in both modes: what the
// person settles in their own name (an approval or a review, an answer to an ask, a kept decision,
// a check result, a deploy's record), a change of a setting of Dispatch, and what reaches a person
// or a session when it is sent (a message, a broadcast).
var goingClassesThatWait = map[goingWriteClass]bool{
	goingClassReview: true, goingClassAskAnswer: true, goingClassDecision: true, goingClassResult: true,
	goingClassSetting: true, goingClassMessage: true, goingClassBroadcast: true,
}

// goingAutoReviewClasses are the classes Auto-review may let through: a change of fields, of
// links and of a claim, and a create with no text but a title, which for an issue only a run in
// Auto-review keeps in its plan: a run in Ask and a new-issue run file each issue with a spec
// (goingCallRun.specRequired).
var goingAutoReviewClasses = map[goingWriteClass]bool{
	goingClassField: true, goingClassLink: true, goingClassClaim: true, goingClassCreatePlain: true,
}

// goingRowClass is the class of a row of tool with arguments: its tool's class, except that an
// issue's create that carries a spec is a create with text (goingClassCreateText), where one with
// a title alone is a plain create (goingClassCreatePlain).
func goingRowClass(tool string, arguments json.RawMessage) goingWriteClass {
	class := goingWriteTools[tool].Class
	if tool != goingIssueCreateTool {
		return class
	}
	var create struct {
		Spec string `json:"spec"`
	}
	if json.Unmarshal(arguments, &create) == nil && strings.TrimSpace(create.Spec) != "" {
		return goingClassCreateText
	}
	return class
}

// The reasons a row waits for the person in both modes beside its class (goingRowShown.Waits):
// its event would reach an agent session, or a session holds its issue's claim.
const (
	goingWaitsDelivery = "delivers_to_session"
	goingWaitsClaimed  = "claimed_by_session"
)

// goingRowWaits is why a row of tool waits for the person in both modes: its class, where
// goingClassesThatWait names it; goingWaitsDelivery where event, the event its write would append
// (goingRowEvent), would reach an agent session (outbox.WouldDeliver), read over routeSet, the
// route the write sets on its issue (goingRouteSet), nil for a write that sets none; and
// goingWaitsClaimed where a session holds the claim of its issue (claim). None for a row
// Auto-review may let through.
func (s *server) goingRowWaits(ctx context.Context, tool string, event *model.Event, routeSet *string, claim *goingHolder) ([]string, error) {
	entry := goingWriteTools[tool]
	var waits []string
	if goingClassesThatWait[entry.Class] {
		waits = append(waits, string(entry.Class))
	}
	if event != nil {
		delivers, err := outbox.WouldDeliver(ctx, s.deps.Store, s.deps.Events, *event, routeSet)
		if err != nil {
			return nil, err
		}
		if delivers {
			waits = append(waits, goingWaitsDelivery)
		}
	}
	if claim != nil && claim.Kind == "session" {
		waits = append(waits, goingWaitsClaimed)
	}
	return waits, nil
}

// goingRouteSet is the route a write, prepared, sets on its issue, "" for one that clears it, and
// nil for a write that sets none: an update of an issue (goingIssuePatchRoute) whose body names a
// route its route takes, none or role:<name> or session:<id> (checkIssueRoute). The outbox sends
// the write's own event to that route, which it reads once the write is in (outbox.WouldDeliver).
// A route that does not parse is refused by the write's route with nothing written, so its write
// is read over the route stored.
func goingRouteSet(prepared goingPrepared) *string {
	if prepared.route.Method+" "+prepared.route.Pattern != goingIssuePatchRoute {
		return nil
	}
	route, named := prepared.call.body["route"].(string)
	if !named || checkIssueRoute(&route) != nil {
		return nil
	}
	return &route
}

// goingRowEvent is the event the write of a row of tool, prepared, would append, as far as the
// outbox reads it to decide where it goes (outbox.WouldDeliver), nil for a tool whose route
// appends none: its type, the person (login) as its actor, its one owner as the broker appends it -
// for a document its route names, by its id or a prefix of it or by its slug under its issue or
// project, the issue that owns it, or the document itself where no issue does; else the issue it
// is of (shown); else the project its route names - and the members of its payload the outbox's
// routing reads: the ask or comment it is of, a comment's ask and the comment it replies to, a
// message's target and the message it answers, the claim it moves, and whether a document's
// version is named.
func (s *server) goingRowEvent(ctx context.Context, tool string, prepared goingPrepared, shown goingRowShown, login string) (*model.Event, error) {
	kind := goingWriteTools[tool].Event
	if kind == goingNoEvent || kind == "" {
		return nil, nil
	}
	event := model.Event{Type: kind, Actor: model.Actor{Kind: "user", ID: login}}
	path, body := prepared.call.path, prepared.call.body
	payload := map[string]any{}
	set := func(member, value string) {
		if value != "" {
			payload[member] = value
		}
	}
	// document reads the document the route names, by its id (or a prefix of it) or by slug
	// under owner, an issue's key or a project's: its event's owner is its issue, which owns the
	// documents under it, and a document no issue owns is its own.
	document := func(id, owner, slug string) error {
		var found, issue string
		err := s.deps.Store.Pool.QueryRow(ctx, `
			select id::text, coalesce(issue_key, '') from artifacts
			where case when $1 <> '' then id::text like $1 || '%'
			      else slug = $3 and (issue_key = $2 or (issue_key is null and project_key = $2)) end
			order by issue_key nulls last limit 1
		`, id, owner, slug).Scan(&found, &issue)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
		case err != nil:
			return fmt.Errorf("read the document of a %s change: %w", tool, err)
		case issue != "":
			event.IssueKey = &issue
		default:
			event.ArtifactID = &found
		}
		return nil
	}
	pattern := prepared.route.Pattern
	switch {
	case strings.HasPrefix(pattern, "/api/v1/asks/{id}"):
		set("id", path["id"])
		if kind == "comment.created" {
			set("ask_id", path["id"])
		}
	case strings.HasPrefix(pattern, "/api/v1/comments/{id}"):
		set("id", path["id"])
		var askID, replyTo string
		err := s.deps.Store.Pool.QueryRow(ctx, `
			select coalesce(ask_id::text, ''), coalesce(reply_to::text, '') from comments where id::text = $1
		`, path["id"]).Scan(&askID, &replyTo)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("read the thread of comment %s: %w", path["id"], err)
		}
		set("ask_id", askID)
		set("reply_to", replyTo)
	case strings.HasPrefix(pattern, "/api/v1/artifacts/{id}") && path["id"] != "":
		if err := document(path["id"], "", ""); err != nil {
			return nil, err
		}
	case strings.Contains(pattern, "/artifacts/{slug}") && path["slug"] != "":
		owner := path["key"]
		if strings.HasPrefix(pattern, "/api/v1/issues/") && shown.Issue != "" {
			owner = shown.Issue
		}
		if err := document("", owner, path["slug"]); err != nil {
			return nil, err
		}
	case pattern == "/api/v1/messages/{id}":
		// An edit or a delete of an issue message: its event carries the message's own thread, the
		// message it answers and the session it targets, as its create's did
		// (messageCorrectionEvent), not the message itself as a reply's in_reply_to.
		var inReplyTo, target string
		err := s.deps.Store.Pool.QueryRow(ctx, `
			select coalesce(in_reply_to::text, ''), coalesce(target, '') from messages where id::text = $1
		`, path["id"]).Scan(&inReplyTo, &target)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("read the thread of message %s: %w", path["id"], err)
		}
		set("id", path["id"])
		set("in_reply_to", inReplyTo)
		set("target", target)
	case strings.HasPrefix(pattern, "/api/v1/messages/{id}"):
		set("in_reply_to", path["id"])
	case strings.HasPrefix(pattern, "/api/v1/agents/{session_id}"):
		set("target", path["session_id"])
	}
	switch {
	case event.IssueKey != nil || event.ArtifactID != nil:
	case shown.Issue != "":
		event.IssueKey = &shown.Issue
	case strings.HasPrefix(pattern, "/api/v1/projects/{key}") && path["key"] != "":
		key := path["key"]
		event.ProjectKey, event.Project = &key, key
	}
	for _, member := range []string{"reply_to", "target", "in_reply_to"} {
		if value, ok := body[member].(string); ok {
			set(member, value)
		}
	}
	switch kind {
	case "artifact.version":
		payload["version"] = map[string]any{"named": tool == "dispatch_name_version"}
	case "issue.claimed", "issue.released":
		if shown.Claim != nil {
			payload["previous_claim"] = map[string]any{"actor": map[string]any{"kind": shown.Claim.Kind, "id": shown.Claim.ID}}
		}
	}
	event.Payload = payload
	return &event, nil
}

// class is the row's class (goingRowClass), read from its tool and the arguments it runs with.
func (row goingPlanRowStored) class() goingWriteClass {
	return goingRowClass(row.tool, row.arguments)
}

// autoReviewLets reports whether Auto-review may let the row through: its class is one
// goingAutoReviewClasses names, and it waits for nothing in both modes.
func (row goingPlanRowStored) autoReviewLets() bool {
	return goingAutoReviewClasses[row.class()] && len(row.shown.Waits) == 0
}
