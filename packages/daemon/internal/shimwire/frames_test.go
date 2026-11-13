package shimwire

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

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
		{"agent_end", `{"type":"agent_end"}`, AgentEnd{}},
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
			"rescue-unpushed",
			`{"type":"rescue-unpushed","id":"r5","branch":"legion/LEGION-484-rescue-9d3f0c7a","workspaceTimeoutMs":120000,"timeoutMs":300000}`,
			RescueUnpushed{ID: "r5", Branch: "legion/LEGION-484-rescue-9d3f0c7a", WorkspaceTimeoutMs: 120_000, TimeoutMs: 300_000},
		},
		{
			"rescue-unpushed-result",
			`{"type":"rescue-unpushed-result","id":"r5","ok":true,"workspaces":[{"dir":"/legion/workspaces/acme/widgets/legion-484","status":"rescued","branches":[{"name":"legion/LEGION-484-rescue-9d3f0c7a-1","commit":"c0ffee"}]},{"dir":"/legion/workspaces/acme/widgets/legion-485","status":"error","error":"push refused"}]}`,
			RescueUnpushedResult{ID: "r5", OK: true, Workspaces: []RescueWorkspace{
				{Dir: "/legion/workspaces/acme/widgets/legion-484", Status: "rescued", Branches: []RescueBranch{{Name: "legion/LEGION-484-rescue-9d3f0c7a-1", Commit: "c0ffee"}}},
				{Dir: "/legion/workspaces/acme/widgets/legion-485", Status: "error", Error: "push refused"},
			}},
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

// The daemon sends no prompt: a task is a value the worker takes (internal/supervise, take.go).
// So no frame of the prompt's is modelled any more. An old shim or an old Oh My Pi may still write
// one — a `prompt` command a backlog replays, a `prompt_result` behind a turn's end — and each
// decodes to Raw, which the stream drops (internal/stream), rather than to a type a reader could
// act on. The turn frames are what the daemon reads of a turn, and they still decode as before:
// a start an old shim replayed with the delivery it named decodes to the plain start.
func TestNoPromptFrameIsModelledAndTheTurnFramesStillDecode(t *testing.T) {
	for _, line := range []string{
		`{"type":"prompt","id":"d1.XYZ","deliveryId":"d1","message":"work"}`,
		`{"type":"prompt_result","id":"d1.XYZ","agentInvoked":true,"status":"completed","sessionSettled":false}`,
		`{"type":"prompt_result","id":"d1.XYZ","agentInvoked":true,"status":"error",` +
			`"error":{"message":"429 Too Many Requests","provider":"anthropic","httpStatus":429,"retryable":true},"sessionSettled":true}`,
	} {
		decoded, err := Decode([]byte(line))
		if err != nil {
			t.Fatalf("Decode(%s): %v", line, err)
		}
		if _, ok := decoded.(Raw); !ok {
			t.Fatalf("Decode(%s) = %#v, want a Raw frame: no prompt frame is modelled", line, decoded)
		}
	}
	for _, tc := range []struct {
		line string
		want Frame
	}{
		{`{"type":"agent_start"}`, AgentStart{}},
		{`{"type":"agent_start","deliveryId":"d1"}`, AgentStart{}},
		{`{"type":"agent_end"}`, AgentEnd{}},
		{`{"type":"agent_end","messages":[{"role":"assistant"}]}`, AgentEnd{}},
	} {
		decoded, err := Decode([]byte(tc.line))
		if err != nil {
			t.Fatalf("Decode(%s): %v", tc.line, err)
		}
		if decoded != tc.want {
			t.Fatalf("Decode(%s) = %#v, want %#v", tc.line, decoded, tc.want)
		}
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

// The two frames the shipped code refuses when their fields are malformed, because in each case
// the sender is the only writer and a malformed one is a bug to name: the hello
// (worker-stream-listener.ts:140-148) and the daemon's adoption request (worker-shim.ts:171-180).
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
	rescue := RescueUnpushed{ID: "r5", Branch: "legion/LEGION-484-rescue-9d3f0c7a", WorkspaceTimeoutMs: 1, TimeoutMs: 2}
	if err := rescue.Validate(); err != nil {
		t.Fatalf("RescueUnpushed.Validate: %v", err)
	}
	for _, tc := range []struct {
		name  string
		frame RescueUnpushed
	}{
		{"no id", RescueUnpushed{Branch: rescue.Branch, WorkspaceTimeoutMs: 1, TimeoutMs: 2}},
		{"a branch outside legion/", RescueUnpushed{ID: "r5", Branch: "main", WorkspaceTimeoutMs: 1, TimeoutMs: 2}},
		{"a branch a refspec misreads", RescueUnpushed{ID: "r5", Branch: "legion/x:refs/heads/main", WorkspaceTimeoutMs: 1, TimeoutMs: 2}},
		{"no workspace bound", RescueUnpushed{ID: "r5", Branch: rescue.Branch, TimeoutMs: 2}},
		{"a workspace bound above the whole", RescueUnpushed{ID: "r5", Branch: rescue.Branch, WorkspaceTimeoutMs: 3, TimeoutMs: 2}},
	} {
		if err := tc.frame.Validate(); !errors.Is(err, ErrMalformedFrame) {
			t.Fatalf("%s: Validate = %v, want ErrMalformedFrame", tc.name, err)
		}
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
