// Package envoy is Dispatch's client for the Envoy listener's HTTP control API
// (cmd/listener/api.go). Every body it sends and every answer it reads is the listener's own
// type (listenerwire), never a copy of one.
package envoy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/nettimeout"
	"github.com/sjawhar/envoy/listenerwire"
	"github.com/sjawhar/envoy/roleholder"
)

// ErrUnavailable reports that the Envoy listener could not serve a request.
var ErrUnavailable = errors.New("envoy listener unavailable")

// ErrUnreachable marks the ErrUnavailable failures no listener answered: the request found no
// connection, lost it, or ran out of time before an answer, or a 502, 503 or 504 came back - a
// gateway in front of a listener that is gone, or a listener behind its starting gate
// (cmd/listener/main.go startingGate). Those are what a listener being replaced gives for a few
// seconds, so asking again a little later may be answered. Every other ErrUnavailable - a status
// the listener chose, such as the 401 it gives a token it refuses, or an answer that does not
// decode - is the listener's own answer, which asking again only repeats. An ErrUnreachable
// failure is ErrUnavailable too, with the same text, so a caller that asks only errors.Is(err,
// ErrUnavailable) reads it exactly as before.
var ErrUnreachable = errors.New("envoy listener unreachable")

// unreachableError marks the ErrUnavailable failure it wraps as ErrUnreachable without adding to
// its text.
type unreachableError struct{ error }

func (unreachableError) Is(target error) bool { return target == ErrUnreachable }

func (e unreachableError) Unwrap() error { return e.error }

// unreachable marks err, an ErrUnavailable failure, as ErrUnreachable.
func unreachable(err error) error { return unreachableError{err} }

// gatewayStatus reports whether a listener answered with a status that comes from in front of it
// or from its starting gate rather than from a decision about the request: a 502, 503 or 504.
func gatewayStatus(code int) bool {
	return code == http.StatusBadGateway || code == http.StatusServiceUnavailable || code == http.StatusGatewayTimeout
}

// unavailableStatus is the ErrUnavailable failure err a non-200 answer reads as, marked
// ErrUnreachable when its status is a gatewayStatus.
func unavailableStatus(code int, err error) error {
	if gatewayStatus(code) {
		return unreachable(err)
	}
	return err
}

// ErrReceiptTimeout reports a send the listener never answered within the client's window. It
// is the one failure that says nothing about whether the message landed: the listener publishes
// the envelope before it answers (cmd/listener/api.go), so the agent may already hold it. Only
// the send path returns it; a resolution lookup that times out sent nothing.
var ErrReceiptTimeout = errors.New("envoy listener did not answer the send in time")

// ErrNotFound reports that the listener has no record of the requested session.
var ErrNotFound = errors.New("envoy listener: session not found")

// ErrNoRoleHolder reports the listener's own answer to a role lookup that no live session holds the
// role: a 404 whose refusal names no claim (roleholder.Unclaimed) or a holder that has lapsed and
// that this client did not resolve to a record (roleholder.HolderLapsed). Every such error matches
// it under errors.Is and reads as the listener's own reason. It is the one decided "nobody" a role
// lookup answers: a status the listener chose otherwise - a 500 for a claim record it cannot read -
// and every ErrUnavailable say nothing about who holds the role.
var ErrNoRoleHolder = errors.New("envoy listener: no live session holds the role")

// noRoleHolderError is an ErrNoRoleHolder carrying the listener's own reason as its text, so a
// caller that records the failure records the words it recorded before.
type noRoleHolderError struct{ reason string }

func (e noRoleHolderError) Error() string        { return e.reason }
func (e noRoleHolderError) Is(target error) bool { return target == ErrNoRoleHolder }

// ErrRefilling reports a list read (Sessions, ListInterests) the listener refused because its
// copy of the bucket cannot be served yet: it has not completed a read of the bucket, or has not
// followed it for longer than its stale bound (cmd/listener writeListRefilling). It is not
// ErrUnavailable: the listener answered, and said exactly that a list from it now could be
// missing a live agent, so a caller says "refilling" rather than "unreachable", and never reads
// it as an empty list. Every *RefillingError matches it under errors.Is; errors.As reaches the
// RefillingError for the Retry-After the listener sent.
var ErrRefilling = errors.New("envoy listener: the agent list is refilling")

// listenerRefillingCode is the code of the listener's refilling answer (cmd/listener
// listRefillingCode). A 503 under any other code, or none, is an ordinary failure.
const listenerRefillingCode = "refilling"

// RefillingError is ErrRefilling as one list read met it.
type RefillingError struct {
	// RetryAfter is the listener's Retry-After header as sent, empty when it sent none.
	RetryAfter string
	route      string
	reason     string
}

func (e *RefillingError) Error() string {
	return fmt.Sprintf("%v: %s returned %d: %s", ErrRefilling, e.route, http.StatusServiceUnavailable, e.reason)
}

// Is makes every RefillingError match ErrRefilling.
func (e *RefillingError) Is(target error) bool { return target == ErrRefilling }

// Session states, as the listener computes them from a registration record's own age
// (packages/envoy/internal/session's StateOf): a session is live, unresponsive or gone, and a
// row's absence from a list is never what says so. StateUnresponsive is a session past the
// listener's live window and inside its gone horizon: it is stalled, not dead, so it keeps its
// role and issue claims, a message sent to it is stored for it, and nothing starts a
// replacement for it (dispatch://INFRA-392).
//
// StateStopped is a fourth answer, which only SessionIncludingStopped gives.
const (
	StateLive         = "live"
	StateUnresponsive = "unresponsive"
	StateGone         = "gone"
	StateStopped      = "stopped"
)

// includeUnresponsive is the selector every session read sends: without it the listener answers
// with the live sessions alone, which is the reading of its registry this client must never
// make - a session that missed its heartbeats under box load is still working its issue. Each
// read then judges by the row's own State. It is written out of StateUnresponsive, so the
// selector and the state it asks for cannot drift apart.
const includeUnresponsive = "?include=" + StateUnresponsive

// includeUnresponsiveAndStopped is the delivery's selector (SessionIncludingStopped): the same
// list with StateStopped added, under which the registry route answers a session it knows only by
// its record 200 with that state rather than 404.
const includeUnresponsiveAndStopped = includeUnresponsive + "," + StateStopped

// SendStateConnected and SendStateStopped are the two values a send's answer puts in `state`
// (cmd/listener, sendHandler), beside the recipient_state the registry's age gives. Connected
// means the session's relay subscriber answered the listener's probe or kept silent; stopped
// means no subscriber holds the session, or the listener knows it only by its record, so the
// message is stored for the session's own consumer and reaches it when it runs again.
const (
	SendStateConnected = "connected"
	SendStateStopped   = "stopped"
)

// Session is Dispatch's reading of one Envoy listener session, live, unresponsive or, on
// SessionIncludingStopped's answer alone, stopped. It is built from the listener's own answers,
// listenerwire.SessionInfo for a registry read or the list (sessionOf) and listenerwire.RoleHolder
// for a role lookup (Role), and keeps what Dispatch uses of them. Its JSON names are Dispatch's:
// its route roster stores the rows (api/issue_route_status.go) and GET /api/v1/agents answers them.
type Session struct {
	SessionID string `json:"session_id"`
	Title     string `json:"title"`
	Dir       string `json:"dir"`
	MachineID string `json:"machine_id"`
	// Host is where the session process actually runs, as the client reported it
	// (envoy-client's machineID()); MachineID is always the registering listener's own
	// machine and keeps its routing meaning. The listener falls back to MachineID when a
	// session's plugin predates this field, so Host is never empty here.
	Host string `json:"host"`
	// TmuxPlace is where the session's process sits in tmux, as the listener answered it, and GET
	// /api/v1/agents answers it unchanged, where packages/contracts' TmuxPlace declares it for the
	// pages that draw a machine's tmux layout. Nil for a session in no pane — one in Kubernetes,
	// or any process started outside tmux — and for one a listener that predates the field
	// answered: both are "Dispatch has no pane to draw", and neither puts a tmux_place key on an
	// agents row.
	TmuxPlace    *listenerwire.TmuxPlace `json:"tmux_place,omitempty"`
	Roles        []string                `json:"roles"`
	Capabilities []string                `json:"capabilities"`
	LastSeen     int64                   `json:"last_seen"`
	// State is StateLive, StateUnresponsive or StateGone, as the listener judged this record
	// when it answered, or StateStopped, which only SessionIncludingStopped reads. A listener
	// that predates the field names none, and normalize reads that as StateLive: every session
	// such a listener answers with is one it holds live, so reading it as anything else would
	// retire working sessions during a rollout.
	State string `json:"state"`
}

// normalize fills in what a listener may leave out: the id of the one session that was asked
// about, the empty lists a decoder leaves nil, and the state an older listener does not name.
func (s *Session) normalize(sessionID string) {
	if s.SessionID == "" {
		s.SessionID = sessionID
	}
	if s.Roles == nil {
		s.Roles = []string{}
	}
	if s.Capabilities == nil {
		s.Capabilities = []string{}
	}
	if s.State == "" {
		s.State = StateLive
	}
}

// sessionOf is Dispatch's reading of a session the listener answered with info (normalize fills
// in what it left out). A stopped session's answer (listenerwire.StoppedSessionInfo) carries its
// fields under the same names, so it reads here too, with no Dir, MachineID, Host, TmuxPlace or
// Roles.
func sessionOf(info listenerwire.SessionInfo, sessionID string) Session {
	session := Session{
		SessionID:    info.SessionID,
		Title:        info.Title,
		Dir:          info.Dir,
		MachineID:    info.MachineID,
		Host:         info.Host,
		TmuxPlace:    info.TmuxPlace,
		Roles:        info.Roles,
		Capabilities: info.Capabilities,
		LastSeen:     info.LastSeen,
		State:        string(info.State),
	}
	session.normalize(sessionID)
	return session
}

// SendInput is a targeted Dispatch delivery through the listener.
type SendInput struct {
	TargetSession  string
	Message        string
	Payload        json.RawMessage
	IdempotencyKey string
	Urgency        string
	// ExpectsReply is what the delivery wants back. It reaches the listener's HTTP body as the
	// JSON literal false, "optional" or "required", which is what contracts.ExpectsReply writes.
	ExpectsReply contracts.ExpectsReply
	// ConnectedOnly asks the listener to refuse a stopped target rather than store the message
	// for it: a 409 with the reason "the session is stopped", and nothing stored. Dispatch sets it
	// on every send of a model or thinking switch the session runs as its own turn
	// (api.isSessionSwitch), whose line would otherwise apply at an unknown later time, and on
	// every send of a message whose agents-route request asked for it. Unset, the field is left
	// out of the body.
	ConnectedOnly bool
	// Role is the role a send for a route to a role is sent for: the session TargetSession names
	// held it when Dispatch resolved the route (resolveDeliveryTarget in internal/dispatch/api).
	// The listener stores it on the envelope (contracts.Envelope.Role), so that a different session
	// that takes the role receives what the holder did not read, and keeps the message for the
	// role's next holder when the role's claim names another session than the target when the
	// send arrives (its 202). It takes the mark from Dispatch's own identity alone, so such a send
	// is made on that identity only (WithAccountTokenFile or WithGoogleIdentity, Send); a client
	// that presents no identity refuses it (ErrNoListenerIdentity) and logs it at Error.
	Role string
}

// SendResult identifies the listener envelope emitted for a successful delivery. Duplicate
// reports that the stream already held this message, so it stored nothing new; the publish still
// reached the agent's subject. What recognises that repeat, and for how long, is stated on
// DELIVERY_DUPLICATE_WINDOW_MS in @legion/contracts (contracts.DeliveryDuplicateWindow here). The
// envelope id then names an envelope JetStream discarded.
//
// State is what the target was when the listener stored the message, SendStateConnected or
// SendStateStopped. A listener that predates the field answers none, which reads as a connected
// target. A send for a role that the listener kept on the role's kept lane, because the role's
// claim named another session than the target when the send arrived, answers no State, and
// EnvelopeID names the copy the lane holds: the message reaches the session that holds the role,
// live, through the lane's walk, and the delivery records it sent like any other.
type SendResult struct {
	EnvelopeID string
	Recipient  string
	Duplicate  bool
	State      string
}

// Client reads live session metadata from the Envoy listener.
type Client struct {
	baseURL  string
	apiToken string
	// identity is Dispatch's own identity at the listener (listenerIdentity): the token file
	// WithAccountTokenFile names, or the Google identity WithGoogleIdentity names; nil presents
	// apiToken alone. The last of the two options given wins; cmd/dispatch refuses both.
	identity   listenerIdentity
	httpClient *http.Client
	// logger takes the line a call made again on the shared token writes (do); slog.Default()
	// unless WithLogger names another.
	logger *slog.Logger
}

// defaultTimeout is the window every listener call gets. A send that misses it is
// ErrReceiptTimeout rather than a plain failure, because the envelope may already be published.
const defaultTimeout = 5 * time.Second

// Option configures a Client.
type Option func(*Client)

// WithTimeout replaces the window every listener call gets. Tests use it to exercise a receipt
// timeout without waiting out the production window.
func WithTimeout(timeout time.Duration) Option {
	return func(c *Client) { c.httpClient.Timeout = timeout }
}

// WithToken sets the bearer every listener call sends (cmd/dispatch: ENVOY_TOKEN). Without it, or
// with an empty token, a call sends no Authorization header.
func WithToken(token string) Option {
	return func(c *Client) { c.apiToken = token }
}

// WithAccountTokenFile makes every listener call present Dispatch's own identity (cmd/dispatch:
// ENVOY_ACCOUNT_TOKEN_FILE): the projected Kubernetes service-account token the kubelet writes into
// the file at path for the listener's audience and rewrites before it expires. Each call reads the
// file when it is made, so the call after a rotation presents the new token, and judges the token
// by its own exp before presenting it, so a token the kubelet did not rewrite in time is never
// sent to earn a 401 (accountTokenFromFile). A call whose token could not be read, or that the
// listener answered 401, reads the file again and is made once more (do). It is presented in
// WithToken's bearer's place, which is then presented only on a call that carries no role mark
// and whose account token the listener did not take on two tries, and it is what lets a send
// carry its role mark (SendInput.Role).
func WithAccountTokenFile(path string) Option {
	return func(c *Client) { c.identity = tokenFile(path) }
}

// WithGoogleIdentity makes every listener call present Dispatch's own identity as the service
// account of the Google Cloud machine it runs on (cmd/dispatch: ENVOY_ACCOUNT_GOOGLE_AUDIENCE): an
// identity token for audience, the listener's ENVOY_OIDC_GOOGLE_AUDIENCE, which the metadata server
// mints (googleidentity.Token) and the client keeps until shortly before its own exp, and drops
// when the listener refuses it (do). A mint that failed is that call's failure (ErrAccountToken),
// which the delivery tries again; whether the machine has a metadata server at all is decided at
// Dispatch's start (CheckGoogleIdentity). It and WithAccountTokenFile are one identity each;
// cmd/dispatch refuses both at once.
func WithGoogleIdentity(audience string) Option {
	return func(c *Client) { c.identity = &googleIdentity{audience: audience} }
}

// WithLogger names the logger the client writes its lines to, slog.Default() when none is named.
func WithLogger(logger *slog.Logger) Option {
	return func(c *Client) { c.logger = logger }
}

// New returns a client for an Envoy listener's HTTP control API.
func New(baseURL string, options ...Option) *Client {
	client := &Client{
		baseURL: strings.TrimSuffix(baseURL, "/"),
		httpClient: &http.Client{
			Timeout: defaultTimeout,
		},
	}
	for _, option := range options {
		option(client)
	}
	if client.logger == nil {
		client.logger = slog.Default()
	}
	return client
}

// do makes one listener call, the request build makes, and returns the listener's answer or the
// error that kept it from answering: build's own, an identity token that could not be presented
// (ErrAccountToken), or a failure of the transport, which transport classifies (each caller's
// ErrUnavailable, and ErrUnreachable where nothing answered).
//
// A client that presents Dispatch's identity (WithAccountTokenFile or WithGoogleIdentity) makes
// the call on its identity token. A try whose token could not be presented, or that the listener
// answered 401, is made once more on a fresh token: the token file read again, where the kubelet
// rewrites a projected token in place, or a Google token minted again, the refused one dropped
// (listenerIdentity.forget). A call that still has no token the listener takes is made on the
// shared token (WithToken), with a WARN line, when the client holds one and the call carries no
// role mark (marked unset): a listener that does not verify Dispatch's identity yet - one of the
// release before, still serving while a deploy rolls - takes the shared token, and nothing of such
// a call needs Dispatch's identity. A marked call - a send for a role (Send) - is never made on the
// shared token, since the listener takes the role mark from Dispatch's identity alone and a send
// without it is not copied when the role moves: it ends with the second try's answer, logged at
// Error, and the delivery records that failure. A 401 is answered before the listener's route runs
// (cmd/listener, apiAuth), so a try it answered did nothing and the next is no repeat.
func (c *Client) do(route string, marked bool, build func() (*http.Request, error), transport func(error) error) (*http.Response, error) {
	try := func(shared bool) (*http.Response, error) {
		request, err := build()
		if err != nil {
			return nil, err
		}
		if err := c.authorize(request, shared); err != nil {
			return nil, fmt.Errorf("%s: %w", route, err)
		}
		response, err := c.httpClient.Do(request)
		if err != nil {
			return nil, transport(err)
		}
		return response, nil
	}
	response, err := try(false)
	if !c.presentsIdentity() || !identityNotTaken(response, err) {
		return response, err
	}
	closeBody(response)
	c.identity.forget()
	if response, err = try(false); !identityNotTaken(response, err) {
		return response, err
	}
	if marked || c.apiToken == "" {
		c.logger.Error("dispatch: the Envoy listener did not take Dispatch's identity token on two tries, so the call is not made on any other credential",
			"route", route, "role_mark", marked, "refusal", identityRefusal(response, err))
		return response, err
	}
	closeBody(response)
	c.logger.Warn("dispatch: the Envoy listener did not take Dispatch's identity token on two tries, so the call is made again on the shared token",
		"route", route, "refusal", identityRefusal(response, err))
	return try(true)
}

// identityNotTaken reports whether a try on Dispatch's identity ended without the listener taking
// that identity: its token could not be presented (ErrAccountToken), or the listener answered 401.
func identityNotTaken(response *http.Response, err error) bool {
	return errors.Is(err, ErrAccountToken) || (err == nil && response.StatusCode == http.StatusUnauthorized)
}

// identityRefusal is what a log line says of a try identityNotTaken reports: the error, which
// names the token's file and never the token, or the listener's status.
func identityRefusal(response *http.Response, err error) string {
	if err != nil {
		return err.Error()
	}
	return fmt.Sprintf("the listener answered %d", response.StatusCode)
}

// closeBody closes the body of an answer a try of do leaves unread; an error leaves none.
func closeBody(response *http.Response) {
	if response != nil {
		_ = response.Body.Close()
	}
}

// Sessions returns every session the listener holds a registration for, live and unresponsive
// alike, each carrying its own State; ErrRefilling (a *RefillingError) while the listener
// refuses the list as refilling, or ErrUnavailable. A caller tests each row's State, never its
// membership of the list: a list without the unresponsive rows reads every stalled session as
// ended, which is the whole of dispatch://INFRA-392.
func (c *Client) Sessions(ctx context.Context) ([]Session, error) {
	const route = "/v1/sessions"
	response, err := c.get(ctx, route, includeUnresponsive)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, listFailure(response, "GET "+route)
	}

	var rows []listenerwire.SessionInfo
	if err := json.NewDecoder(response.Body).Decode(&rows); err != nil {
		return nil, fmt.Errorf("%w: decode GET %s response: %v", ErrUnavailable, route, err)
	}
	// Each row's topics are left behind: Dispatch keeps every row of this answer in its route
	// roster row (api/issue_route_status.go) and serves it from GET /api/v1/agents, and neither
	// holds a session's subscriptions, which it reads through ListInterests instead.
	sessions := make([]Session, len(rows))
	for index, row := range rows {
		sessions[index] = sessionOf(row, row.SessionID)
	}
	return sessions, nil
}

// ListInterests returns every session's persisted topic subscriptions,
// live or not: the source Dispatch reads to find which sessions would
// receive an issue's or document's events. Its failures are Sessions'.
func (c *Client) ListInterests(ctx context.Context) ([]listenerwire.InterestRow, error) {
	const route = "/v1/interests/"
	response, err := c.get(ctx, route, "")
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, listFailure(response, "GET "+route)
	}

	var interests []listenerwire.InterestRow
	if err := json.NewDecoder(response.Body).Decode(&interests); err != nil {
		return nil, fmt.Errorf("%w: decode GET %s response: %v", ErrUnavailable, route, err)
	}
	if interests == nil {
		interests = []listenerwire.InterestRow{}
	}
	for index := range interests {
		if interests[index].Topics == nil {
			interests[index].Topics = []string{}
		}
	}
	return interests, nil
}

// Interest returns one session's persisted topic subscriptions, or ErrNotFound if the listener
// holds no interest record for it: a 404, or a session id the listener refuses as a KV key (a 400
// for one outside nats.go's key alphabet or holding an empty token, a 413 for one too long), under
// which no read finds a record. A subscriber removal names whatever id the dashboard listed, and
// the listener lists an interest a direct bucket write stored under such a key until an operator
// purges it, so a removal left pending on one completes once the interest is gone.
func (c *Client) Interest(ctx context.Context, sessionID string) (listenerwire.InterestRow, error) {
	route := "/v1/interests/" + url.PathEscape(sessionID)
	response, err := c.get(ctx, route, "")
	if err != nil {
		return listenerwire.InterestRow{}, err
	}
	defer response.Body.Close()
	switch response.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound, http.StatusBadRequest, http.StatusRequestEntityTooLarge:
		return listenerwire.InterestRow{}, ErrNotFound
	default:
		return listenerwire.InterestRow{}, unavailableGetStatus(response, route)
	}

	var interest listenerwire.InterestRow
	if err := json.NewDecoder(response.Body).Decode(&interest); err != nil {
		return listenerwire.InterestRow{}, fmt.Errorf("%w: decode GET %s response: %v", ErrUnavailable, route, err)
	}
	if interest.Topics == nil {
		interest.Topics = []string{}
	}
	return interest, nil
}

// Unsubscribe removes topics from a session's persisted interests and
// returns the topics the listener actually removed.
func (c *Client) Unsubscribe(ctx context.Context, sessionID string, topics []string) ([]string, error) {
	body, err := json.Marshal(listenerwire.UnsubscribeRequest{SessionID: sessionID, Topics: topics})
	if err != nil {
		return nil, fmt.Errorf("encode POST /v1/interests/unsubscribe body: %w", err)
	}
	const route = "POST /v1/interests/unsubscribe"
	response, err := c.do(route, false, func() (*http.Request, error) {
		request, err := http.NewRequestWithContext(
			ctx, http.MethodPost, c.baseURL+"/v1/interests/unsubscribe", bytes.NewReader(body),
		)
		if err != nil {
			return nil, fmt.Errorf("%w: build %s request: %v", ErrUnavailable, route, err)
		}
		request.Header.Set("Content-Type", "application/json")
		return request, nil
	}, func(err error) error {
		return unreachable(fmt.Errorf("%w: %s: %v", ErrUnavailable, route, err))
	})
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, unavailableStatus(response.StatusCode,
			fmt.Errorf("%w: POST /v1/interests/unsubscribe returned %d", ErrUnavailable, response.StatusCode))
	}

	var decoded listenerwire.UnsubscribeResponse
	if err := json.NewDecoder(response.Body).Decode(&decoded); err != nil {
		return nil, fmt.Errorf("%w: decode POST /v1/interests/unsubscribe response: %v", ErrUnavailable, err)
	}
	if decoded.Removed == nil {
		decoded.Removed = []string{}
	}
	return decoded.Removed, nil
}

// Role resolves a role's holder and returns its current session metadata, read from the role
// answer (listenerwire.RoleHolder). A role answer names the holder's state as holder_state rather
// than state, and a holder that is only unresponsive still holds the role: the caller delivers to
// it and the message waits for it. Only a holder the listener has no live claim for at all is the
// 404 listenerResponseError carries.
//
// The listener answers an unresponsive holder either way, and both are resolved here. A 200
// naming it holder_state unresponsive is read as it stands. The 404 the lookup route writes for
// it (reason holder_lapsed, holder_state unresponsive, claim_released false) says the role
// cannot be reached right now and that its claim still stands, which is not the same as the role
// having no holder: that holder is read through the session route, which answers an unresponsive
// record like any other, so the delivery still resolves to it and its frame is stored until it
// answers again (lapsedRoleHolder).
func (c *Client) Role(ctx context.Context, role string) (Session, error) {
	route := "/v1/roles/" + url.PathEscape(role)
	response, err := c.get(ctx, route, includeUnresponsive)
	if err != nil {
		return Session{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return c.lapsedRoleHolder(ctx, response)
	}
	var answer listenerwire.RoleHolder
	if err := json.NewDecoder(response.Body).Decode(&answer); err != nil {
		return Session{}, fmt.Errorf("%w: decode GET %s response: %v", ErrUnavailable, route, err)
	}
	// An older listener names no holder_state, which normalize reads as live.
	holder := Session{
		SessionID:    answer.Holder,
		Title:        answer.Title,
		Dir:          answer.Dir,
		MachineID:    answer.MachineID,
		Host:         answer.Host,
		Roles:        []string{role},
		Capabilities: answer.Capabilities,
		State:        string(answer.HolderState),
	}
	if answer.LastSeen != nil {
		holder.LastSeen = *answer.LastSeen
	}
	holder.normalize(answer.Holder)
	return holder, nil
}

// lapsedRoleHolder answers a role lookup the listener refused, having read nothing of its body
// yet. The refusal is listenerwire.RoleRefusal: a reason, and for a lapsed holder the holder's id,
// the state its record puts it in, and claim_released false.
//
// A holder that is only unresponsive keeps its claim, so the role still has a holder and this
// resolves to it: the lookup route refuses because the role cannot be reached now, not because
// it is free. That holder's own record is the authoritative read of it (Session), and it carries
// the title and capabilities a delivery is judged on, which the refusal does not.
//
// Everything else is the listener's own refusal: a role no session claims, a holder the registry
// calls gone, and a holder whose record could not be read at all. A holder whose record has gone
// between the two reads is the same answer, so a role is never resolved to a session the
// listener no longer holds.
func (c *Client) lapsedRoleHolder(ctx context.Context, response *http.Response) (Session, error) {
	var refusal listenerwire.RoleRefusal
	if err := json.NewDecoder(response.Body).Decode(&refusal); err != nil {
		return Session{}, listenerRefusal(response, "")
	}
	if refusal.Holder != "" && refusal.HolderState == StateUnresponsive {
		holder, err := c.Session(ctx, refusal.Holder)
		if err == nil {
			return holder, nil
		}
		if !errors.Is(err, ErrNotFound) {
			return Session{}, err
		}
	}
	err := listenerRefusal(response, refusal.Error)
	if roleholder.IsNoHolderReason(string(refusal.Reason)) && !errors.Is(err, ErrUnavailable) {
		return Session{}, noRoleHolderError{reason: err.Error()}
	}
	return Session{}, err
}

// Session returns one session's listener metadata, whatever its state, or ErrNotFound if the
// listener holds no record of it: a 404, or a session id the listener refuses as a KV key (a
// 400 for one outside nats.go's key alphabet or holding an empty token, a 413 for one too
// long), under which no session can have registered. Those three are "no record" rather than a
// failure because a key the bucket cannot hold names nothing a read could ever find, so a
// caller waiting on that session may stop waiting.
//
// This is the authoritative read a write makes about one session, and it answers the record's
// own State: an unresponsive session is a 200 like any other, and only the state says whether
// that session has ended. A caller asks here rather than scanning Sessions, because that list
// is answered from the listener's in-memory cache alone: a session whose cache entry is still
// being refilled is absent from the list while its record is there to be read, and a missing
// row cannot tell the two apart. A session the listener knows only by its known-session record,
// one that is not running, is ErrNotFound here (SessionIncludingStopped reads it). Every other
// status, a body that does not decode and a transport failure alike are ErrUnavailable carrying
// the listener's own reason, so a read the message bus did not answer reads as "ask again", never
// as "that session is gone"; only a transport failure and a 502, 503 or 504 are also
// ErrUnreachable, because a status the listener chose - a 401 for a token it refuses, say - is
// its answer and asking again repeats it.
func (c *Client) Session(ctx context.Context, sessionID string) (Session, error) {
	return c.readSession(ctx, sessionID, includeUnresponsive)
}

// SessionIncludingStopped is Session widened by one answer: a session the listener's registry
// holds no entry for, or one past its gone horizon, whose known-session record it still holds
// comes back as that record, with State StateStopped and the Title, Capabilities and LastSeen
// it holds, rather than as ErrNotFound. It carries no Dir, MachineID, Host, TmuxPlace or Roles:
// the record holds none, and a session that is not running has no address to be reached at, so
// a caller that needs one of those needs a running session.
//
// A delivery reads this one, and nothing else does. The listener takes a message for a stopped
// session it knows, stores it for the session's own consumer and hands it over when that session
// runs again, so refusing such a session here would throw a person's message away for a session
// that is only suspended. StateStopped is never a registry state: Session, Sessions and Role
// never answer it, so an issue claim's holder liveness (issue_claim.go), the route roster and
// role resolution (lapsedRoleHolder) never read a stopped session as one that holds anything. A
// session the listener holds neither an entry nor a record of is still ErrNotFound, which is
// what "no live session <id>" is recorded for. Every other answer, and every failure, is exactly
// Session's.
func (c *Client) SessionIncludingStopped(ctx context.Context, sessionID string) (Session, error) {
	return c.readSession(ctx, sessionID, includeUnresponsiveAndStopped)
}

// readSession is the one GET /v1/registry/<session_id> both reads make, under the include
// selector each asks with.
func (c *Client) readSession(ctx context.Context, sessionID, include string) (Session, error) {
	route := "/v1/registry/" + url.PathEscape(sessionID)
	response, err := c.get(ctx, route, include)
	if err != nil {
		return Session{}, err
	}
	defer response.Body.Close()
	switch response.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound, http.StatusBadRequest, http.StatusRequestEntityTooLarge:
		return Session{}, ErrNotFound
	default:
		return Session{}, unavailableGetStatus(response, route)
	}

	var info listenerwire.SessionInfo
	if err := json.NewDecoder(response.Body).Decode(&info); err != nil {
		return Session{}, fmt.Errorf("%w: decode GET %s response: %v", ErrUnavailable, route, err)
	}
	return sessionOf(info, sessionID), nil
}

// Agent returns the Legion agent registry's entry for one session (listenerwire.LegionAgentAnswer,
// whose Live is never read as "that agent has stopped"), or ErrNotFound when the registry holds
// none: a 404, or an id the listener refuses (a 400 or 413), under which no entry can be recorded.
// Every other status, a body that does not decode and a transport failure are ErrUnavailable
// carrying the listener's reason: a read the registry did not answer says nothing about whether
// the session was recorded.
func (c *Client) Agent(ctx context.Context, sessionID string) (listenerwire.LegionAgentAnswer, error) {
	route := "/v1/legion/agents/sessions/" + url.PathEscape(sessionID)
	response, err := c.get(ctx, route, "")
	if err != nil {
		return listenerwire.LegionAgentAnswer{}, err
	}
	defer response.Body.Close()
	switch response.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound, http.StatusBadRequest, http.StatusRequestEntityTooLarge:
		return listenerwire.LegionAgentAnswer{}, ErrNotFound
	default:
		return listenerwire.LegionAgentAnswer{}, unavailableGetStatus(response, route)
	}
	var agent listenerwire.LegionAgentAnswer
	if err := json.NewDecoder(response.Body).Decode(&agent); err != nil {
		return listenerwire.LegionAgentAnswer{}, fmt.Errorf("%w: decode GET %s response: %v", ErrUnavailable, route, err)
	}
	return agent, nil
}

// RoleAgents returns every session the Legion agent registry holds for a role token, newest
// first, the first being the role's current holder; none when it holds none. Any answer but a 200
// is ErrUnavailable.
func (c *Client) RoleAgents(ctx context.Context, roleToken string) ([]listenerwire.LegionAgentAnswer, error) {
	return c.legionAgents(ctx, "/v1/legion/agents/roles/"+url.PathEscape(roleToken), "")
}

// IssueAgents returns every session the Legion agent registry holds for an issue, by role in
// workflow order (architect, planner, implementer, tester, reviewer, merger), then newest first;
// none when it holds none. Any answer but a 200 is ErrUnavailable.
func (c *Client) IssueAgents(ctx context.Context, issueKey string) ([]listenerwire.LegionAgentAnswer, error) {
	return c.legionAgents(ctx, "/v1/legion/agents", "?issue="+url.QueryEscape(issueKey))
}

func (c *Client) legionAgents(ctx context.Context, route, query string) ([]listenerwire.LegionAgentAnswer, error) {
	response, err := c.get(ctx, route, query)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, unavailableGetStatus(response, route)
	}
	var body listenerwire.LegionAgentList
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("%w: decode GET %s response: %v", ErrUnavailable, route, err)
	}
	if body.Entries == nil {
		body.Entries = []listenerwire.LegionAgentAnswer{}
	}
	return body.Entries, nil
}

// get makes one authorized GET of route+query, the one shape every read of the listener takes.
// route names the read in each error this returns, without query's selector, so the same read
// reads alike however it was asked. A transport failure is ErrUnavailable and ErrUnreachable
// both: no listener answered it (unreachable).
func (c *Client) get(ctx context.Context, route, query string) (*http.Response, error) {
	return c.do("GET "+route, false, func() (*http.Request, error) {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+route+query, nil)
		if err != nil {
			return nil, fmt.Errorf("%w: build GET %s request: %v", ErrUnavailable, route, err)
		}
		return request, nil
	}, func(err error) error {
		return unreachable(fmt.Errorf("%w: GET %s: %v", ErrUnavailable, route, err))
	})
}

// unavailableGetStatus is ErrUnavailable for a GET of route the listener answered with a status
// the caller does not take, carrying the listener's own reason when its body names one, and
// ErrUnreachable too when that status came from in front of the listener (unavailableStatus).
// The two list reads answer their refusals through listFailure instead, which tells a refilling
// listener from an unavailable one.
func unavailableGetStatus(response *http.Response, route string) error {
	var failure listenerwire.APIError
	if err := json.NewDecoder(response.Body).Decode(&failure); err == nil && failure.Error != "" {
		return unavailableStatus(response.StatusCode,
			fmt.Errorf("%w: GET %s returned %d: %s", ErrUnavailable, route, response.StatusCode, failure.Error))
	}
	return unavailableStatus(response.StatusCode,
		fmt.Errorf("%w: GET %s returned %d", ErrUnavailable, route, response.StatusCode))
}

// sendSource is the source every send Dispatch makes through the listener names itself by.
const sendSource = "dispatch"

// EnvelopeMessage is the message the listener's send route stores for input
// (contracts.AgentSendEnvelope): from Dispatch, with the frame as its payload whole, an empty one
// included, and the delivery's urgency and expects_reply. Send writes its body from it, and
// Dispatch's own queued copy of a send for a session no store of the listener's holds is built
// from it, so the copy carries what the send would have.
func (input SendInput) EnvelopeMessage() contracts.Message {
	payload := string(input.Payload)
	return contracts.Message{
		Source:       sendSource,
		Text:         input.Message,
		Payload:      &payload,
		Urgency:      input.Urgency,
		ExpectsReply: input.ExpectsReply,
	}
}

// Send delivers a Dispatch frame to an already-resolved session. The answer says what that
// session was when the listener stored the message: connected, or stopped and stored for its own
// consumer (SendResult). A send that set ConnectedOnly and met a stopped session is refused
// instead, 409 with the listener's reason "the session is stopped", which arrives here as that
// reason alone (listenerResponseError). A send for a role (SendInput.Role) that the listener kept
// for the role's next holder is its 202, which names the kept copy (keptSend).
//
// A send for a role carries its role mark, which the listener takes from Dispatch's own identity
// alone, so it is made on that identity and never on the shared token (do, marked): a send the
// listener's authentication refused on two tries fails here, naming the role, and the delivery
// records it. A client that presents no identity (neither WithAccountTokenFile nor
// WithGoogleIdentity) cannot mark the send, so it refuses it (ErrNoListenerIdentity, logged at Error)
// rather than make it unmarked: the session that takes the role from the target would not
// receive an unmarked one.
func (c *Client) Send(ctx context.Context, input SendInput) (SendResult, error) {
	message := input.EnvelopeMessage()
	send := listenerwire.SendRequest{
		MessageBody: listenerwire.MessageBody{
			Source:         message.Source,
			Message:        message.Text,
			Payload:        message.Payload,
			IdempotencyKey: input.IdempotencyKey,
		},
		TargetSession: input.TargetSession,
		ConnectedOnly: input.ConnectedOnly,
	}
	if message.Urgency != "" {
		send.Urgency = &message.Urgency
	}
	if message.ExpectsReply != contracts.ExpectsReplyUnset {
		expectsReply, err := json.Marshal(message.ExpectsReply)
		if err != nil {
			return SendResult{}, fmt.Errorf("encode POST /v1/messages/send body: %w", err)
		}
		send.ExpectsReply = expectsReply
	}
	marked := input.Role != ""
	if marked && !c.presentsIdentity() {
		err := fmt.Errorf("%w (a send for role %s to %s)", ErrNoListenerIdentity, input.Role, input.TargetSession)
		c.logger.Error("dispatch: a send for a role is refused: this Dispatch presents no identity to the Envoy listener",
			"role", input.Role, "target_session", input.TargetSession, "error", err.Error())
		return SendResult{}, err
	}
	send.Role = input.Role
	encoded, err := json.Marshal(send)
	if err != nil {
		return SendResult{}, fmt.Errorf("%w: encode POST /v1/messages/send body: %v", ErrUnavailable, err)
	}
	// written records that this client finished putting the request on the wire, which is what
	// separates a listener that went quiet (the envelope may be published) from a connection
	// that never came up (nothing was). Both surface as the same client-timeout error. It is
	// cleared as each request is built, so a second try after a 401 is judged by its own write:
	// the 401 came from the listener's authentication, before any route ran.
	var written atomic.Bool
	ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		WroteRequest: func(info httptrace.WroteRequestInfo) { written.Store(info.Err == nil) },
	})
	response, err := c.do("POST /v1/messages/send", marked, func() (*http.Request, error) {
		written.Store(false)
		request, err := http.NewRequestWithContext(
			ctx, http.MethodPost, c.baseURL+"/v1/messages/send", bytes.NewReader(encoded),
		)
		if err != nil {
			return nil, fmt.Errorf("%w: build POST /v1/messages/send request: %v", ErrUnavailable, err)
		}
		request.Header.Set("Content-Type", "application/json")
		return request, nil
	}, func(err error) error { return sendTransportError(err, written.Load()) })
	if err != nil {
		return SendResult{}, err
	}
	defer response.Body.Close()
	switch {
	case response.StatusCode == http.StatusAccepted:
		return keptSend(response)
	case marked && response.StatusCode == http.StatusUnauthorized:
		return SendResult{}, fmt.Errorf("%w: POST /v1/messages/send for role %s: the listener refused Dispatch's identity token (ENVOY_ACCOUNT_TOKEN_FILE or ENVOY_ACCOUNT_GOOGLE_AUDIENCE) on two tries, and a send for a role is made on that identity alone; the listener verifies it with its ENVOY_OIDC_ISSUER/ENVOY_OIDC_AUDIENCE or ENVOY_OIDC_GOOGLE_AUDIENCE/ENVOY_OIDC_GOOGLE_ACCOUNTS and takes the role mark from ENVOY_DISPATCH_PRINCIPAL alone: %w",
			ErrUnavailable, input.Role, listenerResponseError(response))
	case response.StatusCode != http.StatusOK:
		return SendResult{}, listenerResponseError(response)
	}
	var answer listenerwire.SendResponse
	if err := json.NewDecoder(response.Body).Decode(&answer); err != nil {
		return SendResult{}, fmt.Errorf("%w: decode POST /v1/messages/send response: %v", ErrUnavailable, err)
	}
	return SendResult{
		EnvelopeID: answer.EventID, Recipient: answer.Recipient, Duplicate: answer.Duplicate,
		State: answer.State,
	}, nil
}

// keptSend reads the send route's 202, a send for a role the listener kept for the role's next
// holder (listenerwire.KeptSendResponse), as the envelope the role's kept lane holds, whose walk
// hands it to the session that holds the role, live. A 202 naming no kept copy is no answer a
// send gets, and is ErrUnavailable.
func keptSend(response *http.Response) (SendResult, error) {
	var answer listenerwire.KeptSendResponse
	if err := json.NewDecoder(response.Body).Decode(&answer); err != nil {
		return SendResult{}, fmt.Errorf("%w: decode POST /v1/messages/send response: %v", ErrUnavailable, err)
	}
	if !answer.Kept {
		return SendResult{}, fmt.Errorf("%w: POST /v1/messages/send answered 202 naming no kept copy", ErrUnavailable)
	}
	return SendResult{EnvelopeID: answer.EventID, Recipient: answer.Recipient}, nil
}

// sendTransportError classifies what stopped a send from being answered. Only a request this
// client finished writing can have reached the listener, and only that one may be
// ErrReceiptTimeout - the class that tells a human the message may already have been published
// and a same-mode retry is therefore safe. A connect that timed out wrote nothing, and reports
// the same "Client.Timeout exceeded while awaiting headers" text as a listener that went quiet,
// so the text cannot tell them apart; requestWritten can. The cause is wrapped, not formatted, so
// a caller can ask the same question again. Either way no listener answered, so both are
// ErrUnreachable.
func sendTransportError(err error, requestWritten bool) error {
	timedOut := nettimeout.InChain(err)
	if requestWritten && timedOut {
		return unreachable(fmt.Errorf("%w: %w: POST /v1/messages/send: %w", ErrUnavailable, ErrReceiptTimeout, err))
	}
	return unreachable(fmt.Errorf("%w: POST /v1/messages/send: %w", ErrUnavailable, err))
}

// listenerResponseError is the error a role lookup or a send reports for a non-200 answer. A 502,
// 503 or 504 is ErrUnavailable and ErrUnreachable whatever its body says: the listener behind its
// starting gate answers 503 {"error":"service starting"} (cmd/listener/main.go startingGate), and
// a gateway in front of one that is gone answers 502 or 504, so either is "ask again" rather than
// the listener's verdict on the request. Any other status with a JSON reason is that reason alone,
// which a delivery records and a person reads verbatim ("no live session <id>", "message is
// required", "unauthorized"). The reason is kept on the unavailable error too, for the same
// reader. Any other status without one is ErrUnavailable alone: the listener answered, and asking
// again repeats it.
func listenerResponseError(response *http.Response) error {
	var body listenerwire.APIError
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		return listenerRefusal(response, "")
	}
	if response.StatusCode == http.StatusServiceUnavailable && body.Code == listenerwire.CodeMaybeStored {
		return unreachable(fmt.Errorf("%w: %w: listener returned %d: %s", ErrUnavailable, ErrStoreUnconfirmed, response.StatusCode, body.Error))
	}
	return listenerRefusal(response, body.Error)
}

// ErrStoreUnconfirmed reports a send the listener answered 503 with the code that says the
// message may be stored (listenerwire.CodeMaybeStored): it published the envelope and the message
// bus never acknowledged the publish, so the session may already hold it, as after
// ErrReceiptTimeout. It is a failure of its own rather than ErrReceiptTimeout because the listener
// did answer; it is ErrUnavailable and ErrUnreachable too, as every 503 is.
var ErrStoreUnconfirmed = errors.New("envoy listener could not confirm the send was stored")

// listenerRefusal is a refused answer as its caller reports it: the listener's own reason when
// its body named one, and its status when the body named nothing a caller could act on. A 502,
// 503 or 504 is ErrUnavailable and ErrUnreachable whatever its body says, reason and all, since
// it comes from in front of the listener or from its starting gate rather than from a decision
// about the request.
func listenerRefusal(response *http.Response, reason string) error {
	code := response.StatusCode
	if reason != "" {
		if gatewayStatus(code) {
			return unreachable(fmt.Errorf("%w: listener returned %d: %s", ErrUnavailable, code, reason))
		}
		return errors.New(reason)
	}
	return unavailableStatus(code, fmt.Errorf("%w: listener returned %d", ErrUnavailable, code))
}

// listFailure classifies a list read's non-200 answer from its body: the listener's refilling 503
// is a *RefillingError, anything else ErrUnavailable carrying the listener's own reason when its
// body names one, and ErrUnreachable too when the status is a 502, 503 or 504, which comes from
// in front of the listener or from its starting gate rather than from a verdict on the read.
func listFailure(response *http.Response, route string) error {
	var body listenerwire.APIError
	decoded := json.NewDecoder(response.Body).Decode(&body) == nil
	if decoded && response.StatusCode == http.StatusServiceUnavailable && body.Code == listenerRefillingCode {
		return &RefillingError{RetryAfter: response.Header.Get("Retry-After"), route: route, reason: body.Error}
	}
	if !decoded || body.Error == "" {
		return unavailableStatus(response.StatusCode,
			fmt.Errorf("%w: %s returned %d", ErrUnavailable, route, response.StatusCode))
	}
	return unavailableStatus(response.StatusCode,
		fmt.Errorf("%w: %s returned %d: %s", ErrUnavailable, route, response.StatusCode, body.Error))
}
