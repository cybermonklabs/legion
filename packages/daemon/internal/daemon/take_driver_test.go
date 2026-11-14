package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// takeDriver is a worker's plugin taking its claim's task over the daemon's own routes and its
// worker stream, as pi-legion does now that the daemon pushes no task (dispatch://INFRA-2811): it
// reads the claim's value through POST /legion/v1/claims/task, reports each value it has not seen
// (claims/task-seen) and, while no turn of its own runs, asks to take the number the value names
// (claims/take). A take accepted is the task of the next turn it starts (agent_start on the
// stream), and the end of that turn (agent_end) is reported naming the number
// (claims/turn-outcome), which is what finishes the task. It asks once per value, as the plugin
// does, so a take a hold refused is asked again only at the value the hold's end writes.
//
// The value is the one the daemon's task bucket holds: a test's daemon is given one in memory
// (fakeRuntime), or the daemon's own on the test NATS. The driver reads it from the record's own
// route rather than from a relayed frame, so nothing of Envoy's relay stands between the test and
// the take.
//
// Every test that speaks the daemon's real routes and stream drives its worker through it, tagged
// e2e or not, and so does the idle rig's role process, which runs outside any test. So it answers a
// failure as an error, and the wrappers below are what fail a test.
type takeDriver struct {
	// post sends body to the daemon's route at path and decodes a 2xx answer into answer, which
	// nil leaves unread; any other status is an error naming it.
	post func(path string, body, answer any) error
	// send writes one frame on the claim's worker stream, as the agent's shim forwards it.
	send       func(shimwire.Frame) error
	caller     claim.AgentCaller
	generation uint64

	mu sync.Mutex
	// seen and asked are the values the driver last reported seen and last asked to take at, the
	// zero value before the first of each.
	seen, asked claim.TaskValue
	// accepted is the number of the take accepted whose turn has not started, and running the
	// number of the take whose turn runs; 0 is none.
	accepted, running uint64
	inTurn            bool
	// sequence counts the turns the driver reported, which each report carries.
	sequence uint64
}

// takenTask is a take the daemon accepted: the number taken and the text its turn runs.
type takenTask struct {
	Number uint64
	Text   string
}

// newTakeDriver is the plugin of the session that registered as caller at generation, posting
// through post and writing frames through send.
func newTakeDriver(post func(path string, body, answer any) error, send func(shimwire.Frame) error,
	caller claim.AgentCaller, generation uint64) *takeDriver {
	return &takeDriver{post: post, send: send, caller: caller, generation: generation}
}

// takeDriver is the plugin of the session that registered as session with secret on token's claim
// at generation, posting to d's routes and writing on sh.
func (d *daemon) takeDriver(sh *shim, token claim.Token, session, secret string, generation uint64) *takeDriver {
	return newTakeDriver(d.post, sh.writer.WriteFrame,
		claim.AgentCaller{ClaimToken: token, SessionID: session, Secret: secret}, generation)
}

// post sends body to the route at path and decodes a 2xx answer into answer (nil reads none). It
// reports a failure rather than failing the test, so a driver may post from a goroutine of its
// own.
func (d *daemon) post(path string, body, answer any) error {
	return postJSON(d.client, d.base+path, body, answer)
}

// postJSON posts body as JSON to url with client and decodes a 2xx answer into answer (nil reads
// none); any other status is an error naming it and the body the daemon answered.
func postJSON(client *http.Client, url string, body, answer any) error {
	encoded, err := json.Marshal(body)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(context.Background(), http.MethodPost, url, bytes.NewReader(encoded))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	read, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("POST %s = %d %s", request.URL.Path, response.StatusCode, bytes.TrimSpace(read))
	}
	if answer == nil || len(bytes.TrimSpace(read)) == 0 {
		return nil
	}
	return json.Unmarshal(read, answer)
}

// task is the claim's value now, nil where the bucket holds none, and the number of the last task
// the claim finished.
func (w *takeDriver) task() (claim.TaskResponse, error) {
	var answer claim.TaskResponse
	err := w.post("/legion/v1/claims/task", claim.TaskRequest{AgentCaller: w.caller, Generation: w.generation}, &answer)
	return answer, err
}

// poll is one look at the claim's value, as the plugin takes each value it is handed: a value it
// has not reported is reported seen, and one it has not asked at is taken while no turn of its own
// runs and no take it was answered waits for its turn. It answers the take it made and true, or
// false when it asked nothing.
func (w *takeDriver) poll() (claim.TaskValue, claim.TakeResponse, bool, error) {
	current, err := w.task()
	if err != nil || current.Task == nil {
		return claim.TaskValue{}, claim.TakeResponse{}, false, err
	}
	value := *current.Task
	w.mu.Lock()
	report := value != w.seen
	ask := value != w.asked && !w.inTurn && w.accepted == 0
	w.mu.Unlock()
	if report {
		if err := w.post("/legion/v1/claims/task-seen", claim.TaskSeenRequest{AgentCaller: w.caller,
			Generation: w.generation, Number: value.Number, Epoch: value.Epoch, Revision: value.Revision}, nil); err != nil {
			return claim.TaskValue{}, claim.TakeResponse{}, false, fmt.Errorf("report task %d seen: %w", value.Number, err)
		}
		w.mu.Lock()
		w.seen = value
		w.mu.Unlock()
	}
	if !ask {
		return claim.TaskValue{}, claim.TakeResponse{}, false, nil
	}
	var answer claim.TakeResponse
	if err := w.post("/legion/v1/claims/take", claim.TakeRequest{AgentCaller: w.caller, Generation: w.generation,
		Number: value.Number, Epoch: value.Epoch, Revision: value.Revision}, &answer); err != nil {
		return claim.TaskValue{}, claim.TakeResponse{}, false, fmt.Errorf("take task %d: %w", value.Number, err)
	}
	w.mu.Lock()
	w.asked = value
	if answer.Outcome == string(supervise.TakeOutcomeAccepted) {
		w.accepted = value.Number
	}
	w.mu.Unlock()
	return value, answer, true, nil
}

// takeWithin polls until a take is accepted, and answers it, or until within has passed, which
// is an error naming the last take a hold refused. A take refused waits for the value the hold's
// end writes, and a take dropped for the next value, as the plugin's do.
func (w *takeDriver) takeWithin(within time.Duration) (takenTask, error) {
	deadline := time.Now().Add(within)
	last := "no value to take"
	for {
		value, answer, asked, err := w.poll()
		switch {
		case err != nil:
			return takenTask{}, err
		case asked && answer.Outcome == string(supervise.TakeOutcomeAccepted):
			return takenTask{Number: value.Number, Text: answer.Text}, nil
		case asked:
			last = fmt.Sprintf("task %d answered %+v", value.Number, answer)
		}
		if time.Now().After(deadline) {
			return takenTask{}, fmt.Errorf("no take was accepted within %s: %s", within, last)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// startTurn is the agent beginning a turn: the take accepted last is that turn's task, and a turn
// begun with no take accepted is one of the agent's own. The turn of a take returns once the
// daemon has read its start (turnSeen), as a real turn outlasts the read of its agent_start:
// the driver's turns take no time, and a report the HTTP route carried ahead of the stream's
// agent_start would reach a task whose turn the daemon had not seen begin.
func (w *takeDriver) startTurn() error {
	w.mu.Lock()
	w.inTurn, w.running, w.accepted = true, w.accepted, 0
	running, asked := w.running, w.asked
	w.mu.Unlock()
	if err := w.send(shimwire.AgentStart{}); err != nil || running == 0 {
		return err
	}
	return w.turnSeen(asked)
}

// turnSeen waits until the daemon has read the start of the turn of the take accepted at value.
// It asks the take again, which is the take a worker sends when its answer was lost and which
// the record answers alone, applying nothing (supervise's answeredFromRecord): accepted again
// while that take awaits its turn, refused in-turn once its turn has started. Any other answer
// says the task is no longer that take's — taken back, finished or dropped — and ends the wait.
func (w *takeDriver) turnSeen(value claim.TaskValue) error {
	deadline := time.Now().Add(10 * time.Second)
	for {
		var answer claim.TakeResponse
		if err := w.post("/legion/v1/claims/take", claim.TakeRequest{AgentCaller: w.caller, Generation: w.generation,
			Number: value.Number, Epoch: value.Epoch, Revision: value.Revision}, &answer); err != nil {
			return fmt.Errorf("ask whether the turn of task %d started: %w", value.Number, err)
		}
		if answer.Outcome != string(supervise.TakeOutcomeAccepted) {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the daemon read no start of the turn of task %d within 10s", value.Number)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// endTurn is the agent finishing its turn: the agent_end, and behind it the turn's report naming
// the number of the take the turn ran, which finishes that task. A turn of the agent's own ends
// with no report, as the turn of a worker whose plugin took nothing does.
func (w *takeDriver) endTurn() error {
	w.mu.Lock()
	running := w.running
	w.inTurn, w.running = false, 0
	w.mu.Unlock()
	if err := w.send(shimwire.AgentEnd{}); err != nil || running == 0 {
		return err
	}
	return w.report(claim.TurnOutcomeRequest{Outcome: claim.TurnClean, Task: running})
}

// inATurn is whether a turn of the agent's is running, which is what its answer to the daemon's
// get_state says (isStreaming).
func (w *takeDriver) inATurn() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.inTurn
}

// watch is the plugin taking each task as its value comes, for as long as t runs: it reads the
// claim's value every 50 ms and, for each take the daemon accepts, starts that take's turn and
// then hands the task to took, as the agent's host starts a turn on the text it took. A read the
// daemon did not answer — it is restarting, or it has stopped — is logged and tried again.
func (w *takeDriver) watch(t *testing.T, took func(takenTask)) {
	stop, done := make(chan struct{}), make(chan struct{})
	t.Cleanup(func() {
		close(stop)
		<-done
	})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			case <-time.After(50 * time.Millisecond):
			}
			value, answer, asked, err := w.poll()
			switch {
			case err != nil:
				t.Logf("the worker of %s read its task: %v", w.caller.ClaimToken, err)
			case asked && answer.Outcome == string(supervise.TakeOutcomeAccepted):
				if err := w.startTurn(); err != nil {
					t.Errorf("the worker of %s started the turn of task %d: %v", w.caller.ClaimToken, value.Number, err)
					continue
				}
				took(takenTask{Number: value.Number, Text: answer.Text})
			}
		}
	}()
}

// report posts one turn's outcome under the driver's caller, generation and next sequence.
func (w *takeDriver) report(outcome claim.TurnOutcomeRequest) error {
	w.mu.Lock()
	w.sequence++
	outcome.AgentCaller, outcome.Generation, outcome.Sequence = w.caller, w.generation, w.sequence
	w.mu.Unlock()
	if err := w.post("/legion/v1/claims/turn-outcome", outcome, nil); err != nil {
		return fmt.Errorf("report the end of the turn of task %d: %w", outcome.Task, err)
	}
	return nil
}

// mustTake is takeWithin failing t: the claim's next task, taken within the bound
// testwait.Eventually gives anything a daemon test causes.
func (w *takeDriver) mustTake(t *testing.T, what string) takenTask {
	t.Helper()
	taken, err := w.takeWithin(testwait.Bounded(t, time.Minute, time.Second))
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
	return taken
}

// mustStartTurn is startTurn failing t.
func (w *takeDriver) mustStartTurn(t *testing.T) {
	t.Helper()
	if err := w.startTurn(); err != nil {
		t.Fatalf("start the turn: %v", err)
	}
}

// mustEndTurn is endTurn failing t.
func (w *takeDriver) mustEndTurn(t *testing.T) {
	t.Helper()
	if err := w.endTurn(); err != nil {
		t.Fatalf("end the turn: %v", err)
	}
}
