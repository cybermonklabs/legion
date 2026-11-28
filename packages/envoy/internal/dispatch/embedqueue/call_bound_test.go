package embedqueue

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/sjawhar/envoy/internal/clock/clocktest"
	"github.com/sjawhar/envoy/internal/dispatch/embed"
	"github.com/sjawhar/envoy/internal/testwait"
)

// A call that starts with its claim 59 s old and runs to the queue's bound (callBound, 60 s) has
// the claim renewed before it starts, so it ends with the claim callBound old. Unrenewed, it ended
// with the claim 119 s old, 1 s inside claimWindow, past which another process's scan may take the
// rows and embed them again; and a place with no bound of its own (Bedrock) ran on past it. The
// call's own error is the context's deadline marked an outage, so it moves the pace as no answered
// call does. Both the paced call cmd/dispatch makes and an unpaced one are bounded. The rows run on
// a synctest fake clock, where the call's minute takes no real time.
func TestACallThatCouldOutliveItsClaimRenewsItFirst(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		deps func(embed.Embedder) Deps
	}{
		{"paced", func(e embed.Embedder) Deps { return Deps{Embedder: e}.Paced() }},
		{"unpaced", func(e embed.Embedder) Deps { return Deps{Embedder: e} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				deps := tc.deps(silentEmbedder{})
				started := time.Now()
				renewal := newClaimRenewal(deps, []pendingRow{{kind: "issue", id: "slow", text: "slow"}}, started.Add(-59*time.Second))
				claimed := []time.Time{renewal.last}
				renewal.extend = func(context.Context, []pendingRow) error {
					claimed = append(claimed, time.Now())
					return nil
				}
				_, err := deps.embedDocuments(context.Background(), []string{"slow"}, renewal)
				ended := time.Now()
				if !errors.Is(err, context.DeadlineExceeded) || !embed.IsOutage(err) || embed.IsThrottled(err) {
					t.Errorf("the call ended with %v (outage %v, throttled %v), want the context's deadline as an outage", err, embed.IsOutage(err), embed.IsThrottled(err))
				}
				if ran := ended.Sub(started); ran != callBound {
					t.Errorf("the call ran %v, want the queue's bound, %v", ran, callBound)
				}
				last := claimed[len(claimed)-1]
				if !last.Equal(started) {
					t.Errorf("the claim was last renewed at %v, want before the call started; renewals at %v", last.Sub(started), offsets(claimed, started))
				}
				if age := ended.Sub(last); age > claimWindow-callSlack {
					t.Errorf("the call ended with its claim %v old, past claimWindow less callSlack (%v)", age, claimWindow-callSlack)
				}
			})
		})
	}
}

// A Bedrock call that gets no answer ends at the queue's bound (callBound) with the context's
// deadline, marked an outage, while its caller still waits: Bedrock's SDK sets no bound of its
// own, and unbounded the call ran as long as the service held it, past claimWindow. The stand-in
// Bedrock answers nothing until the row ends. The bound runs on the queue's clock (Deps.Clock), a
// fake the test moves to callBound once the stand-in has the call and the bound is armed. The
// caller gives the call twice callBound on the same fake, the input the bound is checked against,
// which the fake never reaches: unbounded, the call never returns, and the wait for it fails
// naming it.
func TestABedrockCallPastTheBoundEndsWithTheDeadline(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	arrived := make(chan struct{})
	arrive := sync.OnceFunc(func() { close(arrived) })
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		arrive()
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer server.Close()
	defer close(release)
	fake := clocktest.New(time.Date(2026, time.November, 5, 3, 0, 0, 0, time.UTC))
	start := fake.Now()
	deps := Deps{Embedder: bedrockClientFor(server), Clock: fake}.Paced()
	ctx, cancel := fake.WithTimeout(context.Background(), 2*callBound)
	defer cancel()

	ended := make(chan error, 1)
	go func() {
		_, err := deps.embedDocuments(ctx, []string{"bedrock"}, nil)
		ended <- err
	}()
	testwait.Receive(t, "the stand-in Bedrock to have the call", arrived)
	fake.AwaitArmedAt(t, "the call's bound", start.Add(callBound))
	fake.Set(start.Add(callBound))
	err := testwait.Receive(t, "the Bedrock call to return once the queue's clock reached its bound", ended)
	if !errors.Is(err, context.DeadlineExceeded) || !embed.IsOutage(err) || embed.IsThrottled(err) || ctx.Err() != nil {
		t.Fatalf("the Bedrock call ended with %v (outage %v, throttled %v, the caller's context %v), want the bound's deadline as an outage while the caller still waits", err, embed.IsOutage(err), embed.IsThrottled(err), ctx.Err())
	}
}

// silentEmbedder never answers: its call ends only when its context does, or after 10 minutes.
type silentEmbedder struct{ bedrockShaped }

func (silentEmbedder) Embed(ctx context.Context, texts []string, _ embed.InputType) ([][]float32, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(10 * time.Minute):
		return fakeVectors(len(texts)), nil
	}
}
