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
	"github.com/sjawhar/legion/daemon/internal/redact"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// newClaimsCommand is the operator's hand on the daemon's claims, one command per operator route
// (internal/api/operator.go). retry relaunches a failed claim: an architect on its session with
// fresh budgets, a phase worker through its issue's held phase; with --failed, every failed claim.
// compact shortens a running claim's session and sends it whatever task it owes again, which is
// what a claim whose provider keeps refusing its session reaches for before a fresh one. close is
// the tree's close, through its root claim, for a tree no workflow issue backs: the root, then
// every other claim of the tree.
func newClaimsCommand(stdout, stderr io.Writer) *cobra.Command {
	return group(&cobra.Command{Use: "claims", Short: "the operator's hand on the daemon's claims"},
		claimsCommand("spawn", "launch the claim that works a role's step of an issue, creating it where none is stored", claimsSpawn, stdout, stderr),
		claimsCommand("deliver", "give a claim a task", claimsDeliver, stdout, stderr),
		claimsCommand("suspend", "suspend a claim, once the turn its agent is in ends", claimsSuspend, stdout, stderr),
		claimsCommand("resume", "resume a suspended claim", claimRequest("resume"), stdout, stderr),
		claimsCommand("stop", "stop a claim's agent", claimRequest("stop"), stdout, stderr),
		claimsCommand("retry", "retry a failed claim, or every failed claim; a claim with no process whose launch waits its paced turn launches now", claimsRetry, stdout, stderr),
		claimsCommand("compact", "compact a claim's Oh My Pi session and send it the task it owes again", claimsCompact, stdout, stderr),
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

// writeClaimLine is the one line a claim prints as: token, role, issue, state, why it failed or
// that a refused launch's fact is not recorded (claimCauseWord), generation, the plugin contract
// its registration declared (0 when none is recorded), the update round the claim is in with the
// head its worker reported when it
// answered (`-` for a claim in no round, and `-` for a head no worker has reported yet), the
// call its record holds open (openCallWord), and last the sentence its latest launch try was
// refused with because the shared public leak check could not be installed in its pod, quoted so
// the line stays one line per claim (`-` for a claim no leak check refused). This listing is
// where the controller's cause is read, since its claim is on no claim view of the state. A
// claim whose idle pod the daemon released prints `suspended:released` for its state
// (claimStateWord): it is not a suspension anyone is waiting to lift — its next delivery resumes
// its agent in a pod of its own — so an operator reading the list must not read it as one. A
// claim whose agent the project's controller suspended prints `suspended:controller`, so the
// operator tells it from a suspension of its own.
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

// claimStateWord is the claim's state as the line prints it: `suspended:released` for a claim whose
// agent the idle release suspended, `suspended:controller` for one whose agent the project's
// controller suspended through its own tool, a suspension its `resume_agent` lifts as well as the
// operator's, and the state itself for every other claim, whose suspendedBy the operator reads
// with --json. A plain `suspended` is therefore the operator's own suspension, or the daemon's.
func claimStateWord(c api.OperatorClaim) string {
	if c.State != string(supervise.StateSuspended) {
		return c.State
	}
	switch supervise.SuspendSource(c.SuspendedBy) {
	case supervise.SuspendedByReleased, supervise.SuspendedByController:
		return c.State + ":" + c.SuspendedBy
	}
	return c.State
}

// claimCauseWord is why the claim failed as the line prints it: the one word the site that
// failed it chose (supervise.FailureCause), which the line carries beside the state rather than
// the sentence written with it, because one word is what fits a column and what an operator
// sorts a listing by; the sentence, the moment and the counts are read with --json
// (api.ClaimFailureView). A claim with no failure record that carries a refused launch's fact the
// store has not taken (api.OperatorClaim.UnrecordedRefusal) prints `refusal-unrecorded`: the
// daemon tries each such fact again, at least once an hour, for as long as it fails and the refusal
// it names stands; until one is taken the claim's start is not queued, and where the failing fact
// is the refusal's report, the issue and the controller are told nothing; when the oldest such
// fact's first try failed, how many of its tries have failed and its last error are read with
// --json. It is empty for every other claim the daemon holds no failure record for — one that
// never failed, one that has been given another run since, which clears the record, and one
// holding a cause with no sentence (api.FailureViewOf) — which the line prints as `-`.
func claimCauseWord(c api.OperatorClaim) string {
	if c.Failure != nil {
		return string(c.Failure.Cause)
	}
	if c.UnrecordedRefusal != nil {
		return refusalUnrecordedWord
	}
	return ""
}

// refusalUnrecordedWord is the cause word of a claim whose refused launch's fact the store has not
// taken (claimCauseWord).
const refusalUnrecordedWord = "refusal-unrecorded"

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

// claimsSpawn launches the claim that works --role's step of --issue now, in the tree --tree
// roots: the claim of the agent the step names, a keyed agent on a step of another role included,
// or the role's own where no step names one, created where none is stored.
// --prompt-file is an optional operator override; omitted, the daemon composes the role's shared
// and Go-specific parts, or, for a role of the deployment, its prompt_file with the headless part
// and worker-common.md. --task is its first delivery, sent once the agent is ready.
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

// claimsCompact compacts --claim's Oh My Pi session with a handoff and has the daemon send the
// claim whatever task it owes again: what a claim whose provider keeps refusing its session
// reaches for before a fresh session, since the session, its workspace and everything the worker
// knows of its issue stay where they are (api's compact).
//
// The request runs under the --wait every `legion claims` command takes (newClaimsCall), one
// minute by default, while the handoff is a model call over the whole transcript that the daemon
// bounds at ten minutes (runtime.CompactTimeout): a call the --wait ends is refused and the
// handoff goes on, so an operator gives it a longer --wait to read its answer. The daemon's
// answer is printed whether or not the session was shortened — a handoff Oh My Pi refused is
// reported with Oh My Pi's own words for it, and one that made no document with the daemon's —
// and whatever task the claim owed has gone again in either case.
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
// order the daemon lists them: an architect's agent is launched again with fresh budgets, a phase
// worker's is launched again by the start of its issue's phase — the phase it is held from, or
// the one it stands in, which is where an operator's stop leaves it — and the daemon refuses the
// rest, naming why (api's retryClaim). Under --failed each retried claim prints its line as
// --claim prints it, each refusal is reported naming its claim, and the command fails once any
// was refused, having asked for every one. A claim an operator stopped is retired rather than
// failed, so --failed passes over it: name it with --claim.
//
// --fresh-session starts the claim's agent in a new session rather than resuming the recorded
// one. Every other retry resumes the recorded session with fresh budgets, whatever its run of
// refused turns (supervise's freshSession): a new session is the operator's or the controller's
// choice, for an agent wedged in a way no budget counts, or a session its provider keeps refusing
// when a new one would not be. The one exception is a claim held for OpenAI's cyber-policy
// refusal, whose agent every retry starts in a new session, with the flag or without it and under
// --failed as under --claim, since its recorded session would send the refused content to OpenAI
// again.
func claimsRetry(c *claimsCall) action {
	token := c.flags.String("claim", "", "the claim's token (this, or --failed)")
	failed := c.flags.Bool("failed", false, "retry every claim the daemon holds failed")
	fresh := c.flags.Bool("fresh-session", false, "start the claim's agent in a new session rather than resuming the recorded one")
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
		fmt.Fprintf(c.stderr, "%s: read the answer %s served: %v\n", c.name(), redact.URL(op.base+claimsRoute), err)
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

// suspendPoll is how often claimsSuspend reads the claims while a suspension is held.
const suspendPoll = 250 * time.Millisecond

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
	if code, ok := c.check("claim"); !ok {
		return code
	}
	op, ok := c.connect()
	if !ok {
		return 1
	}
	path := claimsRoute + "/" + url.PathEscape(token) + "/suspend"
	status, answer, ok := c.request(ctx, op, http.MethodPost, path, nil)
	if !ok {
		return 1
	}
	if status == http.StatusAccepted {
		if answer, ok = c.awaitSuspended(ctx, op, legionclaim.Token(token)); !ok {
			return 1
		}
		path = claimsRoute
	}
	return c.printAnswer(op, path, answer, c.printer(printClaim))
}

// awaitSuspended reads the claims until token is suspended, and answers the claim as the list
// served it. Every other end stops the wait, reported as what it is and answering false: the claim
// failed, retired, or left the daemon; the daemon holds its suspension no longer (a start run
// against the claim dropped it, or the daemon restarted); or the command's --wait ran out while it
// is still held. That bound is the call's own, fixed when check read the call and shared with
// every request it makes (operatorCall.bound), so the suspend request and this wait together take
// no longer than the operator asked for.
func (c *claimsCall) awaitSuspended(ctx context.Context, op operator, token legionclaim.Token) ([]byte, bool) {
	for {
		_, answer, ok := c.request(ctx, op, http.MethodGet, claimsRoute, nil)
		if !ok {
			return nil, false
		}
		var list api.OperatorClaims
		if err := decodeAnswer(answer, &list); err != nil {
			fmt.Fprintf(c.stderr, "%s: read the answer %s served: %v\n", c.name(), redact.URL(op.base+claimsRoute), err)
			return nil, false
		}
		i := slices.IndexFunc(list.Claims, func(claim api.OperatorClaim) bool { return claim.Token == token })
		if i < 0 {
			fmt.Fprintf(c.stderr, "%s: the daemon no longer holds %s, which was not suspended\n", c.name(), token)
			return nil, false
		}
		seen := list.Claims[i]
		switch supervise.ClaimState(seen.State) {
		case supervise.StateSuspended:
			suspended, err := json.Marshal(seen)
			if err != nil {
				fmt.Fprintf(c.stderr, "%s: %v\n", c.name(), err)
				return nil, false
			}
			// One line, as the daemon's own answers are.
			return append(suspended, '\n'), true
		case supervise.StateFailed, supervise.StateRetired:
			fmt.Fprintf(c.stderr, "%s: %s is %s, so it will not be suspended\n", c.name(), token, seen.State)
			return nil, false
		}
		if !seen.SuspensionHeld {
			fmt.Fprintf(c.stderr, "%s: the daemon no longer holds the suspension of %s, which is %s: a start run against "+
				"the claim dropped it, or the daemon restarted\n", c.name(), token, seen.State)
			return nil, false
		}
		if !c.until.IsZero() && !time.Now().Add(suspendPoll).Before(c.until) {
			fmt.Fprintf(c.stderr, "%s: %s is still %s after the --wait bound of %s, its suspension held: its agent's turn has not ended, or the "+
				"runtime refused the stop, which is tried again every probe interval\n", c.name(), token, seen.State, c.waited)
			return nil, false
		}
		select {
		case <-ctx.Done():
			fmt.Fprintf(c.stderr, "%s: %v\n", c.name(), ctx.Err())
			return nil, false
		case <-time.After(suspendPoll):
		}
	}
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
