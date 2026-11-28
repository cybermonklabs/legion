package daemon

//ci:reads packages/contracts/fixtures/hash-gate/** hashGateFixture

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// The root of TestHashGateSeedTakesDispatchVerdict, gated on its spec, and the document the
// hash-gate fixtures name.
const (
	seedRoot = "LEGION-750"
	seedDoc  = "00000000-0000-4000-8000-000000000001"
	// fixtureDigest is artifact.json's content_sha256 and current_sha256: the approved version's
	// digest, and the latest version's, which matches it.
	fixtureDigest = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
)

// seedReviewer is who gave the review a read shows: the fixtures name nobody (approval.by).
var seedReviewer = dispatch.Actor{Kind: "user", ID: "person@example.com"}

// hashGateFixture is the hash-gate fixture name as packages/contracts/fixtures/hash-gate holds
// it, the wire contract of Dispatch's hash gate (R1 of dispatch://INFRA-6065).
func hashGateFixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "contracts", "fixtures", "hash-gate", name))
	if err != nil {
		t.Fatalf("read the hash-gate fixture %s: %v", name, err)
	}
	return raw
}

// hashGateBody is the GET body of the hash-gate fixture name (hashGateFixture), on the issue key,
// with its approval changed by change: the fixtures hold id, name and approval alone.
func hashGateBody(t *testing.T, name, key string, change func(approval map[string]any)) []byte {
	t.Helper()
	raw := hashGateFixture(t, name)
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode the hash-gate fixture %s: %v", name, err)
	}
	approval, ok := body["approval"].(map[string]any)
	if !ok {
		t.Fatalf("the hash-gate fixture %s holds no approval", name)
	}
	body["issue_key"] = key
	if change != nil {
		change(approval)
	}
	changed, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("encode the GET body of %s: %v", name, err)
	}
	return changed
}

// reviewed is change that marks the review the read shows: seq, its time and the person.
func reviewed(seq int64, at time.Time, change func(approval map[string]any)) func(map[string]any) {
	return func(approval map[string]any) {
		approval["seq"], approval["at"] = seq, at.Format(time.RFC3339Nano)
		approval["by"] = map[string]any{"kind": seedReviewer.Kind, "id": seedReviewer.ID}
		if change != nil {
			change(approval)
		}
	}
}

// versions is change that sets the approved version and the latest one.
func versions(approved, latest int) func(map[string]any) {
	return func(approval map[string]any) {
		approval["version"], approval["latest_version"] = approved, latest
	}
}

// seedDispatch is the real client over a server that answers each document's GET with the body
// bodies holds for it (answer), and the issues and document versions the cases read beside it.
type seedDispatch struct {
	*dispatch.HTTPClient
	mu       sync.Mutex
	bodies   map[string][]byte
	issues   map[string]dispatch.Issue
	versions map[string]string
}

func newSeedDispatch(t *testing.T) *seedDispatch {
	t.Helper()
	d := &seedDispatch{bodies: map[string][]byte{}, issues: map[string]dispatch.Issue{}, versions: map[string]string{}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		body, ok := d.bodies[strings.TrimPrefix(r.URL.Path, "/api/v1/artifacts/")]
		d.mu.Unlock()
		if r.Method != http.MethodGet || !ok {
			t.Errorf("request = %s %s, want the GET of a document the case serves", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if _, err := w.Write(body); err != nil {
			t.Errorf("write the GET body: %v", err)
		}
	}))
	t.Cleanup(server.Close)
	d.HTTPClient = dispatch.New(server.URL, dispatch.BearerTransport("dispatch-token", nil))
	return d
}

// answer makes the server answer the GET of artifact with body.
func (d *seedDispatch) answer(artifact string, body []byte) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.bodies[artifact] = body
}

func (d *seedDispatch) GetIssue(_ context.Context, key string) (dispatch.Issue, error) {
	issue, ok := d.issues[key]
	if !ok {
		return dispatch.Issue{}, fmt.Errorf("no issue %s", key)
	}
	return issue, nil
}

func (d *seedDispatch) DocumentVersion(_ context.Context, artifact string, version int) (string, error) {
	markdown, ok := d.versions[fmt.Sprintf("%s@%d", artifact, version)]
	if !ok {
		return "", fmt.Errorf("no version %d of %s", version, artifact)
	}
	return markdown, nil
}

// seedRig is the root on a database of its own, the workflow's engine over it, and the outbox
// that runs gate seeds against the fake Dispatch at clock, each fact it applies recorded too.
type seedRig struct {
	t        *testing.T
	ctx      context.Context
	pool     *store.Pool
	records  record.Store
	client   *seedDispatch
	runner   *outbox
	recorded *outboxFactHandler
	clock    time.Time
}

func newSeedRig(t *testing.T) *seedRig {
	t.Helper()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	r := &seedRig{t: t, ctx: context.Background(), pool: pool, records: records, client: newSeedDispatch(t), recorded: &outboxFactHandler{},
		clock: time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)}
	engine := workflow.New(records, workflow.Config{Project: "LEGION", Repositories: testRepositories, Steps: config.DesignGateRootIssues.Steps()}, quietLogger())
	r.runner = &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", records: records, dispatch: r.client,
		handlers: []intake.Handler{engine, r.recorded}, now: func() time.Time { return r.clock }}
	r.issue(seedRoot, "in_progress")
	return r
}

// issue records key, a root of its own tree in implementing, at status, and has Dispatch show it
// at status, with no last_seq: a case that reads a sequence floor gives it its own.
func (r *seedRig) issue(key, status string) {
	r.t.Helper()
	at := phase.Implementing
	if record.OutOfWorkflow(status) {
		at = phase.Admitted
	}
	putOutboxIssue(r.t, r.pool, r.records, bootIssue(key, key, status, at))
	r.client.issues[key] = dispatch.Issue{Key: key, Status: status}
}

// since has Dispatch show key's last_seq at seq, the sequence floor the seed's read of the issue
// answers.
func (r *seedRig) since(key string, seq int64) {
	issue := r.client.issues[key]
	issue.LastSeq = seq
	r.client.issues[key] = issue
}

// put records gate.
func (r *seedRig) put(gate record.DesignGate) { putOutboxGate(r.t, r.pool, r.records, gate) }

// gate is key's spec gate as the store reads it.
func (r *seedRig) gate(key string) record.DesignGate {
	r.t.Helper()
	var gate *record.DesignGate
	if err := store.Transact(r.ctx, r.pool, func(ctx context.Context, tx pgx.Tx) error {
		gates, err := r.records.GatesOf(ctx, tx, key)
		gate = record.SpecGate(gates)
		return err
	}); err != nil || gate == nil {
		r.t.Fatalf("read the spec gate of %s: %v, %v", key, gate, err)
	}
	return *gate
}

// seed runs the gate seed of key's spec step queued at version.
func (r *seedRig) seed(key string, version int) {
	r.t.Helper()
	r.run(key, record.GateSeed{ArtifactID: seedDoc, Step: record.DesignStepSpec, Version: version, Generation: 1})
}

// run executes payload as a row of key.
func (r *seedRig) run(key string, payload record.OutboxPayload) {
	r.t.Helper()
	if err := r.runner.execute(r.ctx, mustOutboxRow(r.t, key, payload, r.clock)); err != nil {
		r.t.Fatalf("execute %T of %s: %v", payload, key, err)
	}
}

// processed is the id of every fact the outbox applied whose id begins with prefix, sorted.
func (r *seedRig) processed(prefix string) []string {
	r.t.Helper()
	rows, err := r.pool.Query(r.ctx, `select event_id from processed_events where source = 'outbox' and event_id like $1`, prefix+"%")
	if err != nil {
		r.t.Fatalf("read the processed facts: %v", err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		r.t.Fatalf("read the processed facts: %v", err)
	}
	slices.Sort(ids)
	return ids
}

// readFacts is every DispatchArtifact the outbox applied.
func (r *seedRig) readFacts() []intake.DispatchArtifact {
	var facts []intake.DispatchArtifact
	for _, fact := range r.recorded.facts() {
		if read, ok := fact.(intake.DispatchArtifact); ok {
			facts = append(facts, read)
		}
	}
	return facts
}

// gateSeedsOf is every gate_seed row the outbox holds for issue, oldest first, decoded.
func gateSeedsOf(t *testing.T, pool *store.Pool, issue string) []record.GateSeed {
	t.Helper()
	rows, err := pool.Query(context.Background(), `select payload from outbox where kind = $1 and issue = $2 order by id`, string(record.OutboxKindGateSeed), issue)
	if err != nil {
		t.Fatalf("read the gate seeds of %s: %v", issue, err)
	}
	payloads, err := pgx.CollectRows(rows, pgx.RowTo[[]byte])
	if err != nil {
		t.Fatalf("read the gate seeds of %s: %v", issue, err)
	}
	seeds := make([]record.GateSeed, 0, len(payloads))
	for _, payload := range payloads {
		decoded, err := record.DecodeOutboxPayload(record.OutboxRow{Kind: record.OutboxKindGateSeed, Payload: payload})
		if err != nil {
			t.Fatalf("decode a gate seed of %s: %v", issue, err)
		}
		seeds = append(seeds, decoded.(record.GateSeed))
	}
	return seeds
}

// boot runs the boot's pass over the recorded gates (queueRecordedGatesPublish) under the
// deployment's steps.
func (r *seedRig) boot() {
	r.t.Helper()
	boot := &workflowRuntime{pool: r.pool, records: r.records, dispatchProject: "LEGION", gatedSteps: config.DesignGateRootIssues.Steps(), log: quietLogger()}
	if err := boot.queueRecordedGatesPublish(r.ctx); err != nil {
		r.t.Fatalf("the boot's pass: %v", err)
	}
}

// The gate seed reads Dispatch's verdict where Dispatch sends one (R3 and R5 of
// dispatch://INFRA-6065, "A read"), from the hash-gate fixtures' GET bodies: it records the
// version the person approved (approval.version) with the review's mark, never the latest version,
// and gives the verdict at the latest version, marked as register_gate marks its own: the issue's
// last_seq, read before the approval, and the seed's own time taken before the read, each raised
// to the review's where Dispatch's runs ahead; a Dispatch that gives no sequence leaves the time
// alone. A read of the issue that fails holds the seed on its backoff. A row in version mode whose
// approval of an older version Dispatch reports standing opens on it and stays open on a read
// again, a read after the issue's last_seq moved on among them, and so does a row an older daemon
// moved after the boot's seed reads it. A read whose only change is the digest Dispatch filled is
// a fact of its own, and so are the gate-above and pending-basis reads; a read with no digest_owed
// puts the row back in version mode. A read that shows the approval not standing at the version
// the seed was queued at gives the verdict false. The re-parent read names the latest version. A
// carried copy's read reaches the copy as its old root's event does, with its verdict's sequence
// the review's, never the copy's own last_seq, and is never reset. The boot seeds every closed step
// in a tree in the workflow once, and the boot catch-up replays an artifact.digest.
func TestHashGateSeedTakesDispatchVerdict(t *testing.T) {
	t.Parallel()
	reviewedAt := time.Date(2026, 11, 27, 11, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name string
		// at is the seed's clock, before or after Dispatch's time of the review.
		at time.Time
		// since is the last_seq the read of the issue answers, and seq the review's sequence the read
		// of the approval shows, which Dispatch leaves out at 0.
		since, seq int64
		verdict    record.EventMark
	}{
		{name: "the seed records the approved version and the verdict at the latest, marked by its read", at: reviewedAt.Add(time.Hour), since: 12, seq: 12,
			verdict: record.EventMark{Seq: 12, At: reviewedAt.Add(time.Hour)}},
		{name: "the verdict's mark is raised to a review Dispatch marked after the read", at: reviewedAt.Add(-time.Minute), since: 12, seq: 12,
			verdict: record.EventMark{Seq: 12, At: reviewedAt}},
		{name: "the verdict's sequence is the issue's last_seq, above the review's", at: reviewedAt.Add(time.Hour), since: 20, seq: 12,
			verdict: record.EventMark{Seq: 20, At: reviewedAt.Add(time.Hour)}},
		{name: "the verdict's sequence is raised to a review committed between the read of the issue and the read of the approval",
			at: reviewedAt.Add(time.Hour), since: 10, seq: 12, verdict: record.EventMark{Seq: 12, At: reviewedAt.Add(time.Hour)}},
		{name: "a Dispatch that answers no last_seq has the verdict's sequence raised to the review's, as register_gate's read does",
			at: reviewedAt.Add(time.Hour), since: 0, seq: 12, verdict: record.EventMark{Seq: 12, At: reviewedAt.Add(time.Hour)}},
		{name: "a Dispatch that answers no last_seq and no review sequence leaves the verdict marked by the read's time alone",
			at: reviewedAt.Add(time.Hour), since: 0, seq: 0, verdict: record.EventMark{At: reviewedAt.Add(time.Hour)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newSeedRig(t)
			r.clock = tc.at
			r.since(seedRoot, tc.since)
			r.put(record.DesignGate{Issue: seedRoot, Step: record.DesignStepSpec, ArtifactID: seedDoc, LatestVersion: 2})
			r.client.answer(seedDoc, hashGateBody(t, "artifact.json", seedRoot, reviewed(tc.seq, reviewedAt, func(approval map[string]any) {
				if tc.seq == 0 {
					delete(approval, "seq")
				}
			})))
			r.seed(seedRoot, 2)

			review := record.EventMark{Seq: tc.seq, At: reviewedAt}
			want := intake.DispatchArtifact{Key: seedRoot, ArtifactID: seedDoc, Kind: intake.DispatchArtifactApproved, Version: 1, Latest: 2,
				Mark: review, Actor: seedReviewer, DigestOwed: new(false), Verdict: new(true), VerdictMark: tc.verdict,
				CurrentSHA256: fixtureDigest}
			if got := r.readFacts(); len(got) != 1 || !reflect.DeepEqual(got[0], want) {
				t.Fatalf("the seed's facts = %+v, want the one read %+v", got, want)
			}
			gate := r.gate(seedRoot)
			if gate.ApprovedVersion == nil || *gate.ApprovedVersion != 1 || gate.ApprovedMark != review || !record.GateOpen(gate) ||
				gate.Verdict == nil || *gate.Verdict != (record.GateVerdict{Version: 2, Current: true, Digest: fixtureDigest, Mark: tc.verdict}) {
				t.Fatalf("the gate = %+v (verdict %+v), want version 1 approved at %v and the verdict true at 2 marked %+v", gate, gate.Verdict, review, tc.verdict)
			}
		})
	}

	t.Run("a failed read of the issue holds the seed on its backoff, and its next run applies the read", func(t *testing.T) {
		t.Parallel()
		r := newSeedRig(t)
		r.put(record.DesignGate{Issue: seedRoot, Step: record.DesignStepSpec, ArtifactID: seedDoc, LatestVersion: 2})
		r.client.answer(seedDoc, hashGateBody(t, "artifact.json", seedRoot, reviewed(12, reviewedAt, nil)))
		shown := r.client.issues[seedRoot]
		delete(r.client.issues, seedRoot)
		row := mustOutboxRow(t, seedRoot, record.GateSeed{ArtifactID: seedDoc, Step: record.DesignStepSpec, Version: 2, Generation: 1}, r.clock)
		if err := r.runner.execute(r.ctx, row); err == nil || !strings.Contains(err.Error(), "read Dispatch issue "+seedRoot) || permanentRefusal(row, err) {
			t.Fatalf("the seed whose read of the issue failed = %v, want that read's error, retried on the row's backoff", err)
		}
		if facts, gate := r.readFacts(), r.gate(seedRoot); len(facts) != 0 || record.GateOpen(gate) {
			t.Fatalf("after the failed read of the issue: facts %+v, gate %+v; want nothing applied, the gate closed", facts, gate)
		}
		r.client.issues[seedRoot] = shown
		r.seed(seedRoot, 2)
		if facts, gate := r.readFacts(), r.gate(seedRoot); len(facts) != 1 || !record.GateOpen(gate) {
			t.Fatalf("after the seed ran again: facts %+v, gate %+v; want the read applied, the gate open", facts, gate)
		}
	})

	t.Run("a version-mode row whose older approval Dispatch reports standing opens and stays open", func(t *testing.T) {
		t.Parallel()
		r := newSeedRig(t)
		approved := record.EventMark{Seq: 10, At: reviewedAt}
		r.put(record.DesignGate{Issue: seedRoot, Step: record.DesignStepSpec, ArtifactID: seedDoc, LatestVersion: 8, ApprovedVersion: new(7), ApprovedMark: approved})
		if record.GateOpen(r.gate(seedRoot)) {
			t.Fatal("the version-mode row at 8 approved at 7 reads open before the seed")
		}
		r.since(seedRoot, 10)
		r.client.answer(seedDoc, hashGateBody(t, "artifact.json", seedRoot, reviewed(10, reviewedAt, versions(7, 8))))
		r.seed(seedRoot, 8)
		if gate := r.gate(seedRoot); !record.GateOpen(gate) || *gate.ApprovedVersion != 7 || gate.Verdict == nil || gate.Verdict.Version != 8 {
			t.Fatalf("the gate after the seed = %+v (verdict %+v), want it open at 8 on the approval of 7", gate, gate.Verdict)
		}
		// The read again, after the issue's last_seq moved on, finds the same review, digest and
		// verdict: the same fact, though its verdict's mark is later.
		r.clock = r.clock.Add(time.Hour)
		r.since(seedRoot, 14)
		r.seed(seedRoot, 8)
		if gate := r.gate(seedRoot); !record.GateOpen(gate) || len(r.readFacts()) != 1 {
			t.Fatalf("the gate after a second read = %+v, facts %d; want it open, the same read applied once", gate, len(r.readFacts()))
		}
	})

	t.Run("a row an older daemon moved opens on the boot's seed", func(t *testing.T) {
		t.Parallel()
		r := newSeedRig(t)
		verdictMark := record.EventMark{Seq: 5, At: reviewedAt.Add(-time.Hour)}
		r.put(record.DesignGate{Issue: seedRoot, Step: record.DesignStepSpec, ArtifactID: seedDoc, LatestVersion: 8, ApprovedVersion: new(8),
			ApprovedMark: verdictMark, Verdict: &record.GateVerdict{Version: 8, Current: true, Mark: verdictMark}, ToldOpen: new(true)})
		// The release before writes an approval of 7, marked after the verdict, and none of the
		// verdict's columns (R-old-4): the row reads in version mode, closed.
		if _, err := r.pool.Exec(r.ctx, `update design_gates set approved_version = 7, approved_seq = 10, approved_at = $2 where issue = $1`,
			seedRoot, reviewedAt); err != nil {
			t.Fatalf("write the older daemon's approval: %v", err)
		}
		if gate := r.gate(seedRoot); gate.Verdict != nil || record.GateOpen(gate) {
			t.Fatalf("the row the older daemon moved = %+v, want it in version mode, closed", gate)
		}
		r.boot()
		seeds := gateSeedsOf(t, r.pool, seedRoot)
		if want := (record.GateSeed{ArtifactID: seedDoc, Step: record.DesignStepSpec, Version: 8, Generation: 1}); len(seeds) != 1 || seeds[0] != want {
			t.Fatalf("the boot's gate seeds = %+v, want %+v", seeds, want)
		}
		r.client.answer(seedDoc, hashGateBody(t, "artifact.json", seedRoot, reviewed(10, reviewedAt, versions(7, 8))))
		r.run(seedRoot, seeds[0])
		if gate := r.gate(seedRoot); !record.GateOpen(gate) || gate.Verdict == nil || gate.Verdict.Version != 8 {
			t.Fatalf("the gate after the boot's seed = %+v (verdict %+v), want it open at 8", gate, gate.Verdict)
		}
	})

	t.Run("a read that is owed, then the read that is filled, applies twice", func(t *testing.T) {
		t.Parallel()
		r := newSeedRig(t)
		r.put(record.DesignGate{Issue: seedRoot, Step: record.DesignStepSpec, ArtifactID: seedDoc, LatestVersion: 1, ApprovedVersion: new(1),
			ApprovedMark: record.EventMark{Seq: 12, At: reviewedAt}})
		r.client.answer(seedDoc, hashGateBody(t, "artifact-owed.json", seedRoot, reviewed(12, reviewedAt, nil)))
		r.seed(seedRoot, 1)
		if gate := r.gate(seedRoot); record.GateOpen(gate) || !record.DigestOwed(gate) || gate.LatestVersion != 2 {
			t.Fatalf("the gate after the owed read = %+v (verdict %+v), want it at 2 owing the digest, closed", gate, gate.Verdict)
		}
		r.client.answer(seedDoc, hashGateBody(t, "artifact.json", seedRoot, reviewed(12, reviewedAt, nil)))
		r.seed(seedRoot, 1)
		ids := r.processed("gate-seed:")
		if len(ids) != 2 || !strings.HasSuffix(ids[0], ":latest:2:digest-owed:false:current:"+fixtureDigest) || !strings.HasSuffix(ids[1], ":latest:2:digest-owed:true") {
			t.Fatalf("the seed's fact ids = %q, want the owed read's and the filled read's, each naming what it found", ids)
		}
		if gate := r.gate(seedRoot); !record.GateOpen(gate) || gate.Verdict == nil || gate.Verdict.Version != 2 || !gate.Verdict.Current {
			t.Fatalf("the gate after the filled read = %+v (verdict %+v), want it open on the verdict at 2", gate, gate.Verdict)
		}
	})

	t.Run("a read with no digest_owed puts a hash-mode row in version mode", func(t *testing.T) {
		t.Parallel()
		r := newSeedRig(t)
		mark := record.EventMark{Seq: 20, At: reviewedAt}
		r.put(record.DesignGate{Issue: seedRoot, Step: record.DesignStepSpec, ArtifactID: seedDoc, LatestVersion: 6, ApprovedVersion: new(6),
			ApprovedMark: mark, Verdict: &record.GateVerdict{Version: 6, Current: true, Mark: mark}, ToldOpen: new(true)})
		// A Dispatch rolled back past the hash gate: version 7 approved, no digest field.
		r.client.answer(seedDoc, hashGateBody(t, "artifact.json", seedRoot, reviewed(30, reviewedAt.Add(time.Minute), func(approval map[string]any) {
			delete(approval, "digest_owed")
			delete(approval, "current_sha256")
			versions(7, 7)(approval)
		})))
		r.seed(seedRoot, 6)
		if gate := r.gate(seedRoot); gate.Verdict != nil || gate.ToldOpen != nil || !record.GateOpen(gate) || gate.LatestVersion != 7 {
			t.Fatalf("the gate after the read = %+v (verdict %+v), want it in version mode, open at 7", gate, gate.Verdict)
		}
	})

	t.Run("a read that shows the approval not standing at the version queued gives the verdict false", func(t *testing.T) {
		t.Parallel()
		r := newSeedRig(t)
		r.put(record.DesignGate{Issue: seedRoot, Step: record.DesignStepSpec, ArtifactID: seedDoc, LatestVersion: 2, ApprovedVersion: new(2),
			ApprovedMark: record.EventMark{Seq: 12, At: reviewedAt}})
		if !record.GateOpen(r.gate(seedRoot)) {
			t.Fatal("the version-mode row approved at its latest version 2 reads closed before the seed")
		}
		// Dispatch reports the approval of version 1 stale at its latest version 2, the version the
		// seed was queued at, with no digest owed: a verdict, false, which seedGateDocument applies,
		// since its early return at the version queued is for a read with no digest_owed alone.
		r.client.answer(seedDoc, hashGateBody(t, "artifact.json", seedRoot, reviewed(12, reviewedAt, func(approval map[string]any) {
			approval["state"] = "stale"
			approval["current_sha256"] = strings.Repeat("c", 64)
		})))
		r.seed(seedRoot, 2)
		gate := r.gate(seedRoot)
		if record.GateOpen(gate) || gate.Verdict == nil || *gate.Verdict != (record.GateVerdict{Version: 2, Current: false, Digest: strings.Repeat("c", 64), Mark: record.EventMark{Seq: 12, At: r.clock}}) {
			t.Fatalf("the gate after the read = %+v (verdict %+v), want it closed on the verdict false at 2", gate, gate.Verdict)
		}
	})

	t.Run("the gate-above read names what it found", func(t *testing.T) {
		t.Parallel()
		r := newSeedRig(t)
		// A gate above every version Dispatch holds: the seed was queued at its version 9.
		r.put(record.DesignGate{Issue: seedRoot, Step: record.DesignStepSpec, ArtifactID: seedDoc, LatestVersion: 9})
		for _, body := range [][]byte{
			hashGateBody(t, "artifact-owed.json", seedRoot, reviewed(12, reviewedAt, versions(7, 8))),
			hashGateBody(t, "artifact.json", seedRoot, reviewed(12, reviewedAt, versions(7, 8))),
			hashGateBody(t, "artifact.json", seedRoot, reviewed(12, reviewedAt, func(approval map[string]any) {
				delete(approval, "digest_owed")
				delete(approval, "current_sha256")
				versions(7, 8)(approval)
			})),
		} {
			r.client.answer(seedDoc, body)
			r.seed(seedRoot, 9)
		}
		review := record.EventMark{Seq: 12, At: reviewedAt}.ID()
		approvedRead := fmt.Sprintf("gate-above:%s:1:spec:%s:9:approved:7:%d:latest:8", seedRoot, seedDoc, review)
		want := []string{
			approvedRead + ":digest-owed:false:current:" + fixtureDigest,
			approvedRead + ":digest-owed:true",
			// A read of a Dispatch that sends no digest field keeps today's id.
			fmt.Sprintf("gate-above:%s:1:spec:%s:9:version:8:%d", seedRoot, seedDoc, record.EventMark{}.ID()),
		}
		slices.Sort(want)
		if got := r.processed("gate-above:"); !slices.Equal(got, want) {
			t.Fatalf("the gate-above fact ids = %q, want %q", got, want)
		}
	})

	t.Run("the pending-basis read names what it found", func(t *testing.T) {
		t.Parallel()
		r := newSeedRig(t)
		r.runner.handlers = []intake.Handler{r.recorded}
		const ancestor, basisDoc = "LEGION-100", "artifact-100"
		r.client.issues[ancestor] = dispatch.Issue{Key: ancestor, Status: "in_progress"}
		registered := time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC)
		pending := record.PendingBasisSeed{Step: record.DesignStepSpec, ArtifactID: seedDoc, BasisArtifactID: basisDoc, BasisVersion: 1, Ask: "ask-1",
			Generation: 1, RegisteredAt: registered}
		for _, fixture := range []string{"artifact-owed.json", "artifact.json"} {
			r.client.answer(basisDoc, hashGateBody(t, fixture, ancestor, reviewed(12, reviewedAt, nil)))
			r.run(seedRoot, pending)
		}
		base := fmt.Sprintf("pending-basis:%s:1:spec:ask-1:%d:%s:1:approved:2:1:%d:%s::in_progress:false::", seedRoot, registered.UnixMicro(), basisDoc,
			record.EventMark{Seq: 12, At: reviewedAt}.ID(), seedReviewer.Name())
		want := []string{base + ":digest-owed:false:current:" + fixtureDigest, base + ":digest-owed:true"}
		if got := r.processed("pending-basis:"); !slices.Equal(got, want) {
			t.Fatalf("the pending-basis fact ids = %q, want %q", got, want)
		}
	})

	t.Run("the re-parent read names the latest version a person's approval stands for", func(t *testing.T) {
		t.Parallel()
		const parent, parentDoc = "LEGION-760", "artifact-760"
		for _, tc := range []struct {
			name   string
			change func(map[string]any)
			want   int
		}{
			{name: "standing below the latest", change: nil, want: 2},
			{name: "with no digest_owed", change: func(approval map[string]any) { delete(approval, "digest_owed") }, want: 0},
		} {
			r := newSeedRig(t)
			r.client.issues[parent] = dispatch.Issue{Key: parent, Status: "in_progress", Labels: []string{"legion"}, PrimaryArtifactID: parentDoc}
			r.client.versions[parentDoc+"@2"] = ":::requirement{#r1}\n**R1.** The first requirement.\n:::\n"
			r.client.versions[parentDoc+"@1"] = "No requirement here.\n"
			r.client.answer(parentDoc, hashGateBody(t, "artifact.json", parent, reviewed(12, reviewedAt, tc.change)))
			if got, err := r.runner.reparentBasisVersion(r.ctx, parent); err != nil || got != tc.want {
				t.Fatalf("%s: reparentBasisVersion = %d, %v; want %d", tc.name, got, err, tc.want)
			}
		}
	})

	t.Run("a carried copy's read reaches the copy as its old root's event does", func(t *testing.T) {
		t.Parallel()
		const carriedCopy, oldRoot = "LEGION-751", "LEGION-752"
		for _, tc := range []struct {
			name   string
			gate   record.DesignGate
			change func(map[string]any)
			seedAt int
			open   bool
			latest int
		}{
			{name: "a closed copy opens on the verdict that the approval stands", seedAt: 2, open: true, latest: 2,
				gate: record.DesignGate{LatestVersion: 2, ApprovedVersion: new(1)}},
			{name: "an open copy stays open on the verdict that it does not", seedAt: 2, open: true, latest: 2,
				gate: record.DesignGate{LatestVersion: 2, ApprovedVersion: new(2)},
				change: func(approval map[string]any) {
					approval["state"] = "stale"
					versions(1, 3)(approval)
				}},
			{name: "a copy above the old root's version is not reset", seedAt: 5, latest: 5,
				gate: record.DesignGate{LatestVersion: 5, ApprovedVersion: new(4)}},
		} {
			r := newSeedRig(t)
			r.issue(carriedCopy, "in_progress")
			// The copy's own last_seq, which the seed reads, is no place in the old root's log: the
			// read's verdict takes the review's sequence, 12, and not 40.
			r.since(carriedCopy, 40)
			gate := tc.gate
			gate.Issue, gate.Step, gate.ArtifactID, gate.Carried = carriedCopy, record.DesignStepSpec, seedDoc, true
			r.put(gate)
			r.client.answer(seedDoc, hashGateBody(t, "artifact.json", oldRoot, reviewed(12, reviewedAt, tc.change)))
			r.seed(carriedCopy, tc.seedAt)
			got := r.gate(carriedCopy)
			if record.GateOpen(got) != tc.open || !got.Carried || got.LatestVersion != tc.latest {
				t.Fatalf("%s: the copy = %+v, want open %t at %d, still carried", tc.name, got, tc.open, tc.latest)
			}
			if facts := r.readFacts(); len(facts) != 1 || facts[0].Key != oldRoot || facts[0].VerdictMark != (record.EventMark{Seq: 12, At: r.clock}) {
				t.Fatalf("%s: the seed's facts = %+v, want one keyed by the old root %s, its verdict marked at sequence 12 and %s", tc.name, facts, oldRoot, r.clock)
			}
			if above := r.processed("gate-above:"); len(above) != 0 {
				t.Fatalf("%s: gate-above facts = %q, want none for a copy", tc.name, above)
			}
		}
	})

	t.Run("the boot seeds every closed step in a tree in the workflow once", func(t *testing.T) {
		t.Parallel()
		r := newSeedRig(t)
		const (
			open, carried, pending, parked = "LEGION-753", "LEGION-754", "LEGION-755", "LEGION-756"
		)
		closed := record.DesignGate{Step: record.DesignStepSpec, ArtifactID: seedDoc, LatestVersion: 3, ApprovedVersion: new(2)}
		r.put(withIssue(closed, seedRoot))
		r.issue(open, "in_progress")
		opened := withIssue(closed, open)
		opened.ApprovedVersion = new(3)
		r.put(opened)
		r.issue(carried, "in_progress")
		copied := withIssue(closed, carried)
		copied.Carried = true
		r.put(copied)
		r.issue(pending, "in_progress")
		r.put(record.PendOnBasis(withIssue(closed, pending), record.GateBasis{Issue: "LEGION-100", ArtifactID: "artifact-100", Version: 5,
			Serves: []string{"dispatch://LEGION-100/spec#b-p2@v5"}, Reason: "The checks serve P2.", Session: "ses-architect",
			RegisteredAt: time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC)}, "ask-1"))
		r.issue(parked, "icebox")
		r.put(withIssue(closed, parked))

		want := map[string][]record.GateSeed{}
		for _, key := range []string{seedRoot, carried, pending} {
			want[key] = []record.GateSeed{{ArtifactID: "doc-" + key, Step: record.DesignStepSpec, Version: 3, Generation: 1}}
		}
		for boot := 1; boot <= 2; boot++ {
			r.boot()
			got := map[string][]record.GateSeed{}
			for _, key := range []string{seedRoot, open, carried, pending, parked} {
				if seeds := gateSeedsOf(t, r.pool, key); len(seeds) > 0 {
					got[key] = seeds
				}
			}
			if fmt.Sprint(got) != fmt.Sprint(want) {
				t.Fatalf("boot %d: gate seeds = %+v, want one for each closed step in the workflow %+v", boot, got, want)
			}
		}
	})

	t.Run("the boot catch-up replays an artifact.digest of a root's document", func(t *testing.T) {
		t.Parallel()
		const root = "CAPTURE-100"
		events := []json.RawMessage{
			hashGateEvent(t, "artifact-version.json", root, 19),
			hashGateEvent(t, "artifact-digest.json", root, 20),
		}
		pool, w := catchUpRig(t, &awaitingDispatch{issue: dispatch.Issue{Key: root, LastSeq: 20}, events: events}, 1)
		seedCatchUp(t, pool, func(ctx context.Context, tx pgx.Tx, records *record.Postgres) error {
			if err := records.PutIssue(ctx, tx, captureRoot(phase.AwaitingPerson, &record.Awaiting{GateVersion: 1, Artifact: seedDoc, Since: record.EventMark{Seq: 10}})); err != nil {
				return err
			}
			if err := openTreeTx(ctx, tx, "CAPTURE", root); err != nil {
				return err
			}
			return records.PutGate(ctx, tx, record.DesignGate{Issue: root, Step: record.DesignStepSpec, ArtifactID: seedDoc, LatestVersion: 1})
		})
		if err := bootCatchUp(t, w); err != nil {
			t.Fatalf("catch up the root: %v", err)
		}
		seedCatchUp(t, pool, func(ctx context.Context, tx pgx.Tx, records *record.Postgres) error {
			gates, err := records.GatesOf(ctx, tx, root)
			if err != nil {
				return err
			}
			gate := record.SpecGate(gates)
			if gate == nil || gate.LatestVersion != 2 || gate.Verdict == nil || gate.Verdict.Version != 2 || !gate.Verdict.Current || !record.GateOpen(*gate) {
				t.Errorf("the gate after the catch-up = %+v, want it open on the digest's verdict at version 2", gate)
			}
			return nil
		})
	})
}

// withIssue is gate as issue's, on a document of issue's own (doc-<issue>).
func withIssue(gate record.DesignGate, issue string) record.DesignGate {
	gate.Issue, gate.ArtifactID = issue, "doc-"+issue
	return gate
}

// hashGateEvent is the hash-gate fixture name, an event's type and payload, in the envelope
// Dispatch's events route serves on key at seq, given by Dispatch's fill.
func hashGateEvent(t *testing.T, name, key string, seq int64) json.RawMessage {
	t.Helper()
	raw := hashGateFixture(t, name)
	var event struct {
		Type    string         `json:"type"`
		Payload map[string]any `json:"payload"`
	}
	if err := json.Unmarshal(raw, &event); err != nil {
		t.Fatalf("decode the hash-gate fixture %s: %v", name, err)
	}
	return issueEvent(key, seq, event.Type, settler, event.Payload)
}
