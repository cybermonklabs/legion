package writerhold

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/holdrecord"
	"github.com/sjawhar/envoy/natsgrant"
)

// Operator is a hold command's reach of the clustered hub: its Kubernetes objects, the disks its
// claims are bound to, the operator's NATS connection and the probe's.
type Operator struct {
	Hub   KubeHub
	Disks Disks
	// Connect dials the hub as the operator, on a connection made with
	// nats.CustomInboxPrefix(natsgrant.OperatorInbox). The auth service decides it under the hold
	// row's phase at that moment, so a command that moves the row connects again.
	Connect func(ctx context.Context) (*nats.Conn, error)
	// Probe dials url with the hold probe's credential (Restart.Probe's contract): nil when the
	// server admitted it, an error wrapping nats.ErrAuthorization when it refused it.
	Probe func(ctx context.Context, url string) error
	// URLs are the hub's client URLs, which the probe dials as the hub.
	URLs []string
	// Wait waits d, ending early with ctx's error; a sleep when nil.
	Wait func(ctx context.Context, d time.Duration) error
	// RestartLimit is each pod's restart limit, PodRestartLimit when zero.
	RestartLimit time.Duration
	// ExpectedAbsent are the protected streams the hold's workflow expects absent at step 1 (a
	// buckets create's target, the cutover's selection bucket); `hold set` takes those its
	// workflow names (expectedAbsent).
	ExpectedAbsent []string
}

func (op *Operator) wait(ctx context.Context, d time.Duration) error {
	if op.Wait != nil {
		return op.Wait(ctx, d)
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// expectedAbsent are the protected streams a workflow's preflight records as expected absence when
// the server answers `stream not found`: the selection bucket for the cutover's first create-bucket
// and for selection-recovery, and every protected stream for streams-update, which creates each
// one absent. A buckets workflow's target is the buckets command's, which it passes itself.
func expectedAbsent(w holdrecord.Workflow, extra []string) []string {
	out := append([]string{}, extra...)
	switch w {
	case holdrecord.WorkflowCutover, holdrecord.WorkflowSelectionRecovery:
		out = append(out, kvStreamPrefix+bus.SessionConsumersBucket)
	case holdrecord.WorkflowStreamsUpdate:
		out = append(out, ProtectedStreams()...)
	}
	return out
}

// startHold moves a released row to prepare under a new hold id and inserts the hold's journal
// (its done record) for workflow, with the step-1 map: for hub-restore, each claim's disk id and
// volume, read before the row turns held, so every later comparison with a target's step-1 disk
// reads this record and never a fresh read. A claim it cannot read is UNKNOWN, with the row still
// released. A second hold is refused: a prepare is accepted only from released.
func startHold(ctx context.Context, records *holdrecord.Client, op *Operator, w holdrecord.Workflow, reason string) (string, error) {
	row, err := records.Row(ctx)
	if err != nil {
		return "", err
	}
	if row.Phase != natsgrant.OperatorPhaseReleased {
		return "", refusef(StepPreflight, "the hold row is %s under hold %s (gen %d): a second hold is refused, since a hold is prepared only from released", row.Phase, row.HoldID, row.Gen)
	}
	var step1 map[string]string
	if w == holdrecord.WorkflowHubRestore {
		if step1, err = readStep1(ctx, op); err != nil {
			return "", err
		}
	}
	id := newHoldID()
	if _, err := records.MoveRow(ctx, row.Gen, natsgrant.OperatorPhasePrepare, id, reason); err != nil {
		return "", err
	}
	if err := records.StartJournal(ctx, id, w, step1); err != nil {
		return id, fmt.Errorf("hold %s is prepared, and its journal was not written (hold abort --hold %s releases it): %w", id, id, err)
	}
	return id, nil
}

// readStep1 reads every claim's disk for the step-1 map: step1_disk_id.<claim> and
// step1_volume.<claim>.
func readStep1(ctx context.Context, op *Operator) (map[string]string, error) {
	r, err := Precondition(ctx, op.Hub)
	if err != nil {
		return nil, err
	}
	claims, err := ReadClaims(ctx, op.Hub, r, op.Disks)
	if err != nil {
		return nil, err
	}
	step1 := map[string]string{}
	for _, b := range claims {
		step1["step1_disk_id."+b.Claim] = b.DiskID
		step1["step1_volume."+b.Claim] = b.Volume
	}
	return step1, nil
}

// prepare is `hold prepare`: the minter report checked, then the row moved from released to
// prepare with a new hold id and the hold's journal inserted for its workflow. Clients are still
// admitted. It prints the hold id.
func prepare(ctx context.Context, records *holdrecord.Client, op *Operator, o options, now func() time.Time, stdout io.Writer) error {
	if err := PreflightMinters(ctx, op.Hub.Client, o.mintersReport, now); err != nil {
		return err
	}
	id, err := startHold(ctx, records, op, holdrecord.Workflow(o.workflow), o.reason)
	if err != nil {
		return err
	}
	fmt.Fprintln(stdout, id)
	return nil
}

// abort is `hold abort`. From prepare it deletes the hold's cohort record, then moves the row to
// released under its hold id; the thresholds a prepare raised stay. A held row is refused in every
// workflow but hub-restore, where abort is admitted while no `hold prove` is done and every claim
// is still on its step-1 disk: its intent, the disks read again, the row moved to released, the
// disks read once more (a restore that lands between the last check and the move is a restore
// outside any hold, which it names), then its done entry. A row already released under this hold
// id is the abort's whole effect: a rerun after the move changes nothing.
func abort(ctx context.Context, records *holdrecord.Client, op *Operator, o options, stdout io.Writer) error {
	row, err := records.Row(ctx)
	if err != nil {
		return err
	}
	if row.HoldID != o.holdID {
		return refusef(StepPreflight, "the hold row is %s under hold %s, not %s", row.Phase, row.HoldID, o.holdID)
	}
	switch row.Phase {
	case natsgrant.OperatorPhaseReleased:
		fmt.Fprintf(stdout, "hold %s is already released (row gen %d)\n", o.holdID, row.Gen)
		return nil
	case natsgrant.OperatorPhasePrepare:
		if err := records.DeleteCohort(ctx, o.holdID); err != nil {
			return err
		}
		moved, err := records.MoveRow(ctx, row.Gen, natsgrant.OperatorPhaseReleased, o.holdID, o.reason)
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "aborted hold %s from prepare: row gen %d, released\n", o.holdID, moved.Gen)
		return nil
	}
	d, _, err := records.Journal(ctx, o.holdID)
	if err != nil {
		return err
	}
	if d.Workflow != holdrecord.WorkflowHubRestore {
		return refusef(StepPreflight, "hold %s is held in workflow %s, where abort is refused: release it with nothing changed, or finish the workflow", o.holdID, d.Workflow)
	}
	if _, err := records.Admit(ctx, o.holdID, holdrecord.Request{Command: holdrecord.CommandAbort}, nil); err != nil {
		return err
	}
	stop := func(cause error) error {
		if err := records.AppendStop(ctx, o.holdID, holdrecord.CommandAbort, cause.Error()); err != nil {
			return errors.Join(cause, err)
		}
		return cause
	}
	if err := onStep1Disks(ctx, op, d); err != nil {
		return stop(err)
	}
	moved, err := records.MoveRow(ctx, row.Gen, natsgrant.OperatorPhaseReleased, o.holdID, o.reason)
	if err != nil {
		return err
	}
	after := onStep1Disks(ctx, op, d)
	if err := records.AppendDone(ctx, o.holdID, holdrecord.CommandAbort, nil); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "aborted hold %s in hub-restore: row gen %d, released\n", o.holdID, moved.Gen)
	if after != nil {
		return fmt.Errorf("the hold is released, and a disk changed after abort's last check, outside any hold: %w", after)
	}
	return nil
}

// readDisks is every claim's disk id now, by target (claim name), with the claims' bindings.
func readDisks(ctx context.Context, op *Operator, step string) (map[string]string, map[string]ClaimBinding, error) {
	r, err := Precondition(ctx, op.Hub)
	if err != nil {
		return nil, nil, err
	}
	claims, err := ReadClaims(ctx, op.Hub, r, op.Disks)
	if err != nil {
		return nil, nil, err
	}
	read := map[string]string{}
	byClaim := map[string]ClaimBinding{}
	for _, b := range claims {
		read[b.Claim] = b.DiskID
		byClaim[b.Claim] = b
	}
	return read, byClaim, nil
}

// onStep1Disks refuses unless every target is on the disk id the journal's step-1 map records
// for it. A target the map lacks, or one the map names that is not read, is UNKNOWN (R28-H).
func onStep1Disks(ctx context.Context, op *Operator, d holdrecord.Done) error {
	read, _, err := readDisks(ctx, op, StepPreflight)
	if err != nil {
		return err
	}
	want := map[string]string{}
	step1, ok := holdrecord.Step1(d)
	if !ok {
		return unknownf(StepPreflight, nil, "the hold's journal holds no step-1 map")
	}
	for k, v := range step1 {
		if t, found := strings.CutPrefix(k, "step1_disk_id."); found {
			want[t] = v
		}
	}
	return sameDisks(StepPreflight, "its step-1 disk", want, read)
}

// sameDisks refuses, in step, a target whose disk id read differs from want, and is UNKNOWN when
// the two do not name the same targets.
func sameDisks(step, what string, want, read map[string]string) error {
	if len(want) != len(read) {
		return unknownf(step, nil, "the targets read (%s) are not the journal's (%s)", strings.Join(sortedKeys(read), ", "), strings.Join(sortedKeys(want), ", "))
	}
	var moved []string
	for _, t := range sortedKeys(want) {
		got, found := read[t]
		if !found {
			return unknownf(step, nil, "target %s is in the journal and was not read", t)
		}
		if got != want[t] {
			moved = append(moved, fmt.Sprintf("%s is on disk %s, not %s (%s)", t, got, want[t], what))
		}
	}
	if len(moved) > 0 {
		return refusef(step, "%s", strings.Join(moved, "; "))
	}
	return nil
}

// release is `hold release`. Its admission (holdrecord.Admit) is its decision: the record holds no
// entry but hold set's and its own, or it is complete for its workflow, else ErrOutOfOrder. In
// hub-restore, before its PUT, every claim must be on the disk the latest done prove records for
// it, and every other target on its step-1 disk (holdrecord.Expected); a refusal of that check
// writes a stop entry, which keeps the completion. Its PUT is its last step. Run again on a row
// already released under its hold id, it sends no PUT.
func release(ctx context.Context, records *holdrecord.Client, op *Operator, o options, stdout io.Writer) error {
	row, err := records.Row(ctx)
	if err != nil {
		return err
	}
	if row.HoldID != o.holdID {
		return refusef(StepPreflight, "the hold row is %s under hold %s, not %s", row.Phase, row.HoldID, o.holdID)
	}
	if row.Phase == natsgrant.OperatorPhaseReleased {
		d, _, err := records.Journal(ctx, o.holdID)
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "hold %s is already released (row gen %d); its record's hold_gen is %d\n", o.holdID, row.Gen, d.HoldGen)
		return nil
	}
	if row.Phase != natsgrant.OperatorPhaseHeld {
		return refusef(StepPreflight, "hold %s is %s, not held: a prepared hold is ended by hold abort", o.holdID, row.Phase)
	}
	w, err := records.Admit(ctx, o.holdID, holdrecord.Request{Command: holdrecord.CommandRelease}, nil)
	if err != nil {
		return err
	}
	d, _, err := records.Journal(ctx, o.holdID)
	if err != nil {
		return err
	}
	if w == holdrecord.WorkflowHubRestore {
		if err := onExpectedDisks(ctx, op, d); err != nil {
			if stopErr := records.AppendStop(ctx, o.holdID, holdrecord.CommandRelease, err.Error()); stopErr != nil {
				return errors.Join(err, stopErr)
			}
			return err
		}
	}
	moved, err := records.MoveRow(ctx, row.Gen, natsgrant.OperatorPhaseReleased, o.holdID, o.reason)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "released hold %s: row gen %d; the record's hold_gen is %d\n", o.holdID, moved.Gen, d.HoldGen)
	return nil
}

// onExpectedDisks refuses unless every target is on its expected disk (holdrecord.Expected).
func onExpectedDisks(ctx context.Context, op *Operator, d holdrecord.Done) error {
	want, ok := holdrecord.Expected(d)
	if !ok {
		return unknownf(StepPreflight, nil, "the hold's journal holds no step-1 map")
	}
	read, _, err := readDisks(ctx, op, StepPreflight)
	if err != nil {
		return err
	}
	return sameDisks(StepPreflight, "the disk the latest prove, or step 1, records", want, read)
}

// set is `hold set`: with --workflow, a hold with no prepare step, it first prepares one
// (startHold) and runs the preflight at once; with --hold, it sets the prepared hold. Its steps,
// each a reading of the hub itself (§3a): the preflight under the prepare grant, its intent with
// the row still prepare, the row moved to held, then, under the held grant, the probe's refusal,
// the writer census, the restart of every declared server with its proof, the meta barrier and
// the census inside it, and the report; then its done entry. A refusal after its intent writes a
// stop entry, so `hold set`'s rerun, `hold abort` in hub-restore, or `hold release` with nothing
// changed is admitted. A rerun that finds the row held under its hold id runs the held steps again.
func set(ctx context.Context, records *holdrecord.Client, op *Operator, o options, now func() time.Time, stdout io.Writer) error {
	if err := PreflightMinters(ctx, op.Hub.Client, o.mintersReport, now); err != nil {
		return err
	}
	id := o.holdID
	if id == "" {
		var err error
		if id, err = startHold(ctx, records, op, holdrecord.Workflow(o.workflow), o.reason); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "prepared hold %s\n", id)
	}
	reading, err := Set(ctx, records, op, id, o.reason, restoreInputs(o))
	if err != nil {
		return err
	}
	printReport(stdout, id, reading)
	return nil
}

// restoreInputs are hub-restore's inputs, which hold set's intent carries: disk.<target> and
// snapshot.<target> for each restored target.
func restoreInputs(o options) map[string]string {
	if len(o.restored) == 0 && len(o.snapshots) == 0 {
		return nil
	}
	in := map[string]string{}
	for _, pair := range o.restored {
		t, v, _ := strings.Cut(pair, "=")
		in["disk."+t] = v
	}
	for _, pair := range o.snapshots {
		t, v, _ := strings.Cut(pair, "=")
		in["snapshot."+t] = v
	}
	return in
}

// Set runs hold set's steps on the hold holdID, prepared already, and returns the report's clean
// reading. inputs are its intent's (hub-restore's restore inputs). `envoy-infra buckets` runs it
// for the hold of its one command.
func Set(ctx context.Context, records *holdrecord.Client, op *Operator, holdID, reason string, inputs map[string]string) (*ReportReading, error) {
	row, err := records.Row(ctx)
	if err != nil {
		return nil, err
	}
	if row.HoldID != holdID || (row.Phase != natsgrant.OperatorPhasePrepare && row.Phase != natsgrant.OperatorPhaseHeld) {
		return nil, refusef(StepPreflight, "the hold row is %s under hold %s, not prepared or held under %s", row.Phase, row.HoldID, holdID)
	}
	d, _, err := records.Journal(ctx, holdID)
	if err != nil {
		return nil, err
	}
	pending := latestState(d, holdrecord.CommandSet) == "intent"
	stop := func(cause error) error {
		if !pending {
			return cause
		}
		if err := records.AppendStop(ctx, holdID, holdrecord.CommandSet, cause.Error()); err != nil {
			return errors.Join(cause, err)
		}
		return cause
	}

	// Step 1, under the prepare grant (or the held one on a rerun), mutating nothing.
	nc, err := op.Connect(ctx)
	if err != nil {
		return nil, stop(unknownf(StepPreflight, err, "connect to the hub as the operator"))
	}
	defer func() { nc.Close() }()
	render, err := Precondition(ctx, op.Hub)
	if err != nil {
		return nil, stop(err)
	}
	declared, err := Discover(ctx, op.Hub)
	if err != nil {
		return nil, stop(err)
	}
	discovered := time.Now()
	claims, err := ReadClaims(ctx, op.Hub, render, op.Disks)
	if err != nil {
		return nil, stop(err)
	}
	streams, err := ReadProtectedStreams(ctx, nc, expectedAbsent(d.Workflow, op.ExpectedAbsent))
	if err != nil {
		return nil, stop(err)
	}
	present := presentStreams(streams)

	if row.Phase == natsgrant.OperatorPhasePrepare {
		if !pending {
			if _, err := records.Admit(ctx, holdID, holdrecord.Request{Command: holdrecord.CommandSet}, inputs); err != nil {
				return nil, err
			}
			pending = true
		}
		if _, err := records.MoveRow(ctx, row.Gen, natsgrant.OperatorPhaseHeld, holdID, reason); err != nil {
			return nil, stop(err)
		}
	}

	// Step 2: under the held grant, after one authorization timeout, so a decision that started
	// before the row was written has ended.
	nc.Close()
	if nc, err = op.Connect(ctx); err != nil {
		return nil, stop(unknownf(StepProbe, err, "connect to the hub as the operator under the held grant"))
	}
	if left := declared.AuthTimeout() - time.Since(discovered); left > 0 {
		if err := op.wait(ctx, left); err != nil {
			return nil, stop(err)
		}
	}
	if err := probeRefused(ctx, op, strings.Join(op.URLs, ","), "the hub"); err != nil {
		return nil, stop(err)
	}
	if _, err := TakeCensus(ctx, nc, present); err != nil {
		return nil, stop(err)
	}

	// Steps 3 and 4: every declared server restarted and proved, one at a time.
	restart := Restart{Hub: op.Hub, Declared: declared, Limit: op.RestartLimit,
		Probe: op.Probe,
		Bind: func(ctx context.Context, pod string, step1, now Varz) error {
			if err := CheckReload(StepProof, pod, step1, now); err != nil {
				return err
			}
			return render.ProveBinding(ctx, op.Hub, op.Disks, pod, now, claims[render.Claim(pod)])
		}}
	restarted, err := restart.Run(ctx, nc)
	if err != nil {
		return nil, stop(err)
	}
	after := make(Declared, len(restarted))
	for i, r := range restarted {
		after[i] = DeclaredServer{Pod: r.Pod, Varz: r.Varz}
	}

	// Step 5: the meta barrier, with the writer census again inside it, since the restart.
	barrier := MetaBarrier{Hub: op.Hub, Declared: after, HoldID: holdID}
	if err := barrier.Run(ctx, nc, func(ctx context.Context) error {
		_, err := TakeCensus(ctx, nc, present)
		return err
	}); err != nil {
		return nil, stop(err)
	}

	// Step 6: the report.
	report := Report{Hub: op.Hub, Declared: after, AuthUser: render.AuthUser, Wait: op.Wait}
	reading, err := report.Run(ctx, nc)
	if err != nil {
		return nil, stop(err)
	}
	for _, server := range reading.Servers {
		for _, r := range restarted {
			if r.Pod == server.Pod {
				if err := CheckReload(StepReport, r.Pod, r.Varz, server.Varz); err != nil {
					return nil, stop(err)
				}
			}
		}
	}
	outputs := map[string]string{}
	for _, r := range restarted {
		outputs["server_id."+r.Pod] = r.Varz.ServerID
		outputs["exit."+r.Pod] = r.Exit.String()
	}
	if err := records.AppendDone(ctx, holdID, holdrecord.CommandSet, outputs); err != nil {
		return nil, err
	}
	return reading, nil
}

// probeRefused connects the probe to url and requires the refusal: an error wrapping
// nats.ErrAuthorization. An admitted probe stops the hold; any other error is UNKNOWN.
func probeRefused(ctx context.Context, op *Operator, url, what string) error {
	err := op.Probe(ctx, url)
	switch {
	case err == nil:
		return refusef(StepProbe, "%s admitted the hold's probe under a held row: the auth service does not refuse clients during the hold", what)
	case errors.Is(err, nats.ErrAuthorization):
		return nil
	}
	return unknownf(StepProbe, err, "the probe's connect to %s", what)
}

// latestState is the state of command's latest entry in the journal, or "".
func latestState(d holdrecord.Done, command string) string {
	state := ""
	for _, op := range d.Ops {
		if op.Command == command {
			state = op.State
		}
	}
	return state
}

// presentStreams are the names of the step-1 readings of streams that are present.
func presentStreams(readings []StreamReading) []string {
	var out []string
	for _, r := range readings {
		if !r.Absent {
			out = append(out, r.Name)
		}
	}
	return out
}

// printReport prints the report's clean reading: the hold's record of who remained.
func printReport(stdout io.Writer, holdID string, reading *ReportReading) {
	fmt.Fprintf(stdout, "hold %s: only the operator (server %s, cid %d) and the auth service remain, read %d times\n",
		holdID, reading.Caller.ServerID, reading.Caller.CID, reading.Readings)
	for _, s := range reading.Servers {
		fmt.Fprintf(stdout, "  %s %s (%s): %d connections\n", s.Pod, s.Varz.ServerName, s.Varz.ServerID, len(s.Connections))
		for _, c := range s.Connections {
			fmt.Fprintf(stdout, "    cid %d name %q user %q\n", c.CID, c.Name, c.AuthorizedUser)
		}
	}
}

// prove is `hold prove` in hub-restore: each target its --restored names is the restored one, on
// the disk named, and every other target must be on its step-1 disk. It is admitted with Moved's
// answer for its own reading of every target, writes its intent before its first restart, runs
// steps 3 to 6 again on the restored servers, and writes its done entry with disk.<target> and
// disk_id.<target> for each restored target. A restored target still on its step-1 disk is refused
// (a restore that did not happen ends with hold abort), and a refusal after the intent writes a
// stop entry, so a corrected prove is admitted.
func prove(ctx context.Context, records *holdrecord.Client, op *Operator, o options, stdout io.Writer) error {
	d, _, err := records.Journal(ctx, o.holdID)
	if err != nil {
		return err
	}
	restored := map[string]string{}
	for _, pair := range o.restored {
		t, v, _ := strings.Cut(pair, "=")
		restored[t] = v
	}
	read, bindings, err := readDisks(ctx, op, StepProof)
	if err != nil {
		return err
	}
	moved, err := holdrecord.Moved(d, read)
	if err != nil {
		return err
	}
	if _, err := records.Admit(ctx, o.holdID, holdrecord.Request{Command: holdrecord.CommandProve, DisksMoved: moved}, restoreInputs(o)); err != nil {
		return err
	}
	stop := func(cause error) error {
		if err := records.AppendStop(ctx, o.holdID, holdrecord.CommandProve, cause.Error()); err != nil {
			return errors.Join(cause, err)
		}
		return cause
	}
	step1, _ := holdrecord.Step1(d)
	want := map[string]string{}
	var findings []string
	for _, t := range sortedKeys(read) {
		s1 := step1["step1_disk_id."+t]
		disk, isRestored := restored[t]
		switch {
		case !isRestored:
			want[t] = s1
		case read[t] == s1:
			findings = append(findings, fmt.Sprintf("%s is still on its step-1 disk %s: a restore that did not happen ends with hold abort", t, s1))
		case bindings[t].Disk.Name != disk && bindings[t].Volume != disk:
			findings = append(findings, fmt.Sprintf("%s is bound to disk %s (volume %s), not %s", t, bindings[t].Disk.Name, bindings[t].Volume, disk))
		default:
			want[t] = read[t]
		}
	}
	for t := range restored {
		if _, found := read[t]; !found {
			findings = append(findings, fmt.Sprintf("--restored names %s, which is no claim of the hub", t))
		}
	}
	if len(findings) > 0 {
		return stop(refusef(StepProof, "%s", strings.Join(findings, "; ")))
	}
	if err := sameDisks(StepProof, "the disk this prove expects", want, read); err != nil {
		return stop(err)
	}
	reading, err := proveServers(ctx, op, o.holdID, bindings)
	if err != nil {
		return stop(err)
	}
	outputs := map[string]string{}
	for t := range restored {
		outputs["disk."+t] = bindings[t].Disk.Name
		outputs["disk_id."+t] = read[t]
	}
	if err := records.AppendDone(ctx, o.holdID, holdrecord.CommandProve, outputs); err != nil {
		return err
	}
	printReport(stdout, o.holdID, reading)
	return nil
}

// proveServers runs steps 3 to 6 on the hub's servers, each claim held to bindings, for a prove.
func proveServers(ctx context.Context, op *Operator, holdID string, bindings map[string]ClaimBinding) (*ReportReading, error) {
	nc, err := op.Connect(ctx)
	if err != nil {
		return nil, unknownf(StepRestart, err, "connect to the hub as the operator")
	}
	defer func() { nc.Close() }()
	render, err := Precondition(ctx, op.Hub)
	if err != nil {
		return nil, err
	}
	declared, err := Discover(ctx, op.Hub)
	if err != nil {
		return nil, err
	}
	restart := Restart{Hub: op.Hub, Declared: declared, Limit: op.RestartLimit, Probe: op.Probe,
		Bind: func(ctx context.Context, pod string, step1, now Varz) error {
			if err := CheckReload(StepProof, pod, step1, now); err != nil {
				return err
			}
			return render.ProveBinding(ctx, op.Hub, op.Disks, pod, now, bindings[render.Claim(pod)])
		}}
	restarted, err := restart.Run(ctx, nc)
	if err != nil {
		return nil, err
	}
	after := make(Declared, len(restarted))
	for i, r := range restarted {
		after[i] = DeclaredServer{Pod: r.Pod, Varz: r.Varz}
	}
	if err := (MetaBarrier{Hub: op.Hub, Declared: after, HoldID: holdID}).Run(ctx, nc, nil); err != nil {
		return nil, err
	}
	return Report{Hub: op.Hub, Declared: after, AuthUser: render.AuthUser, Wait: op.Wait}.Run(ctx, nc)
}
