//go:build e2e

// The parts of the rig (RIG.md) that check w5 of this change's verification needs and no other
// check does. w5_unreachable_live_test.go is the check itself; this file is what it runs on, over
// the shared rig (rig_live_test.go and its two siblings):
//
//   - the daemon pointed at the rig's scratch Dispatch (rigOptions.dispatch), on a project key
//     Dispatch accepts, with the project's default repository and the issues in place first, as
//     RIG.md says to point it;
//   - one scripted model stand-in per claim (w5Standin, scriptedStandin's shape with a rule that
//     holds a request and then answers it with a tool call), so each claim does what the check
//     names and the controller calls no `relaunch` of its own;
//   - pods (podRuntime): the rig's local runtime hands the daemon Sandbox locators, one pod per
//     issue and one for the controller, and translates them back for every call it makes of the
//     local runtime. The daemon's pod rule (a relaunch into a pod whose every claim is out of
//     reach) reads the Sandbox locator's pod, which a tmux locator does not carry;
//   - a front in front of the daemon's API that every pane reaches it through (daemonFront),
//     which drops one claim's activity reports from its launch on (the ninth claim's);
//   - a front in front of the rig's listener front that the daemon alone reaches Envoy through
//     (envoyListFront), which fails the daemon's reads of Envoy's session list when told to;
//   - freezing a pane's Oh My Pi process while its `wait` call is open (freeze, thaw): Oh My Pi's
//     `wait` returns within 5 minutes at most (its WAIT_LADDER_MS), so a process that sits in one
//     wait call for longer has stopped, as a production implementer did after its last wait;
//   - an audit of the outbox table (installOutboxAudit), a trigger that copies every row's insert,
//     update and delete with the moment it happened, so a step reads when a row was written and
//     when it was finished, which the outbox's own delete of a finished row leaves nowhere else;
//   - a sampler of the daemon's state and claims once a minute (sampler).
//
// Nothing here names a symbol the base commit of this change lacks: the check's negative control
// lays this file over the base's tree and runs the first block there. What only the head has — the
// runtime's three relaunch refusals — is in w5_inplace_live_test.go, which the control leaves out.
package daemon

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/stream"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
)

// w5ResultsEnv names the results file the entry script keeps outside the run's work directory, so
// a kill keeps every line written so far.
const w5ResultsEnv = "W5_RESULTS_FILE"

// The daemon's own words a step reads, as literals so the file builds over the base.
const (
	w5FleetAlertPrefix      = "LEGION FLEET ALERT: "
	w5NoticeUnreachable     = "unreachable-claims"
	w5NoticeRelaunchReady   = "relaunch-ready"
	w5NoticeGuardHeld       = "relaunch-guard-held"
	w5NoticeNotBack         = "relaunch-not-back"
	w5NoticeExit            = "unreachable-exit"
	w5NoticeNoLive          = "envoy-no-live-session"
	w5NoticeClaimOutOfReach = "claim-out-of-reach"
	w5CaseNoLive            = "envoy_no_live_session"
	w5CaseListUnreadable    = "envoy_session_list_unreadable"
	w5CaseControllerGone    = "controller_unreachable"
	w5CaseControllerRefused = "controller_relaunch_refused"
)

// w5Claim is one claim the check seeds: its issue, where that issue stands in its tree, the role
// and phase, the provider its stand-in serves under, the task its start leaves pending ("" for a
// claim that owes none), and, once seeded, its token and stand-in.
type w5Claim struct {
	name     string
	key      string
	tree     string
	parent   string
	phase    phase.Phase
	role     claim.Role
	provider string
	task     string
	token    claim.Token
	stand    *w5Standin
}

// w5Rig is one rig of w5: the shared rig with what this file adds.
type w5Rig struct {
	*rig
	pods     *podRuntime
	api      *daemonFront
	envoy    *envoyListFront
	results  *w5Results
	claims   map[string]*w5Claim
	ordered  []*w5Claim
	ctl      *w5Claim
	project  string
	sampled  *sampler
	grantsMu sync.Mutex
	// frozenMu guards frozen: the Oh My Pi pid the rig stopped of each claim (freezeInWait,
	// stopProcess), which every results line names while that process stays stopped.
	frozenMu sync.Mutex
	frozen   map[claim.Token]int
}

// w5Options are what a run of w5 asks of its rig: its script, and its claims by name in the order
// their issues are made (the tree's root first, its child after it).
type w5Options struct {
	script string
	claims []w5Claim
}

// newW5Rig starts the rig, points the daemon at the scratch Dispatch, gives each claim a stand-in
// and the daemon pods, and seeds the claims. The daemon is booted by the caller (boot).
func newW5Rig(t *testing.T, opts w5Options) *w5Rig {
	t.Helper()
	r := startRig(t, rigOptions{script: opts.script, standin: true, dispatch: true})
	w := &w5Rig{rig: r, claims: map[string]*w5Claim{}, frozen: map[claim.Token]int{}}
	w.results = openW5Results(t, r.evidence)
	w.results.prefix = w.frozenLabel
	w.switchProject()
	w.pointAtScratchDispatch(opts.claims)
	for i := range opts.claims {
		c := opts.claims[i]
		w.claims[c.name] = &c
		w.ordered = append(w.ordered, &c)
	}
	w.ctl = &w5Claim{name: "controller", role: claim.RoleController, provider: "w5ctl", token: claim.ControllerToken(r.project)}
	w.claims[w.ctl.name] = w.ctl
	w.writeStandins()
	w.api = newDaemonFront(t, r.cfg.DaemonURL)
	r.rt.daemonURL = w.api.url
	w.envoy = newEnvoyListFront(t, r.front.url)
	r.cfg.EnvoyURL = w.envoy.url
	r.cfg.ControllerLaunch = config.ControllerLaunchDaemon
	// testConfig is a fixture, not config.Load, and leaves review_round_cap at zero; the
	// controller's plugin refuses a state whose settings carry it so (its schema holds it > 0) and
	// exits at its claim boot. config.Load's default is 3 (config.go).
	r.cfg.ReviewRoundCap = 3
	// Every issue seeded holds a slot of its own, a tree's child included (admit), and admission
	// reads the cap over the slots it repairs, so the cap holds every one and one more.
	issues := map[string]bool{}
	for _, c := range w.ordered {
		issues[c.key] = true
	}
	r.cfg.AdmissionCap = len(issues) + 1
	w.pods = newPodRuntime(r.runtime, w.podOf)
	w.installOutboxAudit()
	// Oh My Pi writes its own and its plugins' logs under the run's Oh My Pi home, which goes with
	// the test: they are copied beside the panes' logs first, since a pane's own log holds only its
	// shim's lines.
	r.root.Cleanup(func() { copyLogs(w.home, filepath.Join(w.evidence, "omp-logs")) })
	w.seed()
	w.record("rig: project %s, daemon on 127.0.0.1:%d (panes reach it through %s), Envoy listener %s (the daemon reaches it through %s), scratch Dispatch %s, %d claims seeded and the controller under controller: daemon",
		r.cfg.Project, r.cfg.Port, w.api.url, r.listener.BaseURL(), w.envoy.url, r.scratch.base, len(w.ordered))
	return w
}

// switchProject gives the run a project key Dispatch accepts (contracts.ProjectKeyPattern,
// ^[A-Z][A-Z0-9]{1,9}$): the rig's own is TEST and twelve hex digits, which Dispatch refuses.
func (w *w5Rig) switchProject() {
	t := w.t
	key := "W5" + randomSuffix(t)[:6]
	w.cfg.Project = key
	w.cfg.Projects = map[string]config.Project{key: {}}
	project, err := claim.ProjectToken(key)
	if err != nil {
		t.Fatal(err)
	}
	w.rig.project = project
	w.project = key
}

// ---- the scratch Dispatch ------------------------------------------------------------------------

// pointAtScratchDispatch makes the project and every issue in the scratch Dispatch, each labelled
// for Legion and in progress, sets liveRepo as the project's default repository, which Dispatch
// resolves each issue to (the daemon reads no repository mapping, only that resolution), and
// points the daemon at it.
func (w *w5Rig) pointAtScratchDispatch(claims []w5Claim) {
	t := w.t
	base := w.scratch.base
	w.dispatchCall(http.MethodPost, "/api/v1/projects", map[string]any{"key": w.project, "name": "w5 " + w.project}, http.StatusCreated, nil)
	w.dispatchCall(http.MethodPut, "/api/v1/settings/repo-projects/"+liveRepo, map[string]any{"project": w.project, "default": true}, 0, nil)
	keys := map[string]string{}
	for i := range claims {
		c := &claims[i]
		if existing, ok := keys[c.key]; ok {
			c.key = existing
			continue
		}
		label := c.key
		body := map[string]any{"project": w.project, "title": "w5 issue " + label, "labels": []string{"legion"}}
		if c.parent != "" {
			parent := keys[c.parent]
			body["parent"] = parent
		}
		var created struct {
			Key string `json:"key"`
		}
		w.dispatchCall(http.MethodPost, "/api/v1/issues", body, http.StatusCreated, &created)
		if created.Key == "" {
			t.Fatalf("scratch Dispatch made issue %s with no key", label)
		}
		w.dispatchCall(http.MethodPatch, "/api/v1/issues/"+created.Key, map[string]any{"status": "in_progress"}, 0, nil)
		keys[label] = created.Key
		c.key = created.Key
	}
	for i := range claims {
		c := &claims[i]
		if k, ok := keys[c.tree]; ok {
			c.tree = k
		}
		if k, ok := keys[c.parent]; ok {
			c.parent = k
		}
	}
	tokenFile := filepath.Join(t.TempDir(), "scratch-dispatch-token")
	writeProofFile(t, tokenFile, w.scratch.token+"\n")
	w.cfg.DispatchURL = base
	w.cfg.DispatchTokenFile = tokenFile
	w.note("the daemon reads the scratch Dispatch %s for project %s, issues %v, each labelled legion and in progress", base, w.project, keys)
}

// dispatchCall is one request of the scratch Dispatch as a person (its trusted identity header),
// failing the run on any status but want (any 2xx for want 0).
func (w *w5Rig) dispatchCall(method, path string, body any, want int, into any) {
	t := w.t
	encoded, _ := json.Marshal(body)
	req, err := http.NewRequest(method, w.scratch.base+path, bytes.NewReader(encoded))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Dispatch-User", "w5-rig")
	response, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("scratch Dispatch %s %s: %v", method, path, err)
	}
	defer response.Body.Close()
	answer, _ := io.ReadAll(response.Body)
	if (want != 0 && response.StatusCode != want) || (want == 0 && response.StatusCode/100 != 2) {
		t.Fatalf("scratch Dispatch %s %s = %d %s", method, path, response.StatusCode, answer)
	}
	if into != nil {
		if err := json.Unmarshal(answer, into); err != nil {
			t.Fatalf("decode scratch Dispatch %s %s: %v (%s)", method, path, err, answer)
		}
	}
}

// ---- the seed ------------------------------------------------------------------------------------

// seed records every issue as its phase stands, with its tree and parent, admits every tree, and
// records each claim mid-launch with its task pending, as rig.seed does for an issue that is its
// own tree. A claim that owes no task has none pending.
func (w *w5Rig) seed() {
	t := w.t
	ctx := context.Background()
	issues := map[string]*w5Claim{}
	var keys []string
	for _, c := range w.ordered {
		token, err := claim.NewToken(w.rig.project, c.key, c.role)
		if err != nil {
			t.Fatal(err)
		}
		c.token = token
		if _, ok := issues[c.key]; !ok {
			issues[c.key] = c
			keys = append(keys, c.key)
		}
	}
	if err := w.st.Tx(ctx, func(tx pgx.Tx) error {
		for _, key := range keys {
			c := issues[key]
			issue := record.Issue{
				Key: key, Repo: liveRepo, DefaultBranch: "main", GitHubRepo: liveRepo, Tree: c.tree,
				Project: w.cfg.Project, Title: "w5 issue " + key, Phase: w.issuePhase(key), Generation: 1, Status: "in_progress",
				DispatchStatus: "in_progress", Rank: "U", LastDispatchSeq: 1, HandedOver: c.parent == "",
			}
			if c.parent != "" {
				parent := c.parent
				issue.Parent = &parent
			}
			if err := w.records.PutIssue(ctx, tx, issue); err != nil {
				return err
			}
		}
		return w.admit(ctx, tx, keys...)
	}); err != nil {
		t.Fatalf("seed the issues: %v", err)
	}
	opened := map[string]bool{}
	for _, c := range w.ordered {
		if !opened[c.tree] {
			opened[c.tree] = true
			if _, err := w.st.OpenTreeLifecycle(ctx, w.rig.project, c.tree, treelifecycle.AuthorityWorkflow); err != nil {
				t.Fatalf("open the lifecycle of %s: %v", c.tree, err)
			}
		}
		if _, err := w.st.AdmitClaim(ctx, supervise.Claim{
			Token: c.token, Project: w.rig.project, Tree: c.tree, Issue: c.key, Role: c.role,
			Generation: 1, State: supervise.StateLaunching,
			BootTokenHash: supervise.HashBootToken("interrupted-" + randomSuffix(t)), ServingGeneration: 1,
		}); err != nil {
			t.Fatalf("record the unfinished launch of %s: %v", c.token, err)
		}
		if c.task != "" {
			if err := w.st.PutDelivery(ctx, c.token, supervise.Delivery{
				ID: "seeded-start-" + randomSuffix(t), Task: c.task, Generation: 1, Phase: c.phase, QueuedAt: time.Now(),
			}); err != nil {
				t.Fatalf("record the task %s's start left pending: %v", c.token, err)
			}
		}
		writePrompt(t, w.cfg, c.token)
		w.note("seeded %s: %s (%s of tree %s, phase %s), claim %s mid-launch, task pending: %t", c.name, c.key, c.role, c.tree, c.phase, c.token, c.task != "")
	}
}

// issuePhase is the phase an issue stands in: the phase of the claim that works it (the first
// claim seeded on it whose role works its phase), or its first claim's.
func (w *w5Rig) issuePhase(key string) phase.Phase {
	var first phase.Phase
	for _, c := range w.ordered {
		if c.key != key {
			continue
		}
		if first == "" {
			first = c.phase
		}
		if phase.RoleFor(c.phase) == c.role {
			return c.phase
		}
	}
	return first
}

// ---- the stand-ins -------------------------------------------------------------------------------

// writeStandins starts one stand-in per claim, the controller's among them, writes the profile's
// models and enabled models for all of them beside the rig's own default stand-in, and gives each
// claim its own overlay; a claim the check did not seed (one the workflow launches later) keeps the
// rig's default, which answers every turn.
func (w *w5Rig) writeStandins() {
	t := w.t
	agent := filepath.Join(w.home, ".omp", "profiles", w.profile, "agent")
	var models, enabled strings.Builder
	models.WriteString("# Written by packages/daemon/internal/daemon/w5_rig_live_test.go.\nproviders:\n")
	fmt.Fprintf(&models, "  %s:\n    baseUrl: %s\n    api: anthropic-messages\n    apiKey: a-literal-key-no-endpoint-checks\n"+
		"    models:\n      - id: %s\n        name: %s\n        contextWindow: 200000\n        maxTokens: 8192\n",
		rigModelProvider, w.standin.url, rigModelProvider+standinModelID, rigModelProvider)
	fmt.Fprintf(&enabled, "  - %s\n", w.model)
	overlays := map[claim.Token]string{}
	dir := filepath.Join(t.TempDir(), "w5-overlays")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	all := append([]*w5Claim{}, w.ordered...)
	all = append(all, w.ctl)
	for _, c := range all {
		c.stand = newW5Standin(t, c.provider)
		fmt.Fprintf(&models, "  %s:\n    baseUrl: %s\n    api: anthropic-messages\n    apiKey: a-literal-key-no-endpoint-checks\n"+
			"    models:\n      - id: %s\n        name: %s\n        contextWindow: 200000\n        maxTokens: 8192\n",
			c.provider, c.stand.url, c.provider+standinModelID, c.provider)
		fmt.Fprintf(&enabled, "  - %s\n", standinModel(c.provider))
		overlay := filepath.Join(dir, c.provider+".yml")
		writeRoleOverlay(t, overlay, standinModel(c.provider))
		token := c.token
		if token == "" {
			var err error
			if token, err = claim.NewToken(w.rig.project, c.key, c.role); err != nil {
				t.Fatal(err)
			}
		}
		overlays[token] = overlay
	}
	writeProofFile(t, filepath.Join(agent, "models.yml"), models.String())
	writeProofFile(t, filepath.Join(agent, "config.yml"),
		"# Written by packages/daemon/internal/daemon/w5_rig_live_test.go.\nenabledModels:\n"+enabled.String()+
			"startup:\n  setupWizard: false\n")
	defaultOverlay := filepath.Join(dir, "default.yml")
	writeRoleOverlay(t, defaultOverlay, w.model)
	w.rt.overlays = func(token claim.Token) []string {
		if overlay, ok := overlays[token]; ok {
			return []string{overlay}
		}
		return []string{defaultOverlay}
	}
}

// w5Standin is one claim's provider endpoint: paneStandin, which answers every turn alike, behind
// the answers a step scripts in front of it. A rule takes the first request it matches; it may
// hold it until the step lets it go, wait a while, and answer it with a tool call, and it tells
// the step the moment it answered (served). Every Messages request is kept, with its whole
// conversation while capture is on.
type w5Standin struct {
	*paneStandin

	mu      sync.Mutex
	rules   []*w5Rule
	seen    []w5Seen
	capture bool
	stopped chan struct{}
}

// w5Rule is one scripted answer. A rule that holds every request it takes (holdAll) is never
// spent.
type w5Rule struct {
	match   func(seenRequest) bool
	spent   bool
	hold    *heldRequest
	holdAll bool
	delay   time.Duration
	call    *toolCall
	served  chan time.Time
}

// w5Seen is one request as a step reads it back: scriptedStandin's reading, with the whole
// conversation kept while the stand-in captures.
type w5Seen struct {
	seenRequest
	whole string
}

func newW5Standin(t *testing.T, name string) *w5Standin {
	t.Helper()
	s := &w5Standin{paneStandin: &paneStandin{modelStandin: &modelStandin{name: name}}, stopped: make(chan struct{})}
	s.serve(t, s)
	t.Cleanup(func() { close(s.stopped) })
	return s
}

func (s *w5Standin) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost || !strings.HasSuffix(req.URL.Path, "/v1/messages") {
		s.paneStandin.ServeHTTP(w, req)
		return
	}
	body, err := io.ReadAll(req.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	seen := readStandinRequest(body, time.Now())
	seen.side = seen.side || isJobCaption(body)
	s.mu.Lock()
	var rule *w5Rule
	if !seen.side {
		for _, candidate := range s.rules {
			if !candidate.spent && candidate.match(seen) {
				candidate.spent = !candidate.holdAll
				rule = candidate
				break
			}
		}
	}
	kept := w5Seen{seenRequest: seen}
	if s.capture {
		kept.whole = seen.conversation
	}
	kept.conversation = ""
	s.seen = append(s.seen, kept)
	id := fmt.Sprintf("toolu_%s_%d", s.name, len(s.seen))
	s.mu.Unlock()
	if rule != nil && rule.hold != nil && !rule.hold.take(req.Context(), s.stopped) {
		return
	}
	if rule != nil && rule.holdAll {
		select {
		case <-req.Context().Done():
			return
		case <-s.stopped:
		}
	}
	if rule != nil && rule.delay > 0 {
		select {
		case <-time.After(rule.delay):
		case <-req.Context().Done():
			return
		case <-s.stopped:
		}
	}
	if rule != nil && rule.call != nil {
		streamAnthropicToolUse(w, s.name, id, rule.call.name, rule.call.input)
		if rule.served != nil {
			select {
			case rule.served <- time.Now():
			default:
			}
		}
		return
	}
	req.Body = io.NopCloser(bytes.NewReader(body))
	s.paneStandin.ServeHTTP(w, req)
}

// jobCaptionSystemLine opens the system prompt of the request Oh My Pi sends, beside the turn, to
// caption a background job for its job list (job-caption-system.md, in the pinned build).
const jobCaptionSystemLine = "You explain one background shell command"

// isJobCaption is whether a Messages request is Oh My Pi's caption of a background job: no turn of
// the role's. Its message holds the job's command, so a rule that waits for a word the job prints
// would take it when the caption goes out late: a process the rig stops right after it backgrounds
// a job sends the caption only once it runs again, beside the turn's own next request.
func isJobCaption(body []byte) bool {
	var request struct {
		System json.RawMessage `json:"system"`
	}
	return json.Unmarshal(body, &request) == nil && strings.Contains(string(request.System), jobCaptionSystemLine)
}

// A rule that waits for a word a background job's command holds takes the turn's own request, and
// never the caption Oh My Pi asks for that job beside it, whichever of the two comes first. On a
// loaded host the rig stopped the fifth claim's process 0.8 s after it backgrounded its job, before
// the caption went out; the caption went out only when the process ran again, the rule answered
// it, and the turn got the stand-in's plain text and ended with no wait open.
func TestTheW5StandinAnswersAJobCaptionAsNoTurn(t *testing.T) {
	s := newW5Standin(t, "w5caption")
	served := s.script(func(seen seenRequest) bool {
		return anyTurn(seen) && strings.Contains(seen.last, "FIFTH-JOB-DONE")
	}, 0, "bash", map[string]any{"command": "echo started"})
	command := "while [ ! -f done ]; do sleep 1; done; echo FIFTH-JOB-DONE"
	send := func(name string, request map[string]any) string {
		body, err := json.Marshal(request)
		if err != nil {
			t.Fatalf("encode the %s: %v", name, err)
		}
		answer := httptest.NewRecorder()
		s.ServeHTTP(answer, httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body)))
		return answer.Body.String()
	}
	caption := send("caption", map[string]any{
		"model":  "w5caption-model",
		"system": []map[string]any{{"type": "text", "text": jobCaptionSystemLine + " to a person glancing at a list of running jobs."}},
		"messages": []map[string]any{{"role": "user", "content": []map[string]any{{"type": "text",
			"text": "<job>\nWhy it was started: Running the proof's step\nTimeout: none\nCommand:\n```\n" + command + "\n```\n</job>"}}}},
	})
	select {
	case <-served:
		t.Fatalf("the rule answered the job's caption: %s", caption)
	default:
	}
	if strings.Contains(caption, "tool_use") {
		t.Fatalf("the caption was answered with a tool call: %s", caption)
	}
	turn := send("turn", map[string]any{
		"model":  "w5caption-model",
		"system": "You are the role's agent.",
		"messages": []map[string]any{
			{"role": "user", "content": "Run the proof's step."},
			{"role": "assistant", "content": []map[string]any{{"type": "tool_use", "id": "toolu_1", "name": "wait", "input": map[string]any{}}}},
			{"role": "user", "content": []map[string]any{{"type": "tool_result", "tool_use_id": "toolu_1",
				"content": []map[string]any{{"type": "text", "text": "## Completed (1)\n```\nFIFTH-JOB-DONE\n```"}}}}},
		},
	})
	select {
	case <-served:
	default:
		t.Fatalf("the rule did not answer the turn that carries the job's end: %s", turn)
	}
	if !strings.Contains(turn, `"name":"bash"`) {
		t.Fatalf("the turn was not answered with the scripted bash call: %s", turn)
	}
}

// script adds a rule answering the first request match takes with a call of tool, after delay,
// and answers the channel the moment of each answer reaches.
func (s *w5Standin) script(match func(seenRequest) bool, delay time.Duration, tool string, input map[string]any) chan time.Time {
	called := map[string]any{"i": "Running the proof's step"}
	maps.Copy(called, input)
	served := make(chan time.Time, 1)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rules = append(s.rules, &w5Rule{match: match, delay: delay, call: &toolCall{name: tool, input: called}, served: served})
	return served
}

// holdNext holds the first request match takes until the step lets it go.
func (s *w5Standin) holdNext(match func(seenRequest) bool) *heldRequest {
	held := &heldRequest{arrived: make(chan struct{}), release: make(chan struct{})}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rules = append(s.rules, &w5Rule{match: match, hold: held})
	return held
}

// holdEvery holds every request match takes, from now on, until the process that asked goes away
// or the run ends: a model that has not answered yet, as a real one has not a second into a turn.
func (s *w5Standin) holdEvery(match func(seenRequest) bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rules = append(s.rules, &w5Rule{match: match, holdAll: true})
}

// dropRules spends every rule not yet taken, so the stand-in answers every turn from here on.
func (s *w5Standin) dropRules() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, rule := range s.rules {
		rule.spent = true
	}
}

// captureFrom keeps the whole conversation of every request from now on.
func (s *w5Standin) captureFrom() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.capture = true
}

// requestsAfter is every request the stand-in received after at.
func (s *w5Standin) requestsAfter(at time.Time) []w5Seen {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []w5Seen
	for _, seen := range s.seen {
		if seen.at.After(at) {
			out = append(out, seen)
		}
	}
	return out
}

// anyTurn takes a role turn's request: a prompt or a continuation, never a side question or a
// subagent's own request.
func anyTurn(seen seenRequest) bool { return !seen.side && !seen.subagent }

// continuing takes a role turn's request whose last message is a tool's result.
func continuing(seen seenRequest) bool { return !seen.prompt && !seen.side && !seen.subagent }

// ---- the pods ------------------------------------------------------------------------------------

// podRuntime is the rig's runtime as a runtime with pods hands its claims to the daemon: every
// locator the daemon is given is a Sandbox locator of the claim's pod — one pod per issue, the
// roles of an issue sharing it, and one for the controller — and every locator the daemon hands
// back is translated to the local process it stands for. The incarnation is the Sandbox one
// (`<pod uid>/<generation>`, the launch's generation), so the daemon fences observations as it
// fences a pod's. Nothing else of the local runtime changes.
//
// The pod of a claim can be replaced (replacePod), which the controller's relaunch through a
// launcher that is not connected does: its next launch runs in a pod of a new uid.
type podRuntime struct {
	*rigRuntime
	podOf func(claim.Token) string

	mu       sync.Mutex
	pods     map[string]string
	replaced map[string]int
	toLocal  map[string]runtime.Locator
	toPod    map[string]runtime.Locator
	// launcher is the launcher state the rig reports for a claim's relaunch
	// (w5_inplace_live_test.go): "" connected, or one of the w5Launcher words. relaunches is
	// every relaunch refusal and stop the daemon asked of it, oldest first.
	launcher   map[claim.Token]string
	relaunches []podRelaunchCall
}

// The launcher states the rig can report for a claim's relaunch.
const (
	w5LauncherNotConnected = "not-connected"
	w5LauncherReplacesPod  = "replaces-pod"
	w5LauncherOutlivesStop = "outlives-stop"
)

// podRelaunchCall is one call of the relaunch's runtime half, as the rig answered it.
type podRelaunchCall struct {
	at            time.Time
	call          string
	claim         claim.Token
	mayReplacePod bool
	launcher      string
	answer        string
	running       bool
}

func newPodRuntime(local *rigRuntime, podOf func(claim.Token) string) *podRuntime {
	return &podRuntime{rigRuntime: local, podOf: podOf, pods: map[string]string{}, replaced: map[string]int{},
		toLocal: map[string]runtime.Locator{}, toPod: map[string]runtime.Locator{}, launcher: map[claim.Token]string{}}
}

// podOf is the pod a claim's process runs in: its issue's, or the controller's.
func (w *w5Rig) podOf(token claim.Token) string {
	if token == claim.ControllerToken(w.rig.project) {
		return "controller"
	}
	for _, c := range w.ordered {
		if c.token == token {
			return c.key
		}
	}
	// A claim the workflow launched later: its issue is the token's middle.
	return string(token)
}

// podUID is the current uid of pod.
func (p *podRuntime) podUID(pod string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.podUIDLocked(pod)
}

func (p *podRuntime) podUIDLocked(pod string) string {
	if uid, ok := p.pods[pod]; ok {
		return uid
	}
	uid := fmt.Sprintf("pod-%s-%d", strings.ToLower(pod), p.replaced[pod]+1)
	p.pods[pod] = uid
	return uid
}

// replacePod ends pod's uid: the next launch into it runs in a pod of a new uid.
func (p *podRuntime) replacePod(pod string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.replaced[pod]++
	delete(p.pods, pod)
}

func locatorKey(loc runtime.Locator) string { return string(loc.Claim) + "|" + loc.Incarnation }

// wrap records local as the process of a launch of spec, and answers its Sandbox locator.
func (p *podRuntime) wrap(spec runtime.SpawnSpec, local runtime.Locator) runtime.Locator {
	p.mu.Lock()
	defer p.mu.Unlock()
	pod := p.podOf(spec.Claim)
	uid := p.podUIDLocked(pod)
	container := runtime.IssuePodContainer
	name := "issue-" + strings.ToLower(pod)
	if spec.Claim == claim.ControllerToken(spec.Project) || pod == "controller" {
		container, name = runtime.ControllerPodContainer, "controller"
	}
	sandbox := runtime.Locator{
		Runtime: runtime.RuntimeSandbox, Claim: spec.Claim, Incarnation: runtime.SandboxIncarnation(uid, spec.Generation),
		Sandbox: &runtime.SandboxLocator{Namespace: "legion-rig", Name: name, PodUID: uid, Container: container, Generation: spec.Generation},
	}
	p.toLocal[locatorKey(sandbox)] = local
	p.toPod[locatorKey(local)] = sandbox
	return sandbox
}

// local is the local process a Sandbox locator stands for; a locator this runtime never minted is
// passed on as it is, and the local runtime answers it as one naming no process of its.
func (p *podRuntime) local(loc runtime.Locator) runtime.Locator {
	p.mu.Lock()
	defer p.mu.Unlock()
	if local, ok := p.toLocal[locatorKey(loc)]; ok {
		return local
	}
	return loc
}

// pod is the Sandbox locator of a local process's locator, or loc itself.
func (p *podRuntime) pod(loc runtime.Locator) runtime.Locator {
	p.mu.Lock()
	defer p.mu.Unlock()
	if sandbox, ok := p.toPod[locatorKey(loc)]; ok {
		return sandbox
	}
	return loc
}

func (p *podRuntime) known(k runtime.Known) runtime.Known {
	if k.Locator != nil {
		local := p.local(*k.Locator)
		k.Locator = &local
	}
	return k
}

func (p *podRuntime) Spawn(ctx context.Context, spec runtime.SpawnSpec) (runtime.Locator, error) {
	loc, err := p.rigRuntime.Spawn(ctx, spec)
	if err != nil {
		return loc, err
	}
	return p.wrap(spec, loc), nil
}

func (p *podRuntime) Resume(ctx context.Context, prev *runtime.Locator, spec runtime.SpawnSpec) (runtime.Locator, error) {
	if prev != nil {
		local := p.local(*prev)
		prev = &local
	}
	loc, err := p.rigRuntime.Resume(ctx, prev, spec)
	if err != nil {
		return loc, err
	}
	return p.wrap(spec, loc), nil
}

func (p *podRuntime) Suspend(ctx context.Context, loc runtime.Locator) error {
	return p.rigRuntime.Suspend(ctx, p.local(loc))
}

func (p *podRuntime) Release(ctx context.Context, k runtime.Known) error {
	return p.rigRuntime.Release(ctx, p.known(k))
}

func (p *podRuntime) Probe(ctx context.Context, loc runtime.Locator) (runtime.Observation, error) {
	o, err := p.rigRuntime.Probe(ctx, p.local(loc))
	if err != nil {
		return o, err
	}
	o.Locator = p.pod(o.Locator)
	return o, nil
}

func (p *podRuntime) Observe(ctx context.Context) (<-chan runtime.Observation, error) {
	inner, err := p.rigRuntime.Observe(ctx)
	if err != nil {
		return nil, err
	}
	out := make(chan runtime.Observation, 64)
	go func() {
		defer close(out)
		for o := range inner {
			o.Locator = p.pod(o.Locator)
			select {
			case out <- o:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
}

func (p *podRuntime) ReconcileOrphans(ctx context.Context, known []runtime.Known, grace time.Duration) error {
	translated := make([]runtime.Known, 0, len(known))
	for _, k := range known {
		translated = append(translated, p.known(k))
	}
	return p.rigRuntime.ReconcileOrphans(ctx, translated, grace)
}

func (p *podRuntime) AdoptWorkingCopy(ctx context.Context, loc runtime.Locator, id runtime.GitIdentity) error {
	return p.rigRuntime.AdoptWorkingCopy(ctx, p.local(loc), id)
}

func (p *podRuntime) PodDrift(ctx context.Context, k runtime.Known) ([]string, error) {
	return p.rigRuntime.PodDrift(ctx, p.known(k))
}

func (p *podRuntime) ReplacePod(ctx context.Context, k runtime.Known, due func(context.Context) error) error {
	return p.rigRuntime.ReplacePod(ctx, p.known(k), due)
}

// setLauncher has the rig report token's launcher state for its next relaunch: "" connected.
func (p *podRuntime) setLauncher(token claim.Token, state string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if state == "" {
		delete(p.launcher, token)
		return
	}
	p.launcher[token] = state
}

func (p *podRuntime) launcherOf(token claim.Token) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.launcher[token]
}

func (p *podRuntime) noteRelaunch(call podRelaunchCall) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.relaunches = append(p.relaunches, call)
}

// relaunchCalls is every relaunch call of token from at.
func (p *podRuntime) relaunchCalls(token claim.Token, from time.Time) []podRelaunchCall {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []podRelaunchCall
	for _, c := range p.relaunches {
		if c.claim == token && !c.at.Before(from) {
			out = append(out, c)
		}
	}
	return out
}

// ---- the daemon ----------------------------------------------------------------------------------

// overrides is rig.overrides with the pods runtime handed to the daemon.
func (w *w5Rig) overrides() overrides {
	return overrides{
		runtime: func(_ context.Context, listener *stream.Listener, address string, _ appauth.Source,
			_ func(context.Context, string) (ghrepo.Repository, string, error), _ *store.Store, _ func() []ghrepo.Repository) (runtime.Runtime, error) {
			w.built.mu.Lock()
			defer w.built.mu.Unlock()
			w.built.conns, w.built.address = listener, address
			w.runtime.conns, w.runtime.stream = listener, address
			w.relay.setUpstream(address)
			return w.pods, nil
		},
		listen:         heldListen,
		workflowTokens: &workflowTokenRecorder{},
	}
}

// boot starts the daemon over the rig with the pods runtime.
func (w *w5Rig) boot() {
	w.t.Helper()
	w.d = startDaemonWithin(w.root, w.cfg, w.overrides(), slog.New(w.daemon), 3*time.Minute)
}

// restartDaemon is rig.restartDaemon over boot.
func (w *w5Rig) restartDaemon() (stopped, serving time.Time) {
	w.t.Helper()
	w.d.stop()
	stopped = time.Now()
	w.holdPortAgain()
	w.boot()
	return stopped, time.Now()
}

// ---- the fronts ----------------------------------------------------------------------------------

// daemonFront is the address every pane reaches the daemon's API at: a reverse proxy carrying
// every request to the daemon unchanged, but an activity report of a claim whose reports the rig
// drops, which is answered as the daemon answers a recorded one and recorded nowhere.
type daemonFront struct {
	url   string
	proxy *httputil.ReverseProxy

	mu      sync.Mutex
	drop    map[claim.Token]bool
	dropped map[claim.Token]int
	passed  map[claim.Token]int
	// reports is when each passed report of a claim reached the front.
	reports map[claim.Token][]time.Time
}

func newDaemonFront(t *testing.T, daemonURL string) *daemonFront {
	t.Helper()
	target, err := url.Parse(daemonURL)
	if err != nil {
		t.Fatal(err)
	}
	f := &daemonFront{proxy: httputil.NewSingleHostReverseProxy(target), drop: map[claim.Token]bool{},
		dropped: map[claim.Token]int{}, passed: map[claim.Token]int{}, reports: map[claim.Token][]time.Time{}}
	server := httptest.NewServer(f)
	t.Cleanup(server.Close)
	f.url = server.URL
	return f
}

func (f *daemonFront) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if req.Method == http.MethodPost && req.URL.Path == "/legion/v1/claims/activity" {
		body, err := io.ReadAll(req.Body)
		req.Body.Close()
		if err != nil {
			http.Error(w, "read the request", http.StatusBadRequest)
			return
		}
		var named struct {
			ClaimToken claim.Token `json:"claimToken"`
		}
		_ = json.Unmarshal(body, &named)
		f.mu.Lock()
		drop := f.drop[named.ClaimToken]
		if drop {
			f.dropped[named.ClaimToken]++
		} else {
			f.passed[named.ClaimToken]++
			f.reports[named.ClaimToken] = append(f.reports[named.ClaimToken], time.Now())
		}
		f.mu.Unlock()
		if drop {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		req.Body = io.NopCloser(bytes.NewReader(body))
		req.ContentLength = int64(len(body))
	}
	f.proxy.ServeHTTP(w, req)
}

// dropActivity drops every activity report of token from now on.
func (f *daemonFront) dropActivity(token claim.Token) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.drop[token] = true
}

func (f *daemonFront) activityCounts(token claim.Token) (dropped, passed int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.dropped[token], f.passed[token]
}

// reportsFrom is when each activity report of token the front passed on reached it, from from.
func (f *daemonFront) reportsFrom(token claim.Token, from time.Time) []time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []time.Time
	for _, at := range f.reports[token] {
		if !at.Before(from) {
			out = append(out, at)
		}
	}
	return out
}

// envoyListFront is the address the daemon alone reaches Envoy at: a reverse proxy carrying every
// request to the rig's listener front unchanged, but a read of Envoy's session list while the rig
// fails them, which is answered 503 as a listener that cannot read its bucket answers.
type envoyListFront struct {
	url   string
	proxy *httputil.ReverseProxy

	mu       sync.Mutex
	failNext int
	failTill time.Time
	reads    []envoyListRead
}

// envoyListRead is one read of the list the daemon made: whether the front failed it, and for one
// the listener answered, how many sessions it named live (notify.ListedSession.Live) and which. A
// read that names none holds back every mark not made yet for 20 minutes (the reach keeper's
// noLiveAt), so a step that reads a mark records the reads that did.
type envoyListRead struct {
	at     time.Time
	failed bool
	status int
	live   []string
}

func newEnvoyListFront(t *testing.T, front string) *envoyListFront {
	t.Helper()
	target, err := url.Parse(front)
	if err != nil {
		t.Fatal(err)
	}
	f := &envoyListFront{proxy: httputil.NewSingleHostReverseProxy(target)}
	f.proxy.ModifyResponse = f.readList
	server := httptest.NewServer(f)
	t.Cleanup(server.Close)
	f.url = server.URL
	return f
}

func (f *envoyListFront) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if req.Method == http.MethodGet && req.URL.Path == "/v1/sessions" {
		f.mu.Lock()
		fail := f.failNext > 0 || time.Now().Before(f.failTill)
		if f.failNext > 0 {
			f.failNext--
		}
		if fail {
			f.reads = append(f.reads, envoyListRead{at: time.Now(), failed: true})
		}
		f.mu.Unlock()
		if fail {
			http.Error(w, `{"error":"the rig fails this read of the session list"}`, http.StatusServiceUnavailable)
			return
		}
	}
	f.proxy.ServeHTTP(w, req)
}

// readList records a read of the list the listener answered, with the sessions it names live, and
// hands the body on unchanged.
func (f *envoyListFront) readList(response *http.Response) error {
	if response.Request.Method != http.MethodGet || response.Request.URL.Path != "/v1/sessions" {
		return nil
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		return err
	}
	response.Body = io.NopCloser(bytes.NewReader(body))
	read := envoyListRead{at: time.Now(), status: response.StatusCode}
	var rows []struct {
		Session string `json:"session_id"`
		State   string `json:"state"`
	}
	if json.Unmarshal(body, &rows) == nil {
		for _, row := range rows {
			if row.State == "live" {
				read.live = append(read.live, row.Session)
			}
		}
	}
	f.mu.Lock()
	f.reads = append(f.reads, read)
	f.mu.Unlock()
	return nil
}

// emptyReadsFrom is every read from from that the listener answered with no live session.
func (f *envoyListFront) emptyReadsFrom(from time.Time) []time.Time {
	var out []time.Time
	for _, r := range f.readsFrom(from) {
		if !r.failed && r.status == http.StatusOK && len(r.live) == 0 {
			out = append(out, r.at)
		}
	}
	return out
}

// recordEmptyReads records every read of Envoy's list from from that named no live session, each
// of which holds back a mark not made yet until 20 minutes after it, and answers the latest.
func (w *w5Rig) recordEmptyReads(label string, from time.Time) time.Time {
	empty := w.envoy.emptyReadsFrom(from)
	var stamps []string
	var latestAt time.Time
	for _, at := range empty {
		stamps = append(stamps, clockAt(at))
		latestAt = at
	}
	reads := len(w.envoy.readsFrom(from))
	w.record("%s: %d reads of Envoy's list from %s, %d of them naming no live session: %v", label, reads, clockAt(from), len(empty), stamps)
	return latestAt
}

// failReads fails the next n reads of the list.
func (f *envoyListFront) failReads(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failNext += n
}

// failFor fails every read of the list until d from now.
func (f *envoyListFront) failFor(d time.Duration) time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failTill = time.Now().Add(d)
	return f.failTill
}

func (f *envoyListFront) readsFrom(from time.Time) []envoyListRead {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []envoyListRead
	for _, r := range f.reads {
		if !r.at.Before(from) {
			out = append(out, r)
		}
	}
	return out
}

// ---- freezing a process --------------------------------------------------------------------------

// ompPid is the Oh My Pi process of token's current launch: the shim's child.
func (w *w5Rig) ompPid(token claim.Token) (int, bool) {
	launch := w.runtime.current(token)
	if launch == nil || !launch.running() {
		return 0, false
	}
	for _, child := range childrenOf(launch.pane.pid) {
		return child, true
	}
	return 0, false
}

// childrenOf is every process whose parent is pid.
func childrenOf(pid int) []int {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	var children []int
	for _, entry := range entries {
		child, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		raw, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "stat"))
		if err != nil {
			continue
		}
		text := string(raw)
		end := strings.LastIndexByte(text, ')')
		if end < 0 {
			continue
		}
		fields := strings.Fields(text[end+1:])
		if len(fields) < 2 {
			continue
		}
		if parent, err := strconv.Atoi(fields[1]); err == nil && parent == pid {
			children = append(children, child)
		}
	}
	sort.Ints(children)
	return children
}

// processState is /proc/<pid>/stat's state letter, "" for a process that is gone.
func processState(pid int) string {
	raw, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return ""
	}
	text := string(raw)
	end := strings.LastIndexByte(text, ')')
	if end < 0 {
		return ""
	}
	fields := strings.Fields(text[end+1:])
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}

// openToolCall is whether the newest tool frame token's current connection sent the daemon after
// from is a call's start with no end after it, and when that start was carried.
func (w *w5Rig) openToolCall(token claim.Token, from time.Time) (time.Time, bool) {
	launch := w.runtime.current(token)
	if launch == nil {
		return time.Time{}, false
	}
	var started time.Time
	open := false
	for _, c := range w.relay.connsOf(launch.spec.BootToken) {
		for _, f := range c.toDaemon {
			if f.at.Before(from) {
				continue
			}
			switch f.typ {
			case "tool_execution_start":
				started, open = f.at, true
			case "tool_execution_end":
				open = false
			}
		}
	}
	return started, open
}

// freezeInWait stops token's Oh My Pi process with SIGSTOP once the `wait` call its stand-in
// answered at served is open, and confirms afterwards that the call was still open when the stop
// landed. It answers when the call started and when the process was stopped.
func (w *w5Rig) freezeInWait(token claim.Token, served time.Time) (time.Time, time.Time) {
	t := w.t
	t.Helper()
	var started time.Time
	w.await(string(token)+"'s wait call to start", 30*time.Second, func() bool {
		var open bool
		started, open = w.openToolCall(token, served)
		return open
	})
	pid, ok := w.ompPid(token)
	if !ok {
		w.fails("freeze", "%s runs no Oh My Pi process to stop", token)
	}
	if err := syscall.Kill(pid, syscall.SIGSTOP); err != nil {
		w.fails("freeze", "SIGSTOP %d: %v", pid, err)
	}
	stopped := time.Now()
	w.markFrozen(token, pid)
	time.Sleep(500 * time.Millisecond)
	if _, open := w.openToolCall(token, served); !open {
		_ = syscall.Kill(pid, syscall.SIGCONT)
		w.markFrozen(token, 0)
		w.fails("freeze", "%s's wait call ended before its process was stopped", token)
	}
	w.record("%s: Oh My Pi (pid %d) stopped with SIGSTOP at %s, inside the wait call that started at %s; state %s", token, pid, clockAt(stopped), clockAt(started), processState(pid))
	return started, stopped
}

// stopProcess stops token's Oh My Pi process with SIGSTOP from a flow's own goroutine, which
// freezeInWait's failures may not end, and answers its pid, 0 for none.
func (w *w5Rig) stopProcess(token claim.Token) int {
	pid, ok := w.ompPid(token)
	if !ok {
		return 0
	}
	if syscall.Kill(pid, syscall.SIGSTOP) != nil {
		return 0
	}
	w.markFrozen(token, pid)
	return pid
}

// markFrozen records token's Oh My Pi as stopped at pid, or as running for pid 0.
func (w *w5Rig) markFrozen(token claim.Token, pid int) {
	w.frozenMu.Lock()
	defer w.frozenMu.Unlock()
	if pid == 0 {
		delete(w.frozen, token)
		return
	}
	w.frozen[token] = pid
}

// frozenLabel names every claim whose Oh My Pi the rig holds stopped now: one it stopped whose
// process is still in the stopped state, as /proc reads it.
func (w *w5Rig) frozenLabel() string {
	w.frozenMu.Lock()
	defer w.frozenMu.Unlock()
	var names []string
	for token, pid := range w.frozen {
		if state := processState(pid); state != "T" && state != "t" {
			delete(w.frozen, token)
			continue
		}
		name := string(token)
		for n, c := range w.claims {
			if c.token == token {
				name = n
			}
		}
		names = append(names, name)
	}
	if len(names) == 0 {
		return "[frozen: none]"
	}
	sort.Strings(names)
	return "[frozen: " + strings.Join(names, ",") + "]"
}

// thaw lets token's stopped Oh My Pi process run again.
func (w *w5Rig) thaw(token claim.Token) {
	pid, ok := w.ompPid(token)
	if !ok {
		w.record("%s: no Oh My Pi process to let run again", token)
		return
	}
	_ = syscall.Kill(pid, syscall.SIGCONT)
	w.markFrozen(token, 0)
	w.record("%s: Oh My Pi (pid %d) let run again with SIGCONT at %s", token, pid, clockAt(time.Now()))
}

// ---- the outbox audit ----------------------------------------------------------------------------

// installOutboxAudit installs the trigger that copies each outbox row's insert, update and delete,
// with its moment, into w5_outbox_audit.
func (w *w5Rig) installOutboxAudit() {
	t := w.t
	ctx := context.Background()
	for _, statement := range []string{
		`create table if not exists w5_outbox_audit (seq bigserial primary key, op text not null,
			at timestamptz not null default clock_timestamp(), id bigint, kind text, issue text, payload text)`,
		`create or replace function w5_outbox_audit_fn() returns trigger language plpgsql as $$
		begin
			if tg_op = 'DELETE' then
				insert into w5_outbox_audit(op, id, kind, issue, payload) values ('delete', old.id, old.kind, old.issue, old.payload::text);
				return old;
			end if;
			insert into w5_outbox_audit(op, id, kind, issue, payload) values (lower(tg_op), new.id, new.kind, new.issue, new.payload::text);
			return new;
		end $$`,
		`drop trigger if exists w5_outbox_audit on outbox`,
		`create trigger w5_outbox_audit after insert or update or delete on outbox for each row execute function w5_outbox_audit_fn()`,
	} {
		if _, err := w.st.Pool().Exec(ctx, statement); err != nil {
			t.Fatalf("install the outbox audit: %v", err)
		}
	}
}

// auditRow is one audited write of an outbox row.
type auditRow struct {
	seq     int64
	op      string
	at      time.Time
	id      int64
	kind    string
	issue   string
	payload string
}

// outboxAudit is every audited write whose row's kind is one of kinds (all for none), oldest first.
func (w *w5Rig) outboxAudit(kinds ...string) []auditRow {
	rows, err := w.st.Pool().Query(context.Background(), `select seq, op, at, coalesce(id, 0), coalesce(kind, ''), coalesce(issue, ''), coalesce(payload, '')
		from w5_outbox_audit order by seq`)
	if err != nil {
		w.record("read the outbox audit: %v", err)
		return nil
	}
	defer rows.Close()
	var out []auditRow
	for rows.Next() {
		var a auditRow
		if err := rows.Scan(&a.seq, &a.op, &a.at, &a.id, &a.kind, &a.issue, &a.payload); err != nil {
			w.record("scan the outbox audit: %v", err)
			return out
		}
		if len(kinds) == 0 || containsString(kinds, a.kind) {
			out = append(out, a)
		}
	}
	return out
}

func containsString(list []string, s string) bool {
	for _, item := range list {
		if item == s {
			return true
		}
	}
	return false
}

// rowLife is when an outbox row was written, held, and finished, from the audit, with its kind, its
// issue (the row's own column, which a notice's payload does not repeat) and its payload.
type rowLife struct {
	id       int64
	kind     string
	issue    string
	payload  string
	written  time.Time
	updates  int
	finished time.Time
}

func (w *w5Rig) rowLives(kinds ...string) map[int64]*rowLife {
	lives := map[int64]*rowLife{}
	for _, a := range w.outboxAudit(kinds...) {
		life := lives[a.id]
		if life == nil {
			life = &rowLife{id: a.id, kind: a.kind, issue: a.issue, payload: a.payload}
			lives[a.id] = life
		}
		switch a.op {
		case "insert":
			life.written = a.at
		case "update":
			life.updates++
		case "delete":
			life.finished = a.at
		}
	}
	return lives
}

// pendingRows is how many rows of kind for any of issues the outbox holds now, -1 when it cannot
// be read.
func (w *w5Rig) pendingRows(kind string, issues ...string) int {
	var n int
	if err := w.st.Pool().QueryRow(context.Background(), `select count(*) from outbox where kind = $1 and issue = any($2)`, kind, issues).Scan(&n); err != nil {
		w.record("read the outbox's %s rows of %v: %v", kind, issues, err)
		return -1
	}
	return n
}

// enqueueRow writes one outbox row as the workflow writes it, due now; the outbox's sweep takes it
// within its lease (outboxLease).
func (w *w5Rig) enqueueRow(issue string, payload record.OutboxPayload) int64 {
	t := w.t
	row, err := record.NewOutboxRow(issue, payload, time.Now())
	if err != nil {
		t.Fatalf("build the outbox row: %v", err)
	}
	before := len(w.outboxAudit())
	if err := pgx.BeginFunc(context.Background(), w.st.Pool(), func(tx pgx.Tx) error { return w.records.Enqueue(context.Background(), tx, row) }); err != nil {
		t.Fatalf("enqueue the outbox row: %v", err)
	}
	for _, a := range w.outboxAudit()[before:] {
		if a.op == "insert" && a.issue == issue {
			return a.id
		}
	}
	return 0
}

// ---- the daemon's routes -------------------------------------------------------------------------

// apiCall makes one request of the daemon, with the operator's bearer when operator says so, and
// answers the status and body; a transport failure is status 0 and its error as the body.
func (w *w5Rig) apiCall(method, path string, body any, operator bool, within time.Duration) (int, []byte) {
	var reader io.Reader
	if body != nil {
		encoded, _ := json.Marshal(body)
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequest(method, "http://127.0.0.1:"+strconv.Itoa(w.cfg.Port)+path, reader)
	if err != nil {
		return 0, []byte(err.Error())
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if operator {
		req.Header.Set("Authorization", "Bearer "+testOperatorToken)
	}
	response, err := (&http.Client{Timeout: within}).Do(req)
	if err != nil {
		return 0, []byte(err.Error())
	}
	defer response.Body.Close()
	answer, _ := io.ReadAll(response.Body)
	return response.StatusCode, answer
}

// controllerGrant mints a controller grant as `legion controller` does on the operator's bearer.
func (w *w5Rig) controllerGrant() string {
	w.grantsMu.Lock()
	defer w.grantsMu.Unlock()
	status, body := w.apiCall(http.MethodPost, "/legion/v1/grants", map[string]any{}, true, 30*time.Second)
	var grant struct {
		GrantID string `json:"grantId"`
	}
	if status != http.StatusOK || json.Unmarshal(body, &grant) != nil || grant.GrantID == "" {
		w.record("the controller grant = %d %s", status, body)
		return ""
	}
	return grant.GrantID
}

// relaunchAnswer is the daemon's answer to a `relaunch`: at is when the request was sent, and
// answered when the answer arrived, so a time the daemon read while it judged the request lies
// between the two.
type relaunchAnswer struct {
	at       time.Time
	answered time.Time
	status   int
	code     string
	body     string
}

func (a relaunchAnswer) reached() bool { return a.status == http.StatusOK }

func (a relaunchAnswer) String() string {
	return fmt.Sprintf("%d %s at %s: %s", a.status, a.code, clockAt(a.at), a.body)
}

// relaunch calls the controller's `relaunch` of token through the daemon's route, as the `legion`
// tool's relaunch posts it: a fresh controller grant and the claim's token.
func (w *w5Rig) relaunch(token claim.Token) relaunchAnswer {
	grant := w.controllerGrant()
	at := time.Now()
	status, body := w.apiCall(http.MethodPost, "/legion/v1/claims/relaunch", map[string]any{"grantId": grant, "claim": string(token)}, false, 5*time.Minute)
	answer := relaunchAnswer{at: at, answered: time.Now(), status: status, body: strings.TrimSpace(string(body))}
	var failure struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(body, &failure)
	answer.code = failure.Code
	w.record("relaunch %s: %s", token, answer)
	return answer
}

// freshSession calls the controller's `fresh_session` of role on issue.
func (w *w5Rig) freshSession(issue string, role claim.Role) relaunchAnswer {
	grant := w.controllerGrant()
	at := time.Now()
	status, body := w.apiCall(http.MethodPost, "/legion/v1/claims/fresh-session", map[string]any{"grantId": grant, "issue": issue, "role": string(role)}, false, 5*time.Minute)
	answer := relaunchAnswer{at: at, status: status, body: strings.TrimSpace(string(body))}
	var failure struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(body, &failure)
	answer.code = failure.Code
	w.record("fresh_session %s %s: %s", issue, role, answer)
	return answer
}

// stopClaim retires a claim as `legion claims stop` does.
func (w *w5Rig) stopClaim(token claim.Token) (int, string) {
	status, body := w.apiCall(http.MethodPost, "/legion/v1/operator/claims/"+string(token)+"/stop", nil, true, 2*time.Minute)
	w.record("stop %s: %d %s", token, status, strings.TrimSpace(string(body)))
	return status, string(body)
}

// deliver gives a claim a task as `legion claims deliver` does.
func (w *w5Rig) deliver(token claim.Token, task string) (int, string) {
	status, body := w.apiCall(http.MethodPost, "/legion/v1/operator/claims/"+string(token)+"/deliver", map[string]any{"task": task}, true, time.Minute)
	w.record("deliver to %s: %d %s", token, status, strings.TrimSpace(string(body)))
	return status, string(body)
}

// ---- reading the state ---------------------------------------------------------------------------

// w5View is one claim as GET /legion/v1/state (read_state, and read_record for its issue) and the
// operator's claim listing show it: its state, session, generation, its mark out of reach, its
// budgets and its locator's pod.
type w5View struct {
	found          bool
	state          string
	session        string
	generation     uint64
	since          string
	lastSeen       string
	launchFailures int
	deaths         int
	pod            string
	incarnation    string
	pending        bool
}

func (v w5View) marked() bool { return v.since != "" }

func (v w5View) String() string {
	if !v.found {
		return "not held"
	}
	mark := "no mark"
	if v.marked() {
		mark = "unreachable since " + v.since
		if v.lastSeen != "" {
			mark += " (last seen " + v.lastSeen + ")"
		}
	}
	return fmt.Sprintf("%s gen %d, %s, launch failures %d, deaths %d, pod %s", v.state, v.generation, mark, v.launchFailures, v.deaths, v.pod)
}

// w5Snapshot is one reading of the daemon's state and claims.
type w5Snapshot struct {
	at         time.Time
	views      map[claim.Token]w5View
	controller map[string]any
	state      map[string]any
	err        string
}

// answeredSnapshot is a snapshot whose read of read_state answered: a read that did not answer
// says nothing of any claim, so it is read again, every 10 s for up to 5 minutes, and each read
// that did not answer is recorded. A snapshot still unanswered then is returned as it is, its err
// set, which no clause may take for a claim that is not held.
func (w *w5Rig) answeredSnapshot(label string) w5Snapshot {
	deadline := time.Now().Add(5 * time.Minute)
	for {
		snap := w.snapshot()
		if snap.err == "" || time.Now().After(deadline) {
			return snap
		}
		w.record("%s: read_state did not answer at %s (%s); reading it again", label, clockAt(snap.at), snap.err)
		time.Sleep(10 * time.Second)
	}
}

// snapshot reads read_state's state and the operator's claim listing now.
func (w *w5Rig) snapshot() w5Snapshot {
	snap := w5Snapshot{at: time.Now(), views: map[claim.Token]w5View{}}
	status, body := w.apiCall(http.MethodGet, "/legion/v1/state", nil, false, 30*time.Second)
	if status != http.StatusOK {
		snap.err = fmt.Sprintf("state %d %s", status, truncate(string(body), 200))
		return snap
	}
	if err := json.Unmarshal(body, &snap.state); err != nil {
		snap.err = "decode state: " + err.Error()
		return snap
	}
	if locator, ok := snap.state["controllerLocator"].(map[string]any); ok {
		snap.controller = locator
	}
	issues, _ := snap.state["issues"].(map[string]any)
	for key, raw := range issues {
		issue, _ := raw.(map[string]any)
		if arch, ok := issue["architect"].(map[string]any); ok {
			token, _ := claim.NewToken(w.rig.project, key, claim.RoleArchitect)
			snap.views[token] = viewFromState(arch)
		}
		workers, _ := issue["workers"].(map[string]any)
		for role, rawWorker := range workers {
			worker, _ := rawWorker.(map[string]any)
			token, err := claim.NewToken(w.rig.project, key, claim.Role(role))
			if err != nil {
				continue
			}
			view := viewFromState(worker)
			if inner, ok := worker["claim"].(map[string]any); ok {
				view = viewFromState(inner)
			}
			snap.views[token] = view
		}
	}
	status, body = w.apiCall(http.MethodGet, "/legion/v1/operator/claims", nil, true, 30*time.Second)
	if status == http.StatusOK {
		var listing struct {
			Claims []map[string]any `json:"claims"`
		}
		if json.Unmarshal(body, &listing) == nil {
			for _, held := range listing.Claims {
				token, _ := held["token"].(string)
				view := snap.views[claim.Token(token)]
				view.found = true
				if s, ok := held["state"].(string); ok {
					view.state = s
				}
				if s, ok := held["session"].(string); ok && s != "" {
					view.session = s
				}
				if g, ok := held["generation"].(float64); ok {
					view.generation = uint64(g)
				}
				if budgets, ok := held["budgets"].(map[string]any); ok {
					view.launchFailures = intOf(budgets["launchFailures"])
					view.deaths = intOf(budgets["deaths"])
				}
				if loc, ok := held["locator"].(map[string]any); ok {
					view.incarnation, _ = loc["incarnation"].(string)
					if sandbox, ok := loc["sandbox"].(map[string]any); ok {
						view.pod, _ = sandbox["podUid"].(string)
					}
				}
				if mark, ok := held["unreachable"].(map[string]any); ok && view.since == "" {
					view.since, _ = mark["since"].(string)
					view.lastSeen, _ = mark["lastSeen"].(string)
				}
				_, view.pending = held["pending"]
				snap.views[claim.Token(token)] = view
			}
		}
	}
	return snap
}

func viewFromState(raw map[string]any) w5View {
	v := w5View{found: true}
	v.state, _ = raw["state"].(string)
	v.session, _ = raw["session"].(string)
	if mark, ok := raw["unreachable"].(map[string]any); ok {
		v.since, _ = mark["since"].(string)
		v.lastSeen, _ = mark["lastSeen"].(string)
	}
	_, v.pending = raw["pendingTask"]
	return v
}

func intOf(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case map[string]any:
		return intOf(n["count"])
	}
	return 0
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// controllerMark is the controller's mark as read_state's controller locator shows it, "" none.
func (s w5Snapshot) controllerMark() string {
	if s.controller == nil {
		return ""
	}
	if mark, ok := s.controller["unreachable"].(map[string]any); ok {
		since, _ := mark["since"].(string)
		return since
	}
	return ""
}

// issueRecord is read_record's answer for key: the issue's record from the state, as JSON.
func (s w5Snapshot) issueRecord(key string) string {
	issues, _ := s.state["issues"].(map[string]any)
	encoded, _ := json.Marshal(issues[key])
	return string(encoded)
}

// sampler reads the daemon's state and claims once a minute from its start to the run's end, and
// keeps every reading.
type sampler struct {
	mu    sync.Mutex
	snaps []w5Snapshot
	stop  chan struct{}
	done  chan struct{}
}

// startSampler starts the minute's reading; each reading is written to the results.
func (w *w5Rig) startSampler(names map[claim.Token]string) {
	s := &sampler{stop: make(chan struct{}), done: make(chan struct{})}
	w.sampled = s
	go func() {
		defer close(s.done)
		for {
			snap := w.snapshot()
			s.mu.Lock()
			s.snaps = append(s.snaps, snap)
			s.mu.Unlock()
			w.results.line("sample " + describeSnapshot(snap, names))
			select {
			case <-s.stop:
				return
			case <-time.After(time.Minute):
			}
		}
	}()
	w.root.Cleanup(func() {
		close(s.stop)
		<-s.done
	})
}

// samplesBetween is every reading from from to to.
func (s *sampler) samplesBetween(from, to time.Time) []w5Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []w5Snapshot
	for _, snap := range s.snaps {
		if !snap.at.Before(from) && !snap.at.After(to) {
			out = append(out, snap)
		}
	}
	return out
}

func describeSnapshot(snap w5Snapshot, names map[claim.Token]string) string {
	if snap.err != "" {
		return clockAt(snap.at) + " " + snap.err
	}
	tokens := make([]string, 0, len(snap.views))
	for token := range snap.views {
		tokens = append(tokens, string(token))
	}
	sort.Strings(tokens)
	var b strings.Builder
	b.WriteString(clockAt(snap.at))
	for _, token := range tokens {
		name := names[claim.Token(token)]
		if name == "" {
			name = token
		}
		v := snap.views[claim.Token(token)]
		mark := "-"
		if v.marked() {
			mark = "U@" + v.since
		}
		fmt.Fprintf(&b, " | %s %s g%d %s lf%d d%d", name, v.state, v.generation, mark, v.launchFailures, v.deaths)
	}
	if m := snap.controllerMark(); m != "" {
		b.WriteString(" | controller-locator U@" + m)
	}
	return b.String()
}

// ---- the notices ---------------------------------------------------------------------------------

// w5Notice is one notice as the stream stored it: when, its kind, the claim it names, the claims an
// unreachable-claims list names, and its reason.
type w5Notice struct {
	at     time.Time
	kind   string
	claim  string
	rows   []string
	reason string
	raw    map[string]any
}

func (n w5Notice) String() string {
	if len(n.rows) > 0 {
		return fmt.Sprintf("%s %s %v", clockAt(n.at), n.kind, n.rows)
	}
	return fmt.Sprintf("%s %s %s %q", clockAt(n.at), n.kind, n.claim, truncate(n.reason, 240))
}

// noticesOn is every notice the stream holds on subject, oldest first.
func (w *w5Rig) noticesOn(subject string) []w5Notice {
	var out []w5Notice
	for _, stored := range w.nats.storedOn(subject) {
		found, ok := findNotice(stored.data)
		if !ok {
			continue
		}
		n := w5Notice{at: stored.at, raw: found}
		n.kind, _ = found["kind"].(string)
		n.claim, _ = found["claim"].(string)
		n.reason, _ = found["reason"].(string)
		if rows, ok := found["unreachable"].([]any); ok {
			for _, row := range rows {
				if m, ok := row.(map[string]any); ok {
					c, _ := m["claim"].(string)
					n.rows = append(n.rows, c)
				}
			}
		}
		out = append(out, n)
	}
	return out
}

// controllerNoticesW5 is every notice on the controller's topic.
func (w *w5Rig) controllerNoticesW5() []w5Notice {
	return w.noticesOn(notify.ControllerTopic(w.rig.project))
}

// roleNotices is every notice on token's role topic.
func (w *w5Rig) roleNotices(token claim.Token) []w5Notice {
	return w.noticesOn(notify.RoleTopicPrefix + string(token))
}

// findNotice is noticeIn's walk, answering the whole object that carries a kind.
func findNotice(data []byte) (map[string]any, bool) {
	var value any
	if err := json.Unmarshal(data, &value); err != nil {
		return nil, false
	}
	var find func(v any) (map[string]any, bool)
	find = func(v any) (map[string]any, bool) {
		switch v := v.(type) {
		case map[string]any:
			if kind, ok := v["kind"].(string); ok && kind != "" {
				return v, true
			}
			for _, inner := range v {
				if found, ok := find(inner); ok {
					return found, true
				}
			}
		case []any:
			for _, inner := range v {
				if found, ok := find(inner); ok {
					return found, true
				}
			}
		case string:
			if strings.HasPrefix(strings.TrimSpace(v), "{") {
				var inner any
				if json.Unmarshal([]byte(v), &inner) == nil {
					return find(inner)
				}
			}
		}
		return nil, false
	}
	return find(value)
}

// fleetAlerts is every R6 line the daemon wrote, oldest first.
func (w *w5Rig) fleetAlerts() []loggedRecord {
	return w.logs(func(rec loggedRecord) bool { return strings.HasPrefix(rec.Message, w5FleetAlertPrefix) })
}

// w5Describe is one logged record as a note prints it: its moment, message and attributes.
func w5Describe(rec loggedRecord) string {
	keys := make([]string, 0, len(rec.Attrs))
	for key := range rec.Attrs {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var b strings.Builder
	fmt.Fprintf(&b, "%s %q", clockAt(rec.at), rec.Message)
	for _, key := range keys {
		fmt.Fprintf(&b, " %s=%v", key, rec.Attrs[key])
	}
	return b.String()
}

// w5FirstAt is when the first of records was written, as a note prints it.
func w5FirstAt(records []loggedRecord) string {
	if len(records) == 0 {
		return "never"
	}
	return clockAt(records[0].at)
}

// ---- Envoy's own list ----------------------------------------------------------------------------

// envoySession is one row of the listener's own session list.
type envoySession struct {
	session  string
	lastSeen time.Time
	state    string
}

// envoySessions is the listener's own list, read directly with its unresponsive sessions.
func (w *w5Rig) envoySessions() map[string]envoySession {
	req, err := http.NewRequest(http.MethodGet, w.listener.BaseURL()+"/v1/sessions?include=unresponsive", nil)
	if err != nil {
		return nil
	}
	req.Header.Set("Authorization", "Bearer "+w.listener.Token())
	response, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		return nil
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	var rows []map[string]any
	if json.Unmarshal(body, &rows) != nil {
		return nil
	}
	out := map[string]envoySession{}
	for _, row := range rows {
		s := envoySession{}
		s.session, _ = row["session_id"].(string)
		if ms, ok := row["last_seen"].(float64); ok {
			s.lastSeen = time.UnixMilli(int64(ms))
		}
		s.state, _ = row["state"].(string)
		out[s.session] = s
	}
	return out
}

// ---- the results file ----------------------------------------------------------------------------

// w5Results is the results file the entry script names (w5ResultsEnv), else results.txt in the
// evidence directory: every line written there is stamped with the moment it was written and
// synced, so a kill keeps it.
type w5Results struct {
	mu   sync.Mutex
	file *os.File
	buf  *bufio.Writer
	// prefix is what every line says after its moment: which claims' processes the rig holds
	// stopped at that moment.
	prefix func() string
}

func openW5Results(t *testing.T, evidence string) *w5Results {
	t.Helper()
	path := os.Getenv(w5ResultsEnv)
	if path == "" {
		path = filepath.Join(evidence, "results.txt")
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatalf("open the results file %s: %v", path, err)
	}
	t.Cleanup(func() { _ = file.Close() })
	fmt.Printf("   results: %s\n", path)
	return &w5Results{file: file, buf: bufio.NewWriter(file)}
}

func (r *w5Results) line(text string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	label := ""
	if r.prefix != nil {
		label = r.prefix() + " "
	}
	fmt.Fprintf(r.buf, "%s %s%s\n", time.Now().UTC().Format(time.RFC3339Nano), label, text)
	_ = r.buf.Flush()
	_ = r.file.Sync()
}

// record writes a line to the results and to the step's own output.
func (w *w5Rig) record(format string, args ...any) {
	text := fmt.Sprintf(format, args...)
	w.results.line(text)
	fmt.Printf("   %s\n", text)
}

// check records one clause of the check's Expected: PASS or FAIL, with what showed it. A clause
// that fails fails the step it is in, and the step goes on, so every later clause is read too.
func (w *w5Rig) check(clause string, ok bool, format string, args ...any) bool {
	t := w.t
	t.Helper()
	word := "PASS"
	if !ok {
		word = "FAIL"
	}
	text := fmt.Sprintf("CHECK %s %s: %s", word, clause, fmt.Sprintf(format, args...))
	w.results.line(text)
	fmt.Printf("   %s\n", text)
	if !ok {
		t.Errorf("%s", text)
	}
	return ok
}

// copyLogs copies every log and session transcript under from into to, keeping their paths, up to
// 200 MiB in all.
func copyLogs(from, to string) {
	var copied int64
	_ = filepath.WalkDir(from, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".log") && !strings.HasSuffix(path, ".jsonl") {
			return nil
		}
		info, err := entry.Info()
		if err != nil || copied+info.Size() > 200<<20 {
			return nil
		}
		rel, err := filepath.Rel(from, path)
		if err != nil {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		target := filepath.Join(to, rel)
		if os.MkdirAll(filepath.Dir(target), 0o755) == nil && os.WriteFile(target, body, 0o644) == nil {
			copied += info.Size()
		}
		return nil
	})
}
