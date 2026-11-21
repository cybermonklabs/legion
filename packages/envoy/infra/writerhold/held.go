package writerhold

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/holdrecord"
)

// HeldCommand is a command that changes the hub's protected streams inside a hold of its own
// workflow: `envoy-infra buckets create`, `recreate` and `restore`, each its workflow's one
// command (holdrecord's table, "buckets create" final in buckets-create).
type HeldCommand struct {
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
	// Change makes the change on nc, the operator's connection under the held grant, and says what
	// it did to each protected stream it changed (a stream it does not name is kept, unwritten) and
	// its done entry's outputs.
	Change func(ctx context.Context, nc *nats.Conn) (map[string]Change, map[string]string, error)
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
	if d.Workflow != c.Workflow {
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
	if _, err := records.Admit(ctx, id, holdrecord.Request{Command: c.Command}, c.Inputs); err != nil {
		return id, err
	}
	// A Stoppable command's failure after its intent writes a stop entry; any other leaves its
	// intent pending, and the table admits its rerun (holdrecord.Stoppable).
	stop := func(cause error) error {
		if !holdrecord.Stoppable(c.Workflow, c.Command) {
			return cause
		}
		if err := records.AppendStop(ctx, id, c.Command, cause.Error()); err != nil {
			return errors.Join(cause, err)
		}
		return cause
	}
	before, err := ReadProtectedStreams(ctx, nc, c.Absent)
	if err != nil {
		return id, stop(err)
	}
	changes, outputs, err := c.Change(ctx, nc)
	if err != nil {
		return id, stop(err)
	}
	if _, err := Postcheck(ctx, nc, before, changes); err != nil {
		return id, stop(err)
	}
	if err := records.AppendDone(ctx, id, c.Command, outputs); err != nil {
		return id, err
	}
	fmt.Fprintf(stdout, "%s done in hold %s: release it with envoy-infra hold release --hold %s\n", c.Command, id, id)
	return id, nil
}
