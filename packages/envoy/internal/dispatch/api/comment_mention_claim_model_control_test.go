//go:build !race

package api

import (
	"os"
	"testing"
)

// The claim model's controls: each is a sender broken on purpose, and the search
// (comment_mention_claim_model_search_test.go's cmModelRuns) must find the break, so the model is
// shown able to see each failure the sender as built must not have. A sender whose re-reads learn
// nothing of a lost refresh must trust a stale claim; and, in the full run
// (ENVOY_MENTION_ORDER_MODEL=full), a refresh that writes a claim even once the claim it names has
// lapsed and a take that counts the row's claim its own only while the claim it carries is live,
// under the same failures, must trust a stale claim, drop a send and let A-2 pass A-1.
//
// They are a test of their own, apart from TestTheMentionSenderClaimModel, so the two run on two
// runners at once: the search of the sender as built takes envoy-go-claim-model's runner for about
// 11 minutes, and CI's control, 1.5 minutes of search at three periods (job 140032939278), runs in
// envoy-go-race-impossible, beside the API package's other tests the race build cannot run
// (dispatch://INFRA-5794/spec#b-r5).
func TestTheClaimModelCatchesEachBrokenSender(t *testing.T) {
	// Not parallel: it swaps the default logger (cmModelRuns, slog.SetDefault).
	runs := []cmModelRun{{v: cmBlindRepair, periods: 3, breaks: []string{"stale"}}}
	if os.Getenv("ENVOY_MENTION_ORDER_MODEL") == "full" {
		// Four periods reach a lapsed claim, which this control's refresh writes again.
		runs = append(runs, cmModelRun{v: cmLapseBlindStatements, periods: 4, breaks: []string{"stale", "dropped", "order"}})
	}
	cmModelRuns(t, runs)
}
