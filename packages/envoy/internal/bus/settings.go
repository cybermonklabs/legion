package bus

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ReplicasEnvVar, StreamMaxBytesEnvVar and SessionMaxAgeEnvVar are the deployment settings
// WithReplicas, WithStreamMaxBytes and the session registry's maximum age carry. Each program
// reads them itself and hands the bus their values; the bus names them in a server's refusal of a
// stream or a bucket that carries one (refusedSettings).
const (
	ReplicasEnvVar       = "ENVOY_NATS_REPLICAS"
	StreamMaxBytesEnvVar = "ENVOY_STREAM_MAX_BYTES"
	SessionMaxAgeEnvVar  = "ENVOY_SESSION_MAX_AGE"
)

// MaxReplicas is the most copies NATS keeps of a stream or a bucket, and so the largest
// ReplicasEnvVar ParseNATSReplicas takes. aws:WebhookListener's rendering
// (github.com/sjawhar/envoy/infra/aws/rendering) refuses a larger natsReplicas by this bound, so a
// component never deploys a listener its configuration would refuse.
const MaxReplicas = 5

// SenderMessagesPerMinuteEnvVar and SenderMessageBurstEnvVar are envoy-listener's limit on the
// messages each agent session sends through it: how many a minute, and how many at once. The bus
// reads neither. They live here with MaxSenderMessagesPerMinute because the listener's parse
// (cmd/listener, sender_limit.go) and envoy-infra's listener settings
// (github.com/sjawhar/envoy/infra/core), which may not compile a listener package, name the same
// variables.
const (
	SenderMessagesPerMinuteEnvVar = "ENVOY_SENDER_MESSAGES_PER_MINUTE"
	SenderMessageBurstEnvVar      = "ENVOY_SENDER_MESSAGE_BURST"
)

// MaxSenderMessagesPerMinute is the largest SenderMessagesPerMinuteEnvVar the listener takes:
// one message a millisecond, past which a rate limits nothing a session does. The listener's
// parse and envoy-infra's check of a component's inputs both read it, so a component never
// deploys a listener that would refuse its value.
const MaxSenderMessagesPerMinute = 60000

// ParseNATSReplicas reads ReplicasEnvVar, the deployment's replica count: unset or blank is 1,
// and otherwise a whole number from 1 to MaxReplicas. Anything else (0, a negative number, more
// than NATS keeps, a word) is refused naming the variable and the value, at load rather than as a
// server's refusal at the stream. envoy-listener (config.Load in
// github.com/sjawhar/envoy/internal/config) and envoy-dispatch read the setting through it. Every
// program of a deployment carries the same value: one with a lower value can lower the stream
// until the others' keepers raise it back (ConnectOwningStream).
func ParseNATSReplicas(raw string) (int, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return 1, nil
	}
	replicas, err := strconv.Atoi(value)
	if err != nil || replicas < 1 || replicas > MaxReplicas {
		return 0, fmt.Errorf("invalid %s %q: want a whole number of copies from 1 to %d", ReplicasEnvVar, value, MaxReplicas)
	}
	return replicas, nil
}

// ParseStreamMaxBytes reads StreamMaxBytesEnvVar, the stream's byte limit: unset or blank is 0,
// which sets no limit (a deployed stream keeps the one it has), and otherwise a positive whole
// number of bytes. 0, a negative number or a word is refused naming the variable and the value.
// envoy-listener (config.Load) and envoy-dispatch read the setting through it. A deployed limit is
// never lowered (WithStreamMaxBytes), so programs with different limits leave the largest.
func ParseStreamMaxBytes(raw string) (int64, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return 0, nil
	}
	limit, err := strconv.ParseInt(value, 10, 64)
	if err != nil || limit < 1 {
		return 0, fmt.Errorf("invalid %s %q: want a positive whole number of bytes, or unset for no limit", StreamMaxBytesEnvVar, value)
	}
	return limit, nil
}

// ParseSessionMaxAge reads SessionMaxAgeEnvVar, how long the session bucket keeps a registration
// record: unset or blank is 0, which leaves the deployed bucket's own maximum age exactly as it
// is, and otherwise a positive Go duration ("90m", "2h"). 0, a negative duration or a word is
// refused naming the variable and the value, at load rather than as a server's refusal at the
// bucket. envoy-listener (config.Load) reads the setting through it.
//
// A deployment sets it because the bucket's age is the backstop a record is dropped at, and a
// record dropped before the gone horizon leaves an unresponsive session reading gone. A bucket that
// the listener creates with the setting unset keeps a record for 2 hours (session.BucketMaxAge, a
// constant: the default 1-hour gone horizon plus one hour). That age does not follow
// ENVOY_SESSION_GONE_HORIZON, so a deployment that raises the horizon to 2 hours or more raises
// this setting past it too. A bucket an older build created keeps its 5 minutes until this setting
// raises it, and there a session stalled for six minutes loses the record that says it is only
// unresponsive. The listener applies the value at open and on every pass of its session reaper
// (session.SessionRegistry), so a raise reaches a running deployment within one pass.
func ParseSessionMaxAge(raw string) (time.Duration, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return 0, nil
	}
	maxAge, err := time.ParseDuration(value)
	if err != nil || maxAge <= 0 {
		return 0, fmt.Errorf("invalid %s %q: want a positive Go duration such as 90m, or unset to leave the bucket's own maximum age", SessionMaxAgeEnvVar, value)
	}
	return maxAge, nil
}
