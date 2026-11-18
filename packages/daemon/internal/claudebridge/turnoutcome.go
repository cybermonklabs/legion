package claudebridge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
)

// A report waits for a daemon that is not serving as every call an Oh My Pi worker may send again
// waits for it (pi-shared's retryWhileDaemonRestarts, R1): it is sent again, with a backoff from
// firstPause to longestPause, for up to config.StallDaemonWait (DAEMON_RESTART_BUDGET_MS), and
// one post the daemon takes and does not answer within attemptLimit (DAEMON_ATTEMPT_LIMIT_MS) is
// ended and made again, since a daemon that takes a connection and never answers it is not
// serving either. The daemon keeps one outcome per generation and sequence, so a report sent
// again writes the cell its first post wrote and changes no count.
const (
	attemptLimit = 5 * time.Minute
	firstPause   = 500 * time.Millisecond
	longestPause = 30 * time.Second
)

// waitClock is what a report's wait for the daemon runs on: the clock that ages the wait, the
// pause between posts, and one post's own limit. realWait is the bridge's; a test has its own.
type waitClock struct {
	now          func() time.Time
	sleep        func(time.Duration)
	attemptLimit time.Duration
}

var realWait = waitClock{now: time.Now, sleep: time.Sleep, attemptLimit: attemptLimit}

// turnReports is the bridge as the Claude Code backend's producer of turn outcomes: every turn the
// bridge ends is reported to the daemon, clean or refused, with the bridge's own sequence within
// its launch generation (POST /legion/v1/claims/turn-outcome). The daemon derives the claim's run
// of refused turns from those reports and fails the claim once the run reaches its launch failure
// limit, which is the fault that left an implementer's every turn refused by its provider with its
// claim never failing (dispatch://INFRA-714).
//
// The bridge is the producer because it is what sees the refusal: a step Claude Code ends with an
// error is the assistant message it writes with stopReason "error" (result), and the pane's own
// Claude Code hook for a stopped turn is not measured to fire in a pod
// (packages/claude-envoy/src/legion-phase-hook.ts). The bridge holds no registration secret — the
// claim's registration is claude-envoy's, inside Claude Code — so it names itself with the
// launch's boot token, the file its pane is told, which the daemon resolves to this claim.
//
// Each report is made from a goroutine of its own and nothing waits for it: the bridge's loop must
// stay answerable to the shim, to Claude Code and to its own end at every moment. So a report's
// wait for a daemon that is not serving, an hour at most, holds nothing of the bridge's and
// charges the claim nothing. A report the bridge's exit cuts short is a turn the daemon never
// hears of, which counts neither way, and the bridge is torn down only when its pane is ending.
type turnReports struct {
	url        string
	bootToken  string
	generation uint64
	client     *http.Client
	log        *log.Logger
	wait       waitClock
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
		log:        logger,
		wait:       realWait,
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

// turnEnded reports the turn the bridge has just ended: refused, with what the provider said, when
// a step of it failed, and clean otherwise. It takes the next sequence and returns at once.
func (r *turnReports) turnEnded(refusal string) {
	if r == nil {
		return
	}
	request := claim.TurnOutcomeRequest{
		BootToken:  r.bootToken,
		Generation: r.generation,
		Sequence:   r.sequence.Add(1),
		Outcome:    claim.TurnClean,
	}
	if refusal != "" {
		request.Outcome, request.Message = claim.TurnRefused, refusal
	}
	go r.post(request)
}

// post makes one report, waiting out a daemon that is not serving (deliver), and logs what the
// daemon made of it: the run of refused turns the claim's reports now end with, and whether this
// one failed the claim, which is the one place the pane shows the count being made. A report the
// daemon refuses, or one that found it not serving for the whole wait, is logged and nothing
// else: the turn is over, and the bridge has nothing more to do about it. Its turn is then a gap
// in the sequence, which ends the run the daemon counts there (supervise's RefusedRun). Each
// report waits on its own, so through an outage they may land in any order.
func (r *turnReports) post(request claim.TurnOutcomeRequest) {
	body, err := json.Marshal(request)
	if err != nil {
		r.log.Printf("could not encode the outcome of turn %d: %v", request.Sequence, err)
		return
	}
	answer, err := r.deliver(request, body)
	if err != nil {
		r.log.Printf("could not report the outcome of turn %d (%s): %v", request.Sequence, request.Outcome, err)
		return
	}
	said, decoded := decodeTurnOutcome(answer)
	switch {
	case answer.code/100 != 2:
		r.log.Printf("the daemon refused the outcome of turn %d (%s): %s", request.Sequence, request.Outcome, said)
	case decoded.Failed:
		r.log.Printf("the daemon failed this claim: its provider refused %d turns in a row", decoded.RefusedRun)
	case decoded.RefusedRun > 0:
		r.log.Printf("reported turn %d as %s; the daemon counts %d refused turns in a row", request.Sequence, request.Outcome, decoded.RefusedRun)
	}
}

// deliver posts body until the daemon answers it, and returns that answer: while the daemon is not
// serving (notServing) it posts again after a pause, from firstPause doubling to longestPause, for
// up to config.StallDaemonWait from its first post, and says once, when it begins to wait, that it
// waits. The error is a post that failed for any other reason, which is not sent again, or the
// wait ending with the daemon still not serving.
func (r *turnReports) deliver(request claim.TurnOutcomeRequest, body []byte) (daemonAnswer, error) {
	deadline := r.wait.now().Add(config.StallDaemonWait)
	pause := firstPause
	for waiting := false; ; waiting = true {
		answer, err := r.attempt(body)
		if !notServing(answer, err) {
			return answer, err
		}
		why := answer.status
		if err != nil {
			why = err.Error()
		}
		remaining := deadline.Sub(r.wait.now())
		if remaining <= 0 {
			return daemonAnswer{}, fmt.Errorf("the Legion daemon was unreachable (%s) and stayed so for %s", why, config.StallDaemonWait)
		}
		if !waiting {
			r.log.Printf("the Legion daemon is unreachable, most likely restarting (%s); the outcome of turn %d (%s) is being sent again for up to %s",
				why, request.Sequence, request.Outcome, config.StallDaemonWait)
		}
		r.wait.sleep(min(pause, remaining))
		pause = min(2*pause, longestPause)
	}
}

// daemonAnswer is what the daemon answered one post: its status line and code, at most
// maxAnswerBytes of its body, and the error that cut the read of that body short, if one did.
type daemonAnswer struct {
	status string
	code   int
	body   []byte
	unread error
}

// attempt is one post of body, bounded by the wait's attemptLimit: the daemon's answer, or the
// error that ended the post before it. An attempt the limit ends has its connection closed, so the
// daemon never holds two posts of one report at once. A read of the answer that the daemon's end
// cut short is the post's error, as the connection's end before any answer is.
func (r *turnReports) attempt(body []byte) (daemonAnswer, error) {
	ctx, cancel := context.WithTimeout(context.Background(), r.wait.attemptLimit)
	defer cancel()
	post, err := http.NewRequestWithContext(ctx, http.MethodPost, r.url, bytes.NewReader(body))
	if err != nil {
		return daemonAnswer{}, err
	}
	post.Header.Set("Content-Type", "application/json")
	response, err := r.client.Do(post)
	if err != nil {
		return daemonAnswer{}, err
	}
	defer func() { _ = response.Body.Close() }()
	answer := daemonAnswer{status: response.Status, code: response.StatusCode}
	answer.body, answer.unread = readAnswer(response)
	if answer.unread != nil && notServing(daemonAnswer{}, answer.unread) {
		return daemonAnswer{}, answer.unread
	}
	return answer, nil
}

// notServing reports whether a post got no answer from the daemon, as pi-shared's
// daemonUnreachable reads it (R1): it never reached the daemon — the connection refused, the host
// not resolving or not reachable — or it may have and was cut short — the connection reset or
// closed before an answer, a timeout, the attempt's own limit — or a 502, 503 or 504 carrying no
// code of the daemon's came back, which a proxy in front of it answers. Any answer the daemon gave
// is not, a 4xx above all, an uncoded 500, and its own coded 500 (DISPATCH_UNAVAILABLE,
// FACTS_UNAVAILABLE) and 502 (DISPATCH_FAILED), each of which says itself what comes next; nor is
// an error that is no end of its serving.
func notServing(answer daemonAnswer, err error) bool {
	if err == nil {
		switch answer.code {
		case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
			return refusalCode(answer.body) == ""
		}
		return false
	}
	var unresolved *net.DNSError
	var timeout interface{ Timeout() bool }
	for _, end := range []error{syscall.ECONNREFUSED, syscall.EHOSTUNREACH, syscall.ENETUNREACH,
		syscall.ECONNRESET, syscall.EPIPE, syscall.ETIMEDOUT, io.EOF, io.ErrUnexpectedEOF} {
		if errors.Is(err, end) {
			return true
		}
	}
	return errors.As(err, &unresolved) || errors.As(err, &timeout) && timeout.Timeout()
}

// refusalCode is the code of the refusal body the daemon writes (api's Failure), "" for a body that
// carries none or is not that shape, as pi-shared reads a refusal (LegionErrorResponse: a nonempty
// error, and a code only when it is nonempty too).
func refusalCode(body []byte) string {
	var refusal struct {
		Code  string `json:"code"`
		Error string `json:"error"`
	}
	if json.Unmarshal(body, &refusal) != nil || refusal.Error == "" {
		return ""
	}
	return refusal.Code
}

// decodeTurnOutcome is what the daemon answered: the outcome it derived, and the text to log for
// an answer that is not one — a refusal's sentence, or the status alone when the body says
// nothing.
func decodeTurnOutcome(answer daemonAnswer) (string, claim.TurnOutcomeResponse) {
	var decoded claim.TurnOutcomeResponse
	if answer.unread != nil {
		return fmt.Sprintf("%s (its answer could not be read: %v)", answer.status, answer.unread), decoded
	}
	if json.Unmarshal(answer.body, &decoded) != nil || len(answer.body) == 0 {
		return fmt.Sprintf("%s %s", answer.status, strings.TrimSpace(string(answer.body))), decoded
	}
	return answer.status, decoded
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
