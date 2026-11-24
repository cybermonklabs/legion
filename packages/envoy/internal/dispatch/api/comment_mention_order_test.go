package api

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/testwait"
)

// sessionView is, in the one order they reached it, every copy of one issue's events that reaches
// session s1, which follows the issue's topic family: what the outbox publishes to that family or
// to s1's own subject, and the mentions the listener hands s1. Each copy is appended under the
// view's lock at the moment it lands - the outbox's publish as it is accepted, the listener's send
// as it records it - so the order of the list is the order s1's relay hands them on.
//
// It is also the outbox's publisher, and refuses every publish to a topic under refused while that
// is set: the stream not capturing the issue's topic family, so the outbox backs the send off.
type sessionView struct {
	mu      sync.Mutex
	issue   string
	refused string
	// ownOnly is that s1 follows no topic family: only its own subject and its mentions reach it.
	ownOnly bool
	copies  []sessionCopy
}

// sessionCopy is one copy that reached s1: the event it is a copy of, by its id for the outbox's
// envelopes and by the comment's body for a mention, and the channel it came on.
type sessionCopy struct {
	eventID int64
	body    string
	channel string
}

// mentionChannel is the channel a mention the listener hands s1 comes on.
const mentionChannel = "mention"

func (v *sessionView) topicFamily() string {
	return "notifications.dispatch.issue." + v.issue + "."
}

// refuseTopics makes the stream refuse every publish to the issue's topic family, or capture it
// again when refuse is false.
func (v *sessionView) refuseTopics(refuse bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.refused = ""
	if refuse {
		v.refused = v.topicFamily()
	}
}

// mentionLanded is the stand-in listener's landed hook: a send it handed s1.
func (v *sessionView) mentionLanded(send mentionListenerSend) {
	if send.session != "s1" {
		return
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	v.copies = append(v.copies, sessionCopy{body: send.body, channel: mentionChannel})
}

func (v *sessionView) Connected() bool { return true }

func (v *sessionView) ConnectionChanged() <-chan struct{} { return nil }

func (v *sessionView) WindowUntil(func() time.Time) bus.Sender { return v }

func (v *sessionView) Request(_ string, item contracts.Envelope, _ bus.RequestOptions) <-chan error {
	return v.PublishAsync(item)
}

func (v *sessionView) PublishStoredAsync(_ string, item contracts.Envelope, _ string) <-chan error {
	return v.PublishAsync(item)
}

func (v *sessionView) Probe(time.Duration) error { return nil }

func (v *sessionView) PublishAsync(item contracts.Envelope) <-chan error {
	answer := make(chan error, 1)
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.refused != "" && strings.HasPrefix(item.Topic, v.refused) {
		answer <- errors.New("the stream does not capture " + item.Topic)
		return answer
	}
	if (!v.ownOnly && strings.HasPrefix(item.Topic, v.topicFamily())) || item.Topic == contracts.LegacyAgentSubject("s1") {
		id, err := strconv.ParseInt(item.SourceEventID, 10, 64)
		if err != nil {
			answer <- err
			return answer
		}
		v.copies = append(v.copies, sessionCopy{eventID: id, channel: item.Topic})
	}
	answer <- nil
	return answer
}

// order is the order s1 takes the events of ids in: every copy of one of them, in the order the
// copies reached s1, a repeat of an event s1 already took dropped, as its host drops one by its
// event id. mentioned maps the comment bodies of mentions to their events. own is the channel of
// each copy of commentEvent that reached s1 on its own channels - the listener's mentions
// (mentionChannel) and the outbox's publishes to s1's own subject - before any dedupe.
func (v *sessionView) order(ids []int64, mentioned map[string]int64, commentEvent int64) (taken []int64, own []string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	seen := map[int64]bool{}
	for _, copied := range v.copies {
		id := copied.eventID
		if copied.channel == mentionChannel {
			id = mentioned[copied.body]
		}
		if !slices.Contains(ids, id) {
			continue
		}
		if id == commentEvent && (copied.channel == mentionChannel || copied.channel == contracts.LegacyAgentSubject("s1")) {
			own = append(own, copied.channel)
		}
		if !seen[id] {
			seen[id] = true
			taken = append(taken, id)
		}
	}
	return taken, own
}

// commentEventID is the id of the comment.created event of commentID.
func commentEventID(t *testing.T, database *store.Store, commentID string) int64 {
	t.Helper()
	var id int64
	if err := database.Pool.QueryRow(context.Background(), `
		select id from events where type = 'comment.created' and payload ->> 'id' = $1
	`, commentID).Scan(&id); err != nil {
		t.Fatalf("read the comment.created event of %s: %v", commentID, err)
	}
	return id
}

// eventAttempts is the attempt count of event id and whether it is published.
func eventAttempts(t *testing.T, database *store.Store, id int64) (attempts int, published bool) {
	t.Helper()
	if err := database.Pool.QueryRow(context.Background(), `
		select attempt_count, published_at is not null from events where id = $1
	`, id).Scan(&attempts, &published); err != nil {
		t.Fatalf("read event %d: %v", id, err)
	}
	return attempts, published
}

// unpublishedEvents counts the events of issueKey the outbox has not published.
func unpublishedEvents(t *testing.T, database *store.Store, issueKey string) int {
	t.Helper()
	var count int
	if err := database.Pool.QueryRow(context.Background(), `
		select count(*) from events where issue_key = $1 and published_at is null
	`, issueKey).Scan(&count); err != nil {
		t.Fatalf("count the unpublished events of %s: %v", issueKey, err)
	}
	return count
}

// A session takes a mention's copy only after each earlier event of the same issue that targets
// it, on both roads a mention's copy takes: the API's own send of the mention, and the outbox's
// comment_fallback_copies row, which it publishes when that send does not settle. Session s1
// follows the issue's topic family, so event 1 - a session's comment, which the outbox sends to
// that family alone - targets it, and the stream refuses the family's subjects, so event 1's
// topic send backs off. A person's later comment of the issue mentions s1. s1 takes event 1
// first and the comment's copy once.
//
// The control is the same issue with the stream capturing the topic family: event 1 is published
// before the comment is written, and s1 takes them in that order. It is what shows the rig reads
// s1's copies in the order they landed: a rig that read them in any other order would fail it too.
func TestAMentionCopyKeepsTheIssueOrder(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// backsOff is whether the stream refuses the topic family while event 1 is first sent.
		backsOff bool
		// fallback is whether the mention's send is held past a claim lease, the outbox holding
		// s1's copy of the comment as a comment_fallback_copies row, and then fails while event 1
		// still backs off, so the row is the copy s1 takes. The issue's route is s1 then, which is
		// the copy the outbox holds; event 1, a session's comment, goes to the topic family alone
		// either way.
		fallback bool
		// channel is where the comment's one copy reaches s1: the listener's mention, or the
		// outbox's publish of the row to s1's own subject.
		channel string
	}{
		{name: "the API's send of the mention, event 1's topic send backed off", backsOff: true, channel: mentionChannel},
		{name: "the outbox's fallback copy of the mention, event 1's topic send backed off", backsOff: true, fallback: true, channel: contracts.LegacyAgentSubject("s1")},
		{name: "control: event 1's topic send published before the comment", channel: mentionChannel},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Logf("start %s", time.Now().UTC().Format(time.RFC3339Nano))
			listener, state := newMentionListener(t)
			// The deployment publishes its events through an outbox, which runs beside the server
			// below, so a mention's send keeps its order behind the events that outbox still owes.
			handler, database, deps, _ := newRegisteredTestServer(t, testServerOptions{
				envoyURL: listener.URL, envoyTimeout: 30 * time.Second, outboxPublishes: true,
			})
			issue := createInteractionIssue(t, handler, "TEST", "A mention after a backed-off event", "before")
			if tc.fallback {
				if patched := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue.Key, map[string]any{
					"route": "session:s1",
				}, "alice"); patched.Code != http.StatusOK {
					t.Fatalf("set the route: status=%d body=%s", patched.Code, patched.Body.String())
				}
			}
			view := &sessionView{issue: issue.Key}
			state.mu.Lock()
			state.landed = view.mentionLanded
			state.mu.Unlock()
			runOutboxBeside(t, deps, view)
			waitUntil(t, 10*time.Second, "the issue's first events published", func() bool {
				return unpublishedEvents(t, database, issue.Key) == 0
			})

			view.refuseTopics(tc.backsOff)
			firstComment := decodeMentionedComment(t, sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
				"body": "event 1, by a session", "actor": map[string]any{"kind": "session", "id": "s2"},
			}))
			first := commentEventID(t, database, firstComment.ID)
			if tc.backsOff {
				waitUntil(t, 10*time.Second, "event 1's topic send backed off", func() bool {
					attempts, _ := eventAttempts(t, database, first)
					return attempts > 0
				})
			} else {
				waitUntil(t, 10*time.Second, "event 1 published", func() bool {
					_, published := eventAttempts(t, database, first)
					return published
				})
			}

			release := func() {}
			if tc.fallback {
				_, release = state.stall("s1")
				t.Cleanup(release)
			}
			const body = "the comment that mentions s1"
			mention := decodeMentionedComment(t, postMentionedComment(t, handler, issue.Key, map[string]any{
				"body": body, "mentions": []map[string]any{{"target": "session:s1"}},
			}))
			comment := commentEventID(t, database, mention.ID)
			t.Logf("event 1 id=%d, the comment's event id=%d", first, comment)

			if tc.fallback {
				// The send is held, at the listener or before it, under the sender's live claim;
				// a claim lease passes, and the outbox holds s1's copy as a fallback row.
				waitUntil(t, 10*time.Second, "the mention's send held under a live claim", func() bool {
					return claimedPending(t, database, mention.ID, "session:s1")
				})
				ageAttempts(t, database, mention.ID)
				awaitHeldCopy(t, database, mention.ID, contracts.LegacyAgentSubject("s1"))
				// The send fails while event 1 still backs off: here its attempt is written failed,
				// as a send that found s1 no longer live records it with no try, and a send that
				// does reach the listener is refused. The row is the copy s1 takes.
				state.refuseSends("s1")
				if _, err := database.Pool.Exec(context.Background(), `
					update comment_deliveries set state = 'failed', error = 'no live session'
					where comment_id = $1 and state = 'pending'
				`, mention.ID); err != nil {
					t.Fatalf("fail the mention's attempt: %v", err)
				}
				release()
				// Long enough for the fallback pass to publish a row that waits on nothing.
				time.Sleep(3 * time.Second)
			} else if tc.backsOff {
				// Long enough for a send that waits on nothing to land; one that waits for event 1
				// is still waiting when it ends.
				testwait.Holds(3*time.Second, 10*time.Millisecond, func() bool { return len(state.sent()) > 0 })
			}
			view.refuseTopics(false)

			waitUntil(t, 30*time.Second, "every event of the issue published and the mention settled", func() bool {
				settled := true
				for _, attempt := range waitForCommentDeliveries(t, database, mention.ID, "the mention's attempts", func([]model.CommentDelivery) bool { return true }) {
					settled = settled && attempt.State != "pending"
				}
				if tc.fallback {
					for _, row := range fallbackCopiesOf(t, database, mention.ID) {
						settled = settled && row.settled
					}
				}
				return settled && unpublishedEvents(t, database, issue.Key) == 0
			})

			taken, own := view.order([]int64{first, comment}, map[string]int64{body: comment}, comment)
			t.Logf("s1 takes %v (want %v), the comment's copies on s1's own channels: %v (want one, %s)", taken, []int64{first, comment}, own, tc.channel)
			t.Logf("end %s", time.Now().UTC().Format(time.RFC3339Nano))
			if !slices.Equal(taken, []int64{first, comment}) {
				t.Errorf("s1 took the issue's events as %v, want %v: event 1 targets s1 through the topic family it follows, so the mention's copy comes after it", taken, []int64{first, comment})
			}
			if !slices.Equal(own, []string{tc.channel}) {
				t.Errorf("the comment reached s1 on its own channels as %v, want once, on %s", own, tc.channel)
			}
		})
	}
}

// awaitHeldCopy waits, for at most 30 s, until the outbox holds the copy of commentID at
// destination as a comment_fallback_copies row.
func awaitHeldCopy(t *testing.T, database *store.Store, commentID, destination string) {
	t.Helper()
	waitUntil(t, 30*time.Second, "the copy at "+destination+" held as a fallback row", func() bool {
		return slices.ContainsFunc(fallbackCopiesOf(t, database, commentID), func(row heldCopy) bool {
			return row.destination == destination
		})
	})
}
