//go:build e2e

// The harness of this package's live proofs of one issue on an open pull request, whose roles the
// fake runtime launches and the driver plays over the daemon's worker stream and HTTP routes:
// behind_entry_fence_live_test.go, conflict_no_takeover_live_test.go and
// phase_entry_converge_live_test.go, each built with `-tags e2e` by the script under scripts/e2e/
// that bears its name. Each driver embeds liveIssueProof, liveProof (live_proof_test.go) grown for
// that one issue: the issue's seed and the daemon's boot over the fake runtime; the reads of a
// claim's token, its launch and the issue's state; each role's agent, as the registered session it
// is over HTTP (liveSession) and as the turns a step plays on the shim (liveClaimant); GitHub's
// REST API for the pull request at the head the driver last pushed (liveGitHub); and the
// implementer's push, GitHub's synchronize and a push onto the base. firstLine, the cut of a
// prompt a note prints, is here too, for these drivers and the other e2e ones. What one proof
// alone drives stays in its driver.
package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/githubtest"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
)

// liveIssueProof is what a live proof of one issue on an open pull request holds beyond
// liveProof: the fake runtime that launches every claim and the record of the worker stream it was
// built over, the issue and its pull request, GitHub's stand-in for that pull request, and the
// base pushes the proof has published.
type liveIssueProof struct {
	liveProof
	rt    *fake.Runtime
	built *built
	// key is the issue, its own tree, and number its pull request's — the run's own, since the
	// store a pod re-runs the proof against holds every earlier run's pull requests and a fact
	// names a pull request by its repository and number.
	key    string
	number int
	// base is the pull request's base branch, the repository's default, and title the issue's
	// title, which GitHub's events of its pull request carry too.
	base, title string
	// github is GitHub as this proof's daemon reads it, once serveGitHub has started it.
	github *liveGitHub
	// seq numbers the base pushes publishBasePush publishes, so each is its own envelope with its
	// own before and after commits rather than a second report of the one before it.
	seq int64
}

// newLiveIssueProof readies a proof of one issue over the broker newLiveProof reads, skipping as
// it does and naming script: the issue is the run's project's first, titled title, and its pull
// request, on base, is numbered from the run's own suffix.
func newLiveIssueProof(t *testing.T, script, base, title string) liveIssueProof {
	t.Helper()
	proof := newLiveProof(t, script)
	number, err := strconv.ParseInt(randomSuffix(t)[:6], 16, 64)
	if err != nil {
		t.Fatal(err)
	}
	return liveIssueProof{
		liveProof: proof, rt: fake.NewRuntime(), built: &built{},
		key: proof.cfg.Project + "-1", number: int(number) + 1, base: base, title: title,
	}
}

// seedImplementing records the issue as the daemon would hold it at the moment its implementer
// began work: a root handed to Legion, implementing, holding its tree's admission slot, its
// lifecycle open under the workflow, on an open pull request at head, and its implementer's claim
// as a launch a previous daemon began and never recorded, serving run 1, which the boot
// relaunches. That last one is how a proof reaches a live implementer session without an
// operator's spawn. No phases row is written; each driver says why its proof wants none.
func (p *liveIssueProof) seedImplementing(head string) {
	t := p.t
	t.Helper()
	ctx := context.Background()
	p.board.show(p.key, "in_progress")
	p.write("the issue, its pull request and its slot", func(tx pgx.Tx) error {
		if err := p.records.PutIssue(ctx, tx, record.Issue{
			Key: p.key, Repo: liveRepo, DefaultBranch: p.base, GitHubRepo: liveRepo, Tree: p.key, Project: p.cfg.Project,
			Title: p.title, Phase: phase.Implementing, Generation: 1, Status: "in_progress",
			DispatchStatus: "in_progress", Rank: "U", LastDispatchSeq: 1, HandedOver: true,
		}); err != nil {
			return err
		}
		if err := p.records.PutPullRequest(ctx, tx, record.PullRequest{
			State: record.PullRequestOpen, Issue: p.key, Repo: liveRepo, Number: p.number, Branch: "legion/" + p.key,
			HeadSHA: head, Base: p.base, Failing: []string{},
		}); err != nil {
			return err
		}
		return p.admit(ctx, tx, p.key)
	})
	if _, err := p.st.OpenTreeLifecycle(ctx, p.project, p.key, treelifecycle.AuthorityWorkflow); err != nil {
		t.Fatalf("open the lifecycle of %s: %v", p.key, err)
	}
	implementer := p.token(claim.RoleImplementer)
	if _, err := p.st.AdmitClaim(ctx, supervise.Claim{
		Token: implementer, Project: p.project, Tree: p.key, Issue: p.key, Role: claim.RoleImplementer,
		Generation: 1, State: supervise.StateLaunching, BootTokenHash: supervise.HashBootToken("interrupted-" + randomSuffix(t)),
		ServingGeneration: 1,
	}); err != nil {
		t.Fatalf("record the implementer's unfinished launch: %v", err)
	}
	// Every role the proof launches needs its prompt on disk, as the daemon's own launch writes it.
	for _, role := range []claim.Role{claim.RoleImplementer, claim.RoleTester, claim.RoleReviewer} {
		writePrompt(t, p.cfg, p.token(role))
	}
}

// write runs body in one transaction against the daemon's own database, failing the test with what
// it was seeding on an error.
func (p *liveIssueProof) write(what string, body func(tx pgx.Tx) error) {
	p.t.Helper()
	if err := p.st.Tx(context.Background(), body); err != nil {
		p.t.Fatalf("seed %s: %v", what, err)
	}
}

// boot starts the whole daemon on the seeded store through this package's whole-daemon harness
// (startDaemonWithin), with GitHub the stand-in serveGitHub started (the proof's github) and the
// two Apps' tokens workflowTokenRecorder's placeholders. The runtime provisions each claim's
// workspace itself (fake.Runtime's InPod), so a start reaches the fake's launch without the daemon
// cloning acme/widgets from github.com, which the tmux answer would have it try. tune, when not
// nil, sets what one proof's boot needs beyond that.
func (p *liveIssueProof) boot(tune func(*overrides)) {
	p.t.Helper()
	p.rt.InPod = true
	o := fakeRuntime(p.rt, p.built)
	o.workflowTokens = &workflowTokenRecorder{}
	o.githubAPI = p.github.url
	if tune != nil {
		tune(&o)
	}
	p.d = startDaemonWithin(p.t, p.cfg, o, p.log(), 60*time.Second)
}

// token is the claim token of role on the proof's issue, as the daemon spells it.
func (p *liveIssueProof) token(role claim.Role) claim.Token {
	p.t.Helper()
	token, err := claim.NewToken(p.project, p.key, role)
	if err != nil {
		p.t.Fatal(err)
	}
	return token
}

// awaitLaunch waits for the running daemon to launch token's claim and returns that launch.
func (p *liveIssueProof) awaitLaunch(token claim.Token) runtime.SpawnSpec {
	p.t.Helper()
	testwait.Eventually(p.t, "the launch of "+string(token), func() bool { return p.launched(token) })
	return lastLaunch(p.t, p.rt, token)
}

// launched is whether the fake runtime has been asked to start a process for token.
func (p *liveIssueProof) launched(token claim.Token) bool {
	for _, call := range p.rt.Calls() {
		if (call.Method == "Spawn" || call.Method == "Resume") && call.Spec.Claim == token {
			return true
		}
	}
	return false
}

// waitFor polls the state until want is true of the issue, and returns what it last saw.
func (p *liveIssueProof) waitFor(what string, want func(api.Issue) bool) api.Issue {
	p.t.Helper()
	var last api.Issue
	testwait.Eventually(p.t, what, func() bool {
		last = p.issue(p.key)
		return want(last)
	})
	return last
}

// push is the implementer's push of to onto from, subject the pushed commit's: GitHub's own copy
// of the pull request moves first, as it does on github.com, then the push and the synchronize
// Envoy relays (publishPush, publishSynchronize), and it returns once the head the daemon's record
// holds is the pushed one.
func (p *liveIssueProof) push(from, to, subject string) {
	p.t.Helper()
	p.github.head(to)
	p.publishPush(from, to, subject)
	p.publishSynchronize(to)
	p.waitFor("the pull request at the pushed head "+to, func(got api.Issue) bool {
		return got.PullRequest != nil && got.PullRequest.Head == to
	})
}

// publishPush publishes the implementer's push of to onto from on the issue's branch, changing
// code, as Envoy relays a GitHub push (intake's decodePush reads it). headSubject is the subject
// of the commit pushed, which the envelope carries and the daemon does not read.
func (p *liveIssueProof) publishPush(from, to, headSubject string) {
	p.t.Helper()
	payload, err := json.Marshal(map[string]string{
		"kind": "push", "repo": liveRepo, "ref": "refs/heads/legion/" + p.key, "before": from, "after": to,
		"changed_paths": "src/widget.go", "changed_paths_truncated": "false", "forced": "false",
		"pusher": "legion-implement[bot]", "commit_count": "1", "head_subject": headSubject,
	})
	if err != nil {
		p.t.Fatalf("encode the push payload: %v", err)
	}
	subject := fmt.Sprintf("notifications.github.%s.push.branch.legion/%s", p.subjectRepo(), p.key)
	p.publish(subject, "github", fmt.Sprintf("push-%d-%s", p.number, to), string(payload))
}

// publishSynchronize publishes GitHub's synchronize of the pull request to head, as Envoy relays a
// GitHub pull-request event (intake's decodePullRequest reads it).
func (p *liveIssueProof) publishSynchronize(head string) {
	p.t.Helper()
	payload, err := json.Marshal(map[string]string{
		"action": "synchronize", "kind": "pr", "repo": liveRepo, "number": strconv.Itoa(p.number),
		"head_sha": head, "base_ref": p.base, "head_ref": "legion/" + p.key, "title": p.title,
		"url":        "https://github.com/" + liveRepo + "/pull/" + strconv.Itoa(p.number),
		"updated_at": time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		p.t.Fatalf("encode the synchronize payload: %v", err)
	}
	subject := fmt.Sprintf("notifications.github.%s.pr.%d", p.subjectRepo(), p.number)
	p.publish(subject, "github", fmt.Sprintf("pr-%d-synchronize-%s", p.number, head), string(payload))
}

// publishBasePush publishes another pull request's merge onto the issue's base branch, as Envoy
// relays a GitHub push (intake's decodePush reads it as a BasePush, the notice that the base
// moved). Each is the next of seq, with before and after commits of its own.
func (p *liveIssueProof) publishBasePush() {
	p.t.Helper()
	p.seq++
	payload, err := json.Marshal(map[string]string{
		"kind": "push", "repo": liveRepo, "ref": "refs/heads/" + p.base,
		"before": fmt.Sprintf("base-%d", p.seq-1), "after": fmt.Sprintf("base-%d", p.seq),
		"changed_paths": "src/widget.go", "changed_paths_truncated": "false", "forced": "false",
		"pusher": "someone-else", "commit_count": "1", "head_subject": "another pull request merged",
	})
	if err != nil {
		p.t.Fatalf("encode the base push payload: %v", err)
	}
	subject := fmt.Sprintf("notifications.github.%s.push.branch.%s", p.subjectRepo(), p.base)
	p.publish(subject, "github", fmt.Sprintf("base-push-%d-%d", p.number, p.seq), string(payload))
}

// liveSession is one role's claim as its agent reaches the daemon's HTTP routes: the shim that
// dialled the worker stream with the boot token of the launch the fake runtime recorded, and the
// registration that exchanged that token for the session's capability secret. How the agent reads
// that connection is liveClaimant's, or a driver's own; what it does over HTTP is here.
type liveSession struct {
	p       *liveIssueProof
	token   claim.Token
	role    claim.Role
	session string
	secret  string
	sh      *shim
	// generation is the launch's, which the readiness names.
	generation uint64
}

// register brings the launched claim of role to a registered session, as its pane's shim and its
// agent do: it waits for the launch, dials the worker stream with the launch's boot token and
// registers the session. ready then reports it ready.
func (p *liveIssueProof) register(role claim.Role, session string) *liveSession {
	t := p.t
	t.Helper()
	token := p.token(role)
	launch := p.awaitLaunch(token)
	p.built.mu.Lock()
	address := p.built.address
	p.built.mu.Unlock()
	sh := dialShim(t, address, launch.BootToken)
	status, body := p.d.request(http.MethodPost, "/legion/v1/claims/register", claim.RegisterRequest{
		BootToken: launch.BootToken, SessionID: session, OmpSessionFile: "/sessions/" + session + ".jsonl",
		AgentID: session, PluginContract: api.DaemonAPIVersion,
	}, false)
	if status != http.StatusOK {
		t.Fatalf("register %s = %d %s, want 200", token, status, body)
	}
	var registered claim.RegisterResponse
	if err := json.Unmarshal(body, &registered); err != nil {
		t.Fatalf("decode the registration of %s: %v", token, err)
	}
	return &liveSession{p: p, token: token, role: role, session: session, secret: registered.Secret, sh: sh,
		generation: launch.Generation}
}

// ready reports the registered session ready on the launch it registered from.
func (c *liveSession) ready() {
	t := c.p.t
	t.Helper()
	if status, body := c.p.d.request(http.MethodPost, "/legion/v1/claims/ready", claim.ReadyRequest{
		ClaimToken: c.token, SessionID: c.session, Secret: c.secret, Generation: c.generation,
	}, false); status != http.StatusNoContent {
		t.Fatalf("ready %s = %d %s, want 204", c.token, status, body)
	}
}

// grant is a capability grant minted for this session now, as its `legion` tool mints one per
// operation.
func (c *liveSession) grant() string {
	t := c.p.t
	t.Helper()
	status, body := c.p.d.request(http.MethodPost, "/legion/v1/grants",
		api.GrantRequest{SessionID: c.session, Secret: c.secret, Tree: c.p.key, Issue: c.p.key}, false)
	if status != http.StatusOK {
		t.Fatalf("the grant of %s = %d %s, want 200", c.token, status, body)
	}
	var granted api.GrantResponse
	if err := json.Unmarshal(body, &granted); err != nil {
		t.Fatalf("decode the grant of %s: %v", c.token, err)
	}
	return granted.GrantID
}

// handoff seeds on the run's Dispatch board version 1 of this claim's record of phase word,
// written by this claim's own registered session, as `dispatch handoff-write` leaves it, and
// answers its id. A completion of a file-backed phase names a record, and the daemon reads it
// back from Dispatch and refuses one another session wrote (api/handoff.go's handoffRecord), so
// a completion that names one writes its own first, as a worker does.
func (c *liveSession) handoff(word string) string {
	return c.p.board.handoff(c.p.key, word, c.session)
}

// complete reports this role's completion of the phase it works, from the turn it is in, naming
// handoffID, a handoff record of that phase this claim wrote, and returns the status and the
// refusal the daemon answered. Every record the board seeds is version 1 of its own id, so each
// completion that names one names version 1 and a record of its own, and no two are the same one;
// a completion with handoffID "" names no record and no version.
func (c *liveSession) complete(summary, commit, verdict, handoffID string) (int, api.Failure) {
	t := c.p.t
	t.Helper()
	req := api.HandoffCompleteRequest{GrantID: c.grant(), Summary: summary, Commit: commit, Verdict: verdict, HandoffID: handoffID}
	if handoffID != "" {
		req.HandoffVersion = 1
	}
	status, body := c.p.d.request(http.MethodPost, "/legion/v1/handoff/complete", req, false)
	if status == http.StatusOK {
		return status, api.Failure{}
	}
	var failure api.Failure
	if err := json.Unmarshal(body, &failure); err != nil {
		t.Fatalf("decode the %s's refused completion %s: %v", c.role, body, err)
	}
	return status, failure
}

// liveClaimant is a role's claim whose agent the step itself plays: its registered session, and
// the prompt whose turn is running. Each read of the connection is the step's own, on the step's
// own goroutine, so nothing answers the daemon between a step's reads.
type liveClaimant struct {
	*liveSession
	// turn is the id of the prompt the running turn answers, "" for a turn of the agent's own.
	turn string
}

// claimant brings the launched claim of role to ready, as its pane's shim and its agent do: it
// waits for the launch, dials the worker stream with the launch's boot token, registers the
// session and reports it ready.
func (p *liveIssueProof) claimant(role claim.Role, session string) *liveClaimant {
	p.t.Helper()
	c := &liveClaimant{liveSession: p.register(role, session)}
	c.ready()
	return c
}

// takeTask is the agent taking the daemon's next task: it reads the prompt the daemon sends,
// acknowledges it as Oh My Pi does and starts the turn that works it, and returns once the daemon
// has recorded the claim working on that delivery.
func (c *liveClaimant) takeTask(want string) shimwire.Prompt {
	t := c.p.t
	t.Helper()
	prompt := c.sh.prompt()
	if !strings.Contains(prompt.Message, want) {
		t.Fatalf("the %s was prompted %q, want a task saying %q", c.role, prompt.Message, want)
	}
	c.sh.send(shimwire.Response{ID: prompt.ID, Command: shimwire.TypePrompt, Success: true})
	c.startTurn(prompt.ID)
	return prompt
}

// startTurn is the agent beginning a turn, on the prompt id when the daemon sent one and on
// nothing when the turn is its own. It returns once the daemon has recorded the claim working.
func (c *liveClaimant) startTurn(prompt string) {
	t := c.p.t
	t.Helper()
	c.sh.send(shimwire.AgentStart{})
	c.turn = prompt
	testwait.Eventually(t, fmt.Sprintf("the %s's turn running", c.role), func() bool {
		return c.p.d.claim(c.token).State == string(supervise.StateWorking)
	})
}

// endTurn is the agent finishing its turn: the agent_end, and behind it the prompt's own result,
// which is what names the prompt the turn belonged to (shimwire.PromptResult) and retires the
// task. It returns once the daemon has recorded the claim out of its turn.
func (c *liveClaimant) endTurn() {
	t := c.p.t
	t.Helper()
	c.sh.send(shimwire.AgentEnd{})
	if c.turn != "" {
		c.sh.send(shimwire.PromptResult{ID: c.turn, AgentInvoked: true})
	}
	c.turn = ""
	testwait.Eventually(t, fmt.Sprintf("the %s out of its turn", c.role), func() bool {
		return c.p.d.claim(c.token).State == string(supervise.StateIdle)
	})
}

// liveGitHub is GitHub's REST API for acme/widgets as a live proof's daemon reads it: the one
// pull request at the head the driver last pushed, with its base and the `mergeable` the driver
// last set, true until it sets one (conflicts; readPullRequest), the `mergeable_state` a proof
// sets (mergeState), and its reviews as none (readReviews). It also answers what a launch and a
// disarm read: any repository, with the proof's base as its default branch, and a GraphQL root
// that knows no pull request. Anything else is answered 404 unless the proof's also answers it, so
// a read the proof did not foresee fails the step that waited on it rather than reaching
// github.com. A liveIssueProof starts it with serveGitHub; late_completion_live_test.go, whose
// panes are real, starts one of its own.
type liveGitHub struct {
	t      *testing.T
	url    string
	key    string
	number int
	base   string
	// mergeState, when set, is the pull request's `mergeable_state`, which GitHub computes beside
	// `mergeable` ("clean" for one it would merge); unset, the answer carries none.
	mergeState string
	// also is what one proof's stand-in answers beyond that, each answer by its path under
	// /repos/acme/widgets.
	also map[string]any
	// hold, when not nil, runs before each read of the pull request itself, outside the lock
	// below, since a handler waiting under that lock would stop every other answer this stand-in
	// gives.
	hold func()
	mu   sync.Mutex
	// sha is the pull request's head on GitHub, which a push moves.
	sha string
	// conflicting is what GitHub's lazily computed `mergeable` answers: false once the base has
	// moved past the head, which is the one answer of a pull request that moves an issue.
	conflicting bool
}

// serveGitHub starts g as GitHub for this proof's pull request, at the head g's sha names and with
// what its also and hold add, and holds it as the proof's github.
func (p *liveIssueProof) serveGitHub(g *liveGitHub) {
	p.t.Helper()
	g.t, g.key, g.number, g.base = p.t, p.key, p.number, p.base
	g.url = githubtest.Server(p.t, githubtest.Spec{Handle: g.ServeHTTP})
	p.github = g
}

// head moves the pull request to sha, as a push does on GitHub.
func (g *liveGitHub) head(sha string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.sha = sha
}

// conflicts makes GitHub answer `mergeable: false` for the head, as it does once the base has
// moved past it, and answer it mergeable again once the implementer has merged the base forward.
func (g *liveGitHub) conflicts(yes bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.conflicting = yes
}

func (g *liveGitHub) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if g.hold != nil && strings.HasSuffix(req.URL.Path, fmt.Sprintf("/pulls/%d", g.number)) {
		g.hold()
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if req.Method == http.MethodPost && req.URL.Path == "/graphql" {
		_, _ = io.WriteString(w, `{"data":{"repository":{"pullRequest":null}}}`)
		return
	}
	if req.Method == http.MethodGet && g.answer(w, req.URL.Path) {
		return
	}
	w.WriteHeader(http.StatusNotFound)
	_, _ = io.WriteString(w, `{"message":"Not Found"}`)
}

// answer writes the stand-in's answer to a GET of path, and says whether it had one.
func (g *liveGitHub) answer(w http.ResponseWriter, path string) bool {
	rest, ours := strings.CutPrefix(path, "/repos/"+liveRepo)
	if !ours {
		// Any other repository is answered as the launch reads it: a name and its default branch.
		other, isRepository := strings.CutPrefix(path, "/repos/")
		if parts := strings.Split(other, "/"); isRepository && len(parts) == 2 {
			g.write(w, map[string]any{"full_name": other, "default_branch": g.base})
			return true
		}
		return false
	}
	switch {
	case rest == "":
		g.write(w, map[string]any{"full_name": liveRepo, "default_branch": g.base})
	case rest == fmt.Sprintf("/pulls/%d/reviews", g.number):
		g.write(w, []any{})
	case rest == fmt.Sprintf("/pulls/%d", g.number):
		pull := map[string]any{
			"number": g.number, "state": "open", "mergeable": !g.conflicting,
			"base": map[string]string{"ref": g.base},
			"head": map[string]string{"ref": "legion/" + g.key, "sha": g.sha},
		}
		if g.mergeState != "" {
			pull["mergeable_state"] = g.mergeState
		}
		g.write(w, pull)
	default:
		answer, answered := g.also[rest]
		if !answered {
			return false
		}
		g.write(w, answer)
	}
	return true
}

// write encodes value as the answer.
func (g *liveGitHub) write(w http.ResponseWriter, value any) {
	if err := json.NewEncoder(w).Encode(value); err != nil {
		g.t.Errorf("write the GitHub stand-in's answer: %v", err)
	}
}

// firstLine is the first line of a task, for a note that should not carry a whole prompt.
func firstLine(task string) string {
	line, _, _ := strings.Cut(task, "\n")
	if len(line) > 120 {
		line = line[:120] + "…"
	}
	return line
}
