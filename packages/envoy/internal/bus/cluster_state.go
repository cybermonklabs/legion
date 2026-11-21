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
