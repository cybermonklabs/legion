package api

import (
	"encoding/json"
	"maps"
	"net/http"
	"testing"

	"github.com/google/uuid"
)

// Two finished issues whose step records each fall in one count of the read of their project.
// The first holds six records. Record 1 is answered by its set model A alone and record 2 made no
// call: both are on their set model. Record 3 is answered by A and B: off. Record 4 names A and
// does not know its counts. Record 5 holds only the fields a write before set_model and answers
// holds. Both are unknown. Record 6 is a merge with no agent, which no count holds. The second
// holds three records answered by A alone and one that names A and does not know its counts, so
// a read that swapped any two counts, or their names, answers it otherwise. The read answers
// set_model_steps {"on":2,"off":1,"unknown":2} for the first and {"on":3,"off":0,"unknown":1}
// for the second, read as the bytes the route sent.
func TestTheReadOfAProjectCountsStepsOnTheirSetModel(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)
	daemon := daemonSessionOf("ACME")
	// recordSteps writes one finished implementer run of issue for each entry of models, holding
	// its fields.
	recordSteps := func(issue string, models ...map[string]any) {
		t.Helper()
		for index, fields := range models {
			run := map[string]any{
				"actor": daemon, "task": "default", "step": index + 1, "kind": "step", "role": "implementer",
				"type": "implementation", "generation": 1, "agent": modelsAgent,
				"started_at": at(10 * index), "result": "passed", "ended_at": at(10*index + 5),
			}
			putStepOK(t, handler, issue, uuid.NewString(), withFields(run, fields))
		}
	}

	issue := createTestIssue(t, handler, "ACME", "Steps on their set model")
	recordSteps(issue,
		map[string]any{"set_model": modelA, "model": modelA, "answers": []map[string]any{modelCalls(modelA, 7)}},
		map[string]any{"set_model": modelA, "answers": []map[string]any{}},
		map[string]any{"set_model": modelA, "model": modelB, "answers": []map[string]any{modelCalls(modelA, 5), modelCalls(modelB, 2)}},
		map[string]any{"set_model": modelA, "model": modelA},
		map[string]any{"model": modelX},
	)
	putStepOK(t, handler, issue, uuid.NewString(), map[string]any{
		"actor": daemon, "task": "default", "step": 0, "kind": "merge", "role": "merger", "generation": 1,
		"result": "passed", "ended_at": at(60),
	})
	other := createAdvancingIssue(t, handler, map[string]any{"project": "ACME", "title": "Rotate the signing key"}, "alice").Key
	recordSteps(other,
		map[string]any{"set_model": modelA, "model": modelA, "answers": []map[string]any{modelCalls(modelA, 7)}},
		map[string]any{"set_model": modelA, "model": modelA, "answers": []map[string]any{modelCalls(modelA, 3)}},
		map[string]any{"set_model": modelA, "model": modelA, "answers": []map[string]any{modelCalls(modelA, 1)}},
		map[string]any{"set_model": modelA, "model": modelA},
	)
	closeIssue(t, handler, issue)
	closeIssue(t, handler, other)

	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/projects/ACME/plans", nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("read the plans of ACME: status=%d body=%s, want 200", response.Code, response.Body.String())
	}
	var read struct {
		Issues []map[string]json.RawMessage `json:"issues"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &read); err != nil {
		t.Fatalf("decode the plans of ACME: %v (%s)", err, response.Body.String())
	}
	got := map[string]string{}
	for _, listed := range read.Issues {
		var key string
		if err := json.Unmarshal(listed["issue"], &key); err != nil {
			t.Fatalf("decode an issue of the read of ACME: %v (%s)", err, response.Body.String())
		}
		got[key] = string(listed["set_model_steps"])
	}
	want := map[string]string{issue: `{"on":2,"off":1,"unknown":2}`, other: `{"on":3,"off":0,"unknown":1}`}
	if !maps.Equal(got, want) {
		t.Fatalf("the read of ACME carries set_model_steps %v, want %v (%s)", got, want, response.Body.String())
	}
}
