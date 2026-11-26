package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/admit"
	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/blockers"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/testpg/testdb"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// intakeOperatorToken is the bearer of the daemons the `legion intake` tests reach.
const intakeOperatorToken = "intake-operator-bearer-for-tests"

// intakeDaemon is the daemon's HTTP surface as `legion intake` reaches it: the real api.NewServer,
// serving the give-up routes over giveUps, on a loopback port, with every request that reached it
// recorded.
type intakeDaemon struct {
	*recordingDaemon
	t         *testing.T
	tokenFile string
}

func newIntakeDaemon(t *testing.T, giveUps api.GiveUps) *intakeDaemon {
	t.Helper()
	handler := api.NewServer("127.0.0.1", 0, api.Options{
		OperatorToken: intakeOperatorToken, Intake: giveUps, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}).Handler
	d := &intakeDaemon{t: t, tokenFile: writeFile(t, "operator-token", intakeOperatorToken+"\n")}
	d.recordingDaemon = serveRecorded(t, handler)
	return d
}

// intake runs `legion intake <args>` against the daemon, with the flags that reach it, and holds
// it to the rule every operator command keeps: the operator token is in neither stream.
func (d *intakeDaemon) intake(args ...string) (int, string, string) {
	d.t.Helper()
	var out, errb bytes.Buffer
	argv := append([]string{"legion", "intake"}, args...)
	argv = append(argv, "--port", strconv.Itoa(d.port), "--operator-token-file", d.tokenFile)
	code := run(context.Background(), argv, &out, &errb)
	for name, stream := range map[string]string{"stdout": out.String(), "stderr": errb.String()} {
		if strings.Contains(stream, intakeOperatorToken) {
			d.t.Fatalf("legion intake %v printed the operator token on %s: %q", args, name, stream)
		}
	}
	return code, out.String(), errb.String()
}

// standInGiveUps is a stand-in for the replay and the discard: it records what each call was
// handed and answers what the test set.
type standInGiveUps struct {
	mu      sync.Mutex
	calls   []string
	replay  intake.ReplayAnswer
	discard []int64
	err     error
}

func (g *standInGiveUps) Replay(_ context.Context, selector record.IntakeSelector, caller string) (intake.ReplayAnswer, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls = append(g.calls, fmt.Sprintf("replay %+v by %s", selector, caller))
	return g.replay, g.err
}

func (g *standInGiveUps) Discard(_ context.Context, selector record.IntakeSelector, caller, reason string) ([]int64, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls = append(g.calls, fmt.Sprintf("discard %+v by %s for %s", selector, caller, reason))
	return g.discard, g.err
}

func (g *standInGiveUps) made() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.calls)
}

// `legion intake replay` prints one line per record the replay took, how it ended and, for one
// that failed, its error, then how many open give-ups of the selection are left; it exits 1 when a
// record failed, since that give-up is still open, and 0 when none did. A refusal of the daemon is
// printed as every operator command prints one, and exits 1.
func TestIntakeReplayPrintsEachOutcomeAndExitsOneOnAFailure(t *testing.T) {
	t.Parallel()
	const consumer = "legion-go-LEGION-dispatch"
	t.Run("a record failed", func(t *testing.T) {
		d := newIntakeDaemon(t, &standInGiveUps{replay: intake.ReplayAnswer{Replayed: []intake.ReplayedGiveUp{
			{ID: 41, Consumer: consumer, StreamSeq: 10, Outcome: intake.ReplayApplied},
			{ID: 42, Consumer: consumer, StreamSeq: 20, Outcome: intake.ReplayAlreadyApplied},
			{ID: 43, Consumer: consumer, StreamSeq: 30, Outcome: intake.ReplayNoFact},
			{ID: 44, Consumer: consumer, StreamSeq: 40, Outcome: intake.ReplayFailed, Error: "decode Envoy envelope: unexpected end of JSON input"},
		}, Remaining: 1}})
		code, out, errb := d.intake("replay", "--consumer", consumer, "--through-seq", "40")
		want := "41 legion-go-LEGION-dispatch seq 10: applied\n" +
			"42 legion-go-LEGION-dispatch seq 20: already applied\n" +
			"43 legion-go-LEGION-dispatch seq 30: no fact\n" +
			"44 legion-go-LEGION-dispatch seq 40: failed: decode Envoy envelope: unexpected end of JSON input\n" +
			"remaining 1\n"
		if code != 1 || out != want || errb != "" {
			t.Fatalf("legion intake replay = %d, stdout %q, stderr %q; want 1 and %q", code, out, errb, want)
		}
	})
	t.Run("none failed", func(t *testing.T) {
		d := newIntakeDaemon(t, &standInGiveUps{replay: intake.ReplayAnswer{Replayed: []intake.ReplayedGiveUp{
			{ID: 41, Consumer: consumer, StreamSeq: 10, Outcome: intake.ReplayApplied},
		}}})
		code, out, errb := d.intake("replay", "--give-up", "41")
		if want := "41 legion-go-LEGION-dispatch seq 10: applied\nremaining 0\n"; code != 0 || out != want || errb != "" {
			t.Fatalf("legion intake replay = %d, stdout %q, stderr %q; want 0 and %q", code, out, errb, want)
		}
	})
	// A replay's error can carry a publisher's text, and the terminal shows its control
	// characters as escapes, as it shows a give-up's in `legion state`.
	t.Run("an error with control characters", func(t *testing.T) {
		d := newIntakeDaemon(t, &standInGiveUps{replay: intake.ReplayAnswer{Replayed: []intake.ReplayedGiveUp{
			{ID: 44, Consumer: consumer, StreamSeq: 40, Outcome: intake.ReplayFailed, Error: "decode \x1b[2J envelope\x07\nsecond line"},
		}}})
		code, out, errb := d.intake("replay", "--give-up", "44")
		want := "44 legion-go-LEGION-dispatch seq 40: failed: decode \\x1b[2J envelope\\a\\nsecond line\nremaining 0\n"
		if code != 1 || out != want || errb != "" {
			t.Fatalf("legion intake replay = %d, stdout %q, stderr %q; want 1 and %q", code, out, errb, want)
		}
	})
	t.Run("the daemon refuses", func(t *testing.T) {
		d := newIntakeDaemon(t, &standInGiveUps{err: record.ErrIntakeGiveUpNotFound})
		code, out, errb := d.intake("replay", "--give-up", "41")
		want := "legion intake replay: the daemon answered 404 Not Found: give-up 41: " + record.ErrIntakeGiveUpNotFound.Error() + "\n"
		if code != 1 || out != "" || errb != want {
			t.Fatalf("legion intake replay = %d, stdout %q, stderr %q; want 1 and %q", code, out, errb, want)
		}
	})
}

// A discard names why its records are let go, which each record keeps: `legion intake discard`
// without --reason is a usage error, exit 2, and nothing reaches the daemon.
func TestIntakeDiscardRefusesAMissingReason(t *testing.T) {
	t.Parallel()
	stand := &standInGiveUps{}
	d := newIntakeDaemon(t, stand)
	code, out, errb := d.intake("discard", "--give-up", "41")
	if want := "legion intake discard: --reason is required"; code != 2 || out != "" || !strings.Contains(errb, want) {
		t.Fatalf("legion intake discard = %d, stdout %q, stderr %q; want a usage error saying %q", code, out, errb, want)
	}
	if n := len(d.requests()); n != 0 || len(stand.made()) != 0 {
		t.Fatalf("%d requests reached the daemon and the give-ups were called %v, want neither", n, stand.made())
	}
}

// Each command posts its selection to the operator's route under the operator's bearer: --give-up
// as one id, --consumer and --through-seq as a consumer's records at or below that sequence, and
// the discard's --reason beside it; the daemon hands the give-ups that selection with the operator
// as the caller. A selection of neither form, of both, or of half the second is a usage error, exit
// 2, that sends nothing.
func TestIntakeCommandsSendTheirSelector(t *testing.T) {
	t.Parallel()
	const consumer = "legion-go-LEGION-dispatch"
	for _, row := range []struct {
		name   string
		args   []string
		path   string
		body   string
		called string
		out    string
	}{
		{"a replay of one id", []string{"replay", "--give-up", "41"}, "/legion/v1/operator/intake/replay",
			`{"giveUp":41}`, "replay {ID:41 Consumer: ThroughSeq:0} by operator", "remaining 0\n"},
		{"a replay of a consumer's records", []string{"replay", "--consumer", consumer, "--through-seq", "6044896"}, "/legion/v1/operator/intake/replay",
			`{"consumer":"legion-go-LEGION-dispatch","throughSeq":6044896}`, "replay {ID:0 Consumer:legion-go-LEGION-dispatch ThroughSeq:6044896} by operator", "remaining 0\n"},
		{"a discard of one id", []string{"discard", "--give-up", "41", "--reason", "the issue was deleted"}, "/legion/v1/operator/intake/discard",
			`{"giveUp":41,"reason":"the issue was deleted"}`, "discard {ID:41 Consumer: ThroughSeq:0} by operator for the issue was deleted", "discarded 41\ndiscarded 42\n"},
		{"a discard of a consumer's records", []string{"discard", "--consumer", consumer, "--through-seq", "9", "--reason", "a bad deploy"}, "/legion/v1/operator/intake/discard",
			`{"consumer":"legion-go-LEGION-dispatch","throughSeq":9,"reason":"a bad deploy"}`, "discard {ID:0 Consumer:legion-go-LEGION-dispatch ThroughSeq:9} by operator for a bad deploy", "discarded 41\ndiscarded 42\n"},
	} {
		t.Run(row.name, func(t *testing.T) {
			stand := &standInGiveUps{discard: []int64{41, 42}}
			d := newIntakeDaemon(t, stand)
			code, out, errb := d.intake(row.args...)
			if code != 0 || errb != "" || out != row.out {
				t.Fatalf("legion intake %v = %d, stdout %q, stderr %q; want 0 and %q", row.args, code, out, errb, row.out)
			}
			requests := d.requests()
			if len(requests) != 1 {
				t.Fatalf("%d requests reached the daemon, want one", len(requests))
			}
			sent := requests[0]
			if sent.method != http.MethodPost || sent.path != row.path || sent.authorization != "Bearer "+intakeOperatorToken ||
				sent.contentType != "application/json" || string(sent.body) != row.body {
				t.Fatalf("the request = %s %s auth %q type %q body %s; want POST %s under the operator's bearer with %s",
					sent.method, sent.path, sent.authorization, sent.contentType, sent.body, row.path, row.body)
			}
			if calls := stand.made(); !slices.Equal(calls, []string{row.called}) {
				t.Fatalf("the give-ups were called %v, want %q", calls, row.called)
			}
		})
	}
	for _, usage := range []struct {
		name string
		args []string
		says string
	}{
		{"neither form", []string{"replay"}, "name --give-up <id>, or --consumer <name> with --through-seq <seq>"},
		{"both forms", []string{"replay", "--give-up", "41", "--consumer", consumer, "--through-seq", "9"}, "name --give-up, or --consumer with --through-seq, not both"},
		{"a consumer without a sequence", []string{"discard", "--consumer", consumer, "--reason", "r"}, "give --consumer and --through-seq together"},
		{"a sequence without a consumer", []string{"replay", "--through-seq", "9"}, "give --consumer and --through-seq together"},
	} {
		t.Run(usage.name, func(t *testing.T) {
			stand := &standInGiveUps{}
			d := newIntakeDaemon(t, stand)
			code, _, errb := d.intake(usage.args...)
			if want := "legion intake " + usage.args[0] + ": " + usage.says; code != 2 || !strings.Contains(errb, want) {
				t.Fatalf("legion intake %v = %d, stderr %q; want a usage error saying %q", usage.args, code, errb, want)
			}
			if n := len(d.requests()); n != 0 {
				t.Fatalf("%d requests reached the daemon, want none", n)
			}
		})
	}
}

// resolvedRepositories serves every repository at main, which is all the workflow's engine asks of
// the repositories for the issue events these tests replay.
type resolvedRepositories struct{}

func (resolvedRepositories) ResolveRepository(resolved string) (workflow.Resolution, error) {
	return workflow.Resolution{Repo: resolved, DefaultBranch: "main", GitHubRepo: resolved}, nil
}

// `legion intake replay` and `legion intake discard` reach the give-ups a daemon recorded, through
// the real routes and the real replay and discard on PostgreSQL, applying with the daemon's own
// handler chain: the replay applies a recorded give-up and prints `applied`, and the discard closes
// another and prints its id, each read back from the table as closed by the operator.
func TestIntakeCommandsReplayAndDiscardRecordedGiveUps(t *testing.T) {
	t.Parallel()
	const project = "LEGION"
	ctx := context.Background()
	st, err := store.Open(ctx, testdb.Database(t, "legion_intake_commands"))
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}
	t.Cleanup(st.Close)
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	records := record.NewStore()
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	engine := workflow.New(records, workflow.Config{Project: project, Repositories: resolvedRepositories{}, Steps: config.DesignGateRootIssues.Steps()}, quiet)
	giveUps, err := intake.NewGiveUps(intake.Records{Pool: st.Pool(), Spec: intake.ConsumerSpec{Project: project, Logger: quiet},
		Repositories: func() []ghrepo.Repository { return nil }},
		blockers.NewMirror(records, project), engine, admit.New(records, engine, 2, project, quiet))
	if err != nil {
		t.Fatalf("NewGiveUps: %v", err)
	}
	d := newIntakeDaemon(t, giveUps)

	given := func(key string, seq uint64) record.IntakeFailure {
		t.Helper()
		event := fmt.Sprintf(`{"id":30,"issue_key":%q,"seq":3,"type":"issue.updated","notify":true,"actor":{"kind":"user","id":"a-person"},`+
			`"payload":{"key":%q,"status":"backlog","title":"an event given up","rank":"A","parent":null,"labels":[],"resolved_repo":"acme/widgets"}}`, key, key)
		envelope := fmt.Sprintf(`{"event_id":"dispatch-%s","source":"dispatch","source_event_id":"30","topic":"issue","dedupe_key":"dispatch-%s",`+
			`"issued_at":1,"payload_summary":"%s issue updated","payload":%s,"trace_id":"dispatch-%s"}`, key, key, key, strconv.Quote(event), key)
		message := record.IntakeMessage{Project: project, Consumer: "legion-go-LEGION-dispatch", Subject: "notifications.dispatch.issue." + key + ".issue.updated",
			StreamSeq: seq, StoredAt: time.Now().Add(-3 * time.Hour).UTC(), Payload: []byte(envelope), EventSource: "dispatch", EventID: "dispatch-" + key}
		var failure record.IntakeFailure
		if err := store.Transact(ctx, st.Pool(), func(ctx context.Context, tx pgx.Tx) error {
			noted, err := records.NoteFailure(ctx, tx, message, 21, "the handler failed", time.Now().Add(-2*time.Hour))
			if err != nil {
				return err
			}
			failure, _, err = records.GiveUpIntakeMessage(ctx, tx, project, noted.ID, record.GiveUpBound, "the handler failed", time.Now().Add(-time.Hour))
			return err
		}); err != nil {
			t.Fatalf("give up %s's event: %v", key, err)
		}
		return failure
	}
	closed := func(id int64) (how, by, reason string) {
		t.Helper()
		if err := st.Pool().QueryRow(ctx, `select coalesce(closed_how, ''), coalesce(closed_by, ''), coalesce(discard_reason, '')
			from intake_failures where id = $1`, id).Scan(&how, &by, &reason); err != nil {
			t.Fatalf("read give-up %d: %v", id, err)
		}
		return how, by, reason
	}
	replayed, discarded := given("LEGION-501", 11), given("LEGION-502", 12)

	code, out, errb := d.intake("replay", "--give-up", strconv.FormatInt(replayed.ID, 10))
	if want := fmt.Sprintf("%d legion-go-LEGION-dispatch seq 11: applied\nremaining 0\n", replayed.ID); code != 0 || out != want || errb != "" {
		t.Fatalf("legion intake replay = %d, stdout %q, stderr %q; want 0 and %q", code, out, errb, want)
	}
	if how, by, _ := closed(replayed.ID); how != string(record.IntakeApplied) || by != "operator" {
		t.Errorf("the replayed give-up closed as %q by %q, want applied by the operator", how, by)
	}

	code, out, errb = d.intake("discard", "--give-up", strconv.FormatInt(discarded.ID, 10), "--reason", "the issue was deleted")
	if want := fmt.Sprintf("discarded %d\n", discarded.ID); code != 0 || out != want || errb != "" {
		t.Fatalf("legion intake discard = %d, stdout %q, stderr %q; want 0 and %q", code, out, errb, want)
	}
	if how, by, reason := closed(discarded.ID); how != string(record.IntakeDiscarded) || by != "operator" || reason != "the issue was deleted" {
		t.Errorf("the discarded give-up closed as %q by %q for %q, want discarded by the operator for its reason", how, by, reason)
	}
}
