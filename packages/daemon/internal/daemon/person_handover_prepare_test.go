package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/sjawhar/legion/daemon/internal/admit"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/ghbranch"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/testnats"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// eventLogDispatch is chainDispatch with each issue's Dispatch event log, as EventsAfter serves it,
// in logs; a key it holds no log of is served an empty log. A read of a key logErrs holds fails
// with that key's error. logReads counts each key's EventsAfter calls.
type eventLogDispatch struct {
	chainDispatch
	logs     map[string][]json.RawMessage
	logErrs  map[string]error
	logReads map[string]int
}

func (d *eventLogDispatch) EventsAfter(_ context.Context, key string, _ int64) ([]json.RawMessage, error) {
	if d.logReads == nil {
		d.logReads = map[string]int{}
	}
	d.logReads[key]++
	if err := d.logErrs[key]; err != nil {
		return nil, err
	}
	return d.logs[key], nil
}

// issueLogEvent is one issue.created or issue.updated event of key's Dispatch event log, as
// EventsAfter serves it and the durable stream carries it: at seq, written by an actor of kind,
// with the issue in triage carrying labels.
func issueLogEvent(key string, seq int, typ, kind string, labels ...string) json.RawMessage {
	quoted := make([]string, len(labels))
	for i, label := range labels {
		quoted[i] = fmt.Sprintf("%q", label)
	}
	return json.RawMessage(fmt.Sprintf(`{"id":%d,"issue_key":%q,"seq":%d,"type":%q,"actor":{"kind":%q,"id":"%s-1"},"notify":true,"payload":{"key":%q,"status":"triage","title":%q,"labels":[%s]}}`,
		seq, key, seq, typ, kind, kind, key, key, strings.Join(quoted, ",")))
}

// labelledBy is the event log of an issue a person created unlabelled and an actor of kind then
// labelled.
func labelledBy(key, kind string) []json.RawMessage {
	return []json.RawMessage{issueLogEvent(key, 1, "issue.created", "user"), issueLogEvent(key, 2, "issue.updated", kind, "legion")}
}

// An event of an unrecorded root in triage that carries the label is prepared with who set the
// label (intake.DispatchIssue.LabelSetter), read from the issue's whole Dispatch event log in one
// read: a person's label reads "user", a session's "session". Nothing else reads the log — an
// unlabelled triage root, a labelled triage child, a labelled todo root, and a root the daemon
// records that a person moved back to triage, which is never queued again — and a read that fails,
// or a log that does not decode, fails the preparation naming the issue, so the event is delivered
// again rather than applied with a setter nobody read.
func TestPrepareIssueReadsWhoSetTheLabelOfAnUnrecordedTriageRoot(t *testing.T) {
	t.Parallel()
	const key = "LEGION-755"
	triageRoot := intake.DispatchIssue{Key: key, Seq: 2, Type: "issue.updated", Status: "triage", Title: key, Rank: "A", HandedOver: true, ResolvedRepo: "acme/widgets"}
	pool := isolatedOutboxPool(t)
	for _, tc := range []struct {
		name     string
		event    func(issue intake.DispatchIssue) intake.DispatchIssue
		recorded bool
		logs     map[string][]json.RawMessage
		logErr   error
		reads    int
		want     string
		fails    string
	}{
		{name: "a person's label on an unrecorded triage root", event: func(i intake.DispatchIssue) intake.DispatchIssue { return i },
			logs: map[string][]json.RawMessage{key: labelledBy(key, "user")}, reads: 1, want: "user"},
		{name: "a session's label on an unrecorded triage root", event: func(i intake.DispatchIssue) intake.DispatchIssue { return i },
			logs: map[string][]json.RawMessage{key: labelledBy(key, "session")}, reads: 1, want: "session"},
		{name: "an unlabelled triage root", event: func(i intake.DispatchIssue) intake.DispatchIssue { i.HandedOver = false; return i },
			logs: map[string][]json.RawMessage{key: labelledBy(key, "user")}},
		{name: "a labelled triage child", event: func(i intake.DispatchIssue) intake.DispatchIssue { i.Parent = "LEGION-700"; return i },
			logs: map[string][]json.RawMessage{key: labelledBy(key, "user")}},
		{name: "a labelled todo root", event: func(i intake.DispatchIssue) intake.DispatchIssue { i.Status = "todo"; return i },
			logs: map[string][]json.RawMessage{key: labelledBy(key, "user")}},
		{name: "a recorded root a person moved back to triage", event: func(i intake.DispatchIssue) intake.DispatchIssue { return i }, recorded: true,
			logs: map[string][]json.RawMessage{key: labelledBy(key, "user")}},
		{name: "a failed read of the log", event: func(i intake.DispatchIssue) intake.DispatchIssue { return i },
			logErr: errors.New("Dispatch is restarting"), reads: 1, fails: "Dispatch is restarting"},
		{name: "a log that does not decode", event: func(i intake.DispatchIssue) intake.DispatchIssue { return i },
			logs: map[string][]json.RawMessage{key: {json.RawMessage(`{not json`)}}, reads: 1, fails: "decode Dispatch event 1 of 1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The preparation only reads, so the cases share one migrated database and each starts
			// from no recorded issue.
			if _, err := pool.Exec(context.Background(), "delete from issues"); err != nil {
				t.Fatalf("clear the recorded issues: %v", err)
			}
			records := record.NewStore()
			if tc.recorded {
				putOutboxIssue(t, pool, records, recordedOn(key, key, "triage"))
			}
			// acme/widgets is served at main, so the labelled todo root is prepared for admission
			// as today, from the read the repository state holds.
			repos := newRepositories("LEGION", ghrepo.Renames{})
			repos.fill(ghrepo.MustParse("acme/widgets"))
			branches := newDefaultBranches(roleTokens{}, repos, "", defaultBranchTTL, time.Now)
			branches.read["acme/widgets"] = defaultBranchRead{repository: ghbranch.Repository{Name: ghrepo.MustParse("acme/widgets"), DefaultBranch: "main"}, at: time.Now()}
			client := &eventLogDispatch{logs: tc.logs, logErrs: map[string]error{key: tc.logErr}}
			client.issues = map[string]dispatch.Issue{key: shownUnder(key, "todo", "", true)}
			w := &workflowRuntime{pool: pool, records: records, dispatch: client, repositories: repos, defaultBranches: branches, log: quietLogger()}

			got, err := w.prepareRepository(context.Background(), tc.event(triageRoot))
			if tc.fails != "" {
				if err == nil || !strings.Contains(err.Error(), tc.fails) || !strings.Contains(err.Error(), key) {
					t.Fatalf("prepareRepository = %#v, %v; want the failed read to fail the preparation, naming %s and %q", got, err, key, tc.fails)
				}
				return
			}
			if err != nil {
				t.Fatalf("prepareRepository: %v", err)
			}
			if got.LabelSetter != tc.want {
				t.Fatalf("LabelSetter = %q, want %q", got.LabelSetter, tc.want)
			}
			if client.logReads[key] != tc.reads {
				t.Fatalf("the event log of %s was read %d times, want %d", key, client.logReads[key], tc.reads)
			}
		})
	}
}

// A review event is prepared with Dispatch's view of the issue it is on
// (intake.DispatchArtifact.Issue) only for an approval a person made that is not Dispatch's carry
// of an earlier one, in one read of the issue: a session's approval, a carried approval, a changes
// request and a version event read nothing and carry no view, and a read that fails fails the
// preparation, so the event is delivered again. The issue's Dispatch event log is read, once, only
// for such an approval that carries a sequence and that the view shows can queue its root
// (intake.DispatchArtifact.MayQueueHandover: here a parked labelled root whose primary document is
// the one approved): a person's approval of a root Dispatch shows in todo, and one with no
// sequence, read the issue alone, and a failed read of the log fails the preparation, naming the
// issue. The log served here is empty, so it leaves StatusChangedSeq at 0, and every prepared fact
// is the decoded one apart from its view.
func TestPrepareArtifactReadsTheIssueOnlyForAPersonsOwnApproval(t *testing.T) {
	t.Parallel()
	const key = "LEGION-755"
	person := dispatch.Actor{Kind: "user", ID: "sami"}
	approval := intake.DispatchArtifact{Key: key, ArtifactID: "art-1", Kind: intake.DispatchArtifactApproved, Version: 9, Actor: person,
		Mark: record.EventMark{Seq: 9}}
	shown := dispatch.Issue{Key: key, Status: "backlog", Title: key, Rank: "A", HandedOver: true, PrimaryArtifactID: "art-1", LastSeq: 40}
	for _, tc := range []struct {
		name string
		fact func(fact intake.DispatchArtifact) intake.DispatchArtifact
		// status is the status Dispatch shows the issue at, shown's when empty.
		status   string
		failing  bool
		logErr   error
		reads    int
		logReads int
		fails    bool
	}{
		{name: "a person's approval", fact: func(a intake.DispatchArtifact) intake.DispatchArtifact { return a }, reads: 1, logReads: 1},
		{name: "a person's approval with a reason of their own", fact: func(a intake.DispatchArtifact) intake.DispatchArtifact { a.Reason = "Ship it."; return a },
			reads: 1, logReads: 1},
		{name: "a session's approval", fact: func(a intake.DispatchArtifact) intake.DispatchArtifact {
			a.Actor = dispatch.Actor{Kind: "session", ID: "architect-1"}
			return a
		}},
		{name: "a carried approval", fact: func(a intake.DispatchArtifact) intake.DispatchArtifact {
			a.Reason = intake.CarriedApprovalReasonPrefix + " 2: the answer to the open ask was the whole change"
			return a
		}},
		{name: "a person's changes request", fact: func(a intake.DispatchArtifact) intake.DispatchArtifact {
			a.Kind, a.Reason = intake.DispatchArtifactChangesRequested, "Name the limit."
			return a
		}},
		{name: "a version", fact: func(a intake.DispatchArtifact) intake.DispatchArtifact {
			a.Kind, a.Actor = intake.DispatchArtifactVersion, dispatch.Actor{}
			return a
		}},
		{name: "a failed read of the issue", fact: func(a intake.DispatchArtifact) intake.DispatchArtifact { return a }, failing: true, reads: 1, fails: true},
		{name: "a person's approval of a root Dispatch shows in todo", fact: func(a intake.DispatchArtifact) intake.DispatchArtifact { return a }, status: "todo",
			reads: 1},
		{name: "a person's approval with sequence 0", fact: func(a intake.DispatchArtifact) intake.DispatchArtifact { a.Mark = record.EventMark{}; return a },
			reads: 1},
		{name: "a failed read of the log", fact: func(a intake.DispatchArtifact) intake.DispatchArtifact { return a }, logErr: errors.New("Dispatch is restarting"),
			reads: 1, logReads: 1, fails: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			view := shown
			if tc.status != "" {
				view.Status = tc.status
			}
			client := &eventLogDispatch{logErrs: map[string]error{key: tc.logErr}}
			client.issues = map[string]dispatch.Issue{key: view}
			if tc.failing {
				client.failing = key
			}
			w := &workflowRuntime{dispatch: client, log: quietLogger()}
			fact := tc.fact(approval)

			got, err := w.prepareArtifact(context.Background(), fact)
			switch {
			case tc.fails && (err == nil || !strings.Contains(err.Error(), key)):
				t.Fatalf("prepareArtifact = %#v, %v; want the failed read to fail the preparation, naming %s", got, err, key)
			case !tc.fails && err != nil:
				t.Fatalf("prepareArtifact: %v", err)
			}
			if client.reads[key] != tc.reads {
				t.Fatalf("%s was read %d times, want %d", key, client.reads[key], tc.reads)
			}
			if client.logReads[key] != tc.logReads {
				t.Fatalf("the event log of %s was read %d times, want %d", key, client.logReads[key], tc.logReads)
			}
			if tc.fails {
				return
			}
			if tc.reads == 0 {
				if got.Issue != nil {
					t.Fatalf("Issue = %#v, want no view for a review that queues nothing", *got.Issue)
				}
				got.Issue = nil
				if got != fact {
					t.Fatalf("prepareArtifact = %#v, want the fact unchanged, %#v", got, fact)
				}
				return
			}
			if got.Issue == nil || got.Issue.Key != key || got.Issue.Status != view.Status || got.Issue.PrimaryArtifactID != view.PrimaryArtifactID || !got.Issue.HandedOver {
				t.Fatalf("Issue = %#v, want Dispatch's view of %s", got.Issue, key)
			}
			got.Issue = nil
			if got != fact {
				t.Fatalf("prepareArtifact = %#v, want the fact as decoded apart from its view, %#v", got, fact)
			}
		})
	}
}

// The boot listing reads who set the label of a labelled triage root the daemon does not record,
// in one read of its event log, as an event's preparation does, since the listing carries no actor.
// It reads nothing for a labelled triage root it records (one a person moved back to triage, which
// is never queued again), an unlabelled triage root, or a labelled triage child; and a read that
// fails fails the reconcile naming the issue.
func TestBootReconcileReadsWhoSetTheLabelOfAnUnrecordedTriageRoot(t *testing.T) {
	t.Parallel()
	const unrecorded, recorded, unlabelled, child = "CAPTURE-755", "CAPTURE-589", "CAPTURE-526", "CAPTURE-756"
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	conn := testnats.Connect(t, workflowNATS(t))
	js, err := jetstream.New(conn)
	if err != nil {
		t.Fatalf("open JetStream: %v", err)
	}
	log := quietLogger()
	consumers, err := intake.OpenConsumers(ctx, js, pool, intake.ConsumerSpec{
		Project: "CAPTURE", Repositories: []ghrepo.Repository{ghrepo.MustParse("acme/widgets")}, AckWait: time.Second, NakDelay: time.Millisecond, Logger: log,
	})
	if err != nil {
		t.Fatalf("OpenConsumers: %v", err)
	}
	records := record.NewStore()
	back := recordedOn(recorded, recorded, "triage")
	back.Project, back.LastDispatchSeq = "CAPTURE", 5
	putOutboxIssue(t, pool, records, back)
	under := func(key string) *string { return &key }
	client := &eventLogDispatch{logs: map[string][]json.RawMessage{
		unrecorded: labelledBy(unrecorded, "user"),
		recorded:   labelledBy(recorded, "user"),
		unlabelled: labelledBy(unlabelled, "user"),
		child:      labelledBy(child, "user"),
	}}
	client.listed = []dispatch.IssueSummary{
		{Key: unrecorded, ResolvedRepo: "acme/widgets", Title: unrecorded, Status: "triage", Rank: "A", HandedOver: true, LastSeq: 2},
		{Key: recorded, ResolvedRepo: "acme/widgets", Title: recorded, Status: "triage", Rank: "B", HandedOver: true, LastSeq: 5},
		{Key: unlabelled, ResolvedRepo: "acme/widgets", Title: unlabelled, Status: "triage", Rank: "C", LastSeq: 2},
		{Key: child, ResolvedRepo: "acme/widgets", Title: child, Status: "triage", Rank: "D", Parent: under(unrecorded), HandedOver: true, LastSeq: 2},
	}
	engine := workflow.New(records, workflow.Config{Project: "CAPTURE", Repositories: testRepositories, Steps: config.DesignGateRootIssues.Steps()}, log)
	admission := admit.New(records, engine, 2, "CAPTURE", log)
	w := &workflowRuntime{
		pool: pool, records: records, admission: admission, engine: engine, handlers: []intake.Handler{engine, admission}, repositories: newRepositories("CAPTURE", ghrepo.Renames{}),
		dispatch: client, dispatchProject: "CAPTURE", bootID: "handover-boot", log: log, consumers: consumers,
	}

	if err := w.reconcile(ctx); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(client.logReads) != 1 || client.logReads[unrecorded] != 1 {
		t.Fatalf("the boot read the event logs %v, want one read of %s alone", client.logReads, unrecorded)
	}
	if len(client.reads) != 0 {
		t.Fatalf("the boot read %v from Dispatch, want no issue detail: its listing shows every row it prepares", client.reads)
	}

	client.logErrs = map[string]error{unrecorded: errors.New("Dispatch is restarting")}
	err = w.reconcile(ctx)
	if err == nil || !strings.Contains(err.Error(), unrecorded) || !strings.Contains(err.Error(), "Dispatch is restarting") {
		t.Fatalf("reconcile with the log unreadable = %v; want the failed read to fail the reconcile, naming %s", err, unrecorded)
	}
}

// The listing tests below run over one board on acme/widgets, which the daemon has read already,
// so a listing records a todo root on it without reading GitHub: labelledRoot, a triage root a
// person labelled that the daemon does not record; leftOutRoot, another such root, whose Dispatch
// event log each test serves its own way; and listedTodo, a labelled todo root.
const labelledRoot, leftOutRoot, listedTodo = "CAPTURE-755", "CAPTURE-760", "CAPTURE-761"

// leftOutLine is the ERROR line of a listing that leaves an issue out of admission's decision.
const leftOutLine = "workflow: the listing leaves out of admission an issue whose Dispatch event log cannot be read"

// leftOutBoard is a daemon over that board. Its Dispatch serves leftOutRoot's event log as
// leftOutLog, or fails the read with readErr where readErr is set, and the daemon logs JSON to the
// buffer it returns.
func leftOutBoard(t *testing.T, leftOutLog []json.RawMessage, readErr error) (*workflowRuntime, *syncBuffer) {
	t.Helper()
	pool := isolatedOutboxPool(t)
	consumers := bootClaimConsumers(t, pool)
	logs := &syncBuffer{}
	log := slog.New(slog.NewJSONHandler(logs, nil))
	records := record.NewStore()
	client := &eventLogDispatch{logs: map[string][]json.RawMessage{
		labelledRoot: labelledBy(labelledRoot, "user"),
		leftOutRoot:  leftOutLog,
	}, logErrs: map[string]error{leftOutRoot: readErr}}
	client.listed = []dispatch.IssueSummary{
		{Key: labelledRoot, ResolvedRepo: "acme/widgets", Title: labelledRoot, Status: "triage", Rank: "A", HandedOver: true, LastSeq: 2},
		{Key: leftOutRoot, ResolvedRepo: "acme/widgets", Title: leftOutRoot, Status: "triage", Rank: "B", HandedOver: true, LastSeq: 9},
		{Key: listedTodo, ResolvedRepo: "acme/widgets", Title: listedTodo, Status: "todo", Rank: "C", HandedOver: true, LastSeq: 1},
	}
	// A recovery's listing reads again the default branch of a repository an entry records issues
	// on (prepareUnrecorded), and finds main read already.
	repos := newRepositories("CAPTURE", ghrepo.Renames{})
	repos.fill(ghrepo.MustParse("acme/widgets"))
	branches := newDefaultBranches(roleTokens{}, repos, "", defaultBranchTTL, time.Now)
	branches.read["acme/widgets"] = defaultBranchRead{repository: ghbranch.Repository{Name: ghrepo.MustParse("acme/widgets"), DefaultBranch: "main"}, at: time.Now()}
	engine := workflow.New(records, workflow.Config{Project: "CAPTURE", Repositories: testRepositories, Steps: config.DesignGateRootIssues.Steps()}, log)
	admission := admit.New(records, engine, 2, "CAPTURE", log)
	return &workflowRuntime{
		pool: pool, records: records, admission: admission, engine: engine, handlers: []intake.Handler{engine, admission},
		repositories: repos, defaultBranches: branches, dispatch: client, dispatchProject: "CAPTURE", bootID: "handover-boot",
		log: log, consumers: consumers,
	}, logs
}

// appliedWithout checks a listing that left leftOutRoot out of admission's decision and decided
// the rest as ever: labelledRoot is queued as the person's handover and listedTodo is recorded.
// leftOutRoot has no record and no outbox row, and its row in the copy of Dispatch's issues holds
// its listed status, triage. It returns the one line of logs that left leftOutRoot out, an ERROR
// line naming leftOutRoot.
func appliedWithout(t *testing.T, w *workflowRuntime, logs *syncBuffer) map[string]any {
	t.Helper()
	var payload []byte
	if err := w.pool.QueryRow(context.Background(), "select payload from outbox where issue = $1 and kind = $2",
		labelledRoot, string(record.OutboxKindDispatchStatus)).Scan(&payload); err != nil {
		t.Fatalf("read the status row of %s: %v; want the person's handover queued", labelledRoot, err)
	}
	queued, err := record.DecodeOutboxPayload(record.OutboxRow{Kind: record.OutboxKindDispatchStatus, Issue: labelledRoot, Payload: payload})
	if err != nil {
		t.Fatalf("decode the status row of %s (%s): %v", labelledRoot, payload, err)
	}
	if want := record.PersonHandoverStatusWrite("todo", "triage"); queued != want {
		t.Fatalf("the status row of %s = %#v, want the person's handover %#v", labelledRoot, queued, want)
	}
	if got := readIssue(t, w.pool, listedTodo); got == nil {
		t.Fatalf("%s has no record after the listing, want the labelled todo root recorded", listedTodo)
	}
	if got := readIssue(t, w.pool, leftOutRoot); got != nil {
		t.Fatalf("%s = %#v after the listing, want no record: admission decides nothing on an issue whose log it cannot read", leftOutRoot, got)
	}
	if rows := outboxRowsOf(t, w.pool, leftOutRoot); rows != 0 {
		t.Fatalf("%s has %d outbox rows after the listing, want none", leftOutRoot, rows)
	}
	if got := copied(t, w.pool, leftOutRoot).status; got != "triage" {
		t.Fatalf("the copy of %s = %q; want its listed status, triage: the listing still copies an issue it leaves out of admission", leftOutRoot, got)
	}
	lines := loggedMessages(t, logs.String(), leftOutLine)
	if len(lines) != 1 {
		t.Fatalf("the listing logged %d lines %q, want one:\n%s", len(lines), leftOutLine, logs.String())
	}
	if line := lines[0]; line["level"] != "ERROR" || line["issue"] != leftOutRoot {
		t.Fatalf("the line leaving %s out = %v, want ERROR naming %s", leftOutRoot, line, leftOutRoot)
	}
	return lines[0]
}

// appliedNothing checks a listing that failed: it recorded nothing, queued nothing, and logged no
// line that left an issue out.
func appliedNothing(t *testing.T, w *workflowRuntime, logs *syncBuffer) {
	t.Helper()
	for _, key := range []string{labelledRoot, leftOutRoot, listedTodo} {
		if got := readIssue(t, w.pool, key); got != nil {
			t.Fatalf("%s = %#v after the failed listing, want no record", key, got)
		}
		if rows := outboxRowsOf(t, w.pool, key); rows != 0 {
			t.Fatalf("%s has %d outbox rows after the failed listing, want none", key, rows)
		}
	}
	if lines := loggedMessages(t, logs.String(), leftOutLine); len(lines) != 0 {
		t.Fatalf("the failed listing logged %v, want no issue left out", lines)
	}
}

// unreadableLog is one way the daemon cannot read leftOutRoot's Dispatch event log: the log
// Dispatch serves, or the error the read fails with (readErr). The line that leaves the root out
// names the event's place in the log (event), the event's Dispatch sequence (seq, 0 for an event
// that does not decode) and an error that says cause.
type unreadableLog struct {
	name    string
	log     []json.RawMessage
	readErr error
	event   int
	seq     int64
	cause   string
}

// nonAscending is the error EventsAfter gives for leftOutRoot's log when its third event repeats
// sequence 9.
const nonAscending = "page issue events for " + leftOutRoot + ": non-ascending sequence 9 after 9"

// unreadableLogs are the logs and the log pages the daemon cannot read. In the first two rows the
// daemon reads the log and cannot decode its third and last event: an event that is a JSON array
// and not a Dispatch event, or a person's edit at Dispatch sequence 9 whose payload is `null`. In
// the third row the Dispatch client cannot read a page of the log, because its third event repeats
// sequence 9: the read fails with the *dispatch.EventLogError that EventsAfter makes for a seq
// that does not ascend.
var unreadableLogs = []unreadableLog{
	{name: "an event that does not decode as a Dispatch event", log: append(labelledBy(leftOutRoot, "user"), json.RawMessage(`[9]`)),
		event: 3, seq: 0, cause: "decode Dispatch event 3 of 3: json: cannot unmarshal array"},
	{name: "a payload that is no JSON object", log: append(labelledBy(leftOutRoot, "user"), json.RawMessage(fmt.Sprintf(
		`{"id":3,"issue_key":%q,"seq":9,"type":"issue.updated","actor":{"kind":"user","id":"user-1"},"notify":true,"payload":null}`, leftOutRoot))),
		event: 3, seq: 9, cause: "decode Dispatch issue payload of event 3 of 3: the payload is not a JSON object"},
	{name: "a page whose seq does not ascend", readErr: &dispatch.EventLogError{Event: 3, Seq: 9, Err: errors.New(nonAscending)},
		event: 3, seq: 9, cause: nonAscending},
}

// namesTheEvent checks the line that left leftOutRoot out against tc.
func namesTheEvent(t *testing.T, line map[string]any, tc unreadableLog) {
	t.Helper()
	if line["event"] != float64(tc.event) || line["seq"] != float64(tc.seq) || !strings.Contains(fmt.Sprint(line["error"]), tc.cause) {
		t.Fatalf("the line leaving %s out = %v, want event %d (its place in the log), seq %d (its Dispatch sequence) and the error %q",
			leftOutRoot, line, tc.event, tc.seq, tc.cause)
	}
}

// A labelled triage root the daemon does not record, whose Dispatch event log or log page the
// daemon cannot read (unreadableLogs), is left out of the boot listing's decision. The daemon logs
// one ERROR line that names the issue, the event's place in the log and the event's sequence, and
// the boot goes on: the rest of the listing is decided as ever. So another such root a person
// labelled is queued as that person's handover, and an unrecorded labelled todo root is recorded.
// The root left out has no record and no outbox row, and its row in the copy of Dispatch's issues
// is still written.
func TestBootReconcileLeavesOutATriageRootWhoseEventLogCannotBeRead(t *testing.T) {
	for _, tc := range unreadableLogs {
		t.Run(tc.name, func(t *testing.T) {
			w, logs := leftOutBoard(t, tc.log, tc.readErr)
			if err := w.reconcile(context.Background()); err != nil {
				t.Fatalf("reconcile with one unreadable event log = %v, want the boot to go on without that issue", err)
			}
			namesTheEvent(t, appliedWithout(t, w, logs), tc)
		})
	}
}

// refusedReads are refusals of the read of leftOutRoot's event log. waits is whether
// dispatch.Unreachable calls the refusal an outage, which the boot waits out. Three refusals do
// not clear: the 400 INVALID_QUERY of a query the route does not take, the 404 "no route for …" of
// a Dispatch without the route, and Dispatch's coded 404 for an issue it does not know, which the
// events route gives for no issue today (it answers 200 for a key no issue holds). The others are
// outages: a credential answer (401, 403), a 5xx, and no answer.
var refusedReads = []struct {
	name    string
	readErr error
	waits   bool
}{
	{name: "400 INVALID_QUERY", readErr: &dispatch.Error{Status: http.StatusBadRequest, Code: "INVALID_QUERY",
		Message: "after cannot be combined with before or order=desc"}},
	{name: "404 no route", readErr: &dispatch.Error{Status: http.StatusNotFound, Code: "NOT_FOUND",
		Message: "no route for GET /api/v1/issues/" + leftOutRoot + "/events"}},
	{name: "404 issue not found", readErr: &dispatch.Error{Status: http.StatusNotFound, Code: dispatch.IssueNotFound,
		Message: "no issue " + leftOutRoot}},
	{name: "401 UNAUTHORIZED", readErr: &dispatch.Error{Status: http.StatusUnauthorized, Code: "UNAUTHORIZED",
		Message: "invalid bearer token"}, waits: true},
	{name: "403 FORBIDDEN", readErr: &dispatch.Error{Status: http.StatusForbidden, Code: "FORBIDDEN",
		Message: "token revoked"}, waits: true},
	{name: "503 DATABASE_UNAVAILABLE", readErr: &dispatch.Error{Status: http.StatusServiceUnavailable, Code: "DATABASE_UNAVAILABLE",
		Message: "database unavailable"}, waits: true},
	{name: "no answer", readErr: &dispatch.TransientError{Err: errors.New("dial tcp 10.0.0.7:443: connect: connection refused")},
		waits: true},
}

// A refused read of the Dispatch event log of a labelled triage root the daemon does not record
// (refusedReads) fails the boot's listing, with an error that names the issue. No root is left
// out, and the listing records nothing and queues nothing. The boot waits and reads the listing
// again on an error that dispatch.Unreachable calls an outage (waits), and it stops on the others
// (readinessAttempt). A refusal of this read says nothing about one issue, so to leave each root
// out would pass over every labelled triage root and show no failure.
func TestBootReconcileFailsTheListingOnARefusedEventLogRead(t *testing.T) {
	for _, tc := range refusedReads {
		t.Run(tc.name, func(t *testing.T) {
			w, logs := leftOutBoard(t, labelledBy(leftOutRoot, "user"), tc.readErr)
			err := w.reconcile(context.Background())
			if err == nil || !strings.Contains(err.Error(), leftOutRoot) {
				t.Fatalf("reconcile with the read of %s's log answered %v = %v; want the listing to fail, naming %s", leftOutRoot, tc.readErr, err, leftOutRoot)
			}
			if waits := dispatch.Unreachable(err); waits != tc.waits {
				t.Fatalf("dispatch.Unreachable(%v) = %t, want %t", err, waits, tc.waits)
			}
			appliedNothing(t, w, logs)
		})
	}
}

// A repository recovery's listing (recoverRepositories) reads a root's Dispatch event log as the
// boot's listing does. For a log or a log page the daemon cannot read (unreadableLogs), the pass
// leaves the root out with the one ERROR line, applies the rest of its listing and recovers the
// repository, with no warning that its issues were not recovered. For a refused read
// (refusedReads), the pass logs that warning naming the root, queues nothing and keeps the
// repository to recover, so a later pass reads the listing again.
func TestARepositoryRecoveryLeavesOutAnUnreadableEventLogAndFailsOnARefusal(t *testing.T) {
	const notRecovered = "workflow: the issues of the repositories to recover were not recovered from Dispatch's listing"
	// pass runs one recovery pass over the board, with leftOutRoot's log served as log or its read
	// failed with readErr. It returns the daemon, its logs and the pass's warnings that the issues
	// were not recovered.
	pass := func(t *testing.T, log []json.RawMessage, readErr error) (*workflowRuntime, *syncBuffer, []map[string]any) {
		t.Helper()
		w, logs := leftOutBoard(t, log, readErr)
		// GitHub answered for acme/widgets again (readAnswered), so the pass lists the issues on it.
		w.recovering = map[string]bool{"acme/widgets": true}
		w.recoverRepositories(context.Background())
		return w, logs, loggedMessages(t, logs.String(), notRecovered)
	}
	for _, tc := range unreadableLogs {
		t.Run(tc.name, func(t *testing.T) {
			w, logs, warnings := pass(t, tc.log, tc.readErr)
			if len(warnings) != 0 {
				t.Fatalf("the pass logged %v, want no warning %q: it leaves %s out and recovers the rest", warnings, notRecovered, leftOutRoot)
			}
			if w.recovering["acme/widgets"] {
				t.Fatalf("acme/widgets is still to recover after the pass, want it recovered")
			}
			namesTheEvent(t, appliedWithout(t, w, logs), tc)
		})
	}
	for _, tc := range refusedReads {
		t.Run(tc.name, func(t *testing.T) {
			w, logs, warnings := pass(t, labelledBy(leftOutRoot, "user"), tc.readErr)
			if len(warnings) != 1 || !strings.Contains(fmt.Sprint(warnings[0]["error"]), leftOutRoot) {
				t.Fatalf("the pass logged %v, want one warning %q naming %s:\n%s", warnings, notRecovered, leftOutRoot, logs.String())
			}
			if !w.recovering["acme/widgets"] {
				t.Fatalf("acme/widgets is no longer to recover after a pass whose listing failed, want it kept for the next pass")
			}
			appliedNothing(t, w, logs)
		})
	}
}
