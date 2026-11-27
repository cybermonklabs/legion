package main

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

const (
	payloadSession  = "ses_v"
	payloadPrivRole = "legion-priv-priv-1-tester"
	messageID       = "5a660655-04ad-4ce0-8a9b-93dd03c412b7"
	parentID        = "8f14e45f-ceea-467a-9c1e-1b4d9a3f1c2b"
	broadcastID     = "2d1f0c0e-5b8a-4c1e-9e4b-3f2a1b0c9d8e"
	commentID       = "6f1c2a3b-4d5e-4f60-8a1b-2c3d4e5f6a7b"
	documentID      = "6c1f3a52-0000-4000-8000-000000000001"
)

var (
	payloadPerson  = model.Actor{Kind: "user", ID: "alice"}
	payloadCreated = time.Date(2026, 11, 23, 20, 0, 0, 0, time.UTC)
)

// payloadJSON is v as Dispatch marshals it.
func payloadJSON(t *testing.T, v any) string {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// issueEvent is an issue's event as the outbox reads it, its project the issue's
// (readBatchSQL's coalesce of the event's, the issue's and the artifact's project key).
func issueEvent(project, typ string, payload any) model.Event {
	key := project + "-1"
	return model.Event{ID: 41, IssueKey: &key, Project: project, Seq: 7, Type: typ, Actor: payloadPerson,
		Notify: true, CreatedAt: payloadCreated, Payload: payload}
}

// documentEvent is a project document's event, which names its project and no issue.
func documentEvent(project, typ string, payload any) model.Event {
	artifact := documentID
	return model.Event{ID: 42, ArtifactID: &artifact, Project: project, Seq: 3, Type: typ, Actor: payloadPerson,
		Notify: true, CreatedAt: payloadCreated, Payload: payload}
}

// apiEvent is the event the API's targeted-message frame carries (messageEvent,
// api/messages.go): an issue's event under issueOwner, whose project the event leaves empty, or,
// for a message on no issue, an event with no owner at all.
func apiEvent(issue, typ string, payload any) model.Event {
	if issue == "" {
		return model.Event{Type: typ, Actor: payloadPerson, Payload: payload}
	}
	return model.Event{IssueKey: &issue, Type: typ, Actor: payloadPerson, Payload: payload}
}

// message is a message as the API's delivery frame carries it (messageReplyThread.payload over
// the stored message, api/messages.go), to target, on issue ("" for none).
func message(issue, body, target string) model.MessageEventPayload {
	stored := model.Message{ID: messageID, Author: payloadPerson, Body: body, Target: &target,
		CreatedAt: payloadCreated, Deliveries: []model.MessageDelivery{}}
	if issue != "" {
		stored.IssueKey = &issue
	}
	return model.MessageEventPayload{Message: stored}
}

// comment is a comment as a comment.created event carries it, on issue or on the document.
func comment(issue, body string) model.CommentEventPayload {
	stored := model.Comment{ID: commentID, Author: payloadPerson, Body: body, CreatedAt: payloadCreated,
		Mentions: []model.Mention{}, Deliveries: []model.CommentDelivery{}}
	if issue != "" {
		stored.IssueKey = &issue
	} else {
		artifact := documentID
		stored.ArtifactID = &artifact
	}
	return model.CommentEventPayload{Comment: stored, ArtifactName: "spec"}
}

// routed is the outbox's copy of event to one destination (publisher.go's envelope, then
// batch.go's routed.Topic = the destination's subject and its dedupe key where it has one).
func routed(t *testing.T, event model.Event, destination, dedupeKey string) []byte {
	t.Helper()
	item := contracts.Envelope{
		EventID: "dispatch-41", Source: "dispatch", SourceEventID: "41", Topic: destination,
		DedupeKey: "dispatch-41", IssuedAt: payloadCreated.UnixMilli(), PayloadSummary: event.Type,
		Payload: payloadJSON(t, struct {
			model.Event
			Quiet bool `json:"quiet"`
		}{Event: event}),
		TraceID: "dispatch-41",
	}
	if dedupeKey != "" {
		item.DedupeKey = dedupeKey
	}
	return []byte(payloadJSON(t, item))
}

// sent is the API's send of a targeted frame to the session (comment_delivery.go and
// message_delivery.go, through the listener's send route: contracts.AgentSendEnvelope of the
// message Dispatch's client builds), with role as its role mark when the send is for a role.
func sent(t *testing.T, event model.Event, delivery map[string]any, key, role string) []byte {
	t.Helper()
	frame := payloadJSON(t, struct {
		Event    model.Event `json:"event"`
		Delivery any         `json:"delivery"`
	}{event, delivery})
	item := contracts.AgentSendEnvelope(payloadSession, key, contracts.Message{Source: "dispatch", Text: "approved, go ahead", Payload: &frame})
	item.Role = role
	return []byte(payloadJSON(t, item))
}

// withPayload is frame, an envelope, with its payload replaced by payload.
func withPayload(t *testing.T, frame []byte, payload string) []byte {
	t.Helper()
	var item contracts.Envelope
	if err := json.Unmarshal(frame, &item); err != nil {
		t.Fatal(err)
	}
	item.Payload = payload
	return []byte(payloadJSON(t, item))
}

// TestEdgeFrameVerdictPassesADispatchFrameOnlyOfAListedProject holds the edge's frame rule to the
// project of the Dispatch event a frame carries (dispatchFrameWithheld, dispatchEventProject).
// Dispatch sends an issue's event to a session by the session's name - the owner route, the
// claimant, a primary document's author, an author route, a follower, the previous claimant, a
// mention (the API's send, and the outbox's copy to a session whose mention failed), a targeted
// message and a role forward - on the session's own agent subject or on the role's lane, under a
// topic that names the session or the lane and no project. Each kind is built here as Dispatch
// builds it, from Dispatch's own model types (the outbox's envelope with its topic set to the
// destination, or the API's send through contracts.AgentSendEnvelope, with the event it carries),
// once with another project's event, which is withheld, and once with the token's project's,
// which passes. The mention and targeted-message frames carry an issue's event with an empty
// project, as Dispatch sends them, so their project is the one the issue key names. A payload the
// plugin does not read as an event, one with no project, one whose project and issue key disagree,
// and a key of the payload that readers disagree on are withheld; a frame of another source passes
// by its topic whatever its payload says.
func TestEdgeFrameVerdictPassesADispatchFrameOnlyOfAListedProject(t *testing.T) {
	t.Parallel()
	own := contracts.LegacyAgentSubject(payloadSession)
	privLane := contracts.RoleTopicPrefix + payloadPrivRole
	projects := []string{"PRIV"}
	notHeld := frameDrops(withheldProjectNotHeld)
	unread := frameDrops(withheldDispatchProjectUnread)
	target := "session:" + payloadSession
	mention := map[string]any{"attempt": 1, "mode": "steer", "comment_id": commentID, "target": target}
	targeted := map[string]any{"attempt": 1, "mode": "steer"}
	mentionKey := contracts.AgentSendDedupeKey(payloadSession, model.CommentMentionIdempotencyKey(commentID, target, "steer"))

	type row struct {
		name string
		data []byte
		want frameVerdict
	}
	var rows []row
	for _, c := range []struct {
		project string
		want    frameVerdict
	}{{"PUB", notHeld}, {"PRIV", framePasses()}} {
		p, issue := c.project, c.project+"-1"
		rows = append(rows,
			row{"owner route: " + p + "'s issue event", routed(t, issueEvent(p, "comment.created", comment(issue, p+"'s comment")), own, ""), c.want},
			row{"claimant: " + p + "'s claimed issue's event", routed(t, issueEvent(p, "issue.updated", map[string]any{"status": "in_progress"}), own, ""), c.want},
			row{"primary document author: " + p + "'s document's comment", routed(t, documentEvent(p, "comment.created", comment("", "a note")), own, ""), c.want},
			row{"author route: a reply in " + p + "'s thread", routed(t, issueEvent(p, "comment.created", func() model.CommentEventPayload {
				reply := comment(issue, "a reply")
				root := commentID
				reply.ReplyTo = &root
				return reply
			}()), own, ""), c.want},
			row{"follower: " + p + "'s ask answered", routed(t, issueEvent(p, "ask.answered", map[string]any{"id": "ask-1", "answer": "yes"}), own, ""), c.want},
			row{"previous claimant: " + p + "'s issue claimed by another", routed(t, issueEvent(p, "issue.claimed",
				map[string]any{"previous_claim": map[string]any{"kind": "session", "id": payloadSession}}), own, ""), c.want},
			row{"mention, the outbox's copy: " + p + "'s comment", routed(t, issueEvent(p, "comment.created", comment(issue, p+"'s comment")), own, mentionKey), c.want},
			row{"role forward: " + p + "'s issue event on the token's role's lane", routed(t, issueEvent(p, "comment.created", comment(issue, p+"'s comment")), privLane,
				contracts.RoleForwardDedupeKey(contracts.Envelope{DedupeKey: "dispatch-41"}, payloadSession)), c.want},
			row{"mention, the API's send: " + p + "'s comment", sent(t, apiEvent(issue, "comment.created", comment(issue, p+"'s comment")), mention, mentionKey, ""), c.want},
			row{"targeted message: " + p + "'s issue's message", sent(t, apiEvent(issue, "message.created", message(issue, p+"'s message", target)), targeted, "m-1:steer", ""), c.want},
			row{"targeted message for a role, role-marked: " + p + "'s issue's message",
				sent(t, apiEvent(issue, "message.created", message(issue, p+"'s message", target)), targeted, "m-1:steer", payloadPrivRole), c.want},
			row{"the session's own topic and the token's role mark: " + p + "'s issue event", func() []byte {
				var item contracts.Envelope
				if err := json.Unmarshal(routed(t, issueEvent(p, "comment.created", comment(issue, p+"'s comment")), own, ""), &item); err != nil {
					t.Fatal(err)
				}
				item.Role = payloadPrivRole
				return []byte(payloadJSON(t, item))
			}(), c.want},
			// A mention in a comment on a project document, as the API sends it, first and again:
			// the event names the document's project on every send (#4683: the first send's event
			// is the one appended under the document's owner, and a re-send reads that event back
			// with its project, loadCommentCreatedEvent), so the document's project decides it.
			row{"document mention, as the API sends it: " + p + "'s document", sent(t, func() model.Event {
				event := apiEvent("", "comment.created", comment("", "@ses_v please look"))
				artifact := documentID
				event.ArtifactID, event.Project = &artifact, p
				return event
			}(), mention, mentionKey, ""), c.want},
		)
	}
	ownWith := func(payload string) []byte {
		return []byte(payloadJSON(t, contracts.Envelope{EventID: "dispatch-44", Source: "dispatch", Topic: own, DedupeKey: "dispatch-44", Payload: payload}))
	}
	privMessage := sent(t, apiEvent("PRIV-1", "message.created", message("PRIV-1", "PRIV's message", target)), targeted, "m-1:steer", "")
	privMessagePayload := func() string {
		var item contracts.Envelope
		if err := json.Unmarshal(privMessage, &item); err != nil {
			t.Fatal(err)
		}
		return item.Payload
	}()
	privEvent := payloadJSON(t, issueEvent("PRIV", "comment.created", comment("PRIV-1", "PRIV's comment")))
	pubEvent := payloadJSON(t, issueEvent("PUB", "comment.created", comment("PUB-1", "PUB's comment")))
	rows = append(rows,
		row{"a Dispatch payload that names no project and no issue", ownWith(`{"type":"comment.created","actor":{"kind":"user","id":"alice"},"issue_key":null,"payload":{"body":"hi"}}`), unread},
		row{"a Dispatch frame with no payload", ownWith(""), unread},
		row{"a Dispatch payload encoding/json does not read: text after it", ownWith(privEvent + ` x`), unread},
		row{"a Dispatch payload that is no JSON", ownWith(`PUB's comment`), unread},
		row{"a Dispatch payload whose project and issue key disagree", ownWith(strings.Replace(privEvent, `"project":"PRIV"`, `"project":"PUB"`, 1)), unread},
		row{"a Dispatch payload whose issue key is no issue key", ownWith(strings.Replace(privEvent, `"issue_key":"PRIV-1"`, `"issue_key":"pub"`, 1)), unread},
		row{"a targeted Dispatch payload with no event", ownWith(`{"delivery":{"attempt":1,"mode":"steer"}}`), unread},
		row{"a Dispatch payload with a Project key beside project", ownWith(strings.Replace(privEvent, `"project":"PRIV"`, `"project":"PRIV","Project":"PUB"`, 1)), frameDrops(withheldAmbiguousKey)},
		row{"a Dispatch payload with an Issue_Key key beside issue_key", ownWith(strings.Replace(privEvent, `"issue_key":"PRIV-1"`, `"issue_key":"PRIV-1","Issue_Key":"PUB-1"`, 1)), frameDrops(withheldAmbiguousKey)},
		row{"a targeted Dispatch payload with an Event key beside event", withPayload(t, privMessage, strings.Replace(privMessagePayload, `{"event":`, `{"Event":{},"event":`, 1)), frameDrops(withheldAmbiguousKey)},
		row{"a targeted Dispatch payload with a Delivery key beside delivery", withPayload(t, privMessage, strings.Replace(privMessagePayload, `"delivery":`, `"Delivery":{"attempt":1,"mode":"steer"},"delivery":`, 1)), frameDrops(withheldAmbiguousKey)},
		row{"a Dispatch payload with a __proto__ key", ownWith(strings.Replace(privEvent, `"project":"PRIV"`, `"project":"PRIV","__proto__":{"project":"PUB"}`, 1)), frameDrops(withheldPrototypeKey)},
		row{"a frame with a Source key beside source", []byte(`{"source":"agent","Source":"dispatch","topic":"` + own + `"}`), frameDrops(withheldAmbiguousKey)},
		// What the edge reads must be what the plugin shows: a payload the plugin shows no event
		// of (readDispatchPayload) is withheld, so no field the edge reads beside or around the
		// event decides a frame the plugin shows otherwise.
		row{"the token's project beside another project's nested event, which the plugin reads as no event",
			ownWith(`{"event":` + pubEvent + `,"project":"PRIV","issue_key":"PRIV-1"}`), unread},
		row{"a targeted frame with a misspelt delivery line and the token's project beside it",
			withPayload(t, privMessage, `{"delivery":{"attempt":1},"event":`+payloadJSON(t, apiEvent("PUB-1", "message.created", message("PUB-1", "PUB's message", target)))+`,"project":"PRIV"}`), unread},
		row{"a targeted frame of the token's project with a third key beside event and delivery",
			withPayload(t, privMessage, strings.Replace(privMessagePayload, `"delivery":`, `"project":"PUB","delivery":`, 1)), unread},
		row{"an event of the token's project whose payload is no object, which the plugin reads as no event",
			ownWith(strings.Replace(privEvent, `"payload":{`, `"payload":"PUB","x":{`, 1)), unread},
		row{"a targeted message of the token's project carrying a comment's delivery line",
			sent(t, apiEvent("PRIV-1", "message.created", message("PRIV-1", "PRIV's message", target)), mention, "m-1:steer", ""), unread},
		row{"a mention of the token's project whose comment lacks its mentions",
			withPayload(t, sent(t, apiEvent("PRIV-1", "comment.created", comment("PRIV-1", "PRIV's comment")), mention, mentionKey, ""),
				strings.Replace(payloadJSON(t, struct {
					Event    model.Event `json:"event"`
					Delivery any         `json:"delivery"`
				}{apiEvent("PRIV-1", "comment.created", comment("PRIV-1", "PRIV's comment")), mention}), `"mentions":[],`, ``, 1)), unread},
		row{"control: a session's own message whose payload names another project", []byte(payloadJSON(t, contracts.Envelope{
			EventID: "e-1", Source: "agent", Topic: own, DedupeKey: "agent.ses_v.k", Payload: `{"project":"PUB","issue_key":"PUB-1"}`})), framePasses()},
		row{"control: the token's project's document event on the document's topic",
			routed(t, documentEvent("PRIV", "artifact.updated", map[string]any{"version": 2}), "notifications.dispatch.document.PRIV.spec.artifact.updated", ""), framePasses()},
		row{"control: the token's project's issue message", privMessage, framePasses()},
	)
	for _, tc := range rows {
		t.Run(tc.name, func(t *testing.T) {
			verdict := edgeFrameVerdict(nil, payloadSession, projects, own, tc.data)
			if verdict != tc.want {
				t.Fatalf("edgeFrameVerdict(subject=%q, %s) = {action %d, reason %q}, want {action %d, reason %q}",
					own, tc.data, verdict.action, verdict.reason, tc.want.action, tc.want.reason)
			}
		})
	}
}

// TestEdgeFrameVerdictPassesAPersonsMessageInTheSessionsOwnConversation holds the edge's frame
// rule to a message that names no project (dispatchDirectMessage): a person's message to the
// session in its own conversation (POST /api/v1/agents/{session_id}/messages, the dashboard's
// conversation send, a broadcast's copy), which belongs to no issue, passes on the session's own
// agent subject, as Dispatch's API delivers it (finishPendingMessageDelivery). The outbox sends no
// copy of such a message, or of a reply in such a conversation (its batch reads an event with no
// owner alone and finds it no destination). A message aimed at another session or in another
// session's thread, one carrying a key no direct message carries, one on a role's lane, and a
// comment's mention on a project document whose event names no project - the frame the API's
// re-send built before #4683, when loadCommentCreatedEvent read the events row's own project,
// which a document's event leaves empty - are withheld, as frames the edge cannot place in a
// project; the same mention with the document's project on its event, as the API sends it since
// #4683, is judged by that project. A listener that is no edge one hands every such frame on.
func TestEdgeFrameVerdictPassesAPersonsMessageInTheSessionsOwnConversation(t *testing.T) {
	t.Parallel()
	own := contracts.LegacyAgentSubject(payloadSession)
	privLane := contracts.RoleTopicPrefix + payloadPrivRole
	projects := []string{"PRIV"}
	unread := frameDrops(withheldDispatchProjectUnread)
	target := "session:" + payloadSession
	targeted := map[string]any{"attempt": 1, "mode": "steer"}
	mention := map[string]any{"attempt": 1, "mode": "steer", "comment_id": commentID, "target": target}
	mentionKey := contracts.AgentSendDedupeKey(payloadSession, model.CommentMentionIdempotencyKey(commentID, target, "steer"))
	conversation := func(change func(*model.MessageEventPayload)) []byte {
		payload := message("", "Look at the build.", target)
		if change != nil {
			change(&payload)
		}
		return sent(t, apiEvent("", "message.created", payload), targeted, "m-1:steer", "")
	}
	reply := func(payload *model.MessageEventPayload) {
		parent := parentID
		payload.InReplyTo, payload.ReplyBody, payload.ThreadTarget = &parent, "Can you look?", target
	}
	documentMention := func(project string) []byte {
		event := apiEvent("", "comment.created", comment("", "@ses_v please look"))
		artifact := documentID
		event.ArtifactID, event.Project = &artifact, project
		return sent(t, event, mention, mentionKey, "")
	}

	for _, tc := range []struct {
		name    string
		subject string
		data    []byte
		want    frameVerdict
	}{
		{"a person's message in the session's own conversation", own, conversation(nil), framePasses()},
		{"a BTW in the session's own conversation", own, sent(t, apiEvent("", "message.created", message("", "fyi", target)),
			map[string]any{"attempt": 2, "mode": "btw"}, "m-1:btw", ""), framePasses()},
		{"a reply in the session's own conversation", own, conversation(reply), framePasses()},
		{"a broadcast's copy to the session", own, conversation(func(payload *model.MessageEventPayload) {
			id := broadcastID
			payload.BroadcastID = &id
		}), framePasses()},
		{"a message whose body the event cut short", own, conversation(func(payload *model.MessageEventPayload) {
			payload.BodyTruncated = true
		}), framePasses()},
		{"the same message in the session's new-form subject", contracts.AgentSubject("PRIV", payloadSession), conversation(nil), framePasses()},

		{"a message on no issue aimed at another session", own, sent(t, apiEvent("", "message.created", message("", "hi", "session:ses_w")),
			targeted, "m-1:steer", ""), unread},
		{"a reply in another session's thread", own, conversation(func(payload *model.MessageEventPayload) {
			reply(payload)
			payload.ThreadTarget = "session:ses_w"
		}), unread},
		{"a message on no issue with a key no direct message carries", own, conversation(func(payload *model.MessageEventPayload) {
			payload.ReferenceChangesPayload = model.NewReferenceChangesPayload(model.ReferenceChanges{Targets: []model.ChangedReference{{}}})
		}), unread},
		{"a message on no issue with its thread's logins", own, conversation(func(payload *model.MessageEventPayload) {
			payload.ThreadLogins = []string{"alice"}
		}), unread},
		{"a message on no issue on the token's role's lane", privLane, conversation(nil), unread},
		{"a message on no issue on another session's subject", contracts.LegacyAgentSubject("ses_w"), conversation(nil), unread},
		{"a message on no issue naming a document", own, func() []byte {
			event := apiEvent("", "message.created", message("", "hi", target))
			artifact := documentID
			event.ArtifactID = &artifact
			return sent(t, event, targeted, "m-1:steer", "")
		}(), unread},
		{"a message on no issue whose delivery line names a mode and more", own, sent(t, apiEvent("", "message.created", message("", "hi", target)),
			map[string]any{"attempt": 1, "mode": "steer", "target": target}, "m-1:steer", ""), unread},
		{"a message on no issue delivered as a comment's mention", own, sent(t, apiEvent("", "message.created", message("", "hi", target)),
			mention, "m-1:steer", ""), unread},
		{"a comment mention on the token's project's document whose event names no project (the re-send before #4683)", own, documentMention(""), unread},
		{"control: the same mention with the document's project on its event (as sent since #4683)", own, documentMention("PRIV"), framePasses()},
		{"control: the same mention on another project's document", own, documentMention("PUB"), frameDrops(withheldProjectNotHeld)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			verdict := edgeFrameVerdict(nil, payloadSession, projects, tc.subject, tc.data)
			if verdict != tc.want {
				t.Fatalf("edgeFrameVerdict(subject=%q, %s) = {action %d, reason %q}, want {action %d, reason %q}",
					tc.subject, tc.data, verdict.action, verdict.reason, tc.want.action, tc.want.reason)
			}
		})
	}

	// A listener that is no edge one runs no frame rule (sessionRelays.edgeWithholds).
	t.Run("control: a message on no issue aimed at another session, at a listener that is no edge one", func(t *testing.T) {
		msg := bus.SessionMessage{Subject: own, Data: sent(t, apiEvent("", "message.created", message("", "hi", "session:ses_w")), targeted, "m-1:steer", "")}
		if verdict := (&sessionRelays{}).edgeWithholds(payloadSession, msg); verdict != framePasses() {
			t.Fatalf("edgeWithholds at a listener that is no edge one = {action %d, reason %q}, want a pass", verdict.action, verdict.reason)
		}
	})
}

// TestADirectMessagesKeysAreTheOnesDispatchWrites holds directMessageEventKeys and
// directMessageKeys, the keys dispatchDirectMessage lets a direct message carry, to the types
// Dispatch writes the frame from (model.Event, model.MessageEventPayload): every JSON key those
// types can write is on a list, or named here with why a delivery frame never carries it. A field
// added to either type fails this test until it is listed or named, so the rule cannot start
// withholding every direct message, or passing a key it never judged, unseen.
func TestADirectMessagesKeysAreTheOnesDispatchWrites(t *testing.T) {
	t.Parallel()
	excluded := map[string]string{
		"trees":                              "model.Event: filled only for a caller that sent Dispatch-Trees: 1 and on /ws/live frames",
		"references_changed":                 "model.ReferenceChangesPayload: a delivery frame carries model.ReferenceChanges{} (finishPendingMessageDelivery)",
		"references_changed_truncated":       "model.ReferenceChangesPayload: as references_changed",
		"references_changed_truncated_kinds": "model.ReferenceChangesPayload: as references_changed",
		"thread_logins":                      "model.MessageEventPayload: carried by message.created and message.answered events, absent on a delivery frame",
	}
	for _, c := range []struct {
		name    string
		of      reflect.Type
		allowed []string
	}{
		{"the event", reflect.TypeFor[model.Event](), directMessageEventKeys},
		{"the message", reflect.TypeFor[model.MessageEventPayload](), directMessageKeys},
	} {
		keys := jsonKeysOf(c.of)
		if len(keys) < 8 {
			t.Fatalf("%s: read %d JSON keys of %s, too few to be the type's", c.name, len(keys), c.of)
		}
		for _, key := range keys {
			if _, named := excluded[key]; !named && !slices.Contains(c.allowed, key) {
				t.Errorf("%s: %s writes the key %q, which dispatchDirectMessage's list leaves out and this test names no reason for", c.name, c.of, key)
			}
		}
		for _, key := range c.allowed {
			if !slices.Contains(keys, key) {
				t.Errorf("%s: dispatchDirectMessage's list holds %q, which %s does not write", c.name, key, c.of)
			}
		}
	}
}

// jsonKeysOf is every key encoding/json writes for a value of the struct type of, embedded
// structs' keys among them.
func jsonKeysOf(of reflect.Type) []string {
	var keys []string
	for index := range of.NumField() {
		field := of.Field(index)
		tag := field.Tag.Get("json")
		name, _, _ := strings.Cut(tag, ",")
		switch {
		case name == "-":
		case field.Anonymous && name == "" && field.Type.Kind() == reflect.Struct:
			keys = append(keys, jsonKeysOf(field.Type)...)
		case !field.IsExported():
		case name == "":
			keys = append(keys, field.Name)
		default:
			keys = append(keys, name)
		}
	}
	return keys
}
