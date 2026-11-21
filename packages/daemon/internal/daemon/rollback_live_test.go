//go:build e2e

// The rollback proof of this change's notices: a daemon of the release before this change starts on
// a database this change migrated and wrote to, runs its outbox, and leaves every notice it cannot
// decode as it stands, and this change's daemon then delivers each of them.
// scripts/e2e/rollback-notices.sh drives it, over four runs of this package's `-tags e2e` test
// binary on one database and one nats-server: the release before records the fixture
// (TestARollbackFixtureIsRecordedByTheReleaseBeforeLive), this change migrates and seeds it
// (rollback_forward_live_test.go), the release before runs on it again (the rollback step), and
// this change rolls forward (rollback_forward_live_test.go). The release before's two runs are
// built from its own tree with this file copied in, so everything here uses what that release's
// harness already has, and nothing this change adds: the notice kinds are written out as text,
// and the rows are read with SQL.
package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testnats"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
)

// rollbackDSNEnv names the database the three runs share, and rollbackProjectEnv the project key
// they share: the rows each run reads are the ones the run before it left. Both carry the
// LEGION_E2E prefix, which the package's environment guard keeps (fixtureenv's testInput).
const (
	rollbackDSNEnv     = "LEGION_E2E_ROLLBACK_DSN"
	rollbackProjectEnv = "LEGION_E2E_ROLLBACK_PROJECT"
)

// rollbackNoticeKinds are the notice kinds this change adds, which the release before it cannot
// decode: it knows none of them, and its strict decoder refuses their claim and unreachable fields.
// Text, since that release has no names for them.
var rollbackNoticeKinds = []string{
	"unreachable-claims", "relaunch-ready", "relaunch-guard-held", "relaunch-not-back",
	"unreachable-exit", "envoy-no-live-session", "worker-locked-out", "claim-out-of-reach",
}

// rollbackCanaryKind is the ordinary controller notice the seed queues beside them, which every
// release delivers: the rollback step reads its delivery as the proof that the older daemon's
// outbox ran over the rows.
const rollbackCanaryKind = "tick"

// rollbackClaim is one claim of the fixture: its issue (the tree's root, or the child under it),
// its role and the state the seed writes it in. One claim stands in each state a claim can hold.
type rollbackClaim struct {
	child bool
	role  claim.Role
	state supervise.ClaimState
}

var rollbackClaims = []rollbackClaim{
	{false, claim.RoleArchitect, supervise.StateReady},
	{false, claim.RolePlanner, supervise.StateIdle},
	{false, claim.RoleImplementer, supervise.StateWorking},
	{false, claim.RoleTester, supervise.StateRegistered},
	{false, claim.RoleReviewer, supervise.StateShimConnected},
	{false, claim.RoleMerger, supervise.StateLaunching},
	{true, claim.RolePlanner, supervise.StateSuspended},
	{true, claim.RoleImplementer, supervise.StateFailed},
	{true, claim.RoleTester, supervise.StateRetired},
}

// rollbackProof is one run of the proof: the harness every live proof shares (liveProof), over
// the database and the project the script names, and the run's own log file.
type rollbackProof struct {
	liveProof
	rt      *fake.Runtime
	built   *built
	logPath string
}

// newRollbackProof readies a run over the broker liveNATSURLEnv names and the database and project
// the script names, skipping when any is unset, as newLiveProof does. The project's two issues
// are on the board, which lists them, so a boot finds them on Dispatch rather than lingering
// them closed. The stream the daemon's intake consumes is made afresh, since the run before left
// its own consumer on it; nothing of the proof is carried on the stream.
func newRollbackProof(t *testing.T) *rollbackProof {
	t.Helper()
	natsURL, dsn, key := os.Getenv(liveNATSURLEnv), os.Getenv(rollbackDSNEnv), os.Getenv(rollbackProjectEnv)
	if natsURL == "" || dsn == "" || key == "" {
		t.Skipf("%s, %s and %s name the broker, the database and the project the three runs share; scripts/e2e/rollback-notices.sh sets them",
			liveNATSURLEnv, rollbackDSNEnv, rollbackProjectEnv)
	}
	cfg := workflowConfig(t, natsURL)
	cfg.Project, cfg.Projects, cfg.PostgresDSN = key, map[string]config.Project{key: {}}, dsn
	p := &rollbackProof{liveProof: liveProof{t: t, cfg: cfg, board: newLiveDispatch(t, key), notices: newNoticeListener(),
		records: record.NewStore()}, rt: fake.NewRuntime(), built: &built{}}
	p.board.lists = true
	p.board.show(p.root(), "in_progress")
	p.board.place(p.child(), "in_progress", p.root(), true)
	p.cfg.DispatchURL = p.board.url
	envoyServer := httptest.NewServer(p.notices)
	t.Cleanup(envoyServer.Close)
	p.cfg.EnvoyURL = envoyServer.URL

	var err error
	if p.project, err = claim.ProjectToken(key); err != nil {
		t.Fatal(err)
	}
	p.js = workflowJetStream(t, natsURL)
	if err := p.js.DeleteStream(t.Context(), "ENVOY_NOTIFICATIONS"); err != nil && !errors.Is(err, jetstream.ErrStreamNotFound) {
		t.Fatalf("delete the stream the run before left: %v", err)
	}
	testnats.CreateStream(t, p.js, jetstream.StreamConfig{Name: "ENVOY_NOTIFICATIONS", Subjects: []string{"notifications.>"}})
	if p.st, err = store.Open(context.Background(), dsn); err != nil {
		t.Fatalf("open the store: %v", err)
	}
	t.Cleanup(p.st.Close)
	p.logPath = os.Getenv(liveDaemonLogEnv)
	if p.logPath == "" {
		p.logPath = filepath.Join(t.TempDir(), "daemon.log")
		t.Setenv(liveDaemonLogEnv, p.logPath)
	}
	for _, c := range rollbackClaims {
		writePrompt(t, p.cfg, p.token(c))
	}
	return p
}

// root and child are the fixture's two issues: a root of the daemon's own, and a child in its tree.
func (p *rollbackProof) root() string  { return p.cfg.Project + "-1" }
func (p *rollbackProof) child() string { return p.cfg.Project + "-2" }

// issueOf is the issue a fixture claim is of.
func (p *rollbackProof) issueOf(c rollbackClaim) string {
	if c.child {
		return p.child()
	}
	return p.root()
}

// token is a fixture claim's token, as the daemon spells it.
func (p *rollbackProof) token(c rollbackClaim) claim.Token {
	p.t.Helper()
	token, err := claim.NewToken(p.project, p.issueOf(c), c.role)
	if err != nil {
		p.t.Fatal(err)
	}
	return token
}

// boot starts the whole daemon of this tree on the shared database, as every whole-daemon live
// proof boots one (startDaemonWithin), with nothing launched for real: the fake runtime takes each
// launch, provisioning each claim's workspace itself.
func (p *rollbackProof) boot() {
	p.t.Helper()
	p.rt.InPod = true
	o := fakeRuntime(p.rt, p.built)
	o.workflowTokens = &workflowTokenRecorder{}
	p.d = startDaemonWithin(p.t, p.cfg, o, p.log(), 120*time.Second)
}

// rollbackRow is one outbox row of the proof as SQL reads it: whatever kind the release that wrote
// it gave it, the kind of the notice it carries, and what the outbox has done with it so far.
type rollbackRow struct {
	id          int64
	kind        string
	noticeKind  string
	attempts    int
	lastError   string
	nextAt      time.Time
	leaseHolder string
}

// rows is every row of the project's outbox that carries one of kinds as its notice's kind.
func (p *rollbackProof) rows(kinds []string) []rollbackRow {
	p.t.Helper()
	query, err := p.st.Pool().Query(context.Background(), `select id, kind, coalesce(payload->>'kind', ''), attempts,
			last_error, next_at, lease_token from outbox
		where split_part(issue, '-', 1) = $1 and payload->>'kind' = any($2) order by id`, p.cfg.Project, kinds)
	if err != nil {
		p.t.Fatalf("read the outbox: %v", err)
	}
	defer query.Close()
	var rows []rollbackRow
	for query.Next() {
		var row rollbackRow
		if err := query.Scan(&row.id, &row.kind, &row.noticeKind, &row.attempts, &row.lastError, &row.nextAt, &row.leaseHolder); err != nil {
			p.t.Fatalf("read the outbox: %v", err)
		}
		rows = append(rows, row)
	}
	if err := query.Err(); err != nil {
		p.t.Fatalf("read the outbox: %v", err)
	}
	return rows
}

// logLines is every line of the daemon's log, across every run of the proof, that holds text.
func (p *rollbackProof) logLines(text string) []string {
	p.t.Helper()
	file, err := os.Open(p.logPath)
	if err != nil {
		p.t.Fatalf("read the daemon's log %s: %v", p.logPath, err)
	}
	defer file.Close()
	var lines []string
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 1<<20), 1<<24)
	for scanner.Scan() {
		if strings.Contains(scanner.Text(), text) {
			lines = append(lines, scanner.Text())
		}
	}
	if err := scanner.Err(); err != nil {
		p.t.Fatalf("read the daemon's log %s: %v", p.logPath, err)
	}
	return lines
}

// publishedNotice is one notice the listener stand-in took, as its reader decodes it: the dedupe
// key it went under, and the notice's kind and claim.
type publishedNotice struct {
	Key   string
	Kind  string `json:"kind"`
	Claim string `json:"claim"`
}

// published is every notice the listener stand-in took on topic, oldest first.
func (p *rollbackProof) published(topic string) []publishedNotice {
	p.t.Helper()
	var notices []publishedNotice
	for _, taken := range p.notices.acceptedOn(topic) {
		var notice publishedNotice
		if err := json.Unmarshal([]byte(taken.Payload), &notice); err != nil {
			p.t.Fatalf("decode the payload published on %s, %q: %v", topic, taken.Payload, err)
		}
		notice.Key = taken.DedupeKey
		notices = append(notices, notice)
	}
	return notices
}

// noteClaims prints every fixture claim as the running daemon holds it now.
func (p *rollbackProof) noteClaims() {
	p.t.Helper()
	held := p.d.claims()
	for _, c := range rollbackClaims {
		token := p.token(c)
		found := false
		for _, listed := range held {
			if listed.Token == token {
				p.note("%s: seeded %s, held %s at generation %d", token, c.state, listed.State, listed.Generation)
				found = true
			}
		}
		if !found {
			p.note("%s: seeded %s, not held", token, c.state)
		}
	}
}

// The fixture: a daemon of this tree, which for the proof is the release before this change,
// migrates an empty database and records a tree of its own with one claim in each state a claim can
// hold, each at its first launch, as its store records them. That change's migrations then run over
// these rows, as they run over a deployment's database.
func TestARollbackFixtureIsRecordedByTheReleaseBeforeLive(t *testing.T) {
	p := newRollbackProof(t)
	ctx := context.Background()
	p.step("the release before migrates an empty database", func() {
		applied, err := p.st.Migrate(ctx)
		if err != nil {
			t.Fatalf("migrate: %v", err)
		}
		version, err := p.st.SchemaVersion(ctx)
		if err != nil {
			t.Fatalf("read the schema version: %v", err)
		}
		p.note("applied %d migrations; the schema is at version %d", applied, version)
	})
	p.step("it records a tree of the daemon's own, and one claim in each state", func() {
		root, child := p.root(), p.child()
		if err := p.st.Tx(ctx, func(tx pgx.Tx) error {
			for _, issue := range []record.Issue{
				{Key: root, Repo: liveRepo, DefaultBranch: "main", GitHubRepo: liveRepo, Tree: root, Project: p.cfg.Project,
					Title: root, Phase: phase.Implementing, Generation: 1, Status: "in_progress", DispatchStatus: "in_progress",
					Rank: "U", LastDispatchSeq: 1, HandedOver: true},
				{Key: child, Repo: liveRepo, DefaultBranch: "main", GitHubRepo: liveRepo, Tree: root, Parent: &root, Project: p.cfg.Project,
					Title: child, Phase: phase.Planning, Generation: 1, Status: "in_progress", DispatchStatus: "in_progress",
					Rank: "U", LastDispatchSeq: 1},
			} {
				if err := p.records.PutIssue(ctx, tx, issue); err != nil {
					return err
				}
			}
			return p.admit(ctx, tx, root)
		}); err != nil {
			t.Fatalf("record the tree: %v", err)
		}
		if _, err := p.st.OpenTreeLifecycle(ctx, p.project, root, treelifecycle.AuthorityWorkflow); err != nil {
			t.Fatalf("open the lifecycle of %s: %v", root, err)
		}
		for _, c := range rollbackClaims {
			token := p.token(c)
			seeded := supervise.Claim{Token: token, Project: p.project, Tree: root, Issue: p.issueOf(c), Role: c.role,
				Generation: 1, ServingGeneration: 1, State: c.state, BootTokenHash: supervise.HashBootToken(string(token) + "-1")}
			if c.state != supervise.StateLaunching && c.state != supervise.StateShimConnected {
				seeded.Session, seeded.SessionFile = "ses-"+string(token), "/sessions/"+string(token)+".jsonl"
			}
			if c.state == supervise.StateSuspended {
				seeded.SuspendedBy = supervise.SuspendedByDaemon
			}
			admitted, err := p.st.AdmitClaim(ctx, seeded)
			if err != nil {
				t.Fatalf("admit %s: %v", token, err)
			}
			p.note("%s: %s at generation %d", token, admitted.State, admitted.Generation)
		}
	})
}

// The rollback step: a daemon of this tree, which for the proof is the release before this change,
// starts on the database a daemon of that change migrated and wrote one of each of its notices
// into, runs its outbox — the canary tick the seed queued beside them reaches the controller's
// topic and its row finishes — and leaves each of those notices as it stands: no attempt, no error
// and no line of its log names a row it could not decode, and its state answers. Against a seed of
// that change before it gave these notices kinds of their own, the older daemon's board lane takes
// each such row, fails to decode it and retries it for ever, which is what this step catches: its
// log names the decode failure and the rows carry it.
func TestARollbackDaemonLeavesTheNoticesItCannotDecodeLive(t *testing.T) {
	p := newRollbackProof(t)
	var seeded []rollbackRow
	p.step("the seed left one of each notice of this change and a canary tick", func() {
		seeded = p.rows(rollbackNoticeKinds)
		for _, row := range seeded {
			p.note("row %d: kind %s, notice %s, %d attempts", row.id, row.kind, row.noticeKind, row.attempts)
		}
		if len(seeded) != len(rollbackNoticeKinds) {
			t.Fatalf("the outbox holds %d of the %d notices the seed queued", len(seeded), len(rollbackNoticeKinds))
		}
		if canary := p.rows([]string{rollbackCanaryKind}); len(canary) != 1 {
			t.Fatalf("the outbox holds %d canary ticks, want the one the seed queued", len(canary))
		}
	})
	p.step("the daemon starts on the database the newer release migrated", func() {
		p.boot()
		p.note("version %d of the schema, which this daemon reads as it stands", p.d.state().Daemon.SchemaVersion)
	})
	p.step("its outbox runs: the canary tick is delivered and its row finished", func() {
		if !testwait.Holds(90*time.Second, 200*time.Millisecond, func() bool { return len(p.rows([]string{rollbackCanaryKind})) == 0 }) {
			t.Fatalf("the canary tick is still in the outbox: %+v", p.rows([]string{rollbackCanaryKind}))
		}
		published := p.published(notify.ControllerTopic(p.project))
		p.note("the controller's topic took %d notices", len(published))
		if !slices.ContainsFunc(published, func(n publishedNotice) bool { return n.Kind == rollbackCanaryKind }) {
			t.Fatalf("no tick reached the controller's topic; it took %+v", published)
		}
	})
	p.step("it leaves each notice it cannot decode as it stands, and its log names no decode failure", func() {
		// Every lane of the project ran its due rows by the time the canary finished; this is
		// how long a lane takes to come back to a row its first attempt failed, a backoff of one
		// second doubling, so a row the daemon took is seen taken.
		time.Sleep(3 * liveSettle)
		left := p.rows(rollbackNoticeKinds)
		for _, row := range left {
			p.note("row %d: kind %s, notice %s, %d attempts, last error %q", row.id, row.kind, row.noticeKind, row.attempts, row.lastError)
		}
		if len(left) != len(seeded) {
			t.Errorf("%d of the %d notices are left in the outbox; the daemon finished one it cannot decode", len(left), len(seeded))
		}
		for _, row := range left {
			if row.attempts != 0 || row.lastError != "" {
				t.Errorf("row %d (%s, notice %s) was run: %d attempts, last error %q", row.id, row.kind, row.noticeKind, row.attempts, row.lastError)
			}
		}
		failures := p.logLines("decode outbox row")
		for _, line := range failures {
			p.note("log: %s", line)
		}
		if len(failures) != 0 {
			t.Errorf("the daemon's log names %d decode failures", len(failures))
		}
	})
	p.step("its state answers and it holds every claim of the fixture", func() {
		state := p.d.state()
		p.note("the state answers with %d issues", len(state.Issues))
		p.noteClaims()
	})
	p.step("it stops", func() { p.d.stop() })
}
