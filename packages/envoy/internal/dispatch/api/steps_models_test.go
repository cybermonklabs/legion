package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
)

// The models of a step record. set_model is the model its role set its agent to. answers is each
// model that answered the agent's calls during the step, with its count of calls. model stays the
// model that answered last. The Legion daemon writes both at the step's end. The read gives "" for
// no set model, null for counts the record does not know, and [] for a step with no call. The rows
// below read each record's members as the bytes the route sent.

// The models the rows below name: A is the set model, B another model that answered, and X the
// last model of a record that names neither a set model nor its answers.
const (
	modelA = "anthropic/claude-opus-5-5"
	modelB = "anthropic/claude-opus-5"
	modelX = "anthropic/claude-sonnet-4-5"
)

// modelsAgent is the agent of each run the rows below write.
const modelsAgent = "legion-acme-acme-1-implementer"

// modelCalls is one entry of a step write's answers: a model and its count of calls.
func modelCalls(model string, calls int) map[string]any {
	return map[string]any{"model": model, "calls": calls}
}

// rawStepRecords reads the steps of issue as a person reads them and answers each record by its
// id, each member as the bytes the route sent.
func rawStepRecords(t *testing.T, handler http.Handler, issue string) map[string]map[string]json.RawMessage {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue+"/steps", nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("read the steps of %s: status=%d body=%s, want 200", issue, response.Code, response.Body.String())
	}
	var read struct {
		Tasks []struct {
			Steps []map[string]json.RawMessage `json:"steps"`
		} `json:"tasks"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &read); err != nil {
		t.Fatalf("decode the steps of %s: %v (%s)", issue, err, response.Body.String())
	}
	records := map[string]map[string]json.RawMessage{}
	for _, task := range read.Tasks {
		for _, record := range task.Steps {
			var id string
			if err := json.Unmarshal(record["id"], &id); err != nil {
				t.Fatalf("decode the id of a step of %s: %v (%s)", issue, err, response.Body.String())
			}
			records[id] = record
		}
	}
	return records
}

// Five records of one issue, each written under its own id at its start and at its end, read
// back. Record 1 holds only the fields a write before set_model and answers holds, and names the
// model X. Record 2 still runs. Records 3 to 5 ended on the set model A. Record 3 has no counts.
// A answered record 4 41 times and B answered it 3 times, in that order. Record 5 made no call.
// The read gives records 1 and 2 no set model and null counts, and keeps X as record 1's model. It
// gives record 5 the empty list, not null.
func TestAStepRecordHoldsItsSetModelAndAnswers(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)
	issue := createTestIssue(t, handler, "ACME", "A step record's models")
	daemon := daemonSessionOf("ACME")

	type written struct {
		name string
		// end is what the run's end states beside its identity, its result and its moment. It is
		// nil for a run that has not ended.
		end  map[string]any
		want map[string]string
	}
	records := []written{
		{"record 1, which names its last model alone", map[string]any{"model": modelX, "session": "ses_record_one"},
			map[string]string{"set_model": `""`, "answers": `null`, "model": `"` + modelX + `"`}},
		{"record 2, which still runs", nil,
			map[string]string{"set_model": `""`, "answers": `null`, "ended_at": `null`, "result": `""`}},
		{"record 3, which does not know its counts", map[string]any{"set_model": modelA},
			map[string]string{"set_model": `"` + modelA + `"`, "answers": `null`}},
		{"record 4, which A and B answered", map[string]any{"set_model": modelA, "model": modelB,
			"answers": []map[string]any{modelCalls(modelA, 41), modelCalls(modelB, 3)}},
			map[string]string{"set_model": `"` + modelA + `"`, "model": `"` + modelB + `"`,
				"answers": `[{"model":"` + modelA + `","calls":41},{"model":"` + modelB + `","calls":3}]`}},
		{"record 5, which made no call", map[string]any{"set_model": modelA, "answers": []map[string]any{}},
			map[string]string{"set_model": `"` + modelA + `"`, "answers": `[]`}},
	}
	ids := make([]string, len(records))
	for index, record := range records {
		ids[index] = uuid.NewString()
		started := stepStart.Add(time.Duration(index) * time.Hour)
		identity := stepIdentity(daemon)
		putStepOK(t, handler, issue, ids[index], withFields(identity, map[string]any{"agent": modelsAgent, "started_at": started}))
		if record.end != nil {
			end := withFields(identity, map[string]any{"result": "passed", "ended_at": started.Add(25 * time.Minute)})
			putStepOK(t, handler, issue, ids[index], withFields(end, record.end))
		}
	}

	read := rawStepRecords(t, handler, issue)
	if len(read) != len(records) {
		t.Fatalf("the read of %s lists %d records, want %d", issue, len(read), len(records))
	}
	for index, record := range records {
		held, listed := read[ids[index]]
		if !listed {
			t.Fatalf("the read of %s does not list %s, %s", issue, record.name, ids[index])
		}
		for member, want := range record.want {
			got, carried := held[member]
			if !carried {
				t.Errorf("%s carries no %s, want %s", record.name, member, want)
				continue
			}
			if string(got) != want {
				t.Errorf("%s carries %s %s, want %s", record.name, member, got, want)
			}
		}
	}
}

// Each write whose answers break a rule of a step record's answers is refused 400 STEP_INPUT,
// naming the rule, and writes nothing. The rows break each rule: 17 entries, a model with 0 calls,
// a model listed twice and a model that is no agent's text. A set model that is no agent's text is
// refused as the agent, the session and the model are.
func TestAStepWriteRefusesAnswersOutsideTheirRules(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)
	issue := createTestIssue(t, handler, "ACME", "A step record's answers")
	end := withFields(stepIdentity(daemonSessionOf("ACME")), map[string]any{
		"agent": modelsAgent, "result": "passed", "ended_at": stepEnd, "model": modelA, "set_model": modelA,
	})
	seventeen := make([]map[string]any, 17)
	for index := range seventeen {
		seventeen[index] = modelCalls(fmt.Sprintf("anthropic/model-%d", index+1), 1)
	}

	for _, row := range []struct {
		name, refusal string
		body          map[string]any
	}{
		{"17 entries", "answers holds 17 entries, more than the 16 a step record holds",
			withFields(end, map[string]any{"answers": seventeen})},
		{"a count of 0", "answers entry 2 (anthropic/x) has 0 calls: a model that answered has 1 or more",
			withFields(end, map[string]any{"answers": []map[string]any{modelCalls("anthropic/y", 2), modelCalls("anthropic/x", 0)}})},
		{"a model listed twice", "answers names anthropic/x twice: each model is listed once with all its calls",
			withFields(end, map[string]any{"answers": []map[string]any{modelCalls("anthropic/x", 1), modelCalls("anthropic/x", 2)}})},
		{"a model with a space", `answers entry 1 names "a b", which is not an agent's text: printable ASCII with no space, from 1 to 128 characters`,
			withFields(end, map[string]any{"answers": []map[string]any{modelCalls("a b", 1)}})},
		{"a set model with a space", `set_model "a b" is not an agent's text: printable ASCII with no space, from 1 to 128 characters`,
			withFields(end, map[string]any{"set_model": "a b", "answers": []map[string]any{}})},
	} {
		t.Run(row.name, func(t *testing.T) {
			id := uuid.NewString()
			expectRefusal(t, row.name, putStep(t, handler, issue, id, row.body), http.StatusBadRequest, codeStepInput, row.refusal)
			if held, listed := rawStepRecords(t, handler, issue)[id]; listed {
				t.Fatalf("the refused write of %s left the record %s, want none", row.name, asJSON(t, held))
			}
		})
	}
	if read := rawStepRecords(t, handler, issue); len(read) != 0 {
		t.Fatalf("the refused writes left %d records, want none", len(read))
	}
}
