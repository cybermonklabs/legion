// check_states_ci_test.go holds a settled CI run to the check states: the delivery intake, run as a
// server process runs it (delivery.StartIntakes) on a NATS server of the test's own, takes the
// listener's settlement of a pull request's checks, marks the issue of the pull request's open
// head owed, and the settler then publishes the state of the check CI proves that the run moved.
// No table stores a CI run, so the settlement is the one write that marks the issue for it, and a
// take-over of the intake's durable marks every issue with an open head, for the settlements a
// holder before it acknowledged with no mark.
package api

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/testnats"
)

// ciSettledRepo is the repository of #70, the pull request of ACME-1 whose CI runs settle.
const ciSettledRepo = "acme/app"

// settledCIRig is the check states rig whose issue ACME-1 holds k3, a check CI proves, with #70
// of acme/app open at H1 and attributed to ACME-1 while the run of k3's job goes on, the delivery
// intakes running on the rig's database, and every mark settled, the take-over's among them.
type settledCIRig struct {
	*checkStatesRig
	publisher *bus.Client
}

func newSettledCIRig(t *testing.T) *settledCIRig {
	t.Helper()
	rig := newCheckStatesRig(t, checkStatesWithCI)
	rig.github.listings.set(eachHeadH1, pendingRun(901, ciJobName, "in_progress"))
	writeHead(t, rig.database, ciSettledRepo, 70, eachHeadH1, "open", rig.issue)
	url, publisher := deliveryBus(t)
	startDeliveryIntakes(t, rig, url, publisher)
	started := eventsOfCheck(settleOwed(t, rig), "k3", eachHeadH1)
	if len(started) != 1 || started[0].state() != "no" || started[0].Payload.CICase != ciCaseNotCompleted {
		t.Fatalf("k3 at H1 while its run goes on: %v, want one event of no with ci_case %s", eventLines(started), ciCaseNotCompleted)
	}
	return &settledCIRig{checkStatesRig: rig, publisher: publisher}
}

// deliveryBus is a NATS server of the test's own and a client of it that owns its stream and
// publishes.
func deliveryBus(t *testing.T) (url string, publisher *bus.Client) {
	t.Helper()
	_, url = testnats.Start(t)
	publisher, err := bus.ConnectOwningStream([]string{url})
	if err != nil {
		t.Fatalf("connect the publisher: %v", err)
	}
	t.Cleanup(publisher.Close)
	return url, publisher
}

// startDeliveryIntakes runs the delivery intakes on the rig's database and the NATS server at url
// until the test ends, on their own reservation of the rig's lease pool, with the server's GitHub
// App client and event broker, as cmd/dispatch starts them. It returns once the delivery durable
// has its subscriber: the take-over binds the durable after its mark has committed.
func startDeliveryIntakes(t *testing.T, rig *checkStatesRig, url string, publisher *bus.Client) {
	t.Helper()
	if _, err := delivery.PutSettings(t.Context(), rig.database.Pool, delivery.DeliverySettings{
		DeployRepo: "acme/widgets", DeployWorkflowPath: ".github/workflows/deploy.yml",
		ProductionJobNames: []string{"widgets-release / widgets-release"}, PRChecksWorkflowPath: ".github/workflows/pr-checks.yml",
		PopulationAuthors: []string{"octocat"},
	}, model.Actor{Kind: "system", ID: "check-states-test"}); err != nil {
		t.Fatalf("write the delivery settings: %v", err)
	}
	leases, err := delivery.ReserveIntakeLeases(rig.callers)
	if err != nil {
		t.Fatalf("reserve the intakes' callers of the lease pool: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	stop, err := delivery.StartIntakes(ctx, func(name string) (*bus.Client, error) {
		return bus.Connect([]string{url}, bus.WithName(name))
	}, rig.database.Pool, leases, rig.served.deps.GitHub, rig.served.deps.Events, nil, nil, rig.served.deps.Attributor)
	if err != nil {
		cancel()
		t.Fatalf("start the intakes: %v", err)
	}
	t.Cleanup(func() {
		cancel()
		stop()
	})
	awaitDelivery(t, publisher, "the delivery intake binding its durable", func(info *natsgo.ConsumerInfo) bool { return info.PushBound })
}

// awaitDelivery polls the delivery durable for up to 20 s until done holds of it, and fails the
// test naming what when it does not.
func awaitDelivery(t *testing.T, publisher *bus.Client, what string, done func(*natsgo.ConsumerInfo) bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		info, err := publisher.JS().ConsumerInfo(bus.Stream, bus.DispatchDeliveryDurable)
		if err == nil && done(info) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s did not happen within 20s: %+v, %v", what, info, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// publishSettlement publishes the listener's settlement of #number of acme/app at sha, with the
// one green run of k3's job, and waits until the delivery durable has acknowledged every message
// the stream holds: the intake writes before it acknowledges, so what the settlement caused is in
// place by then.
func publishSettlement(t *testing.T, publisher *bus.Client, number int, sha string) {
	t.Helper()
	payload := fmt.Sprintf(`{"kind":"checks","repo":%q,"number":"%d","sha":%q,`+
		`"check_runs":[{"name":%q,"id":901,"conclusion":"success"}],"generation":2,`+
		`"snapshot":"8a05685f026c9caa16161a227de93b41bd04ed7de085ba044b495754afd55bdf",`+
		`"failed":{"count":0,"checks":[]},"running":{"count":0,"checks":[]},"passed":{"count":1,"checks":[%q]},`+
		`"queued":{"count":0,"checks":[]},"cancelled":{"count":0,"checks":[]},"skipped":{"count":0,"checks":[]},"failing_checks":[]}`,
		ciSettledRepo, number, sha, ciJobName, ciJobName)
	id := fmt.Sprintf("%s-%d", t.Name(), time.Now().UnixNano())
	if err := publisher.Publish(contracts.Envelope{
		EventID: id, Source: "github", SourceEventID: id,
		Topic:     fmt.Sprintf("notifications.github.acme.app.pr.%d.checks", number),
		DedupeKey: fmt.Sprintf("github.checks.%s.pr.%d.%s.g2", ciSettledRepo, number, sha),
		Payload:   payload, TraceID: id, IssuedAt: time.Now().Unix(),
	}); err != nil {
		t.Fatalf("publish the settlement of #%d: %v", number, err)
	}
	stream, err := publisher.JS().StreamInfo(bus.Stream)
	if err != nil {
		t.Fatalf("read the stream: %v", err)
	}
	awaitDelivery(t, publisher, fmt.Sprintf("the acknowledgement of the settlement of #%d", number), func(info *natsgo.ConsumerInfo) bool {
		return info.AckFloor.Stream >= stream.State.LastSeq && info.NumAckPending == 0
	})
}

// checkStateMark is one row of check_states_owed, its due time as Postgres prints it.
type checkStateMark struct {
	id       int64
	dueAt    string
	failures int
}

// marksHeld is every mark of issue, in id order.
func marksHeld(t *testing.T, database *store.Store, issue string) []checkStateMark {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(),
		`select id, due_at::text, failures from check_states_owed where issue_key = $1 order by id`, issue)
	if err != nil {
		t.Fatalf("read the marks of %s: %v", issue, err)
	}
	defer rows.Close()
	var marks []checkStateMark
	for rows.Next() {
		var mark checkStateMark
		if err := rows.Scan(&mark.id, &mark.dueAt, &mark.failures); err != nil {
			t.Fatalf("read a mark of %s: %v", issue, err)
		}
		marks = append(marks, mark)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the marks of %s: %v", issue, err)
	}
	return marks
}

// TestASettledCIRunPublishesTheStateOfItsCheck: #70 of acme/app is attributed to ACME-1 at H1, and
// k3 of ACME-1 is a check CI proves, whose job's run goes on. The listener's settlement of #70
// reaches the intake as a notifications.github.acme.app.pr.70.checks envelope and leaves one mark
// of ACME-1. A settle that succeeds deletes the mark and publishes; a failed read of GitHub, and a
// settle whose transaction fails, keep it. A settlement of a pull request no issue holds, or of
// one that is not open, leaves no mark.
func TestASettledCIRunPublishesTheStateOfItsCheck(t *testing.T) {
	t.Run("the run concluded", func(t *testing.T) {
		rig := newSettledCIRig(t)
		rig.github.listings.set(eachHeadH1, completedRun(901, ciJobName, "success"))
		publishSettlement(t, rig.publisher, 70, eachHeadH1)
		if marks := marksOf(t, rig.database, rig.issue); marks != 1 {
			t.Fatalf("the settlement of #70 left %d marks of %s, want 1", marks, rig.issue)
		}
		events := settleOwed(t, rig.checkStatesRig)
		wantEvents(t, "k3's run concluded", events, stateWant{"k3", eachHeadH1, "yes", ""})
		if !slices.Equal(events[0].Payload.Roles, []string{"ci"}) || events[0].Payload.CICase != "success" {
			t.Fatalf("k3 at H1 once its run passed: roles %v, ci_case %q, want [ci] and success", events[0].Payload.Roles, events[0].Payload.CICase)
		}
		if marks := marksOf(t, rig.database, rig.issue); marks != 0 {
			t.Fatalf("the settle left %d marks of %s, want none", marks, rig.issue)
		}
	})
	t.Run("the read of the check runs failed", func(t *testing.T) {
		rig := newSettledCIRig(t)
		rig.github.listings.failWith(http.StatusServiceUnavailable)
		publishSettlement(t, rig.publisher, 70, eachHeadH1)
		if marks := marksOf(t, rig.database, rig.issue); marks != 1 {
			t.Fatalf("the settlement of #70 left %d marks of %s, want 1", marks, rig.issue)
		}
		wantEvents(t, "GitHub answering 503", settleOwed(t, rig.checkStatesRig), stateWant{"k3", eachHeadH1, "unknown", "read_failed"})
		var due float64
		var failures, marks int
		if err := rig.database.Pool.QueryRow(context.Background(), `
			select count(*), coalesce(max(failures), 0), coalesce(extract(epoch from max(due_at) - now()), 0)::float8
			from check_states_owed where issue_key = $1`, rig.issue).Scan(&marks, &failures, &due); err != nil {
			t.Fatalf("read the marks of %s: %v", rig.issue, err)
		}
		if marks != 1 || failures != 1 || due < 20 || due > 30 {
			t.Fatalf("after the failed read %s holds %d marks, failures %d, due in %.1fs; want one mark, failures 1, due 30s on", rig.issue, marks, failures, due)
		}
	})
	t.Run("the settle's transaction failed", func(t *testing.T) {
		rig := newSettledCIRig(t)
		rig.github.listings.set(eachHeadH1, completedRun(901, ciJobName, "success"))
		publishSettlement(t, rig.publisher, 70, eachHeadH1)
		before := marksHeld(t, rig.database, rig.issue)
		if len(before) != 1 {
			t.Fatalf("the settlement of #70 left %d marks of %s, want 1", len(before), rig.issue)
		}
		after := lastEventID(t, rig.database)
		ctx, cancel := context.WithCancel(store.WithTransactionTracking(t.Context()))
		defer cancel()
		rig.settler.hooks.afterCompute = func(string) { cancel() }
		rig.settler.pass(ctx)
		rig.settler.hooks.afterCompute = nil
		if events := checkStateEventsAfter(t, rig.database, after); len(events) != 0 {
			t.Fatalf("a settle whose transaction failed published %v, want nothing", eventLines(events))
		}
		if kept := marksHeld(t, rig.database, rig.issue); !slices.Equal(kept, before) {
			t.Fatalf("after a settle whose transaction failed %s holds the marks %+v, want them as they were: %+v", rig.issue, kept, before)
		}
		wantEvents(t, "the kept mark's next settle", settleOwed(t, rig.checkStatesRig), stateWant{"k3", eachHeadH1, "yes", ""})
	})
	t.Run("a pull request that no issue holds, or that is not open", func(t *testing.T) {
		rig := newSettledCIRig(t)
		writeHead(t, rig.database, ciSettledRepo, 99, eachHeadH2, "open", "")
		writeHead(t, rig.database, ciSettledRepo, 71, eachHeadH3, "closed", rig.issue)
		settleOwed(t, rig.checkStatesRig)
		rig.github.listings.set(eachHeadH1, completedRun(901, ciJobName, "success"))
		publishSettlement(t, rig.publisher, 99, eachHeadH2)
		publishSettlement(t, rig.publisher, 71, eachHeadH3)
		var marks int
		if err := rig.database.Pool.QueryRow(context.Background(), `select count(*) from check_states_owed`).Scan(&marks); err != nil {
			t.Fatalf("count the marks: %v", err)
		}
		if marks != 0 {
			t.Fatalf("the settlements of #99, which no issue holds, and of #71, which is closed, left %d marks, want none", marks)
		}
	})
}

// TestASettlementTheHolderBeforeAcknowledgedIsPublishedAfterTheTakeOver: during a rolling release
// the previous build's intake holds the delivery durable until this build's takes it over, and a
// build before the settlements' marks acknowledges a settlement of a pull request's checks with
// no mark. #70 of acme/app is attributed to ACME-1 at H1 while the run of k3's job goes on. The
// holder before -- a subscriber bound to the durable that acknowledges every message and marks
// nothing -- acknowledges the settlement of #70 after the run passed, and no mark of ACME-1 is
// left. Once it lets go, this build's intake takes the durable over, which leaves one mark of
// ACME-1, and one settle publishes k3 at H1 yes, with roles [ci] and ci_case success.
func TestASettlementTheHolderBeforeAcknowledgedIsPublishedAfterTheTakeOver(t *testing.T) {
	rig := newCheckStatesRig(t, checkStatesWithCI)
	rig.github.listings.set(eachHeadH1, pendingRun(901, ciJobName, "in_progress"))
	writeHead(t, rig.database, ciSettledRepo, 70, eachHeadH1, "open", rig.issue)
	started := eventsOfCheck(settleOwed(t, rig), "k3", eachHeadH1)
	if len(started) != 1 || started[0].state() != "no" || started[0].Payload.CICase != ciCaseNotCompleted {
		t.Fatalf("k3 at H1 while its run goes on: %v, want one event of no with ci_case %s", eventLines(started), ciCaseNotCompleted)
	}
	url, publisher := deliveryBus(t)

	inbox := natsgo.NewInbox()
	before, err := publisher.Conn.Subscribe(inbox, func(msg *natsgo.Msg) { _ = msg.Ack() })
	if err != nil {
		t.Fatalf("subscribe the holder before: %v", err)
	}
	if _, err := publisher.JS().AddConsumer(bus.Stream, &natsgo.ConsumerConfig{
		Durable: bus.DispatchDeliveryDurable, DeliverSubject: inbox, FilterSubject: "notifications.github.>",
		AckPolicy: natsgo.AckExplicitPolicy,
	}); err != nil {
		t.Fatalf("create the durable the holder before delivers from: %v", err)
	}
	rig.github.listings.set(eachHeadH1, completedRun(901, ciJobName, "success"))
	publishSettlement(t, publisher, 70, eachHeadH1)
	if marks := marksOf(t, rig.database, rig.issue); marks != 0 {
		t.Fatalf("the holder before left %d marks of %s, want none: it marks nothing", marks, rig.issue)
	}
	if err := before.Unsubscribe(); err != nil {
		t.Fatalf("the holder before letting go: %v", err)
	}
	awaitDelivery(t, publisher, "the holder before letting the durable go", func(info *natsgo.ConsumerInfo) bool { return !info.PushBound })

	startDeliveryIntakes(t, rig, url, publisher)
	if marks := marksOf(t, rig.database, rig.issue); marks != 1 {
		t.Fatalf("the take-over left %d marks of %s, whose head #70 is open, want 1", marks, rig.issue)
	}
	events := settleOwed(t, rig)
	wantEvents(t, "the settle after the take-over", events, stateWant{"k3", eachHeadH1, "yes", ""})
	if !slices.Equal(events[0].Payload.Roles, []string{"ci"}) || events[0].Payload.CICase != "success" {
		t.Fatalf("k3 at H1 after the take-over: roles %v, ci_case %q, want [ci] and success", events[0].Payload.Roles, events[0].Payload.CICase)
	}
}
