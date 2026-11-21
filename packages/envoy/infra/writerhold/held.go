package writerhold

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/holdrecord"
)

// HeldCommand is a command that changes the hub's protected streams inside a hold: `envoy-infra
// buckets create`, `recreate` and `restore`, and `envoy-infra streams update` and `census`, each
// admitted by the workflow's row of holdrecord's table.
type HeldCommand struct {
	// Workflow is the workflow it runs in, and the one RunHeld prepares when HoldID is empty.
	// Empty with a HoldID, it runs in that hold's own workflow, the table admitting it or not (a
	// census runs in a streams-census, cutover or hub-restore hold).
	Workflow holdrecord.Workflow
	// Command is its name in the workflow's table.
	Command string
	// HoldID is the hold it runs in; empty, RunHeld prepares and sets a new hold of Workflow.
	HoldID string
	// Reason is the hold row's reason when RunHeld prepares the hold, and its intent's.
	Reason string
	// MintersReport is checked (PreflightMinters) when RunHeld prepares the hold.
	MintersReport string
	// Absent are the protected streams the command's targets may be missing: the preflight and
	// the command's own reading record an absent one as expected absence, and stop on any other.
	Absent []string
	// Inputs are its intent's inputs.
	Inputs map[string]string
	// Change makes the change on nc, the operator's connection under the held grant, in the hold
	// holdID, and says what it did to each protected stream it changed (a stream it does not name
	// is kept, unwritten) and its done entry's outputs.
	Change func(ctx context.Context, nc *nats.Conn, holdID string) (map[string]Change, map[string]string, error)
}

// RunHeld runs c inside its hold and returns the hold's id. With no HoldID it checks the minter
// report, prepares a hold of c's workflow and sets it (Set), its preflight recording c's targets as
// expected absence; with one whose `hold set` has no done entry, it runs Set again, `hold set`'s
// rerun. Then, on the operator's connection under the held grant: its admission (holdrecord.Admit:
// the row held under the hold id, c's command admitted in the workflow, its intent written), the
// protected streams read, c's change, the postcheck (Postcheck) against what the change says it
// did, and its done entry. A step after the intent that fails writes a stop entry naming why when
// the command is Stoppable, and else leaves its intent pending; either way the command's rerun is
// admitted. The hold stays set: `envoy-infra hold release` ends it.
func RunHeld(ctx context.Context, records *holdrecord.Client, op *Operator, c HeldCommand, now func() time.Time, stdout io.Writer) (string, error) {
	op.ExpectedAbsent = c.Absent
	id := c.HoldID
	if id == "" && c.Workflow == "" {
		return "", refusef(StepPreflight, "%s names no hold and no workflow to set one of", c.Command)
	}
	if id == "" {
		if err := PreflightMinters(ctx, op.Hub.Client, c.MintersReport, now); err != nil {
			return "", err
		}
		var err error
		if id, err = startHold(ctx, records, op, c.Workflow, c.Reason); err != nil {
			return id, err
		}
		fmt.Fprintf(stdout, "prepared hold %s (%s)\n", id, c.Workflow)
	}
	d, _, err := records.Journal(ctx, id)
	if err != nil {
		return id, err
	}
	if c.Workflow != "" && d.Workflow != c.Workflow {
		return id, refusef(StepPreflight, "hold %s is a %s hold: %s runs only in a %s hold", id, d.Workflow, c.Command, c.Workflow)
	}
	if latestState(d, holdrecord.CommandSet) != "done" {
		reading, err := Set(ctx, records, op, id, c.Reason, nil)
		if err != nil {
			return id, err
		}
		printReport(stdout, id, reading)
	}

	nc, err := op.Connect(ctx)
	if err != nil {
		return id, unknownf(StepPostcheck, err, "connect to the hub as the operator under the held grant")
	}
	defer nc.Close()
	prior := priorLeft(d, c.Command)
	if _, err := records.Admit(ctx, id, holdrecord.Request{Command: c.Command}, c.Inputs); err != nil {
		return id, err
	}
	// A Stoppable command's failure after its intent writes a stop entry recording what it left
	// (holdrecord.AppendStopLeft): LeftUnchanged when neither this run nor an earlier one changed
	// anything outside the journal, else what changed. A command that is not Stoppable leaves its
	// intent pending, and the table admits its rerun (holdrecord.Stoppable).
	stop := func(cause error, left string) error {
		if !holdrecord.Stoppable(d.Workflow, c.Command) {
			return cause
		}
		if prior != "" {
			if left == holdrecord.LeftUnchanged {
				left = prior
			} else {
				left = prior + "; " + left
			}
		}
		if err := records.AppendStopLeft(ctx, id, c.Command, cause.Error(), left); err != nil {
			return errors.Join(cause, err)
		}
		return cause
	}
	before, err := ReadProtectedStreams(ctx, nc, c.Absent)
	if err != nil {
		return id, stop(err, holdrecord.LeftUnchanged)
	}
	changes, outputs, err := c.Change(ctx, nc, id)
	if err != nil {
		left := holdrecord.LeftUnchanged
		var changed *Left
		if errors.As(err, &changed) {
			left = changed.What
		}
		return id, stop(err, left)
	}
	if _, err := Postcheck(ctx, nc, before, changes); err != nil {
		return id, stop(err, describeChanges(changes))
	}
	if err := records.AppendDone(ctx, id, c.Command, outputs); err != nil {
		return id, err
	}
	fmt.Fprintf(stdout, "%s done in hold %s: release it with envoy-infra hold release --hold %s\n", c.Command, id, id)
	return id, nil
}

// Left is a held command's failure that names what the command changed before it failed, outside
// the journal ("deleted KV_envoy_sessions"), which RunHeld's stop entry records. A change's error
// that is no *Left says the change left everything as it was.
type Left struct {
	What string
	Err  error
}

func (l *Left) Error() string { return l.Err.Error() }

func (l *Left) Unwrap() error { return l.Err }

// priorLeft is what earlier runs of command in d left, or "" when every one recorded
// LeftUnchanged: a run whose intent has no stop after it was cut and may have changed anything its
// inputs name, and a stop that recorded a change, or nothing, counts as changed.
func priorLeft(d holdrecord.Done, command string) string {
	var left []string
	pending := false
	for _, op := range d.Ops {
		if op.Command != command {
			continue
		}
		switch op.State {
		case "intent":
			if pending {
				left = append(left, "an earlier run was cut after its intent, and may have changed what its inputs name")
			}
			pending = true
		case "stop":
			pending = false
			switch recorded := op.Outputs["left"]; recorded {
			case holdrecord.LeftUnchanged:
			case "":
				left = append(left, "an earlier run stopped recording nothing, so counted as changed")
			default:
				left = append(left, "an earlier run left: "+recorded)
			}
		case "done":
			pending = false
		}
	}
	if pending {
		left = append(left, "an earlier run was cut after its intent, and may have changed what its inputs name")
	}
	return strings.Join(left, "; ")
}

// describeChanges names each stream a change stated it made, for a stop whose postcheck failed.
func describeChanges(changes map[string]Change) string {
	if len(changes) == 0 {
		return holdrecord.LeftUnchanged
	}
	names := make([]string, 0, len(changes))
	for stream := range changes {
		names = append(names, stream)
	}
	sort.Strings(names)
	parts := make([]string, len(names))
	for i, stream := range names {
		change := changes[stream]
		parts[i] = fmt.Sprintf("%s %s with %d writes", stream, change.Kind, change.Writes)
	}
	return strings.Join(parts, ", ")
}
