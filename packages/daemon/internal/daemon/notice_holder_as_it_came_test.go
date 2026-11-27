package daemon

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/githubtest"
	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// TestALaterCopyThatFailedOnTheArchitectsOwnSessionWaitsWhateverItsNoticeSays: a later copy of a
// notice the listener could not confirm (504 receipt_timeout) names the architect's own ready
// session as its holder, so it keeps its re-send delay (copyDueAtOnce). The notice's own words
// name that session too, and the publish body's message is a credential the rule redacts from
// the listener's answer, so the rule prints the holder as [redacted]. The copy is decided on the
// holder as the listener wrote it, and waits; decided on the rule's reading, it named another
// session and went at once, spending the schedule meant for a session that hears nothing. The
// control's notice does not name the session, and its copy waits either way.
func TestALaterCopyThatFailedOnTheArchitectsOwnSessionWaitsWhateverItsNoticeSays(t *testing.T) {
	const session = "ses_0123456789abcdef"
	for _, tc := range []struct{ name, message string }{
		{"the notice names the session", "pr-blocked on LEGION-2, for the architect " + session},
		{"control: the notice names no session", "pr-blocked on LEGION-2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := isolatedOutboxPool(t)
			sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
			architectClaimOn(t, sup, "LEGION-1", supervise.StateReady, session)
			architect := mustClaimToken(t, "LEGION-1", claim.RoleArchitect)
			listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusGatewayTimeout)
				_, _ = io.WriteString(w, `{"error":"not confirmed","reason":"receipt_timeout","event_id":"evt-1","holder":"`+session+`","attempts":1}`)
			}))
			t.Cleanup(listener.Close)
			clock := time.Now()
			runner := &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", records: record.NewStore(), supervisor: sup, project: "legion", now: func() time.Time { return clock }}
			copied := record.Notice{Kind: "pr-blocked", Role: claim.RoleArchitect, Reason: "review_round_cap", Resends: 1}
			row := mustOutboxRow(t, "LEGION-2", copied, clock)
			row.ID = 7
			_, published := notify.New(listener.URL, githubtest.InstallationToken("listener bearer")).Publish(context.Background(),
				notify.RoleTopicPrefix+string(architect), tc.message, copied, record.OutboxKey(row.ID))
			if err := runner.noticeAnswered(context.Background(), row, copied, treeSnapshot{}, architect, published); err != nil {
				t.Fatalf("the answer %v re-held nothing: %v", published, err)
			}
			var nextAt time.Time
			if err := pool.QueryRow(context.Background(), "select next_at from outbox where kind = 'notice'").Scan(&nextAt); err != nil {
				t.Fatalf("read the re-held row: %v", err)
			}
			if want := clock.Add(noticeReholdDelays[1]).Truncate(time.Microsecond); !nextAt.Equal(want) {
				t.Fatalf("the copy that failed on the architect's own session %s is due %s; want %s, its delay", session, nextAt, want)
			}
		})
	}
}
