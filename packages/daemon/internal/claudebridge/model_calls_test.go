package claudebridge

import (
	"testing"
)

// framesThrough is each frame the bridge writes for a replayed stream, up to and with the nth frame
// of type last. Pane summaries and names of models are included. The gate is open, so the replay
// writes its last line too.
func framesThrough(r *rig, n int, last string) []map[string]any {
	r.t.Helper()
	r.open()
	var frames []map[string]any
	for seen := 0; seen < n; {
		frame := r.nextRaw()
		frames = append(frames, frame)
		if frame["type"] == last {
			seen++
		}
	}
	return frames
}

// setModelsOf is the models that the legion_set_model frames among frames name, as provider/id.
// before is whether the first of them comes before every message_end.
func setModelsOf(frames []map[string]any) (models []string, before bool) {
	ended := false
	for _, frame := range frames {
		switch frame["type"] {
		case "message_end":
			ended = true
		case "legion_set_model":
			if len(models) == 0 {
				before = !ended
			}
			provider, _ := frame["provider"].(string)
			id, _ := frame["id"].(string)
			models = append(models, provider+"/"+id)
		}
	}
	return models, before
}

// answersOf is the models that the message_end frames among frames name, as provider/model, one for
// each frame that names a model.
func answersOf(frames []map[string]any) []string {
	var models []string
	for _, frame := range frames {
		if !answerFrame(frame) {
			continue
		}
		message, _ := frame["message"].(map[string]any)
		provider, _ := message["provider"].(string)
		model, _ := message["model"].(string)
		models = append(models, provider+"/"+model)
	}
	return models
}

// holdsSetModel fails t unless frames name one set model, want, before any message_end.
func holdsSetModel(t *testing.T, frames []map[string]any, want string) {
	t.Helper()
	models, before := setModelsOf(frames)
	switch {
	case len(models) != 1 || models[0] != want:
		t.Errorf("the bridge named the set models %q in %d legion_set_model frames, want one naming %s", models, len(models), want)
	case !before:
		t.Errorf("the bridge named the set model %s after a message_end, want it before every message_end", want)
	}
}

// background-turn.jsonl is a recording of Claude Code: two turns, with three responses in seven
// assistant lines. Claude Code writes one assistant line for each content block of a response, and
// each line carries the response's id. So the shim is told of three answers, one for each
// response. Its first init line names the model the launch runs on.
func TestTheBridgeEndsEachResponseOfBackgroundTurnOnce(t *testing.T) {
	r := newRig(t, "-p", "--resume", sessionID)
	r.prompt("p1", backgroundTurn)
	frames := framesThrough(r, 2, "agent_end")
	holdsSetModel(t, frames, "anthropic/claude-sonnet-4-5")
	answers := answersOf(frames)
	if len(answers) != 3 {
		t.Errorf("%d message_end frames name a model, want 3, one for each response: %q", len(answers), answers)
	}
	for _, model := range answers {
		if model != "anthropic/claude-sonnet-4-5" {
			t.Errorf("a message_end names %s, want anthropic/claude-sonnet-4-5", model)
		}
	}
}

// refusal-turn.jsonl is a recording of Claude Code against a stand-in endpoint. A model wrote one
// response, then refused, and Claude Code wrote its own answer, whose model is `<synthetic>`. Only
// the response a model wrote is an answer. The result's error message_end names no model.
func TestARefusalTurnEndsOnlyTheResponseAModelWrote(t *testing.T) {
	r := newRig(t, "-p", "--resume", sessionID)
	r.prompt("p1", "replay refusal-turn.jsonl")
	frames := framesThrough(r, 1, "prompt_result")
	holdsSetModel(t, frames, "anthropic/claude-sonnet-4-5")
	if answers := answersOf(frames); len(answers) != 1 || answers[0] != "anthropic/claude-sonnet-4-5" {
		t.Errorf("the message_end frames name %q, want one naming anthropic/claude-sonnet-4-5 and none naming <synthetic>", answers)
	}
	failures := 0
	for _, frame := range frames {
		message, _ := frame["message"].(map[string]any)
		if frame["type"] != "message_end" || message["stopReason"] != "error" {
			continue
		}
		failures++
		if _, named := message["model"]; named {
			t.Errorf("the result's error message_end %v names a model, want none", frame)
		}
	}
	if failures != 1 {
		t.Errorf("%d error message_end frames, want 1 for the result", failures)
	}
}

// subagent-turn.jsonl is captured, not made: the stdout of the pinned Claude Code (2.1.311) in one
// print-mode session against a stand-in endpoint. Its init lines name the set model, claude-model.
// The endpoint served claude-served-model for every response, so the set model never answered. Its
// list of what it served is scripts/e2e/lib/testdata/step-models/capture-differ-served.json. It
// names four responses of the main conversation, msg_standin_81 to 83 and msg_standin_85. Two
// assistant lines carry msg_standin_81. It also names msg_standin_84, the response of a subagent.
// That line names the tool call it serves in parent_tool_use_id. A subagent's line is no answer
// of the agent's own conversation, so it ends no response. So the shim is told of four answers.
func TestASubagentLineEndsNoResponse(t *testing.T) {
	r := newRig(t, "-p", "--resume", sessionID)
	r.prompt("p1", "replay subagent-turn.jsonl")
	frames := framesThrough(r, 1, "prompt_result")
	holdsSetModel(t, frames, "anthropic/claude-model")
	answers := answersOf(frames)
	if len(answers) != 4 {
		t.Errorf("%d message_end frames name a model, want 4, one for each response of the main conversation and none for the subagent's: %q", len(answers), answers)
	}
	for _, model := range answers {
		if model != "anthropic/claude-served-model" {
			t.Errorf("a message_end names %s, want anthropic/claude-served-model, the model that served each response", model)
		}
	}
}

// background-during-prompt.jsonl is a recording of Claude Code: one turn, with two responses in six
// assistant lines. Claude Code ran the first tool call of the first response before it wrote that
// response's last line. So a tool result comes between two lines of one response, which still ends
// once.
func TestAResponseThatAToolResultSplitsEndsOnce(t *testing.T) {
	r := newRig(t, "-p", "--resume", sessionID)
	r.prompt("p1", backgroundDuringPrompt)
	if answers := answersOf(framesThrough(r, 1, "prompt_result")); len(answers) != 2 {
		t.Errorf("%d message_end frames name a model, want 2, one for each response: %q", len(answers), answers)
	}
}

// command-init-first.jsonl is made by hand from the recorded lines of command-block.jsonl, not
// captured. The command's block is moved first, and its init names a stand-in model. Its paths and
// the plugin's name are stand-ins too. A command's init names the model the launch runs on. So the
// first init names the set model even when it is a command's, and the turn's init after it does not.
func TestACommandsInitBeforeAnyTurnNamesTheSetModel(t *testing.T) {
	r := newRig(t, "-p", "--resume", sessionID)
	r.prompt("p1", "replay command-init-first.jsonl")
	holdsSetModel(t, framesThrough(r, 1, "prompt_result"), "anthropic/claude-command-stand-in")
}
