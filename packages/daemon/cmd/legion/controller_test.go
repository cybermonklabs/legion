package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/controller"
	"github.com/sjawhar/legion/daemon/internal/daemon"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/fixtureenv"
	"github.com/sjawhar/legion/daemon/internal/ghconfig"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/shellprefix"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testnkeys"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

const (
	controllerProject       = "demo"
	controllerOperatorToken = "op-tok-7f3a"
)

// memoryController is the project's controller record held in memory: what the daemon's store
// holds, with every capability hash it was handed kept in order.
type memoryController struct {
	mu     sync.Mutex
	record controller.Record
	minted [][]byte
}

func (m *memoryController) mint(hash []byte) uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.record = controller.Record{Generation: m.record.Generation + 1, CapabilityHash: hash}
	m.minted = append(m.minted, hash)
	return m.record.Generation
}

// MintControllerThen is the in-memory mint's committed continuation.
func (m *memoryController) MintControllerThen(_ context.Context, _ string, hash []byte, onCommit func(generation uint64)) (uint64, error) {
	if onCommit == nil {
		return 0, errors.New("a controller mint needs its commit hook")
	}
	generation := m.mint(hash)
	onCommit(generation)
	return generation, nil
}

func (m *memoryController) Controller(context.Context, string) (controller.Record, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.record, m.record.Generation != 0, nil
}

func (m *memoryController) RegisterController(_ context.Context, _ string, generation uint64, session string, secretHash []byte, at time.Time, pluginContract int) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if generation != m.record.Generation {
		return false, nil
	}
	m.record.Session, m.record.SecretHash, m.record.RegisteredAt, m.record.PluginContract = session, secretHash, at, pluginContract
	return true, nil
}

// statusWrites is Dispatch as the issue status route reaches it: every status it was asked to
// set. Any other Dispatch call is a test failure (the embedded nil Client panics).
type statusWrites struct {
	dispatch.Client
	mu     sync.Mutex
	writes []string
}

func (s *statusWrites) SetStatus(_ context.Context, key, status string) (dispatch.Issue, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.writes = append(s.writes, key+"="+status)
	return dispatch.Issue{}, nil
}

// controllerDaemon is the daemon's HTTP surface as `legion controller start` and `legion issue status`
// reach it: the real api.NewServer over an in-memory controller record, grants, and Dispatch, on
// a loopback port, with every request that reached it recorded.
type controllerDaemon struct {
	t          *testing.T
	url        string
	port       int
	controller *memoryController
	dispatch   *statusWrites

	mu   sync.Mutex
	seen []seenRequest
}

func newControllerDaemon(t *testing.T) *controllerDaemon {
	return newControllerDaemonGated(t, config.DesignGateRootIssues.Steps())
}

// newControllerDaemonGated is newControllerDaemon for a project whose `gates.design` gates steps.
func newControllerDaemonGated(t *testing.T, steps []record.DesignStep) *controllerDaemon {
	return newControllerDaemonWithOptions(t, controllerDaemonOptions{steps: steps})
}

// newControllerDaemonWithTokens is newControllerDaemon whose GitHub credential route mints review
// App tokens from tokens for a project under two owners, acme and sjawhar, for the tests of the
// controller's GitHub credential fetch and refresh loop.
func newControllerDaemonWithTokens(t *testing.T, tokens appauth.Source) *controllerDaemon {
	return newControllerDaemonWithOptions(t, controllerDaemonOptions{steps: config.DesignGateRootIssues.Steps(), tokens: tokens,
		repos: []ghrepo.Repository{ghrepo.MustParse("sjawhar/legion"), ghrepo.MustParse("acme/widgets")}})
}

// controllerDaemonOptions configures newControllerDaemonWithOptions: the design-gate steps the
// project gates, the repositories the project's live records name (for the state route), and,
// for the tests of the controller's GitHub credential route, a Tokens fake, the repositories the
// daemon serves and the deployment's configured renames, which decide the directories its bundle
// holds beside the ones of the owners the fake says the App is installed on.
type controllerDaemonOptions struct {
	steps        []record.DesignStep
	tokens       appauth.Source
	repos        []ghrepo.Repository
	renames      ghrepo.Renames
	repositories []string
	// rawStateBody, set, replaces GET /legion/v1/state's whole response body verbatim (for a
	// malformed or structurally invalid body a real projection never produces: `{}`, `null`, a
	// missing repositories field, or an invalid live entry).
	rawStateBody string
	// stateStatus, set, replaces GET /legion/v1/state's HTTP status (for a transient failure, e.g.
	// 503), leaving rawStateBody (or the real handler's own body) as the body.
	stateStatus int
}

// servesOneOf is the `served` predicate of a daemon serving repos (api.Options.Served): whether
// GitHub answered both role Apps installed on a repository, which here is whether it is one of
// repos, compared as GitHub compares names, regardless of case.
func servesOneOf(repos []ghrepo.Repository) func(ghrepo.Repository) bool {
	return func(repo ghrepo.Repository) bool {
		return slices.ContainsFunc(repos, func(served ghrepo.Repository) bool { return strings.EqualFold(served.String(), repo.String()) })
	}
}

// fakeTx is the no-op pgx.Tx the controller test daemon's state route opens: fixedStateSource
// never reads from it, so the only calls it sees are the route's own — the bound it puts on its
// statements, and its Commit and deferred Rollback.
type fakeTx struct{}

func (fakeTx) Begin(context.Context) (pgx.Tx, error) { return fakeTx{}, nil }
func (fakeTx) Commit(context.Context) error          { return nil }
func (fakeTx) Rollback(context.Context) error        { return nil }
func (fakeTx) CopyFrom(context.Context, pgx.Identifier, []string, pgx.CopyFromSource) (int64, error) {
	return 0, errors.New("unexpected copy")
}
func (fakeTx) SendBatch(context.Context, *pgx.Batch) pgx.BatchResults { return nil }
func (fakeTx) LargeObjects() pgx.LargeObjects                         { return pgx.LargeObjects{} }
func (fakeTx) Prepare(context.Context, string, string) (*pgconn.StatementDescription, error) {
	return nil, errors.New("unexpected prepare")
}

// Exec takes the one statement the state route runs of its own on this transaction, the bound on
// its statements, and refuses anything else by name: every other statement would be a state
// source's, and this daemon's source runs none.
func (fakeTx) Exec(_ context.Context, sql string, _ ...any) (pgconn.CommandTag, error) {
	if strings.HasPrefix(sql, "set local statement_timeout = ") {
		return pgconn.CommandTag{}, nil
	}
	return pgconn.CommandTag{}, errors.New("unexpected exec: " + sql)
}
func (fakeTx) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, errors.New("unexpected query")
}
func (fakeTx) QueryRow(context.Context, string, ...any) pgx.Row { return nil }
func (fakeTx) Conn() *pgx.Conn                                  { return nil }

// fixedStateTransactions always opens the same no-op tx.
type fixedStateTransactions struct{}

func (fixedStateTransactions) BeginTx(context.Context, pgx.TxOptions) (pgx.Tx, error) {
	return fakeTx{}, nil
}

// fixedStateSource is the controller test daemon's api.StateSource: a state holding only the
// repositories the live records name, each with both role Apps installed, as `GET
// /legion/v1/state` serves them for the read `legion controller start` makes before the mint
// (checkControllerRepositories), in the order given, as the daemon serves them sorted. A nil
// repositories (the zero value, most tests' default) stands in for a normally configured daemon
// and answers one fixed repository; a non-nil empty slice is the explicit "no record is live" case
// a test must ask for by name. The controller's addressing names none of them: it names the owners
// of its gh bundle, which these repositories do not decide. It answers no claims: this daemon
// knows no caller's pod, so the route reads none.
type fixedStateSource struct{ repositories []string }

func (f fixedStateSource) State(context.Context, pgx.Tx) (api.State, []supervise.Claim, error) {
	repos := f.repositories
	if repos == nil {
		repos = []string{"acme/widgets"}
	}
	live := make([]api.LiveRepositoryView, len(repos))
	for i, repo := range repos {
		live[i] = api.LiveRepositoryView{Repo: repo, Apps: &api.AppInstallationsView{Implement: true, Review: true}}
	}
	return api.State{Repositories: api.RepositoriesView{Live: live}}, nil, nil
}

// newControllerDaemonWithOptions is the daemon's HTTP surface as `legion controller start` and
// `legion issue status` reach it: the real api.NewServer over an in-memory controller record, grants,
// and Dispatch, on a loopback port, with every request that reached it recorded.
func newControllerDaemonWithOptions(t *testing.T, opts controllerDaemonOptions) *controllerDaemon {
	t.Helper()
	d := &controllerDaemon{t: t, controller: &memoryController{}, dispatch: &statusWrites{}}
	handler := api.NewServer("127.0.0.1", 0, api.Options{
		Project: controllerProject, OperatorToken: controllerOperatorToken, Controller: d.controller,
		Steps: opts.steps, Dispatch: d.dispatch, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Tokens: opts.tokens, Served: servesOneOf(opts.repos), RepositoryRenames: opts.renames,
		State: fixedStateSource{repositories: opts.repositories}, StateTransactions: fixedStateTransactions{},
	}).Handler
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read the request body: %v", err)
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, r)
		code, answer := recorder.Code, recorder.Body.Bytes()
		if r.Method == http.MethodGet && r.URL.Path == "/legion/v1/state" {
			if opts.rawStateBody != "" {
				answer = []byte(opts.rawStateBody)
			}
			if opts.stateStatus != 0 {
				code = opts.stateStatus
			}
		}
		d.mu.Lock()
		d.seen = append(d.seen, seenRequest{
			method: r.Method, path: r.URL.Path, authorization: r.Header.Get("Authorization"),
			contentType: r.Header.Get("Content-Type"), body: body, status: code, answer: answer,
		})
		d.mu.Unlock()
		for name, values := range recorder.Header() {
			w.Header()[name] = values
		}
		w.WriteHeader(code)
		_, _ = w.Write(answer)
	}))
	t.Cleanup(server.Close)
	d.url = server.URL
	d.port = server.Listener.Addr().(*net.TCPAddr).Port
	return d
}

func (d *controllerDaemon) requests() []seenRequest {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.seen)
}

// operatorMachine is one operator's machine: a controller.yaml naming every key, the files it
// points at, a HOME of its own, an Oh My Pi that records how it was launched and exits, and the
// real gh its LEGION_GH_PATH names.
type operatorMachine struct {
	t          *testing.T
	daemon     *controllerDaemon
	dir, home  string
	config     string
	tokenFile  string
	record     string
	defaultDir string
	gh         string
}

type controllerOptions struct {
	// lines replace the default controller.yaml wholesale.
	lines []string
	// tokenMode is the operator token file's mode; tokenContents its contents.
	tokenMode     os.FileMode
	tokenContents string
	omitToken     bool
	// dispatchBroker writes the default controller.yaml with `dispatch_token_broker: true` in
	// place of its `dispatch_token_file`: the controller's Dispatch credential is this machine's
	// own identity, traded at the secrets broker.
	dispatchBroker bool
	exitCode       int
	// waitForFile, when set, has the recording Oh My Pi wait until that file exists before it
	// records argv, env, and cwd and exits with exitCode, so a test of the refresh loop keeps
	// Oh My Pi running exactly until it has observed what it needs, whatever the machine's load,
	// and then releases it by creating the file, which lies in a t.TempDir of the test's own
	// (releaseWait).
	waitForFile string
}

// dispatchCredentialLine is the default file's line for the Dispatch credential: the bearer in a
// file, or this machine's own identity traded at the secrets broker. The two exclude each other.
func dispatchCredentialLine(broker bool) string {
	if broker {
		return "dispatch_token_broker: true"
	}
	return "dispatch_token_file: ./dispatch-token"
}

func newControllerStart(t *testing.T, d *controllerDaemon, opts controllerOptions) *operatorMachine {
	t.Helper()
	dir, home, record := t.TempDir(), t.TempDir(), t.TempDir()
	c := &operatorMachine{
		t: t, daemon: d, dir: dir, home: home, record: record,
		config:     filepath.Join(dir, "controller.yaml"),
		tokenFile:  filepath.Join(dir, "operator-token"),
		defaultDir: filepath.Join(home, ".local", "state", "legion", controllerProject+"-controller"),
	}
	if !opts.omitToken {
		contents := opts.tokenContents
		if contents == "" {
			contents = controllerOperatorToken + "\n"
		}
		mode := opts.tokenMode
		if mode == 0 {
			mode = 0o600
		}
		c.write("operator-token", contents, mode)
	}
	c.write("instructions.md", "Always be kind.\n", 0o600)
	c.write("envoy-token", "envoy-bearer\n", 0o600)
	c.write("dispatch-token", "dispatch-bearer\n", 0o600)
	c.write("nats-seed", testnkeys.UserSeed(t)+"\n", 0o600)
	lines := opts.lines
	if lines == nil {
		lines = []string{
			"project: " + controllerProject,
			"daemon_url: " + d.url,
			"operator_token_file: ./operator-token",
			"envoy_url: http://envoy.test:9020",
			"envoy_token_file: envoy-token",
			"nats_urls: [nats://a:4222, nats://b:4222]",
			"nats_nkey_seed_file: ./nats-seed",
			"dispatch_url: https://dispatch.test",
			dispatchCredentialLine(opts.dispatchBroker),
			"instructions: ./instructions.md",
			"omp_launch_prefix: [env, LEGION_TEST_PREFIX_RAN=1]",
		}
	}
	c.write("controller.yaml", strings.Join(lines, "\n")+"\n", 0o600)

	// The Oh My Pi LEGION_OMP_PATH names. As the load probe (`models --extension <probe>`) it
	// records its environment and working directory and reports, as the probe extension does,
	// pi-legion loaded from the file `loaded-from` names, or not loaded when there is none;
	// pi-envoy publishing the interface `envoy-interface` holds (1 when absent; `none` for no
	// pi-envoy) from the file `envoy-loaded-from` names; and the pre-split package loaded from the
	// file `legacy-loaded-from` names, when there is one. As the controller it records its argv,
	// a copy of the file its --append-system-prompt names and that file's mode (the command removes
	// the file once Oh My Pi exits), environment, and working directory, and exits with the code
	// the test asked for. Under LEGION_TEST_HOLD it waits for that release (releaseWait) and copies
	// its prompt file only then.
	omp := filepath.Join(record, "omp")
	script := fmt.Sprintf(`#!/bin/sh
if [ "$1" = models ]; then
  env -0 >%[1]s/probe-env
  pwd -P >%[1]s/probe-cwd
  readlink /proc/self/fd/0 >%[1]s/probe-stdin
  if [ -f %[1]s/loaded-from ]; then
    printf 'LEGION_PLUGIN_LOADED=yes\nLEGION_PLUGIN_LOADED_FROM=%%s\nLEGION_PLUGIN_ENVOY_INTERFACE=1\n' "$(cat %[1]s/loaded-from)" >&2
  else
    echo LEGION_PLUGIN_LOADED=no >&2
  fi
  interface=$(cat %[1]s/envoy-interface 2>/dev/null || echo 1)
  if [ "$interface" = none ]; then
    echo LEGION_ENVOY_INTERFACE=none >&2
  else
    printf 'LEGION_ENVOY_INTERFACE=%%s\nLEGION_ENVOY_LOADED_FROM=%%s\n' "$interface" "$(cat %[1]s/envoy-loaded-from)" >&2
  fi
  [ ! -f %[1]s/legacy-loaded-from ] || printf 'LEGION_LEGACY_PLUGIN_LOADED_FROM=%%s\n' "$(cat %[1]s/legacy-loaded-from)" >&2
  exit 0
fi
[ -f "$GH_CONFIG_DIR/hosts.yml" ] && cp "$GH_CONFIG_DIR/hosts.yml" %[1]s/hosts-at-start
%[3]s
if [ -n "$LEGION_TEST_HOLD" ]; then
  : >%[1]s/held
  hold_dir=$(dirname "$LEGION_TEST_HOLD")
%[4]s  [ "$1" = --append-system-prompt ] && cp "$2" %[1]s/held-prompt
  exit 0
fi
if [ "$1" = --append-system-prompt ]; then cp "$2" %[1]s/system-prompt; stat -c %%a "$2" >%[1]s/system-prompt-mode; fi
for a in "$@"; do printf '%%s\0' "$a"; done >%[1]s/argv
readlink /proc/self/fd/0 >%[1]s/stdin
env -0 >%[1]s/env
pwd >%[1]s/cwd
exit %[2]d
`, record, opts.exitCode, waitForFileScript(opts.waitForFile), releaseWait(`"$LEGION_TEST_HOLD"`, `"$hold_dir"`))
	if err := os.WriteFile(omp, []byte(script), 0o700); err != nil {
		t.Fatalf("write the recording omp: %v", err)
	}
	// Oh My Pi's directories are this HOME's: the plugin gate reads the process environment through
	// ompdirs.ProfileRoot, so each variable that would move them is cleared, OMP_CONFIG_ROOT, which
	// ProfileRoot honours over PI_CONFIG_DIR, included.
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("OMP_PROFILE", "")
	t.Setenv("PI_CONFIG_DIR", "")
	t.Setenv("OMP_CONFIG_ROOT", "")
	t.Setenv("PI_CODING_AGENT_DIR", "")
	t.Setenv("PATH", "/usr/bin:/bin")
	t.Setenv("LEGION_OMP_PATH", omp)
	// The real gh the controller's `legion gh` runs, which the operator's environment names here so
	// the run never depends on a gh this machine may or may not have.
	c.gh = filepath.Join(record, "gh")
	if err := os.WriteFile(c.gh, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LEGION_GH_PATH", c.gh)
	// The operator runs the command at a terminal; `go test`'s stdin is none.
	isTerminal := stdinIsTerminal
	stdinIsTerminal = func() bool { return true }
	t.Cleanup(func() { stdinIsTerminal = isTerminal })
	c.installPlugin(api.DaemonAPIVersion())
	return c
}

// waitForFileScript is the recording Oh My Pi's wait for file (releaseWait). Empty when file is
// empty, so a test with no loop to wait for records and exits at once.
func waitForFileScript(file string) string {
	if file == "" {
		return ""
	}
	return releaseWait(shellprefix.Word(file), shellprefix.Word(filepath.Dir(file)))
}

// releaseWait is the recording Oh My Pi's wait for the release the test creates at file, a shell
// word, in dir, the test's own t.TempDir: a poll every tenth of a second that ends once file
// exists, or once the test has ended (its cleanup removed dir) or this test binary has. No span
// of its own ends it, so Oh My Pi outlives every wait the test makes, however slow the machine,
// and a test that never releases it still ends it.
func releaseWait(file, dir string) string {
	return fmt.Sprintf("while [ ! -e %s ] && [ -d %s ] && kill -0 %d 2>/dev/null; do sleep 0.1; done\n", file, dir, os.Getpid())
}

// installPlugin installs, in the operator's default Oh My Pi profile, a pi-legion manifest
// declaring contract and a pi-envoy manifest beside it, and has the recording Oh My Pi load both.
func (c *operatorMachine) installPlugin(contract int) {
	c.t.Helper()
	c.loads(c.installPluginAt(filepath.Join(c.home, ".omp"), contract))
}

// loads has the recording Oh My Pi report pi-legion loaded from the package directory dir, and
// pi-envoy from the pi-envoy package beside it, as the probe extension renders each: a file URL of
// its dist/legion.js or dist/envoy.js with a cache-busting query.
func (c *operatorMachine) loads(dir string) {
	c.t.Helper()
	c.tells("loaded-from", "file://"+filepath.Join(dir, "dist", "legion.js")+"?mtime=1")
	c.tells("envoy-loaded-from", "file://"+filepath.Join(filepath.Dir(dir), "pi-envoy", "dist", "envoy.js")+"?mtime=1")
}

// loadsNothing has the recording Oh My Pi report pi-legion not loaded.
func (c *operatorMachine) loadsNothing() {
	c.t.Helper()
	if err := os.Remove(filepath.Join(c.record, "loaded-from")); err != nil && !os.IsNotExist(err) {
		c.t.Fatal(err)
	}
}

// loadsEnvoyAt has the recording Oh My Pi report pi-envoy publishing plugin interface version;
// "none" is no pi-envoy loaded.
func (c *operatorMachine) loadsEnvoyAt(version string) {
	c.t.Helper()
	c.tells("envoy-interface", version)
}

// loadsLegacyFrom has the recording Oh My Pi report the pre-split package loaded from from.
func (c *operatorMachine) loadsLegacyFrom(from string) {
	c.t.Helper()
	c.tells("legacy-loaded-from", from)
}

// tells writes what the recording Oh My Pi reads under name.
func (c *operatorMachine) tells(name, value string) {
	c.t.Helper()
	if err := os.WriteFile(filepath.Join(c.record, name), []byte(value), 0o600); err != nil {
		c.t.Fatal(err)
	}
}

// probeEnv is the environment the load probe's Oh My Pi ran under.
func (c *operatorMachine) probeEnv() map[string]string {
	c.t.Helper()
	return readEnv(c.t, filepath.Join(c.record, "probe-env"))
}

// installPluginAt installs a pi-legion manifest declaring contract, and a pi-envoy manifest, under
// the Oh My Pi data root root, and answers pi-legion's package directory.
func (c *operatorMachine) installPluginAt(root string, contract int) string {
	c.t.Helper()
	plugins := filepath.Join(root, "plugins", "node_modules", "@sjawhar")
	for name, manifest := range map[string]string{
		"pi-legion": fmt.Sprintf(`{"name":"@sjawhar/pi-legion","version":"9.9.9","legion":{"daemonApiVersion":%d}}`, contract),
		"pi-envoy":  `{"name":"@sjawhar/pi-envoy","version":"9.9.9"}`,
	} {
		if err := os.MkdirAll(filepath.Join(plugins, name), 0o700); err != nil {
			c.t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(plugins, name, "package.json"), []byte(manifest), 0o600); err != nil {
			c.t.Fatal(err)
		}
	}
	return filepath.Join(plugins, "pi-legion")
}

func (c *operatorMachine) write(name, contents string, mode os.FileMode) {
	c.t.Helper()
	path := filepath.Join(c.dir, name)
	if err := os.WriteFile(path, []byte(contents), mode); err != nil {
		c.t.Fatalf("write %s: %v", path, err)
	}
	if err := os.Chmod(path, mode); err != nil {
		c.t.Fatalf("chmod %s: %v", path, err)
	}
}

// run is `legion controller start --config <file> [extra…]`; the operator token is in neither
// stream, whatever happened.
func (c *operatorMachine) run(extra ...string) (int, string, string) {
	c.t.Helper()
	var out, errb bytes.Buffer
	code := run(context.Background(), append([]string{"legion", "controller", "start", "--config", c.config}, extra...), &out, &errb)
	for name, stream := range map[string]string{"stdout": out.String(), "stderr": errb.String()} {
		if strings.Contains(stream, controllerOperatorToken) {
			c.t.Fatalf("legion controller start printed the operator token on %s: %q", name, stream)
		}
	}
	return code, out.String(), errb.String()
}

// refused runs the command and requires it to fail with want on stderr.
func (c *operatorMachine) refused(want string, extra ...string) {
	c.t.Helper()
	code, _, errb := c.run(extra...)
	if code != 1 || !strings.Contains(errb, want) {
		c.t.Fatalf("legion controller start = %d, stderr %q; want 1 and %q", code, errb, want)
	}
}

// launched says whether the recording Oh My Pi ran.
func (c *operatorMachine) launched() bool {
	_, err := os.Stat(filepath.Join(c.record, "argv"))
	return err == nil
}

func (c *operatorMachine) argv() []string {
	c.t.Helper()
	raw, err := os.ReadFile(filepath.Join(c.record, "argv"))
	if err != nil {
		c.t.Fatalf("Oh My Pi was not launched: %v", err)
	}
	return strings.Split(strings.TrimSuffix(string(raw), "\x00"), "\x00")
}

// systemPrompt is the prompt Oh My Pi was started with: the contents of the file its one
// --append-system-prompt value names, as Oh My Pi reads a value with no newline, copied by the
// recording Oh My Pi while it ran.
func (c *operatorMachine) systemPrompt() string {
	c.t.Helper()
	argv := c.argv()
	if len(argv) != 3 || argv[0] != "--append-system-prompt" {
		c.t.Fatalf("Oh My Pi's argv = %q, want --append-system-prompt <file> and the start word", argv)
	}
	prompt, err := os.ReadFile(filepath.Join(c.record, "system-prompt"))
	if err != nil {
		c.t.Fatalf("read the prompt file Oh My Pi was started with: %v", err)
	}
	return string(prompt)
}

func (c *operatorMachine) env() map[string]string {
	c.t.Helper()
	return readEnv(c.t, filepath.Join(c.record, "env"))
}

// readEnv reads an `env -0` recording.
func readEnv(t *testing.T, path string) map[string]string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("Oh My Pi was not launched: %v", err)
	}
	env := map[string]string{}
	for _, entry := range strings.Split(strings.TrimSuffix(string(raw), "\x00"), "\x00") {
		name, value, _ := strings.Cut(entry, "=")
		env[name] = value
	}
	return env
}

func (c *operatorMachine) wantNoSecretRequest() {
	c.t.Helper()
	if requests := c.daemon.requests(); len(requests) != 0 {
		c.t.Fatalf("%d requests reached the daemon, want none: first %s %s", len(requests), requests[0].method, requests[0].path)
	}
}

// wantNoMintRequest is wantNoSecretRequest for a check that runs after the repositories fetch
// (which itself reaches the daemon, GET /legion/v1/state): it asserts no POST
// /legion/v1/controller/secret reached the daemon, so a refusal of the repositories block never
// mints — or revokes — a capability.
func (c *operatorMachine) wantNoMintRequest() {
	c.t.Helper()
	for _, r := range c.daemon.requests() {
		if r.method == http.MethodPost && r.path == "/legion/v1/controller/secret" {
			c.t.Fatalf("a POST /legion/v1/controller/secret reached the daemon, want none")
		}
	}
}

func (c *operatorMachine) wantNothingLaunchedOrWritten(stateDir string) {
	c.t.Helper()
	if c.launched() {
		c.t.Fatal("Oh My Pi was launched")
	}
	if _, err := os.Stat(stateDir); !os.IsNotExist(err) {
		c.t.Fatalf("the state directory %s exists (%v); want nothing written", stateDir, err)
	}
}

func modeOf(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return info.Mode().Perm()
}

// The repositories block is read before anything mints a capability: GET /legion/v1/state, then
// the one call that mints it, POST /legion/v1/controller/secret with the operator token as a
// bearer and the contract the probe held the plugin to as the body, then the github-credential
// fetch.
func TestControllerStartFetchesTheSecretWithTheOperatorBearer(t *testing.T) {
	d := newControllerDaemon(t)
	c := newControllerStart(t, d, controllerOptions{})
	if code, _, errb := c.run(); code != 0 {
		t.Fatalf("legion controller start = %d, stderr %q", code, errb)
	}
	requests := d.requests()
	if len(requests) != 3 {
		t.Fatalf("%d requests reached the daemon, want the state read, the secret request, and the github-credential fetch", len(requests))
	}
	state := requests[0]
	if state.method != http.MethodGet || state.path != "/legion/v1/state" || state.authorization != "" || state.status != http.StatusOK {
		t.Fatalf("state request = %s %s auth %q → %d, want an unauthenticated GET", state.method, state.path, state.authorization, state.status)
	}
	got := requests[1]
	if got.method != http.MethodPost || got.path != "/legion/v1/controller/secret" ||
		got.authorization != "Bearer "+controllerOperatorToken || got.contentType != "application/json" ||
		string(got.body) != fmt.Sprintf(`{"pluginContract":%d}`, api.DaemonAPIVersion()) || got.status != http.StatusOK {
		t.Fatalf("secret request = %s %s auth %q type %q body %q → %d", got.method, got.path, got.authorization, got.contentType, got.body, got.status)
	}
}

// Under the default state directory: the minted capability 0600 in a 0700 secrets directory, the
// gh shim, the legion launcher, the deployment instructions, and the controller's working
// directory.
func TestControllerStartWritesTheSecretAndTheControllersFilesUnderTheStateDirectory(t *testing.T) {
	d := newControllerDaemon(t)
	c := newControllerStart(t, d, controllerOptions{})
	if code, _, errb := c.run(); code != 0 {
		t.Fatalf("legion controller start = %d, stderr %q", code, errb)
	}
	secretFile := filepath.Join(c.defaultDir, "secrets", "legion-demo-controller")
	secret, err := os.ReadFile(secretFile)
	if err != nil {
		t.Fatalf("read the secret file: %v", err)
	}
	if sum := sha256.Sum256(secret); len(d.controller.minted) != 1 || !bytes.Equal(d.controller.minted[0], sum[:]) {
		t.Fatalf("the secret file does not hold the capability the daemon minted")
	}
	if got := modeOf(t, secretFile); got != 0o600 {
		t.Errorf("secret file mode %o, want 0600", got)
	}
	if got := modeOf(t, filepath.Join(c.defaultDir, "secrets")); got != 0o700 {
		t.Errorf("secrets directory mode %o, want 0700", got)
	}
	if got := modeOf(t, filepath.Join(c.defaultDir, "bin", "legion")); got&0o111 == 0 {
		t.Errorf("legion launcher mode %o, want executable", got)
	}
	instructions, err := os.ReadFile(filepath.Join(c.defaultDir, "deployment-instructions.md"))
	if err != nil || !strings.HasPrefix(string(instructions), "# Deployment instructions (demo)\n\nAlways be kind.") {
		t.Errorf("deployment instructions = %q, %v", instructions, err)
	}
	cwd, err := os.ReadFile(filepath.Join(c.record, "cwd"))
	if err != nil || strings.TrimSpace(string(cwd)) != filepath.Join(c.defaultDir, "controller") {
		t.Errorf("Oh My Pi ran in %q (%v), want %s", cwd, err, filepath.Join(c.defaultDir, "controller"))
	}
}

// Oh My Pi runs through the launch prefix, interactive (no --mode rpc, no --resume), with one
// --append-system-prompt naming the file that holds the controller prompt, the daemon's design
// gate steps, the reach of the controller's gh, and the deployment instructions, then the one
// word daemon.ControllerStartCommand; under the operator's own environment plus exactly the
// shared controller environment — the secrets as file pointers, never values — including
// LEGION_CONTROLLER_START_MESSAGE, which that pi-legion command sends as the session's first
// turn. This default daemon has no GitHub App, so the controller's gh acts as nobody and its
// prompt says its App is installed on no owner, whatever repositories the live records name.
func TestControllerStartLaunchesOhMyPiWithTheSharedControllerEnvironment(t *testing.T) {
	d := newControllerDaemon(t)
	c := newControllerStart(t, d, controllerOptions{})
	baseline := map[string]string{}
	for _, entry := range os.Environ() {
		name, value, _ := strings.Cut(entry, "=")
		baseline[name] = value
	}
	code, _, errb := c.run()
	if code != 0 {
		t.Fatalf("legion controller start = %d, stderr %q", code, errb)
	}

	// The controller's role part, then the daemon's part for a controller the operator started.
	var controllerPrompt []byte
	for _, part := range []string{filepath.Join("shared", "controller-root.md"), filepath.Join("go", "controller-interactive.md")} {
		body, err := os.ReadFile(filepath.Join(c.defaultDir, "prompts", part))
		if err != nil {
			t.Fatalf("read the controller prompt snapshot: %v", err)
		}
		controllerPrompt = append(controllerPrompt, body...)
	}
	instructions, err := os.ReadFile(filepath.Join(c.defaultDir, "deployment-instructions.md"))
	if err != nil {
		t.Fatalf("read the deployment instructions: %v", err)
	}
	// The one value of --append-system-prompt is this run's own prompt file under the secrets
	// directory, never the text. The start message travels as LEGION_CONTROLLER_START_MESSAGE;
	// argv ends with the command that sends it.
	argv := c.argv()
	if len(argv) != 3 || argv[0] != "--append-system-prompt" || argv[2] != "/legion-controller-start" {
		t.Fatalf("Oh My Pi's argv = %q, want --append-system-prompt <file> /legion-controller-start", argv)
	}
	promptFile := argv[1]
	if dir, name := filepath.Split(promptFile); dir != filepath.Join(c.defaultDir, "secrets")+"/" ||
		!strings.HasPrefix(name, "legion-demo-controller-") || !strings.HasSuffix(name, "-system-prompt.md") {
		t.Fatalf("the prompt file is %s, want legion-demo-controller-<run>-system-prompt.md under %s", promptFile, filepath.Join(c.defaultDir, "secrets"))
	}
	// Each file's trailing newlines are dropped where the parts join.
	wantPrompt := strings.TrimRight(string(controllerPrompt), "\n") + "\n\nDesign gate steps: `spec`. A root that registers its Verification document as `criteria` " +
		"is held on that step too. Your role's GitHub App is installed on no owner, so you have no GitHub owner and its token reaches no repository.\n\n" +
		strings.TrimRight(string(instructions), "\n")
	if got := c.systemPrompt(); got != wantPrompt {
		t.Fatalf("the controller's prompt file held %q\nwant %q", got, wantPrompt)
	}
	if mode, err := os.ReadFile(filepath.Join(c.record, "system-prompt-mode")); err != nil || strings.TrimSpace(string(mode)) != "600" {
		t.Errorf("the controller's prompt file had mode %q (%v) while Oh My Pi ran, want 600", mode, err)
	}
	if _, err := os.Stat(promptFile); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the controller's prompt file is still there once the command ended: %v", err)
	}

	env := c.env()
	added := map[string]string{}
	for name, value := range env {
		if old, ok := baseline[name]; !ok || old != value {
			added[name] = value
		}
	}
	// What sh itself sets is not the command's.
	for _, shells := range []string{"PWD", "OLDPWD", "SHLVL", "_"} {
		delete(added, shells)
	}
	secrets := filepath.Join(c.defaultDir, "secrets")
	bin := filepath.Join(c.defaultDir, "bin")
	want := map[string]string{
		"LEGION_TEST_PREFIX_RAN":          "1",
		"LEGION_CONTROLLER":               "1",
		"LEGION_ROLE":                     "controller",
		"LEGION_DAEMON_URL":               d.url,
		"LEGION_PROJECT":                  "demo",
		"LEGION_STATE_DIR":                c.defaultDir,
		"ENVOY_NATS_URL":                  "nats://a:4222,nats://b:4222",
		"ENVOY_URL":                       "http://envoy.test:9020",
		"PATH":                            bin + ":/usr/bin:/bin",
		"PI_SHELL_PREFIX":                 shellprefix.For(bin),
		"GH_CONFIG_DIR":                   filepath.Join(c.defaultDir, "gh"),
		"GH_TOKEN":                        "",
		"GITHUB_TOKEN":                    "",
		"GH_HOST":                         "",
		"LEGION_GH_OWNERS_DIR":            filepath.Join(c.defaultDir, "gh"),
		"LEGION_GH_PATH":                  c.gh,
		"LEGION_CONTROLLER_START_MESSAGE": daemon.ControllerStartMessage,
		"PERSON_SIGNIN_TRADE":             "off",
		"DISPATCH_URL":                    "https://dispatch.test",
		"DISPATCH_TOKEN_FILE":             filepath.Join(c.dir, "dispatch-token"),
		"LEGION_CONTROLLER_SECRET_FILE":   filepath.Join(secrets, "legion-demo-controller"),
		"ENVOY_TOKEN_FILE":                filepath.Join(c.dir, "envoy-token"),
		"NATS_NKEY_SEED_FILE":             filepath.Join(c.dir, "nats-seed"),
	}
	for name := range want {
		if old, ok := baseline[name]; ok && old == want[name] {
			// Set before the run to the value the command sets: not observable as added.
			delete(want, name)
		}
	}
	if len(added) != len(want) {
		t.Errorf("added environment has %d keys, want %d:\n got %v\nwant %v", len(added), len(want), added, want)
	}
	for name, value := range want {
		if added[name] != value {
			t.Errorf("%s = %q, want %q", name, added[name], value)
		}
	}
	secret, err := os.ReadFile(filepath.Join(secrets, "legion-demo-controller"))
	if err != nil {
		t.Fatalf("read the secret file: %v", err)
	}
	for name, value := range env {
		if strings.Contains(value, string(secret)) {
			t.Errorf("%s carries the controller secret's value", name)
		}
	}
	for _, name := range []string{"LEGION_CONTROLLER_SECRET", "ENVOY_TOKEN", "DISPATCH_TOKEN", "NATS_NKEY_SEED"} {
		if _, set := env[name]; set {
			t.Errorf("%s is set; only its _FILE pointer may be", name)
		}
	}
	// Three lines: the probe, named before it runs so a prompt or a wait can be tied to it; the
	// no-App line, since this default daemon has no Tokens; then the launch.
	wantProbe := fmt.Sprintf("[legion] checking the controller's Oh My Pi (env 'LEGION_TEST_PREFIX_RAN=1' %s) in %s before the daemon mints a capability\n",
		filepath.Join(c.record, "omp"), filepath.Join(c.defaultDir, "controller"))
	wantNoApp := "[legion] the daemon has no GitHub App to act as; the controller's gh acts as nobody\n"
	wantLog := fmt.Sprintf("[legion] starting the controller for demo against %s; state in %s\n", d.url, c.defaultDir)
	if errb != wantProbe+wantNoApp+wantLog {
		t.Errorf("stderr = %q, want %q", errb, wantProbe+wantNoApp+wantLog)
	}
}

// Under `omp_invocation: mise x <tool> -- omp`, the Oh My Pi mise starts, the load probe's and the
// controller's, has the controller's launcher directory first on its PATH, ahead of whatever mise
// put first. The stand-in mise answers `where <tool>` with an install whose bin/omp is the
// recording Oh My Pi, and runs `x <tool> -- <program> <args…>` as mise runs it under an operator's
// `_.path` entry: a dotfiles shims directory first on PATH, then the program, exec'd.
func TestControllerStartUnderMiseKeepsTheLauncherDirectoryFirstOnOhMyPisPath(t *testing.T) {
	d := newControllerDaemon(t)
	c := newControllerStart(t, d, controllerOptions{})
	const tool = "github:sjawhar/oh-my-pi@18.1.21"
	scratch := t.TempDir()
	install, shims, mise := filepath.Join(scratch, "install"), filepath.Join(scratch, ".dotfiles", "shims"), filepath.Join(scratch, "mise")
	if err := os.MkdirAll(filepath.Join(install, "bin"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(c.record, "omp"), filepath.Join(install, "bin", "omp")); err != nil {
		t.Fatal(err)
	}
	script := fmt.Sprintf(`#!/bin/sh
if [ "$1" = where ] && [ "$2" = %[1]s ]; then
  echo %[2]s
  exit 0
fi
if [ "$1" = x ] && [ "$2" = %[1]s ] && [ "$3" = -- ]; then
  shift 3
  PATH=%[3]s:$PATH
  exec "$@"
fi
echo "stand-in mise: unexpected argv: $*" >&2
exit 64
`, shellprefix.Word(tool), shellprefix.Word(install), shellprefix.Word(shims))
	if err := os.WriteFile(mise, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LEGION_OMP_PATH", "")
	t.Setenv("LEGION_MISE_PATH", mise)
	config, err := os.ReadFile(c.config)
	if err != nil {
		t.Fatal(err)
	}
	c.write("controller.yaml", string(config)+"omp_invocation: mise x "+tool+" -- omp\n", 0o600)
	if code, _, errb := c.run(); code != 0 {
		t.Fatalf("legion controller start = %d, stderr %q", code, errb)
	}
	bin := filepath.Join(c.defaultDir, "bin")
	want := bin + ":" + shims + ":" + bin + ":/usr/bin:/bin"
	for name, env := range map[string]map[string]string{"the load probe's": c.probeEnv(), "the controller's": c.env()} {
		if got := env["PATH"]; got != want {
			t.Errorf("%s Oh My Pi ran with PATH %s, want %s: the controller's launcher directory first, then the PATH mise made", name, got, want)
		}
	}
}

// Two runs of the command overlap, as they do when the operator starts it again while an earlier
// controller still runs (the new capability revokes the old), and each run's Oh My Pi reads the
// prompt file it was handed: one run's exit removes its own file and no other run's. The held run's
// Oh My Pi waits (LEGION_TEST_HOLD) until a second run has written its prompt, launched and
// exited, and only then reads its own file.
func TestOverlappingControllerStartsEachKeepTheirOwnPromptFile(t *testing.T) {
	d := newControllerDaemon(t)
	c := newControllerStart(t, d, controllerOptions{})
	release := filepath.Join(t.TempDir(), "release")
	t.Setenv("LEGION_TEST_HOLD", release)
	held := make(chan int, 1)
	go func() {
		code, _, _ := c.run()
		held <- code
	}()
	testwait.WithinPolling(t, testwait.Bounded(t, 1<<60, 30*time.Second), testwait.Poll, "the held run's Oh My Pi to start and wait", func() bool {
		_, err := os.Stat(filepath.Join(c.record, "held"))
		return err == nil
	})
	t.Setenv("LEGION_TEST_HOLD", "")
	if code, _, errb := c.run(); code != 0 {
		t.Fatalf("the second run of legion controller start = %d, stderr %q", code, errb)
	}
	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if code := testwait.Receive(t, "the held run of legion controller start to end", held); code != 0 {
		t.Fatalf("the held run of legion controller start = %d", code)
	}
	got, err := os.ReadFile(filepath.Join(c.record, "held-prompt"))
	if err != nil || !strings.Contains(string(got), "Design gate steps: `spec`.") {
		t.Fatalf("the held run's Oh My Pi read %q (%v) from the prompt file it was handed, want the controller prompt: the other run's exit removed it", got, err)
	}
	left, err := filepath.Glob(filepath.Join(c.defaultDir, "secrets", "*-system-prompt.md"))
	if err != nil || len(left) != 0 {
		t.Errorf("prompt files left once both runs ended: %q (%v), want none", left, err)
	}
}

// The controller is told the design-gate steps the daemon really gates, so under a deployment
// that gates none its take comment promises no design approval that cannot happen, and under
// one that gates several it names each (skill://legion-controller). `legion controller start`
// accepts every answer the route gives, so each deployment launches. The sentence after the
// steps is the reach of the controller's gh, which this daemon, having no GitHub App, gives none.
func TestControllerStartTellsTheControllerTheDaemonsGatedSteps(t *testing.T) {
	for _, tc := range []struct {
		name  string
		steps []record.DesignStep
		line  string
	}{
		{"two steps", []record.DesignStep{record.DesignStepCriteria, record.DesignStepSpec}, "Design gate steps: `criteria`, `spec`."},
		{"root-issues", config.DesignGateRootIssues.Steps(),
			"Design gate steps: `spec`. A root that registers its Verification document as `criteria` is held on that step too."},
		{"off", config.DesignGateOff.Steps(), "Design gate steps: none."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := newControllerDaemonGated(t, tc.steps)
			c := newControllerStart(t, d, controllerOptions{})
			if code, _, errb := c.run(); code != 0 {
				t.Fatalf("legion controller start = %d, stderr %q", code, errb)
			}
			want := "\n\n" + tc.line + " Your role's GitHub App is installed on no owner, so you have no GitHub owner and its token reaches no repository.\n\n"
			if prompt := c.systemPrompt(); !strings.Contains(prompt, want) {
				t.Fatalf("Oh My Pi's system prompt = %q; want it to carry %q", prompt, want)
			}
		})
	}
}

// The operator-launched controller's addressing gains the reach its GitHub App gives it after
// its design gate steps, as a sentence of its own: the owner accounts the daemon read the review
// App installed on as it built the gh bundle it has just answered (controllerReach). They are
// not the owners of the repositories the live records name (GET /legion/v1/state): here the
// records name acme/widgets alone and the review App is installed on example-org too, so the
// controller's gh holds both directories and its prompt names both. A prompt built from those
// repositories told the controller it reached acme alone. It names no repository, since the App
// installation and not any list of repositories decides where the project may work (INFRA-2543).
func TestControllerStartsAddressingGainsTheReachItsAppGives(t *testing.T) {
	tokens := newControllerCredentialTokens("ghs_first")
	tokens.installOn("acme", "example-org")
	d := newControllerDaemonWithOptions(t, controllerDaemonOptions{
		steps: config.DesignGateOff.Steps(), tokens: tokens,
		repos: []ghrepo.Repository{ghrepo.MustParse("acme/widgets")}, repositories: []string{"acme/widgets"},
	})
	c := newControllerStart(t, d, controllerOptions{})
	if code, _, errb := c.run(); code != 0 {
		t.Fatalf("legion controller start = %d, stderr %q", code, errb)
	}
	for _, owner := range []string{"acme", "example-org"} {
		if _, err := os.Stat(filepath.Join(c.defaultDir, "gh", owner, ghconfig.HostsFile)); err != nil {
			t.Fatalf("the controller's gh holds no token for %s (%v), so this case proves nothing", owner, err)
		}
	}
	want := "\n\nDesign gate steps: none. Your role's GitHub App is installed on the owners acme, example-org, and its token reaches every repository of those owners.\n\n"
	if prompt := c.systemPrompt(); !strings.Contains(prompt, want) {
		t.Fatalf("Oh My Pi's system prompt = %q; want it to contain %q", prompt, want)
	}
}

// The sentence names an owner only where both of its halves are true of it: the review App is
// installed on the account, and the controller's gh holds a token of it. Neither of the two
// lists the daemon answers is that set by itself, so the controller is told their intersection
// (controllerReach), and this holds the two ways they differ.
//
// The bundle's directories hold an alias: the pair sjawhar/legion -> cybermonklabs/legion
// resolves, so the bundle carries a sjawhar directory holding cybermonklabs' lease, on an
// account the App is not installed on. A sentence built from the directories, which is what
// this change first did, names sjawhar as an owner the App is installed on. The installations
// hold an owner with no token: the App is installed on acme and GitHub refuses its reading
// lease there, so the bundle holds no acme directory. A sentence built from the installations
// alone says the controller's token reaches every repository of acme.
func TestControllerStartsAddressingNamesNeitherARenameAliasNorAnOwnerItsGhHoldsNoTokenOf(t *testing.T) {
	tokens := newControllerCredentialTokens("ghs_first")
	tokens.installOn("acme", "cybermonklabs")
	tokens.refuse("acme")
	d := newControllerDaemonWithOptions(t, controllerDaemonOptions{
		steps: config.DesignGateOff.Steps(), tokens: tokens,
		repos:        []ghrepo.Repository{ghrepo.MustParse("cybermonklabs/legion")},
		renames:      ghrepo.MustRenames(ghrepo.MustRename("sjawhar/legion", "cybermonklabs/legion")),
		repositories: []string{"cybermonklabs/legion"},
	})
	c := newControllerStart(t, d, controllerOptions{})
	if code, _, errb := c.run(); code != 0 {
		t.Fatalf("legion controller start = %d, stderr %q", code, errb)
	}

	// The two lists as the daemon answered them, which are the two sources a sentence could be
	// built from: the directories the controller's gh holds, and the installations.
	var answer api.ControllerCredentialResponse
	for _, request := range d.requests() {
		if request.path == "/legion/v1/controller/github-credential" {
			if err := json.Unmarshal(request.answer, &answer); err != nil {
				t.Fatalf("decode the daemon's answer %s: %v", request.answer, err)
			}
		}
	}
	dirs := slices.Sorted(maps.Keys(answer.Owners))
	if want := []string{"cybermonklabs", "sjawhar"}; !slices.Equal(dirs, want) {
		t.Fatalf("the bundle holds the directories %q, want %q: the alias sjawhar and no acme, so this case proves nothing otherwise", dirs, want)
	}
	if want := []string{"acme", "cybermonklabs"}; !slices.Equal(answer.Installed, want) {
		t.Fatalf("the daemon answered installed %q, want %q: acme, whose lease GitHub refuses, and no sjawhar, so this case proves nothing otherwise", answer.Installed, want)
	}
	for _, owner := range dirs {
		if _, err := os.Stat(filepath.Join(c.defaultDir, "gh", owner, ghconfig.HostsFile)); err != nil {
			t.Fatalf("the controller's gh holds no token for %s (%v)", owner, err)
		}
	}

	// The prompt rides a file since #4190, so the sentence is read from the file Oh My Pi was
	// given rather than from argv, which holds its path.
	argv := c.argv()
	if len(argv) != 3 {
		t.Fatalf("Oh My Pi's argv = %q, want three words", argv)
	}
	prompt := c.systemPrompt()
	want := "\n\nDesign gate steps: none. Your role's GitHub App is installed on the owner cybermonklabs, and its token reaches every repository of that owner.\n\n"
	if !strings.Contains(prompt, want) {
		t.Errorf("Oh My Pi's system prompt = %q; want it to contain %q", prompt, want)
	}
	// The negative controls: the sentence each source alone would have written is not the
	// controller's, and each of them names the owner the other half is false of.
	for _, control := range []struct {
		source string
		owners []string
		names  string
	}{
		{"the bundle's directories", dirs, "sjawhar"},
		{"the installations alone", answer.Installed, "acme"},
	} {
		sentence := daemon.RepositoriesSentence(control.owners)
		if !strings.Contains(sentence, control.names) {
			t.Fatalf("the sentence built from %s is %q, want it to name %s: the control proves nothing otherwise", control.source, sentence, control.names)
		}
		if strings.Contains(prompt, sentence) {
			t.Errorf("the controller's prompt carries %q, the sentence built from %s, which names %s", sentence, control.source, control.names)
		}
	}
}

// A daemon whose repositories block is missing or names an invalid repository refuses the
// controller's start outright, naming why — it never silently omits the repositories sentence. An
// empty `live`, or one whose every repository a role App is missing from, is never refused here
// (the two tests below): the daemon boots and keeps running exactly this way on purpose, and the
// controller is what drives recovery by admitting work once an App is installed.
func TestControllerStartRefusesAnUnusableRepositoriesBlock(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts controllerDaemonOptions
		want string
	}{
		{"empty object body", controllerDaemonOptions{rawStateBody: "{}"}, "names no repositories field"},
		{"null body", controllerDaemonOptions{rawStateBody: "null"}, "names no repositories field"},
		{"missing repositories field", controllerDaemonOptions{rawStateBody: `{"daemon":{"project":"demo"}}`}, "names no repositories field"},
		{"an invalid live entry", controllerDaemonOptions{repositories: []string{"not a valid repo"}}, "an invalid live repository"},
		{
			"one valid, one invalid entry: the whole list is refused, not narrowed",
			controllerDaemonOptions{repositories: []string{"acme/widgets", "not a valid repo"}},
			"an invalid live repository",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := newControllerDaemonWithOptions(t, tc.opts)
			c := newControllerStart(t, d, controllerOptions{})
			c.refused(tc.want)
			c.wantNoMintRequest()
			c.wantNothingLaunchedOrWritten(c.defaultDir)
		})
	}
}

// A `live` whose every repository a role App is missing from (a real, fully-booted state) launches
// on both paths, rather than refusing and burning a launch budget the App install cannot recover
// from on its own. The controller is told the owners its App is installed on and its gh holds a
// token of, which the repositories the live records name do not decide: here the review App is
// installed on example-org and not on acme, so acme/widgets reads the review App missing and the
// controller's gh still reaches example-org. A prompt built from the served repositories said the
// App was installed on no owner while the gh directory held that token. The repositories are
// named in the state and never in the addressing, which names no repository at all.
func TestControllerStartLaunchesWhereNoLiveRepositoryHasBothApps(t *testing.T) {
	tokens := newControllerCredentialTokens("ghs_first")
	tokens.installOn("example-org")
	d := newControllerDaemonWithOptions(t, controllerDaemonOptions{
		steps:        config.DesignGateOff.Steps(),
		tokens:       tokens,
		rawStateBody: `{"repositories":{"live":[{"repo":"acme/widgets","apps":{"implement":true,"review":false}}],"caches":[]}}`,
	})
	c := newControllerStart(t, d, controllerOptions{})
	if code, _, errb := c.run(); code != 0 {
		t.Fatalf("legion controller start = %d, stderr %q", code, errb)
	}
	if _, err := os.Stat(filepath.Join(c.defaultDir, "gh", "example-org", ghconfig.HostsFile)); err != nil {
		t.Fatalf("the controller's gh holds no token for example-org (%v), so this case proves nothing", err)
	}
	want := "\n\nDesign gate steps: none. Your role's GitHub App is installed on the owner example-org, and its token reaches every repository of that owner.\n\n"
	if prompt := c.systemPrompt(); !strings.Contains(prompt, want) {
		t.Fatalf("Oh My Pi's system prompt = %q; want it to contain %q", prompt, want)
	}
}

// An empty `live` (no workflow reached, or no record live) also launches. This daemon has no
// GitHub App, so the controller is told its App is installed on no owner — the same wording and
// behavior on both launch paths.
func TestControllerStartLaunchesWithNoLiveRepository(t *testing.T) {
	d := newControllerDaemonWithOptions(t, controllerDaemonOptions{steps: config.DesignGateOff.Steps(), repositories: []string{}})
	c := newControllerStart(t, d, controllerOptions{})
	if code, _, errb := c.run(); code != 0 {
		t.Fatalf("legion controller start = %d, stderr %q", code, errb)
	}
	want := "Your role's GitHub App is installed on no owner, so you have no GitHub owner and its token reaches no repository.\n\n"
	if prompt := c.systemPrompt(); !strings.Contains(prompt, want) {
		t.Fatalf("Oh My Pi's system prompt = %q; want it to contain %q", prompt, want)
	}
}

// A transient read failure (a non-2xx GET /legion/v1/state) is a refusal, never a launch on an
// assumed empty block: "read failed" and "read succeeded with no live repository" are different
// outcomes of checkControllerRepositories, and only the former refuses. This is what keeps a
// running controller from being revoked for a replacement that never starts.
func TestControllerStartNeverMintsWhenTheRepositoryReadFails(t *testing.T) {
	d := newControllerDaemonWithOptions(t, controllerDaemonOptions{stateStatus: http.StatusServiceUnavailable})
	c := newControllerStart(t, d, controllerOptions{})
	c.refused("/legion/v1/state")
	c.wantNoMintRequest()
	c.wantNothingLaunchedOrWritten(c.defaultDir)
}

// A daemon that answers the secret without the steps it gates is refused, never guessed at: the
// controller would otherwise promise, or withhold, a design approval on an assumption. A daemon
// from before the list names no `designGateSteps` at all, and one beyond this release may name a
// step this one does not know; the empty list is neither, and is the deployment that gates
// nothing.
func TestControllerSecretWithoutTheConfiguredStepsIsRefused(t *testing.T) {
	for _, tc := range []struct{ body, want string }{
		{`{"secret":"s"}`, "answered no design gate steps (designGateSteps); upgrade the daemon"},
		{`{"secret":"s","designGateSteps":null}`, "answered no design gate steps (designGateSteps); upgrade the daemon"},
		{`{"secret":"s","designGateSteps":["decisions","sometimes"]}`, `answered a design gate step (designGateSteps[1]) that is not 'decisions', 'criteria' or 'spec'; upgrade the daemon`},
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(tc.body))
		}))
		_, _, err := fetchControllerSecret(context.Background(), server.URL, controllerOperatorToken, api.DaemonAPIVersion())
		server.Close()
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("fetchControllerSecret(%s) error = %v; want it to name %q", tc.body, err, tc.want)
		}
	}
	// The empty list is a deployment that gates no step, which starts the controller.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"secret":"s","designGateSteps":[]}`))
	}))
	defer server.Close()
	secret, steps, err := fetchControllerSecret(context.Background(), server.URL, controllerOperatorToken, api.DaemonAPIVersion())
	if err != nil || secret != "s" || steps == nil || len(steps) != 0 {
		t.Fatalf("fetchControllerSecret of the empty list = %q, %v, %v; want the secret and a list that gates nothing", secret, steps, err)
	}
}

func TestControllerStartOmitsTheOptionalPointersTheFileDoesNotSet(t *testing.T) {
	d := newControllerDaemon(t)
	c := newControllerStart(t, d, controllerOptions{lines: []string{
		"project: demo", "daemon_url: " + d.url, "operator_token_file: ./operator-token",
		"envoy_url: http://envoy.test:9020", "nats_urls: [nats://a:4222]",
	}})
	unsetRestored(t, startupRestored...)
	if code, _, errb := c.run(); code != 0 {
		t.Fatalf("legion controller start = %d, stderr %q", code, errb)
	}
	env := c.env()
	for _, name := range startupRestored {
		if _, set := env[name]; set {
			t.Errorf("%s is set with no key naming it", name)
		}
	}
	if env["LEGION_CONTROLLER"] != "1" {
		t.Errorf("LEGION_CONTROLLER = %q, want 1", env["LEGION_CONTROLLER"])
	}
}

func TestControllerStartExitsWithOhMyPisCode(t *testing.T) {
	d := newControllerDaemon(t)
	c := newControllerStart(t, d, controllerOptions{exitCode: 3})
	if code, _, _ := c.run(); code != 3 {
		t.Fatalf("legion controller start = %d, want Oh My Pi's 3", code)
	}
}

// --daemon-url replaces the file's daemon_url for the secret request and for LEGION_DAEMON_URL.
func TestControllerStartDaemonURLOverridesTheFile(t *testing.T) {
	d := newControllerDaemon(t)
	c := newControllerStart(t, d, controllerOptions{lines: []string{
		"project: demo", "daemon_url: http://127.0.0.1:1", "operator_token_file: ./operator-token",
		"envoy_url: http://envoy.test:9020", "nats_urls: [nats://a:4222]",
	}})
	if code, _, errb := c.run("--daemon-url", d.url+"/"); code != 0 {
		t.Fatalf("legion controller start = %d, stderr %q", code, errb)
	}
	if n := len(d.requests()); n != 3 {
		t.Fatalf("%d requests reached the overriding daemon, want the secret request, the github-credential fetch, and the state read", n)
	}
	if got := c.env()["LEGION_DAEMON_URL"]; got != d.url {
		t.Fatalf("LEGION_DAEMON_URL = %q, want %q", got, d.url)
	}
}

// A 403 names the URL and the one cause the route has: every Go daemon serves it, since none boots
// without operator_token_file. Nothing is written and nothing launched.
func TestControllerStartRefusedByTheDaemonWritesNothing(t *testing.T) {
	d := newControllerDaemon(t)
	c := newControllerStart(t, d, controllerOptions{tokenContents: "wrong-9f3c27e1b04d\n"})
	code, _, errb := c.run()
	want := d.url + "/legion/v1/controller/secret: the daemon answered 403 Forbidden: Invalid operator token — the operator token does not match the daemon's operator_token_file\n"
	if code != 1 || !strings.HasSuffix(errb, want) {
		t.Fatalf("legion controller start = %d, stderr %q; want 1 and a refusal ending %q", code, errb, want)
	}
	c.wantNothingLaunchedOrWritten(c.defaultDir)
}

// A daemon from before contract 12 decodes the secret request strictly into an empty body, so it
// refuses the contract field with a 400 naming it as unknown. That refusal says which side to
// upgrade instead of quoting the daemon, and nothing is launched or written.
func TestControllerStartAgainstADaemonBeforeContract12NamesTheSideToUpgrade(t *testing.T) {
	old := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/legion/v1/state" {
			// A contract-11 daemon still serves state with a repositories object, in the shape
			// of its own contract, which names no invalid live repository: the repositories
			// fetch, which runs before the mint, must succeed here so the test reaches the
			// contract refusal it is actually about.
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"repositories":{"mapped":[{"repo":"acme/widgets","default":true}]}}`))
			return
		}
		// The contract-11 daemon's controllerSecret: readBody into struct{}, unknown fields refused.
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		var empty struct{}
		if err := decoder.Decode(&empty); err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid request body: " + err.Error()})
			return
		}
		t.Errorf("the contract-11 stand-in accepted %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(old.Close)
	c := newControllerStart(t, newControllerDaemon(t), controllerOptions{lines: []string{
		"project: demo", "daemon_url: " + old.URL, "operator_token_file: ./operator-token",
		"envoy_url: http://envoy.test:9020", "nats_urls: [nats://a:4222]",
	}})
	code, _, errb := c.run()
	want := fmt.Sprintf("%s/legion/v1/controller/secret: the daemon speaks a daemon API contract older than 12, and this legion speaks %d; upgrade the daemon to this legion's release, or start the controller with the legion built with that daemon\n", old.URL, api.DaemonAPIVersion())
	if code != 1 || !strings.HasSuffix(errb, want) {
		t.Fatalf("legion controller start = %d, stderr %q; want 1 and a refusal ending %q", code, errb, want)
	}
	c.wantNothingLaunchedOrWritten(c.defaultDir)
}

// An unreachable daemon is named, and no other address is tried.
func TestControllerStartUnreachableDaemonIsNamedAndNoOtherAddressTried(t *testing.T) {
	d := newControllerDaemon(t)
	unreachable := "http://127.0.0.1:1"
	c := newControllerStart(t, d, controllerOptions{lines: []string{
		"project: demo", "daemon_url: " + unreachable, "operator_token_file: ./operator-token",
		"envoy_url: http://envoy.test:9020", "nats_urls: [nats://a:4222]",
	}})
	c.refused("could not reach the Legion daemon at " + unreachable + ": ")
	c.refused("; is the port-forward running? (never falls back to another address)")
	c.wantNoSecretRequest()
	c.wantNothingLaunchedOrWritten(c.defaultDir)
}

// Every refusal the operator's machine can find on its own comes before the one request: the
// daemon revokes the incumbent controller's capability on every mint, so a local failure must
// never cut it off for nothing.
func TestControllerStartRefusesLocallyBeforeTheRequest(t *testing.T) {
	t.Run("a group-readable operator token file, naming the path and mode", func(t *testing.T) {
		d := newControllerDaemon(t)
		c := newControllerStart(t, d, controllerOptions{tokenMode: 0o640})
		c.refused(fmt.Sprintf("operator_token_file %s is readable by its group or others (mode 0640); chmod 0600 it", c.tokenFile))
		c.wantNoSecretRequest()
		c.wantNothingLaunchedOrWritten(c.defaultDir)
	})
	// With a positional word and a stdin that is no terminal, Oh My Pi runs headless print mode,
	// which would run the start command after the mint revoked the running controller.
	t.Run("a stdin that is not a terminal, before any probe or request", func(t *testing.T) {
		d := newControllerDaemon(t)
		c := newControllerStart(t, d, controllerOptions{})
		stdinIsTerminal = func() bool { return false }
		c.refused("stdin is not a terminal: Oh My Pi would run headless print mode")
		c.refused("run `legion controller start` at a terminal")
		c.wantNoSecretRequest()
		if _, err := os.Stat(filepath.Join(c.record, "probe-env")); !os.IsNotExist(err) {
			t.Fatalf("the load probe ran (%v); want no probe", err)
		}
		c.wantNothingLaunchedOrWritten(c.defaultDir)
	})
	t.Run("a group-readable operator token file is refused before a stdin that is not a terminal", func(t *testing.T) {
		d := newControllerDaemon(t)
		c := newControllerStart(t, d, controllerOptions{tokenMode: 0o640})
		stdinIsTerminal = func() bool { return false }
		c.refused(fmt.Sprintf("operator_token_file %s is readable by its group or others (mode 0640); chmod 0600 it", c.tokenFile))
		c.wantNoSecretRequest()
	})
	t.Run("a missing operator token file", func(t *testing.T) {
		d := newControllerDaemon(t)
		c := newControllerStart(t, d, controllerOptions{omitToken: true})
		c.refused(fmt.Sprintf("operator_token_file names %s, which could not be read: ", c.tokenFile))
		c.wantNoSecretRequest()
	})
	t.Run("a blank operator token file", func(t *testing.T) {
		d := newControllerDaemon(t)
		c := newControllerStart(t, d, controllerOptions{tokenContents: " \n"})
		c.refused(fmt.Sprintf("operator_token_file names %s, which is empty", c.tokenFile))
		c.wantNoSecretRequest()
	})
	t.Run("a blank Envoy token file", func(t *testing.T) {
		d := newControllerDaemon(t)
		c := newControllerStart(t, d, controllerOptions{})
		c.write("envoy-token", "\n", 0o600)
		c.refused(fmt.Sprintf("envoy_token_file names %s, which is empty", filepath.Join(c.dir, "envoy-token")))
		c.wantNoSecretRequest()
	})
	t.Run("a blank NATS nkey seed file", func(t *testing.T) {
		d := newControllerDaemon(t)
		c := newControllerStart(t, d, controllerOptions{})
		c.write("nats-seed", " \n", 0o600)
		c.refused(fmt.Sprintf("nats_nkey_seed_file names %s, which is empty", filepath.Join(c.dir, "nats-seed")))
		c.wantNoSecretRequest()
		c.wantNothingLaunchedOrWritten(c.defaultDir)
	})
	t.Run("a missing NATS nkey seed file", func(t *testing.T) {
		d := newControllerDaemon(t)
		c := newControllerStart(t, d, controllerOptions{})
		os.Remove(filepath.Join(c.dir, "nats-seed"))
		c.refused(fmt.Sprintf("nats_nkey_seed_file names %s, which could not be read: ", filepath.Join(c.dir, "nats-seed")))
		c.wantNoSecretRequest()
	})
	t.Run("a NATS nkey seed file holding no user seed", func(t *testing.T) {
		d := newControllerDaemon(t)
		c := newControllerStart(t, d, controllerOptions{})
		seed := testnkeys.Account(t)
		c.write("nats-seed", seed+"\n", 0o600)
		want := fmt.Sprintf("nats_nkey_seed_file (%s) holds an nkey seed that is not a user's", filepath.Join(c.dir, "nats-seed"))
		if code, _, errb := c.run(); code != 1 || !strings.Contains(errb, want) || strings.Contains(errb, seed) {
			t.Fatalf("legion controller start = %d, stderr %q; want 1 and %q, without the seed", code, errb, want)
		}
		c.wantNoSecretRequest()
	})
	t.Run("a NATS nkey seed file its group can read", func(t *testing.T) {
		d := newControllerDaemon(t)
		c := newControllerStart(t, d, controllerOptions{})
		if err := os.Chmod(filepath.Join(c.dir, "nats-seed"), 0o640); err != nil {
			t.Fatal(err)
		}
		c.refused(fmt.Sprintf("nats_nkey_seed_file %s is readable by its group or others (mode 0640); chmod 0600 it", filepath.Join(c.dir, "nats-seed")))
		c.wantNoSecretRequest()
	})
	t.Run("a missing Dispatch token file", func(t *testing.T) {
		d := newControllerDaemon(t)
		c := newControllerStart(t, d, controllerOptions{})
		os.Remove(filepath.Join(c.dir, "dispatch-token"))
		c.refused(fmt.Sprintf("dispatch_token_file names %s, which could not be read: ", filepath.Join(c.dir, "dispatch-token")))
		c.wantNoSecretRequest()
	})
	t.Run("a missing instructions file", func(t *testing.T) {
		d := newControllerDaemon(t)
		c := newControllerStart(t, d, controllerOptions{})
		os.Remove(filepath.Join(c.dir, "instructions.md"))
		c.refused("instructions file " + filepath.Join(c.dir, "instructions.md") + " could not be read: ")
		c.wantNoSecretRequest()
		c.wantNothingLaunchedOrWritten(c.defaultDir)
	})
	t.Run("a blank instructions file", func(t *testing.T) {
		d := newControllerDaemon(t)
		c := newControllerStart(t, d, controllerOptions{})
		c.write("instructions.md", " \n", 0o600)
		c.refused("instructions file " + filepath.Join(c.dir, "instructions.md") + " is empty")
		c.wantNoSecretRequest()
	})
	t.Run("an Oh My Pi invocation it cannot resolve", func(t *testing.T) {
		d := newControllerDaemon(t)
		c := newControllerStart(t, d, controllerOptions{})
		t.Setenv("LEGION_OMP_PATH", "")
		c.refused("omp_invocation is not set: set it to 'mise x <tool> -- omp', or set LEGION_OMP_PATH to an absolute executable path")
		c.wantNoSecretRequest()
		c.wantNothingLaunchedOrWritten(c.defaultDir)
	})
	// The mint revokes the incumbent controller, so an Oh My Pi whose plugin would refuse the
	// controller at session start is found before it: a second start from a profile nobody
	// updated leaves the working controller alone.
	t.Run("a plugin in the operator's Oh My Pi profile that speaks another contract, naming both", func(t *testing.T) {
		d := newControllerDaemon(t)
		c := newControllerStart(t, d, controllerOptions{})
		c.installPlugin(api.DaemonAPIVersion() - 1)
		manifest := filepath.Join(c.home, ".omp", "plugins", "node_modules", "@sjawhar", "pi-legion", "package.json")
		c.refused(fmt.Sprintf("pi-legion at %s (package 9.9.9) speaks daemon API contract %d; this daemon requires %d.",
			manifest, api.DaemonAPIVersion()-1, api.DaemonAPIVersion()))
		c.wantNoMintRequest()
		c.wantNothingLaunchedOrWritten(c.defaultDir)
	})
	// Each refusal of the load probe says what Oh My Pi did, launched as the controller launches
	// it, and the remedy under the controller's environment; none names a profile, which only Oh
	// My Pi resolves.
	for _, testCase := range []struct {
		name  string
		loads func(c *operatorMachine)
		want  []string
	}{
		{"an Oh My Pi that does not load the plugin", (*operatorMachine).loadsNothing,
			[]string{"did not load pi-legion (not installed, disabled, or unregistered). Install the @sjawhar/pi-legion release built from this daemon's commit into the Oh My Pi the controller runs", "plugin list` under the controller's environment"}},
		{"an Oh My Pi that loads the plugin but no pi-envoy", func(c *operatorMachine) { c.loadsEnvoyAt("none") },
			[]string{"loaded pi-legion but no pi-envoy. Install the @sjawhar/pi-envoy release built from this daemon's commit into the Oh My Pi the controller runs, and check it with `cd ", "plugin list`"}},
		{"an Oh My Pi whose pi-envoy publishes another interface", func(c *operatorMachine) { c.loadsEnvoyAt("2") },
			[]string{"loaded pi-envoy from file://" + "@HOME/.omp/plugins/node_modules/@sjawhar/pi-envoy/dist/envoy.js?mtime=1, which publishes plugin interface 2, and a pi-legion that speaks 1. Install both releases built from this daemon's commit into the Oh My Pi the controller runs, and check them with `cd ", "plugin list`"}},
		{"an Oh My Pi that still loads the pre-split package", func(c *operatorMachine) { c.loadsLegacyFrom("file:///plugins/legacy/dist/legion.js") },
			[]string{"loaded @sjawhar/pi-legion-envoy from file:///plugins/legacy/dist/legion.js beside pi-legion. Run `cd ", "plugin uninstall @sjawhar/pi-legion-envoy` under the controller's environment"}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			d := newControllerDaemon(t)
			c := newControllerStart(t, d, controllerOptions{})
			testCase.loads(c)
			code, _, errb := c.run()
			for _, want := range testCase.want {
				want = strings.ReplaceAll(want, "@HOME", c.home)
				if code != 1 || !strings.Contains(errb, want) {
					t.Fatalf("legion controller start = %d, stderr %q; want 1 and %q", code, errb, want)
				}
			}
			if strings.Contains(errb, "OMP profile") {
				t.Errorf("legion controller start's refusal names a profile, which only Oh My Pi resolves: %q", errb)
			}
			c.wantNoMintRequest()
			c.wantNothingLaunchedOrWritten(c.defaultDir)
		})
	}
	t.Run("an unknown key, naming it and the example", func(t *testing.T) {
		d := newControllerDaemon(t)
		c := newControllerStart(t, d, controllerOptions{lines: []string{
			"project: demo", "daemon_url: " + d.url, "operator_token_file: ./operator-token",
			"envoy_url: http://envoy.test:9020", "nats_urls: [nats://a:4222]", "runtime: kubernetes",
		}})
		c.refused(`unknown key "runtime" in the controller configuration`)
		c.refused("deploy/kubernetes/daemon/controller.yaml.example")
		c.wantNoSecretRequest()
	})
}

// The plugin held to the contract is the one Oh My Pi reports loading, launched as the controller
// launches it: no resolution of its own decides which copy that is. Each row installs a copy of
// this binary's contract and one of another at two roots, and has Oh My Pi load one of them.
func TestControllerStartHoldsTheCopyOhMyPiLoadsToTheContract(t *testing.T) {
	t.Run("the loaded copy speaks the contract; the profile's own does not", func(t *testing.T) {
		d := newControllerDaemon(t)
		c := newControllerStart(t, d, controllerOptions{})
		c.installPluginAt(filepath.Join(c.home, ".omp"), api.DaemonAPIVersion()-1)
		c.loads(c.installPluginAt(filepath.Join(c.home, "project", ".omp"), api.DaemonAPIVersion()))
		if code, _, errb := c.run(); code != 0 || !c.launched() {
			t.Fatalf("legion controller start = %d (launched %t), stderr %q; want the controller started", code, c.launched(), errb)
		}
	})
	t.Run("the loaded copy speaks another contract; the profile's own speaks this one", func(t *testing.T) {
		d := newControllerDaemon(t)
		c := newControllerStart(t, d, controllerOptions{})
		loaded := c.installPluginAt(filepath.Join(c.home, "project", ".omp"), api.DaemonAPIVersion()-1)
		c.loads(loaded)
		c.refused(fmt.Sprintf("pi-legion at %s (package 9.9.9) speaks daemon API contract %d; this daemon requires %d.",
			filepath.Join(loaded, "package.json"), api.DaemonAPIVersion()-1, api.DaemonAPIVersion()))
		c.wantNoMintRequest()
		c.wantNothingLaunchedOrWritten(c.defaultDir)
	})
}

// The load probe's Oh My Pi reads the operator's stdin, as the controller does: a launch prefix
// such as `secrets` scopes a human-tier grant by the terminal it is run from, and refuses on a
// stdin that is not one.
func TestControllerStartProbesOnTheOperatorsStdin(t *testing.T) {
	d := newControllerDaemon(t)
	c := newControllerStart(t, d, controllerOptions{})
	terminal, err := os.Create(filepath.Join(t.TempDir(), "operator-terminal"))
	if err != nil {
		t.Fatal(err)
	}
	defer terminal.Close()
	stdin := os.Stdin
	os.Stdin = terminal
	defer func() { os.Stdin = stdin }()

	if code, _, errb := c.run(); code != 0 {
		t.Fatalf("legion controller start = %d, stderr %q", code, errb)
	}
	probe, err := os.ReadFile(filepath.Join(c.record, "probe-stdin"))
	if err != nil {
		t.Fatal(err)
	}
	launch, err := os.ReadFile(filepath.Join(c.record, "stdin"))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(probe)); got != terminal.Name() || strings.TrimSpace(string(launch)) != terminal.Name() {
		t.Errorf("the load probe read %q and the controller %q, want both on the operator's %q", got, strings.TrimSpace(string(launch)), terminal.Name())
	}
}

// The load probe's Oh My Pi runs in the controller's own directory, which exists for it, under the
// environment the controller is then launched with.
func TestControllerStartProbesUnderTheControllersEnvironment(t *testing.T) {
	d := newControllerDaemon(t)
	c := newControllerStart(t, d, controllerOptions{})
	if code, _, errb := c.run(); code != 0 {
		t.Fatalf("legion controller start = %d, stderr %q", code, errb)
	}
	cwd, err := os.ReadFile(filepath.Join(c.record, "probe-cwd"))
	if err != nil {
		t.Fatal(err)
	}
	if want, _ := filepath.EvalSymlinks(filepath.Join(c.defaultDir, "controller")); strings.TrimSpace(string(cwd)) != want {
		t.Errorf("the load probe ran in %q, want the controller's directory %q", strings.TrimSpace(string(cwd)), want)
	}
	probe, launch := c.probeEnv(), c.env()
	for _, shell := range []string{"PWD", "OLDPWD", "SHLVL", "_"} {
		delete(probe, shell)
		delete(launch, shell)
	}
	if !maps.Equal(probe, launch) {
		t.Errorf("the load probe ran under %v, but the controller was launched under %v", probe, launch)
	}
	if probe["LEGION_CONTROLLER"] != "1" || probe["LEGION_CONTROLLER_SECRET_FILE"] == "" {
		t.Errorf("the load probe's environment lacks the controller's set: LEGION_CONTROLLER=%q LEGION_CONTROLLER_SECRET_FILE=%q",
			probe["LEGION_CONTROLLER"], probe["LEGION_CONTROLLER_SECRET_FILE"])
	}
}

// The controller is pane-side: the daemon's own NATS seed, by value or by pointer, exported in the
// operator's shell, reaches neither its load probe nor its Oh My Pi, while its pane seed pointer does.
func TestControllerStartDropsTheDaemonSeedFromTheControllersEnvironment(t *testing.T) {
	d := newControllerDaemon(t)
	c := newControllerStart(t, d, controllerOptions{})
	const seed = "SUAMDAEMONSEEDVALUEFORTHECONTROLLERTEST"
	t.Setenv("NATS_DAEMON_NKEY_SEED", seed)
	t.Setenv("NATS_DAEMON_NKEY_SEED_FILE", filepath.Join(c.dir, "daemon-seed"))
	if code, _, errb := c.run(); code != 0 {
		t.Fatalf("legion controller start = %d, stderr %q", code, errb)
	}
	for kind, env := range map[string]map[string]string{"load probe": c.probeEnv(), "controller": c.env()} {
		for name, value := range env {
			if strings.HasPrefix(name, "NATS_DAEMON_NKEY_SEED") || strings.Contains(value, seed) {
				t.Errorf("the %s's environment carries the daemon seed: %s", kind, name)
			}
		}
		if env["NATS_NKEY_SEED_FILE"] != filepath.Join(c.dir, "nats-seed") {
			t.Errorf("the %s's NATS_NKEY_SEED_FILE = %q, want the pane seed's file", kind, env["NATS_NKEY_SEED_FILE"])
		}
	}
}

// A start from inside a Legion pane inherits that pane's boot token, by value or by pointer. The
// plugin takes a controller session carrying one for a controller the daemon launched (`controller:
// daemon`), which registers with it, so neither reaches the operator's controller or its load probe.
func TestControllerStartDropsAnInheritedBootToken(t *testing.T) {
	d := newControllerDaemon(t)
	c := newControllerStart(t, d, controllerOptions{})
	t.Setenv("LEGION_BOOT_TOKEN", "a-pane-boot-token")
	t.Setenv("LEGION_BOOT_TOKEN_FILE", filepath.Join(c.dir, "boot-token"))
	if code, _, errb := c.run(); code != 0 {
		t.Fatalf("legion controller start = %d, stderr %q", code, errb)
	}
	for kind, env := range map[string]map[string]string{"load probe": c.probeEnv(), "controller": c.env()} {
		for _, name := range []string{"LEGION_BOOT_TOKEN", "LEGION_BOOT_TOKEN_FILE"} {
			if value, set := env[name]; set {
				t.Errorf("the %s's environment carries %s=%q", kind, name, value)
			}
		}
	}
}

// A start from inside the daemon-launched controller's pod, or from any shell that exported it,
// inherits LEGION_SLACK_CHANNELS: another project's channels, or this project's, whose mentions only
// the daemon's controller hears. Neither the operator's controller nor its load probe is handed it.
func TestControllerStartDropsInheritedSlackChannels(t *testing.T) {
	d := newControllerDaemon(t)
	c := newControllerStart(t, d, controllerOptions{})
	t.Setenv("LEGION_SLACK_CHANNELS", "C00000001")
	if code, _, errb := c.run(); code != 0 {
		t.Fatalf("legion controller start = %d, stderr %q", code, errb)
	}
	for kind, env := range map[string]map[string]string{"load probe": c.probeEnv(), "controller": c.env()} {
		if value, set := env["LEGION_SLACK_CHANNELS"]; set {
			t.Errorf("the %s's environment carries LEGION_SLACK_CHANNELS=%q", kind, value)
		}
	}
}

// A start from inside a resumed Legion pane inherits OMP_SESSION_EXCLUSIVE=1, the pair the runtime
// tells a pane that resumes a recorded session. This controller resumes none, so neither it nor
// its load probe is handed the variable, which would stop it where Oh My Pi moves a held session.
func TestControllerStartDropsAnInheritedSessionExclusive(t *testing.T) {
	d := newControllerDaemon(t)
	c := newControllerStart(t, d, controllerOptions{})
	t.Setenv(runtime.SessionExclusiveVariable, runtime.SessionExclusiveValue)
	if code, _, errb := c.run(); code != 0 {
		t.Fatalf("legion controller start = %d, stderr %q", code, errb)
	}
	for kind, env := range map[string]map[string]string{"load probe": c.probeEnv(), "controller": c.env()} {
		if value, set := env[runtime.SessionExclusiveVariable]; set {
			t.Errorf("the %s's environment carries %s=%q", kind, runtime.SessionExclusiveVariable, value)
		}
	}
}

// The TypeScript client trades the controller's own Dispatch credential under
// `dispatch_token_broker` and its Envoy credential wherever no bearer names one, and on a machine
// where the operator ran `agent-secrets login` it would trade that person's sign-in, found under
// the operator's HOME, which the controller keeps. So under either Dispatch credential the load
// probe and the controller are told PERSON_SIGNIN_TRADE=off (controllerPersonTradeVar), and
// nothing else about how agent-secrets signs: the sign-in directory, the key directory and the
// helper socket of the operator's shell reach both exactly as that shell has them, so the
// controller's own agent-secrets reads are signed as they are without the setting.
func TestControllerStartTurnsThePersonTradeOffAndLeavesAgentSecretsAsTheOperatorsShellHasIt(t *testing.T) {
	for _, tc := range []struct {
		name   string
		broker bool
	}{
		{"dispatch_token_file", false},
		{"dispatch_token_broker", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := newControllerDaemon(t)
			c := newControllerStart(t, d, controllerOptions{dispatchBroker: tc.broker})
			if tc.broker {
				c.provesAnIdentity(newStubBroker(t, ""))
			}
			operator := map[string]string{
				"AGENT_SECRETS_SIGNIN_DIR":  filepath.Join(c.home, ".local", "state", "agent-secrets", "operator-signins"),
				"AGENT_SECRETS_KEY_DIR":     filepath.Join(c.home, ".local", "state", "agent-secrets", "operator-keys"),
				"AGENT_SECRETS_HELPER_SOCK": filepath.Join(c.dir, "operator-helper.sock"),
			}
			for name, value := range operator {
				t.Setenv(name, value)
			}
			if code, _, errb := c.run(); code != 0 {
				t.Fatalf("legion controller start = %d, stderr %q", code, errb)
			}
			for kind, env := range map[string]map[string]string{"load probe": c.probeEnv(), "controller": c.env()} {
				if got := env["PERSON_SIGNIN_TRADE"]; got != "off" {
					t.Errorf("the %s's PERSON_SIGNIN_TRADE = %q, want off: the controller's own credentials would trade the operator's sign-in", kind, got)
				}
				for name, want := range operator {
					if got, set := env[name]; !set || got != want {
						t.Errorf("the %s's %s = %q (set %v), want the operator's %q: the controller's agent-secrets reads are not this setting's", kind, name, got, set, want)
					}
				}
			}
		})
	}
}

// `project: sjawhar/Legion`, copied from the daemon's legion.yaml, names the daemon's own token in
// the state directory, the secret file and LEGION_PROJECT.
func TestControllerStartSanitizesTheProjectAsTheDaemonDoes(t *testing.T) {
	d := newControllerDaemon(t)
	c := newControllerStart(t, d, controllerOptions{lines: []string{
		"project: sjawhar/Legion", "daemon_url: " + d.url, "operator_token_file: ./operator-token",
		"envoy_url: http://envoy.test:9020", "nats_urls: [nats://a:4222]",
	}})
	if code, _, errb := c.run(); code != 0 {
		t.Fatalf("legion controller start = %d, stderr %q", code, errb)
	}
	stateDir := filepath.Join(c.home, ".local", "state", "legion", "sjawharlegion-controller")
	env := c.env()
	for name, want := range map[string]string{
		"LEGION_PROJECT":                "sjawharlegion",
		"LEGION_STATE_DIR":              stateDir,
		"LEGION_CONTROLLER_SECRET_FILE": filepath.Join(stateDir, "secrets", "legion-sjawharlegion-controller"),
	} {
		if env[name] != want {
			t.Errorf("%s = %q, want %q", name, env[name], want)
		}
	}
}

// state_dir in the file replaces the default, resolved against the file's directory; so does an
// absolute XDG_STATE_HOME the default.
func TestControllerStartStateDirectory(t *testing.T) {
	t.Run("from the file", func(t *testing.T) {
		d := newControllerDaemon(t)
		c := newControllerStart(t, d, controllerOptions{lines: []string{
			"project: demo", "daemon_url: " + d.url, "operator_token_file: ./operator-token",
			"envoy_url: http://envoy.test:9020", "nats_urls: [nats://a:4222]", "state_dir: ./state",
		}})
		if code, _, errb := c.run(); code != 0 {
			t.Fatalf("legion controller start = %d, stderr %q", code, errb)
		}
		if got := c.env()["LEGION_STATE_DIR"]; got != filepath.Join(c.dir, "state") {
			t.Fatalf("LEGION_STATE_DIR = %q, want %s", got, filepath.Join(c.dir, "state"))
		}
		if _, err := os.Stat(filepath.Join(c.dir, "state", "secrets", "legion-demo-controller")); err != nil {
			t.Fatalf("the secret file is not under state_dir: %v", err)
		}
	})
	t.Run("under XDG_STATE_HOME", func(t *testing.T) {
		d := newControllerDaemon(t)
		c := newControllerStart(t, d, controllerOptions{})
		stateHome := t.TempDir()
		t.Setenv("XDG_STATE_HOME", stateHome)
		if code, _, errb := c.run(); code != 0 {
			t.Fatalf("legion controller start = %d, stderr %q", code, errb)
		}
		if got, want := c.env()["LEGION_STATE_DIR"], filepath.Join(stateHome, "legion", "demo-controller"); got != want {
			t.Fatalf("LEGION_STATE_DIR = %q, want %s", got, want)
		}
	})
}

func TestControllerStartUsage(t *testing.T) {
	for _, tc := range []struct {
		argv []string
		want string
	}{
		{[]string{"legion", "controller"}, "legion controller [command]"},
		{[]string{"legion", "controller", "stop"}, `legion controller: unknown command "stop" for "legion controller"`},
		{[]string{"legion", "controller", "start"}, "legion controller start: --config is required"},
	} {
		var out, errb bytes.Buffer
		if code := run(context.Background(), tc.argv, &out, &errb); code != 2 || !strings.Contains(errb.String(), tc.want) {
			t.Errorf("%v = %d, stderr %q; want exit 2 and %q", tc.argv, code, errb.String(), tc.want)
		}
	}
}

// controllerCredentialTokens is a Tokens fake for the controller's GitHub credential route: each
// owner's token is read and set under a lock, so a test can turn one owner's between the refresh
// loop's ticks, or have every mint fail, for the 502 case. An owner with no token of its own gets
// the fake's base token. installed is the owner accounts the review App is installed on, which
// no list of repositories decides, and refused the ones among them whose leases GitHub refuses.
type controllerCredentialTokens struct {
	mu        sync.Mutex
	token     string
	owners    map[string]string
	installed []string
	refused   []string
	err       error
}

// newControllerCredentialTokens is the fake minting token for every owner, with the review App
// installed on acme and sjawhar, the two accounts newControllerDaemonWithTokens' project serves
// repositories under.
func newControllerCredentialTokens(token string) *controllerCredentialTokens {
	return &controllerCredentialTokens{token: token, owners: map[string]string{}, installed: []string{"acme", "sjawhar"}}
}

// installOn replaces the owner accounts the review App is installed on.
func (c *controllerCredentialTokens) installOn(owners ...string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.installed = owners
}

// refuse has GitHub refuse every lease of owner as it refuses a narrowing
// (appauth.ErrNarrowingRefused): the App is installed on the account, and its installation does
// not hold the permissions a lease asks for, so a bundle leaves that owner's directory out.
func (c *controllerCredentialTokens) refuse(owner string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.refused = append(c.refused, owner)
}

// setOwner turns owner's token to token.
func (c *controllerCredentialTokens) setOwner(owner, token string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.owners[owner] = token
}

func (c *controllerCredentialTokens) failWith(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.err = err
}

func (c *controllerCredentialTokens) Token(_ context.Context, _ appauth.AppRole, owner string) (appauth.Lease, error) {
	return c.lease(owner)
}

func (c *controllerCredentialTokens) ReadToken(_ context.Context, _ appauth.AppRole, owner string) (appauth.Lease, error) {
	lease, err := c.lease(owner)
	lease.Token += "-read"
	return lease, err
}

// InstalledOwners is the owner accounts the review App is installed on (installOn), which the
// controller's bundle reads at every build (appauth.OwnerInstallations) and holds a directory for
// each of.
func (c *controllerCredentialTokens) InstalledOwners(context.Context, appauth.AppRole) ([]string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.installed), nil
}

func (c *controllerCredentialTokens) lease(owner string) (appauth.Lease, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return appauth.Lease{}, c.err
	}
	if slices.Contains(c.refused, owner) {
		return appauth.Lease{}, fmt.Errorf("%w (owner %s)", appauth.ErrNarrowingRefused, owner)
	}
	token := c.token
	if own, ok := c.owners[owner]; ok {
		token = own
	}
	return appauth.Lease{Token: token, ExpiresAt: time.Now().Add(time.Hour)}, nil
}

// requestsTo answers how many of d's recorded requests were to path.
func requestsTo(d *controllerDaemon, path string) int {
	n := 0
	for _, r := range d.requests() {
		if r.path == path {
			n++
		}
	}
	return n
}

// credentialRequests are the bodies of d's recorded github-credential requests, in order.
func credentialRequests(t *testing.T, d *controllerDaemon) []api.ControllerCredentialRequest {
	t.Helper()
	var bodies []api.ControllerCredentialRequest
	for _, r := range d.requests() {
		if r.path == "/legion/v1/controller/github-credential" {
			var body api.ControllerCredentialRequest
			if err := json.Unmarshal(r.body, &body); err != nil {
				t.Fatalf("github-credential request body %s: %v", r.body, err)
			}
			bodies = append(bodies, body)
		}
	}
	return bodies
}

// The controller's gh directory holds the review App's read-only token for every owner of the
// project, as the daemon's credential route rendered them, one directory per owner, written before
// Oh My Pi starts, whose GH_CONFIG_DIR names the first owner's directory and LEGION_GH_OWNERS_DIR
// the bundle's root, with one request made before the loop, naming no default yet.
func TestControllerStartWritesTheControllersGitHubCredentialUnderGh(t *testing.T) {
	tokens := newControllerCredentialTokens("ghs_first")
	d := newControllerDaemonWithTokens(t, tokens)
	c := newControllerStart(t, d, controllerOptions{})
	code, _, errb := c.run()
	if code != 0 {
		t.Fatalf("legion controller start = %d, stderr %q", code, errb)
	}
	ghDir := filepath.Join(c.defaultDir, "gh")
	for _, owner := range []string{"acme", "sjawhar"} {
		hosts, err := os.ReadFile(filepath.Join(ghDir, owner, "hosts.yml"))
		if err != nil || string(hosts) != ghconfig.Hosts("ghs_first-read") {
			t.Fatalf("%s's hosts.yml = %q, %v, want %q", owner, hosts, err, ghconfig.Hosts("ghs_first-read"))
		}
		cfg, err := os.ReadFile(filepath.Join(ghDir, owner, "config.yml"))
		if err != nil || string(cfg) != ghconfig.Config {
			t.Fatalf("%s's config.yml = %q, %v, want %q", owner, cfg, err, ghconfig.Config)
		}
	}
	if entries, err := os.ReadDir(ghDir); err != nil || len(entries) != 3 ||
		entries[0].Name() != "acme" || entries[1].Name() != ghconfig.PairsFile || entries[2].Name() != "sjawhar" {
		t.Errorf("the gh directory holds %v (%v), want the two owners' directories and the bundle's pairs file alone", entries, err)
	}
	env := c.env()
	if env["GH_CONFIG_DIR"] != filepath.Join(ghDir, "acme") || env["LEGION_GH_OWNERS_DIR"] != ghDir || env["LEGION_GH_PATH"] != c.gh {
		t.Errorf("GH_CONFIG_DIR = %q, LEGION_GH_OWNERS_DIR = %q, LEGION_GH_PATH = %q; want %s, %s and %s",
			env["GH_CONFIG_DIR"], env["LEGION_GH_OWNERS_DIR"], env["LEGION_GH_PATH"], filepath.Join(ghDir, "acme"), ghDir, c.gh)
	}
	startHosts, err := os.ReadFile(filepath.Join(c.record, "hosts-at-start"))
	if err != nil || string(startHosts) != ghconfig.Hosts("ghs_first-read") {
		t.Fatalf("hosts.yml at Oh My Pi's start = %q, %v; want it to already hold the token", startHosts, err)
	}
	secret, err := os.ReadFile(filepath.Join(c.defaultDir, "secrets", "legion-demo-controller"))
	if err != nil {
		t.Fatalf("read the secret file: %v", err)
	}
	if requests := credentialRequests(t, d); len(requests) != 1 || requests[0] != (api.ControllerCredentialRequest{Secret: string(secret)}) {
		t.Fatalf("github-credential requests = %+v, want one carrying the secret and no default", requests)
	}
	if !strings.Contains(errb, "[legion] the controller's gh acts as the review App for owners acme, sjawhar (read-only) from "+ghDir) {
		t.Errorf("stderr = %q; want it to name the review App, both owners and the gh directory", errb)
	}
	if _, err := os.Stat(filepath.Join(c.defaultDir, "github-credential.log")); err != nil {
		t.Errorf("github-credential.log does not exist: %v", err)
	}
}

// The refresh loop keeps the controller's gh bundle fresh while Oh My Pi runs: a token the daemon
// mints for one owner after Oh My Pi starts reaches that owner's hosts.yml before Oh My Pi exits,
// the other owner's is not rewritten, every refresh asks for the default the first answer named,
// and the log records the refresh, naming the owner. The recording Oh My Pi runs until the test
// releases it, once it has seen the second token land, so the proof does not depend on the
// machine's speed.
func TestControllerStartRefreshesTheGitHubCredentialWhileOhMyPiRuns(t *testing.T) {
	old := controllerGitHubRefreshInterval
	controllerGitHubRefreshInterval = 50 * time.Millisecond
	t.Cleanup(func() { controllerGitHubRefreshInterval = old })

	tokens := newControllerCredentialTokens("ghs_first")
	d := newControllerDaemonWithTokens(t, tokens)
	release := filepath.Join(t.TempDir(), "release")
	c := newControllerStart(t, d, controllerOptions{waitForFile: release, exitCode: 7})

	type result struct {
		code int
		errb string
	}
	done := make(chan result, 1)
	go func() {
		code, _, errb := c.run()
		done <- result{code, errb}
	}()
	ghDir := filepath.Join(c.defaultDir, "gh")
	acme, sjawhar := filepath.Join(ghDir, "acme", "hosts.yml"), filepath.Join(ghDir, "sjawhar", "hosts.yml")
	// The first write holds the first token; only then does sjawhar's lease turn, so the loop,
	// not the first fetch, is what carries the second token into the file.
	testwait.WithinPolling(t, testwait.Bounded(t, 1<<60, 30*time.Second), testwait.Poll, sjawhar+" to hold ghs_first", func() bool {
		body, err := os.ReadFile(sjawhar)
		return err == nil && strings.Contains(string(body), "ghs_first")
	})
	past := time.Now().Add(-time.Hour)
	if err := os.Chtimes(acme, past, past); err != nil {
		t.Fatal(err)
	}
	tokens.setOwner("sjawhar", "ghs_second")
	testwait.WithinPolling(t, testwait.Bounded(t, 1<<60, 30*time.Second), testwait.Poll, sjawhar+" to hold ghs_second", func() bool {
		body, err := os.ReadFile(sjawhar)
		return err == nil && strings.Contains(string(body), "ghs_second")
	})
	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	r := testwait.Receive(t, "legion controller start to return once Oh My Pi exited", done)
	if r.code != 7 {
		t.Fatalf("legion controller start = %d, want 7 (Oh My Pi's exit code); stderr %q", r.code, r.errb)
	}
	if hosts, err := os.ReadFile(sjawhar); err != nil || string(hosts) != ghconfig.Hosts("ghs_second-read") {
		t.Fatalf("sjawhar's hosts.yml = %q, %v; want the second token", hosts, err)
	}
	if info, err := os.Stat(acme); err != nil || !info.ModTime().Equal(past) {
		t.Errorf("acme's hosts.yml was rewritten (%v, %v), want it left as it was", info, err)
	}
	requests := credentialRequests(t, d)
	if len(requests) < 2 || requests[0].Default != "" {
		t.Fatalf("github-credential requests = %+v, want the first naming no default and a refresh after it", requests)
	}
	for _, req := range requests[1:] {
		if req.Default != "acme" {
			t.Errorf("a refresh asked for default %q, want acme, the one the first answer named", req.Default)
		}
	}
	logBody, err := os.ReadFile(filepath.Join(c.defaultDir, "github-credential.log"))
	if err != nil || !strings.Contains(string(logBody), "github credential refreshed") || !strings.Contains(string(logBody), "owners=[sjawhar]") {
		t.Fatalf("github-credential.log = %q, %v; want a refreshed line naming sjawhar", logBody, err)
	}
}

// A superseded capability (a later legion controller start) ends the refresh loop: the gh
// directory keeps the first tokens, the log records the stop, and no further request reaches the
// daemon. The recording Oh My Pi runs until the test releases it, once it has seen the stop logged
// and the requests settle.
func TestControllerStartStopsTheRefreshLoopWhenTheCapabilityIsSuperseded(t *testing.T) {
	old := controllerGitHubRefreshInterval
	controllerGitHubRefreshInterval = 50 * time.Millisecond
	t.Cleanup(func() { controllerGitHubRefreshInterval = old })

	tokens := newControllerCredentialTokens("ghs_first")
	d := newControllerDaemonWithTokens(t, tokens)
	release := filepath.Join(t.TempDir(), "release")
	c := newControllerStart(t, d, controllerOptions{waitForFile: release})

	done := make(chan struct{})
	go func() {
		c.run()
		close(done)
	}()
	hostsFile := filepath.Join(c.defaultDir, "gh", "acme", "hosts.yml")
	testwait.WithinPolling(t, testwait.Bounded(t, 1<<60, 30*time.Second), testwait.Poll, hostsFile+" to exist", func() bool {
		_, err := os.Stat(hostsFile)
		return err == nil
	})
	// Mint a second capability, which supersedes the one this controller holds.
	answer, err := (operator{base: d.url, bearer: controllerOperatorToken}).do(context.Background(), http.MethodPost,
		"/legion/v1/controller/secret", api.ControllerSecretRequest{PluginContract: api.DaemonAPIVersion()})
	if err != nil || answer.Status() != http.StatusOK {
		t.Fatalf("mint a second capability: status %d, err %v", answer.Status(), err)
	}
	logFile := filepath.Join(c.defaultDir, "github-credential.log")
	testwait.WithinPolling(t, testwait.Bounded(t, 1<<60, 30*time.Second), testwait.Poll, logFile+" to hold \"github credential refresh stopped\"", func() bool {
		body, err := os.ReadFile(logFile)
		return err == nil && strings.Contains(string(body), "github credential refresh stopped")
	})
	before := requestsTo(d, "/legion/v1/controller/github-credential")
	time.Sleep(10 * controllerGitHubRefreshInterval)
	if after := requestsTo(d, "/legion/v1/controller/github-credential"); after != before {
		t.Fatalf("%d further github-credential requests reached the daemon after the stop, want 0 more", after-before)
	}
	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	testwait.Receive(t, "legion controller start to return once Oh My Pi exited", done)
	hosts, err := os.ReadFile(hostsFile)
	if err != nil || string(hosts) != ghconfig.Hosts("ghs_first-read") {
		t.Fatalf("hosts.yml = %q, %v; want it to keep the first token", hosts, err)
	}
}

// A daemon with no GitHub App for the project writes an empty gh directory and runs no loop,
// stating so, and Oh My Pi still starts, its GH_CONFIG_DIR the empty directory.
func TestControllerStartWithNoGitHubAppWritesAnEmptyGhDirectory(t *testing.T) {
	d := newControllerDaemon(t)
	c := newControllerStart(t, d, controllerOptions{})
	code, _, errb := c.run()
	if code != 0 {
		t.Fatalf("legion controller start = %d, stderr %q", code, errb)
	}
	ghDir := filepath.Join(c.defaultDir, "gh")
	if got := modeOf(t, ghDir); got != 0o700 {
		t.Errorf("gh directory mode = %o, want 0700", got)
	}
	if entries, err := os.ReadDir(ghDir); err != nil || len(entries) != 0 {
		t.Errorf("gh directory holds %v (%v); want nothing, since the daemon has no GitHub App", entries, err)
	}
	if got := c.env()["GH_CONFIG_DIR"]; got != ghDir {
		t.Errorf("GH_CONFIG_DIR = %q, want the empty %s", got, ghDir)
	}
	if !strings.Contains(errb, "[legion] the daemon has no GitHub App to act as; the controller's gh acts as nobody\n") {
		t.Errorf("stderr = %q; want the no-App line", errb)
	}
	if _, err := os.Stat(filepath.Join(c.defaultDir, "github-credential.log")); !os.IsNotExist(err) {
		t.Errorf("github-credential.log exists (%v); want none, since no loop ran", err)
	}
	if n := requestsTo(d, "/legion/v1/controller/github-credential"); n != 1 {
		t.Fatalf("%d github-credential requests, want 1", n)
	}
}

// The state directory outlives a run, so a gh directory an earlier start wrote against a daemon
// that had a GitHub App can still hold that run's files: a bundle of owner directories, or the one
// hosts.yml a start wrote before bundles. A start against a daemon with no GitHub App says the
// controller's gh acts as nobody, so it must leave no token for gh to act with: every stale file
// goes (LEGION-668, found by the tester).
func TestControllerStartWithNoGitHubAppRemovesAStaleHostsFile(t *testing.T) {
	d := newControllerDaemon(t)
	c := newControllerStart(t, d, controllerOptions{})
	ghDir := filepath.Join(c.defaultDir, "gh")
	stale := ghconfig.Render("ghs_stale_from_an_earlier_run", "review", time.Now().Add(time.Hour))
	if _, err := ghconfig.WriteBundle(ghDir, ghconfig.Bundle{Default: "acme", Dirs: map[string]ghconfig.Rendered{"acme": stale}}); err != nil {
		t.Fatalf("seed the stale gh bundle: %v", err)
	}
	if err := os.WriteFile(filepath.Join(ghDir, "hosts.yml"), []byte(stale.Hosts), 0o600); err != nil {
		t.Fatalf("seed the stale pre-bundle hosts.yml: %v", err)
	}
	code, _, errb := c.run()
	if code != 0 {
		t.Fatalf("legion controller start = %d, stderr %q", code, errb)
	}
	if !strings.Contains(errb, "[legion] the daemon has no GitHub App to act as; the controller's gh acts as nobody\n") {
		t.Fatalf("stderr = %q; want the no-App line", errb)
	}
	if entries, err := os.ReadDir(ghDir); err != nil || len(entries) != 0 {
		t.Errorf("gh directory holds %v (%v); want nothing: the command said gh acts as nobody, but gh would act as the stale token", entries, err)
	}
	if got := modeOf(t, ghDir); got != 0o700 {
		t.Errorf("gh directory mode = %o, want 0700", got)
	}
}

// legion controller start prints its own line for each code of a daemon that has nothing for the
// controller's gh to act as, and each line says why: a daemon with no GitHub App
// (GITHUB_TOKEN_SOURCE_UNAVAILABLE) has none, and a daemon whose review App is installed on no
// account (GITHUB_OWNER_UNCONFIGURED) has the App and no GitHub owner, so the first line would be
// false of it (dispatch://INFRA-4401 A3). Each start still empties the gh directory and launches.
// Control: with the first line printed for both codes, the review App's case fails.
func TestControllerStartSaysWhyTheControllersGhActsAsNobodyForEachCode(t *testing.T) {
	noApp := "[legion] the daemon has no GitHub App to act as; the controller's gh acts as nobody\n"
	noOwner := "[legion] the review App is installed on no account, so the controller has no GitHub owner; the controller's gh acts as nobody\n"
	for _, tc := range []struct {
		name      string
		daemon    func(*testing.T) *controllerDaemon
		code      string
		want, not string
	}{
		{"a daemon with no GitHub App", newControllerDaemon, "GITHUB_TOKEN_SOURCE_UNAVAILABLE", noApp, noOwner},
		{"a review App installed on no account", func(t *testing.T) *controllerDaemon {
			tokens := newControllerCredentialTokens("ghs_never_minted")
			tokens.installOn()
			return newControllerDaemonWithTokens(t, tokens)
		}, "GITHUB_OWNER_UNCONFIGURED", noOwner, noApp},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := tc.daemon(t)
			c := newControllerStart(t, d, controllerOptions{})
			code, _, errb := c.run()
			if code != 0 {
				t.Fatalf("legion controller start = %d, stderr %q", code, errb)
			}
			var answers []api.Failure
			for _, r := range d.requests() {
				if r.path == "/legion/v1/controller/github-credential" {
					var failure api.Failure
					if err := json.Unmarshal(r.answer, &failure); err != nil {
						t.Fatalf("github-credential answer %s: %v", r.answer, err)
					}
					answers = append(answers, failure)
				}
			}
			if len(answers) != 1 || answers[0].Code != tc.code {
				t.Fatalf("the daemon answered the github-credential requests %+v, want one answer with the code %s", answers, tc.code)
			}
			if !strings.Contains(errb, tc.want) || strings.Contains(errb, tc.not) {
				t.Errorf("stderr = %q; want the line of %s, %q, and not %q", errb, tc.code, tc.want, tc.not)
			}
			ghDir := filepath.Join(c.defaultDir, "gh")
			if entries, err := os.ReadDir(ghDir); err != nil || len(entries) != 0 {
				t.Errorf("gh directory holds %v (%v); want nothing, since the controller's gh acts as nobody", entries, err)
			}
			if !c.launched() {
				t.Errorf("Oh My Pi did not start; want it started with the controller's gh acting as nobody")
			}
		})
	}
}

// The real gh the controller's `legion gh` runs is the operator's LEGION_GH_PATH when set, and
// otherwise the first gh on the operator's PATH that is not this state directory's own wrapper,
// which must be a compiled program: a script there is a gh wrapper (a machine's own shim), which
// would choose its own token in place of the review App's. A start that finds none or a script,
// and a LEGION_GH_PATH that is relative, absent, or no executable file, is refused naming it
// before the daemon is asked for anything: a mint would replace the running controller's
// capability with one whose gh cannot run.
func TestControllerStartResolvesTheRealGh(t *testing.T) {
	t.Run("on PATH, past the controller's own wrapper", func(t *testing.T) {
		d := newControllerDaemon(t)
		c := newControllerStart(t, d, controllerOptions{})
		bin := t.TempDir()
		real := filepath.Join(bin, "gh")
		if err := os.WriteFile(real, []byte("\x7fELF\x02\x01\x01\x00"), 0o700); err != nil {
			t.Fatal(err)
		}
		wrapper := filepath.Join(c.defaultDir, "bin")
		if err := os.MkdirAll(wrapper, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(wrapper, "gh"), []byte("#!/bin/sh\nexit 97\n"), 0o700); err != nil {
			t.Fatal(err)
		}
		fixtureenv.Unset(t, "LEGION_GH_PATH")
		t.Setenv("PATH", wrapper+":"+bin+":/usr/bin:/bin")
		if code, _, errb := c.run(); code != 0 {
			t.Fatalf("legion controller start = %d, stderr %q", code, errb)
		}
		if got := c.env()["LEGION_GH_PATH"]; got != real {
			t.Errorf("LEGION_GH_PATH = %q, want the gh on PATH past the wrapper, %s", got, real)
		}
	})
	t.Run("a script first on PATH", func(t *testing.T) {
		d := newControllerDaemon(t)
		c := newControllerStart(t, d, controllerOptions{})
		shims, bin := t.TempDir(), t.TempDir()
		shim := filepath.Join(shims, "gh")
		if err := os.WriteFile(shim, []byte("#!/bin/bash\nexec knives gh -- \"$@\"\n"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(bin, "gh"), []byte("\x7fELF\x02\x01\x01\x00"), 0o700); err != nil {
			t.Fatal(err)
		}
		fixtureenv.Unset(t, "LEGION_GH_PATH")
		t.Setenv("PATH", shims+":"+bin+":/usr/bin:/bin")
		code, _, errb := c.run()
		if want := "the first gh on PATH, " + shim + ", is a script"; code != 1 || !strings.Contains(errb, want) || !strings.Contains(errb, "LEGION_GH_PATH") {
			t.Fatalf("legion controller start = %d, stderr %q; want 1 and a refusal containing %q and LEGION_GH_PATH", code, errb, want)
		}
		c.wantNoSecretRequest()
	})
	notExecutable := filepath.Join(t.TempDir(), "gh")
	if err := os.WriteFile(notExecutable, []byte("#!/bin/sh\nexit 0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, value, want string }{
		{"no gh at all", "", "gh is not on PATH"},
		{"a relative LEGION_GH_PATH", "gh", `LEGION_GH_PATH "gh" is not an absolute path`},
		{"an absent LEGION_GH_PATH", "/nonexistent/gh", `LEGION_GH_PATH "/nonexistent/gh"`},
		{"a LEGION_GH_PATH that is no executable", notExecutable, `LEGION_GH_PATH "` + notExecutable + `" is not an executable file`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := newControllerDaemon(t)
			c := newControllerStart(t, d, controllerOptions{})
			if tc.value == "" {
				fixtureenv.Unset(t, "LEGION_GH_PATH")
				t.Setenv("PATH", t.TempDir())
			} else {
				t.Setenv("LEGION_GH_PATH", tc.value)
			}
			code, _, errb := c.run()
			if code != 1 || !strings.Contains(errb, tc.want) {
				t.Fatalf("legion controller start = %d, stderr %q; want 1 and a refusal containing %q", code, errb, tc.want)
			}
			c.wantNoSecretRequest()
		})
	}
}

// A failed GitHub credential fetch after the mint exits 1, naming the route and the daemon's
// sentence; Oh My Pi never ran, and the secret file stays written.
func TestControllerStartExitsWhenTheGitHubCredentialFetchFails(t *testing.T) {
	tokens := newControllerCredentialTokens("ghs_first")
	tokens.failWith(errors.New("github is down"))
	d := newControllerDaemonWithTokens(t, tokens)
	c := newControllerStart(t, d, controllerOptions{})
	code, _, errb := c.run()
	want := d.url + "/legion/v1/controller/github-credential: GitHub token exchange failed\n"
	if code != 1 || !strings.HasSuffix(errb, want) {
		t.Fatalf("legion controller start = %d, stderr %q; want 1 and a refusal ending %q", code, errb, want)
	}
	if c.launched() {
		t.Fatal("Oh My Pi was launched")
	}
	if _, err := os.Stat(filepath.Join(c.defaultDir, "secrets", "legion-demo-controller")); err != nil {
		t.Errorf("the secret file does not exist (%v); want it left as controllerStart's other post-mint failures leave it", err)
	}
}
