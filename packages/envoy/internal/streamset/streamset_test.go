package streamset_test

import (
	"testing"

	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/streamset"
)

// deployed is stream as the build makes it, at replicas copies: the info a deployment that has
// never drifted answers.
func deployed(stream streamset.Stream, replicas int) streamset.Info {
	config := stream.Config
	config.Replicas = replicas
	if stream.Name == bus.Stream {
		config.Metadata = map[string]string{bus.StreamIDMetadataKey: "an-id"}
	}
	return streamset.Info{StreamInfo: nats.StreamInfo{Config: config}}
}

// Every declared stream with a configuration, ENVOY_MEASURE among them now that the operator makes
// it, is held to the deployment's copies: at fewer, Drift reports num_replicas, naming the two
// counts, so check fails naming the stream; at the count, or above it, it reports nothing.
func TestEveryDeclaredStreamIsHeldToTheDeploymentsCopies(t *testing.T) {
	streams := streamset.Declared(streamset.Settings{Replicas: 3})
	measured := 0
	for _, stream := range streams {
		if stream.Transient {
			continue
		}
		t.Run(stream.Name, func(t *testing.T) {
			require.Empty(t, streamset.Drift(stream, deployed(stream, 3)), "the stream as the build makes it drifts")
			require.Empty(t, streamset.Drift(stream, deployed(stream, 5)), "copies above the count drift")
			require.Equal(t, []streamset.Difference{{Setting: "num_replicas", Want: "3", Got: "1", Raise: true}},
				streamset.Drift(stream, deployed(stream, 1)), "a stream at one copy of three is not reported")
		})
		if stream.Name == streamset.MeasureStream {
			measured++
		}
	}
	require.Equal(t, 1, measured, "the declared set holds no ENVOY_MEASURE, so its row above read nothing")
}
