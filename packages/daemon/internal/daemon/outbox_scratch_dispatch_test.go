package daemon

//ci:builds packages/envoy ./cmd/dispatch startScratchDispatchOn starts the dispatch command testbuild.EnvoyBinary builds, which runs go build ./cmd/dispatch in packages/envoy (`packages/daemon/internal/testbuild/testbuild.go#buildEnvoy`)

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/testbuild"
	"github.com/sjawhar/legion/daemon/internal/testpg/testdb"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

func TestOutboxMessageCrashRecoveryPostsOnceAgainstScratchDispatch(t *testing.T) {
	t.Parallel()
	baseURL, token := startScratchDispatch(t)
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	issue := createScratchDispatchIssue(t, baseURL)
	now := time.Now().UTC()
	enqueueOutbox(t, pool, records, mustOutboxRow(t, issue, record.MessagePost{Body: "The durable message."}, now))

	first := &outbox{
		dispatchProject: "LEGION",
		pool:            pool, records: crashAfterPostStore{Store: records}, dispatch: dispatch.New(baseURL, dispatch.BearerTransport(token, nil)),
		now: func() time.Time { return now },
	}
	if err := first.RunOnce(context.Background()); err != nil {
		t.Fatalf("first delivery = %v; a failed finish leaves the row leased and is not the tick's error", err)
	}
	if remaining := outboxRows(t, pool); remaining != 1 {
		t.Fatalf("outbox rows after crash = %d, want leased unfinished row", remaining)
	}

	restarted := &outbox{
		dispatchProject: "LEGION",
		pool:            pool, records: records, dispatch: dispatch.New(baseURL, dispatch.BearerTransport(token, nil)),
		now: func() time.Time { return now.Add(outboxLease + time.Second) },
	}
	if err := restarted.RunOnce(context.Background()); err != nil {
		t.Fatalf("restart recovery: %v", err)
	}
	if remaining := outboxRows(t, pool); remaining != 0 {
		t.Fatalf("outbox rows after recovery = %d, want finished row", remaining)
	}

	bodies, err := dispatch.New(baseURL, dispatch.BearerTransport(token, nil)).MessageBodiesSince(context.Background(), issue, now.Add(-time.Second))
	if err != nil {
		t.Fatalf("read real Dispatch messages: %v", err)
	}
	markers := 0
	for _, body := range bodies {
		if strings.Contains(body, "<!-- legion-outbox:") {
			markers++
		}
	}
	if markers != 1 {
		t.Fatalf("outbox-marked messages = %d in %#v, want one post across crash recovery", markers, bodies)
	}
}

// A long READY packet posts whole and reads back byte for byte. Dispatch bounds a message body
// block by block, so a 40,000-unit packet of ordinary paragraphs is taken; its message.created
// event carries only the first part of it and says so (body_truncated), which leaves the outbox's
// marker off the end of the event's body. The runner reads such a body from the message itself,
// so the row run again after a crash before its finish finds its own marker and posts nothing,
// and the issue holds exactly one message.
func TestOutboxPostsALongPacketWholeAndOnceAgainstScratchDispatch(t *testing.T) {
	t.Parallel()
	baseURL, token := startScratchDispatch(t)
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	issue := createScratchDispatchIssue(t, baseURL)
	now := time.Now().UTC()
	// Six paragraphs of 6,665 characters, 40,000 units in all: every block is far under
	// Dispatch's own block bound and the body is far over the units one event carries.
	packet := record.MessagePost{Body: strings.TrimSuffix(strings.Repeat(strings.Repeat("x", 6665)+"\n\n", 6), "\n\n")}
	enqueueOutbox(t, pool, records, mustOutboxRow(t, issue, packet, now))
	var id int64
	if err := pool.QueryRow(context.Background(), "select id from outbox where "+notStateRows).Scan(&id); err != nil {
		t.Fatalf("read the queued row: %v", err)
	}

	first := &outbox{
		dispatchProject: "LEGION", log: quietLogger(),
		pool: pool, records: crashAfterPostStore{Store: records}, dispatch: dispatch.New(baseURL, dispatch.BearerTransport(token, nil)),
		now: func() time.Time { return now },
	}
	if err := first.RunOnce(context.Background()); err != nil {
		t.Fatalf("first delivery = %v; a failed finish leaves the row leased and is not the tick's error", err)
	}
	restarted := &outbox{
		dispatchProject: "LEGION", log: quietLogger(),
		pool: pool, records: records, dispatch: dispatch.New(baseURL, dispatch.BearerTransport(token, nil)),
		now: func() time.Time { return now.Add(outboxLease + time.Second) },
	}
	if err := restarted.RunOnce(context.Background()); err != nil {
		t.Fatalf("restart recovery: %v", err)
	}
	if remaining := outboxRows(t, pool); remaining != 0 {
		t.Fatalf("outbox rows after recovery = %d, want the row posted and finished", remaining)
	}

	bodies, err := dispatch.New(baseURL, dispatch.BearerTransport(token, nil)).MessageBodiesSince(context.Background(), issue, now.Add(-time.Second))
	if err != nil {
		t.Fatalf("read real Dispatch messages: %v", err)
	}
	if want := packet.Posted(id); len(bodies) != 1 || bodies[0] != want {
		t.Fatalf("Dispatch messages = %d of %d characters, want the 40,000-character packet posted once, whole, under row %d's marker",
			len(bodies), len(strings.Join(bodies, "")), id)
	}
	cut, truncated := messageCreatedEvent(t, baseURL, token, issue)
	if !truncated || strings.Contains(cut, record.MessageMarker(id)) {
		t.Fatalf("the message.created event carries %d characters (truncated=%v), want a cut body with no marker in it", len(cut), truncated)
	}
}

// A post Dispatch refuses the same way on every attempt — one block of the body over Dispatch's
// own block bound — is no retry's to repair. The row finishes and its issue's architect is told
// Dispatch's own refusal, rather than the row climbing its backoff for ever and telling nobody.
func TestOutboxFinishesAPostDispatchRefusesAndTellsTheArchitectAgainstScratchDispatch(t *testing.T) {
	t.Parallel()
	baseURL, token := startScratchDispatch(t)
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	issue := createScratchDispatchIssue(t, baseURL)
	now := time.Now().UTC()
	over := record.MessagePost{Body: "READY #42 at head for " + issue + "\n\n" + strings.Repeat("x", 32001)}
	enqueueOutbox(t, pool, records, mustOutboxRow(t, issue, over, now))
	runner := &outbox{
		dispatchProject: "LEGION", log: quietLogger(),
		pool: pool, records: records, dispatch: dispatch.New(baseURL, dispatch.BearerTransport(token, nil)),
		now: func() time.Time { return now },
	}
	if err := runner.RunOnce(context.Background()); err != nil {
		t.Fatalf("run the outbox: %v", err)
	}

	bodies, err := dispatch.New(baseURL, dispatch.BearerTransport(token, nil)).MessageBodiesSince(context.Background(), issue, now.Add(-time.Second))
	if err != nil {
		t.Fatalf("read real Dispatch messages: %v", err)
	}
	if len(bodies) != 0 {
		t.Fatalf("Dispatch messages = %#v, want the refused post on no issue", bodies)
	}
	var kind string
	var payload []byte
	if err := pool.QueryRow(context.Background(), "select kind, payload from outbox where "+notStateRows).Scan(&kind, &payload); err != nil {
		t.Fatalf("read the row left behind: %v", err)
	}
	if kind != string(record.OutboxKindNotice) {
		t.Fatalf("the one row left is a %s row, want the refused post finished and its architect's notice in its place", kind)
	}
	var notice record.Notice
	if err := json.Unmarshal(payload, &notice); err != nil {
		t.Fatalf("decode the notice: %v", err)
	}
	if notice.Kind != "message-refused" || notice.Role != claim.RoleArchitect ||
		!strings.Contains(notice.Reason, issue) || !strings.Contains(notice.Reason, "CAP_EXCEEDED") ||
		!strings.Contains(notice.Reason, "READY #42 at head for "+issue) {
		t.Fatalf("the notice = %+v, want message-refused to the architect naming %s, the message's first line and Dispatch's refusal", notice, issue)
	}
}

// A done write posts its reason on the issue before it writes the status, and the reason here is
// one block over Dispatch's own block bound, which Dispatch refuses on every attempt. Against the
// real server: no message is on the issue, the status is set all the same, and the issue's
// architect is told what was refused and that the status stands. Without that, the daemon's
// record would read done while the Dispatch issue kept the status it had, and nobody would know.
func TestOutboxSetsTheStatusOfAWriteWhoseReasonDispatchRefusesAgainstScratchDispatch(t *testing.T) {
	t.Parallel()
	baseURL, token := startScratchDispatch(t)
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	issue := createScratchDispatchIssue(t, baseURL)
	client := dispatch.New(baseURL, dispatch.BearerTransport(token, nil))
	before, err := client.GetIssue(context.Background(), issue)
	if err != nil {
		t.Fatalf("read the scratch issue: %v", err)
	}
	now := time.Now().UTC()
	reason := "Closed by its architect before its first phase: " + strings.Repeat("x", 32001)
	enqueueOutbox(t, pool, records, mustOutboxRow(t, issue,
		record.OrdinaryStatusWrite("done", before.Status, reason), now))
	runner := &outbox{
		dispatchProject: "LEGION", log: quietLogger(),
		pool: pool, records: records, dispatch: dispatch.New(baseURL, dispatch.BearerTransport(token, nil)),
		now: func() time.Time { return now },
	}
	if err := runner.RunOnce(context.Background()); err != nil {
		t.Fatalf("run the outbox: %v", err)
	}

	bodies, err := client.MessageBodiesSince(context.Background(), issue, now.Add(-time.Second))
	if err != nil {
		t.Fatalf("read real Dispatch messages: %v", err)
	}
	if len(bodies) != 0 {
		t.Fatalf("Dispatch messages = %#v, want the refused reason on no issue", bodies)
	}
	after, err := client.GetIssue(context.Background(), issue)
	if err != nil {
		t.Fatalf("read the scratch issue after the run: %v", err)
	}
	if after.Status != "done" {
		t.Fatalf("the issue stands at %q, want done written though its reason was refused", after.Status)
	}
	var kind string
	var payload []byte
	if err := pool.QueryRow(context.Background(), "select kind, payload from outbox where "+notStateRows).Scan(&kind, &payload); err != nil {
		t.Fatalf("read the row left behind: %v", err)
	}
	if kind != string(record.OutboxKindNotice) {
		t.Fatalf("the one row left is a %s row, want the status row finished and its architect's notice in its place", kind)
	}
	var notice record.Notice
	if err := json.Unmarshal(payload, &notice); err != nil {
		t.Fatalf("decode the notice: %v", err)
	}
	if notice.Kind != "message-refused" || notice.Role != claim.RoleArchitect ||
		!strings.Contains(notice.Reason, issue) || !strings.Contains(notice.Reason, "CAP_EXCEEDED") ||
		!strings.Contains(notice.Reason, "The status done was set on the issue all the same") {
		t.Fatalf("the notice = %+v, want message-refused to the architect naming %s, Dispatch's refusal and the status it set", notice, issue)
	}
}

// The daemon's board lane publishes an issue's role sessions to a real Dispatch built from this
// tree, and the route stores them: the tester's ready claim is the one holder Dispatch lists. Once
// the claim retires, the next publication sends the empty set and the tester's session leaves the
// list. The first read is made before the lane runs, so the holder Dispatch lists after it is the
// publication's own and not a set something else wrote.
func TestRoleHoldersLandInScratchDispatch(t *testing.T) {
	t.Parallel()
	baseURL, token := startScratchDispatch(t)
	issue := createScratchDispatchIssue(t, baseURL)
	r := newRoleHoldersRig(t)
	r.runner.dispatch = dispatch.New(baseURL, dispatch.BearerTransport(token, nil))
	logs := &syncBuffer{}
	r.runner.log = slog.New(slog.NewTextHandler(logs, nil))
	tester := r.seedClaim(issue, claim.RoleTester, "ses-tester", "ready", time.Minute)

	holders := func() []dispatch.RoleHolder {
		t.Helper()
		request, err := http.NewRequest(http.MethodGet, baseURL+"/api/v1/issues/"+url.PathEscape(issue)+"/role-holders", nil)
		if err != nil {
			t.Fatalf("build the role-holder read: %v", err)
		}
		request.Header.Set("Authorization", "Bearer "+token)
		response, err := pollClient.Do(request)
		if err != nil {
			t.Fatalf("read the role holders of %s: %v", issue, err)
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(response.Body)
			t.Fatalf("read the role holders of %s = %s %s", issue, response.Status, body)
		}
		var read struct {
			Issue   string                `json:"issue"`
			Holders []dispatch.RoleHolder `json:"holders"`
		}
		if err := json.NewDecoder(response.Body).Decode(&read); err != nil {
			t.Fatalf("decode the role holders of %s: %v", issue, err)
		}
		if read.Issue != issue {
			t.Fatalf("the read answered issue %q, want %s", read.Issue, issue)
		}
		return read.Holders
	}
	run := func() {
		t.Helper()
		if err := r.runner.RunOnce(r.ctx); err != nil {
			t.Fatalf("run the outbox: %v", err)
		}
		if rows := outboxRows(t, r.pool); rows != 0 {
			t.Fatalf("the outbox holds %d rows after the run, want the publication finished", rows)
		}
	}

	r.enqueue(issue)
	if got := holders(); len(got) != 0 {
		t.Fatalf("before the lane ran, Dispatch lists %+v, want no holder", got)
	}
	run()
	if got, want := holders(), []dispatch.RoleHolder{{Role: "tester", Session: "ses-tester"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("after the publication, Dispatch lists %+v, want %+v", got, want)
	}

	r.moveClaim(tester, "retired")
	r.enqueue(issue)
	run()
	if got := holders(); len(got) != 0 {
		t.Fatalf("after the tester retired, Dispatch lists %+v, want no holder", got)
	}

	// A set the route refuses ends the row on the first answer, rather than being sent for as long
	// as the issue lives: a claim whose session is no session id, which the route answers 400
	// INVALID_ROLE_HOLDER. The daemon logs one ERROR line naming the issue and the code, and
	// Dispatch keeps the set it holds.
	r.seedClaim(issue, claim.RoleReviewer, "ses reviewer", "ready", time.Minute)
	r.enqueue(issue)
	run()
	if got := holders(); len(got) != 0 {
		t.Fatalf("after the refused publication, Dispatch lists %+v, want the set it held, no holder", got)
	}
	var refused []string
	for _, line := range strings.Split(logs.String(), "\n") {
		if strings.Contains(line, "role-holders publish refused by Dispatch") {
			refused = append(refused, line)
		}
	}
	if len(refused) != 1 || !strings.Contains(refused[0], "level=ERROR") || !strings.Contains(refused[0], "issue="+issue) ||
		!strings.Contains(refused[0], "code=INVALID_ROLE_HOLDER") {
		t.Fatalf("the refusal was logged as %q, want one ERROR line naming %s and INVALID_ROLE_HOLDER", refused, issue)
	}
}

// The status-change client against a real Dispatch built from this tree. Which Dispatch it is
// the test reads from the routes Dispatch lists (servesStatusChange), not from the client's own
// answer. One built before the route (INFRA-4731) answers the call with its router's 404, which
// the client reads as dispatch.NoRoute, the answer the outbox's status row waits on. One that
// serves the route answers each call R24's options make, which the client reads, with its
// parent-status rule off and again on (readEachStatusChangeAnswer).
func TestTheStatusChangeClientReadsEachAnswerOfTheRealDispatch(t *testing.T) {
	t.Parallel()
	ruleOff := startScratchDispatchOn(t, scratchDispatchOptions{env: []string{parentStatusRule + "=off"}})
	if !servesStatusChange(t, ruleOff.base) {
		issue := createScratchDispatchIssue(t, ruleOff.base)
		_, err := dispatch.New(ruleOff.base, dispatch.BearerTransport(ruleOff.token, nil)).ChangeStatus(context.Background(), issue,
			dispatch.StatusChange{Status: "backlog", UnlinkChildren: true, IdempotencyKey: "status-change-no-route"})
		if !dispatch.NoRoute(err) {
			t.Fatalf("a status change against a Dispatch that lists no such route = %v, want its router's 404 (dispatch.NoRoute)", err)
		}
		return
	}
	t.Run("rule off", func(t *testing.T) { readEachStatusChangeAnswer(t, ruleOff, false) })
	// The rule-off Dispatch has served its case, so its server stops before the next one starts
	// (stop leaves a stopped server as it is, so the cleanup that registered it does nothing more).
	ruleOff.stop()
	t.Run("rule on", func(t *testing.T) {
		ruleOn := startScratchDispatchOn(t, scratchDispatchOptions{env: []string{parentStatusRule + "=on"}})
		readEachStatusChangeAnswer(t, ruleOn, true)
	})
}

// parentStatusRule is Dispatch's setting that turns its refusal of a write breaking R24's rule on
// (`on`) or off (unset or `off`). A Dispatch from before the rule reads nothing of it.
const parentStatusRule = "DISPATCH_PARENT_STATUS_RULE"

// readEachStatusChangeAnswer calls the status-change route of d, a Dispatch that serves it, once
// for each answer the client reads, each case on issues of its own: a parent at work with a child
// at work under it, or a child under a parent on the backlog. ruleOn is d's parentStatusRule. A
// dry run answers as the call would with the rule on, whatever the setting.
func readEachStatusChangeAnswer(t *testing.T, d *scratchDispatch, ruleOn bool) {
	t.Helper()
	ctx := context.Background()
	client := dispatch.New(d.base, dispatch.BearerTransport(d.token, nil))
	createScratchDispatchIssue(t, d.base)
	calls := 0
	change := func(key string, call dispatch.StatusChange) (dispatch.StatusChangeResult, error) {
		calls++
		call.IdempotencyKey = fmt.Sprintf("status-change-%d", calls)
		return client.ChangeStatus(ctx, key, call)
	}
	read := func(key string) dispatch.Issue {
		t.Helper()
		issue, err := client.GetIssue(ctx, key)
		if err != nil {
			t.Fatalf("read %s: %v", key, err)
		}
		return issue
	}
	move := func(key, status string) {
		t.Helper()
		if _, err := client.SetStatus(ctx, key, status); err != nil {
			t.Fatalf("move %s to %s: %v", key, status, err)
		}
	}
	// atWork is a parent and a child under it, both in_progress, as Dispatch then shows them.
	atWork := func(name string) (dispatch.Issue, dispatch.Issue) {
		t.Helper()
		parent := createScratchDispatchIssueUnder(t, d.base, name+": the parent", "")
		child := createScratchDispatchIssueUnder(t, d.base, name+": the child", parent)
		move(parent, "in_progress")
		move(child, "in_progress")
		return read(parent), read(child)
	}
	// underBacklog is a child at triage under a parent on the backlog, as Dispatch then shows them.
	underBacklog := func(name string) (dispatch.Issue, dispatch.Issue) {
		t.Helper()
		parent := createScratchDispatchIssueUnder(t, d.base, name+": the parent", "")
		move(parent, "backlog")
		child := createScratchDispatchIssueUnder(t, d.base, name+": the child", parent)
		return read(parent), read(child)
	}

	// No option: the dry run names the child that holds the rule; the call itself is refused the
	// same way with the rule on, and taken with it off.
	parent, child := atWork("no option")
	_, err := change(parent.Key, dispatch.StatusChange{Status: "backlog", ExpectedSequence: parent.LastSeq, DryRun: true})
	assertParentStatusConflict(t, err, dispatch.ConflictIssue{Key: child.Key, Status: "in_progress"})
	result, err := change(parent.Key, dispatch.StatusChange{Status: "backlog", ExpectedSequence: parent.LastSeq})
	if ruleOn {
		assertParentStatusConflict(t, err, dispatch.ConflictIssue{Key: child.Key, Status: "in_progress"})
		if after := read(parent.Key); after.Status != "in_progress" || after.LastSeq != parent.LastSeq {
			t.Fatalf("%s after the refused call = %s at %d, want it untouched at in_progress and %d", parent.Key, after.Status, after.LastSeq, parent.LastSeq)
		}
	} else {
		if err != nil {
			t.Fatalf("a move with no option with the rule off = %v, want it taken", err)
		}
		assertEntries(t, result.Changed, parent.Key+" backlog parent=none written")
	}

	// A stale sequence: 409 ISSUE_CHANGED, read as the PATCH's is, and nothing written.
	parent, child = atWork("options")
	_, err = change(parent.Key, dispatch.StatusChange{Status: "backlog", ExpectedSequence: parent.LastSeq - 1, IncludeChildren: true})
	var refusal *dispatch.Error
	if !errors.As(err, &refusal) || refusal.Status != http.StatusConflict || refusal.Code != "ISSUE_CHANGED" || len(refusal.Changed) != 0 {
		t.Fatalf("a call at a stale sequence = %#v (%v), want 409 ISSUE_CHANGED with no step committed", refusal, err)
	}
	if _, permanent := dispatch.PermanentRefusal(err); permanent {
		t.Fatalf("PermanentRefusal(%v) = permanent, want a stale read the caller reads again", err)
	}
	if after := read(child.Key); after.Status != "in_progress" || after.LastSeq != child.LastSeq {
		t.Fatalf("%s after the stale call = %s at %d, want it untouched", child.Key, after.Status, after.LastSeq)
	}

	// A dry run with include_children: what each step would leave, deepest first and the named
	// issue last, each at the sequence it stands at, and nothing written.
	result, err = change(parent.Key, dispatch.StatusChange{Status: "backlog", ExpectedSequence: parent.LastSeq, IncludeChildren: true, DryRun: true})
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	assertEntries(t, result.Changed, child.Key+" backlog parent="+parent.Key+" not written", parent.Key+" backlog parent=none not written")
	if result.Changed[0].PreviousLastSeq != child.LastSeq || result.Changed[1].PreviousLastSeq != parent.LastSeq {
		t.Fatalf("the dry run's entries = %#v, want each at the sequence it stands at (%d, %d)", result.Changed, child.LastSeq, parent.LastSeq)
	}
	if after := read(parent.Key); after.Status != "in_progress" || after.LastSeq != parent.LastSeq {
		t.Fatalf("%s after the dry run = %s at %d, want it untouched", parent.Key, after.Status, after.LastSeq)
	}

	// include_children: the child's step, then the named issue's, each with the issue as its step
	// left it; the child's step adds its child.status event to the named issue first.
	result, err = change(parent.Key, dispatch.StatusChange{Status: "backlog", ExpectedSequence: parent.LastSeq, IncludeChildren: true})
	if err != nil {
		t.Fatalf("include_children: %v", err)
	}
	assertEntries(t, result.Changed, child.Key+" backlog parent="+parent.Key+" written", parent.Key+" backlog parent=none written")
	childEntry, parentEntry := result.Changed[0], result.Changed[1]
	if childEntry.PreviousLastSeq != child.LastSeq || childEntry.Issue.LastSeq <= childEntry.PreviousLastSeq ||
		parentEntry.PreviousLastSeq <= parent.LastSeq || parentEntry.Issue.LastSeq <= parentEntry.PreviousLastSeq {
		t.Fatalf("include_children's entries = %#v, want the child's from %d and the parent's from past %d, each moved on by its step", result.Changed, child.LastSeq, parent.LastSeq)
	}
	if result.Issue.Key != parent.Key || result.Issue.Status != "backlog" || result.Issue.LastSeq != parentEntry.Issue.LastSeq {
		t.Fatalf("include_children's issue = %#v, want %s at backlog as its own step left it", result.Issue, parent.Key)
	}

	// unlink_children: the child, unlinked and left at its status, then the named issue.
	parent, child = atWork("unlink")
	result, err = change(parent.Key, dispatch.StatusChange{Status: "backlog", ExpectedSequence: parent.LastSeq, UnlinkChildren: true})
	if err != nil {
		t.Fatalf("unlink_children: %v", err)
	}
	assertEntries(t, result.Changed, child.Key+" in_progress parent=none written", parent.Key+" backlog parent=none written")
	if result.Changed[0].PreviousLastSeq != child.LastSeq || result.Changed[0].Issue.Parent != nil {
		t.Fatalf("unlink_children's entry of %s = %#v, want it unlinked from %d", child.Key, result.Changed[0], child.LastSeq)
	}

	// A child into work under a parent on the backlog: refused with the rule on and taken with it
	// off; with include_parent, the parent's step first, then the child's.
	parent, child = underBacklog("no include_parent")
	result, err = change(child.Key, dispatch.StatusChange{Status: "todo", ExpectedSequence: child.LastSeq})
	if ruleOn {
		assertParentStatusConflict(t, err, dispatch.ConflictIssue{Key: parent.Key, Status: "backlog"})
	} else {
		if err != nil {
			t.Fatalf("a child's todo under a parent on the backlog with the rule off = %v, want it taken", err)
		}
		assertEntries(t, result.Changed, child.Key+" todo parent="+parent.Key+" written")
	}
	parent, child = underBacklog("include_parent")
	result, err = change(child.Key, dispatch.StatusChange{Status: "todo", ExpectedSequence: child.LastSeq, IncludeParent: true})
	if err != nil {
		t.Fatalf("include_parent: %v", err)
	}
	assertEntries(t, result.Changed, parent.Key+" in_progress parent=none written", child.Key+" todo parent="+parent.Key+" written")
	if result.Changed[0].PreviousLastSeq != parent.LastSeq {
		t.Fatalf("include_parent's entry of %s = %#v, want it from %d", parent.Key, result.Changed[0], parent.LastSeq)
	}
}

// assertParentStatusConflict holds err to Dispatch's 409 PARENT_STATUS_CONFLICT naming exactly
// want, with no step committed, which no caller drops as permanent.
func assertParentStatusConflict(t *testing.T, err error, want ...dispatch.ConflictIssue) {
	t.Helper()
	var refusal *dispatch.Error
	if !dispatch.ParentStatusConflict(err) || !errors.As(err, &refusal) {
		t.Fatalf("status change = %v, want 409 PARENT_STATUS_CONFLICT", err)
	}
	if !reflect.DeepEqual(refusal.Issues, want) || len(refusal.Changed) != 0 {
		t.Fatalf("PARENT_STATUS_CONFLICT = %#v, want it to name %#v with no step committed", refusal, want)
	}
	if _, permanent := dispatch.PermanentRefusal(err); permanent {
		t.Fatalf("PermanentRefusal(%v) = permanent, want a refusal its caller decides on", err)
	}
}

// assertEntries holds a status change's entries, in order, to want, each as `<key> <status>
// parent=<parent or none> written`, or `not written` for an entry with no issue (a dry run's).
// A written entry's issue is the same issue at the entry's status and parent.
func assertEntries(t *testing.T, entries []dispatch.StatusChangeEntry, want ...string) {
	t.Helper()
	var got []string
	for _, entry := range entries {
		parent, written := "none", "not written"
		if entry.Parent != nil {
			parent = *entry.Parent
		}
		if issue := entry.Issue; issue != nil {
			written = "written"
			if issue.Key != entry.Key || issue.Status != entry.Status || !reflect.DeepEqual(issue.Parent, entry.Parent) {
				t.Fatalf("entry %#v carries %#v, want the same issue at its status and parent", entry, issue)
			}
		}
		got = append(got, entry.Key+" "+entry.Status+" parent="+parent+" "+written)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("changed = %q, want %q", got, want)
	}
}

// messageCreatedEvent is the issue's message.created event as Dispatch serves it: the body the
// event carries, and whether Dispatch says that body is only the first part of the message's.
func messageCreatedEvent(t *testing.T, baseURL, token, issue string) (string, bool) {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, baseURL+"/api/v1/issues/"+url.PathEscape(issue)+"/events?limit=200", nil)
	if err != nil {
		t.Fatalf("create the events request: %v", err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := (&http.Client{Timeout: 10 * time.Second}).Do(request)
	if err != nil {
		t.Fatalf("GET the issue's events: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("GET the issue's events = %s", response.Status)
	}
	var events []struct {
		Type    string `json:"type"`
		Payload struct {
			Body          string `json:"body"`
			BodyTruncated bool   `json:"body_truncated"`
		} `json:"payload"`
	}
	if err := json.NewDecoder(response.Body).Decode(&events); err != nil {
		t.Fatalf("decode the issue's events: %v", err)
	}
	for _, event := range events {
		if event.Type == "message.created" {
			return event.Payload.Body, event.Payload.BodyTruncated
		}
	}
	t.Fatal("the issue has no message.created event")
	return "", false
}

type crashAfterPostStore struct{ record.Store }

func (crashAfterPostStore) FinishOutbox(context.Context, pgx.Tx, int64, string) error {
	return errors.New("simulated crash before finish")
}

// startScratchDispatch starts the real Dispatch server against a scratch Postgres database of its
// own with no NATS at all, and returns its base URL and agent token. Nothing it does reaches a
// bus: a test that only drives the outbox against it wants no events.
func startScratchDispatch(t *testing.T) (string, string) {
	t.Helper()
	d := startScratchDispatchOn(t, scratchDispatchOptions{})
	return d.base, d.token
}

// startScratchDispatchWithNATS is startScratchDispatch with its events turned on: the same server,
// publishing every issue event on natsURL, which is the NATS server the test holds
// (testnats.URL). Dispatch reconciles the ENVOY_NOTIFICATIONS stream there on its first
// connection, which it makes in the background once it starts, so the caller creates none — and
// must take the NATS server before this call, since testnats.URL empties it of every stream.
func startScratchDispatchWithNATS(t *testing.T, natsURL string) (string, string) {
	t.Helper()
	if natsURL == "" {
		t.Fatal("startScratchDispatchWithNATS was given no NATS server; use startScratchDispatch for a server with no events")
	}
	d := startScratchDispatchOn(t, scratchDispatchOptions{natsURL: natsURL})
	return d.base, d.token
}

// scratchDispatchOptions are what a scratch Dispatch server's callers vary.
type scratchDispatchOptions struct {
	// natsURL is the NATS server the server publishes its events on; empty runs it with NATS off.
	natsURL string
	// root is the checkout whose packages/envoy the server runs in and whose
	// packages/dispatch/web/dist it serves; empty is this one, four directories above the
	// package's. The binary is not built from it: testbuild.EnvoyBinary builds the checkout
	// testbuild.RootEnv names, else the one holding the working directory.
	root string
	// log, when set, receives the server's output as well.
	log io.Writer
	// env is settings of the caller's own, each NAME=value, set after every setting the server
	// otherwise runs with, so a caller adds one the scratch server does not set or replaces one
	// it does (os/exec keeps the last value of a name): DISPATCH_SERVER_URL naming an origin the
	// server is not served at, the secrets broker its broker tokens are read back at
	// (DISPATCH_AGENT_SECRETS_URL and its account token, DISPATCH_PROOF_ORIGINS), a listener
	// (ENVOY_URL) or a GitHub App and the GitHub it calls. Nothing else of the caller's
	// environment named DISPATCH_, ENVOY_ or NATS_ reaches the server. Dispatch's parent-status
	// rule (DISPATCH_PARENT_STATUS_RULE) is on unless env names it, as it is in production once the
	// rule rolls out (decision 23 of dispatch://INFRA-4730). A Dispatch whose settings table does
	// not name a setting reads nothing of it.
	env []string
}

// scratchDispatch is a real Dispatch server a test builds and runs: its binary, its own database,
// the port it serves on (kept across a restart, since whatever the test pointed at the server
// names it), and the running process. stop and start take it down and bring it back on the same
// port and database; the test's end stops it and drops the database.
type scratchDispatch struct {
	t        *testing.T
	binary   string
	dir      string
	webDist  string
	database string
	natsURL  string
	token    string
	home     string
	log      io.Writer
	env      []string
	// socket is the port's one socket from the first start to the test's end (ownPort): it listens
	// while a Dispatch serves on it, and stop takes it out of listening, the port kept.
	socket  *os.File
	port    int
	base    string
	command *exec.Cmd
	exited  chan error
	output  *syncBuffer
}

// startScratchDispatchOn gives the server a scratch database of its own on the test DSN's
// Postgres (testdb.PlainDatabase: a child process reads it) and starts the cmd/dispatch this
// process built.
func startScratchDispatchOn(t *testing.T, options scratchDispatchOptions) *scratchDispatch {
	t.Helper()
	database := testdb.PlainDatabase(t, "dispatch_scratch_test")
	root := options.root
	if root == "" {
		root = filepath.Join("..", "..", "..", "..")
	}
	root, err := filepath.Abs(root)
	if err != nil {
		t.Fatalf("resolve repository root: %v", err)
	}
	env := options.env
	if !slices.ContainsFunc(env, func(setting string) bool { return strings.HasPrefix(setting, parentStatusRule+"=") }) {
		env = append(slices.Clone(env), parentStatusRule+"=on")
	}
	d := &scratchDispatch{
		t: t, binary: testbuild.EnvoyBinary(t, "./cmd/dispatch"), dir: filepath.Join(root, "packages", "envoy"),
		webDist: filepath.Join(root, "packages", "dispatch", "web", "dist"), database: database,
		natsURL: options.natsURL, token: scratchAgentToken, home: filepath.Join(t.TempDir(), "home"),
		log: options.log, env: env,
	}
	if err := os.MkdirAll(d.home, 0o700); err != nil {
		t.Fatalf("create scratch Dispatch home: %v", err)
	}
	t.Cleanup(d.stop)
	d.start()
	return d
}

// start runs the server and returns once it has logged that it serves on its port and GET /api/v1
// there answers 200. The first start serves on a port the kernel chooses; a restart serves on the
// same one again.
func (d *scratchDispatch) start() {
	t := d.t
	t.Helper()
	// Readiness is a server process starting and migrating a fresh database, whose time is the
	// machine's load: bounded by the test binary's own deadline rather than a fixed span, so a
	// slow start is not read as a server that will never answer.
	deadline := time.Now().Add(2 * time.Minute)
	if testDeadline, ok := t.Deadline(); ok && testDeadline.Add(-10*time.Second).Before(deadline) {
		deadline = testDeadline.Add(-10 * time.Second)
	}
	// Dispatch serves on the port's own socket (DISPATCH_LISTEN_FD), which the test owns from the
	// first start to its end, so no other process can take the port before Dispatch serves it, nor
	// between a stop and the next start.
	if d.socket == nil {
		d.socket, d.port = ownPort(t)
		d.base = fmt.Sprintf("http://127.0.0.1:%d", d.port)
	}
	// The socket listens with SO_REUSEADDR, so the connections a Dispatch served and closed, which
	// wait out TIME_WAIT on the port, share it with the next listen; stop drops it again.
	fd := int(d.socket.Fd())
	if err := syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1); err != nil {
		t.Fatalf("let scratch Dispatch's socket listen past its closed connections: %v", err)
	}
	if err := syscall.Listen(fd, syscall.SOMAXCONN); err != nil {
		t.Fatalf("listen on scratch Dispatch's port %d: %v", d.port, err)
	}
	listening := fmt.Sprintf(`msg="dispatch: listening" addr=127.0.0.1:%d`, d.port)
	command := exec.Command(d.binary)
	command.Dir = d.dir
	// Nothing of the caller's Dispatch, Envoy or NATS settings reaches the scratch server, as
	// run-server.sh drops them: a Legion pane exports DISPATCH_URL and friends.
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "DISPATCH_") && !strings.HasPrefix(entry, "ENVOY_") && !strings.HasPrefix(entry, "NATS_") && !strings.HasPrefix(entry, "HOME=") {
			command.Env = append(command.Env, entry)
		}
	}
	// The bus is the one difference between the two forms: NATS off, or the test's own server,
	// whose ENVOY_NOTIFICATIONS stream this process reconciles as it starts.
	bus := "DISPATCH_NATS_DISABLED=1"
	if d.natsURL != "" {
		bus = "NATS_URLS=" + d.natsURL
	}
	command.Env = append(command.Env,
		"DATABASE_URL="+d.database,
		"DISPATCH_AGENT_TOKEN="+d.token,
		"DISPATCH_IDENTITY=header:X-Dispatch-User",
		"DISPATCH_IDENTITY_HEADER_TRUSTED=1",
		bus,
		// ExtraFiles' first entry is descriptor 3 in Dispatch.
		"DISPATCH_LISTEN_FD=3",
		"DISPATCH_LISTEN_HOST=127.0.0.1",
		fmt.Sprintf("DISPATCH_PORT=%d", d.port),
		"DISPATCH_SERVER_URL="+d.base,
		"DISPATCH_WEB_DIST="+d.webDist,
		"HOME="+d.home,
	)
	command.Env = append(command.Env, d.env...)
	command.ExtraFiles = []*os.File{d.socket}
	output := &syncBuffer{}
	command.Stdout = output
	if d.log != nil {
		command.Stdout = io.MultiWriter(output, d.log)
	}
	command.Stderr = command.Stdout
	// The test keeps its copy of the socket, so the port stays its own once Dispatch exits; stop
	// takes it out of listening then, so a dial is refused while no Dispatch serves.
	err := command.Start()
	if err != nil {
		t.Fatalf("start scratch Dispatch: %v", err)
	}
	exited := make(chan error, 1)
	go func() { exited <- command.Wait() }()
	d.command, d.exited, d.output = command, exited, output
	if !testwait.Holds(time.Until(deadline), 50*time.Millisecond, func() bool {
		select {
		case err := <-exited:
			d.command = nil
			t.Fatalf("scratch Dispatch exited before it was ready (%v):\n%s", err, output.String())
		default:
		}
		// The wait names this start's process by its own output, a buffer each start makes anew:
		// its line that it serves the port, then its 200. The port is the socket's alone (ownPort),
		// the test never accepts on its copy, and stop waited the last Dispatch out, so this process
		// is the only one that can answer there anyway; the line makes that explicit.
		if !strings.Contains(output.String(), listening) {
			return false
		}
		if response, err := pollClient.Get(d.base + "/api/v1"); err == nil {
			response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return true
			}
		}
		return false
	}) {
		t.Fatalf("scratch Dispatch did not become ready:\n%s", output.String())
	}
}

// ownPort is a loopback port the kernel chooses, owned until t ends by a TCP socket bound to that
// port by number and listening only while a Dispatch serves it. A socket bound by number keeps its
// port when it stops listening (Linux's SOCK_BINDPORT_LOCK), where one bound to port 0 gives the
// port back, so the port's number comes from a probe: the probe binds port 0 with SO_REUSEADDR, the
// socket binds the probe's port beside it with SO_REUSEADDR, the probe closes, and the socket drops
// SO_REUSEADDR before it first listens. From then on no other socket of any user binds the port,
// whether it sets SO_REUSEADDR, SO_REUSEPORT or neither, and while the socket does not listen
// every dial is refused. stop takes it out of listening with shutdown(SHUT_RD), SO_REUSEADDR
// dropped first: a listening socket shares its port with no bind, and a socket that is neither
// listening nor sets SO_REUSEADDR shares it with none either.
func ownPort(t *testing.T) (*os.File, int) {
	t.Helper()
	probe := ownPortSocket(t, "the probe of scratch Dispatch's port")
	if err := syscall.SetsockoptInt(int(probe.Fd()), syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1); err != nil {
		t.Fatalf("let the probe of scratch Dispatch's port share it: %v", err)
	}
	if err := syscall.Bind(int(probe.Fd()), &syscall.SockaddrInet4{Addr: [4]byte{127, 0, 0, 1}}); err != nil {
		t.Fatalf("take a loopback port for scratch Dispatch: %v", err)
	}
	name, err := syscall.Getsockname(int(probe.Fd()))
	if err != nil {
		t.Fatalf("read scratch Dispatch's port: %v", err)
	}
	port := name.(*syscall.SockaddrInet4).Port
	owner := ownPortSocket(t, "scratch Dispatch's port")
	fd := int(owner.Fd())
	if err := syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1); err != nil {
		t.Fatalf("let scratch Dispatch's socket bind beside the probe: %v", err)
	}
	if err := syscall.Bind(fd, &syscall.SockaddrInet4{Port: port, Addr: [4]byte{127, 0, 0, 1}}); err != nil {
		t.Fatalf("bind scratch Dispatch's socket to port %d: %v", port, err)
	}
	if err := probe.Close(); err != nil {
		t.Fatalf("close the probe of scratch Dispatch's port: %v", err)
	}
	if err := syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 0); err != nil {
		t.Fatalf("keep scratch Dispatch's port from being shared: %v", err)
	}
	// A listen reads the socket's own SO_REUSEADDR again, which no other socket of the port now
	// sets, so from it on the kernel shares the port with no other bind; the shutdown takes the
	// socket out of listening until the first start.
	if err := syscall.Listen(fd, syscall.SOMAXCONN); err != nil {
		t.Fatalf("settle scratch Dispatch's port %d as its socket's alone: %v", port, err)
	}
	if err := syscall.Shutdown(fd, syscall.SHUT_RD); err != nil {
		t.Fatalf("stop listening on scratch Dispatch's port %d: %v", port, err)
	}
	return owner, port
}

// ownPortSocket is a close-on-exec TCP socket, closed when t ends if it is still open.
func ownPortSocket(t *testing.T, what string) *os.File {
	t.Helper()
	// Under ForkLock, so no child started between the two calls inherits the descriptor.
	syscall.ForkLock.RLock()
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM, 0)
	if err == nil {
		syscall.CloseOnExec(fd)
	}
	syscall.ForkLock.RUnlock()
	if err != nil {
		t.Fatalf("make %s: %v", what, err)
	}
	file := os.NewFile(uintptr(fd), what)
	t.Cleanup(func() { _ = file.Close() })
	return file
}

// stop interrupts the running server and waits for it to exit, killing it after ten seconds, then
// takes the port's socket out of listening, so every dial is refused until the next start. A
// server that is not running is left as it is.
func (d *scratchDispatch) stop() {
	if d.command == nil {
		return
	}
	command := d.command
	d.command = nil
	_ = command.Process.Signal(os.Interrupt)
	select {
	case err := <-d.exited:
		if err != nil {
			d.t.Errorf("scratch Dispatch stopped: %v\n%s", err, d.output.String())
		}
	case <-time.After(10 * time.Second):
		_ = command.Process.Kill()
		<-d.exited
		d.t.Error("scratch Dispatch did not stop after SIGINT")
	}
	// The socket is closed already when the test's own cleanup ran before this one. SO_REUSEADDR
	// goes before the socket stops listening, so the port is shareable only in the moment before a
	// start's listen, and a socket bound beside it then can never listen there: the port's own
	// socket either listens or does not set SO_REUSEADDR.
	fd := int(d.socket.Fd())
	if err := syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 0); err != nil && !errors.Is(err, syscall.EBADF) {
		d.t.Errorf("keep scratch Dispatch's port %d from being shared: %v", d.port, err)
	}
	if err := syscall.Shutdown(fd, syscall.SHUT_RD); err != nil && !errors.Is(err, syscall.EBADF) {
		d.t.Errorf("stop listening on scratch Dispatch's port %d: %v", d.port, err)
	}
}

// kill ends the running server with SIGKILL, as a node's loss ends a process mid-call: nothing of
// it runs on, and a request key it held stays in flight until its lease lapses.
func (d *scratchDispatch) kill() {
	d.t.Helper()
	command := d.command
	d.command = nil
	if err := command.Process.Kill(); err != nil {
		d.t.Fatalf("kill scratch Dispatch: %v", err)
	}
	<-d.exited
}

func createScratchDispatchIssue(t *testing.T, baseURL string) string {
	t.Helper()
	project := asScratchPerson(t, http.MethodPost, baseURL+"/api/v1/projects", map[string]string{"key": "LEGION", "name": "Scratch Legion"})
	project.Body.Close()
	if project.StatusCode != http.StatusCreated {
		t.Fatalf("create scratch project = %s", project.Status)
	}
	return createScratchDispatchIssueUnder(t, baseURL, "Crash-safe outbox message", "")
}

// createScratchDispatchIssueUnder files an issue titled title in the project
// createScratchDispatchIssue made, under the issue parent, or under none when parent is empty,
// and returns its key. Dispatch files it at triage. The create is forced past Dispatch's
// near-copy gate (409 POSSIBLE_DUPLICATE), which refuses a title that shares its words with an
// issue the project holds: a test that files a parent and a child for each of its cases names
// them alike on purpose.
func createScratchDispatchIssueUnder(t *testing.T, baseURL, title, parent string) string {
	t.Helper()
	body := map[string]any{"project": "LEGION", "title": title, "force": true}
	if parent != "" {
		body["parent"] = parent
	}
	issue := asScratchPerson(t, http.MethodPost, baseURL+"/api/v1/issues", body)
	defer issue.Body.Close()
	if issue.StatusCode != http.StatusCreated {
		t.Fatalf("create scratch issue = %s", issue.Status)
	}
	var created struct {
		Key string `json:"key"`
	}
	if err := json.NewDecoder(issue.Body).Decode(&created); err != nil {
		t.Fatalf("decode scratch issue: %v", err)
	}
	if created.Key == "" {
		t.Fatal("scratch Dispatch created an issue without a key")
	}
	return created.Key
}

// asScratchPerson sends body to url as the person smoke, whom the scratch server's trusted
// identity header names, and returns the answer for the caller to read and close.
func asScratchPerson(t *testing.T, method, url string, body any) *http.Response {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("encode %s %s: %v", method, url, err)
	}
	req, err := http.NewRequest(method, url, bytes.NewReader(encoded))
	if err != nil {
		t.Fatalf("create %s %s request: %v", method, url, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Dispatch-User", "smoke")
	response, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	return response
}

// servesStatusChange is whether the Dispatch at baseURL serves the status-change route, read from
// the list of every route it serves (GET /api/v1) rather than from the route's own answer, so a
// test of the client tells the two Dispatches apart without the client: one built before the
// route (INFRA-4731) lists none and answers it with its router's 404.
func servesStatusChange(t *testing.T, baseURL string) bool {
	t.Helper()
	response, err := pollClient.Get(baseURL + "/api/v1")
	if err != nil {
		t.Fatalf("read scratch Dispatch's routes: %v", err)
	}
	defer response.Body.Close()
	var index struct {
		Routes []struct {
			Method string `json:"method"`
			Path   string `json:"path"`
		} `json:"routes"`
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("read scratch Dispatch's routes = %s", response.Status)
	}
	if err := json.NewDecoder(response.Body).Decode(&index); err != nil {
		t.Fatalf("decode scratch Dispatch's routes: %v", err)
	}
	if len(index.Routes) == 0 {
		t.Fatal("scratch Dispatch lists no route at all, so its list cannot say whether it serves the status change")
	}
	for _, route := range index.Routes {
		if route.Method == http.MethodPost && route.Path == "/api/v1/issues/{key}/status-change" {
			return true
		}
	}
	return false
}
