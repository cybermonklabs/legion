// Package legion keeps each Legion daemon's own state in Dispatch (dispatch://INFRA-1077,
// dispatch://INFRA-3931).
//
// A project's daemon is named by one row of legion_settings (store.go). The daemon publishes each
// change of what Dispatch observes as a state event on notifications.legion.<token>.state. The
// intake (intake.go, apply.go) takes those events from the held durable
// bus.DispatchLegionStateDurable and writes every value that moved in legion_observations, with one
// legion_reads row for each transaction whose events it applies, which the transaction's last
// message writes (applyBatch). It reads the daemon's `GET /legion/v1/state` whole
// (Intake.readWhole, through this file's Client) only for the reasons the ReadReason constants
// name (whole_read.go), and a project's reads start at least ReadRetryAfter apart. No recurring
// timer reads a daemon. So the Legion board (api/legion_board.go) and the four metrics
// (legion/metrics) read Dispatch's own tables rather than a daemon. The one other call Dispatch
// makes of a daemon is Client.Controller, the session it names as the project's controller, which
// the state route of the project's record of deploys and failed changes asks at a controller's
// write (api/production_events.go, dispatch://INFRA-978) and nothing records.
//
// Two bounds hold a read (the plan's D14, the spec's contract 6): RequestTimeout on the call and
// ResponseLimit, 64 MiB, on the body. The read asks for the route's observed view, which carries
// only the members Dispatch decodes (Client.State). An answer over the cap is a failed read that
// names the cap and what the read knows of the answer's size (readBounded) — the board's header
// says so and the last good observations stand — never a parse of the first 64 MiB. A controller
// read has bounds of its own, ControllerTimeout and ControllerResponseLimit.
package legion

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// StatePath is the daemon route a whole read (Intake.readWhole) reads a project's state from. It
// authenticates nobody, and Dispatch sends it no credential.
const StatePath = "/legion/v1/state"

// ControllerPath is the daemon route Client.Controller reads the project's registered controller
// from: the session the state's controllerLocator names, and nothing else of the state, which a
// daemon serves from its daemon API entry 20261127150416 controller-route on. Like StatePath it
// authenticates nobody, and Dispatch sends it no credential and states no contract.
const ControllerPath = "/legion/v1/controller"

// ControllerTimeout bounds one controller read, the request and reading its body, unless the
// caller's own context ends sooner. The route reads one row of the daemon's store and no snapshot
// of its state, and its caller is a person's or a controller's write that waits for it, so two
// seconds is a daemon that is not answering rather than one still at work.
const ControllerTimeout = 2 * time.Second

// ControllerResponseLimit is the most of a controller read's answer Dispatch reads: 64 KiB,
// hundreds of times the one session's locator the route answers. A body over it is a failed read
// (readBounded), never a parse of its first 64 KiB.
const ControllerResponseLimit = 64 << 10

// RequestTimeout bounds one call to a daemon — the request and reading its body — unless the
// caller's own context ends sooner, which always wins. It is the daemon's own bound on the read it
// serves, sixty seconds (stateReadBound, packages/daemon/internal/api/server.go:46-51, which
// measured a healthy read of the production deployment at 8.34 s): a shorter window would give up
// on a read the daemon is still allowed to finish, and a bound at all is what keeps a whole read
// from holding its project's runner behind a daemon that accepts a connection and never answers.
const RequestTimeout = 60 * time.Second

// stateView is the view of StatePath a whole read asks for, `?view=observed`: the daemon's state
// in only the members State decodes (state.go), at the same JSON paths. A daemon of an earlier
// build ignores the query and serves its whole state, which the loose decode reads the same way.
const stateView = "observed"

// ResponseLimit is the most of a daemon's answer Dispatch reads: 64 MiB. A body over it is a
// failed read (readBounded, and readBytes for the size its row records). The observed view
// (stateView) measured 665,962 bytes of the INFRA daemon's state at 2026-11-20T20:35Z: 187 bytes
// for each of its 3,273 issues and about 54 KB for the project. INFRA gains about 210 issues a day
// (INFRA-1000 was created 2026-11-02T08:52Z, INFRA-4855 2026-11-20T20:21Z), so the view is about
// 15 MB after a year and reaches the cap after about four and a half years. A daemon of an earlier
// build serves its whole state, which measured 7,307,929 bytes at the same time and 4,055,424
// bytes on 2026-11-12, about 0.4 MB more each day: it stays under the cap for about 150 days,
// which covers a rollout of the daemon that serves the view.
const ResponseLimit = 64 << 20

// ErrUnavailable is every failed call to a daemon but a controller read's 404
// (ErrNoControllerRoute): it could not be reached, it answered something other than 200, its
// answer was over the cap, or its answer was not the JSON the route serves. One sentinel, because
// a reader of legion_reads acts the same way on all of them — the last good observations stand and
// the board's header names the failure.
var ErrUnavailable = errors.New("the Legion daemon could not be read")

// ErrNoControllerRoute is a daemon that answered a controller read 404: a build before
// ControllerPath, which serves no such route. It is the one failure of a controller read that is
// not ErrUnavailable, since it names what to roll out rather than a daemon that is down.
var ErrNoControllerRoute = fmt.Errorf("the Legion daemon answered 404 to GET %s: it runs a build before that route", ControllerPath)

// ErrResponseTooLarge is an answer over its read's cap, ResponseLimit or ControllerResponseLimit,
// wrapped beside ErrUnavailable so a caller that wants to tell a daemon serving too much from a
// daemon that is down can. The failure a read answers wraps it in responseTooLarge, which names the
// cap and what the read knows of the answer's size too.
var ErrResponseTooLarge = errors.New("the answer is over its read's cap")

// responseTooLarge is an answer over limit, its read's cap, as a failed read names it, with what
// the read knows of its size: where the answer declares a Content-Length over the cap, that length
// (declared), which the read refuses before it reads the body; otherwise the limit+1 bytes the read
// stopped at, so the answer is at least that long and nothing more of it is read. It wraps
// ErrResponseTooLarge.
type responseTooLarge struct {
	size     int64
	declared bool
	limit    int64
}

func (e responseTooLarge) Error() string {
	if e.declared {
		return fmt.Sprintf("the answer of %d bytes is over the %d-byte cap", e.size, e.limit)
	}
	return fmt.Sprintf("the answer is over the %d-byte cap: it declares no Content-Length, and the read stopped at %d bytes", e.limit, e.size)
}

func (responseTooLarge) Unwrap() error { return ErrResponseTooLarge }

// Client calls one Legion daemon.
type Client struct {
	baseURL string
	http    *http.Client
}

// Option configures a Client.
type Option func(*Client)

// WithTimeout replaces the window every call to the daemon gets. Tests use it to exercise the
// timeout without waiting out the production window.
func WithTimeout(timeout time.Duration) Option {
	return func(c *Client) { c.http.Timeout = timeout }
}

// New returns a client for the daemon at baseURL.
func New(baseURL string, options ...Option) *Client {
	client := &Client{
		baseURL: strings.TrimSuffix(strings.TrimSpace(baseURL), "/"),
		http: &http.Client{
			Timeout: RequestTimeout,
			// A daemon answers its own routes; it redirects nowhere. Following one would read a
			// state at a host the project's setting does not name, so a redirect is the answer the
			// daemon gave and a failed call (ErrUnavailable, naming the status) rather than a
			// second call.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
	for _, option := range options {
		option(client)
	}
	return client
}

// callFailure is a failed call as legion_reads keeps it and the board's header carries it: the
// transport's own reason, without the address Dispatch called. net/http wraps a transport failure
// in a *url.Error carrying the whole URL, and the dial under it in a *net.OpError carrying the
// host and port it resolved to; the daemon's address belongs to the deployment, and every reader
// of the project's board reads this text (legion_reads.error, the board's header), so only the
// innermost reason is kept — "connect: connection refused", "context deadline exceeded".
func callFailure(err error) error {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		err = urlErr.Err
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		err = opErr.Err
	}
	return err
}

// State reads the daemon's own state: the state it answered and the size of the answer as the read
// knows it, which a whole read records whether the read succeeded or not (Intake.readWhole): the
// bytes it read, or for an answer over ResponseLimit the size its failure names (readBounded).
// Every failure wraps ErrUnavailable, and an answer over ResponseLimit wraps ErrResponseTooLarge
// too.
//
// It asks for the route's observed view (stateView), so the body it reads and decodes grows by
// the 187 bytes of each issue's observed members, not by the whole issue (ResponseLimit). A daemon
// of an earlier build ignores the query and serves its whole state, which the decode reads the
// same way. The decode is loose — encoding/json ignores a field State does not name — so a daemon
// that grows the route's shape is still read (state.go). The call carries no credential: the route
// authenticates nobody.
func (c *Client) State(ctx context.Context) (State, int64, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+StatePath, nil)
	if err != nil {
		return State{}, 0, fmt.Errorf("%w: build GET %s: %v", ErrUnavailable, StatePath, err)
	}
	// A setting's URL carries no query (api/settings_legion.go), so the view is the whole query.
	request.URL.RawQuery = url.Values{"view": {stateView}}.Encode()
	response, err := c.http.Do(request)
	if err != nil {
		return State{}, 0, fmt.Errorf("%w: GET %s: %v", ErrUnavailable, StatePath, callFailure(err))
	}
	defer response.Body.Close()
	body, read, err := readBounded(response.Body, response.ContentLength, ResponseLimit)
	if err != nil {
		return State{}, read, fmt.Errorf("%w: GET %s: %w", ErrUnavailable, StatePath, err)
	}
	if response.StatusCode != http.StatusOK {
		return State{}, read, fmt.Errorf("%w: GET %s answered %d", ErrUnavailable, StatePath, response.StatusCode)
	}
	var state State
	if err := json.Unmarshal(body, &state); err != nil {
		return State{}, read, fmt.Errorf("%w: decode GET %s: %v", ErrUnavailable, StatePath, err)
	}
	return state, read, nil
}

// controllerAnswer is the members of ControllerPath's answer the controller read decodes: the
// session of the state's controllerLocator, absent while no session is registered.
type controllerAnswer struct {
	ControllerLocator *struct {
		SessionID string `json:"sessionId"`
	} `json:"controllerLocator"`
}

// Controller reads the session the daemon names as the project's registered controller
// (ControllerPath), and whether it names one: a daemon answers `{}` while no session is registered.
// The read is held to ControllerTimeout for the whole call and ControllerResponseLimit for the
// body, whatever window the client was built with, and carries no credential and no contract
// header, as State does. A 404 is ErrNoControllerRoute, a daemon of a build before the route; every
// other failure wraps ErrUnavailable with its cause and no daemon address (callFailure): no answer
// within the bound, which the failure names, another status, a body over the cap, which wraps
// ErrResponseTooLarge too, and a body that is not the route's object, a controllerLocator that
// names no session among them. The decode is loose otherwise, so a daemon that grows the answer is
// still read.
func (c *Client) Controller(ctx context.Context) (string, bool, error) {
	bounded, cancel := context.WithTimeout(ctx, ControllerTimeout)
	defer cancel()
	failed := func(err error) error {
		if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
			return fmt.Errorf("%w: GET %s: no answer within %s", ErrUnavailable, ControllerPath, ControllerTimeout)
		}
		return fmt.Errorf("%w: GET %s: %v", ErrUnavailable, ControllerPath, callFailure(err))
	}
	request, err := http.NewRequestWithContext(bounded, http.MethodGet, c.baseURL+ControllerPath, nil)
	if err != nil {
		return "", false, fmt.Errorf("%w: build GET %s: %v", ErrUnavailable, ControllerPath, err)
	}
	response, err := c.http.Do(request)
	if err != nil {
		return "", false, failed(err)
	}
	defer response.Body.Close()
	switch response.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return "", false, ErrNoControllerRoute
	default:
		return "", false, fmt.Errorf("%w: GET %s answered %d", ErrUnavailable, ControllerPath, response.StatusCode)
	}
	body, _, err := readBounded(response.Body, response.ContentLength, ControllerResponseLimit)
	if err != nil {
		if errors.Is(err, ErrResponseTooLarge) {
			return "", false, fmt.Errorf("%w: GET %s: %w", ErrUnavailable, ControllerPath, err)
		}
		return "", false, failed(err)
	}
	var answer *controllerAnswer
	if err := json.Unmarshal(body, &answer); err != nil {
		return "", false, fmt.Errorf("%w: decode GET %s: %v", ErrUnavailable, ControllerPath, err)
	}
	switch {
	case answer == nil:
		return "", false, fmt.Errorf("%w: decode GET %s: the answer is null, not the route's object", ErrUnavailable, ControllerPath)
	case answer.ControllerLocator == nil:
		return "", false, nil
	case answer.ControllerLocator.SessionID == "":
		return "", false, fmt.Errorf("%w: decode GET %s: its controllerLocator names no sessionId", ErrUnavailable, ControllerPath)
	}
	return answer.ControllerLocator.SessionID, true, nil
}

// readBounded reads body, whose Content-Length is length (-1 for none), and refuses an answer over
// limit, its read's cap (responseTooLarge), so the refusal is never a parse of a truncated
// document. A length over the cap is refused with that length before anything is read. Otherwise
// it reads one byte past the cap at the most, to tell a body exactly at the cap from one over it,
// and refuses a body that reaches that byte, with nothing more read: the answer is then at least
// limit+1 bytes long. The size it returns is the bytes it read, or the size the refusal names.
func readBounded(body io.Reader, length, limit int64) ([]byte, int64, error) {
	if length > limit {
		return nil, length, responseTooLarge{size: length, declared: true, limit: limit}
	}
	read, err := io.ReadAll(io.LimitReader(body, limit+1))
	size := int64(len(read))
	if size > limit {
		return nil, size, responseTooLarge{size: size, limit: limit}
	}
	if err != nil {
		return nil, size, fmt.Errorf("read the answer: %w", err)
	}
	return read, size, nil
}
