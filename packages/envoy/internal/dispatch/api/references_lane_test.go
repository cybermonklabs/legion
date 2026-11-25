package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/admission"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// An issue's references read keeps a heavy read lane of its own: on a production-sized database
// the one-hop read still runs to its statement timeout (INFRA-592, c-scale at e151d049), so each
// such read holds its place for seconds, and a flood of it must be held to its lane rather than
// take the read class from every other route. Here the lane is one place wide and its queue holds
// its cap, admission.ReadBurstFactor times the lane's limit, the bound on the queue's memory, and a
// lock on issues, which each references read's first statement (loadIssue) waits for, holds the
// lane's read and the 22 queued behind it. A references read past those is refused at once, 429
// OVERLOADED naming the references lane, and the reads of another route sent while the flood holds
// are admitted to the read units the lane left free and answered at once. Without the lane the
// flood takes three of the read class's four units, the references reads past it take the fourth
// or queue for it, and none of them is refused.
func TestAFloodOfIssueReferencesPastItsLaneIsRefusedWhileOtherReadsAreAdmitted(t *testing.T) {
	const (
		readCap = 4
		lane    = 1
		// queue is the lane's queue at its cap, and held is the lane's place and that queue.
		queue = admission.ReadBurstFactor * lane
		held  = lane + queue
		past  = 2
	)
	limiter := admission.New(admission.Limits{Read: readCap, Write: 4, HeavyReadLane: lane, ReadWait: 5 * time.Second})
	handler, database, _ := newTestServer(t, testServerOptions{admission: limiter})
	createReferenceAPIProject(t, handler, "CORE")
	issue := createReferenceAPIIssue(t, handler, "CORE")
	design := createProjectDocument(t, handler, "CORE", "Design notes", "# Design\n")
	createReferenceAPIComment(t, handler, issue.Key, "dispatch://CORE/artifact/design-notes")

	release := lockTables(t, database, "issues")
	flood := make(chan *httptest.ResponseRecorder, held)
	for range held {
		go func() { flood <- issueReferencesRequest(t, handler, issue.Key, "") }()
	}
	// Each read of the flood holds a read unit or waits in the lane's queue, holding none.
	waitUntil(t, "the flood's references reads admitted or queued in their lane", func() bool {
		return limiter.Stats()[admission.Read].InFlight+referencesLane(t, limiter).Queued == held
	})

	refused := make(chan *httptest.ResponseRecorder, past)
	for range past {
		go func() { refused <- issueReferencesRequest(t, handler, issue.Key, "") }()
	}
	for range past {
		select {
		case response := <-refused:
			body := response.Body.String()
			assertRetryable(t, response.Result(), http.StatusTooManyRequests, admission.CodeOverloaded)
			if !strings.Contains(body, "references lane") {
				t.Fatalf("a references read past the lane's place and queue was refused %s, want the references lane named", body)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("a references read past the lane's place and queue was not answered within 2 s, want 429 OVERLOADED "+
				"naming the references lane at once: the flood holds the read class (%+v)", limiter.Stats()[admission.Read])
		}
	}

	others := make(chan *httptest.ResponseRecorder, readCap-lane)
	sent := time.Now()
	for range readCap - lane {
		go func() { others <- dispatchRequest(t, handler, http.MethodGet, "/api/v1/me/state", nil, "alice") }()
	}
	for range readCap - lane {
		if response := awaitResponse(t, others); response.Code != http.StatusOK {
			t.Fatalf("a read of another route sent while the flood held its lane: status=%d body=%s, want 200",
				response.Code, response.Body.String())
		}
	}
	if took := time.Since(sent); took > time.Second {
		t.Fatalf("the reads of another route sent while the flood held its lane answered after %s, want at once",
			took.Round(time.Millisecond))
	}
	if stats := referencesLane(t, limiter); stats.InFlight != lane || stats.Queued != queue || stats.QueueFull != past {
		t.Fatalf("the references lane holds %d, queues %d and refused %d for a full queue, want %d, %d and %d",
			stats.InFlight, stats.Queued, stats.QueueFull, lane, queue, past)
	}
	release()

	for range held {
		response := awaitFloodResponse(t, flood)
		if response.Code != http.StatusOK {
			t.Fatalf("a references read the lane held or queued: status=%d body=%s, want 200", response.Code, response.Body.String())
		}
		if answer := decodeBody[model.IssueReferences](t, response); len(answer.Members) != 1 || answer.Members[0].Artifact.ID != design.ID {
			t.Fatalf("a references read the lane held or queued answered %#v, want Design notes alone", answer)
		}
	}
	if stats := limiter.Stats()[admission.Read]; stats.QueueFull != 0 || stats.WaitExceeded != 0 {
		t.Fatalf("the read class refused reads during the flood: %+v, want none", stats)
	}
}

// referencesLane reads the references lane's counts.
func referencesLane(t *testing.T, limiter *admission.Limiter) admission.LaneStats {
	t.Helper()
	for _, stats := range limiter.LaneStats() {
		if stats.Lane == admission.References {
			return stats
		}
	}
	t.Fatal("the limiter reports no references lane")
	return admission.LaneStats{}
}

// awaitFloodResponse is awaitResponse with room for a loaded box: the flood's reads and the reads
// behind them all answer once the lock goes, one read class's worth at a time.
func awaitFloodResponse(t *testing.T, responses <-chan *httptest.ResponseRecorder) *httptest.ResponseRecorder {
	t.Helper()
	select {
	case response := <-responses:
		return response
	case <-time.After(10 * time.Second):
		t.Fatal("a read did not answer within 10 s of the lock's release")
		return nil
	}
}
