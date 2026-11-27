// Package notify is the daemon's client for the Envoy listener: it publishes persisted workflow
// notices through its publish route, reads a role's holder through its role route, and ends a
// session's registration through its session route.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/sjawhar/envoy/listenerwire"
	"github.com/sjawhar/envoy/oauth2call"
	"github.com/sjawhar/envoy/roleholder"

	"github.com/sjawhar/envoy/redirects"
	"github.com/sjawhar/legion/daemon/internal/redact"
	"github.com/sjawhar/legion/daemon/internal/urljoin"
)

const requestTimeout = 10 * time.Second

// refusalBodyLimit bounds how much of a refused publish's body the error carries: the listener
// answers `{"error": ...}` (packages/envoy/cmd/listener/api.go writeJSONError).
const refusalBodyLimit = 4096

// ErrNoHolder is a role publish the listener refused because no live session holds the role: nobody
// claimed it, or its holder's registration lapsed past the gone horizon (writeRoleHolderError,
// packages/envoy/cmd/listener/api.go). Every listener release answers it, 404 with one of the
// shared no-holder reasons (roleholder.IsNoHolderReason). It is the ordinary state before a
// role's session claims it, not a broken publish path, so a caller can hold what it published
// until one does. Publish returns it only for a 404 whose body names one of those reasons; any
// other 404, such as a route an older listener lacks, is a plain refusal.
var ErrNoHolder = errors.New("no live session holds the role")

// ErrHolderUnresponsive is a role publish the listener refused 404 `holder_lapsed` naming a holder
// whose `holder_state` is `unresponsive`: its last registration is past the listener's live window
// but still inside its gone horizon, so the session is not dead. It keeps its role claim and its
// issue claims, nothing is released for it and no replacement is started, and a box under load is
// the ordinary cause (dispatch://INFRA-392). The caller holds what it published and sends it
// again rather than acting on a death: the session answers for itself once its heartbeats land.
var ErrHolderUnresponsive = errors.New("the role's holder is unresponsive")

// ReasonHolderLapsed is the listener's 404 reason for a role whose holder is past its live window,
// and StateUnresponsive the `holder_state` it carries while that holder is inside the gone horizon
// (writeRoleHolderError, packages/envoy/cmd/listener/api.go). A 404 that names no `holder_state` is
// an older listener's, whose lapsed holder is no holder at all. ReasonHolderLapsed is the shared
// reason itself (roleholder.HolderLapsed), so the daemon's reading of the wire word and the
// listener's writing of it cannot drift; StateUnresponsive is declared here, with the rest of the
// listener's wire words this daemon reads, so the publisher and the controller's liveness probe
// read one copy of them.
const (
	ReasonHolderLapsed = roleholder.HolderLapsed
	StateUnresponsive  = "unresponsive"
)

// The reasons of a role publish the listener answers 504 (ErrNotDelivered). ReasonReceiptTimeout
// says the last forward reached the NATS server and the holder's session sent no receipt for it,
// which a holder that stopped running while its registration lives gives; ReasonDeliveryFailed
// says the listener found no live holder to forward to, could not read the role or session in
// time, or could not show that a forward reached the server, which a hub that is down gives.
const (
	ReasonReceiptTimeout = "receipt_timeout"
	ReasonDeliveryFailed = "delivery_failed"
)

// ErrNotDelivered is a role publish the listener answered 504: it forwarded the message to the
// role's holder, or tried to, and the holder's session did not confirm it within the listener's
// delivery window (publishDeliveryWindow, packages/envoy/cmd/listener/confirmed.go). Reason is
// ReasonReceiptTimeout or ReasonDeliveryFailed; Holder is the session the listener last resolved,
// empty when it resolved none; EventID names the envelope it forwarded. Holder and EventID are the
// listener's words as they came (listenerwire.RoleSendFailureAs, read with its words held as
// oauth2call.RemoteText), which a caller that decides on them compares as they came
// (RemoteText.Equal: the re-hold's copyDueAtOnce) and which print only as the rule reads them.
// Only a listener that confirms a role message before it answers gives it. An earlier listener
// answers 200 once the NATS server took the message and reports a forward that then fails as a
// role-lane exception (notifications.envoy.exceptions.notifications.role.*), so a caller that acts
// on this answer still has to hear those reports while such a listener runs. A 504 without one of
// the two reasons is a plain refusal, never ErrNotDelivered.
type ErrNotDelivered struct {
	Reason  string
	Holder  oauth2call.RemoteText
	EventID oauth2call.RemoteText
}

func (e ErrNotDelivered) Error() string {
	return fmt.Sprintf("the role's holder %q did not confirm event %s (%s)", e.Holder, e.EventID, e.Reason)
}

// ErrRefusedByHolder is a role publish the listener answered 403 with reason "capability": the
// role's holder, the session Holder names (the listener's words as they came), does not advertise
// the delivery mode of the payload, or
// the mode cannot be read unambiguously, so the listener forwarded nothing. Sending it again
// changes nothing until another session holds the role. Only a listener that confirms a role
// message before it answers gives it; an earlier listener's role lane drops such a message after
// its 200 without a report.
type ErrRefusedByHolder struct {
	Holder oauth2call.RemoteText
}

func (e ErrRefusedByHolder) Error() string {
	return fmt.Sprintf("the role's holder %q does not advertise the delivery mode", e.Holder)
}

// ErrPublishDenied is a role publish the listener answered 403 because the NATS server refused the
// forward under the listener's own publish permissions, so the listener forwarded nothing: its body's
// error opens with publishDeniedPrefix (writeBusUnavailable, packages/envoy/cmd/listener/api.go).
// Message is that error, the listener's words as they came, which name the subject the grant does
// not cover and print only as the rule reads them; it is bounded by
// refusalBodyLimit as the refusal is. Sending it again changes nothing until an operator changes the
// listener's grant. Any other 403 (one whose error does not open with the prefix, or whose body is
// not JSON, as a proxy in front of the listener could write) stays a plain refusal, retried.
type ErrPublishDenied struct {
	Message oauth2call.RemoteText
}

// publishDeniedPrefix mirrors the listener's publishDeniedPrefix (packages/envoy/cmd/listener/api.go),
// the opening of every answer it gives a publish its own NATS grant denies. Every listener since
// sjawhar/legion#2167, which added the prefix, writes it, so a daemon that takes this release before
// its listeners reads their denials too.
const publishDeniedPrefix = "envoy: the message bus denied the publish under this listener's own permissions"

func (e ErrPublishDenied) Error() string {
	return fmt.Sprintf("the listener's NATS grant denied the forward (%s)", e.Message)
}

// RoleTopicPrefix is the Envoy subject a role token is reached on (ROLE_TOPIC_PREFIX,
// packages/contracts/src/subject.ts): a notice goes to its architect's, and a review-stuck notice
// names its reviewer's, so the architect need not build it.
const RoleTopicPrefix = "notifications.role."

// ControllerTopic is the topic of project's controller, the one notice topic the daemon publishes
// to; a workflow notice goes to the role topic of the architect that owns its issue instead (the
// daemon's notice executor), since every issue's topic is a subject its phase workers subscribe
// to. It carries the controller notices (record.ControllerNotice), each an outbox row of its own,
// and only the kinds named here (#4065): a root in triage handed to Legion (`triage`, admission);
// the slot wakes admission sends through wakeController, `slot-free` and `todo`, on which the
// controller runs its prioritization pass and its deployment's intake, and `tick`, its periodic
// wake (admit.Admission.wakeController and its callers say when; the tick alone carries
// `openCapabilities`, the deployment capabilities with no decision,
// admit.Admission.ReportCapabilities); a tree of the project that has stopped (`tree-stalled`,
// admit's tellStalledTrees, once for each stall); a root whose own architect is not taking the
// work the daemon states it is owed (`architect-stalled`, admit's wakeStalledArchitects, once for
// each arm and run of its tree); a person's decision on a tree Legion is not
// running (`parked-decision`, the outbox's parkDecision and workflow's relayParkedDecision); a
// hold the tree's architect escalated (`held` with reason `escalated`, workflow's retryOrEscalate);
// a tree architect's own failed claim while its root is not done (`worker-died`, workflow's
// claimFailed), and the stall of that architect whose replacement failed its claim over a stop
// the runtime did not confirm (`worker-stalled`, the daemon's enqueueStallNotice), both because
// every other notice of the tree reaches that architect; an issue whose pod an update round
// cannot replace (`update-stalled`, the daemon's reconcilePodUpdates); a start the session store
// could not answer (`session-store-fault`, workflow's
// sessionStoreFault); a claim whose turns keep ending in a provider error (`provider-errors`,
// supervise's tellOfProviderErrors), which the issue's architect is told as well, unless the run
// is the tree root's architect's own, and a child's own architect's run is told to the architect
// above that child; one of the controller's own programs its pod's memory guard ended
// (`memory-guard-kill`, workflow's controllerMemoryGuardKill, once for each kill); the messages
// the intake gave up, counted in one notice that waits a minute for the give-ups after its first
// (`intake-gave-up`, record's Postgres.GiveUpIntakeMessage), which the controller replays or
// discards (its replay_give_up and discard_give_up); and the
// operator's pause and resume of the project (`project-paused` and `project-resumed`,
// record.ProjectPause, queued in the store's Pause and Resume), of which the controller's own pause
// or resume tells it nothing. A free slot alone is no reason for the controller to take an issue:
// work reaches Legion only when a person hands it over or a role reports a clear defect the
// controller files (skill://legion-controller, "Free slots"). It sits among the issue topics, where
// no issue key (`[A-Z][A-Z0-9]*-[0-9]+`) can be `controller`, and the plugin's controller registers
// it as an interest while it holds the controller role (legionControllerNoticeSubject,
// packages/contracts/src/subject.ts). project is the project token panes are told as LEGION_PROJECT
// (packages/pi-legion/src/controller-session.ts), never the Dispatch project key. The stream
// retains it and the controller's session receives it from its own JetStream consumer, whose filter
// that registration carries the lane into (Envoy's bus.ReachesPrivateStreamSubject lets the
// project's controller role holder register it, and no other session), so a notice published while
// that session was away is delivered when it registers again, its consumer keeping the place it
// acknowledged. A session new to the lane is given a consumer at the stream's end, so a freshly
// started controller is handed none of what came before it and learns that from `legion state` and
// Dispatch.
func ControllerTopic(project string) string {
	return "notifications.legion." + project + ".controller"
}

// Publisher delivers a notice to one listener topic. Publish asks the listener to keep nothing
// for a role no live session holds, and PublishKept asks it to keep the message for the role's
// next holder and says whether it did.
type Publisher interface {
	Publish(ctx context.Context, topic, message string, payload any, dedupeKey string) error
	PublishKept(ctx context.Context, topic, message string, payload any, dedupeKey string) (kept bool, err error)
}

// HTTPPublisher calls the Envoy listener's publish route.
type HTTPPublisher struct {
	baseURL string
	token   string
	client  *http.Client
}

// New creates a publisher for one Envoy listener at baseURL, each route joined onto its path
// (newRequest). token is the listener's bearer (`envoy_token_file`), sent on every call; "" sends
// none, for a listener that requires none. It follows no redirect (redirects.Refused): the listener
// answers none, and a followed one would hand the bearer to the host the Location names. A 3xx is
// the listener's answer, refused by its status, and a request that got no answer names its failure
// through oauth2call.Unanswered.
func New(baseURL, token string) *HTTPPublisher {
	return &HTTPPublisher{baseURL: baseURL, token: token, client: redirects.Refused(&http.Client{Timeout: requestTimeout})}
}

// newRequest is the request of method to ref, a route with or without its own query, carrying
// body and the listener's bearer where the publisher has one. It goes to ref joined onto the
// listener URL's path (urljoin.Join), the URL's query kept ahead of ref's own, where ref appended
// to the URL's text would sit inside its query or its fragment and the listener would be asked
// another path. A URL that does not parse or names no host is refused in fixed words: url.Parse's
// error quotes the URL whole, user information and query included. A joined URL http.NewRequest
// refuses (url.URL.String can write text url.Parse refuses) is named through oauth2call.Unanswered.
func (p *HTTPPublisher) newRequest(ctx context.Context, method, ref string, body io.Reader) (*http.Request, error) {
	call, err := urljoin.Join(p.baseURL, ref)
	if err != nil {
		return nil, errors.New("the Envoy listener's URL is not an absolute URL with a host (it is not printed)")
	}
	request, err := http.NewRequestWithContext(ctx, method, call.String(), body)
	if err != nil {
		return nil, oauth2call.Unanswered(err, nil, "")
	}
	if p.token != "" {
		request.Header.Set("Authorization", "Bearer "+p.token)
	}
	return request, nil
}

// Publish posts the listener envelope that preserves the outbox row's idempotent delivery key.
//
// It sends keep: false, so the listener keeps nothing for a role no live session holds, and its
// 404 for a role nobody holds is ErrNoHolder. PublishKept is the other choice: it asks the
// listener to keep the message for the role's next live holder.
func (p *HTTPPublisher) Publish(ctx context.Context, topic, message string, payload any, dedupeKey string) error {
	_, err := p.publish(ctx, topic, message, payload, dedupeKey, false)
	return err
}

// PublishKept posts the envelope as Publish does, but with keep: true: a role message that no
// live session holds the role for is kept on the role's kept lane for one hour and handed to the
// role's next live holder. The listener says a sender tells its answers apart by status alone
// (roleSend.kept, packages/envoy/cmd/listener/confirmed.go), and this reads them so:
//
//   - 200 is a live holder that took the message: not kept, and no error.
//   - 202 is a message the listener kept: kept, and no error, the listener being its keeper. The
//     listener of cybermonklabs/legion#4079 keeps it whether nobody claims the role (kept_reason
//     unclaimed) or its claim's holder is not live (holder_lapsed); a listener before #4079 keeps
//     it only for a role nobody claims, the kept lane sjawhar/legion#2588 shipped.
//   - 404 holder_lapsed is what a listener before #4079 answers a claim whose holder is not live
//     — the claim a released role leaves standing until the listener's reaper frees it — keeping
//     nothing, as it answers keep: false: ErrHolderUnresponsive for a holder_state of
//     unresponsive, and ErrNoHolder for any other state, gone among them.
//   - 404 unclaimed or holder_lapsed is also what a listener that keeps nothing for a publish
//     naming no source session answers a role no live session holds, since no notice of an expired
//     copy could reach the sender; this publisher names none, so against such a listener this is
//     the answer for every role with no live holder, read as above (ErrNoHolder or
//     ErrHolderUnresponsive), and the caller is the message's keeper.
//   - Every other answer is the error Publish makes of it.
//
// A repeat of the same dedupe key while its copy is still on the lane inside its hour is answered
// with that copy and stores nothing new. Once the listener has handed the copy to the role's
// holder, a repeat — a caller's retry after a 202 it never read — is forwarded again, so a kept
// publish reaches its holder at least once, and can reach it twice.
func (p *HTTPPublisher) PublishKept(ctx context.Context, topic, message string, payload any, dedupeKey string) (bool, error) {
	return p.publish(ctx, topic, message, payload, dedupeKey, true)
}

// publish is the one body of Publish and PublishKept: it posts the envelope with keep as the
// request's keep field, and answers whether the listener kept the message rather than
// delivering it. Only a publish that asked to keep reads a 202 as kept; every other answer that
// is not a 200 is the refusal it maps to (publishRefusal).
func (p *HTTPPublisher) publish(ctx context.Context, topic, message string, payload any, dedupeKey string, keep bool) (bool, error) {
	// The listener carries a payload as a JSON document in a string
	// (listenerwire.MessageBody.Payload); an object there is refused as invalid JSON.
	encoded, err := json.Marshal(payload)
	if err != nil {
		return false, fmt.Errorf("encode notice payload for %s: %w", topic, err)
	}
	payloadText := string(encoded)
	body, err := json.Marshal(listenerwire.PublishRequest{
		MessageBody: listenerwire.MessageBody{Message: message, Payload: &payloadText, DedupeKey: dedupeKey},
		Topic:       topic,
		Keep:        &keep,
	})
	if err != nil {
		return false, fmt.Errorf("encode notice for %s: %w", topic, err)
	}
	request, err := p.newRequest(ctx, http.MethodPost, "/v1/messages/publish", bytes.NewReader(body))
	if err != nil {
		return false, fmt.Errorf("build notice publish request for %s: %w", topic, err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := p.client.Do(request)
	if err != nil {
		return false, fmt.Errorf("publish notice to %s: %w", topic, oauth2call.Unanswered(err, request, ""))
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusOK {
		return false, nil
	}
	if keep && response.StatusCode == http.StatusAccepted {
		return true, nil
	}
	answer, cut, readErr := listenerAnswer(response, refusalBodyLimit)
	if readErr != nil {
		// Part of a refusal is no refusal: which one it was, a role nobody holds among them,
		// is unknown, so it is the failure a request that never got its answer is.
		return false, unreadAnswer("publish notice to "+topic, oauth2call.StatusLine(response.StatusCode), readErr)
	}
	refusal := fmt.Errorf("publish notice to %s: listener returned %s: %s", topic, oauth2call.StatusLine(response.StatusCode), excerpt(answer, cut))
	return false, publishRefusal(response.StatusCode, answer, refusal)
}

// publishRefusal is the error a refused publish reads as: refusal, wrapping the error its body
// names where the body is the listener's own refusal for its status (listenerwire). A 404 is
// listenerwire.RoleRefusal (writeRoleHolderError, packages/envoy/cmd/listener): ErrNoHolder, or
// ErrHolderUnresponsive for a lapsed holder that is only unresponsive. A 504 is
// listenerwire.RoleSendFailure: ErrNotDelivered for either of its reasons. A 403 is
// listenerwire.CapabilityRefusal, ErrRefusedByHolder for reason capability, or else the
// listenerwire.APIError of a forward the listener's own NATS grant denied, ErrPublishDenied. A
// listener of an earlier release leaves some members out, which decodes as their zero values; a
// body that is not that JSON, as a proxy in front of the listener could write, and every other
// status, is the plain refusal. Which refusal it is, is decided on the body as it came (the
// listener's own shape, decoded from answer), never on the rule's reading of it, which is not
// readable whole where an error names a URL beside the JSON quote that ends it; what an error
// names of the body - a holder, an event id, the listener's error - is held as it came, each
// shape read through its listenerwire form with its words held as oauth2call.RemoteText
// (RoleSendFailureAs, CapabilityRefusalAs, APIErrorAs), so a caller decides on it as it came
// (RemoteText.Equal, HasPrefix) and it prints as the rule reads it.
func publishRefusal(status int, answer oauth2call.Answer, refusal error) error {
	switch status {
	case http.StatusNotFound:
		var lapsed listenerwire.RoleRefusal
		if answer.Decode(&lapsed) != nil {
			return refusal
		}
		switch {
		case lapsed.Reason == ReasonHolderLapsed && lapsed.HolderState == StateUnresponsive:
			return fmt.Errorf("%w: %w", refusal, ErrHolderUnresponsive)
		case roleholder.IsNoHolderReason(string(lapsed.Reason)):
			return fmt.Errorf("%w: %w", refusal, ErrNoHolder)
		}
	case http.StatusGatewayTimeout:
		var failure listenerwire.RoleSendFailureAs[oauth2call.RemoteText]
		if answer.Decode(&failure) == nil {
			if reason, named := failure.Reason.OneOf(ReasonReceiptTimeout, ReasonDeliveryFailed); named {
				return fmt.Errorf("%w: %w", refusal, ErrNotDelivered{Reason: reason, Holder: failure.Holder, EventID: failure.EventID})
			}
		}
	case http.StatusForbidden:
		var capability listenerwire.CapabilityRefusalAs[oauth2call.RemoteText]
		if answer.Decode(&capability) == nil && capability.Reason.Equal("capability") {
			return fmt.Errorf("%w: %w", refusal, ErrRefusedByHolder{Holder: capability.Holder})
		}
		var denied listenerwire.APIErrorAs[oauth2call.RemoteText]
		if answer.Decode(&denied) == nil && denied.Error.HasPrefix(publishDeniedPrefix) {
			return fmt.Errorf("%w: %w", refusal, ErrPublishDenied{Message: denied.Error})
		}
	}
	return refusal
}

// SessionDeleted is what the listener did with a conditional clean stop: Deleted is false only
// for a registration the listener kept because the condition the stop carried refused it, and
// Reason is the listener's own words for why (listenerwire.DeleteSessionResponse,
// packages/envoy/cmd/listener/api.go), held as they came, which print only as the rule reads
// them. A listener of an earlier release, which judges no
// condition and answers 200 with no body, deleted the registration and says nothing, which reads
// as Deleted with an empty Reason.
type SessionDeleted struct {
	Deleted bool
	Reason  oauth2call.RemoteText
}

// SessionRegistration is what the listener's registry holds for one session id at the moment it
// was read (GET /v1/registry/<session_id>?view=registration, read as
// listenerwire.SessionRegistrationAs). Registered is false for the 404 that route answers: a
// listener carrying that selector writes it only where the session bucket holds no record at all,
// and one predating it writes it for a record past the gone horizon as well, since the full answer
// it falls back to still judges the record's state. ClientInstanceID names the client process
// whose registration it is, empty for an entry a client predating that field wrote and for every
// answer a listener predating the field gives: the listener's words as they came, which the delete
// sends back (DeleteConditions) and which print only as the rule reads them. LastSeen is that
// registration's own write in unix milliseconds.
type SessionRegistration struct {
	Registered       bool
	ClientInstanceID oauth2call.RemoteText
	LastSeen         int64
}

// answerBodyLimit bounds the body of a listener's answer this client decodes (a 200):
// ReadSessionRegistration's entry, and RoleHolder's live holder. It is deliberately not
// refusalBodyLimit, which bounds an error body. A listener that predates the registration view
// selector ignores it and answers the whole session row — its topics, its roles, its
// capabilities — and a role lookup's 200 carries the holder's title, dir and capabilities; nothing
// bounds a long-lived session's topic count or capability list, so an answer cut off at 4 KiB would
// fail to decode, and the stop of exactly the busiest sessions would be reported as a read that
// failed and send no delete at all, and a forget the role lookup admits would be refused as a
// lookup the listener never answered. It is bounded at all because the read drains the body, so
// the connection is reused for the call that follows rather than dropped mid-answer, and an
// unbounded read of a body this daemon did not write is memory it cannot refuse. 1 MiB is far past
// the largest answer the listener writes and far short of a body that could cost the daemon
// anything.
const answerBodyLimit = 1 << 20

// ReadSessionRegistration reads what the listener's registry holds for session, the read the
// daemon's clean stop makes before it deletes anything: the delete it then sends names the
// client instance the entry carries, so it can only take that process's registration and never
// its successor's (DeleteSession).
//
// The read asks include=unresponsive and view=registration together: view=registration is what a
// listener carrying it answers from the session bucket alone, cheaply and whatever state the
// record is in, while include=unresponsive is what a listener predating the selector needs in
// order to answer a record past its live window at all — it ignores the unknown selector and
// answers the full row, which carries no client_instance_id, so against that listener the stop
// rests on the moment alone. Which probe judged the death decides whether that matters: the one
// the shim's closing stream starts reads the record moments after the death, while it is still
// live, while the resync tick's can be a probe interval later, past the live window, and it is
// for the second that include=unresponsive is sent at all.
//
// A 404 is no failure and no error: there is no registration for the stop to name, which is the
// outcome the stop wanted, and Registered is false. Every other status is an error naming what
// the listener said, as a refused delete is, and the caller then sends nothing (sessionStop.send
// in packages/daemon/internal/daemon/daemon.go says why).
func (p *HTTPPublisher) ReadSessionRegistration(ctx context.Context, session string) (SessionRegistration, error) {
	request, err := p.newRequest(ctx, http.MethodGet, "/v1/registry/"+url.PathEscape(session)+"?include=unresponsive&view=registration", nil)
	if err != nil {
		return SessionRegistration{}, fmt.Errorf("build the registration read for %s: %w", session, err)
	}
	response, err := p.client.Do(request)
	if err != nil {
		return SessionRegistration{}, fmt.Errorf("read the registration of session %s: %w", session, oauth2call.Unanswered(err, request, ""))
	}
	defer response.Body.Close()
	switch response.StatusCode {
	case http.StatusOK:
		// Read whole within answerBodyLimit and then decoded, rather than decoded off the
		// stream: the read drains the body, so this connection is reused for the delete that
		// follows.
		answer, _, err := listenerAnswer(response, answerBodyLimit)
		if err != nil {
			return SessionRegistration{}, fmt.Errorf("read the registration of session %s: %w", session, err)
		}
		var entry listenerwire.SessionRegistrationAs[oauth2call.RemoteText]
		if err := answer.Decode(&entry); err != nil {
			return SessionRegistration{}, fmt.Errorf("read the registration of session %s: %w", session, err)
		}
		return SessionRegistration{Registered: true, ClientInstanceID: entry.ClientInstanceID, LastSeen: entry.LastSeen}, nil
	case http.StatusNotFound:
		return SessionRegistration{}, nil
	}
	answer, cut, _ := listenerAnswer(response, refusalBodyLimit)
	return SessionRegistration{}, fmt.Errorf("read the registration of session %s: listener returned %s: %s",
		session, oauth2call.StatusLine(response.StatusCode), excerpt(answer, cut))
}

// RoleHolder reads the session the listener names as role's holder, with that holder's state (GET
// /v1/roles/<role>, roleGetHandler and writeRoleHolderError in packages/envoy/cmd/listener): on the
// 200 the live holder, its state `live`; on the 404 of a lapsed holder, that holder and its
// `holder_state`, `unresponsive` or `gone`, since the claim stands until the role claim reaper
// takes it, or `lapsed` where an older listener names no state; on the 404 of a role no claim
// names, reason roleholder.Unclaimed, no holder at all. The 200 is read as
// listenerwire.RoleHolderAs and the 404 as listenerwire.RoleRefusalAs, each with its words and the
// holder's state held as oauth2call.RemoteText, decoded from the answer as it came: the holder is
// the listener's words, which a caller compares as they came (RemoteText.Equal) and prints only as
// the rule reads them, and the state is one of the listener's own words or, for any other text,
// the rule's reading of it (roleHolderState). A 200 is read within answerBodyLimit, an error body
// within refusalBodyLimit. Every other answer is an error, which the caller reads as
// an answer it does not have: the daemon's forget of a session its claim does not hold refuses on
// one, and on every holder it names that is that session (supervise.Deps.RoleHolder). An answer
// the listener gives the same way however often it is asked — a body read whole that does not
// decode, a 404 naming no holder under any reason but unclaimed, and any status but 200, 404 and
// the transient ones — is a *RoleLookupRefused, a fault an operator fixes; a listener it cannot
// reach, an answer whose body could not be read to its end (unreadAnswer), and a 429, 502, 503
// or 504 (writeBusUnavailable's 503 among them) are plain errors that asking again can clear. The
// listener answers from a direct get of its role-claim bucket, which a lagging copy of the bucket
// can answer for the milliseconds after a claim write: the race any read of the bucket has, and
// a forget made on such an answer is no worse than one made a moment earlier (review round 16 of
// #4220, #64).
func (p *HTTPPublisher) RoleHolder(ctx context.Context, role string) (holder oauth2call.RemoteText, state string, err error) {
	request, err := p.newRequest(ctx, http.MethodGet, "/v1/roles/"+url.PathEscape(role), nil)
	if err != nil {
		return holder, "", fmt.Errorf("build the role lookup for %s: %w", role, err)
	}
	response, err := p.client.Do(request)
	if err != nil {
		return holder, "", fmt.Errorf("look up the holder of role %s: %w", role, oauth2call.Unanswered(err, request, ""))
	}
	defer response.Body.Close()
	refused := func(reason string) error {
		return fmt.Errorf("look up the holder of role %s: %w", role, &RoleLookupRefused{Status: response.StatusCode, Reason: reason})
	}
	if response.StatusCode == http.StatusOK {
		answer, _, readErr := listenerAnswer(response, answerBodyLimit)
		if readErr != nil {
			return holder, "", unreadAnswer("look up the holder of role "+role, oauth2call.StatusLine(response.StatusCode), readErr)
		}
		var live listenerwire.RoleHolderAs[oauth2call.RemoteText, oauth2call.RemoteText]
		if answer.Decode(&live) != nil || live.Holder.IsEmpty() {
			return holder, "", refused("the listener's 200 names no holder")
		}
		return live.Holder, roleHolderState(live.HolderState, StateLive), nil
	}
	answer, cut, readErr := listenerAnswer(response, refusalBodyLimit)
	switch response.StatusCode {
	case http.StatusNotFound:
		if readErr != nil {
			return holder, "", unreadAnswer("look up the holder of role "+role, oauth2call.StatusLine(response.StatusCode), readErr)
		}
		var refusal listenerwire.RoleRefusalAs[oauth2call.RemoteText, oauth2call.RemoteText, oauth2call.RemoteText]
		if answer.Decode(&refusal) != nil {
			return holder, "", refused("the listener's 404 is unreadable: " + excerpt(answer, cut))
		}
		if !refusal.Holder.IsEmpty() {
			return refusal.Holder, roleHolderState(refusal.HolderState, StateLapsed), nil
		}
		if refusal.Reason.Equal(string(roleholder.Unclaimed)) {
			return holder, "", nil
		}
		return holder, "", refused(fmt.Sprintf("the listener's 404 names no holder and gives reason %q, not %q: %s", refusal.Reason, roleholder.Unclaimed, excerpt(answer, cut)))
	case http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return holder, "", fmt.Errorf("look up the holder of role %s: listener returned %s: %s", role, oauth2call.StatusLine(response.StatusCode), excerpt(answer, cut))
	}
	return holder, "", refused(fmt.Sprintf("listener returned %s: %s", oauth2call.StatusLine(response.StatusCode), excerpt(answer, cut)))
}

// The states a role lookup names its holder in (RoleHolder): the listener's session states, live,
// unresponsive and gone (listenerwire.SessionState), and lapsed, which an older listener's 404
// gives by naming its lapsed holder with no state.
const (
	StateLive   = "live"
	StateGone   = "gone"
	StateLapsed = "lapsed"
)

// roleHolderState is the holder state a role lookup's answer names (said), as a message may print
// it: the listener's word where said is one of its states, compared as it came (RemoteText.OneOf);
// absent where said is empty; and the rule's reading of any other text, which nothing decides on.
func roleHolderState(said oauth2call.RemoteText, absent string) string {
	if state, named := said.OneOf(StateLive, StateUnresponsive, StateGone); named {
		return state
	}
	if said.IsEmpty() {
		return absent
	}
	return said.String()
}

// RoleLookupRefused is a role lookup the listener answered the same way however often it is
// asked (RoleHolder): a wrong bearer (401, 403), a claim record it cannot decode (500), a body read
// whole that does not decode, or a 404 naming no holder under a reason it does not know. Status is
// the answer's HTTP status and Reason what was wrong with it. Asking again changes nothing until
// an operator fixes the listener or the daemon's bearer, so a caller that retries on a backoff
// logs it as the fault it is rather than as a wait.
type RoleLookupRefused struct {
	Status int
	Reason string
}

func (e *RoleLookupRefused) Error() string {
	return fmt.Sprintf("the Envoy listener refuses the role lookup (status %d): %s", e.Status, e.Reason)
}

// excerpt is answer's words as an error names them, where cut says whether its read was cut
// (listenerAnswer): as the rule prints them, with the bearer the call carried redacted, since
// whatever answers in the listener's place can echo it, a 2xx answer's words among them; trimmed,
// and no longer than refusalBodyLimit. They are cut only once the URL rule has read them:
// redact.Read drops the run a read that was cut stopped inside where it holds a "://", as a URL
// whose "@" was never read does, and redact.Bytes cuts what the rule leaves where that is longer
// than refusalBodyLimit.
func excerpt(answer oauth2call.Answer, cut bool) string {
	text := redact.Read(answer.Words().String(), cut)
	if len(text) > refusalBodyLimit {
		text, _ = redact.Bytes(text, refusalBodyLimit)
	}
	return strings.TrimSpace(text)
}

// listenerAnswer is the listener's answer to response, at most limit bytes, read through
// oauth2call.ReadAnswer, which a caller decides on and decodes as it came (Answer.Decode:
// publishRefusal, ReadSessionRegistration, RoleHolder, DeleteSession) and quotes in an error only
// through excerpt, and whether it was cut. A body past limit is its first limit bytes, cut; a read
// that failed is the read's error, and cut too: it stopped where the error came, which is as
// unknown a place as the bound's (redact.Read).
func listenerAnswer(response *http.Response, limit int64) (answer oauth2call.Answer, cut bool, err error) {
	answer, err = oauth2call.ReadAnswer(response, limit)
	cut = errors.Is(err, oauth2call.ErrAnswerTooLarge)
	if err != nil && !cut {
		return oauth2call.Answer{}, true, err
	}
	return answer, cut, nil
}

// unreadAnswer is a listener answer of status whose body could not be read to its end (err), for
// the call what names: a connection reset mid-body, or the caller's deadline between the headers
// and the body. What the listener said is unknown, so it is a plain error that asking again can
// clear, never a refusal or an outcome read from part of a body (RoleHolder, Publish,
// DeleteSession).
func unreadAnswer(what, status string, err error) error {
	return fmt.Errorf("%s: the listener answered %s, and its answer could not be read to its end: %w", what, status, err)
}

// DeleteConditions is what one clean stop of the session of a role whose process has ended — a
// death a probe judged, or a suspension's stop — holds under, and where this daemon's code argues
// the rule in full: its other comments point here.
//
// The delete must take the ended process's registration and never the registration a relaunch of
// the same claim writes under the same session id — the relaunch of a death in the same decision,
// or a later wake's resume of a suspended claim — so it carries both conditions at once. The
// listener keeps the entry when either refuses it (session.DeleteConditions.keeps,
// packages/envoy/internal/session/delete.go), so neither widens what the other admits; each is
// there to make the other's weakness bearable.
//
// NotRegisteredAfter is the moment the machine told the daemon the process had ended — its probe
// judged it gone, or the runtime's stop of a suspension returned — and rides as
// if_not_registered_after with nothing subtracted from it. Either comes after the process's last
// heartbeat, so on clocks that agree the listener stamped that heartbeat at or before this moment
// and the registration goes; a listener clock ahead of this daemon's by more than the time between
// that heartbeat and the moment keeps it. A moment dated earlier, to cover a clock, puts that
// heartbeat after it and refuses the stop that is owed, because UpdatedAt is the listener's own
// stamp at every write (packages/envoy/internal/session/registry.go) and tells an ended process's
// heartbeat from a successor's registration not at all.
//
// ClientInstanceID is the id the entry itself carried when the stop read it
// (ReadSessionRegistration), and names that one client process, which no comparison of moments
// can. It is what makes carrying the moment undated safe: where the read answered the ended
// process's own entry, a relaunch's registration is refused by its own identity, whatever the
// daemon's clock and the listener's say about the moment. Where the read was answered after the
// relaunch had registered, the id handed back is the successor's and only the moment keeps that
// registration — the listener judges both at the revision it deletes, under its own
// compare-and-swap (session.SessionRegistry.DeleteIf), so what is stored when the delete lands
// decides it, whatever this daemon knew when it sent it. Against a listener that answers no
// client_instance_id at all the id is empty and the moment judges alone, which is the whole of
// the condition until that listener is upgraded.
//
// An empty ClientInstanceID is left out of the request rather than sent empty: the listener
// compares no instance for an empty value as for an absent one (session.InstanceMatches), so the
// two are the same delete and the shorter query states only what the stop really holds. The id is
// the listener's words, which the delete sends back only where the rule prints them as they came
// (RemoteText.SendBack). An id the rule would change - one sharing a run of the request's bearer -
// is one the delete cannot name, and a delete that left it out would hold under the moment alone,
// weaker than the two conditions this argues for, with no one told: so no delete is sent, and
// DeleteSession answers an error, as the stop answers a read that failed.
type DeleteConditions struct {
	ClientInstanceID   oauth2call.RemoteText
	NotRegisteredAfter time.Time
}

// DeleteSession removes session's registration from the listener's registry (DELETE
// /v1/sessions/<session>), the clean stop the daemon sends for a Legion role whose agent process
// has ended, found dead or stopped by a suspension. The listener then reads the session `gone` at
// once rather than at the end of its live window, so its issue claims are free without the wait a
// stopped process would otherwise cost (dispatch://INFRA-392), and a message to it raises the wake
// rather than being stored for a session nothing reads. The role claim it held stays until the listener's role claim reaper
// finds the claim unanswered for the live window (store.Registry.ClaimRenewedWithin). A
// registry that holds no record for the session answers 404: it is already gone, which is the
// outcome asked for, so that is no failure.
//
// The two conditions it carries, and why neither alone would do, are on DeleteConditions. A
// non-empty client instance id the rule would change sends no delete and answers an error that
// names none of it.
func (p *HTTPPublisher) DeleteSession(ctx context.Context, session string, conditions DeleteConditions) (SessionDeleted, error) {
	target := fmt.Sprintf("/v1/sessions/%s?if_not_registered_after=%d", url.PathEscape(session), conditions.NotRegisteredAfter.UnixMilli())
	if !conditions.ClientInstanceID.IsEmpty() {
		id, named := conditions.ClientInstanceID.SendBack()
		if !named {
			return SessionDeleted{}, fmt.Errorf("delete session %s: the registration names a client instance id the redaction rule would change, which the delete cannot send back, so no delete was sent (the id is not printed)", session)
		}
		target += "&client_instance_id=" + url.QueryEscape(id)
	}
	request, err := p.newRequest(ctx, http.MethodDelete, target, nil)
	if err != nil {
		return SessionDeleted{}, fmt.Errorf("build the session delete for %s: %w", session, err)
	}
	response, err := p.client.Do(request)
	if err != nil {
		return SessionDeleted{}, fmt.Errorf("delete session %s: %w", session, oauth2call.Unanswered(err, request, ""))
	}
	defer response.Body.Close()
	switch response.StatusCode {
	case http.StatusOK:
		// Only an answer that says whether it deleted is read as one: a 200 with no body at
		// all is the unconditional delete of a listener that judges no condition, which
		// removed the registration, and so is any other body this route never writes. A body
		// that could not be read to its end is neither: it may have said the listener kept the
		// registration, so what the stop did is unknown (unreadAnswer).
		answer, _, readErr := listenerAnswer(response, refusalBodyLimit)
		if readErr != nil {
			return SessionDeleted{}, unreadAnswer("delete session "+session, oauth2call.StatusLine(response.StatusCode), readErr)
		}
		var kept listenerwire.DeleteSessionResponseAs[oauth2call.RemoteText]
		if answer.Decode(&kept) == nil && kept.Deleted != nil {
			return SessionDeleted{Deleted: *kept.Deleted, Reason: kept.Reason}, nil
		}
		return SessionDeleted{Deleted: true}, nil
	case http.StatusNoContent, http.StatusNotFound:
		return SessionDeleted{Deleted: true}, nil
	}
	answer, cut, _ := listenerAnswer(response, refusalBodyLimit)
	return SessionDeleted{}, fmt.Errorf("delete session %s: listener returned %s: %s", session, oauth2call.StatusLine(response.StatusCode), excerpt(answer, cut))
}
