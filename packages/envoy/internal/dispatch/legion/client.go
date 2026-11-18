// Package legion reads each Legion daemon's own state into Dispatch (dispatch://INFRA-1077).
//
// A project's daemon is named by one row of legion_settings (store.go). Once a minute, from one
// replica under the `legion-state` lease, the sampler (sampler.go) reads that daemon's
// `GET /legion/v1/state`, records how the read went in legion_reads and every value that moved in
// legion_observations, so the Legion board (api/legion_board.go) and the four metrics
// (legion/metrics) read Dispatch's own tables rather than a daemon.
//
// Two bounds hold a read (the plan's D14, the spec's contract 6): RequestTimeout on the call and
// ResponseLimit on the body, about nine times the 877,746 bytes the INFRA daemon's state measured.
// A read over the cap is a failed read — the board's header says so and the last good
// observations stand — never a parse of the first 8 MiB.
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

// StatePath is the daemon route the sampler reads a project's state from. It authenticates
// nobody, and Dispatch sends it no credential.
const StatePath = "/legion/v1/state"

// RequestTimeout bounds one call to a daemon — the request and reading its body — unless the
// caller's own context ends sooner, which always wins. It is the plan's D14: twenty seconds is
// room for a daemon under load to project a state of some megabytes and for Dispatch to read it
// at ResponseLimit, and a bound at all is what keeps a minute's pass from piling up behind a
// daemon that accepts a connection and never answers.
const RequestTimeout = 20 * time.Second

// ResponseLimit is the most of a daemon's answer Dispatch reads: 8 MiB, about nine times the
// 877,746 bytes the INFRA daemon's state measured at (the plan's D14). A body over it is a failed
// read.
const ResponseLimit = 8 << 20

// ErrUnavailable is every failed call to a daemon: it could not be reached, it answered something
// other than 200, its answer was over the cap, or its answer was not the JSON the route serves.
// One sentinel, because a reader of legion_reads acts the same way on all of them — the last good
// observations stand and the board's header names the failure.
var ErrUnavailable = errors.New("the Legion daemon could not be read")

// ErrResponseTooLarge is an answer over ResponseLimit, wrapped beside ErrUnavailable so a caller
// that wants to tell a daemon serving too much from a daemon that is down can.
var ErrResponseTooLarge = fmt.Errorf("the answer is over the %d-byte cap", ResponseLimit)

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

// State reads the daemon's own state: the state it answered and the bytes it sent, which the
// sampler records whether the read succeeded or not. Every failure wraps ErrUnavailable.
//
// The decode is loose — encoding/json ignores a field State does not name — so a daemon that
// grows the route's shape is still read (state.go). The call carries no credential: the route
// authenticates nobody.
func (c *Client) State(ctx context.Context) (State, int, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+StatePath, nil)
	if err != nil {
		return State{}, 0, fmt.Errorf("%w: build GET %s: %v", ErrUnavailable, StatePath, err)
	}
	response, err := c.http.Do(request)
	if err != nil {
		return State{}, 0, fmt.Errorf("%w: GET %s: %v", ErrUnavailable, StatePath, callFailure(err))
	}
	defer response.Body.Close()
	body, read, err := readBounded(response.Body)
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

// readBounded reads at most ResponseLimit bytes of body and refuses one carrying more. It reads
// one byte past the cap to tell a body exactly at it from one over it, so the refusal is never a
// parse of a truncated document; the count it returns for such a body is where the read stopped,
// the cap and one byte, since nothing counts past a body it refused.
func readBounded(body io.Reader) ([]byte, int, error) {
	read, err := io.ReadAll(io.LimitReader(body, ResponseLimit+1))
	if err != nil {
		return nil, len(read), fmt.Errorf("read the answer: %v", err)
	}
	if len(read) > ResponseLimit {
		return nil, len(read), ErrResponseTooLarge
	}
	return read, len(read), nil
}
