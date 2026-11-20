package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/spf13/cobra"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/intake"
)

// newIntakeCommand is `legion intake`: the operator's replay and discard of the messages the
// daemon's intake gave up on, which `legion state` lists under its give-up lines.
func newIntakeCommand(stdout, stderr io.Writer) *cobra.Command {
	return group(&cobra.Command{Use: "intake", Short: "replay or discard the messages the daemon's intake gave up on"},
		newIntakeReplayCommand(stdout, stderr),
		newIntakeDiscardCommand(stdout, stderr),
	)
}

// intakeTokenUsage is how --operator-token-file reads in the help of both intake commands.
const intakeTokenUsage = "file holding the operator bearer the daemon's operator_token_file names (required)"

// intakeCall is one `legion intake` command: an operator command whose bearer is required, and the
// selection of the give-ups it takes, one record by --give-up or a consumer's records at or below
// --through-seq.
type intakeCall struct {
	*operatorCall
	giveUp     *int64
	consumer   *string
	throughSeq *uint64
}

func newIntakeCall(cmd *cobra.Command, stdout, stderr io.Writer) *intakeCall {
	c := &intakeCall{operatorCall: newOperatorCall(cmd, intakeTokenUsage, stdout, stderr)}
	c.giveUp = c.flags.Int64("give-up", 0, "the id of one give-up, as legion state lists it")
	c.consumer = c.flags.String("consumer", "", "the durable consumer whose open give-ups to take, with --through-seq")
	c.throughSeq = c.flags.Uint64("through-seq", 0, "the highest stream sequence of --consumer's open give-ups to take; legion state gives each consumer's newest")
	return c
}

// selection is the give-ups the command names, after its required flags: --give-up alone, or
// --consumer with --through-seq. Naming neither form, both, or half of the second is a usage error,
// exit 2, before anything is read or sent; the daemon judges the values themselves.
func (c *intakeCall) selection(required ...string) (api.IntakeSelection, int, bool) {
	if code, ok := c.check(required...); !ok {
		return api.IntakeSelection{}, code, false
	}
	one := c.flags.Changed("give-up")
	bulk := c.flags.Changed("consumer") || c.flags.Changed("through-seq")
	switch {
	case one && bulk:
		return api.IntakeSelection{}, refuse(c.cmd, "name --give-up, or --consumer with --through-seq, not both"), false
	case !one && !bulk:
		return api.IntakeSelection{}, refuse(c.cmd, "name --give-up <id>, or --consumer <name> with --through-seq <seq>"), false
	case bulk && !(c.flags.Changed("consumer") && c.flags.Changed("through-seq")):
		return api.IntakeSelection{}, refuse(c.cmd, "--consumer and --through-seq are given together"), false
	}
	return api.IntakeSelection{GiveUp: *c.giveUp, Consumer: *c.consumer, ThroughSeq: *c.throughSeq}, 0, true
}

// newIntakeReplayCommand is `legion intake replay`: the daemon applies each give-up selected again
// through the handling a live delivery takes, and the command prints how each ended and how many
// open give-ups of the selection are left.
func newIntakeReplayCommand(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "replay --operator-token-file <file> (--give-up <id> | --consumer <name> --through-seq <seq>)",
		Short: "apply again, through the live handling, one give-up or a consumer's open give-ups at or below a stream sequence",
		Args:  noArguments,
	}
	c := newIntakeCall(cmd, stdout, stderr)
	cmd.RunE = runs(func(ctx context.Context, _ []string) int { return runIntakeReplay(ctx, c) })
	return cmd
}

func runIntakeReplay(ctx context.Context, c *intakeCall) int {
	selection, code, ok := c.selection("operator-token-file")
	if !ok {
		return code
	}
	op, ok := c.connect()
	if !ok {
		return 1
	}
	failed := false
	if code := c.send(ctx, op, http.MethodPost, "/legion/v1/operator/intake/replay", api.OperatorIntakeReplayRequest{IntakeSelection: selection},
		func(w io.Writer, answer []byte) error {
			var replay api.IntakeReplayResponse
			if err := json.Unmarshal(answer, &replay); err != nil {
				return err
			}
			for _, line := range replayLines(replay) {
				if _, err := fmt.Fprintln(w, line); err != nil {
					return err
				}
			}
			failed = replayFailed(replay)
			return nil
		}); code != 0 {
		return code
	}
	if failed {
		return 1
	}
	return 0
}

// replayLines are the lines an operator reads for a replay: one per record it took, `<id>
// <consumer> seq <n>: <outcome>`, with the error after a failed one, then how many open give-ups
// of the selection are left: a record that failed, and the records past what one call takes, which
// the same command run again reaches.
func replayLines(replay api.IntakeReplayResponse) []string {
	lines := make([]string, 0, len(replay.Replayed)+1)
	for _, record := range replay.Replayed {
		line := fmt.Sprintf("%d %s seq %d: %s", record.ID, record.Consumer, record.StreamSeq, record.Outcome)
		if record.Error != "" {
			line += ": " + record.Error
		}
		lines = append(lines, line)
	}
	return append(lines, fmt.Sprintf("remaining %d", replay.Remaining))
}

// replayFailed is whether a record of replay failed, which fails the command: its give-up is still
// open, or still discarded, with the new error.
func replayFailed(replay api.IntakeReplayResponse) bool {
	for _, record := range replay.Replayed {
		if record.Outcome == string(intake.ReplayFailed) {
			return true
		}
	}
	return false
}

// newIntakeDiscardCommand is `legion intake discard`: the daemon closes each open give-up selected
// as discarded for --reason, applying nothing and keeping the record, and the command prints the
// id of each it closed.
func newIntakeDiscardCommand(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "discard --operator-token-file <file> (--give-up <id> | --consumer <name> --through-seq <seq>) --reason <text>",
		Short: "close one give-up, or a consumer's open give-ups at or below a stream sequence, as discarded for a reason, applying nothing",
		Args:  noArguments,
	}
	c := newIntakeCall(cmd, stdout, stderr)
	reason := c.flags.String("reason", "", "why the give-ups are let go, which each record keeps (required)")
	cmd.RunE = runs(func(ctx context.Context, _ []string) int { return runIntakeDiscard(ctx, c, *reason) })
	return cmd
}

func runIntakeDiscard(ctx context.Context, c *intakeCall, reason string) int {
	selection, code, ok := c.selection("operator-token-file", "reason")
	if !ok {
		return code
	}
	op, ok := c.connect()
	if !ok {
		return 1
	}
	return c.send(ctx, op, http.MethodPost, "/legion/v1/operator/intake/discard",
		api.OperatorIntakeDiscardRequest{IntakeSelection: selection, Reason: reason},
		func(w io.Writer, answer []byte) error {
			var discard api.IntakeDiscardResponse
			if err := json.Unmarshal(answer, &discard); err != nil {
				return err
			}
			if len(discard.Discarded) == 0 {
				_, err := fmt.Fprintln(w, "discarded none")
				return err
			}
			for _, id := range discard.Discarded {
				if _, err := fmt.Fprintf(w, "discarded %d\n", id); err != nil {
					return err
				}
			}
			return nil
		})
}
