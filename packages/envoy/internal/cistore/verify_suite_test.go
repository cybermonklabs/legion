package cistore

import (
	"context"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/logging"
)

// With ENVOY_CI_VERIFY on, a suite that is not completed holds the head by the same rule as with
// it off (suiteHolds), at each check of the verify path, all of which read settlementReady: the
// claim (schedule), the wait (checkVerify) and the authorization point (ClaimStillHeld). A record a
// listener before that rule left, its runs finished, beside another App's queued suite, is claimed
// and asked about, and publishes on Dispatch's answer. The same record beside GitHub Actions'
// queued suite, or a queued suite of no App, is claimed by no pass and writes no verify request.
func TestWithVerifyOnAQueuedSuiteHoldsAHeadOnlyIfItIsActionsOrOfNoApp(t *testing.T) {
	h := head{recordRunsOwner, recordRunsRepo, recordRunsNumber, recordRunsSHA}
	for _, tc := range []struct {
		name, app string
		want      int
	}{
		{"another App's queued suite", "5789269", 1},
		{"GitHub Actions' queued suite", actionsApp, 0},
		{"a queued suite the record names no App for", "", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn, cleanup := connectNATS(t)
			defer cleanup()
			s := openVerifyingStore(t, conn)
			writer := dispatchWriter(t, conn)
			putRecord(t, s, h, func(st *State) {
				heldBeforeTheChange(st)
				st.Suites["6200"] = Suite{ID: "6200", AppID: tc.app, Status: "queued", ObservedAt: "2026-11-22T01:00:00Z"}
			})
			z, pub := testSummarizer(s, 0), &recPub{}
			z.pass(pub, logging.New("test"), time.Now().UnixMilli())

			if tc.want == 0 {
				z.pass(pub, logging.New("test"), time.Now().UnixMilli())
				requests, err := writer.VerifyRequests(context.Background())
				if wait, held := waiting(z); held || err != nil || len(requests) != 0 || pub.count() != 0 {
					t.Fatalf("a head held by %s: wait %+v (held %v), verify requests %+v (%v), %d settlements; want none of them", tc.name, wait, held, requests, err, pub.count())
				}
				return
			}
			requested := asked(t, z)
			if pub.count() != 0 {
				t.Fatalf("a head beside %s published %d settlements before any answer", tc.name, pub.count())
			}
			answer(t, writer, requested, s)
			z.pass(pub, logging.New("test"), time.Now().UnixMilli())
			if pub.count() != 1 {
				t.Fatalf("a head beside %s published %d settlements on the answer to request %d, want 1", tc.name, pub.count(), requested)
			}
		})
	}
}
