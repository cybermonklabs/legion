package intake

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// wakeMigration is one daemon's wake consumer on one stream, taken through the four deploys of the
// wake migration (plan C, t-c-10): today's consumer on the shared wake subject alone; deploy 1's
// update in place to both wake subjects (held here as that build's openConsumer makes it: the same
// durable updated with both filters, its start kept); deploy 2's listeners, which put role and
// agent reports on the project's own subject; and this build's boot, which drops the shared filter
// in place (OpenConsumers).
type wakeMigration struct {
	t           *testing.T
	js          jetstream.JetStream
	stream      jetstream.Stream
	pool        *pgxpool.Pool
	spec        ConsumerSpec
	wake        string
	projectWake string
}

func newWakeMigration(t *testing.T) *wakeMigration {
	t.Helper()
	pool := migratedPool(t)
	createWrites(t, pool)
	js, stream := testJetStream(t)
	spec := consumerSpec(&lockedBuffer{})
	projectWake, err := wakeSubject(spec.Project)
	if err != nil {
		t.Fatal(err)
	}
	m := &wakeMigration{t: t, js: js, stream: stream, pool: pool, spec: spec, wake: wakeConsumerName(spec.Project), projectWake: projectWake}
	// Today's consumer, on the shared subject alone, which has applied a liveness report.
	publish(t, js, legionWakeSubject, wakeReport(t, "ready-today", ListenerWakeReady, ""))
	if _, err := stream.CreateConsumer(context.Background(), jetstream.ConsumerConfig{Durable: m.wake, FilterSubject: legionWakeSubject, AckPolicy: jetstream.AckExplicitPolicy}); err != nil {
		t.Fatalf("create today's wake consumer: %v", err)
	}
	m.drain()
	return m
}

// deploy1 updates the wake consumer in place to both wake subjects, as deploy 1's boot does.
func (m *wakeMigration) deploy1() {
	m.t.Helper()
	ctx := context.Background()
	existing, err := m.stream.Consumer(ctx, m.wake)
	if err != nil {
		m.t.Fatal(err)
	}
	config := existing.CachedInfo().Config
	config.FilterSubject, config.FilterSubjects = "", []string{legionWakeSubject, m.projectWake}
	if _, err := m.stream.UpdateConsumer(ctx, config); err != nil {
		m.t.Fatalf("deploy 1's update of the wake consumer: %v", err)
	}
}

// drain delivers and acknowledges every message the wake consumer holds, as a running daemon of
// deploy 1 does, and answers each as <kind>-<role or session>.
func (m *wakeMigration) drain() []string {
	m.t.Helper()
	handle, err := m.stream.Consumer(context.Background(), m.wake)
	if err != nil {
		m.t.Fatal(err)
	}
	var got []string
	for {
		batch, err := handle.Fetch(10, jetstream.FetchMaxWait(time.Second))
		if err != nil {
			m.t.Fatal(err)
		}
		n := 0
		for message := range batch.Messages() {
			n++
			got = append(got, reportName(m.t, message.Data()))
			if err := message.DoubleAck(context.Background()); err != nil {
				m.t.Fatalf("acknowledge %s: %v", message.Subject(), err)
			}
		}
		if n == 0 {
			return got
		}
	}
}

// storedWakeReadClean is plan C's stored-wake read for this consumer: from its ack floor on, the
// shared subject holds listener_ready reports alone.
func (m *wakeMigration) storedWakeReadClean() bool {
	m.t.Helper()
	ctx := context.Background()
	info := consumerInfo(m.t, m.stream, m.wake)
	for seq := info.AckFloor.Stream + 1; ; {
		message, err := m.stream.GetMsg(ctx, seq, jetstream.WithGetMsgSubject(legionWakeSubject))
		if err != nil {
			return true
		}
		if name := reportName(m.t, message.Data); !strings.HasPrefix(name, ListenerWakeReady) {
			return false
		}
		seq = message.Sequence + 1
	}
}

// deploy4 boots this build's consumers and runs them until stop, applying every wake as a mark.
func (m *wakeMigration) deploy4() (stop func()) {
	m.t.Helper()
	consumers, err := OpenConsumers(context.Background(), m.js, m.pool, m.spec)
	if err != nil {
		m.t.Fatalf("this build's OpenConsumers: %v", err)
	}
	if info := consumerInfo(m.t, m.stream, m.wake); info.Config.FilterSubject != m.projectWake || len(info.Config.FilterSubjects) != 0 {
		m.t.Fatalf("the wake consumer filters %q %q after this build's boot, want exactly %s", info.Config.FilterSubject, info.Config.FilterSubjects, m.projectWake)
	}
	return runConsumers(m.t, consumers, wakeMarks())
}

// applied is every wake mark applied so far, sorted.
func (m *wakeMigration) applied() []string {
	m.t.Helper()
	var wakes []string
	for _, mark := range writeHandlers(m.t, m.pool) {
		if strings.HasPrefix(mark, "wake-") {
			wakes = append(wakes, mark)
		}
	}
	slices.Sort(wakes)
	return wakes
}

// reportName reads a wake report envelope as <kind>-<role or session>.
func reportName(t *testing.T, data []byte) string {
	t.Helper()
	var envelope struct {
		Payload string `json:"payload"`
	}
	var report struct {
		Kind, Role, Session string
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		t.Fatalf("read a wake envelope: %v", err)
	}
	if err := json.Unmarshal([]byte(envelope.Payload), &report); err != nil {
		t.Fatalf("read a wake report: %v", err)
	}
	return report.Kind + "-" + report.Role + report.Session
}

// A role wake and an agent wake published while the daemon is down survive each step of the wake
// migration and reach the daemon once: across deploy 1 (a role wake an old listener left on the
// shared subject), across deploy 2 (a role wake and an agent wake a moved listener put on the
// project's subject), past plan C's stored-wake read (step 3: nothing but listener_ready unread on
// the shared subject), and across this build's boot (an agent wake on the project's subject), and
// never twice across a restart after it.
//
// Controls, each failing for its own reason:
//   - one-step narrowing, with no deploy 1: a moved listener's role wake on the project's subject
//     is never delivered while the consumer hears the shared subject alone, a later liveness report
//     moves its position past it, and this build's boot then narrows in place behind it, so the
//     role wake is lost;
//   - step 4 before step 3: an agent wake an old listener left on the shared subject while the
//     daemon was down is lost by this build's boot, and the stored-wake read refuses first.
func TestAWakePublishedWhileTheDaemonIsDownSurvivesEachMigrationStep(t *testing.T) {
	t.Run("each step in order", func(t *testing.T) {
		m := newWakeMigration(t)

		publish(t, m.js, legionWakeSubject, wakeReport(t, "r1", ListenerWakeRole, "legion-capture-capture-1-architect"))
		m.deploy1()
		if got := m.drain(); !slices.Equal(got, []string{"role_message-legion-capture-capture-1-architect"}) {
			t.Fatalf("across deploy 1 the wake consumer delivered %q, want the role wake left on the shared subject, once", got)
		}

		publish(t, m.js, m.projectWake, wakeReport(t, "r2", ListenerWakeRole, "legion-capture-capture-2-architect"))
		publish(t, m.js, m.projectWake, wakeReport(t, "a2", ListenerWakeAgent, "ses_two"))
		publish(t, m.js, legionWakeSubject, wakeReport(t, "l2", ListenerWakeReady, ""))
		if got := m.drain(); !slices.Equal(got, []string{"role_message-legion-capture-capture-2-architect", "agent_message-ses_two", "listener_ready-"}) {
			t.Fatalf("across deploy 2 the wake consumer delivered %q, want the two wakes on the project's subject and the liveness report, once each", got)
		}

		publish(t, m.js, legionWakeSubject, wakeReport(t, "l3", ListenerWakeReady, ""))
		if !m.storedWakeReadClean() {
			t.Fatal("the stored-wake read refused with only a liveness report unread on the shared subject")
		}

		publish(t, m.js, m.projectWake, wakeReport(t, "a4", ListenerWakeAgent, "ses_four"))
		stop := m.deploy4()
		testwait.Eventually(t, "the agent wake published across this build's boot applied", func() bool {
			return slices.Contains(m.applied(), "wake-agent_message-ses_four")
		})
		waitDelivered(t, m.stream, m.wake)
		stop()
		stop = m.deploy4()
		waitDelivered(t, m.stream, m.wake)
		time.Sleep(time.Second)
		stop()
		if got := m.applied(); !slices.Equal(got, []string{"wake-agent_message-ses_four"}) {
			t.Fatalf("this build applied %q across its boot and a restart, want the agent wake once", got)
		}
	})

	t.Run("control: one-step narrowing loses the role wake", func(t *testing.T) {
		m := newWakeMigration(t)
		publish(t, m.js, m.projectWake, wakeReport(t, "r-moved", ListenerWakeRole, "legion-capture-capture-5-architect"))
		publish(t, m.js, legionWakeSubject, wakeReport(t, "l-after", ListenerWakeReady, ""))
		if got := m.drain(); !slices.Equal(got, []string{"listener_ready-"}) {
			t.Fatalf("today's consumer delivered %q, want only the liveness report on its shared subject", got)
		}
		stop := m.deploy4()
		waitDelivered(t, m.stream, m.wake)
		time.Sleep(time.Second)
		stop()
		if slices.Contains(m.applied(), "wake-role_message-legion-capture-capture-5-architect") {
			t.Fatal("one-step narrowing applied the role wake behind the consumer's position, so this control shows nothing")
		}
	})

	t.Run("control: step 4 before step 3 loses the agent wake", func(t *testing.T) {
		m := newWakeMigration(t)
		m.deploy1()
		publish(t, m.js, legionWakeSubject, wakeReport(t, "a-old", ListenerWakeAgent, "ses_old"))
		if m.storedWakeReadClean() {
			t.Fatal("the stored-wake read passed with an agent wake unread on the shared subject")
		}
		stop := m.deploy4()
		waitDelivered(t, m.stream, m.wake)
		waitDelivered(t, m.stream, readyConsumerName(m.spec.Project))
		time.Sleep(time.Second)
		stop()
		if slices.Contains(m.applied(), "wake-agent_message-ses_old") {
			t.Fatal("this build applied the agent wake left unread on the shared subject, so this control shows nothing")
		}
	})
}
