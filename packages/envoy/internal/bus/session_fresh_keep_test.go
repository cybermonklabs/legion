package bus

import (
	"context"
	"testing"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/sjawhar/envoy/consumernames"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/testnats"
)

// In the fresh mode KeepListed keeps a listed consumer only when the session's record names it as
// the current. A consumer of the session that the record retired, made after the current with the
// session's own metadata, filters and configuration (as an abandoned candidate whose create landed
// late is), passes every other test KeepListed applies: no message awaits an ack, its filters are
// the session's, and the owner check takes it. It is not kept, and the relay would not pull it; a
// relay that pulled it beside the current would deliver each message twice. The control is the
// record's current, which is kept. Before this row, deleting the current check in freshKeepListed
// left every test of the package green (the review's planted fault).
func TestTheFreshModeKeepsOnlyTheRecordsCurrentListed(t *testing.T) {
	_, consumers, store := freshSessionConsumersOnStream(t)
	ctx := context.Background()
	const session = "ses_fresh_keep_listed"
	filters := []string{contracts.LegacyAgentSubject(session)}
	start, err := consumers.Ensure(ctx, session, filters, 0)
	if err != nil {
		t.Fatal(err)
	}
	conn := testnats.Connect(t, testnats.URL(t))
	t.Cleanup(conn.Close)
	js, err := jetstream.New(conn)
	if err != nil {
		t.Fatal(err)
	}
	current, err := js.Consumer(ctx, Stream, start.Name)
	if err != nil {
		t.Fatal(err)
	}
	config := current.CachedInfo().Config
	config.Name = consumernames.FreshName(consumernames.SessionPrefix)
	config.Durable = config.Name
	retired, err := js.CreateConsumer(ctx, Stream, config)
	if err != nil {
		t.Fatalf("create the retired name %s: %v", config.Name, err)
	}
	record, revision, err := store.Read(ctx, session)
	if err != nil {
		t.Fatal(err)
	}
	record.Retired = append(record.Retired, config.Name)
	if _, err := store.Write(ctx, session, record, revision); err != nil {
		t.Fatal(err)
	}
	if _, kept, err := consumers.KeepListed(ctx, session, filters, retired.CachedInfo()); err != nil || kept {
		t.Fatalf("KeepListed of the retired %s, made after the current %s = kept %t, %v; want not kept", config.Name, start.Name, kept, err)
	}
	if got, kept, err := consumers.KeepListed(ctx, session, filters, current.CachedInfo()); err != nil || !kept || got.Name != start.Name {
		t.Fatalf("control: KeepListed of the current %s = %+v, kept %t, %v; want it kept", start.Name, got, kept, err)
	}
}
