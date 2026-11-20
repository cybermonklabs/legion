// results_state_hidden_test.go holds the hide-state switch of an end-to-end test process
// (Deps.HideStateFile): while its file exists, GET /api/v1/issues/{key}/results answers the
// answer it gives without the file, less exactly each check's pass_stands and ci and each
// result's currency. The test compares the two answers as JSON, so a member the switch drops
// beside those three, or one of the three it keeps, fails it.
package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// hiddenStateVerification is c1, a check whose ci_check names go-checks, and k1, a check a role
// proves.
const hiddenStateVerification = ciEachHeadVerification + "\n" +
	":::check{#k1 negative_control=\"A page that does not load.\"}\n" +
	"The page loads at the head.\n" +
	":::\n"

// hiddenStateRead is the results read of issue as JSON, its numbers as written.
func hiddenStateRead(t *testing.T, handler http.Handler, issue string) map[string]any {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue+"/results", nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("read the results of %s: status=%d body=%s", issue, response.Code, response.Body.String())
	}
	decoder := json.NewDecoder(bytes.NewReader(response.Body.Bytes()))
	decoder.UseNumber()
	var answer map[string]any
	if err := decoder.Decode(&answer); err != nil {
		t.Fatalf("decode the results of %s: %v", issue, err)
	}
	return answer
}

// hiddenStateMembers counts, over every check of answer and every result of its results and
// recorded_results, how many carry each member.
func hiddenStateMembers(answer map[string]any) map[string]int {
	held := map[string]int{}
	checks, _ := answer["checks"].([]any)
	for _, item := range checks {
		check, _ := item.(map[string]any)
		for member := range check {
			held["check."+member]++
		}
		for _, list := range []string{"results", "recorded_results"} {
			results, _ := check[list].([]any)
			for _, entry := range results {
				result, _ := entry.(map[string]any)
				for member := range result {
					held["result."+member]++
				}
			}
		}
	}
	return held
}

// hiddenStateWithout is answer with each check's pass_stands and ci and each result's currency
// left out: what the contract says the switch answers.
func hiddenStateWithout(answer map[string]any) map[string]any {
	checks, _ := answer["checks"].([]any)
	for _, item := range checks {
		check, _ := item.(map[string]any)
		delete(check, "pass_stands")
		delete(check, "ci")
		for _, list := range []string{"results", "recorded_results"} {
			results, _ := check[list].([]any)
			for _, entry := range results {
				result, _ := entry.(map[string]any)
				delete(result, "currency")
			}
		}
	}
	return answer
}

// TestTheHiddenStateAnswerIsTheAnswerLessTheComputedState: #70 is open at a head where go-checks
// passed (c1's ci and its derived result), and the tester passed k1 there and failed it, the fail
// retracted. The answer without the file carries pass_stands, ci, currency, commit_stands,
// check_words_moved and retracted, checked first, so the comparison cannot pass on an answer that
// lacked them. With the file, the answer equals that answer less each check's pass_stands and ci
// and each result's currency; with the file removed, the whole answer is back.
func TestTheHiddenStateAnswerIsTheAnswerLessTheComputedState(t *testing.T) {
	rig := newCIJobRig(t)
	hide := filepath.Join(t.TempDir(), "hidden")
	rig.served.deps.HideStateFile = hide
	writeHead(t, rig.database, ciJobRepository, 70, ciJobHead, "open", rig.issue)
	rig.github.listings.set(ciJobHead, completedRun(81, ciJobName, "success"))
	jobRecorded(t, "the CI job's result", rig.post(t, rig.issue, rig.jobToken(nil), jobResultOf("c1", "pass")))
	eachHeadResult(t, rig.handler, rig.issue, "k1", "tester", "pass", ciJobHead)
	fail := eachHeadResult(t, rig.handler, rig.issue, "k1", "tester", "fail", ciJobHead)
	if response := dispatchRequest(t, rig.handler, http.MethodPost, "/api/v1/results/"+fail+"/retract",
		map[string]any{"reason": "Recorded against the wrong build."}, "alice"); response.Code != http.StatusCreated && response.Code != http.StatusOK {
		t.Fatalf("retract %s: status=%d body=%s", fail, response.Code, response.Body.String())
	}

	handler, issue := rig.handler, rig.issue
	shown := hiddenStateRead(t, handler, issue)
	recordedCurrency := 0
	for _, item := range shown["checks"].([]any) {
		check := item.(map[string]any)
		if check["block_id"] != "c1" {
			continue
		}
		for _, item := range check["recorded_results"].([]any) {
			if _, ok := item.(map[string]any)["currency"]; ok {
				recordedCurrency++
			}
		}
	}
	if recordedCurrency != 1 {
		t.Fatalf("the recorded result's currency count = %d, want 1", recordedCurrency)
	}
	members := hiddenStateMembers(shown)
	for _, member := range []string{"check.pass_stands", "check.ci", "result.currency", "result.commit_stands", "result.check_words_moved", "result.retracted"} {
		if members[member] == 0 {
			t.Fatalf("the answer without the file holds no %s, so the comparison could not see it: %v", member, members)
		}
	}

	if err := os.WriteFile(hide, nil, 0o600); err != nil {
		t.Fatalf("create the hide-state file: %v", err)
	}
	hidden := hiddenStateRead(t, handler, issue)
	if second := hiddenStateRead(t, handler, issue); !reflect.DeepEqual(hidden, second) {
		t.Fatalf("two reads with the file differ, so neither is the answer the switch gives:\n%v\n%v", hidden, second)
	}
	if err := os.Remove(hide); err != nil {
		t.Fatalf("remove the hide-state file: %v", err)
	}
	again := hiddenStateRead(t, handler, issue)
	if !reflect.DeepEqual(again, shown) {
		got, _ := json.Marshal(again)
		wanted, _ := json.Marshal(shown)
		t.Fatalf("with the file removed the answer is\n%s\nwant the whole answer again:\n%s", got, wanted)
	}

	if want := hiddenStateWithout(shown); !reflect.DeepEqual(hidden, want) {
		got, _ := json.Marshal(hidden)
		wanted, _ := json.Marshal(want)
		t.Fatalf("with the file the answer is\n%s\nwant the answer without it less pass_stands, ci and currency:\n%s", got, wanted)
	}
}
