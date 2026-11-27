package daemon

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// storedEnvelope is the envelope the listener stores for published, encoded.
func storedEnvelope(t *testing.T, published listenerPublish) []byte {
	t.Helper()
	minted := hex.EncodeToString(make([]byte, 16))
	encoded, err := json.Marshal(listenerEnvelope{EventID: minted, Source: "agent", SourceEventID: minted, Topic: published.Topic,
		DedupeKey: published.DedupeKey, IssuedAt: time.Now().UnixMilli(), PayloadSummary: published.Message, Payload: published.Payload, TraceID: minted})
	if err != nil {
		t.Fatalf("encode the envelope: %v", err)
	}
	return encoded
}

// seedBootStore seeds by SQL the store cfg's daemon boots over: issue, which the project shipped,
// and with earlier the state row of its last write, which the run earlierRun wrote and never
// published. It answers the sequence that row's commit gave it, 0 without it. The store is migrated
// first, so the boot applies no migration.
func seedBootStore(t *testing.T, cfg config.Config, issue string, earlier bool, earlierRun string) int64 {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, cfg.PostgresDSN)
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}
	defer st.Close()
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if _, err := st.Pool().Exec(ctx, `insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq, repo, default_branch)
		values ($1, $1, $2, 'shipped', 'done', 1, 'done', 'A', 1, 'acme/widgets', 'main')`, issue, cfg.Project); err != nil {
		t.Fatalf("seed the issue: %v", err)
	}
	if !earlier {
		return 0
	}
	payload := fmt.Sprintf(`{"project":%q,"run":%q,"issue":%q,"changes":[{"field":"phase","old":"merging","new":"done"}]}`, cfg.Project, earlierRun, issue)
	if _, err := st.Pool().Exec(ctx, `insert into outbox (kind, issue, payload, attempts, next_at, last_error)
		values ($1, $2, $3, 0, now(), '')`, string(record.OutboxKindStateChange), cfg.Project, payload); err != nil {
		t.Fatalf("seed the earlier run's state row: %v", err)
	}
	var seq int64
	if err := st.Pool().QueryRow(ctx, `select state_seq from outbox where issue = $1 and kind = $2`,
		cfg.Project, string(record.OutboxKindStateChange)).Scan(&seq); err != nil {
		t.Fatalf("read the earlier run's state row: %v", err)
	}
	if seq != 1 {
		t.Fatalf("the earlier run's row took sequence %d at its commit, want the project's first", seq)
	}
	return seq
}

// A booted daemon's first state event is its boot event (record.StateBoot), published on the
// project's state topic through the Envoy listener: one message of its own, carrying the settings
// and the schema version GET /legion/v1/state serves, the run id the boot logged, the sequence its
// commit took and the store id the state serves, and nothing of the state itself, its envelope as
// the listener stores it well under the 1 MiB a NATS server takes whole. The state serves that run
// id as `daemon.stateRun`, the run of every message this daemon run publishes, and not the run of
// a message an earlier run left. A state row an earlier run left unpublished holds a lower
// sequence, so the lane publishes it first, and the boot event follows it one sequence above.
func TestABootedDaemonPublishesItsBootEventFirst(t *testing.T) {
	const earlierRun = "1c9e4b7a-6d2f-4a83-9b05-e8f7d3c2a160"
	for _, tc := range []struct {
		name    string
		earlier bool
	}{
		{name: "a project with no state event yet"},
		{name: "a state event an earlier run left unpublished", earlier: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := workflowConfig(t, workflowNATS(t))
			listener := newNoticeListener()
			envoy := httptest.NewServer(listener)
			t.Cleanup(envoy.Close)
			cfg.EnvoyURL = envoy.URL
			token, err := claim.ProjectToken(cfg.Project)
			if err != nil {
				t.Fatal(err)
			}
			topic := notify.StateTopic(token)

			issue := cfg.Project + "-1"
			earlierSeq := seedBootStore(t, cfg, issue, tc.earlier, earlierRun)

			var logs syncBuffer
			o := fakeRuntime(fake.NewRuntime(), &built{})
			o.workflowTokens = &workflowTokenRecorder{}
			d := startDaemonLogging(t, cfg, o, slog.New(slog.NewJSONHandler(&logs, nil)))
			messages := 1
			if tc.earlier {
				messages = 2
			}
			testwait.Eventually(t, "the boot event published on the state topic", func() bool {
				return len(listener.acceptedOn(topic)) >= messages
			})
			accepted := listener.acceptedOn(topic)
			var states []publishedState
			for _, published := range accepted[:messages] {
				states = append(states, decodeState(t, published.Topic, published.Message, published.DedupeKey, []byte(published.Payload)))
			}
			if tc.earlier {
				left := states[0]
				if left.message.Run != earlierRun || left.message.FirstSeq != earlierSeq || left.message.Seq != earlierSeq ||
					left.events[0].Kind != "change" || left.events[0].Issue != issue {
					t.Fatalf("the first message = %+v, want the earlier run's change of %s at sequence %d", left, issue, earlierSeq)
				}
			}
			boot := states[messages-1]
			lines := loggedLines(t, logs.String(), "legion daemon state events on")
			if len(lines) != 1 {
				t.Fatalf("the boot logged %d lines naming its run, want one", len(lines))
			}
			if run, _ := lines[0]["run"].(string); boot.message.Run != run || boot.message.Project != cfg.Project {
				t.Fatalf("the boot event names run %q of project %q, want the run the boot logged, %q, of %s", boot.message.Run, boot.message.Project, run, cfg.Project)
			}
			if boot.message.FirstSeq != earlierSeq+1 || boot.message.Seq != earlierSeq+1 || boot.events[0].Kind != "boot" {
				t.Fatalf("the boot's message carries %d-%d, first event %q, want the boot event alone at sequence %d, one above every row before this run",
					boot.message.FirstSeq, boot.message.Seq, boot.events[0].Kind, earlierSeq+1)
			}
			// Nothing of the state rides with it: the event carries its sequence, its kind, the
			// settings and the store's schema version, and no other key.
			var keys map[string]json.RawMessage
			if err := json.Unmarshal(boot.message.Events[0], &keys); err != nil {
				t.Fatal(err)
			}
			if got := slices.Sorted(maps.Keys(keys)); !reflect.DeepEqual(got, []string{"kind", "schemaVersion", "seq", "settings"}) {
				t.Fatalf("the boot event carries the keys %v, want seq, kind, settings and schemaVersion alone", got)
			}
			status, body := d.request(http.MethodGet, "/legion/v1/state", nil, false)
			if status != http.StatusOK {
				t.Fatalf("GET the state = %d %s", status, body)
			}
			var state struct {
				Settings json.RawMessage `json:"settings"`
				Daemon   struct {
					SchemaVersion int64  `json:"schemaVersion"`
					StateSeq      *int64 `json:"stateSeq"`
					StateID       string `json:"stateId"`
					StateRun      string `json:"stateRun"`
				} `json:"daemon"`
			}
			if err := json.Unmarshal(body, &state); err != nil {
				t.Fatalf("decode the state: %v", err)
			}
			if got, want := decodedJSON(t, boot.events[0].Settings), decodedJSON(t, state.Settings); !reflect.DeepEqual(got, want) {
				t.Fatalf("the boot event's settings = %s, want the settings the state serves, %s", boot.events[0].Settings, state.Settings)
			}
			// The schema version is the one the store records after the boot's migrations, which
			// the state serves and the boot's line names.
			if logged, _ := lines[0]["schemaVersion"].(float64); state.Daemon.SchemaVersion < 1 || *boot.events[0].SchemaVersion != state.Daemon.SchemaVersion ||
				int64(logged) != state.Daemon.SchemaVersion {
				t.Fatalf("the boot event carries schema version %d and the boot logged %v, want the state's daemon.schemaVersion %d",
					*boot.events[0].SchemaVersion, lines[0]["schemaVersion"], state.Daemon.SchemaVersion)
			}
			if state.Daemon.StateSeq == nil || *state.Daemon.StateSeq < boot.message.Seq {
				t.Fatalf("the state serves daemon.stateSeq %v, want at least the boot event's %d, which committed before the API served", state.Daemon.StateSeq, boot.message.Seq)
			}
			// The state serves the run that published the boot, which a reader takes as its
			// starting point's run: a boot of that run at or below the state's stateSeq is one
			// the state already holds.
			if state.Daemon.StateRun != boot.message.Run {
				t.Fatalf("the state serves daemon.stateRun %q, want the run of the boot event this daemon published, %q", state.Daemon.StateRun, boot.message.Run)
			}
			// Every message counted in this store carries its one id, the earlier run's left row
			// among them, and the state serves the same id.
			for _, published := range states {
				if published.message.StateID != state.Daemon.StateID {
					t.Fatalf("a message of the store carries stateId %q, want the daemon.stateId %q the state serves", published.message.StateID, state.Daemon.StateID)
				}
			}
			envelope := storedEnvelope(t, accepted[messages-1])
			if len(envelope) >= 1<<20 {
				t.Fatalf("the boot event's envelope takes %d bytes as the listener stores it, want under 1 MiB", len(envelope))
			}
			t.Logf("the boot event's envelope, as the listener's publish route stores it, takes %d bytes", len(envelope))
		})
	}
}

// A daemon booted with no dispatch_url runs no workflow, so it runs no outbox that would send a
// state row and turns no state event on (run): through its whole run it writes no row of the state
// lane, publishes no boot event on the project's state topic, though its Envoy listener takes every
// publish, and GET /legion/v1/state serves no `daemon.stateRun`, the run a reader takes a starting
// point's messages from.
func TestADaemonWithNoDispatchURLWritesAndPublishesNoStateEvent(t *testing.T) {
	cfg := testConfig(t)
	listener := newNoticeListener()
	envoy := httptest.NewServer(listener)
	t.Cleanup(envoy.Close)
	cfg.EnvoyURL = envoy.URL
	token, err := claim.ProjectToken(cfg.Project)
	if err != nil {
		t.Fatal(err)
	}

	d := startDaemon(t, cfg, fakeRuntime(fake.NewRuntime(), &built{}))
	status, body := d.request(http.MethodGet, "/legion/v1/state", nil, false)
	if status != http.StatusOK {
		t.Fatalf("GET the state = %d %s", status, body)
	}
	var state struct {
		Daemon map[string]json.RawMessage `json:"daemon"`
	}
	if err := json.Unmarshal(body, &state); err != nil || state.Daemon == nil {
		t.Fatalf("decode the state %s: %v", body, err)
	}
	if run, served := state.Daemon["stateRun"]; served {
		t.Fatalf("the state serves daemon.stateRun %s, want none: a daemon with no workflow publishes no state event", run)
	}
	d.stop()

	// Read once the run has returned, so every write and every publish of the run is counted.
	st, err := store.Open(context.Background(), cfg.PostgresDSN)
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}
	defer st.Close()
	var rows int
	if err := st.Pool().QueryRow(context.Background(), `select count(*) from outbox where kind = any($1)`, stateKindNames()).Scan(&rows); err != nil {
		t.Fatalf("count the state rows: %v", err)
	}
	if rows != 0 {
		t.Fatalf("the daemon's run wrote %d state rows, want none: a daemon with no workflow writes no state event", rows)
	}
	if published := listener.acceptedOn(notify.StateTopic(token)); len(published) != 0 {
		t.Fatalf("the daemon published %+v on its state topic, want nothing: a daemon with no workflow publishes no boot event", published)
	}
}
