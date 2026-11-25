package daemon

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/capabilities"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime/sandbox"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// The state route's source reads the cap the way an admission pass does, in the state's own
// snapshot (dispatch://INFRA-5616): `admission.cap` is the cap in force, here the controller's 2
// under a ceiling of 4, `admission.ceiling` the deployment's cap, `admission.override` the cap that
// stands with who set it, when and why, and `admission.capChanges` every change, newest first,
// under the names the project's Evals tab reads (dispatch://INFRA-979/spec#b-r39). The control is
// the state before any change: the cap is the ceiling, no override stands, and the list is empty
// rather than absent.
func TestTheStateNamesTheCapInForceTheCeilingAndEveryChange(t *testing.T) {
	t.Parallel()
	cfg := testConfig(t)
	cfg.AdmissionCap = 4
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
	for _, want := range []string{`"cap":4`, `"ceiling":4`, `"capChanges":[]`} {
		if !strings.Contains(before, want) {
			t.Fatalf("the admission before any change = %s, want it to hold %s", before, want)
		}
	}
	if strings.Contains(before, `"override"`) {
		t.Fatalf("the admission before any change = %s, want no override", before)
	}

	at := time.Date(2026, 11, 24, 6, 0, 0, 0, time.UTC)
	for _, change := range []store.AdmissionCapChange{
		{Project: cfg.Project, Cap: 3, CapBefore: 4, CapAfter: 3, Ceiling: 4, By: store.PauseActor{Kind: store.PauseByOperator},
			Reason: "a pool is down", At: at},
		{Project: cfg.Project, Cap: 2, CapBefore: 3, CapAfter: 2, Ceiling: 4,
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
	if got.Cap != 2 || got.Ceiling != 4 || got.Override == nil || got.Override.Cap != 2 || got.Override.SetBy["kind"] != "controller" ||
		got.Override.SetBy["session"] != "ses_controller" || !got.Override.SetAt.Equal(at.Add(time.Hour)) ||
		got.Override.Reason != "the merge queue waits 52 minutes" {
		t.Fatalf("the admission after two changes = %s, want cap 2 of a ceiling of 4 set by the controller", after)
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
	if newest["capBefore"] != float64(3) || newest["capAfter"] != float64(2) || oldest["cap"] != float64(3) {
		t.Errorf("capChanges = %s, want the controller's 3 to 2 first and the operator's 3 after it", after)
	}
	for _, change := range got.CapChanges {
		if _, carried := change["decisionId"]; carried {
			t.Errorf("the change %v carries a decisionId, which no rule of the cap names", change)
		}
	}
}
