package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/sjawhar/legion/daemon/internal/api"
	legionclaim "github.com/sjawhar/legion/daemon/internal/claim" // main_test.go's `claim` helper holds the bare name
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// newClaimsCommand is the operator's hand on the daemon's claims, one command per operator route
// (internal/api/operator.go). retry relaunches a failed claim: an architect on its session with
// fresh budgets, a phase worker through its issue's held phase; with --failed, every failed claim.
// compact shortens a running claim's session and writes again the task it owes, which the claim
// takes: what a claim whose provider keeps refusing its session reaches for before a fresh one.
// close is the tree's close, through its root claim, for a tree no workflow issue backs: the
// root, then every other claim of the tree. stop retires one claim, and waits out a stop the
// daemon holds for the report of a turn its take has ended (claimsStop).
func newClaimsCommand(stdout, stderr io.Writer) *cobra.Command {
	return group(&cobra.Command{Use: "claims", Short: "the operator's hand on the daemon's claims"},
		claimsCommand("spawn", "create a claim on a role of an issue and launch it", claimsSpawn, stdout, stderr),
		claimsCommand("deliver", "give a claim a task", claimsDeliver, stdout, stderr),
		claimsCommand("suspend", "suspend a claim, once the turn its agent is in ends", claimsSuspend, stdout, stderr),
		claimsCommand("resume", "resume a suspended claim", claimRequest("resume"), stdout, stderr),
		claimsCommand("stop", "stop a claim's agent, once the report of a turn its take has ended comes", claimsStop, stdout, stderr),
		claimsCommand("retry", "retry a failed claim, or every failed claim; a claim with no process whose launch waits its paced turn launches now", claimsRetry, stdout, stderr),
		claimsCommand("compact", "compact a claim's Oh My Pi session and write again the task it owes, for it to take", claimsCompact, stdout, stderr),
		claimsCommand("forget-session", "clear a stopped claim's recorded session", claimsForgetSession, stdout, stderr),
		claimsCommand("release-names", "free the names of a claim's other sessions once the secrets broker carries nothing of them", claimsReleaseNames, stdout, stderr),
		claimsCommand("close", "close a tree no workflow issue backs, through its root claim", claimRequest("close"), stdout, stderr),
		claimsCommand("list", "print every claim the daemon supervises, one line each", claimsList, stdout, stderr),
	)
}

// claimsCommand is `legion claims <sub>`: declare gives it the flags of its own beside the ones
// every claims command takes (newClaimsCall), and answers its body.
func claimsCommand(sub, short string, declare func(c *claimsCall) action, stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{Use: sub, Short: short, Args: noArguments}
	cmd.RunE = runs(declare(newClaimsCall(cmd, stdout, stderr)))
	return cmd
}

// claimsRoute is the root of the daemon's operator claim routes.
const claimsRoute = "/legion/v1/operator/claims"

// claimsCall is one `legion claims` subcommand: an operator command whose bearer is required, and
// which can print the daemon's answer as it came.
type claimsCall struct {
	*operatorCall
	asJSON *bool
}

// newClaimsCall gives cmd, one `legion claims` subcommand, its flags: the operator's three,
// --json, and the --wait that bounds the whole call. Every subcommand takes it, so every one of
// them answers — done, held until when, or refused with a reason — inside a bound the operator set
// (dispatch://INFRA-714 R10).
func newClaimsCall(cmd *cobra.Command, stdout, stderr io.Writer) *claimsCall {
	c := newOperatorCall(cmd, "file holding the operator bearer the daemon's operator_token_file names (required)", stdout, stderr)
	c.withWait("how long the whole command may take before it refuses with what it was waiting for")
	return &claimsCall{operatorCall: c, asJSON: c.flags.Bool("json", false, "print the daemon's answer as it served it")}
}

// check is operatorCall.check with --operator-token-file among the required flags.
func (c *claimsCall) check(required ...string) (code int, ok bool) {
	return c.operatorCall.check(append([]string{"operator-token-file"}, required...)...)
}

// send is operatorCall.send on an operator claim route, printing through printer.
func (c *claimsCall) send(ctx context.Context, op operator, method, path string, body any, print func(io.Writer, []byte) error) int {
	return c.operatorCall.send(ctx, op, method, claimsRoute+path, body, c.printer(print))
}

// printer is print, or under --json the answer as the daemon served it.
func (c *claimsCall) printer(print func(io.Writer, []byte) error) func(io.Writer, []byte) error {
	if *c.asJSON {
		return writeAnswer
	}
	return print
}

// writeAnswer writes a 2xx answer as the daemon served it.
func writeAnswer(w io.Writer, answer []byte) error {
	_, err := w.Write(answer)
	return err
}

// decodeAnswer reads a 2xx answer into into, refusing a member the struct does not have: a daemon
// whose wire this binary does not speak says so rather than printing half of it.
func decodeAnswer(answer []byte, into any) error {
	decoder := json.NewDecoder(bytes.NewReader(answer))
	decoder.DisallowUnknownFields()
	return decoder.Decode(into)
}

// printClaim is the claim a route answered, as the one line writeClaimLine writes.
//
// A second line follows it when the answer says what the request did not do, which only
// `retry --fresh-session` sets (api.OperatorClaim.PhaseNotRestarted): the claim is back in a
// new session owing no task and the workflow refused the restart that would have handed it
// one, or the claim was stopped and the machine refused the forget or the start that would
// have brought it back. The line above reports the claim as it stands, which is a success in
// the first of those and a stopped or launching claim in the second, and without this one an
// operator would read either as work handed back.
func printClaim(w io.Writer, answer []byte) error {
	var c api.OperatorClaim
	if err := decodeAnswer(answer, &c); err != nil {
		return err
	}
	writeClaimLine(w, c)
	if c.PhaseNotRestarted != "" {
		fmt.Fprintln(w, c.PhaseNotRestarted)
	}
	return nil
}

// printClaims is the listing: one line for each claim, and under them the one line that says what
// the daemon's last pod-update sweep decided (podUpdateRoundsLine). That line is printed whenever
// the command prints at all, over no claims as over many, because an operator served no line
// cannot tell a daemon holding no issue back from a binary too old to know the limits.
func printClaims(w io.Writer, answer []byte) error {
	var list api.OperatorClaims
	if err := decodeAnswer(answer, &list); err != nil {
		return err
	}
	for _, c := range list.Claims {
		writeClaimLine(w, c)
	}
	fmt.Fprintln(w, podUpdateRoundsLine(list.PodUpdateRounds))
	return nil
}

// podUpdateRoundsLine is the one line an operator reads for the pod rounds the daemon's last
// sweep decided (api.PodUpdateRounds): the project's two limits, the issues holding a round under
// them, and the issues each limit held back. No claim's own line says either half — an issue in a
// round has every worker of it told to do the steps of its update message (supervise.UpdateTask)
// and start nothing new, and an issue a limit held back is drifted and was posted nothing at
// all — so a roll an operator thinks is stuck is read here.
//
// A daemon that has not swept says so rather than print each list empty, which would read as a
// sweep that found nothing to do.
//
// A daemon that does not report the rounds at all says that instead. decodeAnswer refuses a
// member this binary does not know but takes a body that leaves one out, so an answer from a
// release before the daemon served this record decodes with the field nil rather than failing.
// Read as the zero record it would print the line above, and an operator would be told a daemon
// that sweeps every minute has not swept yet.
func podUpdateRoundsLine(rounds *api.PodUpdateRounds) string {
	if rounds == nil {
		return "pod update rounds: this daemon does not report them; it is older than this binary"
	}
	if !rounds.Swept {
		return "pod update rounds: the daemon has not swept yet"
	}
	return fmt.Sprintf("pod update rounds: max open %s, %s a minute; %s; %s; %s",
		limitOrNoLimit(rounds.MaxOpenRounds), limitOrNoLimit(rounds.RoundsPerMinute),
		issuesOrNone("in a round", rounds.InRound),
		issuesOrNone("waiting on the open-round limit", rounds.WaitingOnMaxOpenRounds),
		issuesOrNone("waiting on the limit a minute", rounds.WaitingOnRoundsPerMinute))
}

// limitOrNoLimit is one of the project's two limits as the line prints it: the number legion.yaml
// sets, or `no limit` for a key it does not set, which asks for no limit rather than one of none.
func limitOrNoLimit(limit *int) string {
	if limit == nil {
		return "no limit"
	}
	return strconv.Itoa(*limit)
}

// namedIssueCap is how many issue keys one list of that line names. One template change drifts
// every pod of the project at once, so the issues waiting on a limit are a fleet's worth of keys:
// named in full they would scroll the claims themselves off the operator's screen, and the count
// in front of them is the whole number either way.
const namedIssueCap = 20

// issuesOrNone is one list of that line: its name, then `none`, or how many issues it holds and
// the first of them in key order, with `and <n> more` in place of the rest. It is the shape
// `legion state` prints its own summaries in (main.go's roundsSummary).
func issuesOrNone(name string, issues []string) string {
	if len(issues) == 0 {
		return name + ": none"
	}
	named := issues
	if len(named) > namedIssueCap {
		named = append(slices.Clone(named[:namedIssueCap]), fmt.Sprintf("and %d more", len(issues)-namedIssueCap))
	}
	return fmt.Sprintf("%s: %d — %s", name, len(issues), strings.Join(named, ", "))
}

// writeClaimLine is the one line a claim prints as: token, role, issue, state, why it failed
// (claimCauseWord), generation, the plugin contract its registration declared (0 when none is
// recorded), the update round the claim is in with the head its worker reported when it
// answered (`-` for a claim in no round, and `-` for a head no worker has reported yet), the
// call its record holds open (openCallWord), and last the sentence its latest launch try was
// refused with because the shared public leak check could not be installed in its pod, quoted so
// the line stays one line per claim (`-` for a claim no leak check refused). This listing is
// where the controller's cause is read, since its claim is on no claim view of the state. A
// claim whose idle pod the daemon released prints `suspended:released` for its state
// (claimStateWord): it is not a suspension anyone is waiting to lift — its next delivery resumes
// it in a pod of its own — so an operator reading the list must not read it as one.
func writeClaimLine(w io.Writer, c api.OperatorClaim) {
	fmt.Fprintf(w, "%s %s %s %s %s %d %d %s %s %s %s\n", c.Token, c.Role, c.Issue, claimStateWord(c), orNone(claimCauseWord(c)),
		c.Generation, c.PluginContract,
		orNone(c.UpdateRound), quotedOrNone(c.UpdateHead), quotedOrNone(openCallWord(c.Step)), quotedOrNone(c.LeakCheckCause))
}

// openCallWord is the call a claim's record holds open, whatever the claim's state, as the line
// prints it: the tool, the call id the stream's start and end frames pair by, and when the daemon
// received the start. Only a working claim's agent is in that call: a retired or suspended claim
// keeps the step it was last in on its record. It is empty for a claim whose record holds no open
// call — one whose last call ended, and one whose agent has not stepped at all — which the line
// prints as `-`.
//
// A call the host backgrounded is an ended call here, and its command may still be running: Oh
// My Pi backgrounds a plain foreground shell command that has not returned in about a minute, or
// at once when a steering message reaches the turn, ending the call with no error and handing
// the job to the agent, which is a `tool_execution_end` on
// the stream like any other (measured 2026-11-08 in the plugin's own live proof,
// legion-stall-watchdog-omp.test.ts). So for a working claim this column says what the agent is
// parked in, which is what a stalled turn is read from, and never what is running on the worker's
// machine.
//
// A call still open is the one shape a stalled turn leaves behind (api.StepView), and this is
// where an operator reads a controller's: internal/projection puts the controller's claim on no
// issue, so `legion state` never shows one. The call's arguments are not here and are in no
// column of any surface: the daemon keeps their length and a fingerprint alone, because a shell
// command routinely holds a credential (supervise.StepRecord).
func openCallWord(step *api.StepView) string {
	if step == nil || step.StartedAt == nil || step.EndedAt != nil {
		return ""
	}
	return fmt.Sprintf("%s %s since %s", step.Tool, step.CallID, step.StartedAt.UTC().Format(time.RFC3339))
}

// claimStateWord is the claim's state as the line prints it: `suspended:released` for a claim the
// idle release suspended, and the state itself for every other claim, whose suspendedBy the
// operator reads with --json.
func claimStateWord(c api.OperatorClaim) string {
	if c.State == string(supervise.StateSuspended) && c.SuspendedBy == string(supervise.SuspendedByReleased) {
		return c.State + ":" + c.SuspendedBy
	}
	return c.State
}

// claimCauseWord is why the claim failed as the line prints it: the one word the site that
// failed it chose (supervise.FailureCause), which the line carries beside the state rather than
// the sentence written with it, because one word is what fits a column and what an operator
// sorts a listing by; the sentence, the moment and the counts are read with --json
// (api.ClaimFailureView). It is empty for a claim the daemon holds no failure record for — one
// that never failed, one that has been given another run since, which clears the record, and one
// holding a cause with no sentence (api.FailureViewOf) — which the line prints as `-`.
func claimCauseWord(c api.OperatorClaim) string {
	if c.Failure == nil {
		return ""
	}
	return string(c.Failure.Cause)
}

// orNone is a field a claim need not have, as the line prints it.
func orNone(value string) string {
	if value == "" {
		return "-"
	}
	return value
}

// quotedOrNone is a field a worker supplies, as the line prints it: Go-quoted, so a newline or any
// other control character in it is an escape rather than another row of this listing. The head a
// worker reports when it answers an update round is its own word, which nothing verifies
// (api.OperatorClaim.UpdateHead), and the tool and call id of the call its agent is in are the
// agent's own, as its stream's frames named them (openCallWord).
func quotedOrNone(value string) string {
	if value == "" {
		return "-"
	}
	return strconv.Quote(value)
}

// claimsSpawn creates the claim on --role of --issue, in the tree --tree roots, and launches it.
// --prompt-file is an optional operator override; omitted, the daemon composes the role's shared
// and Go-specific parts. --task is its first delivery, sent once the agent is ready.
func claimsSpawn(c *claimsCall) action {
	tree := c.flags.String("tree", "", "issue key of the tree's root (required)")
	issue := c.flags.String("issue", "", "issue key the claim is on (required)")
	role := c.flags.String("role", "", "role the claim holds on the issue (required)")
	promptFile := c.flags.String("prompt-file", "", "file holding an operator role-prompt override")
	task := c.flags.String("task", "", "the claim's first task, sent once its agent is ready")
	return func(ctx context.Context, _ []string) int {
		if code, ok := c.check("tree", "issue", "role"); !ok {
			return code
		}
		op, ok := c.connect()
		if !ok {
			return 1
		}
		prompt := ""
		if *promptFile != "" {
			body, err := os.ReadFile(*promptFile)
			if err != nil {
				fmt.Fprintf(c.stderr, "%s: read the role prompt: %v\n", c.name(), err)
				return 1
			}
			prompt = string(body)
		}
		return c.send(ctx, op, http.MethodPost, "", api.SpawnRequest{
			Tree: *tree, Issue: *issue, Role: legionclaim.Role(*role), Prompt: prompt, Task: *task,
		}, printClaim)
	}
}

// claimsDeliver gives --claim the task --task.
func claimsDeliver(c *claimsCall) action {
	token := c.flags.String("claim", "", "the claim's token (required)")
	task := c.flags.String("task", "", "the task (required)")
	return func(ctx context.Context, _ []string) int {
		if code, ok := c.check("claim", "task"); !ok {
			return code
		}
		op, ok := c.connect()
		if !ok {
			return 1
		}
		return c.send(ctx, op, http.MethodPost, "/"+url.PathEscape(*token)+"/deliver", api.DeliverRequest{Task: *task}, printClaim)
	}
}

// claimRequest is a subcommand that posts request, with no body, to --claim's route.
func claimRequest(request string) func(c *claimsCall) action {
	return func(c *claimsCall) action {
		token := c.flags.String("claim", "", "the claim's token (required)")
		return func(ctx context.Context, _ []string) int {
			if code, ok := c.check("claim"); !ok {
				return code
			}
			op, ok := c.connect()
			if !ok {
				return 1
			}
			return c.send(ctx, op, http.MethodPost, "/"+url.PathEscape(*token)+"/"+request, nil, printClaim)
		}
	}
}

// claimsForgetSession clears the recorded session of a stopped claim. The operator must name the
// session being abandoned, so it cannot clear a newly registered session by mistake.
func claimsForgetSession(c *claimsCall) action {
	token := c.flags.String("claim", "", "the claim's token (required)")
	session := c.flags.String("session", "", "the claim's recorded session to abandon (required)")
	return func(ctx context.Context, _ []string) int {
		if code, ok := c.check("claim", "session"); !ok {
			return code
		}
		op, ok := c.connect()
		if !ok {
			return 1
		}
		return c.send(ctx, op, http.MethodPost, "/"+url.PathEscape(*token)+"/forget-session",
			api.ForgetSessionRequest{Session: *session}, printClaim)
	}
}

// claimsReleaseNames frees the names of --claim's other sessions once the secrets broker carries
// nothing of them (api's releaseNames): what a claim refused a new session at the most names one
// claim may hold needs, often right after a forget-session. A refusal names each enrollment the
// broker still carries and when it ends; nothing is freed then, and the operator runs it again.
// Where the daemon registers pod sessions (exchange_audience), the broker releases the session
// names among them first, and a broker that does not answer that release (503) or refuses it (502)
// frees nothing either. Each refusal is printed as every one is, the daemon's status and sentence
// (operatorCall.request), and the command fails.
func claimsReleaseNames(c *claimsCall) action {
	token := c.flags.String("claim", "", "the claim's token (required)")
	return func(ctx context.Context, _ []string) int {
		if code, ok := c.check("claim"); !ok {
			return code
		}
		op, ok := c.connect()
		if !ok {
			return 1
		}
		return c.send(ctx, op, http.MethodPost, "/"+url.PathEscape(*token)+"/release-names", nil, printReleasedNames)
	}
}

// printReleasedNames is a release's answer as one line: how many names it freed, how it knew no
// grant follows one, and the abandoned session it freed, if any.
func printReleasedNames(w io.Writer, answer []byte) error {
	var released api.ReleasedNames
	if err := decodeAnswer(answer, &released); err != nil {
		return err
	}
	line := fmt.Sprintf("%s: freed %d names of its other sessions; %s", released.Claim, len(released.Freed), released.Checked)
	if released.Abandoned != "" {
		line += "; the abandoned session " + released.Abandoned + " is marked released"
	}
	_, err := fmt.Fprintln(w, line)
	return err
}

// claimsCompact compacts --claim's Oh My Pi session and has the daemon write again whatever task
// the claim owes, which the claim takes: what a claim whose provider keeps refusing its session
// reaches for before a fresh session, since the session, its workspace and everything the worker
// knows of its issue stay where they are (api's compact).
//
// The request carries no deadline of its own, as no `legion claims` request does: a compaction
// is a model call over the whole transcript, and the daemon bounds it at ten minutes
// (runtime.CompactTimeout). The daemon's answer is printed whether or not the session was
// compacted — a session too small to compact, or one already compacted, is reported with Oh My
// Pi's own words for it, and whatever task the claim owed has gone again in either case.
func claimsCompact(c *claimsCall) action {
	token := c.flags.String("claim", "", "the claim's token (required)")
	return func(ctx context.Context, _ []string) int {
		if code, ok := c.check("claim"); !ok {
			return code
		}
		op, ok := c.connect()
		if !ok {
			return 1
		}
		return c.send(ctx, op, http.MethodPost, "/"+url.PathEscape(*token)+"/compact", nil, printCompaction)
	}
}

// printCompaction is the one line a compaction prints: whether the session was compacted, and
// why it was not when it was not.
func printCompaction(w io.Writer, answer []byte) error {
	var compaction api.CompactResponse
	if err := decodeAnswer(answer, &compaction); err != nil {
		return err
	}
	if compaction.Compacted {
		_, err := fmt.Fprintln(w, "compacted; whatever task the claim owed has gone again")
		return err
	}
	_, err := fmt.Fprintf(w, "not compacted: %s; whatever task the claim owed has gone again\n", orNone(compaction.Reason))
	return err
}

// claimsRetry retries --claim, or with --failed every claim the daemon holds failed, in the
// order the daemon lists them: an architect is relaunched on its session with fresh budgets, a
// phase worker is relaunched by the start of its issue's phase — the phase it is held from, or the
// one it stands in, which is where an operator's stop leaves it — and the daemon refuses the rest,
// naming why (api's retryClaim). Under --failed each retried claim prints its line as --claim
// prints it, each refusal is reported naming its claim, and the command fails once any was
// refused, having asked for every one. A claim an operator stopped is retired rather than failed,
// so --failed passes over it: name it with --claim.
//
// --fresh-session starts the claim in a new session rather than resuming the recorded one. Every
// other retry resumes the recorded session with fresh budgets, whatever its run of refused turns
// (supervise's freshSession): a new session is the operator's or the controller's choice, for an
// agent wedged in a way no budget counts, or a session its provider keeps refusing when a new one
// would not be.
func claimsRetry(c *claimsCall) action {
	token := c.flags.String("claim", "", "the claim's token (this, or --failed)")
	failed := c.flags.Bool("failed", false, "retry every claim the daemon holds failed")
	fresh := c.flags.Bool("fresh-session", false, "start the claim in a new session rather than resuming the recorded one")
	return func(ctx context.Context, _ []string) int { return c.retry(ctx, *token, *failed, *fresh) }
}

func (c *claimsCall) retry(ctx context.Context, token string, failed, fresh bool) int {
	if code, ok := c.check(); !ok {
		return code
	}
	if (token == "") != failed {
		if failed {
			return refuse(c.cmd, "--claim and --failed each name what to retry; give one")
		}
		return refuse(c.cmd, "--claim is required, or --failed")
	}
	op, ok := c.connect()
	if !ok {
		return 1
	}
	// A retry that asks for nothing sends no body at all, as every retry before the flag did: the
	// daemon reads the body only when one is there.
	var retry any
	if fresh {
		retry = api.RetryRequest{FreshSession: true}
	}
	if !failed {
		return c.send(ctx, op, http.MethodPost, "/"+url.PathEscape(token)+"/retry", retry, printClaim)
	}
	_, answer, ok := c.request(ctx, op, http.MethodGet, claimsRoute, nil)
	if !ok {
		return 1
	}
	var list api.OperatorClaims
	if err := decodeAnswer(answer, &list); err != nil {
		fmt.Fprintf(c.stderr, "%s: read the answer %s%s served: %v\n", c.name(), op.base, claimsRoute, err)
		return 1
	}
	code := 0
	for _, held := range list.Claims {
		if supervise.ClaimState(held.State) != supervise.StateFailed {
			continue
		}
		if c.send(ctx, op, http.MethodPost, "/"+url.PathEscape(string(held.Token))+"/retry", retry, printClaim) != 0 {
			fmt.Fprintf(c.stderr, "%s: %s was not retried\n", c.name(), held.Token)
			code = 1
		}
	}
	return code
}

// heldPoll is how often a command whose request the daemon holds reads the claims while it waits
// (awaitHeld): claimsSuspend's held suspension and claimsStop's held stop.
const heldPoll = 250 * time.Millisecond

// claimsSuspend suspends --claim. The daemon suspends a claim whose agent is in no turn at once
// (200), and holds the suspension of one in a turn (202; supervise's holdSuspension), which the
// command then waits out (awaitSuspended) inside its --wait. It prints the claim as it ends,
// suspended. A suspend of a worker whose start is still running is answered 200: the request
// stops that start before it waits for the claim's decision (supervise's Handle), and the start
// is charged nothing.
func claimsSuspend(c *claimsCall) action {
	token := c.flags.String("claim", "", "the claim's token (required)")
	return func(ctx context.Context, _ []string) int { return c.suspend(ctx, *token) }
}

func (c *claimsCall) suspend(ctx context.Context, token string) int {
	return c.postHeld(ctx, token, "suspend", func(op operator, _ string, _ []byte) ([]byte, bool) {
		return c.awaitSuspended(ctx, op, legionclaim.Token(token))
	})
}

// postHeld posts request ("suspend", "stop") to the claim token names: the frame suspend and stop
// share. A request the daemon holds (202) is waited out by await, which is handed the daemon's
// answer and the path it served it at, and answers the claim the wait ended with, or false, which
// fails the command. The command prints the claim as it ends: as the daemon answered it, or as the
// list served it.
func (c *claimsCall) postHeld(ctx context.Context, token, request string, await func(op operator, path string, answer []byte) ([]byte, bool)) int {
	if code, ok := c.check("claim"); !ok {
		return code
	}
	op, ok := c.connect()
	if !ok {
		return 1
	}
	path := claimsRoute + "/" + url.PathEscape(token) + "/" + request
	status, answer, ok := c.request(ctx, op, http.MethodPost, path, nil)
	if !ok {
		return 1
	}
	if status == http.StatusAccepted {
		if answer, ok = await(op, path, answer); !ok {
			return 1
		}
		path = claimsRoute
	}
	return c.printAnswer(op, path, answer, c.printer(printClaim))
}

// awaitHeld reads the claims every heldPoll until judge says the claim token names is settled, and
// answers that claim as the list served it, as one line: the wait awaitSuspended and awaitStopped
// share. judge reads each answer about the claim and says it is settled; or, for any other end that
// stops the wait, what happened; or neither, and the wait goes on. Every other way out stops the
// wait too, said on stderr and answering false: a list that cannot be read; the claim gone from it,
// said to be not unended (the end the wait is for); the command's --wait running out while the
// claim's request is still held, said with outlasted (what is held, and why it can outlast the
// bound); or ctx ending.
func (c *claimsCall) awaitHeld(ctx context.Context, op operator, token legionclaim.Token, unended, outlasted string,
	judge func(seen *api.OperatorClaim) (settled bool, stopped string)) ([]byte, bool) {
	for {
		_, answer, ok := c.request(ctx, op, http.MethodGet, claimsRoute, nil)
		if !ok {
			return nil, false
		}
		var list api.OperatorClaims
		if err := decodeAnswer(answer, &list); err != nil {
			fmt.Fprintf(c.stderr, "%s: read the answer %s%s served: %v\n", c.name(), op.base, claimsRoute, err)
			return nil, false
		}
		i := slices.IndexFunc(list.Claims, func(claim api.OperatorClaim) bool { return claim.Token == token })
		if i < 0 {
			fmt.Fprintf(c.stderr, "%s: the daemon no longer holds %s, which was not %s\n", c.name(), token, unended)
			return nil, false
		}
		seen := list.Claims[i]
		settled, stopped := judge(&seen)
		if settled {
			ended, err := json.Marshal(seen)
			if err != nil {
				fmt.Fprintf(c.stderr, "%s: %v\n", c.name(), err)
				return nil, false
			}
			// One line, as the daemon's own answers are.
			return append(ended, '\n'), true
		}
		if stopped != "" {
			fmt.Fprintf(c.stderr, "%s: %s\n", c.name(), stopped)
			return nil, false
		}
		if !c.until.IsZero() && !time.Now().Add(heldPoll).Before(c.until) {
			fmt.Fprintf(c.stderr, "%s: %s is still %s after the --wait bound of %s, its %s\n", c.name(), token, seen.State, c.waited, outlasted)
			return nil, false
		}
		select {
		case <-ctx.Done():
			fmt.Fprintf(c.stderr, "%s: %v\n", c.name(), ctx.Err())
			return nil, false
		case <-time.After(heldPoll):
		}
	}
}

// awaitSuspended reads the claims until token is suspended (awaitHeld), and answers the claim as
// the list served it. Every other end stops the wait, reported as what it is and answering false:
// the claim failed, retired, or left the daemon; the daemon holds its suspension no longer (a start
// run against the claim dropped it, or the daemon restarted); or the command's --wait ran out while
// it is still held. That bound is the call's own, fixed when check read the call and shared with
// every request it makes (operatorCall.bound), so the suspend request and this wait together take
// no longer than the operator asked for.
func (c *claimsCall) awaitSuspended(ctx context.Context, op operator, token legionclaim.Token) ([]byte, bool) {
	return c.awaitHeld(ctx, op, token, "suspended",
		"suspension held: its agent's turn has not ended, or the runtime refused the stop, which is tried again every probe interval",
		func(seen *api.OperatorClaim) (bool, string) {
			switch supervise.ClaimState(seen.State) {
			case supervise.StateSuspended:
				return true, ""
			case supervise.StateFailed, supervise.StateRetired:
				return false, fmt.Sprintf("%s is %s, so it will not be suspended", token, seen.State)
			}
			if !seen.SuspensionHeld {
				return false, fmt.Sprintf("the daemon no longer holds the suspension of %s, which is %s: a start run against "+
					"the claim dropped it, or the daemon restarted", token, seen.State)
			}
			return false, ""
		})
}

// claimsStop stops --claim. The daemon stops a claim at once (200), except one whose take's turn
// has ended with the report of that turn still to come: it holds that stop for the report (202;
// supervise's holdStop) and stops the claim at the report's finish, or at the wait's bound once it
// has taken the task back as a cut turn. The command then says the stop is held, naming the task
// and the bound (stopHeldLine), on stderr, since stdout carries the claim the command ends with and
// --json keeps that one line; and it waits for the stop (awaitStopped) inside its --wait, as
// claimsSuspend waits out a held suspension. It prints the claim as it ends, retired.
func claimsStop(c *claimsCall) action {
	token := c.flags.String("claim", "", "the claim's token (required)")
	return func(ctx context.Context, _ []string) int { return c.stop(ctx, *token) }
}

func (c *claimsCall) stop(ctx context.Context, token string) int {
	return c.postHeld(ctx, token, "stop", func(op operator, path string, answer []byte) ([]byte, bool) {
		var held api.OperatorClaim
		if err := decodeAnswer(answer, &held); err != nil {
			fmt.Fprintf(c.stderr, "%s: read the answer %s%s served: %v\n", c.name(), op.base, path, err)
			return nil, false
		}
		fmt.Fprintf(c.stderr, "%s: %s\n", c.name(), stopHeldLine(held))
		return c.awaitStopped(ctx, op, held)
	})
}

// stopHeldLine is what the command says of a stop the daemon holds for the report of the claim's
// take's turn: the task, by the id of the delivery the claim's view names (the view carries no
// number), and the bound, by name, since the view carries no timer — the report wait's own while
// the worker's connection shows the daemon every frame of its turns, and the reconnect deadline
// while it does not. The report wait's bound runs from the turn's end, and is armed whole later
// where the daemon restarted or the connection was cut since: at the reconnect, or at the stop
// itself over a take a daemon start restored that the bound reaches only with work held
// (supervise's reportBounded, armReportWait); the daemon's line of the hold names when it comes.
func stopHeldLine(held api.OperatorClaim) string {
	task := "its task"
	if held.Pending != nil {
		task += " " + held.Pending.ID
	}
	return fmt.Sprintf("the stop of %s is held for the report of the turn of %s, which has ended: the daemon stops the claim once that report comes, "+
		"or, with no report, takes the task back as a cut turn and stops it at the report wait's bound (worker_turn_start_timeout_seconds), "+
		"counted from that turn's end, or from the reconnect or the stop where the daemon restarted or the worker's connection was cut since that turn ended, "+
		"or, while the worker's connection is cut, at the reconnect deadline; the daemon's log line of the hold names when it comes (due); waiting for the stop",
		held.Token, task)
}

// awaitStopped reads the claims until the claim held names is retired (awaitHeld), and answers it
// as the list served it. Every other end stops the wait, reported as what it is and answering
// false: the claim left the daemon; it failed or was suspended — a held suspension's deadline
// stopped its process first — or the process the stop waited on was let go with the claim still
// live, a death or a relaunch, either of which drops the held stop with that process (supervise's
// letGo); or the command's --wait ran out while the stop is still held. The claim's view says
// nothing of a held stop, so a daemon restart, which forgets one, and a release the runtime refused
// at the report, which drops it, are read only at that bound, which names them. The bound is the
// call's own, shared with the stop request (operatorCall.bound), as awaitSuspended's is.
func (c *claimsCall) awaitStopped(ctx context.Context, op operator, held api.OperatorClaim) ([]byte, bool) {
	return c.awaitHeld(ctx, op, held.Token, "stopped",
		"stop held for the report of its take's turn: that report has not come and its bound has not passed, the runtime refused the release, "+
			"which the daemon logs, or the daemon restarted, which forgets a held stop; read legion claims list, and run legion claims stop again if it is not retired",
		func(seen *api.OperatorClaim) (bool, string) {
			switch supervise.ClaimState(seen.State) {
			case supervise.StateRetired:
				return true, ""
			case supervise.StateFailed, supervise.StateSuspended:
				return false, fmt.Sprintf("%s is %s, so the held stop was dropped and it is not stopped; run legion claims stop again to stop it", held.Token, seen.State)
			}
			if held.Locator != nil && (seen.Locator == nil || seen.Locator.Incarnation != held.Locator.Incarnation) {
				return false, fmt.Sprintf("the daemon no longer holds the stop of %s, which is %s: the process the stop waited on was let go before the "+
					"report came, and the held stop with it; run legion claims stop again to stop it", held.Token, seen.State)
			}
			return false, ""
		})
}

// claimsList prints every claim the daemon supervises, one line each, in token order.
func claimsList(c *claimsCall) action {
	return func(ctx context.Context, _ []string) int {
		if code, ok := c.check(); !ok {
			return code
		}
		op, ok := c.connect()
		if !ok {
			return 1
		}
		return c.send(ctx, op, http.MethodGet, "", nil, printClaims)
	}
}
