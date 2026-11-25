package daemon

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// Each give-up counts itself in the project's intake-gave-up notice (record.Postgres.RecordUndecodable
// here), and the outbox executor publishes that row as it publishes every controller notice: on the
// controller topic of the daemon's own project, under the row's own key, with the count of the
// give-ups it took and the id, consumer and subject of each. The two give-ups come from two of the
// project's consumers, so the row they join is the project's, not one consumer's.
func TestTheIntakeGaveUpNoticeReachesTheControllerTopic(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	const project = "LEGION"
	token, err := claim.ProjectToken(project)
	if err != nil {
		t.Fatal(err)
	}
	// A registered controller, as `legion controller start` and its session's registration leave
	// the row: with none, a give-up counts itself in no notice.
	if _, err := pool.Exec(ctx, `insert into controllers (project, capability_hash, generation, session, secret_hash, registered_at)
		values ($1, $2, 1, 'ses-controller', $3, now())`, token, []byte("capability"), []byte("secret")); err != nil {
		t.Fatalf("register %s's controller: %v", project, err)
	}

	gaveUpAt := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	var given []record.IntakeFailure
	for i, message := range []record.IntakeMessage{
		{Project: project, Consumer: "legion-go-LEGION-dispatch", Subject: "notifications.dispatch.issue.LEGION-7.issue.updated",
			StreamSeq: 41, StoredAt: gaveUpAt.Add(-time.Minute), Payload: []byte(`{`)},
		{Project: project, Consumer: "legion-go-LEGION-github", Subject: "notifications.github.acme.widgets.pr.7",
			StreamSeq: 42, StoredAt: gaveUpAt.Add(-time.Minute), Payload: []byte(`{`)},
	} {
		if err := store.Transact(ctx, pool, func(ctx context.Context, tx pgx.Tx) error {
			failure, recorded, err := records.RecordUndecodable(ctx, tx, message, 1, "the payload is not JSON", gaveUpAt.Add(time.Duration(i)*time.Second))
			if err == nil && !recorded {
				err = errors.New("it was recorded already")
			}
			given = append(given, failure)
			return err
		}); err != nil {
			t.Fatalf("give up %s's message %d: %v", message.Consumer, message.StreamSeq, err)
		}
	}
	if given[0].NoticeID == 0 || given[1].NoticeID != given[0].NoticeID {
		t.Fatalf("the give-ups were counted in notices %d and %d, want one notice for both", given[0].NoticeID, given[1].NoticeID)
	}

	publisher := &outboxPublisher{}
	runner := &outbox{pool: pool, records: records, notices: publisher, project: token, dispatchProject: project, log: quietLogger(),
		// Past the minute the notice waits for the give-ups after its first to join it.
		now: func() time.Time { return gaveUpAt.Add(2 * time.Minute) }}
	if err := runner.RunOnce(ctx); err != nil {
		t.Fatalf("run the outbox: %v", err)
	}

	published := publisher.publishes()
	if len(published) != 1 {
		t.Fatalf("the outbox published %+v, want the one notice", published)
	}
	got := published[0]
	if want := "notifications.legion." + token + ".controller"; got.topic != want || got.topic != notify.ControllerTopic(token) {
		t.Errorf("the notice went to %s, want the controller topic of the daemon's own project, %s", got.topic, want)
	}
	if want := "intake-gave-up on " + project; got.message != want {
		t.Errorf("the notice's summary is %q, want %q", got.message, want)
	}
	if want := record.OutboxKey(given[0].NoticeID); got.key != want {
		t.Errorf("the notice was published under key %q, want its row's own, %q", got.key, want)
	}
	want := record.Notice{Kind: record.IntakeGaveUpNotice, Count: 2, GiveUps: []record.NoticeGiveUp{
		{ID: given[0].ID, Consumer: given[0].Consumer, Subject: given[0].Subject},
		{ID: given[1].ID, Consumer: given[1].Consumer, Subject: given[1].Subject},
	}}
	if !reflect.DeepEqual(got.payload, want) {
		t.Errorf("the notice's payload = %+v, want %+v: the count and each give-up's id, consumer and subject", got.payload, want)
	}
	if rows := outboxRows(t, pool); rows != 0 {
		t.Errorf("the outbox holds %d rows after the publish, want the notice's finished", rows)
	}
}
