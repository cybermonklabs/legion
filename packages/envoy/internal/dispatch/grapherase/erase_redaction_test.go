package grapherase

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/graphcensus"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// The two comments this file erases, each with the session its mention reached. The first's body
// is the state its receipt records the attempt in. The second's body is part of its own id, which
// the database is given rather than draws, and of the id of the session it reached.
const (
	sentBody        = model.DeliveryStateSent
	sentReached     = "ses_redaction_reached"
	partOfIDBody    = "4000-8000"
	partOfIDID      = "00000000-0000-4000-8000-000000000b0d"
	partOfIDReached = "ses_4000-8000_reached"
)

// heldEvent is one events row as it stood: its type, its time and its actor as the row holds them,
// and its payload decoded.
type heldEvent struct {
	kind, at, actor string
	payload         map[string]any
}

// readHeldEvents reads the events rows whose ids are ids, by id.
func readHeldEvents(t *testing.T, database *store.Store, ids []int64) map[int64]heldEvent {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `
		select id, type, created_at::text, actor::text, payload::text from events where id = any($1) order by id`, ids)
	if err != nil {
		t.Fatalf("read the events %v: %v", ids, err)
	}
	defer rows.Close()
	held := map[int64]heldEvent{}
	for rows.Next() {
		var id int64
		var event heldEvent
		var payload string
		if err := rows.Scan(&id, &event.kind, &event.at, &event.actor, &payload); err != nil {
			t.Fatalf("read an event of %v: %v", ids, err)
		}
		decoder := json.NewDecoder(bytes.NewReader([]byte(payload)))
		decoder.UseNumber()
		if err := decoder.Decode(&event.payload); err != nil {
			t.Fatalf("decode the payload of event %d: %v", id, err)
		}
		held[id] = event
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the events %v: %v", ids, err)
	}
	return held
}

// eventsAboutComment answers the ids of the events about comment that the erase reads: its own
// events and its delivery receipts.
func eventsAboutComment(t *testing.T, database *store.Store, comment string) []int64 {
	t.Helper()
	var ids []int64
	if err := database.Pool.QueryRow(context.Background(), `
		select coalesce(array_agg(id order by id), '{}') from events
		where payload->>'id' = $1 or payload->>'comment_id' = $1`, comment).Scan(&ids); err != nil {
		t.Fatalf("read the events about comment %s: %v", comment, err)
	}
	if len(ids) < 2 {
		t.Fatalf("comment %s has %d event(s), want its comment.created and its receipt", comment, len(ids))
	}
	return ids
}

// R5: the erase takes out of an event what a person wrote and leaves the event's structure as it
// was. Two comments are erased, each with the session its mention reached: one whose body is
// `sent`, the state its own receipt records the attempt in, and one whose body is part of its own
// id and of that session's id. After each erase the body of the comment's event holds the marker
// and nothing else of either event moved: the receipt's state and session id, the target and the
// comment it names, the event's id, its times, its actor and its type are what they were. The
// listing names as redacted exactly the events the erase changed, and lists each it leaves whole
// holding the text, so the confirmed run did what its listing said. The store census reads zero
// afterwards, which it can only where each receipt still says which session the listener took the
// attempt to.
func TestTheEraseRedactsWhatAPersonWroteAndLeavesTheEventsStructure(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	seedAgreeingStore(t, database)
	alice := model.Actor{Kind: "user", ID: "alice"}
	mention := func(session string) model.Mention {
		return model.Mention{Target: "session:" + session, Delivery: "steer", SessionID: new(session)}
	}
	sent, _ := writeIssueComment(t, database, "", alice, sentBody, mention(sentReached))
	settleMention(t, database, sent, alice, sentReached, model.DeliveryStateSent)
	partOfID, _ := writeIssueComment(t, database, partOfIDID, alice, partOfIDBody, mention(partOfIDReached))
	settleMention(t, database, partOfID, alice, partOfIDReached, model.DeliveryStateSent)
	if _, err := graphcensus.Backfill(ctx, database, io.Discard); err != nil {
		t.Fatalf("backfill the comments: %v", err)
	}
	requireCensusZero(t, database, "before the erases")

	for _, erased := range []struct{ comment, body, session string }{
		{sent, sentBody, sentReached},
		{partOfID, partOfIDBody, partOfIDReached},
	} {
		ids := eventsAboutComment(t, database, erased.comment)
		before := readHeldEvents(t, database, ids)
		listing := dryRun(t, database, commentNodeKind, erased.comment)
		erasure, err := Erase(ctx, database.Pool.Config().ConnString(), commentNodeKind, erased.comment, listing.Digest, time.Millisecond)
		if err != nil {
			t.Fatalf("erase --confirm %s: %v", listing.Digest, err)
		}
		requireTheCommentErased(t, database, erasure, erased.comment)
		after := readHeldEvents(t, database, ids)
		marker := erasure.marker()
		for _, id := range slices.Sorted(maps.Keys(before)) {
			was, now := before[id], after[id]
			if now.kind != was.kind || now.at != was.at || now.actor != was.actor {
				t.Errorf("event %d reads type %q at %s by %s after the erase of a comment reading %q, want type %q at %s by %s as before",
					id, now.kind, now.at, now.actor, erased.body, was.kind, was.at, was.actor)
			}
			if was.kind == "comment.delivery" &&
				(now.payload["state"] != model.DeliveryStateSent || now.payload["session_id"] != erased.session) {
				t.Errorf("the receipt %d reads state %v and session_id %v after the erase of a comment reading %q, want %q and %q",
					id, now.payload["state"], now.payload["session_id"], erased.body, model.DeliveryStateSent, erased.session)
			}
			want := maps.Clone(was.payload)
			if body, held := want["body"].(string); held {
				want["body"] = strings.ReplaceAll(body, erased.body, marker)
			}
			if !reflect.DeepEqual(now.payload, want) {
				t.Errorf("event %d %s reads %v after the erase of a comment reading %q, want %v: only what the comment's author wrote holds the marker",
					id, was.kind, now.payload, erased.body, want)
			}
			redacted := !reflect.DeepEqual(now.payload, was.payload)
			if named := listed(listing, fmt.Sprintf("events %d %s", id, was.kind)); named != redacted {
				t.Errorf("the listing names event %d %s as redacted %t, and the erase changed it %t:\n%s",
					id, was.kind, named, redacted, strings.Join(listing.Listing, "\n"))
			}
			written, err := json.Marshal(now.payload)
			if err != nil {
				t.Fatalf("encode the payload of event %d: %v", id, err)
			}
			left := listed(listing, "leave public.events", fmt.Sprintf("(id)=(%d)", id), "payload")
			if holds := bytes.Contains(written, []byte(erased.body)); !redacted && holds && !left {
				t.Errorf("the erase left event %d %s holding %q, and its listing does not name it as a row it leaves:\n%s",
					id, was.kind, erased.body, strings.Join(listing.Listing, "\n"))
			}
		}
		if body, _ := after[ids[0]].payload["body"].(string); body != marker {
			t.Errorf("the comment.created event %d reads the body %q after the erase, want the marker %q", ids[0], body, marker)
		}
	}
	requireCensusZero(t, database, "after the erases")
}

// R5: a comment's own event carries, beside what its author wrote, values that quote other nodes:
// the quote of the document the comment is anchored to, that document's name, the question of the
// ask it replies to and the text of the block its reply answers. Those are the other nodes' words.
// The comment erased reads `ship it on Friday`, and each of those values holds the same words
// inside a longer sentence. After the erase the event's body holds the marker and every one of
// those values reads as it was written, and the listing names the event as one the run redacts and
// as a row it leaves holding the text, so the confirmed run did what its listing said.
func TestTheEraseLeavesTheTextOfOtherNodesInACommentsEvent(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	seeded := seedAgreeingStore(t, database)
	const body = "ship it on Friday"
	quoting := []struct{ object, key, text string }{
		{"anchor", "quote", "We agreed to ship it on Friday if the tests pass."},
		{"", "artifact_name", "Why we ship it on Friday.md"},
		{"", "ask_question", "Should we ship it on Friday or wait a week?"},
		{"reply_block", "text", "I would ship it on Friday, then watch the error rate."},
	}
	if _, err := database.Pool.Exec(ctx, `
		update comments set body = $2, anchor = anchor || jsonb_build_object('quote', $3::text) where id = $1::uuid`,
		seeded.comment, body, quoting[0].text); err != nil {
		t.Fatalf("write the comment's body and its anchor's quote: %v", err)
	}
	payload := map[string]any{
		"id": seeded.comment, "body": body, "author": map[string]any{"kind": "user", "id": "alice"},
		"anchor":      map[string]any{"artifact_id": seeded.document.artifactID},
		"reply_block": map[string]any{"node": "comment/00000000-0000-4000-8000-0000000000c1#b2", "block_id": "b2"},
	}
	for _, quoted := range quoting {
		if quoted.object == "" {
			payload[quoted.key] = quoted.text
		} else {
			payload[quoted.object].(map[string]any)[quoted.key] = quoted.text
		}
	}
	written, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("encode the comment's event: %v", err)
	}
	event := appendEvent(t, database, "CORE-1", "comment.created", string(written))

	listing := dryRun(t, database, commentNodeKind, seeded.comment)
	erasure, err := Erase(ctx, database.Pool.Config().ConnString(), commentNodeKind, seeded.comment, listing.Digest, time.Millisecond)
	if err != nil {
		t.Fatalf("erase --confirm %s: %v", listing.Digest, err)
	}
	requireTheCommentErased(t, database, erasure, seeded.comment)
	after, raw := eventPayload(t, database, event)
	if after["body"] != erasure.marker() {
		t.Errorf("the comment's event reads the body %q after the erase, want the marker %q: the body is what the comment's author wrote: %s",
			after["body"], erasure.marker(), raw)
	}
	for _, quoted := range quoting {
		name, value := quoted.key, after[quoted.key]
		if quoted.object != "" {
			held, _ := after[quoted.object].(map[string]any)
			name, value = quoted.object+"."+quoted.key, held[quoted.key]
		}
		if value != quoted.text {
			t.Errorf("the comment's event reads %s %q after the erase, want it as written, %q: it quotes another node, and the erased words inside a longer line of it are that node's: %s",
				name, value, quoted.text, raw)
		}
	}
	for _, line := range [][]string{
		{fmt.Sprintf("events %d comment.created", event)},
		{"leave public.events", fmt.Sprintf("(id)=(%d)", event), "[comment.created]", "payload"},
	} {
		if !listed(listing, line...) {
			t.Errorf("the listing holds no line with %q, so it does not say the run redacts the event and leaves the quotes in it holding the text:\n%s",
				line, strings.Join(listing.Listing, "\n"))
		}
	}
}
