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
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/sjawhar/legion/daemon/internal/backoff"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
)

// The re-send of the seen report, the take and the turn-outcome report (dispatch://INFRA-2811),
// with the numbers Oh My Pi's daemon client uses (packages/pi-shared/src/daemon-client.ts:
// DAEMON_ANSWER_TIMEOUT_MS, DAEMON_RESTART_FIRST_DELAY_MS, DAEMON_RESTART_MAX_DELAY_MS), so both
// harnesses wait out a daemon that is not serving alike.
const (
	// taskAnswerTimeout bounds one attempt: an attempt with no answer by then is sent again.
	taskAnswerTimeout = 5 * time.Minute
	// taskResendFirst is the first wait before an attempt with no answer is sent again; each wait
	// after it doubles, up to taskResendMax, and nothing limits the attempts.
	taskResendFirst = 500 * time.Millisecond
	taskResendMax   = 30 * time.Second
)

// maxTaskAnswerBytes bounds what the bridge reads of the daemon's answer to any request it sends.
// An accepted take carries the task's whole text, which the daemon's prompt frame used to carry
// within the worker stream's frame limit, so that limit bounds it here too; every other answer is
// a sentence or a few numbers.
const maxTaskAnswerBytes = shimwire.MaxFrameBytes

// daemonLink is the claim's daemon as the bridge speaks to it: the API's address, and the launch's
// boot token and generation, which name this claim in the boot form of every claim route the
// bridge calls (claim.BootCaller) — the turn-outcome report (turnoutcome.go), and the seen report
// and the take of the claim's task (take.go). The bridge holds no registration secret: the claim's
// registration is claude-envoy's, inside Claude Code, and the secret never leaves that process. So
// the bridge names itself with the launch's boot token, the file its pane is told, which the
// daemon resolves to this claim. All three are sent through one re-send policy (untilAnswered).
type daemonLink struct {
	// url is the API's address, with no trailing slash.
	url        string
	bootToken  string
	generation uint64
	log        *log.Logger
	// calls is the timing of every request the bridge sends (untilAnswered), and client what sends
	// them: no timeout of its own, since each attempt is bounded by calls.answerTimeout, and a
	// connection for each attempt (DisableKeepAlives), so a daemon that restarted between two
	// attempts is met as a refused connection rather than as a pooled one it closed.
	calls  daemonCalls
	client *http.Client
}

// daemonCalls is the timing and the network of the requests the bridge sends. Its zero value is
// production's: the waits below and the system's own dialer. A test shortens the waits and stands
// in for the network (Config.calls).
type daemonCalls struct {
	answerTimeout, resendFirst, resendMax time.Duration
	dial                                  func(ctx context.Context, network, address string) (net.Conn, error)
}

// dialDaemon is the daemon cfg names, or nil when the environment the bridge was given names no
// daemon, no boot token file or no generation: a bridge run by hand reports no turn and takes no
// task. A boot token file it cannot read is the same, said once, since nothing it could send would
// authenticate.
func dialDaemon(cfg Config, logger *log.Logger) *daemonLink {
	if cfg.DaemonURL == "" || cfg.BootTokenFile == "" || cfg.Generation == 0 {
		logger.Printf("reporting no turn outcomes and taking no task: this pane names %s", missingDaemonInputs(cfg))
		return nil
	}
	token, err := os.ReadFile(cfg.BootTokenFile)
	if err != nil {
		logger.Printf("reporting no turn outcomes and taking no task: the boot token the daemon would recognise this claim by could not be read: %v", err)
		return nil
	}
	calls := cfg.calls
	if calls.answerTimeout == 0 {
		calls.answerTimeout = taskAnswerTimeout
	}
	if calls.resendFirst == 0 {
		calls.resendFirst = taskResendFirst
	}
	if calls.resendMax == 0 {
		calls.resendMax = taskResendMax
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DisableKeepAlives = true
	if calls.dial != nil {
		transport.DialContext = calls.dial
	}
	return &daemonLink{
		url:        strings.TrimSuffix(cfg.DaemonURL, "/"),
		bootToken:  strings.TrimSpace(string(token)),
		generation: cfg.Generation,
		log:        logger,
		calls:      calls,
		client:     &http.Client{Transport: transport},
	}
}

// missingDaemonInputs names what a pane that speaks to no daemon is missing, so the one log line
// says which value to look for rather than that something is absent.
func missingDaemonInputs(cfg Config) string {
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

// untilAnswered posts request to the route at path until the daemon answers it, and returns that
// answer: its status and its body. An attempt the daemon gave no answer to (unanswered), or one
// whose answer alsoAgain names, is sent again, the same request, after a wait that doubles from
// calls.resendFirst to calls.resendMax, with no attempt limit; each attempt is bounded by
// calls.answerTimeout. alsoAgain is nil for a route whose every answer is the daemon's word; for
// the turn-outcome report it says how an answer that is sent again was answered — a 500, the
// daemon's failure to record it (recordFailed) — and is "" for any other. The first attempt sent
// again is logged, once, naming what and why. ctx ending — the request replaced by a newer value,
// or the bridge ending — ends the sending with ctx's error. An error that came with a status
// `unanswered` does not name is returned as the request's answer, and nothing sends it again.
func (d *daemonLink) untilAnswered(ctx context.Context, what, path string, request any, alsoAgain func(status int) string) (int, []byte, error) {
	body, err := json.Marshal(request)
	if err != nil {
		return 0, nil, err
	}
	delay, told := d.calls.resendFirst, false
	for {
		status, answer, err := d.attempt(ctx, path, body)
		if ctx.Err() != nil {
			return 0, nil, ctx.Err()
		}
		why := d.unanswered(status, answer, err)
		switch {
		case why != "":
			why = "got no answer from the daemon (" + why + ")"
		case err == nil && alsoAgain != nil:
			why = alsoAgain(status)
		}
		if why == "" {
			return status, answer, err
		}
		if !told {
			told = true
			d.log.Printf("%s %s; it is sent again until it is answered", what, why)
		}
		if err := backoff.Sleep(ctx, delay); err != nil {
			return 0, nil, err
		}
		delay = min(delay*2, d.calls.resendMax)
	}
}

// attempt is one post of body to the route at path, bounded by calls.answerTimeout, with the
// daemon's answer read whole, up to maxTaskAnswerBytes. An attempt the bound ended fails with
// errNoAnswerInTime, whatever the transport made of it: a dial or a read the bound cut off reads
// to the transport as a timeout, as one that timed out on its own does, and the two are told
// apart by the bound's cause.
func (d *daemonLink) attempt(ctx context.Context, path string, body []byte) (status int, answer []byte, err error) {
	ctx, cancel := context.WithTimeoutCause(ctx, d.calls.answerTimeout, errNoAnswerInTime)
	defer cancel()
	defer func() {
		if err != nil && context.Cause(ctx) == errNoAnswerInTime {
			err = errNoAnswerInTime
		}
	}()
	post, err := http.NewRequestWithContext(ctx, http.MethodPost, d.url+path, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	post.Header.Set("Content-Type", "application/json")
	response, err := d.client.Do(post)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = response.Body.Close() }()
	answer, err = io.ReadAll(http.MaxBytesReader(nil, response.Body, maxTaskAnswerBytes))
	return response.StatusCode, answer, err
}

// errNoAnswerInTime is what an attempt the daemon did not answer within calls.answerTimeout
// fails with (attempt).
var errNoAnswerInTime = errors.New("no answer within the attempt's bound")

// unanswered names why an attempt got no answer from the daemon, and is "" for an attempt it
// answered. A refused, reset or closed connection, an unreachable host or network, a name that
// did not resolve, and a dial or a read that timed out — which any error in the chain may say
// (timedOut) — never reached the daemon or lost its answer on the way, before its status or
// after it; an attempt past calls.answerTimeout is a daemon that took the request and never
// answered; any other failure of an attempt that got no status at all is no answer either,
// whatever it names itself (a code no case here lists, a malformed answer of something in the
// daemon's place); and a 502, 503 or 504 that names no Legion error code is a proxy in front of a
// daemon that is not serving. No list of failures decides the attempt that got no status: one that
// never reached the daemon applied nothing, and one that did is applied once however often it
// arrives, which is what every request untilAnswered sends is. Every refusal the daemon itself
// makes names a code (api's taskRefusal), so a coded answer, whatever its status, is the daemon's
// word and is never sent again.
func (d *daemonLink) unanswered(status int, body []byte, err error) string {
	var dns *net.DNSError
	switch {
	case err == nil:
	case errors.Is(err, errNoAnswerInTime):
		return fmt.Sprintf("no answer within %v", d.calls.answerTimeout)
	case errors.Is(err, syscall.ECONNREFUSED):
		return "the connection was refused"
	case errors.Is(err, syscall.ECONNRESET), errors.Is(err, syscall.EPIPE):
		return "the connection was reset"
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return "the connection was closed"
	case errors.Is(err, syscall.EHOSTUNREACH):
		return "the daemon's host was unreachable"
	case errors.Is(err, syscall.ENETUNREACH):
		return "the daemon's network was unreachable"
	case errors.As(err, &dns):
		return "the daemon's name did not resolve"
	case timedOut(err):
		return "the connection timed out"
	case status == 0:
		return fmt.Sprintf("the transport failed: %v", err)
	default:
		return ""
	}
	switch status {
	case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		if !coded(body) {
			return fmt.Sprintf("%d with no Legion error code", status)
		}
	}
	return ""
}

// timedOut is whether any error in err's chain, through both forms of Unwrap, says it timed out
// (a Timeout method that answers true). net/http gives one read that timed out in either of two
// shapes — the read's error directly under the client's *url.Error, or wrapped first by its read
// loop — and *url.Error's own Timeout asks only the error it directly wraps, so only the whole
// chain finds the timeout in both.
func timedOut(err error) bool {
	for err != nil {
		if t, ok := err.(interface{ Timeout() bool }); ok && t.Timeout() {
			return true
		}
		switch wrapper := err.(type) {
		case interface{ Unwrap() error }:
			err = wrapper.Unwrap()
		case interface{ Unwrap() []error }:
			return slices.ContainsFunc(wrapper.Unwrap(), timedOut)
		default:
			return false
		}
	}
	return false
}

// coded is whether body is a refusal the daemon made: one JSON object naming a Legion error code
// (api's Failure).
func coded(body []byte) bool {
	var refusal struct {
		Code string `json:"code"`
	}
	return json.Unmarshal(body, &refusal) == nil && refusal.Code != ""
}

// refusalText is what the bridge logs of an answer that is not the one a route answers on
// success: the status, and the daemon's sentence where the body is one.
func refusalText(status int, body []byte) string {
	var refusal struct {
		Code  string `json:"code"`
		Error string `json:"error"`
	}
	if json.Unmarshal(body, &refusal) == nil && refusal.Error != "" {
		if refusal.Code != "" {
			return fmt.Sprintf("%d %s: %s", status, refusal.Code, refusal.Error)
		}
		return fmt.Sprintf("%d: %s", status, refusal.Error)
	}
	return fmt.Sprintf("%d %s", status, strings.TrimSpace(string(body)))
}
