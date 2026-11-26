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
// operator project route (internal/api/pause.go and internal/api/p0_bound.go). pause stops every
// role of the project but the controller and holds every launch, task and admission until resume
// lets the held work out through the wake pacing (dispatch://INFRA-3759); set-p0-bound and
// clear-p0-bound change how many P0 issues may hold a slot outside the admission cap while the
// daemon runs (dispatch://INFRA-5636). The daemon acts on its own project, so none names one.
func newProjectCommand(stdout, stderr io.Writer) *cobra.Command {
	return group(&cobra.Command{Use: "project", Short: "pause or resume the daemon's project, or change its P0 bound"},
		projectCommand("pause", "pause the project: stop every role but the controller and hold their work", projectPause, stdout, stderr),
		projectCommand("resume", "end the project's pause and let the held work out through the wake pacing", projectResume, stdout, stderr),
		projectCommand("set-p0-bound", "set how many P0 issues may hold a slot outside the cap, or none, with no restart",
			projectSetP0Bound, stdout, stderr),
		projectCommand("clear-p0-bound", "return the P0 bound to legion.yaml's admission_p0_bound", projectClearP0Bound, stdout, stderr),
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

// projectSetP0Bound is `legion project set-p0-bound (--bound <n> | --none) --reason <text>`: the
// daemon records the operator's P0 bound and why, and admits under it from that request's own pass
// on, with no restart. --none sets no bound, whatever legion.yaml's admission_p0_bound says. A lower
// bound stops nothing that runs.
func projectSetP0Bound(c *projectCall) action {
	bound := c.flags.Int("bound", 0, "how many P0 issues may hold a slot at once, at least 1 (this or --none is required)")
	none := c.flags.Bool("none", false, "set no bound, whatever legion.yaml's admission_p0_bound says (this or --bound is required)")
	reason := c.flags.String("reason", "", "why the bound changes: the daemon records it with the change and logs it (required)")
	return func(ctx context.Context, _ []string) int {
		if code, ok := c.check("operator-token-file", "reason"); !ok {
			return code
		}
		req := api.OperatorP0BoundRequest{Unbounded: *none, Reason: *reason}
		switch named := c.flags.Changed("bound"); {
		case named && *none:
			return refuse(c.cmd, "--bound and --none name two bounds; give one")
		case !named && !*none:
			return refuse(c.cmd, "--bound <n> or --none is required")
		case named:
			req.Bound = bound
		}
		return c.send(ctx, "/p0-bound", req, printP0BoundAnswer)
	}
}

// projectClearP0Bound is `legion project clear-p0-bound --reason <text>`: the daemon records the
// clear and why, and admits under legion.yaml's admission_p0_bound again from that request's own
// pass on.
func projectClearP0Bound(c *projectCall) action {
	reason := c.flags.String("reason", "", "why the bound returns to legion.yaml's: the daemon records it with the change and logs it (required)")
	return func(ctx context.Context, _ []string) int {
		if code, ok := c.check("operator-token-file", "reason"); !ok {
			return code
		}
		return c.send(ctx, "/p0-bound/clear", api.OperatorP0BoundClearRequest{Reason: *reason}, printP0BoundAnswer)
	}
}

// printP0BoundAnswer is the P0 bound the daemon admits under after a change, as one line.
func printP0BoundAnswer(w io.Writer, answer []byte) error {
	var bound api.P0BoundView
	if err := decodeAnswer(answer, &bound); err != nil {
		return err
	}
	_, err := fmt.Fprintln(w, "P0 bound: "+p0BoundWords(bound))
	return err
}

// p0BoundWords is the P0 bound as `legion state` and a change's answer name it: the bound in force,
// legion.yaml's, and the value set while the daemon runs where one stands, with who set it, when
// and why. The reason is terminalText: the controller or the operator wrote it.
func p0BoundWords(view api.P0BoundView) string {
	words := boundCount(view.Bound) + " in force, " + boundCount(view.Configured) + " in legion.yaml"
	if view.Live == nil {
		return words + "; none set while the daemon runs"
	}
	return words + fmt.Sprintf("; set to %s by %s at %s: %s", boundCount(view.Live.Bound), pauseActorWords(view.Live.SetBy),
		view.Live.SetAt.Format(time.RFC3339), terminalText(view.Live.Reason))
}

// boundCount is a P0 bound as these lines write it: the number, or "none".
func boundCount(bound *int) string {
	if bound == nil {
		return "none"
	}
	return strconv.Itoa(*bound)
}

// p0AdmissionLine is `legion state`'s line for the P0 issues (C4 and C3 of dispatch://INFRA-5636):
// how many P0 issues hold a slot, outside the cap, beside the slots in use and the cap, and the P0
// bound in force, legion.yaml's and who set the live one and when. The daemon writes
// `admission.p0Holders` and `admission.p0Bound` together, and serves a caller below the P0 start's
// contract entry neither (withoutP0Start), so the line is empty unless the state carries both, and
// the caller then prints nothing.
func p0AdmissionLine(admission api.Admission) string {
	if admission.P0Holders == nil || admission.P0Bound == nil {
		return ""
	}
	return fmt.Sprintf("P0: %d of the %d issues holding a slot are P0 and hold it outside the cap of %d; P0 bound %s",
		*admission.P0Holders, len(admission.Active), admission.Cap, p0BoundWords(*admission.P0Bound))
}

// pausedSummary is `legion state`'s line for the project's open pause (api.State.Paused): the
// pause, who paused, since when and why, and the roles whose stop it still waits on, each by
// issue and role. It is empty while the project runs, and the caller then prints nothing. The
// reason is terminalText: the controller or the operator wrote it.
func pausedSummary(paused *api.PausedView) string {
	if paused == nil {
		return ""
	}
	line := fmt.Sprintf("paused: pause %d since %s by %s: %s", paused.ID, paused.Since.Format(time.RFC3339),
		pauseActorWords(paused.By), terminalText(paused.Reason))
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

// pauseWords is a pause as these lines name it: its id, since when, who paused and why. The reason
// is terminalText: the controller or the operator wrote it.
func pauseWords(p api.PauseView) string {
	return fmt.Sprintf("pause %d since %s by %s: %s", p.ID, p.Since.Format(time.RFC3339), pauseActorWords(p.By), terminalText(p.Reason))
}

// endedPauseWords is an ended pause as these lines name it: the pause, then when and by whom it
// was ended.
func endedPauseWords(p api.PauseView) string {
	if p.EndedAt == nil || p.EndedBy == nil {
		return pauseWords(p)
	}
	return fmt.Sprintf("%s; ended %s by %s", pauseWords(p), p.EndedAt.Format(time.RFC3339), pauseActorWords(*p.EndedBy))
}

// pauseActorWords is who acted on a pause or set the P0 bound: the controller with the session it
// acted from, or the operator. The session is terminalText: the controller's session named it.
func pauseActorWords(by api.PauseActorView) string {
	if by.Session != "" {
		return "the " + by.Kind + " (session " + terminalText(by.Session) + ")"
	}
	return "the " + by.Kind
}
