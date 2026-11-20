package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
)

// results_state_hidden.go is the hide-state switch of an end-to-end test process
// (Deps.HideStateFile, cmd/dispatch's DISPATCH_TEST_HIDE_STATE_FILE, under the test hooks). While
// the file it names exists, the results read leaves out of its answer the state the server
// computes from the results: each check's pass_stands (the answer of the whole check and the state
// of each head) and ci (the read of a check CI proves), and each result's currency. Every other
// member stays as the answer writes it, so an agent the skill-scenario rig runs reads each
// result's commit, role, verdict, time and where its commit stands, and judges the check from those
// alone. Nothing the server computes or stores changes; only this one answer's body does. A
// deployment names no file, and its reads pay one comparison.

// hiddenCheckState is what the switch leaves out of each check, and hiddenResultState what it
// leaves out of each result of the check's results and recorded_results (hiddenResultLists).
var (
	hiddenCheckState  = []string{"pass_stands", "ci"}
	hiddenResultState = []string{"currency"}
	hiddenResultLists = []string{"results", "recorded_results"}
)

// stateHidden reports whether the hide-state file exists now. The rig creates it after its own
// reads, before its agent starts, and removes it when the agent has ended.
func (s *server) stateHidden() bool {
	if s.deps.HideStateFile == "" {
		return false
	}
	_, err := os.Stat(s.deps.HideStateFile)
	return err == nil
}

// withoutComputedState is body as the results read writes it, with hiddenCheckState left out of
// each check and hiddenResultState out of each result, and every other member kept as written,
// its numbers too.
func withoutComputedState(body issueCheckResults) (map[string]any, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("encode the results answer: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var answer map[string]any
	if err := decoder.Decode(&answer); err != nil {
		return nil, fmt.Errorf("decode the results answer: %w", err)
	}
	checks, _ := answer["checks"].([]any)
	for _, item := range checks {
		check, ok := item.(map[string]any)
		if !ok {
			continue
		}
		for _, member := range hiddenCheckState {
			delete(check, member)
		}
		for _, list := range hiddenResultLists {
			results, _ := check[list].([]any)
			for _, entry := range results {
				if result, ok := entry.(map[string]any); ok {
					for _, member := range hiddenResultState {
						delete(result, member)
					}
				}
			}
		}
	}
	return answer, nil
}
