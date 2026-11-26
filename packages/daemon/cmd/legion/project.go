package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/sjawhar/legion/daemon/internal/api"
)

// newProjectCommand is the operator's hand on the daemon's project as a whole, one command per
// operator project route (internal/api/pause.go, internal/api/admission_signals.go). pause stops
// every role of the project but the controller and holds every launch, task and admission until
// resume lets the held work out through the wake pacing (dispatch://INFRA-3759); signals turns
// admission by signals on or off and sets or clears each signal's limit (S3 of
// dispatch://INFRA-5899). The daemon acts on its own project, so none names one.
func newProjectCommand(stdout, stderr io.Writer) *cobra.Command {
	return group(&cobra.Command{Use: "project", Short: "pause the daemon's project, holding every role's work but the controller's, or resume it; turn admission by signals on or off and set its limits"},
		projectCommand("pause", "pause the project: stop every role but the controller and hold their work", projectPause, stdout, stderr),
		projectCommand("resume", "end the project's pause and let the held work out through the wake pacing", projectResume, stdout, stderr),
		group(&cobra.Command{Use: "signals", Short: "turn admission by signals on or off, or set or clear a signal's limit, on a person's cited word"},
			projectCommand("on", "turn admission by signals on: an issue below P0 starts only while each signal with a limit reads under it", projectSignals(api.SignalsOn), stdout, stderr),
			projectCommand("off", "turn admission by signals off: the cap alone admits, as before", projectSignals(api.SignalsOff), stdout, stderr),
			projectCommand("set-limit", "set one signal's limit, which it holds admission at or over", projectSignals(api.SignalsSetLimit), stdout, stderr),
			projectCommand("clear-limit", "clear one signal's limit, so it holds nothing", projectSignals(api.SignalsClearLimit), stdout, stderr),
		),
	)
}

// projectCommand is `legion project <sub>`: declare gives it the flags of its own beside the ones
// every project command takes (newProjectCall), and answers its body.
func projectCommand(sub, short string, declare func(c *projectCall) action, stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{Use: sub, Short: short, Args: noArguments}
	cmd.RunE = runs(declare(newProjectCall(cmd, stdout, stderr)))
	return cmd
}

// projectRoute is the root of the daemon's operator project routes.
const projectRoute = "/legion/v1/operator/project"

// projectCall is one `legion project` subcommand: an operator command whose bearer is required,
// and which can print the daemon's answer as it came.
type projectCall struct {
	*operatorCall
	asJSON *bool
}

// newProjectCall gives cmd, one `legion project` subcommand, its flags: the operator's three and
// --json.
func newProjectCall(cmd *cobra.Command, stdout, stderr io.Writer) *projectCall {
	c := newOperatorCall(cmd, "file holding the operator bearer the daemon's operator_token_file names (required)", stdout, stderr)
	return &projectCall{operatorCall: c, asJSON: c.flags.Bool("json", false, "print the daemon's answer as it served it")}
}

// send is operatorCall.send on an operator project route, printing through print, or under --json
// the answer as the daemon served it.
func (c *projectCall) send(ctx context.Context, path string, body any, print func(io.Writer, []byte) error) int {
	op, ok := c.connect()
	if !ok {
		return 1
	}
	if *c.asJSON {
		print = writeAnswer
	}
	return c.operatorCall.send(ctx, op, http.MethodPost, projectRoute+path, body, print)
}

// projectPause is `legion project pause --reason <text>`: the daemon stops every role of its
// project but the controller and holds their work, records the operator as who paused and why,
// and tells the controller. The daemon refuses a second pause, naming the one that stands, and a
// configuration that cannot hold a pause, naming the setting (`legion config check` names it
// too).
func projectPause(c *projectCall) action {
	reason := c.flags.String("reason", "", "why the project is paused: the pause records it and the controller is told it (required)")
	return func(ctx context.Context, _ []string) int {
		if code, ok := c.check("operator-token-file", "reason"); !ok {
			return code
		}
		return c.send(ctx, "/pause", api.OperatorPauseRequest{Reason: *reason}, printPauseAnswer)
	}
}

// projectResume is `legion project resume`: the daemon ends its project's pause, whoever made it,
// records the operator as who ended it, tells the controller, and lets the held work out through
// the wake pacing.
func projectResume(c *projectCall) action {
	return func(ctx context.Context, _ []string) int {
		if code, ok := c.check("operator-token-file"); !ok {
			return code
		}
		return c.send(ctx, "/resume", nil, printResumeAnswer)
	}
}

// printPauseAnswer is the pause the daemon began, as one line.
func printPauseAnswer(w io.Writer, answer []byte) error {
	var paused api.ProjectPauseResponse
	if err := decodeAnswer(answer, &paused); err != nil {
		return err
	}
	_, err := fmt.Fprintln(w, "project paused: "+pauseWords(paused.Pause))
	return err
}

// printResumeAnswer is the pause the daemon ended and how many held rows it let out, as one line.
func printResumeAnswer(w io.Writer, answer []byte) error {
	var resumed api.ProjectResumeResponse
	if err := decodeAnswer(answer, &resumed); err != nil {
		return err
	}
	_, err := fmt.Fprintf(w, "project resumed: %s; %d held rows released\n", endedPauseWords(resumed.Pause), resumed.Released)
	return err
}

// projectSignals is `legion project signals <action>`: on and off turn admission by signals on or
// off, set-limit sets one signal's limit (--signal, --part for the provider's headroom or refusals,
// --limit) and clear-limit clears it; each takes the person's cited word (--reason), which the
// daemon records with the change and the operator as who made it. The daemon refuses a limit
// outside its signal's range, a name of no signal and a blank reason with the reason, changing
// nothing.
func projectSignals(act string) func(c *projectCall) action {
	return func(c *projectCall) action {
		reason := c.flags.String("reason", "", "the person's cited word for the change: whose word it is and what they said (required)")
		required := []string{"operator-token-file", "reason"}
		var signal, part *string
		var limit *float64
		if act == api.SignalsSetLimit || act == api.SignalsClearLimit {
			signal = c.flags.String("signal", "", "the signal: idle, queue, provider, kills or approvals (required)")
			part = c.flags.String("part", "", "the provider's part, headroom or refusals: required for provider, and no other signal takes one")
			required = append(required, "signal")
		}
		if act == api.SignalsSetLimit {
			limit = c.flags.Float64("limit", 0, "the limit, which the signal holds admission at or over: idle above 0 to 1, queue above 0 to 168 hours, "+
				"provider headroom above 0 to 10, and provider refusals, kills and approvals a whole number from 1 to 1000000 (required)")
		}
		return func(ctx context.Context, _ []string) int {
			if code, ok := c.check(required...); !ok {
				return code
			}
			req := api.OperatorSignalsRequest{Action: act, Reason: *reason}
			if signal != nil {
				req.Signal, req.Part = *signal, *part
			}
			if limit != nil {
				if !c.flags.Changed("limit") {
					return refuse(c.cmd, "--limit is required")
				}
				req.Limit = limit
			}
			return c.send(ctx, "/signals", req, printSignalsAnswer)
		}
	}
}

// printSignalsAnswer is the settings of admission by signals as the daemon answered a change, as
// one line: whether the change moved them, the switch, and each signal's limit.
func printSignalsAnswer(w io.Writer, answer []byte) error {
	var changed api.ProjectSignalsResponse
	if err := decodeAnswer(answer, &changed); err != nil {
		return err
	}
	verb := "changed"
	if !changed.Changed {
		verb = "unchanged, as it stood"
	}
	limits := make([]string, 0, len(changed.Settings.Limits))
	for _, limit := range changed.Settings.Limits {
		limits = append(limits, seriesWords(limit.Signal, limit.Part)+" "+limitWords(limit.Limit))
	}
	_, err := fmt.Fprintf(w, "admission by signals %s: %s; limits %s\n", verb, switchWords(changed.Settings.On), strings.Join(limits, ", "))
	return err
}

// signalsLine is `legion state`'s line for admission by signals (api.Admission.Signals): the
// switch, and each signal by its signal and part with its value, or not recorded where the daemon
// cannot compute it, its limit, and whether it holds admission, since when where a pass dated the
// hold, and undated where none did yet, as `legion state --json` gives them.
func signalsLine(reading api.SignalsReading) string {
	words := []string{"signals: " + switchWords(reading.On)}
	for _, series := range reading.Series {
		value := "not recorded"
		if series.Value != nil {
			value = strconv.FormatFloat(*series.Value, 'f', -1, 64)
		}
		hold := "does not hold"
		switch {
		case series.Holds && series.Since != nil:
			hold = "holds since " + series.Since.UTC().Format(time.RFC3339)
		case series.Holds:
			hold = "holds, undated"
		}
		words = append(words, fmt.Sprintf("%s %s %s %s", seriesWords(series.Signal, series.Part), value, limitWords(series.Limit), hold))
	}
	return strings.Join(words, "; ")
}

// switchWords is the switch of admission by signals in words.
func switchWords(on bool) string {
	if on {
		return "admission by signals on"
	}
	return "admission by signals off"
}

// seriesWords is a series by its signal and, for the provider, its part.
func seriesWords(signal string, part *string) string {
	if part == nil {
		return signal
	}
	return signal + "/" + *part
}

// limitWords is a series' limit in words, none where it has none.
func limitWords(limit *float64) string {
	if limit == nil {
		return "limit none"
	}
	return "limit " + strconv.FormatFloat(*limit, 'f', -1, 64)
}

// pausedSummary is `legion state`'s line for the project's open pause (api.State.Paused): the
// pause, who paused, since when and why, and the roles whose stop it still waits on, each by
// issue and role. It is empty while the project runs, and the caller then prints nothing.
func pausedSummary(paused *api.PausedView) string {
	if paused == nil {
		return ""
	}
	line := fmt.Sprintf("paused: pause %d since %s by %s: %s", paused.ID, paused.Since.Format(time.RFC3339),
		pauseActorWords(paused.By), paused.Reason)
	if len(paused.WaitsOn) == 0 {
		return line + "; waits on no role"
	}
	roles := make([]string, len(paused.WaitsOn))
	for i, stop := range paused.WaitsOn {
		roles[i] = stop.Issue + " " + string(stop.Role)
	}
	noun := "roles"
	if len(roles) == 1 {
		noun = "role"
	}
	return fmt.Sprintf("%s; waits on %d %s — %s", line, len(roles), noun, strings.Join(roles, ", "))
}

// lastPauseSummary is `legion state`'s line for the project's last pause while it runs
// (api.State.LastPause): the pause, who paused, since when and why, and when and by whom it was
// ended. It is empty while no pause has ended, and while the project is paused, when the state
// carries none, and the caller then prints nothing.
func lastPauseSummary(last *api.PauseView) string {
	if last == nil {
		return ""
	}
	return "last pause: " + endedPauseWords(*last)
}

// pauseWords is a pause as these lines name it: its id, since when, who paused and why.
func pauseWords(p api.PauseView) string {
	return fmt.Sprintf("pause %d since %s by %s: %s", p.ID, p.Since.Format(time.RFC3339), pauseActorWords(p.By), p.Reason)
}

// endedPauseWords is an ended pause as these lines name it: the pause, then when and by whom it
// was ended.
func endedPauseWords(p api.PauseView) string {
	if p.EndedAt == nil || p.EndedBy == nil {
		return pauseWords(p)
	}
	return fmt.Sprintf("%s; ended %s by %s", pauseWords(p), p.EndedAt.Format(time.RFC3339), pauseActorWords(*p.EndedBy))
}

// pauseActorWords is who acted on a pause: the controller with the session it acted from, or the
// operator.
func pauseActorWords(by api.PauseActorView) string {
	if by.Session != "" {
		return "the " + string(by.Kind) + " (session " + by.Session + ")"
	}
	return "the " + string(by.Kind)
}
