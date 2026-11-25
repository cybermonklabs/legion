package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// A stored event's payload is the record its write made, and it names other rows' objects: the
// issues an issue waits on (blocked_by), the nodes a write's text moved (references_changed), the
// sessions a comment mentions and its deliveries reached, the session or role a message was sent
// to or an issue is routed to, the session or person holding an issue's claim, the person an issue
// is assigned to or an ask waits on. The rule of references (references_seen.go)
// holds for each of them: a reader is answered a payload with every reference to an object it does
// not see left out, as the payload would read had that object never been named. An event whose
// one subject is such an object - a session's follow of an ask, its subscription, a delivery to
// it - is not the reader's to read at all, which the event log's row rule says (eventReadableSQL),
// so a page of the log is never shortened after its limit.
//
// The fields are named by their JSON keys, which every payload that carries one spells alike
// (model's payload types embed Issue, Ask, Comment, Message and ReferenceChangesPayload), so one
// walk reads every event type: payloadReferenceKeys is the census of them, and
// TestNoAnswerNamesAnObjectItsReaderCannotSee holds the stored payloads of its seed to it.

// payloadReferenceKeys is every top-level payload key that names another row's object, with what it
// names and how a reference the reader does not see is left out:
//
//   - blocked_by: the issues an issue waits on, a list of keys; each hidden key is dropped.
//   - references_changed: the nodes a write's text moved, a list of {kind, id, issue_key,
//     artifact_id, ref}; each entry naming a hidden node is dropped (changedReferenceOwner).
//   - assignee, asked_of, suppressed_route_session_id: one person by login, or one session by id; a
//     hidden one is null.
//   - route, target, thread_target, suppressed_route: one route, `session:<id>` or `role:<name>`
//     (payloadStringReference); a route to a hidden session, or to a role of a private project the
//     reader does not see (routeRoleProject), is null.
//   - claim, previous_claim: the claim an issue's write left and the one a claim write replaced,
//     each {actor, at}, held by a session or a person (actorReference); a claim whose holder is
//     hidden is null, and a hidden previous_claim is left out, as a write that replaced no claim
//     records none.
//   - thread_logins: persons by login; each hidden one is dropped.
//   - mentions, deliveries: a comment's or a message's targets, each {target, session_id,
//     resolved_session_id}; each entry naming a hidden session, or a role of a hidden project, is
//     dropped.
var payloadReferenceKeys = map[string]struct{}{
	"blocked_by":                  {},
	model.ReferencesChangedKey:    {},
	"assignee":                    {},
	"asked_of":                    {},
	"thread_logins":               {},
	"route":                       {},
	"target":                      {},
	"thread_target":               {},
	"suppressed_route":            {},
	"suppressed_route_session_id": {},
	"claim":                       {},
	"previous_claim":              {},
	"mentions":                    {},
	"deliveries":                  {},
}

// routeSession is the session a `session:<id>` route names, "" for a route of any other kind.
func routeSession(route string) string {
	session, ok := strings.CutPrefix(route, "session:")
	if !ok {
		return ""
	}
	return session
}

// changedReferenceOwner is the object a references_changed entry is read through: an issue by
// key, a document by uuid (its own, or the one a block or an ask's row list is of), or an ask's
// issue. An entry naming none of them answers ok false and is left out for every reader who does
// not see every object, as an object of no known owner.
func changedReferenceOwner(entry map[string]any) (referenceKind, string, bool) {
	text := func(key string) string {
		value, _ := entry[key].(string)
		return value
	}
	switch kind, id := text("kind"), text("id"); {
	case kind == "issue":
		return issueReference, id, id != ""
	case kind == "artifact":
		return documentReference, id, id != ""
	case text("issue_key") != "":
		return issueReference, text("issue_key"), true
	case text("artifact_id") != "":
		return documentReference, text("artifact_id"), true
	}
	return "", "", false
}

// nameEntryReferences names what one mention or delivery entry names: the sessions it resolved to
// or reached, and the session or the role's project its target names (payloadStringReference).
func nameEntryReferences(entry map[string]any, named references) {
	for _, key := range []string{"session_id", "resolved_session_id"} {
		if session, _ := entry[key].(string); session != "" {
			named.name(sessionReference, session)
		}
	}
	if target, _ := entry["target"].(string); target != "" {
		named.name(payloadStringReference("target", target))
	}
}

// entryReferencesSeen reports whether seen holds everything one mention or delivery entry names.
func entryReferencesSeen(entry map[string]any, seen references) bool {
	for _, key := range []string{"session_id", "resolved_session_id"} {
		if session, _ := entry[key].(string); session != "" && !seen.holds(sessionReference, session) {
			return false
		}
	}
	target, _ := entry["target"].(string)
	kind, id := payloadStringReference("target", target)
	return id == "" || seen.holds(kind, id)
}

// payloadStringReference is the object a payload key's string value names: a person by login
// under assignee and asked_of, a session by id under suppressed_route_session_id, and under every
// other key a route - the session a `session:<id>` route names, or the project the role a
// `role:<name>` route names works for (routeRoleProject). An id of "" names nothing.
func payloadStringReference(key, value string) (referenceKind, string) {
	switch key {
	case "assignee", "asked_of":
		return personReference, value
	case "suppressed_route_session_id":
		return sessionReference, value
	}
	if session := routeSession(value); session != "" {
		return sessionReference, session
	}
	return roleProjectReference, routeRoleProject(value)
}

// payloadClaimReference is the holder a claim value {actor, at} names (actorReference), and an id
// of "" for a value that names none.
func payloadClaimReference(claim map[string]any) (referenceKind, string) {
	actor, _ := claim["actor"].(map[string]any)
	kind, _ := actor["kind"].(string)
	id, _ := actor["id"].(string)
	return actorReference(model.Actor{Kind: kind, ID: id})
}

// namePayloadReferences adds every object payload names under payloadReferenceKeys to named.
func namePayloadReferences(payload map[string]any, named references) {
	for key := range payloadReferenceKeys {
		switch value := payload[key].(type) {
		case string:
			named.name(payloadStringReference(key, value))
		case map[string]any:
			if key == "claim" || key == "previous_claim" {
				if kind, id := payloadClaimReference(value); kind != "" {
					named.name(kind, id)
				}
			}
		case []any:
			for _, element := range value {
				switch element := element.(type) {
				case string:
					if key == "blocked_by" {
						named.name(issueReference, element)
					} else if key == "thread_logins" {
						named.name(personReference, element)
					}
				case map[string]any:
					if key == model.ReferencesChangedKey {
						if kind, id, ok := changedReferenceOwner(element); ok {
							named.name(kind, id)
						}
					} else {
						nameEntryReferences(element, named)
					}
				}
			}
		}
	}
}

// leavePayloadReferencesOut rewrites payload with every reference seen does not hold left out,
// and reports whether it left any out.
func leavePayloadReferencesOut(payload map[string]any, seen references) bool {
	changed := false
	for key := range payloadReferenceKeys {
		switch value := payload[key].(type) {
		case string:
			if kind, id := payloadStringReference(key, value); id != "" && !seen.holds(kind, id) {
				payload[key] = nil
				changed = true
			}
		case map[string]any:
			if key != "claim" && key != "previous_claim" {
				continue
			}
			if kind, id := payloadClaimReference(value); kind != "" && id != "" && !seen.holds(kind, id) {
				if key == "previous_claim" {
					delete(payload, key)
				} else {
					payload[key] = nil
				}
				changed = true
			}
		case []any:
			kept := make([]any, 0, len(value))
			for _, element := range value {
				keep := true
				switch element := element.(type) {
				case string:
					if key == "blocked_by" {
						keep = seen.holds(issueReference, element)
					} else if key == "thread_logins" {
						keep = seen.holds(personReference, element)
					}
				case map[string]any:
					if key == model.ReferencesChangedKey {
						kind, id, ok := changedReferenceOwner(element)
						keep = ok && seen.holds(kind, id)
					} else {
						keep = entryReferencesSeen(element, seen)
					}
				}
				if keep {
					kept = append(kept, element)
				}
			}
			if len(kept) != len(value) {
				payload[key] = kept
				changed = true
			}
		}
	}
	return changed
}

// leaveOutHiddenReferences answers events as who reads them: each payload with every reference to
// an object who does not see left out (payloadReferenceKeys), and every other payload as stored,
// byte for byte. A reader that sees every object (seesEveryObject) is answered every payload as
// stored with no read; any other is answered after one read of what it sees (seenReferences).
func (s *server) leaveOutHiddenReferences(ctx context.Context, q queryer, who model.Actor, events []model.Event) error {
	if seesEveryObject(who) || len(events) == 0 {
		return nil
	}
	payloads := make([]map[string]any, len(events))
	named := references{}
	for index := range events {
		raw, ok := events[index].Payload.(json.RawMessage)
		if !ok {
			return fmt.Errorf("read the references of a %s payload: expected raw JSON", events[index].Type)
		}
		// Numbers are decoded as they are written, so a payload re-encoded with a reference left
		// out carries every number of the stored one exactly.
		var payload map[string]any
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		if err := decoder.Decode(&payload); err != nil || payload == nil {
			continue
		}
		payloads[index] = payload
		namePayloadReferences(payload, named)
	}
	seen, err := seenReferences(ctx, q, who, named)
	if err != nil {
		return err
	}
	for index, payload := range payloads {
		if payload == nil || !leavePayloadReferencesOut(payload, seen) {
			continue
		}
		encoded, err := json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("encode a %s payload: %w", events[index].Type, err)
		}
		events[index].Payload = json.RawMessage(encoded)
	}
	return nil
}
