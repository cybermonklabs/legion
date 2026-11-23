package natscallout

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/natsgrant"
)

// The operator's grant while a hold is prepared raises a session consumer's threshold on
// ENVOY_NOTIFICATIONS with an update, which the grant outside a hold refuses; it still deletes no
// protected stream and no consumer, and lists no stream. The hold's cohort is a record of the
// broker's Postgres, written through the routes (its half of this row is the routes' test).
//
// It fails when prepare's grant loses the consumer update, or released's admits it (the
// control, the first update, made under released before the row turns prepare), or when prepare's
// admits a delete or the stream list.
func TestThePrepareOperatorRaisesThresholdsAndWritesTheCohort(t *testing.T) {
	r := newOperatorRig(t, nil)
	r.setPhase(t, natsgrant.OperatorPhaseReleased)
	_, js := r.admin(t)
	r.makeStreams(t, 1, 16, 0)
	_, err := js.AddConsumer(bus.Stream, &nats.ConsumerConfig{Name: "session-a", AckPolicy: nats.AckExplicitPolicy, InactiveThreshold: time.Hour})
	require.NoError(t, err)
	operator := r.connectOperator(t, hourLong(operatorAccount))
	requireOperatorDecision(t, r.decisions.after(t, operatorName, 0), natsgrant.OperatorPhaseReleased)

	var info struct {
		Config map[string]any `json:"config"`
	}
	require.NoError(t, json.Unmarshal(succeeds(t, operator.Conn, "$JS.API.CONSUMER.INFO."+bus.Stream+".session-a", nil).raw, &info))
	info.Config["inactive_threshold"] = (2 * time.Hour).Nanoseconds()
	raise := map[string]any{"stream_name": bus.Stream, "config": info.Config, "action": "update"}
	subject := "$JS.API.CONSUMER.CREATE." + bus.Stream + ".session-a"
	// The control: the same update under released is refused.
	operator.refused(t, subject, raise)

	r.setPhase(t, natsgrant.OperatorPhasePrepare)
	seen := r.decisions.count(operatorName)
	operator.reconnect(t)
	requireOperatorDecision(t, r.decisions.after(t, operatorName, seen), natsgrant.OperatorPhasePrepare)
	succeeds(t, operator.Conn, subject, raise)
	consumer, err := js.ConsumerInfo(bus.Stream, "session-a")
	require.NoError(t, err)
	require.Equal(t, 2*time.Hour, consumer.Config.InactiveThreshold, "the prepare operator's update did not raise the threshold")

	operator.refused(t, "$JS.API.STREAM.DELETE."+kvBucket(bus.SessionBucket), nil)
	operator.refused(t, "$JS.API.CONSUMER.DELETE."+bus.Stream+".x", nil)
	operator.refused(t, "$JS.API.STREAM.LIST", nil)
}

// The infos of the protected buckets that are not copied, KV_legion_agents, KV_envoy_dpop_proofs
// and KV_envoy_webhook_redelivery, which the hold's preflight reads before the row turns held, are
// answered under prepare and under held, and refused under released.
//
// Run by hand, the control is prepare's grant without them, as it was before they were added: the
// info of KV_legion_agents is refused under prepare, and the test fails naming the stream.
func TestThePrepareOperatorReadsEveryProtectedStream(t *testing.T) {
	r := newOperatorRig(t, nil)
	r.setPhase(t, natsgrant.OperatorPhaseReleased)
	r.makeStreams(t, 1, 16, 0)
	operator := r.connectOperator(t, hourLong(operatorAccount))
	requireOperatorDecision(t, r.decisions.after(t, operatorName, 0), natsgrant.OperatorPhaseReleased)
	for _, bucket := range otherProtected {
		operator.refused(t, "$JS.API.STREAM.INFO."+kvBucket(bucket), nil)
	}
	for _, phase := range []natsgrant.OperatorPhase{natsgrant.OperatorPhasePrepare, natsgrant.OperatorPhaseHeld} {
		r.setPhase(t, phase)
		seen := r.decisions.count(operatorName)
		operator.reconnect(t)
		requireOperatorDecision(t, r.decisions.after(t, operatorName, seen), phase)
		for _, bucket := range otherProtected {
			msg, err := operator.Request("$JS.API.STREAM.INFO."+kvBucket(bucket), nil, 5*time.Second)
			require.NoError(t, err, "the %s operator's info of %s got no answer", phase, kvBucket(bucket))
			var answer apiAnswer
			require.NoError(t, json.Unmarshal(msg.Data, &answer))
			require.Nil(t, answer.Error, "the %s operator's info of %s: %s", phase, kvBucket(bucket), msg.Data)
		}
	}
}
