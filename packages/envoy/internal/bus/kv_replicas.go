package bus

import (
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
)

// raiseReplicas raises the stream of the existing bucket to replicas copies when it has fewer, and
// returns the copies it had then, 0 when it left the bucket alone. The update writes back the
// stream's deployed configuration with only its replicas changed, so the bucket's TTL, history,
// discard policy, direct get, rollup, delete settings, metadata and the rest stay as they are. A
// bucket with as many copies or more is left alone, and one copy needs no read: every stream has at
// least one. As for the stream (ensureStreamWithConfig), two starts raising at once each write the
// deployment's one count. A raise whose answer never came succeeds when the stream, read again,
// has the copies, and one the server refused as overlapping the bucket's own stream is sent again
// until the stream has them or its sends run out (updateStream). Its error names the bucket's
// stream and, for a refused raise, the setting (refusedSettings):
// `raise KV_envoy_interests: ENVOY_NATS_REPLICAS=3: ...`, so the keeper's line says which bucket
// it could not raise.
func raiseReplicas(js nats.JetStreamContext, bucket string, replicas int) (int, error) {
	if replicas <= 1 {
		return 0, nil
	}
	stream := "KV_" + bucket
	info, err := js.StreamInfo(stream)
	if err != nil {
		return 0, fmt.Errorf("raise %s: %w", stream, err)
	}
	if info.Config.Replicas >= replicas {
		return 0, nil
	}
	raised := info.Config
	raised.Replicas = replicas
	if err := updateStream(js, &raised, func(again *nats.StreamInfo) bool { return again.Config.Replicas >= replicas }); err != nil {
		return 0, fmt.Errorf("raise %s: %w", stream, refusedSettings(err, &info.Config, &raised))
	}
	return info.Config.Replicas, nil
}

// awaitReplicasCurrent waits until the bucket's stream reports replicas copies, each one current
// with its leader and none of them offline or lagging, read every bucketAppliedInterval and given
// up at bucketAppliedDeadline. It is what EnsureKeyValue runs after a raise that added copies, and
// nothing else needs it: a bucket nobody raised has no copy that is behind.
//
// The deadline is an error naming the bucket's stream, the copies it asked for and what each copy
// that was not current had to say (copiesBehind), the way a refused raise names its setting, and
// the caller fails its start on it. Carrying on instead would hand the caller a bucket whose
// direct gets a copy that is 90% caught up may answer "key not found" for a key that is there
// (server/jetstream_cluster.go:2622-2641, v2.10.29), and a start that reads one wrong answer is
// worse than a start that does not come up: the webhook redelivery sweep reads its records the
// moment this call answers, and a stale miss there asks GitHub for a redelivery it already made.
//
// A read that fails is returned as it is, since the stream is there: this runs only after a raise
// of it answered.
func awaitReplicasCurrent(js nats.JetStreamContext, bucket string, replicas int) error {
	stream := "KV_" + bucket
	start := time.Now()
	for attempt := 1; ; attempt++ {
		info, err := js.StreamInfo(stream)
		if err != nil {
			return fmt.Errorf("raise %s: read the raised stream: %w", stream, err)
		}
		behind := copiesBehind(info, replicas)
		if behind == "" {
			return nil
		}
		if time.Since(start) >= bucketAppliedDeadline {
			return fmt.Errorf("raise %s: %d copies were not current within %s: %s", stream, replicas, bucketAppliedDeadline, behind)
		}
		if attempt == 1 {
			slog.Warn("envoy nats kv raised copies not current yet, waiting",
				slog.String("bucket", bucket),
				slog.Int("replicas", replicas),
				slog.String("behind", behind),
				slog.String("retry_in", bucketAppliedInterval.String()),
				slog.String("deadline", bucketAppliedDeadline.String()))
		}
		time.Sleep(bucketAppliedInterval)
	}
}

// copiesBehind says what keeps the stream info describes short of replicas copies that are all
// current, and is empty when none does: the stream has a leader, as many copies as were asked for,
// and every copy of it is current, online and not lagging. Each copy that is not is named with
// what it reported, so a start that gave up says which member it waited for.
func copiesBehind(info *nats.StreamInfo, replicas int) string {
	if info.Cluster == nil || info.Cluster.Leader == "" {
		return "the stream has no leader yet"
	}
	var behind []string
	if copies := 1 + len(info.Cluster.Replicas); copies != replicas {
		behind = append(behind, fmt.Sprintf("%d copies of %d", copies, replicas))
	}
	for _, peer := range info.Cluster.Replicas {
		switch {
		case peer.Offline:
			behind = append(behind, peer.Name+" offline")
		case !peer.Current:
			behind = append(behind, fmt.Sprintf("%s not current, lag %d", peer.Name, peer.Lag))
		case peer.Lag > 0:
			behind = append(behind, fmt.Sprintf("%s lag %d", peer.Name, peer.Lag))
		}
	}
	return strings.Join(behind, ", ")
}
