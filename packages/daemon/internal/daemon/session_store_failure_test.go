package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// The tests of this file hold what the daemon does with a session whose Oh My Pi session store
// stops taking its transcript (dispatch://INFRA-5660/spec#b-r2): pi-legion reads the session
// manager's persistence failure on its activity tick and sends it on POST
// /legion/v1/claims/activity as `activity.storeFailure`, and the daemon keeps it on the claim,
// shows it in the state with the moment it began, and tells the issue's architect once it has
// stood a minute, at most once a claim an hour, or the project's controller alone for a burst.
//
// Two of them run the whole daemon and read its state route; the other four run the minute sweep
// the daemon runs (reconcileOrphans) over a rig whose clock the test holds (podUpdateRig), and
// write each report through the store's own call of the route (store.Store.PutClaimActivity), from
// the body pi-legion sends decoded as the route decodes it (storeFailingActivity).

// sessionStoreLine is the constant prefix of every line the daemon writes for a notice of a
// session store's failure, written or held by a bound: the production check reads them by it.
const sessionStoreLine = "session store failing: "

// sessionStoreError is the error a session store answers once it refuses a session's writes, as an
// Oh My Pi store whose session file changed under it reports it.
const sessionStoreError = "Session file changed before rewrite: /home/legion/.omp/agent/sessions/01a1d89b.jsonl"

// storeFailingActivity is the activity report pi-legion sends while its session's store refuses
// its writes, read as the claims/activity route reads it: the wire body, decoded strictly into the
// route's own type, so a reader that does not know `storeFailure` refuses the body as the route
// does rather than dropping the key.
func storeFailingActivity(t *testing.T, at, since time.Time, storeError string) claim.SessionActivity {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"at":             at,
		"backgroundWork": map[string]any{"running": false, "kinds": []string{}},
		"phase":          map[string]any{"nextReminderAt": nil},
		"storeFailure":   map[string]any{"since": since, "error": storeError},
	})
	if err != nil {
		t.Fatalf("encode the activity: %v", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var activity claim.SessionActivity
	if err := decoder.Decode(&activity); err != nil {
		t.Fatalf("decode the activity body %s as the claims/activity route reads it: %v", body, err)
	}
	return activity
}

// reportsStoreFailing is token's session reporting, at the rig's clock, that its store has
// refused its writes since since, onto launch generation, through the store's call of the route.
func (r *podUpdateRig) reportsStoreFailing(token claim.Token, generation uint64, since time.Time) {
	r.t.Helper()
	known, err := r.st.PutClaimActivity(r.ctx, token, generation, storeFailingActivity(r.t, r.clock.Now().UTC(), since, sessionStoreError))
	if err != nil || !known {
		r.t.Fatalf("report the store failure of %s: known %v, %v", token, known, err)
	}
}

// reportsStoreRecovered is token's session reporting, at the rig's clock, a reading with no store
// failure: its store takes its transcript again.
func (r *podUpdateRig) reportsStoreRecovered(token claim.Token, generation uint64) {
	r.t.Helper()
	known, err := r.st.PutClaimActivity(r.ctx, token, generation, claim.SessionActivity{At: r.clock.Now().UTC()})
	if err != nil || !known {
		r.t.Fatalf("report the recovered store of %s: known %v, %v", token, known, err)
	}
}

// storedFailureSince is the moment the claim row of token holds its store's failure from, and the
// zero time where it holds none: what the route wrote, read from the column itself.
func (r *podUpdateRig) storedFailureSince(token claim.Token) time.Time {
	r.t.Helper()
	var since *time.Time
	if err := r.pool.QueryRow(r.ctx, `select store_failed_since from claims where token = $1`, string(token)).Scan(&since); err != nil {
		r.t.Fatalf("read the store failure of %s: %v", token, err)
	}
	if since == nil {
		return time.Time{}
	}
	return *since
}

// sweeping runs the daemon's own orphan sweep over the rig every 10 ms until the test ends, as
// the daemon runs it every minute: the sweep reads the claims the store holds and the rig's clock
// at each pass.
func (r *podUpdateRig) sweeping() {
	r.t.Helper()
	r.s.plan.orphanSweep = 10 * time.Millisecond
	ctx, cancel := context.WithCancel(r.ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.s.reconcileOrphans(store.ForConcurrentWorker(ctx))
	}()
	r.t.Cleanup(func() {
		cancel()
		<-done
	})
}

// swept waits for one whole pass of the sweep that began after the call: each pass asks the
// runtime first, so a second ask after the call means a pass started after it has ended.
func (r *podUpdateRig) swept() {
	r.t.Helper()
	asked := len(r.rt.CallsOf("ReconcileOrphans"))
	testwait.Eventually(r.t, "a whole pass of the orphan sweep", func() bool {
		return len(r.rt.CallsOf("ReconcileOrphans")) >= asked+2
	})
}

// toldNotice is one notice row of the outbox, with the issue it is filed under.
type toldNotice struct {
	issue  string
	notice record.Notice
}

// noticesOfKind is every row of outbox kind whose notice is of kind, oldest first, whatever its
// issue.
func (r *podUpdateRig) noticesOfKind(outboxKind record.OutboxKind, kind record.NoticeKind) []toldNotice {
	r.t.Helper()
	var told []toldNotice
	for _, row := range r.rows(outboxKind) {
		var notice record.Notice
		if err := json.Unmarshal(row.Payload, &notice); err != nil {
			r.t.Fatalf("decode %s row %d: %v", outboxKind, row.ID, err)
		}
		if notice.Kind == kind {
			told = append(told, toldNotice{issue: row.Issue, notice: notice})
		}
	}
	return told
}

// architectsTold is every notice of a session store's failure written to an issue's architect.
func (r *podUpdateRig) architectsTold() []toldNotice {
	r.t.Helper()
	return r.noticesOfKind(record.OutboxKindNotice, "session-store-failed")
}

// controllersTold is every controller notice of a burst of session store failures.
func (r *podUpdateRig) controllersTold() []toldNotice {
	r.t.Helper()
	return r.noticesOfKind(record.OutboxKindControllerNotice, "session-store-burst")
}

// sessionStoreLines is every line the rig's log holds of a notice of a session store's failure.
func (r *podUpdateRig) sessionStoreLines() []map[string]any {
	r.t.Helper()
	return loggedMessages(r.t, r.logs.String(), sessionStoreLine)
}

// requireNames fails unless text holds every part.
func requireNames(t *testing.T, what, text string, parts ...string) {
	t.Helper()
	for _, part := range parts {
		if !strings.Contains(text, part) {
			t.Errorf("%s does not name %q: %s", what, part, text)
		}
	}
}

// postActivity is a claim's session posting one activity report to the daemon's route, the body
// written as pi-legion writes it: claim, session, secret and the reading.
func postActivity(t *testing.T, d *daemon, token claim.Token, secret string, activity map[string]any) (int, []byte) {
	t.Helper()
	return d.request(http.MethodPost, "/legion/v1/claims/activity", map[string]any{
		"claimToken": token, "sessionId": "ses_" + string(token), "secret": secret, "activity": activity,
	}, false)
}

// architectView is the issue's architect's claim view as GET /legion/v1/state serves it, each key
// as the wire carries it.
func architectView(t *testing.T, d *daemon, issue string) map[string]json.RawMessage {
	t.Helper()
	status, body := d.request(http.MethodGet, "/legion/v1/state", nil, false)
	if status != http.StatusOK {
		t.Fatalf("GET the state = %d; body %s", status, body)
	}
	var state struct {
		Issues map[string]struct {
			Architect map[string]json.RawMessage `json:"architect"`
		} `json:"issues"`
	}
	if err := json.Unmarshal(body, &state); err != nil {
		t.Fatalf("decode the state %s: %v", body, err)
	}
	view := state.Issues[issue].Architect
	if view == nil {
		t.Fatalf("the state shows no architect of %s: %s", issue, body)
	}
	return view
}

// failingArchitect is a daemon with one ready architect claim of an admitted issue, and the
// secret its session's registration was issued.
func failingArchitect(t *testing.T) (*daemon, api.SpawnRequest, claim.Token, string) {
	t.Helper()
	cfg := testConfig(t)
	rt := fake.NewRuntime()
	d := startDaemon(t, cfg, fakeRuntime(rt, &built{}))
	spawn := architect()
	token := d.spawn(spawn)
	admitIssue(t, cfg, spawn.Issue, claim.RoleArchitect, token)
	return d, spawn, token, readyClaim(t, d, rt, token)
}

// storeFailureOf is the claim view's `storeFailure` as the wire carries it, and false where the
// view carries no such key.
func storeFailureOf(t *testing.T, view map[string]json.RawMessage) (time.Time, string, bool) {
	t.Helper()
	raw, carried := view["storeFailure"]
	if !carried {
		return time.Time{}, "", false
	}
	var failure struct {
		Since time.Time `json:"since"`
		Error string    `json:"error"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&failure); err != nil {
		t.Fatalf("decode the claim view's storeFailure %s: %v", raw, err)
	}
	return failure.Since, failure.Error, true
}

// A session that reports its store has refused its writes for a minute and a half is shown so in
// the state: its claim view carries `storeFailure`, the moment the session first read the failure
// and the store's error, as the session sent them.
func TestAClaimWhoseSessionStoreFailedForAMinuteShowsTheFailureAndItsStartInState(t *testing.T) {
	t.Parallel()
	d, spawn, token, secret := failingArchitect(t)
	if _, _, carried := storeFailureOf(t, architectView(t, d, spawn.Issue)); carried {
		t.Fatalf("the architect's view carries storeFailure before any report says its store failed")
	}

	now := time.Now().UTC().Truncate(time.Second)
	since := now.Add(-90 * time.Second)
	if status, body := postActivity(t, d, token, secret, map[string]any{
		"at": now, "backgroundWork": map[string]any{"running": false, "kinds": []string{}},
		"phase":        map[string]any{"nextReminderAt": nil},
		"storeFailure": map[string]any{"since": since, "error": sessionStoreError},
	}); status != http.StatusNoContent {
		t.Fatalf("activity = %d with body %s, want 204: the route must take a report of the session store's failure", status, body)
	}

	shown, storeError, carried := storeFailureOf(t, architectView(t, d, spawn.Issue))
	if !carried {
		t.Fatalf("the architect's view carries no storeFailure after its session reported one")
	}
	if !shown.Equal(since) {
		t.Errorf("storeFailure.since = %s, want %s, the moment the session first read the failure", shown, since)
	}
	if storeError != sessionStoreError {
		t.Errorf("storeFailure.error = %q, want the store's error %q", storeError, sessionStoreError)
	}
}

// A report that reads no failure clears the one the claim held: the store takes the session's
// transcript again, and the state shows no failure.
func TestAClaimWhoseSessionStoreRecoversClearsTheFailure(t *testing.T) {
	t.Parallel()
	d, spawn, token, secret := failingArchitect(t)
	now := time.Now().UTC().Truncate(time.Second)
	if status, body := postActivity(t, d, token, secret, map[string]any{
		"at": now.Add(-time.Minute), "backgroundWork": map[string]any{"running": false, "kinds": []string{}},
		"phase":        map[string]any{"nextReminderAt": nil},
		"storeFailure": map[string]any{"since": now.Add(-2 * time.Minute), "error": sessionStoreError},
	}); status != http.StatusNoContent {
		t.Fatalf("activity = %d with body %s, want 204: the route must take a report of the session store's failure", status, body)
	}
	// The control: the failure stood before the recovery, so the clear below is the report's.
	if _, _, carried := storeFailureOf(t, architectView(t, d, spawn.Issue)); !carried {
		t.Fatalf("the architect's view carries no storeFailure after its session reported one")
	}

	if status, body := postActivity(t, d, token, secret, map[string]any{
		"at": now, "backgroundWork": map[string]any{"running": false, "kinds": []string{}},
		"phase": map[string]any{"nextReminderAt": nil},
	}); status != http.StatusNoContent {
		t.Fatalf("the recovered report = %d with body %s, want 204", status, body)
	}
	if since, storeError, carried := storeFailureOf(t, architectView(t, d, spawn.Issue)); carried {
		t.Fatalf("the architect's view carries storeFailure {%s %q} after a report that reads no failure", since, storeError)
	}
}

// A session whose store has refused its writes for a minute has its issue's architect told once,
// in one notice that names the issue, the role, the session, when the failure began and the
// store's error; a sweep before the minute tells nobody, and the sweeps after it tell nobody again
// while that failure stands. The daemon writes one line for the notice, under the prefix the
// production check reads.
func TestAClaimWhoseSessionStoreFailsTellsTheArchitectOnceWithTheSessionAndTheError(t *testing.T) {
	t.Parallel()
	rig := newPodUpdateRig(t, readyWorker(claim.RoleImplementer))
	worker := rig.claimOf(claim.RoleImplementer)
	since := rig.clock.Now().UTC()
	rig.reportsStoreFailing(worker.Token, worker.Generation, since)
	rig.sweeping()

	rig.clock.advance(59 * time.Second)
	rig.swept()
	if told := rig.architectsTold(); len(told) != 0 {
		t.Fatalf("a sweep 59s into the failure told %+v, want nobody: a failure is told once it has stood a minute", told)
	}

	rig.clock.advance(2 * time.Second)
	testwait.Eventually(t, "the architect's notice of the failed session store", func() bool { return len(rig.architectsTold()) > 0 })
	rig.clock.advance(10 * time.Minute)
	rig.swept()

	told := rig.architectsTold()
	if len(told) != 1 {
		t.Fatalf("the architect was told %+v, want one notice", told)
	}
	notice := told[0]
	if notice.issue != podUpdateIssue || notice.notice.Role != claim.RoleArchitect {
		t.Errorf("the notice is filed under %s for %q, want %s's architect", notice.issue, notice.notice.Role, podUpdateIssue)
	}
	requireNames(t, "the notice", notice.notice.Reason, podUpdateIssue, string(claim.RoleImplementer), worker.Session,
		since.Format(time.RFC3339), sessionStoreError)
	if burst := rig.controllersTold(); len(burst) != 0 {
		t.Errorf("the controller was told %+v of one claim's failure, want nothing", burst)
	}

	lines := rig.sessionStoreLines()
	if len(lines) != 1 {
		t.Fatalf("the daemon wrote %d lines under %q, want one for the one notice: %v", len(lines), sessionStoreLine, lines)
	}
	if lines[0]["issue"] != podUpdateIssue || lines[0]["role"] != string(claim.RoleImplementer) || lines[0]["session"] != worker.Session {
		t.Errorf("the line %v does not name the issue %s, the role %s and the session %s", lines[0], podUpdateIssue, claim.RoleImplementer, worker.Session)
	}
}

// A failure the session reads and then reads clear inside a minute tells nobody, however many
// sweeps come after: the minute is how long a failure stands before anybody is told of it.
func TestASessionStoreFailureThatClearsWithinAMinuteTellsNobody(t *testing.T) {
	t.Parallel()
	rig := newPodUpdateRig(t, readyWorker(claim.RoleImplementer))
	worker := rig.claimOf(claim.RoleImplementer)
	since := rig.clock.Now().UTC()
	rig.reportsStoreFailing(worker.Token, worker.Generation, since)
	// The control: the claim holds the failure, so what clears it below is the later report.
	if stored := rig.storedFailureSince(worker.Token); !stored.Equal(since) {
		t.Fatalf("the claim holds a store failure since %s, want %s", stored, since)
	}
	rig.sweeping()

	rig.clock.advance(30 * time.Second)
	rig.swept()
	rig.reportsStoreRecovered(worker.Token, worker.Generation)
	if stored := rig.storedFailureSince(worker.Token); !stored.IsZero() {
		t.Fatalf("the claim holds a store failure since %s after a report that reads none", stored)
	}

	rig.clock.advance(2 * time.Minute)
	rig.swept()
	rig.clock.advance(time.Hour)
	rig.swept()
	if told := rig.architectsTold(); len(told) != 0 {
		t.Errorf("the architect was told %+v of a failure that cleared inside a minute", told)
	}
	if burst := rig.controllersTold(); len(burst) != 0 {
		t.Errorf("the controller was told %+v of a failure that cleared inside a minute", burst)
	}
	if lines := rig.sessionStoreLines(); len(lines) != 0 {
		t.Errorf("the daemon wrote %v under %q for a failure nobody was to be told of", lines, sessionStoreLine)
	}
}

// A claim's architect is told of its session store at most once an hour: a failure that clears
// and comes back inside the hour of the notice is held, with one line naming the bound that held
// it, and the hour is the bound, so the failure that still stands once the hour has passed is told.
func TestASessionStoreThatFailsAgainWithinTheHourTellsTheArchitectNoSecondTime(t *testing.T) {
	t.Parallel()
	rig := newPodUpdateRig(t, readyWorker(claim.RoleImplementer))
	worker := rig.claimOf(claim.RoleImplementer)
	start := rig.clock.Now().UTC()
	rig.reportsStoreFailing(worker.Token, worker.Generation, start)
	rig.sweeping()
	rig.clock.advance(61 * time.Second)
	testwait.Eventually(t, "the architect's notice of the first failure", func() bool { return len(rig.architectsTold()) > 0 })
	toldAt := rig.clock.Now()

	rig.clock.advance(4 * time.Minute)
	rig.reportsStoreRecovered(worker.Token, worker.Generation)
	rig.clock.advance(5 * time.Minute)
	again := rig.clock.Now().UTC()
	rig.reportsStoreFailing(worker.Token, worker.Generation, again)
	rig.clock.advance(2 * time.Minute)
	rig.swept()
	rig.clock.advance(20 * time.Minute)
	rig.swept()

	if told := rig.architectsTold(); len(told) != 1 {
		t.Fatalf("the architect was told %+v, want the first notice alone: the second failure came inside the notice's hour", told)
	}
	var held []map[string]any
	for _, line := range rig.sessionStoreLines() {
		if line["bound"] != nil {
			held = append(held, line)
		}
	}
	if len(held) != 1 {
		t.Fatalf("the daemon wrote %d lines naming a bound, want one for the held notice of the second failure: %v", len(held), rig.sessionStoreLines())
	}
	if held[0]["issue"] != podUpdateIssue || held[0]["role"] != string(claim.RoleImplementer) || held[0]["session"] != worker.Session {
		t.Errorf("the held line %v does not name the issue %s, the role %s and the session %s", held[0], podUpdateIssue, claim.RoleImplementer, worker.Session)
	}

	// The bound is the hour and no more: the second failure still stands an hour after the first
	// notice, and is told.
	rig.clock.advance(toldAt.Add(time.Hour + time.Second).Sub(rig.clock.Now()))
	testwait.Eventually(t, "the architect's notice of the failure still standing an hour later", func() bool { return len(rig.architectsTold()) > 1 })
	told := rig.architectsTold()
	if len(told) != 2 {
		t.Fatalf("the architect was told %+v, want two notices an hour apart", told)
	}
	requireNames(t, "the notice of the second failure", told[1].notice.Reason, worker.Session, again.Format(time.RFC3339), sessionStoreError)
}

// More than five claims of one project whose stores begin failing within five minutes of each
// other are one fault of a store they share, which is no architect's to fix: the project's
// controller is told once, in one notice naming each claim's issue, role, session and start and
// the store's error, and no architect of them is told. A claim whose store begins failing within
// five minutes of one already told in the burst joins it: nobody is told again, and the daemon
// writes the line of the notice the burst held.
func TestABurstOfSessionStoreFailuresTellsTheControllerOnceAndNoArchitect(t *testing.T) {
	t.Parallel()
	roles := []claim.Role{claim.RoleArchitect, claim.RolePlanner, claim.RoleImplementer, claim.RoleTester, claim.RoleReviewer, claim.RoleMerger}
	seats := make([]podUpdateRole, 0, len(roles))
	for _, role := range roles {
		seats = append(seats, readyWorker(role))
	}
	rig := newPodUpdateRig(t, seats...)
	// The seventh claim, of another issue of the project, whose store begins failing after the
	// burst was told.
	const lateIssue = "LEGION-485"
	late := supervise.Claim{
		Token: mustClaimToken(t, lateIssue, claim.RoleImplementer), Project: podUpdateProject, Tree: lateIssue, Issue: lateIssue,
		Role: claim.RoleImplementer, Generation: 1, State: supervise.StateReady, PluginContract: api.DaemonAPIVersion,
		Session: "ses_late_implementer", SessionFile: "/sessions/late_implementer.jsonl",
	}
	seedWorkflowTreeIn(t, rig.st, podUpdateProject, record.Issue{
		Key: lateIssue, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION",
		Tree: lateIssue, Title: "The late failure", Phase: phase.Implementing, Generation: 1, Status: "in_progress",
		HandedOver: true, Rank: "V",
	}, late)

	sinces := map[claim.Role]time.Time{}
	for _, role := range roles {
		c := rig.claimOf(role)
		sinces[role] = rig.clock.Now().UTC()
		rig.reportsStoreFailing(c.Token, c.Generation, sinces[role])
		rig.clock.advance(5 * time.Second)
	}
	rig.sweeping()
	rig.clock.advance(sinces[claim.RoleArchitect].Add(61 * time.Second).Sub(rig.clock.Now()))
	testwait.Eventually(t, "the controller's notice of the burst", func() bool { return len(rig.controllersTold()) > 0 })
	rig.clock.advance(2 * time.Minute)
	rig.swept()

	burst := rig.controllersTold()
	if len(burst) != 1 {
		t.Fatalf("the controller was told %+v, want one notice of the burst", burst)
	}
	for _, role := range roles {
		c := rig.claimOf(role)
		requireNames(t, "the burst's notice", burst[0].notice.Reason, podUpdateIssue+"'s "+string(role), c.Session, sinces[role].Format(time.RFC3339))
	}
	requireNames(t, "the burst's notice", burst[0].notice.Reason, sessionStoreError)
	if told := rig.architectsTold(); len(told) != 0 {
		t.Fatalf("architects were told %+v of a burst, want none", told)
	}

	lateSince := rig.clock.Now().UTC()
	rig.reportsStoreFailing(late.Token, late.Generation, lateSince)
	rig.clock.advance(61 * time.Second)
	rig.swept()
	rig.clock.advance(10 * time.Minute)
	rig.swept()

	if told := rig.architectsTold(); len(told) != 0 {
		t.Errorf("architects were told %+v, want none: the late claim joined the burst", told)
	}
	if burst := rig.controllersTold(); len(burst) != 1 {
		t.Errorf("the controller was told %+v, want the one notice of the burst: the late claim joins it with no notice of its own", burst)
	}
	var joined []map[string]any
	for _, line := range rig.sessionStoreLines() {
		if line["session"] == late.Session {
			joined = append(joined, line)
		}
	}
	if len(joined) != 1 || joined[0]["bound"] == nil || joined[0]["issue"] != lateIssue || joined[0]["role"] != string(claim.RoleImplementer) {
		t.Errorf("the daemon wrote %v for the late claim, want one line naming %s, its role, its session and the burst's bound", joined, lateIssue)
	}
}
