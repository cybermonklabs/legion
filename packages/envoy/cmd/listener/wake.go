// Wake reports for Legion's released issue pods: the two wakes only this listener sees, and the
// liveness signal that says its silence means "nothing to report" rather than "nothing reporting".

package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/id"
	"github.com/sjawhar/envoy/internal/logging"
)

// The kinds of wake report, as the daemon's decoder reads them
// (packages/daemon/internal/intake/decode.go, decodeListenerWake).
const (
	// wakeKindRole is a message published to a role topic with no live holder. The Legion daemon
	// resumes the released claim of that role token, if one of its own is released. The publish
	// route raises it once, beside the 202 kept or the 404 it answers (reportRoleWake): a kept
	// copy reaches the session the wake brings back inside the copy's hour, and a message sent
	// with keep: false keeps nothing and reaches that session on its sender's next attempt. The
	// sweep raises it again while a copy the listener kept on the role's lane has at least twenty
	// minutes of its hour left and no live session holds the role, at most once for each role in
	// each roleWakeInterval across every listener (publishSweptRoleWake).
	wakeKindRole = "role_message"
	// wakeKindAgent is a message sent to the agent subject of a session nothing can place: an
	// Envoy direct message, or a person's answer, comment or message a Dispatch claim routes to
	// that session.
	wakeKindAgent = "agent_message"
	// wakeKindReady is this listener saying it is alive and reporting, published at its start and
	// every listenerReadyInterval after it. The daemon releases no pod unless it has read one
	// within its own window, because without a positive signal "the listener sends no reports"
	// and "the listener has nothing to report" are the same silence.
	wakeKindReady = "listener_ready"
)

// listenerReadyInterval is how often a running listener republishes its liveness report. The
// daemon's window is three times this, so one report lost to a reconnect costs nothing.
const listenerReadyInterval = 5 * time.Minute

// A wake report is retried until the bus acknowledges it. Losing one is not "one wake late": a
// released architect gets no backstop at all (the daemon arms one for a phase worker only), a
// phase worker whose phase is closed gets none either, and the daemon's stranded report passes
// over a released claim no wake ever touched — so a report dropped during a NATS reconnect
// leaves a tree stalled with nothing saying so. R3 asks for the report to reach the daemon
// across a daemon restart too, which the stream's own retention gives once the publish is
// stored, and a publish that never happened defeats in the same way.
//
// wakeReportAttempt bounds one publish, wakeReportRetry is the first wait between two of them,
// doubling to wakeReportRetryCap, and wakeReportBudget bounds the whole report: past it the
// listener has been unable to reach the stream for minutes, which its own liveness report then
// says, and holding the goroutine longer buys nothing. No caller waits any of it out — a role
// report is published off the path of the 404 or the 202 kept answer it is raised beside, and an
// agent report off its undeliverable message's (reportRoleWake, reportAgentWake).
//
// The sweep's repeated role report is the one report not retried: it is one attempt, bounded by
// wakeReportAttempt, because the next sweep raises it again (publishSweptRoleWake), and it holds
// none of wakeReporter's slots, which the reports raised beside a keep need during a bus outage.
const (
	wakeReportAttempt  = 2 * time.Second
	wakeReportRetry    = 250 * time.Millisecond
	wakeReportRetryCap = 15 * time.Second
	wakeReportBudget   = 5 * time.Minute
)

// wakeReport is the wire shape of a wake report. At is this listener's own clock in epoch
// milliseconds, matching contracts.Envelope's KeptAt, because the stream's store time is the NATS
// server's clock and the daemon measures a wake against its own.
//
// token is not on the wire: it is the project token of the one Legion deployment a role or agent
// report is for (legionRoleToken, agentWakeToken), and the report is published on that project's
// own wake subject, contracts.LegionWakeSubject(token), beside its controller lane, where only that
// project's daemon reads. listener_ready names no project and stays on the one subject every daemon
// shares, bus.LegionWakeTopic (wakeReport.subject), so that subject carries no project's role
// name or session id, and a daemon held to its projects reads only its own wakes.
type wakeReport struct {
	Kind    string `json:"kind"`
	Role    string `json:"role,omitempty"`
	Session string `json:"session,omitempty"`
	At      int64  `json:"at"`
	token   string
}

// subject is the subject report is published on: its project's own wake subject, or the shared
// one for a report of no project (listener_ready).
func (report wakeReport) subject() string {
	if report.token == "" {
		return bus.LegionWakeTopic
	}
	return contracts.LegionWakeSubject(report.token)
}

// isLegionWakeTopic says whether topic is a wake subject: the shared one, or a project's own,
// notifications.legion.<token>.wake. The publish route refuses both, since only the listener
// reports a wake.
func isLegionWakeTopic(topic string) bool {
	if topic == bus.LegionWakeTopic {
		return true
	}
	token, found := strings.CutPrefix(topic, "notifications.legion.")
	if !found {
		return false
	}
	token, found = strings.CutSuffix(token, ".wake")
	return found && token != "" && !strings.ContainsAny(token, ".*>")
}

// legionRoleToken is the project token a Legion role names, its name's second part:
// legion-<token>-…, the shape every Legion claim token and controller role has (the daemon's
// claim.NewToken and claim.ControllerToken, whose token is claim.ProjectToken: lower-case letters
// and digits). It is read from the role as written, never lowercased a second time, so the wake
// subject is spelt as the project's controller lane is. A role of any other shape, a lead role
// among them, is no Legion deployment's, and its token is empty.
func legionRoleToken(role string) string {
	rest, found := strings.CutPrefix(role, "legion-")
	if !found {
		return ""
	}
	token, after, found := strings.Cut(rest, "-")
	if !found || after == "" || token == "" || strings.Trim(token, "abcdefghijklmnopqrstuvwxyz0123456789") != "" {
		return ""
	}
	return token
}

// agentWakeToken is the project token of the session a message on topic was sent to: the project
// the agent subject names, notifications.agent.<P>.<session> (contracts.AgentSubject, which the
// send route publishes a message on), as claim.ProjectToken spells a project key, its lower case.
// A subject of no project (NoProject, or the old form, which names none) is no Legion deployment's,
// and its token is empty.
func agentWakeToken(topic string) string {
	project, _, ok := contracts.SubjectProject(topic)
	if !ok || project == contracts.NoProject || project == "" {
		return ""
	}
	return strings.ToLower(project)
}

// roleWakeInterval is the interval the sweep's repeated wake report of a role is named by
// (sweptRoleWakeID): every listener that sweeps a role's lane inside one interval publishes the
// same report, and the stream, which keeps a message id for 72 hours, stores the first of them,
// so a role is woken at most once an interval however many listeners sweep it. It is the sweep's
// own cadence (main.go), so each pass of one listener's sweep falls in an interval of its own.
const roleWakeInterval = 5 * time.Minute

// wakeReportEnvelope builds the envelope report crosses the bus in, under eventID and dedupeKey,
// and reports false, having logged why, for a report that cannot be encoded or whose envelope
// contracts.Envelope.Validate refuses. It is the one envelope every wake report is published as,
// the retried ones (publishWakeReport) and the sweep's (publishSweptRoleWake).
func wakeReportEnvelope(logger *logging.Logger, report wakeReport, eventID, dedupeKey string) (contracts.Envelope, bool) {
	payload, err := json.Marshal(report)
	if err != nil {
		logger.Warn("legion wake report not encoded", slog.String("kind", report.Kind), slog.String("error", err.Error()))
		return contracts.Envelope{}, false
	}
	summary := "legion wake: " + report.Kind
	if report.Role != "" {
		summary += " " + report.Role
	}
	if report.Session != "" {
		summary += " " + report.Session
	}
	// Every field contracts.Envelope.Validate requires is set here, SourceEventID included: a
	// report that fails Validate is dropped before it is published (below), and a dropped
	// listener_ready leaves every Legion daemon's release gate shut.
	item := contracts.Envelope{
		EventID:        eventID,
		Source:         "envoy",
		SourceEventID:  eventID,
		Topic:          report.subject(),
		DedupeKey:      dedupeKey,
		IssuedAt:       report.At,
		PayloadSummary: summary,
		Payload:        string(payload),
		TraceID:        id.New(),
	}
	if err := item.Validate(); err != nil {
		logger.Warn("legion wake report not valid", slog.String("kind", report.Kind), slog.String("error", err.Error()))
		return contracts.Envelope{}, false
	}
	return item, true
}

// publishWakeReport publishes one report on its subject (wakeReport.subject), retrying until the stream
// acknowledges it, the server refuses it under the listener's grant (bus.ErrPublishDenied, which
// the server gives again however often it is sent, so a retry would only hold one of
// wakeReportsInFlight's slots until the budget ran out), the whole report's budget runs out, or ctx
// ends with the listener. Nothing waits for it: the 404 or the 202 kept answer it was raised beside
// is answered whatever the bus does, and is answered without waiting for it (reportRoleWake).
//
// Every attempt publishes the one envelope built below (wakeReportEnvelope), under the one event
// id and dedupe key, as the stream keeps no duplicate window for these: an attempt the stream
// stored and failed to acknowledge is reported twice, and a wake the daemon has taken already is
// taken no second time (its mark's key is the report's event id, which every attempt of one report
// carries, and the role and agent reports of one message carry the same name whichever attempt the
// daemon reads).
func publishWakeReport(lifetime context.Context, client *bus.Client, logger *logging.Logger, report wakeReport) {
	if client == nil {
		return
	}
	report.At = contracts.NowMillis()
	eventID := id.New()
	item, ok := wakeReportEnvelope(logger, report, eventID, "legion.wake."+eventID)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(lifetime, wakeReportBudget)
	defer cancel()
	for attempt, wait := 1, wakeReportRetry; ; attempt++ {
		if err := publishWithinBudget(ctx, client, item); err == nil {
			logger.Info("legion wake report published", slog.String("kind", report.Kind),
				slog.String("role", report.Role), slog.String("session", report.Session), slog.Int("attempts", attempt))
			return
		} else if ctx.Err() != nil || errors.Is(err, bus.ErrPublishDenied) {
			logger.Warn("legion wake report not published", slog.String("kind", report.Kind),
				slog.String("role", report.Role), slog.String("session", report.Session),
				slog.Int("attempts", attempt), slog.String("error", err.Error()))
			return
		} else {
			logger.Warn("legion wake report not published; retrying until the bus takes it",
				slog.String("kind", report.Kind), slog.String("role", report.Role),
				slog.String("session", report.Session), slog.Int("attempts", attempt), slog.String("error", err.Error()))
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		if wait *= 2; wait > wakeReportRetryCap {
			wait = wakeReportRetryCap
		}
	}
}

// publishWithinBudget makes one publish attempt, bounded by wakeReportAttempt and by what is
// left of the report's whole budget.
func publishWithinBudget(budget context.Context, client *bus.Client, item contracts.Envelope) error {
	ctx, cancel := context.WithTimeout(budget, wakeReportAttempt)
	defer cancel()
	return client.PublishWithin(ctx, item)
}

// sweptRoleWakeID is the message id and the dedupe key of the sweep's wake report of role at now:
// legion.wake.role.<role>.<the start of the roleWakeInterval now falls in, in Unix seconds>. Every
// listener names one role's report in one interval the same way, whichever of them sweeps.
func sweptRoleWakeID(role string, now time.Time) string {
	return "legion.wake.role." + role + "." + strconv.FormatInt(now.Truncate(roleWakeInterval).Unix(), 10)
}

// publishSweptRoleWake publishes role's wake report from the sweep (raiseKeptRoleWake), dated now,
// the listener's clock: one attempt, bounded by wakeReportAttempt and by lifetime, under the
// message id and the dedupe key of the interval now falls in (sweptRoleWakeID), and an event id
// made from that key (keyedEventID), so the stream stores one report for a role in each interval
// across every listener. A listener whose report the stream already held logs it as already
// stored and not as published, so a count of published lines is a count of stored reports. A
// report that did not reach the stream is logged and not retried: the next sweep raises it again.
func publishSweptRoleWake(lifetime context.Context, client *bus.Client, logger *logging.Logger, role string, now time.Time) {
	if client == nil {
		return
	}
	key := sweptRoleWakeID(role, now)
	report := wakeReport{Kind: wakeKindRole, Role: role, At: now.UnixMilli(), token: legionRoleToken(role)}
	if report.token == "" {
		return
	}
	item, ok := wakeReportEnvelope(logger, report, keyedEventID(key), key)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(lifetime, wakeReportAttempt)
	defer cancel()
	ack, err := client.PublishStoredAck(ctx, report.subject(), item, key)
	switch {
	case err != nil:
		logger.Warn("legion wake report not published", slog.String("kind", report.Kind),
			slog.String("role", role), slog.String("session", report.Session), slog.Int("attempts", 1),
			slog.String("dedupe_key", key), slog.String("error", err.Error()))
	case ack.Duplicate:
		logger.Info("legion wake report was already stored", slog.String("kind", report.Kind),
			slog.String("role", role), slog.String("dedupe_key", key))
	default:
		logger.Info("legion wake report published", slog.String("kind", report.Kind),
			slog.String("role", role), slog.String("session", report.Session), slog.Int("attempts", 1),
			slog.String("dedupe_key", key))
	}
}

// wakeReportsInFlight is how many wake reports may be retrying at once. One report holds its
// goroutine for as long as wakeReportBudget, and during a bus outage every role send with no
// holder and every message nothing can place raises one, so an unbounded reporter turns a
// minutes-long outage into a goroutine and a report's worth of memory per such request. Past
// the cap a report is dropped and said so in the listener's log. The wake it carried is lost
// until its sender tries again or, for a phase worker whose phase is open, the daemon's
// backstop runs out: a pod the daemon released before the outage stays released.
const wakeReportsInFlight = 64

// wakeReporter publishes wake reports off the paths that raise them: under the listener's own
// lifetime, so the shutdown ends every retry still running, and no more than
// wakeReportsInFlight at a time.
type wakeReporter struct {
	lifetime context.Context
	client   *bus.Client
	slots    chan struct{}
}

func newWakeReporter(lifetime context.Context, client *bus.Client) *wakeReporter {
	return &wakeReporter{lifetime: lifetime, client: client, slots: make(chan struct{}, wakeReportsInFlight)}
}

// report publishes one report off the caller's path. The caller is writing a 404 the report
// owes nothing, and every role send with no holder raises one — including the common case where
// no daemon holds a released claim of that role at all — so publishing it inline would put the
// report's retries in front of each of those 404s.
func (r *wakeReporter) report(logger *logging.Logger, report wakeReport) {
	if r == nil || r.client == nil {
		return
	}
	select {
	case r.slots <- struct{}{}:
	default:
		logger.Warn("legion wake report dropped: too many are already retrying",
			slog.String("kind", report.Kind), slog.String("role", report.Role),
			slog.String("session", report.Session), slog.Int("inFlight", wakeReportsInFlight))
		return
	}
	go func() {
		defer func() { <-r.slots }()
		publishWakeReport(r.lifetime, r.client, logger, report)
	}()
}

// reportRoleWake reports that a message was published to role and no live session held it, on the
// wake subject of the project the role names (legionRoleToken). The role is the claim token a
// Legion role topic is named by, which is what the daemon matches its own released claims on. A
// role that names no Legion project is reported nowhere: no daemon holds a claim on it.
func reportRoleWake(d *listenerDeps, logger *logging.Logger, role string) {
	token := legionRoleToken(role)
	if d == nil || token == "" {
		return
	}
	d.wakes.report(logger, wakeReport{Kind: wakeKindRole, Role: role, token: token})
}

// reportAgentWake reports that a message sent on topic, the agent subject of sessionID, could not be
// placed, on the wake subject of the project that subject names (agentWakeToken). A message on an
// agent subject of no project is reported nowhere: no Legion daemon's session is of no project.
func reportAgentWake(wakes *wakeReporter, logger *logging.Logger, topic, sessionID string) {
	token := agentWakeToken(topic)
	if sessionID == "" || token == "" {
		return
	}
	wakes.report(logger, wakeReport{Kind: wakeKindAgent, Session: sessionID, token: token})
}

// startListenerReadyReports publishes this listener's liveness report at once and every
// listenerReadyInterval until ctx ends. It is the whole of the daemon's R7 gate: a daemon that
// has read no report within its window releases nothing, so a listener that is down, partitioned
// from the stream, or denied the publish by its own grant holds release off rather than letting
// pods be released into a silence nothing would wake them out of.
func startListenerReadyReports(ctx context.Context, client *bus.Client, logger *logging.Logger, interval time.Duration) {
	if interval <= 0 {
		interval = listenerReadyInterval
	}
	go func() {
		publishWakeReport(ctx, client, logger, wakeReport{Kind: wakeKindReady})
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				publishWakeReport(ctx, client, logger, wakeReport{Kind: wakeKindReady})
			}
		}
	}()
}
