package api

import (
	"net/http"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// The person kind of the store census names a person an ask waits on only while that ask is open
// and marked (graphcensus's commentPeople), and the writers of this child make a person's node
// the moment they state a tuple that names them (graphstore.ToPerson, FromPerson). So an ask that
// moves from one person to another, and an ask somebody else answers, each leave a person behind
// whose node the store holds and whose row that arm no longer names: without the log arm that
// reads every person a delivered_to, a mentions, a participates_in or a mark ever named
// (graphcensus's reachedHistory), the census would read each of them as "a live node no row
// projects".
//
// This test writes exactly that: an ask opened waiting on bob (PUT /api/v1/asks/{id}/asked-of),
// moved on to alice, and then answered by bob, so bob waits on nothing while his node stands and
// the answerer is somebody other than the person the ask last waited on. The census then reads no
// difference over any kind, and the person kind compared rows.
func TestTheCensusHoldsEveryPersonAWriteOfThisChildNamed(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{})
	issue := createInteractionIssue(t, handler, "PROJ", "Who answers this?",
		"# Spec\n\nA passage an ask hangs under.\n")

	askID := createIssueAsk(t, handler, issue.Key, "Does this ship?")
	handAskOn(t, handler, askID, "bob", "alice")
	handAskOn(t, handler, askID, "alice", "alice")

	read := dispatchRequest(t, handler, http.MethodGet, "/api/v1/asks/"+askID, nil, "alice")
	if read.Code != http.StatusOK {
		t.Fatalf("read ask %s: status=%d body=%s", askID, read.Code, read.Body.String())
	}
	ask := decodeBody[model.Ask](t, read)
	answered := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+askID+"/answer", map[string]any{
		"text":               "It ships.",
		"expected_edited_at": ask.EditedAt,
	}, "bob")
	if answered.Code != http.StatusOK {
		t.Fatalf("answer the ask as alice: status=%d body=%s", answered.Code, answered.Body.String())
	}

	report := requireTheCensusAgreesOver(t, database, "over an ask moved to bob, moved back to alice and answered by bob", "person")
	if report.Differences() != 0 {
		t.Errorf("the census reads %d difference(s) after the move and the answer, want none", report.Differences())
	}
}
