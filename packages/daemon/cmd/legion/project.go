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
	"github.com/sjawhar/legion/daemon/internal/redact"
)

// newProjectCommand is the operator's hand on the daemon's project as a whole, one command per
// operator project route (internal/api/pause.go and internal/api/p0_bound.go), but release, which
// starts the project release and reads it through its two routes
// (internal/api/project_release.go). pause stops every role of the project but the controller and
// holds every launch, task and admission until resume lets the held work out through the wake
// pacing (dispatch://INFRA-3759); release is the project release of dispatch://INFRA-3583;
// set-p0-bound and clear-p0-bound change how many P0 issues may hold a slot outside the admission
// cap while the daemon runs (dispatch://INFRA-5636). The daemon acts on its own project, so none
// names one.
func newProjectCommand(stdout, stderr io.Writer) *cobra.Command {
	return group(&cobra.Command{Use: "project", Short: "pause or resume the daemon's project, release every pod of it for a move to another cluster, or change its P0 bound"},
		projectCommand("pause", "pause the project: stop every role but the controller and hold their work", projectPause, stdout, stderr),
		projectCommand("resume", "end the project's pause and let the held work out through the wake pacing", projectResume, stdout, stderr),
		newProjectReleaseCommand(stdout, stderr),
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
	bound := c.flags.Int("bound", 0, "how many P0 issues may hold a slot at once, a whole number from 1 to 2147483647 (this or --none is required)")
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

// newProjectReleaseCommand is `legion project release`: the flags every project command takes
// (newProjectCall), and the release's own --bound, --wait and --dry-run. Its --wait is its own
// rather than withWait's, since the call it bounds waits out the release's bound (waitUntil).
func newProjectReleaseCommand(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "release",
		Short: "release every pod of the project, asking each role with unpushed work to push first, for a move to another cluster",
		Args:  noArguments,
	}
	c := newProjectCall(cmd, stdout, stderr)
	bound := c.flags.Duration("bound", 0, "how long the release waits for the roles it asks before it deletes every pod still waiting (default the daemon's pod_update_grace_minutes)")
	wait := c.flags.Duration("wait", 0, "how long the whole command may take before it ends: once the daemon answered, saying the release goes on, and before that, saying it may not have started (default the release's bound plus 30m)")
	dryRun := c.flags.Bool("dry-run", false, "print what a release would do from the reads now, without the settle of replacement records: read each pod's workspace, and ask, stop and delete nothing")
	cmd.RunE = runs(func(ctx context.Context, _ []string) int {
		return runProjectRelease(ctx, c.operatorCall, *bound, *wait, *dryRun, *c.asJSON)
	})
	return cmd
}

// projectReleasePoll is how often `legion project release` reads the release while it runs. A
// variable so that this package's tests read a release that finishes without waiting the poll
// out.
var projectReleasePoll = 5 * time.Second

// projectReleaseWaitBeyondBound is how much longer than the release's bound the command waits for
// it by default: the bound ends every pod's wait, and the stops, the deletes and the holds of a
// left pod's claims come after it.
const projectReleaseWaitBeyondBound = 30 * time.Minute

// releaseGoesOn ends each sentence this command's wait makes about a release the daemon answered:
// the release is the daemon's, which no bound of this command cancels, and the command run again
// reads it.
const releaseGoesOn = "the release goes on in the daemon. Run this command again to read it"

// releaseMayNotHaveStarted ends the sentence this command's wait makes when it ends before the
// daemon answered the release's POST. A request that ends while the release waits for a sweep, or
// before its record commits, has the daemon clear the release's mark and run no release
// (internal/daemon's projectReleaser.Start); a release whose record committed goes on with its
// answer lost. Either way the command run again starts the release or reads the one that runs.
const releaseMayNotHaveStarted = "the release may not have started. Run this command again: it starts the release, or reads the one the daemon started"

// runProjectRelease releases every pod of the project (R6): it asks the daemon to start the
// release, which a daemon that already ran one in this process answers with that release, reads
// the release every projectReleasePoll until it finished or --wait ends, and prints it — one line
// for each Sandbox of the project and one for each claim the release stopped. --dry-run asks for
// the outcome a release would give from the reads now, without the settle of replacement records,
// and stops nothing; a daemon that already ran a release in this process answers it with that
// release, which the command then reads and prints as a release.
//
// It exits 0 once the release finished and left no Sandbox of the project, and for a dry run's
// answer. It exits 1 when the release names a pod it left or stopped short, when the daemon
// refuses, and when --wait ends first: before the daemon answered the release's POST, saying that
// the release may not have started, and after, saying that the release goes on in the daemon. A
// pod deleted with work lost changes no exit: the line names that workspace. A usage error is 2.
func runProjectRelease(ctx context.Context, c *operatorCall, bound, wait time.Duration, dryRun, asJSON bool) int {
	if code, ok := c.check("operator-token-file"); !ok {
		return code
	}
	if bound < 0 || wait < 0 {
		return refuse(c.cmd, "--bound and --wait must not be negative")
	}
	op, ok := c.connect()
	if !ok {
		return 1
	}
	began := time.Now()
	// The call is bounded from its start. Until the daemon names the release's bound, the bound
	// the command was given stands in for it, and a bound the daemon chooses moves the end once
	// the daemon has named it (waitUntil).
	c.until = waitUntil(began, wait, bound)
	request := api.ProjectReleaseRequest{DryRun: dryRun}
	if bound > 0 {
		request.Bound = bound.String()
	}
	release, answer, cut, ok := releaseRequest(ctx, c, op, http.MethodPost, request)
	// The daemon has answered nothing yet, so which sentence ends the wait follows --dry-run.
	if cut {
		ended := releaseMayNotHaveStarted
		if dryRun {
			ended = "a dry run records and starts nothing"
		}
		fmt.Fprintf(c.stderr, "%s: %s %s was not answered before the command's wait of %s ended; %s\n",
			c.name(), http.MethodPost, redact.URL(op.base+api.ProjectReleaseRoute), c.until.Sub(began).Round(time.Millisecond), ended)
		return 1
	}
	if !ok {
		return 1
	}
	// What is printed, and how the command exits, follow the answer and not the flag: a daemon
	// that already ran a release in this process answers a dry run with that release, which may
	// still run or have left a pod.
	if release.DryRun {
		return printProjectRelease(c, release, answer, asJSON)
	}
	if dryRun {
		fmt.Fprintf(c.stderr, "%s: the daemon already ran release %d in this process, so it answered the dry run with that release; it is read as the release it is\n",
			c.name(), release.ID)
	}
	if wait == 0 {
		c.until = waitUntil(began, 0, release.BoundAt.Sub(release.StartedAt))
	}
	// Once the daemon has answered with a release, the end of the wait prints that release as the
	// command last read it, and says it goes on, whether the wait ends between two reads or
	// inside one: a read the wait cuts loses nothing the command already holds.
	waitEndedFirst := func() int {
		printProjectRelease(c, release, answer, asJSON)
		fmt.Fprintf(c.stderr, "%s: the command's wait of %s ended before release %d finished; %s\n",
			c.name(), c.until.Sub(began).Round(time.Millisecond), release.ID, releaseGoesOn)
		return 1
	}
	for release.FinishedAt == nil && release.EndedAt == nil {
		left := time.Until(c.until)
		if left <= 0 {
			return waitEndedFirst()
		}
		select {
		case <-ctx.Done():
			return 1
		case <-time.After(min(projectReleasePoll, left)):
		}
		if time.Now().After(c.until) {
			continue
		}
		read, readAnswer, cut, ok := releaseRequest(ctx, c, op, http.MethodGet, nil)
		if cut {
			return waitEndedFirst()
		}
		if !ok {
			return 1
		}
		release, answer = read, readAnswer
	}
	return printProjectRelease(c, release, answer, asJSON)
}

// releaseRequest makes one request of the release's route under the command's bound
// (operatorCall.requestCut) and answers the release the daemon served, decoded, with its body as
// served. A refusal and an answer that cannot be read are each printed and answer false. A request
// the bound ended answers false with cut true and is not printed: the command says what that end
// means, which depends on what the daemon has already answered.
func releaseRequest(ctx context.Context, c *operatorCall, op operator, method string, body any) (release api.ProjectReleaseResponse, answer []byte, cut, ok bool) {
	_, answer, cut, ok = c.requestCut(ctx, op, method, api.ProjectReleaseRoute, body)
	if !ok {
		return api.ProjectReleaseResponse{}, nil, cut, false
	}
	if err := decodeAnswer(answer, &release); err != nil {
		fmt.Fprintf(c.stderr, "%s: read the answer %s served: %v\n", c.name(), redact.URL(op.base+api.ProjectReleaseRoute), err)
		return api.ProjectReleaseResponse{}, nil, false, false
	}
	return release, answer, false, true
}

// waitUntil is when the command's call ends: --wait from its start when one was given, and
// otherwise the release's bound plus projectReleaseWaitBeyondBound.
func waitUntil(began time.Time, wait, bound time.Duration) time.Time {
	if wait > 0 {
		return began.Add(wait)
	}
	return began.Add(bound + projectReleaseWaitBeyondBound)
}

// printProjectRelease prints release, as the daemon served it under --json and as its lines
// otherwise, and answers the command's exit: 0 for a dry run, and for a release that finished
// leaving no Sandbox of the project; 1 for one that left a Sandbox or stopped short. A release a
// boot ended before it finished is one that stopped short. The exit of a release that has not
// finished is its caller's.
func printProjectRelease(c *operatorCall, release api.ProjectReleaseResponse, answer []byte, asJSON bool) int {
	if asJSON {
		if err := writeAnswer(c.stdout, answer); err != nil {
			return 1
		}
	} else {
		for _, line := range release.Result.Sandboxes {
			fmt.Fprintln(c.stdout, releasedSandboxLine(line, release.DryRun))
		}
		for _, line := range release.Result.Claims {
			fmt.Fprintln(c.stdout, releasedClaimLine(line, release.DryRun))
		}
	}
	if release.DryRun || release.FinishedAt == nil && release.EndedAt == nil {
		return 0
	}
	code := 0
	if release.Result.Failure != "" {
		fmt.Fprintf(c.stderr, "%s: release %d stopped short: %s\n", c.name(), release.ID, release.Result.Failure)
		code = 1
	}
	if release.FinishedAt == nil {
		fmt.Fprintf(c.stderr, "%s: release %d was ended by a %s before it finished; read the Sandboxes left in the cluster before the project moves\n",
			c.name(), release.ID, orNone(string(release.EndedBy)))
		code = 1
	}
	var left []string
	for _, line := range release.Result.Sandboxes {
		if !line.Deleted() {
			left = append(left, line.Name)
		}
	}
	if len(left) > 0 {
		fmt.Fprintf(c.stderr, "%s: release %d left %d Sandbox(es) of the project: %s; move nothing while one stands (docs/kubernetes.md, \"Releasing a project's pods\")\n",
			c.name(), release.ID, len(left), strings.Join(left, ", "))
		code = 1
	}
	return code
}

// releasedSandboxLine is one Sandbox's line: `sandbox <name> <kind> <issue> <outcome>`, then what
// the outcome carries — the workspace's unpushed commits and whether its working copy held a
// change at the last read, or at an earlier read where no role process ran at the last one, the
// shim's refusal, why the Sandbox was left, or the other reasons an issue pod's line gives — then
// a pod that runs another image than the daemon's own, and each role on the pod's line — until
// the bound each role the ask was written to, at it the roles the ask reached or that answered —
// with when it answered or that the bound passed. On a dry run the outcome is the one a release
// would give from the reads now, without the settle of replacement records, and the asked roles
// are the ones it would ask.
func releasedSandboxLine(line api.ReleasedSandbox, dryRun bool) string {
	words := []string{"sandbox", line.Name, string(line.Kind), orNone(line.Issue), string(line.Outcome)}
	var details []string
	if line.Commits != nil {
		changed := "no change"
		if line.WorkingCopyChanged != nil && *line.WorkingCopyChanged {
			changed = "a change"
		}
		// A refused last read writes no count, so a count on a deleted_read_refused line is the one
		// an earlier read took, kept because no role process ran at the last (api.ReleasedSandbox).
		read := "the last read"
		if line.Outcome == api.ReleaseDeletedReadRefused {
			read = "an earlier read"
		}
		details = append(details, fmt.Sprintf("the workspace of %s held %d unpushed commit(s) and its working copy %s at %s", orNone(line.Issue), *line.Commits, changed, read))
	}
	if line.Refusal != "" {
		details = append(details, "the shim refused the read: "+line.Refusal)
	}
	if line.Reason != "" {
		why := line.Reason
		if line.Outcome == api.ReleaseLeft {
			why = "left because " + why
		}
		details = append(details, why)
	}
	if line.OtherImage {
		details = append(details, "its pod runs an image other than the daemon's own")
	}
	if len(line.Asked) > 0 {
		asked := make([]string, len(line.Asked))
		for i, role := range line.Asked {
			switch {
			case dryRun:
				asked[i] = string(role.Role)
			case role.AnsweredAt != nil:
				asked[i] = string(role.Role) + " answered at " + role.AnsweredAt.UTC().Format(time.RFC3339)
			case role.Bound:
				asked[i] = string(role.Role) + " did not answer by the bound"
			default:
				asked[i] = string(role.Role) + " has not answered yet"
			}
		}
		verb := "asked "
		if dryRun {
			verb = "a release would ask "
		}
		details = append(details, verb+strings.Join(asked, ", "))
	}
	if len(details) == 0 {
		return strings.Join(words, " ")
	}
	return strings.Join(words, " ") + ": " + strings.Join(details, "; ")
}

// releasedClaimLine is one stopped claim's line: `claim <token> <issue> <role> <session>`, how it
// ended, and whether the daemon's next boot starts it.
func releasedClaimLine(line api.ReleasedClaim, dryRun bool) string {
	words := []string{"claim", string(line.Token), orNone(line.Issue), string(line.Role), orNone(line.Session), orNone(string(line.Ended))}
	switch {
	case line.BootStarts && dryRun:
		words = append(words, "(the next boot would start it)")
	case line.BootStarts:
		words = append(words, "(the next boot starts it)")
	case line.Ended != "":
		words = append(words, "(the next boot does not start it)")
	}
	if line.Detail != "" {
		words = append(words, "("+line.Detail+")")
	}
	return strings.Join(words, " ")
}
