package settings

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/testpg/testdb"
)

// testProject is the project every row these tests write is filed under.
const testProject = "DEMO"

// minimalFile is the smallest configuration that loads, as internal/config's own tests write
// one. It names neither `admission_cap` nor `review_round_cap`, and settles their defaults (4
// and 3): a test that gives the file its own value for one of them appends that key, since a
// file carrying one key twice is a file no operator writes.
const minimalFile = `project: DEMO
state_dir: /var/lib/legion
postgres_dsn: postgres://legion@127.0.0.1:5432/legion
`

// kubernetesFile is the smallest file `runtime: kubernetes` loads, as internal/config's own
// tests write one: every address a pod dials written as a host a pod can reach, and the
// required members of the block, which the block being last lets a test append to.
const kubernetesFile = `project: DEMO
state_dir: /var/lib/legion
postgres_dsn: postgres://legion@127.0.0.1:5432/legion
bind: 10.0.0.5
daemon_url: http://10.0.0.5:13370
envoy_url: http://envoy-listener.internal.example:9020
nats_urls: [nats://nats.internal.example:4222]
envoy_token_file: /var/run/legion/ENVOY_TOKEN
operator_token_file: /var/run/legion/OPERATOR_TOKEN
dispatch_url: https://dispatch.internal.example
dispatch_token_file: /var/run/legion/DISPATCH_TOKEN
projects:
  DEMO: {}
github_apps:
  implement: { app_id: "1", private_key: implement-test-key }
  review: { app_id: "2", private_key: review-test-key }
runtime:
  kubernetes:
    namespace: legion
    image: ghcr.io/cybermonklabs/legion-worker@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
    storage_class: gp2
    session_store: postgres
    session_dsn_secret: SESSION_DSN
`

// tmuxReading is that same deployment's file rewritten as the other runtime's, the edit
// docs/deployment.md has an operator make before a restart. It loads, and this daemon settles
// nothing of it (config.Live.Apply).
var tmuxReading = runtimeSwapped(kubernetesFile, "runtime: tmux\n")

// runtimeSwapped is file with its `runtime` key, the last key kubernetesFile holds, replaced by
// block. It panics on a file carrying no such key, so a fixture that stops holding one cannot
// pass by swapping nothing.
func runtimeSwapped(file, block string) string {
	head, _, found := strings.Cut(file, "runtime:\n")
	if !found {
		panic("the fixture carries no runtime key to swap")
	}
	return head + block
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// configFile writes body to a legion.yaml of the test's own and answers its path.
func configFile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "legion.yaml")
	rewrite(t, path, body)
	return path
}

func rewrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// testStore is a migrated database of the test's own: the keeper is held to what a real store
// records, through the daemon's own schema.
func testStore(t *testing.T) *Store {
	t.Helper()
	st, err := store.Open(context.Background(), testdb.Database(t, "legion_settings_test"))
	if err != nil {
		t.Fatalf("open the test database: %v", err)
	}
	t.Cleanup(st.Close)
	if _, err := st.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate the test database: %v", err)
	}
	return NewStore(st.Pool(), testProject)
}

// keeperOn is a keeper over path with its own holder, as a boot builds one.
func keeperOn(t *testing.T, path string, store *Store) *Keeper {
	t.Helper()
	return keeperLogging(t, path, store, quiet())
}

// keeperLogging is that keeper with a logger of the test's own, so a test reads the lines an
// operator would.
func keeperLogging(t *testing.T, path string, store *Store, log *slog.Logger) *Keeper {
	t.Helper()
	live, err := config.NewLive(path, func(string) string { return "" }, nil)
	if err != nil {
		t.Fatalf("resolve %s: %v", path, err)
	}
	return New(live, store, log)
}

// logRecorder keeps every line the keeper logs, message and attributes, so a test can count
// the lines one call wrote and read the words in them.
type logRecorder struct {
	mu      sync.Mutex
	records []slog.Record
}

func (r *logRecorder) Enabled(context.Context, slog.Level) bool { return true }

func (r *logRecorder) Handle(_ context.Context, record slog.Record) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.records = append(r.records, record)
	return nil
}

// The keeper logs through the logger it is given and never derives one; a handler that dropped
// attributes here would hide them from the assertions, so it refuses to be derived.
func (r *logRecorder) WithAttrs([]slog.Attr) slog.Handler { panic("the keeper derived a logger") }
func (r *logRecorder) WithGroup(string) slog.Handler      { panic("the keeper derived a logger") }

// warnings is every line the keeper wrote at Warn, message and attributes in one string.
func (r *logRecorder) warnings() []string { return r.at(slog.LevelWarn) }

// at is every line written at level, message and attributes in one string.
func (r *logRecorder) at(level slog.Level) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var lines []string
	for _, record := range r.records {
		if record.Level != level {
			continue
		}
		line := record.Message
		record.Attrs(func(attr slog.Attr) bool {
			line += " " + attr.String()
			return true
		})
		lines = append(lines, line)
	}
	return lines
}

// A route change is stored, and the next boot lays it back over the file: the daemon a restart
// brings up is running on what the person asked for, with their name and the moment of the
// change (acceptance A5, "a route change survives a restart").
func TestARouteChangeIsLaidBackOverTheFileAtTheNextBoot(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	path := configFile(t, minimalFile)
	keeper := keeperOn(t, path, store)

	applied, refused, err := keeper.Route(ctx, config.CallerOperator, "ana@example.com", map[string]any{"admission_cap": 7})
	if err != nil || len(refused) != 0 {
		t.Fatalf("Route: %v, refused %+v", err, refused)
	}
	if len(applied) != 1 || applied[0].Path != "admission_cap" || applied[0].New != "7" {
		t.Fatalf("Route applied %+v, want admission_cap 7", applied)
	}

	restarted := keeperOn(t, path, store)
	if cap := restarted.Live().Snapshot().AdmissionCap; cap != 4 {
		t.Fatalf("the file alone reads admission_cap %d, want the file's own 4 before the restore", cap)
	}
	kept, err := restarted.Restore(ctx)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if cap := restarted.Live().Snapshot().AdmissionCap; cap != 7 {
		t.Fatalf("admission_cap after the restore = %d, want the route's 7", cap)
	}
	if len(kept) != 1 || kept[0].Path != "admission_cap" || kept[0].By != "ana@example.com" ||
		kept[0].Source != config.SourceRoute || kept[0].At.IsZero() {
		t.Fatalf("Restore kept %+v, want ana's admission_cap change with its own time", kept)
	}
}

// A secret path is applied and never stored: the value — a credential, the path of a file
// holding one, or an address that may carry one — stays out of the database every project's
// daemon shares, where a plain value of the same call is kept. The row of such a path goes with
// it, so a restart reads the file's own value for it.
func TestACredentialChangedThroughTheRouteIsAppliedAndNotStored(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	keeper := keeperOn(t, configFile(t, minimalFile), store)

	applied, refused, err := keeper.Route(ctx, config.CallerOperator, "ana@example.com", map[string]any{
		"operator_token_file": "/etc/legion/operator-token",
		"admission_cap":       7,
	})
	if err != nil || len(refused) != 0 {
		t.Fatalf("Route: %v, refused %+v", err, refused)
	}

	paths := map[string]bool{}
	for _, change := range applied {
		paths[change.Path] = true
	}
	if !paths["operator_token_file"] || !paths["admission_cap"] {
		t.Fatalf("the route applied %+v, want both paths", applied)
	}
	if file := keeper.Live().Snapshot().OperatorTokenFile; file != "/etc/legion/operator-token" {
		t.Fatalf("the daemon reads operator_token_file %q, want the one the route set", file)
	}
	rows, err := store.Rows(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Path != "admission_cap" {
		t.Fatalf("the store holds %+v, want the plain value alone and no credential", rows)
	}
}

// The file takes back a key the route applied and the store holds no row for. A credential is
// applied and never stored (above), so nothing in the store says the daemon is running on the
// route's value for it: without the digest the keeper holds in memory the file arm would find
// no row, withdraw nothing, and leave the operator's later edit of that key in `legion.yaml`
// doing nothing at all until the daemon restarted.
func TestTheFileTakesBackACredentialTheRouteAppliedAndStoredNoRowFor(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	path := configFile(t, minimalFile)
	keeper := keeperOn(t, path, store)
	const byRoute = "/etc/legion/operator-token"
	if _, refused, err := keeper.Route(ctx, config.CallerOperator, "ana@example.com", map[string]any{
		"operator_token_file": byRoute,
	}); err != nil || len(refused) != 0 {
		t.Fatalf("Route: %v, refused %+v", err, refused)
	}
	if file := keeper.Live().Snapshot().OperatorTokenFile; file != byRoute {
		t.Fatalf("the daemon reads operator_token_file %q, want the route's %q", file, byRoute)
	}

	// The operator edits that key in the file itself, and the watcher settles the file.
	const byFile = "/etc/legion/operator-token-2"
	rewrite(t, path, minimalFile+"operator_token_file: "+byFile+"\n")
	if _, err := keeper.File(ctx); err != nil {
		t.Fatalf("File: %v", err)
	}

	if file := keeper.Live().Snapshot().OperatorTokenFile; file != byFile {
		t.Fatalf("the daemon reads operator_token_file %q, want the file's own %q: the file changed that key and takes it back", file, byFile)
	}
	// And the key stays the file's: a later settle of an unchanged file withdraws nothing and
	// changes nothing.
	if _, err := keeper.File(ctx); err != nil {
		t.Fatalf("the second File: %v", err)
	}
	if file := keeper.Live().Snapshot().OperatorTokenFile; file != byFile {
		t.Fatalf("the daemon reads operator_token_file %q after a second settle, want the file's own %q", file, byFile)
	}
}

// A settle the loader refuses leaves the keeper's mark of a path no row backs standing. The
// apply changes nothing, so the overlay the daemon is running on is still the route's; a mark
// dropped before that apply would be gone with no overlay withdrawn, and no later settle of the
// fixed file would ever take the key back — the daemon would run on the route's credential
// until it restarted, whatever the operator wrote in `legion.yaml`.
func TestAFailedSettleKeepsTheMarkOfAKeyNoRowBacks(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	path := configFile(t, minimalFile)
	keeper := keeperOn(t, path, store)
	const byRoute = "/route/operator-token"
	if _, refused, err := keeper.Route(ctx, config.CallerOperator, "ana@example.com", map[string]any{
		"operator_token_file": byRoute,
	}); err != nil || len(refused) != 0 {
		t.Fatalf("Route: %v, refused %+v", err, refused)
	}

	// The operator edits that key in the file and, in the same edit, writes a value the loader
	// refuses: the whole settle fails, since every apply resolves the whole document.
	const byFile = "/file/operator-token"
	rewrite(t, path, minimalFile+"operator_token_file: "+byFile+"\nadmission_cap: -1\n")
	if _, err := keeper.File(ctx); err == nil {
		t.Fatal("the settle of a file the loader refuses: no error, want the refusal")
	}
	if file := keeper.Live().Snapshot().OperatorTokenFile; file != byRoute {
		t.Fatalf("operator_token_file = %q after the failed settle, want the route's %q: a settle that fails changes nothing", file, byRoute)
	}

	// The operator fixes the value the loader refused, leaving the file's own value for the
	// credential as it now reads it, and the settle succeeds.
	rewrite(t, path, minimalFile+"operator_token_file: "+byFile+"\nadmission_cap: 5\n")
	if _, err := keeper.File(ctx); err != nil {
		t.Fatalf("the settle of the fixed file: %v", err)
	}
	if cfg := keeper.Live().Snapshot(); cfg.OperatorTokenFile != byFile || cfg.AdmissionCap != 5 {
		t.Fatalf("the fixed file settled operator_token_file %q and admission_cap %d, want the file's own %q and 5: the file changed that key and takes it back",
			cfg.OperatorTokenFile, cfg.AdmissionCap, byFile)
	}
}

// The same holds for a change no row backs because nothing stored it: a keeper with no store,
// which is what a daemon whose store write failed is left with for those paths (NotRecorded).
// The route's value stands until the file's own value for that key moves, and then the file's
// stands.
func TestTheFileTakesBackAKeyAStorelessKeeperApplied(t *testing.T) {
	ctx := context.Background()
	path := configFile(t, minimalFile)
	keeper := keeperOn(t, path, nil)
	if _, refused, err := keeper.Route(ctx, config.CallerOperator, "ana@example.com", map[string]any{"admission_cap": 7}); err != nil || len(refused) != 0 {
		t.Fatalf("Route: %v, refused %+v", err, refused)
	}

	// A settle of a file that did not change that key leaves the route's value standing.
	if _, err := keeper.File(ctx); err != nil {
		t.Fatalf("File: %v", err)
	}
	if cap := keeper.Live().Snapshot().AdmissionCap; cap != 7 {
		t.Fatalf("admission_cap = %d after a settle of an unchanged key, want the route's 7", cap)
	}

	rewrite(t, path, minimalFile+"admission_cap: 9\n")
	if _, err := keeper.File(ctx); err != nil {
		t.Fatalf("the settle after the file changed the key: %v", err)
	}

	if cap := keeper.Live().Snapshot().AdmissionCap; cap != 9 {
		t.Fatalf("admission_cap = %d, want the file's own 9: the file changed that key and takes it back", cap)
	}
}

// The file takes its key back: a change of that key in the file itself, while the daemon was
// down, outlives the route change above it, and the row goes with it. A route change of another
// key stands (acceptance A5, "until the file changes that key").
func TestTheFileTakesBackOnlyTheKeysItChanged(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	path := configFile(t, minimalFile)
	keeper := keeperOn(t, path, store)
	if _, _, err := keeper.Route(ctx, config.CallerOperator, "ana@example.com", map[string]any{"admission_cap": 7, "review_round_cap": 5}); err != nil {
		t.Fatalf("Route: %v", err)
	}

	rewrite(t, path, minimalFile+"admission_cap: 9\n")
	restarted := keeperOn(t, path, store)
	kept, err := restarted.Restore(ctx)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	cfg := restarted.Live().Snapshot()
	if cfg.AdmissionCap != 9 {
		t.Errorf("admission_cap = %d, want the file's own 9: the file changed that key", cfg.AdmissionCap)
	}
	if cfg.ReviewRoundCap != 5 {
		t.Errorf("review_round_cap = %d, want the route's 5: the file changed no such key", cfg.ReviewRoundCap)
	}
	if len(kept) != 1 || kept[0].Path != "review_round_cap" {
		t.Fatalf("Restore kept %+v, want the review_round_cap change alone", kept)
	}
	rows, err := store.Rows(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Path != "review_round_cap" {
		t.Fatalf("the store holds %+v, want the row the file did not take back", rows)
	}
}

// The same under a running daemon: the file arm settles the file as it now reads, keeps the
// route changes of keys the file did not touch, and drops the ones it did.
func TestTheFileArmTakesBackTheKeysTheFileChangedUnderTheRunningDaemon(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	path := configFile(t, minimalFile)
	keeper := keeperOn(t, path, store)
	if _, _, err := keeper.Route(ctx, config.CallerOperator, "ana@example.com", map[string]any{"admission_cap": 7, "review_round_cap": 5}); err != nil {
		t.Fatalf("Route: %v", err)
	}

	rewrite(t, path, minimalFile+"admission_cap: 9\nlaunch_failure_limit: 2\n")
	applied, err := keeper.File(ctx)
	if err != nil {
		t.Fatalf("File: %v", err)
	}
	cfg := keeper.Live().Snapshot()
	if cfg.AdmissionCap != 9 || cfg.ReviewRoundCap != 5 || cfg.LaunchFailureLimit != 2 {
		t.Fatalf("the file arm settled cap %d, rounds %d, launch failures %d; want the file's 9, the route's 5 and the file's 2",
			cfg.AdmissionCap, cfg.ReviewRoundCap, cfg.LaunchFailureLimit)
	}
	var paths []string
	for _, change := range applied {
		paths = append(paths, change.Path)
		if change.Source != config.SourceFile {
			t.Errorf("%s was applied as %q, want the file's own source", change.Path, change.Source)
		}
	}
	if len(paths) != 2 {
		t.Fatalf("the file arm applied %v, want admission_cap and launch_failure_limit", paths)
	}
	rows, err := store.Rows(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Path != "review_round_cap" {
		t.Fatalf("the store holds %+v, want the row the file did not take back", rows)
	}
}

// A path the route withdraws is dropped from the overlay and from the store: the file's own
// value takes the key back at once.
func TestARouteWithdrawalGivesTheKeyBackToTheFile(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	keeper := keeperOn(t, configFile(t, minimalFile), store)
	if _, _, err := keeper.Route(ctx, config.CallerOperator, "ana@example.com", map[string]any{"admission_cap": 7}); err != nil {
		t.Fatalf("Route: %v", err)
	}

	applied, _, err := keeper.Route(ctx, config.CallerOperator, "ana@example.com", map[string]any{"admission_cap": nil})
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if len(applied) != 1 || applied[0].New != "4" {
		t.Fatalf("the withdrawal applied %+v, want admission_cap back at the file's 4", applied)
	}
	rows, err := store.Rows(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("the store holds %+v after the withdrawal, want nothing", rows)
	}
}

// A withdrawal whose store write fails leaves the row the store holds for that path, and the
// next boot lays it back over the file. Nothing here can do better: the one act that would drop
// the row is the write that failed, and the keeper's memory of the change does not outlive the
// process. The caller is told the change is applied and not recorded (config.NotRecorded), and
// that is what the answer means for a withdrawal — the daemon runs on the file's own value for
// the key until it restarts, and on the kept row after it.
func TestAWithdrawalWhoseStoreWriteFailedIsLaidBackAtTheNextBoot(t *testing.T) {
	ctx := context.Background()
	dsn := testdb.Database(t, "legion_settings_withdrawal")
	st, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open the test database: %v", err)
	}
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate the test database: %v", err)
	}
	path := configFile(t, minimalFile)
	keeper := keeperOn(t, path, NewStore(st.Pool(), testProject))
	if _, _, err := keeper.Route(ctx, config.CallerOperator, "ana@example.com", map[string]any{"admission_cap": 7}); err != nil {
		t.Fatalf("Route: %v", err)
	}

	// The store is gone by the time the withdrawal reaches it: the overlay goes, the row stays.
	st.Close()
	applied, refused, err := keeper.Route(ctx, config.CallerOperator, "ana@example.com", map[string]any{"admission_cap": nil})
	var notRecorded *config.NotRecorded
	if !errors.As(err, &notRecorded) || len(refused) != 0 {
		t.Fatalf("the withdrawal answered %v, refused %+v; want a NotRecorded", err, refused)
	}
	if len(applied) != 1 || applied[0].New != "4" {
		t.Fatalf("the withdrawal applied %+v, want admission_cap back at the file's 4", applied)
	}
	if cap := keeper.Live().Snapshot().AdmissionCap; cap != 4 {
		t.Fatalf("admission_cap = %d, want the file's own 4: the daemon is running on the withdrawal", cap)
	}

	// And the restart reads the row the store kept, which is what the answer says it will.
	next, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("reopen the test database: %v", err)
	}
	t.Cleanup(next.Close)
	restarted := keeperOn(t, path, NewStore(next.Pool(), testProject))
	kept, err := restarted.Restore(ctx)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if len(kept) != 1 || kept[0].Path != "admission_cap" {
		t.Fatalf("Restore kept %+v, want the row the withdrawal could not drop", kept)
	}
	if cap := restarted.Live().Snapshot().AdmissionCap; cap != 7 {
		t.Fatalf("admission_cap after the restart = %d, want the kept row's 7", cap)
	}
}

// A path fixed for the process is refused by name, with the reason its row gives, and nothing is
// stored for it (acceptance A5, "a key fixed for the process is refused by name").
func TestAFixedPathIsRefusedAndStoredNowhere(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	keeper := keeperOn(t, configFile(t, minimalFile), store)

	applied, refused, err := keeper.Route(ctx, config.CallerOperator, "ana@example.com", map[string]any{"bind": "0.0.0.0", "admission_cap": 6})
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if len(refused) != 1 || refused[0].Path != "bind" || refused[0].Reason == "" {
		t.Fatalf("Route refused %+v, want bind by name with its reason", refused)
	}
	if len(applied) != 1 || applied[0].Path != "admission_cap" {
		t.Fatalf("Route applied %+v, want the one path it did not refuse", applied)
	}
	rows, err := store.Rows(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Path != "admission_cap" {
		t.Fatalf("the store holds %+v, want the applied path alone", rows)
	}
}

// The file names another runtime, and this keeper's own record stands with the holder's
// overlay: that reading settled nothing (config.Live.Apply), so the key the file would have
// taken back is still the route's, in the store as well as over the file, and the settle made
// of a file this daemon can read takes it back exactly as this one would have.
//
// The second row is the control: that same edit of that same key, in the booted runtime's own
// reading, takes the key back and drops its row — which is what a settle that recorded a
// reading it never ran on would have done here.
func TestAFileNamingAnotherRuntimeTakesBackNoKeyTheRouteHolds(t *testing.T) {
	for _, tc := range []struct {
		name, reading string
		other         bool
		cap           int
		rows          int
	}{
		{name: "the other runtime's reading", reading: tmuxReading + "admission_cap: 5\n", other: true, cap: 7, rows: 1},
		{name: "the booted runtime's own reading", reading: kubernetesFile + "admission_cap: 5\n", cap: 5, rows: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			store := testStore(t)
			path := configFile(t, kubernetesFile)
			keeper := keeperOn(t, path, store)
			if _, refused, err := keeper.Route(ctx, config.CallerOperator, "ana@example.com", map[string]any{"admission_cap": 7}); err != nil || len(refused) != 0 {
				t.Fatalf("Route: %v, refused %+v", err, refused)
			}

			rewrite(t, path, tc.reading)
			_, err := keeper.File(ctx)

			var other *config.OtherRuntime
			switch {
			case tc.other && !errors.As(err, &other):
				t.Fatalf("File: %v, want the reading refused whole", err)
			case tc.other && (other.File != "tmux" || other.Booted != "kubernetes"):
				t.Fatalf("File answered %+v, want both runtimes named", other)
			case !tc.other && err != nil:
				t.Fatalf("File: %v", err)
			}

			if cap := keeper.Live().Snapshot().AdmissionCap; cap != tc.cap {
				t.Fatalf("admission_cap after the settle = %d, want %d", cap, tc.cap)
			}
			if name := keeper.Live().Snapshot().Runtime.Name; name != "kubernetes" {
				t.Fatalf("the settled runtime is %q, want the kubernetes deployment the daemon booted on", name)
			}
			rows, err := store.Rows(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) != tc.rows {
				t.Fatalf("the store holds %+v, want %d row(s): what it keeps is what the daemon is running on", rows, tc.rows)
			}
		})
	}
}

// A route change made while the file names another runtime is stored nowhere: the holder
// settled nothing of it, so a row written for it would be laid back over the file at the next
// boot as a change this daemon never ran on (config.Live.Apply).
//
// The second row is the control: the same call over the booted runtime's own file applies and
// is stored.
func TestARouteChangeMadeWhileTheFileNamesAnotherRuntimeIsStoredNowhere(t *testing.T) {
	for _, tc := range []struct {
		name, reading string
		cap           int
		rows          int
	}{
		{name: "the other runtime's reading", reading: tmuxReading, cap: 4, rows: 0},
		{name: "the booted runtime's own reading", reading: kubernetesFile, cap: 7, rows: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			store := testStore(t)
			path := configFile(t, kubernetesFile)
			keeper := keeperOn(t, path, store)
			rewrite(t, path, tc.reading)

			applied, refused, err := keeper.Route(ctx, config.CallerOperator, "ana@example.com", map[string]any{"admission_cap": 7})

			var other *config.OtherRuntime
			switch {
			case tc.rows == 0 && !errors.As(err, &other):
				t.Fatalf("Route: %v, want the reading refused whole", err)
			case tc.rows == 0 && (len(applied) != 0 || len(refused) != 0):
				t.Fatalf("Route applied %+v and refused %+v, want neither: the reading itself is the refusal", applied, refused)
			case tc.rows == 0 && (other.File != "tmux" || other.Booted != "kubernetes"):
				t.Fatalf("Route answered %+v, want both runtimes named", other)
			case tc.rows > 0 && err != nil:
				t.Fatalf("Route: %v", err)
			}
			if cap := keeper.Live().Snapshot().AdmissionCap; cap != tc.cap {
				t.Fatalf("admission_cap after the route change = %d, want %d", cap, tc.cap)
			}
			rows, err := store.Rows(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) != tc.rows {
				t.Fatalf("the store holds %+v, want %d row(s): what it keeps is what the daemon is running on", rows, tc.rows)
			}
		})
	}
}

// The route arm logs a reading of another runtime once per call, naming the caller, every path
// the call asked for and both runtimes. Nothing else records it: the route answers the caller
// alone, and a daemon whose file has become another deployment's refuses every change until
// somebody reads the log and sees why.
//
// Once per call, and not once per key: a call naming three paths writes one line, so the
// operator reads one event rather than one line per key of a change that did nothing.
//
// The second row is the control: the same call over the booted runtime's own file writes no
// such line at all.
func TestTheRouteLogsAReadingOfAnotherRuntimeOncePerCall(t *testing.T) {
	for _, tc := range []struct {
		name, reading string
		lines         int
	}{
		{name: "the other runtime's reading", reading: tmuxReading, lines: 1},
		{name: "the booted runtime's own reading", reading: kubernetesFile, lines: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			path := configFile(t, kubernetesFile)
			recorder := &logRecorder{}
			keeper := keeperLogging(t, path, testStore(t), slog.New(recorder))
			rewrite(t, path, tc.reading)

			_, _, err := keeper.Route(ctx, config.CallerOperator, "ana@example.com", map[string]any{
				"admission_cap": 7, "review_round_cap": 5, "linger_hours": 3,
			})

			var other *config.OtherRuntime
			if tc.lines > 0 != errors.As(err, &other) {
				t.Fatalf("Route: %v", err)
			}
			warnings := recorder.warnings()
			if len(warnings) != tc.lines {
				t.Fatalf("the route wrote %d warning(s), want %d: one line per call, not one per key\n%s",
					len(warnings), tc.lines, strings.Join(warnings, "\n"))
			}
			if tc.lines == 0 {
				return
			}
			for _, want := range []string{
				"another runtime", "operator", "ana@example.com",
				"admission_cap", "linger_hours", "review_round_cap", "tmux", "kubernetes",
			} {
				if !strings.Contains(warnings[0], want) {
					t.Errorf("the line names no %q:\n\t%s", want, warnings[0])
				}
			}
		})
	}
}

// A boot whose file has become another runtime's does not come up: the restore lays the stored
// route changes back over the file through the holder's route arm, which settles nothing of
// such a reading (config.OtherRuntime), and the boot fails with the reason naming both
// runtimes. A boot that went on would build every holder from a file this daemon never
// settled, while the store credited the route with an overlay the daemon is not running on.
//
// The second row is the control: the same stored row, over a file that names the runtime this
// boot read, is laid back and the boot proceeds.
func TestABootWhoseFileNamesAnotherRuntimeFailsNamingBoth(t *testing.T) {
	for _, tc := range []struct {
		name, reading string
		other         bool
		cap           int
	}{
		{name: "the file names the other runtime", reading: tmuxReading, other: true, cap: 4},
		{name: "the file names the runtime the boot read", reading: kubernetesFile, cap: 7},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			store := testStore(t)
			path := configFile(t, kubernetesFile)
			keeper := keeperOn(t, path, store)
			if _, refused, err := keeper.Route(ctx, config.CallerOperator, "ana@example.com", map[string]any{"admission_cap": 7}); err != nil || len(refused) != 0 {
				t.Fatalf("Route: %v, refused %+v", err, refused)
			}

			// The restart: a keeper of its own reads the file as it stands, and the file is
			// rewritten under it before the restore lays the stored row back — the operator's
			// edit ahead of a planned restart, or a ConfigMap swapped as the daemon came up.
			restarted := keeperOn(t, path, store)
			rewrite(t, path, tc.reading)
			kept, err := restarted.Restore(ctx)

			var other *config.OtherRuntime
			switch {
			case tc.other && !errors.As(err, &other):
				t.Fatalf("Restore: %v, want the boot to fail on the reading", err)
			case tc.other && (other.File != "tmux" || other.Booted != "kubernetes"):
				t.Fatalf("Restore answered %+v, want both runtimes named", other)
			case !tc.other && err != nil:
				t.Fatalf("Restore: %v", err)
			}
			if tc.other && len(kept) != 0 {
				t.Fatalf("Restore kept %+v, want nothing: a boot that failed credits the route with no path", kept)
			}
			if cap := restarted.Live().Snapshot().AdmissionCap; cap != tc.cap {
				t.Fatalf("admission_cap after the restore = %d, want %d", cap, tc.cap)
			}
		})
	}
}

// A boot that fails deletes nothing either: the rows the file's own edits would have taken back
// are still there, so the operator who puts the file back and boots again gets the route
// changes the first boot would otherwise have thrown away. Restore deletes them after its apply
// has succeeded, which is the order the running daemon's arm already had (Keeper.File, whose
// write comes after Apply).
//
// The second row is the control: that same edit of that same key, in the booted runtime's own
// reading, is applied, and the key the file took back loses its row.
func TestABootThatFailsDeletesNoRowTheFileTookBack(t *testing.T) {
	for _, tc := range []struct {
		name, reading string
		other         bool
		rows          []string
	}{
		{
			name: "the file names the other runtime", reading: tmuxReading + "admission_cap: 9\n",
			other: true, rows: []string{"admission_cap", "review_round_cap"},
		},
		{
			name: "the file names the runtime the boot read", reading: kubernetesFile + "admission_cap: 9\n",
			rows: []string{"review_round_cap"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			store := testStore(t)
			path := configFile(t, kubernetesFile)
			keeper := keeperOn(t, path, store)
			// Two paths: the one the reading edits, which the file is taking back, and one it
			// leaves alone, which is what the boot has to lay back over the file and so what
			// makes it apply at all.
			changes := map[string]any{"admission_cap": 7, "review_round_cap": 5}
			if _, refused, err := keeper.Route(ctx, config.CallerOperator, "ana@example.com", changes); err != nil || len(refused) != 0 {
				t.Fatalf("Route: %v, refused %+v", err, refused)
			}

			restarted := keeperOn(t, path, store)
			rewrite(t, path, tc.reading)
			_, err := restarted.Restore(ctx)

			var other *config.OtherRuntime
			if errors.As(err, &other) != tc.other {
				t.Fatalf("Restore: %v, want the boot to fail on the reading: %v", err, tc.other)
			}
			rows, err := store.Rows(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if got := pathsOf(rows); !slices.Equal(got, tc.rows) {
				t.Fatalf("the store holds %v, want %v", got, tc.rows)
			}
			if !tc.other {
				return
			}

			// The operator puts the file back and boots again, which is the one cure for a boot
			// the reading refused: both route changes are laid back over it.
			rewrite(t, path, kubernetesFile)
			recovered := keeperOn(t, path, store)
			kept, err := recovered.Restore(ctx)
			if err != nil {
				t.Fatalf("Restore once the file was put back: %v", err)
			}
			if got := pathsOf(kept); !slices.Equal(got, tc.rows) {
				t.Fatalf("the boot laid %v back over the file, want %v", got, tc.rows)
			}
			if cfg := recovered.Live().Snapshot(); cfg.AdmissionCap != 7 || cfg.ReviewRoundCap != 5 {
				t.Fatalf("admission_cap %d and review_round_cap %d after that boot, want the route's 7 and 5",
					cfg.AdmissionCap, cfg.ReviewRoundCap)
			}
		})
	}
}

// A boot whose file has taken back every stored route change lays nothing over the file and
// reaches no apply (applyRoute), and still deletes every one of those rows: the file's own
// values stand, and the next boot finds nothing to lay back.
func TestABootWhoseFileTookBackEveryRowEmptiesTheStore(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	path := configFile(t, minimalFile)
	changes := map[string]any{"admission_cap": 7, "review_round_cap": 5}
	if _, refused, err := keeperOn(t, path, store).Route(ctx, config.CallerOperator, "ana@example.com", changes); err != nil || len(refused) != 0 {
		t.Fatalf("Route: %v, refused %+v", err, refused)
	}

	rewrite(t, path, minimalFile+"admission_cap: 9\nreview_round_cap: 6\n")
	restarted := keeperOn(t, path, store)
	kept, err := restarted.Restore(ctx)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if len(kept) != 0 {
		t.Fatalf("Restore laid %+v back over the file, want nothing: the file took back every key", kept)
	}
	rows, err := store.Rows(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("the store holds %v after the boot, want nothing: every row was taken back", pathsOf(rows))
	}
	if cfg := restarted.Live().Snapshot(); cfg.AdmissionCap != 9 || cfg.ReviewRoundCap != 6 {
		t.Fatalf("admission_cap %d and review_round_cap %d after the boot, want the file's own 9 and 6", cfg.AdmissionCap, cfg.ReviewRoundCap)
	}
}

// A store with no settings table — a database this release has not migrated yet — carries no
// route change, and the boot reads it as none rather than refusing to start. The boot reads the
// rows before it migrates, which is the one moment that table can be missing.
func TestABootBeforeTheMigrationReadsNoStoredChange(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, testdb.Database(t, "legion_settings_unmigrated"))
	if err != nil {
		t.Fatalf("open the test database: %v", err)
	}
	t.Cleanup(st.Close)

	keeper := keeperOn(t, configFile(t, minimalFile), NewStore(st.Pool(), testProject))
	kept, err := keeper.Restore(ctx)
	if err != nil {
		t.Fatalf("Restore against an unmigrated database: %v", err)
	}
	if len(kept) != 0 {
		t.Fatalf("Restore kept %+v, want nothing", kept)
	}
}
