package embedqueue

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/synctest"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/embed"
)

// A call that starts with its claim 59 s old and runs to the queue's bound (callBound, 60 s) has
// the claim renewed before it starts, so it ends with the claim callBound old. Unrenewed, it ended
// with the claim 119 s old, 1 s inside claimWindow, past which another process's scan may take the
// rows and embed them again; and a place with no bound of its own (Bedrock) ran on past it. The
// call's own error is the context's deadline marked an outage, so it moves the pace as no answered
// call does. Both the paced call cmd/dispatch makes and an unpaced one are bounded. A call the pace
// holds 48 s from a fresh claim is renewed as the wait ends: the wait renews nothing, since its
// renewal falls due at renewClaimInterval (60 s), and unrenewed the call ended with the claim 108 s
// old, past claimWindow less callSlack. The rows run on a synctest fake clock, where the call's
// minute takes no real time.
func TestACallThatCouldOutliveItsClaimRenewsItFirst(t *testing.T) {
	paced := func(e embed.Embedder) Deps { return Deps{Embedder: e}.Paced() }
	for _, tc := range []struct {
		name string
		deps func(embed.Embedder) Deps
		// age is how old the claim is when the call is made; held is how long the pace holds it.
		age, held time.Duration
	}{
		{"paced", paced, 59 * time.Second, 0},
		{"unpaced", func(e embed.Embedder) Deps { return Deps{Embedder: e} }, 59 * time.Second, 0},
		{"paced, held 48 s by the pace from a fresh claim", paced, 0, 48 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx := context.Background()
				silent := &silentEmbedder{}
				deps := tc.deps(silent)
				if tc.held > 0 {
					silent.answer = 1 // the call that takes the pace's token
					requirePaceHolds(t, deps, tc.held)
				}
				started := time.Now()
				renewal := newClaimRenewal(deps, []pendingRow{{kind: "issue", id: "slow", text: "slow"}}, started.Add(-tc.age))
				claimed := []time.Time{renewal.last}
				renewal.extend = func(context.Context, []pendingRow) error {
					claimed = append(claimed, time.Now())
					return nil
				}
				_, err := deps.embedDocuments(ctx, []string{"slow"}, renewal)
				ended := time.Now()
				if !errors.Is(err, context.DeadlineExceeded) || !embed.IsOutage(err) || embed.IsThrottled(err) {
					t.Errorf("the call ended with %v (outage %v, throttled %v), want the context's deadline as an outage", err, embed.IsOutage(err), embed.IsThrottled(err))
				}
				if ran := ended.Sub(started); ran != tc.held+callBound {
					t.Errorf("the call ran %v, want the pace's hold and the queue's bound, %v", ran, tc.held+callBound)
				}
				last := claimed[len(claimed)-1]
				if sent := started.Add(tc.held); !last.Equal(sent) {
					t.Errorf("the claim was last renewed at %v, want as the call went out, at %v; renewals at %v", last.Sub(started), tc.held, offsets(claimed, started))
				}
				if age := ended.Sub(last); age > claimWindow-callSlack {
					t.Errorf("the call ended with its claim %v old, past claimWindow less callSlack (%v)", age, claimWindow-callSlack)
				}
			})
		})
	}
}

// requirePaceHolds makes deps' pace hold its next call held: a first call takes the pace's one
// token at once, and the pace is backed off to held, as failing canaries back it off. held must
// lie between claimWindow less callSlack less callBound, past which a call that runs to callBound
// needs its own renewal, and renewClaimInterval, at which the wait would renew the claim itself.
func requirePaceHolds(t *testing.T, deps Deps, held time.Duration) {
	t.Helper()
	if held <= claimWindow-callSlack-callBound || held >= renewClaimInterval {
		t.Fatalf("a hold of %v tests no call's renewal: want one past %v and short of renewClaimInterval (%v)", held, claimWindow-callSlack-callBound, renewClaimInterval)
	}
	if _, err := deps.embedDocuments(context.Background(), []string{"first"}, nil); err != nil {
		t.Fatalf("first call: %v", err)
	}
	for deps.pace.Interval() < held {
		deps.pace.BackOff()
	}
	if got := deps.pace.Interval(); got != held {
		t.Fatalf("the pace holds a call %v, want %v", got, held)
	}
}

// A Bedrock call that gets no answer ends at the queue's bound (callBound) with the context's
// deadline, marked an outage, so its claim is still held when it ends: Bedrock's SDK sets no bound
// of its own, and unbounded the call ran as long as the service held it, past claimWindow. The
// stand-in Bedrock answers nothing until the row ends. callBound is shrunk so the row takes a
// fraction of a second.
func TestABedrockCallPastTheBoundEndsWithTheDeadline(t *testing.T) {
	shortenForTest(t, &callBound, 300*time.Millisecond)
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer server.Close()
	defer close(release)
	client := bedrockClientFor(server)
	deps := Deps{Embedder: client}.Paced()
	renewal := newClaimRenewal(deps, []pendingRow{{kind: "issue", id: "bedrock", text: "bedrock"}}, time.Now())
	renewal.extend = func(context.Context, []pendingRow) error { return nil }
	claimedAt := renewal.last

	_, err := deps.embedDocuments(context.Background(), []string{"bedrock"}, renewal)
	if !errors.Is(err, context.DeadlineExceeded) || !embed.IsOutage(err) || embed.IsThrottled(err) {
		t.Fatalf("the Bedrock call ended with %v (outage %v, throttled %v), want the context's deadline as an outage", err, embed.IsOutage(err), embed.IsThrottled(err))
	}
	if age := time.Since(claimedAt); age >= claimWindow {
		t.Errorf("the Bedrock call ended %v after its claim was made, past claimWindow (%v)", age, claimWindow)
	}
}

// silentEmbedder answers its first answer calls at once and never answers after: such a call ends
// only when its context does, or after 10 minutes.
type silentEmbedder struct {
	bedrockShaped
	answer int
}

func (e *silentEmbedder) Embed(ctx context.Context, texts []string, _ embed.InputType) ([][]float32, error) {
	if e.answer > 0 {
		e.answer--
		return fakeVectors(len(texts)), nil
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(10 * time.Minute):
		return fakeVectors(len(texts)), nil
	}
}
