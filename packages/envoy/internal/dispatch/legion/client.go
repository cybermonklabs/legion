// Package legion reads each Legion daemon's own state into Dispatch (dispatch://INFRA-1077).
//
// A project's daemon is named by one row of legion_settings (store.go). Once a minute, from one
// replica under the `legion-state` lease, the sampler (sampler.go) reads that daemon's
// `GET /legion/v1/state`, records how the read went in legion_reads and every value that moved in
// legion_observations, so the Legion board (api/legion_board.go) and the four metrics
// (legion/metrics) read Dispatch's own tables rather than a daemon.
//
// Two bounds hold a read (the plan's D14, the spec's contract 6): RequestTimeout on the call and
// ResponseLimit on the body, 32 MiB. The INFRA daemon's state measured 877,746 bytes when the
// bounds were first set at 20 s and 8 MiB, and 7,606,561 bytes on 2026-11-21, growing 0.3 to
// 0.5 MB a day, so both were raised before its reads met the old cap.
// A read over the cap is a failed read — the board's header says so and the last good
// observations stand — never a parse of the first 32 MiB.
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

	"github.com/sjawhar/envoy/internal/redact"
	"github.com/sjawhar/envoy/internal/urljoin"
	"github.com/sjawhar/envoy/redirects"
)

// StatePath is the daemon route the sampler reads a project's state from. It authenticates
// nobody, and Dispatch sends it no credential.
const StatePath = "/legion/v1/state"

// RequestTimeout bounds one call to a daemon — the request and reading its body — unless the
// caller's own context ends sooner, which always wins. It is sixty seconds: a full read of the
// INFRA daemon's state took 10.9 to 11.9 s on 2026-11-21 and grows about 3.4 ms with each issue,
// and a daemon under load must still project and send a state of some megabytes in it. A bound at
// all is what keeps a minute's pass from piling up behind a daemon that accepts a connection and
// never answers.
const RequestTimeout = 60 * time.Second

// ResponseLimit is the most of a daemon's answer Dispatch reads: 32 MiB, about four times the
// 7,606,561 bytes the INFRA daemon's state measured on 2026-11-21. A body over it is a failed
// read.
const ResponseLimit = 32 << 20

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

// New returns a client for the daemon at baseURL, which each call's route is joined onto
// (urljoin.Join).
func New(baseURL string, options ...Option) *Client {
	client := &Client{
		baseURL: strings.TrimSpace(baseURL),
		// A daemon answers its own routes; it redirects nowhere. Following one would read a state
		// at a host the project's setting does not name, so a redirect is the answer the daemon
		// gave and a failed call (ErrUnavailable, naming the status) rather than a second call.
		http: redirects.Refused(&http.Client{Timeout: RequestTimeout}),
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
// The read goes to StatePath joined onto the path of the daemon's URL, that URL's query kept
// (urljoin.Join). A daemon URL that does not parse, or a URL joined from it that does not, is a
// failed read named by its class, since url.Parse's error quotes the URL whole and the board's
// header shows what the read says.
//
// The decode is loose — encoding/json ignores a field State does not name — so a daemon that
// grows the route's shape is still read (state.go). The members State keeps as the bytes the
// daemon sent, each issue's wait and the settings, are read from the answer as data (keptState),
// and every other member through its Decode (answeredState). The call carries no credential: the
// route authenticates nobody.
func (c *Client) State(ctx context.Context) (State, int, error) {
	var request *http.Request
	endpoint, err := urljoin.Join(c.baseURL, StatePath)
	if err == nil {
		request, err = http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	}
	if err != nil {
		return State{}, 0, fmt.Errorf("%w: build GET %s: %v", ErrUnavailable, StatePath, redact.Masked(err, redact.ClassNotAURL))
	}
	response, err := c.http.Do(request)
	if err != nil {
		return State{}, 0, fmt.Errorf("%w: GET %s: %v", ErrUnavailable, StatePath, callFailure(err))
	}
	defer response.Body.Close()
	answer, read, err := readBounded(response)
	if err != nil {
		return State{}, read, fmt.Errorf("%w: GET %s: %w", ErrUnavailable, StatePath, err)
	}
	if response.StatusCode != http.StatusOK {
		return State{}, read, fmt.Errorf("%w: GET %s answered %d", ErrUnavailable, StatePath, response.StatusCode)
	}
	var answered answeredState
	if err := answer.Decode(&answered); err != nil {
		return State{}, read, fmt.Errorf("%w: decode GET %s: %v", ErrUnavailable, StatePath, err)
	}
	var kept keptState
	if err := redact.Unmarshal(answer, &kept); err != nil {
		return State{}, read, fmt.Errorf("%w: decode GET %s: %v", ErrUnavailable, StatePath, err)
	}
	return answered.with(kept), read, nil
}

// answeredState is the state as a read decodes it from the daemon's answer (Answer.Decode): every
// member State holds but those it keeps as the bytes the daemon sent (keptState).
type answeredState struct {
	Issues    map[string]answeredIssue `json:"issues"`
	Admission *Admission               `json:"admission,omitempty"`
}

// keptState is the members State keeps as the bytes the daemon sent, each issue's wait and the
// settings, read from the answer as data (redact.Unmarshal): Dispatch stores them for the board
// and the metrics to read, and prints neither. A member that does not decode is named in fixed
// text (redact.DecodeError), as Decode names one of the rest.
type keptState struct {
	Issues map[string]struct {
		WaitingOn json.RawMessage `json:"waitingOn"`
	} `json:"issues"`
	Settings map[string]json.RawMessage `json:"settings"`
}

// with is the State the two reads of one answer make: each issue's decoded members with its wait,
// and the settings. Both read the same bytes by the same names, so they hold the same issues, and
// an answer whose `issues` is an empty object is still an empty map rather than none.
func (answered answeredState) with(kept keptState) State {
	state := State{Admission: answered.Admission, Settings: kept.Settings}
	if answered.Issues != nil {
		state.Issues = make(map[string]Issue, len(answered.Issues))
		for key, issue := range answered.Issues {
			state.Issues[key] = Issue{answeredIssue: issue, WaitingOn: kept.Issues[key].WaitingOn}
		}
	}
	return state
}

// readBounded reads at most ResponseLimit bytes of response's body, through redact.ReadAnswer,
// and refuses one carrying more. It reads one byte past the cap to tell a body exactly at it from
// one over it, so the refusal is never a parse of a truncated document; the count it returns for
// such a body is where the read stopped, the cap and one byte, since nothing counts past a body
// it refused.
func readBounded(response *http.Response) (redact.Answer, int, error) {
	answer, err := redact.ReadAnswer(response, ResponseLimit)
	if errors.Is(err, redact.ErrAnswerTooLarge) {
		return redact.Answer{}, ResponseLimit + 1, ErrResponseTooLarge
	}
	if err != nil {
		return redact.Answer{}, 0, fmt.Errorf("read the answer: %v", err)
	}
	read, _ := answer.StreamTo(io.Discard)
	return answer, int(read), nil
}
