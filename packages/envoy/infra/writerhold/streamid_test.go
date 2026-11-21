package writerhold

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/infra/streams"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/holdrecord"
)

// fakeStores are daemon stores whose StreamIDs the census reconciles, by family daemon/<P>, as
// consumernames.ReconcileDaemonStreamIDs does: it reads ENVOY_NOTIFICATIONS' identity on the
// census's connection, writes it where a store's differs, and returns one line per store it
// changed. Each store must have its --daemon-database entry.
type fakeStores struct {
	mu  sync.Mutex
	ids map[string]string
}

func (f *fakeStores) reconcile(ctx context.Context, nc *nats.Conn, families, daemonDatabases []string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	js, err := jetstream.New(nc)
	if err != nil {
		return nil, err
	}
	s, err := js.Stream(ctx, bus.Stream)
	if err != nil {
		return nil, err
	}
	id := s.CachedInfo().Config.Metadata[bus.StreamIDMetadataKey]
	if id == "" {
		return nil, bus.ErrNoStreamID
	}
	databases := map[string]bool{}
	for _, entry := range daemonDatabases {
		project, _, _ := strings.Cut(entry, "=")
		databases[project] = true
	}
	sorted := append([]string(nil), families...)
	sort.Strings(sorted)
	var changed []string
	for _, family := range sorted {
		project, ok := strings.CutPrefix(family, "daemon/")
		if !ok {
			continue
		}
		if !databases[project] {
			return nil, fmt.Errorf("%s has no --daemon-database", family)
		}
		if old := f.ids[project]; old != id {
			f.ids[project] = id
			changed = append(changed, fmt.Sprintf("%s: stream_id %q -> %q", family, old, id))
		}
	}
	return changed, nil
}

// snapshot is the stores' ids now.
func (f *fakeStores) snapshot() map[string]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]string{}
	for k, v := range f.ids {
		out[k] = v
	}
	return out
}

// notificationsID reads ENVOY_NOTIFICATIONS' identity as the operator.
func (h *heldStack) notificationsID() string {
	h.t.Helper()
	nc, err := h.s.ConnectOperator(context.Background())
	require.NoError(h.t, err)
	defer nc.Close()
	js, err := jetstream.New(nc)
	require.NoError(h.t, err)
	s, err := js.Stream(context.Background(), bus.Stream)
	require.NoError(h.t, err)
	return s.CachedInfo().Config.Metadata[bus.StreamIDMetadataKey]
}

// doneOutputs are command's latest done entry's outputs in d.
func doneOutputs(d holdrecord.Done, command string) map[string]string {
	var outputs map[string]string
	for _, op := range d.Ops {
		if op.Command == command && op.State == "done" {
			outputs = op.Outputs
		}
	}
	return outputs
}

// The census reconciles each daemon store's StreamID with ENVOY_NOTIFICATIONS' identity. After the
// stream is recreated (a new identity), the census has every daemon store's StreamID rewritten,
// prints each change as a CHANGED line and names it in its done entry (stream_id.daemon/<P>). The
// control is a census with no writer, which is UNKNOWN (exit 2), writes no done entry and leaves
// every store at the old identity. Run again on the stream unchanged, the census changes nothing
// and names no store. It fails when a recreated stream leaves a daemon store with the identity of a
// stream that is gone.
func TestHoldTheCensusReconcilesTheDaemonStoresStreamID(t *testing.T) {
	h := newHeldStack(t)
	h.seedProtectedStreams()
	ctx := context.Background()
	stub := func(context.Context, *nats.Conn) ([]streams.ConsumerShape, error) { return nil, nil }
	old := h.notificationsID()
	require.NotEmpty(t, old)
	stores := &fakeStores{ids: map[string]string{"A": old, "B": old}}
	families := []string{"--family", "session", "--family", "daemon/A", "--family", "daemon/B",
		"--daemon-database", "A=/dev/null", "--daemon-database", "B=/dev/null"}

	// ENVOY_NOTIFICATIONS recreated, as a hub-restore's snapshot or a streams update that made it
	// again would leave it: deleted under a hold of no steps, then made by `streams update`.
	id := h.s.Prepare(t, holdrecord.WorkflowStreamsUpdate)
	h.s.HoldWithoutTheSteps(t, id)
	nc, err := h.s.ConnectOperator(ctx)
	require.NoError(t, err)
	js, err := jetstream.New(nc)
	require.NoError(t, err)
	require.NoError(t, js.DeleteStream(ctx, bus.Stream))
	nc.Close()
	code, _, _ := h.streamsRun(nil, "update", "--hold", id)
	require.Zero(t, code, "the update that makes the stream again")
	code, _, _ = h.run(actionRelease, "--hold", id, "--reason", "the stream is made again")
	require.Zero(t, code)
	current := h.notificationsID()
	require.NotEqual(t, old, current, "the recreated stream kept its identity")
	h.awaitStreamLeaders()

	// The first census sets its hold and stops at its convert check; every family's cutover row is
	// then written done under that hold, so each census after it reaches its reconcile.
	code, _, _ = h.streamsRun(stub, append([]string{"census"}, families...)...)
	require.Equal(t, 1, code, "the census before any family is converted")
	census := h.row().HoldID
	for _, family := range []string{"session", "daemon/A", "daemon/B"} {
		_, err := h.s.Records.PutCutover(ctx, family, "done", census)
		require.NoError(t, err)
	}

	// The control: no writer.
	code, _, stderr := h.streamsRun(stub, append([]string{"census", "--hold", census}, families...)...)
	require.Equal(t, 2, code, "control: a census with no writer of the stores")
	require.Contains(t, stderr, ErrNoStoreWriter.Error())
	require.NotContains(t, h.journalCommands(census), "streams census done")
	require.Equal(t, map[string]string{"A": old, "B": old}, stores.snapshot(), "control: the stores keep the old identity")

	code, stdout, _ := h.streamsRunWith(stub, stores.reconcile, append([]string{"census", "--hold", census}, families...)...)
	require.Zero(t, code, "the census with a writer")
	require.Equal(t, map[string]string{"A": current, "B": current}, stores.snapshot())
	require.Contains(t, stdout, fmt.Sprintf("CHANGED daemon/A: stream_id %q -> %q", old, current))
	require.Contains(t, stdout, fmt.Sprintf("CHANGED daemon/B: stream_id %q -> %q", old, current))
	outputs := doneOutputs(h.journal(census), CommandStreamsCensus)
	require.Contains(t, outputs, "stream_id.daemon/A")
	require.Contains(t, outputs, "stream_id.daemon/B")
	code, _, _ = h.run(actionRelease, "--hold", census, "--reason", "the census is done")
	require.Zero(t, code)
	h.awaitStreamLeaders()

	// The stream unchanged: the next census changes nothing and names no store.
	code, stdout, _ = h.streamsRunWith(stub, stores.reconcile, append([]string{"census"}, families...)...)
	require.Zero(t, code, "the census of an unchanged stream")
	again := h.row().HoldID
	require.NotContains(t, stdout, "CHANGED daemon/")
	for key := range doneOutputs(h.journal(again), CommandStreamsCensus) {
		require.NotContains(t, key, "stream_id.", "the census of an unchanged stream named a store")
	}
}
