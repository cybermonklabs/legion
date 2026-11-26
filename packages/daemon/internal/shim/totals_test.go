package shim_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/shimwire"
)

// The models of these tests, as the shim names them: the provider, a slash and the model.
const (
	modelA = "acme/model-a"
	modelB = "acme/model-b"
)

// setModelA is a launch whose ready answer names model A before any call.
var setModelA = []string{"ready", "await-get-state", "answer-held " + modelA}

// callsOfA2B1 are the responses of a launch. Two are by A, and one is by B as bEnd writes it. One
// more by A ended in an error, which counts for no model.
func callsOfA2B1(bEnd string) []string {
	return []string{"end acme model-a", "end acme model-a", bEnd, "end acme model-a error"}
}

var countsA2B1 = []shimwire.ModelCount{{Model: modelA, Calls: 2}, {Model: modelB, Calls: 1}}

// expectFrames reads the next whole frames, which must be of these types in order: an unmodelled
// frame by its own type member. It returns them.
func (p *peer) expectFrames(t *testing.T, types ...string) []shimwire.Frame {
	t.Helper()
	frames := make([]shimwire.Frame, len(types))
	for i, want := range types {
		frames[i] = p.nextWhole(t)
		if got := typeOf(frames[i]); got != want {
			t.Fatalf("frame %d from the shim is a %s, want a %s: %#v", i, got, want, frames[i])
		}
	}
	return frames
}

func typeOf(frame shimwire.Frame) string {
	if raw, ok := frame.(shimwire.Raw); ok {
		return raw.Type
	}
	return frame.FrameType()
}

// readTotals is the daemon's read of the shim's totals: it sends model_totals with id and reads
// whole frames up to its answer. It returns the answer, which must be valid, and the frames
// before it.
func (p *peer) readTotals(t *testing.T, id string) (shimwire.ModelTotalsResult, []shimwire.Frame) {
	t.Helper()
	p.send(t, shimwire.ModelTotals{ID: id})
	return p.awaitResult(t, id)
}

// awaitResult reads whole frames up to the answer to the read id.
func (p *peer) awaitResult(t *testing.T, id string) (shimwire.ModelTotalsResult, []shimwire.Frame) {
	t.Helper()
	var before []shimwire.Frame
	for {
		frame := p.nextWhole(t)
		result, ok := frame.(shimwire.ModelTotalsResult)
		if !ok || result.ID != id {
			before = append(before, frame)
			continue
		}
		if err := result.Validate(); err != nil {
			t.Fatalf("the shim answered the read %s with a frame the daemon refuses: %v", id, err)
		}
		return result, before
	}
}

// expectTotals checks a read's answer: its totals, given whole.
func expectTotals(t *testing.T, result shimwire.ModelTotalsResult, want shimwire.Totals) {
	t.Helper()
	if !result.OK || result.Totals == nil {
		t.Fatalf("the shim refused the read %s: %q", result.ID, result.Error)
	}
	if !reflect.DeepEqual(*result.Totals, want) {
		t.Fatalf("the read %s gave %+v, want %+v", result.ID, *result.Totals, want)
	}
}

// messageModel is the model a whole message_end names, and its size.
func messageModel(t *testing.T, frame shimwire.Frame) (string, int) {
	t.Helper()
	raw, ok := frame.(shimwire.Raw)
	if !ok || raw.Type != "message_end" {
		t.Fatalf("the frame %#v is no message_end", frame)
	}
	var end struct {
		Message struct{ Provider, Model string } `json:"message"`
	}
	if err := json.Unmarshal(raw.JSON, &end); err != nil {
		t.Fatalf("decode a message_end: %v", err)
	}
	return end.Message.Provider + "/" + end.Message.Model, len(raw.JSON)
}

// The shim counts each call of its launch by the model that answered it, and answers the
// daemon's reads of those totals. A response that ended in an error counts for no model, and so
// do a response no model wrote and a message that is not the assistant's. A name outside the form
// of a step's agent text is counted apart. A call that arrives as an rpc_chunk sequence counts once
// the sequence is whole. A read that arrives while half of one is in the pipe is answered after all
// of it. The set model is the one the harness's ready answer names, unless a call came before that
// answer. The Claude Code bridge names it in a frame that goes no further than the shim. When the
// harness exits, the shim sends its final totals. A launch that more models answered than the
// totals hold (shimwire.MaxTotalsModels) says that its totals overflowed.
//
// The shim writes each frame of its own only between whole frames of the harness. A frame that
// lands inside an rpc_chunk sequence makes the daemon's reader drop the sequence
// (internal/stream's read). The stub reads as that reader does (peer.nextWhole).
func TestTheShimCountsEachCallOfItsLaunchByModel(t *testing.T) {
	const twoMiB = "2097152"
	for _, row := range []struct {
		name   string
		script []string
		drive  func(t *testing.T, sh *running, p *peer, child *omp)
	}{
		{
			name:   "1_the_ready_answer_names_the_set_model_and_each_call_counts",
			script: slices.Concat(setModelA, callsOfA2B1("end acme model-b")),
			drive: func(t *testing.T, _ *running, p *peer, _ *omp) {
				p.expectFrames(t, "ready", "message_end", "message_end", "message_end", "message_end")
				result, _ := p.readTotals(t, "read-1")
				expectTotals(t, result, shimwire.Totals{Read: 1, SetModel: modelA, Counts: countsA2B1})
			},
		},
		{
			name:   "2_a_call_of_2_MiB_counts_once_its_chunk_sequence_is_whole",
			script: slices.Concat(setModelA, callsOfA2B1("end-chunked acme model-b "+twoMiB)),
			drive: func(t *testing.T, _ *running, p *peer, _ *omp) {
				frames := p.expectFrames(t, "ready", "message_end", "message_end", "message_end", "message_end")
				if model, size := messageModel(t, frames[3]); model != modelB || size != 2<<20 {
					t.Fatalf("the daemon's reader got a message_end of %d bytes by %s, want the 2 MiB one by %s", size, model, modelB)
				}
				result, _ := p.readTotals(t, "read-1")
				expectTotals(t, result, shimwire.Totals{Read: 1, SetModel: modelA, Counts: countsA2B1})
			},
		},
		{
			name: "3_a_read_while_half_a_sequence_is_written_is_answered_after_all_of_it",
			script: []string{
				"end acme model-a", "end acme model-a", "end-chunked-half acme model-b " + twoMiB,
				"await-get-state", "end-chunked-rest", "end acme model-a error", "answer-held " + modelA,
			},
			drive: func(t *testing.T, _ *running, p *peer, child *omp) {
				p.expectFrames(t, "message_end", "message_end")
				child.awaitProgress(t, 3)
				result, before := p.readTotals(t, "read-1")
				if got := len(before); got != 2 {
					t.Fatalf("the daemon's reader got %d frames before the read's answer, want the chunked message_end and the error: %#v", got, before)
				}
				if model, size := messageModel(t, before[0]); model != modelB || size != 2<<20 {
					t.Fatalf("the daemon's reader got a message_end of %d bytes by %s, want the 2 MiB one by %s", size, model, modelB)
				}
				expectTotals(t, result, shimwire.Totals{Read: 1, Counts: countsA2B1})
			},
		},
		{
			name:   "4_a_call_before_the_ready_answer_leaves_the_set_model_unknown",
			script: []string{"ready", "await-get-state", "end acme model-a", "answer-held " + modelA},
			drive: func(t *testing.T, _ *running, p *peer, child *omp) {
				p.expectFrames(t, "ready", "message_end")
				child.awaitProgress(t, 4)
				result, _ := p.readTotals(t, "read-1")
				expectTotals(t, result, shimwire.Totals{Read: 1, Counts: []shimwire.ModelCount{{Model: modelA, Calls: 1}}})
			},
		},
		{
			name:   "5_the_last_frame_after_the_harness_exits_is_its_final_totals",
			script: slices.Concat(setModelA, callsOfA2B1("end acme model-b"), []string{"exit 0"}),
			drive: func(t *testing.T, sh *running, p *peer, _ *omp) {
				var last shimwire.Frame
				for {
					frame, ok := p.readWhole(t)
					if !ok {
						break
					}
					last = frame
				}
				final, ok := last.(shimwire.ModelTotalsFinal)
				if !ok {
					t.Fatalf("the last frame the shim sent is %#v, want its final totals", last)
				}
				if err := final.Validate(); err != nil {
					t.Fatalf("the final totals are a frame the daemon refuses: %v", err)
				}
				if want := (shimwire.Totals{Read: 1, SetModel: modelA, Counts: countsA2B1}); !reflect.DeepEqual(final.Totals, want) {
					t.Fatalf("the final totals are %+v, want %+v", final.Totals, want)
				}
				if code := sh.wait(t); code != 0 {
					t.Fatalf("the shim exited %d, want the harness's 0", code)
				}
			},
		},
		{
			name:   "6_a_65th_model_overflows_the_totals",
			script: []string{"models 65"},
			drive: func(t *testing.T, _ *running, p *peer, _ *omp) {
				p.expectFrames(t, slices.Repeat([]string{"message_end"}, 65)...)
				result, _ := p.readTotals(t, "read-1")
				if !result.OK || result.Totals == nil {
					t.Fatalf("the shim refused the read: %q", result.Error)
				}
				if got := result.Totals; !got.Overflowed || len(got.Counts) != 64 || got.OutsideForm != 0 {
					t.Fatalf("the read gave %+v, want 64 models counted and the totals overflowed", *got)
				}
			},
		},
		{
			// The half of the sequence is 16 chunks, about 5.6 MB of lines. That is more than a
			// pipe holds. So the shim has read its first chunk once the fake has written the half.
			// The pump writes the sequence only once it is whole, so the refusal goes before it.
			name: "7_a_refusal_while_half_a_sequence_is_held_goes_before_the_whole_sequence",
			script: slices.Concat(
				[]string{"end-chunked-half acme model-b 8388608"},
				slices.Repeat([]string{"await-get-state"}, 16),
				[]string{"await-go", "end-chunked-rest"},
				slices.Repeat([]string{"answer-held " + modelA}, 16),
			),
			drive: func(t *testing.T, _ *running, p *peer, child *omp) {
				child.awaitProgress(t, 1)
				for n := 1; n <= 16; n++ {
					p.send(t, shimwire.ModelTotals{ID: fmt.Sprintf("read-%02d", n)})
				}
				p.send(t, shimwire.ModelTotals{ID: "read-17"})
				// The fake writes the rest of its sequence only once the shim has handled the 17th
				// read: the shim forwards this line after it.
				p.sendLine(t, `{"type":"fake_go"}`)
				frame := p.nextWhole(t)
				if refused, ok := frame.(shimwire.ModelTotalsResult); !ok || refused.ID != "read-17" || refused.OK || refused.Error != "16 reads already wait for the harness" {
					t.Fatalf("the shim's first frame is %#v, want the refusal of read-17, written before the sequence is whole", frame)
				}
				if model, size := messageModel(t, p.nextWhole(t)); model != modelB || size != 8<<20 {
					t.Fatalf("the daemon's reader got a message_end of %d bytes by %s after the refusal, want the 8 MiB one by %s", size, model, modelB)
				}
				for n := 1; n <= 16; n++ {
					result, before := p.awaitResult(t, fmt.Sprintf("read-%02d", n))
					if len(before) != 0 {
						t.Fatalf("frames %#v came before the answer to read %d", before, n)
					}
					expectTotals(t, result, shimwire.Totals{Read: uint64(n), Counts: []shimwire.ModelCount{{Model: modelB, Calls: 1}}})
				}
			},
		},
		{
			// The wire's validation takes a missing list as no calls, so the shim always sends
			// the list, empty where it counted nothing.
			name:   "8_a_read_before_any_call_sends_an_empty_list",
			script: setModelA,
			drive: func(t *testing.T, _ *running, p *peer, child *omp) {
				p.expectFrames(t, "ready")
				child.awaitProgress(t, 3)
				result, _ := p.readTotals(t, "read-1")
				if !bytes.Contains(p.last, []byte(`"counts":[]`)) {
					t.Fatalf("the read's answer is %s, want its counts as an empty list", p.last)
				}
				expectTotals(t, result, shimwire.Totals{Read: 1, SetModel: modelA, Counts: []shimwire.ModelCount{}})
			},
		},
		{
			// A name of 134 characters is longer than the 128 of the form. No list names it.
			name:   "9_a_call_by_a_name_outside_the_form_counts_apart",
			script: slices.Concat(setModelA, []string{"end acme model-a", "end acme " + strings.Repeat("x", 129)}),
			drive: func(t *testing.T, _ *running, p *peer, _ *omp) {
				p.expectFrames(t, "ready", "message_end", "message_end")
				result, _ := p.readTotals(t, "read-1")
				expectTotals(t, result, shimwire.Totals{Read: 1, SetModel: modelA, Counts: []shimwire.ModelCount{{Model: modelA, Calls: 1}}, OutsideForm: 1})
			},
		},
		{
			name: "10_a_synthetic_response_and_a_users_message_count_for_no_model",
			script: slices.Concat(setModelA, []string{
				"end acme model-a", "end acme " + shimwire.SyntheticModel, "end-as user acme model-a",
			}),
			drive: func(t *testing.T, _ *running, p *peer, _ *omp) {
				p.expectFrames(t, "ready", "message_end", "message_end", "message_end")
				result, _ := p.readTotals(t, "read-1")
				expectTotals(t, result, shimwire.Totals{Read: 1, SetModel: modelA, Counts: []shimwire.ModelCount{{Model: modelA, Calls: 1}}})
			},
		},
		{
			// The daemon's reader sees the message_end alone: the bridge's frame stops at the shim.
			name:   "11_the_bridges_frame_names_the_set_model_and_goes_no_further",
			script: []string{"set-model acme model-b", "end acme model-a"},
			drive: func(t *testing.T, _ *running, p *peer, _ *omp) {
				p.expectFrames(t, "message_end")
				result, _ := p.readTotals(t, "read-1")
				expectTotals(t, result, shimwire.Totals{Read: 1, SetModel: modelB, Counts: []shimwire.ModelCount{{Model: modelA, Calls: 1}}})
			},
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			path := socketPath(t)
			daemon := listen(t, path)
			child := scripted(t, modelA, row.script...)
			sh := run(t, config(t, path, child), newClock())
			p := daemon.accept(t)
			p.open(t)
			p.readAhead(t)
			row.drive(t, sh, p, child)
		})
	}
}

// The get_state the shim sends the harness itself is the shim's: the harness's answer to it
// reaches neither the daemon nor the dedupe. Each read of the totals and the ready frame send one.
func TestTheShimKeepsTheAnswersToItsOwnGetState(t *testing.T) {
	path := socketPath(t)
	daemon := listen(t, path)
	child := scripted(t, modelA, slices.Concat(setModelA, []string{"end acme model-a"})...)
	// Oh My Pi's state answer holds its system prompt and tool schemas, so it can be chunked. Each
	// read's answer here is: the shim must keep every chunk of it from the daemon.
	child.env = append(child.env, "FAKE_OMP_STATE_BYTES=2097152")
	run(t, config(t, path, child), newClock())
	p := daemon.accept(t)
	p.open(t)
	p.readAhead(t)

	child.awaitProgress(t, 4)
	var seen []shimwire.Frame
	for n, id := range []string{"read-1", "read-2"} {
		result, before := p.readTotals(t, id)
		expectTotals(t, result, shimwire.Totals{Read: uint64(n + 1), SetModel: modelA, Counts: []shimwire.ModelCount{{Model: modelA, Calls: 1}}})
		seen = append(seen, before...)
	}
	for _, frame := range seen {
		if response, ok := frame.(shimwire.Response); ok && strings.HasPrefix(response.ID, "legion-shim-") {
			t.Fatalf("the harness's answer to the shim's own get_state reached the daemon: %#v", response)
		}
	}
	if len(seen) != 2 || typeOf(seen[0]) != "ready" || typeOf(seen[1]) != "message_end" {
		t.Fatalf("the daemon's reader got %#v before the answers, want the harness's ready and message_end alone", seen)
	}
	var own []string
	for _, frame := range child.received(t) {
		if state, ok := frame.(shimwire.GetState); ok && strings.HasPrefix(state.ID, "legion-shim-") {
			own = append(own, state.ID)
		}
	}
	if len(own) != 3 {
		t.Fatalf("the harness read %d get_state frames of the shim's own (%v), want one for the ready frame and one for each of the two reads", len(own), own)
	}
}

// A shim that counts its launch's calls says so in its hello. So the daemon reads its totals, and
// does not wait for an answer from a shim that gives none.
func TestTheHelloSaysTheShimCountsModels(t *testing.T) {
	path := socketPath(t)
	daemon := listen(t, path)
	run(t, config(t, path, newOMP(t)), newClock())
	p := daemon.accept(t)

	frame := p.next(t)
	if hello, ok := frame.(shimwire.Hello2); !ok || hello.BootToken != bootToken || !hello.ModelTotals {
		t.Fatalf("the shim's first frame is %#v, want a hello2 with its boot token and modelTotals", frame)
	}
}

// The dedupe reads whole frames. Oh My Pi chunks a get_state answer over the line limit. A chunk
// ended the turn for the dedupe as if it were an agent_end. A repeated delivery of a turn still
// running then got its start and an end that never happened.
func TestAChunkedAnswerDoesNotEndTheTurnForTheDedupe(t *testing.T) {
	path := socketPath(t)
	daemon := listen(t, path)
	child := newOMP(t, "FAKE_OMP_STATE_BYTES=2097152")
	run(t, config(t, path, child), newClock())
	p := daemon.accept(t)
	p.open(t)
	ack := func(id string) shimwire.Response {
		return shimwire.Response{ID: id, Command: shimwire.TypePrompt, Success: true}
	}

	p.send(t, shimwire.Prompt{ID: "r1", DeliveryID: "d1", Message: "hold"})
	p.expect(t, ack("r1"))
	p.expect(t, shimwire.AgentStart{})
	p.send(t, shimwire.GetState{ID: "probe"})
	if frame := p.nextWhole(t); !isAnswerTo(frame, "probe") {
		t.Fatalf("the probe was answered as %#v", frame)
	}

	p.send(t, shimwire.Prompt{ID: "r2", DeliveryID: "d1", Message: "hold"})
	p.expect(t, ack("r2"))
	p.expect(t, shimwire.AgentStart{DeliveryID: "d1"})
	p.send(t, shimwire.GetState{ID: "after"})
	if frame := p.nextWhole(t); !isAnswerTo(frame, "after") {
		t.Fatalf("the shim sent %#v after the repeat's start, want the answer to the next get_state: the turn still runs", frame)
	}
}

func isAnswerTo(frame shimwire.Frame, id string) bool {
	answer, ok := frame.(shimwire.Response)
	return ok && answer.ID == id
}

// An agent_end too large for one line still ends the turn for the dedupe. The pump joins its
// sequence and shows the dedupe the whole agent_end. So a repeated delivery after it gets its end
// replayed, and the daemon does not wait for an end that already came.
func TestAChunkedAgentEndEndsTheTurnForTheDedupe(t *testing.T) {
	path := socketPath(t)
	daemon := listen(t, path)
	run(t, config(t, path, newOMP(t)), newClock())
	p := daemon.accept(t)
	p.open(t)
	ack := func(id string) shimwire.Response {
		return shimwire.Response{ID: id, Command: shimwire.TypePrompt, Success: true}
	}

	p.send(t, shimwire.Prompt{ID: "r1", DeliveryID: "d1", Message: "hold"})
	p.expect(t, ack("r1"))
	p.expect(t, shimwire.AgentStart{})
	p.sendLine(t, `{"type":"fake_end_turn_chunked"}`)
	if frame := p.nextWhole(t); typeOf(frame) != shimwire.TypeAgentEnd {
		t.Fatalf("the shim sent %#v, want the whole agent_end", frame)
	}

	p.send(t, shimwire.Prompt{ID: "r2", DeliveryID: "d1", Message: "hold"})
	p.expect(t, ack("r2"))
	p.expect(t, shimwire.AgentStart{DeliveryID: "d1"})
	p.expect(t, shimwire.AgentEnd{})
}

// A delivery the daemon sends again while its turn's agent_end arrives chunked is answered at once.
// The dedupe answers the acknowledgment and the start of the delivery, since the turn still runs.
// The pump writes the sequence only once it is whole, so the daemon reads both before the agent_end.
// It reads the start of its delivery, and then the end of that turn.
func TestADeliverySentAgainDuringAChunkedAgentEndIsAnsweredBeforeTheEnd(t *testing.T) {
	path := socketPath(t)
	daemon := listen(t, path)
	child := scripted(t, modelA, "await-go", "agent-end-chunked-half", "await-go", "end-chunked-rest")
	run(t, config(t, path, child), newClock())
	p := daemon.accept(t)
	p.open(t)
	p.readAhead(t)
	ack := func(id string) shimwire.Response {
		return shimwire.Response{ID: id, Command: shimwire.TypePrompt, Success: true}
	}

	p.send(t, shimwire.Prompt{ID: "r1", DeliveryID: "d1", Message: "hold"})
	p.expect(t, ack("r1"))
	p.expect(t, shimwire.AgentStart{})
	// The half of the agent_end is more than a pipe holds, so the shim has read its first chunk
	// once the fake has written it.
	p.sendLine(t, `{"type":"fake_go"}`)
	child.awaitProgress(t, 2)
	p.send(t, shimwire.Prompt{ID: "r2", DeliveryID: "d1", Message: "hold"})
	// The fake writes the rest of the agent_end only once the shim has handled the repeat: the
	// shim forwards this line after it.
	p.sendLine(t, `{"type":"fake_go"}`)
	if frame := p.nextWhole(t); !reflect.DeepEqual(frame, ack("r2")) {
		t.Fatalf("the daemon's reader got %#v first, want the acknowledgment of r2", frame)
	}
	if frame := p.nextWhole(t); !reflect.DeepEqual(frame, shimwire.AgentStart{DeliveryID: "d1"}) {
		t.Fatalf("the daemon's reader got %#v after the acknowledgment, want the start of d1", frame)
	}
	if frame := p.nextWhole(t); typeOf(frame) != shimwire.TypeAgentEnd {
		t.Fatalf("the daemon's reader got %#v after the start of d1, want the turn's whole agent_end", frame)
	}
}
