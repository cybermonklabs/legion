package daemon

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/capabilities"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime/sandbox"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// The state route's source reads the cap the way an admission pass does, in the state's own
// snapshot (dispatch://INFRA-5616), under a starting cap of 2 and a ceiling of 4, two numbers a
// swap of the two would show: `admission.cap` is the cap in force, here the controller's 1,
// `admission.ceiling` is admission_ceiling, `admission.override` the cap that stands with who set
// it, when and why, and `admission.capChanges` every change, newest first, under the names the
// project's Evals tab reads (dispatch://INFRA-979/spec#b-r39). The control is the state before any
// change: the cap is the starting cap, the ceiling is apart from it, no override stands, and the
// list is empty rather than absent. The boot line reads the same cap the same way
// (bootAdmissionCap), the controller's session included, and on a budget of its own, so a boot
// whose replay used up its own budget still writes the cap.
func TestTheStateNamesTheCapInForceTheCeilingAndEveryChange(t *testing.T) {
	t.Parallel()
	cfg := testConfig(t)
	cfg.AdmissionCap, cfg.AdmissionCeiling = 2, 4
	ctx := context.Background()
	st, err := store.Open(ctx, cfg.PostgresDSN)
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}
	t.Cleanup(st.Close)
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate the store: %v", err)
	}
	project, err := claim.ProjectToken(cfg.Project)
	if err != nil {
		t.Fatal(err)
	}
	view := &source{
		store: st, records: record.NewStore(), project: cfg.Project, projectToken: project, runtime: "tmux", admissionCap: cfg.AdmissionCap, admissionCeiling: cfg.AdmissionCeiling,
		startedAt: time.Now(), deployment: func([]sandbox.CacheStatus) capabilities.Deployment { return capabilities.Deployment{} },
		reports: func() map[claim.Token]capabilities.Report { return nil },
	}
	admission := func() []byte {
		t.Helper()
		tx, err := st.Pool().BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
		if err != nil {
			t.Fatalf("begin the state snapshot: %v", err)
		}
		defer tx.Rollback(ctx)
		state, _, err := view.State(ctx, tx)
		if err != nil {
			t.Fatalf("State: %v", err)
		}
		encoded, err := json.Marshal(state.Admission)
		if err != nil {
			t.Fatalf("marshal the admission: %v", err)
		}
		return encoded
	}

	before := string(admission())
	for _, want := range []string{`"cap":2`, `"ceiling":4`, `"capChanges":[]`} {
		if !strings.Contains(before, want) {
			t.Fatalf("the admission before any change = %s, want it to hold %s", before, want)
		}
	}
	if strings.Contains(before, `"override"`) {
		t.Fatalf("the admission before any change = %s, want no override", before)
	}

	at := time.Date(2026, 11, 24, 6, 0, 0, 0, time.UTC)
	for _, change := range []store.AdmissionCapChange{
		{Project: cfg.Project, Cap: 3, CapBefore: 2, CapAfter: 3, Ceiling: 4, By: store.PauseActor{Kind: store.PauseByOperator},
			Reason: "a pool is down", At: at},
		{Project: cfg.Project, Cap: 1, CapBefore: 3, CapAfter: 1, Ceiling: 4,
			By:     store.PauseActor{Kind: store.PauseByController, Session: "ses_controller"},
			Reason: "the merge queue waits 52 minutes", At: at.Add(time.Hour)},
	} {
		if err := store.Transact(ctx, st.Pool(), func(ctx context.Context, tx pgx.Tx) error {
			_, err := store.RecordAdmissionCapChangeTx(ctx, tx, change)
			return err
		}); err != nil {
			t.Fatalf("record the change to %d: %v", change.Cap, err)
		}
	}

	var got struct {
		Cap      int `json:"cap"`
		Ceiling  int `json:"ceiling"`
		Override *struct {
			Cap    int               `json:"cap"`
			SetBy  map[string]string `json:"setBy"`
			SetAt  time.Time         `json:"setAt"`
			Reason string            `json:"reason"`
		} `json:"override"`
		CapChanges []map[string]any `json:"capChanges"`
	}
	after := admission()
	if err := json.Unmarshal(after, &got); err != nil {
		t.Fatalf("read the admission back: %v", err)
	}
	if got.Cap != 1 || got.Ceiling != 4 || got.Override == nil || got.Override.Cap != 1 || got.Override.SetBy["kind"] != "controller" ||
		got.Override.SetBy["session"] != "ses_controller" || !got.Override.SetAt.Equal(at.Add(time.Hour)) ||
		got.Override.Reason != "the merge queue waits 52 minutes" {
		t.Fatalf("the admission after two changes = %s, want cap 1 under a ceiling of 4 set by the controller", after)
	}
	if len(got.CapChanges) != 2 {
		t.Fatalf("capChanges = %s, want the two changes", after)
	}
	newest, oldest := got.CapChanges[0], got.CapChanges[1]
	for _, key := range []string{"id", "cap", "capBefore", "capAfter", "ceiling", "setBy", "setAt", "reason"} {
		if _, ok := newest[key]; !ok {
			t.Errorf("the newest change %v names no %s", newest, key)
		}
	}
	if newest["capBefore"] != float64(3) || newest["capAfter"] != float64(1) || oldest["cap"] != float64(3) || oldest["capBefore"] != float64(2) {
		t.Errorf("capChanges = %s, want the controller's 3 to 1 first and the operator's 2 to 3 after it", after)
	}
	for _, change := range got.CapChanges {
		if _, carried := change["decisionId"]; carried {
			t.Errorf("the change %v carries a decisionId, which no rule of the cap names", change)
		}
	}

	want := []any{"admissionStart", 2, "admissionCeiling", 4, "admissionCap", 1, "admissionCapSet", 1,
		"admissionCapSetBy", "controller", "admissionCapSetBySession", "ses_controller",
		"admissionCapSetAt", "2026-11-24T07:00:00Z", "admissionCapReason", "the merge queue waits 52 minutes"}
	if boot := bootAdmissionCap(ctx, st, cfg); !reflect.DeepEqual(boot, want) {
		t.Errorf("the boot line's cap words = %#v, want %#v", boot, want)
	}
	// The boot writes its line after the replay of terminal claims, which runs on start's
	// goroutine (supervision.start) and can use up the budget the boot's starting context carries.
	// The line's read takes a budget of its own, so the context it is handed may have no time
	// left: here one whose deadline has passed, as starting's has after a replay that took its
	// budget. A read under that context would write admissionCapReadError for a cap that reads.
	spent, cancelSpent := context.WithDeadline(ctx, time.Now().Add(-time.Second))
	defer cancelSpent()
	if boot := bootAdmissionCap(spent, st, cfg); !reflect.DeepEqual(boot, want) {
		t.Errorf("the boot line's cap words after a replay used up the boot's budget = %#v, want %#v", boot, want)
	}
}

// The boot's own workflow (openWorkflow) gives admission the deployment's ceiling, not only its
// starting cap (workflow.go's UseCeiling): under a starting cap of 1 and a ceiling of 3, two roots
// wait, the first pass admits one, and a set of 3 admits the second on its own pass and records
// the change from 1 to 3 under a ceiling of 3. The control is the first pass, which admits one
// root under the starting cap; a workflow that gave admission no ceiling admits no second root at
// the set and records it from 1 to 1.
func TestTheBootsWorkflowAdmitsUpToTheDeploymentsCeiling(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	w := boundedCopyRuntime(t, pool, &copyBoard{}, 1, 3)
	for _, key := range []string{"CAPTURE-1", "CAPTURE-2"} {
		seedTickIssue(t, pool, record.Issue{Key: key, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
			Project: "CAPTURE", Title: key, Tree: key, Phase: phase.Admitted, Generation: 1, Status: "todo", Rank: key, HandedOver: true})
		seedTickMirror(t, pool, key, "todo", 1)
	}
	slotted := func() int {
		t.Helper()
		var slots []record.Slot
		if err := store.Transact(ctx, pool, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			slots, err = record.NewStore().Slots(ctx, tx)
			return err
		}); err != nil {
			t.Fatalf("read the slots: %v", err)
		}
		return len(slots)
	}

	applyCopyFact(t, w, "controller", "tick", intake.ControllerTick{})
	if n := slotted(); n != 1 {
		t.Fatalf("slots under the starting cap of 1 = %d, want 1", n)
	}
	applyCopyFact(t, w, "admission-cap", "set-3", intake.AdmissionCapSet{Cap: 3, By: store.PauseActor{Kind: store.PauseByOperator},
		Reason: "a person asked for 3"})
	if n := slotted(); n != 2 {
		t.Fatalf("slots after a set of 3 under a ceiling of 3 = %d, want 2: admission admits under its ceiling", n)
	}
	var change store.AdmissionCapChange
	if err := store.Transact(ctx, pool, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		change, err = store.AdmissionCapTx(ctx, tx, "CAPTURE")
		return err
	}); err != nil {
		t.Fatalf("read the change: %v", err)
	}
	if change.Cap != 3 || change.CapBefore != 1 || change.CapAfter != 3 || change.Ceiling != 3 {
		t.Fatalf("the change = %+v, want 3 from 1 to 3 under a ceiling of 3", change)
	}
}
