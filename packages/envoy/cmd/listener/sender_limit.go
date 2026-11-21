// The operator's limit on the messages each agent session sends through this listener: the two
// settings it is read from, the start line that states it, the take each counted message makes
// from its sender's count, and the 429 a message past that count is answered with.

package main

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/ratelimit"
)

// senderLimit is the operator's limit: perMinute messages a minute for each agent session, burst
// of them at once, counted in counts, one bucket for each sending session, read at now. A nil
// senderLimit is no limit, which is what an operator who sets nothing gets and what every test
// that builds listenerDeps itself runs with, so every method is safe on nil.
//
// The counts are this process's own: no other listener shares them and a restart fills each one
// again, since a session sends through the one listener its client names.
type senderLimit struct {
	perMinute int
	burst     int
	counts    *ratelimit.Keyed
	now       func() time.Time
}

// newSenderLimit is a limit of perMinute messages a minute, burst at once, read at now: the
// listener's is time.Now, and a test passes a clock of its own. perMinute is from 1 to
// bus.MaxSenderMessagesPerMinute and burst 1 or more, which parseSenderLimit holds the settings to.
func newSenderLimit(perMinute, burst int, now func() time.Time) *senderLimit {
	return &senderLimit{
		perMinute: perMinute,
		burst:     burst,
		counts:    ratelimit.NewKeyed(ratelimit.Limit{Every: time.Minute / time.Duration(perMinute), Burst: burst}),
		now:       now,
	}
}

// parseSenderLimit reads the limit from bus.SenderMessagesPerMinuteEnvVar and
// bus.SenderMessageBurstEnvVar, nil when the operator set none. Each value is trimmed and a blank
// one is unset, as bus.ParseStreamMaxBytes reads its own. An unset rate, or 0, is no limit. The
// rate is a whole number from 0 to bus.MaxSenderMessagesPerMinute, and the burst a whole number
// of 1 or more, which unset equals the rate.
//
// Anything else is refused naming the variable and the value, and the listener does not start: an
// operator who wrote a value meant a limit, so a listener that started with none in its place
// would serve what nobody asked for. A burst with no rate, or with a rate of 0, is refused for the
// same reason, since it would limit nothing.
func parseSenderLimit(lookup func(string) string) (*senderLimit, error) {
	rawRate := strings.TrimSpace(lookup(bus.SenderMessagesPerMinuteEnvVar))
	rawBurst := strings.TrimSpace(lookup(bus.SenderMessageBurstEnvVar))
	perMinute := 0
	if rawRate != "" {
		parsed, err := strconv.Atoi(rawRate)
		if err != nil || parsed < 0 || parsed > bus.MaxSenderMessagesPerMinute {
			return nil, fmt.Errorf("invalid %s %q: want a whole number of messages a minute from 0 to %d, or unset for no limit",
				bus.SenderMessagesPerMinuteEnvVar, rawRate, bus.MaxSenderMessagesPerMinute)
		}
		perMinute = parsed
	}
	if perMinute == 0 {
		if rawBurst != "" {
			return nil, fmt.Errorf("%s is %q, but %s sets no limit: set a rate, or unset the burst",
				bus.SenderMessageBurstEnvVar, rawBurst, bus.SenderMessagesPerMinuteEnvVar)
		}
		return nil, nil
	}
	burst := perMinute
	if rawBurst != "" {
		parsed, err := strconv.Atoi(rawBurst)
		if err != nil || parsed < 1 {
			return nil, fmt.Errorf("invalid %s %q: want a whole number of messages at once from 1 up, or unset for the rate",
				bus.SenderMessageBurstEnvVar, rawBurst)
		}
		burst = parsed
	}
	return newSenderLimit(perMinute, burst, time.Now), nil
}

// describeSenderLimit is the listener's one start line for l, so an operator reads in the log
// the limit this process holds each agent session to, or that it holds them to none.
func describeSenderLimit(l *senderLimit) string {
	if l == nil {
		return "listener sender limit: none"
	}
	return fmt.Sprintf("listener sender limit: %s a minute, %d at once, for each agent session in this process", quantity(l.perMinute, "message", "messages"), l.burst)
}

// take counts item against its sender's count at the limit's clock, and answers the charge the
// route marks as it answers (senderCharge), or the refusal it answers 429 with when the count is
// empty. Both routes take it through admit, after their own checks and before they read a
// session, resolve a role, report a wake or store anything, so a refused message reaches nobody.
//
// The sender is the session a secrets-broker token proved, else the request's trimmed
// source_session (messageSender): sessionNotBound has already refused a body that names a session
// the token does not, so a token's session is the sender whether the body names it or not. Two
// messages are not counted, and both answer nil and nil, as no limit does. One is a message with no
// sender, which is how the Legion daemon sends its notices (packages/daemon/internal/notify). The
// other is a message whose source is not agent (contracts.MessageEnvelope writes agent for an absent
// one), whatever credential the request presented: a secrets-broker token names the sender and does
// not make the message count. Such a message is another system's delivery, Dispatch's or Envoy's
// own among them, or a request its caller wrote itself. The Envoy tools send the source agent and
// give an agent no argument for it, so the limit counts each of their messages that has a sender and
// is not a security boundary.
func (l *senderLimit) take(r *http.Request, item contracts.Envelope) (*senderCharge, *senderRefusal) {
	if l == nil || item.Source != "agent" {
		return nil, nil
	}
	sender := messageSender(r, item.SourceSession)
	if sender == "" {
		return nil, nil
	}
	now := l.now()
	giveBackAt, ok := l.counts.TakeAt(sender, now)
	if !ok {
		// The wait is read at the instant the take was refused at, so the seconds the answer
		// names are the ones until this count holds a message again.
		return nil, &senderRefusal{
			sender:            sender,
			perMinute:         l.perMinute,
			burst:             l.burst,
			retryAfterSeconds: ratelimit.RetryAfterSeconds(l.counts.WaitAt(sender, now)),
		}
	}
	return &senderCharge{sender: sender, giveBack: func() { giveBackAt(l.now()) }}, nil
}

// admit takes item from its sender's count (take) for a route that answers through record, and
// reports whether the route goes on. A take the empty count refused is answered here, with the 429
// refuseSenderRateLimited writes, and the route answers nothing more. Otherwise record carries the
// charge, whose sender the request's line names once it costs one (counted), and the route defers
// the charge's settle right after, so the take is given back on every path that marks nothing.
func (l *senderLimit) admit(w http.ResponseWriter, r *http.Request, record *requestRecord, item contracts.Envelope) (*senderCharge, bool) {
	charge, refusal := l.take(r, item)
	if refusal != nil {
		record.refuseSenderRateLimited(w, refusal)
		return nil, false
	}
	record.charge = charge
	return charge, true
}

// senderCharge is one message's take from its sender's count (senderLimit.take). The route marks
// it as it goes, with two marks that a repeat verdict treats differently. woke marks a wake
// reported for the message, and forgive, where the stream or the role's kept lane answers that the
// message repeats one already stored, cancels it: such a repeat costs nothing, also after a wake
// report. stored marks a message the route stored, may have stored, or answered 504 for, and
// nothing cancels it, since a repeat verdict after a send that may have stored the message may
// name that send's own copy. A message the route may have stored is a publish the stream did not
// acknowledge, a duplicate verdict that answers the bus's resend after a closed connection
// (published), and a role forward that failed with no refusal (sendToRole).
//
// settle, which the route defers right after the take (senderLimit.admit), gives the take back
// unless the message cost one (counted), so a path that marks nothing costs nothing: a refusal
// before any store or wake, a 503 for a publish that sent nothing among them, and a role send's
// echo, 403, refused forward or repeat its own record answers, before any forward that may have
// stored it. Each mark comes before the route writes its answer, because the request's log line
// names the sender of a charge that cost one (counted) at the moment it is written.
//
// A nil senderCharge is a message the limit does not count, and every method is safe on it. Only
// the request's own goroutine marks and settles a charge.
type senderCharge struct {
	sender   string
	giveBack func()
	// woken is woke's mark, spent stored's and forgiven forgive's.
	woken    bool
	spent    bool
	forgiven bool
}

// woke marks the message as one a wake was reported for (reportAgentWake, reportRoleWake), which
// costs its sender one unless the message turns out to repeat one already stored (forgive).
func (c *senderCharge) woke() {
	if c != nil {
		c.woken = true
	}
}

// stored marks the message as one the route stored or may have stored, or a role send answered
// 504, which costs one whether or not a forward went out (roleSend.timedOut). That costs its
// sender one whatever the route answers next: forgive does not cancel it.
func (c *senderCharge) stored() {
	if c != nil {
		c.spent = true
	}
}

// forgive marks the message as a repeat of one already stored, which costs nothing, also after a
// wake report (woke). It leaves a stored mark standing.
func (c *senderCharge) forgive() {
	if c != nil {
		c.forgiven = true
	}
}

// published marks the charge by the stream's answer to the route's publish of the message
// (bus.Client.PublishAckWithin), the one rule the send route and the topic publish share. A
// publish that may have stored the message (mayHaveStored) costs one whatever the route answers.
// Any other failure stored nothing and marks
// nothing: a refusal of the server's, and a failure before anything was sent, such as a wait for
// NATS to reconnect that outlasted the route's deadline. A duplicate verdict names a copy the
// stream already held, which costs nothing, also after a wake report, unless it answers the bus's
// resend after a closed connection (bus.StoredAck.Resent), whose first send may have stored that
// copy. Every other answer stored the message.
func (c *senderCharge) published(ack bus.StoredAck, err error) {
	switch {
	case mayHaveStored(err):
		c.stored()
	case err != nil:
		// A refusal, or a failure before any send: the stream holds nothing of this publish.
	case ack.Duplicate && !ack.Resent:
		c.forgive()
	default:
		c.stored()
	}
}

// counted is the sender the message has cost one so far, which the request's log line names, and
// empty while it costs nothing: a message marked stored costs one, and one a wake was reported for
// costs one unless it was forgiven.
func (c *senderCharge) counted() string {
	if c != nil && (c.spent || c.woken && !c.forgiven) {
		return c.sender
	}
	return ""
}

// settle gives the take back at the limit's clock unless the message cost one. It runs once,
// after the answer.
func (c *senderCharge) settle() {
	if c != nil && c.counted() == "" {
		c.giveBack()
	}
}

// senderRefusal is a take the sender's empty count refused: the sender, which the request's log
// line names, the limit, and the whole seconds until the count holds one message again, which the
// answer's Retry-After and body both name (requestRecord.refuseSenderRateLimited).
type senderRefusal struct {
	sender            string
	perMinute         int
	burst             int
	retryAfterSeconds int
}

// senderRateLimitedAnswer is the 429's body. Error is what the agent reads: each client shows
// a refusal's error as it is written and sends nothing below 500 again
// (packages/envoy-client/src/transport.ts), so the sentence alone tells the agent that its sending
// is limited, that this message went nowhere, and when to send it again. Reason is the outcome the
// request's log line records, and not apiauth.go's rate_limited, which refuses a broker token's
// check rather than a session's sending.
type senderRateLimitedAnswer struct {
	Error             string         `json:"error"`
	Reason            requestOutcome `json:"reason"`
	RetryAfterSeconds int            `json:"retry_after_seconds"`
	MessagesPerMinute int            `json:"messages_per_minute"`
	Burst             int            `json:"burst"`
}

// answer is the 429 body that names r's limit and wait. Where a value of the sentence is 1 its noun
// is singular, `1 message a minute` and `after 1 second`; the burst's `<burst> at once` has none.
func (r *senderRefusal) answer() senderRateLimitedAnswer {
	return senderRateLimitedAnswer{
		Error: fmt.Sprintf("Your sending is rate limited. This message was not sent. This listener lets one session send %s a minute and %d at once. You can send it again after %s.",
			quantity(r.perMinute, "message", "messages"), r.burst, quantity(r.retryAfterSeconds, "second", "seconds")),
		Reason:            outcomeSenderRateLimited,
		RetryAfterSeconds: r.retryAfterSeconds,
		MessagesPerMinute: r.perMinute,
		Burst:             r.burst,
	}
}

// quantity is n followed by its noun: one for a count of 1 and many for every other.
func quantity(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return strconv.Itoa(n) + " " + many
}
