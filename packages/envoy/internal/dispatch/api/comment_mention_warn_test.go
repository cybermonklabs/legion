package api

import (
	"context"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/logtest"
)

// Every send that waits on a held lane past the warn threshold is reported, once: the one that
// waited for an earlier event of its issue and each send of that issue to the same recipient
// queued behind it - three sends of A-1, three WARN lines, one naming each comment, and no
// second line for any of them however many times the lane parks again.
func TestEverySendHeldPastTheThresholdIsReportedOnce(t *testing.T) {
	logs := &storetest.LockedLog{}
	logtest.SetDefault(t, slog.NewTextHandler(logs, nil))
	lifetime, stop := context.WithCancel(context.Background())
	sender := newMentionSender(lifetime, time.Hour,
		func(_ context.Context, item mentionSend) (mentionSend, bool) { return item, true },
		func(_ context.Context, queued []mentionClaim) ([]mentionClaim, error) { return queued, nil },
		func(context.Context, []mentionClaim) error { return nil },
	)
	sender.mu.Lock()
	sender.heldWarnAfter = 200 * time.Millisecond
	sender.mu.Unlock()
	t.Cleanup(func() {
		stop()
		sender.wait()
	})
	for _, id := range []string{"a-first", "a-second", "a-third"} {
		sender.enqueue(context.Background(), mentionSend{
			comment: model.Comment{ID: id, IssueKey: new("A-1")}, mention: ResolvedMention{Target: "session:s1"},
			attempt: 1, claimed: time.Now(),
		})
	}
	const marker = `msg="dispatch: a comment mention's send waits behind an earlier event of its owner"`
	waitUntil(t, 5*time.Second, "a WARN line for each held send", func() bool {
		return strings.Count(logs.String(), marker) >= 3
	})
	// A second past the threshold the lane has parked again several times.
	time.Sleep(time.Second)
	logged := logs.String()
	t.Logf("WARN lines: %d", strings.Count(logged, marker))
	if got := strings.Count(logged, marker); got != 3 {
		t.Fatalf("%d WARN lines for three held sends, want three, one each", got)
	}
	for _, id := range []string{"a-first", "a-second", "a-third"} {
		if strings.Count(logged, "comment="+id+" ") != 1 {
			t.Fatalf("the WARN lines name %s other than once:\n%s", id, logged)
		}
	}
}

// A send whose wait passes the threshold while no park can see it is reported as its wait ends.
// A-1's first send waits behind event 42 for s1 and parks, moving A-1's second send behind it; its
// retry starts before the threshold and is in flight past it, then goes, and the second goes after
// it. Neither parks again, yet each waited past the threshold: each logs one WARN naming its own
// comment, s1 and event 42.
func TestASendWhoseWaitPassesTheThresholdInFlightIsReportedAsItGoes(t *testing.T) {
	logs := &storetest.LockedLog{}
	logtest.SetDefault(t, slog.NewTextHandler(logs, nil))
	lifetime, stop := context.WithCancel(context.Background())
	retrying, release := make(chan struct{}), make(chan struct{})
	var tries atomic.Int32
	sender := newMentionSender(lifetime, time.Hour,
		func(_ context.Context, item mentionSend) (mentionSend, bool) {
			if item.comment.ID == "a-first" {
				if tries.Add(1) == 1 {
					item.heldFor, item.heldBy = "s1", model.MentionCopyHolder{EventID: 42}
					return item, true
				}
				close(retrying)
				<-release
			}
			return mentionSend{}, false
		},
		func(_ context.Context, queued []mentionClaim) ([]mentionClaim, error) { return queued, nil },
		func(context.Context, []mentionClaim) error { return nil },
	)
	sender.mu.Lock()
	sender.heldWarnAfter = 200 * time.Millisecond
	sender.mu.Unlock()
	t.Cleanup(func() {
		stop()
		sender.wait()
	})
	for _, id := range []string{"a-first", "a-second"} {
		sender.enqueue(context.Background(), mentionSend{
			comment: model.Comment{ID: id, IssueKey: new("A-1")}, mention: ResolvedMention{Target: "session:s1"},
			attempt: 1, claimed: time.Now(),
		})
	}
	<-retrying
	time.Sleep(300 * time.Millisecond)
	const marker = `msg="dispatch: a comment mention's send waits behind an earlier event of its owner"`
	if got := strings.Count(logs.String(), marker); got != 0 {
		t.Fatalf("%d WARN lines before either send's wait ended, want none: no park saw the threshold pass", got)
	}
	close(release)
	waitUntil(t, 5*time.Second, "A-1's two sends gone", func() bool {
		sender.mu.Lock()
		defer sender.mu.Unlock()
		return len(sender.held) == 0
	})
	logged := logs.String()
	t.Logf("WARN lines: %d", strings.Count(logged, marker))
	for _, id := range []string{"a-first", "a-second"} {
		lines := 0
		for _, line := range strings.Split(logged, "\n") {
			if strings.Contains(line, marker) && strings.Contains(line, "comment="+id+" ") {
				lines++
				if !strings.Contains(line, "session=s1 ") || !strings.Contains(line, "event_id=42 ") {
					t.Fatalf("the WARN line of %s does not name s1 and event 42: %s", id, line)
				}
			}
		}
		if lines != 1 {
			t.Fatalf("%d WARN lines name %s, which waited past the threshold, want one:\n%s", lines, id, logged)
		}
	}
}

// A held lane whose head waits again is parked by the goroutine that served it, and that goroutine
// gives it up then and there: the lane's timer, run here the moment the park ends (after), starts
// one goroutine on it before the first goes on, and no two goroutines ever send two of its sends
// at once. A-1's first send waits three times, A-1's second waits behind it; each send takes 5 ms,
// so two goroutines on the lane would have both in flight together.
func TestAReparkedHeldLaneHasOneGoroutine(t *testing.T) {
	lifetime, stop := context.WithCancel(context.Background())
	var tries, active, most atomic.Int32
	var mu sync.Mutex
	var sent []string
	sender := newMentionSender(lifetime, time.Hour,
		func(_ context.Context, item mentionSend) (mentionSend, bool) {
			now := active.Add(1)
			defer active.Add(-1)
			for seen := most.Load(); now > seen && !most.CompareAndSwap(seen, now); seen = most.Load() {
			}
			time.Sleep(5 * time.Millisecond)
			if item.comment.ID == "a-first" && tries.Add(1) <= 3 {
				item.heldFor, item.heldBy = "s1", model.MentionCopyHolder{EventID: 7}
				return item, true
			}
			mu.Lock()
			defer mu.Unlock()
			sent = append(sent, item.comment.ID)
			return mentionSend{}, false
		},
		func(_ context.Context, queued []mentionClaim) ([]mentionClaim, error) { return queued, nil },
		func(context.Context, []mentionClaim) error { return nil },
	)
	sender.mu.Lock()
	sender.after = func(_ time.Duration, f func()) { f() }
	sender.mu.Unlock()
	t.Cleanup(func() {
		stop()
		sender.wait()
	})
	for _, id := range []string{"a-first", "a-second"} {
		sender.enqueue(context.Background(), mentionSend{
			comment: model.Comment{ID: id, IssueKey: new("A-1")}, mention: ResolvedMention{Target: "session:s1"},
			attempt: 1, claimed: time.Now(),
		})
	}
	waitUntil(t, 5*time.Second, "A-1's two sends gone", func() bool {
		sender.mu.Lock()
		defer sender.mu.Unlock()
		return len(sender.held) == 0
	})
	mu.Lock()
	defer mu.Unlock()
	t.Logf("sent %v; at most %d sends in flight at once", sent, most.Load())
	if most.Load() != 1 {
		t.Fatalf("%d sends of one lane were in flight at once, want one: two goroutines served the held lane", most.Load())
	}
	if !slices.Equal(sent, []string{"a-first", "a-second"}) {
		t.Fatalf("sent %v, want A-1's first, then its second, each once", sent)
	}
}
