package daemon

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// The rows of this file are dispatch://INFRA-5798's: a compaction Legion asks for is a `handoff`
// request to Oh My Pi, never a `compact` one, through `legion claims compact` (the operator's
// route, which that command posts) and through the controller's `compact`. Each row boots the
// whole daemon and puts a peer of its own on the worker stream in place of the claim's shim and
// the Oh My Pi behind it (handoffPeer), which records the type of every line the daemon writes.
//
// The peer reads the wire's own words, never shimwire's names for them, so the same rows run
// against a daemon of any release: at a daemon that writes `compact` they fail on what the peer
// recorded, which is how each row's control is run.

// handoffType and compactType are the two request types that shorten an Oh My Pi session, as the
// worker stream spells them.
const (
	handoffType = "handoff"
	compactType = "compact"
)

// handoffPeerRefusal is what the peer answers every request that names an id and is no
// negotiation, prompt or handoff, a `compact` among them: the request ends at once, refused,
// rather than holding the row for the compaction's ten-minute bound.
const handoffPeerRefusal = "this peer answers a handoff and no other request that shortens a session"

// handoffRefusedInATurn is what the pinned Oh My Pi answers a handoff asked for while a turn
// runs (`case "handoff"` in packages/coding-agent/src/modes/rpc/rpc-mode.ts of
// cybermonklabs/oh-my-pi at the pinned tag): a turn counts as running there from the prompt's
// admission, which its acknowledgement follows.
const handoffRefusedInATurn = "Cannot hand off while a response is in progress"

// handoffRowTask is the task each row's claim is spawned with and owes.
const handoffRowTask = "Work the issue."

// handoffPeer is one claim's shim on the daemon's worker stream. It records the type of every line
// the daemon writes, read from the line's own `type` member, and keeps each prompt and each line
// of a request that shortens a session. It answers the protocol negotiation, acknowledges every
// prompt as Oh My Pi does at its admission, and answers a handoff with what the row scripts.
// Every other request that names an id is refused (handoffPeerRefusal). The turn frames are the
// row's to send (send).
type handoffPeer struct {
	t      *testing.T
	sh     *shim
	answer func(id string) shimwire.Response

	mu       sync.Mutex
	types    []string
	prompts  []shimwire.Prompt
	shortens []string
}

// serve reads every line the daemon d writes until the connection closes or the row ends it
// (shim.serve). An answer that fails to write fails the row, unless the row had begun stopping d.
func (p *handoffPeer) serve(d *daemon) {
	reply := func(frame shimwire.Frame) {
		if err := p.sh.writer.WriteFrame(frame); err != nil && !d.stopped.Load() {
			p.t.Errorf("the peer's %s: %v", frame.FrameType(), err)
		}
	}
	for {
		line, err := p.sh.lines.ReadBytes('\n')
		if err != nil {
			return
		}
		var head struct {
			Type string `json:"type"`
			ID   string `json:"id"`
		}
		if err := json.Unmarshal(line, &head); err != nil {
			p.t.Errorf("the daemon wrote a line that is not a JSON object: %q", line)
			continue
		}
		p.mu.Lock()
		p.types = append(p.types, head.Type)
		p.mu.Unlock()
		switch head.Type {
		case shimwire.TypeNegotiateProtocol:
			reply(shimwire.Response{ID: head.ID, Command: head.Type, Success: true})
		case shimwire.TypePrompt:
			var prompt shimwire.Prompt
			if err := json.Unmarshal(line, &prompt); err != nil {
				p.t.Errorf("decode the prompt %q: %v", line, err)
				continue
			}
			p.mu.Lock()
			p.prompts = append(p.prompts, prompt)
			p.mu.Unlock()
			reply(shimwire.Response{ID: head.ID, Command: head.Type, Success: true})
		case handoffType, compactType:
			p.mu.Lock()
			p.shortens = append(p.shortens, strings.TrimSpace(string(line)))
			p.mu.Unlock()
			if head.Type == handoffType {
				reply(p.answer(head.ID))
				continue
			}
			reply(shimwire.Response{ID: head.ID, Command: head.Type, Success: false, Error: handoffPeerRefusal})
		default:
			if head.ID != "" {
				reply(shimwire.Response{ID: head.ID, Command: head.Type, Success: false, Error: handoffPeerRefusal})
			}
		}
	}
}

// send writes one frame of the claim's agent to the daemon: a turn's start or end, or a prompt's
// result.
func (p *handoffPeer) send(frame shimwire.Frame) {
	p.t.Helper()
	if err := p.sh.writer.WriteFrame(frame); err != nil {
		p.t.Fatalf("the peer's %s: %v", frame.FrameType(), err)
	}
}

// seen is what the peer has recorded so far: the type of every line, each prompt, and each line
// of a request that shortens a session.
func (p *handoffPeer) seen() (types []string, prompts []shimwire.Prompt, shortens []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.types), slices.Clone(p.prompts), slices.Clone(p.shortens)
}

// handoffRow is one row's daemon and the claim it compacts: the project's root architect, spawned
// owing handoffRowTask, connected through a handoffPeer, registered as session and ready, and
// prompted with that task once (first).
type handoffRow struct {
	t       *testing.T
	d       *daemon
	token   claim.Token
	issue   string
	session string
	peer    *handoffPeer
	first   shimwire.Prompt
}

// newHandoffRow boots the row's daemon and brings its claim to the first prompt of the task it
// owes. answer is what the peer answers a handoff with.
func newHandoffRow(t *testing.T, answer func(id string) shimwire.Response) *handoffRow {
	t.Helper()
	cfg := testConfig(t)
	rt := fake.NewRuntime()
	var record built
	d := startDaemon(t, cfg, fakeRuntime(rt, &record))
	spawn := architect()
	spawn.Task = handoffRowTask
	token := d.spawn(spawn)
	admitIssue(t, cfg, spawn.Issue, claim.RoleArchitect, token)
	launch := lastLaunch(t, rt, token)

	sh := dialShim(t, record.address, launch.BootToken)
	testwait.Eventually(t, "the hello to reach the machine", func() bool {
		return d.claim(token).State == string(supervise.StateShimConnected)
	})
	const session = "ses_architect"
	status, body := d.request(http.MethodPost, "/legion/v1/claims/register", claim.RegisterRequest{
		BootToken: launch.BootToken, SessionID: session, OmpSessionFile: "/sessions/" + session + ".jsonl",
		AgentID: session, PluginContract: api.DaemonAPIVersion(),
	}, false)
	if status != http.StatusOK {
		t.Fatalf("register = %d; body %s", status, body)
	}
	var registered claim.RegisterResponse
	if err := json.Unmarshal(body, &registered); err != nil {
		t.Fatalf("decode the registration %s: %v", body, err)
	}
	peer := &handoffPeer{t: t, sh: sh, answer: answer}
	sh.serve(func() { peer.serve(d) })
	if status, body := d.request(http.MethodPost, "/legion/v1/claims/ready", claim.ReadyRequest{
		ClaimToken: token, SessionID: session, Secret: registered.Secret, Generation: launch.Generation,
	}, false); status != http.StatusNoContent {
		t.Fatalf("ready = %d; body %s", status, body)
	}
	r := &handoffRow{t: t, d: d, token: token, issue: spawn.Issue, session: session, peer: peer}
	r.first = r.awaitPrompts("the first prompt of the task the claim owes", 1)[0]
	if r.first.DeliveryID == "" {
		t.Fatalf("the claim was prompted %+v, want its task under a delivery id", r.first)
	}
	return r
}

// awaitPrompts waits until the peer has been prompted count times and answers every prompt it was.
func (r *handoffRow) awaitPrompts(what string, count int) []shimwire.Prompt {
	r.t.Helper()
	var prompts []shimwire.Prompt
	testwait.Eventually(r.t, what, func() bool {
		_, prompts, _ = r.peer.seen()
		return len(prompts) >= count
	})
	return prompts
}

// owesItsTaskOnTheLadder ends the first prompt's turn in a provider error, which keeps the task
// on the claim under a new id and arms the ladder's first wait (supervise's providerErrored): the
// claim is idle and owes its task, as a claim whose provider keeps refusing its session is when a
// compaction is asked for.
func (r *handoffRow) owesItsTaskOnTheLadder() {
	r.t.Helper()
	r.peer.send(shimwire.AgentStart{})
	testwait.Eventually(r.t, "the first prompt's turn to start", func() bool {
		return r.d.claim(r.token).State == string(supervise.StateWorking)
	})
	r.peer.send(shimwire.AgentEnd{})
	r.peer.send(shimwire.PromptResult{ID: r.first.ID, AgentInvoked: true, Error: &shimwire.PromptError{
		Message: "overloaded", Provider: "anthropic", HTTPStatus: http.StatusServiceUnavailable, Retryable: true,
	}})
	testwait.Eventually(r.t, "the provider error to leave the claim idle owing its task", func() bool {
		c := r.d.claim(r.token)
		return c.State == string(supervise.StateIdle) && c.Pending != nil && c.Pending.ID != r.first.DeliveryID
	})
}

// handoffDoor is one of the two doors onto the compaction: the operator's route, which names the
// claim by its token and is what `legion claims compact` posts (cmd/legion's
// TestClaimsCompactPostsToTheClaimsCompactRoute; the live proof drives the built command itself,
// claim_compaction_live_test.go), and the controller's `compact`, which names it by its issue and
// role under a controller grant.
type handoffDoor struct {
	name string
	ask  func(r *handoffRow) (int, []byte)
}

func handoffDoors() []handoffDoor {
	return []handoffDoor{
		{"the operator's route legion claims compact posts", func(r *handoffRow) (int, []byte) {
			return r.d.request(http.MethodPost, "/legion/v1/operator/claims/"+string(r.token)+"/compact", nil, true)
		}},
		{"the controller's compact", func(r *handoffRow) (int, []byte) {
			return r.d.request(http.MethodPost, "/legion/v1/claims/compact",
				api.ClaimRequest{GrantID: r.controllerGrant(), Issue: r.issue, Role: string(claim.RoleArchitect)}, false)
		}},
	}
}

// controllerGrant registers the project's controller as the operator starts one (`legion
// controller start`: the operator's bearer buys the capability, the controller's Oh My Pi
// registers with it) and mints one grant of it, as its plugin does per operation.
func (r *handoffRow) controllerGrant() string {
	r.t.Helper()
	status, body := r.d.request(http.MethodPost, "/legion/v1/controller/secret",
		api.ControllerSecretRequest{PluginContract: api.DaemonAPIVersion()}, true)
	if status != http.StatusOK {
		r.t.Fatalf("the controller's secret = %d; body %s", status, body)
	}
	var capability api.ControllerSecretResponse
	if err := json.Unmarshal(body, &capability); err != nil {
		r.t.Fatalf("decode %s: %v", body, err)
	}
	const session = "ses_controller"
	status, body = r.d.request(http.MethodPost, "/legion/v1/claims/register", claim.RegisterRequest{
		BootToken: capability.Secret, SessionID: session, OmpSessionFile: "/sessions/" + session + ".jsonl",
		AgentID: session, PluginContract: api.DaemonAPIVersion(),
	}, false)
	if status != http.StatusOK {
		r.t.Fatalf("register the controller = %d; body %s", status, body)
	}
	var registration api.ControllerRegisterResponse
	if err := json.Unmarshal(body, &registration); err != nil {
		r.t.Fatalf("decode %s: %v", body, err)
	}
	status, body = r.d.request(http.MethodPost, "/legion/v1/grants",
		api.GrantRequest{SessionID: session, Secret: registration.Secret}, false)
	if status != http.StatusOK {
		r.t.Fatalf("the controller's grant = %d; body %s", status, body)
	}
	var granted api.GrantResponse
	if err := json.Unmarshal(body, &granted); err != nil || granted.GrantID == "" {
		r.t.Fatalf("decode the controller's grant %s: %v", body, err)
	}
	return granted.GrantID
}

// compact asks for the compaction through door and answers what the daemon said of it.
func (r *handoffRow) compact(door handoffDoor) api.CompactResponse {
	r.t.Helper()
	status, body := door.ask(r)
	if status != http.StatusOK {
		r.t.Fatalf("%s = %d, want 200; body %s", door.name, status, body)
	}
	var answer api.CompactResponse
	if err := json.Unmarshal(body, &answer); err != nil {
		r.t.Fatalf("decode the compaction's answer %s: %v", body, err)
	}
	return answer
}

// wantOneHandoff holds that the daemon wrote exactly one request that shortens a session, a
// handoff carrying the compaction's instructions, and no `compact` line at all.
func (r *handoffRow) wantOneHandoff() {
	r.t.Helper()
	types, _, shortens := r.peer.seen()
	if slices.Contains(types, compactType) {
		r.t.Errorf("the daemon wrote a %q line on the worker stream; the lines it wrote are of types %q", compactType, types)
	}
	if len(shortens) != 1 {
		r.t.Fatalf("the daemon wrote %d requests that shorten a session, %q, want one handoff", len(shortens), shortens)
	}
	var asked struct {
		Type               string `json:"type"`
		ID                 string `json:"id"`
		CustomInstructions string `json:"customInstructions"`
	}
	if err := json.Unmarshal([]byte(shortens[0]), &asked); err != nil {
		r.t.Fatalf("decode %q: %v", shortens[0], err)
	}
	if asked.Type != handoffType || asked.ID == "" || !strings.HasPrefix(asked.CustomInstructions, "Write the summary as text.") {
		r.t.Fatalf("the daemon wrote %q, want {type: %q, id, customInstructions: the compaction's instructions}", shortens[0], handoffType)
	}
}

// wantTheOwedTaskSentOnceAfter holds that the claim kept its session and was prompted with the
// task it owes exactly once after the compaction: the second prompt the peer saw, under the
// delivery the claim holds now.
func (r *handoffRow) wantTheOwedTaskSentOnceAfter() {
	r.t.Helper()
	prompts := r.awaitPrompts("the owed task to be sent again after the compaction", 2)
	c := r.d.claim(r.token)
	if c.Session != r.session {
		r.t.Errorf("the claim is on session %q after the compaction, want the session it had, %q", c.Session, r.session)
	}
	if c.Pending == nil || prompts[1].DeliveryID != c.Pending.ID || !strings.Contains(prompts[1].Message, handoffRowTask) {
		r.t.Fatalf("the prompt after the compaction is %+v and the claim holds %+v, want the task it owes", prompts[1], c.Pending)
	}
	time.Sleep(handoffQuiet)
	if _, prompts, _ := r.peer.seen(); len(prompts) != 2 {
		r.t.Fatalf("the claim was prompted %d times, %+v, want twice: the first send and the one after the compaction", len(prompts), prompts)
	}
}

// handoffQuiet is how long a row listens for a prompt that must not come. The daemon sends the
// owed task as the compaction ends, before its answer reaches the caller (supervise's
// compactEnded), so a second send arrives within milliseconds of the answer: a second is ample,
// and the control at a daemon that sends it twice shows it caught.
const handoffQuiet = time.Second

// Each door shortens the claim's session with a handoff carrying the compaction's instructions
// and writes no `compact`: the claim keeps its session, the answer says it was shortened, and the
// task it owes goes to it once, after the handoff (dispatch://INFRA-5798 R1).
func TestEachDoorShortensAClaimsSessionWithAHandoffAndSendsTheOwedTaskOnce(t *testing.T) {
	t.Parallel()
	for _, door := range handoffDoors() {
		t.Run(door.name, func(t *testing.T) {
			t.Parallel()
			r := newHandoffRow(t, func(id string) shimwire.Response {
				return shimwire.Response{ID: id, Command: handoffType, Success: true, Data: json.RawMessage(`{}`)}
			})
			r.owesItsTaskOnTheLadder()

			answer := r.compact(door)

			if !answer.Compacted || answer.Reason != "" {
				t.Errorf("the compaction answered %+v, want the session shortened with no reason", answer)
			}
			r.wantOneHandoff()
			r.wantTheOwedTaskSentOnceAfter()
		})
	}
}

// A task whose prompt Oh My Pi has accepted when the handoff is asked for, and whose turn is still
// to run, is sent once. The handoff aborts no turn: the pinned Oh My Pi refuses a handoff while a
// turn runs (handoffRefusedInATurn), and the turn that prompt started is the task's, so its start
// confirms the task and its result settles it. Taken back and sent again, as the daemon did around
// a `compact`, which aborts the turn, the task would reach the agent twice.
func TestATaskWhosePromptIsAcceptedWhenTheHandoffIsAskedForIsSentOnce(t *testing.T) {
	t.Parallel()
	for _, door := range handoffDoors() {
		t.Run(door.name, func(t *testing.T) {
			t.Parallel()
			r := newHandoffRow(t, func(id string) shimwire.Response {
				return shimwire.Response{ID: id, Command: handoffType, Success: false, Error: handoffRefusedInATurn}
			})
			testwait.Eventually(t, "the first prompt to be accepted", func() bool {
				p := r.d.claim(r.token).Pending
				return p != nil && p.ID == r.first.DeliveryID && p.DeliveredAt != nil && p.ConfirmedAt == nil
			})

			answer := r.compact(door)

			if answer.Compacted || answer.Reason != handoffRefusedInATurn {
				t.Errorf("the compaction answered %+v, want it not shortened because %q", answer, handoffRefusedInATurn)
			}
			time.Sleep(handoffQuiet)
			if _, prompts, _ := r.peer.seen(); len(prompts) != 1 {
				t.Errorf("the task was prompted %d times, %+v, want once: the handoff aborts no turn, and the turn of its first prompt still runs", len(prompts), prompts)
			}
			r.wantOneHandoff()

			r.peer.send(shimwire.AgentStart{})
			r.peer.send(shimwire.AgentEnd{})
			r.peer.send(shimwire.PromptResult{ID: r.first.ID, AgentInvoked: true})
			testwait.Eventually(t, "the turn of the first prompt to settle the task", func() bool {
				c := r.d.claim(r.token)
				return c.State == string(supervise.StateIdle) && c.Pending == nil
			})
			if _, prompts, _ := r.peer.seen(); len(prompts) != 1 {
				t.Fatalf("the task was prompted %d times, %+v, want once", len(prompts), prompts)
			}
		})
	}
}

// The answer of a compaction says whether the session was shortened and, when it was not, why
// (dispatch://INFRA-5798 R2): Oh My Pi's own words for a handoff it refused, and the daemon's for
// a handoff it answered with success and no handoff (`data: null`), which committed nothing. A
// success that carries the handoff's result is a session shortened, with a saved path or without
// one: the pinned Oh My Pi saves the document to a file only for a handoff it starts itself, so a
// handoff the daemon asks for is answered `data: {}`. In every row the claim is back and the task
// it owes goes to it once.
func TestTheAnswerOfAHandoffSaysWhetherTheSessionWasShortened(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		answer func(id string) shimwire.Response
		want   api.CompactResponse
	}{
		{
			name: "refused with a reason",
			answer: func(id string) shimwire.Response {
				return shimwire.Response{ID: id, Command: handoffType, Success: false, Error: "Nothing to hand off (already compacted)"}
			},
			want: api.CompactResponse{Compacted: false, Reason: "Nothing to hand off (already compacted)"},
		},
		{
			name: "success with data null",
			answer: func(id string) shimwire.Response {
				return shimwire.Response{ID: id, Command: handoffType, Success: true, Data: json.RawMessage(`null`)}
			},
			want: api.CompactResponse{Compacted: false, Reason: "Oh My Pi made no handoff document. The session did not change."},
		},
		{
			name: "success with a saved path",
			answer: func(id string) shimwire.Response {
				return shimwire.Response{ID: id, Command: handoffType, Success: true, Data: json.RawMessage(`{"savedPath":"/sessions/handoff.md"}`)}
			},
			want: api.CompactResponse{Compacted: true},
		},
		{
			name: "success with no saved path",
			answer: func(id string) shimwire.Response {
				return shimwire.Response{ID: id, Command: handoffType, Success: true, Data: json.RawMessage(`{}`)}
			},
			want: api.CompactResponse{Compacted: true},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newHandoffRow(t, tc.answer)
			r.owesItsTaskOnTheLadder()

			answer := r.compact(handoffDoors()[0])

			if answer != tc.want {
				t.Errorf("the compaction answered %+v, want %+v", answer, tc.want)
			}
			r.wantOneHandoff()
			r.wantTheOwedTaskSentOnceAfter()
			if state := r.d.claim(r.token).State; state != string(supervise.StateIdle) {
				t.Errorf("the claim is %s after the compaction, want it back idle", state)
			}
		})
	}
}
