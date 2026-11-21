package bus

import (
	"context"
	"slices"

	"github.com/nats-io/nats.go"
)

// NotificationStreamConfig is ENVOY_NOTIFICATIONS as this build makes it, at replicas copies and a
// byte limit of maxBytes, 0 for none: the stream ConnectOwningStream ensures under WithReplicas and
// WithStreamMaxBytes, in a configuration of its own. The operator makes the stream with it beside
// the auth service (internal/streamset, envoy-infra streams), where no start makes it
// (StreamsOperatorMadeEnvVar).
func NotificationStreamConfig(replicas int, maxBytes int64) nats.StreamConfig {
	cfg := parseConnectOptions([]ConnectOption{WithReplicas(replicas), WithStreamMaxBytes(maxBytes)}).streamConfig()
	cfg.Subjects = slices.Clone(cfg.Subjects)
	return cfg
}

// EnsureNotificationStream makes ENVOY_NOTIFICATIONS on conn, through js, with cfg when the server
// has none, under a fresh StreamIDMetadataKey, or reconciles the deployed one towards cfg by a
// start's own rules (ensureStreamWithConfig): subjects only added, limits only raised, copies
// raised, its StreamIDMetadataKey kept, and a stream a newer build stamped left as it is. It
// returns the subjects the stream carries afterwards. It reads whether conn's server caps a stream
// with no consumer cap first, as a start does (connectedDefaultsAnUnsetCap). It is the operator's
// make and raise of the stream (envoy-infra streams update), the one writer of it beside the auth
// service; every other program there only reads it (readOperatorMadeStream).
func EnsureNotificationStream(ctx context.Context, conn *nats.Conn, js nats.JetStreamContext, cfg nats.StreamConfig) ([]string, error) {
	defaultsAnUnsetCap, err := connectedDefaultsAnUnsetCap(ctx, conn)
	if err != nil {
		return nil, err
	}
	return ensureStreamWithConfig(ctx, readsRefusals(conn, js), &cfg, defaultsAnUnsetCap, atStart)
}
