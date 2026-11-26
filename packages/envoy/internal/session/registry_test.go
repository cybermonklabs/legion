package session

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/bus/bustest"
	"github.com/sjawhar/envoy/internal/clock/clocktest"
	"github.com/sjawhar/envoy/internal/kvwatch"
	"github.com/sjawhar/envoy/internal/logtest"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// setupNATS connects t to the package's shared NATS server, in the JetStream account testnats.URL
// hands t, where no earlier test made a session bucket. The client owns the stream, so its close
// waits for its stream keeper to end, and the cleanup closes it through the shared wait
// (bustest.Close).
func setupNATS(t *testing.T) *bus.Client {
	t.Helper()
	client, err := bus.ConnectOwningStream([]string{testnats.URL(t)}, bus.WithReplicas(1))
	if err != nil {
		t.Fatalf("failed to connect bus: %v", err)
	}
	t.Cleanup(func() { bustest.Close(t, client) })
	return client
}

func TestSessionEntry_HostOrMachine(t *testing.T) {
	reported := SessionEntry{MachineID: "example-listener", Host: "example-host-b"}
	if got := reported.HostOrMachine(); got != "example-host-b" {
		t.Fatalf("HostOrMachine() = %q, want the reported host", got)
	}

	legacy := SessionEntry{MachineID: "example-listener"}
	if got := legacy.HostOrMachine(); got != "example-listener" {
		t.Fatalf("HostOrMachine() = %q, want the fallback to MachineID for a session with no reported host", got)
	}
}

// An entry is JSON in the session bucket, so a place reaches the bucket and comes back whole
// with nothing to migrate. A session in no pane must write no tmux_place key at all, rather than
// a null or an empty place: a cluster whose sessions run outside tmux pays no bytes per entry,
// and a reader never mistakes an empty place for a pane that exists.
func TestSessionEntryTmuxPlaceSurvivesTheBucketAndIsOmittedWhenNil(t *testing.T) {
	placed := SessionEntry{MachineID: "example-listener", TmuxPlace: &TmuxPlace{
		Socket:      "/tmp/tmux-1000/default",
		ServerPID:   "4242",
		SessionName: "legion",
		WindowID:    "@7",
		WindowIndex: 3,
		WindowName:  "infra-883",
		PaneID:      "%12",
		PaneIndex:   1,
		Layout:      "a1b2,180x44,0,0,7",
		Zoomed:      true,
	}}
	stored, err := json.Marshal(placed)
	if err != nil {
		t.Fatalf("encode the placed entry: %v", err)
	}
	var read SessionEntry
	if err := json.Unmarshal(stored, &read); err != nil {
		t.Fatalf("decode the placed entry: %v", err)
	}
	if read.TmuxPlace == nil || *read.TmuxPlace != *placed.TmuxPlace {
		t.Fatalf("place read back = %+v, want %+v", read.TmuxPlace, placed.TmuxPlace)
	}

	outside, err := json.Marshal(SessionEntry{MachineID: "example-listener"})
	if err != nil {
		t.Fatalf("encode the entry of a session outside tmux: %v", err)
	}
	if bytes.Contains(outside, []byte("tmux_place")) {
		t.Fatalf("entry of a session outside tmux = %s, want no tmux_place key", outside)
	}

	// The client sends the other nine members when a finely split window grew the layout past
	// what a registration carries, and the entry must carry that place without an empty layout.
	dropped := *placed.TmuxPlace
	dropped.Layout = ""
	encoded, err := json.Marshal(dropped)
	if err != nil {
		t.Fatalf("encode the place whose layout the client dropped: %v", err)
	}
	if bytes.Contains(encoded, []byte("layout")) {
		t.Fatalf("place with no layout = %s, want no layout key", encoded)
	}
}

// stagedSessionTTL is the session TTL of a row whose verdict reads a registration back but is not
// about its expiry: the bucket's age, which nats-server drops a record at, and the deadline Get
// answers a row gone past (expiryFor). A row about that deadline takes it too, and judges the
// deadline on a clock it sets itself (WithSessionClock), so only the bucket's age runs on a clock
// the row does not move. It is staged state for those clocks, not a wait: longer than any
// `go test -timeout` CI gives this package, so a registration the row wrote still reads back
// however long the machine takes, where ten seconds would pass on a loaded one.
const stagedSessionTTL = 24 * time.Hour

func TestSessionRegistry_PutAndGet(t *testing.T) {
	client := setupNATS(t)
	reg, err := OpenSessionRegistry(t.Context(), client.Conn, WithSessionReplicas(1), WithSessionTTL(stagedSessionTTL))
	if err != nil {
		t.Fatalf("failed to open session registry: %v", err)
	}

	entry := SessionEntry{Port: 13381, MachineID: "test-machine", Dir: "/test"}
	if err := reg.Put("ses_test", entry); err != nil {
		t.Fatalf("put failed: %v", err)
	}

	got, err := reg.Get("ses_test")
	if err != nil {
		t.Fatalf("get failed: %v", err)
	}
	if got.Port != 13381 {
		t.Fatalf("expected port 13381, got %d", got.Port)
	}
	if got.MachineID != "test-machine" {
		t.Fatalf("expected machine_id test-machine, got %s", got.MachineID)
	}
}

// The stored entry keeps the plugin a session registered through and that plugin's build, so the
// session list read back from the bucket names them.
func TestSessionRegistry_PutAndGetKeepsThePluginAndItsVersion(t *testing.T) {
	client := setupNATS(t)
	reg, err := OpenSessionRegistry(t.Context(), client.Conn, WithSessionReplicas(1), WithSessionTTL(stagedSessionTTL))
	if err != nil {
		t.Fatalf("failed to open session registry: %v", err)
	}

	entry := SessionEntry{MachineID: "test-machine", Plugin: "@sjawhar/claude-envoy", PluginVersion: "afe10b2f3e1b"}
	if err := reg.Put("ses_plugin", entry); err != nil {
		t.Fatalf("put failed: %v", err)
	}

	got, err := reg.Get("ses_plugin")
	if err != nil {
		t.Fatalf("get failed: %v", err)
	}
	if got.Plugin != "@sjawhar/claude-envoy" || got.PluginVersion != "afe10b2f3e1b" {
		t.Fatalf("got %+v, want plugin @sjawhar/claude-envoy at afe10b2f3e1b", got)
	}
}

// The stored entry keeps the client instance that registered the session, so a soft claim naming
// a predecessor can read whose instance that predecessor belongs to.
func TestSessionRegistry_PutAndGetKeepsTheClientInstanceID(t *testing.T) {
	client := setupNATS(t)
	reg, err := OpenSessionRegistry(t.Context(), client.Conn, WithSessionReplicas(1), WithSessionTTL(stagedSessionTTL))
	if err != nil {
		t.Fatalf("failed to open session registry: %v", err)
	}

	entry := SessionEntry{Port: 13381, MachineID: "test-machine", ClientInstanceID: "4f1c2a9e8b7d6c5a4f1c2a9e8b7d6c5a"}
	if err := reg.Put("ses_instance", entry); err != nil {
		t.Fatalf("put failed: %v", err)
	}

	got, err := reg.Get("ses_instance")
	if err != nil {
		t.Fatalf("get failed: %v", err)
	}
	if got.ClientInstanceID != "4f1c2a9e8b7d6c5a4f1c2a9e8b7d6c5a" {
		t.Fatalf("got %+v, want the registered client instance id", got)
	}
}

// A re-registration carrying no client instance id clears the one stored: the handler builds the
// entry fresh from each subscribe body, so a client that predates the field (or one that stopped
// sending it) leaves the session with the empty id that keeps the old soft-claim rule, rather
// than with another instance's id it no longer belongs to.
func TestSessionRegistry_PutWithoutClientInstanceIDClearsTheStoredOne(t *testing.T) {
	client := setupNATS(t)
	reg, err := OpenSessionRegistry(t.Context(), client.Conn, WithSessionReplicas(1), WithSessionTTL(stagedSessionTTL))
	if err != nil {
		t.Fatalf("failed to open session registry: %v", err)
	}

	registered := SessionEntry{Port: 13381, MachineID: "test-machine", ClientInstanceID: "4f1c2a9e8b7d6c5a4f1c2a9e8b7d6c5a"}
	if err := reg.Put("ses_instance_cleared", registered); err != nil {
		t.Fatalf("first put failed: %v", err)
	}

	// The same process heartbeating again (same port, so no claim arbitration intervenes),
	// this time with no id in its payload.
	if err := reg.Put("ses_instance_cleared", SessionEntry{Port: 13381, MachineID: "test-machine"}); err != nil {
		t.Fatalf("second put failed: %v", err)
	}

	got, err := reg.Get("ses_instance_cleared")
	if err != nil {
		t.Fatalf("get failed: %v", err)
	}
	if got.ClientInstanceID != "" {
		t.Fatalf("got %+v, want the stored client instance id cleared by a registration without one", got)
	}
}

func TestSessionRegistry_GetNotFound(t *testing.T) {
	client := setupNATS(t)
	reg, err := OpenSessionRegistry(t.Context(), client.Conn, WithSessionReplicas(1), WithSessionTTL(10*time.Second))
	if err != nil {
		t.Fatalf("failed to open session registry: %v", err)
	}

	_, err = reg.Get("ses_nonexistent")
	if err == nil {
		t.Fatal("expected error for missing session")
	}
}

// A row leaves the registry one TTL after its record was written, on the registry's own clock, and
// not before: the deadline is the record's Created, nats-server's stamp of the write, a TTL on
// (expiryFor). The bucket's age is far past the run, so nats-server keeps the record while the row
// runs, and the clock the row sets is the one that judges the deadline.
func TestSessionRegistry_TTLExpiry(t *testing.T) {
	const ttl = stagedSessionTTL
	client := setupNATS(t)
	clk := clocktest.New(time.Now())
	reg, err := OpenSessionRegistry(t.Context(), client.Conn, WithSessionReplicas(1), WithSessionTTL(ttl), WithSessionClock(clk.Now))
	if err != nil {
		t.Fatalf("failed to open session registry: %v", err)
	}

	entry := SessionEntry{Port: 13381, MachineID: "test-machine", Dir: "/test"}
	if err := reg.Put("ses_expiry", entry); err != nil {
		t.Fatalf("put failed: %v", err)
	}

	if _, err := reg.Get("ses_expiry"); err != nil {
		t.Fatalf("expected entry to exist immediately: %v", err)
	}

	// The registry takes the record's Created for the row's deadline once it has the record from
	// the bucket, which Refresh reads and applies as the watch would, so the row does not wait on
	// the watch. At that deadline the entry still answers; a millisecond past it, it is gone.
	written := sessionRecord(t, reg, "ses_expiry")
	if _, err := reg.Refresh(context.Background(), "ses_expiry"); err != nil {
		t.Fatalf("read ses_expiry back into the registry: %v", err)
	}
	clk.Set(written.Created().Add(ttl))
	if got, err := reg.Get("ses_expiry"); err != nil {
		t.Fatalf("Get(ses_expiry) at its deadline, one TTL after its record was written at %s, = %+v, %v; want the entry until the deadline has passed",
			written.Created().Format(time.RFC3339Nano), got, err)
	}
	clk.Set(written.Created().Add(ttl + time.Millisecond))
	testwait.Until(t, "Get(ses_expiry) to answer the entry gone once the registry's clock passed its record's write by the TTL", func() (string, bool) {
		got, err := reg.Get("ses_expiry")
		return fmt.Sprintf("Get(ses_expiry) = %+v, %v, the registry's clock at %s, the record written at %s", got, err, clk.Now().Format(time.RFC3339Nano), written.Created().Format(time.RFC3339Nano)), err != nil
	})
}

// A refresh renews an entry's life: the registry measures a row's deadline from its record's
// Created, nats-server's stamp of the write (expiryFor), so a second write of the same id carries a
// later Created, and the row outlives one written beside the first write and never refreshed. The
// bucket's age is far past the run, so nats-server drops neither record while the row runs, and
// the registry's own clock, which the row sets, judges both deadlines.
func TestSessionRegistry_PutRefreshesTTL(t *testing.T) {
	const ttl = stagedSessionTTL
	client := setupNATS(t)
	clk := clocktest.New(time.Now())
	reg, err := OpenSessionRegistry(t.Context(), client.Conn, WithSessionReplicas(1), WithSessionTTL(ttl), WithSessionClock(clk.Now))
	if err != nil {
		t.Fatalf("failed to open session registry: %v", err)
	}

	entry := SessionEntry{Port: 13381, MachineID: "test-machine", Dir: "/test"}
	// ses_control is written beside the first write and never refreshed, so its deadline is the
	// one the first write would have kept.
	for _, sessionID := range []string{"ses_refresh", "ses_control"} {
		if err := reg.Put(sessionID, entry); err != nil {
			t.Fatalf("put %s failed: %v", sessionID, err)
		}
	}
	first, control := sessionRecord(t, reg, "ses_refresh"), sessionRecord(t, reg, "ses_control")
	// nats-server stamps Created by the machine's wall clock, the one this process reads, so the
	// refresh is written once that clock has passed the control's stamp: it then carries a later
	// one.
	testwait.Until(t, "the wall clock nats-server stamps a write by to pass the control record's stamp", func() (string, bool) {
		now := time.Now()
		return fmt.Sprintf("the wall clock at %s, the control's stamp %s", now.Format(time.RFC3339Nano), control.Created().Format(time.RFC3339Nano)), now.After(control.Created())
	})
	if err := reg.Put("ses_refresh", entry); err != nil {
		t.Fatalf("refresh put failed: %v", err)
	}
	refreshed := sessionRecord(t, reg, "ses_refresh")
	if refreshed.Revision() <= first.Revision() || !refreshed.Created().After(control.Created()) {
		t.Fatalf("the refresh left ses_refresh at revision %d written %s (its first write was revision %d at %s, the control's %s); want a later revision written after both",
			refreshed.Revision(), refreshed.Created().Format(time.RFC3339Nano), first.Revision(), first.Created().Format(time.RFC3339Nano), control.Created().Format(time.RFC3339Nano))
	}
	// The registry takes each record's Created for its row's deadline once it has the record from
	// the bucket, which Refresh reads and applies as the watch would, so neither row waits on the
	// watch.
	for _, sessionID := range []string{"ses_refresh", "ses_control"} {
		if _, err := reg.Refresh(context.Background(), sessionID); err != nil {
			t.Fatalf("read %s back into the registry: %v", sessionID, err)
		}
	}

	// Past the control's deadline on the registry's clock and short of the refresh's: halfway
	// between the two records' stamps, a TTL on.
	clk.Set(control.Created().Add(ttl + refreshed.Created().Sub(control.Created())/2))
	testwait.Until(t, "Get(ses_control), written beside the first write and never refreshed, to answer it gone", func() (string, bool) {
		got, err := reg.Get("ses_control")
		return fmt.Sprintf("Get(ses_control) = %+v, %v", got, err), err != nil
	})

	if _, err := reg.Get("ses_refresh"); err != nil {
		t.Fatalf("expected entry to still exist after TTL refresh, got: %v", err)
	}
}

// sessionRecord is sessionID's record as the session bucket holds it, with its revision and
// nats-server's stamp of its write.
func sessionRecord(t *testing.T, reg *SessionRegistry, sessionID string) natsgo.KeyValueEntry {
	t.Helper()
	record, err := reg.watcher.KV().Get(sessionID)
	if err != nil {
		t.Fatalf("read %s's record from the bucket: %v", sessionID, err)
	}
	return record
}

func TestSessionRegistry_Delete(t *testing.T) {
	client := setupNATS(t)
	reg, err := OpenSessionRegistry(t.Context(), client.Conn, WithSessionReplicas(1), WithSessionTTL(10*time.Second))
	if err != nil {
		t.Fatalf("failed to open session registry: %v", err)
	}

	entry := SessionEntry{Port: 13381, MachineID: "test-machine", Dir: "/test"}
	reg.Put("ses_del", entry)

	if err := reg.Delete(context.Background(), "ses_del"); err != nil {
		t.Fatalf("delete failed: %v", err)
	}

	_, err = reg.Get("ses_del")
	if err == nil {
		t.Fatal("expected error after delete")
	}
}

// A deregistration that names the instance that registered the session deletes it, as the
// unconditional Delete does: the /clear handoff this guards is a client deregistering its own
// session before it claims that session's role.
func TestSessionRegistryDeleteIf_DeletesTheSameInstancesEntry(t *testing.T) {
	reg := openRegistryForInstanceDelete(t)
	const sessionID = "ses_instance_same"
	putForInstanceDelete(t, reg, sessionID, "4f1c2a9e8b7d6c5a4f1c2a9e8b7d6c5a")

	outcome, err := reg.DeleteIf(context.Background(), sessionID, DeleteConditions{ClientInstanceID: "4f1c2a9e8b7d6c5a4f1c2a9e8b7d6c5a"})
	if err != nil {
		t.Fatalf("DeleteIf: %v", err)
	}
	if outcome != EntryDeleted {
		t.Fatalf("DeleteIf = %s, want %s for an entry its own client instance deregistered", outcome, EntryDeleted)
	}
	if _, err := reg.watcher.KV().Get(sessionID); !errors.Is(err, natsgo.ErrKeyNotFound) {
		t.Fatalf("read the deleted key: %v, want nats.ErrKeyNotFound", err)
	}
	if _, err := reg.Get(sessionID); err == nil {
		t.Fatal("the cache still answers with the deleted entry")
	}
}

// The hole this closes: another process resumed the session id and registered under its own
// instance, so the earlier instance's deregistration must leave that live entry alone — and still
// answer, since there is nothing for it to retry.
func TestSessionRegistryDeleteIf_KeepsAnotherInstancesEntry(t *testing.T) {
	reg := openRegistryForInstanceDelete(t)
	const sessionID = "ses_instance_other"
	putForInstanceDelete(t, reg, sessionID, "b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2")

	outcome, err := reg.DeleteIf(context.Background(), sessionID, DeleteConditions{ClientInstanceID: "4f1c2a9e8b7d6c5a4f1c2a9e8b7d6c5a"})
	if err != nil {
		t.Fatalf("DeleteIf: %v", err)
	}
	if outcome != EntryKeptForAnotherInstance {
		t.Fatalf("DeleteIf = %s, want %s for another client instance's live entry", outcome, EntryKeptForAnotherInstance)
	}
	stored, err := reg.Get(sessionID)
	if err != nil {
		t.Fatalf("Get the kept entry: %v", err)
	}
	if stored.ClientInstanceID != "b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2" {
		t.Fatalf("got %+v, want the other client instance's entry kept as it was", stored)
	}
}

// An entry a client that predates the field registered carries no instance id, and a client that
// names one still deregisters it: that client is the only one that can be deregistering it.
func TestSessionRegistryDeleteIf_DeletesAnEntryThatNamesNoInstance(t *testing.T) {
	reg := openRegistryForInstanceDelete(t)
	const sessionID = "ses_instance_absent_stored"
	putForInstanceDelete(t, reg, sessionID, "")

	outcome, err := reg.DeleteIf(context.Background(), sessionID, DeleteConditions{ClientInstanceID: "4f1c2a9e8b7d6c5a4f1c2a9e8b7d6c5a"})
	if err != nil {
		t.Fatalf("DeleteIf: %v", err)
	}
	if outcome != EntryDeleted {
		t.Fatalf("DeleteIf = %s, want %s for an entry that carries no client instance id", outcome, EntryDeleted)
	}
	if _, err := reg.Get(sessionID); err == nil {
		t.Fatal("the cache still answers with the deleted entry")
	}
}

// A caller that names no instance id compares nothing, so it deletes whatever is stored, as the
// route does for a client that sends no id of its own.
func TestSessionRegistryDeleteIf_WithNoInstanceIDDeletesWhateverIsStored(t *testing.T) {
	reg := openRegistryForInstanceDelete(t)
	const sessionID = "ses_instance_absent_requested"
	putForInstanceDelete(t, reg, sessionID, "b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2")

	outcome, err := reg.DeleteIf(context.Background(), sessionID, DeleteConditions{})
	if err != nil {
		t.Fatalf("DeleteIf: %v", err)
	}
	if outcome != EntryDeleted {
		t.Fatalf("DeleteIf = %s, want %s for a request naming no client instance id", outcome, EntryDeleted)
	}
	if _, err := reg.Get(sessionID); err == nil {
		t.Fatal("the cache still answers with the deleted entry")
	}
}

// A key that is not there is already gone, which is what the caller asked for: deleted, no error,
// as Delete is idempotent today.
func TestSessionRegistryDeleteIf_AbsentKeyAnswersDeleted(t *testing.T) {
	reg := openRegistryForInstanceDelete(t)

	outcome, err := reg.DeleteIf(context.Background(), "ses_instance_missing", DeleteConditions{ClientInstanceID: "4f1c2a9e8b7d6c5a4f1c2a9e8b7d6c5a"})
	if err != nil {
		t.Fatalf("DeleteIf: %v", err)
	}
	if outcome != EntryDeleted {
		t.Fatalf("DeleteIf = %s, want %s for an absent key", outcome, EntryDeleted)
	}
}

// The deleting instance's own heartbeat lands between the read and the delete, so JetStream
// refuses the delete with wrong last sequence: the sequence runs once more and the entry goes,
// where answering "kept" would leave the predecessor alive and make the claim that follows a 409.
func TestSessionRegistryDeleteIf_RetriesOnceAfterALostCompareAndSwap(t *testing.T) {
	reg := openRegistryForInstanceDelete(t)
	const (
		sessionID  = "ses_instance_race"
		instanceID = "4f1c2a9e8b7d6c5a4f1c2a9e8b7d6c5a"
	)
	putForInstanceDelete(t, reg, sessionID, instanceID)
	heartbeat, err := json.Marshal(SessionEntry{
		Port:             13381,
		MachineID:        "test-machine",
		ClientInstanceID: instanceID,
		UpdatedAt:        time.Now().UnixMilli(),
	})
	if err != nil {
		t.Fatalf("encode the interleaved heartbeat: %v", err)
	}
	deletes := heartbeatBeforeDeletes(t, reg, sessionID, heartbeat, 1)

	outcome, err := reg.DeleteIf(context.Background(), sessionID, DeleteConditions{ClientInstanceID: instanceID})
	if err != nil {
		t.Fatalf("DeleteIf: %v", err)
	}
	if outcome != EntryDeleted {
		t.Fatalf("DeleteIf = %s, want %s where its own heartbeat only lost it one compare-and-swap", outcome, EntryDeleted)
	}
	if got := deletes(); got != 2 {
		t.Fatalf("made %d fenced deletes, want the one retry (2)", got)
	}
	if _, err := reg.watcher.KV().Get(sessionID); !errors.Is(err, natsgo.ErrKeyNotFound) {
		t.Fatalf("read the deleted key: %v, want nats.ErrKeyNotFound", err)
	}
	if _, err := reg.Get(sessionID); err == nil {
		t.Fatal("the cache still answers with the deleted entry")
	}
}

// A value this build cannot read names no instance even when its raw JSON carries one:
// json.Unmarshal fills the fields it decoded before the type error, so the id another client
// instance wrote would keep the entry if the half-decoded value were trusted. The branch drops
// it, and the deregistration is not blocked by a record no reader can act on.
func TestSessionRegistryDeleteIf_DeletesAMalformedValueThatNamesAnotherInstance(t *testing.T) {
	logs := captureSessionRegistryLogs(t)
	reg := openRegistryForInstanceDelete(t)
	const sessionID = "ses_instance_malformed"
	if _, err := reg.watcher.KV().Put(sessionID, []byte(`{"client_instance_id":"b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2","port":"not a port"}`)); err != nil {
		t.Fatalf("put the malformed value: %v", err)
	}

	outcome, err := reg.DeleteIf(context.Background(), sessionID, DeleteConditions{ClientInstanceID: "4f1c2a9e8b7d6c5a4f1c2a9e8b7d6c5a"})
	if err != nil {
		t.Fatalf("DeleteIf: %v", err)
	}
	if outcome != EntryDeleted {
		t.Fatalf("DeleteIf = %s, want %s for a value this build cannot read", outcome, EntryDeleted)
	}
	if _, err := reg.watcher.KV().Get(sessionID); !errors.Is(err, natsgo.ErrKeyNotFound) {
		t.Fatalf("read the deleted key: %v, want nats.ErrKeyNotFound", err)
	}
	logtest.AwaitLogged(t, logs, "DeleteIf's WARN line for the malformed value it deletes",
		"session registry deleting a malformed value under a conditional delete")
}

// Both conditional deletes lose to a write that lands under them, and the answer says only that:
// the entry stayed. It is not the mismatch's outcome, because the write that won the race can be
// this very client instance's heartbeat, and the route's reason for a mismatch would then be
// false.
func TestSessionRegistryDeleteIf_KeepsTheEntryAfterTwoLostCompareAndSwaps(t *testing.T) {
	reg := openRegistryForInstanceDelete(t)
	const (
		sessionID  = "ses_instance_race_lost"
		instanceID = "4f1c2a9e8b7d6c5a4f1c2a9e8b7d6c5a"
	)
	putForInstanceDelete(t, reg, sessionID, instanceID)
	heartbeat, err := json.Marshal(SessionEntry{
		Port:             13381,
		MachineID:        "test-machine",
		ClientInstanceID: instanceID,
		UpdatedAt:        time.Now().UnixMilli(),
	})
	if err != nil {
		t.Fatalf("encode the interleaved heartbeat: %v", err)
	}
	deletes := heartbeatBeforeDeletes(t, reg, sessionID, heartbeat, 2)

	outcome, err := reg.DeleteIf(context.Background(), sessionID, DeleteConditions{ClientInstanceID: instanceID})
	if err != nil {
		t.Fatalf("DeleteIf: %v", err)
	}
	if outcome != EntryKeptAfterLostRace {
		t.Fatalf("DeleteIf = %s, want %s after both compare-and-swaps lost", outcome, EntryKeptAfterLostRace)
	}
	if got := deletes(); got != 2 {
		t.Fatalf("made %d fenced deletes, want the one retry (2)", got)
	}
	if _, err := reg.watcher.KV().Get(sessionID); err != nil {
		t.Fatalf("read the entry that stayed: %v, want it still stored", err)
	}
}

// heartbeatBeforeDeletes rebuilds reg over a handle on its bucket that writes heartbeat under
// sessionID just before each of the first lost deletes through it (faultyDeleteKeyValue), so each
// of those fenced deletes loses its compare-and-swap whichever read reported the revision it is
// fenced on: DeleteIf's first read is a direct get and its retry's the stream leader's
// (readForStop), and a write landed between either and its delete. It returns how many deletes
// the handle has passed, one per DeleteIf attempt.
func heartbeatBeforeDeletes(t *testing.T, reg *SessionRegistry, sessionID string, heartbeat []byte, lost int) func() int {
	t.Helper()
	race := &faultyDeleteKeyValue{KeyValue: reg.watcher.KV().Legacy()}
	useKV(t, reg, race)
	race.armFault(func(n int) error {
		if n > lost {
			return nil
		}
		_, err := race.KeyValue.Put(sessionID, heartbeat)
		return err
	})
	return func() int {
		race.mu.Lock()
		defer race.mu.Unlock()
		return race.deletes
	}
}

// The condition the daemon's clean stop of a dead role's session carries: the registration under
// that session id was written after the moment the stop belongs to, so it is the relaunched
// process's and the delete must not take it (dispatch://INFRA-392).
func TestSessionRegistryDeleteIf_KeepsARegistrationWrittenAfterTheMoment(t *testing.T) {
	reg := openRegistryForInstanceDelete(t)
	const sessionID = "ses_registered_again"
	putForInstanceDelete(t, reg, sessionID, "")
	stored, err := reg.Get(sessionID)
	if err != nil {
		t.Fatalf("read the registration back: %v", err)
	}

	outcome, err := reg.DeleteIf(context.Background(), sessionID, DeleteConditions{NotRegisteredAfter: stored.UpdatedAt - 1})
	if err != nil {
		t.Fatalf("DeleteIf: %v", err)
	}
	if outcome != EntryKeptForALaterRegistration {
		t.Fatalf("DeleteIf = %s, want %s for a registration newer than the stop", outcome, EntryKeptForALaterRegistration)
	}
	kept, err := reg.watcher.KV().Get(sessionID)
	if err != nil {
		t.Fatalf("read the registration that stayed: %v, want it still stored", err)
	}
	if _, err := reg.Get(sessionID); err != nil {
		t.Fatalf("the cache dropped the registration that stayed: %v (revision %d)", err, kept.Revision())
	}
}

// The stop's own registration: nothing has been written under that session id since the moment
// the stop belongs to, so the entry goes and the session reads gone at once.
func TestSessionRegistryDeleteIf_DeletesARegistrationNoNewerThanTheMoment(t *testing.T) {
	reg := openRegistryForInstanceDelete(t)
	const sessionID = "ses_still_the_dead_launchs"
	putForInstanceDelete(t, reg, sessionID, "")
	stored, err := reg.Get(sessionID)
	if err != nil {
		t.Fatalf("read the registration back: %v", err)
	}

	outcome, err := reg.DeleteIf(context.Background(), sessionID, DeleteConditions{NotRegisteredAfter: stored.UpdatedAt})
	if err != nil {
		t.Fatalf("DeleteIf: %v", err)
	}
	if outcome != EntryDeleted {
		t.Fatalf("DeleteIf = %s, want %s for the registration the stop belongs to", outcome, EntryDeleted)
	}
	if _, err := reg.watcher.KV().Get(sessionID); !errors.Is(err, natsgo.ErrKeyNotFound) {
		t.Fatalf("read the deleted key: %v, want nats.ErrKeyNotFound", err)
	}
}

// openRegistryForInstanceDelete opens a registry whose cache has caught up with its bucket, so a
// conditional-delete test reads what it seeded and nothing else. Its TTL is stagedSessionTTL, so
// what a row seeded is still there to delete however long the row takes.
func openRegistryForInstanceDelete(t *testing.T) *SessionRegistry {
	t.Helper()
	client := setupNATS(t)
	reg, err := OpenSessionRegistry(t.Context(), client.Conn, WithSessionReplicas(1), WithSessionTTL(stagedSessionTTL))
	if err != nil {
		t.Fatalf("OpenSessionRegistry: %v", err)
	}
	if err := reg.WaitForCacheReady(testwait.Context(t)); err != nil {
		t.Fatalf("WaitForCacheReady: %v", err)
	}
	return reg
}

// putForInstanceDelete registers sessionID for instanceID, which is empty for a client that
// predates the field.
func putForInstanceDelete(t *testing.T, reg *SessionRegistry, sessionID, instanceID string) {
	t.Helper()
	entry := SessionEntry{Port: 13381, MachineID: "test-machine", Dir: "/test", ClientInstanceID: instanceID}
	if err := reg.Put(sessionID, entry); err != nil {
		t.Fatalf("Put %s: %v", sessionID, err)
	}
}

func TestSessionRegistry_Ping_Healthy(t *testing.T) {
	client := setupNATS(t)
	reg, err := OpenSessionRegistry(t.Context(), client.Conn, WithSessionReplicas(1), WithSessionTTL(10*time.Second))
	if err != nil {
		t.Fatalf("failed to open: %v", err)
	}
	if err := reg.Ping(); err != nil {
		t.Fatalf("Ping on healthy conn returned error: %v", err)
	}
}

func TestSessionRegistry_Ping_ClosedConnReturnsError(t *testing.T) {
	// A closed KV handle must remain observable through Ping so /healthz can
	// report the unavailable dependency while NATS reconnects.
	client := setupNATS(t)
	reg, err := OpenSessionRegistry(t.Context(), client.Conn, WithSessionReplicas(1), WithSessionTTL(10*time.Second))
	if err != nil {
		t.Fatalf("failed to open: %v", err)
	}
	if err := reg.Ping(); err != nil {
		t.Fatalf("sanity: Ping before close: %v", err)
	}

	bustest.Close(t, client)

	if err := reg.Ping(); err == nil {
		t.Fatal("Ping after conn close should return error")
	}
}

func TestSessionRegistry_RewatchRestartsStoppedWatcher(t *testing.T) {
	client := setupNATS(t)
	registry, err := OpenSessionRegistry(t.Context(), client.Conn, WithSessionReplicas(1), WithSessionTTL(stagedSessionTTL), standingClock())
	if err != nil {
		t.Fatalf("open registry: %v", err)
	}
	if err := registry.WaitForCacheReady(testwait.Context(t)); err != nil {
		t.Fatalf("wait for initial cache: %v", err)
	}

	url := client.Conn.ConnectedUrl()
	bustest.Close(t, client)
	testwait.Eventually(t, "the watcher to report its end once its connection closed", func() bool { return registry.WatchErr() != nil })
	if err := registry.Ping(); err == nil {
		t.Fatal("stopped watcher should make the registry unhealthy")
	}

	replacement := testnats.Connect(t, url)
	defer replacement.Close()
	if err := registry.Rewatch(context.Background(), replacement); err != nil {
		t.Fatalf("rewatch registry: %v", err)
	}
	if err := registry.Ping(); err != nil {
		t.Fatalf("ping after rewatch: %v", err)
	}

	js, err := replacement.JetStream(testnats.MaxWait(t))
	if err != nil {
		t.Fatalf("open replacement JetStream: %v", err)
	}
	kv, err := js.KeyValue(bus.SessionBucket)
	if err != nil {
		t.Fatalf("open replacement session bucket: %v", err)
	}
	entry := SessionEntry{Port: 13381, MachineID: "replacement", Dir: "/replacement"}
	value, err := json.Marshal(entry)
	if err != nil {
		t.Fatalf("marshal replacement entry: %v", err)
	}
	if _, err := kv.Put("ses_after_rewatch", value); err != nil {
		t.Fatalf("put replacement entry: %v", err)
	}
	testwait.Until(t, "the rewatched watcher to deliver ses_after_rewatch's put", func() (string, bool) {
		got, err := registry.Get("ses_after_rewatch")
		return fmt.Sprintf("Get(ses_after_rewatch) = %+v, %v", got, err), err == nil && reflect.DeepEqual(got, entry)
	})
}

// The registry's clock stands still (standingClock), so ses_malformed's 10 s TTL never answers it
// absent: only the watcher's eviction of the malformed value ends the wait for its eviction, as
// main's 5 s bound, inside that TTL, required.
func TestSessionRegistryWatcherEvictsMalformedValue(t *testing.T) {
	logs := captureSessionRegistryLogs(t)
	client := setupNATS(t)
	reg, err := OpenSessionRegistry(t.Context(), client.Conn, WithSessionReplicas(1), WithSessionTTL(10*time.Second), standingClock())
	if err != nil {
		t.Fatalf("OpenSessionRegistry: %v", err)
	}
	if err := reg.WaitForCacheReady(testwait.Context(t)); err != nil {
		t.Fatalf("WaitForCacheReady: %v", err)
	}

	const sessionID = "ses_malformed"
	if err := reg.Put(sessionID, SessionEntry{Port: 13381, MachineID: "example-host", Dir: "/example"}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	revision, err := reg.watcher.KV().Put(sessionID, []byte("{"))
	if err != nil {
		t.Fatalf("put malformed value: %v", err)
	}
	waitForSessionEviction(t, reg, sessionID)

	logtest.AwaitLogged(t, logs, "the watcher's WARN line for ses_malformed's malformed value",
		"level=WARN", "key="+sessionID, "revision="+strconv.FormatUint(revision, 10))
	t.Logf("session watcher malformed-value warning: %s", strings.TrimSpace(logs.String()))
}

func TestSessionRegistryPutDoesNotOverwriteNewerWatcherValue(t *testing.T) {
	client := setupNATS(t)
	reg, err := OpenSessionRegistry(t.Context(), client.Conn, WithSessionReplicas(1), WithSessionTTL(10*time.Second), standingClock())
	if err != nil {
		t.Fatalf("OpenSessionRegistry: %v", err)
	}
	if err := reg.WaitForCacheReady(testwait.Context(t)); err != nil {
		t.Fatalf("WaitForCacheReady: %v", err)
	}

	const sessionID = "ses_revision"
	raw := reg.watcher.KV()
	useKV(t, reg, &interleavingSessionPutKeyValue{
		KeyValue: raw.Legacy(),
		afterFirstPut: func() {
			remote := SessionEntry{Port: 13382, MachineID: "remote-host", Dir: "/remote"}
			buf, err := json.Marshal(remote)
			if err != nil {
				t.Fatalf("marshal remote entry: %v", err)
			}
			if _, err := raw.Put(sessionID, buf); err != nil {
				t.Fatalf("put remote entry: %v", err)
			}
			testwait.Until(t, "the watcher to deliver the remote write of "+sessionID, func() (string, bool) {
				got, err := reg.Get(sessionID)
				return fmt.Sprintf("Get(%s) = %+v, %v", sessionID, got, err), err == nil && got.Port == remote.Port
			})
		},
	})
	if err := reg.Put(sessionID, SessionEntry{Port: 13381, MachineID: "local-host", Dir: "/local"}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	got, err := reg.Get(sessionID)
	if err != nil {
		t.Fatalf("Get after interleaving: %v", err)
	}
	if got.Port != 13382 || got.MachineID != "remote-host" {
		t.Fatalf("older write-through restored local entry: %+v", got)
	}
}

func TestSessionRegistryDeleteHistoryFailureSuppressesStaleWatcherUpdate(t *testing.T) {
	client := setupNATS(t)
	reg, err := OpenSessionRegistry(t.Context(), client.Conn, WithSessionReplicas(1), WithSessionTTL(stagedSessionTTL))
	if err != nil {
		t.Fatalf("OpenSessionRegistry: %v", err)
	}
	if err := reg.WaitForCacheReady(testwait.Context(t)); err != nil {
		t.Fatalf("WaitForCacheReady: %v", err)
	}

	const sessionID = "ses_delete"
	item := SessionEntry{Port: 13381, MachineID: "example-host", Dir: "/example"}
	if err := reg.Put(sessionID, item); err != nil {
		t.Fatalf("Put: %v", err)
	}
	entry, err := reg.watcher.KV().Get(sessionID)
	if err != nil {
		t.Fatalf("Get seeded entry: %v", err)
	}
	useKV(t, reg, &historyFailSessionKeyValue{
		KeyValue: reg.watcher.KV().Legacy(),
		err:      errors.New("injected history failure"),
	})
	if err := reg.Delete(context.Background(), sessionID); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	sessionCache{reg}.Apply(kvwatch.LegacyEntry(entry), kvwatch.Read{})
	if _, err := reg.Get(sessionID); err == nil {
		t.Fatal("Get returned a session restored by a stale watcher update")
	}
}

// useKV rebuilds the registry's watcher over kv, a wrapper of its bucket handle that watches
// through the handle's own jetstream one, so the registry writes through kv from here on.
//
// The watcher it replaces is ended rather than stopped before this one takes its place, and the
// field is written under r.mu: the readers of it on other goroutines - a consume's delivery
// through following(), which every scheduled deadline reaches (armExpiryLocked), the expiry
// timers and the reaper - hold that lock. Production assigns the field once at open; a test is
// the only writer there is.
func useKV(t *testing.T, r *SessionRegistry, kv natsgo.KeyValue) {
	t.Helper()
	ctx := testwait.Context(t)
	r.mu.RLock()
	previous := r.watcher
	r.mu.RUnlock()
	base := previous.KV()
	previous.End(ctx)
	rebuilt := kvwatch.New("session registry", bus.NewKeyValue(kv, base.WatchHandle(), base.Streams()), sessionCache{r})
	r.mu.Lock()
	r.watcher = rebuilt
	r.mu.Unlock()
	rebuilt.Start()
	if err := r.WaitForCacheReady(ctx); err != nil {
		t.Fatalf("wait for the rebuilt watcher: %v", err)
	}
}

// interleavingSessionPutKeyValue writes through afterFirstPut once the first entry write has
// landed, whichever call made it: PutWithin writes conditionally (Create over no entry, Update
// over a revision), and an unconditional Put is counted the same way.
type interleavingSessionPutKeyValue struct {
	natsgo.KeyValue

	puts          int
	afterFirstPut func()
}

func (kv *interleavingSessionPutKeyValue) Put(key string, value []byte) (uint64, error) {
	return kv.wrote(kv.KeyValue.Put(key, value))
}

func (kv *interleavingSessionPutKeyValue) Create(key string, value []byte) (uint64, error) {
	return kv.wrote(kv.KeyValue.Create(key, value))
}

func (kv *interleavingSessionPutKeyValue) Update(key string, value []byte, last uint64) (uint64, error) {
	return kv.wrote(kv.KeyValue.Update(key, value, last))
}

func (kv *interleavingSessionPutKeyValue) wrote(revision uint64, err error) (uint64, error) {
	if err != nil {
		return 0, err
	}
	kv.puts++
	if kv.puts == 1 {
		kv.afterFirstPut()
	}
	return revision, nil
}

// interleavingSessionGetKeyValue writes value under key after each of the first interleaves reads
// of key, so whatever that reader does next loses its compare-and-swap: the revision it read is
// no longer the last one by the time it writes. Reads of any other key, and reads that did not
// answer, pass straight through and are not counted.
type interleavingSessionGetKeyValue struct {
	natsgo.KeyValue

	key         string
	value       []byte
	interleaves int

	mu       sync.Mutex
	gets     int
	firstGot []byte
}

func (kv *interleavingSessionGetKeyValue) Get(key string) (natsgo.KeyValueEntry, error) {
	entry, err := kv.KeyValue.Get(key)
	if key != kv.key || err != nil {
		return entry, err
	}
	kv.mu.Lock()
	defer kv.mu.Unlock()
	kv.gets++
	if kv.firstGot == nil {
		kv.firstGot = append([]byte(nil), entry.Value()...)
	}
	if kv.gets > kv.interleaves {
		return entry, nil
	}
	if _, err := kv.KeyValue.Put(key, kv.value); err != nil {
		return nil, err
	}
	return entry, nil
}

// reads is how often the key was read through this bucket.
func (kv *interleavingSessionGetKeyValue) reads() int {
	kv.mu.Lock()
	defer kv.mu.Unlock()
	return kv.gets
}

// firstGetValue is the raw record the first read of key returned to its caller, captured before
// this seam wrote value over the key. It is what a reader's direct get actually saw, where Refresh
// re-reads the cache after applying the read and so can answer the newer value this seam wrote.
func (kv *interleavingSessionGetKeyValue) firstGetValue() []byte {
	kv.mu.Lock()
	defer kv.mu.Unlock()
	return kv.firstGot
}

type historyFailSessionKeyValue struct {
	natsgo.KeyValue

	err error
}

func (kv *historyFailSessionKeyValue) History(string, ...natsgo.WatchOpt) ([]natsgo.KeyValueEntry, error) {
	return nil, kv.err
}

// waitFor is main's bounded wait. A2's callers still use this; A2 deletes it with its last caller.
func waitFor(t *testing.T, timeout time.Duration, condition func() bool) {
	t.Helper()
	testwait.WithinPolling(t, timeout, testwait.Poll, "observable state", condition)
}

// waitForSessionEviction waits until the cache answers sessionID with an error and lists no
// session at all. A give-up prints what Get and List last answered.
func waitForSessionEviction(t *testing.T, reg *SessionRegistry, sessionID string) {
	t.Helper()
	testwait.Until(t, "the watcher to evict "+sessionID+", so Get fails and List answers no session", func() (string, bool) {
		got, getErr := reg.Get(sessionID)
		entries, listErr := reg.List()
		return fmt.Sprintf("Get(%s) = %+v, %v; List() = %+v, %v", sessionID, got, getErr, entries, listErr),
			getErr != nil && listErr == nil && len(entries) == 0
	})
}

// standingClock is a registry clock that stands at the moment it was made, so no session's TTL
// passes while a row waits on the watcher (Get, List and the expiry timer read this clock): what the
// row waits for is the watcher's doing, and the wall clock decides none of it.
func standingClock() SessionRegistryOption {
	at := time.Now()
	return WithSessionClock(func() time.Time { return at })
}

func captureSessionRegistryLogs(t *testing.T) *lockedBuffer {
	t.Helper()
	var logs lockedBuffer
	logtest.SetDefault(t, slog.NewTextHandler(&logs, nil))
	return &logs
}

type lockedBuffer = logtest.Buffer

// TestMain removes the NATS server the package's tests share (testnats.Main).
func TestMain(m *testing.M) { os.Exit(testnats.Main(m.Run)) }
