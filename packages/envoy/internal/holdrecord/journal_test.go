package holdrecord_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sjawhar/envoy/internal/holdrecord"
	"github.com/sjawhar/envoy/internal/holdrecord/holdrecordtest"
	"github.com/sjawhar/envoy/natsgrant"
)

// rig is the broker's hold routes over a Postgres schema of the row's own (holdrecordtest).
type rig struct{ *holdrecordtest.Broker }

func newRig(t *testing.T) rig { return rig{holdrecordtest.Start(t)} }

// client is the operator's client of the rig, as an operator process makes one.
func (r rig) client(t *testing.T) *holdrecord.Client { return r.Client(t) }

// once sets a hook that acts on the first request match accepts, and on no other: a write the
// hook makes through the rig passes it, since the hook has fired by then.
func (r rig) once(match func(*http.Request) bool, act func(http.ResponseWriter, *http.Request) bool) *atomic.Bool {
	fired := &atomic.Bool{}
	r.Intercept(func(w http.ResponseWriter, req *http.Request) bool {
		if !match(req) || !fired.CompareAndSwap(false, true) {
			return false
		}
		return act(w, req)
	})
	return fired
}

func recordPut(req *http.Request) bool {
	return req.Method == http.MethodPut && req.URL.Path == "/v1/nats-hold/records"
}

// lose ends the request's connection without an answer, as a process killed while it waited
// would see it; served first, the routes commit the write and only the answer is lost.
func lose(served http.Handler) func(http.ResponseWriter, *http.Request) bool {
	return func(w http.ResponseWriter, req *http.Request) bool {
		if served != nil {
			served.ServeHTTP(httptest.NewRecorder(), req)
		}
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			_ = conn.Close()
		}
		return true
	}
}

// row reads the hold row, restoring the first one, released, when there is none.
func row(t *testing.T, c *holdrecord.Client) holdrecord.Row {
	t.Helper()
	ctx := context.Background()
	got, err := c.Row(ctx)
	var refusal *holdrecord.RouteError
	if errors.As(err, &refusal) && refusal.Code == "no_row" {
		got, err = c.RestoreRow(ctx, 1, natsgrant.OperatorPhaseReleased, "first-row", "the first row")
	}
	if err != nil {
		t.Fatalf("read the hold row: %v", err)
	}
	return got
}

// hold runs a workflow's start as the operator does: the row from released to prepare under
// holdID, the journal started with step1, hold set admitted with the row prepare, the row moved to
// held and hold set's done entry. It returns the held row.
func hold(t *testing.T, c *holdrecord.Client, holdID string, w holdrecord.Workflow, step1 map[string]string) holdrecord.Row {
	t.Helper()
	held := prepare(t, c, holdID, w, step1)
	if err := c.AppendDone(context.Background(), holdID, holdrecord.CommandSet, nil); err != nil {
		t.Fatalf("hold set's done entry: %v", err)
	}
	return held
}

// prepare is hold up to hold set's done entry: hold set admitted and the row held, its intent
// pending.
func prepare(t *testing.T, c *holdrecord.Client, holdID string, w holdrecord.Workflow, step1 map[string]string) holdrecord.Row {
	t.Helper()
	ctx := context.Background()
	current := row(t, c)
	if current.Phase != natsgrant.OperatorPhaseReleased {
		t.Fatalf("the row is %s, not released, before hold %s", current.Phase, holdID)
	}
	prepared, err := c.MoveRow(ctx, current.Gen, natsgrant.OperatorPhasePrepare, holdID, "test hold")
	if err != nil {
		t.Fatalf("move the row to prepare: %v", err)
	}
	if err := c.StartJournal(ctx, holdID, w, step1); err != nil {
		t.Fatalf("start the journal: %v", err)
	}
	if _, err := c.Admit(ctx, holdID, holdrecord.Request{Command: holdrecord.CommandSet}, nil); err != nil {
		t.Fatalf("admit hold set with the row prepare: %v", err)
	}
	held, err := c.MoveRow(ctx, prepared.Gen, natsgrant.OperatorPhaseHeld, holdID, "test hold")
	if err != nil {
		t.Fatalf("move the row to held: %v", err)
	}
	return held
}

// release ends a hold as hold release does: its admission, the row's move to released, and its
// done entry.
func release(t *testing.T, c *holdrecord.Client, holdID string) {
	t.Helper()
	ctx := context.Background()
	if _, err := c.Admit(ctx, holdID, holdrecord.Request{Command: holdrecord.CommandRelease}, nil); err != nil {
		t.Fatalf("admit hold release: %v", err)
	}
	current := row(t, c)
	if _, err := c.MoveRow(ctx, current.Gen, natsgrant.OperatorPhaseReleased, holdID, "released"); err != nil {
		t.Fatalf("release the row: %v", err)
	}
	if err := c.AppendDone(ctx, holdID, holdrecord.CommandRelease, nil); err != nil {
		t.Fatalf("hold release's done entry: %v", err)
	}
}

// run admits a command and writes its done entry.
func run(t *testing.T, c *holdrecord.Client, holdID string, req holdrecord.Request, inputs, outputs map[string]string) {
	t.Helper()
	ctx := context.Background()
	if _, err := c.Admit(ctx, holdID, req, inputs); err != nil {
		t.Fatalf("admit %s: %v", req.Command, err)
	}
	if err := c.AppendDone(ctx, holdID, req.Command, outputs); err != nil {
		t.Fatalf("%s's done entry: %v", req.Command, err)
	}
}

func cmd(command string) holdrecord.Request { return holdrecord.Request{Command: command} }

func journal(t *testing.T, c *holdrecord.Client, holdID string) (holdrecord.Done, uint64) {
	t.Helper()
	d, gen, err := c.Journal(context.Background(), holdID)
	if err != nil {
		t.Fatalf("read the journal: %v", err)
	}
	return d, gen
}

// refused fails t unless err is target, naming what.
func refused(t *testing.T, step string, err, target error, what string) {
	t.Helper()
	if !errors.Is(err, target) || !strings.Contains(err.Error(), what) {
		t.Fatalf("%s answered %v, want %v naming %q", step, err, target, what)
	}
}

// last is the journal's latest entry.
func last(t *testing.T, d holdrecord.Done) holdrecord.Operation {
	t.Helper()
	if len(d.Ops) == 0 {
		t.Fatal("the journal holds no entry")
	}
	return d.Ops[len(d.Ops)-1]
}

var twoDisks = map[string]string{"step1_disk_id.A": "1", "step1_disk_id.B": "2"}

// The hold's client and its journal on the broker's own routes over Postgres: every command is
// admitted and journaled as the hold's design states, every refusal is the one it names, and every
// answer is verified. Each row runs on a schema of its own.
func TestTheHoldJournal(t *testing.T) {
	ctx := context.Background()

	t.Run("the route methods round-trip and verify", func(t *testing.T) {
		r := newRig(t)
		c := r.client(t)
		first := row(t, c)
		if first.Phase != natsgrant.OperatorPhaseReleased || first.Gen == 0 || first.SignedAt.IsZero() {
			t.Fatalf("the restored first row is %+v", first)
		}
		prepared, err := c.MoveRow(ctx, first.Gen, natsgrant.OperatorPhasePrepare, "h-routes", "routes")
		if err != nil || prepared.Gen != first.Gen+1 || prepared.Phase != natsgrant.OperatorPhasePrepare || prepared.HoldID != "h-routes" {
			t.Fatalf("MoveRow to prepare = %+v, %v", prepared, err)
		}
		cohort := json.RawMessage(`{"names":["a","b"]}`)
		put, err := c.PutRecord(ctx, "h-routes", "cohort", "cohort", 0, cohort)
		if err != nil || put.Gen != 1 || put.HoldID != "h-routes" || put.Kind != "cohort" || put.Key != "cohort" {
			t.Fatalf("PutRecord of the cohort = %+v, %v", put, err)
		}
		records, err := c.Records(ctx, "h-routes", "cohort")
		if err != nil || len(records) != 1 || records[0].Gen != 1 || records[0].Key != "cohort" {
			t.Fatalf("Records of the cohort = %+v, %v", records, err)
		}
		if _, err := c.PutRecord(ctx, "h-routes", "cohort", "cohort", 0, cohort); err == nil {
			t.Fatal("a second insert of the cohort at expected gen 0 was stored")
		}
		if err := c.DeleteCohort(ctx, "h-routes"); err != nil {
			t.Fatalf("DeleteCohort: %v", err)
		}
		if records, err := c.Records(ctx, "h-routes", "cohort"); err != nil || len(records) != 0 {
			t.Fatalf("Records after DeleteCohort = %+v, %v", records, err)
		}
		held, err := c.MoveRow(ctx, prepared.Gen, natsgrant.OperatorPhaseHeld, "h-routes", "routes")
		if err != nil {
			t.Fatalf("MoveRow to held: %v", err)
		}
		cut, err := c.PutCutover(ctx, "session", "done", "h-routes")
		if err != nil || cut.HoldGen != held.Gen || cut.Family != "session" || cut.State != "done" {
			t.Fatalf("PutCutover = %+v, %v; want hold_gen %d", cut, err, held.Gen)
		}
		cutovers, err := c.Cutovers(ctx)
		if err != nil || len(cutovers) != 1 || cutovers[0] != cut {
			t.Fatalf("Cutovers = %+v, %v; want [%+v]", cutovers, err, cut)
		}
		_, err = c.MoveRow(ctx, prepared.Gen, natsgrant.OperatorPhaseReleased, "h-routes", "stale")
		var refusal *holdrecord.RouteError
		if !errors.As(err, &refusal) || refusal.Code != "gen_mismatch" || refusal.Row == nil || refusal.Row.Gen != held.Gen {
			t.Fatalf("MoveRow at a stale gen answered %v, want 409 gen_mismatch carrying the verified current row", err)
		}
	})

	t.Run("Admit refuses a command its workflow does not admit and writes nothing", func(t *testing.T) {
		r := newRig(t)
		c := r.client(t)
		hold(t, c, "h-notadmitted", holdrecord.WorkflowCutover, nil)
		_, before := journal(t, c, "h-notadmitted")
		writes := r.RecordWrites()
		_, err := c.Admit(ctx, "h-notadmitted", cmd("buckets create"), nil)
		refused(t, "Admit of buckets create in cutover", err, holdrecord.ErrNotAdmitted, "cutover")
		if _, after := journal(t, c, "h-notadmitted"); after != before || r.RecordWrites() != writes {
			t.Fatalf("the refused admission wrote: the record's gen moved from %d to %d, %d record writes", before, after,
				r.RecordWrites()-writes)
		}
	})

	t.Run("Admit writes its intent at the gen it read, and decides again after a write between", func(t *testing.T) {
		r := newRig(t)
		c, other := r.client(t), r.client(t)
		hold(t, c, "h-between", holdrecord.WorkflowBucketsCreate, nil)
		records, err := c.Records(ctx, "h-between", "done")
		if err != nil || len(records) != 1 {
			t.Fatalf("Records of the journal = %+v, %v", records, err)
		}
		before := records[0]
		// Another process writes the record between Admit's read and its write, the record's
		// content unchanged and its gen one up, so the write Admit read for is refused and its
		// second decision admits it again.
		fired := r.once(recordPut, func(http.ResponseWriter, *http.Request) bool {
			if _, err := other.PutRecord(ctx, "h-between", "done", "hold", before.Gen, before.Value); err != nil {
				t.Errorf("the write between: %v", err)
			}
			return false
		})
		writes := r.RecordWrites()
		if _, err := c.Admit(ctx, "h-between", cmd("buckets create"), map[string]string{"bucket": "b"}); err != nil {
			t.Fatalf("Admit after a write between its read and its write: %v", err)
		}
		d, gen := journal(t, c, "h-between")
		if !fired.Load() || gen != before.Gen+2 || r.RecordWrites()-writes != 3 {
			t.Fatalf("hook fired %v, the record at gen %d after %d writes; want gen %d after 3 (Admit's refused write, the write between, Admit's second)",
				fired.Load(), gen, r.RecordWrites()-writes, before.Gen+2)
		}
		if e := last(t, d); e.Command != "buckets create" || e.State != "intent" || e.Inputs["bucket"] != "b" {
			t.Fatalf("the journal's last entry is %+v, want buckets create's intent with its inputs", e)
		}
	})

	t.Run("Admit admits hold set with the row prepare and every other command only with the row held", func(t *testing.T) {
		r := newRig(t)
		c := r.client(t)
		current := row(t, c)
		prepared, err := c.MoveRow(ctx, current.Gen, natsgrant.OperatorPhasePrepare, "h-set", "set")
		if err != nil {
			t.Fatal(err)
		}
		if err := c.StartJournal(ctx, "h-set", holdrecord.WorkflowBucketsCreate, nil); err != nil {
			t.Fatal(err)
		}
		_, err = c.Admit(ctx, "h-set", cmd("buckets create"), nil)
		refused(t, "Admit of buckets create with the row prepare", err, holdrecord.ErrNotHeld, "prepare")
		_, err = c.Admit(ctx, "h-other", cmd(holdrecord.CommandSet), nil)
		refused(t, "Admit of hold set under another hold id", err, holdrecord.ErrNotHeld, "h-set")
		if w, err := c.Admit(ctx, "h-set", cmd(holdrecord.CommandSet), nil); err != nil || w != holdrecord.WorkflowBucketsCreate {
			t.Fatalf("Admit of hold set with the row prepare = %q, %v", w, err)
		}
		if _, err := c.MoveRow(ctx, prepared.Gen, natsgrant.OperatorPhaseHeld, "h-set", "set"); err != nil {
			t.Fatal(err)
		}
		if err := c.AppendDone(ctx, "h-set", holdrecord.CommandSet, nil); err != nil {
			t.Fatal(err)
		}
		if _, err := c.Admit(ctx, "h-set", cmd("buckets create"), nil); err != nil {
			t.Fatalf("Admit of buckets create with the row held: %v", err)
		}
	})

	t.Run("every command but hold release and hub-restore's hold abort needs hold set's done entry", func(t *testing.T) {
		r := newRig(t)
		c := r.client(t)
		for i, w := range []holdrecord.Workflow{holdrecord.WorkflowCutover, holdrecord.WorkflowRollback,
			holdrecord.WorkflowSelectionRecovery, holdrecord.WorkflowBucketsCreate, holdrecord.WorkflowBucketsRecreate,
			holdrecord.WorkflowBucketsRestore, holdrecord.WorkflowStreamsUpdate, holdrecord.WorkflowHubRestore,
			holdrecord.WorkflowStreamsCensus} {
			holdID := "h-order-" + strconv.Itoa(i)
			prepare(t, c, holdID, w, twoDisks)
			// hold set's own check refused in held: its stop entry ends its intent with no done.
			if err := c.AppendStop(ctx, holdID, holdrecord.CommandSet, "a step of 2 to 6 failed"); err != nil {
				t.Fatalf("%s: hold set's stop entry: %v", w, err)
			}
			for _, command := range holdrecord.Commands(w) {
				if w == holdrecord.WorkflowHubRestore && command == holdrecord.CommandAbort {
					// Decide is Admit's decision; an admitted abort's intent would keep hold release
					// out below, since only a complete record or one holding hold set's entries alone
					// admits it.
					d, _ := journal(t, c, holdID)
					if err := holdrecord.Decide(d, cmd(command)); err != nil {
						t.Fatalf("%s: hold abort before hold set's done entry answered %v, want admitted", w, err)
					}
					continue
				}
				_, err := c.Admit(ctx, holdID, cmd(command), nil)
				refused(t, string(w)+": Admit of "+command, err, holdrecord.ErrOutOfOrder, holdrecord.CommandSet)
			}
			// With no entry but hold set's, nothing changed, and hold release is admitted.
			release(t, c, holdID)
		}
	})

	t.Run("AppendDone and AppendStop write at the record's gen and need the command's intent", func(t *testing.T) {
		r := newRig(t)
		c := r.client(t)
		hold(t, c, "h-entries", holdrecord.WorkflowCutover, nil)
		refused(t, "AppendDone with no intent", c.AppendDone(ctx, "h-entries", "create-bucket", nil), holdrecord.ErrNoIntent, "create-bucket")
		refused(t, "AppendStop with no intent", c.AppendStop(ctx, "h-entries", "convert", "x"), holdrecord.ErrNoIntent, "convert")
		if _, err := c.Admit(ctx, "h-entries", cmd("create-bucket"), nil); err != nil {
			t.Fatal(err)
		}
		_, gen := journal(t, c, "h-entries")
		if err := c.AppendDone(ctx, "h-entries", "create-bucket", map[string]string{"bucket": "envoy_x"}); err != nil {
			t.Fatal(err)
		}
		d, after := journal(t, c, "h-entries")
		if e := last(t, d); after != gen+1 || e.Command != "create-bucket" || e.State != "done" || e.Outputs["bucket"] != "envoy_x" {
			t.Fatalf("after AppendDone the record is at gen %d (from %d) ending %+v", after, gen, e)
		}
		refused(t, "a second AppendDone", c.AppendDone(ctx, "h-entries", "create-bucket", nil), holdrecord.ErrNoIntent, "create-bucket")
		if _, err := c.Admit(ctx, "h-entries", cmd("convert"), nil); err != nil {
			t.Fatal(err)
		}
		_, gen = journal(t, c, "h-entries")
		if err := c.AppendStop(ctx, "h-entries", "convert", "no safe start for K"); err != nil {
			t.Fatal(err)
		}
		d, after = journal(t, c, "h-entries")
		if e := last(t, d); after != gen+1 || e.Command != "convert" || e.State != "stop" || e.Outputs["reason"] != "no safe start for K" {
			t.Fatalf("after AppendStop the record is at gen %d (from %d) ending %+v", after, gen, e)
		}
		refused(t, "a second AppendStop", c.AppendStop(ctx, "h-entries", "convert", "again"), holdrecord.ErrNoIntent, "convert")
	})

	t.Run("every entry clears the completion but hold release's intent and a stop entry", func(t *testing.T) {
		r := newRig(t)
		c := r.client(t)
		hold(t, c, "h-complete", holdrecord.WorkflowSelectionRecovery, nil)
		complete := func(step string, want bool) {
			t.Helper()
			if d, _ := journal(t, c, "h-complete"); d.Complete != want {
				t.Fatalf("after %s complete is %v, want %v", step, d.Complete, want)
			}
		}
		run(t, c, "h-complete", cmd("create-bucket --after-loss"), nil, nil)
		complete("the final create-bucket --after-loss's done entry", true)
		if _, err := c.Admit(ctx, "h-complete", cmd(holdrecord.CommandRelease), nil); err != nil {
			t.Fatal(err)
		}
		complete("hold release's intent", true)
		if err := c.AppendStop(ctx, "h-complete", holdrecord.CommandRelease, "its check refused"); err != nil {
			t.Fatal(err)
		}
		complete("hold release's stop entry", true)
		if _, err := c.Admit(ctx, "h-complete", cmd("replay"), nil); err != nil {
			t.Fatal(err)
		}
		complete("replay's intent", false)
		if err := c.AppendDone(ctx, "h-complete", "replay", nil); err != nil {
			t.Fatal(err)
		}
		complete("replay's done entry, not final", false)
	})

	t.Run("a final command's done entry completes the record in the same write", func(t *testing.T) {
		r := newRig(t)
		c := r.client(t)
		held := hold(t, c, "h-final", holdrecord.WorkflowCutover, nil)
		for _, command := range []string{"create-bucket", "replay", "convert"} {
			run(t, c, "h-final", cmd(command), nil, nil)
			if d, _ := journal(t, c, "h-final"); d.Complete {
				t.Fatalf("%s's done entry, not final, completed the record", command)
			}
		}
		if _, err := c.Admit(ctx, "h-final", cmd("streams census"), nil); err != nil {
			t.Fatal(err)
		}
		_, gen := journal(t, c, "h-final")
		if err := c.AppendDone(ctx, "h-final", "streams census", nil); err != nil {
			t.Fatal(err)
		}
		d, after := journal(t, c, "h-final")
		if !d.Complete || d.HoldGen != held.Gen || after != gen+1 || last(t, d).State != "done" {
			t.Fatalf("after the census's done entry: complete %v, hold_gen %d (row gen %d), record gen %d from %d", d.Complete,
				d.HoldGen, held.Gen, after, gen)
		}
	})

	t.Run("a final command killed during its done entry leaves no entry or the completed record", func(t *testing.T) {
		// Killed before the write reached the broker: no done entry, and the rerun is admitted and
		// completes the record.
		r := newRig(t)
		c := r.client(t)
		hold(t, c, "h-killed", holdrecord.WorkflowBucketsCreate, nil)
		if _, err := c.Admit(ctx, "h-killed", cmd("buckets create"), nil); err != nil {
			t.Fatal(err)
		}
		r.once(recordPut, lose(nil))
		if err := c.AppendDone(ctx, "h-killed", "buckets create", nil); err == nil {
			t.Fatal("an AppendDone whose request was lost answered no error")
		}
		if d, _ := journal(t, c, "h-killed"); d.Complete || last(t, d).State != "intent" {
			t.Fatalf("after the lost write the record is complete %v, ending %+v", d.Complete, last(t, d))
		}
		run(t, c, "h-killed", cmd("buckets create"), nil, nil)
		if d, _ := journal(t, c, "h-killed"); !d.Complete {
			t.Fatal("the rerun's done entry did not complete the record")
		}
		// Killed after the broker stored the write: the record is complete, and a rerun reads it so.
		r2 := newRig(t)
		c2 := r2.client(t)
		hold(t, c2, "h-killed2", holdrecord.WorkflowBucketsCreate, nil)
		if _, err := c2.Admit(ctx, "h-killed2", cmd("buckets create"), nil); err != nil {
			t.Fatal(err)
		}
		r2.once(recordPut, lose(r2.Routes()))
		if err := c2.AppendDone(ctx, "h-killed2", "buckets create", nil); err == nil {
			t.Fatal("an AppendDone whose answer was lost answered no error")
		}
		d, _ := journal(t, c2, "h-killed2")
		if !d.Complete || last(t, d).State != "done" {
			t.Fatalf("after the lost answer the record is complete %v, ending %+v", d.Complete, last(t, d))
		}
		_, err := c2.Admit(ctx, "h-killed2", cmd("buckets create"), nil)
		refused(t, "the rerun's Admit on the completed record", err, holdrecord.ErrAlreadyDone, "complete")
	})

	t.Run("Journal returns the verified record", func(t *testing.T) {
		r := newRig(t)
		c := r.client(t)
		hold(t, c, "h-journal", holdrecord.WorkflowHubRestore, twoDisks)
		d, gen := journal(t, c, "h-journal")
		if d.Workflow != holdrecord.WorkflowHubRestore || gen != 3 || len(d.Ops) != 2 || d.Ops[0].Command != holdrecord.CommandSet ||
			d.Ops[0].State != "intent" || d.Ops[1].State != "done" || d.Step1["step1_disk_id.A"] != "1" {
			t.Fatalf("Journal = %+v at gen %d", d, gen)
		}
	})

	t.Run("a row, a record and a cutover row that do not verify read as unknown", func(t *testing.T) {
		r := newRig(t)
		c := r.client(t)
		hold(t, c, "h-forged", holdrecord.WorkflowCutover, nil)
		if _, err := c.PutCutover(ctx, "session", "done", "h-forged"); err != nil {
			t.Fatal(err)
		}
		for _, forgery := range []struct {
			name, tamper, undo string
			read               func() error
		}{
			{"a row moved to released by a database writer", `update nats_hold set phase = 'released'`, `update nats_hold set phase = 'held'`,
				func() error { _, err := c.Row(ctx); return err }},
			{"a row without its sig", `update nats_hold set sig = ''`, ``, func() error { _, err := c.Row(ctx); return err }},
			{"a record completed by a database writer", `update nats_hold_records set value = jsonb_set(value, '{complete}', 'true')`, ``,
				func() error { _, _, err := c.Journal(ctx, "h-forged"); return err }},
			{"a cutover row rolled back by a database writer", `update nats_cutover set state = 'rolled_back'`, ``,
				func() error { _, err := c.Cutovers(ctx); return err }},
		} {
			if _, err := r.Pool.Exec(ctx, forgery.tamper); err != nil {
				t.Fatalf("%s: %v", forgery.name, err)
			}
			if err := forgery.read(); !errors.Is(err, holdrecord.ErrUnknown) {
				t.Errorf("%s read as %v, want ErrUnknown", forgery.name, err)
			}
			if forgery.undo != "" {
				if _, err := r.Pool.Exec(ctx, forgery.undo); err != nil {
					t.Fatal(err)
				}
			}
		}
	})

	t.Run("MoveRow refuses a phase outside the row's three", func(t *testing.T) {
		r := newRig(t)
		c := r.client(t)
		current := row(t, c)
		if _, err := c.MoveRow(ctx, current.Gen, natsgrant.OperatorPhase("paused"), "h-paused", "x"); err == nil ||
			!strings.Contains(err.Error(), "paused") {
			t.Fatalf("MoveRow to paused answered %v, want a refusal naming the phase", err)
		}
		if _, err := c.RestoreRow(ctx, current.Gen+1, natsgrant.OperatorPhase("paused"), "h-paused", "x"); err == nil {
			t.Fatal("RestoreRow of paused answered no error")
		}
		if after := row(t, c); after != current {
			t.Fatalf("the row moved from %+v to %+v", current, after)
		}
	})

	t.Run("a row restored held keeps a completed record complete, and hold release releases", func(t *testing.T) {
		r := newRig(t)
		c := r.client(t)
		hold(t, c, "h-restored", holdrecord.WorkflowBucketsRestore, nil)
		run(t, c, "h-restored", cmd("buckets restore"), nil, nil)
		completed, _ := journal(t, c, "h-restored")
		if _, err := r.Pool.Exec(ctx, `delete from nats_hold`); err != nil {
			t.Fatal(err)
		}
		var refusal *holdrecord.RouteError
		if _, err := c.Row(ctx); !errors.As(err, &refusal) || refusal.Code != "no_row" {
			t.Fatalf("Row with the row deleted answered %v, want no_row", err)
		}
		restored, err := c.RestoreRow(ctx, 1, natsgrant.OperatorPhaseHeld, "h-restored", "restored after a delete")
		if err != nil || restored.Gen <= completed.HoldGen || restored.Phase != natsgrant.OperatorPhaseHeld || restored.HoldID != "h-restored" {
			t.Fatalf("RestoreRow = %+v, %v; want held h-restored above gen %d", restored, err, completed.HoldGen)
		}
		d, _ := journal(t, c, "h-restored")
		if !d.Complete || d.HoldGen != completed.HoldGen {
			t.Fatalf("after the row's restore the record is complete %v at hold_gen %d, want complete at %d", d.Complete, d.HoldGen, completed.HoldGen)
		}
		release(t, c, "h-restored")
		if after := row(t, c); after.Phase != natsgrant.OperatorPhaseReleased {
			t.Fatalf("after hold release the row is %s", after.Phase)
		}
	})

	t.Run("in hub-restore the census needs a done prove", func(t *testing.T) {
		r := newRig(t)
		c := r.client(t)
		hold(t, c, "h-census", holdrecord.WorkflowHubRestore, twoDisks)
		_, err := c.Admit(ctx, "h-census", cmd("streams census"), nil)
		refused(t, "the census before any prove", err, holdrecord.ErrOutOfOrder, "hold prove")
	})

	t.Run("a mistyped --restored stops, and the corrected prove proves, completes and releases", func(t *testing.T) {
		r := newRig(t)
		c := r.client(t)
		hold(t, c, "h-mistyped", holdrecord.WorkflowHubRestore, twoDisks)
		prove := cmd("hold prove")
		if _, err := c.Admit(ctx, "h-mistyped", prove, map[string]string{"restored.A": "disk-9"}); err != nil {
			t.Fatal(err)
		}
		if err := c.AppendStop(ctx, "h-mistyped", "hold prove", "target A's disk is not disk-9"); err != nil {
			t.Fatal(err)
		}
		_, err := c.Admit(ctx, "h-mistyped", cmd("streams census"), nil)
		refused(t, "the census after a stopped prove", err, holdrecord.ErrOutOfOrder, "hold prove")
		run(t, c, "h-mistyped", prove, map[string]string{"restored.A": "disk-7"}, map[string]string{"disk.A": "disk-7", "disk_id.A": "7"})
		d, _ := journal(t, c, "h-mistyped")
		proof, ok := holdrecord.Proven(d)
		if !ok || proof.Disks["A"] != "disk-7" || proof.DiskIDs["A"] != "7" || len(proof.Disks) != 1 {
			t.Fatalf("Proven = %+v, %v; want A's disk-7 and id 7 alone", proof, ok)
		}
		run(t, c, "h-mistyped", cmd("streams census"), nil, nil)
		if d, _ := journal(t, c, "h-mistyped"); !d.Complete {
			t.Fatal("the census did not complete the record")
		}
		release(t, c, "h-mistyped")
	})

	t.Run("after completion a prove is admitted only when a disk moved from the expected one", func(t *testing.T) {
		r := newRig(t)
		c := r.client(t)
		hold(t, c, "h-moved", holdrecord.WorkflowHubRestore, twoDisks)
		run(t, c, "h-moved", cmd("hold prove"), map[string]string{"restored.A": "disk-7"}, map[string]string{"disk.A": "disk-7", "disk_id.A": "7"})
		run(t, c, "h-moved", cmd("streams census"), nil, nil)
		d, _ := journal(t, c, "h-moved")
		moved, err := holdrecord.Moved(d, map[string]string{"A": "7", "B": "2"})
		if err != nil || moved {
			t.Fatalf("Moved of the proven disks = %v, %v", moved, err)
		}
		_, err = c.Admit(ctx, "h-moved", holdrecord.Request{Command: "hold prove", DisksMoved: moved}, nil)
		refused(t, "a prove after completion with no disk moved", err, holdrecord.ErrAlreadyDone, "the record is complete")
		// A restore lands on B, which the prove did not name: against the expected map that is a move.
		moved, err = holdrecord.Moved(d, map[string]string{"A": "7", "B": "8"})
		if err != nil || !moved {
			t.Fatalf("Moved after a restore on B = %v, %v", moved, err)
		}
		if _, err := c.Admit(ctx, "h-moved", holdrecord.Request{Command: "hold prove", DisksMoved: moved}, nil); err != nil {
			t.Fatalf("a prove after completion with B moved: %v", err)
		}
		for _, read := range []map[string]string{{"A": "7"}, {"A": "7", "C": "2"}} {
			if _, err := holdrecord.Moved(d, read); !errors.Is(err, holdrecord.ErrUnknown) {
				t.Errorf("Moved of %v, not the step-1 map's targets, answered %v, want ErrUnknown", read, err)
			}
		}
	})

	t.Run("the step-1 map is StartJournal's, whatever a command reads later", func(t *testing.T) {
		r := newRig(t)
		c := r.client(t)
		hold(t, c, "h-step1", holdrecord.WorkflowHubRestore, twoDisks)
		run(t, c, "h-step1", cmd("hold prove"), map[string]string{"restored.A": "disk-7", "restored.B": "disk-8"},
			map[string]string{"disk.A": "disk-7", "disk_id.A": "7", "disk.B": "disk-8", "disk_id.B": "8"})
		d, _ := journal(t, c, "h-step1")
		step1, ok := holdrecord.Step1(d)
		if !ok || len(step1) != 2 || step1["step1_disk_id.A"] != "1" || step1["step1_disk_id.B"] != "2" {
			t.Fatalf("Step1 = %v, %v; want StartJournal's map", step1, ok)
		}
		proof, _ := holdrecord.Proven(d)
		if proof.DiskIDs["A"] != "7" || proof.DiskIDs["B"] != "8" {
			t.Fatalf("Proven's DiskIDs = %v, want each restored target's id", proof.DiskIDs)
		}
	})

	t.Run("hub-restore's hold abort: before a prove it completes, after one it is excluded, refused it stops", func(t *testing.T) {
		r := newRig(t)
		c := r.client(t)
		hold(t, c, "h-abort", holdrecord.WorkflowHubRestore, twoDisks)
		if _, err := c.Admit(ctx, "h-abort", cmd("hold abort"), nil); err != nil {
			t.Fatalf("hold abort before any prove: %v", err)
		}
		current := row(t, c)
		if _, err := c.MoveRow(ctx, current.Gen, natsgrant.OperatorPhaseReleased, "h-abort", "aborted"); err != nil {
			t.Fatal(err)
		}
		if err := c.AppendDone(ctx, "h-abort", "hold abort", nil); err != nil {
			t.Fatal(err)
		}
		if d, _ := journal(t, c, "h-abort"); !d.Complete {
			t.Fatal("hold abort's done entry did not complete the record")
		}

		hold(t, c, "h-abort2", holdrecord.WorkflowHubRestore, twoDisks)
		run(t, c, "h-abort2", cmd("hold prove"), nil, map[string]string{"disk.A": "disk-7", "disk_id.A": "7"})
		_, err := c.Admit(ctx, "h-abort2", cmd("hold abort"), nil)
		refused(t, "hold abort after a done prove", err, holdrecord.ErrExcluded, "hold prove")
		run(t, c, "h-abort2", cmd("streams census"), nil, nil)
		release(t, c, "h-abort2")

		hold(t, c, "h-abort3", holdrecord.WorkflowHubRestore, twoDisks)
		if _, err := c.Admit(ctx, "h-abort3", cmd("hold abort"), nil); err != nil {
			t.Fatal(err)
		}
		if err := c.AppendStop(ctx, "h-abort3", "hold abort", "target A's disk is not step 1's"); err != nil {
			t.Fatal(err)
		}
		if _, err := c.Admit(ctx, "h-abort3", cmd("hold prove"), map[string]string{"restored.A": "disk-7"}); err != nil {
			t.Fatalf("hold prove after a stopped abort: %v", err)
		}
	})

	t.Run("a pending convert makes every other command busy until its rerun finishes", func(t *testing.T) {
		r := newRig(t)
		c := r.client(t)
		hold(t, c, "h-busy", holdrecord.WorkflowCutover, nil)
		run(t, c, "h-busy", cmd("create-bucket"), nil, nil)
		if _, err := c.Admit(ctx, "h-busy", cmd("convert"), nil); err != nil {
			t.Fatal(err)
		}
		// convert's process is gone, its intent pending.
		for _, command := range []string{"replay", "streams census", holdrecord.CommandRelease, "create-bucket"} {
			_, err := c.Admit(ctx, "h-busy", cmd(command), nil)
			refused(t, "Admit of "+command+" with convert pending", err, holdrecord.ErrBusy, "convert")
		}
		run(t, c, "h-busy", cmd("convert"), nil, nil)
		run(t, c, "h-busy", cmd("streams census"), nil, nil)
		if d, _ := journal(t, c, "h-busy"); !d.Complete {
			t.Fatal("the census after convert's rerun did not complete the record")
		}
	})

	t.Run("convert stopped for no safe start lets replay give one, and its rerun completes", func(t *testing.T) {
		r := newRig(t)
		c := r.client(t)
		hold(t, c, "h-nostart", holdrecord.WorkflowCutover, nil)
		if _, err := c.Admit(ctx, "h-nostart", cmd("convert"), nil); err != nil {
			t.Fatal(err)
		}
		if err := c.AppendStop(ctx, "h-nostart", "convert", "no safe start for K"); err != nil {
			t.Fatal(err)
		}
		run(t, c, "h-nostart", cmd("replay"), map[string]string{"key": "K"}, nil)
		run(t, c, "h-nostart", cmd("convert"), nil, nil)
		run(t, c, "h-nostart", cmd("streams census"), nil, nil)
		if d, _ := journal(t, c, "h-nostart"); !d.Complete {
			t.Fatal("the census did not complete the record")
		}
	})

	t.Run("of two processes racing for an admission one is admitted and the other busy", func(t *testing.T) {
		r := newRig(t)
		c, other := r.client(t), r.client(t)
		hold(t, c, "h-race", holdrecord.WorkflowCutover, nil)
		writes := r.RecordWrites()
		r.once(recordPut, func(http.ResponseWriter, *http.Request) bool {
			if _, err := other.Admit(ctx, "h-race", cmd("replay"), nil); err != nil {
				t.Errorf("the other process's admission: %v", err)
			}
			return false
		})
		_, err := c.Admit(ctx, "h-race", cmd("create-bucket"), nil)
		refused(t, "the slower process's admission", err, holdrecord.ErrBusy, "replay")
		if n := r.RecordWrites() - writes; n != 2 {
			t.Fatalf("%d record writes, want 2: the other's intent and the refused one", n)
		}
		if d, _ := journal(t, c, "h-race"); last(t, d).Command != "replay" || last(t, d).State != "intent" {
			t.Fatalf("the journal ends %+v, want replay's intent", last(t, d))
		}
	})

	t.Run("hold release killed after its admission runs again and releases", func(t *testing.T) {
		r := newRig(t)
		c := r.client(t)
		hold(t, c, "h-release", holdrecord.WorkflowBucketsCreate, nil)
		if _, err := c.Admit(ctx, "h-release", cmd(holdrecord.CommandRelease), nil); err != nil {
			t.Fatal(err)
		}
		release(t, c, "h-release")
		if after := row(t, c); after.Phase != natsgrant.OperatorPhaseReleased {
			t.Fatalf("the row is %s after the rerun", after.Phase)
		}
	})

	t.Run("after a complete cutover replay is already done", func(t *testing.T) {
		r := newRig(t)
		c := r.client(t)
		hold(t, c, "h-after", holdrecord.WorkflowCutover, nil)
		for _, command := range []string{"create-bucket", "replay", "convert", "streams census"} {
			run(t, c, "h-after", cmd(command), nil, nil)
		}
		_, err := c.Admit(ctx, "h-after", cmd("replay"), nil)
		refused(t, "replay after a complete cutover", err, holdrecord.ErrAlreadyDone, "complete")
	})

	t.Run("in rollback restore --key is admitted for each logical name", func(t *testing.T) {
		r := newRig(t)
		c := r.client(t)
		hold(t, c, "h-rollback", holdrecord.WorkflowRollback, nil)
		_, err := c.Admit(ctx, "h-rollback", cmd("restore --key"), nil)
		refused(t, "restore --key before restore --prepare", err, holdrecord.ErrOutOfOrder, "restore --prepare")
		run(t, c, "h-rollback", cmd("restore --prepare"), nil, nil)
		run(t, c, "h-rollback", cmd("restore --key"), map[string]string{"name": "a"}, nil)
		run(t, c, "h-rollback", cmd("restore --key"), map[string]string{"name": "b"}, nil)
		run(t, c, "h-rollback", cmd("restore --finish"), nil, nil)
		if d, _ := journal(t, c, "h-rollback"); !d.Complete {
			t.Fatal("restore --finish did not complete the record")
		}
	})

	t.Run("a restore of one claim proves that claim alone, and the hold completes and releases", func(t *testing.T) {
		r := newRig(t)
		c := r.client(t)
		claims := map[string]string{"step1_disk_id.claim-a": "11", "step1_disk_id.claim-b": "12", "step1_disk_id.claim-c": "13"}
		hold(t, c, "h-oneclaim", holdrecord.WorkflowHubRestore, claims)
		run(t, c, "h-oneclaim", cmd("hold prove"), map[string]string{"restored.claim-b": "disk-21"},
			map[string]string{"disk.claim-b": "disk-21", "disk_id.claim-b": "21"})
		d, _ := journal(t, c, "h-oneclaim")
		proof, _ := holdrecord.Proven(d)
		want, _ := holdrecord.Expected(d)
		if len(proof.Disks) != 1 || proof.Disks["claim-b"] != "disk-21" || want["claim-a"] != "11" || want["claim-b"] != "21" || want["claim-c"] != "13" {
			t.Fatalf("Proven's Disks = %v and Expected = %v; want claim-b's disk alone, the others at their step-1 disks", proof.Disks, want)
		}
		if moved, err := holdrecord.Moved(d, map[string]string{"claim-a": "11", "claim-b": "21", "claim-c": "13"}); err != nil || moved {
			t.Fatalf("Moved of the expected disks = %v, %v", moved, err)
		}
		run(t, c, "h-oneclaim", cmd("streams census"), nil, nil)
		release(t, c, "h-oneclaim")
	})
}
