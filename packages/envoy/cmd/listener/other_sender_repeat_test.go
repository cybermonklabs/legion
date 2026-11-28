// What a caller learns by repeating another sender's key: that the key is taken, or, for a client
// that does not read that answer, its own request's envelope, and never what the other sender
// wrote.

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/ciphertoken"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/id"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/roles"
)

// TestARepeatByAnotherSenderLearnsTheKeyIsTakenAndNotTheText is check c-other-sender-repeat.
// Session A publishes to a team topic, sends to a live session, publishes to a role whose holder
// is live, and publishes to a role nobody claims, each under its own key K and with words of its
// own. Session B then sends the same request under K with words of its own, naming its own
// source_session and the key-taken answer as one it reads (listenerwire.ReadsHeader): once alone,
// and once with a secrets-broker token that proves B as well. Each answer is the route's status
// (200, or 202 kept) with duplicate and the stored copy's event_id and times, and carries no
// payload, payload_summary, payload_ref, source, source_session or sender, and none of A's words;
// the request's record line names the copy's event_id and B, with B's own words. B naming no
// header (Dispatch's client, the Legion daemon and every client release before the header) gets
// the envelope its request built, with duplicate, which every client parses, and none of A's
// words.
//
// A copy that names no session has no owner a caller can prove: a caller with no session, with or
// without a token, that repeats it learns only that the key is taken, or gets its own envelope
// without the header. A role publish that names no session to a role nobody claims keeps no copy at
// all, so on that route the first publish and the repeat are each refused 404 (roleSend.noHolder).
//
// The controls: A's own repeat under K gets the whole stored copy, and a request with a token and
// no session to a role nobody claims is refused 404 with nothing kept, whoever its token proves.
func TestARepeatByAnotherSenderLearnsTheKeyIsTakenAndNotTheText(t *testing.T) {
	t.Parallel()
	rig := newSenderLimitRig(t)
	const alice, bob = "ses_other_sender_alice", "ses_other_sender_bob"

	type route struct {
		name string
		// path and fields are the request's route and the body fields that name where it goes.
		path   string
		fields map[string]any
		// status is the route's answer to a request it stored: 200, or 202 for a role nobody
		// claims, whose copy the role's kept lane holds.
		status int
		// stored is the one copy the stream holds for the request, as the route answers it.
		stored func(t *testing.T) contracts.Envelope
		// keptRole is the role whose kept lane holds the copy of a route that keeps one, and empty
		// for every other route.
		keptRole string
	}
	// routes is the four routes, each to a topic, a target or a role of its own.
	routes := func(t *testing.T) []route {
		topic := "notifications.team.other-sender-" + id.New()
		target := "ses_other_sender_target_" + id.New()
		rig.live(t, target, nil)
		holder := "ses_other_sender_holder_" + id.New()
		role := "other-sender-" + id.New()
		claimRoleFor(t, rig.deps.registry, rig.deps.sessions, holder, role, nil)
		unclaimed := "other-sender-unclaimed-" + id.New()
		return []route{
			{"topic publish", "/v1/messages/publish", map[string]any{"topic": topic}, http.StatusOK, func(t *testing.T) contracts.Envelope {
				return heldCopy(t, rig.harness.client, topic)
			}, ""},
			{"send", "/v1/messages/send", map[string]any{"target_session": target}, http.StatusOK, func(t *testing.T) contracts.Envelope {
				return heldCopy(t, rig.harness.client, contracts.LegacyAgentSubject(target))
			}, ""},
			{"role publish", "/v1/messages/publish", map[string]any{"topic": contracts.RoleTopicPrefix + role}, http.StatusOK, func(t *testing.T) contracts.Envelope {
				return heldForward(t, rig.harness.client, holder)
			}, ""},
			{"role publish kept", "/v1/messages/publish", map[string]any{"topic": contracts.RoleTopicPrefix + unclaimed}, http.StatusAccepted, func(t *testing.T) contracts.Envelope {
				return heldCopy(t, rig.harness.client, bus.KeptRoleSubject(unclaimed))
			}, unclaimed},
		}
	}
	// caller is who a request comes from: the session it names (none when empty), the session a
	// broker token it presents proves (no token when empty), and whether it names the key-taken
	// answer as one it reads (readingKeyTaken).
	type caller struct {
		session, token string
		reads          bool
	}
	// request is r's request under key from from, with words, asking for reply where it is not
	// empty.
	request := func(t *testing.T, r route, key string, from caller, words, reply string) heldAnswer {
		t.Helper()
		body := map[string]any{"message": words, "idempotency_key": key}
		for field, value := range r.fields {
			body[field] = value
		}
		if from.session != "" {
			body["source_session"] = from.session
		}
		if reply != "" {
			body["expects_reply"] = reply
		}
		req := postJSON(r.path, messageJSON(t, body, nil))
		if from.token != "" {
			req = req.WithContext(withBrokerCaller(req.Context(), ciphertoken.Caller{Session: from.token}))
		}
		if from.reads {
			req = readingKeyTaken(req)
		}
		return held(t, serveLimited(t, rig.mux, rig.harness.logs.String, req))
	}
	// learnsOnlyTheKey fails unless answer is r's status with duplicate, kept's event_id, times and
	// expects_reply (the copy's own, A's "optional", whatever B asked for), and none of what kept's
	// sender wrote or who it is, and unless the request's record line names the copy's event_id and
	// who asked: session, and the asker's own words.
	learnsOnlyTheKey := func(t *testing.T, what string, r route, answer heldAnswer, kept contracts.Envelope, words, session, asked string) {
		t.Helper()
		answer.requireKeyTaken(t, what, r.status, kept, words)
		var fields map[string]json.RawMessage
		if err := json.Unmarshal([]byte(answer.raw), &fields); err != nil || string(fields["expects_reply"]) != `"optional"` {
			t.Fatalf("%s answered expects_reply %s, want the stored copy's \"optional\"; body %s", what, fields["expects_reply"], answer.raw)
		}
		if answer.line["event_id"] != kept.EventID || answer.line["source_session"] != session || answer.line["payload_summary"] != asked {
			t.Fatalf("%s wrote the line %v, want the copy's event_id %s, the asker's session %q and its own words %q", what, answer.line, kept.EventID, session, asked)
		}
	}

	t.Run("control: the same sender's repeat gets the whole stored copy", func(t *testing.T) {
		for _, r := range routes(t) {
			t.Run(r.name, func(t *testing.T) {
				key := uuid.NewString()
				words := "alice's own words " + id.New()
				request(t, r, key, caller{session: alice}, words, "").requireStatus(t, "A's "+r.name, r.status)
				kept := r.stored(t)
				if !strings.Contains(kept.PayloadSummary+kept.Payload, words) {
					t.Fatalf("the stream holds %+v for A's %s, want A's words in it", kept, r.name)
				}
				repeat := request(t, r, key, caller{session: alice}, "alice again", "")
				if r.status == http.StatusOK {
					repeat.requireHeld(t, "A's repeated "+r.name, kept, true)
					return
				}
				repeat.requireStatus(t, "A's repeated "+r.name, r.status)
				if !reflect.DeepEqual(repeat.envelope, kept) || !repeat.Duplicate {
					t.Fatalf("A's repeated %s answered %s, want the whole kept copy %+v with duplicate", r.name, repeat.raw, kept)
				}
			})
		}
	})

	// Each row: A, naming first (none when empty), stores a copy; B, as second, repeats its key.
	// A client that reads the key-taken answer learns only that the key is taken; any other gets
	// the envelope its own request built.
	for _, row := range []struct {
		name   string
		first  string
		second caller
	}{
		{"case: B naming its own session learns the key is taken and not A's text", alice, caller{session: bob, reads: true}},
		{"case: B with a token that proves B learns the key is taken and not A's text", alice, caller{session: bob, token: bob, reads: true}},
		{"case: B naming its own session, whose client does not read the key-taken answer, gets its own envelope and not A's text", alice, caller{session: bob}},
		{"case: a caller with no session repeating a copy that names no session learns the key is taken and not its text", "", caller{reads: true}},
		{"case: a caller with no session and a token that proves B repeating a copy that names no session learns the key is taken and not its text", "", caller{token: bob, reads: true}},
		{"case: a caller with no session, whose client does not read the key-taken answer, repeating a copy that names no session gets its own envelope and not its text", "", caller{}},
	} {
		t.Run(row.name, func(t *testing.T) {
			for _, r := range routes(t) {
				t.Run(r.name, func(t *testing.T) {
					key := uuid.NewString()
					words := "the first sender's own words " + id.New()
					if row.first == "" && r.keptRole != "" {
						// A role publish that names no session is kept for nobody: no dead letter
						// could reach its sender (roleSend.noHolder). So the first and the repeat
						// are each refused 404, and the lane holds no copy for the repeat to read.
						request(t, r, key, caller{}, words, "optional").requireStatus(t, "the first "+r.name, http.StatusNotFound)
						request(t, r, key, row.second, "the second sender's words", "required").requireStatus(t, "the repeat of the first "+r.name, http.StatusNotFound)
						assertNothingKept(t, rig.harness.client, r.keptRole)
						return
					}
					request(t, r, key, caller{session: row.first}, words, "optional").requireStatus(t, "the first "+r.name, r.status)
					kept := r.stored(t)
					answer := request(t, r, key, row.second, "the second sender's words", "required")
					what := "the repeat of the first " + r.name
					if !row.second.reads {
						t.Logf("%s, by a client that does not read the key-taken answer, answered %s", what, answer.raw)
						answer.requireOwnEnvelope(t, what, r.status, kept, words, row.second.session, "the second sender's words")
						return
					}
					learnsOnlyTheKey(t, what, r, answer, kept, words, row.second.session, "the second sender's words")
				})
			}
		})
	}

	// A request with a token and no session names no session a dead letter could reach, whoever its
	// token proves, so a role publish to a role nobody holds keeps nothing for it and is refused 404
	// (roleSend.noHolder).
	t.Run("control: a request with a token and no session is refused, and nothing is kept for it", func(t *testing.T) {
		r := routes(t)[3]
		first := request(t, r, uuid.NewString(), caller{token: bob, reads: true}, "my own words", "")
		first.requireStatus(t, "the tokened first publish to the unclaimed role", http.StatusNotFound)
		assertNothingKept(t, rig.harness.client, r.keptRole)
	})
}

// A keep that answers its own resend or its own in-process refusal with the copy the role's kept
// lane holds reads it as no repeat (keptRoleCopy.repeat false), and that copy can still be another
// sender's: the 202 then says duplicate, with the copy's ids and times alone for a client that
// reads the key-taken answer, as it does for a repeat. The control is the keep's own copy,
// answered whole with no duplicate.
func TestAKeptAnswerOfAnotherSendersCopyIsDuplicateOutsideARepeat(t *testing.T) {
	t.Parallel()
	rig := newSenderLimitRig(t)
	topic := contracts.RoleTopicPrefix + "kept-outside-" + id.New()
	key := bus.PublishDedupePrefix + uuid.NewString()
	aliceCopy := contracts.MessageEnvelope(contracts.Message{SourceSession: "ses_kept_outside_alice", Text: "alice's kept words"}, topic, key)
	bobItem := contracts.MessageEnvelope(contracts.Message{SourceSession: "ses_kept_outside_bob", Text: "bob's words"}, topic, key)
	// keep is the 202 a send of item answers when the keep, reading no repeat, hands it kept.
	keep := func(t *testing.T, item, kept contracts.Envelope) heldAnswer {
		t.Helper()
		var logs strings.Builder
		recorder := httptest.NewRecorder()
		send := &roleSend{
			w:      recorder,
			record: &requestRecord{logger: logging.NewWithWriter("test", &logs), line: "listener publish", laneKey: "topic", lane: topic, item: &item},
			d:      rig.deps,
			ctx:    t.Context(),
			item:   item,
			sender: requestSender{eventID: item.EventID, source: item.Source, session: item.SourceSession, readsKeyTaken: true},
		}
		send.kept(roles.Resolution{State: roles.Unclaimed}, keptRoleCopy{item: kept, until: 1, repeat: false})
		answer := limitedAnswer{status: recorder.Code, raw: recorder.Body.String()}
		if err := json.Unmarshal(recorder.Body.Bytes(), &answer); err != nil {
			t.Fatalf("decode the kept answer %s: %v", answer.raw, err)
		}
		answer.requireStatus(t, "the kept answer", http.StatusAccepted)
		return held(t, answer)
	}

	t.Run("case: another sender's kept copy, read as no repeat, is answered with duplicate and its ids alone", func(t *testing.T) {
		answer := keep(t, bobItem, aliceCopy)
		got := answer.envelope
		if !answer.Duplicate || got.EventID != aliceCopy.EventID || got.PayloadSummary != "" || got.SourceSession != "" {
			t.Fatalf("the kept answer of Alice's copy to Bob is %s, want duplicate and Alice's event_id with no text or session", answer.raw)
		}
	})

	t.Run("control: the keep's own copy is answered whole with no duplicate", func(t *testing.T) {
		answer := keep(t, bobItem, bobItem)
		if answer.Duplicate || !reflect.DeepEqual(answer.envelope, bobItem) {
			t.Fatalf("the kept answer of Bob's own copy is %s, want it whole with no duplicate", answer.raw)
		}
	})
}
