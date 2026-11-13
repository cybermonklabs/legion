package settings

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/testpg/testdb"
)

// watcherFor is a watcher over path at test speed, already running, and the changes it applies.
// The keeper keeps nothing: what the file arm does with the store is the keeper's own test.
func watcherFor(t *testing.T, path string) (*Keeper, chan []config.Change) {
	t.Helper()
	return watcherLogging(t, path, nil, quiet())
}

// watcherLogging is that watcher over a keeper of store (nil keeps nothing), with a logger of
// the test's own, so a test reads the lines an operator would.
func watcherLogging(t *testing.T, path string, store *Store, log *slog.Logger) (*Keeper, chan []config.Change) {
	t.Helper()
	keeper := keeperOn(t, path, store)
	watcher := NewWatcher(keeper, log)
	watcher.Interval, watcher.Debounce = 10*time.Millisecond, 10*time.Millisecond
	settled := make(chan []config.Change, 8)
	watcher.Settled = func(applied []config.Change) { settled <- applied }
	// The watcher's baseline is the state it reads first, and a change made before that reading
	// is part of it: the test waits for that reading before it changes anything.
	read := make(chan struct{}, 64)
	watcher.watched = func() {
		select {
		case read <- struct{}{}:
		default:
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan struct{})
	go func() {
		defer close(done)
		watcher.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	<-read
	return keeper, settled
}

// awaitSettled is the next apply the watcher made, or a failure naming what was expected.
func awaitSettled(t *testing.T, settled chan []config.Change, why string) []config.Change {
	t.Helper()
	select {
	case applied := <-settled:
		return applied
	case <-time.After(10 * time.Second):
		t.Fatalf("%s: the watcher applied nothing within 10s", why)
		return nil
	}
}

// awaitWarnings is the lines the watcher has written at Warn once it has written want of them,
// or a failure naming what was expected. It is how a test of the watcher's log waits for the
// line itself rather than for the reread Settled makes (apply), so a watcher that logged and
// then skipped the reread fails one of the two tests and not both.
func awaitWarnings(t *testing.T, recorder *logRecorder, want int, why string) []string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		lines := recorder.warnings()
		if len(lines) >= want {
			return lines
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: the watcher wrote %d warning(s) within 10s, want %d", why, len(lines), want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A file written in place is read again: the ordinary edit of a legion.yaml on a host.
func TestTheWatcherSettlesAFileWrittenInPlace(t *testing.T) {
	path := configFile(t, minimalFile)
	keeper, settled := watcherFor(t, path)

	rewrite(t, path, minimalFile+"review_round_cap: 9\n")

	applied := awaitSettled(t, settled, "a file written in place")
	if len(applied) != 1 || applied[0].Path != "review_round_cap" || applied[0].New != "9" {
		t.Fatalf("the watcher applied %+v, want review_round_cap 9", applied)
	}
	if got := keeper.Live().Snapshot().ReviewRoundCap; got != 9 {
		t.Fatalf("review_round_cap = %d, want the file's new 9", got)
	}
}

// A Kubernetes ConfigMap mounted as a volume is updated by writing a new timestamped directory
// beside the old one and swapping the `..data` symlink: the file the daemon opened is never
// written. This is that mount, written exactly as the kubelet writes one, and the watcher must
// see it — a watch bound to the file's own inode would see nothing at all, which is why the
// watcher watches the directory.
func TestTheWatcherSettlesAConfigMapWhoseDataSymlinkWasSwapped(t *testing.T) {
	mount := t.TempDir()
	write := func(stamp, body string) {
		t.Helper()
		dir := filepath.Join(mount, stamp)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("make %s: %v", dir, err)
		}
		rewrite(t, filepath.Join(dir, "legion.yaml"), body)
		link := filepath.Join(mount, "..data_tmp")
		if err := os.Symlink(stamp, link); err != nil {
			t.Fatalf("link %s: %v", link, err)
		}
		if err := os.Rename(link, filepath.Join(mount, "..data")); err != nil {
			t.Fatalf("swap ..data: %v", err)
		}
	}
	write("..2026_11_03_05_04_00.1", minimalFile)
	path := filepath.Join(mount, "legion.yaml")
	if err := os.Symlink(filepath.Join("..data", "legion.yaml"), path); err != nil {
		t.Fatalf("link %s: %v", path, err)
	}
	keeper, settled := watcherFor(t, path)

	write("..2026_11_03_06_00_00.2", minimalFile+"admission_cap: 11\n")

	applied := awaitSettled(t, settled, "a ConfigMap whose ..data symlink was swapped")
	if len(applied) != 1 || applied[0].Path != "admission_cap" || applied[0].New != "11" {
		t.Fatalf("the watcher applied %+v, want admission_cap 11", applied)
	}
	if got := keeper.Live().Snapshot().AdmissionCap; got != 11 {
		t.Fatalf("admission_cap = %d, want the mounted file's new 11", got)
	}
}

// A file a setting names is watched through its own directory too, so a credential rotated in
// place — which names no changed setting, since its path has not moved — still reaches the
// daemon, which is what asks its holders to read their files again (Watcher.Settled).
func TestTheWatcherSeesARotatedFileASettingNames(t *testing.T) {
	dir := t.TempDir()
	token := filepath.Join(dir, "operator-token")
	rewrite(t, token, "first-token")
	path := configFile(t, minimalFile+"operator_token_file: "+token+"\n")
	keeper, settled := watcherFor(t, path)
	if got := WatchedFiles(keeper.Live().Snapshot()); len(got) != 1 || got[0] != token {
		t.Fatalf("the watched files are %v, want the operator token file alone", got)
	}

	rewrite(t, token, "second-token")

	if applied := awaitSettled(t, settled, "a credential rotated in place"); len(applied) != 0 {
		t.Fatalf("the rotation applied %+v, want no changed setting: the path did not move", applied)
	}
}

// The watcher applies nothing for the state it starts in: that state is what the daemon booted
// on, and an apply of it would announce a change nobody made.
func TestTheWatcherAppliesNothingUntilSomethingMoves(t *testing.T) {
	_, settled := watcherFor(t, configFile(t, minimalFile))

	select {
	case applied := <-settled:
		t.Fatalf("the watcher applied %+v with nothing changed", applied)
	case <-time.After(200 * time.Millisecond):
	}
}

// kubernetesTokenFiles are the three credential files kubernetesFile itself names, which every
// reading of that fixture watches whatever its Kubernetes block carries.
var kubernetesTokenFiles = []string{
	"/var/run/legion/DISPATCH_TOKEN", "/var/run/legion/ENVOY_TOKEN", "/var/run/legion/OPERATOR_TOKEN",
}

// kubernetesWatching is kubernetesFile with the two files the Kubernetes block itself names
// appended to that block: the cluster credential and the secrets broker's ServiceAccount token.
func kubernetesWatching(kubeconfig, brokerToken string) string {
	return kubernetesFile +
		"    kubeconfig: " + kubeconfig + "\n" +
		"    agent_secrets:\n" +
		"      url: https://broker.internal.example\n" +
		"      token_file: " + brokerToken + "\n"
}

// A Kubernetes deployment that enrols its pods with no secrets broker has no
// `runtime.kubernetes.agent_secrets` at all, and the block is nil there (config's
// readAgentSecrets). The watcher reads every watched file at its first tick, inside the
// daemon's serve group, so reading that nil block's token file took the daemon down at the
// start of every such deployment — and this suite saw none of it, because every other watcher
// test here is a tmux deployment's file.
func TestTheWatcherWatchesAKubernetesFileThatNamesNoAgentSecrets(t *testing.T) {
	path := configFile(t, kubernetesFile)
	keeper, settled := watcherFor(t, path)

	if got := WatchedFiles(keeper.Live().Snapshot()); !slices.Equal(got, kubernetesTokenFiles) {
		t.Fatalf("the watched files are %v, want the three credential files the file names: a block the file does not carry names none", got)
	}

	rewrite(t, path, kubernetesFile+"admission_cap: 11\n")

	applied := awaitSettled(t, settled, "a Kubernetes file that names no agent_secrets")
	if len(applied) != 1 || applied[0].Path != "admission_cap" || applied[0].New != "11" {
		t.Fatalf("the watcher applied %+v, want admission_cap 11", applied)
	}
}

// The Kubernetes block names two files of its own, and each is watched through the directory
// holding it: the cluster credential, and the ServiceAccount token the daemon logs in to the
// secrets broker with, which the kubelet rewrites as it refreshes it.
func TestTheWatcherWatchesEveryFileTheKubernetesBlockNames(t *testing.T) {
	dir := t.TempDir()
	kubeconfig, brokerToken := filepath.Join(dir, "kubeconfig"), filepath.Join(dir, "broker-token")
	rewrite(t, kubeconfig, "apiVersion: v1\nkind: Config\n")
	rewrite(t, brokerToken, "first-token")
	keeper, settled := watcherFor(t, configFile(t, kubernetesWatching(kubeconfig, brokerToken)))

	want := append([]string{kubeconfig, brokerToken}, kubernetesTokenFiles...)
	slices.Sort(want)
	if got := WatchedFiles(keeper.Live().Snapshot()); !slices.Equal(got, want) {
		t.Fatalf("the watched files are %v, want %v: the kubeconfig and the broker's token among them", got, want)
	}

	rewrite(t, brokerToken, "second-token")

	if applied := awaitSettled(t, settled, "the broker's token refreshed in place"); len(applied) != 0 {
		t.Fatalf("the refresh applied %+v, want no changed setting: the path did not move", applied)
	}
}

// Settled is the daemon's one production trigger of the reread of the credential files
// (daemon.go), and a settle the keeper refuses runs it all the same: the paths the daemon is
// running on are exactly where they were, and a rotated operator bearer, NATS seed, Dispatch or
// Envoy bearer, provider key or kubeconfig would otherwise go unread until the file was fixed
// or the daemon restarted — with the bearer an operator rotated to revoke a leaked one still
// accepted. No later tick stands in for this one: the watcher takes the file's new state as
// read before it applies.
//
// The last row is the control: a reading this daemon settles reports its change, so the first
// two rows are the refusal and not the reread.
func TestAReadingTheKeeperRefusesStillRereadsTheCredentials(t *testing.T) {
	for _, tc := range []struct {
		name, reading string
		cap           int
		applied       int
	}{
		{name: "a file that has become the other runtime's", reading: tmuxReading + "admission_cap: 11\n", cap: 4},
		{name: "a file the loader cannot read", reading: kubernetesFile + "admission_cap: [11\n", cap: 4},
		{name: "a file this daemon settles", reading: kubernetesFile + "admission_cap: 11\n", cap: 11, applied: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := configFile(t, kubernetesFile)
			keeper, settled := watcherFor(t, path)

			rewrite(t, path, tc.reading)

			applied := awaitSettled(t, settled, tc.name)
			if len(applied) != tc.applied {
				t.Fatalf("the settle reported %+v, want %d change(s)", applied, tc.applied)
			}
			if cap := keeper.Live().Snapshot().AdmissionCap; cap != tc.cap {
				t.Fatalf("admission_cap after the settle = %d, want %d: a refused reading settles nothing", cap, tc.cap)
			}
		})
	}
}

// A file that has become the other runtime's is logged at Warn, naming the file and both
// runtimes, as the route arm logs such a call (docs/deployment.md, "What a change of each
// `legion.yaml` key needs"): nothing else records it, and the operator whose edit is applying
// nowhere has this line to read.
//
// The second row is the control: a reading this daemon settles writes no such line.
func TestTheWatcherLogsAFileOfTheOtherRuntimeAtWarnNamingBoth(t *testing.T) {
	for _, tc := range []struct {
		name, reading string
		lines         int
	}{
		{name: "the other runtime's reading", reading: tmuxReading, lines: 1},
		{name: "the booted runtime's own reading", reading: kubernetesFile + "admission_cap: 11\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := configFile(t, kubernetesFile)
			recorder := &logRecorder{}
			_, settled := watcherLogging(t, path, nil, slog.New(recorder))

			rewrite(t, path, tc.reading)

			if tc.lines == 0 {
				// A reading this daemon settles writes no line to wait for, and the settle is
				// what says the watcher has read it.
				awaitSettled(t, settled, tc.name)
			}
			warnings := awaitWarnings(t, recorder, tc.lines, tc.name)
			if len(warnings) != tc.lines {
				t.Fatalf("the watcher wrote %d warning(s), want %d:\n%s", len(warnings), tc.lines, strings.Join(warnings, "\n"))
			}
			if tc.lines == 0 {
				return
			}
			for _, want := range []string{"another runtime", path, "tmux", "kubernetes"} {
				if !strings.Contains(warnings[0], want) {
					t.Errorf("the line names no %q:\n\t%s", want, warnings[0])
				}
			}
		})
	}
}

// A settle whose holder applied the reading and whose store would not delete the route changes
// the file took back is a settle: the daemon runs on the file's values, and the watcher hands
// those changes to Settled as it does any settle's. Its log line says the file is settled and
// names what is left standing — the rows the next boot lays back over the file — rather than
// telling the operator nothing settled while the daemon runs on what did.
func TestASettleWhoseStoreWriteFailedIsReportedSettled(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, testdb.Database(t, "legion_settings_watcher_write"))
	if err != nil {
		t.Fatalf("open the test database: %v", err)
	}
	t.Cleanup(st.Close)
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate the test database: %v", err)
	}
	path := configFile(t, minimalFile)
	stored := NewStore(st.Pool(), testProject)
	if _, _, err := keeperOn(t, path, stored).Route(ctx, config.CallerOperator, "ana@example.com", map[string]any{"admission_cap": 7}); err != nil {
		t.Fatalf("Route: %v", err)
	}
	// The store reads its rows and refuses to delete one, so the settle below reaches the
	// holder, which applies the reading, and fails at the deletion after it.
	if _, err := st.Pool().Exec(ctx, `create function refuse_delete() returns trigger language plpgsql as $$
		begin raise exception 'the store refuses deletions'; end $$;
		create trigger refuse_delete before delete on settings_overlay for each row execute function refuse_delete()`); err != nil {
		t.Fatalf("make the store refuse deletions: %v", err)
	}
	recorder := &logRecorder{}
	_, settled := watcherLogging(t, path, stored, slog.New(recorder))

	rewrite(t, path, minimalFile+"admission_cap: 9\n")

	applied := awaitSettled(t, settled, "a file whose taken-back row the store would not delete")
	if len(applied) != 1 || applied[0].Path != "admission_cap" || applied[0].New != "9" {
		t.Fatalf("Settled was handed %+v, want admission_cap at the file's 9: the holder applied the reading", applied)
	}
	lines := recorder.at(slog.LevelError)
	if len(lines) != 1 {
		t.Fatalf("the watcher wrote %d error line(s), want 1:\n%s", len(lines), strings.Join(lines, "\n"))
	}
	if !strings.Contains(lines[0], "is settled") || strings.Contains(lines[0], "could not be settled") {
		t.Errorf("the line does not say the file is settled:\n\t%s", lines[0])
	}
}
