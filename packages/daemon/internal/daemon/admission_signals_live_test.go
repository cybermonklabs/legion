//go:build e2e

// The live proof's driver for dispatch://INFRA-5899's check c-live: a person turns admission by
// signals on and sets each limit while the daemon runs, through the built `legion` binary, and the
// daemon keeps each change. With the rule on and the merge queue's wait over its limit, a P0 issue
// starts as it does with the rule off and the two issues below P0 wait, the state names the
// signal that holds them with its value and its limit, and the daemon logs each start the signal
// held. A lower limit keeps the first of them waiting at its own pass, which is the proof's
// negative control, and a limit raised above the signal's value starts it with no restart, while
// the second stays behind the Dispatch link that holds it. A restart over the same store reads back
// the switch, the limit, who set them, why and the record of changes. A limit outside its signal's
// range is refused with its reason and changes nothing. A limit cleared while the rule is on takes
// that limit alone away, the other still holding. With the rule off again the cap starts the
// second issue once its link is gone, though the queue still reads over its limit.
// scripts/e2e/admission-signals.sh builds this driver with `-tags e2e` and owns the nats-server
// and the PostgreSQL it runs against, through the run lib/live-daemon-proof.sh makes around every
// whole-daemon live proof.
//
// The daemon is the whole daemon: run, the function `legion start` calls, through this package's
// whole-daemon harness (startDaemonWithin), so the proof exercises the real configuration load,
// the real store and its migrations, the real HTTP API, the real claim supervision, the real
// openWorkflow with its live signals and their timer, the real JetStream consumers, the real
// boot reads of GitHub's merge queue and of Dispatch's open asks, and the real outbox lanes. Each
// change of admission by signals is the operator's `legion project signals` of the binary this
// checkout builds, and each step is judged by that binary's `legion state --json` and by the
// daemon's own log records. The issues enter only by the transports production has: Dispatch's
// envelopes on the ENVOY_NOTIFICATIONS stream, read with Dispatch's own view of each issue.
//
// The queue's wait is the one signal the proof puts over a limit, from a recorded source and never
// from a value: GitHub's timeline of cybermonklabs/legion#4910 as testdata/queue records it
// (added, removed, added again two seconds later, then removed and merged), served by the GitHub
// stand-in with every time moved by one offset so that its last event is 20 minutes before the
// run, which keeps its two waits, 5h47m55s and 1h07m32s, inside the six hours the queue reads. The
// store is seeded as an earlier boot of this release leaves it: that boot began keeping the merge
// queue 8 hours before the run, so the six-hour window is whole, and it kept the pull request's
// first entry into the queue, so the store holds an open wait. The proof's boot then reads the
// pull request's timeline, as every boot reads the timeline of each open wait the queue no longer
// shows (readQueueOnce), and keeps what it shows. The queue reads the median of the two waits,
// 3.46 hours.
//
// What the proof stands in for lies outside the change. The broker is a native nats-server the
// script starts and names in LEGION_E2E_NATS_URL. Dispatch is liveDispatch, the board the
// package's whole-daemon live proofs share (dispatch_board_test.go), fronted by signalsDispatch,
// which adds to each read of an issue the priority and the blocked_by links the driver's last
// event of it carried, since the daemon prepares a todo event of an unrecorded issue from
// Dispatch's own view of it, and which refuses and records any write of an admission reading: the
// daemon calls no route of Dispatch for one (S6), so the proof holds that none came. Envoy's
// listener is the package's noticeListener. GitHub is admissionSignalsGitHub: the recorded
// timeline, a merge queue of no entry on each branch, each repository's default branch, and each
// issue branch the workflow creates. No agent runs: every claim is launched by the package's fake
// runtime, which provisions each claim's workspace in its own pod, and the daemon's supervision
// timers never fire (fakeRuntime's stillClock), so each started issue's claim stays launching.
package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/admit"
	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/githubtest"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/signals"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

const (
	// admissionSignalsCap is the cap the proof runs under: room for every issue it starts, so no
	// start waits for the cap and every wait it reads is the queue's or the Dispatch link's.
	admissionSignalsCap = 4
	// admissionSignalsQueueRepo and admissionSignalsQueuePull are the pull request whose recorded
	// timeline (admissionSignalsTimeline) puts the merge queue over its limit.
	admissionSignalsQueueRepo = "cybermonklabs/legion"
	admissionSignalsQueuePull = 4910
	admissionSignalsTimeline  = "timeline-4910.json"
	// admissionSignalsQueueWait is the queue's wait those events give: the median of the two waits
	// the timeline records, 5h47m55s and 1h07m32s, which is their mean, 3h27m43.5s.
	admissionSignalsQueueWait = (5*time.Hour + 47*time.Minute + 55*time.Second + time.Hour + 7*time.Minute + 32*time.Second) / 2
	// admissionSignalsLastEvent is how long before the run the timeline's last event stands once
	// its times are moved, which keeps both of its exits inside the six hours the queue reads.
	admissionSignalsLastEvent = 20 * time.Minute
	// admissionSignalsKeptSince is how long before the run the earlier boot the store is seeded as
	// began keeping the merge queue: more than the six hours the queue's span needs.
	admissionSignalsKeptSince = 8 * time.Hour
)

// The limits of the merge queue's wait the proof sets, in hours: one and two under the queue's
// 3.46, so the queue holds; four above it, so it does not; and 200, outside the queue's range,
// which tops out at 168. The kills take a whole number, which 2.5 is not. The provider's one part
// is its headroom, so a limit on its refusals, 3, is refused whatever its value.
const (
	queueLimitLow        = 1.0
	queueLimitLower      = 2.0
	queueLimitRaised     = 4.0
	queueLimitRefused    = 200.0
	killsLimit           = 5.0
	killsLimitRefused    = 2.5
	refusalsLimitRefused = 3.0
)

// admissionSignalsLive is one run of the proof: the booted daemon, the `legion` binary it is
// driven with, the Dispatch front and the GitHub stand-in it reads, its own log as a step reads it
// back, and the run's issues.
type admissionSignalsLive struct {
	liveProof
	front  *signalsDispatch
	github string
	// daemon is the daemon's own log, kept for the steps and written to the evidence file the
	// script names, across both boots.
	daemon *recordingLog
	// legion is the `legion` binary this checkout builds, the operator's command line.
	legion string
	// p0 is the P0 issue; first and second the two issues below it, P1 and P2; blocker the
	// Dispatch issue the second waits on until a person takes the link away.
	p0, first, second, blocker string
	// seq is the Dispatch sequence the next envelope the driver publishes carries.
	seq int64
	// evidence is the run's evidence directory, where each state a step was judged on is written,
	// "" where the script names none.
	evidence string
	states   int
	// boots is the daemon's boot count at the first read, which a restart moves by one.
	boots int
}

// TestAdmissionSignalsLive drives the nine steps of the proof against one daemon, restarted once
// over the same store, in the order c-live gives them.
func TestAdmissionSignalsLive(t *testing.T) {
	r := newAdmissionSignalsLive(t)
	r.step("fresh-state-off-with-no-limits", r.freshStateOffWithNoLimits)
	r.step("limit-and-switch-before-the-candidates", r.limitAndSwitchBeforeTheCandidates)
	r.step("the-p0-starts-and-the-others-wait", r.theP0StartsAndTheOthersWait)
	r.step("negative-control-a-lower-limit-keeps-the-candidate-waiting", r.aLowerLimitKeepsTheCandidateWaiting)
	r.step("the-raised-limit-starts-the-candidate", r.theRaisedLimitStartsTheCandidate)
	r.step("a-restart-keeps-the-settings-and-the-record", r.aRestartKeepsTheSettingsAndTheRecord)
	r.step("a-limit-outside-its-range-is-refused", r.aLimitOutsideItsRangeIsRefused)
	r.step("a-limit-cleared-while-another-holds", r.aLimitClearedWhileAnotherHolds)
	r.step("off-lets-the-cap-start-the-held-candidate", r.offLetsTheCapStartTheHeldCandidate)
}

// newAdmissionSignalsLive builds the `legion` binary, moves the recorded timeline to the run's
// clock, seeds the store as an earlier boot of this release leaves it, and boots the daemon.
func newAdmissionSignalsLive(t *testing.T) *admissionSignalsLive {
	t.Helper()
	proof := newLiveProof(t, "scripts/e2e/admission-signals.sh")
	proof.cfg.AdmissionCap = admissionSignalsCap
	root := proofCheckoutRoot(t)
	r := &admissionSignalsLive{
		liveProof: proof,
		p0:        proof.cfg.Project + "-1", first: proof.cfg.Project + "-2", second: proof.cfg.Project + "-3", blocker: proof.cfg.Project + "-4",
	}
	if log := os.Getenv(liveDaemonLogEnv); log != "" {
		r.evidence = filepath.Dir(log)
	}
	r.legion = buildProofLegion(t, root, t.TempDir())
	r.note("built legion from %s", filepath.Join(root, "packages", "daemon"))

	recorded, err := os.ReadFile(filepath.Join(root, "packages", "daemon", "internal", "daemon", "testdata", "queue", admissionSignalsTimeline))
	if err != nil {
		t.Fatalf("read the recorded timeline: %v", err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	timeline := shiftTimeline(t, recorded, now.Add(-admissionSignalsLastEvent))
	var items []json.RawMessage
	if err := json.Unmarshal(timeline, &items); err != nil {
		t.Fatalf("decode the moved timeline: %v", err)
	}
	events, err := parseQueueTimeline(admissionSignalsQueuePull, items)
	if err != nil || len(events) == 0 {
		t.Fatalf("read the moved timeline's queue events = %v, %v; want the recorded events", events, err)
	}
	for _, event := range events {
		r.note("served timeline of %s#%d: event %d at %s, enters %t", event.Repo, event.PullRequest, event.ID, event.At.Format(time.RFC3339), event.Enters)
	}

	// The board lists what it holds, which at the first boot is the blocker alone, an issue no
	// one handed to Legion, so that boot's reconcile admits nothing behind the driver's back.
	proof.board.mu.Lock()
	proof.board.lists = true
	proof.board.mu.Unlock()
	r.front = &signalsDispatch{board: proof.board, views: map[string]issueView{}}
	front := httptest.NewServer(r.front)
	t.Cleanup(front.Close)
	r.cfg.DispatchURL = front.URL
	r.github = admissionSignalsGitHub(t, timeline)

	kept := now.Add(-admissionSignalsKeptSince)
	if err := store.Transact(context.Background(), r.st.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		if err := r.records.BeginSignalInput(ctx, tx, r.cfg.Project, signals.Queue, kept); err != nil {
			return err
		}
		return r.records.KeepQueueEvents(ctx, tx, r.cfg.Project, events[:1], now)
	}); err != nil {
		t.Fatalf("seed the earlier boot's merge queue: %v", err)
	}
	r.note("seeded: the daemon keeps the merge queue since %s, and holds the entry of %s#%d at %s, an open wait",
		kept.Format(time.RFC3339), events[0].Repo, events[0].PullRequest, events[0].At.Format(time.RFC3339))
	r.board.place(r.blocker, "in_progress", "", false)
	r.daemon = newRecordingLog(r.log())
	r.boot()
	return r
}

// boot starts the daemon over the run's configuration and store with a fresh fake runtime: the
// first boot, and the restart, which rebinds the API port first.
func (r *admissionSignalsLive) boot() {
	r.t.Helper()
	rt := fake.NewRuntime()
	// The runtime provisions each claim's workspace itself, so a launch reaches the fake without
	// the daemon cloning acme/widgets from github.com.
	rt.InPod = true
	o := fakeRuntime(rt, &built{})
	o.workflowTokens = &workflowTokenRecorder{}
	o.githubAPI = r.github
	r.d = startDaemonWithin(r.t, r.cfg, o, slog.New(r.daemon), 60*time.Second)
}

// shiftTimeline is the recorded timeline with every event's created_at moved by one offset, so
// that its latest event stands at last: the events, their order, their ids and the spans between
// them are the recording's, and only the clock they are read on moves.
func shiftTimeline(t *testing.T, recorded []byte, last time.Time) []byte {
	t.Helper()
	var items []map[string]any
	if err := json.Unmarshal(recorded, &items); err != nil {
		t.Fatalf("decode the recorded timeline: %v", err)
	}
	var latest time.Time
	for _, item := range items {
		if created, ok := item["created_at"].(string); ok {
			at, err := time.Parse(time.RFC3339, created)
			if err != nil {
				t.Fatalf("read created_at %q of the recorded timeline: %v", created, err)
			}
			if at.After(latest) {
				latest = at
			}
		}
	}
	offset := last.Sub(latest)
	for _, item := range items {
		if created, ok := item["created_at"].(string); ok {
			at, _ := time.Parse(time.RFC3339, created)
			item["created_at"] = at.Add(offset).UTC().Format(time.RFC3339)
		}
	}
	moved, err := json.Marshal(items)
	if err != nil {
		t.Fatalf("encode the moved timeline: %v", err)
	}
	return moved
}

// admissionSignalsGitHub is GitHub as this proof's daemon reads it: the timeline of the queue's
// pull request, as recorded and moved to the run's clock; a merge queue of no entry on every
// branch the boot reads, which GitHub answers as null; no pull request; each repository with
// default branch main at one commit; and each issue branch the workflow creates there taken.
// Nothing else is answered, and nothing reaches github.com.
func admissionSignalsGitHub(t *testing.T, timeline []byte) string {
	t.Helper()
	return githubtest.Server(t, githubtest.Spec{Handle: func(w http.ResponseWriter, r *http.Request) {
		rest, isRepository := strings.CutPrefix(r.URL.Path, "/repos/")
		parts := strings.Split(rest, "/")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == fmt.Sprintf("/repos/%s/issues/%d/timeline", admissionSignalsQueueRepo, admissionSignalsQueuePull):
			_, _ = w.Write(timeline)
		case r.Method == http.MethodPost && r.URL.Path == "/graphql":
			body, _ := io.ReadAll(r.Body)
			if bytes.Contains(body, []byte("mergeQueue")) {
				_, _ = io.WriteString(w, `{"data":{"repository":{"mergeQueue":null}}}`)
				return
			}
			_, _ = io.WriteString(w, `{"data":{"repository":{"pullRequest":null}}}`)
		case r.Method == http.MethodGet && isRepository && len(parts) == 2:
			_, _ = io.WriteString(w, `{"full_name":"`+parts[0]+"/"+parts[1]+`","default_branch":"main"}`)
		case r.Method == http.MethodGet && isRepository && strings.HasSuffix(rest, "/git/ref/heads/main"):
			_, _ = io.WriteString(w, `{"ref":"refs/heads/main","object":{"sha":"base-main"}}`)
		case r.Method == http.MethodPost && isRepository && strings.HasSuffix(rest, "/git/refs"):
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{}`)
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"message":"Not Found"}`)
		}
	}})
}

// signalsDispatch is the run's Dispatch as the daemon reads it: the package's board (liveDispatch),
// which lists the issues it holds, with the priority and the blocked_by links of each issue an event
// of the driver's gave them added to every read and every listing of that issue, as Dispatch's own
// issue detail and listing carry them (dispatch.Client.GetIssue, ListIssues), since the board keeps
// neither and the daemon prepares a todo event of an unrecorded issue from Dispatch's view of it
// (repositories.go's prepareIssue). The board lists because the proof restarts the daemon, whose
// boot reads the project's listing again: a listing that left out the issues the run recorded
// would be a Dispatch that no longer holds them. Any write whose route names an admission reading,
// a signal or an eval it refuses 403 and records: the daemon calls no route of Dispatch for a
// reading (S6), so the proof holds the record empty. Every other request is the board's own, which
// fails the test on a call it does not model.
type signalsDispatch struct {
	board *liveDispatch
	mu    sync.Mutex
	views map[string]issueView
	// readings is each write of an admission reading the daemon made, "METHOD path".
	readings []string
}

// issueView is what Dispatch's view of one issue carries beyond the board's: its priority, 0 for
// P0 to 3 for P3, and the issues it is blocked by.
type issueView struct {
	priority  int
	blockedBy []string
}

// show records the view of key an event of the driver's gave it.
func (d *signalsDispatch) show(key string, view issueView) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.views[key] = view
}

// readingWrites is each write of an admission reading the daemon made.
func (d *signalsDispatch) readingWrites() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.readings)
}

func (d *signalsDispatch) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	path := strings.ToLower(req.URL.Path)
	if req.Method != http.MethodGet && (strings.Contains(path, "reading") || strings.Contains(path, "signal") ||
		strings.Contains(path, "admission") || strings.Contains(path, "eval")) {
		d.mu.Lock()
		d.readings = append(d.readings, req.Method+" "+req.URL.Path)
		d.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		answerRefusal(w, http.StatusForbidden, "READING_REFUSED", "this Dispatch takes no admission reading from a daemon")
		return
	}
	key := issueOfDispatchPath(req.URL.Path)
	listing := req.URL.Path == "/api/v1/issues"
	if req.Method != http.MethodGet || !listing && (key == "" || req.URL.Path != "/api/v1/issues/"+key) {
		d.board.ServeHTTP(w, req)
		return
	}
	read := httptest.NewRecorder()
	d.board.ServeHTTP(read, req)
	var issues []map[string]any
	var decoded error
	if listing {
		decoded = json.Unmarshal(read.Body.Bytes(), &issues)
	} else {
		issues = []map[string]any{{}}
		decoded = json.Unmarshal(read.Body.Bytes(), &issues[0])
	}
	if read.Code != http.StatusOK || decoded != nil {
		w.WriteHeader(read.Code)
		_, _ = w.Write(read.Body.Bytes())
		return
	}
	d.mu.Lock()
	for _, issue := range issues {
		if listed, ok := issue["key"].(string); ok {
			if view, viewed := d.views[listed]; viewed {
				issue["priority"] = view.priority
				issue["blocked_by"] = append([]string{}, view.blockedBy...)
			}
		}
	}
	d.mu.Unlock()
	var answer any = issues
	if !listing {
		answer = issues[0]
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(answer); err != nil {
		d.board.t.Errorf("write the Dispatch read %s: %v", req.URL.Path, err)
	}
}

// publishIssue publishes Dispatch's issue.updated event of key at status, labelled for Legion, at
// priority and blocked by blockedBy, as a person's move from the Dispatch dashboard makes one, on
// the next sequence. The event stands for a write already on Dispatch's board, so the board and
// its front show it first.
func (r *admissionSignalsLive) publishIssue(key, status string, priority int, blockedBy []string) {
	r.t.Helper()
	r.board.place(key, status, "", true)
	r.front.show(key, issueView{priority: priority, blockedBy: blockedBy})
	r.seq++
	payload, err := json.Marshal(map[string]any{
		"id": r.seq, "issue_key": key, "project": r.cfg.Project, "seq": r.seq, "type": "issue.updated",
		"actor": map[string]string{"kind": "user", "id": "operator"}, "notify": true, "created_at": time.Now().UTC().Format(time.RFC3339Nano),
		"payload": map[string]any{
			"key": key, "status": status, "title": key, "parent": nil, "rank": "M", "priority": priority,
			"blocked_by": append([]string{}, blockedBy...), "labels": liveDispatchLabels(true), "last_seq": r.seq,
			"project": r.cfg.Project, "resolved_repo": liveRepo,
		},
	})
	if err != nil {
		r.t.Fatalf("encode the Dispatch event of %s: %v", key, err)
	}
	r.publish("notifications.dispatch.issue."+key+".issue.updated", "dispatch", fmt.Sprintf("%s-%s-%d", key, status, r.seq), string(payload))
	r.note("drove Dispatch's issue.updated of %s: %s, P%d, blocked by %v", key, status, priority, blockedBy)
}

// run runs the `legion` binary this checkout built against the booted daemon, from a directory and
// a HOME of its own, and answers its standard output, its standard error and its exit error.
func (r *admissionSignalsLive) run(args ...string) (string, string, error) {
	r.t.Helper()
	cmd := exec.Command(r.legion, append(args, "--port", strconv.Itoa(r.cfg.Port))...)
	cmd.Dir = r.t.TempDir()
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + r.t.TempDir()}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	return strings.TrimRight(stdout.String(), "\n"), strings.TrimRight(stderr.String(), "\n"), err
}

// signals runs `legion project signals <action> <flags>` with the operator's bearer, and answers
// both streams and the exit error: a refusal is what a step reads.
func (r *admissionSignalsLive) signals(action string, flags ...string) (string, error) {
	r.t.Helper()
	args := append([]string{"project", "signals", action, "--operator-token-file", r.cfg.OperatorTokenFile}, flags...)
	stdout, stderr, err := r.run(args...)
	words := make([]string, 0, len(flags))
	for _, flag := range flags {
		if strings.ContainsRune(flag, ' ') {
			flag = strconv.Quote(flag)
		}
		words = append(words, flag)
	}
	r.note("$ legion project signals %s %s", action, strings.Join(words, " "))
	out := strings.TrimSpace(stdout + "\n" + stderr)
	r.note("  %s", out)
	return out, err
}

// mustSignals is signals for a change the daemon must take.
func (r *admissionSignalsLive) mustSignals(action string, flags ...string) string {
	r.t.Helper()
	out, err := r.signals(action, flags...)
	if err != nil {
		r.t.Fatalf("legion project signals %s: %v\n%s", action, err, out)
	}
	return out
}

// legionState is `legion state --json` now, decoded, with what a step turns on printed under it
// and the answer written to the evidence directory under label.
func (r *admissionSignalsLive) legionState(label string) api.State {
	r.t.Helper()
	stdout, stderr, err := r.run("state", "--json")
	if err != nil {
		r.t.Fatalf("legion state --json: %v\n%s", err, stderr)
	}
	var state api.State
	if err := json.Unmarshal([]byte(stdout), &state); err != nil {
		r.t.Fatalf("decode legion state --json %s: %v", stdout, err)
	}
	if r.evidence != "" {
		r.states++
		path := filepath.Join(r.evidence, fmt.Sprintf("state-%02d-%s.json", r.states, label))
		if err := os.WriteFile(path, []byte(stdout+"\n"), 0o600); err != nil {
			r.t.Fatalf("write the evidence file %s: %v", path, err)
		}
	}
	r.note("legion state --json (%s): admission active %v, waiting %v, blocked %v", label, state.Admission.Active, state.Admission.Waiting, blockedKeys(state))
	if reading := state.Admission.Signals; reading != nil {
		r.note("  admission.signals: on %t; %s", reading.On, seriesWordsOf(*reading))
	}
	return state
}

// waitState polls GET /legion/v1/state until want holds of it, and then answers `legion state
// --json`, the answer the step is judged on. A wait that gives up prints that answer first.
func (r *admissionSignalsLive) waitState(label, what string, want func(api.State) bool) api.State {
	r.t.Helper()
	held := false
	defer func() {
		if !held {
			r.legionState("gave-up-" + label)
		}
	}()
	testwait.Eventually(r.t, what, func() bool { return want(r.d.state()) })
	held = true
	return r.legionState(label)
}

// awaitLog waits for the daemon to write a record matching want after the first from, and answers
// it.
func (r *admissionSignalsLive) awaitLog(what string, from int, want func(loggedRecord) bool) loggedRecord {
	r.t.Helper()
	var found *loggedRecord
	testwait.Eventually(r.t, what, func() bool {
		found = r.daemon.firstAfter(from, want)
		return found != nil
	})
	return *found
}

// queueHeldStart matches the daemon's line of a start the queue held, of issue at limit.
func queueHeldStart(issue string, limit float64) func(loggedRecord) bool {
	return func(line loggedRecord) bool {
		return line.Message == admit.SignalHeldStartLine && line.Attrs["issue"] == issue &&
			line.Attrs["signal"] == signals.Queue && line.Attrs["limit"] == limit
	}
}

// seriesOf is the series signal, with part for the provider, of state's admission.signals.
func seriesOf(t *testing.T, state api.State, signal, part string) store.SignalSeriesReading {
	t.Helper()
	if state.Admission.Signals == nil {
		t.Fatalf("the state carries no admission.signals")
	}
	for _, series := range state.Admission.Signals.Series {
		if series.Signal == signal && (series.Part == nil && part == "" || series.Part != nil && *series.Part == part) {
			return series
		}
	}
	t.Fatalf("admission.signals carries no series %s %s", signal, part)
	return store.SignalSeriesReading{}
}

// seriesWordsOf is each series of reading in one line: its value or why it is absent, its limit
// and whether it holds.
func seriesWordsOf(reading store.SignalsReading) string {
	words := make([]string, 0, len(reading.Series))
	for _, series := range reading.Series {
		name := series.Signal
		if series.Part != nil {
			name += "/" + *series.Part
		}
		value := "absent"
		if series.Value != nil {
			value = strconv.FormatFloat(*series.Value, 'g', 6, 64)
		}
		limit := "none"
		if series.Limit != nil {
			limit = strconv.FormatFloat(*series.Limit, 'g', -1, 64)
		}
		words = append(words, fmt.Sprintf("%s %s limit %s holds %t", name, value, limit, series.Holds))
	}
	return strings.Join(words, "; ")
}

// blockedKeys is each issue of admission.blocked with what it waits on.
func blockedKeys(state api.State) []string {
	keys := make([]string, 0, len(state.Admission.Blocked))
	for _, row := range state.Admission.Blocked {
		keys = append(keys, fmt.Sprintf("%s on %v", row.Issue, row.BlockedBy))
	}
	return keys
}

// queueReads says whether the queue's value in state is the recorded timeline's wait.
func (r *admissionSignalsLive) queueReads(series store.SignalSeriesReading) bool {
	return series.Value != nil && math.Abs(*series.Value-admissionSignalsQueueWait.Hours()) < 1e-9
}

// changesOf is each entry of the record of changes in state of what, for signal ("" for the
// switch), oldest first.
func changesOf(state api.State, what store.SignalChangeKind, signal string) []store.SignalChange {
	var entries []store.SignalChange
	for _, entry := range slices.Backward(state.Admission.SignalChanges) {
		if entry.What == what && (entry.Signal == nil && signal == "" || entry.Signal != nil && *entry.Signal == signal) {
			entries = append(entries, entry)
		}
	}
	return entries
}

// requireSetBy fails the step unless entry is a person's change through the operator's command at
// a moment, with reason.
func (r *admissionSignalsLive) requireSetBy(entry store.SignalChange, reason string) {
	r.t.Helper()
	if entry.SetBy == nil || entry.SetBy.Kind != store.PauseByOperator || entry.Reason == nil || *entry.Reason != reason || entry.At.IsZero() {
		r.t.Fatalf("the record's entry %+v names no operator's change at a moment with the reason %q", entry, reason)
	}
}

// requireContiguous fails the step unless the ids of the newest entries rise by one.
func (r *admissionSignalsLive) requireContiguous(state api.State) {
	r.t.Helper()
	changes := state.Admission.SignalChanges
	for i := 1; i < len(changes); i++ {
		if changes[i-1].ID != changes[i].ID+1 {
			r.t.Fatalf("the record's entries %d and %d are adjacent, newest first, and their ids do not rise by one: %+v", changes[i-1].ID, changes[i].ID, changes)
		}
	}
}

// The reasons each change cites, as a person's word.
const (
	reasonQueueLimit   = "the owner set the merge queue's limit at 1 hour in the weekly plan"
	reasonOn           = "the owner turned admission by signals on in the weekly plan"
	reasonQueueLower   = "the owner set the merge queue's limit at 2 hours"
	reasonQueueRaised  = "the owner raised the merge queue's limit to 4 hours"
	reasonQueueRefused = "the owner asked for a limit of 200 hours"
	reasonKillsRefused = "the owner asked for a limit of 2.5 kills"
	reasonRefusals     = "the owner asked for a limit on the provider's refusals"
	reasonQueueAgain   = "the owner set the merge queue's limit back at 1 hour"
	reasonKills        = "the owner set the kills' limit at 5"
	reasonKillsCleared = "the owner took the kills' limit away"
	reasonOff          = "the owner turned admission by signals off"
)

// Step 1. The daemon boots with room in its cap, admission by signals off and no limit, and its
// boot has read the recorded timeline of the open wait it holds: the queue reads the median of the
// timeline's two waits, 3.46 hours, holding nothing. Until that read the queue reads the seeded
// open wait alone, the wait so far of a pull request still in the queue, so the step waits for the
// timeline's value rather than for any value.
func (r *admissionSignalsLive) freshStateOffWithNoLimits() {
	t := r.t
	state := r.waitState("fresh", "the boot's read of the merge queue's timeline", func(state api.State) bool {
		return state.Admission.Signals != nil && r.queueReads(seriesOf(t, state, signals.Queue, ""))
	})
	r.boots = state.Daemon.Boots
	reading := state.Admission.Signals
	if reading.On || reading.Switch != nil {
		t.Fatalf("admission.signals is on %t with switch %+v; want a fresh project off, its switch never set", reading.On, reading.Switch)
	}
	for _, series := range reading.Series {
		if series.Limit != nil || series.LimitSet != nil || series.Holds {
			t.Fatalf("series %+v has a limit, a setter or a hold; want a fresh project with no limit", series)
		}
	}
	if len(reading.Series) != len(store.LiveSeries()) {
		t.Fatalf("admission.signals carries %d series; want the five of store.LiveSeries()", len(reading.Series))
	}
	queue := seriesOf(t, state, signals.Queue, "")
	if !r.queueReads(queue) {
		t.Fatalf("the queue reads %v (absent %v); want %v hours, the median of the recorded timeline's two waits", queue.Value, queue.Absent, admissionSignalsQueueWait.Hours())
	}
	for _, what := range []store.SignalChangeKind{store.SignalChangeSwitch, store.SignalChangeLimit} {
		if entries := changesOf(state, what, ""); len(entries) != 0 {
			t.Fatalf("the record holds %s entries %+v before any person changed one", what, entries)
		}
	}
	if state.Admission.Cap != admissionSignalsCap || len(state.Admission.Active) != 0 {
		t.Fatalf("admission is cap %d with %v active; want a cap of %d with every slot free", state.Admission.Cap, state.Admission.Active, admissionSignalsCap)
	}
	r.note("read back: off, no limit; the queue reads %v hours from the recorded timeline; cap %d, no slot taken", *queue.Value, state.Admission.Cap)
}

// Step 2. Before any candidate is ready, a person sets the queue's limit at 1 hour and turns
// admission by signals on, so the rule off cannot start them early. The queue now holds.
func (r *admissionSignalsLive) limitAndSwitchBeforeTheCandidates() {
	t := r.t
	mark := r.daemon.count()
	limit := strconv.FormatFloat(queueLimitLow, 'g', -1, 64)
	r.mustSignals("set-limit", "--signal", signals.Queue, "--limit", limit, "--reason", reasonQueueLimit)
	r.mustSignals("on", "--reason", reasonOn)
	state := r.waitState("on", "the queue holding under its limit", func(state api.State) bool {
		return seriesOf(t, state, signals.Queue, "").Holds
	})
	queue := seriesOf(t, state, signals.Queue, "")
	if !state.Admission.Signals.On || queue.Limit == nil || *queue.Limit != queueLimitLow || queue.Since == nil {
		t.Fatalf("admission.signals on %t, queue %+v; want on, the queue's limit %v and its hold dated", state.Admission.Signals.On, queue, queueLimitLow)
	}
	if set := state.Admission.Signals.Switch; set == nil || set.By.Kind != store.PauseByOperator || set.Reason != reasonOn {
		t.Fatalf("the switch was set by %+v; want the operator, with the reason %q", set, reasonOn)
	}
	line := r.awaitLog("the daemon's line of the queue's hold", mark, func(line loggedRecord) bool {
		return line.Message == admit.SignalHoldLine && line.Attrs["signal"] == signals.Queue && line.Attrs["holds"] == true
	})
	r.note("the daemon's log: %s signal=%v holds=%v value=%v limit=%v", line.Message, line.Attrs["signal"], line.Attrs["holds"], line.Attrs["value"], line.Attrs["limit"])
}

// Step 3. One P0 issue and two below it become ready to start, the second blocked by a Dispatch
// link. The P0 issue starts as it does with the rule off. The first below it waits for the queue,
// which `legion state --json` names holding with its value and its limit, and the daemon logs the
// start the queue held; the second waits on its link.
func (r *admissionSignalsLive) theP0StartsAndTheOthersWait() {
	t := r.t
	mark := r.daemon.count()
	r.publishIssue(r.p0, "todo", 0, nil)
	r.publishIssue(r.first, "todo", 1, nil)
	r.publishIssue(r.second, "todo", 2, []string{r.blocker})
	state := r.waitState("held", "the P0 issue on a slot and both others in the line", func(state api.State) bool {
		return holdsSlot(state, r.p0) && slices.Contains(state.Admission.Waiting, r.first) && slices.Contains(state.Admission.Waiting, r.second)
	})
	line := r.awaitLog("the daemon's line of the start the queue held", mark, queueHeldStart(r.first, queueLimitLow))
	r.note("the daemon's log: %s issue=%v signal=%v value=%v limit=%v", line.Message, line.Attrs["issue"], line.Attrs["signal"], line.Attrs["value"], line.Attrs["limit"])
	if value, ok := line.Attrs["value"].(float64); !ok || math.Abs(value-admissionSignalsQueueWait.Hours()) > 1e-9 {
		t.Fatalf("the held start's line names the value %v; want the queue's %v", line.Attrs["value"], admissionSignalsQueueWait.Hours())
	}
	state = r.legionState("held-read")
	if holdsSlot(state, r.first) || holdsSlot(state, r.second) {
		t.Fatalf("admission.active = %v; want neither %s nor %s on a slot while the queue holds", state.Admission.Active, r.first, r.second)
	}
	if !slices.Equal(blockedBy(state, r.second), []string{r.blocker}) {
		t.Fatalf("admission.blocked names %s waiting on %v; want its link to %s", r.second, blockedBy(state, r.second), r.blocker)
	}
	queue := seriesOf(t, state, signals.Queue, "")
	if !queue.Holds || !r.queueReads(queue) || queue.Limit == nil || *queue.Limit != queueLimitLow {
		t.Fatalf("the queue reads %+v; want it holding at %v hours over its limit %v", queue, admissionSignalsQueueWait.Hours(), queueLimitLow)
	}
	text, stderr, err := r.run("state")
	if err != nil {
		t.Fatalf("legion state: %v\n%s", err, stderr)
	}
	for line := range strings.Lines(text) {
		if strings.HasPrefix(line, "signals: ") {
			r.note("legion state: %s", strings.TrimSpace(line))
		}
	}
	r.note("read back: %s on a slot; %s waits for the queue, %s on %s", r.p0, r.first, r.second, r.blocker)
}

// Step 4, the negative control. A lower limit, still under the queue's value, runs a pass of its
// own, and that pass holds the first issue again: the start a limit holds is held at each pass,
// not only the first, so it does not start while the limit stays under the value.
func (r *admissionSignalsLive) aLowerLimitKeepsTheCandidateWaiting() {
	t := r.t
	mark := r.daemon.count()
	r.mustSignals("set-limit", "--signal", signals.Queue, "--limit", strconv.FormatFloat(queueLimitLower, 'g', -1, 64), "--reason", reasonQueueLower)
	line := r.awaitLog("the daemon's line of the start the lower limit held", mark, queueHeldStart(r.first, queueLimitLower))
	r.note("the daemon's log: %s issue=%v signal=%v value=%v limit=%v", line.Message, line.Attrs["issue"], line.Attrs["signal"], line.Attrs["value"], line.Attrs["limit"])
	deadline := time.Now().Add(liveSettle)
	for time.Now().Before(deadline) {
		if holdsSlot(r.d.state(), r.first) {
			t.Fatalf("%s took a slot under the limit %v, which the queue's %v hours is over", r.first, queueLimitLower, admissionSignalsQueueWait.Hours())
		}
		time.Sleep(100 * time.Millisecond)
	}
	state := r.legionState("negative-control-held")
	if holdsSlot(state, r.first) || !seriesOf(t, state, signals.Queue, "").Holds {
		t.Fatalf("admission.active = %v and the queue %+v; want %s waiting while the queue holds", state.Admission.Active, seriesOf(t, state, signals.Queue, ""), r.first)
	}
	r.note("read back over %s: %s still waits under the limit %v", liveSettle, r.first, queueLimitLower)
}

// Step 5. The limit raised above the queue's value, with no restart, starts the first issue below
// P0 at its own pass. The second stays behind its link.
func (r *admissionSignalsLive) theRaisedLimitStartsTheCandidate() {
	t := r.t
	r.mustSignals("set-limit", "--signal", signals.Queue, "--limit", strconv.FormatFloat(queueLimitRaised, 'g', -1, 64), "--reason", reasonQueueRaised)
	state := r.waitState("negative-control-raised", r.first+" on a slot under the raised limit", func(state api.State) bool {
		return holdsSlot(state, r.first)
	})
	queue := seriesOf(t, state, signals.Queue, "")
	if queue.Holds || queue.Limit == nil || *queue.Limit != queueLimitRaised || !r.queueReads(queue) {
		t.Fatalf("the queue reads %+v; want it at %v hours under its limit %v, holding nothing", queue, admissionSignalsQueueWait.Hours(), queueLimitRaised)
	}
	if holdsSlot(state, r.second) || !slices.Equal(blockedBy(state, r.second), []string{r.blocker}) {
		t.Fatalf("%s is on a slot or waits on %v; want it still waiting on %s", r.second, blockedBy(state, r.second), r.blocker)
	}
	if state.Daemon.Boots != r.boots {
		t.Fatalf("the daemon has booted %d times; want %d, no restart", state.Daemon.Boots, r.boots)
	}
	r.note("read back: %s started under the limit %v with no restart; %s still waits on %s", r.first, queueLimitRaised, r.second, r.blocker)
}

// Step 6. The daemon restarts over the same store. The switch, the limit, who set them, why, and
// every entry of the record of changes read back as they were, and the two started issues keep
// their slots.
func (r *admissionSignalsLive) aRestartKeepsTheSettingsAndTheRecord() {
	t := r.t
	before := r.legionState("before-restart")
	r.d.stop()
	rebindHeldPorts(t, &r.cfg)
	r.boot()
	state := r.waitState("after-restart", "the restarted daemon's reading of the signals", func(state api.State) bool {
		return state.Daemon.Boots == before.Daemon.Boots+1 && state.Admission.Signals != nil
	})
	reading := state.Admission.Signals
	if !reading.On || reading.Switch == nil || reading.Switch.By.Kind != store.PauseByOperator || reading.Switch.Reason != reasonOn {
		t.Fatalf("after the restart the switch is on %t, set %+v; want on, set by the operator with the reason %q", reading.On, reading.Switch, reasonOn)
	}
	queue := seriesOf(t, state, signals.Queue, "")
	if queue.Limit == nil || *queue.Limit != queueLimitRaised || queue.LimitSet == nil || queue.LimitSet.By.Kind != store.PauseByOperator ||
		queue.LimitSet.Reason != reasonQueueRaised || queue.LimitSet.At.IsZero() {
		t.Fatalf("after the restart the queue's limit is %+v; want %v, set by the operator with the reason %q", queue, queueLimitRaised, reasonQueueRaised)
	}
	for _, kept := range before.Admission.SignalChanges {
		at := slices.IndexFunc(state.Admission.SignalChanges, func(entry store.SignalChange) bool { return entry.ID == kept.ID })
		if at < 0 {
			t.Fatalf("after the restart the record has no entry %d; it held %+v", kept.ID, kept)
		}
		if got, _ := json.Marshal(state.Admission.SignalChanges[at]); string(got) != mustJSON(t, kept) {
			t.Fatalf("after the restart the record's entry %d is %s; it was %s", kept.ID, got, mustJSON(t, kept))
		}
	}
	r.requireContiguous(state)
	switches := changesOf(state, store.SignalChangeSwitch, "")
	if len(switches) != 1 || switches[0].Value == nil || *switches[0].Value != 1 {
		t.Fatalf("the record's switch entries are %+v; want the one that turned it on", switches)
	}
	r.requireSetBy(switches[0], reasonOn)
	limits := changesOf(state, store.SignalChangeLimit, signals.Queue)
	wantLimits := []float64{queueLimitLow, queueLimitLower, queueLimitRaised}
	wantReasons := []string{reasonQueueLimit, reasonQueueLower, reasonQueueRaised}
	if len(limits) != len(wantLimits) {
		t.Fatalf("the record's limit entries of the queue are %+v; want %v", limits, wantLimits)
	}
	for i, entry := range limits {
		if entry.Value == nil || *entry.Value != wantLimits[i] || entry.Part != nil {
			t.Fatalf("the record's limit entry %d of the queue is %+v; want %v", i, entry, wantLimits[i])
		}
		r.requireSetBy(entry, wantReasons[i])
	}
	holds := changesOf(state, store.SignalChangeHolds, signals.Queue)
	if len(holds) < 2 || *holds[0].Value != 1 || *holds[len(holds)-1].Value != 0 || holds[0].SetBy != nil || holds[0].Reason != nil {
		t.Fatalf("the record's holds entries of the queue are %+v; want it holding at the switch and not since the raised limit, each with no setter", holds)
	}
	for _, key := range []string{r.p0, r.first} {
		if !holdsSlot(state, key) {
			t.Fatalf("after the restart admission.active = %v; want %s still on its slot", state.Admission.Active, key)
		}
	}
	r.note("read back after the restart: on, set by the operator; the queue's limit %v with its reason; the %d entries of before, ids contiguous; %s and %s on their slots",
		*queue.Limit, len(before.Admission.SignalChanges), r.p0, r.first)
}

// Step 7. A limit outside its signal's range is refused with the reason, and changes nothing:
// neither the settings nor the record. So is a limit on the provider's refusals, a part the
// provider does not have, which the state shows no series of.
func (r *admissionSignalsLive) aLimitOutsideItsRangeIsRefused() {
	t := r.t
	before := r.legionState("before-refusal")
	for _, refused := range []struct {
		signal, part, reason, why string
		limit                     float64
	}{
		{signals.Queue, "", reasonQueueRefused, "which is not above 0 and at most 168", queueLimitRefused},
		{signals.Kills, "", reasonKillsRefused, "which is no whole number from 1 to 1000000", killsLimitRefused},
		{signals.Provider, "refusals", reasonRefusals, "provider (part headroom)", refusalsLimitRefused},
	} {
		flags := []string{"--signal", refused.signal, "--limit", strconv.FormatFloat(refused.limit, 'g', -1, 64), "--reason", refused.reason}
		if refused.part != "" {
			flags = append(flags, "--part", refused.part)
		}
		out, err := r.signals("set-limit", flags...)
		if err == nil || !strings.Contains(out, refused.why) || !strings.Contains(out, "nothing changed") {
			t.Fatalf("legion project signals set-limit %v exited %v with %q; want it refused, saying %q and that nothing changed",
				flags, err, out, refused.why)
		}
	}
	state := r.legionState("after-refusal")
	if mustJSON(t, state.Admission.Signals.Switch) != mustJSON(t, before.Admission.Signals.Switch) {
		t.Fatalf("the refusals moved the switch from %+v to %+v", before.Admission.Signals.Switch, state.Admission.Signals.Switch)
	}
	for _, series := range state.Admission.Signals.Series {
		if series.Signal == signals.Provider && (series.Part == nil || *series.Part != store.PartHeadroom) {
			t.Fatalf("the state serves the provider's series %+v; want its headroom alone", series)
		}
	}
	for _, entry := range state.Admission.SignalChanges {
		if entry.Signal != nil && *entry.Signal == signals.Provider && (entry.Part == nil || *entry.Part != store.PartHeadroom) {
			t.Fatalf("the record holds %+v; want no entry of a provider's part but its headroom's", entry)
		}
	}
	for _, name := range []string{signals.Queue, signals.Kills} {
		was, is := seriesOf(t, before, name, ""), seriesOf(t, state, name, "")
		if mustJSON(t, was.Limit) != mustJSON(t, is.Limit) || mustJSON(t, was.LimitSet) != mustJSON(t, is.LimitSet) {
			t.Fatalf("the refusals moved the %s's limit from %+v to %+v", name, was, is)
		}
	}
	newest := before.Admission.SignalChanges[0].ID
	for _, entry := range state.Admission.SignalChanges {
		if entry.ID > newest && (entry.What == store.SignalChangeLimit || entry.What == store.SignalChangeSwitch) {
			t.Fatalf("the record gained %+v after the refusals; want no change of a limit or the switch", entry)
		}
	}
	r.note("read back: the three refused with their reasons; the switch, the queue's and the kills' limits and the record stand as before, and no series of the provider's refusals exists")
}

// Step 8. With the queue's limit at 1 hour again, holding, and the kills' limit set, a person
// clears the kills' limit while the rule is on. That limit alone goes, with its entry naming who
// cleared it and why; the queue's still holds; and no holder loses its slot for the queue.
func (r *admissionSignalsLive) aLimitClearedWhileAnotherHolds() {
	t := r.t
	r.mustSignals("set-limit", "--signal", signals.Queue, "--limit", strconv.FormatFloat(queueLimitLow, 'g', -1, 64), "--reason", reasonQueueAgain)
	r.mustSignals("set-limit", "--signal", signals.Kills, "--limit", strconv.FormatFloat(killsLimit, 'g', -1, 64), "--reason", reasonKills)
	r.waitState("two-limits", "the queue holding again and the kills' limit set", func(state api.State) bool {
		kills := seriesOf(t, state, signals.Kills, "")
		return seriesOf(t, state, signals.Queue, "").Holds && kills.Limit != nil && *kills.Limit == killsLimit
	})
	r.mustSignals("clear-limit", "--signal", signals.Kills, "--reason", reasonKillsCleared)
	state := r.waitState("cleared", "the kills' limit cleared", func(state api.State) bool {
		return seriesOf(t, state, signals.Kills, "").Limit == nil
	})
	kills := seriesOf(t, state, signals.Kills, "")
	if kills.LimitSet == nil || kills.LimitSet.By.Kind != store.PauseByOperator || kills.LimitSet.Reason != reasonKillsCleared {
		t.Fatalf("the kills' limit was last set %+v; want the operator's clear with the reason %q", kills.LimitSet, reasonKillsCleared)
	}
	entries := changesOf(state, store.SignalChangeLimit, signals.Kills)
	if len(entries) != 2 || entries[0].Value == nil || *entries[0].Value != killsLimit || entries[1].Value != nil {
		t.Fatalf("the record's limit entries of the kills are %+v; want the limit %v and then its clear, valued null", entries, killsLimit)
	}
	r.requireSetBy(entries[0], reasonKills)
	r.requireSetBy(entries[1], reasonKillsCleared)
	queue := seriesOf(t, state, signals.Queue, "")
	if !state.Admission.Signals.On || !queue.Holds || queue.Limit == nil || *queue.Limit != queueLimitLow {
		t.Fatalf("after the clear admission.signals is on %t and the queue %+v; want on, the queue holding under its limit %v", state.Admission.Signals.On, queue, queueLimitLow)
	}
	for _, key := range []string{r.p0, r.first} {
		if !holdsSlot(state, key) {
			t.Fatalf("admission.active = %v; want %s on its slot: the pass takes no slot from a holder for a signal", state.Admission.Active, key)
		}
	}
	r.requireContiguous(state)
	r.note("read back: the kills' limit cleared with its entry; the queue still holds under %v; %s and %s keep their slots", queueLimitLow, r.p0, r.first)
}

// Step 9. A person turns admission by signals off. The queue still reads over its limit and holds
// nothing. A person then takes the second issue's link away, and the cap alone starts it. No write
// of an admission reading reached Dispatch in the whole run.
func (r *admissionSignalsLive) offLetsTheCapStartTheHeldCandidate() {
	t := r.t
	r.mustSignals("off", "--reason", reasonOff)
	off := r.waitState("off", "admission by signals off", func(state api.State) bool {
		return state.Admission.Signals != nil && !state.Admission.Signals.On
	})
	queue := seriesOf(t, off, signals.Queue, "")
	if queue.Holds || !r.queueReads(queue) || queue.Limit == nil || *queue.Limit > *queue.Value {
		t.Fatalf("with the rule off the queue reads %+v; want its %v hours at or over its limit %v, holding nothing", queue, admissionSignalsQueueWait.Hours(), queueLimitLow)
	}
	switches := changesOf(off, store.SignalChangeSwitch, "")
	if len(switches) != 2 || switches[1].Value == nil || *switches[1].Value != 0 {
		t.Fatalf("the record's switch entries are %+v; want on and then off", switches)
	}
	r.requireSetBy(switches[1], reasonOff)
	r.publishIssue(r.second, "todo", 2, nil)
	state := r.waitState("started-by-the-cap", r.second+" on a slot once its link is gone", func(state api.State) bool {
		return holdsSlot(state, r.second)
	})
	queue = seriesOf(t, state, signals.Queue, "")
	if state.Admission.Signals.On || !r.queueReads(queue) || *queue.Limit > *queue.Value {
		t.Fatalf("%s started with admission.signals on %t and the queue %+v; want it started by the cap alone while the queue reads over its limit", r.second, state.Admission.Signals.On, queue)
	}
	if got := len(state.Admission.Active); got > state.Admission.Cap {
		t.Fatalf("%d issues hold a slot of a cap of %d", got, state.Admission.Cap)
	}
	if readings := r.front.readingWrites(); len(readings) != 0 {
		t.Fatalf("the daemon wrote admission readings to Dispatch: %v; want none", readings)
	}
	r.note("read back: off; the queue reads %v hours over its limit %v and holds nothing; %s started on the cap; Dispatch took no reading", *queue.Value, *queue.Limit, r.second)
}
