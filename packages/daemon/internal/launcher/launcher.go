// Package launcher runs a launcher container's long-lived process supervisor: one per role it
// serves, each of which connects to the daemon independently from its child worker shim, reports
// actual child state after every reconnect, and owns exactly one generation at a time.
package launcher

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/sjawhar/legion/daemon/internal/ghconfig"
	"github.com/sjawhar/legion/daemon/internal/leakguard"
	"github.com/sjawhar/legion/daemon/internal/memoryguard"
	"github.com/sjawhar/legion/daemon/internal/ompsessions"
	"github.com/sjawhar/legion/daemon/internal/redact"
	"github.com/sjawhar/legion/daemon/internal/rolefile"
	"github.com/sjawhar/legion/daemon/internal/sessionproxy"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// WorkspaceRecreatedVariable is how a role launcher tells the generation it starts whether that
// generation resumes a session in a workspace recreated since the session was last written: "true"
// or "false", set on every generation, so no value from the container's environment reaches the
// child. The Legion plugin tells the agent so in a message it saves to the session at its start,
// ahead of the next turn (pi-legion, src/workspace-recreated.ts).
const WorkspaceRecreatedVariable = "LEGION_WORKSPACE_RECREATED"

// Config is the launcher's configuration. The controller's pod names one role: Token, read from
// its role-private projected Secret before Run is called, Role, and PrivateDir, that container's
// own memory-backed directory, which no other container mounts. An issue pod's one container
// names RolesDir and every resident role instead: the launcher reads each role's token from
// <RolesDir>/<role>/launcher/LAUNCHER_TOKEN, writes that role's generations under
// <RolesDir>/<role>/private, and starts each of them in namespaces of its own (role-init), so no
// role reaches another's credentials, processes or files. Either way a generation's credentials
// live in a fresh `g<generation>` directory for exactly that generation's lifetime.
type Config struct {
	Connect, Token, Sandbox, Role, PodUID, PrivateDir string
	RolesDir                                          string
	Roles                                             []string
	// TestPostgres has this launcher serve the pod's own PostgreSQL for tests (testpg.go), which
	// only an issue pod's container does (RolesDir set). The deployment asks for it with
	// `runtime.kubernetes.test_postgres`.
	TestPostgres bool
	// MicroVM says the pod runs in a GKE Sandbox microVM rather than under gVisor (--microvm), in
	// either form: the daemon renders it on a microVM issue pod's container and on the project
	// controller's pod where the project's sandbox is a microVM. It decides the pod's memory watch
	// (runsGuest). It comes from the pod as built, never from the daemon's acknowledgement, whose
	// settings are the configuration of the moment: a pod kept since its issue, or its project,
	// moved to the other sandbox still runs where it was built.
	MicroVM bool
	// Metadata has this launcher serve the pod-local metadata endpoint (metadata.go), which only a
	// microVM pod's container does (MicroVM set), in either form: a microVM issue pod's roles
	// container and a microVM controller pod's, since a microVM's pod does not reach the node's
	// metadata server. The deployment asks for it with
	// `runtime.kubernetes.sandbox.microvm.metadata`.
	Metadata       *MetadataConfig
	Stdout, Stderr io.Writer
	// StopGrace is the container shutdown and natural-exit descendant cleanup budget. Every role
	// of an issue pod's container is given it whole, since they stop together.
	StopGrace time.Duration
	// SessionDatabase is the Cloud SQL database every generation keeps its Oh My Pi session in,
	// which the launcher proxies to (internal/sessionproxy) as the pod itself, or as the service
	// account the database names (ImpersonateServiceAccount), which the pod impersonates with its
	// own credentials, or, with a writer audience (WriterAudience), with the writer token under
	// the roles directory (SessionWriterDirName). With a writer audience the proxy also admits
	// only the launcher and the generation's agent (sessionPeer). The zero value is every other
	// session store: no proxy, and the child's session storage is whatever its environment names.
	SessionDatabase ompsessions.Database
	// SessionDialer builds that proxy's upstream dialer for signer, which names the service
	// account that SessionDatabase names, or no account for the pod itself
	// (sessionproxy.Config.NewDialer). Nil is the Cloud SQL connector's, which is the only one a
	// pod uses; a test passes a fake.
	SessionDialer func(ctx context.Context, signer sessionproxy.Signer) (sessionproxy.Dialer, error)
	// LeakCheckDir is where the launcher installs the shared public leak check the daemon serves
	// it; "" is leakguard.InstallDir, the one directory every role's shim reads it from. A test
	// names a temporary one.
	LeakCheckDir string
}

// reconnectDelay is the pause between daemon connections. A lost connection never stops the
// child: a daemon restart re-adopts the role process the launcher still owns.
const reconnectDelay = time.Second

// Run serves launcher commands until ctx ends. A stopped child is reported, never restarted by
// the launcher: the daemon owns generations and decides whether a new generation should start.
// Only ctx's end (the container's SIGTERM) stops the children the launcher owns, and in an issue
// pod's container it stops every role's at once, each with the whole stop grace. Run returns only
// once each of those generations has ended (stopActive): its child given its stop grace, and its
// session database proxy, which served it through that grace, closed.
func Run(ctx context.Context, cfg Config) error {
	if cfg.StopGrace == 0 {
		cfg.StopGrace = 30 * time.Second
	}
	if cfg.StopGrace < 0 {
		return errors.New("launcher: stop grace must be positive")
	}
	if err := cfg.validate(); err != nil {
		return err
	}
	address, err := tcpAddress(cfg.Connect)
	if err != nil {
		return err
	}
	pids := &pidGuard{}
	managers, err := newManagers(cfg, pids)
	if err != nil {
		return err
	}
	// PID 1 adopts every orphan of its PID namespace and must reap it. The single-role form, the
	// controller's pod without a writer audience, runs its one role's processes in this
	// namespace. In the roles-dir form each role has a namespace of its own, whose init reaps its
	// processes, but anything else started in the container (a `kubectl exec` and what it leaves
	// behind) is this process's. The reaper never waits on a generation's own process, whose exit
	// its manager reads.
	if os.Getpid() == 1 {
		exits := make(chan os.Signal, 1)
		signal.Notify(exits, syscall.SIGCHLD)
		defer signal.Stop(exits)
		go reapOrphans(ctx, exits, managers, pids)
	}
	if cfg.RolesDir == "" {
		// The controller pod's memory watch, before the role's first connection, so the container
		// is watched while the daemon is away: the guard under gVisor (INFRA-3531), the guest
		// watch in a microVM (INFRA-4567). Only a launcher that is the container's first process
		// runs one, as only that one runs the reaper above, and any other logs a line instead
		// (serveMemoryGuard). Run returns once its loop has, as the roles form's below does.
		memory := serveMemoryGuard(ctx, cfg, managers, pids)
		// A microVM controller pod's metadata endpoint, before the role's first connection, so
		// the role's first generation finds it listening and is told its address only once it
		// listens (metadataEnv), as the roles form's below. Its lines are the role's own
		// (manager.logf), since this form has no roles log, and its server is waited on before
		// Run returns, as the memory watch is.
		metadata := serveMetadataFor(ctx, cfg, managers, managers[0].logf)
		err := managers[0].serveUntil(ctx, address)
		if metadata != nil {
			<-metadata
		}
		<-memory
		return err
	}
	// The clone's stale git locks, swept for the life of the pod (gitLockSweep). This is a
	// roles-dir container: an issue pod's, or the project controller's where the session
	// database names a writer audience. The controller's working directory is no jj workspace
	// and its generations carry no workspace at all, so there the sweep finds no clone, writes
	// one line and sweeps nothing (newGitLockSweep, learn). The first pass is this call's, before
	// serveTestPG, which starts a PostgreSQL: the first pass's rule is that no process of the
	// container holds a lock yet. Only a launcher that is the container's first process sweeps,
	// as only that one runs the memory guard below, since only it sees every process that may
	// hold a lock; any other logs a line instead (serveGitLockSweep).
	locks := serveGitLockSweep(ctx, managers)
	// The pod's one memory guard, before the test database and any role start, so it watches
	// both from their first process; only a launcher that is the container's first process runs
	// one, as only that one runs the reaper above, and any other logs a line instead
	// (serveMemoryGuard). Its loop is waited on with the roles below, as the test database's
	// ticker is. In the controller's pod, under a writer audience, it watches the controller's
	// role as it watches each role of an issue pod, and the controller's acknowledgement hands it
	// the controller pod's settings (serve).
	memory := serveMemoryGuard(ctx, cfg, managers, pids)
	// The pod-local metadata endpoint, before any role starts: a role's first Google client finds
	// it listening, and a role is told its address only once it listens (metadataEnv). Its server
	// is waited on with the roles below, as the test database's ticker is.
	metadata := serveMetadataFor(ctx, cfg, managers, managers[0].log.linef)
	// The pod's own test database, before any role starts: a role that starts at once finds the
	// server serving its DSN (serveTestPG). Its ticker is waited on with the roles below, so Run
	// returns with nothing of the launcher's still running.
	var testPG <-chan struct{}
	if cfg.TestPostgres {
		testPG = serveTestPG(ctx, cfg, managers[0].log, pids)
	}
	// The pod's one cache trimmer, bound to the first role of the --role list: that role is the
	// issue's architect, whose connection exists for the life of the pod, and one connection
	// deciding both the settings the trimmer runs by and where its reports go is what keeps a
	// report from meeting a daemon that did not send the settings that produced it
	// (serveVolumeTrim). It is built before any role starts, as the test database is, and its
	// loop is waited on with the roles below, so Run returns with nothing of the launcher's
	// still running. Only a launcher that is the container's first process builds one, as only
	// that one runs the memory guard, or in a microVM pod the guest watch in its place, and the
	// lock sweep above, and any other logs a line instead.
	trim := serveVolumeTrim(ctx, managers[0], pids)
	var running sync.WaitGroup
	for _, m := range managers {
		running.Add(1)
		go func() {
			defer running.Done()
			_ = m.serveUntil(ctx, address)
		}()
	}
	running.Wait()
	if testPG != nil {
		<-testPG
	}
	if metadata != nil {
		<-metadata
	}
	<-memory
	// The trimmer watches the same context as the roles and is stopped beside them rather than
	// after them: worker_stop_timeout_seconds is the pod's termination grace as well as each
	// role's own --stop-grace, and the roles are given it whole.
	<-trim
	<-locks
	return ctx.Err()
}

// serveMetadataFor starts the pod-local metadata endpoint where cfg asks for it (serveMetadata),
// with its lines written through logf, and tells each of managers the address it serves at, which
// every generation they start is told (metadataEnv). It answers the channel serveMetadata does, nil
// where cfg asks for no endpoint.
func serveMetadataFor(ctx context.Context, cfg Config, managers []*manager, logf func(string, ...any)) <-chan struct{} {
	if cfg.Metadata == nil {
		return nil
	}
	metadata, address := serveMetadata(ctx, cfg, logf)
	for _, m := range managers {
		m.metadataAddress = address
	}
	return metadata
}

// serveVolumeTrim runs the pod's cache trimmer until ctx ends or the trimmer switches itself off
// (trimmer.run), and answers a channel closed once its loop has returned, so Run can wait for it
// as it waits for the test database's ticker (serveTestPG). The controller's pod trims nothing:
// that launcher runs one role, its volume belongs to its Sandbox rather than to an issue, and no
// architect would read its report. In its single-role form Run returns before this is reached.
// Where the session database names a writer audience, its roles-dir form reaches here and builds
// a trimmer, but the controller's acknowledgement carries no VolumeTrim (launcherHelloAck in
// internal/runtime/sandbox), so that trimmer waits for settings that never come and reads,
// deletes and reports nothing (trimmer.awaitSettings).
//
// The trimmer runs only where the launcher is PID 1 of its PID namespace (containerInit), which in
// an issue pod is the container's first process, as the memory guard and the git lock sweep do
// (serveMemoryGuard, serveGitLockSweep). A sweep deletes no entry a process of the pod is using,
// and it reads what each process uses from /proc (readUsedPaths): PID 1 of the container sees every
// process of the container, each role's namespaces' included, where a launcher started anywhere
// else (by hand inside a role) can see that role's processes alone and would then delete a cache
// entry a process of another role is using. Any other launcher logs one line, builds no trimmer,
// and answers a channel already closed, so Run waits on nothing. Every acknowledgement still writes
// the settings it carries into its role's manager (applyAck), where nothing then reads them: the
// trimmer is their one reader and the one caller of sendVolumeReport, so no report is ever sent.
//
// The trimmer is constructed here, once per launcher process and never again: its identifier is
// minted at construction (trimmer.id) and kept across every reconnection, where an identifier
// per connection would multiply the daemon's three-notice bound over a rolling daemon restart.
//
// Nothing of it runs until that connection's first acknowledgement has been applied, because
// the settings and the writer become readable in the same moment and under the same lock
// (applyAck): a trimmer that asked before that would wait out its first settings poll for
// nothing. A connection that merely drops afterwards leaves the settings in force (volumeTrim)
// and the trimmer sweeping, holding any report it cannot write until that connection is up
// again (sendVolumeReport); an acknowledgement that carries none, or carries settings the
// trimmer refuses, stops the sweeping and leaves the trimmer waiting for settings it can run
// (trimmer.run).
//
// It is given the cover each sweep's deleter builds its own mount namespace over before it
// removes anything, the one role-init is given (roleCoverFor): this launcher's roles directory,
// under which every role's private, state and agent-secrets directories and launcher token live,
// and the home, the home mounts directory and the fixed root, which hold the home every role
// starts from, the pod's shared config home and the mount points a role's own directories are
// bound at. Every path a role's namespace replaces is an empty filesystem in that namespace, and
// the deleter is the one thing that keeps a link a role left in a cache out of them (RoleCover,
// volumedelete.go). Only a launcher that names a roles directory reaches here: an issue pod's
// container, or the controller's where the session database names a writer audience. guard is
// the launcher's pidGuard, which each sweep's deleter starts through so PID 1's reaper leaves its
// exit to the deleter's own wait (startRemover).
func serveVolumeTrim(ctx context.Context, m *manager, guard *pidGuard) <-chan struct{} {
	done := make(chan struct{})
	if !containerInit() {
		m.log.linef("launcher: volume trim: off reason=the launcher is pid %d, not the container's first process, so it cannot see every process that may be using the volume",
			os.Getpid())
		close(done)
		return done
	}
	trim := newTrimmer(m.volumeTrim, m.volumeTrimming, m.sendVolumeReport, m.logf, roleCoverFor(m.cfg.RolesDir), guard)
	go func() {
		defer close(done)
		select {
		case <-ctx.Done():
			return
		case <-m.acknowledged:
		}
		podTrimmer(ctx, trim)
	}()
	return done
}

// podTrimmer is the trimmer's own loop, a package variable so a test can drive the trimmer the
// launcher built — its tree, its volume reading, its /proc, its clock and its ticker — and watch
// the very loop Run waits on return. Only a launcher taken for the container's first process
// (containerInit) reads it, and only a test that runs alone makes a launcher one
// (useContainerInit).
var podTrimmer = func(ctx context.Context, trim *trimmer) { trim.run(ctx) }

// newManagers is one manager per role the launcher serves, each with its own token, private
// directory, launcher id and, in an issue pod's container, its own prefix in the container's log.
// guard holds the launcher's own children out of PID 1's reaper (pidGuard). Every line the
// launcher writes passes the URL rule (redact.Writer): the controller pod's manager writes its own
// lines through it, and an issue pod's role log writes every line through it, the launcher's and
// each role child's alike, since the role log writes each line whole. The controller pod's shim
// is handed cfg.Stdout and cfg.Stderr themselves (command), so its descriptors stay the
// container's files rather than a pipe through the launcher.
func newManagers(cfg Config, guard *pidGuard) ([]*manager, error) {
	// One install of the shared public leak check per launcher process, for the pin of its own
	// environment: the daemon pushes it to whichever role's connection it starts first, and
	// every other role's connection finds it installed. Its one run of the check starts through
	// the guard: it is this process's child, and the reaper would otherwise take its exit before
	// the install's own wait read it, which fails the install with `the served check could not be
	// run: waitid: no child processes` (or, under gVisor, `wait: no child processes` or `waitid: no
	// such process`), and the role's launch with it.
	check := newCheckInstall(os.Getenv(leakguard.TagVariable), cfg.LeakCheckDir, guard.starter())
	if cfg.RolesDir == "" {
		return []*manager{{
			cfg: cfg, role: cfg.Role, token: cfg.Token, private: cfg.PrivateDir, launcherID: randomID(),
			stderr: redact.Writer(cfg.Stderr), requests: map[string]request{}, check: check, guard: guard,
			acknowledged: make(chan struct{}),
		}}, nil
	}
	log := &roleLog{stdout: redact.Writer(cfg.Stdout), stderr: redact.Writer(cfg.Stderr)}
	managers := make([]*manager, 0, len(cfg.Roles))
	for _, role := range cfg.Roles {
		file := filepath.Join(cfg.RolesDir, role, RoleLauncherDir, LauncherTokenFile)
		token, err := os.ReadFile(file)
		if err != nil {
			return nil, fmt.Errorf("launcher: role %s launcher token: %v", role, err)
		}
		if strings.TrimSpace(string(token)) == "" {
			return nil, fmt.Errorf("launcher: role %s launcher token %s is empty", role, file)
		}
		// The launcher's own lines for the role are written through a writer of their own, and
		// each generation's child through writers of its own (command), so no launcher line is
		// glued onto a line the child has not finished, and no generation's onto another's.
		_, own := log.writers(role)
		managers = append(managers, &manager{
			cfg: cfg, role: role, token: strings.TrimSpace(string(token)), namespaced: true,
			private: filepath.Join(cfg.RolesDir, role, RolePrivateDir), launcherID: randomID(),
			log: log, stderr: own, requests: map[string]request{}, check: check, guard: guard,
			acknowledged: make(chan struct{}),
		})
	}
	return managers, nil
}

// serveUntil serves one role's daemon connections until ctx ends, reconnecting between them, and
// stops that role's child when it does.
func (m *manager) serveUntil(ctx context.Context, address string) error {
	for {
		err := m.serve(ctx, address)
		if ctx.Err() != nil {
			m.stopActive()
			return ctx.Err()
		}
		m.logf("launcher: daemon connection: %v; reconnecting", err)
		select {
		case <-ctx.Done():
			m.stopActive()
			return ctx.Err()
		case <-time.After(reconnectDelay):
		}
	}
}

// logf writes one line of this slot's log, behind its own prefix in an issue pod's container: the
// slot's name (its --role), which is the name of the agent that runs in it, whatever role that
// agent's step is on.
func (m *manager) logf(format string, args ...any) {
	if m.stderr == nil {
		return
	}
	fmt.Fprintf(m.stderr, format+"\n", args...)
}

// serve is one authenticated daemon connection: hello, acknowledgement, the launcher's actual
// state, then start, stop and leak-check commands until the connection ends. The hello
// names the launcher's leak-check pin and whether this process has installed it, so the daemon
// pushes the check only to a launcher that still needs it. It also says that this launcher looks a
// session named by its id alone up in the session table (resumesBySession, which start's lookup
// serves): the daemon names a session that way only to a launcher whose hello said so. The
// acknowledgement is logged as one line, so a launcher the daemon accepted reads apart from one
// still dialing. The memory guard settings it carries, or their absence, go to the pod's guard,
// and what it carries decides whether the kill reports of this role's processes go on this
// connection (openMemoryReports); what it carries for the pod's cache trimmer is put in force with
// the writer (applyAck).
func (m *manager) serve(ctx context.Context, address string) error {
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", address)
	if err != nil {
		return fmt.Errorf("dial %s: %w", address, err)
	}
	defer conn.Close()
	stopClose := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stopClose()
	writer := shimwire.NewWriter(conn)
	hello := shimwire.LauncherHello{
		Token: m.token, Sandbox: m.cfg.Sandbox, Role: m.role, PodUID: m.cfg.PodUID, LauncherID: m.launcherID, CredentialWait: true,
		ResumesBySession: true,
	}
	hello.LeakCheckTag, hello.LeakCheckInstalled = m.check.hello()
	if err := writer.WriteFrame(hello); err != nil {
		return fmt.Errorf("hello: %w", err)
	}
	reader := shimwire.NewReader(conn)
	frame, err := readFrame(reader)
	if err != nil {
		return fmt.Errorf("hello acknowledgement: %w", err)
	}
	ack, ok := frame.(shimwire.LauncherHelloAck)
	if !ok {
		return fmt.Errorf("hello acknowledgement: got %T, want launcher_hello_ack", frame)
	}
	m.logf("launcher: connected to the daemon at %s as launcher %s (sandbox %s, role %s, pod %s)",
		address, m.launcherID, m.cfg.Sandbox, m.role, m.cfg.PodUID)
	// The guard runs by the newest acknowledgement of any role, and an acknowledgement that
	// carries no settings turns it off: that is how a daemon built before the guard answers, and
	// how a deployment that turned the guard off does. A connection that merely drops keeps the
	// settings in force. A microVM pod's launcher, the controller's as an issue pod's, holds no
	// guard (serveMemoryGuard), so the settings the daemon still sends the controller's reach
	// nothing there.
	if m.memoryGuard != nil {
		m.memoryGuard.Apply(ack.MemoryGuard)
	}
	m.applyAck(writer, ack)
	// The end of this connection takes its writers out of force and leaves everything else: the
	// trimmer settings the acknowledgement carried stay in force, so the trimmer goes on sweeping
	// while the daemon restarts, and only an acknowledgement carrying none, or carrying settings
	// the trimmer refuses, stops its sweeping (applyAck).
	defer func() {
		m.mu.Lock()
		if m.writer == writer {
			m.writer = nil
		}
		if m.reportWriter == writer {
			m.reportWriter, m.reportsCarried = nil, false
		}
		m.mu.Unlock()
	}()
	if err := m.state(); err != nil {
		return err
	}
	// The kill reports of this role's processes go on this connection from here, after its state,
	// or are dropped where the daemon behind it may not know them: a gVisor pod's launcher sends
	// them where the acknowledgement carried guard settings, and a microVM pod's, the controller's
	// as an issue pod's (runsGuest), where it carried KernelKills, whatever MemoryGuard says, since
	// a daemon that sends guard settings and no KernelKills, as one from before the controller's
	// pod ran in a microVM answers the controller, would tell a role of the kernel's kill in the
	// guard's words.
	carried := ack.MemoryGuard != nil
	if m.cfg.runsGuest() {
		carried = ack.KernelKills
	}
	m.openMemoryReports(writer, carried)
	for {
		frame, err := readFrame(reader)
		if err != nil {
			return fmt.Errorf("read command: %w", err)
		}
		var answer shimwire.Frame
		switch command := frame.(type) {
		case shimwire.LauncherStart:
			answer = m.start(ctx, command)
		case shimwire.LauncherStop:
			answer = m.stop(command)
		case shimwire.LauncherLeakCheck:
			answer = m.check.receive(ctx, command, m.logf)
		default:
			return fmt.Errorf("command: got %T", frame)
		}
		if err := writer.WriteFrame(answer); err != nil {
			return fmt.Errorf("answer %T: %w", answer, err)
		}
	}
}

// validate refuses a configuration that is neither of the launcher's two forms: the controller
// pod's one role with its own token and private directory, or an issue pod's roles directory
// holding every resident role's.
func (c Config) validate() error {
	switch {
	case c.Connect == "":
		return errors.New("launcher: no connect address")
	case c.Sandbox == "":
		return errors.New("launcher: no sandbox")
	case c.PodUID == "":
		return errors.New("launcher: no pod UID")
	}
	if err := c.validateMetadata(); err != nil {
		return err
	}
	if c.RolesDir == "" {
		switch {
		case c.Token == "":
			return errors.New("launcher: no token")
		case c.Role == "":
			return errors.New("launcher: no role")
		case len(c.Roles) != 0:
			return errors.New("launcher: roles without a roles directory")
		case !filepath.IsAbs(c.PrivateDir):
			return fmt.Errorf("launcher: private directory %q is not absolute", c.PrivateDir)
		}
		return c.validateSessionDatabase()
	}
	switch {
	case c.Token != "" || c.Role != "" || c.PrivateDir != "":
		return errors.New("launcher: a roles directory holds every role's own token and private directory")
	case len(c.Roles) == 0:
		return errors.New("launcher: no roles")
	case !filepath.IsAbs(c.RolesDir):
		return fmt.Errorf("launcher: roles directory %q is not absolute", c.RolesDir)
	}
	seen := map[string]bool{}
	for _, role := range c.Roles {
		switch {
		case role == "":
			return errors.New("launcher: an empty role")
		case seen[role]:
			return fmt.Errorf("launcher: role %s twice", role)
		}
		seen[role] = true
	}
	return c.validateSessionDatabase()
}

// validateSessionDatabase refuses a session database named only in part: the instance, the database
// in it and the login together are what opens it, so a generation started without one of them would
// proxy to nothing. A service account to impersonate names a database too, so with it each of the
// three is required: startSession, which skips only the zero value, would otherwise hand the proxy
// an account and no instance, and every generation would fail. A writer audience names a database
// too, and needs the account as well, because the writer token signs in only by impersonating it.
// Naming none of the five is the other stores, which need no proxy.
//
// A writer audience is refused, too, in a form other than ompsessions.ValidWriterAudience's, and in
// the single-role form. That form runs its agent in the container's own namespaces, where the
// writer token under the container's filesystem would be in the agent's view. Only the roles-dir
// form hides it, behind the cover role-init puts over the roles directory (SessionWriterDirName).
func (c Config) validateSessionDatabase() error {
	if c.SessionDatabase == (ompsessions.Database{}) {
		return nil
	}
	if err := namedInPart("the session database",
		namedFlag{"--session-instance", c.SessionDatabase.Instance},
		namedFlag{"--session-database", c.SessionDatabase.Name},
		namedFlag{"--session-user", c.SessionDatabase.User}); err != nil {
		return err
	}
	audience := c.SessionDatabase.WriterAudience
	switch {
	case audience == "":
		return nil
	case c.SessionDatabase.ImpersonateServiceAccount == "":
		return errors.New("launcher: --session-writer-audience needs --session-impersonate-service-account:" +
			" the writer token signs in to Cloud SQL only by impersonating that service account")
	case !ompsessions.ValidWriterAudience(audience):
		return fmt.Errorf("launcher: --session-writer-audience %q is not a workload identity provider's full resource name, %s",
			audience, ompsessions.WriterAudienceForm)
	case c.RolesDir == "":
		return errors.New("launcher: --session-writer-audience needs the roles-dir form (--roles-dir):" +
			" the single-role form runs its agent in the container's own namespaces, where the writer token would be in the agent's view")
	}
	return nil
}

// validateMetadata refuses the metadata endpoint anywhere but in a microVM pod's container, and
// named in part: the pool and the identity provider together make the token exchange's audience.
// A gVisor pod reaches the node's metadata server, which the endpoint would shadow. Either form
// serves it with --microvm: a microVM issue pod's roles container, and the controller's pod, which
// the daemon renders in a microVM where the project's sandbox is one.
func (c Config) validateMetadata() error {
	if c.Metadata == nil {
		return nil
	}
	if err := namedInPart("the metadata endpoint",
		namedFlag{"--metadata-workload-pool", c.Metadata.WorkloadPool},
		namedFlag{"--metadata-identity-provider", c.Metadata.IdentityProvider}); err != nil {
		return err
	}
	if !c.MicroVM {
		return errors.New("launcher: the metadata endpoint serves a microVM's pod: it needs --microvm")
	}
	return nil
}

// runsGuest is whether this launcher's memory watch is the guest watch rather than the guard: the
// launcher of a microVM's pod (MicroVM), in either form. The daemon renders --microvm on a microVM
// issue pod's container and on the controller's pod where the project's sandbox is a microVM, so
// a microVM controller pod's launcher runs the guest watch over controllerClassifier
// (serveMemoryGuard) and sends its kill reports where the acknowledgement carried KernelKills
// (serve), as a microVM issue pod's does: the daemon sets KernelKills on the controller's
// acknowledgement too, beside its guard settings (runtime/sandbox's launcherHelloAck).
func (c Config) runsGuest() bool { return c.MicroVM }

// namedFlag is one flag of a setting that several flags name together, and its value.
type namedFlag struct{ flag, value string }

// namedInPart refuses what, a setting that flags name together, where any of them is empty,
// naming each one that is missing. Its caller decides first whether the setting is named at all
// (validateSessionDatabase, validateMetadata).
func namedInPart(what string, flags ...namedFlag) error {
	var missing []string
	for _, named := range flags {
		if named.value == "" {
			missing = append(missing, named.flag)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	return fmt.Errorf("launcher: %s is named in part: no %s", what, strings.Join(missing, ", no "))
}

func tcpAddress(addr string) (string, error) {
	address, ok := strings.CutPrefix(addr, "tcp://")
	if !ok || address == "" {
		return "", fmt.Errorf("launcher: connect %q is not tcp://host:port", addr)
	}
	if _, _, err := net.SplitHostPort(address); err != nil {
		return "", fmt.Errorf("launcher: connect %q is not tcp://host:port: %w", addr, err)
	}
	return address, nil
}

func readFrame(reader *shimwire.Reader) (shimwire.Frame, error) {
	line, err := reader.ReadLine()
	if err != nil {
		return nil, err
	}
	frame, err := shimwire.Decode(line)
	if err != nil {
		return nil, err
	}
	return frame, nil
}

type request struct {
	body   []byte
	result shimwire.Frame
}

// child is the generation this role is running: in the controller's pod the shim itself, in an
// issue pod the role-init that is the shim's own namespaces' init, with the pipe it reports the
// shim's wait status on.
type child struct {
	generation uint64
	pid        int
	// process is the generation's own handle, through which the launcher signals it: once its
	// wait has reaped it, a signal through the handle reaches nothing (Go holds the process's
	// pidfd), where one by pid could reach whatever process the kernel gave that number since.
	process *os.Process
	dir     string
	status  *os.File
	// output is the generation's own prefixed stdout and stderr in an issue pod's container,
	// whose unfinished last lines wait flushes once the generation's process is gone.
	output []io.Writer
	// proxy is the generation's session database proxy, nil when no session database is
	// configured. It belongs to the generation: it outlives the child it serves, through the
	// child's stop grace however the stop arrives, and is closed once the child has exited (wait).
	proxy *sessionproxy.Proxy
	// keeper keeps a Claude Code child's session in the session table, nil for any other child
	// (claudeKeeper). It saves until the child has exited, then makes its last save before the
	// proxy closes (wait).
	keeper *claudeKeeper
	// stopBy is when the stop under way, if any, gives up on the child: the end of its grace
	// (endGeneration), which bounds the keeper's last save. Guarded by manager.mu.
	stopBy time.Time
	done   chan struct{}
}

// manager serves one role: its own daemon connection, its own launcher id, its own token and
// private directory, its own child, and, in an issue pod's container, its own prefix in the log
// and a namespace of its own for every generation it starts. stderr is where the launcher's own
// lines for the role go, through the URL rule (newManagers): in the controller's pod the
// container's stderr, in an issue pod's container the role's own writer of log, which gives each
// generation writers of its own too.
type manager struct {
	cfg        Config
	role       string
	token      string
	private    string
	namespaced bool
	launcherID string
	stderr     io.Writer
	log        *roleLog
	writer     *shimwire.Writer
	mu         sync.Mutex
	child      *child
	lastExit   *shimwire.LauncherExit
	requests   map[string]request
	// gitLocks is the pod's one sweep of the clone's stale git locks, which every role manager
	// of the container shares: the first role start that carries a workspace tells it which
	// clone to sweep, when the container's working directory did not (gitLockSweep.learn). It is
	// nil where the launcher runs no sweep: the single-role form, and a roles launcher that is
	// not the container's first process (serveGitLockSweep); learn, its one use, takes a nil
	// sweep for one with nothing to learn. The controller's roles-dir form, under a writer
	// audience, holds a sweep that never finds a clone (newGitLockSweep).
	gitLocks *gitLockSweep
	// check is the launcher process's one install of the shared public leak check, which every
	// role manager of the container shares as it shares gitLocks.
	check *checkInstall
	// guard holds the launcher's own children, which are no generation's, out of every reap of
	// PID 1's (pidGuard): the orphan reaper's passes, and the controller pod's drain of an exited
	// generation (cleanExitedGeneration).
	guard *pidGuard
	// metadataAddress is the address this launcher serves the pod-local metadata endpoint at, which
	// every generation it starts is told (metadataEnv); "" when it serves none.
	metadataAddress string
	// memoryGuard is the pod's one memory guard, which every role manager of a roles-dir
	// container shares (an issue pod's, or the controller's under a writer audience) and the
	// controller pod's one manager holds alone in the single-role form: each connection's
	// acknowledgement hands it the daemon's settings (serve). It is nil in a launcher that is not
	// its container's first process, which runs none, and in a microVM pod's launcher, an issue
	// pod's or the controller's, which runs the guest watch instead (serveMemoryGuard).
	memoryGuard *memoryguard.Guard
	// memoryKills are the reports of this role's processes stopped for memory that no connection
	// has carried yet, oldest first, at most maxWaitingMemoryKills (queueMemoryKill). reportWriter
	// is the connection they go on, this role's current one from its state frame on, nil while
	// none is up; reportsCarried is whether that connection's acknowledgement says the daemon
	// behind it knows the report: guard settings for a gVisor pod's launcher, KernelKills for a
	// microVM pod's (serve, openMemoryReports). memorySending is the sequence of the oldest report
	// while it is being written, 0 while none is, since a guard counts its kills from 1
	// (sendMemoryKills). All four are guarded by mu.
	memoryKills    []shimwire.LauncherMemoryKill
	reportWriter   *shimwire.Writer
	reportsCarried bool
	memorySending  uint64
	// memoryWake tells this role's report sender a report is waiting (sendMemoryKillsUntil), and
	// memorySend holds one send of the waiting reports at a time (sendMemoryKills).
	memoryWake chan struct{}
	memorySend sync.Mutex
	// volumeTrimSettings is what the latest acknowledgement of this role handed the pod's cache
	// trimmer, and nil when it handed none (shimwire.LauncherHelloAck.VolumeTrim): on nil the
	// trimmer reads and deletes nothing, as on settings it refuses (volumeTrimRuns), and a report
	// it holds is discarded (trimmer.settingsInForce). Only the manager the trimmer is bound to
	// is read for it (serveVolumeTrim), and it lives under mu with the writer because the two
	// must become readable in the same moment: one connection decides both the settings and
	// where the reports go.
	volumeTrimSettings *shimwire.LauncherVolumeTrim
	// volumeTrimRuns is whether volumeTrimSettings are settings the trimmer can run, which is
	// decided once per acknowledgement (applyAck) rather than at each of the tens of thousands of
	// entry boundaries of a sweep that ask it (volumeTrimming).
	volumeTrimRuns bool
	// acknowledged is closed once this role's first acknowledgement has been applied, which is
	// when this connection counts as usable at all, and applied says it has been so that a
	// second connection's acknowledgement does not close it twice.
	acknowledged chan struct{}
	applied      bool
}

// applyAck puts one acknowledgement in force: this connection's writer and the trimmer settings
// it carried are written under the one mutex, so a report can never meet a daemon that did not
// send the settings that produced it. Settings present replace whatever the last
// acknowledgement left in force, which the next sweep runs by and never one in flight (the
// trimmer reads them at the top of each interval); settings absent make them nil, which stops
// the trimmer's sweeping until an acknowledgement carries settings again — that is a daemon
// rolled back to a release that does not know this work, and turning every pod's trimmer off is
// cheaper than meeting a frame it would close the connection on, while the roll-forward after
// it puts every pod's trimmer back without replacing a pod (trimmer.awaitSettings). Settings
// outside the trimmer's own ranges stop its sweeping the same way (refusedTrimSettings,
// trimmer.awaitSettings).
func (m *manager) applyAck(writer *shimwire.Writer, ack shimwire.LauncherHelloAck) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.writer = writer
	m.volumeTrimSettings = ack.VolumeTrim
	m.volumeTrimRuns = ack.VolumeTrim != nil && len(refusedTrimSettings(*ack.VolumeTrim)) == 0
	if !m.applied {
		m.applied = true
		close(m.acknowledged)
	}
}

// volumeTrim is the trimmer settings in force, the latest acknowledgement's: a connection that
// merely drops leaves them standing, since a daemon restart is not an operator turning the
// trimmer off. The trimmer is given a copy of its own, so nothing it read at the start of a
// sweep can change under it while that sweep runs.
func (m *manager) volumeTrim() *shimwire.LauncherVolumeTrim {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.volumeTrimSettings == nil {
		return nil
	}
	set := *m.volumeTrimSettings
	return &set
}

// volumeTrimming is whether settings the trimmer can run are in force, which is the whole of
// what a sweep asks of them at every entry boundary (trimmer.halted): decided under the same
// lock as volumeTrim reads, by the acknowledgement that wrote both, and answered as a bool so a
// sweep of tens of thousands of entries neither takes a copy of the struct nor allocates one.
func (m *manager) volumeTrimming() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.volumeTrimRuns
}

// sendVolumeReport writes one report of the pod's cache trimmer on this role's connection and no
// other: it is the connection the settings came from, so the daemon that hears of a sweep is the
// one that asked for it. The writer and the settings are read together under the one mutex
// applyAck writes them under, and a connection whose acknowledgement carried no settings carries
// no report: a daemon rolled back below this contract has no case for a launcher_volume_report
// and ServeLauncher's switch ends by closing the connection on it. The sweep itself is guarded
// at every entry boundary (trimmer.halted), but the window between its last boundary and this
// write is a fresh size walk of up to a minute (trimmer.report), and a rollback in that window
// would otherwise be met by a write.
//
// A manager with no connection up, and one whose answer carries no settings, each answer an
// error, which is what the trimmer reads as "not now": it holds the report until an
// acknowledgement puts both back, rather than dropping a volume nobody was told about, and an
// answer with no settings has the held report discarded the next time the trimmer waits for
// settings or tries its last report (trimmer.settingsInForce). They are two errors rather than one
// so that the line the pod's log carries says which of them it was. The writer is taken under the
// same mutex as state takes it, and shimwire.Writer has a mutex of its own, so writing here beside
// the command loop is safe.
func (m *manager) sendVolumeReport(report shimwire.LauncherVolumeReport) error {
	m.mu.Lock()
	writer, settings := m.writer, m.volumeTrimSettings
	m.mu.Unlock()
	switch {
	case writer == nil:
		return errors.New("no daemon connection is up to carry it")
	case settings == nil:
		return errors.New("this connection's acknowledgement carries no trimmer settings, so the daemon behind it has no case for a report")
	}
	if err := writer.WriteFrame(report); err != nil {
		return fmt.Errorf("launcher volume report: %w", err)
	}
	return nil
}

// state reports the actual child to the current daemon connection, if one is up. A state change
// while disconnected is reported by the next connection's first state frame.
func (m *manager) state() error {
	m.mu.Lock()
	state := shimwire.LauncherState{LastExit: m.lastExit}
	if m.child != nil {
		state.Child = &shimwire.LauncherChild{Generation: m.child.generation, PID: m.child.pid}
	}
	writer := m.writer
	m.mu.Unlock()
	if writer == nil {
		return nil
	}
	if err := writer.WriteFrame(state); err != nil {
		return fmt.Errorf("launcher state: %w", err)
	}
	return nil
}

// start runs command's generation. ctx is the container's: once it has ended (the container's
// SIGTERM) no generation starts, since one started then would be given the whole stop grace from
// that moment and outlast the pod's own; the session lookup a resume waits on ends with it too.
func (m *manager) start(ctx context.Context, command shimwire.LauncherStart) shimwire.LauncherStartResult {
	if err := command.Validate(); err != nil {
		return shimwire.LauncherStartResult{ID: command.ID, Error: err.Error()}
	}
	body, _ := json.Marshal(command)
	// refuse is how a start that cannot run answers: the refusal is remembered under the command's
	// id, so a retry of that id is answered with it rather than tried again. A refusal over the
	// session to resume names its kind in the answer's own field, and the remembered answer carries
	// it too, so the retry says the same thing (shimwire.SessionRefusal).
	refuse := func(cause error) shimwire.LauncherStartResult {
		result := shimwire.LauncherStartResult{ID: command.ID, Error: cause.Error()}
		for _, kind := range []shimwire.SessionRefusal{shimwire.SessionGone, shimwire.SessionStoreUnreachable} {
			if errors.Is(cause, kind) {
				result.Session = kind
				break
			}
		}
		m.remember(command.ID, body, result)
		return result
	}
	m.mu.Lock()
	if prior, ok := m.requests[command.ID]; ok {
		result, matches := prior.result.(shimwire.LauncherStartResult)
		m.mu.Unlock()
		if !matches || string(prior.body) != string(body) {
			return shimwire.LauncherStartResult{ID: command.ID, Error: "launcher_start reused an id with a different payload"}
		}
		return result
	}
	if current := m.child; current != nil {
		m.mu.Unlock()
		return shimwire.LauncherStartResult{ID: command.ID, Error: fmt.Sprintf("generation %d is still running", current.generation)}
	}
	if m.lastExit != nil && command.Generation <= m.lastExit.Generation {
		m.mu.Unlock()
		return shimwire.LauncherStartResult{ID: command.ID, Error: fmt.Sprintf("generation %d already exited", command.Generation)}
	}
	m.mu.Unlock()
	env := mergeEnv(os.Environ(), command.Env)
	// The generation's own directory comes first, since everything else of the generation lives in
	// it: the session database's connection file and the socket its proxy binds, which the resume
	// lookup reaches the table through and which the child must find already bound.
	dir, err := m.writeFiles(command)
	if err != nil {
		return refuse(err)
	}
	proxy, clientURL, err := m.startSession(ctx, command, env, dir)
	var keeper *claudeKeeper
	// abandon refuses a start that made the generation's directory, undoing what it made first.
	abandon := func(cause error) shimwire.LauncherStartResult {
		keeper.close()
		endSession(proxy, dir)
		return refuse(cause)
	}
	if err != nil {
		return abandon(err)
	}
	// A start that names its session by its id alone (ResumeSession, which the daemon sends only to
	// a launcher whose hello said resumesBySession, and which Validate refused beside a ResumeFile)
	// resumes the newest session the table holds under that id (sessionByID). The found path
	// becomes the start's ResumeFile and takes the place of the argv's one `--resume=<id>`, so the
	// child resumes that exact file rather than leave Oh My Pi to resolve the id, and from here the
	// start is a path resume: resumable, the recreated-workspace notice and the child. A table that
	// holds no such session refuses the start before any child, as resumable refuses a file it
	// does not hold. The record a retry of the same id is compared with keeps the frame as
	// received: body was marshalled from it above, and the change is made to this call's copy of
	// the frame with its argv copied (resumeArgv), so the caller's frame is not changed either.
	if command.ResumeSession != "" {
		file, err := sessionByID(ctx, env, command.ResumeSession, clientURL)
		if err != nil {
			return abandon(err)
		}
		command.Argv = resumeArgv(command.Argv, command.ResumeSession, file)
		command.ResumeFile = file
	}
	// A Claude Code child's session is kept in the session table (claudeKeeper): the order is
	// the resume's lookup, which decides the recreated-workspace notice from the row's last save,
	// then the restore of the session's files from their rows, then the child, then the saves.
	if keeper, err = newClaudeKeeper(env, command.ResumeFile, clientURL, m.stderr); err != nil {
		return abandon(err)
	}
	recreated := false
	var sessions *ompsessions.Conn
	if command.ResumeFile != "" {
		if recreated, sessions, err = resumable(ctx, env, command.ResumeFile, clientURL, m.stderr); err != nil {
			return abandon(err)
		}
	}
	if err := keeper.open(ctx, sessions); err != nil {
		return abandon(err)
	}
	// The clone's stale git locks: this start names the workspace the sweep works, and a sweep
	// that had none sweeps it here, before the role's child runs git and jj against that clone,
	// where a stale lock would fail them (gitLockSweep.learn). A launcher that runs no sweep holds
	// none here, and the role starts on the clone as it stands.
	m.gitLocks.learn(ctx, envValue(env, "LEGION_WORKSPACE"))
	env = mergeEnv(env, []string{WorkspaceRecreatedVariable + "=" + strconv.FormatBool(recreated)})
	// The pod's own test database: the DSN this launcher serves, over whatever the daemon's start
	// command carried, and no such variable at all when this launcher serves none (testPGEnv).
	env = testPGEnv(env, m.cfg.TestPostgres)
	// A start into a pod the daemon kept names the credential its slot's gh files must hold, which
	// the daemon wrote into the slot's Secret just before: the child starts only once the kubelet's
	// projection holds it, so its first push or comment is never made as the App the files held.
	if command.GitHubCredential != nil {
		if err := m.awaitGitHubCredential(ctx, env, *command.GitHubCredential); err != nil {
			return abandon(err)
		}
	}
	// The pod-local metadata endpoint: its address, over whatever the daemon's start command
	// carried, when this launcher serves it, and the environment as it is when it does not
	// (metadataEnv).
	env = metadataEnv(env, m.metadataAddress)
	process, err := m.command(command, env)
	if err != nil {
		return abandon(err)
	}
	// The orphan reaper must see this PID as the direct child before it can observe its exit, and
	// stopActive, which takes the lock, either sees this generation or finds the container
	// stopping here and nothing started.
	m.mu.Lock()
	if ctx.Err() != nil {
		m.mu.Unlock()
		if process.status != nil {
			_ = process.status.Close()
			_ = process.write.Close()
		}
		return abandon(errors.New("the launcher's container is stopping; no generation starts"))
	}
	if err := process.start(); err != nil {
		m.mu.Unlock()
		return abandon(fmt.Errorf("start child: %w", err))
	}
	active := &child{generation: command.Generation, pid: process.cmd.Process.Pid, process: process.cmd.Process, dir: dir, status: process.status, output: process.output, proxy: proxy, keeper: keeper, done: make(chan struct{})}
	m.child = active
	result := shimwire.LauncherStartResult{ID: command.ID, OK: true, RunningGeneration: command.Generation}
	m.requests[command.ID] = request{body: body, result: result}
	m.mu.Unlock()
	go keeper.run()
	go m.wait(process.cmd, active)
	_ = m.state()
	return result
}

// ghRoot is the directory of the slot's gh files as this launcher sees them: the role's own gh
// directory of the roles directory for a role of an issue pod (RoleGHDir), and the gh bundle root
// its start names (LEGION_GH_OWNERS_DIR) for the controller's launcher, whose container mounts it
// there.
func (m *manager) ghRoot(env []string) string {
	if m.namespaced {
		return filepath.Join(m.cfg.RolesDir, m.role, RoleGHDir)
	}
	return envValue(env, "LEGION_GH_OWNERS_DIR")
}

// awaitGitHubCredential waits, reading once a second, until the slot's hosts.yml that want names
// holds a token whose SHA-256 is want's digest, and answers why it stopped waiting when ctx ends
// first.
func (m *manager) awaitGitHubCredential(ctx context.Context, env []string, want shimwire.LauncherGitHubCredential) error {
	if !filepath.IsLocal(want.HostsFile) {
		return fmt.Errorf("the start names the gh file %q, which is not under the slot's gh directory", want.HostsFile)
	}
	path := filepath.Join(m.ghRoot(env), filepath.FromSlash(want.HostsFile))
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		if data, err := os.ReadFile(path); err == nil {
			if token, err := ghconfig.TokenFromHosts(data); err == nil {
				sum := sha256.Sum256([]byte(token))
				if hex.EncodeToString(sum[:]) == want.TokenDigest {
					return nil
				}
			}
		}
		select {
		case <-ctx.Done():
			return errors.New("the slot's gh files do not hold the credential of this start yet")
		case <-tick.C:
		}
	}
}

func (m *manager) remember(id string, body []byte, result shimwire.Frame) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.requests[id] = request{body: body, result: result}
}

// command is one generation's process: in an issue pod's container, role-init as the init of
// this role's new user, mount, PID and IPC namespaces, with the pipe it reports the shim's own
// wait status on and prefixed writers of the generation's own; in the controller's pod, the shim
// itself in the container's own namespaces, writing to the container's stdout and stderr
// themselves (cfg.Stdout and cfg.Stderr, never the launcher's own writer, which is no file), so
// the shim's descriptors are the container's files and no pipe through the launcher.
func (m *manager) command(start shimwire.LauncherStart, env []string) (*roleProcess, error) {
	if m.namespaced {
		stdout, stderr := m.log.writers(m.role)
		process, err := newRoleProcess(RoleInitConfig{
			RoleCover: roleCoverFor(m.cfg.RolesDir), Role: m.role, Generation: start.Generation, Argv: start.Argv,
		}, env, stdout, stderr)
		if err != nil {
			return nil, err
		}
		process.output = []io.Writer{stdout, stderr}
		return process, nil
	}
	cmd := exec.Command(start.Argv[0], start.Argv[1:]...)
	cmd.Env = env
	cmd.Stdout, cmd.Stderr = m.cfg.Stdout, m.cfg.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return &roleProcess{cmd: cmd}, nil
}

// wait reports the generation's exit once its process is gone. A role's exit is the shim's own:
// its namespace init reports the status it waited on, and only an init killed before the shim
// ended — or one that never started it — reports its own. An init that exited with
// RoleInitSetupFailedExit having reported nothing failed before it started the shim (setting up
// the role's namespaces, or starting the shim), which is reported as
// shimwire.LauncherExitSetupFailed rather than as an exit code a shim could have exited with; any
// other exit of an init that reported nothing (a panic's 2, say) is reported as that exit. Once
// the process and its output copies are done, the last line the generation left unfinished is
// written out, so it is neither lost nor joined to the next generation's first.
//
// The generation's session database proxy is closed only once its process is gone, so the child is
// served through the whole of its stop grace however the stop arrives, the daemon's stop or the
// container's own SIGTERM; and after every process of the generation has ended (its PID namespace's
// init gone in an issue pod, cleanExitedGeneration in the controller's PID 1), and so every
// connection they held. A Claude Code child's keeper makes its last save between the two
// (claudeKeeper.finish), until the stop's grace less the proxy's drain is over, so that save and
// the keeper's own connection are done before the proxy closes. Close is bounded by sessionDrain
// however many holders remain: off PID 1 cleanExitedGeneration returns at once and a socket holder
// outside the child's process group outlives it, and Close closes that holder's connection once
// the drain is over rather than wait on it. It is closed before the generation's directory goes, so
// nothing is left listening over a socket that has been removed, and between generations nothing is
// bound at all. active.done is closed last, so endGeneration, and with it Run's stopActive, returns
// only once the proxy is closed.
func (m *manager) wait(cmd *exec.Cmd, active *child) {
	var reported *syscall.WaitStatus
	if active.status != nil {
		raw, _ := io.ReadAll(active.status)
		_ = active.status.Close()
		reported = parseStatus(raw)
	}
	if err := cmd.Wait(); err != nil && cmd.ProcessState == nil {
		m.logf("launcher: wait for generation %d: %v", active.generation, err)
	}
	for _, output := range active.output {
		if w, ok := output.(*prefixWriter); ok {
			w.flush()
		}
	}
	if !m.namespaced {
		m.cleanExitedGeneration()
	}
	exit := exitOf(cmd.ProcessState, active.generation)
	switch {
	case reported != nil:
		exit = exitOfStatus(*reported, active.generation)
	case m.namespaced && cmd.ProcessState != nil && cmd.ProcessState.Exited() && cmd.ProcessState.ExitCode() == RoleInitSetupFailedExit:
		exit = shimwire.LauncherExit{Generation: active.generation, Code: shimwire.LauncherExitSetupFailed}
		m.logf("launcher: generation %d of role %s: role-init failed before it started the shim; its own lines above say why",
			active.generation, m.role)
	}
	m.mu.Lock()
	end := active.stopBy
	m.mu.Unlock()
	// A child that exited on its own is given the whole grace a stop would have given it.
	if end.IsZero() {
		end = time.Now().Add(m.cfg.StopGrace)
	}
	active.keeper.finish(end.Add(-sessionDrain))
	if active.proxy != nil {
		_ = active.proxy.Close()
	}
	m.mu.Lock()
	if m.child == active {
		m.child = nil
		m.lastExit = &exit
	}
	// The generation's credentials end with it. Its agent wrote under the directory, so nothing
	// there is followed (rolefile.RemoveTree).
	_ = rolefile.RemoveTree(active.dir)
	close(active.done)
	m.mu.Unlock()
	_ = m.state()
}

func (m *manager) stop(command shimwire.LauncherStop) shimwire.LauncherStopResult {
	if err := command.Validate(); err != nil {
		return shimwire.LauncherStopResult{ID: command.ID, Error: err.Error()}
	}
	body, _ := json.Marshal(command)
	m.mu.Lock()
	if prior, ok := m.requests[command.ID]; ok {
		result, matches := prior.result.(shimwire.LauncherStopResult)
		m.mu.Unlock()
		if !matches || string(prior.body) != string(body) {
			return shimwire.LauncherStopResult{ID: command.ID, Error: "launcher_stop reused an id with a different payload"}
		}
		return result
	}
	active := m.child
	m.mu.Unlock()
	if active == nil {
		result := shimwire.LauncherStopResult{ID: command.ID, OK: true}
		m.remember(command.ID, body, result)
		return result
	}
	if active.generation != command.Generation {
		result := shimwire.LauncherStopResult{ID: command.ID, Error: fmt.Sprintf("generation %d is running, not %d", active.generation, command.Generation)}
		m.remember(command.ID, body, result)
		return result
	}
	if err := m.endGeneration(active, time.Duration(command.GraceMs)*time.Millisecond); err != nil {
		result := shimwire.LauncherStopResult{ID: command.ID, Error: err.Error()}
		m.remember(command.ID, body, result)
		return result
	}
	result := shimwire.LauncherStopResult{ID: command.ID, OK: true}
	m.remember(command.ID, body, result)
	return result
}

// stopActive ends this role's generation at the container's own stop. A failure there is this
// role's alone in an issue pod's container: PID 1 serves every other role, so it is logged
// rather than thrown, where the controller pod's single role still fails its container loudly.
func (m *manager) stopActive() {
	m.mu.Lock()
	active := m.child
	m.mu.Unlock()
	if active == nil {
		return
	}
	if err := m.endGeneration(active, m.cfg.StopGrace); err != nil {
		if !m.namespaced {
			panic(err)
		}
		m.logf("launcher: stop role %s: %v", m.role, err)
	}
}

// generationDir is where generation's credentials live under the private directory.
func generationDir(private string, generation uint64) string {
	return filepath.Join(private, shimwire.GenerationDir(generation))
}

// writeFiles materializes command's credentials in a fresh directory for its generation. No child
// runs when it is called, so everything already in the private directory — an earlier
// generation's credentials a crashed launcher never removed, or a directory, file or symlink the
// previous generation's agent planted toward the shared workspace — is removed without being
// followed (rolefile.RemoveTree): a link anywhere under the directory is unlinked, never opened,
// so what it names is never touched. An entry the launcher cannot unlink, such as a link the agent
// left under a directory it made 0555, which the launcher, holding no capability, cannot pass, is
// left as it is, and the start is refused naming the entry. The directory is made owner-only and
// each file is written as writeCredential writes one, so no credential is written anywhere but
// that directory. An error names the file, never its bytes.
func (m *manager) writeFiles(command shimwire.LauncherStart) (string, error) {
	entries, err := os.ReadDir(m.private)
	if err != nil {
		return "", fmt.Errorf("read the private directory: %v", err)
	}
	for _, entry := range entries {
		if err := rolefile.RemoveTree(filepath.Join(m.private, entry.Name())); err != nil {
			return "", fmt.Errorf("clear %s from the private directory: %v", entry.Name(), err)
		}
	}
	dir := generationDir(m.private, command.Generation)
	if err := os.Mkdir(dir, 0o700); err != nil {
		return "", fmt.Errorf("make credential directory for generation %d: %v", command.Generation, err)
	}
	for name, content := range command.Files {
		if err := writeCredential(filepath.Join(dir, name), content); err != nil {
			_ = rolefile.RemoveTree(dir)
			return "", fmt.Errorf("write credential %s for generation %d: %v", name, command.Generation, err)
		}
	}
	return dir, nil
}

// roleLog is the one container log an issue pod's slots share: every line a slot's launcher or
// its child writes carries that slot's name (its --role, the name of the agent that runs in it,
// whatever role its step is on), since `kubectl logs` shows one log for the pod's slots where one
// container per role showed six. Whole lines are written under one lock, so two slots never
// interleave inside a line. Each writer holds its own unfinished line, so the launcher's lines for
// a slot and each generation of its child are written through writers of their own, and none is
// ever joined to another's unfinished line. stdout and stderr are the
// container's behind the URL rule (newManagers): each line reaches them in one write, so the rule
// reads it whole, a child's lines among them however its pipe was read, but for a line longer than
// maxLogLine, which reaches them where logLineCut cuts it.
type roleLog struct {
	mu             sync.Mutex
	stdout, stderr io.Writer
}

// writers are a new pair of role's own stdout and stderr: nil where the launcher was given none,
// which exec.Cmd turns into the null device for the child.
func (l *roleLog) writers(role string) (stdout, stderr io.Writer) {
	if l.stdout != nil {
		stdout = &prefixWriter{log: l, out: l.stdout, prefix: role + ": "}
	}
	if l.stderr != nil {
		stderr = &prefixWriter{log: l, out: l.stderr, prefix: role + ": "}
	}
	return stdout, stderr
}

// linef writes one whole line of the launcher's own, no role's, so it carries no prefix: the
// daemon quotes such lines with every role's when the container ends (sandbox's containerLines).
func (l *roleLog) linef(format string, args ...any) {
	if l.stderr == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	_, _ = fmt.Fprintf(l.stderr, format+"\n", args...)
}

// maxLogLine bounds how much of one unfinished line a role can make PID 1 hold: a longer line is
// written out as lines of their own, each at most that length and cut where logLineCut says, and
// the rest follows behind the prefix again.
const maxLogLine = 64 * 1024

// prefixWriter holds back a partial line until its newline arrives, so a prefix only ever
// precedes the start of a line. It never fails its caller: a line the container's log refuses is
// dropped, since it has nowhere else to go, and a role whose output could not be copied would
// block on a full pipe or lose its output for good.
type prefixWriter struct {
	log     *roleLog
	out     io.Writer
	prefix  string
	partial []byte
	// line is the buffer each prefixed line is built in, reused from one line to the next.
	line []byte
}

func (w *prefixWriter) Write(p []byte) (int, error) {
	w.log.mu.Lock()
	defer w.log.mu.Unlock()
	// What partial holds from earlier writes holds no newline, so the search starts after it.
	start, scanned := 0, len(w.partial)
	w.partial = append(w.partial, p...)
	for {
		rest := w.partial[start:]
		end := bytes.IndexByte(rest[scanned:], '\n')
		switch {
		case end >= 0:
			end += scanned
			w.emit(rest[:end+1], false)
			start, scanned = start+end+1, 0
		case len(rest) >= maxLogLine:
			cut := logLineCut(rest[:maxLogLine])
			w.emit(rest[:cut], true)
			// rest holds no newline, so neither does what follows the cut.
			start, scanned = start+cut, len(rest)-cut
		default:
			w.partial = w.partial[:copy(w.partial, rest)]
			return len(p), nil
		}
	}
}

// emit writes one line behind the role's prefix. It ends itself a line that holds no newline of
// its own: a run Write cut at logLineCut, or the unfinished line flush writes out.
func (w *prefixWriter) emit(line []byte, cut bool) {
	w.line = append(append(w.line[:0], w.prefix...), line...)
	if cut {
		w.line = append(w.line, '\n')
	}
	_, _ = w.out.Write(w.line)
}

// logLineCut is where prefixWriter cuts run, a line's first maxLogLine bytes with no newline in
// them, so that a line longer than maxLogLine reaches the container's log in writes of its own:
// just after run's last byte that ends a URL for the URL rule (redact.EndsURLByte). Each URL of
// the part before the cut then ends inside it, and each of the rest starts after it, so the rule
// of the container's log (redact.Writer) reads each whole, in one write, and removes from the two
// writes what it would remove from the line whole. A run whose only such byte is its first, or
// that holds none, is cut at its end: one URL can be such a run, and that cut is the one cut of
// the role log that can fall inside a URL. The search reads back from the run's end to the cut,
// and the bytes it read past hold no such byte, so a line's cuts read each of its bytes twice at
// most.
func logLineCut(run []byte) int {
	for i := len(run) - 1; i > 0; i-- {
		if redact.EndsURLByte(run[i]) {
			return i + 1
		}
	}
	return len(run)
}

// flush writes the unfinished line the writer holds, behind its prefix and ended as a line of its
// own: called once the generation writing to it is gone and its output copied, so what it wrote
// last without a newline is neither lost nor joined to anything written after.
func (w *prefixWriter) flush() {
	w.log.mu.Lock()
	defer w.log.mu.Unlock()
	if len(w.partial) > 0 {
		w.emit(w.partial, true)
		w.partial = w.partial[:0]
	}
}

// writeCredential writes content to path the one way anything of a generation's is written: created
// exclusively, owner-only, and never following a link, so a credential lands in the generation's
// own directory and nowhere else, and never through a file or symlink already at that path. The
// error it returns names neither the file nor its bytes; each caller names the file itself.
func writeCredential(path, content string) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	_, err = file.WriteString(content)
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	return err
}

// startSession gives a generation its session database, when one is configured: the connection URL
// the child's Oh My Pi reads, written into the generation's own directory the way writeFiles writes
// a credential, and the proxy bound on the socket beside it, which has proven one upstream
// connection before Start returns. The URL it answers with is that same endpoint for the launcher's
// own pgx client, which spells a unix socket differently from the client the agent runs
// (ompsessions.Endpoint); with no session database it is "", and the launcher looks a resume up
// through the pointer the child's environment carries instead. A failure leaves nothing bound and
// names the generation; its caller removes the directory.
//
// In an issue pod's container the launcher and the role see the generation's directory at two
// paths: the launcher binds the socket at dir, under the role's own private directory of the roles
// directory, and the role's namespaces show that directory at the fixed root (role-init binds it
// back there), so the URL the agent reads names the socket where the agent finds it. The role's
// processes run as the launcher's own user, so the socket is theirs to connect to.
//
// With a writer audience, the proxy impersonates the database's service account with the writer
// token at <roles-dir>/session-writer/token (SessionWriterDirName), and admits only the launcher and
// the generation's agent (sessionPeer). The token's path goes to the proxy alone, never into the
// generation's environment or a file. The generation is refused before anything is written where
// Yama lets a role's process trace the agent (requireTraceScope), or where no process could be
// known as the agent (newSessionPeer): command names the agent's executable, and env, the
// generation's environment, says whether the start is a Claude Code one.
//
// ctx bounds the proxy's start, so a start still in progress when the container is told to stop is
// abandoned, and its proof dial is bounded besides by proofTimeout, so an instance that never
// answers is refused while the daemon still waits for the answer. A started proxy ignores ctx: it
// serves until the generation's own end closes it (wait), which lets it serve the child through its
// stop grace after ctx has ended.
func (m *manager) startSession(ctx context.Context, command shimwire.LauncherStart, env []string, dir string) (*sessionproxy.Proxy, string, error) {
	database := m.cfg.SessionDatabase
	if database == (ompsessions.Database{}) {
		return nil, "", nil
	}
	generation := command.Generation
	config := sessionproxy.Config{
		Instance: database.Instance, ImpersonateServiceAccount: database.ImpersonateServiceAccount, NewDialer: m.cfg.SessionDialer,
		ProofTimeout: m.proofTimeout(), Drain: sessionDrain, Log: m.stderr,
	}
	if database.WriterAudience != "" {
		if err := requireTraceScope(); err != nil {
			return nil, "", fmt.Errorf("generation %d: %v", generation, err)
		}
		peer, err := newSessionPeer(m, command, env)
		if err != nil {
			return nil, "", fmt.Errorf("generation %d: %v", generation, err)
		}
		config.WriterAudience = database.WriterAudience
		config.WriterTokenFile = filepath.Join(m.cfg.RolesDir, SessionWriterDirName, SessionWriterTokenFile)
		config.Admit = peer.admit
	}
	local := ompsessions.Endpoint{SocketDir: dir, Database: database.Name, User: database.User}
	agent := local
	if m.namespaced {
		agent.SocketDir = generationDir(filepath.Join(roleFixedRoot, RolePrivateDir), generation)
	}
	if err := writeCredential(filepath.Join(dir, ompsessions.DSNFileName), agent.AgentURL()); err != nil {
		return nil, "", fmt.Errorf("write the session database URL for generation %d: %v", generation, err)
	}
	config.Socket = local.Socket()
	proxy, err := sessionproxy.Start(ctx, config)
	if err != nil {
		return nil, "", fmt.Errorf("the session database proxy for generation %d: %v", generation, err)
	}
	return proxy, local.ClientURL(), nil
}

// proofTimeout bounds a generation's proof dial (sessionproxy.Config.ProofTimeout) at half the stop
// grace, which Run has made positive. The daemon's wait on a start ends at the launcher's connection
// deadline plus its termination grace (runtime/sandbox's relaunch), and that grace is the
// --stop-grace it hands the launcher (runtime/sandbox's manifest), so the daemon waits at least
// StopGrace past the launcher's connection, however late it connected. Half of it bounds the
// proof; the other half is left for the rest of the start and its answer.
func (m *manager) proofTimeout() time.Duration { return m.cfg.StopGrace / 2 }

// sessionDrain is how long a generation's proxy lets connections still open at its Close finish
// (sessionproxy.Config.Drain) before it closes them. Close runs only once the generation's child
// has exited, and in a pod's PID 1 once every process of the generation is gone, whose connections
// have then already closed, so anything still connected is no part of the agent's work. One second
// lets a copy already under way deliver what it holds, and no holder outside the generation can
// keep the generation, the next one or Run waiting longer.
const sessionDrain = time.Second

// endSession ends a generation that will not run after all: its proxy stops and its directory goes,
// so no child ever starts against a half-made generation and nothing of it is left bound or on
// disk. wait does the same for a generation that ran.
func endSession(proxy *sessionproxy.Proxy, dir string) {
	if proxy != nil {
		_ = proxy.Close()
	}
	_ = rolefile.RemoveTree(dir)
}

// resumable refuses a resume of a session nothing holds, so the launcher never starts a fresh
// agent behind the daemon's back: Oh My Pi starts a new session at a --resume path it finds empty.
// It looks where the child's Oh My Pi will, in the session table (the runtime's
// runtime.kubernetes.session_store, which must keep sessions in a database: no issue disk holds
// one), reached as sessionDial reaches it. A
// Claude Code resume's file is its transcript's path, `<CLAUDE_CONFIG_DIR>/projects/<dir>/<id>.jsonl`
// (the session a claude-envoy claim records), which is the very key its launcher kept the
// transcript's row under (claudeKeeper): the row's mtime_ms is that file's mtime at its last save.
// It returns the connection it looked through, open, which the generation's keeper keeps
// (claudeKeeper.open), so a resume reaches the database once.
//
// It also reports whether the workspace the child works in, LEGION_WORKSPACE, was recreated since the
// session was last written: provisioning recorded its creation later than that write
// (workspace.Created), so the agent's last turns ran in a workspace that is gone and this one holds
// only what was pushed. A workspace with no record, or a child with no workspace (the controller),
// was not. A record that cannot be read, which an init container killed mid-write can leave, only
// decides that notice: it is written to log and taken as no record, and the resume goes ahead.
func resumable(ctx context.Context, env []string, file, clientURL string, log io.Writer) (recreated bool, conn *ompsessions.Conn, err error) {
	sessions, err := newSessionDial(env, clientURL)
	if err != nil {
		return false, nil, fmt.Errorf("resume session %s: %w: %v", file, shimwire.SessionStoreUnreachable, err)
	}
	written, conn, err := sessionWritten(ctx, sessions, file)
	if err != nil {
		return false, nil, err
	}
	dir := envValue(env, "LEGION_WORKSPACE")
	if dir == "" {
		return false, conn, nil
	}
	created, ok, err := workspace.Created(dir)
	if err != nil {
		if log != nil {
			fmt.Fprintf(log, "legion launcher: resume session %s: %v; the workspace is taken as not recreated\n", file, err)
		}
		return false, conn, nil
	}
	return ok && written.Before(created), conn, nil
}

// sessionWritten is when the session a resume names was last written, read from the session table
// the child's Oh My Pi keeps it in (resumable) through a connection sessions dials, which it returns
// open. A table that could not be asked at all is shimwire.SessionStoreUnreachable; a session the
// table answers it does not hold is shimwire.SessionGone, refused at once.
func sessionWritten(parent context.Context, sessions sessionDial, file string) (time.Time, *ompsessions.Conn, error) {
	var written time.Time
	var found bool
	conn, err := sessions.dial(parent, func(ctx context.Context, conn *ompsessions.Conn) (err error) {
		written, found, err = ompsessions.Written(ctx, conn, file)
		return err
	})
	if err != nil {
		return time.Time{}, nil, fmt.Errorf("resume session %s: %w: %v", file, shimwire.SessionStoreUnreachable, err)
	}
	if !found {
		closeSessions(conn)
		return time.Time{}, nil, fmt.Errorf("resume session %s: %w", file, shimwire.SessionGone)
	}
	return written, conn, nil
}

// sessionByID is the file of the newest session the session table holds under the session id id
// (ompsessions.FindByID), for a start that names its session by its id alone, asked through the
// same sessionDial resumable's lookup uses. The connection is closed once it answers, since the
// path resume that follows (resumable) dials its own and hands it to the keeper. A table that could
// not be asked at all is shimwire.SessionStoreUnreachable; a table that answers it holds no such
// session is shimwire.SessionGone, refused at once, so no child starts.
func sessionByID(ctx context.Context, env []string, id, clientURL string) (string, error) {
	sessions, err := newSessionDial(env, clientURL)
	if err != nil {
		return "", fmt.Errorf("resume session %s: %w: %v", id, shimwire.SessionStoreUnreachable, err)
	}
	var file string
	var found bool
	conn, err := sessions.dial(ctx, func(ctx context.Context, conn *ompsessions.Conn) (err error) {
		file, found, err = ompsessions.FindByID(ctx, conn, id)
		return err
	})
	if err != nil {
		return "", fmt.Errorf("resume session %s: %w: %v", id, shimwire.SessionStoreUnreachable, err)
	}
	closeSessions(conn)
	if !found {
		return "", fmt.Errorf("resume session %s: %w", id, shimwire.SessionGone)
	}
	return file, nil
}

// resumeArgv is a copy of argv whose element `--resume=<id>` reads `--resume=<file>`, so the
// child resumes the exact file the table holds and the frame the argv came from is left as the
// daemon sent it. LauncherStart.Validate holds a start naming its session by its id to exactly one
// such element.
func resumeArgv(argv []string, id, file string) []string {
	resumed := slices.Clone(argv)
	for i, arg := range resumed {
		if arg == "--resume="+id {
			resumed[i] = "--resume=" + file
		}
	}
	return resumed
}

// envValue is name's value in env, the last entry naming it, "" when none does.
func envValue(env []string, name string) string {
	for i := len(env) - 1; i >= 0; i-- {
		if value, ok := strings.CutPrefix(env[i], name+"="); ok {
			return value
		}
	}
	return ""
}

func mergeEnv(base, updates []string) []string {
	values := map[string]string{}
	order := make([]string, 0, len(base)+len(updates))
	add := func(entry string) {
		name, value, ok := strings.Cut(entry, "=")
		if !ok || name == "" {
			return
		}
		if _, seen := values[name]; !seen {
			order = append(order, name)
		}
		values[name] = value
	}
	for _, entry := range base {
		add(entry)
	}
	for _, entry := range updates {
		add(entry)
	}
	out := make([]string, 0, len(order))
	for _, name := range order {
		out = append(out, name+"="+values[name])
	}
	return out
}

// withoutEnv is env with every entry naming any of names dropped. mergeEnv can only give a name a
// value, and a launcher that must tell a generation nothing about a name has to leave it unset: a
// role that read an empty DSN would take it for a server at libpq's defaults rather than for none.
func withoutEnv(env []string, names ...string) []string {
	out := make([]string, 0, len(env))
entries:
	for _, entry := range env {
		for _, name := range names {
			if strings.HasPrefix(entry, name+"=") {
				continue entries
			}
		}
		out = append(out, entry)
	}
	return out
}

// exitOf is the exit a process's own wait status reports. A wait that produced no status reports
// code -1, which no process exits with: the exit is unknown, never a clean 0.
func exitOf(state *os.ProcessState, generation uint64) shimwire.LauncherExit {
	if state != nil {
		if status, ok := state.Sys().(syscall.WaitStatus); ok {
			return exitOfStatus(status, generation)
		}
	}
	return shimwire.LauncherExit{Generation: generation, Code: -1}
}

func randomID() string {
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(bytes[:])
}
