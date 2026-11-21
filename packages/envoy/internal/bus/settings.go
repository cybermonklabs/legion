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

// StreamsOperatorMadeEnvVar is the setting of a program that runs beside the NATS auth service,
// where no client but the operator may create or change a stream (natsgrant.ForCallout): set to 1,
// the program's start reads every stream and bucket it needs and makes none, stopping, naming the
// stream and the operator's command, when one is missing (ErrStreamNotMade); its client runs no
// keeper, and a bucket's age is left as the operator set it (SetKeyValueMaxAge). Every stream is
// then made, and raised, at deploy time by `envoy-infra streams update`. Unset, the start makes and
// reconciles its streams as it always has. The bus reads it from the connect's environment
// (WithEnvironment), as it reads the NATS credential, so every program connecting through it takes
// it; ParseStreamsOperatorMade reads the value.
const StreamsOperatorMadeEnvVar = "ENVOY_STREAMS_OPERATOR_MADE"

// ParseStreamsOperatorMade reads StreamsOperatorMadeEnvVar: "1" is on, and unset, blank or "0"
// off. Anything else is refused naming the variable and the value, so a "true" never reads as off
// and has the start make its streams.
func ParseStreamsOperatorMade(raw string) (bool, error) {
	switch value := strings.TrimSpace(raw); value {
	case "", "0":
		return false, nil
	case "1":
		return true, nil
	default:
		return false, fmt.Errorf("invalid %s %q: want 1, or 0 or unset", StreamsOperatorMadeEnvVar, value)
	}
}

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
// record dropped before the gone horizon leaves an unresponsive session reading gone: with the
// 5-minute default and a 1-hour horizon, a session stalled for six minutes loses the record that
// says it is only unresponsive. The listener applies the value at open and on every pass of its
// session reaper (session.SessionRegistry), so a raise reaches a running deployment within one
// pass.
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
