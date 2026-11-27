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
// (legion/metrics) read Dispatch's own tables rather than a daemon.
//
// Two bounds hold a read (the plan's D14, the spec's contract 6): RequestTimeout on the call and
// ResponseLimit, 64 MiB, on the body. The read asks for the route's observed view, which carries
// only the members Dispatch decodes (Client.State). An answer over the cap is a failed read that
// names the cap and what the read knows of the answer's size (readBounded) — the board's header
// says so and the last good observations stand — never a parse of the first 64 MiB.
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

// ErrUnavailable is every failed call to a daemon: it could not be reached, it answered something
// other than 200, its answer was over the cap, or its answer was not the JSON the route serves.
// One sentinel, because a reader of legion_reads acts the same way on all of them — the last good
// observations stand and the board's header names the failure.
var ErrUnavailable = errors.New("the Legion daemon could not be read")

// ErrResponseTooLarge is an answer over ResponseLimit, wrapped beside ErrUnavailable so a caller
// that wants to tell a daemon serving too much from a daemon that is down can. The failure a read
// answers wraps it in responseTooLarge, which names what the read knows of the answer's size too.
var ErrResponseTooLarge = fmt.Errorf("the answer is over the %d-byte cap", ResponseLimit)

// responseTooLarge is an answer over ResponseLimit as a failed read names it, with what the read
// knows of its size: where the answer declares a Content-Length over the cap, that length
// (declared), which the read refuses before it reads the body; otherwise the ResponseLimit+1
// bytes the read stopped at, so the answer is at least that long and nothing more of it is read.
// It wraps ErrResponseTooLarge.
type responseTooLarge struct {
	size     int64
	declared bool
}

func (e responseTooLarge) Error() string {
	if e.declared {
		return fmt.Sprintf("the answer of %d bytes is over the %d-byte cap", e.size, ResponseLimit)
	}
	return fmt.Sprintf("the answer is over the %d-byte cap: it declares no Content-Length, and the read stopped at %d bytes", ResponseLimit, e.size)
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
	body, read, err := readBounded(response.Body, response.ContentLength)
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

// readBounded reads body, whose Content-Length is length (-1 for none), and refuses an answer over
// ResponseLimit (responseTooLarge), so the refusal is never a parse of a truncated document. A
// length over the cap is refused with that length before anything is read. Otherwise it reads one
// byte past the cap at the most, to tell a body exactly at the cap from one over it, and refuses a
// body that reaches that byte, with nothing more read: the answer is then at least ResponseLimit+1
// bytes long. The size it returns is the bytes it read, or the size the refusal names.
func readBounded(body io.Reader, length int64) ([]byte, int64, error) {
	if length > ResponseLimit {
		return nil, length, responseTooLarge{size: length, declared: true}
	}
	read, err := io.ReadAll(io.LimitReader(body, ResponseLimit+1))
	size := int64(len(read))
	if size > ResponseLimit {
		return nil, size, responseTooLarge{size: size}
	}
	if err != nil {
		return nil, size, fmt.Errorf("read the answer: %v", err)
	}
	return read, size, nil
}
