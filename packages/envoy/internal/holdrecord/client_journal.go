package holdrecord

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/sjawhar/envoy/natsgrant"
)

// StartJournal inserts the `done` record at `expected_gen: 0`, for `hold prepare` and `hold set`,
// with the step-1 map its caller read at step 1, before the row turns `held`.
func (c *Client) StartJournal(ctx context.Context, holdID string, w Workflow, step1 map[string]string) error {
	if len(current.rows[w]) == 0 {
		return fmt.Errorf("holdrecord: %q is no workflow of the hold's table", w)
	}
	return c.writeJournal(ctx, holdID, 0, Done{Workflow: w, Step1: step1})
}

// Journal returns the verified `done` record and its `gen`.
func (c *Client) Journal(ctx context.Context, holdID string) (Done, uint64, error) {
	records, err := c.Records(ctx, holdID, journalKind)
	if err != nil {
		return Done{}, 0, err
	}
	for _, record := range records {
		if record.Key != journalKey {
			continue
		}
		var d Done
		if err := json.Unmarshal(record.Value, &d); err != nil {
			return Done{}, 0, fmt.Errorf("holdrecord: hold %s's %s record does not decode: %s", holdID, journalKind, jsonFailure(err))
		}
		return d, record.Gen, nil
	}
	return Done{}, 0, fmt.Errorf("holdrecord: hold %s has no done record", holdID)
}

// Admit is a command's admission and its `intent` entry in one write. It reads and verifies the
// row, which must be `held` with holdID, or for CommandSet alone `prepare` with holdID, or for
// CommandAbort alone `released` with holdID, else ErrNotHeld, and the `done` record. It sets
// Request.RowReleased from the row, so on a released row Decide admits abort only while the
// journal shows prepare. It takes Decide's answer, and on nil writes the command's
// `intent` entry, with inputs, by a PutRecord at the `gen` it read, so the decision and the write
// are one compare-and-swap. A 409 `gen_mismatch` means another write came between, and Admit
// reads and decides again. The entry sets `complete` to false, except `hold release`'s, which
// keeps it. CommandAbort's entry also carries each stop entry's record of what its command left,
// under `left.<the stop's index in the journal>` as `<command>: <left>`, so the operator ends the
// hold knowing the state; any key of inputs' under `left.` is dropped, so those records are the
// journal's alone, and Entry.Left reads them back. The entry names the row Admit read
// (Operation.Row), the log entry it was admitted under. It returns the record's workflow.
//
// Admit refuses another command while one's intent is pending (ErrBusy), but admits the same
// command again, its rerun after a crash: a second process running the same command at once is
// admitted too. The operator runs one command of a hold at a time (assumption A2); the journal
// cannot tell a rerun from a second process.
func (c *Client) Admit(ctx context.Context, holdID string, req Request, inputs map[string]string) (Workflow, error) {
	for attempt := 1; ; attempt++ {
		row, err := c.Row(ctx)
		if err != nil {
			return "", err
		}
		released := req.Command == CommandAbort && row.Phase == natsgrant.OperatorPhaseReleased
		held := row.Phase == natsgrant.OperatorPhaseHeld || (req.Command == CommandSet && row.Phase == natsgrant.OperatorPhasePrepare) || released
		if row.HoldID != holdID || !held {
			return "", wrap(ErrNotHeld, fmt.Sprintf("the row is %s with hold id %s, and %s asks for hold %s", row.Phase, row.HoldID, req.Command, holdID))
		}
		d, gen, err := c.Journal(ctx, holdID)
		if err != nil {
			return "", err
		}
		req.RowReleased = released
		if err := Decide(d, req); err != nil {
			return d.Workflow, fmt.Errorf("holdrecord: %s in %s: %w", req.Command, d.Workflow, err)
		}
		if req.Command == CommandAbort {
			inputs = withStopRecords(inputs, d)
		}
		d.Ops = append(d.Ops, Operation{Command: req.Command, State: StateIntent, Inputs: inputs, Row: rowRef(row)})
		if req.Command != CommandRelease {
			d.Complete = false
		}
		err = c.writeJournal(ctx, holdID, gen, d)
		if err == nil || !moved(err) || attempt == maxJournalWrites {
			return d.Workflow, err
		}
	}
}

// AppendDone writes the command's `done` entry, with outputs, after the command's own checks
// pass, by a PutRecord at the record's `gen`. It returns ErrNoIntent unless the command's latest
// entry is its `intent`. It reads and verifies the row first, which the entry names
// (Operation.Row), so it needs a row: a row lost mid-command is restored first. When Final holds
// for the record's workflow and the command, the same write sets `complete` to true, with
// `hold_gen` that row's `gen`, so a final command's `done` entry and the completion are one write.
// Every other command's `done` entry sets it to false, but `hold release`'s, which keeps it, as
// its intent did: the hold ended as the record stood. `hold release` prints `hold_gen` and does
// not compare it with the row's `gen`.
func (c *Client) AppendDone(ctx context.Context, holdID, command string, outputs map[string]string) error {
	return c.appendEntry(ctx, holdID, command, func(d *Done, row Row) error {
		d.Ops = append(d.Ops, Operation{Command: command, State: StateDone, Outputs: outputs, Row: rowRef(row)})
		if command == CommandRelease {
			return nil
		}
		d.Complete = false
		if Final(d.Workflow, command) {
			d.Complete, d.HoldGen = true, row.Gen
		}
		return nil
	})
}

// AppendStop writes the command's `stop` entry, with its reason, when Stoppable holds and the
// command's own check refuses. It ends the command's pending `intent` with no `done`, so another
// command can be admitted, and it keeps `complete` as it was. It records nothing of what the
// command left, which counts as changed: AppendStopLeft records it. It returns ErrNoIntent unless
// the command's latest entry is its `intent`. Like AppendDone it reads the row the entry names.
func (c *Client) AppendStop(ctx context.Context, holdID, command, reason string) error {
	return c.appendStop(ctx, holdID, command, map[string]string{"reason": reason})
}

// AppendStopLeft is AppendStop that also records what the command left: LeftUnchanged when it
// changed nothing outside the journal, or what it changed (a recreate that deleted the bucket
// before it failed names the bucket). By LeftUnchanged's rule, left covers every run of the
// command in this hold: LeftUnchanged only when this run changed nothing and no earlier run of it
// recorded a change or ended with no entry (a run cut after its intent); otherwise left names what
// those runs left. The caller knows its earlier runs (the journal holds their entries) and folds
// what they left in, and the journal records left as given, but for the one case the journal
// itself shows: LeftUnchanged after a run of the command that ended with no entry (two intents of
// it in a row) is refused, naming the rule. An empty left is refused.
func (c *Client) AppendStopLeft(ctx context.Context, holdID, command, reason, left string) error {
	if left == "" {
		return fmt.Errorf("holdrecord: %s's stop names nothing it left: give LeftUnchanged or what it changed", command)
	}
	return c.appendStop(ctx, holdID, command, map[string]string{"reason": reason, stopLeftKey: left})
}

func (c *Client) appendStop(ctx context.Context, holdID, command string, outputs map[string]string) error {
	return c.appendEntry(ctx, holdID, command, func(d *Done, row Row) error {
		if !Stoppable(d.Workflow, command) {
			return fmt.Errorf("holdrecord: %s is not Stoppable in %s, so its refusal writes no stop entry", command, d.Workflow)
		}
		if outputs[stopLeftKey] == LeftUnchanged && d.CutRun(command) {
			return fmt.Errorf("holdrecord: %s's stop cannot record %q: an earlier run of %s in this hold ended with no entry, so what it left is unknown; the stop names what the runs left (LeftUnchanged's rule)",
				command, LeftUnchanged, command)
		}
		d.Ops = append(d.Ops, Operation{Command: command, State: StateStop, Outputs: outputs, Row: rowRef(row)})
		return nil
	})
}

// withStopRecords is inputs with each stop entry's record of what its command left (Entry.Left),
// keyed by the stop's index in the journal, in the format Entry.Left reads (leftInputPrefix); any
// key of inputs' under that prefix is dropped.
func withStopRecords(inputs map[string]string, d Done) map[string]string {
	out := make(map[string]string, len(inputs))
	for k, v := range inputs {
		if !strings.HasPrefix(k, leftInputPrefix) {
			out[k] = v
		}
	}
	for i, o := range d.Ops {
		if o.State != StateStop {
			continue
		}
		items, _ := Entry{Index: i, Operation: o}.Left()
		for _, item := range items {
			left := item.Left
			if left == "" {
				left = leftNothing
			}
			out[leftInputPrefix+strconv.Itoa(item.Stop)] = item.Command + leftInputSeparator + left
		}
	}
	return out
}

// appendEntry writes one `done` or `stop` entry of command, which change makes given the row it
// reads and verifies (the row the entry names), at the record's gen, when the command's latest
// entry is its intent, and reads and writes again after a write that came between.
func (c *Client) appendEntry(ctx context.Context, holdID, command string, change func(*Done, Row) error) error {
	for attempt := 1; ; attempt++ {
		d, gen, err := c.Journal(ctx, holdID)
		if err != nil {
			return err
		}
		latest := ""
		for _, o := range d.Ops {
			if o.Command == command {
				latest = o.State
			}
		}
		if latest != StateIntent {
			return wrap(ErrNoIntent, fmt.Sprintf("%s's latest entry is %q", command, latest))
		}
		row, err := c.Row(ctx)
		if err != nil {
			return err
		}
		if err := change(&d, row); err != nil {
			return err
		}
		err = c.writeJournal(ctx, holdID, gen, d)
		if err == nil || !moved(err) || attempt == maxJournalWrites {
			return err
		}
	}
}

// rowRef names row, the row a journal entry is written under.
func rowRef(row Row) *RowRef { return &RowRef{Gen: row.Gen, SignedAt: row.SignedAt} }

// writeJournal writes d as the hold's `done` record at expectedGen.
func (c *Client) writeJournal(ctx context.Context, holdID string, expectedGen uint64, d Done) error {
	if d.Ops == nil {
		d.Ops = []Operation{}
	}
	value, err := json.Marshal(d)
	if err != nil {
		return fmt.Errorf("holdrecord: encode hold %s's done record: %s", holdID, jsonFailure(err))
	}
	_, err = c.PutRecord(ctx, holdID, journalKind, journalKey, expectedGen, value)
	return err
}

// moved reports whether err is a record write refused because the record moved since its read.
func moved(err error) bool {
	var refusal *RouteError
	return errors.As(err, &refusal) && refusal.Code == codeGenMismatch
}
