// Package replay replays a recorded window of a Legion daemon's project (Window) through the
// daemon's own handler chain (admit.Handlers: the Dispatch mirror, the workflow engine and the
// admission pass) under a named slot rule (record.SlotRule), on a simulated clock and in a
// database of its own, and measures the record and each replay with one code (Scorecard). It is
// what `legion replay` runs: a rule is judged by the daemon's own decisions over a window the
// daemon lived through, not by a copy of its rules.
package replay

import (
	"context"
	"errors"
	"time"

	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// RunConfig is one replay of a window: the window, the rule and branch it runs under, and where
// it runs and writes.
type RunConfig struct {
	// Window is the recorded window (ReadWindow).
	Window Window
	// Rule is the slot rule the replay's admission pass and engine run (record.SlotRuleNamed).
	Rule record.SlotRule
	// Branch is the value of each knob the record does not give.
	Branch Branch
	// Seed seeds every draw the replay makes: the experiments' and the conflicts'.
	Seed int64
	// Slowdown is the factor f by which a work segment's recorded length is scaled for each 42
	// issues in flight above the record's; 1.0 keeps the recorded lengths.
	Slowdown float64
	// PGDSN is a Postgres server the replay may create a database on. The replay writes only the
	// database it makes there, never the one the DSN names.
	PGDSN string
	// Out is the directory the replay writes to: log.jsonl, the daemon's lines stamped with the
	// simulated time; scorecard.json; run.json; and segments.jsonl under TraceSegments.
	Out string
	// KeepDB keeps the replay's database after the run, where it is otherwise dropped.
	KeepDB bool
	// TraceSegments writes each work segment's n, m and length (Segment) to Out and to the result.
	TraceSegments bool
}

// RunResult is what one replay made and measured.
type RunResult struct {
	// Database is the name of the database the replay made on RunConfig.PGDSN's server
	// (legion_replay_<run id>), which is dropped at the end of the run unless RunConfig.KeepDB
	// kept it.
	Database string
	// Scorecard is the replay's measures, read from its own log.
	Scorecard Scorecard
	// Segments is each work segment the replay ran, under RunConfig.TraceSegments.
	Segments []Segment
	// Unplaced is each issue the start state could not place at the window's start.
	Unplaced []Unplaced
}

// Segment is one work segment of an issue in a replay: the phase it worked, when it started, the
// issues in flight at its start in the replay (N) and in the record (M), its recorded length and
// the length the replay gave it, Recorded × f^((N − M)/42).
type Segment struct {
	Issue    string      `json:"issue"`
	Phase    phase.Phase `json:"phase"`
	Start    time.Time   `json:"start"`
	N        int         `json:"n"`
	M        int         `json:"m"`
	Recorded Duration    `json:"recorded"`
	Length   Duration    `json:"length"`
}

// Unplaced is an issue the replay's start state could not place at the window's start, and why.
type Unplaced struct {
	Issue  string `json:"issue"`
	Reason string `json:"reason"`
}

// Run replays cfg.Window under cfg.Rule and cfg.Branch. It makes a database of its own on
// cfg.PGDSN's server, migrated with the daemon's migrations; applies every fact of the window —
// the project's Dispatch events, the recorded phase moves, the ticks and the boots — each in one
// pass of the daemon's handler chain (intake.ApplyFact over admit.Handlers), with the rule set on
// the engine and the admission pass and one simulated clock set on the engine, the admission
// pass and the store; runs a worker stub over the outbox rows the daemon's lanes would take after
// each fact; and measures the replay from the log it writes. It makes no request outside the
// database: no Dispatch, no GitHub and no NATS.
func Run(ctx context.Context, cfg RunConfig) (RunResult, error) {
	return RunResult{}, errors.New("replay: not built yet")
}
