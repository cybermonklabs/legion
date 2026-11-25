package shimwire

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

// agentBusyMessage is the message Oh My Pi refuses a prompt with when the agent is busy: the
// default of its AgentBusyError (packages/agent/src/agent.ts of cybermonklabs/oh-my-pi at the
// pinned tag), which it throws for a prompt sent with no streamingBehavior while a turn streams,
// and which its own test of the refusal expects the error to start with
// (packages/coding-agent/test/rpc-prompt-busy-code.test.ts there).
const agentBusyMessage = "Agent is already processing. Use steer() or followUp() to queue messages, or wait for completion."

// Every frame the shim and the daemon exchange, as the shipped TypeScript writes it on the wire
// (worker-rpc.ts:420, 430, 452, 475, 490, 493-496; worker-shim.ts:332, 356-366;
// worker-stream-listener.ts:14; fake-omp-rpc.ts:24-79). A frame decodes to its Go type and
// encodes back to the same line, so a Go peer is indistinguishable from the shipped one.
func TestEveryExchangedFrameRoundTripsItsWireLine(t *testing.T) {
	streaming := true
	for _, tc := range []struct {
		name  string
		line  string
		frame Frame
	}{
		{"hello", `{"type":"hello","bootToken":"boot-1"}`, Hello{BootToken: "boot-1"}},
		{"hello_ack", `{"type":"hello_ack"}`, HelloAck{}},
		{
			"negotiate_protocol",
			`{"type":"negotiate_protocol","id":"r1","protocolVersion":2}`,
			NegotiateProtocol{ID: "r1", ProtocolVersion: 2},
		},
		{
			"prompt",
			`{"type":"prompt","id":"r2","deliveryId":"d1","message":"work"}`,
			Prompt{ID: "r2", DeliveryID: "d1", Message: "work"},
		},
		{
			"response",
			`{"type":"response","id":"r2","command":"prompt","success":true}`,
			Response{ID: "r2", Command: TypePrompt, Success: true},
		},
		{
			"refusal response",
			`{"type":"response","id":"r2","command":"prompt","success":false,"error":"busy"}`,
			Response{ID: "r2", Command: TypePrompt, Success: false, Error: "busy"},
		},
		{
			// Oh My Pi's refusal of a prompt sent while a turn streams: its error response carries
			// `code` at the top level, right after `error`, and the member survives the round
			// trip (Response.Code).
			"busy refusal response",
			`{"type":"response","id":"r2","command":"prompt","success":false,"error":"` + agentBusyMessage + `","code":"session_busy"}`,
			Response{ID: "r2", Command: TypePrompt, Success: false, Error: agentBusyMessage, Code: "session_busy"},
		},
		{
			"get_state response",
			`{"type":"response","id":"r3","command":"get_state","success":true,"data":{"isStreaming":true}}`,
			Response{
				ID:      "r3",
				Command: TypeGetState,
				Success: true,
				Data:    json.RawMessage(`{"isStreaming":true}`),
			},
		},
		{"get_state", `{"type":"get_state","id":"r3"}`, GetState{ID: "r3"}},
		{
			// The compaction the daemon asks Oh My Pi for. Its instructions are what make the
			// summary text rather than snapcompact's images (Compact).
			"compact",
			`{"type":"compact","id":"r9","customInstructions":"Write the summary as text."}`,
			Compact{ID: "r9", CustomInstructions: "Write the summary as text."},
		},
		{
			"compact response",
			`{"type":"response","id":"r9","command":"compact","success":true,"data":{"summary":"what the turn did"}}`,
			Response{
				ID:      "r9",
				Command: TypeCompact,
				Success: true,
				Data:    json.RawMessage(`{"summary":"what the turn did"}`),
			},
		},
		{
			// What Oh My Pi answers a compaction it skipped: its ManualCompactionNoOpError
			// reaches the daemon as this refusal, in those words.
			"skipped compact response",
			`{"type":"response","id":"r9","command":"compact","success":false,"error":"Nothing to compact (session too small)"}`,
			Response{ID: "r9", Command: TypeCompact, Success: false, Error: "Nothing to compact (session too small)"},
		},
		{"agent_start", `{"type":"agent_start"}`, AgentStart{}},
		{"replayed agent_start", `{"type":"agent_start","deliveryId":"d1"}`, AgentStart{DeliveryID: "d1"}},
		{"agent_end", `{"type":"agent_end"}`, AgentEnd{}},
		{
			// Oh My Pi's own, measured against the pinned binary: it follows the agent_end of the
			// turn the prompt ran. Its line carries `status` and `sessionSettled` besides, which
			// the daemon reads nothing of and the decode drops
			// (TestAPromptResultDecodesTheBinarysOwnLineWhole).
			"prompt_result",
			`{"type":"prompt_result","id":"d1.XYZ","agentInvoked":true}`,
			PromptResult{ID: "d1.XYZ", AgentInvoked: true},
		},
		{
			// The error of a turn whose provider request failed with Oh My Pi's own retries
			// exhausted: an object of the binary's PromptError, never a string.
			"prompt_result of a turn that ended in a provider error",
			`{"type":"prompt_result","id":"d1.XYZ","agentInvoked":true,` +
				`"error":{"message":"429 Too Many Requests","provider":"anthropic","model":"claude-opus-4-5","httpStatus":429,"retryable":true}}`,
			PromptResult{ID: "d1.XYZ", AgentInvoked: true, Error: &PromptError{
				Message: "429 Too Many Requests", Provider: "anthropic", Model: "claude-opus-4-5",
				HTTPStatus: 429, Retryable: true,
			}},
		},
		{
			// A prompt Oh My Pi failed outright, which it writes with the two members its fail()
			// fills for a failure it gives no code, and no provider at all.
			"prompt_result of a prompt that ran no turn",
			`{"type":"prompt_result","id":"d1.XYZ","agentInvoked":false,"error":{"message":"no model","retryable":false}}`,
			PromptResult{ID: "d1.XYZ", AgentInvoked: false, Error: &PromptError{Message: "no model"}},
		},
		{
			// The prompt_result of a prompt Oh My Pi refused because the agent was busy: its
			// error carries Oh My Pi's class of the failure, which survives the round trip
			// (PromptError.Code).
			"prompt_result of a prompt refused because the agent was busy",
			`{"type":"prompt_result","id":"d1.XYZ","agentInvoked":false,` +
				`"error":{"message":"` + agentBusyMessage + `","retryable":false,"code":"session_busy"}}`,
			PromptResult{ID: "d1.XYZ", AgentInvoked: false, Error: &PromptError{Message: agentBusyMessage, Code: "session_busy"}},
		},
		{"shutdown", `{"type":"shutdown"}`, Shutdown{}},
		{
			"adopt-working-copy",
			`{"type":"adopt-working-copy","id":"r4","jjUser":"legion","jjEmail":"legion@example.com","timeoutMs":30000}`,
			AdoptWorkingCopy{ID: "r4", JJUser: "legion", JJEmail: "legion@example.com", TimeoutMs: 30_000},
		},
		{
			"adopt-working-copy-result",
			`{"type":"adopt-working-copy-result","id":"r4","ok":true}`,
			AdoptWorkingCopyResult{ID: "r4", OK: true},
		},
		{
			"failed adopt-working-copy-result",
			`{"type":"adopt-working-copy-result","id":"r4","ok":false,"error":"no workspace"}`,
			AdoptWorkingCopyResult{ID: "r4", OK: false, Error: "no workspace"},
		},
		{
			"rpc_chunk",
			`{"type":"rpc_chunk","chunkId":"agent-end-1","index":0,"count":2,"byteLength":1048577,"data":"eA=="}`,
			RPCChunk{ChunkID: "agent-end-1", Index: 0, Count: 2, ByteLength: 1_048_577, Data: "eA=="},
		},
		{
			"launcher_hello",
			`{"type":"launcher_hello","token":"launcher-token","sandbox":"legion-legion-legion-208","role":"tester","podUid":"pod-1","launcherId":"launcher-1"}`,
			LauncherHello{Token: "launcher-token", Sandbox: "legion-legion-legion-208", Role: "tester", PodUID: "pod-1", LauncherID: "launcher-1"},
		},
		{
			// A launcher whose environment pins the shared public leak check and has installed it.
			"launcher_hello naming an installed leak check",
			`{"type":"launcher_hello","token":"launcher-token","sandbox":"legion-legion-legion-208","role":"tester","podUid":"pod-1","launcherId":"launcher-1","leakCheckTag":"leak-check-v15","leakCheckInstalled":true}`,
			LauncherHello{Token: "launcher-token", Sandbox: "legion-legion-legion-208", Role: "tester", PodUID: "pod-1", LauncherID: "launcher-1", LeakCheckTag: "leak-check-v15", LeakCheckInstalled: true},
		},
		{"launcher_hello_ack", `{"type":"launcher_hello_ack"}`, LauncherHelloAck{}},
		{
			// The ack that hands the launcher its trimmer settings. An ack with no volumeTrim is
			// the row above, and the only ack that hands the launcher no settings
			// (TestALauncherHelloAckWithoutSettingsIsTheOnlyAbsenceOfThem).
			"launcher_hello_ack with trimmer settings",
			`{"type":"launcher_hello_ack","volumeTrim":{"highPercent":85,"lowPercent":70,"minAgeMinutes":120,"intervalSeconds":60}}`,
			LauncherHelloAck{VolumeTrim: &LauncherVolumeTrim{HighPercent: 85, LowPercent: 70, MinAgeMinutes: 120, IntervalSeconds: 60}},
		},
		{
			// One sweep as the trimmer reports it, every member set: the episode and what raised
			// it, the volume before and after, the sizes of the directories two levels under the
			// root, one of them read only in part, a directory it could not open with the read the
			// kernel refused, and the faults that have now stopped the trimmer itself.
			"launcher_volume_report",
			`{"type":"launcher_volume_report","trimmer":"trim-9f3c","episode":2,"raisedBy":"rise","usedBytes":48318382080,"freeBytes":5368709120,"freedBytes":10737418240,` +
				`"stopReason":"the reading fell below the low mark","refusedReads":["1842 role-init"],` +
				`"sizes":[{"path":"/legion/go/build","bytes":32212254720,"bytesOverHour":21474836480,"bytesOverDay":10737418240},` +
				`{"path":"/legion/scratch/tmp","bytes":16106127360,"bytesOverHour":8589934592,"bytesOverDay":0,"refused":"/legion/scratch/tmp/.../locked (depth 3)","refusals":2}],` +
				`"sizesAt":"2026-11-10T04:05:06Z","sizesPartial":true,"sizesMissing":[{"path":"/legion/uv","reason":"openat: permission denied"}],` +
				`"faults":["panic: the size walk read a path that went away (goroutine 42)"],"switchedOff":true}`,
			LauncherVolumeReport{
				Trimmer: "trim-9f3c", Episode: 2, RaisedBy: VolumeEpisodeRise,
				UsedBytes: 48_318_382_080, FreeBytes: 5_368_709_120, FreedBytes: 10_737_418_240,
				StopReason:   "the reading fell below the low mark",
				RefusedReads: []string{"1842 role-init"},
				Sizes: []LauncherVolumeSize{
					{Path: "/legion/go/build", Bytes: 32_212_254_720, BytesOverHour: 21_474_836_480, BytesOverDay: 10_737_418_240},
					{Path: "/legion/scratch/tmp", Bytes: 16_106_127_360, BytesOverHour: 8_589_934_592,
						Refused: "/legion/scratch/tmp/.../locked (depth 3)", Refusals: 2},
				},
				SizesAt:      time.Date(2026, 11, 10, 4, 5, 6, 0, time.UTC),
				SizesPartial: true,
				SizesMissing: []LauncherVolumeMissing{{Path: "/legion/uv", Reason: "openat: permission denied"}},
				Faults:       []string{"panic: the size walk read a path that went away (goroutine 42)"},
				SwitchedOff:  true,
			},
		},
		{
			"launcher_state",
			`{"type":"launcher_state","child":{"generation":7,"pid":42},"lastExit":{"generation":6,"code":143,"signal":"terminated"}}`,
			LauncherState{Child: &LauncherChild{Generation: 7, PID: 42}, LastExit: &LauncherExit{Generation: 6, Code: 143, Signal: "terminated"}},
		},
		{
			"launcher_start",
			`{"type":"launcher_start","id":"start-1","generation":7,"argv":["omp","--mode","rpc"],"env":["A=B"],"files":{"LEGION_BOOT_TOKEN":"boot-7"},"resumeFile":"/sessions/test.jsonl"}`,
			LauncherStart{ID: "start-1", Generation: 7, Argv: []string{"omp", "--mode", "rpc"}, Env: []string{"A=B"}, Files: map[string]string{"LEGION_BOOT_TOKEN": "boot-7"}, ResumeFile: "/sessions/test.jsonl"},
		},
		{
			// An Oh My Pi launch wrapped in the deployment's setup: the agent's executable is not
			// the first word of Argv, so the start names it.
			"launcher_start of an Oh My Pi launch",
			`{"type":"launcher_start","id":"start-2","generation":8,"argv":["/bin/sh","-c","exec \"$@\"","legion-start","/opt/omp/bin/omp"],"env":["A=B"],"agentExecutable":"/opt/omp/bin/omp"}`,
			LauncherStart{ID: "start-2", Generation: 8, Argv: []string{"/bin/sh", "-c", `exec "$@"`, "legion-start", "/opt/omp/bin/omp"}, Env: []string{"A=B"}, AgentExecutable: "/opt/omp/bin/omp"},
		},
		{
			"launcher_start_result",
			`{"type":"launcher_start_result","id":"start-1","ok":true,"runningGeneration":7}`,
			LauncherStartResult{ID: "start-1", OK: true, RunningGeneration: 7},
		},
		{
			"launcher_stop",
			`{"type":"launcher_stop","id":"stop-1","generation":7,"graceMs":10000}`,
			LauncherStop{ID: "stop-1", Generation: 7, GraceMs: 10_000},
		},
		{"launcher_stop_result", `{"type":"launcher_stop_result","id":"stop-1","ok":true}`, LauncherStopResult{ID: "stop-1", OK: true}},
		{
			"launcher_leak_check",
			`{"type":"launcher_leak_check","id":"leak-check-leak-check-v15-2c26b46b68ff","index":0,"count":1,"tag":"leak-check-v15","sha256":"2c26b46b68ffc68ff99b453c1d30413413422d706483bfa0f98a5e886266e7ae","payload":"Zm9v"}`,
			LauncherLeakCheck{ID: "leak-check-leak-check-v15-2c26b46b68ff", Index: 0, Count: 1, Tag: "leak-check-v15",
				SHA256: "2c26b46b68ffc68ff99b453c1d30413413422d706483bfa0f98a5e886266e7ae", Payload: []byte("foo")},
		},
		{
			"launcher_leak_check_result of an install",
			`{"type":"launcher_leak_check_result","id":"leak-check-leak-check-v15-2c26b46b68ff","index":0,"ok":true,"installed":true}`,
			LauncherLeakCheckResult{ID: "leak-check-leak-check-v15-2c26b46b68ff", Index: 0, OK: true, Installed: true},
		},
		{
			"refused launcher_leak_check_result",
			`{"type":"launcher_leak_check_result","id":"leak-check-leak-check-v15-2c26b46b68ff","index":0,"ok":false,"error":"the shared public leak check could not be installed: the pod's launcher was served leak-check-v15, not its own pin leak-check-v16"}`,
			LauncherLeakCheckResult{ID: "leak-check-leak-check-v15-2c26b46b68ff", Index: 0,
				Error: "the shared public leak check could not be installed: the pod's launcher was served leak-check-v15, not its own pin leak-check-v16"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			decoded, err := Decode([]byte(tc.line))
			if err != nil {
				t.Fatalf("Decode(%s): %v", tc.line, err)
			}
			if !reflect.DeepEqual(decoded, tc.frame) {
				t.Fatalf("Decode(%s) = %#v, want %#v", tc.line, decoded, tc.frame)
			}
			encoded, err := json.Marshal(tc.frame)
			if err != nil {
				t.Fatalf("Marshal(%#v): %v", tc.frame, err)
			}
			if string(encoded) != tc.line {
				t.Fatalf("Marshal(%#v) = %s, want %s", tc.frame, encoded, tc.line)
			}
		})
	}
	// The get_state answer's one field the daemon reads (worker-rpc.ts:476-479).
	var state StateData
	if err := json.Unmarshal(json.RawMessage(`{"isStreaming":true}`), &state); err != nil {
		t.Fatalf("Unmarshal state data: %v", err)
	}
	if state.IsStreaming == nil || *state.IsStreaming != streaming {
		t.Fatalf("StateData.IsStreaming = %v, want %v", state.IsStreaming, streaming)
	}
	// Absent rather than false: the shipped client leaves the run state alone and logs
	// (worker-rpc.ts:480-485), which it can only do because it can tell the two apart.
	state = StateData{}
	if err := json.Unmarshal(json.RawMessage(`{}`), &state); err != nil {
		t.Fatalf("Unmarshal empty state data: %v", err)
	}
	if state.IsStreaming != nil {
		t.Fatalf("StateData.IsStreaming = %v, want nil for an answer that omits it", *state.IsStreaming)
	}
}

// The prompt_result lines the pinned Oh My Pi actually writes, with the members, and except where
// a case says otherwise their order, as its own schema declares them (`PromptResultEvent { type,
// id?, agentInvoked, status, error?: PromptError, sessionSettled }`, `PromptError { message,
// provider?, model?, httpStatus?, retryable, code? }`, read in
// packages/coding-agent/src/modes/rpc/wire/frames.ts of cybermonklabs/oh-my-pi at the pinned tag).
// The daemon reads three of those members, so the decode must take the rest without being handed
// them and without refusing them: a frame it refuses is a prompt whose end the daemon never
// learns, and a claim that waits for that end forever.
func TestAPromptResultDecodesTheBinarysOwnLineWhole(t *testing.T) {
	for _, tc := range []struct {
		name string
		line string
		want PromptResult
	}{
		{
			"a turn that completed",
			`{"type":"prompt_result","id":"d1.XYZ","agentInvoked":true,"status":"completed","sessionSettled":false}`,
			PromptResult{ID: "d1.XYZ", AgentInvoked: true},
		},
		{
			// What a turn whose provider request ran out of retries ends with: the last assistant
			// message stopped with "error", and Oh My Pi reports its detail as an object.
			"a turn that ended in a provider error",
			`{"type":"prompt_result","id":"d1.XYZ","agentInvoked":true,"status":"error",` +
				`"error":{"message":"429 Too Many Requests","provider":"anthropic","model":"claude-opus-4-5","httpStatus":429,"retryable":true},` +
				`"sessionSettled":true}`,
			PromptResult{ID: "d1.XYZ", AgentInvoked: true, Error: &PromptError{
				Message: "429 Too Many Requests", Provider: "anthropic", Model: "claude-opus-4-5",
				HTTPStatus: 429, Retryable: true,
			}},
		},
		{
			// Oh My Pi's fail(): the prompt never reached a turn, and the error carries only the
			// two members that path fills for a failure it gives no code.
			"a prompt that failed before any turn",
			`{"type":"prompt_result","id":"d1.XYZ","agentInvoked":false,"status":"error",` +
				`"error":{"message":"no model","retryable":false},"sessionSettled":true}`,
			PromptResult{ID: "d1.XYZ", AgentInvoked: false, Error: &PromptError{Message: "no model"}},
		},
		{
			// Oh My Pi's refusal of a prompt sent with no streamingBehavior while a turn streams,
			// with its members in the order Oh My Pi writes them rather than the schema's:
			// `#report` writes `sessionSettled` before the `error` it adds last, and `fail`
			// writes `message`, `retryable`, then the `code` rpcBusyCode gave
			// (packages/coding-agent/src/modes/rpc/rpc-prompt-results.ts at the pinned tag). A
			// turn still streams, so the session has not settled.
			"a prompt refused because the agent was busy",
			`{"type":"prompt_result","id":"d1.XYZ","agentInvoked":false,"status":"error","sessionSettled":false,` +
				`"error":{"message":"` + agentBusyMessage + `","retryable":false,"code":"session_busy"}}`,
			PromptResult{ID: "d1.XYZ", AgentInvoked: false, Error: &PromptError{Message: agentBusyMessage, Code: "session_busy"}},
		},
		{
			// A refusal of the provider's content filter: Oh My Pi sets retryable for its own
			// transient class alone, so this failure is not retryable and names no status at
			// all, where the same words from a 400 would carry one.
			"a turn the provider's content filter refused",
			`{"type":"prompt_result","id":"d1.XYZ","agentInvoked":true,"status":"error",` +
				`"error":{"message":"Output blocked by content filtering policy","provider":"anthropic","model":"claude-opus-4-5","retryable":false},` +
				`"sessionSettled":true}`,
			PromptResult{ID: "d1.XYZ", AgentInvoked: true, Error: &PromptError{
				Message: "Output blocked by content filtering policy", Provider: "anthropic", Model: "claude-opus-4-5",
			}},
		},
		{
			// `id` is optional in the schema, and a result that names no request names no
			// delivery either; it still decodes, and the daemon drops it downstream.
			"a result of no request of the daemon's",
			`{"type":"prompt_result","agentInvoked":true,"status":"aborted","sessionSettled":true}`,
			PromptResult{AgentInvoked: true},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			decoded, err := Decode([]byte(tc.line))
			if err != nil {
				t.Fatalf("Decode(%s): %v", tc.line, err)
			}
			if !reflect.DeepEqual(decoded, tc.want) {
				t.Fatalf("Decode(%s) = %#v, want %#v", tc.line, decoded, tc.want)
			}
			result := decoded.(PromptResult)
			reason := ""
			if result.Error != nil {
				reason = result.Error.Message
			}
			if result.Reason() != reason {
				t.Fatalf("Reason() = %q, want %q", result.Reason(), reason)
			}
			// Each member of the error object by itself: the daemon decides what a failed turn
			// means from four of the members beside the message, so one dropped here is a
			// failure it reads as something else; code, which nothing reads, is kept so the
			// frame survives whole.
			if want := tc.want.Error; want != nil {
				got := result.Error
				if got == nil {
					t.Fatalf("Decode(%s) dropped the whole error object", tc.line)
				}
				for _, member := range []struct {
					name      string
					got, want any
				}{
					{"message", got.Message, want.Message},
					{"provider", got.Provider, want.Provider},
					{"model", got.Model, want.Model},
					{"httpStatus", got.HTTPStatus, want.HTTPStatus},
					{"retryable", got.Retryable, want.Retryable},
					{"code", got.Code, want.Code},
				} {
					if member.got != member.want {
						t.Errorf("error %s = %v, want %v", member.name, member.got, member.want)
					}
				}
			}
		})
	}
}

// The two frames of one tool call, as Oh My Pi writes them (measured on a worker's own stream).
// The start carries the call's arguments and the agent's words for the step; the end repeats the
// tool and the call id, carries neither, and adds a result object this package drops — the
// daemon reads none of it, and a decode that kept it would hold a tool's whole output in memory
// for every call every worker makes.
func TestTheToolCallFramesDecodeTheCallTheyName(t *testing.T) {
	start := `{"type":"tool_execution_start","toolCallId":"toolu_turn1_0","toolName":"bash","args":{"command":"echo hi"},"intent":"Running the case's step"}`
	decoded, err := Decode([]byte(start))
	if err != nil {
		t.Fatalf("Decode(%s): %v", start, err)
	}
	wantStart := ToolExecutionStart{ToolCallID: "toolu_turn1_0", ToolName: "bash",
		Args: json.RawMessage(`{"command":"echo hi"}`), Intent: "Running the case's step"}
	if !reflect.DeepEqual(decoded, wantStart) {
		t.Fatalf("Decode(%s) = %#v, want %#v", start, decoded, wantStart)
	}
	if got := decoded.FrameType(); got != TypeToolExecutionStart {
		t.Fatalf("FrameType() = %q, want %q", got, TypeToolExecutionStart)
	}

	end := `{"type":"tool_execution_end","toolCallId":"toolu_turn1_0","toolName":"bash",` +
		`"result":{"content":[{"type":"text","text":"hi"}],"details":{"wallTimeNoticeEmitted":false,"timeoutSeconds":300,"wallTimeMs":190.57}},"isError":false}`
	decoded, err = Decode([]byte(end))
	if err != nil {
		t.Fatalf("Decode(%s): %v", end, err)
	}
	wantEnd := ToolExecutionEnd{ToolCallID: "toolu_turn1_0", ToolName: "bash"}
	if !reflect.DeepEqual(decoded, wantEnd) {
		t.Fatalf("Decode(%s) = %#v, want %#v", end, decoded, wantEnd)
	}
	// A tool that refused the call says so, and the frame still pairs by its call id alone.
	failed := `{"type":"tool_execution_end","toolCallId":"toolu_turn1_1","toolName":"read","isError":true}`
	decoded, err = Decode([]byte(failed))
	if err != nil {
		t.Fatalf("Decode(%s): %v", failed, err)
	}
	if want := (ToolExecutionEnd{ToolCallID: "toolu_turn1_1", ToolName: "read", IsError: true}); decoded != want {
		t.Fatalf("Decode(%s) = %#v, want %#v", failed, decoded, want)
	}
}

// The shim is a bridge: OMP emits frames this package models nothing of (error, message updates,
// and whatever a later OMP adds), and they must cross it unchanged rather than fail a decode
// (worker-shim.ts:141-142 leaves them unsummarized and :290 forwards them anyway).
func TestAnUnknownFrameTypeDecodesToItsRawJSON(t *testing.T) {
	line := `{"type":"error","message":"the provider closed the stream","retryable":true}`
	decoded, err := Decode([]byte(line))
	if err != nil {
		t.Fatalf("Decode(%s): %v", line, err)
	}
	raw, ok := decoded.(Raw)
	if !ok {
		t.Fatalf("Decode(%s) = %#v, want a Raw frame", line, decoded)
	}
	if raw.FrameType() != "error" {
		t.Fatalf("FrameType() = %q, want %q", raw.FrameType(), "error")
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		t.Fatalf("Marshal(%#v): %v", raw, err)
	}
	if string(encoded) != line {
		t.Fatalf("Marshal(raw) = %s, want the line unchanged %s", encoded, line)
	}
	// A frame with no type at all is still a frame to forward, not an error.
	decoded, err = Decode([]byte(`{"messages":[]}`))
	if err != nil {
		t.Fatalf("Decode of a frame without a type: %v", err)
	}
	if got := decoded.FrameType(); got != "" {
		t.Fatalf("FrameType() = %q, want the empty string", got)
	}
}

// A line that is not a JSON object is not a frame. The shipped shim reaches the same verdict by
// parsing to undefined and matching no branch (worker-shim.ts:146-152, 252), and the listener
// refuses the connection outright (worker-stream-listener.ts:129-138).
func TestALineThatIsNotAJSONObjectIsRefused(t *testing.T) {
	for _, line := range []string{"", "   ", "not json", "null", "[1,2]", `"a string"`, "42", "{"} {
		if _, err := Decode([]byte(line)); !errors.Is(err, ErrNotAFrame) {
			t.Fatalf("Decode(%q) error = %v, want ErrNotAFrame", line, err)
		}
	}
}

// Four frames the shipped code refuses when their fields are malformed, because in each case the
// sender is the only writer and a malformed one is a bug to name: the hello
// (worker-stream-listener.ts:140-148), the daemon's adoption request (worker-shim.ts:171-180), the
// compaction request and the launcher's volume report.
func TestTheFramesTheShippedCodeValidatesRefuseTheirMalformedForms(t *testing.T) {
	if err := (Hello{BootToken: "boot-1"}).Validate(); err != nil {
		t.Fatalf("Hello.Validate: %v", err)
	}
	if err := (Hello{}).Validate(); !errors.Is(err, ErrMalformedFrame) {
		t.Fatalf("Hello{}.Validate = %v, want ErrMalformedFrame", err)
	}
	good := AdoptWorkingCopy{ID: "r4", JJUser: "legion", JJEmail: "l@example.com", TimeoutMs: 1}
	if err := good.Validate(); err != nil {
		t.Fatalf("AdoptWorkingCopy.Validate: %v", err)
	}
	for _, tc := range []struct {
		name  string
		frame AdoptWorkingCopy
	}{
		{"no id", AdoptWorkingCopy{JJUser: "legion", JJEmail: "l@example.com", TimeoutMs: 1}},
		{"no jjUser", AdoptWorkingCopy{ID: "r4", JJEmail: "l@example.com", TimeoutMs: 1}},
		{"no jjEmail", AdoptWorkingCopy{ID: "r4", JJUser: "legion", TimeoutMs: 1}},
		{"no timeout", AdoptWorkingCopy{ID: "r4", JJUser: "legion", JJEmail: "l@example.com"}},
		{"negative timeout", AdoptWorkingCopy{ID: "r4", JJUser: "legion", JJEmail: "l@example.com", TimeoutMs: -1}},
	} {
		if err := tc.frame.Validate(); !errors.Is(err, ErrMalformedFrame) {
			t.Fatalf("%s: Validate = %v, want ErrMalformedFrame", tc.name, err)
		}
	}
	// A compaction with no instructions is the one Oh My Pi would answer with snapcompact's
	// images, which a resumed worker needs a vision model to read, so the daemon never sends it.
	if err := (Compact{ID: "r9", CustomInstructions: "Write the summary as text."}).Validate(); err != nil {
		t.Fatalf("Compact.Validate: %v", err)
	}
	for _, tc := range []struct {
		name  string
		frame Compact
	}{
		{"no id", Compact{CustomInstructions: "Write the summary as text."}},
		{"no instructions", Compact{ID: "r9"}},
	} {
		if err := tc.frame.Validate(); !errors.Is(err, ErrMalformedFrame) {
			t.Fatalf("%s: Validate = %v, want ErrMalformedFrame", tc.name, err)
		}
	}
	// The volume report is the trimmer's own, and the daemon counts its notice bounds per
	// trimmer and per episode: a report missing either is one it could not count.
	report := LauncherVolumeReport{Trimmer: "trim-9f3c", Episode: 1, RaisedBy: VolumeEpisodeFirst}
	if err := report.Validate(); err != nil {
		t.Fatalf("LauncherVolumeReport.Validate: %v", err)
	}
	for _, tc := range []struct {
		name  string
		frame LauncherVolumeReport
	}{
		{"no trimmer", LauncherVolumeReport{Episode: 1, RaisedBy: VolumeEpisodeFirst}},
		{"an episode below the first", LauncherVolumeReport{Trimmer: report.Trimmer, RaisedBy: VolumeEpisodeFirst}},
	} {
		if err := tc.frame.Validate(); !errors.Is(err, ErrMalformedFrame) {
			t.Fatalf("%s: Validate = %v, want ErrMalformedFrame", tc.name, err)
		}
	}
}

// An adoption names, where it has them, the commit a fix's working copy starts from (fixFrom)
// and the commit the task's working copy moves to (moveTo), each a full commit id: 40
// lowercase hex digits. The shim's answer echoes each one it acted on: the directory it made the
// fix's working copy in (fixDir) and the commit it moved the task's working copy to (movedTo). The
// request still names no path, since the shim derives the fix's directory from its own
// workspace. A member that is no full commit id is refused, and a frame with neither member is
// taken, as a daemon before these members sends it, so the refusal is of the member's form.
func TestAnAdoptionNamesTheFixAndTheMove(t *testing.T) {
	const (
		fixFrom = "0123456789abcdef0123456789abcdef01234567"
		moveTo  = "89abcdef0123456789abcdef0123456789abcdef"
		head    = `{"type":"adopt-working-copy","id":"r5","jjUser":"legion","jjEmail":"legion@example.com","timeoutMs":30000`
	)
	adoption := func(t *testing.T, line string) AdoptWorkingCopy {
		t.Helper()
		decoded, err := Decode([]byte(line))
		if err != nil {
			t.Fatalf("Decode(%s): %v", line, err)
		}
		frame, ok := decoded.(AdoptWorkingCopy)
		if !ok {
			t.Fatalf("Decode(%s) = %#v, want an AdoptWorkingCopy", line, decoded)
		}
		return frame
	}
	t.Run("the request and its answer keep each member through the wire", func(t *testing.T) {
		for _, line := range []string{
			head + `,"fixFrom":"` + fixFrom + `","moveTo":"` + moveTo + `"}`,
			head + `,"fixFrom":"` + fixFrom + `"}`,
			head + `,"moveTo":"` + moveTo + `"}`,
			`{"type":"adopt-working-copy-result","id":"r5","ok":true,"fixDir":"/s/workspaces/o/r/widgets-42-fix","movedTo":"` + moveTo + `"}`,
		} {
			decoded, err := Decode([]byte(line))
			if err != nil {
				t.Fatalf("Decode(%s): %v", line, err)
			}
			encoded, err := json.Marshal(decoded)
			if err != nil {
				t.Fatalf("Marshal(%#v): %v", decoded, err)
			}
			if string(encoded) != line {
				t.Errorf("%s decodes to %#v and encodes as %s: the frame loses a member", line, decoded, encoded)
			}
		}
		if err := adoption(t, head+`,"fixFrom":"`+fixFrom+`","moveTo":"`+moveTo+`"}`).Validate(); err != nil {
			t.Errorf("Validate of an adoption naming both commits = %v, want nil", err)
		}
	})
	t.Run("a member that is no full commit id is refused", func(t *testing.T) {
		for _, member := range []string{
			`"fixFrom":"abc"`,
			`"moveTo":"abc"`,
			`"fixFrom":"` + strings.ToUpper(fixFrom) + `"`,
			`"moveTo":"` + moveTo + `0"`,
			`"fixFrom":"` + fixFrom[:39] + `g"`,
		} {
			line := head + "," + member + "}"
			if err := adoption(t, line).Validate(); !errors.Is(err, ErrMalformedFrame) {
				t.Errorf("Validate(%s) = %v, want ErrMalformedFrame", line, err)
			}
		}
	})
	t.Run("a frame with neither member is taken, as a daemon before them sends it", func(t *testing.T) {
		if err := adoption(t, head+"}").Validate(); err != nil {
			t.Errorf("Validate of an adoption naming no commit = %v, want nil", err)
		}
	})
	t.Run("the fix's own branch is taken only where it is a branch beside a fix", func(t *testing.T) {
		const fixBranch = "legion-beside/INFRA-1/2/a/fix-1/implementer"
		line := head + `,"fixFrom":"` + fixFrom + `","fixBranch":"` + fixBranch + `"}`
		decoded, err := Decode([]byte(line))
		if err != nil {
			t.Fatalf("Decode(%s): %v", line, err)
		}
		if encoded, err := json.Marshal(decoded); err != nil || string(encoded) != line {
			t.Errorf("%s decodes to %#v and encodes as %s (%v): the frame loses fixBranch", line, decoded, encoded, err)
		}
		if err := adoption(t, line).Validate(); err != nil {
			t.Errorf("Validate(%s) = %v, want nil", line, err)
		}
		// The control: an issue's own branch, which a fix pushes to only at its end, is no
		// branch of the fix's work in progress, and neither is a name outside legion-beside/.
		for _, branch := range []string{"legion/INFRA-1", "main", "other/legion-beside/INFRA-1/2/a/fix-1/implementer"} {
			line := head + `,"fixFrom":"` + fixFrom + `","fixBranch":"` + branch + `"}`
			if err := adoption(t, line).Validate(); !errors.Is(err, ErrMalformedFrame) {
				t.Errorf("Validate(%s) = %v, want ErrMalformedFrame", line, err)
			}
		}
	})
}

// A volume report may hold each of its four lists up to the list's cap and not one entry past
// it. The caps are the ones the launcher's trimmer cuts its reports at (internal/launcher's
// bound), and this is the daemon's side of them: a report over a cap is refused rather than
// rendered whole into the notice, whatever sent it. The negative control is the report at every
// cap, which validates.
func TestAVolumeReportOverAListCapIsRefused(t *testing.T) {
	at := LauncherVolumeReport{
		Trimmer: "trim-9f3c", Episode: 1, RaisedBy: VolumeEpisodeFirst,
		Sizes:        make([]LauncherVolumeSize, VolumeReportSizes),
		SizesMissing: make([]LauncherVolumeMissing, VolumeReportSizesMissing),
		RefusedReads: make([]string, VolumeReportRefusedReads),
		Faults:       make([]string, VolumeReportFaults),
	}
	if err := at.Validate(); err != nil {
		t.Fatalf("a report holding every list at its cap was refused: %v", err)
	}
	for _, tc := range []struct {
		list string
		over func(*LauncherVolumeReport)
	}{
		{"sizes", func(r *LauncherVolumeReport) { r.Sizes = append(r.Sizes, LauncherVolumeSize{}) }},
		{"sizesMissing", func(r *LauncherVolumeReport) { r.SizesMissing = append(r.SizesMissing, LauncherVolumeMissing{}) }},
		{"refusedReads", func(r *LauncherVolumeReport) { r.RefusedReads = append(r.RefusedReads, "") }},
		{"faults", func(r *LauncherVolumeReport) { r.Faults = append(r.Faults, "") }},
	} {
		t.Run(tc.list, func(t *testing.T) {
			report := at
			tc.over(&report)
			err := report.Validate()
			if !errors.Is(err, ErrMalformedFrame) || !strings.Contains(err.Error(), tc.list+" holds") {
				t.Fatalf("a report holding one entry past the cap of %s validated as %v, want ErrMalformedFrame naming the list", tc.list, err)
			}
		})
	}
}

// A start that names a session by its id alone (resumeSession) is the launcher's to look up, and
// the launcher resumes what it finds by replacing the argv's one `--resume=<id>` with the found
// file's path: a start that also names a file names two sessions, and one whose argv does not
// hold exactly one `--resume=<id>` of that session leaves the launcher no element to replace, or
// two. The sandbox runtime is the frame's only sender, so each is a bug to name. The negative
// control is the round-trip table's start naming its session by id in place of its file, with the
// one element, which validates.
func TestALauncherStartNamingASessionMustNameOneResumeOfIt(t *testing.T) {
	const session = "01a1f5de-4835-7667-b005-e99e3e17edd3"
	byID := LauncherStart{ID: "start-1", Generation: 7, Argv: []string{"omp", "--mode", "rpc", "--resume=" + session},
		Env: []string{"A=B"}, Files: map[string]string{"LEGION_BOOT_TOKEN": "boot-7"}, ResumeSession: session}
	if err := byID.Validate(); err != nil {
		t.Fatalf("a start naming its session by id with one --resume of it was refused: %v", err)
	}
	for name, change := range map[string]func(*LauncherStart){
		"a session file as well": func(s *LauncherStart) { s.ResumeFile = "/sessions/test.jsonl" },
		"no --resume of the session": func(s *LauncherStart) {
			s.Argv = []string{"omp", "--mode", "rpc"}
		},
		"two --resume of the session": func(s *LauncherStart) {
			s.Argv = []string{"omp", "--resume=" + session, "--mode", "rpc", "--resume=" + session}
		},
		"a --resume of another session": func(s *LauncherStart) {
			s.Argv = []string{"omp", "--mode", "rpc", "--resume=01a1f5de-4835-7667-b005-e99e3e28aa01"}
		},
		"the session as an element of its own": func(s *LauncherStart) {
			s.Argv = []string{"omp", "--mode", "rpc", "--resume", session}
		},
	} {
		t.Run(name, func(t *testing.T) {
			start := byID
			change(&start)
			err := start.Validate()
			if !errors.Is(err, ErrMalformedFrame) || !strings.Contains(err.Error(), "resumeSession") {
				t.Fatalf("a start naming session %s with %s validated as %v, want ErrMalformedFrame naming resumeSession", session, name, err)
			}
		})
	}
}

func TestHello2RoundTripsWithAndWithoutAnIdentity(t *testing.T) {
	identity := &AgentSecretsHello{Thumbprint: "NzbLsXh8uDCcd-6MNwXF4W_7noWXFZAfHkxZsRGC9Xs", PodToken: "eyJhbGciOiJSUzI1NiJ9.e30.sig"}
	for name, frame := range map[string]Hello2{
		"with identity":    {BootToken: "boot", AgentSecrets: identity},
		"without identity": {BootToken: "boot"},
	} {
		t.Run(name, func(t *testing.T) {
			line, err := json.Marshal(frame)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.HasPrefix(line, []byte(`{"type":"hello2"`)) {
				t.Fatalf("encoded as %s, want the hello2 type first", line)
			}
			if frame.AgentSecrets == nil && bytes.Contains(line, []byte("agentSecrets")) {
				t.Fatalf("an absent identity is encoded: %s", line)
			}
			decoded, err := Decode(line)
			if err != nil {
				t.Fatal(err)
			}
			got, ok := decoded.(Hello2)
			if !ok || got.BootToken != "boot" || (got.AgentSecrets == nil) != (frame.AgentSecrets == nil) ||
				(got.AgentSecrets != nil && *got.AgentSecrets != *identity) {
				t.Fatalf("decoded %#v", decoded)
			}
			if err := got.Validate(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestHello2ValidateNamesWhatIsMissing(t *testing.T) {
	for name, tc := range map[string]struct {
		frame Hello2
		want  string
	}{
		"no boot token":     {Hello2{}, "no bootToken"},
		"no thumbprint":     {Hello2{BootToken: "b", AgentSecrets: &AgentSecretsHello{PodToken: "a.b.c"}}, "no thumbprint"},
		"bad thumbprint":    {Hello2{BootToken: "b", AgentSecrets: &AgentSecretsHello{Thumbprint: "not base64url!", PodToken: "a.b.c"}}, "not a base64url SHA-256"},
		"no pod token":      {Hello2{BootToken: "b", AgentSecrets: &AgentSecretsHello{Thumbprint: "NzbLsXh8uDCcd-6MNwXF4W_7noWXFZAfHkxZsRGC9Xs"}}, "no podToken"},
		"pod token not jwt": {Hello2{BootToken: "b", AgentSecrets: &AgentSecretsHello{Thumbprint: "NzbLsXh8uDCcd-6MNwXF4W_7noWXFZAfHkxZsRGC9Xs", PodToken: "nodots"}}, "not a JWT"},
	} {
		t.Run(name, func(t *testing.T) {
			err := tc.frame.Validate()
			if err == nil || !errors.Is(err, ErrMalformedFrame) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate() = %v, want ErrMalformedFrame naming %q", err, tc.want)
			}
		})
	}
}

// A hello2 carrying a projected token the size EKS issues (about 1 KiB; a 2 KiB one here, for
// margin) fits the listener's pre-newline bound, so MaxHelloBytes stays as the shipped listener
// had it.
func TestAHello2WithAProjectedTokenFitsTheHelloBound(t *testing.T) {
	token := strings.Repeat("a", 700) + "." + strings.Repeat("b", 1000) + "." + strings.Repeat("c", 342)
	line, err := json.Marshal(Hello2{BootToken: strings.Repeat("t", 64), AgentSecrets: &AgentSecretsHello{
		Thumbprint: "NzbLsXh8uDCcd-6MNwXF4W_7noWXFZAfHkxZsRGC9Xs", PodToken: token}})
	if err != nil {
		t.Fatal(err)
	}
	if len(line)+1 > MaxHelloBytes {
		t.Fatalf("a hello2 with a 2 KiB token is %d bytes; MaxHelloBytes is %d", len(line)+1, MaxHelloBytes)
	}
}

func TestEnrollmentFramesRoundTrip(t *testing.T) {
	request := AgentSecretsEnrollment{ID: "req-1", EnrollmentID: "enr-1"}
	if err := request.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := (AgentSecretsEnrollment{ID: "req-1"}).Validate(); err == nil || !errors.Is(err, ErrMalformedFrame) {
		t.Fatalf("an enrollment frame with no enrollment id validated: %v", err)
	}
	for _, frame := range []Frame{request, AgentSecretsEnrollmentResult{ID: "req-1", OK: false, Error: "key dir is read-only"}} {
		line, err := json.Marshal(frame)
		if err != nil {
			t.Fatal(err)
		}
		decoded, err := Decode(line)
		if err != nil {
			t.Fatal(err)
		}
		if decoded.FrameType() != frame.FrameType() || !reflect.DeepEqual(decoded, frame) {
			t.Fatalf("decoded %#v from %s, want %#v", decoded, line, frame)
		}
	}
}

// The token request and its answer are the shim's alone: a request decodes as the frame the shim
// answers rather than as Raw it would forward to OMP, and the answer carries the token back.
func TestTokenFramesRoundTrip(t *testing.T) {
	request := AgentSecretsToken{ID: "req-1"}
	if err := request.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := (AgentSecretsToken{}).Validate(); err == nil || !errors.Is(err, ErrMalformedFrame) {
		t.Fatalf("a token request with no id validated: %v", err)
	}
	for _, frame := range []Frame{
		request,
		AgentSecretsTokenResult{ID: "req-1", OK: true, PodToken: "eyJhbGciOiJSUzI1NiJ9.e30.sig"},
		AgentSecretsTokenResult{ID: "req-1", OK: false, Error: "read the projected token /var/run/token: no such file"},
	} {
		line, err := json.Marshal(frame)
		if err != nil {
			t.Fatal(err)
		}
		decoded, err := Decode(line)
		if err != nil {
			t.Fatal(err)
		}
		if decoded.FrameType() != frame.FrameType() || !reflect.DeepEqual(decoded, frame) {
			t.Fatalf("decoded %#v from %s, want %#v", decoded, line, frame)
		}
	}
}

// memoryGuardAckLine is the acknowledgement of a daemon that sends the guard its settings, as it
// crosses the wire.
const memoryGuardAckLine = `{"type":"launcher_hello_ack","memoryGuard":{"actPercent":85,"floorPercent":10,"waitSeconds":5,"intervalMilliseconds":100}}`

// memoryKillLine is a launcher's report of one kill, as it crosses the wire.
const memoryKillLine = `{"type":"launcher_memory_kill","guard":"3f2a9c1d5e7b8a60","sequence":1,"pid":4242,"executable":"python3",` +
	`"sizeBytes":1048576000,"readingBytes":3774873600,"counterBytes":4089446400,"limitBytes":4294967296,"at":"2026-11-14T03:00:00Z"}`

func memoryKill() LauncherMemoryKill {
	return LauncherMemoryKill{
		Guard: "3f2a9c1d5e7b8a60", Sequence: 1, PID: 4242, Executable: "python3",
		SizeBytes: 1000 << 20, ReadingBytes: 3600 << 20, CounterBytes: 3900 << 20, LimitBytes: 4 << 30,
		At: time.Date(2026, 11, 14, 3, 0, 0, 0, time.UTC),
	}
}

// Both of the guard's frames cross the wire whole, through the writer and the reader a launcher
// and the daemon speak, and decode to their own type with every member.
func TestMemoryGuardFramesRoundTripThroughTheWire(t *testing.T) {
	for _, tc := range []struct {
		frame Frame
		line  string
	}{
		{LauncherHelloAck{MemoryGuard: &LauncherMemoryGuard{ActPercent: 85, FloorPercent: 10, WaitSeconds: 5, IntervalMilliseconds: 100}}, memoryGuardAckLine},
		{memoryKill(), memoryKillLine},
	} {
		t.Run(tc.frame.FrameType(), func(t *testing.T) {
			var wire bytes.Buffer
			if err := NewWriter(&wire).WriteFrame(tc.frame); err != nil {
				t.Fatal(err)
			}
			if wire.String() != tc.line+"\n" {
				t.Fatalf("the writer wrote %s, want %s", wire.String(), tc.line)
			}
			line, err := NewReader(&wire).ReadLine()
			if err != nil {
				t.Fatal(err)
			}
			decoded, err := Decode(line)
			if err != nil {
				t.Fatalf("Decode(%s): %v", line, err)
			}
			if !reflect.DeepEqual(decoded, tc.frame) {
				t.Fatalf("Decode(%s) = %#v, want %#v", line, decoded, tc.frame)
			}
			err = nil
			switch frame := decoded.(type) {
			case LauncherHelloAck:
				err = frame.MemoryGuard.Validate()
			case LauncherMemoryKill:
				err = frame.Validate()
			}
			if err != nil {
				t.Fatalf("the decoded frame does not validate: %v", err)
			}
		})
	}
}

// An acknowledgement with no memoryGuard member decodes with none, and the launcher's guard turns
// off on that absence: a daemon built before the guard writes such a line, and so does one whose
// deployment turned the guard off, with or without a volumeTrim member beside it. This case
// decodes the empty acknowledgement, the plainest such line.
func TestMemoryGuardAnAcknowledgementWithNoMemberDecodesWithNone(t *testing.T) {
	decoded, err := Decode([]byte(`{"type":"launcher_hello_ack"}`))
	if err != nil {
		t.Fatal(err)
	}
	ack, ok := decoded.(LauncherHelloAck)
	if !ok || ack.MemoryGuard != nil {
		t.Fatalf("decoded %#v, want a launcher_hello_ack with no memoryGuard", decoded)
	}
	line, err := json.Marshal(LauncherHelloAck{})
	if err != nil {
		t.Fatal(err)
	}
	if string(line) != `{"type":"launcher_hello_ack"}` {
		t.Fatalf("the empty acknowledgement encodes as %s", line)
	}
}

// The bytes a daemon that sends the settings writes decode to those settings, which hold to the
// ranges.
func TestMemoryGuardTheAcknowledgementBytesDecode(t *testing.T) {
	decoded, err := Decode([]byte(memoryGuardAckLine))
	if err != nil {
		t.Fatal(err)
	}
	ack, ok := decoded.(LauncherHelloAck)
	want := LauncherMemoryGuard{ActPercent: 85, FloorPercent: 10, WaitSeconds: 5, IntervalMilliseconds: 100}
	if !ok || ack.MemoryGuard == nil || *ack.MemoryGuard != want {
		t.Fatalf("decoded %#v, want the settings %+v", decoded, want)
	}
	if err := ack.MemoryGuard.Validate(); err != nil {
		t.Fatal(err)
	}
}

// Each number of the settings is refused one below its minimum and one above its maximum, by a
// refusal that names the member and its range, and taken at either bound. The defaults, the
// controller pod's wait among them, are inside every range.
func TestMemoryGuardSettingsValidateRefusesEachSideOfEachBound(t *testing.T) {
	defaults := LauncherMemoryGuard{
		ActPercent: MemoryGuardActDefault, FloorPercent: MemoryGuardFloorDefault,
		WaitSeconds: MemoryGuardWaitDefault, IntervalMilliseconds: MemoryGuardIntervalDefault,
	}
	if err := defaults.Validate(); err != nil {
		t.Fatalf("the defaults are refused: %v", err)
	}
	controller := defaults
	controller.WaitSeconds = MemoryGuardControllerWaitDefault
	if err := controller.Validate(); err != nil {
		t.Fatalf("the controller pod's defaults are refused: %v", err)
	}
	for _, tc := range []struct {
		member string
		lo, hi int
		set    func(*LauncherMemoryGuard, int)
	}{
		{"actPercent", MemoryGuardActMin, MemoryGuardActMax, func(g *LauncherMemoryGuard, v int) { g.ActPercent = v }},
		{"floorPercent", MemoryGuardFloorMin, MemoryGuardFloorMax, func(g *LauncherMemoryGuard, v int) { g.FloorPercent = v }},
		{"waitSeconds", MemoryGuardWaitMin, MemoryGuardWaitMax, func(g *LauncherMemoryGuard, v int) { g.WaitSeconds = v }},
		{"intervalMilliseconds", MemoryGuardIntervalMin, MemoryGuardIntervalMax, func(g *LauncherMemoryGuard, v int) { g.IntervalMilliseconds = v }},
	} {
		t.Run(tc.member, func(t *testing.T) {
			for _, v := range []int{tc.lo, tc.hi} {
				g := defaults
				tc.set(&g, v)
				if err := g.Validate(); err != nil {
					t.Fatalf("%s %d is refused: %v", tc.member, v, err)
				}
			}
			for _, v := range []int{tc.lo - 1, tc.hi + 1} {
				g := defaults
				tc.set(&g, v)
				err := g.Validate()
				want := fmt.Sprintf("memoryGuard.%s is %d, not between %d and %d", tc.member, v, tc.lo, tc.hi)
				if !errors.Is(err, ErrMalformedFrame) || !strings.Contains(err.Error(), want) {
					t.Fatalf("%s %d: Validate = %v, want ErrMalformedFrame naming %q", tc.member, v, err, want)
				}
			}
		})
	}
}

// A kill report is refused where the daemon could not apply it once or tell a role of it: no
// guard, a sequence below 1, no process, no limit, or an executable over its bound, which is
// taken at the bound itself.
func TestMemoryGuardKillValidateRefusesWhatTheDaemonCannotApply(t *testing.T) {
	atBound := memoryKill()
	atBound.Executable = strings.Repeat("c", MemoryKillExecutableBytes)
	if err := atBound.Validate(); err != nil {
		t.Fatalf("an executable of %d bytes is refused: %v", MemoryKillExecutableBytes, err)
	}
	for _, tc := range []struct {
		name   string
		change func(*LauncherMemoryKill)
		want   string
	}{
		{"no guard", func(k *LauncherMemoryKill) { k.Guard = "" }, "carries no guard"},
		{"sequence 0", func(k *LauncherMemoryKill) { k.Sequence = 0 }, "sequence is 0"},
		{"pid 0", func(k *LauncherMemoryKill) { k.PID = 0 }, "pid is 0"},
		{"a negative pid", func(k *LauncherMemoryKill) { k.PID = -1 }, "pid is -1"},
		{"no limit", func(k *LauncherMemoryKill) { k.LimitBytes = 0 }, "carries no limitBytes"},
		{"an executable over its bound", func(k *LauncherMemoryKill) { k.Executable = strings.Repeat("c", MemoryKillExecutableBytes+1) }, "executable is 257 bytes, limit 256"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			k := memoryKill()
			tc.change(&k)
			err := k.Validate()
			if !errors.Is(err, ErrMalformedFrame) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate = %v, want ErrMalformedFrame naming %q", err, tc.want)
			}
		})
	}
}

// An ack with no volumeTrim member is the one ack that hands the launcher no settings: a daemon
// built before INFRA-2009 writes such a line, and so does a deployment that turned the trimmer
// off, with or without a memoryGuard member beside it. This case decodes the empty object, the
// plainest such line. Settings whose numbers are all zero are not that line — they are
// settings, which the launcher refuses value by value, and a launcher that read them as an
// absence would say no settings came and discard a held report that the daemon which sent them
// can take — which is why the member is a pointer (LauncherHelloAck.VolumeTrim).
func TestALauncherHelloAckWithoutSettingsIsTheOnlyAbsenceOfThem(t *testing.T) {
	decoded, err := Decode([]byte(`{"type":"launcher_hello_ack"}`))
	if err != nil {
		t.Fatalf("Decode the old empty ack: %v", err)
	}
	ack, ok := decoded.(LauncherHelloAck)
	if !ok {
		t.Fatalf("Decode the old empty ack = %#v, want a LauncherHelloAck", decoded)
	}
	if ack.VolumeTrim != nil {
		t.Fatalf("the old empty ack decoded VolumeTrim = %#v, want nil", ack.VolumeTrim)
	}
	decoded, err = Decode([]byte(`{"type":"launcher_hello_ack","volumeTrim":{}}`))
	if err != nil {
		t.Fatalf("Decode an ack carrying zeroed settings: %v", err)
	}
	ack, ok = decoded.(LauncherHelloAck)
	if !ok {
		t.Fatalf("Decode an ack carrying zeroed settings = %#v, want a LauncherHelloAck", decoded)
	}
	if ack.VolumeTrim == nil {
		t.Fatal("an ack carrying zeroed settings decoded VolumeTrim = nil, want settings a launcher can read")
	}
	if *ack.VolumeTrim != (LauncherVolumeTrim{}) {
		t.Fatalf("an ack carrying zeroed settings decoded VolumeTrim = %#v, want the zero settings it carried", *ack.VolumeTrim)
	}
}

// A report whose walk measured nothing still writes `sizes` as an array: a strict reader refuses
// a null, which is why record.UpdateStalled.MarshalJSON keeps `waiting` one the same way.
func TestAVolumeReportWithNoSizesEncodesAnEmptyArray(t *testing.T) {
	line, err := json.Marshal(LauncherVolumeReport{Trimmer: "trim-9f3c", Episode: 1, RaisedBy: VolumeEpisodeFirst})
	if err != nil {
		t.Fatalf("Marshal a report with no sizes: %v", err)
	}
	if !bytes.Contains(line, []byte(`"sizes":[]`)) || bytes.Contains(line, []byte(`"sizes":null`)) {
		t.Fatalf("Marshal a report with no sizes = %s, want its sizes as an empty array", line)
	}
}

// kernelKillLine and kernelProtectedKillLine are a launcher's reports of one process the pod's
// kernel stopped, a command of the role's and one of the processes that run its session, as they
// cross the wire: the guard's report with the cause last.
const (
	kernelKillLine = `{"type":"launcher_memory_kill","guard":"3f2a9c1d5e7b8a60","sequence":1,"pid":4242,"executable":"python3",` +
		`"sizeBytes":1048576000,"readingBytes":3774873600,"counterBytes":4089446400,"limitBytes":4294967296,"at":"2026-11-14T03:00:00Z","cause":"kernel"}`
	kernelProtectedKillLine = `{"type":"launcher_memory_kill","guard":"3f2a9c1d5e7b8a60","sequence":1,"pid":4242,"executable":"python3",` +
		`"sizeBytes":1048576000,"readingBytes":3774873600,"counterBytes":4089446400,"limitBytes":4294967296,"at":"2026-11-14T03:00:00Z","cause":"kernel-protected"}`
)

// A report of a process the pod's kernel stopped, of either of the kernel's causes, crosses the
// wire whole, through the writer and the reader a launcher and the daemon speak, with its cause,
// and the frame's own rule takes it.
func TestMemoryKillAKernelKillRoundTripsThroughTheWireWithItsCause(t *testing.T) {
	for _, tc := range []struct {
		cause MemoryKillCause
		line  string
	}{
		{MemoryKillCauseKernel, kernelKillLine},
		{MemoryKillCauseKernelProtected, kernelProtectedKillLine},
	} {
		t.Run(string(tc.cause), func(t *testing.T) {
			kill := memoryKill()
			kill.Cause = tc.cause
			var wire bytes.Buffer
			if err := NewWriter(&wire).WriteFrame(kill); err != nil {
				t.Fatal(err)
			}
			if wire.String() != tc.line+"\n" {
				t.Fatalf("the writer wrote %s, want %s", wire.String(), tc.line)
			}
			line, err := NewReader(&wire).ReadLine()
			if err != nil {
				t.Fatal(err)
			}
			decoded, err := Decode(line)
			if err != nil {
				t.Fatalf("Decode(%s): %v", line, err)
			}
			if !reflect.DeepEqual(decoded, kill) {
				t.Fatalf("Decode(%s) = %#v, want %#v", line, decoded, kill)
			}
			if err := decoded.(LauncherMemoryKill).Validate(); err != nil {
				t.Fatalf("the decoded kernel kill does not validate: %v", err)
			}
		})
	}
}

// A report that carries no cause, which is every report a launcher built before the member sends,
// decodes with none and validates: it is the guard's own kill. The guard's report still encodes
// as those bytes, with no cause member.
func TestMemoryKillAReportWithNoCauseIsTheGuardsOwn(t *testing.T) {
	decoded, err := Decode([]byte(memoryKillLine))
	if err != nil {
		t.Fatal(err)
	}
	kill, ok := decoded.(LauncherMemoryKill)
	if !ok || kill.Cause != MemoryKillCauseGuard {
		t.Fatalf("decoded %#v, want a launcher_memory_kill with no cause", decoded)
	}
	if err := kill.Validate(); err != nil {
		t.Fatalf("a report with no cause does not validate: %v", err)
	}
	line, err := json.Marshal(memoryKill())
	if err != nil {
		t.Fatal(err)
	}
	if string(line) != memoryKillLine {
		t.Fatalf("the guard's report encodes as %s, want %s", line, memoryKillLine)
	}
}

// A cause other than the three MemoryKillCause values is one no note says, and the frame's own
// rule refuses it, naming the cause; the three it knows are taken.
func TestMemoryKillValidateRefusesACauseItDoesNotKnow(t *testing.T) {
	for _, cause := range []MemoryKillCause{MemoryKillCauseGuard, MemoryKillCauseKernel, MemoryKillCauseKernelProtected} {
		k := memoryKill()
		k.Cause = cause
		if err := k.Validate(); err != nil {
			t.Fatalf("cause %q is refused: %v", cause, err)
		}
	}
	for _, cause := range []MemoryKillCause{"guard", "Kernel", " kernel", "oom", "protected", "kernel-Protected", "kernel-protected "} {
		t.Run(string(cause), func(t *testing.T) {
			k := memoryKill()
			k.Cause = cause
			err := k.Validate()
			want := fmt.Sprintf("launcher_memory_kill cause is %q, not \"kernel\", \"kernel-protected\" or none", cause)
			if !errors.Is(err, ErrMalformedFrame) || !strings.Contains(err.Error(), want) {
				t.Fatalf("Validate = %v, want ErrMalformedFrame naming %q", err, want)
			}
		})
	}
}

// An acknowledgement without kernelKills, an older daemon's or the controller's pod's, decodes
// with it false, whether or not it carries guard settings; one with it decodes true.
func TestHelloAckKernelKillsDecodesFalseWithoutTheMemberAndTrueWithIt(t *testing.T) {
	settings := &LauncherMemoryGuard{ActPercent: 85, FloorPercent: 10, WaitSeconds: 5, IntervalMilliseconds: 100}
	for _, tc := range []struct {
		line string
		want LauncherHelloAck
	}{
		{`{"type":"launcher_hello_ack"}`, LauncherHelloAck{}},
		{memoryGuardAckLine, LauncherHelloAck{MemoryGuard: settings}},
		{`{"type":"launcher_hello_ack","kernelKills":true}`, LauncherHelloAck{KernelKills: true}},
		{`{"type":"launcher_hello_ack","memoryGuard":{"actPercent":85,"floorPercent":10,"waitSeconds":5,"intervalMilliseconds":100},"kernelKills":true}`,
			LauncherHelloAck{MemoryGuard: settings, KernelKills: true}},
	} {
		decoded, err := Decode([]byte(tc.line))
		if err != nil {
			t.Fatalf("Decode(%s): %v", tc.line, err)
		}
		if !reflect.DeepEqual(decoded, tc.want) {
			t.Fatalf("Decode(%s) = %#v, want %#v", tc.line, decoded, tc.want)
		}
	}
}

// An acknowledgement whose KernelKills is false carries no kernelKills member, so it is byte for
// byte the acknowledgement a daemon built before the member writes, with guard settings or
// without; one whose KernelKills is true carries the member after the settings.
func TestHelloAckEncodesKernelKillsOnlyWhenTrue(t *testing.T) {
	settings := &LauncherMemoryGuard{ActPercent: 85, FloorPercent: 10, WaitSeconds: 5, IntervalMilliseconds: 100}
	for _, tc := range []struct {
		ack  LauncherHelloAck
		line string
	}{
		{LauncherHelloAck{}, `{"type":"launcher_hello_ack"}`},
		{LauncherHelloAck{MemoryGuard: settings}, memoryGuardAckLine},
		{LauncherHelloAck{KernelKills: true}, `{"type":"launcher_hello_ack","kernelKills":true}`},
		{LauncherHelloAck{MemoryGuard: settings, KernelKills: true},
			`{"type":"launcher_hello_ack","memoryGuard":{"actPercent":85,"floorPercent":10,"waitSeconds":5,"intervalMilliseconds":100},"kernelKills":true}`},
	} {
		line, err := json.Marshal(tc.ack)
		if err != nil {
			t.Fatal(err)
		}
		if string(line) != tc.line {
			t.Fatalf("%+v encodes as %s, want %s", tc.ack, line, tc.line)
		}
	}
}
