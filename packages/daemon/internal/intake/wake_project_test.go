package intake

import (
	"context"
	"log/slog"
	"reflect"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/nats-io/nats.go/jetstream"
)

// wakeDelivery is one delivery of the wake durable, standing in for JetStream's message:
// consumeMessage reads its subject, its data and its metadata, and answers it with an
// acknowledgement, a nak or a term, which the test reads back.
type wakeDelivery struct {
	jetstream.Msg
	data []byte

	mu     sync.Mutex
	acks   int
	delays []time.Duration
	// others are the answers that are neither an acknowledgement nor a nak with a delay.
	others []string
}

func (d *wakeDelivery) Subject() string { return legionWakeSubject }
func (d *wakeDelivery) Data() []byte    { return d.data }
func (d *wakeDelivery) Metadata() (*jetstream.MsgMetadata, error) {
	return &jetstream.MsgMetadata{Consumer: "legion-go-INFRA-wake", NumDelivered: 1,
		Sequence: jetstream.SequencePair{Stream: 1, Consumer: 1}, Timestamp: time.Now()}, nil
}

// InProgress is the keeper's signal (keepInProgress), which runs beside the handler and is no
// answer to the delivery.
func (d *wakeDelivery) InProgress() error { return nil }

func (d *wakeDelivery) Ack() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.acks++
	return nil
}

func (d *wakeDelivery) NakWithDelay(delay time.Duration) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.delays = append(d.delays, delay)
	return nil
}

func (d *wakeDelivery) Nak() error                      { return d.other("nak") }
func (d *wakeDelivery) Term() error                     { return d.other("term") }
func (d *wakeDelivery) TermWithReason(string) error     { return d.other("term") }
func (d *wakeDelivery) DoubleAck(context.Context) error { return d.other("double ack") }

func (d *wakeDelivery) other(answer string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.others = append(d.others, answer)
	return nil
}

// answers is what consumeMessage answered the delivery: the acknowledgements, the delays of its
// naks, and every other answer.
func (d *wakeDelivery) answers() (int, []time.Duration, []string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.acks, d.delays, d.others
}

// wakeSpec is the wake durable's specification for project INFRA, logging to logs. HandlerLimit
// is set because consumeMessage reads it as given, and a zero limit would cut every handler.
func wakeSpec(logs *lockedBuffer) ConsumerSpec {
	return ConsumerSpec{Project: "INFRA", AckWait: time.Second, NakDelay: time.Second, HandlerLimit: time.Minute,
		Logger: slog.New(slog.NewTextHandler(logs, nil))}
}

// R6: a daemon reads every report on the wake subject, and a role report of another project is
// no fact. The consumer acknowledges it with no transaction, so it leaves no processed_events row.
// An agent report names a session and no project, and a listener_ready report names neither, so
// both decode as before. The daemon's project is INFRA, whose token is infra. A role is read in
// either form of its name: the token alone, or a project and then the token. A role of INFRA named
// under INFRA is a wake of its token. In the new form, a role of another project is no fact, and
// so is a role named under another project.
func TestAWakeReportForAnotherProjectIsNoFact(t *testing.T) {
	at := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	millis := strconv.FormatInt(at.UnixMilli(), 10)
	roleReport := func(token string) string { return `{"kind":"role_message","role":"` + token + `","at":` + millis + `}` }
	decode := func(t *testing.T, id, payload string) Fact {
		t.Helper()
		decoded, err := decodeMessage(legionWakeSubject, "INFRA", nil, envelopeJSON(t, id, "envoy", payload))
		if err != nil {
			t.Fatalf("decode the wake report %s: %v", payload, err)
		}
		return decoded.Fact
	}

	t.Run("(a) a role of the project bet is no fact and is acknowledged with no row", func(t *testing.T) {
		payload := roleReport("legion-bet-bet-12-implementer")
		if fact := decode(t, "wake-bet", payload); fact != nil {
			t.Fatalf("decoded fact = %#v, want none for a role of another project", fact)
		}

		pool := migratedPool(t)
		called := 0
		handler := handlerFunc(func(context.Context, pgx.Tx, Fact) (Result, error) {
			called++
			return Result{}, nil
		})
		delivery := &wakeDelivery{data: envelopeJSON(t, "wake-bet", "envoy", payload)}
		if !consumeMessage(context.Background(), delivery, wakeSpec(&lockedBuffer{}), nil, pool, []Handler{handler}, &issueNames{}, newDroppedSettlements()) {
			t.Fatal("consumeMessage = false, want the report handled")
		}
		acks, delays, others := delivery.answers()
		if acks != 1 || len(delays) != 0 || len(others) != 0 {
			t.Fatalf("answers = %d acknowledgements, naks after %v, others %v; want one acknowledgement and nothing else", acks, delays, others)
		}
		if called != 0 {
			t.Fatalf("the handler ran %d times, want never for another project's role report", called)
		}
		var processed bool
		if err := pool.QueryRow(context.Background(), `select exists (select 1 from processed_events where event_id = $1)`,
			"wake-bet").Scan(&processed); err != nil {
			t.Fatalf("read processed_events: %v", err)
		}
		if processed {
			t.Fatal("processed_events holds the report of another project's role, want no row")
		}
	})

	t.Run("(b) a role of the project infra is a wake", func(t *testing.T) {
		const token = "legion-infra-infra-12-implementer"
		want := ListenerWake{Kind: ListenerWakeRole, Role: token, At: at, Report: "wake-infra"}
		if fact := decode(t, "wake-infra", roleReport(token)); !reflect.DeepEqual(fact, want) {
			t.Fatalf("decoded fact = %#v, want %#v", fact, want)
		}
	})

	t.Run("(c) a role of the project infrax is no fact", func(t *testing.T) {
		if fact := decode(t, "wake-infrax", roleReport("legion-infrax-infrax-12-implementer")); fact != nil {
			t.Fatalf("decoded fact = %#v, want none: infrax is another project whose token begins with infra", fact)
		}
	})

	t.Run("(d) an agent report and a listener_ready report are wakes", func(t *testing.T) {
		agent := ListenerWake{Kind: ListenerWakeAgent, Session: "session-1", At: at, Report: "wake-agent"}
		if fact := decode(t, "wake-agent", `{"kind":"agent_message","session":"session-1","at":`+millis+`}`); !reflect.DeepEqual(fact, agent) {
			t.Fatalf("decoded agent report = %#v, want %#v", fact, agent)
		}
		ready := ListenerWake{Kind: ListenerWakeReady, At: at, Report: "wake-ready"}
		if fact := decode(t, "wake-ready", `{"kind":"listener_ready","at":`+millis+`}`); !reflect.DeepEqual(fact, ready) {
			t.Fatalf("decoded listener_ready report = %#v, want %#v", fact, ready)
		}
	})

	t.Run("(e) a role of the project infra named under INFRA is a wake of its token", func(t *testing.T) {
		const token = "legion-infra-infra-12-implementer"
		want := ListenerWake{Kind: ListenerWakeRole, Role: token, At: at, Report: "wake-infra-new-form"}
		if fact := decode(t, "wake-infra-new-form", roleReport("INFRA."+token)); !reflect.DeepEqual(fact, want) {
			t.Fatalf("decoded fact = %#v, want %#v", fact, want)
		}
	})

	t.Run("(f) in the new form, a role of another project or under another project is no fact", func(t *testing.T) {
		for _, role := range []string{
			"BET.legion-bet-bet-12-implementer",
			"INFRA.legion-bet-bet-12-implementer",
			"BET.legion-infra-infra-12-implementer",
		} {
			if fact := decode(t, "wake-"+role, roleReport(role)); fact != nil {
				t.Errorf("decoded fact for %s = %#v, want none", role, fact)
			}
		}
	})
}
