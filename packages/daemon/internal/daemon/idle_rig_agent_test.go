package daemon

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/procgroup"
	workershim "github.com/sjawhar/legion/daemon/internal/shim"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// idleRigURLVariable names the rig's own service in the environment of every process the rig's
// launchers start. It is the one variable a role process needs that the pod's own environment
// does not carry: everything else it reads — LEGION_ISSUE, LEGION_ROLE, LEGION_GENERATION,
// LEGION_DAEMON_URL, ENVOY_URL and ENVOY_NATS_URL — is the environment the Sandbox runtime
// builds for a role (internal/runtime/sandbox's mainEnvironment).
const idleRigURLVariable = "LEGION_IDLE_RIG_URL"

// idleRigRole is one role process the rig has seen: the claim it registered, the session it
// became, every message it was given — the daemon's prompts over the worker stream, and the
// messages the session's own consumer was relayed — and every adopt-working-copy frame the daemon
// sent it ahead of a prompt.
type idleRigRole struct {
	Claim       string
	Secret      string
	Session     string
	SessionFile string
	Registered  int
	// RegisteredAt is when the process last registered: a message recorded after it reached the
	// process that registration started.
	RegisteredAt time.Time
	Prompts      []idleRigMessage
	Messages     []idleRigMessage
	Adoptions    []idleRigAdoption
}

// idleRigMessage is one message a role process recorded, with the moment it arrived. Delivery is
// the delivery a prompt carried (shimwire.Prompt.DeliveryID), the id the daemon's own log names
// the task by; a message the session's consumer relayed carries none.
type idleRigMessage struct {
	ID       string
	Delivery string
	Text     string
	At       time.Time
}

// idleRigAdoption is one adopt-working-copy frame a role process was sent, recorded before the
// process answered it: the frame's id, the jj identity it named, and Ahead, how many tasks the
// role had been given when it came. The daemon sends the frame ahead of the task it adopts the
// working copy for and sends that task only once the frame is answered (supervise's startSend),
// so Ahead is the index, in the role's prompts, of the task the frame came before: a count of the
// frames ahead of a task is read from the rig's own order of arrival rather than from two clocks.
type idleRigAdoption struct {
	ID       string
	Identity string
	Ahead    int
}

// idleRigPlan is what a role process does with the next prompt it is given: take a turn on it, or
// acknowledge it and take none, which is the shape of a resume that never reaches its first turn.
// Topics are the topics the process registers with the Envoy listener beside its agent subject,
// as `envoy_subscribe` adds one.
//
// Background is the background work the session's host runs, by the kind its activity report
// names it with — `subagent` for a running `task` subagent, `async-job` for any other job the
// host's manager runs (asyncJobKinds, packages/pi-shared/src/session-activity.ts) — and none
// while nothing runs. The process reports it as the Oh My Pi extension does
// (packages/pi-legion/extensions/legion.ts): at every turn's end whatever it says and twice more
// after every turn it takes (idleRigRoleProcess.prompted), and between turns whenever it changes
// (idleRigRoleProcess.watchActivity), so a test that changes it mid-run is a subagent starting or
// ending in a live session.
//
// ExitsAsReleasedInProduction is a process that ends as a released Oh My Pi does in production:
// it registers again every idleRigRegistrationRefresh while it runs and deletes nothing when it
// stops (idleRigRoleProcess.run).
//
// HoldsBeforeEnvoy is a process that registers with the daemon and then waits, before it
// registers with the Envoy listener and claims its role, until the rig sets it a plan without
// HoldsBeforeEnvoy: a woken Oh My Pi still starting, whose session holds no role yet while the
// daemon publishes to it. The stand-in otherwise claims its role within a second of its pod's
// start, far sooner than an Oh My Pi does, and a test that needs the daemon's publish to meet a
// role its woken session has not claimed yet would be racing it.
//
// The rest is what the process answers a project release (dispatch://INFRA-3583). Unpushed is
// what its shim reads of the workspace's unpushed work (`read-unpushed`) when the plan scripts the
// read; with none, a process whose pod has a workspace (idleRigWorkspaces) reads that workspace as
// the shim does (shim.ReadUnpushed), and one whose pod has none reads nothing unpushed. RefuseRead,
// when set, is the refusal it answers that read with instead, as a shim older than the read
// forwards the frame to Oh My Pi, which refuses it. AnswerRelease is whether the process answers
// the release's ask with ready_for_update, naming the round the ask names, and PushOnRelease
// whether it first pushes its work as the ask tells it to: the working copy's change committed,
// the issue's branch set to that commit and pushed. StayInTurn has the next prompt's turn begin and
// last while the plan says so, as a role still working when the release's bound comes: a plan set
// without it ends that turn within a second (idleRigRoleProcess.watchActivity), as a long turn
// ends on its own. WithholdRole has the process leave its claim's role topic untaken while the
// plan says so, as a session that said ready holds none until its first turn or the first-turn
// bound (supervise.SaidReady): a message published to the topic meanwhile finds no holder, and the
// process takes the topic at the first reading of its plan that no longer withholds it, within a
// second (idleRigRoleProcess.watchActivity). The listener keeps a role claim once taken, and a
// resumed process registers under the session id it had, so a test withholds the topic on the
// seed's plan (idleRigSeed.plan), which the first process of the role reads as it starts.
//
// A process reads Topics and ExitsAsReleasedInProduction once, as it starts
// (idleRigRoleProcess.run), after the plan no longer holds it, and TakeTurn and Background at
// each task and each activity report. So a plan that sets either of the first two, or
// HoldsBeforeEnvoy, reaches only a process that starts after it is set. The daemon's boot starts
// the role process of every pod of the rig, so a plan for that process is given to
// newIdleWakeRig, or to newIdleRigWith on its seed (idleRigSeed.plan), each of which sets it
// before the boot; a plan for a woken process is set before its wake.
type idleRigPlan struct {
	TakeTurn                    bool                 `json:"takeTurn"`
	Topics                      []string             `json:"topics"`
	Background                  []string             `json:"background"`
	ExitsAsReleasedInProduction bool                 `json:"exitsAsReleasedInProduction"`
	HoldsBeforeEnvoy            bool                 `json:"holdsBeforeEnvoy"`
	Unpushed                    *idleRigReadUnpushed `json:"unpushed,omitempty"`
	RefuseRead                  string               `json:"refuseRead,omitempty"`
	AnswerRelease               bool                 `json:"answerRelease,omitempty"`
	PushOnRelease               bool                 `json:"pushOnRelease,omitempty"`
	StayInTurn                  bool                 `json:"stayInTurn,omitempty"`
	WithholdRole                bool                 `json:"withholdRole,omitempty"`
}

// idleRigReadUnpushed is a workspace's unpushed work as a shim's read of it for a project release
// answers it (shimwire.ReadUnpushedResult).
type idleRigReadUnpushed struct {
	Commits            int  `json:"commits"`
	WorkingCopyChanged bool `json:"workingCopyChanged"`
}

// idleRigWorkspaceAnswer is the workspace of a pod as the rig hands it to the pod's role
// processes (idleRigAgents.workspace): its directory, which LEGION_WORKSPACE names in a pod, the
// issue whose branch it is on, and the environment its git and jj run under.
type idleRigWorkspaceAnswer struct {
	Dir   string   `json:"dir"`
	Issue string   `json:"issue"`
	Env   []string `json:"env"`
}

// idleRigReleaseAsk finds the round a project release's ask names in its text
// (supervise.ReleaseTask): the round the role passes back with ready_for_update. The listener
// relays a publish's payload as the JSON the daemon encoded it as, so the quotes around the round
// arrive escaped.
var idleRigReleaseAsk = regexp.MustCompile(`round: \\?"(release:[0-9]+)\\?"`)

// idleRigAgents is the rig's own service: it hands each role process the boot token its pod's
// launcher wrote for the generation, records what each process registered and every message it
// was given, and answers what the next prompt should do. A role process reaches it at
// idleRigURLVariable.
//
// The boot token is handed over rather than read from the path the launch names, because the path
// a launch names is the pod's (`/var/run/legion/private/g<n>/LEGION_BOOT_TOKEN`) and this rig's
// launchers write their generations under a private directory of their own. The rig knows both.
type idleRigAgents struct {
	t    *testing.T
	url  string
	pool *store.Pool

	mu sync.Mutex
	// private is each role's launcher private directory, by "<pod>/<role>".
	private map[string]string
	roles   map[string]*idleRigRole
	plans   map[string]idleRigPlan
	// plan is the plan a role with none of its own gets.
	plan idleRigPlan
	// workspace is the workspace of a running pod by pod name, which the rig sets where its pods
	// have one (idleRigWorkspaces); nil answers none.
	workspace func(pod string) (idleRigWorkspaceAnswer, bool)
}

func newIdleRigAgents(t *testing.T, pool *store.Pool) *idleRigAgents {
	t.Helper()
	a := &idleRigAgents{t: t, pool: pool, private: map[string]string{}, roles: map[string]*idleRigRole{},
		plans: map[string]idleRigPlan{}, plan: idleRigPlan{TakeTurn: true}}
	server := httptest.NewServer(a)
	t.Cleanup(server.Close)
	a.url = server.URL
	return a
}

func idleRigKey(pod, role string) string { return pod + "/" + role }

// setPrivate records where a pod's role launcher writes its generations, so the rig can read that
// generation's boot token for the role process.
func (a *idleRigAgents) setPrivate(pod, role, dir string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.private[idleRigKey(pod, role)] = dir
}

// setPlan is what the role's process does from here, in the parts of the plan it has still to read
// (idleRigPlan).
func (a *idleRigAgents) setPlan(pod, role string, plan idleRigPlan) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.plans[idleRigKey(pod, role)] = plan
}

// planOf is the plan the role's process reads now (servePlan): its own, or the rig's where the
// role has none.
func (a *idleRigAgents) planOf(pod, role string) idleRigPlan {
	a.mu.Lock()
	defer a.mu.Unlock()
	if plan, ok := a.plans[idleRigKey(pod, role)]; ok {
		return plan
	}
	return a.plan
}

// setWorkspaces is how the service finds the workspace of a running pod for the pod's role
// processes.
func (a *idleRigAgents) setWorkspaces(lookup func(pod string) (idleRigWorkspaceAnswer, bool)) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.workspace = lookup
}

// role is what the rig has seen of a role process, empty when it has seen none.
func (a *idleRigAgents) role(pod, role string) idleRigRole {
	a.mu.Lock()
	defer a.mu.Unlock()
	seen, ok := a.roles[idleRigKey(pod, role)]
	if !ok {
		return idleRigRole{}
	}
	copied := *seen
	copied.Prompts = append([]idleRigMessage(nil), seen.Prompts...)
	copied.Messages = append([]idleRigMessage(nil), seen.Messages...)
	copied.Adoptions = append([]idleRigAdoption(nil), seen.Adoptions...)
	return copied
}

// prompts is every task the daemon gave the role over the worker stream.
func (a *idleRigAgents) prompts(pod, role string) []idleRigMessage { return a.role(pod, role).Prompts }

// messages is every message the role's session was given, relayed from its own consumer.
func (a *idleRigAgents) messages(pod, role string) []idleRigMessage {
	return a.role(pod, role).Messages
}

// adoptions is every adopt-working-copy frame the daemon sent the role over the worker stream.
func (a *idleRigAgents) adoptions(pod, role string) []idleRigAdoption {
	return a.role(pod, role).Adoptions
}

// counting is how many of the role's prompts and pushed messages hold want.
func idleRigCount(messages []idleRigMessage, want string) int {
	count := 0
	for _, message := range messages {
		if strings.Contains(message.Text, want) {
			count++
		}
	}
	return count
}

func (a *idleRigAgents) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/agent/boot-token":
		a.bootToken(w, r)
	case "/agent/registered":
		a.registered(w, r)
	case "/agent/record":
		a.record(w, r)
	case "/agent/plan":
		a.servePlan(w, r)
	case "/agent/workspace":
		a.serveWorkspace(w, r)
	default:
		http.Error(w, "unexpected "+r.URL.Path, http.StatusNotFound)
	}
}

// serveWorkspace answers the workspace of the pod the query names, 404 when it has none, as a pod
// whose environment names no LEGION_WORKSPACE has nothing for a shim to read.
func (a *idleRigAgents) serveWorkspace(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	lookup := a.workspace
	a.mu.Unlock()
	if lookup == nil {
		http.Error(w, "the rig's pods have no workspace", http.StatusNotFound)
		return
	}
	answer, ok := lookup(r.URL.Query().Get("pod"))
	if !ok {
		http.Error(w, "the pod "+r.URL.Query().Get("pod")+" runs no workspace", http.StatusNotFound)
		return
	}
	writeIdleRigJSON(w, answer)
}

func (a *idleRigAgents) bootToken(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Pod, Role  string
		Generation uint64
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	a.mu.Lock()
	dir := a.private[idleRigKey(request.Pod, request.Role)]
	a.mu.Unlock()
	if dir == "" {
		http.Error(w, "no launcher private directory for "+idleRigKey(request.Pod, request.Role), http.StatusNotFound)
		return
	}
	path := filepath.Join(dir, shimwire.GenerationDir(request.Generation), "LEGION_BOOT_TOKEN")
	token, err := os.ReadFile(path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	writeIdleRigJSON(w, map[string]string{"token": strings.TrimSpace(string(token))})
}

// registered records a role process's registration and writes the session row its next resume is
// looked up by, as the agent's own Oh My Pi writes its session into the session table.
func (a *idleRigAgents) registered(w http.ResponseWriter, r *http.Request) {
	var request struct{ Pod, Role, Claim, Secret, Session, SessionFile string }
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if _, err := a.pool.Exec(r.Context(),
		`insert into omp_session_files (path, mtime_ms) values ($1, $2)
		 on conflict (path) do update set mtime_ms = excluded.mtime_ms`,
		request.SessionFile, time.Now().UnixMilli()); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	a.mu.Lock()
	key := idleRigKey(request.Pod, request.Role)
	seen, ok := a.roles[key]
	if !ok {
		seen = &idleRigRole{}
		a.roles[key] = seen
	}
	seen.Claim, seen.Secret, seen.Session, seen.SessionFile = request.Claim, request.Secret, request.Session, request.SessionFile
	seen.Registered++
	seen.RegisteredAt = time.Now().UTC()
	a.mu.Unlock()
	writeIdleRigJSON(w, map[string]string{})
}

// record files what a role process was given under the kind it names. An adopt-working-copy frame
// is filed apart from both lists of what the role was told: it is no message, and a frame filed
// under Messages would read as one the session was given, which is the list
// ARoleMessageThatKeepsNothingStillWakesTheRole reads for a woken session that was given nothing.
func (a *idleRigAgents) record(w http.ResponseWriter, r *http.Request) {
	var request struct{ Pod, Role, Kind, ID, Delivery, Text, Identity string }
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	a.mu.Lock()
	key := idleRigKey(request.Pod, request.Role)
	seen, ok := a.roles[key]
	if !ok {
		seen = &idleRigRole{}
		a.roles[key] = seen
	}
	message := idleRigMessage{ID: request.ID, Delivery: request.Delivery, Text: request.Text, At: time.Now().UTC()}
	switch request.Kind {
	case "prompt":
		seen.Prompts = append(seen.Prompts, message)
	case "adopt":
		seen.Adoptions = append(seen.Adoptions, idleRigAdoption{ID: request.ID, Identity: request.Identity, Ahead: len(seen.Prompts)})
	default:
		seen.Messages = append(seen.Messages, message)
	}
	a.mu.Unlock()
	writeIdleRigJSON(w, map[string]string{})
}

func (a *idleRigAgents) servePlan(w http.ResponseWriter, r *http.Request) {
	writeIdleRigJSON(w, a.planOf(r.URL.Query().Get("pod"), r.URL.Query().Get("role")))
}

func writeIdleRigJSON(w http.ResponseWriter, body any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}

// TestIdleRigRoleProcess is the role process of a pod of TestIdleReleaseAndWake: the whole of
// `legion worker-shim … -- <omp argv>` as a pod runs it, with Oh My Pi's own part scripted. The
// pod's real launcher starts it, through the `legion` the runtime was configured with
// (idleRigLegionTool), with the argv and environment a role is given.
//
// It is the stand-in of packages/daemon/internal/capabilities/image_test.go's fakeOmp grown the
// three things the rig's claims need: it registers with the daemon and reports ready over the
// worker stream the shim speaks (packages/daemon/internal/daemon/daemon_test.go's shim), claims
// its role on the Envoy listener and takes the session's messages from the session's own
// consumer over NATS, and records every message it is given — the daemon's prompts and the
// consumer's frames alike — with the rig, and every adopt-working-copy frame the daemon sends it
// as a thing apart, before it answers it.
//
// It also does what the shim does on its own: it redials the worker stream when the daemon goes,
// with the same hello (the shim's run loop), so a restarted daemon adopts it; and it answers a
// project release's read of its pod's workspace with the shim's own read of that workspace
// (shim.ReadUnpushed), which the rig provisions and names to it (idleRigAgents.workspace), since
// LEGION_WORKSPACE names the pod's path and not the rig's.
//
// Run by anything but that launcher it does nothing: with no rig service named in its
// environment there is no rig to serve.
func TestIdleRigRoleProcess(t *testing.T) {
	// Not parallel: it is the idle rig role process and exits the test binary.
	rig := os.Getenv(idleRigURLVariable)
	if rig == "" {
		return
	}
	process, err := newIdleRigRoleProcess(rig)
	if err != nil {
		fmt.Fprintln(os.Stderr, "idle rig role process:", err)
		os.Exit(1)
	}
	if err := process.run(); err != nil {
		fmt.Fprintln(os.Stderr, "idle rig role process:", err)
		os.Exit(1)
	}
	os.Exit(0)
}

// The subjects, headers and replies of a session's own delivery, which the role process reads
// and answers as a plugin in consumer mode does. They are packages/envoy's: bus.DeliverSubject
// and contracts.LegacyAgentSubject build the two subjects, cmd/listener/session_relay.go names the
// frame header and the replies, internal/bus/stream.go the probe header
// (bus.ConnectionProbeHeader), and cmd/listener/session_delivery.go the mode the registration
// answers. The rig writes them out because they are another module's, and an Envoy that renamed
// one would be a contract change this rig has to be told about.
const (
	// idleRigDeliverSubjectPrefix and the session id are the session's relay subject, where the
	// listener that owns the session publishes each message of its durable consumer.
	idleRigDeliverSubjectPrefix = "notifications.envoy.deliver."
	// idleRigAgentSubjectPrefix and the session id are the session's agent subject, the subject
	// the stream stores every message to the session on.
	idleRigAgentSubjectPrefix = "notifications.agent."
	// idleRigStreamSeqHeader is a relay frame's stream sequence, in decimal.
	idleRigStreamSeqHeader = "Envoy-Stream-Seq"
	// idleRigProbeHeader, "1", with no body, is the listener's connection probe on the relay
	// subject (bus.ConnectionProbeHeader): the send route asks with it whether the session still
	// subscribes the subject. It is no frame, and is answered done before anything is decoded.
	idleRigProbeHeader = "Envoy-Probe"
	// idleRigRelayReceived is the answer that the frame decoded, before any work of the
	// session's; idleRigRelayDone is the answer that it was written, which acknowledges the
	// message to the consumer.
	idleRigRelayReceived = "received"
	idleRigRelayDone     = "done"
	// idleRigDeliveryConsumer is the delivery mode a registration answers for a session whose
	// own consumer the listener holds and relays.
	idleRigDeliveryConsumer = "consumer"
)

// idleRigRegistrationRefresh is how often a process whose plan sets ExitsAsReleasedInProduction
// registers again. The plugin does so every 90 s (SESSION_HEARTBEAT_MS); five seconds put the
// last registration moments before the release's stop, so the stop's delete is weighed against a
// registration about as close to its moment as a release can leave one
// (notify.DeleteConditions.NotRegisteredAfter).
const idleRigRegistrationRefresh = 5 * time.Second

// idleRigEnvelope is what the rig reads of an Envoy envelope, the frame's data unchanged: the
// event it carries and the message itself.
type idleRigEnvelope struct {
	EventID        string `json:"event_id"`
	PayloadSummary string `json:"payload_summary"`
	Payload        string `json:"payload"`
}

// text is the message a sender sent: its payload, which the listener fills only for a message
// longer than the one-line summary it also carries (envoy's contracts.MessageEnvelope).
func (e idleRigEnvelope) text() string {
	if e.Payload != "" {
		return e.Payload
	}
	return e.PayloadSummary
}

// idleRigRoleProcess is one running role process: its worker stream connection, its claim, its
// session, and the NATS connection its session's own consumer is relayed to it on.
type idleRigRoleProcess struct {
	rig        string
	pod        string
	role       string
	generation uint64
	daemon     string
	envoy      string
	natsURLs   string
	connect    string
	tokenFile  string
	// helloToken is the hello's boot token, which every dial of this generation sends.
	helloToken string

	// writeMu orders the frames this process writes, and guards the connection, which a redial
	// replaces: a read of the workspace is answered off the loop that reads the daemon's frames,
	// as the shim answers it beside the turn.
	writeMu sync.Mutex
	conn    net.Conn
	lines   *bufio.Reader
	writer  *shimwire.Writer
	// inTurn is whether the agent is in a turn it has not ended (idleRigPlan.StayInTurn), which
	// is what get_state answers. turnMu orders that turn's beginning and its end (prompted,
	// endTurn), and turnPrompt is the prompt it runs on, "" while none runs.
	inTurn     atomic.Bool
	turnMu     sync.Mutex
	turnPrompt string

	claimToken claim.Token
	secret     string
	session    string
	// sessionFile is the session file the process reports, the one a later resume names.
	sessionFile string
	// epoch is this process's one NATS connection, as a plugin's registration names it: a new
	// one tells the listener to restart the session's relay at its consumer's ack floor.
	epoch          string
	bus            *nats.Conn
	client         *http.Client
	unregisterOnce sync.Once

	// recorded is every stream sequence this process has already recorded with the rig. The
	// relay is at-least-once — a frame whose reply it did not see within relayReceiptWait comes
	// again after it restarts at the ack floor — so a session keeps a ledger of what it has
	// written, as a plugin's does, and writes each stored message once. It is keyed on the
	// stream sequence and not on the event id, so a second stored copy of one event is still
	// recorded twice: that would be a delivery the pipeline made twice, which is what the test
	// counts.
	recordedMu sync.Mutex
	recorded   map[uint64]bool

	// reportMu orders this process's activity reports, which a turn's end and the watch between
	// turns both send. reported is the background work the last report sent named, and taken
	// whether the daemon took it: a report it did not take is sent again at the watch's next
	// reading whatever that reads, as the extension forgets a report that did not reach the
	// daemon (lastActivity in packages/pi-legion/extensions/legion.ts). roleHeld is whether the
	// process has taken its claim's role topic (idleRigPlan.WithholdRole), which the watch reads
	// and sets under reportMu too.
	reportMu sync.Mutex
	reported []string
	taken    bool
	roleHeld bool

	// refreshMu is held across each registration refresh (refreshRegistration), and stopped
	// says the process is ending, after which no refresh starts: the process ends with no
	// refresh in flight, so its last registration lands before the daemon dates the stop.
	refreshMu sync.Mutex
	stopped   bool
}

func newIdleRigRoleProcess(rig string) (*idleRigRoleProcess, error) {
	p := &idleRigRoleProcess{rig: rig, client: &http.Client{Timeout: 20 * time.Second}, recorded: map[uint64]bool{}}
	p.pod = os.Getenv("LEGION_SANDBOX")
	if p.pod == "" {
		p.pod = os.Getenv("LEGION_ISSUE")
	}
	if p.pod == "" && os.Getenv("LEGION_CONTROLLER") == "1" {
		p.pod = idleRigControllerPod
	}
	p.role = os.Getenv("LEGION_ROLE")
	p.daemon = os.Getenv("LEGION_DAEMON_URL")
	p.envoy = os.Getenv("ENVOY_URL")
	p.natsURLs = os.Getenv("ENVOY_NATS_URL")
	generation, err := strconv.ParseUint(os.Getenv("LEGION_GENERATION"), 10, 64)
	if err != nil {
		return nil, fmt.Errorf("LEGION_GENERATION %q: %w", os.Getenv("LEGION_GENERATION"), err)
	}
	p.generation = generation
	argv := os.Args
	for i, arg := range argv {
		if i+1 >= len(argv) {
			break
		}
		switch arg {
		case "--connect":
			p.connect = strings.TrimPrefix(argv[i+1], "tcp://")
		case "--boot-token-file":
			p.tokenFile = argv[i+1]
		}
	}
	switch {
	case p.pod == "" || p.role == "":
		return nil, fmt.Errorf("no pod (%q) or role (%q) in the environment", p.pod, p.role)
	case p.connect == "":
		return nil, fmt.Errorf("no --connect in %v", argv)
	case p.daemon == "" || p.envoy == "":
		return nil, fmt.Errorf("no daemon (%q) or Envoy (%q) URL in the environment", p.daemon, p.envoy)
	case p.natsURLs == "":
		return nil, fmt.Errorf("no ENVOY_NATS_URL in the environment: the session's messages come from its own consumer, over NATS")
	}
	// The session is the one the launch resumes, when its agent argv names one (`--resume=<file>`,
	// sandbox's agent argv), and a new one otherwise, named apart from every other: a launch that
	// started a fresh session where the claim had one registers another session id, as Oh My Pi
	// would. The subjects the session is reached on are built from it before anything is
	// registered, so nothing the listener relays can arrive before this process listens for it.
	for i, arg := range argv {
		switch {
		case strings.HasPrefix(arg, "--resume="):
			p.sessionFile = strings.TrimPrefix(arg, "--resume=")
		case arg == "--resume" && i+1 < len(argv):
			p.sessionFile = argv[i+1]
		}
	}
	if p.sessionFile != "" {
		p.session = strings.TrimSuffix(filepath.Base(p.sessionFile), ".jsonl")
	} else {
		p.session = fmt.Sprintf("ses-%s-%s-%d", p.pod, p.role, time.Now().UnixNano())
		p.sessionFile = idleRigSessionFile(p.session)
	}
	p.epoch = fmt.Sprintf("idle-rig-%d-%d", os.Getpid(), time.Now().UnixNano())
	return p, nil
}

// run connects, registers, waits while its plan holds it (idleRigPlan.HoldsBeforeEnvoy),
// registers with the Envoy listener, claims the role unless its plan withholds it
// (idleRigPlan.WithholdRole), reports ready, and then serves the daemon's frames until it is shut
// down, dialling the worker stream again whenever the daemon goes.
func (p *idleRigRoleProcess) run() error {
	bootToken, err := p.bootToken()
	if err != nil {
		return err
	}
	p.helloToken = bootToken
	if err := p.dial(); err != nil {
		return err
	}
	defer p.closeConn()
	if err := p.openDelivery(); err != nil {
		return err
	}
	defer p.bus.Close()
	if err := p.register(bootToken); err != nil {
		return err
	}
	plan, err := p.plan()
	if err != nil {
		return err
	}
	for plan.HoldsBeforeEnvoy {
		time.Sleep(idleRigPoll)
		if plan, err = p.plan(); err != nil {
			return err
		}
	}
	if err := p.subscribe(plan.Topics); err != nil {
		return err
	}
	if !plan.WithholdRole {
		if err := p.claimRole(); err != nil {
			return err
		}
		p.roleHeld = true
	}
	if err := p.ready(); err != nil {
		return err
	}
	if err := p.reportActivity(); err != nil {
		return err
	}
	go p.watchActivity()
	// Once the process ends, the listener holds what the daemon's clean stop of a role's session
	// leaves (notify's DeleteSession, DELETE /v1/sessions/<id>): the session's record gone and its
	// role claim and interest record still naming the session. A message published to the role
	// topic then finds a holder that is not live: the listener keeps it for the role's next live
	// holder (kept_reason holder_lapsed), or answers one that keeps nothing 404 holder_lapsed, and
	// reports the wake either way. A record left standing would have the message forwarded to a
	// session nothing is left of instead.
	//
	// The default stand-in leaves that state itself: it deletes its registration as it stops
	// (unregister), so its session is gone whether or not the daemon's stop reached the listener
	// first. Oh My Pi's plugin sends its own DELETE of the registration in its session_shutdown
	// handler, but at a production release that DELETE does not take effect. A process whose plan
	// sets ExitsAsReleasedInProduction ends that way: it registers again while it runs, as the
	// plugin's heartbeat does, and deletes nothing as it stops, so the daemon's clean stop is what
	// makes its session gone.
	end := p.unregister
	if plan.ExitsAsReleasedInProduction {
		go p.refreshRegistration(plan.Topics)
		end = p.stopRefreshing
	}
	defer end()
	stopping := make(chan os.Signal, 1)
	signal.Notify(stopping, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-stopping
		end()
		os.Exit(0)
	}()
	for {
		if shutdown, err := p.serve(); shutdown || err != nil {
			return err
		}
		if err := p.redial(); err != nil {
			return err
		}
	}
}

// idleRigRedialWait bounds how long a role process dials a daemon that went before it gives up:
// longer than any restart of the rig takes, and short of the test binary's own deadline. Only the
// launcher's stop ends a process sooner, which is how every process of a pod ends.
const idleRigRedialWait = 5 * time.Minute

// redial dials the worker stream again with the same hello once the daemon dropped it, as the
// shim's run loop does, every half second until a daemon acks it or idleRigRedialWait runs out.
func (p *idleRigRoleProcess) redial() error {
	p.closeConn()
	deadline := time.Now().Add(idleRigRedialWait)
	for {
		err := p.dial()
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the daemon did not take the worker stream back within %s: %w", idleRigRedialWait, err)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// closeConn closes the worker stream connection this process holds, if it holds one.
func (p *idleRigRoleProcess) closeConn() {
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	if p.conn != nil {
		_ = p.conn.Close()
	}
}

// write writes one frame to the daemon, ordered with every other this process writes.
func (p *idleRigRoleProcess) write(frame shimwire.Frame) error {
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	return p.writer.WriteFrame(frame)
}

// refreshRegistration registers the session again, under the same epoch, every
// idleRigRegistrationRefresh until the process stops, as the plugin's heartbeat does. A refresh
// the listener refuses is reported and the next one tried: one failed heartbeat stops nothing.
func (p *idleRigRoleProcess) refreshRegistration(topics []string) {
	ticker := time.NewTicker(idleRigRegistrationRefresh)
	defer ticker.Stop()
	for range ticker.C {
		p.refreshMu.Lock()
		if p.stopped {
			p.refreshMu.Unlock()
			return
		}
		if err := p.subscribe(topics); err != nil {
			fmt.Fprintf(os.Stderr, "idle rig role process: refresh the registration of %s: %v\n", p.session, err)
		}
		p.refreshMu.Unlock()
	}
}

// stopRefreshing waits for a refresh in flight to be answered and lets no other start.
func (p *idleRigRoleProcess) stopRefreshing() {
	p.refreshMu.Lock()
	defer p.refreshMu.Unlock()
	p.stopped = true
}

// unregister removes the session's registration with the listener, as the daemon's clean stop
// does (notify's DeleteSession), and leaves its role claim and its interest record standing: the
// claim still names this session, which is the claim a released pod's role publish finds lapsed.
func (p *idleRigRoleProcess) unregister() {
	p.unregisterOnce.Do(func() {
		_ = p.delete(p.envoy + "/v1/sessions/" + p.session)
	})
}

func (p *idleRigRoleProcess) delete(url string) error {
	request, err := http.NewRequestWithContext(context.Background(), http.MethodDelete, url, nil)
	if err != nil {
		return err
	}
	return p.do(request, nil)
}

func (p *idleRigRoleProcess) bootToken() (string, error) {
	var answer struct{ Token string }
	if err := p.post(p.rig+"/agent/boot-token",
		map[string]any{"pod": p.pod, "role": p.role, "generation": p.generation}, &answer); err != nil {
		return "", err
	}
	if answer.Token == "" {
		return "", fmt.Errorf("the rig handed no boot token for %s/%s generation %d (%s)", p.pod, p.role, p.generation, p.tokenFile)
	}
	return answer.Token, nil
}

// dial opens the worker stream and says this generation's hello, waiting for the daemon's ack.
// A frame before the ack is meant for nobody yet and is passed over, as the shim passes it over.
func (p *idleRigRoleProcess) dial() error {
	conn, err := net.DialTimeout("tcp", p.connect, 10*time.Second)
	if err != nil {
		return fmt.Errorf("dial the worker stream %s: %w", p.connect, err)
	}
	p.writeMu.Lock()
	p.conn, p.lines, p.writer = conn, bufio.NewReader(conn), shimwire.NewWriter(conn)
	p.writeMu.Unlock()
	if err := p.write(shimwire.Hello2{BootToken: p.helloToken}); err != nil {
		_ = conn.Close()
		return err
	}
	for {
		frame, err := p.next(30 * time.Second)
		if err != nil {
			_ = conn.Close()
			return err
		}
		if frame.FrameType() == shimwire.TypeHelloAck {
			return nil
		}
	}
}

// openDelivery opens the session's own NATS connection and takes the one subject a session is
// reached on, as a plugin in consumer mode does (packages/envoy/docs/architecture.md, "Runtime
// shape", its "Session consumers" item, and the
// Go stand-ins of packages/envoy/cmd/listener's session_relay_test.go and
// current_registration_test.go's captureLaneForwards, neither of which answers anything on the
// agent subject either): its relay subject, notifications.envoy.deliver.<session id>, where the
// listener that owns the session publishes each message of the session's durable consumer. Every
// message the session is given arrives there, and that is where the rig records it. A role-lane
// forward and an ad-hoc topic's forward are published into the stream on the session's agent
// subject, so each comes back as a frame of that consumer; the session does not subscribe to that
// subject at all and answers nothing on it.
//
// It is taken, and flushed to the server, before the session registers, so nothing the
// listener relays or forwards once it knows the session can arrive before this process listens.
func (p *idleRigRoleProcess) openDelivery() error {
	bus, err := nats.Connect(p.natsURLs, nats.Name("idle-rig-"+p.session), nats.MaxReconnects(-1))
	if err != nil {
		return fmt.Errorf("connect %s: %w", p.natsURLs, err)
	}
	p.bus = bus
	if _, err := bus.Subscribe(idleRigDeliverSubjectPrefix+p.session, p.relayed); err != nil {
		return fmt.Errorf("subscribe to the relay subject of %s: %w", p.session, err)
	}
	if err := bus.Flush(); err != nil {
		return fmt.Errorf("flush the subscriptions of %s: %w", p.session, err)
	}
	return nil
}

// relayed takes one frame of the session's own consumer: it answers `received` as soon as the
// frame decodes, records the message with the rig, and only then answers `done`, which is what
// acknowledges the message to the consumer. A frame whose stream sequence this process has
// already recorded — the relay sends again what it saw no reply to — is acknowledged and not
// recorded twice. The listener's connection probe is no frame: it is answered `done`, as a
// plugin answers it before it decodes anything, and recorded nowhere.
func (p *idleRigRoleProcess) relayed(msg *nats.Msg) {
	if msg.Header.Get(idleRigProbeHeader) == "1" {
		if msg.Reply != "" {
			p.reply(msg, idleRigRelayDone)
		}
		return
	}
	var item idleRigEnvelope
	if err := json.Unmarshal(msg.Data, &item); err != nil {
		fmt.Fprintf(os.Stderr, "idle rig role process: decode a frame for %s: %v\n", p.session, err)
		return
	}
	sequence, err := strconv.ParseUint(msg.Header.Get(idleRigStreamSeqHeader), 10, 64)
	if err != nil {
		fmt.Fprintf(os.Stderr, "idle rig role process: a frame for %s carries stream sequence %q: %v\n",
			p.session, msg.Header.Get(idleRigStreamSeqHeader), err)
		return
	}
	if msg.Reply == "" {
		// There is nothing to answer it on, so the consumer could never be told the session has
		// it: the frame is malformed, as a plugin reads one with no reply (envoy-client's
		// decodeRelayFrame).
		fmt.Fprintf(os.Stderr, "idle rig role process: a frame for %s carries no reply subject\n", p.session)
		return
	}
	p.reply(msg, idleRigRelayReceived)
	if p.firstSighting(sequence) {
		if err := p.post(p.rig+"/agent/record", map[string]any{
			"pod": p.pod, "role": p.role, "kind": "message", "id": item.EventID, "text": item.text(),
		}, nil); err != nil {
			// The message is not recorded, so it is not acknowledged either: the frame's ack
			// wait runs out and the consumer delivers it again.
			p.forget(sequence)
			fmt.Fprintf(os.Stderr, "idle rig role process: record a message for %s: %v\n", p.session, err)
			return
		}
		go p.answerRelease(item.text())
	}
	p.reply(msg, idleRigRelayDone)
}

// answerRelease answers a project release's ask with ready_for_update, naming its round, when the
// rig's plan says this role answers one, and first pushes the workspace's work to the issue's
// branch when the plan says it pushes, as the ask tells a role to; any other message, and a plan
// that does neither, answer nothing. It runs off the relay's own callback, as an agent's turn on
// the ask runs apart from the plugin that took the message. A refused answer and a push that
// fails are said on stderr, and a failed push answers nothing, as a role that could not push does
// not say it is ready.
func (p *idleRigRoleProcess) answerRelease(text string) {
	asked := idleRigReleaseAsk.FindStringSubmatch(text)
	if asked == nil {
		return
	}
	plan, err := p.plan()
	if err != nil || !plan.AnswerRelease {
		return
	}
	if plan.PushOnRelease {
		if err := p.push(); err != nil {
			fmt.Fprintf(os.Stderr, "idle rig role process: push the work the release's ask %s names: %v\n", asked[1], err)
			return
		}
	}
	if err := p.post(p.daemon+"/legion/v1/claims/ready-for-update", claim.ReadyForUpdateRequest{
		ClaimToken: p.claimToken, SessionID: p.session, Secret: p.secret, Generation: p.generation, Round: asked[1],
	}, nil); err != nil {
		fmt.Fprintf(os.Stderr, "idle rig role process: answer the release's ask %s: %v\n", asked[1], err)
	}
}

// workspace is the pod's workspace as the rig names it, and whether the pod has one. The process
// takes the PATH the workspace's environment names as its own, so the jj it starts, and the one
// shim.ReadUnpushed looks up, is the test's: the pod's environment names the pod's PATH.
func (p *idleRigRoleProcess) workspace() (idleRigWorkspaceAnswer, bool, error) {
	var answer idleRigWorkspaceAnswer
	request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, p.rig+"/agent/workspace?pod="+p.pod, nil)
	if err != nil {
		return answer, false, err
	}
	response, err := p.client.Do(request)
	if err != nil {
		return answer, false, err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		return answer, false, nil
	}
	if response.StatusCode != http.StatusOK {
		read, _ := io.ReadAll(io.LimitReader(response.Body, 1<<16))
		return answer, false, fmt.Errorf("GET /agent/workspace = %d %s", response.StatusCode, read)
	}
	if err := json.NewDecoder(response.Body).Decode(&answer); err != nil {
		return answer, false, err
	}
	for _, entry := range answer.Env {
		if path, ok := strings.CutPrefix(entry, "PATH="); ok {
			if err := os.Setenv("PATH", path); err != nil {
				return answer, false, err
			}
		}
	}
	return answer, true, nil
}

// push pushes the workspace's work to the issue's branch as a role does when the release asks:
// a change in the working copy is committed first, then the branch is set to the newest commit
// under the working copy and pushed. The clone's origin is the remote standing for GitHub
// (idleRigWorkspaces), so the push reaches it as a pod's push reaches GitHub.
func (p *idleRigRoleProcess) push() error {
	ws, ok, err := p.workspace()
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("the pod runs no workspace to push from")
	}
	jj := func(args ...string) (string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		cmd := exec.CommandContext(ctx, "jj", append([]string{"-R", ws.Dir}, args...)...)
		cmd.Dir, cmd.Env = ws.Dir, ws.Env
		out, err := cmd.CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("jj %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out)), nil
	}
	changed, err := jj("log", "--no-graph", "-r", "@ & ~empty()", "-T", "commit_id")
	if err != nil {
		return err
	}
	if changed != "" {
		if _, err := jj("commit", "-m", "The "+p.role+"'s work, pushed when the project release asked"); err != nil {
			return err
		}
	}
	branch := "legion/" + ws.Issue
	if _, err := jj("bookmark", "set", branch, "-r", "@-"); err != nil {
		return err
	}
	_, err = jj("git", "push", "--bookmark", branch)
	return err
}

// firstSighting reports whether this process has not recorded the stream sequence yet, taking it
// for recorded when it has not.
func (p *idleRigRoleProcess) firstSighting(sequence uint64) bool {
	p.recordedMu.Lock()
	defer p.recordedMu.Unlock()
	if p.recorded[sequence] {
		return false
	}
	p.recorded[sequence] = true
	return true
}

// forget gives a sequence back to the ledger, so a frame whose record did not reach the rig is
// recorded when the consumer delivers it again.
func (p *idleRigRoleProcess) forget(sequence uint64) {
	p.recordedMu.Lock()
	defer p.recordedMu.Unlock()
	delete(p.recorded, sequence)
}

func (p *idleRigRoleProcess) reply(msg *nats.Msg, word string) {
	if err := p.bus.Publish(msg.Reply, []byte(word)); err != nil {
		fmt.Fprintf(os.Stderr, "idle rig role process: reply %s to a frame for %s: %v\n", word, p.session, err)
		return
	}
	if err := p.bus.Flush(); err != nil {
		fmt.Fprintf(os.Stderr, "idle rig role process: flush the reply %s for %s: %v\n", word, p.session, err)
	}
}

func (p *idleRigRoleProcess) register(bootToken string) error {
	sessionFile := p.sessionFile
	var answer claim.RegisterResponse
	if err := p.post(p.daemon+"/legion/v1/claims/register", claim.RegisterRequest{
		BootToken: bootToken, SessionID: p.session, OmpSessionFile: sessionFile,
		AgentID: "idle-rig-agent", PluginContract: api.DaemonAPIVersion(),
	}, &answer); err != nil {
		return fmt.Errorf("register: %w", err)
	}
	p.claimToken, p.secret = answer.ClaimToken, answer.Secret
	return p.post(p.rig+"/agent/registered", map[string]any{
		"pod": p.pod, "role": p.role, "claim": string(p.claimToken), "secret": p.secret,
		"session": p.session, "sessionFile": sessionFile,
	}, nil)
}

func (p *idleRigRoleProcess) plan() (idleRigPlan, error) {
	var plan idleRigPlan
	err := p.get(p.rig+"/agent/plan?pod="+p.pod+"&role="+p.role, &plan)
	return plan, err
}

// subscribe registers the session with the Envoy listener, as a session's Envoy plugin does: no
// port — this listener delivers nothing to a session's port, and refuses a registration naming
// one — the machine it runs on, every topic it follows beside its own agent subject, and the
// delivery it asks for: its own durable consumer, relayed to the NATS connection openDelivery
// opened, under this process's epoch. The answer says which delivery the session got, and a
// registration this listener can give no consumer is refused with an HTTP status and carries no
// `delivery` at all, so an answer with none, and one whose mode is not the consumer's, are each
// this process's failure to start.
func (p *idleRigRoleProcess) subscribe(topics []string) error {
	if topics == nil {
		topics = []string{}
	}
	var answer struct {
		Delivery *struct {
			Mode    string `json:"mode"`
			Subject string `json:"subject"`
			Reason  string `json:"reason"`
		} `json:"delivery"`
	}
	if err := p.post(p.envoy+"/v1/interests/subscribe", map[string]any{
		"session_id": p.session, "dir": "/legion/workspaces/" + p.pod, "topics": topics,
		"host": "127.0.0.1", "title": p.role, "self_subscribed": true,
		"capabilities": []string{"prompt_async"}, "plugin": "idle-rig", "plugin_version": "0",
		"delivery": map[string]any{"consumer": true, "epoch": p.epoch},
	}, &answer); err != nil {
		return err
	}
	switch {
	case answer.Delivery == nil || answer.Delivery.Mode != idleRigDeliveryConsumer:
		return fmt.Errorf("the listener registered %s on delivery %+v, want its own consumer's", p.session, answer.Delivery)
	case answer.Delivery.Subject != idleRigDeliverSubjectPrefix+p.session:
		return fmt.Errorf("the listener relays %s on %q, not the %q this process reads",
			p.session, answer.Delivery.Subject, idleRigDeliverSubjectPrefix+p.session)
	}
	return nil
}

// claimRole takes the role topic of this claim, which is what makes a message published to it
// reach this session and what makes its absence the listener's wake report.
func (p *idleRigRoleProcess) claimRole() error {
	return p.post(p.envoy+"/v1/roles/set", map[string]any{
		"role": string(p.claimToken), "session_id": p.session,
	}, nil)
}

func (p *idleRigRoleProcess) ready() error {
	return p.post(p.daemon+"/legion/v1/claims/ready", claim.ReadyRequest{
		ClaimToken: p.claimToken, SessionID: p.session, Secret: p.secret, Generation: p.generation,
	}, nil)
}

// reportActivity is the session's report of what it is doing, sent whatever it says, as the
// extension's turn-end report is (its `session_stop` handler): once the process is ready, after
// every task it is given, and twice more after every turn it takes (prompted). It names the
// background work the rig's plan says is running (idleRigPlan.Background) and no reminder due.
// The idle rule reads it before it releases the pod (R1), and holds the pod while it names
// background work running.
func (p *idleRigRoleProcess) reportActivity() error {
	p.reportMu.Lock()
	defer p.reportMu.Unlock()
	plan, err := p.plan()
	if err != nil {
		return err
	}
	return p.report(plan.Background)
}

// idleRigActivityPeriod is how often a role process reads its background work between turns: the
// extension's managed interval, five seconds there (SESSION_ACTIVITY_PERIOD_MS), cut to one here
// so a subagent that ends is reported within a second rather than within the rig's waits.
const idleRigActivityPeriod = time.Second

// idleRigTurnReportDelay is how long after a turn's last frame a role process reports again; it
// reports a last time idleRigActivityPeriod after that frame (prompted).
const idleRigTurnReportDelay = 250 * time.Millisecond

// watchActivity is the extension's managed interval between turns: every idleRigActivityPeriod
// it reads the background work the rig's plan says is running and reports it when it differs
// from what the daemon last took, or when the daemon took nothing last time, and sends nothing
// otherwise. A reading it cannot make is skipped, as the next one comes a period later. Each
// reading is made under reportMu, as reportActivity's is, so the report the daemon holds last is
// always of the latest reading: none read before a change lands after one read since. A reading
// whose plan no longer stays in a turn ends the turn StayInTurn began (endTurn), and reports as
// every turn's end does. A reading whose plan no longer withholds the role topic takes it
// (claimRole) if the process holds it not yet, and a take that fails is made at the next reading.
func (p *idleRigRoleProcess) watchActivity() {
	ticker := time.NewTicker(idleRigActivityPeriod)
	defer ticker.Stop()
	for range ticker.C {
		p.reportMu.Lock()
		plan, planErr := p.plan()
		ended := false
		var endErr error
		if planErr == nil && !plan.StayInTurn {
			if ended, endErr = p.endTurn(); endErr != nil {
				fmt.Fprintf(os.Stderr, "idle rig role process: end the turn of %s: %v\n", p.session, endErr)
			}
		}
		if planErr == nil && endErr == nil && (ended || !p.taken || !slices.Equal(plan.Background, p.reported)) {
			if err := p.report(plan.Background); err != nil {
				fmt.Fprintf(os.Stderr, "idle rig role process: report the activity of %s: %v\n", p.session, err)
			}
		}
		if planErr == nil && !plan.WithholdRole && !p.roleHeld {
			if err := p.claimRole(); err != nil {
				fmt.Fprintf(os.Stderr, "idle rig role process: take the role topic of %s: %v\n", p.session, err)
			} else {
				p.roleHeld = true
			}
		}
		p.reportMu.Unlock()
	}
}

// endTurn ends the turn StayInTurn began, as a turn the agent ends on its own: the agent's end and
// the prompt's result, the frames a plan that takes a turn writes (prompted). It answers whether a
// turn ran to end.
func (p *idleRigRoleProcess) endTurn() (bool, error) {
	p.turnMu.Lock()
	defer p.turnMu.Unlock()
	if p.turnPrompt == "" {
		return false, nil
	}
	id := p.turnPrompt
	p.turnPrompt = ""
	p.inTurn.Store(false)
	for _, frame := range []shimwire.Frame{shimwire.AgentEnd{}, shimwire.PromptResult{ID: id, AgentInvoked: true}} {
		if err := p.write(frame); err != nil {
			return true, err
		}
	}
	return true, nil
}

// report posts one activity report through the daemon's own route, naming kinds as the
// background work running, and records whether the daemon took it. The caller holds reportMu.
func (p *idleRigRoleProcess) report(kinds []string) error {
	err := p.post(p.daemon+"/legion/v1/claims/activity", claim.ActivityRequest{
		ClaimToken: p.claimToken, SessionID: p.session, Secret: p.secret,
		Activity: claim.SessionActivity{At: time.Now().UTC(),
			BackgroundWork: claim.BackgroundWork{Running: len(kinds) > 0, Kinds: kinds}},
	}, nil)
	p.reported, p.taken = kinds, err == nil
	return err
}

// serve answers the daemon's frames until the connection ends, answering false, or a shutdown
// arrives, answering true. A read of the workspace is answered on a goroutine of its own, as the
// shim answers it beside the turn, so a slow jj holds back no other frame.
func (p *idleRigRoleProcess) serve() (shutdown bool, err error) {
	for {
		frame, err := p.next(0)
		if err != nil {
			return false, nil
		}
		switch f := frame.(type) {
		case shimwire.NegotiateProtocol:
			if err := p.write(shimwire.Response{ID: f.ID, Command: shimwire.TypeNegotiateProtocol, Success: true}); err != nil {
				return false, err
			}
		case shimwire.GetState:
			streaming := p.inTurn.Load()
			data, _ := json.Marshal(shimwire.StateData{IsStreaming: &streaming})
			if err := p.write(shimwire.Response{ID: f.ID, Command: shimwire.TypeGetState, Success: true, Data: data}); err != nil {
				return false, err
			}
		case shimwire.Prompt:
			if err := p.prompted(f); err != nil {
				return false, err
			}
		case shimwire.WorkspaceWork:
			// The release's check of the workspace for work origin does not hold. This workspace
			// holds none: its working copy is empty and every commit under it is on origin.
			if err := p.write(shimwire.WorkspaceWorkResult{
				ID: f.ID, OK: true, Head: "0000000000000000000000000000000000000000",
			}); err != nil {
				return false, err
			}
		case shimwire.ReadUnpushed:
			go func() {
				if err := p.readUnpushed(f); err != nil {
					fmt.Fprintf(os.Stderr, "idle rig role process: answer the read of %s's workspace: %v\n", p.pod, err)
				}
			}()
		case shimwire.AdoptWorkingCopy:
			if err := p.adopted(f); err != nil {
				return false, err
			}
		case shimwire.Shutdown:
			return true, nil
		}
	}
}

// readUnpushed answers a project release's read of the workspace's unpushed work: with the plan's
// refusal, as an older shim's; with the count and the working-copy flag the plan scripts; or,
// where the plan scripts none, with the shim's own read of the pod's workspace
// (shim.ReadUnpushed), which counts the issue's work as the frame's issue, merged flag and session
// names say, and nothing unpushed for a pod that has none. A read that fails is answered with its
// failure, as the shim answers it.
func (p *idleRigRoleProcess) readUnpushed(read shimwire.ReadUnpushed) error {
	plan, err := p.plan()
	if err != nil {
		return err
	}
	if plan.RefuseRead != "" {
		return p.write(shimwire.Response{ID: read.ID, Command: shimwire.TypeReadUnpushed, Success: false, Error: plan.RefuseRead})
	}
	answer := shimwire.ReadUnpushedResult{ID: read.ID, OK: true}
	if plan.Unpushed != nil {
		answer.Commits, answer.WorkingCopyChanged = plan.Unpushed.Commits, plan.Unpushed.WorkingCopyChanged
		return p.write(answer)
	}
	ws, ok, err := p.workspace()
	switch {
	case err != nil:
		answer.OK, answer.Error = false, err.Error()
	case ok:
		work, err := workershim.ReadUnpushed(context.Background(), procgroup.NoReaper, ws.Dir, ws.Env, read)
		if err != nil {
			answer.OK, answer.Error = false, err.Error()
		} else {
			answer.Commits, answer.WorkingCopyChanged = work.Commits, work.WorkingCopyChanged
		}
	}
	return p.write(answer)
}

// adopted records the adopt-working-copy frame and only then answers it, as a shim answers once
// its jj commands are done. The daemon sends the task the frame is for only once this answer is
// in (supervise's startSend), so every frame is on the rig's record before the task it came ahead
// of is. A record that does not reach the rig ends the process, as a prompt's does, rather than
// answer a frame the record lacks: a count of these frames must never miss one.
func (p *idleRigRoleProcess) adopted(adopt shimwire.AdoptWorkingCopy) error {
	if err := p.post(p.rig+"/agent/record", map[string]any{
		"pod": p.pod, "role": p.role, "kind": "adopt", "id": adopt.ID, "identity": adopt.JJUser + " <" + adopt.JJEmail + ">",
	}, nil); err != nil {
		return err
	}
	return p.write(shimwire.AdoptWorkingCopyResult{ID: adopt.ID, OK: true})
}

// prompted records the task and, when the rig's plan says so, takes a turn on it. A plan that
// takes no turn acknowledges the prompt and nothing else, which is a resume whose woken role
// never reaches its first turn; a plan that stays in its turn begins one, which lasts until the
// plan no longer says so (watchActivity, endTurn) or the claim is stopped.
func (p *idleRigRoleProcess) prompted(prompt shimwire.Prompt) error {
	if err := p.post(p.rig+"/agent/record", map[string]any{
		"pod": p.pod, "role": p.role, "kind": "prompt", "id": prompt.ID, "delivery": prompt.DeliveryID, "text": prompt.Message,
	}, nil); err != nil {
		return err
	}
	if err := p.write(shimwire.Response{ID: prompt.ID, Command: shimwire.TypePrompt, Success: true}); err != nil {
		return err
	}
	plan, err := p.plan()
	if err != nil {
		return err
	}
	if plan.StayInTurn {
		p.turnMu.Lock()
		defer p.turnMu.Unlock()
		p.turnPrompt = prompt.ID
		p.inTurn.Store(true)
		return p.write(shimwire.AgentStart{})
	}
	if plan.TakeTurn {
		for _, frame := range []shimwire.Frame{shimwire.AgentStart{}, shimwire.AgentEnd{}, shimwire.PromptResult{ID: prompt.ID, AgentInvoked: true}} {
			if err := p.write(frame); err != nil {
				return err
			}
		}
		// The extension reports at the end of a run, which in Oh My Pi comes well after the
		// daemon stamped the run's start (supervise.Claim.LastTurnStart). This turn ends the
		// moment it begins, so the report below can be read before that stamp, and it then says
		// nothing of the turn (claim.ReadSessionWork): an update round or a project release
		// would hold the pod for it to its bound. The report is sent twice more, as the
		// extension's own comes after the stamp: idleRigTurnReportDelay after the turn, which a
		// daemon that stamps the start promptly has read by then, and idleRigActivityPeriod after
		// it, for one that stamps it later, as a machine's handling of the turn's start and its
		// store write can be under the race detector. Each send reads the plan as it stands then.
		go func() {
			time.Sleep(idleRigTurnReportDelay)
			if err := p.reportActivity(); err != nil {
				fmt.Fprintf(os.Stderr, "idle rig role process: report the activity of %s after its turn: %v\n", p.session, err)
			}
			time.Sleep(idleRigActivityPeriod - idleRigTurnReportDelay)
			if err := p.reportActivity(); err != nil {
				fmt.Fprintf(os.Stderr, "idle rig role process: report the activity of %s a period after its turn: %v\n", p.session, err)
			}
		}()
	}
	// The report that follows every turn, and every task acknowledged without one: the idle rule
	// releases no pod whose session has said nothing since its last turn, and a task
	// acknowledged is a turn as far as that rule reads it (claim.ReadSessionWork).
	return p.reportActivity()
}

func (p *idleRigRoleProcess) next(within time.Duration) (shimwire.Frame, error) {
	deadline := time.Time{}
	if within > 0 {
		deadline = time.Now().Add(within)
	}
	if err := p.conn.SetReadDeadline(deadline); err != nil {
		return nil, err
	}
	line, err := p.lines.ReadBytes('\n')
	if err != nil {
		return nil, err
	}
	return shimwire.Decode(line)
}

func (p *idleRigRoleProcess) post(url string, body, answer any) error {
	encoded, err := json.Marshal(body)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(context.Background(), http.MethodPost, url, bytes.NewReader(encoded))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	return p.do(request, answer)
}

func (p *idleRigRoleProcess) get(url string, answer any) error {
	request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	return p.do(request, answer)
}

func (p *idleRigRoleProcess) do(request *http.Request, answer any) error {
	response, err := p.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	read, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return err
	}
	if response.StatusCode >= 300 {
		return fmt.Errorf("%s %s = %d %s", request.Method, request.URL.Path, response.StatusCode, read)
	}
	if answer == nil || len(bytes.TrimSpace(read)) == 0 {
		return nil
	}
	return json.Unmarshal(read, answer)
}
