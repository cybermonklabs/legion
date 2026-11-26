package answer

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/dispatchtool"
)

// While the project is paused the loop's tools run each read and refuse each write as the call's
// result, an error naming the pause, with neither tool handed the write: a gh comment and a
// dispatch comment are refused, a gh read and a dispatch read run, and a web search, which is a
// read, runs. The run goes on to its reply, and the system prompt says the project is paused. The
// control is the same calls while the project runs: every one runs, and the prompt says nothing of
// a pause.
func TestAPauseRefusesEachWriteOfTheToolsAndRunsEachRead(t *testing.T) {
	for _, paused := range []bool{true, false} {
		t.Run(fmt.Sprintf("paused %t", paused), func(t *testing.T) {
			s := newStandIn(t,
				anthropicStream("tool_use", anthropicUsage{Input: 10, Output: 5},
					anthropicGH("toolu_1", "pr", "comment", "7", "--body", "Done."),
					anthropicGH("toolu_2", "pr", "view", "7"),
					anthropicDispatch("toolu_3", "", "comment", "--issue", "INFRA-1", "--body", "Done."),
					anthropicDispatch("toolu_4", "", "read", "--issue", "INFRA-1"),
					anthropicWebSearch("toolu_5", "when do plums ripen")),
				anthropicStream("end_turn", anthropicUsage{Input: 12, Output: 6}, anthropicText(`{"kind":"answered","text":"PR 7 is open.","needs":""}`)),
			)
			gh, dispatch, search := &stubGH{}, &stubDispatch{}, &stubWebSearch{}
			req := anthropicRequest(t, s, gh, 3)
			req.Dispatch, req.WebSearch = dispatch, search
			req.Paused = func() bool { return paused }
			reply, err := Answer(context.Background(), req)
			if err != nil || reply.Kind != Answered || reply.Calls != 2 {
				t.Fatalf("reply %+v, %v; want the run's two model calls to answer", reply, err)
			}

			ghRan := []string{`["pr","comment","7","--body","Done."]`, `["pr","view","7"]`}
			dispatchRan := []string{`["comment","--issue","INFRA-1","--body","Done."]`, `["read","--issue","INFRA-1"]`}
			if paused {
				ghRan, dispatchRan = ghRan[1:], dispatchRan[1:]
			}
			var ran []string
			for _, call := range gh.made() {
				ran = append(ran, call.Args)
			}
			if slices.Sort(ran); !slices.Equal(ran, ghRan) {
				t.Errorf("the gh tool ran %q; want %q", ran, ghRan)
			}
			ran = nil
			for _, call := range dispatch.made() {
				ran = append(ran, call.Args)
			}
			if slices.Sort(ran); !slices.Equal(ran, dispatchRan) {
				t.Errorf("the dispatch tool ran %q; want %q", ran, dispatchRan)
			}
			if searches := search.made(); len(searches) != 1 {
				t.Errorf("the web_search tool ran %+v; want the one search", searches)
			}

			sent := messages(t, s.requests()[1])
			results := map[string]map[string]any{}
			for _, b := range blocks(sent[len(sent)-1]) {
				if id, _ := b["tool_use_id"].(string); b["type"] == "tool_result" {
					results[id] = b
				}
			}
			for _, id := range []string{"toolu_1", "toolu_2", "toolu_3", "toolu_4", "toolu_5"} {
				result, ok := results[id]
				if !ok {
					t.Fatalf("the second call carries no result for %s: %v", id, results)
				}
				content, _ := json.Marshal(result["content"])
				refused := paused && (id == "toolu_1" || id == "toolu_3")
				if named := strings.Contains(string(content), "the Legion project is paused"); named != refused || (refused && result["is_error"] != true) {
					t.Errorf("%s reached the model as %s (error %v); want refused for the pause %t", id, content, result["is_error"], refused)
				}
			}
			if system := offerOf(t, s.requests()[0]).System[0].Text; strings.Contains(system, "Legion has paused your project") != paused {
				t.Errorf("the system prompt, the project paused %t:\n%s", paused, system)
			}
		})
	}
}

// While the project is paused the system prompt names each write its tools refuse, each of the
// dispatch tool's writes among them (dispatchtool.WriteCommands), which the pause refuses all.
func TestThePausedPromptNamesEachDispatchWriteThePauseRefuses(t *testing.T) {
	req := anthropicRequest(t, newStandIn(t), &stubGH{}, 3)
	req.Dispatch = &stubDispatch{}
	req.Paused = func() bool { return true }
	prompt := systemPrompt(req)
	_, refused, _ := strings.Cut(prompt, "refuse every write (")
	refused, _, _ = strings.Cut(refused, ") as the call's result")
	for _, write := range dispatchtool.WriteCommands {
		if !strings.Contains(refused, write) {
			t.Errorf("the paused prompt's refused writes (%s) leave out dispatch %s", refused, write)
		}
	}
}
