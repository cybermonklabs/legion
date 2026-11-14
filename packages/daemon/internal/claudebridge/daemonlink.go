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
	"syscall"
	"time"

	"github.com/sjawhar/legion/daemon/internal/shimwire"
)

// The seen report's and the take's re-send (departure 7 of INFRA-2811's plan), with the numbers
// Oh My Pi's taker uses (packages/pi-shared/src/task-take.ts: TASK_ANSWER_TIMEOUT_MS,
// TASK_RESEND_FIRST_DELAY_MS, TASK_RESEND_MAX_DELAY_MS), so both harnesses wait out a daemon that
// is not serving alike.
const (
	// taskAnswerTimeout bounds one attempt: an attempt with no answer by then is sent again.
	taskAnswerTimeout = 5 * time.Minute
	// taskResendFirst is the first wait before an attempt with no answer is sent again; each wait
	// after it doubles, up to taskResendMax, and nothing limits the attempts.
	taskResendFirst = 500 * time.Millisecond
	taskResendMax   = 30 * time.Second
)

// maxTaskAnswerBytes bounds what the bridge reads of a seen report's or a take's answer. An
// accepted take carries the task's whole text, which the daemon's prompt frame used to carry
// within the worker stream's frame limit, so that limit bounds it here too.
const maxTaskAnswerBytes = shimwire.MaxFrameBytes

// daemonLink is the claim's daemon as the bridge speaks to it: the API's address, and the launch's
// boot token and generation, which name this claim in the boot form of every claim route the
// bridge calls (claim.BootCaller) — the turn-outcome report (turnoutcome.go), and the seen report
// and the take of the claim's task (take.go). The bridge holds no registration secret: the claim's
// registration is claude-envoy's, inside Claude Code, and the secret never leaves that process. So
// the bridge names itself with the launch's boot token, the file its pane is told, which the
// daemon resolves to this claim.
type daemonLink struct {
	// url is the API's address, with no trailing slash.
	url        string
	bootToken  string
	generation uint64
	log        *log.Logger
	// calls is the timing of the seen reports and the takes (untilAnswered), and client what sends
	// them: no timeout of its own, since each attempt is bounded by calls.answerTimeout, and a
	// connection for each attempt (DisableKeepAlives), so a daemon that restarted between two
	// attempts is met as a refused connection rather than as a pooled one it closed.
	calls  daemonCalls
	client *http.Client
}

// daemonCalls is the timing and the network of the seen reports and the takes. Its zero value is
// production's: the waits departure 7 names and the system's own dialer. A test shortens the
// waits and stands in for the network (Config.calls).
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
// answer: its status and its body. An attempt the daemon gave no answer to (unanswered) is sent
// again, the same request, after a wait that doubles from calls.resendFirst to calls.resendMax,
// with no attempt limit; each attempt is bounded by calls.answerTimeout. The first attempt with no
// answer is logged, once, naming what. ctx ending — the request replaced by a newer value, or the
// bridge ending — ends the sending with ctx's error. An error that is no failure departure 7 names
// is returned as the request's answer, and nothing sends it again.
func (d *daemonLink) untilAnswered(ctx context.Context, what, path string, request any) (int, []byte, error) {
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
		if why == "" {
			return status, answer, err
		}
		if !told {
			told = true
			d.log.Printf("%s got no answer from the daemon (%s); it is sent again until it is answered", what, why)
		}
		wait := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			wait.Stop()
			return 0, nil, ctx.Err()
		case <-wait.C:
		}
		delay = min(delay*2, d.calls.resendMax)
	}
}

// attempt is one post of body to the route at path, bounded by calls.answerTimeout, with the
// daemon's answer read whole.
func (d *daemonLink) attempt(ctx context.Context, path string, body []byte) (int, []byte, error) {
	ctx, cancel := context.WithTimeout(ctx, d.calls.answerTimeout)
	defer cancel()
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
	var answer bytes.Buffer
	_, err = answer.ReadFrom(http.MaxBytesReader(nil, response.Body, maxTaskAnswerBytes))
	return response.StatusCode, answer.Bytes(), err
}

// unanswered names why an attempt got no answer from the daemon, and is "" for an attempt it
// answered: the failures departure 7 names, and nothing else. A refused, reset or closed
// connection and a name that did not resolve never reached the daemon or lost its answer on the
// way; an attempt past calls.answerTimeout is a daemon that took the request and never answered;
// and a 502, 503 or 504 that names no Legion error code is a proxy in front of a daemon that is not
// serving. Every refusal the daemon itself makes names a code (api's taskRefusal), so a coded
// answer, whatever its status, is the daemon's word and is never sent again.
func (d *daemonLink) unanswered(status int, body []byte, err error) string {
	var dns *net.DNSError
	switch {
	case err == nil:
	case errors.Is(err, context.DeadlineExceeded):
		return fmt.Sprintf("no answer within %v", d.calls.answerTimeout)
	case errors.Is(err, syscall.ECONNREFUSED):
		return "the connection was refused"
	case errors.Is(err, syscall.ECONNRESET), errors.Is(err, syscall.EPIPE):
		return "the connection was reset"
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return "the connection was closed"
	case errors.As(err, &dns):
		return "the daemon's name did not resolve"
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
