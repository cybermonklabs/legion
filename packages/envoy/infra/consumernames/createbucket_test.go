package consumernames

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/jsm.go"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"

	selection "github.com/sjawhar/envoy/consumernames"
	"github.com/sjawhar/envoy/infra/writerhold/holdtest"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/bus/kvrecords"
	"github.com/sjawhar/envoy/internal/holdrecord"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/natsgrant"
)

// createBucketCohort writes hold id's cohort record with boundary B, as consumer-names prepare
// writes it (prepareCohort).
func createBucketCohort(t *testing.T, records *holdrecord.Client, id string, boundary uint64) {
	t.Helper()
	value, err := json.Marshal(prepareCohort{Boundary: boundary})
	require.NoError(t, err)
	_, err = records.PutRecord(context.Background(), id, prepareCohortKind, prepareCohortKey, 0, value)
	require.NoError(t, err)
}

// createBucketEntries are command's entries in d, by state, in their order.
func createBucketEntries(d holdrecord.Done, command string) []string {
	var states []string
	for _, op := range d.Ops {
		if op.Command == command {
			states = append(states, op.State)
		}
	}
	return states
}

// createBucketState reads the selection bucket as the operator, on a connection of its own (the
// stack closes every operator connection a test opened before it runs a held command): its stream
// with its subjects (false when absent) and its sentinel (false when none).
func createBucketState(h *stack) (*jetstream.StreamInfo, bool, createBucketSentinelRead, bool) {
	h.t.Helper()
	ctx := context.Background()
	nc, _ := h.operator()
	info, present, err := createBucketStream(ctx, nc, true)
	require.NoError(h.t, err)
	if !present {
		return nil, false, createBucketSentinelRead{}, false
	}
	sentinel, found, err := createBucketSentinel(ctx, nc)
	require.NoError(h.t, err)
	return info, true, sentinel, found
}

// createBucketRun runs `envoy-infra consumer-names <args>` as stack.run does, with ctx and e in
// place of the stack's: a run a test ends part way, or one on a hub of the test's own.
func createBucketRun(t *testing.T, ctx context.Context, e env, args ...string) (int, string, string) {
	t.Helper()
	full := append(append([]string{}, args...), commonArgs...)
	if args[0] != actionStatus {
		full = append(full, writingArgs...)
	}
	var stdout, stderr bytes.Buffer
	code := runCommand(ctx, full, &stdout, &stderr, e)
	t.Logf("envoy-infra consumer-names %s: exit %d\n%s%s", strings.Join(args, " "), code, stdout.String(), stderr.String())
	return code, stdout.String(), stderr.String()
}

// createBucketNotifications makes ENVOY_NOTIFICATIONS, which no client but the held operator
// creates: inside a buckets-create hold, set and released with its journal complete
// (operatorInHold), so the row is released again when it returns; then, as a listener of the
// stack, whose grant publishes to it, it publishes messages messages on one agent subject.
func createBucketNotifications(t *testing.T, h *stack, messages int) *jetstream.StreamInfo {
	t.Helper()
	ctx := context.Background()
	operatorInHold(t, h, holdrecord.WorkflowBucketsCreate, func(_ *nats.Conn, js jetstream.JetStream) {
		h.notificationStream(js)
	})

	nc, err := h.s.Connect(h.s.Cluster.URL(), h.s.ListenerCredential(holdtest.ListenerMachines[0]))
	require.NoError(t, err)
	defer nc.Close()
	listener, err := jetstream.New(nc)
	require.NoError(t, err)
	for i := range messages {
		_, err := listener.Publish(ctx, "notifications.agent.createbucket-test", []byte(fmt.Sprintf("message %d", i)))
		require.NoError(t, err)
	}
	info, err := streamInfo(ctx, nc, notifications)
	require.NoError(t, err)
	return info
}

// createBucketKeepLast keeps only the last keep messages of ENVOY_NOTIFICATIONS (its max_msgs,
// with discard old, since under the build's discard new a lowered max_msgs removes nothing and
// refuses the next publish), through js, the held operator's, so its first sequence moves on as
// a retention limit moves it, and answers the stream's first sequence once the stream holds keep
// messages: the update's own answer may carry the state from before the limit was applied.
func createBucketKeepLast(t *testing.T, js jetstream.JetStream, keep int64) uint64 {
	t.Helper()
	ctx := context.Background()
	stream, err := js.Stream(ctx, notifications)
	require.NoError(t, err)
	config := stream.CachedInfo().Config
	config.MaxMsgs, config.Discard = keep, jetstream.DiscardOld
	_, err = js.UpdateStream(ctx, config)
	require.NoError(t, err)
	var info *jetstream.StreamInfo
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if info, err = stream.Info(ctx); err == nil && info.State.Msgs == uint64(keep) {
			return info.State.FirstSeq
		}
	}
	if info == nil {
		t.Fatalf("ENVOY_NOTIFICATIONS after its max_msgs became %d does not answer its info: %v", keep, err)
	}
	t.Fatalf("ENVOY_NOTIFICATIONS after its max_msgs became %d: state %+v, max_msgs %d, discard %v", keep, info.State, info.Config.MaxMsgs, info.Config.Discard)
	return 0
}

// createBucketConsumer creates name on ENVOY_NOTIFICATIONS through nc, as a creator in the fresh
// mode does: a pull consumer of one agent subject with metadata (nil: none).
func createBucketConsumer(t *testing.T, nc *nats.Conn, name string, metadata map[string]string) {
	t.Helper()
	_, err := writeConsumer(context.Background(), nc, notifications, jetstream.ConsumerConfig{
		Durable: name, AckPolicy: jetstream.AckExplicitPolicy, FilterSubject: "notifications.agent." + strings.ReplaceAll(name, "~", "-"),
		Metadata: metadata,
	}, actionCreateConsumer)
	require.NoError(t, err)
}

// createBucketSessionMetadata is a fresh session consumer's metadata (member.Metadata).
func createBucketSessionMetadata(session string) map[string]string {
	return member{Family: familySession, Logical: session, SessionID: session}.Metadata()
}

// createBucketBackupOf makes a jsm.go backup of a selection bucket on a hub of the test's own, of n
// servers with an admin as its no-auth user, at n copies: its sentinel, each of records, and filler
// keys, each an exact selection record (as create-bucket --after-loss takes only those) holding 4
// KiB of pseudo-random bytes in base64, so the archive does not compress to one 64 KiB chunk. It
// answers the backup's directory and the bucket's message count.
func createBucketBackupOf(t *testing.T, n int, sentinel selection.StoreState, records map[string]selection.Record, filler int) (string, uint64) {
	t.Helper()
	ctx := context.Background()
	config := "authorization {\n  users = [ { user: \"admin\", password: \"admin\" } ]\n}\nno_auth_user: \"admin\"\n"
	var url string
	if n > 1 {
		url = testnats.StartClusterConfig(t, n, config).URL()
	} else {
		_, url = testnats.StartConfig(t, "jetstream {}\n"+config, []string{"-js"}, true)
	}
	nc := testnats.Connect(t, url)
	defer nc.Close()
	js, err := jetstream.New(nc)
	require.NoError(t, err)
	_, err = js.CreateKeyValue(ctx, selection.SessionBucketConfig(n))
	require.NoError(t, err)
	store := selection.NewKVStore(selection.ConnRequester{Conn: nc})
	_, err = store.WriteSentinel(ctx, sentinel, 0)
	require.NoError(t, err)
	for key, record := range records {
		_, err := store.Write(ctx, key, record, 0)
		require.NoError(t, err)
	}
	// Pseudo-random values, a stream of its own for each key: a pattern the archive's s2
	// compression folds would leave one chunk, and a cut after the second chunk would fall on the
	// restore's last request instead. The bytes go in base64url as retired names of at most 200
	// characters each, which the library's decode takes as consumer names (no '/' or '+', at most
	// 255 bytes).
	value := make([]byte, 4096)
	for i := range filler {
		random := rand.New(rand.NewPCG(uint64(i), 0x5eed))
		for j := range value {
			value[j] = byte(random.Uint32())
		}
		encoded := base64.RawURLEncoding.EncodeToString(value)
		var retired []string
		for len(encoded) > 0 {
			size := min(200, len(encoded))
			retired, encoded = append(retired, encoded[:size]), encoded[size:]
		}
		_, err := store.Write(ctx, fmt.Sprintf("filler-%d", i), selection.Record{Retired: retired}, 0)
		require.NoError(t, err)
	}
	mgr, err := jsm.New(nc, jsm.WithTimeout(30*time.Second))
	require.NoError(t, err)
	stream, err := mgr.LoadStream(selectionStreamName)
	require.NoError(t, err)
	dir := filepath.Join(t.TempDir(), "backup")
	_, err = stream.SnapshotToDirectory(ctx, dir)
	require.NoError(t, err)
	info, err := js.Stream(ctx, selectionStreamName)
	require.NoError(t, err)
	return dir, info.CachedInfo().State.Msgs
}

// createBucketCutDialer dials as nats.go does, and once, when a connection writes the restore's
// second data chunk (a PUB to $JS.SNAPSHOT.RESTORE.<stream>.<n> with a payload, not the empty
// request that ends the restore), ends the run (cut, the run's ctx's cancel) and closes that
// connection: a restore killed after a cut chunk.
type createBucketCutDialer struct {
	cut  func()
	done atomic.Bool
}

func (d *createBucketCutDialer) Dial(network, address string) (net.Conn, error) {
	conn, err := (&net.Dialer{Timeout: 10 * time.Second}).Dial(network, address)
	if err != nil {
		return nil, err
	}
	return &createBucketCutConn{Conn: conn, d: d}, nil
}

type createBucketCutConn struct {
	net.Conn
	d      *createBucketCutDialer
	chunks int
}

// createBucketChunk is a restore chunk's PUB line: its subject, its reply and its payload's size.
var createBucketChunk = regexp.MustCompile(`PUB \$JS\.SNAPSHOT\.RESTORE\.\S+ \S+ (\d+)\r\n`)

func (c *createBucketCutConn) Write(p []byte) (int, error) {
	for _, match := range createBucketChunk.FindAllSubmatch(p, -1) {
		if string(match[1]) != "0" {
			c.chunks++
		}
	}
	if c.chunks >= 2 && c.d.done.CompareAndSwap(false, true) {
		c.d.cut()
		_ = c.Conn.Close()
		return 0, net.ErrClosed
	}
	return c.Conn.Write(p)
}

// create-bucket in a held cutover hold writes its intent, creates the selection bucket with the
// configuration create-bucket sends, writes its sentinel with the cohort's boundary B and the hold
// id, and writes its done entry, which does not complete the hold (plan §3 step 2). Control: in the
// same hold before its cohort record is written, create-bucket is refused naming the cohort record,
// before its intent: the journal holds no create-bucket entry and no bucket is made.
func TestCreateBucketWritesItsIntentTheBucketAndItsSentinelWithB(t *testing.T) {
	h := newStack(t)
	createBucketNotifications(t, h, 0)
	id := h.heldHold(holdrecord.WorkflowCutover)

	code, _, stderr := h.run("create-bucket", "--hold", id)
	require.Equal(t, 1, code, stderr)
	require.Contains(t, stderr, "has no cohort record")
	require.Empty(t, createBucketEntries(h.journal(id), createBucketCommand))
	_, present, _, _ := createBucketState(h)
	require.False(t, present, "control: a refused create-bucket made the bucket")

	createBucketCohort(t, h.records(), id, 7)
	code, _, stderr = h.run("create-bucket", "--hold", id)
	require.Equal(t, 0, code, stderr)
	d := h.journal(id)
	require.Equal(t, []string{"intent", "done"}, createBucketEntries(d, createBucketCommand))
	require.False(t, d.Complete, "create-bucket completed the cutover hold; only streams census does")
	info, present, sentinel, found := createBucketState(h)
	require.True(t, present)
	require.Empty(t, createBucketConfigDiff(info.Config, 1))
	require.True(t, found)
	require.Equal(t, createBucketJSON(ownSentinel(familySession, selection.StoreState{Boundary: 7, Hold: id})), createBucketJSON(sentinel.state))
	require.Equal(t, natsgrant.OperatorPhaseHeld, h.row().Phase)
}

// create-bucket outside a held row is refused naming the phase it found, under a released row and
// under its own hold's prepare row, and writes nothing. Control: the same hold, once held, runs it.
func TestCreateBucketOutsideHeldIsRefusedNamingThePhase(t *testing.T) {
	h := newStack(t)
	createBucketNotifications(t, h, 0)
	code, _, stderr := h.run("create-bucket", "--hold", "no-such-hold")
	require.Equal(t, 1, code, stderr)
	require.Contains(t, stderr, "create-bucket runs while the hold row is held, and it is released")

	id := h.prepared(holdrecord.WorkflowCutover)
	createBucketCohort(t, h.records(), id, 3)
	code, _, stderr = h.run("create-bucket", "--hold", id)
	require.Equal(t, 1, code, stderr)
	require.Contains(t, stderr, "create-bucket runs while the hold row is held, and it is prepare")
	_, present, _, _ := createBucketState(h)
	require.False(t, present)
	require.Empty(t, createBucketEntries(h.journal(id), createBucketCommand))

	h.held(id)
	code, _, stderr = h.run("create-bucket", "--hold", id)
	require.Equal(t, 0, code, "control: the held hold's create-bucket\n%s", stderr)
}

// create-bucket --replicas 3 on the stack's three servers creates the bucket at three copies and
// writes its sentinel: it waits for the new stream's leader before the write, where a put right
// after the create was answered "no responders" (as selection.CreateSessionBucket's write right
// after its create is answered, on three copies). Control: the stream names a leader, three
// copies, and the sentinel holds the cohort's boundary.
func TestCreateBucketOnThreeCopiesWritesItsSentinel(t *testing.T) {
	h := newStack(t)
	createBucketNotifications(t, h, 0)
	id := h.prepared(holdrecord.WorkflowCutover)
	createBucketCohort(t, h.records(), id, 2)
	h.held(id)
	code, _, stderr := h.run("create-bucket", "--hold", id, "--replicas", "3")
	require.Equal(t, 0, code, stderr)
	info, present, sentinel, found := createBucketState(h)
	require.True(t, present)
	require.Equal(t, 3, info.Config.Replicas)
	require.NotNil(t, info.Cluster)
	require.NotEmpty(t, info.Cluster.Leader)
	require.True(t, found)
	require.Equal(t, createBucketJSON(ownSentinel(familySession, selection.StoreState{Boundary: 2, Hold: id})), createBucketJSON(sentinel.state))
	require.Equal(t, []string{"intent", "done"}, createBucketEntries(h.journal(id), createBucketCommand))
}

// A rerun of create-bucket, admitted because its latest entry is its intent, adopts the bucket it
// finds only when its stream has the configuration create-bucket sends and it holds no key (the
// crash fell between the create and the sentinel: the sentinel is written with B), or only its
// sentinel with this hold's B (nothing changes). Controls, each in the same hold with the bucket
// made again: another configuration (history 2), another key beside the sentinel, and a sentinel
// with another B each stop it as UNKNOWN naming the bucket and leave the bucket as it was. Where
// the cutover's table makes create-bucket Stoppable, each writes its stop and convert is admitted;
// where it does not, its intent stays and convert is refused as busy.
func TestCreateBucketsRerunAdoptsOnlyItsOwnBucket(t *testing.T) {
	ctx := context.Background()
	const boundary = 5
	// crashed is a held cutover hold whose create-bucket wrote its intent and nothing after it.
	crashed := func(h *stack) string {
		createBucketNotifications(h.t, h, 0)
		id := h.prepared(holdrecord.WorkflowCutover)
		createBucketCohort(h.t, h.records(), id, boundary)
		h.held(id)
		_, err := h.records().Admit(ctx, id, holdrecord.Request{Command: createBucketCommand}, map[string]string{"boundary": formatSeq(boundary)})
		require.NoError(h.t, err)
		return id
	}

	t.Run("refused", func(t *testing.T) {
		h := newStack(t)
		id := crashed(h)
		mine := selection.StoreState{Boundary: boundary, Hold: id}
		for _, control := range []struct {
			name, want string
			make       func(nc *nats.Conn, js jetstream.JetStream)
		}{
			{"another configuration", "max_msgs_per_subject 2, not 1", func(_ *nats.Conn, js jetstream.JetStream) {
				config := selection.SessionBucketConfig(1)
				config.History = 2
				_, err := js.CreateKeyValue(ctx, config)
				require.NoError(t, err)
			}},
			{"another key", "2 keys (envoy.bucket, ses_other)", func(nc *nats.Conn, _ jetstream.JetStream) {
				store := createBucketMakeBucket(t, nc, mine)
				_, err := store.Write(ctx, "ses_other", selection.Record{Start: 1}, 0)
				require.NoError(t, err)
			}},
			{"another boundary", `its sentinel {"key":"envoy.bucket","family":"session","boundary":6`, func(nc *nats.Conn, _ jetstream.JetStream) {
				createBucketMakeBucket(t, nc, selection.StoreState{Boundary: boundary + 1, Hold: id})
			}},
		} {
			control.make(h.operator())
			before, _, _, _ := createBucketState(h)
			code, _, stderr := h.run("create-bucket", "--hold", id)
			require.Equal(t, 2, code, "control %s\n%s", control.name, stderr)
			require.Contains(t, stderr, "UNKNOWN selection bucket "+selection.SessionBucket+" exists", control.name)
			require.Contains(t, stderr, control.want, control.name)
			after, present, _, _ := createBucketState(h)
			require.True(t, present, "control %s: the bucket was deleted", control.name)
			require.Equal(t, createBucketKeys(before, id), createBucketKeys(after, id), "control %s: the bucket's keys changed", control.name)
			entries := createBucketEntries(h.journal(id), createBucketCommand)
			refused := createBucketRefused(holdrecord.WorkflowCutover, createBucketCommand)
			require.Equal(t, refused, entries[len(entries)-len(refused):], "control %s: its refusal's entries", control.name)
			err := holdrecord.Decide(h.journal(id), holdrecord.Request{Command: "convert"})
			if holdrecord.Stoppable(holdrecord.WorkflowCutover, createBucketCommand) {
				require.NoError(t, err, "control %s: its stop ends its intent, and convert is admitted", control.name)
			} else {
				require.ErrorIs(t, err, holdrecord.ErrBusy, "control %s: convert was admitted past create-bucket's pending intent", control.name)
			}
			_, js := h.operator()
			require.NoError(t, js.DeleteKeyValue(ctx, selection.SessionBucket))
		}

		// This hold's own sentinel: the rerun changes nothing and completes.
		nc, _ := h.operator()
		createBucketMakeBucket(t, nc, mine)
		_, _, before, _ := createBucketState(h)
		code, stdout, stderr := h.run("create-bucket", "--hold", id)
		require.Equal(t, 0, code, stderr)
		require.Contains(t, stdout, "whose sentinel already holds this hold's boundary 5")
		_, _, after, _ := createBucketState(h)
		require.Equal(t, before.revision, after.revision, "the rerun wrote the sentinel again")
		entries := createBucketEntries(h.journal(id), createBucketCommand)
		require.Equal(t, "done", entries[len(entries)-1])
	})

	t.Run("a crash between the create and the sentinel", func(t *testing.T) {
		h := newStack(t)
		id := crashed(h)
		_, js := h.operator()
		_, err := js.CreateKeyValue(ctx, selection.SessionBucketConfig(1))
		require.NoError(t, err)
		code, stdout, stderr := h.run("create-bucket", "--hold", id)
		require.Equal(t, 0, code, stderr)
		require.Contains(t, stdout, "which held no key: its sentinel written with boundary 5")
		_, _, sentinel, found := createBucketState(h)
		require.True(t, found)
		require.Equal(t, createBucketJSON(ownSentinel(familySession, selection.StoreState{Boundary: boundary, Hold: id})), createBucketJSON(sentinel.state))
		require.Equal(t, []string{"intent", "intent", "done"}, createBucketEntries(h.journal(id), createBucketCommand))
	})
}

// createBucketSelection reads hold id's selection record.
func createBucketSelection(t *testing.T, h *stack, id string) (createBucketInputs, bool) {
	t.Helper()
	r := &run{o: options{holdID: id}, records: h.records()}
	in, _, found, err := r.createBucketReadInputs(context.Background())
	require.NoError(t, err)
	return in, found
}

// create-bucket --after-loss with no backup, in a selection-recovery hold, writes the hold's
// selection record with its inputs (replay_from by default the notification stream's first
// sequence, here 7 once the stream keeps its last 4 of 10 messages; backup "none"; the hold id),
// creates the bucket, lists the session consumers no record selects (a session~ consumer naming
// its session; not a listener's fresh durable, nor a session~ name with no session metadata, which
// the listener's session listing does not select), writes a sentinel in replay mode with the
// broker's cutover state of the session family, and passes its postcheck. It is the workflow's
// final command: its done entry completes the hold. Control: with a bucket present that is not
// this recovery's (a cutover's sentinel of another hold), the same command stops with "selection
// bucket exists and is not this recovery's", naming the bucket, after its selection record, with
// its intent left and the bucket untouched; once that bucket is gone, the rerun takes its inputs
// from the record and completes.
func TestCreateBucketAfterLossRecordsItsInputsAndListsTheOrphans(t *testing.T) {
	ctx := context.Background()
	h := newStack(t)
	createBucketNotifications(t, h, 10)
	id := h.heldHold(holdrecord.WorkflowSelectionRecovery)
	_, err := h.records().PutCutover(ctx, familySession, selection.CutoverDone, id)
	require.NoError(t, err)
	nc, js := h.operator()
	require.Equal(t, uint64(7), createBucketKeepLast(t, js, 4))
	orphan := selection.FreshName(sessionFreshPrefix)
	createBucketConsumer(t, nc, orphan, createBucketSessionMetadata("ses_lost"))
	durable := bus.ListenerDurable(holdtest.ListenerMachines[0])
	createBucketConsumer(t, nc, selection.FreshName(durable), member{Family: familySession, Logical: selection.DurableKey(durable), Fixed: durable}.Metadata())
	createBucketConsumer(t, nc, selection.FreshName(sessionFreshPrefix), nil)

	// The control: a bucket that is not this recovery's.
	createBucketMakeBucket(t, nc, selection.StoreState{Cutover: selection.CutoverDone, Boundary: 3, Hold: "another-hold"})
	code, _, stderr := h.run("create-bucket", "--hold", id, "--after-loss")
	require.Equal(t, 1, code, stderr)
	require.Contains(t, stderr, "selection bucket exists and is not this recovery's: "+selection.SessionBucket+" holds")
	in, found := createBucketSelection(t, h, id)
	require.True(t, found, "control: the stopped run wrote no selection record")
	require.Equal(t, createBucketInputs{ReplayFrom: 7, BackupSHA256: createBucketNoBackup, Hold: id}, in)
	require.Equal(t, createBucketRefused(holdrecord.WorkflowSelectionRecovery, createBucketAfterLossCommand), createBucketEntries(h.journal(id), createBucketAfterLossCommand))
	_, _, other, _ := createBucketState(h)
	require.Equal(t, "another-hold", other.state.Hold, "control: the stopped run changed the other bucket's sentinel")
	_, js = h.operator()
	require.NoError(t, js.DeleteKeyValue(ctx, selection.SessionBucket))

	code, stdout, stderr := h.run("create-bucket", "--hold", id, "--after-loss")
	require.Equal(t, 0, code, stderr)
	require.Contains(t, stdout, "adopted: it held no key; its sentinel written")
	require.Contains(t, stdout, "postcheck passed")
	_, present, sentinel, found := createBucketState(h)
	require.True(t, present)
	require.True(t, found)
	require.Equal(t, createBucketJSON(ownSentinel(familySession, selection.StoreState{Cutover: selection.CutoverDone, ReplayFrom: 7, Orphans: []string{orphan},
		BackupSHA256: createBucketNoBackup, Hold: id})), createBucketJSON(sentinel.state))
	d := h.journal(id)
	require.Equal(t, append(createBucketRefused(holdrecord.WorkflowSelectionRecovery, createBucketAfterLossCommand), "intent", "done"), createBucketEntries(d, createBucketAfterLossCommand))
	require.True(t, d.Complete, "create-bucket --after-loss is selection-recovery's final command")
}

// A postcheck finding stops the recovery's completion (plan §2, "runs its postcheck and completes
// the hold"; §3a, the postcheck): a write to another protected stream, a copied bucket's, that
// lands after create-bucket --after-loss read its step 1 (put while the command writes the hold's
// selection record, before any mutation of its own) makes the postcheck refuse, naming that
// stream and its last_seq, before the done entry: exit 1, the intent left, the hold incomplete.
// Control: the rerun, whose step 1 reads that stream as it now is and which writes nothing to it,
// passes its postcheck and completes the hold.
func TestARecoverysPostcheckFindingStopsItsCompletion(t *testing.T) {
	ctx := context.Background()
	h := newStack(t)
	createBucketNotifications(t, h, 3)
	id := h.heldHold(holdrecord.WorkflowSelectionRecovery)
	copied := "KV_" + bus.CopiedBuckets()[0]
	var injected atomic.Bool
	h.s.Broker.Intercept(func(_ http.ResponseWriter, r *http.Request) bool {
		if r.Method != http.MethodPut || r.URL.Path != "/v1/nats-hold/records" {
			return false
		}
		body, err := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(body))
		var put struct {
			Kind string `json:"kind"`
		}
		if err != nil || json.Unmarshal(body, &put) != nil || put.Kind != createBucketSelectionKind || !injected.CompareAndSwap(false, true) {
			return false
		}
		nc, err := h.s.Connect(h.s.Cluster.URL(), h.s.OperatorCredential()...)
		if err == nil {
			js, _ := jetstream.New(nc)
			_, err = js.Publish(ctx, "$KV."+bus.CopiedBuckets()[0]+".hold.barrier.injected", []byte("a write the command did not make"))
			nc.Close()
		}
		if err != nil {
			h.t.Errorf("the injected write: %v", err)
		}
		return false
	})
	code, stdout, stderr := h.run("create-bucket", "--hold", id, "--after-loss")
	h.s.Broker.Intercept(nil)
	require.True(t, injected.Load(), "the selection record was never written")
	require.Equal(t, 1, code, "the run with a write past its step 1\n%s%s", stdout, stderr)
	require.Contains(t, stderr, "the postcheck")
	require.Contains(t, stderr, copied+"'s last_seq is")
	d := h.journal(id)
	require.Equal(t, createBucketRefused(holdrecord.WorkflowSelectionRecovery, createBucketAfterLossCommand), createBucketEntries(d, createBucketAfterLossCommand))
	require.False(t, d.Complete, "a postcheck finding completed the hold")

	code, stdout, stderr = h.run("create-bucket", "--hold", id, "--after-loss")
	require.Equal(t, 0, code, "control: the rerun\n%s", stderr)
	require.Contains(t, stdout, "postcheck passed")
	require.True(t, h.journal(id).Complete)
}

// A completed recovery adopted: create-bucket --after-loss from a backup, killed after its restore
// and before it completes (its done entry lost), then run again under the same hold.
// The rerun reads the hold's selection record, adopts the bucket by its sentinel, keeps every
// record the backup holds (a replayed key's among them) and the lower replay_from, restores
// nothing, passes its postcheck and completes. Controls: a restore over the present bucket is
// refused by the server (nats-server jetstream_api.go:4415-4421); a sentinel rewritten from the
// rerun's own reading would raise replay_from, the notification stream's first sequence having
// moved past it; and a rerun given other inputs than the selection record's stops, changing
// nothing.
//
// From a walk backup (plan v66), a create-bucket --after-loss killed after its load, at its
// sentinel's write, leaves the walk's records and no sentinel; the rerun adopts the bucket by each
// key's last value and the count of its keys, writes the sentinel with the walk's boundary, loads
// nothing again, passes its postcheck and completes. Control: an adoption by last_seq, the
// snapshot's rule over the walk's recorded state, reads that load, whose sequences started over, as
// not this recovery's and stops. (A partly loaded bucket is TestACutWalkLoadIsDeletedAndLoadedAgain.)
//
// On three servers, a create-bucket --after-loss killed after a cut chunk of its restore leaves
// the empty leftover stream, which the rerun deletes before it restores again, and the recovery
// completes. Control: a rerun without the leftover rule reads the leftover as another recovery's
// bucket and stops, naming it.
func TestACompletedRecoveryAdopted(t *testing.T) {
	ctx := context.Background()
	t.Run("adopted by its sentinel", func(t *testing.T) {
		h := newStack(t)
		createBucketNotifications(t, h, 10)
		kept := map[string]selection.Record{
			"ses_live":     {Current: selection.FreshName(sessionFreshPrefix), Start: 2},
			"ses_replayed": {Start: 4},
		}
		dir, msgs := createBucketBackupOf(t, 1, selection.StoreState{Cutover: selection.CutoverDone, Boundary: 3, Hold: "the-cutover"}, kept, 0)
		id := h.heldHold(holdrecord.WorkflowSelectionRecovery)
		nc, _ := h.operator()
		orphan := selection.FreshName(sessionFreshPrefix)
		createBucketConsumer(t, nc, orphan, createBucketSessionMetadata("ses_gone"))
		createBucketConsumer(t, nc, kept["ses_live"].Current, createBucketSessionMetadata("ses_live"))

		// A broker the command can no longer reach once its last step is done loses its done entry,
		// and the stop entry that would follow it (refuseEntries): the run's intent stays pending.
		h.s.Broker.Intercept(refuseEntries(createBucketAfterLossCommand, "done", "stop"))
		code, _, stderr := h.run("create-bucket", "--hold", id, "--after-loss", "--backup", dir)
		h.s.Broker.Intercept(nil)
		require.NotEqual(t, 0, code, "the run whose done entry was lost exited 0\n%s", stderr)
		require.Equal(t, []string{"intent"}, createBucketEntries(h.journal(id), createBucketAfterLossCommand))
		in, found := createBucketSelection(t, h, id)
		require.True(t, found)
		require.Equal(t, uint64(1), in.ReplayFrom)
		require.Equal(t, &createBucketArchive{FirstSeq: 1, LastSeq: msgs, Msgs: msgs}, in.Archive)
		killed, present, first, found := createBucketState(h)
		require.True(t, present)
		require.True(t, found)
		require.Equal(t, createBucketJSON(ownSentinel(familySession, selection.StoreState{Cutover: selection.CutoverDone, Boundary: 3, ReplayFrom: 1, Orphans: []string{orphan},
			BackupSHA256: in.BackupSHA256, Hold: id})), createBucketJSON(first.state))

		// The notification stream moves on: it keeps only its last 4 messages.
		_, js := h.operator()
		require.Equal(t, uint64(7), createBucketKeepLast(t, js, 4))

		// Control: a rerun given another replay_from stops, and changes nothing.
		code, _, stderr = h.run("create-bucket", "--hold", id, "--after-loss", "--replay-from", "7", "--backup", dir)
		require.Equal(t, 1, code, stderr)
		require.Contains(t, stderr, "this recovery's inputs are its hold's selection record, and this run names others: --replay-from 7, where it holds 1")
		_, _, unchanged, _ := createBucketState(h)
		require.Equal(t, first.revision, unchanged.revision)

		// Control: a restore over the present bucket is refused by the server.
		nc, _ = h.operator()
		mgr, err := jsm.New(nc, jsm.WithTimeout(30*time.Second))
		require.NoError(t, err)
		_, _, err = mgr.RestoreSnapshotFromDirectory(ctx, selectionStreamName, dir)
		require.Error(t, err, "control: the server restored over the present bucket")
		t.Logf("control: a restore over the present bucket: %v", err)

		// Control: the default replay_from now is the stream's first sequence, above the
		// sentinel's, so a sentinel written again from this reading would raise it.
		now, err := streamInfo(ctx, nc, notifications)
		require.NoError(t, err)
		require.Greater(t, max(1, now.State.FirstSeq), first.state.ReplayFrom, "control: a rewrite would not raise replay_from")

		code, stdout, stderr := h.run("create-bucket", "--hold", id, "--after-loss", "--backup", dir)
		require.Equal(t, 0, code, stderr)
		require.Contains(t, stdout, "adopted: its sentinel names this recovery's inputs")
		require.Contains(t, stdout, "postcheck passed")
		after, _, adopted, _ := createBucketState(h)
		require.Equal(t, first.revision, adopted.revision, "the rerun wrote the sentinel again")
		require.Equal(t, createBucketJSON(first.state), createBucketJSON(adopted.state))
		require.Equal(t, killed.State.Msgs, after.State.Msgs, "the rerun restored or wrote more than its barrier")
		nc, _ = h.operator()
		store := selection.NewKVStore(selection.ConnRequester{Conn: nc})
		for key, record := range kept {
			got, _, err := store.Read(ctx, key)
			require.NoError(t, err, key)
			record.Key = key
			require.Equal(t, record, got, key)
		}
		d := h.journal(id)
		require.Equal(t, []string{"intent", "intent", "done"}, createBucketEntries(d, createBucketAfterLossCommand))
		require.True(t, d.Complete)
	})

	t.Run("a walk load adopted by its keys, never by last_seq", func(t *testing.T) {
		h := newStack(t)
		createBucketNotifications(t, h, 3)
		// The walk of a bucket whose every record was written twice: its sequences have gaps, as a
		// bucket's history of one leaves them, and its last_seq is past its record count.
		records := createBucketWalkOfRecords(t, 5)
		for i := range records {
			records[i].Seq = uint64(2*i + 1)
		}
		dir := filepath.Join(t.TempDir(), "walk")
		createBucketWriteWalk(t, dir, records, 1, uint64(2*len(records)))
		id := h.heldHold(holdrecord.WorkflowSelectionRecovery)
		createBucketKilledRun(t, h, id, dir, &createBucketPutCutDialer{on: "$KV." + selection.SessionBucket + "." + selection.SentinelKey, after: 1})
		loaded, present, _, found := createBucketState(h)
		require.True(t, present)
		require.False(t, found, "the killed run wrote the sentinel")
		require.Len(t, createBucketKeys(loaded, id), 5, "the killed run's load: the walk's 5 records")

		// Control: an adoption by last_seq, the snapshot's rule (createBucketArchiveIntact) over the
		// walk's recorded state (its manifest's first_seq, last_seq and records), stops on this load.
		manifest, _, err := kvrecords.Read(dir)
		require.NoError(t, err)
		require.NotEqual(t, manifest.LastSeq, loaded.State.LastSeq, "control: the load kept the walk's last_seq")
		in, recorded := createBucketSelection(t, h, id)
		require.True(t, recorded)
		byLastSeq := in
		byLastSeq.Format = createBucketFormatSnapshot
		byLastSeq.Archive = &createBucketArchive{FirstSeq: manifest.FirstSeq, LastSeq: manifest.LastSeq, Msgs: uint64(manifest.Records)}
		nc, _ := h.operator()
		adoption, err := createBucketAdopt(ctx, nc, loaded, createBucketSentinelRead{}, false, byLastSeq, nil, id)
		require.NoError(t, err)
		require.Equal(t, createBucketStop, adoption.action, "control: an adoption by last_seq took the load: %s", adoption.why)
		require.Contains(t, adoption.why, "selection bucket exists and is not this recovery's")

		code, stdout, stderr := h.run("create-bucket", "--hold", id, "--after-loss", "--backup", dir)
		require.Equal(t, 0, code, stderr)
		require.Contains(t, stdout, "adopted: it holds the walk's 5 records as replayed; its sentinel written")
		require.Contains(t, stdout, "postcheck passed")
		require.True(t, h.journal(id).Complete)
		after, _, sentinel, found := createBucketState(h)
		require.True(t, found)
		require.True(t, after.Created.Equal(loaded.Created), "the rerun made the loaded bucket again")
		require.Equal(t, uint64(5+2), after.State.Msgs, "the adopted bucket: the walk's 5 records, the sentinel and this hold's barrier")
		require.Equal(t, uint64(2), sentinel.state.Boundary)
		require.Equal(t, in.BackupSHA256, sentinel.state.BackupSHA256)
		nc, _ = h.operator()
		store := selection.NewKVStore(selection.ConnRequester{Conn: nc})
		for i := range 5 {
			got, _, err := store.Read(ctx, fmt.Sprintf("ses_%02d", i))
			require.NoError(t, err)
			require.Equal(t, uint64(i+1), got.Start)
		}
	})

	t.Run("a cut restore's leftover on three servers", func(t *testing.T) {
		restoreTimeout := createBucketRestoreTimeout
		createBucketRestoreTimeout = 5 * time.Second
		t.Cleanup(func() { createBucketRestoreTimeout = restoreTimeout })
		h := newStack(t)
		createBucketNotifications(t, h, 3)
		dir, msgs := createBucketBackupOf(t, 3, selection.StoreState{Cutover: selection.CutoverDone, Boundary: 1, Hold: "the-cutover"}, nil, 120)
		id := h.heldHold(holdrecord.WorkflowSelectionRecovery)

		killed, kill := context.WithCancel(ctx)
		defer kill()
		dialer := &createBucketCutDialer{cut: kill}
		code, _, stderr := h.runDialed(killed, dialer, "create-bucket", "--hold", id, "--after-loss", "--backup", dir)
		require.True(t, dialer.done.Load(), "the restore was never cut\n%s", stderr)
		require.NotEqual(t, 0, code, stderr)

		// The leftover: an empty stream of the bucket's name appears within the wait.
		nc, _ := h.operator()
		require.Eventually(t, func() bool {
			info, present, err := createBucketStream(ctx, nc, false)
			return err == nil && present && info.State.Msgs == 0
		}, 2*createBucketLeftoverWait, time.Second, "no empty leftover of the cut restore appeared")

		// Control: without the leftover rule the rerun reads the leftover as another recovery's
		// bucket and stops, naming it.
		createBucketRestoresLeftover = false
		code, _, stderr = h.run("create-bucket", "--hold", id, "--after-loss", "--backup", dir)
		createBucketRestoresLeftover = true
		require.Equal(t, 1, code, stderr)
		require.Contains(t, stderr, "selection bucket exists and is not this recovery's: "+selection.SessionBucket)

		code, stdout, stderr := h.run("create-bucket", "--hold", id, "--after-loss", "--backup", dir)
		require.Equal(t, 0, code, stderr)
		require.Contains(t, stdout, "restored and its sentinel written")
		require.Contains(t, stdout, "postcheck passed")
		info, present, sentinel, found := createBucketState(h)
		require.True(t, present)
		require.True(t, found)
		require.Equal(t, msgs+1, info.State.Msgs, "the restored bucket: its archive's messages and this hold's barrier")
		require.Equal(t, id, sentinel.state.Hold)
		require.True(t, h.journal(id).Complete)
	})
}

// leftWith sets the stop rule's earlier rules (leftRuleSet) for the rest of the test: a control's.
func leftWith(t *testing.T, rules leftRuleSet) {
	t.Helper()
	previous := leftRules
	leftRules = rules
	t.Cleanup(func() { leftRules = previous })
}

// A create-bucket stop counts the barrier key an earlier run of create-bucket in the hold put: a
// stop names every change its command's runs made and did not undo. Run 1 finds the bucket present
// with another hold's sentinel: it puts this hold's barrier key before it reads, then stops, and
// its stop names the key. Run 2, with --replicas 3, stops at the bucket's configuration before any
// barrier, having changed nothing itself; its stop names run 1, whose key is still there, so it is
// not unchanged. Control: under the earlier rules that a stop records its own run alone
// (leftRuleSet.ownRunOnly) and an UNKNOWN's stop its run's account (leftRuleSet.unknownAsAccount,
// since run 2's refusal is UNKNOWN), the same refusal records unchanged.
func TestACreateBucketStopCountsTheBarrierKeyAnEarlierRunPut(t *testing.T) {
	requireStoppable(t, holdrecord.WorkflowCutover, createBucketCommand)
	h := newStack(t)
	createBucketNotifications(t, h, 0)
	id := h.prepared(holdrecord.WorkflowCutover)
	createBucketCohort(t, h.records(), id, 2)
	h.held(id)
	nc, _ := h.operator()
	createBucketMakeBucket(t, nc, selection.StoreState{Boundary: 3, Hold: "another-hold"})

	code, _, stderr := h.run("create-bucket", "--hold", id)
	require.Equal(t, 2, code, stderr)
	require.Contains(t, stderr, "exists and is not this hold's")
	require.Contains(t, h.lastEntry(id, createBucketCommand).Outputs["left"], barrierLeft, "run 1's stop names the key it put")

	refusedBeforeTheBarrier := func() holdrecord.Operation {
		t.Helper()
		code, _, stderr := h.run("create-bucket", "--hold", id, "--replicas", "3")
		require.Equal(t, 2, code, stderr)
		require.Contains(t, stderr, "num_replicas 1, not 3")
		require.Contains(t, stderr, "it left nothing outside the journal", "the hold point: this run changed nothing itself")
		stop := h.lastEntry(id, createBucketCommand)
		require.Equal(t, "stop", stop.State)
		return stop
	}

	leftWith(t, leftRuleSet{ownRunOnly: true, unknownAsAccount: true})
	require.Equal(t, holdrecord.LeftUnchanged, refusedBeforeTheBarrier().Outputs["left"],
		"control: a stop that records its own run alone, its UNKNOWN as its account, records unchanged")

	leftWith(t, leftRuleSet{})
	left := refusedBeforeTheBarrier().Outputs["left"]
	require.NotEqual(t, holdrecord.LeftUnchanged, left, "the stop recorded unchanged over run 1's barrier key")
	require.Contains(t, left, "earlier runs of "+createBucketCommand+" in this hold: run 1 stopped, and its stop entry records what it left")
}

// A create-bucket --after-loss stop leaves out this hold's barrier key when the run deleted the
// bucket that held it: a stop names every change its command's runs made and did not undo. The
// hold finds the empty leftover of a cut restore (no key, no sentinel) beside a snapshot whose
// session key holds the server's STREAM.INFO answer: the run puts the barrier, deletes the leftover
// with the key in it, restores the snapshot, finds the malformed value and deletes the restored
// bucket again. Its stop names the deletes and not the key, and the bucket is absent. Control:
// under the earlier rule that the key counts once put (leftRuleSet.barrierAfterDelete), the same
// run's stop names the key.
func TestARecoveryStopLeavesOutTheBarrierKeyItsDeleteTook(t *testing.T) {
	requireStoppable(t, holdrecord.WorkflowSelectionRecovery, createBucketAfterLossCommand)
	ctx := context.Background()
	h := newStack(t)
	createBucketNotifications(t, h, 3)
	createBucketBackedBucket(t, h)
	released, _ := h.operator()
	createBucketStoreAnswer(t, released, released, createBucketStreamInfo, "ses_forged")
	h.closeOperators()
	dir := createBucketHeldSnapshot(t, h)
	id := createBucketLoseBucket(t, h)

	run := func() holdrecord.Operation {
		t.Helper()
		nc, js := h.operator()
		_, err := js.CreateKeyValue(ctx, selection.SessionBucketConfig(1))
		require.NoError(t, err, "the cut restore's empty leftover")
		require.NoError(t, createBucketAwaitLeader(ctx, nc))
		code, _, stderr := h.run("create-bucket", "--hold", id, "--after-loss", "--backup", dir)
		require.Equal(t, 1, code, stderr)
		require.Contains(t, stderr, "the leftover of a cut restore or load of "+selectionStreamName+" deleted", "the hold point: the run deleted the leftover")
		require.Contains(t, stderr, "the bucket "+selectionStreamName+" restored from the backup and deleted again")
		_, present, _, _ := createBucketState(h)
		require.False(t, present, "the refused snapshot's restored bucket was left")
		stop := h.lastEntry(id, createBucketAfterLossCommand)
		require.Equal(t, "stop", stop.State)
		return stop
	}

	left := run().Outputs["left"]
	require.Contains(t, left, "the leftover of a cut restore or load of "+selectionStreamName+" deleted")
	require.NotContains(t, left, barrierLeft, "the stop named the barrier key that the run's own delete took")

	leftWith(t, leftRuleSet{barrierAfterDelete: true})
	require.Contains(t, run().Outputs["left"], barrierLeft, "control: under the earlier rule the stop names the deleted key")
}

// A create-bucket whose done entry is lost, after it made the bucket and its sentinel, writes its
// stop, recording what it left as unknown, and hold abort is then the way out
// (requireALostDoneStops, with its control: the intent left pending under the earlier rule). The
// rerun adopts the bucket the first run made.
func TestACreateBucketWhoseDoneEntryIsLostStops(t *testing.T) {
	h := newStack(t)
	createBucketNotifications(t, h, 0)
	id := h.prepared(holdrecord.WorkflowCutover)
	createBucketCohort(t, h.records(), id, 2)
	h.held(id)
	h.requireALostDoneStops(id, createBucketCommand, func() (int, string, string) {
		return h.run("create-bucket", "--hold", id)
	})
}

// A create-bucket --after-loss whose done entry is lost, after it made the bucket and wrote its
// sentinel, writes its stop, recording what it left as unknown, and hold abort is then the way out
// (requireALostDoneStops, with its control: the intent left pending under the earlier rule). The
// rerun adopts the bucket by its sentinel.
func TestARecoveryWhoseDoneEntryIsLostStops(t *testing.T) {
	h := newStack(t)
	createBucketNotifications(t, h, 0)
	id := h.heldHold(holdrecord.WorkflowSelectionRecovery)
	h.requireALostDoneStops(id, createBucketAfterLossCommand, func() (int, string, string) {
		return h.run("create-bucket", "--hold", id, "--after-loss")
	})
}

// A bucket with no sentinel whose one key is another hold's barrier key, hold.barrier.<that hold>,
// as a run of that hold leaves it when it put its barrier and stopped short of the sentinel, holds
// no record: every hold's barrier key is no key of the bucket's (createBucketKeys), so each
// create-bucket takes it as the empty bucket it is. create-bucket writes its sentinel with this
// hold's B into it, leaving the other hold's key; create-bucket --after-loss with no backup adopts
// it and writes its sentinel; from a walk it deletes it and loads the walk, as it does its own cut
// load's leftover. Control, first in each: under the earlier rule, which took this hold's barrier
// key alone for no key (createBucketIgnoresEveryBarrier unset), each finds the bucket not its own,
// and no command of this package took it.
func TestCreateBucketTakesABucketHoldingAnotherHoldsBarrier(t *testing.T) {
	ctx := context.Background()
	const other = "another-hold"
	otherKey := "$KV." + selection.SessionBucket + ".hold.barrier." + other
	// leftover makes the bucket that other's run left: the configuration create-bucket sends,
	// holding other's barrier key alone.
	leftover := func(t *testing.T, h *stack) {
		t.Helper()
		nc, js := h.operator()
		_, err := js.CreateKeyValue(ctx, selection.SessionBucketConfig(1))
		require.NoError(t, err)
		require.NoError(t, createBucketAwaitLeader(ctx, nc))
		require.NoError(t, holdrecord.Barrier(ctx, js, selection.SessionBucket, other))
	}
	everyBarrier := func(t *testing.T, every bool) {
		t.Helper()
		previous := createBucketIgnoresEveryBarrier
		createBucketIgnoresEveryBarrier = every
		t.Cleanup(func() { createBucketIgnoresEveryBarrier = previous })
	}

	t.Run("create-bucket", func(t *testing.T) {
		h := newStack(t)
		createBucketNotifications(t, h, 0)
		id := h.prepared(holdrecord.WorkflowCutover)
		createBucketCohort(t, h.records(), id, 4)
		h.held(id)
		leftover(t, h)

		everyBarrier(t, false)
		code, _, stderr := h.run("create-bucket", "--hold", id)
		require.Equal(t, 2, code, "control: the earlier rule: %s", stderr)
		require.Contains(t, stderr, "exists and is not this hold's: it holds 1 keys (hold.barrier."+other+"), no sentinel")

		everyBarrier(t, true)
		code, stdout, stderr := h.run("create-bucket", "--hold", id)
		require.Equal(t, 0, code, stderr)
		require.Contains(t, stdout, "which held no key: its sentinel written with boundary 4")
		info, _, sentinel, found := createBucketState(h)
		require.True(t, found)
		require.Equal(t, createBucketJSON(ownSentinel(familySession, selection.StoreState{Boundary: 4, Hold: id})), createBucketJSON(sentinel.state))
		require.Contains(t, info.State.Subjects, otherKey, "create-bucket removed the other hold's barrier key")
		require.Equal(t, "done", h.lastEntry(id, createBucketCommand).State)
	})

	t.Run("create-bucket --after-loss", func(t *testing.T) {
		h := newStack(t)
		createBucketNotifications(t, h, 0)
		id := h.heldHold(holdrecord.WorkflowSelectionRecovery)
		leftover(t, h)

		everyBarrier(t, false)
		code, _, stderr := h.run("create-bucket", "--hold", id, "--after-loss")
		require.Equal(t, 1, code, "control: the earlier rule: %s", stderr)
		require.Contains(t, stderr, "selection bucket exists and is not this recovery's: "+selection.SessionBucket+" holds 1 keys (hold.barrier."+other+"), no sentinel")

		everyBarrier(t, true)
		code, stdout, stderr := h.run("create-bucket", "--hold", id, "--after-loss")
		require.Equal(t, 0, code, stderr)
		require.Contains(t, stdout, "adopted: it held no key; its sentinel written")
		require.True(t, h.journal(id).Complete)
		info, _, sentinel, found := createBucketState(h)
		require.True(t, found)
		require.Equal(t, id, sentinel.state.Hold)
		require.Contains(t, info.State.Subjects, otherKey, "the recovery removed the other hold's barrier key")
	})

	t.Run("create-bucket --after-loss from a walk", func(t *testing.T) {
		h := newStack(t)
		createBucketNotifications(t, h, 3)
		records := createBucketWalkOfRecords(t, 3)
		dir := filepath.Join(t.TempDir(), "walk")
		createBucketWriteWalk(t, dir, records, 1, uint64(len(records)))
		id := h.heldHold(holdrecord.WorkflowSelectionRecovery)
		leftover(t, h)

		everyBarrier(t, false)
		code, _, stderr := h.run("create-bucket", "--hold", id, "--after-loss", "--backup", dir)
		require.Equal(t, 1, code, "control: the earlier rule: %s", stderr)
		require.Contains(t, stderr, "selection bucket exists and is not this recovery's: "+selection.SessionBucket+" holds 1 keys that are no record of the walk")

		everyBarrier(t, true)
		code, stdout, stderr := h.run("create-bucket", "--hold", id, "--after-loss", "--backup", dir)
		require.Equal(t, 0, code, stderr)
		require.Contains(t, stdout, "restored and its sentinel written")
		require.True(t, h.journal(id).Complete)
		info, _, _, found := createBucketState(h)
		require.True(t, found)
		require.NotContains(t, info.State.Subjects, otherKey, "the walk's load kept the deleted bucket's key")
	})
}
