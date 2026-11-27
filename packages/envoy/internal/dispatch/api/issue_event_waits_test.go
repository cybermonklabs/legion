package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/outbox"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/testwait"
)

// An issue event answers the issue's waits under both spellings through this release, blocked_by
// beside needs (R10 of the issue-tree spec), and two builds serve one database through a roll and
// after a rollback. So the log holds issue events of builds that write one spelling alone: the
// release before wrote blocked_by alone, since its model.Issue had no needs, and the release that
// deletes blocked_by (dispatch://INFRA-1098) writes needs alone. Every read that hands a stored
// event on answers such a payload with both members, the same targets under each, and every other
// member as stored (model.EventPayloadSQL): the issue's event feed under each of its pagings, its
// kept bytes and its ETag, the whole event log, the live socket's relay and its replay, and the
// outbox's envelopes. A payload of this build, which holds both, an issue payload that holds
// neither, and a payload of another type that holds a member named blocked_by read as stored.
func TestEveryReadOfAnIssueEventAnswersItsWaitsUnderBothSpellings(t *testing.T) {
	t.Parallel()
	rig := newLiveSocketRig(t, testServerOptions{}, nil)
	handler, database := rig.handler, rig.database
	ctx := context.Background()
	createTestProject(t, handler, "TEST")
	create := func(title string) model.Issue {
		t.Helper()
		response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{
			"project": "TEST", "title": title, "force": true,
		}, "alice")
		if response.Code != http.StatusCreated {
			t.Fatalf("create %q: status=%d body=%s", title, response.Code, response.Body.String())
		}
		return decodeBody[model.Issue](t, response)
	}
	blocker := create("The issue waited on")
	waiting := create("The issue that waits")
	if response := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+waiting.Key, map[string]any{
		"needs": []string{blocker.Key},
	}, "alice"); response.Code != http.StatusOK {
		t.Fatalf("make %s wait on %s: status=%d body=%s", waiting.Key, blocker.Key, response.Code, response.Body.String())
	}
	// This build's write of the wait holds both members, each naming the blocker, and its changed
	// list names both; the two payloads of one spelling are that write as the other builds wrote it.
	written := newestEventPayload[map[string]any](t, database, "issue.updated", waiting.Key)
	if wait := []any{blocker.Key}; !reflect.DeepEqual(written["needs"], wait) || !reflect.DeepEqual(written["blocked_by"], wait) {
		t.Fatalf("this build's issue.updated of the wait = %v, want needs and blocked_by each %v", written, wait)
	}

	// The relay's frames are the events committed after its subscription started.
	live := rig.dial(t, withBearer("agent-token"))
	head := subscribeEvents(t, live, "live", nil)

	appended := []struct {
		name, eventType string
		payload         map[string]any
		// holds is the one member of the two the payload holds, and lacks the one every read
		// answers beside it; both are "" for a payload every read answers as stored.
		holds, lacks string
	}{
		{"the release before's issue.updated, holding blocked_by alone", "issue.updated", withoutMember(written, "needs"), "blocked_by", "needs"},
		{"a later release's issue.updated, holding needs alone", "issue.updated", withoutMember(written, "blocked_by"), "needs", "blocked_by"},
		{"this build's issue.updated, holding both", "issue.updated", written, "", ""},
		{"an issue.claimed, holding neither", "issue.claimed", map[string]any{
			"key": waiting.Key, "status": waiting.Status, "repo": nil, "resolved_repo": nil, "claim": nil, "reason": "",
		}, "", ""},
		{"a child.status holding a member named blocked_by", "child.status", map[string]any{
			"child_key": waiting.Key, "from": "backlog", "to": "todo", "blocked_by": []any{blocker.Key},
		}, "", ""},
	}
	// Each is appended as its build appends it, through the broker, whose insert stores the
	// payload it is handed.
	ids := make([]int64, len(appended))
	broker := events.NewBroker()
	if err := pgx.BeginFunc(ctx, database.Pool, func(tx pgx.Tx) error {
		for index, event := range appended {
			payload, err := json.Marshal(event.payload)
			if err != nil {
				return err
			}
			stored, err := broker.Append(ctx, tx, model.Event{
				IssueKey: &waiting.Key, Type: event.eventType, Actor: model.Actor{Kind: "user", ID: "alice"},
				Payload: json.RawMessage(payload),
			})
			if err != nil {
				return fmt.Errorf("append %s: %w", event.name, err)
			}
			ids[index] = stored.ID
		}
		return nil
	}); err != nil {
		t.Fatalf("append the events of %s: %v", waiting.Key, err)
	}

	type expectation struct {
		name string
		// want is the payload every read answers, decoded.
		want map[string]any
		// stored is the payload's stored text compacted and HTML-escaped, as an answer encodes a
		// json.RawMessage: the bytes a read that answers JSON as the log holds it answers. It is
		// nil for a payload every read answers with a member the log does not hold.
		stored []byte
	}
	expected := map[int64]expectation{}
	for index, event := range appended {
		var text string
		if err := database.Pool.QueryRow(ctx, `select payload::text from events where id = $1`, ids[index]).Scan(&text); err != nil {
			t.Fatalf("read the stored payload of %s: %v", event.name, err)
		}
		var want map[string]any
		if err := json.Unmarshal([]byte(text), &want); err != nil {
			t.Fatalf("decode the stored payload of %s, %s: %v", event.name, text, err)
		}
		expect := expectation{name: event.name, want: want}
		if event.lacks == "" {
			var compacted, escaped bytes.Buffer
			if err := json.Compact(&compacted, []byte(text)); err != nil {
				t.Fatalf("compact the stored payload of %s, %s: %v", event.name, text, err)
			}
			json.HTMLEscape(&escaped, compacted.Bytes())
			expect.stored = escaped.Bytes()
		} else {
			if _, held := want[event.lacks]; held {
				t.Fatalf("the log holds %s with %s: %s", event.name, event.lacks, text)
			}
			want[event.lacks] = want[event.holds]
		}
		expected[ids[index]] = expect
	}

	// requireAnswered fails unless served holds each appended event, each with the payload every
	// read answers, and, where the surface answers JSON as the log holds it (exact), the stored
	// bytes of each payload it answers as stored. The outbox's envelope encodes the payload it
	// decoded, so its keys and numbers are spelt its own way.
	requireAnswered := func(surface string, served []feedEvent, exact bool) {
		t.Helper()
		seen := map[int64]bool{}
		for _, event := range served {
			expect, ours := expected[event.ID]
			if !ours {
				continue
			}
			seen[event.ID] = true
			var got map[string]any
			if err := json.Unmarshal(event.Payload, &got); err != nil {
				t.Fatalf("%s: decode the payload of %s (event %d), %s: %v", surface, expect.name, event.ID, event.Payload, err)
			}
			if !reflect.DeepEqual(got, expect.want) {
				want, _ := json.Marshal(expect.want)
				t.Errorf("%s answers %s (event %d) as\n%s\nwant\n%s", surface, expect.name, event.ID, event.Payload, want)
			}
			if exact && expect.stored != nil && !bytes.Equal(event.Payload, expect.stored) {
				t.Errorf("%s answers %s (event %d) as\n%s\nwant the stored bytes\n%s", surface, expect.name, event.ID, event.Payload, expect.stored)
			}
		}
		if len(seen) != len(expected) {
			t.Errorf("%s answers %d of the %d appended events", surface, len(seen), len(expected))
		}
	}

	// The relay sends each event committed after the subscription's start, and a subscription
	// resuming after that start is replayed them, both read through readEventsAfterID.
	requireAnswered("the live socket's relay", appendedFrames(t, live, "live", expected), true)
	replay := rig.dial(t, withBearer("agent-token"))
	subscribeEvents(t, replay, "replay", &head)
	requireAnswered("the live socket's replay after "+strconv.FormatInt(head, 10), appendedFrames(t, replay, "replay", expected), true)

	// Each paging of the feed is a statement of its own over eventSelect. A second read is written
	// from the bytes the first kept, and a reader holding the ETag is answered 304.
	feed := "/api/v1/issues/" + waiting.Key + "/events"
	listed := make([]string, len(ids))
	for index, id := range ids {
		listed[index] = strconv.FormatInt(id, 10)
	}
	for _, paging := range []string{
		"", "?order=desc", "?before=" + strconv.Itoa(issueLastSeq(t, database, waiting.Key)+1), "?ids=" + strings.Join(listed, ","),
	} {
		target := feed + paging
		first := listRead(t, handler, target, listReadOptions{})
		etag := listETagOf(t, first, "GET "+target)
		requireAnswered("GET "+target, decodeBody[[]feedEvent](t, first), true)
		answered := rig.served.listAnswers.answered()
		kept := listRead(t, handler, target, listReadOptions{})
		if writes := rig.served.listAnswers.answered() - answered; writes != 1 {
			t.Errorf("the cache wrote %d answers over the second GET %s, want the one it kept", writes, target)
		}
		requireAnswered("the kept answer of GET "+target, decodeBody[[]feedEvent](t, kept), true)
		requireNotModified(t, listRead(t, handler, target, listReadOptions{etag: etag}), etag, "GET "+target+" holding its ETag")
	}

	// The relay has sent every appended event, so the safe horizon the log is read under is past
	// each of them.
	logged := dispatchRequest(t, handler, http.MethodGet, "/api/v1/events/log?after="+strconv.FormatInt(head, 10), nil, "alice")
	if logged.Code != http.StatusOK {
		t.Fatalf("read the event log after %d: status=%d body=%s", head, logged.Code, logged.Body.String())
	}
	requireAnswered("GET /api/v1/events/log", decodeBody[[]feedEvent](t, logged), true)

	requireAnswered("the outbox's envelopes", outboxEvents(t, database, waiting.Key), false)
}

// withoutMember is payload less member and less member's name in its changed list: the payload of
// the same write by a build whose issue has no such member.
func withoutMember(payload map[string]any, member string) map[string]any {
	copied := maps.Clone(payload)
	delete(copied, member)
	if changed, ok := copied["changed"].([]any); ok {
		copied["changed"] = slices.DeleteFunc(slices.Clone(changed), func(name any) bool { return name == member })
	}
	return copied
}

// appendedFrames reads sub's events until it has read each event expected names, and answers
// every event it read.
func appendedFrames[T any](t *testing.T, client *socketClient, sub string, expected map[int64]T) []feedEvent {
	t.Helper()
	var read []feedEvent
	held := 0
	for range maxLiveReplay {
		_, event := nextEvent(t, client, sub)
		read = append(read, event)
		if _, appended := expected[event.ID]; appended {
			if held++; held == len(expected) {
				return read
			}
		}
	}
	t.Fatalf("%s carried %d of the %d appended events in %d frames", sub, held, len(expected), maxLiveReplay)
	return nil
}

// outboxEvents runs the outbox until it has published every event of key, and answers the event
// each envelope it published carries. That wait's one bound is the test binary's deadline
// (testwait.Until).
func outboxEvents(t *testing.T, database *store.Store, key string) []feedEvent {
	t.Helper()
	publisher := &recordingOutboxPublisher{}
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		outbox.New(outbox.Deps{
			Store: database, Publisher: publisher, Broker: events.NewBroker(),
			Listen:     database.Pool.Listener,
			RoleClaims: unclaimedRoles{}, Sessions: unclaimedRoles{},
		}).Run(ctx)
	}()
	defer func() {
		cancel()
		<-stopped
	}()
	testwait.Until(t, "the outbox to publish every event of "+key+" (those unpublished)", func() (int, bool) {
		var unpublished int
		if err := database.Pool.QueryRow(context.Background(),
			`select count(*) from events where issue_key = $1 and published_at is null`, key).Scan(&unpublished); err != nil {
			t.Fatalf("count the unpublished events of %s: %v", key, err)
		}
		return unpublished, unpublished == 0
	})
	publisher.mu.Lock()
	defer publisher.mu.Unlock()
	published := make([]feedEvent, 0, len(publisher.items))
	for _, item := range publisher.items {
		var event feedEvent
		if err := json.Unmarshal([]byte(item.Payload), &event); err != nil {
			t.Fatalf("decode the envelope published to %s: %v", item.Topic, err)
		}
		published = append(published, event)
	}
	return published
}
