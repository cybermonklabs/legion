package launcher

// A child under a URL file, which no proxy of the generation's serves, can fail its first open on a
// store that answers again before the launcher asks it after the exit. The launcher's own look at
// the store for the child's whole life (storeWitness) ties that exit to the store when the latest
// look met the fault, and leaves a child that fails after a look found the store answering again
// charged, as it is one that fails while the store answers.

import (
	"context"
	"net/url"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/ompsessions"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/testpg/testdb"
)

func TestAURLFileStoreThatFailsTheChildsOpenAndAnswersBeforeTheAskIsTheStoresFault(t *testing.T) {
	for name, tc := range map[string]struct {
		// every is how often the witness looks; down is whether the store fails from the child's
		// start, and stage what the store does once the child has started, before the child opens
		// it; recovers is whether it answers again before the child exits.
		every    time.Duration
		down     bool
		stage    func(t *testing.T, store *testdb.Flapping)
		recovers bool
		want     shimwire.SessionRefusal
	}{
		"the store fails the witness's look and the child's open, and answers again before the ask": {every: time.Hour, down: true,
			stage: func(t *testing.T, store *testdb.Flapping) { awaitAtLeast(t, "connections refused", &store.Refused, 1) }, recovers: true,
			want: shimwire.SessionStoreUnreachable},
		"the store answers the witness's look, then fails the child's open and is still down at the ask": {every: time.Hour,
			stage: func(t *testing.T, store *testdb.Flapping) {
				awaitAtLeast(t, "connections passed", &store.Passed, 1)
				store.Down.Store(true)
			}, want: shimwire.SessionStoreUnreachable},
		"control: the store fails the witness's first look and answers its next, then the child fails on its own": {every: 200 * time.Millisecond, down: true,
			stage: func(t *testing.T, store *testdb.Flapping) {
				awaitAtLeast(t, "connections refused", &store.Refused, 1)
				store.Down.Store(false)
				awaitAtLeast(t, "connections passed", &store.Passed, 1)
			}},
		"control: the store answers and the child fails on its own": {every: time.Hour,
			stage: func(t *testing.T, store *testdb.Flapping) { awaitAtLeast(t, "connections passed", &store.Passed, 1) }},
	} {
		t.Run(name, func(t *testing.T) {
			lookEvery(t, tc.every)
			store := testdb.NewFlapping(t, testdb.Database(t, "legion_launcher_witness_test"), tc.down)
			urlFile := filepath.Join(t.TempDir(), "dsn")
			if err := replaceURLFile(urlFile, store.URL); err != nil {
				t.Fatal(err)
			}
			g := newRig(t)
			marker := filepath.Join(t.TempDir(), "child")
			command := g.childStart(1, filepath.Join(t.TempDir(), "marker"), nil)
			env := append(command.Env, urlStoreChildEnv+"="+marker,
				ompsessions.StorageVariable+"="+ompsessions.SQLStorage, ompsessions.DSNFileVariable+"="+urlFile)
			if got := g.start(shimwire.LauncherStart{ID: "start-1", Generation: 1,
				Argv: []string{command.Argv[0], "-test.run=^TestLauncherURLStoreChild$"}, Env: env}); !got.OK {
				t.Fatalf("start = %#v, want the child started", got)
			}
			tc.stage(t, store)
			markFile(t, marker+".go")
			if store.Down.Load() {
				awaitFile(t, marker+".failed", 30*time.Second)
				if tc.recovers {
					store.Down.Store(false)
				}
				markFile(t, marker+".recovered")
			}
			exit := g.awaitExitWithin(1, time.Minute)
			if exit.Code != 1 || exit.Session != tc.want {
				t.Fatalf("the child's exit = %+v, want status 1 and session refusal %q\nlauncher log:\n%s", exit, tc.want, g.stderr.String())
			}
		})
	}
}

// Under the generation's proxy the child's own open is seen: an instance that answers it that it
// cannot take a connection now (57P03) ends that connection with its FATAL answer, which the proxy
// records (sessionproxy.Proxy.Fault), so the child's exit is the store's even when the instance
// answers again before the launcher's ask. The control is an instance that answers the child, which
// then fails on its own.
func TestAProxiedStoreThatRefusesTheChildsOpenAndAnswersBeforeTheAskIsTheStoresFault(t *testing.T) {
	for name, tc := range map[string]struct {
		down bool
		want shimwire.SessionRefusal
	}{
		"the instance cannot take the child's open (57P03) and answers again before the ask": {down: true, want: shimwire.SessionStoreUnreachable},
		"control: the instance answers the child, which fails on its own":                    {},
	} {
		t.Run(name, func(t *testing.T) {
			dsn := testdb.Database(t, "legion_launcher_proxied_open_test")
			store := testdb.NewFlapping(t, dsn, false)
			database, dialer := sessionDatabaseAt(t, store.URL)
			g := newSessionRig(t, Config{SessionDatabase: database, SessionDialer: dialer.build})
			parsed, err := url.Parse(dsn)
			if err != nil {
				t.Fatal(err)
			}
			password, _ := parsed.User.Password()
			socketURL := (&url.URL{Scheme: "postgres", User: url.UserPassword(database.User, password), Path: "/" + database.Name,
				RawQuery: url.Values{"host": {g.generationDir(1)}, "sslmode": {"disable"}}.Encode()}).String()
			marker := filepath.Join(t.TempDir(), "child")
			command := g.childStart(1, filepath.Join(t.TempDir(), "marker"), nil)
			env := append(command.Env, urlStoreChildEnv+"="+marker, urlStoreChildURLEnv+"="+socketURL,
				ompsessions.StorageVariable+"="+ompsessions.SQLStorage,
				ompsessions.DSNFileVariable+"="+filepath.Join(g.generationDir(1), ompsessions.DSNFileName))
			if got := g.start(shimwire.LauncherStart{ID: "start-1", Generation: 1,
				Argv: []string{command.Argv[0], "-test.run=^TestLauncherURLStoreChild$"}, Env: env}); !got.OK {
				t.Fatalf("start = %#v, want the child started", got)
			}
			store.Down.Store(tc.down)
			markFile(t, marker+".go")
			if tc.down {
				awaitFile(t, marker+".failed", 30*time.Second)
				store.Down.Store(false)
				markFile(t, marker+".recovered")
			}
			exit := g.awaitExitWithin(1, time.Minute)
			if exit.Code != 1 || exit.Session != tc.want {
				t.Fatalf("the child's exit = %+v, want status 1 and session refusal %q\nlauncher log:\n%s", exit, tc.want, g.stderr.String())
			}
		})
	}
}

// The witness keeps looking once the store has answered it: a store that answers its first look,
// then fails the child's own open and stays down until a later look has met it, ties the child's
// exit to the store, even when the store answers again by the launcher's ask after the exit. Each
// step waits for the witness's own looks to end (awaitLook), never for a clock, so a look that is
// slow to end is waited for. The control is a store that answers throughout, which leaves the
// child's error exit its own once the witness has looked at it twice.
func TestAStoreWitnessKeepsWatchingAfterItsFirstAnswer(t *testing.T) {
	lookEvery(t, 100*time.Millisecond)
	store := testdb.NewFlapping(t, testdb.Database(t, "legion_launcher_late_witness_test"), false)
	dial, witness := urlFileWitness(t, store)

	// The store fails the child's own first open, and stays down until a look has met it.
	store.Down.Store(true)
	conn, err := dial.connect(context.Background())
	closeSessions(conn)
	if !ompsessions.StoreFault(err) {
		t.Fatalf("the child's open of the store that went down returned %v, want a store fault", err)
	}
	awaitLook(t, witness, true)

	// The child exits with an error while the store is down; the launcher's ask finds it up.
	witnessed := witness.finish(time.Second)
	store.Down.Store(false)
	if got := childExitFault(dial, nil, nil, witnessed, time.Second); !ompsessions.StoreFault(got) {
		t.Fatalf("a child whose first open failed on a store that went down after the witness's first answer, and that a later look met down, was read as %v; want the store's fault", got)
	}
}

func TestAStoreWitnessWithNoStoreFaultReturnsNone(t *testing.T) {
	lookEvery(t, 100*time.Millisecond)
	store := testdb.NewFlapping(t, testdb.Database(t, "legion_launcher_healthy_witness_test"), false)
	dial, witness := urlFileWitness(t, store)
	awaitLook(t, witness, false)
	if got := childExitFault(dial, nil, nil, witness.finish(time.Second), time.Second); got != nil {
		t.Fatalf("a child that failed while its store answered was read as %v; want its own exit", got)
	}
}

// urlFileWitness starts a witness over a URL file that names store, and returns once its first look
// has ended with the store answering it.
func urlFileWitness(t *testing.T, store *testdb.Flapping) (sessionDial, *storeWitness) {
	t.Helper()
	urlFile := filepath.Join(t.TempDir(), "dsn")
	if err := replaceURLFile(urlFile, store.URL); err != nil {
		t.Fatal(err)
	}
	dial, err := newSessionDial([]string{ompsessions.DSNFileVariable + "=" + urlFile}, "")
	if err != nil {
		t.Fatal(err)
	}
	witness := watchStore(&dial, false)
	if witness == nil {
		t.Fatal("a URL-file store has no witness")
	}
	awaitLook(t, witness, false)
	return dial, witness
}

// awaitLook waits for the witness to end a look that began after awaitLook was called and met a
// store fault when fault is set, or the store answering otherwise, and fails the test by name when
// thirty seconds pass with none: a witness that stopped looking, or one that met the other.
func awaitLook(t *testing.T, w *storeWitness, fault bool) {
	t.Helper()
	w.mu.Lock()
	// A look under way when the test called may have begun before the store's change; the one
	// after it began later.
	after := w.looks + 1
	w.mu.Unlock()
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		w.mu.Lock()
		looks, met := w.looks, w.latest != nil
		w.mu.Unlock()
		if looks > after && met == fault {
			return
		}
	}
	t.Fatalf("the store witness ended no look that met a store fault %t within 30s", fault)
}

// lookEvery has every store witness the test starts look every d.
func lookEvery(t *testing.T, d time.Duration) {
	t.Helper()
	saved := witnessInterval
	witnessInterval = d
	t.Cleanup(func() { witnessInterval = saved })
}

// urlStoreChildEnv marks the test binary re-executed as a child that opens its session store as
// Oh My Pi does at its start (TestLauncherURLStoreChild); its value is the path its markers are
// named after. urlStoreChildURLEnv, when set, is the URL it opens in place of the URL file's: the
// generation's socket, for a child under the proxy.
const (
	urlStoreChildEnv    = "LEGION_LAUNCHER_URL_STORE_CHILD"
	urlStoreChildURLEnv = "LEGION_LAUNCHER_URL_STORE_CHILD_URL"
)

func init() {
	helperChildVariables = append(helperChildVariables, urlStoreChildEnv)
}

// TestLauncherURLStoreChild is a child that, once the marker urlStoreChildEnv names with ".go"
// appears, opens its session store: the URL urlStoreChildURLEnv names, or else the one in the URL
// file OMP_SESSION_SQL_DSN_FILE names. On a store that fails the open it writes that marker with
// ".failed", waits up to ten seconds for ".recovered", and exits 1; on a store that answers it
// exits 1 at once, a failure of its own. Run as a test of its own, outside a launcher, it does
// nothing.
func TestLauncherURLStoreChild(t *testing.T) {
	marker := os.Getenv(urlStoreChildEnv)
	if marker == "" {
		return
	}
	pollFor(marker+".go", 30*time.Second)
	dsn := os.Getenv(urlStoreChildURLEnv)
	var err error
	if dsn == "" {
		dsn, err = ompsessions.ReadDSN(os.Getenv(ompsessions.DSNFileVariable))
	}
	if err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		var conn *pgx.Conn
		conn, err = ompsessions.ConnectURL(ctx, dsn)
		cancel()
		if conn != nil {
			_ = conn.Close(context.Background())
		}
	}
	if err != nil {
		_ = os.WriteFile(marker+".failed", nil, 0o600)
		pollFor(marker+".recovered", 10*time.Second)
	}
	os.Exit(1)
}

// pollFor waits for path to exist, or within to pass, as a child that has no test to fail does.
func pollFor(path string, within time.Duration) {
	for deadline := time.Now().Add(within); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if _, err := os.Stat(path); err == nil {
			return
		}
	}
}

// awaitFile waits for path to exist, and fails the test by name past within.
func awaitFile(t *testing.T, path string, within time.Duration) {
	t.Helper()
	for deadline := time.Now().Add(within); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if _, err := os.Stat(path); err == nil {
			return
		}
	}
	t.Fatalf("%s did not appear within %s", path, within)
}

// markFile makes the marker file path, for a child that waits for it.
func markFile(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
}

// awaitAtLeast waits for n to reach want, and fails the test by name past thirty seconds.
func awaitAtLeast(t *testing.T, what string, n *atomic.Int32, want int32) {
	t.Helper()
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if n.Load() >= want {
			return
		}
	}
	t.Fatalf("%s: %d within 30s, want %d", what, n.Load(), want)
}
