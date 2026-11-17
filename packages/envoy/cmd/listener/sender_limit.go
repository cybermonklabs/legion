// The operator's limit on the messages each agent session sends through this listener: the two
// settings it is read from, and the start line that states it.

package main

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
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
	return fmt.Sprintf("listener sender limit: %d messages a minute, %d at once, for each agent session in this process", l.perMinute, l.burst)
}
