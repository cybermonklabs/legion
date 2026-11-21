package daemon

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// reportNews posts the tester's plugin's report that a delivery under key reached its session
// after the declaration of wait, and answers the route's decoded 200.
func (r *waitDeliveryRig) reportNews(wait record.ClaimWait, key string) api.WaitNewsResponse {
	r.t.Helper()
	recorder := r.request("/legion/v1/claims/wait-news", api.WaitNewsRequest{ClaimToken: r.token, SessionID: waitingSession,
		Secret: waitingSecret, WaitID: wait.ID, DeliveryKey: key}, "")
	if recorder.Code != http.StatusOK {
		r.t.Fatalf("the news report under %q = %d %s, want 200", key, recorder.Code, recorder.Body)
	}
	var answer api.WaitNewsResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &answer); err != nil {
		r.t.Fatalf("decode the news report's answer %s: %v", recorder.Body, err)
	}
	return answer
}

// The update round's ask, as the daemon's own outbox publishes it, is no news to the waiting
// worker it reaches (dispatch://INFRA-2040 D9): the tester declares a wait in its turn, the pod's
// update round opens on its claim, and the outbox's executor publishes the round's ask to the
// tester's topic. The key it publishes under is the one a daemon that restored the claim from the
// store derives as well, from the opening read back to the microsecond. The plugin's report of
// that delivery, under the key bare and behind the listener's role-forward mark, answers not
// ended and names the update ask, and the wait stands; each is logged once. A person's message
// afterwards is news and ends the wait.
func TestWaitStandsForThePodUpdateAskItsOutboxPublished(t *testing.T) {
	r := newWaitDeliveryRig(t)
	r.working(1, "Test it.")
	wait := r.declare()
	r.handle(supervise.PodUpdateOpen{Claim: r.token, Round: askedRound, Reasons: "image"})
	publisher := &outboxPublisher{}
	r.runner.notices = publisher

	row := mustOutboxRow(t, r.issue.Key, record.UpdateAsk{Role: claim.RoleTester, Round: askedRound, Pod: askedPod, Text: askedText}, time.Now())
	row.ID = 91
	if err := r.runner.execute(r.ctx, row); err != nil {
		t.Fatalf("run the round's ask to the tester: %v", err)
	}
	published := publisher.publishes()
	if len(published) != 1 {
		t.Fatalf("publishes = %#v, want the round's one ask", published)
	}
	key := published[0].key
	stored, err := r.st.Claims(r.ctx)
	if err != nil {
		t.Fatalf("read the stored claims: %v", err)
	}
	var restored *supervise.Claim
	for i := range stored {
		if stored[i].Token == r.token {
			restored = &stored[i]
		}
	}
	if restored == nil || restored.UpdateOpenedAt == nil || restored.UpdateRound != askedRound {
		t.Fatalf("the store holds the tester's claim as %+v, want it in round %s with its opening", restored, askedRound)
	}
	if again := record.UpdateAskKey(r.token, askedRound, restored.UpdateOpenedAt); again != key {
		t.Fatalf("the ask went under %s, and the opening read back from the store (%s) gives %s, want one key",
			key, restored.UpdateOpenedAt.Format(time.RFC3339Nano), again)
	}

	for _, reported := range []string{key, "envoy.role.forward." + key} {
		if answer := r.reportNews(wait, reported); answer != (api.WaitNewsResponse{UpdateAsk: true}) {
			t.Fatalf("the report of the round's ask under %s = %+v, want not ended, the update ask", reported, answer)
		}
		r.wantStanding()
		if lines := r.lines(`msg="wait: the update ask is no news"`, "claim="+string(r.token), "issue="+r.issue.Key, "role=tester",
			"wait="+wait.ID, " key="+reported); len(lines) != 1 {
			t.Fatalf("the report under %s was logged %d times, want once; the log is:\n%s", reported, len(lines), r.logs)
		}
	}

	if answer := r.reportNews(wait, ""); answer != (api.WaitNewsResponse{Ended: true}) {
		t.Fatalf("a person's message after the ask = %+v, want ended", answer)
	}
	r.wantEnded(wait, workflow.WaitEndNews, "api/wait-news:"+string(r.token)+":"+wait.ID)
}
