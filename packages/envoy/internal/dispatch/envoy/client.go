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
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
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
	// Projects is the listener's sorted list of the projects the session's credential names (K6):
	// [] for a credential that names none, and nil for a session the listener sent no list for, as
	// every listener does until it learns each session's projects. The two stay apart, because a
	// list Dispatch does not know is no credential that names none: the route roster stores nil as
	// no projects key and [] as "projects":[] (omitzero), and the session rule passes a session with
	// no history and no stored list to no viewer but its own while a project is private
	// (access.SessionClause). Dispatch reads it there and serves it to no caller: agentResponse
	// shadows it with a field nothing sets, and the OpenAPI document declares neither
	// (api.unservedFields).
	Projects []string `json:"projects,omitzero"`
	LastSeen int64    `json:"last_seen"`
	// State is StateLive, StateUnresponsive or StateGone, as the listener judged this record
	// when it answered, or StateStopped, which only SessionIncludingStopped reads. A listener
	// that predates the field names none, and normalize reads that as StateLive: every session
	// such a listener answers with is one it holds live, so reading it as anything else would
	// retire working sessions during a rollout.
	State string `json:"state"`
}

// normalize fills in what a listener may leave out: the id of the one session that was asked
// about, the empty lists a decoder leaves nil, and the state an older listener does not name. It
// leaves Projects as the listener sent it: a list it did not send is unknown, never empty.
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
		Projects:     projectsOf(info.Projects),
		LastSeen:     info.LastSeen,
		State:        string(info.State),
	}
	session.normalize(sessionID)
	return session
}

// projectsOf is a session's projects as the listener sent them: nil for no list, and a list, never
// nil, for one it sent, [] included.
func projectsOf(sent *[]string) []string {
	if sent == nil {
		return nil
	}
	if *sent == nil {
		return []string{}
	}
	return *sent
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
	// Project is the project the delivered thing is of, a project key or contracts.NoProject:
	// every Dispatch send names one (api.listenerSendInput), the comment's project for a comment
	// mention and the message's, or NoProject, for a targeted message. It reaches the listener as
	// the body's project, which the listener takes for the message's own
	// (listenerwire.SendRequest.Project). Empty, the field is left out and the listener chooses it.
	Project string
}

// SendResult identifies the listener envelope emitted for a successful delivery. Duplicate
// reports that the stream already held this message, so it stored nothing new; the publish still
// reached the agent's subject. What recognises that repeat, and for how long, is stated on
// DELIVERY_DUPLICATE_WINDOW_MS in @legion/contracts (contracts.DeliveryDuplicateWindow here). The
// envelope id then names an envelope JetStream discarded.
//
// State is what the target was when the listener stored the message, SendStateConnected or
// SendStateStopped. A listener that predates the field answers none, which reads as a connected
// target.
type SendResult struct {
	EnvelopeID string
	Recipient  string
	Duplicate  bool
	State      string
}

// Client reads live session metadata from the Envoy listener.
type Client struct {
	baseURL    string
	apiToken   string
	httpClient *http.Client
	// registrations records the projects each session the listener answers lists
	// (WithRegistrations); nil records nothing.
	registrations RegistrationRecorder
}

// RegistrationRecorder records the projects the listener answers sessions with, the projects the
// token that made each registration lists (model.SessionProjects): events.Registrations, which
// writes them to session_projects.
type RegistrationRecorder interface {
	Record(ctx context.Context, answers []model.SessionProjects) error
}

// WithRegistrations records, through recorder, the projects of every session Session,
// SessionIncludingStopped and Sessions answer whose list names a project, in one call per read,
// before the read returns: a recorder's error is the read's, so nothing that resolves a session
// through the listener goes on without the record of the projects it registered under - the
// route of a direct message, a broadcast's recipients, a claim's holder and the agent list among
// them (access.SessionClause reads it).
func WithRegistrations(recorder RegistrationRecorder) Option {
	return func(c *Client) { c.registrations = recorder }
}

// recordRegistrations hands the recorder the projects of each of sessions whose list names one,
// in one call, and none when none does: a session whose list is empty, an all-projects token or a
// record from before the listener kept each session's projects, and one the listener sent no
// list for, record nothing.
func (c *Client) recordRegistrations(ctx context.Context, sessions ...Session) error {
	if c.registrations == nil {
		return nil
	}
	var answers []model.SessionProjects
	for _, session := range sessions {
		if len(session.Projects) > 0 {
			answers = append(answers, model.SessionProjects{SessionID: session.SessionID, Projects: session.Projects})
		}
	}
	if len(answers) == 0 {
		return nil
	}
	return c.registrations.Record(ctx, answers)
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
	return client
}

func (c *Client) authorize(request *http.Request) {
	if c.apiToken != "" {
		request.Header.Set("Authorization", "Bearer "+c.apiToken)
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
	// The projects each session registered under, in one record for the whole answer
	// (WithRegistrations).
	if err := c.recordRegistrations(ctx, sessions...); err != nil {
		return nil, err
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
	request, err := http.NewRequestWithContext(
		ctx, http.MethodPost, c.baseURL+"/v1/interests/unsubscribe", bytes.NewReader(body),
	)
	if err != nil {
		return nil, fmt.Errorf("%w: build POST /v1/interests/unsubscribe request: %v", ErrUnavailable, err)
	}
	request.Header.Set("Content-Type", "application/json")
	c.authorize(request)
	response, err := c.httpClient.Do(request)
	if err != nil {
		return nil, unreachable(fmt.Errorf("%w: POST /v1/interests/unsubscribe: %v", ErrUnavailable, err))
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

// NoRoleHolder is the error Role answers for a role no session claims: the listener's own words
// for it (cmd/listener writeRoleHolderError, roles.Unclaimed; TestNoRoleHolderIsTheListenersWords
// holds the two equal), so a caller that must answer a role as unheld answers it as Role would.
func NoRoleHolder(role string) error {
	return fmt.Errorf("no holder for role %s", role)
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
	session := sessionOf(info, sessionID)
	// The projects the session registered under (WithRegistrations), before the read returns.
	if err := c.recordRegistrations(ctx, session); err != nil {
		return Session{}, err
	}
	return session, nil
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
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+route+query, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: build GET %s request: %v", ErrUnavailable, route, err)
	}
	c.authorize(request)
	response, err := c.httpClient.Do(request)
	if err != nil {
		return nil, unreachable(fmt.Errorf("%w: GET %s: %v", ErrUnavailable, route, err))
	}
	return response, nil
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
// reason alone (listenerResponseError).
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
		Project:       input.Project,
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
	body, err := json.Marshal(send)
	if err != nil {
		return SendResult{}, fmt.Errorf("encode POST /v1/messages/send body: %w", err)
	}
	// written records that this client finished putting the request on the wire, which is what
	// separates a listener that went quiet (the envelope may be published) from a connection
	// that never came up (nothing was). Both surface as the same client-timeout error.
	var written atomic.Bool
	ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		WroteRequest: func(info httptrace.WroteRequestInfo) { written.Store(info.Err == nil) },
	})
	request, err := http.NewRequestWithContext(
		ctx, http.MethodPost, c.baseURL+"/v1/messages/send", bytes.NewReader(body),
	)
	if err != nil {
		return SendResult{}, fmt.Errorf("%w: build POST /v1/messages/send request: %v", ErrUnavailable, err)
	}
	request.Header.Set("Content-Type", "application/json")
	c.authorize(request)
	response, err := c.httpClient.Do(request)
	if err != nil {
		return SendResult{}, sendTransportError(err, written.Load())
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
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

// sendTransportError classifies what stopped a send from being answered. Only a request this
// client finished writing can have reached the listener, and only that one may be
// ErrReceiptTimeout - the class that tells a human the message may already have been published
// and a same-mode retry is therefore safe. A connect that timed out wrote nothing, and reports
// the same "Client.Timeout exceeded while awaiting headers" text as a listener that went quiet,
// so the text cannot tell them apart; requestWritten can. The cause is wrapped, not formatted,
// so a caller can ask the same question again. Either way no listener answered, so both are
// ErrUnreachable.
func sendTransportError(err error, requestWritten bool) error {
	var timeout net.Error
	timedOut := errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &timeout) && timeout.Timeout())
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
	return listenerRefusal(response, body.Error)
}

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
