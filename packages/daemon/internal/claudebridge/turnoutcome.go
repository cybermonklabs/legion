package claudebridge

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// reportTimeout bounds one report's call. A turn's outcome is a few hundred bytes to a daemon on
// the cluster network, and the report is made from a goroutine of its own, so the bound is there
// for a daemon that accepts the connection and never answers rather than for a slow one.
const reportTimeout = 15 * time.Second

// errReportTimeout is the cause a report's context ends with when reportTimeout passes on the
// bridge's clock, so the pane's line says the bound rather than the cancel that carried it.
var errReportTimeout = fmt.Errorf("the daemon gave no answer within the report's bound of %s", reportTimeout)

// turnReports is the bridge as the Claude Code backend's producer of turn outcomes: every turn the
// bridge ends is reported to the daemon, clean or refused, with the bridge's own sequence within
// its launch generation (POST /legion/v1/claims/turn-outcome). The daemon derives the claim's run
// of refused turns from those reports and answers each report with it; the run fails no claim
// (dispatch://INFRA-4508) and decides nothing. A turn that ran no prompt of the daemon's — one an
// inbox delivery started, the phase hook's reminder among them, or one Claude Code started itself
// when a background task finished — is judged by the daemon from its report alone, since no
// prompt result comes for it (supervise's turnOutcome), so its report says so and carries the
// status the provider refused it with.
//
// The bridge is the producer because it is what sees the refusal: a step Claude Code ends with an
// error is the assistant message it writes with stopReason "error" (result), and the pane's own
// Claude Code hook for a stopped turn is not measured to fire in a pod
// (packages/claude-envoy/src/legion-phase-hook.ts). The bridge holds no registration secret — the
// claim's registration is claude-envoy's, inside Claude Code — so it names itself with the
// launch's boot token, the file its pane is told, which the daemon resolves to this claim.
//
// Each report is made from a goroutine of its own and nothing waits for it: the bridge's loop must
// stay answerable to the shim, to Claude Code and to its own end at every moment. A report the
// bridge's exit cuts short is a turn the daemon never hears of, which counts neither way, and the
// bridge is torn down only when its pane is ending.
type turnReports struct {
	url        string
	bootToken  string
	generation uint64
	client     *http.Client
	// clock is the bridge's (Config.Clock), which the report's bound runs on.
	clock Clock
	log   *log.Logger
	// sequence is the bridge's own count of the turns it has ended at generation, from 1. It is
	// atomic because the posts run off the loop, though only the loop ever takes a number.
	sequence atomic.Uint64
}

// startTurnReports is the reporter for cfg, or nil when the environment the bridge was given names
// no daemon, no boot token file or no generation: a bridge run by hand reports to nobody. A boot
// token file it cannot read is the same, said once, since nothing it could report would
// authenticate.
func startTurnReports(cfg Config, logger *log.Logger) *turnReports {
	if cfg.DaemonURL == "" || cfg.BootTokenFile == "" || cfg.Generation == 0 {
		logger.Printf("reporting no turn outcomes: this pane names %s", missingReportInputs(cfg))
		return nil
	}
	token, err := os.ReadFile(cfg.BootTokenFile)
	if err != nil {
		logger.Printf("reporting no turn outcomes: the boot token the daemon would recognise this claim by could not be read: %v", err)
		return nil
	}
	return &turnReports{
		url:        strings.TrimSuffix(cfg.DaemonURL, "/") + "/legion/v1/claims/turn-outcome",
		bootToken:  strings.TrimSpace(string(token)),
		generation: cfg.Generation,
		client:     &http.Client{},
		clock:      cfg.clock(),
		log:        logger,
	}
}

// missingReportInputs names what a pane that reports nothing is missing, so the one log line says
// which value to look for rather than that something is absent.
func missingReportInputs(cfg Config) string {
	var missing []string
	if cfg.DaemonURL == "" {
		missing = append(missing, "no LEGION_DAEMON_URL")
	}
	if cfg.BootTokenFile == "" {
		missing = append(missing, "no LEGION_BOOT_TOKEN_FILE")
	}
	if cfg.Generation == 0 {
		missing = append(missing, "no LEGION_GENERATION")
	}
	return strings.Join(missing, ", ")
}

// turnEnded reports the turn the bridge has just ended: refused, with what the provider said and
// the HTTP status it answered (0 where Claude Code named none), when a step of it failed, and clean
// otherwise. prompted is whether the turn ran the daemon's prompt, whose prompt_result then judges
// it: the report of a turn that ran none is marked unprompted, which is what has the daemon judge
// it. It takes the next sequence and returns at once.
func (r *turnReports) turnEnded(refusal string, status int, prompted bool) {
	if r == nil {
		return
	}
	request := claim.TurnOutcomeRequest{
		BootToken:  r.bootToken,
		Generation: r.generation,
		Sequence:   r.sequence.Add(1),
		Outcome:    claim.TurnClean,
		Unprompted: !prompted,
	}
	if refusal != "" {
		request.Outcome, request.Message = claim.TurnRefused, refusal
		// A number outside the HTTP range is no status the provider answered, and the daemon
		// takes such a number as no status, with a warning that names it, so the bridge sends the
		// refusal as one whose status cannot be read and leaves the daemon nothing to warn about.
		if status >= 100 && status <= 599 {
			request.HTTPStatus = status
		}
	}
	go r.post(request)
}

// post makes one report and logs what the daemon made of it: the run of refused turns the claim's
// reports now end with, which is the one place the pane shows the count being made. A report the
// daemon refuses is logged and nothing else: the turn is over, and the bridge has nothing to do
// about it.
func (r *turnReports) post(request claim.TurnOutcomeRequest) {
	body, err := json.Marshal(request)
	if err != nil {
		r.log.Printf("could not encode the outcome of turn %d: %v", request.Sequence, err)
		return
	}
	// The call's bound, reportTimeout, runs on the bridge's clock; ending the request's context
	// ends its dial, its wait for an answer and its read of the body alike, and the cause it ends
	// with, errReportTimeout, is what the line says.
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	go func() {
		select {
		case <-r.clock.After(reportTimeout):
			cancel(errReportTimeout)
		case <-ctx.Done():
		}
	}()
	post, err := http.NewRequestWithContext(ctx, http.MethodPost, r.url, bytes.NewReader(body))
	if err != nil {
		r.log.Printf("could not report the outcome of turn %d: %v", request.Sequence, err)
		return
	}
	post.Header.Set("Content-Type", "application/json")
	response, err := r.client.Do(post)
	if err != nil {
		if cause := context.Cause(ctx); cause != nil {
			err = fmt.Errorf("%w (%v)", cause, err)
		}
		r.log.Printf("could not report the outcome of turn %d (%s): %v", request.Sequence, request.Outcome, err)
		return
	}
	defer func() { _ = response.Body.Close() }()
	answer, decoded := decodeTurnOutcome(ctx, response)
	switch {
	case response.StatusCode/100 != 2:
		r.log.Printf("the daemon refused the outcome of turn %d (%s): %s", request.Sequence, request.Outcome, answer)
	case decoded.Failed:
		// claim.TurnOutcomeResponse says which report sets Failed.
		r.log.Printf("the daemon failed this claim: turn %d ran no prompt of the daemon's and the provider refused it with an error that cannot clear (%s)", request.Sequence, request.Message)
	case decoded.RefusedRun > 0:
		r.log.Printf("reported turn %d as %s; the daemon counts %d refused turns in a row", request.Sequence, request.Outcome, decoded.RefusedRun)
	}
}

// decodeTurnOutcome is what the daemon answered: the outcome it derived, and the text to log for
// an answer that is not one — a refusal's sentence, or the status alone when the body says
// nothing.
func decodeTurnOutcome(ctx context.Context, response *http.Response) (string, claim.TurnOutcomeResponse) {
	var decoded claim.TurnOutcomeResponse
	body, err := readAnswer(response)
	if err != nil {
		if cause := context.Cause(ctx); cause != nil {
			err = fmt.Errorf("%w (%v)", cause, err)
		}
		return fmt.Sprintf("%s (its answer could not be read: %v)", response.Status, err), decoded
	}
	if json.Unmarshal(body, &decoded) != nil || len(body) == 0 {
		return fmt.Sprintf("%s %s", response.Status, strings.TrimSpace(string(body))), decoded
	}
	return response.Status, decoded
}

// readAnswer reads at most maxAnswerBytes of the daemon's answer: a refusal is a sentence, and
// nothing the bridge logs needs more.
func readAnswer(response *http.Response) ([]byte, error) {
	var buffer bytes.Buffer
	_, err := buffer.ReadFrom(http.MaxBytesReader(nil, response.Body, maxAnswerBytes))
	return buffer.Bytes(), err
}

// maxAnswerBytes bounds what the bridge reads of the daemon's answer.
const maxAnswerBytes = 8 << 10
