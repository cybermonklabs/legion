package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// The daemon a workflow configuration boots serves the give-up routes over its own intake: a
// give-up recorded in its store is discarded through the operator's route and answered with its
// id, rather than the 503 of a daemon that runs no intake, and the replay of another answers how
// it ended, closing it as the operator's.
func TestTheBootedDaemonsIntakeRoutesReachItsGiveUps(t *testing.T) {
	cfg := workflowConfig(t, workflowNATS(t))
	o := fakeRuntime(fake.NewRuntime(), &built{})
	o.workflowTokens = &workflowTokenRecorder{}
	d := startDaemon(t, cfg, o)

	ctx := context.Background()
	raw, err := pgxpool.New(ctx, cfg.PostgresDSN)
	if err != nil {
		t.Fatalf("open the daemon's database: %v", err)
	}
	pool := store.NewPool(raw)
	t.Cleanup(pool.Close)
	// Two messages of the daemon's Dispatch durable that did not decode, given up at their first
	// delivery. The second's subject names an issue of another project, which a replay decodes to
	// no fact.
	given := func(seq uint64, key string) record.IntakeFailure {
		t.Helper()
		message := record.IntakeMessage{Project: cfg.Project, Consumer: "legion-go-" + cfg.Project + "-dispatch",
			Subject: "notifications.dispatch.issue." + key + ".issue.updated", StreamSeq: seq, StoredAt: time.Now().Add(-time.Hour).UTC(),
			Payload: []byte(`{`)}
		var failure record.IntakeFailure
		if err := store.Transact(ctx, pool, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			failure, _, err = record.NewStore().RecordUndecodable(ctx, tx, message, 1, "decode Envoy envelope: unexpected end of JSON input", time.Now().UTC())
			return err
		}); err != nil {
			t.Fatalf("record the give-up at %d: %v", seq, err)
		}
		return failure
	}
	discarded := given(1, cfg.Project+"-1")
	replayed := given(2, "OTHER-1")

	status, body := d.request(http.MethodPost, "/legion/v1/operator/intake/discard",
		api.OperatorIntakeDiscardRequest{IntakeSelection: api.IntakeSelection{GiveUp: discarded.ID}, Reason: "a test discard"}, true)
	var discard api.IntakeDiscardResponse
	if status != http.StatusOK || json.Unmarshal(body, &discard) != nil || !slices.Equal(discard.Discarded, []int64{discarded.ID}) {
		t.Fatalf("the discard of %d = %d %s, want 200 naming it", discarded.ID, status, body)
	}

	status, body = d.request(http.MethodPost, "/legion/v1/operator/intake/replay",
		api.OperatorIntakeReplayRequest{IntakeSelection: api.IntakeSelection{GiveUp: replayed.ID}}, true)
	var replay api.IntakeReplayResponse
	if status != http.StatusOK || json.Unmarshal(body, &replay) != nil || len(replay.Replayed) != 1 ||
		replay.Replayed[0].ID != replayed.ID || replay.Replayed[0].Outcome != intake.ReplayNoFact {
		t.Fatalf("the replay of %d = %d %s, want 200 answering it no fact", replayed.ID, status, body)
	}

	for id, how := range map[int64]record.IntakeClosedHow{discarded.ID: record.IntakeDiscarded, replayed.ID: record.IntakeNoFact} {
		var closedHow, closedBy string
		if err := pool.QueryRow(ctx, `select coalesce(closed_how, ''), coalesce(closed_by, '') from intake_failures where id = $1`, id).
			Scan(&closedHow, &closedBy); err != nil {
			t.Fatalf("read give-up %d: %v", id, err)
		}
		if closedHow != string(how) || closedBy != "operator" {
			t.Errorf("give-up %d closed as %q by %q, want %s by the operator", id, closedHow, closedBy, how)
		}
	}
}
