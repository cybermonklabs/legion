// Package notify is the daemon's client for the Envoy listener: it publishes persisted workflow
// notices through its publish route, and ends a session's registration through its session route.
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

	"github.com/sjawhar/envoy/roleholder"
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

// RoleRefusal is the body the listener writes when it refuses a role publish or a role lookup
// (writeRoleHolderError and the publish route's refusals, packages/envoy/cmd/listener). Each
// reader takes the members its own decision needs and ignores the rest; a listener of an earlier
// release leaves some of them out, which decodes as their zero values.
type RoleRefusal struct {
	Error       string `json:"error"`
	Reason      string `json:"reason"`
	Holder      string `json:"holder"`
	HolderState string `json:"holder_state"`
	// LastSeen is the holder's last registration in unix milliseconds, absent when the listener
	// can no longer name one.
	LastSeen *int64 `json:"last_seen"`
	EventID  string `json:"event_id"`
}

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
// "" when it resolved none; EventID names the envelope it forwarded. Only a listener
// that confirms a role message before it answers gives it. An earlier listener answers 200 once
// the NATS server took the message and reports a forward that then fails as a role-lane exception
// (notifications.envoy.exceptions.notifications.role.*), so a caller that acts on this answer
// still has to hear those reports while such a listener runs. A 504 without one of the two
// reasons is a plain refusal, never ErrNotDelivered.
type ErrNotDelivered struct {
	Reason  string
	Holder  string
	EventID string
}

func (e ErrNotDelivered) Error() string {
	return fmt.Sprintf("the role's holder %q did not confirm event %s (%s)", e.Holder, e.EventID, e.Reason)
}

// ErrRefusedByHolder is a role publish the listener answered 403 with reason "capability": the
// role's holder, the session Holder names, does not advertise the delivery mode of the payload, or
// the mode cannot be read unambiguously, so the listener forwarded nothing. Sending it again
// changes nothing until another session holds the role. Only a listener that confirms a role
// message before it answers gives it; an earlier listener's role lane drops such a message after
// its 200 without a report.
type ErrRefusedByHolder struct {
	Holder string
}

func (e ErrRefusedByHolder) Error() string {
	return fmt.Sprintf("the role's holder %q does not advertise the delivery mode", e.Holder)
}

// ErrPublishDenied is a role publish the listener answered 403 because the NATS server refused the
// forward under the listener's own publish permissions, so the listener forwarded nothing: its body's
// error opens with publishDeniedPrefix (writeBusUnavailable, packages/envoy/cmd/listener/api.go).
// Message is that error, which names the subject the grant does not cover; it is bounded by
// refusalBodyLimit as the refusal is. Sending it again changes nothing until an operator changes the
// listener's grant. Any other 403 (one whose error does not open with the prefix, or whose body is
// not JSON, as a proxy in front of the listener could write) stays a plain refusal, retried.
type ErrPublishDenied struct {
	Message string
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
// to. It carries the controller notices (record.ControllerNotice), each an outbox row of its own:
// every hold and a tree architect's own failed claim (workflow's noticeWithController), a root in
// triage handed to Legion (admission), a root that gave its admission slot up while its architect
// waits on a person (`awaiting-person`, workflow's enterAwaiting), a root admission took out of
// the workflow because no tree of it runs and nothing would ever start it (`unslotted`, admit's
// releaseUnslotted), a person's decision on a tree Legion is not running (`parked-decision`, the
// outbox's parkDecision and workflow's relayParkedDecision), an issue whose pod an update round
// cannot replace (`update-stalled`, the daemon's reconcilePodUpdates), and the slot wakes
// admission sends through wakeController, `slot-free`, `todo` and `tick`
// (admit.Admission.wakeController and its callers say when; the tick alone carries
// `openCapabilities`, the deployment capabilities with no decision,
// admit.Admission.ReportCapabilities). The controller takes no issue for `slot-free`,
// `todo` or `awaiting-person`: it names the free slots in its summary and report, and work reaches
// Legion only when a person hands it over or a role reports a clear defect the controller files
// (skill://legion-controller, "Free slots"). It sits among the issue topics, where no
// issue key (`[A-Z][A-Z0-9]*-[0-9]+`) can be `controller`, and the plugin's controller subscribes to it
// while it holds the controller role (legionControllerNoticeSubject,
// packages/contracts/src/subject.ts). project is the project token panes are told as LEGION_PROJECT
// (packages/pi-legion/src/controller-session.ts), never the Dispatch project key. It is a live
// wake only: the stream retains it, but an Oh My Pi session subscribes over core NATS and is never
// handed a retained copy, so a controller learns what happened before it started from `legion
// state` and Dispatch.
func ControllerTopic(project string) string {
	return "notifications.legion." + project + ".controller"
}

// Publisher delivers a notice to one listener topic.
type Publisher interface {
	Publish(ctx context.Context, topic, message string, payload any, dedupeKey string) error
}

// HTTPPublisher calls the Envoy listener's publish route.
type HTTPPublisher struct {
	baseURL string
	token   string
	client  *http.Client
}

// New creates a publisher for one Envoy listener. token is the listener's bearer
// (`envoy_token_file`), sent on every publish; "" sends none, for a listener that requires none.
func New(baseURL, token string) *HTTPPublisher {
	return &HTTPPublisher{baseURL: strings.TrimRight(baseURL, "/"), token: token, client: &http.Client{Timeout: requestTimeout}}
}

// Publish posts the listener envelope that preserves the outbox row's idempotent delivery key.
//
// It sends keep: false, so the listener refuses a role nobody holds with its 404 (ErrNoHolder
// below) instead of keeping the message for the role's next holder. The daemon keeps its own
// queue of notices, which does more than a kept copy can: the outbox row is held and each
// architect's notices keep their order behind it, architectEnded finishes a row whose architect
// is gone, a receipt timeout reholds the notice, and a merge-queue row posts a Dispatch message
// when the role is unheld. One message has one keeper, so this sender keeps its own
// (packages/daemon/internal/daemon/outbox.go).
//
// THIS BINARY SHIPS BEFORE THE ENVOY LISTENER THAT CARRIES THE KEPT LANE. The Envoy listener and
// this binary are separate releases (.github/workflows/release-envoy-listener.yaml and
// release.yaml), and a binary of the release before this one sends no keep field at all. Against
// the new listener that publish is answered 202 with the message kept, which the older code reads
// as neither success nor ErrNoHolder: its outbox row is held and keeps retrying while Envoy also
// hands the message to the role's next holder, so the role gets it twice. Deploying this binary
// first leaves no window in which two keepers hold one message. Measured on a live listener by
// INFRA-839's tester on 2026-10-31: the old publisher got `listener returned 202 Accepted` with
// errors.Is(err, ErrNoHolder) false, and the kept lane then held that message.
func (p *HTTPPublisher) Publish(ctx context.Context, topic, message string, payload any, dedupeKey string) error {
	// The listener carries a payload as a JSON document in a string (its messageBody,
	// packages/envoy/cmd/listener/api.go:68-80); an object there is refused as invalid JSON.
	encoded, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode notice payload for %s: %w", topic, err)
	}
	body, err := json.Marshal(struct {
		Topic     string `json:"topic"`
		Message   string `json:"message"`
		Payload   string `json:"payload"`
		DedupeKey string `json:"dedupe_key"`
		Keep      bool   `json:"keep"`
	}{Topic: topic, Message: message, Payload: string(encoded), DedupeKey: dedupeKey, Keep: false})
	if err != nil {
		return fmt.Errorf("encode notice for %s: %w", topic, err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/v1/messages/publish", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build notice publish request for %s: %w", topic, err)
	}
	request.Header.Set("Content-Type", "application/json")
	if p.token != "" {
		request.Header.Set("Authorization", "Bearer "+p.token)
	}
	response, err := p.client.Do(request)
	if err != nil {
		return fmt.Errorf("publish notice to %s: %w", topic, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(response.Body, refusalBodyLimit))
		refusal := fmt.Errorf("publish notice to %s: listener returned %s: %s", topic, response.Status, strings.TrimSpace(string(body)))
		var answer RoleRefusal
		if json.Unmarshal(body, &answer) != nil {
			return refusal
		}
		switch {
		case response.StatusCode == http.StatusNotFound && answer.Reason == ReasonHolderLapsed && answer.HolderState == StateUnresponsive:
			return fmt.Errorf("%w: %w", refusal, ErrHolderUnresponsive)
		case response.StatusCode == http.StatusNotFound && roleholder.IsNoHolderReason(answer.Reason):
			return fmt.Errorf("%w: %w", refusal, ErrNoHolder)
		case response.StatusCode == http.StatusGatewayTimeout && (answer.Reason == ReasonReceiptTimeout || answer.Reason == ReasonDeliveryFailed):
			return fmt.Errorf("%w: %w", refusal, ErrNotDelivered{Reason: answer.Reason, Holder: answer.Holder, EventID: answer.EventID})
		case response.StatusCode == http.StatusForbidden && answer.Reason == "capability":
			return fmt.Errorf("%w: %w", refusal, ErrRefusedByHolder{Holder: answer.Holder})
		case response.StatusCode == http.StatusForbidden && strings.HasPrefix(answer.Error, publishDeniedPrefix):
			return fmt.Errorf("%w: %w", refusal, ErrPublishDenied{Message: answer.Error})
		}
		return refusal
	}
	return nil
}

// SessionDeleted is what the listener did with a conditional clean stop: Deleted is false only
// for a registration the listener kept because the condition the stop carried refused it, and
// Reason is the listener's own words for why (deleteSessionResponse,
// packages/envoy/cmd/listener/api.go). A listener of an earlier release, which judges no
// condition and answers 200 with no body, deleted the registration and says nothing, which reads
// as Deleted with an empty Reason.
type SessionDeleted struct {
	Deleted bool
	Reason  string
}

// SessionRegistration is what the listener's registry holds for one session id at the moment it
// was read (GET /v1/registry/<session_id>?view=registration, sessionRegistration in
// packages/envoy/cmd/listener/api.go). Registered is false for the 404 that route answers: a
// listener carrying that selector writes it only where the session bucket holds no record at
// all, and one predating it writes it for a record past the gone horizon as well, since the
// full answer it falls back to still judges the record's state. ClientInstanceID names the
// client process whose registration it is, empty for an entry a client predating that field
// wrote and for every answer a listener predating the field gives, and LastSeen is that
// registration's own write in unix milliseconds.
type SessionRegistration struct {
	Registered       bool
	ClientInstanceID string
	LastSeen         int64
}

// registrationReadBodyLimit bounds the body ReadSessionRegistration decodes an entry from. It is
// deliberately not refusalBodyLimit, which bounds an error body: a listener that predates the
// view selector ignores it and answers the whole session row here — its topics, its roles, its
// capabilities — and nothing bounds a long-lived session's topic count, so a row cut off at
// 4 KiB would fail to decode and the stop of exactly the busiest sessions would be reported as a
// read that failed and send no delete at all. It is bounded at all because the read drains the
// body, so the connection is reused for the delete that follows rather than dropped mid-answer,
// and an unbounded read of a body this daemon did not write is memory it cannot refuse. 1 MiB is
// far past the largest row the listener writes and far short of a body that could cost the
// daemon anything.
const registrationReadBodyLimit = 1 << 20

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
	target := fmt.Sprintf("%s/v1/registry/%s?include=unresponsive&view=registration", p.baseURL, url.PathEscape(session))
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return SessionRegistration{}, fmt.Errorf("build the registration read for %s: %w", session, err)
	}
	if p.token != "" {
		request.Header.Set("Authorization", "Bearer "+p.token)
	}
	response, err := p.client.Do(request)
	if err != nil {
		return SessionRegistration{}, fmt.Errorf("read the registration of session %s: %w", session, err)
	}
	defer response.Body.Close()
	switch response.StatusCode {
	case http.StatusOK:
		// Read whole within registrationReadBodyLimit and then unmarshalled, rather than
		// decoded off the stream: the read drains the body, so this connection is reused
		// for the delete that follows.
		body, err := io.ReadAll(io.LimitReader(response.Body, registrationReadBodyLimit))
		if err != nil {
			return SessionRegistration{}, fmt.Errorf("read the registration of session %s: %w", session, err)
		}
		var entry struct {
			ClientInstanceID string `json:"client_instance_id"`
			LastSeen         int64  `json:"last_seen"`
		}
		if err := json.Unmarshal(body, &entry); err != nil {
			return SessionRegistration{}, fmt.Errorf("read the registration of session %s: %w", session, err)
		}
		return SessionRegistration{Registered: true, ClientInstanceID: entry.ClientInstanceID, LastSeen: entry.LastSeen}, nil
	case http.StatusNotFound:
		return SessionRegistration{}, nil
	}
	body, _ := io.ReadAll(io.LimitReader(response.Body, refusalBodyLimit))
	return SessionRegistration{}, fmt.Errorf("read the registration of session %s: listener returned %s: %s",
		session, response.Status, strings.TrimSpace(string(body)))
}

// DeleteConditions is what one clean stop of a dead role's session holds under, and where this
// daemon's code argues the rule in full: its other comments point here.
//
// The delete must take the dead process's registration and never the registration a relaunch of
// the same claim writes under the same session id, so it carries both conditions at once. The
// listener keeps the entry when either refuses it (session.DeleteConditions.keeps,
// packages/envoy/internal/session/delete.go), so neither widens what the other admits; each is
// there to make the other's weakness bearable.
//
// NotRegisteredAfter is the moment the daemon's probe judged the process gone, and rides as
// if_not_registered_after with nothing subtracted from it. The probe runs after the dead
// process's last heartbeat, so on clocks that agree the listener stamped that heartbeat at or
// before this moment and the registration goes; a listener clock ahead of this daemon's by more
// than the time between that heartbeat and the probe keeps it. A moment dated earlier than the
// probe's, to cover a clock, puts that heartbeat after it and refuses the stop that is owed,
// because UpdatedAt is the listener's own stamp at every write
// (packages/envoy/internal/session/registry.go) and tells a dead process's heartbeat from a
// successor's registration not at all.
//
// ClientInstanceID is the id the entry itself carried when the stop read it
// (ReadSessionRegistration), and names that one client process, which no comparison of moments
// can. It is what makes carrying the probe's moment undated safe: where the read answered the
// dead process's own entry, a relaunch's registration is refused by its own identity, whatever
// the daemon's clock and the listener's say about the moment. Where the read was answered after
// the relaunch had registered, the id handed back is the successor's and only the moment keeps
// that registration — the listener judges both at the revision it deletes, under its own
// compare-and-swap (session.SessionRegistry.DeleteIf), so what is stored when the delete lands
// decides it, whatever this daemon knew when it sent it. Against a listener that answers no
// client_instance_id at all the id is empty and the moment judges alone, which is the whole of
// the condition until that listener is upgraded.
//
// An empty ClientInstanceID is left out of the request rather than sent empty: the listener
// compares no instance for an empty value as for an absent one (session.InstanceMatches), so the
// two are the same delete and the shorter query states only what the stop really holds.
type DeleteConditions struct {
	ClientInstanceID   string
	NotRegisteredAfter time.Time
}

// DeleteSession removes session's registration from the listener's registry (DELETE
// /v1/sessions/<session>), the clean stop the daemon sends for a Legion role whose agent process
// is gone. The listener then reads the session `gone` at once rather than at the end of its live
// window, so the role claim it held is released and its issue claims are free without the wait a
// stopped process would otherwise cost (dispatch://INFRA-392). A registry that holds no record
// for the session answers 404: it is already gone, which is the outcome asked for, so that is no
// failure.
//
// The two conditions it carries, and why neither alone would do, are on DeleteConditions.
func (p *HTTPPublisher) DeleteSession(ctx context.Context, session string, conditions DeleteConditions) (SessionDeleted, error) {
	target := fmt.Sprintf("%s/v1/sessions/%s?if_not_registered_after=%d",
		p.baseURL, url.PathEscape(session), conditions.NotRegisteredAfter.UnixMilli())
	if conditions.ClientInstanceID != "" {
		target += "&client_instance_id=" + url.QueryEscape(conditions.ClientInstanceID)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodDelete, target, nil)
	if err != nil {
		return SessionDeleted{}, fmt.Errorf("build the session delete for %s: %w", session, err)
	}
	if p.token != "" {
		request.Header.Set("Authorization", "Bearer "+p.token)
	}
	response, err := p.client.Do(request)
	if err != nil {
		return SessionDeleted{}, fmt.Errorf("delete session %s: %w", session, err)
	}
	defer response.Body.Close()
	switch response.StatusCode {
	case http.StatusOK:
		// Only an answer that says whether it deleted is read as one: a 200 with no body at
		// all is the unconditional delete of a listener that judges no condition, which
		// removed the registration, and so is any other body this route never writes.
		body, _ := io.ReadAll(io.LimitReader(response.Body, refusalBodyLimit))
		var kept struct {
			Deleted *bool  `json:"deleted"`
			Reason  string `json:"reason"`
		}
		if json.Unmarshal(body, &kept) == nil && kept.Deleted != nil {
			return SessionDeleted{Deleted: *kept.Deleted, Reason: kept.Reason}, nil
		}
		return SessionDeleted{Deleted: true}, nil
	case http.StatusNoContent, http.StatusNotFound:
		return SessionDeleted{Deleted: true}, nil
	}
	body, _ := io.ReadAll(io.LimitReader(response.Body, refusalBodyLimit))
	return SessionDeleted{}, fmt.Errorf("delete session %s: listener returned %s: %s", session, response.Status, strings.TrimSpace(string(body)))
}
