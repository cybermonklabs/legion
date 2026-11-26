package daemon

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/execfile"
	"github.com/sjawhar/legion/daemon/internal/fixturegit"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/launcher"
	"github.com/sjawhar/legion/daemon/internal/ompsessions"
	"github.com/sjawhar/legion/daemon/internal/sessionproxy"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/testtmp"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// idleRigPods runs the processes of every pod the rig's Kubernetes stand-in reports Ready: one
// real `legion launcher` per role of the pod, started in this process as
// packages/daemon/cmd/legion/launcher_test.go starts the command's launcher (launcher.Run over the
// address the daemon's worker stream bound, with the role's own token), and stopped when the pod
// is deleted.
//
// It is the launcher's single-role form, one Run per role rather than one Run for the whole pod's
// roles directory. An issue pod's container runs the roles-dir form, which starts every role
// generation as the init of new user, mount, PID and IPC namespaces over the worker image's
// layout (`legion launcher role-init`, internal/launcher/roleinit_linux.go), and this container
// has neither that layout nor a way to make its mount points. The daemon's side of the protocol is
// the same either way: one authenticated launcher connection per role, each reporting its own
// child's state.
//
// Each launcher's generations keep their Oh My Pi session in the rig's Postgres, through the
// launcher's own session proxy: the runtime is configured with the cloudsql-iam store, so a
// resume's session lookup goes through the proxy's socket (internal/launcher's startSession and
// resumable) rather than through a providers mount this container cannot have, and the proxy's
// upstream dialer is a plain TCP dial of the test database.
type idleRigPods struct {
	t *testing.T
	// launch is how the launchers of a pod run, asked when the pod is Ready: the worker stream
	// they dial and whether they run as a microVM pod's (launcher.Config's Connect and MicroVM),
	// or ok false where none of them may start.
	launch func(pod, podUID string) (connect string, microVM, ok bool)
	// session is the Cloud SQL session database the launchers proxy to, and dial is its upstream.
	session ompsessions.Database
	dial    func(ctx context.Context, signer sessionproxy.Signer) (sessionproxy.Dialer, error)
	// gitHub is a slot's gh files as the pod's projection of the slot's Secret holds them, by
	// their path under its gh directory (issueSandboxAPI.gitHubFiles), which each launcher's
	// projection writes (projectGitHub).
	gitHub func(pod, slot string) map[string][]byte

	mu sync.Mutex
	// running is each live pod's launchers, by pod name.
	running map[string]*idleRigPodProcesses
	// logs is every launcher's own log, for a failure to name what the pod did.
	logs *syncBuffer
	// started counts the launchers started for a pod, by pod name: a pod that came back is a
	// second start, which is what a wake must produce.
	started map[string]int

	// dirMu guards dirNext, the counter privateDir numbers each launcher's directory with.
	// dirRoot is set once, in newIdleRigPods, and only read afterwards.
	dirMu   sync.Mutex
	dirRoot string
	dirNext int

	// workspace is the issue workspace every launcher of the rig reads when the daemon asks
	// (launcher.Config.Workspace): a jj workspace holding no work, of a shared clone, both where
	// provisioning lays them out (workspace.Location), since the launcher's read runs the
	// workspace guards, which derive the clone from that layout (workspace.GuardJJ). It is made once
	// in newIdleRigPods, so the release of a pod no claim owns reads it clean through the pod's
	// launcher (R6). The launcher's own working directory is the test package's, which is no jj
	// workspace.
	workspace string
}

// idleRigPodProcesses is one pod's launchers and what runs beside them (run): the worker stream
// the launchers dial, the context that stops them all and the wait that their goroutines have
// ended.
type idleRigPodProcesses struct {
	connect string
	ctx     context.Context
	cancel  context.CancelFunc
	done    sync.WaitGroup
	dirs    []string
}

// run runs f on a goroutine of the pod's: f's context ends when the pod is gone, and podGone
// waits for f to return before it removes the pod's directories. f may call run itself, since
// the goroutine it runs on is counted until it returns, so the wait cannot end in between.
func (processes *idleRigPodProcesses) run(f func(ctx context.Context)) {
	processes.done.Add(1)
	go func() {
		defer processes.done.Done()
		f(processes.ctx)
	}()
}

func newIdleRigPods(t *testing.T, launch func(pod, podUID string) (connect string, microVM, ok bool), session ompsessions.Database, dial func(ctx context.Context, signer sessionproxy.Signer) (sessionproxy.Dialer, error), gitHub func(pod, slot string) map[string][]byte) *idleRigPods {
	t.Helper()
	// Each launcher's private directory is a numbered subdirectory of this root, and the unix
	// socket of a generation's session database proxy is `<dir>/g<n>/.s.PGSQL.5432`. A unix
	// socket path is bounded at 107 bytes (docs/testing.md), so the root comes from testtmp.Dir,
	// told the longest path a launcher builds under it: a counter, the generation directory and
	// the session socket's name, written with the constants production uses. The helper measures
	// the root it made against that path and takes a shorter base where the machine's own
	// temporary directory leaves too little room. The root is made here, on the test's own
	// goroutine, because testtmp.Dir ends in t.Fatalf when neither base takes a directory, and
	// privateDir runs on the fake Kubernetes server's goroutine, where a t.Fatalf would stop
	// that goroutine alone and leave the test to run to its deadline.
	root := testtmp.Dir(t, filepath.Join("999", shimwire.GenerationDir(999), ompsessions.SocketName))
	located, err := workspace.Location(t.TempDir(), ghrepo.MustParse("acme/widgets"), "legion-208")
	if err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{filepath.Dir(located.Clone), filepath.Dir(located.Dir)} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	jj := fixturegit.Isolated(t, t.TempDir())
	jj.Run(filepath.Dir(located.Clone), fixturegit.JJInit(located.Clone, false)...)
	jj.Run(located.Clone, "jj", "workspace", "add", located.Dir, "--name", "legion-208", "-R", located.Clone)
	p := &idleRigPods{t: t, launch: launch, session: session, dial: dial, gitHub: gitHub,
		running: map[string]*idleRigPodProcesses{}, logs: &syncBuffer{}, started: map[string]int{},
		dirRoot: root, workspace: located.Dir}
	t.Cleanup(p.stopAll)
	return p
}

// podReady starts a launcher for each role of the pod, with that role's own launcher token and
// the stream and MicroVM launch answers for the pod; a pod launch refuses gets no launcher and
// counts no start. Before each launcher starts, record is handed its role, its private directory
// and the pod's processes, beside which record may run its own (idleRigPodProcesses.run). Beside
// each launcher runs the projection of its slot's gh files (projectGitHub).
func (p *idleRigPods) podReady(pod, podUID string, roles map[string]string, record func(role, dir string, processes *idleRigPodProcesses)) {
	connect, microVM, ok := p.launch(pod, podUID)
	if !ok {
		return
	}
	p.mu.Lock()
	if _, live := p.running[pod]; live {
		p.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	processes := &idleRigPodProcesses{connect: connect, ctx: ctx, cancel: cancel}
	p.running[pod] = processes
	p.started[pod]++
	p.mu.Unlock()
	for role, token := range roles {
		dir, err := p.privateDir()
		if err != nil {
			p.t.Errorf("make the private directory of %s/%s: %v", pod, role, err)
			continue
		}
		gh := dir + "-gh"
		if err := os.Mkdir(gh, 0o700); err != nil {
			p.t.Errorf("make the gh directory of %s/%s: %v", pod, role, err)
			continue
		}
		processes.dirs = append(processes.dirs, dir, gh)
		record(role, dir, processes)
		cfg := launcher.Config{
			Connect: connect, MicroVM: microVM, Token: token, Sandbox: pod, Role: role, PodUID: podUID, PrivateDir: dir,
			Stdout: p.logs, Stderr: p.logs, StopGrace: 10 * time.Second,
			SessionDatabase: p.session, SessionDialer: p.dial, Workspace: p.workspace, GHDir: gh,
		}
		processes.run(func(ctx context.Context) { p.projectGitHub(ctx, pod, role, gh) })
		processes.run(func(ctx context.Context) {
			if err := launcher.Run(ctx, cfg); err != nil && ctx.Err() == nil {
				fmt.Fprintf(p.logs, "launcher %s/%s: %v\n", pod, role, err)
			}
		})
	}
}

// idleRigGitHubSync is how often a launcher's projection of its slot's gh files reads the slot's
// Secret again: the kubelet's sync period, which is about a minute in a cluster and is the rig's
// tick here, so a start the daemon makes into a pod it keeps waits for its files a tick or two.
const idleRigGitHubSync = idleRigTick

// projectGitHub writes the slot's gh files into dir, the directory its launcher reads them in
// (launcher.Config.GHDir), as the kubelet projects the slot's Secret into the pod's gh directory:
// at once, and again each idleRigGitHubSync while the pod lives, so a file the daemon rewrites
// reaches the pod. A start into a pod the daemon keeps names the credential the slot's files
// must hold before its generation runs (runtime/sandbox's writeLaunchCredential), and the
// launcher waits for it (launcher's awaitGitHubCredential): without this projection that start
// waited out its whole bound, so a role whose process ended was never started again in its pod.
// Each file is written beside its place and renamed into it, so the launcher never reads half of
// one, as the kubelet swaps a projection whole.
func (p *idleRigPods) projectGitHub(ctx context.Context, pod, slot, dir string) {
	tick := time.NewTicker(idleRigGitHubSync)
	defer tick.Stop()
	written := map[string]string{}
	for {
		for name, data := range p.gitHub(pod, slot) {
			if written[name] == string(data) {
				continue
			}
			if err := idleRigProjectFile(filepath.Join(dir, filepath.FromSlash(name)), data); err != nil {
				fmt.Fprintf(p.logs, "project %s/%s's %s: %v\n", pod, slot, name, err)
				continue
			}
			written[name] = string(data)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// idleRigProjectFile writes data at path: beside it first, then renamed into place.
func idleRigProjectFile(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(path+".new", data, 0o600); err != nil {
		return err
	}
	return os.Rename(path+".new", path)
}

// privateDir is one launcher's private directory, where its generations' credentials and the unix
// socket of each generation's session database proxy live: a numbered subdirectory of the root
// newIdleRigPods made, whose budget that function explains.
//
// The root was `<os.TempDir()>/p<pid>` before, which two takers could share. dirRoot is a field
// of each idleRigPods value, but that path was the same for every value in one process, so two
// rigs in one run shared the root, not only two runs on one host; and a role process runs in a
// PID namespace of its own, where small process ids repeat, so even two hosts' runs collided.
// Takers that share a root delete each other's live sockets when one of them cleans up.
// os.MkdirTemp, which testtmp.Dir makes its root with, creates with O_EXCL, so no two takers
// can share a root or remove what another still serves.
func (p *idleRigPods) privateDir() (string, error) {
	p.dirMu.Lock()
	defer p.dirMu.Unlock()
	p.dirNext++
	dir := filepath.Join(p.dirRoot, strconv.Itoa(p.dirNext))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}

// podGone stops every launcher of the pod and waits for them, as the kubelet's stop of the
// container does: the pod is gone, so nothing of it may still answer the daemon.
func (p *idleRigPods) podGone(pod string) {
	p.mu.Lock()
	processes, live := p.running[pod]
	delete(p.running, pod)
	p.mu.Unlock()
	if !live {
		return
	}
	processes.cancel()
	processes.done.Wait()
	for _, dir := range processes.dirs {
		_ = os.RemoveAll(dir)
	}
}

func (p *idleRigPods) stopAll() {
	p.mu.Lock()
	pods := make([]string, 0, len(p.running))
	for pod := range p.running {
		pods = append(pods, pod)
	}
	p.mu.Unlock()
	for _, pod := range pods {
		p.podGone(pod)
	}
}

// starts is how many times the pod's launchers were started: one per pod the rig saw Ready.
func (p *idleRigPods) starts(pod string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.started[pod]
}

// log is every launcher's output so far.
func (p *idleRigPods) log() string { return p.logs.String() }

// idleRigDialer dials the rig's Postgres for the launcher's session proxy: the upstream a pod's
// Cloud SQL connector is in production, here a plain TCP connection to the test database's server.
type idleRigDialer struct{ address string }

func (d idleRigDialer) Dial(ctx context.Context, _ string) (net.Conn, error) {
	var dialer net.Dialer
	return dialer.DialContext(ctx, "tcp", d.address)
}

func (idleRigDialer) Close() error { return nil }

// idleRigWriteSessionRow records a session file in the rig's session table, as the agent's Oh My
// Pi does when it writes its session: the launcher refuses to resume a session the table does not
// hold (internal/launcher's sessionWritten), so every session the rig resumes has a row.
func idleRigSessionTable() string {
	return `create table if not exists omp_session_files (
		path text primary key,
		mtime_ms bigint not null,
		content bytea not null default ''
	)`
}

// idleRigSessionFile is the session file the rig's stand-in agent of role reports to the daemon,
// the path a resume names. It is the shape a pod's Oh My Pi writes under its own state directory.
func idleRigSessionFile(issue, role string) string {
	return "/legion/sessions/" + issue + "-" + role + ".jsonl"
}

// idleRigLegionTool writes, under dir, the `legion` the pod's launcher runs as the role process:
// a shell script that re-enters this test binary as TestIdleRigRoleProcess with the whole argv
// the launcher was told, in the shape of packages/daemon/internal/capabilities/image_test.go's
// fakeOmp. The role process the real launcher starts in a pod is `legion worker-shim … -- <omp
// argv>`; this stand-in is that whole process, Oh My Pi included: it speaks the worker stream's
// frames, registers with the daemon, reports ready, claims its role on the Envoy listener, and
// records every message it is given. The script names the rig that process serves, rigURL, in
// idleRigURLVariable itself, so the rig's own test process never sets it: each rig writes its own
// stand-in (toolsDir), and the rigs run beside each other, which a t.Setenv refuses.
func idleRigLegionTool(t *testing.T, dir, rigURL string) string {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "legion")
	script := "#!/bin/sh\n" + idleRigURLVariable + "=" + shellQuote(rigURL) + "\nexport " + idleRigURLVariable + "\n" +
		"exec " + shellQuote(self) + " -test.run='^TestIdleRigRoleProcess$' -test.timeout=0 -test.count=1 \"$@\"\n"
	if err := execfile.Write(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write the rig's legion stand-in: %v", err)
	}
	return path
}

// shellQuote is one argument as a POSIX shell reads it.
func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}

// idleRigAgentTool is the agent command the shim's argv ends with, which the stand-in records and
// never runs: a path that names the agent the pod would have started.
func idleRigAgentTool(t *testing.T, dir string) []string {
	t.Helper()
	path := filepath.Join(dir, "omp")
	if err := execfile.Write(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write the rig's omp stand-in: %v", err)
	}
	return []string{path}
}
