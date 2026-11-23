package api

import (
	"context"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/agentstream"
)

// withheldReplay is a session whose answer to a replay never comes: its Replay waits for the
// context it is given to end.
type withheldReplay struct{ agentstream.Source }

func (withheldReplay) Replay(ctx context.Context, _ string) (agentstream.Frame, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// A replay whose answer never comes ends at agentStreamReplayTimeout, whatever context the
// viewer's socket gave it, and leaves the session counted as responding, since a slow answer is
// not a missing one (replayAgentStream). This is the bound of Dispatch's agent-stream replay that
// internal/measurebounds' replay row cannot see: that row calls the transport's Replay under a
// context of its own.
//
// The control is this row with replayAgentStream's context.WithTimeout replaced by
// context.WithCancel: the replay is still waiting when the row's wait ends.
func TestAReplayWhoseAnswerNeverComesEndsAtTheReplayTimeout(t *testing.T) {
	const margin = 3 * time.Second
	done := make(chan bool, 1)
	started := time.Now()
	go func() {
		_, responding := (&server{}).replayAgentStream(context.Background(), withheldReplay{}, "ses_withheld_replay")
		done <- responding
	}()
	select {
	case responding := <-done:
		if took := time.Since(started); took < agentStreamReplayTimeout-time.Second || !responding {
			t.Fatalf("the replay answered responding=%t after %s, want responding at its bound %s", responding, took, agentStreamReplayTimeout)
		}
	case <-time.After(agentStreamReplayTimeout + margin):
		t.Fatalf("the replay was still waiting %s after it began, past its bound %s", agentStreamReplayTimeout+margin, agentStreamReplayTimeout)
	}
}
