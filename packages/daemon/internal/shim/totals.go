package shim

import (
	"cmp"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"slices"
	"sync"

	"github.com/sjawhar/envoy/steprecord"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
)

// The model totals. The shim counts the calls of its launch by the model that answered each one.
// It answers the daemon's reads of those counts (shimwire.ModelTotals). The shim reads every
// frame of the harness before a connection to the daemon can lose one. So it counts what a daemon
// that was away or restarted never saw.
//
// A call is one whole message_end of the harness, an rpc_chunk sequence joined first. Its message
// is the assistant's, names a model that is not shimwire.SyntheticModel, and did not end in an
// error. Oh My Pi writes one for each response of the agent's own conversation, and none for a
// subagent's. A subagent's events reach its RPC stream only after set_subagent_subscription,
// which Legion never sends.
//
// The set model is the model the launch's session started on. Oh My Pi names it in its answer
// to get_state, which the shim asks for on the harness's ready frame. An extension can start a
// turn before that answer, so the shim takes it only where no call came before it. The Claude
// Code bridge names it in a legion_set_model frame instead.
//
// A read waits for the harness: the shim sends it a get_state of its own and answers the daemon
// once the harness has answered. The harness writes its frames in order, so the totals then hold
// each response it ended before the read.

const (
	// ownPrefix starts the id of each get_state the shim sends the harness itself. The harness's
	// answer to it is the shim's own: it goes neither to the daemon nor to the dedupe.
	ownPrefix = "legion-shim-"
	// maxWaitingReads is how many reads of the totals may wait for the harness at one time.
	maxWaitingReads = 16
)

var (
	// refusedExited refuses a read once the harness has exited: the final totals answer it.
	refusedExited = "the harness has exited, and its final totals follow"
	// refusedWaiting refuses a read while maxWaitingReads others wait for the harness.
	refusedWaiting = fmt.Sprintf("%d reads already wait for the harness", maxWaitingReads)
)

// totals is what the shim counted of its launch. Its zero value is ready. The pump counts, the
// daemon's reads arrive on the connection's goroutine, and reap ends it, so it has a lock of its
// own.
type totals struct {
	mu          sync.Mutex
	counts      map[string]uint64
	outsideForm uint64
	overflowed  bool
	// read numbers the reads the shim answered, and the final frame.
	read uint64
	// setModel is the launch's set model, and setModelDecided whether the shim has taken its
	// answer. An undecided set model may still be named. A decided "" never is.
	setModel        string
	setModelDecided bool
	// readyID is the get_state the shim sent on the harness's ready frame, "" before it.
	readyID string
	// pending maps each get_state of a read the harness has not answered to the daemon's id of
	// that read.
	pending map[string]string
	// exited is set once the harness has exited and the shim has made its final totals.
	exited bool
}

// called is whether any call came, counted by name or not. The caller holds t.mu.
func (t *totals) called() bool {
	return len(t.counts) > 0 || t.outsideForm > 0 || t.overflowed
}

// setModelName is the set model a frame names by provider and id: their name (shimwire.ModelName),
// or "" where the id is empty or the name is outside the form of a step's agent text.
func setModelName(provider, id string) string {
	if name := shimwire.ModelName(provider, id); id != "" && steprecord.AgentTextHolds(name) {
		return name
	}
	return ""
}

// call counts the message_end frame raw where it is a call.
func (t *totals) call(raw json.RawMessage) {
	var frame struct {
		Message struct {
			Role       string `json:"role"`
			Provider   string `json:"provider"`
			Model      string `json:"model"`
			StopReason string `json:"stopReason"`
		} `json:"message"`
	}
	if json.Unmarshal(raw, &frame) != nil {
		return
	}
	m := frame.Message
	if m.Role != "assistant" || m.Model == "" || m.Model == shimwire.SyntheticModel || m.StopReason == "error" {
		return
	}
	name := shimwire.ModelName(m.Provider, m.Model)
	t.mu.Lock()
	defer t.mu.Unlock()
	if !steprecord.AgentTextHolds(name) {
		t.outsideForm++
		return
	}
	if _, counted := t.counts[name]; !counted && len(t.counts) >= shimwire.MaxTotalsModels {
		t.overflowed = true
		return
	}
	if t.counts == nil {
		t.counts = map[string]uint64{}
	}
	t.counts[name]++
}

// askReady is the id of the get_state the shim sends the harness on its first ready frame, and
// "" on every later one.
func (t *totals) askReady() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.readyID != "" {
		return ""
	}
	t.readyID = ownPrefix + "ready-" + rand.Text()
	return t.readyID
}

// bridgeNames takes the set model a legion_set_model frame names, where none is decided yet.
func (t *totals) bridgeNames(f shimwire.LegionSetModel) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.setModelDecided {
		return
	}
	t.setModelDecided = true
	t.setModel = setModelName(f.Provider, f.ID)
}

// ask takes the daemon's read id: it answers the id of the get_state to send the harness, or the
// reason the read is refused.
func (t *totals) ask(readID string) (stateID, refusal string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	switch {
	case t.exited:
		return "", refusedExited
	case len(t.pending) >= maxWaitingReads:
		return "", refusedWaiting
	}
	if t.pending == nil {
		t.pending = map[string]string{}
	}
	stateID = ownPrefix + "totals-" + rand.Text()
	t.pending[stateID] = readID
	return stateID, ""
}

// answered takes the harness's answer to a get_state of the shim's own. The ready one decides
// the set model, and a read's makes the result the daemon gets, which is nil for any other.
func (t *totals) answered(response shimwire.Response) *shimwire.ModelTotalsResult {
	t.mu.Lock()
	defer t.mu.Unlock()
	if response.ID == t.readyID && !t.setModelDecided {
		t.setModelDecided = true
		var state struct {
			Model *struct {
				Provider string `json:"provider"`
				ID       string `json:"id"`
			} `json:"model"`
		}
		if !t.called() && response.Success && json.Unmarshal(response.Data, &state) == nil && state.Model != nil {
			t.setModel = setModelName(state.Model.Provider, state.Model.ID)
		}
	}
	if readID, waiting := t.pending[response.ID]; waiting {
		delete(t.pending, response.ID)
		t.read++
		answer := t.snapshot()
		return &shimwire.ModelTotalsResult{ID: readID, OK: true, Totals: &answer}
	}
	return nil
}

// exit ends the reads once the harness has exited: it refuses each read still waiting, and every
// later one, and makes the final totals.
func (t *totals) exit() ([]shimwire.ModelTotalsResult, shimwire.ModelTotalsFinal) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.exited = true
	refused := make([]shimwire.ModelTotalsResult, 0, len(t.pending))
	for _, readID := range t.pending {
		refused = append(refused, shimwire.ModelTotalsResult{ID: readID, Error: refusedExited})
	}
	slices.SortFunc(refused, func(a, b shimwire.ModelTotalsResult) int { return cmp.Compare(a.ID, b.ID) })
	t.pending = nil
	t.read++
	return refused, shimwire.ModelTotalsFinal{Totals: t.snapshot()}
}

// snapshot is the totals as the wire carries them, counts sorted by model. The counts are an
// empty list where no call came, never null, so no reader has to tell the two apart. The caller
// holds t.mu.
func (t *totals) snapshot() shimwire.Totals {
	counts := make([]shimwire.ModelCount, 0, len(t.counts))
	for model, calls := range t.counts {
		counts = append(counts, shimwire.ModelCount{Model: model, Calls: calls})
	}
	slices.SortFunc(counts, func(a, b shimwire.ModelCount) int { return cmp.Compare(a.Model, b.Model) })
	return shimwire.Totals{
		Read:        t.read,
		SetModel:    t.setModel,
		Counts:      counts,
		OutsideForm: t.outsideForm,
		Overflowed:  t.overflowed,
	}
}
