package daemon

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// The rows of this file are dispatch://INFRA-5798's, carried onto the task a worker takes
// (dispatch://INFRA-2811, departure 10 of its plan): a compaction Legion asks for is a `handoff`
// request to Oh My Pi, never a `compact` one, through `legion claims compact` (the operator's
// route, which that command posts) and through the controller's `compact`. Each row boots the
// whole daemon on the test NATS (bucketRig) and puts a worker of its own on the worker stream in
// place of the claim's shim and the Oh My Pi behind it (bucketWorker, whose ompPeer records the
// type of every line the daemon writes), and the worker's plugin takes the claim's task
// (takeDriver): the daemon pushes no task.
//
// The peer reads the wire's own words, never shimwire's names for them, so the same rows run
// against a daemon of any release: at a daemon that writes `compact` they fail on what the peer
// recorded, which is how each row's control is run.

// handoffType and compactType are the two request types that shorten an Oh My Pi session, as the
// worker stream spells them, and promptType the type of the frame the daemon pushed a task in
// before a worker took it, which no daemon of the take writes.
const (
	handoffType = "handoff"
	compactType = "compact"
	promptType  = "prompt"
)

// handoffPeerRefusal is what the peer answers a `compact` request: it ends at once, refused,
// rather than holding the row for the compaction's ten-minute bound.
const handoffPeerRefusal = "this peer answers a handoff and no other request that shortens a session"

// handoffRefusedInATurn is what the pinned Oh My Pi answers a handoff asked for while a turn
// runs (`case "handoff"` in packages/coding-agent/src/modes/rpc/rpc-mode.ts of
// cybermonklabs/oh-my-pi at the pinned tag): a turn counts as running there from the admission of
// the text the plugin handed it, which comes before the turn's agent_start.
const handoffRefusedInATurn = "Cannot hand off while a response is in progress"

// unreadHandoffReason is the daemon's answer to a successful handoff whose data is no object
// (internal/stream's handoffResult), in those words.
const unreadHandoffReason = "Oh My Pi gave a handoff result that is not an object. The daemon cannot say if the session changed."

// handoffRowTask is the task each row's claim is spawned with and owes.
const handoffRowTask = "Work the issue."

// handoffShortened is the peer's answer to a handoff that shortened the session: success with the
// result object the pinned Oh My Pi answers a handoff the daemon asked for (no saved path).
func handoffShortened(id string) shimwire.Response {
	return shimwire.Response{ID: id, Command: handoffType, Success: true, Data: json.RawMessage(`{}`)}
}

// heldOmpRequest is a request the peer has not answered yet, on the connection it came on.
type heldOmpRequest struct {
	sh *shim
	id string
}

// ompPeer is the Oh My Pi behind a worker's shim, as far as the daemon's requests reach it. It
// outlives its connection, as the worker's process outlives a daemon restart: a handoff it is
// running goes on after the connection that asked for it is gone. It records the type of every
// line the daemon writes, read from the line's own `type` member, and each line of a request that
// shortens a session. It answers the protocol negotiation, and the state probe from the worker's
// plugin (streaming while a turn of the plugin's runs), and a handoff with what the row scripts
// (answer), or, where the row scripts none, holds the handoff until the row ends it (endHandoff).
// The pinned Oh My Pi runs a handoff and a get_state on one serial queue (`RpcInputDispatcher`
// in rpc-mode.ts at the pinned tag), so a state probe that comes while a handoff runs is answered
// only once that handoff is over. A `compact` is refused (handoffPeerRefusal); every other line
// is recorded and left unanswered, as the worker of a task bucket test leaves it.
type ompPeer struct {
	t *testing.T

	mu sync.Mutex
	// answer is what the peer answers a handoff with; nil holds the handoff (endHandoff).
	answer   func(id string) shimwire.Response
	types    []string
	shortens []string
	// handoff is the handoff the peer runs, nil for none, and states the state probes waiting
	// behind it.
	handoff *heldOmpRequest
	states  []heldOmpRequest
}

// answers sets what the peer answers each handoff with from now on; nil holds every handoff.
func (p *ompPeer) answers(answer func(id string) shimwire.Response) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.answer = answer
}

// serve reads every line the daemon writes on sh until the connection closes, and answers it for
// the worker w. An answer that fails to write is dropped: the daemon may have gone, and with it
// the request.
func (p *ompPeer) serve(w *bucketWorker, sh *shim) {
	_ = sh.conn.SetReadDeadline(time.Time{})
	reply := func(frame shimwire.Frame) { _ = sh.writer.WriteFrame(frame) }
	for {
		line, err := sh.lines.ReadBytes('\n')
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
		case shimwire.TypeGetState:
			p.mu.Lock()
			if p.handoff != nil {
				p.states = append(p.states, heldOmpRequest{sh: sh, id: head.ID})
				p.mu.Unlock()
				continue
			}
			p.mu.Unlock()
			reply(stateAnswer(head.ID, w.streaming()))
		case handoffType, compactType:
			p.mu.Lock()
			p.shortens = append(p.shortens, strings.TrimSpace(string(line)))
			answer := p.answer
			if head.Type == handoffType && answer == nil {
				p.handoff = &heldOmpRequest{sh: sh, id: head.ID}
			}
			p.mu.Unlock()
			switch {
			case head.Type == compactType:
				reply(shimwire.Response{ID: head.ID, Command: head.Type, Success: false, Error: handoffPeerRefusal})
			case answer != nil:
				reply(answer(head.ID))
			}
		}
	}
}

// stateAnswer is the agent's answer to the state probe id: streaming or not.
func stateAnswer(id string, streaming bool) shimwire.Response {
	data, _ := json.Marshal(shimwire.StateData{IsStreaming: &streaming})
	return shimwire.Response{ID: id, Command: shimwire.TypeGetState, Success: true, Data: data}
}

// running is whether the peer is running a handoff it holds.
func (p *ompPeer) running() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.handoff != nil
}

// statesHeld is how many state probes wait behind the handoff the peer runs.
func (p *ompPeer) statesHeld() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.states)
}

// endHandoff ends the handoff the peer holds: it answers the handoff with answer, on the
// connection that asked for it, and then each state probe that waited behind it, on its own
// connection, streaming or not. An answer whose connection is gone is lost, as Oh My Pi's is.
func (p *ompPeer) endHandoff(answer func(id string) shimwire.Response, streaming bool) {
	p.mu.Lock()
	handoff, states := p.handoff, p.states
	p.handoff, p.states = nil, nil
	p.mu.Unlock()
	if handoff != nil {
		_ = handoff.sh.writer.WriteFrame(answer(handoff.id))
	}
	for _, state := range states {
		_ = state.sh.writer.WriteFrame(stateAnswer(state.id, streaming))
	}
}

// seen is what the peer has recorded so far: the type of every line, and each line of a request
// that shortens a session.
func (p *ompPeer) seen() (types, shortens []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.types), slices.Clone(p.shortens)
}

// handoffRow is one row's daemon and the claim it compacts: the project's root architect, spawned
// owing handoffRowTask, connected through a bucketWorker, registered as its session and ready,
// and holding that task's take accepted (first).
type handoffRow struct {
	t     *testing.T
	r     *bucketRig
	w     *bucketWorker
	issue string
	first takenTask
}

// newHandoffRow boots the row's daemon and brings its claim to the accepted take of the task it
// owes. answer is what the peer answers a handoff with.
func newHandoffRow(t *testing.T, answer func(id string) shimwire.Response) *handoffRow {
	t.Helper()
	r := newBucketRig(t)
	r.boot()
	const issue = "LEGION-1"
	w := r.spawn(issue, handoffRowTask)
	w.omp.answers(answer)
	admitIssue(t, r.cfg, issue, claim.RoleArchitect, w.token)
	w.ready()
	row := &handoffRow{t: t, r: r, w: w, issue: issue}
	row.first = w.driver.mustTake(t, "the worker takes the task the claim owes")
	if row.first.Number == 0 || !strings.Contains(row.first.Text, handoffRowTask) {
		t.Fatalf("the worker took %+v, want the task the claim owes under its number", row.first)
	}
	return row
}

// owesItsTaskOnTheLadder runs the first take's turn and ends it in a provider error, which keeps
// the task on the claim under its number, untaken, and arms the ladder's first wait (supervise's
// providerErrored), which the rig's still clock never fires: the claim is idle and owes its task,
// as a claim whose provider keeps refusing its session is when a compaction is asked for.
func (row *handoffRow) owesItsTaskOnTheLadder() {
	row.t.Helper()
	d := row.w.driver
	d.mustStartTurn(row.t)
	d.turnOver()
	if err := d.send(shimwire.AgentEnd{}); err != nil {
		row.t.Fatalf("end the turn: %v", err)
	}
	if err := d.report(claim.TurnOutcomeRequest{Outcome: claim.TurnRefused, Message: "overloaded", Task: row.first.Number,
		ProviderError: &claim.ProviderError{Status: http.StatusServiceUnavailable, Provider: "anthropic", Model: "claude-opus", Message: "overloaded", Retryable: true}}); err != nil {
		row.t.Fatal(err)
	}
	testwait.Eventually(row.t, "the provider error to leave the claim idle owing its task untaken", func() bool {
		c := row.r.d.claim(row.w.token)
		return c.State == string(supervise.StateIdle) && c.Pending != nil && c.Pending.ConfirmedAt == nil
	})
	if p := storedClaim(row.t, row.r.cfg, row.w.token).Pending; p == nil || p.Number != row.first.Number || p.ProviderErrorRetries != 1 {
		row.t.Fatalf("the claim holds %+v after the provider error, want task %d kept on the ladder's first rung", p, row.first.Number)
	}
}

// handoffDoor is one of the two doors onto the compaction: the operator's route, which names the
// claim by its token and is what `legion claims compact` posts (cmd/legion's
// TestClaimsCompactPostsToTheClaimsCompactRoute; the live proof drives the built command itself,
// claim_compaction_live_test.go), and the controller's `compact`, which names it by its issue and
// role under a controller grant.
type handoffDoor struct {
	name string
	ask  func(row *handoffRow) (int, []byte)
}

func handoffDoors() []handoffDoor {
	return []handoffDoor{
		{"the operator's route legion claims compact posts", func(row *handoffRow) (int, []byte) {
			return row.r.d.request(http.MethodPost, "/legion/v1/operator/claims/"+string(row.w.token)+"/compact", nil, true)
		}},
		{"the controller's compact", func(row *handoffRow) (int, []byte) {
			return row.r.d.request(http.MethodPost, "/legion/v1/claims/compact",
				api.ClaimRequest{GrantID: row.controllerGrant(), Issue: row.issue, Role: string(claim.RoleArchitect)}, false)
		}},
	}
}

// controllerGrant registers the project's controller as the operator starts one (`legion
// controller start`: the operator's bearer buys the capability, the controller's Oh My Pi
// registers with it) and mints one grant of it, as its plugin does per operation.
func (row *handoffRow) controllerGrant() string {
	row.t.Helper()
	d := row.r.d
	status, body := d.request(http.MethodPost, "/legion/v1/controller/secret",
		api.ControllerSecretRequest{PluginContract: api.DaemonAPIVersion()}, true)
	if status != http.StatusOK {
		row.t.Fatalf("the controller's secret = %d; body %s", status, body)
	}
	var capability api.ControllerSecretResponse
	if err := json.Unmarshal(body, &capability); err != nil {
		row.t.Fatalf("decode %s: %v", body, err)
	}
	const session = "ses_controller"
	status, body = d.request(http.MethodPost, "/legion/v1/claims/register", claim.RegisterRequest{
		BootToken: capability.Secret, SessionID: session, OmpSessionFile: "/sessions/" + session + ".jsonl",
		AgentID: session, PluginContract: api.DaemonAPIVersion(),
	}, false)
	if status != http.StatusOK {
		row.t.Fatalf("register the controller = %d; body %s", status, body)
	}
	var registration api.ControllerRegisterResponse
	if err := json.Unmarshal(body, &registration); err != nil {
		row.t.Fatalf("decode %s: %v", body, err)
	}
	status, body = d.request(http.MethodPost, "/legion/v1/grants",
		api.GrantRequest{SessionID: session, Secret: registration.Secret}, false)
	if status != http.StatusOK {
		row.t.Fatalf("the controller's grant = %d; body %s", status, body)
	}
	var granted api.GrantResponse
	if err := json.Unmarshal(body, &granted); err != nil || granted.GrantID == "" {
		row.t.Fatalf("decode the controller's grant %s: %v", body, err)
	}
	return granted.GrantID
}

// compact asks for the compaction through door and answers what the daemon said of it.
func (row *handoffRow) compact(door handoffDoor) api.CompactResponse {
	row.t.Helper()
	status, body := door.ask(row)
	if status != http.StatusOK {
		row.t.Fatalf("%s = %d, want 200; body %s", door.name, status, body)
	}
	var answer api.CompactResponse
	if err := json.Unmarshal(body, &answer); err != nil {
		row.t.Fatalf("decode the compaction's answer %s: %v", body, err)
	}
	return answer
}

// wantOneHandoff holds that the daemon wrote exactly one request that shortens a session, a
// handoff carrying the compaction's instructions, no `compact` line at all, and no prompt: the
// worker takes its task.
func (row *handoffRow) wantOneHandoff() {
	row.t.Helper()
	wantOneHandoff(row.t, row.w.omp)
}

func wantOneHandoff(t *testing.T, peer *ompPeer) {
	t.Helper()
	types, shortens := peer.seen()
	if slices.Contains(types, compactType) || slices.Contains(types, promptType) {
		t.Errorf("the daemon wrote a %q or %q line on the worker stream; the lines it wrote are of types %q", compactType, promptType, types)
	}
	if len(shortens) != 1 {
		t.Fatalf("the daemon wrote %d requests that shorten a session, %q, want one handoff", len(shortens), shortens)
	}
	var asked struct {
		Type               string `json:"type"`
		ID                 string `json:"id"`
		CustomInstructions string `json:"customInstructions"`
	}
	if err := json.Unmarshal([]byte(shortens[0]), &asked); err != nil {
		t.Fatalf("decode %q: %v", shortens[0], err)
	}
	if asked.Type != handoffType || asked.ID == "" || !strings.HasPrefix(asked.CustomInstructions, "Write the summary as text.") {
		t.Fatalf("the daemon wrote %q, want {type: %q, id, customInstructions: the compaction's instructions}", shortens[0], handoffType)
	}
}

// wantTheOwedTaskOfferedOnceAfter holds that the claim kept its session and that the task it owes,
// untaken when the compaction was asked for, was offered once after the compaction: its value
// written once more than the writes before it (written), and taken once, under its own number.
func (row *handoffRow) wantTheOwedTaskOfferedOnceAfter(written int) {
	row.t.Helper()
	r, w := row.r, row.w
	again := w.driver.mustTake(row.t, "the worker takes the owed task offered after the compaction")
	if again.Number != row.first.Number || !strings.Contains(again.Text, handoffRowTask) {
		row.t.Fatalf("the worker took %+v after the compaction, want task %d, the task the claim owes", again, row.first.Number)
	}
	if c := r.d.claim(w.token); c.Session != w.session {
		row.t.Errorf("the claim is on session %q after the compaction, want the session it had, %q", c.Session, w.session)
	}
	time.Sleep(handoffQuiet)
	if causes := r.writes(w.token); len(causes) != written+1 {
		row.t.Fatalf("the daemon wrote the claim's value for %v, want the %d writes before the compaction and one after it", causes, written)
	}
	if n := r.accepted(w.token); n != 2 {
		row.t.Fatalf("the daemon accepted %d takes of the task, want two: the first and the one after the compaction", n)
	}
}

// handoffQuiet is how long a row listens for a write or a take that must not come. The daemon
// writes the owed task's value as the compaction ends, before its answer reaches the caller
// (supervise's compactEnded), so a second write arrives within milliseconds of the answer: a
// second is ample, and the control at a daemon that writes it twice shows it caught.
const handoffQuiet = time.Second

// A compaction through either door is one handoff and no `compact` (dispatch://INFRA-5798 R1),
// and a take accepted when the handoff is asked for is taken once (departure 10 of
// dispatch://INFRA-2811's plan, row H7). The pinned Oh My Pi refuses a handoff while a turn runs,
// and counts a turn as running from its admission of the text the plugin handed it
// (handoffRefusedInATurn); text it receives while a handoff runs it parks, and starts that text's
// turn once the handoff is over. Either way the handoff aborts no turn and the take's own turn
// runs the task: the daemon keeps the take, offers the task again nowhere, and the turn's report
// finishes it once. Taken back and offered again, as the daemon did around a `compact`, which
// could abort the turn, the task would reach the agent twice.
//
// A task the claim owes untaken when the handoff is asked for — one the provider-error ladder
// holds — is offered once, after the compaction: its value is written once and taken once, and
// the claim keeps its session.
//
// Its control is a run at a daemon whose compaction's start takes the accepted take back, as it
// did before #5567's handoff: the value is written again (`retake`), and the worker, idle after
// its take's turn, takes the task a second time.
func TestTaskValueATakeAcceptedWhenTheHandoffIsAskedForIsTakenOnce(t *testing.T) {
	t.Parallel()
	for _, door := range handoffDoors() {
		t.Run(door.name, func(t *testing.T) {
			t.Parallel()
			t.Run("an untaken task the ladder holds is offered once after the handoff", func(t *testing.T) {
				t.Parallel()
				row := newHandoffRow(t, handoffShortened)
				row.owesItsTaskOnTheLadder()
				written := len(row.r.writes(row.w.token))

				answer := row.compact(door)

				if !answer.Compacted || answer.Reason != "" {
					t.Errorf("the compaction answered %+v, want the session shortened with no reason", answer)
				}
				row.wantOneHandoff()
				row.wantTheOwedTaskOfferedOnceAfter(written)
			})
			for _, tc := range []struct {
				name   string
				answer func(id string) shimwire.Response
				want   api.CompactResponse
			}{
				{
					name: "an accepted take whose text Oh My Pi admitted, so it refuses the handoff in that turn",
					answer: func(id string) shimwire.Response {
						return shimwire.Response{ID: id, Command: handoffType, Success: false, Error: handoffRefusedInATurn}
					},
					want: api.CompactResponse{Compacted: false, Reason: handoffRefusedInATurn},
				},
				{
					name:   "an accepted take whose text Oh My Pi parks behind a handoff that shortens the session",
					answer: handoffShortened,
					want:   api.CompactResponse{Compacted: true},
				},
			} {
				t.Run(tc.name, func(t *testing.T) {
					t.Parallel()
					row := newHandoffRow(t, tc.answer)
					r, w := row.r, row.w
					written := len(r.writes(w.token))

					answer := row.compact(door)

					if answer != tc.want {
						t.Errorf("the compaction answered %+v, want %+v", answer, tc.want)
					}
					row.wantOneHandoff()
					time.Sleep(handoffQuiet)
					if causes := r.writes(w.token); len(causes) != written {
						t.Errorf("the daemon wrote the claim's value for %v, want nothing after the %d before the compaction: the take stands", causes, written)
					}
					// Not fatal: a daemon that took the take back at the compaction, as it did
					// around a `compact`, is caught below by the second take it accepts.
					c := r.d.claim(w.token)
					if c.Pending == nil || c.Pending.ConfirmedAt == nil || c.Session != w.session {
						t.Errorf("after the compaction the claim holds %+v on session %q, want the accepted take kept on %q", c.Pending, c.Session, w.session)
					}

					// The take's text runs its turn, whose report names the take's number; then
					// the plugin, idle again, looks at the claim's value once more, as it does at
					// every idle point, and takes whatever the daemon offers it.
					w.driver.mustStartTurn(t)
					w.driver.mustEndTurn(t)
					time.Sleep(handoffQuiet)
					if _, again, asked, err := w.driver.poll(); err != nil || asked {
						t.Errorf("after the take's turn the worker took again: %+v (asked %t, %v), want no value left to take", again, asked, err)
					}
					if n := r.accepted(w.token); n != 1 {
						t.Fatalf("the daemon accepted %d takes of the task, want the one accepted before the handoff", n)
					}
					r.awaitEntry("the finished task's value deleted", string(w.token), func(e jetstream.KeyValueEntry) bool { return e == nil })
					testwait.Eventually(t, "the take's report to finish the task", func() bool {
						c := r.d.claim(w.token)
						return c.State == string(supervise.StateIdle) && c.Pending == nil
					})
					if finished := storedClaim(t, r.cfg, w.token).TaskFinished; finished != row.first.Number {
						t.Errorf("the claim finished task %d, want %d, the take's", finished, row.first.Number)
					}
				})
			}
		})
	}
}

// The answer of a compaction says whether the session was shortened and, when it was not, why
// (dispatch://INFRA-5798 R2): Oh My Pi's own words for a handoff it refused, and the daemon's for
// a handoff it answered with success and no handoff (`data: null`), which committed nothing. A
// success that carries the handoff's result object is a session shortened, with a saved path or
// without one: the pinned Oh My Pi saves the document to a file only for a handoff it starts
// itself, so a handoff the daemon asks for is answered `data: {}`. A success whose data is no
// object — a boolean, an array — is an answer the daemon cannot read, so it claims no change of
// the session. In every row the claim is back and the task it owes is offered once and taken once.
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
			name:   "success with no saved path",
			answer: handoffShortened,
			want:   api.CompactResponse{Compacted: true},
		},
		{
			// Any object is a result: the daemon reads none of its members.
			name: "success with an object whose savedPath is no string",
			answer: func(id string) shimwire.Response {
				return shimwire.Response{ID: id, Command: handoffType, Success: true, Data: json.RawMessage(`{"savedPath":3}`)}
			},
			want: api.CompactResponse{Compacted: true},
		},
		{
			name: "success with data true",
			answer: func(id string) shimwire.Response {
				return shimwire.Response{ID: id, Command: handoffType, Success: true, Data: json.RawMessage(`true`)}
			},
			want: api.CompactResponse{Compacted: false, Reason: unreadHandoffReason},
		},
		{
			name: "success with an array",
			answer: func(id string) shimwire.Response {
				return shimwire.Response{ID: id, Command: handoffType, Success: true, Data: json.RawMessage(`[]`)}
			},
			want: api.CompactResponse{Compacted: false, Reason: unreadHandoffReason},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			row := newHandoffRow(t, tc.answer)
			row.owesItsTaskOnTheLadder()
			written := len(row.r.writes(row.w.token))

			answer := row.compact(handoffDoors()[0])

			if answer != tc.want {
				t.Errorf("the compaction answered %+v, want %+v", answer, tc.want)
			}
			row.wantOneHandoff()
			row.wantTheOwedTaskOfferedOnceAfter(written)
			if state := row.r.d.claim(row.w.token).State; state != string(supervise.StateIdle) {
				t.Errorf("the claim is %s after the compaction, want it back idle", state)
			}
		})
	}
}
