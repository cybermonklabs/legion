package reopenreport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// The fixtures' documents: the issue's close-wait.md and its Verification document.
const (
	closeWaitID    = "doc-close-wait"
	verificationID = "doc-verification"
)

// The fixtures' change: the controller's request, the generation it started, and the pull request
// that merged it.
const (
	requestID   int64  = 17
	changeGen   uint64 = 2
	pullRequest        = 51
)

// requested is the moment of the controller's accepted todo in every fixture.
var requested = time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)

var (
	owner      = dispatch.Actor{Kind: "session", ID: "ses-owner"}
	controller = dispatch.Actor{Kind: "session", ID: "ses-controller"}
	person     = dispatch.Actor{Kind: "user", ID: "operator@example.com"}
)

// daemonOf is the actor the daemon of key's project writes Dispatch as.
func daemonOf(key string) dispatch.Actor {
	return dispatch.Actor{Kind: "session", ID: dispatch.DaemonSession(key)}
}

// fakeDispatch is Dispatch as the report reads one issue of it: the issue's two documents, its
// event log, the markdown of each version of close-wait.md, what each version of the Verification
// document changed from the one before it, and the blocks of that document's first version.
// eventsErr is a page of the log that could not be read, which fails the whole read of the log, as
// dispatch.HTTPClient.EventsAfter fails it. todo is the sequence of the accepted todo the schedule
// wrote, which the row's interval starts at.
type fakeDispatch struct {
	events    []json.RawMessage
	eventsErr error
	closeWait map[int]string
	changes   map[int]dispatch.ArtifactChanges
	first     []dispatch.DocumentBlock
	todo      int64
}

func (f *fakeDispatch) Artifacts(context.Context, string) ([]dispatch.Artifact, error) {
	return []dispatch.Artifact{
		{ID: verificationID, Slug: dispatch.VerificationSlug, Kind: "doc", Role: dispatch.ArtifactRoleVerification},
		{ID: closeWaitID, Slug: record.CloseWaitDocumentSlug, Kind: "doc"},
	}, nil
}

func (f *fakeDispatch) EventsAfter(_ context.Context, key string, after int64) ([]json.RawMessage, error) {
	if f.eventsErr != nil {
		return nil, f.eventsErr
	}
	if after != 0 {
		return nil, fmt.Errorf("read the events of %s after %d: the report reads the whole log", key, after)
	}
	return f.events, nil
}

func (f *fakeDispatch) DocumentVersion(_ context.Context, id string, version int) (string, error) {
	markdown, ok := f.closeWait[version]
	if id != closeWaitID || !ok {
		return "", &dispatch.Error{Status: 404, Code: "NOT_FOUND", Message: fmt.Sprintf("no version %d of %s", version, id)}
	}
	return markdown, nil
}

func (f *fakeDispatch) ArtifactChanges(_ context.Context, id string, from, to int) (dispatch.ArtifactChanges, error) {
	changes, ok := f.changes[to]
	if id != verificationID || from != to-1 || !ok {
		return dispatch.ArtifactChanges{}, fmt.Errorf("no changes of %s from %d to %d", id, from, to)
	}
	return changes, nil
}

func (f *fakeDispatch) PlanDocument(_ context.Context, id string, version int) (dispatch.DocumentVersion, error) {
	if id != verificationID || version != 1 || f.first == nil {
		return dispatch.DocumentVersion{}, fmt.Errorf("no blocks of %s at version %d", id, version)
	}
	return dispatch.DocumentVersion{ArtifactID: id, Version: 1, Blocks: f.first}, nil
}

// schedule writes one issue's Dispatch log in order, each event at the next sequence, and the
// versions of its two documents beside it.
type schedule struct {
	t     *testing.T
	key   string
	seq   int64
	waits int
	fake  *fakeDispatch
}

// event appends one event of typ by actor at at, carrying payload, as Dispatch's log serves it.
func (s *schedule) event(typ string, actor dispatch.Actor, at time.Time, payload map[string]any) int64 {
	s.t.Helper()
	s.seq++
	encoded, err := json.Marshal(map[string]any{
		"id": 1000 + s.seq, "issue_key": s.key, "artifact_id": nil, "project": "ACME", "seq": s.seq, "type": typ,
		"actor": map[string]string{"kind": actor.Kind, "id": actor.ID}, "notify": false,
		"created_at": at.Format(time.RFC3339Nano), "payload": payload,
	})
	if err != nil {
		s.t.Fatal(err)
	}
	s.fake.events = append(s.fake.events, encoded)
	return s.seq
}

// status appends an issue event that leaves the issue at status.
func (s *schedule) status(typ string, actor dispatch.Actor, at time.Time, status string) int64 {
	return s.event(typ, actor, at, map[string]any{"key": s.key, "status": status, "changed": []string{"status"}})
}

// wait appends the daemon's next version of close-wait.md, which renders stored as the daemon
// renders it (record.CloseWaitDocument): its first version is the document's creation.
func (s *schedule) wait(at time.Time, stored record.StoredWait) {
	s.t.Helper()
	encoded, err := json.Marshal(stored)
	if err != nil {
		s.t.Fatal(err)
	}
	since := requested.Add(-72 * time.Hour)
	markdown, err := record.CloseWaitDocument(record.Issue{Key: s.key, CloseWait: encoded, CloseWaitSince: &since})
	if err != nil {
		s.t.Fatal(err)
	}
	s.waits++
	s.fake.closeWait[s.waits] = markdown
	s.versionEvent(daemonOf(s.key), at, closeWaitID, s.waits)
}

// versionEvent appends the event of version number of document by actor: artifact.created for
// the first, artifact.version for each later one.
func (s *schedule) versionEvent(actor dispatch.Actor, at time.Time, document string, number int) {
	if number == 1 {
		s.event("artifact.created", actor, at, map[string]any{"artifact": map[string]any{
			"id": document, "versions": []map[string]any{{"number": 1}}}})
		return
	}
	s.event("artifact.version", actor, at, map[string]any{"artifact_id": document, "version": map[string]any{"number": number}})
}

// verification appends version number of the Verification document by actor, which changed what
// changes says from the version before it.
func (s *schedule) verification(actor dispatch.Actor, at time.Time, number int, changes dispatch.ArtifactChanges) {
	changes.ArtifactID, changes.From, changes.To = verificationID, number-1, number
	s.fake.changes[number] = changes
	s.versionEvent(actor, at, verificationID, number)
}

// checkAdded is a version's change that adds the check block id.
func checkAdded(id string) dispatch.ArtifactChanges {
	return dispatch.ArtifactChanges{Blocks: []dispatch.BlockChange{{ID: id, Type: "check", Change: dispatch.ChangeAdded}}}
}

// unrelated is a version's change of a section's prose and of a requirement block, and of no check.
func unrelated() dispatch.ArtifactChanges {
	return dispatch.ArtifactChanges{
		Sections: []dispatch.SectionChange{{Heading: "Notes", Change: dispatch.ChangeChanged}},
		Blocks:   []dispatch.BlockChange{{ID: "r-widgets", Type: "requirement", Change: dispatch.ChangeChanged}},
	}
}

// openWait is the stored wait of change changeGen while it is open, carrying the controller's
// request.
func openWait(since time.Time) record.StoredWait {
	return record.StoredWait{Entered: requested.Add(-72 * time.Hour), NextChange: &record.WaitChange{
		Generation: changeGen, Since: since, Reasons: []record.WaitReason{{ID: requestID, Kind: record.DefectKindRequest,
			Text: "Reopened after an early close: the widget export was dropped.", Role: claim.RoleController, Since: since}},
	}}
}

// reopened is an issue whose first work merged and closed, then the controller's request: its
// comment, and its todo, which the daemon wrote through set_status. In the same admission pass
// the change opens (close-wait.md's version naming it) and its first phase starts (the daemon's
// in_progress), which is the schedule of D17's first case. Before the request, the owner's own
// version 3 of the Verification document added the issue's first checks.
func reopened(t *testing.T, key string) *schedule {
	t.Helper()
	s := &schedule{t: t, key: key, fake: &fakeDispatch{closeWait: map[int]string{}, changes: map[int]dispatch.ArtifactChanges{}}}
	s.status("issue.created", person, requested.Add(-96*time.Hour), "todo")
	s.status("issue.updated", daemonOf(key), requested.Add(-95*time.Hour), "in_progress")
	s.verification(owner, requested.Add(-90*time.Hour), 3, checkAdded("c-widgets"))
	closed := requested.Add(-72 * time.Hour)
	s.wait(closed, record.StoredWait{Entered: closed, Closed: &closed})
	s.status("issue.closed", daemonOf(key), closed.Add(time.Second), "done")
	s.event("message.created", controller, requested.Add(-time.Minute), map[string]any{"body": "Reopened after an early close: the widget export was dropped."})
	s.fake.todo = s.status("issue.updated", daemonOf(key), requested, "todo")
	s.wait(requested.Add(2*time.Second), openWait(requested.Add(time.Second)))
	s.status("issue.updated", daemonOf(key), requested.Add(3*time.Second), "in_progress")
	return s
}

// merged appends the daemon's version of close-wait.md that says the change merged at at.
func (s *schedule) merged(at time.Time) {
	s.wait(at.Add(5*time.Second), record.StoredWait{Entered: at, Ended: &record.WaitChangeEnd{Generation: changeGen, PullRequest: pullRequest, At: at}})
}

// The five cases of D17's measurement, each classified by what the program alone can tell, with
// the interval from the accepted todo to the change's end: its merge, or its end with no pull
// request, as close-wait.md says it. Beside them: a Verification document first written in the
// interval, and three ways a row cannot be read that only the report names, the change's end that
// no version says, a version in the same second as the end, and a section the renderer does not
// write. Every version of close-wait.md is rendered by record.CloseWaitDocument, and the types it
// renders from hold the fields the fixtures set, so a change of the renderer fails here.
func TestReopenInstructionCountUsesTheChangeEnd(t *testing.T) {
	t.Parallel()
	mergedAt := requested.Add(3 * time.Hour)
	withdrawnAt := requested.Add(2 * time.Hour)
	const evidence = "The work is done: ACME-90 merged it in pull request #60."
	type want struct {
		category   Category
		start      bool
		end        *End
		versions   []int
		checks     map[int][]dispatch.BlockChange
		withdrawal *Withdrawal
	}
	added := map[int][]dispatch.BlockChange{4: checkAdded("c-widget-export").Blocks}
	cases := []struct {
		name  string
		build func(t *testing.T, key string) *fakeDispatch
		want  want
	}{
		{
			name: "the owner writes the checks during the first phase and the change merges",
			build: func(t *testing.T, key string) *fakeDispatch {
				s := reopened(t, key)
				s.verification(owner, requested.Add(time.Hour), 4, checkAdded("c-widget-export"))
				s.merged(mergedAt)
				s.verification(owner, mergedAt.Add(time.Hour), 5, checkAdded("c-late"))
				return s.fake
			},
			want: want{category: ChecksWritten, start: true, end: &End{PullRequest: pullRequest, At: mergedAt}, versions: []int{4}, checks: added},
		},
		{
			name: "the same schedule with no checks and no withdrawal",
			build: func(t *testing.T, key string) *fakeDispatch {
				s := reopened(t, key)
				s.verification(daemonOf(key), requested.Add(90*time.Minute), 4, checkAdded("c-daemon"))
				s.merged(mergedAt)
				s.verification(owner, mergedAt.Add(time.Hour), 5, checkAdded("c-late"))
				return s.fake
			},
			want: want{category: NotFollowed, start: true, end: &End{PullRequest: pullRequest, At: mergedAt}},
		},
		{
			name: "an unrelated edit of the Verification document",
			build: func(t *testing.T, key string) *fakeDispatch {
				s := reopened(t, key)
				s.verification(owner, requested.Add(time.Hour), 4, unrelated())
				s.merged(mergedAt)
				return s.fake
			},
			want: want{category: NotFollowed, start: true, end: &End{PullRequest: pullRequest, At: mergedAt}, versions: []int{4},
				checks: map[int][]dispatch.BlockChange{}},
		},
		{
			name: "the request withdrawn with evidence and no pull request",
			build: func(t *testing.T, key string) *fakeDispatch {
				s := reopened(t, key)
				s.wait(withdrawnAt.Add(5*time.Second), record.StoredWait{Entered: withdrawnAt,
					Ended: &record.WaitChangeEnd{Generation: changeGen, At: withdrawnAt},
					Withdrawn: []record.WaitWithdrawal{{ID: requestID, Kind: record.DefectKindRequest, Text: "Reopened after an early close: the widget export was dropped.",
						Reason: evidence, At: withdrawnAt}}})
				return s.fake
			},
			want: want{category: RequestWithdrawn, start: true, end: &End{At: withdrawnAt},
				withdrawal: &Withdrawal{Request: requestID, Reason: evidence, At: withdrawnAt}},
		},
		{
			name: "a change still open is pending, though its owner wrote checks",
			build: func(t *testing.T, key string) *fakeDispatch {
				s := reopened(t, key)
				s.verification(owner, requested.Add(time.Hour), 4, checkAdded("c-widget-export"))
				s.wait(requested.Add(2*time.Hour), openWait(requested.Add(time.Second)))
				return s.fake
			},
			want: want{category: Pending, start: true, versions: []int{4}, checks: added},
		},
		{
			name: "a missing page of events is not assessable",
			build: func(t *testing.T, key string) *fakeDispatch {
				s := reopened(t, key)
				s.verification(owner, requested.Add(time.Hour), 4, checkAdded("c-widget-export"))
				s.merged(mergedAt)
				s.fake.eventsErr = &dispatch.EventLogError{Event: 201, Err: errors.New("page issue events for " + key + ": unexpected end of JSON input")}
				return s.fake
			},
			want: want{category: NotAssessable},
		},
		{
			name: "a Verification document first written during the first phase",
			build: func(t *testing.T, key string) *fakeDispatch {
				s := &schedule{t: t, key: key, fake: &fakeDispatch{closeWait: map[int]string{}, changes: map[int]dispatch.ArtifactChanges{}}}
				s.status("issue.created", person, requested.Add(-96*time.Hour), "todo")
				closed := requested.Add(-72 * time.Hour)
				s.wait(closed, record.StoredWait{Entered: closed, Closed: &closed})
				s.status("issue.closed", daemonOf(key), closed.Add(time.Second), "done")
				s.fake.todo = s.status("issue.updated", daemonOf(key), requested, "todo")
				s.wait(requested.Add(2*time.Second), openWait(requested.Add(time.Second)))
				s.status("issue.updated", daemonOf(key), requested.Add(3*time.Second), "in_progress")
				s.fake.first = []dispatch.DocumentBlock{
					{ID: "r-widgets", Type: "requirement"},
					{ID: "c-widget-export", Type: "check", Attributes: map[string]string{"when": dispatch.CheckAfterMerge}},
				}
				s.versionEvent(owner, requested.Add(time.Hour), verificationID, 1)
				s.merged(mergedAt)
				return s.fake
			},
			want: want{category: ChecksWritten, start: true, end: &End{PullRequest: pullRequest, At: mergedAt}, versions: []int{1},
				checks: map[int][]dispatch.BlockChange{1: checkAdded("c-widget-export").Blocks}},
		},
		{
			name: "a change that stopped being open with no version saying its end is not assessable",
			build: func(t *testing.T, key string) *fakeDispatch {
				s := reopened(t, key)
				s.verification(owner, requested.Add(time.Hour), 4, checkAdded("c-widget-export"))
				s.wait(mergedAt.Add(time.Minute), record.StoredWait{Entered: mergedAt})
				return s.fake
			},
			want: want{category: NotAssessable, start: true},
		},
		{
			name: "a version in the same second as the change's end is not assessable",
			build: func(t *testing.T, key string) *fakeDispatch {
				s := reopened(t, key)
				s.verification(owner, mergedAt.Add(400*time.Millisecond), 4, checkAdded("c-widget-export"))
				s.merged(mergedAt)
				return s.fake
			},
			want: want{category: NotAssessable, start: true, end: &End{PullRequest: pullRequest, At: mergedAt}},
		},
		{
			name: "a version with a section the renderer does not write is not assessable, never passed over",
			build: func(t *testing.T, key string) *fakeDispatch {
				s := reopened(t, key)
				s.merged(mergedAt)
				s.fake.closeWait[s.waits] += "\n## Requests withdrawn by hand\n\n- request 17, withdrawn at 2026-11-20 12:00:00 UTC, because:\n"
				return s.fake
			},
			want: want{category: NotAssessable},
		},
	}
	var rows []Row
	for i, tc := range cases {
		key := fmt.Sprintf("ACME-%d", i+1)
		fake := tc.build(t, key)
		row := Read(context.Background(), fake, key)
		rows = append(rows, row)
		t.Run(tc.name, func(t *testing.T) {
			if row.Issue != key || row.Category != tc.want.category {
				t.Fatalf("%s reads as %q in category %q, want %s in %q; it could not read %q", tc.name, row.Issue, row.Category, key, tc.want.category, row.Unread)
			}
			if unread := len(row.Unread) > 0; unread != (tc.want.category == NotAssessable) {
				t.Fatalf("%s names %d reads it could not make (%q), want some exactly where it is not assessable", key, len(row.Unread), row.Unread)
			}
			switch {
			case !tc.want.start && row.Start != nil:
				t.Fatalf("%s starts at %+v, want no start: its log could not be read", key, row.Start)
			case tc.want.start && (row.Start == nil || row.Start.Seq != fake.todo || !row.Start.At.Equal(requested)):
				t.Fatalf("%s starts at %+v, want the accepted todo at seq %d, %s", key, row.Start, fake.todo, requested)
			}
			if tc.want.start && row.Change != changeGen {
				t.Fatalf("%s names change %d, want %d", key, row.Change, changeGen)
			}
			if !reflect.DeepEqual(row.End, tc.want.end) {
				t.Fatalf("%s ends at %+v, want %+v", key, row.End, tc.want.end)
			}
			if !reflect.DeepEqual(row.Withdrawal, tc.want.withdrawal) {
				t.Fatalf("%s's withdrawal is %+v, want %+v", key, row.Withdrawal, tc.want.withdrawal)
			}
			var numbers []int
			for _, version := range row.Versions {
				numbers = append(numbers, version.Number)
				if version.Actor != owner {
					t.Fatalf("%s lists version %d by %+v, want only versions by actors other than the daemon", key, version.Number, version.Actor)
				}
				if want := tc.want.checks[version.Number]; !reflect.DeepEqual(version.Checks, want) {
					t.Fatalf("%s's version %d changed the check blocks %+v, want %+v", key, version.Number, version.Checks, want)
				}
			}
			if !slices.Equal(numbers, tc.want.versions) {
				t.Fatalf("%s lists the versions %v in its interval, want %v", key, numbers, tc.want.versions)
			}
		})
	}
	totals := Totals(rows)
	wantTotals := []Total{
		{Category: ChecksWritten, Issues: []string{"ACME-1", "ACME-7"}},
		{Category: RequestWithdrawn, Issues: []string{"ACME-4"}},
		{Category: NotFollowed, Issues: []string{"ACME-2", "ACME-3"}},
		{Category: Pending, Issues: []string{"ACME-5"}},
		{Category: NotAssessable, Issues: []string{"ACME-6", "ACME-8", "ACME-9", "ACME-10"}},
	}
	if !reflect.DeepEqual(totals, wantTotals) {
		t.Fatalf("the totals are %+v, want %+v", totals, wantTotals)
	}
	assessed := 0
	for _, total := range totals {
		if total.Category.Assessed() {
			assessed += len(total.Issues)
		}
	}
	if assessed != 5 {
		t.Fatalf("%d ended rows could be assessed, want 5: checks written, withdrawn and not followed, and neither pending nor not assessable", assessed)
	}
	// The fixtures render close-wait.md from these types with record.CloseWaitDocument, so a form
	// the renderer changes fails the cases above. A field the types gain is one no fixture sets, and
	// a renderer that writes a request's withdrawal from it would pass here unread. So each type
	// holds exactly the fields the fixtures were written against.
	for _, shape := range []struct {
		of     any
		fields []string
	}{
		{record.StoredWait{}, []string{"Entered", "Checks", "Production", "Beneath", "OpenChildren", "Defects", "NextChange", "Unread", "Unapproved", "Ended", "Withdrawn", "Closed"}},
		{record.WaitChange{}, []string{"Generation", "Reasons", "Since"}},
		{record.WaitReason{}, []string{"ID", "Kind", "Text", "Check", "Role", "Head", "Since"}},
		{record.WaitWithdrawal{}, []string{"ID", "Kind", "Text", "Reason", "At"}},
		{record.WaitChangeEnd{}, []string{"Generation", "PullRequest", "At"}},
	} {
		typ := reflect.TypeOf(shape.of)
		var fields []string
		for i := range typ.NumField() {
			fields = append(fields, typ.Field(i).Name)
		}
		if !slices.Equal(fields, shape.fields) {
			t.Errorf("record.%s has the fields %v, and the fixtures were written against %v: set each new field in the fixture that renders it, and read what close-wait.md says of it",
				typ.Name(), fields, shape.fields)
		}
	}
}
