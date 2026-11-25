package bus

import (
	"errors"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// The JetStream API error codes of nats-server's answers that come of its cluster's state rather
// than of the request (ClusterStateAnswer), as nats-server v2.15.1 numbers them in
// server/errors.json and server/jetstream_errors_generated.go. nats.go v1.55.0 names two of them
// (nats.JSErrCodeJetStreamNotAvailable, nats.JSErrCodeInsufficientResourcesErr) and none of the
// others.
const (
	// jsClusterNoPeers (JSClusterNoPeersErrF, errors.json:83) answers a create or a raise of a
	// stream whose copies the meta leader cannot place: fewer members it has heard from than the
	// copies asked for, or too few of them online for the copies' quorum (selectPeerGroup,
	// server/jetstream_cluster.go:10630; the create at :11091, the raise at :11435).
	jsClusterNoPeers = 10005
	// jsClusterNotLeader (JSClusterNotLeaderErr, errors.json:533) answers a request the member it
	// reached cannot handle because it leads no longer (server/jetstream_cluster.go:13121,
	// server/stream.go:7581).
	jsClusterNotLeader = 10009
	// jsStreamReplicasNotSupported (JSStreamReplicasNotSupportedErr, errors.json:733) answers a
	// create or an update of more than one copy on a server that runs with no cluster, a hub of
	// one member (server/stream.go:866, :2565).
	jsStreamReplicasNotSupported = 10074
)

// ClusterStateAnswer reports whether err carries an answer nats-server gives from its cluster's
// state, with the answer's JetStream API error code: a stream or a bucket's stream the cluster
// cannot place at the copies asked for (jsClusterNoPeers, nats.JSErrCodeInsufficientResourcesErr
// at errors.json:103, jsStreamReplicasNotSupported), the meta group with no leader, which is
// JetStream's "temporarily unavailable" (nats.JSErrCodeJetStreamNotAvailable at errors.json:43,
// server/jetstream_api.go:1466), or a member that leads no longer (jsClusterNotLeader). Each is
// what a hub answers while it has fewer members than the deployment's copies, as during a rolling
// restart of its servers or while a cluster forms, and the same request succeeds once the members
// are back, with no setting of the asker changed: a start that ends on one only crash-loops until
// then. The answer is read by its code alone, through the legacy package's *nats.APIError or the
// jetstream package's *jetstream.APIError (a bucket's handle open, wrapKeyValue), and never by
// its text. Any other error is (0, false).
func ClusterStateAnswer(err error) (code int, ok bool) {
	var legacy *nats.APIError
	var api *jetstream.APIError
	switch {
	case errors.As(err, &legacy):
		code = int(legacy.ErrorCode)
	case errors.As(err, &api):
		code = int(api.ErrorCode)
	default:
		return 0, false
	}
	switch code {
	case jsClusterNoPeers, int(nats.JSErrCodeJetStreamNotAvailable), jsClusterNotLeader,
		int(nats.JSErrCodeInsufficientResourcesErr), jsStreamReplicasNotSupported:
		return code, true
	}
	return 0, false
}

// The JetStream API error codes of nats-server's answers that come of the asker's account's
// JetStream limits rather than of the request (AccountStateAnswer), as nats-server v2.15.1 numbers
// them in server/errors.json. nats.go v1.55.0 names none of them.
const (
	// jsAccountResourcesExceeded (JSAccountResourcesExceededErr, errors.json:35) answers a write the
	// account's storage or memory has no room for (server/stream.go:7342, :7610, :8108).
	jsAccountResourcesExceeded = 10002
	// jsMaximumConsumersLimit (JSMaximumConsumersLimitErr, errors.json:455) answers the create of a
	// consumer on a stream or in an account at its consumer cap (server/consumer.go:1174), and the
	// create or the update of a stream whose cap is above the account's own (server/jetstream.go:
	// 2561-2562): the notification stream's cap is streamMaxConsumers, compiled in, and no setting
	// of the asker's lowers it.
	jsMaximumConsumersLimit = 10026
	// jsMaximumStreamsLimit (JSMaximumStreamsLimitErr, errors.json:595) answers the create of a
	// stream, a bucket's included, in an account that holds as many streams as its limit
	// (server/jetstream_api.go:4343-4344, server/jetstream_cluster.go:11019).
	jsMaximumStreamsLimit = 10027
	// jsMemoryResourcesExceeded (JSMemoryResourcesExceededErr, errors.json:655) and
	// jsStorageResourcesExceeded (JSStorageResourcesExceededErr, errors.json:385) answer the create
	// or the update of a stream whose reservation the account's memory or storage limit, or the
	// server's, has no room for (server/jetstream.go:2597-2610).
	jsMemoryResourcesExceeded  = 10028
	jsStorageResourcesExceeded = 10047
	// jsStreamMaxBytesRequired (JSStreamMaxBytesRequired, errors.json:1125) answers the create of a
	// stream that sets no byte limit in an account that requires one (server/stream.go:2460-2461).
	jsStreamMaxBytesRequired = 10113
)

// AccountStateAnswer reports whether err carries an answer nats-server gives from the state of the
// asker's account or of the server itself, which no setting of the asker's changes, with the
// answer's JetStream API error code: the account at one of its JetStream limits, its resources
// (jsAccountResourcesExceeded), its consumers (jsMaximumConsumersLimit), its streams
// (jsMaximumStreamsLimit), or its memory or storage (jsMemoryResourcesExceeded,
// jsStorageResourcesExceeded); an account that requires every stream to set a byte limit
// (jsStreamMaxBytesRequired), when byteLimitIsCode is set; and any other answer whose HTTP status
// is 500 or more, the server failing a request it took, but for the server's refusal of the
// configuration asked for (nats.JSStreamInvalidConfig, 10052, status 500), which a setting can
// make: SessionMaxAgeEnvVar below the session bucket's two-minute duplicate window is refused so
// (server/stream.go:2026-2027, SetKeyValueMaxAge). An operator who raises the account's limit or
// frees what it counts (a stream or a consumer deleted, stored messages gone) lifts each, as the
// server's own recovery does, with nothing of the asker changed, so a start that ends on one only
// crash-loops until then, and every program of a deployment shares the account and would be
// refused alike.
//
// byteLimitIsCode says whether the byte limit of what the request creates is the asker's code and
// no setting, as a KV bucket's is (EnsureKeyValue): where a setting chooses it, as
// StreamMaxBytesEnvVar chooses the notification stream's, an account that requires one is answered
// by that setting, and is not this. An answer ClusterStateAnswer reads is the cluster's, and not
// this either. The answer is read by its code and its status alone, through the legacy package's
// *nats.APIError or the jetstream package's *jetstream.APIError as ClusterStateAnswer reads them,
// and never by its text. Any other error is (0, false), the account limits a setting of the
// asker's answers among them: a stream byte limit above the account's
// (JSStreamMaxStreamBytesExceeded, 10122, which StreamMaxBytesEnvVar sets) and no tier of the
// account's limits at the copies asked for (JSNoLimitsErr, 10120, which ReplicasEnvVar sets).
func AccountStateAnswer(err error, byteLimitIsCode bool) (code int, ok bool) {
	if _, cluster := ClusterStateAnswer(err); cluster {
		return 0, false
	}
	var legacy *nats.APIError
	var api *jetstream.APIError
	var status int
	switch {
	case errors.As(err, &legacy):
		code, status = int(legacy.ErrorCode), legacy.Code
	case errors.As(err, &api):
		code, status = int(api.ErrorCode), api.Code
	default:
		return 0, false
	}
	switch {
	case code == jsAccountResourcesExceeded, code == jsMaximumConsumersLimit, code == jsMaximumStreamsLimit,
		code == jsMemoryResourcesExceeded, code == jsStorageResourcesExceeded,
		code == jsStreamMaxBytesRequired && byteLimitIsCode,
		status >= 500 && code != int(nats.JSStreamInvalidConfig):
		return code, true
	}
	return 0, false
}
