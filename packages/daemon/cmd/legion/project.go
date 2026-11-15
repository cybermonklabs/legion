package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/sjawhar/legion/daemon/internal/api"
)

// projectCommands is the operator's hand on the daemon's project as a whole, one subcommand per
// operator project route (internal/api/pause.go) and the one list `legion project --help` names.
// pause stops every role of the project but the controller and holds every launch, task and
// admission until resume lets the held work out through the wake pacing
// (dispatch://INFRA-3759); the daemon acts on its own project, so neither names one.
var projectCommands = map[string]command{
	"pause":  runProjectPause,
	"resume": runProjectResume,
}

func runProject(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	return runSubcommand(ctx, "project", projectCommands, args, stdout, stderr)
}

// projectRoute is the root of the daemon's operator project routes.
const projectRoute = "/legion/v1/operator/project"

// projectCall is one `legion project` subcommand: an operator command whose bearer is required,
// and which can print the daemon's answer as it came.
type projectCall struct {
	*operatorCall
	asJSON *bool
}

// newProjectCall is one `legion project` subcommand's flags: the operator's three and --json.
func newProjectCall(sub, usage string, stdout, stderr io.Writer) *projectCall {
	c := newOperatorCall("project "+sub, usage, "file holding the operator bearer the daemon's operator_token_file names (required)", stdout, stderr)
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

// runProjectPause is `legion project pause --reason <text>`: the daemon stops every role of its
// project but the controller and holds their work, records the operator as who paused and why,
// and tells the controller. The daemon refuses a second pause, naming the one that stands, and a
// configuration that cannot hold a pause, naming the setting (`legion start --check-config` names
// it too).
func runProjectPause(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	c := newProjectCall("pause", "usage: legion project pause --reason <text> [flags]", stdout, stderr)
	reason := c.flags.String("reason", "", "why the project is paused: the pause records it and the controller is told it (required)")
	if code, ok := c.parse(args, "operator-token-file", "reason"); !ok {
		return code
	}
	return c.send(ctx, "/pause", api.OperatorPauseRequest{Reason: *reason}, printPauseAnswer)
}

// runProjectResume is `legion project resume`: the daemon ends its project's pause, whoever made
// it, records the operator as who ended it, tells the controller, and lets the held work out
// through the wake pacing.
func runProjectResume(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	c := newProjectCall("resume", "usage: legion project resume [flags]", stdout, stderr)
	if code, ok := c.parse(args, "operator-token-file"); !ok {
		return code
	}
	return c.send(ctx, "/resume", nil, printResumeAnswer)
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
		return "the " + by.Kind + " (session " + by.Session + ")"
	}
	return "the " + by.Kind
}
