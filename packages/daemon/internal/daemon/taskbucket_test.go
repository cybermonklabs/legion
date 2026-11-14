package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/sjawhar/envoy/daemonpermissions"
	"github.com/sjawhar/envoy/legiontasks"
	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/natsauth"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testnats"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// bucketCheck is how often a bucket rig's daemon reads its task bucket's stream for a bucket gone
// or created again (overrides.taskBucketCheck), and settle the wait a test gives the daemon to
// write a value a second time, which several checks and watch deliveries fit in.
const (
	bucketCheck = 100 * time.Millisecond
	settle      = 10 * bucketCheck
)

// bucketRig is a daemon on the test NATS whose machines write their claims' values to its own task
// bucket (taskBucket), over the fake runtime, with its log kept at Debug, and a JetStream client of
// the test's own that reads and changes the bucket as another writer of the shared NATS user could.
type bucketRig struct {
	t       *testing.T
	cfg     config.Config
	project string
	rt      *fake.Runtime
	record  *built
	o       overrides
	js      jetstream.JetStream
	// logs is the running daemon's log; each boot starts a new one, so what a test reads after a
	// restart is that boot's.
	logs *syncBuffer
	d    *daemon
}

func newBucketRig(t *testing.T) *bucketRig {
	t.Helper()
	natsURL := workflowNATS(t)
	cfg := testConfig(t)
	cfg.NatsURLs = []string{natsURL}
	project, err := claim.ProjectToken(cfg.Project)
	if err != nil {
		t.Fatal(err)
	}
	r := &bucketRig{t: t, cfg: cfg, project: project, rt: fake.NewRuntime(), record: &built{}, js: workflowJetStream(t, natsURL)}
	r.o = fakeRuntime(r.rt, r.record)
	r.o.taskValues = nil
	r.o.taskBucketCheck = bucketCheck
	return r
}

func (r *bucketRig) boot() {
	r.t.Helper()
	r.logs = &syncBuffer{}
	r.d = startDaemonLogging(r.t, r.cfg, r.o, slog.New(slog.NewJSONHandler(r.logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
}

// restart stops the daemon and boots another on the same store, runtime and bucket.
func (r *bucketRig) restart() {
	r.t.Helper()
	r.d.stop()
	rebindHeldPorts(r.t, &r.cfg)
	r.boot()
}

// entry is the bucket's latest entry of key, nil where it holds no value for it or does not exist.
func (r *bucketRig) entry(key string) jetstream.KeyValueEntry {
	r.t.Helper()
	kv, err := r.js.KeyValue(context.Background(), legiontasks.Bucket)
	if errors.Is(err, jetstream.ErrBucketNotFound) {
		return nil
	}
	if err != nil {
		r.t.Fatalf("open the task bucket: %v", err)
	}
	entry, err := kv.Get(context.Background(), key)
	if errors.Is(err, jetstream.ErrKeyNotFound) {
		return nil
	}
	if err != nil {
		r.t.Fatalf("read %s from the task bucket: %v", key, err)
	}
	return entry
}

// awaitEntry waits until key's entry is one want holds, and returns it.
func (r *bucketRig) awaitEntry(what, key string, want func(jetstream.KeyValueEntry) bool) jetstream.KeyValueEntry {
	r.t.Helper()
	var got jetstream.KeyValueEntry
	testwait.Eventually(r.t, what, func() bool {
		got = r.entry(key)
		return want(got)
	})
	return got
}

// put writes task as key's value, as another writer of the shared NATS user could, and returns the
// write's revision.
func (r *bucketRig) put(key string, task uint64) uint64 {
	r.t.Helper()
	value, err := legiontasks.Value{Task: task}.Encode()
	if err != nil {
		r.t.Fatal(err)
	}
	kv, err := r.js.KeyValue(context.Background(), legiontasks.Bucket)
	if err != nil {
		r.t.Fatalf("open the task bucket: %v", err)
	}
	revision, err := kv.Put(context.Background(), key, value)
	if err != nil {
		r.t.Fatalf("write %s: %v", key, err)
	}
	return revision
}

// remove deletes key's value, as another writer could.
func (r *bucketRig) remove(key string) {
	r.t.Helper()
	kv, err := r.js.KeyValue(context.Background(), legiontasks.Bucket)
	if err != nil {
		r.t.Fatalf("open the task bucket: %v", err)
	}
	if err := kv.Delete(context.Background(), key); err != nil {
		r.t.Fatalf("delete %s: %v", key, err)
	}
}

// epoch is the task bucket's epoch now.
func (r *bucketRig) epoch() string {
	r.t.Helper()
	stream, err := r.js.Stream(context.Background(), legiontasks.Stream)
	if err != nil {
		r.t.Fatalf("read the task bucket's stream: %v", err)
	}
	return legiontasks.Epoch(stream.CachedInfo().Created)
}

// lines is every line of the running daemon's log whose message is msg and which is token's.
func (r *bucketRig) lines(msg string, token claim.Token) []map[string]any {
	r.t.Helper()
	var own []map[string]any
	for _, line := range loggedLines(r.t, r.logs.String(), msg) {
		if line["msg"] == msg && line["claim"] == string(token) {
			own = append(own, line)
		}
	}
	return own
}

// writes is every write of token's value the running daemon logged, with its cause.
func (r *bucketRig) writes(token claim.Token) []string {
	r.t.Helper()
	var causes []string
	for _, line := range r.lines("supervise: a task value was written", token) {
		causes = append(causes, line["cause"].(string))
	}
	return causes
}

// accepted counts the takes of token's task the running daemon accepted.
func (r *bucketRig) accepted(token claim.Token) int {
	r.t.Helper()
	n := 0
	for _, line := range r.lines("supervise: a take was answered", token) {
		if line["outcome"] == string(supervise.TakeOutcomeAccepted) {
			n++
		}
	}
	return n
}

// awaitStood waits for token's machine to have read the bucket's value at cause and found it the
// one its record wrote: the read every machine is handed at the boot, or by the watch.
func (r *bucketRig) awaitStood(token claim.Token, cause string) {
	r.t.Helper()
	testwait.Eventually(r.t, "the claim's machine reads the bucket's value as the one its record wrote", func() bool {
		for _, line := range r.lines("supervise: the task bucket holds the value the record wrote", token) {
			if line["cause"] == cause {
				return true
			}
		}
		return false
	})
}

// deliver is the operator's task to token's claim.
func (r *bucketRig) deliver(token claim.Token, task string) {
	r.t.Helper()
	if status, body := r.d.request(http.MethodPost, "/legion/v1/operator/claims/"+string(token)+"/deliver",
		api.DeliverRequest{Task: task}, true); status != http.StatusOK {
		r.t.Fatalf("deliver to %s = %d; body %s", token, status, body)
	}
}

// taskOf is the number entry's value names.
func taskOf(t *testing.T, entry jetstream.KeyValueEntry) uint64 {
	t.Helper()
	value, err := legiontasks.Decode(entry.Value())
	if err != nil {
		t.Fatalf("decode %s's value: %v", entry.Key(), err)
	}
	return value.Task
}

// bucketWorker is one claim's worker process as its pane runs it: the shim on the worker stream,
// which answers what the daemon asks of the agent, and the plugin that takes the claim's task
// (takeDriver). The plugin's memory of what it was handed and what it asked at is the process's, so
// it outlives a daemon restart: the worker reconnects to the daemon that started (reattach).
type bucketWorker struct {
	rig        *bucketRig
	token      claim.Token
	session    string
	bootToken  string
	generation uint64
	secret     string
	sh         *shim
	driver     *takeDriver
	// silent is whether the shim leaves the daemon's get_state unanswered, as one whose agent is
	// busy compacting answers nothing the daemon can read as idle or as in a turn.
	silent bool
}

// spawn has the operator spawn an architect of issue with task, and connects its worker's shim and
// registers its agent; the agent says it is ready only at ready.
func (r *bucketRig) spawn(issue, task string) *bucketWorker {
	r.t.Helper()
	token := r.d.spawn(api.SpawnRequest{Tree: issue, Issue: issue, Role: claim.RoleArchitect, Prompt: "Reply ready and wait.", Task: task})
	launch := lastLaunch(r.t, r.rt, token)
	w := &bucketWorker{rig: r, token: token, session: "ses_" + string(token), bootToken: launch.BootToken, generation: launch.Generation}
	w.connect()
	testwait.Eventually(r.t, "the hello to reach the machine", func() bool {
		return r.d.claim(token).State == string(supervise.StateShimConnected)
	})
	status, body := r.d.request(http.MethodPost, "/legion/v1/claims/register", claim.RegisterRequest{
		BootToken: w.bootToken, SessionID: w.session, OmpSessionFile: "/sessions/" + string(token) + ".jsonl",
		AgentID: "agent", PluginContract: api.DaemonAPIVersion,
	}, false)
	if status != http.StatusOK {
		r.t.Fatalf("register %s = %d; body %s", token, status, body)
	}
	var registered claim.RegisterResponse
	if err := json.Unmarshal(body, &registered); err != nil {
		r.t.Fatalf("decode the registration: %v", err)
	}
	w.secret = registered.Secret
	w.driver = r.d.takeDriver(w.sh, token, w.session, w.secret, w.generation)
	return w
}

// connect dials the worker stream with the launch's boot token and serves the daemon's frames.
func (w *bucketWorker) connect() {
	w.rig.t.Helper()
	w.rig.record.mu.Lock()
	address := w.rig.record.address
	w.rig.record.mu.Unlock()
	w.sh = dialShimUntilAcked(w.rig.t, address, w.bootToken)
	go w.serve(w.sh)
}

// serve answers the protocol negotiation and, unless the shim is silent, the state probe — idle,
// or streaming while a turn of the plugin's runs — for as long as the connection lasts.
func (w *bucketWorker) serve(sh *shim) {
	_ = sh.conn.SetReadDeadline(time.Time{})
	for {
		line, err := sh.lines.ReadBytes('\n')
		if err != nil {
			return
		}
		frame, err := shimwire.Decode(line)
		if err != nil {
			continue
		}
		switch f := frame.(type) {
		case shimwire.NegotiateProtocol:
			_ = sh.writer.WriteFrame(shimwire.Response{ID: f.ID, Command: shimwire.TypeNegotiateProtocol, Success: true})
		case shimwire.GetState:
			if w.silent {
				continue
			}
			streaming := w.driver != nil && w.driver.inATurn()
			data, _ := json.Marshal(shimwire.StateData{IsStreaming: &streaming})
			_ = sh.writer.WriteFrame(shimwire.Response{ID: f.ID, Command: shimwire.TypeGetState, Success: true, Data: data})
		}
	}
}

// ready is the agent saying it is ready.
func (w *bucketWorker) ready() {
	w.rig.t.Helper()
	if status, body := w.rig.d.request(http.MethodPost, "/legion/v1/claims/ready", claim.ReadyRequest{
		ClaimToken: w.token, SessionID: w.session, Secret: w.secret, Generation: w.generation,
	}, false); status != http.StatusNoContent {
		w.rig.t.Fatalf("ready %s = %d; body %s", w.token, status, body)
	}
}

// reattach is the worker's process reaching the daemon that started after a restart: its shim
// reconnects, and its plugin, with all it remembers, posts to the new daemon.
func (w *bucketWorker) reattach() {
	w.rig.t.Helper()
	w.connect()
	w.driver.post, w.driver.send = w.rig.d.post, w.sh.writer.WriteFrame
}

// The daemon watches the whole task bucket, which every project's daemon shares, and repairs and
// deletes the keys of its own project alone. A key of its own claim changed by another writer is
// written again from the record; another project's keys, one of whose project token begins with
// this one's, are left as they stand, written or deleted; and once the record owes the claim
// nothing, a value written into its key is deleted while the same write into another project's
// key stands.
func TestTaskBucketRepairsOnlyItsOwnProjectsKeys(t *testing.T) {
	r := newBucketRig(t)
	r.boot()
	w := r.spawn("LEGION-1", "Say hello.")
	w.ready()
	key := string(w.token)
	first := r.awaitEntry("the claim's value written", key, func(e jetstream.KeyValueEntry) bool { return e != nil })
	number := taskOf(t, first)

	other := "legion-" + r.project + "x-legion-1-architect"
	gone := "legion-elsewhere-legion-1-architect"
	otherRevision := r.put(other, 7)
	r.put(gone, 8)
	r.remove(gone)
	tampered := r.put(key, number+40)

	repaired := r.awaitEntry("the claim's value written again from the record", key, func(e jetstream.KeyValueEntry) bool {
		return e != nil && e.Revision() > tampered
	})
	if got := taskOf(t, repaired); got != number {
		t.Fatalf("the repair wrote task %d, want the record's %d", got, number)
	}
	// The watch hands over the bucket's changes in order, so the other projects' came before the
	// claim's own, and the daemon has passed them.
	if e := r.entry(other); e == nil || e.Revision() != otherRevision || taskOf(t, e) != 7 {
		t.Fatalf("another project's key %s is %v, want its own write of task 7 at revision %d left as it stands", other, e, otherRevision)
	}
	if e := r.entry(gone); e != nil {
		t.Fatalf("another project's deleted key %s holds %q, want it left deleted", gone, e.Value())
	}
	if causes := r.writes(w.token); !slices.Equal(causes, []string{"queued", "repair"}) {
		t.Fatalf("the daemon wrote the claim's value for %v, want its queue and one repair", causes)
	}
	if stored := storedClaim(t, r.cfg, w.token); stored.TaskWrittenRevision != repaired.Revision() || stored.TaskWritten != number {
		t.Fatalf("the record holds written %d at revision %d, want task %d at the repair's revision %d",
			stored.TaskWritten, stored.TaskWrittenRevision, number, repaired.Revision())
	}

	taken := w.driver.mustTake(t, "the worker takes the repaired value")
	if taken.Number != number {
		t.Fatalf("the worker took task %d, want %d", taken.Number, number)
	}
	w.driver.mustStartTurn(t)
	w.driver.mustEndTurn(t)
	r.awaitEntry("the finished task's value deleted", key, func(e jetstream.KeyValueEntry) bool { return e == nil })

	stray := r.put(key, number)
	otherAgain := r.put(other, 9)
	r.awaitEntry("a value of a claim the record owes nothing deleted", key, func(e jetstream.KeyValueEntry) bool { return e == nil })
	if e := r.entry(other); e == nil || e.Revision() != otherAgain || taskOf(t, e) != 9 {
		t.Fatalf("another project's key %s is %v after the daemon deleted its own stray value (revision %d), want task 9 at revision %d left",
			other, e, stray, otherAgain)
	}
}

// A relay frame is lost while the session it is for is not subscribed, so a worker reads its
// claim's value from the daemon at its delivery's next epoch. The Go stand-in for the plugin is
// unsubscribed while the value is written, and written again; at its next epoch it reads
// claims/task and is handed the bucket's latest revision under the bucket's own epoch, which it
// reports seen and takes.
func TestTaskBucketAValueDroppedWhileUnsubscribedIsReadAtTheNextEpoch(t *testing.T) {
	r := newBucketRig(t)
	r.boot()
	w := r.spawn("LEGION-1", "")
	w.ready()
	if current, err := w.driver.task(); err != nil || current.Task != nil {
		t.Fatalf("the claim's value before any task = %+v (%v), want none", current.Task, err)
	}

	r.deliver(w.token, "Say hello.")
	key := string(w.token)
	written := r.awaitEntry("the task's value written", key, func(e jetstream.KeyValueEntry) bool { return e != nil })
	r.remove(key)
	latest := r.awaitEntry("the value written again", key, func(e jetstream.KeyValueEntry) bool {
		return e != nil && e.Revision() > written.Revision()
	})

	current, err := w.driver.task()
	if err != nil {
		t.Fatalf("read claims/task at the next epoch: %v", err)
	}
	want := claim.TaskValue{Number: taskOf(t, latest), Epoch: r.epoch(), Revision: latest.Revision()}
	if current.Task == nil || *current.Task != want {
		t.Fatalf("claims/task at the next epoch = %+v, want the bucket's latest %+v", current.Task, want)
	}
	if taken := w.driver.mustTake(t, "the stand-in takes the value it read"); taken.Number != want.Number || taken.Text != "Say hello." {
		t.Fatalf("the stand-in took %+v, want task %d's text", taken, want.Number)
	}
}

// valueRepairedOnce is a claim whose value another writer changed (change), written again from
// the record exactly once under the record's own number, and taken at the repair's revision.
func valueRepairedOnce(t *testing.T, change func(r *bucketRig, key string, number uint64)) {
	t.Helper()
	r := newBucketRig(t)
	r.boot()
	w := r.spawn("LEGION-1", "Say hello.")
	w.ready()
	key := string(w.token)
	first := r.awaitEntry("the claim's value written", key, func(e jetstream.KeyValueEntry) bool { return e != nil })
	number := taskOf(t, first)
	change(r, key, number)
	repaired := r.awaitEntry("the value written again from the record", key, func(e jetstream.KeyValueEntry) bool {
		return e != nil && e.Revision() > first.Revision()+1
	})
	time.Sleep(settle)
	if e := r.entry(key); e == nil || e.Revision() != repaired.Revision() || taskOf(t, e) != number {
		t.Fatalf("after the repair the key holds %v, want task %d at the repair's revision %d, written once", e, number, repaired.Revision())
	}
	if causes := r.writes(w.token); !slices.Equal(causes, []string{"queued", "repair"}) {
		t.Fatalf("the daemon wrote the claim's value for %v, want its queue and one repair", causes)
	}
	stored := storedClaim(t, r.cfg, w.token)
	if stored.TaskWritten != number || stored.Pending == nil || stored.Pending.Number != number || stored.TaskWrittenRevision != repaired.Revision() {
		t.Fatalf("the record holds written %d, pending %+v, last write at %d; want task %d still its number, written at %d",
			stored.TaskWritten, stored.Pending, stored.TaskWrittenRevision, number, repaired.Revision())
	}
	if taken := w.driver.mustTake(t, "the worker takes the repaired value"); taken.Number != number {
		t.Fatalf("the worker took task %d, want %d", taken.Number, number)
	}
}

// A value another writer changed to another number is written again from the record, once, under
// the record's own number.
func TestTaskValueWrittenAgainOnceWhenItDiffersFromTheRecord(t *testing.T) {
	valueRepairedOnce(t, func(r *bucketRig, key string, number uint64) { r.put(key, number+1) })
}

// A value another writer deleted is written again from the record, once, under its own number.
func TestTaskValueWrittenAgainOnceWhenDeleted(t *testing.T) {
	valueRepairedOnce(t, func(r *bucketRig, key string, _ uint64) { r.remove(key) })
}

// A bucket deleted while the daemon runs is created again by it, under a new epoch, and the value
// the record owes is written into it once, under the record's own number; the worker reads it at
// the new epoch and takes it.
func TestTaskValueWrittenFromTheRecordWhenTheBucketIsCreatedAgain(t *testing.T) {
	r := newBucketRig(t)
	r.boot()
	w := r.spawn("LEGION-1", "Say hello.")
	w.ready()
	key := string(w.token)
	first := r.awaitEntry("the claim's value written", key, func(e jetstream.KeyValueEntry) bool { return e != nil })
	number, before := taskOf(t, first), r.epoch()

	if err := r.js.DeleteKeyValue(context.Background(), legiontasks.Bucket); err != nil {
		t.Fatalf("delete the task bucket: %v", err)
	}
	r.awaitEntry("the value written into the bucket created again", key, func(e jetstream.KeyValueEntry) bool { return e != nil })
	time.Sleep(settle)
	epoch := r.epoch()
	if epoch == before {
		t.Fatalf("the bucket created again has epoch %s, the epoch of the one deleted", epoch)
	}
	if e := r.entry(key); e == nil || taskOf(t, e) != number || e.Revision() != 1 {
		t.Fatalf("the new bucket holds %v, want task %d written once", e, number)
	}
	if causes := r.writes(w.token); !slices.Equal(causes, []string{"queued", "repair"}) {
		t.Fatalf("the daemon wrote the claim's value for %v, want its queue and one repair", causes)
	}
	if stored := storedClaim(t, r.cfg, w.token); stored.TaskWrittenEpoch != epoch || stored.TaskWritten != number {
		t.Fatalf("the record holds written %d under epoch %q, want task %d under the new epoch %q", stored.TaskWritten, stored.TaskWrittenEpoch, number, epoch)
	}
	if taken := w.driver.mustTake(t, "the worker takes the value of the new epoch"); taken.Number != number {
		t.Fatalf("the worker took task %d, want %d", taken.Number, number)
	}
}

// A restart writes nothing for a value that still stands: the worker saw it and has not asked
// about it, and the restarted daemon's boot read finds the bucket holding the record's own last
// write, writes nothing, and the worker reconnects and takes that same revision.
func TestTaskValueRestartWithTheValueStandingWritesNothing(t *testing.T) {
	r := newBucketRig(t)
	r.boot()
	w := r.spawn("LEGION-1", "Say hello.")
	w.ready()
	key := string(w.token)
	first := r.awaitEntry("the claim's value written", key, func(e jetstream.KeyValueEntry) bool { return e != nil })
	if err := w.driver.post("/legion/v1/claims/task-seen", claim.TaskSeenRequest{AgentCaller: w.driver.caller, Generation: w.generation,
		Number: taskOf(t, first), Epoch: r.epoch(), Revision: first.Revision()}, nil); err != nil {
		t.Fatalf("report the value seen: %v", err)
	}

	r.restart()
	r.awaitStood(w.token, supervise.TaskValueBoot)
	w.reattach()
	time.Sleep(settle)
	if causes := r.writes(w.token); len(causes) != 0 {
		t.Fatalf("the restarted daemon wrote the standing value for %v, want nothing", causes)
	}
	if e := r.entry(key); e == nil || e.Revision() != first.Revision() {
		t.Fatalf("after the restart the key holds %v, want revision %d standing", e, first.Revision())
	}
	if taken := w.driver.mustTake(t, "the worker takes the standing value"); taken.Number != taskOf(t, first) {
		t.Fatalf("the worker took task %d, want %d", taken.Number, taskOf(t, first))
	}
}

// A hold that refused a take before a restart gets its write when it ends after the restart, and
// one write only: the take of an agent that has not said it is ready is refused by the claim's own
// hold, the restarted daemon's boot writes nothing for a value its worker asked at and was refused,
// and the ready after the restart ends the hold, which writes the value once.
func TestTaskValueAHoldThatRefusedBeforeARestartIsWrittenOnceAfter(t *testing.T) {
	r := newBucketRig(t)
	r.boot()
	w := r.spawn("LEGION-1", "Say hello.")
	key := string(w.token)
	first := r.awaitEntry("the claim's value written", key, func(e jetstream.KeyValueEntry) bool { return e != nil })
	if _, answer, asked, err := w.driver.poll(); err != nil || !asked ||
		answer != (claim.TakeResponse{Outcome: string(supervise.TakeOutcomeRefused), Hold: supervise.HoldNotReady}) {
		t.Fatalf("the take before ready = %+v (asked %t, %v), want it refused %q", answer, asked, err, supervise.HoldNotReady)
	}

	r.restart()
	r.awaitStood(w.token, supervise.TaskValueBoot)
	w.reattach()
	time.Sleep(settle)
	if causes := r.writes(w.token); len(causes) != 0 {
		t.Fatalf("the restarted daemon wrote the refused value for %v before the hold ended, want nothing", causes)
	}
	w.ready()
	r.awaitEntry("the value written at the hold's end", key, func(e jetstream.KeyValueEntry) bool {
		return e != nil && e.Revision() > first.Revision()
	})
	time.Sleep(settle)
	if causes := r.writes(w.token); !slices.Equal(causes, []string{"hold-ended"}) {
		t.Fatalf("the restarted daemon wrote the value for %v, want once at the hold's end", causes)
	}
	if taken := w.driver.mustTake(t, "the worker takes the value the hold's end wrote"); taken.Number != taskOf(t, first) {
		t.Fatalf("the worker took task %d, want %d", taken.Number, taskOf(t, first))
	}
}

// A daemon restarted during the wait a provider error armed writes the owed task once, since its
// worker asked at the value's latest revision and asks once per revision, and the worker takes it
// exactly once, with the provider error's note in front of its text.
func TestTaskValueARestartDuringAProviderErrorsWaitWritesOnceAndIsTakenOnce(t *testing.T) {
	r := newBucketRig(t)
	r.boot()
	w := r.spawn("LEGION-1", "Say hello.")
	w.ready()
	key := string(w.token)
	taken := w.driver.mustTake(t, "the worker takes its task")
	w.driver.mustStartTurn(t)
	w.driver.mu.Lock()
	w.driver.inTurn, w.driver.running = false, 0
	w.driver.mu.Unlock()
	if err := w.driver.send(shimwire.AgentEnd{}); err != nil {
		t.Fatalf("end the turn: %v", err)
	}
	if err := w.driver.report(claim.TurnOutcomeRequest{Outcome: claim.TurnRefused, Message: "overloaded", Task: taken.Number,
		ProviderError: &claim.ProviderError{Status: http.StatusServiceUnavailable, Provider: "anthropic", Model: "claude-opus", Message: "overloaded", Retryable: true}}); err != nil {
		t.Fatal(err)
	}
	testwait.Eventually(t, "the provider error to take the task back into its wait", func() bool {
		p := storedClaim(t, r.cfg, w.token).Pending
		return p != nil && p.Number == taken.Number && p.ConfirmedAt.IsZero() && p.ProviderErrorRetries == 1
	})
	before := r.entry(key)

	r.restart()
	w.reattach()
	r.awaitEntry("the owed task written again after the restart", key, func(e jetstream.KeyValueEntry) bool {
		return e != nil && e.Revision() > before.Revision()
	})
	again := w.driver.mustTake(t, "the worker takes the task again")
	if again.Number != taken.Number || !strings.Contains(again.Text, "provider error") {
		t.Fatalf("the worker took %+v, want task %d with the provider error's note", again, taken.Number)
	}
	time.Sleep(settle)
	if causes := r.writes(w.token); len(causes) != 1 {
		t.Fatalf("the restarted daemon wrote the owed task for %v, want once", causes)
	}
	if n := r.accepted(w.token); n != 1 {
		t.Fatalf("the restarted daemon accepted %d takes, want the one", n)
	}
}

// A daemon restarted while a claim is stored working with its task taken back — the record a
// compaction's start writes (supervise's compactStart), which a restart in the middle of the
// compaction finds — writes the owed task once at the boot. The worker, whose shim answers the
// restarted daemon nothing of a turn, asks at the boot's revision as the word that its host is
// idle; the take is accepted, and the turn start is held up to the second bound
// (worker_turn_start_timeout_seconds) with no charge and no second take.
func TestTaskValueARestartDuringACompactionWritesOnceAndIsTakenOnce(t *testing.T) {
	r := newBucketRig(t)
	r.cfg.WorkerTakeTimeout, r.cfg.WorkerTurnStartTimeout = 5*time.Minute, 25*time.Minute
	r.o.clock = &heldClock{}
	r.boot()
	w := r.spawn("LEGION-1", "Say hello.")
	w.ready()
	key := string(w.token)
	taken := w.driver.mustTake(t, "the worker takes its task")
	before := r.entry(key)
	r.d.stop()

	compacting := storedClaim(t, r.cfg, w.token)
	if compacting.Pending == nil || compacting.TaskTakeState != supervise.TakeAccepted {
		t.Fatalf("the stopped claim holds %+v with take state %q, want its accepted take", compacting.Pending, compacting.TaskTakeState)
	}
	compacting.State = supervise.StateWorking
	compacting.Pending.ConfirmedAt, compacting.Pending.TakenText = time.Time{}, ""
	putStoredClaim(t, r.cfg, compacting)
	// The compaction aborted the turn the take was to start, so the worker holds no take awaiting
	// its turn: what it remembers is the revision it asked at.
	w.driver.mu.Lock()
	w.driver.accepted = 0
	w.driver.mu.Unlock()

	clock := &heldClock{}
	r.o.clock = clock
	rebindHeldPorts(t, &r.cfg)
	r.boot()
	w.silent = true
	w.reattach()
	r.awaitEntry("the owed task written at the boot", key, func(e jetstream.KeyValueEntry) bool {
		return e != nil && e.Revision() > before.Revision()
	})
	again := w.driver.mustTake(t, "the worker takes the task at the boot's revision")
	if again.Number != taken.Number {
		t.Fatalf("the worker took task %d, want %d", again.Number, taken.Number)
	}
	time.Sleep(settle)
	if causes := r.writes(w.token); !slices.Equal(causes, []string{supervise.TaskValueBoot}) {
		t.Fatalf("the restarted daemon wrote the owed task for %v, want once at the boot", causes)
	}
	if n := r.accepted(w.token); n != 1 {
		t.Fatalf("the restarted daemon accepted %d takes, want the one", n)
	}
	if live, _ := clock.armed(r.cfg.WorkerTurnStartTimeout); live != 1 || r.cfg.WorkerTurnStartTimeout < 10*time.Minute {
		t.Fatalf("%d waits of %s are armed for the take's turn, want the one second bound, never below the compaction timeout", live, r.cfg.WorkerTurnStartTimeout)
	}
	if c := r.d.claim(w.token); c.State != string(supervise.StateIdle) || c.Budgets.PromptFailures != 0 {
		t.Fatalf("while the take's turn is held the claim is %s with %d prompt failures, want idle with none", c.State, c.Budgets.PromptFailures)
	}
	w.driver.mustStartTurn(t)
	testwait.Eventually(t, "the take's turn to start", func() bool { return r.d.claim(w.token).State == string(supervise.StateWorking) })
	if c := r.d.claim(w.token); c.Budgets.PromptFailures != 0 {
		t.Fatalf("the take's turn started with %d prompt failures charged, want none", c.Budgets.PromptFailures)
	}
}

// putStoredClaim writes c, its pending task with it, to the store under cfg, as a daemon that
// stopped before it wrote anything after c leaves it.
func putStoredClaim(t *testing.T, cfg config.Config, c supervise.Claim) {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, cfg.PostgresDSN)
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}
	defer st.Close()
	if c.Pending == nil {
		err = st.PutClaim(ctx, supervise.StoredClaim(c))
	} else {
		err = st.PutClaimAndDelivery(ctx, supervise.StoredClaim(c), *c.Pending)
	}
	if err != nil {
		t.Fatalf("write %s: %v", c.Token, err)
	}
}

// The line a daemon whose configuration names no NATS says at boot and `legion start
// --check-config` prints, written out here; a configuration that names NATS has none.
func TestNoNATSLineIsSaidOnlyForAConfigurationThatNamesNoNATS(t *testing.T) {
	const want = "nats: nats_urls names no NATS, so this daemon has no task bucket (legion_tasks) to hand a worker its task through: " +
		"it launches no worker, and refuses every spawn, retry and resume of one until nats_urls names Envoy's NATS"
	if got := NoNATSLine(config.Config{}); got != want {
		t.Fatalf("NoNATSLine of a configuration with no nats_urls = %q, want %q", got, want)
	}
	if got := NoNATSLine(config.Config{NatsURLs: []string{"nats://127.0.0.1:4222"}}); got != "" {
		t.Fatalf("NoNATSLine of a configuration with nats_urls = %q, want none", got)
	}
}

// A daemon whose configuration names no NATS boots, says so once at boot, and launches no worker:
// the operator's spawn is refused naming the missing NATS and stores no claim, and a claim an
// earlier boot with a bucket stored is refused its resume for the same reason, with nothing
// launched.
func TestNoNATSLaunchesNoWorker(t *testing.T) {
	cfg := testConfig(t)
	rt := fake.NewRuntime()
	record := &built{}
	withBucket := fakeRuntime(rt, record)
	d := startDaemon(t, cfg, withBucket)
	token := d.spawn(architect())
	launch := lastLaunch(t, rt, token)
	dialShim(t, record.address, launch.BootToken)
	registerReady(t, d, launch)
	if status, body := d.request(http.MethodPost, "/legion/v1/operator/claims/"+string(token)+"/suspend", nil, true); status != http.StatusOK {
		t.Fatalf("suspend = %d; body %s", status, body)
	}
	testwait.Eventually(t, "the claim suspended", func() bool { return d.claim(token).State == string(supervise.StateSuspended) })
	d.stop()

	rebindHeldPorts(t, &cfg)
	noBucket := fakeRuntime(rt, record)
	noBucket.taskValues = nil
	logs := &syncBuffer{}
	d = startDaemonLogging(t, cfg, noBucket, slog.New(slog.NewJSONHandler(logs, nil)))
	said := loggedLines(t, logs.String(), NoNATSLine(cfg))
	if len(said) != 1 || said[0]["msg"] != NoNATSLine(cfg) || said[0]["level"] != "WARN" {
		t.Fatalf("the boot logged %v, want the no-NATS line once at Warn", said)
	}
	launches := len(rt.CallsOf("Spawn")) + len(rt.CallsOf("Resume"))

	spawn := architect()
	spawn.Issue, spawn.Tree = "LEGION-2", "LEGION-2"
	status, body := d.request(http.MethodPost, "/legion/v1/operator/claims", spawn, true)
	if status != http.StatusConflict || !strings.Contains(string(body), "nats_urls names no NATS") {
		t.Fatalf("the spawn = %d %s, want 409 naming the missing NATS", status, body)
	}
	for _, c := range d.claims() {
		if c.Token != token {
			t.Fatalf("the refused spawn stored claim %s", c.Token)
		}
	}
	status, body = d.request(http.MethodPost, "/legion/v1/operator/claims/"+string(token)+"/resume", nil, true)
	if status != http.StatusConflict || !strings.Contains(string(body), "nats_urls names no NATS") {
		t.Fatalf("the resume = %d %s, want 409 naming the missing NATS", status, body)
	}
	if got := len(rt.CallsOf("Spawn")) + len(rt.CallsOf("Resume")); got != launches {
		t.Fatalf("the daemon with no NATS launched %d workers, want none", got-launches)
	}
	if state := d.claim(token).State; state != string(supervise.StateSuspended) {
		t.Fatalf("the refused resume left the claim %s, want it suspended as it was", state)
	}
}

// taskBucketRefusalServer is a NATS server, in a container (testnats.StartNkeyAuthorizedUsers, so
// CI runs this with its Docker), whose daemon user holds the daemon's grant less missing and whose
// session user holds what a session's does of NATS and nothing of the task bucket, with the
// notification stream an admin created on it.
func taskBucketRefusalServer(t *testing.T, missing string) (uri, daemonSeed, sessionSeed string) {
	t.Helper()
	grant := daemonpermissions.Grant()
	held := len(grant.Publish)
	grant.Publish = slices.DeleteFunc(grant.Publish, func(subject string) bool { return subject == missing })
	if len(grant.Publish) != held-1 {
		t.Fatalf("the daemon grant does not contain %s: %v", missing, grant.Publish)
	}
	daemonSeed, daemonUser := testnats.User(t)
	sessionSeed, sessionUser := testnats.User(t)
	adminSeed, adminUser := testnats.User(t)
	session := daemonpermissions.Permissions{Publish: []string{"$JS.API.INFO"}, Subscribe: []string{nats.InboxPrefix + ">"}}
	_, uri = testnats.StartNkeyAuthorizedUsers(t, testnats.Image,
		testnats.NkeyUser{Public: daemonUser, Permissions: grantPermissions(t, grant)},
		testnats.NkeyUser{Public: sessionUser, Permissions: grantPermissions(t, session)},
		testnats.NkeyUser{Public: adminUser})
	admin, err := natsauth.Connect([]string{uri}, adminSeed, t.Name()+" admin", nats.Timeout(5*time.Second))
	if err != nil {
		t.Fatalf("connect the admin user: %v", err)
	}
	t.Cleanup(admin.Close)
	js, err := jetstream.New(admin)
	if err != nil {
		t.Fatal(err)
	}
	createDaemonNotificationStream(t, js)
	return uri, daemonSeed, sessionSeed
}

// taskBucketRefusals are the two refusals of the bucket a boot and `legion probe-boot` name: the
// daemon's own user missing the bucket's create, and a daemon connected as the pane seed's
// session user, which holds no grant on the bucket and is told to take a user of its own.
func taskBucketRefusals(t *testing.T, check func(t *testing.T, cfg func(config.Config) config.Config, want []string)) {
	const missing = "$JS.API.STREAM.CREATE." + legiontasks.Stream
	uri, daemonSeed, sessionSeed := taskBucketRefusalServer(t, missing)
	t.Run("the daemon's own user lacks the bucket's create", func(t *testing.T) {
		check(t, func(cfg config.Config) config.Config {
			cfg.NatsURLs = []string{uri}
			cfg.NatsDaemonNkeySeedFile = testnats.SeedFile(t, daemonSeed+"\n")
			return cfg
		}, []string{legiontasks.Bucket, missing})
	})
	t.Run("the daemon connects as the pane seed's session user", func(t *testing.T) {
		check(t, func(cfg config.Config) config.Config {
			cfg.NatsURLs = []string{uri}
			cfg.NatsNkeySeedFile = testnats.SeedFile(t, sessionSeed+"\n")
			return cfg
		}, []string{legiontasks.Bucket, "$JS.API.STREAM.INFO." + legiontasks.Stream, natsauth.SeedFileVariable, natsauth.DaemonSeedFileKey})
	})
}

// A daemon that cannot open or create the task bucket refuses to boot, naming the bucket and the
// subject its NATS user is refused, and records no boot: no wait gives a user a grant it lacks.
// The refused-grant server is a container, so CI's Docker runs this.
func TestTaskBucketRefusedRefusesBoot(t *testing.T) {
	taskBucketRefusals(t, func(t *testing.T, configure func(config.Config) config.Config, want []string) {
		cfg := configure(testConfig(t))
		o := fakeRuntime(fake.NewRuntime(), &built{})
		o.taskValues = nil
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		err := run(ctx, cfg, quietLogger(), o)
		for _, w := range want {
			if err == nil || !strings.Contains(err.Error(), w) {
				t.Errorf("run = %v, want a refusal naming %s", err, w)
			}
		}
		if natsauth.Unreachable(err) {
			t.Errorf("the refusal %v reads as an unreachable NATS, which the boot would wait out", err)
		}
		if count, _ := boots(t, cfg); count != 0 {
			t.Errorf("the refused daemon recorded %d boots, want none", count)
		}
	})
}
