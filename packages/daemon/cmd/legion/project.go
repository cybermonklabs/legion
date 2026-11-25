package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/sjawhar/legion/daemon/internal/api"
)

// newProjectCommand is the operator's hand on the daemon's project as a whole, one command per
// operator project route (internal/api/pause.go and internal/api/admission_cap.go). pause stops
// every role of the project but the controller and holds every launch, task and admission until
// resume lets the held work out through the wake pacing (dispatch://INFRA-3759); set-admission-cap
// and clear-admission-cap change the cap the daemon admits under while it runs, up to legion.yaml's
// admission_cap (dispatch://INFRA-5616). The daemon acts on its own project, so none names one.
func newProjectCommand(stdout, stderr io.Writer) *cobra.Command {
	return group(&cobra.Command{Use: "project", Short: "pause or resume the daemon's project, or change the cap it admits under"},
		projectCommand("pause", "pause the project: stop every role but the controller and hold their work", projectPause, stdout, stderr),
		projectCommand("resume", "end the project's pause and let the held work out through the wake pacing", projectResume, stdout, stderr),
		projectCommand("set-admission-cap", "set the cap the daemon admits under, up to legion.yaml's admission_cap, with no restart",
			projectSetAdmissionCap, stdout, stderr),
		projectCommand("clear-admission-cap", "return the daemon to legion.yaml's admission_cap", projectClearAdmissionCap, stdout, stderr),
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

// projectSetAdmissionCap is `legion project set-admission-cap --cap <n> --reason <text>`: the
// daemon records the operator's cap and why, and admits under it from that request's own pass on,
// with no restart. A lower cap stops nothing that runs. The daemon refuses a cap below 1, and one
// above legion.yaml's admission_cap, its ceiling, naming the ceiling: raising the ceiling is a
// change of legion.yaml and a restart.
func projectSetAdmissionCap(c *projectCall) action {
	capValue := c.flags.Int("cap", 0, "how many issues the daemon may have started at once, from 1 to legion.yaml's admission_cap (required)")
	reason := c.flags.String("reason", "", "why the cap changes: the daemon records it with the change and logs it (required)")
	return func(ctx context.Context, _ []string) int {
		if code, ok := c.check("operator-token-file", "reason"); !ok {
			return code
		}
		if !c.flags.Changed("cap") {
			return refuse(c.cmd, "--cap is required")
		}
		return c.send(ctx, "/admission-cap", api.OperatorAdmissionCapRequest{Cap: *capValue, Reason: *reason}, printAdmissionCapAnswer)
	}
}

// projectClearAdmissionCap is `legion project clear-admission-cap --reason <text>`: the daemon
// records the clear and why, and admits under legion.yaml's admission_cap again from that request's
// own pass on.
func projectClearAdmissionCap(c *projectCall) action {
	reason := c.flags.String("reason", "", "why the cap returns to legion.yaml's admission_cap: the daemon records it with the change and logs it (required)")
	return func(ctx context.Context, _ []string) int {
		if code, ok := c.check("operator-token-file", "reason"); !ok {
			return code
		}
		return c.send(ctx, "/admission-cap/clear", api.OperatorAdmissionCapClearRequest{Reason: *reason}, printAdmissionCapAnswer)
	}
}

// printAdmissionCapAnswer is the cap the daemon admits under after a change, as one line.
func printAdmissionCapAnswer(w io.Writer, answer []byte) error {
	var changed api.AdmissionCapResponse
	if err := decodeAnswer(answer, &changed); err != nil {
		return err
	}
	_, err := fmt.Fprintln(w, "admission cap: "+admissionCapWords(changed.Cap, changed.Ceiling, changed.Override))
	return err
}

// admissionCapWords is the cap the daemon admits under as `legion state` and a change's answer name
// it: the cap, the ceiling, and the cap set while the daemon runs where one stands, with who set it,
// when and why. The reason is a person's words, quoted, so a newline in it cannot pose as a line of
// its own.
func admissionCapWords(capValue, ceiling int, override *api.AdmissionCapOverride) string {
	words := fmt.Sprintf("%d in force, ceiling %d", capValue, ceiling)
	if override == nil {
		return words + "; no cap set while the daemon runs"
	}
	return words + fmt.Sprintf("; set to %d by %s at %s: %q", override.Cap, pauseActorWords(override.SetBy), override.SetAt.Format(time.RFC3339), override.Reason)
}

// pausedSummary is `legion state`'s line for the project's open pause (api.State.Paused): the
// pause, who paused, since when and why, and the roles whose stop it still waits on, each by
// issue and role. It is empty while the project runs, and the caller then prints nothing.
func pausedSummary(paused *api.PausedView) string {
	if paused == nil {
		return ""
	}
	line := fmt.Sprintf("paused: pause %d since %s by %s: %q", paused.ID, paused.Since.Format(time.RFC3339),
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

// pauseWords is a pause as these lines name it: its id, since when, who paused and why. The reason
// is a person's words, quoted, so a newline in it cannot pose as a line of its own.
func pauseWords(p api.PauseView) string {
	return fmt.Sprintf("pause %d since %s by %s: %q", p.ID, p.Since.Format(time.RFC3339), pauseActorWords(p.By), p.Reason)
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
		return "the " + by.Kind + " (session " + by.Session + ")"
	}
	return "the " + by.Kind
}
